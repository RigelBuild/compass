//go:build unix

package runnerhub

// The durable commit seam (#24 / OQ-3): Hub.CommitConversationFrame resolves the
// relayed session to its bound agent account (fail-closed, exactly as
// RelayCommsCall does) and commits one agent-authored conversation frame at most
// once, keyed on the agent-minted idempotency_key. Every test here defends one
// contract clause the downstream Runner depends on:
//
//   - an unbound/unknown session fails closed CodeNotFound (no live account,
//     never a stale one), and NEVER reaches the caller;
//   - a hub with no CommsCaller wired fails CodeUnavailable, checked BEFORE
//     resolution (a Deliver-only hub);
//   - a frame with neither conversation variant set is CodeInvalidArgument (the
//     terminal malformed-frame the Runner does not retry);
//   - a posted frame forwards under the bound account WITH the idempotency key
//     threaded, and the ack is committed=true carrying the committed row's id.
//
// White-box (package runnerhub) so the tests drive the unexported binding
// lifecycle and assert attribution through the fake CommsCaller, matching
// relay_comms_test.go. Sleep-free: the hub calls the caller inline.

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
)

// commitPostReq builds a CommitConversationFrameRequest carrying a posted
// conversation variant, under sessionID and the agent-minted idempotency key.
func commitPostReq(sessionID, idempotencyKey string, posted *compassv1.MessagePosted) *compassv1internal.CommitConversationFrameRequest {
	return &compassv1internal.CommitConversationFrameRequest{
		SessionId: sessionID,
		Frame: &compassv1internal.AgentFrame{
			Frame: &compassv1internal.AgentFrame_ConversationPosted{ConversationPosted: posted},
		},
		IdempotencyKey: idempotencyKey,
	}
}

// commitUnsetFrameReq builds a request whose AgentFrame has NO oneof variant set
// — the malformed frame the hub rejects CodeInvalidArgument.
func commitUnsetFrameReq(sessionID, idempotencyKey string) *compassv1internal.CommitConversationFrameRequest {
	return &compassv1internal.CommitConversationFrameRequest{
		SessionId:      sessionID,
		Frame:          &compassv1internal.AgentFrame{},
		IdempotencyKey: idempotencyKey,
	}
}

// 1. An unbound session fails closed CodeNotFound and NEVER reaches the caller —
// the same fail-closed guard RelayCommsCall enforces, for the durable path: a
// session_id selects an account from the hub's own binding, it never carries
// one, so an id the hub never bound resolves to nothing and no commit is
// attempted under any account.
//
// Mutation: hardcode accountForSession to return a fixed account (ok=true) and
// this fails twice over — the error goes nil and the caller records a commit.
func TestCommitConversationFrameUnboundSessionFailsClosedNotFound(t *testing.T) {
	hub, comms := newHubWithComms()

	_, err := hub.CommitConversationFrame(context.Background(), commitPostReq("never-bound", "key-1", postedTextFrame("hi")))
	if err == nil {
		t.Fatal("CommitConversationFrame for an unbound session = nil error, want CodeNotFound (fail closed)")
	}
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("unbound-session error code = %v, want NotFound", got)
	}
	if calls := comms.snapshot(); len(calls) != 0 {
		t.Fatalf("caller was invoked %d times for an unbound session, want 0 (no commit attempt)", len(calls))
	}
}

// 2. A hub with no CommsCaller wired fails CommitConversationFrame closed with
// CodeUnavailable — the comms leg is not mounted, never a silent success. Like
// RelayCommsCall, this is checked BEFORE session resolution, so even a bound
// session gets Unavailable on a Deliver-only hub.
func TestCommitConversationFrameNilCommsIsUnavailable(t *testing.T) {
	// A Deliver-only hub: comms is nil.
	hub := NewHub(&fakeConversationSink{}, &fakeLifecycleSink{}, &fakeTailSink{}, nil, discardLogger())
	bindLiveSession(hub)

	_, err := hub.CommitConversationFrame(context.Background(), commitPostReq("sess-1", "key-1", postedTextFrame("hi")))
	if err == nil {
		t.Fatal("CommitConversationFrame on a nil-comms hub = nil error, want CodeUnavailable")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("nil-comms error code = %v, want Unavailable", got)
	}
}

// 3. A frame with neither conversation_posted nor conversation_updated set is a
// malformed frame — CodeInvalidArgument, the TERMINAL class the Runner does not
// retry (a permanent per-frame refusal). The caller is never reached, and the
// error is a real Connect status error (never committed=false + nil, which the
// Runner would misread as a successful commit).
func TestCommitConversationFrameNeitherVariantIsInvalidArgument(t *testing.T) {
	hub, comms := newHubWithComms()
	bindLiveSession(hub)

	_, err := hub.CommitConversationFrame(context.Background(), commitUnsetFrameReq("sess-1", "key-1"))
	if err == nil {
		t.Fatal("CommitConversationFrame with an unset frame variant = nil error, want CodeInvalidArgument")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("unset-variant error code = %v, want InvalidArgument", got)
	}
	if calls := comms.snapshot(); len(calls) != 0 {
		t.Fatalf("caller was invoked %d times for a malformed frame, want 0", len(calls))
	}
}

// 4. The happy posted path forwards the frame under the BOUND account WITH the
// idempotency key threaded, and returns committed=true carrying the committed
// row's message_id. This pins the three things the durable contract depends on:
// attribution is the bound account (not the Runner's, not admin), the
// agent-minted key reaches the keyed commit (so the store can dedup a replay),
// and the ack reports committed=true + the row id (seq deferred to 0).
func TestCommitConversationFrameHappyPostForwardsKeyedUnderBoundAccount(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.commitPost = &compassv1.PostMessageResponse{Message: &compassv1.Message{Id: "m-42"}}
	bindLiveSession(hub)

	posted := postedTextFrame("the agent speaks")
	resp, err := hub.CommitConversationFrame(context.Background(), commitPostReq("sess-1", "idem-key-1", posted))
	if err != nil {
		t.Fatalf("CommitConversationFrame(post) = %v, want success", err)
	}

	calls := comms.snapshot()
	if len(calls) != 1 {
		t.Fatalf("caller invoked %d times, want exactly 1", len(calls))
	}
	if calls[0].account != "acct-agent" {
		t.Fatalf("caller attributed to %q, want the bound account acct-agent", calls[0].account)
	}
	if calls[0].commitPost != posted {
		t.Fatalf("caller received a different MessagePosted than the relayed one")
	}
	if calls[0].commitKey != "idem-key-1" {
		t.Fatalf("caller received idempotency key %q, want the relayed idem-key-1 (the key must thread to the keyed commit)", calls[0].commitKey)
	}
	if !resp.GetCommitted() {
		t.Fatal("ack committed = false, want true on a fresh commit")
	}
	if got := resp.GetMessageId(); got != "m-42" {
		t.Fatalf("ack message_id = %q, want the committed row id m-42", got)
	}
	if got := resp.GetSeq(); got != 0 {
		t.Fatalf("ack seq = %d, want 0 (deferred)", got)
	}
}

// 5. A comms-layer error is propagated AS-IS — the frame's Connect code (already
// mapped by edgeError) reaches the Runner unchanged, so the retryable/terminal
// split reads the right code, and it is NEVER swallowed into a committed=false
// nil-error ack. Here the caller returns CodeInvalidArgument (a malformed frame
// the comms layer rejected) and the hub surfaces exactly that code.
func TestCommitConversationFramePropagatesCommsErrorCode(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.commitErr = connect.NewError(connect.CodeInvalidArgument, errors.New("empty block set"))
	bindLiveSession(hub)

	resp, err := hub.CommitConversationFrame(context.Background(), commitPostReq("sess-1", "key-1", postedTextFrame("bad")))
	if err == nil {
		t.Fatal("CommitConversationFrame with a comms error = nil error, want the comms Connect code (never a committed=false nil-error ack)")
	}
	if resp != nil {
		t.Fatalf("response = %v on a comms error, want nil (a non-commit is a Connect error, never an ack)", resp)
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("propagated error code = %v, want InvalidArgument (the comms layer's own code, not CodeUnknown)", got)
	}
}

// postedTextFrame wraps a single text block in the MessagePosted frame shape the
// Runner relays on the durable path. Container/id are server-assigned, so the
// frame carries only blocks.
func postedTextFrame(text string) *compassv1.MessagePosted {
	return &compassv1.MessagePosted{Message: &compassv1.Message{
		Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: text}}},
	}}
}
