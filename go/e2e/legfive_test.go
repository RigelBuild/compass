//go:build podman

package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// TestLegFivePersistAndResume is the full leg-5 scenario over the real stack
// (design.md:667-688): a first session runs a canned turn, its container is torn
// down (RemoveWorkspace — the persist boundary), a SECOND container is freshly
// provisioned, and the session is RESUMED into it with resume_session_id set. The
// resumed turn settles on a DIFFERENT canned reply, and the durable transcript —
// still keyed under the ORIGINAL logical session id across the resume — carries
// BOTH replies, proving the reconstructed body was loaded rather than a fresh
// session started. Modeled EXACTLY on TestLegTwoRealTurn and
// TestLegThreeFourSpawnAndMessaging: //go:build podman, the podmanUsable() skip
// guard first, context.Background() as the test root, sharedFixture(t) with
// this file's TestMain-registered canned route, a container-reaping t.Cleanup registered before each
// container's session start, and store-side assertions via store.Open(ctx,
// f.DSN()).
//
// It is PRESENT-BUT-SKIPPED (RED) on the bare stack today, exactly as
// TestLegTwoRealTurn was on H2: resume-into-a-fresh-container needs the
// assembled agent image + the full
// agent-lane (#202 gaps 1+3), unmerged, so the resumed turn cannot settle on the
// bare stack. podmanUsable() SKIPs it in a container-less sandbox. Every wait it
// makes is ctx-bounded (AwaitTurnSettled) — no sleeps, no polling, no retries.
func TestLegFivePersistAndResume(t *testing.T) {
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into sharedFixture + every primitive
	f := sharedFixture(t)
	const reply1 = leg5Reply1
	const reply2 = leg5Reply2
	accountID, err := f.CreateAgent(ctx, "leg5-persistresume", "Leg Five Persist And Resume")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// ── First lifetime: provision container1, run a turn, then tear it down ──

	container1, err := f.Provision(ctx, accountID, "leg5-provision-1")
	if err != nil {
		t.Fatalf("Provision (container1): %v", err)
	}
	// Reap container1 explicitly because rootless conmon outlives stack Down.
	// Register before StartSession so a later failure still cleans it up; the
	// mid-test remove is safe to repeat under RemoveWorkspace's idempotent contract.
	t.Cleanup(func() {
		_ = f.RemoveWorkspace(ctx, container1, "leg5-teardown-1")
	})

	originalSessionID, err := f.StartSession(ctx, container1)
	if err != nil {
		t.Fatalf("StartSession (container1): %v", err)
	}

	st, err := store.Open(ctx, f.DSN())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	// Both turns are driven by posts to the home channel. Live delivery fires the
	// current session; the resume sweep handles only messages still unacknowledged,
	// so post1 must be acked before resume or it consumes the next canned turn.
	acc, err := adminAgentByHandle(ctx, st, "leg5-persistresume")
	if err != nil {
		t.Fatalf("AgentByHandle: %v", err)
	}
	homeChannelID := string(acc.Agent.HomeChannelID)
	// Open the original session tail before post1; its registration ack guarantees
	// the fast turn's state frames cannot be missed.
	tail1, err := f.OpenSessionTail(ctx, originalSessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail (original): %v", err)
	}
	defer tail1.Close()

	post1ID, err := f.PostMessage(ctx, homeChannelID, "general", "say the pre-teardown reply and stop "+leg5Marker)
	if err != nil {
		t.Fatalf("PostMessage(home, pre-teardown): %v", err)
	}

	// Wait for the first turn to reach READY on the already-open tail.
	if err := f.AwaitTurnSettled(ctx, tail1); err != nil {
		t.Fatalf("AwaitTurnSettled (original): %v", err)
	}

	// Wait for delivery_ack before teardown. READY does not prove the cursor moved,
	// and resume would redeliver post1 if it remained owed.
	if err := f.waitDeliveryCursorPast(ctx, st, acc.ID, acc.Agent.HomeChannelID, store.MessageID(post1ID)); err != nil {
		t.Fatalf("waitDeliveryCursorPast (post1): %v", err)
	}

	// Ensure reply1 reached the durable transcript before teardown; resume rebuilds
	// session context from that transcript, and the commit is independent of READY
	// and delivery acknowledgement.
	if _, err := f.awaitTranscriptPersisted(ctx, st, originalSessionID, reply1); err != nil {
		t.Fatalf("awaitTranscriptPersisted (pre-teardown reply1): %v", err)
	}

	// Remove container1 at the persist boundary; its logical session transcript
	// remains under originalSessionID.
	if err := f.RemoveWorkspace(ctx, container1, "leg5-teardown-1"); err != nil {
		t.Fatalf("RemoveWorkspace (container1, persist boundary): %v", err)
	}

	// ── Second lifetime: provision container2, RESUME the logical session ──

	container2, err := f.Provision(ctx, accountID, "leg5-provision-2")
	if err != nil {
		t.Fatalf("Provision (container2): %v", err)
	}
	// Reap container2 on failure too; register cleanup before Resume.
	t.Cleanup(func() {
		_ = f.RemoveWorkspace(ctx, container2, "leg5-teardown-2")
	})

	// Resume the original logical session into a fresh container. Resume must reuse
	// its id so both lifetimes append to one transcript lineage.
	resumedSessionID, err := f.Resume(ctx, container2, originalSessionID)
	if err != nil {
		t.Fatalf("Resume (container2): %v", err)
	}

	// Open the resumed session tail before post2; the reused id and registration ack
	// guarantee the fast turn's state frames are observed.
	tail2, err := f.OpenSessionTail(ctx, resumedSessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail (resumed): %v", err)
	}
	defer tail2.Close()

	// Post 2 drives the resumed turn on the same home channel.
	if _, err := f.PostMessage(ctx, homeChannelID, "general", "say the resumed reply and stop "+leg5Marker); err != nil {
		t.Fatalf("PostMessage(home, resumed): %v", err)
	}

	// Wait for the resumed turn to reach READY on the already-open tail.
	if err := f.AwaitTurnSettled(ctx, tail2); err != nil {
		t.Fatalf("AwaitTurnSettled (resumed): %v", err)
	}

	// Read the logical session's transcript after reply2 commits. Resume binds new
	// entry sequences above the stored maximum, so the same transcript retains
	// reply1 and appends reply2 rather than starting a new lineage.
	transcript, err := f.awaitTranscriptPersisted(ctx, st, originalSessionID, reply2)
	if err != nil {
		t.Fatalf("awaitTranscriptPersisted(originalSessionID, reply2): %v", err)
	}
	var joined strings.Builder
	for _, e := range transcript {
		joined.WriteString(e.EntryJSON)
	}
	body := joined.String()
	// reply1 must survive RemoveWorkspace and resume, proving the reconstructed
	// prior context was carried forward.
	if !strings.Contains(body, reply1) {
		t.Fatalf("transcript under the original session id is missing the pre-teardown reply %q; the resumed session did not carry the persisted lineage", reply1)
	}
	// reply2 must append under the original session id, proving the resumed lifetime
	// shares the same durable transcript key.
	if !strings.Contains(body, reply2) {
		t.Fatalf("transcript under the original session id is missing the resumed reply %q; the resumed turn did not append to the logical lineage", reply2)
	}

	// Resume returns the logical session id as the live id. An empty or fresh id
	// would split the transcript and violate the single-lineage contract.
	if resumedSessionID == "" {
		t.Fatal("Resume returned an empty session id; the resumed lifetime did not come online")
	}
	if resumedSessionID != originalSessionID {
		t.Fatalf("Resume returned %q, want the original session id %q; resume must REUSE the logical id as the live id so the durable transcript is one lineage", resumedSessionID, originalSessionID)
	}
}

// Distinct replies let the transcript prove that resume carried context from
// before teardown, rather than containing only the second lifetime's reply.
const (
	leg5Marker = "e2e-route-leg5-resume"
	leg5Reply1 = "canned leg-5 pre-teardown turn settled OK"
	leg5Reply2 = "canned leg-5 resumed turn settled OK"
)

func registerLegFiveFixtureOptions() {
	registerSharedFixtureOption(WithCannedMarkerScript(leg5Marker, CannedText(leg5Reply1), CannedText(leg5Reply2)))
}
