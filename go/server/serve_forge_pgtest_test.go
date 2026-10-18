//go:build pgtest && unix

package server

// Store-gated tests drive signed board webhooks through Postgres, covering
// durable issue writes, App-absent boot, and production scope gates.

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/envelope"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/ingest"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
)

const forgeTestHost = "github.com"

// forgeTestStore opens a fresh isolated-schema store for a forge pgtest.
func forgeTestStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(context.Background(), dsn) // test root context
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// Scripted issue hydrator for the webhook path. It returns the requested issue
// number without live GitHub access and implements the conditional-read seam.
type fakeBoardHydrator struct {
	issue forge.Issue
}

func (h *fakeBoardHydrator) GetIssueConditional(_ context.Context, _ string, number uint64, _ string) (forge.ConditionalResult[forge.Issue], error) {
	iss := h.issue
	iss.Number = number
	return forge.ConditionalResult[forge.Issue]{V: iss}, nil
}

// signalingIssueSink wraps the real IssueProjection sink and fires published
// after each PublishIssueUpdate durably commits, so the test event-gates on the
// actual persist rather than a fixed sleep. It satisfies the Ingester's issueSink
// seam structurally (PublishIssueUpdate).
type signalingIssueSink struct {
	inner     *board.IssueProjection
	published chan struct{}
}

func (s *signalingIssueSink) PublishIssueUpdate(ctx context.Context, issue *compassv1.Issue) error {
	if err := s.inner.PublishIssueUpdate(ctx, issue); err != nil {
		return err
	}
	select {
	case s.published <- struct{}{}:
	default:
	}
	return nil
}

// capHandler captures slog records for the Warn-path assertions.
type capHandler struct {
	recs []slog.Record
}

func (h *capHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capHandler) Handle(_ context.Context, r slog.Record) error {
	h.recs = append(h.recs, r.Clone())
	return nil
}
func (h *capHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capHandler) WithGroup(string) slog.Handler      { return h }

func warnCount(recs []slog.Record) int {
	n := 0
	for _, r := range recs {
		if r.Level == slog.LevelWarn {
			n++
		}
	}
	return n
}

// --- test: signed webhook -> board projection observes the hydrated issue -----

// A signed webhook through the mounted ingress hydrates and stores an issue at
// its forge coordinate. Only hydration is fake; secret and target gates are real.
func TestBoardWebhookIngressSinksHydratedIssue(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root

	const repo = "owner/repo"
	// Seed the repo as an enabled target so the arm's IsEnabledRepo gate admits it.
	if err := st.EnsureForgeRepoSubscription(ctx, store.ForgeRepoSubscription{
		Provider: store.ForgeProviderGitHub, Host: forgeTestHost, Repo: repo, Enabled: true,
	}); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	brd := board.NewIssueProjection(bus, st)

	// The arm exactly as buildBoardIngestLane assembles it: a hydrator (faked
	// here), the shared Ingester over the real projection sink (wrapped so the
	// test event-gates on the durable publish, never a sleep), and the real
	// boardTargetStore gate over *store.Store.
	published := make(chan struct{}, 1)
	sink := &signalingIssueSink{inner: brd, published: published}
	ing := ingest.NewIngester(forge.NewFakeProvider("gh"), sink, &compassv1.ForgeRef{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
		Host:     forgeTestHost,
	})
	hydrator := &fakeBoardHydrator{issue: forge.Issue{
		Title:        "a bug",
		Body:         "it broke",
		State:        "open",
		URL:          "https://github.com/owner/repo/issues/1",
		ForgeAccount: "octocat",
	}}
	arm := ingest.NewBoardWebhookArm(hydrator, ing, &boardTargetStore{st: st}, ingest.BoardArmConfig{})

	// Drive the arm's drain on the serve-like errgroup member for the test window.
	armCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- arm.Run(armCtx) }()

	// Mount the REAL ingress: a fanoutSink over the arm, with the webhook secret
	// resolved by the same declared-secret closure shape Serve threads through.
	secret := []byte("gh-webhook-secret")
	_, handler := NewGitHubWebhookHandler(
		func(context.Context) ([]byte, error) { return secret, nil },
		&fanoutSink{sinks: []ForgeEventSink{arm}}, nil)

	// POST a signed issues.opened webhook through the real handler.
	gh := newFakeGitHubForge(secret, repo)
	sw := gh.openIssue(t, 1, "https://github.com/owner/repo/issues/1")
	rec := doPost(t, handler, sw.event, sw.delivery, sw.sig, sw.body)
	if rec.Code != 200 {
		t.Fatalf("ingress code = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// Event-gate on the durable publish: the signaling sink fires after the real
	// projection PublishIssueUpdate persists the issue. Block on that signal, not
	// a clock.
	select {
	case <-published:
	case <-time.After(10 * time.Second):
		t.Fatal("issue never sank to the board within the deadline")
	}
	issues, err := st.ListIssues(ctx)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("stored issues = %d, want 1 (the sunk issue)", len(issues))
	}
	got := issues[0]
	if got.Number != 1 || got.Repo != repo {
		t.Fatalf("stored issue = %+v, want number 1 repo %s", got, repo)
	}
	if got.ForgeState != "open" || got.Title != "a bug" {
		t.Fatalf("stored issue forge fields = %+v, want the hydrated open 'a bug'", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("arm Run returned %v, want nil on clean cancel", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("arm Run did not return after cancel")
	}
}

// --- test: App-config-absent boot leaves both lanes off + Warns on enabled rows

// Without an App, shared wiring disables both lanes and warns once for enabled
// rows at the bound coordinate; no webhook mounts.
func TestBoardIngestionDisabledWarnsOnEnabledRows(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root
	const host = forgeTestHost
	cfg := ServeConfig{Forge: ForgeConfig{Host: host}}

	assertAllNil := func(t *testing.T, lane *boardIngestLane, notify *forgeNotifyLane, sink ForgeEventSink, secret func(context.Context) ([]byte, error), client *forge.GitHub, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("buildBoardWebhookWiring (App absent): %v", err)
		}
		if lane != nil || notify != nil || sink != nil || secret != nil || client != nil {
			t.Fatalf("wiring not all-nil with no App configured: lane==nil? %t notify==nil? %t sink==nil? %t secret==nil? %t client==nil? %t", lane == nil, notify == nil, sink == nil, secret == nil, client == nil)
		}
	}

	t.Run("no Warn when no enabled rows exist", func(t *testing.T) {
		h := &capHandler{}
		lane, notify, sink, secret, client, err := buildBoardWebhookWiring(ctx, cfg, st, nil, nil, &fakeResolver{}, slog.New(h))
		assertAllNil(t, lane, notify, sink, secret, client, err)
		if n := warnCount(h.recs); n != 0 {
			t.Fatalf("Warn count with no enabled rows = %d, want 0", n)
		}
	})

	if err := st.EnsureForgeRepoSubscription(ctx, store.ForgeRepoSubscription{
		Provider: store.ForgeProviderGitHub, Host: host, Repo: "a/b", Enabled: true,
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	t.Run("exactly one Warn when enabled rows exist for the bound coordinate", func(t *testing.T) {
		h := &capHandler{}
		lane, notify, sink, secret, client, err := buildBoardWebhookWiring(ctx, cfg, st, nil, nil, &fakeResolver{}, slog.New(h))
		assertAllNil(t, lane, notify, sink, secret, client, err)
		if n := warnCount(h.recs); n != 1 {
			t.Fatalf("Warn count with an enabled row = %d, want exactly 1", n)
		}
	})

	t.Run("no Warn for a different bound host (abandoned rows give no false comfort)", func(t *testing.T) {
		h := &capHandler{}
		otherCfg := ServeConfig{Forge: ForgeConfig{Host: "other.example.com"}}
		lane, notify, sink, secret, client, err := buildBoardWebhookWiring(ctx, otherCfg, st, nil, nil, &fakeResolver{}, slog.New(h))
		assertAllNil(t, lane, notify, sink, secret, client, err)
		if n := warnCount(h.recs); n != 0 {
			t.Fatalf("Warn count for a different host = %d, want 0 (count is bound-coordinate only)", n)
		}
	})
}

// --- test: App configured but a secret undeclared -> fail fast ----------------

// Configured App secrets must be declared or startup fails before either lane is
// built. The resolver is fake; the store is real.
func TestBoardIngestLaneFailsFastOnMissingAppSecret(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root

	cfg := ServeConfig{Forge: ForgeConfig{
		Host: forgeTestHost,
		App: ForgeAppConfig{
			AppID:                42,
			InstallationID:       7,
			AppPrivateKeySecret:  "APP_KEY",
			AppWebhookSecretName: "APP_WEBHOOK",
		},
	}}

	// Neither App secret declared -> the FIRST validateForgeSecret (app key) fails.
	t.Run("both undeclared fails on the app key", func(t *testing.T) {
		res := &fakeResolver{resolved: nil}
		_, _, _, _, _, err := buildBoardWebhookWiring(ctx, cfg, st, nil, nil, res, slog.Default())
		if err == nil {
			t.Fatal("buildBoardWebhookWiring with no App secrets = nil, want a startup error")
		}
		if !strings.Contains(err.Error(), "APP_KEY") {
			t.Fatalf("error = %q, want it to name the missing app key APP_KEY", err)
		}
	})

	// With only the App key declared, validation must catch the missing webhook secret.
	t.Run("app key present but webhook secret undeclared fails on the webhook secret", func(t *testing.T) {
		// Keep the resolved server-prefixed name distinct from the unprefixed
		// config and assertion, so this subtest reaches webhook-secret
		// validation instead of duplicating the missing-App-key case.
		res := &fakeResolver{resolved: []secrets.ResolvedSecret{{Name: serverSecretName("APP_KEY"), Value: "pem"}}}
		_, _, _, _, _, err := buildBoardWebhookWiring(ctx, cfg, st, nil, nil, res, slog.Default())
		if err == nil {
			t.Fatal("buildBoardWebhookWiring with the webhook secret undeclared = nil, want a startup error")
		}
		if !strings.Contains(err.Error(), "APP_WEBHOOK") {
			t.Fatalf("error = %q, want it to name the missing webhook secret APP_WEBHOOK", err)
		}
	})
}

// Boot declarations must include configured secrets or consumers fail validation.
//
//nolint:contextcheck // test helpers (newTestStore, RequireDSN, must*) root their own context; threading ctx through them would touch every caller
func TestDeclareServerSecretNamesIsIdempotentAndGated(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background()

	t.Run("neither App nor Linear configured declares nothing", func(t *testing.T) {
		if err := declareServerSecretNames(ctx, st, ServeConfig{}); err != nil {
			t.Fatalf("declare with nothing configured: %v", err)
		}
		rows, err := st.DeclaredServerSecrets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// Check the raw config: resolved() defaults Linear credentials, but
		// an unconfigured deployment must not declare them or load a provider.
		if len(rows) != 0 {
			t.Fatalf("declared %d names with nothing configured, want 0: %+v", len(rows), rows)
		}
	})

	t.Run("configured App declares its secrets, prefixed, and re-declares cleanly", func(t *testing.T) {
		cfg := ServeConfig{Forge: ForgeConfig{
			App: ForgeAppConfig{
				AppID: 1, InstallationID: 2,
				AppPrivateKeySecret:  "APP_KEY",
				AppWebhookSecretName: "APP_WEBHOOK",
			},
		}}
		if err := declareServerSecretNames(ctx, st, cfg); err != nil {
			t.Fatalf("first declare: %v", err)
		}
		// Repeated boot declarations must be idempotent.
		if err := declareServerSecretNames(ctx, st, cfg); err != nil {
			t.Fatalf("second declare (idempotence): %v", err)
		}

		rows, err := st.DeclaredServerSecrets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got := make(map[string]bool, len(rows))
		for _, r := range rows {
			got[r.Name] = true
			// Names must have the reserved prefix or the database constraint rejects them.
			if !store.HasServerSecretPrefix(r.Name) {
				t.Fatalf("unprefixed name %q in server_secrets", r.Name)
			}
		}
		for _, want := range []string{serverSecretName("APP_KEY"), serverSecretName("APP_WEBHOOK")} {
			if !got[want] {
				t.Fatalf("%q not declared; have %v", want, got)
			}
		}

		// The end-to-end point: the SERVER resolver's registry is now non-empty
		// and carries exactly the names the re-pointed consumers look for.
		view := store.ServerDeclaredSecrets{Store: st}
		decls, err := view.DeclaredSecrets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(decls) == 0 {
			t.Fatal("server resolver view is empty; Resolve would short-circuit to (nil, nil)")
		}
	})

	// The production write-enabled topology is TWO Apps, and the reviewer PEM is
	// a consumed name (buildForgeWriteService validates it), so a regression
	// that stops declaring it is a hard boot failure for every such deployment.
	t.Run("both Apps declare all three names, reviewer PEM included", func(t *testing.T) {
		st := forgeTestStore(t)
		cfg := ServeConfig{Forge: ForgeConfig{
			App: ForgeAppConfig{
				AppID: 1, InstallationID: 2,
				AppPrivateKeySecret:  "APP_KEY",
				AppWebhookSecretName: "APP_WEBHOOK",
			},
			ReviewerApp: ForgeAppConfig{
				AppID: 3, InstallationID: 4,
				AppPrivateKeySecret: "REVIEWER_KEY",
			},
		}}
		if err := declareServerSecretNames(ctx, st, cfg); err != nil {
			t.Fatalf("declare: %v", err)
		}
		assertDeclaredExactly(ctx, t, st,
			serverSecretName("APP_KEY"),
			serverSecretName("APP_WEBHOOK"),
			serverSecretName("REVIEWER_KEY"),
		)
	})

	// Reviewer-only configuration declares its key so validation can report missing
	// primary write credentials.
	t.Run("reviewer App alone still declares its PEM, so the partial config is diagnosable", func(t *testing.T) {
		st := forgeTestStore(t)
		cfg := ServeConfig{Forge: ForgeConfig{
			ReviewerApp: ForgeAppConfig{
				AppID: 3, InstallationID: 4,
				AppPrivateKeySecret: "REVIEWER_KEY",
			},
		}}
		if err := declareServerSecretNames(ctx, st, cfg); err != nil {
			t.Fatalf("declare: %v", err)
		}
		assertDeclaredExactly(ctx, t, st, serverSecretName("REVIEWER_KEY"))

		// A reviewer key without a primary key must trigger the partial-config warning.
		declared := []secrets.ResolvedSecret{{Name: serverSecretName("REVIEWER_KEY"), Value: "k"}}
		havePrimary, haveReviewer := cfg.Forge.forgeWriteAppsConfigured(declared)
		if havePrimary || !haveReviewer {
			t.Fatalf("forgeWriteAppsConfigured = (primary %v, reviewer %v), want (false, true) — the partial-config warn cannot fire unless these differ", havePrimary, haveReviewer)
		}
	})
}

// Asserts the exact declared-name set: missing secrets break boot, and extras
// can trigger provider loads for lanes the deployment did not configure.
func assertDeclaredExactly(ctx context.Context, t *testing.T, st *store.Store, want ...string) {
	t.Helper()
	rows, err := st.DeclaredServerSecrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(rows))
	for _, r := range rows {
		got[r.Name] = true
	}
	if len(got) != len(want) {
		t.Fatalf("declared %v, want exactly %v", rows, want)
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("%q not declared; have %v", w, got)
		}
	}
}

// Exercises resolver-instance routing through buildDoors, which leaf tests of
// buildLinearWebhookWiring cannot prove. This boundary matters: passing the
// server resolver to runnerhub would expose deployment secrets to agent
// containers; the inverse leaves the webhook handler without its secret.
func TestBuildDoorsRoutesTheResolverInstancesOverTheRealCallGraph(t *testing.T) {
	ctx := context.Background()
	st := forgeTestStore(t)

	// The container uses the real resolver; the server fake has only the webhook
	// secret. Pointer identity verifies which instance reaches each path.
	container := secrets.NewStoreResolver(st, secretsFixtureKey(t), 1)
	server := &fakeResolver{resolved: []secrets.ResolvedSecret{
		{Name: serverSecretName("LWH"), Value: "shh"},
	}}

	// Keep the admin token in a temporary directory, not the source tree.
	cfg := ServeConfig{
		StateDir: t.TempDir(),
		Forge:    ForgeConfig{LinearWebhookSecretName: "LWH"},
	}

	// Require a listener so buildNetworkServer's network-door arm is assembled.
	netListener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = netListener.Close() })

	bus := events.NewBus[busPayload]()
	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	admin, err := st.CreateUser(ctx, store.NewUser{Handle: "routing-admin", DisplayName: "Routing Admin"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	commsSvc := comms.NewComms(ctx, st, commsBus, nil, admin.ID)
	brd := board.NewProjection(bus)
	issueBrd := board.NewIssueProjection(bus, st)
	tail := newSessionTail()
	hub := newRunnerHub(st, brd, tail, commsSvc, slog.Default())
	svc := newService("test", bus, st, hub, brd, issueBrd, tail)
	secretsSvc := newSecretsService(st, container, server, nil)
	doors, err := buildDoors(ctx, cfg, svc, commsSvc, secretsSvc, hub, st, admin.ID, "",
		container, server, nil, netListener, nil, nil, nil, nil, envelope.Key{}, 0)
	if err != nil {
		t.Fatalf("buildDoors: %v", err)
	}
	t.Cleanup(func() {
		if doors.net != nil {
			_ = doors.net.Close()
		}
	})

	// Require a listener so buildNetworkServer assembles the network-door arm.
	if doors.net == nil {
		t.Fatal("no net door built; the container-delivery arm was never exercised")
	}

	// The Linear webhook mounted at all proves the SERVER instance reached
	// buildLinearWebhookWiring: the secret lives only in that fake, and the
	// consumer reads it through the resolver it was handed. Threading the
	// container instance instead yields a nil handler and no mounted route.
	if !hasLinearWebhookRoute(t, doors.net) {
		t.Fatal("Linear webhook not mounted: buildDoors did not thread the SERVER resolver to buildLinearWebhookWiring, so a deployment's webhook would silently never mount (failure mode (b))")
	}

	// D6: the net door — runnerhub's FetchSecrets delivery path — must have
	// received the CONTAINER instance. Pointer identity is the assertion: this
	// is the swap that leaks every deployment secret into every agent container,
	if doors.netResolver == nil || doors.netResolver.inner != container {
		t.Fatal("net door did not receive the CONTAINER resolver: runnerhub FetchSecrets would serve server_secrets, delivering every deployment secret (App PEMs, webhook secrets, Linear credentials) into every agent container")
	}

	// Both fakes must have been read. A fake with zero calls means its consumer
	// never ran, which would make the routing assertions above accidental.
	if server.calls == 0 {
		t.Fatal("server resolver was never read; the webhook assertion is vacuous")
	}
}

// hasLinearWebhookRoute reports whether the net door actually serves the Linear
// webhook path. It probes the mounted mux rather than inspecting wiring structs,
// so it answers the operator-visible question: is the ingress reachable.
func hasLinearWebhookRoute(t *testing.T, srv *http.Server) bool {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhooks/linear", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	// An unmounted path 404s. A mounted handler rejects an unsigned body
	// (401/400) — either way it is NOT a 404, which is the distinction here.
	return rec.Code != http.StatusNotFound
}

func TestBuildForgeWriteServiceEnforcesScopesBehaviorally(t *testing.T) {
	for _, tc := range []struct {
		name                string
		scopeEnforcementOff bool
	}{
		{name: "zero-value config rejects ungranted write"},
		{name: "explicit opt-out allows write", scopeEnforcementOff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background() // test root
			st := forgeTestStore(t)
			owner, err := st.CreateUser(ctx, store.NewUser{Handle: "scope-owner", DisplayName: "Scope Owner"})
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			agent, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "scope-agent", DisplayName: "Scope Agent"})
			if err != nil {
				t.Fatalf("CreateAgent: %v", err)
			}

			primary, providerCalls := newScopeTestGitHubProvider(t)
			const reviewerKey = "REVIEWER_APP_KEY"
			cfg := ServeConfig{Forge: ForgeConfig{
				Host:                     forgeTestHost,
				ScopeEnforcementDisabled: tc.scopeEnforcementOff,
				App:                      ForgeAppConfig{AppID: 42, InstallationID: 7, AppPrivateKeySecret: "APP_KEY"},
				ReviewerApp:              ForgeAppConfig{AppID: 43, InstallationID: 8, AppPrivateKeySecret: reviewerKey},
			}}
			resolver := &fakeResolver{resolved: []secrets.ResolvedSecret{{Name: serverSecretName(reviewerKey), Value: "key"}}}
			svc, err := buildForgeWriteService(ctx, cfg, st, nil, resolver, primary, nil, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatalf("buildForgeWriteService: %v", err)
			}

			call := &compassv1internal.ForgeCallRequest{
				Call: &compassv1internal.ForgeCallRequest_CreateIssue{CreateIssue: &compassv1internal.CreateIssueRequest{
					Repo: "owner/repo", Title: "title", Body: "body",
				}},
			}
			result, err := svc.ExecuteForgeCallAsAccount(ctx, agent.ID, "scope-session", call)
			if err != nil {
				t.Fatalf("ExecuteForgeCallAsAccount: %v", err)
			}
			if !tc.scopeEnforcementOff {
				fe := result.GetError()
				if fe == nil || fe.GetCode() != "not_found" || fe.GetMessage() != "forge: artifact not found" {
					t.Fatalf("ungranted write error = %v, want fixed in-band not_found", fe)
				}
				if n := providerCalls.Load(); n != 0 {
					t.Fatalf("provider calls = %d, want 0", n)
				}
				return
			}
			if result.GetError() != nil || result.GetIssue() == nil {
				t.Fatalf("opt-out write result = %v, want created issue", result.GetError())
			}
			if n := providerCalls.Load(); n != 1 {
				t.Fatalf("opt-out provider calls = %d, want 1", n)
			}
		})
	}
}

func newScopeTestGitHubProvider(t *testing.T) (*forge.GitHub, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/repos/owner/repo/issues" {
			t.Errorf("provider request = %s %s, want POST /repos/owner/repo/issues", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if _, err := w.Write([]byte(`{"number":17,"title":"created","body":"body","state":"open","html_url":"https://github.com/owner/repo/issues/17","user":{"login":"compass"}}`)); err != nil {
			t.Errorf("write fake provider response: %v", err)
		}
	}))
	t.Cleanup(api.Close)
	target, err := url.Parse(api.URL)
	if err != nil {
		t.Fatalf("parse fake provider URL: %v", err)
	}
	primary := forge.NewGitHub(forge.GitHubConfig{
		Host:   forgeTestHost,
		Token:  staticTokenSource{},
		Client: &http.Client{Transport: &armedRewriteTransport{target: target}},
	})
	return primary, calls
}
