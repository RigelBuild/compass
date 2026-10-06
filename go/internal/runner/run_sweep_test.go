//go:build unix

package runner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

type sweepScenario struct {
	name            string
	engine          func() runtime.WorkloadRuntime
	wantRemoved     []runtime.WorkloadID
	wantAttempted   []runtime.WorkloadID
	wantWarning     string
	checkConcurrent bool
}

type sweepTestEngine struct {
	runtime.WorkloadRuntime
	mu           sync.Mutex
	listed       []runtime.WorkloadID
	listErr      error
	removeErrors map[runtime.WorkloadID]error
	removed      map[runtime.WorkloadID]bool
	attempted    []runtime.WorkloadID
	entered      chan runtime.WorkloadID
	removeGate   chan struct{}
}

func (e *sweepTestEngine) ListByOwner(_ context.Context, prefix, runnerID string) ([]runtime.WorkloadID, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if prefix != AgentContainerNamePrefix || runnerID != "runner-1" {
		return nil, errors.New("unexpected ownership query")
	}
	return slices.Clone(e.listed), e.listErr
}

func (e *sweepTestEngine) Remove(_ context.Context, id runtime.WorkloadID) error {
	e.mu.Lock()
	e.attempted = append(e.attempted, id)
	gate := e.removeGate
	entered := e.entered
	err := e.removeErrors[id]
	e.mu.Unlock()
	if entered != nil {
		entered <- id
	}
	if gate != nil {
		<-gate
	}
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.removed[id] = true
	e.mu.Unlock()
	return nil
}

type noSweepTestEngine struct {
	runtime.WorkloadRuntime
	mu      sync.Mutex
	removed []runtime.WorkloadID
}

func (e *noSweepTestEngine) Remove(_ context.Context, id runtime.WorkloadID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.removed = append(e.removed, id)
	return nil
}

type cancelSweepTestEngine struct {
	*pipeRuntime
	cancel context.CancelFunc
}

func (e *cancelSweepTestEngine) ListByOwner(context.Context, string, string) ([]runtime.WorkloadID, error) {
	e.cancel()
	return nil, nil
}

func seedSweepRuntimeDir(t *testing.T) string {
	t.Helper()
	runtimeDir := t.TempDir()
	containerDir := filepath.Join(runtimeDir, agentSocketDir)
	agentDir := filepath.Join(containerDir, AgentContainerNamePrefix+"a")
	if err := os.MkdirAll(filepath.Join(agentDir, "config"), 0o700); err != nil {
		t.Fatalf("create stale agent config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, agentSocketFile), nil, 0o600); err != nil {
		t.Fatalf("create stale agent socket: %v", err)
	}
	if err := os.Mkdir(filepath.Join(containerDir, "other-x"), 0o700); err != nil {
		t.Fatalf("create other runtime dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, AgentContainerNamePrefix+"file"), nil, 0o600); err != nil {
		t.Fatalf("create stray file: %v", err)
	}
	return runtimeDir
}

func assertSweepRuntimeDir(t *testing.T, runtimeDir string) {
	t.Helper()
	containerDir := filepath.Join(runtimeDir, agentSocketDir)
	if _, err := os.Stat(filepath.Join(containerDir, AgentContainerNamePrefix+"a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale agent directory stat error = %v, want not-exist", err)
	}
	if info, err := os.Stat(filepath.Join(containerDir, "other-x")); err != nil || !info.IsDir() {
		t.Fatalf("other-x stat = (%v, %v), want surviving directory", info, err)
	}
	if info, err := os.Stat(filepath.Join(containerDir, AgentContainerNamePrefix+"file")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("stray file stat = (%v, %v), want surviving regular file", info, err)
	}
}

func TestSweepStaleAgentState(t *testing.T) {
	listErr := errors.New("list failed")
	removeErr := errors.New("remove failed")
	listed := []runtime.WorkloadID{"compass-agent-a", "compass-agent-b"}
	tests := []sweepScenario{
		{
			name: "removes containers concurrently and continues after failure",
			engine: func() runtime.WorkloadRuntime {
				return &sweepTestEngine{
					listed:       listed,
					removeErrors: map[runtime.WorkloadID]error{"compass-agent-a": removeErr},
					removed:      map[runtime.WorkloadID]bool{},
					entered:      make(chan runtime.WorkloadID, len(listed)),
					removeGate:   make(chan struct{}),
				}
			},
			wantRemoved:     []runtime.WorkloadID{"compass-agent-b"},
			wantAttempted:   listed,
			wantWarning:     "removing stale agent container",
			checkConcurrent: true,
		},
		{
			name: "list error still sweeps socket dirs",
			engine: func() runtime.WorkloadRuntime {
				return &sweepTestEngine{listErr: listErr, removed: map[runtime.WorkloadID]bool{}}
			},
			wantWarning: "listing stale agent containers",
		},
		{
			name: "backend without ownership probe skips containers",
			engine: func() runtime.WorkloadRuntime {
				return &noSweepTestEngine{}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { runSweepScenario(t, tc, listed) })
	}
}

func runSweepScenario(t *testing.T, tc sweepScenario, listed []runtime.WorkloadID) {
	t.Helper()
	runtimeDir := seedSweepRuntimeDir(t)
	engine := tc.engine()
	log := newCaptureLog()
	done := make(chan struct{})
	go func() {
		sweepStaleAgentContainers(t.Context(), engine, runtimeDir, "runner-1", log.logger())
		close(done)
	}()
	if tc.checkConcurrent {
		waitForRemoveStarts(t, engine.(*sweepTestEngine), listed)
	}
	awaitSweep(t, done)
	assertSweepRuntimeDir(t, runtimeDir)
	assertNoProbeWork(t, engine)
	assertSweepWarning(t, log, tc)
	assertSweepResults(t, engine, tc)
}

func waitForRemoveStarts(t *testing.T, engine *sweepTestEngine, listed []runtime.WorkloadID) {
	t.Helper()
	for range listed {
		select {
		case <-engine.entered:
		case <-timeAfter():
			t.Fatal("stale container removals did not start concurrently")
		}
	}
	close(engine.removeGate)
}

func awaitSweep(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-timeAfter():
		t.Fatal("startup sweep did not finish")
	}
}

func assertNoProbeWork(t *testing.T, engine runtime.WorkloadRuntime) {
	t.Helper()
	testEngine, ok := engine.(*noSweepTestEngine)
	if !ok {
		return
	}
	testEngine.mu.Lock()
	removed := slices.Clone(testEngine.removed)
	testEngine.mu.Unlock()
	if len(removed) != 0 {
		t.Fatalf("backend without ownership probe removed workloads: %v", removed)
	}
}

func assertSweepWarning(t *testing.T, log *captureLog, tc sweepScenario) {
	t.Helper()
	if tc.wantWarning == "" {
		return
	}
	lines := 1
	if tc.checkConcurrent {
		lines = 2
	}
	found := false
	for range lines {
		if got := log.recvLine(t); got.msg == tc.wantWarning {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing warning %q", tc.wantWarning)
	}
}

func assertSweepResults(t *testing.T, engine runtime.WorkloadRuntime, tc sweepScenario) {
	t.Helper()
	testEngine, ok := engine.(*sweepTestEngine)
	if !ok {
		return
	}
	testEngine.mu.Lock()
	defer testEngine.mu.Unlock()
	if !sameWorkloadIDs(testEngine.attempted, tc.wantAttempted) {
		t.Fatalf("Remove attempts = %v, want %v", testEngine.attempted, tc.wantAttempted)
	}
	gotRemoved := make([]runtime.WorkloadID, 0, len(testEngine.removed))
	for id := range testEngine.removed {
		gotRemoved = append(gotRemoved, id)
	}
	if !sameWorkloadIDs(gotRemoved, tc.wantRemoved) {
		t.Fatalf("successful removals = %v, want %v", gotRemoved, tc.wantRemoved)
	}
}

func sameWorkloadIDs(a, b []runtime.WorkloadID) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

func TestRunReturnsNilWhenSweepCancelsContext(t *testing.T) {
	rec := &recordingEnroll{}
	url := recordingEnrollServer(t, rec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine := &cancelSweepTestEngine{pipeRuntime: newPipeRuntime(), cancel: cancel}
	err := Run(ctx, RunnerConfig{
		RunnerID:   "runner-1",
		ServerAddr: url,
		Token:      "tok",
		Engine:     engine,
		RuntimeDir: shortRuntimeDir(t),
		HTTPClient: h2cHTTPClient(t),
	}, nil, discardLoggerRunner())
	if err != nil {
		t.Fatalf("Run after cancellation during startup sweep = %v, want nil", err)
	}
	if rec.enrolled() == nil {
		t.Fatal("Run did not enroll before starting the stale-state sweep")
	}
}

type blockingRemoveTestEngine struct {
	*pipeRuntime
	listed    chan struct{}
	entered   chan struct{}
	removed   chan struct{}
	removeErr error // the ctx error Remove unwound on; read after removed closes
}

func (e *blockingRemoveTestEngine) ListByOwner(context.Context, string, string) ([]runtime.WorkloadID, error) {
	close(e.listed)
	return []runtime.WorkloadID{"compass-agent-stale"}, nil
}

func (e *blockingRemoveTestEngine) Remove(ctx context.Context, _ runtime.WorkloadID) error {
	close(e.entered)
	<-ctx.Done()
	e.removeErr = ctx.Err()
	close(e.removed)
	return e.removeErr
}

type sessionsStartedTestHandler struct {
	recordingEnroll
	sessions chan struct{}
}

func (h *sessionsStartedTestHandler) Sessions(context.Context, *connect.BidiStream[compassv1internal.SessionsRequest, compassv1internal.SessionsResponse]) error {
	close(h.sessions)
	return nil
}

func TestRunStartsSessionsWhenStaleSweepTimesOut(t *testing.T) {
	prev := staleContainerSweepTimeout
	staleContainerSweepTimeout = 100 * time.Millisecond
	t.Cleanup(func() { staleContainerSweepTimeout = prev })
	handler := &sessionsStartedTestHandler{sessions: make(chan struct{})}
	path, service := compassv1internalconnect.NewRunnerServiceHandler(handler)
	mux := http.NewServeMux()
	mux.Handle(path, service)
	server := httptest.NewUnstartedServer(mux)
	server.Config.Protocols = cleartextHTTP2()
	server.Start()
	t.Cleanup(server.Close)

	engine := &blockingRemoveTestEngine{
		pipeRuntime: newPipeRuntime(),
		listed:      make(chan struct{}),
		entered:     make(chan struct{}),
		removed:     make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(t.Context())
	runDone := make(chan struct{})
	var runErr error
	runtimeDir := shortRuntimeDir(t)
	httpClient := h2cHTTPClient(t)
	go func() {
		defer close(runDone)
		runErr = Run(ctx, RunnerConfig{
			RunnerID:   "runner-1",
			ServerAddr: server.URL,
			Token:      "tok",
			Engine:     engine,
			RuntimeDir: runtimeDir,
			HTTPClient: httpClient,
		}, nil, discardLoggerRunner())
	}()
	// Registered after the timeout restore so it runs first: Run is joined
	// before the var and runtime dir it reads are torn down.
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(testTimeout):
			t.Error("Run did not return after cancellation")
		}
	})

	// Event-gated, no wall-clock bound asserted: the blocked Remove unwinds on the
	// sweep's own deadline, then Sessions must start without any cancel.
	select {
	case <-engine.entered:
	case <-runDone:
		t.Fatalf("Run returned %v before the stale Remove started", runErr)
	case <-time.After(testTimeout):
		t.Fatal("stale Remove did not start")
	}
	select {
	case <-engine.removed:
	case <-time.After(testTimeout):
		t.Fatal("stale cleanup did not stop at its deadline")
	}
	if !errors.Is(engine.removeErr, context.DeadlineExceeded) {
		t.Fatalf("stale Remove unwound on %v, want the sweep deadline", engine.removeErr)
	}
	select {
	case <-handler.sessions:
	case <-runDone:
		t.Fatalf("Run returned %v before Sessions started", runErr)
	case <-time.After(testTimeout):
		t.Fatal("Sessions did not start after the stale sweep deadline")
	}
	select {
	case <-runDone:
		if runErr != nil {
			t.Fatalf("Run after bounded startup sweep = %v, want nil", runErr)
		}
	case <-time.After(testTimeout):
		t.Fatal("Run did not return after Sessions completed")
	}
}

// The sweep reaches container backends only through a type assertion.
var _ ownedWorkloadLister = (*runtime.PodmanCLI)(nil)
var _ ownedWorkloadLister = (*runtime.AppleContainerCLI)(nil)
