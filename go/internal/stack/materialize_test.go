//go:build unix

package stack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeRegistry is an httptest OCI distribution endpoint serving exactly one
// guest artifact: the manifest and the three layers, by digest. It counts
// requests so the verified-no-op case can assert ZERO additional traffic, and
// can corrupt or truncate a named blob to drive the fail-closed paths for real.
type fakeRegistry struct {
	t        *testing.T
	server   *httptest.Server
	repo     string
	manifest []byte
	digest   string
	blobs    map[string][]byte

	// corrupt names a layer whose body is served with one byte flipped (a
	// digest mismatch at the declared size); truncate names one served short (a
	// size mismatch). Keyed by the layer's descriptor digest.
	corrupt  map[string]bool
	truncate map[string]bool

	mu       sync.Mutex
	requests []string
}

// guestBlobBytes is the per-asset layer content the stub serves. Small and
// distinct per asset, so a swapped layer would fail the digest check.
var guestBlobBytes = [...][]byte{
	[]byte("fake-kernel-bzImage-bytes"),
	[]byte("fake-rootfs-erofs-bytes"),
	[]byte("fake-initrd-cpio-zst-bytes"),
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// newFakeRegistry builds a stub serving a well-formed guest artifact. mutate
// (may be nil) edits the manifest value before it is serialised, so a negative
// case can publish a manifest whose descriptor disagrees with the blob the
// registry actually serves.
func newFakeRegistry(t *testing.T, mutate func(m *guestManifest)) *fakeRegistry {
	t.Helper()
	r := &fakeRegistry{
		t:        t,
		repo:     "rigelbuild/compass-guest-image",
		blobs:    map[string][]byte{},
		corrupt:  map[string]bool{},
		truncate: map[string]bool{},
	}
	m := guestManifest{
		SchemaVersion: 2,
		ArtifactType:  guestArtifactType,
		Config:        guestDescriptor{MediaType: emptyConfigMediaType, Digest: emptyConfigDigest, Size: emptyConfigSize},
		Annotations: map[string]string{
			"org.opencontainers.image.revision": "0123456789abcdef",
			guestAgentDigestAnnotation:          digestOf([]byte("agent-image")),
		},
	}
	for i, asset := range guestAssets {
		body := guestBlobBytes[i]
		d := digestOf(body)
		r.blobs[d] = body
		m.Layers = append(m.Layers, guestDescriptor{MediaType: asset.mediaType, Digest: d, Size: int64(len(body))})
		m.Annotations[guestLayerAnnotationPrefix+asset.annotationName] = strings.TrimPrefix(d, "sha256:")
	}
	if mutate != nil {
		mutate(&m)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal stub manifest: %v", err)
	}
	r.manifest = raw
	r.digest = digestOf(raw)
	r.server = httptest.NewServer(http.HandlerFunc(r.handle))
	t.Cleanup(r.server.Close)
	return r
}

func (r *fakeRegistry) handle(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.requests = append(r.requests, req.URL.Path)
	r.mu.Unlock()

	prefix := "/v2/" + r.repo + "/"
	switch {
	case req.URL.Path == prefix+"manifests/"+r.digest:
		w.Header().Set("Content-Type", ociManifestMediaType)
		_, _ = w.Write(r.manifest) // test stub; a short write surfaces as the client's own error
	case strings.HasPrefix(req.URL.Path, prefix+"blobs/"):
		d := strings.TrimPrefix(req.URL.Path, prefix+"blobs/")
		body, ok := r.blobs[d]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch {
		case r.corrupt[d]:
			flipped := append([]byte(nil), body...)
			flipped[0] ^= 0xff
			body = flipped
		case r.truncate[d]:
			body = body[:len(body)-1]
		}
		_, _ = w.Write(body) // test stub
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// registry returns the guestRegistry aimed at this stub. httptest serves plain
// HTTP, so the test drives materializeGuestArtifact (which takes a resolved
// base URL) rather than materializeGuestImage's https-only reference parse; the
// reference parse is covered directly by TestParseGuestRef.
func (r *fakeRegistry) registry() guestRegistry {
	return guestRegistry{baseURL: r.server.URL, repository: r.repo, digest: r.digest}
}

func (r *fakeRegistry) requestCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// TestMaterializeGuestHappyPath is the load-bearing positive case: the three
// blobs land at the four fixed basenames, each byte-exact, and manifest.sha256
// is written in the exact format the runner's parseManifest consumes (two
// spaces, lowercase hex, the ON-DISK basenames — not the annotation ones).
func TestMaterializeGuestHappyPath(t *testing.T) {
	reg := newFakeRegistry(t, nil)
	stateDir := t.TempDir()

	paths, err := materializeGuestArtifact(context.Background(), reg.registry(), stateDir)
	if err != nil {
		t.Fatalf("materializeGuestArtifact = %v, want nil", err)
	}

	wantDir := filepath.Join(stateDir, guestImageDirName, strings.TrimPrefix(reg.digest, "sha256:"))
	if want := guestPathsIn(wantDir); paths != want {
		t.Fatalf("paths = %+v, want %+v", paths, want)
	}
	for i, path := range paths.assets() {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != string(guestBlobBytes[i]) {
			t.Errorf("%s content = %q, want %q", path, got, guestBlobBytes[i])
		}
	}

	var want strings.Builder
	for i, asset := range guestAssets {
		want.WriteString(strings.TrimPrefix(digestOf(guestBlobBytes[i]), "sha256:") + "  " + asset.fileName + "\n")
	}
	got, err := os.ReadFile(paths.Manifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if string(got) != want.String() {
		t.Fatalf("manifest.sha256 =\n%q\nwant\n%q", got, want.String())
	}
}

// TestMaterializeGuestManifestFileFeedsRunnerVerification proves the written
// manifest is keyed on what the runner looks up: filepath.Base of each
// configured --microvm-* path. The publish lane annotates different basenames,
// so copying those through would write a manifest the runner cannot match.
func TestMaterializeGuestManifestFileFeedsRunnerVerification(t *testing.T) {
	reg := newFakeRegistry(t, nil)
	paths, err := materializeGuestArtifact(context.Background(), reg.registry(), t.TempDir())
	if err != nil {
		t.Fatalf("materializeGuestArtifact = %v, want nil", err)
	}
	recorded, err := readGuestManifestFile(paths.Manifest)
	if err != nil {
		t.Fatalf("readGuestManifestFile = %v", err)
	}
	for _, path := range paths.assets() {
		if _, ok := recorded[filepath.Base(path)]; !ok {
			t.Errorf("manifest has no entry for %q, the basename the runner keys on; entries: %v", filepath.Base(path), recorded)
		}
	}
}

// TestMaterializeGuestFailsClosedOnBlobMismatch: a served blob whose bytes
// disagree with its descriptor — either hash (corrupt) or length (truncated) —
// must fail and leave NO final directory or asset behind. Neither is transient,
// so neither may be retried into a slow failure.
func TestMaterializeGuestFailsClosedOnBlobMismatch(t *testing.T) {
	tests := []struct {
		name     string
		wantErr  string
		arrange  func(r *fakeRegistry, d string)
		assetIdx int
	}{
		{name: "kernel digest mismatch", wantErr: "hashes to", assetIdx: 0, arrange: func(r *fakeRegistry, d string) { r.corrupt[d] = true }},
		{name: "rootfs digest mismatch", wantErr: "hashes to", assetIdx: 1, arrange: func(r *fakeRegistry, d string) { r.corrupt[d] = true }},
		{name: "initrd size mismatch", wantErr: "want the declared", assetIdx: 2, arrange: func(r *fakeRegistry, d string) { r.truncate[d] = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := newFakeRegistry(t, nil)
			tt.arrange(reg, digestOf(guestBlobBytes[tt.assetIdx]))
			stateDir := t.TempDir()

			_, err := materializeGuestArtifact(context.Background(), reg.registry(), stateDir)
			if err == nil {
				t.Fatal("materializeGuestArtifact = nil error, want a fail-closed mismatch")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to name %q", err, tt.wantErr)
			}
			assertNoMaterializedDir(t, stateDir, reg.digest)
		})
	}
}

// TestMaterializeGuestPartialFetchLeavesNothing: the first blob verifies and
// lands, the second 404s. The whole directory must be absent — a partially
// materialised guest dir would later pass a bare existence check and boot with
// a missing asset.
func TestMaterializeGuestPartialFetchLeavesNothing(t *testing.T) {
	reg := newFakeRegistry(t, nil)
	delete(reg.blobs, digestOf(guestBlobBytes[1]))
	stateDir := t.TempDir()

	if _, err := materializeGuestArtifact(context.Background(), reg.registry(), stateDir); err == nil {
		t.Fatal("materializeGuestArtifact = nil error, want the missing-blob failure")
	}
	assertNoMaterializedDir(t, stateDir, reg.digest)

	// The staging directory is cleaned up too, so a retry does not accumulate
	// half-fetched multi-GiB copies under the state dir.
	entries, err := os.ReadDir(filepath.Join(stateDir, guestImageDirName))
	if err != nil {
		t.Fatalf("read guest-image root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("guest-image root holds %d leftover entries, want none", len(entries))
	}
}

// TestMaterializeGuestSecondCallIsVerifiedNoOp: the same digest twice makes ZERO
// additional requests, and returns identical paths. The request count is the
// assertion that matters — a re-fetch would be correct-but-wasteful, while a
// bare existence check would be fast-but-unsafe (the corruption case below).
func TestMaterializeGuestSecondCallIsVerifiedNoOp(t *testing.T) {
	reg := newFakeRegistry(t, nil)
	stateDir := t.TempDir()

	first, err := materializeGuestArtifact(context.Background(), reg.registry(), stateDir)
	if err != nil {
		t.Fatalf("first materializeGuestArtifact = %v", err)
	}
	after := reg.requestCount()
	if after == 0 {
		t.Fatal("first call made no requests; the stub was never reached")
	}

	second, err := materializeGuestArtifact(context.Background(), reg.registry(), stateDir)
	if err != nil {
		t.Fatalf("second materializeGuestArtifact = %v, want a verified no-op", err)
	}
	if second != first {
		t.Fatalf("second call paths = %+v, want the first call's %+v", second, first)
	}
	if got := reg.requestCount(); got != after {
		t.Fatalf("second call made %d additional requests, want 0", got-after)
	}
}

// TestMaterializeGuestExistingDirCorruptFailsClosed: an existing final directory
// whose contents no longer match its own manifest must fail rather than be
// trusted OR overwritten. Overwriting would silently repair a tampered or
// bit-rotted dir with no signal; trusting it would boot a corrupt guest.
func TestMaterializeGuestExistingDirCorruptFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(t *testing.T, paths GuestPaths)
	}{
		{
			name: "asset content diverged from the manifest",
			corrupt: func(t *testing.T, paths GuestPaths) { //nolint:thelper // callback is a table case, not a test helper
				if err := os.WriteFile(paths.Rootfs, []byte("tampered"), 0o644); err != nil {
					t.Fatalf("tamper rootfs: %v", err)
				}
			},
		},
		{
			name: "asset removed entirely",
			corrupt: func(t *testing.T, paths GuestPaths) { //nolint:thelper // callback is a table case, not a test helper
				if err := os.Remove(paths.Initrd); err != nil {
					t.Fatalf("remove initrd: %v", err)
				}
			},
		},
		{
			name: "manifest removed",
			corrupt: func(t *testing.T, paths GuestPaths) { //nolint:thelper // callback is a table case, not a test helper
				if err := os.Remove(paths.Manifest); err != nil {
					t.Fatalf("remove manifest: %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := newFakeRegistry(t, nil)
			stateDir := t.TempDir()
			paths, err := materializeGuestArtifact(context.Background(), reg.registry(), stateDir)
			if err != nil {
				t.Fatalf("seed materializeGuestArtifact = %v", err)
			}
			tt.corrupt(t, paths)
			before := reg.requestCount()

			if _, err := materializeGuestArtifact(context.Background(), reg.registry(), stateDir); err == nil {
				t.Fatal("materializeGuestArtifact over a corrupt dir = nil error, want a fail-closed verification error")
			}
			// Fail closed, not repair: no re-fetch, and the directory is left
			// exactly as the operator left it.
			if got := reg.requestCount(); got != before {
				t.Errorf("corrupt-dir path made %d requests, want 0 (it must not re-fetch over an existing dir)", got-before)
			}
			if _, err := os.Stat(filepath.Dir(paths.Kernel)); err != nil {
				t.Errorf("existing dir was removed on the failure path: %v", err)
			}
		})
	}
}

// TestMaterializeGuestRejectsWrongManifestShape: the manifest is the only thing
// naming what the blobs ARE, so every structural claim is checked. Each case
// publishes a manifest the registry serves under its own (recomputed) digest —
// so the failure is the SHAPE check, not the identity check.
func TestMaterializeGuestRejectsWrongManifestShape(t *testing.T) {
	tests := []struct {
		name    string
		wantErr string
		mutate  func(m *guestManifest)
	}{
		{name: "wrong schemaVersion", wantErr: "schemaVersion", mutate: func(m *guestManifest) { m.SchemaVersion = 1 }},
		{name: "wrong artifactType", wantErr: "artifactType", mutate: func(m *guestManifest) { m.ArtifactType = "application/vnd.oci.image.config.v1+json" }},
		{
			name:    "runnable config instead of the empty config",
			wantErr: "empty config",
			mutate: func(m *guestManifest) {
				m.Config = guestDescriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: digestOf([]byte("cfg")), Size: 3}
			},
		},
		{name: "too few layers", wantErr: "layers", mutate: func(m *guestManifest) { m.Layers = m.Layers[:2] }},
		{
			name:    "layers out of contract order",
			wantErr: "mediaType",
			mutate: func(m *guestManifest) {
				m.Layers[0], m.Layers[1] = m.Layers[1], m.Layers[0]
			},
		},
		{
			name:    "layer size not positive",
			wantErr: "positive byte count",
			mutate:  func(m *guestManifest) { m.Layers[1].Size = 0 },
		},
		{
			name:    "layer digest not lowercase sha256",
			wantErr: "digest is",
			mutate:  func(m *guestManifest) { m.Layers[2].Digest = strings.ToUpper(m.Layers[2].Digest) },
		},
		{
			name:    "layer annotation names a different basename",
			wantErr: "annotation",
			mutate: func(m *guestManifest) {
				delete(m.Annotations, guestLayerAnnotationPrefix+guestAssets[0].annotationName)
				m.Annotations[guestLayerAnnotationPrefix+"vmlinuz"] = strings.TrimPrefix(m.Layers[0].Digest, "sha256:")
			},
		},
		{
			name:    "layer annotation disagrees with the descriptor digest",
			wantErr: "want the layer digest",
			mutate: func(m *guestManifest) {
				m.Annotations[guestLayerAnnotationPrefix+guestAssets[1].annotationName] = strings.Repeat("0", 64)
			},
		},
		{
			name:    "missing provenance annotation",
			wantErr: guestAgentDigestAnnotation,
			mutate:  func(m *guestManifest) { delete(m.Annotations, guestAgentDigestAnnotation) },
		},
		{
			name:    "provenance annotation is not a sha256 digest",
			wantErr: guestAgentDigestAnnotation,
			mutate:  func(m *guestManifest) { m.Annotations[guestAgentDigestAnnotation] = "latest" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := newFakeRegistry(t, tt.mutate)
			stateDir := t.TempDir()

			_, err := materializeGuestArtifact(context.Background(), reg.registry(), stateDir)
			if err == nil {
				t.Fatal("materializeGuestArtifact = nil error, want the manifest rejected")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to name %q", err, tt.wantErr)
			}
			assertNoMaterializedDir(t, stateDir, reg.digest)
		})
	}
}

// TestMaterializeGuestRetriesMidStreamDrop: a connection that dies mid-body is
// transient, so the blob fetch reattempts and succeeds. The distinction from the
// mismatch cases: a SHORT body that completes cleanly is final, while one that
// never completes is worth another attempt — both arrive through the same sink.
func TestMaterializeGuestRetriesMidStreamDrop(t *testing.T) {
	reg := newFakeRegistry(t, nil)
	rootfsDigest := digestOf(guestBlobBytes[1])
	var mu sync.Mutex
	dropped := false
	base := reg.server.Config.Handler
	reg.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "blobs/"+rootfsDigest) { //nolint:nestif // test handler intentionally nests retry and connection-drop branches
			mu.Lock()
			first := !dropped
			dropped = true
			mu.Unlock()
			if first {
				// Declare the full length, send part of it, then hijack the
				// connection and close it — the client sees an unexpected EOF
				// mid-body rather than a clean short response.
				w.Header().Set("Content-Length", strconv.Itoa(len(guestBlobBytes[1])))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(guestBlobBytes[1][:4]) // test stub
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, err := hj.Hijack()
					if err == nil {
						_ = conn.Close() // deliberately severing mid-body
					}
				}
				return
			}
		}
		base.ServeHTTP(w, req)
	})

	paths, err := materializeGuestArtifact(context.Background(), reg.registry(), t.TempDir())
	if err != nil {
		t.Fatalf("materializeGuestArtifact = %v, want the mid-stream drop retried to success", err)
	}
	got, err := os.ReadFile(paths.Rootfs)
	if err != nil {
		t.Fatalf("read rootfs: %v", err)
	}
	if string(got) != string(guestBlobBytes[1]) {
		t.Fatalf("rootfs content = %q, want the complete %q", got, guestBlobBytes[1])
	}
}

// TestMaterializeGuestRejectsManifestDigestMismatch: the digest IS the identity,
// so a registry or proxy answering with different bytes must fail — otherwise
// that content would be written under the pinned digest's directory name and
// pass every later verification.
func TestMaterializeGuestRejectsManifestDigestMismatch(t *testing.T) {
	reg := newFakeRegistry(t, nil)
	stateDir := t.TempDir()
	// Serve a different (still well-formed) manifest under the pinned path.
	other := newFakeRegistry(t, func(m *guestManifest) { m.Annotations["org.opencontainers.image.revision"] = "deadbeefcafe" })
	reg.manifest = other.manifest

	_, err := materializeGuestArtifact(context.Background(), reg.registry(), stateDir)
	if err == nil {
		t.Fatal("materializeGuestArtifact = nil error, want the manifest identity rejected")
	}
	if !strings.Contains(err.Error(), "registry answered manifest") {
		t.Errorf("error = %v, want it to name the answered manifest digest", err)
	}
	assertNoMaterializedDir(t, stateDir, reg.digest)
}

// TestMaterializeGuestDoesNotRetry4xx: a 4xx is final. Retrying it would turn a
// clear "no such artifact" into three times the wait for the same answer.
func TestMaterializeGuestDoesNotRetry4xx(t *testing.T) {
	reg := newFakeRegistry(t, nil)
	reg.digest = digestOf([]byte("a manifest this registry does not serve"))

	if _, err := materializeGuestArtifact(context.Background(), reg.registry(), t.TempDir()); err == nil {
		t.Fatal("materializeGuestArtifact = nil error, want the 404 surfaced")
	}
	if got := reg.requestCount(); got != 1 {
		t.Fatalf("a 404 manifest fetch made %d requests, want exactly 1 (no retry on 4xx)", got)
	}
}

// TestMaterializeGuestRetriesTransient5xx: a 500 is transient, so the fetch
// retries within its bounded attempt budget and succeeds when the registry
// recovers.
func TestMaterializeGuestRetriesTransient5xx(t *testing.T) {
	reg := newFakeRegistry(t, nil)
	var mu sync.Mutex
	failures := 0
	base := reg.server.Config.Handler
	reg.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		first := failures == 0
		if first {
			failures++
		}
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		base.ServeHTTP(w, req)
	})

	if _, err := materializeGuestArtifact(context.Background(), reg.registry(), t.TempDir()); err != nil {
		t.Fatalf("materializeGuestArtifact = %v, want the transient 503 retried to success", err)
	}
}

// TestMaterializeGuestImageRejectsUnpinnedRef: the fetcher fails closed at its
// OWN boundary even though Config.Validate already checks this, because the
// digest keys the content-addressed directory — a caller reaching this helper
// directly with a tag must not create a mutable-keyed dir.
func TestMaterializeGuestImageRejectsUnpinnedRef(t *testing.T) {
	refs := []string{
		"ghcr.io/rigelbuild/compass-guest-image:latest",
		"ghcr.io/rigelbuild/compass-guest-image",
		"ghcr.io/rigelbuild/compass-guest-image@sha256:" + strings.Repeat("0", 63),
		"ghcr.io/rigelbuild/compass-guest-image@sha256:" + strings.ToUpper(strings.Repeat("a", 64)),
		"ghcr.io/rigelbuild/compass-guest-image@sha512:" + strings.Repeat("a", 64),
	}
	for _, ref := range refs {
		t.Run(ref, func(t *testing.T) {
			stateDir := t.TempDir()
			if _, err := materializeGuestImage(context.Background(), ref, stateDir); err == nil {
				t.Fatalf("materializeGuestImage(%q) = nil error, want a digest-pin rejection", ref)
			}
			if entries, err := os.ReadDir(stateDir); err == nil && len(entries) != 0 {
				t.Errorf("rejected ref created %d state-dir entries, want none", len(entries))
			}
		})
	}
}

// TestParseGuestRef covers the reference split the https fetch path depends on:
// a valid reference yields the registry base URL, the repository path, and the
// digest; a reference whose repository could address a different endpoint (a
// traversal component, an absent repository) is refused.
func TestParseGuestRef(t *testing.T) { //nolint:gocognit // the exhaustive invalid-reference table documents the parser boundary
	const digest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	t.Run("valid reference splits into registry, repository, digest", func(t *testing.T) {
		reg, err := parseGuestRef("ghcr.io/rigelbuild/compass-guest-image@" + digest)
		if err != nil {
			t.Fatalf("parseGuestRef = %v, want nil", err)
		}
		if reg.baseURL != "https://ghcr.io" {
			t.Errorf("baseURL = %q, want https://ghcr.io", reg.baseURL)
		}
		if reg.repository != "rigelbuild/compass-guest-image" {
			t.Errorf("repository = %q, want rigelbuild/compass-guest-image", reg.repository)
		}
		if reg.digest != digest {
			t.Errorf("digest = %q, want %q", reg.digest, digest)
		}
		// The repository is interpolated into a request path, so the built URL
		// must address exactly the pinned manifest.
		want := "https://ghcr.io/v2/rigelbuild/compass-guest-image/manifests/" + digest
		if got := reg.baseURL + "/v2/" + reg.repository + "/manifests/" + reg.digest; got != want {
			t.Errorf("manifest URL = %q, want %q", got, want)
		}
		if _, err := url.Parse(want); err != nil {
			t.Errorf("built manifest URL does not parse: %v", err)
		}
	})

	t.Run("port-bearing registry is accepted", func(t *testing.T) {
		reg, err := parseGuestRef("registry.internal:5000/compass/guest@" + digest)
		if err != nil {
			t.Fatalf("parseGuestRef = %v, want nil", err)
		}
		if reg.baseURL != "https://registry.internal:5000" {
			t.Errorf("baseURL = %q, want https://registry.internal:5000", reg.baseURL)
		}
	})

	t.Run("invalid references are refused", func(t *testing.T) {
		refs := map[string]string{
			"no repository path":         "compass-guest-image@" + digest,
			"traversal component":        "ghcr.io/../../secret@" + digest,
			"uppercase repository":       "ghcr.io/Rigel/Guest@" + digest,
			"space in repository":        "ghcr.io/guest image@" + digest,
			"query string in repository": "ghcr.io/guest?tag=x@" + digest,
			// The lenient split took the LAST "@", so everything before it was
			// unvalidated: a userinfo authority would dial an attacker host
			// while the reference still read as the pinned repository.
			"userinfo authority":  "attacker.example.com@ghcr.io/guest@" + digest,
			"two digests":         "ghcr.io/guest@" + digest + "@" + digest,
			"scheme prefix":       "https://ghcr.io/guest@" + digest,
			"tag beside digest":   "ghcr.io/guest:latest@" + digest,
			"port out of range":   "ghcr.io:99999/guest@" + digest,
			"empty port":          "ghcr.io:/guest@" + digest,
			"no digest at all":    "ghcr.io/guest",
			"tag only":            "ghcr.io/guest:latest",
			"uppercase hex":       "ghcr.io/guest@sha256:" + strings.Repeat("A", 64),
			"short digest":        "ghcr.io/guest@sha256:" + strings.Repeat("a", 63),
			"long digest":         "ghcr.io/guest@sha256:" + strings.Repeat("a", 65),
			"wrong algorithm":     "ghcr.io/guest@sha512:" + strings.Repeat("a", 64),
			"no algorithm":        "ghcr.io/guest@" + strings.Repeat("a", 64),
			"empty":               "",
			"trailing newline":    "ghcr.io/guest@" + digest + "\n",
			"leading space":       " ghcr.io/guest@" + digest,
			"embedded tab":        "ghcr.io\tguest@" + digest,
			"absolute path host":  "/ghcr.io/guest@" + digest,
			"empty repository":    "ghcr.io/@" + digest,
			"double slash":        "ghcr.io/rigel//guest@" + digest,
			"trailing slash":      "ghcr.io/guest/@" + digest,
			"leading separator":   "ghcr.io/-guest@" + digest,
			"double separator":    "ghcr.io/rigel__guest@" + digest,
			"fragment in ref":     "ghcr.io/guest#frag@" + digest,
			"credentials in host": "user:pass@ghcr.io/guest@" + digest,
		}
		for name, ref := range refs {
			t.Run(name, func(t *testing.T) {
				if _, err := parseGuestRef(ref); err == nil {
					t.Errorf("parseGuestRef(%q) = nil error, want a rejection", ref)
				}
			})
		}
	})

	// The parser is the ONE gate both Config.Validate and the fetcher use, so a
	// reference it accepts must produce a request path with no traversal or
	// escaping left in it.
	t.Run("accepted references build a clean request path", func(t *testing.T) {
		for _, ref := range []string{
			"ghcr.io/rigelbuild/compass-guest-image@" + digest,
			"registry.internal:5000/compass/guest@" + digest,
			"localhost:5000/guest@" + digest,
			"reg.example.com/a/b/c/deep-name.v1@" + digest,
		} {
			t.Run(ref, func(t *testing.T) {
				reg, err := parseGuestRef(ref)
				if err != nil {
					t.Fatalf("parseGuestRef(%q) = %v, want nil", ref, err)
				}
				raw := reg.baseURL + "/v2/" + reg.repository + "/manifests/" + reg.digest
				u, err := url.Parse(raw)
				if err != nil {
					t.Fatalf("built URL %q does not parse: %v", raw, err)
				}
				if u.Path != path.Clean(u.Path) {
					t.Errorf("path %q is not already clean", u.Path)
				}
				if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
					t.Errorf("built URL %q carries query/fragment/userinfo", raw)
				}
				if u.Scheme != "https" {
					t.Errorf("scheme = %q, want https", u.Scheme)
				}
			})
		}
	})
}

// TestResolveGuestDir covers the air-gapped bypass: a complete directory yields
// the same four fixed paths the fetch path produces, and an incomplete one is
// refused so the runner is never started against a missing asset.
func TestResolveGuestDir(t *testing.T) {
	t.Run("complete dir yields the fixed paths", func(t *testing.T) {
		dir := seedGuestDir(t)
		got, err := resolveGuestDir(dir)
		if err != nil {
			t.Fatalf("resolveGuestDir = %v, want nil", err)
		}
		if want := guestPathsIn(dir); got != want {
			t.Fatalf("paths = %+v, want %+v", got, want)
		}
	})

	t.Run("missing or unusable assets are refused", func(t *testing.T) {
		tests := []struct {
			name    string
			arrange func(t *testing.T, dir string)
		}{
			{name: "missing kernel", arrange: func(t *testing.T, dir string) { //nolint:thelper // table callback, not a test helper
				remove(t, filepath.Join(dir, guestKernelFile))
			}},
			{name: "missing rootfs", arrange: func(t *testing.T, dir string) { //nolint:thelper // table callback, not a test helper
				remove(t, filepath.Join(dir, guestRootfsFile))
			}},
			{name: "missing initrd", arrange: func(t *testing.T, dir string) { //nolint:thelper // table callback, not a test helper
				remove(t, filepath.Join(dir, guestInitrdFile))
			}},
			{name: "missing manifest", arrange: func(t *testing.T, dir string) { //nolint:thelper // table callback, not a test helper
				remove(t, filepath.Join(dir, guestManifestFile))
			}},
			{
				name: "empty asset",
				arrange: func(t *testing.T, dir string) { //nolint:thelper // table callback, not a test helper
					if err := os.WriteFile(filepath.Join(dir, guestRootfsFile), nil, 0o644); err != nil {
						t.Fatalf("truncate rootfs: %v", err)
					}
				},
			},
			{
				name: "directory where an asset belongs",
				arrange: func(t *testing.T, dir string) { //nolint:thelper // table callback, not a test helper
					remove(t, filepath.Join(dir, guestInitrdFile))
					if err := os.Mkdir(filepath.Join(dir, guestInitrdFile), 0o700); err != nil {
						t.Fatalf("mkdir over initrd: %v", err)
					}
				},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				dir := seedGuestDir(t)
				tt.arrange(t, dir)
				if _, err := resolveGuestDir(dir); err == nil {
					t.Fatal("resolveGuestDir = nil error, want a rejection")
				}
			})
		}
	})

	t.Run("absent dir is refused", func(t *testing.T) {
		if _, err := resolveGuestDir(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Fatal("resolveGuestDir on an absent dir = nil error, want a rejection")
		}
	})

	t.Run("file where the dir belongs is refused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "guest")
		if err := os.WriteFile(path, []byte("not a dir"), 0o644); err != nil {
			t.Fatalf("seed file: %v", err)
		}
		if _, err := resolveGuestDir(path); err == nil {
			t.Fatal("resolveGuestDir on a file = nil error, want a rejection")
		}
	})
}

// seedGuestDir writes a complete air-gapped guest directory: the three assets
// plus a coherent sha256sum manifest.
func seedGuestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var manifest strings.Builder
	for i, asset := range guestAssets {
		if err := os.WriteFile(filepath.Join(dir, asset.fileName), guestBlobBytes[i], 0o644); err != nil {
			t.Fatalf("write %s: %v", asset.fileName, err)
		}
		manifest.WriteString(strings.TrimPrefix(digestOf(guestBlobBytes[i]), "sha256:") + "  " + asset.fileName + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, guestManifestFile), []byte(manifest.String()), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
}

// assertNoMaterializedDir asserts the content-addressed final directory for
// digest does not exist — the fail-closed invariant every negative path shares:
// a later up must not find a directory it would trust on existence alone.
func assertNoMaterializedDir(t *testing.T, stateDir, digest string) {
	t.Helper()
	final := filepath.Join(stateDir, guestImageDirName, strings.TrimPrefix(digest, "sha256:"))
	if _, err := os.Stat(final); err == nil {
		t.Fatalf("final dir %q exists after a failed materialisation; want no final directory", final)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %q: %v", final, err)
	}
}

// TestReadGuestManifestFileRejectsMalformed: the verification path reads this
// file to decide an existing dir is trustworthy, so a half-understood manifest
// must be an error rather than a partial map that verifies a subset.
func TestReadGuestManifestFileRejectsMalformed(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "single space separator", content: strings.Repeat("a", 64) + " kernel\n"},
		{name: "uppercase digest", content: strings.ToUpper(strings.Repeat("a", 64)) + "  kernel\n"},
		{name: "short digest", content: strings.Repeat("a", 63) + "  kernel\n"},
		{name: "no basename", content: strings.Repeat("a", 64) + "  \n"},
		{name: "duplicate basename", content: strings.Repeat("a", 64) + "  kernel\n" + strings.Repeat("b", 64) + "  kernel\n"},
		{name: "prose", content: "not a manifest\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), guestManifestFile)
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("seed manifest: %v", err)
			}
			if _, err := readGuestManifestFile(path); err == nil {
				t.Fatalf("readGuestManifestFile(%q) = nil error, want a rejection", tt.content)
			}
		})
	}
}
