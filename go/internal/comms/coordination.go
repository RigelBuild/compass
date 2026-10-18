package comms

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/RigelBuild/compass/go/internal/store"
)

// The manager-comms coordination-channel reconcile (RIG-1722 T5). A manager's
// coordination channel is auto-provisioned in-tx from the agent tree's parent edges,
// atomic with the edge; ChannelChanged is emitted POST-COMMIT best-effort (a lost
// emit self-heals on the next sweep). A name collision suffixes without erroring.

// coordChange is one coordination channel the in-tx reconcile touched, buffered
// for the post-commit ChannelChanged emit: the channel id to re-read and the
// accounts a membership resync removed (carried in removed_account_ids so a
// departing report gets its one final event, design.md:567-568).
type coordChange struct {
	channelID store.ChannelID
	removed   []store.AccountID
}

// coordChanges carries hook output through the RPC context because the hook can return only an error.
// The single-RPC call chain confines it to one goroutine.
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
// (RIG-1722 T5). It is the comms->store direction of the coordination wiring: the
// store invokes the closure on its own tx from the two parent-edge writers, so
// the reconcile runs comms-owned logic without the store importing comms. The
// hook does NOT install a coordChanges buffer itself — the parent-edge RPC
// (CreateAgent/ReparentAgent) installs one and drains it post-commit, so a hook
// firing on a path with no buffer still reconciles correctly and self-heals its
// event.
func (c *Comms) RegisterCoordinationHook(st *store.Store) {
	st.SetCoordinationHook(c.reconcileCoordinationTx)
}

// reconcileCoordinationTx upserts the manager's OWNER_ONLY channel and resyncs its members in the parent-edge transaction.
// All reads and writes use tx; the hook runs mid-flight.
// The post-commit event is buffered for emitCoordChanges.
func (c *Comms) reconcileCoordinationTx(ctx context.Context, tx pgx.Tx, managerAgentID store.AccountID) error {
	_, ownerUserID, err := c.store.ResolveCoordinationManagerTx(ctx, tx, managerAgentID)
	if err != nil {
		return err
	}

	// Serialize every coordination reconcile under this owner. Resolve the handle
	// again after locking so a queued reconcile cannot restore an old name after a
	// concurrent account rename commits.
	if err := store.LockOwnerCoordinationTx(ctx, tx, ownerUserID); err != nil {
		return err
	}
	handle, _, err := c.store.ResolveCoordinationManagerTx(ctx, tx, managerAgentID)
	if err != nil {
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

// emitCoordChanges publishes buffered ChannelChanged events after commit.
// Read failures are logged and skipped; a later coordination reconcile can publish the state again.
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
		c.publishChannelChanged(ctx, channel, ch.removed)
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
