//go:build unix

package delivery

import (
	"context"
	"slices"
)

// scanBatchLimit bounds one UnroutedMentionMessages read so a long-idle deploy
// (a large committed-but-unmarked backlog) cannot hold the whole set in memory
// at once — the scan loops, one bounded batch at a time.
const scanBatchLimit = 256

// scanMissedMentions replays the settle-edge mention pass over committed
// messages whose durable marker is still NULL (mentions_routed_at IS NULL),
// closing the pre-settle mention-loss window at a recovery point (RIG-2490 T2).
// For each unmarked message it runs the ONE shared mention routine
// (routeMentionsFor) against current durable state, then marks the message so a
// later scan structurally excludes it.
//
// The loop keeps a scan-LOCAL, non-persisted seq floor (afterSeq, starting 0):
// each batch advances it to the last returned row's seq, so a message the scan
// SKIPS — because it is currently in c.held, i.e. the live settle path genuinely
// owns it — is stepped past WITHIN this invocation and cannot re-fill the same
// batch and hang the loop; but because nothing is persisted, the skipped
// message stays NULL and is re-scanned from seq 0 at the NEXT recovery point,
// until its settle pass marks it. This is deliberately NOT the killed
// high-water: the floor lives only for this invocation's termination.
//
// The scan never fails Run or the consumer: a per-message fault leaves that
// message UNMARKED (re-scanned next recovery point) and continues; a batch-read
// fault stops this scan. Never returns an error, never panics (§Global
// Constraints: never fail the consumer).
func (c *Consumer) scanMissedMentions(ctx context.Context) {
	var afterSeq int64
	for {
		batch, err := c.st.UnroutedMentionMessages(ctx, afterSeq, scanBatchLimit)
		if err != nil {
			c.log.ErrorContext(ctx, "delivery: scan missed mentions batch read", "error", err, "after_seq", afterSeq)
			return
		}
		for _, row := range batch {
			id := string(row.ID)
			if c.messageHeld(id) {
				// The live settle path owns this message (fireHeld will mark it);
				// leave it NULL and re-scan it next recovery point.
				continue
			}
			wireMsg, channel, author, err := c.storeMessageToWire(ctx, id)
			if err != nil {
				c.log.ErrorContext(ctx, "delivery: scan missed mentions re-read message", "error", err, "message_id", id)
				continue // leave unmarked; re-scanned next recovery point
			}
			c.routeMentionsFor(ctx, channel, author, wireMsg)
			if err := c.st.MarkMentionsRouted(ctx, id); err != nil {
				c.log.ErrorContext(ctx, "delivery: scan missed mentions mark", "error", err, "message_id", id)
				continue // leave unmarked; the pass ran but re-runs next point (at-least-once)
			}
		}
		if len(batch) < scanBatchLimit {
			return // short batch: the backlog is drained
		}
		afterSeq = batch[len(batch)-1].Seq
	}
}

// messageHeld reports whether messageID is currently registered in the
// held-deliver registry — its id appears in the flattened value set across all
// author-session entries (c.held is keyed by author session id; the values are
// ordered held message ids). Read under c.mu; the caller must NOT hold c.mu
// across the mention routine.
func (c *Consumer) messageHeld(messageID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ids := range c.held {
		if slices.Contains(ids, messageID) {
			return true
		}
	}
	return false
}
