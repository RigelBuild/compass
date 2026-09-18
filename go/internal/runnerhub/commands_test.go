//go:build unix

package runnerhub

// The Server-facing command surface: the RunnerError→Connect code mapping and
// the no-Runner→Unavailable path. Every row pins the exact Connect code a client
// sees, so a mis-mapping (an ALREADY_RUNNING surfaced as Internal, a NOT_FOUND
// swallowed) is caught. The end-to-end relay test drives the mapping through the
// real router so the wiring, not just the pure function, is proven.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// runnerErrorToConnect maps each RunnerErrorCode to the exact Connect code the
// client sees. Table-driven over every code, including UNSPECIFIED (the "else"
// arm) which must fall to Internal.
func TestRunnerErrorToConnectCodeMapping(t *testing.T) {
	cases := []struct {
		name string
		code compassv1internal.RunnerErrorCode
		want connect.Code
	}{
		{"already running", compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_ALREADY_RUNNING, connect.CodeAlreadyExists},
		{"not found", compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_NOT_FOUND, connect.CodeNotFound},
		{"failed precondition", compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_FAILED_PRECONDITION, connect.CodeFailedPrecondition},
		{"internal", compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_INTERNAL, connect.CodeInternal},
		{"unspecified falls to internal", compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_UNSPECIFIED, connect.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runnerErrorToConnect(&compassv1internal.RunnerError{Code: tc.code, Message: "boom"})
			if got := connect.CodeOf(err); got != tc.want {
				t.Fatalf("runnerErrorToConnect(%v) code = %v, want %v", tc.code, got, tc.want)
			}
			// The Runner's message is surfaced, not swallowed.
			mustContain(t, err.Error(), "boom")
		})
	}
}

// A session RPC with no enrolled Runner is Unavailable — there is no Runner to
// serve it, a transport-class failure, never a per-command error code. Driven
// across every command surface so no method mis-classifies the empty registry.
func TestCommandsNoRunnerIsUnavailable(t *testing.T) {
	hub := newHubOnly() // no enroll → no Runner
	ctx := context.Background()

	cases := []struct {
		name string
		call func() error
	}{
		{"provision", func() error {
			_, _, err := hub.Provision(ctx, "r1", &compassv1.ProvisionAgentWorkspaceRequest{})
			return err
		}},
		{"start", func() error {
			_, err := hub.Start(ctx, "r1", &compassv1.StartAgentSessionRequest{ContainerName: "c1"})
			return err
		}},
		{"stop", func() error {
			_, err := hub.Stop(ctx, "r1", &compassv1.StopAgentSessionRequest{SessionId: "s1"})
			return err
		}},
		{"reload", func() error {
			_, err := hub.Reload(ctx, "r1", &compassv1.ReloadAgentSessionRequest{SessionId: "s1"})
			return err
		}},
		{"status", func() error {
			_, err := hub.Status(ctx, "r1", &compassv1.GetAgentStatusRequest{SessionId: "s1"})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("%s with no runner = nil error, want Unavailable", tc.name)
			}
			if got := connect.CodeOf(err); got != connect.CodeUnavailable {
				t.Fatalf("%s with no runner = code %v, want Unavailable", tc.name, got)
			}
		})
	}
}

// End-to-end through the real router: a Start whose Runner returns an
// ALREADY_RUNNING RunnerError surfaces as CodeAlreadyExists at the Hub.Start
// boundary — proving the relay wires the mapping, not just the pure function.
// This is OQ6 row 3's genuine-double half: a real already-live container.
func TestStartRelaySurfacesAlreadyRunningAsAlreadyExists(t *testing.T) {
	hub := newHubOnly()
	// Enroll a Runner and bind a send that answers every command with an
	// ALREADY_RUNNING error result correlated by the pushed request id.
	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	router, _, err := hub.routerFor("any")
	if err != nil {
		t.Fatalf("routerFor after enroll = %v, want a router", err)
	}
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		// Answer asynchronously the way the real Sessions loop does.
		go router.complete(&compassv1internal.SessionsRequest{
			RequestId: cmd.GetRequestId(),
			Result: &compassv1internal.SessionsRequest_Error{Error: &compassv1internal.RunnerError{
				Code:    compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_ALREADY_RUNNING,
				Message: "session already running on container",
			}},
		})
		return nil
	})

	_, err = hub.Start(context.Background(), "req-1", &compassv1.StartAgentSessionRequest{ContainerName: "c1"})
	if err == nil {
		t.Fatal("Start against an already-running container = nil, want AlreadyExists")
	}
	if got := connect.CodeOf(err); got != connect.CodeAlreadyExists {
		t.Fatalf("Start error code = %v, want AlreadyExists (a genuine double start)", got)
	}
}

// A successful relay returns the typed response, not an error — the happy path
// through the same wiring, so the error tests above are not the only path
// exercised.
func TestStartRelayReturnsSessionIdOnSuccess(t *testing.T) {
	hub := newHubOnly()
	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	router, _, _ := hub.routerFor("any")
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		go router.complete(&compassv1internal.SessionsRequest{
			RequestId: cmd.GetRequestId(),
			Result:    &compassv1internal.SessionsRequest_Start{Start: &compassv1.StartAgentSessionResponse{SessionId: "sess-ok"}},
		})
		return nil
	})

	resp, err := hub.Start(context.Background(), "req-ok", &compassv1.StartAgentSessionRequest{ContainerName: "c1"})
	if err != nil {
		t.Fatalf("Start = %v, want success", err)
	}
	if got := resp.GetSessionId(); got != "sess-ok" {
		t.Fatalf("Start session id = %q, want sess-ok", got)
	}
}

// attachStartStopRecorder enrolls a Runner and answers every Start with
// startedSession and every Stop with success, recording which command variants
// reached the wire — the seam the RIG-3696 rollback tests assert on. A Start
// answered successfully is what makes the rollback reachable at all: the session
// IS live on the Runner when the promotion is refused.
func attachStartStopRecorder(t *testing.T, hub *Hub, startedSession string) *startStopRecorder {
	t.Helper()
	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	router, _, err := hub.routerFor("any")
	if err != nil {
		t.Fatalf("routerFor after enroll = %v, want a router", err)
	}
	return bindStartStopRecorder(router, startedSessionResult(startedSession))
}

// bindStartStopRecorder binds a send onto router that answers every Start with
// startResult(requestID) and every Stop with success, recording both. Split out
// of attachStartStopRecorder so a test can bind a SECOND router (a replacement
// Runner's, after a re-enroll) or vary what the Start is answered with (a
// MALFORMED result).
func bindStartStopRecorder(router *commandRouter, startResult func(requestID string) *compassv1internal.SessionsRequest) *startStopRecorder {
	rec := &startStopRecorder{}
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		var result *compassv1internal.SessionsRequest
		switch cmd.GetCommand().(type) {
		case *compassv1internal.SessionsResponse_Start:
			rec.recordStart()
			result = startResult(cmd.GetRequestId())
		case *compassv1internal.SessionsResponse_Stop:
			rec.recordStop(cmd.GetStop().GetSessionId())
			result = &compassv1internal.SessionsRequest{
				RequestId: cmd.GetRequestId(),
				Result:    &compassv1internal.SessionsRequest_Stop{Stop: &compassv1.StopAgentSessionResponse{}},
			}
		}
		go router.complete(result)
		return nil
	})
	return rec
}

// startedSessionResult is the WELL-FORMED Start answer: a start response
// carrying sessionID, correlated by request id.
func startedSessionResult(sessionID string) func(requestID string) *compassv1internal.SessionsRequest {
	return func(requestID string) *compassv1internal.SessionsRequest {
		return &compassv1internal.SessionsRequest{
			RequestId: requestID,
			Result:    &compassv1internal.SessionsRequest_Start{Start: &compassv1.StartAgentSessionResponse{SessionId: sessionID}},
		}
	}
}

// startStopRecorder records the Start count and the session ids Stop was asked
// to tear down. Concurrency-safe: the router's sender goroutine drives it.
type startStopRecorder struct {
	mu      sync.Mutex
	starts  int
	stopped []string
}

func (r *startStopRecorder) recordStart() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts++
}

func (r *startStopRecorder) recordStop(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = append(r.stopped, sessionID)
}

func (r *startStopRecorder) snapshot() (starts int, stopped []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts, append([]string(nil), r.stopped...)
}

// conflictOnSession returns a fake binding store whose durable table already
// binds sessionID to another account, so promoting it raises store.ErrConflict —
// the RIG-3696 precondition both start legs must fail closed on.
func conflictOnSession(hub *Hub, sessionID string) *fakeBindingStore {
	bindings := newFakeBindingStore()
	seedConflict(bindings, sessionID)
	hub.SetSessionBindingStore(bindings)
	return bindings
}

// seedConflict seeds sessionID as durably owned by ANOTHER account, bypassing
// the displacement path (test setup, like fakeBindingStore.seed).
func seedConflict(bindings *fakeBindingStore, sessionID string) {
	bindings.mu.Lock()
	defer bindings.mu.Unlock()
	bindings.bindings[sessionID] = store.SessionBinding{SessionID: sessionID, AccountID: "acct-durable-owner", RunnerID: testRunnerID}
}

// durableOwnerOf reads the account the durable table currently binds sessionID
// to — the probe for "the row the conflict protected SURVIVED the rollback".
func durableOwnerOf(bindings *fakeBindingStore, sessionID string) store.AccountID {
	bindings.mu.Lock()
	defer bindings.mu.Unlock()
	return bindings.bindings[sessionID].AccountID
}

// TestStartRollsBackTheStartedSessionOnABindingConflict is the RIG-3696 caller
// half: when the promotion is REFUSED (the minted session id is durably owned by
// another account), Start must not return success on a session nothing can
// authorize. It stops the session it just started and returns the conflict.
//
// The Stop MUST go through the raw relay, not Hub.Stop: Hub.Stop follows its
// relay with unbindSession, which deletes the durable row by session id — and
// that row belongs to the OTHER account, whose ownership is the whole reason the
// promotion was refused. This asserts the row SURVIVES the rollback, which is
// what fails if the rollback is ever re-pointed at Hub.Stop.
func TestStartRollsBackTheStartedSessionOnABindingConflict(t *testing.T) {
	const contested = "sess-contested"
	hub := newHubOnly()
	rec := attachStartStopRecorder(t, hub, contested)
	bindings := conflictOnSession(hub, contested)
	hub.bindContainer("c1", testAgentAccount)

	resp, err := hub.Start(context.Background(), "req-1", &compassv1.StartAgentSessionRequest{ContainerName: "c1"})

	if err == nil {
		t.Fatalf("Start with a conflicting binding = (%v, nil), want an error: a session no binding can authorize must not be reported as started", resp)
	}
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Start error = %v, want one wrapping store.ErrConflict (the cause must survive the Connect wrap)", err)
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("Start error code = %v, want Internal", got)
	}
	starts, stopped := rec.snapshot()
	if starts != 1 {
		t.Fatalf("Start commands relayed = %d, want 1 (the session WAS started; that is why it needs rolling back)", starts)
	}
	if len(stopped) != 1 || stopped[0] != contested {
		t.Fatalf("Stop commands relayed = %+v, want exactly [%s] — the started session must be torn down", stopped, contested)
	}
	// The durable owner's row survived: the rollback used the raw relay, so no
	// unbindSession ran to delete the row the conflict was protecting.
	if owner := durableOwnerOf(bindings, contested); owner != "acct-durable-owner" {
		t.Fatalf("durable binding for %s after rollback = %q, want acct-durable-owner — rolling back through Hub.Stop would unbind the very row the conflict protected", contested, owner)
	}
	// And the rolled-back session never resolves to the account that tried to
	// claim it. The read-through still answers with the DURABLE OWNER (the row
	// above survived), which is the arbiter's correct answer; the promoting
	// account resolving is the divergence this whole change closes.
	if acct, ok := hub.accountForSession(context.Background(), contested); ok && acct == testAgentAccount {
		t.Fatalf("accountForSession(%s) after a rolled-back Start = %q — the promoting account must never resolve the session it failed to claim", contested, acct)
	}
}

// TestStartResumeRollsBackTheStartedSessionOnABindingConflict: the resume leg
// carries IDENTICAL promotion semantics. It matters more here than on the fresh
// leg — a resume deliberately REUSES the logical session id as the live id, so a
// durably-owned id is reachable by construction rather than by a collision.
// The resume body is still relayed verbatim (the promotion runs after the relay);
// what changes is that the refused promotion tears the session back down.
func TestStartResumeRollsBackTheStartedSessionOnABindingConflict(t *testing.T) {
	const contested = "sess-resumed"
	hub := newHubOnly()
	rec := attachStartStopRecorder(t, hub, contested)
	bindings := conflictOnSession(hub, contested)
	hub.bindContainer("c1", testAgentAccount)

	resp, err := hub.StartResume(context.Background(), "req-1",
		&compassv1.StartAgentSessionRequest{ContainerName: "c1", ResumeSessionId: contested},
		[]byte("transcript body"))

	if err == nil {
		t.Fatalf("StartResume with a conflicting binding = (%v, nil), want an error", resp)
	}
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("StartResume error = %v, want one wrapping store.ErrConflict", err)
	}
	starts, stopped := rec.snapshot()
	if starts != 1 {
		t.Fatalf("Start commands relayed = %d, want 1", starts)
	}
	if len(stopped) != 1 || stopped[0] != contested {
		t.Fatalf("Stop commands relayed = %+v, want exactly [%s] — the resumed session must be torn down too", stopped, contested)
	}
	// The durable owner's row survived the RESUME rollback too. The resume leg is
	// where this matters most: it REUSES the logical id, so the row the conflict
	// protected is the row a Hub.Stop rollback would delete by that very id.
	if owner := durableOwnerOf(bindings, contested); owner != "acct-durable-owner" {
		t.Fatalf("durable binding for %s after the resume rollback = %q, want acct-durable-owner — rolling back through Hub.Stop would unbind the very row the conflict protected", contested, owner)
	}
}

// TestStartLegsRejectAMalformedStartResponse: a relay that returned no error has
// established only that the Runner answered WITHOUT a RunnerError — not that it
// answered the command that was asked. Both start legs must reject an answer
// carrying no start response instead of reading GetStart() straight through.
//
// RED against pre-fix: `resp := result.GetStart()` yielded nil, promoteSession's
// own empty-session-id guard then refused to promote and returned nil, and the
// leg returned (nil, nil) — a "success" with no response at all. The handler
// wraps that nil message into a Connect response and records session ownership
// under an EMPTY session id, while whatever the Runner did start runs on under
// an id nobody holds.
//
// No rollback may be attempted either: the answer names no session to Stop, and
// the promotion never ran, so there is nothing to undo. Both are asserted — the
// container binding SURVIVING is the direct probe that promoteSession never ran
// (a promotion deletes it).
func TestStartLegsRejectAMalformedStartResponse(t *testing.T) {
	shapes := []struct {
		name  string
		build func(requestID string) *compassv1internal.SessionsRequest
	}{
		{"no result variant set", func(requestID string) *compassv1internal.SessionsRequest {
			return &compassv1internal.SessionsRequest{RequestId: requestID}
		}},
		{"a stop response correlated onto the start's request id", func(requestID string) *compassv1internal.SessionsRequest {
			return &compassv1internal.SessionsRequest{
				RequestId: requestID,
				Result:    &compassv1internal.SessionsRequest_Stop{Stop: &compassv1.StopAgentSessionResponse{}},
			}
		}},
	}
	legs := []struct {
		name string
		call func(hub *Hub) (*compassv1.StartAgentSessionResponse, error)
	}{
		{"Start", func(hub *Hub) (*compassv1.StartAgentSessionResponse, error) {
			return hub.Start(context.Background(), "req-1", &compassv1.StartAgentSessionRequest{ContainerName: "c1"})
		}},
		{"StartResume", func(hub *Hub) (*compassv1.StartAgentSessionResponse, error) {
			return hub.StartResume(context.Background(), "req-1",
				&compassv1.StartAgentSessionRequest{ContainerName: "c1", ResumeSessionId: "sess-logical"},
				[]byte("transcript body"))
		}},
	}
	for _, shape := range shapes {
		for _, leg := range legs {
			t.Run(leg.name+"/"+shape.name, func(t *testing.T) {
				hub := newHubOnly()
				hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
				router, _, err := hub.routerFor("any")
				if err != nil {
					t.Fatalf("routerFor after enroll = %v, want a router", err)
				}
				rec := bindStartStopRecorder(router, shape.build)
				hub.bindContainer("c1", testAgentAccount)

				resp, err := leg.call(hub)

				if err == nil {
					t.Fatalf("%s against a %s = (%v, nil), want an error: a start with no response must never report success", leg.name, shape.name, resp)
				}
				if resp != nil {
					t.Fatalf("%s against a %s returned response %v alongside its error, want nil", leg.name, shape.name, resp)
				}
				if got := connect.CodeOf(err); got != connect.CodeInternal {
					t.Fatalf("%s error code = %v, want Internal (the Runner broke its own contract; not a client fault to retry)", leg.name, got)
				}
				starts, stopped := rec.snapshot()
				if starts != 1 {
					t.Fatalf("Start commands relayed = %d, want 1 (the malformed answer is the REPLY to a relayed start)", starts)
				}
				if len(stopped) != 0 {
					t.Fatalf("Stop commands relayed = %+v, want none: the answer names no session to stop, so no rollback may be attempted", stopped)
				}
				if _, ok := hub.AccountForContainer("c1"); !ok {
					t.Fatalf("container binding for c1 was cleared, want it intact: the leg must bail BEFORE promoteSession, which is what clears it")
				}
			})
		}
	}
}

// TestStartResponseRejectsANilResult covers the one malformed shape real routing
// cannot deliver: complete() keys on the result's request id, and a nil result
// reads as the empty id, so it matches no in-flight call and the dispatch never
// returns. The nil guard is therefore defence in depth against a future relay
// path that could hand one back — exercised directly, since no wiring can.
func TestStartResponseRejectsANilResult(t *testing.T) {
	resp, err := startResponse(nil)
	if err == nil {
		t.Fatalf("startResponse(nil) = (%v, nil), want an error", resp)
	}
	if resp != nil {
		t.Fatalf("startResponse(nil) returned response %v alongside its error, want nil", resp)
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("startResponse(nil) code = %v, want Internal", got)
	}
}

// TestStartRollbackStopsOnTheRunnerThatStartedTheSession: the rollback Stop must
// reach the Runner that RETURNED the start response, never whichever Runner
// happens to be enrolled by the time the promotion is refused.
//
// The window is real and not narrow. promoteSession releases h.mu BEFORE its
// durable write (relay_comms.go), enroll arrives on its own Enroll RPC, and
// enroll does NOT detach the previous attachment — so a Runner can re-enroll
// during the store round-trip while the previous Sessions stream is still open
// and still holding the started session. Each enroll installs a FRESH
// commandRouter (hub.go).
//
// RED against pre-fix: the rollback re-resolved the router through h.relay →
// routerFor → h.runner, so the Stop went down the REPLACEMENT's stream. The
// Runner actually holding the session never hears it and keeps it running
// unreaped — the exact stranding the rollback exists to prevent — and against a
// replacement that re-minted the same id the Stop would tear down an UNRELATED
// session (which is why enroll drops every binding in the first place).
func TestStartRollbackStopsOnTheRunnerThatStartedTheSession(t *testing.T) {
	const contested = "sess-contested"
	hub := newHubOnly()
	original := attachStartStopRecorder(t, hub, contested)
	bindings := &reenrollingBindingStore{fakeBindingStore: newFakeBindingStore(), hub: hub}
	seedConflict(bindings.fakeBindingStore, contested)
	hub.SetSessionBindingStore(bindings)
	hub.bindContainer("c1", testAgentAccount)

	_, err := hub.Start(context.Background(), "req-1", &compassv1.StartAgentSessionRequest{ContainerName: "c1"})

	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Start error = %v, want one wrapping store.ErrConflict", err)
	}
	if bindings.replacement == nil {
		t.Fatal("precondition: no replacement Runner enrolled during the durable write, so the re-enroll window was never opened")
	}
	starts, stopped := original.snapshot()
	if starts != 1 {
		t.Fatalf("Start commands on the ORIGINAL runner = %d, want 1", starts)
	}
	if len(stopped) != 1 || stopped[0] != contested {
		t.Fatalf("Stop commands on the ORIGINAL runner = %+v, want exactly [%s]: the rollback must reach the runner that started the session", stopped, contested)
	}
	if _, replacementStopped := bindings.replacement.snapshot(); len(replacementStopped) != 0 {
		t.Fatalf("Stop commands on the REPLACEMENT runner = %+v, want none: a re-enrollment must never redirect the rollback", replacementStopped)
	}
	// The re-enroll's own durable reap is keyed by the REPLACEMENT's runner id, so
	// the conflicting row (seeded under the original) is untouched by it — and the
	// rollback's direct Stop leaves it alone too.
	if owner := durableOwnerOf(bindings.fakeBindingStore, contested); owner != "acct-durable-owner" {
		t.Fatalf("durable binding for %s after the rollback = %q, want acct-durable-owner", contested, owner)
	}
}

// reenrollingBindingStore enrolls a REPLACEMENT Runner from inside the
// conflicting RecordSessionBinding call, then returns the conflict — making the
// re-enroll-during-the-durable-write interleaving deterministic rather than
// hoping the scheduler produces it (the posture blockingBindingStore takes for
// the reap window). The replacement enrolls under a DIFFERENT runner id, so
// enroll's durable reap — keyed by runner id — does not sweep the conflicting row
// this test also asserts on.
type reenrollingBindingStore struct {
	*fakeBindingStore
	hub *Hub
	// replacement is the replacement Runner's recorder, bound at re-enroll. Read
	// by the test after the Start returns; written on the same goroutine (the
	// store call runs inline under Start), so it needs no lock of its own.
	replacement *startStopRecorder
}

func (s *reenrollingBindingStore) RecordSessionBinding(ctx context.Context, sessionID string, accountID store.AccountID, runnerID string) (string, error) {
	if s.replacement == nil {
		s.hub.enroll(ctx, "runner-2", store.Subject{Kind: store.SubjectRunner, ID: "runner-2"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
		router, _, err := s.hub.routerFor("any")
		if err != nil {
			return "", err
		}
		s.replacement = bindStartStopRecorder(router, startedSessionResult("sess-replacement"))
	}
	return s.fakeBindingStore.RecordSessionBinding(ctx, sessionID, accountID, runnerID)
}

// TestStartResumePassesTheResumeIDThroughUnchanged: the rollback arm must not
// have disturbed the resume pass-through. The public request is relayed VERBATIM
// — resume_session_id included — and the Runner's minted id (which for a resume
// IS the logical id) comes back as the response session id.
func TestStartResumePassesTheResumeIDThroughUnchanged(t *testing.T) {
	const resumeID = "sess-logical"
	hub := newHubOnly()
	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	router, _, err := hub.routerFor("any")
	if err != nil {
		t.Fatalf("routerFor after enroll = %v, want a router", err)
	}
	var relayedResumeID, relayedBody string
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		relayedResumeID = cmd.GetStart().GetResumeSessionId()
		relayedBody = cmd.GetResumeBody().GetSessionBody()
		go router.complete(&compassv1internal.SessionsRequest{
			RequestId: cmd.GetRequestId(),
			Result:    &compassv1internal.SessionsRequest_Start{Start: &compassv1.StartAgentSessionResponse{SessionId: resumeID}},
		})
		return nil
	})

	resp, err := hub.StartResume(context.Background(), "req-ok",
		&compassv1.StartAgentSessionRequest{ContainerName: "c1", ResumeSessionId: resumeID},
		[]byte("transcript body"))
	if err != nil {
		t.Fatalf("StartResume = %v, want success", err)
	}
	if relayedResumeID != resumeID {
		t.Fatalf("relayed resume_session_id = %q, want %q (the public request is relayed verbatim)", relayedResumeID, resumeID)
	}
	if relayedBody != "transcript body" {
		t.Fatalf("relayed resume body = %q, want the reconstructed transcript body", relayedBody)
	}
	if got := resp.GetSessionId(); got != resumeID {
		t.Fatalf("StartResume session id = %q, want %q (a resume reuses the logical id)", got, resumeID)
	}
}

// TestStartEmitsNoInitialSignal: the initial secret materialize is pre-exec on
// the Runner (host.Start, FetchSecretsByContainer), so a bound Start pushes NO
// SecretsVersion frame. Signalling here would drive a redundant second
// materialize over the T6 rotation path — the race Start was conformed away
// from. A non-zero count means the initial-signal path was re-introduced.
func TestStartEmitsNoInitialSignal(t *testing.T) {
	hub := newHubOnly()
	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("c1", testAgentAccount)
	router, _, _ := hub.routerFor("any")
	rec := newRecordingSend()
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		_ = rec.send(cmd)
		if cmd.GetStart() != nil {
			go router.complete(&compassv1internal.SessionsRequest{
				RequestId: cmd.GetRequestId(),
				Result:    &compassv1internal.SessionsRequest_Start{Start: &compassv1.StartAgentSessionResponse{SessionId: "sess-ok"}},
			})
		}
		return nil
	})

	if _, err := hub.Start(context.Background(), "req-1", &compassv1.StartAgentSessionRequest{ContainerName: "c1"}); err != nil {
		t.Fatalf("Start = %v, want success", err)
	}
	if pushed := secretsVersionsPushed(t, rec); len(pushed) != 0 {
		t.Fatalf("bound Start pushed %d SecretsVersion frames, want 0 (initial materialize is pre-exec)", len(pushed))
	}
}

// A successful Remove relay sends a RemoveAgentWorkspace command down the
// Sessions stream and returns the Runner's RemoveAgentWorkspaceResponse — the
// container-teardown counterpart to Provision. This drives the mapping through
// the real router (like the Start-relay happy path) so the wiring, not just the
// pure function, is proven: the command variant the Runner sees is the Remove
// variant, and the typed result flows back.
func TestRemoveRelayReturnsResponseOnSuccess(t *testing.T) {
	hub := newHubOnly()
	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	router, _, _ := hub.routerFor("any")
	var sawRemove bool
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		if cmd.GetRemove() != nil {
			sawRemove = true
		}
		go router.complete(&compassv1internal.SessionsRequest{
			RequestId: cmd.GetRequestId(),
			Result:    &compassv1internal.SessionsRequest_Remove{Remove: &compassv1.RemoveAgentWorkspaceResponse{}},
		})
		return nil
	})

	resp, err := hub.Remove(context.Background(), "req-rm", &compassv1.RemoveAgentWorkspaceRequest{ContainerName: "c1"})
	if err != nil {
		t.Fatalf("Remove = %v, want success", err)
	}
	if resp == nil {
		t.Fatal("Remove returned a nil response, want the Runner's RemoveAgentWorkspaceResponse")
	}
	if !sawRemove {
		t.Fatal("Runner did not receive a RemoveAgentWorkspace command variant")
	}
}

// Remove clears the container's provisioned account binding — the teardown
// counterpart to Provision's bindContainer. On a Provision->Remove path that
// never reached Start (promoteSession clears it there), a lingering binding would
// keep authorizing a pre-exec FetchSecrets materialize (AccountForContainer) for
// a container that no longer exists.
//
// Mutation: dropping the unbindContainer call in Remove leaves AccountForContainer
// true after teardown and reddens this.
func TestRemoveClearsContainerBinding(t *testing.T) {
	hub := newHubOnly()
	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("c1", testAgentAccount)
	if _, ok := hub.AccountForContainer("c1"); !ok {
		t.Fatal("precondition: container c1 should be bound after bindContainer")
	}
	router, _, _ := hub.routerFor("any")
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		go router.complete(&compassv1internal.SessionsRequest{
			RequestId: cmd.GetRequestId(),
			Result:    &compassv1internal.SessionsRequest_Remove{Remove: &compassv1.RemoveAgentWorkspaceResponse{}},
		})
		return nil
	})

	if _, err := hub.Remove(context.Background(), "req-rm", &compassv1.RemoveAgentWorkspaceRequest{ContainerName: "c1"}); err != nil {
		t.Fatalf("Remove = %v, want success", err)
	}
	if _, ok := hub.AccountForContainer("c1"); ok {
		t.Fatal("container c1 still bound after Remove, want the binding cleared (stale binding authorizes pre-exec secrets materialize)")
	}
}

// attachStatusResponder enrolls a Runner and binds its router to answer every
// GetAgentStatus command with a canned response carrying statuses, correlated by
// the pushed request id — the seam the SessionState fallback tests drive.
func attachStatusResponder(t *testing.T, hub *Hub, statuses []*compassv1.AgentSessionStatus) {
	t.Helper()
	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	router, _, err := hub.routerFor("any")
	if err != nil {
		t.Fatalf("routerFor after enroll = %v, want a router", err)
	}
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		go router.complete(&compassv1internal.SessionsRequest{
			RequestId: cmd.GetRequestId(),
			Result: &compassv1internal.SessionsRequest_Status{
				Status: &compassv1.GetAgentStatusResponse{Statuses: statuses},
			},
		})
		return nil
	})
}

// L2 (RIG-1569 T8 review): SessionState adopts the SOLE status as this session's
// state only when that status carries NO session id (the "Runner answered without
// echoing the id" case). A sole status echoing an EMPTY id resolves ok=true.
func TestSessionStateSoleStatusEmptyIDResolves(t *testing.T) {
	hub := newHubOnly()
	attachStatusResponder(t, hub, []*compassv1.AgentSessionStatus{
		{SessionId: "", State: compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING},
	})

	state, ok := hub.SessionState(context.Background(), "sess-1")
	if !ok {
		t.Fatal("SessionState with a sole empty-id status = ok=false, want ok=true (adopt the id-less answer)")
	}
	if state != compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING {
		t.Fatalf("SessionState = %v, want WORKING (the sole status's state)", state)
	}
}

// L2 (RIG-1569 T8 review): a sole status echoing a NON-EMPTY MISMATCHED id is NOT
// this session's state — a Runner bug echoing a wrong id must not reconstruct a
// wrong presence — so it is unresolved (ok=false → UNSPECIFIED). RED against
// pre-fix: the pre-fix fallback adopted the sole status unconditionally, so this
// returned ok=true with the wrong session's state.
func TestSessionStateSoleStatusMismatchedIDIsUnresolved(t *testing.T) {
	hub := newHubOnly()
	attachStatusResponder(t, hub, []*compassv1.AgentSessionStatus{
		{SessionId: "some-other-session", State: compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING},
	})

	state, ok := hub.SessionState(context.Background(), "sess-1")
	if ok {
		t.Fatalf("SessionState with a sole MISMATCHED-id status = ok=true (state %v), want ok=false — a wrong-id echo must not resolve this session", state)
	}
	if state != compassv1.AgentSessionState_AGENT_SESSION_STATE_UNSPECIFIED {
		t.Fatalf("SessionState unresolved state = %v, want UNSPECIFIED", state)
	}
}

// L2 companion: a status whose id MATCHES the requested session resolves ok=true
// through the id-match loop (the primary path, unchanged by the fix) — so the
// tightened fallback did not regress the matched case.
func TestSessionStateMatchedIDResolves(t *testing.T) {
	hub := newHubOnly()
	attachStatusResponder(t, hub, []*compassv1.AgentSessionStatus{
		{SessionId: "sess-1", State: compassv1.AgentSessionState_AGENT_SESSION_STATE_READY},
	})

	state, ok := hub.SessionState(context.Background(), "sess-1")
	if !ok || state != compassv1.AgentSessionState_AGENT_SESSION_STATE_READY {
		t.Fatalf("SessionState(sess-1) = %v ok=%v, want READY ok=true (id-match path)", state, ok)
	}
}
