//go:build podman

package e2e

import (
	"context"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// TestLegTwoPrimitives is the deterministic proof that the leg-2 client-RPC
// primitives work over H1's real stack TODAY: CreateAgent -> Provision ->
// StartSession each return a non-empty id. No agent turn is needed — Provision
// and StartSession succeed without a model backend; the agent simply idles. This
// is GREEN on H1 and is the red->green gate for the primitives themselves
// (before agent_ops.go existed, this file did not compile).
//
// podmanUsable-guarded so a container-less sandbox SKIPS rather than fails. Each
// primitive derives its own deterministic per-call deadline internally from the
// passed-in ctx via the fixture clients; the outer ctx is the test root.
func TestLegTwoPrimitives(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into NewFixture + every primitive

	f := NewFixture(ctx, t)

	accountID, err := f.CreateAgent(ctx, "leg2-primitives", "Leg Two Primitives")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if accountID == "" {
		t.Fatal("CreateAgent returned an empty account id")
	}

	containerName, err := f.Provision(ctx, accountID, "leg2-primitives-provision")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if containerName == "" {
		t.Fatal("Provision returned an empty container name")
	}
	// Reap the provisioned container: stack Down stops only the stack processes,
	// and the rootless conmon holding this container is reparented, so without an
	// explicit RemoveWorkspace it leaks past every green run. Registered before
	// StartSession so a StartSession failure still tears the container down.
	// Best-effort — teardown, not an assertion.
	t.Cleanup(func() {
		_ = f.RemoveWorkspace(ctx, containerName, "leg2-primitives-teardown")
	})

	sessionID, err := f.StartSession(ctx, containerName)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if sessionID == "" {
		t.Fatal("StartSession returned an empty session id")
	}
}

// TestLegTwoRealTurn is the full leg-2 scenario: CreateAgent -> Provision ->
// StartSession -> OpenSessionTail(sessionID) -> PostMessage(home) drives the
// turn -> AwaitTurnSettled -> assert the session's transcript is non-empty. On
// H2 it was PRESENT-BUT-SKIPPED: the leg-2 turn cannot complete without a
// deterministic model backend, so on the bare stack the settle would hang and
// the transcript stay empty. H3 (SEA-1787) lands that backend — the canned stub
// the fixture stands up via WithCannedModel — so this same scenario now runs
// GREEN with zero live-model egress.
//
// The turn is driven entirely by the canned stub (a fixed scripted reply). The
// tail is opened BEFORE the post so a fast canned turn cannot fan its edges into
// the post→subscribe gap, and the settle is event-gated on AwaitTurnSettled (the
// WORKING→READY edge) — no sleeps, no polling, no retries. The split settle
// primitives have no hermetic unit shape (they need a live settling session), so
// their proof rides this scenario — deliberately NOT faking a frame stream.
func TestLegTwoRealTurn(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into NewFixture + every primitive

	// The exact assistant reply the canned stub settles every turn on; asserted
	// present in the persisted transcript below.
	const cannedReply = "canned leg-2 turn settled OK"
	f := NewFixture(ctx, t, WithCannedModel(cannedReply))

	accountID, err := f.CreateAgent(ctx, "leg2-realturn", "Leg Two Real Turn")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	containerName, err := f.Provision(ctx, accountID, "leg2-realturn-provision")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// Reap the provisioned container (see TestLegTwoPrimitives): the reparented
	// rootless conmon outlives stack Down, so without an explicit RemoveWorkspace
	// the container leaks past every green run. Registered before StartSession so
	// a StartSession failure still tears it down. Best-effort — teardown, not an
	// assertion.
	t.Cleanup(func() {
		_ = f.RemoveWorkspace(ctx, containerName, "leg2-realturn-teardown")
	})

	sessionID, err := f.StartSession(ctx, containerName)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	st, err := store.Open(ctx, f.DSN())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	// Open the session tail BEFORE the post drives the turn: OpenSessionTail
	// subscribes to the frame stream so it is already tailing when the turn
	// fans. SubscribeAgentSession is live-fan (no replay ring), so were the tail
	// opened AFTER the post a fast canned turn could fan its WORKING/READY edges
	// into the post→subscribe gap and AwaitTurnSettled would hang. Must precede
	// the post.
	tail, err := f.OpenSessionTail(ctx, sessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail: %v", err)
	}
	defer tail.Close()

	// Post to the agent's home channel: this post lands on the already-live
	// session and is delivered via the live fan-out (the delivery consumer
	// tailing the comms bus), which fires the agent's first turn — the turn
	// AwaitTurnSettled waits on. The session-start sweep only redelivers
	// messages left undelivered from a prior lifetime (relevant only to leg-5's
	// post1), not this one.
	acc, err := adminAgentByHandle(ctx, st, "leg2-realturn")
	if err != nil {
		t.Fatalf("AgentByHandle: %v", err)
	}
	if _, err := f.PostMessage(ctx, string(acc.Agent.HomeChannelID), "general", "say hello and stop"); err != nil {
		t.Fatalf("PostMessage(home): %v", err)
	}

	// Event-gated settle on the already-open tail: skip until WORKING, then
	// return on the next READY (WORKING→READY = one settled turn) — no sleeps.
	if err := f.AwaitTurnSettled(ctx, tail); err != nil {
		t.Fatalf("AwaitTurnSettled: %v", err)
	}

	// The transcript persists on the CommitConversationFrame unary, INDEPENDENT
	// of the PublishEvents session-state channel AwaitTurnSettled gates on, so it
	// commits one runner→server round-trip AFTER the WORKING→READY settle. Gate
	// the read on that convergence rather than reading immediately (which races
	// the commit and flakes on store: not found). The turn is deterministic — the
	// canned stub settles on exactly cannedReply — so the persisted transcript
	// MUST contain that text verbatim; awaitTranscriptPersisted returns only once
	// it does. Asserting the canned reply's presence (not a bare non-empty check)
	// is what proves the canned model actually drove the settled turn (the
	// record's "final settled entry is present", design.md §H2).
	if _, err := f.awaitTranscriptPersisted(ctx, st, sessionID, cannedReply); err != nil {
		t.Fatalf("awaitTranscriptPersisted: %v", err)
	}
}
