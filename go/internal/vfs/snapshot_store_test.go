package vfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	for _, test := range []struct {
		name   string
		cloner cloner
	}{
		{name: "copy", cloner: copyCloner{}},
		{name: "reflink", cloner: reflinkCloner{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := snapshotTestBase(t, test.name)
			m := mustManager(t, filepath.Join(base, "volumes"))
			m.cloner = test.cloner
			source := mustCreate(t, m, "source")
			fixture := filepath.Join(source.HostRoot, "nested", "deeper")
			if err := os.MkdirAll(fixture, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(source.HostRoot, "empty"), 0o710); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(fixture, "run")
			if err := os.WriteFile(file, []byte("executable tree"), 0o751); err != nil {
				t.Fatal(err)
			}
			stamp := time.Date(2022, time.April, 3, 4, 5, 6, 0, time.UTC)
			if err := os.Chtimes(file, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(fixture, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			linkTarget := filepath.Join(base, "outside-file")
			if err := os.WriteFile(linkTarget, []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(source.HostRoot, "outside-link")
			if err := os.Symlink(linkTarget, link); err != nil {
				t.Fatal(err)
			}
			snapshot, err := m.Snapshot(t.Context(), source)
			if err != nil {
				t.Fatalf("Snapshot: %v", err)
			}
			first := mustCreate(t, m, "restore-one")
			if err := m.RestoreSnapshot(t.Context(), snapshot, first); err != nil {
				t.Fatalf("RestoreSnapshot: %v", err)
			}
			assertTreeEqual(t, source.HostRoot, first.HostRoot)
			info, err := os.Lstat(filepath.Join(first.HostRoot, "outside-link"))
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("restored outside entry is not a symlink: info=%v err=%v", info, err)
			}
			gotTarget, err := os.Readlink(filepath.Join(first.HostRoot, "outside-link"))
			if err != nil || gotTarget != linkTarget {
				t.Fatalf("restored link target = %q, %v; want %q", gotTarget, err, linkTarget)
			}
			if entries, err := os.ReadDir(first.HostRoot); err != nil || len(entries) != 3 {
				t.Fatalf("restored root entries = %d, %v; want three entries", len(entries), err)
			}
		})
	}
}

func TestRestoreDoesNotMutateSnapshot(t *testing.T) {
	m := newManager(t)
	source := mustCreate(t, m, "source")
	if err := os.WriteFile(filepath.Join(source.HostRoot, "data"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := m.Snapshot(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	first := mustCreate(t, m, "first")
	if err := m.RestoreSnapshot(t.Context(), id, first); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first.HostRoot, "data"), []byte("mutated"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := mustCreate(t, m, "second")
	if err := m.RestoreSnapshot(t.Context(), id, second); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(second.HostRoot, "data"))
	if err != nil || string(got) != "original" {
		t.Fatalf("second restore = %q, %v; want original", got, err)
	}
}

func TestSnapshotIndexIsAccountScoped(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "source")
	id, err := m.Snapshot(t.Context(), v)
	if err != nil {
		t.Fatal(err)
	}
	key := SnapshotKey{AgentAccountID: "acctA", Repo: "repo"}
	if err := m.PromoteSnapshot(t.Context(), key, id); err != nil {
		t.Fatal(err)
	}
	if got, err := m.CurrentSnapshot(t.Context(), key); err != nil || got != id {
		t.Fatalf("CurrentSnapshot = (%q, %v), want %q", got, err, id)
	}
	for _, missing := range []SnapshotKey{{AgentAccountID: "acctB", Repo: "repo"}, {AgentAccountID: "acctA", Repo: "unseen"}} {
		if _, err := m.CurrentSnapshot(t.Context(), missing); !errors.Is(err, ErrSnapshotNotFound) {
			t.Errorf("CurrentSnapshot(%+v) = %v, want ErrSnapshotNotFound", missing, err)
		}
	}
}

func TestPromoteSnapshotSupersedesUnreferencedTree(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "source")
	id1, err := m.Snapshot(t.Context(), v)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := m.Snapshot(t.Context(), v)
	if err != nil {
		t.Fatal(err)
	}
	key := SnapshotKey{AgentAccountID: "acct", Repo: "repo"}
	if err := m.PromoteSnapshot(t.Context(), key, id1); err != nil {
		t.Fatal(err)
	}
	if err := m.PromoteSnapshot(t.Context(), key, id2); err != nil {
		t.Fatal(err)
	}
	if got, err := m.CurrentSnapshot(t.Context(), key); err != nil || got != id2 {
		t.Fatalf("CurrentSnapshot = (%q, %v), want %q", got, err, id2)
	}
	if exists(t, m.snapshotPath(id1)) {
		t.Fatal("superseded snapshot tree remains")
	}
	entries, err := os.ReadDir(filepath.Join(m.snapshotStoreDir(), snapshotIndexDir))
	if err != nil || len(entries) != 1 {
		t.Fatalf("snapshot index entries = %d, %v; want one", len(entries), err)
	}

	id3, err := m.Snapshot(t.Context(), v)
	if err != nil {
		t.Fatal(err)
	}
	other := SnapshotKey{AgentAccountID: "other", Repo: "repo"}
	if err := m.PromoteSnapshot(t.Context(), other, id2); err != nil {
		t.Fatal(err)
	}
	if err := m.PromoteSnapshot(t.Context(), key, id3); err != nil {
		t.Fatal(err)
	}
	if !exists(t, m.snapshotPath(id2)) {
		t.Fatal("tree referenced by another key was removed")
	}
}

func TestRestoreSnapshotRejectsNonEmptyAndRecoversMarkedRoot(t *testing.T) {
	m := newManager(t)
	source := mustCreate(t, m, "source")
	if err := os.WriteFile(filepath.Join(source.HostRoot, "source"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := m.Snapshot(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	destination := mustCreate(t, m, "destination")
	junk := filepath.Join(destination.HostRoot, "junk")
	if err := os.WriteFile(junk, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.RestoreSnapshot(t.Context(), id, destination); !errors.Is(err, ErrVolumeNotEmpty) {
		t.Fatalf("RestoreSnapshot on non-empty root = %v, want ErrVolumeNotEmpty", err)
	}
	if got, err := os.ReadFile(junk); err != nil || string(got) != "keep" {
		t.Fatalf("non-empty root changed: %q, %v", got, err)
	}
	marker := filepath.Join(metaDir(destination.HostRoot), restoreMarkerName)
	if err := os.WriteFile(marker, []byte("incomplete"), stampFileMode); err != nil {
		t.Fatal(err)
	}
	if err := m.RestoreSnapshot(t.Context(), id, destination); err != nil {
		t.Fatalf("RestoreSnapshot with marker: %v", err)
	}
	if exists(t, junk) || !exists(t, filepath.Join(destination.HostRoot, "source")) || exists(t, marker) {
		t.Fatal("marked restore did not clear prior contents and marker")
	}
}

func TestCloneRegularRejectsSymlinkToOutsideRoot(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "source")
	dst := filepath.Join(base, "destination")
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(base, "outside-secret")
	if err := os.WriteFile(secret, []byte("outside secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(src, "race")); err != nil {
		t.Fatal(err)
	}
	srcRoot, err := os.OpenRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer srcRoot.Close()
	dstRoot, err := os.OpenRoot(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer dstRoot.Close()
	if err := cloneRegular(t.Context(), srcRoot, dstRoot, "race", 0o600, time.Now(), false); err == nil {
		t.Fatal("cloneRegular copied a symlink target outside the source root")
	}
	if exists(t, filepath.Join(dst, "race")) {
		t.Fatal("outside file content was copied into destination")
	}
}

func TestSnapshotRefreshesTreeRootMtimeForExpiry(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "source")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(v.HostRoot, old, old); err != nil {
		t.Fatal(err)
	}
	id, err := m.Snapshot(t.Context(), v)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := m.Expire(t.Context(), time.Hour); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if !exists(t, m.snapshotPath(id)) {
		t.Fatal("unpromoted snapshot was swept immediately after capture")
	}
}

func TestSnapshotRejectsSpecialFileAndCleansStaging(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "source")
	if err := syscall.Mkfifo(filepath.Join(v.HostRoot, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snapshot(t.Context(), v); err == nil || !strings.Contains(err.Error(), "pipe") {
		t.Fatalf("Snapshot error = %v, want unsupported path pipe", err)
	}
	for _, dir := range []string{filepath.Join(m.snapshotStoreDir(), snapshotStagingDir), filepath.Join(m.snapshotStoreDir(), snapshotTreesDir)} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("snapshot dir %q has %d entries after failed copy: %v", dir, len(entries), err)
		}
	}
}

func TestSnapshotKeyValidation(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "source")
	id, err := m.Snapshot(t.Context(), v)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []SnapshotKey{{Repo: "repo"}, {AgentAccountID: "acct"}} {
		if err := m.PromoteSnapshot(t.Context(), key, id); !errors.Is(err, ErrInvalidSnapshotKey) {
			t.Errorf("PromoteSnapshot(%+v) = %v, want ErrInvalidSnapshotKey", key, err)
		}
		if _, err := m.CurrentSnapshot(t.Context(), key); !errors.Is(err, ErrInvalidSnapshotKey) {
			t.Errorf("CurrentSnapshot(%+v) = %v, want ErrInvalidSnapshotKey", key, err)
		}
	}
	for _, id := range []VolumeSnapshotID{"", ".", "..", "bad/id", "bad\x00id"} {
		if err := m.PromoteSnapshot(t.Context(), SnapshotKey{AgentAccountID: "acct", Repo: "repo"}, id); !errors.Is(err, ErrSnapshotNotFound) {
			t.Errorf("PromoteSnapshot with invalid id %q = %v, want ErrSnapshotNotFound", id, err)
		}
	}
}

func TestExpireSweepsSnapshotsByAgeAndKeepsReferences(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "source")
	idOld, err := m.Snapshot(t.Context(), v)
	if err != nil {
		t.Fatal(err)
	}
	idFresh, err := m.Snapshot(t.Context(), v)
	if err != nil {
		t.Fatal(err)
	}
	idReferenced, err := m.Snapshot(t.Context(), v)
	if err != nil {
		t.Fatal(err)
	}
	key := SnapshotKey{AgentAccountID: "acct", Repo: "repo"}
	if err := m.PromoteSnapshot(t.Context(), key, idReferenced); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, id := range []VolumeSnapshotID{idOld, idReferenced} {
		if err := os.Chtimes(m.snapshotPath(id), old, old); err != nil {
			t.Fatal(err)
		}
	}
	stagingOld := filepath.Join(m.snapshotStoreDir(), snapshotStagingDir, "old")
	if err := os.Mkdir(stagingOld, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stagingOld, old, old); err != nil {
		t.Fatal(err)
	}
	if err := m.Expire(t.Context(), 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if exists(t, m.snapshotPath(idOld)) || exists(t, stagingOld) {
		t.Fatal("old unreferenced tree or staging leftover survived Expire")
	}
	if !exists(t, m.snapshotPath(idFresh)) || !exists(t, m.snapshotPath(idReferenced)) {
		t.Fatal("Expire removed a fresh or referenced snapshot")
	}
}

func TestExpireAndReconcileIgnoreSnapshotStore(t *testing.T) {
	m := newManager(t)
	store := m.snapshotStoreDir()
	marker := filepath.Join(store, "sentinel")
	if err := os.WriteFile(marker, []byte("store"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Expire(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if err := m.ReconcileOrphans(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !exists(t, marker) {
		t.Fatal("volume maintenance touched the snapshot store")
	}
}

func TestSnapshotIndexEntryShape(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "source")
	id, err := m.Snapshot(t.Context(), v)
	if err != nil {
		t.Fatal(err)
	}
	key := SnapshotKey{AgentAccountID: "acct", Repo: "repo"}
	if err := m.PromoteSnapshot(t.Context(), key, id); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(key.AgentAccountID + "\x00" + key.Repo))
	path := filepath.Join(m.snapshotStoreDir(), snapshotIndexDir, hex.EncodeToString(sum[:])+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"agentAccountId": key.AgentAccountID, "repo": key.Repo, "snapshotId": string(id)} {
		if got[name] != want {
			t.Errorf("index %s = %v, want %q", name, got[name], want)
		}
	}
	if _, ok := got["promotedAt"]; !ok {
		t.Fatal("index entry lacks promotedAt")
	}
}

func TestSnapshotClonesSymlinkWithoutFollowing(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "source")
	dst := filepath.Join(base, "destination")
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "secret")
	if err := os.WriteFile(outside, []byte("host secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := (copyCloner{}).cloneTree(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Join(dst, "link")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("clone followed symlink: info=%v err=%v", info, err)
	}
	got, err := os.Readlink(filepath.Join(dst, "link"))
	if err != nil || got != outside {
		t.Fatalf("link target = %q, %v; want %q", got, err, outside)
	}
}

func snapshotTestBase(t *testing.T, name string) string {
	t.Helper()
	if name != "reflink" {
		return t.TempDir()
	}
	parent := os.Getenv("COMPASS_VFS_REFLINK_DIR")
	if parent == "" {
		t.Skip("COMPASS_VFS_REFLINK_DIR is unset")
	}
	base, err := os.MkdirTemp(parent, "vfs-roundtrip-") //nolint:usetesting // t.TempDir cannot choose the reflink-capable filesystem.
	if err != nil {
		t.Fatalf("creating reflink fixture: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Errorf("removing reflink fixture: %v", err)
		}
	})
	return base
}

func mustManager(t *testing.T, base string) *LocalManager {
	t.Helper()
	m, err := NewLocalManager(base)
	if err != nil {
		t.Fatalf("NewLocalManager: %v", err)
	}
	return m
}

func assertTreeEqual(t *testing.T, want, got string) {
	t.Helper()
	if err := filepath.WalkDir(want, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return compareTreeEntry(want, got, path)
	}); err != nil {
		t.Fatalf("comparing trees: %v", err)
	}
	if err := filepath.WalkDir(got, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return rejectUnexpectedTreeEntry(want, got, path)
	}); err != nil {
		t.Fatalf("checking restored tree entries: %v", err)
	}
}

func compareTreeEntry(want, got, path string) error {
	rel, err := filepath.Rel(want, path)
	if err != nil {
		return err
	}
	other := filepath.Join(got, rel)
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	otherInfo, err := os.Lstat(other)
	if err != nil {
		return err
	}
	if !sameTreeMetadata(info, otherInfo, rel == ".") {
		return fmt.Errorf("metadata mismatch for %q: %v versus %v", rel, info, otherInfo)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return compareTreeSymlink(path, other, rel)
	}
	if info.Mode().IsRegular() {
		return compareTreeFile(path, other, rel)
	}
	return nil
}

func sameTreeMetadata(want, got fs.FileInfo, root bool) bool {
	if root {
		return want.Mode().Perm() == got.Mode().Perm()
	}
	return want.Mode().Type() == got.Mode().Type() && want.Mode().Perm() == got.Mode().Perm() && want.ModTime().Equal(got.ModTime())
}

func compareTreeSymlink(want, got, rel string) error {
	a, err := os.Readlink(want)
	if err != nil {
		return err
	}
	b, err := os.Readlink(got)
	if err != nil {
		return err
	}
	if a != b {
		return fmt.Errorf("symlink target mismatch for %q: %q versus %q", rel, a, b)
	}
	return nil
}

func compareTreeFile(want, got, rel string) error {
	a, err := os.ReadFile(want)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(got)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("content mismatch for %q", rel)
	}
	return nil
}

func rejectUnexpectedTreeEntry(want, got, path string) error {
	rel, err := filepath.Rel(got, path)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	if _, err := os.Lstat(filepath.Join(want, rel)); err != nil {
		return fmt.Errorf("unexpected restored entry %q: %w", rel, err)
	}
	return nil
}
