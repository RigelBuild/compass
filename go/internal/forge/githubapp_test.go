package forge

// Unit tests for the GitHub App installation-token source, driven by an
// httptest.Server standing in for the access-tokens mint endpoint (no network).
// Covers the T1 test cycle: App JWT header/claims/signature verify against a
// test RSA key; mint caches (a second Token = no HTTP); refresh before the
// expiry boundary via an injected clock; Invalidate forces a re-mint; a 401 on
// mint surfaces as an error (not a panic); singleflight under concurrent Token
// (N goroutines -> one mint HTTP call).
// context.Background() here is the test root — the sanctioned F-ttsr exemption.

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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

// testAppKey generates an RSA key once per test run for signing/verification.
func testAppKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	return key
}

// pemPKCS1 encodes a key as a PKCS#1 "RSA PRIVATE KEY" PEM block.
func pemPKCS1(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// mintServer scripts the access-tokens endpoint: it counts hits, captures the
// last presented App JWT, and returns the configured token/expiry (or a status
// override). expiresAt is resolved lazily per request via nowFn so a test can
// advance the clock and assert the fresh expiry.
type mintServer struct {
	srv      *httptest.Server
	hits     int64
	lastJWT  string
	token    string
	status   int
	nowFn    func() time.Time
	lifetime time.Duration
}

func newMintServer(t *testing.T, token string, nowFn func() time.Time) *mintServer {
	t.Helper()
	m := &mintServer{token: token, status: http.StatusCreated, nowFn: nowFn, lifetime: time.Hour}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&m.hits, 1)
		auth := r.Header.Get("Authorization")
		m.lastJWT = strings.TrimPrefix(auth, "Bearer ")
		if m.status != http.StatusCreated {
			w.WriteHeader(m.status)
			_, _ = w.Write([]byte(`{"message":"scripted failure"}`))
			return
		}
		exp := m.nowFn().Add(m.lifetime)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(installationToken{Token: m.token, ExpiresAt: exp})
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// rewriteTransport routes every request to the test server regardless of the
// https://<host>/api/v3 URL the source builds, so the source's real URL
// construction is exercised while the request still lands on httptest.
type rewriteTransport struct {
	target *url.URL
}

func newAppSource(t *testing.T, m *mintServer, cfg GitHubAppConfig) TokenSource {
	t.Helper()
	target, err := url.Parse(m.srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	cfg.Client = &http.Client{Transport: &rewriteTransport{target: target}}
	src, err := NewAppTokenSource(cfg)
	if err != nil {
		t.Fatalf("NewAppTokenSource: %v", err)
	}
	return src
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = rt.target.Scheme
	req.URL.Host = rt.target.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestAppTokenSourceJWTClaimsAndSignature(t *testing.T) {
	key := testAppKey(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	m := newMintServer(t, "ghs_installtoken", clock)
	src := newAppSource(t, m, GitHubAppConfig{
		AppID:          1234,
		InstallationID: 5678,
		PrivateKey:     func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
		Host:           "github.com",
		Clock:          clock,
	})

	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "ghs_installtoken" {
		t.Fatalf("token = %q, want ghs_installtoken", tok)
	}

	header, claims := verifyJWT(t, m.lastJWT, &key.PublicKey)
	if header["alg"] != "RS256" {
		t.Errorf("alg = %v, want RS256", header["alg"])
	}
	if header["typ"] != "JWT" {
		t.Errorf("typ = %v, want JWT", header["typ"])
	}
	if iss := int64(claims["iss"].(float64)); iss != 1234 {
		t.Errorf("iss = %d, want 1234", iss)
	}
	iat := int64(claims["iat"].(float64))
	exp := int64(claims["exp"].(float64))
	// iat is back-dated 60s for skew; exp is ~10 min out.
	if wantIat := now.Add(-appJWTSkew).Unix(); iat != wantIat {
		t.Errorf("iat = %d, want %d", iat, wantIat)
	}
	if wantExp := now.Add(appJWTTTL).Unix(); exp != wantExp {
		t.Errorf("exp = %d, want %d", exp, wantExp)
	}
}

func TestAppTokenSourceCaches(t *testing.T) {
	key := testAppKey(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	m := newMintServer(t, "ghs_cache", clock)
	src := newAppSource(t, m, GitHubAppConfig{
		AppID:          1,
		InstallationID: 2,
		PrivateKey:     func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
		Clock:          clock,
	})

	for i := range 3 {
		if _, err := src.Token(context.Background()); err != nil {
			t.Fatalf("Token #%d: %v", i, err)
		}
	}
	if got := atomic.LoadInt64(&m.hits); got != 1 {
		t.Fatalf("mint hits = %d, want 1 (cache should serve calls 2 and 3)", got)
	}
}

func TestAppTokenSourceRefreshBeforeExpiry(t *testing.T) {
	key := testAppKey(t)
	base := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	var cur atomic.Pointer[time.Time]
	cur.Store(&base)
	clock := func() time.Time { return *cur.Load() }
	m := newMintServer(t, "ghs_refresh", clock)
	src := newAppSource(t, m, GitHubAppConfig{
		AppID:          1,
		InstallationID: 2,
		PrivateKey:     func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
		Clock:          clock,
	})

	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("first Token: %v", err)
	}
	// Just before the refresh boundary (exp - 5min): still cached, no new mint.
	beforeBoundary := base.Add(time.Hour - tokenRefreshLead - time.Second)
	cur.Store(&beforeBoundary)
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("pre-boundary Token: %v", err)
	}
	if got := atomic.LoadInt64(&m.hits); got != 1 {
		t.Fatalf("mint hits before boundary = %d, want 1", got)
	}
	// Past the refresh boundary: re-mint.
	afterBoundary := base.Add(time.Hour - tokenRefreshLead + time.Second)
	cur.Store(&afterBoundary)
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("post-boundary Token: %v", err)
	}
	if got := atomic.LoadInt64(&m.hits); got != 2 {
		t.Fatalf("mint hits after boundary = %d, want 2 (re-mint)", got)
	}
}

func TestAppTokenSourceInvalidateForcesRemint(t *testing.T) {
	key := testAppKey(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	m := newMintServer(t, "ghs_inval", clock)
	src := newAppSource(t, m, GitHubAppConfig{
		AppID:          1,
		InstallationID: 2,
		PrivateKey:     func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
		Clock:          clock,
	})

	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("first Token: %v", err)
	}
	src.Invalidate()
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("post-invalidate Token: %v", err)
	}
	if got := atomic.LoadInt64(&m.hits); got != 2 {
		t.Fatalf("mint hits = %d, want 2 (Invalidate should force a re-mint)", got)
	}
}

func TestAppTokenSourceMintErrorSurfaces(t *testing.T) {
	key := testAppKey(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	m := newMintServer(t, "unused", clock)
	m.status = http.StatusUnauthorized
	src := newAppSource(t, m, GitHubAppConfig{
		AppID:          1,
		InstallationID: 2,
		PrivateKey:     func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
		Clock:          clock,
	})

	tok, err := src.Token(context.Background())
	if err == nil {
		t.Fatalf("Token returned nil error on 401, got token %q", tok)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want it to mention 401", err)
	}
}

func TestAppTokenSourceSingleflight(t *testing.T) {
	key := testAppKey(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	// Gate the handler so all goroutines pile into the flight before any mint
	// completes — event-gated (release channel), not sleep-gated.
	release := make(chan struct{})
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(installationToken{Token: "ghs_sf", ExpiresAt: now.Add(time.Hour)})
	}))
	t.Cleanup(srv.Close)

	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	src, err := NewAppTokenSource(GitHubAppConfig{
		AppID:          1,
		InstallationID: 2,
		PrivateKey:     func(context.Context) ([]byte, error) { return pemPKCS1(t, key), nil },
		Clock:          clock,
		Client:         &http.Client{Transport: &rewriteTransport{target: target}},
	})
	if err != nil {
		t.Fatalf("NewAppTokenSource: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	var started sync.WaitGroup
	wg.Add(n)
	started.Add(n)
	errs := make(chan error, n)
	for range n {
		go func() {
			defer wg.Done()
			started.Done()
			tok, err := src.Token(context.Background())
			if err != nil {
				errs <- err
				return
			}
			if tok != "ghs_sf" {
				errs <- fmt.Errorf("token = %q, want ghs_sf", tok)
			}
		}()
	}
	started.Wait()
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("goroutine error: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("mint hits = %d, want 1 (singleflight should collapse the stampede)", got)
	}
}

// verifyJWT splits a compact JWS, verifies its RS256 signature against pub, and
// returns the decoded header and claims maps.
func verifyJWT(t *testing.T, jwt string, pub *rsa.PublicKey) (map[string]any, map[string]any) {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d segments, want 3", len(parts))
	}
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("verify RS256 signature: %v", err)
	}
	header := decodeSegment(t, parts[0])
	claims := decodeSegment(t, parts[1])
	return header, claims
}

func decodeSegment(t *testing.T, seg string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("decode segment: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal segment: %v", err)
	}
	return m
}
