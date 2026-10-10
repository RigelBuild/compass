package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/RigelBuild/compass/go/internal/store"
)

const testTenant store.TenantID = "tenant-boot"

// fakeIssuer is an httptest TLS OIDC issuer whose JWKS and status tests change.
type fakeIssuer struct {
	srv *httptest.Server

	mu         sync.Mutex
	keys       []jose.JSONWebKey
	status     int
	jwksHits   int
	anyRequest int
}

func newFakeIssuer(t *testing.T, keys ...jose.JSONWebKey) *fakeIssuer {
	t.Helper()
	f := &fakeIssuer{keys: keys, status: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.anyRequest++
		status := f.status
		f.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		writeJSON(t, w, map[string]string{"issuer": f.srv.URL, "jwks_uri": f.srv.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.anyRequest++
		f.jwksHits++
		status := f.status
		set := jose.JSONWebKeySet{Keys: append([]jose.JSONWebKey(nil), f.keys...)}
		f.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		writeJSON(t, w, set)
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encoding response: %v", err)
	}
}

func (f *fakeIssuer) set(status int, keys ...jose.JSONWebKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
	if keys != nil {
		f.keys = keys
	}
}

func (f *fakeIssuer) hits() (jwks, any int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jwksHits, f.anyRequest
}

// trustingClient trusts every given issuer's self-signed certificate.
func trustingClient(issuers ...*fakeIssuer) *http.Client {
	pool := x509.NewCertPool()
	for _, f := range issuers {
		pool.AddCert(f.srv.Certificate())
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

// fakeClock is a settable now().
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// signingKey is a private key with its kid and algorithm.
type signingKey struct {
	kid string
	alg jose.SignatureAlgorithm
	key crypto.Signer
}

func (k signingKey) public() jose.JSONWebKey {
	return jose.JSONWebKey{Key: k.key.Public(), KeyID: k.kid, Algorithm: string(k.alg), Use: "sig"}
}

func newRSAKey(t *testing.T, kid string) signingKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	return signingKey{kid: kid, alg: jose.RS256, key: k}
}

func newECKey(t *testing.T, kid string) signingKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating EC key: %v", err)
	}
	return signingKey{kid: kid, alg: jose.ES256, key: k}
}

func sign(t *testing.T, k signingKey, claims map[string]any) string {
	t.Helper()
	opts := (&jose.SignerOptions{}).WithType("JWT")
	if k.kid != "" {
		opts = opts.WithHeader("kid", k.kid)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: k.alg, Key: k.key}, opts)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return signRaw(t, signer, claims)
}

func signRaw(t *testing.T, signer jose.Signer, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshalling claims: %v", err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	tok, err := obj.CompactSerialize()
	if err != nil {
		t.Fatalf("serializing: %v", err)
	}
	return tok
}

var testEpoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// goodClaims is a valid projected token for cluster issuer iss at testEpoch.
func goodClaims(iss string) map[string]any {
	return map[string]any{
		"iss": iss,
		"aud": []string{"compass-runner"},
		"sub": "system:serviceaccount:compass-runner:compass-runner",
		"iat": testEpoch.Unix(),
		"nbf": testEpoch.Unix(),
		"exp": testEpoch.Add(600 * time.Second).Unix(),
		"jti": "j1",
		"kubernetes.io": map[string]any{
			"namespace":      "compass-runner",
			"node":           map[string]any{"name": "ip-10-0-1-5.ec2.internal", "uid": "n1"},
			"pod":            map[string]any{"name": "compass-runner-abcde", "uid": "p1"},
			"serviceaccount": map[string]any{"name": "compass-runner", "uid": "s1"},
		},
	}
}

func testCluster(name, issuer string) RunnerCluster {
	return RunnerCluster{
		Name: name, Issuer: issuer, Audience: "compass-runner",
		Namespace: "compass-runner", ServiceAccount: "compass-runner",
		MaxTokenLifetime: 600 * time.Second,
	}
}

// newTestVerifier builds a verifier over one fake issuer and loads its keys
// synchronously, so no test waits on Start's goroutine.
func newTestVerifier(t *testing.T, f *fakeIssuer) (*RunnerVerifier, *fakeClock) {
	t.Helper()
	clock := &fakeClock{t: testEpoch}
	v, err := NewRunnerVerifier([]RunnerCluster{testCluster("prod", f.srv.URL)}, testTenant, trustingClient(f), clock.now)
	if err != nil {
		t.Fatalf("NewRunnerVerifier: %v", err)
	}
	return v, clock
}

func loadAll(t *testing.T, v *RunnerVerifier) {
	t.Helper()
	for _, ck := range v.clusters {
		v.refresh(t.Context(), ck)
	}
}

func TestVerifyAcceptsEachAlgorithm(t *testing.T) {
	for _, mk := range []func(*testing.T, string) signingKey{newRSAKey, newECKey} {
		k := mk(t, "k1")
		t.Run(string(k.alg), func(t *testing.T) {
			f := newFakeIssuer(t, k.public())
			v, _ := newTestVerifier(t, f)
			loadAll(t, v)
			subj, err := v.Verify(t.Context(), sign(t, k, goodClaims(f.srv.URL)))
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			want := store.Subject{Kind: store.SubjectRunner, ID: "prod/ip-10-0-1-5_ec2_internal", Tenant: testTenant}
			if subj != want {
				t.Fatalf("Verify = %+v, want %+v", subj, want)
			}
		})
	}
}

func TestVerifyRejectsBadTokens(t *testing.T) {
	k := newRSAKey(t, "k1")
	f := newFakeIssuer(t, k.public())
	v, _ := newTestVerifier(t, f)
	loadAll(t, v)
	iss := f.srv.URL

	k8s := func(c map[string]any) map[string]any { return c["kubernetes.io"].(map[string]any) }
	mutated := func(mut func(map[string]any)) string {
		c := goodClaims(iss)
		mut(c)
		return sign(t, k, c)
	}
	hmacSigner, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte("0123456789abcdef0123456789abcdef")},
		(&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatalf("HS256 signer: %v", err)
	}
	noneHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"k1"}`))
	nonePayload, err := json.Marshal(goodClaims(iss))
	if err != nil {
		t.Fatalf("marshalling claims: %v", err)
	}

	tests := []struct {
		name  string
		token string
	}{
		{"wrong aud", mutated(func(c map[string]any) { c["aud"] = []string{"other"} })},
		{"empty kid", sign(t, signingKey{alg: k.alg, key: k.key}, goodClaims(iss))},
		{"expired", mutated(func(c map[string]any) {
			c["iat"] = testEpoch.Add(-20 * time.Minute).Unix()
			c["nbf"] = c["iat"]
			c["exp"] = testEpoch.Add(-10*time.Minute + -time.Second).Unix()
		})},
		{"future nbf", mutated(func(c map[string]any) { c["nbf"] = testEpoch.Add(2 * time.Minute).Unix() })},
		{"absent exp", mutated(func(c map[string]any) { delete(c, "exp") })},
		{"absent iat", mutated(func(c map[string]any) { delete(c, "iat") })},
		{"exp - iat over max", mutated(func(c map[string]any) { c["exp"] = testEpoch.Add(601 * time.Second).Unix() })},
		{"wrong sub", mutated(func(c map[string]any) { c["sub"] = "system:serviceaccount:compass-runner:other" })},
		{"wrong namespace", mutated(func(c map[string]any) { k8s(c)["namespace"] = "other" })},
		{"wrong serviceaccount", mutated(func(c map[string]any) {
			k8s(c)["serviceaccount"] = map[string]any{"name": "other"}
		})},
		{"missing pod", mutated(func(c map[string]any) { delete(k8s(c), "pod") })},
		{"missing node", mutated(func(c map[string]any) { k8s(c)["node"] = map[string]any{"name": ""} })},
		{"alg none", noneHeader + "." + base64.RawURLEncoding.EncodeToString(nonePayload) + "."},
		{"HS256", signRaw(t, hmacSigner, goodClaims(iss))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.Verify(t.Context(), tt.token)
			if !errors.Is(err, errInvalidToken) || errors.Is(err, ErrKeysUnavailable) {
				t.Fatalf("Verify error = %v, want errInvalidToken and not ErrKeysUnavailable", err)
			}
		})
	}
}

func TestVerifyUnknownIssuerDoesNotFetch(t *testing.T) {
	k := newRSAKey(t, "k1")
	f := newFakeIssuer(t, k.public())
	v, _ := newTestVerifier(t, f)
	loadAll(t, v)
	_, before := f.hits()

	_, err := v.Verify(t.Context(), sign(t, k, goodClaims("https://unknown.example.test")))
	if !errors.Is(err, errInvalidToken) {
		t.Fatalf("Verify error = %v, want errInvalidToken", err)
	}
	if _, after := f.hits(); after != before {
		t.Fatalf("issuer requests = %d after an unknown-issuer token, want %d", after, before)
	}
}

func TestVerifyUnknownKidRefetchesOncePerCooldown(t *testing.T) {
	k := newRSAKey(t, "k1")
	stranger := newRSAKey(t, "k-unknown")
	f := newFakeIssuer(t, k.public())
	v, clock := newTestVerifier(t, f)
	loadAll(t, v)
	tok := sign(t, stranger, goodClaims(f.srv.URL))

	verifyCountingFetches := func(want int) {
		t.Helper()
		before, _ := f.hits()
		if _, err := v.Verify(t.Context(), tok); !errors.Is(err, errInvalidToken) {
			t.Fatalf("Verify error = %v, want errInvalidToken", err)
		}
		if after, _ := f.hits(); after-before != want {
			t.Fatalf("JWKS fetches = %d, want %d", after-before, want)
		}
	}
	verifyCountingFetches(1)
	verifyCountingFetches(0)
	clock.advance(5*time.Minute - time.Second)
	verifyCountingFetches(0)
	clock.advance(time.Second)
	verifyCountingFetches(1)
}

func TestVerifyRotatedKeyAfterRefetch(t *testing.T) {
	old := newRSAKey(t, "k1")
	rotated := newECKey(t, "k2")
	f := newFakeIssuer(t, old.public())
	v, _ := newTestVerifier(t, f)
	loadAll(t, v)

	f.set(http.StatusOK, old.public(), rotated.public())
	if _, err := v.Verify(t.Context(), sign(t, rotated, goodClaims(f.srv.URL))); err != nil {
		t.Fatalf("Verify with the rotated key: %v", err)
	}
}

func TestVerifyKeepsLastGoodKeysThroughFetchErrors(t *testing.T) {
	k := newRSAKey(t, "k1")
	f := newFakeIssuer(t, k.public())
	v, clock := newTestVerifier(t, f)
	loadAll(t, v)

	f.set(http.StatusServiceUnavailable)
	loadAll(t, v)
	clock.advance(23 * time.Hour)
	loadAll(t, v)
	claims := goodClaims(f.srv.URL)
	claims["iat"] = clock.now().Unix()
	claims["nbf"] = clock.now().Unix()
	claims["exp"] = clock.now().Add(10 * time.Minute).Unix()
	if _, err := v.Verify(t.Context(), sign(t, k, claims)); err != nil {
		t.Fatalf("Verify within 24h of a fetch error: %v", err)
	}

	clock.advance(time.Hour)
	loadAll(t, v)
	claims["iat"] = clock.now().Unix()
	claims["nbf"] = clock.now().Unix()
	claims["exp"] = clock.now().Add(10 * time.Minute).Unix()
	if _, err := v.Verify(t.Context(), sign(t, k, claims)); !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("Verify 24h after the first fetch error = %v, want ErrKeysUnavailable", err)
	}
}

func TestVerifyKeysUnavailableVersusBadSignature(t *testing.T) {
	k := newRSAKey(t, "k1")
	f := newFakeIssuer(t, k.public())
	f.set(http.StatusServiceUnavailable)
	v, _ := newTestVerifier(t, f)
	loadAll(t, v)

	if _, err := v.Verify(t.Context(), sign(t, k, goodClaims(f.srv.URL))); !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("Verify with no keys loaded = %v, want ErrKeysUnavailable", err)
	}

	f.set(http.StatusOK)
	loadAll(t, v)
	forger := newRSAKey(t, "k1") // same kid, different key
	_, err := v.Verify(t.Context(), sign(t, forger, goodClaims(f.srv.URL)))
	if !errors.Is(err, errInvalidToken) || errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("Verify with a bad signature = %v, want errInvalidToken and not ErrKeysUnavailable", err)
	}
}

func TestVerifyStartLoadsKeysInBackground(t *testing.T) {
	k := newRSAKey(t, "k1")
	f := newFakeIssuer(t, k.public())
	v, _ := newTestVerifier(t, f)
	tok := sign(t, k, goodClaims(f.srv.URL))
	if _, err := v.Verify(t.Context(), tok); !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("Verify before Start = %v, want ErrKeysUnavailable", err)
	}

	loaded := make(chan struct{})
	v.onLoad = func() { close(loaded) }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	v.Start(ctx)
	<-loaded
	if _, err := v.Verify(t.Context(), tok); err != nil {
		t.Fatalf("Verify after Start's first load: %v", err)
	}
}

func TestVerifySharedKeyFailsEveryHolder(t *testing.T) {
	for _, order := range []string{"A first", "B first"} {
		t.Run(order, func(t *testing.T) {
			shared := newECKey(t, "shared")
			bOnly := newECKey(t, "b-only")
			fa := newFakeIssuer(t, shared.public())
			fb := newFakeIssuer(t, shared.public(), bOnly.public())
			clock := &fakeClock{t: testEpoch}
			v, err := NewRunnerVerifier([]RunnerCluster{testCluster("a", fa.srv.URL), testCluster("b", fb.srv.URL)},
				testTenant, trustingClient(fa, fb), clock.now)
			if err != nil {
				t.Fatalf("NewRunnerVerifier: %v", err)
			}
			first, second := v.clusters[0], v.clusters[1]
			if order == "B first" {
				first, second = second, first
			}
			v.refresh(t.Context(), first)
			v.refresh(t.Context(), second)

			tokA := sign(t, shared, goodClaims(fa.srv.URL))
			tokB := sign(t, bOnly, goodClaims(fb.srv.URL))
			for name, tok := range map[string]string{"a": tokA, "b": tokB} {
				if _, err := v.Verify(t.Context(), tok); !errors.Is(err, ErrKeysUnavailable) {
					t.Fatalf("Verify for cluster %s = %v, want ErrKeysUnavailable", name, err)
				}
			}

			fb.set(http.StatusOK, bOnly.public())
			v.refresh(t.Context(), v.clusters[1])
			for name, tok := range map[string]string{"a": tokA, "b": tokB} {
				if _, err := v.Verify(t.Context(), tok); err != nil {
					t.Fatalf("Verify for cluster %s after B dropped the shared key: %v", name, err)
				}
			}
		})
	}
}

func TestNewRunnerVerifierReadsJWKSFile(t *testing.T) {
	k := newECKey(t, "k1")
	data, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{k.public()}})
	if err != nil {
		t.Fatalf("marshalling jwks: %v", err)
	}
	path := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing jwks: %v", err)
	}
	c := testCluster("static", "https://static.example.test")
	c.JWKSFile = path
	clock := &fakeClock{t: testEpoch}
	v, err := NewRunnerVerifier([]RunnerCluster{c}, testTenant, nil, clock.now)
	if err != nil {
		t.Fatalf("NewRunnerVerifier: %v", err)
	}
	if _, err := v.Verify(t.Context(), sign(t, k, goodClaims(c.Issuer))); err != nil {
		t.Fatalf("Verify against jwksFile: %v", err)
	}
}

// A jwksFile cluster's keys change only on rollout, so a forged kid must never
// start a fetch whose failure could later expire the file's keys.
func TestVerifyJWKSFileClusterNeverRefetches(t *testing.T) {
	k := newECKey(t, "k1")
	stranger := newECKey(t, "k-unknown")
	data, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{k.public()}})
	if err != nil {
		t.Fatalf("marshalling jwks: %v", err)
	}
	path := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing jwks: %v", err)
	}
	f := newFakeIssuer(t, k.public())
	c := testCluster("static", f.srv.URL)
	c.JWKSFile = path
	clock := &fakeClock{t: testEpoch}
	v, err := NewRunnerVerifier([]RunnerCluster{c}, testTenant, trustingClient(f), clock.now)
	if err != nil {
		t.Fatalf("NewRunnerVerifier: %v", err)
	}

	_, err = v.Verify(t.Context(), sign(t, stranger, goodClaims(c.Issuer)))
	if !errors.Is(err, errInvalidToken) || errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("Verify with an unknown kid = %v, want errInvalidToken and not ErrKeysUnavailable", err)
	}
	clock.advance(25 * time.Hour)
	claims := goodClaims(c.Issuer)
	claims["iat"] = clock.now().Unix()
	claims["nbf"] = clock.now().Unix()
	claims["exp"] = clock.now().Add(10 * time.Minute).Unix()
	if _, err := v.Verify(t.Context(), sign(t, k, claims)); err != nil {
		t.Fatalf("Verify a known kid 25h after an unknown-kid token: %v", err)
	}
	if _, requests := f.hits(); requests != 0 {
		t.Fatalf("issuer requests = %d, want 0 for a jwksFile cluster", requests)
	}
}

func TestWorkloadRunnerIDAndShape(t *testing.T) {
	if got := WorkloadRunnerID("prod-eks", "ip-10-0-1-5.ec2.internal"); got != "prod-eks/ip-10-0-1-5_ec2_internal" {
		t.Fatalf("WorkloadRunnerID = %q", got)
	}
	for tok, want := range map[string]bool{"a.b.c": true, "aGVsbG8": false, "a.b": false, "a.b.c.d": false} {
		if got := LooksLikeJWT(tok); got != want {
			t.Fatalf("LooksLikeJWT(%q) = %v, want %v", tok, got, want)
		}
	}
}
