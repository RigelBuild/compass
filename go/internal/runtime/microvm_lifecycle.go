//go:build unix

package runtime

// microvm_lifecycle.go fills the eight MicroVMRuntime lifecycle verbs behind the
// frozen ContainerRuntime signatures (microvm.go holds the type + config +
// SelectBackend). It is //go:build unix because the microvm package it drives
// (Launch/GuestExec/VM/GuestClient, all //go:build unix) is unix-only; keeping
// the bodies here lets the untagged runtime package still type-check backend
// selection on any platform.
//
// The design (record §(c)/(d)/(e)) translates each container verb onto V2a's
// boot harness plus the U3 GuestExec layer: Create allocates a session without
// booting (mirroring `podman create`), Start boots + Health-polls + nonce-binds
// + Provisions transactionally, Exec/ExecStreaming map the spec onto GuestExec,
// Stop is graceful-then-kill via the guest Signal RPC, and Remove is an
// idempotent teardown. The load-bearing invariants: the mutex-guarded session
// table, Start's tear-down-on-any-failure posture, and ExecStreaming's waitFunc
// constructing a *runtime.ExitStatusError for a signalled exit so the runner's
// isDeliberateKill recognizes a deliberate kill (OQ-G/U3b).

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/runtime/microvm"
)

// guestVsockCID is the fixed guest context id every session VM boots with. One
// VMM per session means CID uniqueness only matters per-host for observability;
// the hybrid transport addresses by socket path, not CID, so nothing routes on
// it (OQ-F, record §(c)). CIDs 0-2 are reserved, so 3 is the first usable.
const guestVsockCID uint32 = 3

// guestVsockPort is the fixed port guestd serves the control plane on inside
// every session VM. Per-session uniqueness is carried entirely by the AF_UNIX
// socket paths under the session runtime dir, never the port (OQ-F). This
// matches the value the V2a boot harness tests boot guestd on (testVsockPort).
const guestVsockPort uint32 = 1024

// agentGatewayVsockPort is the fixed guest port the host serves the per-session
// AgentGateway on, mirroring guestVsockPort's identity model: per-session
// uniqueness rides the suffixed AF_UNIX path under the runtime dir
// (microvm.GatewaySocketPath), never the port, so every VM boots with the same
// value (record §(e), OQ-4). Distinct from guestVsockPort (1024, the control
// plane) so the two guest-initiated channels never share a suffix base.
const agentGatewayVsockPort uint32 = 1025

// sunPathMax is the longest AF_UNIX path the kernel accepts: sockaddr_un's
// sun_path holds the path plus a NUL terminator. It mirrors the runner
// gateway's own budget check (gateway/socket.go); Create uses it to reject an
// over-long suffixed gateway socket path BEFORE booting a VM, rather than at
// the post-Launch gateway.Serve bind after a full ~60s boot (record §(e)).
const sunPathMax = len(syscall.RawSockaddrUnix{}.Path) - 1

// workspaceFSTag is the virtio-fs tag the single read-write workspace share is
// exported under — the tag guestd mounts at /workspace (config.go FSTag).
const workspaceFSTag = "workspace"

// workspaceMountPath is the guest path guestd always mounts the single
// workspace share at (/workspace). A single mount targeting any other
// ContainerPath is refused rather than silently remapped here (OQ-C: refuse,
// don't drop the target path).
const workspaceMountPath = "/workspace"

// guestMAC is the per-session-fixed MAC handed to the guest virtio-net device.
// One VM per session with its own network namespace means the MAC need not be
// unique across sessions; a fixed value keeps Create allocation-free here.
const guestMAC = "12:34:56:78:9a:bc"

// bootDeadline bounds Start's Launch→Health-OK window when the caller's ctx
// carries no deadline of its own — the V2a full-boot budget (record §T4: 60s).
const bootDeadline = 60 * time.Second

// healthPollInterval is how often Start re-probes Health while waiting for the
// guest to report net_provisioned && workspace_mounted. A short interval keeps
// boot latency observation tight without busy-spinning the vsock.
const healthPollInterval = 200 * time.Millisecond

// execDefaultTimeout is the per-command wall-clock cap Exec enforces host-side
// (a ctx deadline) and mirrors guest-side (ExecCall.TimeoutSeconds), matching
// PodmanCLI's defaultCommandTimeout posture: a wedged child must surface as a
// timeout error, never block the calling task forever.
const execDefaultTimeout = 120 * time.Second

// guestVM is the running-guest handle MicroVMRuntime drives, an interface over
// *microvm.VM so Start is hermetically testable behind a fake handle. Its method
// set is NOT merely what Start calls — it retypes the shared microvmSession.vm
// field, so it must cover EVERY method invoked on that field anywhere in the
// package across ALL build tags: Health (awaitHealthy's poll in Start),
// Shutdown (Start's defer, Stop, and Remove), WaitVMMExit (Stop,
// microvm_lifecycle.go), and PSS (the Q-budget contract test's session.vm.PSS(),
// contract_microvm_test.go, //go:build microvm && unix). All in launch.go except
// where noted. *microvm.VM satisfies all four as-is (design §W2 seams).
type guestVM interface {
	Health(ctx context.Context) (*compassv1.HealthResponse, error)
	Shutdown(ctx context.Context) error
	WaitVMMExit(timeout time.Duration) bool
	PSS() (map[string]int64, error)
}

// guestLaunchFunc boots a session guest, returning it behind the guestVM seam.
// It defaults to a thin adapter over microvm.Launch (installSeamDefaults) and is
// overridden in hermetic Start tests so no real VMM boots.
type guestLaunchFunc func(context.Context, microvm.BootConfig) (guestVM, error)

// guestClientFunc dials the guest control plane, returning a GuestControlClient.
// It defaults to microvm.GuestClient (installSeamDefaults) and is overridden in
// hermetic Start tests so a fake client answers Provision with no real vsock
// dial. It seams Start's Provision client ONLY; Stop's own dial stays direct.
type guestClientFunc func(socket string, port uint32) compassv1internalconnect.GuestControlClient

// installSeamDefaults wires the production launch + client implementations onto a
// freshly constructed MicroVMRuntime. It is //go:build unix (like the microvm
// package it names) and is called by NewMicroVMRuntime, so hermetic tests can
// override the seams after construction.
func (m *MicroVMRuntime) installSeamDefaults() {
	m.launchFunc = func(ctx context.Context, cfg microvm.BootConfig) (guestVM, error) {
		vm, err := microvm.Launch(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return vm, nil
	}
	m.newGuestClient = microvm.GuestClient
}

// microvmSession is one allocated microVM session's state. Created by Create
// (not yet booted), populated with the running VM handle and exec client by
// Start, and dropped by Remove. All fields are read/written under
// MicroVMRuntime.mu.
type microvmSession struct {
	// id is the ContainerID Create minted (also the runtime-dir leaf name).
	id ContainerID
	// name is spec.Name — the Runner's stable handle, answered by Exists and
	// used to refuse a duplicate-name Create (matching podman's engine).
	name string
	// cfg is the assembled BootConfig Start boots from.
	cfg microvm.BootConfig
	// uid and env are recorded from the spec at Create for the Provision RPC
	// Start issues (default_exec_uid + base_env).
	uid uint32
	env map[string]string
	// nonce is the per-session boot nonce (raw bytes); its hex encoding rides
	// the cmdline, and Start verifies guestd echoes it before opening the gate.
	nonce []byte
	// nftScript is the egress ruleset delivered to guestd on Start's Provision
	// RPC (as ProvisionRequest.nft_script). Recorded at Create from
	// spec.Egress.NftScript(); NEVER empty for a ContainerSpec-created session,
	// since the zero-value EgressPolicy still emits the full default-deny base
	// ruleset (design §(e), egress.go). guestd arms it as guest root before the
	// exec gate opens.
	nftScript string
	// runtimeDir is <RunRoot>/microvm/<id>/, holding the session's sockets.
	runtimeDir string
	// vm and guestExec are nil until Start boots the guest; Start sets both
	// under the lock once the boot + Provision succeed. vm is typed as the
	// unexported guestVM interface (not *microvm.VM directly) so Start is
	// hermetically testable behind a fake handle (design §W2 seams).
	vm        guestVM
	guestExec *microvm.GuestExec
}

// DuplicateNameError is a Create refused because a session with the same
// spec.Name already exists — matching podman's engine rejecting a second
// container of the same name, which createAndStart's retry cleanliness leans on.
type DuplicateNameError struct {
	Name string
}

func (e *DuplicateNameError) Error() string {
	return fmt.Sprintf("microvm: a session named %q already exists", e.Name)
}

// UnsupportedMountError is a Create refused because the spec carries a bind
// mount the microVM backend cannot express: V2b boots exactly one read-write
// workspace share, so any additional or differently-shaped mount is refused
// rather than silently dropped (OQ-C, record §(c)).
type UnsupportedMountError struct {
	Mount Mount
}

func (e *UnsupportedMountError) Error() string {
	return fmt.Sprintf(
		"microvm: unsupported mount %s->%s: the backend expresses exactly one read-write workspace share",
		e.Mount.HostPath, e.Mount.ContainerPath)
}

// Create allocates a session without booting it (mirroring `podman create`): it
// refuses a duplicate name, mints a random session id + runtime dir + boot
// nonce, validates the mount set down to the single workspace share, assembles
// the BootConfig, records the uid/env for Provision, and stores the session in
// the table. spec.Command and spec.CapAdd are IGNORED on this backend — a VM's
// keep-alive is the VMM + guestd PID 1, not a sleep-loop entrypoint, and
// CAP_NET_ADMIN is never granted to the workload boundary (record §(c)). No VM
// is booted here; Start does that.
func (m *MicroVMRuntime) Create(_ context.Context, spec ContainerSpec) (ContainerID, error) {
	shared, err := workspaceShare(spec.Mounts)
	if err != nil {
		return "", err
	}

	id, err := mintSessionID()
	if err != nil {
		return "", err
	}
	nonce, err := mintNonce()
	if err != nil {
		return "", err
	}

	runtimeDir := filepath.Join(m.config.RunRoot, "microvm", string(id))
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return "", fmt.Errorf("microvm: creating session runtime dir %s: %w", runtimeDir, err)
	}

	session := &microvmSession{
		id:    id,
		name:  spec.Name,
		cfg:   m.bootConfig(runtimeDir, nonce, shared),
		uid:   spec.UID,
		env:   spec.Env,
		nonce: nonce,
		// Never empty: the zero-value EgressPolicy still emits the default-deny
		// base ruleset, so every ContainerSpec-created session boots armed (§(e)).
		nftScript:  spec.Egress.NftScript(),
		runtimeDir: runtimeDir,
	}

	// Reject an over-long suffixed gateway socket path before boot: the host
	// serves the AgentGateway at GatewaySocketPath(VsockSocket, gateway port)
	// post-Launch, and its bind is sun_path-budgeted. Failing here — one length
	// comparison, before any VM boots — turns an over-long RunRoot into an
	// operator-actionable error instead of a post-boot Serve failure the caller
	// then has to tear down (record §(e)). Drop the runtime dir Create just made
	// so a refused Create leaves nothing behind, mirroring the duplicate-name leg.
	if gatewayPath := microvm.GatewaySocketPath(session.cfg.VsockSocket, agentGatewayVsockPort); len(gatewayPath) > sunPathMax {
		err := fmt.Errorf("microvm: gateway socket path %q is %d bytes, over the %d-byte AF_UNIX limit: shorten --microvm-runroot or $COMPASS_MICROVM_RUNROOT", gatewayPath, len(gatewayPath), sunPathMax)
		if rmErr := os.RemoveAll(runtimeDir); rmErr != nil {
			return "", errors.Join(err, fmt.Errorf("microvm: cleaning up refused session dir %s: %w", runtimeDir, rmErr))
		}
		return "", err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Refuse a duplicate name under the same lock that inserts, so two
	// concurrent Creates of the same name cannot both pass the check.
	for _, existing := range m.sessions {
		if existing.name == spec.Name {
			// The runtime dir was created above; drop it so a refused Create
			// leaves nothing behind. Removal failure is not actionable here —
			// the refusal is the outcome the caller acts on.
			if rmErr := os.RemoveAll(runtimeDir); rmErr != nil {
				return "", errors.Join(&DuplicateNameError{Name: spec.Name},
					fmt.Errorf("microvm: cleaning up refused session dir %s: %w", runtimeDir, rmErr))
			}
			return "", &DuplicateNameError{Name: spec.Name}
		}
	}
	m.sessions[id] = session
	return id, nil
}

// bootConfig assembles the microvm.BootConfig for a session: boot images from
// the operator config, distinct AF_UNIX socket paths inside the runtime dir,
// the fixed CID/port, the workspace share as FSSharedDir, the default guest
// sizing, and the boot nonce carried on the cmdline as lowercase hex under the
// compass.boot_nonce key guestd parses. Split out so spec→BootConfig assembly
// is unit-testable without booting.
func (m *MicroVMRuntime) bootConfig(runtimeDir string, nonce []byte, shared Mount) microvm.BootConfig {
	return microvm.BootConfig{
		Kernel:      m.config.KernelImage,
		Initrd:      m.config.InitrdImage,
		Rootfs:      m.config.RootfsImage,
		GatewayPort: agentGatewayVsockPort,
		Cmdline:     "compass.boot_nonce=" + hex.EncodeToString(nonce),
		VsockCID:    guestVsockCID,
		VsockPort:   guestVsockPort,
		VsockSocket: filepath.Join(runtimeDir, "vsock.sock"),
		FSTag:       workspaceFSTag,
		FSSocket:    filepath.Join(runtimeDir, "virtiofsd.sock"),
		FSSharedDir: shared.HostPath,
		CPUs:        m.config.DefaultCPUs,
		MemoryMB:    m.config.DefaultMemoryMB,
		Net: microvm.NetConfig{
			VhostUserSocket: filepath.Join(runtimeDir, "net.sock"),
			MAC:             guestMAC,
		},
	}
}

// workspaceShare validates the spec's mount set down to the single read-write
// workspace share the microVM backend can express, returning that mount. An
// empty mount set yields a zero Mount (FSSharedDir left empty — a guest boots
// with an empty workspace tree). More than one mount, a read-only mount, or a
// single mount targeting a ContainerPath other than /workspace is refused with
// an UnsupportedMountError naming the offending mount (OQ-C: refuse, don't
// drop). Split out so mount validation is unit-testable.
func workspaceShare(mounts []Mount) (Mount, error) {
	switch len(mounts) {
	case 0:
		return Mount{}, nil
	case 1:
		if mounts[0].ReadOnly {
			return Mount{}, &UnsupportedMountError{Mount: mounts[0]}
		}
		if mounts[0].ContainerPath != workspaceMountPath {
			return Mount{}, &UnsupportedMountError{Mount: mounts[0]}
		}
		return mounts[0], nil
	default:
		// Refuse the whole spec, naming the first mount beyond the one share
		// the backend can express.
		return Mount{}, &UnsupportedMountError{Mount: mounts[1]} //nolint:gosec // G602 false positive: the default branch is reached only when len(mounts) >= 2, so index 1 is in range
	}
}

// mintSessionID mints a random 16-byte hex session id used as the ContainerID
// and the runtime-dir leaf. There is no engine to print an id, so the backend
// generates one; hex keeps it filesystem-safe.
func mintSessionID() (ContainerID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("microvm: minting session id: %w", err)
	}
	return ContainerID(hex.EncodeToString(b[:])), nil
}

// mintNonce mints a random 16-byte boot nonce (raw bytes; the cmdline carries
// its hex encoding). It binds the guest answering Start's Health handshake to
// THIS BootConfig, catching a stale VMM on a recycled socket path (record §(e)).
func mintNonce() ([]byte, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("microvm: minting boot nonce: %w", err)
	}
	return b, nil
}

// Start boots and provisions the session transactionally: Launch, poll Health
// under a boot deadline until net_provisioned && workspace_mounted, verify the
// echoed boot nonce binds this guest to the BootConfig, then Provision to open
// the exec gate. Any step failing tears down whatever booted (vm.Shutdown)
// before returning — on this backend the boot IS Start, so Start cleans its own
// partial boot and Remove stays idempotent (record §(c)). On success the VM
// handle + GuestExec are stored on the session under the lock.
func (m *MicroVMRuntime) Start(ctx context.Context, id ContainerID) error {
	session, err := m.session(id)
	if err != nil {
		return err
	}

	vm, err := m.launchFunc(ctx, session.cfg)
	if err != nil {
		return fmt.Errorf("microvm: launching session %s: %w", id, err)
	}
	if vm == nil {
		return fmt.Errorf("microvm: launching session %s: launch returned a nil guest with no error", id)
	}
	// From here any failure must tear down the booted VM before returning:
	// booted is cleared once ownership transfers to the session table.
	booted := true
	defer func() {
		if booted {
			// Best-effort teardown of a partial boot; the returned start error
			// is what the caller acts on, so a Shutdown error is not surfaced.
			_ = vm.Shutdown(context.WithoutCancel(ctx))
		}
	}()

	if err := m.awaitHealthy(ctx, vm, session.nonce); err != nil {
		return err
	}

	if session.uid == 0 {
		return fmt.Errorf("microvm: session %s has a zero exec uid; Provision requires a non-zero default_exec_uid", id)
	}
	client := m.newGuestClient(session.cfg.VsockSocket, session.cfg.VsockPort)
	if _, err := client.Provision(ctx, connect.NewRequest(&compassv1.ProvisionRequest{
		NftScript:      session.nftScript,
		DefaultExecUid: session.uid,
		BaseEnv:        session.env,
	})); err != nil {
		return fmt.Errorf("microvm: provisioning session %s: %w", id, err)
	}

	m.mu.Lock()
	// Re-check membership under the same lock the store happens under: a
	// concurrent Remove may have won the race and deleted the entry while this
	// Start was booting. If so, do NOT store onto the orphaned session (that
	// would strand a live VMM+daemons); leave booted=true so the deferred
	// Shutdown tears the freshly-booted VM down, and return an error.
	if _, ok := m.sessions[id]; !ok {
		m.mu.Unlock()
		return fmt.Errorf("microvm: session %s was removed during Start", id)
	}
	session.vm = vm
	session.guestExec = microvm.NewGuestExec(client)
	m.mu.Unlock()
	booted = false // ownership transferred to the session; the defer must not tear it down
	return nil
}

// EgressArmedInGuest marks this backend as self-arming egress in-guest: the
// Provision RPC Start issues carries nft_script, so guestd arms as guest root
// before the exec gate opens (§(b)/(c)). AgentRuntime.provision probes for this
// marker (the unexported inGuestEgressArmer, agent.go) and skips its host-side
// armEgress exec — which on this backend would run capability-less and fail.
// Deliberately NOT a verb on the frozen ContainerRuntime interface (podman.go).
func (m *MicroVMRuntime) EgressArmedInGuest() bool { return true }

// awaitHealthy polls the guest's Health until it reports net_provisioned &&
// workspace_mounted (the V2a fail-closed readiness proof) within the boot
// deadline, then verifies the echoed boot_nonce equals the minted nonce before
// returning — a mismatch is an error (§(e) identity binding). The deadline
// derives from ctx when it carries one, else bootDeadline.
func (m *MicroVMRuntime) awaitHealthy(ctx context.Context, vm guestVM, nonce []byte) error {
	pollCtx, cancel := bootPollContext(ctx)
	defer cancel()

	ticker := time.NewTicker(healthPollInterval)
	defer ticker.Stop()

	for {
		resp, err := vm.Health(pollCtx)
		if err == nil && resp.GetNetProvisioned() && resp.GetWorkspaceMounted() {
			if !bytes.Equal(resp.GetBootNonce(), nonce) {
				return fmt.Errorf(
					"microvm: boot nonce mismatch: guest echoed %x, minted %x (stale VMM on a recycled socket?)",
					resp.GetBootNonce(), nonce)
			}
			return nil
		}
		select {
		case <-pollCtx.Done():
			if err != nil {
				return fmt.Errorf("microvm: guest did not become healthy before the boot deadline: %w", err)
			}
			return fmt.Errorf("microvm: guest did not become healthy before the boot deadline: %s",
				"net_provisioned && workspace_mounted never held")
		case <-ticker.C:
		}
	}
}

// bootPollContext derives the Health-poll deadline: the caller's ctx deadline
// when it has one (so the boot honors caller cancellation/timeout), else a
// fresh bootDeadline-bounded context.
func bootPollContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, bootDeadline)
}

// Exec runs one command to completion in the session guest, mapping the spec
// onto microvm.ExecCall and the result back onto ExecOutput. A non-zero exit is
// a SUCCESSFUL call (captured in ExecOutput.ExitCode), never an error; a guest
// refusal or transport failure is an error, and a host-side timeout is mapped
// to a *runtime.TimeoutError so requireSuccess/atStage callers behave
// identically to the podman path (record §(c)).
func (m *MicroVMRuntime) Exec(ctx context.Context, id ContainerID, spec ExecSpec) (ExecOutput, error) {
	guestExec, err := m.startedExec(id)
	if err != nil {
		return ExecOutput{}, err
	}
	call, err := execCall(spec)
	if err != nil {
		return ExecOutput{}, err
	}

	result, err := guestExec.Exec(ctx, call)
	if err != nil {
		var timeout *microvm.TimeoutError
		if errors.As(err, &timeout) {
			return ExecOutput{}, &TimeoutError{
				Summary: "microvm exec",
				Timeout: timeout.Timeout,
			}
		}
		return ExecOutput{}, fmt.Errorf("microvm: exec in session %s: %w", id, err)
	}
	return ExecOutput{
		Stdout:   string(result.Stdout),
		Stderr:   string(result.Stderr),
		ExitCode: result.ExitCode,
	}, nil
}

// execCall maps an ExecSpec onto a microvm.ExecCall: User (a numeric-string uid
// on every callsite) parses to a *uint32 UID (a non-numeric User is a host-side
// error), Stdin *string becomes []byte, and the per-command timeout is set so
// guestd mirrors the host-side ctx deadline. Split out so spec→ExecCall mapping
// is unit-testable.
func execCall(spec ExecSpec) (microvm.ExecCall, error) {
	uid, err := parseUID(spec.User)
	if err != nil {
		return microvm.ExecCall{}, err
	}
	var stdin []byte
	if spec.Stdin != nil {
		stdin = []byte(*spec.Stdin)
	}
	return microvm.ExecCall{
		Command:        spec.Command,
		UID:            uid,
		Workdir:        spec.Workdir,
		Env:            spec.Env,
		Stdin:          stdin,
		TimeoutSeconds: uint32(execDefaultTimeout.Seconds()),
	}, nil
}

// parseUID parses a --user value (a numeric-string uid on every Runner
// callsite, e.g. AsUser(strconv.FormatUint(uid))) into a *uint32. A nil User
// leaves the uid nil (the session default set by Provision); a non-numeric User
// is a host-side error rather than a guest-side refusal. Split out so numeric-
// uid parsing is unit-testable.
func parseUID(user *string) (*uint32, error) {
	if user == nil {
		return nil, nil //nolint:nilnil // a nil *uint32 is the meaningful "no uid override" (the session default set by Provision), not an error condition
	}
	parsed, err := strconv.ParseUint(*user, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("microvm: exec user %q is not a numeric uid: %w", *user, err)
	}
	uid := uint32(parsed)
	return &uid, nil
}

// exitError maps a guest ExitStatus onto the portable exit error contract: a
// signalled exit carries the signal (isDeliberateKill true), a non-zero code
// carries the code, a clean exit is nil (OQ-G/U3b).
func exitError(st microvm.ExitStatus) error {
	switch {
	case st.Signal != 0:
		return &ExitStatusError{Code: st.Code, Signal: syscall.Signal(st.Signal)}
	case st.Code != 0:
		return &ExitStatusError{Code: st.Code}
	default:
		return nil
	}
}

// ExecStreaming starts a long-lived streaming exec in the session guest,
// mapping the spec onto microvm.StreamCall and the GuestStream onto a
// *StreamingExec. ExecStream awaits the ExecStarted frame, so a spawn failure
// surfaces as the returned error. The ChildHandle is built over the remote
// exec's kill/wait pair: killFunc issues a bounded SIGKILL Signal (never
// blocking teardown), and waitFunc maps the guest exit onto nil / a
// *runtime.ExitStatusError so the runner's isDeliberateKill recognizes a
// signalled exit as a deliberate kill (OQ-G/U3b, record §(c)).
func (m *MicroVMRuntime) ExecStreaming(ctx context.Context, id ContainerID, spec StreamingExecSpec) (*StreamingExec, error) {
	guestExec, err := m.startedExec(id)
	if err != nil {
		return nil, err
	}
	uid, err := parseUID(spec.User)
	if err != nil {
		return nil, err
	}

	gs, err := guestExec.ExecStream(ctx, microvm.StreamCall{
		Command: spec.Command,
		UID:     uid,
		Workdir: spec.Workdir,
		Env:     spec.Env,
	})
	if err != nil {
		return nil, &SpawnError{Program: "microvm guest exec", Err: err}
	}

	killFunc := func() error { //nolint:contextcheck // GuestStream.Kill issues a self-bounded SIGKILL Signal RPC and takes no ctx by design (mirrors podman's local-cancel Kill; newChildHandleFuncs's kill is a func() error)
		// Kill is bounded by killSignalTimeout inside GuestStream and never
		// blocks teardown past it; a transport error is returned but the
		// teardown caller ignores it (the VMM-kill escalation is the backstop).
		return gs.Kill(int(syscall.SIGKILL))
	}
	waitFunc := func() error { return exitError(gs.Wait()) }
	return &StreamingExec{
		IO:      StreamingIO{Stdin: gs.Stdin, Stdout: gs.Stdout, Stderr: gs.Stderr},
		Process: newChildHandleFuncs(killFunc, waitFunc),
	}, nil
}

// Stop stops the session VM gracefully then forcibly, mirroring podman's
// --time semantics. It sends the guest-stop Signal RPC (empty exec_id targets
// the guest itself: guestd cancels its serving ctx, SIGTERMs its children,
// drains, and reboot(RB_POWER_OFF)s so the VMM observes shutdown), then awaits a
// real VMM exit up to timeout. Past the timeout it kills the VMM outright via
// vm.Shutdown (which also reaps the daemons and removes the sockets). A session
// that never started (no VM handle) is a no-op success (record §(d)).
func (m *MicroVMRuntime) Stop(ctx context.Context, id ContainerID, timeout time.Duration) error {
	session, err := m.session(id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	vm := session.vm
	m.mu.Unlock()
	if vm == nil {
		return nil // never started: nothing to stop
	}

	// The graceful preamble: ask the guest to power itself off. A failed Signal
	// is not fatal — the VMM-kill escalation below is the backstop — but it is
	// wrapped into the deadline wait's outcome rather than silently dropped.
	client := microvm.GuestClient(session.cfg.VsockSocket, session.cfg.VsockPort)
	signalErr := stopGuest(ctx, client)

	// Await a real VMM exit up to timeout (a SIGTERM-honoring guest powers off
	// before this elapses, observed via the reaper); past it, kill the VMM
	// outright.
	if vm.WaitVMMExit(timeout) {
		return vm.Shutdown(context.WithoutCancel(ctx)) // reap daemons + remove sockets
	}
	if err := vm.Shutdown(context.WithoutCancel(ctx)); err != nil {
		return errors.Join(fmt.Errorf("microvm: stopping session %s: %w", id, err), signalErr)
	}
	return nil
}

// stopGuest sends the guest-stop Signal RPC (empty exec_id, SIGTERM) that tells
// guestd to drain and power off. The returned error is informational — the
// caller escalates to a VMM kill regardless — so it is threaded into the Stop
// error rather than handled here.
func stopGuest(ctx context.Context, client compassv1internalconnect.GuestControlClient) error {
	_, err := client.Signal(ctx, connect.NewRequest(&compassv1.SignalRequest{
		ExecId: "",
		Signal: int32(syscall.SIGTERM),
	}))
	if err != nil {
		return fmt.Errorf("microvm: sending guest stop signal: %w", err)
	}
	return nil
}

// Remove force-kills the session VM if still running (vm.Shutdown is
// sync.Once-guarded, safe to call twice), deletes the runtime dir, and drops
// the session-table entry. It is idempotent: a Remove of an unknown or
// already-removed id is not an error (matching `podman rm --force`), and a
// session that never started is torn down to just its dir + entry (record §(d)).
func (m *MicroVMRuntime) Remove(ctx context.Context, id ContainerID) error {
	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return nil // unknown/already-removed: idempotent no-op
	}
	vm := session.vm
	delete(m.sessions, id)
	m.mu.Unlock()

	var errs []error
	if vm != nil {
		if err := vm.Shutdown(context.WithoutCancel(ctx)); err != nil {
			errs = append(errs, fmt.Errorf("microvm: shutting down session %s: %w", id, err))
		}
	}
	if err := os.RemoveAll(session.runtimeDir); err != nil {
		errs = append(errs, fmt.Errorf("microvm: removing session dir %s: %w", session.runtimeDir, err))
	}
	return errors.Join(errs...)
}

// Exists answers a NAME query from the session table: true if a session with
// spec.Name == name exists in any state, else false. It keys on spec.Name, NOT
// the random session id, so the Runner's stable handle resolves (record §(c)).
func (m *MicroVMRuntime) Exists(_ context.Context, name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, session := range m.sessions {
		if session.name == name {
			return true, nil
		}
	}
	return false, nil
}

// AgentGatewayEndpoint resolves the named session and returns the host-side
// AF_UNIX path the Runner serves its AgentGateway on — GatewaySocketPath over
// the session's own vsock socket base and the fixed gateway port (record
// §(b)/§(c)/§(e)). An unknown name returns ("", false). It keys on spec.Name
// like Exists, so the Runner's stable handle resolves. Deliberately NOT a verb
// on the frozen ContainerRuntime interface: agentHost probes for it via an
// unexported single-method assertion, so the podman backend (which lacks it) is
// unaffected (record §(c), Global Constraints).
func (m *MicroVMRuntime) AgentGatewayEndpoint(name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, session := range m.sessions {
		if session.name == name {
			return microvm.GatewaySocketPath(session.cfg.VsockSocket, agentGatewayVsockPort), true
		}
	}
	return "", false
}

// MountLabel returns the empty label unconditionally: the microVM backend has
// no SELinux mount label to report (the workspace is a virtio-fs share, not a
// relabeled bind mount), and the config materializer treats an empty label as
// skip-chcon (the parent's Q-mountlabel deferral, record §(c)). An unknown id
// is not distinguished — the empty answer is correct for it too.
func (m *MicroVMRuntime) MountLabel(_ context.Context, _ ContainerID) (string, error) {
	return "", nil
}

// Resize mirrors PodmanCLI.Resize: it returns the shared ErrResizeNotImplemented
// sentinel until C3 fills in resize-in-place behind the S1-frozen seam (the
// C3/D5 deferral, record §(c)). It is not a microVM-specific unimplemented
// verb, so it shares the podman backend's sentinel.
func (m *MicroVMRuntime) Resize(_ context.Context, _ ContainerID, _ ResourceLimits) error {
	return ErrResizeNotImplemented
}

// session looks up a session by id under the lock, returning a stage-agnostic
// error if it is absent.
func (m *MicroVMRuntime) session(id ContainerID) (*microvmSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("microvm: no session %s", id)
	}
	return session, nil
}

// startedExec looks up a session's GuestExec client under the lock, erroring if
// the session is absent or not yet started (Exec/ExecStreaming both require a
// booted, provisioned guest).
func (m *MicroVMRuntime) startedExec(id ContainerID) (*microvm.GuestExec, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("microvm: no session %s", id)
	}
	if session.guestExec == nil {
		return nil, fmt.Errorf("microvm: session %s is not started", id)
	}
	return session.guestExec, nil
}

var _ ContainerRuntime = (*MicroVMRuntime)(nil)
