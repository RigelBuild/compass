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
		if _, err := fmt.Fprint(w, `[{"id":168669446,"account":{"login":"mattwilkinsonn"}}]`); err != nil {
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

	id, err := resolver.InstallationFor(context.Background(), "mAtTwIlKiNsOnN")
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

func TestInstallationResolverCachedMissRefreshesAfterTTL(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if _, err := fmt.Fprint(w, `[]`); err != nil {
			t.Errorf("write empty installation response: %v", err)
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

	for range 2 {
		if _, err := resolver.InstallationFor(context.Background(), "unknown"); !errors.Is(err, ErrNoInstallation) {
			t.Fatalf("InstallationFor(unknown) error = %v, want ErrNoInstallation", err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests for repeated cached miss = %d, want 1", got)
	}

	now = now.Add(installationCacheTTL)
	if _, err := resolver.InstallationFor(context.Background(), "unknown"); !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("InstallationFor(unknown after TTL) error = %v, want ErrNoInstallation", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests after TTL = %d, want 2", got)
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
			if _, err := fmt.Fprint(w, `[{"id":168669446,"account":{"login":"mattwilkinsonn"}}]`); err != nil {
				t.Errorf("write installation response: %v", err)
			}
		case http.MethodPost:
			mintPath = r.URL.Path
			w.WriteHeader(http.StatusCreated)
			if _, err := fmt.Fprint(w, `{"token":"ghs_owner","expires_at":"2026-10-17T13:00:00Z","repositories":[{"full_name":"mattwilkinsonn/zireael"}]}`); err != nil {
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
	result, err := minter.MintFor(context.Background(), "mattwilkinsonn", []string{"zireael"}, map[string]string{"contents": "write"})
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
