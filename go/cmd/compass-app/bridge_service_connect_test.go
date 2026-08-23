//go:build unix

package main

// The T5.3 Connect gate: the bound Connect probe classified against real
// httptest TLS servers, one row per sealed failure kind plus the success path.
// Each row stands up a self-signed TLS stub (mirroring the T5.1 tlsStubServer
// pattern) serving the two probed RPCs through a real
// compassv1connect.CompassServiceHandler, builds a bridge.Target pinned to that
// server's cert, and asserts the classification, the token side effects
// (tokenstore write + SetBearer armed), and — for every row — that the token
// string never leaks into Message or captured log output.

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/bridge"
	"github.com/RigelBuild/compass/go/internal/tokenstore"
)

const connectTestTimeout = 5 * time.Second

const probeToken = "s3cr3t-connect-token"

// authRecorder captures the Authorization header of the last WhoAmI the stub
// served, so the success path can assert the target is armed with the bearer.
type authRecorder struct {
	mu   sync.Mutex
	last string
}

func (r *authRecorder) set(v string) {
	r.mu.Lock()
	r.last = v
	r.mu.Unlock()
}

func (r *authRecorder) get() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// syncBuffer is a concurrency-safe log sink. The row redirects the global log
// writer here to assert the token never leaks into a log line, and the httptest
// server logs (e.g. a TLS-handshake error on the bad-cert row) fire on the
// server's own goroutine — so the sink is written concurrently with the test's
// read. A bare strings.Builder would race under -race; the mutex serializes both.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// connectStub is a CompassService handler whose two probed RPCs are supplied per
// row; every other method returns CodeUnimplemented via the embedded base.
type connectStub struct {
	compassv1connect.UnimplementedCompassServiceHandler
	getServerInfo func(context.Context, *connect.Request[compassv1.GetServerInfoRequest]) (*connect.Response[compassv1.GetServerInfoResponse], error)
	whoAmI        func(context.Context, *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error)
	// rec, when set, captures the Authorization header of every probed RPC the
	// stub serves, so a row can assert the target is armed (success) or disarmed
	// (failure) by inspecting what the last forwarded request carried.
	rec *authRecorder
}

func (s connectStub) GetServerInfo(ctx context.Context, req *connect.Request[compassv1.GetServerInfoRequest]) (*connect.Response[compassv1.GetServerInfoResponse], error) {
	if s.rec != nil {
		s.rec.set(req.Header().Get("Authorization"))
	}
	return s.getServerInfo(ctx, req)
}

func (s connectStub) WhoAmI(ctx context.Context, req *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
	if s.rec != nil {
		s.rec.set(req.Header().Get("Authorization"))
	}
	return s.whoAmI(ctx, req)
}

// okServerInfo replies with the app's own API version so the probe passes the
// version gate.
func okServerInfo(_ context.Context, _ *connect.Request[compassv1.GetServerInfoRequest]) (*connect.Response[compassv1.GetServerInfoResponse], error) {
	return connect.NewResponse(&compassv1.GetServerInfoResponse{Version: "1.2.3", ApiVersion: clientAPIVersion}), nil
}

// mismatchServerInfo replies with a different API version so the probe fails the
// exact-match version gate.
func mismatchServerInfo(_ context.Context, _ *connect.Request[compassv1.GetServerInfoRequest]) (*connect.Response[compassv1.GetServerInfoResponse], error) {
	return connect.NewResponse(&compassv1.GetServerInfoResponse{Version: "9.9.9", ApiVersion: "compass.v2"}), nil
}

// emptyAccountWhoAmI replies OK but with an empty account id, which Connect
// rejects as an invalid identity.
func emptyAccountWhoAmI(_ context.Context, _ *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
	return connect.NewResponse(&compassv1.WhoAmIResponse{AccountId: ""}), nil
}

// unavailableServerInfo fails the GetServerInfo probe with a transport-style
// error over the trusted, reachable TLS server: a reachable server that errors
// on the first probe. It exercises the GetServerInfo-error disarm branch
// (classified `other`) with a target that can still be re-probed to prove the
// bearer was cleared.
func unavailableServerInfo(_ context.Context, _ *connect.Request[compassv1.GetServerInfoRequest]) (*connect.Response[compassv1.GetServerInfoResponse], error) {
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("stub: server info unavailable"))
}

// unauthWhoAmI is the shared bad-token WhoAmI: it fails closed with
// CodeUnauthenticated regardless of the bearer.
func unauthWhoAmI(_ context.Context, _ *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
	return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("stub: no caller"))
}

// connectTLSStub starts an httptest TLS server serving stub over a real connect
// handler and returns its cert PEM for pinning. Torn down via t.Cleanup.
func connectTLSStub(t *testing.T, stub connectStub) (srv *httptest.Server, certPEM []byte) {
	t.Helper()
	mux := http.NewServeMux()
	path, handler := compassv1connect.NewCompassServiceHandler(stub)
	mux.Handle(path, handler)

	srv = httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	leaf := srv.Certificate()
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})
	return srv, certPEM
}

// connectService builds a bridgeService with a TLS target pinned to serverURL/caPEM
// and a fresh temp-dir tokenstore.
func connectService(t *testing.T, serverURL string, caPEM []byte) (*bridgeService, tokenstore.Store) {
	t.Helper()
	target, err := bridge.NewTLSTarget(serverURL, caPEM)
	if err != nil {
		t.Fatalf("NewTLSTarget: %v", err)
	}
	store := tokenstore.New(t.TempDir())
	svc := newBridgeService(nil, nil, target, store)
	return svc, store
}

// connectCase is one row of the Connect classification table: a stub behaviour,
// a target-wiring toggle, and the expected outcome.
type connectCase struct {
	name         string
	whoAmI       func(*authRecorder) func(context.Context, *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error)
	serverInfo   func(context.Context, *connect.Request[compassv1.GetServerInfoRequest]) (*connect.Response[compassv1.GetServerInfoResponse], error)
	token        string
	badURL       bool // point the target at an unreachable URL
	untrusted    bool // do not pin the server's cert
	preStore     bool // pre-store probeToken for the empty-token path
	nilService   bool // build the service with nil target+tokens (test wiring)
	wantKind     string
	wantOK       bool
	wantAccount  string
	wantDisarmed bool // a reachable failure must leave the target disarmed
}

// staticWhoAmI adapts a plain WhoAmI handler into the per-row recorder-taking
// shape for the rows that ignore the recorder.
func staticWhoAmI(h func(context.Context, *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error)) func(*authRecorder) func(context.Context, *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
	return func(*authRecorder) func(context.Context, *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
		return h
	}
}

// recordingWhoAmI returns a WhoAmI that records the forwarded Authorization
// header (so a success row can assert the probe was armed) and replies with id.
func recordingWhoAmI(id string) func(*authRecorder) func(context.Context, *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
	return func(rec *authRecorder) func(context.Context, *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
		return func(_ context.Context, req *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
			rec.set(req.Header().Get("Authorization"))
			return connect.NewResponse(&compassv1.WhoAmIResponse{AccountId: id}), nil
		}
	}
}

func connectClassificationCases() []connectCase {
	return []connectCase{
		{name: "bad-url unreachable host", serverInfo: okServerInfo, whoAmI: staticWhoAmI(unauthWhoAmI), token: probeToken, badURL: true, wantKind: connectKindBadURL},
		{name: "bad-cert self-signed not pinned", serverInfo: okServerInfo, whoAmI: staticWhoAmI(unauthWhoAmI), token: probeToken, untrusted: true, wantKind: connectKindBadCert},
		{name: "bad-token WhoAmI unauthenticated", serverInfo: okServerInfo, whoAmI: staticWhoAmI(unauthWhoAmI), token: probeToken, wantKind: connectKindBadToken, wantDisarmed: true},
		{name: "version-mismatch", serverInfo: mismatchServerInfo, whoAmI: staticWhoAmI(unauthWhoAmI), token: probeToken, wantKind: connectKindVersionMismatch, wantDisarmed: true},
		{name: "empty account id rejected as other", serverInfo: okServerInfo, whoAmI: staticWhoAmI(emptyAccountWhoAmI), token: probeToken, wantKind: connectKindOther, wantDisarmed: true},
		{name: "server-info transport error disarms", serverInfo: unavailableServerInfo, whoAmI: staticWhoAmI(unauthWhoAmI), token: probeToken, wantKind: connectKindOther, wantDisarmed: true},
		{name: "success path", serverInfo: okServerInfo, whoAmI: recordingWhoAmI("acct-123"), token: probeToken, wantOK: true, wantAccount: "acct-123"},
		{name: "empty token with stored succeeds", serverInfo: okServerInfo, whoAmI: recordingWhoAmI("acct-stored"), token: "", preStore: true, wantOK: true, wantAccount: "acct-stored"},
		{name: "empty token nothing stored", serverInfo: okServerInfo, whoAmI: staticWhoAmI(unauthWhoAmI), token: "", wantKind: connectKindBadToken},
		{name: "nil target fails closed", serverInfo: okServerInfo, whoAmI: staticWhoAmI(unauthWhoAmI), token: probeToken, nilService: true, wantKind: connectKindOther},
	}
}

func TestConnectClassification(t *testing.T) {
	for _, tc := range connectClassificationCases() {
		t.Run(tc.name, func(t *testing.T) {
			rec := &authRecorder{}
			stub := connectStub{rec: rec, getServerInfo: tc.serverInfo, whoAmI: tc.whoAmI(rec)}
			srv, certPEM := connectTLSStub(t, stub)

			serverURL := srv.URL
			caPEM := certPEM
			if tc.untrusted {
				caPEM = nil // system roots do not trust the self-signed cert
			}
			if tc.badURL {
				serverURL = "https://127.0.0.1:1" // nothing listening
			}

			svc, store := connectService(t, serverURL, caPEM)
			if tc.nilService {
				// Test wiring: the service is bound with no target/tokenstore.
				// Connect must fail closed rather than nil-deref.
				svc = newBridgeService(nil, nil, nil, nil)
			}
			if tc.preStore {
				if err := store.Write(serverURL, probeToken); err != nil {
					t.Fatalf("pre-store Write: %v", err)
				}
			}

			// Capture log output to assert the token never leaks into a log line.
			var logBuf syncBuffer
			orig := log.Writer()
			log.SetOutput(&logBuf)
			t.Cleanup(func() { log.SetOutput(orig) })

			ctx, cancel := context.WithTimeout(context.Background(), connectTestTimeout)
			defer cancel()

			res := svc.Connect(ctx, connectRequest{Token: tc.token})

			if res.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q (Message=%q)", res.Kind, tc.wantKind, res.Message)
			}
			if res.OK != tc.wantOK {
				t.Errorf("OK = %v, want %v", res.OK, tc.wantOK)
			}
			if res.AccountID != tc.wantAccount {
				t.Errorf("AccountID = %q, want %q", res.AccountID, tc.wantAccount)
			}

			// (g) the token must never appear in Message or the log output.
			if strings.Contains(res.Message, probeToken) {
				t.Errorf("token leaked into Message: %q", res.Message)
			}
			if strings.Contains(logBuf.String(), probeToken) {
				t.Errorf("token leaked into log output: %q", logBuf.String())
			}

			if tc.wantDisarmed {
				assertDisarmed(t, svc, rec)
			}

			if tc.wantOK {
				assertConnectSuccess(t, svc, store, serverURL, rec)
			}
		})
	}
}

// assertConnectSuccess checks the success side effects: the token is persisted,
// the probe carried the candidate bearer, and the target is left armed.
func assertConnectSuccess(t *testing.T, svc *bridgeService, store tokenstore.Store, serverURL string, rec *authRecorder) {
	t.Helper()
	got, err := store.Read(serverURL)
	if err != nil {
		t.Fatalf("post-connect Read: %v", err)
	}
	if got != probeToken {
		t.Errorf("stored token = %q, want %q", got, probeToken)
	}
	// The probe's WhoAmI carried the candidate bearer, proving the target was
	// armed via SetBearer for the probe...
	if want := "Bearer " + probeToken; rec.get() != want {
		t.Errorf("probe Authorization = %q, want %q", rec.get(), want)
	}
	// ...and it is LEFT armed on success: a fresh forwarded request through the
	// same target still carries the bearer.
	assertStillArmed(t, svc, rec)
}

// assertStillArmed forwards a fresh WhoAmI through the service's target and
// asserts the success-armed bearer is still injected (SetBearer left armed).
func assertStillArmed(t *testing.T, svc *bridgeService, rec *authRecorder) {
	t.Helper()
	cc := compassv1connect.NewCompassServiceClient(svc.target.Client())
	ctx, cancel := context.WithTimeout(context.Background(), connectTestTimeout)
	defer cancel()
	if _, err := cc.WhoAmI(ctx, connect.NewRequest(&compassv1.WhoAmIRequest{})); err != nil {
		t.Fatalf("re-probe WhoAmI: %v", err)
	}
	if want := "Bearer " + probeToken; rec.get() != want {
		t.Errorf("target disarmed after success: Authorization = %q, want %q", rec.get(), want)
	}
}

// assertDisarmed forwards a fresh GetServerInfo through the service's target and
// asserts NO bearer is injected: a failed probe must leave the target disarmed
// so a rejected candidate token is never armed for subsequent traffic.
func assertDisarmed(t *testing.T, svc *bridgeService, rec *authRecorder) {
	t.Helper()
	// Seed a sentinel so a re-probe that never reaches the server cannot pass as
	// "disarmed": the stub records the forwarded Authorization BEFORE its handler
	// returns, so a handler that ERRORS (the GetServerInfo-transport-error row)
	// still proves what the target injected — the RPC error itself is expected
	// and ignored; only the recorded header is the assertion.
	rec.set("<not-probed>")
	cc := compassv1connect.NewCompassServiceClient(svc.target.Client())
	ctx, cancel := context.WithTimeout(context.Background(), connectTestTimeout)
	defer cancel()
	_, _ = cc.GetServerInfo(ctx, connect.NewRequest(&compassv1.GetServerInfoRequest{}))
	if got := rec.get(); got != "" {
		t.Errorf("target left armed after failure: Authorization = %q, want empty", got)
	}
}

// clientAPIVersion must match the server's unexported apiVersion constant.
// keep in sync with go/server/service.go apiVersion.
func TestClientAPIVersionMatchesServer(t *testing.T) {
	if clientAPIVersion != "compass.v1" {
		t.Errorf("clientAPIVersion = %q, want %q (keep in sync with go/server/service.go apiVersion)", clientAPIVersion, "compass.v1")
	}
}

// TestConnectConcurrentIsSerialized fires many Connect calls at once, each with
// a DISTINCT token, against one shared target+service. The stub echoes the
// forwarded bearer's token back as the account id, so each caller's result must
// carry the account id derived from ITS OWN token. Without connectMu serializing
// the arm→probe→persist transaction, one probe carries another's bearer and the
// account ids cross-contaminate; with it, every result maps back to its caller.
// The final persisted token must be one a caller actually submitted (not torn).
func TestConnectConcurrentIsSerialized(t *testing.T) {
	// The stub reads the forwarded Authorization and reflects the bearer token
	// as the account id, so a mismatched (token, accountID) proves interleaving.
	echoWhoAmI := func(_ context.Context, req *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
		bearer := strings.TrimPrefix(req.Header().Get("Authorization"), "Bearer ")
		return connect.NewResponse(&compassv1.WhoAmIResponse{AccountId: "acct-for-" + bearer}), nil
	}
	srv, certPEM := connectTLSStub(t, connectStub{getServerInfo: okServerInfo, whoAmI: echoWhoAmI})
	svc, store := connectService(t, srv.URL, certPEM)

	const n = 16
	tokens := make([]string, n)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("token-%02d", i)
	}

	start := make(chan struct{}) // released together so the calls genuinely overlap
	var wg sync.WaitGroup
	results := make([]connectResult, n)
	for i := range tokens {
		wg.Go(func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), connectTestTimeout)
			defer cancel()
			results[i] = svc.Connect(ctx, connectRequest{Token: tokens[i]})
		})
	}
	close(start)
	wg.Wait()

	for i, res := range results {
		if !res.OK {
			t.Errorf("call %d: OK = false, want true (Kind=%q Message=%q)", i, res.Kind, res.Message)
			continue
		}
		if want := "acct-for-" + tokens[i]; res.AccountID != want {
			t.Errorf("call %d: AccountID = %q, want %q (probe carried another caller's token)", i, res.AccountID, want)
		}
	}

	// The persisted token must be one an actual caller submitted, never a torn
	// interleave — and the target must be left armed with that same token.
	stored, err := store.Read(srv.URL)
	if err != nil {
		t.Fatalf("post-connect Read: %v", err)
	}
	if !slices.Contains(tokens, stored) {
		t.Errorf("stored token = %q, want one of the submitted tokens", stored)
	}
}
