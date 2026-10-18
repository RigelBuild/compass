//go:build podman

package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect/v2"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// CreateAgent creates a first-party agent account over CommsService and returns
// its account id. Thin client-RPC primitive the later legs reuse; returns an
// error rather than panicking so the caller (a test) decides fatality. The
// per-call deadline is threaded from ctx.
func (f *Fixture) CreateAgent(ctx context.Context, handle, displayName string) (accountID string, err error) {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Comms().CreateAgent(rctx, &compassv1.CreateAgentRequest{
		Handle:      handle,
		DisplayName: displayName,
		Role:        "manager",
	})
	if err != nil {
		return "", fmt.Errorf("CreateAgent RPC: %w", err)
	}
	return resp.GetAccount().GetId(), nil
}

// Provision provisions the agent's per-account workspace container over
// CompassService and returns the assigned container name. accountID is mapped to
// the `owner/agent` handle the request takes. clientRequestID is the idempotency
// key. Repo carriage was removed (RIG-1527), so no repo/ref fields exist to set.
func (f *Fixture) Provision(ctx context.Context, accountID, clientRequestID string) (containerName string, err error) {
	handle, err := f.wireHandle(ctx, accountID)
	if err != nil {
		return "", fmt.Errorf("resolving agent handle: %w", err)
	}
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Compass().ProvisionAgentWorkspace(rctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: handle, ClientRequestId: clientRequestID})
	if err != nil {
		return "", fmt.Errorf("ProvisionAgentWorkspace RPC: %w", err)
	}
	return resp.GetContainerName(), nil
}

// StartSession brings the agent in a provisioned container online over
// CompassService and returns the server-side session id.
func (f *Fixture) StartSession(ctx context.Context, containerName string) (sessionID string, err error) {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Compass().StartAgentSession(rctx, &compassv1.StartAgentSessionRequest{
		ContainerName: containerName,
	})
	if err != nil {
		return "", fmt.Errorf("StartAgentSession RPC: %w", err)
	}
	return resp.GetSessionId(), nil
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
	resp, err := f.Compass().StartAgentSession(rctx, &compassv1.StartAgentSessionRequest{
		ContainerName:   containerName,
		ResumeSessionId: resumeSessionID,
	})
	if err != nil {
		return "", fmt.Errorf("StartAgentSession(resume) RPC: %w", err)
	}
	return resp.GetSessionId(), nil
}

// OpenSessionTail opens the session frame stream (SubscribeAgentSession) and
// returns it for the caller to consume and Close. It is the OPEN half of the
// be opened BEFORE the PostMessage that drives the turn. SubscribeAgentSession
// is live-fan with no replay ring, but it now sends a leading registration-ack
// frame the instant the subscriber is registered server-side, so this OPEN
// returns on REGISTRATION (not on the first relayed frame). That makes
// open-before-post a SERVER-GUARANTEED happens-before: once OpenSessionTail
// returns, the subscription is provably live, so a turn driven afterwards
// cannot fan its edges to zero subscribers (mirroring the deliver-side
// precedent at legthreefour_test.go, "Open one subscription before the post").
//
// It derives NO deadline: the stream's lifetime spans the whole post+settle, so
// the ctx the caller passes governs (the caller Closes the returned stream and
// keeps ctx alive across the post). The RECEIVE loop is bounded separately in
// AwaitTurnSettled via its own derived settleTimeout, mirroring how
// SubscribeComms opens under the caller's ctx and AwaitDelivery bounds the read.
func (f *Fixture) OpenSessionTail(ctx context.Context, sessionID string) (compassv1connect.CompassServiceSubscribeAgentSessionClientStream, error) {
	return f.OpenSessionTailAs(ctx, f.Compass(), sessionID)
}

// OpenSessionTailAs opens the tail as client's account, which must be a member
// of the agent's home channel (another owner's agent is invisible to admin).
func (f *Fixture) OpenSessionTailAs(ctx context.Context, client compassServiceClient, sessionID string) (compassv1connect.CompassServiceSubscribeAgentSessionClientStream, error) {
	stream, err := client.SubscribeAgentSession(ctx, &compassv1.SubscribeAgentSessionRequest{
		SessionId: sessionID,
	})
	if err != nil {
		return compassv1connect.CompassServiceSubscribeAgentSessionClientStream{}, fmt.Errorf("SubscribeAgentSession RPC: %w", err)
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
func (f *Fixture) AwaitTurnSettled(ctx context.Context, stream compassv1connect.CompassServiceSubscribeAgentSessionClientStream) error {
	ctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()

	// Buffered so the pump goroutine never blocks writing its terminal result
	// after this method has already returned on the ctx deadline — it sends once
	// and exits, no leak past the caller's Close.
	out := make(chan error, 1)
	go func() {
		workingSeen := false
		for {
			frame, err := stream.Receive()
			if err != nil {
				if errors.Is(err, io.EOF) {
					out <- errors.New("frame stream ended before reaching a WORKING→READY settle")
				} else {
					out <- fmt.Errorf("SubscribeAgentSession stream: %w", err)
				}
				return
			}
			st := frame.GetState()
			switch st {
			case compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING:
				workingSeen = true
			case compassv1.AgentSessionState_AGENT_SESSION_STATE_READY:
				if workingSeen {
					out <- nil
					return
				}
			case compassv1.AgentSessionState_AGENT_SESSION_STATE_UNSPECIFIED,
				compassv1.AgentSessionState_AGENT_SESSION_STATE_STARTING,
				compassv1.AgentSessionState_AGENT_SESSION_STATE_STOPPED,
				compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED,
				compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED:
			}
		}
	}()

	select {
	case err := <-out:
		return err
	case <-ctx.Done():
		return fmt.Errorf("awaiting WORKING→READY settle: %w", ctx.Err())
	}
}

// AwaitControlDispatchOn reads an ALREADY-OPEN session-tail stream (from
// OpenSessionTail) and blocks until a SessionInjection observation frame whose
// (opKind, messageID) satisfies match fans onto it, returning the matched
// op-kind in its string form (SessionInjectionKind.String(), e.g.
// "SESSION_INJECTION_KIND_STEER"). It is the control-plane observation
// counterpart to AwaitDelivery's deliver side: the split-observation seam lets
// a scenario assert WHICH control op (steer vs deliver) a session dispatched
// for a given message, read off the public SessionInjection frames on
// SubscribeAgentSession.
//
// It is the READ half, split from the OPEN half (OpenSessionTail) so a caller
// opens the tail SYNCHRONOUSLY before the post that drives the dispatch. That
// ordering is a SERVER-GUARANTEED happens-before: OpenSessionTail returns on the
// leading registration-ack (the subscriber is provably live server-side before
// it returns), so a synchronous open-before-post can no longer self-deadlock on
// an idle session — the ack, not the driven injection, unblocks the open. The
// injection cannot be raced away: the subscription registered before the post
// fanned it. (This corrects the old bundled AwaitControlDispatch, which opened
// its own tail from a goroutine concurrent-with-the-post because it wrongly
// assumed the open could only unblock on the injection itself and relied on the
// cross-container relay being slower than the local subscribe — that wall-clock
// assumption WAS the RIG-3044 flake.)
//
// SessionInjection carries no session id, so session scoping is done entirely by
// which session's stream the caller opened. The leading registration-ack (nil
// event) is skipped by the GetSessionInjection()==nil guard below, exactly like
// any non-injection frame.
//
// It is FULLY EVENT-GATED: a goroutine pumps stream.Receive() and the select
// races each frame against ctx — no sleeps, no polling, no retry loops. The
// stream was opened under the caller's ctx (whose lifetime OpenSessionTail does
// not bound), so this derives its own settleTimeout deadline HERE and bounds the
// blocking receive by pumping in a goroutine and racing the derived deadline —
// the exact shape AwaitDelivery/AwaitTurnSettled use — so an injection that
// never arrives fails visibly here rather than blocking to the go-test timeout.
// settleTimeout (not a tighter bound) is used because an injection can trail a
// multi-turn scenario.
//
// match is invoked for EVERY observed injection frame, not only the matching
// one, so the caller's match closure is the retention hook: a caller needing
// window-scoped exclusion (asserting some other op-kind did NOT dispatch for the
// id within the window) accumulates every (opKind, messageID) it is offered into
// its own slice and inspects it after the positive match returns. The window
// closes at that positive match, which is correct for the seam's window-scoped
// exclusion — a later sweep-redelivered deliver (settle.go builds a deliverOp for
// every owed message) is out of window and legitimately unobserved. The method
// keeps no internal retention; the surface stays exactly this one signature.
func (f *Fixture) AwaitControlDispatchOn(ctx context.Context, stream compassv1connect.CompassServiceSubscribeAgentSessionClientStream, match func(opKind, messageID string) bool) (opKind string, err error) {
	ctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()

	type received struct {
		opKind string
		err    error
	}
	// Buffered so the pump goroutine never blocks writing its terminal result
	// after this method has already returned on the ctx deadline — it sends
	// once and exits, no leak past the caller's Close.
	out := make(chan received, 1)
	go func() {
		for {
			frame, err := stream.Receive()
			if err != nil {
				if errors.Is(err, io.EOF) {
					out <- received{err: errors.New("frame stream ended before a matching SessionInjection arrived")}
				} else {
					out <- received{err: fmt.Errorf("SubscribeAgentSession stream: %w", err)}
				}
				return
			}
			inj := frame.GetEvent().GetSessionInjection()
			if inj == nil {
				continue
			}
			kind := inj.GetOpKind().String()
			mid := inj.GetMessageId()
			if match(kind, mid) {
				out <- received{opKind: kind}
				return
			}
		}
	}()
	select {
	case r := <-out:
		return r.opKind, r.err
	case <-ctx.Done():
		return "", fmt.Errorf("awaiting matching SessionInjection: %w", ctx.Err())
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
	if _, err := f.Compass().RemoveAgentWorkspace(rctx, &compassv1.RemoveAgentWorkspaceRequest{
		ContainerName:   containerName,
		ClientRequestId: clientRequestID,
	}); err != nil {
		return fmt.Errorf("RemoveAgentWorkspace RPC: %w", err)
	}
	return nil
}

// waitRunnerEnrolled waits for the Sessions stream to attach after Enroll.
// A Stop on a synthetic unknown session is side-effect-free; it returns
// Unavailable until a runner is ready. Only that state is polled, within ctx.
func (f *Fixture) waitRunnerEnrolled(ctx context.Context) error {
	deadline := f.now().Add(enrollPollBudget)
	ticker := time.NewTicker(enrollPollInterval)
	defer ticker.Stop()
	for {
		if !f.now().Before(deadline) {
			return fmt.Errorf("runner Sessions stream did not attach within %s", enrollPollBudget)
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

// rootSupervisorHandle mirrors server/serve_seed.go's unexported seed handle; a
// rename there makes waitSeedSettled time out legibly rather than skip the gate.
const rootSupervisorHandle = "supervisor"

// bootstrapAdminHandle mirrors the server's default handle so owner-qualified
// agent lookups resolve the admin's namespace.
const bootstrapAdminHandle = "admin"

// adminAgentByHandle resolves an agent created through the fixture's admin
// client; AgentByHandle requires the resolved owner account id.
func adminAgentByHandle(ctx context.Context, st *store.Store, handle string) (store.Account, error) {
	admin, err := st.UserByHandle(ctx, bootstrapAdminHandle)
	if err != nil {
		return store.Account{}, err
	}
	return st.AgentByHandle(ctx, admin.ID, handle)
}

// waitSeedSettled waits for the supervisor's durable placement before leg
// Provision calls contend for rootless-podman. Missing-account or placement
// states are pending; other errors fail. Existing placement permits WithSite
// reattachment without waiting on run2's seed.
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

// seedSettledProbe reads the supervisor account and placement once. Missing
// account or placement means seeding is in progress; other store errors fail.
// Both reads share a deadline clamped to the remaining budget, so a wedged
// query cannot outlive the bounded wait.
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
	// The supervisor is a root agent owned by the bootstrap admin, so resolve the
	// admin's account id first and look the supervisor up in that owner's agent
	// namespace (AgentByHandle is owner-qualified after RIG-2751). A not-yet-created
	// admin or supervisor is the same "still seeding" ErrNotFound the caller polls.
	admin, adminErr := st.UserByHandle(rctx, bootstrapAdminHandle)
	if adminErr != nil {
		return classifySeedSettle(adminErr, nil)
	}
	sup, handleErr := st.AgentByHandle(rctx, admin.ID, rootSupervisorHandle)
	if handleErr != nil {
		return classifySeedSettle(handleErr, nil)
	}
	_, _, placementErr := st.PlacementForAgent(rctx, sup.ID)
	return classifySeedSettle(nil, placementErr)
}

// classifySeedSettle treats ErrNotFound from either lookup as still seeding.
// It fails on other lookup errors and succeeds only when both lookups succeed.
// placementErr is consulted only when handleErr is nil.
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

// waitDeliveryCursorPast waits until post1 is no longer owed before resume.
// READY does not imply delivery_ack, and the resume start-sweep would redeliver
// an owed post and consume the next canned turn. This bounded poll gates on the
// actual cross-process acknowledgement rather than session settle.
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

// cursorPollInterval and cursorPollBudget bound the ack wait: delivery_ack
// trails READY by one bus round-trip, not an agent turn. The finite deadline is
// well below settleTimeout so a stuck ack fails before the suite timeout.
const (
	cursorPollInterval = 100 * time.Millisecond
	cursorPollBudget   = 15 * time.Second
)

// awaitTranscriptPersisted waits for the durable transcript commit that trails
// READY on a separate RPC path. It succeeds only when the transcript contains
// wantReply; ErrNotFound or incomplete content is pending, while other errors,
// cancellation, and budget expiry fail.
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

// transcriptPollInterval and transcriptPollBudget bound the commit wait, which
// trails READY by one RPC round-trip. Keep the finite budget below settleTimeout
// so a stuck commit fails before the suite timeout.
const (
	transcriptPollInterval = 100 * time.Millisecond
	transcriptPollBudget   = 15 * time.Second
)

// enrollProbeSessionID is the synthetic, never-started session id the enrollment
// probe Stops. It is namespaced so it can never collide with a real
// Server-minted session id; a Stop of it is an idempotent Runner-side no-op.
const enrollProbeSessionID = "e2e-enroll-probe-nonexistent-session"

// runnerEnrolledProbe treats only the runner-not-ready CodeUnavailable response
// as pending. Other errors fail immediately; the synthetic session id cannot
// collide with a server-minted id, and Stop is a no-op when enrolled.
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
	_, err = f.Compass().StopAgentSession(rctx, &compassv1.StopAgentSessionRequest{
		SessionId: enrollProbeSessionID,
	})
	ready, retry, cerr := classifyEnrollProbe(err)
	if retry {
		return false, nil
	}
	return ready, cerr
}

// classifyEnrollProbe retries only a CodeUnavailable carrying a runnerhub not-ready
// message; any other error, a transport flap included, fails the gate at once.
func classifyEnrollProbe(err error) (ready bool, retry bool, cerr error) {
	if err == nil {
		return true, false, nil
	}
	// A runner that enrolled but has not attached its sessions stream yet (or is
	// reattaching after a restart) is still not ready.
	if connect.CodeOf(err) == connect.CodeUnavailable &&
		(strings.Contains(err.Error(), runnerhub.ErrMsgNoRunnerEnrolled) || strings.Contains(err.Error(), runnerhub.ErrMsgNoSessionsStream)) {
		return false, true, nil
	}
	return false, false, fmt.Errorf("runner enrollment probe (StopAgentSession): %w", err)
}
