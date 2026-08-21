package vfs

// The VirtualFS contract suite: a shared table-driven contract every backend
// must satisfy, run against both a hermetic in-memory fake and the real
// GitCheckout backend (driven against a temp git repo over a file:// remote, no
// network), plus the pure argv-builder tests that pin exactly what the backend
// shells out to git.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// fakeVFS is a hermetic in-memory VirtualFS: it materializes a tree by creating
// a directory and dropping a marker file, and honors the same
// ErrSourceNotImplemented contract as the real backend. It exists so the
// contract test proves the contract itself, independent of git.
type fakeVFS struct {
	root string
	mu   sync.Mutex
	live map[string]bool
}

func newFakeVFS(root string) *fakeVFS {
	return &fakeVFS{root: root, live: map[string]bool{}}
}

func (f *fakeVFS) Materialize(_ context.Context, src TreeSource) (string, error) {
	if src.Snapshot != "" || src.CustomerMount != "" {
		return "", ErrSourceNotImplemented
	}
	if src.Repo == "" || src.Ref == "" {
		return "", errors.New("fakeVFS: Repo and Ref required")
	}
	dest := filepath.Join(f.root, destName(src))
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dest, markerFile), []byte("fake"), 0o644); err != nil {
		return "", err
	}
	f.mu.Lock()
	f.live[dest] = true
	f.mu.Unlock()
	return dest, nil
}

func (f *fakeVFS) Release(_ context.Context, root string) error {
	f.mu.Lock()
	delete(f.live, root)
	f.mu.Unlock()
	return os.RemoveAll(root)
}

// markerFile is the file the contract test asserts is present in a materialized
// tree. The real backend's temp repo commits it; the fake writes it.
const markerFile = "README.md"

// gitAvailable reports whether a usable git binary is on PATH, so the real-
// backend contract test skips cleanly on a host without git rather than failing.
func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping real-backend contract test")
	}
}

// initTempRepo creates a local git repo in a temp dir with a single committed
// file (markerFile) on branch main, and returns a file:// URL usable as a
// TreeSource.Repo. Hermetic: no network, no shared global git state.
func initTempRepo(t *testing.T) (repoURL, ref string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// Deterministic identity + main branch so the checkout is reproducible
		// regardless of the host's global git config.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, markerFile), []byte("hello"), 0o644); err != nil {
		t.Fatalf("writing repo file: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "initial")
	return "file://" + dir, "main"
}

// TestVirtualFSContract runs the same behavioral contract against both a fake
// and the real GitCheckout backend: a full checkout materializes a tree whose
// content is present at the returned root, Release removes it, and a
// snapshot/customer-mount source returns the not-implemented sentinel.
func TestVirtualFSContract(t *testing.T) {
	backends := []struct {
		name string
		// newFS returns a VirtualFS rooted at a fresh temp dir plus a TreeSource
		// that materializes successfully for this backend.
		newFS func(t *testing.T) (VirtualFS, TreeSource)
	}{
		{
			name: "fake",
			newFS: func(t *testing.T) (VirtualFS, TreeSource) {
				t.Helper()
				return newFakeVFS(t.TempDir()), TreeSource{Repo: "any", Ref: "main"}
			},
		},
		{
			name: "git-checkout",
			newFS: func(t *testing.T) (VirtualFS, TreeSource) {
				t.Helper()
				gitAvailable(t)
				repo, ref := initTempRepo(t)
				return NewGitCheckout(t.TempDir()), TreeSource{Repo: repo, Ref: ref}
			},
		},
	}

	for _, be := range backends {
		t.Run(be.name, func(t *testing.T) {
			ctx := context.Background()

			t.Run("materialize then release", func(t *testing.T) {
				fs, src := be.newFS(t)
				root, err := fs.Materialize(ctx, src)
				if err != nil {
					t.Fatalf("Materialize: %v", err)
				}
				marker := filepath.Join(root, markerFile)
				if _, err := os.Stat(marker); err != nil {
					t.Fatalf("materialized tree missing %q: %v", markerFile, err)
				}
				if err := fs.Release(ctx, root); err != nil {
					t.Fatalf("Release: %v", err)
				}
				if _, err := os.Stat(root); !os.IsNotExist(err) {
					t.Fatalf("Release left %q behind (stat err=%v)", root, err)
				}
			})

			t.Run("snapshot source is not implemented", func(t *testing.T) {
				fs, _ := be.newFS(t)
				if _, err := fs.Materialize(ctx, TreeSource{Snapshot: "snap-1"}); !errors.Is(err, ErrSourceNotImplemented) {
					t.Fatalf("Materialize(snapshot) err = %v, want ErrSourceNotImplemented", err)
				}
			})

			t.Run("customer mount source is not implemented", func(t *testing.T) {
				fs, _ := be.newFS(t)
				if _, err := fs.Materialize(ctx, TreeSource{CustomerMount: "/mnt/customer"}); !errors.Is(err, ErrSourceNotImplemented) {
					t.Fatalf("Materialize(customerMount) err = %v, want ErrSourceNotImplemented", err)
				}
			})
		})
	}
}

// TestGitCheckoutSparseMaterialize drives the sparse-checkout parameter of the
// real backend end to end: a repo with two files, a sparse source selecting only
// one, must land that file and omit the other. Proves sparse is a parameter of
// the one backend, not a no-op.
func TestGitCheckoutSparseMaterialize(t *testing.T) {
	gitAvailable(t)
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-b", "main")
	for _, sub := range []string{"keep", "drop"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
		if err := os.WriteFile(filepath.Join(dir, sub, "f.txt"), []byte(sub), 0o644); err != nil {
			t.Fatalf("write %s: %v", sub, err)
		}
	}
	run("add", ".")
	run("commit", "-m", "initial")

	fs := NewGitCheckout(t.TempDir())
	root, err := fs.Materialize(context.Background(), TreeSource{
		Repo:   "file://" + dir,
		Ref:    "main",
		Sparse: []string{"/keep/"},
	})
	if err != nil {
		t.Fatalf("Materialize(sparse): %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "keep", "f.txt")); err != nil {
		t.Fatalf("sparse checkout dropped included path keep/f.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "drop", "f.txt")); !os.IsNotExist(err) {
		t.Fatalf("sparse checkout kept excluded path drop/f.txt (stat err=%v)", err)
	}
}

func TestCloneArgs(t *testing.T) {
	tests := []struct {
		name   string
		src    TreeSource
		sparse bool
		want   []string
	}{
		{
			// A full checkout lands directly on the ref via --branch and lets
			// git populate the working tree.
			name: "full checkout branches to ref",
			src:  TreeSource{Repo: "file:///r", Ref: "main"},
			want: []string{"clone", "--no-tags", "--depth", "1", "--branch", "main", "file:///r", "/dst"},
		},
		{
			// A sparse checkout defers the working-tree populate (--no-checkout)
			// so sparse-checkout can narrow it before files land, and never
			// passes --branch (the ref is checked out after narrowing).
			name:   "sparse checkout defers populate",
			src:    TreeSource{Repo: "file:///r", Ref: "main", Sparse: []string{"/a"}},
			sparse: true,
			want:   []string{"clone", "--no-tags", "--depth", "1", "--no-checkout", "file:///r", "/dst"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cloneArgs(tc.src, "/dst", tc.sparse)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("cloneArgs = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSparseSetArgs(t *testing.T) {
	got := sparseSetArgs("/dst", []string{"/a/", "/b/"})
	want := []string{"-C", "/dst", "sparse-checkout", "set", "--no-cone", "/a/", "/b/"}
	if !slices.Equal(got, want) {
		t.Fatalf("sparseSetArgs = %q, want %q", got, want)
	}
}

func TestCheckoutArgs(t *testing.T) {
	got := checkoutArgs("/dst", "main")
	want := []string{"-C", "/dst", "checkout", "main"}
	if !slices.Equal(got, want) {
		t.Fatalf("checkoutArgs = %q, want %q", got, want)
	}
}

// TestReleaseRefusesOutsideRoot pins the safety invariant: a backend never
// deletes a tree it did not materialize, so Release rejects a path escaping its
// root and leaves that path intact.
func TestReleaseRefusesOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatalf("seeding victim file: %v", err)
	}
	fs := NewGitCheckout(root)
	if err := fs.Release(context.Background(), outside); err == nil {
		t.Fatalf("Release(%q) outside root %q = nil, want refusal", outside, root)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("Release deleted a path outside root: %v", err)
	}
}
