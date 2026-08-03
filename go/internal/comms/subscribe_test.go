//go:build pgtest

package comms

// SubscribeComms stream contracts, driven end-to-end over a real connect
// server-stream: a post fans out to a live subscriber as MessagePosted, and a
// reconnect with a stale cursor (a prior server instance's epoch) collapses to a
// terminal CommsResyncRequired so the client re-snapshots from Postgres via
// ListMessages — deduped by id. Mutations are driven in-process on the SAME
// handler the stream server holds (so they share one bus and one store), while
// the stream is consumed through the generated connect client.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// streamHarness is a running handler: the same *Comms is both mounted on a
// connect httptest server (for the SubscribeComms stream) and callable directly
// in-process (for mutations, so a test controls the caller via WithActor). Both
// paths share the one store and the one bus.
type streamHarness struct {
	svc    *Comms
	store  *store.Store
	bus    *events.Bus[*compassv1.SubscribeCommsResponse]
	client compassv1connect.CommsServiceClient
}

// commsTestActorHeader carries the acting account id from a test client to the
// server-side handler, where withActorHeader translates it into the same
// context value the T3 token interceptor would set (comms.WithActor). Over an
// httptest server there is no interceptor, so without this every RPC would fall
// back to the bootstrap admin — and the D9 stream filter would then evaluate
// visibility for the admin, not the account the test means to act as. This is
// the test-only stand-in for that interceptor, letting one harness drive a
// stream as a member (A) or a non-member (B).
const commsTestActorHeader = "X-Test-Actor"

// withActorHeader wraps a handler so a request carrying commsTestActorHeader is
// attributed to that account (via comms.WithActor on the request context),
// mirroring the T3 token interceptor. A request without the header keeps the
// socket-door admin fallback.
func withActorHeader(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if actor := r.Header.Get(commsTestActorHeader); actor != "" {
			r = r.WithContext(WithActor(r.Context(), store.AccountID(actor)))
		}
		h.ServeHTTP(w, r)
	})
}

// newStreamHarness wires a store, a bus, and a Comms handler, mounts it on an
// httptest server, and returns a client plus the in-process handle. adminID is a
// bootstrapped admin so the socket-door fallback attributes to a real account;
// a request carrying commsTestActorHeader overrides it (withActorHeader), so a
// stream can be driven as any account.
func newStreamHarness(t *testing.T) streamHarness {
	t.Helper()
	st := newTestStore(t)
	bus := newBus(t)
	admin, err := st.BootstrapAdmin(context.Background(), store.NewUser{Handle: "root", DisplayName: "Root"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	svc := NewComms(st, bus, admin.ID)

	path, handler := compassv1connect.NewCommsServiceHandler(svc)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(withActorHeader(mux))
	t.Cleanup(srv.Close)

	client := compassv1connect.NewCommsServiceClient(srv.Client(), srv.URL)
	return streamHarness{svc: svc, store: st, bus: bus, client: client}
}

// firstEvent carries the first delivered stream response, or the error that
// ended the stream first.
type firstEvent struct {
	msg *compassv1.SubscribeCommsResponse
	err error
}

// subscribeFirst opens a SubscribeComms stream AS actor in a background
// goroutine and returns a channel yielding the first delivered event. The actor
// is carried on commsTestActorHeader so the server-side D9 filter evaluates
// visibility for that account (empty actor keeps the admin fallback). connect
// server-streaming over HTTP/1 is half-duplex — the client SubscribeComms call
// blocks until the server flushes its first frame — so the subscribe MUST run
// concurrently with the mutation that triggers the event, not before it.
// sinceSeq=0 snapshots the ring under the same lock that registers the live
// subscriber (events.go Subscribe), so an event published concurrently is
// delivered exactly once (replay or live) with no gap and no sleep —
// deterministic gating.
func subscribeFirst(t *testing.T, h streamHarness, actor store.AccountID, req *compassv1.SubscribeCommsRequest) <-chan firstEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	out := make(chan firstEvent, 1)
	connectReq := connect.NewRequest(req)
	if actor != "" {
		connectReq.Header().Set(commsTestActorHeader, string(actor))
	}
	go func() {
		stream, err := h.client.SubscribeComms(ctx, connectReq)
		if err != nil {
			out <- firstEvent{err: err}
			return
		}
		if !stream.Receive() {
			out <- firstEvent{err: stream.Err()}
			return
		}
		out <- firstEvent{msg: stream.Msg()}
	}()
	return out
}

// firstEventAfterBoundary opens a since_seq=0 stream, consumes the leading
// snapshot-boundary control frame (Seq=0, no payload), and returns the first
// real EVENT frame. Use it wherever a test drives a since_seq=0 subscribe and
// asserts on the first positioned event. It mirrors subscribeFirst's
// goroutine+channel half-duplex shape (the subscribe runs concurrently with the
// mutation that triggers the event) but calls stream.Receive() twice: the first
// frame MUST be the boundary (Seq==0 && no payload — a misdelivered first frame
// is a real bug, so fail loudly), then the second frame is returned. Sleep-free
// and deterministic; no boundary frame is ever sent on since_seq>0, so only pass
// a since_seq=0 request here.
func firstEventAfterBoundary(t *testing.T, h streamHarness, actor store.AccountID, req *compassv1.SubscribeCommsRequest) <-chan firstEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	out := make(chan firstEvent, 1)
	connectReq := connect.NewRequest(req)
	if actor != "" {
		connectReq.Header().Set(commsTestActorHeader, string(actor))
	}
	go func() {
		stream, err := h.client.SubscribeComms(ctx, connectReq)
		if err != nil {
			out <- firstEvent{err: err}
			return
		}
		if !stream.Receive() {
			out <- firstEvent{err: stream.Err()}
			return
		}
		if boundary := stream.Msg(); boundary.GetSeq() != 0 || boundary.GetPayload() != nil {
			t.Errorf("first frame = seq %d payload %T, want the snapshot-boundary frame (seq 0, no payload)", boundary.GetSeq(), boundary.GetPayload())
			out <- firstEvent{err: fmt.Errorf("first frame was not the snapshot boundary: seq %d payload %T", boundary.GetSeq(), boundary.GetPayload())}
			return
		}
		if !stream.Receive() {
			out <- firstEvent{err: stream.Err()}
			return
		}
		out <- firstEvent{msg: stream.Msg()}
	}()
	return out
}

// awaitFirst blocks for the first stream event or fails after the deadline. The
// timeout only fires on a genuine miss; a correctly-fanned event arrives at once.
func awaitFirst(t *testing.T, ch <-chan firstEvent) *compassv1.SubscribeCommsResponse {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("stream ended before a message arrived: %v", r.err)
		}
		return r.msg
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a stream message")
		return nil
	}
}

// TestSubscribeCommsSnapshotBoundaryFirstFrame proves the since_seq=0 subscribe
// response carries a leading snapshot_seq boundary frame (comms.proto:353-368,
// design.md:807-817) before any event, including the empty-ring case. The
// boundary frame is the server half of gap-free resync: Seq=0 with no payload,
// stamped with the current instance epoch and the store-space head the client
// passes to each catch-up read.
func TestSubscribeCommsSnapshotBoundaryFirstFrame(t *testing.T) {
	// Populated store: two posted messages advance the message head, so the
	// boundary frame's SnapshotSeq is a positive value the client can page from.
	t.Run("non_empty_ring", func(t *testing.T) {
		h := newStreamHarness(t)
		ctx := context.Background()

		poster := mustUser(t, h.store, "poster")
		ch, err := h.store.CreateChannel(ctx, poster.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
		if err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}
		const k = 2
		for i := range k {
			if _, err := h.svc.PostMessage(WithActor(ctx, poster.ID), connect.NewRequest(&compassv1.PostMessageRequest{
				Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
				Blocks:    []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "seeded"}}},
			})); err != nil {
				t.Fatalf("PostMessage(%d): %v", i, err)
			}
		}

		// The head is read AFTER the posts, so it reflects them; assert on the
		// relationship (frame == store head), not a raw literal, since other
		// harness rows may share the sequence.
		head, err := h.store.MessagesHeadSeq(ctx)
		if err != nil {
			t.Fatalf("MessagesHeadSeq: %v", err)
		}
		if head == 0 {
			t.Fatalf("store head = 0 after posting %d messages, want a positive snapshot boundary", k)
		}

		// The store already holds the posts, so the boundary frame is flushed
		// immediately as the first frame — no concurrent mutation needed.
		got := awaitFirst(t, subscribeFirst(t, h, poster.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0}))
		if got.GetSeq() != 0 {
			t.Fatalf("boundary frame seq = %d, want 0", got.GetSeq())
		}
		if got.GetPayload() != nil {
			t.Fatalf("boundary frame payload = %T, want none (no event carried)", got.GetPayload())
		}
		if got.GetMessagePosted() != nil {
			t.Fatalf("boundary frame carried a MessagePosted, want the bare boundary")
		}
		if got.GetResyncRequired() != nil {
			t.Fatalf("boundary frame carried a CommsResyncRequired, want the bare boundary (not a resync)")
		}
		if got.GetSnapshotSeq() != head {
			t.Fatalf("boundary SnapshotSeq = %d, want the store head %d", got.GetSnapshotSeq(), head)
		}
		if got.GetInstanceEpoch() != h.bus.InstanceEpoch() {
			t.Fatalf("boundary InstanceEpoch = %d, want the current bus epoch %d", got.GetInstanceEpoch(), h.bus.InstanceEpoch())
		}
	})

	// Empty ring: no message posted, so the store head is 0. This is the
	// load-bearing case — it proves the boundary is delivered even when there is
	// no event to carry it, which is exactly the reconnect a client must page
	// from without a live post to piggyback on.
	t.Run("empty_ring", func(t *testing.T) {
		h := newStreamHarness(t)
		ctx := context.Background()

		subscriber := mustUser(t, h.store, "subscriber")

		head, err := h.store.MessagesHeadSeq(ctx)
		if err != nil {
			t.Fatalf("MessagesHeadSeq: %v", err)
		}
		if head != 0 {
			t.Fatalf("store head = %d on a fresh harness with no messages, want 0", head)
		}

		got := awaitFirst(t, subscribeFirst(t, h, subscriber.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0}))
		if got.GetSeq() != 0 {
			t.Fatalf("empty-ring boundary frame seq = %d, want 0", got.GetSeq())
		}
		if got.GetPayload() != nil {
			t.Fatalf("empty-ring boundary frame payload = %T, want none", got.GetPayload())
		}
		if got.GetSnapshotSeq() != 0 {
			t.Fatalf("empty-ring boundary SnapshotSeq = %d, want 0", got.GetSnapshotSeq())
		}
		if got.GetInstanceEpoch() != h.bus.InstanceEpoch() {
			t.Fatalf("empty-ring boundary InstanceEpoch = %d, want the current bus epoch %d", got.GetInstanceEpoch(), h.bus.InstanceEpoch())
		}
	})
}

func TestSubscribeCommsPostDeliversMessagePosted(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()

	poster := mustUser(t, h.store, "poster")
	ch, err := h.store.CreateChannel(ctx, poster.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	// Open the subscription concurrently AS the poster (a channel member, so the
	// D9 filter delivers the post to it), then post — half-duplex demands the
	// subscribe not block the post.
	events := firstEventAfterBoundary(t, h, poster.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})

	posted, err := h.svc.PostMessage(WithActor(ctx, poster.ID), connect.NewRequest(&compassv1.PostMessageRequest{
		Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
		Blocks:    []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "hello stream"}}},
	}))
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	wantID := posted.Msg.GetMessage().GetId()

	got := awaitFirst(t, events)
	mp := got.GetMessagePosted()
	if mp == nil {
		t.Fatalf("first event payload = %T, want MessagePosted", got.GetPayload())
	}
	if mp.GetMessage().GetId() != wantID {
		t.Fatalf("delivered message id = %q, want the posted %q", mp.GetMessage().GetId(), wantID)
	}
	if blocks := mp.GetMessage().GetBlocks(); len(blocks) != 1 || blocks[0].GetText() != "hello stream" {
		t.Fatalf("delivered blocks = %+v, want one text block 'hello stream'", blocks)
	}
}

// TestSubscribeCommsAgentPresenceSharedChannelScoping is the SEA-1569 T8
// visibility arm: an AgentPresenceChanged is delivered to an actor sharing a
// visible channel with the agent and filtered from an actor sharing none — the
// shared-channel rule the subscribe edge enforces (design.md:487-491). The
// event is published straight onto the bus (the presence publisher's arm), so
// this test drives the subscribe-edge filter directly.
func TestSubscribeCommsAgentPresenceSharedChannelScoping(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()

	owner := mustUser(t, h.store, "owner")
	agent := mustAgent(t, h.store, owner.ID, "agent")
	sharer := mustUser(t, h.store, "sharer")
	stranger := mustUser(t, h.store, "stranger")

	// A channel co-inhabited by the agent and the sharer; the stranger is not a
	// member, so it shares no channel with the agent.
	if _, err := h.store.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name: "shared", Kind: store.ChannelKindChannel,
		MemberAccountIDs: []store.AccountID{agent.ID, sharer.ID},
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	presence := &compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_AgentPresenceChanged{
			AgentPresenceChanged: &compassv1.AgentPresenceChanged{
				AgentAccountId: string(agent.ID),
				Presence:       compassv1.AgentPresence_AGENT_PRESENCE_WAITING,
			},
		},
	}

	// The sharer receives the agent's presence (shares a channel with it).
	sharerEvents := firstEventAfterBoundary(t, h, sharer.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})
	h.bus.Publish(presence)
	got := awaitFirst(t, sharerEvents)
	pc := got.GetAgentPresenceChanged()
	if pc == nil {
		t.Fatalf("sharer first event = %T, want AgentPresenceChanged", got.GetPayload())
	}
	if pc.GetAgentAccountId() != string(agent.ID) || pc.GetPresence() != compassv1.AgentPresence_AGENT_PRESENCE_WAITING {
		t.Fatalf("sharer presence = %+v, want {%s, WAITING}", pc, agent.ID)
	}

	// The stranger shares no channel with the agent: the presence is filtered, so
	// the stranger sees only the snapshot boundary and then nothing. A concurrent
	// post to a channel the stranger CAN see is the positive control that proves
	// the stream is live and the presence was filtered, not merely delayed.
	strangerCh, err := h.store.CreateChannel(ctx, stranger.ID, store.NewChannel{Name: "stranger-room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel(stranger): %v", err)
	}
	strangerEvents := firstEventAfterBoundary(t, h, stranger.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})
	h.bus.Publish(presence)
	posted, err := h.svc.PostMessage(WithActor(ctx, stranger.ID), connect.NewRequest(&compassv1.PostMessageRequest{
		Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(strangerCh.ID)},
		Blocks:    []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "visible to me"}}},
	}))
	if err != nil {
		t.Fatalf("PostMessage(stranger): %v", err)
	}
	first := awaitFirst(t, strangerEvents)
	if first.GetAgentPresenceChanged() != nil {
		t.Fatalf("stranger received AgentPresenceChanged for an agent it shares no channel with; want it filtered")
	}
	if mp := first.GetMessagePosted(); mp == nil || mp.GetMessage().GetId() != posted.Msg.GetMessage().GetId() {
		t.Fatalf("stranger first event = %T, want its own MessagePosted (the presence must have been filtered, not delayed)", first.GetPayload())
	}
}

// TestPostMessageIdempotentRetrySuppressesDuplicatePublish is the M3 handler
// regression: a first PostMessage carrying a client_request_id delivers exactly
// one MessagePosted, and a SECOND PostMessage with the SAME id returns the
// stored message (idempotent dedup) but publishes NO second MessagePosted — the
// subscriber sees the post exactly once. The handler now publishes only when the
// store reports inserted=true; pre-fix it re-fanned MessagePosted on every
// retry, so a duplicate would appear on the stream. The exactly-once proof is
// deterministic and sleep-free: the retry's publish (if any) happens
// synchronously before the globally-visible canary is created, so draining the
// replay up to the canary yields the complete set of MessagePosted the channel
// ever emitted.
func TestPostMessageIdempotentRetrySuppressesDuplicatePublish(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()

	poster := mustUser(t, h.store, "poster")
	ch, err := h.store.CreateChannel(ctx, poster.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	// Live path: subscribe concurrently (half-duplex), then the first post
	// delivers exactly one MessagePosted to the member.
	events := firstEventAfterBoundary(t, h, poster.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})
	first, err := h.svc.PostMessage(WithActor(ctx, poster.ID), connect.NewRequest(&compassv1.PostMessageRequest{
		Container:       &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
		Blocks:          []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "only once"}}},
		ClientRequestId: "req-dup",
	}))
	if err != nil {
		t.Fatalf("PostMessage(first): %v", err)
	}
	wantID := first.Msg.GetMessage().GetId()
	got := awaitFirst(t, events)
	if mp := got.GetMessagePosted(); mp == nil || mp.GetMessage().GetId() != wantID {
		t.Fatalf("first live event = %+v, want MessagePosted for %q", got.GetPayload(), wantID)
	}

	// The retry with the SAME id returns the already-stored message (dedup), not
	// a fresh one — the store deduped and the handler must not re-publish.
	retry, err := h.svc.PostMessage(WithActor(ctx, poster.ID), connect.NewRequest(&compassv1.PostMessageRequest{
		Container:       &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
		Blocks:          []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "different body, same id"}}},
		ClientRequestId: "req-dup",
	}))
	if err != nil {
		t.Fatalf("PostMessage(retry): %v", err)
	}
	if got := retry.Msg.GetMessage().GetId(); got != wantID {
		t.Fatalf("retry returned message id %q, want the stored %q", got, wantID)
	}

	// Exactly-once proof: create a globally-visible canary (published strictly
	// after any retry publish), then drain the replay up to it. The channel must
	// have emitted exactly ONE MessagePosted across the first post and its retry.
	canary := mkCanary(t, h, "canary")
	evts := drainReplayAsActor(t, h, poster.ID, canary)
	var posted int
	for _, c := range messagePostedChannels(evts) {
		if c == string(ch.ID) {
			posted++
		}
	}
	if posted != 1 {
		t.Fatalf("channel emitted %d MessagePosted across a post + idempotent retry, want exactly 1", posted)
	}
}

func TestPostMessageWriteThrough(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()

	poster := mustUser(t, h.store, "poster")
	ch, err := h.store.CreateChannel(ctx, poster.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	events := firstEventAfterBoundary(t, h, poster.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})

	posted, err := h.svc.PostMessage(WithActor(ctx, poster.ID), connect.NewRequest(&compassv1.PostMessageRequest{
		Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
		Blocks:    []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "durable and fanned"}}},
	}))
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	wantID := posted.Msg.GetMessage().GetId()

	// Write-through means BOTH sides observe one post: the store row is durable
	// (read as the poster, a member — the store scopes to the actor)...
	listed, err := h.svc.ListMessages(WithActor(ctx, poster.ID), connect.NewRequest(&compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: string(ch.ID)},
	}))
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if msgs := listed.Msg.GetMessages(); len(msgs) != 1 || msgs[0].GetId() != wantID {
		t.Fatalf("store rows = %+v, want exactly the posted message %q", msgs, wantID)
	}
	// ...and the event fired.
	got := awaitFirst(t, events)
	if mp := got.GetMessagePosted(); mp == nil || mp.GetMessage().GetId() != wantID {
		t.Fatalf("event = %+v, want MessagePosted for %q", got.GetPayload(), wantID)
	}
}

func TestSubscribeCommsStaleEpochResyncsAndRedelivers(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()

	poster := mustUser(t, h.store, "poster")
	ch, err := h.store.CreateChannel(ctx, poster.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	// Post one message so there is a real store row to re-snapshot.
	posted, err := h.svc.PostMessage(WithActor(ctx, poster.ID), connect.NewRequest(&compassv1.PostMessageRequest{
		Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
		Blocks:    []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "before restart"}}},
	}))
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	wantID := posted.Msg.GetMessage().GetId()

	// A reconnect carrying a POSITIONED cursor from a prior instance (a stale
	// epoch — simulating a server restart with a fresh instance_epoch) cannot be
	// served gap-free, so the stream delivers a single terminal
	// CommsResyncRequired stamped with the current epoch. The server flushes
	// this frame immediately, so the subscribe returns without a concurrent
	// mutation — but subscribeFirst keeps the half-duplex handling uniform.
	staleEpoch := h.bus.InstanceEpoch() + 1
	got := awaitFirst(t, subscribeFirst(t, h, poster.ID, &compassv1.SubscribeCommsRequest{
		SinceSeq: 1, InstanceEpoch: staleEpoch,
	}))
	if got.GetResyncRequired() == nil {
		t.Fatalf("stale-cursor first event = %T, want CommsResyncRequired", got.GetPayload())
	}
	if got.GetInstanceEpoch() != h.bus.InstanceEpoch() {
		t.Fatalf("resync epoch = %d, want the current instance epoch %d", got.GetInstanceEpoch(), h.bus.InstanceEpoch())
	}

	// The client responds to the resync by re-snapshotting from Postgres via
	// ListMessages (as the poster, a member): the message appears exactly once,
	// deduped by id.
	listed, err := h.svc.ListMessages(WithActor(ctx, poster.ID), connect.NewRequest(&compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: string(ch.ID)},
	}))
	if err != nil {
		t.Fatalf("re-snapshot ListMessages: %v", err)
	}
	msgs := listed.Msg.GetMessages()
	if len(msgs) != 1 {
		t.Fatalf("re-snapshot returned %d messages, want exactly 1 (deduped)", len(msgs))
	}
	if msgs[0].GetId() != wantID {
		t.Fatalf("re-snapshot message id = %q, want %q", msgs[0].GetId(), wantID)
	}
}
