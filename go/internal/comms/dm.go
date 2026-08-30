package comms

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/RigelBuild/compass/go/internal/store"
)

// dmChannelName derives the deterministic sorted-handle pair name for a peer DM.
// Handles are sorted lexicographically so open(a,b) and open(b,a) resolve the
// same channel (store dm_pgtest_test.go proves reversed member order → same name).
func dmChannelName(h1, h2 string) string {
	lo, hi := h1, h2
	if lo > hi {
		lo, hi = hi, lo
	}
	return "dm--" + lo + "--" + hi
}

// openDMTx runs the whole peer-DM open for owner in ONE store transaction — the
// same shape the store's openDM test helper (dm_pgtest_test.go:27-49) and
// EnsureCoordinationChannel (coordination.go:179-195) use: take the per-owner DM
// advisory lock, ensure the owner's reserved __dm__ group, then upsert the
// deterministic-name channel for the two agent parties. Returns the resolved
// channel id and whether it was created this call (a resume returns false). The
// lock serializes every open under owner's DM namespace, so the group-ensure and
// the channel-upsert cannot race a concurrent first-open into two groups or two
// channels.
func (c *Comms) openDMTx(ctx context.Context, owner store.AccountID, name string, members []store.AccountID) (store.ChannelID, bool, error) {
	var (
		channelID store.ChannelID
		created   bool
	)
	if err := c.store.WithTx(ctx, func(tx pgx.Tx) error {
		if err := store.LockOwnerDMTx(ctx, tx, owner); err != nil {
			return err
		}
		gid, err := c.store.EnsureOwnerDMGroupTx(ctx, tx, owner)
		if err != nil {
			return err
		}
		channelID, created, err = c.store.UpsertDMChannelTx(ctx, tx, store.DMChannelSpec{
			GroupID: gid,
			Name:    name,
			Members: members,
		})
		return err
	}); err != nil {
		return "", false, err
	}
	return channelID, created, nil
}

// emitDMCreated fans a best-effort ChannelChanged after a DM open COMMITTED and
// only when the channel was created this call — the coordination hook's
// post-commit emit posture (coordination.go:158-171): a resume (created=false)
// is a no-op event-wise (nothing changed), and a re-read failure is logged and
// swallowed, never propagated, since the channel already committed and the event
// self-heals on the next open. NEVER call before the commit.
func (c *Comms) emitDMCreated(ctx context.Context, channelID store.ChannelID, created bool) {
	if !created {
		return
	}
	ch, err := c.store.GetChannel(ctx, channelID)
	if err != nil {
		slog.WarnContext(ctx, "comms: post-commit DM channel read for event failed; self-heals on next open",
			"channel_id", string(channelID), "error", err.Error())
		return
	}
	c.publishChannelChanged(ch, nil)
}
