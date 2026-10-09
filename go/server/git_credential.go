package server

import (
	"context"
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
	gitCredentialSecretName = "GITHUB_APP_TOKEN" //nolint:gosec // secret NAME, not a value
	gitCredentialReason     = "runner fetch"
	gitCredentialMaxRepos   = 500
	gitCredentialMaxAge     = 90 * time.Minute
)

var gitCredentialPermissions = map[string]string{
	"contents":      "write",
	"pull_requests": "write",
	"metadata":      "read",
}

type gitCredentialMinter interface {
	Mint(ctx context.Context, repos []string, perms map[string]string) (string, time.Time, error)
}

type gitCredentialGrantLister interface {
	ListForgeScopeRepos(ctx context.Context, accountID store.AccountID, provider store.ForgeProvider, host string) ([]string, error)
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

type gitCredentialBroker struct {
	host   string
	grants gitCredentialGrantLister
	minter gitCredentialMinter
	log    *slog.Logger
	signal func() error
	clock  func() time.Time

	mu      sync.Mutex
	entries map[string]gitCredentialEntry
	group   singleflight.Group
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
	if err != nil || r.broker == nil || reason != gitCredentialReason || hasGitHubSecret(resolved, r.broker.host) {
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

func hasGitHubSecret(resolved []secrets.ResolvedSecret, host string) bool {
	for _, secret := range resolved {
		if secret.Kind == secrets.SecretGH && secret.Host == host {
			return true
		}
	}
	return false
}

func (b *gitCredentialBroker) credential(ctx context.Context, agent store.AccountID) (string, bool) {
	repos, err := b.grants.ListForgeScopeRepos(ctx, agent, store.ForgeProviderGitHub, b.host)
	if err != nil {
		b.log.ErrorContext(ctx, "list GitHub credential grants", "error", err)
		return "", false
	}
	if len(repos) == 0 {
		return "", false
	}
	key, names, ok := gitCredentialScope(repos)
	if !ok {
		b.log.WarnContext(ctx, "omit GitHub credential for invalid grant scope", "agent", agent)
		return "", false
	}
	if len(names) > gitCredentialMaxRepos {
		b.log.WarnContext(ctx, "omit GitHub credential for oversized grant scope", "agent", agent, "repositories", len(names))
		return "", false
	}
	now := b.clock()
	b.mu.Lock()
	if entry, exists := b.entries[key]; exists {
		entry.lastUsed = now
		b.entries[key] = entry
		if now.Before(entry.refreshAt) {
			b.mu.Unlock()
			return entry.token, true
		}
	}
	b.mu.Unlock()

	value, err, _ := b.group.Do(key, func() (any, error) {
		if tok, ok := b.cachedCredential(key); ok {
			return tok, nil
		}
		return b.mint(ctx, key, names)
	})
	if err != nil {
		b.log.ErrorContext(ctx, "mint scoped GitHub credential", "scope", key, "error", err)
		tok := b.staleCredential(key)
		return tok, tok != ""
	}
	tok, ok := value.(string)
	if !ok {
		b.log.ErrorContext(ctx, "unexpected GitHub credential mint result", "scope", key)
		tok := b.staleCredential(key)
		return tok, tok != ""
	}
	return tok, tok != ""
}

func gitCredentialScope(grants []string) (string, []string, bool) {
	if slices.Contains(grants, "*") {
		return "*", nil, true
	}
	owner := ""
	names := make([]string, 0, len(grants))
	for _, grant := range grants {
		parts := strings.Split(grant, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", nil, false
		}
		if owner == "" {
			owner = parts[0]
		} else if owner != parts[0] {
			return "", nil, false
		}
		names = append(names, parts[1])
	}
	sort.Strings(names)
	return strings.Join(names, ","), names, true
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

func (b *gitCredentialBroker) staleCredential(key string) string {
	now := b.clock()
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.entries[key]
	if !ok || !now.Before(entry.expiresAt) {
		return ""
	}
	entry.lastUsed = now
	b.entries[key] = entry
	return entry.token
}

func (b *gitCredentialBroker) mint(ctx context.Context, key string, repos []string) (string, error) {
	tok, expiresAt, err := b.minter.Mint(ctx, repos, gitCredentialPermissions)
	if err != nil {
		return "", err
	}
	if tok == "" {
		return "", nil
	}
	now := b.clock()
	if expiresAt.IsZero() || !expiresAt.After(now) {
		expiresAt = now.Add(time.Hour)
	}
	b.mu.Lock()
	lastUsed := now
	if previous, ok := b.entries[key]; ok {
		lastUsed = previous.lastUsed
	}
	b.entries[key] = gitCredentialEntry{
		token: tok, expiresAt: expiresAt,
		refreshAt: expiresAt.Add(-5 * time.Minute), lastUsed: lastUsed,
	}
	b.mu.Unlock()
	return tok, nil
}

func (b *gitCredentialBroker) refreshDue(ctx context.Context) {
	now := b.clock()
	b.mu.Lock()
	keys := make([]string, 0, len(b.entries))
	previousTokens := make(map[string]string, len(b.entries))
	for key, entry := range b.entries {
		if now.Sub(entry.lastUsed) > gitCredentialMaxAge {
			delete(b.entries, key)
			continue
		}
		if !now.Before(entry.refreshAt) {
			keys = append(keys, key)
			previousTokens[key] = entry.token
		}
	}
	b.mu.Unlock()
	sort.Strings(keys)
	changed := false
	for _, key := range keys {
		value, err, _ := b.group.Do(key, func() (any, error) {
			b.mu.Lock()
			_, ok := b.entries[key]
			b.mu.Unlock()
			if !ok {
				return "", nil
			}
			var repos []string
			if key != "*" {
				repos = strings.Split(key, ",")
			}
			return b.mint(ctx, key, repos)
		})
		if err != nil {
			b.log.ErrorContext(ctx, "refresh scoped GitHub credential", "scope", key, "error", err)
			continue
		}
		if tok, ok := value.(string); ok && tok != "" && tok != previousTokens[key] {
			changed = true
		}
	}
	if changed && b.signal != nil {
		if err := b.signal(); err != nil {
			b.log.ErrorContext(ctx, "signal refreshed GitHub credential", "error", err)
		}
	}
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
		host: rc.Host, grants: st, minter: minter, log: log,
		signal: hub.SignalSecretsVersion, clock: time.Now,
		entries: make(map[string]gitCredentialEntry),
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
