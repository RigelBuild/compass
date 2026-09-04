//go:build unix

package microvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec" //nolint:depguard // microVM launcher: LookPath-resolved VMM/virtiofsd/passt guest-support processes (G204 sites justified below)
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// The host-controlled IPv4 plan passt offers the guest over its built-in DHCP
// server (§(c) lines 128-136): a fixed private /24, delivered as the lease the
// guest's in-process DHCP client (T2) consumes. The address plan is passt's
// launch flags (-a/-g/-n/-D), not a guest guess, so the host stays in control
// of addressing; DHCP is only the delivery mechanism. The 10.x block is a
// throwaway per-boot plan — nothing outside the guest routes it.
const (
	guestAddr   = "10.0.2.15" // -a: the address passt leases the guest
	guestGW     = "10.0.2.2"  // -g: the default gateway passt advertises
	guestPrefix = "24"        // -n: the netmask, as a prefix length
	guestDNS    = "10.0.2.3"  // -D: the resolver passt hands out
)

// socketReadyTimeout bounds the wait for virtiofsd's and passt's control
// sockets to appear before cloud-hypervisor is started against them. Both
// daemons create their AF_UNIX socket early in startup; a bounded poll (not an
// unbounded sleep) fails fast with a named cause if a daemon dies on launch
// instead of hanging the boot until the outer test deadline.
const socketReadyTimeout = 10 * time.Second

// reapGrace is how long Shutdown waits for a SIGTERM'd auxiliary daemon
// (virtiofsd/passt) to exit before escalating to SIGKILL.
const reapGrace = 5 * time.Second

// diagnosticTailBytes caps how much of a captured log (serial console, or a
// daemon's stderr) is surfaced in a failure message — enough to carry the cause
// (a kernel panic banner, a passt/CH negotiation error) without dumping a whole
// boot log into the test output.
const diagnosticTailBytes = 8 << 10

// launchOptions selects which virtio devices cloud-hypervisor is booted with.
// The full boot wires every device; the OQ-G net-only smoke omits virtio-fs and
// vsock so the passt×CH vhost-user-net negotiation — the spike's primary
// unknown (§(c)/OQ-G) — is exercised in isolation, and a negotiation failure
// surfaces from the serial console as itself rather than as an ambiguous
// full-stack timeout.
type launchOptions struct {
	withFS    bool
	withVsock bool
}

// child is one managed guest-support process: the *exec.Cmd plus the file its
// stdout+stderr are captured to, so Shutdown can reap it and a failure can
// surface its log.
type child struct {
	name    string
	cmd     *exec.Cmd
	logPath string
	waited  atomic.Bool // set once cmd.Wait has returned, so liveness probes and PSS skip a reaped process
}

// VM is a running (or partially-started, on the Launch error path) guest and
// its two supporting host daemons. It owns their process handles, the captured
// serial console + per-daemon logs, and the AF_UNIX sockets/pidfile that must
// be removed on teardown. The zero devices (virtiofsd nil under the net-only
// smoke) are tolerated by Shutdown and PSS.
type VM struct {
	vmm       *child // cloud-hypervisor
	virtiofsd *child // nil under the net-only smoke (no --fs)
	passt     *child

	// vmmExited is closed by the sole VMM reaper (started in launch) once the
	// cloud-hypervisor process has been Wait'd, so the caller can observe a
	// prompt guest self-power-off instead of a zombie-blind Signal(0) poll. Nil
	// only under the hermetic fail-closed path (no VMM on PATH).
	vmmExited chan struct{}

	consolePath string // --serial file: the guest serial console

	vsockSocket string // host end of the hybrid vsock (empty under the net-only smoke)
	vsockPort   uint32

	// Cleanup targets: the AF_UNIX sockets the daemons/VMM serve and passt's
	// pidfile. Removed by Shutdown after the processes are reaped.
	sockets []string
	pidfile string

	shutdownOnce sync.Once
	shutdownErr  error
}

// Launch boots one session guest: it starts virtiofsd and passt, waits for
// their control sockets, then starts cloud-hypervisor wired to both plus the
// hybrid vsock. It is the full-stack boot (virtio-fs + vsock + vhost-user-net).
// On any spawn failure it tears down everything already started before
// returning, so no child process is orphaned and no socket is leaked.
func Launch(ctx context.Context, cfg BootConfig) (*VM, error) {
	return launch(ctx, cfg, launchOptions{withFS: true, withVsock: true})
}

// LaunchNetOnly boots the guest with only the vhost-user-net device — no
// virtio-fs, no vsock (§(c)/OQ-G). It is the isolation smoke that exercises the
// passt×cloud-hypervisor vhost-user negotiation on its own: the guest brings its
// link up and acquires its DHCP lease (observable in the serial console) before
// the boot sequence fails-closed at the absent workspace mount. That failure is
// expected and irrelevant here — the smoke asserts only that the address landed,
// read from the console, so a negotiation failure is a named cause rather than a
// full-stack timeout.
func LaunchNetOnly(ctx context.Context, cfg BootConfig) (*VM, error) {
	return launch(ctx, cfg, launchOptions{withFS: false, withVsock: false})
}

// launch is the shared boot body. Binaries are resolved from PATH here (not
// carried on BootConfig) so the harness stays a pure boot contract and the test
// gate (microvmtest.Require) owns proving they are present; exec.LookPath
// against the same PATH yields the identical binaries Require resolved.
func launch(ctx context.Context, cfg BootConfig, opts launchOptions) (_ *VM, err error) {
	dir := filepath.Dir(cfg.Net.VhostUserSocket)
	// vm is a LOCAL, not the named return. The error sites below return an
	// explicit `nil, err`; were vm the named return, that nil would clobber the
	// handle before the deferred cleanup runs — nil-dereferencing in Shutdown
	// and orphaning any daemon already started. Keeping vm local means the defer
	// always sees the real, partially-built VM. (This is the fail-closed path CI
	// exercises when a daemon cannot sandbox on the runner and dies on launch.)
	vm := &VM{
		consolePath: filepath.Join(dir, "console.log"),
		vsockPort:   cfg.VsockPort,
	}
	if opts.withVsock {
		vm.vsockSocket = cfg.VsockSocket
	}
	// Tear down whatever started if a later spawn fails: no orphans, no leaked
	// sockets on the error path.
	defer func() {
		if err != nil {
			_ = vm.Shutdown(ctx) // best-effort cleanup on an already-failing launch
		}
	}()

	if opts.withFS {
		virtiofsdPath, lookErr := exec.LookPath("virtiofsd")
		if lookErr != nil {
			return nil, fmt.Errorf("microvm: resolving virtiofsd on PATH: %w", lookErr)
		}
		vm.virtiofsd = &child{
			name:    "virtiofsd",
			logPath: filepath.Join(dir, "virtiofsd.log"),
			//nolint:gosec // G204: the microVM harness seam — virtiofsdPath is LookPath-resolved and the argv is harness-built from BootConfig, neither user-controlled
			cmd: exec.CommandContext(ctx, virtiofsdPath,
				"--socket-path="+cfg.FSSocket,
				"--shared-dir="+cfg.FSSharedDir,
				"--sandbox=namespace"),
		}
		if startErr := startChild(vm.virtiofsd); startErr != nil {
			return nil, fmt.Errorf("microvm: starting virtiofsd: %w", startErr)
		}
		vm.sockets = append(vm.sockets, cfg.FSSocket)
	}

	passtPath, lookErr := exec.LookPath("passt")
	if lookErr != nil {
		return nil, fmt.Errorf("microvm: resolving passt on PATH: %w", lookErr)
	}
	vm.pidfile = filepath.Join(dir, "passt.pid")
	vm.passt = &child{
		name:    "passt",
		logPath: filepath.Join(dir, "passt.log"),
		// -f keeps passt in the foreground so this *exec.Cmd IS the passt
		// process (default is to daemonize, which would orphan it and make the
		// Cmd exit immediately). The -a/-g/-n/-D flags fix the host-controlled
		// address plan passt serves over DHCP (§(c)).
		//nolint:gosec // G204: the microVM harness seam — passtPath is LookPath-resolved and the argv is harness-built (fixed flags + BootConfig socket), neither user-controlled
		cmd: exec.CommandContext(ctx, passtPath,
			"--vhost-user",
			"--socket", cfg.Net.VhostUserSocket,
			"--pid", vm.pidfile,
			"-f",
			"-a", guestAddr,
			"-g", guestGW,
			"-n", guestPrefix,
			"-D", guestDNS),
	}
	if startErr := startChild(vm.passt); startErr != nil {
		return nil, fmt.Errorf("microvm: starting passt: %w", startErr)
	}
	vm.sockets = append(vm.sockets, cfg.Net.VhostUserSocket)

	// virtiofsd + passt must be serving before cloud-hypervisor connects to
	// their sockets. Bounded poll, not a fixed sleep.
	ready := []string{cfg.Net.VhostUserSocket}
	if opts.withFS {
		ready = append(ready, cfg.FSSocket)
	}
	if waitErr := waitForSockets(ctx, ready, socketReadyTimeout); waitErr != nil {
		return nil, fmt.Errorf("microvm: waiting for daemon sockets: %w", waitErr)
	}

	vmmPath, lookErr := exec.LookPath("cloud-hypervisor")
	if lookErr != nil {
		return nil, fmt.Errorf("microvm: resolving cloud-hypervisor on PATH: %w", lookErr)
	}
	vm.vmm = &child{
		name:    "cloud-hypervisor",
		logPath: filepath.Join(dir, "cloud-hypervisor.log"),
		//nolint:gosec // G204: the microVM harness seam — vmmPath is LookPath-resolved and vmmArgs is harness-built from BootConfig, neither user-controlled
		cmd: exec.CommandContext(ctx, vmmPath, vmmArgs(cfg, vm.consolePath, opts)...),
	}
	if startErr := startChild(vm.vmm); startErr != nil {
		return nil, fmt.Errorf("microvm: starting cloud-hypervisor: %w", startErr)
	}
	// The sole VMM reaper owns the single cmd.Wait for cloud-hypervisor: it
	// unblocks WaitVMMExit on a guest self-power-off and lets Shutdown observe
	// the exit without a second Wait. The Wait error is deliberately discarded —
	// a killed VMM yields an expected *exec.ExitError, mirroring waitResult.
	vm.vmmExited = make(chan struct{})
	go func() {
		_ = vm.vmm.cmd.Wait() // discard: a killed VMM's *exec.ExitError is the expected teardown outcome (mirrors waitResult)
		vm.vmm.waited.Store(true)
		close(vm.vmmExited)
	}()
	if opts.withVsock {
		vm.sockets = append(vm.sockets, cfg.VsockSocket)
	}
	return vm, nil
}

// vmmArgs builds the cloud-hypervisor argv exactly per the record (lines
// 542-547), dropping --fs/--vsock under the net-only smoke. Launch appends to
// the guest cmdline (per BootConfig.Cmdline's contract):
//   - console=ttyS0 so guestd's stderr reaches the captured serial console;
//   - compass.vsock_port=<port> which guestd reads from /proc/cmdline (last-wins,
//     so appending is safe even if cfg.Cmdline already carries one);
//   - net.ifnames=0 so the single virtio-net device is named eth0, the fixed
//     name guestd's netProvisioner binds (guestd/net.go: "cloud-hypervisor
//     presents the single virtio-net device as eth0"). The pinned generic
//     kernel defaults to predictable naming (enpNsM); without this the guest's
//     link is enp0s5 and guestd fail-closes with "Link not found". This is a
//     host-side boot parameter, not a guest change — guestd's eth0 contract is
//     frozen (T2), and the host is what must present that name.
func vmmArgs(cfg BootConfig, consolePath string, opts launchOptions) []string {
	cmdline := strings.TrimSpace(cfg.Cmdline +
		" console=ttyS0 net.ifnames=0 compass.vsock_port=" + strconv.FormatUint(uint64(cfg.VsockPort), 10))
	// compass.gateway_port carries the host-served AgentGateway port to guestd's
	// unix→vsock forwarder (record §(b)/§(d)); a zero port (a V2a-era boot or a
	// hermetic harness that starts no gateway) is omitted so the guest starts no
	// proxy and the V2b/V3 suites keep booting unchanged.
	if cfg.GatewayPort != 0 {
		cmdline += " compass.gateway_port=" + strconv.FormatUint(uint64(cfg.GatewayPort), 10)
	}
	args := []string{
		"--kernel", cfg.Kernel,
		"--initramfs", cfg.Initrd,
		"--disk", "path=" + cfg.Rootfs + ",readonly=on",
		"--cmdline", cmdline,
		"--cpus", "boot=" + strconv.Itoa(cfg.CPUs),
		"--memory", "size=" + strconv.Itoa(cfg.MemoryMB) + "M,shared=on",
		"--serial", "file=" + consolePath,
		"--console", "off",
	}
	if opts.withFS {
		args = append(args, "--fs", "tag="+cfg.FSTag+",socket="+cfg.FSSocket)
	}
	args = append(args, "--net", "vhost_user=true,socket="+cfg.Net.VhostUserSocket+",mac="+cfg.Net.MAC)
	if opts.withVsock {
		args = append(args, "--vsock", "cid="+strconv.FormatUint(uint64(cfg.VsockCID), 10)+",socket="+cfg.VsockSocket)
	}
	return args
}

// startChild installs the child's best-effort orphan guard via the
// platform-specific orphanGuardSysProcAttr (Linux PR_SET_PDEATHSIG, which fires
// on the spawning THREAD's death — so the spawn holds the OS thread for it to
// bind reliably; a no-op elsewhere) and captures its stdout+stderr to logPath so
// a boot failure can surface the daemon's own diagnostics. The real teardown
// guarantee is Shutdown, not Pdeathsig (record §(g) lines 300-303).
func startChild(c *child) error {
	logFile, err := os.OpenFile(c.logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("opening %s log %s: %w", c.name, c.logPath, err)
	}
	// The Cmd owns the fd for the child's lifetime; drop our copy once Start has
	// dup'd it into the child. Close after Start below.
	c.cmd.Stdout = logFile
	c.cmd.Stderr = logFile
	c.cmd.SysProcAttr = orphanGuardSysProcAttr()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	startErr := c.cmd.Start()
	// Our handle on the log file is no longer needed: Start dup'd it into the
	// child (on success) or it stays unused (on failure). Either way close it.
	_ = logFile.Close() // the child holds its own dup; our copy is done with
	if startErr != nil {
		return startErr
	}
	return nil
}

// waitForSockets polls until every path exists or the deadline elapses. A
// missing socket at the deadline is a named error naming the first path still
// absent, so a daemon that died on launch fails the boot fast.
func waitForSockets(ctx context.Context, paths []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		missing := ""
		for _, p := range paths {
			if _, err := os.Stat(p); err != nil {
				missing = p
				break
			}
		}
		if missing == "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("socket %s did not appear within %s", missing, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// Health dials the guest over the hybrid vsock (T3's client) and returns the
// unwrapped Health response. Not valid under the net-only smoke, which boots
// without a vsock device.
func (vm *VM) Health(ctx context.Context) (*compassv1.HealthResponse, error) {
	client := GuestClient(vm.vsockSocket, vm.vsockPort)
	resp, err := client.Health(ctx, connect.NewRequest(&compassv1.HealthRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// Shutdown tears the guest and its daemons down: the VMM is killed first (a VM
// gets no graceful drain), then virtiofsd and passt are reaped (SIGTERM, a
// bounded wait, then SIGKILL), each Wait'd to avoid zombies, and finally the
// AF_UNIX sockets and passt's pidfile are removed. It runs at most once (guarded
// by sync.Once) so it is safe to call explicitly AND from t.Cleanup. The serial
// console log is deliberately NOT removed — the test reads it after teardown.
func (vm *VM) Shutdown(ctx context.Context) error {
	vm.shutdownOnce.Do(func() {
		var errs []error
		// VMM first: kill outright, then let the sole reaper's single Wait
		// complete via vmmExited (Shutdown must not Wait the VMM itself — that
		// would be a second Wait on the same process).
		if vm.vmm != nil && vm.vmm.cmd.Process != nil {
			if killErr := vm.vmm.cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				errs = append(errs, fmt.Errorf("killing cloud-hypervisor: %w", killErr))
			}
			<-vm.vmmExited
		}
		// Then the auxiliary daemons: SIGTERM, bounded wait, SIGKILL.
		for _, c := range []*child{vm.virtiofsd, vm.passt} {
			if c == nil || c.cmd.Process == nil {
				continue
			}
			if reapErr := reap(c); reapErr != nil {
				errs = append(errs, reapErr)
			}
		}
		// Remove the sockets and pidfile now that nothing is serving them.
		for _, s := range vm.sockets {
			if rmErr := os.Remove(s); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("removing socket %s: %w", s, rmErr))
			}
		}
		if vm.pidfile != "" {
			if rmErr := os.Remove(vm.pidfile); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("removing pidfile %s: %w", vm.pidfile, rmErr))
			}
		}
		vm.shutdownErr = errors.Join(errs...)
	})
	return vm.shutdownErr
}

// reap terminates an auxiliary daemon gracefully then forcibly: SIGTERM, wait up
// to reapGrace, SIGKILL if it is still alive, then Wait to collect the exit and
// avoid a zombie.
func reap(c *child) error {
	if termErr := c.cmd.Process.Signal(syscall.SIGTERM); termErr != nil && !errors.Is(termErr, os.ErrProcessDone) {
		return fmt.Errorf("SIGTERM %s: %w", c.name, termErr)
	}
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case err := <-done:
		c.waited.Store(true)
		return waitResult(c.name, err)
	case <-time.After(reapGrace):
		if killErr := c.cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return fmt.Errorf("SIGKILL %s: %w", c.name, killErr)
		}
		c.waited.Store(true)
		return waitResult(c.name, <-done)
	}
}

// WaitVMMExit reports whether the VMM process exited within timeout, observed
// via the reaper (not a zombie-blind Signal(0) poll): a guest that powers itself
// off makes the reaper's Wait return and close vmmExited promptly, so the caller
// sees the self-exit instead of burning the full grace window on a zombie.
func (vm *VM) WaitVMMExit(timeout time.Duration) bool {
	if vm.vmm == nil || vm.vmmExited == nil {
		return true
	}
	select {
	case <-vm.vmmExited:
		return true
	case <-time.After(timeout):
		return false
	}
}

// waitResult swallows the ExitError a deliberately-killed process yields (a
// signalled or non-zero exit is expected on teardown) but propagates a genuine
// wait failure.
func waitResult(name string, err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil // killed/non-zero exit is the expected teardown outcome
	}
	return fmt.Errorf("waiting for %s: %w", name, err)
}

// Running reports whether the named child's process is still alive. It is used
// by the test to assert Shutdown left no orphan. A process that has been Wait'd
// is definitively gone; otherwise signal 0 probes liveness without affecting it.
func (vm *VM) Running(name string) bool {
	c := vm.childByName(name)
	if c == nil || c.cmd.Process == nil || c.waited.Load() {
		return false
	}
	return c.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// PSS returns each live process's proportional set size in kB, read from
// /proc/<pid>/smaps_rollup and keyed by process name. It is PSS, NOT summed
// VmHWM: under --memory shared=on the guest RAM is one shared mapping all three
// processes map, so VmHWM counts those pages in each and summing double/triple-
// counts; PSS divides shared pages among their mappers (record §(g) lines
// 292-296). The unit is kB, as smaps_rollup reports it.
func (vm *VM) PSS() (map[string]int64, error) {
	out := make(map[string]int64)
	var errs []error
	for _, c := range []*child{vm.vmm, vm.virtiofsd, vm.passt} {
		if c == nil || c.cmd.Process == nil || c.waited.Load() {
			continue
		}
		pss, err := readPSS(c.cmd.Process.Pid)
		if err != nil {
			// Best-effort: a sandboxed helper makes its own smaps_rollup
			// unreadable to the rootless harness — passt sets PR_SET_DUMPABLE=0,
			// which reparents /proc/<pid>/smaps_rollup to root and denies the
			// non-root reader — and an already-exited process's proc entry is
			// gone. Both are expected and leave no entry rather than failing:
			// PSS is informational spike output (record §(g)), not a boot gate.
			if errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) {
				continue
			}
			errs = append(errs, fmt.Errorf("reading PSS for %s (pid %d): %w", c.name, c.cmd.Process.Pid, err))
			continue
		}
		out[c.name] = pss
	}
	return out, errors.Join(errs...)
}

// readPSS parses the Pss line (kB) from a process's smaps_rollup.
func readPSS(pid int) (int64, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/smaps_rollup")
	if err != nil {
		return 0, fmt.Errorf("reading smaps_rollup: %w", err)
	}
	for line := range strings.Lines(string(raw)) {
		rest, ok := strings.CutPrefix(line, "Pss:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest) // "  1234 kB" -> ["1234", "kB"]
		if len(fields) < 1 {
			return 0, fmt.Errorf("malformed Pss line %q", strings.TrimSpace(line))
		}
		kb, parseErr := strconv.ParseInt(fields[0], 10, 64)
		if parseErr != nil {
			return 0, fmt.Errorf("parsing Pss value %q: %w", fields[0], parseErr)
		}
		return kb, nil
	}
	return 0, errors.New("no Pss line in smaps_rollup")
}

// ConsoleTail returns the tail of the captured guest serial console, so a boot
// failure surfaces the guest's own diagnostics (a guestd fail-closed line, a
// kernel panic) instead of an opaque deadline timeout.
func (vm *VM) ConsoleTail() string {
	return tailFile(vm.consolePath)
}

// Diagnostics returns the tails of the serial console and every daemon's
// captured stderr, the evidence set for an OQ-G negotiation failure (passt/CH
// vhost-user) or any other boot abort.
func (vm *VM) Diagnostics() string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== guest serial console (%s) ===\n%s\n", vm.consolePath, tailFile(vm.consolePath))
	for _, c := range []*child{vm.vmm, vm.passt, vm.virtiofsd} {
		if c == nil {
			continue
		}
		fmt.Fprintf(&b, "=== %s (%s) ===\n%s\n", c.name, c.logPath, tailFile(c.logPath))
	}
	return b.String()
}

// tailFile returns up to diagnosticTailBytes from the end of path, or a short
// note if it cannot be read.
func tailFile(path string) string {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is a harness-owned capture-log path (console/daemon stderr under the test temp dir), not user input
	if err != nil {
		return fmt.Sprintf("<unreadable: %v>", err)
	}
	if len(raw) > diagnosticTailBytes {
		raw = raw[len(raw)-diagnosticTailBytes:]
	}
	return string(raw)
}

func (vm *VM) childByName(name string) *child {
	switch name {
	case "cloud-hypervisor":
		return vm.vmm
	case "virtiofsd":
		return vm.virtiofsd
	case "passt":
		return vm.passt
	default:
		return nil
	}
}
