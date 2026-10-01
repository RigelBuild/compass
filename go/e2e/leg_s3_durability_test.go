//go:build podman

package e2e

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"

	"github.com/RigelBuild/compass/go/internal/store"
)

func TestLegS3DurabilityValves(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here")
	}
	ctx := context.Background() // test root context
	for _, tc := range []struct {
		name string
		run  func(*testing.T, context.Context)
	}{
		{"valve_agent_crash", runS3AgentCrash},
		{"valve_server_restart", runS3ServerRestart},
		{"valve_runner_restart", runS3RunnerRestart},
		{"s3_outage", runS3Outage},
		{"crash_leaves_no_session_end", runS3CrashNoSessionEnd},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, ctx) })
	}
}

func runS3ServerRestart(t *testing.T, ctx context.Context) {
	t.Helper()
	runS3Restart(t, ctx, true)
}

func runS3RunnerRestart(t *testing.T, ctx context.Context) {
	t.Helper()
	runS3Restart(t, ctx, false)
}

func runS3DurabilityBase(t *testing.T, ctx context.Context, handle string, site *fixtureSite) (*Fixture, *store.Store, string, string, string) {
	t.Helper()
	g := startGarageFixture(t)
	toolScript := []CannedTurn{CannedToolCall("compass_set_status", `{"activity":"durability driving"}`), CannedText("durability first reply")}
	secondScript := []CannedTurn{CannedToolCall("compass_set_status", `{"activity":"durability resumed"}`), CannedText("durability second reply")}
	// The second marker registers first: a resumed request still carries the first turn's text, and the first registered match wins.
	options := []fixtureOption{WithObjectStore(g), WithSafetyValveCap(100), WithForgeStub(), WithCannedScript(CannedText("durability fallback")), WithCannedMarkerScript("durability-second", secondScript...), WithCannedMarkerScript("durability-first", toolScript...)}
	if site != nil {
		*site = newPersistentSite(t)
		options = append(options, WithSite(*site))
	}
	f := NewFixture(ctx, t, options...)
	accountID, err := f.CreateAgent(ctx, handle, "S3 durability")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	container, err := f.Provision(ctx, accountID, "durability-provision")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() {
		if err := podmanRemoveForce(ctx, container); err != nil {
			t.Errorf("remove container %q: %v", container, err)
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
	t.Cleanup(func() { st.Close() })
	acc, err := adminAgentByHandle(ctx, st, handle)
	if err != nil {
		t.Fatalf("AgentByHandle: %v", err)
	}
	return f, st, accountID, container, sessionID + "\x00" + string(acc.Agent.HomeChannelID)
}

func splitDurabilityIDs(t *testing.T, joined string) (string, string) {
	t.Helper()
	for i := range joined {
		if joined[i] == 0 {
			return joined[:i], joined[i+1:]
		}
	}
	t.Fatal("missing channel delimiter")
	return "", ""
}

func runS3AgentCrash(t *testing.T, ctx context.Context) {
	t.Helper()
	f, st, _, container, joined := runS3DurabilityBase(t, ctx, "s3-valve-agent-crash", nil)
	sessionID, channel := splitDurabilityIDs(t, joined)
	if err := runS3TurnSettled(ctx, f, sessionID, channel, "durability-first"); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	waitSafetyValveSegment(t, ctx, f.DSN(), sessionID)
	assertSafetyValveEviction(t, ctx, f, sessionID)
	// A lost container (as after a redeploy), not a killed one: a killed container lingers Exited and holds its name.
	if err := podmanRemoveForce(ctx, container); err != nil {
		t.Fatalf("crash agent container %q: %v", container, err)
	}
	assertNoSessionEnd(t, ctx, f.DSN(), sessionID)
	resumeS3AndVerify(t, ctx, f, st, "s3-valve-agent-crash", sessionID, channel)
}

func runS3Restart(t *testing.T, ctx context.Context, wholeStack bool) {
	t.Helper()
	handle := "s3-valve-runner-restart"
	if wholeStack {
		handle = "s3-valve-server-restart"
	}
	var site fixtureSite
	f, st, accountID, container, joined := runS3DurabilityBase(t, ctx, handle, &site)
	sessionID, channel := splitDurabilityIDs(t, joined)
	if err := runS3TurnSettled(ctx, f, sessionID, channel, "durability-first"); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	waitSafetyValveSegment(t, ctx, f.DSN(), sessionID)
	assertSafetyValveEviction(t, ctx, f, sessionID)

	if err := podmanRemoveForce(ctx, container); err != nil {
		t.Fatalf("remove old container: %v", err)
	}
	if wholeStack {
		st.Close()
		if err := f.Stack().Down(ctx); err != nil {
			t.Fatalf("Stack.Down: %v", err)
		}
		f = NewFixture(ctx, t, WithObjectStore(f.garage), WithSite(site), WithSafetyValveCap(100), WithForgeStub(), WithCannedScript(CannedText("durability fallback")), WithCannedMarkerScript("durability-second", CannedToolCall("compass_set_status", `{"activity":"durability resumed"}`), CannedText("durability second reply")))
		newStore, openErr := store.Open(ctx, f.DSN())
		if openErr != nil {
			t.Fatalf("store.Open after restart: %v", openErr)
		}
		st = newStore
		// Registered after NewFixture so it runs first: an open pool stalls postgres's smart shutdown in Down.
		t.Cleanup(newStore.Close)
	} else {
		if err := f.Stack().RestartRunner(ctx); err != nil {
			t.Fatalf("RestartRunner: %v", err)
		}
		if err := f.waitRunnerEnrolled(ctx); err != nil {
			t.Fatalf("waitRunnerEnrolled: %v", err)
		}
	}
	container2, err := f.Provision(ctx, accountID, "durability-provision-2")
	if err != nil {
		t.Fatalf("Provision after restart: %v", err)
	}
	t.Cleanup(func() {
		if err := podmanRemoveForce(ctx, container2); err != nil {
			t.Errorf("remove resumed container: %v", err)
		}
	})
	resumed, err := f.Resume(ctx, container2, sessionID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed != sessionID {
		t.Fatalf("resumed session = %q, want %q", resumed, sessionID)
	}
	if err := runS3Turn(ctx, f, st, resumed, channel, "durability-second", "durability second reply"); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if _, err := f.awaitTranscriptPersisted(ctx, st, sessionID, "durability second reply"); err != nil {
		t.Fatalf("await resumed transcript: %v", err)
	}
	// The valve moved the first reply out of PG, so history is PG tail plus archive.
	if !historyContains(t, ctx, f, st, sessionID, "durability first reply") {
		t.Fatal("first reply missing from PG tail and S3 archive after restart")
	}
	assertArchiveHasReply(t, ctx, f, sessionID, "durability first reply")
	assertArchiveKindPresent(t, ctx, f.DSN(), sessionID, "safety_valve")
}

func runS3Outage(t *testing.T, ctx context.Context) {
	t.Helper()
	f, _, _, container, joined := runS3DurabilityBase(t, ctx, "s3-durability-outage", nil)
	if container == "" {
		t.Fatal("empty agent container")
	}
	sessionID, channel := splitDurabilityIDs(t, joined)
	if err := runS3TurnSettled(ctx, f, sessionID, channel, "durability-first"); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	before := segmentCount(t, ctx, f.DSN(), sessionID)
	if err := podmanAction(ctx, "pause", f.garage.name); err != nil {
		t.Fatalf("pause Garage: %v", err)
	}
	// Unpause even if an assertion fails; a frozen Garage stalls fixture teardown.
	t.Cleanup(func() { _ = podmanAction(ctx, "unpause", f.garage.name) })
	if err := runS3TurnSettled(ctx, f, sessionID, channel, "durability-second"); err != nil {
		t.Fatalf("turn during outage: %v", err)
	}
	// Read while Garage is still paused: blocked PUTs complete once it resumes.
	if got := segmentCount(t, ctx, f.DSN(), sessionID); got != before {
		t.Fatalf("segments during outage = %d, want %d", got, before)
	}
	if err := podmanAction(ctx, "unpause", f.garage.name); err != nil {
		t.Fatalf("unpause Garage: %v", err)
	}
	if err := runS3TurnSettled(ctx, f, sessionID, channel, "durability-second"); err != nil {
		t.Fatalf("turn after outage: %v", err)
	}
	waitArchiveContains(t, ctx, f, sessionID, "durability second reply")
}

// waitArchiveContains polls until want lands in an S3 segment, proving flushes resume after an outage.
func waitArchiveContains(t *testing.T, ctx context.Context, f *Fixture, sessionID, want string) {
	t.Helper()
	deadline := time.Now().Add(settleTimeout)
	ticker := time.NewTicker(transcriptPollInterval)
	defer ticker.Stop()
	for !archiveContains(t, ctx, f, sessionID, want) {
		if !time.Now().Before(deadline) {
			t.Fatalf("%q did not reach the S3 archive within %s", want, settleTimeout)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for archive: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func runS3TurnSettled(ctx context.Context, f *Fixture, sessionID, channel, marker string) error {
	tail, err := f.OpenSessionTail(ctx, sessionID)
	if err != nil {
		return err
	}
	defer tail.Close()
	if _, err := f.PostMessage(ctx, channel, "general", "marker "+marker); err != nil {
		return err
	}
	return f.AwaitTurnSettled(ctx, tail)
}

func waitSafetyValveSegment(t *testing.T, ctx context.Context, dsn, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(settleTimeout)
	ticker := time.NewTicker(transcriptPollInterval)
	defer ticker.Stop()
	for {
		if segmentCountByKind(t, ctx, dsn, sessionID, "safety_valve") > 0 {
			return
		}
		if !time.Now().Before(deadline) {
			assertArchiveAndCheckpointDiagnostics(t, ctx, dsn, sessionID)
			t.Fatalf("safety_valve segment did not appear within %s", settleTimeout)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for safety_valve segment: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func segmentCount(t *testing.T, ctx context.Context, dsn, sessionID string) int {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open segment count pool: %v", err)
	}
	defer pool.Close()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_session_archive_segments WHERE session_id=$1`, sessionID).Scan(&count); err != nil {
		t.Fatalf("count segments: %v", err)
	}
	return count
}

func segmentCountByKind(t *testing.T, ctx context.Context, dsn, sessionID, kind string) int {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open segment kind pool: %v", err)
	}
	defer pool.Close()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_session_archive_segments WHERE session_id=$1 AND kind=$2`, sessionID, kind).Scan(&count); err != nil {
		t.Fatalf("count %s segments: %v", kind, err)
	}
	return count
}

func assertArchiveKindPresent(t *testing.T, ctx context.Context, dsn, sessionID, kind string) {
	t.Helper()
	if segmentCountByKind(t, ctx, dsn, sessionID, kind) == 0 {
		t.Fatalf("%s archive segment missing", kind)
	}
}

func assertArchiveAndCheckpointDiagnostics(t *testing.T, ctx context.Context, dsn, sessionID string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open diagnostic pool: %v", err)
	}
	defer pool.Close()
	rows, err := pool.Query(ctx, `SELECT kind FROM agent_session_archive_segments WHERE session_id=$1`, sessionID)
	if err != nil {
		t.Fatalf("query archive kinds: %v", err)
	}
	var kinds []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			rows.Close()
			t.Fatalf("scan archive kind: %v", err)
		}
		kinds = append(kinds, kind)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterate archive kinds: %v", err)
	}
	rows.Close()
	rows, err = pool.Query(ctx, `SELECT entry_seq, checkpoint FROM agent_session_transcript_entries WHERE session_id=$1 ORDER BY entry_seq`, sessionID)
	if err != nil {
		t.Fatalf("query transcript checkpoints: %v", err)
	}
	var checkpoints []string
	for rows.Next() {
		var seq int64
		var checkpoint bool
		if err := rows.Scan(&seq, &checkpoint); err != nil {
			rows.Close()
			t.Fatalf("scan transcript checkpoint: %v", err)
		}
		checkpoints = append(checkpoints, fmt.Sprintf("%d:%t", seq, checkpoint))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterate transcript checkpoints: %v", err)
	}
	rows.Close()
	t.Logf("archive kinds=%v transcript checkpoints=%v", kinds, checkpoints)
}

func assertSafetyValveEviction(t *testing.T, ctx context.Context, f *Fixture, sessionID string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, f.DSN())
	if err != nil {
		t.Fatalf("open valve query pool: %v", err)
	}
	defer pool.Close()
	var key string
	var minSeq, maxSeq int64
	if err := pool.QueryRow(ctx, `SELECT object_key, min_entry_seq, max_entry_seq FROM agent_session_archive_segments WHERE session_id=$1 AND kind='safety_valve' ORDER BY min_entry_seq LIMIT 1`, sessionID).Scan(&key, &minSeq, &maxSeq); err != nil {
		t.Fatalf("safety_valve segment: %v", err)
	}
	if _, err := garageS3Client(t, f.garage).StatObject(ctx, f.garage.bucket, key, minio.StatObjectOptions{}); err != nil {
		t.Fatalf("StatObject(%q): %v", key, err)
	}
	var retainedInRange int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_session_transcript_entries WHERE session_id=$1 AND entry_seq BETWEEN $2 AND $3`, sessionID, minSeq, maxSeq).Scan(&retainedInRange); err != nil {
		t.Fatalf("query retained evicted range: %v", err)
	}
	if retainedInRange != 0 {
		t.Fatalf("PG retains %d transcript rows in evicted range [%d,%d]", retainedInRange, minSeq, maxSeq)
	}
}

// Crash does not archive the tail; archive-on-crash remains a design decision.
func runS3CrashNoSessionEnd(t *testing.T, ctx context.Context) {
	t.Helper()
	f, st, _, container, joined := runS3DurabilityBase(t, ctx, "s3-crash-no-end", nil)
	sessionID, channel := splitDurabilityIDs(t, joined)
	if err := runS3TurnSettled(ctx, f, sessionID, channel, "durability-first"); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if err := podmanRemoveForce(ctx, container); err != nil {
		t.Fatalf("crash agent container: %v", err)
	}
	assertNoSessionEnd(t, ctx, f.DSN(), sessionID)
	var minHotSeq int64
	pool, err := pgxpool.New(ctx, f.DSN())
	if err != nil {
		t.Fatalf("open crash tail pool: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(MIN(entry_seq), 0) FROM agent_session_transcript_entries WHERE session_id=$1`, sessionID).Scan(&minHotSeq); err != nil {
		pool.Close()
		t.Fatalf("query crash hot tail min: %v", err)
	}
	pool.Close()
	if minHotSeq == 0 {
		t.Fatal("crash left no transcript entries in PostgreSQL hot tail")
	}
	resumeS3AndVerify(t, ctx, f, st, "s3-crash-no-end", sessionID, channel)
}

func resumeS3AndVerify(t *testing.T, ctx context.Context, f *Fixture, st *store.Store, accountHandle, sessionID, channel string) {
	t.Helper()
	account, err := f.lookupAccount(ctx, accountHandle)
	if err != nil {
		t.Fatalf("lookup account: %v", err)
	}
	container, err := f.Provision(ctx, account.GetId(), "durability-resume-provision")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() {
		if err := podmanRemoveForce(ctx, container); err != nil {
			t.Errorf("remove resumed container: %v", err)
		}
	})
	resumed, err := f.Resume(ctx, container, sessionID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed != sessionID {
		t.Fatalf("resumed session = %q, want %q", resumed, sessionID)
	}
	if err := runS3Turn(ctx, f, st, resumed, channel, "durability-second", "durability second reply"); err != nil {
		t.Fatalf("resumed turn: %v", err)
	}
	assertArchiveHasReply(t, ctx, f, sessionID, "durability first reply")
}

func assertNoSessionEnd(t *testing.T, ctx context.Context, dsn, sessionID string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open assertion pool: %v", err)
	}
	defer pool.Close()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_session_archive_segments WHERE session_id=$1 AND kind='session_end'`, sessionID).Scan(&count); err != nil {
		t.Fatalf("query session_end segments: %v", err)
	}
	if count != 0 {
		t.Fatalf("session_end segments after crash = %d, want 0", count)
	}
}

// assertArchiveHasReply requires want in an archived S3 object, not just in the PG tail.
func assertArchiveHasReply(t *testing.T, ctx context.Context, f *Fixture, sessionID, want string) {
	t.Helper()
	if !archiveContains(t, ctx, f, sessionID, want) {
		t.Fatalf("no archived S3 segment for session %s contains %q", sessionID, want)
	}
}

func podmanAction(ctx context.Context, action, name string) error {
	if out, err := exec.CommandContext(ctx, "podman", action, name).CombinedOutput(); err != nil {
		return fmt.Errorf("podman %s %s: %w: %s", action, name, err, out)
	}
	return nil
}

func historyContains(t *testing.T, ctx context.Context, f *Fixture, st *store.Store, sessionID, want string) bool {
	t.Helper()
	tail, err := st.SessionTranscript(ctx, sessionID)
	if err != nil {
		t.Fatalf("SessionTranscript: %v", err)
	}
	for _, entry := range tail {
		if strings.Contains(entry.EntryJSON, want) {
			return true
		}
	}
	return archiveContains(t, ctx, f, sessionID, want)
}

func archiveContains(t *testing.T, ctx context.Context, f *Fixture, sessionID, want string) bool {
	t.Helper()
	pool, err := pgxpool.New(ctx, f.DSN())
	if err != nil {
		t.Fatalf("open history pool: %v", err)
	}
	defer pool.Close()
	rows, err := pool.Query(ctx, `SELECT object_key FROM agent_session_archive_segments WHERE session_id=$1`, sessionID)
	if err != nil {
		t.Fatalf("query segment keys: %v", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect segment keys: %v", err)
	}
	client := garageS3Client(t, f.garage)
	for _, key := range keys {
		obj, err := client.GetObject(ctx, f.garage.bucket, key, minio.GetObjectOptions{})
		if err != nil {
			t.Fatalf("GetObject(%q): %v", key, err)
		}
		data, err := io.ReadAll(obj)
		_ = obj.Close()
		if err != nil {
			t.Fatalf("read segment %q: %v", key, err)
		}
		if strings.Contains(string(data), want) {
			return true
		}
	}
	return false
}
