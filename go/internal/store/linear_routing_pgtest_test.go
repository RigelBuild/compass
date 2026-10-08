//go:build pgtest

package store

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// routingParties is an admin, its root supervisor agent, and the Linear bridge.
func routingParties(t *testing.T, s *Store) (admin, supervisor, bridge Account) {
	t.Helper()
	admin = mustUser(t, s, "routing-admin")
	supervisor = mustAgent(t, s, admin.ID, "supervisor")
	bridge, err := s.EnsureLinearBridgeAccount(t.Context())
	if err != nil {
		t.Fatalf("EnsureLinearBridgeAccount: %v", err)
	}
	return admin, supervisor, bridge
}

// routingGroupID resolves the admin's reserved, owner-visible __linear__ group.
func routingGroupID(t *testing.T, s *Store, adminID AccountID) ChannelGroupID {
	t.Helper()
	var gid string
	if err := s.pool.QueryRow(t.Context(),
		`SELECT id FROM channel_groups WHERE owner_user_id = $1 AND name = $2 AND parent_group_id IS NULL AND visibility = $3`,
		string(adminID), linearRoutingGroupName, int32(VisibilityOwner),
	).Scan(&gid); err != nil {
		t.Fatalf("resolve linear routing group for %s: %v", adminID, err)
	}
	return ChannelGroupID(gid)
}

// TestEnsureLinearRoutingChannelSkipsSharedGroup: a planted SHARED __linear__ group
// holding a routing channel is never adopted by the ensure or the keyed read.
func TestEnsureLinearRoutingChannelSkipsSharedGroup(t *testing.T) {
	s := newTestStore(t)
	admin, supervisor, bridge := routingParties(t, s)
	stranger := mustUser(t, s, "stranger")
	// CreateChannelGroup and CreateChannel refuse the reserved group, so plant the
	// look-alike group and its routing channel with raw SQL.
	sharedID, plantedID := newID(), newID()
	if _, err := s.pool.Exec(t.Context(),
		"INSERT INTO channel_groups (id, name, parent_group_id, owner_user_id, namespace_owner_id, visibility, tenant_id) VALUES ($1,$2,NULL,$3,$3,$4,$5)",
		sharedID, linearRoutingGroupName, string(admin.ID), int16(VisibilityShared), string(s.resolveTenant(t.Context()))); err != nil {
		t.Fatalf("plant shared __linear__ group: %v", err)
	}
	if _, err := s.pool.Exec(t.Context(),
		`INSERT INTO channels (id, name, group_id, kind, post_policy, owner_account_id, mandatory_subscription, tenant_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		plantedID, LinearRoutingChannelName, sharedID, int32(ChannelKindChannel),
		int32(ChannelPostPolicyOpen), string(stranger.ID), false, string(s.resolveTenant(t.Context()))); err != nil {
		t.Fatalf("plant routing channel in SHARED __linear__: %v", err)
	}

	planted := ChannelID(plantedID)
	_, err := s.LinearRoutingChannel(t.Context(), admin.ID)
	sentinelIs(t, err, ErrNotFound, "routing read with only a SHARED look-alike")

	id, err := s.EnsureLinearRoutingChannel(t.Context(), admin.ID, supervisor.ID, bridge.ID)
	if err != nil {
		t.Fatalf("EnsureLinearRoutingChannel: %v", err)
	}
	if id == planted {
		t.Fatalf("ensure adopted the planted SHARED-group channel %s", planted)
	}
	if ok, err := s.IsChannelMember(t.Context(), supervisor.ID, planted); err != nil || ok {
		t.Fatalf("supervisor membership of planted channel = (%v, %v), want (false, nil)", ok, err)
	}
	got, err := s.LinearRoutingChannel(t.Context(), admin.ID)
	if err != nil {
		t.Fatalf("LinearRoutingChannel after ensure: %v", err)
	}
	if got != id {
		t.Fatalf("LinearRoutingChannel = %s, want the ensured %s", got, id)
	}
}

// TestEnsureLinearRoutingChannelReconcilesDrift: a reserved channel that lost mandatory,
// its open post policy and its members is restored, with the supervisor a delivery target again.
func TestEnsureLinearRoutingChannelReconcilesDrift(t *testing.T) {
	s := newTestStore(t)
	admin, supervisor, bridge := routingParties(t, s)
	id, err := s.EnsureLinearRoutingChannel(t.Context(), admin.ID, supervisor.ID, bridge.ID)
	if err != nil {
		t.Fatalf("EnsureLinearRoutingChannel(first): %v", err)
	}

	for _, stmt := range []string{
		`UPDATE channels SET mandatory_subscription = FALSE WHERE id = $1`,
		`UPDATE channels SET post_policy = 1, owner_account_id = (SELECT account_id FROM channel_members WHERE channel_id = $1 LIMIT 1) WHERE id = $1`,
		`DELETE FROM agent_delivery_cursors WHERE channel_id = $1`,
		`DELETE FROM channel_members WHERE channel_id = $1`,
	} {
		if _, err := s.pool.Exec(t.Context(), stmt, string(id)); err != nil {
			t.Fatalf("plant drift (%s): %v", stmt, err)
		}
	}

	got, err := s.EnsureLinearRoutingChannel(t.Context(), admin.ID, supervisor.ID, bridge.ID)
	if err != nil {
		t.Fatalf("EnsureLinearRoutingChannel(reconcile): %v", err)
	}
	if got != id {
		t.Fatalf("reconcile resolved %s, want the drifted %s", got, id)
	}
	ch, err := s.GetChannel(t.Context(), id)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if !ch.Policy.MandatorySubscription {
		t.Fatal("mandatory_subscription still FALSE after reconcile, want TRUE")
	}
	if ch.Policy.PostPolicy != ChannelPostPolicyOpen || ch.Policy.OwnerAccountID != "" {
		t.Fatalf("policy after reconcile = %+v, want open and ownerless", ch.Policy)
	}
	members := memberSet(ch)
	for _, want := range []AccountID{supervisor.ID, bridge.ID, admin.ID} {
		if !members[want] {
			t.Fatalf("members %v missing %s after reconcile", ch.MemberAccountIDs, want)
		}
	}
	if !cursorExists(t, s, supervisor.ID, id) {
		t.Fatal("supervisor has no delivery cursor after reconcile (D2 hazard)")
	}
	recips, err := s.SubscribedAgents(t.Context(), id, bridge.ID)
	if err != nil {
		t.Fatalf("SubscribedAgents: %v", err)
	}
	if !containsAccount(recips, supervisor.ID) {
		t.Fatalf("supervisor not a delivery target after reconcile (recipients %v)", recips)
	}
}

// TestCreateChannelIntoLinearRoutingGroupIsNotFound: the manual create path is
// refused on the reserved group, with the merged ErrNotFound.
func TestCreateChannelIntoLinearRoutingGroupIsNotFound(t *testing.T) {
	s := newTestStore(t)
	admin, supervisor, bridge := routingParties(t, s)
	if _, err := s.EnsureLinearRoutingChannel(t.Context(), admin.ID, supervisor.ID, bridge.ID); err != nil {
		t.Fatalf("EnsureLinearRoutingChannel: %v", err)
	}

	_, err := s.CreateChannel(t.Context(), admin.ID, NewChannel{
		Name: "side-door", GroupID: routingGroupID(t, s, admin.ID), Kind: ChannelKindChannel,
	})
	sentinelIs(t, err, ErrNotFound, "manual create into the reserved Linear routing group")
}

// TestLinearRoutingChannelRefusesWrongKind: a non-plain row at the reserved key is refused
// by the read and the ensure, never adopted and reconciled into a delivery target.
func TestLinearRoutingChannelRefusesWrongKind(t *testing.T) {
	s := newTestStore(t)
	admin, supervisor, bridge := routingParties(t, s)
	id, err := s.EnsureLinearRoutingChannel(t.Context(), admin.ID, supervisor.ID, bridge.ID)
	if err != nil {
		t.Fatalf("EnsureLinearRoutingChannel: %v", err)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE channels SET kind = $1 WHERE id = $2`, int32(ChannelKindDM), string(id)); err != nil {
		t.Fatalf("plant wrong kind: %v", err)
	}

	_, err = s.LinearRoutingChannel(t.Context(), admin.ID)
	sentinelIs(t, err, ErrNotFound, "routing read onto a wrong-kind row")
	_, err = s.EnsureLinearRoutingChannel(t.Context(), admin.ID, supervisor.ID, bridge.ID)
	sentinelIs(t, err, ErrNotFound, "routing ensure onto a wrong-kind row")
}

// TestEnsureLinearRoutingChannelTakesAdvisoryLock: two ensures block behind a gate holding
// the per-admin lock, then converge on one group and one channel once it is released.
func TestEnsureLinearRoutingChannelTakesAdvisoryLock(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	admin, supervisor, bridge := routingParties(t, s)

	gate, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin gate tx: %v", err)
	}
	defer func() {
		// Releases the lock after an early Fatal; after the explicit release it is closed.
		if err := gate.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("rollback gate: %v", err)
		}
	}()
	// Character-identical to LockLinearRouting: a drifted key stops blocking and fails loudly.
	if _, err := gate.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('linear-routing:' || $1))`, string(admin.ID)); err != nil {
		t.Fatalf("gate advisory lock: %v", err)
	}

	type outcome struct {
		id  ChannelID
		err error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			id, err := s.EnsureLinearRoutingChannel(ctx, admin.ID, supervisor.ID, bridge.ID)
			results <- outcome{id, err}
		}()
	}
	// Event gate on pg_locks, not a timer; polled on the gate's connection so it needs no
	// extra pool slot. An ensure that finishes while the gate holds the key took no lock.
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiters int
		if err := gate.QueryRow(ctx,
			`WITH k AS (SELECT hashtext('linear-routing:' || $1)::bigint AS key)
			 SELECT count(*) FROM pg_locks, k
			 WHERE locktype = 'advisory' AND NOT granted
			   AND classid = ((k.key >> 32) & 4294967295)::oid
			   AND objid = (k.key & 4294967295)::oid`,
			string(admin.ID),
		).Scan(&waiters); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if waiters >= 2 {
			break
		}
		select {
		case r := <-results:
			t.Fatalf("an ensure completed (id=%s err=%v) while the gate held the advisory lock", r.id, r.err)
		case <-deadline:
			t.Fatalf("%d of 2 ensures waited on the per-admin advisory lock before the deadline", waiters)
		case <-tick.C:
		}
	}
	if err := gate.Rollback(ctx); err != nil {
		t.Fatalf("release gate: %v", err)
	}

	var ids [2]ChannelID
	for i := range ids {
		r := <-results
		if r.err != nil {
			t.Fatalf("EnsureLinearRoutingChannel after release: %v", r.err)
		}
		ids[i] = r.id
	}
	if ids[0] != ids[1] {
		t.Fatalf("the two ensures resolved %s and %s, want one channel", ids[0], ids[1])
	}
	var groups, channels int
	if err := s.pool.QueryRow(ctx,
		`SELECT
		   (SELECT COUNT(*) FROM channel_groups WHERE owner_user_id = $1 AND name = $2),
		   (SELECT COUNT(*) FROM channels c JOIN channel_groups g ON g.id = c.group_id
		     WHERE g.owner_user_id = $1 AND g.name = $2)`,
		string(admin.ID), linearRoutingGroupName,
	).Scan(&groups, &channels); err != nil {
		t.Fatalf("count routing rows: %v", err)
	}
	if groups != 1 || channels != 1 {
		t.Fatalf("gated ensures produced %d groups and %d channels, want 1 and 1", groups, channels)
	}
}
