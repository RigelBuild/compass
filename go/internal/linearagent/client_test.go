package linearagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// tokenServer is an httptest.Server standing in for the Linear OAuth token
// endpoint. It counts mints and returns a distinct access token per mint so a
// test can prove which token a caller received.
type tokenServer struct {
	srv   *httptest.Server
	mints atomic.Int64
	// block, when non-nil, gates each mint until closed — the test uses it to
	// hold a mint in-flight while concurrent callers pile up on singleflight.
	block chan struct{}
}

func newTokenServer(t *testing.T) *tokenServer {
	t.Helper()
	ts := &tokenServer{}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("token endpoint: method = %s, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("token endpoint: parse form: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "client_credentials" {
			t.Errorf("token endpoint: grant_type = %q, want client_credentials", got)
		}
		if got := r.Form.Get("scope"); got != tokenScope {
			t.Errorf("token endpoint: scope = %q, want %q", got, tokenScope)
		}
		if ts.block != nil {
			<-ts.block
		}
		n := ts.mints.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": tokenName(n),
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
	t.Cleanup(ts.srv.Close)
	return ts
}

func tokenName(n int64) string {
	return "tok-" + strconv.FormatInt(n, 10)
}

func TestTokenSourceCachesAcrossCalls(t *testing.T) {
	ts := newTokenServer(t)
	src := NewTokenSource("cid", "csecret", ts.srv.Client(), ts.srv.URL)
	for range 5 {
		tok, err := src.Token(context.Background())
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		if tok != tokenName(1) {
			t.Fatalf("Token = %q, want %q (cache should serve the first mint)", tok, tokenName(1))
		}
	}
	if got := ts.mints.Load(); got != 1 {
		t.Fatalf("mints = %d, want 1 (token must be cached across calls)", got)
	}
}

func TestTokenSourceConcurrentMintCoalesces(t *testing.T) {
	ts := newTokenServer(t)
	ts.block = make(chan struct{})
	src := NewTokenSource("cid", "csecret", ts.srv.Client(), ts.srv.URL)

	const callers = 8
	var wg sync.WaitGroup
	toks := make([]string, callers)
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, err := src.Token(context.Background())
			if err != nil {
				t.Errorf("Token: %v", err)
				return
			}
			toks[i] = tok
		}(i)
	}
	// Release the single in-flight mint; all coalesced callers share its result.
	close(ts.block)
	wg.Wait()

	if got := ts.mints.Load(); got != 1 {
		t.Fatalf("mints = %d, want 1 (concurrent callers must coalesce to one mint)", got)
	}
	for i, tok := range toks {
		if tok != tokenName(1) {
			t.Fatalf("caller %d token = %q, want %q", i, tok, tokenName(1))
		}
	}
}

// graphQLServer stands in for the Linear GraphQL endpoint. It records the last
// mutation query/variables and, when failFirst401 is set, returns a single 401
// before succeeding — the stale-token path.
type graphQLServer struct {
	srv          *httptest.Server
	mu           sync.Mutex
	lastQuery    string
	lastVars     map[string]any
	bearers      []string
	requests     atomic.Int64
	failFirst401 atomic.Bool
	returnErrors []string
	returnStatus int
	always401    atomic.Bool
	successFalse atomic.Bool
}

func newGraphQLServer(t *testing.T) *graphQLServer {
	t.Helper()
	gs := &graphQLServer{}
	gs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gs.requests.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("graphql: read body: %v", err)
		}
		var req graphQLRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("graphql: decode: %v", err)
		}
		gs.mu.Lock()
		gs.lastQuery = req.Query
		gs.lastVars = req.Variables
		gs.bearers = append(gs.bearers, r.Header.Get("Authorization"))
		gs.mu.Unlock()

		if gs.failFirst401.CompareAndSwap(true, false) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
			return
		}
		if gs.always401.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
			return
		}
		if gs.returnStatus != 0 {
			w.WriteHeader(gs.returnStatus)
			_, _ = io.WriteString(w, "boom")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{}
		if gs.successFalse.Load() {
			resp["data"] = map[string]any{"agentActivityCreate": map[string]any{"success": false}}
		}
		if len(gs.returnErrors) > 0 {
			errs := make([]map[string]string, 0, len(gs.returnErrors))
			for _, m := range gs.returnErrors {
				errs = append(errs, map[string]string{"message": m})
			}
			resp["errors"] = errs
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(gs.srv.Close)
	return gs
}

func TestClientCreateActivityIssuesMutation(t *testing.T) {
	ts := newTokenServer(t)
	gs := newGraphQLServer(t)
	src := NewTokenSource("cid", "csecret", ts.srv.Client(), ts.srv.URL)
	c := NewClient(src, gs.srv.Client(), gs.srv.URL)

	err := c.CreateActivity(context.Background(), "sess-1", ActivityContent{Type: "thought", Body: "ack"})
	if err != nil {
		t.Fatalf("CreateActivity: %v", err)
	}
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if !strings.Contains(gs.lastQuery, "agentActivityCreate") {
		t.Fatalf("query = %q, want agentActivityCreate mutation", gs.lastQuery)
	}
	input, _ := gs.lastVars["input"].(map[string]any)
	if input["agentSessionId"] != "sess-1" {
		t.Fatalf("agentSessionId = %v, want sess-1", input["agentSessionId"])
	}
	content, _ := input["content"].(map[string]any)
	if content["type"] != "thought" || content["body"] != "ack" {
		t.Fatalf("content = %v, want {type:thought, body:ack}", content)
	}
}

func TestClientUpdateSessionIssuesMutation(t *testing.T) {
	ts := newTokenServer(t)
	gs := newGraphQLServer(t)
	src := NewTokenSource("cid", "csecret", ts.srv.Client(), ts.srv.URL)
	c := NewClient(src, gs.srv.Client(), gs.srv.URL)

	err := c.UpdateSession(context.Background(), "sess-2", []ExternalURL{{Label: "Compass", URL: "https://x/y"}})
	if err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if !strings.Contains(gs.lastQuery, "agentSessionUpdate") {
		t.Fatalf("query = %q, want agentSessionUpdate mutation", gs.lastQuery)
	}
	if gs.lastVars["id"] != "sess-2" {
		t.Fatalf("id = %v, want sess-2", gs.lastVars["id"])
	}
	input, _ := gs.lastVars["input"].(map[string]any)
	urls, _ := input["externalUrls"].([]any)
	if len(urls) != 1 {
		t.Fatalf("externalUrls len = %d, want 1", len(urls))
	}
	first, _ := urls[0].(map[string]any)
	if first["label"] != "Compass" || first["url"] != "https://x/y" {
		t.Fatalf("externalUrl = %v, want {label:Compass, url:https://x/y}", first)
	}
}

func TestClientRemintsOnceOn401(t *testing.T) {
	ts := newTokenServer(t)
	gs := newGraphQLServer(t)
	gs.failFirst401.Store(true)
	src := NewTokenSource("cid", "csecret", ts.srv.Client(), ts.srv.URL)
	c := NewClient(src, gs.srv.Client(), gs.srv.URL)

	err := c.CreateActivity(context.Background(), "sess-1", ActivityContent{Type: "thought", Body: "ack"})
	if err != nil {
		t.Fatalf("CreateActivity: %v", err)
	}
	// Two mints: the initial, plus the one re-mint the 401 forced.
	if got := ts.mints.Load(); got != 2 {
		t.Fatalf("mints = %d, want 2 (a 401 must force exactly one re-mint)", got)
	}
	// Two GraphQL requests: the 401 and the retry.
	if got := gs.requests.Load(); got != 2 {
		t.Fatalf("graphql requests = %d, want 2 (401 then retry)", got)
	}
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if len(gs.bearers) != 2 {
		t.Fatalf("bearers = %v, want 2", gs.bearers)
	}
	if gs.bearers[0] != "Bearer "+tokenName(1) {
		t.Fatalf("first bearer = %q, want first token", gs.bearers[0])
	}
	if gs.bearers[1] != "Bearer "+tokenName(2) {
		t.Fatalf("retry bearer = %q, want re-minted token", gs.bearers[1])
	}
}

func TestClientSurfacesNon401Error(t *testing.T) {
	ts := newTokenServer(t)
	gs := newGraphQLServer(t)
	gs.returnStatus = http.StatusInternalServerError
	src := NewTokenSource("cid", "csecret", ts.srv.Client(), ts.srv.URL)
	c := NewClient(src, gs.srv.Client(), gs.srv.URL)

	err := c.CreateActivity(context.Background(), "sess-1", ActivityContent{Type: "thought", Body: "ack"})
	if err == nil {
		t.Fatal("CreateActivity: want error on 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error = %v, want it to mention status 500", err)
	}
	// A non-401 must not trigger a re-mint or retry.
	if got := ts.mints.Load(); got != 1 {
		t.Fatalf("mints = %d, want 1 (a non-401 must not re-mint)", got)
	}
	if got := gs.requests.Load(); got != 1 {
		t.Fatalf("graphql requests = %d, want 1 (a non-401 must not retry)", got)
	}
}

func TestClientSurfacesGraphQLErrors(t *testing.T) {
	ts := newTokenServer(t)
	gs := newGraphQLServer(t)
	gs.returnErrors = []string{"session not found"}
	src := NewTokenSource("cid", "csecret", ts.srv.Client(), ts.srv.URL)
	c := NewClient(src, gs.srv.Client(), gs.srv.URL)

	err := c.CreateActivity(context.Background(), "sess-1", ActivityContent{Type: "thought", Body: "ack"})
	if err == nil || !strings.Contains(err.Error(), "session not found") {
		t.Fatalf("error = %v, want it to surface the GraphQL error", err)
	}
}

func TestClientRemintNoLoopOnPersistent401(t *testing.T) {
	ts := newTokenServer(t)
	gs := newGraphQLServer(t)
	gs.always401.Store(true)
	src := NewTokenSource("cid", "csecret", ts.srv.Client(), ts.srv.URL)
	c := NewClient(src, gs.srv.Client(), gs.srv.URL)

	err := c.CreateActivity(context.Background(), "sess-1", ActivityContent{Type: "thought", Body: "ack"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want it to mention status 401", err)
	}
	// Exactly one re-mint: the initial token plus one forced by the first 401.
	if got := ts.mints.Load(); got != 2 {
		t.Fatalf("mints = %d, want 2 (a persistent 401 must re-mint exactly once, never loop)", got)
	}
	// Exactly two GraphQL requests: the original and the single retry — never a third.
	if got := gs.requests.Load(); got != 2 {
		t.Fatalf("graphql requests = %d, want 2 (re-mint retries once, then surfaces the error)", got)
	}
}

func TestClientSurfacesSuccessFalse(t *testing.T) {
	ts := newTokenServer(t)
	gs := newGraphQLServer(t)
	gs.successFalse.Store(true)
	src := NewTokenSource("cid", "csecret", ts.srv.Client(), ts.srv.URL)
	c := NewClient(src, gs.srv.Client(), gs.srv.URL)

	// A 200 with empty errors but payload success=false is a soft failure that
	// must surface (under Option B this emit is the entire ack return path).
	err := c.CreateActivity(context.Background(), "sess-1", ActivityContent{Type: "thought", Body: "ack"})
	if err == nil || !strings.Contains(err.Error(), "success=false") {
		t.Fatalf("error = %v, want it to surface the mutation success=false", err)
	}
}
