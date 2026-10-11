//go:build unix

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
)

type fakeGitCredentialGrants struct {
	repos          []string
	reposByAccount map[store.AccountID][]string
	owners         map[store.AccountID]store.AccountID
	err            error
	ownerErr       error
	ownerCalls     int
	mu             sync.Mutex
	listed         chan struct{}
}

func (g *fakeGitCredentialGrants) ListForgeScopeRepos(_ context.Context, account store.AccountID, _ store.ForgeProvider, _ string) ([]string, error) {
	g.mu.Lock()
	repos, ok := g.reposByAccount[account]
	if !ok {
		repos = g.repos
	}
	repos, err := append([]string(nil), repos...), g.err
	listed := g.listed
	g.mu.Unlock()
	if listed != nil {
		select {
		case listed <- struct{}{}:
		default:
		}
	}
	return repos, err
}

func (g *fakeGitCredentialGrants) AgentOwner(_ context.Context, agent store.AccountID) (store.AccountID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ownerCalls++
	return g.owners[agent], g.ownerErr
}

func (g *fakeGitCredentialGrants) setRepos(account store.AccountID, repos []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reposByAccount == nil {
		g.reposByAccount = make(map[store.AccountID][]string)
	}
	g.reposByAccount[account] = append([]string(nil), repos...)
}

func (g *fakeGitCredentialGrants) ownerCallCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ownerCalls
}

type fakeGitCredentialMinter struct {
	tokens       []string
	expiresAt    time.Time
	err          error
	calls        int
	repos        [][]string
	grantedRepos []string
	perms        []map[string]string
	mu           sync.Mutex
	started      chan struct{}
	release      chan struct{}
	errByCall    []error
}

func (m *fakeGitCredentialMinter) Mint(ctx context.Context, repos []string, perms map[string]string) (forge.ScopedToken, error) {
	m.mu.Lock()
	m.calls++
	m.repos = append(m.repos, append([]string(nil), repos...))
	copyPerms := make(map[string]string, len(perms))
	maps.Copy(copyPerms, perms)
	callIndex := m.calls - 1
	m.perms = append(m.perms, copyPerms)
	index := min(callIndex, len(m.tokens)-1)
	token := m.tokens[index]
	expiresAt, granted, err, started, release := m.expiresAt, m.grantedRepos, m.err, m.started, m.release
	if callIndex < len(m.errByCall) && m.errByCall[callIndex] != nil {
		err = m.errByCall[callIndex]
	}
	m.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return forge.ScopedToken{}, ctx.Err()
		case <-release:
		}
	}
	if err != nil {
		return forge.ScopedToken{}, err
	}
	if granted == nil && len(repos) > 0 {
		granted = make([]string, len(repos))
		for i, repo := range repos {
			granted[i] = "owner/" + repo
		}
	}
	return forge.ScopedToken{Token: token, ExpiresAt: expiresAt, Repositories: granted}, nil
}

func (m *fakeGitCredentialMinter) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *fakeGitCredentialMinter) mintRepos() [][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	repos := make([][]string, len(m.repos))
	for i, repo := range m.repos {
		repos[i] = append([]string(nil), repo...)
	}
	return repos
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

func newGitCredentialTestBroker(credentialStore gitCredentialStore, minter gitCredentialMinter, now *time.Time, signal func() error) *gitCredentialBroker {
	return &gitCredentialBroker{
		host:     "github.com",
		store:    credentialStore,
		minter:   minter,
		log:      slog.New(slog.DiscardHandler),
		signal:   signal,
		clock:    func() time.Time { return *now },
		entries:  make(map[string]gitCredentialEntry),
		negative: make(map[string]gitCredentialNegativeEntry),
		last:     make(map[store.AccountID]string),
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

func TestGitCredentialBrokerAgentRepositoryWidensMintedSet(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{owners: map[store.AccountID]store.AccountID{"agent-id": "owner-id"}}
	grants.setRepos("owner-id", []string{"owner/base"})
	grants.setRepos("agent-id", []string{"owner/base", "owner/workstream"})
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_widened"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)

	tok, ok := broker.credential(context.Background(), "agent-id")
	if !ok || tok != "ghs_widened" {
		t.Fatalf("credential = (%q, %v), want widened token", tok, ok)
	}
	if got, want := minter.mintRepos()[0], []string{"base", "workstream"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mint repositories = %v, want %v", got, want)
	}
}

func TestGitCredentialBrokerCrossOwnerAgentRowFallsBackToOwnerGrants(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{owners: map[store.AccountID]store.AccountID{"agent-id": "owner-id"}}
	grants.setRepos("owner-id", []string{"owner/base"})
	grants.setRepos("agent-id", []string{"owner/base", "other/workstream"})
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_owner"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)

	tok, ok := broker.credential(context.Background(), "agent-id")
	if !ok || tok != "ghs_owner" {
		t.Fatalf("credential = (%q, %v), want owner-grants token", tok, ok)
	}
	if got, want := minter.mintRepos(), [][]string{{"base"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mint repository sets = %v, want %v", got, want)
	}
	if grants.ownerCallCount() != 1 {
		t.Fatalf("owner lookups = %d, want one", grants.ownerCallCount())
	}
}

func TestGitCredentialBrokerOversizedAgentScopeFallsBackToOwnerGrants(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{owners: map[store.AccountID]store.AccountID{"agent-id": "owner-id"}}
	grants.setRepos("owner-id", []string{"owner/base"})
	repos := make([]string, gitCredentialMaxRepos)
	for i := range repos {
		repos[i] = fmt.Sprintf("owner/agent-%d", i)
	}
	grants.setRepos("agent-id", append([]string{"owner/base"}, repos...))
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_owner"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)

	tok, ok := broker.credential(context.Background(), "agent-id")
	if !ok || tok != "ghs_owner" {
		t.Fatalf("credential = (%q, %v), want owner-grants token", tok, ok)
	}
	if got, want := minter.mintRepos(), [][]string{{"base"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mint repository sets = %v, want %v", got, want)
	}
}

func TestGitCredentialBrokerCachesScopeRejectedMintAndFallsBack(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{owners: map[store.AccountID]store.AccountID{"agent-id": "owner-id"}}
	grants.setRepos("owner-id", []string{"owner/base"})
	grants.setRepos("agent-id", []string{"owner/base", "owner/workstream"})
	minter := &fakeGitCredentialMinter{
		tokens: []string{"unused", "ghs_owner"}, expiresAt: now.Add(time.Hour),
		errByCall: []error{&forge.StatusError{Status: http.StatusUnprocessableEntity, Message: "scope rejected"}},
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)

	for range 2 {
		tok, ok := broker.credential(context.Background(), "agent-id")
		if !ok || tok != "ghs_owner" {
			t.Fatalf("credential = (%q, %v), want owner-grants token", tok, ok)
		}
	}
	if minter.callCount() != 2 {
		t.Fatalf("mint calls = %d, want one full-set mint and one owner mint", minter.callCount())
	}
	if got, want := minter.mintRepos(), [][]string{{"base", "workstream"}, {"base"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mint repository sets = %v, want %v", got, want)
	}
}

func TestGitCredentialBrokerRateLimitServesStaleFullScopeWithoutOwnerLookup(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{owners: map[store.AccountID]store.AccountID{"agent-id": "owner-id"}}
	grants.setRepos("owner-id", []string{"owner/base"})
	grants.setRepos("agent-id", []string{"owner/base", "owner/workstream"})
	minter := &fakeGitCredentialMinter{
		tokens: []string{"ghs_full"}, expiresAt: now.Add(time.Hour),
		errByCall: []error{nil, &forge.StatusError{Status: http.StatusTooManyRequests, Message: "try later"}},
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	if tok, ok := broker.credential(context.Background(), "agent-id"); !ok || tok != "ghs_full" {
		t.Fatalf("initial credential = (%q, %v), want full-set token", tok, ok)
	}
	now = now.Add(time.Hour - gitCredentialRefreshLead)
	if tok, ok := broker.credential(context.Background(), "agent-id"); !ok || tok != "ghs_full" {
		t.Fatalf("credential after 429 = (%q, %v), want stale full-set token", tok, ok)
	}
	if minter.callCount() != 2 {
		t.Fatalf("mint calls = %d, want initial mint and one failed refresh", minter.callCount())
	}
	if grants.ownerCallCount() != 0 {
		t.Fatalf("owner lookups = %d, want none after transient failure", grants.ownerCallCount())
	}
}

func TestGitCredentialBrokerOwnerWildcardDominatesAgentRows(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{owners: map[store.AccountID]store.AccountID{"agent-id": "owner-id"}}
	grants.setRepos("owner-id", []string{"*"})
	grants.setRepos("agent-id", []string{"*", "other/workstream"})
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_wildcard"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)

	if tok, ok := broker.credential(context.Background(), "agent-id"); !ok || tok != "ghs_wildcard" {
		t.Fatalf("credential = (%q, %v), want wildcard token", tok, ok)
	}
	if got := minter.mintRepos(); len(got) != 1 || got[0] != nil {
		t.Fatalf("mint repository sets = %v, want one installation-wide request", got)
	}
}

func TestGitCredentialBrokerOwnerLookupErrorStopsFallback(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{
		owners:   map[store.AccountID]store.AccountID{"agent-id": "owner-id"},
		ownerErr: errors.New("owner lookup failed"),
	}
	grants.setRepos("agent-id", []string{"owner/base", "other/workstream"})
	minter := &fakeGitCredentialMinter{tokens: []string{"must-not-mint"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)

	if tok, ok := broker.credential(context.Background(), "agent-id"); ok || tok != "" {
		t.Fatalf("credential = (%q, %v), want none after owner lookup error", tok, ok)
	}
	if minter.callCount() != 0 {
		t.Fatalf("mint calls = %d, want none", minter.callCount())
	}
}

func TestGitCredentialBrokerAgentRowsMintWithoutOwnerGrants(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{owners: map[store.AccountID]store.AccountID{"agent-id": "owner-id"}}
	grants.setRepos("owner-id", []string{})
	grants.setRepos("agent-id", []string{"owner/workstream"})
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_agent"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)

	if tok, ok := broker.credential(context.Background(), "agent-id"); !ok || tok != "ghs_agent" {
		t.Fatalf("credential = (%q, %v), want agent-row token", tok, ok)
	}
	if grants.ownerCallCount() != 0 {
		t.Fatalf("owner lookups = %d, want none for a valid agent scope", grants.ownerCallCount())
	}
}

func TestGitCredentialBrokerTransientWidenedMintServesLastOwnerToken(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{}
	grants.setRepos("owner-id", []string{"owner/base"})
	grants.setRepos("agent-id", []string{"owner/base"})
	minter := &fakeGitCredentialMinter{
		tokens: []string{"ghs_owner"}, expiresAt: now.Add(time.Hour),
		errByCall: []error{
			nil,
			&forge.StatusError{Status: http.StatusTooManyRequests, Message: "try later"},
			&forge.StatusError{Status: http.StatusTooManyRequests, Message: "try later"},
		},
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)

	if tok, ok := broker.credential(context.Background(), "agent-id"); !ok || tok != "ghs_owner" {
		t.Fatalf("initial credential = (%q, %v), want owner-set token", tok, ok)
	}
	grants.setRepos("agent-id", []string{"owner/base", "owner/workstream"})
	for attempt := range 2 {
		if tok, ok := broker.credential(context.Background(), "agent-id"); !ok || tok != "ghs_owner" {
			t.Fatalf("credential after widened-set 429 #%d = (%q, %v), want last owner token", attempt+1, tok, ok)
		}
	}
	if minter.callCount() != 3 {
		t.Fatalf("mint calls = %d, want initial mint and two widened-set attempts", minter.callCount())
	}
}

func TestGitCredentialBrokerRefreshPrunesExpiredNegativeEntries(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	broker := newGitCredentialTestBroker(nil, nil, &now, nil)
	broker.negative["owner/expired"] = gitCredentialNegativeEntry{expiresAt: now}
	broker.negative["owner/live"] = gitCredentialNegativeEntry{expiresAt: now.Add(time.Nanosecond)}

	broker.refreshDue(context.Background())
	if _, exists := broker.negative["owner/expired"]; exists {
		t.Fatal("expired negative entry remains after refresh")
	}
	if _, exists := broker.negative["owner/live"]; !exists {
		t.Fatal("unexpired negative entry pruned")
	}
}

func TestGitCredentialBrokerScopeRejectedWidenedMintUsesOwnerFallback(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{owners: map[store.AccountID]store.AccountID{"agent-id": "owner-id"}}
	grants.setRepos("owner-id", []string{"owner/base"})
	grants.setRepos("agent-id", []string{"owner/base", "owner/workstream"})
	minter := &fakeGitCredentialMinter{
		tokens: []string{"unused", "ghs_owner"}, expiresAt: now.Add(time.Hour),
		errByCall: []error{&forge.StatusError{Status: http.StatusUnprocessableEntity, Message: "scope rejected"}},
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)

	if tok, ok := broker.credential(context.Background(), "agent-id"); !ok || tok != "ghs_owner" {
		t.Fatalf("credential = (%q, %v), want owner fallback token", tok, ok)
	}
	if got, want := minter.mintRepos(), [][]string{{"base", "workstream"}, {"base"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mint repository sets = %v, want %v", got, want)
	}
}

func TestGitCredentialBrokerTransientFailureAfterLastKeyEvictionReturnsNoToken(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{}
	grants.setRepos("agent-id", []string{"owner/base"})
	minter := &fakeGitCredentialMinter{
		tokens: []string{"ghs_owner"}, expiresAt: now.Add(time.Hour),
		errByCall: []error{nil, &forge.StatusError{Status: http.StatusTooManyRequests, Message: "try later"}},
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	if tok, ok := broker.credential(context.Background(), "agent-id"); !ok || tok != "ghs_owner" {
		t.Fatalf("initial credential = (%q, %v), want owner-set token", tok, ok)
	}
	now = now.Add(gitCredentialMaxAge + time.Nanosecond)
	broker.refreshDue(context.Background())
	grants.setRepos("agent-id", []string{"owner/base", "owner/workstream"})
	if tok, ok := broker.credential(context.Background(), "agent-id"); ok || tok != "" {
		t.Fatalf("credential after eviction and transient failure = (%q, %v), want none", tok, ok)
	}
	if minter.callCount() != 2 {
		t.Fatalf("mint calls = %d, want one initial mint and one failed widened-set attempt", minter.callCount())
	}
}
func TestGitCredentialBrokerMintsNarrowedGrant(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/zeta", "owner/alpha"}}
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_scoped"}, expiresAt: now.Add(time.Hour)}
	signals := 0
	broker := newGitCredentialTestBroker(grants, minter, &now, func() error { signals++; return nil })
	resolver := &brokeredSecretResolver{inner: &fakeAgentSecretResolver{}, broker: broker}

	got := resolveBrokeredSecret(t, resolver)
	if len(got) != 1 || got[0].Name != gitCredentialSecretName || got[0].Value != "ghs_scoped" || got[0].Kind != secrets.SecretGH || got[0].Host != "github.com" || got[0].Delivery != secrets.DeliveryFile {
		t.Fatalf("resolved secrets = %+v, want one file-delivered GitHub credential", got)
	}
	if want := []string{"alpha", "zeta"}; !reflect.DeepEqual(minter.mintRepos()[0], want) {
		t.Fatalf("mint repositories = %v, want %v", minter.mintRepos()[0], want)
	}
	minter.mu.Lock()
	gotPerms := minter.perms[0]
	minter.mu.Unlock()
	if !reflect.DeepEqual(gotPerms, gitCredentialPermissions) {
		t.Fatalf("mint permissions = %v, want %v", gotPerms, gitCredentialPermissions)
	}
	if signals != 0 {
		t.Fatalf("signal calls after first mint = %d, want 0", signals)
	}
}

// The negative entry lands between the pre-flight check and the flight, as when
// a concurrent caller's flight rejects the same set.
func TestGitCredentialBrokerNegativeCachedInFlightFallsBackToOwnerGrants(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{owners: map[store.AccountID]store.AccountID{"agent-id": "owner-id"}}
	grants.setRepos("owner-id", []string{"owner/base"})
	grants.setRepos("agent-id", []string{"owner/base", "owner/workstream"})
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_owner"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	clockCalls := 0
	broker.clock = func() time.Time {
		clockCalls++
		if clockCalls == 2 {
			broker.mu.Lock()
			broker.negative["owner/base,owner/workstream"] = gitCredentialNegativeEntry{expiresAt: now.Add(gitCredentialMismatchCache)}
			broker.mu.Unlock()
		}
		return now
	}

	if tok, ok := broker.credential(context.Background(), "agent-id"); !ok || tok != "ghs_owner" {
		t.Fatalf("credential = (%q, %v), want owner-grants token", tok, ok)
	}
	if got, want := minter.mintRepos(), [][]string{{"base"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mint repository sets = %v, want only the owner set", got)
	}
}

func TestGitCredentialScopeSeparatesOwners(t *testing.T) {
	keyA, reposA, okA := gitCredentialScope([]string{"alice/app"})
	keyB, reposB, okB := gitCredentialScope([]string{"bob/app"})
	if !okA || !okB || keyA != "alice/app" || keyB != "bob/app" {
		t.Fatalf("scope keys = %q and %q, want distinct owner-qualified keys", keyA, keyB)
	}
	if !reflect.DeepEqual(reposA, []string{"app"}) || !reflect.DeepEqual(reposB, []string{"app"}) {
		t.Fatalf("mint repositories = %v and %v, want the leaf names submitted to the App API", reposA, reposB)
	}
	if key, repos, ok := gitCredentialScope([]string{"owner/z", "owner/a"}); !ok || key != "owner/a,owner/z" || !reflect.DeepEqual(repos, []string{"a", "z"}) {
		t.Fatalf("canonical scope = (%q, %v, %v), want sorted leaf names and owner-qualified cache key", key, repos, ok)
	}
	if _, _, ok := gitCredentialScope([]string{"owner/repo", "other/repo"}); ok {
		t.Fatal("mixed owners accepted")
	}
}

func TestGitCredentialBrokerRejectsGrantedRepositoryMismatch(t *testing.T) {
	tests := []struct {
		name        string
		grantedRepo string
	}{
		{name: "different owner", grantedRepo: "bob/app"},
		{name: "different repository same owner", grantedRepo: "alice/other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
			grants := &fakeGitCredentialGrants{repos: []string{"alice/app"}}
			minter := &fakeGitCredentialMinter{
				tokens: []string{"must-not-deliver", "ghs_after_negative_cache"}, expiresAt: now.Add(time.Hour),
				grantedRepos: []string{tt.grantedRepo},
			}
			broker := newGitCredentialTestBroker(grants, minter, &now, nil)
			resolver := &brokeredSecretResolver{inner: &fakeAgentSecretResolver{}, broker: broker}
			if got := resolveBrokeredSecret(t, resolver); len(got) != 0 {
				t.Fatalf("resolved secrets = %+v, want no credential for mismatched repository grant", got)
			}
			if len(broker.entries) != 0 {
				t.Fatalf("cached entries = %d, want 0 after scope mismatch", len(broker.entries))
			}
			if got := resolveBrokeredSecret(t, resolver); len(got) != 0 {
				t.Fatalf("resolved secrets = %+v, want negative cache to suppress remint", got)
			}
			if minter.callCount() != 1 {
				t.Fatalf("mint calls = %d, want one before negative cache expiry", minter.callCount())
			}
			now = now.Add(gitCredentialMismatchCache)
			minter.mu.Lock()
			minter.grantedRepos = []string{"alice/app"}
			minter.mu.Unlock()
			if got := resolveBrokeredSecret(t, resolver); len(got) != 1 || got[0].Value != "ghs_after_negative_cache" {
				t.Fatalf("resolved secrets after negative cache expiry = %+v, want newly minted credential", got)
			}
			if minter.callCount() != 2 {
				t.Fatalf("mint calls after negative cache expiry = %d, want 2", minter.callCount())
			}
		})
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
	minterRepos := minter.mintRepos()
	if len(minterRepos) != 1 || minterRepos[0] != nil {
		t.Fatalf("mint repositories = %v, want nil installation-wide scope", minterRepos)
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
			if minter.callCount() != 0 {
				t.Fatalf("mint calls = %d, want 0", minter.callCount())
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
	if minter.callCount() != 0 {
		t.Fatalf("mint calls = %d, want 0", minter.callCount())
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
	if minter.callCount() != 0 {
		t.Fatalf("mint calls = %d, want 0", minter.callCount())
	}
}

func TestBrokeredSecretResolverFiltersUserGitHubSecrets(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	userSecrets := []secrets.ResolvedSecret{
		{Name: "USER_GH", Value: "user-token", Kind: secrets.SecretGH, Host: "github.com"},
		{Name: "OTHER_GH", Value: "other-token", Kind: secrets.SecretGH, Host: "github.enterprise"},
		{Name: "GENERIC", Value: "generic-value", Kind: secrets.SecretGeneric},
		{Name: "PROVIDER", Value: "provider-value", Kind: secrets.SecretProvider, Provider: "anthropic"},
		{Name: "GH_TOKEN", Value: "legacy-token", Delivery: secrets.DeliveryEnv, Kind: secrets.SecretGeneric},
		{Name: "GITHUB_TOKEN", Value: "legacy-token", Delivery: secrets.DeliveryEnv, Kind: secrets.SecretGeneric},
		{Name: "GH_ENTERPRISE_TOKEN", Value: "legacy-token", Delivery: secrets.DeliveryEnv, Kind: secrets.SecretGeneric},
		{Name: "GITHUB_ENTERPRISE_TOKEN", Value: "legacy-token", Delivery: secrets.DeliveryEnv, Kind: secrets.SecretGeneric},
		{Name: "GIT_CONFIG_VALUE_0", Value: "!legacy-helper", Delivery: secrets.DeliveryEnv, Kind: secrets.SecretGeneric},
	}
	tests := []struct {
		name          string
		broker        bool
		grants        []string
		reason        string
		wantToken     string
		wantMintCalls int
		resolverError bool
	}{
		{name: "broker credential replaces user credentials", broker: true, grants: []string{"owner/repo"}, reason: gitCredentialReason, wantToken: "ghs_scoped", wantMintCalls: 1},
		{name: "no grants drops user credentials", broker: true, reason: gitCredentialReason},
		{name: "nil broker drops user credentials", reason: gitCredentialReason},
		{name: "other reason drops user credentials", broker: true, grants: []string{"owner/repo"}, reason: "other secret consumer"},
		{name: "resolver error drops credentials", broker: true, grants: []string{"owner/repo"}, reason: gitCredentialReason, resolverError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := &fakeAgentSecretResolver{resolved: userSecrets}
			if tt.resolverError {
				inner.err = errors.New("user secret resolution failed")
			}
			var resolver *brokeredSecretResolver
			var minter *fakeGitCredentialMinter
			if tt.broker {
				grants := &fakeGitCredentialGrants{repos: tt.grants}
				minter = &fakeGitCredentialMinter{tokens: []string{"ghs_scoped"}, expiresAt: now.Add(time.Hour)}
				broker := newGitCredentialTestBroker(grants, minter, &now, nil)
				resolver = &brokeredSecretResolver{inner: inner, broker: broker}
			} else {
				resolver = &brokeredSecretResolver{inner: inner}
			}

			got, err := resolver.ResolveFor(context.Background(), "agent-id", tt.reason)
			if (err != nil) != tt.resolverError {
				t.Fatalf("ResolveFor error = %v, want resolver error %v", err, tt.resolverError)
			}
			assertNoUserGitHubSecrets(t, got, tt.wantToken)
			if minter != nil && minter.callCount() != tt.wantMintCalls {
				t.Errorf("mint calls = %d, want %d", minter.callCount(), tt.wantMintCalls)
			}
		})
	}
}

func assertNoUserGitHubSecrets(t *testing.T, got []secrets.ResolvedSecret, wantBrokered string) {
	t.Helper()
	wantNames := []string{"GENERIC", "PROVIDER"}
	if wantBrokered != "" {
		wantNames = append(wantNames, gitCredentialSecretName)
	}
	gotNames := make([]string, len(got))
	githubSecrets := 0
	for i, secret := range got {
		gotNames[i] = secret.Name
		if secret.Kind == secrets.SecretGH {
			githubSecrets++
			if secret.Name != gitCredentialSecretName || secret.Value != wantBrokered || secret.Host != "github.com" {
				t.Errorf("GitHub secret = %+v, want the brokered token only", secret)
			}
		}
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Errorf("resolved secret names = %v, want %v", gotNames, wantNames)
	}
	wantGitHubSecrets := 0
	if wantBrokered != "" {
		wantGitHubSecrets = 1
	}
	if githubSecrets != wantGitHubSecrets {
		t.Errorf("GitHub secret count = %d, want %d", githubSecrets, wantGitHubSecrets)
	}
}

func TestBrokeredSecretResolverNilInnerReturnsNil(t *testing.T) {
	resolver := &brokeredSecretResolver{}
	got, err := resolver.ResolveFor(context.Background(), "agent-id", gitCredentialReason)
	if err != nil || got != nil {
		t.Fatalf("ResolveFor with nil inner = (%v, %v), want (nil, nil)", got, err)
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
	if minter.callCount() != 1 {
		t.Fatalf("mint calls = %d, want one cached mint", minter.callCount())
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

	now = now.Add(time.Hour - gitCredentialRefreshLead)
	minter.mu.Lock()
	minter.expiresAt = now.Add(time.Hour)
	minter.mu.Unlock()
	broker.refreshDue(context.Background())
	if minter.callCount() != 2 {
		t.Fatalf("mint calls = %d, want 2 at refresh boundary", minter.callCount())
	}
	if signals != 1 {
		t.Fatalf("signal calls = %d, want exactly 1", signals)
	}
}

func TestGitCredentialBrokerRefreshSkipsEntryRefreshedAfterSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/repo"}}
	minter := &fakeGitCredentialMinter{
		tokens: []string{"ghs_old", "ghs_request", "ghs_unexpected"}, expiresAt: now.Add(time.Hour),
	}
	signals := 0
	broker := newGitCredentialTestBroker(grants, minter, &now, func() error { signals++; return nil })
	resolver := &brokeredSecretResolver{inner: &fakeAgentSecretResolver{}, broker: broker}
	resolveBrokeredSecret(t, resolver)

	now = now.Add(time.Hour - gitCredentialRefreshLead)
	minter.mu.Lock()
	minter.expiresAt = now.Add(time.Hour)
	minter.mu.Unlock()
	previousToken := broker.entries["owner/repo"].token
	if token, ok := broker.credential(context.Background(), "agent-id"); !ok || token != "ghs_request" {
		t.Fatalf("request refresh credential = (%q, %v), want request token", token, ok)
	}
	if minter.callCount() != 2 {
		t.Fatalf("mint calls = %d, want initial and request refresh only", minter.callCount())
	}
	if signals != 1 {
		t.Fatalf("signal calls after request replacement = %d, want exactly 1", signals)
	}
	broker.refreshSnapshot(context.Background(), []string{"owner/repo"}, map[string]string{"owner/repo": previousToken})
	if minter.callCount() != 2 {
		t.Fatalf("mint calls after processing snapshot = %d, want no duplicate mint", minter.callCount())
	}
	if signals != 1 {
		t.Fatalf("signal calls after processing snapshot = %d, want exactly 1", signals)
	}
}

func TestGitCredentialBrokerRefreshMismatchRemovesEntry(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/repo"}}
	minter := &fakeGitCredentialMinter{
		tokens: []string{"ghs_old", "ghs_mismatch"}, expiresAt: now.Add(time.Hour),
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	resolver := &brokeredSecretResolver{inner: &fakeAgentSecretResolver{}, broker: broker}
	resolveBrokeredSecret(t, resolver)

	now = now.Add(time.Hour - gitCredentialRefreshLead)
	minter.mu.Lock()
	minter.grantedRepos = []string{"owner/other"}
	minter.mu.Unlock()
	broker.refreshDue(context.Background())
	if _, exists := broker.entries["owner/repo"]; exists {
		t.Fatal("cached entry remains after refresh scope mismatch")
	}
	if minter.callCount() != 2 {
		t.Fatalf("mint calls after mismatch = %d, want initial and one refresh", minter.callCount())
	}
	broker.refreshDue(context.Background())
	if minter.callCount() != 2 {
		t.Fatalf("mint calls after next tick = %d, want no repeated refresh", minter.callCount())
	}
}

// A request-path mismatch leaves the old entry cached; refresh must not re-mint
// while the mismatch is negatively cached.
func TestGitCredentialBrokerRefreshHonoursMismatchCache(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/repo"}}
	minter := &fakeGitCredentialMinter{
		tokens: []string{"ghs_old", "ghs_mismatch", "ghs_unused"}, expiresAt: now.Add(time.Hour),
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	resolver := &brokeredSecretResolver{inner: &fakeAgentSecretResolver{}, broker: broker}
	resolveBrokeredSecret(t, resolver)

	now = now.Add(time.Hour - gitCredentialRefreshLead)
	minter.mu.Lock()
	minter.grantedRepos = []string{"owner/other"}
	minter.mu.Unlock()
	if tok, ok := broker.credential(context.Background(), "agent"); ok {
		t.Fatalf("credential after request-path mismatch = %q, want none", tok)
	}
	if _, exists := broker.entries["owner/repo"]; !exists {
		t.Fatal("precondition: request-path mismatch should leave the old entry cached")
	}
	broker.refreshDue(context.Background())
	if minter.callCount() != 2 {
		t.Fatalf("mint calls = %d, want initial and request-path only", minter.callCount())
	}
}

func TestGitCredentialBrokerRefreshStopsWhenContextCancelled(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/alpha"}}
	minter := &fakeGitCredentialMinter{
		tokens: []string{"ghs_alpha_new"}, expiresAt: now.Add(time.Hour),
		started: make(chan struct{}, 1), release: make(chan struct{}),
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	broker.entries["owner/alpha"] = gitCredentialEntry{
		token: "ghs_alpha_old", expiresAt: now.Add(time.Hour), refreshAt: now, lastUsed: now,
	}
	broker.entries["owner/beta"] = gitCredentialEntry{
		token: "ghs_beta_old", expiresAt: now.Add(time.Hour), refreshAt: now, lastUsed: now,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() {
		broker.refreshDue(ctx)
		close(finished)
	}()
	<-minter.started
	cancel()
	close(minter.release)
	<-finished
	if minter.callCount() != 1 {
		t.Fatalf("mint calls = %d, want one before cancellation stops remaining keys", minter.callCount())
	}
}

func TestGitCredentialBrokerRefreshFailureServesStaleToken(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/repo"}}
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_old"}, expiresAt: now.Add(time.Hour)}
	signals := 0
	broker := newGitCredentialTestBroker(grants, minter, &now, func() error { signals++; return nil })
	resolver := &brokeredSecretResolver{inner: &fakeAgentSecretResolver{}, broker: broker}
	resolveBrokeredSecret(t, resolver)

	now = now.Add(time.Hour - gitCredentialRefreshLead)
	minter.mu.Lock()
	minter.err = errors.New("mint failed")
	minter.mu.Unlock()
	got := resolveBrokeredSecret(t, resolver)
	if len(got) != 1 || got[0].Value != "ghs_old" {
		t.Fatalf("resolved secrets = %+v, want stale token with fifteen minutes remaining", got)
	}
	if signals != 0 {
		t.Fatalf("signal calls = %d, want 0", signals)
	}
	if got := broker.staleCredential("owner/repo"); got != "ghs_old" {
		t.Fatalf("stale credential with fifteen minutes left = %q, want old token", got)
	}
}

func TestGitCredentialBrokerStaleTokenRequiresTenMinutesRemaining(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	broker := newGitCredentialTestBroker(nil, nil, &now, nil)
	for _, remaining := range []time.Duration{gitCredentialMinStale, gitCredentialMinStale - time.Nanosecond} {
		broker.entries["owner/repo"] = gitCredentialEntry{
			token: "cached", expiresAt: now.Add(remaining), lastUsed: now,
		}
		got := broker.staleCredential("owner/repo")
		if remaining == gitCredentialMinStale && got != "cached" {
			t.Fatalf("credential with exactly ten minutes left = %q, want cached", got)
		}
		if remaining < gitCredentialMinStale && got != "" {
			t.Fatalf("credential with less than ten minutes left = %q, want omitted", got)
		}
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
	if minter.callCount() != 1 {
		t.Fatalf("mint calls = %d, want no refresh during prune", minter.callCount())
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
	if len(got) != 0 || minter.callCount() != 0 {
		t.Fatalf("resolved=%v mint calls=%d, want no broker access", got, minter.callCount())
	}
}

func TestGitCredentialBrokerSingleflightsConcurrentMints(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	const callers = 8
	grants := &fakeGitCredentialGrants{repos: []string{"owner/repo"}, listed: make(chan struct{}, callers)}
	minter := &blockingGitCredentialMinter{
		started: make(chan struct{}, callers), release: make(chan struct{}), expiresAt: now.Add(time.Hour),
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
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
	for range callers {
		<-grants.listed
	}
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

func TestGitCredentialBrokerCancelledFirstCallerDoesNotCancelSharedMint(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	const callers = 2
	grants := &fakeGitCredentialGrants{repos: []string{"owner/repo"}, listed: make(chan struct{}, callers)}
	minter := &blockingGitCredentialMinter{
		started: make(chan struct{}, callers), release: make(chan struct{}), expiresAt: now.Add(time.Hour),
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstResult := make(chan string, 1)
	secondResult := make(chan string, 1)
	go func() {
		token, ok := broker.credential(firstCtx, "agent-id")
		if ok {
			firstResult <- token
			return
		}
		firstResult <- ""
	}()
	<-minter.started
	<-grants.listed
	go func() {
		token, ok := broker.credential(context.Background(), "agent-id")
		if ok {
			secondResult <- token
			return
		}
		secondResult <- ""
	}()
	<-grants.listed
	cancelFirst()
	close(minter.release)
	if token := <-firstResult; token != "ghs_concurrent" {
		t.Fatalf("canceled first caller token = %q, want shared mint result", token)
	}
	if token := <-secondResult; token != "ghs_concurrent" {
		t.Fatalf("second caller token = %q, want shared mint result", token)
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

func (m *blockingGitCredentialMinter) Mint(ctx context.Context, repos []string, _ map[string]string) (forge.ScopedToken, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	m.started <- struct{}{}
	select {
	case <-ctx.Done():
		return forge.ScopedToken{}, ctx.Err()
	case <-m.release:
		granted := make([]string, len(repos))
		for i, repo := range repos {
			granted[i] = "owner/" + repo
		}
		return forge.ScopedToken{Token: "ghs_concurrent", ExpiresAt: m.expiresAt, Repositories: granted}, nil
	}
}

func (m *blockingGitCredentialMinter) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}
