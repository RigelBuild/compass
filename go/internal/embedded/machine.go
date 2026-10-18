// Package embedded contains the podman-machine probe and ensure step behind an injected seam.
// On macOS podman runs in a Linux VM, which a fresh machine must provision.
// OS is injected so every state can be tested on any host without podman.
// Parsing is defensive because podman CLI output is external and spike-verified assumptions remain.
package embedded

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

// Resource floors for four containers plus a second agent. These are reasoned
// values, not measured on macOS; see the self-host documentation.
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

// machineDeps inverts each podman CLI effect for deterministic tests.
type machineDeps struct {
	// list distinguishes an absent machine; stopped-machine inspect behavior is
	// unknown, so classification does not infer from its failure.
	list func(ctx context.Context) ([]byte, error)
	// inspect reports the default machine's state and host-side API socket.
	inspect func(ctx context.Context) ([]byte, error)
	// initMachine downloads a VM image on a fresh host and can take minutes.
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

// machineListEntry reads fields that vary across podman versions. List presence
// establishes existence; a mid-start machine classifies as stopped and start
// is a harmless no-op.
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

// errMachineUnclassified distinguishes unreadable state from states ensure can fix.
var errMachineUnclassified = errors.New("the podman machine state could not be determined")

// probeMachine lists first to distinguish absent from stopped, then inspects the
// default machine. CLI or parse errors never guess that it is ready.
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

// machineStateUnreadable is shared by probe and ensure for unknown state.
func machineStateUnreadable() error {
	return fmt.Errorf("%w; run `%s machine ls --format json` to see what podman reports",
		errMachineUnclassified, podmanBin)
}

// machineSocketReachable treats an empty or unreachable API socket on a running
// machine as not ready.
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

// ensureMachineReady provisions or starts the machine, then trusts a fresh
// probe rather than command exit status. The caller bounds the long init.
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

// machineReadyAdapter enables provisioning only on darwin. Injected GOOS lets
// linux tests verify macOS wiring without build-tagged dead code.
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
