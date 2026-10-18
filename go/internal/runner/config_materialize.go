//go:build unix

// The Runner-side ConfigMaterializer: turn a fetched config bundle into a
// versioned host dir whose `current` symlink a live container follows; an atomic
// flip is visible without a remount, and unpack stages then renames so a crash
// never leaves a half-written dir. The untrusted tarball is re-validated, failing closed.
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
	"os/exec" //nolint:depguard // config materializer: the SELinux `chcon` relabel seam (G204 site justified below)
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/RigelBuild/compass/go/internal/runner/gateway"
	yaml "go.yaml.in/yaml/v3"
)

const (
	// maxConfigBytes bounds decompressed bundles to limit gzip-bomb memory use.
	maxConfigBytes = 64 << 20
	// maxConfigFiles bounds archives with many tiny members.
	maxConfigFiles = 10_000
)

// configTopLevelName constrains first-level names; traversal and containment
// checks protect deeper segments.
var configTopLevelName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Bundle top-directory names shared with the store-door whitelist.
const (
	topDirSkills     = "skills"
	topDirExtensions = "extensions"
	topDirMCP        = "mcp"
	topDirSettings   = "settings"
	topDirRules      = "rules"
	topDirAgents     = "agents"
	topDirPrompts    = "prompts"
	topDirProfiles   = "profiles"
)

// configTopDirs are the only permitted top-level directories in a config bundle.
var configTopDirs = map[string]struct{}{
	topDirSkills:     {},
	topDirExtensions: {},
	topDirMCP:        {},
	topDirSettings:   {},
	topDirRules:      {},
	topDirAgents:     {},
	topDirPrompts:    {},
	topDirProfiles:   {},
}

// Admit only these top-level files and settings/config.yml; credential filtering
// remains exclusive to the store door.
const (
	memberAgentsMD = "AGENTS.md"
	memberModels   = "models.yml"
	settingsMember = "settings/config.yml"
	// memberSystemMD is the only filename admitted under prompts/<role>/
	// (RIG-3075 T2); structural twin of the store door.
	memberSystemMD = "SYSTEM.md"
	// memberProfileYML is the only filename admitted under profiles/<name>/
	// (RIG-2968 T1); structural twin of the store door.
	memberProfileYML = "profile.yml"
)

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
// The mcsLabel parameter resolves a contract point: the design record's prose
// puts the chcon inside Materialize on the update path only, between unpack and
// the flip, but the literal Materialize(ctx) signature can't distinguish
// provision from update — so the label is threaded as a parameter (empty =
// provision/skip, non-empty = update/chcon).
func (m *ConfigMaterializer) Materialize(ctx context.Context, mcsLabel string) (ConfigMount, error) {
	bundle, err := m.fetch.FetchAgentConfig(ctx, "")
	if err != nil {
		// CodeFailedPrecondition is the Server's "no config surface" signal, distinct
		// from the CodeUnavailable of a transient transport fault: the Runner reads
		// it as "no config to inject" and provisions anyway. Any other error is a
		// genuine fault that must abort provision, so it propagates.
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
		if err := m.unpackVersion(ctx, bundle, versionDir); err != nil {
			return ConfigMount{}, err
		}
	}

	if mcsLabel != "" {
		if err := relabel(ctx, mcsLabel, versionDir); err != nil {
			return ConfigMount{}, fmt.Errorf("relabeling version dir %q: %w", versionDir, err)
		}
	}

	if err := m.flipCurrent(ctx, bundle.Version); err != nil {
		return ConfigMount{}, err
	}

	m.prune(ctx, bundle.Version)

	return ConfigMount{HostPath: m.root, Version: bundle.Version}, nil
}

// ensureRoot keeps the read-only mount root traversable despite the Runner umask.
func (m *ConfigMaterializer) ensureRoot() error {
	if err := os.MkdirAll(m.root, 0o755); err != nil { //nolint:gosec // G301: the config root is mounted read-only into the container; it must be 0755 for the confined agent to traverse it
		return fmt.Errorf("ensuring config root %q: %w: %w", m.root, err, gateway.ErrOperatorConfig)
	}
	if err := os.Chmod(m.root, 0o755); err != nil { //nolint:gosec // G302: pin 0755 independent of the Runner umask; see the mode rationale above
		return fmt.Errorf("pinning config root %q mode: %w: %w", m.root, err, gateway.ErrOperatorConfig)
	}
	return nil
}

// unpackVersion validates into staging, then renames atomically; errors remove
// staging so incomplete versions are never exposed.
func (m *ConfigMaterializer) unpackVersion(ctx context.Context, bundle AgentConfigBundle, versionDir string) error {
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
				m.log.WarnContext(ctx, "removing config staging dir", "staging_dir", staging, "error", rmErr)
			}
		}
	}()

	if err := validateAndUnpack(bundle.Tarball, staging); err != nil {
		return fmt.Errorf("validating config bundle %q: %w", bundle.Version, err)
	}

	if err := os.WriteFile(filepath.Join(staging, "version"), []byte(bundle.Version), 0o644); err != nil { //nolint:gosec // G306: the version file is read inside the container for observability; 0644 is required
		return fmt.Errorf("writing version file: %w", err)
	}

	// Pin modes across the staging tree before promoting: a non-022 umask masks
	// the requested 0755/0644 down and MkdirTemp forced 0700, so a nested dir could
	// lose its traverse bit and the confined container agent hits EACCES walking
	// the tree. Pinning here makes the 0755-dir/0644-file invariant hold regardless.
	if err := pinConfigModes(staging); err != nil {
		return fmt.Errorf("pinning config tree modes: %w", err)
	}

	if err := os.Rename(staging, versionDir); err != nil {
		return fmt.Errorf("promoting staging dir to %q: %w", versionDir, err)
	}
	cleanup = false
	return nil
}

// pinConfigModes sets unpacked modes after validation excludes symlinks.
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
func (m *ConfigMaterializer) flipCurrent(ctx context.Context, version string) error {
	tmp, err := os.MkdirTemp(m.root, ".curlink-")
	if err != nil {
		return fmt.Errorf("creating symlink temp dir: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(tmp); rmErr != nil {
			m.log.WarnContext(ctx, "removing symlink temp dir", "temp_dir", tmp, "error", rmErr)
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

// prune keeps the current version and skips temporary dirs to avoid racing
// active materialization.
func (m *ConfigMaterializer) prune(ctx context.Context, keep string) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		m.log.WarnContext(ctx, "reading config root for prune", "root", m.root, "error", err)
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name == keep || name == "current" || strings.HasPrefix(name, ".") {
			continue
		}
		p := filepath.Join(m.root, name)
		if err := os.RemoveAll(p); err != nil {
			m.log.WarnContext(ctx, "pruning superseded config version dir", "version_dir", p, "error", err)
		}
	}
}

// relabel assigns the container's SELinux MCS category; tests stub the shellout.
var relabel = func(ctx context.Context, mcsLabel, destDir string) error {
	cmd := exec.CommandContext(ctx, "chcon", "-R", mcsLabel, destDir) //nolint:gosec // G204: the SELinux relabel seam — mcsLabel is a Runner-read podman MountLabel and destDir is a Runner-built path, neither user-controlled
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("chcon %s %s: %w: %s", mcsLabel, destDir, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// validateAndUnpack enforces archive limits and path/layout rules before writing.
// destDir must already exist.
func validateAndUnpack(tarball []byte, destDir string) error {
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return fmt.Errorf("opening gzip reader: %w", err)
	}
	defer func() { _ = gz.Close() }() // read-only reader: close error is not actionable

	// Bound decompression as it happens so a gzip bomb cannot exhaust memory.
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
		// settings/config.yml and top-level models.yml must parse as a YAML
		// mapping — the structural twin of the store door (T1). Best-effort:
		// T4's container-side Bun-parse guard is the authoritative backstop.
		if clean == settingsMember || clean == memberModels {
			if err := requireYAMLMapping(buf); err != nil {
				return fmt.Errorf("config bundle member %q: %w", hdr.Name, err)
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

// validateMemberPath rejects traversal and members outside the permitted layout.
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

	if len(parts) == 1 {
		return validateTopLevelMember(name, clean, top, typeflag)
	}
	if _, ok := configTopDirs[top]; !ok {
		return "", fmt.Errorf("config bundle member %q is not under skills/, extensions/, mcp/, settings/, rules/, agents/, prompts/, or profiles/", name)
	}
	return validateNestedMember(name, clean, top, parts, typeflag)
}

// validateTopLevelMember validates a single-component member: a regular file
// named exactly AGENTS.md or models.yml, or a bare top-dir container directory.
func validateTopLevelMember(name, clean, top string, typeflag byte) (string, error) {
	if typeflag == tar.TypeReg {
		if clean == memberAgentsMD || clean == memberModels {
			return clean, nil
		}
		return "", fmt.Errorf("config bundle top-level file %q must be %s or %s", name, memberAgentsMD, memberModels)
	}
	if _, ok := configTopDirs[top]; !ok {
		return "", fmt.Errorf("config bundle member %q is not under a permitted top dir and is not a top-level %s or %s", name, memberAgentsMD, memberModels)
	}
	if typeflag != tar.TypeDir {
		return "", fmt.Errorf("config bundle top-level %q must be a directory", name)
	}
	return clean, nil
}

// validateNestedMember validates a member with two or more path components under
// a whitelisted top dir. Directory members (and skills/extensions files) require
// only a safe first-level name; regular files under settings/rules/agents/mcp
// carry per-dir grammar.
func validateNestedMember(name, clean, top string, parts []string, typeflag byte) (string, error) {
	if typeflag == tar.TypeReg {
		return validateNestedRegularMember(name, clean, top, parts)
	}
	// Directory entries under any top dir: only the first-level name must be
	// safe; deeper segments rely on the traversal + containment guards.
	if !configTopLevelName.MatchString(parts[1]) {
		return "", fmt.Errorf("config bundle member name %q is not a safe name", parts[1])
	}
	return clean, nil
}

// validateNestedRegularMember validates a regular-file member (two or more path
// components) under a whitelisted top dir. settings/rules/agents/mcp/prompts/
// profiles carry per-dir grammar; a regular file under skills/ or extensions/
// requires only a safe first-level name.
func validateNestedRegularMember(name, clean, top string, parts []string) (string, error) {
	switch top {
	case topDirSettings:
		// Exactly settings/config.yml — yml-only (OQ-1).
		if clean != settingsMember {
			return "", fmt.Errorf("config bundle settings member %q must be exactly %s", name, settingsMember)
		}
		return clean, nil
	case topDirRules:
		if err := validateFlatRunnerMember(topDirRules, name, parts, ".md", ".mdc"); err != nil {
			return "", err
		}
		return clean, nil
	case topDirAgents:
		if err := validateFlatRunnerMember(topDirAgents, name, parts, ".md"); err != nil {
			return "", err
		}
		return clean, nil
	case topDirMCP:
		// mcp/<name>.json — safe base name, required .json suffix.
		entry := parts[1]
		base, ok := strings.CutSuffix(entry, ".json")
		if !ok {
			return "", fmt.Errorf("config bundle mcp member %q must have a .json suffix", name)
		}
		if !configTopLevelName.MatchString(base) {
			return "", fmt.Errorf("config bundle mcp member name %q is not a safe name", entry)
		}
		return clean, nil
	case topDirPrompts:
		// Exactly prompts/<role>/SYSTEM.md — three components, safe <role>,
		// filename exactly SYSTEM.md (RIG-3075 T2).
		if len(parts) != 3 || parts[2] != memberSystemMD {
			return "", fmt.Errorf("config bundle prompts member %q must be prompts/<role>/%s", name, memberSystemMD)
		}
		if !configTopLevelName.MatchString(parts[1]) {
			return "", fmt.Errorf("config bundle prompts role name %q is not a safe name", parts[1])
		}
		return clean, nil
	case topDirProfiles:
		// Exactly profiles/<name>/profile.yml — three components, safe <name>,
		// filename exactly profile.yml. The store door carries the profile SCHEMA
		// validation; the runner is the structural twin — layout only, no content
		// schema (mirrors the settings/models asymmetry).
		if len(parts) != 3 || parts[2] != memberProfileYML {
			return "", fmt.Errorf("config bundle profiles member %q must be profiles/<name>/%s", name, memberProfileYML)
		}
		if !configTopLevelName.MatchString(parts[1]) {
			return "", fmt.Errorf("config bundle profiles name %q is not a safe name", parts[1])
		}
		return clean, nil
	}
	// skills/ or extensions/: first-level name must be safe; deeper segments
	// rely on the traversal + containment guards.
	if !configTopLevelName.MatchString(parts[1]) {
		return "", fmt.Errorf("config bundle member name %q is not a safe name", parts[1])
	}
	return clean, nil
}

// validateFlatRunnerMember enforces a flat top/<name><ext> member: exactly two
// path components, and <name> (with an allowed <ext> stripped) is a safe name.
func validateFlatRunnerMember(top, name string, parts []string, exts ...string) error {
	if len(exts) == 0 {
		return fmt.Errorf("config bundle %s member %q: no allowed extensions configured", top, name)
	}
	if len(parts) != 2 {
		return fmt.Errorf("config bundle %s member %q must be flat %s/<name>%s", top, name, top, exts[0])
	}
	entry := parts[1]
	for _, ext := range exts {
		if base, ok := strings.CutSuffix(entry, ext); ok {
			if !configTopLevelName.MatchString(base) {
				return fmt.Errorf("config bundle %s member name %q is not a safe name", top, base)
			}
			return nil
		}
	}
	return fmt.Errorf("config bundle %s member %q must end in one of %s", top, name, strings.Join(exts, ", "))
}

// requireYAMLMapping parses content as YAML and requires it to be a mapping
// (rejecting a scalar or sequence). Structural twin of the store door's
// parseYAMLMapping; an empty document (YAML null) counts as an empty mapping.
func requireYAMLMapping(content []byte) error {
	var doc any
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return fmt.Errorf("not valid YAML: %w", err)
	}
	if doc == nil {
		return nil
	}
	if _, ok := doc.(map[string]any); !ok {
		return errors.New("must be a YAML mapping")
	}
	return nil
}
