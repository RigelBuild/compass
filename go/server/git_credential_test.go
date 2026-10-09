//go:build unix

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
)

type fakeGitCredentialGrants struct {
	repos []string
	err   error
}

func (g *fakeGitCredentialGrants) ListForgeScopeRepos(context.Context, store.AccountID, store.ForgeProvider, string) ([]string, error) {
	return append([]string(nil), g.repos...), g.err
}

type fakeGitCredentialMinter struct {
	tokens    []string
	expiresAt time.Time
	err       error
	calls     int
	repos     [][]string
	perms     []map[string]string
}

func (m *fakeGitCredentialMinter) Mint(_ context.Context, repos []string, perms map[string]string) (string, time.Time, error) {
	m.calls++
	m.repos = append(m.repos, append([]string(nil), repos...))
	copyPerms := make(map[string]string, len(perms))
	maps.Copy(copyPerms, perms)
	m.perms = append(m.perms, copyPerms)
	if m.err != nil {
		return "", time.Time{}, m.err
	}
	index := min(m.calls-1, len(m.tokens)-1)
	return m.tokens[index], m.expiresAt, nil
}

type fakeAgentSecretResolver struct {
	resolved []secrets.ResolvedSecret
	err      error
	calls    int
}

func (r *fakeAgentSecretResolver) ResolveFor(context.Context, store.AccountID, string) ([]secrets.ResolvedSecret, error) {
	r.calls++
	return append([]secrets.ResolvedSecret(nil), r.resolved...), r.err
}

func newGitCredentialTestBroker(grants gitCredentialGrantLister, minter gitCredentialMinter, now *time.Time, signal func() error) *gitCredentialBroker {
	return &gitCredentialBroker{
		host:    "github.com",
		grants:  grants,
		minter:  minter,
		log:     slog.New(slog.DiscardHandler),
		signal:  signal,
		clock:   func() time.Time { return *now },
		entries: make(map[string]gitCredentialEntry),
	}
}

func resolveBrokeredSecret(t *testing.T, resolver *brokeredSecretResolver) []secrets.ResolvedSecret {
	t.Helper()
	got, err := resolver.ResolveFor(context.Background(), "agent-id", "runner fetch")
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	return got
}

func TestGitCredentialBrokerMintsNarrowedGrant(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/zeta", "owner/alpha"}}
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_scoped"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	resolver := &brokeredSecretResolver{inner: &fakeAgentSecretResolver{}, broker: broker}

	got := resolveBrokeredSecret(t, resolver)
	if len(got) != 1 || got[0].Name != gitCredentialSecretName || got[0].Value != "ghs_scoped" || got[0].Kind != secrets.SecretGH || got[0].Host != "github.com" || got[0].Delivery != secrets.DeliveryFile {
		t.Fatalf("resolved secrets = %+v, want one file-delivered GitHub credential", got)
	}
	if want := []string{"alpha", "zeta"}; !reflect.DeepEqual(minter.repos[0], want) {
		t.Fatalf("mint repositories = %v, want %v", minter.repos[0], want)
	}
	if !reflect.DeepEqual(minter.perms[0], gitCredentialPermissions) {
		t.Fatalf("mint permissions = %v, want %v", minter.perms[0], gitCredentialPermissions)
	}
}

func TestGitCredentialBrokerWildcardMintsInstallationWide(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/repo", "*"}}
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_wildcard"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	resolver := &brokeredSecretResolver{inner: &fakeAgentSecretResolver{}, broker: broker}

	got := resolveBrokeredSecret(t, resolver)
	if len(got) != 1 || got[0].Value != "ghs_wildcard" {
		t.Fatalf("resolved secrets = %+v, want wildcard GitHub credential", got)
	}
	if len(minter.repos) != 1 || minter.repos[0] != nil {
		t.Fatalf("mint repositories = %v, want nil installation-wide scope", minter.repos)
	}
}

func TestGitCredentialBrokerDoesNotMintWithoutSafeGrants(t *testing.T) {
	tests := []struct {
		name  string
		repos []string
	}{
		{name: "no grants"},
		{name: "mixed owners", repos: []string{"one/repo", "two/other"}},
		{name: "malformed grant", repos: []string{"owner/repo/extra"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
			grants := &fakeGitCredentialGrants{repos: tt.repos}
			minter := &fakeGitCredentialMinter{tokens: []string{"must-not-mint"}, expiresAt: now.Add(time.Hour)}
			broker := newGitCredentialTestBroker(grants, minter, &now, nil)
			resolver := &brokeredSecretResolver{inner: &fakeAgentSecretResolver{}, broker: broker}

			if got := resolveBrokeredSecret(t, resolver); len(got) != 0 {
				t.Fatalf("resolved secrets = %+v, want none", got)
			}
			if minter.calls != 0 {
				t.Fatalf("mint calls = %d, want 0", minter.calls)
			}
		})
	}
}

func TestGitCredentialBrokerDoesNotMintOnGrantLookupError(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/repo"}, err: errors.New("store unavailable")}
	minter := &fakeGitCredentialMinter{tokens: []string{"must-not-mint"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	resolver := &brokeredSecretResolver{inner: &fakeAgentSecretResolver{}, broker: broker}

	if got := resolveBrokeredSecret(t, resolver); len(got) != 0 {
		t.Fatalf("resolved secrets = %+v, want none", got)
	}
	if minter.calls != 0 {
		t.Fatalf("mint calls = %d, want 0", minter.calls)
	}
}

func TestGitCredentialBrokerDoesNotMintOversizedGrant(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	repos := make([]string, gitCredentialMaxRepos+1)
	for i := range repos {
		repos[i] = fmt.Sprintf("owner/repo-%d", i)
	}
	grants := &fakeGitCredentialGrants{repos: repos}
	minter := &fakeGitCredentialMinter{tokens: []string{"must-not-mint"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	resolver := &brokeredSecretResolver{inner: &fakeAgentSecretResolver{}, broker: broker}

	if got := resolveBrokeredSecret(t, resolver); len(got) != 0 {
		t.Fatalf("resolved secrets = %+v, want none", got)
	}
	if minter.calls != 0 {
		t.Fatalf("mint calls = %d, want 0", minter.calls)
	}
}

func TestBrokeredSecretResolverExplicitGitHubSecretWins(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/repo"}}
	minter := &fakeGitCredentialMinter{tokens: []string{"must-not-mint"}, expiresAt: now.Add(time.Hour)}
	inner := &fakeAgentSecretResolver{resolved: []secrets.ResolvedSecret{{Name: "USER_GH", Value: "user-token", Kind: secrets.SecretGH, Host: "github.com"}}}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	resolver := &brokeredSecretResolver{inner: inner, broker: broker}

	got := resolveBrokeredSecret(t, resolver)
	if len(got) != 1 || got[0].Name != "USER_GH" || got[0].Value != "user-token" {
		t.Fatalf("resolved secrets = %+v, want the explicit user credential only", got)
	}
	if minter.calls != 0 {
		t.Fatalf("mint calls = %d, want 0", minter.calls)
	}
}

func TestBrokeredSecretResolverCachesCredential(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/repo"}}
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_cache"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	resolver := &brokeredSecretResolver{inner: &fakeAgentSecretResolver{}, broker: broker}

	for range 2 {
		if got := resolveBrokeredSecret(t, resolver); len(got) != 1 || got[0].Value != "ghs_cache" {
			t.Fatalf("resolved secrets = %+v, want cached credential", got)
		}
	}
	if minter.calls != 1 {
		t.Fatalf("mint calls = %d, want one cached mint", minter.calls)
	}
}

func TestGitCredentialBrokerRefreshSignalsOnce(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/alpha"}}
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_old", "ghs_new"}, expiresAt: now.Add(time.Hour)}
	signals := 0
	broker := newGitCredentialTestBroker(grants, minter, &now, func() error { signals++; return nil })
	resolver := &brokeredSecretResolver{inner: &fakeAgentSecretResolver{}, broker: broker}
	resolveBrokeredSecret(t, resolver)

	now = now.Add(time.Hour - 5*time.Minute)
	broker.refreshDue(context.Background())
	if minter.calls != 2 {
		t.Fatalf("mint calls = %d, want 2 after refresh boundary", minter.calls)
	}
	if signals != 1 {
		t.Fatalf("signal calls = %d, want exactly 1", signals)
	}
}

func TestGitCredentialBrokerRefreshFailureKeepsUnexpiredToken(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/repo"}}
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_old"}, expiresAt: now.Add(time.Hour)}
	signals := 0
	broker := newGitCredentialTestBroker(grants, minter, &now, func() error { signals++; return nil })
	resolver := &brokeredSecretResolver{inner: &fakeAgentSecretResolver{}, broker: broker}
	resolveBrokeredSecret(t, resolver)

	now = now.Add(time.Hour - 5*time.Minute)
	minter.err = errors.New("mint failed")
	got := resolveBrokeredSecret(t, resolver)
	if len(got) != 1 || got[0].Value != "ghs_old" {
		t.Fatalf("resolved secrets = %+v, want previous token until expiry", got)
	}
	if signals != 0 {
		t.Fatalf("signal calls = %d, want 0", signals)
	}

	now = now.Add(5 * time.Minute)
	if got := resolveBrokeredSecret(t, resolver); len(got) != 0 {
		t.Fatalf("resolved secrets = %+v, want expired token omitted", got)
	}
}

func TestGitCredentialBrokerPrunesUnusedEntries(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/repo"}}
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_prune"}, expiresAt: now.Add(2 * time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	resolver := &brokeredSecretResolver{inner: &fakeAgentSecretResolver{}, broker: broker}
	resolveBrokeredSecret(t, resolver)

	now = now.Add(gitCredentialMaxAge + time.Nanosecond)
	broker.refreshDue(context.Background())
	if len(broker.entries) != 0 {
		t.Fatalf("entry count = %d, want unused entry pruned", len(broker.entries))
	}
	if minter.calls != 1 {
		t.Fatalf("mint calls = %d, want no refresh during prune", minter.calls)
	}
}

func TestBrokeredSecretResolverNilBrokerPassesThrough(t *testing.T) {
	inner := &fakeAgentSecretResolver{resolved: []secrets.ResolvedSecret{{Name: "USER_SECRET", Value: "value"}}}
	resolver := &brokeredSecretResolver{inner: inner}
	got := resolveBrokeredSecret(t, resolver)
	if len(got) != 1 || got[0].Name != "USER_SECRET" {
		t.Fatalf("resolved secrets = %+v, want passthrough", got)
	}
}

func TestBrokeredSecretResolverOnlyMintsForRunnerFetch(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/repo"}}
	minter := &fakeGitCredentialMinter{tokens: []string{"must-not-mint"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	inner := &fakeAgentSecretResolver{}
	resolver := &brokeredSecretResolver{inner: inner, broker: broker}

	got, err := resolver.ResolveFor(context.Background(), "agent-id", "other secret consumer")
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	if len(got) != 0 || minter.calls != 0 {
		t.Fatalf("resolved=%v mint calls=%d, want no broker access", got, minter.calls)
	}
}

func TestGitCredentialBrokerSingleflightsConcurrentMints(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/repo"}}
	minter := &blockingGitCredentialMinter{
		started: make(chan struct{}), release: make(chan struct{}), expiresAt: now.Add(time.Hour),
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	const callers = 8
	results := make(chan string, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	start := make(chan struct{})
	for range callers {
		go func() {
			ready.Done()
			<-start
			tok, ok := broker.credential(context.Background(), "agent-id")
			if !ok {
				results <- ""
				return
			}
			results <- tok
		}()
	}
	ready.Wait()
	close(start)
	<-minter.started
	close(minter.release)
	for range callers {
		if token := <-results; token != "ghs_concurrent" {
			t.Fatalf("credential = %q, want shared token", token)
		}
	}
	if minter.callCount() != 1 {
		t.Fatalf("mint calls = %d, want 1", minter.callCount())
	}
}

type blockingGitCredentialMinter struct {
	started   chan struct{}
	release   chan struct{}
	expiresAt time.Time
	mu        sync.Mutex
	calls     int
}

func (m *blockingGitCredentialMinter) Mint(ctx context.Context, _ []string, _ map[string]string) (string, time.Time, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	select {
	case m.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return "", time.Time{}, ctx.Err()
	case <-m.release:
		return "ghs_concurrent", m.expiresAt, nil
	}
}

func (m *blockingGitCredentialMinter) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}
