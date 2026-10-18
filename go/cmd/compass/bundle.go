//go:build unix

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// Bundle grammar mirrors the store door's member whitelist, filename rules,
// name grammar, and content shapes.
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

// Top-level regular-file members admitted by exact filename, not under a top dir
// (store door: memberAgentsMD, memberModels). Any other bare top-level file is
// rejected. settingsMember is the ONLY file admitted under settings/ (yml-only).
const (
	memberAgentsMD = "AGENTS.md"
	memberModels   = "models.yml"
	settingsMember = "settings/config.yml"
	// memberSystemMD is the only filename admitted under prompts/<role>/
	// (store door: RIG-3075 T2).
	memberSystemMD = "SYSTEM.md"
	// memberProfileYML is the only filename admitted under profiles/<name>/
	// (store door: RIG-2968 T1).
	memberProfileYML = "profile.yml"
)

// maxBundleFileCount and maxBundleContentBytes match the store door's default
// layer caps. The server also bounds total decompressed tar bytes, so the
// content-byte check is only an early lower-cost rejection.
const (
	maxBundleFileCount          = 2560
	maxBundleContentBytes int64 = 48 << 20
)

// bundleTopDirs is the whitelisted set of top-level directories a member may
// live under (store door: configBundleTopDirs).
var bundleTopDirs = map[string]bool{
	topDirSkills:     true,
	topDirExtensions: true,
	topDirMCP:        true,
	topDirSettings:   true,
	topDirRules:      true,
	topDirAgents:     true,
	topDirPrompts:    true,
	topDirProfiles:   true,
}

// bundleNamePattern is the grammar for a member's <name> segment (store door:
// configNamePattern).
var bundleNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// buildBundle tars validated regular files and rejects symlinks and other
// non-regular files before sending them to the store door.
func buildBundle(dir string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	var fileCount int
	var contentBytes int64
	root := filepath.Clean(dir)
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)

		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("bundle member %q is a symlink; the store door rejects symlink members", name)
		}
		if d.IsDir() {
			return validateDirMember(name)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("bundle member %q is not a regular file; only regular files and directories are allowed", name)
		}
		n, err := addRegularMember(tw, root, name)
		if err != nil {
			return err
		}
		fileCount++
		contentBytes += n
		if err := checkBundleCaps(fileCount, contentBytes); err != nil {
			return err
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if fileCount == 0 {
		return nil, fmt.Errorf("bundle directory %q contains no members under skills/, extensions/, mcp/, settings/, rules/, agents/, prompts/, or profiles/ and no top-level %s or %s; use `agent-config delete` to clear the fleet config", dir, memberAgentsMD, memberModels)
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("finalizing tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("finalizing gzip: %w", err)
	}
	return buf.Bytes(), nil
}

// checkBundleCaps enforces the client-side file-count and content-byte caps
// that mirror the store door (maxBundleFileCount, maxBundleContentBytes),
// naming the exceeded cap. Extracted so a test can drive the cap boundary
// directly rather than materialize a door-sized bundle on disk.
func checkBundleCaps(fileCount int, contentBytes int64) error {
	if fileCount > maxBundleFileCount {
		return fmt.Errorf("bundle exceeds the %d-file limit", maxBundleFileCount)
	}
	if contentBytes > maxBundleContentBytes {
		return fmt.Errorf("bundle exceeds the %d-byte content limit", maxBundleContentBytes)
	}
	return nil
}

// validateDirMember checks the top-dir whitelist and name grammar. Checking
// directories reports invalid paths early; leaf names are checked separately.
func validateDirMember(name string) error {
	parts := strings.Split(name, "/")
	if !bundleTopDirs[parts[0]] {
		return errNotWhitelisted(name)
	}
	if (parts[0] == topDirSkills || parts[0] == topDirExtensions || parts[0] == topDirPrompts || parts[0] == topDirProfiles) && len(parts) >= 2 && !bundleNamePattern.MatchString(parts[1]) {
		return fmt.Errorf("bundle member name %q must match %s", parts[1], bundleNamePattern.String())
	}
	return nil
}

// errNotWhitelisted names the full admitted member set for a member that is
// neither under a whitelisted top dir nor one of the two top-level singletons
// (store door: configMemberParts).
func errNotWhitelisted(name string) error {
	return fmt.Errorf("bundle member %q is not under skills/, extensions/, mcp/, settings/, rules/, agents/, prompts/, or profiles/ and is not a top-level %s or %s", name, memberAgentsMD, memberModels)
}

// addRegularMember validates member grammar and content shape, writes the tar
// header and contents, and returns the content size. Credential checks remain
// the store door's security boundary.
func addRegularMember(tw *tar.Writer, root, name string) (int64, error) {
	parts := strings.Split(name, "/")

	// A single-component member is a bare top-level file: admit only the two
	// exact filenames (store door: configMemberParts len==1 branch).
	if len(parts) == 1 {
		if parts[0] != memberAgentsMD && parts[0] != memberModels {
			return 0, errNotWhitelisted(name)
		}
	} else if !bundleTopDirs[parts[0]] {
		return 0, errNotWhitelisted(name)
	}

	content, err := readMember(root, name)
	if err != nil {
		return 0, err
	}

	if err := validateMemberGrammar(parts, name, content); err != nil {
		return 0, err
	}

	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return 0, fmt.Errorf("writing tar header for %q: %w", name, err)
	}
	if _, err := tw.Write(content); err != nil {
		return 0, fmt.Errorf("writing tar content for %q: %w", name, err)
	}
	return int64(len(content)), nil
}

// validateMemberGrammar mirrors the store door's per-category content shape.
// Credential rejection stays at the door; duplicating its denylist here would
// create a bypassable copy that can drift.
func validateMemberGrammar(parts []string, name string, content []byte) error {
	// Top-level singletons (parts already restricted to the two exact names).
	if len(parts) == 1 {
		switch parts[0] {
		case memberAgentsMD:
			return nil // prose, no content check
		case memberModels:
			return validateYAMLMapping(name, content)
		}
	}

	switch parts[0] {
	case topDirMCP:
		if len(parts) != 2 {
			return fmt.Errorf("mcp member %q must be mcp/<name>.json", name)
		}
		base, ok := strings.CutSuffix(parts[1], ".json")
		if !ok {
			return fmt.Errorf("mcp member %q must be a .json file", name)
		}
		if !bundleNamePattern.MatchString(base) {
			return fmt.Errorf("mcp member name %q must match %s", base, bundleNamePattern.String())
		}
		if !json.Valid(content) {
			return fmt.Errorf("mcp member %q is not valid JSON", name)
		}
		return nil
	case topDirSettings:
		if name != settingsMember {
			return fmt.Errorf("settings member %q must be exactly %s", name, settingsMember)
		}
		return validateYAMLMapping(name, content)
	case topDirRules:
		if len(parts) != 2 {
			return fmt.Errorf("rules member %q must be flat rules/<name>.md or rules/<name>.mdc", name)
		}
		return validateFlatNamedMember(topDirRules, parts[1], name, ".md", ".mdc")
	case topDirAgents:
		if len(parts) != 2 {
			return fmt.Errorf("agents member %q must be flat agents/<name>.md", name)
		}
		return validateFlatNamedMember(topDirAgents, parts[1], name, ".md")
	case topDirPrompts:
		// Exactly prompts/<role>/SYSTEM.md — three components, grammar-valid
		// <role>, filename exactly SYSTEM.md (store door: RIG-3075 T2).
		if len(parts) != 3 || parts[2] != memberSystemMD {
			return fmt.Errorf("prompts member %q must be prompts/<role>/%s", name, memberSystemMD)
		}
		if !bundleNamePattern.MatchString(parts[1]) {
			return fmt.Errorf("prompts role name %q must match %s", parts[1], bundleNamePattern.String())
		}
		return nil
	case topDirProfiles:
		// Only profiles/<name>/profile.yml is admitted, with a YAML mapping body.
		// Cross-member and superset-key checks remain the store door's authority.
		if len(parts) != 3 || parts[2] != memberProfileYML {
			return fmt.Errorf("profiles member %q must be profiles/<name>/%s", name, memberProfileYML)
		}
		if !bundleNamePattern.MatchString(parts[1]) {
			return fmt.Errorf("profiles name %q must match %s", parts[1], bundleNamePattern.String())
		}
		return validateYAMLMapping(name, content)
	default:
		// skills/ or extensions/: the <name> second component must match.
		if len(parts) < 3 {
			return fmt.Errorf("bundle member %q must be a file under %s/<name>/", name, parts[0])
		}
		if !bundleNamePattern.MatchString(parts[1]) {
			return fmt.Errorf("bundle member name %q must match %s", parts[1], bundleNamePattern.String())
		}
		return nil
	}
}

// validateFlatNamedMember enforces dir/<name><ext>, with a valid name and an
// allowed extension.
func validateFlatNamedMember(topDir, filename, name string, exts ...string) error {
	for _, ext := range exts {
		if base, ok := strings.CutSuffix(filename, ext); ok {
			if !bundleNamePattern.MatchString(base) {
				return fmt.Errorf("%s member name %q must match %s", topDir, base, bundleNamePattern.String())
			}
			return nil
		}
	}
	return fmt.Errorf("%s member %q must end in one of %s", topDir, name, strings.Join(exts, ", "))
}

// validateYAMLMapping checks settings and models with the store door's parser.
// Empty documents are accepted as empty mappings; credential checks remain there.
func validateYAMLMapping(name string, content []byte) error {
	var doc any
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return fmt.Errorf("bundle member %q is not valid YAML: %w", name, err)
	}
	if doc == nil {
		return nil // empty document is an empty mapping
	}
	if _, ok := doc.(map[string]any); !ok {
		return fmt.Errorf("bundle member %q must be a YAML mapping", name)
	}
	return nil
}

// readMember reads a path made from WalkDir's clean relative name under root.
func readMember(root, name string) ([]byte, error) {
	full := filepath.Join(root, filepath.FromSlash(name))
	f, err := os.Open(full) //nolint:gosec // full is under the operator-provided --dir root, the whole point of push
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only member; close error is not actionable
	content, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("reading bundle member %q: %w", name, err)
	}
	return content, nil
}

// bundleMemberNames re-reads a built bundle and returns its regular-file member
// header names in sorted order — the test seam that lets a builder test assert
// the tar's contents without a live server.
func bundleMemberNames(bundle []byte) ([]string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return nil, fmt.Errorf("opening gzip: %w", err)
	}
	defer func() { _ = gz.Close() }() // read-only decompress; close error is not actionable
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg {
			names = append(names, path.Clean(hdr.Name))
		}
	}
	sort.Strings(names)
	return names, nil
}
