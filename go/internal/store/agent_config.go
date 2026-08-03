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

	yaml "go.yaml.in/yaml/v3"
)

// The fleet CONFIG-BUNDLE store (SEA-1624 T1). One fleet-wide singleton bundle
// row (agent_config_bundle, 0008_agent_config) holds the gzip-tarball of the
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
)

// Top-level regular-file members admitted by exact filename (not under a top
// dir): the fleet context file and the fleet model config (SEA-1678 T1). Any
// other top-level file stays rejected.
const (
	memberAgentsMD = "AGENTS.md"
	memberModels   = "models.yml"
	// settingsMember is the ONLY file admitted under settings/ — yml-only per
	// OQ-1 (no .yaml/.json variant, no other name).
	settingsMember = "settings/config.yml"
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
}

// configNamePattern is the grammar for a config entry's <name> segment —
// skills/<name>, extensions/<name>, mcp/<name>.json. A declared name is
// validated at the store door (PutAgentConfig) before it can reach a row
// because it later becomes a host path segment under the agent's config dir
// (T4): constrained at the door, not escaped downstream (mirrors secrets.go's
// secretNamePattern posture).
var configNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

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
// passes, upserts it as the single current bundle (SEA-1624 T1). It returns the
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
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO agent_config_bundle (singleton, version, bundle)
		 VALUES (TRUE, $1, $2)
		 ON CONFLICT (singleton)
		 DO UPDATE SET version = EXCLUDED.version, bundle = EXCLUDED.bundle, updated_at = now()`,
		version, bundle,
	); err != nil {
		return "", fmt.Errorf("store: put agent config: %w", err)
	}
	return version, nil
}

// CurrentAgentConfig returns the single current config bundle and its canonical
// content version. ErrNotFound when no bundle has been declared — a valid state
// downstream (the fetch path then materializes an empty config dir), but the
// store still reports the absence; the caller decides empty-is-ok.
func (s *Store) CurrentAgentConfig(ctx context.Context) (version string, bundle []byte, err error) {
	if err := s.pool.QueryRow(ctx,
		`SELECT version, bundle FROM agent_config_bundle WHERE singleton = TRUE`,
	).Scan(&version, &bundle); err != nil {
		if noRows(err) {
			return "", nil, fmt.Errorf("%w: no agent config bundle declared", ErrNotFound)
		}
		return "", nil, fmt.Errorf("store: read agent config: %w", err)
	}
	return version, bundle, nil
}

// DeleteAgentConfig clears the fleet config bundle, returning the store to the
// unconfigured state (CurrentAgentConfig then reports ErrNotFound — a valid
// downstream state, the empty-config door). Idempotent: deleting when the
// singleton is already absent is a no-op success, not ErrNotFound — the caller's
// intent (no bundle) already holds, so a repeated Delete or a Delete on a
// never-configured fleet both succeed. This is the operator's explicit
// return-to-unconfigured path (SEA-1625 T2), chosen over blessing an
// empty-tarball push.
func (s *Store) DeleteAgentConfig(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM agent_config_bundle WHERE singleton = TRUE`,
	); err != nil {
		return fmt.Errorf("store: delete agent config: %w", err)
	}
	return nil
}

// AgentConfigInfo reports the current bundle's version and the NAMES of its
// declared members, bucketed by top dir (skills / extensions / mcp) — names
// only, never content (SEA-1625 T2). Each bucket is deduplicated and sorted: a
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
// AGENTS.md, models.yml). Names only, never content (SEA-1625 T2 / SEA-1678 T1).
type AgentConfigInfoResult struct {
	Version     string
	Skills      []string
	Extensions  []string
	McpServers  []string
	Rules       []string
	Subagents   []string
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
		}
	}
	info.Skills = sortedKeys(skillSet)
	info.Extensions = sortedKeys(extSet)
	info.McpServers = sortedKeys(mcpSet)
	info.Rules = sortedKeys(ruleSet)
	info.Subagents = sortedKeys(agentSet)
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
		return nil, fmt.Errorf("%w: bundle member %q is not under skills/, extensions/, mcp/, settings/, rules/, or agents/ and is not a top-level %s or %s", ErrInvalidArgument, name, memberAgentsMD, memberModels)
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
