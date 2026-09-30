//go:build podman

package e2e

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
)

// Every fixture here takes WithForgeStub: the server declares the forge secrets
// as required, and only the stub seeds them when the leg runs without the shared stack.
func TestLegS3ArchiveRestartResume(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here")
	}
	ctx := context.Background()
	t.Run("flush", func(t *testing.T) {
		const reply = "s3 flush turn complete"
		objectStore := WithObjectStore(startGarageFixture(t))
		f := NewFixture(ctx, t, objectStore, WithForgeStub(), WithCannedScript(CannedText(reply)))
		accountID, err := f.CreateAgent(ctx, "s3-flush", "S3 flush")
		if err != nil {
			t.Fatalf("CreateAgent: %v", err)
		}
		container, err := f.Provision(ctx, accountID, "s3-flush-provision")
		if err != nil {
			t.Fatalf("Provision: %v", err)
		}
		t.Cleanup(func() {
			if err := f.RemoveWorkspace(ctx, container, "s3-flush-cleanup"); err != nil {
				t.Errorf("RemoveWorkspace cleanup: %v", err)
			}
		})
		sessionID, err := f.StartSession(ctx, container)
		if err != nil {
			t.Fatalf("StartSession: %v", err)
		}
		st, err := store.Open(ctx, f.DSN())
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		defer st.Close()
		acc, err := adminAgentByHandle(ctx, st, "s3-flush")
		if err != nil {
			t.Fatalf("AgentByHandle: %v", err)
		}
		tail, err := f.OpenSessionTail(ctx, sessionID)
		if err != nil {
			t.Fatalf("OpenSessionTail: %v", err)
		}
		defer tail.Close()
		if _, err := f.PostMessage(ctx, string(acc.Agent.HomeChannelID), "general", "say the s3 flush turn and stop"); err != nil {
			t.Fatalf("PostMessage: %v", err)
		}
		if err := f.AwaitTurnSettled(ctx, tail); err != nil {
			t.Fatalf("AwaitTurnSettled: %v", err)
		}
		if _, err := f.awaitTranscriptPersisted(ctx, st, sessionID, reply); err != nil {
			t.Fatalf("awaitTranscriptPersisted: %v", err)
		}
		if _, err := f.Compass().StopAgentSession(ctx, connect.NewRequest(&compassv1.StopAgentSessionRequest{SessionId: sessionID})); err != nil {
			t.Fatalf("StopAgentSession: %v", err)
		}
		assertArchived(ctx, t, f, sessionID)
	})
	t.Run("server_restart", func(t *testing.T) { runS3Resume(t, ctx, true) })
	t.Run("runner_restart", func(t *testing.T) { runS3Resume(t, ctx, false) })
}

func runS3Resume(t *testing.T, ctx context.Context, wholeStack bool) {
	t.Helper()
	const handle, reply1, reply2 = "s3-resume", "s3 first reply", "s3 second reply"
	objectStore := WithObjectStore(startGarageFixture(t))
	site := newPersistentSite(t)
	f := NewFixture(ctx, t, objectStore, WithForgeStub(), WithSite(site), WithCannedScript(CannedText("s3 unrouted turn")), WithCannedMarkerScript(s3Marker, CannedText(reply1), CannedText(reply2)))
	accountID, err := f.CreateAgent(ctx, handle, "S3 resume")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	container, err := f.Provision(ctx, accountID, "s3-resume-provision")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// Removed explicitly before the restart; this cleanup only covers an early
	// Fatal, and runs before the first stack's Down.
	firstFixture, removed := f, false
	t.Cleanup(func() {
		if removed {
			return
		}
		if err := firstFixture.RemoveWorkspace(ctx, container, "s3-resume-cleanup"); err != nil {
			t.Errorf("RemoveWorkspace cleanup: %v", err)
		}
	})
	sessionID, err := f.StartSession(ctx, container)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	st, err := store.Open(ctx, f.DSN())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { st.Close() }()
	acc, err := adminAgentByHandle(ctx, st, handle)
	if err != nil {
		t.Fatalf("AgentByHandle: %v", err)
	}
	channel := string(acc.Agent.HomeChannelID)
	if err := runS3Turn(ctx, f, st, sessionID, channel, s3Marker, reply1); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	// Stop archives the pre-restart history as a session_end segment.
	if _, err := f.Compass().StopAgentSession(ctx, connect.NewRequest(&compassv1.StopAgentSessionRequest{SessionId: sessionID})); err != nil {
		t.Fatalf("StopAgentSession: %v", err)
	}
	if err := f.RemoveWorkspace(ctx, container, "s3-resume-teardown"); err != nil {
		t.Fatalf("RemoveWorkspace: %v", err)
	}
	removed = true
	if wholeStack {
		// An open pool keeps postgres from finishing its shutdown inside Down.
		st.Close()
		if err := f.Stack().Down(ctx); err != nil {
			t.Fatalf("Stack.Down: %v", err)
		}
		// Stack exposes no server-only restart, so the whole stack restarts.
		f = NewFixture(ctx, t, objectStore, WithForgeStub(), WithSite(site), WithCannedScript(CannedText("s3 unrouted turn")), WithCannedMarkerScript(s3Marker, CannedText(reply2)))
		if st, err = store.Open(ctx, f.DSN()); err != nil {
			t.Fatalf("store.Open after restart: %v", err)
		}
	} else {
		if err := f.Stack().RestartRunner(ctx); err != nil {
			t.Fatalf("RestartRunner: %v", err)
		}
		if err := f.waitRunnerEnrolled(ctx); err != nil {
			t.Fatalf("runner reattach: %v", err)
		}
	}
	container2, err := f.Provision(ctx, accountID, "s3-resume-provision-2")
	if err != nil {
		t.Fatalf("Provision after restart: %v", err)
	}
	t.Cleanup(func() {
		if err := f.RemoveWorkspace(ctx, container2, "s3-resume-cleanup-2"); err != nil {
			t.Errorf("RemoveWorkspace cleanup: %v", err)
		}
	})
	resumed, err := f.Resume(ctx, container2, sessionID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := runS3Turn(ctx, f, st, resumed, channel, s3Marker, reply2); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	transcript, err := f.awaitTranscriptPersisted(ctx, st, sessionID, reply2)
	if err != nil {
		t.Fatalf("awaitTranscriptPersisted: %v", err)
	}
	var body strings.Builder
	for _, entry := range transcript {
		body.WriteString(entry.EntryJSON)
	}
	if !strings.Contains(body.String(), reply1) || !strings.Contains(body.String(), reply2) {
		t.Fatalf("transcript missing expected replies: %q", body.String())
	}
	// The segment written before the restart is still listed and readable after it.
	assertArchived(ctx, t, f, sessionID)
}

// assertArchived checks the session has a manifest row and its object is in the bucket.
func assertArchived(ctx context.Context, t *testing.T, f *Fixture, sessionID string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, f.DSN())
	if err != nil {
		t.Fatalf("open manifest query pool: %v", err)
	}
	defer pool.Close()
	var key string
	if err := pool.QueryRow(ctx, `SELECT object_key FROM agent_session_archive_segments WHERE session_id=$1 ORDER BY min_entry_seq LIMIT 1`, sessionID).Scan(&key); err != nil {
		t.Fatalf("archive manifest row for %s: %v", sessionID, err)
	}
	if _, err := garageS3Client(t, f.garage).StatObject(ctx, f.garage.bucket, key, minio.StatObjectOptions{}); err != nil {
		t.Fatalf("StatObject(%q): %v", key, err)
	}
}

func runS3Turn(ctx context.Context, f *Fixture, st *store.Store, sessionID, channel, marker, reply string) error {
	tail, err := f.OpenSessionTail(ctx, sessionID)
	if err != nil {
		return err
	}
	defer tail.Close()
	if _, err := f.PostMessage(ctx, channel, "general", "marker "+marker); err != nil {
		return err
	}
	if err := f.AwaitTurnSettled(ctx, tail); err != nil {
		return err
	}
	_, err = f.awaitTranscriptPersisted(ctx, st, sessionID, reply)
	return err
}

// s3Marker routes both turns through one ordered script: a resumed request body
// still carries the first turn's text, so a second marker could never win.
const s3Marker = "e2e-route-s3-resume"
