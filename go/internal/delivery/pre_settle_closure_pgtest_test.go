//go:build pgtest && unix

package delivery

// RIG-2490 T4 — the pre-settle mention-loss closure cycle, end-to-end over the FULL
// T1–T3 stack against a REAL Postgres. Each leg drives the true consumer and gates
// on the durable effect (an owed_mentions row, a steer). Crash/restart legs post the
// mention THROUGH THE STORE with no ref published, so only the recovery scan surfaces it.

import (
	"context"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// openDeliveryStore opens a Store against a freshly-migrated database, SKIPPING
// (via pgtest.RequireDSN) when no DSN and no opted-in container runtime are
// available — the delivery package's own store construction, mirroring the store
// package's openStore (harness_test.go) since that helper is unexported to this
// package. context.Background() is the test root (thread-context exemption).
func openDeliveryStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := pgtest.RequireDSN(t)
	s, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// newPgConsumer builds a consumer over the REAL store and a fresh fake fabric,
// with the shared in-memory fakes for the hub's dispatch + resolution roles. A
// store-only post publishes no ref, so only the recovery scan surfaces it.
func newPgConsumer(t *testing.T, s *store.Store) (*Consumer, *fakeDispatcher, *fakeResolver) {
	t.Helper()
	disp := newFakeDispatcher()
	res := newFakeResolver()
	c := NewConsumer(s, disp, res, newFakeFabric(), discardLogger())
	return c, disp, res
}

// mustOwner creates the human owner account or fails the test.
func mustOwner(t *testing.T, ctx context.Context, s *store.Store) store.Account {
	t.Helper()
	u, err := s.CreateUser(ctx, store.NewUser{Handle: "owner", DisplayName: "owner"})
	if err != nil {
		t.Fatalf("CreateUser(owner): %v", err)
	}
	return u
}

// mustAgentAcct creates an owned agent account or fails the test.
func mustAgentAcct(t *testing.T, ctx context.Context, s *store.Store, owner store.AccountID, handle string) store.Account {
	t.Helper()
	a, err := s.CreateAgent(ctx, owner, store.NewAgent{Handle: handle, DisplayName: handle})
	if err != nil {
		t.Fatalf("CreateAgent(%q): %v", handle, err)
	}
	return a
}

// mustRoomWithMembers creates a plain channel named "room" owned by owner with
// the given members added (unsubscribed on create — the exact out-of-sweep-set
// state a crash-recovery mention targets).
func mustRoomWithMembers(t *testing.T, ctx context.Context, s *store.Store, owner store.AccountID, members ...store.AccountID) store.ChannelID {
	t.Helper()
	ch, err := s.CreateChannel(ctx, owner, store.NewChannel{
		Name: "room", Kind: store.ChannelKindChannel, MemberAccountIDs: members,
	})
	if err != nil {
		t.Fatalf("CreateChannel(room): %v", err)
	}
	return ch.ID
}

// subscribeMember flips a member's subscribed flag true through the public
// member-update path (so it can post / receive live delivers).
func subscribeMember(t *testing.T, ctx context.Context, s *store.Store, owner store.AccountID, ch store.ChannelID, agent store.AccountID) {
	t.Helper()
	if _, _, err := s.UpdateChannelMembers(ctx, owner, ch, []store.MemberUpdate{{AccountID: agent, Subscribed: true}}, store.MemberUpdatesOptions{}); err != nil {
		t.Fatalf("UpdateChannelMembers(subscribe %s): %v", agent, err)
	}
}

// postThroughStore commits a message directly via the store's public append
// path and publishes NO ref: the message is durable with a NULL mention marker,
// so a consumer can surface it only via the recovery scan.
func postThroughStore(t *testing.T, ctx context.Context, s *store.Store, ch store.ChannelID, author store.AccountID, body string) store.Message {
	t.Helper()
	m, _, err := s.AppendMessage(ctx, store.Message{
		AuthorAccountID: author,
		Blocks:          []store.MessageBlock{{Text: &body}},
	}, string(ch), store.TopicRef{Name: "general", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage(%q): %v", body, err)
	}
	return m
}

// owedTotal counts the owed_mention rows for agent across all channels.
func owedTotal(t *testing.T, ctx context.Context, s *store.Store, agent store.AccountID) int {
	t.Helper()
	owed, err := s.OwedMentions(ctx, agent)
	if err != nil {
		t.Fatalf("OwedMentions(%s): %v", agent, err)
	}
	n := 0
	for _, msgs := range owed {
		n += len(msgs)
	}
	return n
}

// waitOwed blocks until agent has exactly want owed rows, or fails at the
// deadline — the durable barrier for the crash/agent-authored legs, where the
// owed row (not a dispatch) is the recovery scan's observable effect. Polls the
// real store, never a sleep-as-synchronization.
func waitOwed(t *testing.T, ctx context.Context, s *store.Store, agent store.AccountID, want int) {
	t.Helper()
	deadline := time.After(testTimeout)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if owedTotal(t, ctx, s, agent) == want {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("owed rows for %s = %d, want %d", agent, owedTotal(t, ctx, s, agent), want)
		}
	}
}

// unroutedContains reports whether messageID is still in the committed-but-
// unmarked set (mentions_routed_at IS NULL) — i.e. the recovery scan has NOT yet
// marked it. A recovered-and-marked message is absent.
func unroutedContains(t *testing.T, ctx context.Context, s *store.Store, messageID string) bool {
	t.Helper()
	rows, err := s.UnroutedMentionMessages(ctx, 0, 256)
	if err != nil {
		t.Fatalf("UnroutedMentionMessages: %v", err)
	}
	for _, r := range rows {
		if string(r.ID) == messageID {
			return true
		}
	}
	return false
}

// waitMarked blocks until messageID leaves the committed-but-unmarked set — the
// recovery scan records the owed row (routeMentionsFor) BEFORE it marks the
// message (scan.go), so a waitOwed return can precede the mark by a window;
// gating the mark assertion on its own durable effect keeps the leg deterministic
// without a sleep.
func waitMarked(t *testing.T, ctx context.Context, s *store.Store, messageID string) {
	t.Helper()
	deadline := time.After(testTimeout)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if !unroutedContains(t, ctx, s, messageID) {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("message %s still unmarked at deadline", messageID)
		}
	}
}

// Leg 1 — CRASH. A mention to an offline, out-of-sweep-set agent member is
// committed with a NULL marker and no ref published. Run's START scan recovers it
// into an owed row; the member's session start sweeps it as EXACTLY ONE steer; an
// ack clears it so a second start sweeps nothing.
func TestCrashRecoveryStartScanOwedThenOneSteerThenAckClears(t *testing.T) {
	ctx := context.Background()
	s := openDeliveryStore(t)
	owner := mustOwner(t, ctx, s)
	member := mustAgentAcct(t, ctx, s, owner.ID, "aa")
	ch := mustRoomWithMembers(t, ctx, s, owner.ID, member.ID)

	// Precondition: the member is genuinely OUT of the sweep set — no cursor-sweep
	// backstop, so the mention needs the durable owed row the scan records.
	if in, err := s.InSweepSet(ctx, member.ID, ch); err != nil || in {
		t.Fatalf("InSweepSet(member,ch) = (%v,%v), want (false,nil): the leg requires an out-of-sweep-set member", in, err)
	}

	// The mention is committed (marker NULL) but never published — severed.
	msg := postThroughStore(t, ctx, s, ch, owner.ID, "@aa ping while you were offline")

	c, disp, res := newPgConsumer(t, s)
	startConsumer(t, c) // the start scan runs before Run subscribes

	// Recovered: a durable owed row materialized purely from durable state.
	waitOwed(t, ctx, s, member.ID, 1)
	waitMarked(t, ctx, s, string(msg.ID))

	// Session start with the resolver bound: the owed mention sweeps as one steer.
	res.bind(member.ID, "sess-member")
	c.OnSessionStarted("sess-member", member.ID)
	if !disp.waitForMessage(t, string(msg.ID)) {
		t.Fatalf("owed mention %s never steered on session start", msg.ID)
	}
	got := disp.snapshot()
	if len(got) != 1 {
		t.Fatalf("dispatches = %d, want exactly 1 (one steer for the recovered mention)", len(got))
	}
	if got[0].sessionID != "sess-member" || got[0].messageID != string(msg.ID) || got[0].kind != opSteer {
		t.Fatalf("dispatch = %+v, want {sess-member, %s, steer}", got[0], msg.ID)
	}

	// Ack clears the owed row (store T1); a second start edge then sweeps nothing.
	// The load-bearing guard is the owedTotal==0 durable read below (synchronous,
	// right after AckDelivery); the post-restart dispatch-count assertion is
	// belt-and-suspenders, not a reliable negative barrier.
	if err := s.AckDelivery(ctx, member.ID, ch, string(msg.ID)); err != nil {
		t.Fatalf("AckDelivery: %v", err)
	}
	if n := owedTotal(t, ctx, s, member.ID); n != 0 {
		t.Fatalf("owed rows after ack = %d, want 0", n)
	}
	c.OnSessionStarted("sess-member", member.ID)
	c.waitStartsDrained(t) // sound: the drained edge sweeps nothing (owed cleared, out-of-sweep-set)
	if n := len(disp.snapshot()); n != 1 {
		t.Fatalf("dispatches after ack + re-start = %d, want 1 (the re-sweep owes nothing)", n)
	}
}

// Leg 2 — NEGATIVE CONTROL. The store-only post is in the committed-but-unmarked
// set, yet OwedMentions is empty until the scan runs — so the crash leg's owed row
// is the scan's alone (the start-scan ablation reddens Leg 1/4).
func TestNegativeControlStoreOnlyPostNoOwedWithoutScan(t *testing.T) {
	ctx := context.Background()
	s := openDeliveryStore(t)
	owner := mustOwner(t, ctx, s)
	member := mustAgentAcct(t, ctx, s, owner.ID, "aa")
	ch := mustRoomWithMembers(t, ctx, s, owner.ID, member.ID)

	msg := postThroughStore(t, ctx, s, ch, owner.ID, "@aa severed mention")

	// The scan's INPUT exists (committed, NULL marker) ...
	if !unroutedContains(t, ctx, s, string(msg.ID)) {
		t.Fatalf("committed mention %s absent from the unrouted set: the scan would have no input", msg.ID)
	}
	// ... but WITHOUT the scan running, no owed row is produced.
	if n := owedTotal(t, ctx, s, member.ID); n != 0 {
		t.Fatalf("owed rows without running the scan = %d, want 0 (only the recovery scan may create one)", n)
	}
}

// Leg 3 — AGENT-AUTHORED, held-then-restart. An agent-authored mention posted while
// its author streams is HELD live and marked only at settle. A restart before
// settle is a fresh consumer: no author session, c.held empty, so the
// committed-NULL message is scannable and the start scan recovers it.
func TestAgentAuthoredHeldThenRestartStartScanRecovers(t *testing.T) {
	ctx := context.Background()
	s := openDeliveryStore(t)
	owner := mustOwner(t, ctx, s)
	author := mustAgentAcct(t, ctx, s, owner.ID, "author")
	member := mustAgentAcct(t, ctx, s, owner.ID, "aa")
	ch := mustRoomWithMembers(t, ctx, s, owner.ID, author.ID, member.ID)
	subscribeMember(t, ctx, s, owner.ID, ch, author.ID) // the agent author posts here

	// member stays out of the sweep set (unsubscribed, non-home, non-mandatory).
	if in, err := s.InSweepSet(ctx, member.ID, ch); err != nil || in {
		t.Fatalf("InSweepSet(member,ch) = (%v,%v), want (false,nil)", in, err)
	}

	// Agent-authored mention, committed NULL, unpublished — the held message the
	// restart severs from any live author turn.
	msg := postThroughStore(t, ctx, s, ch, author.ID, "@aa agent-authored mention held then lost")

	c, _, _ := newPgConsumer(t, s) // fresh consumer => c.held empty, no live author
	// Intent marker (a construction invariant, not a live guard): a freshly-built
	// consumer's c.held is empty, so the restart premise — the held message is no
	// longer held and is therefore scannable — holds by construction.
	if c.messageHeld(string(msg.ID)) {
		t.Fatalf("message %s unexpectedly held on a fresh consumer: the restart must clear c.held", msg.ID)
	}
	startConsumer(t, c)

	waitOwed(t, ctx, s, member.ID, 1)
	waitMarked(t, ctx, s, string(msg.ID))
}

// Leg 4 — PUBLISH FAILED MID-RUN. A mention committed after Run subscribed, whose
// publish failed, is invisible to the start scan; the scan a fabric reconnect
// triggers recovers it into a durable owed row and marks it.
func TestReconnectScanRecoversPublishFailedMention(t *testing.T) {
	ctx := context.Background()
	s := openDeliveryStore(t)
	owner := mustOwner(t, ctx, s)
	member := mustAgentAcct(t, ctx, s, owner.ID, "aa")
	ch := mustRoomWithMembers(t, ctx, s, owner.ID, member.ID)

	if in, err := s.InSweepSet(ctx, member.ID, ch); err != nil || in {
		t.Fatalf("InSweepSet(member,ch) = (%v,%v), want (false,nil)", in, err)
	}

	c, _, _ := newPgConsumer(t, s)
	startConsumer(t, c)
	fab := fakeFabricOf(c)
	fab.waitSubscribed(t)

	m1 := postThroughStore(t, ctx, s, ch, owner.ID, "@aa committed but never published")
	fab.fireReconnect()

	waitOwed(t, ctx, s, member.ID, 1)
	waitMarked(t, ctx, s, string(m1.ID))
}
