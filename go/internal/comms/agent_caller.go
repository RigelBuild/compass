//go:build unix

// The agent-comms *AsAccount family: execute one agent-originated comms operation
// as a resolved agent account (the RunnerHub resolves the relayed session_id
// first). NOT new CommsService RPCs: they reuse the human PostMessage/ListMessages
// paths via WithActor. Fail-closed: an empty account is a hard CodeInvalidArgument.
package comms

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// errNoActor is the fail-closed cause when PostAsAccount/ListAsAccount is called
// without a resolved account. It maps to CodeInvalidArgument — a server-side
// wiring fault, never a silent bootstrap-admin attribution.
var errNoActor = connect.NewError(
	connect.CodeInvalidArgument,
	noActorError{},
)

type noActorError struct{}

func (noActorError) Error() string {
	return "comms: agent-initiated call requires a resolved account; refusing to attribute to the bootstrap admin"
}

// errNotAgentAccount is the fail-closed cause when an agent-initiated call
// resolves to an account that is NOT an agent — a user account, whose
// Account.Agent is nil (scanAccount sets the subtype only for the side of the
// join that populated, store/accounts.go:313-322). Only an agent account has a
// home channel, so there is no channel to default to and nothing to post into.
//
// It maps to CodeFailedPrecondition, NOT CodeInvalidArgument, and the difference
// is load-bearing at the relay. The RunnerHub classifies a sink error by its
// Connect code: InvalidArgument is a routine per-frame refusal, counted among
// the frames a healthy relay occasionally rejects, while FailedPrecondition is a
// CONTRACT DEFECT — counted separately and logged as a misconfiguration
// (runnerhub/hub.go, isContractDefect). A session bound to a user account is the
// latter: no retry and no better-formed frame can fix it, and every frame from
// that session will fail identically, so it must not read as routine garbage.
var errNotAgentAccount = connect.NewError(
	connect.CodeFailedPrecondition,
	notAgentAccountError{},
)

type notAgentAccountError struct{}

func (notAgentAccountError) Error() string {
	return "comms: agent-initiated call resolved to a non-agent account, which has no home channel; the session->account binding is wrong"
}

// PostAsAccount executes one agent-initiated PostMessage as account. It sets the
// account on the context (WithActor) and delegates to the same PostMessage
// handler path a human caller takes, so authz (D9), idempotency, and fan-out match.
// Non-members get CodeNotFound; visible non-owners on OWNER_ONLY get
// CodePermissionDenied because membership already reveals the channel.
func (c *Comms) PostAsAccount(
	ctx context.Context,
	account store.AccountID,
	req *compassv1.PostMessageRequest,
) (*compassv1.PostMessageResponse, error) {
	if account == "" {
		return nil, errNoActor
	}
	req, err := c.defaultChannel(ctx, account, req)
	if err != nil {
		return nil, err
	}
	resp, err := c.PostMessage(WithActor(ctx, account), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("comms PostAsAccount: PostMessage returned nil response"))
	}
	return resp.Msg, nil
}

// ListAsAccount executes one agent-initiated ListMessages as account, mirroring
// PostAsAccount: WithActor + the shared ListMessages handler path, empty
// channel_id defaulting to the account's home channel, non-member channel →
// CodeNotFound.
func (c *Comms) ListAsAccount(
	ctx context.Context,
	account store.AccountID,
	req *compassv1.ListMessagesRequest,
) (*compassv1.ListMessagesResponse, error) {
	if account == "" {
		return nil, errNoActor
	}
	channelID, err := c.defaultListChannel(ctx, account, req.GetChannelId())
	if err != nil {
		return nil, err
	}
	listReq := &compassv1.ListMessagesRequest{
		Container:       &compassv1.ListMessagesRequest_ChannelId{ChannelId: channelID},
		Limit:           req.GetLimit(),
		BeforeMessageId: req.GetBeforeMessageId(),
		SnapshotSeq:     req.GetSnapshotSeq(),
		TopicId:         req.GetTopicId(),
	}
	resp, err := c.ListMessages(WithActor(ctx, account), connect.NewRequest(listReq))
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("comms ListAsAccount: ListMessages returned nil response"))
	}
	return resp.Msg, nil
}

// PostAsAccountByName is the AGENT TOOL entry for comms_post_message /
// comms_post_ask: it resolves the request's channel NAME (carried in the
// channel_id container arm — the TS tool sends a name there per peer-DM R1) to a
// real channel id within account's visible set, then delegates to the id-typed
// PostAsAccount. This is the ONLY caller that treats the container arm as a name;
// PostAsAccount stays id-typed for its internal id-holder co-callers
// (postSetupThread, the offline-mention e2e), which the design
// record's "resolve in PostAsAccount" wording predates.
//
// Per R2 there is NO home-channel default at the tool level: an empty channel
// name is NOT filled from home (the agent must NAME its channel, even its own
// home), so an empty name resolves to ErrNotFound like any other miss. An
// unknown or invisible name collapses to CodeNotFound (the D9 merge); an
// ambiguous name is CodeInvalidArgument naming the collision — both surfaced via
// edgeError from ChannelByNameForViewer.
func (c *Comms) PostAsAccountByName(
	ctx context.Context,
	account store.AccountID,
	req *compassv1.PostMessageRequest,
) (*compassv1.PostMessageResponse, error) {
	if account == "" {
		return nil, errNoActor
	}
	ch, err := c.store.ChannelByNameForViewer(ctx, account, req.GetChannelId())
	if err != nil {
		return nil, edgeError(err)
	}
	resolved := &compassv1.PostMessageRequest{
		Container:       &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
		Blocks:          req.GetBlocks(),
		Topic:           req.GetTopic(),
		ClientRequestId: req.GetClientRequestId(),
		CreateTopic:     req.GetCreateTopic(),
		TurnSequence:    req.GetTurnSequence(),
	}
	// The delegated PostAsAccount runs defaultChannel again, but it is a no-op
	// here: the channel id is already resolved-non-empty, so the empty→home fill
	// never fires. R2 (no home default for post/ask) is enforced upstream by
	// ChannelByNameForViewer rejecting an empty name, NOT by this residual pass.
	return c.PostAsAccount(ctx, account, resolved)
}

// ListAsAccountByName is the AGENT TOOL entry for comms_list_messages. It mirrors
// PostAsAccountByName's name resolution but KEEPS omit-=home (a read has no
// misroute hazard, peer-DM R2): an empty channel name defaults to the account's
// home channel, a non-empty name resolves through ChannelByNameForViewer. The
// resolved id-typed request is delegated to the unchanged ListAsAccount.
func (c *Comms) ListAsAccountByName(
	ctx context.Context,
	account store.AccountID,
	req *compassv1.ListMessagesRequest,
) (*compassv1.ListMessagesResponse, error) {
	if account == "" {
		return nil, errNoActor
	}
	channelID := req.GetChannelId()
	if channelID != "" {
		ch, err := c.store.ChannelByNameForViewer(ctx, account, channelID)
		if err != nil {
			return nil, edgeError(err)
		}
		channelID = string(ch.ID)
	}
	resolved := &compassv1.ListMessagesRequest{
		Container:       &compassv1.ListMessagesRequest_ChannelId{ChannelId: channelID},
		Limit:           req.GetLimit(),
		BeforeMessageId: req.GetBeforeMessageId(),
		SnapshotSeq:     req.GetSnapshotSeq(),
		TopicId:         req.GetTopicId(),
	}
	return c.ListAsAccount(ctx, account, resolved)
}

// ListTopicsAsAccountByName resolves the tool channel and returns topics with stats.
func (c *Comms) ListTopicsAsAccountByName(
	ctx context.Context,
	account store.AccountID,
	req *compassv1.ListTopicsRequest,
) (*compassv1.ListTopicsResponse, error) {
	if account == "" {
		return nil, errNoActor
	}
	channelID := req.GetChannelId()
	if channelID == "" {
		var err error
		channelID, err = c.homeChannel(ctx, account)
		if err != nil {
			return nil, err
		}
	} else {
		ch, err := c.store.ChannelByNameForViewer(ctx, account, channelID)
		if err != nil {
			return nil, edgeError(err)
		}
		channelID = string(ch.ID)
	}
	topics, err := c.store.ListTopicsWithStats(ctx, string(account), channelID, req.GetIncludeArchived())
	if err != nil {
		return nil, edgeError(err)
	}
	return &compassv1.ListTopicsResponse{Topics: topicsToWire(topics)}, nil
}

// UpdatePinnedBoardAsAccount executes one agent-initiated board update as account
// via the shared handler path. Non-members get CodeNotFound; a visible member
// who is not the owner of an OWNER_ONLY channel gets CodePermissionDenied.
// The request always names its channel explicitly, so there is no home default.
func (c *Comms) UpdatePinnedBoardAsAccount(
	ctx context.Context,
	account store.AccountID,
	req *compassv1.UpdatePinnedBoardRequest,
) (*compassv1.UpdatePinnedBoardResponse, error) {
	if account == "" {
		return nil, errNoActor
	}
	resp, err := c.UpdatePinnedBoard(WithActor(ctx, account), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("comms UpdatePinnedBoardAsAccount: UpdatePinnedBoard returned nil response"))
	}
	return resp.Msg, nil
}

func (c *Comms) resolveAgentGroupRef(
	ctx context.Context,
	account store.AccountID,
	groupID string,
	groupName string,
	nameField string,
) (string, error) {
	if groupID != "" {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("comms: agent tools name groups; use "+nameField))
	}
	if groupName == "" {
		return "", nil
	}
	group, err := c.store.ChannelGroupByRefForViewer(ctx, account, groupName)
	if err != nil {
		return "", edgeError(err)
	}
	return string(group.ID), nil
}

// CreateChannelAsAccount resolves a group name within the agent's visible groups,
// then runs the shared CreateChannel handler as that account.
func (c *Comms) CreateChannelAsAccount(
	ctx context.Context,
	account store.AccountID,
	req *compassv1.CreateChannelRequest,
) (*compassv1.CreateChannelResponse, error) {
	if account == "" {
		return nil, errNoActor
	}
	groupID, err := c.resolveAgentGroupRef(ctx, account, req.GetGroupId(), req.GetGroupName(), "group_name")
	if err != nil {
		return nil, err
	}
	if groupID != "" {
		resolved := proto.CloneOf(req)
		resolved.GroupId = groupID
		resolved.GroupName = ""
		req = resolved
	}
	resp, err := c.CreateChannel(WithActor(ctx, account), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("comms CreateChannelAsAccount: CreateChannel returned nil response"))
	}
	return resp.Msg, nil
}

// UpdateChannelMembersAsAccountByName executes one agent-initiated UpdateChannelMembers
// as account. Agent tools address channels by NAME, so channel_id is resolved
// within account's visible set first (unknown or invisible → CodeNotFound,
// ambiguous → CodeInvalidArgument), with no home default. The resolved request
// then runs the shared handler under WithActor, so authz and fan-out match a
// human caller's.
func (c *Comms) UpdateChannelMembersAsAccountByName(
	ctx context.Context,
	account store.AccountID,
	req *compassv1.UpdateChannelMembersRequest,
) (*compassv1.UpdateChannelMembersResponse, error) {
	if account == "" {
		return nil, errNoActor
	}
	ch, err := c.store.ChannelByNameForViewer(ctx, account, req.GetChannelId())
	if err != nil {
		return nil, edgeError(err)
	}
	resolved := proto.CloneOf(req)
	resolved.ChannelId = string(ch.ID)
	resp, err := c.UpdateChannelMembers(WithActor(ctx, account), connect.NewRequest(resolved))
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("comms UpdateChannelMembersAsAccountByName: UpdateChannelMembers returned nil response"))
	}
	return resp.Msg, nil
}

// CreateChannelGroupAsAccount resolves a parent group name within the agent's
// visible groups, then runs the shared handler as that account.
func (c *Comms) CreateChannelGroupAsAccount(
	ctx context.Context,
	account store.AccountID,
	req *compassv1.CreateChannelGroupRequest,
) (*compassv1.CreateChannelGroupResponse, error) {
	if account == "" {
		return nil, errNoActor
	}
	parentGroupID, err := c.resolveAgentGroupRef(ctx, account, req.GetParentGroupId(), req.GetParentGroupName(), "parent_group_name")
	if err != nil {
		return nil, err
	}
	if parentGroupID != "" {
		resolved := proto.CloneOf(req)
		resolved.ParentGroupId = parentGroupID
		resolved.ParentGroupName = ""
		req = resolved
	}
	resp, err := c.CreateChannelGroup(WithActor(ctx, account), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("comms CreateChannelGroupAsAccount: CreateChannelGroup returned nil response"))
	}
	return resp.Msg, nil
}

// OpenDMAsAccount executes an agent-initiated OpenDM as account, mirroring
// UpdatePinnedBoardAsAccount: it uses the shared handler path, including peer
// resolution, same-owner or mutual-peering authorization, DM upsert, and event.
// Unknown and unpeered peers return the same error; self opens are invalid.
// The request names the peer by handle, so there is no home-channel defaulting.
func (c *Comms) OpenDMAsAccount(
	ctx context.Context,
	account store.AccountID,
	req *compassv1.OpenDMRequest,
) (*compassv1.OpenDMResponse, error) {
	if account == "" {
		return nil, errNoActor
	}
	resp, err := c.OpenDM(WithActor(ctx, account), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("comms OpenDMAsAccount: OpenDM returned nil response"))
	}
	return resp.Msg, nil
}

// defaultChannel returns req with an empty channel_id filled from the account's
// home channel, so the common "post in my own channel" case needs no id plumbed
// into the container. A non-empty channel_id is left untouched. The returned
// request is a shallow copy — the caller's message is never mutated.
func (c *Comms) defaultChannel(
	ctx context.Context,
	account store.AccountID,
	req *compassv1.PostMessageRequest,
) (*compassv1.PostMessageRequest, error) {
	if req.GetChannelId() != "" {
		return req, nil
	}
	home, err := c.homeChannel(ctx, account)
	if err != nil {
		return nil, err
	}
	return &compassv1.PostMessageRequest{
		Container:       &compassv1.PostMessageRequest_ChannelId{ChannelId: home},
		Blocks:          req.GetBlocks(),
		Topic:           req.GetTopic(),
		ClientRequestId: req.GetClientRequestId(),
		CreateTopic:     req.GetCreateTopic(),
		TurnSequence:    req.GetTurnSequence(),
	}, nil
}

// defaultListChannel returns channelID unchanged when set, else the account's
// home channel.
func (c *Comms) defaultListChannel(ctx context.Context, account store.AccountID, channelID string) (string, error) {
	if channelID != "" {
		return channelID, nil
	}
	return c.homeChannel(ctx, account)
}

// homeChannel resolves the account's home channel id. An agent account always
// has one, minted at CreateAgent (store/accounts.go:156-158); a store failure
// surfaces mapped to its Connect code, never leaked verbatim.
//
// A NON-AGENT account is refused rather than dereferenced. scanAccount populates
// Account.Agent only for the agent side of the join (store/accounts.go:313-322),
// so for a user account it is nil and reading acc.Agent.HomeChannelID panicked —
// inside the RunnerHub's PublishEvents handler goroutine, where a panic takes the
// relay down and no error ever reaches the classification the hub does on the
// return value (rule://go-no-panic-in-lib). Failing closed with
// errNotAgentAccount both keeps the process up and gives the hub a code it can
// route: FailedPrecondition marks it a contract defect, so a bad binding is
// reported as the wiring fault it is instead of hiding among per-frame refusals.
//
// This is the call-site guard only. The deeper question — that a container can be
// bound to a non-agent account at all, i.e. the ordering of bindContainer's
// validation against RecordAgentContainer — belongs to that flow, not here.
func (c *Comms) homeChannel(ctx context.Context, account store.AccountID) (string, error) {
	acc, err := c.store.GetAccount(ctx, account)
	if err != nil {
		return "", edgeError(err)
	}
	if acc.Agent == nil {
		return "", errNotAgentAccount
	}
	return string(acc.Agent.HomeChannelID), nil
}
