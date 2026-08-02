//go:build unix

// The Runner-side ConfigMaterializer turns an untrusted fetched bundle into a
// versioned host dir with an atomically-flipped `current` symlink. Every case
// pins a contract a plausible bug would break: the happy path must land files +
// modes + version file + a resolving symlink; an unconfigured fleet must still
// yield a mountable empty root with no symlink; a re-materialize must be
// idempotent; a version change must flip and prune; and — the security core —
// every validation guard (traversal, absolute, symlink, hardlink, bad name,
// non-JSON mcp, file-count cap, decompressed-size cap) must fail closed, writing
// no version dir and no `current`. A fetch error must propagate wrapped.
package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// fakeConfigFetcher returns a canned bundle (or error), recording the requested
// if_version so the unkeyed fetch contract can be asserted.
type fakeConfigFetcher struct {
	bundle     AgentConfigBundle
	err        error
	gotVersion string
	calls      int
}

func (f *fakeConfigFetcher) FetchAgentConfig(_ context.Context, ifVersion string) (AgentConfigBundle, error) {
	f.calls++
	f.gotVersion = ifVersion
	if f.err != nil {
		return AgentConfigBundle{}, f.err
	}
	return f.bundle, nil
}

// buildConfigTarball builds an in-memory gzip+tar bundle from entries keyed by
// member path. A trailing "/" key is written as a directory entry.
func buildConfigTarball(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range entries {
		if strings.HasSuffix(name, "/") {
			if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
				t.Fatalf("write dir header %q: %v", name, err)
			}
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}); err != nil {
			t.Fatalf("write header %q: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("write body %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// buildRawTarball builds a gzip+tar from raw headers so a test can craft
// symlink/hardlink/absolute members the high-level builder would not produce.
func buildRawTarball(t *testing.T, hdrs []tar.Header, bodies map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := range hdrs {
		h := hdrs[i]
		if body, ok := bodies[h.Name]; ok {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatalf("write raw header %q: %v", h.Name, err)
		}
		if body, ok := bodies[h.Name]; ok {
			if _, err := tw.Write(body); err != nil {
				t.Fatalf("write raw body %q: %v", h.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func validBundle() map[string][]byte {
	return map[string][]byte{
		"skills/greet":                  []byte("hello skill\n"),
		"skills/farewell":               []byte("bye skill\n"),
		"skills/helper/references/n.md": []byte("nested note\n"),
		"extensions/theme":              []byte("ext body\n"),
		"mcp/tool.json":                 []byte(`{"name":"tool"}`),
	}
}

func TestConfigMaterializeHappyPath(t *testing.T) {
	root := t.TempDir()
	tarball := buildConfigTarball(t, validBundle())
	f := &fakeConfigFetcher{bundle: AgentConfigBundle{Version: "v1", Tarball: tarball}}
	m := NewConfigMaterializer(root, f, nil)

	mount, err := m.Materialize(context.Background(), "")
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if mount.HostPath != root || mount.Version != "v1" {
		t.Fatalf("mount = %+v, want HostPath=%q Version=v1", mount, root)
	}
	if f.gotVersion != "" {
		t.Fatalf("fetch if_version = %q, want empty (unkeyed)", f.gotVersion)
	}

	versionDir := filepath.Join(root, "v1")
	// File contents.
	got, err := os.ReadFile(filepath.Join(versionDir, "skills", "greet"))
	if err != nil || string(got) != "hello skill\n" {
		t.Fatalf("skills/greet = %q err=%v", got, err)
	}
	// Version file.
	vf, err := os.ReadFile(filepath.Join(versionDir, "version"))
	if err != nil || string(vf) != "v1" {
		t.Fatalf("version file = %q err=%v", vf, err)
	}
	// File mode 0644, dir mode 0755.
	fi, err := os.Stat(filepath.Join(versionDir, "skills", "greet"))
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("greet mode = %v err=%v", fi.Mode().Perm(), err)
	}
	di, err := os.Stat(filepath.Join(versionDir, "skills"))
	if err != nil || di.Mode().Perm() != 0o755 {
		t.Fatalf("skills dir mode = %v err=%v", di.Mode().Perm(), err)
	}
	// The version dir itself must be 0755: it is the traversal root the confined
	// container agent (a distinct uid) walks through to reach current/.
	vdi, err := os.Stat(versionDir)
	if err != nil || vdi.Mode().Perm() != 0o755 {
		t.Fatalf("version dir mode = %v err=%v, want 0755", vdi.Mode().Perm(), err)
	}
	// Nested member: an intermediate dir created by MkdirAll must also be 0755
	// and the deep file 0644 — the umask-independent mode pin covers the whole
	// unpacked tree, not just top-level entries.
	nested, err := os.ReadFile(filepath.Join(versionDir, "skills", "helper", "references", "n.md"))
	if err != nil || string(nested) != "nested note\n" {
		t.Fatalf("nested member = %q err=%v", nested, err)
	}
	ndi, err := os.Stat(filepath.Join(versionDir, "skills", "helper", "references"))
	if err != nil || ndi.Mode().Perm() != 0o755 {
		t.Fatalf("nested dir mode = %v err=%v, want 0755", ndi.Mode().Perm(), err)
	}
	nfi, err := os.Stat(filepath.Join(versionDir, "skills", "helper", "references", "n.md"))
	if err != nil || nfi.Mode().Perm() != 0o644 {
		t.Fatalf("nested file mode = %v err=%v, want 0644", nfi.Mode().Perm(), err)
	}
	// current symlink resolves to the version dir.
	target, err := os.Readlink(filepath.Join(root, "current"))
	if err != nil {
		t.Fatalf("readlink current: %v", err)
	}
	if target != "v1" {
		t.Fatalf("current -> %q, want relative v1", target)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, "current"))
	if err != nil {
		t.Fatalf("eval current: %v", err)
	}
	wantResolved, _ := filepath.EvalSymlinks(versionDir)
	if resolved != wantResolved {
		t.Fatalf("current resolves to %q, want %q", resolved, wantResolved)
	}
}

func TestConfigMaterializeUnconfiguredFleet(t *testing.T) {
	root := filepath.Join(t.TempDir(), "config")
	f := &fakeConfigFetcher{bundle: AgentConfigBundle{}}
	m := NewConfigMaterializer(root, f, nil)

	mount, err := m.Materialize(context.Background(), "")
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if mount.HostPath != root || mount.Version != "" {
		t.Fatalf("mount = %+v, want HostPath=%q Version empty", mount, root)
	}
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		t.Fatalf("root not a dir: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "current")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("current should not exist, lstat err = %v", err)
	}
}

func TestConfigMaterializeIdempotent(t *testing.T) {
	root := t.TempDir()
	tarball := buildConfigTarball(t, validBundle())
	f := &fakeConfigFetcher{bundle: AgentConfigBundle{Version: "v1", Tarball: tarball}}
	m := NewConfigMaterializer(root, f, nil)

	if _, err := m.Materialize(context.Background(), ""); err != nil {
		t.Fatalf("first Materialize: %v", err)
	}
	if _, err := m.Materialize(context.Background(), ""); err != nil {
		t.Fatalf("second Materialize: %v", err)
	}
	target, err := os.Readlink(filepath.Join(root, "current"))
	if err != nil || target != "v1" {
		t.Fatalf("current -> %q err=%v", target, err)
	}
	// Only the v1 dir plus current remain (no staging leftovers).
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("leftover temp entry: %q", e.Name())
		}
	}
}

func TestConfigMaterializeVersionChangePrunes(t *testing.T) {
	root := t.TempDir()
	f := &fakeConfigFetcher{bundle: AgentConfigBundle{Version: "v1", Tarball: buildConfigTarball(t, validBundle())}}
	m := NewConfigMaterializer(root, f, nil)
	if _, err := m.Materialize(context.Background(), ""); err != nil {
		t.Fatalf("materialize v1: %v", err)
	}

	f.bundle = AgentConfigBundle{Version: "v2", Tarball: buildConfigTarball(t, validBundle())}
	if _, err := m.Materialize(context.Background(), ""); err != nil {
		t.Fatalf("materialize v2: %v", err)
	}

	target, err := os.Readlink(filepath.Join(root, "current"))
	if err != nil || target != "v2" {
		t.Fatalf("current -> %q err=%v", target, err)
	}
	if _, err := os.Stat(filepath.Join(root, "v1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("v1 dir should be pruned, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "v2")); err != nil {
		t.Fatalf("v2 dir should exist: %v", err)
	}
}

func TestConfigMaterializeRejectsMalicious(t *testing.T) {
	cases := map[string][]byte{
		"traversal": buildConfigTarball(t, map[string][]byte{
			"skills/sub/../../../escape": []byte("x"),
		}),
		"absolute": buildRawTarball(t, []tar.Header{
			{Name: "/etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644},
		}, map[string][]byte{"/etc/passwd": []byte("x")}),
		"symlink": buildRawTarball(t, []tar.Header{
			{Name: "skills/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777},
		}, nil),
		"hardlink": buildRawTarball(t, []tar.Header{
			{Name: "skills/hard", Typeflag: tar.TypeLink, Linkname: "skills/greet", Mode: 0o644},
		}, nil),
		"bad_name": buildConfigTarball(t, map[string][]byte{
			"skills/bad name!": []byte("x"),
		}),
		"bad_mcp_json": buildConfigTarball(t, map[string][]byte{
			"mcp/tool.json": []byte("{not json"),
		}),
	}
	for name, tarball := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			f := &fakeConfigFetcher{bundle: AgentConfigBundle{Version: "vbad", Tarball: tarball}}
			m := NewConfigMaterializer(root, f, nil)
			if _, err := m.Materialize(context.Background(), ""); err == nil {
				t.Fatalf("expected rejection, got nil error")
			}
			if _, err := os.Stat(filepath.Join(root, "vbad")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("version dir must not be written, stat err = %v", err)
			}
			if _, err := os.Lstat(filepath.Join(root, "current")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("current must not exist, lstat err = %v", err)
			}
		})
	}
}

func TestConfigMaterializeFileCountCap(t *testing.T) {
	entries := make(map[string][]byte, maxConfigFiles+10)
	for i := range maxConfigFiles + 5 {
		entries["skills/f"+strconv.Itoa(i)] = []byte("x")
	}
	tarball := buildConfigTarball(t, entries)
	root := t.TempDir()
	f := &fakeConfigFetcher{bundle: AgentConfigBundle{Version: "vcap", Tarball: tarball}}
	m := NewConfigMaterializer(root, f, nil)
	if _, err := m.Materialize(context.Background(), ""); err == nil {
		t.Fatalf("expected file-count cap rejection")
	}
	if _, err := os.Stat(filepath.Join(root, "vcap")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("version dir must not be written, stat err = %v", err)
	}
}

func TestConfigMaterializeSizeCap(t *testing.T) {
	// One member whose decompressed size exceeds maxConfigBytes. Highly
	// compressible zeros keep the gzip small while the stream trips the cap.
	big := make([]byte, maxConfigBytes+1024)
	tarball := buildConfigTarball(t, map[string][]byte{"skills/big": big})
	root := t.TempDir()
	f := &fakeConfigFetcher{bundle: AgentConfigBundle{Version: "vbig", Tarball: tarball}}
	m := NewConfigMaterializer(root, f, nil)
	if _, err := m.Materialize(context.Background(), ""); err == nil {
		t.Fatalf("expected decompressed-size cap rejection")
	}
	if _, err := os.Stat(filepath.Join(root, "vbig")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("version dir must not be written, stat err = %v", err)
	}
}

func TestConfigMaterializeFetchErrorPropagates(t *testing.T) {
	sentinel := errors.New("boom")
	root := t.TempDir()
	f := &fakeConfigFetcher{err: sentinel}
	m := NewConfigMaterializer(root, f, nil)
	_, err := m.Materialize(context.Background(), "")
	if !errors.Is(err, sentinel) {
		t.Fatalf("Materialize err = %v, want wrapped sentinel", err)
	}
	entries, rerr := os.ReadDir(root)
	if rerr != nil {
		t.Fatalf("readdir: %v", rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("nothing should be written on fetch error, got %d entries", len(entries))
	}
}

// The unpacked tree must carry 0755 dirs / 0644 files regardless of the Runner
// process umask: the confined container agent is a distinct uid (keep-id) that
// needs the traverse + read bits. Under a restrictive umask the os.MkdirAll /
// os.WriteFile requested modes are masked down, so the materializer re-pins them.
// RED before pinConfigModes: under umask 0o077 a nested dir lands 0700 and the
// file 0600, and these assertions fail.
func TestConfigMaterializeModesUnderRestrictiveUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	root := t.TempDir()
	tarball := buildConfigTarball(t, validBundle())
	f := &fakeConfigFetcher{bundle: AgentConfigBundle{Version: "v1", Tarball: tarball}}
	m := NewConfigMaterializer(root, f, nil)
	if _, err := m.Materialize(context.Background(), ""); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	versionDir := filepath.Join(root, "v1")
	checks := []struct {
		path string
		want os.FileMode
	}{
		// The config root itself: podman bind-mounts it read-only and exposes its
		// perms at the mountpoint, so the confined agent must traverse INTO it.
		{root, 0o755},
		{versionDir, 0o755},
		{filepath.Join(versionDir, "skills"), 0o755},
		{filepath.Join(versionDir, "skills", "helper", "references"), 0o755},
		{filepath.Join(versionDir, "skills", "greet"), 0o644},
		{filepath.Join(versionDir, "skills", "helper", "references", "n.md"), 0o644},
		{filepath.Join(versionDir, "version"), 0o644},
	}
	for _, c := range checks {
		fi, err := os.Stat(c.path)
		if err != nil {
			t.Fatalf("stat %q: %v", c.path, err)
		}
		if fi.Mode().Perm() != c.want {
			t.Fatalf("%q mode = %v, want %v (umask-independent)", c.path, fi.Mode().Perm(), c.want)
		}
	}
}

// The decompressed-size cap is a RUNNING TOTAL across members, not a per-member
// limit: several individually-under-cap members whose sum exceeds maxConfigBytes
// must trip it. RED if totalBytes reset per member instead of accumulating.
func TestConfigMaterializeSizeCapAccumulates(t *testing.T) {
	half := make([]byte, maxConfigBytes/2+1024)
	tarball := buildConfigTarball(t, map[string][]byte{
		"skills/a": half,
		"skills/b": half,
	})
	root := t.TempDir()
	f := &fakeConfigFetcher{bundle: AgentConfigBundle{Version: "vsum", Tarball: tarball}}
	m := NewConfigMaterializer(root, f, nil)
	if _, err := m.Materialize(context.Background(), ""); err == nil {
		t.Fatalf("expected size-cap rejection from accumulated members")
	}
	if _, err := os.Stat(filepath.Join(root, "vsum")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("version dir must not be written, stat err = %v", err)
	}
}

// relabelCall records one stubbed relabel invocation and the filesystem state at
// the moment it ran, so the update-path ordering contract can be asserted.
type relabelCall struct {
	mcsLabel        string
	destDir         string
	versionDirThere bool
	currentPointsAt string // Readlink(current) at call time; "" if absent
}

// stubRelabel swaps the package-level relabel var for the duration of a test and
// records each call. It is NOT parallel-safe (relabel is global); callers must
// not t.Parallel.
func stubRelabel(t *testing.T, root string) *[]relabelCall {
	t.Helper()
	var calls []relabelCall
	old := relabel
	relabel = func(_ context.Context, mcsLabel, destDir string) error {
		c := relabelCall{mcsLabel: mcsLabel, destDir: destDir}
		if _, err := os.Stat(destDir); err == nil {
			c.versionDirThere = true
		}
		if target, err := os.Readlink(filepath.Join(root, "current")); err == nil {
			c.currentPointsAt = target
		}
		calls = append(calls, c)
		return nil
	}
	t.Cleanup(func() { relabel = old })
	return &calls
}

// On the update path (mcsLabel != "") relabel runs exactly once, targets the
// version dir, runs AFTER the version dir exists and BEFORE current flips to it.
// RED if the chcon is dropped (zero calls), mis-targeted, or reordered past the
// flip (currentPointsAt would already be the new version).
func TestConfigMaterializeUpdatePathRelabelsBeforeFlip(t *testing.T) {
	root := t.TempDir()
	calls := stubRelabel(t, root)
	tarball := buildConfigTarball(t, validBundle())
	f := &fakeConfigFetcher{bundle: AgentConfigBundle{Version: "v1", Tarball: tarball}}
	m := NewConfigMaterializer(root, f, nil)

	if _, err := m.Materialize(context.Background(), "system_u:object_r:container_file_t:s0:c1,c2"); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("relabel calls = %d, want exactly 1", len(*calls))
	}
	c := (*calls)[0]
	if c.destDir != filepath.Join(root, "v1") {
		t.Fatalf("relabel destDir = %q, want the v1 version dir", c.destDir)
	}
	if !c.versionDirThere {
		t.Fatalf("relabel ran before the version dir existed (ordering: unpack -> relabel)")
	}
	if c.currentPointsAt == "v1" {
		t.Fatalf("relabel ran AFTER current flipped to v1 (ordering: relabel -> flip); current was already %q", c.currentPointsAt)
	}
	// current must resolve to v1 after Materialize returns.
	if target, err := os.Readlink(filepath.Join(root, "current")); err != nil || target != "v1" {
		t.Fatalf("current -> %q err=%v, want v1 after flip", target, err)
	}
}

// The genuine update transition (T6): a live v0 is already current when v1 is
// materialized with a label. relabel must still run on v1 BEFORE current flips
// off v0 — at relabel time current must still point at v0, never yet v1.
// RED if relabel is reordered past the flip: currentPointsAt would be "v1".
func TestConfigMaterializeUpdatePathRelabelsBeforeFlipOverExistingCurrent(t *testing.T) {
	root := t.TempDir()
	// Provision v0 first (no stub): current flips to v0.
	f := &fakeConfigFetcher{bundle: AgentConfigBundle{Version: "v0", Tarball: buildConfigTarball(t, validBundle())}}
	m := NewConfigMaterializer(root, f, nil)
	if _, err := m.Materialize(context.Background(), ""); err != nil {
		t.Fatalf("provision v0: %v", err)
	}
	if target, _ := os.Readlink(filepath.Join(root, "current")); target != "v0" {
		t.Fatalf("precondition: current -> %q, want v0", target)
	}

	// Now update to v1 with a label; stub relabel to record the flip state at
	// call time.
	calls := stubRelabel(t, root)
	f.bundle = AgentConfigBundle{Version: "v1", Tarball: buildConfigTarball(t, validBundle())}
	if _, err := m.Materialize(context.Background(), "mcs-label"); err != nil {
		t.Fatalf("update to v1: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("relabel calls = %d, want exactly 1 on the update", len(*calls))
	}
	c := (*calls)[0]
	if c.destDir != filepath.Join(root, "v1") {
		t.Fatalf("relabel destDir = %q, want the v1 version dir", c.destDir)
	}
	if c.currentPointsAt != "v0" {
		t.Fatalf("relabel saw current -> %q, want v0 (relabel must run BEFORE the flip off v0)", c.currentPointsAt)
	}
	if target, err := os.Readlink(filepath.Join(root, "current")); err != nil || target != "v1" {
		t.Fatalf("current -> %q err=%v, want v1 after the update flip", target, err)
	}
}

// A relabel failure aborts Materialize wrapped with "relabeling version dir",
// and does NOT flip current (a confined agent must never see a tree it cannot
// enter). RED if the error is swallowed or the flip still runs.
func TestConfigMaterializeUpdatePathRelabelErrorAborts(t *testing.T) {
	root := t.TempDir()
	sentinel := errors.New("chcon boom")
	old := relabel
	relabel = func(context.Context, string, string) error { return sentinel }
	t.Cleanup(func() { relabel = old })

	tarball := buildConfigTarball(t, validBundle())
	f := &fakeConfigFetcher{bundle: AgentConfigBundle{Version: "v1", Tarball: tarball}}
	m := NewConfigMaterializer(root, f, nil)

	_, err := m.Materialize(context.Background(), "mcs-label")
	if !errors.Is(err, sentinel) {
		t.Fatalf("Materialize err = %v, want wrapped sentinel", err)
	}
	if !strings.Contains(err.Error(), "relabeling version dir") {
		t.Fatalf("err = %q, want it to mention relabeling version dir", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "current")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("current must not flip when relabel fails, lstat err = %v", err)
	}
}

// The provision path (mcsLabel == "") must NOT relabel: the create-time :Z
// relabel covers the whole tree, and there is no container label yet. RED if
// Materialize relabels unconditionally.
func TestConfigMaterializeProvisionPathDoesNotRelabel(t *testing.T) {
	root := t.TempDir()
	calls := stubRelabel(t, root)
	tarball := buildConfigTarball(t, validBundle())
	f := &fakeConfigFetcher{bundle: AgentConfigBundle{Version: "v1", Tarball: tarball}}
	m := NewConfigMaterializer(root, f, nil)

	if _, err := m.Materialize(context.Background(), ""); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("relabel calls = %d, want 0 on the provision path", len(*calls))
	}
}
