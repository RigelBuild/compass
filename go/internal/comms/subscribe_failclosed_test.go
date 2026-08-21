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

package comms

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
