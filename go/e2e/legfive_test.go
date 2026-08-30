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
// guard first, context.Background() as the test root, NewFixture(ctx, t,
// WithCannedScript(...)), a container-reaping t.Cleanup registered before each
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
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into NewFixture + every primitive

	// Two DISTINCT canned replies: the pre-teardown turn settles on reply1, the
	// resumed turn on reply2. Distinct strings are what let the lineage assertion
	// prove the resumed session carried prior context — a fresh session would
	// hold only reply2, so reply1's presence is the load-of-reconstructed-body
	// proof.
	const reply1 = "canned leg-5 pre-teardown turn settled OK"
	const reply2 = "canned leg-5 resumed turn settled OK"
	f := NewFixture(ctx, t, WithCannedScript(
		CannedText(reply1),
		CannedText(reply2),
	))

	accountID, err := f.CreateAgent(ctx, "leg5-persistresume", "Leg Five Persist And Resume")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// ── First lifetime: provision container1, run a turn, then tear it down ──

	container1, err := f.Provision(ctx, accountID, "leg5-provision-1")
	if err != nil {
		t.Fatalf("Provision (container1): %v", err)
	}
	// Reap container1 (see TestLegTwoRealTurn): the reparented rootless conmon
	// outlives stack Down, so an explicit RemoveWorkspace is the only reliable
	// reap. Registered before StartSession so a later failure still tears it
	// down. This is the safety net; the mid-test RemoveWorkspace below is the
	// scenario's persist step — a second remove of an already-gone container is a
	// best-effort no-op by RemoveWorkspace's idempotency contract, so the two do
	// not fight. Best-effort — teardown, not an assertion — so the error is
	// deliberately discarded.
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

	// Resolve the agent's home channel once; both the pre-teardown and resumed
	// turns are driven by posts to it. Each post lands on the already-live
	// session and is delivered via the LIVE delivery path — the delivery
	// consumer tailing the comms bus dispatches it to the live session, which
	// fires the turn AwaitTurnSettled waits on. The session-start sweep only
	// redelivers messages left UNDELIVERED from a prior lifetime, which is
	// exactly why post1 below must be acked (cursor advanced) before the resume:
	// otherwise container2's start-sweep would redeliver it and consume the
	// resumed lifetime's canned turn. So each post must precede its settle wait.
	acc, err := adminAgentByHandle(ctx, st, "leg5-persistresume")
	if err != nil {
		t.Fatalf("AgentByHandle: %v", err)
	}
	homeChannelID := string(acc.Agent.HomeChannelID)
	// Open the tail on the ORIGINAL session BEFORE post1 drives the turn: the
	// frame stream is live-fan with no replay ring, so opening it first keeps a
	// fast canned turn's WORKING/READY edges from fanning into the post→subscribe
	// gap. Its own defer Close bounds this lifetime's tail.
	tail1, err := f.OpenSessionTail(ctx, originalSessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail (original): %v", err)
	}
	defer tail1.Close()

	post1ID, err := f.PostMessage(ctx, homeChannelID, "general", "say the pre-teardown reply and stop")
	if err != nil {
		t.Fatalf("PostMessage(home, pre-teardown): %v", err)
	}

	// Event-gated settle on the first session's already-open tail — skip until
	// WORKING, then return on the next READY: the canned turn0 (reply1) runs.
	if err := f.AwaitTurnSettled(ctx, tail1); err != nil {
		t.Fatalf("AwaitTurnSettled (original): %v", err)
	}

	// Gate on the delivery cursor advancing past post1 BEFORE the container1
	// teardown, so the resume start-sweep in container2 does not observe post1
	// still owed and redeliver it. The cursor advances on the agent's
	// delivery_ack, NOT on the settle above (AwaitTurnSettled returns on the
	// WORKING→READY edge; it does not gate on the ack), so this is the event that
	// actually proves post1 is consumed. See waitDeliveryCursorPast for the WHY.
	if err := f.waitDeliveryCursorPast(ctx, st, acc.ID, acc.Agent.HomeChannelID, store.MessageID(post1ID)); err != nil {
		t.Fatalf("waitDeliveryCursorPast (post1): %v", err)
	}

	// Gate on lifetime-1's reply1 being PERSISTED to the durable transcript before
	// the teardown: the resume below reconstructs the session body from the stored
	// transcript (ReconstructSessionBody → SessionResumeSnapshot), so if reply1's
	// CommitConversationFrame is still in flight when we tear container1 down and
	// resume, the reconstruction reads an empty transcript and the resume RPC fails
	// not_found. The commit trails the WORKING→READY settle by one runner→server
	// round-trip and is independent of the delivery-cursor ack gated above, so this
	// is the event that actually proves reply1 is durable and resumable. Same
	// keyed transcript the post-resume assertion reads (originalSessionID).
	if _, err := f.awaitTranscriptPersisted(ctx, st, originalSessionID, reply1); err != nil {
		t.Fatalf("awaitTranscriptPersisted (pre-teardown reply1): %v", err)
	}

	// The persist boundary: tear container1 down mid-test (the leg-5 teardown
	// step). The logical session's transcript survives in the store keyed under
	// originalSessionID; container1's Cleanup above then no-ops idempotently.
	if err := f.RemoveWorkspace(ctx, container1, "leg5-teardown-1"); err != nil {
		t.Fatalf("RemoveWorkspace (container1, persist boundary): %v", err)
	}

	// ── Second lifetime: provision container2, RESUME the logical session ──

	container2, err := f.Provision(ctx, accountID, "leg5-provision-2")
	if err != nil {
		t.Fatalf("Provision (container2): %v", err)
	}
	// Reap container2, same reparented-conmon reason as container1. Registered
	// before Resume so a Resume/settle failure still tears it down. Best-effort —
	// teardown, not an assertion — so the error is deliberately discarded.
	t.Cleanup(func() {
		_ = f.RemoveWorkspace(ctx, container2, "leg5-teardown-2")
	})

	// Resume the ORIGINAL logical session into the fresh container. Resume REUSES
	// the logical session id as the live id, so the resumed lifetime's transcript
	// frames commit under the SAME key the first lifetime used — one durable
	// lineage. resumedSessionID therefore equals originalSessionID.
	resumedSessionID, err := f.Resume(ctx, container2, originalSessionID)
	if err != nil {
		t.Fatalf("Resume (container2): %v", err)
	}

	// Open the tail on the RESUMED session BEFORE post2 drives the turn. The
	// frame stream keys on the live id, which on resume equals the logical id
	// (resumedSessionID == originalSessionID), and the tail opens BEFORE the
	// post, so a fast canned turn cannot fan its edges into the post→subscribe
	// gap.
	tail2, err := f.OpenSessionTail(ctx, resumedSessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail (resumed): %v", err)
	}
	defer tail2.Close()

	// Post 2 drives the resumed turn: same home channel, resolved once above.
	if _, err := f.PostMessage(ctx, homeChannelID, "general", "say the resumed reply and stop"); err != nil {
		t.Fatalf("PostMessage(home, resumed): %v", err)
	}

	// Event-gated settle on the RESUMED session's already-open tail — skip until
	// WORKING, then return on the next READY: the canned turn1 (reply2) runs. If
	// the resumed turn never settled, this errors (the design.md:687 "resumed
	// turn completes" leg).
	if err := f.AwaitTurnSettled(ctx, tail2); err != nil {
		t.Fatalf("AwaitTurnSettled (resumed): %v", err)
	}

	// The carried transcript lives under the logical session id, which on resume
	// IS the resumed live id (resumedSessionID == originalSessionID). The resumed
	// lifetime's frames commit under that same key, and BindLifetime rebases the
	// new lifetime's agent-stamped entry_seq onto the session's stored maximum so
	// the persisted sequence stays monotonic per session across the resume
	// (agent_transcripts.go:20-26, BindLifetime; server startResumeSession). Query
	// originalSessionID (== resumedSessionID) for the one durable lineage.
	// The resumed turn's transcript commits on the CommitConversationFrame unary,
	// one runner→server round-trip AFTER the WORKING→READY settle above — so gate
	// the read on reply2 (the resumed turn's own reply) converging rather than
	// reading immediately, which races the commit and flakes on store: not found
	// or a body still missing reply2. Once reply2 is present the earlier reply1
	// (committed in lifetime-1, before the teardown) is necessarily already there,
	// so this one gate covers both assertions below.
	transcript, err := f.awaitTranscriptPersisted(ctx, st, originalSessionID, reply2)
	if err != nil {
		t.Fatalf("awaitTranscriptPersisted(originalSessionID, reply2): %v", err)
	}
	var joined strings.Builder
	for _, e := range transcript {
		joined.WriteString(e.EntryJSON)
	}
	body := joined.String()
	// reply1 is durable across the persist boundary regardless of resume:
	// RemoveWorkspace does not prune the transcript, so the pre-teardown row
	// survives. Its presence here after resume confirms lifetime-2's start
	// carried the reconstructed prior body forward rather than superseding it
	// with a fresh-header checkpoint that prunes it. The primary rebase-onto-
	// the-logical-id proof is the reply2 assertion below.
	if !strings.Contains(body, reply1) {
		t.Fatalf("transcript under the original session id is missing the pre-teardown reply %q; the resumed session did not carry the persisted lineage", reply1)
	}
	// reply2 present proves the resumed turn's own settle landed in the SAME
	// keyed transcript — i.e. the new lifetime's frames rebased onto the logical
	// id rather than a fresh key; without it the resume would have appended
	// nowhere the original id can see.
	if !strings.Contains(body, reply2) {
		t.Fatalf("transcript under the original session id is missing the resumed reply %q; the resumed turn did not append to the logical lineage", reply2)
	}

	// The id-reuse contract: Resume REUSES the logical session id as the live id,
	// so the durable transcript is one lineage keyed under that single id. Pin
	// both halves: the returned id is non-empty (the resumed lifetime came
	// online) AND equals originalSessionID (the reuse, not a fresh mint). A fresh
	// mint would split the transcript across two keys — the exact bug this leg
	// guards (frames would land under the minted id, orphaned from the logical
	// lineage the reader queries).
	if resumedSessionID == "" {
		t.Fatal("Resume returned an empty session id; the resumed lifetime did not come online")
	}
	if resumedSessionID != originalSessionID {
		t.Fatalf("Resume returned %q, want the original session id %q; resume must REUSE the logical id as the live id so the durable transcript is one lineage", resumedSessionID, originalSessionID)
	}
}
