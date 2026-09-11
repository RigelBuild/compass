//go:build podman

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// The two container agents this leg stands up, plus the shared channel and the
// topic their conversation lands on. Handles obey the account handle grammar
// (store/handle.go `^[a-z0-9][a-z0-9._-]*$`) and every handle/channel/topic is
// duo- namespaced: this leg shares ONE stack with the other comms legs, so a
// bare name would collide with theirs.
const (
	duoAgentAHandle = "duo-agent-a"
	duoAgentBHandle = "duo-agent-b"
	duoRoomChannel  = "duo-room"
	duoRoomTopic    = "duo-topic"
	duoDMTopic      = "duo-dm-topic"
)

// The two marker routes, one per agent, each carrying that agent's WHOLE ordered
// turn script (RIG-3528 T1 marker scripts). Routing by marker — never the
// positional script — is mandatory on the shared stack: the positional counter
// is a single contended resource two legs drawing unmarked turns would race.
//
// duoMarkerA is a substring of every human trigger posted to A (never of
// anything A posts), so it appears in every one of A's model requests and in NO
// other agent's. duoMarkerB is a substring of the content A posts that B
// RECEIVES (the room post and the DM), so it appears in B's requests.
//
// The registration ORDER below is load-bearing: A authors the content carrying
// duoMarkerB (its own tool-call args), so duoMarkerB is also present in A's
// request history — but duoMarkerA is present in the SAME requests and, matched
// first, wins. B's requests never carry duoMarkerA, so they fall through to
// duoMarkerB. Registering A before B is what disambiguates the two.
const (
	duoMarkerA = "duo-drive-alpha"
	duoMarkerB = "duo-relay-beta"
)

// The distinctive message bodies the fan-out assertions select on. duoRoomPost
// and duoDM both carry duoMarkerB (they are what B receives, so they must route
// B's turns); duoReply carries neither marker (nothing is triggered by B's
// reply — A is not in the room's deliver set). Each is unique so an AwaitDelivery
// match cannot collide with a trigger post or the other agent's message.
const (
	duoRoomPost = duoMarkerB + " channel post authored by agent A"
	duoReply    = "duo agent B reply on the shared room topic"
	duoDM       = duoMarkerB + " direct message authored by agent A"
)

// The clean text turns each tool-call turn settles on after its tool result
// returns. Their content is never asserted — the comms fan-out + author checks
// below are the strictly stronger proof the tools ran — they only give each turn
// something to settle on.
const (
	duoSettleARoom = "duo agent A standing by after the room post"
	duoSettleADM   = "duo agent A standing by after the dm"
	duoSettleBRoom = "duo agent B standing by after the reply"
)

func init() {
	// A's ordered script across BOTH its actions: post to the room (tool-call +
	// settle), then DM the peer (open_dm, dm, settle). A tool-call turn needs a
	// SECOND round-trip to terminate, so every tool-call turn is paired with a
	// following turn; the per-marker counter advances one slot per round-trip.
	postArgs := fmt.Sprintf(
		`{"text":%q,"topic":%q,"channel":%q,"create_topic":true}`,
		duoRoomPost, duoRoomTopic, duoRoomChannel,
	)
	openDMArgs := fmt.Sprintf(`{"peer_handle":%q}`, duoAgentBHandle)
	dmArgs := fmt.Sprintf(
		`{"peer_handle":%q,"text":%q,"topic":%q,"create_topic":true}`,
		duoAgentBHandle, duoDM, duoDMTopic,
	)
	// B's ordered script: reply into the SAME room topic (tool-call + settle).
	// B's later DM-receipt turn draws no scripted slot — the marker route clamps
	// to its terminal text turn and settles B cleanly without a room reply.
	replyArgs := fmt.Sprintf(
		`{"text":%q,"topic":%q,"channel":%q,"create_topic":false}`,
		duoReply, duoRoomTopic, duoRoomChannel,
	)

	// A registered BEFORE B: see the duoMarkerA/duoMarkerB comment above.
	registerSharedFixtureOption(
		WithCannedMarkerScript(duoMarkerA,
			CannedToolCall("comms_post_message", postArgs),
			CannedText(duoSettleARoom),
			CannedToolCall("comms_open_dm", openDMArgs),
			CannedToolCall("comms_dm", dmArgs),
			CannedText(duoSettleADM),
		),
		WithCannedMarkerScript(duoMarkerB,
			CannedToolCall("comms_post_message", replyArgs),
			CannedText(duoSettleBRoom),
		),
	)
}

// TestCommsTwoAgentConversation proves two live container agents converse over
// the real stack: A posts into a shared channel B is subscribed to, B receives
// the deliver and replies on the same topic, then A DMs B and B receives it.
// Both agents' native comms tools (comms_post_message, comms_open_dm, comms_dm)
// run through their real agent loops against the live Server, and every effect is
// observed cross-process — the fan-out on the comms bus (author-attributed) and
// the control-plane deliver on each session's own frame tail.
//
// It runs on the SHARED fixture: one stack.Up for every comms leg. Both
// agents route ALL their turns by marker so neither races the positional script.
// podmanUsable() SKIPs it in a container-less sandbox, matching every sibling
// leg. Every wait is event-gated and ctx-bounded (AwaitTurnSettled /
// AwaitDelivery / AwaitControlDispatchOn) — no sleeps, no polling, no retries.
func TestCommsTwoAgentConversation(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into sharedFixture + every primitive

	f := sharedFixture(t)

	// Each container's reap is registered BEFORE its StartSession: the reparented
	// rootless conmon outlives stack Down, so RemoveWorkspace is the only
	// reliable reap, and registering early survives a later t.Fatal.
	agentAID, err := f.CreateAgent(ctx, duoAgentAHandle, "Duo Agent A")
	if err != nil {
		t.Fatalf("CreateAgent (A): %v", err)
	}
	containerA, err := f.Provision(ctx, agentAID, "duo-agent-a-provision")
	if err != nil {
		t.Fatalf("Provision (A): %v", err)
	}
	t.Cleanup(func() {
		_ = f.RemoveWorkspace(ctx, containerA, "duo-agent-a-teardown") // best-effort reap
	})
	sessionA, err := f.StartSession(ctx, containerA)
	if err != nil {
		t.Fatalf("StartSession (A): %v", err)
	}

	agentBID, err := f.CreateAgent(ctx, duoAgentBHandle, "Duo Agent B")
	if err != nil {
		t.Fatalf("CreateAgent (B): %v", err)
	}
	containerB, err := f.Provision(ctx, agentBID, "duo-agent-b-provision")
	if err != nil {
		t.Fatalf("Provision (B): %v", err)
	}
	t.Cleanup(func() {
		_ = f.RemoveWorkspace(ctx, containerB, "duo-agent-b-teardown") // best-effort reap
	})
	sessionB, err := f.StartSession(ctx, containerB)
	if err != nil {
		t.Fatalf("StartSession (B): %v", err)
	}

	st, err := store.Open(ctx, f.DSN())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	// Resolve A's home channel (the trigger posts land there to drive A's turns).
	agentA, err := adminAgentByHandle(ctx, st, duoAgentAHandle)
	if err != nil {
		t.Fatalf("AgentByHandle(A): %v", err)
	}

	// B joins SUBSCRIBED so it lands in the room's deliver set. A stays a bare
	// member, excluded from its own deliver, so it never receives B's reply and
	// drives no stray turn.
	roomID, err := f.CreateChannel(ctx, agentAID, duoRoomChannel, false)
	if err != nil {
		t.Fatalf("CreateChannel(%q): %v", duoRoomChannel, err)
	}
	if err := f.SubscribeMember(ctx, roomID, duoAgentBHandle); err != nil {
		t.Fatalf("SubscribeMember(B → room): %v", err)
	}

	// Opened BEFORE any trigger: OpenSessionTail returns on the registration-ack,
	// a server-guaranteed happens-before, so a fast turn's edges cannot fan into
	// a subscribe gap. B's single tail carries both deliver observations.
	tailA, err := f.OpenSessionTail(ctx, sessionA)
	if err != nil {
		t.Fatalf("OpenSessionTail (A): %v", err)
	}
	defer tailA.Close()
	tailB, err := f.OpenSessionTail(ctx, sessionB)
	if err != nil {
		t.Fatalf("OpenSessionTail (B): %v", err)
	}
	defer tailB.Close()
	sub, err := f.SubscribeComms(ctx, 0)
	if err != nil {
		t.Fatalf("SubscribeComms: %v", err)
	}
	defer sub.Close()

	// ── Channel post → deliver → reply ──────────────────────────────────────

	// Drive A's room-post turn: the trigger lands on A's live home session and
	// fires the marker-routed turn that issues comms_post_message into the room.
	if _, err := f.PostMessage(ctx, string(agentA.Agent.HomeChannelID), "general", duoMarkerA+": post to the shared room and stand by"); err != nil {
		t.Fatalf("PostMessage(A room trigger): %v", err)
	}
	if err := f.AwaitTurnSettled(ctx, tailA); err != nil {
		t.Fatalf("AwaitTurnSettled (A room post): %v", err)
	}

	// A's post fanned out on the comms bus, authored by A itself (not the human
	// trigger). Selecting on the distinctive body isolates A's own post.
	roomPost, err := f.AwaitDelivery(ctx, sub, func(m *compassv1.Message) bool {
		// mutation: catches comms_post_message never running through A's loop (no
		// agent-authored post fans out, so AwaitDelivery times out).
		return firstBlockText(m) == duoRoomPost
	})
	if err != nil {
		t.Fatalf("AwaitDelivery(A room post): %v — the scripted comms_post_message did not fan out", err)
	}
	// mutation: catches the post landing under the wrong author (the human
	// trigger's identity, or empty) — it must be authored by agent A.
	if got := roomPost.GetAuthorAccountId(); got != agentAID {
		t.Fatalf("room post author = %q, want agent A's account id %q", got, agentAID)
	}
	roomPostID := roomPost.GetId()

	// B, the subscribed member, RECEIVED the post: its live session dispatched a
	// DELIVER control for that exact message id. The message id is already known,
	// so the match closure captures it directly (no atomic needed — the pump only
	// starts inside this call, after the id is set).
	if _, err := f.AwaitControlDispatchOn(ctx, tailB, func(kind, mid string) bool {
		// mutation: catches B never receiving the post (no DELIVER for the id ever
		// dispatches to B's session, so the wait times out).
		return mid == roomPostID && strings.Contains(kind, "DELIVER")
	}); err != nil {
		t.Fatalf("AwaitControlDispatchOn(B, room post DELIVER): %v", err)
	}

	// B's deliver-driven turn replied on the SAME topic, authored by B.
	reply, err := f.AwaitDelivery(ctx, sub, func(m *compassv1.Message) bool {
		// mutation: catches B's reply turn never running (the deliver did not
		// drive B's marker-routed comms_post_message, so no reply fans out).
		return firstBlockText(m) == duoReply
	})
	if err != nil {
		t.Fatalf("AwaitDelivery(B reply): %v — B's marker-routed reply did not fan out", err)
	}
	// mutation: catches the reply landing under the wrong author — it must be
	// authored by agent B, proving B (not A) posted it.
	if got := reply.GetAuthorAccountId(); got != agentBID {
		t.Fatalf("reply author = %q, want agent B's account id %q", got, agentBID)
	}
	// mutation: catches the reply landing on a different topic — it must share A's
	// post's topic, which is what makes it a reply in the conversation.
	if got := reply.GetTopicId(); got != roomPost.GetTopicId() {
		t.Fatalf("reply topic = %q, want the room post's topic %q (same-topic reply)", got, roomPost.GetTopicId())
	}

	// ── The DM crossing A → B ────────────────────────────────────────────────

	// Drive A's DM turn: the second trigger advances A's marker script to its
	// open_dm → dm → settle tail, so A resolves-or-creates a DM with B and posts.
	if _, err := f.PostMessage(ctx, string(agentA.Agent.HomeChannelID), "general", duoMarkerA+": now direct-message the peer"); err != nil {
		t.Fatalf("PostMessage(A dm trigger): %v", err)
	}
	if err := f.AwaitTurnSettled(ctx, tailA); err != nil {
		t.Fatalf("AwaitTurnSettled (A dm): %v", err)
	}

	// The DM fanned out on the bus authored by A — this yields the DM message id.
	dmMsg, err := f.AwaitDelivery(ctx, sub, func(m *compassv1.Message) bool {
		// mutation: catches comms_dm never running through A's loop (no DM message
		// fans out, so the wait times out).
		return firstBlockText(m) == duoDM
	})
	if err != nil {
		t.Fatalf("AwaitDelivery(A dm): %v — the scripted comms_dm did not fan out", err)
	}
	if got := dmMsg.GetAuthorAccountId(); got != agentAID {
		t.Fatalf("dm author = %q, want agent A's account id %q", got, agentAID)
	}
	dmID := dmMsg.GetId()

	// B — the live agent on the other side — RECEIVED the DM: its session
	// dispatched a DELIVER control for the DM message id. This is the crossing
	// asserted as delivery between two live agents, the part needing two
	// containers.
	if _, err := f.AwaitControlDispatchOn(ctx, tailB, func(kind, mid string) bool {
		// mutation: catches the DM never reaching B (no DELIVER for the DM id ever
		// dispatches to B's session, so the wait times out).
		return mid == dmID && strings.Contains(kind, "DELIVER")
	}); err != nil {
		t.Fatalf("AwaitControlDispatchOn(B, dm DELIVER): %v", err)
	}
}
