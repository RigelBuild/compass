package vfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrSourceNotImplemented is returned by the S1 checkout backend for a
// TreeSource selecting a source it does not build yet — a volume snapshot or a
// mounted customer VFS. It is an honest sentinel, not a silent success: a caller
// reaching for an unbuilt tree gets an error it can test with errors.Is, never
// an empty root that looks like a completed checkout.
var ErrSourceNotImplemented = errors.New("vfs: tree source not implemented")

// defaultCommandTimeout is the per-git-command wall-clock cap. A hung git
// (stalled fetch, wedged checkout) must surface as an error, never block the
// provisioning task forever.
const defaultCommandTimeout = 120 * time.Second

// git subcommand + flag tokens, shared across the argv builders so the clone
// and sparse steps stay in lockstep.
const (
	subClone          = "clone"
	subSparseCheckout = "sparse-checkout"
	flagBranch        = "--branch"
	flagDepth         = "--depth"
	flagNoCheckout    = "--no-checkout"
	flagNoTags        = "--no-tags"
)

// GitCheckout is the S1 VirtualFS backend: it materializes a tree by a plain
// git checkout into a subdirectory of its configured root. Sparse-checkout (when
// TreeSource.Sparse is non-empty) is a parameter of this same backend, not a
// second one. It is the only place in the package a subprocess is spawned.
type GitCheckout struct {
	// root is the instance's binding destination: every Materialize creates a
	// subdirectory under it. Constructor argument, never a Materialize
	// parameter, so a later phase can swap the destination behind the frozen
	// signature.
	root string
	// program is the git binary to invoke (default "git" on PATH).
	program string
	// timeout is the per-command wall-clock cap.
	timeout time.Duration
}

// NewGitCheckout builds a GitCheckout materializing into root, invoking `git` on
// PATH with the default per-command timeout.
func NewGitCheckout(root string) *GitCheckout {
	return &GitCheckout{root: root, program: "git", timeout: defaultCommandTimeout}
}

// WithProgram uses an explicit git binary (e.g. an absolute path). Defaults to
// `git` on PATH.
func (g *GitCheckout) WithProgram(program string) *GitCheckout {
	g.program = program
	return g
}

// WithTimeout overrides the per-command timeout.
func (g *GitCheckout) WithTimeout(timeout time.Duration) *GitCheckout {
	g.timeout = timeout
	return g
}

// Materialize checks out src.Repo at src.Ref into a fresh subdirectory of the
// instance root and returns that subdirectory. A src selecting a snapshot or a
// customer mount returns ErrSourceNotImplemented. When src.Sparse is non-empty
// the checkout is restricted to those paths via git sparse-checkout.
func (g *GitCheckout) Materialize(ctx context.Context, src TreeSource) (string, error) {
	if src.Snapshot != "" || src.CustomerMount != "" {
		return "", fmt.Errorf("%w: snapshot/customer-mount sources arrive in a later phase", ErrSourceNotImplemented)
	}
	if src.Repo == "" || src.Ref == "" {
		return "", fmt.Errorf("vfs: checkout requires both Repo and Ref, got Repo=%q Ref=%q", src.Repo, src.Ref)
	}

	dest := filepath.Join(g.root, destName(src))
	// 0o755 so a confined agent (a distinct uid under a userns remap) can
	// traverse into the materialized tree; mirrors the config-root mode pin.
	if err := os.MkdirAll(g.root, 0o755); err != nil { //nolint:gosec // G301: the materialized tree is mounted into the container; it must be 0755 for the confined agent to traverse it
		return "", fmt.Errorf("vfs: creating materialization root: %w", err)
	}
	// A stale destination from a prior aborted checkout would make git clone
	// refuse; start clean so Materialize is idempotent.
	if err := os.RemoveAll(dest); err != nil {
		return "", fmt.Errorf("vfs: clearing destination %q: %w", dest, err)
	}

	sparse := len(src.Sparse) > 0
	if err := g.run(ctx, "git clone", cloneArgs(src, dest, sparse)); err != nil {
		return "", err
	}
	if sparse {
		if err := g.run(ctx, "git sparse-checkout set", sparseSetArgs(dest, src.Sparse)); err != nil {
			return "", err
		}
		if err := g.run(ctx, "git checkout", checkoutArgs(dest, src.Ref)); err != nil {
			return "", err
		}
	}
	return dest, nil
}

// Release removes a tree previously returned by Materialize. Releasing a path
// outside the instance root is refused — a backend never deletes a tree it did
// not materialize.
func (g *GitCheckout) Release(_ context.Context, root string) error {
	rel, err := filepath.Rel(g.root, root)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("vfs: refusing to release %q outside root %q", root, g.root)
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("vfs: releasing %q: %w", root, err)
	}
	return nil
}

// destName derives a stable, filesystem-safe subdirectory name for a source
// from its repo and ref, so two distinct trees never collide under one root.
func destName(src TreeSource) string {
	sum := sha256.Sum256([]byte(src.Repo + "\x00" + src.Ref))
	return "tree-" + hex.EncodeToString(sum[:8])
}

// cloneArgs assembles the argv for `git clone`. Split out so the argv assembly
// is unit-testable without spawning git, mirroring internal/runtime's
// createArgs. --no-tags keeps the checkout minimal; --depth 1 makes it a shallow
// checkout of just the requested ref. For a full checkout the ref is given via
// --branch so clone lands directly on it; for a sparse checkout --no-checkout
// defers populating the working tree until sparse-checkout has narrowed it.
func cloneArgs(src TreeSource, dest string, sparse bool) []string {
	args := []string{subClone, flagNoTags, flagDepth, "1"}
	if sparse {
		args = append(args, flagNoCheckout)
	} else {
		args = append(args, flagBranch, src.Ref)
	}
	return append(args, src.Repo, dest)
}

// sparseSetArgs assembles the argv for `git sparse-checkout set` in dest,
// restricting the working tree to paths. --no-cone takes the paths as literal
// patterns rather than cone-mode directory prefixes. Split out so the argv is
// unit-testable without spawning git.
func sparseSetArgs(dest string, paths []string) []string {
	args := make([]string, 0, 5+len(paths))
	args = append(args, "-C", dest, subSparseCheckout, "set", "--no-cone")
	return append(args, paths...)
}

// checkoutArgs assembles the argv for `git checkout <ref>` in dest, used after a
// --no-checkout clone to populate the sparse working tree at the requested ref.
// Split out so the argv is unit-testable without spawning git.
func checkoutArgs(dest, ref string) []string {
	return []string{"-C", dest, "checkout", ref}
}

// run runs `git <args>` under the command timeout, folding a non-zero exit (with
// git's stderr) into an error. The single subprocess seam of the backend.
func (g *GitCheckout) run(ctx context.Context, summary string, args []string) error {
	cctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	//nolint:gosec // G204: this is the git-checkout seam — spawning the
	// operator-set git binary with Runner-assembled argv is the backend's
	// entire purpose; Repo/Ref/Sparse originate from a validated provision
	// request, not attacker-controlled free text.
	cmd := exec.CommandContext(cctx, g.program, args...)
	out, err := cmd.CombinedOutput()
	if cctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("vfs: %s timed out after %s", summary, g.timeout)
	}
	if err != nil {
		return fmt.Errorf("vfs: %s failed: %w: %s", summary, err, strings.TrimSpace(string(out)))
	}
	return nil
}
