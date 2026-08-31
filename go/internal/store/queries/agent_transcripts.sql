-- Agent-transcript queries (sqlc adoption T5, RIG-3034). These replace the
-- inline SQL literals in internal/store/agent_transcripts.go; the hand-written
-- Store methods keep their exact signatures and own the two-tier flush
-- orchestration (PUT-before-prune, the RepeatableRead/ReadOnly resume snapshot
-- tx, the safety-valve cut-point loop) plus the uint64<->int64 seq narrowing
-- (toInt64/toUint64). They map generated rows into TranscriptEntryRow and
-- ArchiveSegmentRow (via the transcriptRowsFromDB/segmentRowsFromDB helpers).
--
-- COALESCE(MAX/SUM(...), 0) sites carry an explicit ::BIGINT cast so sqlc types
-- them int64 rather than interface{} (mirrors messages.sql MessagesHeadSeq). The
-- SessionTranscript and SafetyValveSegments reads are each issued on BOTH the
-- pool (the eponymous method) and a snapshot tx (SessionResumeSnapshot, via
-- WithTx), so one generated query backs both call sites.

-- name: BindLifetime :one
UPDATE agent_sessions
   SET base_entry_seq = COALESCE(
           (SELECT MAX(te.entry_seq)
              FROM agent_session_transcript_entries te
             WHERE te.session_id = $1), 0)
 WHERE session_id = $1
RETURNING base_entry_seq;

-- name: SessionBase :one
SELECT base_entry_seq FROM agent_sessions WHERE session_id = $1;

-- name: InsertTranscriptEntry :execrows
INSERT INTO agent_session_transcript_entries
            (session_id, entry_seq, checkpoint, entry_json, idempotency_key)
     VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (idempotency_key) DO NOTHING;

-- name: SessionTranscript :many
SELECT e.entry_seq, e.checkpoint, e.entry_json
   FROM agent_session_transcript_entries e
  WHERE e.session_id = $1
    AND e.entry_seq >= COALESCE(
            (SELECT MAX(cp.entry_seq)
               FROM agent_session_transcript_entries cp
              WHERE cp.session_id = $1 AND cp.checkpoint), 0)
  ORDER BY entry_seq;

-- name: SafetyValveSegments :many
SELECT object_key, min_entry_seq, max_entry_seq, kind
   FROM agent_session_archive_segments
  WHERE session_id = $1 AND kind = $2
  ORDER BY min_entry_seq;

-- name: HotTailBytes :one
SELECT COALESCE(SUM(octet_length(entry_json)), 0)::BIGINT AS bytes
   FROM agent_session_transcript_entries
  WHERE session_id = $1 AND entry_seq > $2;

-- name: HotTailSizes :many
SELECT entry_seq, octet_length(entry_json)::BIGINT AS bytes
   FROM agent_session_transcript_entries
  WHERE session_id = $1 AND entry_seq > $2
  ORDER BY entry_seq;

-- name: LatestCheckpointSeq :one
SELECT COALESCE(MAX(entry_seq), 0)::BIGINT AS seq
   FROM agent_session_transcript_entries
  WHERE session_id = $1 AND checkpoint;

-- name: SessionMaxEntrySeq :one
SELECT COALESCE(MAX(entry_seq), 0)::BIGINT AS seq
   FROM agent_session_transcript_entries
  WHERE session_id = $1;

-- name: CollectSegment :many
SELECT entry_seq, entry_json
   FROM agent_session_transcript_entries
  WHERE session_id = $1 AND entry_seq >= $2 AND entry_seq <= $3
  ORDER BY entry_seq;

-- name: InsertArchiveSegment :exec
INSERT INTO agent_session_archive_segments
            (session_id, object_key, min_entry_seq, max_entry_seq, kind)
     VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (session_id, object_key) DO NOTHING;

-- name: PruneTranscriptEntries :exec
DELETE FROM agent_session_transcript_entries
  WHERE session_id = $1 AND entry_seq >= $2 AND entry_seq <= $3;

-- name: RemarkSafetyValveSuperseded :exec
UPDATE agent_session_archive_segments
   SET kind = 'superseded'
 WHERE session_id = $1 AND kind = 'safety_valve' AND max_entry_seq < $2;
