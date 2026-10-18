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
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into every primitive

	f := sharedFixture(t)

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
	// Reap the container because rootless conmon outlives stack Down. Register
	// before StartSession so startup failure still triggers best-effort cleanup.
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

// TestLegTwoRealTurn exercises a full agent turn against the canned backend.
// Open the live-only session tail before posting so fast state frames are not
// missed; the transcript assertion proves the deterministic reply was stored.
func TestLegTwoRealTurn(t *testing.T) {
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into sharedFixture + every primitive

	f := sharedFixture(t)
	const cannedReply = leg2RealTurnReply
	accountID, err := f.CreateAgent(ctx, "leg2-realturn", "Leg Two Real Turn")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	containerName, err := f.Provision(ctx, accountID, "leg2-realturn-provision")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// Reap the container at test end because rootless conmon outlives stack Down.
	// Register before StartSession so failures still trigger best-effort cleanup.
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

	// Open the live-only tail before posting. Its registration ack proves the server
	// subscribed before a fast canned turn can emit state frames.
	tail, err := f.OpenSessionTail(ctx, sessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail: %v", err)
	}
	defer tail.Close()

	// The post reaches the already-live session and drives its first turn. The
	// session-start sweep handles only messages left undelivered by a prior lifetime.
	acc, err := adminAgentByHandle(ctx, st, "leg2-realturn")
	if err != nil {
		t.Fatalf("AgentByHandle: %v", err)
	}
	if _, err := f.PostMessage(ctx, string(acc.Agent.HomeChannelID), "general", "say hello and stop "+leg2RealTurnMarker); err != nil {
		t.Fatalf("PostMessage(home): %v", err)
	}

	// Wait for the turn's WORKING-to-READY transition on the open tail.
	if err := f.AwaitTurnSettled(ctx, tail); err != nil {
		t.Fatalf("AwaitTurnSettled: %v", err)
	}

	// Transcript commit uses a separate RPC and can trail READY. Wait for the exact
	// canned reply rather than racing the commit with an immediate read; this proves
	// the stub drove the turn.
	if _, err := f.awaitTranscriptPersisted(ctx, st, sessionID, cannedReply); err != nil {
		t.Fatalf("awaitTranscriptPersisted: %v", err)
	}
}

// leg2RealTurnMarker routes TestLegTwoRealTurn's prompt on the shared canned
// stub; leg2RealTurnReply is the exact assistant reply, asserted present in the
// persisted transcript.
const (
	leg2RealTurnMarker = "e2e-route-leg2-real-turn"
	leg2RealTurnReply  = "canned leg-2 turn settled OK"
)

func registerLegTwoFixtureOptions() {
	registerSharedFixtureOption(WithCannedMarkerReply(leg2RealTurnMarker, leg2RealTurnReply))
}
