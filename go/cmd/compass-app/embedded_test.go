//go:build unix && gtk3

package main

// T4.1 embedded launch-pipeline gate. The pipeline is exercised through its Go
// entrypoints with INJECTED effects — no real podman/postgres/compass-stack/exec
// — so mode-select, preflight short-circuit, the exact compass-stack argv, the
// WhoAmI hop, and the two error paths are all verified deterministically. The
// one seam wired to a real transport is whoAmIOverUDS, driven against a REAL
// in-process compass.v1 WhoAmI server over h2c on a Unix socket (mirroring the
// bridge-service gate's stubServer and internal/runner/e2e_transport_test.go's
// UDS/connect pattern), so the h2c-UDS dial is proven on the wire it ships on.

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/appconfig"
	"github.com/RigelBuild/compass/go/internal/bridge"
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
	preflightErr := errors.New("embedded-mode preflight failed:\n  - windows is not linux")
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

// clientConfig is a representative valid native-client config. ServerURL is an
// absolute https URL (validated upstream by appconfig); runClient never dials it.
var clientConfig = appconfig.Config{
	Mode:      appconfig.ModeClient,
	ServerURL: "https://remote.example:8443",
}

// TestRunClientBuildsServiceWithoutPipelineEffect: a valid client config yields a
// bridge service with a non-nil TLS target + tokenstore and an empty accountID,
// and touches NO embedded pipeline/stack-bin effect. runClient takes no pipeline,
// so the assertion is structural: the service it returns is wired for Connect
// (target + tokens non-nil), which the embedded arm never sets. No network dial.
func TestRunClientBuildsServiceWithoutPipelineEffect(t *testing.T) {
	svc, err := runClient(clientConfig, t.TempDir())
	if err != nil {
		t.Fatalf("runClient err = %v, want nil for a valid client config", err)
	}
	if svc.target == nil {
		t.Error("client service target is nil, want a TLS target wired for Connect")
	}
	if svc.tokens == nil {
		t.Error("client service tokenstore is nil, want one wired for Connect")
	}
	if svc.accountID != "" {
		t.Errorf("client service accountID = %q, want empty (identity is UI-resolved, OQ-7)", svc.accountID)
	}
}

// TestRunClientBadCACertNamesThePath: an unreadable ca_cert path aborts launch
// with a legible error that NAMES the path (not a panic, not a bare os error).
func TestRunClientBadCACertNamesThePath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.pem")
	cfg := clientConfig
	cfg.CACert = missing

	svc, err := runClient(cfg, t.TempDir())
	if err == nil {
		t.Fatal("runClient with a missing ca_cert returned nil error, want a legible read error")
	}
	if svc != nil {
		t.Error("runClient returned a non-nil service alongside an error")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("CA read error %q does not name the ca_cert path %q", err.Error(), missing)
	}
}

// TestRunClientBadCAPEMNamesTheCause: a ca_cert file that is not a usable PEM
// aborts launch with a legible error surfacing the TLS-target cause (NewTLSTarget
// rejects a PEM yielding no certificate).
func TestRunClientBadCAPEMNamesTheCause(t *testing.T) {
	badPEM := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(badPEM, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("writing bad PEM fixture: %v", err)
	}
	cfg := clientConfig
	cfg.CACert = badPEM

	svc, err := runClient(cfg, t.TempDir())
	if err == nil {
		t.Fatal("runClient with an unusable CA PEM returned nil error, want a legible target error")
	}
	if svc != nil {
		t.Error("runClient returned a non-nil service alongside an error")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Errorf("CA PEM error %q does not surface the no-usable-certificate cause", err.Error())
	}
}

// TestRunClientSharesOneTargetAcrossPumpAndService is the load-bearing wiring
// invariant (design §T5.6): the client pump and the bridge service MUST share ONE
// *bridge.Target, or a bearer armed on the service's target (Connect →
// SetBearer) never reaches the pump's forwarded requests (CompassRPC → pump →
// target.client.Do).
//
// The assertion is behavioral, not a private-field pointer peek: arm a bearer on
// the service's target, then run a real gRPC-Web forward through the service's
// pump against an in-process TLS server that records the Authorization header it
// receives. If the pump forwarded through the SAME target, the armed bearer
// arrives; if runClient had built two targets, the pump's target would be unarmed
// and no bearer would arrive. Non-vacuous: mutating runClient to pass a second
// bridge.NewTLSTarget(...) to NewPump reddens this (the recorded header is empty).
func TestRunClientSharesOneTargetAcrossPumpAndService(t *testing.T) {
	const bearer = "arm-me-9f3c"

	gotAuth := make(chan string, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotAuth <- r.Header.Get("Authorization"):
		default:
		}
		// Minimal well-formed response so the pump emits head→end, not an error
		// frame; the body is irrelevant to the header assertion.
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	caPEM := pemEncodeCert(t, srv.Certificate())
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatalf("writing CA fixture: %v", err)
	}
	cfg := appconfig.Config{Mode: appconfig.ModeClient, ServerURL: srv.URL, CACert: caFile}

	svc, err := runClient(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("runClient err = %v, want nil", err)
	}

	// Arm the bearer on the SERVICE's target (the point Connect arms it).
	svc.target.SetBearer(bearer)

	// Forward one call through the SERVICE's pump; block until the terminal
	// frame so the request has completed (no time.Sleep — the pump's Do is
	// synchronous and signals completion by emitting a terminal frame).
	done := make(chan struct{})
	svc.pump.Do(context.Background(), bridge.Call{Path: "/probe"}, func(f bridge.Frame) {
		switch f.(type) {
		case bridge.EndFrame, bridge.ErrorFrame:
			close(done)
		default:
		}
	})
	<-done

	select {
	case auth := <-gotAuth:
		if auth != "Bearer "+bearer {
			t.Errorf("pump forwarded Authorization = %q, want %q — the pump's target is NOT the service's armed target",
				auth, "Bearer "+bearer)
		}
	default:
		t.Fatal("server received no request through the pump")
	}
}

// pemEncodeCert PEM-encodes a certificate for use as a CA anchor fixture.
func pemEncodeCert(t *testing.T, cert *x509.Certificate) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

// TestShellStartupJS covers the OQ-8 startup-global injection: the mode token per
// mode, __COMPASS_SERVER_URL__ present ONLY in client mode, and JSON-escaping of
// a hostile server URL so it cannot break out of the script.
func TestShellStartupJS(t *testing.T) {
	t.Run("embedded injects mode only", func(t *testing.T) {
		js, err := shellStartupJS(appconfig.ModeEmbedded.String(), "")
		if err != nil {
			t.Fatalf("shellStartupJS err = %v, want nil", err)
		}
		if !strings.Contains(js, `window.__COMPASS_MODE__="embedded";`) {
			t.Errorf("embedded JS = %q, want the embedded mode global", js)
		}
		if strings.Contains(js, "__COMPASS_SERVER_URL__") {
			t.Errorf("embedded JS = %q, want NO server-url global", js)
		}
	})

	t.Run("client injects mode and server url", func(t *testing.T) {
		js, err := shellStartupJS(appconfig.ModeClient.String(), "https://remote.example:8443")
		if err != nil {
			t.Fatalf("shellStartupJS err = %v, want nil", err)
		}
		if !strings.Contains(js, `window.__COMPASS_MODE__="client";`) {
			t.Errorf("client JS = %q, want the client mode global", js)
		}
		if !strings.Contains(js, `window.__COMPASS_SERVER_URL__="https://remote.example:8443";`) {
			t.Errorf("client JS = %q, want the server-url global", js)
		}
	})

	t.Run("hostile server url is JSON-escaped, not a breakout", func(t *testing.T) {
		hostile := `https://x/"+alert(1)+"</script><script>`
		js, err := shellStartupJS(appconfig.ModeClient.String(), hostile)
		if err != nil {
			t.Fatalf("shellStartupJS err = %v, want nil", err)
		}

		// The raw hostile string must NOT appear verbatim — the double-quote and
		// the </script> would break out of the script if concatenated unescaped.
		if strings.Contains(js, hostile) {
			t.Fatalf("JS contains the raw hostile URL verbatim (breakout): %q", js)
		}
		// The value must be a valid JS/JSON string literal: parse back the
		// assigned literal and confirm it round-trips to the original URL.
		assign := "window.__COMPASS_SERVER_URL__="
		i := strings.Index(js, assign)
		if i < 0 {
			t.Fatalf("JS missing the server-url assignment: %q", js)
		}
		literal := strings.TrimSuffix(js[i+len(assign):], ";")
		var decoded string
		if err := json.Unmarshal([]byte(literal), &decoded); err != nil {
			t.Fatalf("server-url literal %q is not a valid JSON string: %v", literal, err)
		}
		if decoded != hostile {
			t.Errorf("decoded server url = %q, want the original %q", decoded, hostile)
		}
	})
}

// TestStackUpArgsOmitsDatabase: the pure argv builder omits --database
// (compass-stack recomputes the identical default from --state-dir, so the app
// carries no second DSN).
func TestStackUpArgsOmitsDatabase(t *testing.T) {
	args := stackUpArgs(baseParams)
	if slices.Contains(args, "--database") {
		t.Errorf("argv carries --database, want it omitted so compass-stack defaults the DSN: %v", args)
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
// for the sleep's 60s; the fix returns immediately. The assertion is that Run
// completes well under the sleep — a plain wall-clock bound, but the pre-fix gap
// (60s vs milliseconds) is enormous, so it is not flaky. The backgrounded sleep
// is cleaned up via its own short lifetime; the test spawns nothing it must kill
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
// GOOS linux, uid the runner's expected uid) — mirroring preflight_test.go's
// okDeps. Tests override individual fields to drive one failing check at a time
// through classifyPreflight.
func classifyDeps() preflight.Deps {
	return preflight.Deps{
		GOOS:             "linux",
		CurrentUID:       preflight.DefaultAgentUID,
		ExpectedAgentUID: preflight.DefaultAgentUID,
		PodmanRootless:   func(context.Context) error { return nil },
		ImagePresent:     func(context.Context, string) error { return nil },
		DBReachable:      func(context.Context, string) error { return nil },
	}
}

var classifyParams = preflight.Params{
	AgentImage:  "ghcr.io/rigelbuild/compass-agent:latest",
	DatabaseDSN: "host=/state/compass/postgres/sock port=5432 dbname=compass sslmode=disable",
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

// TestClassifyPreflightHostCapUnmetIsFatal: each host-capability check (OS, UID,
// podman) that `up` CANNOT create is fatal — classifyPreflight returns non-nil,
// naming the failed check's copy.
func TestClassifyPreflightHostCapUnmetIsFatal(t *testing.T) {
	sentinel := errors.New("no rootless podman here")
	cases := map[string]struct {
		mutate func(*preflight.Deps)
		want   string
	}{
		"os": {
			mutate: func(d *preflight.Deps) { d.GOOS = "windows" },
			want:   "windows",
		},
		"uid": {
			mutate: func(d *preflight.Deps) { d.CurrentUID = 501 },
			want:   "501",
		},
		"podman": {
			mutate: func(d *preflight.Deps) {
				d.PodmanRootless = func(context.Context) error { return sentinel }
			},
			want: sentinel.Error(),
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

// TestClassifyPreflightImageOrDBUnmetIsAdvisory: the load-bearing cold-start
// case. When only image and/or database are unmet (every host capability OK),
// classifyPreflight returns NIL — `up` will start postgres and pull the image,
// and `up`-Ready verifies both. Gating on them here would make the app unable to
// cold-start on a fresh state dir.
func TestClassifyPreflightImageOrDBUnmetIsAdvisory(t *testing.T) {
	imgErr := errors.New("agent image absent locally")
	dbErr := errors.New("postgres not accepting yet")
	cases := map[string]func(*preflight.Deps){
		"image only": func(d *preflight.Deps) {
			d.ImagePresent = func(context.Context, string) error { return imgErr }
		},
		"db only": func(d *preflight.Deps) {
			d.DBReachable = func(context.Context, string) error { return dbErr }
		},
		"image and db (cold fresh-state-dir launch)": func(d *preflight.Deps) {
			d.ImagePresent = func(context.Context, string) error { return imgErr }
			d.DBReachable = func(context.Context, string) error { return dbErr }
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			deps := classifyDeps()
			mutate(&deps)
			if err := classify(t, deps); err != nil {
				t.Fatalf("%s: classify err = %v, want nil (advisory, not fatal)", name, err)
			}
		})
	}
}

// TestClassifyPreflightHostCapFatalEvenWithAdvisoryUnmet: a fatal host-cap
// failure is still fatal when image/DB are ALSO unmet — the advisory checks
// never mask a genuine host-capability gap.
func TestClassifyPreflightHostCapFatalEvenWithAdvisoryUnmet(t *testing.T) {
	deps := classifyDeps()
	deps.PodmanRootless = func(context.Context) error { return errors.New("no podman") }
	deps.ImagePresent = func(context.Context, string) error { return errors.New("no image") }
	deps.DBReachable = func(context.Context, string) error { return errors.New("no db") }
	err := classify(t, deps)
	if err == nil {
		t.Fatal("classify err = nil, want fatal (podman unmet) despite advisory image/db failures")
	}
	if !strings.Contains(err.Error(), "no podman") {
		t.Errorf("fatal error %q does not carry the podman failure", err.Error())
	}
	if strings.Contains(err.Error(), "no image") || strings.Contains(err.Error(), "no db") {
		t.Errorf("fatal error %q leaked an advisory failure into the fatal fold", err.Error())
	}
}

// TestEmbeddedDatabaseDSNMatchesCompassStackDefault: the app-side DSN formula
// must stay byte-identical to cmd/compass-stack's defaultDSN
// (go/cmd/compass-stack/main.go defaultDSN — the source of truth). compass-stack
// is package main and unimportable, so this asserts against the literal expected
// value; a human changing one formula must update both, and this reddens if the
// app-side formula drifts.
func TestEmbeddedDatabaseDSNMatchesCompassStackDefault(t *testing.T) {
	const want = "host=/tmp/st/postgres/sock port=5432 dbname=compass sslmode=disable"
	if got := embeddedDatabaseDSN("/tmp/st"); got != want {
		t.Errorf("embeddedDatabaseDSN = %q, want %q (must stay in lockstep with cmd/compass-stack defaultDSN)", got, want)
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

// TestResolveStateDir: flag wins, then $COMPASS_STATE_DIR, then an ABSOLUTE
// $XDG_STATE_HOME/compass. A RELATIVE $XDG_STATE_HOME is treated as unset and
// falls through to $HOME/.compass — the load-bearing determinism guard.
func TestResolveStateDir(t *testing.T) {
	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("COMPASS_STATE_DIR", "/env/state")
		if got := resolveStateDir("/flag/state"); got != "/flag/state" {
			t.Errorf("got %q, want the flag value", got)
		}
	})
	t.Run("env wins", func(t *testing.T) {
		t.Setenv("COMPASS_STATE_DIR", "/env/state")
		t.Setenv("XDG_STATE_HOME", "/xdg/state")
		if got := resolveStateDir(""); got != "/env/state" {
			t.Errorf("got %q, want the env value", got)
		}
	})
	t.Run("absolute XDG_STATE_HOME", func(t *testing.T) {
		t.Setenv("COMPASS_STATE_DIR", "")
		xdg := t.TempDir()
		t.Setenv("XDG_STATE_HOME", xdg)
		if got := resolveStateDir(""); got != filepath.Join(xdg, "compass") {
			t.Errorf("got %q, want %q", got, filepath.Join(xdg, "compass"))
		}
	})
	t.Run("relative XDG_STATE_HOME falls through to HOME/.compass", func(t *testing.T) {
		t.Setenv("COMPASS_STATE_DIR", "")
		t.Setenv("XDG_STATE_HOME", "rel/state")
		home := t.TempDir()
		t.Setenv("HOME", home)
		if got := resolveStateDir(""); got != filepath.Join(home, ".compass") {
			t.Errorf("got %q, want %q (relative XDG_STATE_HOME must fall through)", got, filepath.Join(home, ".compass"))
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
