//go:build pgtest

package store

// The durable two-tier transcript store (RIG-1667 T4), proven against real
// Postgres with the object-store seam FAKED (no live S3). Covers the record's
// T4 test cycle: FK rejection; idempotent duplicate key; ordering; the
// session-scoped rebase across lifetimes; the post-supersession read view;
// not-found; the PRIMARY (checkpoint-arrival) flush; PUT-before-prune;
// manifest-INSERT+prune atomicity and the ON CONFLICT re-run; PG-only
// reconstruction; the size-cap safety valve; a later checkpoint re-marking a
// stale safety_valve segment; and the session_end flush that records without
// pruning.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
)

// fakeObjectStore is the in-memory ObjectStore fake. It records every PUT body
// by key, and can be armed to fail PutSegment (the PUT-before-prune probe) or to
// fail the test if GetSegment is ever called (the PG-only-reconstruction probe).
type fakeObjectStore struct {
	mu        sync.Mutex
	objects   map[string][]byte
	putErr    error
	failOnGet *testing.T
	getCalled bool
}

func newFakeObjectStore() *fakeObjectStore {
	return &fakeObjectStore{objects: map[string][]byte{}}
}

func (f *fakeObjectStore) PutSegment(_ context.Context, key string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	// Copy so a later caller mutation cannot alias the stored body.
	stored := make([]byte, len(body))
	copy(stored, body)
	f.objects[key] = stored
	return nil
}

func (f *fakeObjectStore) GetSegment(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalled = true
	if f.failOnGet != nil {
		f.failOnGet.Errorf("GetSegment(%q) called during a PG-only path", key)
	}
	body, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("fake object store: no object at %q", key)
	}
	return body, nil
}

func (f *fakeObjectStore) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

// storeWithFake returns a store wired to a fresh in-memory object-store fake.
func storeWithFake(t *testing.T) (*Store, *fakeObjectStore) {
	t.Helper()
	s := newTestStore(t)
	fake := newFakeObjectStore()
	s.SetObjectStore(fake)
	return s, fake
}

// seedSession creates a user + agent + recorded session and returns the
// session_id — the FK root every transcript row references.
func seedSession(t *testing.T, s *Store, handle, sessionID string) string {
	t.Helper()
	owner := mustUser(t, s, handle+"-owner")
	agent := mustAgent(t, s, owner.ID, handle+"-agent")
	if err := s.RecordAgentSession(t.Context(), sessionID, agent.ID); err != nil {
		t.Fatalf("RecordAgentSession(%q): %v", sessionID, err)
	}
	return sessionID
}

// manifestRows reads the archive manifest for a session, ordered by min seq —
// the direct-pool assertion surface (no public read for non-safety_valve kinds).
func manifestRows(t *testing.T, s *Store, sessionID string) []ArchiveSegmentRow {
	t.Helper()
	rows, err := s.pool.Query(t.Context(),
		`SELECT object_key, min_entry_seq, max_entry_seq, kind
		   FROM agent_session_archive_segments
		  WHERE session_id = $1 ORDER BY min_entry_seq, object_key`,
		sessionID,
	)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	defer rows.Close()
	var out []ArchiveSegmentRow
	for rows.Next() {
		var (
			key            string
			minSeq, maxSeq int64
			kind           string
		)
		if err := rows.Scan(&key, &minSeq, &maxSeq, &kind); err != nil {
			t.Fatalf("scan manifest: %v", err)
		}
		out = append(out, ArchiveSegmentRow{
			ObjectKey: key, MinEntrySeq: toUint64(minSeq), MaxEntrySeq: toUint64(maxSeq), Kind: SegmentKind(kind),
		})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate manifest: %v", err)
	}
	return out
}

// hotTailSeqs reads the raw entry_seqs held in the hot-tail for a session,
// ascending — the prune-assertion surface.
func hotTailSeqs(t *testing.T, s *Store, sessionID string) []uint64 {
	t.Helper()
	rows, err := s.pool.Query(t.Context(),
		`SELECT entry_seq FROM agent_session_transcript_entries
		  WHERE session_id = $1 ORDER BY entry_seq`, sessionID)
	if err != nil {
		t.Fatalf("read hot tail: %v", err)
	}
	defer rows.Close()
	var out []uint64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatalf("scan hot tail: %v", err)
		}
		out = append(out, toUint64(seq))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate hot tail: %v", err)
	}
	return out
}

func transcriptSeqs(rows []TranscriptEntryRow) []uint64 {
	out := make([]uint64, len(rows))
	for i, r := range rows {
		out[i] = r.EntrySeq
	}
	return out
}

func seqsEqual(a []uint64, b ...uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── Core store contracts ────────────────────────────────────────────────────

// TestAppendTranscriptEntryUnknownSessionInvalidArgument pins the FK rejection:
// an entry for a session with no agent_sessions row is ErrInvalidArgument (the
// AppendTranscriptEntry contract), never a silent write.
func TestAppendTranscriptEntryUnknownSessionInvalidArgument(t *testing.T) {
	s, _ := storeWithFake(t)
	err := s.AppendTranscriptEntry(t.Context(), "ghost-session", 1, false, `{"e":1}`, "idem-1")
	sentinelIs(t, err, ErrInvalidArgument, "append to unknown session")
}

// TestAppendTranscriptEntryRejectsOutOfRangeSeq pins the entry_seq range guard:
// a lifetimeSeq above math.MaxInt64 (which would wrap negative under the int64
// narrowing and break the monotonic-positive (session_id, entry_seq) PK) is
// rejected as ErrInvalidArgument before it reaches the insert.
func TestAppendTranscriptEntryRejectsOutOfRangeSeq(t *testing.T) {
	s, _ := storeWithFake(t)
	sess := seedSession(t, s, "oor", "sess-oor")
	if _, err := s.BindLifetime(t.Context(), sess); err != nil {
		t.Fatalf("BindLifetime: %v", err)
	}
	err := s.AppendTranscriptEntry(t.Context(), sess, uint64(math.MaxInt64)+1, false, `{"e":1}`, "idem-oor")
	sentinelIs(t, err, ErrInvalidArgument, "append with out-of-range seq")
	// The guard returns before the INSERT: a rejected seq writes nothing.
	if got := hotTailSeqs(t, s, sess); len(got) != 0 {
		t.Fatalf("hot tail = %v, want empty (out-of-range seq must write no row)", got)
	}
}

// TestAppendTranscriptEntryIdempotentDuplicateKey pins the at-most-once contract:
// a second append on the same idempotency_key is a SILENT SUCCESS (the retry
// dedup) and writes no second row — not an error.
func TestAppendTranscriptEntryIdempotentDuplicateKey(t *testing.T) {
	s, _ := storeWithFake(t)
	sess := seedSession(t, s, "dup", "sess-dup")
	if _, err := s.BindLifetime(t.Context(), sess); err != nil {
		t.Fatalf("BindLifetime: %v", err)
	}
	if err := s.AppendTranscriptEntry(t.Context(), sess, 1, false, `{"e":1}`, "idem-1"); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// Same key, DIFFERENT body/seq — the dedup must absorb it silently.
	if err := s.AppendTranscriptEntry(t.Context(), sess, 2, false, `{"e":2}`, "idem-1"); err != nil {
		t.Fatalf("duplicate-key append must be a silent success, got %v", err)
	}
	if got := hotTailSeqs(t, s, sess); !seqsEqual(got, 1) {
		t.Fatalf("hot tail = %v, want exactly [1] (duplicate key wrote no second row)", got)
	}
}

// TestAppendTranscriptEntryOrdersByEntrySeq pins the ordered read: entries appended
// out of lifetime-seq order still read back ascending by entry_seq.
func TestAppendTranscriptEntryOrdersByEntrySeq(t *testing.T) {
	s, _ := storeWithFake(t)
	sess := seedSession(t, s, "order", "sess-order")
	if _, err := s.BindLifetime(t.Context(), sess); err != nil {
		t.Fatalf("BindLifetime: %v", err)
	}
	// Append 3, 1, 2 (distinct keys) — no checkpoint, so all retained.
	for _, seq := range []uint64{3, 1, 2} {
		if err := s.AppendTranscriptEntry(t.Context(), sess, seq, false, fmt.Sprintf(`{"e":%d}`, seq), fmt.Sprintf("idem-%d", seq)); err != nil {
			t.Fatalf("append %d: %v", seq, err)
		}
	}
	got, err := s.SessionTranscript(t.Context(), sess)
	if err != nil {
		t.Fatalf("SessionTranscript: %v", err)
	}
	if !seqsEqual(transcriptSeqs(got), 1, 2, 3) {
		t.Fatalf("transcript order = %v, want [1 2 3]", transcriptSeqs(got))
	}
}

// TestBindLifetimeRebasesAcrossLifetimes is the headline identity contract: two
// container lifetimes, each stamping entry_seq from 1, land as ONE gapless
// session-scoped sequence — the second lifetime's first delta is base+1, never a
// PK collision with the first lifetime's rows.
func TestBindLifetimeRebasesAcrossLifetimes(t *testing.T) {
	s, _ := storeWithFake(t)
	sess := seedSession(t, s, "rebase", "sess-rebase")

	// Lifetime 1: base 0, three deltas stamped 1..3 -> entry_seq 1..3.
	base1, err := s.BindLifetime(t.Context(), sess)
	if err != nil {
		t.Fatalf("BindLifetime L1: %v", err)
	}
	if base1 != 0 {
		t.Fatalf("first lifetime base = %d, want 0", base1)
	}
	for seq := uint64(1); seq <= 3; seq++ {
		if err := s.AppendTranscriptEntry(t.Context(), sess, seq, false, fmt.Sprintf(`{"l":1,"s":%d}`, seq), fmt.Sprintf("l1-%d", seq)); err != nil {
			t.Fatalf("L1 append %d: %v", seq, err)
		}
	}

	// Lifetime 2 (resume): base snapshots the stored max (3). The agent again
	// stamps from 1, so its first delta rebases to entry_seq 4 — no collision.
	base2, err := s.BindLifetime(t.Context(), sess)
	if err != nil {
		t.Fatalf("BindLifetime L2: %v", err)
	}
	if base2 != 3 {
		t.Fatalf("second lifetime base = %d, want 3 (the stored max)", base2)
	}
	if err := s.AppendTranscriptEntry(t.Context(), sess, 1, false, `{"l":2,"s":1}`, "l2-1"); err != nil {
		t.Fatalf("L2 first post-resume delta must not collide, got %v", err)
	}
	if err := s.AppendTranscriptEntry(t.Context(), sess, 2, false, `{"l":2,"s":2}`, "l2-2"); err != nil {
		t.Fatalf("L2 second delta: %v", err)
	}

	got, err := s.SessionTranscript(t.Context(), sess)
	if err != nil {
		t.Fatalf("SessionTranscript: %v", err)
	}
	if !seqsEqual(transcriptSeqs(got), 1, 2, 3, 4, 5) {
		t.Fatalf("rebased sequence = %v, want one gapless [1 2 3 4 5]", transcriptSeqs(got))
	}
}

// TestBindLifetimeIdempotentWithinLifetime pins the write-once property: a retry
// of the bind before any frames re-reads the SAME max, so a first-frame retry can
// never re-snapshot base against an already-advanced max (no double-rebase).
func TestBindLifetimeIdempotentWithinLifetime(t *testing.T) {
	s, _ := storeWithFake(t)
	sess := seedSession(t, s, "bindidem", "sess-bindidem")
	if _, err := s.BindLifetime(t.Context(), sess); err != nil {
		t.Fatalf("BindLifetime: %v", err)
	}
	if err := s.AppendTranscriptEntry(t.Context(), sess, 1, false, `{"e":1}`, "idem-1"); err != nil {
		t.Fatalf("append: %v", err)
	}
	// A retried bind for the SAME lifetime, before any further frames, must
	// re-read the same max (1) — not advance the base.
	base, err := s.BindLifetime(t.Context(), sess)
	if err != nil {
		t.Fatalf("re-bind: %v", err)
	}
	if base != 1 {
		t.Fatalf("re-bind base = %d, want 1 (idempotent within a lifetime)", base)
	}
}

// TestBindLifetimeUnknownSessionNotFound pins the not-found path for a bind
// against a session that was never recorded.
func TestBindLifetimeUnknownSessionNotFound(t *testing.T) {
	s, _ := storeWithFake(t)
	_, err := s.BindLifetime(t.Context(), "never-recorded")
	sentinelIs(t, err, ErrNotFound, "bind unknown session")
}

// TestSessionTranscriptSupersessionView pins the post-supersession read: after
// deltas -> checkpoint -> deltas, the view returns the checkpoint entry and its
// tail ONLY (never the pre-checkpoint deltas), so compacted history is not
// double-counted on resume.
func TestSessionTranscriptSupersessionView(t *testing.T) {
	s, _ := storeWithFake(t)
	sess := seedSession(t, s, "supersede", "sess-supersede")
	if _, err := s.BindLifetime(t.Context(), sess); err != nil {
		t.Fatalf("BindLifetime: %v", err)
	}
	// deltas 1,2 ; checkpoint 3 (fires PRIMARY flush of 1,2) ; deltas 4,5.
	appends := []struct {
		seq        uint64
		checkpoint bool
	}{{1, false}, {2, false}, {3, true}, {4, false}, {5, false}}
	for _, a := range appends {
		if err := s.AppendTranscriptEntry(t.Context(), sess, a.seq, a.checkpoint, fmt.Sprintf(`{"e":%d}`, a.seq), fmt.Sprintf("idem-%d", a.seq)); err != nil {
			t.Fatalf("append %d: %v", a.seq, err)
		}
	}
	got, err := s.SessionTranscript(t.Context(), sess)
	if err != nil {
		t.Fatalf("SessionTranscript: %v", err)
	}
	if !seqsEqual(transcriptSeqs(got), 3, 4, 5) {
		t.Fatalf("supersession view = %v, want [3 4 5] (checkpoint + tail, no double-count)", transcriptSeqs(got))
	}
	if !got[0].Checkpoint {
		t.Fatalf("view head entry_seq %d is not the checkpoint", got[0].EntrySeq)
	}
}

// TestSessionTranscriptUnknownNotFound pins the empty/unknown-session read as
// ErrNotFound.
func TestSessionTranscriptUnknownNotFound(t *testing.T) {
	s, _ := storeWithFake(t)
	_, err := s.SessionTranscript(t.Context(), "no-such-session")
	sentinelIs(t, err, ErrNotFound, "transcript of unknown session")
}

// ── Flush machinery ─────────────────────────────────────────────────────────

// TestPrimaryFlushOnCheckpointArrival pins the compaction flush embedded in
// AppendTranscriptEntry: when a checkpoint row commits, all pre-checkpoint
// hot-tail rows flush to ONE superseded segment + a manifest row, and the
// hot-tail is pruned to [checkpoint..now].
func TestPrimaryFlushOnCheckpointArrival(t *testing.T) {
	s, fake := storeWithFake(t)
	sess := seedSession(t, s, "primary", "sess-primary")
	if _, err := s.BindLifetime(t.Context(), sess); err != nil {
		t.Fatalf("BindLifetime: %v", err)
	}
	for _, a := range []struct {
		seq        uint64
		checkpoint bool
	}{{1, false}, {2, false}, {3, true}} {
		if err := s.AppendTranscriptEntry(t.Context(), sess, a.seq, a.checkpoint, fmt.Sprintf(`{"e":%d}`, a.seq), fmt.Sprintf("idem-%d", a.seq)); err != nil {
			t.Fatalf("append %d: %v", a.seq, err)
		}
	}
	// Hot-tail pruned to [3] (the checkpoint), the pre-checkpoint 1,2 flushed.
	if got := hotTailSeqs(t, s, sess); !seqsEqual(got, 3) {
		t.Fatalf("hot tail after primary flush = %v, want [3]", got)
	}
	// One superseded segment covering [1..2], and its object exists.
	man := manifestRows(t, s, sess)
	if len(man) != 1 || man[0].Kind != SegmentKindSuperseded || man[0].MinEntrySeq != 1 || man[0].MaxEntrySeq != 2 {
		t.Fatalf("manifest = %+v, want one superseded [1..2]", man)
	}
	if !fake.has(man[0].ObjectKey) {
		t.Fatalf("object %q missing from the store after flush", man[0].ObjectKey)
	}
	// Appending past the checkpoint grows the retained tail.
	if err := s.AppendTranscriptEntry(t.Context(), sess, 4, false, `{"e":4}`, "idem-4"); err != nil {
		t.Fatalf("append 4: %v", err)
	}
	if got := hotTailSeqs(t, s, sess); !seqsEqual(got, 3, 4) {
		t.Fatalf("hot tail = %v, want [3 4]", got)
	}
}

// TestFlushPutBeforePrune pins the ordering: a failed PutSegment leaves PG intact
// and records NO manifest row — nothing is pruned before the archive object is
// durable.
func TestFlushPutBeforePrune(t *testing.T) {
	s, fake := storeWithFake(t)
	sess := seedSession(t, s, "putfail", "sess-putfail")
	if _, err := s.BindLifetime(t.Context(), sess); err != nil {
		t.Fatalf("BindLifetime: %v", err)
	}
	for _, seq := range []uint64{1, 2} {
		if err := s.AppendTranscriptEntry(t.Context(), sess, seq, false, fmt.Sprintf(`{"e":%d}`, seq), fmt.Sprintf("idem-%d", seq)); err != nil {
			t.Fatalf("append %d: %v", seq, err)
		}
	}
	// Arm the PUT to fail, then land the checkpoint that triggers the flush.
	fake.putErr = errors.New("object store down")
	err := s.AppendTranscriptEntry(t.Context(), sess, 3, true, `{"e":3}`, "idem-3")
	if err == nil {
		t.Fatal("checkpoint append with a failing PutSegment must return an error")
	}
	// The checkpoint row committed (the append), but the flush left PG intact:
	// no prune, no manifest row.
	if got := hotTailSeqs(t, s, sess); !seqsEqual(got, 1, 2, 3) {
		t.Fatalf("hot tail after failed flush = %v, want [1 2 3] (nothing pruned)", got)
	}
	if man := manifestRows(t, s, sess); len(man) != 0 {
		t.Fatalf("manifest = %+v, want empty (no row recorded before a durable PUT)", man)
	}
}

// TestFlushCrashAfterPutBeforeCommitReRunCompletes simulates a crash between the
// PUT and the commit: the object exists at the deterministic key but PG holds
// NEITHER a manifest row NOR a prune. A re-run completes cleanly onto the same
// key.
func TestFlushCrashAfterPutBeforeCommitReRunCompletes(t *testing.T) {
	s, fake := storeWithFake(t)
	sess := seedSession(t, s, "crash", "sess-crash")
	if _, err := s.BindLifetime(t.Context(), sess); err != nil {
		t.Fatalf("BindLifetime: %v", err)
	}
	for _, seq := range []uint64{1, 2} {
		if err := s.AppendTranscriptEntry(t.Context(), sess, seq, false, fmt.Sprintf(`{"e":%d}`, seq), fmt.Sprintf("idem-%d", seq)); err != nil {
			t.Fatalf("append %d: %v", seq, err)
		}
	}
	// Simulate the completed PUT of a crashed flush attempt: the object is
	// present at the deterministic key, but the flush txn never committed.
	crashedKey := segmentKey(sess, 1, 2)
	if err := fake.PutSegment(t.Context(), crashedKey, []byte(`{"e":1}`+"\n"+`{"e":2}`)); err != nil {
		t.Fatalf("seed crashed PUT: %v", err)
	}
	// Neither a manifest row nor a prune yet.
	if man := manifestRows(t, s, sess); len(man) != 0 {
		t.Fatalf("manifest = %+v, want empty before the re-run commits", man)
	}
	if got := hotTailSeqs(t, s, sess); !seqsEqual(got, 1, 2) {
		t.Fatalf("hot tail = %v, want [1 2] before the re-run", got)
	}
	// Re-run via an explicit superseded flush of [1..2]: it re-PUTs the same key
	// and completes the manifest INSERT + prune.
	if err := s.FlushSuperseded(t.Context(), sess, 2, SegmentKindSuperseded); err != nil {
		t.Fatalf("re-run flush: %v", err)
	}
	man := manifestRows(t, s, sess)
	if len(man) != 1 || man[0].ObjectKey != crashedKey || man[0].Kind != SegmentKindSuperseded {
		t.Fatalf("manifest after re-run = %+v, want one superseded row at %q", man, crashedKey)
	}
	if got := hotTailSeqs(t, s, sess); len(got) != 0 {
		t.Fatalf("hot tail after re-run = %v, want empty (pruned)", got)
	}
}

// TestFlushManifestInsertIdempotentOnConflict proves the manifest INSERT is
// ON CONFLICT (session_id, object_key) DO NOTHING: a re-run that re-derives the
// SAME deterministic key onto an already-present manifest row does not wedge on a
// PK violation — it completes, leaving exactly one row.
func TestFlushManifestInsertIdempotentOnConflict(t *testing.T) {
	s, fake := storeWithFake(t)
	sess := seedSession(t, s, "onconflict", "sess-onconflict")
	if _, err := s.BindLifetime(t.Context(), sess); err != nil {
		t.Fatalf("BindLifetime: %v", err)
	}
	for _, seq := range []uint64{1, 2} {
		if err := s.AppendTranscriptEntry(t.Context(), sess, seq, false, fmt.Sprintf(`{"e":%d}`, seq), fmt.Sprintf("idem-%d", seq)); err != nil {
			t.Fatalf("append %d: %v", seq, err)
		}
	}
	key := segmentKey(sess, 1, 2)
	// Pre-seed a committed manifest row at the deterministic key (the object too)
	// WITHOUT pruning — the exact state ON CONFLICT DO NOTHING must survive a
	// defensive re-run of.
	if err := fake.PutSegment(t.Context(), key, []byte("seed")); err != nil {
		t.Fatalf("seed object: %v", err)
	}
	if _, err := s.pool.Exec(t.Context(),
		`INSERT INTO agent_session_archive_segments (session_id, object_key, min_entry_seq, max_entry_seq, kind)
		 VALUES ($1, $2, 1, 2, 'superseded')`, sess, key); err != nil {
		t.Fatalf("seed manifest row: %v", err)
	}
	// The re-run must not error on the PK; it completes and prunes.
	if err := s.FlushSuperseded(t.Context(), sess, 2, SegmentKindSuperseded); err != nil {
		t.Fatalf("re-run flush must not wedge on ON CONFLICT, got %v", err)
	}
	if man := manifestRows(t, s, sess); len(man) != 1 {
		t.Fatalf("manifest = %+v, want exactly one row (ON CONFLICT DO NOTHING)", man)
	}
	if got := hotTailSeqs(t, s, sess); len(got) != 0 {
		t.Fatalf("hot tail = %v, want empty (pruned on re-run)", got)
	}
}

// TestReconstructionIsPGOnlyAfterFlush pins that the normal resume read
// (SessionTranscript) touches PG only — the object store is never read even
// though a superseded segment sits in the archive.
func TestReconstructionIsPGOnlyAfterFlush(t *testing.T) {
	s, fake := storeWithFake(t)
	sess := seedSession(t, s, "pgonly", "sess-pgonly")
	if _, err := s.BindLifetime(t.Context(), sess); err != nil {
		t.Fatalf("BindLifetime: %v", err)
	}
	for _, a := range []struct {
		seq        uint64
		checkpoint bool
	}{{1, false}, {2, false}, {3, true}, {4, false}} {
		if err := s.AppendTranscriptEntry(t.Context(), sess, a.seq, a.checkpoint, fmt.Sprintf(`{"e":%d}`, a.seq), fmt.Sprintf("idem-%d", a.seq)); err != nil {
			t.Fatalf("append %d: %v", a.seq, err)
		}
	}
	// Any GetSegment from here fails the test.
	fake.failOnGet = t
	got, err := s.SessionTranscript(t.Context(), sess)
	if err != nil {
		t.Fatalf("SessionTranscript: %v", err)
	}
	if !seqsEqual(transcriptSeqs(got), 3, 4) {
		t.Fatalf("PG-only reconstruction = %v, want [3 4]", transcriptSeqs(got))
	}
	if fake.getCalled {
		t.Fatal("GetSegment was called during normal PG-only reconstruction")
	}
}

// TestSafetyValveFlushOnSizeCap pins the high-size-cap defense: with the cap
// lowered, an accumulating post-checkpoint hot-tail evicts its oldest chunk as a
// safety_valve segment (and prunes it), so the tail cannot grow without bound,
// while the newest entry is retained.
func TestSafetyValveFlushOnSizeCap(t *testing.T) {
	s, _ := storeWithFake(t)
	s.safetyValveCapBytes = 40 // well below the payload sizes below
	sess := seedSession(t, s, "valve", "sess-valve")
	if _, err := s.BindLifetime(t.Context(), sess); err != nil {
		t.Fatalf("BindLifetime: %v", err)
	}
	// A checkpoint anchors the post-checkpoint window, then oversize deltas.
	if err := s.AppendTranscriptEntry(t.Context(), sess, 1, true, `{"cp":true}`, "idem-cp"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	big := `{"pad":"` + strings.Repeat("x", 30) + `"}`
	for seq := uint64(2); seq <= 5; seq++ {
		if err := s.AppendTranscriptEntry(t.Context(), sess, seq, false, big, fmt.Sprintf("idem-%d", seq)); err != nil {
			t.Fatalf("append %d: %v", seq, err)
		}
	}
	seg, err := s.SafetyValveSegments(t.Context(), sess)
	if err != nil {
		t.Fatalf("SafetyValveSegments: %v", err)
	}
	if len(seg) == 0 {
		t.Fatal("no safety_valve segment recorded despite the tail exceeding the cap")
	}
	if seg[0].Kind != SegmentKindSafetyValve || seg[0].MinEntrySeq < 2 {
		t.Fatalf("safety valve segment = %+v, want a post-checkpoint (seq>=2) safety_valve", seg[0])
	}
	// The checkpoint (seq 1) and the newest entry (seq 5) are retained in PG.
	tail := hotTailSeqs(t, s, sess)
	if len(tail) == 0 || tail[0] != 1 || tail[len(tail)-1] != 5 {
		t.Fatalf("hot tail = %v, want the checkpoint (1) retained and the newest (5) kept", tail)
	}
}

// TestSafetyValveResupersededByLaterCheckpoint pins the re-mark: once a later
// checkpoint supersedes a safety_valve segment (its max < the new checkpoint
// seq), the PRIMARY flush re-marks it to superseded, so SafetyValveSegments no
// longer returns it — a stale pre-checkpoint segment is never spliced on resume.
func TestSafetyValveResupersededByLaterCheckpoint(t *testing.T) {
	s, _ := storeWithFake(t)
	s.safetyValveCapBytes = 40
	sess := seedSession(t, s, "remark", "sess-remark")
	if _, err := s.BindLifetime(t.Context(), sess); err != nil {
		t.Fatalf("BindLifetime: %v", err)
	}
	if err := s.AppendTranscriptEntry(t.Context(), sess, 1, true, `{"cp":1}`, "idem-cp1"); err != nil {
		t.Fatalf("checkpoint 1: %v", err)
	}
	big := `{"pad":"` + strings.Repeat("y", 30) + `"}`
	var last uint64
	for seq := uint64(2); seq <= 5; seq++ {
		if err := s.AppendTranscriptEntry(t.Context(), sess, seq, false, big, fmt.Sprintf("idem-%d", seq)); err != nil {
			t.Fatalf("append %d: %v", seq, err)
		}
		last = seq
	}
	seg, err := s.SafetyValveSegments(t.Context(), sess)
	if err != nil {
		t.Fatalf("SafetyValveSegments: %v", err)
	}
	if len(seg) == 0 {
		t.Fatal("expected a safety_valve segment before the later checkpoint")
	}
	// A later checkpoint above every evicted seq: its PRIMARY flush re-marks the
	// now pre-checkpoint safety_valve segment(s) to superseded.
	if err := s.AppendTranscriptEntry(t.Context(), sess, last+1, true, `{"cp":2}`, "idem-cp2"); err != nil {
		t.Fatalf("checkpoint 2: %v", err)
	}
	after, err := s.SafetyValveSegments(t.Context(), sess)
	if err != nil {
		t.Fatalf("SafetyValveSegments after re-mark: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("safety valve segments after later checkpoint = %+v, want none (re-marked to superseded)", after)
	}
	// The segment object is still catalogued — reclassified, not deleted.
	if man := manifestRows(t, s, sess); len(man) == 0 {
		t.Fatal("manifest is empty; the re-marked segment must remain as superseded")
	}
}

// TestSessionEndFlushRecordsWithoutPruning pins the teardown archival:
// FlushSuperseded(session_end) records one session_end segment for the retained
// tail but does NOT prune the PG hot-tail (a restarted agent still resumes fast
// from the authoritative PG tail).
func TestSessionEndFlushRecordsWithoutPruning(t *testing.T) {
	s, fake := storeWithFake(t)
	sess := seedSession(t, s, "sessend", "sess-sessend")
	if _, err := s.BindLifetime(t.Context(), sess); err != nil {
		t.Fatalf("BindLifetime: %v", err)
	}
	for _, a := range []struct {
		seq        uint64
		checkpoint bool
	}{{1, true}, {2, false}, {3, false}} {
		if err := s.AppendTranscriptEntry(t.Context(), sess, a.seq, a.checkpoint, fmt.Sprintf(`{"e":%d}`, a.seq), fmt.Sprintf("idem-%d", a.seq)); err != nil {
			t.Fatalf("append %d: %v", a.seq, err)
		}
	}
	if err := s.FlushSuperseded(t.Context(), sess, 3, SegmentKindSessionEnd); err != nil {
		t.Fatalf("session_end flush: %v", err)
	}
	// A session_end segment recorded over [1..3], object present.
	man := manifestRows(t, s, sess)
	if len(man) != 1 || man[0].Kind != SegmentKindSessionEnd || man[0].MinEntrySeq != 1 || man[0].MaxEntrySeq != 3 {
		t.Fatalf("manifest = %+v, want one session_end [1..3]", man)
	}
	if !fake.has(man[0].ObjectKey) {
		t.Fatalf("session_end object %q missing", man[0].ObjectKey)
	}
	// NOT pruned: the whole retained tail stays in PG.
	if got := hotTailSeqs(t, s, sess); !seqsEqual(got, 1, 2, 3) {
		t.Fatalf("hot tail after session_end = %v, want [1 2 3] (not pruned)", got)
	}
	// session_end is never resume-eligible.
	if seg, err := s.SafetyValveSegments(t.Context(), sess); err != nil || len(seg) != 0 {
		t.Fatalf("SafetyValveSegments = %v, %v; want empty (session_end excluded)", seg, err)
	}
}
