//go:build unix

package server

import (
	"context"
	_ "embed"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// The root supervisor seeded on first launch. A fixed handle makes the seed
// idempotent (empty-tree gate + unique-handle constraint); role "supervisor"
// selects config/prompts/supervisor/SYSTEM.md as block-0 (RIG-1732). The tree root
// is a supervisor (owns intake, incidents, broadcasts), not a leaf manager (RIG-3066).
const (
	rootSupervisorHandle      = "supervisor"
	rootSupervisorDisplayName = "Supervisor"
	rootSupervisorRole        = "supervisor"
)

// errTreeNotEmpty is createRootSupervisor's "operator-built root" outcome: the seed
// adopts nothing, so it must say so when Linear needs the supervisor as its fallback.
var errTreeNotEmpty = errors.New("root-supervisor seed: agent tree already has an operator-built root")

// seedClientRequestID keeps re-enrollment retries on the same supervisor spawn,
// joining its result or rejecting an already-live agent without reprovisioning.
const seedClientRequestID = "compass-root-supervisor-seed"

// seedTimeout bounds setup so a wedged Runner cannot hang enrollment forever.
const seedTimeout = 2 * time.Minute

// Setup idempotency is supervisor-scoped because the index is global per author;
// a recreated supervisor must still receive its first-turn message.
const setupThreadClientRequestIDPrefix = "compass-root-supervisor-setup"

// setupTopicName is the topic in the supervisor's home channel where Setup is posted.
const setupTopicName = "Setup"

// setupThreadBody is the first-turn Setup message posted as @compass to the supervisor.
//
//go:embed setup_thread.md
var setupThreadBody string

// seedDeps is what the root-supervisor seed needs from Serve's wiring.
type seedDeps struct {
	st                                 *store.Store
	svc                                *service
	cm                                 *comms.Comms
	adminID, compassID, linearBridgeID store.AccountID
	log                                *slog.Logger
}

// wireSeed sets hub's runner-ready hook to the seed and returns its cancel.
func wireSeed(ctx context.Context, hub *runnerhub.Hub, d seedDeps) context.CancelFunc {
	hook, cancel := rootSupervisorSeed(ctx, d)
	hub.SetRunnerReadyHook(hook)
	return cancel
}

// rootSupervisorSeed builds the runner-ready hook that runs the seed under its own
// ctx, derived from Serve's. The cancel lets the drain stop the seed when a door
// error, not the parent, ends Serve; Serve also defers it.
func rootSupervisorSeed(ctx context.Context, d seedDeps) (hook func(), cancel context.CancelFunc) {
	seedCtx, cancel := context.WithCancel(ctx)
	return func() {
		seedRootSupervisor(seedCtx, d.st, d.svc, d.cm, d.adminID, d.compassID, d.linearBridgeID, d.log)
	}, cancel
}

// The runner-ready hook seeds a root only on an empty tree after enrollment.
// Incomplete setup retries on reconnect; successful starts stay memoized until expiry.
func seedRootSupervisor(ctx context.Context, st *store.Store, svc *service, cm *comms.Comms, adminID, compassID, linearBridgeID store.AccountID, log *slog.Logger) {
	ctx, span := otel.Tracer(instrumentationScope).Start(ctx, "server.seed.rootSupervisor", trace.WithNewRoot())
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, seedTimeout)
	defer cancel()

	// Find-or-create the supervisor; a miss falls through to the empty-tree gate.
	supervisor, err := st.AgentByHandle(ctx, adminID, rootSupervisorHandle)
	switch {
	case err == nil:
		// The global handle may belong to another account; verify ownership before
		// re-driving this seed.
		if !isAdminRootSupervisor(supervisor, adminID) {
			log.ErrorContext(ctx, "root-supervisor seed: agent holding the supervisor handle is not the admin's root; skipping seed",
				"agent_account_id", supervisor.ID)
			warnNoLinearRoutingTarget(ctx, log, linearBridgeID, "the supervisor handle is not the admin's root agent")
			return
		}
	case errors.Is(err, store.ErrNotFound):
		created, ok, cerr := createRootSupervisor(ctx, st, adminID, log)
		if errors.Is(cerr, errTreeNotEmpty) {
			warnNoLinearRoutingTarget(ctx, log, linearBridgeID, "the agent tree has an operator-built root")
		}
		if cerr != nil || !ok {
			return // createRootSupervisor logged the reason (or the tree was non-empty).
		}
		supervisor = created
	default:
		log.ErrorContext(ctx, "root-supervisor seed: looking up supervisor failed; skipping seed", "err", err)
		return
	}

	// Routing needs only the rows, so establish it before spawning.
	if linearBridgeID != "" {
		if _, err := st.EnsureLinearRoutingChannel(ctx, adminID, supervisor.ID, linearBridgeID); err != nil {
			log.ErrorContext(ctx, "root-supervisor seed: ensuring the linear routing channel failed; will retry on next enroll",
				"agent_account_id", supervisor.ID, "err", err)
		}
	}

	// Provision + Start under the fixed idempotency key. reject-on-live + the
	// spawn memo make this a no-op for an already-live supervisor, so a re-enroll
	// re-fire never launches a second container.
	if _, err := svc.spawnAccount(ctx, supervisor, seedClientRequestID); err != nil {
		if connect.CodeOf(err) == connect.CodeAlreadyExists {
			// Already live: still post Setup, since a prior-boot supervisor needs
			// the thread and its scoped idempotency key makes repeats no-ops.
			postSetupThread(ctx, cm, st, compassID, supervisor, log)
			return
		}
		log.ErrorContext(ctx, "root-supervisor seed: starting supervisor failed; will retry on next enroll",
			"agent_account_id", supervisor.ID, "err", err)
		return
	}

	// A successful spawn may join a memoized result; this does not independently
	// confirm Runner liveness.
	log.InfoContext(ctx, "root-supervisor seed: root supervisor seed completed", "agent_account_id", supervisor.ID, "handle", rootSupervisorHandle)

	// Post the supervisor's first-turn Setup thread. Failure is logged and
	// retried on the next ready-hook run.
	postSetupThread(ctx, cm, st, compassID, supervisor, log)
}

// isAdminRootSupervisor is the ownership assertion on a supervisor-handle lookup:
// handles are global, so only the admin's own root agent is the supervisor.
func isAdminRootSupervisor(acct store.Account, adminID store.AccountID) bool {
	return acct.Agent != nil && acct.Agent.OwnerUserID == adminID && acct.Agent.ParentAgentID == ""
}

// warnNoLinearRoutingTarget flags a seed that returns before the routing channel exists:
// with Linear on, cold delegations and bare @mentions then have no routing target.
func warnNoLinearRoutingTarget(ctx context.Context, log *slog.Logger, linearBridgeID store.AccountID, reason string) {
	if linearBridgeID == "" {
		return
	}
	log.WarnContext(ctx, "root-supervisor seed: cold Linear delegations have no routing target", "reason", reason)
}

// postSetupThread adds @compass to the home channel before posting because
// PostMessage requires membership. Its supervisor-scoped key deduplicates retries.
// Failures are non-fatal and retried on the next ready hook; compassID only
// authors the post and has no delivery cursor.
func postSetupThread(ctx context.Context, cm *comms.Comms, st *store.Store, compassID store.AccountID, supervisor store.Account, log *slog.Logger) {
	homeChannelID := supervisor.Agent.HomeChannelID
	if err := st.EnsureChannelMember(ctx, homeChannelID, compassID); err != nil {
		log.ErrorContext(ctx, "root-supervisor seed: making @compass a member of the supervisor home channel failed; skipping Setup post",
			"agent_account_id", supervisor.ID, "channel_id", homeChannelID, "err", err)
		return
	}
	if _, err := cm.PostAsAccount(ctx, compassID, &compassv1.PostMessageRequest{
		Container:       &compassv1.PostMessageRequest_ChannelId{ChannelId: string(homeChannelID)},
		Topic:           &compassv1.PostMessageRequest_TopicName{TopicName: setupTopicName},
		CreateTopic:     true,
		Blocks:          []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: setupThreadBody}}},
		ClientRequestId: setupThreadClientRequestIDPrefix + "-" + string(supervisor.ID),
	}); err != nil {
		log.ErrorContext(ctx, "root-supervisor seed: posting the Setup thread failed; will retry on next enroll",
			"agent_account_id", supervisor.ID, "channel_id", homeChannelID, "err", err)
		return
	}
}

// createRootSupervisor creates only on an empty tree. Existing roots are
// reported as errTreeNotEmpty; a concurrent winner is a no-op.
func createRootSupervisor(ctx context.Context, st *store.Store, adminID store.AccountID, log *slog.Logger) (store.Account, bool, error) {
	roots, err := st.CountRootAgents(ctx, adminID)
	if err != nil {
		log.ErrorContext(ctx, "root-supervisor seed: counting root agents failed; skipping seed", "err", err)
		return store.Account{}, false, err
	}
	if roots > 0 {
		return store.Account{}, false, errTreeNotEmpty
	}
	agent, err := st.CreateAgent(ctx, adminID, store.NewAgent{
		Handle:      rootSupervisorHandle,
		DisplayName: rootSupervisorDisplayName,
		Role:        rootSupervisorRole,
		// Empty ParentAgentID creates a root.
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Raced another ready-hook fire to the handle; the winner drives the
			// start. Nothing to create.
			return store.Account{}, false, nil
		}
		log.ErrorContext(ctx, "root-supervisor seed: creating supervisor agent failed", "err", err)
		return store.Account{}, false, err
	}
	return agent, true, nil
}
