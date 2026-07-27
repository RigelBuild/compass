//go:build unix

package runner

// The Runner-side dispatcher: OQ6 request-id idempotency (a repeated id executes
// the SessionHost exactly once, count-verified), the command→result mapping for
// each variant, the host-sentinel→RunnerErrorCode mapping, and the
// unknown-variant→INTERNAL error. Every test names the contract a plausible bug
// would break: a dedup that re-executed would double-provision a container; a
// wrong sentinel mapping would hand the Server the wrong Connect code; an
// unhandled variant must surface as an error, never hang the call.

import (
	"context"
	"errors"
	"sync"
	"testing"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
)

// fakeSessionHost is a hand-written SessionHost that counts calls and returns
// scripted results, so the dispatcher's dedup and mapping are asserted without
// the production host or a container.
type fakeSessionHost struct {
	mu sync.Mutex

	startCalls     int
	provisionCalls int
	stopCalls      int
	reloadCalls    int
	statusCalls    int

	startErr     error
	provisionErr error
	stopErr      error
	reloadErr    error
	statusErr    error

	sessionID     string
	containerName string
	statuses      []*compassv1.AgentSessionStatus

	lastStopID   string
	lastReloadID string
	lastStatusID string
}

func (f *fakeSessionHost) Start(_ context.Context, _ *compassv1.StartAgentSessionRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	return f.sessionID, f.startErr
}

func (f *fakeSessionHost) Provision(_ context.Context, _ *compassv1.ProvisionAgentWorkspaceRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.provisionCalls++
	return f.containerName, f.provisionErr
}

func (f *fakeSessionHost) Stop(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls++
	f.lastStopID = sessionID
	return f.stopErr
}

func (f *fakeSessionHost) Reload(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloadCalls++
	f.lastReloadID = sessionID
	return f.reloadErr
}

func (f *fakeSessionHost) Status(_ context.Context, sessionID string) ([]*compassv1.AgentSessionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	f.lastStatusID = sessionID
	return f.statuses, f.statusErr
}

// startCommand builds a Start command carrying a request id.
func startCommand(id string) *compassv1internal.SessionsResponse {
	return &compassv1internal.SessionsResponse{
		RequestId: id,
		Command:   &compassv1internal.SessionsResponse_Start{Start: &compassv1.StartAgentSessionRequest{ContainerName: "c1"}},
	}
}

// OQ6 (Runner-side twin): the dispatcher's handle with a repeated request id
// executes the SessionHost ONCE and returns the recorded result both times. A
// bug that re-executed would double-invoke Start (a duplicate session / spurious
// ALREADY_RUNNING).
func TestHandleDedupExecutesHostOnce(t *testing.T) {
	host := &fakeSessionHost{sessionID: "sess-1"}
	d := newDispatcher(host)

	first := d.handle(context.Background(), startCommand("req-1"))
	second := d.handle(context.Background(), startCommand("req-1"))

	if host.startCalls != 1 {
		t.Fatalf("host.Start called %d times for a repeated id, want 1 (dedup)", host.startCalls)
	}
	if first.GetStart().GetSessionId() != "sess-1" || second.GetStart().GetSessionId() != "sess-1" {
		t.Fatalf("results = %q/%q, want both sess-1 (recorded result returned on retry)",
			first.GetStart().GetSessionId(), second.GetStart().GetSessionId())
	}
	// The recorded result is the very same message returned again.
	if first != second {
		t.Fatal("retry returned a different result object; the recorded result must be returned verbatim")
	}
}

// Distinct request ids each execute the host — dedup keys on the id, it does not
// collapse everything.
func TestHandleDistinctIdsExecuteEach(t *testing.T) {
	host := &fakeSessionHost{sessionID: "s"}
	d := newDispatcher(host)
	d.handle(context.Background(), startCommand("a"))
	d.handle(context.Background(), startCommand("b"))
	if host.startCalls != 2 {
		t.Fatalf("host.Start called %d times for two distinct ids, want 2", host.startCalls)
	}
}

// Each command variant maps to the right typed result, and carries the request
// id back. Table-driven over all five variants.
func TestExecuteMapsEachVariantToItsResult(t *testing.T) {
	statuses := []*compassv1.AgentSessionStatus{{SessionId: "s1", State: compassv1.AgentSessionState_AGENT_SESSION_STATE_READY}}
	host := &fakeSessionHost{sessionID: "sess-x", containerName: "cont-x", statuses: statuses}

	cases := []struct {
		name  string
		cmd   *compassv1internal.SessionsResponse
		check func(t *testing.T, res *compassv1internal.SessionsRequest)
	}{
		{
			name: "start",
			cmd:  startCommand("r1"),
			check: func(t *testing.T, res *compassv1internal.SessionsRequest) {
				t.Helper()
				if res.GetStart().GetSessionId() != "sess-x" {
					t.Fatalf("start result session id = %q, want sess-x", res.GetStart().GetSessionId())
				}
			},
		},
		{
			name: "provision",
			cmd: &compassv1internal.SessionsResponse{
				RequestId: "r2",
				Command:   &compassv1internal.SessionsResponse_Provision{Provision: &compassv1.ProvisionAgentWorkspaceRequest{}},
			},
			check: func(t *testing.T, res *compassv1internal.SessionsRequest) {
				t.Helper()
				if res.GetProvision().GetContainerName() != "cont-x" {
					t.Fatalf("provision result container = %q, want cont-x", res.GetProvision().GetContainerName())
				}
			},
		},
		{
			name: "stop",
			cmd: &compassv1internal.SessionsResponse{
				RequestId: "r3",
				Command:   &compassv1internal.SessionsResponse_Stop{Stop: &compassv1.StopAgentSessionRequest{SessionId: "s-stop"}},
			},
			check: func(t *testing.T, res *compassv1internal.SessionsRequest) {
				t.Helper()
				if res.GetStop() == nil {
					t.Fatalf("stop result = nil, want a StopAgentSessionResponse")
				}
				if host.lastStopID != "s-stop" {
					t.Fatalf("host.Stop got id %q, want s-stop (the command's session id)", host.lastStopID)
				}
			},
		},
		{
			name: "reload",
			cmd: &compassv1internal.SessionsResponse{
				RequestId: "r4",
				Command:   &compassv1internal.SessionsResponse_Reload{Reload: &compassv1.ReloadAgentSessionRequest{SessionId: "s-reload"}},
			},
			check: func(t *testing.T, res *compassv1internal.SessionsRequest) {
				t.Helper()
				if res.GetReload().GetSessionId() != "s-reload" {
					t.Fatalf("reload result session id = %q, want s-reload (reused id)", res.GetReload().GetSessionId())
				}
			},
		},
		{
			name: "status",
			cmd: &compassv1internal.SessionsResponse{
				RequestId: "r5",
				Command:   &compassv1internal.SessionsResponse_Status{Status: &compassv1.GetAgentStatusRequest{}},
			},
			check: func(t *testing.T, res *compassv1internal.SessionsRequest) {
				t.Helper()
				got := res.GetStatus().GetStatuses()
				if len(got) != 1 || got[0].GetSessionId() != "s1" {
					t.Fatalf("status result = %+v, want the host's live set", got)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDispatcher(host)
			res := d.execute(context.Background(), tc.cmd.GetRequestId(), tc.cmd)
			if res.GetRequestId() != tc.cmd.GetRequestId() {
				t.Fatalf("result request id = %q, want %q (correlation)", res.GetRequestId(), tc.cmd.GetRequestId())
			}
			if res.GetError() != nil {
				t.Fatalf("%s produced an error result: %v", tc.name, res.GetError())
			}
			tc.check(t, res)
		})
	}
}

// A host sentinel error maps to its wire RunnerErrorCode: errAlreadyRunning →
// ALREADY_RUNNING, errSessionUnknown → NOT_FOUND, any other error → INTERNAL. A
// bug in the mapping hands the Server the wrong Connect code.
func TestExecuteMapsHostSentinelsToCodes(t *testing.T) {
	cases := []struct {
		name     string
		startErr error
		want     compassv1internal.RunnerErrorCode
	}{
		{"already running", errAlreadyRunning, compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_ALREADY_RUNNING},
		{"session unknown", errSessionUnknown, compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_NOT_FOUND},
		{"other error", errors.New("engine exploded"), compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_INTERNAL},
		{"wrapped already running", errWrap(errAlreadyRunning), compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_ALREADY_RUNNING},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := &fakeSessionHost{startErr: tc.startErr}
			d := newDispatcher(host)
			res := d.execute(context.Background(), "r", startCommand("r"))
			re := res.GetError()
			if re == nil {
				t.Fatalf("%s did not produce an error result", tc.name)
			}
			if re.GetCode() != tc.want {
				t.Fatalf("%s error code = %v, want %v", tc.name, re.GetCode(), tc.want)
			}
			// The host's message rides along for diagnosis.
			if re.GetMessage() == "" {
				t.Fatalf("%s error carried an empty message", tc.name)
			}
		})
	}
}

// An unset/unrecognized command variant surfaces an INTERNAL error result (a
// contract skew), never hangs the call or panics.
func TestExecuteUnknownCommandVariantIsInternalError(t *testing.T) {
	host := &fakeSessionHost{}
	d := newDispatcher(host)
	// A SessionsResponse with no command oneof set.
	res := d.execute(context.Background(), "r", &compassv1internal.SessionsResponse{RequestId: "r"})
	re := res.GetError()
	if re == nil {
		t.Fatal("unknown command variant did not produce an error result")
	}
	if re.GetCode() != compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_INTERNAL {
		t.Fatalf("unknown variant code = %v, want INTERNAL", re.GetCode())
	}
	// No host method ran — there was nothing to execute.
	if host.startCalls+host.provisionCalls+host.stopCalls+host.reloadCalls+host.statusCalls != 0 {
		t.Fatal("an unknown command variant invoked a host method; it must not")
	}
}

// errWrap wraps an error so the sentinel mapping is proven to use errors.Is
// (unwrap), not identity — a wrapped errAlreadyRunning must still map to
// ALREADY_RUNNING.
func errWrap(err error) error { return &wrapError{err} }

type wrapError struct{ inner error }

func (w *wrapError) Error() string { return "wrapped: " + w.inner.Error() }
func (w *wrapError) Unwrap() error { return w.inner }
