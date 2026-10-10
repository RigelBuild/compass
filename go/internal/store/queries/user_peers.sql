-- name: InsertUserPeer :execrows
INSERT INTO user_peers (user_id, peer_user_id)
VALUES ($1, $2)
ON CONFLICT (user_id, peer_user_id) DO NOTHING;

-- name: DeleteUserPeer :execrows
DELETE FROM user_peers
WHERE user_id = $1 AND peer_user_id = $2;

-- name: ListUserPeerings :many
SELECT COALESCE(outgoing.peer_user_id, incoming.user_id) AS peer_id,
       ah.handle,
       (outgoing.peer_user_id IS NOT NULL)::boolean AS outgoing,
       (incoming.user_id IS NOT NULL)::boolean AS incoming
FROM (SELECT p_out.peer_user_id FROM user_peers p_out WHERE p_out.user_id = $1) outgoing
FULL OUTER JOIN (SELECT p_in.user_id FROM user_peers p_in WHERE p_in.peer_user_id = $1) incoming
  ON incoming.user_id = outgoing.peer_user_id
JOIN accounts a
  ON a.id = COALESCE(outgoing.peer_user_id, incoming.user_id)
JOIN account_handles ah ON ah.account_id = a.id
ORDER BY ah.handle;

-- name: UserPeerPair :one
SELECT EXISTS (SELECT 1 FROM user_peers p_out WHERE p_out.user_id = sqlc.arg(user_id) AND p_out.peer_user_id = sqlc.arg(peer_user_id)) AS outgoing,
       EXISTS (SELECT 1 FROM user_peers p_in WHERE p_in.user_id = sqlc.arg(peer_user_id) AND p_in.peer_user_id = sqlc.arg(user_id)) AS incoming;
-- name: OwnersPeered :one
SELECT EXISTS (
    SELECT 1
    FROM user_peers p_out
    JOIN user_peers p_in
      ON p_in.user_id = p_out.peer_user_id AND p_in.peer_user_id = p_out.user_id
    WHERE p_out.user_id = $1 AND p_out.peer_user_id = $2
);

-- name: OwnersPeeredRowsForShare :many
SELECT user_id, peer_user_id
FROM user_peers
WHERE (user_id = $1 AND peer_user_id = $2)
   OR (user_id = $2 AND peer_user_id = $1)
ORDER BY user_id, peer_user_id
FOR SHARE;
