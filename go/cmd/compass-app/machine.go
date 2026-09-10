//go:build (linux && gtk4) || darwin

// The podman-machine probe and ensure step behind an injected seam. On macOS the
// podman CLI drives a Linux VM ("the machine") and a fresh Mac has no machine at
// all, so embedded mode must both DETECT the machine's state and PROVISION it —
// mirroring how `compass-stack up` ensures the agent image and the database
// rather than gating on them. The OS choice is a parameter, not a build tag
// (machineReadyAdapter takes the GOOS the preflight core is already keyed on),
// so the darwin wiring is reachable from a test on any host — a build-tagged
// darwin adapter would make the regression this closes untestable on every
// lane that actually runs. The classification, the error copy, and the ensure
// orchestration are inverted over machineDeps, so all four states (no machine
// / stopped / running / init fails) are unit-testable with no podman present.
//
// Every shape this file reads out of the podman CLI is an ASSUMPTION about
// external behavior — the design record marks the `machine inspect` socket path
// and the `machine ls --format json` no-machine-vs-stopped distinction as
// spike-verified, and the spike has not run. So the parsing is deliberately
// defensive and TOLERANT of shape drift (both the `Running` bool and the `State`
// string are accepted; a missing field degrades, never panics), and an
// unparseable answer is classified UNKNOWN, which is never ready. The failure
// copy always names the podman command the operator can run themselves.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec" //nolint:depguard // podman machine seam: fixed-arg `podman machine ls|inspect|init|start` subprocesses
	"strconv"
	"strings"
)

// podmanBin is the podman executable name, resolved on PATH.
const podmanBin = "podman"

// The machine resource floor passed to `podman machine init`. `podman machine
// init`'s own defaults are modest (2 GiB of memory), and the embedded stack runs
// FOUR containers inside the VM — postgres, the collector, the server, and an
// agent container per session — so the default leaves the machine thrashing or
// OOM-killing the agent under a normal session. The floor below is sized for
// that set with headroom for a second concurrent agent; the disk figure covers
// the pulled images (agent + postgres + collector) plus the postgres data
// directory's growth over a long-lived install.
//
// UNVERIFIED: no macOS host has run the spike, so this floor is a reasoned
// choice, not a measured one, and the units are the podman CLI's documented ones
// (--memory in MiB, --disk-size in GiB). The spike is what turns it into a
// number recorded in the self-host doc.
const (
	machineMemoryFloorMiB = 8192
	machineDiskFloorGiB   = 100
)

// machineStatus is the classified state of the podman machine. The zero value is
// machineUnknown so a failed classification can never read as ready.
type machineStatus int

const (
	// machineUnknown: the podman CLI's answer could not be classified (it failed,
	// or its output did not parse). Never treated as ready.
	machineUnknown machineStatus = iota
	// machineAbsent: no machine exists at all — the fresh-Mac state. Fixed by init.
	machineAbsent
	// machineStopped: a machine exists but is not running. Fixed by start.
	machineStopped
	// machineRunning: the machine reports itself running; its socket still has to
	// be reachable before the machine counts as ready.
	machineRunning
)

// machineDeps is the seam the machine probe and ensure step are inverted over:
// one field per genuine external effect, exactly as preflight.Deps inverts the
// host checks it runs. The real adapters shell the podman CLI
// (realMachineDeps); tests supply deterministic stubs.
type machineDeps struct {
	// list runs `podman machine ls --format json` and returns its stdout. Its
	// presence-or-absence is the one thing the classification depends on: an
	// empty list means no machine exists. What `inspect` does for a stopped
	// machine is one of the unspiked assumptions this file's doc flags, so
	// nothing is inferred from it failing.
	list func(ctx context.Context) ([]byte, error)
	// inspect runs `podman machine inspect` (no machine named, so the DEFAULT
	// machine) and returns its stdout: the authoritative state plus the host-side
	// API socket path the VM forwards.
	inspect func(ctx context.Context) ([]byte, error)
	// initMachine runs `podman machine init` with the resource floor. On a fresh
	// host this DOWNLOADS a VM image and takes minutes.
	initMachine func(ctx context.Context) error
	// startMachine runs `podman machine start` on the default machine.
	startMachine func(ctx context.Context) error
	// dialSocket proves the machine's forwarded API socket is actually reachable
	// from the host (a running machine whose socket does not answer is not ready).
	dialSocket func(ctx context.Context, path string) error
}

// machineInfo is one classification of the machine: its status, the machine name
// to name in operator copy, and the host-side API socket path when known.
type machineInfo struct {
	status machineStatus
	name   string
	socket string
}

// machineListEntry is the subset of `podman machine ls --format json` this code
// reads. Running is a pointer and State is a string because the field set
// differs across podman versions and this parse must not depend on either being
// present: presence in the list is what establishes existence, and running-ness
// is read from whichever field the CLI supplied.
//
// A machine mid-start is deliberately NOT a distinct case. It classifies as
// stopped and gets a `machine start`, which is a no-op on a machine already
// coming up — one redundant command on a rare path, against a third state to
// carry through the whole ensure step.
type machineListEntry struct {
	Name    string `json:"Name"`
	Running *bool  `json:"Running"`
	State   string `json:"State"`
	Default bool   `json:"Default"`
}

// running reports whether this entry says the machine is up, accepting either
// the boolean or the string spelling.
func (e machineListEntry) running() bool {
	if e.Running != nil && *e.Running {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(e.State), "running")
}

// machineInspectEntry is the subset of `podman machine inspect` this code reads:
// the state and the forwarded podman API socket path
// (.ConnectionInfo.PodmanSocket.Path).
type machineInspectEntry struct {
	Name           string `json:"Name"`
	State          string `json:"State"`
	ConnectionInfo struct {
		PodmanSocket struct {
			Path string `json:"Path"`
		} `json:"PodmanSocket"`
	} `json:"ConnectionInfo"`
}

// errMachineUnclassified is the sentinel for "the podman CLI's answer could not
// be turned into a state". Callers wrap it with the command to run; it exists so
// the ensure step can tell an unclassifiable answer (do nothing, surface it)
// from a state it knows how to fix.
var errMachineUnclassified = errors.New("the podman machine state could not be determined")

// probeMachine classifies the machine from the podman CLI. It calls `machine ls`
// FIRST — that is the only call that separates no-machine from stopped-machine —
// and then `machine inspect` for the authoritative state and the socket path of
// the default machine. A CLI or parse failure yields machineUnknown with an
// error naming the command to run by hand; it never guesses ready.
func probeMachine(ctx context.Context, d machineDeps) (machineInfo, error) {
	out, err := d.list(ctx)
	if err != nil {
		return machineInfo{}, fmt.Errorf("%w: `%s machine ls --format json` failed: %w",
			errMachineUnclassified, podmanBin, err)
	}
	var listed []machineListEntry
	if err := json.Unmarshal(out, &listed); err != nil {
		return machineInfo{}, fmt.Errorf(
			"%w: `%s machine ls --format json` output did not parse (%w); run it by hand to see what podman reports",
			errMachineUnclassified, podmanBin, err)
	}
	if len(listed) == 0 {
		return machineInfo{status: machineAbsent}, nil
	}

	// A machine exists. Prefer the default entry for the name, since the podman
	// CLI resolves its connection to the default machine.
	entry := listed[0]
	for _, e := range listed {
		if e.Default {
			entry = e
			break
		}
	}

	insp, err := inspectMachine(ctx, d)
	if err != nil {
		return machineInfo{name: entry.Name}, err
	}
	name := insp.Name
	if name == "" {
		name = entry.Name
	}

	// Running-ness: inspect's state is authoritative when it says running;
	// otherwise fall back to the list entry, so a podman version that omits
	// State from one of the two commands still classifies.
	running := strings.EqualFold(strings.TrimSpace(insp.State), "running") || entry.running()
	if !running {
		return machineInfo{status: machineStopped, name: name}, nil
	}
	return machineInfo{
		status: machineRunning,
		name:   name,
		socket: strings.TrimSpace(insp.ConnectionInfo.PodmanSocket.Path),
	}, nil
}

// inspectMachine runs the inspect seam and pulls out the single entry for the
// default machine. inspect returns a JSON ARRAY even for one machine; an empty
// array or a parse failure is unclassified, never ready.
func inspectMachine(ctx context.Context, d machineDeps) (machineInspectEntry, error) {
	out, err := d.inspect(ctx)
	if err != nil {
		return machineInspectEntry{}, fmt.Errorf("%w: `%s machine inspect` failed: %w",
			errMachineUnclassified, podmanBin, err)
	}
	var entries []machineInspectEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return machineInspectEntry{}, fmt.Errorf(
			"%w: `%s machine inspect` output did not parse (%w); run it by hand to see what podman reports",
			errMachineUnclassified, podmanBin, err)
	}
	if len(entries) == 0 {
		return machineInspectEntry{}, fmt.Errorf(
			"%w: `%s machine inspect` described no machine even though `%s machine ls` listed one",
			errMachineUnclassified, podmanBin, podmanBin)
	}
	return entries[0], nil
}

// machineReady is the probe half: nil when the machine is up AND its forwarded
// API socket answers, and otherwise an error whose copy distinguishes the three
// failing states the operator can act on — no machine, a stopped machine, and a
// running machine with an unreachable socket — each naming the command to run.
func machineReady(ctx context.Context, d machineDeps) error {
	info, err := probeMachine(ctx, d)
	if err != nil {
		return err
	}
	switch info.status {
	case machineAbsent:
		return fmt.Errorf("no podman machine exists; create one with `%s machine init --memory %d --disk-size %d` "+
			"(the first run downloads a VM image and takes several minutes)",
			podmanBin, machineMemoryFloorMiB, machineDiskFloorGiB)
	case machineStopped:
		return fmt.Errorf("the podman machine %q exists but is not running; start it with `%s machine start %s`",
			info.name, podmanBin, info.name)
	case machineRunning:
		return machineSocketReachable(ctx, d, info)
	case machineUnknown:
		return machineStateUnreadable()
	default:
		return machineStateUnreadable()
	}
}

// machineStateUnreadable is the error for a machine state the podman CLI would
// not tell us. Shared by the probe and the ensure step so both report the same
// copy, and pointing at the command whose output could not be classified.
func machineStateUnreadable() error {
	return fmt.Errorf("%w; run `%s machine ls --format json` to see what podman reports",
		errMachineUnclassified, podmanBin)
}

// machineSocketReachable checks the third failing state: the machine is running
// but the API socket the VM forwards to the host does not answer. An empty path
// counts as unreachable — a running machine that reports no socket is exactly
// the unparseable-response case, and treating it as ready is what would produce
// a green preflight followed by an undiagnosable failure.
func machineSocketReachable(ctx context.Context, d machineDeps, info machineInfo) error {
	if info.socket == "" {
		return fmt.Errorf("the podman machine %q is running but `%s machine inspect` reported no API socket path; "+
			"restart it with `%s machine stop %s && %s machine start %s`",
			info.name, podmanBin, podmanBin, info.name, podmanBin, info.name)
	}
	if err := d.dialSocket(ctx, info.socket); err != nil {
		return fmt.Errorf("the podman machine %q is running but its API socket %s is unreachable (%w); "+
			"restart it with `%s machine stop %s && %s machine start %s`",
			info.name, info.socket, err, podmanBin, info.name, podmanBin, info.name)
	}
	return nil
}

// ensureMachineReady is what the darwin MachineReady adapter wires: it makes the
// machine ready rather than merely reporting on it. On no-machine it inits then
// starts; on a stopped machine it starts; then it RE-PROBES, because the
// authority on readiness is the probe, never the exit status of init/start. A
// state it cannot fix (an unclassifiable CLI answer, or a running machine whose
// socket does not answer) is surfaced from the probe unchanged.
//
// The init download is minutes long and runs under the caller's context, which
// the embedded pipeline bounds with its bring-up window. On darwin that window
// is sized for a cold provision (bringUpTimeoutFor in main.go), so a healthy
// first run fits inside it. The copy on the failure path still names the init
// command, so an operator who does exhaust the window gets something to run by
// hand rather than a bare deadline error.
func ensureMachineReady(ctx context.Context, d machineDeps) error {
	info, err := probeMachine(ctx, d)
	if err != nil {
		return err
	}
	switch info.status {
	case machineAbsent:
		if err := d.initMachine(ctx); err != nil {
			return fmt.Errorf("provisioning a podman machine with `%s machine init --memory %d --disk-size %d` "+
				"failed (%w); run it by hand — the first run downloads a VM image and takes several minutes",
				podmanBin, machineMemoryFloorMiB, machineDiskFloorGiB, err)
		}
		if err := d.startMachine(ctx); err != nil {
			return fmt.Errorf("the podman machine was created but `%s machine start` failed (%w); "+
				"run it by hand to see what podman reports", podmanBin, err)
		}
	case machineStopped:
		if err := d.startMachine(ctx); err != nil {
			return fmt.Errorf("starting the podman machine %q with `%s machine start %s` failed (%w); "+
				"run it by hand to see what podman reports", info.name, podmanBin, info.name, err)
		}
	case machineRunning:
		// Nothing to provision; a running machine only needs its socket checked,
		// which the re-probe below does.
	case machineUnknown:
		// Not something init/start can fix, and re-probing would only ask the
		// same unintelligible question again — reporting the second answer
		// instead of the first, which is worse if the machine changed state
		// between the two. Report what the probe already told us.
		return machineStateUnreadable()
	default:
		return machineStateUnreadable()
	}
	return machineReady(ctx, d)
}

// machineReadyAdapter returns the preflight.Deps.MachineReady adapter for the
// given host OS: on darwin the podman-machine ENSURE step (provision or start
// the Linux VM, then re-probe), and nil elsewhere — linux podman is native, so
// there is no machine and the preflight core omits the check.
//
// The OS is a PARAMETER rather than a build tag, matching preflight.Deps.GOOS:
// a build-tagged darwin-only adapter would be uncompilable from a linux test, so
// the very regression this closes — a darwin build reaching preflight with no
// machine adapter — could not be tested anywhere the CI actually runs. Keyed off
// GOOS instead, a linux host can assert the darwin wiring.
func machineReadyAdapter(goos string) func(ctx context.Context) error {
	if goos != "darwin" {
		return nil
	}
	deps := realMachineDeps()
	return func(ctx context.Context) error {
		return ensureMachineReady(ctx, deps)
	}
}

// realMachineDeps builds the machine seam over the real podman CLI. Each field
// is one fixed-argv subprocess; the argv carries no caller-supplied strings
// except the resource floor constants, so there is nothing to inject into it.
func realMachineDeps() machineDeps {
	return machineDeps{
		list: func(ctx context.Context) ([]byte, error) {
			return machineOutput(ctx, "ls", "--format", "json")
		},
		inspect: func(ctx context.Context) ([]byte, error) {
			return machineOutput(ctx, "inspect")
		},
		initMachine: func(ctx context.Context) error {
			return machineRun(ctx, "init",
				"--memory", strconv.Itoa(machineMemoryFloorMiB),
				"--disk-size", strconv.Itoa(machineDiskFloorGiB))
		},
		startMachine: func(ctx context.Context) error {
			return machineRun(ctx, "start")
		},
		dialSocket: dialUnixSocket,
	}
}

// machineOutput runs `podman machine <args...>` and returns its STDOUT only —
// the JSON readers must not be fed podman's warnings — wrapping a failure with
// the captured stderr so the copy names why podman refused.
func machineOutput(ctx context.Context, args ...string) ([]byte, error) {
	//nolint:gosec // G204: fixed argv. Every caller is a closure in
	// realMachineDeps passing literal subcommands and the two resource-floor
	// constants, so nothing caller-supplied reaches the argv.
	cmd := exec.CommandContext(ctx, podmanBin, append([]string{"machine"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			if msg := strings.TrimSpace(string(exitErr.Stderr)); msg != "" {
				return nil, fmt.Errorf("%w: %s", err, msg)
			}
		}
		return nil, err
	}
	return out, nil
}

// machineRun runs a `podman machine <args...>` mutation and discards its output,
// wrapping a failure with the combined output so the copy names why podman
// refused (init and start report their progress and their reasons there).
func machineRun(ctx context.Context, args ...string) error {
	//nolint:gosec // G204: fixed argv, same as machineOutput above.
	cmd := exec.CommandContext(ctx, podmanBin, append([]string{"machine"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// dialUnixSocket proves the forwarded podman API socket answers a connect. A
// stat would only prove the file exists, which a stale forward from a
// half-stopped machine also satisfies.
func dialUnixSocket(ctx context.Context, path string) error {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return err
	}
	// The connect itself is the whole signal; nothing is written or read, and a
	// close error on a socket we only probed is not actionable.
	_ = conn.Close()
	return nil
}
