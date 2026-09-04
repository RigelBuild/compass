//go:build pgtest

package store

// T3 (design record compass-system-sender-first-turn, section T3): the reserved
// system account @compass (seeded by EnsureSystemAccount, handle
// SystemAccountHandle) is STRUCTURALLY excluded from every delivery, agent-roster,
// and agent-by-handle read, and is directory-visible only through the ordinary
// shared-channel EXISTS clause — never through a system-specific disjunct. These
// guarantees already hold by construction: a system account has neither an
// agent_accounts row (so the INNER JOIN agent_accounts in the delivery/roster
// reads and AgentByHandle's IsAgent gate drop it) nor a user_accounts row (so it
// falls outside ListAccounts' broad user disjunct). This file pins those
// structural facts so a FUTURE subtype change that gave @compass an agent or user
// row would redden CI instead of silently leaking the platform sender into a
// deliver set, a roster, a handle resolve, or an unrelated viewer's directory.

import (
	"context"
	"testing"
)

// insertSystemMember makes the seeded system account a direct channel_members
// row (subscribed=true). The guarded membership APIs won't add a system account
// cleanly, so the row is inserted directly through the pool, mirroring the
// pre-guard-database simulation in TestEnsureSystemAccountWrongShapeSquatterConflicts.
// This is the strongest form of the guard: @compass is a fully-fledged, subscribed
// member of the channel, so its absence from the reads below can only be the
// structural INNER JOIN / user-disjunct exclusion, not a missing membership row.
func insertSystemMember(t *testing.T, s *Store, ch ChannelID, sys AccountID) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		"INSERT INTO channel_members (channel_id, account_id, subscribed, tenant_id) VALUES ($1, $2, $3, $4)",
		string(ch), string(sys), true, string(s.resolveTenant(context.Background())),
	); err != nil {
		t.Fatalf("insert system channel_members row (%s,%s): %v", ch, sys, err)
	}
}

// TestSystemAccountExcludedFromDeliverSet asserts SubscribedAgents never returns
// the system account, even when @compass is a subscribed member of the channel.
// The proof is contrastive: a real agent member of the same channel IS returned,
// so the query works, yet the system id is absent — its exclusion is the INNER
// JOIN agent_accounts (a system account has no agent_accounts row). Reddens if a
// future change gave @compass an agent_accounts row: it would then appear in the
// deliver set and the exact-id absence assertion would fail.
func TestSystemAccountExcludedFromDeliverSet(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	owner := mustUser(t, s, "owner")
	author := mustAgent(t, s, owner.ID, "author")
	recip := mustAgent(t, s, owner.ID, "recip")
	// Owner-owned channel with both agents as members; the deliver set is
	// non-empty and provably correct once recip is subscribed.
	ch := mustNamedChannelWith(t, s, owner.ID, "shared", author.ID, recip.ID)
	subscribeAgent(t, s, owner.ID, ch, recip.ID)

	sys, err := s.EnsureSystemAccount(ctx)
	if err != nil {
		t.Fatalf("EnsureSystemAccount: %v", err)
	}
	insertSystemMember(t, s, ch, sys.ID)

	agents, err := s.SubscribedAgents(ctx, ch, author.ID)
	if err != nil {
		t.Fatalf("SubscribedAgents: %v", err)
	}
	got := accountIDSet(accountsFromIDs(agents))
	if !got[recip.ID] {
		t.Fatalf("deliver set %v missing the real agent member %s; the query must resolve agents", agents, recip.ID)
	}
	if got[sys.ID] {
		t.Fatalf("deliver set %v leaked the system account %s; @compass must never be a delivery recipient", agents, sys.ID)
	}
}

// TestSystemAccountExcludedFromAgentRoster asserts ChannelAgentMembers (the
// mention->steer routing set, membership not subscription) never returns the
// system account, even when @compass is a member. Same contrastive proof: the
// real agent member is present, the system id is absent via the INNER JOIN
// agent_accounts. Reddens if @compass gained an agent_accounts row.
func TestSystemAccountExcludedFromAgentRoster(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	owner := mustUser(t, s, "owner")
	author := mustAgent(t, s, owner.ID, "author")
	member := mustAgent(t, s, owner.ID, "member")
	// membership, not subscription: member is NOT subscribed but must still be
	// in the roster.
	ch := mustNamedChannelWith(t, s, owner.ID, "shared", author.ID, member.ID)

	sys, err := s.EnsureSystemAccount(ctx)
	if err != nil {
		t.Fatalf("EnsureSystemAccount: %v", err)
	}
	insertSystemMember(t, s, ch, sys.ID)

	agents, err := s.ChannelAgentMembers(ctx, ch, author.ID)
	if err != nil {
		t.Fatalf("ChannelAgentMembers: %v", err)
	}
	got := accountIDSet(accountsFromIDs(agents))
	if !got[member.ID] {
		t.Fatalf("roster %v missing the real agent member %s; the query must resolve agent members", agents, member.ID)
	}
	if got[sys.ID] {
		t.Fatalf("roster %v leaked the system account %s; @compass must never appear in the agent roster", agents, sys.ID)
	}
}

// TestSystemAccountByHandleIsNotFound asserts AgentByHandle("compass") fails
// closed as ErrNotFound after @compass is seeded: it exists as an account but has
// no agent_accounts row, so AgentByHandle's IsAgent gate rejects it exactly as it
// would an unknown handle. Reddens if @compass gained an agent_accounts row: the
// lookup would then resolve the system account as an agent.
func TestSystemAccountByHandleIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	sys, err := s.EnsureSystemAccount(ctx)
	if err != nil {
		t.Fatalf("EnsureSystemAccount: %v", err)
	}
	// Positive anchor: the account EXISTS (GetAccount resolves it), so the
	// AgentByHandle ErrNotFound below is unambiguously the IsAgent gate rejecting
	// an existing non-agent account — not the noRows path masking a seed that
	// silently no-op'd. Without this, a hypothetical EnsureSystemAccount
	// regression would leave the test green for the wrong reason.
	if _, err := s.GetAccount(ctx, sys.ID); err != nil {
		t.Fatalf("GetAccount(%s) after seed: %v; the account must exist for the ErrNotFound below to prove the IsAgent gate", sys.ID, err)
	}

	// A system account carries a global (owner_user_id NULL) handle row, never an
	// agent-namespace one, so an owner-qualified agent lookup misses regardless of
	// the owner passed — the IsAgent gate's fail-closed, indistinguishable from
	// unknown.
	owner := mustUser(t, s, "owner")
	_, err = s.AgentByHandle(ctx, owner.ID, SystemAccountHandle)
	sentinelIs(t, err, ErrNotFound, "AgentByHandle on the reserved system handle")
}

// TestSystemAccountListAccountsVisibility asserts the directory visibility of
// @compass is governed purely by the shared-channel EXISTS disjunct in
// accountVisibleFromWhere, NOT by any system-specific rule. A stranger sharing no
// channel with @compass does not see it (it has no user_accounts row, so it is
// outside the broad user disjunct); a co-member sharing a channel with @compass
// does see it (the EXISTS disjunct). Reddens if @compass gained a user_accounts
// row: the stranger sub-case would then see it via the user disjunct.
func TestSystemAccountListAccountsVisibility(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	sys, err := s.EnsureSystemAccount(ctx)
	if err != nil {
		t.Fatalf("EnsureSystemAccount: %v", err)
	}

	t.Run("stranger sharing no channel does not see @compass", func(t *testing.T) {
		stranger := mustUser(t, s, "stranger")
		got, err := s.ListAccounts(ctx, stranger.ID)
		if err != nil {
			t.Fatalf("ListAccounts(stranger): %v", err)
		}
		if accountIDSet(got)[sys.ID] {
			t.Fatalf("stranger sees the system account %s despite sharing no channel; @compass has no user row and must be outside the user disjunct", sys.ID)
		}
	})

	t.Run("co-member sharing a channel sees @compass", func(t *testing.T) {
		comember := mustUser(t, s, "comember")
		ch := mustNamedChannelWith(t, s, comember.ID, "with-system")
		insertSystemMember(t, s, ch, sys.ID)

		got, err := s.ListAccounts(ctx, comember.ID)
		if err != nil {
			t.Fatalf("ListAccounts(comember): %v", err)
		}
		if !accountIDSet(got)[sys.ID] {
			t.Fatalf("co-member cannot see the system account %s despite sharing a channel; the shared-channel EXISTS disjunct must surface it", sys.ID)
		}
	})
}

// accountsFromIDs adapts a []AccountID (the delivery/roster read shape) into the
// []Account accountIDSet indexes, so the same membership helper serves both the
// id-returning reads and the account-returning ListAccounts reads.
func accountsFromIDs(ids []AccountID) []Account {
	accts := make([]Account, len(ids))
	for i, id := range ids {
		accts[i] = Account{ID: id}
	}
	return accts
}

// TestLinearBridgeAccountSeedsIdempotently asserts EnsureLinearBridgeAccount
// mints @linear as a single system-subtype row and that a second call resolves
// the SAME id rather than a duplicate — the unique-violation-means-fetch restart
// path, exactly as @compass. It also pins that @linear is a DISTINCT account from
// @compass, so the two system handles never collapse into one row.
func TestLinearBridgeAccountSeedsIdempotently(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	first, err := s.EnsureLinearBridgeAccount(ctx)
	if err != nil {
		t.Fatalf("first EnsureLinearBridgeAccount: %v", err)
	}
	if first.System == nil {
		t.Fatalf("@linear has nil System subtype: %+v", first)
	}
	if first.User != nil || first.Agent != nil {
		t.Fatalf("@linear carries a user/agent subtype: %+v", first)
	}
	if first.Handle != LinearBridgeAccountHandle {
		t.Fatalf("@linear handle = %q, want %q", first.Handle, LinearBridgeAccountHandle)
	}
	if first.DisplayName != "Linear" {
		t.Fatalf("@linear display name = %q, want %q", first.DisplayName, "Linear")
	}

	second, err := s.EnsureLinearBridgeAccount(ctx)
	if err != nil {
		t.Fatalf("second EnsureLinearBridgeAccount: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("EnsureLinearBridgeAccount not idempotent: first id %q, second id %q", first.ID, second.ID)
	}
	if second.System == nil {
		t.Fatalf("second call returned non-system account: %+v", second)
	}

	// Exactly one @linear row exists after two calls.
	var n int
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM accounts WHERE handle = $1", LinearBridgeAccountHandle,
	).Scan(&n); err != nil {
		t.Fatalf("count @linear rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("@linear row count = %d, want 1 after two idempotent seeds", n)
	}

	// @linear and @compass are distinct system accounts.
	sys, err := s.EnsureSystemAccount(ctx)
	if err != nil {
		t.Fatalf("EnsureSystemAccount: %v", err)
	}
	if sys.ID == first.ID {
		t.Fatalf("@linear and @compass collapsed into one account id %q", first.ID)
	}
}

// TestLinearBridgeAccountExcludedFromDeliverSet asserts SubscribedAgents never
// returns @linear, even when it is a subscribed member — the same structural
// INNER JOIN agent_accounts exclusion that protects @compass, proved
// contrastively against a real agent member.
func TestLinearBridgeAccountExcludedFromDeliverSet(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	owner := mustUser(t, s, "owner")
	author := mustAgent(t, s, owner.ID, "author")
	recip := mustAgent(t, s, owner.ID, "recip")
	ch := mustNamedChannelWith(t, s, owner.ID, "shared", author.ID, recip.ID)
	subscribeAgent(t, s, owner.ID, ch, recip.ID)

	linear, err := s.EnsureLinearBridgeAccount(ctx)
	if err != nil {
		t.Fatalf("EnsureLinearBridgeAccount: %v", err)
	}
	insertSystemMember(t, s, ch, linear.ID)

	agents, err := s.SubscribedAgents(ctx, ch, author.ID)
	if err != nil {
		t.Fatalf("SubscribedAgents: %v", err)
	}
	got := accountIDSet(accountsFromIDs(agents))
	if !got[recip.ID] {
		t.Fatalf("deliver set %v missing the real agent member %s; the query must resolve agents", agents, recip.ID)
	}
	if got[linear.ID] {
		t.Fatalf("deliver set %v leaked the @linear system account %s; it must never be a delivery recipient", agents, linear.ID)
	}
}

// TestLinearBridgeAccountExcludedFromAgentRoster asserts ChannelAgentMembers
// never returns @linear, mirroring the @compass roster exclusion.
func TestLinearBridgeAccountExcludedFromAgentRoster(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	owner := mustUser(t, s, "owner")
	author := mustAgent(t, s, owner.ID, "author")
	member := mustAgent(t, s, owner.ID, "member")
	ch := mustNamedChannelWith(t, s, owner.ID, "shared", author.ID, member.ID)

	linear, err := s.EnsureLinearBridgeAccount(ctx)
	if err != nil {
		t.Fatalf("EnsureLinearBridgeAccount: %v", err)
	}
	insertSystemMember(t, s, ch, linear.ID)

	agents, err := s.ChannelAgentMembers(ctx, ch, author.ID)
	if err != nil {
		t.Fatalf("ChannelAgentMembers: %v", err)
	}
	got := accountIDSet(accountsFromIDs(agents))
	if !got[member.ID] {
		t.Fatalf("roster %v missing the real agent member %s; the query must resolve agent members", agents, member.ID)
	}
	if got[linear.ID] {
		t.Fatalf("roster %v leaked the @linear system account %s; it must never appear in the agent roster", agents, linear.ID)
	}
}

// TestLinearBridgeAccountByHandleIsNotFound asserts AgentByHandle("linear") fails
// closed as ErrNotFound after @linear is seeded: it exists as an account but has
// no agent_accounts row, so the IsAgent gate rejects it. The GetAccount anchor
// proves the account exists so the ErrNotFound is the gate, not a no-op seed.
func TestLinearBridgeAccountByHandleIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	linear, err := s.EnsureLinearBridgeAccount(ctx)
	if err != nil {
		t.Fatalf("EnsureLinearBridgeAccount: %v", err)
	}
	if _, err := s.GetAccount(ctx, linear.ID); err != nil {
		t.Fatalf("GetAccount(%s) after seed: %v; the account must exist for the ErrNotFound below to prove the IsAgent gate", linear.ID, err)
	}

	owner := mustUser(t, s, "owner")
	_, err = s.AgentByHandle(ctx, owner.ID, LinearBridgeAccountHandle)
	sentinelIs(t, err, ErrNotFound, "AgentByHandle on the reserved @linear handle")
}

// TestReservedHandleRejectsLinearForUserAndAgent asserts the reserved-handle
// guard rejects `linear` for both user and agent creation with
// ErrInvalidArgument and writes no row — the T1 guard extended to the bridge
// handle, mirroring the `compass` rejection.
func TestReservedHandleRejectsLinearForUserAndAgent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	_, err := s.CreateUser(ctx, NewUser{Handle: LinearBridgeAccountHandle, DisplayName: "x"})
	sentinelIs(t, err, ErrInvalidArgument, "CreateUser reserved @linear handle")
	assertNoAccountRow(t, s, LinearBridgeAccountHandle)

	owner := mustUser(t, s, "owner")
	_, err = s.CreateAgent(ctx, owner.ID, NewAgent{Handle: LinearBridgeAccountHandle, DisplayName: "x"})
	sentinelIs(t, err, ErrInvalidArgument, "CreateAgent reserved @linear handle")
	assertNoAccountRow(t, s, LinearBridgeAccountHandle)
}
