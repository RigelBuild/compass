package store

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/RigelBuild/compass/go/internal/store/db"
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

	tx, err := s.beginTenantTx(ctx)
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
		parentVis, err := s.q.WithTx(tx).GetChannelGroupVisibility(ctx, string(g.ParentGroupID))
		if err != nil {
			return ChannelGroup{}, fmt.Errorf("store: read parent group: %w", err)
		}
		// A higher enum value is more open (OWNER=0 < SHARED=1), so the child's
		// value must not exceed the parent's.
		if int32(g.Visibility) > int32(parentVis) {
			return ChannelGroup{}, fmt.Errorf(
				"%w: group visibility %d wider than parent %d", ErrInvalidArgument, g.Visibility, parentVis)
		}
	}

	id := newID()
	if err := s.q.WithTx(tx).InsertChannelGroup(ctx, db.InsertChannelGroupParams{
		ID:          id,
		Name:        g.Name,
		Column3:     string(g.ParentGroupID),
		OwnerUserID: string(ownerUserID),
		Visibility:  int16(g.Visibility), //nolint:gosec // G115: ChannelGroupVisibility is a CHECK-constrained 0/1 enum (channel_groups.visibility), always within int16
	}); err != nil {
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
	tx, err := s.beginTenantTx(ctx)
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

	// R3 primary defense: the manual create path is server-forbidden from
	// targeting a reserved per-owner DM group. Only the OpenDM path may write
	// there (UpsertDMChannelTx), so rejecting a create here makes squatting a
	// deterministic dm--… name impossible — no in-advance existence check
	// needed. The rejection is the merged ErrNotFound (never confirms the group
	// exists, so a stranger cannot probe the reserved namespace).
	if c.GroupID != "" {
		reserved, err := isReservedDMGroupTx(ctx, tx, c.GroupID)
		if err != nil {
			return Channel{}, err
		}
		if reserved {
			return Channel{}, fmt.Errorf("%w: group %q", ErrNotFound, c.GroupID)
		}
	}

	if err := s.q.WithTx(tx).InsertChannel(ctx, db.InsertChannelParams{
		ID:                    id,
		Name:                  c.Name,
		Column3:               string(c.GroupID),
		Kind:                  int16(c.Kind), //nolint:gosec // G115: ChannelKind is a CHECK-constrained 0/1/2 enum (channels.kind), always within int16
		PostPolicy:            int16(c.Policy.PostPolicy),
		Column6:               string(c.Policy.OwnerAccountID),
		MandatorySubscription: c.Policy.MandatorySubscription,
	}); err != nil {
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
	qtx := s.q.WithTx(tx)
	for _, m := range members {
		if err := qtx.EnsureChannelMember(ctx, db.EnsureChannelMemberParams{
			ChannelID: id,
			AccountID: string(m),
		}); err != nil {
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
	// SetChannelPolicy's newly-mandatory seed. One set-based statement seeds
	// every agent member of the channel; it is self-guarding (agent-only) and
	// idempotent, so a human member is a no-op. The member INSERTs above have
	// already landed in this tx's snapshot, so the statement's channel_members
	// read sees exactly this channel's member set. A non-mandatory channel seeds
	// nothing here — its members seed at subscribe time (addOrUpdateMember), the
	// pre-substrate behavior.
	if c.Policy.MandatorySubscription {
		if err := seedChannelDeliveryCursors(ctx, tx, ChannelID(id)); err != nil {
			return Channel{}, err
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
	owners, err := db.New(tx).AgentOwnersByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("store: resolve agent owners: %w", err)
	}
	for _, o := range owners {
		add(AccountID(o))
	}
	return ordered, nil
}

// ListChannelGroups returns the channel groups visible to visibleTo (see
// groupVisiblePredicate / effectiveVisibilityCTE for the rule).
func (s *Store) ListChannelGroups(ctx context.Context, visibleTo AccountID) ([]ChannelGroup, error) {
	rows, err := s.q.ListChannelGroups(ctx, string(visibleTo))
	if err != nil {
		return nil, fmt.Errorf("store: list channel groups: %w", err)
	}
	var groups []ChannelGroup
	for _, row := range rows {
		groups = append(groups, ChannelGroup{
			ID:            ChannelGroupID(row.ID),
			Name:          row.Name,
			ParentGroupID: ChannelGroupID(row.ParentGroupID),
			OwnerUserID:   AccountID(row.OwnerUserID),
			Visibility:    ChannelGroupVisibility(row.Visibility),
		})
	}
	return groups, nil
}

// ChannelGroupVisibleTo reports whether actor may see groupID — the single-id
// form of the ListChannelGroups predicate, used by the SubscribeComms stream
// edge to filter ChannelGroupChanged at read-parity. Shares the CTEs + predicate
// with the list read so the two cannot drift.
func (s *Store) ChannelGroupVisibleTo(ctx context.Context, actor AccountID, groupID ChannelGroupID) (bool, error) {
	visible, err := s.q.ChannelGroupVisibleTo(ctx, db.ChannelGroupVisibleToParams{
		AccountID: string(actor),
		ID:        string(groupID),
	})
	if err != nil {
		return false, fmt.Errorf("store: check group visibility: %w", err)
	}
	return visible, nil
}

// ListChannels returns the channels visible to visibleTo (see
// channelVisiblePredicate / effectiveVisibilityCTE for the rule).
func (s *Store) ListChannels(ctx context.Context, visibleTo AccountID) ([]Channel, error) {
	rows, err := s.q.ListChannels(ctx, string(visibleTo))
	if err != nil {
		return nil, fmt.Errorf("store: list channels: %w", err)
	}
	var channels []Channel
	for _, row := range rows {
		channels = append(channels, channelFromRow(row.ID, row.Name, row.GroupID, row.Kind, row.PostPolicy, row.OwnerAccountID, row.MandatorySubscription))
	}
	if err := loadChannelMembers(ctx, s.scopedPool(), channels); err != nil {
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
	visible, err := s.q.ChannelVisibleTo(ctx, db.ChannelVisibleToParams{
		AccountID: string(actor),
		ID:        string(channelID),
	})
	if err != nil {
		return false, fmt.Errorf("store: check channel visibility: %w", err)
	}
	return visible, nil
}

// ChannelByNameForViewer resolves a channel NAME to its Channel within the set
// visible to viewer — the viewer-scoped name→id resolve the agent tool edge runs
// ahead of the id-typed store calls (peer-DM record R1). Channel names are not
// globally unique — "Ungrouped channels (empty group) are not name-constrained"
// and a name may recur across groups — so resolution is deliberately scoped to
// the caller's visible set (channelVisiblePredicate, shared with ListChannels so
// the resolve cannot drift from the read) and enforces uniqueness WITHIN it:
//
//   - no visible channel of that name (unknown, or real-but-invisible) →
//     ErrNotFound, the two indistinguishable so a probe cannot enumerate names it
//     lacks visibility for (the D9 not-found/forbidden merge);
//   - exactly one → that channel;
//   - two or more visible channels sharing the name → ErrInvalidArgument naming
//     the collision, so the caller disambiguates rather than the server guessing
//     (there is no ErrAmbiguous sentinel — invalid_argument is the R1 rule).
//
// Match is exact-case, mirroring the channels_group_name_key uniqueness the store
// enforces on writes.
func (s *Store) ChannelByNameForViewer(ctx context.Context, viewer AccountID, name string) (Channel, error) {
	rows, err := s.q.ChannelsByNameForViewer(ctx, db.ChannelsByNameForViewerParams{
		AccountID: string(viewer),
		Name:      name,
	})
	if err != nil {
		return Channel{}, fmt.Errorf("store: resolve channel by name: %w", err)
	}
	var channels []Channel
	for _, row := range rows {
		channels = append(channels, channelFromRow(row.ID, row.Name, row.GroupID, row.Kind, row.PostPolicy, row.OwnerAccountID, row.MandatorySubscription))
	}
	if err := loadChannelMembers(ctx, s.scopedPool(), channels); err != nil {
		return Channel{}, err
	}
	switch len(channels) {
	case 0:
		return Channel{}, fmt.Errorf("%w: channel %q", ErrNotFound, name)
	case 1:
		return channels[0], nil
	default:
		return Channel{}, fmt.Errorf("%w: channel name %q is ambiguous — it names %d visible channels; address it by id", ErrInvalidArgument, name, len(channels))
	}
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
func (s *Store) UpdateChannelMembers(ctx context.Context, actor AccountID, channelID ChannelID, updates []MemberUpdate, opts MemberUpdatesOptions) (Channel, []AccountID, error) {
	tx, err := s.beginTenantTx(ctx)
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

	// T4/R4: read the channel's mandatory_subscription flag AND kind once under
	// the tx. mandatory serves two purposes: (1) an explicit unsubscribe on a
	// mandatory channel is refused; (2) a plain add to a mandatory channel must
	// seed the new member's delivery cursor (else it mints an un-seeded delivery
	// target, the fail-DANGEROUS D2 hazard). The read is FOR UPDATE so it
	// serializes against a concurrent SetChannelPolicy mandatory flip (same
	// channels-row lock), guaranteeing a member added concurrently with a flip is
	// seeded by exactly one writer, never zero. kind drives the R4 DM guards: a
	// genuine member ADD on a kind=DM channel is a conversion, and a remove may
	// not strand a DM below two agent parties. policy/owner fields are server-set
	// and never mutated through this path.
	lock, err := s.q.WithTx(tx).LockChannelMandatoryKind(ctx, string(channelID))
	if err != nil {
		return Channel{}, nil, fmt.Errorf("store: read channel mandatory flag: %w", err)
	}
	mandatory := lock.MandatorySubscription
	kind := ChannelKind(lock.Kind)
	// The unsubscribe guard reads the PRE-convert mandatory state: a DM is
	// born-mandatory, so an unsubscribe batched with a genuine convert-add is
	// rejected here even though the post-convert channel is non-mandatory and
	// would permit it. This mid-batch ambiguity is not reachable through the RPC
	// surface (open_dm and member edits are distinct calls) and convert+unsubscribe
	// is not a real use case; evaluating against the pre-convert state is the
	// conservative choice.
	for _, u := range updates {
		if u.Unsubscribe {
			if mandatory {
				return Channel{}, nil, fmt.Errorf("%w: cannot unsubscribe from a mandatory-subscription channel", ErrInvalidArgument)
			}
			break
		}
	}

	// R4 convert-on-add: a genuine member ADD (not a remove, not an unsubscribe,
	// naming an account not already a member) on a kind=DM channel converts the
	// two-party DM into a named CHANNEL before the add path runs. maybeConvertDM
	// returns the channel's kind after any conversion (unchanged when no genuine
	// add, or the channel was never a DM) and whether it converted this call. A
	// convert clears mandatory_subscription in the DB (the result is a normal
	// opt-in channel), so the caller's `mandatory` local — read pre-convert as
	// TRUE for a born-mandatory DM — MUST be refreshed to FALSE, or the add loop
	// below would seed the opt-in third member's delivery cursor as if the channel
	// were still mandatory (a spurious seed: an unsubscribed add on a normal
	// channel owes no cursor until it subscribes — the D2 seed-at-subscribe rule).
	kind, converted, err := maybeConvertDM(ctx, tx, channelID, kind, updates, opts)
	if err != nil {
		return Channel{}, nil, err
	}
	if converted {
		mandatory = false
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
			// R4: a DM is a fixed two-party surface — a remove may not strand it
			// below two agent parties (teardown, not member surgery, ends a DM).
			if err := requireDMTwoParties(ctx, tx, channelID, kind); err != nil {
				return Channel{}, nil, err
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

// maybeConvertDM applies the R4 DM-to-CHANNEL conversion when a genuine member
// ADD targets a kind=DM channel, and returns the channel's kind afterward. A
// genuine add on a DM MUST supply opts.ConvertChannelName (else
// ErrInvalidArgument); with it, one tx converts the two-party DM into a normal
// opt-in channel before the caller's add path runs:
//
//   - kind=CHANNEL, name=ConvertChannelName, group_id=NULL — leaving the reserved
//     DM group frees the deterministic dm--a--b name so a future open_dm mints a
//     FRESH pair DM.
//   - mandatory_subscription=FALSE — the result is a genuine normal channel
//     (opt-in), not a force-subscribe surface (Matt's ruling: a converted DM is a
//     normal channel; a normal channel is opt-in). Without this the converted
//     channel would keep the DM's born-mandatory flag and NO member could ever
//     unsubscribe (the unsubscribe guard rejects it on mandatory channels).
//   - the two original DM parties are flipped subscribed=TRUE. A DM member carries
//     subscribed=FALSE (it was a delivery target only via the mandatory flag we
//     just cleared), so without this they would SILENTLY stop receiving the very
//     conversation they were mid-way through — and nothing notifies an agent it
//     lost delivery (there is no add/drop notification; delivery is the only
//     signal). Flipping the incumbents subscribed keeps them in the conversation;
//     the newly-added third member joins opt-in (subscribed per its own update),
//     the normal-channel default.
//
// When the channel is not a DM, or the batch has no genuine add (a pure
// subscribe-flip/remove adds no party), the kind is returned unchanged and no
// name is required.
func maybeConvertDM(ctx context.Context, tx pgx.Tx, channelID ChannelID, kind ChannelKind, updates []MemberUpdate, opts MemberUpdatesOptions) (ChannelKind, bool, error) {
	if kind != ChannelKindDM {
		return kind, false, nil
	}
	genuineAdd, err := hasGenuineAdd(ctx, tx, channelID, updates)
	if err != nil {
		return kind, false, err
	}
	if !genuineAdd {
		return kind, false, nil
	}
	if opts.ConvertChannelName == "" {
		return kind, false, fmt.Errorf("%w: adding a third member converts a DM to a channel and requires a channel name", ErrInvalidArgument)
	}
	// The convert sets group_id=NULL, and the channel-name unique index is
	// partial on group_id IS NOT NULL — an ungrouped channel is exempt, so this
	// UPDATE cannot raise a (group_id, name) unique violation. Ungrouped channel
	// names are deliberately not constrained (mirrors home-channel dup behavior).
	if err := db.New(tx).ConvertDMChannel(ctx, db.ConvertDMChannelParams{
		Kind: int16(ChannelKindChannel),
		Name: opts.ConvertChannelName,
		ID:   string(channelID),
	}); err != nil {
		return kind, false, fmt.Errorf("store: convert dm channel: %w", err)
	}
	// Keep the two incumbent DM parties in the conversation: flip every current
	// AGENT member subscribed (a human owner member is left as-is — subscription
	// is an agent-delivery concept). They already have a seeded delivery cursor
	// from the DM's born-mandatory create, so no seed is owed here.
	if err := db.New(tx).SubscribeConvertedDMParties(ctx, string(channelID)); err != nil {
		return kind, false, fmt.Errorf("store: subscribe converted dm parties: %w", err)
	}
	return ChannelKindChannel, true, nil
}

// hasGenuineAdd reports whether updates contain at least one genuine member ADD
// against channelID: an update that is not a remove and not an unsubscribe,
// naming an account not already a member. A subscribe-flip of an existing member
// (or a re-add of a current member) is NOT a genuine add — it adds no party — so
// it does not trigger the R4 DM conversion. The membership probe reads this tx's
// snapshot, so a member added earlier in the same batch counts as present.
func hasGenuineAdd(ctx context.Context, tx pgx.Tx, channelID ChannelID, updates []MemberUpdate) (bool, error) {
	for _, u := range updates {
		if u.Remove || u.Unsubscribe || u.AccountID == "" {
			continue
		}
		exists, err := db.New(tx).ChannelMemberExists(ctx, db.ChannelMemberExistsParams{
			ChannelID: string(channelID),
			AccountID: string(u.AccountID),
		})
		if err != nil {
			return false, fmt.Errorf("store: probe member presence: %w", err)
		}
		if !exists {
			return true, nil
		}
	}
	return false, nil
}

// requireDMTwoParties enforces the R4 two-party floor after a remove: on a
// kind=DM channel it counts the AGENT members remaining in this tx's snapshot and
// rejects the remove (ErrInvalidArgument, rolling it back) if fewer than two
// remain — a one-party DM is not a thing; teardown, not member surgery, ends a
// DM. Human owner members are excluded from the count: the DM's parties are its
// two agents; their pulled-in owners are membership bookkeeping, not parties. A
// non-DM channel has no such floor and returns nil immediately.
func requireDMTwoParties(ctx context.Context, tx pgx.Tx, channelID ChannelID, kind ChannelKind) error {
	if kind != ChannelKindDM {
		return nil
	}
	parties, err := db.New(tx).CountAgentMembers(ctx, string(channelID))
	if err != nil {
		return fmt.Errorf("store: count agent members: %w", err)
	}
	if parties < 2 {
		return fmt.Errorf("%w: a DM must keep two agent parties; convert or tear it down instead", ErrInvalidArgument)
	}
	return nil
}

// removeMember deletes one member row, preserving transitive owner-membership
// (design.md:231-234) symmetrically with creation: a user must stay while any
// agent it owns remains in the channel, so removing such an owner is rejected as
// ErrInvalidArgument rather than orphaning the agent's owner from what it can
// read. Reports whether a row was actually deleted — removing an account that
// was not a member is a no-op that owes no ChannelChanged to anyone.
func removeMember(ctx context.Context, tx pgx.Tx, channelID ChannelID, accountID AccountID) (bool, error) {
	qtx := db.New(tx)
	ownsPresentAgent, err := qtx.OwnerHasPresentAgent(ctx, db.OwnerHasPresentAgentParams{
		OwnerUserID: string(accountID),
		ChannelID:   string(channelID),
	})
	if err != nil {
		return false, fmt.Errorf("store: check dependent agents: %w", err)
	}
	if ownsPresentAgent {
		return false, fmt.Errorf("%w: cannot remove %q while an agent it owns remains in the channel", ErrInvalidArgument, accountID)
	}
	rowsAffected, err := qtx.DeleteChannelMember(ctx, db.DeleteChannelMemberParams{
		ChannelID: string(channelID),
		AccountID: string(accountID),
	})
	if err != nil {
		return false, fmt.Errorf("store: remove member: %w", err)
	}
	return rowsAffected > 0, nil
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
	qtx := db.New(tx)
	for i, m := range toAdd {
		if i == 0 {
			if err := qtx.UpsertChannelMember(ctx, db.UpsertChannelMemberParams{
				ChannelID:  string(channelID),
				AccountID:  string(m),
				Subscribed: u.Subscribed,
			}); err != nil {
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
		if err := qtx.EnsureChannelMember(ctx, db.EnsureChannelMemberParams{
			ChannelID: string(channelID),
			AccountID: string(m),
		}); err != nil {
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
	tx, err := s.beginTenantTx(ctx)
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
	lock, err := s.q.WithTx(tx).LockChannelPolicy(ctx, string(channelID))
	if err != nil {
		// Defensive/unreachable: requireChannelMember above already proved the
		// channel exists (a nonexistent channel has no members), so this
		// FOR UPDATE cannot return no-rows. Kept for symmetry with messages.go.
		if noRows(err) {
			return Channel{}, fmt.Errorf("%w: channel %q", ErrNotFound, channelID)
		}
		return Channel{}, fmt.Errorf("store: lock channel for policy: %w", err)
	}
	wasMandatory := lock.MandatorySubscription
	currentOwner := lock.OwnerAccountID

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
		ownerIsMember, err := s.q.WithTx(tx).ChannelMemberExists(ctx, db.ChannelMemberExistsParams{
			ChannelID: string(channelID),
			AccountID: string(p.OwnerAccountID),
		})
		if err != nil {
			return Channel{}, fmt.Errorf("store: check owner membership: %w", err)
		}
		if !ownerIsMember {
			return Channel{}, fmt.Errorf("%w: owner account %q must be a channel member", ErrInvalidArgument, p.OwnerAccountID)
		}
	}

	if err := s.q.WithTx(tx).UpdateChannelPolicy(ctx, db.UpdateChannelPolicyParams{
		ID:                    string(channelID),
		PostPolicy:            int16(p.PostPolicy), //nolint:gosec // G115: ChannelPostPolicy is a CHECK-constrained 0/1 enum (channels.post_policy), always within int16
		Column3:               string(p.OwnerAccountID),
		MandatorySubscription: p.MandatorySubscription,
	}); err != nil {
		if pgErrIs(err, pgForeignKeyViolation) {
			return Channel{}, fmt.Errorf("%w: unknown owner account %q", ErrInvalidArgument, p.OwnerAccountID)
		}
		return Channel{}, fmt.Errorf("store: update channel policy: %w", err)
	}

	// Newly-mandatory: every member becomes a delivery target, so seed each
	// agent member's cursor in this same txn — an un-seeded delivery target is
	// the fail-DANGEROUS D2 hazard. One set-based statement seeds every agent
	// member of the channel; it is self-guarding (agent-only) and idempotent, so
	// seeding across the whole member set is safe (a human member is a no-op).
	if p.MandatorySubscription && !wasMandatory {
		if err := seedChannelDeliveryCursors(ctx, tx, channelID); err != nil {
			return Channel{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Channel{}, fmt.Errorf("store: commit set channel policy: %w", err)
	}
	return s.getChannel(ctx, channelID)
}

// GetChannel loads one channel with its member set and policy by id, or
// ErrNotFound for an unknown id — the exported form of getChannel. It is a pool
// read (post-commit), not a tx read, and applies no caller-visibility scoping;
// the caller-facing not-found/forbidden merge is the caller's, layered on the
// membership it reads from the returned channel. Two callers rely on it: the
// comms coordination reconcile reads a just-committed coordination channel for
// its post-commit ChannelChanged emit (RIG-1722 T5), and the UpdatePinnedBoard
// handler reads the member set and post policy together to authorize a board
// mutation against the channel's policy (RIG-1723 T6).
func (s *Store) GetChannel(ctx context.Context, id ChannelID) (Channel, error) {
	return s.getChannel(ctx, id)
}

// getChannel loads one channel with its member set, or ErrNotFound.
func (s *Store) getChannel(ctx context.Context, id ChannelID) (Channel, error) {
	row, err := s.q.GetChannel(ctx, string(id))
	if err != nil {
		if noRows(err) {
			return Channel{}, fmt.Errorf("%w: channel %q", ErrNotFound, id)
		}
		return Channel{}, fmt.Errorf("store: get channel: %w", err)
	}
	channels := []Channel{channelFromRow(row.ID, row.Name, row.GroupID, row.Kind, row.PostPolicy, row.OwnerAccountID, row.MandatorySubscription)}
	if err := loadChannelMembers(ctx, s.scopedPool(), channels); err != nil {
		return Channel{}, err
	}
	return channels[0], nil
}

// scanChannels reads channel rows and populates each channel's member set with
// one follow-up query over the whole id set, so member loading is O(1)
// round-trips rather than one per channel.
func scanChannels(ctx context.Context, q db.DBTX, rows pgx.Rows) ([]Channel, error) { //nolint:unused // called by the pgtest-tagged test helper coordChannels (coordination_pgtest_test.go); the untagged lint build excludes that file and reads this as dead
	var channels []Channel
	for rows.Next() {
		var (
			id, name, groupID     string
			kind                  int16
			postPolicy            int16
			ownerAccountID        string
			mandatorySubscription bool
		)
		if err := rows.Scan(&id, &name, &groupID, &kind, &postPolicy, &ownerAccountID, &mandatorySubscription); err != nil {
			return nil, fmt.Errorf("store: scan channel: %w", err)
		}
		channels = append(channels, channelFromRow(id, name, groupID, kind, postPolicy, ownerAccountID, mandatorySubscription))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate channels: %w", err)
	}
	if err := loadChannelMembers(ctx, q, channels); err != nil {
		return nil, err
	}
	return channels, nil
}

// channelFromRow builds the base Channel (id, name, group, kind, policy) from
// the shared seven-column channel projection every channel read selects; the
// caller populates the member/subscriber sets with loadChannelMembers.
func channelFromRow(id, name, groupID string, kind, postPolicy int16, ownerAccountID string, mandatorySubscription bool) Channel {
	return Channel{
		ID:      ChannelID(id),
		Name:    name,
		GroupID: ChannelGroupID(groupID),
		Kind:    ChannelKind(kind),
		Policy: ChannelPolicy{
			PostPolicy:            ChannelPostPolicy(postPolicy),
			OwnerAccountID:        AccountID(ownerAccountID),
			MandatorySubscription: mandatorySubscription,
		},
	}
}

// loadChannelMembers populates each channel's member and subscriber sets with
// one follow-up query over the whole id set, so member loading is O(1)
// round-trips rather than one per channel. Runs against the pool or a tx (any
// db.DBTX), mirroring the former scanChannels member follow-up.
func loadChannelMembers(ctx context.Context, q db.DBTX, channels []Channel) error {
	if len(channels) == 0 {
		return nil
	}
	byID := make(map[ChannelID]int, len(channels))
	ids := make([]string, len(channels))
	for i := range channels {
		byID[channels[i].ID] = i
		ids[i] = string(channels[i].ID)
	}
	members, err := db.New(q).ChannelMembersByChannelIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("store: load channel members: %w", err)
	}
	for _, m := range members {
		idx := byID[ChannelID(m.ChannelID)]
		channels[idx].MemberAccountIDs = append(channels[idx].MemberAccountIDs, AccountID(m.AccountID))
		if m.Subscribed {
			channels[idx].SubscriberAccountIDs = append(channels[idx].SubscriberAccountIDs, AccountID(m.AccountID))
		}
	}
	return nil
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

	tx, err := s.beginTenantTx(ctx)
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
	qtx := s.q.WithTx(tx)
	if err := qtx.InsertAgentWorkspaceIgnore(ctx, db.InsertAgentWorkspaceIgnoreParams{
		ID:             id,
		AgentAccountID: string(agentAccountID),
	}); err != nil {
		if pgErrIs(err, pgForeignKeyViolation) {
			return AgentWorkspace{}, fmt.Errorf("%w: unknown agent %q", ErrInvalidArgument, agentAccountID)
		}
		return AgentWorkspace{}, fmt.Errorf("store: open workspace: %w", err)
	}

	wsID, err := qtx.GetAgentWorkspaceID(ctx, string(agentAccountID))
	if err != nil {
		return AgentWorkspace{}, fmt.Errorf("store: read workspace: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentWorkspace{}, fmt.Errorf("store: commit open workspace: %w", err)
	}
	return AgentWorkspace{ID: WorkspaceID(wsID), AgentAccountID: agentAccountID}, nil
}
