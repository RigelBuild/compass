//go:build unix

package server

// DB-free unit tests for the SEA-1810 forge wiring that need no Postgres: the
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
	"strings"
	"testing"
	"time"

	"github.com/sealedsecurity/compass/go/internal/secrets"
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
	t.Run("disabled by default", func(t *testing.T) {
		if (ForgeConfig{}).forgePollingEnabled() {
			t.Fatal("empty ForgeConfig should be polling-disabled")
		}
	})
	t.Run("enabled by a non-empty seed", func(t *testing.T) {
		if !(ForgeConfig{SeedRepos: []string{"a/b"}}).forgePollingEnabled() {
			t.Fatal("a non-empty seed should enable polling")
		}
	})
	t.Run("enabled by the poll flag", func(t *testing.T) {
		if !(ForgeConfig{Poll: true}).forgePollingEnabled() {
			t.Fatal("Poll=true should enable polling")
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
		if got.PollInterval != defaultForgePollInterval {
			t.Fatalf("PollInterval = %v, want %v", got.PollInterval, defaultForgePollInterval)
		}
	})
	t.Run("explicit fields survive defaulting", func(t *testing.T) {
		in := ForgeConfig{Host: "ghe.example.com", SecretName: "TOK", PollInterval: 3 * time.Minute}
		got := in.resolved()
		if got.Host != "ghe.example.com" || got.SecretName != "TOK" || got.PollInterval != 3*time.Minute {
			t.Fatalf("explicit fields clobbered by defaulting: %+v", got)
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
		err := validateForgeSecret(ctx, res, "GITHUB_FORGE_TOKEN")
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
		err := validateForgeSecret(ctx, res, "GITHUB_FORGE_TOKEN")
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
			&fakeResolver{resolved: []secrets.ResolvedSecret{{Name: "OTHER"}}}, "GITHUB_FORGE_TOKEN")
		failed := validateForgeSecret(ctx, &fakeResolver{err: errors.New("boom")}, "GITHUB_FORGE_TOKEN")
		if absent.Error() == failed.Error() {
			t.Fatal("the not-declared and resolve-failed texts must differ so a crash-loop is diagnosable")
		}
	})

	t.Run("name present -> nil", func(t *testing.T) {
		res := &fakeResolver{resolved: []secrets.ResolvedSecret{{Name: "GITHUB_FORGE_TOKEN", Value: "x"}}}
		if err := validateForgeSecret(ctx, res, "GITHUB_FORGE_TOKEN"); err != nil {
			t.Fatalf("validateForgeSecret with the name present = %v, want nil", err)
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
