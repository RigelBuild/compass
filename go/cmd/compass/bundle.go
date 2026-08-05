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

// Bundle grammar — mirrors the store door (internal/store/agent_config.go) so a
// bundle this builder produces passes validateAndHashConfigBundle. Kept in sync
// with the door constants at that file: the top-dir whitelist, the two admitted
// top-level singleton filenames, the <name> grammar, and the per-category
// content shape (mcp/<name>.json JSON, settings/config.yml and models.yml YAML
// mapping).
const (
	topDirSkills     = "skills"
	topDirExtensions = "extensions"
	topDirMCP        = "mcp"
	topDirSettings   = "settings"
	topDirRules      = "rules"
	topDirAgents     = "agents"
)

// Top-level regular-file members admitted by exact filename, not under a top dir
// (store door: memberAgentsMD, memberModels). Any other bare top-level file is
// rejected. settingsMember is the ONLY file admitted under settings/ (yml-only).
const (
	memberAgentsMD = "AGENTS.md"
	memberModels   = "models.yml"
	settingsMember = "settings/config.yml"
)

// maxBundleFileCount and maxBundleContentBytes are a fail-fast client-side check
// so a too-large bundle is rejected here with a named cap rather than as an
// opaque server InvalidArgument. The file-count cap equals the store door's
// maxFileCount (internal/store/agent_config.go). The byte cap is an APPROXIMATE
// lower-bound on the door's maxDecompressedBytes: the client sums member file
// content only, while the door bounds the whole decompressed tar stream
// (content + 512-byte headers + block padding + trailer), so a bundle just
// under this cap can still be rejected at the door. The door is authoritative;
// this check only shortens the common failure path.
const (
	maxBundleFileCount          = 4096
	maxBundleContentBytes int64 = 64 << 20 // 64 MiB
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
}

// bundleNamePattern is the grammar for a member's <name> segment (store door:
// configNamePattern).
var bundleNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// buildBundle walks dir — the operator-pointed bundle root whose immediate
// children are the whitelisted top dirs (skills/, extensions/, mcp/, settings/,
// rules/, agents/) plus the two admitted top-level files (AGENTS.md,
// models.yml) — and tars+gzips its regular files into a bundle the store door
// accepts. It validates each member client-side (top-dir whitelist, <name>
// grammar, mcp/<name>.json JSON, settings/config.yml and models.yml YAML
// mapping) so a malformed dir is rejected here with a clear message rather than
// as an opaque server InvalidArgument. Symlinks, absolute paths, and .. never
// enter the tar: a symlink under dir is a hard error (the door rejects
// TypeSymlink outright).
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
		return nil, fmt.Errorf("bundle directory %q contains no members under skills/, extensions/, mcp/, settings/, rules/, or agents/ and no top-level %s or %s; use `agent-config delete` to clear the fleet config", dir, memberAgentsMD, memberModels)
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

// validateDirMember checks a directory member's top dir is whitelisted and, for
// skills/extensions, that its <name> segment matches the grammar. Directories
// are not written as tar members (the door skips non-regular members), but
// validating them surfaces a bad top dir at the offending path. rules/, agents/,
// and settings/ hold flat files, not <name> subdirs, so their leaves carry the
// grammar check in addRegularMember; the container dir only needs a whitelisted
// top dir.
func validateDirMember(name string) error {
	parts := strings.Split(name, "/")
	if !bundleTopDirs[parts[0]] {
		return errNotWhitelisted(name)
	}
	if (parts[0] == topDirSkills || parts[0] == topDirExtensions) && len(parts) >= 2 && !bundleNamePattern.MatchString(parts[1]) {
		return fmt.Errorf("bundle member name %q must match %s", parts[1], bundleNamePattern.String())
	}
	return nil
}

// errNotWhitelisted names the full admitted member set for a member that is
// neither under a whitelisted top dir nor one of the two top-level singletons
// (store door: configMemberParts).
func errNotWhitelisted(name string) error {
	return fmt.Errorf("bundle member %q is not under skills/, extensions/, mcp/, settings/, rules/, or agents/ and is not a top-level %s or %s", name, memberAgentsMD, memberModels)
}

// addRegularMember validates a regular file against the door grammar and writes
// it to the tar with a clean relative header name, returning the member's
// content byte count so the caller can enforce the cumulative size cap. It
// mirrors the store door's per-category grammar (validateRegularMember):
//
//   - mcp/<name>.json — grammar-valid <name>, content parses as JSON.
//   - skills/<name>/… , extensions/<name>/… — grammar-valid <name>.
//   - settings/config.yml — the ONLY settings/ member; content parses as a YAML
//     mapping (the twin of the mcp JSON check; NO credential denylist — that is
//     the door's authoritative security boundary, not the client's).
//   - rules/<name>.md|.mdc — flat, grammar-valid <name>; prose, no content check.
//   - agents/<name>.md — flat, grammar-valid <name>; prose, no content check.
//   - top-level AGENTS.md — prose, no content check.
//   - top-level models.yml — content parses as a YAML mapping (same cheap check
//     as settings; NO credential denylist).
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

// validateMemberGrammar enforces the per-category content shape a member must
// satisfy, mirroring the store door (validateRegularMember). It does NOT port
// the door's credential denylist: the door owns credential rejection as its
// authoritative security boundary, and duplicating a bypassable denylist here
// only invites stale-denylist drift.
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
	default:
		// skills/ or extensions/: the <name> second component must match.
		if len(parts) < 2 {
			return fmt.Errorf("bundle member %q must live under %s/<name>", name, parts[0])
		}
		if !bundleNamePattern.MatchString(parts[1]) {
			return fmt.Errorf("bundle member name %q must match %s", parts[1], bundleNamePattern.String())
		}
		return nil
	}
}

// validateFlatNamedMember enforces a flat dir/<name><ext> member: <name> matches
// the config name grammar and <ext> is one of the allowed extensions (store
// door: validateFlatNamedMember).
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

// validateYAMLMapping is the cheap YAML shape check — the twin of the mcp
// json.Valid check — for settings/config.yml and models.yml. It uses the SAME
// YAML package as the store door (go.yaml.in/yaml/v3) and mirrors the door's
// parseYAMLMapping: unmarshal into any, treat an empty document (nil) as an
// empty mapping, and require the result to be a mapping. It deliberately does
// NOT replicate the door's credential denylist.
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

// readMember reads a member file's content from disk. root+name is rejoined
// with path cleanliness already guaranteed by WalkDir producing a relative path
// under root.
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
