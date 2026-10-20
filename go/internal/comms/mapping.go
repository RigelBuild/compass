package comms

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect/v2"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/fabric"
	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/refs"
	"github.com/RigelBuild/compass/go/internal/store"
)

// This file maps the compass.v1 wire messages onto the store's domain types at
// the service edge, and publishes the write-through events onto the comms bus. The
// store owns its own Go types, distinct from the generated stubs, so every
// crossing is explicit here — the one place the two shapes meet.

// ---- store -> wire ----

func accountToWire(a store.Account) *compassv1.Account {
	out := &compassv1.Account{
		Id:          string(a.ID),
		Handle:      a.Handle,
		DisplayName: a.DisplayName,
	}
	switch {
	case a.User != nil:
		out.Kind = &compassv1.Account_User{User: &compassv1.UserAccount{
			Role: userRoleToWire(a.User.Role),
		}}
	case a.Agent != nil:
		out.Kind = &compassv1.Account_Agent{Agent: &compassv1.AgentAccount{
			OwnerUserId:   string(a.Agent.OwnerUserID),
			HomeChannelId: string(a.Agent.HomeChannelID),
			ParentAgentId: string(a.Agent.ParentAgentID),
		}}
	case a.System != nil:
		out.Kind = &compassv1.Account_System{System: &compassv1.SystemAccount{}}
	}
	return out
}

// peeringToWire maps a store peering to its wire form.
func peeringToWire(p store.Peering) *compassv1.Peering {
	return &compassv1.Peering{
		UserAccountId: string(p.PeerID),
		Handle:        p.Handle,
		State:         peeringStateToWire(p.State),
	}
}

func peeringStateToWire(state store.PeeringState) compassv1.PeeringState {
	switch state {
	case store.PeeringPendingOutgoing:
		return compassv1.PeeringState_PEERING_STATE_PENDING_OUTGOING
	case store.PeeringPendingIncoming:
		return compassv1.PeeringState_PEERING_STATE_PENDING_INCOMING
	case store.PeeringApproved:
		return compassv1.PeeringState_PEERING_STATE_APPROVED
	default:
		return compassv1.PeeringState_PEERING_STATE_UNSPECIFIED
	}
}

func userRoleToWire(r store.UserRole) compassv1.UserRole {
	if r == store.UserRoleAdmin {
		return compassv1.UserRole_USER_ROLE_ADMIN
	}
	return compassv1.UserRole_USER_ROLE_MEMBER
}

func groupToWire(g store.ChannelGroup) *compassv1.ChannelGroup {
	return &compassv1.ChannelGroup{
		Id:            string(g.ID),
		Name:          g.Name,
		ParentGroupId: string(g.ParentGroupID),
		OwnerUserId:   string(g.OwnerUserID),
		Visibility:    groupVisibilityToWire(g.Visibility),
	}
}

func groupVisibilityToWire(v store.ChannelGroupVisibility) compassv1.ChannelGroupVisibility {
	if v == store.VisibilityShared {
		return compassv1.ChannelGroupVisibility_CHANNEL_GROUP_VISIBILITY_SHARED
	}
	return compassv1.ChannelGroupVisibility_CHANNEL_GROUP_VISIBILITY_OWNER
}

func channelToWire(c store.Channel) *compassv1.Channel {
	return &compassv1.Channel{
		Id:                    string(c.ID),
		Name:                  c.Name,
		GroupId:               string(c.GroupID),
		Kind:                  channelKindToWire(c.Kind),
		MemberAccountIds:      accountIDsToWire(c.MemberAccountIDs),
		SubscriberAccountIds:  accountIDsToWire(c.SubscriberAccountIDs),
		PostPolicy:            channelPostPolicyToWire(c.Policy.PostPolicy),
		OwnerAccountId:        string(c.Policy.OwnerAccountID),
		MandatorySubscription: c.Policy.MandatorySubscription,
		ParentAgentId:         string(c.ParentAgentID),
		MembershipMode:        channelMembershipModeToWire(c.MembershipMode),
	}
}

func channelMembershipModeToWire(m store.ChannelMembershipMode) compassv1.ChannelMembershipMode {
	if m == store.ChannelMembershipModeTree {
		return compassv1.ChannelMembershipMode_CHANNEL_MEMBERSHIP_MODE_TREE
	}
	return compassv1.ChannelMembershipMode_CHANNEL_MEMBERSHIP_MODE_EXPLICIT
}

// channelMembershipModeFromWire rejects an unknown mode instead of silently
// creating an EXPLICIT channel the caller did not ask for.
func channelMembershipModeFromWire(m compassv1.ChannelMembershipMode) (store.ChannelMembershipMode, error) {
	switch m {
	case compassv1.ChannelMembershipMode_CHANNEL_MEMBERSHIP_MODE_EXPLICIT:
		return store.ChannelMembershipModeExplicit, nil
	case compassv1.ChannelMembershipMode_CHANNEL_MEMBERSHIP_MODE_TREE:
		return store.ChannelMembershipModeTree, nil
	default:
		return 0, fmt.Errorf("%w: unknown membership mode %d", store.ErrInvalidArgument, m)
	}
}

func channelKindToWire(k store.ChannelKind) compassv1.ChannelKind {
	switch k {
	case store.ChannelKindDM:
		return compassv1.ChannelKind_CHANNEL_KIND_DM
	default:
		// GROUP_DM is retired (never produced; a DM converts to a named
		// CHANNEL). A legacy kind=2 row renders as a plain channel.
		return compassv1.ChannelKind_CHANNEL_KIND_CHANNEL
	}
}

func channelPostPolicyToWire(p store.ChannelPostPolicy) compassv1.ChannelPostPolicy {
	switch p {
	case store.ChannelPostPolicyOpen:
		return compassv1.ChannelPostPolicy_CHANNEL_POST_POLICY_OPEN
	case store.ChannelPostPolicyOwnerOnly:
		return compassv1.ChannelPostPolicy_CHANNEL_POST_POLICY_OWNER_ONLY
	default:
		return compassv1.ChannelPostPolicy_CHANNEL_POST_POLICY_OPEN
	}
}

func channelPostPolicyFromWire(p compassv1.ChannelPostPolicy) store.ChannelPostPolicy {
	switch p {
	case compassv1.ChannelPostPolicy_CHANNEL_POST_POLICY_OWNER_ONLY:
		return store.ChannelPostPolicyOwnerOnly
	case compassv1.ChannelPostPolicy_CHANNEL_POST_POLICY_OPEN:
		return store.ChannelPostPolicyOpen
	default:
		return store.ChannelPostPolicyOpen
	}
}

// pinnedEntriesToWire preserves store position order.
// An empty board maps to nil, not a non-nil empty repeated field.
func pinnedEntriesToWire(entries []store.PinnedEntry) []*compassv1.PinnedEntry {
	if len(entries) == 0 {
		return nil
	}
	wire := make([]*compassv1.PinnedEntry, len(entries))
	for i, e := range entries {
		wire[i] = &compassv1.PinnedEntry{
			MessageId:         string(e.MessageID),
			Position:          e.Position,
			PinnedAtUnixMs:    e.PinnedAtUnixMs,
			PinnedByAccountId: string(e.PinnedByAccountID),
		}
	}
	return wire
}

func workspaceToWire(w store.AgentWorkspace) *compassv1.AgentWorkspace {
	return &compassv1.AgentWorkspace{
		Id:             string(w.ID),
		AgentAccountId: string(w.AgentAccountID),
	}
}

// MessageToWire maps a store.Message onto the compass.v1 wire Message. Exported
// so the delivery consumer (internal/delivery, RIG-1569 T3) dispatches a
// re-read settled message through the ONE store->wire mapper rather than a
// second convention (the settle gate and the no-live-author path both re-read
// the message's current blocks from the store before dispatch).
func MessageToWire(m store.Message) *compassv1.Message {
	out := &compassv1.Message{
		Id:              string(m.ID),
		TopicId:         m.TopicID,
		ChannelId:       string(m.ChannelID),
		AuthorAccountId: string(m.AuthorAccountID),
		AuthorHandle:    m.AuthorHandle,
		AtUnixMs:        m.At.UnixMilli(),
		Blocks:          blocksToWire(m.Blocks),
		TurnSequence:    m.TurnSequence,
	}
	return out
}

func messagesToWire(ms []store.Message) []*compassv1.Message {
	out := make([]*compassv1.Message, len(ms))
	for i, m := range ms {
		out[i] = MessageToWire(m)
	}
	return out
}

// topicToWire maps a store.Topic onto the compass.v1 wire Topic — the ONE
// store->wire topic mapper, shared by the ListTopics/UpdateTopic responses and
// the TopicUpserted fan-out (design.md's live topic index).
func topicToWire(t store.Topic) *compassv1.Topic {
	wire := &compassv1.Topic{
		Id:                 t.ID,
		ChannelId:          t.ChannelID,
		Name:               t.Name,
		CreatedAtUnixMs:    t.CreatedAtUnixMS,
		CreatedByAccountId: t.CreatedByAccountID,
		Archived:           t.Archived,
	}
	if t.Stats != nil {
		count := uint64(max(t.Stats.MessageCount, 0))
		wire.MessageCount = &count
		wire.LastMessageAtUnixMs = &t.Stats.LastMessageAtUnixMS
	}
	return wire
}

func topicsToWire(ts []store.Topic) []*compassv1.Topic {
	out := make([]*compassv1.Topic, len(ts))
	for i, t := range ts {
		out[i] = topicToWire(t)
	}
	return out
}

func blocksToWire(blocks []store.MessageBlock) []*compassv1.MessageBlock {
	out := make([]*compassv1.MessageBlock, len(blocks))
	for i, b := range blocks {
		switch {
		case b.Text != nil:
			out[i] = &compassv1.MessageBlock{Block: &compassv1.MessageBlock_Text{Text: *b.Text}}
		case b.Ask != nil:
			out[i] = &compassv1.MessageBlock{Block: &compassv1.MessageBlock_Ask{Ask: askToWire(b.Ask)}}
		case b.AskAnswer != nil:
			out[i] = &compassv1.MessageBlock{Block: &compassv1.MessageBlock_AskAnswer{AskAnswer: askAnswerToWire(b.AskAnswer)}}
		}
	}
	return out
}

// askAnswerToWire maps the server-owned store AskAnswerBlock onto the wire
// variant, reusing askToWire for the answered snapshot.
func askAnswerToWire(b *store.AskAnswerBlock) *compassv1.AskAnswerBlock {
	return &compassv1.AskAnswerBlock{
		Ask:            askToWire(&b.Ask),
		AskerAccountId: string(b.AskerAccountID),
	}
}

func askToWire(a *store.Ask) *compassv1.Ask {
	questions := make([]*compassv1.AskQuestion, len(a.Questions))
	for i, q := range a.Questions {
		opts := make([]*compassv1.AskOption, len(q.Options))
		for j, o := range q.Options {
			opts[j] = &compassv1.AskOption{Id: o.ID, Label: o.Label, Description: o.Description, Preview: o.Preview}
		}
		questions[i] = &compassv1.AskQuestion{
			QuestionId:      q.QuestionID,
			Question:        q.Question,
			Header:          q.Header,
			Options:         opts,
			AllowMultiple:   q.AllowMultiple,
			Recommended:     q.Recommended,
			ChosenOptionIds: q.ChosenOptionIDs,
			CustomText:      q.CustomText,
			TimedOut:        q.TimedOut,
		}
	}
	return &compassv1.Ask{AskId: a.AskID, Questions: questions, Answered: a.Answered, Ref: a.Ref}
}

func accountIDsToWire(ids []store.AccountID) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}

// ---- wire -> store ----

func groupVisibilityFromWire(v compassv1.ChannelGroupVisibility) store.ChannelGroupVisibility {
	if v == compassv1.ChannelGroupVisibility_CHANNEL_GROUP_VISIBILITY_SHARED {
		return store.VisibilityShared
	}
	return store.VisibilityOwner
}

func channelKindFromWire(k compassv1.ChannelKind) store.ChannelKind {
	switch k {
	case compassv1.ChannelKind_CHANNEL_KIND_DM:
		return store.ChannelKindDM
	default:
		// GROUP_DM is retired: a wire GROUP_DM (only a legacy/hostile input,
		// never freshly produced) maps to a plain channel. The collapse itself
		// is the sole retirement mechanism at this edge — the retired kind can
		// never round-trip into the store.
		return store.ChannelKindChannel
	}
}

// memberUpdatesFromWire resolves all four handle lists atomically and merges updates by resolved account ID.
// Equivalent spellings collapse into one update; any miss fails with every submitted spelling.
// No mutation runs unless all handles resolve in the caller's visibility scope.
func (c *Comms) memberUpdatesFromWire(ctx context.Context, caller store.AccountID, req *compassv1.UpdateChannelMembersRequest) ([]store.MemberUpdate, error) {
	addH := req.GetAddMemberHandles()
	subscribeH := req.GetSubscribeHandles()
	unsubscribeH := req.GetUnsubscribeHandles()
	removeH := req.GetRemoveMemberHandles()

	// Resolve all four lists in one AccountsByHandles call so the OQ-2 error
	// names every unresolved handle across every list, not just the first list
	// with a miss. resolveHandles preserves submitted order, so each list's ids
	// are sliced back out by offset.
	combined := make([]string, 0, len(addH)+len(subscribeH)+len(unsubscribeH)+len(removeH))
	combined = append(combined, addH...)
	combined = append(combined, subscribeH...)
	combined = append(combined, unsubscribeH...)
	combined = append(combined, removeH...)
	resolved, err := c.resolveHandles(ctx, caller, combined)
	if err != nil {
		return nil, err
	}
	i := 0
	next := func(n int) []store.AccountID { s := resolved[i : i+n]; i += n; return s }
	add := next(len(addH))
	subscribe := next(len(subscribeH))
	unsubscribe := next(len(unsubscribeH))
	remove := next(len(removeH))

	byID := make(map[store.AccountID]*store.MemberUpdate)
	order := make([]store.AccountID, 0)
	touch := func(id store.AccountID) *store.MemberUpdate {
		if u, ok := byID[id]; ok {
			return u
		}
		u := &store.MemberUpdate{AccountID: id}
		byID[id] = u
		order = append(order, id)
		return u
	}

	for _, id := range add {
		touch(id)
	}
	for _, id := range subscribe {
		touch(id).Subscribed = true
	}
	for _, id := range unsubscribe {
		u := touch(id)
		u.Subscribed = false
		u.Unsubscribe = true
	}
	for _, id := range remove {
		touch(id).Remove = true
	}

	out := make([]store.MemberUpdate, len(order))
	for i, id := range order {
		u, ok := byID[id]
		if !ok || u == nil {
			return nil, connect.NewError(connect.CodeInternal, errMissingMemberUpdate.Error()).WithCause(errMissingMemberUpdate)
		}
		out[i] = *u
	}
	return out, nil
}

// blocksFromWire maps wire message blocks onto store blocks, rejecting an empty
// or unset block oneof as an invalid argument (the store also rejects a
// malformed oneof, but catching it at the edge gives a precise connect error).
func blocksFromWire(blocks []*compassv1.MessageBlock, githubHost string) ([]store.MessageBlock, error) {
	out := make([]store.MessageBlock, 0, len(blocks))
	for _, b := range blocks {
		switch body := b.GetBlock().(type) {
		case *compassv1.MessageBlock_Text:
			if body == nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, errEmptyBlock.Error()).WithCause(errEmptyBlock)
			}
			text := body.Text
			out = append(out, store.MessageBlock{Text: &text})
		case *compassv1.MessageBlock_Ask:
			if body == nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, errEmptyBlock.Error()).WithCause(errEmptyBlock)
			}
			ask, err := askFromWire(body.Ask, githubHost)
			if err != nil {
				return nil, err
			}
			out = append(out, store.MessageBlock{Ask: ask})
		case *compassv1.MessageBlock_AskAnswer:
			// ask_answer is server-owned: constructed only by RespondToAsk. An
			// inbound one on the POST path is rejected at this edge — the only
			// layer with caller identity (RIG-2257).
			return nil, connect.NewError(connect.CodeInvalidArgument, errServerOwnedBlock.Error()).WithCause(errServerOwnedBlock)
		default:
			return nil, connect.NewError(connect.CodeInvalidArgument,
				errEmptyBlock.Error()).WithCause(errEmptyBlock)
		}
	}
	return out, nil
}

func askFromWire(a *compassv1.Ask, githubHost string) (*store.Ask, error) {
	ref, err := askRefFromWire(a.GetRef())
	if err != nil {
		return nil, err
	}
	// ask_id is server-owned on the POST path: drop any caller-supplied value so
	// the store's mintAskIDs assigns a fresh, globally-unique id on append. Honoring
	// a wire value would let two posts share an ask_id, making RespondToAsk's
	// containment SELECT match multiple rows and answer a nondeterministic one.
	var coord *store.ForgeCoord
	if ref != "" {
		r, parseErr := refs.Parse(ref)
		if parseErr != nil {
			wrappedErr := fmt.Errorf("comms: parse canonical ask ref: %w", parseErr)
			return nil, connect.NewError(connect.CodeInvalidArgument, wrappedErr.Error()).WithCause(wrappedErr)
		}
		if r.Kind == refs.KindForge {
			if githubHost == "" {
				githubHost = "github.com"
			}
			coord = &store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: githubHost, Repo: r.Owner + "/" + r.Repo, Number: r.Number}
		} else {
			coord = &store.ForgeCoord{Provider: store.ForgeProviderLinear, Host: forge.LinearHost, Repo: r.Key, Number: r.Number}
		}
	}
	return &store.Ask{AskID: "", Ref: ref, RefCoord: coord, Questions: askQuestionsFromWire(a)}, nil
}

// askRefFromWire canonicalizes an ask's optional ref; "" means no ref.
func askRefFromWire(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	r, err := refs.Parse(raw)
	if err != nil {
		wrappedErr := fmt.Errorf("ask ref: %w", err)
		return "", connect.NewError(connect.CodeInvalidArgument, wrappedErr.Error()).WithCause(wrappedErr)
	}
	return r.String(), nil
}

// askQuestionsFromWire maps an ask's questions onto store types.
func askQuestionsFromWire(a *compassv1.Ask) []store.AskQuestion {
	questions := make([]store.AskQuestion, len(a.GetQuestions()))
	for i, q := range a.GetQuestions() {
		opts := make([]store.AskOption, len(q.GetOptions()))
		for j, o := range q.GetOptions() {
			opts[j] = store.AskOption{ID: o.GetId(), Label: o.GetLabel(), Description: o.GetDescription(), Preview: o.GetPreview()}
		}
		questions[i] = store.AskQuestion{
			QuestionID:    q.GetQuestionId(),
			Question:      q.GetQuestion(),
			Header:        q.GetHeader(),
			Options:       opts,
			AllowMultiple: q.GetAllowMultiple(),
			Recommended:   q.Recommended,
		}
	}
	return questions
}

// ---- write-through event publishers ----
// Each mutation publishes its comms event onto the bus after the store commit. The
// payload is the whole SubscribeCommsResponse with only its oneof set; the bus
// stamps seq/at_unix_ms/instance_epoch at publish and the stream edge copies them.

func (c *Comms) publishAccountChanged(ctx context.Context, a store.Account) {
	c.bus.Publish(ctx, &compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_AccountChanged{
			AccountChanged: &compassv1.AccountChanged{Account: accountToWire(a)},
		},
	})
}

func (c *Comms) publishChannelGroupChanged(ctx context.Context, g store.ChannelGroup) {
	c.bus.Publish(ctx, &compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_ChannelGroupChanged{
			ChannelGroupChanged: &compassv1.ChannelGroupChanged{Group: groupToWire(g)},
		},
	})
}

// publishChannelChanged emits current channel state plus removed accounts so departing members receive one final event.
func (c *Comms) publishChannelChanged(ctx context.Context, ch store.Channel, removed []store.AccountID) {
	c.bus.Publish(ctx, &compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_ChannelChanged{
			ChannelChanged: &compassv1.ChannelChanged{
				Channel:           channelToWire(ch),
				RemovedAccountIds: accountIDsToWire(removed),
			},
		},
	})
}

// publishHomeChannelChanged announces the home channel the store created with
// the agent; an open client otherwise holds its id with no channel to render.
// The read follows a committed create, so a caller that hung up must not drop it.
func (c *Comms) publishHomeChannelChanged(ctx context.Context, channelID store.ChannelID) {
	ctx = context.WithoutCancel(ctx)
	channel, err := c.store.GetChannel(ctx, channelID)
	if err != nil {
		slog.ErrorContext(ctx, "comms: reading new home channel for event", "channel_id", channelID, "err", err)
		return
	}
	c.publishChannelChanged(ctx, channel, nil)
}

func (c *Comms) publishAgentWorkspaceChanged(ctx context.Context, w store.AgentWorkspace) {
	c.bus.Publish(ctx, &compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_AgentWorkspaceChanged{
			AgentWorkspaceChanged: &compassv1.AgentWorkspaceChanged{Workspace: workspaceToWire(w)},
		},
	})
}

// fabricPublishTimeout bounds the post-commit publish once it no longer follows
// the request's cancellation, so a stalled JetStream ack cannot pin the RPC.
const fabricPublishTimeout = 5 * time.Second

func (c *Comms) publishMessagePosted(ctx context.Context, m store.Message) {
	c.bus.Publish(ctx, &compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_MessagePosted{
			MessagePosted: &compassv1.MessagePosted{Message: MessageToWire(m), TurnSequence: m.TurnSequence},
		},
	})
	if c.fabric == nil {
		return
	}
	tenant := string(c.store.EffectiveTenant(ctx))
	ref := fabric.EventRef{Tenant: tenant, Kind: fabric.KindMessagePosted, RowID: string(m.ID)}
	subject, err := fabric.CommsSubject(tenant, fabric.KindMessagePosted)
	if err == nil {
		// A retry of this post is idempotent and never republishes, so a client
		// hanging up after commit must not cancel the only publish attempt.
		pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fabricPublishTimeout)
		err = c.fabric.Publish(pubCtx, subject, ref)
		cancel()
	}
	// The row is already committed, so failing the RPC would report a persisted
	// write as lost; the delivery recovery sweep owns redelivery instead.
	if err != nil {
		slog.ErrorContext(ctx, "comms: publishing message_posted to fabric failed", "error", err, "message_id", string(m.ID))
		if c.fabricPublishFailures != nil {
			c.fabricPublishFailures.Add(ctx, 1)
		}
	}
}

func (c *Comms) publishMessageUpdated(ctx context.Context, m store.Message) {
	c.bus.Publish(ctx, &compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_MessageUpdated{
			MessageUpdated: &compassv1.MessageUpdated{Message: MessageToWire(m)},
		},
	})
}

// publishTopicUpserted emits a TopicUpserted carrying the topic's current state
// after a topic create/rename/merge/archive commit (write-through fan-out), so
// a live topic index stays current without re-reading (design.md: the new event
// covers the topic namespace the way MessagePosted covers messages).
func (c *Comms) publishTopicUpserted(ctx context.Context, t store.Topic) {
	c.bus.Publish(ctx, &compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_TopicUpserted{
			TopicUpserted: &compassv1.TopicUpserted{Topic: topicToWire(t)},
		},
	})
}
