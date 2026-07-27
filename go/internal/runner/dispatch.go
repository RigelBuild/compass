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
	"io"
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
	// a session returns errAlreadyRunning.
	Start(ctx context.Context, req *compassv1.StartAgentSessionRequest) (sessionID string, err error)
	// Provision creates the isolated per-agent container for a workstream via the
	// AgentRuntime façade, returning its stable container_name. Provision and
	// start are separate: a container can exist idle before a session runs in it.
	Provision(ctx context.Context, req *compassv1.ProvisionAgentWorkspaceRequest) (containerName string, err error)
	// Stop tears a session down. Stopping an unknown/already-stopped session
	// succeeds (idempotent, matching the frozen StopAgentSession semantics).
	Stop(ctx context.Context, sessionID string) error
	// Reload restarts a session's agent in place, reusing the session id.
	Reload(ctx context.Context, sessionID string) error
	// Status returns the live status of one session, or every live session when
	// id is empty — answered from the Runner's authoritative session set.
	Status(ctx context.Context, sessionID string) ([]*compassv1.AgentSessionStatus, error)
}

// Sentinel errors the host returns, mapped to RunnerErrorCode on the wire.
var (
	errAlreadyRunning = errors.New("session already running on container")
	errSessionUnknown = errors.New("session unknown to runner")
)

// dispatcher runs the Sessions command loop with request-id idempotency.
type dispatcher struct {
	host SessionHost

	mu sync.Mutex
	// handled records the result for each request id already processed, so a
	// retry returns the recorded result rather than re-executing. Single-Runner
	// MVP: the set is small and lives for the stream's life — it is not evicted,
	// and single-delivery-per-id (so this dedup is not itself raced by two
	// concurrent pushes of one id) relies on the upstream Server router joining
	// retries on the one Sessions stream. A future high-volume / multi-stream
	// Runner needs bounded eviction + in-flight-sentinel dedup here; deferred to
	// T9 (go-toolchain-default.md:979).
	handled map[string]*compassv1internal.SessionsRequest
}

func newDispatcher(host SessionHost) *dispatcher {
	return &dispatcher{host: host, handled: map[string]*compassv1internal.SessionsRequest{}}
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
func (l *ServerLink) RunSessions(ctx context.Context, host SessionHost) error {
	d := newDispatcher(host)
	// Derive a cancelable ctx so the watcher goroutine below always exits when
	// RunSessions returns — on ctx cancel, EOF, or error alike — never leaking.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := l.client.Sessions(ctx)
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
			if errors.Is(err, io.EOF) || errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return err
		}
		result := d.handle(ctx, cmd)
		if err := stream.Send(result); err != nil {
			return err
		}
	}
}

// handle executes one command (or returns the recorded result for a retried
// request id) and builds the correlated result message.
func (d *dispatcher) handle(ctx context.Context, cmd *compassv1internal.SessionsResponse) *compassv1internal.SessionsRequest {
	id := cmd.GetRequestId()

	// Idempotent retry: a request id already handled returns the recorded
	// result, never a second execution (OQ6).
	d.mu.Lock()
	if prev, ok := d.handled[id]; ok {
		d.mu.Unlock()
		return prev
	}
	d.mu.Unlock()

	result := d.execute(ctx, id, cmd)

	d.mu.Lock()
	d.handled[id] = result
	d.mu.Unlock()
	return result
}

// execute runs the command against the host and maps the outcome to a result
// message (a typed response variant, or a RunnerError with the mapped code).
func (d *dispatcher) execute(ctx context.Context, id string, cmd *compassv1internal.SessionsResponse) *compassv1internal.SessionsRequest {
	switch c := cmd.GetCommand().(type) {
	case *compassv1internal.SessionsResponse_Start:
		sessionID, err := d.host.Start(ctx, c.Start)
		if err != nil {
			return errorResult(id, err)
		}
		return &compassv1internal.SessionsRequest{
			RequestId: id,
			Result:    &compassv1internal.SessionsRequest_Start{Start: &compassv1.StartAgentSessionResponse{SessionId: sessionID}},
		}
	case *compassv1internal.SessionsResponse_Provision:
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
