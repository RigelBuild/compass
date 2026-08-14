//go:build unix

package server

import (
	"context"
	_ "embed"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/comms"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// The root Manager seeded on first launch. A fixed handle so the empty-tree gate
// and CreateAgent's unique-handle constraint together make the seed idempotent;
// role "manager" selects config/prompts/manager/SYSTEM.md as the container's
// block-0 prompt (SEA-1732), which is what makes the seeded agent a real Manager
// rather than a default agent.
const (
	rootSupervisorHandle      = "supervisor"
	rootSupervisorDisplayName = "Supervisor"
	rootSupervisorRole        = "manager"
)

// seedClientRequestID is the fixed idempotency key the seed's SpawnAgent runs
// under. Fixed (not per-call) so a re-enroll that re-fires the seed for an
// already-seeded-and-live supervisor joins the completed spawn or is rejected
// on-live, never provisioning a second container for it.
const seedClientRequestID = "compass-root-supervisor-seed"

// seedTimeout bounds the whole seed (CreateAgent + Provision + Start) so a wedged
// Runner cannot hang the enroll-hook goroutine forever.
const seedTimeout = 2 * time.Minute

// setupThreadClientRequestIDPrefix + "-" + supervisorAccountID is the effective
// idempotency key the Setup post runs under. SUPERVISOR-SCOPED, not a single
// global fixed key (OQ-7): the idempotency index is (author, client_request_id)
// global per author, and the author is @compass forever — so a global key would
// silently suppress the Setup post for a RECREATED root supervisor (operator
// deletes it, the empty-tree gate re-seeds a NEW one), leaving the recreated
// Manager with no first turn. A supervisor-scoped key is immune and costs nothing.
const setupThreadClientRequestIDPrefix = "compass-root-supervisor-setup"

// setupTopicName is the topic the Setup thread is posted into, get-or-created by
// name in the supervisor's home channel.
const setupTopicName = "Setup"

// setupThreadBody is the platform's first-turn Setup message, posted as @compass
// into the root supervisor's home channel to give the Manager its first turn.
// OQ-5: the copy is a placeholder pending Matt's product sign-off at the PR.
//
//go:embed setup_thread.md
var setupThreadBody string

// seedRootSupervisor brings up the root Manager "supervisor" on first launch: on
// an empty agent tree it creates one root agent under the bootstrap admin, then
// provisions and starts it. It is the runner-ready-hook body (wired via
// Hub.SetRunnerReadyHook) because Provision/Start need a Runner whose command
// stream can serve them, which is not up when the server boots — the embedded
// stack starts the Runner only after the server is already serving, and the
// Runner's command stream attaches only after it enrolls. It runs on the hook's
// own goroutine.
//
// It is find-or-create-then-start: on a later Runner reconnect it re-fires and
// re-drives a supervisor whose row exists but was never started (a prior boot
// created the row but its Start failed), because that supervisor has no success
// memo to join, so SpawnAgent runs reject-on-live then Provision/Start for real.
// The create half stays gated on an EMPTY tree: if the operator has built any
// other root, the seed adopts nothing and creates nothing.
//
// Re-drive is bounded by the spawn memo, and does NOT cover a Runner-only
// restart within the memo's success-retention window (spawnMemoTTL). The start
// is SpawnAgent under a fixed client_request_id (seedClientRequestID); once a
// boot's Start succeeds, that success is memoized, so a re-fire inside the window
// joins the completed spawn and returns its cached session id WITHOUT consulting
// the Runner — even if the session actually died with a restarted Runner. Real
// liveness-checked re-drive (consult the Runner's authoritative live set, spawn
// under a fresh key when the cached session is gone) is tracked as a follow-up;
// SEA-1820 covers first-launch seed and the never-started re-drive above.
//
// A failure is logged, not fatal: the server stays up and the next Runner
// reconnect re-fires the seed.
func seedRootSupervisor(ctx context.Context, st *store.Store, svc *service, cm *comms.Comms, adminID, compassID store.AccountID, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, seedTimeout)
	defer cancel()

	// Find-or-create the supervisor. AgentByHandle resolves a prior boot's row;
	// ErrNotFound means it does not exist yet, so fall through to the empty-tree
	// create gate.
	supervisor, err := st.AgentByHandle(ctx, rootSupervisorHandle)
	switch {
	case err == nil:
		// Exists already (prior boot). The find half resolves by a globally
		// unique handle, so assert the found agent is actually THIS admin's root
		// before re-driving it — mirroring the create half's owner+root invariant
		// (createRootSupervisor is empty-tree-gated and admin-scoped). Without
		// this, a non-admin-owned or non-root agent that happened to hold the
		// reserved handle would be auto-provisioned and started. Defensive under
		// the single-admin MVP, but it keeps the create half's "adopts nothing it
		// did not seed" contract honest on the find half too.
		if supervisor.Agent == nil || supervisor.Agent.OwnerUserID != adminID || supervisor.Agent.ParentAgentID != "" {
			log.Error("root-supervisor seed: agent holding the supervisor handle is not the admin's root; skipping seed",
				"agent_account_id", supervisor.ID)
			return
		}
	case errors.Is(err, store.ErrNotFound):
		created, ok, cerr := createRootSupervisor(ctx, st, adminID, log)
		if cerr != nil || !ok {
			return // createRootSupervisor logged the reason (or the tree was non-empty).
		}
		supervisor = created
	default:
		log.Error("root-supervisor seed: looking up supervisor failed; skipping seed", "err", err)
		return
	}

	// Provision + Start under the fixed idempotency key. reject-on-live + the
	// spawn memo make this a no-op for an already-live supervisor, so a re-enroll
	// re-fire never launches a second container.
	if _, err := svc.SpawnAgent(ctx, connect.NewRequest(&compassv1.SpawnAgentRequest{
		AgentAccountId:  string(supervisor.ID),
		ClientRequestId: seedClientRequestID,
	})); err != nil {
		if connect.CodeOf(err) == connect.CodeAlreadyExists {
			// Already live (reject-on-live) — the supervisor is up. Still post the
			// Setup thread: on a re-fire for a supervisor that came up on a prior
			// boot, this arm is the ONLY path that reaches the post, and the
			// supervisor-scoped idempotency key makes a repeat post a no-op.
			postSetupThread(ctx, cm, st, compassID, supervisor, log)
			return
		}
		log.Error("root-supervisor seed: starting supervisor failed; will retry on next enroll",
			"agent_account_id", supervisor.ID, "err", err)
		return
	}

	// SpawnAgent returned without error: either it drove Provision/Start, or it
	// joined this boot's completed seed spawn. It does NOT re-confirm liveness
	// against the Runner on a memo join (see the memo caveat above), so this
	// reports the seed drove to completion, not an independently verified session.
	log.Info("root-supervisor seed: root Manager seed completed", "agent_account_id", supervisor.ID, "handle", rootSupervisorHandle)

	// Give the Manager its first turn: post the Setup thread as @compass into its
	// home channel. A post failure is logged, not fatal (matching the seed's own
	// posture); the next ready-hook re-fire retries it.
	postSetupThread(ctx, cm, st, compassID, supervisor, log)
}

// postSetupThread posts the platform's Setup thread as @compass into the
// supervisor's home channel, giving the seeded root Manager its first turn. It
// first makes @compass an (unsubscribed) member of that channel — PostMessage
// D9-gates the post on membership, so a post before membership collapses to
// CodeNotFound — then posts under a supervisor-scoped idempotency key, so a
// re-fire (or the reject-on-live arm) is deduped to the one Setup message.
//
// Both steps are non-fatal: an error is logged and the seed continues. The next
// ready-hook re-fire retries. compassID is the reserved system sender; it never
// receives (no delivery cursor is seeded), it only authors this post.
func postSetupThread(ctx context.Context, cm *comms.Comms, st *store.Store, compassID store.AccountID, supervisor store.Account, log *slog.Logger) {
	homeChannelID := supervisor.Agent.HomeChannelID
	if err := st.EnsureChannelMember(ctx, homeChannelID, compassID); err != nil {
		log.Error("root-supervisor seed: making @compass a member of the supervisor home channel failed; skipping Setup post",
			"agent_account_id", supervisor.ID, "channel_id", homeChannelID, "err", err)
		return
	}
	if _, err := cm.PostAsAccount(ctx, compassID, &compassv1.PostMessageRequest{
		Container:       &compassv1.PostMessageRequest_ChannelId{ChannelId: string(homeChannelID)},
		Topic:           &compassv1.PostMessageRequest_TopicName{TopicName: setupTopicName},
		Blocks:          []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: setupThreadBody}}},
		ClientRequestId: setupThreadClientRequestIDPrefix + "-" + string(supervisor.ID),
	}); err != nil {
		log.Error("root-supervisor seed: posting the Setup thread failed; will retry on next enroll",
			"agent_account_id", supervisor.ID, "channel_id", homeChannelID, "err", err)
		return
	}
}

// createRootSupervisor creates the root supervisor agent, but only on an EMPTY
// tree (no root under the admin). It returns (agent, true, nil) when it created
// one, and (zero, false, nil) when it created nothing — either the tree already
// held a root (operator-built), or a concurrent seed won the unique-handle race
// (that winner drives the start). The caller starts the supervisor only on a
// true. Any real error is returned with created=false.
func createRootSupervisor(ctx context.Context, st *store.Store, adminID store.AccountID, log *slog.Logger) (store.Account, bool, error) {
	roots, err := st.CountRootAgents(ctx, adminID)
	if err != nil {
		log.Error("root-supervisor seed: counting root agents failed; skipping seed", "err", err)
		return store.Account{}, false, err
	}
	if roots > 0 {
		// The tree is not empty (operator built a root), so seed nothing.
		return store.Account{}, false, nil
	}
	agent, err := st.CreateAgent(ctx, adminID, store.NewAgent{
		Handle:      rootSupervisorHandle,
		DisplayName: rootSupervisorDisplayName,
		Role:        rootSupervisorRole,
		// ParentAgentID empty => root.
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Raced another ready-hook fire to the handle; the winner drives the
			// start. Nothing to create.
			return store.Account{}, false, nil
		}
		log.Error("root-supervisor seed: creating supervisor agent failed", "err", err)
		return store.Account{}, false, err
	}
	return agent, true, nil
}
