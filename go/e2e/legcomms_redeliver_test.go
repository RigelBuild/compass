//go:build podman

package e2e

import (
	"context"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// TestCommsOfflineRedeliveryOnSessionStart proves the session-start sweep
// redelivers a message that accrued while the agent was offline (RIG-3532 T5).
// "Offline" is a DESPAWN, not a never-started agent: a post to a
// provisioned-but-idle agent wakes and fresh-starts it (the placement recorded
// at Provision makes the wake an auto-start), so the message would never be
// owed. Posting AFTER RemoveWorkspace deletes the placement makes the wake a
// benign no-placement no-op, so the message stays owed for the next start.
func TestCommsOfflineRedeliveryOnSessionStart(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into NewFixture + every primitive

	// Slot 0 settles lifetime 1's warm turn, driven only so the resume has a
	// persisted transcript to reconstruct; slot 1 settles the resumed turn the
	// sweep's redelivery of the offline message drives.
	const warmReply = "canned lifetime-1 warm turn settled OK"
	const redeliverReply = "canned redelivered turn settled OK"
	f := NewFixture(ctx, t, WithCannedScript(
		CannedText(warmReply),
		CannedText(redeliverReply),
	))

	accountID, err := f.CreateAgent(ctx, "comms-redeliver", "Comms Offline Redelivery")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// ── Lifetime 1: provision, drive one turn so the session is resumable ──

	container1, err := f.Provision(ctx, accountID, "comms-redeliver-provision-1")
	if err != nil {
		t.Fatalf("Provision (container1): %v", err)
	}
	// Reap net: the reparented rootless conmon outlives stack Down, so an explicit
	// RemoveWorkspace is the reliable reap. Registered before StartSession so a
	// later failure still tears it down; the mid-test despawn below no-ops this
	// one idempotently. Best-effort teardown, so the error is deliberately dropped.
	t.Cleanup(func() {
		_ = f.RemoveWorkspace(ctx, container1, "comms-redeliver-teardown-1")
	})

	sessionID, err := f.StartSession(ctx, container1)
	if err != nil {
		t.Fatalf("StartSession (container1): %v", err)
	}

	st, err := store.Open(ctx, f.DSN())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	acc, err := adminAgentByHandle(ctx, st, "comms-redeliver")
	if err != nil {
		t.Fatalf("AgentByHandle: %v", err)
	}
	home := acc.Agent.HomeChannelID

	tail1, err := f.OpenSessionTail(ctx, sessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail (lifetime 1): %v", err)
	}
	defer tail1.Close()

	warmID, err := f.PostMessage(ctx, string(home), "general", "warm the session so it persists a transcript")
	if err != nil {
		t.Fatalf("PostMessage (warm): %v", err)
	}
	if err := f.AwaitTurnSettled(ctx, tail1); err != nil {
		t.Fatalf("AwaitTurnSettled (lifetime 1): %v", err)
	}

	// Ack the warm message BEFORE teardown so the resume sweep does not redeliver
	// it too — only the offline post below must be owed at resume time.
	if err := f.waitDeliveryCursorPast(ctx, st, acc.ID, home, store.MessageID(warmID)); err != nil {
		t.Fatalf("waitDeliveryCursorPast (warm): %v", err)
	}
	// The resume reconstructs the session body from the durable transcript, so the
	// warm turn must be committed before container1 is torn down.
	if _, err := f.awaitTranscriptPersisted(ctx, st, sessionID, warmReply); err != nil {
		t.Fatalf("awaitTranscriptPersisted (warm reply): %v", err)
	}

	// Despawn boundary: RemoveWorkspace deletes the placement, which is what makes
	// the offline post's wake a no-placement no-op instead of an auto-start.
	if err := f.RemoveWorkspace(ctx, container1, "comms-redeliver-teardown-1"); err != nil {
		t.Fatalf("RemoveWorkspace (despawn boundary): %v", err)
	}

	offlineID, err := f.PostMessage(ctx, string(home), "general", "message accrued while the agent is offline")
	if err != nil {
		t.Fatalf("PostMessage (offline): %v", err)
	}

	// Pre-condition: the offline message IS owed. Without it the post-condition
	// ("no longer owed") would pass vacuously against a never-owed message.
	owed, err := st.UndeliveredMessages(ctx, acc.ID)
	if err != nil {
		t.Fatalf("UndeliveredMessages (pre-condition): %v", err)
	}
	preOwed := false
	for _, m := range owed[home] {
		if m.ID == store.MessageID(offlineID) {
			preOwed = true
			break
		}
	}
	if !preOwed {
		t.Fatalf("offline message %s is not owed before session start; the pre-condition the redelivery proof rests on does not hold", offlineID)
	}

	// ── Lifetime 2: provision a fresh container, RESUME the logical session ──

	container2, err := f.Provision(ctx, accountID, "comms-redeliver-provision-2")
	if err != nil {
		t.Fatalf("Provision (container2): %v", err)
	}
	// Reap net for container2, same reparented-conmon reason; registered before
	// Resume so a Resume failure still tears it down. Best-effort, error dropped.
	t.Cleanup(func() {
		_ = f.RemoveWorkspace(ctx, container2, "comms-redeliver-teardown-2")
	})

	if _, err := f.Resume(ctx, container2, sessionID); err != nil {
		t.Fatalf("Resume (container2): %v", err)
	}

	// Post-condition read DURABLY from the store, not a live tail: the start-sweep
	// is enqueued INSIDE Resume and drained async, so a tail opened after Resume
	// can miss the redelivery (the RIG-3044 flake shape). The cursor advancing past
	// the offline message proves the sweep redelivered it and the session acked.
	if err := f.waitDeliveryCursorPast(ctx, st, acc.ID, home, store.MessageID(offlineID)); err != nil {
		t.Fatalf("waitDeliveryCursorPast (offline redelivery): %v", err)
	}
}
