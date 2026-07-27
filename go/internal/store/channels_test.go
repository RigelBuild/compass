//go:build pgtest

package store

// Channel and group contracts: the child ≤ parent visibility ceiling, transitive
// owner-membership on channel creation, the D9 visibility lattice as seen by
// ListChannelGroups / ListChannels (effective visibility is the most-restrictive
// value on the path to root; DM/ungrouped access is membership-only), the RT-1
// member mutations of UpdateChannelMembers, and idempotent OpenAgentWorkspace.

import (
	"context"
	"testing"
)

func TestCreateChannelGroupCeilingRejectsWiderChild(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")

	parent, err := s.CreateChannelGroup(ctx, owner.ID, NewChannelGroup{Name: "private", Visibility: VisibilityOwner})
	if err != nil {
		t.Fatalf("CreateChannelGroup(parent): %v", err)
	}

	// A SHARED child under an OWNER parent is wider than its parent — rejected.
	_, err = s.CreateChannelGroup(ctx, owner.ID, NewChannelGroup{
		Name: "leak", ParentGroupID: parent.ID, Visibility: VisibilityShared,
	})
	sentinelIs(t, err, ErrInvalidArgument, "shared child under owner parent")

	// An OWNER child under the same OWNER parent is within the ceiling — allowed.
	if _, err := s.CreateChannelGroup(ctx, owner.ID, NewChannelGroup{
		Name: "ok", ParentGroupID: parent.ID, Visibility: VisibilityOwner,
	}); err != nil {
		t.Fatalf("CreateChannelGroup(owner child): %v", err)
	}
}

func TestCreateChannelGroupUnknownParentNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	// An unknown ParentGroupID is not authorized (it names no group the caller
	// may create under), so it collapses to ErrNotFound — the same error a real
	// but unauthorized parent returns, so a stranger cannot probe which group
	// ids exist (the not-found/forbidden merge in requireGroupCreateAuthz).
	_, err := s.CreateChannelGroup(ctx, owner.ID, NewChannelGroup{
		Name: "orphan", ParentGroupID: ChannelGroupID("nope"),
	})
	sentinelIs(t, err, ErrNotFound, "unknown parent group")
}

// TestCreateChannelGroupAuthorizesParent pins the M2 D9 gate: nesting under a
// parent requires authorization against that parent, and an unauthorized parent
// is indistinguishable from an unknown one (both ErrNotFound), so a stranger
// gets no existence oracle. The positive companions prove the three authorized
// paths — owner of the parent, an agent whose owning user owns it, and any
// caller under a SHARED parent — all nest successfully.
func TestCreateChannelGroupAuthorizesParent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	stranger := mustUser(t, s, "stranger")
	agent := mustAgent(t, s, owner.ID, "agent")

	// An OWNER-visibility parent owned by `owner`.
	parent, err := s.CreateChannelGroup(ctx, owner.ID, NewChannelGroup{
		Name: "private", Visibility: VisibilityOwner,
	})
	if err != nil {
		t.Fatalf("CreateChannelGroup(parent): %v", err)
	}

	// Negative: a caller who neither owns nor can see the OWNER parent gets
	// ErrNotFound — NOT success, and NOT a distinct forbidden error. This is the
	// M2 fix: pre-fix a stranger could nest under (and thereby confirm the
	// existence of) any group id.
	_, err = s.CreateChannelGroup(ctx, stranger.ID, NewChannelGroup{
		Name: "intruder", ParentGroupID: parent.ID, Visibility: VisibilityOwner,
	})
	sentinelIs(t, err, ErrNotFound, "unauthorized parent group")

	// The unauthorized error is byte-identical to the unknown-parent error, so
	// the two are indistinguishable to a probing caller (no existence oracle).
	_, unknownErr := s.CreateChannelGroup(ctx, stranger.ID, NewChannelGroup{
		Name: "ghost", ParentGroupID: ChannelGroupID("does-not-exist"), Visibility: VisibilityOwner,
	})
	sentinelIs(t, unknownErr, ErrNotFound, "unknown parent group (companion to unauthorized)")

	// Positive 1: the owner of the parent may nest under it.
	if _, err := s.CreateChannelGroup(ctx, owner.ID, NewChannelGroup{
		Name: "owner-child", ParentGroupID: parent.ID, Visibility: VisibilityOwner,
	}); err != nil {
		t.Fatalf("owner cannot nest under its own parent: %v", err)
	}

	// Positive 2: an agent whose owning user owns the parent may nest under it
	// (an agent acts within its owner's space).
	if _, err := s.CreateChannelGroup(ctx, agent.ID, NewChannelGroup{
		Name: "agent-child", ParentGroupID: parent.ID, Visibility: VisibilityOwner,
	}); err != nil {
		t.Fatalf("owner's agent cannot nest under the owner's parent: %v", err)
	}

	// Positive 3: a SHARED parent is nestable by a non-owner — visibility, not
	// ownership, authorizes it.
	shared, err := s.CreateChannelGroup(ctx, owner.ID, NewChannelGroup{
		Name: "shared-parent", Visibility: VisibilityShared,
	})
	if err != nil {
		t.Fatalf("CreateChannelGroup(shared parent): %v", err)
	}
	if _, err := s.CreateChannelGroup(ctx, stranger.ID, NewChannelGroup{
		Name: "stranger-child", ParentGroupID: shared.ID, Visibility: VisibilityShared,
	}); err != nil {
		t.Fatalf("non-owner cannot nest under a SHARED parent: %v", err)
	}
}

func TestCreateChannelTransitiveOwnerMembership(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	other := mustUser(t, s, "other")
	agent := mustAgent(t, s, owner.ID, "agent")

	// A channel started by `other` with the agent as a member must pull in the
	// agent's owning user, and always include the actor.
	ch, err := s.CreateChannel(ctx, other.ID, NewChannel{
		Name: "collab", Kind: ChannelKindGroupDM,
		MemberAccountIDs: []AccountID{agent.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	for _, want := range []AccountID{other.ID, agent.ID, owner.ID} {
		if !containsAccount(ch.MemberAccountIDs, want) {
			t.Fatalf("members %v missing %s (actor/agent/owner all required)", ch.MemberAccountIDs, want)
		}
	}
}

func TestCreateChannelUnknownMemberInvalid(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "actor")
	_, err := s.CreateChannel(ctx, actor.ID, NewChannel{
		Name: "bad", MemberAccountIDs: []AccountID{"ghost"},
	})
	sentinelIs(t, err, ErrInvalidArgument, "unknown member account")
}

func TestCreateChannelUnknownGroupInvalid(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "actor")

	// D9 group-authz now precedes the FK insert: a non-empty GroupID that names
	// no group the actor may create in — including one that names no group at
	// all — collapses to ErrNotFound in requireGroupCreateAuthz (the
	// not-found/forbidden merge), so a non-owner cannot probe which group ids
	// exist. Pre-authz this reached the insert and the FK surfaced the "unknown
	// group" guard as ErrInvalidArgument; the gate now short-circuits first.
	_, err := s.CreateChannel(ctx, actor.ID, NewChannel{
		Name: "orphan-group", Kind: ChannelKindChannel,
		GroupID: ChannelGroupID("grp-does-not-exist"),
	})
	sentinelIs(t, err, ErrNotFound, "unknown channel group")

	// Positive companion: an EMPTY GroupID is ungrouped. It must succeed and
	// read back with GroupID == "" — proving the NULLIF($3,'') write and the
	// COALESCE(group_id,'') read round-trip NULL as ungrouped. (No existing test
	// asserts the ungrouped read-back; TestListChannelsLattice creates an
	// ungrouped channel but never checks its GroupID.)
	ch, err := s.CreateChannel(ctx, actor.ID, NewChannel{
		Name: "ungrouped", Kind: ChannelKindChannel,
	})
	if err != nil {
		t.Fatalf("CreateChannel(ungrouped): %v", err)
	}
	got, err := s.getChannel(ctx, ch.ID)
	if err != nil {
		t.Fatalf("getChannel(ungrouped): %v", err)
	}
	if got.GroupID != "" {
		t.Fatalf("ungrouped channel read back GroupID = %q, want \"\"", got.GroupID)
	}
}

func TestCreateChannelRejectsDuplicateNameInGroup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")

	groupA, err := s.CreateChannelGroup(ctx, owner.ID, NewChannelGroup{Name: "team-a"})
	if err != nil {
		t.Fatalf("CreateChannelGroup(A): %v", err)
	}
	groupB, err := s.CreateChannelGroup(ctx, owner.ID, NewChannelGroup{Name: "team-b"})
	if err != nil {
		t.Fatalf("CreateChannelGroup(B): %v", err)
	}

	// First "general" in group A succeeds.
	if _, err := s.CreateChannel(ctx, owner.ID, NewChannel{
		Name: "general", GroupID: groupA.ID, Kind: ChannelKindChannel,
	}); err != nil {
		t.Fatalf("CreateChannel(general in A): %v", err)
	}

	// A SECOND "general" in the SAME group collides with the partial unique
	// index channels_group_name_key and is mapped to ErrConflict. Drop that
	// index and this insert would succeed — the assertion is what pins the
	// per-group uniqueness contract.
	_, err = s.CreateChannel(ctx, owner.ID, NewChannel{
		Name: "general", GroupID: groupA.ID, Kind: ChannelKindChannel,
	})
	sentinelIs(t, err, ErrConflict, "duplicate channel name in same group")

	// The SAME name in a DIFFERENT group is fine: uniqueness is (group_id, name),
	// not global on name.
	if _, err := s.CreateChannel(ctx, owner.ID, NewChannel{
		Name: "general", GroupID: groupB.ID, Kind: ChannelKindChannel,
	}); err != nil {
		t.Fatalf("CreateChannel(general in B): %v", err)
	}

	// Ungrouped exemption: the index is partial (WHERE group_id IS NOT NULL), so
	// two ungrouped channels (empty GroupID -> NULL) with the same name BOTH
	// succeed. Make the index non-partial and the second insert here would
	// conflict — this case is what proves the WHERE clause.
	if _, err := s.CreateChannel(ctx, owner.ID, NewChannel{
		Name: "general", Kind: ChannelKindChannel,
	}); err != nil {
		t.Fatalf("CreateChannel(ungrouped general #1): %v", err)
	}
	if _, err := s.CreateChannel(ctx, owner.ID, NewChannel{
		Name: "general", Kind: ChannelKindChannel,
	}); err != nil {
		t.Fatalf("CreateChannel(ungrouped general #2): %v", err)
	}
}

func TestListChannelGroupsLattice(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	ownerA := mustUser(t, s, "owner-a")
	outsider := mustUser(t, s, "outsider")

	// A top-level SHARED group is visible to a non-owner.
	shared, err := s.CreateChannelGroup(ctx, ownerA.ID, NewChannelGroup{Name: "announce", Visibility: VisibilityShared})
	if err != nil {
		t.Fatalf("CreateChannelGroup(shared): %v", err)
	}
	// A top-level OWNER group is not visible to a non-owner.
	private, err := s.CreateChannelGroup(ctx, ownerA.ID, NewChannelGroup{Name: "private", Visibility: VisibilityOwner})
	if err != nil {
		t.Fatalf("CreateChannelGroup(private): %v", err)
	}
	// An OWNER group nested under a SHARED parent: effective visibility is the
	// most-restrictive on the path, so it stays hidden from a non-owner despite
	// the open ancestor. (The mirror case — a SHARED child under an OWNER parent
	// — is unconstructible by design: the ceiling rejects it, covered above.)
	nestedOwner, err := s.CreateChannelGroup(ctx, ownerA.ID, NewChannelGroup{
		Name: "nested-private", ParentGroupID: shared.ID, Visibility: VisibilityOwner,
	})
	if err != nil {
		t.Fatalf("CreateChannelGroup(nested owner): %v", err)
	}

	outsiderView, err := s.ListChannelGroups(ctx, outsider.ID)
	if err != nil {
		t.Fatalf("ListChannelGroups(outsider): %v", err)
	}
	if !containsGroup(outsiderView, shared.ID) {
		t.Fatalf("outsider cannot see SHARED group %s: %v", shared.ID, groupIDs(outsiderView))
	}
	if containsGroup(outsiderView, private.ID) {
		t.Fatalf("outsider can see OWNER group %s; must be hidden", private.ID)
	}
	if containsGroup(outsiderView, nestedOwner.ID) {
		t.Fatalf("outsider can see OWNER group nested under SHARED parent %s; most-restrictive-on-path must hide it", nestedOwner.ID)
	}

	// The owner sees all three of its own groups.
	ownerView, err := s.ListChannelGroups(ctx, ownerA.ID)
	if err != nil {
		t.Fatalf("ListChannelGroups(owner): %v", err)
	}
	for _, want := range []ChannelGroupID{shared.ID, private.ID, nestedOwner.ID} {
		if !containsGroup(ownerView, want) {
			t.Fatalf("owner cannot see own group %s: %v", want, groupIDs(ownerView))
		}
	}
}

func TestListChannelsLattice(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	member := mustUser(t, s, "member")
	outsider := mustUser(t, s, "outsider")

	shared, err := s.CreateChannelGroup(ctx, owner.ID, NewChannelGroup{Name: "shared", Visibility: VisibilityShared})
	if err != nil {
		t.Fatalf("CreateChannelGroup(shared): %v", err)
	}

	// A plain channel in a SHARED group is visible to a non-member.
	groupedShared, err := s.CreateChannel(ctx, owner.ID, NewChannel{
		Name: "public-room", GroupID: shared.ID, Kind: ChannelKindChannel,
	})
	if err != nil {
		t.Fatalf("CreateChannel(grouped shared): %v", err)
	}
	// An ungrouped channel is owner-scoped: visible only via membership.
	ungrouped, err := s.CreateChannel(ctx, owner.ID, NewChannel{
		Name: "ungrouped", Kind: ChannelKindChannel,
	})
	if err != nil {
		t.Fatalf("CreateChannel(ungrouped): %v", err)
	}
	// A DM is visible only to its members regardless of the group lattice.
	dm, err := s.CreateChannel(ctx, owner.ID, NewChannel{
		Name: "dm", Kind: ChannelKindDM, MemberAccountIDs: []AccountID{member.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel(dm): %v", err)
	}

	// Outsider (member of nothing here): sees the grouped SHARED channel only.
	outsiderView, err := s.ListChannels(ctx, outsider.ID)
	if err != nil {
		t.Fatalf("ListChannels(outsider): %v", err)
	}
	if !containsChannel(outsiderView, groupedShared.ID) {
		t.Fatalf("outsider cannot see SHARED-group channel %s: %v", groupedShared.ID, channelIDs(outsiderView))
	}
	if containsChannel(outsiderView, ungrouped.ID) {
		t.Fatalf("outsider can see owner-scoped ungrouped channel %s; membership-only", ungrouped.ID)
	}
	if containsChannel(outsiderView, dm.ID) {
		t.Fatalf("outsider can see a DM %s it is not a member of", dm.ID)
	}

	// Member of the DM sees the DM (and the shared channel), not the ungrouped
	// owner-scoped channel it was never added to.
	memberView, err := s.ListChannels(ctx, member.ID)
	if err != nil {
		t.Fatalf("ListChannels(member): %v", err)
	}
	if !containsChannel(memberView, dm.ID) {
		t.Fatalf("DM member cannot see the DM %s: %v", dm.ID, channelIDs(memberView))
	}
	if containsChannel(memberView, ungrouped.ID) {
		t.Fatalf("DM member can see the owner's ungrouped channel %s it never joined", ungrouped.ID)
	}
}

func TestUpdateChannelMembersMutations(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	newcomer := mustUser(t, s, "newcomer")

	ch, err := s.CreateChannel(ctx, owner.ID, NewChannel{Name: "room", Kind: ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	// Add a member, subscribed.
	updated, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: newcomer.ID, Subscribed: true},
	})
	if err != nil {
		t.Fatalf("UpdateChannelMembers(add): %v", err)
	}
	if !containsAccount(updated.MemberAccountIDs, newcomer.ID) {
		t.Fatalf("after add, members %v missing %s", updated.MemberAccountIDs, newcomer.ID)
	}
	if !memberSubscribed(t, s, ch.ID, newcomer.ID) {
		t.Fatal("added member should be subscribed")
	}

	// Flip subscribed off.
	if _, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: newcomer.ID, Subscribed: false},
	}); err != nil {
		t.Fatalf("UpdateChannelMembers(unsubscribe): %v", err)
	}
	if memberSubscribed(t, s, ch.ID, newcomer.ID) {
		t.Fatal("subscribed flag did not flip off")
	}

	// Remove the member.
	afterRemove, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: newcomer.ID, Remove: true},
	})
	if err != nil {
		t.Fatalf("UpdateChannelMembers(remove): %v", err)
	}
	if containsAccount(afterRemove.MemberAccountIDs, newcomer.ID) {
		t.Fatalf("member %s still present after remove: %v", newcomer.ID, afterRemove.MemberAccountIDs)
	}
}

func TestUpdateChannelMembersAddingAgentPullsOwner(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	actor := mustUser(t, s, "actor")
	agent := mustAgent(t, s, owner.ID, "agent")

	// A channel `actor` owns, without the agent's owner in it yet.
	ch, err := s.CreateChannel(ctx, actor.ID, NewChannel{Name: "room", Kind: ChannelKindGroupDM})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if containsAccount(ch.MemberAccountIDs, owner.ID) {
		t.Fatalf("precondition: owner %s already a member: %v", owner.ID, ch.MemberAccountIDs)
	}

	// Adding the agent must also pull its owner into the channel.
	updated, _, err := s.UpdateChannelMembers(ctx, actor.ID, ch.ID, []MemberUpdate{
		{AccountID: agent.ID},
	})
	if err != nil {
		t.Fatalf("UpdateChannelMembers(add agent): %v", err)
	}
	if !containsAccount(updated.MemberAccountIDs, agent.ID) {
		t.Fatalf("agent %s not added: %v", agent.ID, updated.MemberAccountIDs)
	}
	if !containsAccount(updated.MemberAccountIDs, owner.ID) {
		t.Fatalf("adding agent did not pull in owner %s: %v", owner.ID, updated.MemberAccountIDs)
	}
}

func TestUpdateChannelMembersPreservesOwnerSubscription(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	// A channel the owner is a member of, with the owner explicitly subscribed.
	ch, err := s.CreateChannel(ctx, owner.ID, NewChannel{Name: "room", Kind: ChannelKindGroupDM})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: owner.ID, Subscribed: true},
	}); err != nil {
		t.Fatalf("UpdateChannelMembers(subscribe owner): %v", err)
	}
	if !memberSubscribed(t, s, ch.ID, owner.ID) {
		t.Fatal("precondition: owner should be subscribed before the agent add")
	}

	// F1: adding the agent pulls in its owner transitively (owner is NOT named
	// in the update — it arrives only via expandOwnerMembership as an i>0 row).
	// That pulled-in owner row must be additive-only (ON CONFLICT DO NOTHING);
	// the bug used DO UPDATE SET subscribed = EXCLUDED.subscribed, which
	// clobbered the already-subscribed owner to FALSE on the agent join.
	updated, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: agent.ID},
	})
	if err != nil {
		t.Fatalf("UpdateChannelMembers(add agent): %v", err)
	}
	if !containsAccount(updated.MemberAccountIDs, agent.ID) {
		t.Fatalf("agent %s not added: %v", agent.ID, updated.MemberAccountIDs)
	}
	if !containsAccount(updated.MemberAccountIDs, owner.ID) {
		t.Fatalf("owner %s not pulled in transitively: %v", owner.ID, updated.MemberAccountIDs)
	}
	if !memberSubscribed(t, s, ch.ID, owner.ID) {
		t.Fatal("adding the agent downgraded the owner's subscription: subscribed flipped TRUE->FALSE via the pulled-in owner row")
	}
}

func TestUpdateChannelMembersUnknownChannelNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "actor")
	_, _, err := s.UpdateChannelMembers(ctx, actor.ID, ChannelID("ghost"), []MemberUpdate{
		{AccountID: actor.ID},
	})
	sentinelIs(t, err, ErrNotFound, "unknown channel")
}

func TestOpenAgentWorkspaceIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	first, err := s.OpenAgentWorkspace(ctx, owner.ID, agent.ID)
	if err != nil {
		t.Fatalf("OpenAgentWorkspace(first): %v", err)
	}
	if first.ID == "" {
		t.Fatal("first open returned empty workspace id")
	}
	second, err := s.OpenAgentWorkspace(ctx, owner.ID, agent.ID)
	if err != nil {
		t.Fatalf("OpenAgentWorkspace(second): %v", err)
	}
	// Idempotent: the second open returns the same workspace, not a new one.
	if second.ID != first.ID {
		t.Fatalf("second open minted a new workspace %q, want the existing %q", second.ID, first.ID)
	}
	if second.AgentAccountID != agent.ID {
		t.Fatalf("workspace agent = %q, want %q", second.AgentAccountID, agent.ID)
	}
}

func TestOpenAgentWorkspaceUnknownAgentNotFound(t *testing.T) {
	s := newTestStore(t)
	// An unknown agent id resolves no home channel, so IsAgentWorkspaceVisible
	// is false and the open collapses to ErrNotFound — the not-found/forbidden
	// merge (a caller cannot tell an unknown agent from one whose workspace it
	// may not see). Pre-fix this took an ungated insert path and mapped the FK
	// violation to ErrInvalidArgument; the membership gate now precedes the
	// insert, so an unknown agent never reaches it.
	_, err := s.OpenAgentWorkspace(context.Background(), AccountID("actor"), AccountID("ghost"))
	sentinelIs(t, err, ErrNotFound, "unknown agent")
}

func TestUpdateChannelMembersRejectsOwnerRemovalWithAgentPresent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	// A channel the owner starts, then adds its agent to — the add pulls the
	// owner in transitively (design.md:231-234), so both the owner and the agent
	// it owns are members.
	ch, err := s.CreateChannel(ctx, owner.ID, NewChannel{Name: "room", Kind: ChannelKindGroupDM})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: agent.ID},
	}); err != nil {
		t.Fatalf("UpdateChannelMembers(add agent): %v", err)
	}

	// Removing the OWNER while an agent it owns is still a member must be
	// rejected — the removal half of transitive owner-membership. Orphaning the
	// owner from a channel its agent still reads would break the invariant that
	// a user can always read anything its agent is party to.
	_, _, err = s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: owner.ID, Remove: true},
	})
	sentinelIs(t, err, ErrInvalidArgument, "remove owner while its agent remains")

	// Positive companion, proving the invariant is about the DEPENDENT agent,
	// not the owner unconditionally: remove the AGENT first (nothing depends on
	// it, so this succeeds)...
	afterAgent, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: agent.ID, Remove: true},
	})
	if err != nil {
		t.Fatalf("UpdateChannelMembers(remove agent): %v", err)
	}
	if containsAccount(afterAgent.MemberAccountIDs, agent.ID) {
		t.Fatalf("agent %s still present after remove: %v", agent.ID, afterAgent.MemberAccountIDs)
	}

	// ...then removing the owner now succeeds, since no agent it owns remains in
	// the channel to depend on its membership.
	afterOwner, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: owner.ID, Remove: true},
	})
	if err != nil {
		t.Fatalf("UpdateChannelMembers(remove owner after agent gone): %v", err)
	}
	if containsAccount(afterOwner.MemberAccountIDs, owner.ID) {
		t.Fatalf("owner %s still present after remove: %v", owner.ID, afterOwner.MemberAccountIDs)
	}
}

// TestUpdateChannelMembersRemovalScansAllOwnedAgents proves the owner-removal
// invariant's EXISTS scans EVERY agent the removed user owns, not just the
// first-added one. The single-agent sibling above cannot distinguish "scans all
// owned agents" from "scans the first owned agent"; this one can.
func TestUpdateChannelMembersRemovalScansAllOwnedAgents(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent1 := mustAgent(t, s, owner.ID, "agent1")
	agent2 := mustAgent(t, s, owner.ID, "agent2")

	// A second owner with its own agent, joined to the SAME channel. Its
	// presence proves the invariant's owner_user_id=$removed scoping does not
	// over-match across owners: removing `owner` must never be gated by an agent
	// that `other` owns.
	other := mustUser(t, s, "other")
	otherAgent := mustAgent(t, s, other.ID, "otheragent")

	ch, err := s.CreateChannel(ctx, owner.ID, NewChannel{Name: "room", Kind: ChannelKindGroupDM})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	// Add both owned agents plus the other owner's agent. Each add pulls its
	// owner in transitively, so owner, other, and all three agents are members.
	added, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: agent1.ID},
		{AccountID: agent2.ID},
		{AccountID: otherAgent.ID},
	})
	if err != nil {
		t.Fatalf("UpdateChannelMembers(add agents): %v", err)
	}
	for _, want := range []AccountID{agent1.ID, agent2.ID, otherAgent.ID, owner.ID, other.ID} {
		if !containsAccount(added.MemberAccountIDs, want) {
			t.Fatalf("account %s not a member after add: %v", want, added.MemberAccountIDs)
		}
	}

	// (a) Both owned agents remain: removing the owner is rejected.
	_, _, err = s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: owner.ID, Remove: true},
	})
	sentinelIs(t, err, ErrInvalidArgument, "remove owner while both its agents remain")

	// (b) Remove the FIRST-added owned agent. If the EXISTS scanned only the
	// first owned agent, the gate would now be gone and owner-removal would
	// wrongly succeed. agent2 still remains, so removal must STILL be rejected.
	afterAgent1, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: agent1.ID, Remove: true},
	})
	if err != nil {
		t.Fatalf("UpdateChannelMembers(remove agent1): %v", err)
	}
	if containsAccount(afterAgent1.MemberAccountIDs, agent1.ID) {
		t.Fatalf("agent1 %s still present after remove: %v", agent1.ID, afterAgent1.MemberAccountIDs)
	}
	if !containsAccount(afterAgent1.MemberAccountIDs, agent2.ID) {
		t.Fatalf("agent2 %s unexpectedly gone: %v", agent2.ID, afterAgent1.MemberAccountIDs)
	}
	_, _, err = s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: owner.ID, Remove: true},
	})
	sentinelIs(t, err, ErrInvalidArgument, "remove owner while its second agent remains")

	// (c) Remove the second owned agent. No agent the owner owns remains, so
	// owner-removal now succeeds — even though the OTHER owner's agent is still
	// a channel member, confirming the gate is scoped to owner_user_id=$removed.
	afterAgent2, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: agent2.ID, Remove: true},
	})
	if err != nil {
		t.Fatalf("UpdateChannelMembers(remove agent2): %v", err)
	}
	if containsAccount(afterAgent2.MemberAccountIDs, agent2.ID) {
		t.Fatalf("agent2 %s still present after remove: %v", agent2.ID, afterAgent2.MemberAccountIDs)
	}
	if !containsAccount(afterAgent2.MemberAccountIDs, otherAgent.ID) {
		t.Fatalf("other owner's agent %s unexpectedly gone: %v", otherAgent.ID, afterAgent2.MemberAccountIDs)
	}
	afterOwner, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: owner.ID, Remove: true},
	})
	if err != nil {
		t.Fatalf("UpdateChannelMembers(remove owner after all its agents gone): %v", err)
	}
	if containsAccount(afterOwner.MemberAccountIDs, owner.ID) {
		t.Fatalf("owner %s still present after remove: %v", owner.ID, afterOwner.MemberAccountIDs)
	}
}

func TestSubscriberAccountIDsHomeChannelSeeding(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	// The agent's minted home channel seeds the agent always-subscribed and the
	// owner unsubscribed (accounts.go home-channel seeding). scanChannels
	// populates SubscriberAccountIDs from the per-member subscribed flag, so the
	// subscriber set is exactly [agent] while the member set is [owner, agent].
	home, err := s.getChannel(ctx, agent.Agent.HomeChannelID)
	if err != nil {
		t.Fatalf("getChannel(home): %v", err)
	}
	if !containsAccount(home.MemberAccountIDs, owner.ID) || !containsAccount(home.MemberAccountIDs, agent.ID) {
		t.Fatalf("home members %v, want both owner %s and agent %s", home.MemberAccountIDs, owner.ID, agent.ID)
	}
	if !containsAccount(home.SubscriberAccountIDs, agent.ID) {
		t.Fatalf("home subscribers %v missing always-subscribed agent %s", home.SubscriberAccountIDs, agent.ID)
	}
	if containsAccount(home.SubscriberAccountIDs, owner.ID) {
		t.Fatalf("home subscribers %v include the owner, want it unsubscribed by default", home.SubscriberAccountIDs)
	}

	// The same subscriber set surfaces through the list read path, not only the
	// id-addressed get.
	listed, err := s.ListChannels(ctx, owner.ID)
	if err != nil {
		t.Fatalf("ListChannels(owner): %v", err)
	}
	var found *Channel
	for i := range listed {
		if listed[i].ID == home.ID {
			found = &listed[i]
		}
	}
	if found == nil {
		t.Fatalf("home channel %s not in owner's ListChannels", home.ID)
	}
	if !containsAccount(found.SubscriberAccountIDs, agent.ID) || containsAccount(found.SubscriberAccountIDs, owner.ID) {
		t.Fatalf("listed subscribers %v, want exactly [agent %s]", found.SubscriberAccountIDs, agent.ID)
	}
}

func TestSubscriberAccountIDsToggle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	newcomer := mustUser(t, s, "newcomer")

	ch, err := s.CreateChannel(ctx, owner.ID, NewChannel{Name: "room", Kind: ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	// Add a member subscribed: it appears in both member and subscriber sets.
	subbed, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: newcomer.ID, Subscribed: true},
	})
	if err != nil {
		t.Fatalf("UpdateChannelMembers(add subscribed): %v", err)
	}
	if !containsAccount(subbed.MemberAccountIDs, newcomer.ID) {
		t.Fatalf("after add, members %v missing %s", subbed.MemberAccountIDs, newcomer.ID)
	}
	if !containsAccount(subbed.SubscriberAccountIDs, newcomer.ID) {
		t.Fatalf("after subscribe, subscribers %v missing %s", subbed.SubscriberAccountIDs, newcomer.ID)
	}

	// Unsubscribe: the member stays a member (read access) but drops out of the
	// subscriber set — the join/subscribe tier split.
	unsubbed, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: newcomer.ID, Subscribed: false},
	})
	if err != nil {
		t.Fatalf("UpdateChannelMembers(unsubscribe): %v", err)
	}
	if !containsAccount(unsubbed.MemberAccountIDs, newcomer.ID) {
		t.Fatalf("after unsubscribe, member %s dropped from members %v", newcomer.ID, unsubbed.MemberAccountIDs)
	}
	if containsAccount(unsubbed.SubscriberAccountIDs, newcomer.ID) {
		t.Fatalf("after unsubscribe, subscribers %v still include %s", unsubbed.SubscriberAccountIDs, newcomer.ID)
	}
}

// groupIDs / channelIDs / containsChannel are small readability helpers for the
// lattice assertions above.
func groupIDs(groups []ChannelGroup) []ChannelGroupID {
	ids := make([]ChannelGroupID, len(groups))
	for i, g := range groups {
		ids[i] = g.ID
	}
	return ids
}

func channelIDs(channels []Channel) []ChannelID {
	ids := make([]ChannelID, len(channels))
	for i, c := range channels {
		ids[i] = c.ID
	}
	return ids
}

func containsChannel(channels []Channel, want ChannelID) bool {
	for _, c := range channels {
		if c.ID == want {
			return true
		}
	}
	return false
}
