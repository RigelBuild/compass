package comms

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// commsResyncSeq is the seq on a CommsResyncRequired: a control signal, not a
// positioned event. The client discards its cursor and reconnects at
// since_seq = 0, so the resync carries no meaningful position. Mirrors the
// CompassService SubscribeEvents resync (server/service.go:28-31).
const commsResyncSeq uint64 = 0

// SubscribeComms snapshots the comms event ring then tails live updates until
// the client disconnects, the server shuts down, or the subscriber lags past
// the ring — the lag case emits a final CommsResyncRequired so the client
// re-snapshots from Postgres via the read RPCs (deduping by message id).
//
// It mirrors CompassService.SubscribeEvents exactly (server/service.go:70-89):
// connect-go drives the stream inside this handler, blocking until it returns;
// both the replay drain and the live tail select on ctx (cancelled on client
// hang-up), and the bus closing its Live channel is what ends a held-open stream
// on server shutdown.
func (c *Comms) SubscribeComms(
	ctx context.Context,
	req *connect.Request[compassv1.SubscribeCommsRequest],
	stream *connect.ServerStream[compassv1.SubscribeCommsResponse],
) error {
	sub, err := c.bus.Subscribe(req.Msg.GetSinceSeq(), req.Msg.GetInstanceEpoch())
	if err != nil {
		if errors.Is(err, events.ErrBufferUnderflow) {
			// Cursor can't be served gap-free: a single terminal resync so the
			// client re-snapshots at since_seq = 0.
			return stream.Send(commsResyncRequired(c.bus.InstanceEpoch()))
		}
		return connect.NewError(connect.CodeInternal, err)
	}
	// Free the fan-out slot on every exit path (hang-up, lag, shutdown, send
	// error). Cancel is idempotent, safe after the bus already closed the channel.
	defer sub.Cancel()
	actor := c.actorFromContext(ctx)

	// since_seq=0 recovery gets a leading snapshot-boundary frame before any
	// event: it carries the store-space snapshot_seq the client passes to each
	// catch-up read RPC so every page reads one point-in-time view
	// (comms.proto:353-368, design.md:807-817). The subscriber is already
	// registered (bus.Subscribe above), so capturing the store head here is
	// subscribe-first: a message committing in this window lands on the live
	// tail rather than falling between the snapshot and the tail. The boundary
	// is sent unconditionally on since_seq=0 — including the empty-ring case,
	// where no event frame exists to carry it — so a client on a quiet channel
	// still learns the boundary instead of defaulting to 0 (no boundary). A
	// positioned resubscribe (since_seq>0) already holds state and tails from
	// its own cursor, so it needs no fresh boundary.
	// The boundary carries the instance-global store head (MessagesHeadSeq,
	// messages.go:129) and is sent before per-event visibility filtering, so any
	// authenticated subscriber — including a non-member of a private channel —
	// learns the instance-wide durable message count (one monotonic integer, no
	// content, author, or channel identity) from frame 1. This is the established
	// contract's ratified shape: a single instance-wide, store-space snapshot_seq
	// token that survives restarts and covers the empty-ring bootstrap
	// (design.md:809-816). A visibility-scoped boundary is a different token with
	// a different meaning; the count-metadata exposure is accepted as within the
	// threat model (RIG-1333 OQ4, Matt's ruling).
	if req.Msg.GetSinceSeq() == 0 {
		head, err := c.store.MessagesHeadSeq(ctx)
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		if err := stream.Send(commsSnapshotBoundary(head, sub.Epoch)); err != nil {
			return nil // client hung up before the first frame: a clean end
		}
	}
	if err := forwardComms(ctx, actor, c.store, sub, stream); err != nil {
		// A store error resolving per-event visibility: the stream ends rather
		// than failing open (the event is never sent unfiltered), but the client
		// must see a fault, not a clean EOF indistinguishable from shutdown, and
		// the fault must be diagnosable. The underlying error is logged, not
		// returned to the client, so the unfiltered event's existence never
		// leaks through an error message.
		slog.ErrorContext(ctx, "comms stream ended: visibility resolution failed",
			"actor", actor, "error", err)
		return connect.NewError(connect.CodeInternal, errStreamVisibility)
	}
	return nil
}

// errStreamVisibility is the client-facing error a SubscribeComms stream ends
// with when per-event visibility resolution hits a store fault. It carries no
// detail about the event being filtered — the diagnostic goes to the server log,
// never the client — so the fault is distinguishable from a clean end without
// leaking what could not be resolved.
var errStreamVisibility = errors.New("visibility resolution failed")

// forwardComms drains the replay snapshot (oldest first), then forwards the live
// tail until the client disconnects, the server shuts down, or the subscriber
// lags past the ring. The lag case emits a final CommsResyncRequired. Both
// phases select on ctx so a shutdown or hang-up mid-replay returns promptly
// rather than stalling graceful drain.
//
// Every event is filtered by actor visibility before it is sent (D9: the
// SubscribeComms fan-out is visibility-scoped, design.md:446-447): the shared
// bus fans every event to every subscriber, so a non-member would otherwise
// receive private-channel MessagePosted/MessageUpdated content. A non-visible
// event is silently skipped (not a resync — the client is not lagging, it just
// may not see that event). A visibility-check store error ends the stream rather
// than failing open (the event is never sent unfiltered) and is returned so the
// caller surfaces it as a fault; a client hang-up or a stream.Send failure ends
// the stream cleanly (nil), indistinguishable from graceful shutdown as it
// should be.
func forwardComms(
	ctx context.Context,
	actor store.AccountID,
	vis eventVisibility,
	sub events.Subscription[*compassv1.SubscribeCommsResponse],
	stream *connect.ServerStream[compassv1.SubscribeCommsResponse],
) error {
	// send reports (visErr, ok): visErr is a store fault resolving visibility —
	// propagate it so the caller surfaces a fault. ok is false on a clean end —
	// a stream.Send failure (client hung up) or a cancellation racing the
	// in-flight visibility query (client gone / server shutting down); the
	// caller stops the loop and returns cleanly, since neither is a fault. The
	// two are kept distinct so a hang-up never masquerades as a fault and a
	// fault never masquerades as a clean end.
	send := func(event events.Stamped[*compassv1.SubscribeCommsResponse]) (visErr error, ok bool) {
		visible, err := visibleToActor(ctx, vis, actor, event.Payload)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				// Cancellation raced the in-flight visibility query: the client
				// hung up or the server is shutting down. End cleanly — a
				// cancellation is a graceful end, not a store fault, so it must
				// not surface as ERROR + CodeInternal (the M5 clean-end contract).
				return nil, false
			}
			return err, false // store error resolving visibility: end the stream, never fail open
		}
		if !visible {
			return nil, true // not the actor's to see: skip, not a resync
		}
		// A stream.Send failure means the client hung up: end cleanly (ok=false),
		// never a fault. Expressed as ok = (send succeeded) so the client-side
		// end is not a `return nil` on a non-nil error (which is a real hang-up,
		// deliberately swallowed, not a fault to surface).
		return nil, stream.Send(commsToResponse(event)) == nil
	}

	for _, event := range sub.Replay {
		select {
		case <-ctx.Done():
			return nil // client hung up or server shutting down mid-replay
		default:
		}
		visErr, ok := send(event)
		if visErr != nil {
			return visErr
		}
		if !ok {
			return nil // client hung up mid-replay: end the stream cleanly
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil // client hung up or server shutting down
		case event, ok := <-sub.Live:
			if !ok {
				// Live closed: an overrun (emit terminal resync) vs a clean bus
				// shutdown (end silently).
				if sub.Lagged() {
					_ = stream.Send(commsResyncRequired(sub.Epoch))
				}
				return nil
			}
			visErr, delivered := send(event)
			if visErr != nil {
				return visErr
			}
			if !delivered {
				return nil // client hung up: end the stream cleanly
			}
		}
	}
}

// commsToResponse maps the bus's Stamped envelope onto the concrete
// SubscribeComms response at the stream edge: seq/at_unix_ms/instance_epoch
// transfer from the envelope and the payload oneof comes from the stamped
// message (mirrors server/service.go:135-142).
func commsToResponse(event events.Stamped[*compassv1.SubscribeCommsResponse]) *compassv1.SubscribeCommsResponse {
	return &compassv1.SubscribeCommsResponse{
		Seq:           event.Seq,
		AtUnixMs:      event.AtUnixMS,
		InstanceEpoch: event.InstanceEpoch,
		Payload:       event.Payload.GetPayload(),
	}
}

// commsResyncRequired is the typed resync signal: the last event the server
// sends before closing a stream whose cursor it can no longer serve gap-free.
func commsResyncRequired(instanceEpoch uint64) *compassv1.SubscribeCommsResponse {
	return &compassv1.SubscribeCommsResponse{
		Seq:           commsResyncSeq,
		InstanceEpoch: instanceEpoch,
		Payload: &compassv1.SubscribeCommsResponse_ResyncRequired{
			ResyncRequired: &compassv1.CommsResyncRequired{},
		},
	}
}

// commsSnapshotBoundary is the leading control frame a since_seq=0 subscribe
// receives before any event: it carries the store-space snapshot_seq boundary
// (comms.proto:353-368) the client passes to each catch-up read RPC. It is a
// control frame, not a positioned event — Seq=0, no payload — so a client
// discriminates it from a positioned event by its zero seq and absent payload,
// and from the terminal resync (also Seq=0) by payload: this frame has none,
// the resync carries a CommsResyncRequired. instance_epoch is the subscription's
// epoch so a client can pair the boundary with the seq space it belongs to.
func commsSnapshotBoundary(snapshotSeq, instanceEpoch uint64) *compassv1.SubscribeCommsResponse {
	return &compassv1.SubscribeCommsResponse{
		Seq:           commsResyncSeq,
		InstanceEpoch: instanceEpoch,
		SnapshotSeq:   snapshotSeq,
	}
}

// eventVisibility is the store surface the stream edge consults to decide
// whether an event is visible to a subscriber. Each method is the single-id form
// of a List* read predicate, so the per-event stream filter rides at exact
// read-parity — the store is the one D9 source of truth and the two cannot drift
// (the frozen record's anti-drift requirement). *store.Store satisfies it; a
// test can substitute a fake to drive the filter without a database.
type eventVisibility interface {
	// IsTopicChannelMember gates MessagePosted/MessageUpdated: a wire message
	// carries only its topic, so the channel is resolved through topics.channel_id
	// (the frozen record's topic->channel resolution), keeping the per-event
	// filter at read-parity with ListMessages (a channel_members JOIN on the
	// topic's channel).
	IsTopicChannelMember(ctx context.Context, actor store.AccountID, topicID string) (bool, error)
	// IsChannelMember gates TopicUpserted: a topic carries its channel_id, so a
	// topic event reaches only members of its channel (read-parity with
	// ListTopics).
	IsChannelMember(ctx context.Context, actor store.AccountID, channelID store.ChannelID) (bool, error)
	// ChannelVisibleTo gates ChannelChanged (ListChannels: member OR
	// SHARED-grouped — bare membership would wrongly drop a SHARED channel's
	// change from a non-member viewer).
	ChannelVisibleTo(ctx context.Context, actor store.AccountID, channelID store.ChannelID) (bool, error)
	// ChannelGroupVisibleTo gates ChannelGroupChanged (ListChannelGroups: SHARED
	// effective visibility OR owner/agent-of-owner).
	ChannelGroupVisibleTo(ctx context.Context, actor store.AccountID, groupID store.ChannelGroupID) (bool, error)
	// AccountVisibleTo gates AccountChanged (ListAccounts: self + all users +
	// owned/co-member agents).
	AccountVisibleTo(ctx context.Context, actor store.AccountID, target store.AccountID) (bool, error)
	// IsAgentWorkspaceVisible gates AgentWorkspaceChanged (OpenAgentWorkspace:
	// home-channel membership).
	IsAgentWorkspaceVisible(ctx context.Context, actor store.AccountID, agentAccountID store.AccountID) (bool, error)
	// SharesVisibleChannel gates AgentPresenceChanged (RIG-1569 T8): the actor
	// receives an agent's presence only when it shares at least one channel with
	// that agent — the shared-channel rule matching the fan-out's per-actor
	// scoping (design.md:487-491).
	SharesVisibleChannel(ctx context.Context, actor store.AccountID, agent store.AccountID) (bool, error)
}

// visibleToActor reports whether actor may receive resp — the per-event D9 gate
// the stream applies to the shared fan-out (design.md:446-447). The bus fans
// every event to every subscriber, so each variant is filtered by the SAME
// predicate its List* read uses (via eventVisibility), never a uniform
// membership check: a uniform check would leak nothing but would wrongly starve
// a subscriber of ChannelChanged/ChannelGroupChanged/AccountChanged for
// channels/groups/accounts it can legitimately see without plain membership.
// ResyncRequired and any unset payload are control frames with no private
// content and always pass (also guaranteed by construction: the resync sends at
// the stream edge are synthesized, never bus events, so they never reach here).
func visibleToActor(
	ctx context.Context,
	vis eventVisibility,
	actor store.AccountID,
	resp *compassv1.SubscribeCommsResponse,
) (bool, error) {
	payload := resp.GetPayload()
	if payload == nil {
		// An unset payload is a control frame with no private content.
		return true, nil
	}
	switch p := payload.(type) {
	case *compassv1.SubscribeCommsResponse_MessagePosted:
		return vis.IsTopicChannelMember(ctx, actor, p.MessagePosted.GetMessage().GetTopicId())
	case *compassv1.SubscribeCommsResponse_MessageUpdated:
		return vis.IsTopicChannelMember(ctx, actor, p.MessageUpdated.GetMessage().GetTopicId())
	case *compassv1.SubscribeCommsResponse_ChannelChanged:
		// A member this change removed is no longer in the channel's set, so it
		// would never see its own removal — deliver this one final event to a
		// departed account too (Matt's ruling), then it goes silent to them.
		// The event carries the channel's post-mutation roster, so a single
		// batch that both adds X and removes Y reveals newly-added X to departing
		// Y (a state Y was never a member alongside). Accepted for T2: a minor,
		// same-batch-only roster exposure to an account that was a member moments
		// before; closing it would require a per-removed-member before-image.
		// Otherwise gate on full channel visibility (member OR SHARED-grouped),
		// not bare membership, so a SHARED channel's change reaches a non-member
		// viewer at read-parity with ListChannels.
		cc := p.ChannelChanged
		for _, id := range cc.GetRemovedAccountIds() {
			if store.AccountID(id) == actor {
				return true, nil
			}
		}
		return vis.ChannelVisibleTo(ctx, actor, store.ChannelID(cc.GetChannel().GetId()))
	case *compassv1.SubscribeCommsResponse_ChannelGroupChanged:
		return vis.ChannelGroupVisibleTo(ctx, actor, store.ChannelGroupID(p.ChannelGroupChanged.GetGroup().GetId()))
	case *compassv1.SubscribeCommsResponse_AccountChanged:
		return vis.AccountVisibleTo(ctx, actor, store.AccountID(p.AccountChanged.GetAccount().GetId()))
	case *compassv1.SubscribeCommsResponse_AgentWorkspaceChanged:
		return vis.IsAgentWorkspaceVisible(ctx, actor, store.AccountID(p.AgentWorkspaceChanged.GetWorkspace().GetAgentAccountId()))
	case *compassv1.SubscribeCommsResponse_AgentPresenceChanged:
		return vis.SharesVisibleChannel(ctx, actor, store.AccountID(p.AgentPresenceChanged.GetAgentAccountId()))
	case *compassv1.SubscribeCommsResponse_TopicUpserted:
		// A topic event only reaches members of its channel: the topic carries
		// its channel_id, so gate on plain channel membership (read-parity with
		// ListTopics, which requires the caller be a member of the channel).
		return vis.IsChannelMember(ctx, actor, store.ChannelID(p.TopicUpserted.GetTopic().GetChannelId()))
	default:
		// ResyncRequired and any unset payload: control frames, always delivered.
		return true, nil
	}
}
