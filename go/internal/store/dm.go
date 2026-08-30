package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// dmGroupName is the fixed reserved name of the per-owner DM group — the
// namespace every one of an owner's peer-DM channels lives in (design R3). It is
// distinct from the coordination group's __coordination__ so the two reserved
// namespaces never collide, and it is the discriminator (paired with
// VisibilityOwner) both the CreateChannel create-guard and EnsureOwnerDMGroupTx
// key on.
const dmGroupName = "__dm__"

// DMChannelSpec is the resolved identity + membership for one peer-DM channel,
// computed by the OpenDM path (which owns the deterministic sorted-handle NAME
// and the two agent parties) and handed to UpsertDMChannelTx (which owns the
// SQL). The store never encodes the name derivation or the DM policy — it only
// persists what the OpenDM edge decides. Members are the two agent parties,
// pre-resolved to account ids; UpsertDMChannelTx pulls in each party's owning
// user(s) itself (transitive owner-membership).
type DMChannelSpec struct {
	GroupID ChannelGroupID
	Name    string
	Members []AccountID
}

// EnsureOwnerDMGroupTx get-or-creates the single per-owner namespace group that
// hosts every one of ownerUserID's peer-DM channels, on tx, and returns its id.
// It mirrors EnsureOwnerCoordinationGroupTx exactly — deterministic + idempotent
// on owner_user_id + the fixed reserved name, VisibilityOwner (DMs are
// owner-private, never lattice-shared), un-parented — differing only in the
// reserved name (__dm__ vs __coordination__) so the two reserved namespaces stay
// disjoint. The get-half is VISIBILITY-DISCRIMINATED (AND visibility = $3, bound
// to VisibilityOwner): CreateChannelGroup has no reserved-name guard, so a user
// CAN plant a top-level group named __dm__ at any visibility; a wider
// (VisibilityShared) planted group must NEVER be adopted (it would host
// owner-private DMs in a shared group — a cross-tenant leak), so the
// discriminator excludes it and the create-half INSERTs the correct
// owner-visible group. The caller holds the per-owner DM advisory lock
// (LockOwnerDMTx), so the SELECT-then-INSERT cannot race a concurrent first-open
// for the same owner into two groups.
func (s *Store) EnsureOwnerDMGroupTx(ctx context.Context, tx pgx.Tx, ownerUserID AccountID) (ChannelGroupID, error) {
	if ownerUserID == "" {
		return "", fmt.Errorf("%w: owner user id is required", ErrInvalidArgument)
	}

	var existing string
	switch err := tx.QueryRow(ctx,
		`SELECT id FROM channel_groups WHERE owner_user_id = $1 AND name = $2 AND parent_group_id IS NULL AND visibility = $3`,
		string(ownerUserID), dmGroupName, int32(VisibilityOwner),
	).Scan(&existing); {
	case err == nil:
		return ChannelGroupID(existing), nil
	case !noRows(err):
		return "", fmt.Errorf("store: resolve dm group: %w", err)
	}

	id := newID()
	if _, err := tx.Exec(ctx,
		`INSERT INTO channel_groups (id, name, parent_group_id, owner_user_id, visibility) VALUES ($1, $2, NULL, $3, $4)`,
		id, dmGroupName, string(ownerUserID), int32(VisibilityOwner),
	); err != nil {
		return "", fmt.Errorf("store: insert dm group: %w", err)
	}
	return ChannelGroupID(id), nil
}

// UpsertDMChannelTx get-or-creates spec's peer-DM channel on tx and returns its
// id plus whether it was created this call. Unlike UpsertCoordinationChannelTx
// there is NO suffix search: a DM name is the deterministic sorted-handle pair
// key, so a collision on (group, name) is EITHER the caller's own prior open (a
// RESUME) or a squat the R3 belt rejects — never a name to route around.
//
// Resolution loop (the caller holds the per-owner DM advisory lock, so no
// concurrent OPEN for the same owner interleaves — but the loop still tolerates a
// concurrent writer for defense-in-depth):
//   - A channel exists at (group, name): RESUME. Run the R3 belt verify-reconcile
//     (kind=DM ∧ mandatory ∧ {members} ⊆ actual, reconciling recoverable drift;
//     a wrong kind → ErrNotFound, never adopted) and return (id, false, nil).
//   - No channel: INSERT it born kind=DM, zero-value policy (OPEN, ownerless) +
//     mandatory_subscription=true, poison-free via ON CONFLICT (group_id, name)
//     DO NOTHING on the partial unique index. On a returned row → created path:
//     insert the expanded member set (both parties + their owners) and seed every
//     agent member's delivery cursor in this tx (the born-mandatory discipline),
//     return (id, true, nil). On no row (a concurrent open won the race) → loop
//     back to re-SELECT, which now resumes the committed row.
func (s *Store) UpsertDMChannelTx(ctx context.Context, tx pgx.Tx, spec DMChannelSpec) (ChannelID, bool, error) {
	if spec.GroupID == "" {
		return "", false, fmt.Errorf("%w: dm channel group is required", ErrInvalidArgument)
	}
	if spec.Name == "" {
		return "", false, fmt.Errorf("%w: dm channel name is required", ErrInvalidArgument)
	}
	if len(spec.Members) < 2 {
		return "", false, fmt.Errorf("%w: dm channel requires two agent parties", ErrInvalidArgument)
	}

	// The final member set: both agent parties plus each party's owning user(s)
	// — the transitive owner-membership invariant (design.md:231-234), computed
	// identically on the create and resume paths so a reconcile converges on
	// exactly what a create would have produced. One party is the actor, the
	// rest are the requested set.
	members, err := expandOwnerMembership(ctx, tx, spec.Members[0], spec.Members[1:])
	if err != nil {
		return "", false, err
	}

	for {
		var (
			existingID   string
			existingKind int32
		)
		switch err := tx.QueryRow(ctx,
			`SELECT id, kind FROM channels WHERE group_id = $1 AND name = $2`,
			string(spec.GroupID), spec.Name,
		).Scan(&existingID, &existingKind); {
		case err == nil:
			// Resume: the R3 belt verifies + reconciles the resolved row before
			// adopting it, and returns ErrNotFound on a wrong-kind squat.
			if err := verifyReconcileDMTx(ctx, tx, ChannelID(existingID), ChannelKind(existingKind), members); err != nil {
				return "", false, err
			}
			return ChannelID(existingID), false, nil
		case !noRows(err):
			return "", false, fmt.Errorf("store: resolve dm channel: %w", err)
		}

		// Free name (as of our SELECT): INSERT born kind=DM, zero-value policy
		// (OPEN, no owner) + mandatory. ON CONFLICT DO NOTHING absorbs a row a
		// concurrent open committed between our SELECT and this INSERT — zero
		// rows returned rather than a raised unique-violation — so the tx is
		// never poisoned; we loop and resume the committed row.
		id := newID()
		switch err := tx.QueryRow(ctx,
			`INSERT INTO channels (id, name, group_id, kind, post_policy, owner_account_id, mandatory_subscription) `+
				`VALUES ($1, $2, $3, $4, $5, NULL, $6) `+
				`ON CONFLICT (group_id, name) WHERE group_id IS NOT NULL DO NOTHING `+
				`RETURNING id`,
			id, spec.Name, string(spec.GroupID), int32(ChannelKindDM),
			int32(ChannelPostPolicyOpen), true,
		).Scan(&id); {
		case err == nil:
			for _, m := range members {
				if _, err := tx.Exec(ctx,
					`INSERT INTO channel_members (channel_id, account_id, subscribed) VALUES ($1, $2, FALSE) `+
						`ON CONFLICT (channel_id, account_id) DO NOTHING`,
					id, string(m),
				); err != nil {
					return "", false, upsertMemberErr(err, m)
				}
			}
			// Born mandatory ⇒ every member is a delivery target regardless of
			// the subscribed flag (the D1 disjunct), so each agent member's
			// delivery cursor MUST be seeded in this same tx — an un-seeded
			// delivery target is the fail-DANGEROUS D2 hazard. Self-guarding
			// (agent-only) and idempotent, so human members are a no-op.
			if err := seedChannelDeliveryCursors(ctx, tx, ChannelID(id)); err != nil {
				return "", false, err
			}
			return ChannelID(id), true, nil
		case noRows(err):
			// A concurrent open won the (group, name) race. Re-SELECT resolves it
			// as a resume on the next iteration.
			continue
		default:
			return "", false, fmt.Errorf("store: insert dm channel: %w", err)
		}
	}
}

// LockOwnerDMTx takes the per-owner advisory lock that serializes every peer-DM
// open under one owner's DM namespace, on tx (auto-released at tx end). It
// mirrors LockOwnerCoordinationTx with a DISTINCT key domain ('dm:' vs
// 'coordination:') so DM opens never serialize behind coordination reconciles
// (or the per-owner-tree reparent lock). hashtext widens the text key to the int
// the advisory lock takes; a hash collision across two owners is a benign
// redundant wait, never a wrong result.
func LockOwnerDMTx(ctx context.Context, tx pgx.Tx, ownerUserID AccountID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('dm:' || $1))`, string(ownerUserID)); err != nil {
		return fmt.Errorf("store: lock owner dm: %w", err)
	}
	return nil
}

// verifyReconcileDMTx is the R3 belt (design.md:355-361, T2:708-710): on a resume
// it asserts the resolved channel is a real DM and reconciles recoverable drift
// in-tx, or refuses to adopt a wrong-kind row.
//
//   - A wrong kind (a hand-planted squat, or drift) → ErrNotFound. The
//     CreateChannel create-guard makes this unreachable via the manual path, but
//     the belt defends any other write into the reserved group rather than
//     resolving every future open to a hostile row.
//   - mandatory drift (a DM found non-mandatory) is recoverable: re-assert it,
//     then seed every current agent member's cursor (the SetChannelPolicy
//     flip discipline — a FALSE→TRUE re-assert makes every member a delivery
//     target, so none may be left un-seeded, the fail-DANGEROUS D2 hazard).
//   - a missing wanted member is recoverable: re-add its row, then seed cursors
//     again so the re-added member is a seeded delivery target. Both seeds are
//     self-guarding (agent-only) and idempotent — a present member and an
//     already-seeded cursor are no-ops.
func verifyReconcileDMTx(ctx context.Context, tx pgx.Tx, channelID ChannelID, kind ChannelKind, wanted []AccountID) error {
	if kind != ChannelKindDM {
		return fmt.Errorf("%w: dm channel %q", ErrNotFound, channelID)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE channels SET mandatory_subscription = TRUE WHERE id = $1 AND mandatory_subscription = FALSE`,
		string(channelID),
	); err != nil {
		return fmt.Errorf("store: reassert dm mandatory: %w", err)
	}
	// Seed EVERY current agent member's delivery cursor (not only re-added ones),
	// matching SetChannelPolicy's mandatory-flip discipline: a FALSE→TRUE
	// re-assert makes every member a delivery target, so a pre-existing member
	// that somehow lacked a cursor must not be left an un-seeded delivery target
	// (the fail-DANGEROUS D2 hazard). Self-guarding (agent-only) and idempotent,
	// so an already-seeded member is a no-op.
	if err := seedChannelDeliveryCursors(ctx, tx, channelID); err != nil {
		return err
	}
	for _, m := range wanted {
		if _, err := tx.Exec(ctx,
			`INSERT INTO channel_members (channel_id, account_id, subscribed) VALUES ($1, $2, FALSE) `+
				`ON CONFLICT (channel_id, account_id) DO NOTHING`,
			string(channelID), string(m),
		); err != nil {
			return upsertMemberErr(err, m)
		}
	}
	// Re-added members get their cursor from the seed-all above only if they were
	// present at that statement; a member added just now by the loop needs its own
	// seed, so seed once more after the adds (idempotent).
	if err := seedChannelDeliveryCursors(ctx, tx, channelID); err != nil {
		return err
	}
	return nil
}

// isReservedDMGroupTx reports whether groupID is a reserved per-owner DM group —
// the discriminator the CreateChannel create-guard (R3 primary defense) keys on:
// the fixed reserved name AND VisibilityOwner (symmetric with the coordination
// group's visibility-discriminated get-half). A group id that names no group is
// not reserved (false, nil) — the caller's own not-found handling covers an
// unknown group. Because only the OpenDM path writes into a group this predicate
// matches, guarding CreateChannel against it makes squatting a dm--… name
// impossible with no in-advance existence check.
func isReservedDMGroupTx(ctx context.Context, tx pgx.Tx, groupID ChannelGroupID) (bool, error) {
	var (
		name string
		vis  int32
	)
	switch err := tx.QueryRow(ctx,
		`SELECT name, visibility FROM channel_groups WHERE id = $1`,
		string(groupID),
	).Scan(&name, &vis); {
	case err == nil:
		return name == dmGroupName && ChannelGroupVisibility(vis) == VisibilityOwner, nil
	case noRows(err):
		return false, nil
	default:
		return false, fmt.Errorf("store: resolve dm group discriminator: %w", err)
	}
}
