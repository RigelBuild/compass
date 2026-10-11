package vfs

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestProbeSelectsCopyOnTmpfs(t *testing.T) {
	const tmpfsMagic = 0x01021994
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skip("/dev/shm is unavailable")
	}
	var stat unix.Statfs_t
	if err := unix.Statfs("/dev/shm", &stat); err != nil {
		t.Skipf("cannot inspect /dev/shm: %v", err)
	}
	if uint64(stat.Type) != tmpfsMagic {
		t.Skipf("/dev/shm filesystem type %#x is not tmpfs", stat.Type)
	}
	base, err := os.MkdirTemp("/dev/shm", "compass-vfs-probe-") //nolint:usetesting // t.TempDir cannot choose the filesystem.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Errorf("removing tmpfs fixture: %v", err)
		}
	})
	clone, err := probeCloner(filepath.Join(base, storeDirName))
	if err != nil {
		t.Fatalf("probeCloner on tmpfs: %v", err)
	}
	if got := clone.name(); got != "copy" {
		t.Fatalf("probe selected %q on tmpfs, want copy", got)
	}
}

func TestProbeSelectsReflinkOnACapableFS(t *testing.T) {
	parent := os.Getenv("COMPASS_VFS_REFLINK_DIR")
	if parent == "" {
		t.Skip("COMPASS_VFS_REFLINK_DIR is unset")
	}
	base, err := os.MkdirTemp(parent, "vfs-reflink-") //nolint:usetesting // t.TempDir cannot choose the filesystem.
	if err != nil {
		t.Fatalf("creating test base under reflink directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Errorf("removing reflink fixture: %v", err)
		}
	})
	m, err := NewLocalManager(filepath.Join(base, "volumes"))
	if err != nil {
		t.Fatalf("NewLocalManager on reflink filesystem: %v", err)
	}
	if got := m.cloner.name(); got != "reflink" {
		t.Fatalf("probe selected %q, want reflink", got)
	}
	v := mustCreate(t, m, "reflink-source")
	content := []byte("reflink-backed snapshot")
	if err := os.WriteFile(filepath.Join(v.HostRoot, "file"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := m.Snapshot(t.Context(), v)
	if err != nil {
		t.Fatalf("Snapshot with probed reflink cloner: %v", err)
	}
	restored := mustCreate(t, m, "reflink-restored")
	if err := m.RestoreSnapshot(t.Context(), id, restored); err != nil {
		t.Fatalf("RestoreSnapshot with probed reflink cloner: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(restored.HostRoot, "file"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("restored content = %q, want %q", got, content)
	}
}

func TestSnapshotClonersRoundTrip(t *testing.T) {
	for _, test := range []struct {
		name   string
		cloner cloner
	}{
		{name: "copy", cloner: copyCloner{}},
		{name: "reflink", cloner: reflinkCloner{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			src, dst := clonerTestDirs(t, test.name)
			file := filepath.Join(src, "file")
			want := []byte("cloner output")
			if err := os.WriteFile(file, want, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(file, 0o751); err != nil {
				t.Fatal(err)
			}
			if err := test.cloner.cloneTree(t.Context(), src, dst); err != nil {
				t.Fatalf("cloneTree: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(dst, "file"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("cloned bytes = %q, want %q", got, want)
			}
			info, err := os.Stat(filepath.Join(dst, "file"))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o751 {
				t.Fatalf("cloned mode = %04o, want 0751", info.Mode().Perm())
			}
		})
	}
}

func clonerTestDirs(t *testing.T, name string) (string, string) {
	t.Helper()
	base := t.TempDir()
	if name == "reflink" {
		parent := os.Getenv("COMPASS_VFS_REFLINK_DIR")
		if parent == "" {
			t.Skip("COMPASS_VFS_REFLINK_DIR is unset")
		}
		var err error
		base, err = os.MkdirTemp(parent, "vfs-cloner-") //nolint:usetesting // t.TempDir cannot choose the filesystem.
		if err != nil {
			t.Fatalf("creating reflink test base: %v", err)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(base); err != nil {
				t.Errorf("removing reflink cloner fixture: %v", err)
			}
		})
	}
	src := filepath.Join(base, "source")
	dst := filepath.Join(base, "destination")
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	return src, dst
}
