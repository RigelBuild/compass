//go:build pgtest && unix

package server

// Store-gated end-to-end proofs for the RIG-2883 board webhook-ingestion serve
// wiring: the pieces buildBoardIngestLane assembles (the boardTargetStore adapter
// over a real *store.Store, the shared IssueProjection sink, the T1 webhook arm
// behind the mounted /webhooks/github ingress) driven over the REAL signed
// ingress against a REAL Postgres — no live GitHub (hydration is a fake). Behind
// `pgtest && unix` (SKIP when no runtime). Each test opens its own isolated-schema
// store (pgtest.RequireDSN + store.Open), so parallel packages never collide.
//
// The store-backed test-cycle items live here:
//   - a signed fake `issues` webhook POSTed through the mounted /webhooks/github
//     handler -> the board arm hydrates the coordinate and sinks the issue, which
//     lands durably in the store at its forge coordinate;
//   - App-config-ABSENT boot: buildBoardIngestLane returns a nil lane, and
//     warnDisabledBoardIngestion Warns when enabled subscription rows exist (no
//     webhook mounted, no panic).
//
// These pgtests only need to COMPILE locally; the CI gate runs them against
// suite Postgres.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/ingest"
	"github.com/RigelBuild/compass/go/internal/pgtest"
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

// fakeBoardHydrator is a scripted issueHydrator (the shared GitHub client's role
// on the webhook path): it returns a fixed issue stamped with the requested
// number, so the ingress -> arm -> Ingester -> store pipeline is exercised end to
// end without a live GitHub. It satisfies the arm's conditional point-read seam
// structurally (GetIssueConditional), the same shape *forge.GitHub provides.
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

// TestBoardWebhookIngressSinksHydratedIssue drives the REAL landed ingress: a
// signed fake `issues` webhook POSTed through the mounted /webhooks/github
// handler (fanoutSink over the board arm) causes the arm to hydrate the changed
// coordinate and sink the fresh issue through the shared Ingester, which lands it
// durably in the store at its forge coordinate. The webhook secret and the target
// gate are the REAL server seams (newDeclaredSecretResolver-shaped closure and
// boardTargetStore over the enabled subscription); only the hydrate is a fake.
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
	rec := doPost(handler, sw.event, sw.delivery, sw.sig, sw.body)
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

// --- test: App-config-absent boot leaves the lane off + Warns on enabled rows --

// TestBoardIngestionDisabledWarnsOnEnabledRows proves the App-absent posture:
// buildBoardIngestLane returns a nil lane (no App configured), and
// warnDisabledBoardIngestion Warns exactly once when the table holds enabled
// subscription rows for the bound coordinate — no webhook mounted, no panic.
func TestBoardIngestionDisabledWarnsOnEnabledRows(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root
	const host = forgeTestHost

	// App absent -> nil lane, no error.
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	brd := board.NewIssueProjection(bus, st)
	lane, err := buildBoardIngestLane(ctx, ServeConfig{Forge: ForgeConfig{Host: host}}, st, brd, &fakeResolver{}, slog.Default())
	if err != nil {
		t.Fatalf("buildBoardIngestLane (App absent): %v", err)
	}
	if lane != nil {
		t.Fatal("lane != nil with no App configured, want nil (board ingestion hard-off)")
	}

	t.Run("no Warn when no enabled rows exist", func(t *testing.T) {
		h := &capHandler{}
		warnDisabledBoardIngestion(ctx, st, store.ForgeProviderGitHub, host, slog.New(h))
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
		warnDisabledBoardIngestion(ctx, st, store.ForgeProviderGitHub, host, slog.New(h))
		if n := warnCount(h.recs); n != 1 {
			t.Fatalf("Warn count with an enabled row = %d, want exactly 1", n)
		}
	})

	t.Run("no Warn for a different bound host (abandoned rows give no false comfort)", func(t *testing.T) {
		h := &capHandler{}
		warnDisabledBoardIngestion(ctx, st, store.ForgeProviderGitHub, "other.example.com", slog.New(h))
		if n := warnCount(h.recs); n != 0 {
			t.Fatalf("Warn count for a different host = %d, want 0 (count is bound-coordinate only)", n)
		}
	})
}

// --- test: App configured but a secret undeclared -> fail fast ----------------

// TestBoardIngestLaneFailsFastOnMissingAppSecret pins Constraint #3's fail-fast:
// a configured App (AppID != 0) with an undeclared App secret is a startup error
// (a likely operator typo), not a silent degrade. The resolver is a fake; the
// store is real because buildBoardIngestLane reconciles the seed after a
// successful resolve.
func TestBoardIngestLaneFailsFastOnMissingAppSecret(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	brd := board.NewIssueProjection(bus, st)
	cfg := ServeConfig{Forge: ForgeConfig{
		Host: forgeTestHost,
		App: ForgeAppConfig{
			AppID:                42,
			InstallationID:       7,
			AppPrivateKeySecret:  "APP_KEY",
			AppWebhookSecretName: "APP_WEBHOOK",
		},
	}}

	// Neither App secret declared -> the first validateForgeSecret fails with a
	// not-declared error.
	res := &fakeResolver{resolved: nil}
	_, err := buildBoardIngestLane(ctx, cfg, st, brd, res, slog.Default())
	if err == nil {
		t.Fatal("buildBoardIngestLane with an undeclared App secret = nil, want a startup error")
	}
}
