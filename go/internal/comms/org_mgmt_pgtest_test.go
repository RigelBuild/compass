//go:build pgtest

package comms

// Agent-initiated org-management adapter contracts (RIG-2673 T2): the
// CreateChannelAsAccount / UpdateChannelMembersAsAccount / CreateChannelGroupAsAccount
// adapters run the SAME handler path a human caller takes under WithActor, so
// authz collapses to the same codes (an invisible/non-member target → the code a
// human gets), an empty account short-circuits to errNoActor (CodeInvalidArgument),
// and founding membership + the ChannelChanged fan-out are identical. Driven
// in-process against a real store and bus. context.Background() is the test root
// (test-root ctx exemption).

import (
	"context"
	"slices"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// TestCreateChannelAsAccountSeatsFounderAndEmitsChannelChanged: an agent creates
// a channel in a group its owning user owns → the Channel is returned, the
// creating agent account IS in the returned member set (founding membership via
// expandOwnerMembership, which is what lets the Manager immediately post to the
// channel it made), and a ChannelChanged is fanned out.
func TestCreateChannelAsAccountSeatsFounderAndEmitsChannelChanged(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()
	owner := mustUser(t, h.store, "owner")
	agent := mustAgent(t, h.store, owner.ID, "manager")
	grp, err := h.store.CreateChannelGroup(ctx, owner.ID, store.NewChannelGroup{Name: "team", Visibility: store.VisibilityOwner})
	if err != nil {
		t.Fatalf("CreateChannelGroup: %v", err)
	}

	events := firstEventAfterBoundary(t, h, owner.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})

	resp, err := h.svc.CreateChannelAsAccount(ctx, agent.ID, &compassv1.CreateChannelRequest{
		Name: "room", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL, GroupId: string(grp.ID),
	})
	if err != nil {
		t.Fatalf("CreateChannelAsAccount: %v", err)
	}
	members := resp.GetChannel().GetMemberAccountIds()
	if !slices.Contains(members, string(agent.ID)) {
		t.Fatalf("founding member set = %v, want it to contain the creating agent %q", members, agent.ID)
	}

	got := awaitFirst(t, events)
	cc := got.GetChannelChanged()
	if cc == nil {
		t.Fatalf("event payload = %T, want ChannelChanged", got.GetPayload())
	}
	if cc.GetChannel().GetId() != resp.GetChannel().GetId() {
		t.Fatalf("ChannelChanged channel id = %q, want %q", cc.GetChannel().GetId(), resp.GetChannel().GetId())
	}
}

// TestCreateChannelAsAccountEmptyAccountIsNoActor: an empty account short-circuits
// to errNoActor (CodeInvalidArgument) before any handler work.
func TestCreateChannelAsAccountEmptyAccountIsNoActor(t *testing.T) {
	svc, _ := newHandler(t)
	_, err := svc.CreateChannelAsAccount(context.Background(), "", &compassv1.CreateChannelRequest{Name: "room"})
	connectCodeIs(t, err, connect.CodeInvalidArgument, "CreateChannelAsAccount empty account")
}

// TestCreateChannelAsAccountInvisibleGroupIsNotFound: an agent creating a channel
// in a group it neither owns nor may see collapses to the SAME CodeNotFound a
// human non-owner gets — the agent never learns the group exists.
func TestCreateChannelAsAccountInvisibleGroupIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	stranger := mustUser(t, st, "stranger")
	strangerAgent := mustAgent(t, st, stranger.ID, "outsider")
	grp, err := st.CreateChannelGroup(ctx, owner.ID, store.NewChannelGroup{Name: "private", Visibility: store.VisibilityOwner})
	if err != nil {
		t.Fatalf("CreateChannelGroup: %v", err)
	}

	_, err = svc.CreateChannelAsAccount(ctx, strangerAgent.ID, &compassv1.CreateChannelRequest{
		Name: "intrusion", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL, GroupId: string(grp.ID),
	})
	connectCodeIs(t, err, connect.CodeNotFound, "CreateChannelAsAccount in invisible group")
}

// TestUpdateChannelMembersAsAccountAddsMember: an agent adds a member to a channel
// it authored (and so can mutate) → the updated Channel carries the new member,
// and a ChannelChanged is fanned out (parity with the human caller's path).
func TestUpdateChannelMembersAsAccountAddsMember(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()
	owner := mustUser(t, h.store, "owner")
	agent := mustAgent(t, h.store, owner.ID, "manager")
	newcomer := mustUser(t, h.store, "newcomer")

	ch, err := h.store.CreateChannel(ctx, agent.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	events := firstEventAfterBoundary(t, h, owner.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})

	resp, err := h.svc.UpdateChannelMembersAsAccount(ctx, agent.ID, &compassv1.UpdateChannelMembersRequest{
		ChannelId:        string(ch.ID),
		AddMemberHandles: []string{newcomer.Handle},
	})
	if err != nil {
		t.Fatalf("UpdateChannelMembersAsAccount: %v", err)
	}
	if !slices.Contains(resp.GetChannel().GetMemberAccountIds(), string(newcomer.ID)) {
		t.Fatalf("member set = %v, want it to contain the added %q", resp.GetChannel().GetMemberAccountIds(), newcomer.ID)
	}

	got := awaitFirst(t, events)
	cc := got.GetChannelChanged()
	if cc == nil {
		t.Fatalf("event payload = %T, want ChannelChanged", got.GetPayload())
	}
	if cc.GetChannel().GetId() != resp.GetChannel().GetId() {
		t.Fatalf("ChannelChanged channel id = %q, want %q", cc.GetChannel().GetId(), resp.GetChannel().GetId())
	}
}

// TestUpdateChannelMembersAsAccountUnknownMemberHandleIsNotFound: an agent adds a
// member naming a handle that resolves to no account → the T3 batch resolver
// (AccountsByHandles, OQ-2) fails the whole call with the oracle-safe CodeNotFound
// naming the submitted handle, identical to the code a human caller gets. The
// org-management adapter inherits that resolution; it never partially applies a
// member set with an unresolved handle in it.
func TestUpdateChannelMembersAsAccountUnknownMemberHandleIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "manager")

	ch, err := st.CreateChannel(ctx, agent.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	_, err = svc.UpdateChannelMembersAsAccount(ctx, agent.ID, &compassv1.UpdateChannelMembersRequest{
		ChannelId:        string(ch.ID),
		AddMemberHandles: []string{"ghost"},
	})
	connectCodeIs(t, err, connect.CodeNotFound, "UpdateChannelMembersAsAccount with an unresolvable member handle")
}

// TestUpdateChannelMembersAsAccountInvisibleMemberHandleIsNotFound: an agent adds
// a member naming a handle that IS a real account but one the caller cannot see —
// an agent living only under a DIFFERENT owner's per-owner namespace (DL-271). The
// bare handle misses the global user index and misses the caller-owner agent index,
// so it resolves to nothing and collapses to the SAME oracle-safe CodeNotFound an
// entirely-unknown handle gets: the caller cannot distinguish "no such handle" from
// "a handle I'm not allowed to see", so it cannot probe another owner's roster by
// naming its agents as members.
func TestUpdateChannelMembersAsAccountInvisibleMemberHandleIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "manager")
	// A second owner and its agent — a real account under a foreign per-owner
	// namespace, invisible to the caller's bare-handle resolution.
	otherOwner := mustUser(t, st, "other")
	otherAgent := mustAgent(t, st, otherOwner.ID, "hidden")

	ch, err := st.CreateChannel(ctx, agent.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	_, err = svc.UpdateChannelMembersAsAccount(ctx, agent.ID, &compassv1.UpdateChannelMembersRequest{
		ChannelId:        string(ch.ID),
		AddMemberHandles: []string{otherAgent.Handle},
	})
	connectCodeIs(t, err, connect.CodeNotFound, "UpdateChannelMembersAsAccount with a foreign-owner (invisible) member handle")
}

// TestUpdateChannelMembersAsAccountEmptyAccountIsNoActor: an empty account →
// errNoActor (CodeInvalidArgument).
func TestUpdateChannelMembersAsAccountEmptyAccountIsNoActor(t *testing.T) {
	svc, _ := newHandler(t)
	_, err := svc.UpdateChannelMembersAsAccount(context.Background(), "", &compassv1.UpdateChannelMembersRequest{ChannelId: "ch-1"})
	connectCodeIs(t, err, connect.CodeInvalidArgument, "UpdateChannelMembersAsAccount empty account")
}

// TestUpdateChannelMembersAsAccountNonMemberIsNotFound: an agent mutating a
// channel it cannot see collapses to the SAME CodeNotFound a human non-member gets.
func TestUpdateChannelMembersAsAccountNonMemberIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	stranger := mustUser(t, st, "stranger")
	strangerAgent := mustAgent(t, st, stranger.ID, "outsider")

	ch, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	_, err = svc.UpdateChannelMembersAsAccount(ctx, strangerAgent.ID, &compassv1.UpdateChannelMembersRequest{
		ChannelId:        string(ch.ID),
		AddMemberHandles: []string{stranger.Handle},
	})
	connectCodeIs(t, err, connect.CodeNotFound, "UpdateChannelMembersAsAccount on invisible channel")
}

// TestCreateChannelGroupAsAccountReturnsGroup: an agent creates a top-level group
// → the ChannelGroup is returned, created under the agent's account (the store
// stamps owner = the actor account, resolved server-side from the actor context),
// and a ChannelGroupChanged is fanned out (parity with the human caller's path).
func TestCreateChannelGroupAsAccountReturnsGroup(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()
	owner := mustUser(t, h.store, "owner")
	agent := mustAgent(t, h.store, owner.ID, "manager")

	events := firstEventAfterBoundary(t, h, agent.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})

	resp, err := h.svc.CreateChannelGroupAsAccount(ctx, agent.ID, &compassv1.CreateChannelGroupRequest{Name: "team"})
	if err != nil {
		t.Fatalf("CreateChannelGroupAsAccount: %v", err)
	}
	if resp.GetGroup().GetId() == "" {
		t.Fatalf("returned group has no id")
	}
	if resp.GetGroup().GetName() != "team" {
		t.Fatalf("returned group name = %q, want team", resp.GetGroup().GetName())
	}

	got := awaitFirst(t, events)
	cg := got.GetChannelGroupChanged()
	if cg == nil {
		t.Fatalf("event payload = %T, want ChannelGroupChanged", got.GetPayload())
	}
	if cg.GetGroup().GetId() != resp.GetGroup().GetId() {
		t.Fatalf("ChannelGroupChanged group id = %q, want %q", cg.GetGroup().GetId(), resp.GetGroup().GetId())
	}
}

// TestCreateChannelGroupAsAccountEmptyAccountIsNoActor: an empty account →
// errNoActor (CodeInvalidArgument).
func TestCreateChannelGroupAsAccountEmptyAccountIsNoActor(t *testing.T) {
	svc, _ := newHandler(t)
	_, err := svc.CreateChannelGroupAsAccount(context.Background(), "", &compassv1.CreateChannelGroupRequest{Name: "team"})
	connectCodeIs(t, err, connect.CodeInvalidArgument, "CreateChannelGroupAsAccount empty account")
}

// TestCreateChannelGroupAsAccountInvisibleParentIsNotFound: an agent creating a
// group under a parent it neither owns nor may see collapses to the SAME
// CodeNotFound a human non-owner gets.
func TestCreateChannelGroupAsAccountInvisibleParentIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	stranger := mustUser(t, st, "stranger")
	strangerAgent := mustAgent(t, st, stranger.ID, "outsider")
	parent, err := st.CreateChannelGroup(ctx, owner.ID, store.NewChannelGroup{Name: "private", Visibility: store.VisibilityOwner})
	if err != nil {
		t.Fatalf("CreateChannelGroup: %v", err)
	}

	_, err = svc.CreateChannelGroupAsAccount(ctx, strangerAgent.ID, &compassv1.CreateChannelGroupRequest{
		Name: "child", ParentGroupId: string(parent.ID), Visibility: compassv1.ChannelGroupVisibility_CHANNEL_GROUP_VISIBILITY_OWNER,
	})
	connectCodeIs(t, err, connect.CodeNotFound, "CreateChannelGroupAsAccount under invisible parent")
}
