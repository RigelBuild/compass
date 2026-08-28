//go:build (pgtest || podman) && unix

package pgshare

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDecideDSNSource(t *testing.T) {
	tests := []struct {
		name         string
		dsn          string
		useContainer string
		cli          string
		requireLive  string
		want         dsnSource
	}{
		{
			name: "dsn set uses shared schema",
			dsn:  "postgres://localhost/db",
			want: sourceSharedSchema,
		},
		{
			name: "no dsn no runtime skips",
			cli:  "",
			want: sourceSkipNoRuntime,
		},
		{
			name: "no dsn runtime present no opt-in fails",
			cli:  "podman",
			want: sourceFailMisconfigured,
		},
		{
			name:         "no dsn runtime present opt-in uses container",
			cli:          "podman",
			useContainer: "1",
			want:         sourceContainer,
		},
		{
			name:        "require-live no dsn no runtime fails",
			cli:         "",
			requireLive: "1",
			want:        sourceFailRequireLive,
		},
		{
			name:        "require-live no dsn runtime present still misconfigured fail",
			cli:         "podman",
			requireLive: "1",
			want:        sourceFailMisconfigured,
		},
		{
			name: "no require-live no dsn no runtime still skips",
			cli:  "",
			want: sourceSkipNoRuntime,
		},
		{
			name:        "dsn set uses shared schema regardless of require-live",
			dsn:         "postgres://localhost/db",
			requireLive: "1",
			want:        sourceSharedSchema,
		},
		{
			name:         "require-live never suppresses the opt-in container path",
			cli:          "podman",
			useContainer: "1",
			requireLive:  "1",
			want:         sourceContainer,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideDSNSource(tt.dsn, tt.useContainer, tt.cli, tt.requireLive); got != tt.want {
				t.Errorf("decideDSNSource(%q, %q, %q, %q) = %d, want %d",
					tt.dsn, tt.useContainer, tt.cli, tt.requireLive, got, tt.want)
			}
		})
	}
}

// TestRemoveContainerArgsCarriesVolumesFlag pins the load-bearing --volumes flag
// in the container-removal argv. Without it a throwaway's anonymous data volume
// (the postgres image's VOLUME /var/lib/postgresql/data) is orphaned on cleanup,
// and enough orphans exhaust podman's num_locks pool so no new container can
// start — a fleet-wide pgtest wedge that presents as a hang. This is the cheap
// guard that runs even without a container runtime; TestThrowawayContainerLeavesNoDanglingVolume
// proves the end-to-end reclaim against a real runtime.
func TestRemoveContainerArgsCarriesVolumesFlag(t *testing.T) {
	got := removeContainerArgs("some-container")
	// Assert the exact argv, not just the presence of --volumes: this also
	// catches an accidentally dropped --force or a reordering, not only a
	// missing volumes flag.
	want := []string{"rm", "--force", "--volumes", "some-container"}
	if !slices.Equal(got, want) {
		t.Fatalf("removeContainerArgs = %v, want %v (must carry --force --volumes so the anonymous data volume is removed with the container)", got, want)
	}
}

// TestThrowawayContainerLeavesNoDanglingVolume is the end-to-end regression: it
// starts a throwaway container the same way the harness does, captures the
// anonymous volume the postgres image creates, runs the exact production removal
// argv, and asserts that specific volume is gone. It correlates by the
// container's OWN volume name, never a global volume count, so it stays
// deterministic while other agents run pgtest concurrently on the same box.
func TestThrowawayContainerLeavesNoDanglingVolume(t *testing.T) {
	cli := containerCLI()
	if cli == "" {
		t.Skip("no podman/docker; skipping real-runtime volume-reclaim test")
	}

	name := "compass-voltest-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	out, err := exec.Command(cli, "run", "-d", "--rm",
		"--name", name,
		"-e", "POSTGRES_PASSWORD=compass-test",
		"-e", "POSTGRES_DB=compass",
		"-P",
		pgImage,
	).CombinedOutput()
	if err != nil {
		t.Skipf("cannot start postgres container (%s): %v\n%s", cli, err, out)
	}
	// Belt-and-suspenders: if an assertion below t.Fatal's before the explicit
	// removal, still reap the container and its volume so the test never leaks
	// the very thing it defends against.
	t.Cleanup(func() { _ = exec.Command(cli, removeContainerArgs(name)...).Run() })

	vol := anonymousVolumeName(t, cli, name)
	if vol == "" {
		t.Fatalf("throwaway container %s has no anonymous volume; the postgres image is expected to declare one", name)
	}
	if !volumeExists(t, cli, vol) {
		t.Fatalf("anonymous volume %s not found while the container is running", vol)
	}

	// The production removal path, verbatim.
	if out, err := exec.Command(cli, removeContainerArgs(name)...).CombinedOutput(); err != nil {
		t.Fatalf("remove container: %v\n%s", err, out)
	}

	if volumeExists(t, cli, vol) {
		t.Fatalf("anonymous volume %s still exists after force-remove; the removal argv must carry --volumes or the pool leaks", vol)
	}
}

// anonymousVolumeName returns the first volume-type mount name of the container,
// or "" if it has none.
func anonymousVolumeName(t *testing.T, cli, name string) string {
	t.Helper()
	out, err := exec.Command(cli, "inspect", name,
		"-f", `{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}}{{"\n"}}{{end}}{{end}}`,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect container mounts: %v\n%s", err, out)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// volumeExists reports whether a volume with the given name is present. It uses
// `volume inspect`, which exists on both podman and docker (unlike podman's
// podman-only `volume exists`), so the end-to-end test stays valid on the docker
// fallback containerCLI advertises. inspect exits 0 when the volume is present;
// when it is absent it exits nonzero AND names a not-found condition on stderr.
// The absence classification keys on that message, not merely on a nonzero exit:
// podman uses 125 as a generic catch-all (daemon hiccup, permission error,
// malformed name), so treating any nonzero exit as "absent" could misread a
// transient failure as "volume gone" and let the post-removal assertion pass
// falsely green. Anything that ran-and-failed without a not-found message, or a
// failure to launch the CLI at all, is a real error and fails the test loudly.
func volumeExists(t *testing.T, cli, vol string) bool {
	t.Helper()
	out, err := exec.Command(cli, "volume", "inspect", vol).CombinedOutput()
	if err == nil {
		return true
	}
	// Both engines name a missing volume in stderr ("no such volume" on podman,
	// "no such volume"/"not found" on docker); match either, case-insensitively,
	// so the classification is a genuine absence and not a catch-all 125.
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		lower := strings.ToLower(string(out))
		if strings.Contains(lower, "no such volume") || strings.Contains(lower, "not found") {
			return false
		}
	}
	t.Fatalf("volume inspect %s: %v\n%s", vol, err, out)
	return false
}

// TestStartSuitePostgresMainErrors covers the StartSuitePostgresMain failure
// paths that must return a non-nil error (never panic, never fail a t) so a
// TestMain caller can clean up and os.Exit(1). These are the pre-exec guards, so
// they need no live Postgres and no container runtime.
func TestStartSuitePostgresMainErrors(t *testing.T) {
	t.Run("empty state dir errors", func(t *testing.T) {
		dsn, stop, err := StartSuitePostgresMain("", t.TempDir(), 5432)
		if err == nil {
			t.Fatal("StartSuitePostgresMain with empty state dir: want error, got nil")
		}
		if stop != nil {
			t.Errorf("want nil stop on error, got non-nil")
		}
		if dsn != "" {
			t.Errorf("want empty dsn on error, got %q", dsn)
		}
	})

	t.Run("binary not on PATH errors", func(t *testing.T) {
		// Shadow PATH with a dir that holds no compass-postgres so LookPath fails
		// deterministically regardless of what is installed on the box.
		t.Setenv("PATH", t.TempDir())
		dsn, stop, err := StartSuitePostgresMain(t.TempDir(), t.TempDir(), 5432)
		if err == nil {
			t.Fatal("StartSuitePostgresMain with no compass-postgres on PATH: want error, got nil")
		}
		if !errors.Is(err, exec.ErrNotFound) {
			t.Errorf("want a not-found error wrapping exec.ErrNotFound, got %v", err)
		}
		if stop != nil {
			t.Errorf("want nil stop on error, got non-nil")
		}
		if dsn != "" {
			t.Errorf("want empty dsn on error, got %q", dsn)
		}
	})
}

// TestStartSuitePostgresMainChildExitShortCircuits proves waitSuiteReady's
// child-exit short-circuit: when the wrapper exits before ever accepting a
// connection, StartSuitePostgresMain must fail fast with the "exited before
// accepting connections" error rather than polling out the full
// suiteReadyBudget. A stub compass-postgres on PATH that exits immediately
// exercises the done-channel path (and the goroutine's happens-before on
// waitErr) without a real Postgres.
func TestStartSuitePostgresMainChildExitShortCircuits(t *testing.T) {
	// A stub `compass-postgres` that exits immediately: the reaper goroutine's
	// cmd.Wait returns, close(done) fires, and waitSuiteReady's `case <-done`
	// must return the exit error before the 30s budget elapses.
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "compass-postgres")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write stub compass-postgres: %v", err)
	}
	t.Setenv("PATH", binDir)

	start := time.Now()
	dsn, stop, err := StartSuitePostgresMain(t.TempDir(), t.TempDir(), 5432)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want error when compass-postgres exits before binding, got nil")
	}
	if !strings.Contains(err.Error(), "exited before accepting connections") {
		t.Errorf("want 'exited before accepting connections' error, got %v", err)
	}
	if stop != nil {
		t.Errorf("want nil stop on error, got non-nil")
	}
	if dsn != "" {
		t.Errorf("want empty dsn on error, got %q", dsn)
	}
	// The short-circuit must fire off the child-exit signal, not the deadline.
	// A bound well under suiteReadyBudget (30s) proves the select returned on
	// `<-done` rather than the timeout, while tolerating poll-tick + reap
	// latency. If the exit short-circuit were dropped, this would take the
	// full ~30s and blow the bound.
	if elapsed >= 10*time.Second {
		t.Errorf("want fast fail via child-exit short-circuit (< 10s), took %s", elapsed)
	}
}
