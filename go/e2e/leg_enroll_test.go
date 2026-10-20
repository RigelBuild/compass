//go:build podman

package e2e

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/RigelBuild/compass/go/internal/store"
)

const (
	enrollHandle         = "enroll-agent"
	enrollTurnlessHandle = "enroll-turnless"
	enrollFirstMarker    = "enroll-first-drive"
	enrollFirstReply     = "enroll first reply"
	enrollMarker         = "enroll-resume-drive"
	enrollReply          = "enroll resumed reply"
	enrollTokenFile      = "runner.token"
	enrollFallbackTxt    = "enroll unrouted turn"
)

// TestLegEnrollHardening runs on its own stack: the revoked-token half leaves the
// Runner unable to enroll, so it must not share the suite fixture. Order matters:
// the restart half needs a healthy Runner.
func TestLegEnrollHardening(t *testing.T) {
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman cannot run compass-agent:latest here")
	}
	ctx := context.Background()
	// The resumed request replays the first turn's history, so its marker is checked first.
	f := NewFixture(ctx, t, WithCannedScript(CannedText(enrollFallbackTxt)),
		WithCannedMarkerReply(enrollMarker, enrollReply), WithCannedMarkerReply(enrollFirstMarker, enrollFirstReply))
	st, err := store.Open(ctx, f.DSN())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	// Registered after NewFixture so it runs first: an open pool stalls postgres's smart shutdown in Down.
	t.Cleanup(st.Close)

	t.Run("restart resumes delivery", func(t *testing.T) { enrollRestartResumesDelivery(ctx, t, f, st) })
	t.Run("restart wakes a turnless session", func(t *testing.T) { enrollRestartWakesTurnless(ctx, t, f, st) })
	t.Run("revoked token fails closed", func(t *testing.T) { enrollRevokedTokenFailsClosed(ctx, t, f, st) })
}

// enrollRestartResumesDelivery restarts the Runner under a session that has taken
// a turn. Runner shutdown removes its containers, so a plain post must wake the
// agent, which re-provisions and resumes the same session and answers on it.
func enrollRestartResumesDelivery(ctx context.Context, t *testing.T, f *Fixture, st *store.Store) {
	t.Helper()
	accountID, err := f.CreateAgent(ctx, enrollHandle, "Enroll agent")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	container, err := f.Provision(ctx, accountID, "enroll-provision")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// The wake re-provisions under the same account-derived name, so one reap covers both.
	t.Cleanup(func() { _ = podmanRemoveForce(ctx, container) })
	sessionID, err := f.StartSession(ctx, container)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	acc, err := adminAgentByHandle(ctx, st, enrollHandle)
	if err != nil {
		t.Fatalf("AgentByHandle: %v", err)
	}
	first, err := f.OpenSessionTail(ctx, sessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail: %v", err)
	}
	if _, err := f.PostMessage(ctx, string(acc.Agent.HomeChannelID), "general", "before restart "+enrollFirstMarker); err != nil {
		t.Fatalf("PostMessage before restart: %v", err)
	}
	if err := f.AwaitTurnSettled(ctx, first); err != nil {
		t.Fatalf("AwaitTurnSettled before restart: %v", err)
	}
	first.Close()
	if _, err := f.awaitTranscriptPersisted(ctx, st, sessionID, enrollFirstReply); err != nil {
		t.Fatalf("first-turn transcript: %v", err)
	}

	if err := f.Stack().RestartRunner(ctx); err != nil {
		t.Fatalf("RestartRunner: %v", err)
	}
	if err := f.waitRunnerEnrolled(ctx); err != nil {
		t.Fatalf("waitRunnerEnrolled: %v", err)
	}

	// The old tail ended with the session; a fresh one on the same id sees the resumed turn.
	tail, err := f.OpenSessionTail(ctx, sessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail after restart: %v", err)
	}
	defer tail.Close()
	if _, err := f.PostMessage(ctx, string(acc.Agent.HomeChannelID), "general", "after restart "+enrollMarker); err != nil {
		t.Fatalf("PostMessage after restart: %v", err)
	}
	if err := f.AwaitTurnSettled(ctx, tail); err != nil {
		t.Fatalf("AwaitTurnSettled on the resumed session: %v", err)
	}
	if _, err := f.awaitTranscriptPersisted(ctx, st, sessionID, enrollReply); err != nil {
		t.Fatalf("resumed session transcript: %v", err)
	}
	latest, ok, err := st.LatestSessionForAccount(ctx, store.AccountID(accountID))
	if err != nil || !ok {
		t.Fatalf("LatestSessionForAccount: ok=%v err=%v", ok, err)
	}
	if latest != sessionID {
		t.Fatalf("latest session = %q, want the resumed %q (the wake minted a new session)", latest, sessionID)
	}
}

// enrollRestartWakesTurnless restarts the Runner under a session that never took
// a turn. With no transcript to resume, the wake must fresh-start the agent so
// the post is delivered; the delivery cursor passing it proves the injection.
func enrollRestartWakesTurnless(ctx context.Context, t *testing.T, f *Fixture, st *store.Store) {
	t.Helper()
	accountID, err := f.CreateAgent(ctx, enrollTurnlessHandle, "Enroll turnless agent")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	container, err := f.Provision(ctx, accountID, "enroll-turnless-provision")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() { _ = podmanRemoveForce(ctx, container) })
	turnless, err := f.StartSession(ctx, container)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	acc, err := adminAgentByHandle(ctx, st, enrollTurnlessHandle)
	if err != nil {
		t.Fatalf("AgentByHandle: %v", err)
	}
	if err := f.Stack().RestartRunner(ctx); err != nil {
		t.Fatalf("RestartRunner: %v", err)
	}
	if err := f.waitRunnerEnrolled(ctx); err != nil {
		t.Fatalf("waitRunnerEnrolled: %v", err)
	}
	postID, err := f.PostMessage(ctx, string(acc.Agent.HomeChannelID), "general", "after restart "+enrollMarker)
	if err != nil {
		t.Fatalf("PostMessage after restart: %v", err)
	}
	if err := f.waitDeliveryCursorPast(ctx, st, acc.ID, acc.Agent.HomeChannelID, store.MessageID(postID)); err != nil {
		t.Fatalf("post after restart was never delivered: %v", err)
	}
	latest, ok, err := st.LatestSessionForAccount(ctx, acc.ID)
	if err != nil || !ok || latest == turnless {
		t.Fatalf("latest session = %q (ok=%v err=%v), want a fresh session replacing %q", latest, ok, err, turnless)
	}
}

// enrollRevokedTokenFailsClosed revokes the Runner's token and restarts it. The
// stack keeps a revoked token on purpose, so the Runner must fail enrollment and
// exit, and the hub must refuse Provision rather than queue it.
func enrollRevokedTokenFailsClosed(ctx context.Context, t *testing.T, f *Fixture, st *store.Store) {
	t.Helper()
	token, err := os.ReadFile(filepath.Join(f.StateDir(), enrollTokenFile))
	if err != nil {
		t.Fatalf("read runner token: %v", err)
	}
	if err := st.RevokeToken(ctx, sha256.Sum256(token)); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	// A retrying Runner would instead time out the enroll wait; only a prompt exit is fail-closed.
	err = f.Stack().RestartRunner(ctx)
	if err == nil || !strings.Contains(err.Error(), "exited before enrolling") {
		t.Fatalf("RestartRunner with a revoked token = %v, want the Runner to exit before enrolling", err)
	}
	accountID, err := f.CreateAgent(ctx, "enroll-denied", "Enroll denied")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	container, err := f.Provision(ctx, accountID, "enroll-denied-provision")
	if err == nil {
		_ = podmanRemoveForce(ctx, container)
		t.Fatalf("Provision with no enrolled Runner returned container %q, want UNAVAILABLE", container)
	}
	if code := connect.CodeOf(err); code != connect.CodeUnavailable {
		t.Fatalf("Provision code = %v (%v), want UNAVAILABLE", code, err)
	}
}
