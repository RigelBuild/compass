package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
)

// treeAccountText wraps a tree read's non-nullable agent column (owner id,
// persona, role) into the pgtype.Text the shared accountFromRow expects. The
// three tree queries INNER-join agent_accounts, so these columns are always
// present and sqlc types them as plain strings — but accountFromRow keys the
// agent subtype on ownerUserID.Valid, so each must be marked Valid to reconstruct
// the Agent subtype (a tree row is always an agent).
func treeAccountText(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: true}
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
// roster handler clips the result through the account-visibility predicate (design
// record T2, go/internal/comms), so a caller on a visibility-sensitive path MUST
// apply that same clip and never surface this set unscoped.
func (s *Store) AgentNeighborhood(ctx context.Context, agentAccountID AccountID) ([]Account, error) {
	rows, err := s.q.AgentNeighborhood(ctx, string(agentAccountID))
	if err != nil {
		return nil, fmt.Errorf("store: agent neighborhood: %w", err)
	}
	accounts := make([]Account, 0, len(rows))
	for _, row := range rows {
		accounts = append(accounts, accountFromRow(row.ID, row.Handle, row.DisplayName, row.UserRole,
			treeAccountText(row.OwnerUserID), row.HomeChannelID, treeAccountText(row.Persona),
			treeAccountText(row.AgentRole), row.ParentAgentID, row.SystemAccountID))
	}
	return accounts, nil
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
	rows, err := s.q.AgentSubtree(ctx, string(agentAccountID))
	if err != nil {
		return nil, fmt.Errorf("store: agent subtree: %w", err)
	}
	accounts := make([]Account, 0, len(rows))
	for _, row := range rows {
		accounts = append(accounts, accountFromRow(row.ID, row.Handle, row.DisplayName, row.UserRole,
			treeAccountText(row.OwnerUserID), row.HomeChannelID, treeAccountText(row.Persona),
			treeAccountText(row.AgentRole), row.ParentAgentID, row.SystemAccountID))
	}
	return accounts, nil
}

// AgentsByOwner returns every agent account owned by ownerUserID, regardless of
// its position in the tree — the flat "all my agents" read. Results are ordered
// by account id for a stable read. This read IS owner-scoped by construction
// (owner_user_id = $1); it remains a raw store read that applies no further
// account-visibility clip.
func (s *Store) AgentsByOwner(ctx context.Context, ownerUserID AccountID) ([]Account, error) {
	rows, err := s.q.AgentsByOwner(ctx, string(ownerUserID))
	if err != nil {
		return nil, fmt.Errorf("store: agents by owner: %w", err)
	}
	accounts := make([]Account, 0, len(rows))
	for _, row := range rows {
		accounts = append(accounts, accountFromRow(row.ID, row.Handle, row.DisplayName, row.UserRole,
			treeAccountText(row.OwnerUserID), row.HomeChannelID, treeAccountText(row.Persona),
			treeAccountText(row.AgentRole), row.ParentAgentID, row.SystemAccountID))
	}
	return accounts, nil
}
