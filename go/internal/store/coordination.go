package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// CoordinationHook is the manager-comms coordination-channel reconcile the comms
// layer registers on the store (RIG-1722 T5, design.md:542-551). The two
// parent-edge writers (CreateAgent, ReparentAgent) invoke it on their OWN pgx.Tx
// right after writing agent_accounts.parent_agent_id, so the reconcile runs in
// the SAME transaction as the parent-edge write WITHOUT the store importing
// comms/channel types — the store owns the tx; the hook is an injected callback,
// never a *Comms method reached from inside *Store. managerAgentID is the
// manager whose coordination channel must be reconciled (the PARENT of the agent
// whose edge just changed): its membership resyncs against the manager's current
// reports. A hook returning an error aborts the writer's whole tx (the whole
// parent-edge write rolls back) — fail-loud on a genuine store fault; the comms
// reconcile is written so its only EXPECTED outcome (a name collision) never
// errors (design.md:579-585), so a returned error is always a real fault.
type CoordinationHook func(ctx context.Context, tx pgx.Tx, managerAgentID AccountID) error

// SetCoordinationHook registers the coordination-channel reconcile hook, wired
// once at server assembly before serving (mirrors hub.SetSettleSink). No lock:
// the write happens-before the first concurrent
// parent-edge write. A nil hook (the store-only test path) leaves the writers a
// no-op. The comms layer owns the closure; the store only invokes it.
func (s *Store) SetCoordinationHook(h CoordinationHook) {
	s.coordinationHook = h
}

// invokeCoordinationHook runs the registered coordination hook on tx for
// managerAgentID, a no-op if unregistered. The two parent-edge writers call it
// right after writing parent_agent_id, INSIDE their tx, so the reconcile commits
// atomically with the tree edge (design.md:554). managerAgentID is the empty
// string when the just-written edge has no parent (a root agent has no manager,
// so there is no coordination channel to reconcile); the caller must skip the
// call in that case rather than pass empty.
func (s *Store) invokeCoordinationHook(ctx context.Context, tx pgx.Tx, managerAgentID AccountID) error {
	if s.coordinationHook == nil {
		return nil
	}
	return s.coordinationHook(ctx, tx, managerAgentID)
}

// CoordinationChannelSpec is the resolved identity + policy for one manager's
// coordination channel, computed by the comms reconcile (which owns the NAME and
// the policy values, design.md:543-544) and handed to UpsertCoordinationChannelTx
// (which owns the SQL). Keeping the split here means the store never encodes the
// `<handle>-coordination` naming or the OWNER_ONLY/mandatory policy — it only
// persists what the comms edge decides.
type CoordinationChannelSpec struct {
	// GroupID is the manager's owner's per-owner coordination namespace group,
	// get-or-created by EnsureOwnerCoordinationGroupTx. The channel is always
	// GROUPED so the partial unique index channels_group_name_key applies
	// (design.md:570-573).
	GroupID ChannelGroupID
	// BaseName is the un-suffixed channel name (`<manager-handle>-coordination`);
	// UpsertCoordinationChannelTx resolves the final name, suffixing on an
	// owner-mismatch collision (design.md:579-585).
	BaseName string
	// OwnerAccountID is the manager account that owns/operates the channel; it is
	// the resume-vs-suffix discriminator (resume only when an existing same-named
	// channel's owner_account_id matches this).
	OwnerAccountID AccountID
	// Policy is the fixed coordination policy (OWNER_ONLY + mandatory_subscription
	// + owner = the manager). Seeded at insert so the channel is born correct.
	Policy ChannelPolicy
}

// EnsureOwnerCoordinationGroupTx get-or-creates the single per-owner namespace
// group that hosts every one of ownerUserID's coordination channels, on tx, and
// returns its id. Deterministic + idempotent: keyed on owner_user_id + a fixed
// reserved name, so re-provisioning any manager under the same owner resolves the
// SAME group — the invariant the same-owner collision analysis depends on
// (design.md:570-585: all of an owner's coordination channels must share one
// group or the collision could never occur). The group is VisibilityOwner
// (coordination is owner-private, never shared) and un-parented (a top-level
// per-owner namespace). The caller holds the per-owner advisory lock, so the
// SELECT-then-INSERT cannot race a concurrent first-report for the same owner.
func (s *Store) EnsureOwnerCoordinationGroupTx(ctx context.Context, tx pgx.Tx, ownerUserID AccountID) (ChannelGroupID, error) {
	if ownerUserID == "" {
		return "", fmt.Errorf("%w: owner user id is required", ErrInvalidArgument)
	}
	// A fixed reserved name scoped to the owner — deterministic, so every
	// reconcile for this owner resolves the identical group. The get-half is
	// VISIBILITY-DISCRIMINATED (AND visibility = $3, bound to VisibilityOwner):
	// CreateChannelGroup has no reserved-name guard, so a user CAN plant a
	// top-level group named __coordination__ at any visibility. A wider
	// (VisibilityShared) planted group must NEVER be adopted — inserting the
	// OWNER_ONLY coordination channel into a SHARED group would make an
	// owner-private channel visible to every account (channelVisiblePredicate),
	// a cross-tenant leak (decision 1 owner-private, decision 3 never-adopt). The
	// discriminator excludes it, so the create-half then INSERTs the correct
	// owner-visible group. Adopting an owner-VISIBILITY, owner-matched,
	// correctly-named top-level planted group is harmless: it has the exact shape
	// the reconcile would itself create. The caller holds the per-owner advisory
	// lock (LockOwnerCoordinationTx), so the get-then-create cannot race a
	// concurrent reconcile for the same owner into two groups; channel_groups has
	// no unique index on (name, owner, parent), and the only unguarded writer
	// (the user's CreateChannelGroup) cannot produce an owner-visibility row the
	// discriminated SELECT would wrongly adopt.
	const coordinationGroupName = "__coordination__"

	qtx := db.New(tx)
	existing, err := qtx.GetCoordinationGroup(ctx, db.GetCoordinationGroupParams{
		OwnerUserID: string(ownerUserID),
		Name:        coordinationGroupName,
		Visibility:  int16(VisibilityOwner),
	})
	switch {
	case err == nil:
		return ChannelGroupID(existing), nil
	case !noRows(err):
		return "", fmt.Errorf("store: resolve coordination group: %w", err)
	}

	id := newID()
	if err := qtx.InsertCoordinationGroup(ctx, db.InsertCoordinationGroupParams{
		ID:          id,
		Name:        coordinationGroupName,
		OwnerUserID: string(ownerUserID),
		Visibility:  int16(VisibilityOwner),
	}); err != nil {
		return "", fmt.Errorf("store: insert coordination group: %w", err)
	}
	return ChannelGroupID(id), nil
}

// UpsertCoordinationChannelTx get-or-creates spec's coordination channel on tx
// and returns its id, resolving the one EXPECTED collision without ever erroring
// the tx (design.md:579-585). It is the store SQL half of the reconcile; the
// comms closure owns spec's name + policy.
//
// Resolution (the caller holds the per-owner advisory lock, so no concurrent
// RECONCILE for the same owner can interleave a second row for the same (group,
// name) — but a concurrent USER CreateChannel is NOT under this lock and can):
//   - No channel named spec.BaseName in spec.GroupID: INSERT it with spec.Policy
//     (born OWNER_ONLY + mandatory + owner=manager), poison-free.
//   - A channel named spec.BaseName exists AND its owner_account_id == the
//     manager: RESUME it — return its id (idempotent re-provision; policy already
//     correct from a prior insert, never re-forced).
//   - A channel named spec.BaseName exists with a DIFFERENT owner (a user's
//     manual channel): NEVER adopt. Deterministically suffix `-2`, `-3`, … and
//     retry the same resolution on the suffixed name until a free name or a
//     manager-owned resume is found. The suffix search terminates: names are
//     probed in increasing integer order and the manager owns at most finitely
//     many, so a free name is always reached.
//
// The INSERT is `ON CONFLICT (group_id, name) WHERE group_id IS NOT NULL DO
// NOTHING` — index-inference on the PARTIAL unique index channels_group_name_key.
// This is what makes the "never poisons the outer parent-edge tx" invariant TRUE
// BY CONSTRUCTION rather than by an unenforced assumption about who else writes:
// a user's concurrent CreateChannel can commit the same (group, name) between our
// SELECT and our INSERT, but ON CONFLICT DO NOTHING yields zero rows (NOT a raised
// unique-violation), so the tx is never poisoned (there is no savepoint,
// design.md:554, so a raised violation would abort the whole parent-edge write —
// wedging report creation, decision 2). On the DO-NOTHING zero-row case we loop:
// the next iteration re-SELECTs, sees the now-present row, and either resumes
// (manager-owned) or suffixes (user-owned, never-adopt, decision 3) — the same
// SELECT-guided resolution above. Termination still holds: each committed row is
// resolved to resume-or-suffix, and the manager owns finitely many names.
func (s *Store) UpsertCoordinationChannelTx(ctx context.Context, tx pgx.Tx, spec CoordinationChannelSpec) (ChannelID, error) {
	if spec.GroupID == "" {
		return "", fmt.Errorf("%w: coordination channel group is required", ErrInvalidArgument)
	}
	if spec.BaseName == "" {
		return "", fmt.Errorf("%w: coordination channel name is required", ErrInvalidArgument)
	}

	qtx := db.New(tx)
	for suffix := 1; ; suffix++ {
		name := spec.BaseName
		if suffix > 1 {
			name = fmt.Sprintf("%s-%d", spec.BaseName, suffix)
		}

		existing, err := qtx.GetCoordinationChannelByName(ctx, db.GetCoordinationChannelByNameParams{
			GroupID: pgtype.Text{String: string(spec.GroupID), Valid: true},
			Name:    name,
		})
		switch {
		case err == nil:
			// A channel with this name already exists. Resume only when the
			// manager owns it; otherwise it is a user's channel we must never
			// adopt — advance to the next suffix.
			if AccountID(existing.OwnerAccountID) == spec.OwnerAccountID {
				return ChannelID(existing.ID), nil
			}
			continue
		case !noRows(err):
			return "", fmt.Errorf("store: resolve coordination channel: %w", err)
		}

		// Free name (as of our SELECT): INSERT the channel born with the
		// coordination policy, poison-free. ON CONFLICT DO NOTHING on the partial
		// unique index absorbs a row a concurrent user CreateChannel committed
		// between our SELECT and this INSERT: instead of a raised unique-violation
		// (which, with no savepoint, would poison the parent-edge tx and wedge
		// report creation), the INSERT affects zero rows and RETURNING yields no
		// row. On that no-row case we loop back to the SELECT, which now sees the
		// concurrently-committed row and resumes-or-suffixes it.
		id := newID()
		insertedID, err := qtx.InsertCoordinationChannel(ctx, db.InsertCoordinationChannelParams{
			ID:                    id,
			Name:                  name,
			GroupID:               pgtype.Text{String: string(spec.GroupID), Valid: true},
			Kind:                  int16(ChannelKindChannel),
			PostPolicy:            int16(spec.Policy.PostPolicy), //nolint:gosec // G115: ChannelPostPolicy is a CHECK-constrained 0/1 enum (channels.post_policy), always within int16
			Column6:               string(spec.Policy.OwnerAccountID),
			MandatorySubscription: spec.Policy.MandatorySubscription,
		})
		switch {
		case err == nil:
			return ChannelID(insertedID), nil
		case noRows(err):
			// A concurrent writer won the (group, name) race between our SELECT
			// and this INSERT. Re-resolve the SAME name (undo the loop's suffix
			// advance): the next iteration's SELECT now sees the committed row and
			// resumes-or-suffixes it. Under the per-owner lock the only concurrent
			// writer is a user CreateChannel (user-owned), so this re-resolve
			// suffixes; keeping it a re-SELECT rather than a blind advance leaves
			// the resume branch correct should that invariant ever weaken.
			suffix--
			continue
		default:
			return "", fmt.Errorf("store: insert coordination channel: %w", err)
		}
	}
}

// SetCoordinationMembersTx resyncs channelID's membership to exactly want on tx:
// it adds every wanted member missing a row (seeding each agent member's D2
// delivery cursor to channel head IN THIS TX, so a mandatory channel never mints
// an un-seeded delivery target — the fail-DANGEROUS D2 hazard), and removes every
// current member not in want, returning the accounts a removal actually deleted
// so the comms edge can carry them in the final ChannelChanged.removed_account_ids
// (design.md:567-568). Membership-set reconcile, idempotent: a re-run with the
// same want adds and removes nothing. want is the manager + its current reports;
// the caller holds the per-owner advisory lock, so the read-then-write of the
// member set is race-free against a concurrent reconcile for the same owner.
func (s *Store) SetCoordinationMembersTx(ctx context.Context, tx pgx.Tx, channelID ChannelID, want []AccountID) ([]AccountID, error) {
	wantSet := make(map[AccountID]bool, len(want))
	for _, m := range want {
		wantSet[m] = true
	}

	qtx := db.New(tx)
	memberIDs, err := qtx.ChannelMemberIDs(ctx, string(channelID))
	if err != nil {
		return nil, fmt.Errorf("store: list coordination members: %w", err)
	}
	current := make(map[AccountID]bool, len(memberIDs))
	for _, m := range memberIDs {
		current[AccountID(m)] = true
	}

	// Add every wanted member missing a row, seeding its delivery cursor in this
	// tx. seedDeliveryCursor is self-guarding (agent-only) and idempotent, so a
	// human member (the owner is a user) yields no cursor row and an existing
	// cursor is untouched.
	for _, m := range want {
		if current[m] {
			continue
		}
		if err := qtx.EnsureChannelMember(ctx, db.EnsureChannelMemberParams{
			ChannelID: string(channelID),
			AccountID: string(m),
		}); err != nil {
			if pgErrIs(err, pgForeignKeyViolation) {
				return nil, fmt.Errorf("%w: unknown coordination member %q", ErrInvalidArgument, m)
			}
			return nil, fmt.Errorf("store: insert coordination member: %w", err)
		}
		if err := seedDeliveryCursor(ctx, tx, m, channelID); err != nil {
			return nil, err
		}
	}

	// Remove every current member not wanted, collecting the deletions for the
	// final ChannelChanged. Iterate `current` in no particular order; the caller
	// treats removed as a set.
	var removed []AccountID
	for m := range current {
		if wantSet[m] {
			continue
		}
		if _, err := qtx.DeleteChannelMember(ctx, db.DeleteChannelMemberParams{
			ChannelID: string(channelID),
			AccountID: string(m),
		}); err != nil {
			return nil, fmt.Errorf("store: remove coordination member: %w", err)
		}
		removed = append(removed, m)
	}
	return removed, nil
}

// CoordinationReports returns managerAgentID + every agent whose parent_agent_id
// is managerAgentID (its direct reports), on tx — the wanted member set for the
// manager's coordination channel. The manager is always included (it owns and
// posts to the channel). Reports are the DIRECT children only (the coordination
// channel is one manager's report set, not a subtree). Ordered by account id for
// a stable set. Runs on the passed tx so it reads the tree state the parent-edge
// write just committed within this same transaction.
func (s *Store) CoordinationReports(ctx context.Context, tx pgx.Tx, managerAgentID AccountID) ([]AccountID, error) {
	reports, err := db.New(tx).CoordinationReports(ctx, pgtype.Text{String: string(managerAgentID), Valid: true})
	if err != nil {
		return nil, fmt.Errorf("store: list coordination reports: %w", err)
	}
	members := make([]AccountID, 0, len(reports)+1)
	members = append(members, managerAgentID)
	for _, m := range reports {
		members = append(members, AccountID(m))
	}
	return members, nil
}

// ResolveCoordinationManagerTx resolves the manager's handle and its owning user
// on tx — the two facts the comms reconcile needs to build the channel spec (the
// name derives from the handle; the group from the owner). An id that names no
// agent account is ErrNotFound. Runs on the passed tx (mid-parent-edge-write).
func (s *Store) ResolveCoordinationManagerTx(ctx context.Context, tx pgx.Tx, managerAgentID AccountID) (handle string, ownerUserID AccountID, err error) {
	row, scanErr := db.New(tx).ResolveCoordinationManager(ctx, string(managerAgentID))
	switch {
	case scanErr == nil:
		return row.Handle, AccountID(row.OwnerUserID), nil
	case noRows(scanErr):
		return "", "", fmt.Errorf("%w: coordination manager %q", ErrNotFound, managerAgentID)
	default:
		return "", "", fmt.Errorf("store: resolve coordination manager: %w", scanErr)
	}
}

// LockOwnerCoordinationTx takes the per-owner advisory lock that serializes every
// coordination reconcile under one owner's namespace, on tx (auto-released at tx
// end). It keys on the owner so the group get-or-create and the channel provision
// for two concurrent first-reports of DIFFERENT managers under the SAME owner
// cannot race into two groups or two channels. hashtext widens the text key to
// the int the advisory lock takes; a hash collision across two owners is a benign
// redundant wait, never a wrong result (mirrors ReparentAgent's per-owner-tree
// lock, accounts.go).
func LockOwnerCoordinationTx(ctx context.Context, tx pgx.Tx, ownerUserID AccountID) error {
	// Namespace the key against ReparentAgent's per-owner-tree lock (which keys on
	// the bare owner) so the two locks never spuriously serialize each other: a
	// reconcile runs INSIDE a parent-edge write that may itself hold the tree
	// lock, and a distinct key avoids a self-deadlock-adjacent double-take while
	// still serializing coordination reconciles against each other.
	if err := db.New(tx).LockOwnerCoordination(ctx, pgtype.Text{String: string(ownerUserID), Valid: true}); err != nil {
		return fmt.Errorf("store: lock owner coordination: %w", err)
	}
	return nil
}

// WithTx runs fn inside a single store transaction, committing on success and
// rolling back on error (or panic). It is the store's transaction seam for the
// comms coordination manual/backfill entrypoints (EnsureCoordinationChannel,
// ReconcileCoordinationMembership, design.md:558-559), which must run the same
// in-tx reconcile the hook runs but under a tx they own (the hook path rides the
// parent-edge writer's tx). Keeping the seam here means the comms layer never
// touches the pgx pool directly. fn must confine all its reads/writes to the
// passed tx.
func (s *Store) WithTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit.
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit tx: %w", err)
	}
	return nil
}
