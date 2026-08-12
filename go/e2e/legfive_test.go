//go:build podman

package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/sealedsecurity/compass/go/internal/store"
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
// TestLegTwoRealTurn was on H2 and TestLegThreeFourSpawnAndMessaging is on H4:
// resume-into-a-fresh-container needs the assembled agent image + the full
// agent-lane (#202 gaps 1+3), unmerged, so the resumed turn cannot settle on the
// bare stack. podmanUsable() SKIPs it in a container-less sandbox. Every wait it
// makes is ctx-bounded (AwaitSessionSettled) — no sleeps, no polling, no retries.
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
	// turns are driven by posts to it. The server sweeps each undelivered
	// message in on session start and it fires that lifetime's turn, so each post
	// must precede its settle wait.
	acc, err := st.AgentByHandle(ctx, "leg5-persistresume")
	if err != nil {
		t.Fatalf("AgentByHandle: %v", err)
	}
	homeChannelID := string(acc.Agent.HomeChannelID)
	if _, err := f.PostMessage(ctx, homeChannelID, "general", "say the pre-teardown reply and stop"); err != nil {
		t.Fatalf("PostMessage(home, pre-teardown): %v", err)
	}

	// Event-gated settle on the first session — the canned turn0 (reply1) runs.
	if err := f.AwaitSessionSettled(ctx, originalSessionID); err != nil {
		t.Fatalf("AwaitSessionSettled (original): %v", err)
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

	// Resume the ORIGINAL logical session into the fresh container. Resume MINTS
	// a NEW live session id for this lifetime (the durable transcript stays keyed
	// under originalSessionID); resumedSessionID is that minted id.
	resumedSessionID, err := f.Resume(ctx, container2, originalSessionID)
	if err != nil {
		t.Fatalf("Resume (container2): %v", err)
	}

	// Post 2 drives the resumed turn: same home channel, resolved once above.
	if _, err := f.PostMessage(ctx, homeChannelID, "general", "say the resumed reply and stop"); err != nil {
		t.Fatalf("PostMessage(home, resumed): %v", err)
	}

	// Event-gated settle on the RESUMED session: the frame stream keys on the
	// minted live id, so wait on resumedSessionID — the canned turn1 (reply2)
	// runs. If the resumed turn never settled, this errors (the design.md:687
	// "resumed turn completes" leg).
	if err := f.AwaitSessionSettled(ctx, resumedSessionID); err != nil {
		t.Fatalf("AwaitSessionSettled (resumed): %v", err)
	}

	// The carried transcript lives under the ORIGINAL logical session id, NOT the
	// minted resumedSessionID: the persisted entry_seq is monotonic per session
	// across resumes and the transcript stays keyed under the logical id
	// (agent_transcripts.go:25-26; server/service.go:521-524 rebases the new
	// lifetime's frames onto the stored maximum under the stable logical id).
	// Querying resumedSessionID here would be a silent false-green (empty/wrong
	// set), so query originalSessionID.
	transcript, err := st.SessionTranscript(ctx, originalSessionID)
	if err != nil {
		t.Fatalf("SessionTranscript(originalSessionID): %v", err)
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

	// The mint-new-id contract: Resume returns a NEW live session id, never the
	// one passed in. The inequality with originalSessionID (below) pins that
	// resume minted a fresh live id rather than reusing the logical id — the
	// contract that separates the control-plane live id from the durable
	// transcript key (runnerhub/resume_start.go). The non-empty check is an
	// explicit contract statement for the reader; AwaitSessionSettled on
	// resumedSessionID above would already fail an empty id before this point.
	if resumedSessionID == "" {
		t.Fatal("Resume returned an empty session id; the resumed lifetime did not come online")
	}
	if resumedSessionID == originalSessionID {
		t.Fatalf("Resume returned the original session id %q; resume must MINT a new live id (the durable transcript stays keyed under the original)", originalSessionID)
	}
}
