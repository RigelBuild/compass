//go:build unix

package stack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// runtimeBackendMicroVM is the RuntimeBackend value that selects the microVM
// session backend. It gates three things that must agree: the guest knobs, the
// omitted --image, and the skipped agent-image pull.
const runtimeBackendMicroVM = "microvm"

// microVM reports whether this config selects the microVM session backend. The
// agent ships inside the guest rootfs there, so no agent OCI image is pulled,
// forwarded, or required.
func (c Config) microVM() bool {
	return c.RuntimeBackend == runtimeBackendMicroVM
}

// The four fixed basenames a materialised guest directory holds. The runner's
// verifyImages keys its sha256sum manifest on filepath.Base of each configured
// --microvm-* path, so these names are the contract between what this file
// writes and what the runner reads.
const (
	guestKernelFile   = "kernel"
	guestRootfsFile   = "rootfs.erofs"
	guestInitrdFile   = "initrd"
	guestManifestFile = "manifest.sha256"
	// guestImageDirName is the state-dir subdirectory the content-addressed
	// per-digest directories live under.
	guestImageDirName = "guest-image"
)

// The published artifact shape: an OCI image manifest marked as a non-runnable
// artifact (nothing can run an erofs blob), pointing at the OCI 1.1 empty
// config — the literal two bytes `{}` — and three layers with dedicated media
// types.
const (
	guestArtifactType    = "application/vnd.compass.guest-image.v1"
	ociManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	emptyConfigMediaType = "application/vnd.oci.empty.v1+json"
	emptyConfigDigest    = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
	emptyConfigSize      = 2
)

// The provenance annotations the publish lane stamps: one per-layer digest keyed
// by the realised asset's basename, and the agent image digest the rootfs was
// derived from.
const (
	guestLayerAnnotationPrefix = "org.compass.guest.layer."
	guestAgentDigestAnnotation = "org.compass.guest.agent-image-digest"
)

// guestAsset binds one layer POSITION (layer order is contract) to everything
// that identifies it: the media type, the annotation basename the publish lane
// keys its digest under, and the filename written on disk.
type guestAsset struct {
	// mediaType is the per-asset layer media type from the publish lane.
	mediaType string
	// annotationName is the basename the PUBLISH lane uses — the realised
	// build output's name (tools/guest-image/publish-core.ts ASSET_FILENAMES).
	// It is the key half of the org.compass.guest.layer.<basename> annotation.
	annotationName string
	// fileName is the basename written on DISK, which the runner's verifyImages
	// keys its manifest lookup on (filepath.Base of each --microvm-* path).
	// It deliberately differs from annotationName: writing the publish lane's
	// names through would yield a manifest the runner reports as all-absent.
	fileName string
}

// guestAssets is the canonical layer-order table. Fields are named rather than
// positional: annotationName and fileName are both strings, so a positional
// literal would let a reorder silently swap the published name with the on-disk
// one — the exact defect this mapping exists to prevent.
var guestAssets = [...]guestAsset{
	{
		mediaType:      "application/vnd.compass.guest-kernel.v1",
		annotationName: "bzImage",
		fileName:       guestKernelFile,
	},
	{
		mediaType:      "application/vnd.compass.guest-rootfs.v1+erofs",
		annotationName: "compass-guest-rootfs.erofs",
		fileName:       guestRootfsFile,
	},
	{
		mediaType:      "application/vnd.compass.guest-initrd.v1+cpio.zst",
		annotationName: "compass-guest-initrd",
		fileName:       guestInitrdFile,
	},
}

// layerAnnotation is the full annotation key this asset's digest is published
// under. One accessor so the prefix is joined in exactly one place.
func (a guestAsset) layerAnnotation() string {
	return guestLayerAnnotationPrefix + a.annotationName
}

// Bounds on the anonymous registry fetch. There is no credential input, no
// registry client library, and no cache beyond the content-addressed directory.
const (
	// guestRequestTimeout is the ceiling on one whole exchange INCLUDING the
	// body: the rootfs layer is multi-GiB, so it is sized for a slow link
	// rather than for an API call.
	guestRequestTimeout = 30 * time.Minute
	// guestHeaderTimeout fails a silent registry fast, so a dead endpoint
	// cannot consume the transfer budget above.
	guestHeaderTimeout = 30 * time.Second
	// guestFetchAttempts bounds retries of a transient transport/5xx failure.
	guestFetchAttempts = 3
	guestRetryBackoff  = 500 * time.Millisecond
	// guestManifestMaxBytes bounds the manifest read: a real guest manifest is
	// well under a kilobyte, so anything larger is a wrong or hostile endpoint.
	guestManifestMaxBytes = 64 << 10
	// guestAssetMode is the deterministic mode every materialised asset lands
	// with, so a re-materialisation on a different umask cannot produce a
	// directory the runner reads differently.
	guestAssetMode = 0o644
)

// GuestPaths is the resolved location of one guest image: the three boot assets
// plus the sha256sum manifest the runner hash-verifies them against. The zero
// value means no guest paths are configured, which leaves the assets baked into
// the Runner image live.
type GuestPaths struct {
	Kernel   string
	Rootfs   string
	Initrd   string
	Manifest string
}

// assets returns the three boot asset paths in layer order, so verification can
// walk them beside guestAssets.
func (p GuestPaths) assets() []string {
	return []string{p.Kernel, p.Rootfs, p.Initrd}
}

// guestPathsIn is the fixed layout of a materialised guest directory. It is the
// one place the four basenames are spelled, so the fetcher, the air-gapped
// bypass, and the runner's flags cannot drift onto different names.
func guestPathsIn(dir string) GuestPaths {
	return GuestPaths{
		Kernel:   filepath.Join(dir, guestKernelFile),
		Rootfs:   filepath.Join(dir, guestRootfsFile),
		Initrd:   filepath.Join(dir, guestInitrdFile),
		Manifest: filepath.Join(dir, guestManifestFile),
	}
}

// materializeGuest is the fetch seam the spawn chain drives: the real
// content-addressed fetcher in production, swapped by tests so no unit test
// reaches a registry (the readStartTime pattern).
var materializeGuest = materializeGuestImage

// guestRegistry is one digest-pinned artifact's resolved location: the registry
// base URL, the repository path, and the manifest digest. materializeGuestImage
// derives it from the reference; tests build one aimed at an httptest server.
type guestRegistry struct {
	baseURL    string
	repository string
	digest     string
}

// ociRepositoryPattern is the OCI Distribution name grammar for the repository
// path: lowercase alphanumeric components joined by single separators. Enforced
// because the repository is interpolated into a request path, where a traversal
// component would address an entirely different endpoint.
var ociRepositoryPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)

// ociRegistryPattern is the host[:port] authority the reference must start
// with. There is no Docker-Hub-style implicit default: the registry is always
// spelled out, so what gets dialed is exactly what the operator pinned.
var ociRegistryPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(?::([0-9]{1,5}))?$`)

// guestDigestPrefix is the only digest algorithm this lane accepts. The publish
// lane emits sha256 exclusively, and the digest keys the content-addressed
// state dir, so a second algorithm would be a second directory namespace.
const guestDigestPrefix = "sha256:"

// materializeGuestImage fetches the digest-pinned guest artifact into
// <stateDir>/guest-image/<digest>/ and returns the four resolved paths. An
// existing directory is a VERIFIED no-op (zero requests, but only after
// re-hashing every asset) and is never overwritten; every failure is closed.
func materializeGuestImage(ctx context.Context, ref string, stateDir string) (GuestPaths, error) {
	reg, err := parseGuestRef(ref)
	if err != nil {
		return GuestPaths{}, err
	}
	return materializeGuestArtifact(ctx, reg, stateDir)
}

// parseGuestRef is the ONE strict parser for a guest artifact reference, used
// both by Config.Validate and by the fetcher, so the two can never disagree on
// what is acceptable. It accepts exactly `<host>[:port]/<repository>@sha256:<64
// lowercase hex>` and rejects everything else with a reason.
//
// Strict rather than lenient on purpose: the digest names the content-addressed
// directory and the repository is interpolated into a request path, so an
// almost-valid reference must fail here rather than fetch something else.
func parseGuestRef(ref string) (guestRegistry, error) {
	if ref == "" {
		return guestRegistry{}, errors.New("guest artifact reference is empty")
	}
	if strings.ContainsFunc(ref, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return guestRegistry{}, fmt.Errorf("guest artifact %q contains whitespace or control characters", ref)
	}
	// Exactly one "@": splitting on the LAST one would let a second digest-like
	// tail hide an arbitrary earlier authority (userinfo, or another digest).
	name, digest, ok := strings.Cut(ref, "@")
	if !ok {
		return guestRegistry{}, fmt.Errorf("guest artifact %q is not digest-pinned: want <registry>/<repository>@sha256:<64 lowercase hex>", ref)
	}
	if strings.Contains(digest, "@") {
		return guestRegistry{}, fmt.Errorf("guest artifact %q carries more than one %q", ref, "@")
	}
	if !isSHA256Digest(digest) {
		return guestRegistry{}, fmt.Errorf("guest artifact %q digest %q must be sha256:<64 lowercase hex>", ref, digest)
	}
	host, repository, ok := strings.Cut(name, "/")
	if !ok {
		return guestRegistry{}, fmt.Errorf("guest artifact %q names no repository path: want <registry>/<repository>@%s<64 lowercase hex>", ref, guestDigestPrefix)
	}
	if err := validGuestRegistryHost(host); err != nil {
		return guestRegistry{}, fmt.Errorf("guest artifact %q: %w", ref, err)
	}
	// A tag alongside the digest is legal OCI but ambiguous here: two mutable
	// and immutable identifiers for one artifact, only one of which is read.
	if strings.Contains(repository, ":") {
		return guestRegistry{}, fmt.Errorf("guest artifact %q carries a tag as well as a digest; pin by digest alone", ref)
	}
	if !ociRepositoryPattern.MatchString(repository) {
		return guestRegistry{}, fmt.Errorf("guest artifact %q has an invalid repository path %q (want lowercase OCI name components)", ref, repository)
	}
	return guestRegistry{baseURL: "https://" + host, repository: repository, digest: digest}, nil
}

// validGuestRegistryHost checks the authority is a bare host[:port] with an
// in-range port — never userinfo, a scheme, or a path.
func validGuestRegistryHost(host string) error {
	m := ociRegistryPattern.FindStringSubmatch(host)
	if m == nil {
		return fmt.Errorf("%q is not a bare registry host[:port]", host)
	}
	if m[2] == "" {
		return nil
	}
	port, err := strconv.Atoi(m[2])
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("registry port %q is out of range", m[2])
	}
	return nil
}

// materializeGuestArtifact is the fetch/verify/publish body, split from
// materializeGuestImage so tests can aim it at an httptest registry without a
// package-level client or scheme override.
func materializeGuestArtifact(ctx context.Context, reg guestRegistry, stateDir string) (GuestPaths, error) {
	if stateDir == "" {
		return GuestPaths{}, errors.New("materialise guest image: a state directory is required")
	}
	root := filepath.Join(stateDir, guestImageDirName)
	final := filepath.Join(root, strings.TrimPrefix(reg.digest, guestDigestPrefix))
	paths := guestPathsIn(final)

	switch _, err := os.Stat(final); {
	case err == nil:
		// Mere existence is never trust: a truncated or tampered asset must
		// fail closed rather than boot, and must not be overwritten either —
		// the operator decides what to do with a corrupt content-addressed dir.
		if err := verifyGuestDir(paths); err != nil {
			return GuestPaths{}, fmt.Errorf("materialise guest image %s: existing %q failed verification: %w", reg.digest, final, err)
		}
		return paths, nil
	case !errors.Is(err, os.ErrNotExist):
		return GuestPaths{}, fmt.Errorf("materialise guest image %s: stat %q: %w", reg.digest, final, err)
	}

	transport := guestTransport()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: guestRequestTimeout}

	manifest, err := fetchGuestManifest(ctx, client, reg)
	if err != nil {
		return GuestPaths{}, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return GuestPaths{}, fmt.Errorf("materialise guest image %s: create %q: %w", reg.digest, root, err)
	}
	// The assets are staged in a SIBLING of the final directory so the publish
	// step is one rename: a failed fetch leaves no partial final directory, and
	// only this call's own staging dir is ever removed.
	staging, err := os.MkdirTemp(root, ".staging-*")
	if err != nil {
		return GuestPaths{}, fmt.Errorf("materialise guest image %s: create staging dir in %q: %w", reg.digest, root, err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging) // best-effort cleanup of this call's own staging dir
		}
	}()

	for i, asset := range guestAssets {
		if err := fetchGuestBlob(ctx, client, reg, manifest.Layers[i], staging, asset.fileName); err != nil {
			return GuestPaths{}, fmt.Errorf("materialise guest image %s: %w", reg.digest, err)
		}
	}
	if err := writeGuestManifestFile(staging, manifest.Layers); err != nil {
		return GuestPaths{}, fmt.Errorf("materialise guest image %s: %w", reg.digest, err)
	}
	if err := os.Rename(staging, final); err != nil {
		return GuestPaths{}, fmt.Errorf("materialise guest image %s: publish %q: %w", reg.digest, final, err)
	}
	published = true
	return paths, nil
}

// guestTransport is the anonymous HTTPS transport every registry request rides:
// no credentials, no cookies, and finite waits on the parts that would otherwise
// hang under the multi-GiB body budget.
func guestTransport() *http.Transport {
	return &http.Transport{
		ResponseHeaderTimeout: guestHeaderTimeout,
		TLSHandshakeTimeout:   guestHeaderTimeout,
	}
}

// guestDescriptor is the subset of an OCI descriptor this fetcher consumes.
type guestDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// guestManifest is the subset of the OCI image manifest this fetcher consumes.
// Unknown fields are tolerated (a registry may add its own) but every field read
// below is checked.
type guestManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	ArtifactType  string            `json:"artifactType"`
	Config        guestDescriptor   `json:"config"`
	Layers        []guestDescriptor `json:"layers"`
	Annotations   map[string]string `json:"annotations"`
}

// fetchGuestManifest GETs the manifest BY DIGEST and re-derives its identity
// from the bytes received. The digest IS the identity, so a registry or proxy
// that answered with anything else fails closed here rather than having its
// content written under the pinned digest's directory name.
func fetchGuestManifest(ctx context.Context, client *http.Client, reg guestRegistry) (guestManifest, error) {
	url := reg.baseURL + "/v2/" + reg.repository + "/manifests/" + reg.digest
	var raw []byte
	err := doGuestGet(ctx, client, url, ociManifestMediaType, func(body io.Reader) error {
		b, err := io.ReadAll(io.LimitReader(body, guestManifestMaxBytes+1))
		if err != nil {
			return fmt.Errorf("read manifest from %s: %w", url, err)
		}
		if len(b) > guestManifestMaxBytes {
			return fmt.Errorf("manifest from %s exceeds %d bytes", url, guestManifestMaxBytes)
		}
		raw = b
		return nil
	})
	if err != nil {
		return guestManifest{}, fmt.Errorf("fetch guest manifest %s: %w", reg.digest, err)
	}
	sum := sha256.Sum256(raw)
	if got := guestDigestPrefix + hex.EncodeToString(sum[:]); got != reg.digest {
		return guestManifest{}, fmt.Errorf("fetch guest manifest %s: registry answered manifest %s", reg.digest, got)
	}
	var m guestManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return guestManifest{}, fmt.Errorf("fetch guest manifest %s: parse: %w", reg.digest, err)
	}
	if err := validateGuestManifest(m); err != nil {
		return guestManifest{}, fmt.Errorf("guest manifest %s is not a compass guest artifact: %w", reg.digest, err)
	}
	return m, nil
}

// validateGuestManifest enforces the published artifact shape: a v2 manifest
// marked with the guest artifact type, pointing at the 2-byte empty config, with
// exactly three layers in kernel/rootfs/initrd order and the provenance
// annotation present.
func validateGuestManifest(m guestManifest) error {
	if m.SchemaVersion != 2 {
		return fmt.Errorf("schemaVersion is %d, want 2", m.SchemaVersion)
	}
	if m.ArtifactType != guestArtifactType {
		return fmt.Errorf("artifactType is %q, want %q", m.ArtifactType, guestArtifactType)
	}
	if m.Config.MediaType != emptyConfigMediaType || m.Config.Digest != emptyConfigDigest || m.Config.Size != emptyConfigSize {
		return fmt.Errorf("config descriptor is {%s %s %d}, want the OCI empty config {%s %s %d}",
			m.Config.MediaType, m.Config.Digest, m.Config.Size, emptyConfigMediaType, emptyConfigDigest, emptyConfigSize)
	}
	if len(m.Layers) != len(guestAssets) {
		return fmt.Errorf("manifest carries %d layers, want exactly %d (kernel, rootfs, initrd)", len(m.Layers), len(guestAssets))
	}
	for i, asset := range guestAssets {
		if err := validateGuestLayer(m, i, asset); err != nil {
			return err
		}
	}
	if d := m.Annotations[guestAgentDigestAnnotation]; !isSHA256Digest(d) {
		return fmt.Errorf("provenance annotation %s is %q, want sha256:<64 lowercase hex>", guestAgentDigestAnnotation, d)
	}
	return nil
}

// validateGuestLayer checks one layer against the position it must occupy: the
// media type, a well-formed digest, a positive size, and the per-layer
// annotation. The annotation basename is load-bearing — it is how the publish
// lane says WHICH asset this layer is, so an unexpected one is a different layout.
func validateGuestLayer(m guestManifest, i int, asset guestAsset) error {
	layer := m.Layers[i]
	if layer.MediaType != asset.mediaType {
		return fmt.Errorf("layer %d mediaType is %q, want %q", i, layer.MediaType, asset.mediaType)
	}
	if !isSHA256Digest(layer.Digest) {
		return fmt.Errorf("layer %d digest is %q, want sha256:<64 lowercase hex>", i, layer.Digest)
	}
	if layer.Size <= 0 {
		return fmt.Errorf("layer %d size is %d, want a positive byte count", i, layer.Size)
	}
	key := asset.layerAnnotation()
	got, ok := m.Annotations[key]
	if !ok {
		return fmt.Errorf("layer %d (%s) carries no %s annotation", i, asset.fileName, key)
	}
	if want := strings.TrimPrefix(layer.Digest, guestDigestPrefix); got != want {
		return fmt.Errorf("layer %d annotation %s is %q, want the layer digest %q", i, key, got, want)
	}
	return nil
}

// fetchGuestBlob streams one layer into staging/<name>, hashing and counting
// while it writes, and renames the temp file into place only once the digest and
// the declared size both match. A mismatch removes the temp file and leaves no
// asset at the final name.
func fetchGuestBlob(ctx context.Context, client *http.Client, reg guestRegistry, layer guestDescriptor, staging, name string) error {
	url := reg.baseURL + "/v2/" + reg.repository + "/blobs/" + layer.Digest
	return doGuestGet(ctx, client, url, "*/*", func(body io.Reader) error {
		return writeGuestBlob(body, layer, staging, name)
	})
}

// writeGuestBlob is the verify-then-rename half of a blob fetch. It is split out
// so the retry loop's sink stays one call, and so the temp-file cleanup on every
// failure path lives in one function.
func writeGuestBlob(body io.Reader, layer guestDescriptor, staging, name string) error {
	tmp, err := os.CreateTemp(staging, name+".*.tmp")
	if err != nil {
		return fmt.Errorf("create %s temp in %q: %w", name, staging, err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()        // already failing; the caller's error is the actionable one
		_ = os.Remove(tmpName) // best-effort cleanup of the abandoned temp
	}
	h := sha256.New()
	// LimitReader at size+1 bounds the transfer: an over-long body is caught by
	// the size check below without downloading past the declared length.
	written, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(body, layer.Size+1))
	if err != nil {
		cleanup()
		// A mid-stream transport failure is worth another attempt: the whole
		// multi-GiB rootfs must not be abandoned because one connection
		// dropped. A digest or size mismatch below is NOT marked, so it stays
		// final — retrying it would only make a clear failure slow.
		return retryable{fmt.Errorf("stream %s: %w", name, err)}
	}
	if written != layer.Size {
		cleanup()
		return fmt.Errorf("%s is %d bytes, want the declared %d", name, written, layer.Size)
	}
	if got := guestDigestPrefix + hex.EncodeToString(h.Sum(nil)); got != layer.Digest {
		cleanup()
		return fmt.Errorf("%s hashes to %s, want %s", name, got, layer.Digest)
	}
	if err := tmp.Chmod(guestAssetMode); err != nil {
		cleanup()
		return fmt.Errorf("chmod %s temp: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync %s temp: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName) // best-effort cleanup of the abandoned temp
		return fmt.Errorf("close %s temp: %w", name, err)
	}
	if err := os.Rename(tmpName, filepath.Join(staging, name)); err != nil {
		_ = os.Remove(tmpName) // best-effort cleanup: the rename failed, so the temp still exists
		return fmt.Errorf("publish %s: %w", name, err)
	}
	return nil
}

// writeGuestManifestFile writes the sha256sum-format manifest the runner's
// --microvm-image-manifest consumes: one `<lowercase hex>␠␠<basename>` line per
// asset, in layer order. Written atomically (temp + rename, the pgid-file
// discipline) so a completed directory never holds a partial manifest.
func writeGuestManifestFile(staging string, layers []guestDescriptor) error {
	var b strings.Builder
	for i, asset := range guestAssets {
		fmt.Fprintf(&b, "%s  %s\n", strings.TrimPrefix(layers[i].Digest, guestDigestPrefix), asset.fileName)
	}
	tmp, err := os.CreateTemp(staging, guestManifestFile+".*.tmp")
	if err != nil {
		return fmt.Errorf("create manifest temp in %q: %w", staging, err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()        // already failing; the caller's error is the actionable one
		_ = os.Remove(tmpName) // best-effort cleanup of the abandoned temp
	}
	if err := tmp.Chmod(guestAssetMode); err != nil {
		cleanup()
		return fmt.Errorf("chmod manifest temp: %w", err)
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		cleanup()
		return fmt.Errorf("write manifest temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync manifest temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName) // best-effort cleanup of the abandoned temp
		return fmt.Errorf("close manifest temp: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(staging, guestManifestFile)); err != nil {
		_ = os.Remove(tmpName) // best-effort cleanup: the rename failed, so the temp still exists
		return fmt.Errorf("publish manifest: %w", err)
	}
	return nil
}

// retryable marks a sink failure the fetch loop may reattempt — a dropped
// connection mid-body, never a verification mismatch. Only the sink needs to
// say so; every other retry decision is visible to doGuestGet directly.
type retryable struct{ error }

func (r retryable) Unwrap() error { return r.error }

// doGuestGet issues one bounded anonymous GET and hands the response body to
// sink. A transport failure with a live context, a 429, a 5xx, and a
// sink-reported transient retry with doubling backoff; everything else is final
// — a digest or size mismatch would only become a slower clear failure.
func doGuestGet(ctx context.Context, client *http.Client, url, accept string, sink func(io.Reader) error) error {
	var last error
	for attempt := 1; attempt <= guestFetchAttempts; attempt++ {
		if attempt > 1 {
			if err := waitBackoff(ctx, guestRetryBackoff<<(attempt-2)); err != nil {
				return err
			}
		}
		retry, err := guestGetOnce(ctx, client, url, accept, sink)
		if err == nil {
			return nil
		}
		if !retry {
			return err
		}
		last = err
	}
	return fmt.Errorf("after %d attempts: %w", guestFetchAttempts, last)
}

// guestGetOnce performs one attempt and reports whether the failure is worth
// retrying.
func guestGetOnce(ctx context.Context, client *http.Client, url, accept string, sink func(io.Reader) error) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("build request for %s: %w", url, err)
	}
	req.Header.Set("Accept", accept)
	resp, err := client.Do(req)
	if err != nil {
		// A cancelled or expired context is the caller's decision, never a
		// transient fault to retry against.
		return ctx.Err() == nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() {
		_ = resp.Body.Close() // read-only body; a close error cannot affect the verified bytes
	}()
	if resp.StatusCode != http.StatusOK {
		transient := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError
		return transient, fmt.Errorf("GET %s: unexpected status %s", url, resp.Status)
	}
	if err := sink(resp.Body); err != nil {
		var transient retryable
		return errors.As(err, &transient) && ctx.Err() == nil, err
	}
	return false, nil
}

// waitBackoff waits d unless ctx is done first — a timer rather than a fixed
// sleep, so a cancelled up returns immediately instead of sitting out the delay.
func waitBackoff(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// verifyGuestDir re-checks a materialised directory against the manifest it
// shipped with: the manifest must name exactly the three assets, and each asset
// must be a regular file hashing to its recorded digest. A hash match pins the
// byte count too, so this subsumes a size check.
func verifyGuestDir(paths GuestPaths) error {
	recorded, err := readGuestManifestFile(paths.Manifest)
	if err != nil {
		return err
	}
	if len(recorded) != len(guestAssets) {
		return fmt.Errorf("manifest %q records %d assets, want exactly %d", paths.Manifest, len(recorded), len(guestAssets))
	}
	assetPaths := paths.assets()
	for i, asset := range guestAssets {
		want, ok := recorded[asset.fileName]
		if !ok {
			return fmt.Errorf("manifest %q records no %s entry", paths.Manifest, asset.fileName)
		}
		got, err := hashGuestFile(assetPaths[i])
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("%s hashes to %s, want the recorded %s", assetPaths[i], got, want)
		}
	}
	return nil
}

// readGuestManifestFile parses a materialised manifest.sha256 into a
// basename→digest map. It is strict about the format it writes itself: a
// malformed or duplicated line is a hard error, since a half-understood manifest
// is exactly what must not be verified against.
func readGuestManifestFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the stack-owned manifest inside the content-addressed guest dir
	if err != nil {
		return nil, fmt.Errorf("read guest manifest %q: %w", path, err)
	}
	out := make(map[string]string, len(guestAssets))
	for line := range strings.SplitSeq(strings.TrimRight(string(raw), "\n"), "\n") {
		digest, name, ok := strings.Cut(line, "  ")
		if ok && isLowerHex64(digest) && name != "" {
			if _, dup := out[name]; dup {
				return nil, fmt.Errorf("guest manifest %q records %q twice", path, name)
			}
			out[name] = digest
			continue
		}
		return nil, fmt.Errorf("guest manifest %q has a malformed line %q: want `<64 lowercase hex>  <basename>`", path, line)
	}
	return out, nil
}

// hashGuestFile returns the lowercase-hex sha256 of a materialised asset,
// refusing anything that is not a regular file (a symlink or device at an asset
// path would make the hash meaningless).
func hashGuestFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the path is a fixed basename inside the stack-owned guest dir
	if err != nil {
		return "", fmt.Errorf("open guest asset %q: %w", path, err)
	}
	defer func() {
		_ = f.Close() // read-only handle; a close error cannot change the computed digest
	}()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat guest asset %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("guest asset %q is not a regular file (mode %s)", path, info.Mode())
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash guest asset %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// resolveGuestDir validates the air-gapped bypass directory and returns its
// fixed paths, fetching nothing. It checks the four files are present, non-empty
// regular files; hash verification is the runner's own manifest check, since
// this directory is operator-supplied rather than content-addressed.
func resolveGuestDir(dir string) (GuestPaths, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return GuestPaths{}, fmt.Errorf("guest dir %q: %w", dir, err)
	}
	if !info.IsDir() {
		return GuestPaths{}, fmt.Errorf("guest dir %q is not a directory", dir)
	}
	paths := guestPathsIn(dir)
	for _, path := range append(paths.assets(), paths.Manifest) {
		info, err := os.Stat(path)
		if err != nil {
			return GuestPaths{}, fmt.Errorf("guest dir %q: %w", dir, err)
		}
		if !info.Mode().IsRegular() {
			return GuestPaths{}, fmt.Errorf("guest asset %q is not a regular file (mode %s)", path, info.Mode())
		}
		if info.Size() == 0 {
			return GuestPaths{}, fmt.Errorf("guest asset %q is empty", path)
		}
	}
	return paths, nil
}

// isSHA256Digest reports whether d is a sha256:<64 lowercase hex> descriptor
// digest. Lowercase only, for the same reason isDigestPinned insists on it: the
// digest keys a content-addressed directory.
func isSHA256Digest(d string) bool {
	return strings.HasPrefix(d, guestDigestPrefix) && isLowerHex64(strings.TrimPrefix(d, guestDigestPrefix))
}

// isLowerHex64 reports whether s is exactly 64 lowercase hex characters — one
// sha256 digest's worth.
func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, ch := range s {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}
