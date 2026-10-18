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

// spawnToolName is the SDK's registered native spawn tool.
const spawnToolName = "agents_spawn_peer"

// mentionMarker routes both mention-driven turns off the spawner's ordered
// script. It must occur in mentionText, checked below.
const mentionMarker = "e2e-route-leg34-mention"

// TestLegThreeFourSpawnAndMessaging runs the spawn and messaging scenarios on
// the real stack: spawn creates a peer and container; a later @mention steers
// that peer and delivers to an unmentioned subscriber. Marker scripts isolate
// each leg's turns, and every wait is ctx-bounded.
func TestLegThreeFourSpawnAndMessaging(t *testing.T) {
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into every primitive

	f := sharedFixture(t)

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
	// Reap the spawner container because rootless conmon outlives stack Down.
	// Register before StartSession so failures still clean it up.
	t.Cleanup(func() {
		_ = f.RemoveWorkspace(ctx, spawnerContainer, "leg34-spawner-teardown")
	})
	// Reap the spawned peer by its deterministic exact name, even if assertions
	// fail. If spawn did not create an account, there is no container to remove.
	t.Cleanup(func() {
		st, err := store.Open(ctx, f.DSN())
		if err != nil {
			return
		}
		defer st.Close()
		peer, err := adminAgentByHandle(ctx, st, leg34PeerHandle)
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

	// Open the spawner tail before the trigger; the live-only stream's registration
	// ack prevents a fast turn's state frames from being missed.
	tail, err := f.OpenSessionTail(ctx, sessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail (spawner): %v", err)
	}
	defer tail.Close()

	// Post to the spawner's live session to trigger its first turn; the session-start
	// sweep applies only to messages left undelivered by a prior lifetime.
	spawner, err := adminAgentByHandle(ctx, st, "leg34-spawner")
	if err != nil {
		t.Fatalf("AgentByHandle(spawner): %v", err)
	}
	if _, err := f.PostMessage(ctx, string(spawner.Agent.HomeChannelID), "general", "spawn a peer and stand by "+leg34SpawnMarker); err != nil {
		t.Fatalf("PostMessage(home): %v", err)
	}

	// Wait for the spawn tool call and closing text turn to settle on the open tail.
	if err := f.AwaitTurnSettled(ctx, tail); err != nil {
		t.Fatalf("AwaitTurnSettled (spawner): %v", err)
	}

	peer, err := adminAgentByHandle(ctx, st, leg34PeerHandle)
	if err != nil {
		t.Fatalf("AgentByHandle(%q): %v — the scripted spawn did not mint the peer account (design.md:655-659)", leg34PeerHandle, err)
	}
	// ── Leg 3: fresh peer account (F2 ownership) + a second real container ──

	// The spawn must create a fresh agent account under the peer handle.
	if !peer.IsAgent() {
		t.Fatalf("resolved account %q is not an agent", leg34PeerHandle)
	}
	// The peer inherits the spawner's owner; assert the stored owner explicitly.
	spawnerOwner, err := st.AgentOwner(ctx, store.AccountID(spawnerID))
	if err != nil {
		t.Fatalf("AgentOwner(spawner): %v", err)
	}
	if peer.Agent.OwnerUserID != spawnerOwner {
		t.Fatalf("peer owner = %q, want the spawner's owner %q (F2 ownership inheritance)", peer.Agent.OwnerUserID, spawnerOwner)
	}

	// The second container name derives deterministically from its account id.
	peerContainer := runner.AgentContainerNamePrefix + string(peer.ID)
	if peerContainer == spawnerContainer {
		t.Fatalf("peer container name %q collides with the spawner's — the deterministic name did not derive from the fresh account id", peerContainer)
	}
	// Assert a container exists under the deterministic name, which distinguishes
	// a spawn that created a container from an account-only spawn.
	if exec.CommandContext(t.Context(), "podman", "container", "exists", peerContainer).Run() != nil {
		t.Fatalf("peer container %q is not present in podman's runtime set — the scripted spawn minted the account but its container did not come up (design.md:655-659)", peerContainer)
	}

	// ── Leg 4: post an @mention → mentioned member steers, subscriber delivers ─

	// Subscribe before the @mention so its delivery event is observed on the live
	// fan. Recipient-side steer/deliver is asserted below.
	sub, err := f.SubscribeComms(ctx, 0)
	if err != nil {
		t.Fatalf("SubscribeComms: %v", err)
	}
	defer sub.Close()

	// Post into the peer's home channel; the @handle grammar routes the mention to
	// the peer, and the home topic is general.
	const mentionText = "@leg34-peer please take a look " + mentionMarker
	messageID, err := f.PostMessage(ctx, string(peer.Agent.HomeChannelID), "general", mentionText)
	if err != nil {
		t.Fatalf("PostMessage(@mention): %v", err)
	}
	if messageID == "" {
		t.Fatal("PostMessage returned an empty message id")
	}

	// Observe the exact mention post on the subscription's live tail.
	delivered, err := f.AwaitDelivery(ctx, sub, func(m *compassv1.Message) bool {
		return m.GetId() == messageID
	})
	if err != nil {
		t.Fatalf("AwaitDelivery: %v", err)
	}
	if got := firstBlockText(delivered); got != mentionText {
		t.Fatalf("delivered message text = %q, want the exact @mention body %q", got, mentionText)
	}

	// The mentioned peer must receive STEER while the subscribed spawner receives
	// DELIVER for the same message. The spawner is reused as the second recipient;
	// open both live-only observation tails before posting so registration acks
	// guarantee neither driven injection can be missed.

	assertLeg34MentionSplit(t, ctx, f, st, peer, sessionID, mentionText)
}

// firstBlockText returns the first non-empty text block without expanding the
// thin RPC helper into message block-walking.
func firstBlockText(m *compassv1.Message) string {
	for _, b := range m.GetBlocks() {
		if t := b.GetText(); t != "" {
			return t
		}
	}
	return ""
}

// The spawned peer stays idle by default and has no model backend of its own.
const leg34PeerHandle = "leg34-peer"
const leg34PeerDisplayName = "Leg Three-Four Peer"

// agents_spawn_peer requires a role and persona. The unconfigured manager role
// uses the default prompt, while persona accepts free text.
const leg34PeerRole = "manager"
const leg34PeerPersona = "Leg-3/4 e2e peer: idle standby, no lane."

// The spawn tool arguments follow lifecycle.ts's JSON schema and derive from
// the constants used by the assertions.
var leg34SpawnArgsJSON = fmt.Sprintf(
	`{"handle":%q,"role":%q,"persona":%q,"display_name":%q}`,
	leg34PeerHandle, leg34PeerRole, leg34PeerPersona, leg34PeerDisplayName,
)

// The closing text is asserted in the spawner's transcript.
const leg34SettleReply = "peer spawned, standing by"

const leg34SpawnMarker = "e2e-route-leg34-spawn"

// Route the spawner's spawn+settle turns by leg34SpawnMarker. Mention-driven
// turns use mentionMarker so they cannot consume that script; marker replies
// only need to settle the turns.
func registerLegThreeFourFixtureOptions() {
	registerSharedFixtureOption(
		WithCannedMarkerScript(leg34SpawnMarker,
			CannedToolCall(spawnToolName, leg34SpawnArgsJSON),
			CannedText(leg34SettleReply),
		),
		WithCannedMarkerReply(mentionMarker, "canned mention turn settled OK"),
	)
}

func assertLeg34MentionSplit(t *testing.T, ctx context.Context, f *Fixture, st *store.Store, peer store.Account, sessionID, mentionText string) {
	t.Helper()
	// Ensure mentionMarker occurs in the post so mention-driven turns stay off the
	// spawn script.
	if !strings.Contains(mentionText, mentionMarker) {
		t.Fatalf("mentionMarker %q is not a substring of mentionText %q — the off-script routing would not catch the mention-driven turns", mentionMarker, mentionText)
	}

	// Subscribe the spawner to the peer's home channel as a plain delivery target.
	if err := f.SubscribeMember(ctx, string(peer.Agent.HomeChannelID), "leg34-spawner"); err != nil {
		t.Fatalf("SubscribeMember(spawner → peer home channel): %v", err)
	}

	// Resolve the peer session recorded during spawn, before posting the mention.
	peerSessionID, ok, err := st.LatestSessionForAccount(ctx, peer.ID)
	if err != nil {
		t.Fatalf("LatestSessionForAccount(peer): %v", err)
	}
	if !ok {
		t.Fatalf("peer %q has no recorded session — the spawn did not start the peer's session", leg34PeerHandle)
	}

	// injectionLog records each offered (opKind, messageID) until the positive
	// match closes the observation window. Exclusion is window-scoped: a later
	// sweep may legitimately deliver an unacknowledged steered message.
	type injectionLog struct {
		mu     sync.Mutex
		frames []struct{ opKind, messageID string }
	}
	// recordAndMatch retains frames and matches the requested kind for splitMsgID.
	recordAndMatch := func(log *injectionLog, wantKind string, splitMsgID *atomic.Pointer[string]) func(opKind, messageID string) bool {
		return func(opKind, messageID string) bool {
			log.mu.Lock()
			log.frames = append(log.frames, struct{ opKind, messageID string }{opKind, messageID})
			log.mu.Unlock()
			want := splitMsgID.Load()
			return want != nil && messageID == *want && strings.Contains(opKind, wantKind)
		}
	}

	// The post id is unknown until PostMessage returns, so early frames cannot match.
	var splitMsgID atomic.Pointer[string]
	peerLog := &injectionLog{}
	spawnerLog := &injectionLog{}

	// Open both tails before posting; their registration acks prove both recipients
	// are subscribed, avoiding a race that could lose a driven injection.
	peerStream, err := f.OpenSessionTail(ctx, peerSessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail(peer): %v", err)
	}
	defer peerStream.Close()
	spawnerStream, err := f.OpenSessionTail(ctx, sessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail(spawner): %v", err)
	}
	defer spawnerStream.Close()

	// Post the mention after both tails are live; its marker routes both turns off
	// the positional spawn script.
	splitMessageID, err := f.PostMessage(ctx, string(peer.Agent.HomeChannelID), "general", mentionText)
	if err != nil {
		t.Fatalf("PostMessage(split @mention): %v", err)
	}
	if splitMessageID == "" {
		t.Fatal("PostMessage(split @mention) returned an empty message id")
	}
	splitMsgID.Store(&splitMessageID)

	// Read the two buffered tails sequentially. Each returns on its positive match;
	// no goroutine is needed and failures remain bounded by settleTimeout.
	peerOpKind, err := f.AwaitControlDispatchOn(ctx, peerStream, recordAndMatch(peerLog, "STEER", &splitMsgID))
	if err != nil {
		t.Fatalf("AwaitControlDispatchOn(peer, want STEER): %v", err)
	}
	spawnerOpKind, err := f.AwaitControlDispatchOn(ctx, spawnerStream, recordAndMatch(spawnerLog, "DELIVER", &splitMsgID))
	if err != nil {
		t.Fatalf("AwaitControlDispatchOn(spawner, want DELIVER): %v", err)
	}

	// Positive: peer steered, spawner delivered, same message id.
	if !strings.Contains(peerOpKind, "STEER") {
		t.Fatalf("peer op-kind = %q, want a STEER for the mentioned peer", peerOpKind)
	}
	if !strings.Contains(spawnerOpKind, "DELIVER") {
		t.Fatalf("spawner op-kind = %q, want a DELIVER for the unmentioned subscriber", spawnerOpKind)
	}

	// Exclusion covers only frames offered before the positive match. A later sweep
	// redelivery is outside this window and is not a violation.
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
