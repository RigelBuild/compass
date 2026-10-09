package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// agentRebuildHint is the CI seed recipe plus a re-tag: load names the image
// docker.io/library/…, but the bare ref resolves an older localhost/ copy first.
const agentRebuildHint = `rebuild it from this tree:
  src=$(bun tools/toolchain/devenv-cli/index.ts --lock agent-image/devenv.lock --mode flakeref)
  ( cd agent-image && nix run "$src" -- container copy agent -r "docker-archive:$TMPDIR/compass-agent-seed.tar:" )
  podman load -i "$TMPDIR/compass-agent-seed.tar"
  podman tag docker.io/library/compass-agent:latest localhost/compass-agent:latest`

// agentSourceFingerprint recomputes agent-image/source-fingerprint.nix over
// pkgDir: sha256 of the sorted `<sha256>  <relpath>\n` lines of every file,
// minus the top-level node_modules and moon.yml. A file symlink is hashed by its
// target content, as lib.fileset does; anything else non-regular is an error.
func agentSourceFingerprint(pkgDir string) (string, error) {
	type entry struct{ rel, sum string }
	var entries []entry
	err := filepath.WalkDir(pkgDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(pkgDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case rel == "node_modules" && d.IsDir():
			return filepath.SkipDir
		case rel == "node_modules", rel == "moon.yml", d.IsDir():
			return nil
		case d.Type()&fs.ModeSymlink != 0:
			info, err := os.Stat(path)
			if err != nil {
				return fmt.Errorf("resolve symlink %s: %w", rel, err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("symlink %s does not resolve to a regular file", rel)
			}
		case !d.Type().IsRegular():
			return fmt.Errorf("unsupported file type at %s", rel)
		}
		sum, err := fileSHA256(path)
		if err != nil {
			return err
		}
		entries = append(entries, entry{rel: rel, sum: sum})
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk %s: %w", pkgDir, err)
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("no source files under %s", pkgDir)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s  %s\n", e.sum, e.rel)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:]), nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a file walked under the repo package dir, not user input
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// errStaleAgentImage marks a local image built from different agent source.
var errStaleAgentImage = errors.New("compass-agent:latest is stale")

// checkAgentImageFresh compares the image's stamped fingerprint with the tree.
// An empty stamp (an image built before stamping) counts as stale.
func checkAgentImageFresh(stamped, tree string) error {
	stamped = strings.TrimSpace(stamped)
	if stamped == tree {
		return nil
	}
	if stamped == "" {
		stamped = "(none)"
	}
	return fmt.Errorf("%w: image source fingerprint %s, tree %s; every tool-call leg would fail against it, so %s",
		errStaleAgentImage, stamped, tree, agentRebuildHint)
}

// agentImageGate fails when the image was built from different agent source
// than pkgDir: a stale image fails every tool-call leg with symptoms that point
// at the code. CI's pull branch sets the opt-out, because it tests published
// :latest on purpose and that may lag the tree; stamp is not read then.
func agentImageGate(getenv func(string) string, pkgDir string, stamp func() string) error {
	if getenv("COMPASS_E2E_ALLOW_PUBLISHED_AGENT_IMAGE") == "1" {
		return nil
	}
	tree, err := agentSourceFingerprint(pkgDir)
	if err != nil {
		return fmt.Errorf("fingerprint agent source: %w", err)
	}
	return checkAgentImageFresh(stamp(), tree)
}
