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
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

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
func (g *fakeGitCredentialGrants) setSharedRepos(repos []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.repos = append([]string(nil), repos...)
}

type grantChangeOnListStore struct {
	*fakeGitCredentialGrants
	mu        sync.Mutex
	changed   bool
	afterList func()
}

func (s *grantChangeOnListStore) ListForgeScopeRepos(ctx context.Context, agent store.AccountID, provider store.ForgeProvider, host string) ([]string, error) {
	repos, err := s.fakeGitCredentialGrants.ListForgeScopeRepos(ctx, agent, provider, host)
	s.mu.Lock()
	change := !s.changed
	s.changed = true
	s.mu.Unlock()
	if change && s.afterList != nil {
		s.afterList()
	}
	return repos, err
}

func (g *fakeGitCredentialGrants) ownerCallCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ownerCalls
}

type fakeGitCredentialMinter struct {
	tokens          []string
	expiresAt       time.Time
	err             error
	calls           int
	repos           [][]string
	grantedRepos    []string
	perms           []map[string]string
	revoked         []string
	revokeErr       error
	revokeErrByCall []error
	mu              sync.Mutex
	started         chan struct{}
	release         chan struct{}
	releaseByCall   []chan struct{}
	errByCall       []error
}

func (m *fakeGitCredentialMinter) Mint(ctx context.Context, repos []string, perms map[string]string) (forge.ScopedToken, error) {
	m.mu.Lock()
	m.calls++
	m.repos = append(m.repos, append([]string(nil), repos...))
	copyPerms := make(map[string]string, len(perms))
	maps.Copy(copyPerms, perms)
	callIndex := m.calls - 1
	m.perms = append(m.perms, copyPerms)
	var token string
	if len(m.tokens) > 0 {
		index := min(callIndex, len(m.tokens)-1)
		token = m.tokens[index]
	}
	expiresAt, granted, err, started, release := m.expiresAt, m.grantedRepos, m.err, m.started, m.release
	var releaseForCall <-chan struct{}
	if callIndex < len(m.releaseByCall) && m.releaseByCall[callIndex] != nil {
		releaseForCall = m.releaseByCall[callIndex]
	} else {
		releaseForCall = release
	}
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
	if releaseForCall != nil {
		select {
		case <-ctx.Done():
			return forge.ScopedToken{}, ctx.Err()
		case <-releaseForCall:
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

func (m *fakeGitCredentialMinter) Revoke(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	callIndex := len(m.revoked)
	m.revoked = append(m.revoked, token)
	if callIndex < len(m.revokeErrByCall) {
		return m.revokeErrByCall[callIndex]
	}
	return m.revokeErr
}

func (*blockingGitCredentialMinter) Revoke(context.Context, string) error {
	return nil
}

func (m *fakeGitCredentialMinter) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *fakeGitCredentialMinter) revokeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.revoked)
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

func newGitCredentialTestBroker(credentialStore gitCredentialStore, minter gitCredentialMinter, now *time.Time, signal func(context.Context) error) *gitCredentialBroker {
	return &gitCredentialBroker{
		host:     "github.com",
		store:    credentialStore,
		minter:   minter,
		log:      slog.New(slog.DiscardHandler),
		signal:   signal,
		clock:    func() time.Time { return *now },
		limiter:  rate.NewLimiter(gitCredentialMintRate, gitCredentialMintBurst),
		entries:  make(map[string]gitCredentialEntry),
		negative: make(map[string]gitCredentialNegativeEntry),
		last:     make(map[store.AccountID]string),
		served:   make(map[store.AccountID]map[string]struct{}),
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

func TestGitCredentialBrokerGrantsChangedRevokesRemovedScope(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{}
	grants.setRepos("agent-id", []string{"owner/base", "owner/removed"})
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_old", "ghs_narrow"}, expiresAt: now.Add(time.Hour)}
	signals := 0
	ctx := context.WithValue(t.Context(), signalCtxKey{}, "grant-change")
	broker := newGitCredentialTestBroker(grants, minter, &now, func(got context.Context) error {
		signals++
		if got.Value(signalCtxKey{}) != "grant-change" {
			t.Error("grant-change signal lost the caller ctx")
		}
		return nil
	})
	if token, ok := broker.credential(context.Background(), "agent-id"); !ok || token != "ghs_old" {
		t.Fatalf("initial credential = (%q, %v), want old token", token, ok)
	}
	grants.setRepos("agent-id", []string{"owner/base"})
	oldKey := "owner/base,owner/removed"
	old := broker.entries[oldKey]
	broker.grantsChanged(ctx, "agent-id")
	if got := broker.entries["owner/base"].token; got != "ghs_narrow" {
		t.Fatalf("new entry token = %q, want narrow token", got)
	}
	if _, exists := broker.entries[oldKey]; exists {
		t.Fatal("old over-granting entry remains")
	}
	if got := broker.lastCredential("agent-id"); got != "owner/base" {
		t.Fatalf("last key = %q, want narrowed key", got)
	}
	if !old.expiresAt.After(now) {
		t.Fatal("test precondition: old token is not live")
	}
	if got := minter.revokeCount(); got != 0 {
		t.Fatalf("revoke calls before grace = %d, want none", got)
	}
	if signals != 1 {
		t.Fatalf("signals = %d, want one after replacement", signals)
	}
	now = now.Add(gitCredentialRevokeGrace)
	broker.refreshDue(context.Background())
	minter.mu.Lock()
	gotRevoked := append([]string(nil), minter.revoked...)
	minter.mu.Unlock()
	if !reflect.DeepEqual(gotRevoked, []string{"ghs_old"}) {
		t.Fatalf("revoked tokens = %v, want old token after delivery grace", gotRevoked)
	}
}

func TestGitCredentialBrokerGrantsChangedRetrySignalsAfterLimiterRefill(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{}
	grants.setRepos("agent-id", []string{"owner/base", "owner/removed"})
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_old", "ghs_new"}, expiresAt: now.Add(time.Hour)}
	signals := 0
	broker := newGitCredentialTestBroker(grants, minter, &now, func(context.Context) error {
		signals++
		return nil
	})
	if token, ok := broker.credential(context.Background(), "agent-id"); !ok || token != "ghs_old" {
		t.Fatalf("initial credential = (%q, %v), want old token", token, ok)
	}
	for broker.limiter.AllowN(now, 1) {
	}
	grants.setRepos("agent-id", []string{"owner/base"})
	broker.grantsChanged(context.Background(), "agent-id")
	if minter.callCount() != 1 {
		t.Fatalf("mint calls after budget-denied grant change = %d, want one", minter.callCount())
	}
	if signals != 1 {
		t.Fatalf("grant-change signal calls = %d, want one", signals)
	}
	broker.refreshDue(context.Background())
	if signals != 1 {
		t.Fatalf("signal calls before limiter refill = %d, want one", signals)
	}
	now = now.Add(time.Second)
	broker.refreshDue(context.Background())
	if signals != 2 {
		t.Fatalf("signal calls after limiter refill = %d, want two including grant-change signal", signals)
	}
	if token, ok := broker.credential(context.Background(), "agent-id"); !ok || token != "ghs_new" {
		t.Fatalf("credential after retry signal = (%q, %v), want new token", token, ok)
	}
}

func TestGitCredentialBrokerGrantsChangedWideningKeepsToken(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/base"}}
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_old", "ghs_wide"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	if token, ok := broker.credential(context.Background(), "agent-id"); !ok || token != "ghs_old" {
		t.Fatalf("initial credential = (%q, %v), want old token", token, ok)
	}
	grants.repos = []string{"owner/base", "owner/added"}
	broker.grantsChanged(context.Background(), "agent-id")
	if len(minter.revoked) != 0 {
		t.Fatalf("revoked tokens = %v, want none for widening", minter.revoked)
	}
	if token, ok := broker.cachedCredential("owner/added,owner/base"); !ok || token != "ghs_wide" {
		t.Fatalf("widened credential = (%q, %v), want new token", token, ok)
	}
}

func TestGitCredentialBrokerGrantsChangedWidenThenNarrowRevokesHistoricalTokens(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/a"}}
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_a", "ghs_a_b", "ghs_b"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	ctx := context.Background()
	if token, ok := broker.credential(ctx, "agent-id"); !ok || token != "ghs_a" {
		t.Fatalf("initial credential = (%q, %v), want A token", token, ok)
	}
	grants.setSharedRepos([]string{"owner/a", "owner/b"})
	broker.grantsChanged(ctx, "agent-id")
	grants.setSharedRepos([]string{"owner/b"})
	broker.grantsChanged(ctx, "agent-id")
	if got := minter.revokeCount(); got != 0 {
		t.Fatalf("revoke calls before grace = %d, want none", got)
	}
	now = now.Add(gitCredentialRevokeGrace)
	broker.refreshDue(ctx)
	minter.mu.Lock()
	revoked := append([]string(nil), minter.revoked...)
	minter.mu.Unlock()
	if len(revoked) != 2 || !slices.Contains(revoked, "ghs_a") || !slices.Contains(revoked, "ghs_a_b") {
		t.Fatalf("revoked tokens = %v, want both historical over-granting tokens", revoked)
	}
}

func TestGitCredentialBrokerGrantsChangedDiscardsInFlightOldMint(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/a", "owner/b"}}
	release := make(chan struct{})
	minter := &fakeGitCredentialMinter{
		tokens: []string{"ghs_late"}, expiresAt: now.Add(time.Hour),
		started: make(chan struct{}, 1), releaseByCall: []chan struct{}{release},
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	type result struct {
		token string
		ok    bool
	}
	finished := make(chan result, 1)
	go func() {
		token, ok := broker.credential(context.Background(), "agent-id")
		finished <- result{token: token, ok: ok}
	}()
	<-minter.started
	grants.setSharedRepos([]string{"owner/b"})
	broker.grantsChanged(context.Background(), "agent-id")
	close(release)
	got := <-finished
	if got.ok || got.token != "" {
		t.Fatalf("in-flight credential = (%q, %v), want no token", got.token, got.ok)
	}
	if _, exists := broker.entries["owner/a,owner/b"]; exists {
		t.Fatal("late old-scope token was cached")
	}
	minter.mu.Lock()
	revoked := append([]string(nil), minter.revoked...)
	minter.mu.Unlock()
	if !reflect.DeepEqual(revoked, []string{"ghs_late"}) {
		t.Fatalf("revoked tokens = %v, want late token", revoked)
	}
}

func TestGitCredentialBrokerGrantsChangedFailedNarrowMintRevokesAndFailsClosed(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/base", "owner/removed"}}
	minter := &fakeGitCredentialMinter{
		tokens: []string{"ghs_old"}, expiresAt: now.Add(time.Hour),
		errByCall: []error{nil, errors.New("mint unavailable"), errors.New("mint unavailable")},
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	if token, ok := broker.credential(context.Background(), "agent-id"); !ok || token != "ghs_old" {
		t.Fatalf("initial credential = (%q, %v), want old token", token, ok)
	}
	grants.repos = []string{"owner/base"}
	broker.grantsChanged(context.Background(), "agent-id")
	if _, exists := broker.entries["owner/base,owner/removed"]; exists {
		t.Fatal("old over-granting entry remains after failed mint")
	}
	if got := broker.lastCredential("agent-id"); got != "" {
		t.Fatalf("last key after failed mint = %q, want none", got)
	}
	if token, ok := broker.credential(context.Background(), "agent-id"); ok || token != "" {
		t.Fatalf("credential after failed scope change = (%q, %v), want fail closed", token, ok)
	}
	if got := minter.revokeCount(); got != 0 {
		t.Fatalf("revoke calls before grace = %d, want none", got)
	}
	now = now.Add(gitCredentialRevokeGrace)
	broker.refreshDue(context.Background())
	minter.mu.Lock()
	gotRevoked := append([]string(nil), minter.revoked...)
	minter.mu.Unlock()
	if !reflect.DeepEqual(gotRevoked, []string{"ghs_old"}) {
		t.Fatalf("revoked tokens = %v, want old token after delivery grace", gotRevoked)
	}
}

func TestGitCredentialBrokerGrantsChangedInvalidatesSharedOldScope(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{}
	grants.setRepos("agent-a", []string{"owner/base", "owner/removed"})
	grants.setRepos("agent-b", []string{"owner/base", "owner/removed"})
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_shared", "ghs_a_narrow", "ghs_b_narrow"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	for _, agent := range []store.AccountID{"agent-a", "agent-b"} {
		if token, ok := broker.credential(context.Background(), agent); !ok || token != "ghs_shared" {
			t.Fatalf("%s initial credential = (%q, %v), want shared token", agent, token, ok)
		}
	}
	grants.setRepos("agent-a", []string{"owner/base"})
	broker.grantsChanged(context.Background(), "agent-a")
	if got := broker.lastCredential("agent-b"); got != "" {
		t.Fatalf("agent B last key = %q, want removed", got)
	}
	if token, ok := broker.credential(context.Background(), "agent-b"); !ok || token != "ghs_b_narrow" {
		t.Fatalf("agent B credential after invalidation = (%q, %v), want re-mint", token, ok)
	}
	if got := minter.callCount(); got != 3 {
		t.Fatalf("mint calls = %d, want shared token and two replacements", got)
	}
}

func TestGitCredentialBrokerRefreshDropsOrphanedEntry(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	minter := &fakeGitCredentialMinter{tokens: []string{"unused"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(nil, minter, &now, nil)
	broker.entries["owner/orphan"] = gitCredentialEntry{token: "orphan", expiresAt: now.Add(time.Hour), refreshAt: now}
	broker.served["agent-orphan"] = map[string]struct{}{"owner/orphan": {}}
	broker.refreshDue(context.Background())
	if _, exists := broker.entries["owner/orphan"]; exists {
		t.Fatal("orphaned entry remains after refresh")
	}
	if got := minter.callCount(); got != 0 {
		t.Fatalf("mint calls for orphaned entry = %d, want none", got)
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

func TestGitCredentialBrokerTransientNarrowedMintDoesNotServeRevokedWiderToken(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{}
	grants.setRepos("agent-id", []string{"owner/base", "owner/workstream"})
	minter := &fakeGitCredentialMinter{
		tokens: []string{"ghs_wide", "unused", "ghs_remint"}, expiresAt: now.Add(time.Hour),
		errByCall: []error{nil, &forge.StatusError{Status: http.StatusTooManyRequests, Message: "try later"}},
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	start := now
	if tok, ok := broker.credential(context.Background(), "agent-id"); !ok || tok != "ghs_wide" {
		t.Fatalf("initial credential = (%q, %v), want widened-set token", tok, ok)
	}

	grants.setRepos("agent-id", []string{"owner/base"})
	now = start.Add(40 * time.Minute)
	if tok, ok := broker.credential(context.Background(), "agent-id"); ok || tok != "" {
		t.Fatalf("credential after revoke and narrowed-set 429 = (%q, %v), want none", tok, ok)
	}
	now = start.Add(gitCredentialMaxAge + time.Nanosecond)
	broker.refreshDue(context.Background())
	if _, exists := broker.entries["owner/base,owner/workstream"]; exists {
		t.Fatal("revoked widened-set entry kept alive past its idle window")
	}
	if minter.callCount() != 2 {
		t.Fatalf("mint calls = %d, want no re-mint of the revoked widened set", minter.callCount())
	}
}

func TestGitCredentialBrokerTransientNarrowedMintDoesNotServeWildcardToken(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{owners: map[store.AccountID]store.AccountID{"agent-id": "owner-id"}}
	grants.setRepos("owner-id", []string{"*"})
	grants.setRepos("agent-id", []string{"*"})
	minter := &fakeGitCredentialMinter{
		tokens: []string{"ghs_wildcard", "unused"}, expiresAt: now.Add(time.Hour),
		errByCall: []error{nil, &forge.StatusError{Status: http.StatusTooManyRequests, Message: "try later"}},
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	if tok, ok := broker.credential(context.Background(), "agent-id"); !ok || tok != "ghs_wildcard" {
		t.Fatalf("initial credential = (%q, %v), want wildcard token", tok, ok)
	}

	grants.setRepos("owner-id", []string{"owner/base"})
	grants.setRepos("agent-id", []string{"owner/base"})
	if tok, ok := broker.credential(context.Background(), "agent-id"); ok || tok != "" {
		t.Fatalf("credential after narrowing and 429 = (%q, %v), want none", tok, ok)
	}
	if minter.callCount() != 2 {
		t.Fatalf("mint calls = %d, want wildcard mint and one narrowed attempt", minter.callCount())
	}
}

func TestGitCredentialBrokerTransientWidenedWildcardServesFiniteLastKey(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/base"}}
	minter := &fakeGitCredentialMinter{
		tokens: []string{"ghs_finite"}, expiresAt: now.Add(time.Hour),
		errByCall: []error{nil, &forge.StatusError{Status: http.StatusTooManyRequests, Message: "try later"}},
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	if token, ok := broker.credential(context.Background(), "agent-id"); !ok || token != "ghs_finite" {
		t.Fatalf("initial finite credential = (%q, %v), want finite token", token, ok)
	}
	grants.repos = []string{"*"}
	if token, ok := broker.credential(context.Background(), "agent-id"); !ok || token != "ghs_finite" {
		t.Fatalf("credential after wildcard grant and 429 = (%q, %v), want finite last token", token, ok)
	}
	if minter.callCount() != 2 {
		t.Fatalf("mint calls = %d, want initial finite mint and failed wildcard mint", minter.callCount())
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
	broker := newGitCredentialTestBroker(grants, minter, &now, func(context.Context) error { signals++; return nil })
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

func TestGitCredentialBrokerNegativeCachedInFlightFallsBackToOwnerGrants(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{owners: map[store.AccountID]store.AccountID{"agent-id": "owner-id"}, listed: make(chan struct{}, 2)}
	grants.setRepos("owner-id", []string{"owner/base"})
	grants.setRepos("agent-id", []string{"owner/base", "owner/workstream"})
	started, release := make(chan struct{}, 1), make(chan struct{})
	minter := &fakeGitCredentialMinter{
		tokens: []string{"unused", "ghs_owner"}, expiresAt: now.Add(time.Hour),
		errByCall: []error{&forge.StatusError{Status: http.StatusUnprocessableEntity, Message: "scope rejected"}},
		started:   started, release: release,
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	type result struct {
		token string
		ok    bool
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			token, ok := broker.credential(context.Background(), "agent-id")
			results <- result{token: token, ok: ok}
		}()
	}
	<-started
	<-grants.listed
	<-grants.listed
	close(release)
	for range 2 {
		got := <-results
		if !got.ok || got.token != "ghs_owner" {
			t.Fatalf("credential = (%q, %v), want owner-grants token", got.token, got.ok)
		}
	}
	if got, want := minter.mintRepos(), [][]string{{"base", "workstream"}, {"base"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mint repository sets = %v, want one rejected full set and one owner set", got)
	}
}

func TestGitCredentialScopeWithin(t *testing.T) {
	tests := []struct {
		name  string
		inner string
		outer string
		want  bool
	}{
		{name: "same scope", inner: "owner/a,owner/b", outer: "owner/a,owner/b", want: true},
		{name: "finite inner contained by wildcard", inner: "owner/a,owner/b", outer: "*", want: true},
		{name: "inner wildcard is not finite", inner: "*", outer: "owner/a,owner/b", want: false},
		{name: "missing repository", inner: "owner/a,owner/c", outer: "owner/a,owner/b", want: false},
		{name: "ordered merge skips earlier outer repository", inner: "owner/b", outer: "owner/a,owner/b,owner/c", want: true},
		{name: "inner repository absent between outer entries", inner: "owner/b", outer: "owner/a,owner/c", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := gitCredentialScopeWithin(tc.inner, tc.outer); got != tc.want {
				t.Errorf("gitCredentialScopeWithin(%q, %q) = %t, want %t", tc.inner, tc.outer, got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name  string
		inner string
		outer string
	}{
		{name: "trailing comma", inner: "owner/a,", outer: "owner/a"},
		{name: "repeated comma", inner: "owner/a,,owner/b", outer: "owner/a,owner/b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if gitCredentialScopeWithin(tc.inner, tc.outer) {
				t.Errorf("gitCredentialScopeWithin(%q, %q) = true, want false", tc.inner, tc.outer)
			}
		})
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

func TestGitCredentialBrokerScopeRejectedRefreshSignals(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{owners: map[store.AccountID]store.AccountID{"agent-id": "owner-id"}}
	grants.setRepos("owner-id", []string{"owner/base"})
	grants.setRepos("agent-id", []string{"owner/base", "owner/workstream"})
	minter := &fakeGitCredentialMinter{
		tokens: []string{"ghs_old", "unused", "ghs_owner"}, expiresAt: now.Add(time.Hour),
		errByCall: []error{nil, &forge.StatusError{Status: http.StatusUnprocessableEntity, Message: "scope rejected"}},
	}
	signals := 0
	broker := newGitCredentialTestBroker(grants, minter, &now, func(context.Context) error { signals++; return nil })
	resolver := &brokeredSecretResolver{inner: &fakeAgentSecretResolver{}, broker: broker}
	resolveBrokeredSecret(t, resolver)

	now = now.Add(time.Hour - gitCredentialRefreshLead)
	broker.refreshDue(context.Background())
	if signals != 1 {
		t.Fatalf("signal calls after scope-rejected refresh = %d, want one", signals)
	}
	if len(broker.last) != 0 {
		t.Fatalf("last credential references after rejected refresh = %v, want none", broker.last)
	}
	if token, ok := broker.credential(context.Background(), "agent-id"); !ok || token != "ghs_owner" {
		t.Fatalf("credential after rejected refresh = (%q, %v), want owner-grants token", token, ok)
	}
}

func TestGitCredentialBrokerRefreshSignalsOnce(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/alpha"}}
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_old", "ghs_new"}, expiresAt: now.Add(time.Hour)}
	signals := 0
	broker := newGitCredentialTestBroker(grants, minter, &now, func(context.Context) error { signals++; return nil })
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
	broker := newGitCredentialTestBroker(grants, minter, &now, func(context.Context) error { signals++; return nil })
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

func TestGitCredentialBrokerCancelledRefreshSkipsPendingRevokes(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	minter := &fakeGitCredentialMinter{tokens: []string{"unused"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(&fakeGitCredentialGrants{}, minter, &now, nil)
	broker.pendingRevokes = []supersededToken{{token: "ghs_old", expiresAt: now.Add(time.Hour)}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	broker.refreshDue(ctx)
	minter.mu.Lock()
	revoked := len(minter.revoked)
	minter.mu.Unlock()
	if revoked != 0 {
		t.Fatalf("revoke calls after shutdown = %d, want none", revoked)
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
	broker.last["agent-alpha"] = "owner/alpha"
	broker.last["agent-beta"] = "owner/beta"
	broker.served["agent-alpha"] = map[string]struct{}{"owner/alpha": {}}
	broker.served["agent-beta"] = map[string]struct{}{"owner/beta": {}}
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
	broker := newGitCredentialTestBroker(grants, minter, &now, func(context.Context) error { signals++; return nil })
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

func TestGitCredentialBrokerShorterRateLimitHintKeepsLongerBackoff(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	minter := &hintedRateLimitMinter{hints: []time.Duration{5 * time.Minute, 0}, started: make(chan struct{}, 2), release: make(chan struct{})}
	broker := newGitCredentialTestBroker(&fakeGitCredentialGrants{}, minter, &now, nil)
	done := make(chan struct{}, 2)
	for _, key := range []string{"owner/a", "owner/b"} {
		go func() {
			generation := broker.currentGeneration()
			_, _, _ = broker.mint(context.Background(), key, []string{key[len("owner/"):]}, generation)
			done <- struct{}{}
		}()
		<-minter.started
	}
	minter.release <- struct{}{} // the 5-minute hint lands first
	<-done
	minter.release <- struct{}{} // then the no-hint (1-minute floor) response
	<-done
	if _, _, err := broker.mint(context.Background(), "owner/c", []string{"c"}, broker.currentGeneration()); !errors.Is(err, forge.ErrBudgetExhausted) {
		t.Fatalf("mint inside the longer backoff = %v, want budget exhausted", err)
	}
	if got := minter.callCount(); got != 2 {
		t.Fatalf("mint calls = %d, want two (none during backoff)", got)
	}
}

// hintedRateLimitMinter answers each blocked mint with the next rate-limit hint, in release order.
type hintedRateLimitMinter struct {
	mu      sync.Mutex
	hints   []time.Duration
	calls   int
	started chan struct{}
	release chan struct{}
}

func (m *hintedRateLimitMinter) Mint(context.Context, []string, map[string]string) (forge.ScopedToken, error) {
	m.mu.Lock()
	m.calls++
	blocked := m.calls <= 2
	m.mu.Unlock()
	if !blocked {
		return forge.ScopedToken{Token: "ghs_unexpected"}, nil
	}
	m.started <- struct{}{}
	<-m.release
	m.mu.Lock()
	hint := m.hints[0]
	m.hints = m.hints[1:]
	m.mu.Unlock()
	return forge.ScopedToken{}, &forge.RateLimitError{RetryAfter: hint}
}

func (*hintedRateLimitMinter) Revoke(context.Context, string) error {
	return nil
}

func (m *hintedRateLimitMinter) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
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

func TestGitCredentialBrokerMintBurstLimitServesStaleToken(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{}
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_cached"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	for i := range 30 {
		grants.repos = []string{fmt.Sprintf("owner/repo-%d", i)}
		if _, ok := broker.credential(context.Background(), "agent-id"); !ok {
			t.Fatalf("initial credential %d unavailable", i)
		}
	}
	grants.repos = []string{"owner/repo-0"}
	broker.mu.Lock()
	entry := broker.entries["owner/repo-0"]
	entry.refreshAt = now
	broker.entries["owner/repo-0"] = entry
	broker.mu.Unlock()
	if token, ok := broker.credential(context.Background(), "agent-id"); !ok || token != "ghs_cached" {
		t.Fatalf("credential after burst exhaustion = (%q, %v), want stale token", token, ok)
	}
	if got := minter.callCount(); got != 30 {
		t.Fatalf("mint calls after burst exhaustion = %d, want 30", got)
	}
}

func TestGitCredentialBrokerMintLimiterRefillsWithFakeClock(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{}
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_refilled"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	for i := range 30 {
		grants.repos = []string{fmt.Sprintf("owner/repo-%d", i)}
		if _, ok := broker.credential(context.Background(), "agent-id"); !ok {
			t.Fatalf("initial credential %d unavailable", i)
		}
	}
	grants.repos = []string{"owner/refill"}
	if token, ok := broker.credential(context.Background(), "agent-id"); ok || token != "" {
		t.Fatalf("credential at exhausted burst = (%q, %v), want none", token, ok)
	}
	if got := minter.callCount(); got != 30 {
		t.Fatalf("mint calls at exhausted burst = %d, want 30", got)
	}
	now = now.Add(time.Second)
	if token, ok := broker.credential(context.Background(), "agent-id"); !ok || token != "ghs_refilled" {
		t.Fatalf("credential after refill = (%q, %v), want minted token", token, ok)
	}
	if got := minter.callCount(); got != 31 {
		t.Fatalf("mint calls after refill = %d, want 31", got)
	}
}

func TestGitCredentialBrokerRateLimitBackoff(t *testing.T) {
	tests := []struct {
		name string
		hint time.Duration
		wait time.Duration
	}{
		{name: "hint", hint: 2 * time.Minute, wait: 2 * time.Minute},
		{name: "minimum without hint", wait: time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
			grants := &fakeGitCredentialGrants{repos: []string{"owner/repo"}}
			minter := &fakeGitCredentialMinter{
				tokens: []string{"ghs_after_backoff"}, expiresAt: now.Add(time.Hour),
				errByCall: []error{&forge.RateLimitError{RetryAfter: tt.hint}},
			}
			broker := newGitCredentialTestBroker(grants, minter, &now, nil)
			if token, ok := broker.credential(context.Background(), "agent-id"); ok || token != "" {
				t.Fatalf("credential after rate-limit error = (%q, %v), want none", token, ok)
			}
			if token, ok := broker.credential(context.Background(), "agent-id"); ok || token != "" {
				t.Fatalf("credential during backoff = (%q, %v), want none", token, ok)
			}
			if got := minter.callCount(); got != 1 {
				t.Fatalf("mint calls during backoff = %d, want one", got)
			}
			now = now.Add(tt.wait - time.Second)
			if token, ok := broker.credential(context.Background(), "agent-id"); ok || token != "" {
				t.Fatalf("credential before backoff expiry = (%q, %v), want none", token, ok)
			}
			if got := minter.callCount(); got != 1 {
				t.Fatalf("mint calls before backoff expiry = %d, want one", got)
			}
			now = now.Add(time.Second - time.Nanosecond)
			if token, ok := broker.credential(context.Background(), "agent-id"); ok || token != "" {
				t.Fatalf("credential one nanosecond before backoff expiry = (%q, %v), want none", token, ok)
			}
			now = now.Add(time.Nanosecond)
			if token, ok := broker.credential(context.Background(), "agent-id"); !ok || token != "ghs_after_backoff" {
				t.Fatalf("credential after backoff = (%q, %v), want minted token", token, ok)
			}
			if got := minter.callCount(); got != 2 {
				t.Fatalf("mint calls after backoff = %d, want two", got)
			}
		})
	}
}

func TestGitCredentialBrokerSignalsPendingRetryWhenBudgetReturns(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/retry"}}
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_retried"}, expiresAt: now.Add(time.Hour)}
	signals := 0
	ctx := context.WithValue(t.Context(), signalCtxKey{}, "retry")
	broker := newGitCredentialTestBroker(grants, minter, &now, func(got context.Context) error {
		signals++
		if got.Value(signalCtxKey{}) != "retry" {
			t.Error("pending-retry signal lost the refresh ctx")
		}
		return nil
	})
	for range gitCredentialMintBurst {
		if !broker.limiter.AllowN(now, 1) {
			t.Fatal("failed to exhaust mint burst")
		}
	}
	if token, ok := broker.credential(context.Background(), "agent-id"); ok || token != "" {
		t.Fatalf("credential at exhausted burst = (%q, %v), want none", token, ok)
	}
	broker.mu.Lock()
	broker.backoffUntil = now.Add(time.Minute)
	broker.mu.Unlock()
	broker.refreshDue(context.Background())
	if signals != 0 {
		t.Fatalf("signal calls during backoff = %d, want none", signals)
	}
	now = now.Add(time.Minute)
	broker.refreshDue(ctx)
	if signals != 1 {
		t.Fatalf("signal calls after refill = %d, want one", signals)
	}
	broker.refreshDue(context.Background())
	if signals != 1 {
		t.Fatalf("signal calls after pending retry cleared = %d, want one", signals)
	}
}

// signalCtxKey tags the ctx a test hands the broker, to check signal receives it.
type signalCtxKey struct{}

func TestGitCredentialBrokerGrantSnapshotChangedBeforeMint(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/old"}}
	var broker *gitCredentialBroker
	store := &grantChangeOnListStore{
		fakeGitCredentialGrants: grants,
		afterList: func() {
			grants.setSharedRepos([]string{"owner/new"})
			broker.grantsChanged(context.Background(), "agent-id")
		},
	}
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_new", "ghs_old"}, expiresAt: now.Add(time.Hour)}
	broker = newGitCredentialTestBroker(store, minter, &now, nil)
	if token, ok := broker.credential(context.Background(), "agent-id"); ok || token != "" {
		t.Fatalf("credential from stale grant snapshot = (%q, %v), want none", token, ok)
	}
	if _, exists := broker.entries["owner/old"]; exists {
		t.Fatal("old-scope token was cached")
	}
	if got := minter.callCount(); got != 1 {
		t.Fatalf("mint calls = %d, want only fresh-scope mint", got)
	}
}
func TestGitCredentialBrokerGrantsChangedAfterEmptyFetchRevokesLastToken(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/last"}}
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_last"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	if token, ok := broker.credential(context.Background(), "agent-id"); !ok || token != "ghs_last" {
		t.Fatalf("initial credential = (%q, %v), want old token", token, ok)
	}
	grants.setSharedRepos(nil)
	if token, ok := broker.credential(context.Background(), "agent-id"); ok || token != "" {
		t.Fatalf("credential after removing final grant = (%q, %v), want none", token, ok)
	}
	broker.grantsChanged(context.Background(), "agent-id")
	if _, exists := broker.entries["owner/last"]; exists {
		t.Fatal("entry for removed final grant remains")
	}
	if got := minter.revokeCount(); got != 0 {
		t.Fatalf("revoke calls before grace = %d, want none", got)
	}
	now = now.Add(gitCredentialRevokeGrace)
	broker.refreshDue(context.Background())
	minter.mu.Lock()
	revoked := append([]string(nil), minter.revoked...)
	minter.mu.Unlock()
	if !reflect.DeepEqual(revoked, []string{"ghs_last"}) {
		t.Fatalf("revoked tokens = %v, want old token after delivery grace", revoked)
	}
}
func TestGitCredentialBrokerGrantsChangedRevokesRefreshedAndSupersededTokens(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/base", "owner/removed"}}
	minter := &fakeGitCredentialMinter{tokens: []string{"ghs_original", "ghs_refreshed", "ghs_narrow"}, expiresAt: now.Add(time.Hour)}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	if token, ok := broker.credential(context.Background(), "agent-id"); !ok || token != "ghs_original" {
		t.Fatalf("initial credential = (%q, %v), want original token", token, ok)
	}
	oldEntry := broker.entries["owner/base,owner/removed"]
	minter.mu.Lock()
	minter.expiresAt = now.Add(time.Hour)
	minter.mu.Unlock()
	broker.entries["owner/base,owner/removed"] = gitCredentialEntry{
		token: oldEntry.token, expiresAt: oldEntry.expiresAt,
		refreshAt: now, lastUsed: oldEntry.lastUsed,
	}
	if token, ok := broker.credential(context.Background(), "agent-id"); !ok || token != "ghs_refreshed" {
		t.Fatalf("replacement credential = (%q, %v), want refreshed token", token, ok)
	}
	grants.setSharedRepos([]string{"owner/base"})
	broker.grantsChanged(context.Background(), "agent-id")
	if got := minter.revokeCount(); got != 0 {
		t.Fatalf("revoke calls before grace = %d, want none", got)
	}
	now = now.Add(gitCredentialRevokeGrace)
	broker.refreshDue(context.Background())
	minter.mu.Lock()
	revoked := append([]string(nil), minter.revoked...)
	minter.mu.Unlock()
	if len(revoked) != 2 || !slices.Contains(revoked, "ghs_original") || !slices.Contains(revoked, "ghs_refreshed") {
		t.Fatalf("revoked tokens = %v, want original and refreshed tokens after grace", revoked)
	}
}

func TestGitCredentialBrokerRetriesPendingRevokes(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/base", "owner/removed"}}
	minter := &fakeGitCredentialMinter{
		tokens: []string{"ghs_old", "ghs_new"}, expiresAt: now.Add(time.Hour),
		errByCall:       []error{nil, errors.New("narrow mint unavailable")},
		revokeErrByCall: []error{errors.New("revoke unavailable"), nil},
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	if token, ok := broker.credential(context.Background(), "agent-id"); !ok || token != "ghs_old" {
		t.Fatalf("initial credential = (%q, %v), want old token", token, ok)
	}
	grants.setSharedRepos([]string{"owner/base"})
	broker.grantsChanged(context.Background(), "agent-id")
	if got := minter.revokeCount(); got != 0 {
		t.Fatalf("revoke calls before grace = %d, want none", got)
	}
	now = now.Add(gitCredentialRevokeGrace)
	broker.refreshDue(context.Background())
	if len(broker.pendingRevokes) != 1 {
		t.Fatalf("pending revokes after failed retry = %+v, want old token", broker.pendingRevokes)
	}
	broker.refreshDue(context.Background())
	if len(broker.pendingRevokes) != 0 {
		t.Fatalf("pending revokes after successful retry = %+v, want empty", broker.pendingRevokes)
	}
	minter.mu.Lock()
	if got := len(minter.revoked); got != 2 || minter.revoked[0] != "ghs_old" || minter.revoked[1] != "ghs_old" {
		minter.mu.Unlock()
		t.Fatalf("revoke attempts after retry = %v, want old token twice", minter.revoked)
	}
	minter.mu.Unlock()

	expiring := &fakeGitCredentialMinter{
		tokens: []string{"ghs_expiring", "ghs_next"}, expiresAt: now.Add(time.Minute),
		revokeErr: errors.New("revoke unavailable"),
	}
	expiringBroker := newGitCredentialTestBroker(grants, expiring, &now, nil)
	grants.setSharedRepos([]string{"owner/base", "owner/removed"})
	if token, ok := expiringBroker.credential(context.Background(), "agent-id"); !ok || token != "ghs_expiring" {
		t.Fatalf("expiring credential = (%q, %v), want expiring token", token, ok)
	}
	grants.setSharedRepos([]string{"owner/base"})
	expiring.mu.Lock()
	expiring.expiresAt = now.Add(time.Hour)
	expiring.mu.Unlock()
	expiringBroker.grantsChanged(context.Background(), "agent-id")
	expiring.mu.Lock()
	attemptsBeforeExpiry := len(expiring.revoked)
	expiring.revokeErr = nil
	expiring.mu.Unlock()
	if len(expiringBroker.pendingRevokes) != 1 {
		t.Fatalf("pending revoke count = %d, revoked = %v, last = %q, served = %+v, entries = %+v", len(expiringBroker.pendingRevokes), expiring.revoked, expiringBroker.lastCredential("agent-id"), expiringBroker.served, expiringBroker.entries)
	}
	now = now.Add(gitCredentialRevokeGrace + time.Minute)
	expiringBroker.refreshDue(context.Background())
	expiring.mu.Lock()
	defer expiring.mu.Unlock()
	if len(expiring.revoked) != attemptsBeforeExpiry {
		t.Fatalf("revoke attempts after expiry = %v, want no retry", expiring.revoked)
	}
	if len(expiringBroker.pendingRevokes) != 0 {
		t.Fatalf("pending revokes after expiry = %+v, want empty", expiringBroker.pendingRevokes)
	}
}

func TestGitCredentialBrokerDefersGrantRevocationForDeliveryGrace(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	grants := &fakeGitCredentialGrants{repos: []string{"owner/base", "owner/removed"}}
	minter := &fakeGitCredentialMinter{
		tokens: []string{"ghs_old", "ghs_narrow"}, expiresAt: now.Add(time.Hour),
	}
	broker := newGitCredentialTestBroker(grants, minter, &now, nil)
	if token, ok := broker.credential(context.Background(), "agent-id"); !ok || token != "ghs_old" {
		t.Fatalf("initial credential = (%q, %v), want old token", token, ok)
	}
	grants.setSharedRepos([]string{"owner/base"})
	broker.grantsChanged(context.Background(), "agent-id")
	if got := minter.revokeCount(); got != 0 {
		t.Fatalf("revoke calls before grace = %d, want none", got)
	}
	now = now.Add(gitCredentialRevokeGrace)
	broker.refreshDue(context.Background())
	minter.mu.Lock()
	revoked := append([]string(nil), minter.revoked...)
	minter.mu.Unlock()
	if !reflect.DeepEqual(revoked, []string{"ghs_old"}) {
		t.Fatalf("revoked tokens after grace = %v, want old token exactly once", revoked)
	}
}
