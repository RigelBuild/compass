//go:build unix

package adapters

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	termed     []string
	removed    []string
	rmExited   []string
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

func (f *fakeContainerCLI) term(_ context.Context, name string) error {
	f.termed = append(f.termed, name)
	return nil
}

func (f *fakeContainerCLI) remove(_ context.Context, name string) error {
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeContainerCLI) removeExited(_ context.Context, name string) error {
	f.rmExited = append(f.rmExited, name)
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

	if err := p.Signal(context.Background(), stack.SignalTerm); err != nil {
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
	if err := p.Signal(context.Background(), stack.SignalKill); err == nil {
		t.Fatal("Signal(SignalKill) = nil, want a rejection (in-process handle is graceful-only)")
	}
	if p.Pid() != 0 {
		t.Fatalf("Pid() = %d, want the 0 sentinel (a container carries no persisted pgid)", p.Pid())
	}
}

// TestControllerDispatch pins the ContainerController seam this adapter also
// fills: Exists reads the fake's existence map; Stop sends the non-blocking
// stop signal, never the blocking `podman stop`; RemoveExited and Remove drive
// their podman calls by name.
func TestControllerDispatch(t *testing.T) {
	ctx := context.Background()
	cli := &fakeContainerCLI{existsResp: map[string]bool{"live": true}}
	pc := &PostgresContainer{cli: cli, superuser: "bob"}

	if !pc.Exists(ctx, "live") {
		t.Error("Exists(live) = false, want true")
	}
	if pc.Exists(ctx, "gone") {
		t.Error("Exists(gone) = true, want false")
	}
	if err := pc.Stop(ctx, "live"); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	if err := pc.RemoveExited(ctx, "live"); err != nil {
		t.Fatalf("RemoveExited() = %v", err)
	}
	if err := pc.Remove(ctx, "live"); err != nil {
		t.Fatalf("Remove() = %v", err)
	}
	if len(cli.stopped) != 0 || !reflect.DeepEqual(cli.termed, []string{"live"}) {
		t.Errorf("stop calls = %v, term calls = %v, want only a term of [live]", cli.stopped, cli.termed)
	}
	if !reflect.DeepEqual(cli.rmExited, []string{"live"}) {
		t.Errorf("non-forced remove calls = %v, want [live]", cli.rmExited)
	}
	if !reflect.DeepEqual(cli.removed, []string{"live"}) {
		t.Errorf("remove calls = %v, want [live]", cli.removed)
	}
}

// TestRemoveExitedToleratesRunningAndAbsent: a non-forced rm that podman refuses
// because the container still runs, or because it is gone, is not an error; any
// other failure still surfaces. The stderr lines are podman 5's real output.
func TestRemoveExitedToleratesRunningAndAbsent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stderr  string
		wantErr bool
	}{
		{"running", "Error: cannot remove container x as it is running - running or paused containers cannot be removed without force: container state improper", false},
		{"absent", `Error: no container with ID or name "x" found: no such container`, false},
		{"engine failure", "Error: database is locked", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := fakePodman(t, "echo '"+tc.stderr+"' >&2\nexit 2\n")
			if err := e.removeExited(context.Background(), "x"); (err != nil) != tc.wantErr {
				t.Fatalf("removeExited() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// fakePodman returns a podmanExec over a shell script with the given body. Each
// invocation's argv is appended, one line per call, to the returned log path.
func fakePodman(t *testing.T, body string) (*podmanExec, string) {
	t.Helper()
	dir := t.TempDir()
	prog := filepath.Join(dir, "podman")
	argvLog := filepath.Join(dir, "argv")
	script := "#!/bin/sh\necho \"$*\" >> '" + argvLog + "'\n" + body
	if err := os.WriteFile(prog, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return &podmanExec{program: prog, timeout: 5 * time.Second}, argvLog
}

// readArgv returns the recorded podman invocations, one per element.
func readArgv(t *testing.T, argvLog string) []string {
	t.Helper()
	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// TestRemoveSkipsStopTimeout: a force-remove is the hard kill, so it must not
// wait out the container's --stop-timeout grace first.
func TestRemoveSkipsStopTimeout(t *testing.T) {
	e, argvLog := fakePodman(t, "exit 0\n")
	if err := e.remove(context.Background(), "x"); err != nil {
		t.Fatalf("remove() = %v", err)
	}
	if got, want := readArgv(t, argvLog), []string{"rm --force --time 0 --volumes x"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

// TestTermSendsConfiguredStopSignal: term reads the container's stop signal and
// kills with it (postgres stops on SIGINT), defaults to SIGTERM when none is
// set, and treats a vanished container as already stopped.
func TestTermSendsConfiguredStopSignal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		inspect  string
		wantArgv []string
	}{
		{"configured", "echo SIGINT", []string{"container inspect --format {{.Config.StopSignal}} x", "kill --signal SIGINT x"}},
		{"unset", "echo", []string{"container inspect --format {{.Config.StopSignal}} x", "kill --signal SIGTERM x"}},
		{"absent", "echo 'Error: no such container x' >&2; exit 125", []string{"container inspect --format {{.Config.StopSignal}} x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, argvLog := fakePodman(t, "if [ \"$1\" = container ]; then "+tc.inspect+"; fi\n")
			if err := e.term(context.Background(), "x"); err != nil {
				t.Fatalf("term() = %v", err)
			}
			if got := readArgv(t, argvLog); !reflect.DeepEqual(got, tc.wantArgv) {
				t.Fatalf("argv = %q, want %q", got, tc.wantArgv)
			}
		})
	}
}

// TestPodmanCallReturnsWhenGrandchildHoldsStderr: a podman that exits while a
// child it spawned keeps stderr open must not hang the call past WaitDelay.
func TestPodmanCallReturnsWhenGrandchildHoldsStderr(t *testing.T) {
	e, _ := fakePodman(t, "sleep 8 &\nexit 0\n")
	done := make(chan error, 2)
	go func() { done <- e.fireAndCheck(context.Background(), []string{"rm", "x"}) }()
	go func() {
		_, err := e.output(context.Background(), []string{"inspect", "x"})
		done <- err
	}()
	deadline := time.NewTimer(e.timeout - time.Second)
	defer deadline.Stop()
	for range 2 {
		select {
		case <-done:
		case <-deadline.C:
			t.Fatal("podman call still blocked on an inherited stderr pipe")
		}
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

	if !pc.Exists(context.Background(), "compass-postgres-x") {
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
