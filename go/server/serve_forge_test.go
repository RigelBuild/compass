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

	"github.com/RigelBuild/compass/go/internal/linearagent"
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

func (r *fakeResolver) Set(context.Context, string, string, string) error { return nil }
func (r *fakeResolver) Delete(context.Context, string) error              { return nil }

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
		if got.LinearClientIDSecretName != defaultForgeLinearClientIDSecretName {
			t.Fatalf("LinearClientIDSecretName = %q, want %q", got.LinearClientIDSecretName, defaultForgeLinearClientIDSecretName)
		}
		if got.LinearClientSecretName != defaultForgeLinearClientSecretName {
			t.Fatalf("LinearClientSecretName = %q, want %q", got.LinearClientSecretName, defaultForgeLinearClientSecretName)
		}
		if got.App.ReconcileBackstop != defaultReconcileBackstop {
			t.Fatalf("ReconcileBackstop = %v, want %v", got.App.ReconcileBackstop, defaultReconcileBackstop)
		}
	})
	t.Run("explicit fields survive defaulting", func(t *testing.T) {
		in := ForgeConfig{Host: "ghe.example.com", LinearClientIDSecretName: "LID", App: ForgeAppConfig{ReconcileBackstop: 3 * time.Minute}}
		got := in.resolved()
		if got.Host != "ghe.example.com" || got.LinearClientIDSecretName != "LID" || got.App.ReconcileBackstop != 3*time.Minute {
			t.Fatalf("explicit fields clobbered by defaulting: %+v", got)
		}
	})
}

// TestForgeWriteAppsGate pins the 2-App write-path enablement contract
// (DEC-1/DEC-3): the forge-WRITE path is enabled iff BOTH the primary App and
// the reviewer App are configured — each AppID != 0 AND its private-key secret
// declared. Requiring the primary App force-couples writes to board ingestion
// (both key on App.AppID) — the unified shape Matt wants.
func TestForgeWriteAppsGate(t *testing.T) {
	// bothApps is a config with the primary + reviewer Apps configured; declared
	// carries both App key secrets.
	bothApps := ForgeConfig{
		App:         ForgeAppConfig{AppID: 1, InstallationID: 2, AppPrivateKeySecret: "PRIMARY_KEY", AppWebhookSecretName: "WH"},
		ReviewerApp: ForgeAppConfig{AppID: 3, InstallationID: 4, AppPrivateKeySecret: "REVIEWER_KEY"},
	}
	bothDeclared := []secrets.ResolvedSecret{{Name: "PRIMARY_KEY"}, {Name: "REVIEWER_KEY"}}

	t.Run("both Apps configured + both keys declared -> writes enabled", func(t *testing.T) {
		if !bothApps.forgeWritesEnabled(bothDeclared) {
			t.Fatal("both Apps configured with both key secrets declared should enable the write path")
		}
	})
	t.Run("primary App only -> writes disabled", func(t *testing.T) {
		cfg := ForgeConfig{App: bothApps.App}
		if cfg.forgeWritesEnabled(bothDeclared) {
			t.Fatal("primary-App-only should NOT enable the write path (both Apps required)")
		}
	})
	t.Run("reviewer App only -> writes disabled", func(t *testing.T) {
		cfg := ForgeConfig{ReviewerApp: bothApps.ReviewerApp}
		if cfg.forgeWritesEnabled(bothDeclared) {
			t.Fatal("reviewer-App-only should NOT enable the write path (both Apps required)")
		}
	})
	t.Run("both App ids set but a key secret undeclared -> writes disabled", func(t *testing.T) {
		// The reviewer key is missing from the declared set: configured means
		// AppID != 0 AND key declared, so this is a partial (disabled) state.
		onlyPrimaryKey := []secrets.ResolvedSecret{{Name: "PRIMARY_KEY"}}
		if bothApps.forgeWritesEnabled(onlyPrimaryKey) {
			t.Fatal("a configured reviewer App with its key undeclared must NOT enable writes")
		}
	})
	t.Run("neither App configured -> writes disabled", func(t *testing.T) {
		if (ForgeConfig{}).forgeWritesEnabled(nil) {
			t.Fatal("no Apps configured should leave the write path disabled")
		}
	})
	t.Run("enabling writes force-enables board ingestion (unified shape)", func(t *testing.T) {
		// Both Apps configured -> writes enabled AND boardIngestionEnabled true
		// (both key on App.AppID). The 2026-08-19 independent-gates ruling is
		// amended (DL-305): writes now require the primary App.
		if !bothApps.forgeWritesEnabled(bothDeclared) {
			t.Fatal("fixture precondition: writes should be enabled")
		}
		if !bothApps.boardIngestionEnabled() {
			t.Fatal("enabling writes must force board ingestion on (primary App configured)")
		}
	})
}

// TestBuildLinearNotifyLaneGate pins the Linear notify lane's gate: it is built
// iff the caller passes a non-nil shared Linear token source (Linear configured);
// a nil source is the off-state (lane nil). The gate short-circuits before any
// store/hub touch, so a nil store + nil hub suffice; the built lane binds the
// Linear coordinate (store.ForgeProviderLinear / "linear.app").
func TestBuildLinearNotifyLaneGate(t *testing.T) {
	t.Run("nil token source -> nil lane (off-state)", func(t *testing.T) {
		if lane := buildLinearNotifyLane(nil, nil, nil, nil); lane != nil {
			t.Fatal("lane != nil with a nil Linear token source, want nil (lane off)")
		}
	})
	t.Run("non-nil token source -> non-nil lane with a sink", func(t *testing.T) {
		tokens := linearagent.NewTokenSource("cid", "csecret", nil, "")
		lane := buildLinearNotifyLane(nil, nil, tokens, nil)
		if lane == nil {
			t.Fatal("lane == nil with a configured Linear token source, want a non-nil lane")
		}
		if lane.arm == nil || lane.reconciler == nil || lane.sink == nil {
			t.Fatalf("assembled lane has a nil member: %+v", lane)
		}
	})
}

// TestBuildLinearTokenSourceGate pins buildLinearTokenSource's pre-mint gating —
// the branches that decide WHETHER a Linear token source is built, before any
// network mint (the mint itself is covered by linearagent's TokenSource tests).
// Neither client-cred secret declared is the clean off-state (nil, nil); a
// resolve fault fails fast; exactly one of the pair declared is a likely
// operator typo, surfaced by ONE Warn naming the declared + missing secret and
// treated as off (nil, nil) — never a fatal.
func TestBuildLinearTokenSourceGate(t *testing.T) {
	ctx := context.Background() // test root
	cfg := ServeConfig{}        // Forge zero -> resolved() defaults the two client-cred names.
	idName, secretName := defaultForgeLinearClientIDSecretName, defaultForgeLinearClientSecretName

	t.Run("neither secret declared -> nil source (off-state), no Warn", func(t *testing.T) {
		h := &capWarnHandler{}
		tokens, err := buildLinearTokenSource(ctx, cfg, &fakeResolver{}, slog.New(h))
		if err != nil {
			t.Fatalf("buildLinearTokenSource (neither declared): %v", err)
		}
		if tokens != nil {
			t.Fatal("token source != nil with neither Linear secret declared, want nil (off)")
		}
		if h.warns != 0 {
			t.Fatalf("Warn count = %d, want 0 for the intentional both-absent off state", h.warns)
		}
	})

	t.Run("resolve fault -> error (fail-fast)", func(t *testing.T) {
		tokens, err := buildLinearTokenSource(ctx, cfg, &fakeResolver{err: errors.New("boom")}, slog.Default())
		if err == nil {
			t.Fatal("buildLinearTokenSource on a resolve fault = nil error, want fail-fast")
		}
		if tokens != nil {
			t.Fatalf("token source = %v on a resolve fault, want nil", tokens)
		}
	})

	t.Run("only the client id declared -> nil source + one Warn naming declared+missing", func(t *testing.T) {
		h := &capWarnHandler{}
		res := &fakeResolver{resolved: []secrets.ResolvedSecret{{Name: idName, Value: "cid"}}}
		tokens, err := buildLinearTokenSource(ctx, cfg, res, slog.New(h))
		if err != nil {
			t.Fatalf("buildLinearTokenSource (partial): %v", err)
		}
		if tokens != nil {
			t.Fatal("token source != nil with only the client id declared, want nil (partial -> off)")
		}
		if h.warns != 1 {
			t.Fatalf("Warn count = %d, want exactly 1 on a partial (id-only) misconfig", h.warns)
		}
		if h.lastAttr["declared"] != idName {
			t.Fatalf("declared attr = %q, want %q", h.lastAttr["declared"], idName)
		}
		if h.lastAttr["missing"] != secretName {
			t.Fatalf("missing attr = %q, want %q", h.lastAttr["missing"], secretName)
		}
	})

	t.Run("only the client secret declared -> nil source + one Warn naming declared+missing", func(t *testing.T) {
		h := &capWarnHandler{}
		res := &fakeResolver{resolved: []secrets.ResolvedSecret{{Name: secretName, Value: "csecret"}}}
		tokens, err := buildLinearTokenSource(ctx, cfg, res, slog.New(h))
		if err != nil {
			t.Fatalf("buildLinearTokenSource (partial): %v", err)
		}
		if tokens != nil {
			t.Fatal("token source != nil with only the client secret declared, want nil (partial -> off)")
		}
		if h.warns != 1 {
			t.Fatalf("Warn count = %d, want exactly 1 on a partial (secret-only) misconfig", h.warns)
		}
		if h.lastAttr["declared"] != secretName {
			t.Fatalf("declared attr = %q, want %q", h.lastAttr["declared"], secretName)
		}
		if h.lastAttr["missing"] != idName {
			t.Fatalf("missing attr = %q, want %q", h.lastAttr["missing"], idName)
		}
	})
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
		err := validateForgeSecret(ctx, res, "forge write", "APP_KEY")
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
		err := validateForgeSecret(ctx, res, "forge write", "APP_KEY")
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
			&fakeResolver{resolved: []secrets.ResolvedSecret{{Name: "OTHER"}}}, "forge write", "APP_KEY")
		failed := validateForgeSecret(ctx, &fakeResolver{err: errors.New("boom")}, "forge write", "APP_KEY")
		if absent.Error() == failed.Error() {
			t.Fatal("the not-declared and resolve-failed texts must differ so a crash-loop is diagnosable")
		}
	})

	t.Run("name present -> nil", func(t *testing.T) {
		res := &fakeResolver{resolved: []secrets.ResolvedSecret{{Name: "APP_KEY", Value: "x"}}}
		if err := validateForgeSecret(ctx, res, "forge write", "APP_KEY"); err != nil {
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
// write-App misconfiguration: exactly ONE of the two required Apps configured
// emits ONE Warn naming the configured and the missing role, while the
// intentional both-absent OFF state and the both-present ENABLED state stay
// silent. Guards against a silent hard-outage on an operator typo (a missing App
// id or an undeclared App key).
func TestWarnPartialForgeWriteSecrets(t *testing.T) {
	primary := ForgeAppConfig{AppID: 1, InstallationID: 2, AppPrivateKeySecret: "PRIMARY_KEY"}
	reviewer := ForgeAppConfig{AppID: 3, InstallationID: 4, AppPrivateKeySecret: "REVIEWER_KEY"}
	primaryDeclared := []secrets.ResolvedSecret{{Name: "PRIMARY_KEY"}}
	reviewerDeclared := []secrets.ResolvedSecret{{Name: "REVIEWER_KEY"}}
	bothDeclared := []secrets.ResolvedSecret{{Name: "PRIMARY_KEY"}, {Name: "REVIEWER_KEY"}}

	t.Run("primary-App-only -> one Warn naming configured+missing", func(t *testing.T) {
		h := &capWarnHandler{}
		warnPartialForgeWriteSecrets(ForgeConfig{App: primary}, primaryDeclared, slog.New(h))
		if h.warns != 1 {
			t.Fatalf("Warn count = %d, want exactly 1 on a partial (primary-App-only) misconfig", h.warns)
		}
		if h.lastAttr["configured"] != "primary App" {
			t.Fatalf("configured attr = %q, want %q", h.lastAttr["configured"], "primary App")
		}
		if h.lastAttr["missing"] != "reviewer App" {
			t.Fatalf("missing attr = %q, want %q", h.lastAttr["missing"], "reviewer App")
		}
	})
	t.Run("reviewer-App-only -> one Warn naming configured+missing", func(t *testing.T) {
		h := &capWarnHandler{}
		warnPartialForgeWriteSecrets(ForgeConfig{ReviewerApp: reviewer}, reviewerDeclared, slog.New(h))
		if h.warns != 1 {
			t.Fatalf("Warn count = %d, want exactly 1 on a partial (reviewer-App-only) misconfig", h.warns)
		}
		if h.lastAttr["configured"] != "reviewer App" {
			t.Fatalf("configured attr = %q, want %q", h.lastAttr["configured"], "reviewer App")
		}
		if h.lastAttr["missing"] != "primary App" {
			t.Fatalf("missing attr = %q, want %q", h.lastAttr["missing"], "primary App")
		}
	})
	t.Run("neither configured -> silent (intentional off)", func(t *testing.T) {
		h := &capWarnHandler{}
		warnPartialForgeWriteSecrets(ForgeConfig{}, nil, slog.New(h))
		if h.warns != 0 {
			t.Fatalf("Warn count = %d, want 0 for the intentional both-absent off state", h.warns)
		}
	})
	t.Run("both configured -> silent (enabled path warns nothing)", func(t *testing.T) {
		h := &capWarnHandler{}
		warnPartialForgeWriteSecrets(ForgeConfig{App: primary, ReviewerApp: reviewer}, bothDeclared, slog.New(h))
		if h.warns != 0 {
			t.Fatalf("Warn count = %d, want 0 when both Apps are configured", h.warns)
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
