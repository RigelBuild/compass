//go:build podman

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runner"
	"github.com/RigelBuild/compass/go/internal/store"
)

// spawnToolName is the registered name of the native spawn tool the agent's SDK
// exposes — the tool the canned script issues as a tool-call so the agent's loop
// runs Lifecycle(Spawn). Grounded firsthand against the compass-agent TS side:
// createLifecycleTools registers it as "agents_spawn_peer"
// (packages/compass-agent/src/lifecycle.ts:144).
const spawnToolName = "agents_spawn_peer"

// mentionMarker is a stable substring of the leg-4 @-mention body (mentionText,
// defined at the leg-4 post below) used to route BOTH mention-driven turns — the
// mentioned peer's steer-driven turn and the subscribed spawner's deliver-driven
// turn — off the positional canned script via WithCannedMarkerReply, so the
// spawner's 2-turn spawn+settle script stays drawn only by its own turns. It
// must stay a substring of mentionText, asserted at the split assertion.
const mentionMarker = "please take a look"

// TestLegThreeFourSpawnAndMessaging is the full legs-3/4 scenario over the real
// stack (design.md:634-665): a canned turn issues Lifecycle(Spawn) so a fresh
// peer agent + its own container come up (leg 3), then a human posts an
// @mention and the mentioned peer gets a STEER while an unmentioned subscriber
// gets a DELIVER (leg 4). Modeled EXACTLY on TestLegTwoRealTurn: //go:build
// podman, the podmanUsable() skip guard first, context.Background() as the test
// root, NewFixture(ctx, t, WithCannedScript(...)), container-reaping t.Cleanup
// registered before the settling wait, and store-side assertions via
// store.Open(ctx, f.DSN()).
//
// It is PRESENT-BUT-SKIPPED (RED) on the bare stack today, exactly as
// TestLegTwoRealTurn was on H2: the full H3 agent-lane (native tool
// registration + the headless approval policy, #202 gaps 1+3, plus the agent
// image) is unmerged, so the scripted spawn tool-call resolves to "unknown
// tool" and the peer never comes up (design.md:655-659). That is the intended
// RED state; podmanUsable() SKIPs it in a container-less sandbox. Every wait it
// makes is ctx-bounded (AwaitTurnSettled / AwaitDelivery) — no sleeps, no
// polling, no retries.
func TestLegThreeFourSpawnAndMessaging(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into NewFixture + every primitive

	// The peer the scripted spawn mints: a unique handle the leg-3 assertions
	// resolve the fresh account and its container by. The peer stays idle by
	// default (no live-model egress: the peer has no canned backend of its own;
	// it simply provisions and idles, like the leg-2 primitives path).
	const peerHandle = "leg34-peer"
	const peerDisplayName = "Leg Three-Four Peer"
	// The spawn tool's arguments, serialized JSON (the OpenAI tool-call
	// contract). Built from the consts above so the minted handle/display name
	// cannot drift from what the leg-3 assertions resolve. Field names are the
	// spawnParameters wire schema (lifecycle.ts): handle, display_name.
	spawnArgsJSON := fmt.Sprintf(
		`{"handle":%q,"display_name":%q}`,
		peerHandle, peerDisplayName,
	)
	// The assistant text the closing turn settles on, asserted present in the
	// spawner's transcript (the same transcript-contains-canned-reply proof as
	// leg-2).
	const settleReply = "peer spawned, standing by"

	// A 2-turn script: turn 0 issues the spawn tool-call (so the agent loop runs
	// Lifecycle(Spawn)); turn 1 is a clean text settle after the tool result
	// returns. The leg-4 @-mention (mentionText) drives a turn on the mentioned
	// peer (via steer) AND on the subscribed spawner (via deliver), both dialing
	// this shared backend; routing them off the positional script with a marker
	// on the mention body keeps the 2-turn script above drawn ONLY by the
	// spawner's own spawn+settle turns. mentionMarker is a stable substring of
	// mentionText (asserted below); off-script marker turns settle cleanly and
	// carry no assertion, so their reply text only needs to settle.
	f := NewFixture(ctx, t,
		WithCannedScript(
			CannedToolCall(spawnToolName, spawnArgsJSON),
			CannedText(settleReply),
		),
		WithCannedMarkerReply(mentionMarker, "canned mention turn settled OK"),
	)

	// The spawner agent: created, provisioned, and started exactly as leg-2. Its
	// turn issues the spawn.
	spawnerID, err := f.CreateAgent(ctx, "leg34-spawner", "Leg Three-Four Spawner")
	if err != nil {
		t.Fatalf("CreateAgent (spawner): %v", err)
	}

	spawnerContainer, err := f.Provision(ctx, spawnerID, "leg34-spawner-provision")
	if err != nil {
		t.Fatalf("Provision (spawner): %v", err)
	}
	// Reap the spawner's container (see TestLegTwoRealTurn): the reparented
	// rootless conmon outlives stack Down, so an explicit RemoveWorkspace is the
	// only reliable reap. Registered before StartSession so a later failure still
	// tears it down. Best-effort — teardown, not an assertion — so the error is
	// deliberately discarded.
	t.Cleanup(func() {
		_ = f.RemoveWorkspace(ctx, spawnerContainer, "leg34-spawner-teardown")
	})
	// Reap the SPAWNED peer's container too. The spawn mints it under the
	// deterministic name (AgentContainerNamePrefix + the peer account id), and it
	// outlives stack Down the same way, so reap it by that exact name. Registered
	// now (before the turn) so it is torn down even if an assertion below
	// t.Fatals. Best-effort — the peer account id is resolved inside, and if the
	// spawn never happened (the RED path) there is simply nothing to remove.
	t.Cleanup(func() {
		st, err := store.Open(ctx, f.DSN())
		if err != nil {
			return
		}
		defer st.Close()
		peer, err := adminAgentByHandle(ctx, st, peerHandle)
		if err != nil {
			return
		}
		_ = f.RemoveWorkspace(ctx, runner.AgentContainerNamePrefix+string(peer.ID), "leg34-peer-teardown")
	})

	sessionID, err := f.StartSession(ctx, spawnerContainer)
	if err != nil {
		t.Fatalf("StartSession (spawner): %v", err)
	}

	st, err := store.Open(ctx, f.DSN())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	// Open the spawner session tail BEFORE the post drives the turn: the frame
	// stream is live-fan with no replay ring, so opening it first (mirroring the
	// deliver-side "Open one subscription before the post" below) guarantees it
	// is already tailing when the turn fans, rather than losing a fast canned
	// turn's WORKING/READY edges into the post→subscribe gap.
	tail, err := f.OpenSessionTail(ctx, sessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail (spawner): %v", err)
	}
	defer tail.Close()

	// Post to the SPAWNER's home channel: this post lands on the already-live
	// session and is delivered via the live fan-out (the delivery consumer
	// tailing the comms bus), which fires the spawner's first turn (the one that
	// issues the spawn tool-call) — the turn AwaitTurnSettled waits on. The
	// session-start sweep only redelivers messages left undelivered from a prior
	// lifetime (relevant only to leg-5's post1), not this one. Must precede the
	// settle wait.
	spawner, err := adminAgentByHandle(ctx, st, "leg34-spawner")
	if err != nil {
		t.Fatalf("AgentByHandle(spawner): %v", err)
	}
	if _, err := f.PostMessage(ctx, string(spawner.Agent.HomeChannelID), "general", "spawn a peer and stand by"); err != nil {
		t.Fatalf("PostMessage(home): %v", err)
	}

	// Event-gated settle on the spawner's already-open tail: skip until WORKING,
	// then return on the next READY (WORKING→READY = one settled turn) — the
	// scripted spawn tool-call executes and the closing text turn settles — no
	// sleeps.
	if err := f.AwaitTurnSettled(ctx, tail); err != nil {
		t.Fatalf("AwaitTurnSettled (spawner): %v", err)
	}

	// ── Leg 3: fresh peer account (F2 ownership) + a second real container ──

	// The spawn minted a fresh agent account resolvable by its handle.
	peer, err := adminAgentByHandle(ctx, st, peerHandle)
	if err != nil {
		t.Fatalf("AgentByHandle(%q): %v — the scripted spawn did not mint the peer account (RED until H3 registers the native spawn tool, design.md:655-659)", peerHandle, err)
	}
	if !peer.IsAgent() {
		t.Fatalf("resolved account %q is not an agent", peerHandle)
	}
	// F2 ownership: the peer inherits the spawner's owner (SpawnPeerRequest is
	// owned by the caller agent's owner, agent_gateway.proto:135). The spawner is
	// itself an agent, so resolve its owner and assert the peer shares it.
	spawnerOwner, err := st.AgentOwner(ctx, store.AccountID(spawnerID))
	if err != nil {
		t.Fatalf("AgentOwner(spawner): %v", err)
	}
	if peer.Agent.OwnerUserID != spawnerOwner {
		t.Fatalf("peer owner = %q, want the spawner's owner %q (F2 ownership inheritance)", peer.Agent.OwnerUserID, spawnerOwner)
	}

	// A SECOND real container exists, named by the deterministic
	// NamePrefix + accountID rule (spec.go:85; NamePrefix ==
	// AgentContainerNamePrefix, run.go:48). Assert it is present in podman's
	// container set by that exact name — the observable outcome, mirroring leg-2's
	// transcript-contains-canned-reply proof.
	peerContainer := runner.AgentContainerNamePrefix + string(peer.ID)
	if peerContainer == spawnerContainer {
		t.Fatalf("peer container name %q collides with the spawner's — the deterministic name did not derive from the fresh account id", peerContainer)
	}
	// The peer container is actually PRESENT in podman's runtime set by that
	// exact name — the observable leg-3 outcome (design.md:660 "second
	// container inspected by exact name"), probed with a dependency-free
	// `podman container exists`, the same check the runtime suite uses
	// (runtime/lifecycle_test.go:77) and the same direct podman-shelling this
	// package already relies on (podmanUsable, fixture.go:397). This is what
	// tells "spawn minted the account AND its container came up" apart from
	// "account minted, container never started" — the name derivation asserted
	// above cannot distinguish those.
	if exec.Command("podman", "container", "exists", peerContainer).Run() != nil {
		t.Fatalf("peer container %q is not present in podman's runtime set — the scripted spawn minted the account but its container did not come up (RED until H3's agent image carries the native lane, design.md:655-659)", peerContainer)
	}

	// ── Leg 4: post an @mention → mentioned member steers, subscriber delivers ─

	// Open one subscription before the post so it sees the live fan of the
	// deliver-side MessagePosted event. sinceSeq 0 snapshots then tails. (The
	// recipient-side steer/deliver split is the deferred TODO(SEA-1788) below,
	// not observed here.)
	sub, err := f.SubscribeComms(ctx, 0)
	if err != nil {
		t.Fatalf("SubscribeComms: %v", err)
	}
	defer sub.Close()

	// Post into the peer's home channel, @mentioning the peer's handle. The
	// mention grammar is `@` + handle (consumer.go:310-318), so "@leg34-peer"
	// routes to the peer. The channel is the peer's home channel (minted at
	// CreateAgent); the topic is the channel's home topic "general".
	const mentionText = "@leg34-peer please take a look"
	messageID, err := f.PostMessage(ctx, string(peer.Agent.HomeChannelID), "general", mentionText)
	if err != nil {
		t.Fatalf("PostMessage(@mention): %v", err)
	}
	if messageID == "" {
		t.Fatal("PostMessage returned an empty message id")
	}

	// Deliver-side observation: the post fans onto the subscription as a
	// MessagePosted carrying the exact text — event-gated via AwaitDelivery, no
	// polling.
	delivered, err := f.AwaitDelivery(ctx, sub, func(m *compassv1.Message) bool {
		return m.GetId() == messageID
	})
	if err != nil {
		t.Fatalf("AwaitDelivery: %v", err)
	}
	if got := firstBlockText(delivered); got != mentionText {
		t.Fatalf("delivered message text = %q, want the exact @mention body %q", got, mentionText)
	}

	// ── Leg-4 recipient-side steer/deliver SPLIT (RIG-2488, replaces the former
	// TODO(SEA-1788)) ──────────────────────────────────────────────────────────
	//
	// The mentioned peer's live session must receive a STEER for a mention while
	// an unmentioned-but-subscribed member receives a plain DELIVER of the same
	// message (steer-only precedence, design.md:537-546). The second recipient is
	// the REUSED leg-3 spawner (no third container): it is subscribed onto the
	// peer's home channel below so it joins the deliver set (SubscribedAgents,
	// delivery_reads.go — subscribe, not a bare add, because a non-home,
	// non-mandatory member must carry the subscribed flag to be a deliver target).
	// The post's author is the fixture human admin, so neither agent is excluded.
	//
	// This drives its OWN @-mention post (a second one), because the observation
	// tails must be opened BEFORE the post that drives the dispatch: the frame
	// stream is live-fan with no replay ring (AwaitControlDispatch doc,
	// legthreefour_test.go OpenSessionTail note), so a post-first order would race
	// the injection frame and only fail at settleTimeout.

	// mentionMarker (routed off the canned script) must be a substring of the
	// mention body, else the mention-driven turns would draw the positional
	// script and desync the spawner's spawn+settle turns.
	if !strings.Contains(mentionText, mentionMarker) {
		t.Fatalf("mentionMarker %q is not a substring of mentionText %q — the off-script routing would not catch the mention-driven turns", mentionMarker, mentionText)
	}

	// Subscribe the spawner onto the peer's home channel so it becomes a plain
	// deliver target there (the second recipient, no new container).
	// The spawner is a bare agent handle in the caller's own owner namespace
	// (created via CreateAgent above); T3 resolves it to the spawner account.
	if err := f.SubscribeMember(ctx, string(peer.Agent.HomeChannelID), "leg34-spawner"); err != nil {
		t.Fatalf("SubscribeMember(spawner → peer home channel): %v", err)
	}

	// Resolve the peer's session id BEFORE the post (the spawner's session id is
	// the sessionID resolved at StartSession above). The peer session was
	// recorded when the spawn started it; the durable read resolves it whether or
	// not the peer is mid-turn.
	peerSessionID, ok, err := st.LatestSessionForAccount(ctx, peer.ID)
	if err != nil {
		t.Fatalf("LatestSessionForAccount(peer): %v", err)
	}
	if !ok {
		t.Fatalf("peer %q has no recorded session — the spawn did not start the peer's session", peerHandle)
	}

	// injectionLog accumulates every (opKind, messageID) a recipient's tail is
	// offered, under a mutex because match runs on AwaitControlDispatch's pump
	// goroutine. It is BOTH the positive-match predicate and the window-scoped
	// exclusion retention hook: the window closes at the positive match (when
	// AwaitControlDispatch returns), and after the call the accumulated frames are
	// inspected to assert the excluded op-kind never appeared for the target id
	// within that window. Exclusion is deliberately window-scoped, NOT absolute: a
	// steered-but-unacked message can be sweep-redelivered as a plain deliver
	// LATER (settle.go:241 builds a deliverOp for every owed message), which is
	// out of window and legitimately unobserved.
	type injectionLog struct {
		mu     sync.Mutex
		frames []struct{ opKind, messageID string }
	}
	// recordAndMatch returns a match closure that retains every offered frame and
	// resolves true once an op-kind CONTAINING wantKind (a stable substring of the
	// SessionInjectionKind string form, e.g. "STEER"/"DELIVER") is offered for
	// splitMsgID — the positive match that closes the window.
	recordAndMatch := func(log *injectionLog, wantKind string, splitMsgID *atomic.Pointer[string]) func(opKind, messageID string) bool {
		return func(opKind, messageID string) bool {
			log.mu.Lock()
			log.frames = append(log.frames, struct{ opKind, messageID string }{opKind, messageID})
			log.mu.Unlock()
			want := splitMsgID.Load()
			return want != nil && messageID == *want && strings.Contains(opKind, wantKind)
		}
	}

	// splitMsgID is set AFTER the post returns the message id, but the tails must
	// be observing BEFORE the post — so the match closures read it via an atomic
	// pointer that is nil until the post lands. A frame that arrives before the id
	// is known cannot match (want == nil), which is correct: the driven injection
	// for this post cannot precede the post.
	var splitMsgID atomic.Pointer[string]
	peerLog := &injectionLog{}
	spawnerLog := &injectionLog{}

	type dispatchResult struct {
		opKind string
		err    error
	}
	peerResult := make(chan dispatchResult, 1)
	spawnerResult := make(chan dispatchResult, 1)

	// Open BOTH observations (each opens its own tail internally via
	// AwaitControlDispatch) from goroutines, launched BEFORE the post. This is
	// concurrent-with-the-post by necessity, not a launch-and-hope: the tail is a
	// live fan with no replay ring (AwaitControlDispatch doc, sessiontail.go), and
	// OpenSessionTail's client call blocks in its first round-trip until the
	// server flushes a frame. The peer wakes on the mention, but the SPAWNER is
	// idle here (settled after leg-3) and flushes nothing until the post drives
	// its DELIVER — so its tail open cannot complete BEFORE the post; the post is
	// what unblocks it. Opening synchronously on this goroutine before the post
	// would therefore self-deadlock (the frame that unblocks the open is sequenced
	// after it). Running the opens concurrently with the post is the correct gate:
	// each open returns on a server frame, which the server emits only after
	// registering the subscription, so the driven injection is observed, not raced
	// away. A genuinely missed frame is a fail-safe settleTimeout red (the excluded
	// -kind assertion only weakens if frames are missed — there is no false-green).
	go func() {
		kind, err := f.AwaitControlDispatch(ctx, peerSessionID, recordAndMatch(peerLog, "STEER", &splitMsgID))
		peerResult <- dispatchResult{kind, err}
	}()
	go func() {
		kind, err := f.AwaitControlDispatch(ctx, sessionID, recordAndMatch(spawnerLog, "DELIVER", &splitMsgID))
		spawnerResult <- dispatchResult{kind, err}
	}()

	// Now post the @-mention into the peer's home channel. The mention steers the
	// peer and delivers to the subscribed spawner. Its body carries mentionMarker,
	// so both mention-driven model turns route off the positional canned script.
	splitMessageID, err := f.PostMessage(ctx, string(peer.Agent.HomeChannelID), "general", mentionText)
	if err != nil {
		t.Fatalf("PostMessage(split @mention): %v", err)
	}
	if splitMessageID == "" {
		t.Fatal("PostMessage(split @mention) returned an empty message id")
	}
	splitMsgID.Store(&splitMessageID)

	// Join both observations. Each returns on its positive match (or fails at its
	// derived settleTimeout).
	peerDispatch := <-peerResult
	if peerDispatch.err != nil {
		t.Fatalf("AwaitControlDispatch(peer, want STEER): %v", peerDispatch.err)
	}
	spawnerDispatch := <-spawnerResult
	if spawnerDispatch.err != nil {
		t.Fatalf("AwaitControlDispatch(spawner, want DELIVER): %v", spawnerDispatch.err)
	}

	// Positive: peer steered, spawner delivered, same message id.
	if !strings.Contains(peerDispatch.opKind, "STEER") {
		t.Fatalf("peer op-kind = %q, want a STEER for the mentioned peer", peerDispatch.opKind)
	}
	if !strings.Contains(spawnerDispatch.opKind, "DELIVER") {
		t.Fatalf("spawner op-kind = %q, want a DELIVER for the unmentioned subscriber", spawnerDispatch.opKind)
	}

	// Window-scoped exclusion: within each recipient's observation window (up to
	// its positive match), the peer saw NO deliver and the spawner NO steer for
	// this message id. NOT an absolute negative — a later sweep-redelivered
	// deliver of a steered-unacked message (settle.go:241) is out of window.
	assertExcluded := func(log *injectionLog, excludedKind string, who string) {
		log.mu.Lock()
		defer log.mu.Unlock()
		for _, fr := range log.frames {
			if fr.messageID == splitMessageID && strings.Contains(fr.opKind, excludedKind) {
				t.Fatalf("%s observed an excluded %s (op-kind %q) for message %q within the window — steer-only precedence violated", who, excludedKind, fr.opKind, splitMessageID)
			}
		}
	}
	assertExcluded(peerLog, "DELIVER", "mentioned peer")
	assertExcluded(spawnerLog, "STEER", "unmentioned spawner")
}

// firstBlockText returns the first non-empty text block of a wire Message, the
// deliver-side text accessor mirroring integration_pgtest_test.go:462 firstText
// over the *compassv1.Message the SubscribeComms stream (and AwaitDelivery)
// threads through — kept local so comms_ops.go's primitive stays a thin RPC with
// no block-walking.
func firstBlockText(m *compassv1.Message) string {
	for _, b := range m.GetBlocks() {
		if t := b.GetText(); t != "" {
			return t
		}
	}
	return ""
}
