//go:build unix

package adapters

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/stack"
)

// fakeContainerCLI records every podman call so the argv contract and the
// Process lifecycle are exercised without a real podman.
type fakeContainerCLI struct {
	runArgs    []string
	runErr     error
	waited     []string
	stopped    []string
	removed    []string
	existsResp map[string]bool
	existsErr  error
}

func (f *fakeContainerCLI) run(_ context.Context, args []string) error {
	f.runArgs = args
	return f.runErr
}

func (f *fakeContainerCLI) wait(_ context.Context, name string) error {
	f.waited = append(f.waited, name)
	return nil
}

func (f *fakeContainerCLI) stop(_ context.Context, name string, _ time.Duration) error {
	f.stopped = append(f.stopped, name)
	return nil
}

func (f *fakeContainerCLI) remove(_ context.Context, name string) error {
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeContainerCLI) exists(_ context.Context, name string) (bool, error) {
	if f.existsErr != nil {
		return false, f.existsErr
	}
	return f.existsResp[name], nil
}

// testSpec is a representative resolved spec. The socket dir is a real temp dir
// so Start's MkdirAll succeeds; the data dir likewise.
func testSpec(t *testing.T) stack.PostgresContainerSpec {
	t.Helper()
	root := t.TempDir()
	return stack.PostgresContainerSpec{
		Name:        "compass-postgres-deadbeef",
		Image:       "docker.io/library/postgres:18@sha256:abc",
		DataDir:     filepath.Join(root, "postgres"),
		SocketDir:   filepath.Join(root, "pgsock"),
		Port:        "5433",
		StopTimeout: 30 * time.Second,
	}
}

// TestRunArgsMatchesS4Contract pins the exact `podman run` argv the S4 container
// contract requires: the userns remap, the stop timeout, the env set (including
// the POSTGRES_USER superuser the DSN-identity invariant forces beyond S4's
// enumerated list), the two bind-mounts, and the server args (both socket dirs,
// socket-only, the DSN port).
func TestRunArgsMatchesS4Contract(t *testing.T) {
	spec := testSpec(t)
	got := runArgs(spec, "alice")
	want := []string{
		"run", "--detach",
		"--rm",
		"--replace",
		"--name", "compass-postgres-deadbeef",
		"--userns=keep-id",
		"--stop-timeout", "30",
		"-e", "POSTGRES_DB=compass",
		"-e", "POSTGRES_HOST_AUTH_METHOD=trust",
		"-e", "POSTGRES_USER=alice",
		"-e", "PGDATA=/pgdata",
		"-v", spec.DataDir + ":/pgdata:Z",
		"-v", spec.SocketDir + ":" + spec.SocketDir + ":Z",
		"docker.io/library/postgres:18@sha256:abc",
		"-c", "unix_socket_directories=/var/run/postgresql," + spec.SocketDir,
		"-c", "listen_addresses=",
		"-p", "5433",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runArgs mismatch:\n got  %v\n want %v", got, want)
	}
}

// TestStartCreatesDirsAndRuns pins Start's side effects: it creates the
// bind-mount source dirs (podman requires the source to pre-exist) and issues
// exactly the run argv, returning a Process handle.
func TestStartCreatesDirsAndRuns(t *testing.T) {
	spec := testSpec(t)
	cli := &fakeContainerCLI{}
	pc := &PostgresContainer{cli: cli, superuser: "bob"}

	p, err := pc.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	if p == nil {
		t.Fatal("Start returned a nil Process")
	}
	for _, dir := range []string{spec.SocketDir, spec.DataDir} {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("Start did not create %q as a dir (err=%v)", dir, err)
		}
	}
	if !reflect.DeepEqual(cli.runArgs, runArgs(spec, "bob")) {
		t.Errorf("Start ran %v, want the S4 argv", cli.runArgs)
	}
}

// TestStartRunFailurePropagates pins that a failed `podman run` surfaces as an
// error, not a phantom Process handle.
func TestStartRunFailurePropagates(t *testing.T) {
	spec := testSpec(t)
	cli := &fakeContainerCLI{runErr: errors.New("podman: pull denied")}
	pc := &PostgresContainer{cli: cli, superuser: "bob"}

	if _, err := pc.Start(context.Background(), spec); err == nil {
		t.Fatal("Start() = nil error on a failed run, want the run error")
	}
}

// TestContainerProcessSignalStopsWaitBlocks pins the in-process Process contract
// over podman: Signal(SignalTerm) maps to podman stop, Wait maps to podman wait,
// and SignalKill is rejected (the cross-process teardown escalates via
// ContainerController.Remove, not the in-process handle).
func TestContainerProcessSignalStopsWaitBlocks(t *testing.T) {
	cli := &fakeContainerCLI{}
	p := &containerProcess{cli: cli, name: "compass-postgres-x", stopTimeout: 30 * time.Second}

	if err := p.Signal(stack.SignalTerm); err != nil {
		t.Fatalf("Signal(SignalTerm) = %v, want nil", err)
	}
	if !reflect.DeepEqual(cli.stopped, []string{"compass-postgres-x"}) {
		t.Fatalf("stop calls = %v, want one stop of the container", cli.stopped)
	}
	if err := p.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}
	if !reflect.DeepEqual(cli.waited, []string{"compass-postgres-x"}) {
		t.Fatalf("wait calls = %v, want one wait of the container", cli.waited)
	}
	if err := p.Signal(stack.SignalKill); err == nil {
		t.Fatal("Signal(SignalKill) = nil, want a rejection (in-process handle is graceful-only)")
	}
	if p.Pid() != 0 {
		t.Fatalf("Pid() = %d, want the 0 sentinel (a container carries no persisted pgid)", p.Pid())
	}
}

// TestControllerDispatch pins the ContainerController seam this adapter also
// fills: Exists reads the fake's existence map, Stop and Remove drive the
// respective podman calls by name.
func TestControllerDispatch(t *testing.T) {
	cli := &fakeContainerCLI{existsResp: map[string]bool{"live": true}}
	pc := &PostgresContainer{cli: cli, superuser: "bob"}

	if !pc.Exists("live") {
		t.Error("Exists(live) = false, want true")
	}
	if pc.Exists("gone") {
		t.Error("Exists(gone) = true, want false")
	}
	if err := pc.Stop("live", 10*time.Second); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	if err := pc.Remove("live"); err != nil {
		t.Fatalf("Remove() = %v", err)
	}
	if !reflect.DeepEqual(cli.stopped, []string{"live"}) {
		t.Errorf("stop calls = %v, want [live]", cli.stopped)
	}
	if !reflect.DeepEqual(cli.removed, []string{"live"}) {
		t.Errorf("remove calls = %v, want [live]", cli.removed)
	}
}

// TestExistsAssumesPresentOnEngineError pins the stranded-container guard: a
// genuine podman engine error (not the exit-1 "absent" verdict) makes Exists
// report PRESENT, so entryAlive still builds a teardown target instead of
// silently dropping a live container after the pgid record is consumed. A
// false "absent" here would strand the container and let down report success.
func TestExistsAssumesPresentOnEngineError(t *testing.T) {
	cli := &fakeContainerCLI{existsErr: errors.New("podman: daemon wedged")}
	pc := &PostgresContainer{cli: cli, superuser: "bob"}

	if !pc.Exists("compass-postgres-x") {
		t.Error("Exists() on a podman engine error = false, want true (assume present so teardown still drives Stop/Remove)")
	}
}

// TestStopSecondsRounding pins the whole-second conversion: a sub-second grace
// rounds up (never truncates to an immediate SIGKILL), a negative clamps to 0.
func TestStopSecondsRounding(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int64
	}{
		{30 * time.Second, 30},
		{500 * time.Millisecond, 1},
		{0, 0},
		{-5 * time.Second, 0},
	}
	for _, c := range cases {
		if got := stopSeconds(c.in); got != c.want {
			t.Errorf("stopSeconds(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}
