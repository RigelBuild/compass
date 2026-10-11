//go:build unix

package runnerhub

// The agent-comms Server leg (comms-tools design T2, OQ-2 ratified — the
// load-bearing security leg). Every test here defends one invariant of the
// session->account binding + RelayCommsCall handler: the Runner asserts no
// account, so the SERVER's binding is the sole authority for whose account a
// relayed call runs under. A regression that let an unbound, stopped, or
// reconnect-dropped session resolve to ANY account — or attributed a call to the
// wrong account, or turned a tool failure into a transport teardown — must
// redden a test below.
//
// White-box (package runnerhub) so the tests drive the unexported binding
// lifecycle (bindContainer/promoteSession/unbindSession/enroll) and the command
// path directly, while asserting the account attribution through the fake
// CommsCaller. Sleep-free: the hub calls the caller inline, so every assertion
// reads a synchronously-recorded fact.

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// relayPost builds a RelayCommsCallRequest carrying a post variant under callID.
func relayPost(sessionID, callID string, post *compassv1.PostMessageRequest) *compassv1internal.RelayCommsCallRequest {
	return &compassv1internal.RelayCommsCallRequest{
		SessionId: sessionID,
		Call: &compassv1internal.CommsCallRequest{
			CallId: callID,
			Call:   &compassv1internal.CommsCallRequest_Post{Post: post},
		},
	}
}

// relayList builds a RelayCommsCallRequest carrying a list variant under callID.
func relayList(sessionID, callID string, list *compassv1.ListMessagesRequest) *compassv1internal.RelayCommsCallRequest {
	return &compassv1internal.RelayCommsCallRequest{
		SessionId: sessionID,
		Call: &compassv1internal.CommsCallRequest{
			CallId: callID,
			Call:   &compassv1internal.CommsCallRequest_List{List: list},
		},
	}
}

func relayListTopics(sessionID, callID string, topics *compassv1.ListTopicsRequest) *compassv1internal.RelayCommsCallRequest {
	return &compassv1internal.RelayCommsCallRequest{
		SessionId: sessionID,
		Call: &compassv1internal.CommsCallRequest{
			CallId: callID,
			Call:   &compassv1internal.CommsCallRequest_ListTopics{ListTopics: topics},
		},
	}
}

// bindLiveSession binds the canonical session under test to its account through
// the real Provision->Start promotion path (bindContainer then promoteSession),
// the same two-step the command handlers drive. Every case binds the same
// (container, session, account) triple, so the values are fixed here and the
// test bodies refer to "sess-1"/"acct-agent" directly.
func bindLiveSession(hub *Hub) {
	const (
		containerName = "c1"
		sessionID     = "sess-1"
		account       = store.AccountID("acct-agent")
	)
	hub.enroll(context.Background(), testRunnerID, runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer(containerName, account, testRunnerID)
	hub.promoteSession(context.Background(), containerName, sessionID)
}

// A session owned by another Runner must read as unbound on every relay leg,
// with the unknown-session error verbatim so its liveness is not observable.
func TestRunnerRelayCallsRequireOwningRunner(t *testing.T) {
	cases := []struct {
		name string
		call func(*Hub, string) error
	}{
		{"comms", func(h *Hub, runnerID string) error {
			_, err := h.RelayCommsCall(context.Background(), runnerID, relayPost("sess-1", "tc-owner", &compassv1.PostMessageRequest{}))
			return err
		}},
		{"lifecycle", func(h *Hub, runnerID string) error {
			_, err := h.RelayLifecycleCall(context.Background(), runnerID, relaySpawn("sess-1", "lc-owner", &compassv1internal.SpawnPeerRequest{Handle: "peer"}))
			return err
		}},
		{"board", func(h *Hub, runnerID string) error {
			_, err := h.RelayBoardCall(context.Background(), runnerID, relaySetIssueState("sess-1", "bc-owner", &compassv1internal.SetIssueStateRequest{IssueId: "iss-1", State: compassv1.IssueState_ISSUE_STATE_TODO}))
			return err
		}},
		{"forge", func(h *Hub, runnerID string) error {
			_, err := h.RelayForgeCall(context.Background(), runnerID, relayCreateIssue("sess-1", "fc-owner", &compassv1internal.CreateIssueRequest{Repo: "o/r", Title: "t"}))
			return err
		}},
		{"transcript", func(h *Hub, runnerID string) error {
			_, err := h.CommitConversationFrame(context.Background(), runnerID, transcriptReq("sess-1", "frame-owner", 1, false, `{"e":1}`))
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub, _ := newHubWithComms()
			hub.SetLifecycleCaller(&fakeLifecycleCaller{})
			hub.SetBoardCaller(&fakeBoardCaller{})
			hub.SetForgeCaller(&fakeForgeCaller{})
			hub.SetTranscriptStore(&fakeTranscriptStore{})
			bindLiveSession(hub)
			foreignErr := tc.call(hub, "runner-2")
			ownerErr := tc.call(hub, testRunnerID)
			if foreignErr == nil || connect.CodeOf(foreignErr) != connect.CodeFailedPrecondition {
				t.Fatalf("foreign Runner error = %v, want FailedPrecondition", foreignErr)
			}
			if got := connect.CodeOf(ownerErr); got == connect.CodeFailedPrecondition {
				t.Fatalf("owning Runner call returned FailedPrecondition: %v", ownerErr)
			}
		})
	}
}

func TestRunnerRelayBindingErrorContractAcrossLegs(t *testing.T) {
	cases := []struct {
		name string
		call func(*Hub, string, string) error
	}{
		{"comms", func(h *Hub, runnerID, sessionID string) error {
			_, err := h.RelayCommsCall(context.Background(), runnerID, relayPost(sessionID, "tc-bind", &compassv1.PostMessageRequest{}))
			return err
		}},
		{"lifecycle", func(h *Hub, runnerID, sessionID string) error {
			_, err := h.RelayLifecycleCall(context.Background(), runnerID, relaySpawn(sessionID, "lc-bind", &compassv1internal.SpawnPeerRequest{Handle: "peer"}))
			return err
		}},
		{"board", func(h *Hub, runnerID, sessionID string) error {
			_, err := h.RelayBoardCall(context.Background(), runnerID, relaySetIssueState(sessionID, "bc-bind", &compassv1internal.SetIssueStateRequest{IssueId: "iss-1", State: compassv1.IssueState_ISSUE_STATE_TODO}))
			return err
		}},
		{"forge", func(h *Hub, runnerID, sessionID string) error {
			_, err := h.RelayForgeCall(context.Background(), runnerID, relayCreateIssue(sessionID, "fc-bind", &compassv1internal.CreateIssueRequest{Repo: "o/r", Title: "t"}))
			return err
		}},
		{"transcript", func(h *Hub, runnerID, sessionID string) error {
			_, err := h.CommitConversationFrame(context.Background(), runnerID, transcriptReq(sessionID, "frame-bind", 1, false, `{"e":1}`))
			return err
		}},
		{"session blob", func(h *Hub, runnerID, sessionID string) error {
			blob := []byte("blob")
			_, err := h.RelaySessionBlob(context.Background(), runnerID, &compassv1internal.RelaySessionBlobRequest{
				SessionId: sessionID,
				Blob:      &compassv1internal.PutSessionBlobRequest{Sha256: blobHash(blob), Data: blob},
			})
			return err
		}},
	}

	newHub := func() (*Hub, func() int) {
		comms := &fakeCommsCaller{}
		lifecycle := &fakeLifecycleCaller{}
		board := &fakeBoardCaller{}
		forge := &fakeForgeCaller{}
		transcripts := &fakeTranscriptStore{}
		blobs := &fakeSessionBlobStore{}
		hub := NewHub(&fakeLifecycleSink{}, &fakeTailSink{}, comms, discardLogger())
		hub.SetLifecycleCaller(lifecycle)
		hub.SetBoardCaller(board)
		hub.SetForgeCaller(forge)
		hub.SetTranscriptStore(transcripts)
		hub.SetSessionBlobStore(blobs)
		return hub, func() int {
			return len(comms.snapshot()) + len(lifecycle.snapshot()) + len(board.snapshot()) +
				len(forge.snapshot()) + len(transcripts.snapshot()) + blobs.putCalls
		}
	}

	messages := map[string][]string{"unknown": {}, "foreign": {}, "tenant conflict": {}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			check := func(class string, hub *Hub, calls func() int, runnerID, sessionID, wantMessage string) string {
				err := tc.call(hub, runnerID, sessionID)
				if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
					t.Fatalf("%s binding error = %v, want FailedPrecondition", class, err)
				}
				if got := err.Error(); wantMessage != "" && got != wantMessage {
					t.Fatalf("%s binding message = %q, want %q", class, got, wantMessage)
				}
				messages[class] = append(messages[class], err.Error())
				if n := calls(); n != 0 {
					t.Fatalf("%s binding delegated %d times, want 0", class, n)
				}
				return err.Error()
			}

			hub, calls := newHub()
			bindLiveSession(hub)
			baseline := check("unknown", hub, calls, testRunnerID, "never-bound", "")
			check("foreign", hub, calls, "runner-2", "sess-1", baseline)

			hub, calls = newHub()
			bindings := newFakeBindingStore()
			bindings.tenantErr = store.ErrConflict
			hub.SetSessionBindingStore(bindings)
			bindLiveSession(hub)
			check("tenant conflict", hub, calls, testRunnerID, "sess-1", baseline)
		})
	}
	var wantMessage string
	for class, got := range messages {
		if len(got) != len(cases) {
			t.Fatalf("%s errors = %d, want one from each of %d relay legs", class, len(got), len(cases))
		}
		for _, message := range got {
			if wantMessage == "" {
				wantMessage = message
			}
			if message != wantMessage {
				t.Fatalf("%s binding message %q differs from shared message %q", class, message, wantMessage)
			}
		}
	}
}

// An unknown session fails generically before the CommsCaller is reached.
func TestRelayCommsCallUnknownSessionFailsClosed(t *testing.T) {
	hub, comms := newHubWithComms()

	_, err := hub.RelayCommsCall(context.Background(), testRunnerID, relayPost("never-bound", "tc-1", &compassv1.PostMessageRequest{
		Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "hi"}}},
	}))
	if err == nil {
		t.Fatal("RelayCommsCall for an unbound session = nil error, want CodeFailedPrecondition")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("unbound-session error code = %v, want FailedPrecondition", got)
	}
	if calls := comms.snapshot(); len(calls) != 0 {
		t.Fatalf("caller was invoked %d times for an unbound session, want 0 (no attribution attempt)", len(calls))
	}
}

// 2. The happy post path forwards the exact request under the bound account and
// stamps the request's call_id onto the response. This pins the two things the
// transport (T3) depends on: attribution is the bound account (not the Runner's,
// not admin), and the call_id round-trips so the agent correlates its call.
func TestRelayCommsCallHappyPostForwardsUnderBoundAccountAndStampsCallID(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.postResp = &compassv1.PostMessageResponse{Message: &compassv1.Message{Id: "m-1"}}
	bindLiveSession(hub)

	req := &compassv1.PostMessageRequest{
		Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: "chan-1"},
		Blocks:    []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "hello"}}},
	}
	resp, err := hub.RelayCommsCall(context.Background(), testRunnerID, relayPost("sess-1", "tc-1", req))
	if err != nil {
		t.Fatalf("RelayCommsCall(post) = %v, want success", err)
	}

	calls := comms.snapshot()
	if len(calls) != 1 {
		t.Fatalf("caller invoked %d times, want exactly 1", len(calls))
	}
	if calls[0].account != "acct-agent" {
		t.Fatalf("caller attributed to %q, want the bound account acct-agent", calls[0].account)
	}
	if calls[0].post != req {
		t.Fatalf("caller received a different PostMessageRequest than the relayed one")
	}
	if got := resp.GetResult().GetCallId(); got != "tc-1" {
		t.Fatalf("response call_id = %q, want the request's tc-1", got)
	}
	if resp.GetResult().GetPost() != comms.postResp {
		t.Fatalf("response post result is not the caller's response")
	}
}

// 3. The happy list path mirrors 2: the exact list request forwards under the
// bound account and the call_id is stamped onto the list result.
func TestRelayCommsCallHappyListForwardsUnderBoundAccountAndStampsCallID(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.listResp = &compassv1.ListMessagesResponse{Messages: []*compassv1.Message{{Id: "m-1"}}}
	bindLiveSession(hub)

	req := &compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: "chan-1"},
	}
	resp, err := hub.RelayCommsCall(context.Background(), testRunnerID, relayList("sess-1", "tc-2", req))
	if err != nil {
		t.Fatalf("RelayCommsCall(list) = %v, want success", err)
	}

	calls := comms.snapshot()
	if len(calls) != 1 {
		t.Fatalf("caller invoked %d times, want exactly 1", len(calls))
	}
	if calls[0].account != "acct-agent" {
		t.Fatalf("caller attributed to %q, want the bound account acct-agent", calls[0].account)
	}
	if calls[0].list != req {
		t.Fatalf("caller received a different ListMessagesRequest than the relayed one")
	}
	if got := resp.GetResult().GetCallId(); got != "tc-2" {
		t.Fatalf("response call_id = %q, want the request's tc-2", got)
	}
	if resp.GetResult().GetList() != comms.listResp {
		t.Fatalf("response list result is not the caller's response")
	}
}

func TestRelayCommsCallHappyListTopicsForwardsUnderBoundAccount(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.topicListResp = &compassv1.ListTopicsResponse{Topics: []*compassv1.Topic{{Id: "topic-1"}}}
	bindLiveSession(hub)
	req := &compassv1.ListTopicsRequest{ChannelId: "war-room", IncludeArchived: true}
	resp, err := hub.RelayCommsCall(context.Background(), testRunnerID, relayListTopics("sess-1", "tc-topics", req))
	if err != nil {
		t.Fatalf("RelayCommsCall(list_topics): %v", err)
	}
	calls := comms.snapshot()
	if len(calls) != 1 || calls[0].account != "acct-agent" || calls[0].topicList != req {
		t.Fatalf("topic-list calls = %+v, want the exact request under the bound account", calls)
	}
	if got := resp.GetResult().GetCallId(); got != "tc-topics" {
		t.Fatalf("response call_id = %q, want tc-topics", got)
	}
	if resp.GetResult().GetListTopics() != comms.topicListResp {
		t.Fatal("response list_topics result is not the caller's response")
	}
}

// 3b. A list caller failure is rendered as a CommsCallError while RelayCommsCall
// itself remains successful and preserves the request call_id.
//
// Mutation: dropping caller error handling makes the in-band list error absent,
// so this test fails.
func TestRelayCommsCallListToolErrorIsInBandNotStreamError(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.listErr = connect.NewError(connect.CodePermissionDenied, errors.New("list denied"))
	bindLiveSession(hub)

	resp, err := hub.RelayCommsCall(context.Background(), testRunnerID, relayList("sess-1", "tc-list-err", &compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: "chan-1"},
	}))
	if err != nil {
		t.Fatalf("RelayCommsCall returned a stream error %v, want in-band list error", err)
	}
	toolErr := resp.GetResult().GetError()
	if toolErr == nil {
		t.Fatal("response has no in-band CommsCallError, want the list failure rendered in-band")
	}
	if got := toolErr.GetCode(); got != connect.CodePermissionDenied.String() {
		t.Fatalf("in-band error code = %q, want %q", got, connect.CodePermissionDenied.String())
	}
	if got := resp.GetResult().GetCallId(); got != "tc-list-err" {
		t.Fatalf("response call_id = %q, want tc-list-err", got)
	}
}

// 4. A tool-level failure is rendered IN-BAND as a CommsCallError, not as a
// Connect stream error: the agent gets a renderable error and the transport
// survives. This is the "tool failure != transport teardown" invariant. A
// non-member channel (CodeNotFound from the caller) collapses to the in-band
// code token "not_found" — the D9 answer a human also gets — with the Go error
// nil so the RelayCommsCall stream is not torn down.
func TestRelayCommsCallToolErrorIsInBandNotStreamError(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.postErr = connect.NewError(connect.CodeNotFound, errors.New("channel not found"))
	bindLiveSession(hub)

	resp, err := hub.RelayCommsCall(context.Background(), testRunnerID, relayPost("sess-1", "tc-3", &compassv1.PostMessageRequest{
		Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "hi"}}},
	}))
	if err != nil {
		t.Fatalf("RelayCommsCall with a tool error returned a Go error %v, want nil (in-band render)", err)
	}
	toolErr := resp.GetResult().GetError()
	if toolErr == nil {
		t.Fatal("response has no in-band CommsCallError, want the tool failure rendered in-band")
	}
	if toolErr.GetCode() != "not_found" {
		t.Fatalf("in-band error code = %q, want not_found (the D9 collapse token)", toolErr.GetCode())
	}
	// commsCallError sets Message = err.Error(); a connect error renders as
	// "<code>: <message>", so the code token prefixes the cause here.
	if toolErr.GetMessage() != "not_found: channel not found" {
		t.Fatalf("in-band error message = %q, want the caller's rendered error", toolErr.GetMessage())
	}
	// The call_id still round-trips on the error variant so the agent correlates
	// the failed call.
	if got := resp.GetResult().GetCallId(); got != "tc-3" {
		t.Fatalf("in-band error call_id = %q, want tc-3", got)
	}
}

// 5. A hub with no CommsCaller wired fails RelayCommsCall closed with
// CodeUnavailable — the comms leg is not mounted, never a silent success. This
// is checked BEFORE session resolution, so even a bound session gets Unavailable
// on a Deliver-only hub.
func TestRelayCommsCallNilCommsIsUnavailable(t *testing.T) {
	hub := newHubOnly() // comms == nil
	// Bind a session anyway to prove the nil-comms guard precedes resolution.
	bindLiveSession(hub)

	_, err := hub.RelayCommsCall(context.Background(), testRunnerID, relayPost("sess-1", "tc-4", &compassv1.PostMessageRequest{
		Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "hi"}}},
	}))
	if err == nil {
		t.Fatal("RelayCommsCall on a nil-comms hub = nil error, want CodeUnavailable")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("nil-comms error code = %v, want Unavailable", got)
	}
}

// 6. A call whose post/list oneof is unset is an invalid request. executeCall's
// default arm returns a Connect CodeInvalidArgument error, which RelayCommsCall
// renders IN-BAND (a non-resolution error is always in-band) — so the Go error
// is nil and result.error.code == "invalid_argument". Pinning the OBSERVED
// behavior (in-band, not a Connect error) per the handoff.
func TestRelayCommsCallUnsetOneofIsInBandInvalidArgument(t *testing.T) {
	hub, comms := newHubWithComms()
	bindLiveSession(hub)

	resp, err := hub.RelayCommsCall(context.Background(), testRunnerID, &compassv1internal.RelayCommsCallRequest{
		SessionId: "sess-1",
		Call:      &compassv1internal.CommsCallRequest{CallId: "tc-5"}, // no post/list variant set
	})
	if err != nil {
		t.Fatalf("RelayCommsCall with an unset oneof returned a Go error %v, want nil (in-band render)", err)
	}
	toolErr := resp.GetResult().GetError()
	if toolErr == nil {
		t.Fatal("response has no in-band CommsCallError for an unset oneof, want invalid_argument in-band")
	}
	if toolErr.GetCode() != "invalid_argument" {
		t.Fatalf("in-band error code = %q, want invalid_argument", toolErr.GetCode())
	}
	if got := resp.GetResult().GetCallId(); got != "tc-5" {
		t.Fatalf("in-band error call_id = %q, want tc-5", got)
	}
	// An invalid call never reaches the caller.
	if calls := comms.snapshot(); len(calls) != 0 {
		t.Fatalf("caller invoked %d times for an unset oneof, want 0", len(calls))
	}
}

// 7. Drop-on-reconnect (OQ-2) — THE security test. A bound session serves a
// call; then the Runner re-enrolls (reconnect). Because session_id /
// container_name are Runner-minted, a restarted Runner could re-mint an id still
// bound to the OLD account; enroll clears every binding so the re-minted id
// resolves to the generic FailedPrecondition until bound anew, never a stale account.
//
// Mutation: remove the two clear(...) lines in enroll and this test fails — the
// post-reconnect call resolves the stale account and succeeds instead of failing closed.
func TestRelayCommsCallDropsBindingOnRunnerReconnect(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.postResp = &compassv1.PostMessageResponse{Message: &compassv1.Message{Id: "m-1"}}
	bindLiveSession(hub)

	// Pre-reconnect: the bound session serves the call under its account.
	if _, err := hub.RelayCommsCall(context.Background(), testRunnerID, relayPost("sess-1", "tc-6", &compassv1.PostMessageRequest{
		Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "before"}}},
	})); err != nil {
		t.Fatalf("pre-reconnect RelayCommsCall = %v, want success", err)
	}

	// The Runner reconnects (re-enroll), which drops ALL agent-comms bindings.
	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

	// The SAME session_id now fails closed — the binding is gone, so no stale
	// account is reachable.
	_, err := hub.RelayCommsCall(context.Background(), testRunnerID, relayPost("sess-1", "tc-7", &compassv1.PostMessageRequest{
		Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "after"}}},
	}))
	if err == nil {
		t.Fatal("post-reconnect RelayCommsCall = nil error, want CodeFailedPrecondition (binding must drop on reconnect)")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("post-reconnect error code = %v, want FailedPrecondition", got)
	}
	// Exactly one call ever reached the caller — the pre-reconnect one. The
	// post-reconnect call never attributed to anyone.
	if calls := comms.snapshot(); len(calls) != 1 {
		t.Fatalf("caller invoked %d times total, want exactly 1 (only the pre-reconnect call)", len(calls))
	}
}

// 8. Stop unbinds: a session's account binding is dropped by unbindSession (the
// Stop path), so a later RelayCommsCall for the stopped id fails closed with the
// same generic FailedPrecondition as a never-seen session, never a stale reuse.
// Driven explicitly through the stop path so it is distinct from case 1.
func TestRelayCommsCallStoppedSessionFailsClosed(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.postResp = &compassv1.PostMessageResponse{Message: &compassv1.Message{Id: "m-1"}}
	bindLiveSession(hub)
	if _, err := hub.RelayCommsCall(context.Background(), testRunnerID, relayPost("sess-1", "tc-8", &compassv1.PostMessageRequest{})); err != nil {
		t.Fatalf("pre-stop RelayCommsCall = %v, want success", err)
	}
	hub.unbindSession(context.Background(), "sess-1")
	_, err := hub.RelayCommsCall(context.Background(), testRunnerID, relayPost("sess-1", "tc-9", &compassv1.PostMessageRequest{}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stopped-session error = %v, want FailedPrecondition", err)
	}
	if calls := comms.snapshot(); len(calls) != 1 {
		t.Fatalf("caller invoked %d times total, want exactly 1", len(calls))
	}
}

// 9. Provision->Start through the real command path binds the minted session to
// the account id passed to Hub.Provision, so accountForSession resolves it.
// Driven through Hub.Provision and Hub.Start (with a fake Runner returning canned
// container/session ids) so the binding is proven end to end, not just via the
// helper.
func TestProvisionThenStartBindsSessionToProvisionedAccount(t *testing.T) {
	hub := newHubOnly()
	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	router, _, err := hub.routerFor("any")
	if err != nil {
		t.Fatalf("routerFor after enroll = %v, want a router", err)
	}
	// The fake Runner answers a Provision with a container name and a Start with
	// a session id, correlated by the pushed request id.
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		var result *compassv1internal.SessionsRequest
		switch cmd.GetCommand().(type) {
		case *compassv1internal.SessionsResponse_Provision:
			result = &compassv1internal.SessionsRequest{
				RequestId: cmd.GetRequestId(),
				Result:    &compassv1internal.SessionsRequest_Provision{Provision: &compassv1.ProvisionAgentWorkspaceResponse{ContainerName: "cont-1"}},
			}
		case *compassv1internal.SessionsResponse_Start:
			result = &compassv1internal.SessionsRequest{
				RequestId: cmd.GetRequestId(),
				Result:    &compassv1internal.SessionsRequest_Start{Start: &compassv1.StartAgentSessionResponse{SessionId: "sess-live"}},
			}
		}
		go router.complete(result)
		return nil
	})

	ctx := context.Background()
	if _, _, err := hub.Provision(ctx, "req-prov", "0123456789abcdef0123456789abcdef", &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "matt/ada"}); err != nil {
		t.Fatalf("Provision = %v, want success", err)
	}
	if _, err := hub.Start(ctx, "req-start", &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}); err != nil {
		t.Fatalf("Start = %v, want success", err)
	}

	account, ok := hub.accountForSession(context.Background(), "sess-live")
	if !ok {
		t.Fatal("accountForSession(sess-live) = not bound, want the provisioned account after Provision->Start")
	}
	if account != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("session bound to %q, want the provisioned account id 0123456789abcdef0123456789abcdef", account)
	}
}

// RelayCommsCall fails closed with FailedPrecondition, never an empty-account
// attribution.
func TestProvisionWithEmptyAccountLeavesNoBindingAndFailsClosed(t *testing.T) {
	hub, comms := newHubWithComms()
	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	router, _, err := hub.routerFor("any")
	if err != nil {
		t.Fatalf("routerFor after enroll = %v, want a router", err)
	}
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		var result *compassv1internal.SessionsRequest
		switch cmd.GetCommand().(type) {
		case *compassv1internal.SessionsResponse_Provision:
			result = &compassv1internal.SessionsRequest{
				RequestId: cmd.GetRequestId(),
				Result:    &compassv1internal.SessionsRequest_Provision{Provision: &compassv1.ProvisionAgentWorkspaceResponse{ContainerName: "cont-1"}},
			}
		case *compassv1internal.SessionsResponse_Start:
			result = &compassv1internal.SessionsRequest{
				RequestId: cmd.GetRequestId(),
				Result:    &compassv1internal.SessionsRequest_Start{Start: &compassv1.StartAgentSessionResponse{SessionId: "sess-live"}},
			}
		}
		go router.complete(result)
		return nil
	})

	ctx := context.Background()
	if _, _, err := hub.Provision(ctx, "req-prov", "", &compassv1.ProvisionAgentWorkspaceRequest{}); err != nil {
		t.Fatalf("Provision (empty account) = %v, want success", err)
	}
	if _, err := hub.Start(ctx, "req-start", &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}); err != nil {
		t.Fatalf("Start = %v, want success", err)
	}

	if _, ok := hub.accountForSession(context.Background(), "sess-live"); ok {
		t.Fatal("accountForSession(sess-live) resolved a binding, want none (empty account must not bind)")
	}
	// An empty-account provision leaves this session unbound.
	_, callErr := hub.RelayCommsCall(ctx, testRunnerID, relayPost("sess-live", "tc-10", &compassv1.PostMessageRequest{
		Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "hi"}}},
	}))
	if callErr == nil || connect.CodeOf(callErr) != connect.CodeFailedPrecondition {
		t.Fatalf("empty-account session error = %v, want FailedPrecondition", callErr)
	}
	if calls := comms.snapshot(); len(calls) != 0 {
		t.Fatalf("caller invoked %d times for an empty-account session, want 0", len(calls))
	}
}

// FIX 2 (RIG-1569 T3 review): enroll() must clear the reverse accountSessions
// map alongside sessionAccounts. The accountSessions doc (hub.go) states the
// reverse map is "maintained wherever sessionAccounts is so the two never
// drift: promoteSession adds, unbindSession removes, enroll clears" — the
// pre-fix code omitted the enroll clear, so a Runner re-enroll emptied
// sessionAccounts while accountSessions retained the dead Runner's stale
// account->session entry. The delivery consumer then resolved a DEAD session as
// live (SessionForAccount returns stale; LiveAgentSessions hands dead sessions
// to the sweep), breaking design.md:177-178. This binds an account->session,
// re-enrolls, and asserts the reverse map is empty.
func TestEnrollClearsReverseAccountSessions(t *testing.T) {
	hub := newHubOnly()
	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	bindLiveSession(hub) // acct-agent -> sess-1, via the real Provision->Start path

	// Sanity: the reverse map is populated before the re-enroll.
	if _, ok := hub.SessionForAccount(context.Background(), "acct-agent"); !ok {
		t.Fatal("SessionForAccount(acct-agent) not bound before re-enroll; the test setup is wrong")
	}

	// A Runner reconnect: enroll re-attaches and MUST drop every stale binding,
	// forward AND reverse.
	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

	if sess, ok := hub.SessionForAccount(context.Background(), "acct-agent"); ok {
		t.Fatalf("SessionForAccount(acct-agent) = %q, ok=true after re-enroll; want ok=false — enroll left a stale reverse entry, so a dead session resolves as live", sess)
	}
	if live := hub.LiveAgentSessions(); len(live) != 0 {
		t.Fatalf("LiveAgentSessions = %v after re-enroll, want empty — the sweep would hand dead sessions a deliver", live)
	}
}

// M1 (RIG-1569 T8 review): unbindSession (the clean-Stop teardown path) fires
// exactly one terminal (DISCONNECTED → OFFLINE) presence edge for the account
// whose live session was torn down. Pre-fix the hub fired the presence edge only
// at deliverSession while the session was still bound, so a Stop left the
// account's presence stuck at its last live state forever. This binds a session,
// unbinds it, and asserts one DISCONNECTED edge for that account.
func TestUnbindSessionFiresTerminalPresenceEdge(t *testing.T) {
	hub := newHubOnly()
	pres := &fakePresenceSink{}
	hub.SetPresenceSink(pres)
	hub.bindContainer("c1", "acct-a", "runner-1")
	hub.promoteSession(context.Background(), "c1", "sess-a") // fires one promoted edge, not a lifecycle one

	hub.unbindSession(context.Background(), "sess-a")

	life := pres.lifecycleSnapshot()
	if len(life) != 1 {
		t.Fatalf("lifecycle edges after unbind = %d, want 1 (the terminal OFFLINE edge): %+v", len(life), life)
	}
	if life[0].account != "acct-a" || life[0].sessionID != "sess-a" ||
		life[0].state != compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED {
		t.Fatalf("terminal edge = %+v, want {acct-a, sess-a, DISCONNECTED}", life[0])
	}
}

// M1 (RIG-1569 T8 review): unbindSession must NOT fire a terminal edge when the
// account was already re-pointed to a NEWER session — the account is not offline,
// its newer session is live. This binds acct-a to sess-old, re-points it to
// sess-new via a second promoteSession, then unbinds the STALE sess-old; the
// stale unbind must not delete the live reverse entry nor drive OFFLINE.
func TestUnbindStaleSessionFiresNoTerminalEdgeWhenRepointed(t *testing.T) {
	hub := newHubOnly()
	pres := &fakePresenceSink{}
	hub.SetPresenceSink(pres)
	hub.bindContainer("c1", "acct-a", "runner-1")
	hub.promoteSession(context.Background(), "c1", "sess-old")
	// A new container/session promotes onto the SAME account, re-pointing the
	// reverse entry to sess-new (the newer live session).
	hub.bindContainer("c2", "acct-a", "runner-1")
	hub.promoteSession(context.Background(), "c2", "sess-new")

	// Unbind the stale session: its forward entry is dropped, but the reverse
	// entry now points at sess-new, so no terminal edge fires.
	hub.unbindSession(context.Background(), "sess-old")

	if life := pres.lifecycleSnapshot(); len(life) != 0 {
		t.Fatalf("lifecycle edges after stale unbind = %d, want 0 (account re-pointed, not offline): %+v", len(life), life)
	}
	// The live session's reverse binding survives the stale unbind.
	if sess, ok := hub.SessionForAccount(context.Background(), "acct-a"); !ok || sess != "sess-new" {
		t.Fatalf("SessionForAccount(acct-a) = %q ok=%v after stale unbind, want sess-new (live binding must survive)", sess, ok)
	}
}

// M1 (RIG-1569 T8 review): enroll (the Runner-reconnect teardown path) fires one
// terminal (DISCONNECTED → OFFLINE) presence edge per PREVIOUSLY-bound account
// before clearing every binding, and the maps are cleared. Pre-fix enroll emitted
// no lifecycle frames at all, so a reconnect left every agent's presence stuck at
// its last live state. This binds two accounts, re-enrolls, and asserts one edge
// per account plus empty maps.
func TestEnrollFiresTerminalPresenceEdgePerBoundAccountAndClears(t *testing.T) {
	hub := newHubOnly()
	pres := &fakePresenceSink{}
	hub.SetPresenceSink(pres)
	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("c1", "acct-a", "runner-1")
	hub.promoteSession(context.Background(), "c1", "sess-a")
	hub.bindContainer("c2", "acct-b", "runner-1")
	hub.promoteSession(context.Background(), "c2", "sess-b")

	// A Runner reconnect: enroll drops every binding and drives each previously-
	// bound account OFFLINE.
	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

	life := pres.lifecycleSnapshot()
	if len(life) != 2 {
		t.Fatalf("lifecycle edges after re-enroll = %d, want 2 (one OFFLINE per bound account): %+v", len(life), life)
	}
	got := map[store.AccountID]compassv1.AgentSessionState{}
	for _, r := range life {
		if r.state != compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED {
			t.Fatalf("edge for %s = %v, want DISCONNECTED", r.account, r.state)
		}
		got[r.account] = r.state
	}
	if _, ok := got["acct-a"]; !ok {
		t.Fatalf("no terminal edge for acct-a: %+v", life)
	}
	if _, ok := got["acct-b"]; !ok {
		t.Fatalf("no terminal edge for acct-b: %+v", life)
	}
	// The maps are cleared: no account resolves a live session after re-enroll.
	if live := hub.LiveAgentSessions(); len(live) != 0 {
		t.Fatalf("LiveAgentSessions = %v after re-enroll, want empty", live)
	}
}

// M1 (RIG-1569 T8 review): a first-ever enroll (no prior bindings) fires no
// terminal presence edge — there is nothing bound to drive offline.
func TestFirstEnrollFiresNoTerminalPresenceEdge(t *testing.T) {
	hub := newHubOnly()
	pres := &fakePresenceSink{}
	hub.SetPresenceSink(pres)

	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

	if life := pres.lifecycleSnapshot(); len(life) != 0 {
		t.Fatalf("lifecycle edges after first enroll = %d, want 0 (nothing was bound): %+v", len(life), life)
	}
}
