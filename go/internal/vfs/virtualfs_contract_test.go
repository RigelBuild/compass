package vfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestVirtualFSContract(t *testing.T) {
	implementations := []struct {
		name  string
		newFS func(t *testing.T) VirtualFS
	}{
		{
			name: "fake",
			newFS: func(t *testing.T) VirtualFS {
				t.Helper()
				return &fakeVirtualFS{root: filepath.Join(t.TempDir(), "root")}
			},
		},
		{
			name: "checkout",
			newFS: func(t *testing.T) VirtualFS {
				t.Helper()
				manager := newManager(t)
				volume := mustCreate(t, manager, "contract")
				checkout, err := NewCheckoutFS(manager, volume)
				if err != nil {
					t.Fatalf("NewCheckoutFS: %v", err)
				}
				return checkout
			},
		},
	}

	for _, implementation := range implementations {
		t.Run(implementation.name, func(t *testing.T) {
			runVirtualFSContract(t, implementation.newFS)
		})
	}
}

func runVirtualFSContract(t *testing.T, newFS func(t *testing.T) VirtualFS) {
	t.Helper()
	newSource := func() TreeSource {
		return TreeSource{Repo: "https://example.invalid/repo.git", Ref: "main"}
	}

	t.Run("materialize returns existing destination", func(t *testing.T) {
		fs := newFS(t)
		root, err := fs.Materialize(t.Context(), newSource())
		if err != nil {
			t.Fatalf("Materialize: %v", err)
		}
		info, err := os.Stat(root)
		if err != nil {
			t.Fatalf("stat materialized root: %v", err)
		}
		if !info.IsDir() {
			t.Errorf("materialized root %q is not a directory", root)
		}
	})

	t.Run("release keeps volume contents", func(t *testing.T) {
		fs := newFS(t)
		root, err := fs.Materialize(t.Context(), newSource())
		if err != nil {
			t.Fatalf("Materialize: %v", err)
		}
		file := filepath.Join(root, "kept.txt")
		if err := os.WriteFile(file, []byte("kept"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		if err := fs.Release(t.Context(), root); err != nil {
			t.Fatalf("Release: %v", err)
		}
		got, err := os.ReadFile(file)
		if err != nil || string(got) != "kept" {
			t.Fatalf("file after Release = %q, %v; want kept", got, err)
		}
	})

	t.Run("release rejects a different root", func(t *testing.T) {
		fs := newFS(t)
		root, err := fs.Materialize(t.Context(), newSource())
		if err != nil {
			t.Fatalf("Materialize: %v", err)
		}
		if err := fs.Release(t.Context(), filepath.Join(root, "wrong")); !errors.Is(err, ErrNotMaterialized) {
			t.Fatalf("Release(wrong root) = %v, want ErrNotMaterialized", err)
		}
		if err := fs.Release(t.Context(), root); err != nil {
			t.Fatalf("Release(correct root): %v", err)
		}
	})

	t.Run("materialize can repeat after release", func(t *testing.T) {
		fs := newFS(t)
		first, err := fs.Materialize(t.Context(), newSource())
		if err != nil {
			t.Fatalf("first Materialize: %v", err)
		}
		if _, err := fs.Materialize(t.Context(), newSource()); !errors.Is(err, ErrAlreadyMaterialized) {
			t.Fatalf("second Materialize = %v, want ErrAlreadyMaterialized", err)
		}
		if err := fs.Release(t.Context(), first); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if _, err := fs.Materialize(t.Context(), newSource()); err != nil {
			t.Fatalf("Materialize after Release: %v", err)
		}
	})

	t.Run("rejects invalid sources", func(t *testing.T) {
		for _, test := range []struct {
			name   string
			source TreeSource
		}{
			{name: "empty source"},
			{name: "customer mount with repo", source: TreeSource{Repo: "repo", CustomerMount: t.TempDir()}},
		} {
			t.Run(test.name, func(t *testing.T) {
				fs := newFS(t)
				if _, err := fs.Materialize(t.Context(), test.source); !errors.Is(err, ErrInvalidTreeSource) {
					t.Fatalf("Materialize(%+v) = %v, want ErrInvalidTreeSource", test.source, err)
				}
			})
		}
	})

	t.Run("snapshot-only source with no snapshot fails", func(t *testing.T) {
		if _, err := newFS(t).Materialize(t.Context(), TreeSource{Snapshot: "missing"}); !errors.Is(err, ErrSnapshotNotFound) {
			t.Fatalf("Materialize = %v, want ErrSnapshotNotFound", err)
		}
	})

	t.Run("customer mount does not rebind the destination", func(t *testing.T) {
		contractMountKeepsBinding(t, newFS(t), newSource())
	})
}

func contractMountKeepsBinding(t *testing.T, fs VirtualFS, source TreeSource) {
	t.Helper()
	bound, err := fs.Materialize(t.Context(), source)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if err := fs.Release(t.Context(), bound); err != nil {
		t.Fatalf("Release: %v", err)
	}
	mount := t.TempDir()
	got, err := fs.Materialize(t.Context(), TreeSource{CustomerMount: mount})
	if err != nil || got != mount {
		t.Fatalf("Materialize(mount) = %q, %v; want %q", got, err, mount)
	}
	if err := fs.Release(t.Context(), mount); err != nil {
		t.Fatalf("Release(mount): %v", err)
	}
	again, err := fs.Materialize(t.Context(), source)
	if err != nil || again != bound {
		t.Fatalf("Materialize after mount = %q, %v; want bound root %q", again, err, bound)
	}
}

type fakeVirtualFS struct {
	root   string
	active string
}

func (f *fakeVirtualFS) Materialize(_ context.Context, src TreeSource) (string, error) {
	if src.CustomerMount != "" && (src.Repo != "" || src.Snapshot != "" || len(src.Sparse) != 0) {
		return "", ErrInvalidTreeSource
	}
	if src.CustomerMount == "" && src.Snapshot == "" && src.Repo == "" {
		return "", ErrInvalidTreeSource
	}
	// The fake stores no snapshots, so an empty root with no repo has no cold path.
	if src.Snapshot != "" && src.Repo == "" {
		return "", ErrSnapshotNotFound
	}
	if f.active != "" {
		return "", ErrAlreadyMaterialized
	}
	root := f.root
	if src.CustomerMount != "" {
		root = src.CustomerMount
	} else if err := os.MkdirAll(f.root, 0o700); err != nil {
		return "", err
	}
	f.active = root
	return root, nil
}

func (f *fakeVirtualFS) Release(_ context.Context, root string) error {
	if f.active == "" || root != f.active {
		return ErrNotMaterialized
	}
	f.active = ""
	return nil
}
