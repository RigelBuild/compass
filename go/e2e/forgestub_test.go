package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/runner"
)

func TestForgeStubProviderClient(t *testing.T) {
	stub := newForgeStub(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
	client, err := runner.NewCATrustClient(stub.CAPath())
	if err != nil {
		t.Fatalf("NewCATrustClient: %v", err)
	}
	appTokens, err := forge.NewAppTokenSource(forge.GitHubAppConfig{AppID: 1001, InstallationID: 1, PrivateKey: func(context.Context) ([]byte, error) { return pemKey, nil }, Host: stub.Host(), Client: client.(*http.Client)})
	if err != nil {
		t.Fatalf("NewAppTokenSource: %v", err)
	}
	provider := forge.NewGitHub(forge.GitHubConfig{Host: stub.Host(), Token: appTokens, Client: client.(*http.Client)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	created, err := provider.CreateIssue(ctx, "owner/repo", forge.CreateIssue{Title: "title", Body: "body", Labels: []string{"bug"}})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if created.Number != forgeStubIssueNumber || created.Title != "title" {
		t.Fatalf("created number=%d title=%q", created.Number, created.Title)
	}
	requests := stub.Requests()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want mint and create", len(requests))
	}
	if requests[0].Path != "/api/v3/app/installations/1/access_tokens" || requests[0].Method != http.MethodPost {
		t.Fatalf("mint request method=%q path=%q", requests[0].Method, requests[0].Path)
	}
	if requests[1].Path != "/api/v3/repos/owner/repo/issues" || requests[1].Method != http.MethodPost {
		t.Fatalf("create request method=%q path=%q", requests[1].Method, requests[1].Path)
	}
	if requests[1].Authorization != "Bearer forge-stub-installation-1" {
		t.Fatalf("authorization = %q", requests[1].Authorization)
	}
}

// TestForgeStubRejectsUnauthenticatedRoutes pins the stub's own oracle: a
// repository path must never be served without the author bearer, and a path
// missing the /api/v3 prefix must 404 rather than route. Before the routing
// fix, a bare /repos/... created and stored an issue with no token at all,
// which is the shape a host-classification regression emits.
func TestForgeStubRejectsUnauthenticatedRoutes(t *testing.T) {
	stub := newForgeStub(t)
	client, err := runner.NewCATrustClient(stub.CAPath())
	if err != nil {
		t.Fatalf("NewCATrustClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name string
		path string
		want int
	}{
		{"unprefixed path does not route", "/repos/owner/repo/issues", http.StatusNotFound},
		{"prefixed path demands a bearer", "/api/v3/repos/owner/repo/issues", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+stub.Host()+tc.path, strings.NewReader(`{"title":"x"}`))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := client.(*http.Client).Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
	if got := stub.Issue(forgeStubIssueNumber); len(got) != 0 {
		t.Fatalf("unauthenticated requests mutated stub state: %v", got)
	}
}
