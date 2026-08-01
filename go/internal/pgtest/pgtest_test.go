//go:build pgtest

package pgtest

import (
	"errors"
	"os"
	"os/exec"
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideDSNSource(tt.dsn, tt.useContainer, tt.cli); got != tt.want {
				t.Errorf("decideDSNSource(%q, %q, %q) = %d, want %d",
					tt.dsn, tt.useContainer, tt.cli, got, tt.want)
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
	args := removeContainerArgs("some-container")
	var hasVolumes bool
	for _, a := range args {
		if a == "--volumes" || a == "-v" {
			hasVolumes = true
			break
		}
	}
	if !hasVolumes {
		t.Fatalf("removeContainerArgs = %v, want it to carry --volumes so the anonymous data volume is removed with the container", args)
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
// fallback containerCLI advertises. inspect exits 0 when the volume is present
// and nonzero when it is absent — podman returns 125 for a missing volume, not
// 1, so the check keys on ran-and-exited-nonzero (an *exec.ExitError), never a
// specific code. A failure to launch the CLI at all (not an ExitError) is a real
// error and fails the test.
func volumeExists(t *testing.T, cli, vol string) bool {
	t.Helper()
	err := exec.Command(cli, "volume", "inspect", vol).Run()
	if err == nil {
		return true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return false
	}
	t.Fatalf("volume inspect %s: %v", vol, err)
	return false
}
