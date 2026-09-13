//go:build unix

package runner

// Dial declares the Runner's runtime tier and egress posture ONCE at enrollment,
// derived from the engine it drives. This drives the real Dial (interceptor-
// wrapped client) against a recording RunnerService handler and asserts the
// EnrollRequest carried the right wire enums for a HOST engine (tier HOST, egress
// UNENFORCED) — the two facts the hub stamps onto every session status.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

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
	mu  sync.Mutex
	req *compassv1internal.EnrollRequest
}

func (r *recordingEnroll) Enroll(_ context.Context, req *connect.Request[compassv1internal.EnrollRequest]) (*connect.Response[compassv1internal.EnrollResponse], error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.req = req.Msg
	return connect.NewResponse(&compassv1internal.EnrollResponse{Reattached: false}), nil
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
		Token:      "tok",
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
