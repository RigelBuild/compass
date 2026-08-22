//go:build unix

package delivery

// The mention→steer routing acceptance cases (SEA-1569 T7, design record D5,
// design.md:507-562, 848-855), RED-first. Each drives the consumer through the
// real events bus + hand-written fakes and gates on the recorder's observed
// dispatches (op-kind: steer vs deliver) — never a sleep, never a retry
// (rule://no-retries). context.Background() is the test root
// (rule://go-thread-context exemption for _test.go); it is threaded into Run via
// startConsumer and never re-rooted below.

import (
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// recordsFor returns the dispatch records targeting sessionID.
func recordsFor(recs []dispatchRecord, sessionID string) []dispatchRecord {
	var out []dispatchRecord
	for _, r := range recs {
		if r.sessionID == sessionID {
			out = append(out, r)
		}
	}
	return out
}

// Case 1 (design.md:848): an `@agent` member with a live session gets a STEER,
// not a deliver — exactly one dispatch to its session, op-kind = steer.
func TestMentionedMemberGetsSteerNotDeliver(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.subscribers[ch] = []store.AccountID{agentA}
	reads.members[ch] = []store.AccountID{agentA}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	res.bind(agentA, "sess-a")
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", author, "hey @aa look here")))
	disp.waitForDispatches(t, 1)

	got := disp.snapshot()
	if len(got) != 1 {
		t.Fatalf("dispatches = %d, want 1 (a single steer to the mentioned member)", len(got))
	}
	if got[0].sessionID != "sess-a" || got[0].messageID != "m1" || got[0].kind != opSteer {
		t.Fatalf("dispatch = %+v, want {sess-a, m1, steer}", got[0])
	}
}

// Case 2 (design.md:848-849): a subscribed agent that was NOT mentioned gets the
// plain DELIVER of the same message, while the mentioned member gets a steer.
func TestUnmentionedSubscriberGetsDeliver(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA, agentB store.AccountID = "agent-a", "agent-b"

	reads.subscribers[ch] = []store.AccountID{agentA, agentB}
	reads.members[ch] = []store.AccountID{agentA, agentB}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	res.bind(agentA, "sess-a")
	res.bind(agentB, "sess-b")
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", author, "hey @aa")))
	disp.waitForDispatches(t, 2)

	got := disp.snapshot()
	if len(got) != 2 {
		t.Fatalf("dispatches = %d, want 2 (steer to A, deliver to B)", len(got))
	}
	a := recordsFor(got, "sess-a")
	if len(a) != 1 || a[0].kind != opSteer {
		t.Fatalf("sess-a records = %+v, want one steer", a)
	}
	b := recordsFor(got, "sess-b")
	if len(b) != 1 || b[0].kind != opDeliver {
		t.Fatalf("sess-b records = %+v, want one deliver", b)
	}
}

// Case 3 (design.md:849): a mention that resolves to an agent that is NOT a
// channel member is a no-op — no steer, even though its session is live. A plain
// subscriber still gets its deliver.
func TestNonMemberMentionIsNoop(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA, agentB store.AccountID = "agent-a", "agent-b"

	// agentA resolves by handle but is NOT a member of ch; agentB is the member.
	reads.subscribers[ch] = []store.AccountID{agentB}
	reads.members[ch] = []store.AccountID{agentB}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	res.bind(agentA, "sess-a") // live, to prove membership (not liveness) is the gate
	res.bind(agentB, "sess-b")
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", author, "@aa are you there")))
	disp.waitForDispatches(t, 1)

	got := disp.snapshot()
	if len(got) != 1 {
		t.Fatalf("dispatches = %d, want 1 (only B's deliver; the non-member mention is a no-op)", len(got))
	}
	if a := recordsFor(got, "sess-a"); len(a) != 0 {
		t.Fatalf("sess-a records = %+v, want none (non-member mention must not steer)", a)
	}
	if got[0].sessionID != "sess-b" || got[0].kind != opDeliver {
		t.Fatalf("dispatch = %+v, want {sess-b, deliver}", got[0])
	}
}

// Case 4 (design.md:849-850): the author never steers itself — neither via a
// self-mention of its own handle NOR via a reserved ping (@agents) that would
// otherwise expand to include it. The author is live, so a leak would be
// observable as a dispatch to its session; there is none. Another live member
// mentioned by @agents does get a steer.
func TestSelfMentionAndReservedSelfNoop(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const authorAgent store.AccountID = "agent-author"
	const agentB store.AccountID = "agent-b"

	reads.subscribers[ch] = []store.AccountID{agentB}
	reads.members[ch] = []store.AccountID{authorAgent, agentB}
	reads.agents[authorAgent] = true // agent-authored: held until the author settles
	reads.handles["author"] = agentAccount(authorAgent, "author")
	res.bind(authorAgent, "sess-author")
	res.bind(agentB, "sess-b")
	// The settled block set carries the self-mention AND the reserved ping.
	reads.seedMessage(textMessage("m1", authorAgent, "@author @agents standup"))
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", authorAgent, "streaming...")))
	c.waitHeld(t, "sess-author", 1)
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	disp.waitForDispatches(t, 1)

	got := disp.snapshot()
	if len(got) != 1 {
		t.Fatalf("dispatches = %d, want 1 (only B's steer; the author never steers itself)", len(got))
	}
	if a := recordsFor(got, "sess-author"); len(a) != 0 {
		t.Fatalf("sess-author records = %+v, want none (self-mention + @agents must exclude the author)", a)
	}
	if got[0].sessionID != "sess-b" || got[0].kind != opSteer {
		t.Fatalf("dispatch = %+v, want {sess-b, steer} (agentB steered by @agents)", got[0])
	}
}

// Case 5 (design.md:850-851): @agents expands to the channel's agent members
// ONLY, author excluded. Two live agent members (both distinct from the author)
// each get a steer; the author — itself an agent member — is excluded by the
// ChannelAgentMembers query. A human member (no agent session) is never on this
// path (structural: the fake's member set holds only agents, mirroring the JOIN
// to agent_accounts).
func TestReservedAgentsExpandsToAgentMembersAuthorExcluded(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const authorAgent store.AccountID = "agent-author"
	const agentA, agentB store.AccountID = "agent-a", "agent-b"

	reads.subscribers[ch] = []store.AccountID{agentA, agentB}
	reads.members[ch] = []store.AccountID{authorAgent, agentA, agentB}
	reads.agents[authorAgent] = true
	// Author has NO live session, so its message delivers at post from the stored
	// (settled) blocks — no hold needed. It is still excluded from @agents.
	reads.seedMessage(textMessage("m1", authorAgent, "@agents sync"))
	res.bind(agentA, "sess-a")
	res.bind(agentB, "sess-b")
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", authorAgent, "@agents sync")))
	disp.waitForDispatches(t, 2)

	got := disp.snapshot()
	if len(got) != 2 {
		t.Fatalf("dispatches = %d, want 2 (steer to A and B; author excluded from @agents)", len(got))
	}
	for _, sess := range []string{"sess-a", "sess-b"} {
		r := recordsFor(got, sess)
		if len(r) != 1 || r[0].kind != opSteer {
			t.Fatalf("%s records = %+v, want one steer", sess, r)
		}
	}
}

// Case 6 (design.md:851-852): a mentioned agent member with NO live session gets
// NOTHING this cycle — no steer (there is no turn to interrupt) and it is NOT
// added to the plain deliver fan-out either (it is still the mentioned agent; the
// cursor+sweep delivers it later). A plain live subscriber still gets its deliver.
func TestMentionedAgentNoLiveSessionSkipped(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA, agentB store.AccountID = "agent-a", "agent-b"

	reads.subscribers[ch] = []store.AccountID{agentA, agentB}
	reads.members[ch] = []store.AccountID{agentA, agentB}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	// agentA (mentioned) has NO live session; only agentB is live.
	res.bind(agentB, "sess-b")
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", author, "@aa ping")))
	disp.waitForDispatches(t, 1)

	got := disp.snapshot()
	if len(got) != 1 {
		t.Fatalf("dispatches = %d, want 1 (only B's deliver; the no-session mentioned agent falls to the sweep)", len(got))
	}
	if got[0].sessionID != "sess-b" || got[0].kind != opDeliver {
		t.Fatalf("dispatch = %+v, want {sess-b, deliver}", got[0])
	}
}

// Case 7 (design.md:852-853): the steer op carries the mentioned message — assert
// op-kind = steer and the message id matches. The ack-path/above-set cursor
// arithmetic is T2's proven behavior (delivery_cursors_test.go Case 3 gap-fill +
// Case 4 self-post: the ack arm is message-id-keyed and blind to deliver-vs-steer),
// so T7 asserts op-kind + message id ONLY, never cursor state.
func TestSteerCarriesMessage(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.subscribers[ch] = []store.AccountID{agentA}
	reads.members[ch] = []store.AccountID{agentA}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	res.bind(agentA, "sess-a")
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m7", author, "@aa urgent")))
	disp.waitForDispatches(t, 1)

	got := disp.snapshot()[0]
	if got.kind != opSteer {
		t.Fatalf("op kind = %v, want steer", got.kind)
	}
	if got.messageID != "m7" {
		t.Fatalf("steer message id = %q, want m7", got.messageID)
	}
}

// RIG-2486 T1: the author's handle is denormalized onto BOTH the steer op (to a
// mentioned member) and the deliver op (to a plain subscriber), resolved once
// server-side via GetAccount from the message's author_account_id. The agent
// reads it off the control to emit the SessionInjection observation's
// from_handle without a per-injection roster lookup. RED before the build sites
// populated FromHandle: both ops carried an empty from_handle, so the handle
// assertions failed.
func TestDeliverAndSteerCarryAuthorFromHandle(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const human store.AccountID = "human-1"
	const agentA, agentB store.AccountID = "agent-a", "agent-b"

	reads.subscribers[ch] = []store.AccountID{agentA, agentB}
	reads.members[ch] = []store.AccountID{agentA, agentB}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	// The author's account resolves its handle for the denormalized from_handle.
	reads.accounts[human] = store.Account{ID: human, Handle: "matt"}
	res.bind(agentA, "sess-a")
	res.bind(agentB, "sess-b")
	startConsumer(t, c)

	// @aa steers agent-a; agent-b (subscribed, unmentioned) gets a plain deliver.
	c.bus.Publish(postedResponse(wireText("m1", human, "hey @aa")))
	disp.waitForDispatches(t, 2)

	got := disp.snapshot()
	a := recordsFor(got, "sess-a")
	if len(a) != 1 || a[0].kind != opSteer || a[0].fromHandle != "matt" {
		t.Fatalf("sess-a records = %+v, want one steer with from_handle=matt", a)
	}
	b := recordsFor(got, "sess-b")
	if len(b) != 1 || b[0].kind != opDeliver || b[0].fromHandle != "matt" {
		t.Fatalf("sess-b records = %+v, want one deliver with from_handle=matt", b)
	}
}

// RIG-2486 T1: a from_handle resolution MISS (the author account is not found)
// is logged and yields an empty from_handle — it never blocks the delivery. The
// deliver still dispatches; only its from_handle is empty.
func TestFromHandleMissDeliversWithEmptyHandle(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const human store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.subscribers[ch] = []store.AccountID{agentA}
	// No reads.accounts entry for the author: GetAccount is ErrNotFound.
	res.bind(agentA, "sess-a")
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", human, "hi")))
	disp.waitForDispatches(t, 1)

	got := disp.snapshot()
	if len(got) != 1 || got[0].kind != opDeliver || got[0].messageID != "m1" {
		t.Fatalf("dispatch = %+v, want one deliver of m1", got)
	}
	if got[0].fromHandle != "" {
		t.Fatalf("from_handle = %q on a store miss, want empty", got[0].fromHandle)
	}
}

// Case 8 (design.md:853-855): a mention absent from the initial MessagePosted
// block set but streamed in via a later store-grow still steers at the author's
// settle edge. The message is HELD while the author streams (no mention yet); the
// store then grows to add the `@mention` block; the author's settle fires the
// held routing from the SETTLED (grown) blocks. Mirrors
// TestAgentAuthoredHeldUntilSettle (settle machinery) +
// TestAgentAuthoredNoLiveAuthorDeliversStoredBlocks (grown-block re-read).
func TestStreamedMentionAtSettleEdgeSteers(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const authorAgent store.AccountID = "agent-author"
	const agentA store.AccountID = "agent-a"

	reads.subscribers[ch] = []store.AccountID{agentA}
	reads.members[ch] = []store.AccountID{agentA, authorAgent}
	reads.agents[authorAgent] = true
	reads.handles["aa"] = agentAccount(agentA, "aa")
	res.bind(authorAgent, "sess-author")
	res.bind(agentA, "sess-a")
	// The SETTLED, grown block set carries the @mention that the POSTED wire did
	// not — the exact stream-in-at-settle gap D5 closes.
	reads.seedMessage(textMessage("m8", authorAgent, "here you go @aa"))
	startConsumer(t, c)

	// Posted while streaming: NO mention yet, so it is HELD; nothing dispatched.
	c.bus.Publish(postedResponse(wireText("m8", authorAgent, "one moment")))
	c.waitHeld(t, "sess-author", 1)
	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("dispatched %d before settle, want 0 (held)", len(got))
	}

	// Author settles: the held routing fires from the grown blocks and steers A.
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	disp.waitForDispatches(t, 1)

	got := disp.snapshot()
	if len(got) != 1 {
		t.Fatalf("dispatches = %d, want 1 (a steer from the settled/grown blocks)", len(got))
	}
	if got[0].sessionID != "sess-a" || got[0].messageID != "m8" || got[0].kind != opSteer {
		t.Fatalf("dispatch = %+v, want {sess-a, m8, steer}", got[0])
	}
}

// Case 9 (design.md:507-562): a `@users` reserved ping is a no-op on the steer
// path — it expands to human members only (no agent session to interrupt), so no
// agent member is steered. Two live subscribed agent members each get their plain
// DELIVER of the post; ZERO steers. This fails if `@users` were ever wired into
// the everyone/agents expansion. (Grounding: dispatch.go:159-166 — `@users` hits
// the reserved branch but is neither "everyone" nor "agents", so it adds nothing.)
func TestReservedUsersIsNoop(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA, agentB store.AccountID = "agent-a", "agent-b"

	reads.subscribers[ch] = []store.AccountID{agentA, agentB}
	reads.members[ch] = []store.AccountID{agentA, agentB}
	res.bind(agentA, "sess-a")
	res.bind(agentB, "sess-b")
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", author, "@users ping")))
	disp.waitForDispatches(t, 2)

	got := disp.snapshot()
	if len(got) != 2 {
		t.Fatalf("dispatches = %d, want 2 (a deliver to each subscriber; @users steers no one)", len(got))
	}
	for _, sess := range []string{"sess-a", "sess-b"} {
		r := recordsFor(got, sess)
		if len(r) != 1 || r[0].kind != opDeliver {
			t.Fatalf("%s records = %+v, want one deliver (never a steer from @users)", sess, r)
		}
	}
}

// Case 10 (design.md:850-851): `@everyone` expands to the channel's agent members
// ONLY, author excluded — the sibling arm to Case 5's `@agents`. Two live agent
// members distinct from the author each get a steer; the author — itself a member
// — is excluded by ChannelAgentMembers. Proves the `h == "everyone"` arm of the
// shared reserved branch (dispatch.go:160), not just `@agents`.
func TestReservedEveryoneExpandsToAgentMembersAuthorExcluded(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const authorAgent store.AccountID = "agent-author"
	const agentA, agentB store.AccountID = "agent-a", "agent-b"

	reads.subscribers[ch] = []store.AccountID{agentA, agentB}
	reads.members[ch] = []store.AccountID{authorAgent, agentA, agentB}
	reads.agents[authorAgent] = true
	// Author has NO live session, so its message delivers at post from the stored
	// (settled) blocks — no hold needed. It is still excluded from @everyone.
	reads.seedMessage(textMessage("m1", authorAgent, "@everyone sync"))
	res.bind(agentA, "sess-a")
	res.bind(agentB, "sess-b")
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", authorAgent, "@everyone sync")))
	disp.waitForDispatches(t, 2)

	got := disp.snapshot()
	if len(got) != 2 {
		t.Fatalf("dispatches = %d, want 2 (steer to A and B; author excluded from @everyone)", len(got))
	}
	for _, sess := range []string{"sess-a", "sess-b"} {
		r := recordsFor(got, sess)
		if len(r) != 1 || r[0].kind != opSteer {
			t.Fatalf("%s records = %+v, want one steer", sess, r)
		}
	}
}

// Case 11 (design.md:849): a mention whose handle resolves to NOTHING (unknown or
// human handle → store.ErrNotFound) is a no-op on the steer path. `@nobody` is not
// seeded in reads.handles, so AgentByHandle returns ErrNotFound and the mention is
// dropped; the one live subscribed member still gets its plain DELIVER, ZERO
// steers. Proves the ErrNotFound → no-op arm (dispatch.go:170-171) directly; Case 3
// only covers a resolvable-but-non-member handle.
func TestUnknownHandleMentionIsNoop(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.subscribers[ch] = []store.AccountID{agentA}
	reads.members[ch] = []store.AccountID{agentA}
	// @nobody is deliberately NOT seeded in reads.handles → AgentByHandle ErrNotFound.
	res.bind(agentA, "sess-a")
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", author, "@nobody ping")))
	disp.waitForDispatches(t, 1)

	got := disp.snapshot()
	if len(got) != 1 {
		t.Fatalf("dispatches = %d, want 1 (only the subscriber's deliver; the unknown handle is a no-op)", len(got))
	}
	if got[0].sessionID != "sess-a" || got[0].kind != opDeliver {
		t.Fatalf("dispatch = %+v, want {sess-a, deliver}", got[0])
	}
}

// Case 12 (design.md:516, consumer.go:319-340): a handle mentioned in TWO separate
// text blocks steers exactly ONCE. mentionHandles dedupes globally across the block
// set, so the mentioned member's session receives a single steer, not one per block.
func TestMultiBlockMentionDedupsToOneSteer(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.subscribers[ch] = []store.AccountID{agentA}
	reads.members[ch] = []store.AccountID{agentA}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	res.bind(agentA, "sess-a")
	startConsumer(t, c)

	// Same @aa in two distinct blocks: global dedup must collapse to one steer.
	c.bus.Publish(postedResponse(wireTextBlocks("m1", author, "hey @aa", "again @aa")))
	disp.waitForDispatches(t, 1)

	got := disp.snapshot()
	if len(got) != 1 {
		t.Fatalf("dispatches = %d, want 1 (@aa across two blocks steers once)", len(got))
	}
	if got[0].sessionID != "sess-a" || got[0].messageID != "m1" || got[0].kind != opSteer {
		t.Fatalf("dispatch = %+v, want {sess-a, m1, steer}", got[0])
	}
}

// Case 13: a mentioned agent member that is NOT subscribed AND has NO live
// session gets NO IMMEDIATE DISPATCH this cycle — no steer (no turn to interrupt)
// and it is NOT folded into the plain deliver fan-out (it is the mentioned agent,
// not a subscriber). Only the subscribed live member gets its deliver.
//
// This pins the fan-out-time contract: an offline mentioned member never receives
// a synchronous steer or deliver. Its redelivery is the RIG-1641 owed-mention arm
// — routeMentions records a durable owed row (out-of-sweep-set members) and wakes
// it, and the start-edge sweep re-steers the owed mention on resume — covered in
// offline_mention_test.go, not here. This test wires no waker (the nil-safe wake
// is a no-op) and asserts only the dispatch layer, so it stays scoped to the
// no-immediate-dispatch invariant. Contrast Case 6, whose mentioned-offline agent
// WAS subscribed, so the cursor sweep is its backstop.
func TestUnsubscribedOfflineMentionedMemberGetsNoImmediateDispatch(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA, agentB store.AccountID = "agent-a", "agent-b"

	// agentA is a member (so mention-resolvable + a steer target when live) but is
	// NOT subscribed and is offline; agentB is subscribed and live.
	reads.members[ch] = []store.AccountID{agentA, agentB}
	reads.subscribers[ch] = []store.AccountID{agentB}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	res.bind(agentB, "sess-b") // agentA is offline — never bound.
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", author, "@aa ping")))
	disp.waitForDispatches(t, 1)

	got := disp.snapshot()
	if len(got) != 1 {
		t.Fatalf("dispatches = %d, want 1 (only B's deliver; the unsubscribed-offline mentioned member gets no immediate dispatch)", len(got))
	}
	if a := recordsFor(got, "sess-a"); len(a) != 0 {
		t.Fatalf("sess-a records = %+v, want none (offline mentioned member must not steer)", a)
	}
	if got[0].sessionID != "sess-b" || got[0].kind != opDeliver {
		t.Fatalf("dispatch = %+v, want {sess-b, deliver} (agentA folded into neither steer nor deliver)", got[0])
	}
}
