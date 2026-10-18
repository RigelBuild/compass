package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// ErrNoInstallation indicates that the GitHub App is not installed for an owner.
var ErrNoInstallation = errors.New("forge: no installation")

// ErrAppCredentials indicates that GitHub App credentials cannot authenticate.
var ErrAppCredentials = errors.New("forge: app credentials rejected or misconfigured")

const (
	installationCacheTTL         = 10 * time.Minute
	installationMissRefreshFloor = 30 * time.Second
	installationRefreshTimeout   = 30 * time.Second
	installationListPageLimit    = 100
)

// InstallationResolver maps GitHub owner logins to App installation ids.
// Refreshes are cached and concurrent refreshes are singleflighted.
type InstallationResolver struct {
	source *appTokenSource

	mu            sync.Mutex
	installations map[string]int64
	snapshotAt    time.Time
	lastAttemptAt time.Time
	group         singleflight.Group
}

// NewInstallationResolver constructs an App installation lookup using the
// configured App identity, key resolver, host, HTTP client, and clock.
func NewInstallationResolver(cfg GitHubAppConfig) (*InstallationResolver, error) {
	source, err := newAppTokenSource(cfg, false)
	if err != nil {
		return nil, err
	}
	return &InstallationResolver{source: source}, nil
}

// InstallationFor returns the installation id for owner, matching GitHub logins
// case-insensitively. Hits are cached for ten minutes; misses refresh every 30s.
func (r *InstallationResolver) InstallationFor(ctx context.Context, owner string) (int64, error) {
	key := strings.ToLower(owner)
	if installations, snapshotAt, ok := r.snapshot(); ok && snapshotFresh(installations, snapshotAt, key, r.source.clock()) {
		return installationFor(installations, owner)
	}

	resultCh := r.group.DoChan("installations", func() (any, error) {
		now := r.source.clock()
		r.mu.Lock()
		installations, snapshotAt := r.installations, r.snapshotAt
		if installations != nil && snapshotFresh(installations, snapshotAt, key, now) {
			r.mu.Unlock()
			return installations, nil
		}
		// A refresh failed within the floor; never answer from an expired snapshot.
		if !r.lastAttemptAt.IsZero() && now.Before(r.lastAttemptAt.Add(installationMissRefreshFloor)) {
			r.mu.Unlock()
			return nil, errInstallationRefreshThrottled
		}
		r.lastAttemptAt = now
		r.mu.Unlock()

		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), installationRefreshTimeout)
		defer cancel()
		installations, err := r.source.listInstallations(refreshCtx)
		r.mu.Lock()
		if err == nil {
			r.installations = installations
			r.snapshotAt = r.source.clock()
		}
		r.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return installations, nil
	})

	var result singleflight.Result
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case result = <-resultCh:
	}
	if result.Err != nil {
		return 0, fmt.Errorf("forge: resolve installation for owner %q: %w", owner, result.Err)
	}
	installations, ok := result.Val.(map[string]int64)
	if !ok {
		return 0, fmt.Errorf("forge: unexpected installation refresh result %T", result.Val)
	}
	return installationFor(installations, owner)
}

// errInstallationRefreshThrottled reports a lookup inside the retry floor after a failed refresh.
var errInstallationRefreshThrottled = errors.New("forge: installation refresh failed recently; retry shortly")

// snapshotFresh reports whether the snapshot can answer key: a hit within the
// TTL, or a miss within the miss-refresh floor.
func snapshotFresh(installations map[string]int64, snapshotAt time.Time, key string, now time.Time) bool {
	if _, found := installations[key]; found {
		return now.Before(snapshotAt.Add(installationCacheTTL))
	}
	return now.Before(snapshotAt.Add(installationMissRefreshFloor))
}

// DefaultInstallation returns the configured default installation id, or 0.
func (r *InstallationResolver) DefaultInstallation() int64 {
	return r.source.cfg.InstallationID
}

func noInstallation(owner string) error {
	return fmt.Errorf("forge: owner %q: %w", owner, ErrNoInstallation)
}

func installationFor(installations map[string]int64, owner string) (int64, error) {
	if id, ok := installations[strings.ToLower(owner)]; ok {
		return id, nil
	}
	return 0, noInstallation(owner)
}

func (r *InstallationResolver) snapshot() (map[string]int64, time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.installations, r.snapshotAt, r.installations != nil
}

func (s *appTokenSource) listInstallations(ctx context.Context) (map[string]int64, error) {
	jwt, err := s.appJWT(ctx)
	if err != nil {
		return nil, err
	}

	installations := make(map[string]int64)
	for page := 1; page <= installationListPageLimit; page++ {
		pageInstallations, hasNext, err := s.installationPage(ctx, jwt, page)
		if err != nil {
			return nil, err
		}
		for _, installation := range pageInstallations {
			if installation.ID > 0 && installation.Account.Login != "" {
				installations[strings.ToLower(installation.Account.Login)] = installation.ID
			}
		}
		if !hasNext {
			return installations, nil
		}
	}
	return nil, fmt.Errorf("forge: app installation list exceeds %d pages", installationListPageLimit)
}

type appInstallation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
	} `json:"account"`
}

func (s *appTokenSource) installationPage(ctx context.Context, jwt string, page int) ([]appInstallation, bool, error) {
	url := fmt.Sprintf("%s/app/installations?per_page=%d&page=%d", s.apiBase, perPage, page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("forge: build installation list request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("forge: list app installations: %w", err)
	}
	body, err := readInstallationResponseBody(resp)
	if err != nil {
		return nil, false, err
	}
	if err := installationResponseError(resp, body, s.clock()); err != nil {
		return nil, false, err
	}
	var result []appInstallation
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, false, fmt.Errorf("forge: decode installation list response: %w", err)
	}
	return result, hasNextLink(resp.Header.Get("Link")), nil
}

// installationPageBodyLimit bounds one installation-list page (100 entries is ~50 KiB).
const installationPageBodyLimit = 1 << 20

func readInstallationResponseBody(resp *http.Response) ([]byte, error) {
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, installationPageBodyLimit+1))
	if readErr == nil && len(body) > installationPageBodyLimit {
		readErr = fmt.Errorf("response exceeds %d bytes", installationPageBodyLimit)
	}
	closeErr := resp.Body.Close()
	if readErr != nil {
		wrappedReadErr := fmt.Errorf("forge: read installation list response: %w", readErr)
		if closeErr != nil {
			return nil, errors.Join(wrappedReadErr, fmt.Errorf("forge: close installation list response: %w", closeErr))
		}
		return nil, wrappedReadErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("forge: close installation list response: %w", closeErr)
	}
	return body, nil
}

func installationResponseError(resp *http.Response, body []byte, now time.Time) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	rateLimited := isRateLimited(resp)
	credentialRejected := resp.StatusCode == http.StatusUnauthorized ||
		(resp.StatusCode == http.StatusForbidden && !rateLimited)
	if credentialRejected {
		return fmt.Errorf("forge: list app installations: %w", errors.Join(ErrAppCredentials, &StatusError{Status: resp.StatusCode}))
	}
	rateLimitStatus := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden
	if rateLimitStatus && rateLimited {
		var retryAfter time.Duration
		if reset := rateLimitResetAt(resp, now); !reset.IsZero() {
			retryAfter = max(reset.Sub(now), 0)
		}
		return fmt.Errorf("forge: list app installations: %w", &RateLimitError{RetryAfter: retryAfter})
	}
	var ghErr ghError
	if jsonErr := json.Unmarshal(body, &ghErr); jsonErr == nil && ghErr.Message != "" {
		return fmt.Errorf("forge: list app installations: %w", &StatusError{Status: resp.StatusCode, Message: ghErr.Message})
	}
	return fmt.Errorf("forge: list app installations: %w", &StatusError{Status: resp.StatusCode})
}
