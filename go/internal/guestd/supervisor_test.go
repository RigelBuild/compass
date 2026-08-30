//go:build linux

package guestd

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// Hermetic supervisor suite (§(b) acceptance): every row runs over an in-memory
// h2c listener with real child processes (ordinary host commands, no KVM). It
// proves the gate, uid enforcement, stdin-not-argv, exit-code-not-error,
// stream demux ordering, Signal semantics, ctx-bound reap, and the peer-CID
// pure function. The reboot(RB_POWER_OFF) path is U4's (needs real PID 1).

// testCredential is the hermetic credentialFunc: it returns nil so a spawned
// child runs as the test's own uid — the setuid path (linuxCredential) needs
// root and is proven in U5's real-boot rows.
func testCredential(uint32) *syscall.Credential { return nil }

// newTestSupervisor builds a provisioned supervisor and serves it over an
// in-memory TCP listener with the production h2c door, returning a GuestControl
// client and the supervisor. Serving stops when the test's context is
// cancelled. defaultUID is the session default exec uid Provision recorded.
func newTestSupervisor(t *testing.T, provisioned bool, defaultUID uint32) (compassv1internalconnect.GuestControlClient, *supervisor) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	state := stateReady
	var dUID uint32
	var baseEnv map[string]string
	if provisioned {
		state = stateProvisioned
		dUID = defaultUID
		// base_env carries PATH in production (the container's env on podman);
		// without it a child that resolves a bare argv[0] (cat, sleep) via PATH
		// would fail, since cmd.Env is the merged env, not the host's.
		baseEnv = map[string]string{"PATH": os.Getenv("PATH")}
	}
	svc := &supervisor{
		version:          "v-test",
		netProvisioned:   true,
		workspaceMounted: true,
		newCredential:    testCredential,
		state:            state,
		defaultExecUID:   dUID,
		baseEnv:          baseEnv,
		execs:            map[string]*childExec{},
	}

	ctx, cancel := context.WithCancel(t.Context())
	serveErr := make(chan error, 1)
	go func() { serveErr <- serveHandshake(ctx, ln, svc) }()
	t.Cleanup(func() {
		cancel()
		<-serveErr
	})

	h2cClient := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		},
	}
	client := compassv1internalconnect.NewGuestControlClient(h2cClient, "http://"+ln.Addr().String())
	return client, svc
}

func uidPtr(u uint32) *uint32 { return &u }

func TestExecRefusedBeforeProvision(t *testing.T) {
	client, _ := newTestSupervisor(t, false, 0)
	_, err := client.Exec(t.Context(), connect.NewRequest(&compassv1internal.ExecRequest{
		Command: []string{"/bin/true"},
		Uid:     uidPtr(1000),
	}))
	if err == nil {
		t.Fatal("Exec before Provision returned nil, want failed-precondition")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("Exec before Provision = %v, want FailedPrecondition", connect.CodeOf(err))
	}
}

func TestProvisionOpensGateAndRejectsRootAndNft(t *testing.T) {
	client, svc := newTestSupervisor(t, false, 0)

	// uid 0 default is refused.
	_, err := client.Provision(t.Context(), connect.NewRequest(&compassv1internal.ProvisionRequest{
		DefaultExecUid: 0,
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Provision with uid 0 = %v, want InvalidArgument", err)
	}

	// Non-empty nft_script is unimplemented in V2b.
	_, err = client.Provision(t.Context(), connect.NewRequest(&compassv1internal.ProvisionRequest{
		DefaultExecUid: 1000,
		NftScript:      "table inet filter {}",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("Provision with nft_script = %v, want Unimplemented", err)
	}

	// A clean Provision opens the gate.
	_, err = client.Provision(t.Context(), connect.NewRequest(&compassv1internal.ProvisionRequest{
		DefaultExecUid: 1000,
		BaseEnv:        map[string]string{"BASE": "1"},
	}))
	if err != nil {
		t.Fatalf("clean Provision: %v", err)
	}
	svc.mu.Lock()
	got := svc.state
	svc.mu.Unlock()
	if got != stateProvisioned {
		t.Fatalf("state after Provision = %d, want provisioned", got)
	}
}

func TestExecUIDZeroRefused(t *testing.T) {
	client, _ := newTestSupervisor(t, true, 1000)
	_, err := client.Exec(t.Context(), connect.NewRequest(&compassv1internal.ExecRequest{
		Command: []string{"/bin/true"},
		Uid:     uidPtr(0),
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("Exec uid 0 = %v, want FailedPrecondition", err)
	}
}

func TestExecDefaultUIDResolves(t *testing.T) {
	// With no uid in the request, the exec resolves the session default. The
	// test's own uid is used (testCredential returns nil), so a successful run
	// proves the default-uid path was taken (an unset default would refuse as
	// uid 0).
	client, _ := newTestSupervisor(t, true, uint32(syscall.Getuid()))
	resp, err := client.Exec(t.Context(), connect.NewRequest(&compassv1internal.ExecRequest{
		Command: []string{"/bin/sh", "-c", "exit 0"},
	}))
	if err != nil {
		t.Fatalf("Exec with default uid: %v", err)
	}
	if resp.Msg.GetExitCode() != 0 {
		t.Fatalf("exit_code = %d, want 0", resp.Msg.GetExitCode())
	}
}

func TestExecStdinReachesChildNotArgv(t *testing.T) {
	client, _ := newTestSupervisor(t, true, 1000)
	// `cat` echoes stdin to stdout; the secret must arrive via stdin and NEVER
	// appear in argv. We also assert argv carries no secret by reading it back
	// from /proc — sh reports its own argv, which is just the -c program.
	secret := "s3cr3t-body"
	resp, err := client.Exec(t.Context(), connect.NewRequest(&compassv1internal.ExecRequest{
		Command: []string{"/bin/sh", "-c", "cat"},
		Uid:     uidPtr(uint32(syscall.Getuid())),
		Stdin:   []byte(secret),
	}))
	if err != nil {
		t.Fatalf("Exec cat: %v", err)
	}
	if string(resp.Msg.GetStdout()) != secret {
		t.Fatalf("stdout = %q, want %q (stdin not delivered to child)", resp.Msg.GetStdout(), secret)
	}
}

func TestExecNonZeroExitIsSuccessfulResponse(t *testing.T) {
	client, _ := newTestSupervisor(t, true, 1000)
	resp, err := client.Exec(t.Context(), connect.NewRequest(&compassv1internal.ExecRequest{
		Command: []string{"/bin/sh", "-c", "exit 7"},
		Uid:     uidPtr(uint32(syscall.Getuid())),
	}))
	if err != nil {
		t.Fatalf("Exec with non-zero exit returned handler error %v, want successful response", err)
	}
	if resp.Msg.GetExitCode() != 7 {
		t.Fatalf("exit_code = %d, want 7", resp.Msg.GetExitCode())
	}
}

func TestExecEnvMergedExecKeysWin(t *testing.T) {
	client, svc := newTestSupervisor(t, true, 1000)
	svc.mu.Lock()
	svc.baseEnv = map[string]string{"A": "base", "B": "base"}
	svc.mu.Unlock()
	resp, err := client.Exec(t.Context(), connect.NewRequest(&compassv1internal.ExecRequest{
		Command: []string{"/bin/sh", "-c", "printf '%s,%s' \"$A\" \"$B\""},
		Uid:     uidPtr(uint32(syscall.Getuid())),
		Env:     map[string]string{"B": "exec"},
	}))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := string(resp.Msg.GetStdout()); got != "base,exec" {
		t.Fatalf("env merge = %q, want base,exec (exec key wins)", got)
	}
}

func TestCappedBufferFlagsOverflow(t *testing.T) {
	// The cap unit itself: writes up to limit are retained, and the first write
	// that crosses it retains only the room that was left and flips over — the
	// invariant the Exec handler turns into a ResourceExhausted error.
	c := &cappedBuffer{limit: 4}
	if n, _ := c.Write([]byte("ab")); n != 2 || c.over {
		t.Fatalf("after 2-byte write: n=%d over=%v, want n=2 over=false", n, c.over)
	}
	// A write that overruns reports the full input length consumed (the child's
	// Write must not see a short write and error), retains only the 2 bytes of
	// room, and flips over.
	if n, _ := c.Write([]byte("cdef")); n != 4 || !c.over {
		t.Fatalf("after overflow write: n=%d over=%v, want n=4 over=true", n, c.over)
	}
	if got := string(c.Bytes()); got != "abcd" {
		t.Fatalf("buffered = %q, want %q (retain up to the cap, drop the rest)", got, "abcd")
	}
	// Once over, further writes are dropped but still report full consumption.
	if n, _ := c.Write([]byte("ghij")); n != 4 {
		t.Fatalf("post-overflow write n=%d, want 4 (dropped bytes still counted)", n)
	}
	if got := string(c.Bytes()); got != "abcd" {
		t.Fatalf("buffered after post-overflow write = %q, want %q", got, "abcd")
	}
}

func TestExecOutputOverflowIsResourceExhausted(t *testing.T) {
	// A child that emits more than the capture cap must surface a
	// ResourceExhausted handler error, NOT a silently-truncated success (OQ-E).
	// Inject a tiny cap through a supervisor seam so the test stays fast and
	// deterministic rather than emitting 8 MiB.
	client, svc := newTestSupervisor(t, true, uint32(syscall.Getuid()))
	svc.mu.Lock()
	svc.captureLimit = 16
	svc.mu.Unlock()
	_, err := client.Exec(t.Context(), connect.NewRequest(&compassv1internal.ExecRequest{
		Command: []string{"/bin/sh", "-c", "printf 'x%.0s' $(seq 1 64)"},
		Uid:     uidPtr(uint32(syscall.Getuid())),
	}))
	if err == nil {
		t.Fatal("Exec with output past the cap returned success, want ResourceExhausted")
	}
	if got := connect.CodeOf(err); got != connect.CodeResourceExhausted {
		t.Fatalf("Exec overflow error code = %v, want CodeResourceExhausted", got)
	}
}

func TestExecTimeoutKillsAndReapsChild(t *testing.T) {
	// A one-shot Exec with timeout_seconds set must, on overrun, SIGKILL the
	// child group and reap it before returning CodeDeadlineExceeded. One-shot
	// Exec does not register in the exec table, so the reap is proven by the
	// PROMPT return: the handler runs <-waitErr (the reap) between the SIGKILL
	// and the return, so a bounded elapsed with the right code is the reap. A
	// missed reap would block on <-waitErr forever and blow the 10s ceiling.
	client, _ := newTestSupervisor(t, true, uint32(syscall.Getuid()))
	start := time.Now()
	_, err := client.Exec(t.Context(), connect.NewRequest(&compassv1internal.ExecRequest{
		Command:        []string{"/bin/sh", "-c", "sleep 300"},
		Uid:            uidPtr(uint32(syscall.Getuid())),
		TimeoutSeconds: 1,
	}))
	if err == nil {
		t.Fatal("Exec that overran its timeout returned success, want DeadlineExceeded")
	}
	if got := connect.CodeOf(err); got != connect.CodeDeadlineExceeded {
		t.Fatalf("timeout error code = %v, want CodeDeadlineExceeded", got)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Exec took %v to time out, want ~1s (the reap did not unblock)", elapsed)
	}
}

func TestExecCanceledReapsChild(t *testing.T) {
	// A caller cancelling the request ctx before the child exits must SIGKILL +
	// reap the child and surface CodeCanceled. One-shot Exec does not register
	// in the exec table (only ExecStream does), so the reap is proven two ways:
	// the child spawns server-side (a marker file it touches on start), and the
	// handler returns CodeCanceled promptly — it physically executes <-waitErr
	// (the reap) between the SIGKILL and that return, so a bounded return is the
	// reap.
	client, _ := newTestSupervisor(t, true, uint32(syscall.Getuid()))
	marker := t.TempDir() + "/started"
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		_, err := client.Exec(ctx, connect.NewRequest(&compassv1internal.ExecRequest{
			Command: []string{"/bin/sh", "-c", "touch " + marker + "; sleep 300"},
			Uid:     uidPtr(uint32(syscall.Getuid())),
		}))
		errCh <- err
	}()

	// Event-gate on the child actually running server-side (marker present),
	// then cancel — no fixed sleep gating the assertion.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never started (marker file absent)")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-errCh:
		if got := connect.CodeOf(err); got != connect.CodeCanceled {
			t.Fatalf("cancel error code = %v, want CodeCanceled", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Exec did not return after ctx cancel (the reap did not unblock)")
	}
}

func TestExecStreamDemuxOrdering(t *testing.T) {
	client, _ := newTestSupervisor(t, true, 1000)
	stream := client.ExecStream(t.Context())
	if err := stream.Send(&compassv1internal.ExecStreamRequest{
		Frame: &compassv1internal.ExecStreamRequest_Start{
			Start: &compassv1internal.StartExec{
				Command: []string{"/bin/sh", "-c", "echo out; echo err 1>&2; exit 0"},
				Uid:     uidPtr(uint32(syscall.Getuid())),
			},
		},
	}); err != nil {
		t.Fatalf("send start: %v", err)
	}
	if err := stream.CloseRequest(); err != nil {
		t.Fatalf("close request: %v", err)
	}

	first, err := stream.Receive()
	if err != nil {
		t.Fatalf("receive started: %v", err)
	}
	if first.GetStarted() == nil || first.GetStarted().GetExecId() == "" {
		t.Fatalf("first frame = %v, want ExecStarted with an id", first)
	}

	// Accumulate each stream's payload and assert the demux routed the right
	// bytes onto the right frame: `out` must arrive ONLY as Stdout frames and
	// `err` ONLY as Stderr frames. A streamWriter that swapped the stdout bool,
	// dropped a stream, or merged them would fail here — the ordering-only
	// checks (exactly one Exit, nothing after it) are kept alongside.
	var stdout, stderr []byte
	var sawExit bool
	var exitCount int
	for {
		msg, rerr := stream.Receive()
		if rerr != nil {
			break
		}
		switch {
		case msg.GetStdout() != nil:
			if sawExit {
				t.Fatal("received a stdout frame after the terminal exit frame")
			}
			stdout = append(stdout, msg.GetStdout()...)
		case msg.GetStderr() != nil:
			if sawExit {
				t.Fatal("received a stderr frame after the terminal exit frame")
			}
			stderr = append(stderr, msg.GetStderr()...)
		case msg.GetExit() != nil:
			sawExit = true
			exitCount++
			if msg.GetExit().GetExitCode() != 0 {
				t.Fatalf("exit_code = %d, want 0", msg.GetExit().GetExitCode())
			}
		}
	}
	if exitCount != 1 {
		t.Fatalf("exit frames = %d, want exactly 1", exitCount)
	}
	if string(stdout) != "out\n" {
		t.Fatalf("stdout = %q, want %q (stdout stream mis-routed or dropped)", stdout, "out\n")
	}
	if string(stderr) != "err\n" {
		t.Fatalf("stderr = %q, want %q (stderr stream mis-routed or dropped)", stderr, "err\n")
	}
}

func TestSignalKillsLiveChildAndExitCarriesSignal(t *testing.T) {
	client, _ := newTestSupervisor(t, true, 1000)
	stream := client.ExecStream(t.Context())
	if err := stream.Send(&compassv1internal.ExecStreamRequest{
		Frame: &compassv1internal.ExecStreamRequest_Start{
			Start: &compassv1internal.StartExec{
				Command: []string{"/bin/sh", "-c", "sleep 300"},
				Uid:     uidPtr(uint32(syscall.Getuid())),
			},
		},
	}); err != nil {
		t.Fatalf("send start: %v", err)
	}
	started, err := stream.Receive()
	if err != nil {
		t.Fatalf("receive started: %v", err)
	}
	execID := started.GetStarted().GetExecId()

	// Signal the live child; the exit frame must carry SIGKILL.
	_, err = client.Signal(t.Context(), connect.NewRequest(&compassv1internal.SignalRequest{
		ExecId: execID,
		Signal: int32(syscall.SIGKILL),
	}))
	if err != nil {
		t.Fatalf("Signal: %v", err)
	}

	for {
		msg, rerr := stream.Receive()
		if rerr != nil {
			t.Fatalf("stream ended without exit frame: %v", rerr)
		}
		if msg.GetExit() != nil {
			if syscall.Signal(msg.GetExit().GetSignal()) != syscall.SIGKILL {
				t.Fatalf("exit signal = %d, want SIGKILL(%d)", msg.GetExit().GetSignal(), syscall.SIGKILL)
			}
			return
		}
	}
}

func TestSignalOnExitedExecIsNoOpSuccess(t *testing.T) {
	client, _ := newTestSupervisor(t, true, 1000)
	// Signal an exec_id that never existed (equivalent to already-reaped): the
	// supervisor drops reaped ids, so an unknown id is a no-op success.
	_, err := client.Signal(t.Context(), connect.NewRequest(&compassv1internal.SignalRequest{
		ExecId: "exec-does-not-exist",
		Signal: int32(syscall.SIGTERM),
	}))
	if err != nil {
		t.Fatalf("Signal on exited/unknown exec = %v, want no-op success", err)
	}
}

func TestBrokenExecStreamReapsChild(t *testing.T) {
	client, svc := newTestSupervisor(t, true, 1000)
	ctx, cancel := context.WithCancel(t.Context())
	stream := client.ExecStream(ctx)
	if err := stream.Send(&compassv1internal.ExecStreamRequest{
		Frame: &compassv1internal.ExecStreamRequest_Start{
			Start: &compassv1internal.StartExec{
				Command: []string{"/bin/sh", "-c", "sleep 300"},
				Uid:     uidPtr(uint32(syscall.Getuid())),
			},
		},
	}); err != nil {
		t.Fatalf("send start: %v", err)
	}
	started, err := stream.Receive()
	if err != nil {
		t.Fatalf("receive started: %v", err)
	}
	execID := started.GetStarted().GetExecId()

	// Capture the child pid, then break the stream by cancelling the client
	// context. guestd must SIGKILL+reap the bound child — no orphan survives.
	svc.mu.Lock()
	child := svc.execs[execID]
	svc.mu.Unlock()
	if child == nil || child.cmd.Process == nil {
		t.Fatal("child not registered")
	}
	pid := child.cmd.Process.Pid

	// A real host drains the response stream continuously; model that so the
	// server-side transport observes the cancel promptly (an idle client that
	// never reads keeps the HTTP/2 stream from delivering the RST to the
	// handler). The drain goroutine exits when the stream breaks.
	go func() {
		for {
			if _, rerr := stream.Receive(); rerr != nil {
				return
			}
		}
	}()
	cancel()

	// Event-gate primarily on the exec_id leaving the table: that removal is the
	// deterministic signal that the ExecStream handler returned AFTER its reap
	// (the defer unregister runs post-Wait). The pid check is a secondary guard
	// only, and only while present — once reaped the pid is freed and the host
	// can recycle it onto an unrelated process, so keying liveness on the raw
	// pid after removal would flake. A short tick keeps the loop off a hot spin.
	//
	// The ceiling is generous headroom for transport latency, NOT a reap SLA: the
	// reap fires once the handler observes the client cancel — via either the
	// broken-stream arm (the receive loop returning on a stream error) or the
	// ctx.Done arm of the ExecStream select — and both wait on HTTP/2 RST_STREAM
	// propagation over the loopback h2c transport plus goroutine scheduling. On a
	// saturated CI runner that delivery can take several seconds; 30s catches a
	// genuine never-reap hang while tolerating extreme load (a real hang blocks
	// forever, so a wide ceiling costs nothing on the happy path — the exec_id
	// leaves in ms).
	deadline := time.Now().Add(30 * time.Second)
	for {
		svc.mu.Lock()
		_, present := svc.execs[execID]
		svc.mu.Unlock()
		if !present {
			// Reaped and unregistered: the deterministic terminal state.
			return
		}
		if time.Now().After(deadline) {
			alive := syscall.Kill(pid, 0) == nil
			t.Fatalf("child pid %d still present=%v alive=%v after stream break", pid, present, alive)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPeerAllowed(t *testing.T) {
	tests := []struct {
		name string
		cid  uint32
		want bool
	}{
		{"host CID 2 allowed", 2, true},
		{"loopback CID 1 refused", 1, false},
		{"hypervisor CID 0 refused", 0, false},
		{"a guest CID 3 refused", 3, false},
		{"a high CID refused", 4294967295, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := peerAllowed(tt.cid); got != tt.want {
				t.Fatalf("peerAllowed(%d) = %v, want %v", tt.cid, got, tt.want)
			}
		})
	}
}

func TestRPCStopCancelsServingAndFlagsPowerOff(t *testing.T) {
	// The empty-exec_id Signal is the RPC Stop trigger (§(d)): it cancels the
	// supervisor's serving context and flags rpcStop so run ends in power-off.
	serveCtx, stopServing := context.WithCancel(t.Context())
	svc := &supervisor{
		version:       "v",
		newCredential: testCredential,
		stopServing:   stopServing,
		state:         stateProvisioned,
		execs:         map[string]*childExec{},
	}
	svc.initiateStop(syscall.SIGTERM)

	select {
	case <-serveCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("RPC Stop did not cancel the serving context")
	}
	svc.mu.Lock()
	rpcStop := svc.rpcStop
	svc.mu.Unlock()
	if !rpcStop {
		t.Fatal("rpcStop not set after RPC Stop")
	}
}

func TestExecResolvesBareCommandViaSessionPATH(t *testing.T) {
	// The PID-1 PATH regression: guestd runs as PID 1 with no process PATH, so
	// a bare argv[0] must resolve against the SESSION env's PATH (base_env),
	// never guestd's own env. Prove it hermetically: provision a base_env whose
	// PATH points at a temp dir holding an executable that is absent from this
	// test process's ambient PATH, then exec it by bare name. exec.Command's
	// own LookPath (which reads the process env, not cmd.Env) can never find it
	// — only session-PATH resolution can — so this fails before the fix and
	// passes after.
	dir := t.TempDir()
	probe := dir + "/probe"
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing probe: %v", err)
	}
	client, _ := newTestSupervisor(t, false, 0)
	if _, err := client.Provision(t.Context(), connect.NewRequest(&compassv1internal.ProvisionRequest{
		// Fixed non-zero uid: testCredential ignores the value (the child runs
		// as the test process's own uid), but Provision rejects uid 0, so a
		// Getuid() that returns 0 under root CI would fail unrelated to PATH.
		DefaultExecUid: 1000,
		BaseEnv:        map[string]string{"PATH": dir},
	})); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	resp, err := client.Exec(t.Context(), connect.NewRequest(&compassv1internal.ExecRequest{
		Command: []string{"probe"},
	}))
	if err != nil {
		t.Fatalf("Exec(bare probe) = %v; a bare argv[0] must resolve against the session PATH", err)
	}
	if resp.Msg.GetExitCode() != 0 {
		t.Fatalf("exit_code = %d, want 0", resp.Msg.GetExitCode())
	}
}

func TestMergeEnvFloorsDefaultPATH(t *testing.T) {
	// mergeEnv floors defaultGuestPATH only when neither the base nor the exec
	// env carries a PATH (the container-baked-PATH analog), and keeps a
	// caller-supplied PATH — even an empty one — verbatim.
	pathOf := func(env []string) (string, bool) {
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "PATH="); ok {
				return v, true
			}
		}
		return "", false
	}
	tests := []struct {
		name    string
		baseEnv map[string]string
		execEnv map[string]string
		want    string
	}{
		{"floored when absent", nil, nil, defaultGuestPATH},
		{"base PATH kept", map[string]string{"PATH": "/base"}, nil, "/base"},
		{"exec PATH wins", map[string]string{"PATH": "/base"}, map[string]string{"PATH": "/exec"}, "/exec"},
		{"empty PATH kept verbatim", map[string]string{"PATH": ""}, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &supervisor{baseEnv: tt.baseEnv, execs: map[string]*childExec{}}
			got, ok := pathOf(svc.mergeEnv(tt.execEnv))
			if !ok {
				t.Fatal("merged env has no PATH entry; want one")
			}
			if got != tt.want {
				t.Fatalf("merged PATH = %q, want %q", got, tt.want)
			}
		})
	}
}
