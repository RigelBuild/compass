package linearagent

// Responder routing resolution (RIG-2717 T4, design
// docs/designs/server/compass-linear-agent-responder/design.md §Part 2 / §T4).
//
// A Linear delegation or @mention names the Compass app, never a specific
// Manager, so the bridge must resolve which stable Manager runs the session.
// The trusted routing source is Compass's own recorded ownership truth — the
// DL-055 forge_authored_artifacts index — never a header parsed from forge text
// (DL-050 / DL-094): an owner claim parsed from a body is untrusted display
// metadata that must never reach a routing decision.
//
//   - A delegated issue with a recorded ownership row resolves to the stable
//     Manager for that recorded work. The row records the AUTHORING agent, which
//     may be a transient peer/sub-agent (not itself a Manager), so the resolver
//     walks from the recorded agent to its owning Manager and returns that
//     Manager's home channel.
//   - No recorded row (a human-filed issue delegated cold, any coordinate
//     Compass has never authored) — or a bare @mention carrying no issue
//     coordinate — routes to the supervisor / top-level Manager via the
//     dedicated routing channel, where the lane is decided and stamped.

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/RigelBuild/compass/go/internal/store"
)

// OwnershipIndex is the narrow read T4 defines over the DL-055 ownership index
// (store.forge_authored_artifacts). It is backed by *store.Store's landed
// AuthoredArtifactByCoordinate — individual coordinate params, full-artifact
// return carrying the recorded .AgentAccountID — so the concrete store
// satisfies it directly and the resolver never embeds the store's query shape.
//
// The returned AuthoredArtifact's AgentAccountID is the recorded AUTHORING
// agent (may be a peer, not a Manager); store.ErrNotFound means Compass has
// never authored the coordinate. Recorded truth only — never a header parsed
// from forge text (DL-050 / DL-094).
type OwnershipIndex interface {
	AuthoredArtifactByCoordinate(ctx context.Context, provider store.ForgeProvider, host, repo string, kind store.ForgeArtifactKind, number uint64) (store.AuthoredArtifact, error)
}

// ManagerResolver walks a recorded authoring agent (possibly a peer) to its
// owning Manager and returns that Manager's account id and home channel id.
// It is its own narrow seam because no single store method spans the tree walk
// (up parent_agent_id to the nearest tree ancestor — any Manager-class role,
// not a role=="manager" filter, since every tree node is now Manager-class and
// an owner parent must not be skipped) AND the home-channel read; the
// driver backs it with the store's agent-tree + account reads at assembly.
// store.ErrNotFound when the agent (or a walk ancestor) does not resolve.
type ManagerResolver interface {
	OwningManager(ctx context.Context, agent store.AccountID) (managerAccountID store.AccountID, homeChannelID string, err error)
}

// Resolver resolves a delegated Linear session to a stable (Manager, home
// channel). It holds the ownership-index and manager-walk seams plus the
// config-resolved fallback target (the supervisor / top-level Manager account
// and the dedicated routing channel id).
type Resolver struct {
	ownership OwnershipIndex
	managers  ManagerResolver

	// forgeHost is the forge coordinate host recorded for Linear-authored
	// artifacts, config-resolved at construction. It completes the coordinate
	// key the ownership index is queried on (provider is always Linear here).
	forgeHost string

	// supervisorAccountID and routingChannelID are the fallback target: the
	// supervisor / top-level Manager and the dedicated routing channel a
	// no-recorded-row (or coordinate-less) event routes to.
	supervisorAccountID store.AccountID
	routingChannelID    string
}

// NewResolver constructs a Resolver over its two seams and the config-resolved
// fallback target. forgeHost is the coordinate host Linear-authored rows carry;
// supervisorAccountID and routingChannelID are the dedicated routing fallback.
func NewResolver(ownership OwnershipIndex, managers ManagerResolver, forgeHost string, supervisorAccountID store.AccountID, routingChannelID string) *Resolver {
	return &Resolver{
		ownership:           ownership,
		managers:            managers,
		forgeHost:           forgeHost,
		supervisorAccountID: supervisorAccountID,
		routingChannelID:    routingChannelID,
	}
}

// ResolveResponder resolves the stable Manager and home channel that should run
// ev's session (design §Part 2). A recorded ownership row for the delegated
// issue's forge coordinate walks the recorded authoring agent to its owning
// Manager; a missing row (store.ErrNotFound) or an event with no issue
// coordinate falls back to the supervisor + dedicated routing channel.
func (r *Resolver) ResolveResponder(ctx context.Context, ev *SessionEvent) (managerAccountID store.AccountID, homeChannelID string, err error) {
	provider, host, repo, number, ok := r.coordinate(ev)
	if !ok {
		// A bare @mention with no issue coordinate: route to the supervisor.
		return r.supervisorAccountID, r.routingChannelID, nil
	}

	art, err := r.ownership.AuthoredArtifactByCoordinate(ctx, provider, host, repo, store.ForgeArtifactKindIssue, number)
	if errors.Is(err, store.ErrNotFound) {
		// No recorded row: a cold delegation Compass has never authored.
		return r.supervisorAccountID, r.routingChannelID, nil
	}
	if err != nil {
		return "", "", err
	}

	// Recorded row: walk the AUTHORING agent (possibly a peer) to its owning
	// Manager and that Manager's home channel.
	return r.managers.OwningManager(ctx, art.AgentAccountID)
}

// coordinate extracts the delegated issue's forge coordinate from ev. A Linear
// issue identifier is "TEAM-NUMBER" (e.g. "RIG-2717"): the team key is the
// forge repo, the number is the artifact number, the provider is Linear, and
// the host is config-resolved. ok=false when ev carries no parseable issue
// identifier (a bare @mention), which routes to the supervisor fallback.
func (r *Resolver) coordinate(ev *SessionEvent) (provider store.ForgeProvider, host, repo string, number uint64, ok bool) {
	team, num, ok := parseIssueIdentifier(ev.AgentSession.Issue.Identifier)
	if !ok {
		return 0, "", "", 0, false
	}
	return store.ForgeProviderLinear, r.forgeHost, team, num, true
}

// parseIssueIdentifier splits a Linear issue identifier "TEAM-NUMBER" into its
// team key and issue number. It splits on the LAST '-' so a team key may itself
// contain a dash. ok=false for an empty identifier, a missing separator, an
// empty team key, or an unparseable / zero number.
func parseIssueIdentifier(identifier string) (team string, number uint64, ok bool) {
	i := strings.LastIndex(identifier, "-")
	if i <= 0 || i == len(identifier)-1 {
		return "", 0, false
	}
	team = identifier[:i]
	n, err := strconv.ParseUint(identifier[i+1:], 10, 64)
	if err != nil || n == 0 {
		return "", 0, false
	}
	return team, n, true
}
