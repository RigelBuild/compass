package forge

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type routedIdentityTokenSource struct {
	token string
	login string
}

func (s routedIdentityTokenSource) Token(context.Context) (string, error) { return s.token, nil }
func (routedIdentityTokenSource) Invalidate()                             {}
func (s routedIdentityTokenSource) AppLogin(context.Context) (string, error) {
	return s.login, nil
}

func TestOwnerRoutedGitHubRoutesByInstallation(t *testing.T) {
	var defaultWrites, ownerBWrites int
	var sawOwnerBToken bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			if _, err := fmt.Fprint(w, `[{"id":7,"account":{"login":"owner-a"}},{"id":8,"account":{"login":"owner-b"}}]`); err != nil {
				t.Errorf("write installation list: %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/8/access_tokens":
			w.Header().Set("Content-Type", "application/json")
			if _, err := fmt.Fprint(w, `{"token":"owner-b-installation-token","expires_at":"2030-01-01T00:00:00Z"}`); err != nil {
				t.Errorf("write token response: %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner-a/repo/issues":
			defaultWrites++
			if r.Header.Get("Authorization") != "Bearer primary-default-token" {
				t.Error("default owner did not use the primary client token")
			}
			w.Header().Set("X-Ratelimit-Remaining", "0")
			w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			if _, err := fmt.Fprint(w, `{"number":1,"title":"created","state":"open"}`); err != nil {
				t.Errorf("write default issue response: %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner-b/repo/issues":
			ownerBWrites++
			sawOwnerBToken = r.Header.Get("Authorization") == "Bearer owner-b-installation-token"
			if !sawOwnerBToken {
				t.Error("owner B write did not use owner B's installation token")
			}
			if _, err := fmt.Fprint(w, `{"number":2,"title":"created","state":"open"}`); err != nil {
				t.Errorf("write owner B issue response: %v", err)
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate app key: %v", err)
	}
	appConfig := GitHubAppConfig{
		AppID:          42,
		InstallationID: 7,
		PrivateKey: func(context.Context) ([]byte, error) {
			return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), nil
		},
		Client: testAppHTTPClient(t, api),
	}
	resolver := newTestInstallationResolver(t, api, appConfig)
	primary := NewGitHub(GitHubConfig{
		Host:   "github.com",
		Token:  routedIdentityTokenSource{token: "primary-default-token", login: "compass[bot]"},
		Client: testAppHTTPClient(t, api),
	})
	router := NewOwnerRoutedGitHub(primary, resolver, appConfig, testAppHTTPClient(t, api))

	if _, err := router.CreateIssue(context.Background(), "owner-a/repo", CreateIssue{Title: "default"}); err != nil {
		t.Fatalf("default-owner write: %v", err)
	}
	if _, err := router.CreateIssue(context.Background(), "owner-a/repo", CreateIssue{Title: "gated"}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("second default-owner write error = %v, want ErrBudgetExhausted from the primary client's rate gate", err)
	}
	if _, err := router.CreateIssue(context.Background(), "owner-b/repo", CreateIssue{Title: "owner B"}); err != nil {
		t.Fatalf("owner B write: %v", err)
	}
	if defaultWrites != 1 || ownerBWrites != 1 || !sawOwnerBToken {
		t.Fatalf("write counts/default auth = %d/%d/%v, want 1/1/true", defaultWrites, ownerBWrites, sawOwnerBToken)
	}
}

func TestOwnerRoutedGitHubAppLoginDelegatesToPrimary(t *testing.T) {
	primary := NewGitHub(GitHubConfig{Token: routedIdentityTokenSource{token: "primary", login: "compass[bot]"}})
	router := NewOwnerRoutedGitHub(primary, nil, GitHubAppConfig{}, nil)
	got, err := router.AppLogin(context.Background())
	if err != nil {
		t.Fatalf("AppLogin: %v", err)
	}
	if got != "compass[bot]" {
		t.Fatalf("AppLogin = %q, want primary App login", got)
	}
}

func TestOwnerRoutedGitHubWrapsInstallationLookupErrors(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer api.Close()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate app key: %v", err)
	}
	appConfig := GitHubAppConfig{
		AppID: 42,
		PrivateKey: func(context.Context) ([]byte, error) {
			return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), nil
		},
		Client: testAppHTTPClient(t, api),
	}
	resolver, err := NewInstallationResolver(appConfig)
	if err != nil {
		t.Fatalf("NewInstallationResolver: %v", err)
	}
	primary := NewGitHub(GitHubConfig{Token: routedIdentityTokenSource{token: "primary"}})
	router := NewOwnerRoutedGitHub(primary, resolver, appConfig, testAppHTTPClient(t, api))
	_, err = router.GetIssue(context.Background(), "owner-b/repo", 1)
	if !errors.Is(err, ErrInstallationLookup) {
		t.Fatalf("GetIssue error = %v, want ErrInstallationLookup", err)
	}
}

func TestOwnerRoutedGitHubOwnerWithoutInstallation(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/app/installations" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if _, err := fmt.Fprint(w, `[{"id":7,"account":{"login":"owner-a"}}]`); err != nil {
			t.Errorf("write installation list: %v", err)
		}
	}))
	defer api.Close()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate app key: %v", err)
	}
	appConfig := GitHubAppConfig{
		AppID:          42,
		InstallationID: 7,
		PrivateKey: func(context.Context) ([]byte, error) {
			return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), nil
		},
	}
	resolver := newTestInstallationResolver(t, api, appConfig)
	primary := NewGitHub(GitHubConfig{Token: routedIdentityTokenSource{token: "primary"}, Client: testAppHTTPClient(t, api)})
	router := NewOwnerRoutedGitHub(primary, resolver, appConfig, testAppHTTPClient(t, api))

	_, err = router.CreateIssue(context.Background(), "owner-c/repo", CreateIssue{Title: "x"})
	if !errors.Is(err, ErrNoInstallation) || !errors.Is(err, ErrInstallationLookup) {
		t.Fatalf("CreateIssue error = %v, want ErrNoInstallation inside ErrInstallationLookup", err)
	}
	if _, err := router.CreateIssue(context.Background(), "no-slash", CreateIssue{Title: "x"}); !errors.Is(err, ErrMalformedRepo) {
		t.Fatalf("malformed repo error = %v, want ErrMalformedRepo", err)
	}
}

func TestOwnerRoutedGitHubUsesInstallationIDCache(t *testing.T) {
	var mintRequests int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			if _, err := fmt.Fprint(w, `[{"id":8,"account":{"login":"owner-b"}}]`); err != nil {
				t.Errorf("write installation list: %v", err)
			}
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/"):
			mintRequests++
			if _, err := fmt.Fprint(w, `{"token":"owner-b-installation-token","expires_at":"2030-01-01T00:00:00Z"}`); err != nil {
				t.Errorf("write token response: %v", err)
			}
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/repos/owner-b/repo/issues"):
			if _, err := fmt.Fprint(w, `{"number":2,"title":"created","state":"open"}`); err != nil {
				t.Errorf("write issue response: %v", err)
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate app key: %v", err)
	}
	appConfig := GitHubAppConfig{
		AppID: 42,
		PrivateKey: func(context.Context) ([]byte, error) {
			return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), nil
		},
		Client: testAppHTTPClient(t, api),
	}
	resolver := newTestInstallationResolver(t, api, appConfig)
	primary := NewGitHub(GitHubConfig{Token: routedIdentityTokenSource{token: "primary"}, Client: testAppHTTPClient(t, api)})
	router := NewOwnerRoutedGitHub(primary, resolver, appConfig, testAppHTTPClient(t, api))
	for range 2 {
		if _, err := router.CreateIssue(context.Background(), "owner-b/repo", CreateIssue{Title: "owner B"}); err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
	}
	if mintRequests != 1 {
		t.Fatalf("installation token mint requests = %d, want one cached source", mintRequests)
	}
}
