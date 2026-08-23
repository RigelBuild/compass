//go:build unix

package delivery

// RIG-2490 T2 — the recovery scan (scanMissedMentions), RED-first. Each case
// drives the scan SYNCHRONOUSLY (not through the bus) over hand-written fakes
// and asserts the observable effects: owed rows recorded, wakes, steers, and the
// mentions-routed mark. context.Background() is the test root
// (rule://go-thread-context exemption for _test.go); it is passed straight into
// scanMissedMentions and never re-rooted.

import (
	"context"
	"errors"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// Case a: an unmarked message mentioning an OFFLINE out-of-sweep-set member ⇒
// the scan records a durable owed row, wakes the member, and marks the message
// complete — the crash/overrun recovery of a pre-settle mention.
func TestScanRecoversOfflineOutOfSweepSetMention(t *testing.T) {
	c, disp, _, reads := newTestConsumer(t)
	w := withWaker(c)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.members[ch] = []store.AccountID{agentA}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	// agentA offline (never bound) and out of the sweep set (sweepSet unseeded).
	reads.seedUnrouted(textMessage("m1", author, "@aa ping"), ch, 1)

	c.scanMissedMentions(context.Background())

	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("dispatches = %d, want 0 (offline member: no live session to steer)", len(got))
	}
	if n := reads.owedCount(agentA); n != 1 {
		t.Fatalf("owed rows for agentA = %d, want 1 (out-of-sweep-set offline mention records durably)", n)
	}
	if got := w.count(agentA); got != 1 {
		t.Fatalf("wakes for agentA = %d, want 1", got)
	}
	if got := reads.markCount("m1"); got != 1 {
		t.Fatalf("marks for m1 = %d, want 1 (a processed message is marked complete)", got)
	}
}

// Case b: an unmarked message whose id is currently in c.held ⇒ the live settle
// path owns it, so the scan SKIPS it: no routing effects and NOT marked (it
// stays NULL for the next recovery point until its settle pass marks it).
func TestScanSkipsHeldMessage(t *testing.T) {
	c, disp, _, reads := newTestConsumer(t)
	w := withWaker(c)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.members[ch] = []store.AccountID{agentA}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	reads.seedUnrouted(textMessage("m1", author, "@aa ping"), ch, 1)
	c.hold("author-sess", "m1") // registered in c.held under its author session

	c.scanMissedMentions(context.Background())

	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("dispatches = %d, want 0 (held message is skipped)", len(got))
	}
	if n := reads.owedCount(agentA); n != 0 {
		t.Fatalf("owed rows for agentA = %d, want 0 (held message not routed by the scan)", n)
	}
	if got := w.total(); got != 0 {
		t.Fatalf("wakes = %d, want 0 (held message is skipped)", got)
	}
	if got := reads.markCount("m1"); got != 0 {
		t.Fatalf("marks for m1 = %d, want 0 (a held message stays NULL until its settle pass)", got)
	}
}

// Case c: an unmarked message mentioning a LIVE gap-population member ⇒ the scan
// steers it directly (the live arm survives the factoring) and marks the message.
func TestScanSteersLiveMention(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const ch2 store.ChannelID = "chan-2"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"
	const agentB store.AccountID = "agent-b"

	// Two channels with disjoint member sets: the scan must route each message
	// against its OWN row.Channel, not a single hardcoded channel — a scan that
	// ignored row.Channel would resolve the wrong member set.
	reads.members[ch] = []store.AccountID{agentA}
	reads.members[ch2] = []store.AccountID{agentB}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	reads.handles["bb"] = agentAccount(agentB, "bb")
	res.bind(agentA, "sess-a") // live
	res.bind(agentB, "sess-b") // live
	reads.seedUnrouted(textMessage("m1", author, "@aa ping"), ch, 1)
	reads.seedUnrouted(textMessage("m2", author, "@bb ping"), ch2, 2)

	c.scanMissedMentions(context.Background())

	got := disp.snapshot()
	if len(got) != 2 {
		t.Fatalf("dispatches = %d, want 2 (both live mentions steered)", len(got))
	}
	steers := map[string]string{} // messageID -> sessionID
	for _, d := range got {
		if d.kind != opSteer {
			t.Fatalf("dispatch = %+v, want a steer", d)
		}
		steers[d.messageID] = d.sessionID
	}
	if steers["m1"] != "sess-a" {
		t.Fatalf("m1 steered to %q, want sess-a (ch-1 member)", steers["m1"])
	}
	if steers["m2"] != "sess-b" {
		t.Fatalf("m2 steered to %q, want sess-b (ch-2 member — per-row channel resolution)", steers["m2"])
	}
	if n := reads.owedCount(agentA); n != 0 {
		t.Fatalf("owed rows for agentA = %d, want 0 (live member: steered, not owed)", n)
	}
	if got := reads.markCount("m1"); got != 1 {
		t.Fatalf("marks for m1 = %d, want 1", got)
	}
	if got := reads.markCount("m2"); got != 1 {
		t.Fatalf("marks for m2 = %d, want 1", got)
	}
}

// Case d: a per-message re-read fault (storeMessageToWire error — an unseeded
// message row) leaves that message UNMARKED and the scan continues to the next.
func TestScanPerMessageFaultContinues(t *testing.T) {
	c, _, _, reads := newTestConsumer(t)
	w := withWaker(c)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.members[ch] = []store.AccountID{agentA}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	// m1 is in the unrouted set but its message row is NOT seeded, so
	// storeMessageToWire's MessageByID returns ErrNotFound — a per-message fault.
	reads.mu.Lock()
	reads.unrouted = append(reads.unrouted, store.MessageWithChannel{
		Message: store.Message{ID: "m1"}, Channel: ch, Seq: 1,
	})
	reads.mu.Unlock()
	// m2 is well-formed and follows m1.
	reads.seedUnrouted(textMessage("m2", author, "@aa ping"), ch, 2)

	c.scanMissedMentions(context.Background())

	if got := reads.markCount("m1"); got != 0 {
		t.Fatalf("marks for m1 = %d, want 0 (a faulted message is left unmarked)", got)
	}
	if got := reads.markCount("m2"); got != 1 {
		t.Fatalf("marks for m2 = %d, want 1 (the scan continues past the fault)", got)
	}
	if n := reads.owedCount(agentA); n != 1 {
		t.Fatalf("owed rows for agentA = %d, want 1 (m2 routed after m1 faulted)", n)
	}
	if got := w.count(agentA); got != 1 {
		t.Fatalf("wakes for agentA = %d, want 1 (m2 woke the offline member)", got)
	}
}

// Case e (batch-walk): more than scanBatchLimit unmarked messages are all
// processed across batches, and the scan-local afterSeq floor advances past each
// batch's last seq (so the loop terminates and never re-reads a prefix).
func TestScanWalksMultipleBatches(t *testing.T) {
	c, _, _, reads := newTestConsumer(t)
	withWaker(c)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.members[ch] = []store.AccountID{agentA}
	reads.handles["aa"] = agentAccount(agentA, "aa")

	const total = scanBatchLimit + 5
	for i := 1; i <= total; i++ {
		id := "m" + itoa(i)
		reads.seedUnrouted(textMessage(id, author, "@aa ping"), ch, int64(i))
	}

	c.scanMissedMentions(context.Background())

	for i := 1; i <= total; i++ {
		id := "m" + itoa(i)
		if got := reads.markCount(id); got != 1 {
			t.Fatalf("marks for %s = %d, want 1 (every message processed across batches)", id, got)
		}
	}
	// The floor advances: first read at 0, second read at the first batch's last
	// seq (scanBatchLimit), and a terminating short-batch read follows.
	reads.mu.Lock()
	calls := append([]int64(nil), reads.unroutedCalls...)
	reads.mu.Unlock()
	if len(calls) < 2 {
		t.Fatalf("UnroutedMentionMessages calls = %d, want >= 2 (batch-walk)", len(calls))
	}
	if calls[0] != 0 {
		t.Fatalf("first afterSeq = %d, want 0 (scan-local floor starts at 0)", calls[0])
	}
	if calls[1] != scanBatchLimit {
		t.Fatalf("second afterSeq = %d, want %d (floor advanced to the first batch's last seq)", calls[1], scanBatchLimit)
	}
}

// Case f: a batch-read fault STOPS the current scan (never returned up, never
// panics) — nothing after the failed read is processed.
func TestScanBatchReadFaultStops(t *testing.T) {
	c, _, _, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	reads.seedUnrouted(textMessage("m1", author, "@aa ping"), ch, 1)
	reads.unroutedErr = errors.New("db down")

	c.scanMissedMentions(context.Background()) // must not panic / must return

	if got := reads.markCount("m1"); got != 0 {
		t.Fatalf("marks for m1 = %d, want 0 (batch-read fault stops the scan before processing)", got)
	}
	// The read must have been ATTEMPTED exactly once, then the scan returned on
	// the error — a regression that skipped the read entirely would also leave
	// m1 unmarked, so the mark check alone does not prove attempted-then-stopped.
	reads.mu.Lock()
	calls := append([]int64(nil), reads.unroutedCalls...)
	reads.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("UnroutedMentionMessages calls = %d, want 1 (read attempted once, then scan stops on the fault)", len(calls))
	}
}

// itoa is a tiny base-10 int→string for building distinct test message ids
// without importing strconv into the test surface.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
