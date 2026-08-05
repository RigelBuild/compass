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
	"fmt"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
	"github.com/sealedsecurity/compass/go/internal/store"
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
	// askWaker pushes an ask-answer wake to the asking agent's live session when
	// a participant answers (SEA-1577), over the runnerhub T3 rail. Nil until
	// SetAskWaker wires it (comms<->hub is a construction cycle, broken by a
	// post-construction setter exactly like hub.SetSettleSink). Set once at
	// server assembly BEFORE any RPC is served, so it needs no lock: the write
	// happens-before the first concurrent read. Nil-safe: a Comms with no waker
	// (a unit test, or today's un-wired path) still answers asks.
	askWaker AskAnswerWaker
	// presence is the in-memory presence enum source GetRoster joins the durable
	// tree + activity against (SEA-1721 T2). Nil until SetPresenceSource wires it
	// (comms<->hub is a construction cycle, broken by a post-construction setter
	// exactly like askWaker / hub.SetSettleSink). Set once at server assembly
	// BEFORE any RPC is served, so it needs no lock. Nil-safe: a Comms with no
	// presence source (a unit test, or an un-wired path) reports every agent
	// OFFLINE.
	presence PresenceSource
}

// NewComms constructs the CommsService handler over store and bus. adminID is the
// bootstrap-admin account the local-socket door attributes callers to until the
// T3 interceptor sets a real identity (design.md:1219-1222).
func NewComms(st *store.Store, bus commsBus, adminID store.AccountID) *Comms {
	return &Comms{store: st, bus: bus, adminID: adminID}
}

// SetAskWaker wires the ask-answer wake sink (runnerhub) AFTER both Comms and the
// hub exist — the post-construction setter that breaks the comms<->hub
// construction cycle (comms is built before the hub because the hub's
// RelayCommsCall executes through the comms handler; serve.go:228,247). Mirrors
// hub.SetSettleSink. Called once at server assembly before serving; no lock
// because the write happens-before the first RPC. Nil-safe to leave unset (a
// hub-less handler does not wake — today's behavior).
func (c *Comms) SetAskWaker(w AskAnswerWaker) {
	c.askWaker = w
}

// SetPresenceSource wires the in-memory presence enum source GetRoster joins
// against, AFTER both Comms and the hub exist — the same post-construction
// setter that breaks the comms<->hub construction cycle as SetAskWaker. Called
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

// CreateAgent creates an agent account owned by the authenticated caller; the
// store mints the agent's home channel (RT-2) in the same transaction.
func (c *Comms) CreateAgent(
	ctx context.Context,
	req *connect.Request[compassv1.CreateAgentRequest],
) (*connect.Response[compassv1.CreateAgentResponse], error) {
	owner := c.actorFromContext(ctx)
	// CreateAgent is a user-caller API: `owner` is the caller's own id, used
	// directly both as the new agent's owner and as the parent same-owner
	// comparison key below. This matches the store only for user callers — the
	// store's ReparentAgent resolves an agent caller to its owner via COALESCE,
	// but no such resolution is applied here.
	// A supplied parent is validated before the account is minted (§Server
	// validation, applied on creation too): it must exist (clause 3 → NotFound)
	// and belong to the creating caller's owner (clauses 0/1 → PermissionDenied,
	// since the creator owns what it creates). Clause 2 (cycle) cannot arise on
	// create — a new account has no descendants.
	if parent := req.Msg.GetParentAgentId(); parent != "" {
		parentOwner, err := c.store.AgentOwner(ctx, store.AccountID(parent))
		if err != nil {
			return nil, edgeError(err) // ErrNotFound → NotFound
		}
		if parentOwner != owner {
			return nil, connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("parent agent %q has a different owner", parent))
		}
	}
	acc, err := c.store.CreateAgent(ctx, owner, store.NewAgent{
		Handle:        req.Msg.GetHandle(),
		DisplayName:   req.Msg.GetDisplayName(),
		ParentAgentID: store.AccountID(req.Msg.GetParentAgentId()),
	})
	if err != nil {
		return nil, edgeError(err)
	}
	c.publishAccountChanged(acc)
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
	ch, err := c.store.CreateChannel(ctx, c.actorFromContext(ctx), store.NewChannel{
		Name:             req.Msg.GetName(),
		GroupID:          store.ChannelGroupID(req.Msg.GetGroupId()),
		Kind:             channelKindFromWire(req.Msg.GetKind()),
		MemberAccountIDs: accountIDsFromWire(req.Msg.GetMemberAccountIds()),
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
	ch, removed, err := c.store.UpdateChannelMembers(
		ctx,
		c.actorFromContext(ctx),
		store.ChannelID(req.Msg.GetChannelId()),
		memberUpdatesFromWire(req.Msg),
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
	acc, err := c.store.ReparentAgent(
		ctx,
		c.actorFromContext(ctx),
		store.AccountID(req.Msg.GetAgentAccountId()),
		store.AccountID(req.Msg.GetNewParentAgentId()),
	)
	if err != nil {
		return nil, edgeError(err)
	}
	c.publishAccountChanged(acc)
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
	ws, err := c.store.OpenAgentWorkspace(
		ctx,
		c.actorFromContext(ctx),
		store.AccountID(req.Msg.GetAgentAccountId()),
	)
	if err != nil {
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
		ID:   req.Msg.GetTopicId(),
		Name: req.Msg.GetTopicName(),
	}, req.Msg.GetClientRequestId())
	if err != nil {
		return nil, edgeError(err)
	}
	// Publish only on a genuine insert: an idempotent retry returns the stored
	// row unchanged (inserted=false), so re-fanning MessagePosted would emit a
	// spurious live state-change for a row that did not change.
	if inserted {
		c.publishMessagePosted(msg)
	}
	return connect.NewResponse(&compassv1.PostMessageResponse{Message: MessageToWire(msg)}), nil
}

// RespondToAsk answers a pending structured ask; the caller must be a member of
// the channel the ask belongs to (store-enforced). Answering updates the ask's
// message in place and emits MessageUpdated.
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
	msg, err := c.store.AnswerAsk(
		ctx,
		c.actorFromContext(ctx),
		req.Msg.GetAskId(),
		answers,
	)
	if err != nil {
		return nil, edgeError(err)
	}
	c.publishMessageUpdated(msg)
	// SEA-1577: wake the asking agent's live session with the answer, after the
	// AnswerAsk err short-circuit — so a second RespondToAsk (rejected by the
	// answer-once guard) never wakes. The ask is authored by the AGENT; the
	// human/participant answers it, so the account to wake is the ask message's
	// author. Best-effort and nil-safe: a wake failure is swallowed in the rail
	// layer (the answer is already durably recorded + fanned out), never failing
	// the RPC.
	if c.askWaker != nil {
		c.askWaker.WakeAskAnswer(ctx, msg.AuthorAccountID, req.Msg.GetAskId(), req.Msg.GetAnswers())
	}
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

// ---- manager-comms-substrate RPCs (SEA-1740 T1) ----
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
	ch, err := c.store.SetChannelPolicy(
		ctx,
		c.actorFromContext(ctx),
		store.ChannelID(req.Msg.GetChannelId()),
		store.ChannelPolicy{
			PostPolicy:            channelPostPolicyFromWire(req.Msg.GetPostPolicy()),
			OwnerAccountID:        store.AccountID(req.Msg.GetOwnerAccountId()),
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
	entries, err := c.roster(ctx, c.actorFromContext(ctx), req.Msg.GetAgentAccountId(), req.Msg.GetScope())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&compassv1.GetRosterResponse{Entries: entries}), nil
}

// UpdatePinnedBoard is unimplemented until T6.
func (c *Comms) UpdatePinnedBoard(
	ctx context.Context,
	req *connect.Request[compassv1.UpdatePinnedBoardRequest],
) (*connect.Response[compassv1.UpdatePinnedBoardResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("compass.v1.CommsService.UpdatePinnedBoard is not implemented"))
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
