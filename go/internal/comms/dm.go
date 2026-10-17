package comms

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/RigelBuild/compass/go/internal/store"
)

// crossOwnerDMName keys a peer DM on both party ids, not reclaimable handles.
func crossOwnerDMName(a, b store.AccountID) string {
	lo, hi := string(a), string(b)
	if lo > hi {
		lo, hi = hi, lo
	}
	return "xdm:" + lo + ":" + hi
}

// openDMTx runs peer-DM authorization and the upsert in one store transaction.
// Row locks held by the peering check serialize a revoke through this commit.
func (c *Comms) openDMTx(ctx context.Context, owner, callerOwner, peerOwner store.AccountID, name, peerHandle string, members []store.AccountID) (store.ChannelID, bool, error) {
	var (
		channelID store.ChannelID
		created   bool
	)
	if err := c.store.WithTx(ctx, func(tx pgx.Tx) error {
		if err := store.LockOwnerDMTx(ctx, tx, owner); err != nil {
			return err
		}
		if callerOwner == peerOwner {
			callerHandle, err := c.store.AccountHandleTx(ctx, tx, members[0])
			if err != nil {
				return err
			}
			peerCurrentHandle, err := c.store.AccountHandleTx(ctx, tx, members[1])
			if err != nil {
				return err
			}
			name = store.DMChannelName(callerHandle, peerCurrentHandle)
		}
		if callerOwner != peerOwner {
			peered, err := c.store.OwnersPeeredTx(ctx, tx, callerOwner, peerOwner)
			if err != nil {
				return err
			}
			if !peered {
				return notFoundHandle(store.ErrNotFound, peerHandle)
			}
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
// is a no-op event-wise (nothing changed). It takes the channel the caller
// already read for the response, so the create path does not re-read the same row
// a second time. NEVER call before the commit.
func (c *Comms) emitDMCreated(ctx context.Context, ch store.Channel, created bool) {
	if !created {
		return
	}
	c.publishChannelChanged(ctx, ch, nil)
}
