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
	errRoutingChannel    = errors.New("linear routing: " + linearRoutingChannelName + " channel unresolved")
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
	notify    *forgeNotifyLane
	responder *linearagent.Dispatcher
	webhook   http.Handler
}

// linearRouting backs the resolver's store-side routing seams.
type linearRouting struct {
	st      *store.Store
	adminID store.AccountID
}

// OwningManager is ancestor-or-self: every tree node is Manager-class, so the
// recorded authoring agent is the Manager and its home channel is the target.
func (r *linearRouting) OwningManager(ctx context.Context, agent store.AccountID) (store.AccountID, string, error) {
	acct, err := r.st.GetAccount(ctx, agent)
	if err != nil {
		return "", "", err
	}
	if acct.Agent == nil || acct.Agent.HomeChannelID == "" {
		return "", "", fmt.Errorf("%w: account %q has no agent home channel", store.ErrNotFound, agent)
	}
	return acct.ID, string(acct.Agent.HomeChannelID), nil
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
	channel, err := r.st.ChannelByNameForViewer(ctx, r.adminID, linearRoutingChannelName)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", errRoutingChannel, err)
	}
	return supervisor.ID, string(channel.ID), nil
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
	responder := buildLinearResponder(cfg, st, cm, adminID, bridgeID, tokens)
	// A nil *Dispatcher boxed in the interface is non-nil, so off must stay a nil interface.
	var sessionSink SessionEventSink
	if responder != nil {
		sessionSink = responder
	}
	webhook, err := buildLinearWebhookWiring(ctx, cfg, serverResolver, dataSink, sessionSink, slog.Default())
	if err != nil {
		return linearWiring{}, err
	}
	// A mounted responder emits deep links, so it cannot boot without a public base URL.
	if responder != nil && webhook != nil {
		if err := requirePublicURL(cfg.PublicURL); err != nil {
			return linearWiring{}, err
		}
	}
	return linearWiring{notify: notify, responder: responder, webhook: webhook}, nil
}

// buildLinearResponder assembles the session dispatcher over the shared Linear
// token source; nil when Linear is not configured.
func buildLinearResponder(cfg ServeConfig, st *store.Store, cm *comms.Comms, adminID, bridgeID store.AccountID, tokens *linearagent.TokenSource) *linearagent.Dispatcher {
	if tokens == nil {
		return nil
	}
	routing := &linearRouting{st: st, adminID: adminID}
	resolver := linearagent.NewResolver(st, routing, forge.LinearHost, routing)
	return linearagent.NewDispatcher(linearagent.DispatcherParams{
		Buffer:       linearResponderBuffer,
		Resolve:      resolver.ResolveResponder,
		Poster:       cm,
		Members:      st,
		Topics:       st,
		Associations: st,
		Client:       linearagent.NewClient(tokens, &http.Client{Timeout: linearAPITimeout}, ""),
		DeepLinkFor:  func(channelID string) string { return deepLinkFor(cfg.PublicURL, channelID) },
		Bridge:       bridgeID,
	})
}

// startLinearResponder drains the session queue on the serve group; nil starts nothing.
func startLinearResponder(gctx context.Context, g *errgroup.Group, d *linearagent.Dispatcher) {
	if d == nil {
		return
	}
	g.Go(func() error {
		// Run returns only once gctx ends, so any exit is shutdown, not a serve error.
		_ = d.Run(gctx)
		return nil
	})
}

// linearRoutingBridge is the bridge the seed puts in the routing channel, or empty
// to skip the channel when Linear is not configured and nothing would route there.
func linearRoutingBridge(tokens *linearagent.TokenSource, bridgeID store.AccountID) store.AccountID {
	if tokens == nil {
		return ""
	}
	return bridgeID
}
