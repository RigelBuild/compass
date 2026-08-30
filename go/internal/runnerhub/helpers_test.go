//go:build unix

package runnerhub

// Shared scaffolding for the runnerhub seam tests: hand-written fakes for the
// three write-through sinks, a fake TokenResolver that models the real
// auth.ResolveToken kind-gate contract, protobuf frame builders, and the in-
// process h2c transport (the same cleartext-HTTP/2 door serve.go ships) so the
// RunnerService handler is exercised through a real connect-go client rather
// than called directly. White-box (package runnerhub) so tests can reach the
// unexported router/dispatch/auth internals while still driving the wire.

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
)

// discardLogger builds a slog.Logger that drops output, so the hub's warn-level
// gap/unknown diagnostics do not spam the test log.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// waitRouterAttached blocks until the enrolled Runner's command router has a
// live send bound (the Sessions handler ran router.attach), or fails at the
// deadline. It event-gates on the observed attach state — a monotonic signal
// set exactly once when the handler binds the stream — using runtime.Gosched to
// yield to the handler goroutine between probes, never a wall-clock sleep. This
// is the same readiness-gate shape as the server suite's waitListening.
func waitRouterAttached(t *testing.T, hub *Hub) {
	t.Helper()
	deadline := timeAfter()
	for {
		select {
		case <-deadline:
			t.Fatal("runner command router never attached a live Sessions send")
		default:
		}
		if router, _, err := hub.routerFor("gate"); err == nil {
			router.mu.Lock()
			attached := router.sender != nil
			router.mu.Unlock()
			if attached {
				return
			}
		}
		runtime.Gosched()
	}
}

// testTimeout bounds every blocking wait so a wedged handler fails fast instead
// of hanging the suite. It is a deadline safety net, never a synchronization
// device: tests event-gate on channel sends and observed calls, not elapsed
// time.
const testTimeout = 15 * time.Second

func timeAfter() <-chan time.Time { return time.After(testTimeout) }

// waitRecorded blocks until the recording send has observed at least n frames,
// or fails at the deadline. Since attach now drains an outbound queue on a
// sender goroutine, a frame is queued-not-pushed: a test asserting a signal
// reached the wire must gate on the recorder observing it rather than assuming
// the enqueue call pushed synchronously. Channel-gated on the deadline, never a
// wall-clock sleep.
func waitRecorded(t *testing.T, rec *recordingSend, n int) {
	t.Helper()
	deadline := timeAfter()
	for {
		if rec.count() >= n {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("recording send saw %d frames, want at least %d", rec.count(), n)
		default:
		}
	}
}

// fakeLifecycleSink records the AgentSessionStatus values extracted onto
// SubscribeEvents.
type fakeLifecycleSink struct {
	mu       sync.Mutex
	statuses []*compassv1.AgentSessionStatus
}

func (f *fakeLifecycleSink) PublishSessionStatus(status *compassv1.AgentSessionStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, status)
}

func (f *fakeLifecycleSink) snapshot() []*compassv1.AgentSessionStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*compassv1.AgentSessionStatus(nil), f.statuses...)
}

// tailCall is one recorded RelaySessionFrame.
type tailCall struct {
	sessionID string
	frame     *compassv1internal.SessionFrame
}

// fakeTailSink records the opaque session frames relayed to the observation-pane
// tail.
type fakeTailSink struct {
	mu    sync.Mutex
	calls []tailCall
}

func (f *fakeTailSink) RelaySessionFrame(sessionID string, frame *compassv1internal.SessionFrame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, tailCall{sessionID: sessionID, frame: frame})
}

func (f *fakeTailSink) snapshot() []tailCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tailCall(nil), f.calls...)
}

// newHub builds a hub over two fresh fake sinks and returns them, so a test
// asserts on exactly the sink it targets.
func newHub() (*Hub, *fakeLifecycleSink, *fakeTailSink) {
	life := &fakeLifecycleSink{}
	tail := &fakeTailSink{}
	return NewHub(life, tail, nil, discardLogger()), life, tail
}

// newHubOnly builds a hub over two fresh fake sinks and returns just the hub,
// for a test that drives Deliver/enroll/routing and asserts through the wire or
// the hub's own accessors rather than reaching into a specific sink.
func newHubOnly() *Hub {
	return NewHub(&fakeLifecycleSink{}, &fakeTailSink{}, nil, discardLogger())
}

// testAgentAccount is the account every conversation test binds its session to.
// The tests assert WHICH account the hub attributed a frame to, so the value
// has to be nameable at the assertion site; it is a constant rather than a
// bindSession parameter because no test needs a second account — a cross
// -account case is built by binding this one and asserting a refusal, not by
// binding a different one.
const testAgentAccount store.AccountID = "acct-agent"

// bindSession binds sessionID to testAgentAccount through the real
// Provision->Start promotion path, the same two-step the command handlers
// drive. Deliver's conversation arms resolve against this binding and fail
// closed without it, so any conversation test that expects a write-through must
// bind first.
func bindSession(hub *Hub, sessionID string) {
	container := "container-for-" + sessionID
	hub.bindContainer(container, testAgentAccount)
	hub.promoteSession(container, sessionID)
}

// commsCall records one CommsCaller invocation: the account the hub resolved
// the session to, and exactly one of the post/list request it dispatched. It is
// the fake's proof of WHICH account attribution the hub applied — the security
// invariant the RelayCommsCall tests defend.
type commsCall struct {
	account   store.AccountID
	post      *compassv1.PostMessageRequest
	list      *compassv1.ListMessagesRequest
	roster    *compassv1.GetRosterRequest
	setStatus string
	pin       *compassv1.UpdatePinnedBoardRequest
}

// fakeCommsCaller is a hand-written CommsCaller: it records every call (account
// + request) so a test asserts the hub attributed to the bound account and
// forwarded the exact request, and returns a configurable canned response or
// error per method so a test drives both the success and the in-band tool-error
// path without a real store. Concurrency-safe for parity with the real caller,
// though the hub calls it inline.
type fakeCommsCaller struct {
	mu    sync.Mutex
	calls []commsCall

	postResp *compassv1.PostMessageResponse
	postErr  error
	listResp *compassv1.ListMessagesResponse
	listErr  error

	rosterResp *compassv1.GetRosterResponse
	rosterErr  error
	// setStatusTruncateTo, when >0, truncates the recorded activity to that many
	// runes, modeling the real server-side cap so a relay test asserts the
	// PUBLISHED value equals the truncated one. setStatusReturned captures what
	// SetStatusAsAccount returned (the value the relay arm publishes).
	setStatusErr        error
	setStatusTruncateTo int
	setStatusReturned   string
	pinResp             *compassv1.UpdatePinnedBoardResponse
	pinErr              error
}

func (f *fakeCommsCaller) PostAsAccount(_ context.Context, account store.AccountID, req *compassv1.PostMessageRequest) (*compassv1.PostMessageResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, commsCall{account: account, post: req})
	if f.postErr != nil {
		return nil, f.postErr
	}
	return f.postResp, nil
}

// PostAsAccountByName is the agent-tool entry. The fake records and responds
// identically to PostAsAccount (the name→id resolve is the real Comms', proven
// in the comms pgtests), so a relay test drives the same attribute→forward path
// and asserts on the recorded post + canned response/error.
func (f *fakeCommsCaller) PostAsAccountByName(ctx context.Context, account store.AccountID, req *compassv1.PostMessageRequest) (*compassv1.PostMessageResponse, error) {
	return f.PostAsAccount(ctx, account, req)
}

func (f *fakeCommsCaller) ListAsAccount(_ context.Context, account store.AccountID, req *compassv1.ListMessagesRequest) (*compassv1.ListMessagesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, commsCall{account: account, list: req})
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResp, nil
}

// ListAsAccountByName is the agent-tool list entry; records and responds
// identically to ListAsAccount (see PostAsAccountByName).
func (f *fakeCommsCaller) ListAsAccountByName(ctx context.Context, account store.AccountID, req *compassv1.ListMessagesRequest) (*compassv1.ListMessagesResponse, error) {
	return f.ListAsAccount(ctx, account, req)
}

func (f *fakeCommsCaller) RosterAsAccount(_ context.Context, account store.AccountID, req *compassv1.GetRosterRequest) (*compassv1.GetRosterResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, commsCall{account: account, roster: req})
	if f.rosterErr != nil {
		return nil, f.rosterErr
	}
	return f.rosterResp, nil
}

func (f *fakeCommsCaller) SetStatusAsAccount(_ context.Context, account store.AccountID, activity string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, commsCall{account: account, setStatus: activity})
	if f.setStatusErr != nil {
		return "", f.setStatusErr
	}
	// Mirror the real truncation so a relay test asserts the published value.
	truncated := activity
	if f.setStatusTruncateTo > 0 {
		r := []rune(activity)
		if len(r) > f.setStatusTruncateTo {
			truncated = string(r[:f.setStatusTruncateTo])
		}
	}
	f.setStatusReturned = truncated
	return truncated, nil
}

func (f *fakeCommsCaller) UpdatePinnedBoardAsAccount(_ context.Context, account store.AccountID, req *compassv1.UpdatePinnedBoardRequest) (*compassv1.UpdatePinnedBoardResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, commsCall{account: account, pin: req})
	if f.pinErr != nil {
		return nil, f.pinErr
	}
	return f.pinResp, nil
}

func (f *fakeCommsCaller) snapshot() []commsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]commsCall(nil), f.calls...)
}

// newHubWithComms builds a hub whose CommsCaller is the returned fake, so a
// RelayCommsCall test drives the resolve->attribute->execute path and asserts on
// the account the fake was called with. Like newHubOnly otherwise (the two
// write-through sinks are unused stubs).
func newHubWithComms() (*Hub, *fakeCommsCaller) {
	comms := &fakeCommsCaller{}
	return NewHub(&fakeLifecycleSink{}, &fakeTailSink{}, comms, discardLogger()), comms
}

// lifecycleCall records one LifecycleCaller invocation: the account the hub
// attributed it to plus the request forwarded, one of spawn/despawn set. A test
// asserts the hub delegated under the RESOLVED caller account (never the
// Runner's, never admin) and dispatched the right variant.
type lifecycleCall struct {
	account store.AccountID
	spawn   *compassv1internal.SpawnPeerRequest
	despawn *compassv1internal.DespawnPeerRequest
}

// fakeLifecycleCaller is a hand-written LifecycleCaller mirroring
// fakeCommsCaller: it records every call (account + request) so a test asserts
// the hub attributed to the bound account and forwarded the exact request, and
// returns a configurable canned response or error per method so a test drives
// both the success and the in-band tool-error path without a real service.
// Concurrency-safe for parity with the real caller, though the hub calls it
// inline.
type fakeLifecycleCaller struct {
	mu    sync.Mutex
	calls []lifecycleCall

	spawnResp   *compassv1internal.SpawnPeerResponse
	spawnErr    error
	despawnResp *compassv1internal.DespawnPeerResponse
	despawnErr  error
}

func (f *fakeLifecycleCaller) SpawnAsAccount(_ context.Context, caller store.AccountID, req *compassv1internal.SpawnPeerRequest) (*compassv1internal.SpawnPeerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, lifecycleCall{account: caller, spawn: req})
	if f.spawnErr != nil {
		return nil, f.spawnErr
	}
	return f.spawnResp, nil
}

func (f *fakeLifecycleCaller) DespawnAsAccount(_ context.Context, caller store.AccountID, req *compassv1internal.DespawnPeerRequest) (*compassv1internal.DespawnPeerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, lifecycleCall{account: caller, despawn: req})
	if f.despawnErr != nil {
		return nil, f.despawnErr
	}
	return f.despawnResp, nil
}

func (f *fakeLifecycleCaller) snapshot() []lifecycleCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]lifecycleCall(nil), f.calls...)
}

// newHubWithLifecycle builds a hub whose LifecycleCaller is the returned fake
// (wired post-construction via SetLifecycleCaller, the real wiring path), so a
// RelayLifecycleCall test drives the resolve->attribute->delegate path and
// asserts on the caller account the fake was called with. Like newHubOnly
// otherwise.
func newHubWithLifecycle() (*Hub, *fakeLifecycleCaller) {
	fake := &fakeLifecycleCaller{}
	hub := newHubOnly()
	hub.SetLifecycleCaller(fake)
	return hub, fake
}

// boardCall records one BoardCaller invocation: the account the hub attributed
// it to plus the request forwarded. A test asserts the hub delegated under the
// RESOLVED caller account (never the Runner's, never admin).
type boardCall struct {
	account       store.AccountID
	setIssueState *compassv1internal.SetIssueStateRequest
}

// fakeBoardCaller is a hand-written BoardCaller mirroring fakeLifecycleCaller:
// it records every call (account + request) so a test asserts the hub attributed
// to the bound account and forwarded the exact request, and returns a
// configurable canned response or error so a test drives both the success and
// the in-band tool-error path without a real service. Concurrency-safe for
// parity with the real caller, though the hub calls it inline.
type fakeBoardCaller struct {
	mu    sync.Mutex
	calls []boardCall

	resp *compassv1internal.SetIssueStateResponse
	err  error
}

func (f *fakeBoardCaller) SetIssueStateAsAccount(_ context.Context, caller store.AccountID, req *compassv1internal.SetIssueStateRequest) (*compassv1internal.SetIssueStateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, boardCall{account: caller, setIssueState: req})
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func (f *fakeBoardCaller) snapshot() []boardCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]boardCall(nil), f.calls...)
}

// newHubWithBoard builds a hub whose BoardCaller is the returned fake (wired
// post-construction via SetBoardCaller, the real wiring path), so a
// RelayBoardCall test drives the resolve->attribute->delegate path and asserts
// on the caller account the fake was called with. Like newHubOnly otherwise.
func newHubWithBoard() (*Hub, *fakeBoardCaller) {
	fake := &fakeBoardCaller{}
	hub := newHubOnly()
	hub.SetBoardCaller(fake)
	return hub, fake
}

// resolverEntry is one token the fakeResolver knows: the subject it resolves to
// and whether it has been revoked.
type resolverEntry struct {
	subj    store.Subject
	revoked bool
}

// fakeResolver is a hand-written TokenResolver that models the T3
// auth.ResolveToken contract verbatim: an unknown token is store.ErrNotFound, a
// revoked one is store.ErrTokenRevoked, and a token whose stored kind differs
// from the wanted kind is a wrong-kind error. The door under test collapses all
// three to a bare Unauthenticated — the no-oracle contract the cross-door tests
// pin. Keyed by the presented plaintext token.
type fakeResolver struct {
	tokens map[string]resolverEntry
}

// errWrongKind is the wrong-kind resolver failure: a stored token whose subject
// kind is not the kind the door asked for (the OQ7 cross-door rejection cause on
// this side — an account token presented to the RunnerService door).
var errWrongKind = &wrongKindError{}

type wrongKindError struct{}

func (*wrongKindError) Error() string { return "store: token subject kind mismatch" }

func (r *fakeResolver) resolve(_ context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
	e, ok := r.tokens[presented]
	if !ok {
		return store.Subject{}, store.ErrNotFound
	}
	if e.revoked {
		return store.Subject{}, store.ErrTokenRevoked
	}
	if e.subj.Kind != want {
		return store.Subject{}, errWrongKind
	}
	return e.subj, nil
}

// --- protobuf frame builders -------------------------------------------------

// sessionStateFrame wraps a SessionFrame carrying only a lifecycle transition.
func sessionStateFrame(state compassv1.AgentSessionState) *compassv1internal.AgentFrame {
	return &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_Session{
			Session: &compassv1internal.SessionFrame{State: state},
		},
	}
}

// sessionTraceFrame wraps a SessionFrame carrying a trace event only (no
// transition — state UNSPECIFIED).
func sessionTraceFrame(event string) *compassv1internal.AgentFrame {
	return &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_Session{
			Session: &compassv1internal.SessionFrame{
				TypedEvent: &compassv1.SessionEvent{
					Event: &compassv1.SessionEvent_AssistantText{
						AssistantText: &compassv1.SessionAssistantText{Text: event},
					},
				},
				State: compassv1.AgentSessionState_AGENT_SESSION_STATE_UNSPECIFIED,
			},
		},
	}
}

// --- h2c transport (the shipped cleartext-HTTP/2 door, minus the socket) -----

// cleartextHTTP2 enables HTTP/1.1 and prior-knowledge cleartext HTTP/2 (h2c),
// matching serve.go's door so the handler is exercised over the wire it ships
// on.
func cleartextHTTP2() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

// h2cHTTPClient builds an *http.Client that speaks prior-knowledge h2c to a TCP
// base URL, routing every dial through a plaintext dialer.
func h2cHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{
		Protocols: p,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// newMountedH2CServer stands up the RunnerService handler (mounted behind the
// bearer interceptor over resolve) on an httptest h2c server and returns its
// base URL. Torn down via t.Cleanup. No secret resolver is wired — for the
// FetchSecrets tests that need one, use newMountedH2CServerWithResolver.
func newMountedH2CServer(t *testing.T, hub *Hub, resolve TokenResolver) string {
	t.Helper()
	return newMountedH2CServerWithResolver(t, hub, resolve, nil)
}

// newMountedH2CServerWithResolver is newMountedH2CServer with a secret resolver
// threaded into the handler, so a FetchSecrets test drives the resolve path over
// the real wire.
func newMountedH2CServerWithResolver(t *testing.T, hub *Hub, resolve TokenResolver, resolver secrets.Resolver) string {
	t.Helper()
	return newMountedH2CServerWith(t, hub, resolve, resolver, nil)
}

// newMountedH2CServerWithConfig is newMountedH2CServer with a config store
// threaded into the handler, so a FetchAgentConfig test drives the delegate path
// over the real wire.
func newMountedH2CServerWithConfig(t *testing.T, hub *Hub, resolve TokenResolver, configStore AgentConfigStore) string {
	t.Helper()
	return newMountedH2CServerWith(t, hub, resolve, nil, configStore)
}

// newMountedH2CServerWith mounts the handler with both delegate surfaces (either
// may be nil) on an httptest h2c server and returns its base URL.
func newMountedH2CServerWith(t *testing.T, hub *Hub, resolve TokenResolver, resolver secrets.Resolver, configStore AgentConfigStore) string {
	t.Helper()
	path, handler := NewMountedHandler(hub, resolve, resolver, configStore)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

// newRawRunnerClient builds the real generated RunnerServiceClient dialing
// baseURL over h2c, stamping token on every RPC via a client bearer interceptor.
func newRawRunnerClient(t *testing.T, baseURL, token string) compassv1internalconnect.RunnerServiceClient {
	t.Helper()
	return compassv1internalconnect.NewRunnerServiceClient(
		h2cHTTPClient(t), baseURL,
		connect.WithInterceptors(&clientBearer{token: token}),
	)
}

// clientBearer stamps a bearer token on every outbound RPC (unary + streaming),
// mirroring the Runner-side interceptor so the door authenticates the client.
type clientBearer struct {
	token string
}

func (b *clientBearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if b.token != "" {
			req.Header().Set("Authorization", "Bearer "+b.token)
		}
		return next(ctx, req)
	}
}

func (b *clientBearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		if b.token != "" {
			conn.RequestHeader().Set("Authorization", "Bearer "+b.token)
		}
		return conn
	}
}

func (b *clientBearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// staticHeader is a minimal header for driving authenticate() directly: it
// satisfies the interface{ Get(string) string } authenticate accepts.
type staticHeader map[string]string

func (h staticHeader) Get(key string) string { return h[key] }

// bearerHeader builds a staticHeader carrying "Authorization: Bearer <token>".
func bearerHeader(token string) staticHeader {
	return staticHeader{"Authorization": "Bearer " + token}
}

// mustContain fails the test unless haystack contains needle.
func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%q does not contain %q", haystack, needle)
	}
}
