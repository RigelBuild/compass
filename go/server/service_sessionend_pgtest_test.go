//go:build pgtest && unix

package server

// The SEA-1667 T4 session-end flush trigger (the third of three, design.md
// §1040-1046), driven through the real StopAgentSession handler against a real
// Postgres and a real Runner door. On a successful Stop the handler archives the
// session's remaining hot-tail as ONE session_end segment so history is complete
// for analytics — WITHOUT pruning the PG tail (session_end is never read on
// resume). It is BEST-EFFORT: a flush failure never converts a successful Stop
// into a failure, and a session with no transcript rows Stops cleanly recording
// nothing.
//
// The placement fixture (service_placement_pgtest_test.go) supplies store + hub +
// service + a fake Runner that answers Start/Stop; here we add the object-store
// seam (FAKED, no live S3) so the flush's PUT lands and the manifest row is
// assertable.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// startBoundSession seeds a placement for the fake container and drives a real
// StartAgentSession, so the fake Runner mints fakeSessionID and the handler
// records its agent_sessions ownership row — the FK root a transcript row needs.
func startBoundSession(t *testing.T, f placementFixture, ctx context.Context) {
	t.Helper()
	if err := f.store.RecordAgentPlacement(ctx, f.agentID, fakeRunnerID, fakeContainer); err != nil {
		t.Fatalf("RecordAgentPlacement: %v", err)
	}
	if _, err := f.client.StartAgentSession(ctx, connect.NewRequest(&compassv1.StartAgentSessionRequest{
		ContainerName: fakeContainer,
	})); err != nil {
		t.Fatalf("StartAgentSession: %v", err)
	}
}

// sessionEndSegments reads the session_end manifest rows for a session directly
// (no public read exposes non-safety_valve kinds).
func sessionEndSegments(t *testing.T, ctx context.Context, dsn, sessionID string) []struct {
	minSeq, maxSeq uint64
} {
	t.Helper()
	conn := connectPG(t, ctx, dsn)
	rows, err := conn.Query(ctx,
		`SELECT min_entry_seq, max_entry_seq
		   FROM agent_session_archive_segments
		  WHERE session_id = $1 AND kind = 'session_end'
		  ORDER BY min_entry_seq`,
		sessionID)
	if err != nil {
		t.Fatalf("query session_end segments: %v", err)
	}
	defer rows.Close()
	var out []struct{ minSeq, maxSeq uint64 }
	for rows.Next() {
		var mn, mx int64
		if err := rows.Scan(&mn, &mx); err != nil {
			t.Fatalf("scan segment: %v", err)
		}
		out = append(out, struct{ minSeq, maxSeq uint64 }{uint64(mn), uint64(mx)})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate segments: %v", err)
	}
	return out
}

// transcriptRowCount counts the PG hot-tail rows still held for a session — the
// no-prune assertion (session_end must leave the tail intact).
func transcriptRowCount(t *testing.T, ctx context.Context, dsn, sessionID string) int {
	t.Helper()
	conn := connectPG(t, ctx, dsn)
	var n int
	if err := conn.QueryRow(ctx,
		`SELECT COUNT(*) FROM agent_session_transcript_entries WHERE session_id = $1`,
		sessionID).Scan(&n); err != nil {
		t.Fatalf("count transcript rows: %v", err)
	}
	return n
}

// TestStopAgentSessionFlushesSessionEndWithoutPruning pins the session-end flush:
// after transcript rows are committed for a live session, a successful Stop
// records exactly one session_end segment spanning the whole tail AND leaves
// every PG row in place (session_end never prunes — the tail stays authoritative
// for the trace projection). The mutation that reddens it: dropping the flush
// call from StopAgentSession leaves zero session_end segments; a flush that
// prunes (wrong kind) empties the tail.
func TestStopAgentSessionFlushesSessionEndWithoutPruning(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background() // the test root context

	// The object-store seam is faked so the flush's PUT lands and the manifest
	// row is written (a nil object store would take the best-effort log path).
	f.store.SetObjectStore(newServerFakeObjectStore())

	startBoundSession(t, f, ctx)

	// A checkpoint plus two later deltas — the hot-tail the session-end flush
	// archives as one segment [1..3].
	if err := f.store.AppendTranscriptEntry(ctx, fakeSessionID, 1, true, `{"e":1}`, "k1"); err != nil {
		t.Fatalf("append checkpoint: %v", err)
	}
	if err := f.store.AppendTranscriptEntry(ctx, fakeSessionID, 2, false, `{"e":2}`, "k2"); err != nil {
		t.Fatalf("append delta 2: %v", err)
	}
	if err := f.store.AppendTranscriptEntry(ctx, fakeSessionID, 3, false, `{"e":3}`, "k3"); err != nil {
		t.Fatalf("append delta 3: %v", err)
	}

	if _, err := f.client.StopAgentSession(ctx, connect.NewRequest(&compassv1.StopAgentSessionRequest{
		SessionId: fakeSessionID,
	})); err != nil {
		t.Fatalf("StopAgentSession = %v, want success", err)
	}

	segs := sessionEndSegments(t, ctx, f.dsn, fakeSessionID)
	if len(segs) != 1 {
		t.Fatalf("session_end segments = %d, want exactly 1 (the whole tail archived once)", len(segs))
	}
	if segs[0].minSeq != 1 || segs[0].maxSeq != 3 {
		t.Fatalf("session_end segment span = [%d..%d], want [1..3]", segs[0].minSeq, segs[0].maxSeq)
	}

	// session_end does NOT prune: every PG row survives so the trace projection
	// keeps serving the retained tail.
	if got := transcriptRowCount(t, ctx, f.dsn, fakeSessionID); got != 3 {
		t.Fatalf("transcript rows after session_end flush = %d, want 3 (session_end never prunes)", got)
	}
}

// TestStopAgentSessionWithNoTranscriptRowsSucceedsAndArchivesNothing pins the
// best-effort/skip path: a session with zero transcript rows (max entry_seq 0)
// Stops cleanly and records NO session_end segment — there is nothing to
// archive, and the empty case must never fabricate a segment nor fail the Stop.
func TestStopAgentSessionWithNoTranscriptRowsSucceedsAndArchivesNothing(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background() // the test root context

	// An object store is wired, so a stray flush WOULD land a segment if the skip
	// path were wrong — proving the skip is driven by max==0, not by a nil store.
	f.store.SetObjectStore(newServerFakeObjectStore())

	startBoundSession(t, f, ctx)

	if _, err := f.client.StopAgentSession(ctx, connect.NewRequest(&compassv1.StopAgentSessionRequest{
		SessionId: fakeSessionID,
	})); err != nil {
		t.Fatalf("StopAgentSession on a session with no transcript rows = %v, want success", err)
	}

	if segs := sessionEndSegments(t, ctx, f.dsn, fakeSessionID); len(segs) != 0 {
		t.Fatalf("session_end segments = %d for a session with no rows, want 0 (nothing to archive)", len(segs))
	}
}

// serverFakeObjectStore is a minimal in-memory ObjectStore for the server-package
// session-end tests: the flush only needs PutSegment to land; these tests assert
// the manifest row, not the segment body, so GetSegment is unused.
type serverFakeObjectStore struct {
	objects map[string][]byte
}

func newServerFakeObjectStore() *serverFakeObjectStore {
	return &serverFakeObjectStore{objects: map[string][]byte{}}
}

func (f *serverFakeObjectStore) PutSegment(_ context.Context, key string, body []byte) error {
	stored := make([]byte, len(body))
	copy(stored, body)
	f.objects[key] = stored
	return nil
}

func (f *serverFakeObjectStore) GetSegment(_ context.Context, key string) ([]byte, error) {
	body, ok := f.objects[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return body, nil
}
