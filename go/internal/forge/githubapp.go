package forge

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// GitHubAppConfig identifies one GitHub App installation this deployment
// registered (each deployment registers its OWN App — see the T7 runbook).
type GitHubAppConfig struct {
	AppID          int64
	InstallationID int64
	PrivateKey     func(ctx context.Context) ([]byte, error) // PEM, lazily resolved (server_only secret)
	Host           string                                    // "github.com" or GHES; API base derives as in GitHubConfig
	Client         *http.Client                              // nil -> default
	Clock          func() time.Time                          // nil -> time.Now (test seam, the github.go idiom)
}

// appJWTTTL is the App JWT validity window. GitHub caps App JWTs at 10 minutes;
// the mint request completes well inside it, so ~10 min leaves headroom without
// courting clock-skew rejection (GitHub also back-dates iat by 60s below).
const appJWTTTL = 10 * time.Minute

// appJWTSkew back-dates the JWT iat to tolerate clock skew between this
// deployment and GitHub (GitHub rejects a JWT whose iat is in its future).
const appJWTSkew = 60 * time.Second

// tokenRefreshLead re-mints the installation token this long before its 1 h
// expiry, so an in-flight batch never carries a token that expires mid-fetch.
const tokenRefreshLead = 5 * time.Minute

// appTokenSource mints GitHub App installation access tokens behind the
// TokenSource seam: an RS256 App JWT authenticates a POST to the installation
// access-tokens endpoint, and the returned token is cached until shortly before
// its expiry. Safe for concurrent use; the mint is singleflighted so N
// concurrent Token calls trigger at most one HTTP round-trip.
type appTokenSource struct {
	cfg     GitHubAppConfig
	client  *http.Client
	clock   func() time.Time
	apiBase string

	group singleflight.Group

	mu       sync.Mutex
	token    string
	expsAt   time.Time // when the cached installation token itself expires
	refresh  time.Time // when we proactively re-mint (expsAt - tokenRefreshLead)
	hasToken bool
}

// NewAppTokenSource returns a TokenSource minting installation access tokens:
// RS256 App JWT (~10 min) -> POST /app/installations/{id}/access_tokens ->
// cached until ~5 min before the 1 h expiry. Invalidate drops the cache (the
// client calls it on 401/bad-creds-403, github.go:24-31). Safe for concurrent
// use; mint is singleflighted.
func NewAppTokenSource(cfg GitHubAppConfig) (TokenSource, error) {
	if cfg.AppID == 0 {
		return nil, errors.New("forge: GitHubAppConfig.AppID is required")
	}
	if cfg.InstallationID == 0 {
		return nil, errors.New("forge: GitHubAppConfig.InstallationID is required")
	}
	if cfg.PrivateKey == nil {
		return nil, errors.New("forge: GitHubAppConfig.PrivateKey is required")
	}
	s := &appTokenSource{
		cfg:     cfg,
		client:  cfg.Client,
		clock:   cfg.Clock,
		apiBase: appAPIBase(cfg.Host),
	}
	if s.client == nil {
		s.client = &http.Client{Timeout: 30 * time.Second}
	}
	if s.clock == nil {
		s.clock = time.Now
	}
	return s, nil
}

// appAPIBase derives the REST API base URL from the configured host, mirroring
// (*GitHub).apiBase (github.go:900-907): github.com -> api.github.com; a GHES
// host -> https://<host>/api/v3.
func appAPIBase(host string) string {
	if host == "" || host == "github.com" {
		return "https://api.github.com"
	}
	return "https://" + host + "/api/v3"
}

// Token returns the cached installation token when it is still comfortably
// inside its lifetime, else mints a fresh one. The mint is singleflighted:
// concurrent callers observing a stale cache share one HTTP round-trip.
func (s *appTokenSource) Token(ctx context.Context) (string, error) {
	if tok, ok := s.cached(); ok {
		return tok, nil
	}

	// singleflight collapses the concurrent stampede; the winner mints, losers
	// wait for and reuse its result. The key is constant — one installation.
	v, err, _ := s.group.Do("mint", func() (any, error) {
		// Re-check under the flight: a racing caller may have refreshed the
		// cache between our miss and acquiring the flight.
		if tok, ok := s.cached(); ok {
			return tok, nil
		}
		return s.mint(ctx)
	})
	if err != nil {
		return "", err
	}
	tok, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("forge: unexpected mint result type %T", v)
	}
	return tok, nil
}

// Invalidate drops the cached installation token so the next Token call
// re-mints. The client calls it on a 401 / bad-creds-403 (github.go:24-31).
func (s *appTokenSource) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hasToken = false
	s.token = ""
	s.expsAt = time.Time{}
	s.refresh = time.Time{}
}

// cached returns the current token when it is present and has not reached its
// refresh boundary.
func (s *appTokenSource) cached() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasToken {
		return "", false
	}
	if !s.clock().Before(s.refresh) {
		return "", false
	}
	return s.token, true
}

// installationToken is the wire shape of the access-tokens POST 201 response.
// Only the fields this source needs are decoded.
type installationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// mint builds an App JWT, POSTs it to the installation access-tokens endpoint,
// and caches the returned token until tokenRefreshLead before its expiry.
func (s *appTokenSource) mint(ctx context.Context) (string, error) {
	pemBytes, err := s.cfg.PrivateKey(ctx)
	if err != nil {
		return "", fmt.Errorf("forge: resolve app private key: %w", err)
	}
	key, err := parseRSAPrivateKey(pemBytes)
	if err != nil {
		return "", fmt.Errorf("forge: parse app private key: %w", err)
	}

	jwt, err := s.buildAppJWT(key)
	if err != nil {
		return "", fmt.Errorf("forge: build app jwt: %w", err)
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", s.apiBase, s.cfg.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		return "", fmt.Errorf("forge: build mint request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("forge: mint installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // deferred cleanup on a read body; no actionable close error

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("forge: read mint response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var ghErr ghError
		if jsonErr := json.Unmarshal(body, &ghErr); jsonErr == nil && ghErr.Message != "" {
			return "", fmt.Errorf("forge: mint installation token: status %d: %s", resp.StatusCode, ghErr.Message)
		}
		return "", fmt.Errorf("forge: mint installation token: status %d", resp.StatusCode)
	}

	var out installationToken
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("forge: decode mint response: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("forge: mint response carried no token")
	}

	s.store(out)
	return out.Token, nil
}

// store caches the minted token and computes its refresh boundary. A missing or
// non-positive expiry falls back to a conservative 1 h GitHub default so the
// cache still self-refreshes rather than pinning a possibly-dead token forever.
func (s *appTokenSource) store(out installationToken) {
	exp := out.ExpiresAt
	if exp.IsZero() || !exp.After(s.clock()) {
		exp = s.clock().Add(time.Hour)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = out.Token
	s.expsAt = exp
	s.refresh = exp.Add(-tokenRefreshLead)
	s.hasToken = true
}

// buildAppJWT hand-rolls an RS256 JWS over the App JWT claims (three base64url
// segments: header.payload.signature) using stdlib crypto only — no JWT
// dependency, matching the client's no-dependency posture (github.go:53-58).
func (s *appTokenSource) buildAppJWT(key *rsa.PrivateKey) (string, error) {
	now := s.clock()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iat": now.Add(-appJWTSkew).Unix(),
		"exp": now.Add(appJWTTTL).Unix(),
		"iss": s.cfg.AppID,
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("marshal header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal claims: %w", err)
	}

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// parseRSAPrivateKey decodes a PEM-encoded RSA private key, accepting both the
// PKCS#1 ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE KEY") encodings GitHub may
// hand out.
func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS#8 key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("PEM key is %T, not RSA", parsed)
	}
	return key, nil
}

// Compile-time proof that appTokenSource satisfies the TokenSource seam.
var _ TokenSource = (*appTokenSource)(nil)
