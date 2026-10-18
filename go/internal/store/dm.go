package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// dmGroupName is the reserved per-owner peer-DM namespace, distinct from the
// coordination group and paired with owner visibility in its guards.
const dmGroupName = "__dm__"

// coordinationGroupName is the fixed reserved name of the per-owner
// coordination group.
const coordinationGroupName = "__coordination__"

// isReservedGroupName reports whether name is a system group name that a
// caller may not claim at top level. A new name must also join the exemption in
// channel_groups_owner_parent_name_key, which needs a migration.
func isReservedGroupName(name string) bool {
	return name == dmGroupName || name == coordinationGroupName || name == linearRoutingGroupName
}

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

// DMChannelName derives the deterministic sorted-handle pair name for a peer DM.
// Handles are sorted lexicographically so open(a,b) and open(b,a) resolve the
// same channel.
//
// The separator is `:`, a byte the handle grammar excludes, so the split is
// unambiguous. A hyphen delimiter is not injective because handles may contain
// consecutive hyphens.
func DMChannelName(h1, h2 string) string {
	lo, hi := h1, h2
	if lo > hi {
		lo, hi = hi, lo
	}
	return "dm:" + lo + ":" + hi
}

// EnsureOwnerDMGroupTx get-or-creates the single per-owner namespace group that
// hosts every one of ownerUserID's peer-DM channels, on tx, and returns its id.
// It mirrors EnsureOwnerCoordinationGroupTx exactly — deterministic + idempotent
// on owner_user_id + the fixed reserved name, VisibilityOwner (DMs are
// owner-private, never lattice-shared), un-parented — differing only in the
// reserved name (__dm__ vs __coordination__) so the two reserved namespaces stay
// disjoint. The get-half is VISIBILITY-DISCRIMINATED (AND visibility = $3, bound
// to VisibilityOwner): a top-level group named __dm__ may predate the
// reserved-name guard or come from a raw insert, at any visibility; a wider
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

	qtx := db.New(tx)
	switch existing, err := qtx.GetOwnerDMGroup(ctx, db.GetOwnerDMGroupParams{
		OwnerUserID: string(ownerUserID),
		Name:        dmGroupName,
		Visibility:  int16(VisibilityOwner),
	}); {
	case err == nil:
		return ChannelGroupID(existing), nil
	case !noRows(err):
		return "", fmt.Errorf("store: resolve dm group: %w", err)
	}

	id := newID()
	if err := qtx.InsertOwnerDMGroup(ctx, db.InsertOwnerDMGroupParams{
		ID:          id,
		Name:        dmGroupName,
		OwnerUserID: string(ownerUserID),
		Visibility:  int16(VisibilityOwner),
	}); err != nil {
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
	// identically on create and resume so a reconcile converges on what a create
	// would have produced.
	members, err := expandOwnerMembership(ctx, tx, spec.Members[0], spec.Members[1:])
	if err != nil {
		return "", false, err
	}

	qtx := db.New(tx)
	groupID := pgtype.Text{String: string(spec.GroupID), Valid: true}
	for {
		switch existing, err := qtx.GetDMChannelByName(ctx, db.GetDMChannelByNameParams{
			GroupID: groupID,
			Name:    spec.Name,
		}); {
		case err == nil:
			// Resume: the R3 belt verifies + reconciles the resolved row before
			// adopting it, and returns ErrNotFound on a wrong-kind squat.
			if err := verifyReconcileDMTx(ctx, tx, ChannelID(existing.ID), ChannelKind(existing.Kind), members); err != nil {
				return "", false, err
			}
			return ChannelID(existing.ID), false, nil
		case !noRows(err):
			return "", false, fmt.Errorf("store: resolve dm channel: %w", err)
		}

		// Free name (as of our SELECT): INSERT born kind=DM, OPEN + mandatory.
		// ON CONFLICT DO NOTHING absorbs a row a concurrent open committed since
		// our SELECT — zero rows instead of a unique-violation — so the tx is
		// never poisoned; we loop and resume the committed row.
		id := newID()
		switch insertedID, err := qtx.InsertDMChannel(ctx, db.InsertDMChannelParams{
			ID:                    id,
			Name:                  spec.Name,
			GroupID:               groupID,
			Kind:                  int16(ChannelKindDM),
			PostPolicy:            int16(ChannelPostPolicyOpen),
			MandatorySubscription: true,
		}); {
		case err == nil:
			for _, m := range members {
				if err := qtx.EnsureChannelMember(ctx, db.EnsureChannelMemberParams{
					ChannelID: insertedID,
					AccountID: string(m),
				}); err != nil {
					return "", false, upsertMemberErr(err, m)
				}
			}
			// Born mandatory ⇒ every member is a delivery target (D1 disjunct),
			// so seed each agent member's cursor in this tx — an un-seeded target
			// is the fail-DANGEROUS D2 hazard. Self-guarding (agent-only) and
			// idempotent, so human members are a no-op.
			if err := seedChannelDeliveryCursors(ctx, tx, ChannelID(insertedID)); err != nil {
				return "", false, err
			}
			return ChannelID(insertedID), true, nil
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
	if err := db.New(tx).LockOwnerDM(ctx, pgtype.Text{String: string(ownerUserID), Valid: true}); err != nil {
		return fmt.Errorf("store: lock owner dm: %w", err)
	}
	return nil
}

// verifyReconcileDMTx rejects wrong-kind rows and repairs recoverable drift.
// Restoring mandatory delivery or a wanted member also seeds current agent
// members' cursors; the seed is agent-only and idempotent.
func verifyReconcileDMTx(ctx context.Context, tx pgx.Tx, channelID ChannelID, kind ChannelKind, wanted []AccountID) error {
	if kind != ChannelKindDM {
		return fmt.Errorf("%w: dm channel %q", ErrNotFound, channelID)
	}
	qtx := db.New(tx)
	if err := qtx.ReassertDMMandatory(ctx, string(channelID)); err != nil {
		return fmt.Errorf("store: reassert dm mandatory: %w", err)
	}
	// Seed EVERY current agent member's cursor (not only re-added ones), per
	// SetChannelPolicy's flip discipline: a FALSE→TRUE re-assert makes every
	// member a delivery target, so none may be left un-seeded (the D2 hazard).
	// Self-guarding (agent-only) and idempotent.
	if err := seedChannelDeliveryCursors(ctx, tx, channelID); err != nil {
		return err
	}
	for _, m := range wanted {
		if err := qtx.EnsureChannelMember(ctx, db.EnsureChannelMemberParams{
			ChannelID: string(channelID),
			AccountID: string(m),
		}); err != nil {
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

// isReservedGroupTx matches the system-written groups (owner DM, admin Linear routing) as their
// get-halves do, by reserved name AND VisibilityOwner; an unknown group id is not reserved.
func isReservedGroupTx(ctx context.Context, tx pgx.Tx, groupID ChannelGroupID) (bool, error) {
	switch row, err := db.New(tx).GetGroupNameVisibility(ctx, string(groupID)); {
	case err == nil:
		reservedName := row.Name == dmGroupName || row.Name == linearRoutingGroupName
		return row.TopLevel && reservedName && ChannelGroupVisibility(row.Visibility) == VisibilityOwner, nil
	case noRows(err):
		return false, nil
	default:
		return false, fmt.Errorf("store: resolve reserved group discriminator: %w", err)
	}
}
