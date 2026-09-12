-- server_key_state queries: the master-key tripwire. Single-row by construction
-- (CHECK (id = 1)); GetServerKeyState reads it, InsertServerKeyState writes it
-- once at first boot. No UPDATE and no DELETE here: rotation is a later record's
-- versioned re-encrypt, and DELETE is revoked on the table so the digest cannot
-- be dropped to defeat the key-swap check.

-- name: GetServerKeyState :one
SELECT id, key_version, key_fingerprint, fingerprint_salt, updated_at
FROM server_key_state WHERE id = 1;

-- name: InsertServerKeyState :exec
INSERT INTO server_key_state (id, key_version, key_fingerprint, fingerprint_salt)
VALUES (1, $1, $2, $3);
