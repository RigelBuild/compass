package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/RigelBuild/compass/go/internal/store/db"
	yaml "go.yaml.in/yaml/v3"
)

// The fleet CONFIG-BUNDLE store (RIG-1624 T1). One fleet-wide singleton bundle
// row (agent_config_bundle, 0001_init.sql) holds the gzip-tarball of the
// skills/, extensions/, and mcp/ material every agent materializes into its
// scoped config dir (T3/T4). Unlike the secrets NAMES registry (secrets.go, a
// set of named rows), config is CURRENT-ONLY: PutAgentConfig replaces the one
// row in place, and version is the canonical CONTENT hash so a re-put of
// identical content is version-stable. The bundle is credential-free by MVP
// rule (CD-3) — secrets ride the separate resolve path, never this bundle.

// Config-bundle top-dir names — the whitelisted top-level directories a bundle
// member may live under. Each becomes a host directory when the bundle is
// materialized into an agent's config dir (T4), so the set is closed at the
// store door, not filtered downstream.
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

// Top-level regular-file members admitted by exact filename (not under a top
// dir): the fleet context file and the fleet model config (RIG-1678 T1). Any
// other top-level file stays rejected.
const (
	memberAgentsMD = "AGENTS.md"
	memberModels   = "models.yml"
	// settingsMember is the ONLY file admitted under settings/ — yml-only per
	// OQ-1 (no .yaml/.json variant, no other name).
	settingsMember = "settings/config.yml"
	// memberSystemMD is the ONLY filename admitted under prompts/<role>/ — the
	// role prompt is exactly prompts/<role>/SYSTEM.md (RIG-3075 T2).
	memberSystemMD = "SYSTEM.md"
	// memberProfileYML is the ONLY filename admitted under profiles/<name>/ —
	// the profile is exactly profiles/<name>/profile.yml (RIG-2968 T1).
	memberProfileYML = "profile.yml"
)

// configBundleTopDirs is the whitelist as a set, for the O(1) membership check in
// configMemberParts.
var configBundleTopDirs = map[string]bool{
	topDirSkills:     true,
	topDirExtensions: true,
	topDirMCP:        true,
	topDirSettings:   true,
	topDirRules:      true,
	topDirAgents:     true,
	topDirPrompts:    true,
	topDirProfiles:   true,
}

// configNamePattern is the grammar for a config entry's <name> segment —
// skills/<name>, extensions/<name>, mcp/<name>.json. A declared name is
// validated at the store door (PutAgentConfig) before it can reach a row
// because it later becomes a host path segment under the agent's config dir
// (T4): constrained at the door, not escaped downstream (mirrors secrets.go's
// secretNamePattern posture).
var configNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// frontmatterNamePattern matches a `name:` line in an agents/*.md frontmatter
// block for the tolerant line-scan fallback in agentDefFrontmatterName, used
// when a strict whole-block YAML parse fails because a SIBLING field is a
// YAML-ambiguous scalar. Anchored; the first match wins.
var frontmatterNamePattern = regexp.MustCompile(`^name:\s*(.*)$`)

const (
	// maxDecompressedBytes caps the total DECOMPRESSED size of a config bundle,
	// enforced DURING gunzip (cappedReader) so a gzip bomb — a few KiB that
	// inflates to gigabytes — is aborted mid-stream rather than after a full
	// decompress. 64 MiB is generous headroom over the skills/extensions/mcp
	// material a fleet realistically ships while still bounding a single Put's
	// memory and the BYTEA row.
	maxDecompressedBytes int64 = 64 << 20 // 64 MiB
	// maxFileCount caps the number of regular-file members, also enforced during
	// the streamed read, so a bundle of millions of tiny files (each under the
	// byte cap) cannot exhaust memory/handles. 4096 files comfortably covers a
	// realistic skill/extension/mcp set.
	maxFileCount = 4096
)

// errBundleTooLarge is returned through the tar/gzip read stack the moment the
// decompressed byte cap is exceeded. It wraps ErrInvalidArgument so the door's
// caller sees a field error, and is a distinct sentinel so the read loop can
// recognize it via errors.Is after tar propagates it.
var errBundleTooLarge = fmt.Errorf("%w: bundle exceeds decompressed size cap of %d bytes", ErrInvalidArgument, maxDecompressedBytes)

// cappedReader bounds the total bytes read from an underlying reader, returning
// errBundleTooLarge once the cap is crossed. Wrapping the gunzip stream (the
// DECOMPRESSED side) with it is the gzip-bomb defense: the tar reader can never
// pull more than maxDecompressedBytes of inflated content regardless of how
// small the compressed input is.
type cappedReader struct {
	r io.Reader
	n int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	nn, err := c.r.Read(p)
	c.n += int64(nn)
	if c.n > maxDecompressedBytes {
		return nn, errBundleTooLarge
	}
	return nn, err
}

// PutAgentConfig validates a fleet config bundle at the store door and, if it
// passes, upserts it as the single current bundle (RIG-1624 T1). It returns the
// bundle's canonical content version (the content hash computed by
// validateAndHashConfigBundle): the sha256 over the DECOMPRESSED,
// metadata-zeroed (path, bytes) content, so tar member
// ordering, mtimes/uid/gid, and gzip framing never perturb it — a re-put of
// byte-identical CONTENT yields the same version and replaces the row in place
// (current-only retention via the singleton PK upsert).
//
// Validation runs BEFORE any row write: a bundle that is not a gzip tarball,
// whose members escape the whitelisted skills/|extensions/|mcp/ top dirs, use
// absolute or ".." paths, carry a symlink or hardlink member, violate the
// <name> grammar, exceed the decompressed-size or file-count cap, or contain an
// mcp/*.json that is not valid JSON, is rejected as a %w-wrapped
// ErrInvalidArgument and NO row is written. actor is the operator-scoped writer
// (empty → ErrInvalidArgument); the singleton has no per-actor column, but the
// param documents the operator-scoped write and matches the record.
func (s *Store) PutAgentConfig(ctx context.Context, actor AccountID, bundle []byte) (version string, err error) {
	if actor == "" {
		return "", fmt.Errorf("%w: config-bundle writer account id is required", ErrInvalidArgument)
	}
	version, err = validateAndHashConfigBundle(bundle)
	if err != nil {
		return "", err
	}
	if err := s.q.PutAgentConfig(ctx, db.PutAgentConfigParams{
		Version: version,
		Bundle:  bundle,
	}); err != nil {
		return "", fmt.Errorf("store: put agent config: %w", err)
	}
	return version, nil
}

// ValidateConfigBundle validates a config bundle against the store door's
// grammar and returns its canonical content version, without touching the
// database. It is the pure door check PutAgentConfig runs before the row
// write, exported so a bundle producer (the operator CLI builder) can prove
// its output against the real door in tests, closing the builder/door drift
// gap a parallel hand-rolled grammar leaves open.
func ValidateConfigBundle(bundle []byte) (version string, err error) {
	return validateAndHashConfigBundle(bundle)
}

// CurrentAgentConfig returns the single current config bundle and its canonical
// content version. ErrNotFound when no bundle has been declared — a valid state
// downstream (the fetch path then materializes an empty config dir), but the
// store still reports the absence; the caller decides empty-is-ok.
func (s *Store) CurrentAgentConfig(ctx context.Context) (version string, bundle []byte, err error) {
	row, err := s.q.CurrentAgentConfig(ctx)
	if err != nil {
		if noRows(err) {
			return "", nil, fmt.Errorf("%w: no agent config bundle declared", ErrNotFound)
		}
		return "", nil, fmt.Errorf("store: read agent config: %w", err)
	}
	return row.Version, row.Bundle, nil
}

// DeleteAgentConfig clears the fleet config bundle, returning the store to the
// unconfigured state (CurrentAgentConfig then reports ErrNotFound — a valid
// downstream state, the empty-config door). Idempotent: deleting when the
// singleton is already absent is a no-op success, not ErrNotFound — the caller's
// intent (no bundle) already holds, so a repeated Delete or a Delete on a
// never-configured fleet both succeed. This is the operator's explicit
// return-to-unconfigured path (RIG-1625 T2), chosen over blessing an
// empty-tarball push.
func (s *Store) DeleteAgentConfig(ctx context.Context) error {
	if err := s.q.DeleteAgentConfig(ctx); err != nil {
		return fmt.Errorf("store: delete agent config: %w", err)
	}
	return nil
}

// AgentConfigInfo reports the current bundle's version and the NAMES of its
// declared members, bucketed by top dir (skills / extensions / mcp) — names
// only, never content (RIG-1625 T2). Each bucket is deduplicated and sorted: a
// skill spreads many files under skills/<name>/, but the operator-facing view is
// the set of declared <name>s. ErrNotFound when no bundle is declared (the
// caller decides empty-is-ok, mirroring CurrentAgentConfig).
//
// It re-walks the stored bundle with the SAME grammar the store door enforced at
// Put (configMemberParts + the decompressed cap via cappedReader), so it reuses
// the door's path validation rather than re-inventing a tar walk. The bundle was
// already validated at Put, so this walk is over trusted content; re-applying the
// cap is the cheap defense-in-depth posture every unpack re-enforces.
func (s *Store) AgentConfigInfo(ctx context.Context) (info AgentConfigInfoResult, err error) {
	version, bundle, err := s.CurrentAgentConfig(ctx)
	if err != nil {
		return AgentConfigInfoResult{}, err
	}
	info, err = configBundleMemberNames(bundle)
	if err != nil {
		return AgentConfigInfoResult{}, err
	}
	info.Version = version
	return info, nil
}

// AgentConfigInfoResult is the value-free member inventory of a config bundle:
// version, the multi-member name buckets (skills/extensions/mcp + the CP-4
// rules/subagents), and presence flags for the singleton members (settings,
// AGENTS.md, models.yml). Names only, never content (RIG-1625 T2 / RIG-1678 T1).
type AgentConfigInfoResult struct {
	Version     string
	Skills      []string
	Extensions  []string
	McpServers  []string
	Rules       []string
	Subagents   []string
	Prompts     []string
	Profiles    []string
	HasSettings bool
	HasAgentsMD bool
	HasModels   bool
}

// configBundleMemberNames walks a stored config bundle and returns the declared
// member names bucketed by top dir, each deduplicated and sorted. It never reads
// member CONTENT — only the tar HEADERS — so it decompresses (bounded by the
// same cappedReader gzip-bomb guard as the store door) without materializing any
// file body. skills/<name>/... and extensions/<name>/... contribute <name> (the
// second path component); mcp/<name>.json contributes <name> (the base without
// the .json suffix). Directory members and any name-less top-dir-only entry are
// skipped — they declare no member.
func configBundleMemberNames(bundle []byte) (AgentConfigInfoResult, error) {
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return AgentConfigInfoResult{}, fmt.Errorf("%w: bundle is not a valid gzip stream: %w", ErrInvalidArgument, err)
	}
	// Read-only gunzip: Close only releases the decompressor, so its error is
	// not actionable here (nothing was written to flush).
	defer func() { _ = gz.Close() }()

	skillSet := make(map[string]bool)
	extSet := make(map[string]bool)
	mcpSet := make(map[string]bool)
	ruleSet := make(map[string]bool)
	agentSet := make(map[string]bool)
	promptSet := make(map[string]bool)
	profileSet := make(map[string]bool)
	var info AgentConfigInfoResult

	tr := tar.NewReader(&cappedReader{r: gz})
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, errBundleTooLarge) {
				return AgentConfigInfoResult{}, errBundleTooLarge
			}
			return AgentConfigInfoResult{}, fmt.Errorf("%w: bundle is not a valid tar stream: %w", ErrInvalidArgument, err)
		}
		parts, err := configMemberParts(hdr.Name)
		if err != nil {
			return AgentConfigInfoResult{}, err
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		// Top-level singleton files declare a presence flag, not a name.
		if len(parts) == 1 {
			switch parts[0] {
			case memberAgentsMD:
				info.HasAgentsMD = true
			case memberModels:
				info.HasModels = true
			}
			continue
		}
		switch parts[0] {
		case topDirSkills:
			skillSet[parts[1]] = true
		case topDirExtensions:
			extSet[parts[1]] = true
		case topDirMCP:
			mcpSet[strings.TrimSuffix(parts[1], ".json")] = true
		case topDirSettings:
			// The only settings/ member is settings/config.yml (door-enforced).
			info.HasSettings = true
		case topDirRules:
			ruleSet[trimAnySuffix(parts[1], ".md", ".mdc")] = true
		case topDirAgents:
			agentSet[strings.TrimSuffix(parts[1], ".md")] = true
		case topDirPrompts:
			// A role prompt is exactly prompts/<role>/SYSTEM.md; count the
			// <role> name, matching the door grammar (validateRegularMember).
			if len(parts) == 3 && parts[2] == memberSystemMD {
				promptSet[parts[1]] = true
			}
		case topDirProfiles:
			// A profile is exactly profiles/<name>/profile.yml; count the
			// <name>, matching the door grammar (validateProfileMember).
			if len(parts) == 3 && parts[2] == memberProfileYML {
				profileSet[parts[1]] = true
			}
		}
	}
	info.Skills = sortedKeys(skillSet)
	info.Extensions = sortedKeys(extSet)
	info.McpServers = sortedKeys(mcpSet)
	info.Rules = sortedKeys(ruleSet)
	info.Subagents = sortedKeys(agentSet)
	info.Prompts = sortedKeys(promptSet)
	info.Profiles = sortedKeys(profileSet)
	return info, nil
}

// trimAnySuffix trims the first matching suffix from s, else returns s.
func trimAnySuffix(s string, suffixes ...string) string {
	for _, suf := range suffixes {
		if base, ok := strings.CutSuffix(s, suf); ok {
			return base
		}
	}
	return s
}

// sortedKeys returns a set's keys as a sorted slice — the stable, deduplicated
// name list AgentConfigInfo reports per bucket.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validateAndHashConfigBundle is the security-critical store door: a SINGLE
// streamed pass over the gzip tarball that both validates every member and
// accumulates the canonical content hash, returning a %w-wrapped
// ErrInvalidArgument on the first violation (so nothing is decompressed twice).
//
// Canonical version serialization (metadata-zeroed, order-independent): collect
// (name, content) for every regular file, sort by name, then hash a
// LENGTH-PREFIXED framing so two distinct member sets cannot collide —
//
//	uint64(len(members))
//	for each member in name order:
//	    uint64(len(name)) name
//	    uint64(len(content)) content
//
// all big-endian. Only the member NAME and file CONTENT feed the hash; tar
// ordering, mtimes, uid/gid, and gzip mtime/level are excluded, so identical
// content re-packed any which way hashes identically.
func validateAndHashConfigBundle(bundle []byte) (string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return "", fmt.Errorf("%w: bundle is not a valid gzip stream: %w", ErrInvalidArgument, err)
	}
	// Read-only gunzip: Close only releases the decompressor, so its error is
	// not actionable here (nothing was written to flush).
	defer func() { _ = gz.Close() }()

	type member struct {
		name    string
		content []byte
	}
	var members []member
	fileCount := 0
	// seen tracks regular-member names to reject duplicates at the door (M1);
	// see the dedup comment in the loop for why version-stability requires it.
	seen := make(map[string]bool)

	tr := tar.NewReader(&cappedReader{r: gz})
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, errBundleTooLarge) {
				return "", errBundleTooLarge
			}
			return "", fmt.Errorf("%w: bundle is not a valid tar stream: %w", ErrInvalidArgument, err)
		}

		// Reject the escape-vector typeflags outright. A symlink and a hardlink
		// are DISTINCT escapes (TypeSymlink vs TypeLink); both can point outside
		// the materialized config dir, so both are refused before any path
		// analysis.
		switch hdr.Typeflag {
		case tar.TypeSymlink:
			return "", fmt.Errorf("%w: bundle member %q is a symlink (not allowed)", ErrInvalidArgument, hdr.Name)
		case tar.TypeLink:
			return "", fmt.Errorf("%w: bundle member %q is a hardlink (not allowed)", ErrInvalidArgument, hdr.Name)
		}

		parts, err := configMemberParts(hdr.Name)
		if err != nil {
			return "", err
		}

		// The member typeflag must resolve to a regular file or a directory —
		// an explicit ALLOWLIST, not a catch-all skip. A directory contributes
		// no content and no host file (its path already passed the escape +
		// whitelist checks above), so it is skipped. A contiguous-file member
		// (tar.TypeCont) is reported as a regular file by archive/tar and is
		// treated as one: its content is hashed and it materializes to a regular
		// host file, so the version covers it. Every remaining typeflag
		// (char/block device, FIFO, socket, and any future non-regular flag) is
		// REJECTED: such a member would ride into the verbatim-persisted bundle
		// bytes yet contribute nothing to the version hash, so two bundles with
		// identical regular files but a differing device member would share a
		// version while differing on disk (M2).
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if !hdr.FileInfo().Mode().IsRegular() {
			return "", fmt.Errorf("%w: bundle member %q has unsupported typeflag %d (only regular files and directories are allowed)", ErrInvalidArgument, hdr.Name, hdr.Typeflag)
		}

		// Reject duplicate regular-member names at the door (M1). Tar permits
		// duplicate entries, so two regular members at the same path with
		// different content would leave two equal-keyed members whose relative
		// order an unstable sort cannot normalize — the same logical bundle
		// re-packed in a different tar order could then hash to a different
		// version. Rejecting duplicates closes that ambiguity and keeps every
		// sort key unique. Only regular files are tracked: directories feed no
		// content/hash and persist no host file, so a duplicate dir is harmless.
		if seen[hdr.Name] {
			return "", fmt.Errorf("%w: bundle contains duplicate member %q", ErrInvalidArgument, hdr.Name)
		}
		seen[hdr.Name] = true

		// Count-before-read (L1): enforce the file-count cap before reading the
		// member body, so the (maxFileCount+1)th body is never read into memory.
		fileCount++
		if fileCount > maxFileCount {
			return "", fmt.Errorf("%w: bundle exceeds file-count cap of %d files", ErrInvalidArgument, maxFileCount)
		}

		content, err := validateRegularMember(parts, tr)
		if err != nil {
			if errors.Is(err, errBundleTooLarge) {
				return "", errBundleTooLarge
			}
			return "", err
		}
		members = append(members, member{name: hdr.Name, content: content})
	}

	// Cross-member profile lint (RIG-2968 T1). The per-member pass above
	// validated each profiles/<name>/profile.yml in isolation (YAML mapping +
	// superset-key closure + models.* selector shape); the models.agents key
	// lint is CROSS-MEMBER — each key must match the frontmatter name: of an
	// agents/*.md def in the SAME bundle — so it runs here over the fully
	// collected member set, after the single streamed pass. It reads only the
	// already-collected member bytes (no re-decompress) and never feeds the
	// hash, so the canonical version stays order-independent and metadata-zeroed.
	// Separate the two member classes the lint needs (agent-def frontmatter
	// names, and the profile bodies) into plain maps so the check is a pure
	// function over collected bytes. The member NAME segment already passed the
	// grammar in validateRegularMember, so parts[1] is safe to index.
	agentDefNames := make(map[string]bool)
	profileBodies := make(map[string][]byte)
	for _, m := range members {
		parts := strings.Split(m.name, "/")
		switch {
		case len(parts) == 2 && parts[0] == topDirAgents:
			if name := agentDefFrontmatterName(m.content); name != "" {
				agentDefNames[name] = true
			}
		case len(parts) == 3 && parts[0] == topDirProfiles && parts[2] == memberProfileYML:
			profileBodies[m.name] = m.content
		}
	}
	if err := lintProfileAgentKeys(profileBodies, agentDefNames); err != nil {
		return "", err
	}

	// Sort by name. Duplicate regular names are rejected above, so keys are
	// unique and this ordering is total — sort stability is moot.
	sort.Slice(members, func(i, j int) bool { return members[i].name < members[j].name })

	h := sha256.New()
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(members)))
	h.Write(lenBuf[:])
	for _, m := range members {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(m.name)))
		h.Write(lenBuf[:])
		h.Write([]byte(m.name))
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(m.content)))
		h.Write(lenBuf[:])
		h.Write(m.content)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// configMemberParts validates a member path for escapes and the top-dir
// whitelist, returning its cleaned path components. It rejects an absolute
// path, the empty/root path, and any "", ".", or ".." component (the ".."
// traversal escape), then requires the top-level part to be either a
// whitelisted top dir (skills/|extensions/|mcp/|settings/|rules/|agents/) or,
// for a single-component path, one of the two admitted top-level filenames
// (AGENTS.md, models.yml).
func configMemberParts(name string) ([]string, error) {
	if strings.HasPrefix(name, "/") {
		return nil, fmt.Errorf("%w: bundle member %q is an absolute path", ErrInvalidArgument, name)
	}
	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" {
		return nil, fmt.Errorf("%w: bundle member has an empty path", ErrInvalidArgument)
	}
	parts := strings.Split(trimmed, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return nil, fmt.Errorf("%w: bundle member %q has an illegal path component %q", ErrInvalidArgument, name, p)
		}
	}
	// A single-component path is a top-level member: admit only the two exact
	// filenames (or a directory named one of the top dirs — handled below).
	if len(parts) == 1 && (parts[0] == memberAgentsMD || parts[0] == memberModels) {
		return parts, nil
	}
	if !configBundleTopDirs[parts[0]] {
		return nil, fmt.Errorf("%w: bundle member %q is not under skills/, extensions/, mcp/, settings/, rules/, agents/, prompts/, or profiles/ and is not a top-level %s or %s", ErrInvalidArgument, name, memberAgentsMD, memberModels)
	}
	return parts, nil
}

// validateRegularMember enforces the per-file grammar and reads the member's
// content (bounded by the enclosing cappedReader), returning it for the hash.
//
//   - mcp/<name>.json — grammar-valid <name>, content parses as JSON.
//   - skills/<name>/… , extensions/<name>/… — grammar-valid <name>.
//   - settings/config.yml — the ONLY settings/ member (yml-only, OQ-1); content
//     parses as a YAML mapping and sets no credential-denylisted key.
//   - rules/<name>.md|.mdc — flat, grammar-valid <name>; prose, no content check.
//   - agents/<name>.md — flat, grammar-valid <name>; prose, no content check.
//   - prompts/<role>/SYSTEM.md — grammar-valid <role>, filename exactly
//     SYSTEM.md; prose, no content check (RIG-3075 T2).
//   - top-level AGENTS.md — prose, no content check beyond the read.
//   - top-level models.yml — content parses as a YAML mapping and sets no
//     credentialed provider surface (apiKey, or a headers.* literal secret).
//
// The read propagates errBundleTooLarge if the decompressed cap is crossed
// mid-file.
func validateRegularMember(parts []string, r io.Reader) ([]byte, error) {
	joined := strings.Join(parts, "/")
	switch parts[0] {
	case topDirMCP:
		return validateMCPMember(parts, joined, r)
	case topDirSettings:
		return validateSettingsMember(joined, r)
	case topDirRules:
		if len(parts) != 2 {
			return nil, fmt.Errorf("%w: rules member %q must be flat rules/<name>.md or rules/<name>.mdc", ErrInvalidArgument, joined)
		}
		if err := validateFlatNamedMember("rules", parts[1], joined, ".md", ".mdc"); err != nil {
			return nil, err
		}
		return io.ReadAll(r)
	case topDirAgents:
		if len(parts) != 2 {
			return nil, fmt.Errorf("%w: agents member %q must be flat agents/<name>.md", ErrInvalidArgument, joined)
		}
		if err := validateFlatNamedMember("agents", parts[1], joined, ".md"); err != nil {
			return nil, err
		}
		return io.ReadAll(r)
	case topDirPrompts:
		// Exactly prompts/<role>/SYSTEM.md: three components, grammar-valid
		// <role>, filename exactly SYSTEM.md. Stricter than skills/extensions
		// (which admit arbitrary depth under <name>), so it needs its own case
		// rather than the fall-through below.
		if len(parts) != 3 || parts[2] != memberSystemMD {
			return nil, fmt.Errorf("%w: prompts member %q must be prompts/<role>/%s", ErrInvalidArgument, joined, memberSystemMD)
		}
		if !configNamePattern.MatchString(parts[1]) {
			return nil, fmt.Errorf("%w: prompts role name %q must match %s", ErrInvalidArgument, parts[1], configNamePattern.String())
		}
		return io.ReadAll(r)
	case topDirProfiles:
		return validateProfileMember(parts, joined, r)
	}

	// Top-level single-component files (configMemberParts admits only the two
	// exact names).
	if len(parts) == 1 {
		switch parts[0] {
		case memberAgentsMD:
			return io.ReadAll(r) // prose, no content validation beyond the read
		case memberModels:
			return validateModelsMember(joined, r)
		}
	}

	// skills/ or extensions/: the <name> segment must match the grammar; deeper
	// components have already passed the escape check in configMemberParts.
	if len(parts) < 2 {
		return nil, fmt.Errorf("%w: bundle member %q must live under %s/<name>", ErrInvalidArgument, joined, parts[0])
	}
	if !configNamePattern.MatchString(parts[1]) {
		return nil, fmt.Errorf("%w: bundle member name %q must match %s", ErrInvalidArgument, parts[1], configNamePattern.String())
	}
	return io.ReadAll(r)
}

// validateMCPMember validates mcp/<name>.json (grammar-valid <name>, JSON body).
func validateMCPMember(parts []string, joined string, r io.Reader) ([]byte, error) {
	if len(parts) != 2 {
		return nil, fmt.Errorf("%w: mcp member %q must be mcp/<name>.json", ErrInvalidArgument, joined)
	}
	base, ok := strings.CutSuffix(parts[1], ".json")
	if !ok {
		return nil, fmt.Errorf("%w: mcp member %q must be a .json file", ErrInvalidArgument, joined)
	}
	if !configNamePattern.MatchString(base) {
		return nil, fmt.Errorf("%w: mcp member name %q must match %s", ErrInvalidArgument, base, configNamePattern.String())
	}
	content, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if !json.Valid(content) {
		return nil, fmt.Errorf("%w: mcp member %q is not valid JSON", ErrInvalidArgument, joined)
	}
	return content, nil
}

// validateSettingsMember validates the settings/config.yml member: yml-only
// (OQ-1), a YAML mapping, with no credential-denylisted key set.
func validateSettingsMember(joined string, r io.Reader) ([]byte, error) {
	if joined != settingsMember {
		return nil, fmt.Errorf("%w: settings member %q must be exactly %s", ErrInvalidArgument, joined, settingsMember)
	}
	content, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	mapping, err := parseYAMLMapping(content, joined)
	if err != nil {
		return nil, err
	}
	if err := rejectCredentialSettings(mapping, joined); err != nil {
		return nil, err
	}
	return content, nil
}

// validateModelsMember validates the top-level models.yml member: a YAML mapping
// with no credentialed provider surface (apiKey, or a headers.* literal secret).
func validateModelsMember(joined string, r io.Reader) ([]byte, error) {
	content, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	mapping, err := parseYAMLMapping(content, joined)
	if err != nil {
		return nil, err
	}
	if err := rejectCredentialModels(mapping, joined); err != nil {
		return nil, err
	}
	return content, nil
}

// profileSupersetKeys is the closed set of top-level keys a profile.yml may
// declare (RIG-2968 T1, §Approach superset schema). v1 CONSUMES only `models`;
// `corpus`/`extensions`/`settings` are schema'd, consumption deferred — but all
// four are ACCEPTED at the door so later phases grow additively with no schema
// break. "Unknown key" = a top-level key OUTSIDE this set, never a deferred axis.
var profileSupersetKeys = map[string]bool{
	"models":     true,
	"corpus":     true,
	"extensions": true,
	"settings":   true,
}

// validateProfileMember validates a profiles/<name>/profile.yml member in
// isolation: exactly three components with filename profile.yml and a
// grammar-valid <name>, a YAML-mapping body, top-level keys within the profile
// superset, and (where present) string-shaped models.* selectors. The
// CROSS-MEMBER models.agents key lint (each key must match a shipped agent def's
// frontmatter name) is not enforceable per-member and runs in
// validateAndHashConfigBundle over the collected member set.
func validateProfileMember(parts []string, joined string, r io.Reader) ([]byte, error) {
	if len(parts) != 3 || parts[2] != memberProfileYML {
		return nil, fmt.Errorf("%w: profiles member %q must be profiles/<name>/%s", ErrInvalidArgument, joined, memberProfileYML)
	}
	if !configNamePattern.MatchString(parts[1]) {
		return nil, fmt.Errorf("%w: profiles name %q must match %s", ErrInvalidArgument, parts[1], configNamePattern.String())
	}
	content, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	mapping, err := parseYAMLMapping(content, joined)
	if err != nil {
		return nil, err
	}
	for key := range mapping {
		if !profileSupersetKeys[key] {
			return nil, fmt.Errorf("%w: profile member %q sets unknown top-level key %q (allowed: corpus, extensions, models, settings)", ErrInvalidArgument, joined, key)
		}
	}
	if err := validateProfileModelSelectors(mapping, joined); err != nil {
		return nil, err
	}
	// The profile settings sub-mapping shares the settings/config.yml credential
	// axis, so reuse the same denylist here (F3). The extensions/corpus axes'
	// credential surfaces are deferred with their consumption task — not now.
	if settingsRaw, present := mapping["settings"]; present && settingsRaw != nil {
		if err := rejectNonStringKeys(settingsRaw, joined, "settings"); err != nil {
			return nil, err
		}
		if settingsMap, ok := settingsRaw.(map[string]any); ok {
			if err := rejectCredentialSettings(settingsMap, joined); err != nil {
				return nil, err
			}
		}
	}
	return content, nil
}

// rejectNonStringKeys rejects a YAML value that is a mapping with any non-string
// key. yaml.v3 decodes such a mapping as map[any]any (not map[string]any), so a
// v.(map[string]any) assertion on it fails OPEN, silently skipping every
// key-level check below. A non-mapping value (scalar/list/absent) is not this
// class and passes through for the caller's own shape handling.
func rejectNonStringKeys(v any, joined, path string) error {
	if _, ok := v.(map[any]any); ok {
		return fmt.Errorf("%w: profile member %q %s must be a string-keyed mapping", ErrInvalidArgument, joined, path)
	}
	return nil
}

// validateProfileModelSelectors enforces the models.* selector SHAPE: a model
// selector is an opaque string (the split-on-last-colon grammar is the SDK's,
// never re-parsed here). models.manager, where present, must be a string; every
// value under models.agents, where present, must be a string. An absent or null
// axis is fine (deferred/empty). Non-string selectors are rejected so a
// mis-shaped profile fails closed at the door rather than silently at render.
// A models (or models.agents) mapping with any non-string key is rejected up
// front: yaml.v3 decodes it as map[any]any, so a naive map[string]any assertion
// would fail OPEN and skip every selector check below.
func validateProfileModelSelectors(mapping map[string]any, joined string) error {
	modelsRaw, present := mapping["models"]
	if !present || modelsRaw == nil {
		return nil
	}
	if err := rejectNonStringKeys(modelsRaw, joined, "models"); err != nil {
		return err
	}
	models, ok := modelsRaw.(map[string]any)
	if !ok {
		// A non-mapping models value (scalar/list) is a deferred-shape concern,
		// not a v1 door failure (v1 consumes models but tolerates an empty axis).
		// The map[any]any case was already rejected above.
		return nil
	}
	if v, present := models["manager"]; present && v != nil {
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%w: profile member %q models.manager must be a string selector", ErrInvalidArgument, joined)
		}
	}
	agentsRaw, present := models["agents"]
	if !present || agentsRaw == nil {
		return nil
	}
	if err := rejectNonStringKeys(agentsRaw, joined, "models.agents"); err != nil {
		return err
	}
	agents, ok := agentsRaw.(map[string]any)
	if !ok {
		return nil
	}
	for name, v := range agents {
		if v == nil {
			continue
		}
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%w: profile member %q models.agents.%s must be a string selector", ErrInvalidArgument, joined, name)
		}
	}
	return nil
}

// lintProfileAgentKeys is the CROSS-MEMBER models.agents key lint (RIG-2968 T1):
// every key under a profile's models.agents must match the FRONTMATTER name: of
// an agents/*.md def shipped in the SAME bundle — NOT its filename stem. The SDK
// resolves a subagent by agent.name and consults the override record per spawned
// agentName, so a key matching no def name is a SILENT no-op at spawn; the lint
// turns that typo into a reviewable door failure. agentDefNames is the set of
// frontmatter names collected from the bundle's agents/ members; profileBodies
// maps each profile member path to its raw YAML. Runs after the streamed pass,
// over already-collected bytes, so it never perturbs the canonical hash.
func lintProfileAgentKeys(profileBodies map[string][]byte, agentDefNames map[string]bool) error {
	for joined, body := range profileBodies {
		mapping, err := parseYAMLMapping(body, joined)
		if err != nil {
			// Already validated during the per-member pass; a re-parse failure
			// here would be a logic error, but fail closed regardless.
			return err
		}
		// The per-member validateProfileModelSelectors pass runs and aborts the
		// bundle before this cross-member lint, and it already rejected any
		// non-string-keyed models mapping (yaml.v3's map[any]any). So no such
		// mapping reaches here: these map[string]any assertions cannot fail-open
		// on that class, and a miss below is a genuine non-mapping value.
		models, ok := mapping["models"].(map[string]any)
		if !ok {
			continue
		}
		agents, ok := models["agents"].(map[string]any)
		if !ok {
			continue
		}
		for name := range agents {
			if !agentDefNames[name] {
				return fmt.Errorf("%w: profile member %q models.agents key %q matches no shipped agents/*.md def frontmatter name (a key matching no def name is a silent no-op at spawn)", ErrInvalidArgument, joined, name)
			}
		}
	}
	return nil
}

// agentDefFrontmatterName parses an agents/*.md def's leading YAML frontmatter
// and returns its name: field, or "" if there is no frontmatter or no name. It
// recovers the name even when a SIBLING frontmatter field is a YAML-ambiguous
// scalar (e.g. `description: A thing: with a colon`) that would fail a strict
// whole-block parse, matching the SDK loader's permissiveness: it first tries a
// full YAML parse, then falls back to a tolerant `name:` line-scan (mirroring
// the SDK's parseFrontmatter line-parser cascade for the name field). The lint
// keys on this parsed name, NOT the filename stem, so a def whose frontmatter
// name diverges from its stem lints correctly.
func agentDefFrontmatterName(content []byte) string {
	fm, ok := extractFrontmatter(content)
	if !ok {
		return ""
	}
	var doc struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(fm, &doc); err == nil && doc.Name != "" {
		return doc.Name
	}
	// Whole-block parse failed or yielded no name: a sibling field may be a
	// YAML-ambiguous scalar. Fall back to a tolerant line-scan for the name
	// field alone, mirroring the SDK's parseFrontmatter line-parser fallback.
	for line := range strings.SplitSeq(string(fm), "\n") {
		m := frontmatterNamePattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		raw := strings.TrimSpace(m[1])
		var scalar string
		if err := yaml.Unmarshal([]byte(raw), &scalar); err == nil && scalar != "" {
			return scalar
		}
		return raw
	}
	return ""
}

// extractFrontmatter returns the YAML frontmatter block bytes between a leading
// `---` line and the next `---` line, and whether such a block was found. It
// mirrors the standard Markdown front-matter shape the SDK's def loader consumes:
// the opening fence must be the file's first line.
func extractFrontmatter(content []byte) ([]byte, bool) {
	s := string(content)
	s = strings.TrimPrefix(s, "\ufeff")
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return nil, false
	}
	rest := s[strings.IndexByte(s, '\n')+1:]
	for _, fence := range []string{"\n---\n", "\n---\r\n", "\n---"} {
		if idx := strings.Index(rest, fence); idx >= 0 {
			return []byte(rest[:idx+1]), true
		}
	}
	return nil, false
}

// validateFlatNamedMember enforces a flat dir/<name><ext> member: <name> matches
// the config name grammar and <ext> is one of the allowed extensions.
func validateFlatNamedMember(topDir, filename, joined string, exts ...string) error {
	for _, ext := range exts {
		if base, ok := strings.CutSuffix(filename, ext); ok {
			if !configNamePattern.MatchString(base) {
				return fmt.Errorf("%w: %s member name %q must match %s", ErrInvalidArgument, topDir, base, configNamePattern.String())
			}
			return nil
		}
	}
	return fmt.Errorf("%w: %s member %q must end in one of %s", ErrInvalidArgument, topDir, joined, strings.Join(exts, ", "))
}

// parseYAMLMapping parses content as YAML and requires it to be a mapping
// (rejecting a scalar or sequence), returning the decoded map. This is the door
// twin of the mcp/*.json "must parse as JSON" rule and mirrors the SDK's strict
// overlay-loader contract (settings.ts #loadOverlayYaml: a non-object/array
// overlay is rejected). It is best-effort — Go and Bun YAML parsers can diverge;
// the container-side Bun-parse guard (T4) is the authoritative backstop. An
// empty document (YAML null) is treated as an empty mapping, matching the
// loader's `parsed === null → {}` path.
func parseYAMLMapping(content []byte, joined string) (map[string]any, error) {
	var doc any
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("%w: member %q is not valid YAML: %w", ErrInvalidArgument, joined, err)
	}
	if doc == nil {
		return map[string]any{}, nil
	}
	mapping, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: member %q must be a YAML mapping", ErrInvalidArgument, joined)
	}
	return mapping, nil
}

// credentialKeys (credential_keys_gen.go) is generated from the SDK's
// isCredential markers; refresh it at a fork bump with `go generate ./...`.
//
//go:generate go run gen_credential_keys.go

// rejectCredentialSettings walks the parsed settings/config.yml mapping and
// rejects any set path that is credential-marked in the SDK (credentialKeys,
// generated from isCredential). A settings path is dotted (auth.broker.token);
// the YAML nests it (auth: { broker: { token: … } }), so a key is "set" when the
// full nested path resolves to a present leaf. The door is authoritative: it
// survives a raw PutAgentConfig (GC-5, OQ-2 (c)).
func rejectCredentialSettings(mapping map[string]any, joined string) error {
	for _, key := range credentialKeys {
		if yamlPathIsSet(mapping, strings.Split(key, ".")) {
			return fmt.Errorf("%w: settings member %q sets credential-marked key %q (credentials never ride the config bundle)", ErrInvalidArgument, joined, key)
		}
	}
	return nil
}

// yamlPathIsSet reports whether the nested path resolves to a present (non-nil)
// value in the mapping. An intermediate segment that is not a mapping means the
// path is not set.
func yamlPathIsSet(mapping map[string]any, segments []string) bool {
	var current any = mapping
	for _, seg := range segments {
		m, ok := current.(map[string]any)
		if !ok {
			return false
		}
		next, present := m[seg]
		if !present || next == nil {
			return false
		}
		current = next
	}
	return true
}

// rejectCredentialModels rejects a models.yml member that sets either of the two
// credential-bearing provider surfaces (CP-4):
//
//   - providers.<name>.apiKey — a literal or any configured key.
//   - providers.<name>.headers.<h> set to a NON-env-indirection literal (a
//     pinned secret). An env-referenced header value passes: the SDK resolves a
//     header value env-name-first, literal fallback (resolveConfigValue), so a
//     value that names an env var is indirection, not a secret; a value that is
//     not an env var name (and not a `!command`) resolves to itself — a pinned
//     credential.
//
// The door cannot see the container's env, so "is an env reference" is a
// syntactic judgment: a bare identifier that is a plausible env-var name (or a
// `!command`) passes; anything else (contains spaces, punctuation like a bearer
// token, a URL, etc.) is treated as a literal secret and rejected.
func rejectCredentialModels(mapping map[string]any, joined string) error {
	providers, ok := mapping["providers"].(map[string]any)
	if !ok {
		return nil
	}
	for name, raw := range providers {
		prov, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if v, present := prov["apiKey"]; present && v != nil {
			return fmt.Errorf("%w: models member %q sets providers.%s.apiKey (provider credentials never ride the config bundle)", ErrInvalidArgument, joined, name)
		}
		headers, ok := prov["headers"].(map[string]any)
		if !ok {
			continue
		}
		for h, hv := range headers {
			s, ok := hv.(string)
			if !ok {
				continue
			}
			if !isEnvIndirection(s) {
				return fmt.Errorf("%w: models member %q sets providers.%s.headers.%s to a literal secret (pin it to an env reference instead)", ErrInvalidArgument, joined, name, h)
			}
		}
	}
	return nil
}

// envIndirectionPattern matches a bare env-var name — the syntactic form the SDK
// treats as an env reference (resolveConfigValue: env-name-first). A value that
// matches is indirection (passes); anything else is a pinned literal.
var envIndirectionPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// isEnvIndirection reports whether a header value is an env reference (or a
// `!command` indirection) rather than a pinned literal secret. It mirrors the
// syntactic surface of the SDK's resolveConfigValue: a leading `!` is a command
// (indirection), and an otherwise bare env-var-name token is an env reference.
// A literal secret (a bearer token, an inline key) matches neither.
func isEnvIndirection(value string) bool {
	if strings.HasPrefix(value, "!") {
		return true
	}
	return envIndirectionPattern.MatchString(value)
}
