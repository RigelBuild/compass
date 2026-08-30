//go:build unix

package server

// DB-free unit tests for the RIG-1810 forge wiring that need no Postgres: the
// ForgeConfig enable predicate + defaulting, the TTL-caching TokenSource
// (record test 6), and the two distinct startup secret-validation error texts
// (record test 7's discriminability — validateForgeSecret is a pure function of
// the resolver, so the resolve-errors vs name-absent split is proven here; the
// full Serve fail-fast + listener-cleanup path is proven in the pgtest lane).
//
// The store-backed halves — the seed reconcile, the polling-disabled Warn, and
// the end-to-end boot pipeline — live in serve_forge_pgtest_test.go behind the
// `pgtest` tag.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/secrets"
)

// fakeResolver is a secrets.Resolver whose Resolve returns a scripted set (or a
// scripted error). Set/Delete are unused here. The resolved value can be changed
// between calls so a test proves the TokenSource picks up a rotation.
type fakeResolver struct {
	resolved []secrets.ResolvedSecret
	err      error
	calls    int
}

func (r *fakeResolver) Resolve(_ context.Context, _ string) ([]secrets.ResolvedSecret, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return r.resolved, nil
}

func (r *fakeResolver) Set(context.Context, string, string) error { return nil }
func (r *fakeResolver) Delete(context.Context, string) error      { return nil }

func TestForgeConfigEnableAndDefaults(t *testing.T) {
	t.Run("board ingestion disabled by default", func(t *testing.T) {
		if (ForgeConfig{}).boardIngestionEnabled() {
			t.Fatal("empty ForgeConfig should be board-ingestion-disabled")
		}
	})
	t.Run("board ingestion enabled by a configured App id", func(t *testing.T) {
		if !(ForgeConfig{App: ForgeAppConfig{AppID: 42}}).boardIngestionEnabled() {
			t.Fatal("a non-zero App id should enable board ingestion")
		}
	})
	t.Run("a seed alone does NOT enable board ingestion (App-only)", func(t *testing.T) {
		if (ForgeConfig{SeedRepos: []string{"a/b"}}).boardIngestionEnabled() {
			t.Fatal("a seed without an App must NOT enable board ingestion (Constraint #3)")
		}
	})
	t.Run("defaults applied to zero fields", func(t *testing.T) {
		got := ForgeConfig{}.resolved()
		if got.Host != defaultForgeHost {
			t.Fatalf("Host = %q, want %q", got.Host, defaultForgeHost)
		}
		if got.SecretName != defaultForgeSecretName {
			t.Fatalf("SecretName = %q, want %q", got.SecretName, defaultForgeSecretName)
		}
		if got.App.ReconcileBackstop != defaultReconcileBackstop {
			t.Fatalf("ReconcileBackstop = %v, want %v", got.App.ReconcileBackstop, defaultReconcileBackstop)
		}
	})
	t.Run("explicit fields survive defaulting", func(t *testing.T) {
		in := ForgeConfig{Host: "ghe.example.com", SecretName: "TOK", App: ForgeAppConfig{ReconcileBackstop: 3 * time.Minute}}
		got := in.resolved()
		if got.Host != "ghe.example.com" || got.SecretName != "TOK" || got.App.ReconcileBackstop != 3*time.Minute {
			t.Fatalf("explicit fields clobbered by defaulting: %+v", got)
		}
	})
}

// TestForgeReviewerSecretDefaultingAndWritesEnabled pins the T8 write-path
// enablement contract (Matt's 2026-08-19 ruling): the reviewer secret name
// defaults to defaultForgeReviewerSecretName, an explicit one survives, and the
// write path is enabled iff BOTH the author and reviewer secrets are declared —
// independent of the board lane's boardIngestionEnabled gate.
func TestForgeReviewerSecretDefaultingAndWritesEnabled(t *testing.T) {
	t.Run("reviewer secret defaulted to the F1 default name", func(t *testing.T) {
		if got := (ForgeConfig{}).resolved().ReviewerSecretName; got != defaultForgeReviewerSecretName {
			t.Fatalf("ReviewerSecretName = %q, want %q", got, defaultForgeReviewerSecretName)
		}
	})
	t.Run("explicit reviewer secret survives defaulting", func(t *testing.T) {
		if got := (ForgeConfig{ReviewerSecretName: "REV_TOK"}).resolved().ReviewerSecretName; got != "REV_TOK" {
			t.Fatalf("ReviewerSecretName = %q, want the explicit REV_TOK", got)
		}
	})
	t.Run("both defaulted secrets declared -> writes enabled", func(t *testing.T) {
		declared := []secrets.ResolvedSecret{
			{Name: defaultForgeSecretName}, {Name: defaultForgeReviewerSecretName},
		}
		if !(ForgeConfig{}).forgeWritesEnabled(declared) {
			t.Fatal("both secrets declared should enable the write path")
		}
	})
	t.Run("only the author secret declared -> writes disabled", func(t *testing.T) {
		declared := []secrets.ResolvedSecret{{Name: defaultForgeSecretName}}
		if (ForgeConfig{}).forgeWritesEnabled(declared) {
			t.Fatal("author-only should NOT enable the write path (both required)")
		}
	})
	t.Run("only the reviewer secret declared -> writes disabled", func(t *testing.T) {
		declared := []secrets.ResolvedSecret{{Name: defaultForgeReviewerSecretName}}
		if (ForgeConfig{}).forgeWritesEnabled(declared) {
			t.Fatal("reviewer-only should NOT enable the write path (both required)")
		}
	})
	t.Run("neither declared -> writes disabled", func(t *testing.T) {
		if (ForgeConfig{}).forgeWritesEnabled(nil) {
			t.Fatal("no declared secrets should leave the write path disabled")
		}
	})
	t.Run("enablement honours explicit secret names, not just defaults", func(t *testing.T) {
		cfg := ForgeConfig{SecretName: "AUTHOR_TOK", ReviewerSecretName: "REVIEWER_TOK"}
		both := []secrets.ResolvedSecret{{Name: "AUTHOR_TOK"}, {Name: "REVIEWER_TOK"}}
		if !cfg.forgeWritesEnabled(both) {
			t.Fatal("explicit names both declared should enable the write path")
		}
		// The DEFAULT names being present must NOT enable a config that named
		// custom secrets — the predicate keys on the resolved config's names.
		defaults := []secrets.ResolvedSecret{{Name: defaultForgeSecretName}, {Name: defaultForgeReviewerSecretName}}
		if cfg.forgeWritesEnabled(defaults) {
			t.Fatal("default names must not satisfy a config that declared custom secret names")
		}
	})
	t.Run("write enablement is independent of the board ingestion gate", func(t *testing.T) {
		// boardIngestionEnabled is false (no App) yet writes are enabled on both
		// secrets — the two gates are orthogonal (Matt's ruling).
		cfg := ForgeConfig{}
		if cfg.boardIngestionEnabled() {
			t.Fatal("fixture precondition: board ingestion should be disabled")
		}
		declared := []secrets.ResolvedSecret{{Name: defaultForgeSecretName}, {Name: defaultForgeReviewerSecretName}}
		if !cfg.forgeWritesEnabled(declared) {
			t.Fatal("writes must enable on both secrets even with board ingestion disabled")
		}
	})
}

// TestBuildLinearNotifyLaneGate pins the RIG-2732 T7 Linear notify lane's
// App-INDEPENDENT gate: buildLinearNotifyLane runs iff LINEAR_FORGE_TOKEN is
// declared (the read credential the reconciler needs), NOT the GitHub App gate.
// The gate short-circuits before any store/hub touch, so a nil store + nil hub
// suffice; the declared path binds the Linear coordinate
// (store.ForgeProviderLinear / "linear.app"). A resolve fault fails fast.
func TestBuildLinearNotifyLaneGate(t *testing.T) {
	ctx := context.Background() // test root
	t.Run("undeclared LINEAR_FORGE_TOKEN -> nil lane (off-state)", func(t *testing.T) {
		lane, err := buildLinearNotifyLane(ctx, nil, nil, &fakeResolver{}, nil)
		if err != nil {
			t.Fatalf("buildLinearNotifyLane (undeclared): %v", err)
		}
		if lane != nil {
			t.Fatal("lane != nil with LINEAR_FORGE_TOKEN undeclared, want nil (lane off)")
		}
	})
	t.Run("declared LINEAR_FORGE_TOKEN -> non-nil lane with a sink", func(t *testing.T) {
		res := &fakeResolver{resolved: []secrets.ResolvedSecret{{Name: defaultForgeLinearSecretName, Value: "lin-tok"}}}
		lane, err := buildLinearNotifyLane(ctx, nil, nil, res, nil)
		if err != nil {
			t.Fatalf("buildLinearNotifyLane (declared): %v", err)
		}
		if lane == nil {
			t.Fatal("lane == nil with LINEAR_FORGE_TOKEN declared, want a non-nil lane")
		}
		if lane.arm == nil || lane.reconciler == nil || lane.sink == nil {
			t.Fatalf("assembled lane has a nil member: %+v", lane)
		}
	})
	t.Run("resolve fault -> error (fail-fast)", func(t *testing.T) {
		res := &fakeResolver{err: errors.New("boom")}
		lane, err := buildLinearNotifyLane(ctx, nil, nil, res, nil)
		if err == nil {
			t.Fatal("buildLinearNotifyLane returned nil error on a resolve fault, want fail-fast")
		}
		if lane != nil {
			t.Fatalf("lane = %+v on a resolve fault, want nil", lane)
		}
	})
}

func TestForgeTokenSourceCachesUntilTTL(t *testing.T) {
	res := &fakeResolver{resolved: []secrets.ResolvedSecret{{Name: "GITHUB_FORGE_TOKEN", Value: "tok-1"}}}
	ts := newForgeTokenSource(res, "GITHUB_FORGE_TOKEN")

	// A fixed clock the test advances by hand — no real sleeps.
	now := time.Unix(0, 0)
	ts.now = func() time.Time { return now }
	ts.ttl = time.Minute

	ctx := context.Background() // test root
	tok, err := ts.Token(ctx)
	if err != nil || tok != "tok-1" {
		t.Fatalf("first Token = %q, %v; want tok-1, nil", tok, err)
	}
	if res.calls != 1 {
		t.Fatalf("resolve calls = %d after first Token, want 1", res.calls)
	}

	// Rotate the resolver's value; within the TTL the cache still serves the OLD
	// value and does NOT re-resolve — the whole point of the TTL cache.
	res.resolved[0].Value = "tok-2"
	now = now.Add(30 * time.Second)
	tok, err = ts.Token(ctx)
	if err != nil || tok != "tok-1" {
		t.Fatalf("within-TTL Token = %q, %v; want cached tok-1, nil", tok, err)
	}
	if res.calls != 1 {
		t.Fatalf("resolve calls = %d within TTL, want still 1 (cache hit)", res.calls)
	}

	// Cross the TTL: the next Token re-resolves and the rotated value takes over.
	now = now.Add(time.Minute)
	tok, err = ts.Token(ctx)
	if err != nil || tok != "tok-2" {
		t.Fatalf("post-TTL Token = %q, %v; want re-resolved tok-2, nil", tok, err)
	}
	if res.calls != 2 {
		t.Fatalf("resolve calls = %d post-TTL, want 2 (re-resolve)", res.calls)
	}
}

func TestForgeTokenSourceInvalidateDropsCache(t *testing.T) {
	res := &fakeResolver{resolved: []secrets.ResolvedSecret{{Name: "GITHUB_FORGE_TOKEN", Value: "tok-1"}}}
	ts := newForgeTokenSource(res, "GITHUB_FORGE_TOKEN")
	now := time.Unix(0, 0)
	ts.now = func() time.Time { return now }
	ts.ttl = time.Hour // long TTL, so only Invalidate can force a re-resolve

	ctx := context.Background() // test root
	if tok, err := ts.Token(ctx); err != nil || tok != "tok-1" {
		t.Fatalf("first Token = %q, %v; want tok-1, nil", tok, err)
	}

	// A rotation the driver learns about via an auth failure (client calls
	// Invalidate). Within the TTL, only Invalidate drops the cache so the next
	// Token re-resolves and picks up the changed value.
	res.resolved[0].Value = "tok-2"
	ts.Invalidate()
	tok, err := ts.Token(ctx)
	if err != nil || tok != "tok-2" {
		t.Fatalf("post-Invalidate Token = %q, %v; want re-resolved tok-2, nil", tok, err)
	}
	if res.calls != 2 {
		t.Fatalf("resolve calls = %d, want 2 (initial + post-Invalidate)", res.calls)
	}
}

func TestForgeTokenSourceMissingNameErrors(t *testing.T) {
	res := &fakeResolver{resolved: []secrets.ResolvedSecret{{Name: "OTHER", Value: "x"}}}
	ts := newForgeTokenSource(res, "GITHUB_FORGE_TOKEN")
	if _, err := ts.Token(context.Background()); err == nil {
		t.Fatal("Token with the configured name absent = nil error, want a not-declared error")
	}
}

// TestValidateForgeSecretDistinctErrors pins record test 7's core property: the
// two startup failure modes have DISTINCT, distinguishable error text so a
// permanent misconfig (name absent) is not confused with a transient outage
// (the resolve call itself errors). validateForgeSecret is the pure gate the
// Serve fail-fast path calls; the listener-cleanup wiring is proven in pgtest.
func TestValidateForgeSecretDistinctErrors(t *testing.T) {
	ctx := context.Background() // test root

	t.Run("name absent -> not declared", func(t *testing.T) {
		res := &fakeResolver{resolved: []secrets.ResolvedSecret{{Name: "OTHER", Value: "x"}}}
		err := validateForgeSecret(ctx, res, "forge write", "GITHUB_FORGE_TOKEN")
		if err == nil {
			t.Fatal("validateForgeSecret with the name absent = nil, want an error")
		}
		if !strings.Contains(err.Error(), "not declared") {
			t.Fatalf("error %q should say the secret is not declared", err.Error())
		}
	})

	t.Run("resolve errors -> resolve failed at startup", func(t *testing.T) {
		sentinel := errors.New("provider unreachable")
		res := &fakeResolver{err: sentinel}
		err := validateForgeSecret(ctx, res, "forge write", "GITHUB_FORGE_TOKEN")
		if err == nil {
			t.Fatal("validateForgeSecret with a resolve error = nil, want an error")
		}
		if !strings.Contains(err.Error(), "resolve failed at startup") {
			t.Fatalf("error %q should say the resolve failed at startup", err.Error())
		}
		if !errors.Is(err, sentinel) {
			t.Fatalf("error %q should wrap the underlying resolve error", err.Error())
		}
	})

	t.Run("the two texts are distinguishable", func(t *testing.T) {
		absent := validateForgeSecret(ctx,
			&fakeResolver{resolved: []secrets.ResolvedSecret{{Name: "OTHER"}}}, "forge write", "GITHUB_FORGE_TOKEN")
		failed := validateForgeSecret(ctx, &fakeResolver{err: errors.New("boom")}, "forge write", "GITHUB_FORGE_TOKEN")
		if absent.Error() == failed.Error() {
			t.Fatal("the not-declared and resolve-failed texts must differ so a crash-loop is diagnosable")
		}
	})

	t.Run("name present -> nil", func(t *testing.T) {
		res := &fakeResolver{resolved: []secrets.ResolvedSecret{{Name: "GITHUB_FORGE_TOKEN", Value: "x"}}}
		if err := validateForgeSecret(ctx, res, "forge write", "GITHUB_FORGE_TOKEN"); err != nil {
			t.Fatalf("validateForgeSecret with the name present = %v, want nil", err)
		}
	})
}

// capWarnHandler is a DB-free slog.Handler that counts Warn records and keeps
// the last one's attributes, for the partial-misconfig warn assertions. Kept
// local to this unix (no-pgtest) file — the warn path under test is a pure
// function of config + declared secrets, no store needed.
type capWarnHandler struct {
	warns    int
	lastAttr map[string]string
}

func (h *capWarnHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capWarnHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		h.warns++
		h.lastAttr = map[string]string{}
		r.Attrs(func(a slog.Attr) bool {
			h.lastAttr[a.Key] = a.Value.String()
			return true
		})
	}
	return nil
}
func (h *capWarnHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capWarnHandler) WithGroup(string) slog.Handler      { return h }

// TestWarnPartialForgeWriteSecrets pins the observability contract for a partial
// write-secret misconfiguration: exactly one of the two required secrets
// declared emits ONE Warn naming the declared and the missing secret, while the
// intentional both-absent OFF state and the both-present ENABLED state stay
// silent. Guards against a silent hard-outage on an operator typo in one of the
// two env-var names.
func TestWarnPartialForgeWriteSecrets(t *testing.T) {
	t.Run("author-only declared -> one Warn naming declared+missing", func(t *testing.T) {
		h := &capWarnHandler{}
		declared := []secrets.ResolvedSecret{{Name: defaultForgeSecretName}}
		warnPartialForgeWriteSecrets(ForgeConfig{}, declared, slog.New(h))
		if h.warns != 1 {
			t.Fatalf("Warn count = %d, want exactly 1 on a partial (author-only) misconfig", h.warns)
		}
		if h.lastAttr["declared"] != defaultForgeSecretName {
			t.Fatalf("declared attr = %q, want %q", h.lastAttr["declared"], defaultForgeSecretName)
		}
		if h.lastAttr["missing"] != defaultForgeReviewerSecretName {
			t.Fatalf("missing attr = %q, want %q", h.lastAttr["missing"], defaultForgeReviewerSecretName)
		}
	})
	t.Run("reviewer-only declared -> one Warn naming declared+missing", func(t *testing.T) {
		h := &capWarnHandler{}
		declared := []secrets.ResolvedSecret{{Name: defaultForgeReviewerSecretName}}
		warnPartialForgeWriteSecrets(ForgeConfig{}, declared, slog.New(h))
		if h.warns != 1 {
			t.Fatalf("Warn count = %d, want exactly 1 on a partial (reviewer-only) misconfig", h.warns)
		}
		if h.lastAttr["declared"] != defaultForgeReviewerSecretName {
			t.Fatalf("declared attr = %q, want %q", h.lastAttr["declared"], defaultForgeReviewerSecretName)
		}
		if h.lastAttr["missing"] != defaultForgeSecretName {
			t.Fatalf("missing attr = %q, want %q", h.lastAttr["missing"], defaultForgeSecretName)
		}
	})
	t.Run("neither declared -> silent (intentional off)", func(t *testing.T) {
		h := &capWarnHandler{}
		warnPartialForgeWriteSecrets(ForgeConfig{}, nil, slog.New(h))
		if h.warns != 0 {
			t.Fatalf("Warn count = %d, want 0 for the intentional both-absent off state", h.warns)
		}
	})
	t.Run("both declared -> silent (enabled path warns nothing)", func(t *testing.T) {
		h := &capWarnHandler{}
		declared := []secrets.ResolvedSecret{{Name: defaultForgeSecretName}, {Name: defaultForgeReviewerSecretName}}
		warnPartialForgeWriteSecrets(ForgeConfig{}, declared, slog.New(h))
		if h.warns != 0 {
			t.Fatalf("Warn count = %d, want 0 when both secrets are declared", h.warns)
		}
	})
}

// TestNormalizeGitHubRepo covers the seed-boundary normalization/validation the
// server-side reconcile applies (the CLI does the same at parse time; both must
// agree so a row is keyed identically regardless of entry point).
func TestNormalizeGitHubRepo(t *testing.T) {
	ok := []struct{ in, want string }{
		{"owner/name", "owner/name"},
		{"Owner/Name", "owner/name"},
		{"  owner/name  ", "owner/name"},
		{"OWNER/NAME", "owner/name"},
	}
	for _, tc := range ok {
		got, err := normalizeGitHubRepo(tc.in)
		if err != nil {
			t.Fatalf("normalizeGitHubRepo(%q) = error %v, want %q", tc.in, err, tc.want)
		}
		if got != tc.want {
			t.Fatalf("normalizeGitHubRepo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "ownername", "/name", "owner/", "a/b/c"} {
		if _, err := normalizeGitHubRepo(bad); err == nil {
			t.Fatalf("normalizeGitHubRepo(%q) = nil error, want a validation error", bad)
		}
	}
}

// TestCachedWebhookSecretCachesUntilTTL pins the medium-severity fix: the
// webhook-secret resolver runs on every request to the public, unauthenticated
// /webhooks/github BEFORE the HMAC check, so it MUST NOT re-resolve (a full
// secretspec provider Load) per request. Within the TTL a garbage flood costs at
// most one resolve; a rotated secret still takes over after the TTL.
func TestCachedWebhookSecretCachesUntilTTL(t *testing.T) {
	res := &fakeResolver{resolved: []secrets.ResolvedSecret{{Name: "WEBHOOK_SECRET", Value: "sec-1"}}}
	c := &cachedWebhookSecret{base: newDeclaredSecretResolver(res, "WEBHOOK_SECRET"), ttl: time.Minute}
	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }

	ctx := context.Background() // test root
	got, err := c.get(ctx)
	if err != nil || string(got) != "sec-1" {
		t.Fatalf("first get = %q, %v; want sec-1, nil", got, err)
	}
	if res.calls != 1 {
		t.Fatalf("resolve calls = %d after first get, want 1", res.calls)
	}

	// Rotate the value; within the TTL the cache serves the OLD bytes and does
	// NOT re-resolve — the amplification the fix closes.
	res.resolved[0].Value = "sec-2"
	now = now.Add(30 * time.Second)
	got, err = c.get(ctx)
	if err != nil || string(got) != "sec-1" {
		t.Fatalf("within-TTL get = %q, %v; want cached sec-1, nil", got, err)
	}
	if res.calls != 1 {
		t.Fatalf("resolve calls = %d within TTL, want still 1 (cache hit)", res.calls)
	}

	// Cross the TTL: the next get re-resolves and the rotated value takes over.
	now = now.Add(time.Minute)
	got, err = c.get(ctx)
	if err != nil || string(got) != "sec-2" {
		t.Fatalf("post-TTL get = %q, %v; want re-resolved sec-2, nil", got, err)
	}
	if res.calls != 2 {
		t.Fatalf("resolve calls = %d post-TTL, want 2 (re-resolve)", res.calls)
	}
}

// TestCachedWebhookSecretDoesNotCacheErrors pins fail-closed behavior: a resolve
// fault is never cached, so the ingress 503s and the next request retries rather
// than serving a stale/absent secret.
func TestCachedWebhookSecretDoesNotCacheErrors(t *testing.T) {
	res := &fakeResolver{err: errors.New("provider down")}
	c := &cachedWebhookSecret{base: newDeclaredSecretResolver(res, "WEBHOOK_SECRET"), ttl: time.Hour, now: time.Now}

	ctx := context.Background() // test root
	if _, err := c.get(ctx); err == nil {
		t.Fatal("get with a failing resolver = nil error, want the resolve fault")
	}
	// A second call within the (long) TTL must re-resolve, not serve a cached
	// error — the error path leaves the cache invalid.
	if _, err := c.get(ctx); err == nil {
		t.Fatal("second get still failing = nil error, want the resolve fault")
	}
	if res.calls != 2 {
		t.Fatalf("resolve calls = %d, want 2 (errors are never cached)", res.calls)
	}
}
