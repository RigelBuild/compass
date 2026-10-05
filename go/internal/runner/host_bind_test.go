//go:build unix

package runner

// The Runner binds a lifetime's transcript base under the container lock, after
// the live-session checks and before StartAgent: once per accepted resume or
// reload, never on a refused or fresh Start. A bind failure fails the command
// before the agent runs and maps to RUNNER_ERROR_CODE_INTERNAL.

import (
	"errors"
	"sync"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

const bindAccount = "0123456789abcdef0123456789abcdef"

// provisionForBind provisions cont-1 on a capture-backed host and returns it.
func provisionForBind(t *testing.T) (SessionHost, *stubStreamingRuntime, *capturePublish) {
	t.Helper()
	host, engine, pub := newHostFixtureWithPublish(t, &fakeSpecBuilder{spec: liveSpec()})
	if _, err := host.Provision(t.Context(), &compassv1.ProvisionAgentWorkspaceRequest{}, bindAccount); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	return host, engine, pub
}

// execsAtBind arms the bind hook to record how many agent launches had run when
// each bind arrived, so a test can assert the bind precedes StartAgent.
func execsAtBind(pub *capturePublish, engine *stubStreamingRuntime, bindErr error) func() []int {
	var mu sync.Mutex
	var seen []int
	pub.setBind(bindErr, func() {
		n := engine.countCall("exec_streaming")
		mu.Lock()
		seen = append(seen, n)
		mu.Unlock()
	})
	return func() []int {
		mu.Lock()
		defer mu.Unlock()
		return append([]int(nil), seen...)
	}
}

// runnerCode maps err through the dispatcher's wire mapping.
func runnerCode(t *testing.T, err error) compassv1internal.RunnerErrorCode {
	t.Helper()
	return newDispatcher(&fakeSessionHost{}, discardLoggerRunner()).errorResult(t.Context(), "r", err).GetError().GetCode()
}

func stopSession(t *testing.T, host SessionHost, sessionID string) {
	t.Helper()
	if err := host.Stop(t.Context(), sessionID); err != nil {
		t.Fatalf("Stop(%q) = %v", sessionID, err)
	}
}

func TestResumeStartBindsOnceBeforeStartAgent(t *testing.T) {
	host, engine, pub := provisionForBind(t)
	seen := execsAtBind(pub, engine, nil)

	sessionID, err := host.Start(t.Context(), &compassv1.StartAgentSessionRequest{ContainerName: "cont-1", ResumeSessionId: "resume-1"}, "body", "")
	if err != nil {
		t.Fatalf("resume Start = %v", err)
	}
	binds := pub.bindRequests()
	if len(binds) != 1 {
		t.Fatalf("resume Start made %d binds, want exactly 1", len(binds))
	}
	if binds[0].GetContainerName() != "cont-1" || binds[0].GetSessionId() != "resume-1" {
		t.Fatalf("bind = (%q, %q), want (cont-1, resume-1)", binds[0].GetContainerName(), binds[0].GetSessionId())
	}
	if seen()[0] != 0 {
		t.Fatalf("bind arrived after %d agent launches, want 0 (bind precedes StartAgent)", seen()[0])
	}
	stopSession(t, host, sessionID)
}

// A refused resume must not move a live session's base: zero binds.
func TestResumeStartOnLiveContainerRecordsZeroBinds(t *testing.T) {
	host, _, pub := provisionForBind(t)
	live, err := host.Start(t.Context(), &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, "", "live-1")
	if err != nil {
		t.Fatalf("fresh Start = %v", err)
	}
	for _, resumeID := range []string{live, "other-session"} {
		_, err := host.Start(t.Context(), &compassv1.StartAgentSessionRequest{ContainerName: "cont-1", ResumeSessionId: resumeID}, "body", "")
		if !errors.Is(err, errAlreadyRunning) {
			t.Fatalf("resume %q on a live container = %v, want errAlreadyRunning", resumeID, err)
		}
		if got := runnerCode(t, err); got != compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_ALREADY_RUNNING {
			t.Fatalf("refused resume code = %v, want ALREADY_RUNNING", got)
		}
	}
	if n := len(pub.bindRequests()); n != 0 {
		t.Fatalf("refused resumes made %d binds, want 0", n)
	}
	stopSession(t, host, live)
}

// A fresh Start has no session row yet; it keeps the default base 0.
func TestFreshStartRecordsZeroBinds(t *testing.T) {
	host, _, pub := provisionForBind(t)
	sessionID, err := host.Start(t.Context(), &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, "", "fresh-1")
	if err != nil {
		t.Fatalf("fresh Start = %v", err)
	}
	if n := len(pub.bindRequests()); n != 0 {
		t.Fatalf("fresh Start made %d binds, want 0", n)
	}
	stopSession(t, host, sessionID)
}

// A bind error fails the resume before the agent runs, as INTERNAL: NOT_FOUND
// would make the Server reprovision the container.
func TestResumeStartBindErrorFailsBeforeStartAgent(t *testing.T) {
	host, engine, pub := provisionForBind(t)
	pub.setBind(connect.NewError(connect.CodePermissionDenied, errors.New("denied")), nil)

	_, err := host.Start(t.Context(), &compassv1.StartAgentSessionRequest{ContainerName: "cont-1", ResumeSessionId: "resume-1"}, "body", "")
	if err == nil {
		t.Fatal("resume Start with a failing bind = nil, want an error")
	}
	if errors.Is(err, errSessionUnknown) {
		t.Fatalf("bind failure wraps errSessionUnknown: %v", err)
	}
	if got := runnerCode(t, err); got != compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_INTERNAL {
		t.Fatalf("bind failure code = %v, want INTERNAL", got)
	}
	if n := engine.countCall("exec_streaming"); n != 0 {
		t.Fatalf("agent launched %d times after a failed bind, want 0", n)
	}
	if statuses, err := host.Status(t.Context(), ""); err != nil || len(statuses) != 0 {
		t.Fatalf("Status after failed bind = %v, %v; want no session", statuses, err)
	}
}

func TestReloadBindsOnceBeforeStartAgent(t *testing.T) {
	host, engine, pub := provisionForBind(t)
	sessionID, err := host.Start(t.Context(), &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, "", "reload-1")
	if err != nil {
		t.Fatalf("fresh Start = %v", err)
	}
	seen := execsAtBind(pub, engine, nil)

	if err := host.Reload(t.Context(), sessionID); err != nil {
		t.Fatalf("Reload = %v", err)
	}
	binds := pub.bindRequests()
	if len(binds) != 1 {
		t.Fatalf("Reload made %d binds, want exactly 1", len(binds))
	}
	if binds[0].GetContainerName() != "cont-1" || binds[0].GetSessionId() != sessionID {
		t.Fatalf("bind = (%q, %q), want (cont-1, %q)", binds[0].GetContainerName(), binds[0].GetSessionId(), sessionID)
	}
	if seen()[0] != 1 {
		t.Fatalf("bind arrived after %d agent launches, want 1 (only the original; bind precedes the relaunch)", seen()[0])
	}
	if n := engine.countCall("exec_streaming"); n != 2 {
		t.Fatalf("agent launches after Reload = %d, want 2", n)
	}
	stopSession(t, host, sessionID)
}

func TestReloadBindErrorFailsBeforeStartAgent(t *testing.T) {
	host, engine, pub := provisionForBind(t)
	sessionID, err := host.Start(t.Context(), &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, "", "reload-2")
	if err != nil {
		t.Fatalf("fresh Start = %v", err)
	}
	pub.setBind(connect.NewError(connect.CodeUnavailable, errors.New("server down")), nil)

	err = host.Reload(t.Context(), sessionID)
	if err == nil {
		t.Fatal("Reload with a failing bind = nil, want an error")
	}
	if errors.Is(err, errSessionUnknown) {
		t.Fatalf("bind failure wraps errSessionUnknown: %v", err)
	}
	if got := runnerCode(t, err); got != compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_INTERNAL {
		t.Fatalf("Reload bind failure code = %v, want INTERNAL", got)
	}
	if n := engine.countCall("exec_streaming"); n != 1 {
		t.Fatalf("agent launches after failed Reload bind = %d, want 1 (no relaunch)", n)
	}
	statuses, err := host.Status(t.Context(), sessionID)
	if err != nil || len(statuses) != 1 || statuses[0].GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED {
		t.Fatalf("Status after failed Reload bind = %v, %v; want one ERRORED session", statuses, err)
	}
}

// A Reload between a fresh Start and the Server's session-row write is denied
// (no row). With no row there are no transcript rows, so the relaunch proceeds.
func TestReloadBindDeniedStillRelaunches(t *testing.T) {
	host, engine, pub := provisionForBind(t)
	sessionID, err := host.Start(t.Context(), &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, "", "reload-3")
	if err != nil {
		t.Fatalf("fresh Start = %v", err)
	}
	pub.setBind(connect.NewError(connect.CodePermissionDenied, errors.New("no row")), nil)

	if err := host.Reload(t.Context(), sessionID); err != nil {
		t.Fatalf("Reload with a denied bind = %v, want the relaunch to proceed", err)
	}
	if n := len(pub.bindRequests()); n != 1 {
		t.Fatalf("Reload made %d binds, want 1", n)
	}
	if n := engine.countCall("exec_streaming"); n != 2 {
		t.Fatalf("agent launches after denied Reload bind = %d, want 2 (relaunched)", n)
	}
	statuses, err := host.Status(t.Context(), sessionID)
	if err != nil || len(statuses) != 1 || statuses[0].GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_READY {
		t.Fatalf("Status after denied Reload bind = %v, %v; want one READY session", statuses, err)
	}
	stopSession(t, host, sessionID)
}
