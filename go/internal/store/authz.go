package store

import (
	"context"
	"fmt"
)

// requireChannelMember is the D9 write-authorization primitive: it verifies the
// actor is a member of channelID and returns ErrNotFound if not. This mirrors
// the read paths' membership gate (ListMessages/SearchMessages/AnswerAsk JOIN
// channel_members) so a write authorizes against the same visible set a read
// does — a caller who cannot see a channel cannot mutate it either, and the
// refusal is the not-found/forbidden merge (a non-member cannot tell an
// unauthorized channel apart from a nonexistent one, so a probe enumerates
// nothing).
//
// It runs against either the pool or an open transaction (querier), so a
// mutation can gate inside its own tx before touching state — the D9 discipline
// the frozen record requires on every write RPC ("authorized server-side
// against the authenticated account's visible set", design.md:1101-1102).
func requireChannelMember(ctx context.Context, q querier, actor AccountID, channelID ChannelID) error {
	var member bool
	if err := q.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM channel_members WHERE channel_id = $1 AND account_id = $2)",
		string(channelID), string(actor),
	).Scan(&member); err != nil {
		return fmt.Errorf("store: check channel membership: %w", err)
	}
	if !member {
		// The not-found/forbidden merge: a non-member is told the channel does
		// not exist, never that it exists but is forbidden (errors.go ErrNotFound).
		return fmt.Errorf("%w: channel %q", ErrNotFound, channelID)
	}
	return nil
}

// IsChannelMember reports whether actor is a member of channelID. It is the
// exported form used by the SubscribeComms stream edge to filter each
// fanned-out event by the subscriber's visible set (a non-member never receives
// an event for a channel it cannot see) without turning a non-visible event
// into an error — the D9 discipline extended from the read RPCs to the live
// stream (design.md:446-447: the fan-out is visibility-scoped).
func (s *Store) IsChannelMember(ctx context.Context, actor AccountID, channelID ChannelID) (bool, error) {
	return isChannelMember(ctx, s.pool, actor, channelID)
}

// isChannelMember reports whether actor is a member of channelID (the
// package-internal form IsChannelMember exports and requireChannelMember wraps).
func isChannelMember(ctx context.Context, q querier, actor AccountID, channelID ChannelID) (bool, error) {
	var member bool
	if err := q.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM channel_members WHERE channel_id = $1 AND account_id = $2)",
		string(channelID), string(actor),
	).Scan(&member); err != nil {
		return false, fmt.Errorf("store: check channel membership: %w", err)
	}
	return member, nil
}

// requireGroupCreateAuthz authorizes creating a channel inside groupID. The
// actor is authorized when it owns the group, when it is an agent whose owning
// user owns the group (an agent acts within its owner's space — Matt's ruling),
// or when the group is visible to everyone (VisibilityShared). A group the actor
// neither owns nor may see — and an unknown group — both return ErrNotFound (the
// not-found/forbidden merge), so a non-owner cannot probe which group ids exist.
// This realizes the frozen record's "CreateChannel — caller-authorized against
// the parent group" (design.md:362-367).
func requireGroupCreateAuthz(ctx context.Context, q querier, actor AccountID, groupID ChannelGroupID) error {
	var authorized bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (
		        SELECT 1 FROM channel_groups g
		        WHERE g.id = $1 AND (
		              g.owner_user_id = $2
		           -- Gates on BARE g.visibility = SHARED, not effective
		           -- (MIN-over-ancestry) visibility. Sound only because groups are
		           -- immutable post-create: the sole channel_groups mutation is the
		           -- CreateChannelGroup INSERT (no UpdateChannelGroup / re-parent
		           -- RPC), and CreateChannelGroup enforces child <= parent ceiling,
		           -- so bare-SHARED implies effective-SHARED. If a re-parent or
		           -- visibility-update RPC ever lands, switch this to
		           -- effectiveVisibilityCTE or it becomes a create-leak (a
		           -- bare-SHARED group nested under an OWNER parent would authorize
		           -- creates it should not).
		           OR g.visibility = $3
		           OR g.owner_user_id = (SELECT owner_user_id FROM agent_accounts WHERE account_id = $2)))`,
		string(groupID), string(actor), int32(VisibilityShared),
	).Scan(&authorized); err != nil {
		return fmt.Errorf("store: check group create authz: %w", err)
	}
	if !authorized {
		return fmt.Errorf("%w: group %q", ErrNotFound, groupID)
	}
	return nil
}

// IsAgentWorkspaceVisible reports whether actor may observe the agent's
// workspace: it is a member of the agent's home channel (fork f — workspace
// access is a projection of home-channel membership). Used by the SubscribeComms
// stream edge to filter AgentWorkspaceChanged events, mirroring the
// OpenAgentWorkspace read gate. An unknown agent yields false (not visible).
func (s *Store) IsAgentWorkspaceVisible(ctx context.Context, actor AccountID, agentAccountID AccountID) (bool, error) {
	return isAgentWorkspaceVisible(ctx, s.pool, actor, agentAccountID)
}

// isAgentWorkspaceVisible is the querier-based form IsAgentWorkspaceVisible
// exports and OpenAgentWorkspace wraps, so the workspace open can gate inside
// its own transaction (the same-tx D9 discipline every write RPC upholds).
func isAgentWorkspaceVisible(ctx context.Context, q querier, actor AccountID, agentAccountID AccountID) (bool, error) {
	var visible bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (
		        SELECT 1 FROM agent_accounts ag
		        JOIN channel_members cm ON cm.channel_id = ag.home_channel_id AND cm.account_id = $1
		        WHERE ag.account_id = $2)`,
		string(actor), string(agentAccountID),
	).Scan(&visible); err != nil {
		return false, fmt.Errorf("store: check workspace visibility: %w", err)
	}
	return visible, nil
}
