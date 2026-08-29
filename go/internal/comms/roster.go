package comms

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// roster computes the caller's view of the vantage agent's roster for one scope,
// joining ALL THREE sources (design.md T2:426-452): the durable tree (store), the
// live presence enum (in-memory, via the presence source — absent → OFFLINE), and
// the durable activity string (agent_activity table — absent → empty). The raw
// tree read is UNSCOPED (agent_tree.go warns each caller must clip), so the
// result is intersected with the CALLER's account-visible set (D9,
// accountVisibleFromWhere via ListAccounts): a non-visible agent never appears,
// even one structurally in the vantage's tree. Tree order (by account id) is
// preserved.
//
// vantage defaults to the caller when the vantage handle is empty — an agent
// caller is session-resolved to itself (actorFromContext), a human/UI caller
// names a vantage explicitly. A non-empty vantageHandle is a `@handle` the
// server resolves via resolveAgentAccount: unknown → NOT_FOUND, and (roster's
// DEFINED error posture, not inherited) a real-but-caller-invisible vantage maps
// to the SAME NOT_FOUND an unknown handle gets — closing the
// NOT_FOUND-vs-empty-success vantage-probe oracle.
func (c *Comms) roster(
	ctx context.Context,
	caller store.AccountID,
	vantageHandle string,
	scope compassv1.RosterScope,
) ([]*compassv1.RosterEntry, error) {
	vantage := caller
	if vantageHandle != "" {
		id, err := c.resolveVisibleAgentHandle(ctx, caller, vantageHandle)
		if err != nil {
			return nil, edgeError(err)
		}
		vantage = id
	}

	tree, err := c.treeForScope(ctx, vantage, scope)
	if err != nil {
		return nil, err
	}

	// D9 clip: keep only agents the CALLER may see. ListAccounts is the store's
	// account-visibility realization (accountVisibleFromWhere); a tree agent not
	// in it is dropped, so an owner-scoped agent never leaks to an unrelated
	// caller.
	visible, err := c.store.ListAccounts(ctx, caller)
	if err != nil {
		return nil, edgeError(err)
	}
	visibleIDs := make(map[store.AccountID]struct{}, len(visible))
	for _, acc := range visible {
		visibleIDs[acc.ID] = struct{}{}
	}

	clipped := make([]store.Account, 0, len(tree))
	ids := make([]store.AccountID, 0, len(tree))
	for _, acc := range tree {
		if _, ok := visibleIDs[acc.ID]; !ok {
			continue
		}
		clipped = append(clipped, acc)
		ids = append(ids, acc.ID)
	}

	// Live presence enum (absent → OFFLINE) + durable activity string (absent →
	// empty), both bulk-read for the clipped set.
	var presence map[store.AccountID]compassv1.AgentPresence
	if c.presence != nil {
		presence = c.presence.PresenceFor(ids)
	}
	activity, err := c.store.ActivityFor(ctx, ids)
	if err != nil {
		return nil, edgeError(err)
	}

	entries := make([]*compassv1.RosterEntry, 0, len(clipped))
	for _, acc := range clipped {
		pres := compassv1.AgentPresence_AGENT_PRESENCE_OFFLINE
		if p, ok := presence[acc.ID]; ok {
			pres = p
		}
		var parentAgentID string
		if acc.Agent != nil {
			parentAgentID = string(acc.Agent.ParentAgentID)
		}
		act := activity[acc.ID] // zero value: empty activity, zero timestamp
		entries = append(entries, &compassv1.RosterEntry{
			AgentAccountId:   string(acc.ID),
			Handle:           acc.Handle,
			DisplayName:      acc.DisplayName,
			ParentAgentId:    parentAgentID,
			Presence:         pres,
			Activity:         act.Activity,
			ActivityAtUnixMs: act.ActivityAtUnixMs,
		})
	}
	return entries, nil
}

// treeForScope selects the tree read for scope, relative to the vantage agent
// (design.md T2:426-452). NEIGHBORHOOD → parent+siblings+children; SUBTREE →
// vantage + descendants; OWNER → every agent owned by the vantage's owner. A
// store error maps through edgeError; an unknown scope is a handler-level guard.
func (c *Comms) treeForScope(
	ctx context.Context,
	vantage store.AccountID,
	scope compassv1.RosterScope,
) ([]store.Account, error) {
	switch scope {
	case compassv1.RosterScope_ROSTER_SCOPE_NEIGHBORHOOD:
		tree, err := c.store.AgentNeighborhood(ctx, vantage)
		if err != nil {
			return nil, edgeError(err)
		}
		return tree, nil
	case compassv1.RosterScope_ROSTER_SCOPE_SUBTREE:
		tree, err := c.store.AgentSubtree(ctx, vantage)
		if err != nil {
			return nil, edgeError(err)
		}
		return tree, nil
	case compassv1.RosterScope_ROSTER_SCOPE_OWNER:
		owner, err := c.ownerOf(ctx, vantage)
		if err != nil {
			return nil, err
		}
		tree, err := c.store.AgentsByOwner(ctx, owner)
		if err != nil {
			return nil, edgeError(err)
		}
		return tree, nil
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("comms: unknown roster scope"))
	}
}

// ownerOf resolves the owning user for the OWNER-scope tree read. When vantage is
// an agent, its owner is agent_accounts.owner_user_id; when vantage is a user
// (a human caller naming itself), the vantage IS the owner, so AgentOwner's
// not-found is the signal to use vantage directly. Any other store error surfaces
// mapped.
func (c *Comms) ownerOf(ctx context.Context, vantage store.AccountID) (store.AccountID, error) {
	owner, err := c.store.AgentOwner(ctx, vantage)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return vantage, nil
		}
		return "", edgeError(err)
	}
	return owner, nil
}

// activityCap bounds a set_status activity string, applied server-side so the
// truncated value is what lands in the durable table AND what publishes
// (design.md T3:483-486). Measured in RUNES so a multibyte string is never cut
// mid-codepoint. 140 per the tool contract.
const activityCap = 140

// RosterAsAccount executes one agent-initiated GetRoster as account — the relay
// arm's entry (RelayCommsCall). The account is the caller AND, when the request
// names no vantage, the session-resolved vantage (an agent asking for "my"
// roster). Mirrors PostAsAccount/ListAsAccount: fail-closed on an empty account,
// then the same roster join a human GetRoster takes, clipped to this account's
// visible set.
func (c *Comms) RosterAsAccount(
	ctx context.Context,
	account store.AccountID,
	req *compassv1.GetRosterRequest,
) (*compassv1.GetRosterResponse, error) {
	if account == "" {
		return nil, errNoActor
	}
	entries, err := c.roster(ctx, account, req.GetVantageHandle(), req.GetScope())
	if err != nil {
		return nil, err
	}
	return &compassv1.GetRosterResponse{Entries: entries}, nil
}

// SetStatusAsAccount write-throughs the durable activity for account: it
// truncates the activity server-side to activityCap runes and upserts it via
// Store.SetActivity, which COMMITS. It returns the TRUNCATED value so the relay
// arm publishes exactly what landed in the table (the ordered write-then-publish
// of design.md T3:473-486: this is the durable write; the best-effort
// PublishActivity is the relay arm's next step). Fail-closed on an empty account,
// mirroring PostAsAccount — never a bootstrap-admin attribution.
func (c *Comms) SetStatusAsAccount(
	ctx context.Context,
	account store.AccountID,
	activity string,
) (string, error) {
	if account == "" {
		return "", errNoActor
	}
	truncated := truncateActivity(activity)
	if err := c.store.SetActivity(ctx, account, truncated, time.Now().UnixMilli()); err != nil {
		return "", edgeError(err)
	}
	return truncated, nil
}

// truncateActivity clips s to activityCap runes, returning s unchanged when it
// already fits. Rune-based so a multibyte codepoint is never split.
func truncateActivity(s string) string {
	r := []rune(s)
	if len(r) <= activityCap {
		return s
	}
	return string(r[:activityCap])
}
