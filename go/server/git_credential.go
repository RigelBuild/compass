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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"

	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
)

const (
	gitCredentialSecretName  = "GITHUB_APP_TOKEN" //nolint:gosec // secret NAME, not a value
	gitCredentialReason      = "runner fetch"
	gitCredentialMaxRepos    = 500
	gitCredentialMaxAge      = 90 * time.Minute
	gitCredentialMinStale    = 10 * time.Minute
	gitCredentialMintTimeout = 30 * time.Second
	// The signal has no delivery acknowledgement, so this bounds the replacement token's delivery window.
	gitCredentialRevokeGrace = 30 * time.Second
	// Keep pending revokes within one tick of their delivery grace deadline.
	gitCredentialRefreshTick   = 10 * time.Second
	gitCredentialRefreshLead   = 15 * time.Minute
	gitCredentialMismatchCache = 5 * time.Minute
	gitCredentialMintRate      = rate.Limit(1)
	gitCredentialMintBurst     = 30
	gitCredentialRateBackoff   = time.Minute
)

var (
	errGitCredentialScopeMismatch = errors.New("GitHub granted repository scope differs from requested scope")
	errGitCredentialScopeCached   = errors.New("GitHub repository scope recently rejected")
	errGitCredentialGrantsChanged = errors.New("GitHub credential grants changed during mint")
)

var gitCredentialPermissions = map[string]string{
	"contents":      "write",
	"pull_requests": "write",
	"metadata":      "read",
}

type gitCredentialMinter interface {
	Mint(ctx context.Context, repos []string, perms map[string]string) (forge.ScopedToken, error)
	MintFor(ctx context.Context, owner string, repos []string, perms map[string]string) (forge.ScopedToken, error)
	Revoke(ctx context.Context, token string) error
}

type gitCredentialStore interface {
	AgentOwner(ctx context.Context, agentAccountID store.AccountID) (store.AccountID, error)
	ListForgeScopeRepos(ctx context.Context, accountID store.AccountID, provider store.ForgeProvider, host string) ([]string, error)
}

// gitCredentialScopeRejected reports a mint failure that repeats for the same set.
func gitCredentialScopeRejected(err error) bool {
	if errors.Is(err, errGitCredentialScopeMismatch) || errors.Is(err, errGitCredentialScopeCached) || errors.Is(err, forge.ErrNoInstallation) {
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

type supersededToken struct {
	token     string
	expiresAt time.Time
	notBefore time.Time
}

type gitCredentialEntry struct {
	token      string
	expiresAt  time.Time
	refreshAt  time.Time
	lastUsed   time.Time
	superseded []supersededToken
}

type gitCredentialNegativeEntry struct {
	expiresAt time.Time
}

type gitCredentialFlightResult struct {
	token  string
	minted bool
}

type gitCredentialBroker struct {
	host    string
	store   gitCredentialStore
	minter  gitCredentialMinter
	log     *slog.Logger
	signal  func(context.Context) error
	clock   func() time.Time
	limiter *rate.Limiter

	mu             sync.Mutex
	entries        map[string]gitCredentialEntry
	negative       map[string]gitCredentialNegativeEntry
	last           map[store.AccountID]string
	served         map[store.AccountID]map[string]struct{}
	pendingRevokes []supersededToken
	generation     uint64
	backoffUntil   time.Time
	retryPending   bool
	group          singleflight.Group
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
	b.mu.Lock()
	gen := b.generation
	b.mu.Unlock()
	repos, err := b.store.ListForgeScopeRepos(ctx, agent, store.ForgeProviderGitHub, b.host)
	if err != nil {
		b.log.ErrorContext(ctx, "list GitHub credential grants", "error", err)
		return "", false
	}
	tok, servedKey, rejected := b.credentialForRepos(ctx, agent, repos, gen)
	if !rejected {
		return b.recordCredentialIfCurrent(agent, servedKey, tok, gen)
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
	tok, servedKey, _ = b.credentialForRepos(ctx, agent, ownerRepos, gen)
	return b.recordCredentialIfCurrent(agent, servedKey, tok, gen)
}

func (b *gitCredentialBroker) recordCredentialIfCurrent(agent store.AccountID, key, token string, generation uint64) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.generation != generation || token == "" || key == "" {
		return "", false
	}
	if entry, ok := b.entries[key]; !ok || entry.token != token {
		return "", false
	}
	b.last[agent] = key
	if b.served[agent] == nil {
		b.served[agent] = make(map[string]struct{})
	}
	b.served[agent][key] = struct{}{}
	return token, true
}

// Keep lookup separate from recording so callers can reject credentials from stale grant snapshots.
func (b *gitCredentialBroker) credentialForRepos(ctx context.Context, agent store.AccountID, repos []string, generation uint64) (tok, servedKey string, rejected bool) {
	if len(repos) == 0 {
		b.clearLastCredential(agent)
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
	if b.currentGeneration() != generation {
		return "", "", false
	}
	if tok, ok := b.credentialFromCache(key); ok {
		if b.currentGeneration() != generation {
			return "", "", false
		}
		return tok, key, false
	}
	value, err, _ := b.group.Do(key, func() (any, error) {
		if b.negativeCredential(key) {
			return gitCredentialFlightResult{}, errGitCredentialScopeCached
		}
		if tok, ok := b.cachedCredential(key); ok {
			if b.currentGeneration() != generation {
				return gitCredentialFlightResult{}, errGitCredentialGrantsChanged
			}
			return gitCredentialFlightResult{token: tok}, nil
		}
		mintCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitCredentialMintTimeout)
		defer cancel()
		tok, replaced, err := b.mint(mintCtx, key, names, generation)
		if err == nil && replaced && b.signal != nil {
			if signalErr := b.signal(ctx); signalErr != nil {
				b.log.ErrorContext(ctx, "signal replaced GitHub credential", "error", signalErr)
			}
		}
		return gitCredentialFlightResult{token: tok}, err
	})
	if err != nil {
		b.log.ErrorContext(ctx, "mint scoped GitHub credential", "scope", key, "error", err)
		if errors.Is(err, errGitCredentialGrantsChanged) {
			return "", "", false
		}
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
	if b.currentGeneration() != generation {
		return "", "", false
	}
	return result.token, key, false
}
func (b *gitCredentialBroker) credentialFromCache(key string) (string, bool) {
	now := b.clock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if negative, exists := b.negative[key]; exists {
		if now.Before(negative.expiresAt) {
			return "", false
		}
		delete(b.negative, key)
	}
	if entry, exists := b.entries[key]; exists {
		entry.lastUsed = now
		b.entries[key] = entry
		if now.Before(entry.refreshAt) {
			return entry.token, true
		}
	}
	return "", false
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

func (b *gitCredentialBroker) grantsChanged(ctx context.Context, agent store.AccountID) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.generation++
	b.mu.Unlock()

	repos, err := b.store.ListForgeScopeRepos(ctx, agent, store.ForgeProviderGitHub, b.host)
	requestedKey := ""
	if err != nil {
		b.log.ErrorContext(ctx, "list GitHub credential grants after change", "agent", agent, "error", err)
	} else if len(repos) > 0 {
		requestedKey, _, _ = gitCredentialScope(repos)
	}

	var revokeTokens []supersededToken
	b.mu.Lock()
	for key := range b.served[agent] {
		if requestedKey != "" && gitCredentialScopeWithin(key, requestedKey) {
			continue
		}
		if entry, exists := b.entries[key]; exists {
			if entry.token != "" {
				revokeTokens = append(revokeTokens, supersededToken{token: entry.token, expiresAt: entry.expiresAt})
			}
			revokeTokens = append(revokeTokens, entry.superseded...)
		}
		delete(b.entries, key)
		b.dropServedFor(key)
		b.dropLastFor(key)
	}
	b.mu.Unlock()

	if err == nil {
		b.credential(ctx, agent)
	}
	if b.signal != nil {
		if err := b.signal(ctx); err != nil {
			b.log.ErrorContext(ctx, "signal GitHub credential grant change", "agent", agent, "error", err)
		}
	}
	for _, token := range revokeTokens {
		token.notBefore = b.clock().Add(gitCredentialRevokeGrace)
		b.queueRevoke(token)
	}
}

func (b *gitCredentialBroker) currentGeneration() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.generation
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
	innerRest, outerRest := inner, outer
	for innerRest != "" {
		innerRepo, nextInnerRepo, hasNextInner := strings.Cut(innerRest, ",")
		if innerRepo == "" || (hasNextInner && nextInnerRepo == "") {
			return false
		}
		innerRest = nextInnerRepo
		matched := false
		for outerRest != "" {
			outerRepo, nextOuterRepo, hasNextOuter := strings.Cut(outerRest, ",")
			if outerRepo == "" {
				return false
			}
			if outerRepo >= innerRepo {
				if outerRepo != innerRepo {
					return false
				}
				outerRest = nextOuterRepo
				if !hasNextOuter {
					outerRest = ""
				}
				matched = true
				break
			}
			outerRest = nextOuterRepo
			if !hasNextOuter {
				outerRest = ""
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func (b *gitCredentialBroker) mint(ctx context.Context, key string, repos []string, generation uint64) (string, bool, error) {
	now := b.clock()
	b.mu.Lock()
	if b.generation != generation {
		b.mu.Unlock()
		return "", false, fmt.Errorf("discard GitHub credential for stale grant snapshot: %w", errGitCredentialGrantsChanged)
	}
	backedOff := now.Before(b.backoffUntil)
	b.mu.Unlock()
	if backedOff {
		b.mu.Lock()
		b.retryPending = true
		b.mu.Unlock()
		return "", false, fmt.Errorf("GitHub credential mint backoff: %w", forge.ErrBudgetExhausted)
	}
	if b.limiter == nil || !b.limiter.AllowN(now, 1) {
		b.mu.Lock()
		b.retryPending = true
		b.mu.Unlock()
		return "", false, fmt.Errorf("GitHub credential mint budget: %w", forge.ErrBudgetExhausted)
	}
	var token forge.ScopedToken
	var err error
	if key == "*" {
		token, err = b.minter.Mint(ctx, repos, gitCredentialPermissions)
	} else {
		firstRepo, _, _ := strings.Cut(key, ",")
		owner, _, _ := strings.Cut(firstRepo, "/")
		token, err = b.minter.MintFor(ctx, owner, repos, gitCredentialPermissions)
	}
	if err != nil {
		b.mu.Lock()
		if b.generation != generation {
			b.mu.Unlock()
			return "", false, fmt.Errorf("discard late GitHub credential after grants changed: %w", errGitCredentialGrantsChanged)
		}
		if rateErr, ok := errors.AsType[*forge.RateLimitError](err); ok {
			backoff := max(rateErr.RetryAfter, gitCredentialRateBackoff)
			if until := b.clock().Add(backoff); until.After(b.backoffUntil) {
				b.backoffUntil = until
			}
		}
		if gitCredentialScopeRejected(err) {
			b.negative[key] = gitCredentialNegativeEntry{expiresAt: b.clock().Add(gitCredentialMismatchCache)}
		}
		b.mu.Unlock()
		return "", false, err
	}
	if token.Token == "" {
		return "", false, nil
	}
	if key != "*" && !sameRepositorySet(strings.Split(key, ","), token.Repositories) {
		b.mu.Lock()
		if b.generation != generation {
			b.mu.Unlock()
			b.revokeLateMint(ctx, token.Token, token.ExpiresAt)
			return "", false, fmt.Errorf("discard late GitHub credential after grants changed: %w", errGitCredentialGrantsChanged)
		}
		b.negative[key] = gitCredentialNegativeEntry{expiresAt: b.clock().Add(gitCredentialMismatchCache)}
		b.mu.Unlock()
		return "", false, errGitCredentialScopeMismatch
	}
	now = b.clock()
	expiresAt := token.ExpiresAt
	if expiresAt.IsZero() || !expiresAt.After(now) {
		expiresAt = now.Add(time.Hour)
	}
	b.mu.Lock()
	if b.generation != generation {
		b.mu.Unlock()
		b.revokeLateMint(ctx, token.Token, expiresAt)
		return "", false, fmt.Errorf("discard late GitHub credential after grants changed: %w", errGitCredentialGrantsChanged)
	}
	lastUsed := now
	previous, existed := b.entries[key]
	superseded := make([]supersededToken, 0, len(previous.superseded)+1)
	if existed {
		lastUsed = previous.lastUsed
		superseded = append(superseded, previous.superseded...)
		if previous.token != "" && previous.token != token.Token && now.Before(previous.expiresAt) {
			superseded = append(superseded, supersededToken{token: previous.token, expiresAt: previous.expiresAt})
		}
	}
	delete(b.negative, key)
	b.entries[key] = gitCredentialEntry{
		token: token.Token, expiresAt: expiresAt,
		refreshAt: expiresAt.Add(-gitCredentialRefreshLead), lastUsed: lastUsed,
		superseded: superseded,
	}
	b.mu.Unlock()
	return token.Token, existed && previous.token != token.Token, nil
}

func (b *gitCredentialBroker) revokeOrPend(ctx context.Context, agent store.AccountID, token supersededToken, reason string) {
	if token.token == "" || !b.clock().Before(token.expiresAt) {
		return
	}
	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitCredentialMintTimeout)
	err := b.minter.Revoke(revokeCtx, token.token)
	cancel()
	if err == nil {
		return
	}
	b.log.ErrorContext(ctx, reason, "agent", agent, "error", err)
	b.queueRevoke(token)
}

func (b *gitCredentialBroker) queueRevoke(token supersededToken) {
	b.mu.Lock()
	b.pendingRevokes = append(b.pendingRevokes, token)
	b.mu.Unlock()
}

func (b *gitCredentialBroker) retryPendingRevokes(ctx context.Context, now time.Time) {
	b.mu.Lock()
	pending := b.pendingRevokes
	b.pendingRevokes = nil
	b.mu.Unlock()
	var retry []supersededToken
	for _, token := range pending {
		if !now.Before(token.expiresAt) {
			continue
		}
		if now.Before(token.notBefore) {
			retry = append(retry, token)
			continue
		}
		revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitCredentialMintTimeout)
		err := b.minter.Revoke(revokeCtx, token.token)
		cancel()
		if err != nil {
			b.log.ErrorContext(ctx, "retry GitHub credential revoke", "error", err)
			retry = append(retry, token)
		}
	}
	if len(retry) > 0 {
		b.mu.Lock()
		b.pendingRevokes = append(b.pendingRevokes, retry...)
		b.mu.Unlock()
	}
}

func (b *gitCredentialBroker) revokeLateMint(ctx context.Context, token string, expiresAt time.Time) {
	now := b.clock()
	if expiresAt.IsZero() || !expiresAt.After(now) {
		expiresAt = now.Add(time.Hour)
	}
	b.revokeOrPend(ctx, "", supersededToken{token: token, expiresAt: expiresAt}, "revoke GitHub credential minted after grant change")
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

// instrumentationScope names this package's tracer, as delivery and comms do.
const instrumentationScope = "github.com/RigelBuild/compass/go/server"

func (b *gitCredentialBroker) refreshDue(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	ctx, span := otel.Tracer(instrumentationScope).Start(ctx, "server.gitcredential.refresh", trace.WithNewRoot())
	defer span.End()
	now := b.clock()
	b.retryPendingRevokes(ctx, now)
	b.mu.Lock()
	keys := make([]string, 0, len(b.entries))
	previousTokens := make(map[string]string, len(b.entries))
	for key, entry := range b.negative {
		if !now.Before(entry.expiresAt) {
			delete(b.negative, key)
		}
	}
	for key, entry := range b.entries {
		superseded := entry.superseded[:0]
		for _, token := range entry.superseded {
			if now.Before(token.expiresAt) {
				superseded = append(superseded, token)
			}
		}
		entry.superseded = superseded
		b.entries[key] = entry
		if !b.hasServedKey(key) {
			delete(b.entries, key)
			continue
		}
		if now.Sub(entry.lastUsed) > gitCredentialMaxAge {
			delete(b.entries, key)
			b.dropLastFor(key)
			b.dropServedFor(key)
			continue
		}
		if !now.Before(entry.refreshAt) {
			keys = append(keys, key)
			previousTokens[key] = entry.token
		}
	}
	retryPending := b.retryPending && !now.Before(b.backoffUntil) && b.limiter != nil && b.limiter.TokensAt(now) >= 1
	if retryPending {
		b.retryPending = false
	}
	b.mu.Unlock()
	sort.Strings(keys)
	b.refreshSnapshot(ctx, keys, previousTokens)
	if retryPending && b.signal != nil {
		if err := b.signal(ctx); err != nil {
			b.log.ErrorContext(ctx, "signal GitHub credential retry", "error", err)
			b.mu.Lock()
			b.retryPending = true
			b.mu.Unlock()
		}
	}
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
		if err := b.signal(ctx); err != nil {
			b.log.ErrorContext(ctx, "signal refreshed GitHub credential", "error", err)
		}
	}
}

func (b *gitCredentialBroker) dropLastFor(key string) {
	for agent, lastKey := range b.last {
		if lastKey == key {
			delete(b.last, agent)
		}
	}
}

func (b *gitCredentialBroker) hasServedKey(key string) bool {
	for _, keys := range b.served {
		if _, exists := keys[key]; exists {
			return true
		}
	}
	return false
}

func (b *gitCredentialBroker) dropServedFor(key string) {
	for agent, keys := range b.served {
		delete(keys, key)
		if len(keys) == 0 {
			delete(b.served, agent)
		}
	}
}

func (b *gitCredentialBroker) clearLastCredential(agent store.AccountID) {
	b.mu.Lock()
	delete(b.last, agent)
	b.mu.Unlock()
}
func (b *gitCredentialBroker) refreshCredential(ctx context.Context, key, previous string) bool {
	generation := b.currentGeneration()
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
		token, _, err := b.mint(mintCtx, key, repos, generation)
		return gitCredentialFlightResult{token: token, minted: err == nil && token != ""}, err
	})
	if err != nil {
		if !errors.Is(err, errGitCredentialScopeCached) {
			b.log.ErrorContext(ctx, "refresh scoped GitHub credential", "scope", key, "error", err)
		}
		if gitCredentialScopeRejected(err) {
			b.mu.Lock()
			removed := false
			if entry, exists := b.entries[key]; exists && entry.token == previous {
				delete(b.entries, key)
				b.dropLastFor(key)
				removed = true
			}
			b.mu.Unlock()
			return removed
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

func buildGitCredentialBroker(cfg ServeConfig, st *store.Store, resolver secrets.Resolver, installations *forge.InstallationResolver, hub *runnerhub.Hub, log *slog.Logger) (*gitCredentialBroker, error) {
	if !cfg.Forge.boardIngestionEnabled() {
		return nil, nil //nolint:nilnil // nil broker means App not configured
	}
	rc := cfg.Forge.resolved()
	client, err := forgeHTTPClient(rc.ForgeCAPath)
	if err != nil {
		return nil, fmt.Errorf("GitHub credential App HTTP client: %w", err)
	}
	minter, err := forge.NewScopedAppMinterWithResolver(forge.GitHubAppConfig{
		AppID:          rc.App.AppID,
		InstallationID: rc.App.InstallationID,
		PrivateKey:     newDeclaredSecretResolver(resolver, rc.App.AppPrivateKeySecret),
		Host:           rc.Host,
		Client:         client,
	}, installations)
	if err != nil {
		return nil, fmt.Errorf("GitHub credential App minter: %w", err)
	}
	return &gitCredentialBroker{
		host: rc.Host, store: st, minter: minter, log: log,
		signal: hub.SignalSecretsVersion, clock: time.Now,
		limiter: rate.NewLimiter(gitCredentialMintRate, gitCredentialMintBurst),
		entries: make(map[string]gitCredentialEntry), negative: make(map[string]gitCredentialNegativeEntry),
		last:   make(map[store.AccountID]string),
		served: make(map[store.AccountID]map[string]struct{}),
	}, nil
}

// buildRepoInstallationResolver resolves owners through one shared App installation snapshot.
func buildRepoInstallationResolver(cfg ServeConfig, resolver secrets.Resolver) (*forge.InstallationResolver, error) {
	if !cfg.Forge.boardIngestionEnabled() {
		return nil, nil //nolint:nilnil // nil resolver means App not configured
	}
	rc := cfg.Forge.resolved()
	client, err := forgeHTTPClient(rc.ForgeCAPath)
	if err != nil {
		return nil, fmt.Errorf("GitHub repository installation HTTP client: %w", err)
	}
	installations, err := forge.NewInstallationResolver(forge.GitHubAppConfig{
		AppID:          rc.App.AppID,
		InstallationID: rc.App.InstallationID,
		PrivateKey:     newDeclaredSecretResolver(resolver, rc.App.AppPrivateKeySecret),
		Host:           rc.Host,
		Client:         client,
	})
	if err != nil {
		return nil, fmt.Errorf("GitHub repository installation resolver: %w", err)
	}
	return installations, nil
}

// startGitCredentialRefresh refreshes brokered App tokens on the serve group;
// a nil broker (no GitHub App) starts nothing.
func startGitCredentialRefresh(ctx context.Context, group *errgroup.Group, broker *gitCredentialBroker) {
	if broker == nil {
		return
	}
	ticker := time.NewTicker(gitCredentialRefreshTick)
	group.Go(func() error {
		defer ticker.Stop()
		broker.run(ctx, ticker.C)
		return nil
	})
}
