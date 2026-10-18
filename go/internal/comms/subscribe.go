package comms

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"connectrpc.com/connect/v2"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
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
	req *compassv1.SubscribeCommsRequest,
	stream compassv1connect.CommsServiceSubscribeCommsServerStream,
) error {
	ctx = otelx.WithoutSpan(ctx)
	sub, err := c.bus.Subscribe(req.GetSinceSeq(), req.GetInstanceEpoch())
	if err != nil {
		if errors.Is(err, events.ErrBufferUnderflow) {
			// Cursor can't be served gap-free: a single terminal resync so the
			// client re-snapshots at since_seq = 0.
			return stream.Send(commsResyncRequired(c.bus.InstanceEpoch()))
		}
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	// Free the fan-out slot on every exit path (hang-up, lag, shutdown, send
	// error). Cancel is idempotent, safe after the bus already closed the channel.
	defer sub.Cancel()
	actor := c.actorFromContext(ctx)

	// since_seq=0 recovery gets a leading snapshot-boundary frame carrying the
	// store-space snapshot_seq for each catch-up read. Subscribe-first so a message
	// in this window lands on the live tail. The boundary carries the store head
	// BEFORE visibility filtering — the count leak is within the threat model (OQ4).
	if req.GetSinceSeq() == 0 {
		head, err := c.store.MessagesHeadSeq(ctx)
		if err != nil {
			return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
		}
		if err := stream.Send(commsSnapshotBoundary(head, sub.Epoch)); err != nil {
			return nil // client hung up before the first frame: a clean end
		}
	}
	if err := forwardComms(ctx, actor, storeEventVisibility{Store: c.store, boardVisibleFn: c.boardVisible}, sub, stream); err != nil {
		// A store error resolving per-event visibility: the stream ends rather than
		// failing open (the event is never sent unfiltered), but the client must see
		// a fault, not a clean EOF. The underlying error is logged, not returned, so
		// the unfiltered event's existence never leaks through an error message.
		slog.ErrorContext(ctx, "comms stream ended: visibility resolution failed",
			"actor", actor, "error", err)
		return connect.NewError(connect.CodeInternal, errStreamVisibility.Error()).WithCause(errStreamVisibility)
	}
	return nil
}

// errStreamVisibility ends a stream when per-event visibility lookup fails.
// Details stay in server logs so the event being filtered is not exposed.
var errStreamVisibility = errors.New("visibility resolution failed")

// forwardComms replays the snapshot, then streams live events until disconnect, shutdown, or lag.
// It filters each event by actor visibility because the bus is shared across subscribers.
// Lookup failures end the stream rather than fail open; hang-up and send failure end cleanly.
func forwardComms(
	ctx context.Context,
	actor store.AccountID,
	vis eventVisibility,
	sub events.Subscription[*compassv1.SubscribeCommsResponse],
	stream compassv1connect.CommsServiceSubscribeCommsServerStream,
) error {
	// send reports (visErr, ok): visErr is a store fault resolving visibility —
	// propagate it. ok is false on a clean end (a stream.Send failure or a
	// cancellation racing the visibility query). Kept distinct so a hang-up
	// never masquerades as a fault, nor a fault as a clean end.
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
		return nil, stream.Send(trimRemovedToActor(commsToResponse(actor, event), actor)) == nil
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
// message, trimmed for an account the event removed.
func commsToResponse(actor store.AccountID, event events.Stamped[*compassv1.SubscribeCommsResponse]) *compassv1.SubscribeCommsResponse {
	resp := &compassv1.SubscribeCommsResponse{
		Seq:           event.Seq,
		AtUnixMs:      event.AtUnixMS,
		InstanceEpoch: event.InstanceEpoch,
		Payload:       event.Payload.GetPayload(),
	}
	// A departed account gets only the channel id: the roster, name, and policy
	// are post-change state it was never a member alongside (a same-batch add).
	if cc := event.Payload.GetChannelChanged(); cc.GetChannel() != nil && slices.Contains(cc.GetRemovedAccountIds(), string(actor)) {
		resp.Payload = &compassv1.SubscribeCommsResponse_ChannelChanged{ChannelChanged: &compassv1.ChannelChanged{
			Channel:           &compassv1.Channel{Id: cc.GetChannel().GetId()},
			RemovedAccountIds: cc.GetRemovedAccountIds(),
		}}
	}
	return resp
}

// trimRemovedToActor keeps only the recipient's own id in removed_account_ids:
// the list exists to deliver each departed account its final event, and the
// other ids may name accounts the recipient could never see.
func trimRemovedToActor(resp *compassv1.SubscribeCommsResponse, actor store.AccountID) *compassv1.SubscribeCommsResponse {
	cc := resp.GetChannelChanged()
	if cc == nil || len(cc.GetRemovedAccountIds()) == 0 {
		return resp
	}
	var own []string
	if slices.Contains(cc.GetRemovedAccountIds(), string(actor)) {
		own = []string{string(actor)}
	}
	resp.Payload = &compassv1.SubscribeCommsResponse_ChannelChanged{
		ChannelChanged: &compassv1.ChannelChanged{Channel: cc.GetChannel(), RemovedAccountIds: own},
	}
	return resp
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

// commsSnapshotBoundary precedes a since_seq=0 snapshot and carries its store-space sequence boundary.
// Seq=0 and no payload distinguish it from positioned events and terminal resync; the epoch identifies its sequence space.
func commsSnapshotBoundary(snapshotSeq, instanceEpoch uint64) *compassv1.SubscribeCommsResponse {
	return &compassv1.SubscribeCommsResponse{
		Seq:           commsResyncSeq,
		InstanceEpoch: instanceEpoch,
		SnapshotSeq:   snapshotSeq,
	}
}

// eventVisibility mirrors the store's per-event visibility predicates used by List* reads.
// Tests can substitute a fake without a database.
type eventVisibility interface {
	// IsTopicChannelMember resolves the message's topic to its channel and matches ListMessages membership visibility.
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
	// boardVisible gates AgentBoardChanged with the same clip as board reads.
	boardVisible(ctx context.Context, actor, agent store.AccountID) (bool, error)
}

type storeEventVisibility struct {
	*store.Store
	boardVisibleFn func(context.Context, store.AccountID, store.AccountID) (bool, error)
}

func (s storeEventVisibility) boardVisible(ctx context.Context, viewer, agent store.AccountID) (bool, error) {
	return s.boardVisibleFn(ctx, viewer, agent)
}

// visibleToActor applies the matching List* predicate to each event in shared fan-out.
// Control frames pass because they carry no private content.
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
		// A member this change removed would never see its own removal — deliver this
		// one final event to a departed account too (Matt's ruling); commsToResponse
		// trims it to the channel id. Otherwise gate on full channel visibility.
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
	case *compassv1.SubscribeCommsResponse_AgentBoardChanged:
		return vis.boardVisible(ctx, actor, store.AccountID(p.AgentBoardChanged.GetAgentAccountId()))
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
