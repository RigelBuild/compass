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
	"syscall"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// passt leases a fixed private /24 via DHCP from its launch flags. The
// throwaway per-boot subnet is not routed outside the guest.
const (
	guestAddr   = "10.0.2.15" // -a: the address passt leases the guest
	guestGW     = "10.0.2.2"  // -g: the default gateway passt advertises
	guestPrefix = "24"        // -n: the netmask, as a prefix length
	guestDNS    = "10.0.2.3"  // -D: the resolver passt hands out
)

// socketReadyTimeout bounds socket startup polling so a daemon that dies early
// returns a named error instead of hanging until the outer deadline.
const socketReadyTimeout = 10 * time.Second

// reapGrace is how long Shutdown waits for a SIGTERM'd auxiliary daemon
// (virtiofsd/passt) to exit before escalating to SIGKILL.
const reapGrace = 5 * time.Second

// diagnosticTailBytes limits failure output while retaining the daemon or
// kernel error that explains the boot failure.
const diagnosticTailBytes = 8 << 10

// modcapsDropMknod is shared with the argv assertion because virtiofsd silently
// accepts unknown capability names; the test is the only typo guard.
const modcapsDropMknod = "--modcaps=-mknod"

// launchOptions selects boot devices. The net-only smoke omits fs and vsock to
// isolate vhost-user negotiation from full-stack boot failures.
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

	// exited is closed by the sole reaper after cmd.Wait. Readiness, Shutdown,
	// Running, and PSS observe this channel instead of calling Wait again.
	exited chan struct{}
	// waitErr is cmd.Wait's error, written by the reaper BEFORE it closes
	// exited. Reading it is only safe after a receive from exited, which is the
	// happens-before edge that makes the unsynchronized field race-free.
	waitErr error
}

// hasExited reports whether the child's reaper has already observed its exit,
// without blocking. It is NOT a signal-0 probe: an exited-but-unwaited process
// is a zombie that Signal(0) reports as alive, which is precisely why the
// liveness check reads the reaper's channel instead.
func (c *child) hasExited() bool {
	select {
	case <-c.exited:
		return true
	default:
		return false
	}
}

// VM is a running (or partially-started, on the Launch error path) guest and
// its two supporting host daemons. It owns their process handles, the captured
// serial console + per-daemon logs, and the AF_UNIX sockets/pidfiles that must
// be removed on teardown. The zero devices (virtiofsd nil under the net-only
// smoke) are tolerated by Shutdown and PSS.
type VM struct {
	vmm       *child // cloud-hypervisor
	virtiofsd *child // nil under the net-only smoke (no --fs)
	passt     *child

	// vmmExited is the VMM child's reaper channel (vm.vmm.exited), hoisted onto
	// the VM so WaitVMMExit can observe a prompt guest self-power-off instead of
	// polling a zombie. Nil only under the hermetic fail-closed path (no VMM on
	// PATH), where no VMM child was ever started.
	vmmExited chan struct{}

	consolePath string // --serial file: the guest serial console

	vsockSocket string // host end of the hybrid vsock (empty under the net-only smoke)
	vsockPort   uint32

	// Cleanup targets: the AF_UNIX sockets and the three host-written pidfiles
	// (§(a)), removed by Shutdown after reap. A pidfile path is appended by
	// startRecordedChild BEFORE its first write, so a boot that fails mid-record
	// still has its record cleaned up by the deferred Shutdown.
	sockets  []string
	pidfiles []string

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

// LaunchNetOnly boots without virtio-fs or vsock to isolate passt/CH vhost-user
// negotiation. It checks DHCP address acquisition before the expected mount
// failure, making network errors visible without a full-stack timeout.
func LaunchNetOnly(ctx context.Context, cfg BootConfig) (*VM, error) {
	return launch(ctx, cfg, launchOptions{withFS: false, withVsock: false})
}

// launch resolves binaries from PATH so BootConfig remains a pure boot contract;
// microvmtest.Require verifies those same binaries for the test harness.
func launch(ctx context.Context, cfg BootConfig, opts launchOptions) (_ *VM, err error) {
	console, err := serialConsole(runtime.GOARCH)
	if err != nil {
		return nil, fmt.Errorf("microvm: selecting serial console: %w", err)
	}
	dir := filepath.Dir(cfg.Net.VhostUserSocket)
	// Keep vm local: returning nil through the named result would clobber the
	// partially-started VM before deferred Shutdown could reap its daemons.
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
			cmd: exec.CommandContext(ctx, virtiofsdPath, virtiofsdArgs(cfg)...),
		}
		if startErr := vm.startRecordedChild(vm.virtiofsd, dir, "virtiofsd.pid"); startErr != nil {
			return nil, startErr
		}
		vm.sockets = append(vm.sockets, cfg.FSSocket)
	}

	passtPath, lookErr := exec.LookPath("passt")
	if lookErr != nil {
		return nil, fmt.Errorf("microvm: resolving passt on PATH: %w", lookErr)
	}
	vm.passt = &child{
		name:    "passt",
		logPath: filepath.Join(dir, "passt.log"),
		// -f keeps passt as this process; its DHCP flags set the host-controlled
		// address plan, and --no-map-gw prevents guest access to host listeners.

		// NO --pid: passt's pidfile is retired (§(a)). -f makes this Cmd the passt
		// process, so the host knows its pid and carries the starttime+boot-id
		// reuse defense. One writer, one format, three files.
		//nolint:gosec // G204: the microVM harness seam — passtPath is LookPath-resolved and the argv is harness-built (fixed flags + BootConfig socket), neither user-controlled
		cmd: exec.CommandContext(ctx, passtPath,
			"--vhost-user",
			"--socket", cfg.Net.VhostUserSocket,
			"-f",
			"-a", guestAddr,
			"-g", guestGW,
			"-n", guestPrefix,
			"-D", guestDNS,
			"--no-map-gw"),
	}
	if startErr := vm.startRecordedChild(vm.passt, dir, "passt.pid"); startErr != nil {
		return nil, startErr
	}
	vm.sockets = append(vm.sockets, cfg.Net.VhostUserSocket)

	// virtiofsd + passt must be serving before cloud-hypervisor connects.
	// Bounded poll, LIVENESS-AWARE not path-existence: virtiofsd binds its socket
	// BEFORE the sandbox setup that can fail, so a sandbox failure leaves the
	// socket behind a dead daemon and a path-only poll would launch against a corpse.
	waiting := []*child{vm.passt}
	ready := []string{cfg.Net.VhostUserSocket}
	if opts.withFS {
		waiting = append(waiting, vm.virtiofsd)
		ready = append(ready, cfg.FSSocket)
	}
	if waitErr := waitForSockets(ctx, ready, waiting, socketReadyTimeout); waitErr != nil {
		return nil, fmt.Errorf("microvm: waiting for daemon sockets: %w", waitErr)
	}
	// Belt-and-braces over the poll above: a daemon that dies in the window
	// between the last poll iteration and here must not be handed to the VMM.
	for _, c := range waiting {
		if c.hasExited() {
			return nil, fmt.Errorf("microvm: %w",
				deathError(c, phaseVMMStart))
		}
	}

	vmmPath, lookErr := exec.LookPath("cloud-hypervisor")
	if lookErr != nil {
		return nil, fmt.Errorf("microvm: resolving cloud-hypervisor on PATH: %w", lookErr)
	}
	vm.vmm = &child{
		name:    "cloud-hypervisor",
		logPath: filepath.Join(dir, "cloud-hypervisor.log"),
		//nolint:gosec // G204: the microVM harness seam — vmmPath is LookPath-resolved and vmmArgs is harness-built from BootConfig, neither user-controlled
		cmd: exec.CommandContext(ctx, vmmPath, vmmArgs(cfg, vm.consolePath, console, opts)...),
	}
	if startErr := vm.startRecordedChild(vm.vmm, dir, "vmm.pid"); startErr != nil {
		return nil, startErr
	}
	// startChild installed the sole reaper that owns cloud-hypervisor's single
	// cmd.Wait; hoist its channel onto the VM so WaitVMMExit observes a guest
	// self-power-off promptly instead of polling a zombie.
	vm.vmmExited = vm.vmm.exited
	if opts.withVsock {
		vm.sockets = append(vm.sockets, cfg.VsockSocket)
	}
	return vm, nil
}

// virtiofsdArgs builds the socket/share/sandbox argv and appends ownership
// translation. It drops CAP_MKNOD because a workspace needs no device nodes;
// the shared argv assertion catches typos virtiofsd silently accepts.
func virtiofsdArgs(cfg BootConfig) []string {
	translate := virtiofsdTranslateArgs(cfg.AgentUID)
	args := make([]string, 0, 4+len(translate))
	args = append(args,
		"--socket-path="+cfg.FSSocket,
		"--shared-dir="+cfg.FSSharedDir,
		"--sandbox=namespace",
		modcapsDropMknod,
	)
	return append(args, translate...)
}

// virtiofsdTranslateArgs maps guest agent ids to the invoking host's ids,
// preserving share ownership; zero leaves the V2a harness untranslated. The
// translation flags avoid namespace-root capabilities but disable POSIX ACLs.
func virtiofsdTranslateArgs(agentUID uint32) []string {
	if agentUID == 0 {
		return nil
	}
	agent := strconv.FormatUint(uint64(agentUID), 10)
	return []string{
		"--translate-uid", translateSpec(agent, strconv.Itoa(os.Getuid())),
		"--translate-gid", translateSpec(agent, strconv.Itoa(os.Getgid())),
	}
}

// translateSpec renders one bidirectional, one-id-wide virtiofsd translation in
// its `map:<guest id>:<host id>:<count>` form.
func translateSpec(guestID, hostID string) string {
	return "map:" + guestID + ":" + hostID + ":1"
}

// serialConsole returns the guest UART device for the requested architecture.
func serialConsole(goarch string) (string, error) {
	switch goarch {
	case "amd64":
		return "ttyS0", nil
	case "arm64":
		return "ttyAMA0", nil
	default:
		return "", fmt.Errorf("unsupported microVM architecture %q", goarch)
	}
}

// vmmArgs builds cloud-hypervisor argv, omitting fs/vsock for the net-only smoke.
// It appends console, vsock-port, and net.ifnames=0: guestd binds eth0, while
// the generic kernel otherwise names the single virtio-net interface enpNsM.
func vmmArgs(cfg BootConfig, consolePath, console string, opts launchOptions) []string {
	cmdline := strings.TrimSpace(
		cfg.Cmdline + " console=" + console + " net.ifnames=0 compass.vsock_port=" +
			strconv.FormatUint(uint64(cfg.VsockPort), 10),
	)
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

// startChild installs the platform orphan guard, captures logs, and starts the
// sole cmd.Wait reaper. Linux PDEATHSIG fires on the spawning thread's death, so
// Start holds the OS thread; Shutdown remains the real teardown guarantee.
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
	_ = logFile.Close() // deliberate: the child holds its own dup of the fd, so a close error on our spent copy is not actionable
	if startErr != nil {
		return startErr
	}
	c.exited = make(chan struct{})
	go func() {
		// The sole reaper writes waitErr before closing exited, so receivers see
		// the result. Teardown filters ExitError; startup reports it via deathError.
		c.waitErr = c.cmd.Wait()
		close(c.exited)
	}()
	return nil
}

// waitForSockets requires both socket existence and live daemons. virtiofsd may
// bind before sandbox setup fails, leaving a stale socket that would mask its
// real log cause; missing sockets also produce a named timeout error.
func waitForSockets(ctx context.Context, paths []string, waiting []*child, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		// Liveness first: a dead daemon's leftover socket must not read as
		// readiness, so this check precedes the Stat sweep.
		for _, c := range waiting {
			if c.hasExited() {
				return deathError(c, phaseSocket)
			}
		}
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
	resp, err := client.Health(ctx, &compassv1.HealthRequest{})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Shutdown tears the guest and its daemons down: the VMM is killed first (a VM
// gets no graceful drain), then virtiofsd and passt are reaped (SIGTERM, a
// bounded wait, then SIGKILL), each Wait'd to avoid zombies, and finally the
// AF_UNIX sockets and the three pidfiles are removed. It runs at most once
// (guarded by sync.Once) so it is safe to call explicitly AND from t.Cleanup.
// ctx bounds only the SIGTERM grace: once it is done the daemons are SIGKILLed
// at once, but every child is still reaped, so teardown never returns early.
// The serial console log is deliberately NOT removed — the test reads it after
// teardown.
func (vm *VM) Shutdown(ctx context.Context) error {
	vm.shutdownOnce.Do(func() {
		var errs []error
		// VMM first: kill outright, then let the sole reaper's single Wait
		// complete via vmmExited (Shutdown must not Wait the VMM itself — that
		// would be a second Wait on the same process).

		// The RECEIVE carries its own nil guard: vmmExited is hoisted onto the VM
		// only AFTER startRecordedChild returns, so a failure once the VMM is
		// spawned leaves a live handle beside a NIL channel, and receiving on it
		// would block FOREVER. Same condition WaitVMMExit guards on.

		// The guard is on the receive alone and NOT on the arm: such a process
		// is started, live, and unrecorded, so skipping the Kill would orphan
		// precisely the child the deferred Shutdown exists to clean up. Its
		// own reaper still owns the Wait, so nothing is left unreaped.
		if vm.vmm != nil && vm.vmm.cmd.Process != nil {
			if killErr := vm.vmm.cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				errs = append(errs, fmt.Errorf("killing cloud-hypervisor: %w", killErr))
			}
			if vm.vmmExited != nil {
				<-vm.vmmExited
			}
		}
		// Then the auxiliary daemons: SIGTERM, bounded wait, SIGKILL.
		for _, c := range []*child{vm.virtiofsd, vm.passt} {
			if c == nil || c.cmd.Process == nil {
				continue
			}
			if reapErr := reap(ctx, c); reapErr != nil {
				errs = append(errs, reapErr)
			}
		}
		// Remove the sockets and pidfiles now that nothing is serving them.
		for _, s := range vm.sockets {
			if rmErr := os.Remove(s); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("removing socket %s: %w", s, rmErr))
			}
		}
		for _, p := range vm.pidfiles {
			if rmErr := os.Remove(p); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("removing pidfile %s: %w", p, rmErr))
			}
		}
		vm.shutdownErr = errors.Join(errs...)
	})
	return vm.shutdownErr
}

// reap terminates an auxiliary daemon gracefully then forcibly: SIGTERM, wait up
// to reapGrace or until ctx is done, then SIGKILL if it is still alive. It does
// NOT Wait — startChild's sole reaper owns this child's single cmd.Wait, and
// reap observes the exit through c.exited, so no second Wait can race it.
func reap(ctx context.Context, c *child) error {
	if termErr := c.cmd.Process.Signal(syscall.SIGTERM); termErr != nil && !errors.Is(termErr, os.ErrProcessDone) {
		return fmt.Errorf("SIGTERM %s: %w", c.name, termErr)
	}
	grace := time.NewTimer(reapGrace)
	defer grace.Stop()
	select {
	case <-c.exited:
		return waitResult(c.name, c.waitErr)
	case <-ctx.Done():
	case <-grace.C:
	}
	if killErr := c.cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		return fmt.Errorf("SIGKILL %s: %w", c.name, killErr)
	}
	<-c.exited
	return waitResult(c.name, c.waitErr)
}

// WaitVMMExit reports whether the VMM process exited within timeout, observed
// via the reaper (not a zombie-blind Signal(0) poll): a guest that powers itself
// off makes the reaper's Wait return and close vmmExited promptly, so the caller
// sees the self-exit instead of burning the full grace window on a zombie.
func (vm *VM) WaitVMMExit(timeout time.Duration) bool {
	if vm.vmm == nil || vm.vmmExited == nil {
		return true
	}
	// An already-exited VMM must win over a zero timeout: select picks randomly
	// between ready cases, so a bare timed select could report a dead VMM alive.
	select {
	case <-vm.vmmExited:
		return true
	default:
	}
	if timeout <= 0 {
		return false
	}
	select {
	case <-vm.vmmExited:
		return true
	case <-time.After(timeout):
		return false
	}
}

// waitResult swallows the outcomes a teardown expects (a signalled or non-zero
// exit, or the launch ctx already done) but propagates a genuine wait failure.
func waitResult(name string, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
		return nil // killed/non-zero exit is the expected teardown outcome
	}
	// exec.Cmd.Wait returns ctx.Err() when the process exited cleanly after its ctx was done.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return fmt.Errorf("waiting for %s: %w", name, err)
}

// deathCause renders why a child died during startup. Do not use waitResult:
// it suppresses ExitError as expected teardown, but startup ExitError is the
// cause and wrapping nil would render %!w(<nil>).
func deathCause(c *child) string {
	if c.waitErr == nil {
		return "exited cleanly (status 0) without serving"
	}
	return c.waitErr.Error()
}

// startupPhase names what a child failed to reach before it died.
type startupPhase string

const (
	phaseVMMStart startupPhase = "cloud-hypervisor was started"
	phaseSocket   startupPhase = "its socket was serving"
	phasePidfile  startupPhase = "its pidfile could be recorded"
)

// allStartupPhases returns a fresh slice for the guard test. Keep it in sync
// with the constants; phaseText cannot detect a phase omitted from this list.
func allStartupPhases() []startupPhase {
	return []startupPhase{phaseVMMStart, phaseSocket, phasePidfile}
}

// phaseText renders a phase; the switch lets exhaustive catch unhandled values.
// Keep the fallback outside the switch because the repo's default-signifies-
// exhaustive setting would disable that check if a default arm were added.
func phaseText(phase startupPhase) string {
	switch phase {
	case phaseVMMStart, phaseSocket, phasePidfile:
		return string(phase)
	}
	return string(phase)
}

// deathError builds the startup error for an exited child; all startup sites use
// it so daemon cause and log tail stay consistent.
func deathError(c *child, phase startupPhase) error {
	return fmt.Errorf("%s exited before %s: %s; log tail:\n%s",
		c.name, phaseText(phase), deathCause(c), tailFile(c.logPath))
}

// Running reports whether the named child's process is still alive. It is used
// by the test to assert Shutdown left no orphan. A child whose sole reaper has
// already observed the exit is definitively gone — and that check must come
// FIRST, because an exited-but-unreaped process is a zombie that Signal(0)
// reports as alive; otherwise signal 0 probes liveness without affecting it.
func (vm *VM) Running(name string) bool {
	c := vm.childByName(name)
	if c == nil || c.cmd.Process == nil || c.hasExited() {
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
		if c == nil || c.cmd.Process == nil || c.hasExited() {
			continue
		}
		pss, err := readPSS(c.cmd.Process.Pid)
		if err != nil {
			// Best-effort: a sandboxed helper makes its smaps_rollup unreadable
			// (passt sets PR_SET_DUMPABLE=0, reparenting it to root), and an
			// exited process's proc entry is gone. Both are expected and leave no
			// entry — PSS is informational (record §(g)), not a boot gate.
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

// startRecordedChild writes the intent before spawning and the settled identity
// afterward. Either write failing aborts boot so no potentially-live child goes
// unrecorded; deferred Shutdown removes registered paths.
func (vm *VM) startRecordedChild(c *child, dir, pidfileName string) error {
	path := filepath.Join(dir, pidfileName)
	// Registered before the first write, not after the last: the intent record
	// exists precisely to survive a crash between the two writes, so Shutdown
	// must already know to remove it if the spawn in between fails.
	vm.pidfiles = append(vm.pidfiles, path)
	if err := writePidIntent(path); err != nil {
		return fmt.Errorf("microvm: recording %s pid intent: %w", c.name, err)
	}
	if err := startChild(c); err != nil {
		return fmt.Errorf("microvm: starting %s: %w", c.name, err)
	}
	if err := writePidfile(path, c.cmd.Process.Pid); err != nil {
		return pidfileWriteError(c, err)
	}
	return nil
}

// pidfileWriteError reclassifies a failed settled write only when /proc says the
// process is gone AND its sole reaper confirms exit. Other failures remain
// fatal; the intent record stays registered for Shutdown cleanup.
func pidfileWriteError(c *child, err error) error {
	if procReadMeansGone(err) && c.hasExited() {
		return fmt.Errorf("microvm: %w",
			deathError(c, phasePidfile))
	}
	return fmt.Errorf("microvm: recording %s pidfile: %w", c.name, err)
}
