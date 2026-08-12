//go:build podman

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"testing"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/runner"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// spawnToolName is the registered name of the native spawn tool the agent's SDK
// exposes — the tool the canned script issues as a tool-call so the agent's loop
// runs Lifecycle(Spawn). Grounded firsthand against the compass-agent TS side:
// createLifecycleTools registers it as "agents_spawn_peer"
// (packages/compass-agent/src/lifecycle.ts:144).
const spawnToolName = "agents_spawn_peer"

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
// makes is ctx-bounded (AwaitSessionSettled / AwaitDelivery) — no sleeps, no
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
	// returns.
	f := NewFixture(ctx, t, WithCannedScript(
		CannedToolCall(spawnToolName, spawnArgsJSON),
		CannedText(settleReply),
	))

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
		peer, err := st.AgentByHandle(ctx, peerHandle)
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

	// Post to the SPAWNER's home channel: the server sweeps the undelivered
	// message in on session start and it fires the spawner's first turn (the one
	// that issues the spawn tool-call), so this post is what drives the turn
	// AwaitSessionSettled waits on. Must precede the settle wait.
	spawner, err := st.AgentByHandle(ctx, "leg34-spawner")
	if err != nil {
		t.Fatalf("AgentByHandle(spawner): %v", err)
	}
	if _, err := f.PostMessage(ctx, string(spawner.Agent.HomeChannelID), "general", "spawn a peer and stand by"); err != nil {
		t.Fatalf("PostMessage(home): %v", err)
	}

	// Event-gated settle on the spawner's session: the scripted spawn tool-call
	// executes and the closing text turn settles — no sleeps.
	if err := f.AwaitSessionSettled(ctx, sessionID); err != nil {
		t.Fatalf("AwaitSessionSettled (spawner): %v", err)
	}

	// ── Leg 3: fresh peer account (F2 ownership) + a second real container ──

	// The spawn minted a fresh agent account resolvable by its handle.
	peer, err := st.AgentByHandle(ctx, peerHandle)
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

	// TODO(SEA-1788): assert the steer-vs-deliver SPLIT on the recipient side —
	// the mentioned peer's live session receives a STEER (opSteer, mid-turn
	// interrupt) while an unmentioned channel subscriber receives a DELIVER
	// (turn-end coalesced), per mention_test.go:33,59 and consumer.go:296-308.
	// This is unconfirmable from the e2e fixture as it stands: the steer/deliver
	// op-kind is an AgentControl the delivery consumer dispatches over the
	// Runner's per-session Control stream (agent-facing internal surface), NOT a
	// comms-bus event a client SubscribeComms observes — the in-process suite
	// asserts it through a dispatchRecorder fake (mention_test.go), which the
	// over-the-wire e2e harness has no equivalent of. Confirming it needs a
	// second real peer subscribed-but-unmentioned AND a harness seam onto the
	// delivered AgentControl op-kind per recipient (a new Fixture primitive or a
	// store-side delivery-cursor read distinguishing the two). The observable
	// bus fan (the post committed + delivered above) is asserted; the
	// recipient-side split is FLAGGED. Until then leg-4's split assertion is the
	// missing green — the test is RED here by construction, not by a passing
	// stub.
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
