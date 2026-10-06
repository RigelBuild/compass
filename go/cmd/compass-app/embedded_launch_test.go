//go:build (linux && gtk4) || darwin

package main

// App-side embedded launch gate: runEmbedded runs the pipeline and wires the
// quit controller, exercised with injected seams and no real exec.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/embedded"
)

// baseParams is a representative resolved launch input the argv/dial assertions
// key off. The socket is a fixed path (the tests never dial it except in the
// WhoAmI-server case, which overrides it).
var baseParams = embedded.Params{
	Socket:   "/run/compass/server.sock",
	StateDir: "/state/compass",
	Image:    "ghcr.io/rigelbuild/compass-agent:latest",
}

// stubPipeline builds an embedded.Pipeline whose three seams are deterministic
// stubs, recording what the orchestration invoked. Each seam defaults to a
// success no-op; a test overrides the ones it drives.
type recorder struct {
	preflightCalled bool
	stackUpCalled   bool
	stackUpArgs     []string
	whoAmICalled    bool
	whoAmISocket    string
}

func stubPipeline(rec *recorder, preflightErr, stackUpErr, whoAmIErr error, accountID string) embedded.Pipeline {
	return embedded.Pipeline{
		Preflight: func(_ context.Context) error {
			rec.preflightCalled = true
			return preflightErr
		},
		StackUp: func(_ context.Context, args []string) error {
			rec.stackUpCalled = true
			rec.stackUpArgs = args
			return stackUpErr
		},
		WhoAmI: func(_ context.Context, socket string) (string, error) {
			rec.whoAmICalled = true
			rec.whoAmISocket = socket
			return accountID, whoAmIErr
		},
	}
}

// runEmbeddedStub runs runEmbedded with a recording stackDown seam so the quit
// controller wiring is observable without a real exec.
func runEmbeddedStub(
	t *testing.T, pipeline embedded.Pipeline,
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
	assertArgPair(t, rec.stackUpArgs, "--socket", baseParams.Socket)
	assertArgPair(t, rec.stackUpArgs, "--state-dir", baseParams.StateDir)
	assertArgPair(t, rec.stackUpArgs, "--image", baseParams.Image)
	if rec.whoAmISocket != baseParams.Socket {
		t.Errorf("WhoAmI dialed %q, want the SAME socket passed to --socket %q",
			rec.whoAmISocket, baseParams.Socket)
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
	if !strings.Contains(err.Error(), baseParams.Socket) {
		t.Errorf("WhoAmI error %q does not name the socket for context", err.Error())
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
