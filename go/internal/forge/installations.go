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

const installationCacheTTL = 10 * time.Minute

// InstallationResolver maps GitHub owner logins to App installation ids.
// Refreshes are cached and concurrent refreshes are singleflighted.
type InstallationResolver struct {
	source *appTokenSource

	mu            sync.Mutex
	installations map[string]int64
	expiresAt     time.Time
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
// case-insensitively. The owner map, including misses, is cached for ten minutes.
func (r *InstallationResolver) InstallationFor(ctx context.Context, owner string) (int64, error) {
	if installations, ok := r.cachedInstallations(); ok {
		return installationFor(installations, owner)
	}

	value, err, _ := r.group.Do("installations", func() (any, error) {
		if installations, ok := r.cachedInstallations(); ok {
			return installations, nil
		}
		installations, err := r.source.listInstallations(ctx)
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.installations = installations
		r.expiresAt = r.source.clock().Add(installationCacheTTL)
		r.mu.Unlock()
		return installations, nil
	})
	if err != nil {
		return 0, fmt.Errorf("forge: resolve installation for owner %q: %w", owner, err)
	}
	installations, ok := value.(map[string]int64)
	if !ok {
		return 0, fmt.Errorf("forge: unexpected installation refresh result %T", value)
	}
	return installationFor(installations, owner)
}

func installationFor(installations map[string]int64, owner string) (int64, error) {
	if id, ok := installations[strings.ToLower(owner)]; ok {
		return id, nil
	}
	return 0, fmt.Errorf("forge: owner %q: %w", owner, ErrNoInstallation)
}

func (r *InstallationResolver) cachedInstallations() (map[string]int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.installations == nil || !r.source.clock().Before(r.expiresAt) {
		return nil, false
	}
	return r.installations, true
}

func (s *appTokenSource) listInstallations(ctx context.Context) (map[string]int64, error) {
	jwt, err := s.appJWT(ctx)
	if err != nil {
		return nil, err
	}

	installations := make(map[string]int64)
	for page := 1; ; page++ {
		url := fmt.Sprintf("%s/app/installations?per_page=%d&page=%d", s.apiBase, perPage, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("forge: build installation list request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := s.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("forge: list app installations: %w", err)
		}
		body, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil || closeErr != nil {
			var closeWrapped error
			if closeErr != nil {
				closeWrapped = fmt.Errorf("forge: close installation list response: %w", closeErr)
			}
			if readErr != nil {
				return nil, errors.Join(fmt.Errorf("forge: read installation list response: %w", readErr), closeWrapped)
			}
			return nil, closeWrapped
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			var ghErr ghError
			if jsonErr := json.Unmarshal(body, &ghErr); jsonErr == nil && ghErr.Message != "" {
				return nil, fmt.Errorf("forge: list app installations: %w", &StatusError{Status: resp.StatusCode, Message: ghErr.Message})
			}
			return nil, fmt.Errorf("forge: list app installations: %w", &StatusError{Status: resp.StatusCode})
		}

		var pageInstallations []struct {
			ID      int64 `json:"id"`
			Account struct {
				Login string `json:"login"`
			} `json:"account"`
		}
		if err := json.Unmarshal(body, &pageInstallations); err != nil {
			return nil, fmt.Errorf("forge: decode installation list response: %w", err)
		}
		for _, installation := range pageInstallations {
			if installation.ID > 0 && installation.Account.Login != "" {
				installations[strings.ToLower(installation.Account.Login)] = installation.ID
			}
		}
		if !hasNextLink(resp.Header.Get("Link")) {
			return installations, nil
		}
	}
}
