//go:build unix

// The Runner-side ConfigMaterializer: turn a fetched fleet config bundle into a
// versioned host dir under root whose `current` symlink a live container can
// follow. The container mounts the PARENT dir (root) read-only, so an atomic
// flip of `current` becomes visible inside the running container without a
// remount. Unpack goes into a staging dir then renames into place, so a crashed
// unpack never leaves a half-written version dir the flip could point at. The
// tarball is untrusted: every validation (size/count caps, traversal, symlink/
// hardlink, layout) is re-enforced here at unpack, failing closed before any
// byte is written.
package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"connectrpc.com/connect"
)

const (
	// maxConfigBytes caps the total decompressed size of a config bundle. A
	// fleet config (skills, extensions, mcp descriptors) is small text; 64 MiB
	// is far above any legitimate bundle yet bounds a gzip bomb well below host
	// memory pressure. Enforced during streamed decompression, never after.
	maxConfigBytes = 64 << 20
	// maxConfigFiles caps the number of members in a config bundle. A fleet has
	// tens of skills/extensions/mcp files; 10k is generous headroom while still
	// bounding a tar-of-many-tiny-files DoS. Enforced during streaming.
	maxConfigFiles = 10_000
)

// configTopLevelName matches the FIRST-level entry name under skills/,
// extensions/, or mcp/ (e.g. the "greet" in skills/greet/...). Only this
// first-level name is regex-constrained to a conservative safe set; deeper
// segments of a nested member rely on the traversal + containment guards
// (absolute/".." rejection and the destDir-prefix check) rather than this
// regex, so a nested file may carry a name this pattern would reject and still
// land safely contained under its version dir.
var configTopLevelName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// configTopDirs are the only permitted top-level directories in a config bundle.
var configTopDirs = map[string]struct{}{
	"skills":     {},
	"extensions": {},
	"mcp":        {},
}

// configFetcher is the T3 fetch seam the materializer pulls through. *ServerLink
// satisfies it; tests supply a fake. (Runner-internal, not a wire contract.)
type configFetcher interface {
	FetchAgentConfig(ctx context.Context, ifVersion string) (AgentConfigBundle, error)
}

// ConfigMaterializer turns a fetched config bundle into a versioned host dir
// under root, with an atomically-flipped `current` symlink a live container
// (which mounts the PARENT dir read-only) can follow.
type ConfigMaterializer struct {
	root  string // <runner-state>/config — the parent dir mounted into the container
	fetch configFetcher
	log   *slog.Logger
}

// NewConfigMaterializer builds a materializer rooted at root, fetching bundles
// through fetch. A nil log falls back to slog.Default, matching
// NewSecretMaterializer.
func NewConfigMaterializer(root string, fetch configFetcher, log *slog.Logger) *ConfigMaterializer {
	if log == nil {
		log = slog.Default()
	}
	return &ConfigMaterializer{root: root, fetch: fetch, log: log}
}

// ConfigMount is what T5 mounts: HostPath is the PARENT dir (root), never the
// resolved version dir — mounting the parent keeps a later `current` flip visible
// inside a live container.
type ConfigMount struct {
	HostPath string
	Version  string
}

// Materialize fetches the current fleet bundle, validates + unpacks it into
// <root>/<version>/, (on the update path only) relabels that dir into the
// container's SELinux MCS category, atomically flips <root>/current -> <version>,
// prunes superseded version dirs, and returns the mount.
//
// mcsLabel is "" on the PROVISION path (T5): Materialize runs before the
// container exists, so there is no label to target and the create-time :Z relabel
// covers the whole tree — skip chcon. mcsLabel is non-empty on the UPDATE path
// (T6, read via `podman inspect` MountLabel): the version dir is written into an
// already-mounted tree and carries the Runner's label, so chcon -R it into the
// container's MCS category AFTER writing and BEFORE the flip, or a confined agent
// gets EACCES.
//
// The mcsLabel parameter resolves a contract point: the frozen record's prose
// puts the chcon inside Materialize on the update path only, between unpack and
// the flip, but the literal Materialize(ctx) signature can't distinguish
// provision from update — so the label is threaded as a parameter (empty =
// provision/skip, non-empty = update/chcon).
func (m *ConfigMaterializer) Materialize(ctx context.Context, mcsLabel string) (ConfigMount, error) {
	bundle, err := m.fetch.FetchAgentConfig(ctx, "")
	if err != nil {
		// CodeFailedPrecondition is the Server's "no config surface" signal — no
		// config store is wired to serve FetchAgentConfig (runnerhub handler
		// contract). It is deliberately distinct from the CodeUnavailable of a
		// transient transport fault: the Runner reads it as "no config to inject"
		// and provisions anyway, exactly as it treats an unconfigured fleet. Any
		// other error (transport CodeUnavailable, store CodeInternal, a contract
		// skew) is a genuine fault that must abort provision, so it propagates.
		if connect.CodeOf(err) == connect.CodeFailedPrecondition {
			// Logged at Warn so a config-less provision is visible — it is a
			// degraded posture even when intended, mirroring the secrets path.
			m.log.WarnContext(ctx, "no config surface; provisioning without materialized config")
			if rootErr := m.ensureRoot(); rootErr != nil {
				return ConfigMount{}, rootErr
			}
			return ConfigMount{HostPath: m.root, Version: ""}, nil
		}
		return ConfigMount{}, fmt.Errorf("fetching agent config: %w", err)
	}

	// Unconfigured fleet: no config to materialize. Ensure root exists so T5 can
	// always mount it, but create no `current` symlink.
	if bundle.Version == "" {
		if err := m.ensureRoot(); err != nil {
			return ConfigMount{}, err
		}
		return ConfigMount{HostPath: m.root, Version: ""}, nil
	}

	versionDir := filepath.Join(m.root, bundle.Version)

	// Idempotent re-materialize: a prior Materialize already wrote this version.
	// Skip unpack; still (re-)flip current, chcon (update path), and prune.
	if _, statErr := os.Stat(versionDir); statErr != nil {
		if !errors.Is(statErr, os.ErrNotExist) {
			return ConfigMount{}, fmt.Errorf("stat version dir %q: %w", versionDir, statErr)
		}
		if err := m.unpackVersion(bundle, versionDir); err != nil {
			return ConfigMount{}, err
		}
	}

	if mcsLabel != "" {
		if err := relabel(ctx, mcsLabel, versionDir); err != nil {
			return ConfigMount{}, fmt.Errorf("relabeling version dir %q: %w", versionDir, err)
		}
	}

	if err := m.flipCurrent(bundle.Version); err != nil {
		return ConfigMount{}, err
	}

	m.prune(bundle.Version)

	return ConfigMount{HostPath: m.root, Version: bundle.Version}, nil
}

// ensureRoot creates the config root and pins it to 0755. podman bind-mounts the
// root read-only into the container and exposes the source dir's own perms at the
// mountpoint, so the confined agent (a distinct uid under keep-id) must be able
// to traverse INTO root to resolve current/ -> versionDir. os.MkdirAll requests
// 0755 but a non-022 Runner umask masks that down, so chmod pins it independent
// of ambient umask — the parent-level counterpart to pinConfigModes on the tree.
func (m *ConfigMaterializer) ensureRoot() error {
	if err := os.MkdirAll(m.root, 0o755); err != nil { //nolint:gosec // G301: the config root is mounted read-only into the container; it must be 0755 for the confined agent to traverse it
		return fmt.Errorf("ensuring config root %q: %w", m.root, err)
	}
	if err := os.Chmod(m.root, 0o755); err != nil { //nolint:gosec // G302: pin 0755 independent of the Runner umask; see the mode rationale above
		return fmt.Errorf("pinning config root %q mode: %w", m.root, err)
	}
	return nil
}

// unpackVersion validates + unpacks the bundle into a staging dir then atomically
// renames it into versionDir, and writes the observability version file. Staging
// is removed on any error so a crashed unpack leaves no half-written version dir.
func (m *ConfigMaterializer) unpackVersion(bundle AgentConfigBundle, versionDir string) error {
	if err := m.ensureRoot(); err != nil {
		return err
	}

	staging, err := os.MkdirTemp(m.root, ".staging-"+bundle.Version+"-")
	if err != nil {
		return fmt.Errorf("creating staging dir: %w", err)
	}
	// Staging stays at MkdirTemp's 0700 while untrusted content is written into
	// it; pinConfigModes widens the whole tree to 0755/0644 just before promote.
	cleanup := true
	defer func() {
		if cleanup {
			if rmErr := os.RemoveAll(staging); rmErr != nil {
				m.log.Warn("removing config staging dir", "staging_dir", staging, "error", rmErr)
			}
		}
	}()

	if err := validateAndUnpack(bundle.Tarball, staging); err != nil {
		return fmt.Errorf("validating config bundle %q: %w", bundle.Version, err)
	}

	if err := os.WriteFile(filepath.Join(staging, "version"), []byte(bundle.Version), 0o644); err != nil { //nolint:gosec // G306: the version file is read inside the container for observability; 0644 is required
		return fmt.Errorf("writing version file: %w", err)
	}

	// Pin modes across the whole staging tree before promoting it: os.MkdirAll
	// and os.WriteFile above requested 0755/0644 but a non-022 process umask
	// masks that down, and MkdirTemp forced the staging root to 0700. Under a
	// restrictive umask a nested dir loses its traverse bit and the confined
	// container agent (a distinct uid under keep-id) hits EACCES walking the
	// mounted tree. Pinning here — after unpack, before the rename carries the
	// modes across — makes the documented 0755-dir/0644-file invariant hold
	// regardless of ambient umask.
	if err := pinConfigModes(staging); err != nil {
		return fmt.Errorf("pinning config tree modes: %w", err)
	}

	if err := os.Rename(staging, versionDir); err != nil {
		return fmt.Errorf("promoting staging dir to %q: %w", versionDir, err)
	}
	cleanup = false
	return nil
}

// pinConfigModes walks dir and chmods every directory to 0755 and every regular
// file to 0644, so the unpacked tree carries the documented modes independent of
// the Runner process umask (the os.MkdirAll/os.WriteFile requested modes are
// masked down by a non-022 umask). validateAndUnpack admits only dirs and
// regular files, so the walk never encounters — nor follows — a symlink.
func pinConfigModes(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if d.IsDir() {
			mode = 0o755
		}
		if err := os.Chmod(p, mode); err != nil { //nolint:gosec // G302: the mounted config tree must be 0755/0644 for the confined agent to traverse + read
			return fmt.Errorf("chmod %q: %w", p, err)
		}
		return nil
	})
}

// flipCurrent atomically points <root>/current at version (a relative sibling
// target). A temp symlink is written then renamed over current, since a plain
// os.Symlink fails if current exists and rename is atomic on one fs.
func (m *ConfigMaterializer) flipCurrent(version string) error {
	tmp, err := os.MkdirTemp(m.root, ".curlink-")
	if err != nil {
		return fmt.Errorf("creating symlink temp dir: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(tmp); rmErr != nil {
			m.log.Warn("removing symlink temp dir", "temp_dir", tmp, "error", rmErr)
		}
	}()

	tmpLink := filepath.Join(tmp, "current")
	if err := os.Symlink(version, tmpLink); err != nil {
		return fmt.Errorf("writing temp current symlink: %w", err)
	}
	if err := os.Rename(tmpLink, filepath.Join(m.root, "current")); err != nil {
		return fmt.Errorf("flipping current symlink: %w", err)
	}
	return nil
}

// prune removes every version dir under root other than keep. The Server is the
// store of record (old versions are re-fetchable), so the host keeps only
// current. A prune failure is non-fatal: the flip already succeeded and a
// leftover dir is harmless, so log at Warn and return.
//
// Dot-prefixed entries (the .staging-/.curlink- temp dirs) are deliberately
// skipped: a concurrent Materialize's live unpack or flip is using one, and
// pruning it mid-write would corrupt that run. The cost is that a temp dir
// orphaned by a crash between MkdirTemp and its deferred RemoveAll is never
// reclaimed here — a small, bounded leak (one aborted bundle each) accepted in
// favor of never racing a concurrent writer.
func (m *ConfigMaterializer) prune(keep string) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		m.log.Warn("reading config root for prune", "root", m.root, "error", err)
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name == keep || name == "current" || strings.HasPrefix(name, ".") {
			continue
		}
		p := filepath.Join(m.root, name)
		if err := os.RemoveAll(p); err != nil {
			m.log.Warn("pruning superseded config version dir", "version_dir", p, "error", err)
		}
	}
}

// relabel runs chcon -R to move destDir into the container's SELinux MCS
// category (update path only). It is a package var so tests can stub the host
// shellout; the production value uses exec.CommandContext.
var relabel = func(ctx context.Context, mcsLabel, destDir string) error {
	cmd := exec.CommandContext(ctx, "chcon", "-R", mcsLabel, destDir) //nolint:gosec // G204: the SELinux relabel seam — mcsLabel is a Runner-read podman MountLabel and destDir is a Runner-built path, neither user-controlled
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("chcon %s %s: %w: %s", mcsLabel, destDir, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// validateAndUnpack streams the gzip-tar bundle into destDir, enforcing every
// security guard (size/count caps, traversal, symlink/hardlink, layout) BEFORE
// writing each member and failing closed on the first violation. destDir must
// already exist. It is a focused helper so tests can drive it directly with
// crafted tarballs.
func validateAndUnpack(tarball []byte, destDir string) error {
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return fmt.Errorf("opening gzip reader: %w", err)
	}
	defer func() { _ = gz.Close() }() // read-only reader: close error is not actionable

	// limited bounds the decompressed stream: the moment a member's bytes would
	// push the running total past maxConfigBytes, the copy errors — a gzip bomb
	// cannot force us to decompress the whole thing first.
	tr := tar.NewReader(gz)

	var (
		totalBytes int64
		fileCount  int
	)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}

		fileCount++
		if fileCount > maxConfigFiles {
			return fmt.Errorf("config bundle exceeds file-count cap of %d", maxConfigFiles)
		}

		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			return fmt.Errorf("config bundle member %q has disallowed type %d (only regular files and dirs allowed)", hdr.Name, hdr.Typeflag)
		}

		clean, err := validateMemberPath(hdr.Name, hdr.Typeflag)
		if err != nil {
			return err
		}

		target := filepath.Join(destDir, clean)
		// Belt-and-suspenders: the cleaned target must stay within destDir.
		if target != destDir && !strings.HasPrefix(target, destDir+string(os.PathSeparator)) {
			return fmt.Errorf("config bundle member %q escapes the version dir", hdr.Name)
		}

		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil { //nolint:gosec // G301: unpacked config dirs must be 0755 so the confined agent can traverse the mounted tree
				return fmt.Errorf("creating dir %q: %w", target, err)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // G301: unpacked config dirs must be 0755 so the confined agent can traverse the mounted tree
			return fmt.Errorf("creating parent dir for %q: %w", target, err)
		}

		remaining := maxConfigBytes - totalBytes
		// +1 so a member exactly filling the budget still leaves detectable
		// overrun room: if it reads more than `remaining`, the cap is exceeded.
		buf, n, err := readCapped(tr, remaining+1)
		if err != nil {
			return fmt.Errorf("reading member %q: %w", hdr.Name, err)
		}
		if n > remaining {
			return fmt.Errorf("config bundle exceeds decompressed-size cap of %d bytes", maxConfigBytes)
		}
		totalBytes += n

		if strings.HasPrefix(clean, "mcp/") {
			if !json.Valid(buf) {
				return fmt.Errorf("config bundle mcp member %q is not valid JSON", hdr.Name)
			}
		}

		if err := os.WriteFile(target, buf, 0o644); err != nil { //nolint:gosec // G306: unpacked config files must be 0644 so the confined agent can read them from the mounted tree
			return fmt.Errorf("writing member %q: %w", target, err)
		}
	}
	return nil
}

// readCapped reads up to limit bytes from r, returning the bytes and the count.
// It reads limit+? via io.LimitReader semantics handled by the caller: n may
// equal limit, which the caller treats as an overrun signal.
func readCapped(r io.Reader, limit int64) ([]byte, int64, error) {
	buf, err := io.ReadAll(io.LimitReader(r, limit))
	if err != nil {
		return nil, 0, err
	}
	return buf, int64(len(buf)), nil
}

// validateMemberPath enforces traversal + layout rules on a tar member name and
// returns the cleaned relative path. It rejects absolute paths, `..` segments,
// and anything not under a permitted top-level dir with a safe name.
func validateMemberPath(name string, typeflag byte) (string, error) {
	if name == "" {
		return "", errors.New("config bundle member has an empty name")
	}
	if path.IsAbs(name) || filepath.IsAbs(name) {
		return "", fmt.Errorf("config bundle member %q is an absolute path", name)
	}

	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", fmt.Errorf("config bundle member %q contains a traversal segment", name)
	}
	if clean == "." || clean == "/" {
		return "", fmt.Errorf("config bundle member %q resolves outside the bundle", name)
	}

	parts := strings.Split(strings.TrimSuffix(clean, "/"), "/")
	top := parts[0]
	if _, ok := configTopDirs[top]; !ok {
		return "", fmt.Errorf("config bundle member %q is not under skills/, extensions/, or mcp/", name)
	}

	// A bare top-level dir entry (e.g. "skills/") is allowed as a container dir.
	if len(parts) == 1 {
		if typeflag != tar.TypeDir {
			return "", fmt.Errorf("config bundle top-level %q must be a directory", name)
		}
		return clean, nil
	}

	entry := parts[1]
	if top == "mcp" {
		// mcp/<name>.json — validate the base name sans .json, require suffix on files.
		if typeflag == tar.TypeReg {
			if !strings.HasSuffix(entry, ".json") {
				return "", fmt.Errorf("config bundle mcp member %q must have a .json suffix", name)
			}
			base := strings.TrimSuffix(entry, ".json")
			if !configTopLevelName.MatchString(base) {
				return "", fmt.Errorf("config bundle mcp member name %q is not a safe name", entry)
			}
			return clean, nil
		}
	}

	if !configTopLevelName.MatchString(entry) {
		return "", fmt.Errorf("config bundle member name %q is not a safe name", entry)
	}
	return clean, nil
}
