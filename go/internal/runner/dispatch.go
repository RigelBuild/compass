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

func newDispatcher(host SessionHost, log *slog.Logger) *dispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &dispatcher{host: host, log: log, handled: map[string]*compassv1internal.SessionsRequest{}}
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
	d := newDispatcher(host, log)
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
		// A signal-only command (SecretsVersion) has no result variant on the
		// request half — handle returns nil, and nothing is sent back.
		if result == nil {
			continue
		}
		if err := stream.Send(result); err != nil {
			return err
		}
	}
}

// handle executes one command (or returns the recorded result for a retried
// request id) and builds the correlated result message.
func (d *dispatcher) handle(ctx context.Context, cmd *compassv1internal.SessionsResponse) *compassv1internal.SessionsRequest {
	// A signal-only command (SecretsVersion, ConfigVersion) carries no request id
	// and has no result variant — it must never enter the request-id dedup map, or
	// the empty-id key would collapse every signal to one and a later rotation or
	// config update would never re-materialize. Execute it directly and return no
	// result frame.
	switch cmd.GetCommand().(type) {
	case *compassv1internal.SessionsResponse_SecretsVersion, *compassv1internal.SessionsResponse_ConfigVersion:
		return d.execute(ctx, "", cmd)
	}
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
		sessionID, err := d.host.Start(ctx, c.Start, cmd.GetResumeBody().GetSessionBody())
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
		// Signal-only: the fleet config bundle changed. T3 lands the wire surface
		// and recognizes the signal so it is never a contract-skew error; the
		// coalesced re-fetch → re-materialize → in-place Reload loop is T6
		// (SEA-1629), which replaces this body. Fleet-wide (no session id), not
		// request_id-correlated, no result frame.
		d.log.InfoContext(ctx, "received ConfigVersion signal",
			slog.String("version", c.ConfigVersion.GetVersion()))
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
