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
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/RigelBuild/compass/go/internal/store/db"
	yaml "go.yaml.in/yaml/v3"
)

// The fleet CONFIG-BUNDLE store (RIG-1624 T1). One fleet-wide singleton bundle row
// holds the gzip-tarball of skills/, extensions/, mcp/ material every agent materializes
// into its scoped config dir. Config is CURRENT-ONLY: PutAgentConfig replaces the row,
// version is the content hash. Credential-free by MVP rule (CD-3).

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
	memberLocksYML   = "locks.yml"
	jsonNull         = "null"
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

// configNamePattern is the grammar for a config entry's <name> segment (skills/<name>,
// extensions/<name>, mcp/<name>.json). Validated at the store door before it can reach
// a row, since it later becomes a host path segment (mirrors secretNamePattern).
var configNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// frontmatterNamePattern matches a `name:` line in an agents/*.md frontmatter
// block for the tolerant line-scan fallback in agentDefFrontmatterName, used
// when a strict whole-block YAML parse fails because a SIBLING field is a
// YAML-ambiguous scalar. Anchored; the first match wins.
var (
	frontmatterNamePattern = regexp.MustCompile(`^name:\s*(.*)$`)
	envIndirectionPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

const (
	maxDecompressedBytes int64 = 64 << 20
	maxFileCount               = 4096
)

type layerCaps struct {
	bytes int64
	files int
}

var (
	defaultLayerCaps = layerCaps{bytes: 48 << 20, files: 2560}
	overlayLayerCaps = layerCaps{bytes: 4 << 20, files: 512}
)

var errBundleTooLarge = fmt.Errorf("%w: bundle exceeds decompressed size cap of %d bytes", ErrInvalidArgument, maxDecompressedBytes)

func bundleSizeError(limit int64) error {
	return fmt.Errorf("%w: bundle exceeds layer content cap of %d bytes", ErrInvalidArgument, limit)
}

// cappedReader bounds the total bytes read from an underlying reader, returning
// errBundleTooLarge once the cap is crossed. Wrapping the gunzip (DECOMPRESSED)
// stream is the gzip-bomb defense regardless of how small the compressed input is.
type cappedReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	nn, err := c.r.Read(p)
	c.n += int64(nn)
	limit := c.max
	if limit == 0 {
		limit = maxDecompressedBytes
	}
	if c.n > limit {
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
// ErrInvalidArgument and NO row is written. A bundle whose profile references a
// model stable name absent from the current registry (the reverse orphan guard,
// design.md §P2 L530-532) is likewise ErrInvalidArgument. actor is the
// operator-scoped writer (empty → ErrInvalidArgument); the singleton has no
// per-actor column, but the param documents the operator-scoped write and
// matches the record.
func (s *Store) PutAgentConfig(ctx context.Context, actor AccountID, bundle []byte) (version string, err error) {
	if actor == "" {
		return "", fmt.Errorf("%w: config-bundle writer account id is required", ErrInvalidArgument)
	}
	version, profiles, err := validateAndHashConfigBundle(bundle, LayerDefault)
	if err != nil {
		return "", err
	}
	if err := s.checkBundleProfileRefsAgainstRegistry(ctx, profiles); err != nil {
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

// ValidateConfigBundle validates a default config bundle and returns its canonical content version.
func ValidateConfigBundle(bundle []byte) (version string, err error) {
	version, _, err = validateAndHashConfigBundle(bundle, LayerDefault)
	return version, err
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
// unconfigured state. Idempotent: deleting an already-absent singleton is a no-op
// success, not ErrNotFound — the operator's explicit return-to-unconfigured path
// (RIG-1625 T2), chosen over blessing an empty-tarball push.
func (s *Store) DeleteAgentConfig(ctx context.Context) error {
	if err := s.q.DeleteAgentConfig(ctx); err != nil {
		return fmt.Errorf("store: delete agent config: %w", err)
	}
	return nil
}

// AgentConfigInfo reports the current bundle's version and the NAMES of its declared
// members, bucketed by top dir — names only, never content (RIG-1625 T2). Each bucket
// is deduplicated and sorted. ErrNotFound when no bundle is declared. It re-walks the
// stored bundle with the SAME grammar the door enforced at Put (configMemberParts + the
// cappedReader cap), reusing the door's path validation rather than re-inventing a walk.
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

// configBundleMemberNames walks a stored config bundle and returns the declared member
// names bucketed by top dir, deduplicated and sorted. It reads only the tar HEADERS
// (bounded by the cappedReader guard), never member CONTENT; skills/<name>/... and
// extensions/<name>/... contribute <name>, mcp/<name>.json contributes <name>.
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
		name := canonicalConfigPath(hdr.Name)
		parts, err := configMemberParts(name)
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

// configBundleProfileBodies walks the STORED config bundle and returns each profile's
// raw profile.yml body keyed by profile <name>, for the registry-side orphan guard.
// PutAgentConfig takes the same map from the door's single walk instead.
func configBundleProfileBodies(bundle []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return nil, fmt.Errorf("%w: bundle is not a valid gzip stream: %w", ErrInvalidArgument, err)
	}
	// Read-only gunzip: Close only releases the decompressor, so its error is
	// not actionable here (nothing was written to flush).
	defer func() { _ = gz.Close() }()

	bodies := make(map[string][]byte)
	tr := tar.NewReader(&cappedReader{r: gz})
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, errBundleTooLarge) {
				return nil, errBundleTooLarge
			}
			return nil, fmt.Errorf("%w: bundle is not a valid tar stream: %w", ErrInvalidArgument, err)
		}
		parts, err := configMemberParts(canonicalConfigPath(hdr.Name))
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		// A profile is exactly profiles/<name>/profile.yml (door grammar).
		if len(parts) == 3 && parts[0] == topDirProfiles && parts[2] == memberProfileYML {
			content, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("%w: reading profile member %q: %w", ErrInvalidArgument, hdr.Name, err)
			}
			bodies[parts[1]] = content
		}
	}
	return bodies, nil
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

// configMemberContent is one regular file collected during bundle validation.
type configMemberContent struct {
	name    string
	parts   []string
	content []byte
}

// validateAndHashConfigBundle is the store door for config bundles. It validates
// the decompressed tar stream, lints the collected members, then hashes their
// canonical names and contents. It also returns profile bodies keyed by name.
func validateAndHashConfigBundle(bundle []byte, layer ConfigLayer) (string, map[string][]byte, error) {
	caps, err := capsForLayer(layer)
	if err != nil {
		return "", nil, err
	}
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return "", nil, fmt.Errorf("%w: bundle is not a valid gzip stream: %w", ErrInvalidArgument, err)
	}
	defer func() { _ = gz.Close() }()

	members, err := readAndValidateConfigMembers(gz, layer, caps)
	if err != nil {
		return "", nil, err
	}
	profiles, err := lintConfigMembers(members)
	if err != nil {
		return "", nil, err
	}
	return hashConfigMembers(members), profiles, nil
}

func readAndValidateConfigMembers(gz io.Reader, layer ConfigLayer, caps layerCaps) ([]configMemberContent, error) {
	members := make([]configMemberContent, 0)
	seen := make(map[string]bool)
	var contentBytes int64
	tr := tar.NewReader(&cappedReader{r: gz, max: maxDecompressedBytes})
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return members, nil
		}
		if err != nil {
			if errors.Is(err, errBundleTooLarge) {
				return nil, errBundleTooLarge
			}
			return nil, fmt.Errorf("%w: bundle is not a valid tar stream: %w", ErrInvalidArgument, err)
		}
		if err := validateConfigHeaderType(hdr); err != nil {
			return nil, err
		}
		name := canonicalConfigPath(hdr.Name)
		parts, err := configMemberParts(name)
		if err != nil {
			return nil, err
		}
		if err := validateConfigHeaderPath(parts, hdr.Typeflag, layer); err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		hdr.Name = name
		if err := validateConfigFileLimits(hdr, caps, &contentBytes, seen, len(members)+1); err != nil {
			return nil, err
		}
		content, err := validateRegularMember(parts, name, tr)
		if err != nil {
			if errors.Is(err, errBundleTooLarge) {
				return nil, errBundleTooLarge
			}
			return nil, err
		}
		members = append(members, configMemberContent{name: name, parts: parts, content: content})
	}
}

func validateConfigHeaderType(hdr *tar.Header) error {
	switch hdr.Typeflag {
	case tar.TypeSymlink:
		return fmt.Errorf("%w: bundle member %q is a symlink (not allowed)", ErrInvalidArgument, hdr.Name)
	case tar.TypeLink:
		return fmt.Errorf("%w: bundle member %q is a hardlink (not allowed)", ErrInvalidArgument, hdr.Name)
	case tar.TypeDir, tar.TypeReg:
		return nil
	default:
		return fmt.Errorf("%w: bundle member %q has unsupported typeflag %d (only regular files and directories are allowed)", ErrInvalidArgument, hdr.Name, hdr.Typeflag)
	}
}

func validateConfigHeaderPath(parts []string, kind byte, layer ConfigLayer) error {
	return validateConfigMemberPath(parts, kind, layer)
}

// validateConfigMemberPath is shared by the upload door, stored-row validator,
// and lock parser so each applies one layer-aware path grammar.
func validateConfigMemberPath(parts []string, kind byte, layer ConfigLayer) error {
	if len(parts) == 1 && parts[0] == memberLocksYML {
		if kind != tar.TypeReg || layer != LayerTenant {
			return fmt.Errorf("%w: %s is allowed only as a tenant regular file", ErrInvalidArgument, memberLocksYML)
		}
		return nil
	}
	if kind == tar.TypeDir {
		return configDirectoryPathAllowed(parts, layer)
	}
	joined := strings.Join(parts, "/")
	if len(parts) == 1 && (parts[0] == memberAgentsMD || parts[0] == memberModels) {
		return nil
	}
	if len(parts) < 2 || !configBundleTopDirs[parts[0]] {
		return fmt.Errorf("%w: bundle member %q is not under an admitted config directory", ErrInvalidArgument, joined)
	}
	if parts[len(parts)-1] == memberLocksYML {
		return fmt.Errorf("%w: %s is allowed only at the bundle root in the tenant layer", ErrInvalidArgument, memberLocksYML)
	}
	if parts[0] == topDirProfiles && layer != LayerDefault {
		return fmt.Errorf("%w: profiles/ is allowed only in the default layer", ErrInvalidArgument)
	}
	if parts[0] == topDirSkills || parts[0] == topDirExtensions {
		if len(parts) < 3 || !configNamePattern.MatchString(parts[1]) || (len(parts) > 3 && !validNestedConfigDir(parts[2:len(parts)-1])) {
			return fmt.Errorf("%w: %s member %q must be a file under %s/<name>/", ErrInvalidArgument, parts[0], joined, parts[0])
		}
		return nil
	}
	return validateStoredMemberPathGrammar(parts, layer)
}

func validateStoredMemberPathGrammar(parts []string, layer ConfigLayer) error {
	if len(parts) == 1 {
		if parts[0] == memberAgentsMD || parts[0] == memberModels {
			return nil
		}
		return fmt.Errorf("%w: stored top-level file %q is not admitted", ErrInvalidArgument, parts[0])
	}
	switch parts[0] {
	case topDirSettings:
		if strings.Join(parts, "/") == settingsMember {
			return nil
		}
	case topDirRules:
		if len(parts) == 2 && validateFlatNamedMember(parts[0], parts[1], strings.Join(parts, "/"), ".md", ".mdc") == nil {
			return nil
		}
	case topDirAgents:
		if len(parts) == 2 && validateFlatNamedMember(parts[0], parts[1], strings.Join(parts, "/"), ".md") == nil {
			return nil
		}
	case topDirPrompts:
		if len(parts) == 3 && parts[2] == memberSystemMD && configNamePattern.MatchString(parts[1]) {
			return nil
		}
	case topDirProfiles:
		if layer == LayerDefault && len(parts) == 3 && parts[2] == memberProfileYML && configNamePattern.MatchString(parts[1]) {
			return nil
		}
	case topDirMCP:
		if len(parts) == 2 {
			base, ok := strings.CutSuffix(parts[1], ".json")
			if ok && configNamePattern.MatchString(base) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: stored member %q is not admitted", ErrInvalidArgument, strings.Join(parts, "/"))
}

func storedDirectoryPathAllowed(parts []string, layer ConfigLayer) error {
	return validateConfigMemberPath(parts, tar.TypeDir, layer)
}

func storedMemberPathAllowed(parts []string, layer ConfigLayer) error {
	return validateConfigMemberPath(parts, tar.TypeReg, layer)
}

func configDirectoryPathAllowed(parts []string, layer ConfigLayer) error {
	if len(parts) == 1 {
		if parts[0] == memberLocksYML || !configBundleTopDirs[parts[0]] {
			return fmt.Errorf("%w: bundle directory %q is not admitted", ErrInvalidArgument, strings.Join(parts, "/"))
		}
		if parts[0] == topDirProfiles && layer != LayerDefault {
			return fmt.Errorf("%w: profiles/ is allowed only in the default layer", ErrInvalidArgument)
		}
		return nil
	}
	if len(parts) == 2 && (parts[0] == topDirSkills || parts[0] == topDirExtensions) && configNamePattern.MatchString(parts[1]) {
		return nil
	}
	if len(parts) == 2 && parts[0] == topDirProfiles && layer == LayerDefault && configNamePattern.MatchString(parts[1]) {
		return nil
	}
	if len(parts) == 2 && parts[0] == topDirPrompts && configNamePattern.MatchString(parts[1]) {
		return nil
	}
	if len(parts) > 2 && (parts[0] == topDirSkills || parts[0] == topDirExtensions) && configNamePattern.MatchString(parts[1]) && validNestedConfigDir(parts[2:]) {
		return nil
	}
	return fmt.Errorf("%w: bundle directory %q is not admitted", ErrInvalidArgument, strings.Join(parts, "/"))
}

func validNestedConfigDir(parts []string) bool {
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `/\\`) {
			return false
		}
	}
	return len(parts) > 0
}

func validateConfigFileLimits(hdr *tar.Header, caps layerCaps, contentBytes *int64, seen map[string]bool, fileCount int) error {
	if fileCount > caps.files {
		return fmt.Errorf("%w: bundle exceeds file-count cap of %d files", ErrInvalidArgument, caps.files)
	}
	if seen[hdr.Name] {
		return fmt.Errorf("%w: bundle contains duplicate member %q", ErrInvalidArgument, hdr.Name)
	}
	seen[hdr.Name] = true
	if hdr.Size < 0 || hdr.Size > caps.bytes-*contentBytes {
		return bundleSizeError(caps.bytes)
	}
	*contentBytes += hdr.Size
	return nil
}

func lintConfigMembers(members []configMemberContent) (map[string][]byte, error) {
	agentDefNames := make(map[string]bool)
	profiles := make(map[string][]byte)
	manifestBodies := make(map[string][]byte)
	memberPaths := make(map[string]bool, len(members))
	for _, member := range members {
		memberPaths[member.name] = true
		switch {
		case len(member.parts) == 2 && member.parts[0] == topDirAgents:
			if name := agentDefFrontmatterName(member.content); name != "" {
				agentDefNames[name] = true
			}
		case len(member.parts) == 3 && member.parts[0] == topDirProfiles && member.parts[2] == memberProfileYML:
			profiles[member.parts[1]] = member.content
		case len(member.parts) == 3 && member.parts[0] == topDirExtensions && member.parts[2] == "package.json":
			manifestBodies[member.parts[1]] = member.content
		}
	}
	if err := lintProfileAgentKeys(profiles, agentDefNames); err != nil {
		return nil, err
	}
	if err := lintExtensionManifests(manifestBodies, memberPaths); err != nil {
		return nil, err
	}
	return profiles, nil
}

func hashConfigMembers(members []configMemberContent) string {
	sort.Slice(members, func(i, j int) bool { return members[i].name < members[j].name })
	h := sha256.New()
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(members)))
	_, _ = h.Write(lenBuf[:])
	for _, member := range members {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(member.name)))
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write([]byte(member.name))
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(member.content)))
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write(member.content)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func validateStoredConfigBundle(bundle []byte, layer ConfigLayer) error {
	if layer < LayerDefault || layer > LayerRole {
		return fmt.Errorf("unknown stored config layer %d", layer)
	}
	caps := layerCaps{bytes: maxDecompressedBytes, files: maxFileCount}
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return fmt.Errorf("invalid gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()
	decompressed := &cappedReader{r: gz, max: maxDecompressedBytes}
	tr := tar.NewReader(decompressed)
	fileCount := 0
	var contentBytes int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			if _, err := io.Copy(io.Discard, decompressed); err != nil {
				return err
			}
			if err := gz.Close(); err != nil {
				return err
			}
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			return fmt.Errorf("stored member %q has disallowed type %d", hdr.Name, hdr.Typeflag)
		}
		parts, err := configMemberParts(canonicalConfigPath(hdr.Name))
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := storedDirectoryPathAllowed(parts, layer); err != nil {
				return err
			}
			continue
		}
		fileCount++
		if fileCount > caps.files {
			return fmt.Errorf("stored bundle exceeds file-count cap %d", caps.files)
		}
		if err := storedMemberPathAllowed(parts, layer); err != nil {
			return err
		}
		if hdr.Size < 0 || hdr.Size > caps.bytes-contentBytes {
			return fmt.Errorf("stored bundle exceeds layer content cap %d", caps.bytes)
		}
		contentBytes += hdr.Size
		if _, err := io.CopyN(io.Discard, tr, hdr.Size); err != nil {
			return err
		}
	}
}

func capsForLayer(layer ConfigLayer) (layerCaps, error) {
	switch layer {
	case LayerDefault:
		return defaultLayerCaps, nil
	case LayerTenant, LayerUser, LayerRole:
		return overlayLayerCaps, nil
	default:
		return layerCaps{}, fmt.Errorf("%w: unknown config layer %d", ErrInvalidArgument, layer)
	}
}

// configMemberParts validates a member path for escapes and the top-dir allowlist,
// returning its cleaned path components. It rejects an absolute path, the empty/root
// path, and any "", ".", or ".." component.
func configMemberParts(name string) ([]string, error) {
	if strings.HasPrefix(name, "/") {
		return nil, fmt.Errorf("%w: bundle member %q is an absolute path", ErrInvalidArgument, name)
	}
	if name == "" {
		return nil, fmt.Errorf("%w: bundle member has an empty path", ErrInvalidArgument)
	}
	parts := strings.Split(name, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return nil, fmt.Errorf("%w: bundle member %q has an illegal path component %q", ErrInvalidArgument, name, p)
		}
	}
	if len(parts) == 1 && parts[0] == memberLocksYML {
		return parts, nil
	}
	if len(parts) == 1 && (parts[0] == memberAgentsMD || parts[0] == memberModels) {
		return parts, nil
	}
	if !configBundleTopDirs[parts[0]] {
		return nil, fmt.Errorf("%w: bundle member %q is not under an admitted config directory or top-level member", ErrInvalidArgument, name)
	}
	return parts, nil
}

// validateRegularMember enforces the per-file grammar and reads the member's content
// (bounded by the enclosing cappedReader), returning it for the hash. mcp/*.json parses
// as JSON; settings/config.yml and models.yml as credential-free YAML mappings; the rest
// are name-grammar-checked prose. The read propagates errBundleTooLarge on cap crossing.
func validateRegularMember(parts []string, joined string, r io.Reader) ([]byte, error) {
	if len(parts) == 1 && parts[0] == memberLocksYML {
		content, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		if _, err := parseConfigLocks(content); err != nil {
			return nil, err
		}
		return content, nil
	}
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
		if len(parts) != 3 || parts[2] != memberSystemMD || !configNamePattern.MatchString(parts[1]) {
			return nil, fmt.Errorf("%w: prompts member %q must be prompts/<role>/%s", ErrInvalidArgument, joined, memberSystemMD)
		}
		return io.ReadAll(r)
	case topDirProfiles:
		return validateProfileMember(parts, joined, r)
	}
	if len(parts) == 1 {
		switch parts[0] {
		case memberAgentsMD:
			return io.ReadAll(r)
		case memberModels:
			return validateModelsMember(joined, r)
		}
	}
	if len(parts) < 2 || !configNamePattern.MatchString(parts[1]) {
		return nil, fmt.Errorf("%w: bundle member name in %q must match %s", ErrInvalidArgument, joined, configNamePattern.String())
	}
	return io.ReadAll(r)
}

// validateMCPMember validates mcp/<name>.json and enforces path identity.
func validateMCPMember(parts []string, joined string, r io.Reader) ([]byte, error) {
	if len(parts) != 2 {
		return nil, fmt.Errorf("%w: mcp member %q must be mcp/<name>.json", ErrInvalidArgument, joined)
	}
	base, ok := strings.CutSuffix(parts[1], ".json")
	if !ok || !configNamePattern.MatchString(base) {
		return nil, fmt.Errorf("%w: mcp member %q must use a valid .json server name", ErrInvalidArgument, joined)
	}
	content, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(content, &root); err != nil || root == nil {
		return nil, fmt.Errorf("%w: mcp member %q must be a JSON object", ErrInvalidArgument, joined)
	}
	serversRaw, ok := root["mcpServers"]
	if !ok || string(serversRaw) == jsonNull {
		return nil, fmt.Errorf("%w: mcp member %q mcpServers must be an object", ErrInvalidArgument, joined)
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(serversRaw, &servers); err != nil || servers == nil {
		return nil, fmt.Errorf("%w: mcp member %q mcpServers must be an object", ErrInvalidArgument, joined)
	}
	if len(servers) > 1 || len(servers) == 1 && servers[base] == nil {
		return nil, fmt.Errorf("%w: mcp member %q must contain only server %q", ErrInvalidArgument, joined, base)
	}
	if len(servers) == 1 {
		var server map[string]any
		if err := json.Unmarshal(servers[base], &server); err != nil || server == nil {
			return nil, fmt.Errorf("%w: MCP server %q must be an object", ErrInvalidArgument, base)
		}
	}
	var document map[string]any
	if err := json.Unmarshal(content, &document); err != nil {
		return nil, fmt.Errorf("%w: mcp member %q must be an object", ErrInvalidArgument, joined)
	}
	if err := rejectCredentialMCP(document, joined); err != nil {
		return nil, err
	}
	return content, nil
}
func rejectCredentialMCP(doc map[string]any, joined string) error {
	return walkCredentialMCP(doc, "", joined)
}

func walkCredentialMCP(value any, path, member string) error {
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			childPath := path + "." + key
			handled, err := validateCredentialMCPField(key, child, childPath, member)
			if err != nil {
				return err
			}
			if handled {
				continue
			}
			if err := walkCredentialMCP(child, childPath, member); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range node {
			if err := walkCredentialMCP(child, fmt.Sprintf("%s[%d]", path, i), member); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateCredentialMCPField(key string, value any, path, member string) (bool, error) {
	switch strings.ToLower(key) {
	case "clientsecret", "envpolicy", "headerpolicy":
		return true, fmt.Errorf("%w: MCP member %q contains forbidden field %s", ErrInvalidArgument, member, path)
	case "env", "headers":
		return true, validateMCPIndirections(value, path, member)
	case "url", "tokenurl":
		return true, validateMCPURL(value, path, member)
	case "args":
		return true, validateMCPArgs(value, path, member)
	default:
		return false, nil
	}
}

func validateMCPIndirections(value any, path, member string) error {
	values, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: MCP member %q field %s must be an object", ErrInvalidArgument, member, path)
	}
	for name, raw := range values {
		text, ok := raw.(string)
		if !ok || (!strings.HasPrefix(text, "!") && !envIndirectionPattern.MatchString(text)) {
			return fmt.Errorf("%w: MCP member %q field %s.%s must use an environment indirection", ErrInvalidArgument, member, path, name)
		}
	}
	return nil
}

func validateMCPURL(value any, path, member string) error {
	text, ok := value.(string)
	if !ok {
		return fmt.Errorf("%w: MCP member %q URL field %s must be a string", ErrInvalidArgument, member, path)
	}
	if isHTTPURL(text) {
		return validateCredentialFreeMCPURL(text, path, member)
	}
	return nil
}

func validateMCPArgs(value any, path, member string) error {
	args, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%w: MCP member %q args must be a list", ErrInvalidArgument, member)
	}
	for i, raw := range args {
		text, ok := raw.(string)
		if !ok {
			continue
		}
		if strings.Contains(strings.ToLower(text), "exaapikey=") {
			return fmt.Errorf("%w: MCP member %q args contains an API key", ErrInvalidArgument, member)
		}
		if isHTTPURL(text) {
			argPath := fmt.Sprintf("%s[%d]", path, i)
			if err := validateCredentialFreeMCPURL(text, argPath, member); err != nil {
				return err
			}
		}
	}
	return nil
}

// isHTTPURL detects an http(s) URL after the C0-control/space trim WHATWG applies.
func isHTTPURL(value string) bool {
	scheme, _, ok := strings.Cut(trimURLSpace(value), ":")
	return ok && (strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https"))
}

func trimURLSpace(value string) string {
	return strings.TrimFunc(value, func(r rune) bool { return r <= ' ' })
}

func validateCredentialFreeMCPURL(value, path, member string) error {
	normalized, authority, rawAuthority, isHTTP := normalizeCredentialMCPURL(trimURLSpace(value))
	if isHTTP && (strings.Contains(authority, "@") || strings.Contains(rawAuthority, "@")) {
		return fmt.Errorf("%w: MCP member %q has URL userinfo at %s", ErrInvalidArgument, member, path)
	}
	u, err := url.Parse(normalized)
	if err != nil {
		return fmt.Errorf("%w: MCP member %q has an unparseable URL at %s", ErrInvalidArgument, member, path)
	}
	if u.User != nil {
		return fmt.Errorf("%w: MCP member %q has URL userinfo at %s", ErrInvalidArgument, member, path)
	}
	if name, err := credentialMCPQueryParam(u.RawQuery); err != nil || name != "" {
		return fmt.Errorf("%w: MCP member %q has a credential query at %s", ErrInvalidArgument, member, path)
	}
	return nil
}

func normalizeCredentialMCPURL(value string) (normalized, authority, rawAuthority string, isHTTP bool) {
	scheme, rest, ok := strings.Cut(value, ":")
	if !ok {
		return value, "", "", false
	}
	isHTTP = strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https")
	if !isHTTP {
		return value, "", "", false
	}
	normalizedRest := strings.ReplaceAll(rest, "\\", "/")
	if !strings.HasPrefix(normalizedRest, "//") {
		normalizedRest = "//" + strings.TrimLeft(normalizedRest, "/")
	}
	normalized = strings.ToLower(scheme) + ":" + normalizedRest
	authority = strings.TrimPrefix(normalizedRest, "//")
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	rawAuthority = strings.TrimLeft(rest, "/")
	if end := strings.IndexAny(rawAuthority, "/?#"); end >= 0 {
		rawAuthority = rawAuthority[:end]
	}
	return normalized, authority, rawAuthority, true
}

func extensionIDFromPath(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) < 3 || parts[0] != topDirExtensions {
		return ""
	}
	base := parts[len(parts)-1]
	if base == "index.ts" || base == "index.js" {
		return parts[len(parts)-2]
	}
	if !strings.HasSuffix(base, ".ts") && !strings.HasSuffix(base, ".js") {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSuffix(base, ".ts"), ".js")
}

func lintExtensionManifests(manifests map[string][]byte, members map[string]bool) error {
	for member := range members {
		parts := strings.Split(member, "/")
		if len(parts) == 2 && parts[0] == topDirExtensions {
			return fmt.Errorf("%w: extension must be a directory member, got %q", ErrInvalidArgument, member)
		}
	}
	for id, body := range manifests {
		var manifest struct {
			OMP *struct {
				Extensions []string `json:"extensions"`
			} `json:"omp"`
			PI *struct {
				Extensions []string `json:"extensions"`
			} `json:"pi"`
		}
		if err := json.Unmarshal(body, &manifest); err != nil {
			continue
		}
		var entries []string
		if manifest.OMP != nil {
			entries = manifest.OMP.Extensions
		} else if manifest.PI != nil {
			entries = manifest.PI.Extensions
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry, "/") || strings.Contains(entry, "\\") {
				return fmt.Errorf("%w: extension %q has unsafe manifest path %q", ErrInvalidArgument, id, entry)
			}
			normalized := strings.TrimPrefix(entry, "./")
			cleaned := path.Clean(normalized)
			if cleaned != normalized || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
				return fmt.Errorf("%w: extension %q has unsafe manifest path %q", ErrInvalidArgument, id, entry)
			}
			candidate := "extensions/" + id + "/" + cleaned
			if !members[candidate] || extensionIDFromPath(candidate) != id {
				return fmt.Errorf("%w: extension %q manifest entry %q does not resolve to its directory identity", ErrInvalidArgument, id, entry)
			}
		}
	}
	return nil
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
// with no credentialed provider surface (see rejectCredentialModels).
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

// profileSupersetKeys is the closed set of top-level keys a profile.yml may declare
// (RIG-2968 T1). v1 CONSUMES only `models`; `corpus`/`extensions`/`settings` are
// schema'd but consumption is deferred — all four are ACCEPTED so later phases grow
// additively. "Unknown key" = a top-level key OUTSIDE this set.
var profileSupersetKeys = map[string]bool{
	"models":     true,
	"corpus":     true,
	"extensions": true,
	"settings":   true,
}

// validateProfileMember validates a profiles/<name>/profile.yml member in isolation:
// three components with filename profile.yml and a grammar-valid <name>, a YAML-mapping
// body, superset top-level keys, and string-shaped models.* selectors. The CROSS-MEMBER
// models.agents key lint runs in validateAndHashConfigBundle over the collected set.
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
	// The profile settings sub-mapping shares the settings/config.yml credential axis, so
	// reuse the same denylist here (F3). The extensions/corpus axes are deferred.
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

// rejectNonStringKeys rejects a YAML mapping value with any non-string key. yaml.v3
// decodes such a mapping as map[any]any, so a v.(map[string]any) assertion fails OPEN,
// silently skipping every key-level check. A non-mapping value passes through.
func rejectNonStringKeys(v any, joined, path string) error {
	if _, ok := v.(map[any]any); ok {
		return fmt.Errorf("%w: profile member %q %s must be a string-keyed mapping", ErrInvalidArgument, joined, path)
	}
	return nil
}

// validateProfileModelSelectors enforces the models.* selector SHAPE: each selector is
// an opaque string. models.manager and every models.agents value, where present, must be
// a string; an absent/null axis is fine. A mapping with any non-string key is rejected up
// front (yaml.v3 decodes it as map[any]any, and a naive assertion fails OPEN).
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
		// A non-mapping models value (scalar/list) is a deferred-shape concern, not a v1
		// door failure. The map[any]any case was already rejected above.
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

// lintProfileAgentKeys is the CROSS-MEMBER models.agents key lint (RIG-2968 T1): every
// key under a profile's models.agents must match the FRONTMATTER name: of an agents/*.md
// def in the SAME bundle. The SDK resolves a subagent by agent.name, so a key matching no
// def is a SILENT no-op at spawn; the lint turns that typo into a door failure.
func lintProfileAgentKeys(profiles map[string][]byte, agentDefNames map[string]bool) error {
	for name, body := range profiles {
		joined := topDirProfiles + "/" + name + "/" + memberProfileYML
		mapping, err := parseYAMLMapping(body, joined)
		if err != nil {
			// Already validated during the per-member pass; a re-parse failure here would
			// be a logic error, but fail closed regardless.
			return err
		}
		// The per-member selector pass already rejected any non-string-keyed models
		// mapping (yaml.v3's map[any]any), so these map[string]any assertions cannot
		// fail-open on that class; a miss below is a genuine non-mapping value.
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

// agentDefFrontmatterName parses an agents/*.md def's leading YAML frontmatter and
// returns its name: field, or "" if absent. It recovers the name even when a sibling
// field is a YAML-ambiguous scalar (full parse first, then a tolerant `name:` line-scan,
// mirroring the SDK loader). The lint keys on this parsed name, NOT the filename stem.
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
	// Whole-block parse failed or yielded no name: a sibling field may be YAML-ambiguous.
	// Fall back to a tolerant line-scan for the name field alone, mirroring the SDK.
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

// parseYAMLMapping parses content as YAML and requires it to be a mapping (rejecting a
// scalar or sequence), returning the decoded map. Door twin of the mcp/*.json "must
// parse as JSON" rule; best-effort (the container-side Bun parse is authoritative). An
// empty document (YAML null) is treated as an empty mapping, matching the loader.
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
// isCredential markers, read from the installed @oh-my-pi/pi-coding-agent;
// refresh it at an SDK bump with `go generate ./...`.

//go:generate go run gen_credential_keys.go

// rejectCredentialSettings walks the parsed settings/config.yml mapping and rejects any
// set path credential-marked in the SDK (credentialKeys). A dotted path (auth.broker.token)
// nests in YAML, so a key is "set" when the full nested path resolves to a present leaf.
// The door is authoritative: it survives a raw PutAgentConfig (GC-5).
func rejectCredentialSettings(mapping map[string]any, joined string) error {
	for _, key := range credentialKeys {
		if yamlPathIsSet(mapping, strings.Split(key, ".")) {
			return fmt.Errorf("%w: settings member %q sets credential-marked key %q (credentials never ride the config bundle)", ErrInvalidArgument, joined, key)
		}
	}
	return nil
}

// yamlPathIsSet reports whether the nested path resolves to a present (non-nil) value.
// An intermediate segment that is not a mapping means the path is not set. It descends
// through both string-keyed and non-string-keyed nodes, so a non-string sibling one
// level above a credential leaf cannot shield it from the denylist walk.
func yamlPathIsSet(mapping map[string]any, segments []string) bool {
	var current any = mapping
	for _, seg := range segments {
		next, present := yamlMapIndex(current, seg)
		if !present || next == nil {
			return false
		}
		current = next
	}
	return true
}

// yamlMapIndex looks up a string key in a YAML-decoded mapping node that may be
// map[string]any or map[any]any (yaml.v3's shape when ANY key is non-string). Handling
// both means a non-string sibling can't flip the node type and fail-open a
// string-only lookup, hiding a leaf from a security walk. A non-mapping node yields (nil, false).
func yamlMapIndex(node any, key string) (any, bool) {
	switch m := node.(type) {
	case map[string]any:
		v, ok := m[key]
		return v, ok
	case map[any]any:
		v, ok := m[key]
		return v, ok
	default:
		return nil, false
	}
}

// yamlMapEntry is one entry of a YAML-decoded mapping node; key is rendered to a
// string for diagnostics (see yamlMapEntries).
type yamlMapEntry struct {
	key string
	val any
}

// yamlMapEntries returns every entry of a YAML-decoded mapping node as a slice, handling
// both map[string]any and map[any]any (non-string keys rendered with fmt.Sprint,
// diagnostics-only). The yamlMapIndex analog for a walk that ENUMERATES; a slice (not a
// map) is collision-safe when a non-string key renders to a sibling's string.
func yamlMapEntries(node any) []yamlMapEntry {
	switch m := node.(type) {
	case map[string]any:
		out := make([]yamlMapEntry, 0, len(m))
		for k, v := range m {
			out = append(out, yamlMapEntry{key: k, val: v})
		}
		return out
	case map[any]any:
		out := make([]yamlMapEntry, 0, len(m))
		for k, v := range m {
			out = append(out, yamlMapEntry{key: fmt.Sprint(k), val: v})
		}
		return out
	default:
		return nil
	}
}

// rejectCredentialModels walks a models.yml member for literal credentials (CP-4):
// providers.<name>.apiKey, then per owner (provider, model definition, model
// override) header literals and URL-embedded credentials, via rejectCredentialOwner.
// An env reference is a syntactic judgment: the door can't see the container env.
func rejectCredentialModels(mapping map[string]any, joined string) error {
	providers, present := mapping["providers"]
	if !present || providers == nil {
		return nil
	}
	// The SDK schema coerces a list at a record-typed node into an index-keyed
	// record, so a non-mapping node here would skip the walk; refuse it instead.
	entries := yamlMapEntries(providers)
	if entries == nil {
		return fmt.Errorf("%w: models member %q sets providers to a non-mapping (providers must map names to provider configs)", ErrInvalidArgument, joined)
	}
	for _, p := range entries {
		if v, present := yamlMapIndex(p.val, "apiKey"); present && v != nil {
			return fmt.Errorf("%w: models member %q sets providers.%s.apiKey (provider credentials never ride the config bundle)", ErrInvalidArgument, joined, p.key)
		}
		if err := rejectCredentialOwner(p.val, joined, "providers."+p.key); err != nil {
			return err
		}
		models, _ := yamlMapIndex(p.val, "models")
		list, _ := models.([]any)
		for i, m := range list {
			if err := rejectCredentialOwner(m, joined, fmt.Sprintf("providers.%s.models[%d]", p.key, i)); err != nil {
				return err
			}
		}
		overrides, _ := yamlMapIndex(p.val, "modelOverrides")
		overrideEntries := yamlMapEntries(overrides)
		if overrides != nil && overrideEntries == nil {
			return fmt.Errorf("%w: models member %q sets providers.%s.modelOverrides to a non-mapping (overrides must map model ids to overrides)", ErrInvalidArgument, joined, p.key)
		}
		for _, o := range overrideEntries {
			if err := rejectCredentialOwner(o.val, joined, "providers."+p.key+".modelOverrides."+o.key); err != nil {
				return err
			}
		}
	}
	return nil
}

// rejectCredentialOwner runs the per-owner checks shared by a provider, a model
// definition, and a model override.
func rejectCredentialOwner(owner any, joined, path string) error {
	if err := rejectCredentialHeaders(owner, joined, path); err != nil {
		return err
	}
	return rejectCredentialURLs(owner, joined, path)
}

// remoteCompactionURLKeys are the remoteCompaction members the SDK dials as URLs.
var remoteCompactionURLKeys = []string{"endpoint", "v2Endpoint", "streamingEndpoint"}

// rejectCredentialURLs refuses a literal credential embedded in owner's baseUrl or
// remoteCompaction endpoints. URL fields bypass env indirection, so any value is literal.
func rejectCredentialURLs(owner any, joined, path string) error {
	if err := rejectCredentialURL(owner, "baseUrl", joined, path); err != nil {
		return err
	}
	rc, ok := yamlMapIndex(owner, "remoteCompaction")
	if !ok || rc == nil {
		return nil
	}
	if yamlMapEntries(rc) == nil {
		return fmt.Errorf("%w: models member %q sets %s.remoteCompaction to a non-mapping (remoteCompaction must map fields to values)", ErrInvalidArgument, joined, path)
	}
	for _, key := range remoteCompactionURLKeys {
		if err := rejectCredentialURL(rc, key, joined, path+".remoteCompaction"); err != nil {
			return err
		}
	}
	return nil
}

// rejectCredentialURL refuses owner[key] when it is not a string, is not an
// absolute http(s) URL, carries userinfo or a fragment, or has a credential-named query parameter.
func rejectCredentialURL(owner any, key, joined, path string) error {
	v, ok := yamlMapIndex(owner, key)
	if !ok || v == nil {
		return nil
	}
	s, isString := v.(string)
	if !isString {
		return fmt.Errorf("%w: models member %q sets %s.%s to a non-string value", ErrInvalidArgument, joined, path, key)
	}
	if s == "" {
		return nil
	}
	if colon := strings.IndexByte(s, ':'); colon >= 0 {
		scheme, rest := s[:colon], s[colon+1:]
		if strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https") {
			s = strings.ToLower(scheme) + ":" + rest
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("%w: models member %q sets %s.%s to an unparseable URL", ErrInvalidArgument, joined, path, key)
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.Fragment != "" {
		return fmt.Errorf("%w: models member %q sets %s.%s to a URL that is not absolute http(s) without a fragment", ErrInvalidArgument, joined, path, key)
	}
	if u.User != nil {
		return fmt.Errorf("%w: models member %q sets %s.%s with URL userinfo (credentials never ride the config bundle)", ErrInvalidArgument, joined, path, key)
	}
	name, err := credentialQueryParam(u.RawQuery)
	if err != nil {
		return fmt.Errorf("%w: models member %q sets %s.%s to an unparseable URL", ErrInvalidArgument, joined, path, key)
	}
	if name != "" {
		return fmt.Errorf("%w: models member %q sets %s.%s with credential query parameter %q (credentials never ride the config bundle)", ErrInvalidArgument, joined, path, key, name)
	}
	return nil
}

// credentialQueryParam returns the first credential-named parameter in rawQuery.
// It splits on both '&' and ';' and fails on a bad escape: url.Values silently
// drops such pairs, while the SDK sends the raw query unchanged.
func credentialQueryParam(rawQuery string) (string, error) {
	for _, pair := range strings.FieldsFunc(rawQuery, func(r rune) bool { return r == '&' || r == ';' }) {
		rawName, value, _ := strings.Cut(pair, "=")
		name, err := url.QueryUnescape(rawName)
		if err != nil {
			return "", err
		}
		if _, err := url.QueryUnescape(value); err != nil {
			return "", err
		}
		if isCredentialQueryName(name) {
			return name, nil
		}
	}
	return "", nil
}

func credentialMCPQueryParam(rawQuery string) (string, error) {
	for _, pair := range strings.FieldsFunc(rawQuery, func(r rune) bool { return r == '&' || r == ';' }) {
		rawName, value, _ := strings.Cut(pair, "=")
		name, err := url.QueryUnescape(rawName)
		if err != nil {
			return "", err
		}
		if _, err := url.QueryUnescape(value); err != nil {
			return "", err
		}
		if isCredentialQueryName(name) || strings.Contains(strings.ToLower(name), "key") || strings.Contains(strings.ToLower(name), "token") || strings.Contains(strings.ToLower(name), "secret") || strings.Contains(strings.ToLower(name), "auth") {
			return name, nil
		}
	}
	return "", nil
}

// credentialQueryExact and credentialQuerySuffixes judge names after punctuation is removed.
var (
	credentialQueryExact    = map[string]bool{"auth": true, "authorization": true, "sig": true, "pwd": true, "passwd": true}
	credentialQuerySuffixes = []string{"key", "token", "secret", "password", "signature", "credential"}
)

func isCredentialQueryName(name string) bool {
	norm := strings.Map(func(r rune) rune {
		if ('a' <= r && r <= 'z') || ('0' <= r && r <= '9') {
			return r
		}
		return -1
	}, strings.ToLower(name))
	if credentialQueryExact[norm] {
		return true
	}
	for _, suffix := range credentialQuerySuffixes {
		if strings.HasSuffix(norm, suffix) {
			return true
		}
	}
	return false
}

// rejectCredentialHeaders checks the headers node under owner (a provider, model
// definition, or model override, named by path). Each value must be a string env
// reference; the SDK sends a headers list's items under index names, so a
// non-mapping headers node is refused.
func rejectCredentialHeaders(owner any, joined, path string) error {
	headers, ok := yamlMapIndex(owner, "headers")
	if !ok || headers == nil {
		return nil
	}
	entries := yamlMapEntries(headers)
	if entries == nil {
		return fmt.Errorf("%w: models member %q sets %s.headers to a non-mapping (headers must map names to env references)", ErrInvalidArgument, joined, path)
	}
	for _, hdr := range entries {
		s, ok := hdr.val.(string)
		if !ok {
			return fmt.Errorf("%w: models member %q sets %s.headers.%s to a non-string value (pin it to an env reference instead)", ErrInvalidArgument, joined, path, hdr.key)
		}
		if !isEnvIndirection(s) {
			return fmt.Errorf("%w: models member %q sets %s.headers.%s to a literal secret (pin it to an env reference instead)", ErrInvalidArgument, joined, path, hdr.key)
		}
	}
	return nil
}

// isEnvIndirection reports whether a header value is an env reference (or a `!command`)
// rather than a pinned literal secret, mirroring the SDK's resolveConfigValue: a leading
// `!` is a command, a bare env-var-name token is an env reference. A literal secret matches neither.
func isEnvIndirection(value string) bool {
	if strings.HasPrefix(value, "!") {
		return true
	}
	return envIndirectionPattern.MatchString(value)
}
