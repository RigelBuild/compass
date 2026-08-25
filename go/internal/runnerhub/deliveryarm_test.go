//go:build unix

package runnerhub

// The RunnerHub's SEA-1569 T3 arms: the send-only DispatchControl relay (§5, the
// crux: a successful deliver must NOT block on a result), the delivery_ack cursor
// arm (§6), and the settle-edge sink fired at deliverSession (§2). Each test pins
// the observable contract a plausible regression would break, with a fake
// DeliveryStore / settle recorder mirroring the seam fakes in helpers_test.go.

import (
	"context"
	"errors"
	"sync"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// ackCall is one recorded AckDelivery: the (agent, channel, message) the arm
// resolved and advanced the cursor for.
type ackCall struct {
	agent     store.AccountID
	channel   store.ChannelID
	messageID string
}

// fakeDeliveryStore records the ack arm's channel resolution + cursor advance.
// channels maps message id -> channel (the ack-arm resolution); an id absent
// from it resolves ErrNotFound (a foreign/fabricated ack). Concurrency-safe for
// parity with the real store.
type fakeDeliveryStore struct {
	mu       sync.Mutex
	channels map[string]store.ChannelID
	acks     []ackCall
	ackErr   error // if set, AckDelivery returns it (a store fault)
}

func newFakeDeliveryStore() *fakeDeliveryStore {
	return &fakeDeliveryStore{channels: map[string]store.ChannelID{}}
}

func (f *fakeDeliveryStore) MessageChannel(_ context.Context, messageID string) (store.ChannelID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.channels[messageID]
	if !ok {
		return "", store.ErrNotFound
	}
	return ch, nil
}

func (f *fakeDeliveryStore) AckDelivery(_ context.Context, agent store.AccountID, channel store.ChannelID, messageID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ackErr != nil {
		return f.ackErr
	}
	f.acks = append(f.acks, ackCall{agent: agent, channel: channel, messageID: messageID})
	return nil
}

func (f *fakeDeliveryStore) ackSnapshot() []ackCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ackCall, len(f.acks))
	copy(out, f.acks)
	return out
}

// settleRecord is one recorded settle edge.
type settleRecord struct {
	sessionID string
	state     compassv1.AgentSessionState
}

// fakeSettleSink records OnSessionSettled calls — the hub's settle-edge sink.
type fakeSettleSink struct {
	mu      sync.Mutex
	settles []settleRecord
}

func (f *fakeSettleSink) OnSessionSettled(sessionID string, state compassv1.AgentSessionState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settles = append(f.settles, settleRecord{sessionID: sessionID, state: state})
}

func (f *fakeSettleSink) snapshot() []settleRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]settleRecord, len(f.settles))
	copy(out, f.settles)
	return out
}

// deliveryAckFrame wraps a DeliveryAck variant carrying messageID.
func deliveryAckFrame(messageID string) *compassv1internal.AgentFrame {
	return &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_DeliveryAck{
			DeliveryAck: &compassv1internal.DeliveryAck{MessageId: messageID},
		},
	}
}

// Case 14: the ack arm resolves the channel and calls AckDelivery with the right
// (agent, channel, message_id); an ack for an unknown message is a fail-closed
// no-op (no cursor advance, no teardown).
func TestDeliveryAckAdvancesCursor(t *testing.T) {
	hub := newHubOnly()
	del := newFakeDeliveryStore()
	hub.SetDeliveryStore(del)
	bindSession(hub, "sess-1") // binds sess-1 -> testAgentAccount
	del.channels["m1"] = "chan-1"

	// A valid ack: resolve channel, advance the cursor for the bound agent.
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1, SessionID: "sess-1", Frame: deliveryAckFrame("m1"),
	}); err != nil {
		t.Fatalf("Deliver(delivery_ack) = %v, want nil (never a teardown)", err)
	}
	acks := del.ackSnapshot()
	if len(acks) != 1 {
		t.Fatalf("acks = %d, want 1", len(acks))
	}
	if acks[0].agent != testAgentAccount || acks[0].channel != "chan-1" || acks[0].messageID != "m1" {
		t.Fatalf("ack = %+v, want {%s, chan-1, m1}", acks[0], testAgentAccount)
	}

	// An ack for an unknown message: fail-closed no-op, no advance, no teardown.
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 2, SessionID: "sess-1", Frame: deliveryAckFrame("ghost"),
	}); err != nil {
		t.Fatalf("Deliver(unknown ack) = %v, want nil (fail-closed no-op)", err)
	}
	if got := len(del.ackSnapshot()); got != 1 {
		t.Fatalf("acks after unknown = %d, want still 1 (no advance on a foreign ack)", got)
	}
}

// Case 14 (unbound half): an ack for a session with no bound agent is a
// fail-closed no-op — never a cursor advance under a wrong/absent account.
func TestDeliveryAckUnboundSessionIsNoOp(t *testing.T) {
	hub := newHubOnly()
	del := newFakeDeliveryStore()
	hub.SetDeliveryStore(del)
	del.channels["m1"] = "chan-1"

	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1, SessionID: "never-bound", Frame: deliveryAckFrame("m1"),
	}); err != nil {
		t.Fatalf("Deliver(ack, unbound) = %v, want nil", err)
	}
	if got := len(del.ackSnapshot()); got != 0 {
		t.Fatalf("acks = %d, want 0 (unbound session advances nothing)", got)
	}
}

// Case 2/§2: the hub fires its settle-edge sink at the deliverSession arm, right
// after the lifecycle publish, with the transition's session + state — and does
// NOT fire on a trace-only frame (UNSPECIFIED). Nil-safe: a hub with no settle
// sink (every pre-existing test) is unchanged, covered by the existing suite.
func TestDeliverSessionFiresSettleSink(t *testing.T) {
	hub, life, _ := newHub()
	settle := &fakeSettleSink{}
	hub.SetSettleSink(settle)

	// A lifecycle transition fires both the lifecycle publish and the settle sink.
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1, SessionID: "sess-1",
		Frame: sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_READY),
	}); err != nil {
		t.Fatalf("Deliver(session READY) = %v, want nil", err)
	}
	if got := len(life.snapshot()); got != 1 {
		t.Fatalf("lifecycle publishes = %d, want 1 (settle must not replace the lifecycle publish)", got)
	}
	got := settle.snapshot()
	if len(got) != 1 || got[0].sessionID != "sess-1" || got[0].state != compassv1.AgentSessionState_AGENT_SESSION_STATE_READY {
		t.Fatalf("settle = %+v, want one {sess-1, READY}", got)
	}

	// A trace-only frame (UNSPECIFIED) is not a settle edge: no settle fires.
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 2, SessionID: "sess-1", Frame: sessionTraceFrame("trace"),
	}); err != nil {
		t.Fatalf("Deliver(trace) = %v, want nil", err)
	}
	if got := len(settle.snapshot()); got != 1 {
		t.Fatalf("settles after trace = %d, want still 1 (trace is not a settle edge)", got)
	}
}

// lifecycleRecord is one recorded OnSessionLifecycle call.
type lifecycleRecord struct {
	account   store.AccountID
	sessionID string
	state     compassv1.AgentSessionState
}

// promotedRecord is one recorded OnSessionPromoted call.
type promotedRecord struct {
	account   store.AccountID
	sessionID string
}

// fakePresenceSink records the hub's presence-edge calls — the SEA-1569 T8
// lifecycle + reconciliation sink. Concurrency-safe for parity with the real
// component, which is fed from the hub's goroutine.
type fakePresenceSink struct {
	mu        sync.Mutex
	lifecycle []lifecycleRecord
	promoted  []promotedRecord
}

func (f *fakePresenceSink) OnSessionLifecycle(account store.AccountID, sessionID string, state compassv1.AgentSessionState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lifecycle = append(f.lifecycle, lifecycleRecord{account: account, sessionID: sessionID, state: state})
}

func (f *fakePresenceSink) OnSessionPromoted(account store.AccountID, sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.promoted = append(f.promoted, promotedRecord{account: account, sessionID: sessionID})
}

func (f *fakePresenceSink) lifecycleSnapshot() []lifecycleRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]lifecycleRecord, len(f.lifecycle))
	copy(out, f.lifecycle)
	return out
}

func (f *fakePresenceSink) promotedSnapshot() []promotedRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]promotedRecord, len(f.promoted))
	copy(out, f.promoted)
	return out
}

// T8: the hub fires its presence-edge sink at deliverSession (a lifecycle
// transition, with the RESOLVED account) and at promoteSession (the
// reconciliation edge). A trace-only frame (UNSPECIFIED) is not a lifecycle
// edge, and a lifecycle transition on a session with no bound account publishes
// no presence (nothing to attribute). Nil-safe: a hub with no presence sink is
// today's behavior, covered by every other test.
func TestDeliverSessionFiresPresenceSink(t *testing.T) {
	hub := newHubOnly()
	pres := &fakePresenceSink{}
	hub.SetPresenceSink(pres)
	// bindSession promotes sess-1 -> testAgentAccount, which itself fires the
	// reconciliation edge once.
	bindSession(hub, "sess-1")
	if got := pres.promotedSnapshot(); len(got) != 1 || got[0].account != testAgentAccount || got[0].sessionID != "sess-1" {
		t.Fatalf("promoted = %+v, want one {%s, sess-1} from bindSession's promote", pres.promotedSnapshot(), testAgentAccount)
	}

	// A lifecycle transition on the bound session fires the lifecycle edge with
	// the resolved account.
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1, SessionID: "sess-1",
		Frame: sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING),
	}); err != nil {
		t.Fatalf("Deliver(session WORKING) = %v, want nil", err)
	}
	life := pres.lifecycleSnapshot()
	if len(life) != 1 || life[0].account != testAgentAccount || life[0].sessionID != "sess-1" ||
		life[0].state != compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING {
		t.Fatalf("lifecycle = %+v, want one {%s, sess-1, WORKING}", life, testAgentAccount)
	}

	// A trace-only frame is not a lifecycle edge: no further presence call.
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 2, SessionID: "sess-1", Frame: sessionTraceFrame("trace"),
	}); err != nil {
		t.Fatalf("Deliver(trace) = %v, want nil", err)
	}
	if got := len(pres.lifecycleSnapshot()); got != 1 {
		t.Fatalf("lifecycle after trace = %d, want still 1 (trace is not a lifecycle edge)", got)
	}

	// A lifecycle transition on an UNBOUND session publishes no presence (no
	// account to attribute it to).
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 3, SessionID: "never-bound",
		Frame: sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_READY),
	}); err != nil {
		t.Fatalf("Deliver(unbound session) = %v, want nil", err)
	}
	if got := len(pres.lifecycleSnapshot()); got != 1 {
		t.Fatalf("lifecycle after unbound transition = %d, want still 1 (no account, no presence)", got)
	}
}

// T8 nil-safe: a hub with no presence sink handles a lifecycle transition and a
// promotion without panicking.
func TestDeliverSessionNilPresenceSinkIsSafe(t *testing.T) {
	hub := newHubOnly()
	bindSession(hub, "sess-1") // promoteSession with a nil presence sink
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1, SessionID: "sess-1",
		Frame: sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_READY),
	}); err != nil {
		t.Fatalf("Deliver with nil presence sink = %v, want nil (nil-safe)", err)
	}
}

// Case 13: DispatchControl is SEND-ONLY — a successful deliver returns PROMPTLY
// with no synchronous result (success rides a later delivery_ack). A bug reusing
// dispatch/relay would register an inflight call and block on waitCall for a
// result that never comes, hanging until ctx timeout. The test drives a live
// Sessions stream whose Send succeeds but returns NO result, and asserts
// DispatchControl returns without blocking.
func TestDispatchControlSendOnlyDoesNotBlock(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	router, _, err := hub.routerFor("sess-1")
	if err != nil {
		t.Fatalf("routerFor: %v", err)
	}
	// A live stream that accepts the push and returns NO result (a successful
	// deliver sends no SessionsRequest back).
	sent := make(chan *compassv1internal.SessionsResponse, 1)
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		sent <- cmd
		return nil
	})

	op := &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_Deliver{
			Deliver: &compassv1internal.DeliverControl{Message: &compassv1.Message{Id: "m1"}},
		},
	}
	// If DispatchControl blocked on a result, this call would not return; the test
	// would hang and fail at the -timeout deadline. It returning at all is the
	// proof of the send-only contract.
	done := make(chan error, 1)
	go func() { done <- hub.DispatchControl(context.Background(), "sess-1", op) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DispatchControl (send-only, live stream) = %v, want nil", err)
		}
	case <-timeAfter():
		t.Fatal("DispatchControl blocked on a send-only deliver — it must not wait for a result")
	}

	// The command reached the stream, wrapped as a deliver.
	select {
	case cmd := <-sent:
		if cmd.GetDeliverControl().GetOp().GetDeliver().GetMessage().GetId() != "m1" {
			t.Fatalf("pushed command = %+v, want a deliver for m1", cmd)
		}
	case <-timeAfter():
		t.Fatal("DispatchControl pushed nothing to the live stream")
	}
}

// Case 4 (companion): a synchronous refusal — no live Sessions stream — returns
// an error to the consumer (which falls to the sweep), and the cursor is never
// advanced by DispatchControl (it advances only on delivery_ack). A send-only
// deliver registers no inflight call, so a LATER RunnerError refusal for its id
// is observed (counted), not dropped as unknown.
func TestDispatchControlNoLiveStreamRefuses(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	// No stream attached: send is nil.
	op := &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_Deliver{
			Deliver: &compassv1internal.DeliverControl{Message: &compassv1.Message{Id: "m1"}},
		},
	}
	if err := hub.DispatchControl(context.Background(), "sess-1", op); err == nil {
		t.Fatal("DispatchControl with no live stream = nil, want an error so the consumer falls to the sweep")
	}
}

// Case 4/5 (refusal observability): a RunnerError result correlated to a
// send-only deliver's request id is COUNTED (observed), not dropped as unknown —
// the cursor stays unadvanced and the sweep redelivers. A successful deliver
// (no result) is never counted a refusal.
func TestSendOnlyRefusalIsObservedNotDropped(t *testing.T) {
	r := newCommandRouter()
	captured := make(chan *compassv1internal.SessionsResponse, 1)
	r.attach(func(cmd *compassv1internal.SessionsResponse) error {
		captured <- cmd
		return nil
	})
	defer r.detach(errStreamClosed)
	deliverCmd := &compassv1internal.SessionsResponse{
		RequestId: "req-deliver",
		Command: &compassv1internal.SessionsResponse_DeliverControl{
			DeliverControl: &compassv1internal.DispatchControl{SessionId: "sess-1"},
		},
	}
	if err := r.send1(deliverCmd); err != nil {
		t.Fatalf("send1 = %v, want nil", err)
	}
	// The frame is queued-not-pushed; gate on the sender draining it to the wire.
	select {
	case got := <-captured:
		if got.GetRequestId() != "req-deliver" {
			t.Fatalf("send1 pushed %q, want req-deliver", got.GetRequestId())
		}
	case <-timeAfter():
		t.Fatal("send1 never reached the wire")
	}
	if got := r.RefusedDelivers(); got != 0 {
		t.Fatalf("RefusedDelivers before any refusal = %d, want 0", got)
	}

	// A RunnerError result for the deliver's id: complete observes it as a refusal.
	r.complete(&compassv1internal.SessionsRequest{
		RequestId: "req-deliver",
		Result: &compassv1internal.SessionsRequest_Error{Error: &compassv1internal.RunnerError{
			Code: compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_RESOURCE_EXHAUSTED,
		}},
	})
	if got := r.RefusedDelivers(); got != 1 {
		t.Fatalf("RefusedDelivers after a refusal = %d, want 1 (observed, not dropped)", got)
	}

	// A truly-unknown id is still ignored (the original contract), not counted.
	r.complete(&compassv1internal.SessionsRequest{
		RequestId: "ghost",
		Result:    &compassv1internal.SessionsRequest_Error{Error: &compassv1internal.RunnerError{}},
	})
	if got := r.RefusedDelivers(); got != 1 {
		t.Fatalf("RefusedDelivers after an unknown id = %d, want still 1", got)
	}
}

// The ack arm tolerates a store fault advancing the cursor: it is a non-fatal
// drop (a missed ack costs a redundant redeliver on the next sweep), never a
// teardown.
func TestDeliveryAckStoreFaultIsNonFatal(t *testing.T) {
	hub := newHubOnly()
	del := newFakeDeliveryStore()
	del.channels["m1"] = "chan-1"
	del.ackErr = errors.New("transient store fault")
	hub.SetDeliveryStore(del)
	bindSession(hub, "sess-1")

	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1, SessionID: "sess-1", Frame: deliveryAckFrame("m1"),
	}); err != nil {
		t.Fatalf("Deliver(ack, store fault) = %v, want nil (non-fatal drop, not a teardown)", err)
	}
}

// SEA-1569 T3 §6: an ack drop increments the dedicated DroppedAcks counter. A
// delivery_ack is not a conversation frame; a drop (an unbound acking session or
// an unknown message) is logged + counted and never a teardown. This drives two
// ack drops — an unbound acking session and an unknown message — and asserts
// DroppedAcks counts them, including the FrameDiagnostics snapshot mirror.
func TestDeliveryAckDropsAreCounted(t *testing.T) {
	hub := newHubOnly()
	del := newFakeDeliveryStore()
	hub.SetDeliveryStore(del)
	del.channels["m1"] = "chan-1"

	// Drop 1: an ack for a session with no bound agent.
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1, SessionID: "never-bound", Frame: deliveryAckFrame("m1"),
	}); err != nil {
		t.Fatalf("Deliver(ack, unbound) = %v, want nil", err)
	}
	// Drop 2: an ack for an unknown message under a bound session.
	bindSession(hub, "sess-1")
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 2, SessionID: "sess-1", Frame: deliveryAckFrame("ghost"),
	}); err != nil {
		t.Fatalf("Deliver(ack, unknown message) = %v, want nil", err)
	}

	if got := hub.DroppedAcks(); got != 2 {
		t.Fatalf("DroppedAcks = %d, want 2 (both ack drops land in the dedicated counter)", got)
	}
	// And the snapshot mirrors the accessor under one lock.
	if diag := hub.FrameDiagnostics(); diag.DroppedAcks != 2 {
		t.Fatalf("FrameDiagnostics.DroppedAcks = %d, want 2", diag.DroppedAcks)
	}
}

// SEA-1577 ask-answer wake arm: WakeAskAnswer with a live bound session
// dispatches an AgentControl.ask_answer op down the T3 rail carrying the askID
// and answers. Mirrors TestDispatchControlSendOnlyDoesNotBlock's live-stream
// fake; the binding is the real Provision->Start promotion (bindSession ->
// testAgentAccount).
func TestWakeAskAnswerLiveSessionDispatchesAskAnswerOp(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	bindSession(hub, "sess-1")
	router, _, err := hub.routerFor("sess-1")
	if err != nil {
		t.Fatalf("routerFor: %v", err)
	}
	sent := make(chan *compassv1internal.SessionsResponse, 1)
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		sent <- cmd
		return nil
	})

	// Two answers, the second custom-text-only with an empty ChosenOptionIds, to
	// pin the "no remap" passthrough at the boundary: the slice reaches the op
	// byte-for-byte (SEA-1577 forwards req.Msg.GetAnswers() straight through).
	answers := []*compassv1.AskQuestionAnswer{
		{QuestionId: "q1", ChosenOptionIds: []string{"opt-a"}},
		{QuestionId: "q2", CustomText: "freeform"},
	}
	hub.WakeAskAnswer(context.Background(), testAgentAccount, "ask-1", answers)

	select {
	case cmd := <-sent:
		op := cmd.GetDeliverControl().GetOp()
		aa := op.GetAskAnswer()
		if aa == nil {
			t.Fatalf("pushed op = %T, want an AgentControl_AskAnswer", op.GetControl())
		}
		if aa.GetAskId() != "ask-1" {
			t.Fatalf("ask_answer op ask_id = %q, want ask-1", aa.GetAskId())
		}
		got := aa.GetAnswers()
		if len(got) != 2 {
			t.Fatalf("ask_answer op answers len = %d, want 2", len(got))
		}
		if got[0].GetQuestionId() != "q1" ||
			len(got[0].GetChosenOptionIds()) != 1 || got[0].GetChosenOptionIds()[0] != "opt-a" {
			t.Fatalf("ask_answer op answers[0] = %+v, want {q1 [opt-a]}", got[0])
		}
		if got[1].GetQuestionId() != "q2" || got[1].GetCustomText() != "freeform" ||
			len(got[1].GetChosenOptionIds()) != 0 {
			t.Fatalf("ask_answer op answers[1] = %+v, want {q2 custom_text=freeform, no chosen ids}", got[1])
		}
	case <-timeAfter():
		t.Fatal("WakeAskAnswer pushed nothing to the live stream")
	}
}

// SEA-1577: WakeAskAnswer for an account with no bound live session is a silent
// no-op — nothing is dispatched, nothing panics. The agent reads the answer on
// its next turn via the normal delivery path.
func TestWakeAskAnswerNoLiveSessionIsSilentNoOp(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	router, _, err := hub.routerFor("sess-1")
	if err != nil {
		t.Fatalf("routerFor: %v", err)
	}
	sent := make(chan *compassv1internal.SessionsResponse, 1)
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		sent <- cmd
		return nil
	})

	// No bindSession: testAgentAccount has no live session -> SessionForAccount
	// ok=false -> nothing dispatched.
	hub.WakeAskAnswer(context.Background(), testAgentAccount, "ask-1",
		[]*compassv1.AskQuestionAnswer{{QuestionId: "q1"}})

	select {
	case cmd := <-sent:
		t.Fatalf("WakeAskAnswer with no live session pushed %+v, want nothing", cmd)
	case <-timeAfter():
		// nothing dispatched — the no-op contract holds.
	}
}

// SEA-1577: a synchronous dispatch refusal (a bound session but no live Sessions
// stream) is swallowed — WakeAskAnswer is void and must not panic or surface the
// error, so the RPC path (comms.RespondToAsk) never fails on a wake fault. The
// answer is already durably recorded + fanned out.
func TestWakeAskAnswerRefusedDispatchIsSwallowed(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	bindSession(hub, "sess-1")
	// No router.attach: DispatchControl finds no live stream and refuses; the
	// refusal must be logged and swallowed, not surfaced.
	hub.WakeAskAnswer(context.Background(), testAgentAccount, "ask-1",
		[]*compassv1.AskQuestionAnswer{{QuestionId: "q1"}})
	// Reaching here without a panic is the void-swallow contract.
}
