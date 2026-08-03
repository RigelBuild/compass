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
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
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
	removeCalls    int
	reloadCalls    int
	statusCalls    int

	startErr     error
	provisionErr error
	stopErr      error
	removeErr    error
	reloadErr    error
	statusErr    error

	sessionID     string
	containerName string
	statuses      []*compassv1.AgentSessionStatus

	lastStopID     string
	lastRemoveName string
	lastReloadID   string
	lastStatusID   string

	refreshCalls  int
	lastRefreshID string
	refreshErr    error

	refreshConfigCalls int
	refreshConfigErr   error
	// refreshConfigEntered, when non-nil, receives one value at the START of
	// each RefreshConfig call; refreshConfigRelease, when non-nil, blocks the
	// call until a value is sent on it. Together they let a test hold a config
	// pass in flight to exercise coalescing, deterministically — no sleeps.
	refreshConfigEntered chan struct{}
	refreshConfigRelease chan struct{}
}

func (f *fakeSessionHost) Start(_ context.Context, _ *compassv1.StartAgentSessionRequest, _ string) (string, error) {
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

func (f *fakeSessionHost) Remove(_ context.Context, containerName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls++
	f.lastRemoveName = containerName
	return f.removeErr
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

func (f *fakeSessionHost) RefreshSecrets(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshCalls++
	f.lastRefreshID = sessionID
	return f.refreshErr
}

func (f *fakeSessionHost) RefreshConfig(ctx context.Context) error {
	f.mu.Lock()
	f.refreshConfigCalls++
	entered, release, err := f.refreshConfigEntered, f.refreshConfigRelease, f.refreshConfigErr
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

// configRefreshCount reads the RefreshConfig call count under the lock, so a test
// can gate on the worker's async pass without racing the counter.
func (f *fakeSessionHost) configRefreshCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshConfigCalls
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
	d := newDispatcher(host, discardLoggerRunner())

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
	d := newDispatcher(host, discardLoggerRunner())
	d.handle(context.Background(), startCommand("a"))
	d.handle(context.Background(), startCommand("b"))
	if host.startCalls != 2 {
		t.Fatalf("host.Start called %d times for two distinct ids, want 2", host.startCalls)
	}
}

// Each command variant maps to the right typed result, and carries the request
// id back. Table-driven over all six variants.
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
		{
			name: "remove",
			cmd: &compassv1internal.SessionsResponse{
				RequestId: "r6",
				Command:   &compassv1internal.SessionsResponse_Remove{Remove: &compassv1.RemoveAgentWorkspaceRequest{ContainerName: "cont-rm"}},
			},
			check: func(t *testing.T, res *compassv1internal.SessionsRequest) {
				t.Helper()
				if res.GetRemove() == nil {
					t.Fatalf("remove result = nil, want a RemoveAgentWorkspaceResponse")
				}
				if host.lastRemoveName != "cont-rm" {
					t.Fatalf("host.Remove got container %q, want cont-rm (the command's container name)", host.lastRemoveName)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDispatcher(host, discardLoggerRunner())
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
			d := newDispatcher(host, discardLoggerRunner())
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

// A Remove teardown failure returns a plain (non-sentinel) wrapped error, which
// errorResult maps to RUNNER_ERROR_CODE_INTERNAL — the sensible code for an
// engine/Runner teardown fault (nothing the Server can retry into a different
// outcome). A bug that swallowed the host error would answer a lying success.
func TestExecuteRemoveErrorMapsToInternal(t *testing.T) {
	host := &fakeSessionHost{removeErr: errors.New("engine remove failed")}
	d := newDispatcher(host, discardLoggerRunner())
	cmd := &compassv1internal.SessionsResponse{
		RequestId: "r",
		Command:   &compassv1internal.SessionsResponse_Remove{Remove: &compassv1.RemoveAgentWorkspaceRequest{ContainerName: "cont-rm"}},
	}
	res := d.execute(context.Background(), "r", cmd)
	re := res.GetError()
	if re == nil {
		t.Fatal("Remove teardown failure did not produce an error result")
	}
	if re.GetCode() != compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_INTERNAL {
		t.Fatalf("Remove error code = %v, want INTERNAL", re.GetCode())
	}
	if re.GetMessage() == "" {
		t.Fatal("Remove error carried an empty message")
	}
}

// An unset/unrecognized command variant surfaces an INTERNAL error result (a
// contract skew), never hangs the call or panics.
func TestExecuteUnknownCommandVariantIsInternalError(t *testing.T) {
	host := &fakeSessionHost{}
	d := newDispatcher(host, discardLoggerRunner())
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

// secretsVersionCommand builds a signal-only SecretsVersion command for the
// canonical test session. It carries no request id and expects no result on the
// request half.
func secretsVersionCommand() *compassv1internal.SessionsResponse {
	return &compassv1internal.SessionsResponse{
		Command: &compassv1internal.SessionsResponse_SecretsVersion{
			SecretsVersion: &compassv1internal.SecretsVersion{SessionId: "sess-1", Version: "1"},
		},
	}
}

// A SecretsVersion signal triggers a secret re-fetch+materialize for its session
// and produces NO result frame on the request half (signal-only, no result
// variant — secrets_signal.go). A bug that returned a result would desync the
// Server's request-id correlation; one that skipped the refresh would never
// materialize the initial or rotated secret set.
func TestExecuteSecretsVersionTriggersRefreshNoResult(t *testing.T) {
	host := &fakeSessionHost{}
	d := newDispatcher(host, discardLoggerRunner())
	res := d.execute(context.Background(), "", secretsVersionCommand())
	if res != nil {
		t.Fatalf("SecretsVersion produced a result frame %+v, want nil (signal-only)", res)
	}
	if host.refreshCalls != 1 {
		t.Fatalf("RefreshSecrets called %d times, want 1", host.refreshCalls)
	}
	if host.lastRefreshID != "sess-1" {
		t.Fatalf("RefreshSecrets got session %q, want sess-1", host.lastRefreshID)
	}
}

// A SecretsVersion signal is NOT deduped on request id: two signals (both with
// an empty request id) each re-fetch. A bug that ran the command through the
// request-id dedup would collapse every signal to one, so a rotation after the
// first would never re-materialize.
func TestHandleSecretsVersionNotDeduped(t *testing.T) {
	host := &fakeSessionHost{}
	d := newDispatcher(host, discardLoggerRunner())
	if r := d.handle(context.Background(), secretsVersionCommand()); r != nil {
		t.Fatalf("first signal returned a result %+v, want nil", r)
	}
	if r := d.handle(context.Background(), secretsVersionCommand()); r != nil {
		t.Fatalf("second signal returned a result %+v, want nil", r)
	}
	if host.refreshCalls != 2 {
		t.Fatalf("RefreshSecrets called %d times for two signals, want 2 (not deduped)", host.refreshCalls)
	}
}

// A refresh failure on the SecretsVersion hook is logged and the session
// survives — never a crash, never a silent success. The dispatcher recovers on
// the next signal/reconnect (best-effort, mirroring secrets_signal.go). A bug
// that returned an error result would put a spurious frame on the request half;
// one that panicked would take the session down.
func TestExecuteSecretsVersionRefreshErrorLoggedAndContinues(t *testing.T) {
	host := &fakeSessionHost{refreshErr: errors.New("fetch exploded")}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))
	d := newDispatcher(host, log)

	res := d.execute(context.Background(), "", secretsVersionCommand())
	if res != nil {
		t.Fatalf("refresh failure produced a result frame %+v, want nil (logged + continue)", res)
	}
	if host.refreshCalls != 1 {
		t.Fatalf("RefreshSecrets called %d times, want 1", host.refreshCalls)
	}
	logged := buf.String()
	if logged == "" {
		t.Fatal("refresh failure was not logged; it must be loud, not silent")
	}
	if !strings.Contains(logged, "sess-1") {
		t.Fatalf("refresh error log did not name the session; log:\n%s", logged)
	}
}

// configVersionCommand builds a signal-only ConfigVersion command. Fleet-wide (no
// session id), no request id, expects no result on the request half.
func configVersionCommand(version string) *compassv1internal.SessionsResponse {
	return &compassv1internal.SessionsResponse{
		Command: &compassv1internal.SessionsResponse_ConfigVersion{
			ConfigVersion: &compassv1internal.ConfigVersion{Version: version},
		},
	}
}

// A ConfigVersion signal is RECOGNIZED (not a contract-skew error) and produces
// NO result frame on the request half — T3 lands the wire surface; the
// re-materialize+Reload loop is T6. A bug that fell through to the default arm
// would return an "unrecognized variant" error result, desyncing the Server's
// request-id correlation with a frame for a signal that has none.
func TestExecuteConfigVersionRecognizedNoResult(t *testing.T) {
	host := &fakeSessionHost{}
	d := newDispatcher(host, discardLoggerRunner())
	res := d.execute(context.Background(), "", configVersionCommand("v-1"))
	if res != nil {
		t.Fatalf("ConfigVersion produced a result frame %+v, want nil (signal-only)", res)
	}
}

// A ConfigVersion signal drives exactly one RefreshConfig pass on the background
// worker (fleet-wide, so no session id is threaded) and produces no result
// frame. A bug that logged the signal without signalling the worker would never
// re-materialize; one that ran the fan-out inline would block the receive loop.
func TestConfigVersionSignalDrivesRefreshConfig(t *testing.T) {
	host := &fakeSessionHost{refreshConfigEntered: make(chan struct{}, 1)}
	d := newDispatcher(host, discardLoggerRunner())
	ctx, cancel := context.WithCancel(context.Background())
	go d.runConfigWorker(ctx)
	defer func() {
		cancel()
		<-d.configWorkerDone
	}()

	res := d.execute(ctx, "", configVersionCommand("v-1"))
	if res != nil {
		t.Fatalf("ConfigVersion produced a result frame %+v, want nil (signal-only)", res)
	}
	select {
	case <-host.refreshConfigEntered:
	case <-timeAfter():
		t.Fatal("ConfigVersion signal did not drive a RefreshConfig pass within the deadline")
	}
}

// A ConfigVersion signal is NOT deduped on request id: like SecretsVersion it
// bypasses the request-id map (both carry an empty id), so two signals each
// dispatch. A bug that ran it through the dedup would collapse every config
// update to one, and a later bundle change would never re-materialize.
func TestHandleConfigVersionNotDeduped(t *testing.T) {
	host := &fakeSessionHost{}
	d := newDispatcher(host, discardLoggerRunner())
	if r := d.handle(context.Background(), configVersionCommand("v-1")); r != nil {
		t.Fatalf("first ConfigVersion signal returned a result %+v, want nil", r)
	}
	if r := d.handle(context.Background(), configVersionCommand("v-2")); r != nil {
		t.Fatalf("second ConfigVersion signal returned a result %+v, want nil", r)
	}
}

// Coalescing: two ConfigVersion signals arriving DURING an in-flight pass
// collapse to exactly ONE follow-up pass — two extra signals do not queue two
// extra passes. Deterministic via the fake's entered/release gates: pass 1 is
// held mid-flight while both extra signals are delivered, so they can only
// coalesce into the single buffered slot. A bug that queued per-signal would run
// three passes total (2N Reloads at the host level); the coalescer runs two.
func TestConfigWorkerCoalescesSignalsMidPass(t *testing.T) {
	host := &fakeSessionHost{
		refreshConfigEntered: make(chan struct{}),
		refreshConfigRelease: make(chan struct{}),
	}
	d := newDispatcher(host, discardLoggerRunner())
	ctx, cancel := context.WithCancel(context.Background())
	go d.runConfigWorker(ctx)
	defer func() {
		cancel()
		<-d.configWorkerDone
	}()

	recvEntered := func(what string) {
		t.Helper()
		select {
		case <-host.refreshConfigEntered:
		case <-timeAfter():
			t.Fatalf("%s did not begin within the deadline", what)
		}
	}
	release := func(what string) {
		t.Helper()
		select {
		case host.refreshConfigRelease <- struct{}{}:
		case <-timeAfter():
			t.Fatalf("%s did not reach its release point within the deadline", what)
		}
	}

	// Pass 1 begins and blocks mid-flight. The worker is now parked inside the
	// pass, NOT draining the signal buffer — so the buffer state below is stable
	// and the assertion is race-free.
	d.signalConfig()
	recvEntered("first pass")
	// Two more signals arrive while pass 1 is in flight. They must coalesce into
	// the single pending slot: a coalescing buffer holds exactly ONE, so N
	// mid-pass signals yield exactly one follow-up pass, never N. A per-signal
	// queue (the regression) would hold 2 here and later run 2N passes.
	d.signalConfig()
	d.signalConfig()
	if n := len(d.configSignal); n != 1 {
		t.Fatalf("config signal buffer holds %d pending passes after two mid-pass signals, want exactly 1 (coalesced)", n)
	}

	// Let pass 1 finish; the single coalesced signal drives exactly ONE follow-up
	// pass, after which the buffer is drained and the worker parks.
	release("first pass")
	recvEntered("coalesced follow-up pass")
	release("follow-up pass")

	cancel()
	<-d.configWorkerDone
	if n := host.configRefreshCount(); n != 2 {
		t.Fatalf("RefreshConfig ran %d times, want 2 (one in-flight + one coalesced from two mid-pass signals)", n)
	}
}

// The config worker exits on ctx cancel, closing configWorkerDone — no leaked
// goroutine. A bug that ignored ctx.Done() would hang the join forever.
func TestConfigWorkerExitsOnContextCancel(t *testing.T) {
	host := &fakeSessionHost{}
	d := newDispatcher(host, discardLoggerRunner())
	ctx, cancel := context.WithCancel(context.Background())
	go d.runConfigWorker(ctx)
	cancel()
	select {
	case <-d.configWorkerDone:
	case <-timeAfter():
		t.Fatal("config worker did not exit on ctx cancel; goroutine leaked")
	}
}

// errWrap wraps an error so the sentinel mapping is proven to use errors.Is
// (unwrap), not identity — a wrapped errAlreadyRunning must still map to
// ALREADY_RUNNING.
func errWrap(err error) error { return &wrapError{err} }

type wrapError struct{ inner error }

func (w *wrapError) Error() string { return "wrapped: " + w.inner.Error() }
func (w *wrapError) Unwrap() error { return w.inner }
