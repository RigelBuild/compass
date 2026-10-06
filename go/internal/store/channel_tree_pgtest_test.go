//go:build pgtest

package store

import (
	"context"
	"testing"
)

func mustAttachedChannel(t *testing.T, s *Store, actor, parent AccountID, name string, mode ChannelMembershipMode, members ...AccountID) Channel {
	t.Helper()
	ch, err := s.CreateChannel(t.Context(), actor, NewChannel{
		Name: name, Kind: ChannelKindChannel, ParentAgentID: parent,
		MembershipMode: mode, MemberAccountIDs: members,
	})
	if err != nil {
		t.Fatalf("CreateChannel(%q): %v", name, err)
	}
	return ch
}

func channelTreeState(t *testing.T, s *Store, id ChannelID) (string, int16, int) {
	t.Helper()
	var parent string
	var mode int16
	var members int
	if err := s.pool.QueryRow(t.Context(), `
		SELECT COALESCE(parent_agent_id, ''), membership_mode,
		       (SELECT count(*) FROM channel_members WHERE channel_id = channels.id)
		FROM channels WHERE id = $1`, string(id)).Scan(&parent, &mode, &members); err != nil {
		t.Fatalf("read channel tree state: %v", err)
	}
	return parent, mode, members
}

func TestChannelTreeCreateUnderAgent(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "tree-owner")
	anchor := mustAgent(t, s, owner.ID, "tree-anchor")

	explicit := mustAttachedChannel(t, s, owner.ID, anchor.ID, "explicit", ChannelMembershipModeExplicit, anchor.ID)
	parent, mode, members := channelTreeState(t, s, explicit.ID)
	if parent != string(anchor.ID) || mode != int16(ChannelMembershipModeExplicit) || members != 2 {
		t.Fatalf("explicit channel state = (%q, %d, %d), want (%q, 0, 2)", parent, mode, members, anchor.ID)
	}
	if len(explicit.MemberAccountIDs) != 2 {
		t.Fatalf("explicit returned members = %v, want owner and agent", explicit.MemberAccountIDs)
	}

	tree := mustAttachedChannel(t, s, owner.ID, anchor.ID, "tree", ChannelMembershipModeTree)
	parent, mode, members = channelTreeState(t, s, tree.ID)
	if parent != string(anchor.ID) || mode != int16(ChannelMembershipModeTree) || members != 0 {
		t.Fatalf("tree channel state = (%q, %d, %d), want (%q, 1, 0)", parent, mode, members, anchor.ID)
	}
	if tree.MemberAccountIDs != nil {
		t.Fatalf("tree returned members = %v, want nil before derived reads", tree.MemberAccountIDs)
	}
}

func TestChannelTreeCreateAuthorizationAndInputRefusals(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "create-owner")
	other := mustUser(t, s, "create-other")
	agent := mustAgent(t, s, owner.ID, "create-agent")
	target := mustAgent(t, s, owner.ID, "create-target")
	foreign := mustAgent(t, s, other.ID, "create-foreign")

	if _, err := s.CreateChannel(t.Context(), agent.ID, NewChannel{
		Name: "agent-created", ParentAgentID: target.ID,
	}); err != nil {
		t.Fatalf("same-owner agent attach: %v", err)
	}
	_, err := s.CreateChannel(t.Context(), owner.ID, NewChannel{Name: "cross-owner", ParentAgentID: foreign.ID})
	sentinelIs(t, err, ErrNotFound, "cross-owner channel attach")
	_, err = s.CreateChannel(t.Context(), owner.ID, NewChannel{Name: "unknown-agent", ParentAgentID: AccountID("missing-agent")})
	sentinelIs(t, err, ErrNotFound, "unknown agent attach")

	ownerOnly := ChannelPolicy{PostPolicy: ChannelPostPolicyOwnerOnly, OwnerAccountID: owner.ID}
	cases := []struct {
		name  string
		input NewChannel
	}{
		{name: "both parents", input: NewChannel{Name: "both", GroupID: ChannelGroupID("group"), ParentAgentID: agent.ID}},
		{name: "tree without anchor", input: NewChannel{Name: "no-anchor", MembershipMode: ChannelMembershipModeTree}},
		{name: "tree mandatory", input: NewChannel{Name: "mandatory", ParentAgentID: agent.ID, MembershipMode: ChannelMembershipModeTree, Policy: ChannelPolicy{MandatorySubscription: true}}},
		{name: "tree owner only", input: NewChannel{Name: "owner-only", ParentAgentID: agent.ID, MembershipMode: ChannelMembershipModeTree, Policy: ownerOnly}},
		{name: "unknown membership mode", input: NewChannel{Name: "bad-mode", ParentAgentID: agent.ID, MembershipMode: ChannelMembershipMode(7)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateChannel(t.Context(), owner.ID, tc.input)
			sentinelIs(t, err, ErrInvalidArgument, tc.name)
		})
	}
}

func TestChannelTreeNameNamespace(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "namespace-owner")
	first := mustAgent(t, s, owner.ID, "namespace-first")
	second := mustAgent(t, s, owner.ID, "namespace-second")
	mustAttachedChannel(t, s, owner.ID, first.ID, "same", ChannelMembershipModeExplicit)
	_, err := s.CreateChannel(t.Context(), owner.ID, NewChannel{Name: "same", Kind: ChannelKindChannel, ParentAgentID: first.ID})
	sentinelIs(t, err, ErrConflict, "duplicate channel name under one agent")
	if _, err := s.CreateChannel(t.Context(), owner.ID, NewChannel{Name: "same", Kind: ChannelKindChannel, ParentAgentID: second.ID}); err != nil {
		t.Fatalf("same channel name under different agents: %v", err)
	}
}

func TestReparentChannelShapeRefusals(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "shape-owner")
	outsider := mustUser(t, s, "shape-outsider")
	anchor := mustAgent(t, s, owner.ID, "shape-anchor")
	group, err := s.CreateChannelGroup(t.Context(), owner.ID, NewChannelGroup{Name: "shape-group"})
	if err != nil {
		t.Fatalf("CreateChannelGroup: %v", err)
	}
	grouped, err := s.CreateChannel(t.Context(), owner.ID, NewChannel{Name: "grouped", GroupID: group.ID})
	if err != nil {
		t.Fatalf("CreateChannel(grouped): %v", err)
	}
	dm, err := s.CreateChannel(t.Context(), owner.ID, NewChannel{Name: "dm", Kind: ChannelKindDM})
	if err != nil {
		t.Fatalf("CreateChannel(DM): %v", err)
	}
	tree := mustAttachedChannel(t, s, owner.ID, anchor.ID, "tree", ChannelMembershipModeTree)

	cases := []struct {
		name      string
		actor     AccountID
		channelID ChannelID
		parent    AccountID
		want      error
	}{
		{name: "grouped channel", actor: owner.ID, channelID: grouped.ID, want: ErrInvalidArgument},
		{name: "DM kind", actor: owner.ID, channelID: dm.ID, parent: anchor.ID, want: ErrInvalidArgument},
		{name: "home channel", actor: owner.ID, channelID: anchor.Agent.HomeChannelID, parent: anchor.ID, want: ErrInvalidArgument},
		{name: "tree detach to root", actor: owner.ID, channelID: tree.ID, want: ErrInvalidArgument},
		{name: "non-participant grouped channel", actor: outsider.ID, channelID: grouped.ID, want: ErrNotFound},
		{name: "non-participant DM", actor: outsider.ID, channelID: dm.ID, want: ErrNotFound},
		{name: "non-participant home channel", actor: outsider.ID, channelID: anchor.Agent.HomeChannelID, want: ErrNotFound},
		{name: "non-participant tree channel", actor: outsider.ID, channelID: tree.ID, want: ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.ReparentChannel(t.Context(), tc.actor, tc.channelID, tc.parent)
			sentinelIs(t, err, tc.want, tc.name)
		})
	}
	_, err = s.ReparentChannel(t.Context(), owner.ID, ChannelID("unknown-channel"), anchor.ID)
	sentinelIs(t, err, ErrNotFound, "unknown channel")
}

func TestReparentChannelOwnerBoundaryAndDestination(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "move-owner")
	other := mustUser(t, s, "move-other")
	anchor := mustAgent(t, s, owner.ID, "move-anchor")
	foreign := mustAgent(t, s, other.ID, "move-foreign")
	ch := mustAttachedChannel(t, s, owner.ID, anchor.ID, "explicit", ChannelMembershipModeExplicit)

	_, err := s.ReparentChannel(t.Context(), owner.ID, ch.ID, foreign.ID)
	sentinelIs(t, err, ErrNotFound, "cross-owner destination")
	_, err = s.ReparentChannel(t.Context(), owner.ID, ch.ID, AccountID("missing-agent"))
	sentinelIs(t, err, ErrNotFound, "unknown destination agent")
}

func TestReparentChannelLeafAllowsDescendantAgent(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "leaf-owner")
	anchor := mustAgent(t, s, owner.ID, "leaf-anchor")
	descendant := mustAgentWithParent(t, s, owner.ID, anchor.ID, "leaf-descendant")
	ch := mustAttachedChannel(t, s, owner.ID, anchor.ID, "tree", ChannelMembershipModeTree)

	// A channel is a leaf, so anchoring it below an agent descendant cannot cycle.
	moved, err := s.ReparentChannel(t.Context(), descendant.ID, ch.ID, descendant.ID)
	if err != nil {
		t.Fatalf("descendant re-anchors ancestor channel: %v", err)
	}
	parent, mode, members := channelTreeState(t, s, ch.ID)
	if parent != string(descendant.ID) || mode != int16(ChannelMembershipModeTree) || members != 0 {
		t.Fatalf("reparented tree channel state = (%q, %d, %d), want (%q, 1, 0)", parent, mode, members, descendant.ID)
	}
	if moved.ID != ch.ID {
		t.Fatalf("reparented channel id = %q, want %q", moved.ID, ch.ID)
	}
}

func TestReparentExplicitChannelToRoot(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "detach-owner")
	anchor := mustAgent(t, s, owner.ID, "detach-anchor")
	ch := mustAttachedChannel(t, s, owner.ID, anchor.ID, "explicit", ChannelMembershipModeExplicit)

	if _, err := s.ReparentChannel(t.Context(), owner.ID, ch.ID, ""); err != nil {
		t.Fatalf("detach explicit channel: %v", err)
	}
	parent, mode, _ := channelTreeState(t, s, ch.ID)
	if parent != "" || mode != int16(ChannelMembershipModeExplicit) {
		t.Fatalf("detached channel state = (%q, %d), want root explicit", parent, mode)
	}
}

func TestReparentChannelDuplicateNameAtDestination(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "duplicate-owner")
	first := mustAgent(t, s, owner.ID, "duplicate-first")
	second := mustAgent(t, s, owner.ID, "duplicate-second")
	ch := mustAttachedChannel(t, s, owner.ID, first.ID, "duplicate", ChannelMembershipModeExplicit)
	mustAttachedChannel(t, s, owner.ID, second.ID, "duplicate", ChannelMembershipModeExplicit)

	_, err := s.ReparentChannel(t.Context(), owner.ID, ch.ID, second.ID)
	sentinelIs(t, err, ErrConflict, "duplicate channel name at destination")
	parent, _, _ := channelTreeState(t, s, ch.ID)
	if parent != string(first.ID) {
		t.Fatalf("failed reparent changed source parent to %q, want %q", parent, first.ID)
	}
}

func TestConvertedDMOwnersCanAttachToOwnAgents(t *testing.T) {
	s := newTestStore(t)
	ownerA := mustUser(t, s, "converted-owner-a")
	ownerB := mustUser(t, s, "converted-owner-b")
	ownerC := mustUser(t, s, "converted-owner-c")
	agentA := mustAgent(t, s, ownerA.ID, "converted-a")
	agentB := mustAgent(t, s, ownerB.ID, "converted-b")
	agentC := mustAgent(t, s, ownerC.ID, "converted-c")

	channelID, _ := openDM(t, s, ownerA.ID, "dm--converted-a--converted-b", []AccountID{agentA.ID, agentB.ID})
	if _, _, err := s.UpdateChannelMembers(t.Context(), agentA.ID, channelID,
		[]MemberUpdate{{AccountID: agentC.ID}}, MemberUpdatesOptions{ConvertChannelName: "converted-room"}); err != nil {
		t.Fatalf("convert DM: %v", err)
	}
	for _, tc := range []struct {
		name   string
		actor  AccountID
		parent AccountID
	}{
		{name: "owner A", actor: ownerA.ID, parent: agentA.ID},
		{name: "owner B", actor: ownerB.ID, parent: agentB.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.ReparentChannel(t.Context(), tc.actor, channelID, tc.parent); err != nil {
				t.Fatalf("attach converted DM by %s: %v", tc.name, err)
			}
			parent, _, _ := channelTreeState(t, s, channelID)
			if parent != string(tc.parent) {
				t.Fatalf("converted channel parent = %q, want %q", parent, tc.parent)
			}
		})
	}
}

func TestChannelTreeMembershipModeDoesNotChangeAfterCreate(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "immutable-owner")
	other := mustUser(t, s, "immutable-other")
	first := mustAgent(t, s, owner.ID, "immutable-first")
	second := mustAgent(t, s, owner.ID, "immutable-second")
	explicit := mustAttachedChannel(t, s, owner.ID, first.ID, "explicit", ChannelMembershipModeExplicit, first.ID)
	tree := mustAttachedChannel(t, s, owner.ID, first.ID, "tree", ChannelMembershipModeTree)

	if _, _, err := s.UpdateChannelMembers(t.Context(), owner.ID, explicit.ID,
		[]MemberUpdate{{AccountID: other.ID, Subscribed: true}}, MemberUpdatesOptions{}); err != nil {
		t.Fatalf("UpdateChannelMembers(explicit): %v", err)
	}
	if _, err := s.SetChannelPolicy(t.Context(), owner.ID, explicit.ID, ChannelPolicy{}); err != nil {
		t.Fatalf("SetChannelPolicy(explicit): %v", err)
	}
	message, _, err := s.AppendMessage(t.Context(), Message{
		AuthorAccountID: owner.ID, Blocks: []MessageBlock{textBlock("pin target")},
	}, string(explicit.ID), TopicRef{Name: "general", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage(explicit): %v", err)
	}
	if _, err := s.PinMessage(t.Context(), explicit.ID, message.ID, "", owner.ID); err != nil {
		t.Fatalf("PinMessage(explicit): %v", err)
	}
	if _, err := s.ReparentChannel(t.Context(), owner.ID, explicit.ID, second.ID); err != nil {
		t.Fatalf("ReparentChannel(explicit): %v", err)
	}

	// The outcome of each write does not matter here, only that none of them
	// moves the mode; a refused write is as good as a committed one.
	_, _, err = s.UpdateChannelMembers(t.Context(), owner.ID, tree.ID,
		[]MemberUpdate{{AccountID: other.ID, Subscribed: true}}, MemberUpdatesOptions{})
	t.Logf("UpdateChannelMembers(tree): %v", err)
	_, err = s.SetChannelPolicy(t.Context(), owner.ID, tree.ID, ChannelPolicy{})
	t.Logf("SetChannelPolicy(tree): %v", err)
	if _, err := s.ReparentChannel(t.Context(), owner.ID, tree.ID, second.ID); err != nil {
		t.Fatalf("ReparentChannel(tree): %v", err)
	}

	_, explicitMode, _ := channelTreeState(t, s, explicit.ID)
	_, treeMode, _ := channelTreeState(t, s, tree.ID)
	if explicitMode != int16(ChannelMembershipModeExplicit) || treeMode != int16(ChannelMembershipModeTree) {
		t.Fatalf("membership modes changed: explicit=%d tree=%d", explicitMode, treeMode)
	}
}

func TestChannelTreeSubscriptionsTenantIsolation(t *testing.T) {
	s := newTestStore(t)
	tenantA := seedTenant(t, s, "channel-subscriptions-a")
	tenantB := seedTenant(t, s, "channel-subscriptions-b")
	ctxA := WithTenant(t.Context(), tenantA)
	ctxB := WithTenant(t.Context(), tenantB)
	owner, err := s.CreateUser(ctxA, NewUser{Handle: "subscription-owner", DisplayName: "subscription-owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	channel, err := s.CreateChannel(ctxA, owner.ID, NewChannel{Name: "subscription-room"})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	tx, err := s.beginTenantTx(ctxA)
	if err != nil {
		t.Fatalf("begin tenant A insert: %v", err)
	}
	if _, err := tx.Exec(ctxA, `INSERT INTO channel_subscriptions (channel_id, account_id, subscribed) VALUES ($1, $2, TRUE)`, string(channel.ID), string(owner.ID)); err != nil {
		t.Fatalf("insert tenant A subscription: %v", err)
	}
	if err := tx.Commit(ctxA); err != nil {
		t.Fatalf("commit tenant A subscription: %v", err)
	}

	count := func(ctx context.Context) int {
		t.Helper()
		tx, err := s.beginTenantTx(ctx)
		if err != nil {
			t.Fatalf("begin scoped subscription read: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var rows int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM channel_subscriptions WHERE channel_id = $1`, string(channel.ID)).Scan(&rows); err != nil {
			t.Fatalf("read scoped subscriptions: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit scoped subscription read: %v", err)
		}
		return rows
	}
	if got := count(ctxB); got != 0 {
		t.Fatalf("tenant B read %d tenant A subscriptions, want 0", got)
	}
	if got := count(ctxA); got != 1 {
		t.Fatalf("tenant A read %d subscriptions, want 1", got)
	}
}
