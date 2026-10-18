//go:build unix

package delivery

import (
	"context"
)

// scanBatchLimit bounds one UnroutedMentionMessages read so a long-idle deploy
// (a large committed-but-unmarked backlog) cannot hold the whole set in memory
// at once — the scan loops, one bounded batch at a time.
const scanBatchLimit = 256

// scanMissedMentions replays the shared mention policy for committed messages whose marker is NULL, then marks each message.
// A call-local sequence floor prevents held messages from refilling a batch; nothing persists, so they retry next recovery point.
// Per-message faults leave that row unmarked and continue; batch-read faults stop the scan.
// Recovery faults never fail the consumer.
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
			// RIG-2257: re-derive the ask_answer out-of-sweep owed row here too,
			// so a consumer restart in the AnswerAsk-commit → fanOut window still
			// backstops the out-of-sweep asker (the answer message is committed
			// mentions_routed_at IS NULL, so it appears in this scan).
			c.routeAskAnswerFor(ctx, channel, wireMsg)
			if err := c.markUnlessHeld(ctx, id); err != nil {
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

// messageHeld checks the held-deliver registry under c.mu.
// Callers must release c.mu before running the mention routine.
func (c *Consumer) messageHeld(messageID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entries := range c.held {
		for _, e := range entries {
			if e.messageID == messageID {
				return true
			}
		}
	}
	return false
}

// markUnlessHeld marks messageID routed unless a callback held it after the
// scan's first check; a message held at mark time stays NULL for its settle
// pass to mark.
func (c *Consumer) markUnlessHeld(ctx context.Context, messageID string) error {
	if c.messageHeld(messageID) {
		return nil
	}
	return c.st.MarkMentionsRouted(ctx, messageID)
}
