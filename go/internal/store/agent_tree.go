package store

import (
	"context"
	"fmt"
)

// agentTreeProjection is the account column list + FROM/JOINs shared by the
// three tree reads. It mirrors the ListAccounts projection (accounts.go:544-546)
// exactly so a tree-read row scans through the same scanAccount helper and reads
// identically to every other account read. The join to agent_accounts is INNER:
// the tree is agents-only, so a user account can never appear in a tree read.
// The trailing WHERE is completed by each caller; $1 is the addressing id.
const agentTreeProjection = `
		SELECT a.id, a.handle, a.display_name,
		       u.role,
		       ag.owner_user_id, ag.home_channel_id, ag.persona, ag.role, ag.parent_agent_id
		FROM accounts a
		LEFT JOIN user_accounts u ON u.account_id = a.id
		JOIN agent_accounts ag ON ag.account_id = a.id`

// queryAgents runs an agent-tree query built from agentTreeProjection and scans
// every row through scanAccount, so all three reads share one row-handling path.
func (s *Store) queryAgents(ctx context.Context, what, sql string, arg string) ([]Account, error) {
	rows, err := s.pool.Query(ctx, sql, arg)
	if err != nil {
		return nil, fmt.Errorf("store: %s: %w", what, err)
	}
	defer rows.Close()

	var accounts []Account
	for rows.Next() {
		acc, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan %s: %w", what, err)
		}
		accounts = append(accounts, acc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate %s: %w", what, err)
	}
	return accounts, nil
}

// AgentNeighborhood returns the agent's local tree neighborhood: its parent, the
// agents sharing that parent (its siblings, which INCLUDE the agent itself), and
// its own direct children. This is the "who is around me" read the roster uses
// to render an agent's immediate context. Sibling-set membership is structural —
// two agents are siblings when their parent_agent_id is equal (roots, whose
// parent is NULL, are co-siblings via IS NOT DISTINCT FROM) — so self-inclusion
// falls out of the agent being its own sibling; the caller need not re-add it.
// Tree edges are agent_accounts.parent_agent_id (nullable; DL-095). Results are
// ordered by account id for a stable read.
//
// This is a RAW tree read: it applies no account-visibility scoping, and the
// NULL-parent sibling rule is deliberately broad — a root seed (parent NULL), an
// unknown id, or a non-agent id all resolve the parent subquery to NULL and so
// match EVERY root agent across ALL owners. Visibility is the caller's job: the
// roster handler clips the result through accountVisibleFromWhere (design record
// T2, go/internal/comms), so a caller on a visibility-sensitive path MUST apply
// that same clip and never surface this set unscoped.
func (s *Store) AgentNeighborhood(ctx context.Context, agentAccountID AccountID) ([]Account, error) {
	sql := agentTreeProjection + `
		WHERE a.id = $1
		   OR a.id = (SELECT parent_agent_id FROM agent_accounts WHERE account_id = $1)
		   OR ag.parent_agent_id IS NOT DISTINCT FROM
		        (SELECT parent_agent_id FROM agent_accounts WHERE account_id = $1)
		   OR ag.parent_agent_id = $1
		ORDER BY a.id`
	return s.queryAgents(ctx, "agent neighborhood", sql, string(agentAccountID))
}

// AgentSubtree returns the agent plus every transitive descendant beneath it in
// the agent tree, walking agent_accounts.parent_agent_id downward with a
// recursive CTE. The seed row is the agent itself, so the agent is always
// included. Results are ordered by account id for a stable read.
//
// The recursive term uses UNION (not UNION ALL) so the walk dedups on account_id
// and therefore TERMINATES even if the data holds a parent-chain cycle — the same
// pre-existing-cycle hazard ReparentAgent defends its upward walk against
// (accounts.go). On an acyclic tree each descendant is reached by exactly one
// path, so UNION yields the identical set. Like the other tree reads this is raw
// and unscoped by owner; the caller applies any visibility clip.
func (s *Store) AgentSubtree(ctx context.Context, agentAccountID AccountID) ([]Account, error) {
	sql := `
		WITH RECURSIVE subtree AS (
			SELECT account_id FROM agent_accounts WHERE account_id = $1
			UNION
			SELECT ag.account_id
			FROM agent_accounts ag
			JOIN subtree ON ag.parent_agent_id = subtree.account_id
		)` + agentTreeProjection + `
		WHERE a.id IN (SELECT account_id FROM subtree)
		ORDER BY a.id`
	return s.queryAgents(ctx, "agent subtree", sql, string(agentAccountID))
}

// AgentsByOwner returns every agent account owned by ownerUserID, regardless of
// its position in the tree — the flat "all my agents" read. Results are ordered
// by account id for a stable read. This read IS owner-scoped by construction
// (owner_user_id = $1); it remains a raw store read that applies no further
// account-visibility clip.
func (s *Store) AgentsByOwner(ctx context.Context, ownerUserID AccountID) ([]Account, error) {
	sql := agentTreeProjection + `
		WHERE ag.owner_user_id = $1
		ORDER BY a.id`
	return s.queryAgents(ctx, "agents by owner", sql, string(ownerUserID))
}
