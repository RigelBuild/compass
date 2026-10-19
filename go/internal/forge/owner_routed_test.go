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

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
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

	defaultClient, err := router.clientFor(context.Background(), "owner-a/repo")
	if err != nil {
		t.Fatalf("default owner clientFor: %v", err)
	}
	if defaultClient != primary {
		t.Fatal("default installation resolved to a different client than primary")
	}
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

func newOwnerRoutedReadFixture(t *testing.T, reads http.HandlerFunc) *OwnerRoutedGitHub {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			if _, err := fmt.Fprint(w, `[{"id":7,"account":{"login":"owner-a"}},{"id":8,"account":{"login":"owner-b"}}]`); err != nil {
				t.Errorf("write installations: %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/8/access_tokens":
			w.Header().Set("Content-Type", "application/json")
			if _, err := fmt.Fprint(w, `{"token":"owner-b-installation-token","expires_at":"2030-01-01T00:00:00Z"}`); err != nil {
				t.Errorf("write owner B token: %v", err)
			}
		default:
			reads.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(api.Close)

	key := testAppKey(t)
	appHTTP := testAppHTTPClient(t, api)
	appConfig := GitHubAppConfig{
		AppID:          42,
		InstallationID: 7,
		PrivateKey:     func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
		Host:           "github.com",
		Client:         appHTTP,
	}
	resolver := newTestInstallationResolver(t, api, appConfig)
	primary := NewGitHub(GitHubConfig{
		Host:   "github.com",
		Token:  routedIdentityTokenSource{token: "primary-default-token"},
		Client: appHTTP,
	})
	return NewOwnerRoutedGitHub(primary, resolver, appConfig, appHTTP)
}

func TestOwnerRoutedGitHubNotifyReadsUseOwnerInstallation(t *testing.T) {
	issueBody := `{"number":7,"title":"bug","state":"open","html_url":"https://github.com/example/repo/issues/7","user":{"login":"octocat"}}`
	commentsBody := `[{"id":1,"html_url":"https://github.com/example/repo/issues/7#comment-1","body":"new","user":{"login":"octocat"}}]`
	router := newOwnerRoutedReadFixture(t, func(w http.ResponseWriter, r *http.Request) {
		wantToken := "primary-default-token"
		if strings.HasPrefix(r.URL.Path, "/repos/owner-b/") {
			wantToken = "owner-b-installation-token"
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantToken {
			t.Errorf("%s %s Authorization = %q, want Bearer %s", r.Method, r.URL.Path, got, wantToken)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/7"):
			if _, err := fmt.Fprint(w, issueBody); err != nil {
				t.Errorf("write issue response: %v", err)
			}
		case strings.HasSuffix(r.URL.Path, "/issues/7/comments"):
			if _, err := fmt.Fprint(w, commentsBody); err != nil {
				t.Errorf("write comments response: %v", err)
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})

	for _, tc := range []struct {
		name string
		repo string
	}{
		{name: "default owner", repo: "owner-a/repo"},
		{name: "owner B", repo: "owner-b/repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := router.GetIssueConditional(t.Context(), tc.repo, 7, ""); err != nil {
				t.Fatalf("GetIssueConditional: %v", err)
			}
			comments, err := router.ListComments(t.Context(), tc.repo, kindIssueR, 7, "")
			if err != nil {
				t.Fatalf("ListComments: %v", err)
			}
			if len(comments.V) != 1 || comments.V[0].Body != "new" {
				t.Fatalf("comments = %+v, want the fixture comment", comments.V)
			}
		})
	}
}

func TestOwnerRoutedGitHubBoardReadUsesOwnerInstallation(t *testing.T) {
	var owners []string
	router := newOwnerRoutedReadFixture(t, func(w http.ResponseWriter, r *http.Request) {
		owner, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
		owners = append(owners, owner)
		wantToken := "primary-default-token"
		if owner == "owner-b" {
			wantToken = "owner-b-installation-token"
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantToken {
			t.Errorf("%s %s Authorization = %q, want Bearer %s", r.Method, r.URL.Path, got, wantToken)
		}
		switch r.URL.Path {
		case "/repos/owner-a/repo/issues", "/repos/owner-b/repo/issues":
			if _, err := fmt.Fprint(w, `[{"number":9,"title":"bug","state":"open","html_url":"https://github.com/example/repo/issues/9","user":{"login":"octocat"}}]`); err != nil {
				t.Errorf("write issues response: %v", err)
			}
		case "/repos/owner-a/repo/issues/9", "/repos/owner-b/repo/issues/9":
			if _, err := fmt.Fprint(w, `{"number":9,"title":"bug","state":"open","html_url":"https://github.com/example/repo/issues/9","user":{"login":"octocat"}}`); err != nil {
				t.Errorf("write issue response: %v", err)
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	for _, repo := range []string{"owner-a/repo", "owner-b/repo"} {
		issues, err := router.ListIssues(t.Context(), repo, IssueFilter{})
		if err != nil {
			t.Fatalf("ListIssues(%q): %v", repo, err)
		}
		if len(issues) != 1 || issues[0].Number != 9 {
			t.Fatalf("ListIssues(%q) = %+v, want issue #9", repo, issues)
		}
		if _, err := router.GetIssue(t.Context(), repo, 9); err != nil {
			t.Fatalf("GetIssue(%q, 9): %v", repo, err)
		}
	}
	if len(owners) != 4 || owners[0] != "owner-a" || owners[1] != "owner-a" || owners[2] != "owner-b" || owners[3] != "owner-b" {
		t.Fatalf("board read owners = %v, want owner-a list/detail then owner-b list/detail", owners)
	}
}

func TestOwnerRoutedGitHubPRReadsUseOwnerInstallation(t *testing.T) {
	const repo = "owner-b/repo"
	var reads []string
	router := newOwnerRoutedReadFixture(t, func(w http.ResponseWriter, r *http.Request) {
		reads = append(reads, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer owner-b-installation-token" {
			t.Errorf("%s %s Authorization = %q, want owner B installation token", r.Method, r.URL.Path, got)
		}
		key := r.Method + " " + r.URL.Path
		if key == "GET /repos/owner-b/repo/pulls" && r.URL.Query().Get("state") == "open" {
			key += "?open"
		}
		response, ok := ownerBPullReadResponses[key]
		if !ok {
			t.Errorf("unexpected request: %s", key)
			http.NotFound(w, r)
			return
		}
		if _, err := fmt.Fprint(w, response); err != nil {
			t.Errorf("write %s response: %v", key, err)
		}
	})

	tests := []struct {
		name string
		read func() error
	}{
		{name: "GetPullRequestConditional", read: func() error { _, err := router.GetPullRequestConditional(t.Context(), repo, 7, ""); return err }},
		{name: "ChecksConditional", read: func() error { _, err := router.ChecksConditional(t.Context(), repo, 7, "head-sha", ""); return err }},
		{name: "ListNewArtifacts", read: func() error {
			_, err := router.ListNewArtifacts(t.Context(), repo, compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST, 0, "")
			return err
		}},
		{name: "ListUpdatedIssues", read: func() error { _, err := router.ListUpdatedIssues(t.Context(), repo, time.Time{}, ""); return err }},
		{name: "PullRequestForSHA", read: func() error { _, err := router.PullRequestForSHA(t.Context(), repo, "head-sha"); return err }},
		{name: "GetPullRequest", read: func() error { _, err := router.GetPullRequest(t.Context(), repo, 7); return err }},
		{name: "ListOpenPullRequests", read: func() error { _, err := router.ListOpenPullRequests(t.Context(), repo); return err }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := len(reads)
			if err := tc.read(); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(reads) == before {
				t.Fatalf("%s sent no owner-B read", tc.name)
			}
		})
	}
}

var ownerBPullReadResponses = map[string]string{
	"GET /repos/owner-b/repo/pulls/7":                     `{"number":7,"state":"open","html_url":"https://github.com/owner-b/repo/pull/7","head":{"sha":"head-sha"},"base":{"ref":"main"},"user":{"login":"octocat"}}`,
	"GET /repos/owner-b/repo/pulls/7/reviews":             `[]`,
	"GET /repos/owner-b/repo/commits/head-sha/check-runs": `{"check_runs":[]}`,
	"GET /repos/owner-b/repo/commits/head-sha/status":     `{"statuses":[]}`,
	"POST /graphql": emptyPullGraphQL("head-sha"),
	"GET /repos/owner-b/repo/commits/head-sha/pulls": `[{"number":7,"state":"open"}]`,
	"GET /repos/owner-b/repo/issues":                 `[]`,
	"GET /repos/owner-b/repo/pulls":                  `[]`,
	"GET /repos/owner-b/repo/pulls?open":             `[{"number":7,"state":"open","updated_at":"2026-01-01T00:00:00Z"}]`,
}
