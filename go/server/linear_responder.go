//go:build unix

// The Linear Agent Session responder assembly: the dispatcher a verified
// AgentSessionEvent reaches, and the store-backed routing seams its resolver reads.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"

	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/linearagent"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
)

// linearResponderBuffer bounds the session queue; overflow is a 500 that Linear retries.
const linearResponderBuffer = 64

// linearAPITimeout bounds each Linear call so one hung request cannot wedge the
// single-goroutine drain.
const linearAPITimeout = 30 * time.Second

var (
	errRoutingSupervisor = errors.New("linear routing: root supervisor unresolved")
	errRoutingChannel    = errors.New("linear routing: " + store.LinearRoutingChannelName + " channel unresolved")
)

var (
	_ linearagent.OwnershipIndex  = (*store.Store)(nil)
	_ linearagent.Memberships     = (*store.Store)(nil)
	_ linearagent.Topics          = (*store.Store)(nil)
	_ linearagent.Associations    = (*store.Store)(nil)
	_ linearagent.CommsPoster     = (*comms.Comms)(nil)
	_ linearagent.ManagerResolver = (*linearRouting)(nil)
	_ linearagent.FallbackTarget  = (*linearRouting)(nil)
	_ SessionEventSink            = (*linearagent.Dispatcher)(nil)
)

// linearWiring is the Linear half of the doors; each part is nil when off.
type linearWiring struct {
	notify      *forgeNotifyLane
	responder   *linearagent.Dispatcher
	webhook     http.Handler
	sessionLink http.Handler
}

// linearRouting backs the resolver's store-side routing seams.
type linearRouting struct {
	st      *store.Store
	adminID store.AccountID
}

// OwningManager walks from the recorded author up ParentAgentID, self first, to the
// first agent with a placement; a despawned author's work lands on its live ancestor.
func (r *linearRouting) OwningManager(ctx context.Context, agent store.AccountID) (store.AccountID, string, error) {
	// The visited set stops a parent cycle in the data from spinning the walk forever.
	visited := map[store.AccountID]bool{}
	for current := agent; current != "" && !visited[current]; {
		visited[current] = true
		acct, err := r.st.GetAccount(ctx, current)
		if err != nil {
			return "", "", err
		}
		if acct.Agent == nil {
			return "", "", fmt.Errorf("%w: account %q is not an agent", store.ErrNotFound, current)
		}
		switch _, _, err := r.st.PlacementForAgent(ctx, current); {
		case err == nil:
			if acct.Agent.HomeChannelID == "" {
				return "", "", fmt.Errorf("%w: agent %q has no home channel", store.ErrNotFound, current)
			}
			return acct.ID, string(acct.Agent.HomeChannelID), nil
		case !errors.Is(err, store.ErrNotFound):
			return "", "", err
		}
		current = acct.Agent.ParentAgentID
	}
	return "", "", fmt.Errorf("%w: agent %q has no live ancestor", store.ErrNotFound, agent)
}

// RoutingTarget resolves both halves on every call because both are seeded after
// boot; a miss names the half that is missing.
func (r *linearRouting) RoutingTarget(ctx context.Context) (store.AccountID, string, error) {
	supervisor, err := r.st.AgentByHandle(ctx, r.adminID, rootSupervisorHandle)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", errRoutingSupervisor, err)
	}
	if !isAdminRootSupervisor(supervisor, r.adminID) {
		return "", "", fmt.Errorf("%w: %q is not the admin's root agent", errRoutingSupervisor, rootSupervisorHandle)
	}
	channel, err := r.st.LinearRoutingChannel(ctx, r.adminID)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", errRoutingChannel, err)
	}
	return supervisor.ID, string(channel), nil
}

// buildLinearWiring builds the Linear lanes beside the webhook that feeds them:
// the webhook mounts iff its secret is declared, the lanes iff Linear is configured.
func buildLinearWiring(
	ctx context.Context,
	cfg ServeConfig,
	st *store.Store,
	hub *runnerhub.Hub,
	cm *comms.Comms,
	serverResolver secrets.Resolver,
	adminID, bridgeID store.AccountID,
	tokens *linearagent.TokenSource,
) (linearWiring, error) {
	notify := buildLinearNotifyLane(st, hub, tokens, slog.Default())
	var dataSink ForgeEventSink
	if notify != nil {
		dataSink = notify.sink
	}
	routeResolver := buildLinearResolver(st, adminID)
	responder := buildLinearResponder(cfg, st, cm, bridgeID, tokens, "", routeResolver)
	// A nil *Dispatcher boxed in the interface is non-nil, so off must stay a nil interface.
	var sessionSink SessionEventSink
	if responder != nil {
		sessionSink = responder
	}
	webhook, err := buildLinearWebhookWiring(ctx, cfg, serverResolver, dataSink, sessionSink, slog.Default())
	if err != nil {
		return linearWiring{}, err
	}
	var sessionLink http.Handler
	if responder != nil && webhook != nil {
		if err := requirePublicURL(cfg.PublicURL); err != nil {
			return linearWiring{}, err
		}
		sessionLink = newLinearSessionLinkHandler(st, routeResolver.ResolveResponder, cfg.PublicURL, slog.Default())
	}
	return linearWiring{notify: notify, responder: responder, webhook: webhook, sessionLink: sessionLink}, nil
}

// buildLinearResolver shares the click-time routing walk between dispatch and redirects.
func buildLinearResolver(st *store.Store, adminID store.AccountID) *linearagent.Resolver {
	routing := &linearRouting{st: st, adminID: adminID}
	return linearagent.NewResolver(st, routing, forge.LinearHost, routing)
}

// buildLinearResponder assembles the session dispatcher over the shared Linear
// token source; nil when Linear is not configured. An empty graphQLURL is Linear's own.
func buildLinearResponder(cfg ServeConfig, st *store.Store, cm *comms.Comms, bridgeID store.AccountID, tokens *linearagent.TokenSource, graphQLURL string, resolver *linearagent.Resolver) *linearagent.Dispatcher {
	if tokens == nil {
		return nil
	}
	return linearagent.NewDispatcher(linearagent.DispatcherParams{
		Buffer:       linearResponderBuffer,
		Resolve:      resolver.ResolveResponder,
		Poster:       cm,
		Members:      st,
		Topics:       st,
		Associations: st,
		Deliveries:   st,
		Client:       linearagent.NewClient(tokens, &http.Client{Timeout: linearAPITimeout}, graphQLURL),
		SessionLinkFor: func(id string) string {
			return sessionLinkFor(cfg.PublicURL, id)
		},
		Bridge: bridgeID,
	})
}

// startLinearResponder drains the session queue and tails the comms bus for the
// Manager reply that ends a Linear session's "Thinking"; nil starts nothing.
func startLinearResponder(gctx context.Context, g *errgroup.Group, d *linearagent.Dispatcher, commsBus *events.Bus[*compassv1.SubscribeCommsResponse]) {
	if d == nil {
		return
	}
	g.Go(func() error {
		// Run ends only with gctx, so its ctx error is shutdown, not a serve error.
		if err := d.Run(gctx); err != nil && gctx.Err() == nil {
			return err
		}
		return nil
	})
	g.Go(func() error { return d.TailComms(gctx, commsBus) })
}

// linearRoutingBridge is the bridge the seed puts in the routing channel, or empty
// to skip the channel when Linear is not configured and nothing would route there.
func linearRoutingBridge(tokens *linearagent.TokenSource, bridgeID store.AccountID) store.AccountID {
	if tokens == nil {
		return ""
	}
	return bridgeID
}
