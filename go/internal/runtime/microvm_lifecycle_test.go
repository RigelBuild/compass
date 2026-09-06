//go:build unix

package runtime

// The microVM lifecycle suite: hermetic, no KVM, no subprocess. It exercises
// the pure/mappable logic the lifecycle methods are built from — spec→BootConfig
// assembly, spec→ExecCall mapping, numeric-uid parsing, mount validation, the
// session table, duplicate-name refusal, Exists by name, idempotent Remove,
// MountLabel, and Resize — WITHOUT booting a guest (Create/Start/Exec all need a
// booted VM, so the full lifecycle is KVM-gated in
// microvm_lifecycle_microvm_test.go). It is //go:build unix because the
// lifecycle file it tests is unix-only.

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/RigelBuild/compass/go/internal/runtime/microvm"
)

// TestBootConfigAssembly pins spec→BootConfig assembly: the image paths come
// from config, every AF_UNIX socket lives under the session runtime dir, the
// workspace mount becomes FSSharedDir, the CID/port are the fixed values, the
// sizing comes from config, and the boot nonce rides the cmdline as lowercase
// hex under the compass.boot_nonce key.
func TestBootConfigAssembly(t *testing.T) {
	runRoot := t.TempDir()
	m := NewMicroVMRuntime(MicroVMConfig{
		KernelImage:     "/img/kernel",
		RootfsImage:     "/img/rootfs",
		InitrdImage:     "/img/initrd",
		RunRoot:         runRoot,
		DefaultCPUs:     4,
		DefaultMemoryMB: 2048,
	})
	nonce := []byte{0xde, 0xad, 0xbe, 0xef}
	share := Mount{HostPath: "/host/checkout", ContainerPath: "/workspace"}
	runtimeDir := runRoot + "/microvm/sess1"

	cfg := m.bootConfig(runtimeDir, nonce, share, 1000)

	if cfg.Kernel != "/img/kernel" || cfg.Initrd != "/img/initrd" || cfg.Rootfs != "/img/rootfs" {
		t.Fatalf("boot images = %q/%q/%q, want the config paths", cfg.Kernel, cfg.Initrd, cfg.Rootfs)
	}
	if cfg.FSSharedDir != "/host/checkout" {
		t.Fatalf("FSSharedDir = %q, want the workspace mount host path", cfg.FSSharedDir)
	}
	if cfg.FSTag != workspaceFSTag {
		t.Fatalf("FSTag = %q, want %q", cfg.FSTag, workspaceFSTag)
	}
	// The in-guest agent uid rides the BootConfig so virtiofsd can map it back
	// to the invoking host user (record §(d) host-ownership parity). Dropping it
	// silently reverts the share to unmapped ownership.
	if cfg.AgentUID != 1000 {
		t.Fatalf("AgentUID = %d, want the spec uid 1000", cfg.AgentUID)
	}
	if cfg.VsockCID != guestVsockCID || cfg.VsockPort != guestVsockPort {
		t.Fatalf("CID/port = %d/%d, want %d/%d", cfg.VsockCID, cfg.VsockPort, guestVsockCID, guestVsockPort)
	}
	if cfg.GatewayPort != agentGatewayVsockPort {
		t.Fatalf("GatewayPort = %d, want %d", cfg.GatewayPort, agentGatewayVsockPort)
	}
	if cfg.CPUs != 4 || cfg.MemoryMB != 2048 {
		t.Fatalf("sizing = %d cpus / %d MB, want 4 / 2048", cfg.CPUs, cfg.MemoryMB)
	}
	for _, sock := range []string{cfg.VsockSocket, cfg.FSSocket, cfg.Net.VhostUserSocket} {
		if !strings.HasPrefix(sock, runtimeDir+"/") {
			t.Fatalf("socket %q is not under the session runtime dir %q", sock, runtimeDir)
		}
	}
	wantToken := "compass.boot_nonce=" + hex.EncodeToString(nonce)
	if !strings.Contains(cfg.Cmdline, wantToken) {
		t.Fatalf("cmdline = %q, want it to carry %q", cfg.Cmdline, wantToken)
	}
}

// TestCreateAllocatesWithoutBoot: Create records a session in the table without
// booting (no VM handle, no exec client), and the returned id resolves.
func TestCreateAllocatesWithoutBoot(t *testing.T) {
	m := NewMicroVMRuntime(MicroVMConfig{RunRoot: t.TempDir()})
	id, err := m.Create(context.Background(), ContainerSpec{Name: "agent-1", UID: 1000})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	session, err := m.session(id)
	if err != nil {
		t.Fatalf("session %s not in table after Create: %v", id, err)
	}
	if session.vm != nil || session.guestExec != nil {
		t.Fatal("Create booted the session; it must allocate without booting")
	}
	if session.name != "agent-1" || session.uid != 1000 {
		t.Fatalf("session name/uid = %q/%d, want agent-1/1000", session.name, session.uid)
	}
	if len(session.nonce) == 0 {
		t.Fatal("Create did not mint a boot nonce")
	}
}

// TestCreateRefusesDuplicateName: a second Create with a name already in the
// table is refused with a typed DuplicateNameError naming the collision.
func TestCreateRefusesDuplicateName(t *testing.T) {
	m := NewMicroVMRuntime(MicroVMConfig{RunRoot: t.TempDir()})
	if _, err := m.Create(context.Background(), ContainerSpec{Name: "dup", UID: 1000}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := m.Create(context.Background(), ContainerSpec{Name: "dup", UID: 1000})
	var dupErr *DuplicateNameError
	if !errors.As(err, &dupErr) {
		t.Fatalf("second Create err = %v, want *DuplicateNameError", err)
	}
	if dupErr.Name != "dup" {
		t.Fatalf("DuplicateNameError.Name = %q, want dup", dupErr.Name)
	}
}

// TestCreateRefusesInexpressibleMount: a spec carrying a mount the backend
// cannot express (more than one, or a read-only mount) is refused with a typed
// UnsupportedMountError naming the offending mount (OQ-C: refuse, don't drop).
func TestCreateRefusesInexpressibleMount(t *testing.T) {
	m := NewMicroVMRuntime(MicroVMConfig{RunRoot: t.TempDir()})
	tests := []struct {
		name      string
		mounts    []Mount
		wantMount Mount
	}{
		{
			name: "two mounts",
			mounts: []Mount{
				{HostPath: "/a", ContainerPath: "/workspace"},
				{HostPath: "/b", ContainerPath: "/config", ReadOnly: true},
			},
			wantMount: Mount{HostPath: "/b", ContainerPath: "/config", ReadOnly: true},
		},
		{
			name:      "single read-only mount",
			mounts:    []Mount{{HostPath: "/a", ContainerPath: "/config", ReadOnly: true}},
			wantMount: Mount{HostPath: "/a", ContainerPath: "/config", ReadOnly: true},
		},
		{
			name:      "single read-write mount at non-workspace path",
			mounts:    []Mount{{HostPath: "/a", ContainerPath: "/config"}},
			wantMount: Mount{HostPath: "/a", ContainerPath: "/config"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := m.Create(context.Background(), ContainerSpec{Name: tt.name, UID: 1000, Mounts: tt.mounts})
			var mountErr *UnsupportedMountError
			if !errors.As(err, &mountErr) {
				t.Fatalf("Create err = %v, want *UnsupportedMountError", err)
			}
			if mountErr.Mount != tt.wantMount {
				t.Fatalf("UnsupportedMountError.Mount = %+v, want %+v", mountErr.Mount, tt.wantMount)
			}
		})
	}
}

// TestWorkspaceShare: the single supported mount passes through; zero mounts
// yield an empty share; the refusal cases mirror Create's mount validation.
func TestWorkspaceShare(t *testing.T) {
	single := Mount{HostPath: "/host", ContainerPath: "/workspace"}
	got, err := workspaceShare([]Mount{single})
	if err != nil {
		t.Fatalf("single workspace mount: unexpected err %v", err)
	}
	if got != single {
		t.Fatalf("share = %+v, want %+v", got, single)
	}

	got, err = workspaceShare(nil)
	if err != nil || (got != Mount{}) {
		t.Fatalf("no mounts = (%+v, %v), want (zero Mount, nil)", got, err)
	}

	rw := Mount{HostPath: "/host", ContainerPath: "/config"}
	if _, err := workspaceShare([]Mount{rw}); err == nil {
		t.Fatalf("single read-write mount at /config: want UnsupportedMountError, got nil")
	} else {
		var mountErr *UnsupportedMountError
		if !errors.As(err, &mountErr) {
			t.Fatalf("err = %v, want *UnsupportedMountError", err)
		}
		if mountErr.Mount != rw {
			t.Fatalf("UnsupportedMountError.Mount = %+v, want %+v", mountErr.Mount, rw)
		}
	}
}

// TestExecCallMapping pins spec→ExecCall: a numeric User parses to a *uint32
// UID, Stdin *string becomes []byte, and command/workdir/env carry through.
func TestExecCallMapping(t *testing.T) {
	stdin := "secret-body"
	workdir := "/workspace"
	spec := ExecSpec{
		Command: []string{"sh", "-s"},
		User:    strPtr("1000"),
		Workdir: &workdir,
		Env:     map[string]string{"K": "V"},
		Stdin:   &stdin,
	}
	call, err := execCall(spec)
	if err != nil {
		t.Fatalf("execCall: %v", err)
	}
	if call.UID == nil || *call.UID != 1000 {
		t.Fatalf("UID = %v, want *1000", call.UID)
	}
	if string(call.Stdin) != stdin {
		t.Fatalf("Stdin = %q, want %q", call.Stdin, stdin)
	}
	if call.Workdir == nil || *call.Workdir != workdir {
		t.Fatalf("Workdir = %v, want %q", call.Workdir, workdir)
	}
	if len(call.Command) != 2 || call.Command[0] != "sh" {
		t.Fatalf("Command = %v, want [sh -s]", call.Command)
	}
	if call.Env["K"] != "V" {
		t.Fatalf("Env = %v, want K=V", call.Env)
	}
	if call.TimeoutSeconds != uint32(execDefaultTimeout.Seconds()) {
		t.Fatalf("TimeoutSeconds = %d, want %d", call.TimeoutSeconds, uint32(execDefaultTimeout.Seconds()))
	}
}

// TestExecCallNilStdinIsNil: a nil Stdin maps to a nil []byte (not an empty
// slice), so the guest never feeds an empty body to a command that expects no
// stdin.
func TestExecCallNilStdinIsNil(t *testing.T) {
	call, err := execCall(ExecSpec{Command: []string{"true"}})
	if err != nil {
		t.Fatalf("execCall: %v", err)
	}
	if call.Stdin != nil {
		t.Fatalf("Stdin = %v, want nil", call.Stdin)
	}
	if call.UID != nil {
		t.Fatalf("UID = %v, want nil (no User)", call.UID)
	}
}

// TestParseUID: nil User → nil UID; a numeric User → the parsed uid; a
// non-numeric User → a host-side error.
func TestParseUID(t *testing.T) {
	if uid, err := parseUID(nil); err != nil || uid != nil {
		t.Fatalf("parseUID(nil) = (%v, %v), want (nil, nil)", uid, err)
	}
	uid, err := parseUID(strPtr("1000"))
	if err != nil || uid == nil || *uid != 1000 {
		t.Fatalf("parseUID(1000) = (%v, %v), want (*1000, nil)", uid, err)
	}
	if _, err := parseUID(strPtr("agent")); err == nil {
		t.Fatal("parseUID(agent) err = nil, want a non-numeric-uid error")
	}
}

// TestExistsByName: Exists answers a NAME query from the table — a created
// session's spec.Name is present, an unknown name is absent, and the random
// session id is NOT a name match.
func TestExistsByName(t *testing.T) {
	m := NewMicroVMRuntime(MicroVMConfig{RunRoot: t.TempDir()})
	id, err := m.Create(context.Background(), ContainerSpec{Name: "agent-x", UID: 1000})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	present, err := m.Exists(context.Background(), "agent-x")
	if err != nil || !present {
		t.Fatalf("Exists(agent-x) = (%v, %v), want (true, nil)", present, err)
	}
	absent, err := m.Exists(context.Background(), "nope")
	if err != nil || absent {
		t.Fatalf("Exists(nope) = (%v, %v), want (false, nil)", absent, err)
	}
	// The random session id is not a name — Exists must not match on it.
	byID, err := m.Exists(context.Background(), string(id))
	if err != nil || byID {
		t.Fatalf("Exists(<session id>) = (%v, %v), want (false, nil): Exists keys on spec.Name", byID, err)
	}
}

// TestRemoveIdempotent: Remove of an unknown id is a no-op success, and Remove
// of a never-started session tears down its dir + table entry without error.
func TestRemoveIdempotent(t *testing.T) {
	m := NewMicroVMRuntime(MicroVMConfig{RunRoot: t.TempDir()})
	if err := m.Remove(context.Background(), ContainerID("never-existed")); err != nil {
		t.Fatalf("Remove(unknown) = %v, want nil", err)
	}

	id, err := m.Create(context.Background(), ContainerSpec{Name: "agent-r", UID: 1000})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Remove(context.Background(), id); err != nil {
		t.Fatalf("Remove(never-started) = %v, want nil", err)
	}
	if _, err := m.session(id); err == nil {
		t.Fatal("session still in table after Remove")
	}
	// A second Remove is still a no-op success.
	if err := m.Remove(context.Background(), id); err != nil {
		t.Fatalf("second Remove = %v, want nil (idempotent)", err)
	}
}

// TestMountLabelEmpty: MountLabel returns the empty label (skip-chcon) for any
// id, per the parent's Q-mountlabel deferral.
func TestMountLabelEmpty(t *testing.T) {
	m := NewMicroVMRuntime(MicroVMConfig{RunRoot: t.TempDir()})
	label, err := m.MountLabel(context.Background(), ContainerID("anything"))
	if err != nil || label != "" {
		t.Fatalf("MountLabel = (%q, %v), want (\"\", nil)", label, err)
	}
}

// TestResizeNotImplemented: Resize returns the shared ErrResizeNotImplemented
// sentinel, matching PodmanCLI.Resize (the C3/D5 deferral).
func TestResizeNotImplemented(t *testing.T) {
	m := NewMicroVMRuntime(MicroVMConfig{RunRoot: t.TempDir()})
	if err := m.Resize(context.Background(), ContainerID("c"), ResourceLimits{}); !errors.Is(err, ErrResizeNotImplemented) {
		t.Fatalf("Resize err = %v, want ErrResizeNotImplemented", err)
	}
}

// TestStartUnknownSession: Start on an id with no session errors rather than
// booting.
func TestStartUnknownSession(t *testing.T) {
	m := NewMicroVMRuntime(MicroVMConfig{RunRoot: t.TempDir()})
	if err := m.Start(context.Background(), ContainerID("ghost")); err == nil {
		t.Fatal("Start(unknown) err = nil, want a no-session error")
	}
}

// TestExecUnstartedSession: Exec on a created-but-not-started session errors
// (no exec client yet) rather than panicking.
func TestExecUnstartedSession(t *testing.T) {
	m := NewMicroVMRuntime(MicroVMConfig{RunRoot: t.TempDir()})
	id, err := m.Create(context.Background(), ContainerSpec{Name: "agent-e", UID: 1000})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Exec(context.Background(), id, NewExecSpec("true")); err == nil {
		t.Fatal("Exec on an unstarted session err = nil, want a not-started error")
	}
}

// TestExitErrorMapping pins ExecStreaming's waitFunc ExitStatus→error contract
// (OQ-G/U3b): a signalled exit carries the signal (so isDeliberateKill sees
// Signal!=0), a non-zero code carries the code, a clean exit is nil.
func TestExitErrorMapping(t *testing.T) {
	// (a) signalled exit → *ExitStatusError with the signal.
	err := exitError(microvm.ExitStatus{Signal: int(syscall.SIGKILL)})
	var signalled *ExitStatusError
	if !errors.As(err, &signalled) {
		t.Fatalf("signalled exit err = %v, want *ExitStatusError", err)
	}
	if signalled.Signal != syscall.SIGKILL {
		t.Fatalf("Signal = %v, want SIGKILL", signalled.Signal)
	}
	if signalled.Signal == 0 {
		t.Fatal("signalled exit must have Signal != 0 so isDeliberateKill recognizes it")
	}

	// (b) non-zero code → *ExitStatusError with the code, no signal.
	err = exitError(microvm.ExitStatus{Code: 3})
	var coded *ExitStatusError
	if !errors.As(err, &coded) {
		t.Fatalf("non-zero code err = %v, want *ExitStatusError", err)
	}
	if coded.Code != 3 || coded.Signal != 0 {
		t.Fatalf("coded = {Code:%d Signal:%d}, want {Code:3 Signal:0}", coded.Code, coded.Signal)
	}

	// (c) clean exit → nil.
	if err := exitError(microvm.ExitStatus{}); err != nil {
		t.Fatalf("clean exit err = %v, want nil", err)
	}
}

// TestAgentGatewayEndpoint: a created session resolves to its suffixed host-side
// gateway path (GatewaySocketPath over the session's own vsock base and the
// fixed gateway port), and an unknown name returns ("", false) — the probe shape
// agentHost's vsock leg relies on (record §(b)/§(c)/§(e)).
func TestAgentGatewayEndpoint(t *testing.T) {
	m := NewMicroVMRuntime(MicroVMConfig{RunRoot: t.TempDir()})
	id, err := m.Create(context.Background(), ContainerSpec{Name: "agent-1", UID: 1000})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	session, err := m.session(id)
	if err != nil {
		t.Fatalf("session %s not in table: %v", id, err)
	}
	want := microvm.GatewaySocketPath(session.cfg.VsockSocket, agentGatewayVsockPort)
	got, ok := m.AgentGatewayEndpoint("agent-1")
	if !ok {
		t.Fatal("AgentGatewayEndpoint(created session) ok = false, want true")
	}
	if got != want {
		t.Fatalf("AgentGatewayEndpoint = %q, want the suffixed path %q", got, want)
	}
	if !strings.HasSuffix(got, "/vsock.sock_1025") {
		t.Fatalf("endpoint %q does not carry the fixed gateway port suffix", got)
	}
	if _, ok := m.AgentGatewayEndpoint("no-such-session"); ok {
		t.Fatal("AgentGatewayEndpoint(unknown) ok = true, want false")
	}
}

// TestCreateRejectsOverLongGatewaySocketPath pins the pre-boot sun_path guard
// (record §(e)) at its exact off-by-one boundary: the fully-suffixed gateway
// path (<RunRoot>/microvm/<32-hex>/vsock.sock_1025) at exactly sunPathMax bytes
// must be ACCEPTED (the 108-byte sun_path holds sunPathMax chars + NUL), at
// sunPathMax+1 must be REJECTED before any VM boots, and far-over stays
// rejected. A '>' vs '>=' confusion in the guard would pass the boundary case
// but fail here. A refused Create leaves no session in the table.
func TestCreateRejectsOverLongGatewaySocketPath(t *testing.T) {
	// The suffixed path is <RunRoot>/microvm/<32-hex id>/vsock.sock_1025. The
	// id is 16 random bytes hex-encoded (32 chars, mintSessionID), so the tail
	// past RunRoot is a fixed length; derive it from the same constants Create
	// uses so the boundary is pinned, not guessed.
	const idLen = 2 * 16 // mintSessionID: hex of 16 bytes
	suffixed := microvm.GatewaySocketPath("/microvm/"+strings.Repeat("a", idLen)+"/vsock.sock", agentGatewayVsockPort)
	tailLen := len(suffixed) // bytes the path adds past RunRoot

	tests := []struct {
		name       string
		total      int
		wantReject bool
	}{
		{name: "at budget accepted", total: sunPathMax, wantReject: false},
		{name: "one over budget rejected", total: sunPathMax + 1, wantReject: true},
		{name: "far over budget rejected", total: sunPathMax + 40, wantReject: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The accepted case must actually create dirs, and the fully-suffixed
			// path must land at exactly tt.total bytes, so root the RunRoot under
			// a SHORT /tmp base (t.TempDir embeds the long test-function name and
			// would overflow the target) and pad the tail to hit the byte count.
			//nolint:usetesting // t.TempDir()'s path embeds the long test name, overflowing the sunPathMax-byte target this test pins; a short /tmp base is required.
			base, err := os.MkdirTemp("", "mvb-")
			if err != nil {
				t.Fatalf("creating temp base: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(base) })
			want := tt.total - tailLen - len(base) - len("/")
			if want < 1 {
				t.Fatalf("temp base %q (%d bytes) too long to build a %d-byte total path", base, len(base), tt.total)
			}
			runRoot := filepath.Join(base, strings.Repeat("r", want))
			if err := os.MkdirAll(runRoot, 0o700); err != nil {
				t.Fatalf("creating runroot: %v", err)
			}
			m := NewMicroVMRuntime(MicroVMConfig{RunRoot: runRoot})
			_, createErr := m.Create(context.Background(), ContainerSpec{Name: "agent-1", UID: 1000})
			if tt.wantReject {
				if createErr == nil {
					t.Fatal("Create succeeded; want the pre-boot budget error")
				}
				if !strings.Contains(createErr.Error(), "AF_UNIX limit") {
					t.Fatalf("Create err = %v, want the operator-actionable AF_UNIX budget error", createErr)
				}
				if ok, _ := m.Exists(context.Background(), "agent-1"); ok {
					t.Fatal("a refused Create left a session in the table; it must leave nothing behind")
				}
				return
			}
			if createErr != nil {
				t.Fatalf("Create at the budget boundary = %v, want success (path fits sun_path exactly)", createErr)
			}
			if ok, _ := m.Exists(context.Background(), "agent-1"); !ok {
				t.Fatal("an accepted Create left no session in the table")
			}
		})
	}
}

func strPtr(s string) *string { return &s }
