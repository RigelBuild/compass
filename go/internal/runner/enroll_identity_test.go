//go:build unix

package runner

// Dial declares the Runner's runtime tier and egress posture ONCE at enrollment,
// derived from the engine it drives. This drives the real Dial (interceptor-
// wrapped client) against a recording RunnerService handler and asserts the
// EnrollRequest carried the right wire enums for a HOST engine (tier HOST, egress
// UNENFORCED) — the two facts the hub stamps onto every session status.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// recordingEnroll is a RunnerService handler that captures the EnrollRequest it
// received, so a Dial test can assert on the tier + posture the client declared.
type recordingEnroll struct {
	compassv1internalconnect.UnimplementedRunnerServiceHandler
	mu               sync.Mutex
	req              *compassv1internal.EnrollRequest
	assignedRunnerID string
	authorization    []string
}

func (r *recordingEnroll) Enroll(_ context.Context, req *connect.Request[compassv1internal.EnrollRequest]) (*connect.Response[compassv1internal.EnrollResponse], error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.req = req.Msg
	r.authorization = append(r.authorization, req.Header().Get("Authorization"))
	return connect.NewResponse(&compassv1internal.EnrollResponse{
		Reattached: false,
		RunnerId:   r.assignedRunnerID,
	}), nil
}

func (r *recordingEnroll) FetchSecrets(_ context.Context, req *connect.Request[compassv1internal.FetchSecretsRequest]) (*connect.Response[compassv1internal.FetchSecretsResponse], error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.authorization = append(r.authorization, req.Header().Get("Authorization"))
	return connect.NewResponse(&compassv1internal.FetchSecretsResponse{}), nil
}

func (r *recordingEnroll) authRequests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.authorization...)
}

func (r *recordingEnroll) enrolled() *compassv1internal.EnrollRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.req
}

// recordingEnrollServer stands up an h2c httptest RunnerService serving rec and
// returns its base URL, torn down via t.Cleanup.
func recordingEnrollServer(t *testing.T, rec *recordingEnroll) string {
	t.Helper()
	path, handler := compassv1internalconnect.NewRunnerServiceHandler(rec)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestDialDeclaresEngineTierAndPosture pins the enrollment declaration: dialing
// with a HOST engine sends runtime_tier=HOST and egress_posture=UNENFORCED on the
// EnrollRequest, derived from the engine via runtime.TierOf / runtime.PostureOf.
//
// Negative control: dropping the two fields from Dial's EnrollRequest (the
// pre-fix shape, runner_id only) reddens both assertions — observed "runtime_tier
// = RUNTIME_TIER_UNSPECIFIED, want RUNTIME_TIER_HOST".
func TestDialDeclaresEngineTierAndPosture(t *testing.T) {
	rec := &recordingEnroll{}
	url := recordingEnrollServer(t, rec)

	// context.Background() is the test root context.
	if _, err := Dial(context.Background(), RunnerConfig{
		RunnerID:   "r-1",
		ServerAddr: url,
		Token:      StaticToken("tok"),
		Engine:     runtime.NewHostRuntime(t.TempDir()),
	}); err != nil {
		t.Fatalf("Dial = %v, want success", err)
	}

	got := rec.enrolled()
	if got == nil {
		t.Fatal("handler recorded no EnrollRequest")
	}
	if got.GetRuntimeTier() != compassv1.RuntimeTier_RUNTIME_TIER_HOST {
		t.Errorf("EnrollRequest runtime_tier = %v, want RUNTIME_TIER_HOST", got.GetRuntimeTier())
	}
	if got.GetEgressPosture() != compassv1.EgressPosture_EGRESS_POSTURE_UNENFORCED {
		t.Errorf("EnrollRequest egress_posture = %v, want EGRESS_POSTURE_UNENFORCED", got.GetEgressPosture())
	}
}

func TestDialAdoptsServerAssignedRunnerID(t *testing.T) {
	const assignedID = "cluster/node"
	rec := &recordingEnroll{assignedRunnerID: assignedID}
	url := recordingEnrollServer(t, rec)
	link, err := Dial(context.Background(), RunnerConfig{
		ServerAddr: url,
		Token:      StaticToken("tok"),
		Engine:     runtime.NewHostRuntime(t.TempDir()),
	})
	if err != nil {
		t.Fatalf("Dial = %v, want success", err)
	}
	if got := rec.enrolled().GetRunnerId(); got != "" {
		t.Fatalf("EnrollRequest runner_id = %q, want empty so Server can assign it", got)
	}
	if got := link.RunnerID(); got != assignedID {
		t.Fatalf("ServerLink.RunnerID() = %q, want %q", got, assignedID)
	}
}

func TestDialFallsBackToConfiguredRunnerID(t *testing.T) {
	rec := &recordingEnroll{}
	url := recordingEnrollServer(t, rec)
	link, err := Dial(context.Background(), RunnerConfig{
		RunnerID:   "legacy-runner",
		ServerAddr: url,
		Token:      StaticToken("tok"),
		Engine:     runtime.NewHostRuntime(t.TempDir()),
	})
	if err != nil {
		t.Fatalf("Dial = %v, want success", err)
	}
	if got := link.RunnerID(); got != "legacy-runner" {
		t.Fatalf("ServerLink.RunnerID() = %q, want configured ID fallback", got)
	}
}

func TestFileTokenChangesBetweenRPCs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("write first token: %v", err)
	}
	rec := &recordingEnroll{}
	url := recordingEnrollServer(t, rec)
	link, err := Dial(context.Background(), RunnerConfig{
		RunnerID:   "runner-1",
		ServerAddr: url,
		Token:      NewFileToken(path, discardLoggerRunner(), time.Now),
		Engine:     runtime.NewHostRuntime(t.TempDir()),
	})
	if err != nil {
		t.Fatalf("Dial = %v, want success", err)
	}
	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatalf("rewrite token: %v", err)
	}
	if _, err := link.FetchSecrets(context.Background(), "session-1"); err != nil {
		t.Fatalf("FetchSecrets = %v, want success", err)
	}
	want := []string{"Bearer first", "Bearer second"}
	if got := rec.authRequests(); !slices.Equal(got, want) {
		t.Fatalf("RPC authorization headers = %v, want %v", got, want)
	}
}

type errorAfterEnrollToken struct {
	mu    sync.Mutex
	calls int
}

func (s *errorAfterEnrollToken) Token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls == 1 {
		return "enroll-token", nil
	}
	return "", errors.New("projected token unavailable")
}

type sessionsHeaderRecorder struct {
	recordingEnroll
	authorization chan string
}

func (h *sessionsHeaderRecorder) Sessions(_ context.Context, stream *connect.BidiStream[compassv1internal.SessionsRequest, compassv1internal.SessionsResponse]) error {
	h.authorization <- stream.RequestHeader().Get("Authorization")
	_, err := stream.Receive()
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func TestSessionsOmitsAuthorizationWhenTokenSourceErrors(t *testing.T) {
	handler := &sessionsHeaderRecorder{authorization: make(chan string, 1)}
	path, service := compassv1internalconnect.NewRunnerServiceHandler(handler)
	mux := http.NewServeMux()
	mux.Handle(path, service)
	server := httptest.NewUnstartedServer(mux)
	server.Config.Protocols = cleartextHTTP2()
	server.Start()
	t.Cleanup(server.Close)

	link, err := Dial(context.Background(), RunnerConfig{
		RunnerID:   "runner-1",
		ServerAddr: server.URL,
		Token:      &errorAfterEnrollToken{},
		Engine:     runtime.NewHostRuntime(t.TempDir()),
		HTTPClient: h2cHTTPClient(t),
	})
	if err != nil {
		t.Fatalf("Dial = %v, want successful enrollment", err)
	}
	if err := link.RunSessions(context.Background(), nil, discardLoggerRunner()); err != nil {
		t.Fatalf("RunSessions = %v, want clean end", err)
	}
	if got := <-handler.authorization; got != "" {
		t.Fatalf("Sessions Authorization = %q, want absent after Token error", got)
	}
}
