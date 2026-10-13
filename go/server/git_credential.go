package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
)

const (
	gitCredentialSecretName    = "GITHUB_APP_TOKEN" //nolint:gosec // secret NAME, not a value
	gitCredentialReason        = "runner fetch"
	gitCredentialMaxRepos      = 500
	gitCredentialMaxAge        = 90 * time.Minute
	gitCredentialMinStale      = 10 * time.Minute
	gitCredentialRefreshLead   = 15 * time.Minute
	gitCredentialMintTimeout   = 30 * time.Second
	gitCredentialMismatchCache = 5 * time.Minute
)

var (
	errGitCredentialScopeMismatch = errors.New("GitHub granted repository scope differs from requested scope")
	errGitCredentialScopeCached   = errors.New("GitHub repository scope recently rejected")
)

var gitCredentialPermissions = map[string]string{
	"contents":      "write",
	"pull_requests": "write",
	"metadata":      "read",
}

type gitCredentialMinter interface {
	Mint(ctx context.Context, repos []string, perms map[string]string) (forge.ScopedToken, error)
}

type gitCredentialStore interface {
	AgentOwner(ctx context.Context, agentAccountID store.AccountID) (store.AccountID, error)
	ListForgeScopeRepos(ctx context.Context, accountID store.AccountID, provider store.ForgeProvider, host string) ([]string, error)
}

// gitCredentialScopeRejected reports a mint failure that repeats for the same set.
func gitCredentialScopeRejected(err error) bool {
	if errors.Is(err, errGitCredentialScopeMismatch) || errors.Is(err, errGitCredentialScopeCached) {
		return true
	}
	if statusErr, ok := errors.AsType[*forge.StatusError](err); ok {
		return statusErr.Status == 404 || statusErr.Status == 422
	}
	return false
}

type agentSecretResolver interface {
	ResolveFor(ctx context.Context, agent store.AccountID, reason string) ([]secrets.ResolvedSecret, error)
}

type gitCredentialEntry struct {
	token     string
	expiresAt time.Time
	refreshAt time.Time
	lastUsed  time.Time
}

type gitCredentialNegativeEntry struct {
	expiresAt time.Time
}

type gitCredentialFlightResult struct {
	token  string
	minted bool
}

type gitCredentialBroker struct {
	host   string
	store  gitCredentialStore
	minter gitCredentialMinter
	log    *slog.Logger
	signal func() error
	clock  func() time.Time

	mu       sync.Mutex
	entries  map[string]gitCredentialEntry
	negative map[string]gitCredentialNegativeEntry
	last     map[store.AccountID]string
	group    singleflight.Group
}

type brokeredSecretResolver struct {
	inner  agentSecretResolver
	broker *gitCredentialBroker
}

func (r *brokeredSecretResolver) ResolveFor(ctx context.Context, agent store.AccountID, reason string) ([]secrets.ResolvedSecret, error) {
	if r.inner == nil {
		return nil, nil
	}
	resolved, err := r.inner.ResolveFor(ctx, agent, reason)
	resolved = slices.DeleteFunc(resolved, func(secret secrets.ResolvedSecret) bool {
		return secret.Kind == secrets.SecretGH || secrets.IsReservedGitHubEnvName(secret.Name)
	})
	if err != nil || r.broker == nil || reason != gitCredentialReason {
		return resolved, err
	}
	tok, ok := r.broker.credential(ctx, agent)
	if !ok {
		return resolved, nil
	}
	return append(resolved, secrets.ResolvedSecret{
		Name:     gitCredentialSecretName,
		Value:    tok,
		Version:  secrets.Version(tok),
		Delivery: secrets.DeliveryFile,
		Kind:     secrets.SecretGH,
		Host:     r.broker.host,
	}), nil
}

func (b *gitCredentialBroker) credential(ctx context.Context, agent store.AccountID) (string, bool) {
	repos, err := b.store.ListForgeScopeRepos(ctx, agent, store.ForgeProviderGitHub, b.host)
	if err != nil {
		b.log.ErrorContext(ctx, "list GitHub credential grants", "error", err)
		return "", false
	}
	tok, servedKey, rejected := b.credentialForRepos(ctx, agent, repos)
	if !rejected {
		b.recordLastCredential(agent, servedKey)
		return tok, servedKey != ""
	}
	owner, err := b.store.AgentOwner(ctx, agent)
	if err != nil {
		b.log.ErrorContext(ctx, "resolve GitHub credential owner", "agent", agent, "error", err)
		return "", false
	}
	ownerRepos, err := b.store.ListForgeScopeRepos(ctx, owner, store.ForgeProviderGitHub, b.host)
	if err != nil {
		b.log.ErrorContext(ctx, "list owner GitHub credential grants", "owner", owner, "error", err)
		return "", false
	}
	ownerKey, _, ownerOK := gitCredentialScope(ownerRepos)
	if !ownerOK || len(ownerRepos) == 0 || ownerKey == gitCredentialScopeKey(repos) {
		return "", false
	}
	tok, servedKey, _ = b.credentialForRepos(ctx, agent, ownerRepos)
	b.recordLastCredential(agent, servedKey)
	return tok, servedKey != ""
}

// credentialForRepos returns the token and the cache key that supplied it.
func (b *gitCredentialBroker) credentialForRepos(ctx context.Context, agent store.AccountID, repos []string) (tok, servedKey string, rejected bool) {
	if len(repos) == 0 {
		return "", "", false
	}
	key, names, valid := gitCredentialScope(repos)
	if !valid {
		b.log.WarnContext(ctx, "omit GitHub credential for invalid grant scope", "agent", agent)
		return "", "", true
	}
	if len(names) > gitCredentialMaxRepos {
		b.log.WarnContext(ctx, "omit GitHub credential for oversized grant scope", "agent", agent, "repositories", len(names))
		return "", "", true
	}
	now := b.clock()
	b.mu.Lock()
	if negative, exists := b.negative[key]; exists {
		if now.Before(negative.expiresAt) {
			b.mu.Unlock()
			return "", "", true
		}
		delete(b.negative, key)
	}
	if entry, exists := b.entries[key]; exists {
		entry.lastUsed = now
		b.entries[key] = entry
		if now.Before(entry.refreshAt) {
			b.mu.Unlock()
			return entry.token, key, false
		}
	}
	b.mu.Unlock()

	value, err, _ := b.group.Do(key, func() (any, error) {
		if b.negativeCredential(key) {
			return gitCredentialFlightResult{}, errGitCredentialScopeCached
		}
		if tok, ok := b.cachedCredential(key); ok {
			return gitCredentialFlightResult{token: tok}, nil
		}
		mintCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitCredentialMintTimeout)
		defer cancel()
		tok, replaced, err := b.mint(mintCtx, key, names)
		if err == nil && replaced && b.signal != nil {
			if signalErr := b.signal(); signalErr != nil {
				b.log.ErrorContext(ctx, "signal replaced GitHub credential", "error", signalErr)
			}
		}
		return gitCredentialFlightResult{token: tok}, err
	})
	if err != nil {
		b.log.ErrorContext(ctx, "mint scoped GitHub credential", "scope", key, "error", err)
		if gitCredentialScopeRejected(err) {
			return "", "", true
		}
		if tok = b.staleCredential(key); tok != "" {
			return tok, key, false
		}
		tok, servedKey = b.credentialForLastKey(agent, key)
		return tok, servedKey, false
	}
	result, validResult := value.(gitCredentialFlightResult)
	if !validResult {
		b.log.ErrorContext(ctx, "unexpected GitHub credential mint result", "scope", key)
		if tok = b.staleCredential(key); tok != "" {
			return tok, key, false
		}
		return "", "", false
	}
	if result.token == "" {
		return "", "", false
	}
	return result.token, key, false
}

func (b *gitCredentialBroker) cachedCredential(key string) (string, bool) {
	now := b.clock()
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.entries[key]
	if !ok {
		return "", false
	}
	entry.lastUsed = now
	b.entries[key] = entry
	return entry.token, now.Before(entry.refreshAt)
}
func (b *gitCredentialBroker) negativeCredential(key string) bool {
	now := b.clock()
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.negative[key]
	if !ok {
		return false
	}
	if now.Before(entry.expiresAt) {
		return true
	}
	delete(b.negative, key)
	return false
}

func (b *gitCredentialBroker) staleCredential(key string) string {
	now := b.clock()
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.entries[key]
	if !ok || entry.expiresAt.Sub(now) < gitCredentialMinStale {
		return ""
	}
	entry.lastUsed = now
	b.entries[key] = entry
	return entry.token
}

func gitCredentialScope(grants []string) (string, []string, bool) {
	if slices.Contains(grants, "*") {
		return "*", nil, true
	}
	qualified := make([]string, 0, len(grants))
	owner := ""
	for _, grant := range grants {
		parts := strings.Split(grant, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", nil, false
		}
		grantOwner := strings.ToLower(parts[0])
		if owner == "" {
			owner = grantOwner
		} else if owner != grantOwner {
			return "", nil, false
		}
		qualified = append(qualified, strings.ToLower(parts[0])+"/"+strings.ToLower(parts[1]))
	}
	sort.Strings(qualified)
	names := make([]string, len(qualified))
	for i, grant := range qualified {
		names[i] = strings.SplitN(grant, "/", 2)[1]
	}
	return strings.Join(qualified, ","), names, true
}

func gitCredentialScopeKey(repos []string) string {
	key, _, ok := gitCredentialScope(repos)
	if !ok {
		return ""
	}
	return key
}

// recordLastCredential records key as the agent's last served key while it still has a cached token.
func (b *gitCredentialBroker) recordLastCredential(agent store.AccountID, key string) {
	if agent == "" || key == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, cached := b.entries[key]; cached {
		b.last[agent] = key
	}
}

func (b *gitCredentialBroker) lastCredential(agent store.AccountID) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last[agent]
}

// credentialForLastKey returns the stale token of the agent's last served key, and that key,
// only when that key grants no repository outside the requested key.
func (b *gitCredentialBroker) credentialForLastKey(agent store.AccountID, requested string) (string, string) {
	key := b.lastCredential(agent)
	if key == "" || !gitCredentialScopeWithin(key, requested) {
		return "", ""
	}
	if tok := b.staleCredential(key); tok != "" {
		return tok, key
	}
	return "", ""
}

// gitCredentialScopeWithin reports whether every repository in scope key inner is in key outer.
func gitCredentialScopeWithin(inner, outer string) bool {
	if outer == "*" {
		return true
	}
	if inner == "*" {
		return false
	}
	granted := strings.Split(outer, ",")
	for qualified := range strings.SplitSeq(inner, ",") {
		if !slices.Contains(granted, qualified) {
			return false
		}
	}
	return true
}

func (b *gitCredentialBroker) mint(ctx context.Context, key string, repos []string) (string, bool, error) {
	token, err := b.minter.Mint(ctx, repos, gitCredentialPermissions)
	if err != nil {
		if gitCredentialScopeRejected(err) {
			b.mu.Lock()
			b.negative[key] = gitCredentialNegativeEntry{expiresAt: b.clock().Add(gitCredentialMismatchCache)}
			b.mu.Unlock()
		}
		return "", false, err
	}
	if token.Token == "" {
		return "", false, nil
	}
	if key != "*" && !sameRepositorySet(strings.Split(key, ","), token.Repositories) {
		b.mu.Lock()
		b.negative[key] = gitCredentialNegativeEntry{expiresAt: b.clock().Add(gitCredentialMismatchCache)}
		b.mu.Unlock()
		return "", false, errGitCredentialScopeMismatch
	}
	now := b.clock()
	expiresAt := token.ExpiresAt
	if expiresAt.IsZero() || !expiresAt.After(now) {
		expiresAt = now.Add(time.Hour)
	}
	b.mu.Lock()
	lastUsed := now
	previous, existed := b.entries[key]
	if existed {
		lastUsed = previous.lastUsed
	}
	delete(b.negative, key)
	b.entries[key] = gitCredentialEntry{
		token: token.Token, expiresAt: expiresAt,
		refreshAt: expiresAt.Add(-gitCredentialRefreshLead), lastUsed: lastUsed,
	}
	b.mu.Unlock()
	return token.Token, existed && previous.token != token.Token, nil
}

func sameRepositorySet(want, got []string) bool {
	normalize := func(repositories []string) []string {
		result := make([]string, len(repositories))
		for i, repository := range repositories {
			result[i] = strings.ToLower(repository)
		}
		sort.Strings(result)
		return slices.Compact(result)
	}
	return slices.Equal(normalize(want), normalize(got))
}

func (b *gitCredentialBroker) refreshDue(ctx context.Context) {
	now := b.clock()
	b.mu.Lock()
	keys := make([]string, 0, len(b.entries))
	previousTokens := make(map[string]string, len(b.entries))
	for key, entry := range b.entries {
		if now.Sub(entry.lastUsed) > gitCredentialMaxAge {
			delete(b.entries, key)
			for agent, lastKey := range b.last {
				if lastKey == key {
					delete(b.last, agent)
				}
			}
			continue
		}
		if !now.Before(entry.refreshAt) {
			keys = append(keys, key)
			previousTokens[key] = entry.token
		}
	}
	for key, entry := range b.negative {
		if !now.Before(entry.expiresAt) {
			delete(b.negative, key)
		}
	}
	b.mu.Unlock()
	sort.Strings(keys)
	b.refreshSnapshot(ctx, keys, previousTokens)
}

func (b *gitCredentialBroker) refreshSnapshot(ctx context.Context, keys []string, previousTokens map[string]string) {
	changed := false
	for _, key := range keys {
		if ctx.Err() != nil {
			break
		}
		if b.refreshCredential(ctx, key, previousTokens[key]) {
			changed = true
		}
	}
	if changed && b.signal != nil {
		if err := b.signal(); err != nil {
			b.log.ErrorContext(ctx, "signal refreshed GitHub credential", "error", err)
		}
	}
}

func (b *gitCredentialBroker) refreshCredential(ctx context.Context, key, previous string) bool {
	value, err, _ := b.group.Do(key, func() (any, error) {
		if b.negativeCredential(key) {
			return gitCredentialFlightResult{}, errGitCredentialScopeCached
		}
		b.mu.Lock()
		entry, ok := b.entries[key]
		if !ok || entry.token != previous || b.clock().Before(entry.refreshAt) {
			b.mu.Unlock()
			return gitCredentialFlightResult{token: entry.token}, nil
		}
		b.mu.Unlock()
		var repos []string
		if key != "*" {
			for qualified := range strings.SplitSeq(key, ",") {
				parts := strings.SplitN(qualified, "/", 2)
				if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
					return gitCredentialFlightResult{}, errors.New("invalid cached GitHub scope")
				}
				repos = append(repos, parts[1])
			}
		}
		mintCtx, cancel := context.WithTimeout(ctx, gitCredentialMintTimeout)
		defer cancel()
		token, _, err := b.mint(mintCtx, key, repos)
		return gitCredentialFlightResult{token: token, minted: err == nil && token != ""}, err
	})
	if err != nil {
		b.log.ErrorContext(ctx, "refresh scoped GitHub credential", "scope", key, "error", err)
		if gitCredentialScopeRejected(err) {
			b.mu.Lock()
			if entry, exists := b.entries[key]; exists && entry.token == previous {
				delete(b.entries, key)
			}
			b.mu.Unlock()
		}
		return false
	}
	result, ok := value.(gitCredentialFlightResult)
	return ok && result.minted && result.token != previous
}

func (b *gitCredentialBroker) run(ctx context.Context, tick <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			b.refreshDue(ctx)
		}
	}
}

func buildGitCredentialBroker(cfg ServeConfig, st *store.Store, resolver secrets.Resolver, hub *runnerhub.Hub, log *slog.Logger) (*gitCredentialBroker, error) {
	if !cfg.Forge.boardIngestionEnabled() {
		return nil, nil //nolint:nilnil // nil broker means App not configured
	}
	rc := cfg.Forge.resolved()
	client, err := forgeHTTPClient(rc.ForgeCAPath)
	if err != nil {
		return nil, fmt.Errorf("GitHub credential App HTTP client: %w", err)
	}
	minter, err := forge.NewScopedAppMinter(forge.GitHubAppConfig{
		AppID:          rc.App.AppID,
		InstallationID: rc.App.InstallationID,
		PrivateKey:     newDeclaredSecretResolver(resolver, rc.App.AppPrivateKeySecret),
		Host:           rc.Host,
		Client:         client,
	})
	if err != nil {
		return nil, fmt.Errorf("GitHub credential App minter: %w", err)
	}
	return &gitCredentialBroker{
		host: rc.Host, store: st, minter: minter, log: log,
		signal: hub.SignalSecretsVersion, clock: time.Now,
		entries: make(map[string]gitCredentialEntry), negative: make(map[string]gitCredentialNegativeEntry),
		last: make(map[store.AccountID]string),
	}, nil
}

// startGitCredentialRefresh refreshes brokered App tokens on the serve group;
// a nil broker (no GitHub App) starts nothing.
func startGitCredentialRefresh(ctx context.Context, group *errgroup.Group, broker *gitCredentialBroker) {
	if broker == nil {
		return
	}
	ticker := time.NewTicker(time.Minute)
	group.Go(func() error {
		defer ticker.Stop()
		broker.run(ctx, ticker.C)
		return nil
	})
}
