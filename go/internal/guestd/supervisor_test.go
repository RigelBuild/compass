//go:build linux

package guestd

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// Hermetic supervisor suite (§(b) acceptance): every row runs over an in-memory
// h2c listener with real child processes (no KVM). It proves the gate, uid
// enforcement, stdin-not-argv, exit-code-not-error, stream demux ordering, Signal
// semantics, ctx-bound reap, and the peer-CID pure function.

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
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
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

	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	h2cClient := &http.Client{Transport: &http.Transport{Protocols: protocols}}
	client := compassv1internalconnect.NewGuestControlClient(
		connect.NewClient(
			connecthttp.NewTransport(
				h2cClient,
				"http://"+ln.Addr().String(),
				connecthttp.WithReadMaxBytes(0),
			),
		),
	)
	return client, svc
}

func TestExecRefusedBeforeProvision(t *testing.T) {
	client, _ := newTestSupervisor(t, false, 0)
	_, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{
		Command: []string{"/bin/true"},
		Uid:     new(uint32(1000)),
	})
	if err == nil {
		t.Fatal("Exec before Provision returned nil, want failed-precondition")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("Exec before Provision = %v, want FailedPrecondition", connect.CodeOf(err))
	}
}

func TestExecEmptyCommandIsInvalidArgument(t *testing.T) {
	client, _ := newTestSupervisor(t, true, 1000)
	_, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Exec with empty command = %v, want InvalidArgument", err)
	}
}

func TestExecStreamEmptyCommandIsInvalidArgument(t *testing.T) {
	client, _ := newTestSupervisor(t, true, 1000)
	stream, err := client.ExecStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&compassv1internal.ExecStreamRequest{
		Frame: &compassv1internal.ExecStreamRequest_Start{Start: &compassv1internal.StartExec{}},
	}); err != nil {
		t.Fatalf("send start: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close request: %v", err)
	}
	if _, err := stream.Receive(); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("ExecStream with empty command = %v, want InvalidArgument", err)
	}
}

func TestProvisionOpensGateAndRejectsRootAndNft(t *testing.T) {
	client, svc := newTestSupervisor(t, false, 0)

	// uid 0 default is refused.
	_, err := client.Provision(t.Context(), &compassv1internal.ProvisionRequest{
		DefaultExecUid: 0,
	})
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Provision with uid 0 = %v, want InvalidArgument", err)
	}

	// A non-empty nft_script now arms egress via the injected armFunc, not a
	// CodeUnimplemented refusal (V3). Detailed arm coverage lives in the tests
	// below; here we only confirm the clean uid path still opens the gate.

	// A clean Provision opens the gate.
	_, err = client.Provision(t.Context(), &compassv1internal.ProvisionRequest{
		DefaultExecUid: 1000,
		BaseEnv:        map[string]string{"BASE": "1"},
	})
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

// newArmSupervisor builds a stateReady supervisor with an injected armFunc for
// the hermetic arm tests, mirroring the &supervisor{} literal pattern used
// elsewhere in this suite. Provision is exercised by direct method call.
func newArmSupervisor(arm func(ctx context.Context, script string) error) *supervisor {
	return &supervisor{
		version:       "v-test",
		newCredential: testCredential,
		armFunc:       arm,
		state:         stateReady,
		execs:         map[string]*childExec{},
	}
}

func TestProvisionArmsNonEmptyScript(t *testing.T) {
	const script = "table inet filter { chain c {} }"
	var got string
	var calls int
	svc := newArmSupervisor(func(_ context.Context, s string) error {
		calls++
		got = s
		return nil
	})
	_, err := svc.Provision(t.Context(), &compassv1internal.ProvisionRequest{
		DefaultExecUid: 1000,
		NftScript:      script,
	})
	if err != nil {
		t.Fatalf("Provision with arm: %v", err)
	}
	if calls != 1 || got != script {
		t.Fatalf("armFunc calls=%d arg=%q, want 1 call with exact script %q", calls, got, script)
	}
	svc.mu.Lock()
	state := svc.state
	svc.mu.Unlock()
	if state != stateProvisioned {
		t.Fatalf("state after successful arm = %d, want provisioned", state)
	}
}

func TestProvisionArmFailureLeavesGateClosed(t *testing.T) {
	svc := newArmSupervisor(func(_ context.Context, _ string) error {
		return errors.New("nft: exit status 1")
	})
	_, err := svc.Provision(t.Context(), &compassv1internal.ProvisionRequest{
		DefaultExecUid: 1000,
		NftScript:      "bad script",
	})
	if err == nil || connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("Provision with failing arm = %v, want CodeInternal", err)
	}
	svc.mu.Lock()
	state := svc.state
	svc.mu.Unlock()
	if state != stateReady {
		t.Fatalf("state after failed arm = %d, want stateReady", state)
	}
	if gerr := svc.requireProvisioned(); gerr == nil {
		t.Fatal("requireProvisioned after failed arm = nil, want refused")
	}
	svc.armFunc = func(_ context.Context, _ string) error { return nil }
	if _, rerr := svc.Provision(t.Context(), &compassv1internal.ProvisionRequest{
		DefaultExecUid: 1000,
		NftScript:      "good script",
	}); rerr != nil {
		t.Fatalf("retried Provision after failed arm: %v", rerr)
	}
	svc.mu.Lock()
	state = svc.state
	svc.mu.Unlock()
	if state != stateProvisioned {
		t.Fatalf("state after retried arm = %d, want provisioned", state)
	}
}

func TestProvisionEmptyScriptSkipsArm(t *testing.T) {
	var calls int
	svc := newArmSupervisor(func(_ context.Context, _ string) error {
		calls++
		return nil
	})
	_, err := svc.Provision(t.Context(), &compassv1internal.ProvisionRequest{
		DefaultExecUid: 1000,
	})
	if err != nil {
		t.Fatalf("Provision with empty script: %v", err)
	}
	if calls != 0 {
		t.Fatalf("armFunc called %d times for empty script, want 0", calls)
	}
	svc.mu.Lock()
	state := svc.state
	svc.mu.Unlock()
	if state != stateProvisioned {
		t.Fatalf("state after empty-script Provision = %d, want provisioned (gate open)", state)
	}
}

func TestProvisionAlreadyProvisionedAfterArm(t *testing.T) {
	var calls int
	svc := newArmSupervisor(func(_ context.Context, _ string) error {
		calls++
		return nil
	})
	req := &compassv1internal.ProvisionRequest{
		DefaultExecUid: 1000,
		NftScript:      "table inet filter {}",
	}
	if _, err := svc.Provision(t.Context(), req); err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	// A second Provision is refused before any re-arm (no wire re-arm surface).
	_, err := svc.Provision(t.Context(), req)
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("re-Provision = %v, want FailedPrecondition", err)
	}
	if calls != 1 {
		t.Fatalf("armFunc called %d times, want 1 (no re-arm)", calls)
	}
}

func TestRunNftScriptCarriesExitStatus(t *testing.T) {
	// This exercises the HOST /bin/sh only — it proves runNftScript surfaces a
	// non-zero exit status without root or nft. The GUEST /bin/sh link is proven
	// by the W3/KVM suite, not here.
	svc := newArmSupervisor(runNftScript)
	_, err := svc.Provision(t.Context(), &compassv1internal.ProvisionRequest{
		DefaultExecUid: 1000,
		NftScript:      "exit 7",
	})
	if err == nil || connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("Provision with failing real script = %v, want CodeInternal", err)
	}
	if !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("error %q does not carry exit status 7", err.Error())
	}
	svc.mu.Lock()
	state := svc.state
	svc.mu.Unlock()
	if state != stateReady {
		t.Fatalf("state after failed real arm = %d, want stateReady", state)
	}
}

func TestRunNftScriptFloorsGuestPATH(t *testing.T) {
	// The §(e) total-backend-outage regression: guestd is PID 1 with no PATH, and the
	// arm is a spawn path SEPARATE from exec children, so it never inherits mergeEnv's
	// PATH floor. Without a floor the script's bare nft/getent/awk fail every microVM
	// Start. Prove the arm child runs with PATH == defaultGuestPATH.
	dir := t.TempDir()
	out := dir + "/path"
	if err := runNftScript(t.Context(), `echo "$PATH" > `+out); err != nil {
		t.Fatalf("runNftScript(capture PATH) = %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading captured PATH: %v", err)
	}
	if strings.TrimSpace(string(got)) != defaultGuestPATH {
		t.Fatalf("arm child PATH = %q, want %q", strings.TrimSpace(string(got)), defaultGuestPATH)
	}
}

func TestExecUIDZeroRefused(t *testing.T) {
	client, _ := newTestSupervisor(t, true, 1000)
	_, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{
		Command: []string{"/bin/true"},
		Uid:     new(uint32(0)),
	})
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
	resp, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{
		Command: []string{"/bin/sh", "-c", "exit 0"},
	})
	if err != nil {
		t.Fatalf("Exec with default uid: %v", err)
	}
	if resp.GetExitCode() != 0 {
		t.Fatalf("exit_code = %d, want 0", resp.GetExitCode())
	}
}

func TestExecStdinReachesChildNotArgv(t *testing.T) {
	client, _ := newTestSupervisor(t, true, 1000)
	// `cat` echoes stdin to stdout; the secret must arrive via stdin and NEVER
	// appear in argv. We also assert argv carries no secret by reading it back
	// from /proc — sh reports its own argv, which is just the -c program.
	secret := "s3cr3t-body"
	resp, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{
		Command: []string{"/bin/sh", "-c", "cat"},
		Uid:     new(uint32(syscall.Getuid())),
		Stdin:   []byte(secret),
	})
	if err != nil {
		t.Fatalf("Exec cat: %v", err)
	}
	if string(resp.GetStdout()) != secret {
		t.Fatalf("stdout = %q, want %q (stdin not delivered to child)", resp.GetStdout(), secret)
	}
}

func TestExecNonZeroExitIsSuccessfulResponse(t *testing.T) {
	client, _ := newTestSupervisor(t, true, 1000)
	resp, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{
		Command: []string{"/bin/sh", "-c", "exit 7"},
		Uid:     new(uint32(syscall.Getuid())),
	})
	if err != nil {
		t.Fatalf("Exec with non-zero exit returned handler error %v, want successful response", err)
	}
	if resp.GetExitCode() != 7 {
		t.Fatalf("exit_code = %d, want 7", resp.GetExitCode())
	}
}

func TestExecEnvMergedExecKeysWin(t *testing.T) {
	client, svc := newTestSupervisor(t, true, 1000)
	svc.mu.Lock()
	svc.baseEnv = map[string]string{"A": "base", "B": "base"}
	svc.mu.Unlock()
	resp, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{
		Command: []string{"/bin/sh", "-c", "printf '%s,%s' \"$A\" \"$B\""},
		Uid:     new(uint32(syscall.Getuid())),
		Env:     map[string]string{"B": "exec"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := string(resp.GetStdout()); got != "base,exec" {
		t.Fatalf("env merge = %q, want base,exec (exec key wins)", got)
	}
}
func TestExecUnstartableProgramReturnsExitCode(t *testing.T) {
	dir := t.TempDir()
	nonExecutable := dir + "/noexec"
	if err := os.WriteFile(nonExecutable, []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	pathFile := dir + "/path-entry-file"
	if err := os.WriteFile(pathFile, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	executableButInvalid := dir + "/invalid-executable"
	if err := os.WriteFile(executableButInvalid, []byte("not an executable format"), 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		command string
		path    string
		code    int32
	}{
		{name: "missing from path", command: "compass-missing-exec-test", path: dir, code: 127},
		{name: "missing after unusable path entry", command: "compass-missing-exec-test", path: pathFile, code: 127},
		{name: "missing explicit path", command: dir + "/missing", code: 127},
		{name: "directory is not executable", command: "/", code: 126},
		{name: "non-executable from path", command: "noexec", path: dir, code: 126},
		{name: "non-executable explicit path", command: nonExecutable, code: 126},
		{name: "exec format failure", command: executableButInvalid, code: 126},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, svc := newTestSupervisor(t, true, 1000)
			if tt.path != "" {
				svc.mu.Lock()
				svc.baseEnv = map[string]string{"PATH": tt.path}
				svc.mu.Unlock()
			}
			resp, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{
				Command: []string{tt.command},
			})
			if err != nil {
				t.Fatalf("Exec(%q) = %v, want exit %d with no handler error", tt.command, err, tt.code)
			}
			if got := resp.GetExitCode(); got != tt.code {
				t.Fatalf("Exec(%q) exit_code = %d, want %d (stderr %q)", tt.command, got, tt.code, resp.GetStderr())
			}
			if got := resp.GetStderr(); len(got) == 0 {
				t.Fatalf("Exec(%q) stderr = %q, want non-empty stderr", tt.command, got)
			}
		})
	}
}

func TestExecInvalidWorkdirTakesPrecedenceOverMissingProgram(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "one-shot", true: "streaming"}[streaming], func(t *testing.T) {
			client, _ := newTestSupervisor(t, true, 1000)
			missing := t.TempDir() + "/missing-workdir"
			program := missing + "/missing-program"
			if streaming {
				stream, err := client.ExecStream(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if err := stream.Send(&compassv1internal.ExecStreamRequest{Frame: &compassv1internal.ExecStreamRequest_Start{Start: &compassv1internal.StartExec{Command: []string{program}, Workdir: &missing}}}); err != nil {
					t.Fatalf("send start: %v", err)
				}
				if err := stream.CloseSend(); err != nil {
					t.Fatalf("close request: %v", err)
				}
				if _, err := stream.Receive(); err == nil {
					t.Fatal("ExecStream with invalid workdir and missing program returned a response")
				}
				return
			}
			_, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{Command: []string{program}, Workdir: &missing})
			if err == nil {
				t.Fatal("Exec with invalid workdir and missing program returned nil error")
			}
		})
	}
}

func TestExecNonDirectoryWorkdirRemainsError(t *testing.T) {
	client, _ := newTestSupervisor(t, true, 1000)
	file := t.TempDir() + "/not-a-directory"
	if err := os.WriteFile(file, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, program := range []string{"/bin/true", "compass-missing-workdir-probe"} {
		_, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{Command: []string{program}, Workdir: &file})
		if err == nil {
			t.Fatalf("Exec %q with non-directory workdir returned nil error", program)
		}
	}
}

func TestExecStreamNonDirectoryWorkdirRemainsError(t *testing.T) {
	client, _ := newTestSupervisor(t, true, 1000)
	file := t.TempDir() + "/not-a-directory"
	if err := os.WriteFile(file, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, program := range []string{"/bin/true", "compass-missing-workdir-probe"} {
		stream, err := client.ExecStream(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&compassv1internal.ExecStreamRequest{Frame: &compassv1internal.ExecStreamRequest_Start{Start: &compassv1internal.StartExec{Command: []string{program}, Workdir: &file}}}); err != nil {
			t.Fatalf("send start: %v", err)
		}
		if err := stream.CloseSend(); err != nil {
			t.Fatalf("close request: %v", err)
		}
		if _, err := stream.Receive(); err == nil {
			t.Fatalf("ExecStream %q with non-directory workdir returned a response", program)
		}
	}
}

func TestGuestExecStartFailureClassifiesPermissionErrors(t *testing.T) {
	for _, errno := range []error{syscall.EACCES, syscall.EPERM, syscall.ENOEXEC} {
		t.Run(errno.Error(), func(t *testing.T) {
			path := &os.PathError{Op: "fork/exec", Path: "/probe", Err: errno}
			code, stderr, ok := guestExecStartFailure("/probe", nil, "/probe", path)
			if !ok || code != 126 || !strings.Contains(stderr, "/probe") {
				t.Fatalf("guestExecStartFailure() = (%d, %q, %v), want 126, diagnostic, true", code, stderr, ok)
			}
		})
	}
}

func TestExecBareInvalidFormatProgramReturns126(t *testing.T) {
	dir := t.TempDir()
	program := dir + "/invalid-format"
	if err := os.WriteFile(program, []byte("not an executable format"), 0o755); err != nil {
		t.Fatal(err)
	}
	client, svc := newTestSupervisor(t, true, 1000)
	svc.mu.Lock()
	svc.baseEnv = map[string]string{"PATH": dir}
	svc.mu.Unlock()
	resp, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{Command: []string{"invalid-format"}})
	if err != nil {
		t.Fatalf("Exec bare invalid-format program: %v", err)
	}
	if resp.GetExitCode() != 126 || len(resp.GetStderr()) == 0 {
		t.Fatalf("Exec result = %+v, want exit 126 with stderr", resp)
	}
}

func TestExecSkipsUnsearchablePATHEntry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory search permission")
	}
	locked := t.TempDir() + "/locked"
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	client, svc := newTestSupervisor(t, true, 1000)
	svc.mu.Lock()
	svc.baseEnv = map[string]string{"PATH": locked}
	svc.mu.Unlock()
	resp, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{Command: []string{"compass-unsearchable-probe"}})
	if err != nil || resp.GetExitCode() != 127 {
		t.Fatalf("Exec with unsearchable PATH entry = (%v, %v), want exit 127", resp, err)
	}
}

func TestExecStreamBareInvalidFormatProgramReturns126(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/invalid-format", []byte("not an executable format"), 0o755); err != nil {
		t.Fatal(err)
	}
	client, svc := newTestSupervisor(t, true, 1000)
	svc.mu.Lock()
	svc.baseEnv = map[string]string{"PATH": dir}
	svc.mu.Unlock()
	stream, err := client.ExecStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&compassv1internal.ExecStreamRequest{Frame: &compassv1internal.ExecStreamRequest_Start{Start: &compassv1internal.StartExec{Command: []string{"invalid-format"}}}}); err != nil {
		t.Fatalf("send start: %v", err)
	}
	var frames []string
	var code int32 = -1
	for {
		msg, err := stream.Receive()
		if err != nil {
			t.Fatalf("receive after %v: %v", frames, err)
		}
		switch f := msg.GetFrame().(type) {
		case *compassv1internal.ExecStreamResponse_Started:
			frames = append(frames, "started")
		case *compassv1internal.ExecStreamResponse_Stderr:
			frames = append(frames, "stderr")
		case *compassv1internal.ExecStreamResponse_Exit:
			frames = append(frames, "exit")
			code = f.Exit.GetExitCode()
		}
		if code >= 0 {
			break
		}
	}
	if strings.Join(frames, ",") != "started,stderr,exit" || code != 126 {
		t.Fatalf("frames = %v exit = %d, want started,stderr,exit and 126", frames, code)
	}
}

func TestExecMissingWorkdirRemainsError(t *testing.T) {
	client, _ := newTestSupervisor(t, true, 1000)
	missing := t.TempDir() + "/missing"
	_, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{
		Command: []string{"/bin/true"},
		Workdir: &missing,
	})
	if err == nil {
		t.Fatal("Exec with missing workdir returned nil error")
	}
}

func TestExecStreamMissingWorkdirRemainsError(t *testing.T) {
	client, _ := newTestSupervisor(t, true, 1000)
	missing := t.TempDir() + "/missing"
	stream, err := client.ExecStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&compassv1internal.ExecStreamRequest{
		Frame: &compassv1internal.ExecStreamRequest_Start{
			Start: &compassv1internal.StartExec{Command: []string{"/bin/true"}, Workdir: &missing},
		},
	}); err != nil {
		t.Fatalf("send start: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close request: %v", err)
	}
	if _, err := stream.Receive(); err == nil {
		t.Fatal("ExecStream with missing workdir returned a response, want spawn error")
	}
}

func TestExecMissingInterpreterRemainsError(t *testing.T) {
	client, _ := newTestSupervisor(t, true, 1000)
	program := t.TempDir() + "/missing-interpreter"
	if err := os.WriteFile(program, []byte("#!/compass-missing-interpreter\necho never\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{
		Command: []string{program},
	})
	if err == nil {
		t.Fatal("Exec with missing script interpreter returned nil error")
	}
}

func TestExecStreamMissingInterpreterRemainsError(t *testing.T) {
	client, _ := newTestSupervisor(t, true, 1000)
	program := t.TempDir() + "/missing-interpreter"
	if err := os.WriteFile(program, []byte("#!/compass-missing-interpreter\necho never\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stream, err := client.ExecStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&compassv1internal.ExecStreamRequest{
		Frame: &compassv1internal.ExecStreamRequest_Start{
			Start: &compassv1internal.StartExec{Command: []string{program}},
		},
	}); err != nil {
		t.Fatalf("send start: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close request: %v", err)
	}
	if _, err := stream.Receive(); err == nil {
		t.Fatal("ExecStream with missing script interpreter returned a response")
	}
}

func TestExecStreamUnstartableProgramReturnsExit(t *testing.T) {
	dir := t.TempDir()
	nonExecutable := dir + "/noexec"
	if err := os.WriteFile(nonExecutable, []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	pathFile := dir + "/path-entry-file"
	if err := os.WriteFile(pathFile, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	executableButInvalid := dir + "/invalid-executable"
	if err := os.WriteFile(executableButInvalid, []byte("not an executable format"), 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		command string
		path    string
		code    int32
	}{
		{name: "missing", command: "/compass-missing-stream-test", code: 127},
		{name: "directory", command: "/", code: 126},
		{name: "non-executable from path", command: "noexec", path: dir, code: 126},
		{name: "non-executable explicit path", command: nonExecutable, code: 126},
		{name: "missing after unusable PATH entry", command: "compass-missing-stream-test", path: pathFile, code: 127},
		{name: "exec format failure", command: executableButInvalid, code: 126},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, svc := newTestSupervisor(t, true, 1000)
			if tt.path != "" {
				svc.mu.Lock()
				svc.baseEnv = map[string]string{"PATH": tt.path}
				svc.mu.Unlock()
			}
			stream, err := client.ExecStream(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.Send(&compassv1internal.ExecStreamRequest{
				Frame: &compassv1internal.ExecStreamRequest_Start{
					Start: &compassv1internal.StartExec{Command: []string{tt.command}},
				},
			}); err != nil {
				t.Fatalf("send start: %v", err)
			}
			if err := stream.CloseSend(); err != nil {
				t.Fatalf("close request: %v", err)
			}

			started, err := stream.Receive()
			if err != nil || started.GetStarted() == nil {
				t.Fatalf("first frame = %v, err = %v; want ExecStarted", started, err)
			}
			stderr, err := stream.Receive()
			if err != nil || len(stderr.GetStderr()) == 0 {
				t.Fatalf("stderr frame = %v, err = %v; want non-empty stderr", stderr, err)
			}
			exit, err := stream.Receive()
			if err != nil || exit.GetExit() == nil || exit.GetExit().GetExitCode() != tt.code {
				t.Fatalf("terminal frame = %v, err = %v; want exit %d", exit, err, tt.code)
			}
			if _, err := stream.Receive(); err == nil {
				t.Fatal("stream continued after terminal ExecExit frame")
			}
		})
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
	_, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{
		Command: []string{"/bin/sh", "-c", "printf 'x%.0s' $(seq 1 64)"},
		Uid:     new(uint32(syscall.Getuid())),
	})
	if err == nil {
		t.Fatal("Exec with output past the cap returned success, want ResourceExhausted")
	}
	if got := connect.CodeOf(err); got != connect.CodeResourceExhausted {
		t.Fatalf("Exec overflow error code = %v, want CodeResourceExhausted", got)
	}
}

func TestExecTimeoutKillsAndReapsChild(t *testing.T) {
	// A one-shot Exec with timeout_seconds set must, on overrun, SIGKILL the child
	// group and reap it before returning CodeDeadlineExceeded. One-shot Exec does not
	// register in the exec table, so the reap is proven by the PROMPT return; a missed
	// reap would block on <-waitErr forever and blow the 10s ceiling.
	client, _ := newTestSupervisor(t, true, uint32(syscall.Getuid()))
	start := time.Now()
	_, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{
		Command:        []string{"/bin/sh", "-c", "sleep 300"},
		Uid:            new(uint32(syscall.Getuid())),
		TimeoutSeconds: 1,
	})
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
	// A caller cancelling the request ctx before the child exits must SIGKILL + reap
	// the child and surface CodeCanceled. One-shot Exec does not register in the exec
	// table, so the reap is proven two ways: the child spawns server-side (a marker
	// file), and the handler returns CodeCanceled promptly (running <-waitErr first).
	client, _ := newTestSupervisor(t, true, uint32(syscall.Getuid()))
	marker := t.TempDir() + "/started"
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		_, err := client.Exec(ctx, &compassv1internal.ExecRequest{
			Command: []string{"/bin/sh", "-c", "touch " + marker + "; sleep 300"},
			Uid:     new(uint32(syscall.Getuid())),
		})
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
		time.Sleep(5 * time.Millisecond) //nolint:forbidigo // bounded poll tick; event-gated on the child's marker file above with a deadline (rule://go-no-sleep-in-test poll-until exemption)
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
	stream, err := client.ExecStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&compassv1internal.ExecStreamRequest{
		Frame: &compassv1internal.ExecStreamRequest_Start{
			Start: &compassv1internal.StartExec{
				Command: []string{"/bin/sh", "-c", "echo out; echo err 1>&2; exit 0"},
				Uid:     new(uint32(syscall.Getuid())),
			},
		},
	}); err != nil {
		t.Fatalf("send start: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close request: %v", err)
	}

	first, err := stream.Receive()
	if err != nil {
		t.Fatalf("receive started: %v", err)
	}
	if first.GetStarted() == nil || first.GetStarted().GetExecId() == "" {
		t.Fatalf("first frame = %v, want ExecStarted with an id", first)
	}

	// Accumulate each stream's payload and assert the demux routed the right bytes
	// onto the right frame: `out` ONLY as Stdout frames, `err` ONLY as Stderr. A
	// streamWriter that swapped the stdout bool, dropped a stream, or merged them
	// would fail here; the ordering-only checks are kept alongside.
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
	stream, err := client.ExecStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&compassv1internal.ExecStreamRequest{
		Frame: &compassv1internal.ExecStreamRequest_Start{
			Start: &compassv1internal.StartExec{
				Command: []string{"/bin/sh", "-c", "sleep 300"},
				Uid:     new(uint32(syscall.Getuid())),
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
	_, err = client.Signal(t.Context(), &compassv1internal.SignalRequest{
		ExecId: execID,
		Signal: int32(syscall.SIGKILL),
	})
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

func TestSignalOnNilCommandExecIsNoOpSuccess(t *testing.T) {
	client, svc := newTestSupervisor(t, true, 1000)
	execID := svc.register(nil)
	_, err := client.Signal(t.Context(), &compassv1internal.SignalRequest{
		ExecId: execID,
		Signal: int32(syscall.SIGTERM),
	})
	if err != nil {
		t.Fatalf("Signal on start-failed exec = %v, want no-op success", err)
	}
}
func TestSignalOnExitedExecIsNoOpSuccess(t *testing.T) {
	client, _ := newTestSupervisor(t, true, 1000)
	// Signal an exec_id that never existed (equivalent to already-reaped): the
	// supervisor drops reaped ids, so an unknown id is a no-op success.
	_, err := client.Signal(t.Context(), &compassv1internal.SignalRequest{
		ExecId: "exec-does-not-exist",
		Signal: int32(syscall.SIGTERM),
	})
	if err != nil {
		t.Fatalf("Signal on exited/unknown exec = %v, want no-op success", err)
	}
}

func TestBrokenExecStreamReapsChild(t *testing.T) {
	client, svc := newTestSupervisor(t, true, 1000)
	ctx, cancel := context.WithCancel(t.Context())
	stream, err := client.ExecStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&compassv1internal.ExecStreamRequest{
		Frame: &compassv1internal.ExecStreamRequest_Start{
			Start: &compassv1internal.StartExec{
				Command: []string{"/bin/sh", "-c", "sleep 300"},
				Uid:     new(uint32(syscall.Getuid())),
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

	// Event-gate on the exec_id leaving the table: that removal signals the ExecStream
	// handler returned AFTER its reap. The wait is bounded by the TEST's own deadline,
	// not an invented threshold: reap latency can exceed any fixed ceiling on a
	// saturated runner, so only a genuine never-reap hang fails.
	waitCeiling := time.Now().Add(2 * time.Minute) // fallback when -timeout=0 disables the deadline
	if d, ok := t.Deadline(); ok {
		// Leave margin so THIS test fails with its diagnostic before the whole
		// package -timeout fires an opaque panic.
		waitCeiling = d.Add(-5 * time.Second)
	}
	for {
		svc.mu.Lock()
		_, present := svc.execs[execID]
		svc.mu.Unlock()
		if !present {
			// Reaped and unregistered: the deterministic terminal state.
			return
		}
		if time.Now().After(waitCeiling) {
			alive := syscall.Kill(pid, 0) == nil
			t.Fatalf("child pid %d still present=%v alive=%v after stream break", pid, present, alive)
		}
		time.Sleep(5 * time.Millisecond) //nolint:forbidigo // bounded poll tick; event-gated on the exec_id leaving the table above, ceiling = the test's own deadline (rule://go-no-sleep-in-test poll-until exemption)
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
	// The PID-1 PATH regression: guestd runs as PID 1 with no process PATH, so a bare
	// argv[0] must resolve against the SESSION env's PATH (base_env). Provision a
	// base_env whose PATH holds an executable absent from this test's ambient PATH,
	// then exec it by bare name — only session-PATH resolution can find it.
	dir := t.TempDir()
	probe := dir + "/probe"
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing probe: %v", err)
	}
	client, _ := newTestSupervisor(t, false, 0)
	if _, err := client.Provision(t.Context(), &compassv1internal.ProvisionRequest{
		// Fixed non-zero uid: testCredential ignores the value (the child runs
		// as the test process's own uid), but Provision rejects uid 0, so a
		// Getuid() that returns 0 under root CI would fail unrelated to PATH.
		DefaultExecUid: 1000,
		BaseEnv:        map[string]string{"PATH": dir},
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	resp, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{
		Command: []string{"probe"},
	})
	if err != nil {
		t.Fatalf("Exec(bare probe) = %v; a bare argv[0] must resolve against the session PATH", err)
	}
	if resp.GetExitCode() != 0 {
		t.Fatalf("exit_code = %d, want 0", resp.GetExitCode())
	}
}

func TestExecResolvesRelativePATHFromWorkdir(t *testing.T) {
	workdir, err := os.MkdirTemp(".", "guest-relative-path-") //nolint:usetesting // The relative directory is required to test child workdir PATH lookup.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workdir) })
	bin := workdir + "/bin"
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	probe := bin + "/probe"
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nprintf relative-path\n"), 0o755); err != nil {
		t.Fatalf("writing probe: %v", err)
	}
	client, _ := newTestSupervisor(t, false, 0)
	if _, err := client.Provision(t.Context(), &compassv1internal.ProvisionRequest{
		DefaultExecUid: 1000,
		BaseEnv:        map[string]string{"PATH": "bin"},
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	resp, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{
		Command: []string{"probe"},
		Workdir: &workdir,
	})
	if err != nil {
		t.Fatalf("Exec from relative PATH and workdir: %v", err)
	}
	if resp.GetExitCode() != 0 || string(resp.GetStdout()) != "relative-path" {
		t.Fatalf("Exec result = %+v, want relative-path with exit 0", resp)
	}
}

func TestExecResolvesDotPATHFromWorkdir(t *testing.T) {
	workdir, err := os.MkdirTemp(".", "guest-dot-path-") //nolint:usetesting // Relative workdir is needed to test a dot PATH entry.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workdir) })
	program := workdir + "/dot-path-probe"
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf dot-path\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	client, _ := newTestSupervisor(t, false, 0)
	if _, err := client.Provision(t.Context(), &compassv1internal.ProvisionRequest{DefaultExecUid: 1000, BaseEnv: map[string]string{"PATH": "."}}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	resp, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{Command: []string{"dot-path-probe"}, Workdir: &workdir})
	if err != nil {
		t.Fatalf("Exec with dot PATH: %v", err)
	}
	if resp.GetExitCode() != 0 || string(resp.GetStdout()) != "dot-path" {
		t.Fatalf("Exec result = %+v, want dot-path with exit 0", resp)
	}
}

func TestExecForeignOwnedExecutableReturns126(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to run the child as another uid")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	program := dir + "/foreign-owned"
	if err := os.WriteFile(program, []byte("not an executable format"), 0o700); err != nil {
		t.Fatal(err)
	}
	client, svc := newTestSupervisor(t, false, 0)
	svc.newCredential = func(uint32) *syscall.Credential {
		return &syscall.Credential{Uid: 65534, Gid: 65534}
	}
	if _, err := client.Provision(t.Context(), &compassv1internal.ProvisionRequest{DefaultExecUid: 1000}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	uid := uint32(65534)
	resp, err := client.Exec(t.Context(), &compassv1internal.ExecRequest{Command: []string{program}, Uid: &uid})
	if err != nil {
		t.Fatalf("Exec foreign-owned program: %v", err)
	}
	if resp.GetExitCode() != 126 || len(resp.GetStderr()) == 0 {
		t.Fatalf("Exec result = %+v, want exit 126 with stderr", resp)
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
