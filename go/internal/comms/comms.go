// Package comms is the CommsService handler: the communication-layer door of the
// compass.v1 contract (accounts, channel groups + channels, messages, agent
// workspaces, and the comms event stream). It is a thin shell over the T1
// Postgres store (the store of record and the owner of D9 visibility, enforced
// server-side in SQL) and the generic event bus (the live fan-out): every RPC
// maps proto <-> store at its edge, authorizes against the caller's visible set
// through the store, and — for a mutation — writes Postgres first, then publishes
// the corresponding event onto the comms bus (write-through fan-out).
//
// The caller identity is never a request field (spoofable); it is the account
// authenticated on the connection, read from the request context. On the shipped
// local-socket door there is no interceptor yet, so every RPC is attributed to
// the bootstrap admin (the 0600 socket is the local credential); the T3 network
// door adds a token interceptor that sets the real caller. actorFromContext is
// the single seam both paths write through.
package comms

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/store"
)

// commsBus is the comms event bus instantiation: a second bus instance distinct
// from CompassService's SubscribeEvents bus, with its own seq space and its own
// per-boot instance_epoch (design.md:1198-1208). Like the landed busPayload
// (server/service.go:33-37) it carries the whole wire response with only the
// payload oneof set, so the bus generic needs no domain sum type; the stream
// edge stamps Seq/AtUnixMs/InstanceEpoch onto a copy.
type commsBus = *events.Bus[*compassv1.SubscribeCommsResponse]

// Comms implements compassv1connect.CommsServiceHandler over the store and the
// comms event bus. Cheap to share by pointer; the store and bus are each safe
// for concurrent use, and Comms holds no mutable state of its own — the store is
// the source of truth, so there is no in-memory account/channel/group state.
type Comms struct {
	store *store.Store
	bus   commsBus
	// adminID attributes every RPC on the local-socket door (the door has no
	// interceptor yet). The T3 interceptor overrides this per-request by setting
	// a caller on the context; adminID is the fallback when none is set.
	adminID store.AccountID
	// presence is the in-memory presence enum source GetRoster joins the durable
	// tree + activity against (RIG-1721 T2). Nil until SetPresenceSource wires it
	// (comms<->hub is a construction cycle, broken by a post-construction setter
	// exactly like hub.SetSettleSink). Set once at server assembly BEFORE any RPC
	// is served, so it needs no lock. Nil-safe: a Comms with no presence source
	// (a unit test, or an un-wired path) reports every agent OFFLINE.
	presence PresenceSource
}

// NewComms constructs the CommsService handler over store and bus. adminID is the
// bootstrap-admin account the local-socket door attributes callers to until the
// T3 interceptor sets a real identity (design.md:1219-1222).
func NewComms(st *store.Store, bus commsBus, adminID store.AccountID) *Comms {
	return &Comms{store: st, bus: bus, adminID: adminID}
}

// SetPresenceSource wires the in-memory presence enum source GetRoster joins
// against, AFTER both Comms and the hub exist — a post-construction setter that
// breaks the comms<->hub construction cycle (comms is built before the hub
// because the hub's RelayCommsCall executes through the comms handler). Called
// once at server assembly before serving; no lock because the write
// happens-before the first RPC. Nil-safe to leave unset (a hub-less handler
// reports every agent OFFLINE — today's behavior).
func (c *Comms) SetPresenceSource(src PresenceSource) {
	c.presence = src
}

// Ensure Comms satisfies the generated handler interface at compile time.
var _ compassv1connect.CommsServiceHandler = (*Comms)(nil)

// ---- account RPCs (D9) ----

// CreateUser creates a human member account. Role elevation to admin is a
// separate path, never a signup field (comms.proto:39-42).
func (c *Comms) CreateUser(
	ctx context.Context,
	req *connect.Request[compassv1.CreateUserRequest],
) (*connect.Response[compassv1.CreateUserResponse], error) {
	acc, err := c.store.CreateUser(ctx, store.NewUser{
		Handle:      req.Msg.GetHandle(),
		DisplayName: req.Msg.GetDisplayName(),
	})
	if err != nil {
		return nil, edgeError(err)
	}
	c.publishAccountChanged(acc)
	return connect.NewResponse(&compassv1.CreateUserResponse{Account: accountToWire(acc)}), nil
}

// CreateAgent creates an agent account owned by the caller's resolved owner (an
// agent caller's child belongs to the caller's owner, a user caller's to itself
// — see the resolution below); the store mints the agent's home channel (RT-2)
// in the same transaction.
func (c *Comms) CreateAgent(
	ctx context.Context,
	req *connect.Request[compassv1.CreateAgentRequest],
) (*connect.Response[compassv1.CreateAgentResponse], error) {
	caller := c.actorFromContext(ctx)
	owner, err := c.store.ResolveOwner(ctx, caller)
	if err != nil {
		return nil, edgeError(err)
	}
	// CreateAgent now resolves the caller's owner via store.ResolveOwner
	// (mirroring ReparentAgent's clause-0 resolution): an agent caller resolves
	// to its owner_user_id, a user caller to itself. The resolved user-owner is
	// both the parent same-owner comparison key below AND the owner the new agent
	// is created under — so an agent spawning a child under a same-owner parent is
	// authorized correctly, and the store's owner_user_id FK (which requires a
	// user) is satisfied.
	// A supplied parent is validated before the account is minted (§Server
	// validation, applied on creation too): the parent_handle must resolve to an
	// existing agent (clause 3 → NotFound) that belongs to the creating caller's
	// owner. Clause 2 (cycle) cannot arise on create — a new account has no
	// descendants. Oracle-safe remap (DL-269): a resolved-but-foreign parent
	// (an owner-qualified handle naming another owner's agent) is byte-identical
	// to an unknown one — NOT_FOUND naming the SUBMITTED handle, never the old
	// PermissionDenied "different owner" that leaked the parent's existence.
	var parentID store.AccountID
	if parent := req.Msg.GetParentHandle(); parent != "" {
		parentID, err = c.resolveAgentHandle(ctx, caller, parent)
		if err != nil {
			return nil, edgeError(err) // resolver miss → NOT_FOUND naming the handle
		}
		parentOwner, err := c.store.AgentOwner(ctx, parentID)
		if err != nil {
			return nil, edgeError(notFoundHandle(err, parent))
		}
		if parentOwner != owner {
			return nil, edgeError(notFoundHandle(store.ErrNotFound, parent))
		}
	}
	// Install the coordination emit buffer so the store's in-tx coordination hook
	// (fired inside CreateAgent when this agent has a parent) records its channel
	// change here; drained + emitted post-commit below (RIG-1722 T5).
	ctx, coordChanges := withCoordChanges(ctx)
	acc, err := c.store.CreateAgent(ctx, owner, store.NewAgent{
		Handle:        req.Msg.GetHandle(),
		DisplayName:   req.Msg.GetDisplayName(),
		ParentAgentID: parentID,
	})
	if err != nil {
		return nil, edgeError(err)
	}
	c.publishAccountChanged(acc)
	c.emitCoordChanges(ctx, coordChanges)
	return connect.NewResponse(&compassv1.CreateAgentResponse{Account: accountToWire(acc)}), nil
}

// ListAccounts lists the accounts visible to the caller (store-scoped in SQL).
func (c *Comms) ListAccounts(
	ctx context.Context,
	_ *connect.Request[compassv1.ListAccountsRequest],
) (*connect.Response[compassv1.ListAccountsResponse], error) {
	accs, err := c.store.ListAccounts(ctx, c.actorFromContext(ctx))
	if err != nil {
		return nil, edgeError(err)
	}
	out := make([]*compassv1.Account, len(accs))
	for i, a := range accs {
		out[i] = accountToWire(a)
	}
	return connect.NewResponse(&compassv1.ListAccountsResponse{Accounts: out}), nil
}

// ---- channel group + channel RPCs (D9) ----

// CreateChannelGroup creates a namespace node owned by the caller.
func (c *Comms) CreateChannelGroup(
	ctx context.Context,
	req *connect.Request[compassv1.CreateChannelGroupRequest],
) (*connect.Response[compassv1.CreateChannelGroupResponse], error) {
	grp, err := c.store.CreateChannelGroup(ctx, c.actorFromContext(ctx), store.NewChannelGroup{
		Name:          req.Msg.GetName(),
		ParentGroupID: store.ChannelGroupID(req.Msg.GetParentGroupId()),
		Visibility:    groupVisibilityFromWire(req.Msg.GetVisibility()),
	})
	if err != nil {
		return nil, edgeError(err)
	}
	c.publishChannelGroupChanged(grp)
	return connect.NewResponse(&compassv1.CreateChannelGroupResponse{Group: groupToWire(grp)}), nil
}

// ListChannelGroups lists the channel groups visible to the caller.
func (c *Comms) ListChannelGroups(
	ctx context.Context,
	_ *connect.Request[compassv1.ListChannelGroupsRequest],
) (*connect.Response[compassv1.ListChannelGroupsResponse], error) {
	grps, err := c.store.ListChannelGroups(ctx, c.actorFromContext(ctx))
	if err != nil {
		return nil, edgeError(err)
	}
	out := make([]*compassv1.ChannelGroup, len(grps))
	for i, g := range grps {
		out[i] = groupToWire(g)
	}
	return connect.NewResponse(&compassv1.ListChannelGroupsResponse{Groups: out}), nil
}

// ListChannels lists the channels the caller may see.
func (c *Comms) ListChannels(
	ctx context.Context,
	_ *connect.Request[compassv1.ListChannelsRequest],
) (*connect.Response[compassv1.ListChannelsResponse], error) {
	chans, err := c.store.ListChannels(ctx, c.actorFromContext(ctx))
	if err != nil {
		return nil, edgeError(err)
	}
	out := make([]*compassv1.Channel, len(chans))
	for i, ch := range chans {
		out[i] = channelToWire(ch)
	}
	return connect.NewResponse(&compassv1.ListChannelsResponse{Channels: out}), nil
}

// CreateChannel creates a channel within a group, caller-authorized against the
// parent group; emits ChannelChanged (additive RPC, this task).
func (c *Comms) CreateChannel(
	ctx context.Context,
	req *connect.Request[compassv1.CreateChannelRequest],
) (*connect.Response[compassv1.CreateChannelResponse], error) {
	caller := c.actorFromContext(ctx)
	members, err := c.resolveHandles(ctx, caller, req.Msg.GetMemberHandles())
	if err != nil {
		return nil, edgeError(err)
	}
	ch, err := c.store.CreateChannel(ctx, caller, store.NewChannel{
		Name:             req.Msg.GetName(),
		GroupID:          store.ChannelGroupID(req.Msg.GetGroupId()),
		Kind:             channelKindFromWire(req.Msg.GetKind()),
		MemberAccountIDs: members,
	})
	if err != nil {
		return nil, edgeError(err)
	}
	c.publishChannelChanged(ch, nil)
	return connect.NewResponse(&compassv1.CreateChannelResponse{Channel: channelToWire(ch)}), nil
}

// UpdateChannelMembers adds/removes members and flips the per-member subscribe
// opt-in, caller-authorized against channel visibility; emits ChannelChanged.
// One RPC covers join, subscribe-toggle, DM-expansion, and share-replacement
// (RT-1): the request's add/remove/subscribe/unsubscribe lists collapse into the
// store's per-member MemberUpdate set.
func (c *Comms) UpdateChannelMembers(
	ctx context.Context,
	req *connect.Request[compassv1.UpdateChannelMembersRequest],
) (*connect.Response[compassv1.UpdateChannelMembersResponse], error) {
	caller := c.actorFromContext(ctx)
	updates, err := c.memberUpdatesFromWire(ctx, caller, req.Msg)
	if err != nil {
		return nil, edgeError(err)
	}
	ch, removed, err := c.store.UpdateChannelMembers(
		ctx,
		caller,
		store.ChannelID(req.Msg.GetChannelId()),
		updates,
		store.MemberUpdatesOptions{ConvertChannelName: req.Msg.GetConvertChannelName()},
	)
	if err != nil {
		return nil, edgeError(err)
	}
	c.publishChannelChanged(ch, removed)
	return connect.NewResponse(&compassv1.UpdateChannelMembersResponse{Channel: channelToWire(ch)}), nil
}

// ReparentAgent moves an agent to a new parent in the agent tree, or promotes it
// to a root (empty new_parent_agent_id) — caller-authorized against the agent's
// owner; emits AccountChanged (the existing account event, so every surface
// re-derives the tree from the changed parent_agent_id with no new plumbing).
// The store runs the serialized validate-and-write (§Server validation); its
// sentinel errors map to PERMISSION_DENIED / FAILED_PRECONDITION / NOT_FOUND at
// the edge (edgeError).
func (c *Comms) ReparentAgent(
	ctx context.Context,
	req *connect.Request[compassv1.ReparentAgentRequest],
) (*connect.Response[compassv1.ReparentAgentResponse], error) {
	// Install the coordination emit buffer so the store's in-tx coordination hook
	// (fired inside ReparentAgent for both the new and old managers) records its
	// channel changes here; drained + emitted post-commit below (RIG-1722 T5).
	ctx, coordChanges := withCoordChanges(ctx)
	caller := c.actorFromContext(ctx)
	agentID, err := c.resolveAgentHandle(ctx, caller, req.Msg.GetAgentHandle())
	if err != nil {
		return nil, edgeError(err)
	}
	// new_parent_handle empty ⇒ promote to root (no parent to resolve).
	var newParentID store.AccountID
	if h := req.Msg.GetNewParentHandle(); h != "" {
		newParentID, err = c.resolveAgentHandle(ctx, caller, h)
		if err != nil {
			return nil, edgeError(err)
		}
		// Oracle-safe remap (DL-269), mirroring CreateAgent's parent pre-check:
		// a resolved-but-foreign parent (an owner-qualified handle naming another
		// owner's agent) must be byte-identical to an unknown one. Resolve the
		// caller's owner and reject a foreign parent HERE, naming the SUBMITTED
		// new_parent_handle — otherwise the store's clause-1 ErrPermissionDenied
		// ("parent agent %q has a different owner") gets re-keyed below to name
		// the AGENT handle, which differs from the unknown-parent NOT_FOUND
		// (named with the parent handle) and leaks the parent's existence.
		owner, err := c.store.ResolveOwner(ctx, caller)
		if err != nil {
			return nil, edgeError(err)
		}
		parentOwner, err := c.store.AgentOwner(ctx, newParentID)
		if err != nil {
			return nil, edgeError(notFoundHandle(err, h))
		}
		if parentOwner != owner {
			return nil, edgeError(notFoundHandle(store.ErrNotFound, h))
		}
	}
	acc, err := c.store.ReparentAgent(
		ctx,
		caller,
		agentID,
		newParentID,
	)
	if err != nil {
		// Oracle-safe remap (DL-269): the store's clause-0 authority failure
		// (ErrPermissionDenied "caller may not re-parent agent %q") on a resolved
		// but foreign agent must be byte-identical to the unknown-handle
		// NOT_FOUND — a real-but-foreign handle is guessable, so it cannot leak a
		// distinct code/message. Re-key it to NOT_FOUND naming the SUBMITTED
		// agent handle, never the resolved id.
		if errors.Is(err, store.ErrPermissionDenied) {
			return nil, edgeError(notFoundHandle(store.ErrNotFound, req.Msg.GetAgentHandle()))
		}
		return nil, edgeError(err)
	}
	c.publishAccountChanged(acc)
	c.emitCoordChanges(ctx, coordChanges)
	return connect.NewResponse(&compassv1.ReparentAgentResponse{Account: accountToWire(acc)}), nil
}

// ---- agent workspace RPC (D5) ----

// OpenAgentWorkspace opens (or fetches) the caller's observation pane for an
// agent; access is a projection of the agent's channel membership, enforced by
// the store. Idempotent.
func (c *Comms) OpenAgentWorkspace(
	ctx context.Context,
	req *connect.Request[compassv1.OpenAgentWorkspaceRequest],
) (*connect.Response[compassv1.OpenAgentWorkspaceResponse], error) {
	caller := c.actorFromContext(ctx)
	agentID, err := c.resolveAgentHandle(ctx, caller, req.Msg.GetAgentHandle())
	if err != nil {
		return nil, edgeError(err)
	}
	ws, err := c.store.OpenAgentWorkspace(ctx, caller, agentID)
	if err != nil {
		// The store names the resolved ACCOUNT ID in its NOT_FOUND (`agent %q`)
		// and edgeError maps store errors verbatim — leaking the resolved id of
		// an invisible target. Re-key to name the SUBMITTED handle (DL-269), so
		// an invisible/unknown/foreign target is byte-identical.
		if errors.Is(err, store.ErrNotFound) {
			return nil, edgeError(notFoundHandle(store.ErrNotFound, req.Msg.GetAgentHandle()))
		}
		return nil, edgeError(err)
	}
	c.publishAgentWorkspaceChanged(ws)
	return connect.NewResponse(&compassv1.OpenAgentWorkspaceResponse{Workspace: workspaceToWire(ws)}), nil
}

// ---- message RPCs (D5) ----

// ListMessages pages a channel's message history, newest-first.
func (c *Comms) ListMessages(
	ctx context.Context,
	req *connect.Request[compassv1.ListMessagesRequest],
) (*connect.Response[compassv1.ListMessagesResponse], error) {
	msgs, err := c.store.ListMessages(ctx, store.ListMessagesQuery{
		Actor:     c.actorFromContext(ctx),
		ChannelID: store.ChannelID(req.Msg.GetChannelId()),
		TopicID:   req.Msg.GetTopicId(),
		Page: store.Page{
			Limit:           req.Msg.GetLimit(),
			BeforeMessageID: store.MessageID(req.Msg.GetBeforeMessageId()),
			SnapshotSeq:     req.Msg.GetSnapshotSeq(),
		},
	})
	if err != nil {
		return nil, edgeError(err)
	}
	return connect.NewResponse(&compassv1.ListMessagesResponse{Messages: messagesToWire(msgs)}), nil
}

// PostMessage posts a message to a channel — a human turn, or a human prompt
// into an agent's channel; emits MessagePosted (write-through).
func (c *Comms) PostMessage(
	ctx context.Context,
	req *connect.Request[compassv1.PostMessageRequest],
) (*connect.Response[compassv1.PostMessageResponse], error) {
	blocks, err := blocksFromWire(req.Msg.GetBlocks())
	if err != nil {
		return nil, err
	}
	msg, inserted, err := c.store.AppendMessage(ctx, store.Message{
		AuthorAccountID: c.actorFromContext(ctx),
		Blocks:          blocks,
	}, req.Msg.GetChannelId(), store.TopicRef{
		ID:     req.Msg.GetTopicId(),
		Name:   req.Msg.GetTopicName(),
		Create: req.Msg.GetCreateTopic(),
	}, req.Msg.GetClientRequestId())
	if err != nil {
		return nil, edgeError(err)
	}
	// Stamp the appended message's id onto the handler's RPC span (the
	// otelconnect origin span mounted on this service), so a trace filters to
	// one message. A no-op when no span is active (no provider installed).
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("compass.message.id", string(msg.ID)))
	// Publish only on a genuine insert: an idempotent retry returns the stored
	// row unchanged (inserted=false), so re-fanning MessagePosted would emit a
	// spurious live state-change for a row that did not change.
	if inserted {
		c.publishMessagePosted(ctx, msg)
	}
	return connect.NewResponse(&compassv1.PostMessageResponse{Message: MessageToWire(msg)}), nil
}

// RespondToAsk answers a pending structured ask; the caller must be a member of
// the channel the ask belongs to (store-enforced). Answering updates the ask's
// message in place AND posts the answer as a new message authored by the
// answerer, both in one store transaction. It emits MessageUpdated for the ask
// (the UI ask-state update) and MessagePosted for the answer message (the
// delivery trigger: the answer rides the normal message rail to the asking
// agent — RIG-2257).
func (c *Comms) RespondToAsk(
	ctx context.Context,
	req *connect.Request[compassv1.RespondToAskRequest],
) (*connect.Response[compassv1.RespondToAskResponse], error) {
	answers := make([]store.AskAnswer, len(req.Msg.GetAnswers()))
	for i, a := range req.Msg.GetAnswers() {
		answers[i] = store.AskAnswer{
			QuestionID:      a.GetQuestionId(),
			ChosenOptionIDs: a.GetChosenOptionIds(),
			CustomText:      a.GetCustomText(),
		}
	}
	askMsg, answerMsg, err := c.store.AnswerAsk(
		ctx,
		c.actorFromContext(ctx),
		req.Msg.GetAskId(),
		answers,
	)
	if err != nil {
		return nil, edgeError(err)
	}
	// Stamp the answer message's id onto the handler's RPC span (the delivery
	// origin for the answer post), matching PostMessage. A no-op when no span is
	// active.
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("compass.message.id", string(answerMsg.ID)))
	// MessageUpdated carries the ask's new answered state to the UI; it is NOT a
	// delivery trigger. MessagePosted for the answer message IS the delivery
	// trigger — it fans out on the normal message rail, so an offline or
	// reconnecting asker gets the answer via the ack-gated cursor + resweep
	// (RIG-2257: no bespoke ask wake).
	c.publishMessageUpdated(askMsg)
	c.publishMessagePosted(ctx, answerMsg)
	return connect.NewResponse(&compassv1.RespondToAskResponse{}), nil
}

// SearchMessages runs a visibility-scoped full-text search; the store scopes
// results to the caller's visible set regardless of the scope field.
func (c *Comms) SearchMessages(
	ctx context.Context,
	req *connect.Request[compassv1.SearchMessagesRequest],
) (*connect.Response[compassv1.SearchMessagesResponse], error) {
	msgs, err := c.store.SearchMessages(
		ctx,
		c.actorFromContext(ctx),
		store.SearchScope{ChannelID: store.ChannelID(req.Msg.GetChannelId())},
		req.Msg.GetQuery(),
		store.Page{Limit: req.Msg.GetLimit(), SnapshotSeq: req.Msg.GetSnapshotSeq()},
	)
	if err != nil {
		return nil, edgeError(err)
	}
	return connect.NewResponse(&compassv1.SearchMessagesResponse{Messages: messagesToWire(msgs)}), nil
}

// ---- topic RPCs (D5) ----

// ListTopics lists the topics of a channel visible to the caller (store-scoped:
// the caller must be a member of the channel). includeArchived controls whether
// the tidiness-archived topics are returned.
func (c *Comms) ListTopics(
	ctx context.Context,
	req *connect.Request[compassv1.ListTopicsRequest],
) (*connect.Response[compassv1.ListTopicsResponse], error) {
	topics, err := c.store.ListTopics(ctx, string(c.actorFromContext(ctx)), req.Msg.GetChannelId(), req.Msg.GetIncludeArchived())
	if err != nil {
		return nil, edgeError(err)
	}
	return connect.NewResponse(&compassv1.ListTopicsResponse{Topics: topicsToWire(topics)}), nil
}

// UpdateTopic renames and/or archives a topic, caller-authorized against the
// topic's channel by the store; emits TopicUpserted (write-through, only on a
// successful commit) so the live topic index stays current.
func (c *Comms) UpdateTopic(
	ctx context.Context,
	req *connect.Request[compassv1.UpdateTopicRequest],
) (*connect.Response[compassv1.UpdateTopicResponse], error) {
	topic, err := c.store.UpdateTopic(ctx, string(c.actorFromContext(ctx)), req.Msg.GetTopicId(), req.Msg.Name, req.Msg.Archived)
	if err != nil {
		return nil, edgeError(err)
	}
	c.publishTopicUpserted(topic)
	return connect.NewResponse(&compassv1.UpdateTopicResponse{Topic: topicToWire(topic)}), nil
}

// ---- manager-comms-substrate RPCs (RIG-1740 T1) ----
//
// GetRoster, SetChannelPolicy, and UpdatePinnedBoard are the T1 proto surface of
// the manager-comms substrate. T1 lands the contract (proto + regen) proto-first;
// the real handler bodies are the T1-gated legs (T2 roster read, T4 channel
// policy, T6 pinned board) that replace these stubs. Until then they return
// CodeUnimplemented so the handler satisfies the generated interface (comms.go
// asserts CommsServiceHandler with no Unimplemented embed) without pretending to
// serve a surface whose store legs do not exist yet.

// SetChannelPolicy sets a channel's post policy, owner/operator account, and
// mandatory-subscription flag (T4) — the only mutation path for these fields
// after creation. The store enforces D9 write-authz (a non-member and an
// unknown channel both map to CodeNotFound via edgeError) and transactionally
// seeds the D2 delivery cursor for every member a newly-set mandatory flag turns
// into a delivery target. Emits ChannelChanged (write-through) so the live
// projection carries the updated policy.
func (c *Comms) SetChannelPolicy(
	ctx context.Context,
	req *connect.Request[compassv1.SetChannelPolicyRequest],
) (*connect.Response[compassv1.SetChannelPolicyResponse], error) {
	caller := c.actorFromContext(ctx)
	// owner_handle empty ⇒ the channel is left unowned (no resolution). A
	// non-empty owner_handle names a user OR agent, so it resolves through the
	// general batch resolver; a miss is NOT_FOUND naming the submitted handle.
	var ownerID store.AccountID
	if h := req.Msg.GetOwnerHandle(); h != "" {
		ids, err := c.resolveHandles(ctx, caller, []string{h})
		if err != nil {
			return nil, edgeError(err)
		}
		ownerID = ids[0]
	}
	ch, err := c.store.SetChannelPolicy(
		ctx,
		caller,
		store.ChannelID(req.Msg.GetChannelId()),
		store.ChannelPolicy{
			PostPolicy:            channelPostPolicyFromWire(req.Msg.GetPostPolicy()),
			OwnerAccountID:        ownerID,
			MandatorySubscription: req.Msg.GetMandatorySubscription(),
		},
	)
	if err != nil {
		return nil, edgeError(err)
	}
	c.publishChannelChanged(ch, nil)
	return connect.NewResponse(&compassv1.SetChannelPolicyResponse{Channel: channelToWire(ch)}), nil
}

// GetRoster joins the three roster sources for the vantage agent's scope: the
// tree (durable store), the live presence enum (in-memory hub), and the durable
// activity string (agent_activity table), clipped to the CALLER's account-
// visible set (D9). Implemented in roster.go.
func (c *Comms) GetRoster(
	ctx context.Context,
	req *connect.Request[compassv1.GetRosterRequest],
) (*connect.Response[compassv1.GetRosterResponse], error) {
	entries, err := c.roster(ctx, c.actorFromContext(ctx), req.Msg.GetVantageHandle(), req.Msg.GetScope())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&compassv1.GetRosterResponse{Entries: entries}), nil
}

// UpdatePinnedBoard mutates a channel's pinned board (T6): a plain pin, a
// compare-and-swap repoint (pin with replace), or an unpin. The board carries
// only POINTERS to messages already in the channel — this RPC never writes a
// Message row (DL-099). It emits ChannelChanged carrying the updated board so
// the live projection stays current, mirroring SetChannelPolicy's write-through.
//
// Authz is the channel's post_policy (design.md:626-629): on OWNER_ONLY only
// owner_account_id may mutate the board; on OPEN any member may. A non-owner on
// an OWNER_ONLY channel is refused with the SAME CodeNotFound a non-member gets
// — the no-oracle in-band rejection, consistent with PostMessage's OWNER_ONLY
// enforcement (store/messages.go:80-82) so the policy leaks no existence signal.
// The authoritative who-may-act decision is made in the store's board txn, under
// the channels-row FOR UPDATE lock (membership + post_policy, mirroring
// PostMessage), so it is serialized against a concurrent membership/policy change
// — no TOCTOU. The handler does not pre-check authz on a non-tx snapshot; it
// relies on the store call returning ErrNotFound (mapped to CodeNotFound via
// edgeError), which preserves the exact no-oracle client contract.
func (c *Comms) UpdatePinnedBoard(
	ctx context.Context,
	req *connect.Request[compassv1.UpdatePinnedBoardRequest],
) (*connect.Response[compassv1.UpdatePinnedBoardResponse], error) {
	actor := c.actorFromContext(ctx)
	channelID := store.ChannelID(req.Msg.GetChannelId())

	entries, err := c.applyBoardOp(ctx, channelID, actor, req.Msg)
	if err != nil {
		return nil, err
	}

	// Re-read the channel so ChannelChanged and the response carry the current
	// member/policy projection alongside the updated board.
	ch, err := c.store.GetChannel(ctx, channelID)
	if err != nil {
		return nil, edgeError(err)
	}
	wire := channelToWire(ch)
	wire.PinnedEntries = pinnedEntriesToWire(entries)
	// ChannelChanged carries the updated board (design.md:637). channelToWire
	// does not project pinned_entries (the board is maintained only here), so the
	// event is published from the same board-carrying wire the response returns.
	c.bus.Publish(&compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_ChannelChanged{
			ChannelChanged: &compassv1.ChannelChanged{Channel: wire},
		},
	})
	return connect.NewResponse(&compassv1.UpdatePinnedBoardResponse{Channel: wire}), nil
}

// OpenDM resolves-or-creates the two-party DM channel between the caller and a
// peer, addressed by handle (RIG-2962). T1 lands the contract (proto + regen)
// proto-first; the real handler — caller/peer resolve, same-owner authz, the
// reserved-DM-group upsert, and the post-commit ChannelChanged emit — is the
// T3 leg (compass-agent-peer-dm design.md T3), which replaces this stub. Until
// then it returns CodeUnimplemented so *Comms satisfies the generated
// CommsServiceHandler (asserted with no Unimplemented embed) without pretending
// to serve a surface whose store legs (T2 dm.go) do not exist yet.
func (c *Comms) OpenDM(
	_ context.Context,
	_ *connect.Request[compassv1.OpenDMRequest],
) (*connect.Response[compassv1.OpenDMResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("comms: OpenDM not implemented until RIG-2962 T3"))
}

// applyBoardOp maps the request's op oneof to its store call: a plain pin
// (PinMessage, replace ""), a compare-and-swap repoint (PinMessage, replace set),
// or an unpin (UnpinMessage). An unset oneof is a malformed request
// (CodeInvalidArgument). Store errors map through edgeError to their Connect
// codes: a message from another channel or a stale CAS surface in-band.
func (c *Comms) applyBoardOp(
	ctx context.Context,
	channelID store.ChannelID,
	actor store.AccountID,
	msg *compassv1.UpdatePinnedBoardRequest,
) ([]store.PinnedEntry, error) {
	switch op := msg.GetOp().(type) {
	case *compassv1.UpdatePinnedBoardRequest_Pin:
		entries, err := c.store.PinMessage(
			ctx,
			channelID,
			store.MessageID(op.Pin.GetMessageId()),
			store.MessageID(op.Pin.GetReplaceMessageId()),
			actor,
		)
		if err != nil {
			return nil, edgeError(err)
		}
		return entries, nil
	case *compassv1.UpdatePinnedBoardRequest_UnpinMessageId:
		entries, err := c.store.UnpinMessage(ctx, channelID, store.MessageID(op.UnpinMessageId), actor)
		if err != nil {
			return nil, edgeError(err)
		}
		return entries, nil
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("comms: UpdatePinnedBoard request has no pin/unpin op set"))
	}
}

// SubscribeComms is implemented in subscribe.go.

// actorFromContext returns the authenticated caller for this request. The T3
// interceptor sets it on the context; on the shipped socket door no interceptor
// runs, so it falls back to the bootstrap admin (the socket is the local
// credential). It never returns empty — an unattributed request on a door that
// should have set an identity is a server fault, but the socket path always has
// the admin fallback.
func (c *Comms) actorFromContext(ctx context.Context) store.AccountID {
	if actor, ok := actorFrom(ctx); ok && actor != "" {
		return actor
	}
	return c.adminID
}
