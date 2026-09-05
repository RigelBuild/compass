//go:build (linux && gtk4) || darwin

package main

// Embedded launch-pipeline gate. The pipeline is exercised through its Go
// entrypoints with INJECTED effects — no real podman/compass-stack/exec — so
// mode-select, preflight short-circuit, the exact compass-stack argv, the WhoAmI
// hop, and the two error paths are all verified deterministically. The one seam
// wired to a real transport is whoAmIOverUDS, driven against a REAL in-process
// compass.v1 WhoAmI server over h2c on a Unix socket (mirroring the
// bridge-service gate's stubServer and internal/runner/e2e_transport_test.go's
// UDS/connect pattern), so the h2c-UDS dial is proven on the wire it ships on.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/preflight"
)

const embeddedTestTimeout = 5 * time.Second

// baseParams is a representative resolved launch input the argv/dial assertions
// key off. The socket is a fixed path (the tests never dial it except in the
// WhoAmI-server case, which overrides it).
var baseParams = embeddedParams{
	socket:   "/run/compass/server.sock",
	stateDir: "/state/compass",
	image:    "ghcr.io/rigelbuild/compass-agent:latest",
}

// stubPipeline builds an embeddedPipeline whose three seams are deterministic
// stubs, recording what the orchestration invoked. Each seam defaults to a
// success no-op; a test overrides the ones it drives.
type recorder struct {
	preflightCalled bool
	stackUpCalled   bool
	stackUpArgs     []string
	whoAmICalled    bool
	whoAmISocket    string
}

func stubPipeline(rec *recorder, preflightErr, stackUpErr, whoAmIErr error, accountID string) embeddedPipeline {
	return embeddedPipeline{
		preflight: func(_ context.Context) error {
			rec.preflightCalled = true
			return preflightErr
		},
		stackUp: func(_ context.Context, args []string) error {
			rec.stackUpCalled = true
			rec.stackUpArgs = args
			return stackUpErr
		},
		whoAmI: func(_ context.Context, socket string) (string, error) {
			rec.whoAmICalled = true
			rec.whoAmISocket = socket
			return accountID, whoAmIErr
		},
	}
}

// runEmbeddedStub runs runEmbedded with a recording stackDown seam so the quit
// controller wiring is observable without a real exec.
func runEmbeddedStub(
	t *testing.T, pipeline embeddedPipeline,
) (string, *quitController, error) {
	t.Helper()
	stackDown := func(_ context.Context, _ []string) error { return nil }
	return runEmbedded(context.Background(), pipeline, baseParams, stackDown)
}

// TestRunEmbeddedHappyPath: embedded mode runs preflight → stack up → WhoAmI in
// order, passes the SAME socket to the dial that the argv carries, returns the
// resolved account id, and builds a quit controller wired to the params. Asserting
// the argv (up, --socket, --state-dir, --image) is the stack-invocation contract;
// asserting whoAmISocket == socket is the single-socket invariant (the value
// passed to --socket IS the value dialed).
func TestRunEmbeddedHappyPath(t *testing.T) {
	rec := &recorder{}
	pipeline := stubPipeline(rec, nil, nil, nil, "acc-42")

	id, quitter, err := runEmbeddedStub(t, pipeline)
	if err != nil {
		t.Fatalf("embedded happy path err = %v, want nil", err)
	}
	if id != "acc-42" {
		t.Errorf("account id = %q, want acc-42", id)
	}
	if quitter == nil {
		t.Fatal("embedded mode returned a nil quit controller, want one wired to the stack teardown")
	}
	if quitter.params != baseParams {
		t.Errorf("quit controller params = %+v, want %+v", quitter.params, baseParams)
	}
	if !rec.preflightCalled || !rec.stackUpCalled || !rec.whoAmICalled {
		t.Fatalf("not every stage ran: %+v", rec)
	}
	assertArg(t, rec.stackUpArgs, "up")
	assertArgPair(t, rec.stackUpArgs, "--socket", baseParams.socket)
	assertArgPair(t, rec.stackUpArgs, "--state-dir", baseParams.stateDir)
	assertArgPair(t, rec.stackUpArgs, "--image", baseParams.image)
	if rec.whoAmISocket != baseParams.socket {
		t.Errorf("WhoAmI dialed %q, want the SAME socket passed to --socket %q",
			rec.whoAmISocket, baseParams.socket)
	}
}

// TestRunEmbeddedPreflightShortCircuits: a preflight failure returns the
// aggregated legible error VERBATIM, never proceeds to stack-up or WhoAmI, and
// builds no quit controller. Mutation that reddens it: running the checks after a
// failure, or reformatting Results.Err's copy.
func TestRunEmbeddedPreflightShortCircuits(t *testing.T) {
	rec := &recorder{}
	preflightErr := errors.New("embedded-mode preflight failed:\n  - windows is not supported")
	pipeline := stubPipeline(rec, preflightErr, nil, nil, "acc-x")

	id, quitter, err := runEmbeddedStub(t, pipeline)
	if !errors.Is(err, preflightErr) {
		t.Fatalf("preflight-fail err = %v, want the preflight error verbatim", err)
	}
	if id != "" {
		t.Errorf("account id = %q, want empty on preflight failure", id)
	}
	if quitter != nil {
		t.Error("preflight failure returned a quit controller, want nil")
	}
	if !rec.preflightCalled {
		t.Error("preflight did not run")
	}
	if rec.stackUpCalled || rec.whoAmICalled {
		t.Errorf("pipeline proceeded past a failed preflight: %+v", rec)
	}
}

// TestRunEmbeddedStackUpFails: a non-zero compass-stack up exit is surfaced and
// the pipeline stops before WhoAmI. The stackUp seam already folds stderr into
// its error (see TestRunStackUpNonZeroExitSurfacesStderr); here the contract is
// that runEmbedded propagates it and does not dial.
func TestRunEmbeddedStackUpFails(t *testing.T) {
	rec := &recorder{}
	stackErr := errors.New("compass-stack up failed: exit status 1: postgres refused")
	pipeline := stubPipeline(rec, nil, stackErr, nil, "acc-x")

	id, quitter, err := runEmbeddedStub(t, pipeline)
	if !errors.Is(err, stackErr) {
		t.Fatalf("stack-up-fail err = %v, want the stack-up error", err)
	}
	if id != "" {
		t.Errorf("account id = %q, want empty on stack-up failure", id)
	}
	if quitter != nil {
		t.Error("stack-up failure returned a quit controller, want nil")
	}
	if rec.whoAmICalled {
		t.Error("pipeline dialed WhoAmI after a failed stack-up")
	}
}

// TestRunEmbeddedWhoAmIFails: a WhoAmI error is surfaced (wrapped with the socket
// for context) and no account id is returned. Mutation that reddens it:
// swallowing the WhoAmI error and returning an empty id as success.
func TestRunEmbeddedWhoAmIFails(t *testing.T) {
	rec := &recorder{}
	whoErr := errors.New("connect: connection refused")
	pipeline := stubPipeline(rec, nil, nil, whoErr, "")

	id, quitter, err := runEmbeddedStub(t, pipeline)
	if !errors.Is(err, whoErr) {
		t.Fatalf("whoami-fail err = %v, want the WhoAmI error wrapped", err)
	}
	if id != "" {
		t.Errorf("account id = %q, want empty on WhoAmI failure", id)
	}
	if quitter != nil {
		t.Error("WhoAmI failure returned a quit controller, want nil")
	}
	if !strings.Contains(err.Error(), baseParams.socket) {
		t.Errorf("WhoAmI error %q does not name the socket for context", err.Error())
	}
}

// TestStackUpArgsOmitsCLIDefaultedFlags: the pure argv builder passes ONLY
// --state-dir/--image/--socket. It omits --database (compass-stack recomputes the
// identical default DSN from --state-dir) AND the flags the CLI defaults on its
// own — --postgres-image, --collector-image, --listen — so the CLI's defaults are
// the contract and the app re-learns nothing the stack already owns (§A2
// reconciliation 2).
func TestStackUpArgsOmitsCLIDefaultedFlags(t *testing.T) {
	args := stackUpArgs(baseParams)
	for _, flag := range []string{
		"--database", "--postgres-image", "--collector-image", "--listen",
	} {
		if slices.Contains(args, flag) {
			t.Errorf("argv carries %s, want it omitted so compass-stack owns the default: %v", flag, args)
		}
	}
}

// stubWhoAmIServer implements just the WhoAmI RPC over the generated
// CompassService handler; every other method returns Unimplemented. accountID is
// what WhoAmI reports; whoErr, when non-nil, is returned instead.
type stubWhoAmIServer struct {
	compassv1connect.UnimplementedCompassServiceHandler
	accountID string
	whoErr    error
}

func (s *stubWhoAmIServer) WhoAmI(
	_ context.Context, _ *connect.Request[compassv1.WhoAmIRequest],
) (*connect.Response[compassv1.WhoAmIResponse], error) {
	if s.whoErr != nil {
		return nil, s.whoErr
	}
	return connect.NewResponse(&compassv1.WhoAmIResponse{AccountId: s.accountID}), nil
}

// serveWhoAmI stands up a real h2c compass.v1 server on a UDS listener, torn
// down via t.Cleanup, and returns the socket path whoAmIOverUDS dials.
func serveWhoAmI(t *testing.T, srv *stubWhoAmIServer) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "server.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	_, handler := compassv1connect.NewCompassServiceHandler(srv)
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	httpSrv := &http.Server{Handler: handler, Protocols: p}
	go func() { _ = httpSrv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), embeddedTestTimeout)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	})
	return socket
}

// TestWhoAmIOverUDSReturnsAccountID: the real h2c-UDS WhoAmI dial returns the
// server-derived account id. This proves the transport shape (borrowed from
// health.go) actually speaks to a compass.v1 server over the socket.
func TestWhoAmIOverUDSReturnsAccountID(t *testing.T) {
	socket := serveWhoAmI(t, &stubWhoAmIServer{accountID: "acc-served"})
	ctx, cancel := context.WithTimeout(context.Background(), embeddedTestTimeout)
	defer cancel()

	id, err := whoAmIOverUDS(ctx, socket)
	if err != nil {
		t.Fatalf("whoAmIOverUDS err = %v, want nil", err)
	}
	if id != "acc-served" {
		t.Errorf("account id = %q, want acc-served", id)
	}
}

// TestWhoAmIOverUDSSurfacesError: an RPC error from the server surfaces as a
// non-nil error and an empty id (the dial does not fabricate an identity).
func TestWhoAmIOverUDSSurfacesError(t *testing.T) {
	srv := &stubWhoAmIServer{whoErr: connect.NewError(connect.CodeUnavailable, errors.New("starting"))}
	socket := serveWhoAmI(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), embeddedTestTimeout)
	defer cancel()

	id, err := whoAmIOverUDS(ctx, socket)
	if err == nil {
		t.Fatal("whoAmIOverUDS err = nil, want the server's error surfaced")
	}
	if id != "" {
		t.Errorf("account id = %q, want empty on a WhoAmI error", id)
	}
}

// TestRunStackUpNonZeroExitSurfacesStderr: the real stackUp seam surfaces a
// non-zero exit as an error carrying the child's stderr, so the failure copy is
// legible. Driven with /bin/sh printing to stderr and exiting 1 — no real
// compass-stack (the argv is not compass-stack's; only the exec+stderr contract
// is under test here).
func TestRunStackUpNonZeroExitSurfacesStderr(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), embeddedTestTimeout)
	defer cancel()

	stackUp := runStackUp("/bin/sh")
	err := stackUp(ctx, []string{"-c", "echo 'boom on stderr' >&2; exit 1"})
	if err == nil {
		t.Fatal("stackUp err = nil, want a non-zero-exit error")
	}
	if !strings.Contains(err.Error(), "boom on stderr") {
		t.Errorf("stackUp error %q does not carry the child stderr", err.Error())
	}
}

// TestRunStackUpZeroExitSucceeds: a zero-exit child (fire-and-return) returns
// nil — the Ready postcondition compass-stack up encodes.
func TestRunStackUpZeroExitSucceeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), embeddedTestTimeout)
	defer cancel()

	stackUp := runStackUp("/bin/sh")
	if err := stackUp(ctx, []string{"-c", "exit 0"}); err != nil {
		t.Fatalf("stackUp on a zero exit err = %v, want nil", err)
	}
}

// TestRunStackUpReturnsWhileChildrenLinger is the regression guard for the
// fire-and-return hang: `compass-stack up` exits 0 once the stack is Ready while
// its postgres/server/runner children keep running, and those children inherit
// the exec'd command's stderr. If runStackUp captured stderr into a bytes.Buffer
// (os/exec's pipe + copy-goroutine path), cmd.Wait would block until the pipe
// hit EOF — which the lingering children hold open — so Run would hang for the
// children's whole lifetime. Capturing to a temp *os.File (captureStderr) makes
// Run return the instant the top-level child exits, regardless of survivors.
//
// Driven with /bin/sh that backgrounds a long sleep (a stand-in for the
// reparented stack children) holding stderr, then exits 0. Pre-fix this blocks
// for the sleep's lifetime; the fix returns immediately. The assertion is that
// Run completes well under the sleep — a plain wall-clock bound, but the pre-fix
// gap is enormous, so it is not flaky. The backgrounded sleep is cleaned up via
// its own short lifetime; the test spawns nothing it must kill
// (rule://process-safety — never pkill).
func TestRunStackUpReturnsWhileChildrenLinger(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), embeddedTestTimeout)
	defer cancel()

	// A short-lived grandchild that outlives its parent and inherits stderr: the
	// exact fire-and-return shape of `compass-stack up`. sleep 5 is far longer
	// than any correct runStackUp (which returns at the parent's exit, ~ms) and
	// well past the 1s assertion below, yet short enough that a regressed run's
	// leaked grandchild self-reaps in seconds rather than a minute.
	stackUp := runStackUp("/bin/sh")
	start := time.Now()
	err := stackUp(ctx, []string{"-c", "sleep 5 & exit 0"})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("stackUp with a lingering child err = %v, want nil", err)
	}
	// Generous bound: the fix returns in single-digit ms, while a regressed Run
	// blocks until the grandchild exits (~5s) — far past 1s. Anything under a
	// second proves Run did not wait on the grandchild.
	if elapsed > time.Second {
		t.Fatalf("stackUp took %s with a lingering child — Run waited on the inherited stderr pipe (the fire-and-return hang regressed)", elapsed)
	}
}

// TestRunStackDownNonZeroExitSurfacesStderr: the real stackDown seam surfaces a
// non-zero exit as an error carrying the child's stderr, mirroring runStackUp.
// Driven with /bin/sh printing to stderr and exiting 1 — no real compass-stack
// (the argv is not compass-stack's; only the exec+stderr contract is tested).
func TestRunStackDownNonZeroExitSurfacesStderr(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), embeddedTestTimeout)
	defer cancel()

	stackDown := runStackDown("/bin/sh")
	err := stackDown(ctx, []string{"-c", "echo 'down boom on stderr' >&2; exit 1"})
	if err == nil {
		t.Fatal("stackDown err = nil, want a non-zero-exit error")
	}
	if !strings.Contains(err.Error(), "down boom on stderr") {
		t.Errorf("stackDown error %q does not carry the child stderr", err.Error())
	}
}

// TestRunStackDownZeroExitSucceeds: a zero-exit child (a clean teardown) returns
// nil — the postcondition compass-stack down encodes.
func TestRunStackDownZeroExitSucceeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), embeddedTestTimeout)
	defer cancel()

	stackDown := runStackDown("/bin/sh")
	if err := stackDown(ctx, []string{"-c", "exit 0"}); err != nil {
		t.Fatalf("stackDown on a zero exit err = %v, want nil", err)
	}
}

// classifyDeps returns a preflight.Deps whose every injected effect passes (host
// GOOS linux) — mirroring preflight_test.go's okDeps. Tests override individual
// fields to drive one failing check at a time through classifyPreflight.
func classifyDeps() preflight.Deps {
	return preflight.Deps{
		GOOS:           "linux",
		PodmanRootless: func(context.Context) error { return nil },
		PodmanVersion:  func(context.Context) error { return nil },
		ImagePresent:   func(context.Context, string) error { return nil },
	}
}

var classifyParams = preflight.Params{
	AgentImage: "ghcr.io/rigelbuild/compass-agent:latest",
}

// classify runs deps and folds through the boundary classifier, the exact path
// realPreflight uses.
func classify(t *testing.T, deps preflight.Deps) error {
	t.Helper()
	return classifyPreflight(deps.Run(context.Background(), classifyParams))
}

// TestClassifyPreflightAllPass: no failing check → nil.
func TestClassifyPreflightAllPass(t *testing.T) {
	if err := classify(t, classifyDeps()); err != nil {
		t.Fatalf("all-pass classify err = %v, want nil", err)
	}
}

// TestClassifyPreflightHostCapUnmetIsFatal: each host-capability check (OS,
// podman, podman-version) that `up` CANNOT create is fatal — classifyPreflight
// returns non-nil, naming the failed check's copy.
func TestClassifyPreflightHostCapUnmetIsFatal(t *testing.T) {
	podmanErr := errors.New("no rootless podman here")
	versionErr := errors.New("podman 4.3 or newer is required")
	cases := map[string]struct {
		mutate func(*preflight.Deps)
		want   string
	}{
		"os": {
			mutate: func(d *preflight.Deps) { d.GOOS = "windows" },
			want:   "windows",
		},
		"podman": {
			mutate: func(d *preflight.Deps) {
				d.PodmanRootless = func(context.Context) error { return podmanErr }
			},
			want: podmanErr.Error(),
		},
		"podman-version": {
			mutate: func(d *preflight.Deps) {
				d.PodmanVersion = func(context.Context) error { return versionErr }
			},
			want: versionErr.Error(),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			deps := classifyDeps()
			tc.mutate(&deps)
			err := classify(t, deps)
			if err == nil {
				t.Fatalf("%s unmet: classify err = nil, want a fatal error", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s fatal error %q does not carry %q", name, err.Error(), tc.want)
			}
		})
	}
}

// TestClassifyPreflightImageUnmetIsAdvisory: the load-bearing cold-start case.
// When only the image is unmet (every host capability OK), classifyPreflight
// returns NIL — `up` will pull the image and `up`-Ready verifies it. Gating on it
// here would make the app unable to cold-start on a fresh state dir.
func TestClassifyPreflightImageUnmetIsAdvisory(t *testing.T) {
	deps := classifyDeps()
	deps.ImagePresent = func(context.Context, string) error {
		return errors.New("agent image absent locally")
	}
	if err := classify(t, deps); err != nil {
		t.Fatalf("image-only unmet: classify err = %v, want nil (advisory, not fatal)", err)
	}
}

// TestClassifyPreflightHostCapFatalEvenWithAdvisoryUnmet: a fatal host-cap
// failure is still fatal when the image is ALSO unmet — the advisory check never
// masks a genuine host-capability gap, and no advisory copy leaks into the fatal
// fold.
func TestClassifyPreflightHostCapFatalEvenWithAdvisoryUnmet(t *testing.T) {
	deps := classifyDeps()
	deps.PodmanRootless = func(context.Context) error { return errors.New("no podman") }
	deps.ImagePresent = func(context.Context, string) error { return errors.New("no image") }
	err := classify(t, deps)
	if err == nil {
		t.Fatal("classify err = nil, want fatal (podman unmet) despite an advisory image failure")
	}
	if !strings.Contains(err.Error(), "no podman") {
		t.Errorf("fatal error %q does not carry the podman failure", err.Error())
	}
	if strings.Contains(err.Error(), "no image") {
		t.Errorf("fatal error %q leaked an advisory failure into the fatal fold", err.Error())
	}
}

// TestRunStackUpDeadlineExceededNamesBringUpWindow: when the child fails because
// the context deadline was exceeded, the error names the bring-up window (the
// likely cause) rather than surfacing a bare deadline error. Driven with an
// already-past deadline against a real binary so the classification is
// deterministic (no wall-clock wait).
func TestRunStackUpDeadlineExceededNamesBringUpWindow(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	stackUp := runStackUp("/bin/sh")
	err := stackUp(ctx, []string{"-c", "exit 0"})
	if err == nil {
		t.Fatal("stackUp err = nil, want a deadline-exceeded error")
	}
	if !strings.Contains(err.Error(), "bring-up window") {
		t.Errorf("stackUp error %q does not name the bring-up window", err.Error())
	}
}

// TestWhoAmIOverUDSRejectsEmptyAccountID: a SUCCESSFUL WhoAmI that reports an
// empty account id is rejected (non-nil error, empty id) rather than resolving
// an empty identity downstream.
func TestWhoAmIOverUDSRejectsEmptyAccountID(t *testing.T) {
	socket := serveWhoAmI(t, &stubWhoAmIServer{accountID: ""})
	ctx, cancel := context.WithTimeout(context.Background(), embeddedTestTimeout)
	defer cancel()

	id, err := whoAmIOverUDS(ctx, socket)
	if err == nil {
		t.Fatal("whoAmIOverUDS err = nil, want an error on an empty account id")
	}
	if id != "" {
		t.Errorf("account id = %q, want empty when WhoAmI returns an empty id", id)
	}
}

// TestResolveStackBin: flag wins, then $COMPASS_STACK_BIN, then a not-found error
// that names every place it looked. The executable-sibling branch is not covered
// because os.Executable can't be overridden without mocking; the other three
// legs are deterministic.
func TestResolveStackBin(t *testing.T) {
	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("COMPASS_STACK_BIN", "/env/compass-stack")
		got, err := resolveStackBin("/flag/compass-stack")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got != "/flag/compass-stack" {
			t.Errorf("got %q, want the flag value", got)
		}
	})
	t.Run("env wins over PATH", func(t *testing.T) {
		t.Setenv("COMPASS_STACK_BIN", "/env/compass-stack")
		got, err := resolveStackBin("")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got != "/env/compass-stack" {
			t.Errorf("got %q, want the env value", got)
		}
	})
	t.Run("not found names all four locations", func(t *testing.T) {
		t.Setenv("COMPASS_STACK_BIN", "")
		t.Setenv("PATH", "")
		_, err := resolveStackBin("")
		if err == nil {
			t.Fatal("err = nil, want a not-found error")
		}
		for _, want := range []string{"--compass-stack", "$COMPASS_STACK_BIN", "$PATH", "beside"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("not-found error %q does not name %q", err.Error(), want)
			}
		}
	})
}

// TestResolveSocket: flag wins, then $COMPASS_SOCKET, then an ABSOLUTE
// $XDG_RUNTIME_DIR/compass/server.sock. A RELATIVE $XDG_RUNTIME_DIR is treated as
// unset and falls through to $HOME/.compass/server.sock — the determinism guard.
func TestResolveSocket(t *testing.T) {
	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("COMPASS_SOCKET", "/env/server.sock")
		if got := resolveSocket("/flag/server.sock"); got != "/flag/server.sock" {
			t.Errorf("got %q, want the flag value", got)
		}
	})
	t.Run("env wins", func(t *testing.T) {
		t.Setenv("COMPASS_SOCKET", "/env/server.sock")
		t.Setenv("XDG_RUNTIME_DIR", "/xdg/run")
		if got := resolveSocket(""); got != "/env/server.sock" {
			t.Errorf("got %q, want the env value", got)
		}
	})
	t.Run("absolute XDG_RUNTIME_DIR", func(t *testing.T) {
		t.Setenv("COMPASS_SOCKET", "")
		xdg := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", xdg)
		if got := resolveSocket(""); got != filepath.Join(xdg, "compass", "server.sock") {
			t.Errorf("got %q, want %q", got, filepath.Join(xdg, "compass", "server.sock"))
		}
	})
	t.Run("relative XDG_RUNTIME_DIR falls through to HOME/.compass", func(t *testing.T) {
		t.Setenv("COMPASS_SOCKET", "")
		t.Setenv("XDG_RUNTIME_DIR", "rel/run")
		home := t.TempDir()
		t.Setenv("HOME", home)
		if got := resolveSocket(""); got != filepath.Join(home, ".compass", "server.sock") {
			t.Errorf("got %q, want %q (relative XDG_RUNTIME_DIR must fall through)", got, filepath.Join(home, ".compass", "server.sock"))
		}
	})
}

// TestResolveImage: flag wins, then $COMPASS_AGENT_IMAGE, then the locked GHCR
// default.
func TestResolveImage(t *testing.T) {
	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("COMPASS_AGENT_IMAGE", "env/image:tag")
		if got := resolveImage("flag/image:tag"); got != "flag/image:tag" {
			t.Errorf("got %q, want the flag value", got)
		}
	})
	t.Run("env wins", func(t *testing.T) {
		t.Setenv("COMPASS_AGENT_IMAGE", "env/image:tag")
		if got := resolveImage(""); got != "env/image:tag" {
			t.Errorf("got %q, want the env value", got)
		}
	})
	t.Run("default", func(t *testing.T) {
		t.Setenv("COMPASS_AGENT_IMAGE", "")
		if got := resolveImage(""); got != defaultAgentImage {
			t.Errorf("got %q, want defaultAgentImage %q", got, defaultAgentImage)
		}
	})
	// Pin the default to the canonical live GHCR owner. This guards the value
	// itself (not just that resolveImage returns it): the old
	// ghcr.io/rigelbuild/compass-agent owner 403s post org-rename, so a
	// shipped app that fell back to it could not pull its agent image (RIG-1967).
	t.Run("default is the canonical rigelbuild ref", func(t *testing.T) {
		if defaultAgentImage != "ghcr.io/rigelbuild/compass-agent:latest" {
			t.Errorf("defaultAgentImage = %q, want the canonical rigelbuild ref", defaultAgentImage)
		}
	})
}

// TestResolveMode: flag wins, then $COMPASS_APP_MODE, then "" (no override).
func TestResolveMode(t *testing.T) {
	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("COMPASS_APP_MODE", "client")
		if got := resolveMode("embedded"); got != "embedded" {
			t.Errorf("got %q, want the flag value", got)
		}
	})
	t.Run("env wins", func(t *testing.T) {
		t.Setenv("COMPASS_APP_MODE", "client")
		if got := resolveMode(""); got != "client" {
			t.Errorf("got %q, want the env value", got)
		}
	})
	t.Run("both empty", func(t *testing.T) {
		t.Setenv("COMPASS_APP_MODE", "")
		if got := resolveMode(""); got != "" {
			t.Errorf("got %q, want empty (no override)", got)
		}
	})
}

// assertArg fails unless want appears as a token in args.
func assertArg(t *testing.T, args []string, want string) {
	t.Helper()
	if !slices.Contains(args, want) {
		t.Errorf("argv %v missing token %q", args, want)
	}
}

// assertArgPair fails unless flag is immediately followed by value in args.
func assertArgPair(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i, a := range args {
		if a == flag {
			if i+1 < len(args) && args[i+1] == value {
				return
			}
			t.Errorf("argv %v: flag %q not followed by %q", args, flag, value)
			return
		}
	}
	t.Errorf("argv %v missing flag %q", args, flag)
}
