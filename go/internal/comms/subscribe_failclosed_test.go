// The M5 fail-closed / clean-end contract of the SubscribeComms visibility
// filter (subscribe.go forwardComms + the SubscribeComms CodeInternal wrap),
// driven with NO database. forwardComms consults visibility through the
// eventVisibility interface, so a fake drives the two security-critical branches
// the DB-backed tests never reach: a store fault (the real store returns
// (bool, nil) on the happy path, never (_, err)) and a cancellation racing the
// in-flight query. This file is untagged, so it runs on the default `go test`
// lane — no pgtest, no COMPASS_TEST_DATABASE_DSN.
//
// The invariant (subscribe.go:76-85, :93-120):
//   - Store fault resolving visibility  -> event NEVER sent (fail closed), the
//     fault is propagated, and SubscribeComms surfaces connect.CodeInternal with
//     the opaque errStreamVisibility text (no leak of the filtered event).
//   - Cancellation (context.Canceled / DeadlineExceeded wrapping the visibility
//     query) -> clean end (nil), no fault, no CodeInternal.
//   - Visible (true, nil)      -> event delivered once.
//   - Not visible (false, nil) -> event skipped, the stream continues.
//
// A real *connect.ServerStream has no exported constructor, so forwardComms is
// driven through a one-shot connect server-stream handler over httptest (the
// same wire-through pattern as subscribe_test.go); Send calls are observed as
// client-side receives (zero received == Send never called). The handler mirrors
// SubscribeComms's error wrap (subscribe.go:49-60) verbatim — the only part of
// the M5 path unreachable in a no-DB lane, since Comms holds the store
// concretely — so the client-observable CodeInternal/opaque-message contract is
// asserted against forwardComms's real return and the real errStreamVisibility.
//
// This file also hosts the untagged, no-database ring-lag resync and
// replay-boundary tests (RIG-3538): they share driveForwardComms + a real
// events.Bus, and the arm-2 underflow cases drive SubscribeComms with a nil
// store, whose terminal resync precedes any store access.

package comms

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/store"
)

// testActor is the subscriber whose visibility the fake resolves. Its value is
// irrelevant to the fake (it keys on channel id) but it is the actor forwardComms
// passes through, so it is fixed for determinism.
const testActor store.AccountID = "actor-1"

// errUnexpectedVisCall fails a test loudly if forwardComms routes a MessagePosted
// event to any predicate other than IsTopicChannelMember (visibleToActor's
// dispatch). Every event these tests publish is a MessagePosted, whose channel is
// resolved through its topic now, so only IsTopicChannelMember should ever be
// consulted.
var errUnexpectedVisCall = errors.New("fakeVisibility: unexpected predicate call")

// fakeVisibility is a no-DB eventVisibility whose IsTopicChannelMember verdict is
// supplied per topic id, so one fake drives every case: (false, errBoom) for a
// fault, (false, wrapped-context-error) for a cancellation, (true, nil) for a
// delivery, (false, nil) for a skip. The other predicates return
// errUnexpectedVisCall — they are never exercised by a MessagePosted, and a
// dispatch regression that routed one elsewhere would redden immediately.
type fakeVisibility struct {
	isMember func(topicID string) (bool, error)
}

func (f fakeVisibility) IsTopicChannelMember(_ context.Context, _ store.AccountID, topicID string) (bool, error) {
	return f.isMember(topicID)
}

func (f fakeVisibility) IsChannelMember(context.Context, store.AccountID, store.ChannelID) (bool, error) {
	return false, errUnexpectedVisCall
}

func (f fakeVisibility) ChannelVisibleTo(context.Context, store.AccountID, store.ChannelID) (bool, error) {
	return false, errUnexpectedVisCall
}

func (f fakeVisibility) ChannelGroupVisibleTo(context.Context, store.AccountID, store.ChannelGroupID) (bool, error) {
	return false, errUnexpectedVisCall
}

func (f fakeVisibility) AccountVisibleTo(context.Context, store.AccountID, store.AccountID) (bool, error) {
	return false, errUnexpectedVisCall
}

func (f fakeVisibility) IsAgentWorkspaceVisible(context.Context, store.AccountID, store.AccountID) (bool, error) {
	return false, errUnexpectedVisCall
}

func (f fakeVisibility) SharesVisibleChannel(context.Context, store.AccountID, store.AccountID) (bool, error) {
	return false, errUnexpectedVisCall
}

// compile-time proof the fake satisfies the seam forwardComms consults.
var _ eventVisibility = fakeVisibility{}

// messagePostedOn is a MessagePosted response on topicID, the shape the bus fans
// out and forwardComms filters via IsTopicChannelMember (the channel is resolved
// through the topic now).
func messagePostedOn(topicID string) *compassv1.SubscribeCommsResponse {
	return &compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_MessagePosted{
			MessagePosted: &compassv1.MessagePosted{
				Message: &compassv1.Message{
					TopicId: topicID,
				},
			},
		},
	}
}

// subscriptionOf publishes events onto a fresh bus, subscribes at since_seq=0 so
// they land in Replay, then closes the bus so the live channel is closed. The
// resulting Subscription drives forwardComms to completion with no sleeps and no
// wall-clock waits: it drains the replay snapshot, then the closed (unlagged)
// live channel ends the loop cleanly (subscribe.go:141-148).
func subscriptionOf(t *testing.T, events_ ...*compassv1.SubscribeCommsResponse) events.Subscription[*compassv1.SubscribeCommsResponse] {
	t.Helper()
	bus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	for _, e := range events_ {
		bus.Publish(e)
	}
	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	bus.Close()
	return sub
}

// forwardResult is what one drive of forwardComms observes: its raw return
// (fwdErr), the events the client actually received (Send calls that reached the
// wire), and the terminal stream error the client saw after the SubscribeComms
// error wrap.
type forwardResult struct {
	fwdErr    error
	received  []*compassv1.SubscribeCommsResponse
	streamErr error
}

// driveForwardComms runs forwardComms(ctx, testActor, vis, sub, stream) inside a
// one-shot connect server-stream handler — the only way to hand it a real
// *connect.ServerStream — and returns its raw return alongside the client's
// received events and terminal error. The handler mirrors SubscribeComms's error
// handling verbatim (subscribe.go:49-60): a non-nil forwardComms return becomes
// connect.CodeInternal + errStreamVisibility, so the client observes the exact
// fault contract SubscribeComms exposes; a nil return is a clean EOF.
func driveForwardComms(t *testing.T, vis eventVisibility, sub events.Subscription[*compassv1.SubscribeCommsResponse]) forwardResult {
	t.Helper()

	fwdErrCh := make(chan error, 1)
	const proc = compassv1connect.CommsServiceSubscribeCommsProcedure
	handler := connect.NewServerStreamHandler(
		proc,
		func(ctx context.Context, _ *connect.Request[compassv1.SubscribeCommsRequest], stream *connect.ServerStream[compassv1.SubscribeCommsResponse]) error {
			err := forwardComms(ctx, testActor, vis, sub, stream)
			fwdErrCh <- err
			if err != nil {
				// Mirrors SubscribeComms (subscribe.go:56-58): the underlying
				// error is never returned to the client, only the opaque
				// errStreamVisibility under CodeInternal.
				return connect.NewError(connect.CodeInternal, errStreamVisibility)
			}
			return nil
		},
	)

	mux := http.NewServeMux()
	mux.Handle(proc, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := connect.NewClient[compassv1.SubscribeCommsRequest, compassv1.SubscribeCommsResponse](
		srv.Client(), srv.URL+proc,
	)

	stream, err := client.CallServerStream(context.Background(), connect.NewRequest(&compassv1.SubscribeCommsRequest{}))
	if err != nil {
		t.Fatalf("CallServerStream: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	var received []*compassv1.SubscribeCommsResponse
	for stream.Receive() {
		received = append(received, stream.Msg())
	}

	return forwardResult{
		fwdErr:    <-fwdErrCh,
		received:  received,
		streamErr: stream.Err(),
	}
}

// TestForwardCommsFailsClosedOnStoreFault pins the M5 fail-closed core: a store
// fault resolving a MessagePosted's visibility (a) is propagated by forwardComms
// (not swallowed to nil), (b) NEVER reaches stream.Send (the private event is
// never leaked unfiltered), and (c) surfaces to the client as CodeInternal with
// the opaque errStreamVisibility text — never the underlying fault's detail.
//
// Teeth: a fail-open regression (Send before the visibility check) delivers the
// event -> received != 0. An error-swallowing regression (return nil) makes the
// stream a clean EOF -> fwdErr != errBoom and streamErr is not CodeInternal.
func TestForwardCommsFailsClosedOnStoreFault(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom: private channel 42 membership row")
	vis := fakeVisibility{isMember: func(string) (bool, error) {
		return false, errBoom
	}}
	sub := subscriptionOf(t, messagePostedOn("private-42"))

	got := driveForwardComms(t, vis, sub)

	// (a) fault propagated, not swallowed.
	if !errors.Is(got.fwdErr, errBoom) {
		t.Fatalf("forwardComms should propagate the store fault, got %v", got.fwdErr)
	}
	// (b) fail closed: the event never reached the wire.
	if len(got.received) != 0 {
		t.Fatalf("fail-open leak: %d event(s) sent on a visibility fault, want 0", len(got.received))
	}
	// (c) client sees CodeInternal + opaque message, no leak of the fault detail.
	if code := connect.CodeOf(got.streamErr); code != connect.CodeInternal {
		t.Fatalf("client code = %v, want CodeInternal", code)
	}
	var connErr *connect.Error
	if !errors.As(got.streamErr, &connErr) {
		t.Fatalf("stream error is not a *connect.Error: %v", got.streamErr)
	}
	if connErr.Message() != errStreamVisibility.Error() {
		t.Fatalf("client message = %q, want opaque %q", connErr.Message(), errStreamVisibility.Error())
	}
	if msg := connErr.Message(); strings.Contains(msg, "boom") || strings.Contains(msg, "private channel 42") {
		t.Fatalf("fault detail leaked to client: %q", msg)
	}
}

// TestForwardCommsCleanEndOnCancellation pins the M5 clean-end contract: a
// cancellation racing the in-flight visibility query (an error wrapping
// context.Canceled or context.DeadlineExceeded, subscribe.go:103-108) is a
// graceful end, not a store fault — forwardComms returns nil, nothing is sent,
// and the client sees a clean EOF, never CodeInternal (so SubscribeComms would
// neither log ERROR nor return a fault). Paired against the fault test above so
// it cannot pass by simply never surfacing anything: a fault DOES surface, a
// cancellation does not.
//
// Teeth: were cancellation treated as a store fault (return err instead of
// nil, false), fwdErr would be non-nil and the client would see CodeInternal.
func TestForwardCommsCleanEndOnCancellation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{"wrapped_canceled", fmt.Errorf("visibility query: %w", context.Canceled)},
		{"wrapped_deadline_exceeded", fmt.Errorf("visibility query: %w", context.DeadlineExceeded)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			vis := fakeVisibility{isMember: func(string) (bool, error) {
				return false, tc.err
			}}
			sub := subscriptionOf(t, messagePostedOn("chan-x"))

			got := driveForwardComms(t, vis, sub)

			if got.fwdErr != nil {
				t.Fatalf("cancellation must be a clean end, forwardComms returned %v", got.fwdErr)
			}
			if len(got.received) != 0 {
				t.Fatalf("cancellation sent %d event(s), want 0", len(got.received))
			}
			if got.streamErr != nil {
				t.Fatalf("cancellation must be a clean EOF, client saw %v (code %v)", got.streamErr, connect.CodeOf(got.streamErr))
			}
		})
	}
}

// TestForwardCommsDeliversVisibleEvent is the positive companion: a visible
// (true, nil) MessagePosted IS delivered exactly once, with the payload mapped
// through commsToResponse, and forwardComms drains cleanly (nil, no error). This
// is what proves the fault/skip negatives are not passing vacuously — the same
// harness DOES deliver when visibility permits.
//
// Teeth: a regression that dropped visible events yields received == 0.
func TestForwardCommsDeliversVisibleEvent(t *testing.T) {
	t.Parallel()

	vis := fakeVisibility{isMember: func(string) (bool, error) {
		return true, nil
	}}
	sub := subscriptionOf(t, messagePostedOn("chan-visible"))
	wantSeq := sub.Replay[0].Seq

	got := driveForwardComms(t, vis, sub)

	if got.fwdErr != nil {
		t.Fatalf("forwardComms returned %v, want nil on a clean drain", got.fwdErr)
	}
	if got.streamErr != nil {
		t.Fatalf("client saw terminal error %v, want clean EOF", got.streamErr)
	}
	if len(got.received) != 1 {
		t.Fatalf("delivered %d event(s), want exactly 1", len(got.received))
	}
	if ch := got.received[0].GetMessagePosted().GetMessage().GetTopicId(); ch != "chan-visible" {
		t.Fatalf("delivered channel = %q, want %q", ch, "chan-visible")
	}
	if seq := got.received[0].GetSeq(); seq != wantSeq {
		t.Fatalf("delivered seq = %d, want %d (commsToResponse must carry the envelope seq)", seq, wantSeq)
	}
}

// TestForwardCommsSkipsNonVisibleEventAndContinues pins that a not-visible
// (false, nil) event is silently skipped — NOT a fault, NOT a stream end: the
// stream continues and a subsequent visible event is still delivered
// (subscribe.go:112-113). The two-event ordering is the proof: the private event
// is dropped, the following visible one arrives, so exactly the visible one
// reaches the client.
//
// Teeth: if a skip aborted the stream (return instead of continue), the trailing
// visible event would never be sent -> received == 0. If the skip leaked, both
// would arrive -> received == 2.
func TestForwardCommsSkipsNonVisibleEventAndContinues(t *testing.T) {
	t.Parallel()

	vis := fakeVisibility{isMember: func(topicID string) (bool, error) {
		return topicID == "chan-visible", nil
	}}
	sub := subscriptionOf(t,
		messagePostedOn("chan-hidden"),
		messagePostedOn("chan-visible"),
	)

	got := driveForwardComms(t, vis, sub)

	if got.fwdErr != nil {
		t.Fatalf("a skip must not be a fault, forwardComms returned %v", got.fwdErr)
	}
	if got.streamErr != nil {
		t.Fatalf("client saw terminal error %v, want clean EOF", got.streamErr)
	}
	if len(got.received) != 1 {
		t.Fatalf("delivered %d event(s), want exactly 1 (the visible one)", len(got.received))
	}
	if ch := got.received[0].GetMessagePosted().GetMessage().GetTopicId(); ch != "chan-visible" {
		t.Fatalf("delivered channel = %q, want the visible %q (hidden one must be skipped)", ch, "chan-visible")
	}
}

// visibleAlways is a no-DB eventVisibility that admits every event, so a
// bus-only test drives forwardComms's delivery + terminal-resync paths without a
// store: the ring-lag contract is a bus/handler property, not a visibility one.
type visibleAlways struct{}

func (visibleAlways) IsTopicChannelMember(context.Context, store.AccountID, string) (bool, error) {
	return true, nil
}
func (visibleAlways) IsChannelMember(context.Context, store.AccountID, store.ChannelID) (bool, error) {
	return true, nil
}
func (visibleAlways) ChannelVisibleTo(context.Context, store.AccountID, store.ChannelID) (bool, error) {
	return true, nil
}
func (visibleAlways) ChannelGroupVisibleTo(context.Context, store.AccountID, store.ChannelGroupID) (bool, error) {
	return true, nil
}
func (visibleAlways) AccountVisibleTo(context.Context, store.AccountID, store.AccountID) (bool, error) {
	return true, nil
}
func (visibleAlways) IsAgentWorkspaceVisible(context.Context, store.AccountID, store.AccountID) (bool, error) {
	return true, nil
}
func (visibleAlways) SharesVisibleChannel(context.Context, store.AccountID, store.AccountID) (bool, error) {
	return true, nil
}

var _ eventVisibility = visibleAlways{}

// overrunSubscription subscribes to a fresh bus, then publishes more than the
// live buffer holds without draining Live, so the bus latches the subscriber
// lagged and closes its channel (events_test.go TestOverrunClosesLiveAndLatchesLagged,
// a different package — the technique, not its symbols). The returned
// subscription drives forwardComms's sub.Lagged() terminal-resync branch.
func overrunSubscription(t *testing.T) events.Subscription[*compassv1.SubscribeCommsResponse] {
	t.Helper()
	bus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("bus.Subscribe(0,0): %v", err)
	}
	// One past the live buffer (sized to events.RingCapacity): the overflowing
	// publish is what the non-blocking fan-out cannot place, so it latches
	// lagged. Derived from the constant, never a literal — a capacity bump must
	// keep overrunning rather than silently stop.
	for i := range events.RingCapacity + 1 {
		bus.Publish(messagePostedOn(fmt.Sprintf("chan-%d", i)))
	}
	if !sub.Lagged() {
		t.Fatalf("subscription not lagged after 1025 undrained publishes, want lagged")
	}
	return sub
}

// TestForwardCommsLiveTailOverrunEmitsTerminalResync pins arm 1: a subscriber
// that overruns the ring (Lagged) makes forwardComms emit a CommsResyncRequired
// as its FINAL frame, then end the stream cleanly (nil, clean EOF) — never a
// fault. The last-frame assertion is the contract: a resync merely "present"
// somewhere would be a weaker claim than terminal.
//
// Teeth: dropping the sub.Lagged() send in forwardComms yields a final frame
// that is a MessagePosted, not a resync.
func TestForwardCommsLiveTailOverrunEmitsTerminalResync(t *testing.T) {
	sub := overrunSubscription(t)
	wantEpoch := sub.Epoch

	got := driveForwardComms(t, visibleAlways{}, sub)

	if got.fwdErr != nil {
		t.Fatalf("forwardComms returned %v, want nil (an overrun ends cleanly, not a fault)", got.fwdErr)
	}
	if got.streamErr != nil {
		t.Fatalf("client saw terminal error %v (code %v), want a clean EOF after the resync", got.streamErr, connect.CodeOf(got.streamErr))
	}
	if len(got.received) == 0 {
		t.Fatalf("received 0 frames, want the buffered live tail then a terminal resync")
	}
	// Every frame ahead of the resync must be real buffered content: a
	// degradation that dropped the live tail but still sent the terminal frame
	// would satisfy the last-frame assertion alone.
	for i, f := range got.received[:len(got.received)-1] {
		if f.GetMessagePosted() == nil {
			t.Fatalf("frame %d = %T, want a buffered MessagePosted ahead of the terminal resync", i, f.GetPayload())
		}
	}
	final := got.received[len(got.received)-1]
	if final.GetResyncRequired() == nil {
		t.Fatalf("final frame = %T, want CommsResyncRequired as the last frame", final.GetPayload())
	}
	if final.GetInstanceEpoch() != wantEpoch {
		t.Fatalf("resync epoch = %d, want the subscription epoch %d", final.GetInstanceEpoch(), wantEpoch)
	}
}

// driveUnderflowResync opens a SubscribeComms stream over httptest against a
// nil-store Comms and returns the frames the client received plus its terminal
// error. The underflow branch returns its terminal resync before
// actorFromContext or any c.store access, so a nil store is sound here — not a
// weakened substitute. The bus supplies the seq space and instance epoch the
// cursor is validated against.
//
// The context is bounded because the failure mode of a dropped underflow guard
// is a registered live subscriber tailing forever: without a deadline that
// surfaces as a whole-suite timeout instead of this test failing.
func driveUnderflowResync(t *testing.T, bus *events.Bus[*compassv1.SubscribeCommsResponse], req *compassv1.SubscribeCommsRequest) forwardResult {
	t.Helper()
	svc := NewComms(nil, bus, testActor)

	path, handler := compassv1connect.NewCommsServiceHandler(svc)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := compassv1connect.NewCommsServiceClient(srv.Client(), srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	stream, err := client.SubscribeComms(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("SubscribeComms: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	var received []*compassv1.SubscribeCommsResponse
	for stream.Receive() {
		received = append(received, stream.Msg())
	}
	return forwardResult{received: received, streamErr: stream.Err()}
}

// TestSubscribeCommsCursorAtOrBeyondHeadResyncs pins arm 2's at/beyond-head
// branch in Subscribe: a positioned cursor >= the next seq the bus would
// assign refers to an event never emitted, so SubscribeComms returns a single
// terminal CommsResyncRequired stamped with the CURRENT instance epoch, then a
// clean end. Distinct from the stale-epoch branch (same-epoch cursor here).
//
// Teeth: were the at/beyond-head guard removed, Subscribe would register a live
// subscriber and the stream would hang on the live tail instead of resyncing.
//
// The guard itself is covered at the bus layer too (events_test.go
// TestSubscribeAtOrBeyondNextSeqUnderflows). The redundancy is deliberate: the
// handler translates every underflow trigger into the same wire frame, so this
// is the black-box confirmation that THIS trigger reaches a client as a resync.
func TestSubscribeCommsCursorAtOrBeyondHeadResyncs(t *testing.T) {
	bus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	bus.Publish(messagePostedOn("chan-a")) // seq 1, so nextSeq is 2

	got := driveUnderflowResync(t, bus, &compassv1.SubscribeCommsRequest{
		SinceSeq: 5, InstanceEpoch: bus.InstanceEpoch(),
	})

	if got.streamErr != nil {
		t.Fatalf("client saw terminal error %v, want a clean EOF after the resync", got.streamErr)
	}
	if len(got.received) != 1 {
		t.Fatalf("received %d frames, want exactly 1 (the terminal resync)", len(got.received))
	}
	if got.received[0].GetResyncRequired() == nil {
		t.Fatalf("first frame = %T, want CommsResyncRequired", got.received[0].GetPayload())
	}
	if epoch := got.received[0].GetInstanceEpoch(); epoch != bus.InstanceEpoch() {
		t.Fatalf("resync epoch = %d, want the current instance epoch %d", epoch, bus.InstanceEpoch())
	}
}

// TestSubscribeCommsCursorOlderThanRetainedResyncs pins arm 2's evicted-cursor
// branch in Subscribe, genuinely distinct from the at/beyond-head case: the
// ring is overrun so its oldest retained seq is far above 1, then a cursor at
// seq 1 (older than anything retained) can't be caught up by replay, so the
// stream is a single terminal resync at the current epoch.
//
// Teeth: without the eviction guard, replay would silently start mid-ring and
// the client would miss the gap between its cursor and the oldest retained seq.
func TestSubscribeCommsCursorOlderThanRetainedResyncs(t *testing.T) {
	bus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	// Publish past ring capacity so seq 1 is evicted: after RingCapacity+N
	// publishes the oldest retained seq is N+1, and the guard needs it above
	// sinceSeq+1, so any N >= 2 works. Derived from the constant so a capacity
	// bump still evicts rather than quietly registering a live tail.
	for i := range events.RingCapacity + 10 {
		bus.Publish(messagePostedOn(fmt.Sprintf("chan-%d", i)))
	}

	got := driveUnderflowResync(t, bus, &compassv1.SubscribeCommsRequest{
		SinceSeq: 1, InstanceEpoch: bus.InstanceEpoch(),
	})

	if got.streamErr != nil {
		t.Fatalf("client saw terminal error %v, want a clean EOF after the resync", got.streamErr)
	}
	if len(got.received) != 1 {
		t.Fatalf("received %d frames, want exactly 1 (the terminal resync)", len(got.received))
	}
	if got.received[0].GetResyncRequired() == nil {
		t.Fatalf("first frame = %T, want CommsResyncRequired", got.received[0].GetPayload())
	}
	if epoch := got.received[0].GetInstanceEpoch(); epoch != bus.InstanceEpoch() {
		t.Fatalf("resync epoch = %d, want the current instance epoch %d", epoch, bus.InstanceEpoch())
	}
}

// TestForwardCommsCursorAtHeadReplaysNothingBeforeLiveTail pins the
// replay-boundary exactness: a subscription positioned exactly at head has an
// empty Replay, so the FIRST frame forwardComms sends is a live-tail event
// published AFTER subscribing — never a replayed one. Proven by a sentinel
// (happens-before: publish is under the same lock Subscribe took), NOT a sleep:
// the sentinel is the first frame iff nothing replayed ahead of it.
//
// Teeth: an off-by-one that replayed the at-head event (Seq > sinceSeq flipped
// to >=) would put that event first, ahead of the sentinel.
func TestForwardCommsCursorAtHeadReplaysNothingBeforeLiveTail(t *testing.T) {
	bus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	headSeq := bus.Publish(messagePostedOn("chan-before")) // the at-head event

	sub, err := bus.Subscribe(headSeq, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("bus.Subscribe(%d, epoch): %v", headSeq, err)
	}
	if len(sub.Replay) != 0 {
		t.Fatalf("replay len = %d for a cursor at head, want 0 (nothing after head)", len(sub.Replay))
	}
	// Published after Subscribe registered the live subscriber, so it lands on
	// the live tail: it must be the first thing forwardComms sends.
	sentinelSeq := bus.Publish(messagePostedOn("chan-sentinel"))
	bus.Close()

	got := driveForwardComms(t, visibleAlways{}, sub)

	if got.fwdErr != nil {
		t.Fatalf("forwardComms returned %v, want nil on a clean drain", got.fwdErr)
	}
	if got.streamErr != nil {
		t.Fatalf("client saw terminal error %v, want a clean EOF", got.streamErr)
	}
	if len(got.received) != 1 {
		t.Fatalf("received %d frames, want exactly 1 (the sentinel, no replay ahead of it)", len(got.received))
	}
	if seq := got.received[0].GetSeq(); seq != sentinelSeq {
		t.Fatalf("first frame seq = %d, want the sentinel seq %d (an at-head cursor must replay nothing)", seq, sentinelSeq)
	}
	if ch := got.received[0].GetMessagePosted().GetMessage().GetTopicId(); ch != "chan-sentinel" {
		t.Fatalf("first frame channel = %q, want %q", ch, "chan-sentinel")
	}
}

// TestForwardCommsConcurrentPublishesObserveOneTotalSeqOrder pins that two
// subscribers tailing one bus over concurrent publishes observe the SAME total
// seq order: the bus stamps seq under a single lock, so every subscriber sees
// one linearization. Both observed seq slices must be identical AND strictly
// increasing — "both non-empty" would not distinguish a shared order from two
// divergent ones.
//
// Teeth: were seq assignment not serialized, two concurrent publishers could
// interleave differently per subscriber and the two slices would diverge.
func TestForwardCommsConcurrentPublishesObserveOneTotalSeqOrder(t *testing.T) {
	bus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	subA, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("bus.Subscribe A: %v", err)
	}
	subB, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("bus.Subscribe B: %v", err)
	}

	// Publish concurrently: the bus's publish lock is what forces a single total
	// order despite the racing goroutines.
	const publishers, perPublisher = 4, 50
	var wg sync.WaitGroup
	for p := range publishers {
		wg.Go(func() {
			for i := range perPublisher {
				bus.Publish(messagePostedOn(fmt.Sprintf("chan-%d-%d", p, i)))
			}
		})
	}
	wg.Wait()
	bus.Close()

	gotA := driveForwardComms(t, visibleAlways{}, subA)
	gotB := driveForwardComms(t, visibleAlways{}, subB)
	if gotA.fwdErr != nil || gotB.fwdErr != nil {
		t.Fatalf("forwardComms returned A=%v B=%v, want nil (clean drain both)", gotA.fwdErr, gotB.fwdErr)
	}

	seqsA := seqsOf(gotA.received)
	seqsB := seqsOf(gotB.received)
	total := publishers * perPublisher
	if len(seqsA) != total || len(seqsB) != total {
		t.Fatalf("observed %d/%d events, want %d each", len(seqsA), len(seqsB), total)
	}
	for i := range seqsA {
		if seqsA[i] != seqsB[i] {
			t.Fatalf("seq order diverges at index %d: A=%d B=%d, want identical total order", i, seqsA[i], seqsB[i])
		}
		if i > 0 && seqsA[i] <= seqsA[i-1] {
			t.Fatalf("seq not strictly increasing at index %d: %d after %d", i, seqsA[i], seqsA[i-1])
		}
	}
}

// seqsOf projects the stream seq off each received frame, for the total-order
// comparison above.
func seqsOf(frames []*compassv1.SubscribeCommsResponse) []uint64 {
	seqs := make([]uint64, len(frames))
	for i, f := range frames {
		seqs[i] = f.GetSeq()
	}
	return seqs
}
