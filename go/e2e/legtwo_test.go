//go:build podman

package e2e

import (
	"context"
	"strings"
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
// StartSession -> PostMessage(home) drives the turn -> AwaitSessionSettled -> assert the session's
// transcript is non-empty. On H2 it was PRESENT-BUT-SKIPPED: the leg-2 turn
// cannot complete without a deterministic model backend, so on the bare stack
// AwaitSessionSettled would hang and the transcript stay empty. H3 (SEA-1787)
// lands that backend — the canned stub the fixture stands up via WithCannedModel
// — so this same scenario now runs GREEN with zero live-model egress.
//
// The turn is driven entirely by the canned stub (a fixed scripted reply), and
// the settle is event-gated on AwaitSessionSettled (the READY frame) — no
// sleeps, no polling, no retries. AwaitSessionSettled has no hermetic unit shape
// (it needs a live settling session), so its proof rides this scenario —
// deliberately NOT faking a frame stream.
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

	// Post to the agent's home channel: the server sweeps the undelivered
	// message in on session start and it fires the agent's first turn, so this
	// post is what drives the turn AwaitSessionSettled waits on. Must precede the
	// settle wait.
	acc, err := st.AgentByHandle(ctx, "leg2-realturn")
	if err != nil {
		t.Fatalf("AgentByHandle: %v", err)
	}
	if _, err := f.PostMessage(ctx, string(acc.Agent.HomeChannelID), "general", "say hello and stop"); err != nil {
		t.Fatalf("PostMessage(home): %v", err)
	}

	if err := f.AwaitSessionSettled(ctx, sessionID); err != nil {
		t.Fatalf("AwaitSessionSettled: %v", err)
	}

	transcript, err := st.SessionTranscript(ctx, sessionID)
	if err != nil {
		t.Fatalf("SessionTranscript: %v", err)
	}
	if len(transcript) == 0 {
		t.Fatal("SessionTranscript returned an empty transcript; the completed turn was not persisted")
	}
	// The turn is deterministic: the canned stub settles on exactly cannedReply,
	// so that text MUST appear verbatim in the persisted transcript. A bare
	// non-empty check would pass on a misconfigured backend that logged only an
	// error entry or echoed back the prompt; asserting the canned reply's
	// presence is what proves the canned model actually drove the settled turn
	// (the record's "final settled entry is present", design.md §H2).
	var joined strings.Builder
	for _, e := range transcript {
		joined.WriteString(e.EntryJSON)
	}
	if !strings.Contains(joined.String(), cannedReply) {
		t.Fatalf("transcript does not contain the canned reply %q; the settled turn was not driven by the canned model", cannedReply)
	}
}
