package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInstallationResolverMapsOwnersAndPaginates(t *testing.T) {
	var requests atomic.Int64
	var pagesMu sync.Mutex
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/app/installations" {
			t.Errorf("request = %s %s, want GET /app/installations", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("authorization = %q, want an App JWT", r.Header.Get("Authorization"))
		}
		pagesMu.Lock()
		pages = append(pages, r.URL.Query().Get("page"))
		pagesMu.Unlock()
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", `<https://api.github.com/app/installations?per_page=100&page=2>; rel="next"`)
			if _, err := fmt.Fprint(w, `[{"id":159353126,"account":{"login":"RigelBuild"}}]`); err != nil {
				t.Errorf("write page 1: %v", err)
			}
			return
		}
		if _, err := fmt.Fprint(w, `[{"id":168669446,"account":{"login":"second-owner"}}]`); err != nil {
			t.Errorf("write page 2: %v", err)
		}
	}))
	defer server.Close()

	key := testAppKey(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	resolver := newTestInstallationResolver(t, server, GitHubAppConfig{
		AppID: 1, InstallationID: 2,
		PrivateKey: func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
		Clock:      func() time.Time { return now },
	})

	id, err := resolver.InstallationFor(context.Background(), "SeCoNd-OwNeR")
	if err != nil || id != 168669446 {
		t.Fatalf("InstallationFor(mixed case) = (%d, %v), want (168669446, nil)", id, err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("installation requests = %d, want two paginated requests", got)
	}
	pagesMu.Lock()
	if len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
		t.Fatalf("requested pages = %v, want [1 2]", pages)
	}
	pagesMu.Unlock()
}

func TestInstallationResolverUnknownOwnerAndCachedHit(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Query().Get("page") != "1" {
			t.Errorf("page = %q, want 1", r.URL.Query().Get("page"))
		}
		if _, err := fmt.Fprint(w, `[{"id":159353126,"account":{"login":"RigelBuild"}}]`); err != nil {
			t.Errorf("write installation response: %v", err)
		}
	}))
	defer server.Close()

	key := testAppKey(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	resolver := newTestInstallationResolver(t, server, GitHubAppConfig{
		AppID:      1,
		PrivateKey: func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
		Clock:      func() time.Time { return now },
	})

	id, err := resolver.InstallationFor(context.Background(), "rigelbuild")
	if err != nil || id != 159353126 {
		t.Fatalf("InstallationFor(rigelbuild) = (%d, %v), want (159353126, nil)", id, err)
	}
	if _, err := resolver.InstallationFor(context.Background(), "missing-owner"); !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("InstallationFor(unknown owner) error = %v, want ErrNoInstallation", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests after cached hit and unknown owner = %d, want 1", got)
	}
}

func TestInstallationResolverCachedMissRefreshesAfterFloor(t *testing.T) {
	var requests atomic.Int64
	var installed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if installed.Load() {
			if _, err := fmt.Fprint(w, `[{"id":168669446,"account":{"login":"second-owner"}}]`); err != nil {
				t.Errorf("write installation response: %v", err)
			}
			return
		}
		if _, err := fmt.Fprint(w, `[]`); err != nil {
			t.Errorf("write empty installation response: %v", err)
		}
	}))
	defer server.Close()

	key := testAppKey(t)
	clock := &installationTestClock{now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	resolver := newTestInstallationResolver(t, server, GitHubAppConfig{
		AppID:      1,
		PrivateKey: func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
		Clock:      clock.Now,
	})
	for range 2 {
		if _, err := resolver.InstallationFor(context.Background(), "second-owner"); !errors.Is(err, ErrNoInstallation) {
			t.Fatalf("initial miss = %v, want ErrNoInstallation", err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests inside refresh floor = %d, want 1", got)
	}
	installed.Store(true)
	clock.Add(installationMissRefreshFloor)
	if id, err := resolver.InstallationFor(context.Background(), "second-owner"); err != nil || id != 168669446 {
		t.Fatalf("InstallationFor after floor = (%d, %v), want installation 168669446", id, err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests after refresh floor = %d, want 2", got)
	}
}

type installationTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *installationTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *installationTestClock) Add(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func TestInstallationResolverWaiterContextCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		close(started)
		<-release
		if _, err := fmt.Fprint(w, `[{"id":168669446,"account":{"login":"second-owner"}}]`); err != nil {
			t.Errorf("write installation response: %v", err)
		}
	}))
	defer server.Close()
	key := testAppKey(t)
	clock := &installationTestClock{now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	resolver := newTestInstallationResolver(t, server, GitHubAppConfig{
		AppID:      1,
		PrivateKey: func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
		Clock:      clock.Now,
	})

	leaderDone := make(chan error, 1)
	go func() {
		_, err := resolver.InstallationFor(context.Background(), "second-owner")
		leaderDone <- err
	}()
	<-started
	waiterCtx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := resolver.InstallationFor(waiterCtx, "second-owner")
		waiterDone <- err
	}()
	cancel()
	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not return after its context was canceled")
	}
	close(release)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader refresh: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("installation requests = %d, want 1", got)
	}
}

func TestInstallationResolverLeaderContextCancellationDoesNotFailWaiter(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(started)
		<-release
		if _, err := fmt.Fprint(w, `[{"id":168669446,"account":{"login":"second-owner"}}]`); err != nil {
			t.Errorf("write installation response: %v", err)
		}
	}))
	defer server.Close()
	key := testAppKey(t)
	clock := &installationTestClock{now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	resolver := newTestInstallationResolver(t, server, GitHubAppConfig{
		AppID:      1,
		PrivateKey: func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
		Clock:      clock.Now,
	})

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := resolver.InstallationFor(leaderCtx, "second-owner")
		leaderDone <- err
	}()
	<-started
	waiterDone := make(chan error, 1)
	go func() {
		id, err := resolver.InstallationFor(context.Background(), "second-owner")
		if err == nil && id != 168669446 {
			err = fmt.Errorf("installation id = %d, want 168669446", id)
		}
		waiterDone <- err
	}()
	cancelLeader()
	select {
	case err := <-leaderDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("leader error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("leader did not return after its context was canceled")
	}
	close(release)
	select {
	case err := <-waiterDone:
		if err != nil {
			t.Fatalf("waiter refresh: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not receive the shared refresh")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("installation requests = %d, want 1", got)
	}
}

func TestInstallationResolverInstallationListRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "15")
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	key := testAppKey(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	resolver := newTestInstallationResolver(t, server, GitHubAppConfig{
		AppID:      1,
		PrivateKey: func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
		Clock:      func() time.Time { return now },
	})
	_, err := resolver.InstallationFor(context.Background(), "owner")
	rateLimit, ok := errors.AsType[*RateLimitError](err)
	if !ok {
		t.Fatalf("InstallationFor error = %v, want RateLimitError", err)
	}
	if rateLimit.RetryAfter != 15*time.Second {
		t.Fatalf("RetryAfter = %v, want 15s", rateLimit.RetryAfter)
	}
}

func TestInstallationResolverFailedRefreshNeverServesExpiredSnapshot(t *testing.T) {
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if _, err := fmt.Fprint(w, `[{"id":7,"account":{"login":"owner"}}]`); err != nil {
			t.Errorf("write installation response: %v", err)
		}
	}))
	defer server.Close()
	key := testAppKey(t)
	clock := &installationTestClock{now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	resolver := newTestInstallationResolver(t, server, GitHubAppConfig{
		AppID:      1,
		PrivateKey: func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
		Clock:      clock.Now,
	})
	if id, err := resolver.InstallationFor(context.Background(), "owner"); err != nil || id != 7 {
		t.Fatalf("initial lookup = (%d, %v), want 7", id, err)
	}
	fail.Store(true)
	clock.Add(installationCacheTTL)
	if _, err := resolver.InstallationFor(context.Background(), "owner"); err == nil {
		t.Fatal("lookup with failing refresh succeeded, want an error")
	}
	clock.Add(time.Second)
	if _, err := resolver.InstallationFor(context.Background(), "owner"); !errors.Is(err, errInstallationRefreshThrottled) {
		t.Fatalf("lookup inside retry floor = %v, want errInstallationRefreshThrottled, not the expired snapshot", err)
	}
}

func TestInstallationResolverRejectsOversizedPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprint(w, "["+strings.Repeat(" ", installationPageBodyLimit)+"]"); err != nil {
			t.Errorf("write oversized response: %v", err)
		}
	}))
	defer server.Close()
	key := testAppKey(t)
	resolver := newTestInstallationResolver(t, server, GitHubAppConfig{
		AppID:      1,
		PrivateKey: func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
	})
	if _, err := resolver.InstallationFor(context.Background(), "owner"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("InstallationFor error = %v, want oversized response error", err)
	}
}

func TestInstallationResolverClassifiesStatusErrors(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		credentials bool
	}{
		{"unauthorized", http.StatusUnauthorized, true},
		{"forbidden without rate limit", http.StatusForbidden, true},
		{"server error", http.StatusInternalServerError, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			key := testAppKey(t)
			resolver := newTestInstallationResolver(t, server, GitHubAppConfig{
				AppID:      1,
				PrivateKey: func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
			})
			_, err := resolver.InstallationFor(context.Background(), "owner")
			if got := errors.Is(err, ErrAppCredentials); got != tc.credentials {
				t.Fatalf("errors.Is(ErrAppCredentials) = %v for %v, want %v", got, err, tc.credentials)
			}
			if statusErr, ok := errors.AsType[*StatusError](err); !ok || statusErr.Status != tc.status {
				t.Fatalf("StatusError = %v, want status %d", err, tc.status)
			}
		})
	}
}

func TestInstallationResolverCapsPages(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Link", `<https://api.github.com/app/installations?per_page=100&page=2>; rel="next"`)
		if _, err := fmt.Fprint(w, `[]`); err != nil {
			t.Errorf("write empty installation response: %v", err)
		}
	}))
	defer server.Close()
	key := testAppKey(t)
	resolver := newTestInstallationResolver(t, server, GitHubAppConfig{
		AppID:      1,
		PrivateKey: func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
	})
	_, err := resolver.InstallationFor(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), "exceeds 100 pages") {
		t.Fatalf("InstallationFor error = %v, want page limit error", err)
	}
	if got := requests.Load(); got != installationListPageLimit {
		t.Fatalf("installation pages requested = %d, want %d", got, installationListPageLimit)
	}
}

func TestInstallationResolverMissCacheRefreshFloorAndHitTTL(t *testing.T) {
	var requests atomic.Int64
	var installed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if installed.Load() {
			if _, err := fmt.Fprint(w, `[{"id":168669446,"account":{"login":"second-owner"}}]`); err != nil {
				t.Errorf("write installation response: %v", err)
			}
			return
		}
		if _, err := fmt.Fprint(w, `[]`); err != nil {
			t.Errorf("write empty installation response: %v", err)
		}
	}))
	defer server.Close()
	key := testAppKey(t)
	clock := &installationTestClock{now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	resolver := newTestInstallationResolver(t, server, GitHubAppConfig{
		AppID:      1,
		PrivateKey: func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
		Clock:      clock.Now,
	})
	if _, err := resolver.InstallationFor(context.Background(), "second-owner"); !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("initial miss = %v, want ErrNoInstallation", err)
	}
	clock.Add(installationMissRefreshFloor - time.Second)
	if _, err := resolver.InstallationFor(context.Background(), "second-owner"); !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("miss inside refresh floor = %v, want ErrNoInstallation", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests inside floor = %d, want 1", got)
	}
	installed.Store(true)
	clock.Add(time.Second)
	if id, err := resolver.InstallationFor(context.Background(), "second-owner"); err != nil || id != 168669446 {
		t.Fatalf("InstallationFor after floor = (%d, %v), want 168669446", id, err)
	}
	clock.Add(installationCacheTTL - installationMissRefreshFloor)
	if id, err := resolver.InstallationFor(context.Background(), "second-owner"); err != nil || id != 168669446 {
		t.Fatalf("cached hit before TTL = (%d, %v), want 168669446", id, err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests before hit TTL = %d, want 2", got)
	}
	// Past the hit TTL the snapshot refreshes, so an uninstall is seen.
	installed.Store(false)
	clock.Add(installationMissRefreshFloor)
	if _, err := resolver.InstallationFor(context.Background(), "second-owner"); !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("lookup after hit TTL = %v, want ErrNoInstallation from a refreshed snapshot", err)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("requests after hit TTL = %d, want 3", got)
	}
}

func TestScopedAppMinterMintForUsesResolvedInstallation(t *testing.T) {
	var installationRequests atomic.Int64
	var mintPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			installationRequests.Add(1)
			if r.URL.Path != "/app/installations" {
				t.Errorf("installation path = %q, want /app/installations", r.URL.Path)
			}
			if _, err := fmt.Fprint(w, `[{"id":168669446,"account":{"login":"second-owner"}}]`); err != nil {
				t.Errorf("write installation response: %v", err)
			}
		case http.MethodPost:
			mintPath = r.URL.Path
			w.WriteHeader(http.StatusCreated)
			if _, err := fmt.Fprint(w, `{"token":"ghs_owner","expires_at":"2026-10-17T13:00:00Z","repositories":[{"full_name":"second-owner/workstream"}]}`); err != nil {
				t.Errorf("write mint response: %v", err)
			}
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	key := testAppKey(t)

	client := testAppHTTPClient(t, server)
	minter, err := NewScopedAppMinter(GitHubAppConfig{
		AppID:          1,
		InstallationID: 159353126, // the default; MintFor must not use it
		PrivateKey:     func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
		Client:         client,
	})
	if err != nil {
		t.Fatalf("NewScopedAppMinter: %v", err)
	}
	result, err := minter.MintFor(
		context.Background(),
		"second-owner",
		[]string{"workstream"},
		map[string]string{"contents": "write"},
	)
	if err != nil {
		t.Fatalf("MintFor: %v", err)
	}
	if result.Token != "ghs_owner" {
		t.Fatalf("MintFor token = %q, want ghs_owner", result.Token)
	}
	if want := "/app/installations/168669446/access_tokens"; mintPath != want {
		t.Fatalf("mint path = %q, want %q", mintPath, want)
	}
	if got := installationRequests.Load(); got != 1 {
		t.Fatalf("installation list requests = %d, want one", got)
	}
}

func newTestInstallationResolver(t *testing.T, server *httptest.Server, cfg GitHubAppConfig) *InstallationResolver {
	t.Helper()
	cfg.Client = testAppHTTPClient(t, server)
	resolver, err := NewInstallationResolver(cfg)
	if err != nil {
		t.Fatalf("NewInstallationResolver: %v", err)
	}
	return resolver
}

func testAppHTTPClient(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	return &http.Client{Transport: &rewriteTransport{target: target}}
}
