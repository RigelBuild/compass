package comms

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/sealedsecurity/compass/go/internal/store"
)

// The manager-comms coordination-channel reconcile (SEA-1722 T5, design.md:
// 530-592). A manager's coordination channel is auto-provisioned from the agent
// tree's parent edges: the store's two parent-edge writers (CreateAgent,
// ReparentAgent) invoke a store-registered hook on their own tx right after
// writing parent_agent_id, and the closure registered here does the in-tx
// reconcile — channel upsert + membership rows + D2 cursor seeds — all on that
// same tx, so the coordination state commits atomically with the tree edge.
//
// The ChannelChanged event is emitted POST-COMMIT, best-effort (design.md:
// 170-173, 555-556): the in-tx reconcile cannot publish (its writes are not yet
// visible, and a rolled-back tx must publish nothing), so it records what it
// changed into a ctx-scoped buffer that the parent-edge RPC drains and emits
// after the store confirms the commit. A lost emit self-heals on the next
// reconcile / D1 sweep, so a dropped event never leaves the tree edge and the
// channel state divergent.
//
// IN-TX vs NEVER-ROLLS-BACK-THE-PARENT-WRITE (design.md:554 in-tx vs 581-582,
// 591-592 never wedges report creation): NO savepoint. The reconcile runs
// directly on the parent-edge writer's tx, and its ONLY expected failure — a
// same-owner name collision — is resolved WITHOUT erroring, by the advisory-lock
// + SELECT-guided suffix search in UpsertCoordinationChannelTx (a user's
// manually-named channel is suffixed around, never adopted, never a
// unique-violation). So normal operation cannot poison the tx; only a genuine
// unexpected store fault returns an error, and that correctly rolls back the
// whole parent-edge write (fail-loud beats a silently half-provisioned tree).

// coordChange is one coordination channel the in-tx reconcile touched, buffered
// for the post-commit ChannelChanged emit: the channel id to re-read and the
// accounts a membership resync removed (carried in removed_account_ids so a
// departing report gets its one final event, design.md:567-568).
type coordChange struct {
	channelID store.ChannelID
	removed   []store.AccountID
}

// coordChanges is the mutable post-commit emit buffer the in-tx reconcile appends
// to (via the ctx it is threaded) and the parent-edge RPC drains after the store
// commits. A pointer lives in the ctx so the hook — which cannot return values
// through its error-only signature — hands its result back to the caller that
// owns the emit. Confined to one RPC's single-threaded call chain, so no lock.
type coordChanges struct {
	pending []coordChange
}

type coordChangesKey struct{}

// withCoordChanges installs a fresh coordination emit buffer on ctx and returns
// both, so a parent-edge RPC can thread the buffer into the store call (the hook
// finds it) and drain it post-commit.
func withCoordChanges(ctx context.Context) (context.Context, *coordChanges) {
	cc := &coordChanges{}
	return context.WithValue(ctx, coordChangesKey{}, cc), cc
}

// coordChangesFrom returns the emit buffer on ctx, if a parent-edge RPC installed
// one. Absent on any other path that reaches the hook (there is none today), in
// which case the reconcile still runs but records nothing to emit — the state is
// correct and self-heals its event on the next reconcile.
func coordChangesFrom(ctx context.Context) (*coordChanges, bool) {
	cc, ok := ctx.Value(coordChangesKey{}).(*coordChanges)
	return cc, ok
}

// coordinationChannelSuffix is the base name suffix for a manager's coordination
// channel: `<manager-handle>-coordination` (design.md:562). Handles are globally
// unique, so the base name is deterministic per manager.
const coordinationChannelSuffix = "-coordination"

// RegisterCoordinationHook registers this handler's in-tx coordination reconcile
// as st's CoordinationHook, wired once at server assembly before serving
// (SEA-1722 T5). It is the comms->store direction of the coordination wiring: the
// store invokes the closure on its own tx from the two parent-edge writers, so
// the reconcile runs comms-owned logic without the store importing comms. The
// hook does NOT install a coordChanges buffer itself — the parent-edge RPC
// (CreateAgent/ReparentAgent) installs one and drains it post-commit, so a hook
// firing on a path with no buffer still reconciles correctly and self-heals its
// event.
func (c *Comms) RegisterCoordinationHook(st *store.Store) {
	st.SetCoordinationHook(c.reconcileCoordinationTx)
}

// reconcileCoordinationTx is the in-tx coordination reconcile registered as the
// store's CoordinationHook: on the passed tx (mid-parent-edge-write) it (a)
// resolves the manager's handle + owning user; (b) get-or-creates the per-owner
// coordination namespace group and UPSERTs the `<handle>-coordination` channel in
// it (owner=manager, OWNER_ONLY, mandatory_subscription), suffixing around a
// user-owned name collision; (c) resyncs membership to manager + current reports,
// seeding each agent member's D2 cursor in this tx; (d) records the change for the
// post-commit emit. Idempotent per manager: a re-run with the tree unchanged
// upserts nothing new and resyncs to the same set. ALL reads/writes use tx, never
// the pool — the tx is mid-flight.
func (c *Comms) reconcileCoordinationTx(ctx context.Context, tx pgx.Tx, managerAgentID store.AccountID) error {
	handle, ownerUserID, err := c.store.ResolveCoordinationManagerTx(ctx, tx, managerAgentID)
	if err != nil {
		return err
	}

	// Serialize every coordination reconcile under this owner so the group
	// get-or-create and the channel provision cannot race a concurrent
	// first-report for a different manager of the same owner into two groups or
	// two channels (design.md fork-2 resolution). Auto-released at tx end.
	if err := store.LockOwnerCoordinationTx(ctx, tx, ownerUserID); err != nil {
		return err
	}

	groupID, err := c.store.EnsureOwnerCoordinationGroupTx(ctx, tx, ownerUserID)
	if err != nil {
		return err
	}

	channelID, err := c.store.UpsertCoordinationChannelTx(ctx, tx, store.CoordinationChannelSpec{
		GroupID:        groupID,
		BaseName:       handle + coordinationChannelSuffix,
		OwnerAccountID: managerAgentID,
		Policy: store.ChannelPolicy{
			PostPolicy:            store.ChannelPostPolicyOwnerOnly,
			OwnerAccountID:        managerAgentID,
			MandatorySubscription: true,
		},
	})
	if err != nil {
		return err
	}

	want, err := c.store.CoordinationReports(ctx, tx, managerAgentID)
	if err != nil {
		return err
	}
	removed, err := c.store.SetCoordinationMembersTx(ctx, tx, channelID, want)
	if err != nil {
		return err
	}

	if cc, ok := coordChangesFrom(ctx); ok {
		cc.pending = append(cc.pending, coordChange{channelID: channelID, removed: removed})
	}
	return nil
}

// emitCoordChanges publishes the buffered coordination ChannelChanged events
// AFTER the store commit, best-effort: it re-reads each touched channel and fans
// a ChannelChanged (carrying removed_account_ids). A read failure is logged and
// skipped, never propagated — the tree edge already committed, and the event
// self-heals on the next reconcile / D1 sweep (design.md:555-556). NEVER call
// this before the commit.
func (c *Comms) emitCoordChanges(ctx context.Context, cc *coordChanges) {
	if cc == nil {
		return
	}
	for _, ch := range cc.pending {
		channel, err := c.store.GetChannel(ctx, ch.channelID)
		if err != nil {
			slog.WarnContext(ctx, "coordination: post-commit channel read for event failed; self-heals on next reconcile",
				"channel_id", string(ch.channelID), "error", err.Error())
			continue
		}
		c.publishChannelChanged(channel, ch.removed)
	}
}

// EnsureCoordinationChannel is the manual/backfill entrypoint (design.md:558):
// it runs the full coordination reconcile for managerAgentID in a store tx it
// owns (not a parent-edge writer's), commits, then emits the ChannelChanged
// post-commit best-effort. Returns the reconciled channel's id. Idempotent — a
// second call for an unchanged tree resumes the same channel and resyncs to the
// same members.
func (c *Comms) EnsureCoordinationChannel(ctx context.Context, managerAgentID store.AccountID) (store.ChannelID, error) {
	ctx, changes := withCoordChanges(ctx)
	if err := c.store.WithTx(ctx, func(tx pgx.Tx) error {
		return c.reconcileCoordinationTx(ctx, tx, managerAgentID)
	}); err != nil {
		return "", edgeError(err)
	}
	if len(changes.pending) == 0 {
		// The reconcile always records exactly one change on success; a missing
		// record is an internal invariant break, surfaced rather than returning
		// an empty id. Checked BEFORE the emit so the fault is not masked by a
		// best-effort emit step that would iterate an empty buffer.
		return "", edgeError(fmt.Errorf("%w: coordination reconcile recorded no channel", store.ErrNotFound))
	}
	c.emitCoordChanges(ctx, changes)
	return changes.pending[0].channelID, nil
}

// ReconcileCoordinationMembership is the membership-only manual resync
// (design.md:559): same own-tx + post-commit-emit shape as
// EnsureCoordinationChannel. The in-tx reconcile is already a full membership
// resync (channel upsert is idempotent, so re-running it is the membership
// resync), so this shares the same closure — the distinct entrypoint name states
// the caller's intent (fix drifted membership) without a second code path.
func (c *Comms) ReconcileCoordinationMembership(ctx context.Context, managerAgentID store.AccountID) error {
	ctx, changes := withCoordChanges(ctx)
	if err := c.store.WithTx(ctx, func(tx pgx.Tx) error {
		return c.reconcileCoordinationTx(ctx, tx, managerAgentID)
	}); err != nil {
		return edgeError(err)
	}
	c.emitCoordChanges(ctx, changes)
	return nil
}
