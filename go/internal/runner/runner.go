//go:build unix

// Package runner is the Runner-side of the Server<->Runner seam (design
// compass-0.6 §T4): the second binary. It dials OUT to the Server over gRPC with
// its per-Runner token, enrolls, opens the Sessions bidi stream to receive
// session commands, and relays agent events up the PublishEvents client-stream.
// It wraps the already-built internal/runtime container layer (AgentRuntime /
// PodmanCLI) — it does not reimplement container hosting.
//
// Because the Runner dials out, the Server has no inbound route to it: every RPC
// is Runner-initiated. Enroll is the handshake; Sessions is Runner-opened with
// the Server pushing commands on the response half; PublishEvents is a
// Runner->Server client-stream. This is the dial-out model the frozen proto
// shape realizes (go-toolchain-default.md:929-934).
package runner

import (
	"context"
	"fmt"
	"net/http"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// RunnerConfig is everything the Runner needs to attach to a Server and host
// agents. ServerAddr is the Server's base URL (the authenticated TCP door);
// Token is the per-Runner bearer credential (OQ7, operator-provisioned, stored
// 0600); Engine is the container runtime the Runner drives (a *runtime.PodmanCLI
// in production, a fake in tests).
type RunnerConfig struct {
	// RunnerID is this Runner's stable identity, cross-checked against the token
	// subject at enrollment.
	RunnerID string
	// ServerAddr is the Server's base URL, e.g. https://server.example:443.
	ServerAddr string
	// Token is the per-Runner bearer token presented on every RPC.
	Token string
	// Engine is the container runtime seam the Runner hosts agents on.
	Engine runtime.ContainerRuntime
	// RuntimeDir is the Runner-owned base directory under which per-container
	// agent sockets live (RuntimeDir/containers/<container>/agent.sock, OQ-5).
	// Owner-only; the socket is a local hop that never touches the network.
	RuntimeDir string
	// AgentModel is the model selector handed to every agent this Runner
	// starts (the agent's COMPASS_MODEL). Empty leaves each agent on its own
	// default rather than exporting a blank value it would have to ignore.
	AgentModel string
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
	token      string
	reattached bool
}

// Reattached reports whether enrollment re-attached an already-registered Runner
// (OQ6 duplicate enrollment) rather than registering fresh.
func (l *ServerLink) Reattached() bool { return l.reattached }

// bearerToken is the interceptor that stamps the per-Runner bearer credential on
// every outbound RPC (unary + streaming), so the Server door authenticates it.
type bearerToken struct {
	token string
}

func (b *bearerToken) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+b.token)
		return next(ctx, req)
	}
}

func (b *bearerToken) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+b.token)
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
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	client := compassv1internalconnect.NewRunnerServiceClient(
		httpClient, cfg.ServerAddr,
		connect.WithInterceptors(&bearerToken{token: cfg.Token}),
	)
	resp, err := client.Enroll(ctx, connect.NewRequest(&compassv1internal.EnrollRequest{
		RunnerId: cfg.RunnerID,
	}))
	if err != nil {
		return nil, fmt.Errorf("enrolling runner %q: %w", cfg.RunnerID, err)
	}
	return &ServerLink{
		client:     client,
		runnerID:   cfg.RunnerID,
		token:      cfg.Token,
		reattached: resp.Msg.GetReattached(),
	}, nil
}
