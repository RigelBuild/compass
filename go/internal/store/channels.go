package store

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// CreateChannelGroup inserts a namespace group owned by ownerUserID. When a
// parent is named, two D9 gates apply, both in one transaction with the insert:
// the actor must be authorized against the parent (own it, be an agent whose
// owning user owns it, or the parent is shared — requireGroupCreateAuthz), so a
// caller cannot nest a group under a parent it neither owns nor may see, and an
// unauthorized-or-unknown parent both return ErrNotFound (the not-found/forbidden
// merge, so a stranger cannot probe which group ids exist); and the child ≤
// parent visibility ceiling (comms.proto:149-151) — a SHARED child under an
// OWNER parent is ErrInvalidArgument. A top-level group (empty parent) is
// un-parented, so neither gate applies and it may take any visibility.
func (s *Store) CreateChannelGroup(ctx context.Context, ownerUserID AccountID, g NewChannelGroup) (ChannelGroup, error) {
	if g.Name == "" {
		return ChannelGroup{}, fmt.Errorf("%w: group name is required", ErrInvalidArgument)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ChannelGroup{}, fmt.Errorf("store: begin create group: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if g.ParentGroupID != "" {
		// D9: authorize the caller against the parent. This also subsumes the
		// existence check — an unknown parent is not authorized, so unknown and
		// unauthorized both collapse to ErrNotFound rather than leaking which
		// group ids exist across the visibility boundary.
		if err := requireGroupCreateAuthz(ctx, tx, ownerUserID, g.ParentGroupID); err != nil {
			return ChannelGroup{}, err
		}
		var parentVis int32
		if err := tx.QueryRow(ctx,
			"SELECT visibility FROM channel_groups WHERE id = $1", string(g.ParentGroupID),
		).Scan(&parentVis); err != nil {
			return ChannelGroup{}, fmt.Errorf("store: read parent group: %w", err)
		}
		// A higher enum value is more open (OWNER=0 < SHARED=1), so the child's
		// value must not exceed the parent's.
		if int32(g.Visibility) > parentVis {
			return ChannelGroup{}, fmt.Errorf(
				"%w: group visibility %d wider than parent %d", ErrInvalidArgument, g.Visibility, parentVis)
		}
	}

	id := newID()
	if _, err := tx.Exec(ctx,
		"INSERT INTO channel_groups (id, name, parent_group_id, owner_user_id, visibility) VALUES ($1, $2, NULLIF($3, ''), $4, $5)",
		id, g.Name, string(g.ParentGroupID), string(ownerUserID), int32(g.Visibility),
	); err != nil {
		return ChannelGroup{}, fmt.Errorf("store: insert channel group: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ChannelGroup{}, fmt.Errorf("store: commit create group: %w", err)
	}
	return ChannelGroup{
		ID:            ChannelGroupID(id),
		Name:          g.Name,
		ParentGroupID: g.ParentGroupID,
		OwnerUserID:   ownerUserID,
		Visibility:    g.Visibility,
	}, nil
}

// CreateChannel inserts a channel and its membership. Transitive
// owner-membership (design.md:231-234) is enforced here: the actor is always a
// member, and for each agent in the requested member set that agent's owning
// user(s) are added too, so a user can always read anything their agent is
// party to (an agent↔agent DM carries both owners). The caller-supplied member
// set is augmented, never trusted as complete. A channel name already taken in
// its group is ErrConflict; an unknown group is ErrInvalidArgument. Ungrouped
// channels (empty group) are not name-constrained.
func (s *Store) CreateChannel(ctx context.Context, actor AccountID, c NewChannel) (Channel, error) {
	if c.Name == "" {
		return Channel{}, fmt.Errorf("%w: channel name is required", ErrInvalidArgument)
	}

	// Coherence: OWNER_ONLY with no owner account bricks the channel — the post
	// gate's COALESCE('') rejects EVERY author (unpostable). Reject at birth,
	// mirroring SetChannelPolicy's guard. (0013 comment: owner-empty is the only
	// legal state when OPEN.)
	if c.Policy.PostPolicy == ChannelPostPolicyOwnerOnly && c.Policy.OwnerAccountID == "" {
		return Channel{}, fmt.Errorf("%w: OWNER_ONLY requires an owner account", ErrInvalidArgument)
	}

	// Coherence: OPEN admits every member as an author, so an owner account is
	// meaningless there — and a non-empty owner on an OPEN channel would let a
	// member silently claim the operator slot (locking future policy changes to
	// itself). owner-empty is the only legal state when OPEN, so reject a
	// non-empty owner outright.
	if c.Policy.PostPolicy == ChannelPostPolicyOpen && c.Policy.OwnerAccountID != "" {
		return Channel{}, fmt.Errorf("%w: OPEN channel must not name an owner account", ErrInvalidArgument)
	}

	id := newID()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Channel{}, fmt.Errorf("store: begin create channel: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// D9 write-authz: a channel created inside a group is authorized against
	// that parent group (design.md:362-367) — the actor must own the group or
	// the group must be visible to them (VisibilityShared). An unknown group is
	// ErrNotFound (the not-found/forbidden merge), so a non-owner cannot probe
	// group ids. An ungrouped channel (DM/GROUP_DM, or a top-level channel) has
	// no parent group to authorize against; the actor is a founding member by
	// construction (expandOwnerMembership adds them), so no gate applies.
	if c.GroupID != "" {
		if err := requireGroupCreateAuthz(ctx, tx, actor, c.GroupID); err != nil {
			return Channel{}, err
		}
	}

	if _, err := tx.Exec(ctx,
		"INSERT INTO channels (id, name, group_id, kind, post_policy, owner_account_id, mandatory_subscription) "+
			"VALUES ($1, $2, NULLIF($3, ''), $4, $5, NULLIF($6, ''), $7)",
		id, c.Name, string(c.GroupID), int32(c.Kind),
		int32(c.Policy.PostPolicy), string(c.Policy.OwnerAccountID), c.Policy.MandatorySubscription,
	); err != nil {
		if pgErrIs(err, pgUniqueViolation) {
			return Channel{}, fmt.Errorf("%w: channel %q already exists in group %q", ErrConflict, c.Name, c.GroupID)
		}
		if pgErrIs(err, pgForeignKeyViolation) {
			return Channel{}, fmt.Errorf("%w: unknown group %q", ErrInvalidArgument, c.GroupID)
		}
		return Channel{}, fmt.Errorf("store: insert channel: %w", err)
	}

	members, err := expandOwnerMembership(ctx, tx, actor, c.MemberAccountIDs)
	if err != nil {
		return Channel{}, err
	}
	// Coherence facet 1: an OWNER_ONLY channel whose owner is not itself a member
	// is unpostable from birth — the post gate demands the author be BOTH a member
	// AND the owner, so a non-member owner fails its own membership gate and no
	// account can ever post. The owner MUST be among the channel's members. The
	// expansion above is the authoritative final member set (actor + requested +
	// transitive owners), so check the resolved owner against it before the insert.
	if c.Policy.OwnerAccountID != "" && !slices.Contains(members, c.Policy.OwnerAccountID) {
		return Channel{}, fmt.Errorf("%w: owner account %q must be a channel member", ErrInvalidArgument, c.Policy.OwnerAccountID)
	}
	for _, m := range members {
		if _, err := tx.Exec(ctx,
			"INSERT INTO channel_members (channel_id, account_id, subscribed) VALUES ($1, $2, FALSE) "+
				"ON CONFLICT (channel_id, account_id) DO NOTHING",
			id, string(m),
		); err != nil {
			if pgErrIs(err, pgForeignKeyViolation) {
				return Channel{}, fmt.Errorf("%w: unknown member account %q", ErrInvalidArgument, m)
			}
			return Channel{}, fmt.Errorf("store: insert channel member: %w", err)
		}
	}
	// A channel born mandatory_subscription=true makes every member a delivery
	// target via the D1 disjunct regardless of the subscribed flag, so each
	// agent member's delivery cursor MUST be seeded in this same tx — an
	// un-seeded delivery target is the fail-DANGEROUS D2 hazard
	// (compass-notification-delivery/design.md:293-311). Symmetric with
	// SetChannelPolicy's newly-mandatory seed. seedDeliveryCursor is
	// self-guarding (agent-only) and idempotent, so seeding every member is safe
	// (a human member is a no-op). A non-mandatory channel seeds nothing here —
	// its members seed at subscribe time (addOrUpdateMember), the pre-substrate
	// behavior.
	if c.Policy.MandatorySubscription {
		for _, m := range members {
			if err := seedDeliveryCursor(ctx, tx, m, ChannelID(id)); err != nil {
				return Channel{}, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Channel{}, fmt.Errorf("store: commit create channel: %w", err)
	}

	return Channel{
		ID:               ChannelID(id),
		Name:             c.Name,
		GroupID:          c.GroupID,
		Kind:             c.Kind,
		MemberAccountIDs: members,
		Policy:           c.Policy,
	}, nil
}

// expandOwnerMembership computes the final member set for a new channel: the
// requested members, plus the actor, plus the owning user of every agent in the
// set — the transitive owner-membership invariant (design.md:231-234),
// deduplicated in stable order (actor first, then requested order).
func expandOwnerMembership(ctx context.Context, tx pgx.Tx, actor AccountID, requested []AccountID) ([]AccountID, error) {
	seen := make(map[AccountID]bool)
	ordered := make([]AccountID, 0, len(requested)+1)
	add := func(id AccountID) {
		if id != "" && !seen[id] {
			seen[id] = true
			ordered = append(ordered, id)
		}
	}
	add(actor)
	for _, m := range requested {
		add(m)
	}

	// For every agent already in the set, pull its owner and add it. One query
	// over the current set keeps this O(1) round-trips regardless of set size.
	ids := make([]string, 0, len(ordered))
	for _, m := range ordered {
		ids = append(ids, string(m))
	}
	rows, err := tx.Query(ctx,
		"SELECT owner_user_id FROM agent_accounts WHERE account_id = ANY($1)", ids,
	)
	if err != nil {
		return nil, fmt.Errorf("store: resolve agent owners: %w", err)
	}
	defer rows.Close()

	var owners []AccountID
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, fmt.Errorf("store: scan agent owner: %w", err)
		}
		owners = append(owners, AccountID(owner))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate agent owners: %w", err)
	}
	for _, o := range owners {
		add(o)
	}
	return ordered, nil
}

// effectiveVisibilityCTE computes each channel group's effective visibility —
// the most restrictive value on its path to the root (D9) — by walking the
// parent chain. Every read and stream-filter predicate that gates on group
// visibility opens with this identical CTE so they compute `effective(id,
// eff_vis)` the same way and cannot drift. eff_vis = 1 is SHARED.
const effectiveVisibilityCTE = `
	WITH RECURSIVE ancestry AS (
		SELECT id, parent_group_id, visibility AS min_vis
		FROM channel_groups
		UNION ALL
		SELECT a.id, g.parent_group_id, LEAST(a.min_vis, g.visibility)
		FROM ancestry a
		JOIN channel_groups g ON g.id = a.parent_group_id
	),
	effective AS (
		SELECT id, MIN(min_vis) AS eff_vis
		FROM ancestry
		GROUP BY id
	)`

// viewerCTE resolves the set of user ids a caller ($1) views as: itself, plus —
// for an agent caller — its owning user, so an agent sees its owner's groups.
// Appended after effectiveVisibilityCTE (hence the leading comma). Referenced by
// the group predicate's owner check.
const viewerCTE = `,
	viewer AS (
		SELECT owner_user_id AS uid FROM agent_accounts WHERE account_id = $1
		UNION ALL
		SELECT $1 AS uid
	)`

// channelVisiblePredicate is the ListChannels visibility rule as a reusable
// boolean over a channel row aliased `c` and the `effective` CTE, viewer $1: the
// caller is a member (which governs DM/GROUP_DM directly, design.md:235-243), or
// it is a plain grouped channel (kind=0) whose group's effective visibility is
// SHARED. An ungrouped channel is owner-scoped, visible only through membership.
// Shared by ListChannels and ChannelVisibleTo so the stream edge cannot drift
// from the read.
const channelVisiblePredicate = `(
		EXISTS (
		    SELECT 1 FROM channel_members cm
		    WHERE cm.channel_id = c.id AND cm.account_id = $1
		)
		OR (
		    c.kind = 0 AND c.group_id IS NOT NULL AND EXISTS (
		        SELECT 1 FROM effective e WHERE e.id = c.group_id AND e.eff_vis = 1
		    )
		)
	)`

// groupVisiblePredicate is the ListChannelGroups visibility rule as a reusable
// boolean over a group row aliased `g`, the `effective` CTE and `viewer` CTE: the
// group's effective visibility is SHARED, or the caller (or its owning user)
// owns it. Shared by ListChannelGroups and ChannelGroupVisibleTo.
const groupVisiblePredicate = `(e.eff_vis = 1 OR g.owner_user_id IN (SELECT uid FROM viewer))`

// ListChannelGroups returns the channel groups visible to visibleTo (see
// groupVisiblePredicate / effectiveVisibilityCTE for the rule).
func (s *Store) ListChannelGroups(ctx context.Context, visibleTo AccountID) ([]ChannelGroup, error) {
	const q = effectiveVisibilityCTE + viewerCTE + `
		SELECT g.id, g.name, COALESCE(g.parent_group_id, ''), g.owner_user_id, g.visibility
		FROM channel_groups g
		JOIN effective e ON e.id = g.id
		WHERE ` + groupVisiblePredicate + `
		ORDER BY g.name`
	rows, err := s.pool.Query(ctx, q, string(visibleTo))
	if err != nil {
		return nil, fmt.Errorf("store: list channel groups: %w", err)
	}
	defer rows.Close()

	var groups []ChannelGroup
	for rows.Next() {
		var (
			g                       ChannelGroup
			id, name, parent, owner string
			visibility              int32
		)
		if err := rows.Scan(&id, &name, &parent, &owner, &visibility); err != nil {
			return nil, fmt.Errorf("store: scan channel group: %w", err)
		}
		g.ID = ChannelGroupID(id)
		g.Name = name
		g.ParentGroupID = ChannelGroupID(parent)
		g.OwnerUserID = AccountID(owner)
		g.Visibility = ChannelGroupVisibility(visibility)
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate channel groups: %w", err)
	}
	return groups, nil
}

// ChannelGroupVisibleTo reports whether actor may see groupID — the single-id
// form of the ListChannelGroups predicate, used by the SubscribeComms stream
// edge to filter ChannelGroupChanged at read-parity. Shares the CTEs + predicate
// with the list read so the two cannot drift.
func (s *Store) ChannelGroupVisibleTo(ctx context.Context, actor AccountID, groupID ChannelGroupID) (bool, error) {
	const q = effectiveVisibilityCTE + viewerCTE + `
		SELECT EXISTS (
			SELECT 1 FROM channel_groups g
			JOIN effective e ON e.id = g.id
			WHERE g.id = $2 AND ` + groupVisiblePredicate + `
		)`
	var visible bool
	if err := s.pool.QueryRow(ctx, q, string(actor), string(groupID)).Scan(&visible); err != nil {
		return false, fmt.Errorf("store: check group visibility: %w", err)
	}
	return visible, nil
}

// ListChannels returns the channels visible to visibleTo (see
// channelVisiblePredicate / effectiveVisibilityCTE for the rule).
func (s *Store) ListChannels(ctx context.Context, visibleTo AccountID) ([]Channel, error) {
	const q = effectiveVisibilityCTE + `
		SELECT c.id, c.name, COALESCE(c.group_id, ''), c.kind, c.post_policy, COALESCE(c.owner_account_id, ''), c.mandatory_subscription
		FROM channels c
		WHERE ` + channelVisiblePredicate + `
		ORDER BY c.name`
	rows, err := s.pool.Query(ctx, q, string(visibleTo))
	if err != nil {
		return nil, fmt.Errorf("store: list channels: %w", err)
	}
	defer rows.Close()

	channels, err := scanChannels(ctx, s.pool, rows)
	if err != nil {
		return nil, err
	}
	return channels, nil
}

// ChannelVisibleTo reports whether actor may see channelID — the single-id form
// of the ListChannels predicate, used by the SubscribeComms stream edge to
// filter ChannelChanged so a SHARED-grouped channel's change still reaches a
// non-member viewer (which bare membership would wrongly drop) while a private
// channel's does not. Shares the CTE + predicate with the list read.
func (s *Store) ChannelVisibleTo(ctx context.Context, actor AccountID, channelID ChannelID) (bool, error) {
	const q = effectiveVisibilityCTE + `
		SELECT EXISTS (
			SELECT 1 FROM channels c
			WHERE c.id = $2 AND ` + channelVisiblePredicate + `
		)`
	var visible bool
	if err := s.pool.QueryRow(ctx, q, string(actor), string(channelID)).Scan(&visible); err != nil {
		return false, fmt.Errorf("store: check channel visibility: %w", err)
	}
	return visible, nil
}

// UpdateChannelMembers applies a set of add/remove/subscribe mutations to a
// channel (RT-1: the single membership-mutation carrier). Adds and
// subscribe-flips upsert a member row; removes delete it. An add of an agent
// also adds that agent's owning user(s), preserving transitive owner-membership
// on join, and rejecting a removal that would strand an owner whose agent stays
// (checked per update against the rows already mutated in this transaction, so a
// batch that removes both an owner and its agent must order the agent first).
// Returns the channel with its updated member set, plus the accounts a removal
// actually deleted (a remove of a non-member deletes nothing and owes no event)
// so the stream can deliver each departed member its one final ChannelChanged.
// D9 write-authz is enforced here in the store: the actor must be a member of
// the channel to mutate it, so an unknown channel and a non-member both return
// ErrNotFound (the not-found/forbidden merge).
func (s *Store) UpdateChannelMembers(ctx context.Context, actor AccountID, channelID ChannelID, updates []MemberUpdate) (Channel, []AccountID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Channel{}, nil, fmt.Errorf("store: begin update members: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// D9 write-authz: the actor must already be a member of the channel to
	// mutate its membership (any current member may add/remove/subscribe —
	// design.md:1782). This subsumes the existence check: a nonexistent channel
	// has no members, so a non-member and an unknown channel both collapse to
	// ErrNotFound (the not-found/forbidden merge), never leaking that a private
	// channel exists.
	if err := requireChannelMember(ctx, tx, actor, channelID); err != nil {
		return Channel{}, nil, err
	}

	// T4: read the channel's mandatory_subscription flag once under the tx. It
	// serves two purposes: (1) an explicit unsubscribe on a mandatory channel is
	// refused with InvalidArgument (membership implies a non-togglable
	// subscription there); (2) a plain add to a mandatory channel must seed the
	// new member's delivery cursor (below), because a mandatory channel makes
	// every member a delivery target regardless of the subscribed flag. The read
	// is FOR UPDATE so it serializes against a concurrent SetChannelPolicy
	// mandatory flip, which takes the same channels-row lock before seeding all
	// current members. Without the lock a member added concurrently with a flip
	// can be dropped by both writers — B's seed-all runs before A inserts M, and
	// A reads the stale mandatory=false and skips M — leaving M an unseeded
	// member of a mandatory channel (the absent cursor coalesces to live head,
	// so M is permanently caught-up and silently receives nothing). The lock
	// guarantees the seed-presence invariant: whichever writer commits first is
	// observed by the second, so M is seeded by exactly one of them, never zero.
	// owner_account_id/policy fields are server-set and never mutated through
	// this path — UpdateChannelMembers only ever touches membership rows.
	var mandatory bool
	if err := tx.QueryRow(ctx,
		"SELECT mandatory_subscription FROM channels WHERE id = $1 FOR UPDATE", string(channelID),
	).Scan(&mandatory); err != nil {
		return Channel{}, nil, fmt.Errorf("store: read channel mandatory flag: %w", err)
	}
	for _, u := range updates {
		if u.Unsubscribe {
			if mandatory {
				return Channel{}, nil, fmt.Errorf("%w: cannot unsubscribe from a mandatory-subscription channel", ErrInvalidArgument)
			}
			break
		}
	}

	var removed []AccountID
	for _, u := range updates {
		if u.AccountID == "" {
			return Channel{}, nil, fmt.Errorf("%w: member update missing account id", ErrInvalidArgument)
		}
		if u.Remove {
			deleted, err := removeMember(ctx, tx, channelID, u.AccountID)
			if err != nil {
				return Channel{}, nil, err
			}
			if deleted {
				removed = append(removed, u.AccountID)
			}
			continue
		}
		if err := addOrUpdateMember(ctx, tx, channelID, u, mandatory); err != nil {
			return Channel{}, nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Channel{}, nil, fmt.Errorf("store: commit update members: %w", err)
	}

	ch, err := s.getChannel(ctx, channelID)
	if err != nil {
		return Channel{}, nil, err
	}
	return ch, removed, nil
}

// removeMember deletes one member row, preserving transitive owner-membership
// (design.md:231-234) symmetrically with creation: a user must stay while any
// agent it owns remains in the channel, so removing such an owner is rejected as
// ErrInvalidArgument rather than orphaning the agent's owner from what it can
// read. Reports whether a row was actually deleted — removing an account that
// was not a member is a no-op that owes no ChannelChanged to anyone.
func removeMember(ctx context.Context, tx pgx.Tx, channelID ChannelID, accountID AccountID) (bool, error) {
	var ownsPresentAgent bool
	if err := tx.QueryRow(ctx,
		"SELECT EXISTS ("+
			"SELECT 1 FROM agent_accounts aa "+
			"JOIN channel_members cm ON cm.account_id = aa.account_id "+
			"WHERE aa.owner_user_id = $1 AND cm.channel_id = $2 AND aa.account_id <> $1)",
		string(accountID), string(channelID),
	).Scan(&ownsPresentAgent); err != nil {
		return false, fmt.Errorf("store: check dependent agents: %w", err)
	}
	if ownsPresentAgent {
		return false, fmt.Errorf("%w: cannot remove %q while an agent it owns remains in the channel", ErrInvalidArgument, accountID)
	}
	tag, err := tx.Exec(ctx,
		"DELETE FROM channel_members WHERE channel_id = $1 AND account_id = $2",
		string(channelID), string(accountID),
	)
	if err != nil {
		return false, fmt.Errorf("store: remove member: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// addOrUpdateMember adds (or subscribe-flips) the directly-named member, then
// pulls in the owning user(s) of an added agent so a join preserves transitive
// owner-membership. The directly-added member (index 0 of the expansion) carries
// the requested subscribed flag and may flip an existing row; pulled-in owner
// rows are additive-only (DO NOTHING) so adding an agent never clobbers an
// owner's existing subscription.
//
// mandatory carries the channel's mandatory_subscription flag: on a mandatory
// channel EVERY member is a delivery target regardless of its subscribed flag
// (the D1 read-side disjunct), so a plain (unsubscribed) add there must STILL
// seed the member's delivery cursor — else the add mints an un-seeded delivery
// target that the absent-cursor fail-safe treats as permanently caught-up,
// silently never delivering (the fail-DANGEROUS D2 hazard).
func addOrUpdateMember(ctx context.Context, tx pgx.Tx, channelID ChannelID, u MemberUpdate, mandatory bool) error {
	toAdd, err := expandOwnerMembership(ctx, tx, u.AccountID, nil)
	if err != nil {
		return err
	}
	for i, m := range toAdd {
		if i == 0 {
			if _, err := tx.Exec(ctx,
				"INSERT INTO channel_members (channel_id, account_id, subscribed) VALUES ($1, $2, $3) "+
					"ON CONFLICT (channel_id, account_id) DO UPDATE SET subscribed = EXCLUDED.subscribed",
				string(channelID), string(m), u.Subscribed,
			); err != nil {
				return upsertMemberErr(err, m)
			}
			// Seed this member's delivery cursor in the SAME txn as the member
			// insert when it is subscribed (D2 seed-at-subscribe) OR the channel
			// is mandatory (every member is a delivery target regardless of the
			// subscribed flag). The seed is self-guarding (agent-only via WHERE
			// EXISTS), so a user member is a silent no-op — no separate kind
			// lookup. Pulled-in owner rows (index > 0, DO NOTHING) are not seeded.
			if u.Subscribed || mandatory {
				if err := seedDeliveryCursor(ctx, tx, m, channelID); err != nil {
					return err
				}
			}
			continue
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO channel_members (channel_id, account_id, subscribed) VALUES ($1, $2, FALSE) "+
				"ON CONFLICT (channel_id, account_id) DO NOTHING",
			string(channelID), string(m),
		); err != nil {
			return upsertMemberErr(err, m)
		}
	}
	return nil
}

// upsertMemberErr maps a member-insert failure, translating an FK violation
// (an account id that names no account) to ErrInvalidArgument.
func upsertMemberErr(err error, m AccountID) error {
	if pgErrIs(err, pgForeignKeyViolation) {
		return fmt.Errorf("%w: unknown member account %q", ErrInvalidArgument, m)
	}
	return fmt.Errorf("store: upsert member: %w", err)
}

// SetChannelPolicy applies a channel-policy update (T4, the ONLY mutation path
// for post_policy/owner_account_id/mandatory_subscription after creation) and
// returns the updated channel. The actor must be a member of the channel
// (D9 write-authz, mirroring UpdateChannelMembers): an unknown channel and a
// non-member both collapse to ErrNotFound (the not-found/forbidden merge). All
// of the following run in ONE transaction so a mandatory flip and its cursor
// seeds commit atomically:
//
//   - The policy row update.
//   - When the update NEWLY sets mandatory_subscription=true (was false, now
//     true), the D2 delivery cursor is seeded (seed-to-head, no replay) for
//     EVERY agent member — because a mandatory channel makes every member a
//     delivery target regardless of its channel_members.subscribed flag (the D1
//     read-side disjunct). An un-seeded delivery target is the fail-DANGEROUS
//     hazard D2 names (compass-notification-delivery/design.md:293-311: seeds
//     are transactional with the membership row), so the seed rides the same
//     commit as the flag flip. seedDeliveryCursor is self-guarding (agent-only
//     WHERE EXISTS) and idempotent (ON CONFLICT DO NOTHING), so an
//     already-subscribed member whose cursor exists is a no-op and a human
//     member yields no row.
func (s *Store) SetChannelPolicy(ctx context.Context, actor AccountID, channelID ChannelID, p ChannelPolicy) (Channel, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Channel{}, fmt.Errorf("store: begin set channel policy: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // deferred cleanup; the Commit below is the real outcome.

	// D9 write-authz: the actor must be a member of the channel to set its
	// policy. This subsumes the existence check — an unknown channel has no
	// members — so a non-member and an unknown channel both collapse to
	// ErrNotFound, never leaking that a private channel exists.
	if err := requireChannelMember(ctx, tx, actor, channelID); err != nil {
		return Channel{}, err
	}

	// Read the pre-update mandatory flag AND the current owner under the row
	// lock so both the newly-mandatory transition and the owner-authz gate are
	// computed against the committed state, serialized against a concurrent
	// policy change on the same channel.
	var (
		wasMandatory bool
		currentOwner string
	)
	if err := tx.QueryRow(ctx,
		"SELECT mandatory_subscription, COALESCE(owner_account_id, '') FROM channels WHERE id = $1 FOR UPDATE",
		string(channelID),
	).Scan(&wasMandatory, &currentOwner); err != nil {
		// Defensive/unreachable: requireChannelMember above already proved the
		// channel exists (a nonexistent channel has no members), so this
		// FOR UPDATE cannot return no-rows. Kept for symmetry with messages.go.
		if noRows(err) {
			return Channel{}, fmt.Errorf("%w: channel %q", ErrNotFound, channelID)
		}
		return Channel{}, fmt.Errorf("store: lock channel for policy: %w", err)
	}

	// T4 owner-only policy gate. SetChannelPolicy is create-or-update of policy:
	// an ownerless channel (empty owner, the only legal state when OPEN) has no
	// owner to be yet, so any member may establish the first owner/policy. Once
	// an owner EXISTS, only that owner may change policy or reassign ownership —
	// a non-owner (including a plain member) is refused with the SAME ErrNotFound
	// the non-member path returns (the not-found/forbidden merge, mirroring
	// PostMessage's OWNER_ONLY gate), so the policy leaks no oracle. Because a
	// non-owner can never reach the UPDATE, a member cannot reassign ownership to
	// itself and bypass the OWNER_ONLY post-gate (privilege escalation).
	if currentOwner != "" && string(actor) != currentOwner {
		return Channel{}, fmt.Errorf("%w: channel %q", ErrNotFound, channelID)
	}

	// Coherence: OWNER_ONLY with no owner account bricks the channel — NULLIF
	// yields a NULL owner, so the post gate's COALESCE('') rejects EVERY author
	// (unpostable, no diagnostic). Reject before the write. (0013 comment:
	// owner-empty is the only legal state when OPEN.)
	if p.PostPolicy == ChannelPostPolicyOwnerOnly && p.OwnerAccountID == "" {
		return Channel{}, fmt.Errorf("%w: OWNER_ONLY requires an owner account", ErrInvalidArgument)
	}

	// Coherence facet 2: owner-empty is the only legal state when OPEN — OPEN
	// admits every member as an author, so an owner account is meaningless there,
	// and a non-empty owner would let a member silently claim the operator slot
	// (locking future policy changes to itself). Reject a non-empty owner on OPEN.
	// This is InvalidArgument and MUST stay after the no-oracle owner gate above:
	// a non-owner already collapsed to ErrNotFound and never reaches here, so no
	// InvalidArgument signal leaks channel existence to an unauthorized caller.
	if p.PostPolicy == ChannelPostPolicyOpen && p.OwnerAccountID != "" {
		return Channel{}, fmt.Errorf("%w: OPEN channel must not name an owner account", ErrInvalidArgument)
	}

	// Coherence facet 1: the owner MUST be a member of the channel. An OWNER_ONLY
	// channel whose owner is a non-member is unpostable — the post gate demands
	// the author be BOTH a member AND the owner, so a non-member owner fails its
	// own membership gate and no account can ever post. Reject before the write.
	// Only the authorized actor (establishing on an ownerless channel, or the
	// existing owner) reaches this after the owner gate, so the membership EXISTS
	// reveals nothing an authorized caller should not already know.
	if p.OwnerAccountID != "" {
		var ownerIsMember bool
		if err := tx.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM channel_members WHERE channel_id = $1 AND account_id = $2)",
			string(channelID), string(p.OwnerAccountID),
		).Scan(&ownerIsMember); err != nil {
			return Channel{}, fmt.Errorf("store: check owner membership: %w", err)
		}
		if !ownerIsMember {
			return Channel{}, fmt.Errorf("%w: owner account %q must be a channel member", ErrInvalidArgument, p.OwnerAccountID)
		}
	}

	if _, err := tx.Exec(ctx,
		"UPDATE channels SET post_policy = $2, owner_account_id = NULLIF($3, ''), mandatory_subscription = $4 WHERE id = $1",
		string(channelID), int32(p.PostPolicy), string(p.OwnerAccountID), p.MandatorySubscription,
	); err != nil {
		if pgErrIs(err, pgForeignKeyViolation) {
			return Channel{}, fmt.Errorf("%w: unknown owner account %q", ErrInvalidArgument, p.OwnerAccountID)
		}
		return Channel{}, fmt.Errorf("store: update channel policy: %w", err)
	}

	// Newly-mandatory: every member becomes a delivery target, so seed each
	// agent member's cursor in this same txn. seedDeliveryCursor is self-guarding
	// (agent-only) and idempotent, so seeding every member is safe.
	if p.MandatorySubscription && !wasMandatory {
		rows, err := tx.Query(ctx,
			"SELECT account_id FROM channel_members WHERE channel_id = $1", string(channelID))
		if err != nil {
			return Channel{}, fmt.Errorf("store: list channel members for seed: %w", err)
		}
		var members []AccountID
		for rows.Next() {
			var m string
			if err := rows.Scan(&m); err != nil {
				rows.Close()
				return Channel{}, fmt.Errorf("store: scan member for seed: %w", err)
			}
			members = append(members, AccountID(m))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return Channel{}, fmt.Errorf("store: iterate members for seed: %w", err)
		}
		rows.Close()
		for _, m := range members {
			if err := seedDeliveryCursor(ctx, tx, m, channelID); err != nil {
				return Channel{}, err
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Channel{}, fmt.Errorf("store: commit set channel policy: %w", err)
	}
	return s.getChannel(ctx, channelID)
}

// getChannel loads one channel with its member set, or ErrNotFound.
func (s *Store) getChannel(ctx context.Context, id ChannelID) (Channel, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT id, name, COALESCE(group_id, ''), kind, post_policy, COALESCE(owner_account_id, ''), mandatory_subscription FROM channels WHERE id = $1", string(id))
	if err != nil {
		return Channel{}, fmt.Errorf("store: get channel: %w", err)
	}
	defer rows.Close()
	channels, err := scanChannels(ctx, s.pool, rows)
	if err != nil {
		return Channel{}, err
	}
	if len(channels) == 0 {
		return Channel{}, fmt.Errorf("%w: channel %q", ErrNotFound, id)
	}
	return channels[0], nil
}

// scanChannels reads channel rows and populates each channel's member set with
// one follow-up query over the whole id set, so member loading is O(1)
// round-trips rather than one per channel.
func scanChannels(ctx context.Context, q querier, rows pgx.Rows) ([]Channel, error) {
	var (
		channels []Channel
		ids      []string
	)
	byID := make(map[ChannelID]int)
	for rows.Next() {
		var (
			c                     Channel
			id, name, groupID     string
			kind                  int32
			postPolicy            int32
			ownerAccountID        string
			mandatorySubscription bool
		)
		if err := rows.Scan(&id, &name, &groupID, &kind, &postPolicy, &ownerAccountID, &mandatorySubscription); err != nil {
			return nil, fmt.Errorf("store: scan channel: %w", err)
		}
		c.ID = ChannelID(id)
		c.Name = name
		c.GroupID = ChannelGroupID(groupID)
		c.Kind = ChannelKind(kind)
		c.Policy = ChannelPolicy{
			PostPolicy:            ChannelPostPolicy(postPolicy),
			OwnerAccountID:        AccountID(ownerAccountID),
			MandatorySubscription: mandatorySubscription,
		}
		byID[c.ID] = len(channels)
		channels = append(channels, c)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate channels: %w", err)
	}
	if len(channels) == 0 {
		return channels, nil
	}

	memRows, err := q.Query(ctx,
		"SELECT channel_id, account_id, subscribed FROM channel_members WHERE channel_id = ANY($1) ORDER BY account_id",
		ids,
	)
	if err != nil {
		return nil, fmt.Errorf("store: load channel members: %w", err)
	}
	defer memRows.Close()
	for memRows.Next() {
		var (
			channelID, accountID string
			subscribed           bool
		)
		if err := memRows.Scan(&channelID, &accountID, &subscribed); err != nil {
			return nil, fmt.Errorf("store: scan channel member: %w", err)
		}
		idx := byID[ChannelID(channelID)]
		channels[idx].MemberAccountIDs = append(channels[idx].MemberAccountIDs, AccountID(accountID))
		if subscribed {
			channels[idx].SubscriberAccountIDs = append(channels[idx].SubscriberAccountIDs, AccountID(accountID))
		}
	}
	if err := memRows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate channel members: %w", err)
	}
	return channels, nil
}

// OpenAgentWorkspace returns the agent's observation-pane workspace, creating it
// on first open and returning the existing one after (idempotent,
// comms.proto:62-64). Access is a projection of the agent's home-channel
// membership (fork f): the actor must be a member of the agent's
// home_channel_id, enforced here in the store. A non-member — or an unknown
// agent — is ErrNotFound (the not-found/forbidden merge), never a hint the
// agent exists.
func (s *Store) OpenAgentWorkspace(ctx context.Context, actor AccountID, agentAccountID AccountID) (AgentWorkspace, error) {
	if agentAccountID == "" {
		return AgentWorkspace{}, fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentWorkspace{}, fmt.Errorf("store: begin open workspace: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// D9 write-authz: the actor must be a member of the agent's home channel
	// (the same projection the stream edge filters AgentWorkspaceChanged on). An
	// unknown agent or a member gap both collapse to ErrNotFound. Checked in the
	// same tx as the insert-or-return so a membership revoked mid-open cannot
	// race the gate.
	authorized, err := isAgentWorkspaceVisible(ctx, tx, actor, agentAccountID)
	if err != nil {
		return AgentWorkspace{}, err
	}
	if !authorized {
		return AgentWorkspace{}, fmt.Errorf("%w: agent %q", ErrNotFound, agentAccountID)
	}

	id := newID()
	// Insert-or-return: create the workspace on first open, else return the
	// existing row. ON CONFLICT DO NOTHING then a read covers the concurrent
	// case without a unique-violation surfacing to the caller.
	if _, err := tx.Exec(ctx,
		"INSERT INTO agent_workspaces (id, agent_account_id) VALUES ($1, $2) "+
			"ON CONFLICT (agent_account_id) DO NOTHING",
		id, string(agentAccountID),
	); err != nil {
		if pgErrIs(err, pgForeignKeyViolation) {
			return AgentWorkspace{}, fmt.Errorf("%w: unknown agent %q", ErrInvalidArgument, agentAccountID)
		}
		return AgentWorkspace{}, fmt.Errorf("store: open workspace: %w", err)
	}

	var wsID string
	if err := tx.QueryRow(ctx,
		"SELECT id FROM agent_workspaces WHERE agent_account_id = $1", string(agentAccountID),
	).Scan(&wsID); err != nil {
		return AgentWorkspace{}, fmt.Errorf("store: read workspace: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentWorkspace{}, fmt.Errorf("store: commit open workspace: %w", err)
	}
	return AgentWorkspace{ID: WorkspaceID(wsID), AgentAccountID: agentAccountID}, nil
}
