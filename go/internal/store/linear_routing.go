package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// linearRoutingGroupName is the admin's reserved group for the Linear routing channel.
// CreateChannel refuses it, so only EnsureLinearRoutingChannel writes a channel there.
const linearRoutingGroupName = "__linear__"

// LinearRoutingChannelName names the channel where a Linear session no Manager owns
// lands for the root supervisor to triage.
const LinearRoutingChannelName = "linear-routing"

// EnsureLinearRoutingChannel get-or-creates the admin's routing channel under a per-admin
// advisory lock and reconciles it on both paths: mandatory, members present, cursors seeded.
func (s *Store) EnsureLinearRoutingChannel(ctx context.Context, adminID, supervisorID, bridgeID AccountID) (ChannelID, error) {
	if adminID == "" || supervisorID == "" || bridgeID == "" {
		return "", fmt.Errorf("%w: admin, supervisor and bridge account ids are required", ErrInvalidArgument)
	}
	var id ChannelID
	if err := s.WithTx(ctx, func(tx pgx.Tx) error {
		qtx := db.New(tx)
		// The group get-then-insert has no unique key to lean on, so the lock is what
		// keeps racing seeds from minting two groups (and two channels).
		if err := qtx.LockLinearRouting(ctx, pgtype.Text{String: string(adminID), Valid: true}); err != nil {
			return fmt.Errorf("store: lock linear routing: %w", err)
		}
		groupID, err := ensureLinearRoutingGroupTx(ctx, qtx, adminID)
		if err != nil {
			return err
		}
		if id, err = getOrInsertLinearRoutingChannelTx(ctx, qtx, groupID); err != nil {
			return err
		}
		members, err := expandOwnerMembership(ctx, tx, supervisorID, []AccountID{bridgeID})
		if err != nil {
			return err
		}
		return reconcileLinearRoutingChannelTx(ctx, tx, id, members)
	}); err != nil {
		return "", err
	}
	return id, nil
}

// LinearRoutingChannel returns the admin's routing channel by its reserved (group, name)
// key; ErrNotFound until EnsureLinearRoutingChannel has created it.
func (s *Store) LinearRoutingChannel(ctx context.Context, adminID AccountID) (ChannelID, error) {
	if adminID == "" {
		return "", fmt.Errorf("%w: admin account id is required", ErrInvalidArgument)
	}
	groupID, err := s.q.GetLinearRoutingGroup(ctx, linearRoutingGroupParams(adminID))
	switch {
	case noRows(err):
		return "", fmt.Errorf("%w: linear routing group", ErrNotFound)
	case err != nil:
		return "", fmt.Errorf("store: resolve linear routing group: %w", err)
	}
	row, err := s.q.GetLinearRoutingChannel(ctx, db.GetLinearRoutingChannelParams{
		GroupID: pgtype.Text{String: groupID, Valid: true},
		Name:    LinearRoutingChannelName,
	})
	switch {
	case noRows(err):
		return "", fmt.Errorf("%w: %s channel", ErrNotFound, LinearRoutingChannelName)
	case err != nil:
		return "", fmt.Errorf("store: resolve linear routing channel: %w", err)
	}
	return linearRoutingChannelID(row)
}

// linearRoutingGroupParams keys the reserved group; the visibility term means a planted
// wider __linear__ group is never adopted (the EnsureOwnerDMGroupTx discriminator).
func linearRoutingGroupParams(adminID AccountID) db.GetLinearRoutingGroupParams {
	return db.GetLinearRoutingGroupParams{
		OwnerUserID: string(adminID),
		Name:        linearRoutingGroupName,
		Visibility:  int16(VisibilityOwner),
	}
}

// ensureLinearRoutingGroupTx get-or-creates the admin's reserved group; the caller holds
// the advisory lock, so the select-then-insert cannot race.
func ensureLinearRoutingGroupTx(ctx context.Context, qtx *db.Queries, adminID AccountID) (ChannelGroupID, error) {
	switch existing, err := qtx.GetLinearRoutingGroup(ctx, linearRoutingGroupParams(adminID)); {
	case err == nil:
		return ChannelGroupID(existing), nil
	case !noRows(err):
		return "", fmt.Errorf("store: resolve linear routing group: %w", err)
	}
	id := newID()
	if err := qtx.InsertLinearRoutingGroup(ctx, db.InsertLinearRoutingGroupParams{
		ID:          id,
		Name:        linearRoutingGroupName,
		OwnerUserID: string(adminID),
		Visibility:  int16(VisibilityOwner),
	}); err != nil {
		return "", fmt.Errorf("store: insert linear routing group: %w", err)
	}
	return ChannelGroupID(id), nil
}

// getOrInsertLinearRoutingChannelTx resolves the channel at the reserved (group, name)
// key, inserting it born mandatory when absent.
func getOrInsertLinearRoutingChannelTx(ctx context.Context, qtx *db.Queries, groupID ChannelGroupID) (ChannelID, error) {
	group := pgtype.Text{String: string(groupID), Valid: true}
	for {
		switch existing, err := qtx.GetLinearRoutingChannel(ctx, db.GetLinearRoutingChannelParams{
			GroupID: group,
			Name:    LinearRoutingChannelName,
		}); {
		case err == nil:
			return linearRoutingChannelID(existing)
		case !noRows(err):
			return "", fmt.Errorf("store: resolve linear routing channel: %w", err)
		}
		switch insertedID, err := qtx.InsertLinearRoutingChannel(ctx, db.InsertLinearRoutingChannelParams{
			ID:                    newID(),
			Name:                  LinearRoutingChannelName,
			GroupID:               group,
			Kind:                  int16(ChannelKindChannel),
			PostPolicy:            int16(ChannelPostPolicyOpen),
			MandatorySubscription: true,
		}); {
		case err == nil:
			return ChannelID(insertedID), nil
		case !noRows(err):
			return "", fmt.Errorf("store: insert linear routing channel: %w", err)
		}
		// A concurrent writer won the (group, name) race; the next pass selects its row.
	}
}

// linearRoutingChannelID refuses a wrong-kind row at the reserved key rather than
// adopting it, as the DM belt does.
func linearRoutingChannelID(row db.GetLinearRoutingChannelRow) (ChannelID, error) {
	if ChannelKind(row.Kind) != ChannelKindChannel {
		return "", fmt.Errorf("%w: linear routing channel %q is not a plain channel", ErrNotFound, row.ID)
	}
	return ChannelID(row.ID), nil
}

// reconcileLinearRoutingChannelTx converges the channel on its required shape. A
// mandatory channel delivers to every member, so every agent member needs a cursor.
func reconcileLinearRoutingChannelTx(ctx context.Context, tx pgx.Tx, channelID ChannelID, members []AccountID) error {
	qtx := db.New(tx)
	if err := qtx.ReassertLinearRoutingShape(ctx, db.ReassertLinearRoutingShapeParams{
		ID:         string(channelID),
		PostPolicy: int16(ChannelPostPolicyOpen),
	}); err != nil {
		return fmt.Errorf("store: reassert linear routing shape: %w", err)
	}
	for _, m := range members {
		if err := qtx.EnsureChannelMember(ctx, db.EnsureChannelMemberParams{
			ChannelID: string(channelID),
			AccountID: string(m),
		}); err != nil {
			return upsertMemberErr(err, m)
		}
	}
	return seedChannelDeliveryCursors(ctx, tx, channelID)
}
