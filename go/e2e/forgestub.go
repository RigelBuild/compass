package e2e

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/certgen"
)

const (
	forgeStubIssueNumber       uint64 = 4242
	forgeStubPullRequestNumber uint64 = 4243
)

type forgeStubRequest struct {
	Method        string
	Path          string
	Authorization string
	Body          []byte
}

type forgeStub struct {
	srv      *httptest.Server
	caPath   string
	mu       sync.Mutex
	requests []forgeStubRequest
	issues   map[uint64]map[string]any
	tokens   map[int64]string
}

func newForgeStub(t *testing.T) *forgeStub {
	t.Helper()
	kp, err := certgen.Generate([]string{"127.0.0.1", "localhost"}, 0)
	if err != nil {
		t.Fatalf("certgen.Generate: %v", err)
	}
	cert, err := tls.X509KeyPair(kp.CertPEM, kp.KeyPEM)
	if err != nil {
		t.Fatalf("tls.X509KeyPair: %v", err)
	}
	caPath := filepath.Join(t.TempDir(), "forge-ca.pem")
	if err := os.WriteFile(caPath, kp.CertPEM, 0o600); err != nil {
		t.Fatalf("write CA: %v", err)
	}

	s := &forgeStub{
		caPath: caPath,
		issues: map[uint64]map[string]any{},
		tokens: map[int64]string{},
	}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.handle))
	s.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	s.srv.StartTLS()
	t.Cleanup(s.srv.Close)
	return s
}

func (s *forgeStub) Host() string {
	return strings.TrimPrefix(s.srv.URL, "https://")
}

func (s *forgeStub) CAPath() string {
	return s.caPath
}

func (s *forgeStub) MintToken(installationID int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens[installationID] == "" {
		s.tokens[installationID] = fmt.Sprintf("forge-stub-installation-%d", installationID)
	}
	return s.tokens[installationID]
}

func (s *forgeStub) Requests() []forgeStubRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]forgeStubRequest, len(s.requests))
	for i, request := range s.requests {
		out[i] = request
		out[i].Body = append([]byte(nil), request.Body...)
	}
	return out
}

func (s *forgeStub) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusInternalServerError)
		return
	}
	if err := r.Body.Close(); err != nil {
		http.Error(w, "close body", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.requests = append(s.requests, forgeStubRequest{
		Method:        r.Method,
		Path:          r.URL.Path,
		Authorization: r.Header.Get("Authorization"),
		Body:          append([]byte(nil), body...),
	})
	s.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, "/api/v3/repos/") && !s.validAuthorization(r.Header.Get("Authorization")) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v3/"), "/"), "/")
	if len(parts) == 4 && parts[0] == "app" && parts[1] == "installations" && parts[3] == "access_tokens" && r.Method == http.MethodPost {
		if !validAppJWT(r.Header.Get("Authorization")) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		installationID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		jsonOut(w, http.StatusCreated, map[string]any{
			"token":      s.MintToken(installationID),
			"expires_at": time.Now().Add(time.Hour),
		})
		return
	}
	if len(parts) < 4 || parts[0] != "repos" {
		http.NotFound(w, r)
		return
	}
	switch parts[3] {
	case "issues":
		s.handleIssue(w, r, parts, body)
	case "pulls":
		if len(parts) != 4 || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var input map[string]any
		if err := json.Unmarshal(body, &input); err != nil {
			http.Error(w, "json", http.StatusBadRequest)
			return
		}
		jsonOut(w, http.StatusCreated, issue(forgeStubPullRequestNumber, input, "open", "/pulls/4243"))
	default:
		http.NotFound(w, r)
	}
}

func (s *forgeStub) handleIssue(w http.ResponseWriter, r *http.Request, parts []string, body []byte) {
	if len(parts) == 4 && r.Method == http.MethodPost {
		var input map[string]any
		if err := json.Unmarshal(body, &input); err != nil {
			http.Error(w, "json", http.StatusBadRequest)
			return
		}
		value := issue(forgeStubIssueNumber, input, "open", "/issues/4242")
		s.mu.Lock()
		s.issues[forgeStubIssueNumber] = value
		s.mu.Unlock()
		jsonOut(w, http.StatusCreated, value)
		return
	}
	if len(parts) == 6 && parts[5] == "comments" && r.Method == http.MethodPost {
		jsonOut(w, http.StatusCreated, map[string]any{"id": 1, "body": "comment", "user": map[string]any{"login": "forge-stub"}})
		return
	}
	if len(parts) != 5 || (r.Method != http.MethodGet && r.Method != http.MethodPatch) {
		http.NotFound(w, r)
		return
	}
	number, err := strconv.ParseUint(parts[4], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	value := s.issues[number]
	if value == nil {
		value = issue(number, nil, "open", "/issues/"+parts[4])
		s.issues[number] = value
	}
	if r.Method == http.MethodPatch {
		var input map[string]any
		if err := json.Unmarshal(body, &input); err != nil {
			s.mu.Unlock()
			http.Error(w, "json", http.StatusBadRequest)
			return
		}
		maps.Copy(value, input)
	}
	s.mu.Unlock()
	jsonOut(w, http.StatusOK, value)
}

func issue(number uint64, input map[string]any, state, path string) map[string]any {
	out := map[string]any{
		"number":   number,
		"title":    "forge stub issue",
		"body":     "",
		"state":    state,
		"html_url": "https://forge.stub" + path,
		"user":     map[string]any{"login": "forge-stub"},
		"labels":   []map[string]string{},
	}
	for key, value := range input {
		if key == "labels" {
			if labels, ok := value.([]any); ok {
				normalized := make([]map[string]string, 0, len(labels))
				for _, label := range labels {
					if name, ok := label.(string); ok {
						normalized = append(normalized, map[string]string{"name": name})
					}
				}
				out[key] = normalized
				continue
			}
		}
		out[key] = value
	}
	return out
}
func (s *forgeStub) validAuthorization(value string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for installationID, token := range s.tokens {
		if installationID == 1 && value == "Bearer "+token {
			return true
		}
	}
	return false
}

func validAppJWT(value string) bool {
	return strings.HasPrefix(value, "Bearer eyJ")
}

func jsonOut(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
	}
}
