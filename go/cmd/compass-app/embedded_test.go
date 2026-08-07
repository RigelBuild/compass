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
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
	"github.com/sealedsecurity/compass/go/internal/appconfig"
	"github.com/sealedsecurity/compass/go/internal/preflight"
)

const embeddedTestTimeout = 5 * time.Second

// baseParams is a representative resolved launch input the argv/dial assertions
// key off. The socket is a fixed path (the tests never dial it except in the
// WhoAmI-server case, which overrides it).
var baseParams = embeddedParams{
	socket:   "/run/compass/server.sock",
	stateDir: "/state/compass",
	image:    "ghcr.io/sealedsecurity/compass-agent:latest",
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

// TestLaunchByModeClientNotImplemented: client mode returns the T5 sentinel and
// touches NO pipeline effect (no preflight, no stack-up, no WhoAmI) — the client
// path is not stubbed-in here, it is explicitly out of scope. Mutation that
// reddens it: routing client mode into run() would flip a recorder flag.
func TestLaunchByModeClientNotImplemented(t *testing.T) {
	rec := &recorder{}
	pipeline := stubPipeline(rec, nil, nil, nil, "acc-x")

	id, err := launchByMode(context.Background(), appconfig.ModeClient, pipeline, baseParams)
	if !errors.Is(err, errClientNotImplemented) {
		t.Fatalf("client mode err = %v, want errClientNotImplemented", err)
	}
	if id != "" {
		t.Errorf("client mode account id = %q, want empty", id)
	}
	if rec.preflightCalled || rec.stackUpCalled || rec.whoAmICalled {
		t.Errorf("client mode ran a pipeline effect: %+v", rec)
	}
}

// TestLaunchByModeEmbeddedHappyPath: embedded mode runs preflight → stack up →
// WhoAmI in order, passes the SAME socket to the dial that the argv carries, and
// returns the resolved account id. Asserting the argv (up, --socket, --state-dir,
// --image) is the stack-invocation contract; asserting whoAmISocket == socket is
// the single-socket invariant (the value passed to --socket IS the value dialed).
func TestLaunchByModeEmbeddedHappyPath(t *testing.T) {
	rec := &recorder{}
	pipeline := stubPipeline(rec, nil, nil, nil, "acc-42")

	id, err := launchByMode(context.Background(), appconfig.ModeEmbedded, pipeline, baseParams)
	if err != nil {
		t.Fatalf("embedded happy path err = %v, want nil", err)
	}
	if id != "acc-42" {
		t.Errorf("account id = %q, want acc-42", id)
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

// TestLaunchByModePreflightShortCircuits: a preflight failure returns the
// aggregated legible error VERBATIM and never proceeds to stack-up or WhoAmI.
// Mutation that reddens it: running the checks after a failure, or reformatting
// Results.Err's copy.
func TestLaunchByModePreflightShortCircuits(t *testing.T) {
	rec := &recorder{}
	preflightErr := errors.New("embedded-mode preflight failed:\n  - windows is not linux")
	pipeline := stubPipeline(rec, preflightErr, nil, nil, "acc-x")

	id, err := launchByMode(context.Background(), appconfig.ModeEmbedded, pipeline, baseParams)
	if !errors.Is(err, preflightErr) {
		t.Fatalf("preflight-fail err = %v, want the preflight error verbatim", err)
	}
	if id != "" {
		t.Errorf("account id = %q, want empty on preflight failure", id)
	}
	if !rec.preflightCalled {
		t.Error("preflight did not run")
	}
	if rec.stackUpCalled || rec.whoAmICalled {
		t.Errorf("pipeline proceeded past a failed preflight: %+v", rec)
	}
}

// TestLaunchByModeStackUpFails: a non-zero compass-stack up exit is surfaced and
// the pipeline stops before WhoAmI. The stackUp seam already folds stderr into
// its error (see TestRunStackUpNonZeroExitSurfacesStderr); here the contract is
// that run() propagates it and does not dial.
func TestLaunchByModeStackUpFails(t *testing.T) {
	rec := &recorder{}
	stackErr := errors.New("compass-stack up failed: exit status 1: postgres refused")
	pipeline := stubPipeline(rec, nil, stackErr, nil, "acc-x")

	id, err := launchByMode(context.Background(), appconfig.ModeEmbedded, pipeline, baseParams)
	if !errors.Is(err, stackErr) {
		t.Fatalf("stack-up-fail err = %v, want the stack-up error", err)
	}
	if id != "" {
		t.Errorf("account id = %q, want empty on stack-up failure", id)
	}
	if rec.whoAmICalled {
		t.Error("pipeline dialed WhoAmI after a failed stack-up")
	}
}

// TestLaunchByModeWhoAmIFails: a WhoAmI error is surfaced (wrapped with the
// socket for context) and no account id is returned. Mutation that reddens it:
// swallowing the WhoAmI error and returning an empty id as success.
func TestLaunchByModeWhoAmIFails(t *testing.T) {
	rec := &recorder{}
	whoErr := errors.New("connect: connection refused")
	pipeline := stubPipeline(rec, nil, nil, whoErr, "")

	id, err := launchByMode(context.Background(), appconfig.ModeEmbedded, pipeline, baseParams)
	if !errors.Is(err, whoErr) {
		t.Fatalf("whoami-fail err = %v, want the WhoAmI error wrapped", err)
	}
	if id != "" {
		t.Errorf("account id = %q, want empty on WhoAmI failure", id)
	}
	if !strings.Contains(err.Error(), baseParams.socket) {
		t.Errorf("WhoAmI error %q does not name the socket for context", err.Error())
	}
}

// TestStackUpArgsOmitsDatabaseAndListen: the pure argv builder omits --database
// (compass-stack recomputes the identical default from --state-dir, so the app
// carries no second DSN) and omits --listen when unset (the CLI defaults it).
func TestStackUpArgsOmitsDatabaseAndListen(t *testing.T) {
	args := stackUpArgs(baseParams)
	if slices.Contains(args, "--database") {
		t.Errorf("argv carries --database, want it omitted so compass-stack defaults the DSN: %v", args)
	}
	if slices.Contains(args, "--listen") {
		t.Errorf("argv carries --listen when unset, want it omitted: %v", args)
	}
}

// TestStackUpArgsIncludesListenWhenSet: a set listen addr rides the argv.
func TestStackUpArgsIncludesListenWhenSet(t *testing.T) {
	p := baseParams
	p.listen = "127.0.0.1:50052"
	args := stackUpArgs(p)
	assertArgPair(t, args, "--listen", "127.0.0.1:50052")
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
	AgentImage:  "ghcr.io/sealedsecurity/compass-agent:latest",
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
