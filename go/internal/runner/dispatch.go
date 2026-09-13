//go:build unix

// The Runner-side session-command dispatcher: this loop reads the commands the
// Server pushes on the Sessions stream, executes each against the container
// lifecycle, and returns the correlated result. Request-id idempotency (OQ6): a
// handled id returns the recorded result, so a retried relay-Start makes no duplicate.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runner/gateway"
)

// SessionHost is the container-lifecycle surface the dispatcher drives — the
// subset of the Runner's runtime work a session command touches. The production
// Runner implements it over AgentRuntime + StartAgent; a test drives a fake.
type SessionHost interface {
	// Start brings a session online: resolves the container and starts the agent relay,
	// returning the live session id. A start for a container already running a session
	// returns errAlreadyRunning. resumeBody is the reconstructed session-JSONL body
	// materialized before the agent starts; empty means a fresh start.
	Start(ctx context.Context, req *compassv1.StartAgentSessionRequest, resumeBody string) (sessionID string, err error)
	// Provision creates the isolated per-agent container for a workstream via the
	// AgentRuntime façade, returning its stable container_name. Provision and
	// start are separate: a container can exist idle before a session runs in it.
	Provision(ctx context.Context, req *compassv1.ProvisionAgentWorkspaceRequest) (containerName string, err error)
	// Stop tears a session down. Stopping an unknown/already-stopped session
	// succeeds (idempotent, matching the established StopAgentSession semantics).
	Stop(ctx context.Context, sessionID string) error
	// Remove tears down a container and everything bound to it: retires any live session,
	// tears the container down (stop + remove + deregister), and closes its agent socket.
	// An unknown container succeeds as a no-op — the teardown-symmetric counterpart to
	// Provision, idempotent like Stop.
	Remove(ctx context.Context, containerName string) error
	// Reload restarts a session's agent in place, reusing the session id.
	Reload(ctx context.Context, sessionID string) error
	// Status returns the live status of one session, or every live session when
	// id is empty — answered from the Runner's authoritative session set.
	Status(ctx context.Context, sessionID string) ([]*compassv1.AgentSessionStatus, error)
	// RefreshSecrets re-fetches the session's resolved secret set and materializes it into
	// the container — the SecretsVersion-driven install path (initial and rotation share
	// the signal). An unknown session errors; a fetch/materialize failure is returned for
	// the caller to recover from on the next signal.
	RefreshSecrets(ctx context.Context, sessionID string) error
	// RefreshConfig re-materializes the current fleet config bundle into every live
	// session's per-container root and Reloads each agent whose config version moved — the
	// fleet-wide ConfigVersion path. Per-session failures are logged and swallowed inside;
	// the returned error is reserved for a fleet-level fault.
	RefreshConfig(ctx context.Context) error
	// Deliver writes a server-relayed control op to the session's container socket — the
	// receive arm of the Server's send-only DeliverControl dispatch. An unknown session
	// returns errSessionUnknown; success means durably queued, confirmed later by the
	// agent's delivery_ack (not by this return).
	Deliver(ctx context.Context, sessionID string, op *compassv1internal.AgentControl) error
}

// Sentinel errors the host returns, mapped to RunnerErrorCode on the wire. These are
// package-private to runner; the operator-fault sentinel gateway.ErrOperatorConfig is
// exported from package gateway and errorResult maps it to
// RUNNER_ERROR_CODE_FAILED_PRECONDITION.
var (
	errAlreadyRunning = errors.New("session already running on container")
	errSessionUnknown = errors.New("session unknown to runner")
)

// dispatcher runs the Sessions command loop with request-id idempotency.
type dispatcher struct {
	host SessionHost
	log  *slog.Logger

	mu sync.Mutex
	// handled records an in-flight-or-completed entry per request id, so a retry of an
	// in-flight id JOINS its execution (via a done channel) rather than starting a second,
	// and a completed id returns the recorded result. Mirrors the Server router's
	// pendingCall. Single-Runner MVP: small, not evicted (bounded eviction is T9, RIG-1328).
	handled map[string]*inflightResult

	// configSignal coalesces ConfigVersion signals into a single pending
	// re-materialize+Reload pass. Buffered cap 1 with a non-blocking send, so N signals
	// collapse to at most one queued pass on top of the one in flight. The signal is
	// fleet-wide, so the receive loop only signals here and never blocks on the fan-out.
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
	// provisionConcurrency (T-cap, OQ-4=(i)): it restores an intentional throttle without
	// queueing any other command — only the Provision arm acquires it, so a Provision
	// backlog never delays a Stop/Status.
	provisionSem chan struct{}
}

// provisionConcurrency caps how many Provision arms run at once (T-cap, OQ-4=(i)): the
// single tunable restoring an intentional throttle on agent-triggered Provisions in place
// of the accidental concurrency-1 the serial loop provided. Sized for the single-Runner
// dogfood target.
const provisionConcurrency = 8

// inflightResult is one request id's dispatch entry: done closes when the execution
// completes and result is set, so a concurrent same-id push waits on done and observes the
// one identical outcome (mirroring the Server router's pendingCall).
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

// runConfigWorker drains configSignal and runs one RefreshConfig pass per drained signal
// until ctx is cancelled. Because configSignal is a coalescing buffer of one, a burst
// during an in-flight pass yields exactly one follow-up pass. It exits on ctx cancel,
// closing configWorkerDone so the caller can join leak-free.
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

// RunSessions opens the Sessions bidi stream and runs the dispatch loop until the stream
// ends or ctx is cancelled. Each pushed command is executed (or deduped) and its result
// sent back correlated by request id.
//
// The stream is server-speaks-first: connect-go does not send request headers — and so
// does not run the Server's handler — until the client's first Send. So open with one
// empty bootstrap frame to flush the headers; the Server's router ignores a result frame
// with no matching in-flight id, so the bootstrap is a harmless no-op.
func (l *ServerLink) RunSessions(ctx context.Context, host SessionHost, log *slog.Logger) error {
	return runSessions(ctx, l.client.Sessions(ctx), host, log)
}

// sessionStream is the Sessions client bidi stream surface the dispatch loop drives: the
// real *connect.BidiStreamForClient satisfies it directly. It is the seam RunSessions
// wraps around the live stream so the loop runs over the real wire in production and is
// drivable with a scripted stream in a loop-level unit test.
type sessionStream interface {
	Send(result *compassv1internal.SessionsRequest) error
	Receive() (*compassv1internal.SessionsResponse, error)
	CloseResponse() error
}

// runSessions runs the dispatch loop over stream until it ends or ctx is cancelled. Each
// pushed command is executed (or deduped) in its own goroutine and its result sent back
// correlated by request id (Approach (a)), so a slow Provision does not head-of-line-block
// every other command. Wire ordering across commands is NOT preserved.
func runSessions(ctx context.Context, stream sessionStream, host SessionHost, log *slog.Logger) error {
	d := newDispatcher(host, log)
	// Derive a cancelable ctx (with cause) so the watcher goroutine below always exits when
	// the loop returns. The cause distinguishes a clean shutdown from a send-failure unwind.
	ctx, cancelCause := context.WithCancelCause(ctx)
	// The config worker runs the coalesced re-materialize+Reload passes off the receive
	// loop. On return, cancel first (nil cause = clean shutdown), then join every in-flight
	// command goroutine, then the config worker — so no spawned goroutine outlives runSessions.
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
			// Classify the loop's exit. The send-failure cause is checked FIRST and overrides
			// io.EOF: a broken Send commonly surfaces the next Receive as io.EOF (the watcher's
			// CloseResponse pops the blocked Receive), so returning nil on io.EOF first would
			// swallow it. Only a clean context.Canceled, or a genuine external EOF, returns nil.
			if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
				return cause
			}
			if errors.Is(err, io.EOF) || errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return err
		}
		// ConfigVersion is a signal-only arm carrying no id/result and genuinely non-blocking
		// (it only marks a pass pending), so it runs inline. SecretsVersion is also signal-only
		// but NOT cheap — it re-fetches and re-materializes synchronously — so it goes through
		// the per-command goroutine like a correlated command (execute(ctx, ""), no result frame).
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
	// A signal-only command (SecretsVersion, ConfigVersion) carries no request id and no
	// result — it must never enter the dedup map, or the empty-id key would collapse every
	// signal to one. Execute it directly, return no result frame; ConfigVersion is filtered
	// inline before the spawn, so this branch is reached only by a direct caller (tests).
	switch cmd.GetCommand().(type) {
	case *compassv1internal.SessionsResponse_SecretsVersion, *compassv1internal.SessionsResponse_ConfigVersion:
		return d.execute(ctx, "", cmd)
	}
	id := cmd.GetRequestId()

	// Idempotent retry under concurrent dispatch (Approach (b)): create an entry the FIRST
	// time an id is seen, record its result on completion. A concurrent same-id push JOINS
	// the entry rather than executing again. A joiner during shutdown returns nil on ctx.Done;
	// the Server's retry then observes the detach, so the unsent frame is covered server-side.
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
	command := cmd.GetCommand()
	if command == nil {
		// A dispatched command with no variant set — a wire contract skew.
		return d.errorResult(ctx, id, errors.New("sessions stream: command frame missing command variant"))
	}
	switch c := command.(type) {
	case *compassv1internal.SessionsResponse_Start:
		sessionID, err := d.host.Start(ctx, c.Start, cmd.GetResumeBody().GetSessionBody())
		if err != nil {
			return d.errorResult(ctx, id, err)
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
			return d.errorResult(ctx, id, ctx.Err())
		}
		defer func() { <-d.provisionSem }()
		containerName, err := d.host.Provision(ctx, c.Provision)
		if err != nil {
			return d.errorResult(ctx, id, err)
		}
		return &compassv1internal.SessionsRequest{
			RequestId: id,
			Result:    &compassv1internal.SessionsRequest_Provision{Provision: &compassv1.ProvisionAgentWorkspaceResponse{ContainerName: containerName}},
		}
	case *compassv1internal.SessionsResponse_Stop:
		if err := d.host.Stop(ctx, c.Stop.GetSessionId()); err != nil {
			return d.errorResult(ctx, id, err)
		}
		return &compassv1internal.SessionsRequest{
			RequestId: id,
			Result:    &compassv1internal.SessionsRequest_Stop{Stop: &compassv1.StopAgentSessionResponse{}},
		}
	case *compassv1internal.SessionsResponse_Remove:
		if err := d.host.Remove(ctx, c.Remove.GetContainerName()); err != nil {
			return d.errorResult(ctx, id, err)
		}
		return &compassv1internal.SessionsRequest{
			RequestId: id,
			Result:    &compassv1internal.SessionsRequest_Remove{Remove: &compassv1.RemoveAgentWorkspaceResponse{}},
		}
	case *compassv1internal.SessionsResponse_Reload:
		sessionID := c.Reload.GetSessionId()
		if err := d.host.Reload(ctx, sessionID); err != nil {
			return d.errorResult(ctx, id, err)
		}
		return &compassv1internal.SessionsRequest{
			RequestId: id,
			Result:    &compassv1internal.SessionsRequest_Reload{Reload: &compassv1.ReloadAgentSessionResponse{SessionId: sessionID}},
		}
	case *compassv1internal.SessionsResponse_Status:
		statuses, err := d.host.Status(ctx, c.Status.GetSessionId())
		if err != nil {
			return d.errorResult(ctx, id, err)
		}
		return &compassv1internal.SessionsRequest{
			RequestId: id,
			Result:    &compassv1internal.SessionsRequest_Status{Status: &compassv1.GetAgentStatusResponse{Statuses: statuses}},
		}
	case *compassv1internal.SessionsResponse_SecretsVersion:
		// Signal-only: re-fetch and re-materialize the session's secret set. A failure is
		// logged (never a secret value) and swallowed — the Runner recovers on the next
		// signal or reconnect, best-effort like the Server's emit side. No result frame.
		sessionID := c.SecretsVersion.GetSessionId()
		if err := d.host.RefreshSecrets(ctx, sessionID); err != nil {
			d.log.ErrorContext(ctx, "refreshing secrets on SecretsVersion signal failed; will retry on next signal",
				slog.String("session_id", sessionID), slog.Any("error", err))
		}
		return nil
	case *compassv1internal.SessionsResponse_ConfigVersion:
		// Signal-only, fleet-wide: the config bundle changed. The re-materialize + Reload
		// fan-out over every live session is slow, so the arm only marks a pass pending on
		// the coalescing config worker and returns. Best-effort: the worker logs and swallows
		// any failure. No result frame (no session id, not request_id-correlated).
		d.log.InfoContext(ctx, "received ConfigVersion signal",
			slog.String("version", c.ConfigVersion.GetVersion()))
		d.signalConfig()
		return nil
	case *compassv1internal.SessionsResponse_DeliverControl:
		// Send-only: relay a server-pushed control op to the session's container socket. On
		// SUCCESS return nil — NO result frame: success is confirmed later by delivery_ack,
		// not synchronously (a typed success is read as a refusal). A FAILURE returns
		// errorResult, leaving the cursor unadvanced for the D2 reconnect sweep.
		if err := d.host.Deliver(ctx, c.DeliverControl.GetSessionId(), c.DeliverControl.GetOp()); err != nil {
			return d.errorResult(ctx, id, err)
		}
		return nil
	default:
		// An unset/unrecognized command variant — a contract skew. Return an internal error
		// so the Server surfaces it rather than hanging the call.
		return d.errorResult(ctx, id, errors.New("unrecognized session command variant"))
	}
}

// errorResult maps a host error to a RunnerError result with the wire code the Server
// translates to a Connect status, and logs the failure once at a level chosen by class: a
// context cancellation drops to Debug, an INTERNAL fault logs at Error, and a classified
// operator/client fault logs at Warn — so the diagnostic survives locally without over-logging.
func (d *dispatcher) errorResult(ctx context.Context, id string, err error) *compassv1internal.SessionsRequest {
	code := compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_INTERNAL
	switch {
	case errors.Is(err, errAlreadyRunning):
		code = compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_ALREADY_RUNNING
	case errors.Is(err, errSessionUnknown):
		code = compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_NOT_FOUND
	case errors.Is(err, gateway.ErrOperatorConfig):
		code = compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_FAILED_PRECONDITION
	}

	level := slog.LevelError
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		level = slog.LevelDebug
	case code != compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_INTERNAL:
		level = slog.LevelWarn
	}
	d.log.Log(ctx, level, "session command failed",
		slog.String("request_id", id), slog.Any("error", err), slog.String("code", code.String()))

	return &compassv1internal.SessionsRequest{
		RequestId: id,
		Result: &compassv1internal.SessionsRequest_Error{Error: &compassv1internal.RunnerError{
			Code:    code,
			Message: err.Error(),
		}},
	}
}
