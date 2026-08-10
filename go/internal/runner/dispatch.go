//go:build unix

// The Runner-side session-command dispatcher: the Runner opens the Sessions bidi
// stream and this loop reads the commands the Server pushes on it, executes each
// against the container lifecycle, and returns the correlated result on the
// request half. Request-id idempotency (OQ6): a command whose request id was
// already handled returns the recorded result rather than re-executing — so a
// relay-Start retried after a timeout creates no duplicate container and no
// spurious ALREADY_RUNNING (go-toolchain-default.md:1388-1389). The Runner is
// authoritative for live session truth (OQ6): it holds the session set and a
// Status command answers from it.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
)

// SessionHost is the container-lifecycle surface the dispatcher drives — the
// subset of the Runner's runtime work a session command touches. The production
// Runner implements it over AgentRuntime + StartAgent; a test drives a fake.
type SessionHost interface {
	// Start brings a session online: resolves the container and starts the agent
	// relay. Returns the live session id. A start for a container already running
	// a session returns errAlreadyRunning. resumeBody is the server-reconstructed
	// session-JSONL body materialized into the container before the agent starts;
	// empty means a fresh (non-resume) start.
	Start(ctx context.Context, req *compassv1.StartAgentSessionRequest, resumeBody string) (sessionID string, err error)
	// Provision creates the isolated per-agent container for a workstream via the
	// AgentRuntime façade, returning its stable container_name. Provision and
	// start are separate: a container can exist idle before a session runs in it.
	Provision(ctx context.Context, req *compassv1.ProvisionAgentWorkspaceRequest) (containerName string, err error)
	// Stop tears a session down. Stopping an unknown/already-stopped session
	// succeeds (idempotent, matching the frozen StopAgentSession semantics).
	Stop(ctx context.Context, sessionID string) error
	// Remove tears down a container and everything bound to it: it retires any
	// live session on the container, tears the container down (stop + remove +
	// deregister), and closes the container's agent socket. An unknown container
	// (never provisioned, or already removed) succeeds as a no-op — the
	// teardown-symmetric counterpart to Provision, idempotent like Stop.
	Remove(ctx context.Context, containerName string) error
	// Reload restarts a session's agent in place, reusing the session id.
	Reload(ctx context.Context, sessionID string) error
	// Status returns the live status of one session, or every live session when
	// id is empty — answered from the Runner's authoritative session set.
	Status(ctx context.Context, sessionID string) ([]*compassv1.AgentSessionStatus, error)
	// RefreshSecrets re-fetches the session's resolved secret set from the
	// Server and materializes it into the container — the SecretsVersion-driven
	// install path (initial materialize and rotation ride the same signal). An
	// unknown session errors; a fetch/materialize failure is returned for the
	// caller to log and recover from on the next signal.
	RefreshSecrets(ctx context.Context, sessionID string) error
	// RefreshConfig re-materializes the current fleet config bundle into every
	// live session's per-container root and Reloads each agent whose config
	// version actually moved — the fleet-wide ConfigVersion-driven update path
	// (contrast RefreshSecrets, which is per-session). Per-session failures are
	// logged and swallowed inside; the returned error is reserved for a
	// fleet-level fault the caller logs and recovers from on the next signal.
	RefreshConfig(ctx context.Context) error
}

// Sentinel errors the host returns, mapped to RunnerErrorCode on the wire.
var (
	errAlreadyRunning = errors.New("session already running on container")
	errSessionUnknown = errors.New("session unknown to runner")
)

// dispatcher runs the Sessions command loop with request-id idempotency.
type dispatcher struct {
	host SessionHost
	log  *slog.Logger

	mu sync.Mutex
	// handled records an in-flight-or-completed entry per request id, so a retry
	// of an id whose execution is still running JOINS that execution rather than
	// starting a second — and a retry of a completed id returns the recorded
	// result. Concurrent per-command dispatch (Approach (a)) opens the
	// check-then-record window to concurrent same-id pushes, so the entry carries
	// a done channel the joiner waits on; this mirrors the Server router's
	// pendingCall (runnerhub/router.go). Single-Runner MVP: the set is small and
	// lives for the stream's life — it is not evicted. Bounded eviction (plus the
	// per-container transition lock this change already lands) is the remaining
	// T9 work (SEA-1328); see docs/designs/platform/compass-runner-concurrent-dispatch/design.md.
	handled map[string]*inflightResult

	// configSignal coalesces ConfigVersion signals into a single pending
	// re-materialize+Reload pass. It is buffered with capacity 1 and written by a
	// non-blocking send (signalConfig): a signal arriving while a pass is already
	// pending is dropped, so N signals collapse to at most one queued pass on top
	// of the one in flight — never N queued Reload fan-outs. The background config
	// worker (runConfigWorker) drains it. The ConfigVersion signal is fleet-wide,
	// so the pass itself (agentHost.RefreshConfig) fans out over every live
	// session; the dispatch receive loop must not block on that slow fan-out, so
	// it only ever signals here.
	configSignal chan struct{}
	// configWorkerDone is closed when the config worker goroutine has exited, so
	// RunSessions can join it on shutdown (no leaked goroutine) and a test can
	// assert a clean exit on ctx cancel.
	configWorkerDone chan struct{}

	// wg tracks the per-command dispatch goroutines RunSessions spawns (Approach
	// (a)), so the deferred shutdown join waits for every in-flight command to
	// unwind on ctx-cancel before returning — the leak-free guarantee.
	wg sync.WaitGroup
	// sendMu serializes the local stream.Send: connect-go's client BidiStream
	// Send is not safe for concurrent callers, and per-command goroutines now
	// call it concurrently. Taken only around the Send, never while holding mu —
	// mirroring the Server router's sendMu (runnerhub/router.go).
	sendMu sync.Mutex
	// send pushes one correlated result down the Sessions request half under
	// sendMu. RunSessions sets it over the live stream; a per-command goroutine
	// calls it rather than touching the stream directly.
	send func(*compassv1internal.SessionsRequest) error
	// provisionSem is a counting semaphore bounding concurrent Provision arms to
	// provisionConcurrency (T-cap, OQ-4=(i)): it restores an intentional throttle
	// on agent-triggered Provisions in place of the accidental concurrency-1 the
	// serial loop provided, WITHOUT queueing any other command (only the Provision
	// arm acquires it, so a Provision backlog never delays a Stop/Status).
	provisionSem chan struct{}
}

// provisionConcurrency caps how many Provision arms run at once (T-cap,
// OQ-4=(i)): the single tunable restoring an intentional throttle on
// agent-triggered Provisions in place of the accidental concurrency-1 the serial
// dispatch loop provided. Sized for the single-Runner dogfood target; see
// docs/designs/platform/compass-runner-concurrent-dispatch/design.md.
const provisionConcurrency = 8

// inflightResult is one request id's dispatch entry: done closes when the
// execution completes and result is set, so a concurrent same-id push waits on
// done and observes the one identical outcome (Approach (b), mirroring the
// Server router's pendingCall in runnerhub/router.go).
type inflightResult struct {
	done   chan struct{}
	result *compassv1internal.SessionsRequest
}

func newDispatcher(host SessionHost, log *slog.Logger) *dispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &dispatcher{
		host:             host,
		log:              log,
		handled:          map[string]*inflightResult{},
		configSignal:     make(chan struct{}, 1),
		configWorkerDone: make(chan struct{}),
		provisionSem:     make(chan struct{}, provisionConcurrency),
	}
}

// signalConfig marks a config re-materialize pass pending without blocking the
// caller: the buffered configSignal collapses repeated signals to one queued
// pass, so the sequential dispatch receive loop never stalls on the fan-out.
func (d *dispatcher) signalConfig() {
	select {
	case d.configSignal <- struct{}{}:
	default:
		// A pass is already pending; this signal coalesces into it.
	}
}

// runConfigWorker drains configSignal and runs one RefreshConfig pass per drained
// signal until ctx is cancelled. Because configSignal is a coalescing buffer of
// one, a burst of signals during an in-flight pass results in exactly one
// follow-up pass, never one per signal. It exits on ctx cancel, closing
// configWorkerDone so the caller can join it leak-free.
func (d *dispatcher) runConfigWorker(ctx context.Context) {
	defer close(d.configWorkerDone)
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.configSignal:
			if err := d.host.RefreshConfig(ctx); err != nil {
				d.log.ErrorContext(ctx, "refreshing config on ConfigVersion signal failed; will retry on next signal",
					slog.Any("error", err))
			}
		}
	}
}

// RunSessions opens the Sessions bidi stream on the link and runs the dispatch
// loop until the stream ends or ctx is cancelled. Each command the Server pushes
// is executed (or deduped) and its result sent back correlated by request id.
//
// The Sessions stream is server-speaks-first: the Runner dials out, but the
// Server pushes commands and only reads results. connect-go does not send the
// request headers — and so does not run the Server's Sessions handler — until
// the client's first Send (CallBidiStream: "request headers are not sent
// automatically ... require an explicit call to Send"). Without an initial Send
// the handler never runs, the command router never attaches, and every
// server-pushed command fails CodeUnavailable until the Runner happens to send.
// So open with one empty bootstrap frame (no request id, no result variant) to
// flush the headers; the Server's router ignores a result frame with no matching
// in-flight request id, so the bootstrap is a harmless no-op there.
func (l *ServerLink) RunSessions(ctx context.Context, host SessionHost, log *slog.Logger) error {
	return runSessions(ctx, l.client.Sessions(ctx), host, log)
}

// sessionStream is the Sessions client bidi stream surface the dispatch loop
// drives: the real *connect.BidiStreamForClient satisfies it directly (no
// adapter). It is the seam RunSessions wraps around the live stream so the loop
// — the concurrent-dispatch logic in runSessions — is exercised over the real
// wire in production AND drivable with a scripted stream in a loop-level unit
// test.
type sessionStream interface {
	Send(result *compassv1internal.SessionsRequest) error
	Receive() (*compassv1internal.SessionsResponse, error)
	CloseResponse() error
}

// runSessions runs the dispatch loop over stream until it ends or ctx is
// cancelled. Each command the Server pushes is executed (or deduped) in its own
// goroutine and its result sent back correlated by request id (Approach (a)):
// this keeps a slow Provision from head-of-line-blocking every other command on
// the stream. The wire ordering across commands is NOT preserved — the Server
// router correlates by request id, so out-of-order completions are legal.
func runSessions(ctx context.Context, stream sessionStream, host SessionHost, log *slog.Logger) error {
	d := newDispatcher(host, log)
	// Derive a cancelable ctx (with cause) so the watcher goroutine below always
	// exits when the loop returns — on ctx cancel, EOF, or error alike — never
	// leaking. The cause distinguishes a clean shutdown from a send-failure
	// unwind (see the Receive-error classification below).
	ctx, cancelCause := context.WithCancelCause(ctx)
	// The config worker runs the coalesced re-materialize+Reload passes off the
	// receive loop. On return, cancel first (nil cause = clean shutdown), then
	// join every in-flight command goroutine, then the config worker — so no
	// spawned goroutine outlives runSessions (leak-free), and cancel precedes the
	// join regardless of return path.
	go d.runConfigWorker(ctx)
	defer func() {
		cancelCause(nil)
		d.wg.Wait()
		<-d.configWorkerDone
	}()
	// send serializes the local Send under sendMu, so concurrent per-command
	// goroutines never race connect's non-concurrent-safe stream Send.
	d.send = func(result *compassv1internal.SessionsRequest) error {
		d.sendMu.Lock()
		defer d.sendMu.Unlock()
		return stream.Send(result)
	}
	if err := stream.Send(&compassv1internal.SessionsRequest{}); err != nil {
		return err
	}
	// Unblock the blocking Receive below on ctx cancel: closing the response
	// side makes an in-flight Receive return, so a cancelled Runner shuts the
	// Sessions loop down promptly instead of parking until the stream drops.
	go func() {
		<-ctx.Done()
		_ = stream.CloseResponse()
	}()
	for {
		cmd, err := stream.Receive()
		if err != nil {
			// Classify the loop's exit. The send-failure cause is checked FIRST
			// and overrides the io.EOF arm: a broken Send commonly surfaces the
			// next Receive as io.EOF (the watcher's CloseResponse on ctx.Done pops
			// the blocked Receive as EOF), so returning nil on io.EOF ahead of the
			// cause check would silently swallow the send failure the serial loop
			// used to return directly. Only a clean context.Canceled cause, or a
			// genuine external EOF with no send-failure cause, returns nil.
			if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
				return cause
			}
			if errors.Is(err, io.EOF) || errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return err
		}
		// ConfigVersion is a signal-only arm that carries no request id and no
		// result and is genuinely non-blocking: its arm only marks a pass pending
		// on the coalescing config worker (signalConfig) and returns, so it runs
		// inline on the receive loop without ever blocking it. SecretsVersion is
		// also signal-only but is NOT cheap — its arm re-fetches (a network
		// FetchSecrets) and re-materializes (a container exec) synchronously, so
		// running it inline would head-of-line-block every other command behind a
		// slow rotation, the exact block this dispatch exists to remove. It goes
		// through the per-command goroutine like a correlated command; handle
		// routes it to execute(ctx, "") and returns nil, so no result frame is
		// sent, and it is joined by the shutdown wg.Wait like every other spawn.
		// The initial before-start materialize is unaffected: it lives in
		// host.Start (FetchSecretsByContainer + Install, strictly before
		// StartAgent), and RefreshSecrets no-ops with errSessionUnknown until the
		// session Start records is live, so a rotation can never precede it.
		if _, ok := cmd.GetCommand().(*compassv1internal.SessionsResponse_ConfigVersion); ok {
			d.execute(ctx, "", cmd)
			continue
		}
		d.wg.Go(func() {
			result := d.handle(ctx, cmd)
			if result == nil {
				return
			}
			if err := d.send(result); err != nil {
				d.log.ErrorContext(ctx, "sending session result failed", slog.Any("error", err))
				cancelCause(fmt.Errorf("session result send failed: %w", err))
			}
		})
	}
}

// handle executes one command (or returns the recorded result for a retried
// request id) and builds the correlated result message.
func (d *dispatcher) handle(ctx context.Context, cmd *compassv1internal.SessionsResponse) *compassv1internal.SessionsRequest {
	// A signal-only command (SecretsVersion, ConfigVersion) carries no request id
	// and has no result variant — it must never enter the request-id dedup map, or
	// the empty-id key would collapse every signal to one and a later rotation or
	// config update would never re-materialize. Execute it directly and return no
	// result frame. SecretsVersion reaches here as its live path (the receive loop
	// spawns it through this goroutine so its network+exec never blocks the loop);
	// ConfigVersion is filtered inline before the spawn, so it reaches this branch
	// only via a direct caller (tests) — the case stays for that and for the
	// empty-id guard above.
	switch cmd.GetCommand().(type) {
	case *compassv1internal.SessionsResponse_SecretsVersion, *compassv1internal.SessionsResponse_ConfigVersion:
		return d.execute(ctx, "", cmd)
	}
	id := cmd.GetRequestId()

	// Idempotent retry under concurrent dispatch (Approach (b)): create an entry
	// the FIRST time an id is seen, and record its result when the execution
	// lands. A concurrent same-id push that finds the entry JOINS it — waiting on
	// done and returning the one recorded result — rather than executing a second
	// time (execute-once). A joiner that lands while the Runner is shutting down
	// returns nil on ctx.Done and sends nothing: the Server's own retry then
	// observes the Runner detach, so the unsent frame is covered server-side, not
	// lost.
	d.mu.Lock()
	if entry, ok := d.handled[id]; ok {
		d.mu.Unlock()
		select {
		case <-entry.done:
			return entry.result
		case <-ctx.Done():
			return nil
		}
	}
	entry := &inflightResult{done: make(chan struct{})}
	d.handled[id] = entry
	d.mu.Unlock()

	entry.result = d.execute(ctx, id, cmd)
	close(entry.done)
	return entry.result
}

// execute runs the command against the host and maps the outcome to a result
// message (a typed response variant, or a RunnerError with the mapped code).
func (d *dispatcher) execute(ctx context.Context, id string, cmd *compassv1internal.SessionsResponse) *compassv1internal.SessionsRequest {
	switch c := cmd.GetCommand().(type) {
	case *compassv1internal.SessionsResponse_Start:
		sessionID, err := d.host.Start(ctx, c.Start, cmd.GetResumeBody().GetSessionBody())
		if err != nil {
			return errorResult(id, err)
		}
		return &compassv1internal.SessionsRequest{
			RequestId: id,
			Result:    &compassv1internal.SessionsRequest_Start{Start: &compassv1.StartAgentSessionResponse{SessionId: sessionID}},
		}
	case *compassv1internal.SessionsResponse_Provision:
		// Bound concurrent Provisions to provisionConcurrency (T-cap): acquire a
		// slot before the heavy podman work and release on the arm's exit. Only
		// this arm touches the semaphore, so a Provision backlog never queues a
		// Stop/Status. ctx.Done releases a caller blocked for a slot on shutdown.
		select {
		case d.provisionSem <- struct{}{}:
		case <-ctx.Done():
			return errorResult(id, ctx.Err())
		}
		defer func() { <-d.provisionSem }()
		containerName, err := d.host.Provision(ctx, c.Provision)
		if err != nil {
			return errorResult(id, err)
		}
		return &compassv1internal.SessionsRequest{
			RequestId: id,
			Result:    &compassv1internal.SessionsRequest_Provision{Provision: &compassv1.ProvisionAgentWorkspaceResponse{ContainerName: containerName}},
		}
	case *compassv1internal.SessionsResponse_Stop:
		if err := d.host.Stop(ctx, c.Stop.GetSessionId()); err != nil {
			return errorResult(id, err)
		}
		return &compassv1internal.SessionsRequest{
			RequestId: id,
			Result:    &compassv1internal.SessionsRequest_Stop{Stop: &compassv1.StopAgentSessionResponse{}},
		}
	case *compassv1internal.SessionsResponse_Remove:
		if err := d.host.Remove(ctx, c.Remove.GetContainerName()); err != nil {
			return errorResult(id, err)
		}
		return &compassv1internal.SessionsRequest{
			RequestId: id,
			Result:    &compassv1internal.SessionsRequest_Remove{Remove: &compassv1.RemoveAgentWorkspaceResponse{}},
		}
	case *compassv1internal.SessionsResponse_Reload:
		sessionID := c.Reload.GetSessionId()
		if err := d.host.Reload(ctx, sessionID); err != nil {
			return errorResult(id, err)
		}
		return &compassv1internal.SessionsRequest{
			RequestId: id,
			Result:    &compassv1internal.SessionsRequest_Reload{Reload: &compassv1.ReloadAgentSessionResponse{SessionId: sessionID}},
		}
	case *compassv1internal.SessionsResponse_Status:
		statuses, err := d.host.Status(ctx, c.Status.GetSessionId())
		if err != nil {
			return errorResult(id, err)
		}
		return &compassv1internal.SessionsRequest{
			RequestId: id,
			Result:    &compassv1internal.SessionsRequest_Status{Status: &compassv1.GetAgentStatusResponse{Statuses: statuses}},
		}
	case *compassv1internal.SessionsResponse_SecretsVersion:
		// Signal-only: re-fetch and re-materialize the session's secret set. A
		// fetch/materialize failure is logged (never a secret value, per the
		// no-log posture) and swallowed — the Runner recovers on the next signal
		// or reconnect, and never crashes the session over a rotation blip
		// (best-effort, mirroring the Server's emit side). No result frame.
		sessionID := c.SecretsVersion.GetSessionId()
		if err := d.host.RefreshSecrets(ctx, sessionID); err != nil {
			d.log.ErrorContext(ctx, "refreshing secrets on SecretsVersion signal failed; will retry on next signal",
				slog.String("session_id", sessionID), slog.Any("error", err))
		}
		return nil
	case *compassv1internal.SessionsResponse_ConfigVersion:
		// Signal-only, fleet-wide: the config bundle changed. Fan-out over every
		// live session (re-materialize + in-place Reload) is slow and would block
		// this sequential receive loop from reading further commands, so the arm
		// only marks a pass pending on the coalescing config worker and returns.
		// Best-effort like SecretsVersion: the worker logs and swallows any
		// failure, recovering on the next signal or reconnect. No result frame
		// (no session id, not request_id-correlated).
		d.log.InfoContext(ctx, "received ConfigVersion signal",
			slog.String("version", c.ConfigVersion.GetVersion()))
		d.signalConfig()
		return nil
	default:
		// An unset/unrecognized command variant — a contract skew. Return an
		// internal error so the Server surfaces it rather than hanging the call.
		return errorResult(id, errors.New("unrecognized session command variant"))
	}
}

// errorResult maps a host error to a RunnerError result with the wire code the
// Server translates to a Connect status.
func errorResult(id string, err error) *compassv1internal.SessionsRequest {
	code := compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_INTERNAL
	switch {
	case errors.Is(err, errAlreadyRunning):
		code = compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_ALREADY_RUNNING
	case errors.Is(err, errSessionUnknown):
		code = compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_NOT_FOUND
	}
	return &compassv1internal.SessionsRequest{
		RequestId: id,
		Result: &compassv1internal.SessionsRequest_Error{Error: &compassv1internal.RunnerError{
			Code:    code,
			Message: err.Error(),
		}},
	}
}
