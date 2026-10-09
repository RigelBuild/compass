-- Session blob index queries. Every lookup remains scoped to the owning session
-- and tenant; global object keys are reached only after this row check.

-- name: SessionBlobRowExists :one
SELECT EXISTS (
    SELECT 1
      FROM agent_session_blobs
     WHERE session_id = $1 AND sha256 = $2
);

-- name: InsertSessionBlob :exec
INSERT INTO agent_session_blobs (session_id, sha256, size_bytes)
VALUES ($1, $2, $3)
ON CONFLICT (session_id, sha256) DO NOTHING;

-- name: ResumeSessionBlobs :many
SELECT b.sha256, b.size_bytes
  FROM agent_session_blobs b
  JOIN agent_sessions s ON s.session_id = b.session_id
 WHERE b.session_id = $1
   AND s.agent_account_id = $2
   AND b.sha256 = ANY ($3::TEXT[])
 ORDER BY array_position($3::TEXT[], b.sha256);
