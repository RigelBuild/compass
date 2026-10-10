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
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/appconfig"
	"github.com/RigelBuild/compass/go/internal/bridge"
	"github.com/RigelBuild/compass/go/internal/tokenstore"
)

// A hang guard, not a latency assertion: connectMu serializes arm→probe→persist,
// so a queued caller's budget must cover every call ahead of it. A deadline near
// one call's cost makes queueing look unreachable, since classifyConnectErr
// folds DeadlineExceeded into bad-url.
const connectTestTimeout = 2 * time.Minute

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
	conn := &connection{mode: "client", serverURL: serverURL, target: target, pump: bridge.NewPump(target)}
	svc := newBridgeService(conn, nil, store)
	return svc, store
}

// connectCase is one row of the Connect classification table: a stub behaviour,
// a target-wiring toggle, and the expected outcome.
type connectCase struct {
	name         string
	whoAmI       func(*authRecorder) func(context.Context, *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error)
	serverInfo   func(context.Context, *connect.Request[compassv1.GetServerInfoRequest]) (*connect.Response[compassv1.GetServerInfoResponse], error)
	token        string
	badURL       bool   // point the target at an unreachable URL
	untrusted    bool   // do not pin the server's cert
	preStore     bool   // pre-store probeToken for the empty-token path
	wiring       string // "", or how the service is miswired: nilConn, emptyConn, nilStore, failingStore
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
		{name: "nil connection fails closed", serverInfo: okServerInfo, whoAmI: staticWhoAmI(unauthWhoAmI), token: probeToken, wiring: "nilConn", wantKind: connectKindOther},
		{name: "nil target fails closed", serverInfo: okServerInfo, whoAmI: staticWhoAmI(unauthWhoAmI), token: probeToken, wiring: "emptyConn", wantKind: connectKindOther},
		{name: "nil token store fails closed", serverInfo: okServerInfo, whoAmI: recordingWhoAmI("acct-123"), token: probeToken, wiring: "nilStore", wantKind: connectKindOther, wantDisarmed: true},
		{name: "token save failure disarms", serverInfo: okServerInfo, whoAmI: recordingWhoAmI("acct-123"), token: probeToken, wiring: "failingStore", wantKind: connectKindOther, wantDisarmed: true},
	}
}

// failingWriteStore reads through to a real store but refuses every Write.
type failingWriteStore struct{ tokenstore.Store }

func (failingWriteStore) Write(string, string) error { return errors.New("keyring locked") }

type memoryTokenStore struct {
	mu     sync.Mutex
	tokens map[string]string
}

func (s *memoryTokenStore) Read(serverURL string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token, ok := s.tokens[serverURL]
	if !ok {
		return "", tokenstore.ErrNotFound
	}
	return token, nil
}

func (s *memoryTokenStore) Write(serverURL, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		s.tokens = make(map[string]string)
	}
	s.tokens[serverURL] = token
	return nil
}

func (s *memoryTokenStore) Delete(serverURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, serverURL)
	return nil
}

// assertWiringOutcome checks the miswired rows: no store means no probe, and a
// failed save reports the save failure.
func assertWiringOutcome(t *testing.T, wiring string, res connectResult, rec *authRecorder) {
	t.Helper()
	if wiring == "failingStore" && res.Message != "Connected, but could not save the token" {
		t.Errorf("Message = %q, want the save-failure message", res.Message)
	}
	if wiring == "nilStore" && rec.get() != "" {
		t.Errorf("probe ran with no token store (Authorization %q)", rec.get())
	}
}

func TestConnectClassification(t *testing.T) {
	for _, tc := range connectClassificationCases() {
		t.Run(tc.name, func(t *testing.T) {
			runConnectClassificationCase(t, tc)
		})
	}
}

func runConnectClassificationCase(t *testing.T, tc connectCase) {
	t.Helper()
	rec := &authRecorder{}
	stub := connectStub{rec: rec, getServerInfo: tc.serverInfo, whoAmI: tc.whoAmI(rec)}
	srv, certPEM := connectTLSStub(t, stub)

	serverURL := srv.URL
	caPEM := certPEM
	if tc.untrusted {
		caPEM = nil
	}
	if tc.badURL {
		serverURL = "https://127.0.0.1:1"
	}

	svc, store := connectService(t, serverURL, caPEM)
	switch tc.wiring {
	case "nilConn":
		svc = newBridgeService(nil, nil, nil)
	case "emptyConn":
		svc = newBridgeService(&connection{}, nil, nil)
	case "nilStore":
		svc = newBridgeService(svc.conn.Load(), nil, nil)
	case "failingStore":
		svc = newBridgeService(svc.conn.Load(), nil, failingWriteStore{store})
	}
	if tc.preStore {
		if err := store.Write(serverURL, probeToken); err != nil {
			t.Fatalf("pre-store Write: %v", err)
		}
	}

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
	assertWiringOutcome(t, tc.wiring, res, rec)
	if res.OK != tc.wantOK {
		t.Errorf("OK = %v, want %v", res.OK, tc.wantOK)
	}
	if res.AccountID != tc.wantAccount {
		t.Errorf("AccountID = %q, want %q", res.AccountID, tc.wantAccount)
	}
	if strings.Contains(res.Message, probeToken) || strings.Contains(logBuf.String(), probeToken) {
		t.Error("token leaked into message or log output")
	}
	if tc.wantDisarmed {
		assertDisarmed(t, svc, rec)
	}
	if tc.wantOK && res.ServerURL != serverURL {
		t.Errorf("ServerURL = %q, want %q", res.ServerURL, serverURL)
	}
	if tc.wantOK {
		assertConnectSuccess(t, svc, store, serverURL, rec)
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
	cc := compassv1connect.NewCompassServiceClient(svc.conn.Load().target.Client())
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
	cc := compassv1connect.NewCompassServiceClient(svc.conn.Load().target.Client())
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

func TestCAPickReferencesAreRandomHexAndRetryable(t *testing.T) {
	picks := &caPicks{}
	caPEM := []byte("certificate bytes")
	first := picks.add(caPEM)
	second := picks.add(caPEM)
	if len(first) != 32 || first == second {
		t.Fatalf("refs = %q and %q, want distinct 128-bit hex references", first, second)
	}
	if _, err := hex.DecodeString(first); err != nil {
		t.Fatalf("ref %q is not hexadecimal: %v", first, err)
	}
	if decoded, err := hex.DecodeString(first); err != nil || len(decoded) != 16 {
		t.Fatalf("ref %q decoded to %d bytes, %v; want 16-byte random reference", first, len(decoded), err)
	}
	got, ok := picks.get(first)
	if !ok || string(got) != string(caPEM) {
		t.Fatalf("get(%q) = %q, %v; want original bytes and true", first, got, ok)
	}
	got[0] = 'X'
	got, ok = picks.get(first)
	if !ok || string(got) != string(caPEM) {
		t.Fatalf("get(%q) did not preserve the stored pick: %q, %v", first, got, ok)
	}
	picks.clear()
	if _, ok := picks.get(first); ok {
		t.Fatalf("get(%q) succeeded after clear", first)
	}
}

func TestFirstRunGateSerializesAndDecides(t *testing.T) {
	gate := &firstRunGate{}
	if err := gate.begin(); err != nil {
		t.Fatalf("first begin: %v", err)
	}
	if err := gate.begin(); err == nil || err.Error() != "Another window is setting up Compass." {
		t.Fatalf("busy begin error = %v, want busy message", err)
	}
	gate.end(false)
	if err := gate.begin(); err != nil {
		t.Fatalf("begin after end(false): %v", err)
	}
	emitter := &setupDecisionEmitter{}
	svc := newSetupBridgeService(emitter, tokenstore.New(t.TempDir()), &setupWiring{gate: gate, picks: &caPicks{}})
	emitter.svc = svc
	decide(gate, svc, false)
	if got := emitter.state; got != (shellStateResult{Mode: "reopen"}) {
		t.Fatalf("state observed during setup:decided = %+v, want reopen before event", got)
	}
	if emitter.count != 1 {
		t.Fatalf("setup:decided events = %d, want 1", emitter.count)
	}
	if err := gate.begin(); err == nil || err.Error() != "Compass is already set up. Quit and reopen it to change this." {
		t.Fatalf("decided begin error = %v, want final message", err)
	}
}

type setupDecisionEmitter struct {
	svc    *bridgeService
	state  shellStateResult
	count  int
	frames *fakeEmitter
}

func (e *setupDecisionEmitter) Emit(name string, data ...any) bool {
	if name != "setup:decided" {
		if e.frames != nil {
			return e.frames.Emit(name, data...)
		}
		return false
	}
	e.count++
	e.state = e.svc.ShellState()
	return true
}

func setupTLSStub(t *testing.T, rpcReached chan<- struct{}) (*httptest.Server, []byte) {
	t.Helper()
	stub := connectStub{
		getServerInfo: okServerInfo,
		whoAmI: func(_ context.Context, req *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
			if req.Header().Get("Authorization") != "Bearer "+probeToken {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("stub: no caller"))
			}
			return connect.NewResponse(&compassv1.WhoAmIResponse{AccountId: "setup-account"}), nil
		},
	}
	_, connectHandler := compassv1connect.NewCompassServiceHandler(stub)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc-web") {
			if rpcReached != nil {
				select {
				case rpcReached <- struct{}{}:
				default:
				}
			}
			w.Header().Set("Content-Type", "application/grpc-web+proto")
			w.WriteHeader(http.StatusOK)
			return
		}
		connectHandler.ServeHTTP(w, r)
	})
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	leaf := srv.Certificate()
	return srv, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})
}

func newFirstRunService(t *testing.T, store tokenstore.Store, events eventEmitter, save func(string, appconfig.Config, []byte) (appconfig.Config, error)) (*bridgeService, *firstRunGate, *caPicks, string) {
	t.Helper()
	configHome := t.TempDir()
	configPath, err := appconfig.ConfigPath(configHome, "")
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	gate := &firstRunGate{}
	picks := &caPicks{}
	setup := &setupWiring{configPath: configPath, gate: gate, picks: picks, saveClient: save}
	svc := newSetupBridgeService(events, store, setup)
	return svc, gate, picks, configHome
}

// setup:decided must expose the installed client state to subscribers.
func TestConnectServerChoiceSuccessAndRPC(t *testing.T) {
	rpcReached := make(chan struct{}, 1)
	srv, certPEM := setupTLSStub(t, rpcReached)
	store := tokenstore.New(t.TempDir())
	frames := newFakeEmitter()
	emitter := &setupDecisionEmitter{frames: frames}
	svc, _, picks, configHome := newFirstRunService(t, store, emitter, nil)
	emitter.svc = svc
	ref := picks.add(certPEM)
	ctx, cancel := context.WithTimeout(context.Background(), connectTestTimeout)
	defer cancel()
	res := svc.Connect(ctx, connectRequest{Token: probeToken, Server: &serverChoice{URL: " " + srv.URL + "/ ", CARef: ref}})
	if !res.OK || res.Kind != "" || res.ServerURL != srv.URL {
		t.Fatalf("Connect = %+v, want success with normalized server URL %q", res, srv.URL)
	}
	cfg, err := appconfig.Load(configHome, "", "")
	if err != nil {
		t.Fatalf("Load saved config: %v", err)
	}
	if cfg.Mode != appconfig.ModeClient || cfg.ServerURL != srv.URL || cfg.CACert == "" {
		t.Fatalf("saved config = %+v, want client mode, URL, and CA copy", cfg)
	}
	if got, err := os.ReadFile(cfg.CACert); err != nil || string(got) != string(certPEM) {
		t.Fatalf("saved CA = %q, %v; want picked PEM", got, err)
	}
	if got, err := store.Read(srv.URL); err != nil || got != probeToken {
		t.Fatalf("stored token = %q, %v; want submitted token", got, err)
	}
	if emitter.count != 1 {
		t.Fatalf("setup:decided events = %d, want 1", emitter.count)
	}
	wantState := shellStateResult{Mode: "client", ServerURL: srv.URL}
	if emitter.state != wantState {
		t.Fatalf("ShellState() at setup:decided emit = %+v, want %+v", emitter.state, wantState)
	}
	if got := svc.ShellState(); got != wantState {
		t.Fatalf("ShellState() = %+v, want configured client", got)
	}
	svc.CompassRPC(context.Background(), rpcRequest{
		RequestID: "setup-rpc",
		Path:      compassv1connect.CompassServiceWhoAmIProcedure,
		Headers:   []headerPair{{Name: "Content-Type", Value: "application/grpc-web+proto"}},
	})
	select {
	case <-rpcReached:
	case <-time.After(testTimeout):
		t.Fatal("CompassRPC did not reach setup server")
	}
	if got := recv(t, frames); got.name != "compass_rpc:setup-rpc" || got.frame.Kind != frameKindHead {
		t.Fatalf("CompassRPC first frame = %+v, want head", got.frame)
	}
	if got := recv(t, frames); got.name != "compass_rpc:setup-rpc" || got.frame.Kind != frameKindEnd {
		t.Fatalf("CompassRPC terminal frame = %+v, want end", got.frame)
	}
}

func TestConnectServerChoiceUsesStoredToken(t *testing.T) {
	srv, certPEM := connectTLSStub(t, connectStub{getServerInfo: okServerInfo, whoAmI: func(_ context.Context, _ *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
		return connect.NewResponse(&compassv1.WhoAmIResponse{AccountId: "stored-account"}), nil
	}})
	store := tokenstore.New(t.TempDir())
	if err := store.Write(srv.URL, probeToken); err != nil {
		t.Fatalf("pre-store token: %v", err)
	}
	svc, _, picks, _ := newFirstRunService(t, store, newFakeEmitter(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), connectTestTimeout)
	defer cancel()
	res := svc.Connect(ctx, connectRequest{
		Server: &serverChoice{URL: srv.URL, CARef: picks.add(certPEM)},
	})
	if !res.OK || res.ServerURL != srv.URL || res.AccountID != "stored-account" {
		t.Fatalf("Connect with stored token = %+v, want successful normalized result", res)
	}
}

func TestConnectServerChoiceValidationFailures(t *testing.T) {
	srv, certPEM := setupTLSStub(t, nil)
	badPEM := []byte("not a certificate")
	for _, tc := range []struct {
		name     string
		choice   serverChoice
		token    string
		wantKind string
		wantMsg  string
	}{
		{name: "invalid url", choice: serverChoice{URL: "http://example.test"}, token: probeToken, wantKind: connectKindInvalidURL, wantMsg: "The server URL must use https."},
		{name: "unknown ca reference", choice: serverChoice{URL: srv.URL, CARef: "missing"}, token: probeToken, wantKind: connectKindInvalidCA, wantMsg: "Choose the certificate again."},
		{name: "non pem ca", choice: serverChoice{URL: srv.URL, CARef: "bad-pem"}, token: probeToken, wantKind: connectKindInvalidCA, wantMsg: "The file is not a PEM certificate."},
		{name: "empty ca pick", choice: serverChoice{URL: srv.URL, CARef: "empty"}, token: probeToken, wantKind: connectKindInvalidCA, wantMsg: "The file is not a PEM certificate."},
		{name: "wrong token", choice: serverChoice{URL: srv.URL, CARef: "good"}, token: "wrong", wantKind: connectKindBadToken},
		{name: "missing ca", choice: serverChoice{URL: srv.URL}, token: probeToken, wantKind: connectKindBadCert},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := tokenstore.New(t.TempDir())
			svc, gate, picks, configHome := newFirstRunService(t, store, newFakeEmitter(), nil)
			switch tc.choice.CARef {
			case "bad-pem":
				tc.choice.CARef = picks.add(badPEM)
			case "good":
				tc.choice.CARef = picks.add(certPEM)
			case "empty":
				tc.choice.CARef = picks.add(nil)
			}
			ctx, cancel := context.WithTimeout(context.Background(), connectTestTimeout)
			defer cancel()
			res := svc.Connect(ctx, connectRequest{Token: tc.token, Server: &tc.choice})
			if res.Kind != tc.wantKind || (tc.wantMsg != "" && res.Message != tc.wantMsg) {
				t.Errorf("Connect = %+v, want kind %q and message %q", res, tc.wantKind, tc.wantMsg)
			}
			if svc.conn.Load() != nil {
				t.Errorf("connection installed after %s failure", tc.name)
			}
			if _, err := os.Stat(filepath.Join(configHome, "compass", "app.toml")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("failed attempt app.toml stat error = %v, want absent", err)
			}
			if err := gate.begin(); err != nil {
				t.Errorf("gate remains busy after failed attempt: %v", err)
			} else {
				gate.end(false)
			}
		})
	}
}

func TestConnectServerChoiceRetriesSameCAPick(t *testing.T) {
	whoAmI := func(_ context.Context, req *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
		if req.Header().Get("Authorization") != "Bearer "+probeToken {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("stub: no caller"))
		}
		return connect.NewResponse(&compassv1.WhoAmIResponse{AccountId: "retry-account"}), nil
	}
	srv, certPEM := connectTLSStub(t, connectStub{getServerInfo: okServerInfo, whoAmI: whoAmI})
	store := tokenstore.New(t.TempDir())
	svc, _, picks, _ := newFirstRunService(t, store, newFakeEmitter(), nil)
	ref := picks.add(certPEM)
	ctx, cancel := context.WithTimeout(context.Background(), connectTestTimeout)
	defer cancel()
	first := svc.Connect(ctx, connectRequest{Token: "wrong", Server: &serverChoice{URL: srv.URL, CARef: ref}})
	if first.Kind != connectKindBadToken {
		t.Fatalf("first Connect = %+v, want bad token", first)
	}
	if _, ok := picks.get(ref); !ok {
		t.Fatal("failed probe discarded CA pick needed for retry")
	}
	second := svc.Connect(ctx, connectRequest{Token: probeToken, Server: &serverChoice{URL: srv.URL, CARef: ref}})
	if !second.OK {
		t.Fatalf("retry Connect = %+v, want success", second)
	}
}

func TestConnectServerChoiceAfterDecisionAndPlainConnect(t *testing.T) {
	srv, certPEM := connectTLSStub(t, connectStub{getServerInfo: okServerInfo, whoAmI: func(_ context.Context, _ *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
		return connect.NewResponse(&compassv1.WhoAmIResponse{AccountId: "setup-account"}), nil
	}})
	store := tokenstore.New(t.TempDir())
	emitter := newFakeEmitter()
	svc, _, picks, configHome := newFirstRunService(t, store, emitter, nil)
	ref := picks.add(certPEM)
	ctx, cancel := context.WithTimeout(context.Background(), connectTestTimeout)
	defer cancel()
	first := svc.Connect(ctx, connectRequest{Token: probeToken, Server: &serverChoice{URL: srv.URL, CARef: ref}})
	if !first.OK {
		t.Fatalf("initial setup Connect = %+v", first)
	}
	configPath, err := appconfig.ConfigPath(configHome, "")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	other, otherPEM := connectTLSStub(t, connectStub{getServerInfo: okServerInfo, whoAmI: func(_ context.Context, _ *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
		return connect.NewResponse(&compassv1.WhoAmIResponse{AccountId: "other-account"}), nil
	}})
	res := svc.Connect(ctx, connectRequest{Token: probeToken, Server: &serverChoice{URL: other.URL, CARef: picks.add(otherPEM)}})
	if res.Kind != connectKindOther || res.Message != "Compass is already set up. Quit and reopen it to change this." {
		t.Fatalf("second server choice = %+v, want already-set-up result", res)
	}
	after, err := os.ReadFile(configPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("second choice changed app.toml: err=%v", err)
	}
	plain := svc.Connect(ctx, connectRequest{Token: probeToken})
	if !plain.OK || plain.ServerURL != srv.URL {
		t.Fatalf("plain Connect = %+v, want configured server success", plain)
	}
}

func TestConnectServerChoiceRejectedOnConfiguredService(t *testing.T) {
	srv, certPEM := connectTLSStub(t, connectStub{getServerInfo: okServerInfo, whoAmI: func(_ context.Context, _ *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
		return connect.NewResponse(&compassv1.WhoAmIResponse{AccountId: "account"}), nil
	}})
	service, _ := connectService(t, srv.URL, certPEM)
	ctx, cancel := context.WithTimeout(context.Background(), connectTestTimeout)
	defer cancel()
	res := service.Connect(ctx, connectRequest{Token: probeToken, Server: &serverChoice{URL: srv.URL}})
	if res.Kind != connectKindOther || res.Message != "The server is set in app.toml." || service.conn.Load() == nil {
		t.Fatalf("server choice on configured service = %+v, want setup-only refusal", res)
	}
}

func TestConnectServerChoiceSaveErrorCanRetry(t *testing.T) {
	srv, certPEM := connectTLSStub(t, connectStub{getServerInfo: okServerInfo, whoAmI: func(_ context.Context, _ *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
		return connect.NewResponse(&compassv1.WhoAmIResponse{AccountId: "account"}), nil
	}})
	store := &memoryTokenStore{}
	fail := true
	save := func(path string, cfg appconfig.Config, caPEM []byte) (appconfig.Config, error) {
		if fail {
			return appconfig.Config{}, errors.New("disk full")
		}
		return appconfig.SaveClient(path, cfg, caPEM)
	}
	svc, gate, picks, configHome := newFirstRunService(t, store, newFakeEmitter(), save)
	ref := picks.add(certPEM)
	ctx, cancel := context.WithTimeout(context.Background(), connectTestTimeout)
	defer cancel()
	res := svc.Connect(ctx, connectRequest{Token: probeToken, Server: &serverChoice{URL: srv.URL, CARef: ref}})
	if res.Kind != connectKindOther || res.Message != "Connected, but the settings could not be saved: disk full" {
		t.Fatalf("save failure = %+v, want exact settings-save message", res)
	}
	if svc.conn.Load() != nil {
		t.Fatal("connection installed after SaveClient failure")
	}
	if _, err := store.Read(srv.URL); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("token read after save failure = %v, want ErrNotFound", err)
	}
	if err := gate.begin(); err != nil {
		t.Fatalf("gate not open after SaveClient failure: %v", err)
	}
	gate.end(false)
	fail = false
	if retry := svc.Connect(ctx, connectRequest{Token: probeToken, Server: &serverChoice{URL: srv.URL, CARef: ref}}); !retry.OK {
		t.Fatalf("valid retry = %+v, want success", retry)
	}
	if got, err := appconfig.Load(configHome, "", ""); err != nil || got.ServerURL != srv.URL {
		t.Fatalf("retry config = %+v, %v", got, err)
	}
}

func TestConnectServerChoiceTokenStoreFailureDecidesReopen(t *testing.T) {
	rec := &authRecorder{}
	srv, certPEM := connectTLSStub(t, connectStub{rec: rec, getServerInfo: okServerInfo, whoAmI: func(_ context.Context, _ *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
		return connect.NewResponse(&compassv1.WhoAmIResponse{AccountId: "account"}), nil
	}})
	store := failingWriteStore{Store: tokenstore.New(t.TempDir())}
	frames := newFakeEmitter()
	emitter := &setupDecisionEmitter{frames: frames}
	svc, _, picks, configHome := newFirstRunService(t, store, emitter, nil)
	emitter.svc = svc
	var candidate *bridge.Target
	svc.setup.newTarget = func(serverURL string, caPEM []byte) (*bridge.Target, error) {
		var err error
		candidate, err = bridge.NewTLSTarget(serverURL, caPEM)
		return candidate, err
	}
	ref := picks.add(certPEM)
	ctx, cancel := context.WithTimeout(context.Background(), connectTestTimeout)
	defer cancel()
	res := svc.Connect(ctx, connectRequest{Token: probeToken, Server: &serverChoice{URL: srv.URL, CARef: ref}})
	if res.Kind != connectKindOther || res.Message != "Saved, but could not store the token. Quit and reopen Compass, then enter it again." {
		t.Fatalf("token-store failure = %+v, want fixed recovery message", res)
	}
	if svc.conn.Load() != nil {
		t.Fatal("connection installed after token store failure")
	}
	if candidate == nil {
		t.Fatal("candidate target was not created")
	}
	client, serverURL := candidate.Client()
	cc := compassv1connect.NewCompassServiceClient(client, serverURL)
	if _, err := cc.WhoAmI(ctx, connect.NewRequest(&compassv1.WhoAmIRequest{})); err != nil {
		t.Fatalf("WhoAmI after token-store failure: %v", err)
	}
	if got := rec.get(); got != "" {
		t.Errorf("candidate retained bearer after token-store failure: %q", got)
	}
	if got := svc.ShellState(); got != (shellStateResult{Mode: "reopen"}) {
		t.Fatalf("ShellState() = %+v, want reopen", got)
	}
	if _, ok := picks.get(ref); ok {
		t.Fatal("CA picks remain after successful config save")
	}
	if emitter.count != 1 || emitter.state.Mode != "reopen" {
		t.Fatalf("decision event count/state = %d/%+v, want one reopen event", emitter.count, emitter.state)
	}
	if _, err := appconfig.Load(configHome, "", ""); err != nil {
		t.Fatalf("settings should remain saved after token-store failure: %v", err)
	}
}

func TestConnectServerChoiceConfigCreatedAfterChooser(t *testing.T) {
	srv, certPEM := connectTLSStub(t, connectStub{getServerInfo: okServerInfo, whoAmI: func(_ context.Context, _ *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
		return connect.NewResponse(&compassv1.WhoAmIResponse{AccountId: "account"}), nil
	}})
	store := tokenstore.New(t.TempDir())
	emitter := &setupDecisionEmitter{}
	svc, gate, picks, configHome := newFirstRunService(t, store, emitter, nil)
	emitter.svc = svc
	ref := picks.add(certPEM)
	configPath, err := appconfig.ConfigPath(configHome, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appconfig.SaveClient(configPath, appconfig.Config{Mode: appconfig.ModeClient, ServerURL: srv.URL}, certPEM); err != nil {
		t.Fatalf("racing SaveClient: %v", err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(srv.URL, "preexisting-token"); err != nil {
		t.Fatal(err)
	}
	tokenBefore, err := store.Read(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectTestTimeout)
	defer cancel()
	res := svc.Connect(ctx, connectRequest{Token: probeToken, Server: &serverChoice{URL: srv.URL, CARef: ref}})
	if res.Kind != connectKindOther || res.Message != "Compass is already set up. Quit and reopen it to change this." {
		t.Fatalf("racing config Connect = %+v, want already-set-up message", res)
	}
	after, err := os.ReadFile(configPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("racing app.toml changed: %v", err)
	}
	tokenAfter, err := store.Read(srv.URL)
	if err != nil || tokenAfter != tokenBefore {
		t.Fatalf("racing token changed to %q (read error %v), want unchanged", tokenAfter, err)
	}
	if state := svc.ShellState(); state != (shellStateResult{Mode: "reopen"}) || emitter.count != 1 || emitter.state != state {
		t.Fatalf("state/event = %+v, %d, %+v; want one reopen decision", state, emitter.count, emitter.state)
	}
	if err := gate.begin(); err == nil {
		t.Fatal("gate reopened after ErrConfigExists")
	}
}

func TestConnectServerChoiceConcurrentCompassRPC(t *testing.T) {
	srv, certPEM := setupTLSStub(t, nil)
	// A host keyring probe can stall past the collector deadline under load.
	store := &memoryTokenStore{}
	frames := &fakeEmitter{ch: make(chan emitted, 512)}
	events := &setupDecisionEmitter{frames: frames}
	saving := make(chan struct{})
	releaseSave := make(chan struct{})
	save := func(path string, cfg appconfig.Config, caPEM []byte) (appconfig.Config, error) {
		close(saving)
		<-releaseSave
		return appconfig.SaveClient(path, cfg, caPEM)
	}
	svc, _, picks, _ := newFirstRunService(t, store, events, save)
	events.svc = svc
	ref := picks.add(certPEM)
	connectDone := make(chan connectResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), connectTestTimeout)
		defer cancel()
		connectDone <- svc.Connect(ctx, connectRequest{Token: probeToken, Server: &serverChoice{URL: srv.URL, CARef: ref}})
	}()
	select {
	case <-saving:
	case <-time.After(testTimeout):
		t.Fatal("Connect did not reach save seam")
	}

	callCompassRPC(svc, "setup-race-before")
	assertDisconnectedRPC(t, frames, "setup-race-before")

	issue, stopWorkers, workersDone := startConcurrentRPCWorkers(svc, 4)
	result := collectConcurrentRPCResults(t, frames, issue, releaseSave, connectDone, stopWorkers, workersDone)
	if !result.OK || result.ServerURL != srv.URL {
		t.Fatalf("concurrent setup Connect = %+v, want success", result)
	}

	callCompassRPC(svc, "setup-race-after")
	assertSuccessfulRPC(t, frames, "setup-race-after")
	if got := svc.ShellState(); got != (shellStateResult{Mode: "client", ServerURL: srv.URL}) {
		t.Fatalf("post-race ShellState() = %+v, want configured client", got)
	}
}

type concurrentRPCTask struct {
	requestID string
	ready     chan struct{}
	done      chan struct{}
}

func startConcurrentRPCWorkers(svc *bridgeService, count int) (<-chan concurrentRPCTask, chan<- struct{}, <-chan struct{}) {
	issue := make(chan concurrentRPCTask)
	stop := make(chan struct{})
	started := make(chan struct{}, count)
	var workers sync.WaitGroup
	for worker := range count {
		workers.Go(func() {
			started <- struct{}{}
			for sequence := 0; ; sequence++ {
				select {
				case <-stop:
					return
				default:
				}
				task := concurrentRPCTask{
					requestID: fmt.Sprintf("setup-race-%d-%d", worker, sequence),
					ready:     make(chan struct{}),
					done:      make(chan struct{}),
				}
				select {
				case issue <- task:
				case <-stop:
					return
				}
				// The collector closes ready on every issued task. Once it does, the
				// RPC must run even if stop closed too, or its frames never arrive.
				<-task.ready
				callCompassRPC(svc, task.requestID)
				select {
				case <-task.done:
				case <-stop:
					return
				}
			}
		})
	}
	for range count {
		<-started
	}
	workersDone := make(chan struct{})
	go func() {
		workers.Wait()
		close(workersDone)
	}()
	return issue, stop, workersDone
}

// collectConcurrentRPCResults releases the save only after a worker RPC has
// failed as disconnected, and stops the workers only after one has been served
// by the installed connection, so worker RPCs span the install.
func collectConcurrentRPCResults(t *testing.T, frames *fakeEmitter, issue <-chan concurrentRPCTask, releaseSave chan<- struct{}, connectDone <-chan connectResult, stop chan<- struct{}, workersDone <-chan struct{}) connectResult {
	t.Helper()
	states := make(map[string]rpcFrameState)
	pending := make(map[string]concurrentRPCTask)
	connectC := connectDone
	issueC := issue
	var result connectResult
	released, connected, stopped := false, false, false
	deadline := time.After(testTimeout)
	stopIfDone := func() {
		if connectC == nil && (connected || !result.OK) && !stopped {
			stopped = true
			issueC = nil
			close(stop)
		}
	}
	defer func() {
		if !stopped {
			close(stop)
		}
	}()
	for !stopped || len(pending) > 0 {
		select {
		case task := <-issueC:
			if _, exists := states[task.requestID]; exists {
				t.Fatalf("duplicate RPC request ID %q", task.requestID)
			}
			pending[task.requestID] = task
			states[task.requestID] = rpcInitial
			close(task.ready)
		case event := <-frames.ch:
			requestID := strings.TrimPrefix(event.name, "compass_rpc:")
			state, exists := states[requestID]
			if !exists {
				t.Fatalf("unexpected RPC event %q", event.name)
			}
			states[requestID] = advanceRPCFrame(t, requestID, state, event.frame)
			if states[requestID] != rpcTerminated {
				continue
			}
			close(pending[requestID].done)
			delete(pending, requestID)
			if event.frame.Kind == frameKindEnd {
				connected = true
				stopIfDone()
			} else if !released {
				released = true
				close(releaseSave)
			}
		case result = <-connectC:
			connectC = nil
			stopIfDone()
		case <-deadline:
			t.Fatal("timed out waiting for concurrent RPC outcomes")
		}
	}
	<-workersDone
	for requestID, state := range states {
		if state != rpcTerminated {
			t.Errorf("RPC %q did not reach a terminal outcome", requestID)
		}
	}
	return result
}

type rpcFrameState int

const (
	rpcInitial rpcFrameState = iota
	rpcHeadSeen
	rpcTerminated
)

// advanceRPCFrame accepts one no-connection error, or a head then an end.
func advanceRPCFrame(t *testing.T, requestID string, state rpcFrameState, frame responseFrame) rpcFrameState {
	t.Helper()
	switch {
	case state == rpcTerminated:
		t.Fatalf("RPC %q emitted a frame after termination", requestID)
	case frame.Kind == frameKindError && state == rpcInitial && frame.Message == "Not connected to a server":
		return rpcTerminated
	case frame.Kind == frameKindHead && state == rpcInitial:
		return rpcHeadSeen
	case frame.Kind == frameKindEnd && state == rpcHeadSeen:
		return rpcTerminated
	}
	t.Fatalf("RPC %q frame %+v in state %d, want one no-connection error or head then end", requestID, frame, state)
	return state
}

func callCompassRPC(svc *bridgeService, requestID string) {
	svc.CompassRPC(context.Background(), rpcRequest{
		RequestID: requestID,
		Path:      compassv1connect.CompassServiceWhoAmIProcedure,
		Headers:   []headerPair{{Name: "Content-Type", Value: "application/grpc-web+proto"}},
	})
}

func assertDisconnectedRPC(t *testing.T, frames *fakeEmitter, requestID string) {
	t.Helper()
	event := recv(t, frames)
	if event.name != "compass_rpc:"+requestID || event.frame.Kind != frameKindError || event.frame.Message != "Not connected to a server" {
		t.Fatalf("RPC event = %+v, want disconnected error for %q", event, requestID)
	}
}

func assertSuccessfulRPC(t *testing.T, frames *fakeEmitter, requestID string) {
	t.Helper()
	if event := recv(t, frames); event.name != "compass_rpc:"+requestID || event.frame.Kind != frameKindHead {
		t.Fatalf("RPC first frame = %+v, want head for %q", event, requestID)
	}
	if event := recv(t, frames); event.name != "compass_rpc:"+requestID || event.frame.Kind != frameKindEnd {
		t.Fatalf("RPC terminal frame = %+v, want end for %q", event, requestID)
	}
}
