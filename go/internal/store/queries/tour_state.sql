-- The caller is bound to one tenant by the store's scoped query path.
-- name: GetTourState :one
SELECT outcome, step_id, created_at, updated_at
FROM account_tour_state
WHERE account_id = $1;

-- The unique key makes concurrent first-run attempts a single-winner claim.
-- name: ClaimTourStart :one
INSERT INTO account_tour_state (account_id, outcome, step_id)
VALUES ($1, 'started', NULLIF($2::text, ''))
ON CONFLICT (tenant_id, account_id) DO NOTHING
RETURNING account_id;

-- name: SetTourState :exec
INSERT INTO account_tour_state (account_id, outcome, step_id)
VALUES ($1, $2, NULLIF($3::text, ''))
ON CONFLICT (tenant_id, account_id) DO UPDATE SET
    outcome = EXCLUDED.outcome,
    step_id = EXCLUDED.step_id;
