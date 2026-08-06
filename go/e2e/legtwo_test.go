//go:build podman

package e2e

import (
	"context"
	"os"
	"testing"

	"github.com/sealedsecurity/compass/go/internal/store"
)

// TestLegTwoPrimitives is the deterministic proof that the leg-2 client-RPC
// primitives work over H1's real stack TODAY: CreateAgent -> Provision ->
// StartSession each return a non-empty id. No agent turn is needed — Provision
// and StartSession succeed without a model backend; the agent simply idles. This
// is GREEN on H1 and is the red->green gate for the primitives themselves
// (before agent_ops.go existed, this file did not compile).
//
// podmanUsable-guarded so a container-less sandbox SKIPS rather than fails. Each
// primitive carries its own deterministic per-call deadline internally-free RPC
// via the fixture clients; the outer ctx is the test root.
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

	sessionID, err := f.StartSession(ctx, containerName, "hello from leg-2 primitives")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if sessionID == "" {
		t.Fatal("StartSession returned an empty session id")
	}
}

// TestLegTwoRealTurn is the full leg-2 scenario: CreateAgent -> Provision ->
// StartSession(initial_prompt) -> AwaitSessionSettled -> assert the session's
// transcript is non-empty. It is PRESENT-BUT-SKIPPED on H2: the leg-2 turn
// cannot complete without H3's canned model backend (SEA-1787). On H1's stack
// the real agent has no deterministic model to drive a turn, so
// AwaitSessionSettled would hang and the transcript would stay empty. The frozen
// H2 decomposition gates this test's green on H3.
//
// It therefore t.Skips when the H3 canned-model sentinel is absent — never hangs,
// never fails. Once H3 wires the canned backend and sets COMPASS_E2E_CANNED_MODEL,
// this same test runs green. AwaitSessionSettled has no hermetic unit shape (it
// needs a live settling session), so its proof rides this scenario — deliberately
// NOT faking a frame stream.
func TestLegTwoRealTurn(t *testing.T) {
	if os.Getenv("COMPASS_E2E_CANNED_MODEL") == "" {
		t.Skip("leg-2 real turn needs the H3 canned model backend (SEA-1787); skipping until H3 lands")
	}
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into NewFixture + every primitive

	f := NewFixture(ctx, t)

	accountID, err := f.CreateAgent(ctx, "leg2-realturn", "Leg Two Real Turn")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	containerName, err := f.Provision(ctx, accountID, "leg2-realturn-provision")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	sessionID, err := f.StartSession(ctx, containerName, "say hello and stop")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if err := f.AwaitSessionSettled(ctx, sessionID); err != nil {
		t.Fatalf("AwaitSessionSettled: %v", err)
	}

	st, err := store.Open(ctx, f.DSN())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	transcript, err := st.SessionTranscript(ctx, sessionID)
	if err != nil {
		t.Fatalf("SessionTranscript: %v", err)
	}
	if len(transcript) == 0 {
		t.Fatal("SessionTranscript returned an empty transcript; the completed turn was not persisted")
	}
}
