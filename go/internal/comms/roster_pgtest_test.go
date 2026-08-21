//go:build pgtest

package comms

// GetRoster roster-read contracts (SEA-1721 T2), driven against a real Postgres
// store + real bus (no mocks — newHandler), each defending one clause of the
// three-source join (design.md T2:426-452): the durable tree, the live presence
// enum (an in-memory fake presence source), and the durable activity string
// (agent_activity). Scoping, D9 visibility clip, OFFLINE default, activity
// round-trip through the DURABLE store, and durability across a simulated restart
// are pinned separately. context.Background() is the test root (test-root ctx
// exemption).

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// fakePresenceSource is an in-memory PresenceSource: a per-agent presence enum a
// test seeds. An agent absent from the map is OMITTED (the handler defaults it to
// OFFLINE), modeling the hub's "absent-from-map → OFFLINE" posture.
type fakePresenceSource struct {
	presence map[store.AccountID]compassv1.AgentPresence
}

func (f fakePresenceSource) PresenceFor(accountIDs []store.AccountID) map[store.AccountID]compassv1.AgentPresence {
	out := make(map[store.AccountID]compassv1.AgentPresence, len(accountIDs))
	for _, id := range accountIDs {
		if p, ok := f.presence[id]; ok {
			out[id] = p
		}
	}
	return out
}

// rosterByID indexes a roster response by agent account id for assertions.
func rosterByID(entries []*compassv1.RosterEntry) map[string]*compassv1.RosterEntry {
	out := make(map[string]*compassv1.RosterEntry, len(entries))
	for _, e := range entries {
		out[e.GetAgentAccountId()] = e
	}
	return out
}

// mustChildAgent creates an agent owned by owner with parent as its tree parent.
func mustChildAgent(t *testing.T, s *store.Store, owner store.AccountID, handle string, parent store.AccountID) store.Account {
	t.Helper()
	acc, err := s.CreateAgent(context.Background(), owner, store.NewAgent{
		Handle: handle, DisplayName: handle, ParentAgentID: parent,
	})
	if err != nil {
		t.Fatalf("CreateAgent(%q under %q): %v", handle, parent, err)
	}
	return acc
}

// TestGetRosterNeighborhoodScope: the NEIGHBORHOOD scope returns the vantage
// agent's parent, siblings (incl. itself), and children — and NOT an unrelated
// agent in a different branch. The owner (a user, always visible) is the caller.
func TestGetRosterNeighborhoodScope(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")

	root := mustAgent(t, st, owner.ID, "treetop")
	mid := mustChildAgent(t, st, owner.ID, "mid", root.ID)    // vantage
	sib := mustChildAgent(t, st, owner.ID, "sib", root.ID)    // sibling of mid
	child := mustChildAgent(t, st, owner.ID, "child", mid.ID) // child of mid
	niece := mustChildAgent(t, st, owner.ID, "niece", sib.ID) // NOT in mid's neighborhood

	resp, err := svc.GetRoster(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.GetRosterRequest{
		Scope:          compassv1.RosterScope_ROSTER_SCOPE_NEIGHBORHOOD,
		AgentAccountId: string(mid.ID),
	}))
	if err != nil {
		t.Fatalf("GetRoster(neighborhood): %v", err)
	}
	got := rosterByID(resp.Msg.GetEntries())
	for _, want := range []store.Account{root, mid, sib, child} {
		if _, ok := got[string(want.ID)]; !ok {
			t.Errorf("neighborhood missing %q (%s)", want.Handle, want.ID)
		}
	}
	if _, ok := got[string(niece.ID)]; ok {
		t.Errorf("neighborhood wrongly includes niece %q — it is under a sibling, not mid", niece.ID)
	}
}

// TestGetRosterSubtreeScope: SUBTREE returns the vantage plus every transitive
// descendant, and excludes the parent and siblings.
func TestGetRosterSubtreeScope(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")

	root := mustAgent(t, st, owner.ID, "treetop")
	mid := mustChildAgent(t, st, owner.ID, "mid", root.ID) // vantage
	sib := mustChildAgent(t, st, owner.ID, "sib", root.ID)
	child := mustChildAgent(t, st, owner.ID, "child", mid.ID)
	grand := mustChildAgent(t, st, owner.ID, "grand", child.ID)

	resp, err := svc.GetRoster(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.GetRosterRequest{
		Scope:          compassv1.RosterScope_ROSTER_SCOPE_SUBTREE,
		AgentAccountId: string(mid.ID),
	}))
	if err != nil {
		t.Fatalf("GetRoster(subtree): %v", err)
	}
	got := rosterByID(resp.Msg.GetEntries())
	for _, want := range []store.Account{mid, child, grand} {
		if _, ok := got[string(want.ID)]; !ok {
			t.Errorf("subtree missing %q (%s)", want.Handle, want.ID)
		}
	}
	if _, ok := got[string(root.ID)]; ok {
		t.Errorf("subtree wrongly includes parent root %q", root.ID)
	}
	if _, ok := got[string(sib.ID)]; ok {
		t.Errorf("subtree wrongly includes sibling sib %q", sib.ID)
	}
}

// TestGetRosterOwnerScope: OWNER returns every agent owned by the vantage's
// owner, tree position irrelevant, and NOT another owner's agent.
func TestGetRosterOwnerScope(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	other := mustUser(t, st, "other")

	a1 := mustAgent(t, st, owner.ID, "a1")
	a2 := mustChildAgent(t, st, owner.ID, "a2", a1.ID)
	foreign := mustAgent(t, st, other.ID, "foreign")

	resp, err := svc.GetRoster(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.GetRosterRequest{
		Scope:          compassv1.RosterScope_ROSTER_SCOPE_OWNER,
		AgentAccountId: string(a1.ID),
	}))
	if err != nil {
		t.Fatalf("GetRoster(owner): %v", err)
	}
	got := rosterByID(resp.Msg.GetEntries())
	for _, want := range []store.Account{a1, a2} {
		if _, ok := got[string(want.ID)]; !ok {
			t.Errorf("owner scope missing %q (%s)", want.Handle, want.ID)
		}
	}
	if _, ok := got[string(foreign.ID)]; ok {
		t.Errorf("owner scope wrongly includes another owner's agent %q", foreign.ID)
	}
}

// TestGetRosterClipsNonVisibleAgent (D9): an agent structurally in the vantage's
// OWNER tree but NOT visible to the CALLER never appears. The caller is a
// different owner's agent that shares no channel with the vantage's agents, so
// the account-visibility clip drops them even though they are in the requested
// tree. This is the security-critical clause: the raw tree read is unscoped, and
// the handler must intersect with the caller's visible set.
func TestGetRosterClipsNonVisibleAgent(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	intruderOwner := mustUser(t, st, "intruder-owner")

	victim := mustAgent(t, st, owner.ID, "victim")
	// The caller is an agent owned by a DIFFERENT user, sharing no channel with
	// victim: victim is not visible to it (not owned, no shared channel).
	intruder := mustAgent(t, st, intruderOwner.ID, "intruder")

	resp, err := svc.GetRoster(WithActor(ctx, intruder.ID), connect.NewRequest(&compassv1.GetRosterRequest{
		Scope:          compassv1.RosterScope_ROSTER_SCOPE_OWNER,
		AgentAccountId: string(victim.ID), // vantage in victim's tree
	}))
	if err != nil {
		t.Fatalf("GetRoster(clip): %v", err)
	}
	got := rosterByID(resp.Msg.GetEntries())
	if _, ok := got[string(victim.ID)]; ok {
		t.Fatalf("D9 breach: caller %q saw non-visible agent %q in the roster", intruder.ID, victim.ID)
	}
}

// TestGetRosterOfflineDefaultAndPresenceJoin: an agent present in the presence
// source reports its enum; an agent absent from it defaults to OFFLINE.
func TestGetRosterOfflineDefaultAndPresenceJoin(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")

	working := mustAgent(t, st, owner.ID, "working")
	offline := mustAgent(t, st, owner.ID, "offline")

	svc.SetPresenceSource(fakePresenceSource{presence: map[store.AccountID]compassv1.AgentPresence{
		working.ID: compassv1.AgentPresence_AGENT_PRESENCE_WORKING,
		// offline deliberately absent from the map.
	}})

	resp, err := svc.GetRoster(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.GetRosterRequest{
		Scope:          compassv1.RosterScope_ROSTER_SCOPE_OWNER,
		AgentAccountId: string(working.ID),
	}))
	if err != nil {
		t.Fatalf("GetRoster(presence): %v", err)
	}
	got := rosterByID(resp.Msg.GetEntries())
	if p := got[string(working.ID)].GetPresence(); p != compassv1.AgentPresence_AGENT_PRESENCE_WORKING {
		t.Errorf("working agent presence = %v, want WORKING", p)
	}
	if p := got[string(offline.ID)].GetPresence(); p != compassv1.AgentPresence_AGENT_PRESENCE_OFFLINE {
		t.Errorf("absent-from-map agent presence = %v, want OFFLINE default", p)
	}
}

// TestGetRosterActivityRoundTripsThroughDurableStore: SetActivity → GetRoster
// surfaces the durable activity string + timestamp; an agent with no row reports
// empty activity.
func TestGetRosterActivityRoundTripsThroughDurableStore(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")

	busy := mustAgent(t, st, owner.ID, "busy")
	quiet := mustAgent(t, st, owner.ID, "quiet")

	const atMs = int64(1_700_000_000_123)
	if err := st.SetActivity(ctx, busy.ID, "reviewing PR #1090", atMs); err != nil {
		t.Fatalf("SetActivity: %v", err)
	}

	resp, err := svc.GetRoster(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.GetRosterRequest{
		Scope:          compassv1.RosterScope_ROSTER_SCOPE_OWNER,
		AgentAccountId: string(busy.ID),
	}))
	if err != nil {
		t.Fatalf("GetRoster(activity): %v", err)
	}
	got := rosterByID(resp.Msg.GetEntries())
	if a := got[string(busy.ID)].GetActivity(); a != "reviewing PR #1090" {
		t.Errorf("busy activity = %q, want the durable string", a)
	}
	if ts := got[string(busy.ID)].GetActivityAtUnixMs(); ts != atMs {
		t.Errorf("busy activity_at_unix_ms = %d, want %d", ts, atMs)
	}
	if a := got[string(quiet.ID)].GetActivity(); a != "" {
		t.Errorf("quiet (no row) activity = %q, want empty", a)
	}
}

// TestGetRosterActivitySurvivesSimulatedRestart: the activity string is durable,
// so a Server restart — modeled by a FRESH handler over the SAME store, with a
// fresh (empty) presence source — still returns the activity from the table even
// though the in-memory presence rebuilds to OFFLINE. This is the durability
// contract: the roster reads activity straight from Postgres, not from the hub.
func TestGetRosterActivitySurvivesSimulatedRestart(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")

	const atMs = int64(1_700_000_555_000)
	if err := st.SetActivity(ctx, agent.ID, "long-running compile", atMs); err != nil {
		t.Fatalf("SetActivity: %v", err)
	}
	// Give the pre-restart handler a live presence so we can prove the restart
	// loses presence but keeps activity.
	svc.SetPresenceSource(fakePresenceSource{presence: map[store.AccountID]compassv1.AgentPresence{
		agent.ID: compassv1.AgentPresence_AGENT_PRESENCE_WORKING,
	}})

	// Simulate the restart: a brand-new handler over the SAME store, its in-memory
	// presence projection empty (nothing re-enrolled yet).
	fresh := NewComms(st, newBus(t), owner.ID)

	resp, err := fresh.GetRoster(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.GetRosterRequest{
		Scope:          compassv1.RosterScope_ROSTER_SCOPE_OWNER,
		AgentAccountId: string(agent.ID),
	}))
	if err != nil {
		t.Fatalf("GetRoster(after restart): %v", err)
	}
	got := rosterByID(resp.Msg.GetEntries())
	entry := got[string(agent.ID)]
	if entry == nil {
		t.Fatalf("agent missing from roster after restart")
	}
	if a := entry.GetActivity(); a != "long-running compile" {
		t.Errorf("post-restart activity = %q, want the durable string (reloaded from the table)", a)
	}
	if p := entry.GetPresence(); p != compassv1.AgentPresence_AGENT_PRESENCE_OFFLINE {
		t.Errorf("post-restart presence = %v, want OFFLINE (in-memory map rebuilt empty)", p)
	}
}

// TestSetStatusAsAccountTruncatesOverCap: an over-cap activity is truncated
// server-side to activityCap runes, and the TRUNCATED value is what lands in the
// durable table (proven by reading it back through GetRoster).
func TestSetStatusAsAccountTruncatesOverCap(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")

	over := make([]rune, activityCap+50)
	for i := range over {
		over[i] = 'x'
	}
	returned, err := svc.SetStatusAsAccount(ctx, agent.ID, string(over))
	if err != nil {
		t.Fatalf("SetStatusAsAccount: %v", err)
	}
	if len([]rune(returned)) != activityCap {
		t.Errorf("returned activity len = %d runes, want cap %d", len([]rune(returned)), activityCap)
	}

	resp, err := svc.GetRoster(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.GetRosterRequest{
		Scope:          compassv1.RosterScope_ROSTER_SCOPE_OWNER,
		AgentAccountId: string(agent.ID),
	}))
	if err != nil {
		t.Fatalf("GetRoster(after set_status): %v", err)
	}
	got := rosterByID(resp.Msg.GetEntries())
	landed := got[string(agent.ID)].GetActivity()
	if len([]rune(landed)) != activityCap {
		t.Errorf("activity landed in table = %d runes, want the truncated cap %d", len([]rune(landed)), activityCap)
	}
	if landed != returned {
		t.Errorf("table value %q != returned truncated value %q", landed, returned)
	}
}

// TestRosterAsAccountEmptyAccountFailsClosed: an empty account is a hard
// CodeInvalidArgument (errNoActor) and enumerates NOTHING — the fail-closed
// guard that refuses to attribute an unresolved caller to the bootstrap-admin
// fallback. Without the guard the call falls through to c.roster with an empty
// caller: the SUBTREE read runs and the D9 clip's ListAccounts(ctx, "") matches
// every account, so the call returns a (non-nil) response with a nil error
// instead of failing closed. Asserting the call errors AND returns a nil
// response proves the guard short-circuited before any tree read or enumeration.
//
// Mutation: remove `if account == ""` in RosterAsAccount → the call falls
// through to a non-nil response with a nil error; this test fails.
func TestRosterAsAccountEmptyAccountFailsClosed(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	// Seed accounts that a missing guard's enumeration would reach.
	owner := mustUser(t, st, "owner")
	mustAgent(t, st, owner.ID, "agent")

	resp, err := svc.RosterAsAccount(ctx, "", &compassv1.GetRosterRequest{
		Scope: compassv1.RosterScope_ROSTER_SCOPE_SUBTREE,
	})
	connectCodeIs(t, err, connect.CodeInvalidArgument, "RosterAsAccount(empty account)")
	if resp != nil {
		t.Fatalf("RosterAsAccount(empty account) returned a non-nil response (%d entries), want nil (no enumeration)", len(resp.GetEntries()))
	}
}

// TestSetStatusAsAccountEmptyAccountFailsClosedNoWrite: an empty account is a
// hard CodeInvalidArgument (errNoActor) and writes NOTHING — without the guard
// the call falls through to Store.SetActivity keyed on the empty account id,
// planting an orphan agent_activity row. Asserting the error AND that
// ActivityFor([""]) reads back no row proves the guard short-circuited before
// the write.
//
// Mutation: remove `if account == ""` in SetStatusAsAccount → the call commits a
// row for the empty id and returns nil error; this test fails twice.
func TestSetStatusAsAccountEmptyAccountFailsClosedNoWrite(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	returned, err := svc.SetStatusAsAccount(ctx, "", "some status")
	connectCodeIs(t, err, connect.CodeInvalidArgument, "SetStatusAsAccount(empty account)")
	if returned != "" {
		t.Fatalf("SetStatusAsAccount(empty account) returned %q, want empty (no write)", returned)
	}

	// No agent_activity row landed for the empty id.
	got, err := st.ActivityFor(ctx, []store.AccountID{""})
	if err != nil {
		t.Fatalf("ActivityFor: %v", err)
	}
	if _, ok := got[""]; ok {
		t.Fatalf("agent_activity holds a row for the empty account id, want none (no write)")
	}
}
