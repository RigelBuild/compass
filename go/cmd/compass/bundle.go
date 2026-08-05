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
)

// Bundle grammar — mirrors the store door (internal/store/agent_config.go) so a
// bundle this builder produces passes validateAndHashConfigBundle. Kept in sync
// with the door constants at that file: the top-dir whitelist, the <name>
// grammar, and the mcp/<name>.json shape.
const (
	topDirSkills     = "skills"
	topDirExtensions = "extensions"
	topDirMCP        = "mcp"
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
}

// bundleNamePattern is the grammar for a member's <name> segment (store door:
// configNamePattern).
var bundleNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// buildBundle walks dir — the operator-pointed bundle root whose immediate
// children are skills/, extensions/, mcp/ — and tars+gzips its regular files
// into a bundle the store door accepts. It validates each member client-side
// (top-dir whitelist, <name> grammar, mcp/<name>.json JSON) so a malformed dir
// is rejected here with a clear message rather than as an opaque server
// InvalidArgument. Symlinks, absolute paths, and .. never enter the tar: a
// symlink under dir is a hard error (the door rejects TypeSymlink outright).
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
		return nil, fmt.Errorf("bundle directory %q contains no skills/, extensions/, or mcp/ members; use `agent-config delete` to clear the fleet config", dir)
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
// validating them surfaces a bad top dir or name at the offending path.
func validateDirMember(name string) error {
	parts := strings.Split(name, "/")
	if !bundleTopDirs[parts[0]] {
		return fmt.Errorf("bundle member %q is not under skills/, extensions/, or mcp/", name)
	}
	if parts[0] != topDirMCP && len(parts) >= 2 && !bundleNamePattern.MatchString(parts[1]) {
		return fmt.Errorf("bundle member name %q must match %s", parts[1], bundleNamePattern.String())
	}
	return nil
}

// addRegularMember validates a regular file against the door grammar and writes
// it to the tar with a clean relative header name, returning the member's
// content byte count so the caller can enforce the cumulative size cap. mcp/
// members must be exactly mcp/<name>.json with a grammar-valid <name> and valid
// JSON content; skills/ and extensions/ members must carry a grammar-valid
// <name> second component.
func addRegularMember(tw *tar.Writer, root, name string) (int64, error) {
	parts := strings.Split(name, "/")
	if !bundleTopDirs[parts[0]] {
		return 0, fmt.Errorf("bundle member %q is not under skills/, extensions/, or mcp/", name)
	}

	content, err := readMember(root, name)
	if err != nil {
		return 0, err
	}

	switch parts[0] {
	case topDirMCP:
		if len(parts) != 2 {
			return 0, fmt.Errorf("mcp member %q must be mcp/<name>.json", name)
		}
		base, ok := strings.CutSuffix(parts[1], ".json")
		if !ok {
			return 0, fmt.Errorf("mcp member %q must be a .json file", name)
		}
		if !bundleNamePattern.MatchString(base) {
			return 0, fmt.Errorf("mcp member name %q must match %s", base, bundleNamePattern.String())
		}
		if !json.Valid(content) {
			return 0, fmt.Errorf("mcp member %q is not valid JSON", name)
		}
	default:
		if len(parts) < 2 {
			return 0, fmt.Errorf("bundle member %q must live under %s/<name>", name, parts[0])
		}
		if !bundleNamePattern.MatchString(parts[1]) {
			return 0, fmt.Errorf("bundle member name %q must match %s", parts[1], bundleNamePattern.String())
		}
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
