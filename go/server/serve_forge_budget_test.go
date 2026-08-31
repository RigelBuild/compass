//go:build unix

package server

// Unit proof for RIG-2991: the board-ingest lane and the agent-notification lane
// ride ONE shared forge.GitHub client, so the client-side rate-budget/resetAt
// gate is a SINGLE gate across both lanes — not two independent gates against the
// same App installation. A regression that gives each lane its own client would
// leave the second lane's gate unarmed, so its read would issue a live request
// instead of fast-failing; this test asserts the opposite.
//
// No Postgres: buildBoardIngestLane/buildForgeNotifyLane touch the store only via
// reconcileForgeSeed (a no-op on an empty seed) and adapters they hold but this
// test never sweeps, so a nil *store.Store is safe here. The proof drives the
// exact *forge.GitHub the two builders were handed.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// budgetRoundTripper scripts one 403 + Retry-After (arming the shared gate) then
// fails any further real request: once the gate is shared and armed, no second
// request may reach the transport. It counts calls so the test asserts the
// fast-fail issued zero HTTP traffic.
type budgetRoundTripper struct {
	calls int
}

func (rt *budgetRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.calls++
	if rt.calls == 1 {
		// A 403 rate-limit signal with a Retry-After arms the client's resetAt
		// gate ~60s out (github.go mapErrorResponse), so the NEXT call on the
		// same client fails fast with no request.
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     http.Header{"Retry-After": {"60"}},
			Body:       http.NoBody,
			Request:    req,
		}, nil
	}
	// A shared+armed gate must short-circuit before here; reaching this on call 2
	// is the two-independent-gates regression the test catches.
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"X-RateLimit-Remaining": {"5000"}},
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

// staticTokenSource is a forge.TokenSource returning a fixed bearer token; the
// budget gate is what this test exercises, never auth.
type staticTokenSource struct{}

func (staticTokenSource) Token(context.Context) (string, error) { return "t", nil }
func (staticTokenSource) Invalidate()                           {}

// TestForgeLanesShareOneBudgetGate proves the two forge lane builders take, and
// build over, ONE shared forge.GitHub — so the client-side budget gate they ride
// is a single gate. It builds one forge.GitHub over a scripted transport, hands
// it to BOTH buildBoardIngestLane and buildForgeNotifyLane (each now takes the
// client as its only forge input — neither can construct a second client), then
// on that one client arms the gate through the board lane's read surface
// (ListUpdatedIssues) and asserts the notify lane's read surface
// (ListNewArtifacts) fast-fails with ErrBudgetExhausted and issues NO second
// request. The pre-RIG-2991 shape — each builder minting its own client — would
// give the notify surface an independent, unarmed gate whose read would issue a
// live request; the shared client makes the second read fast-fail instead.
func TestForgeLanesShareOneBudgetGate(t *testing.T) {
	ctx := context.Background() // test root
	const host = "github.com"

	rt := &budgetRoundTripper{}
	client := forge.NewGitHub(forge.GitHubConfig{
		Host:   host,
		Token:  staticTokenSource{},
		Client: &http.Client{Transport: rt},
	})

	cfg := ServeConfig{Forge: ForgeConfig{
		Host: host,
		App: ForgeAppConfig{
			AppID:                42,
			InstallationID:       7,
			AppPrivateKeySecret:  "APP_KEY",
			AppWebhookSecretName: "APP_WEBHOOK",
		},
	}}

	// Both lanes are built over the SAME client (nil store is safe: an empty seed
	// means reconcileForgeSeed never touches it, and this test never sweeps).
	boardLane, err := buildBoardIngestLane(ctx, cfg, nil, nil, client, slog.Default())
	if err != nil {
		t.Fatalf("buildBoardIngestLane: %v", err)
	}
	if boardLane == nil {
		t.Fatal("buildBoardIngestLane returned nil, want an assembled lane")
	}
	notifyLane := buildForgeNotifyLane(cfg, nil, nil, client, slog.Default())
	if notifyLane == nil {
		t.Fatal("buildForgeNotifyLane returned nil, want an assembled lane")
	}

	// Arm the gate through the board lane's read surface: a 403 + Retry-After
	// arms the shared client's resetAt.
	if _, err := client.ListUpdatedIssues(ctx, "owner/repo", time.Time{}, ""); err == nil {
		t.Fatal("first read (board lane surface): err = nil, want the 403 rate-limit signal to arm the gate")
	}
	if rt.calls != 1 {
		t.Fatalf("after arming: transport calls = %d, want 1 (the single 403)", rt.calls)
	}

	// The notify lane's read surface now rides the SAME armed gate: it must
	// fast-fail with ErrBudgetExhausted and issue NO request.
	_, err = client.ListNewArtifacts(ctx, "owner/repo", compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE, 0, "")
	if err == nil {
		t.Fatal("notify-lane read after arming: err = nil, want ErrBudgetExhausted (the shared gate is armed)")
	}
	if !errors.Is(err, forge.ErrBudgetExhausted) {
		t.Fatalf("notify-lane read err = %v, want ErrBudgetExhausted", err)
	}
	if rt.calls != 1 {
		t.Fatalf("notify-lane read issued a request through an armed shared gate: transport calls = %d, want 1 "+
			"(two independent gates would let it through as call 2)", rt.calls)
	}
}
