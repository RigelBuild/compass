//go:build (linux && gtk4) || darwin

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Fixtures shaped like the podman CLI output the machine code parses. Every one
// of these shapes is an ASSUMPTION about external podman behavior (the design
// record marks the inspect socket path and the `machine ls --format json`
// no-machine-vs-stopped distinction as spike-verified, and no macOS host has run
// the spike), so the parse is written to tolerate drift and the tests pin the
// tolerance, not one exact vendor shape.
const (
	listEmpty      = `[]`
	listRunning    = `[{"Name":"podman-machine-default","Default":true,"Running":true}]`
	listStopped    = `[{"Name":"podman-machine-default","Default":true,"Running":false}]`
	listStateOnly  = `[{"Name":"podman-machine-default","Default":true,"State":"running"}]`
	inspectRunning = `[{"Name":"podman-machine-default","State":"running",` +
		`"ConnectionInfo":{"PodmanSocket":{"Path":"/tmp/podman.sock"}}}]`
	inspectStopped   = `[{"Name":"podman-machine-default","State":"stopped","ConnectionInfo":{}}]`
	inspectNoSocket  = `[{"Name":"podman-machine-default","State":"running","ConnectionInfo":{}}]`
	inspectEmptyList = `[]`
)

// stubMachineDeps returns a machineDeps whose every effect succeeds against a
// running machine with a reachable socket. Tests override one field at a time.
// The counters let a test assert the ensure step's ORDER and idempotence (that a
// running machine is never re-initialized).
type machineRecorder struct {
	listCalls    int
	inspectCalls int
	initCalls    int
	startCalls   int
	dialCalls    int
	dialed       string
	// listOut is returned by list; it is a field so the ensure step's re-probe
	// can observe a DIFFERENT state than the first probe, which is how a real
	// init/start becomes visible.
	listOut    string
	inspectOut string
}

func stubMachineDeps(rec *machineRecorder) machineDeps {
	return machineDeps{
		list: func(context.Context) ([]byte, error) {
			rec.listCalls++
			return []byte(rec.listOut), nil
		},
		inspect: func(context.Context) ([]byte, error) {
			rec.inspectCalls++
			return []byte(rec.inspectOut), nil
		},
		initMachine: func(context.Context) error {
			rec.initCalls++
			// A real init creates the machine, so the next probe sees it stopped.
			rec.listOut = listStopped
			rec.inspectOut = inspectStopped
			return nil
		},
		startMachine: func(context.Context) error {
			rec.startCalls++
			rec.listOut = listRunning
			rec.inspectOut = inspectRunning
			return nil
		},
		dialSocket: func(_ context.Context, path string) error {
			rec.dialCalls++
			rec.dialed = path
			return nil
		},
	}
}

// runningRecorder is the all-good starting state: a machine that exists and runs.
func runningRecorder() *machineRecorder {
	return &machineRecorder{listOut: listRunning, inspectOut: inspectRunning}
}

// TestMachineReadyRunning: the running state — the probe passes and it proves
// readiness by DIALING the forwarded socket inspect reported, not by trusting
// the state string alone.
func TestMachineReadyRunning(t *testing.T) {
	rec := runningRecorder()
	if err := machineReady(context.Background(), stubMachineDeps(rec)); err != nil {
		t.Fatalf("machineReady on a running machine = %v, want nil", err)
	}
	if rec.dialCalls != 1 {
		t.Errorf("dial calls = %d, want 1 (readiness must probe the socket)", rec.dialCalls)
	}
	if rec.dialed != "/tmp/podman.sock" {
		t.Errorf("dialed %q, want the inspect ConnectionInfo.PodmanSocket.Path", rec.dialed)
	}
}

// TestMachineReadyStateStringOnly: a podman version that reports running-ness as
// a State string rather than a Running bool still classifies as running — the
// parse must not depend on either single spelling.
func TestMachineReadyStateStringOnly(t *testing.T) {
	rec := &machineRecorder{listOut: listStateOnly, inspectOut: inspectRunning}
	if err := machineReady(context.Background(), stubMachineDeps(rec)); err != nil {
		t.Fatalf("machineReady with a State-only list entry = %v, want nil", err)
	}
}

// TestMachineReadyNoMachine: the fresh-Mac state. `machine ls` lists nothing, so
// the copy must say no machine exists and name the init command — and must NOT
// have consulted inspect (which fails identically for absent and stopped).
func TestMachineReadyNoMachine(t *testing.T) {
	rec := &machineRecorder{listOut: listEmpty}
	err := machineReady(context.Background(), stubMachineDeps(rec))
	if err == nil {
		t.Fatal("machineReady with no machine = nil, want an error")
	}
	for _, tok := range []string{"no podman machine exists", "machine init", "--memory", "--disk-size"} {
		if !strings.Contains(err.Error(), tok) {
			t.Errorf("no-machine copy %q missing %q", err.Error(), tok)
		}
	}
	if rec.inspectCalls != 0 {
		t.Errorf("inspect calls = %d, want 0 (absence is established by ls alone)", rec.inspectCalls)
	}
}

// TestMachineReadyStopped: a machine exists but is not running — distinguishable
// from no-machine, and the copy names start (not init) plus the machine name.
func TestMachineReadyStopped(t *testing.T) {
	rec := &machineRecorder{listOut: listStopped, inspectOut: inspectStopped}
	err := machineReady(context.Background(), stubMachineDeps(rec))
	if err == nil {
		t.Fatal("machineReady on a stopped machine = nil, want an error")
	}
	for _, tok := range []string{"is not running", "machine start", "podman-machine-default"} {
		if !strings.Contains(err.Error(), tok) {
			t.Errorf("stopped copy %q missing %q", err.Error(), tok)
		}
	}
	if strings.Contains(err.Error(), "no podman machine exists") {
		t.Errorf("stopped copy %q conflates a stopped machine with an absent one", err.Error())
	}
	if rec.dialCalls != 0 {
		t.Errorf("dial calls = %d, want 0 (a stopped machine has no socket to dial)", rec.dialCalls)
	}
}

// TestMachineReadyUnreachableSocket: the third distinguishable state — running,
// but the forwarded API socket does not answer. This is the state a state-string
// check alone would call ready.
func TestMachineReadyUnreachableSocket(t *testing.T) {
	rec := runningRecorder()
	deps := stubMachineDeps(rec)
	deps.dialSocket = func(context.Context, string) error {
		return errors.New("connect: connection refused")
	}
	err := machineReady(context.Background(), deps)
	if err == nil {
		t.Fatal("machineReady with an unreachable socket = nil, want an error")
	}
	for _, tok := range []string{"is running", "unreachable", "/tmp/podman.sock", "machine stop", "machine start"} {
		if !strings.Contains(err.Error(), tok) {
			t.Errorf("unreachable-socket copy %q missing %q", err.Error(), tok)
		}
	}
}

// TestMachineReadyRunningWithoutSocketPath: a running machine whose inspect
// reports NO socket path is the unparseable-response case, and must never read
// as ready — that is precisely how a green preflight would precede an
// undiagnosable downstream failure.
func TestMachineReadyRunningWithoutSocketPath(t *testing.T) {
	rec := &machineRecorder{listOut: listRunning, inspectOut: inspectNoSocket}
	err := machineReady(context.Background(), stubMachineDeps(rec))
	if err == nil {
		t.Fatal("machineReady with no reported socket path = nil, want an error")
	}
	if !strings.Contains(err.Error(), "no API socket path") {
		t.Errorf("copy %q does not name the missing socket path", err.Error())
	}
	if rec.dialCalls != 0 {
		t.Errorf("dial calls = %d, want 0 (there is no path to dial)", rec.dialCalls)
	}
}

// TestMachineReadyUnparseableOutput: every way the podman CLI can answer
// unintelligibly — a failing command, non-JSON output, an inspect that describes
// no machine — classifies as UNKNOWN and errors. None of them may read as ready.
func TestMachineReadyUnparseableOutput(t *testing.T) {
	cliErr := errors.New("podman: command not found")
	cases := map[string]struct {
		mutate func(*machineDeps)
		want   string
	}{
		"ls fails": {
			mutate: func(d *machineDeps) {
				d.list = func(context.Context) ([]byte, error) { return nil, cliErr }
			},
			want: "machine ls --format json` failed",
		},
		"ls is not json": {
			mutate: func(d *machineDeps) {
				d.list = func(context.Context) ([]byte, error) { return []byte("Error: unknown flag"), nil }
			},
			want: "did not parse",
		},
		"inspect fails": {
			mutate: func(d *machineDeps) {
				d.inspect = func(context.Context) ([]byte, error) { return nil, cliErr }
			},
			want: "machine inspect` failed",
		},
		"inspect is not json": {
			mutate: func(d *machineDeps) {
				d.inspect = func(context.Context) ([]byte, error) { return []byte("not json"), nil }
			},
			want: "did not parse",
		},
		"inspect describes no machine": {
			mutate: func(d *machineDeps) {
				d.inspect = func(context.Context) ([]byte, error) { return []byte(inspectEmptyList), nil }
			},
			want: "described no machine",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			deps := stubMachineDeps(runningRecorder())
			tc.mutate(&deps)
			err := machineReady(context.Background(), deps)
			if err == nil {
				t.Fatalf("%s: machineReady = nil, want an error (unparseable is never ready)", name)
			}
			if !errors.Is(err, errMachineUnclassified) {
				t.Errorf("%s: err %v does not wrap errMachineUnclassified", name, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: copy %q missing %q", name, err.Error(), tc.want)
			}
		})
	}
}

// TestEnsureMachineReadyNoMachineProvisions: the fresh-Mac path — init, then
// start, then RE-PROBE to nil. The re-probe is what establishes readiness; the
// exit status of init/start is not trusted on its own.
func TestEnsureMachineReadyNoMachineProvisions(t *testing.T) {
	rec := &machineRecorder{listOut: listEmpty}
	if err := ensureMachineReady(context.Background(), stubMachineDeps(rec)); err != nil {
		t.Fatalf("ensureMachineReady from no machine = %v, want nil after provisioning", err)
	}
	if rec.initCalls != 1 {
		t.Errorf("init calls = %d, want 1", rec.initCalls)
	}
	if rec.startCalls != 1 {
		t.Errorf("start calls = %d, want 1 (a freshly-created machine is not running)", rec.startCalls)
	}
	if rec.listCalls < 2 {
		t.Errorf("list calls = %d, want >= 2 (the ensure step must re-probe)", rec.listCalls)
	}
	if rec.dialCalls != 1 {
		t.Errorf("dial calls = %d, want 1 (the re-probe proves the socket answers)", rec.dialCalls)
	}
}

// TestEnsureMachineReadyStoppedStarts: a stopped machine is STARTED, never
// re-initialized (init on an existing machine would fail, and would re-download).
func TestEnsureMachineReadyStoppedStarts(t *testing.T) {
	rec := &machineRecorder{listOut: listStopped, inspectOut: inspectStopped}
	if err := ensureMachineReady(context.Background(), stubMachineDeps(rec)); err != nil {
		t.Fatalf("ensureMachineReady from stopped = %v, want nil after start", err)
	}
	if rec.initCalls != 0 {
		t.Errorf("init calls = %d, want 0 (the machine already exists)", rec.initCalls)
	}
	if rec.startCalls != 1 {
		t.Errorf("start calls = %d, want 1", rec.startCalls)
	}
}

// TestEnsureMachineReadyRunningIsNoOp: an already-ready machine provisions
// nothing — the ensure step is idempotent, so a normal launch pays only the probe.
func TestEnsureMachineReadyRunningIsNoOp(t *testing.T) {
	rec := runningRecorder()
	if err := ensureMachineReady(context.Background(), stubMachineDeps(rec)); err != nil {
		t.Fatalf("ensureMachineReady on a running machine = %v, want nil", err)
	}
	if rec.initCalls != 0 || rec.startCalls != 0 {
		t.Errorf("provisioned an already-running machine: init=%d start=%d", rec.initCalls, rec.startCalls)
	}
}

// TestEnsureMachineReadyInitFails: the init-fails state. The error names the
// init command WITH the resource floor so the operator can run it by hand, and
// the step does not press on to start a machine that was never created.
func TestEnsureMachineReadyInitFails(t *testing.T) {
	rec := &machineRecorder{listOut: listEmpty}
	deps := stubMachineDeps(rec)
	deps.initMachine = func(context.Context) error {
		rec.initCalls++
		return errors.New("no space left on device")
	}
	err := ensureMachineReady(context.Background(), deps)
	if err == nil {
		t.Fatal("ensureMachineReady with a failing init = nil, want an error")
	}
	for _, tok := range []string{"machine init", "--memory", "--disk-size", "no space left on device"} {
		if !strings.Contains(err.Error(), tok) {
			t.Errorf("init-failure copy %q missing %q", err.Error(), tok)
		}
	}
	if rec.startCalls != 0 {
		t.Errorf("start calls = %d, want 0 (nothing was created to start)", rec.startCalls)
	}
}

// TestEnsureMachineReadyStartFails: a start that refuses surfaces podman's
// reason and names the command, rather than reporting a bare unready machine.
func TestEnsureMachineReadyStartFails(t *testing.T) {
	rec := &machineRecorder{listOut: listStopped, inspectOut: inspectStopped}
	deps := stubMachineDeps(rec)
	deps.startMachine = func(context.Context) error {
		rec.startCalls++
		return errors.New("vfkit: not permitted")
	}
	err := ensureMachineReady(context.Background(), deps)
	if err == nil {
		t.Fatal("ensureMachineReady with a failing start = nil, want an error")
	}
	for _, tok := range []string{"machine start", "podman-machine-default", "vfkit: not permitted"} {
		if !strings.Contains(err.Error(), tok) {
			t.Errorf("start-failure copy %q missing %q", err.Error(), tok)
		}
	}
}

// TestEnsureMachineReadyUnclassifiedDoesNotProvision: an unintelligible CLI
// answer is NOT something init/start can fix, so the ensure step must surface it
// rather than blindly initializing over a machine whose state it cannot read.
func TestEnsureMachineReadyUnclassifiedDoesNotProvision(t *testing.T) {
	rec := runningRecorder()
	deps := stubMachineDeps(rec)
	deps.list = func(context.Context) ([]byte, error) {
		rec.listCalls++
		return []byte("Error: unknown flag: --format"), nil
	}
	err := ensureMachineReady(context.Background(), deps)
	if !errors.Is(err, errMachineUnclassified) {
		t.Fatalf("ensureMachineReady on an unparseable ls = %v, want errMachineUnclassified", err)
	}
	if rec.initCalls != 0 || rec.startCalls != 0 {
		t.Errorf("provisioned against an unreadable state: init=%d start=%d", rec.initCalls, rec.startCalls)
	}
}

// TestMachineResourceFloorIsExplicit: the floor must stay ABOVE `podman machine
// init`'s own 2 GiB memory default — the whole reason the flags are passed is
// that the default starves the four containers the embedded stack runs. A future
// edit that drops the floor back to the default silently reintroduces that.
func TestMachineResourceFloorIsExplicit(t *testing.T) {
	const podmanDefaultMemoryMiB = 2048
	if machineMemoryFloorMiB <= podmanDefaultMemoryMiB {
		t.Errorf("memory floor %d MiB does not exceed podman's own default %d MiB",
			machineMemoryFloorMiB, podmanDefaultMemoryMiB)
	}
	if machineDiskFloorGiB <= 0 {
		t.Errorf("disk floor %d GiB is not a usable size", machineDiskFloorGiB)
	}
}
