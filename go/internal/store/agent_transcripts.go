package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The durable TWO-TIER transcript store (RIG-1667 T4). A Postgres HOT TAIL
// (agent_session_transcript_entries) holds [latest checkpoint .. now] = the
// normal resume set; superseded/evicted/ended history is flushed as verbatim
// JSONL segments to an object store behind the ObjectStore seam and indexed by
// the archive manifest (agent_session_archive_segments). Resume reads the PG
// hot-tail ONLY in normal operation (T5); the archive is the permanent,
// analytics-ready record.
//
// entry_seq is SESSION-scoped: the wire entry_seq is agent-stamped monotonic
// from 1 per container lifetime, and the store rebases it onto the session's
// stored maximum at lifetime bind (BindLifetime writes agent_sessions.
// base_entry_seq; AppendTranscriptEntry reads it per frame and persists at
// base + lifetimeSeq), so the persisted entry_seq is monotonic per session
// across resumes and the PK (session_id, entry_seq) holds.

// defaultSafetyValveCapBytes is the default HIGH size cap on the post-checkpoint
// hot-tail, in bytes of entry_json (octet length). It sits ABOVE the normal
// compaction window so it never engages in normal operation — pure
// defense-in-depth: if compaction is disabled/bugged/never-trips, the oldest
// post-checkpoint chunk is evicted to a safety_valve segment so the hot-tail
// cannot grow without bound. TUNABLE: the record freezes the mechanism, not the
// value (design.md T4 "the cap value is a tuning parameter, not freeze-scope").
// Store.safetyValveCapBytes carries it so it can be tuned (and lowered by tests).
const defaultSafetyValveCapBytes = 64 << 20 // 64 MiB

// SegmentKind classifies an archive-manifest segment, matching the SQL CHECK
// constraint on agent_session_archive_segments.kind. Only SegmentKindSafetyValve
// segments are read back on resume (T5); SegmentKindSuperseded and
// SegmentKindSessionEnd are analytics-only.
type SegmentKind string

const (
	// SegmentKindSuperseded holds pre-checkpoint history flushed at compaction;
	// NEVER read on resume (the read view starts at the latest checkpoint).
	SegmentKindSuperseded SegmentKind = "superseded"
	// SegmentKindSafetyValve holds post-checkpoint entries evicted by the high
	// size cap; spliced back on resume (T5). A later checkpoint re-marks any now
	// pre-checkpoint safety_valve row to superseded, so a safety_valve row is
	// post-latest-checkpoint by construction.
	SegmentKindSafetyValve SegmentKind = "safety_valve"
	// SegmentKindSessionEnd archives the retained post-checkpoint tail at
	// teardown for analytics completeness; NEVER read on resume (the PG tail
	// stays authoritative).
	SegmentKindSessionEnd SegmentKind = "session_end"
)

// valid reports whether k is one of the three defined kinds (the SQL CHECK
// mirror, enforced at the store door before a write).
func (k SegmentKind) valid() bool {
	switch k {
	case SegmentKindSuperseded, SegmentKindSafetyValve, SegmentKindSessionEnd:
		return true
	default:
		return false
	}
}

// TranscriptEntryRow is one hot-tail entry as returned by SessionTranscript.
// EntryJSON is the opaque, verbatim SDK JSONL — never parsed by the store.
type TranscriptEntryRow struct {
	EntrySeq   uint64
	Checkpoint bool
	EntryJSON  string
}

// ArchiveSegmentRow is one archive-manifest row as returned by
// SafetyValveSegments. The object body lives in the object store under ObjectKey;
// the manifest holds only the locator and the [min..max] seq range it covers.
type ArchiveSegmentRow struct {
	ObjectKey   string
	MinEntrySeq uint64
	MaxEntrySeq uint64
	Kind        SegmentKind
}

// ObjectStore is the server-internal, endpoint-agnostic archive seam (faked in
// store tests; a real Garage/R2/MinIO client in production, wired by slice B).
// The store never holds bucket/endpoint/credentials directly — only this seam.
type ObjectStore interface {
	PutSegment(ctx context.Context, key string, body []byte) error
	GetSegment(ctx context.Context, key string) ([]byte, error)
}

// SetObjectStore injects the archive-tier object-store client. It is set once at
// construction (slice B wires the real client; store tests inject an in-memory
// fake) before the store serves any flush. A nil object store fails a flush
// loudly rather than silently dropping the archive.
func (s *Store) SetObjectStore(os ObjectStore) {
	s.objectStore = os
}

// SetSafetyValveCapBytesForTest lowers the safety-valve size cap so a test can
// trip the valve without writing a huge transcript. The ForTest suffix marks it
// test-only: production tunes the cap at Open via defaultSafetyValveCapBytes, and
// this setter exists solely so out-of-package tests (e.g. the server resume
// pgtest) can exercise the S3 fallback leg end-to-end.
func (s *Store) SetSafetyValveCapBytesForTest(n int) {
	s.safetyValveCapBytes = n
}

// BindLifetime snapshots the session's rebase base ONCE at lifetime bind: it
// writes base = max(entry_seq) over the session's transcript rows onto
// agent_sessions.base_entry_seq and returns it. AppendTranscriptEntry then reads
// that base per frame and persists at base + lifetimeSeq, so a lifetime's
// agent-stamped sequence rebases onto the session's stored maximum and the PK
// (session_id, entry_seq) holds across resumes. Idempotent within a lifetime:
// called once before any of the lifetime's frames, a retry re-reads the same max
// -> the same base (no double-rebase). An unknown session_id is ErrNotFound.
func (s *Store) BindLifetime(ctx context.Context, sessionID string) (uint64, error) {
	if sessionID == "" {
		return 0, fmt.Errorf("%w: session id is required", ErrInvalidArgument)
	}
	var base int64
	if err := s.pool.QueryRow(ctx,
		`UPDATE agent_sessions
		    SET base_entry_seq = COALESCE(
		            (SELECT MAX(entry_seq)
		               FROM agent_session_transcript_entries
		              WHERE session_id = $1), 0)
		  WHERE session_id = $1
		RETURNING base_entry_seq`,
		sessionID,
	).Scan(&base); err != nil {
		if noRows(err) {
			return 0, fmt.Errorf("%w: session %q", ErrNotFound, sessionID)
		}
		return 0, fmt.Errorf("store: bind lifetime: %w", err)
	}
	return toUint64(base), nil
}

// AppendTranscriptEntry persists one emitted entry at-most-once on
// idempotencyKey (a duplicate key is a silent success — the retry dedup).
// lifetimeSeq is the agent-stamped per-lifetime sequence; the store rebases it
// onto the session's base at lifetime bind (entry_seq = base + lifetimeSeq).
// Unknown session_id is ErrInvalidArgument (the FK). A distinct entry re-using an
// existing (session_id, entry_seq) is ErrConflict (the PK). When a new
// checkpoint row commits, the PRIMARY flush fires (all pre-checkpoint hot-tail
// rows -> one superseded segment, pruned); otherwise the SAFETY-VALVE cap is
// checked. A deduped retry short-circuits before either flush.
func (s *Store) AppendTranscriptEntry(ctx context.Context, sessionID string, lifetimeSeq uint64, checkpoint bool, entryJSON, idempotencyKey string) error {
	switch {
	case sessionID == "":
		return fmt.Errorf("%w: session id is required", ErrInvalidArgument)
	case idempotencyKey == "":
		return fmt.Errorf("%w: idempotency key is required", ErrInvalidArgument)
	case entryJSON == "":
		return fmt.Errorf("%w: entry json is required", ErrInvalidArgument)
	case lifetimeSeq == 0:
		return fmt.Errorf("%w: lifetime seq must be >= 1 (agent-stamped monotonic from 1)", ErrInvalidArgument)
	}

	base, err := s.sessionBase(ctx, sessionID)
	if err != nil {
		return err
	}
	if lifetimeSeq > math.MaxInt64 || base > uint64(math.MaxInt64)-lifetimeSeq {
		return fmt.Errorf("%w: entry_seq %d out of range", ErrInvalidArgument, lifetimeSeq)
	}
	entrySeq := base + lifetimeSeq

	tag, err := s.pool.Exec(ctx,
		`INSERT INTO agent_session_transcript_entries
		            (session_id, entry_seq, checkpoint, entry_json, idempotency_key)
		     VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		sessionID, toInt64(entrySeq), checkpoint, entryJSON, idempotencyKey,
	)
	if err != nil {
		// A unique_violation here is the PK (session_id, entry_seq): a distinct
		// entry re-using a stamped seq — a real conflict, not the keyed dedup
		// (that is absorbed by ON CONFLICT above). An FK violation means the
		// session vanished between the base read and the insert.
		if pgErrIs(err, pgUniqueViolation) {
			return fmt.Errorf("%w: session %q already has entry_seq %d", ErrConflict, sessionID, entrySeq)
		}
		if pgErrIs(err, pgForeignKeyViolation) {
			return fmt.Errorf("%w: session %q does not exist", ErrInvalidArgument, sessionID)
		}
		return fmt.Errorf("store: append transcript entry: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Duplicate idempotency_key: the retry dedup. Silent success, and NO
		// flush — a retried checkpoint frame must not re-invoke the PRIMARY
		// flush (design.md T4: it short-circuits before it). If the ORIGINAL
		// checkpoint committed its row but its primaryFlush then failed, this
		// retried commit also skips the flush — safe, because the next
		// checkpoint's primaryFlush (or the session-end flush) re-covers that
		// pre-checkpoint range, and the PG-only read view stays correct throughout.
		return nil
	}

	if checkpoint {
		return s.primaryFlush(ctx, sessionID, entrySeq)
	}
	return s.maybeSafetyValve(ctx, sessionID)
}

// SessionTranscript returns the hot-tail post-supersession view: the latest
// checkpoint entry (if any) followed by every later delta held in PG, ordered by
// entry_seq — the PG-only normal resume set. With no checkpoint the whole
// retained hot-tail is returned. An unknown or empty session is ErrNotFound.
func (s *Store) SessionTranscript(ctx context.Context, sessionID string) ([]TranscriptEntryRow, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("%w: session id is required", ErrInvalidArgument)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT entry_seq, checkpoint, entry_json
		   FROM agent_session_transcript_entries
		  WHERE session_id = $1
		    AND entry_seq >= COALESCE(
		            (SELECT MAX(entry_seq)
		               FROM agent_session_transcript_entries
		              WHERE session_id = $1 AND checkpoint), 0)
		  ORDER BY entry_seq`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: read session transcript: %w", err)
	}
	defer rows.Close()
	out, err := scanTranscriptRows(rows)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: session %q", ErrNotFound, sessionID)
	}
	return out, nil
}

// scanTranscriptRows scans hot-tail rows into TranscriptEntryRow values. Shared
// by SessionTranscript (pool read) and SessionResumeSnapshot (tx read) so the
// column list and scan live in one place.
func scanTranscriptRows(rows pgx.Rows) ([]TranscriptEntryRow, error) {
	var out []TranscriptEntryRow
	for rows.Next() {
		var (
			seq        int64
			checkpoint bool
			entryJSON  string
		)
		if err := rows.Scan(&seq, &checkpoint, &entryJSON); err != nil {
			return nil, fmt.Errorf("store: scan transcript entry: %w", err)
		}
		out = append(out, TranscriptEntryRow{
			EntrySeq:   toUint64(seq),
			Checkpoint: checkpoint,
			EntryJSON:  entryJSON,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate transcript entries: %w", err)
	}
	return out, nil
}

// FlushSuperseded writes the hot-tail entries up to uptoEntrySeq as one
// verbatim-JSONL segment via the object-store seam; after the PUT is confirmed it
// INSERTs the manifest row (kind) and — for every kind except session_end —
// prunes those PG rows, in ONE transaction (both commit or neither). session_end
// records the segment WITHOUT pruning (the PG tail stays authoritative for
// resume). Crash-safe: a crash between PUT and commit re-flushes onto the
// deterministic [min..max] key and the manifest INSERT is
// ON CONFLICT (session_id, object_key) DO NOTHING, so a re-run cannot wedge; a
// crash before the PUT leaves PG intact. This is the SESSION-END / external flush
// entry point (the call-site is slice C); the PRIMARY and SAFETY-VALVE flushes
// fire internally from AppendTranscriptEntry.
func (s *Store) FlushSuperseded(ctx context.Context, sessionID string, uptoEntrySeq uint64, kind SegmentKind) error {
	if sessionID == "" {
		return fmt.Errorf("%w: session id is required", ErrInvalidArgument)
	}
	if !kind.valid() {
		return fmt.Errorf("%w: unknown segment kind %q", ErrInvalidArgument, kind)
	}
	prune := kind != SegmentKindSessionEnd
	return s.flushUpto(ctx, sessionID, 0, uptoEntrySeq, kind, prune, 0)
}

// SafetyValveSegments returns the manifest rows for a session whose kind is
// safety_valve (post-checkpoint entries evicted to the object store by the size
// cap), ordered by min_entry_seq — the only segments T5's reconstructor fetches
// on resume. By construction these are all post-latest-checkpoint: the PRIMARY
// flush re-marks any safety_valve row a later checkpoint superseded to
// superseded, so a stale pre-checkpoint segment is never returned. Empty for a
// normal session (superseded and session_end segments are excluded).
func (s *Store) SafetyValveSegments(ctx context.Context, sessionID string) ([]ArchiveSegmentRow, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("%w: session id is required", ErrInvalidArgument)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT object_key, min_entry_seq, max_entry_seq, kind
		   FROM agent_session_archive_segments
		  WHERE session_id = $1 AND kind = $2
		  ORDER BY min_entry_seq`,
		sessionID, string(SegmentKindSafetyValve),
	)
	if err != nil {
		return nil, fmt.Errorf("store: read safety valve segments: %w", err)
	}
	defer rows.Close()
	return scanSegmentRows(rows)
}

// scanSegmentRows scans safety_valve manifest rows into ArchiveSegmentRow
// values. Shared by SafetyValveSegments (pool read) and SessionResumeSnapshot
// (tx read) so the column list and scan live in one place.
func scanSegmentRows(rows pgx.Rows) ([]ArchiveSegmentRow, error) {
	var out []ArchiveSegmentRow
	for rows.Next() {
		var (
			objectKey string
			minSeq    int64
			maxSeq    int64
			kind      string
		)
		if err := rows.Scan(&objectKey, &minSeq, &maxSeq, &kind); err != nil {
			return nil, fmt.Errorf("store: scan archive segment: %w", err)
		}
		out = append(out, ArchiveSegmentRow{
			ObjectKey:   objectKey,
			MinEntrySeq: toUint64(minSeq),
			MaxEntrySeq: toUint64(maxSeq),
			Kind:        SegmentKind(kind),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate archive segments: %w", err)
	}
	return out, nil
}

// SessionResumeSnapshot is the ATOMIC two-tier read the T5 resume reconstructor
// (runnerhub.ReconstructSessionBody) uses: it takes the PG hot-tail and the
// safety_valve manifest inside ONE read-only REPEATABLE READ snapshot
// transaction, so a concurrent safety-valve flush (which prunes PG rows and
// inserts a manifest row in one tx) can never commit BETWEEN the two reads and
// corrupt the reconstructed body (double-count an entry, or drop one). Under the
// snapshot the at-rest disjointness invariant — safety_valve seqs are disjoint
// from PG-tail seqs — is observed as a single consistent DB state, closing the
// flush-between-reads race.
//
// It preserves SessionTranscript's ErrNotFound-on-empty-tail behavior and
// short-circuits before reading segments, mirroring the reconstructor's normal
// path. Returns (tail, segments, err).
func (s *Store) SessionResumeSnapshot(ctx context.Context, sessionID string) ([]TranscriptEntryRow, []ArchiveSegmentRow, error) {
	if sessionID == "" {
		return nil, nil, fmt.Errorf("%w: session id is required", ErrInvalidArgument)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, fmt.Errorf("store: begin resume snapshot: %w", err)
	}
	// Rollback is a no-op after the successful Commit below (the store's
	// rollback-after-commit convention); on any early return it aborts the tx.
	defer func() { _ = tx.Rollback(ctx) }()

	tailRows, err := tx.Query(ctx,
		`SELECT entry_seq, checkpoint, entry_json
		   FROM agent_session_transcript_entries
		  WHERE session_id = $1
		    AND entry_seq >= COALESCE(
		            (SELECT MAX(entry_seq)
		               FROM agent_session_transcript_entries
		              WHERE session_id = $1 AND checkpoint), 0)
		  ORDER BY entry_seq`,
		sessionID,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("store: read session transcript: %w", err)
	}
	tail, err := scanTranscriptRows(tailRows)
	tailRows.Close()
	if err != nil {
		return nil, nil, err
	}
	if len(tail) == 0 {
		return nil, nil, fmt.Errorf("%w: session %q", ErrNotFound, sessionID)
	}

	segRows, err := tx.Query(ctx,
		`SELECT object_key, min_entry_seq, max_entry_seq, kind
		   FROM agent_session_archive_segments
		  WHERE session_id = $1 AND kind = $2
		  ORDER BY min_entry_seq`,
		sessionID, string(SegmentKindSafetyValve),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("store: read safety valve segments: %w", err)
	}
	segments, err := scanSegmentRows(segRows)
	segRows.Close()
	if err != nil {
		return nil, nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("store: commit resume snapshot: %w", err)
	}
	return tail, segments, nil
}

// ReadArchiveSegment fetches one archive segment's verbatim-JSONL body from the
// object store by its manifest ObjectKey — the thin read T5's resume
// reconstructor uses to pull back a safety_valve segment (SafetyValveSegments
// yields the keys). The store holds the injected ObjectStore seam; this exposes
// a read over it WITHOUT widening the seam the store depends on. A nil object
// store fails loudly rather than returning an empty body a resume would silently
// load as a truncated transcript, mirroring flushUpto's nil guard.
func (s *Store) ReadArchiveSegment(ctx context.Context, objectKey string) ([]byte, error) {
	if objectKey == "" {
		return nil, fmt.Errorf("%w: object key is required", ErrInvalidArgument)
	}
	if s.objectStore == nil {
		return nil, errors.New("store: object store not configured for archive read")
	}
	body, err := s.objectStore.GetSegment(ctx, objectKey)
	if err != nil {
		return nil, fmt.Errorf("store: read archive segment: %w", err)
	}
	return body, nil
}

// sessionBase reads the write-once rebase base for a session. An unknown session
// is ErrInvalidArgument — the FK the AppendTranscriptEntry contract names.
func (s *Store) sessionBase(ctx context.Context, sessionID string) (uint64, error) {
	var base int64
	if err := s.pool.QueryRow(ctx,
		`SELECT base_entry_seq FROM agent_sessions WHERE session_id = $1`,
		sessionID,
	).Scan(&base); err != nil {
		if noRows(err) {
			return 0, fmt.Errorf("%w: session %q does not exist", ErrInvalidArgument, sessionID)
		}
		return 0, fmt.Errorf("store: read session base: %w", err)
	}
	return toUint64(base), nil
}

// primaryFlush is the compaction/checkpoint-arrival flush: when a checkpoint row
// at checkpointSeq commits, it flushes ALL pre-checkpoint hot-tail rows
// (entry_seq < checkpointSeq) as one superseded segment and prunes them, and in
// the SAME transaction re-marks any existing safety_valve manifest row whose
// max_entry_seq < checkpointSeq to superseded — so no crash window leaves a
// superseded safety_valve segment resume-eligible. Self-healing: it targets the
// rows STILL IN the hot-tail, so a crash before this commits is idempotently
// reclaimed by the next compaction.
func (s *Store) primaryFlush(ctx context.Context, sessionID string, checkpointSeq uint64) error {
	return s.flushUpto(ctx, sessionID, 0, checkpointSeq-1, SegmentKindSuperseded, true, checkpointSeq)
}

// maybeSafetyValve is the high-size-cap check on the same AppendTranscriptEntry
// path. Two-phase: a CHEAP single-scalar aggregate sums the post-checkpoint
// hot-tail bytes in ONE query and returns early when under the cap (the common
// path — the valve sits above the compaction window, so it never engages in
// normal operation, and no per-row materialization happens). ONLY when the
// scalar exceeds the cap does it run the per-row query to pick the eviction cut
// point, evict the oldest post-checkpoint chunk as a safety_valve segment, and
// prune it, always retaining the newest entry.
func (s *Store) maybeSafetyValve(ctx context.Context, sessionID string) error {
	cpSeq, err := s.latestCheckpointSeq(ctx, sessionID)
	if err != nil {
		return err
	}
	// Cheap gate: sum the post-checkpoint byte total in ONE scalar (no per-row
	// materialization on the common path). The valve sits above the compaction
	// window, so in normal operation this scalar is under the cap and returns here.
	var tailBytes int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(octet_length(entry_json)), 0)
		   FROM agent_session_transcript_entries
		  WHERE session_id = $1 AND entry_seq > $2`,
		sessionID, toInt64(cpSeq),
	).Scan(&tailBytes); err != nil {
		return fmt.Errorf("store: measure hot tail: %w", err)
	}
	if tailBytes <= int64(s.safetyValveCapBytes) {
		return nil
	}
	// Over the cap: NOW materialize the rows to pick the eviction cut point.
	rows, err := s.pool.Query(ctx,
		`SELECT entry_seq, octet_length(entry_json)
		   FROM agent_session_transcript_entries
		  WHERE session_id = $1 AND entry_seq > $2
		  ORDER BY entry_seq`,
		sessionID, toInt64(cpSeq),
	)
	if err != nil {
		return fmt.Errorf("store: measure hot tail: %w", err)
	}
	type sized struct {
		seq   uint64
		bytes int
	}
	var (
		entries []sized
		total   int
	)
	for rows.Next() {
		var (
			seq   int64
			bytes int
		)
		if err := rows.Scan(&seq, &bytes); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan hot tail size: %w", err)
		}
		entries = append(entries, sized{seq: toUint64(seq), bytes: bytes})
		total += bytes
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate hot tail size: %w", err)
	}
	if total <= s.safetyValveCapBytes {
		return nil
	}

	// Evict the oldest post-checkpoint rows until the remainder fits under the
	// cap, but never the newest entry (the just-appended one) — the eviction is
	// a contiguous oldest chunk with a deterministic upper bound.
	remaining := total
	var upto uint64
	for i := range len(entries) - 1 {
		if remaining <= s.safetyValveCapBytes {
			break
		}
		upto = entries[i].seq
		remaining -= entries[i].bytes
	}
	if upto == 0 {
		return nil
	}
	return s.flushUpto(ctx, sessionID, cpSeq+1, upto, SegmentKindSafetyValve, true, 0)
}

// latestCheckpointSeq returns the entry_seq of the session's newest checkpoint
// row, or 0 when the session has no checkpoint.
func (s *Store) latestCheckpointSeq(ctx context.Context, sessionID string) (uint64, error) {
	var cp int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(entry_seq), 0)
		   FROM agent_session_transcript_entries
		  WHERE session_id = $1 AND checkpoint`,
		sessionID,
	).Scan(&cp); err != nil {
		return 0, fmt.Errorf("store: read latest checkpoint: %w", err)
	}
	return toUint64(cp), nil
}

// SessionMaxEntrySeq returns the highest entry_seq held for a session, or 0 when
// it has no transcript rows. It is the session-end flush's upto bound: at
// StopAgentSession the whole remaining hot-tail window [.. max] is archived as
// one session_end segment (FlushSuperseded, prune=false) so history is complete
// for analytics. Mirrors latestCheckpointSeq's shape, without the checkpoint
// filter.
func (s *Store) SessionMaxEntrySeq(ctx context.Context, sessionID string) (uint64, error) {
	var maxSeq int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(entry_seq), 0)
		   FROM agent_session_transcript_entries
		  WHERE session_id = $1`,
		sessionID,
	).Scan(&maxSeq); err != nil {
		return 0, fmt.Errorf("store: read session max entry seq: %w", err)
	}
	return toUint64(maxSeq), nil
}

// flushUpto is the shared flush machinery (PUT-before-prune). It reads the rows
// in [fromEntrySeq..uptoEntrySeq], writes them as one verbatim-JSONL segment to
// the object store under a deterministic [min..max] key, and only after the PUT
// confirms, in ONE transaction, INSERTs the manifest row
// (ON CONFLICT (session_id, object_key) DO NOTHING), optionally prunes the
// flushed rows, and optionally re-marks stale safety_valve rows to superseded
// (remarkBelow > 0, the PRIMARY flush). With no rows in range it is a no-op
// except for a requested remark, which still applies.
func (s *Store) flushUpto(ctx context.Context, sessionID string, fromEntrySeq, uptoEntrySeq uint64, kind SegmentKind, prune bool, remarkBelow uint64) error {
	seqs, body, err := s.collectSegment(ctx, sessionID, fromEntrySeq, uptoEntrySeq)
	if err != nil {
		return err
	}
	if len(seqs) == 0 {
		// Nothing to flush. A PRIMARY flush with no pre-checkpoint rows may still
		// owe a safety_valve re-mark (a stale segment below the new checkpoint).
		if remarkBelow > 0 {
			return s.remarkSafetyValveSuperseded(ctx, sessionID, remarkBelow)
		}
		return nil
	}
	minSeq, maxSeq := seqs[0], seqs[len(seqs)-1]

	if s.objectStore == nil {
		return errors.New("store: object store not configured for transcript flush")
	}
	key := segmentKey(sessionID, minSeq, maxSeq)
	// PUT-before-prune: the segment must be durable in the object store before
	// any PG mutation. A failed PUT returns here with PG untouched.
	if err := s.objectStore.PutSegment(ctx, key, body); err != nil {
		return fmt.Errorf("store: flush put segment: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin flush: %w", err)
	}
	// A rollback after a successful Commit is a no-op returning ErrTxClosed —
	// not actionable, so it is discarded (the store-wide convention).
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_session_archive_segments
		            (session_id, object_key, min_entry_seq, max_entry_seq, kind)
		     VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (session_id, object_key) DO NOTHING`,
		sessionID, key, toInt64(minSeq), toInt64(maxSeq), string(kind),
	); err != nil {
		return fmt.Errorf("store: insert archive segment: %w", err)
	}
	if prune {
		if _, err := tx.Exec(ctx,
			`DELETE FROM agent_session_transcript_entries
			  WHERE session_id = $1 AND entry_seq >= $2 AND entry_seq <= $3`,
			sessionID, toInt64(minSeq), toInt64(maxSeq),
		); err != nil {
			return fmt.Errorf("store: prune flushed entries: %w", err)
		}
	}
	if remarkBelow > 0 {
		if _, err := tx.Exec(ctx, remarkSafetyValveSQL, sessionID, toInt64(remarkBelow)); err != nil {
			return fmt.Errorf("store: re-mark stale safety valve: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit flush: %w", err)
	}
	return nil
}

// collectSegment reads the entries in [from..upto] for a session in seq order,
// returning their ordered seqs and the verbatim-JSONL body (entry_json joined by
// newlines). Empty seqs means no rows in range.
func (s *Store) collectSegment(ctx context.Context, sessionID string, fromEntrySeq, uptoEntrySeq uint64) ([]uint64, []byte, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT entry_seq, entry_json
		   FROM agent_session_transcript_entries
		  WHERE session_id = $1 AND entry_seq >= $2 AND entry_seq <= $3
		  ORDER BY entry_seq`,
		sessionID, toInt64(fromEntrySeq), toInt64(uptoEntrySeq),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("store: read segment entries: %w", err)
	}
	defer rows.Close()

	var (
		seqs  []uint64
		jsons []string
	)
	for rows.Next() {
		var (
			seq       int64
			entryJSON string
		)
		if err := rows.Scan(&seq, &entryJSON); err != nil {
			return nil, nil, fmt.Errorf("store: scan segment entry: %w", err)
		}
		seqs = append(seqs, toUint64(seq))
		jsons = append(jsons, entryJSON)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: iterate segment entries: %w", err)
	}
	if len(seqs) == 0 {
		return nil, nil, nil
	}
	return seqs, []byte(strings.Join(jsons, "\n")), nil
}

// remarkSafetyValveSQL re-marks a session's safety_valve manifest rows whose
// max_entry_seq is below a checkpoint seq to superseded — the S3 object is
// unmoved, only reclassified from resume-eligible to analytics-only. $1 session,
// $2 the checkpoint seq.
const remarkSafetyValveSQL = `
	UPDATE agent_session_archive_segments
	   SET kind = 'superseded'
	 WHERE session_id = $1 AND kind = 'safety_valve' AND max_entry_seq < $2`

// remarkSafetyValveSuperseded applies the safety_valve re-mark on its own when a
// PRIMARY flush had no pre-checkpoint rows to flush but still owes the re-mark.
func (s *Store) remarkSafetyValveSuperseded(ctx context.Context, sessionID string, belowSeq uint64) error {
	if _, err := s.pool.Exec(ctx, remarkSafetyValveSQL, sessionID, toInt64(belowSeq)); err != nil {
		return fmt.Errorf("store: re-mark stale safety valve: %w", err)
	}
	return nil
}

// segmentKey is the deterministic object key for the segment covering
// [minSeq..maxSeq] of a session, prefixed sessions/<session_id>/ (bucket and
// endpoint are server config, not per-key). Deterministic per seq range so a
// crash between the PUT and the flush commit re-PUTs onto the SAME key (a
// harmless overwrite), the crash-safety the record relies on.
func segmentKey(sessionID string, minSeq, maxSeq uint64) string {
	return fmt.Sprintf("sessions/%s/%d-%d.jsonl", sessionID, minSeq, maxSeq)
}

// toInt64 narrows an entry_seq to the BIGINT domain for a query parameter.
// AppendTranscriptEntry rejects any seq > math.MaxInt64 before it reaches here,
// so the narrowing never wraps negative.
func toInt64(seq uint64) int64 {
	return int64(seq) //nolint:gosec // G115: callers guard seq <= math.MaxInt64 before narrowing (AppendTranscriptEntry range check)
}

// toUint64 widens a stored BIGINT entry_seq (always positive) back to uint64.
func toUint64(seq int64) uint64 {
	return uint64(seq) //nolint:gosec // G115: a stored entry_seq is a positive BIGINT, within the uint64 domain
}
