//go:build unix

package server

// Unit proof for RIG-2991: board ingestion, notify, and author calls for the
// default installation share the primary client's rate gate through the router.
// No Postgres.
import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/linearagent"
)

// budgetRoundTripper scripts one 403 + Retry-After (arming the shared gate) then
// fails any further real request: once the gate is shared and armed, no second
// request may reach the transport. It counts calls so the test asserts the
// fast-fail issued zero HTTP traffic.
type budgetRoundTripper struct {
	calls int
}

func (rt *budgetRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet && req.URL.Path == "/app/installations" {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`[{"id":7,"account":{"login":"owner-a"}}]`)),
			Request:    req,
		}, nil
	}
	rt.calls++
	if rt.calls == 1 {
		// A 403 with Retry-After arms the client's resetAt gate, so its next call fails fast.
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     http.Header{"Retry-After": {"60"}},
			Body:       http.NoBody,
			Request:    req,
		}, nil
	}
	// Reaching this on call 2 means a second, independent gate let the request through.
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

// TestForgeLanesShareOneBudgetGate proves the board-ingest and agent-notification
// lanes share their default installation's primary rate gate through one router.
// It observes both lanes' reader wiring, arms the gate through board reconciliation,
// and confirms a notify read is rejected without issuing another HTTP request.
func TestForgeLanesShareOneBudgetGate(t *testing.T) {
	ctx := context.Background() // test root
	const host = "github.com"
	rt := &budgetRoundTripper{}
	httpClient := &http.Client{Transport: rt}
	primary := forge.NewGitHub(forge.GitHubConfig{
		Host:   host,
		Token:  staticTokenSource{},
		Client: httpClient,
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

	appConfig := forge.GitHubAppConfig{
		AppID:          42,
		InstallationID: 7,
		PrivateKey:     func(context.Context) ([]byte, error) { return testForgeAppKey(t), nil },
		Host:           host,
		Client:         httpClient,
	}
	installationResolver, err := forge.NewInstallationResolver(appConfig)
	if err != nil {
		t.Fatalf("NewInstallationResolver: %v", err)
	}
	router := forge.NewOwnerRoutedGitHub(primary, installationResolver, appConfig, httpClient)
	boardLane, err := buildBoardIngestLane(ctx, cfg, nil, nil, router, nil, slog.Default())
	if err != nil {
		t.Fatalf("buildBoardIngestLane: %v", err)
	}
	if boardLane == nil {
		t.Fatal("buildBoardIngestLane returned nil, want an assembled lane")
	}
	notifyLane := buildForgeNotifyLane(cfg, nil, nil, router, slog.Default())
	if notifyLane == nil {
		t.Fatal("buildForgeNotifyLane returned nil, want an assembled lane")
	}
	if boardLane.router != router || notifyLane.reader != forge.NotifyReader(router) {
		t.Fatal("board and notify lanes do not record the same owner router")
	}
	// The first primary-client read arms the shared default-installation gate.
	if _, err := primary.ListUpdatedIssues(ctx, "owner-a/repo", time.Time{}, ""); err == nil {
		t.Fatal("primary read: err = nil, want the 403 rate-limit signal to arm the gate")
	}
	if rt.calls != 1 {
		t.Fatalf("after arming primary: transport calls = %d, want 1 (the single 403)", rt.calls)
	}
	if _, err := boardLane.router.ListUpdatedIssues(ctx, "owner-a/repo", time.Time{}, ""); !errors.Is(err, forge.ErrBudgetExhausted) {
		t.Fatalf("board lane default-owner read error = %v, want ErrBudgetExhausted from primary gate", err)
	}

	_, err = notifyLane.reader.ListNewArtifacts(ctx, "owner-a/repo", compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE, 0, "")
	if err == nil {
		t.Fatal("notify lane's reader after arming: err = nil, want ErrBudgetExhausted (the shared gate is armed)")
	}
	if !errors.Is(err, forge.ErrBudgetExhausted) {
		t.Fatalf("notify lane's reader err = %v, want ErrBudgetExhausted", err)
	}
	if rt.calls != 1 {
		t.Fatalf("notify lane's reader issued a request through an armed shared gate: transport calls = %d, want 1", rt.calls)
	}

	reg := newForgeProviderRegistry()
	reviewerClient := forge.NewGitHub(forge.GitHubConfig{Host: host, Token: staticTokenSource{}})
	if err := registerGitHubForgeCoordinate(reg, cfg.Forge.resolved(), router, reviewerClient); err != nil {
		t.Fatalf("register: %v", err)
	}
	resolved, ok := reg.resolve(nil)
	if !ok {
		t.Fatal("registry did not resolve the default GitHub coordinate")
	}
	if resolved.author != forge.Provider(router) {
		t.Fatal("author role is not the shared owner router")
	}
	_, err = resolved.author.CommentOnIssue(ctx, "owner-a/repo", 1, "body", "")
	if err == nil {
		t.Fatal("author write after arming: err = nil, want ErrBudgetExhausted (the shared gate is armed)")
	}
	if !errors.Is(err, forge.ErrBudgetExhausted) {
		t.Fatalf("author write err = %v, want ErrBudgetExhausted", err)
	}
	if rt.calls != 1 {
		t.Fatalf("author write issued a request through an armed shared gate: transport calls = %d, want 1", rt.calls)
	}
}

// TestForgeNotifyLaneWiresPullNumberResolver pins the ASSEMBLY of RIG-2869's
// step 0, which no unit test reaches: every other test of the resolution path
// constructs its own router, so all of them would keep passing if
// buildForgeNotifyLane stopped threading a resolver. That regression is
// invisible until production, where it silently restores the bug — a
// check_suite webhook that cannot route.
//
// It also pins the resolver onto the SHARED client. A resolver built over a
// freshly-minted client would carry its own rate-limit gate, quietly breaking
// the one-budget-gate invariant buildForgeNotifyLane's siblings maintain.
func TestForgeNotifyLaneWiresPullNumberResolver(t *testing.T) {
	ctx := context.Background() // test root
	const host = "github.com"

	rt := &budgetRoundTripper{}
	httpClient := &http.Client{Transport: rt}
	client := forge.NewGitHub(forge.GitHubConfig{
		Host:   host,
		Token:  staticTokenSource{},
		Client: httpClient,
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

	appHTTP := httpClient
	appConfig := forge.GitHubAppConfig{
		AppID:          42,
		InstallationID: 7,
		PrivateKey:     func(context.Context) ([]byte, error) { return testForgeAppKey(t), nil },
		Host:           host,
		Client:         appHTTP,
	}
	installationResolver, err := forge.NewInstallationResolver(appConfig)
	if err != nil {
		t.Fatalf("NewInstallationResolver: %v", err)
	}
	router := forge.NewOwnerRoutedGitHub(client, installationResolver, appConfig, appHTTP)
	notifyLane := buildForgeNotifyLane(cfg, nil, nil, router, slog.Default())
	if notifyLane == nil {
		t.Fatal("buildForgeNotifyLane returned nil, want an assembled lane")
	}

	// (1) The GitHub lane threaded a resolver at all — the assertion that fails
	// if a future edit passes nil.
	if notifyLane.pulls == nil {
		t.Fatal("GitHub notify lane recorded a nil PullNumberResolver: step 0 would never resolve, " +
			"so every check_suite webhook fails the zero-number guard (RIG-2869)")
	}

	// (2) Arm the shared gate through the lane's own recorded read client.
	if _, err := router.ListUpdatedIssues(ctx, "owner-a/repo", time.Time{}, ""); err == nil {
		t.Fatal("first read: err = nil, want the 403 rate-limit signal to arm the gate")
	}
	if rt.calls != 1 {
		t.Fatalf("after arming: transport calls = %d, want 1 (the single 403)", rt.calls)
	}

	// (3) The recorded resolver rides that SAME armed gate: it fast-fails and
	// issues no request. A resolver over a separately-minted client would carry
	// its own unarmed gate and let this through as call 2.
	_, err = notifyLane.pulls.PullNumberForSHA(ctx, "owner-a/repo", "abc123headsha")
	if err == nil {
		t.Fatal("resolver after arming: err = nil, want ErrBudgetExhausted (the shared gate is armed)")
	}
	if !errors.Is(err, forge.ErrBudgetExhausted) {
		t.Fatalf("resolver err = %v, want ErrBudgetExhausted", err)
	}
	if rt.calls != 1 {
		t.Fatalf("resolver issued a request through an armed shared gate: transport calls = %d, want 1 "+
			"(a separately-minted client would let it through as call 2)", rt.calls)
	}

	// (4) The Linear lane records NO resolver: Linear is issues-only and never
	// emits a CHECKS event, so there is no head SHA to resolve.
	linearLane := buildLinearNotifyLane(nil, nil, nil, slog.Default())
	if linearLane != nil && linearLane.pulls != nil {
		t.Error("Linear notify lane recorded a PullNumberResolver, want nil (Linear emits no CHECKS event)")
	}
}

// TestNotifyLanesWireIdentityResolver pins that BOTH assembled lanes RECORD a
// non-nil self-origin identity seam — the test-observable proxy for the seam
// each builder threaded into its router, the same proxy the pulls test above
// uses. It catches a builder that resolved no identity seam at all; the e2e
// cells cover the router's use of one. Without it nothing observes the
// assembly, and a nil seam would compile and leave every suppression suite
// green (they build their own routers) while production notified each agent
// about its own actions forever.
func TestNotifyLanesWireIdentityResolver(t *testing.T) {
	cfg := ServeConfig{Forge: ForgeConfig{Host: "github.com", App: ForgeAppConfig{
		AppID:                42,
		InstallationID:       7,
		AppPrivateKeySecret:  "APP_KEY",
		AppWebhookSecretName: "APP_WEBHOOK",
	}}}

	appHTTP := &http.Client{Transport: &budgetRoundTripper{}}
	appConfig := forge.GitHubAppConfig{
		AppID:          42,
		InstallationID: 7,
		PrivateKey:     func(context.Context) ([]byte, error) { return testForgeAppKey(t), nil },
		Host:           "github.com",
		Client:         appHTTP,
	}
	installationResolver, err := forge.NewInstallationResolver(appConfig)
	if err != nil {
		t.Fatalf("NewInstallationResolver: %v", err)
	}
	primary := forge.NewGitHub(forge.GitHubConfig{Host: "github.com", Token: staticTokenSource{}, Client: appHTTP})
	router := forge.NewOwnerRoutedGitHub(primary, installationResolver, appConfig, appHTTP)
	ghLane := buildForgeNotifyLane(cfg, nil, nil, router, slog.Default())
	if ghLane == nil {
		t.Fatal("buildForgeNotifyLane returned nil, want an assembled lane")
	}
	if ghLane.identities == nil {
		t.Error("GitHub notify lane recorded a nil IdentityResolver: self-origin suppression " +
			"would be disabled wholesale, so agents keep being notified about their own actions")
	}

	// The Linear lane needs a token source to assemble at all; without one it is
	// the documented off-state (nil lane), which wires nothing.
	linearLane := buildLinearNotifyLane(nil, nil, linearagent.NewTokenSource("cid", "csecret", nil, ""), slog.Default())
	if linearLane == nil {
		t.Fatal("buildLinearNotifyLane returned nil for a configured token source, want a lane")
	}
	if linearLane.identities == nil {
		t.Error("Linear notify lane recorded a nil IdentityResolver: Linear-side self-origin " +
			"suppression would be disabled wholesale")
	}
}
