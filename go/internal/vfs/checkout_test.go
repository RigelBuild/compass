package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNewCheckoutFS(t *testing.T) {
	for _, test := range []struct {
		name     string
		hostRoot func(t *testing.T) string
	}{
		{name: "empty root"},
		{name: "missing root", hostRoot: func(t *testing.T) string {
			t.Helper()
			return filepath.Join(t.TempDir(), "missing")
		}},
		{name: "file root", hostRoot: func(t *testing.T) string {
			t.Helper()
			path := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var hostRoot string
			if test.hostRoot != nil {
				hostRoot = test.hostRoot(t)
			}
			if _, err := NewCheckoutFS(newManager(t), Volume{HostRoot: hostRoot}); err == nil {
				t.Fatalf("NewCheckoutFS(%q) succeeded, want error", hostRoot)
			}
		})
	}

	t.Run("existing directory", func(t *testing.T) {
		root := t.TempDir()
		checkout, err := NewCheckoutFS(newManager(t), Volume{SessionID: "valid", HostRoot: root})
		if err != nil {
			t.Fatalf("NewCheckoutFS: %v", err)
		}
		if got := checkout.Expected(); got.Repo != "" || got.Ref != "" || got.Sparse != nil || got.Snapshot != "" || got.CustomerMount != "" {
			t.Errorf("Expected() before materialize = %+v, want zero value", got)
		}
	})
}

func TestCheckoutFSMaterializeRestoresSnapshot(t *testing.T) {
	manager := newManager(t)
	sourceVolume := mustCreate(t, manager, "source")
	want := filepath.Join(sourceVolume.HostRoot, "nested", "file.txt")
	if err := os.MkdirAll(filepath.Dir(want), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("snapshot contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := manager.Snapshot(t.Context(), sourceVolume)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := manager.PromoteSnapshot(t.Context(), SnapshotKey{AgentAccountID: "account", Repo: "repo"}, id); err != nil {
		t.Fatalf("PromoteSnapshot: %v", err)
	}
	target := mustCreate(t, manager, "target")
	checkout, err := NewCheckoutFS(manager, target)
	if err != nil {
		t.Fatalf("NewCheckoutFS: %v", err)
	}
	source := TreeSource{Repo: "repo", Ref: "main", Sparse: []string{"nested"}, Snapshot: id}
	root, err := checkout.Materialize(t.Context(), source)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if root != target.HostRoot {
		t.Errorf("Materialize root = %q, want %q", root, target.HostRoot)
	}
	got, err := os.ReadFile(filepath.Join(root, "nested", "file.txt"))
	if err != nil || string(got) != "snapshot contents" {
		t.Fatalf("restored file = %q, %v; want snapshot contents", got, err)
	}
	if expected := checkout.Expected(); expected.Repo != source.Repo || expected.Ref != source.Ref || !reflect.DeepEqual(expected.Sparse, source.Sparse) {
		t.Errorf("Expected() = %+v, want Repo/Ref/Sparse from source", expected)
	}
	gotExpected := checkout.Expected()
	gotExpected.Sparse[0] = "mutated"
	if expected := checkout.Expected(); !reflect.DeepEqual(expected.Sparse, source.Sparse) {
		t.Errorf("Expected() exposed mutable sparse paths: %+v", expected.Sparse)
	}
}

// A reattached warm volume keeps its tree even when a snapshot is offered.
func TestCheckoutFSSnapshotKeepsWarmVolume(t *testing.T) {
	manager := newManager(t)
	source := mustCreate(t, manager, "source")
	if err := os.WriteFile(filepath.Join(source.HostRoot, "snap.txt"), []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := manager.Snapshot(t.Context(), source)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	target := mustCreate(t, manager, "target")
	warm := filepath.Join(target.HostRoot, "warm.txt")
	if err := os.WriteFile(warm, []byte("warm"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkout, err := NewCheckoutFS(manager, target)
	if err != nil {
		t.Fatalf("NewCheckoutFS: %v", err)
	}
	if _, err := checkout.Materialize(t.Context(), TreeSource{Repo: "repo", Ref: "main", Snapshot: id}); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if data, err := os.ReadFile(warm); err != nil || string(data) != "warm" {
		t.Fatalf("warm file = %q, %v; want kept", data, err)
	}
	if _, err := os.Stat(filepath.Join(target.HostRoot, "snap.txt")); !os.IsNotExist(err) {
		t.Fatalf("snapshot file restored over a warm volume: %v", err)
	}
}

// An unusable snapshot falls back to the cold path without touching existing volume contents.
func TestCheckoutFSUnknownSnapshotFallsBackWithoutClearing(t *testing.T) {
	for _, test := range []struct {
		name       string
		snapshotID func(t *testing.T, manager *LocalManager) VolumeSnapshotID
	}{
		{name: "unknown", snapshotID: func(t *testing.T, _ *LocalManager) VolumeSnapshotID {
			t.Helper()
			return "unknown"
		}},
		{name: "superseded", snapshotID: func(t *testing.T, manager *LocalManager) VolumeSnapshotID {
			t.Helper()
			source := mustCreate(t, manager, "source")
			first, err := manager.Snapshot(t.Context(), source)
			if err != nil {
				t.Fatal(err)
			}
			key := SnapshotKey{AgentAccountID: "account", Repo: "repo"}
			if err := manager.PromoteSnapshot(t.Context(), key, first); err != nil {
				t.Fatal(err)
			}
			second, err := manager.Snapshot(t.Context(), source)
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.PromoteSnapshot(t.Context(), key, second); err != nil {
				t.Fatal(err)
			}
			return first
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := newManager(t)
			snapshotID := test.snapshotID(t, manager)
			target := mustCreate(t, manager, "target")
			stale := filepath.Join(target.HostRoot, "stale.txt")
			if err := os.WriteFile(stale, []byte("old tree"), 0o600); err != nil {
				t.Fatal(err)
			}
			checkout, err := NewCheckoutFS(manager, target)
			if err != nil {
				t.Fatal(err)
			}
			root, err := checkout.Materialize(t.Context(), TreeSource{Repo: "repo", Ref: "main", Snapshot: snapshotID})
			if err != nil {
				t.Fatalf("Materialize: %v", err)
			}
			if root != target.HostRoot {
				t.Fatalf("root = %q, want %q", root, target.HostRoot)
			}
			if data, err := os.ReadFile(stale); err != nil || string(data) != "old tree" {
				t.Fatalf("existing volume file = %q, %v; want kept", data, err)
			}
		})
	}
}

func TestCheckoutFSReleaseRequiresMaterialization(t *testing.T) {
	checkout := newCheckoutForTest(t)
	if err := checkout.Release(t.Context(), checkout.root); !errors.Is(err, ErrNotMaterialized) {
		t.Fatalf("Release before Materialize = %v, want ErrNotMaterialized", err)
	}
	root, err := checkout.Materialize(t.Context(), TreeSource{Repo: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkout.Release(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	if err := checkout.Release(t.Context(), root); !errors.Is(err, ErrNotMaterialized) {
		t.Fatalf("second Release = %v, want ErrNotMaterialized", err)
	}
}

func TestCheckoutFSCustomerMountPassesThrough(t *testing.T) {
	manager := newManager(t)
	volume := mustCreate(t, manager, "target")
	mount := filepath.Join(t.TempDir(), "customer")
	if err := os.Mkdir(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(mount, "kept.txt")
	if err := os.WriteFile(file, []byte("customer data"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkout, err := NewCheckoutFS(manager, volume)
	if err != nil {
		t.Fatal(err)
	}
	root, err := checkout.Materialize(t.Context(), TreeSource{CustomerMount: mount})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if root != mount {
		t.Errorf("Materialize root = %q, want unchanged mount %q", root, mount)
	}
	if entries, err := os.ReadDir(volume.HostRoot); err != nil || len(entries) != 0 {
		t.Errorf("destination volume entries = %v, %v; want untouched empty root", entries, err)
	}
	if err := checkout.Release(t.Context(), mount); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "customer data" {
		t.Errorf("customer file after Release = %q, %v", got, err)
	}
}

// A snapshot with no repo has no cold path: the agent could not complete an empty root.
func TestCheckoutFSSnapshotOnlyMissingSnapshotFails(t *testing.T) {
	checkout := newCheckoutForTest(t)
	if _, err := checkout.Materialize(t.Context(), TreeSource{Snapshot: "unknown"}); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("Materialize = %v, want ErrSnapshotNotFound", err)
	}
}

func TestCheckoutFSRejectsInvalidCustomerMounts(t *testing.T) {
	unclean := filepath.Join(t.TempDir(), "mount") + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "mount"
	symlinked := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), symlinked); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		path string
	}{
		{name: "relative", path: "relative"},
		{name: "unclean", path: unclean},
		{name: "missing", path: filepath.Join(t.TempDir(), "missing")},
		{name: "symlink", path: symlinked},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkout := newCheckoutForTest(t)
			if _, err := checkout.Materialize(t.Context(), TreeSource{CustomerMount: test.path}); !errors.Is(err, ErrInvalidTreeSource) {
				t.Fatalf("Materialize(%q) = %v, want ErrInvalidTreeSource", test.path, err)
			}
		})
	}

	for _, test := range []struct {
		name   string
		source TreeSource
	}{
		{name: "repo with customer mount", source: TreeSource{Repo: "repo", CustomerMount: t.TempDir()}},
		{name: "snapshot with customer mount", source: TreeSource{Snapshot: "snapshot", CustomerMount: t.TempDir()}},
		{name: "sparse with customer mount", source: TreeSource{Sparse: []string{"path"}, CustomerMount: t.TempDir()}},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkout := newCheckoutForTest(t)
			if _, err := checkout.Materialize(t.Context(), test.source); !errors.Is(err, ErrInvalidTreeSource) {
				t.Errorf("Materialize(%+v) = %v, want ErrInvalidTreeSource", test.source, err)
			}
		})
	}
}

func TestCheckoutFSKeepsWarmVolume(t *testing.T) {
	manager := newManager(t)
	volume := mustCreate(t, manager, "warm")
	file := filepath.Join(volume.HostRoot, "existing.txt")
	if err := os.WriteFile(file, []byte("warm tree"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkout, err := NewCheckoutFS(manager, volume)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkout.Materialize(t.Context(), TreeSource{Repo: "repo", Ref: "main"}); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "warm tree" {
		t.Fatalf("warm file = %q, %v; want warm tree", got, err)
	}
}

func TestCheckoutFSRejectsInvalidSparsePaths(t *testing.T) {
	for _, path := range []string{"../outside", "dir/../outside", "/absolute"} {
		t.Run(path, func(t *testing.T) {
			checkout := newCheckoutForTest(t)
			if _, err := checkout.Materialize(t.Context(), TreeSource{Repo: "repo", Sparse: []string{path}}); !errors.Is(err, ErrInvalidTreeSource) {
				t.Fatalf("Materialize sparse %q = %v, want ErrInvalidTreeSource", path, err)
			}
		})
	}
}

func TestCheckoutFSReleaseKeepsRestoredVolume(t *testing.T) {
	manager := newManager(t)
	source := mustCreate(t, manager, "source")
	if err := os.WriteFile(filepath.Join(source.HostRoot, "restored.txt"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := manager.Snapshot(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	target := mustCreate(t, manager, "target")
	checkout, err := NewCheckoutFS(manager, target)
	if err != nil {
		t.Fatal(err)
	}
	root, err := checkout.Materialize(t.Context(), TreeSource{Repo: "repo", Snapshot: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkout.Release(t.Context(), root); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "restored.txt")); err != nil || string(got) != "kept" {
		t.Fatalf("restored file after Release = %q, %v; want kept", got, err)
	}
}

func newCheckoutForTest(t *testing.T) *CheckoutFS {
	t.Helper()
	manager := newManager(t)
	volume := mustCreate(t, manager, "checkout")
	checkout, err := NewCheckoutFS(manager, volume)
	if err != nil {
		t.Fatal(err)
	}
	return checkout
}
