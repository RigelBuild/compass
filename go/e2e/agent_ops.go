//go:build podman

package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// CreateAgent creates a first-party agent account over CommsService and returns
// its account id. Thin client-RPC primitive the later legs reuse; returns an
// error rather than panicking so the caller (a test) decides fatality. The
// per-call deadline is threaded from ctx.
func (f *Fixture) CreateAgent(ctx context.Context, handle, displayName string) (accountID string, err error) {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Comms().CreateAgent(rctx, connect.NewRequest(&compassv1.CreateAgentRequest{
		Handle:      handle,
		DisplayName: displayName,
	}))
	if err != nil {
		return "", fmt.Errorf("CreateAgent RPC: %w", err)
	}
	return resp.Msg.GetAccount().GetId(), nil
}

// Provision provisions the agent's per-account workspace container over
// CompassService and returns the assigned container name. clientRequestID is the
// idempotency key. Repo carriage was removed (SEA-1527), so no repo/ref fields
// exist to set.
func (f *Fixture) Provision(ctx context.Context, accountID, clientRequestID string) (containerName string, err error) {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Compass().ProvisionAgentWorkspace(rctx, connect.NewRequest(&compassv1.ProvisionAgentWorkspaceRequest{
		AgentAccountId:  accountID,
		ClientRequestId: clientRequestID,
	}))
	if err != nil {
		return "", fmt.Errorf("ProvisionAgentWorkspace RPC: %w", err)
	}
	return resp.Msg.GetContainerName(), nil
}

// StartSession brings the agent in a provisioned container online over
// CompassService and returns the server-side session id.
func (f *Fixture) StartSession(ctx context.Context, containerName string) (sessionID string, err error) {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Compass().StartAgentSession(rctx, connect.NewRequest(&compassv1.StartAgentSessionRequest{
		ContainerName: containerName,
	}))
	if err != nil {
		return "", fmt.Errorf("StartAgentSession RPC: %w", err)
	}
	return resp.Msg.GetSessionId(), nil
}

// Resume brings the agent in a freshly provisioned container online over
// CompassService resuming a persisted logical session: resumeSessionID is the
// session id to resume (the id a prior StartSession returned), reconstructed
// server-side into the new container. The resumed lifetime REUSES that logical
// id as its live id, so the returned session id equals resumeSessionID and the
// durable transcript stays one lineage under that single key. Returns an error
// rather than panicking so the caller decides fatality; the per-call deadline is
// threaded from ctx.
func (f *Fixture) Resume(ctx context.Context, containerName, resumeSessionID string) (sessionID string, err error) {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Compass().StartAgentSession(rctx, connect.NewRequest(&compassv1.StartAgentSessionRequest{
		ContainerName:   containerName,
		ResumeSessionId: resumeSessionID,
	}))
	if err != nil {
		return "", fmt.Errorf("StartAgentSession(resume) RPC: %w", err)
	}
	return resp.Msg.GetSessionId(), nil
}

// OpenSessionTail opens the session frame stream (SubscribeAgentSession) and
// returns it for the caller to consume and Close. It is the OPEN half of the
// settle-wait, split out from the READ half (AwaitTurnSettled) so the tail can
// be opened BEFORE the PostMessage that drives the turn: SubscribeAgentSession
// is live-fan with no replay ring, so a turn that starts AND settles in the
// post→subscribe window would fan its WORKING/READY edges to zero subscribers
// and the wait would hang. Opening here first guarantees the stream is already
// subscribed when the turn fans (mirroring the deliver-side precedent at
// legthreefour_test.go:190-198, "Open one subscription before the post").
//
// It derives NO deadline: the stream's lifetime spans the whole post+settle, so
// the ctx the caller passes governs (the caller Closes the returned stream and
// keeps ctx alive across the post). The RECEIVE loop is bounded separately in
// AwaitTurnSettled via its own derived settleTimeout, mirroring how
// SubscribeComms opens under the caller's ctx and AwaitDelivery bounds the read.
func (f *Fixture) OpenSessionTail(ctx context.Context, sessionID string) (*connect.ServerStreamForClient[compassv1.AgentSessionFrame], error) {
	stream, err := f.Compass().SubscribeAgentSession(ctx, connect.NewRequest(&compassv1.SubscribeAgentSessionRequest{
		SessionId: sessionID,
	}))
	if err != nil {
		return nil, fmt.Errorf("SubscribeAgentSession RPC: %w", err)
	}
	return stream, nil
}

// AwaitTurnSettled reads an ALREADY-OPEN session frame stream (from
// OpenSessionTail) and returns once one turn has settled. It is FULLY
// EVENT-GATED: it reads frames off the stream and returns on the WORKING→READY
// edge — no sleeps, no polling, no retry loops. It SKIPS every frame until the
// first frame reporting AGENT_SESSION_STATE_WORKING, then returns nil on the
// next frame reporting AGENT_SESSION_STATE_READY: WORKING→READY is exactly one
// settled turn. The skip-until-WORKING guard is load-bearing — the agent emits
// STARTING at boot and READY only at agent_end (mapping.ts:115-116), so gating
// on a bare READY without first seeing WORKING would settle on a stray edge
// rather than the driven turn.
//
// The stream was opened under the caller's ctx in OpenSessionTail (whose
// lifetime spans the post), so AwaitTurnSettled cannot re-wrap the stream's own
// ctx to bound the blocking stream.Receive(). Instead it derives its own
// settleTimeout deadline HERE (as the old AwaitSessionSettled did) and bounds
// the receive by pumping stream.Receive() in a goroutine and racing it against
// the derived deadline — the exact shape AwaitDelivery uses to bound a
// caller-ctx stream. So a wedged stream fails visibly here rather than blocking
// to the go-test timeout.
func (f *Fixture) AwaitTurnSettled(ctx context.Context, stream *connect.ServerStreamForClient[compassv1.AgentSessionFrame]) error {
	ctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()

	// Buffered so the pump goroutine never blocks writing its terminal result
	// after this method has already returned on the ctx deadline — it sends once
	// and exits, no leak past the caller's Close.
	out := make(chan error, 1)
	go func() {
		workingSeen := false
		for stream.Receive() {
			st := stream.Msg().GetState()
			switch st {
			case compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING:
				workingSeen = true
			case compassv1.AgentSessionState_AGENT_SESSION_STATE_READY:
				if workingSeen {
					out <- nil
					return
				}
			}
		}
		if err := stream.Err(); err != nil {
			out <- fmt.Errorf("SubscribeAgentSession stream: %w", err)
			return
		}
		out <- fmt.Errorf("frame stream ended before reaching a WORKING→READY settle")
	}()

	select {
	case err := <-out:
		return err
	case <-ctx.Done():
		return fmt.Errorf("awaiting WORKING→READY settle: %w", ctx.Err())
	}
}

// RemoveWorkspace tears down a provisioned agent workspace container over
// CompassService — the teardown counterpart to Provision. clientRequestID is the
// idempotency key (same retry-dedup contract as Provision). Returns an error
// rather than panicking so the caller (a test) decides fatality; a best-effort
// t.Cleanup ignores it. The per-call deadline is threaded from ctx.
func (f *Fixture) RemoveWorkspace(ctx context.Context, containerName, clientRequestID string) error {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	if _, err := f.Compass().RemoveAgentWorkspace(rctx, connect.NewRequest(&compassv1.RemoveAgentWorkspaceRequest{
		ContainerName:   containerName,
		ClientRequestId: clientRequestID,
	})); err != nil {
		return fmt.Errorf("RemoveAgentWorkspace RPC: %w", err)
	}
	return nil
}

// waitRunnerEnrolled blocks until the embedded compass-runner has enrolled with
// the server, or the budget elapses. It is the enrollment counterpart to the
// stack's own waitReady/waitPostgres poll (stack.go): stack.Up returns as soon
// as the runner CHILD is spawned, but the runner enrolls ASYNCHRONOUSLY over the
// TLS door AFTER Up returns, so a leg that Provisions immediately races that
// enrollment and fails `unavailable: no runner enrolled to serve session`. This
// gate closes that race so every Provisioning leg starts against an enrolled
// runner.
//
// The observable enrollment signal available to the cross-process fixture is a
// lightweight enrollment-gated probe. The client GetAgentStatus is served off
// the Server's board projection (server/service.go), NOT a Runner relay, so it
// answers even with no Runner and cannot observe enrollment. StopAgentSession,
// by contrast, relays through the hub's routerFor exactly as Provision does, so
// it returns the CodeUnavailable `no runner enrolled` error until a Runner has
// enrolled — and once one has, a Stop of a synthetic never-started session id is
// an idempotent Runner-side no-op (host.Stop returns success for an unknown
// session; the session-end transcript flush is skipped since the id has no
// entries), so the probe has NO container or session side effect. ONLY that
// specific unavailable-no-runner condition is treated as not-yet-ready; any
// other error is a real failure and is returned immediately. Enrollment is a
// MONOTONIC one-time transition, so this is an event-gated readiness poll on a
// real cross-process signal, not a retry-as-sync: it returns the instant the
// probe stops reporting no-runner. The poll respects ctx cancellation; a budget
// timeout is a legible error.
func (f *Fixture) waitRunnerEnrolled(ctx context.Context) error {
	deadline := f.now().Add(enrollPollBudget)
	ticker := time.NewTicker(enrollPollInterval)
	defer ticker.Stop()
	for {
		if !f.now().Before(deadline) {
			return fmt.Errorf("runner did not enroll within %s", enrollPollBudget)
		}
		if ready, err := f.runnerEnrolledProbe(ctx, deadline); err != nil {
			return err
		} else if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// rootSupervisorHandle is the fixed handle the server seeds the first-launch
// root supervisor under (server/serve_seed.go rootSupervisorHandle). A
// load-bearing cross-package coupling to that unexported const — the e2e package
// cannot import server internals, so it mirrors the literal, exactly as
// classifyEnrollProbe couples to the runnerhub "no runner enrolled" string. If
// the server ever renames the seed handle, waitSeedSettled stops resolving the
// supervisor and its budget-timeout fires legibly rather than silently skipping
// the gate.
const rootSupervisorHandle = "supervisor"

// waitSeedSettled blocks until the first-launch root-supervisor seed has finished
// provisioning its container, or the budget elapses. It is the seed counterpart
// to waitRunnerEnrolled: the seed (server/serve_seed.go) hangs off the Runner
// Sessions-stream attach — the SAME event waitRunnerEnrolled returns on — and
// drives its own Provision+Start of the supervisor on the hook goroutine, so a
// leg that Provisions the instant NewFixture returns races the seed's in-flight
// Provision (two cold rootless-podman bring-ups contending on the engine storage
// lock, overrunning the leg's 30s rpcTimeout under CI load — RIG-2403).
//
// The observable settle signal is the seed's DURABLE PLACEMENT row: the
// ProvisionAgentWorkspace handler writes agent_placements (RecordAgentPlacement,
// server/service.go) immediately after the Runner relay returns the container, so
// a resolvable placement for the supervisor account means the seed's container
// work is DONE. It is read cross-process from the store (the fixture has no seed
// handle) via a short-lived connection. Two states are "still seeding", polled
// through, never a failure until the budget elapses: the supervisor account not
// yet created (AgentByHandle ErrNotFound — the seed's create half has not run)
// and the account present but unplaced (PlacementForAgent ErrNotFound — Provision
// in flight). Any other store error is a real failure surfaced immediately.
//
// It is agent-turn-INDEPENDENT by design: the placement lands at Provision, well
// before the supervisor's first model turn, so the gate does NOT depend on the
// board projection (which trails the agent's first lifecycle frame and filters
// terminal states — a seed that came up model-less would never reach a live board
// entry, hanging a board-based gate). On the WithSite re-attach path the placement
// row PERSISTS from the prior boot, so the gate passes on the first probe and
// correctly does NOT wait on run2's re-fired seed — which is designed to lose its
// re-drive to the fast teardown (serve_seed.go), so waiting on it would hang.
//
// Event-gated bounded poll, mirroring waitRunnerEnrolled: a ticker-driven
// deadline off f.now() with a select on ctx.Done() vs the ticker, returning the
// instant the placement resolves. A budget timeout is a legible fail-closed error
// (rule://no-retries: bounded and loud, never a retry-as-sync).
func (f *Fixture) waitSeedSettled(ctx context.Context, st *store.Store) error {
	deadline := f.now().Add(seedSettlePollBudget)
	ticker := time.NewTicker(seedSettlePollInterval)
	defer ticker.Stop()
	for {
		if !f.now().Before(deadline) {
			return fmt.Errorf("root-supervisor seed did not provision within %s", seedSettlePollBudget)
		}
		if settled, err := f.seedSettledProbe(ctx, st, deadline); err != nil {
			return err
		} else if settled {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// seedSettledProbe runs one seed-settle check against the store: it resolves the
// supervisor account by its fixed handle, then reads its placement. It returns
// settled=true once the placement resolves (the seed's Provision recorded it),
// settled=false for the two not-yet states (supervisor account absent, or present
// but unplaced — both ErrNotFound), and a non-nil error for any other store
// failure, which waitSeedSettled surfaces immediately rather than polling through.
// Split out as its own function so its ErrNotFound-vs-real-error branching is
// unit-testable (classifySeedSettle) without a live stack, mirroring classifyEnrollProbe.
//
// Both store reads share a per-probe deadline (rpcTimeout, clamped to the budget
// remaining), so a wedged read — an exhausted pgxpool, a lock wait, a slow query
// under the CI-load contention this gate targets — fails at the per-probe bound
// as a context.DeadlineExceeded (never store.ErrNotFound, so classifySeedSettle
// surfaces it as a real error) rather than escaping the budget and hanging
// NewFixture to the go-test global timeout. This mirrors runnerEnrolledProbe,
// keeping the whole gate bounded and fail-loud within seedSettlePollBudget.
func (f *Fixture) seedSettledProbe(ctx context.Context, st *store.Store, deadline time.Time) (settled bool, err error) {
	perProbe := rpcTimeout
	if !deadline.IsZero() {
		if remaining := deadline.Sub(f.now()); remaining < perProbe {
			perProbe = remaining
		}
	}
	if perProbe <= 0 {
		perProbe = time.Millisecond
	}
	rctx, cancel := context.WithTimeout(ctx, perProbe)
	defer cancel()
	sup, handleErr := st.AgentByHandle(rctx, rootSupervisorHandle)
	if handleErr != nil {
		return classifySeedSettle(handleErr, nil)
	}
	_, _, placementErr := st.PlacementForAgent(rctx, sup.ID)
	return classifySeedSettle(nil, placementErr)
}

// classifySeedSettle is the pure branching at the heart of the seed-settle probe,
// split out so its not-yet-vs-real-error logic is unit-testable without a live
// store (mirroring classifyEnrollProbe). It takes the two store outcomes in
// order: handleErr from resolving the supervisor by handle, and — only when that
// succeeded — placementErr from reading its placement. A store.ErrNotFound in
// EITHER is a "still seeding" not-yet state (the seed's create half has not run,
// or its Provision is in flight), returning settled=false with no error so the
// caller polls on. Any OTHER error is a real failure, wrapped and surfaced so the
// caller aborts rather than polling a broken store to the budget. Both nil means
// the placement resolved: the seed's Provision is done, settled=true. placementErr
// is consulted only when handleErr is nil (the probe short-circuits otherwise).
func classifySeedSettle(handleErr, placementErr error) (settled bool, cerr error) {
	if handleErr != nil {
		if errors.Is(handleErr, store.ErrNotFound) {
			return false, nil // seed's create half has not run yet
		}
		return false, fmt.Errorf("resolving root supervisor for seed-settle: %w", handleErr)
	}
	if placementErr != nil {
		if errors.Is(placementErr, store.ErrNotFound) {
			return false, nil // supervisor created, Provision still in flight
		}
		return false, fmt.Errorf("resolving root supervisor placement for seed-settle: %w", placementErr)
	}
	return true, nil
}

// waitDeliveryCursorPast blocks until post1 is no longer OWED to the agent on
// its home channel — the moment the agent's delivery cursor has advanced past
// it — or the budget elapses. leg-5's resume is only correct if this ordering
// holds BEFORE the resumed lifetime's server-side start-sweep runs: the cursor
// advances on the agent's delivery_ack (runnerhub deliverAck), NOT on the
// WORKING→READY settle AwaitTurnSettled observes, so a settle can return
// with post1 still owed. The resume start-sweep reads UndeliveredMessages for
// the agent and redelivers anything still owed into the fresh container2; were
// post1 still owed at that point the sweep would redeliver it, consume the
// resumed lifetime's canned turn, and desync the 2-turn script (a
// hang-to-timeout flake OR a mis-attributed green). Gating on the cursor — the
// real cross-process ack signal — instead of trusting the settle-implies-acked
// ordering closes that race.
//
// It is an event-gated bounded poll on UndeliveredMessages, mirroring
// waitRunnerEnrolled: a ticker-driven bound off f.now() with a deadline check
// that returns a legible timeout error, and a select on ctx.Done() vs the
// ticker so it respects cancellation — no sleeps, no retry-as-sync. It returns
// nil the instant post1ID is absent from the home-channel slice of the map
// UndeliveredMessages returns; a store error, ctx cancellation, or a budget
// timeout is a legible error.
func (f *Fixture) waitDeliveryCursorPast(ctx context.Context, st *store.Store, agent store.AccountID, home store.ChannelID, post1ID store.MessageID) error {
	deadline := f.now().Add(cursorPollBudget)
	ticker := time.NewTicker(cursorPollInterval)
	defer ticker.Stop()
	for {
		if !f.now().Before(deadline) {
			return fmt.Errorf("delivery cursor did not advance past message %s within %s", post1ID, cursorPollBudget)
		}
		owed, err := st.UndeliveredMessages(ctx, agent)
		if err != nil {
			return err
		}
		stillOwed := false
		for _, m := range owed[home] {
			if m.ID == post1ID {
				stillOwed = true
				break
			}
		}
		if !stillOwed {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// cursorPollInterval and cursorPollBudget bound waitDeliveryCursorPast: the
// wait between leg-5's pre-teardown settle and the container1 teardown for the
// agent's delivery cursor to advance past post1. The cursor advances on the
// agent's delivery_ack (runnerhub deliverAck), which trails the WORKING→READY
// settle by a bus round-trip, not by an agent turn — so like enrollment this is
// a fast one-time transition and the budget can be far smaller than
// settleTimeout while still failing a genuinely stuck ack legibly rather than
// hanging to the go-test timeout; the interval matches enrollPollInterval's
// magnitude. A deterministic deadline, never a retry loop.
const (
	cursorPollInterval = 100 * time.Millisecond
	cursorPollBudget   = 15 * time.Second
)

// awaitTranscriptPersisted blocks until the session's durable transcript is
// present in the store AND contains wantReply, or the budget elapses. It closes
// the same class of race waitDeliveryCursorPast does: the WORKING→READY settle
// AwaitTurnSettled observes rides the PublishEvents session-state channel, while
// the turn's transcript persists on the INDEPENDENT CommitConversationFrame
// unary (runnerhub/relay_comms.go) — the agent's tee sends its transcript
// frame, the server commits it to agent_session_transcript_entries, and only
// then is SessionTranscript non-empty. That commit trails READY by one runner→
// server round-trip, so a SessionTranscript read fired the instant settle
// returns can legitimately find nothing yet (store: not found) or a transcript
// missing the just-settled reply. A leg that reads immediately is racing the
// commit.
//
// Like waitDeliveryCursorPast this is an event-gated bounded poll, NOT a
// retry-as-sync: it gates on a fast one-time cross-process convergence (the
// commit round-trip), a ticker-driven deadline off f.now() with a select on
// ctx.Done() vs the ticker, returning the instant the transcript holds the
// reply. It returns the persisted entries on success; a store error other than
// ErrNotFound, ctx cancellation, or a budget timeout is a legible error. A bare
// ErrNotFound (or a present-but-incomplete transcript) is the not-yet-committed
// state it waits through, never a failure until the budget elapses.
func (f *Fixture) awaitTranscriptPersisted(ctx context.Context, st *store.Store, sessionID, wantReply string) ([]store.TranscriptEntryRow, error) {
	deadline := f.now().Add(transcriptPollBudget)
	ticker := time.NewTicker(transcriptPollInterval)
	defer ticker.Stop()
	for {
		transcript, err := st.SessionTranscript(ctx, sessionID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if err == nil {
			var joined strings.Builder
			for _, e := range transcript {
				joined.WriteString(e.EntryJSON)
			}
			if strings.Contains(joined.String(), wantReply) {
				return transcript, nil
			}
		}
		if !f.now().Before(deadline) {
			return nil, fmt.Errorf("transcript for session %s did not persist reply %q within %s", sessionID, wantReply, transcriptPollBudget)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// transcriptPollInterval and transcriptPollBudget bound awaitTranscriptPersisted.
// The transcript commit trails the WORKING→READY settle by one runner→server
// CommitConversationFrame round-trip — a fast one-time transition like the
// delivery-cursor ack, so the budget can be far smaller than settleTimeout while
// still failing a genuinely stuck commit legibly rather than hanging to the
// go-test timeout; the interval matches cursorPollInterval's magnitude. A
// deterministic deadline, never a retry loop.
const (
	transcriptPollInterval = 100 * time.Millisecond
	transcriptPollBudget   = 15 * time.Second
)

// enrollProbeSessionID is the synthetic, never-started session id the enrollment
// probe Stops. It is namespaced so it can never collide with a real
// Server-minted session id; a Stop of it is an idempotent Runner-side no-op.
const enrollProbeSessionID = "e2e-enroll-probe-nonexistent-session"

// runnerEnrolledProbe runs one lightweight enrollment-gated probe. It reports
// ready=true once the Runner is enrolled (the Stop relay no longer returns the
// no-runner error), ready=false while enrollment is still pending (the specific
// CodeUnavailable `no runner enrolled` condition), and a non-nil error for any
// other failure — which waitRunnerEnrolled surfaces immediately rather than
// polling through.
func (f *Fixture) runnerEnrolledProbe(ctx context.Context, deadline time.Time) (ready bool, err error) {
	perProbe := rpcTimeout
	if !deadline.IsZero() {
		if remaining := deadline.Sub(f.now()); remaining < perProbe {
			perProbe = remaining
		}
	}
	if perProbe <= 0 {
		perProbe = time.Millisecond
	}
	rctx, cancel := context.WithTimeout(ctx, perProbe)
	defer cancel()
	_, err = f.Compass().StopAgentSession(rctx, connect.NewRequest(&compassv1.StopAgentSessionRequest{
		SessionId: enrollProbeSessionID,
	}))
	ready, retry, cerr := classifyEnrollProbe(err)
	if retry {
		return false, nil
	}
	return ready, cerr
}

// classifyEnrollProbe classifies a StopAgentSession probe result into the
// enrollment-readiness signal, as a pure function so its branches are unit
// testable without a live client. A nil error means the Runner is enrolled. The
// substring "no runner enrolled" is a load-bearing cross-package coupling to the
// production error raised by routerFor at go/internal/runnerhub/hub.go; only that specific
// CodeUnavailable condition is treated as not-yet-ready (retry). A CodeUnavailable
// that does NOT carry that message — a transient transport flap — is intentionally
// surfaced as fatal rather than retried, which is acceptable for a deterministic
// e2e readiness gate. Any other error is a real failure surfaced immediately.
func classifyEnrollProbe(err error) (ready bool, retry bool, cerr error) {
	if err == nil {
		return true, false, nil
	}
	if connect.CodeOf(err) == connect.CodeUnavailable && strings.Contains(err.Error(), "no runner enrolled") {
		return false, true, nil
	}
	return false, false, fmt.Errorf("runner enrollment probe (StopAgentSession): %w", err)
}
