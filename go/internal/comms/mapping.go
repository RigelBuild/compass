package comms

import (
	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// This file maps the compass.v1 wire messages onto the store's domain types at
// the service edge (store types.go:9-12: "the comms service maps proto <-> store
// at their edge"), and publishes the write-through events onto the comms bus.
// The store owns its own Go types, distinct from the generated stubs, so every
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
	}
	return out
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
	}
}

func channelKindToWire(k store.ChannelKind) compassv1.ChannelKind {
	switch k {
	case store.ChannelKindDM:
		return compassv1.ChannelKind_CHANNEL_KIND_DM
	case store.ChannelKindGroupDM:
		return compassv1.ChannelKind_CHANNEL_KIND_GROUP_DM
	default:
		return compassv1.ChannelKind_CHANNEL_KIND_CHANNEL
	}
}

func channelPostPolicyToWire(p store.ChannelPostPolicy) compassv1.ChannelPostPolicy {
	if p == store.ChannelPostPolicyOwnerOnly {
		return compassv1.ChannelPostPolicy_CHANNEL_POST_POLICY_OWNER_ONLY
	}
	return compassv1.ChannelPostPolicy_CHANNEL_POST_POLICY_OPEN
}

func channelPostPolicyFromWire(p compassv1.ChannelPostPolicy) store.ChannelPostPolicy {
	if p == compassv1.ChannelPostPolicy_CHANNEL_POST_POLICY_OWNER_ONLY {
		return store.ChannelPostPolicyOwnerOnly
	}
	return store.ChannelPostPolicyOpen
}

func workspaceToWire(w store.AgentWorkspace) *compassv1.AgentWorkspace {
	return &compassv1.AgentWorkspace{
		Id:             string(w.ID),
		AgentAccountId: string(w.AgentAccountID),
	}
}

// MessageToWire maps a store.Message onto the compass.v1 wire Message. Exported
// so the delivery consumer (internal/delivery, SEA-1569 T3) dispatches a
// re-read settled message through the ONE store->wire mapper rather than a
// second convention (the settle gate and the no-live-author path both re-read
// the message's current blocks from the store before dispatch).
func MessageToWire(m store.Message) *compassv1.Message {
	out := &compassv1.Message{
		Id:              string(m.ID),
		TopicId:         m.TopicID,
		AuthorAccountId: string(m.AuthorAccountID),
		AtUnixMs:        m.At.UnixMilli(),
		Blocks:          blocksToWire(m.Blocks),
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
	return &compassv1.Topic{
		Id:                 t.ID,
		ChannelId:          t.ChannelID,
		Name:               t.Name,
		CreatedAtUnixMs:    t.CreatedAtUnixMS,
		CreatedByAccountId: t.CreatedByAccountID,
		Archived:           t.Archived,
	}
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
		}
	}
	return out
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
	return &compassv1.Ask{AskId: a.AskID, Questions: questions, Answered: a.Answered}
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
	case compassv1.ChannelKind_CHANNEL_KIND_GROUP_DM:
		return store.ChannelKindGroupDM
	default:
		return store.ChannelKindChannel
	}
}

func accountIDsFromWire(ids []string) []store.AccountID {
	if len(ids) == 0 {
		return nil
	}
	out := make([]store.AccountID, len(ids))
	for i, id := range ids {
		out[i] = store.AccountID(id)
	}
	return out
}

// memberUpdatesFromWire collapses the UpdateChannelMembers request's four
// parallel lists (add / remove / subscribe / unsubscribe) into the store's
// per-member MemberUpdate set (RT-1). A removed member is one update with
// Remove; an added member and a subscribe-toggle for an existing member merge
// onto the same MemberUpdate so one member never yields two conflicting rows.
func memberUpdatesFromWire(req *compassv1.UpdateChannelMembersRequest) []store.MemberUpdate {
	byID := make(map[store.AccountID]*store.MemberUpdate)
	upd := func(id store.AccountID) *store.MemberUpdate {
		if u, ok := byID[id]; ok {
			return u
		}
		u := &store.MemberUpdate{AccountID: id}
		byID[id] = u
		return u
	}
	order := make([]store.AccountID, 0)
	touch := func(id store.AccountID) *store.MemberUpdate {
		if _, seen := byID[id]; !seen {
			order = append(order, id)
		}
		return upd(id)
	}

	for _, id := range req.GetAddMemberAccountIds() {
		touch(store.AccountID(id))
	}
	for _, id := range req.GetSubscribeAccountIds() {
		touch(store.AccountID(id)).Subscribed = true
	}
	for _, id := range req.GetUnsubscribeAccountIds() {
		u := touch(store.AccountID(id))
		u.Subscribed = false
		u.Unsubscribe = true
	}
	for _, id := range req.GetRemoveMemberAccountIds() {
		touch(store.AccountID(id)).Remove = true
	}

	out := make([]store.MemberUpdate, len(order))
	for i, id := range order {
		out[i] = *byID[id]
	}
	return out
}

// blocksFromWire maps wire message blocks onto store blocks, rejecting an empty
// or unset block oneof as an invalid argument (the store also rejects a
// malformed oneof, but catching it at the edge gives a precise connect error).
func blocksFromWire(blocks []*compassv1.MessageBlock) ([]store.MessageBlock, error) {
	out := make([]store.MessageBlock, 0, len(blocks))
	for _, b := range blocks {
		switch body := b.GetBlock().(type) {
		case *compassv1.MessageBlock_Text:
			text := body.Text
			out = append(out, store.MessageBlock{Text: &text})
		case *compassv1.MessageBlock_Ask:
			out = append(out, store.MessageBlock{Ask: askFromWire(body.Ask)})
		default:
			return nil, connect.NewError(connect.CodeInvalidArgument,
				errEmptyBlock)
		}
	}
	return out, nil
}

// updateBlocksFromWire maps wire blocks for the UPDATE write-through
// (CommitAgentUpdate), differing from blocksFromWire in ONE respect: an ask
// block PRESERVES its wire ask_id rather than stripping it. Stripping is correct
// for a POST (the server mints a fresh id via mintAskIDs), but an update carries
// the id the append already minted — dropping it would make every ask-bearing
// update fail the store's immutable-ask_id check. The preserved value is NOT
// trusted as-is: CommitAgentUpdate reconciles it against the stored row before
// the write (an empty id is filled from the stored ask, a non-empty id that
// disagrees with the stored one is rejected as a forged frame).
func updateBlocksFromWire(blocks []*compassv1.MessageBlock) ([]store.MessageBlock, error) {
	out := make([]store.MessageBlock, 0, len(blocks))
	for _, b := range blocks {
		switch body := b.GetBlock().(type) {
		case *compassv1.MessageBlock_Text:
			text := body.Text
			out = append(out, store.MessageBlock{Text: &text})
		case *compassv1.MessageBlock_Ask:
			out = append(out, store.MessageBlock{Ask: askFromWireForUpdate(body.Ask)})
		default:
			return nil, connect.NewError(connect.CodeInvalidArgument,
				errEmptyBlock)
		}
	}
	return out, nil
}

func askFromWire(a *compassv1.Ask) *store.Ask {
	// ask_id is server-owned on the POST path: drop any caller-supplied value so
	// the store's mintAskIDs always assigns a fresh, globally-unique id on append
	// (comms.proto: "server-assigned and globally unique"). Honoring a wire value
	// would let two posts share an ask_id, making RespondToAsk's containment
	// SELECT match multiple rows and answer a nondeterministic one.
	return &store.Ask{AskID: "", Questions: askQuestionsFromWire(a)}
}

// askFromWireForUpdate is askFromWire's UPDATE sibling: it PRESERVES the wire
// ask_id (an update must carry back the id minted at append) instead of
// stripping it. Safe only because CommitAgentUpdate reconciles the preserved
// value against the immutable stored ask_id before writing — this mapper alone
// does not trust it.
func askFromWireForUpdate(a *compassv1.Ask) *store.Ask {
	return &store.Ask{AskID: a.GetAskId(), Questions: askQuestionsFromWire(a)}
}

// askQuestionsFromWire maps an ask's questions onto store types, shared by the
// POST (askFromWire) and UPDATE (askFromWireForUpdate) mappers — the two differ
// only in ask_id handling, never in how the question set crosses the edge.
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
//
// Each mutation publishes the corresponding comms event onto the bus after the
// store commit (write-through fan-out, design.md:1198-1201). The bus payload is
// the whole SubscribeCommsResponse with only its payload oneof set; the bus
// stamps seq/at_unix_ms/instance_epoch at publish, and the stream edge
// (subscribe.go) copies them onto the delivered response.

func (c *Comms) publishAccountChanged(a store.Account) {
	c.bus.Publish(&compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_AccountChanged{
			AccountChanged: &compassv1.AccountChanged{Account: accountToWire(a)},
		},
	})
}

func (c *Comms) publishChannelGroupChanged(g store.ChannelGroup) {
	c.bus.Publish(&compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_ChannelGroupChanged{
			ChannelGroupChanged: &compassv1.ChannelGroupChanged{Group: groupToWire(g)},
		},
	})
}

// publishChannelChanged emits a ChannelChanged carrying the channel's current
// state plus the accounts this change removed (nil on a create or pure add), so
// the stream edge can deliver a departing member its one final event before the
// channel goes silent to it (the removed member no longer matches the
// membership-visibility filter on the channel's current member set).
func (c *Comms) publishChannelChanged(ch store.Channel, removed []store.AccountID) {
	c.bus.Publish(&compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_ChannelChanged{
			ChannelChanged: &compassv1.ChannelChanged{
				Channel:           channelToWire(ch),
				RemovedAccountIds: accountIDsToWire(removed),
			},
		},
	})
}

func (c *Comms) publishAgentWorkspaceChanged(w store.AgentWorkspace) {
	c.bus.Publish(&compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_AgentWorkspaceChanged{
			AgentWorkspaceChanged: &compassv1.AgentWorkspaceChanged{Workspace: workspaceToWire(w)},
		},
	})
}

func (c *Comms) publishMessagePosted(m store.Message) {
	c.bus.Publish(&compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_MessagePosted{
			MessagePosted: &compassv1.MessagePosted{Message: MessageToWire(m)},
		},
	})
}

func (c *Comms) publishMessageUpdated(m store.Message) {
	c.bus.Publish(&compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_MessageUpdated{
			MessageUpdated: &compassv1.MessageUpdated{Message: MessageToWire(m)},
		},
	})
}

// publishTopicUpserted emits a TopicUpserted carrying the topic's current state
// after a topic create/rename/merge/archive commit (write-through fan-out), so
// a live topic index stays current without re-reading (design.md: the new event
// covers the topic namespace the way MessagePosted covers messages).
func (c *Comms) publishTopicUpserted(t store.Topic) {
	c.bus.Publish(&compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_TopicUpserted{
			TopicUpserted: &compassv1.TopicUpserted{Topic: topicToWire(t)},
		},
	})
}
