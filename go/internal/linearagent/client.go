package linearagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// linearOAuthTokenURL is the Linear client-credentials token endpoint. Minting
// requests POST here with grant_type=client_credentials.
const linearOAuthTokenURL = "https://api.linear.app/oauth/token" //nolint:gosec // G101 false positive: a public API endpoint URL, not a credential

// linearGraphQLURL is the Linear GraphQL endpoint the emitter posts mutations to.
const linearGraphQLURL = "https://api.linear.app/graphql"

// tokenScope is the pinned OAuth scope string requested on every mint. It MUST
// stay identical across mints: Linear revokes a client-credentials app's
// existing tokens when a mint requests a different scope set, so varying the
// scope here would silently invalidate tokens already in flight.
const tokenScope = "read,write,app:assignable,app:mentionable" //nolint:gosec // G101 false positive: an OAuth scope string, not a credential

// httpDoer is the injectable HTTP seam. *http.Client satisfies it; tests supply
// an httptest.Server-backed client.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// ActivityContent is the body of an agentActivityCreate mutation. Per
// linear.app/developers/agent-interaction §Activity content payload it is a
// discriminated union keyed on Type (e.g. "thought"); Body carries the text for
// the content types the responder emits (the one "thought" ack under Option B).
type ActivityContent struct {
	Type string `json:"type"`
	Body string `json:"body"`
}

// ExternalURL is a deep-link entry attached to a session via agentSessionUpdate.
type ExternalURL struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Client emits agent activity and session updates to Linear. The dispatcher
// (T6) calls it; the concrete implementation is *graphQLClient.
type Client interface {
	// CreateActivity posts an agent activity (the "thought" ack) to sessionID.
	CreateActivity(ctx context.Context, sessionID string, content ActivityContent) error
	// UpdateSession attaches externalURLs (the deep link) to sessionID.
	UpdateSession(ctx context.Context, sessionID string, externalURLs []ExternalURL) error
}

// TokenSource mints and caches a Linear client-credentials access token in
// memory. It never persists the token. A re-mint (initial or post-401) is
// coalesced through singleflight so N concurrent callers share one HTTP mint.
type TokenSource struct {
	doer         httpDoer
	tokenURL     string
	clientID     string
	clientSecret string
	now          func() time.Time

	group singleflight.Group

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewTokenSource builds a TokenSource for the given credentials. doer defaults
// to http.DefaultClient and tokenURL to the Linear OAuth endpoint when zero, so
// production callers pass only the credentials and tests inject both.
func NewTokenSource(clientID, clientSecret string, doer httpDoer, tokenURL string) *TokenSource {
	if doer == nil {
		doer = http.DefaultClient
	}
	if tokenURL == "" {
		tokenURL = linearOAuthTokenURL
	}
	return &TokenSource{
		doer:         doer,
		tokenURL:     tokenURL,
		clientID:     clientID,
		clientSecret: clientSecret,
		now:          time.Now,
	}
}

// Token returns the cached token while unexpired, else mints a fresh one. The
// mint is coalesced: concurrent callers arriving with no valid cached token
// share one HTTP round trip via singleflight.
func (t *TokenSource) Token(ctx context.Context) (string, error) {
	if tok, ok := t.cached(); ok {
		return tok, nil
	}
	return t.mint(ctx)
}

// Invalidate drops the cached token so the next Token mints afresh — the client
// calls it on an observed 401.
func (t *TokenSource) Invalidate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.token = ""
	t.expires = time.Time{}
}

// cached returns the live cached token, if any.
func (t *TokenSource) cached() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && t.now().Before(t.expires) {
		return t.token, true
	}
	return "", false
}

// tokenResponse is the OAuth token endpoint reply.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

// mint performs (or coalesces onto) a single client-credentials mint and caches
// the result. singleflight collapses a burst of concurrent misses into one HTTP
// call; the shared result is cached by the leader before any caller returns.
func (t *TokenSource) mint(ctx context.Context) (string, error) {
	// Re-check under the singleflight leader so a caller that lost the race to a
	// just-completed mint reuses the fresh cache instead of minting again.
	v, err, _ := t.group.Do("mint", func() (any, error) {
		if tok, ok := t.cached(); ok {
			return tok, nil
		}
		return t.doMint(ctx)
	})
	if err != nil {
		return "", err
	}
	tok, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("linear token mint: unexpected singleflight result type %T", v)
	}
	return tok, nil
}

// doMint POSTs the client-credentials grant and caches the returned token.
func (t *TokenSource) doMint(ctx context.Context) (string, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {t.clientID},
		"client_secret": {t.clientSecret},
		"scope":         {tokenScope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("linear token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.doer.Do(req)
	if err != nil {
		return "", fmt.Errorf("linear token mint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // best-effort close on a read body; nothing actionable on failure
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("linear token read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("linear token mint: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("linear token decode: %w", err)
	}
	if tr.AccessToken == "" {
		return "", errors.New("linear token mint: empty access_token")
	}

	expires := t.now().Add(time.Hour)
	if tr.ExpiresIn > 0 {
		expires = t.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}

	t.mu.Lock()
	t.token = tr.AccessToken
	t.expires = expires
	t.mu.Unlock()
	return tr.AccessToken, nil
}

// graphQLClient implements Client over the Linear GraphQL endpoint, attaching a
// TokenSource bearer and re-minting once on a 401.
type graphQLClient struct {
	doer       httpDoer
	graphQLURL string
	tokens     *TokenSource
}

// NewClient builds a Client over the given TokenSource. doer defaults to
// http.DefaultClient and graphQLURL to the Linear GraphQL endpoint when zero.
func NewClient(tokens *TokenSource, doer httpDoer, graphQLURL string) Client {
	if doer == nil {
		doer = http.DefaultClient
	}
	if graphQLURL == "" {
		graphQLURL = linearGraphQLURL
	}
	return &graphQLClient{doer: doer, graphQLURL: graphQLURL, tokens: tokens}
}

// agentActivityCreateMutation posts one agent activity to a session.
const agentActivityCreateMutation = `mutation AgentActivityCreate($input: AgentActivityCreateInput!) {
  agentActivityCreate(input: $input) { success }
}`

// agentSessionUpdateMutation attaches external URLs (the deep link) to a session.
const agentSessionUpdateMutation = `mutation AgentSessionUpdate($id: String!, $input: AgentSessionUpdateInput!) {
  agentSessionUpdate(id: $id, input: $input) { success }
}`

// CreateActivity wraps the agentActivityCreate mutation.
func (c *graphQLClient) CreateActivity(ctx context.Context, sessionID string, content ActivityContent) error {
	return c.mutate(ctx, agentActivityCreateMutation, map[string]any{
		"input": map[string]any{
			"agentSessionId": sessionID,
			"content":        content,
		},
	})
}

// UpdateSession wraps the agentSessionUpdate mutation.
func (c *graphQLClient) UpdateSession(ctx context.Context, sessionID string, externalURLs []ExternalURL) error {
	return c.mutate(ctx, agentSessionUpdateMutation, map[string]any{
		"id": sessionID,
		"input": map[string]any{
			"externalUrls": externalURLs,
		},
	})
}

// graphQLRequest is the GraphQL POST body.
type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// graphQLResponse carries the top-level GraphQL errors array plus each
// mutation's own payload. A mutation can fail two ways at the GraphQL layer:
// a 200 with a populated top-level errors list, or a 200 with an empty errors
// list but a payload success=false. Both mutations select `{ success }`, so a
// false there is an actionable soft failure (under Option B these two emits are
// the entire return path and carry the ack-liveness SLA); Data is keyed by the
// mutation's top-level field name (agentActivityCreate / agentSessionUpdate).
type graphQLResponse struct {
	Data map[string]struct {
		Success *bool `json:"success"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// mutate posts a GraphQL mutation with the TokenSource bearer, re-minting once
// and retrying once on a 401.
func (c *graphQLClient) mutate(ctx context.Context, query string, variables map[string]any) error {
	body, err := json.Marshal(graphQLRequest{Query: query, Variables: variables})
	if err != nil {
		return fmt.Errorf("linear mutation encode: %w", err)
	}

	resp, err := c.do(ctx, body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// Stale token: drop it, re-mint, and retry exactly once.
		_ = resp.Body.Close() // discarding the 401 body before retry; nothing actionable on failure
		c.tokens.Invalidate()
		resp, err = c.do(ctx, body)
		if err != nil {
			return err
		}
	}
	defer func() { _ = resp.Body.Close() }() // best-effort close on a read body; nothing actionable on failure

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("linear mutation read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("linear mutation: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var gr graphQLResponse
	if err := json.Unmarshal(respBody, &gr); err != nil {
		return fmt.Errorf("linear mutation decode: %w", err)
	}
	if len(gr.Errors) > 0 {
		return fmt.Errorf("linear mutation: %s", gr.Errors[0].Message)
	}
	// A requested `success` field that is present and false is a soft failure
	// (the mutation was accepted at the transport/GraphQL layer but did not take
	// effect); surface it like an error so the dispatcher's fallback fires. An
	// absent success (nil) is tolerated in case the live schema omits it.
	for field, payload := range gr.Data {
		if payload.Success != nil && !*payload.Success {
			return fmt.Errorf("linear mutation %s: success=false", field)
		}
	}
	return nil
}

// do issues one authenticated GraphQL POST, attaching the current bearer token.
func (c *graphQLClient) do(ctx context.Context, body []byte) (*http.Response, error) {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.graphQLURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("linear mutation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("linear mutation post: %w", err)
	}
	return resp, nil
}
