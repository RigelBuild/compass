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
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	// hold, when set, parks the next JWKS response (served from the key set at
	// request time) until it is closed; entered reports the request arrived.
	hold    chan struct{}
	entered chan struct{}
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
		hold, entered := f.hold, f.entered
		f.hold, f.entered = nil, nil
		f.mu.Unlock()
		if hold != nil {
			close(entered)
			<-hold
		}
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

// holdNext parks the next JWKS response; it returns the arrival signal and the
// release.
func (f *fakeIssuer) holdNext() (entered <-chan struct{}, release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hold, arrived := make(chan struct{}), make(chan struct{})
	f.hold, f.entered = hold, arrived
	return arrived, sync.OnceFunc(func() { close(hold) })
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
	tlsConfig := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}
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
	hmacKey := []byte("0123456789abcdef0123456789abcdef")
	hmacSigner, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: hmacKey},
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
	f.set(http.StatusServiceUnavailable)
	if _, err := v.Verify(t.Context(), tok); !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("Verify before Start with the issuer down = %v, want ErrKeysUnavailable", err)
	}
	f.set(http.StatusOK)

	loaded := make(chan struct{})
	v.onLoad = sync.OnceFunc(func() { close(loaded) })
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
			_, aBefore := fa.hits()
			_, bBefore := fb.hits()
			for name, tok := range map[string]string{"a": tokA, "b": tokB} {
				if _, err := v.Verify(t.Context(), tok); !errors.Is(err, ErrKeysUnavailable) {
					t.Fatalf("Verify for cluster %s = %v, want ErrKeysUnavailable", name, err)
				}
			}
			// Sharing a key is not missing keys: a fail-closed cluster must not fetch.
			if _, a := fa.hits(); a != aBefore {
				t.Fatalf("cluster a requests = %d during fail-closed Verify, want %d", a, aBefore)
			}
			if _, b := fb.hits(); b != bBefore {
				t.Fatalf("cluster b requests = %d during fail-closed Verify, want %d", b, bBefore)
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

// A failed startup fetch must not lock Runners out until the hourly refresh.
func TestVerifyRefetchesWhenNoKeysLoaded(t *testing.T) {
	k := newRSAKey(t, "k1")
	f := newFakeIssuer(t, k.public())
	f.set(http.StatusServiceUnavailable)
	v, _ := newTestVerifier(t, f)
	loadAll(t, v)

	f.set(http.StatusOK)
	if _, err := v.Verify(t.Context(), sign(t, k, goodClaims(f.srv.URL))); err != nil {
		t.Fatalf("Verify after the issuer recovered, before any refresh: %v", err)
	}
}

func TestVerifyTokenSizeLimit(t *testing.T) {
	k := newECKey(t, "k1")
	f := newFakeIssuer(t, k.public())
	v, _ := newTestVerifier(t, f)
	loadAll(t, v)

	claims := goodClaims(f.srv.URL)
	claims["pad"] = strings.Repeat("x", 10<<10)
	if tok := sign(t, k, claims); len(tok) > maxRunnerTokenBytes {
		t.Fatalf("padded token is %d bytes, over the cap the test means to stay under", len(tok))
	} else if _, err := v.Verify(t.Context(), tok); err != nil {
		t.Fatalf("Verify a %d-byte token: %v", len(tok), err)
	}

	atCap := strings.Repeat("a", maxRunnerTokenBytes-4) + ".b.c"
	if _, err := v.Verify(t.Context(), atCap); errors.Is(err, errRunnerTokenTooLarge) {
		t.Fatalf("Verify a token of exactly %d bytes = %v, want no size rejection", maxRunnerTokenBytes, err)
	}
	overCap := strings.Repeat("a", maxRunnerTokenBytes-3) + ".b.c"
	_, err := v.Verify(t.Context(), overCap)
	if !errors.Is(err, errRunnerTokenTooLarge) || !errors.Is(err, errInvalidToken) {
		t.Fatalf("Verify a token of %d bytes = %v, want errRunnerTokenTooLarge and errInvalidToken", len(overCap), err)
	}
}

// An older fetch response must never overwrite a newer one.
func TestVerifyFetchesForOneClusterNeverOverlap(t *testing.T) {
	old := newECKey(t, "k1")
	rotated := newECKey(t, "k2")
	f := newFakeIssuer(t, old.public())
	v, _ := newTestVerifier(t, f)

	entered, release := f.holdNext()
	defer release()
	queued := make(chan struct{})
	v.onFetchQueued = sync.OnceFunc(func() { close(queued) })
	loaded := make(chan struct{}, 2)
	v.onLoad = func() { loaded <- struct{}{} }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	v.Start(ctx)
	<-entered // the scheduled fetch holds the old set

	f.set(http.StatusOK, old.public(), rotated.public())
	tok := sign(t, rotated, goodClaims(f.srv.URL))
	verified := make(chan error, 1)
	go func() {
		_, err := v.Verify(t.Context(), tok)
		verified <- err
	}()
	<-queued // the re-fetch waits behind the held scheduled fetch
	release()
	<-loaded
	<-loaded
	if err := <-verified; err != nil {
		t.Fatalf("Verify with the rotated key during a scheduled fetch: %v", err)
	}
	if _, err := v.Verify(t.Context(), tok); err != nil {
		t.Fatalf("Verify with the rotated key after both fetches applied: %v", err)
	}
}

func TestVerifyCancelledWaiterLeavesSharedRefetchRunning(t *testing.T) {
	old := newECKey(t, "k1")
	rotated := newECKey(t, "k2")
	f := newFakeIssuer(t, old.public())
	v, _ := newTestVerifier(t, f)
	loadAll(t, v)

	f.set(http.StatusOK, old.public(), rotated.public())
	entered, release := f.holdNext()
	defer release()
	joined := make(chan struct{}, 2)
	v.onRefetchWait = func() { joined <- struct{}{} }
	tok := sign(t, rotated, goodClaims(f.srv.URL))

	ctxA, cancelA := context.WithCancel(t.Context())
	doneA := make(chan error, 1)
	go func() {
		_, err := v.Verify(ctxA, tok)
		doneA <- err
	}()
	<-entered
	<-joined
	doneB := make(chan error, 1)
	go func() {
		_, err := v.Verify(t.Context(), tok)
		doneB <- err
	}()
	<-joined

	cancelA()
	select {
	case err := <-doneA:
		if err == nil {
			t.Fatal("cancelled Verify succeeded before the re-fetch finished")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled Verify still waiting on the shared re-fetch")
	}
	release()
	if err := <-doneB; err != nil {
		t.Fatalf("Verify sharing the re-fetch: %v", err)
	}
}

func TestParseJWKSSkipsUnsupportedKeys(t *testing.T) {
	k := newECKey(t, "good")
	good, err := json.Marshal(k.public())
	if err != nil {
		t.Fatalf("marshalling key: %v", err)
	}
	unsupported := `{"kty":"OKP","crv":"X448","x":"AAAA","kid":"x448"}`
	broken := `{"kty":"RSA","n":"!!","e":"AQAB","kid":"broken"}`
	set, err := parseJWKS([]byte(`{"keys":[` + unsupported + `,` + string(good) + `,` + broken + `]}`))
	if err != nil {
		t.Fatalf("parseJWKS with one usable key: %v", err)
	}
	if len(set.jwks.Keys) != 1 || set.jwks.Keys[0].KeyID != "good" {
		t.Fatalf("parseJWKS kept %d keys, want only kid good", len(set.jwks.Keys))
	}
	if _, err := parseJWKS([]byte(`{"keys":[` + unsupported + `,` + broken + `]}`)); err == nil {
		t.Fatal("parseJWKS with no usable key succeeded")
	}
}

func TestClusterHTTPClientNilTransportKeepsDefaults(t *testing.T) {
	ca := filepath.Join(t.TempDir(), "ca.pem")
	f := newFakeIssuer(t)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	if err := os.WriteFile(ca, pemBytes, 0o600); err != nil {
		t.Fatalf("writing ca: %v", err)
	}
	hc, err := clusterHTTPClient(&http.Client{}, ca)
	if err != nil {
		t.Fatalf("clusterHTTPClient: %v", err)
	}
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", hc.Transport)
	}
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is %T", http.DefaultTransport)
	}
	if tr.TLSHandshakeTimeout != def.TLSHandshakeTimeout || tr.IdleConnTimeout != def.IdleConnTimeout {
		t.Fatalf("transport timeouts = %s/%s, want the default transport's %s/%s",
			tr.TLSHandshakeTimeout, tr.IdleConnTimeout, def.TLSHandshakeTimeout, def.IdleConnTimeout)
	}
}

func TestNewRunnerVerifierNilNowUsesWallClock(t *testing.T) {
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
	v, err := NewRunnerVerifier([]RunnerCluster{c}, testTenant, nil, nil)
	if err != nil {
		t.Fatalf("NewRunnerVerifier: %v", err)
	}
	claims := goodClaims(c.Issuer)
	now := time.Now()
	claims["iat"], claims["nbf"], claims["exp"] = now.Unix(), now.Unix(), now.Add(10*time.Minute).Unix()
	if _, err := v.Verify(t.Context(), sign(t, k, claims)); err != nil {
		t.Fatalf("Verify a token valid now: %v", err)
	}
}

func TestFetchErrorOmitsQueryString(t *testing.T) {
	f := newFakeIssuer(t)
	f.set(http.StatusServiceUnavailable)
	_, err := fetchHTTPS(t.Context(), trustingClient(f), f.srv.URL+"/keys?access_token=s3cr3t")
	if err == nil {
		t.Fatal("fetchHTTPS against a 503 succeeded")
	}
	if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "access_token") {
		t.Fatalf("fetch error leaks the query string: %v", err)
	}
	_, err = fetchHTTPS(t.Context(), trustingClient(f), "http://example.test/keys?access_token=s3cr3t")
	if err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("non-https fetch error = %v, want a rejection without the query string", err)
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
