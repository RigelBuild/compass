//go:build unix

package server

// The Runner door's token-shape dispatch: a compact JWS goes to the projected
// token verifier only, never the hash lookup; anything else is the minted path.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	jose "github.com/go-jose/go-jose/v4"

	"github.com/RigelBuild/compass/go/internal/auth"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

const (
	testClusterIssuer = "https://oidc.prod.example"
	// testNodeRunnerID is auth.WorkloadRunnerID("prod", "ip-10-0-1-5.ec2.internal").
	testNodeRunnerID = "prod/ip-10-0-1-5_ec2_internal"
)

var testTokenEpoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// lookupSpy is a hash-lookup TokenResolver that records whether it ran.
type lookupSpy struct {
	calls int
	subj  store.Subject
}

func (s *lookupSpy) resolve(context.Context, string, store.SubjectKind) (store.Subject, error) {
	s.calls++
	return s.subj, nil
}

// verifierSpy is a runnerTokenVerifier that records whether it ran.
type verifierSpy struct {
	calls int
	subj  store.Subject
	err   error
}

func (v *verifierSpy) Verify(context.Context, string) (store.Subject, error) {
	v.calls++
	return v.subj, v.err
}

const jwsShaped = "eyJhbGciOiJFUzI1NiJ9.eyJpc3MiOiJ4In0.c2ln"

func TestRunnerResolveJWSNeverReachesHashLookup(t *testing.T) {
	runnerSubj := store.Subject{Kind: store.SubjectRunner, ID: testNodeRunnerID}

	t.Run("account door gets wrong kind", func(t *testing.T) {
		lookup, verifier := &lookupSpy{}, &verifierSpy{subj: runnerSubj}
		_, err := newRunnerResolve(lookup.resolve, verifier)(t.Context(), jwsShaped, store.SubjectAccount)
		if !errors.Is(err, auth.ErrWrongKind) {
			t.Fatalf("err = %v, want ErrWrongKind", err)
		}
		if lookup.calls != 0 || verifier.calls != 0 {
			t.Fatalf("lookup calls = %d, verifier calls = %d; want 0 and 0", lookup.calls, verifier.calls)
		}
	})

	t.Run("no clusters is not found", func(t *testing.T) {
		lookup := &lookupSpy{}
		_, err := newRunnerResolve(lookup.resolve, nil)(t.Context(), jwsShaped, store.SubjectRunner)
		if !errors.Is(err, auth.ErrTokenNotFound) {
			t.Fatalf("err = %v, want ErrTokenNotFound", err)
		}
		if lookup.calls != 0 {
			t.Fatalf("lookup calls = %d, want 0", lookup.calls)
		}
	})

	t.Run("runner door goes to the verifier", func(t *testing.T) {
		lookup, verifier := &lookupSpy{}, &verifierSpy{subj: runnerSubj}
		got, err := newRunnerResolve(lookup.resolve, verifier)(t.Context(), jwsShaped, store.SubjectRunner)
		if err != nil || got != runnerSubj {
			t.Fatalf("resolve = (%+v, %v), want (%+v, nil)", got, err, runnerSubj)
		}
		if lookup.calls != 0 || verifier.calls != 1 {
			t.Fatalf("lookup calls = %d, verifier calls = %d; want 0 and 1", lookup.calls, verifier.calls)
		}
	})

	t.Run("minted token goes to the hash lookup", func(t *testing.T) {
		minted := store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}
		lookup, verifier := &lookupSpy{subj: minted}, &verifierSpy{}
		got, err := newRunnerResolve(lookup.resolve, verifier)(t.Context(), "bWludGVkLXRva2Vu", store.SubjectRunner)
		if err != nil || got != minted {
			t.Fatalf("resolve = (%+v, %v), want (%+v, nil)", got, err, minted)
		}
		if lookup.calls != 1 || verifier.calls != 0 {
			t.Fatalf("lookup calls = %d, verifier calls = %d; want 1 and 0", lookup.calls, verifier.calls)
		}
	})
}

// TestRunnerDoorProjectedTokenCodes drives a real verifier through the mounted
// RunnerService door, so the code mapping is the one a Runner sees.
func TestRunnerDoorProjectedTokenCodes(t *testing.T) {
	key := newTestSigningKey(t, "k1")
	fetches := &refusingTransport{}
	verifier := newTestRunnerVerifier(t, key, &http.Client{Transport: fetches})
	url := mountRunnerDoor(t, newRunnerResolve((&lookupSpy{}).resolve, verifier))

	t.Run("valid token enrolls and learns its id", func(t *testing.T) {
		resp, err := newRunnerDoorClient(t, url, signTestToken(t, key, testClusterIssuer)).
			Enroll(t.Context(), connect.NewRequest(&compassv1internal.EnrollRequest{}))
		if err != nil {
			t.Fatalf("Enroll = %v, want success", err)
		}
		if got := resp.Msg.GetRunnerId(); got != testNodeRunnerID {
			t.Fatalf("runner_id = %q, want %q", got, testNodeRunnerID)
		}
	})

	t.Run("bad signature is unauthenticated", func(t *testing.T) {
		forged := newTestSigningKey(t, "k1") // same kid, different key
		_, err := newRunnerDoorClient(t, url, signTestToken(t, forged, testClusterIssuer)).
			Enroll(t.Context(), connect.NewRequest(&compassv1internal.EnrollRequest{}))
		if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
			t.Fatalf("code = %v (%v), want Unauthenticated", got, err)
		}
	})

	t.Run("keys unavailable is unavailable", func(t *testing.T) {
		_, err := newRunnerDoorClient(t, url, signTestToken(t, key, testUnfetchedIssuer)).
			Enroll(t.Context(), connect.NewRequest(&compassv1internal.EnrollRequest{}))
		if got := connect.CodeOf(err); got != connect.CodeUnavailable {
			t.Fatalf("code = %v (%v), want Unavailable", got, err)
		}
		// The refetch must go through the injected client, never the network.
		if fetches.count() == 0 {
			t.Fatal("verifier never used the injected client")
		}
	})
}

// refusingTransport fails every request at once and counts them, so a key
// fetch is observable and never leaves the process.
type refusingTransport struct {
	mu sync.Mutex
	n  int
}

func (r *refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	return nil, errors.New("network disabled in test")
}

func (r *refusingTransport) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// testUnfetchedIssuer names a fetched-JWKS cluster that is never started, so
// its keys are unavailable without any network.
const testUnfetchedIssuer = "https://oidc.unfetched.example"

type testSigningKey struct {
	kid string
	key *ecdsa.PrivateKey
}

func newTestSigningKey(t *testing.T, kid string) testSigningKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	return testSigningKey{kid: kid, key: k}
}

// newTestRunnerVerifier registers "prod" from a jwksFile holding key, and an
// unfetched cluster whose keys are unavailable. Fetches go through client.
func newTestRunnerVerifier(t *testing.T, key testSigningKey, client *http.Client) *auth.RunnerVerifier {
	t.Helper()
	path := writeTestJWKS(t, key)
	prod := testRunnerCluster("prod", testClusterIssuer)
	prod.JWKSFile = path
	unfetched := testRunnerCluster("unfetched", testUnfetchedIssuer)
	v, err := auth.NewRunnerVerifier([]auth.RunnerCluster{prod, unfetched}, "tenant-boot", client,
		func() time.Time { return testTokenEpoch })
	if err != nil {
		t.Fatalf("NewRunnerVerifier: %v", err)
	}
	return v
}

// writeTestJWKS writes key's public half as a JWKS file and returns its path.
func writeTestJWKS(t *testing.T, key testSigningKey) string {
	t.Helper()
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: key.key.Public(), KeyID: key.kid, Algorithm: string(jose.ES256), Use: "sig"}}}
	data, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshalling jwks: %v", err)
	}
	path := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing jwks: %v", err)
	}
	return path
}

func testRunnerCluster(name, issuer string) auth.RunnerCluster {
	return auth.RunnerCluster{
		Name: name, Issuer: issuer, Audience: "compass-runner",
		Namespace: "compass-runner", ServiceAccount: "compass-runner",
		MaxTokenLifetime: 600 * time.Second,
	}
}

// signTestToken signs a valid projected token for the node behind
// testNodeRunnerID, issued by iss at testTokenEpoch.
func signTestToken(t *testing.T, k testSigningKey, iss string) string {
	t.Helper()
	return signTestTokenAt(t, k, iss, testTokenEpoch)
}

// signTestTokenAt is signTestToken issued at iat, for a verifier on the real clock.
func signTestTokenAt(t *testing.T, k testSigningKey, iss string, iat time.Time) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: k.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", k.kid))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"iss": iss,
		"aud": []string{"compass-runner"},
		"sub": "system:serviceaccount:compass-runner:compass-runner",
		"iat": iat.Unix(),
		"nbf": iat.Unix(),
		"exp": iat.Add(600 * time.Second).Unix(),
		"kubernetes.io": map[string]any{
			"namespace":      "compass-runner",
			"node":           map[string]any{"name": "ip-10-0-1-5.ec2.internal"},
			"pod":            map[string]any{"name": "compass-runner-abcde"},
			"serviceaccount": map[string]any{"name": "compass-runner"},
		},
	})
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

// mountRunnerDoor serves the RunnerService door over resolve on an h2c test server.
func mountRunnerDoor(t *testing.T, resolve runnerhub.TokenResolver) string {
	t.Helper()
	otelIC, err := otelconnect.NewInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewInterceptor: %v", err)
	}
	hub := runnerhub.NewHub(nil, nil, nil, slog.Default())
	path, handler := runnerhub.NewMountedHandler(hub, resolve, nil, nil, otelIC)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

// newRunnerDoorClient dials baseURL over h2c with token as the bearer.
func newRunnerDoorClient(t *testing.T, baseURL, token string) compassv1internalconnect.RunnerServiceClient {
	t.Helper()
	tr := h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
	t.Cleanup(tr.CloseIdleConnections)
	return compassv1internalconnect.NewRunnerServiceClient(&http.Client{Transport: tr}, baseURL,
		connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				req.Header().Set("Authorization", "Bearer "+token)
				return next(ctx, req)
			}
		})))
}
