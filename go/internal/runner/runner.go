//go:build unix

// Package runner is the Runner-side of the Server<->Runner seam (design:
// architecture-lineage): the second binary. It dials OUT to the Server over gRPC with
// its per-Runner token, enrolls, opens the Sessions bidi stream to receive
// session commands, and relays agent events up the PublishEvents client-stream.
// It wraps the already-built internal/runtime container layer (AgentRuntime /
// PodmanCLI) — it does not reimplement container hosting.
//
// Because the Runner dials out, the Server has no inbound route to it: every RPC
// is Runner-initiated. Enroll is the handshake; Sessions is Runner-opened with
// the Server pushing commands on the response half; PublishEvents is a
// Runner->Server client-stream. This is the dial-out model the established proto
// shape realizes.
package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// dialConfigurationError marks local Dial setup failures that no server retry can fix.
type dialConfigurationError struct{ err error }

func (e *dialConfigurationError) Error() string { return "runner dial configuration: " + e.err.Error() }
func (e *dialConfigurationError) Unwrap() error { return e.err }

// RunnerConfig is everything the Runner needs to attach to a Server and host
// agents. ServerAddr is the Server's base URL; Token supplies its bearer token.
type RunnerConfig struct {
	// RunnerID is this Runner's stable identity, cross-checked against the token
	// subject at enrollment. Empty is accepted for Server-assigned identities.
	RunnerID string
	// ServerAddr is the Server's base URL, e.g. https://server.example:443.
	ServerAddr string
	// Token supplies a bearer token on every RPC.
	Token TokenSource
	// Engine is the container runtime seam the Runner hosts agents on.
	Engine runtime.WorkloadRuntime
	// RuntimeDir is the Runner-owned base directory under which per-container
	// agent sockets live (RuntimeDir/containers/<container>/agent.sock, OQ-5).
	// Owner-only; the socket is a local hop that never touches the network.
	RuntimeDir string
	// AgentModel is the model selector handed to every agent this Runner
	// starts (the agent's COMPASS_MODEL). Empty leaves each agent on its own
	// default rather than exporting a blank value it would have to ignore.
	AgentModel string
	// AgentBatching is the inbound batching setting passed to each agent; empty
	// leaves batching off.
	AgentBatching string
	// HTTPClient dials the Server. Nil uses a default HTTP/2 client; tests inject
	// one wired to an httptest server.
	HTTPClient connect.HTTPClient
}

// ServerLink is a live connection to the Server: the RunnerService client plus
// the enrollment outcome. Dial establishes it (constructs the client and
// enrolls); the caller then opens Sessions and PublishEvents on it.
type ServerLink struct {
	client     compassv1internalconnect.RunnerServiceClient
	runnerID   string
	reattached bool
	// beforeWait, set only by tests, runs inside each stream's shared wait just
	// before Process.Wait, so a test can hold the reaper there while Stop runs.
	beforeWait func()
	// drainGrace, set only by tests, overrides the post-exit drain join bound.
	drainGrace time.Duration
}

// Reattached reports whether enrollment re-attached an already-registered Runner
// (OQ6 duplicate enrollment) rather than registering fresh.
func (l *ServerLink) Reattached() bool { return l.reattached }

// RunnerID reports the identity assigned by the Server during enrollment.
func (l *ServerLink) RunnerID() string { return l.runnerID }

// bearerToken asks its source for a fresh token on every outbound RPC.
type bearerToken struct {
	source TokenSource
}

func (b *bearerToken) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		token, err := b.source.Token()
		if err != nil {
			return nil, err
		}
		req.Header().Set("Authorization", "Bearer "+token)
		return next(ctx, req)
	}
}

func (b *bearerToken) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Del("Authorization")
		if token, err := b.source.Token(); err == nil {
			conn.RequestHeader().Set("Authorization", "Bearer "+token)
		}
		return conn
	}
}

func (b *bearerToken) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// Dial constructs the RunnerService client for serverAddr, authenticated with
// token, and enrolls the Runner. It returns a live ServerLink or the enrollment
// error (an Unauthenticated here means a bad/expired/wrong-kind token — the
// Server rejected the credential at the door). httpClient may be nil for the
// default.
func Dial(ctx context.Context, cfg RunnerConfig) (*ServerLink, error) {
	if cfg.Token == nil {
		return nil, &dialConfigurationError{err: errors.New("runner token source is required")}
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	otelInterceptor, err := otelconnect.NewInterceptor()
	if err != nil {
		return nil, &dialConfigurationError{err: fmt.Errorf("otel: connect interceptor: %w", err)}
	}
	client := compassv1internalconnect.NewRunnerServiceClient(
		httpClient, cfg.ServerAddr,
		// otelconnect goes first (outermost) so enroll/Sessions dials emit
		// client spans; it is a no-op when no global provider is installed.
		connect.WithInterceptors(otelInterceptor, &bearerToken{source: cfg.Token}),
	)
	resp, err := client.Enroll(ctx, connect.NewRequest(&compassv1internal.EnrollRequest{
		RunnerId:      cfg.RunnerID,
		RuntimeTier:   runtimeTierProto(runtime.TierOf(cfg.Engine)),
		EgressPosture: egressPostureProto(runtime.PostureOf(cfg.Engine)),
	}))
	if err != nil {
		return nil, fmt.Errorf("enrolling runner %q: %w", cfg.RunnerID, err)
	}
	runnerID := resp.Msg.GetRunnerId()
	if runnerID == "" {
		runnerID = cfg.RunnerID
	}
	return &ServerLink{
		client:     client,
		runnerID:   runnerID,
		reattached: resp.Msg.GetReattached(),
	}, nil

}
