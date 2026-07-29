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
		Id:                   string(c.ID),
		Name:                 c.Name,
		GroupId:              string(c.GroupID),
		Kind:                 channelKindToWire(c.Kind),
		MemberAccountIds:     accountIDsToWire(c.MemberAccountIDs),
		SubscriberAccountIds: accountIDsToWire(c.SubscriberAccountIDs),
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

func workspaceToWire(w store.AgentWorkspace) *compassv1.AgentWorkspace {
	return &compassv1.AgentWorkspace{
		Id:             string(w.ID),
		AgentAccountId: string(w.AgentAccountID),
	}
}

func messageToWire(m store.Message) *compassv1.Message {
	out := &compassv1.Message{
		Id:              string(m.ID),
		Container:       &compassv1.Message_ChannelId{ChannelId: string(m.Container.ChannelID)},
		AuthorAccountId: string(m.AuthorAccountID),
		AtUnixMs:        m.At.UnixMilli(),
		Blocks:          blocksToWire(m.Blocks),
		ParentMessageId: string(m.ParentMessageID),
	}
	return out
}

func messagesToWire(ms []store.Message) []*compassv1.Message {
	out := make([]*compassv1.Message, len(ms))
	for i, m := range ms {
		out[i] = messageToWire(m)
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

func askFromWire(a *compassv1.Ask) *store.Ask {
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
	// ask_id and every answer-state field are server-owned: drop any
	// caller-supplied value.
	//
	// ask_id, because the store's mintAskIDs must assign a fresh globally-unique
	// id on append (comms.proto: "server-assigned and globally unique").
	// Honoring a wire value would let two posts share an ask_id, making
	// RespondToAsk's containment SELECT match multiple rows and answer a
	// nondeterministic one.
	//
	// The answer state — Answered, and the per-question ChosenOptionIDs /
	// CustomText / TimedOut — because an ask arriving over the wire has by
	// definition not been answered yet: it is being posted, and only
	// RespondToAsk may record an answer. Honoring them let a caller post an ask
	// that arrives pre-populated, which a client reading the per-question
	// fields renders as already settled while the server still holds it
	// pending and answerable. Zeroing them by construction (omitted from the
	// composite literal, not stripped afterwards) means there is no line to
	// delete without a compile-visible change.
	return &store.Ask{AskID: "", Questions: questions}
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
			MessagePosted: &compassv1.MessagePosted{Message: messageToWire(m)},
		},
	})
}

func (c *Comms) publishMessageUpdated(m store.Message) {
	c.bus.Publish(&compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_MessageUpdated{
			MessageUpdated: &compassv1.MessageUpdated{Message: messageToWire(m)},
		},
	})
}
