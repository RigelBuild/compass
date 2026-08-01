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

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/sealedsecurity/compass/go/internal/secrets"
	"github.com/sealedsecurity/compass/go/internal/store"
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
			attached := router.send != nil
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

// convCall is one recorded PostAgentMessage: the account the hub resolved the
// session to, the session id it resolved from, and exactly one of
// posted/updated — mirroring the ConversationSink contract. The account is the
// fake's proof of WHICH attribution the hub applied, the same security
// invariant the RelayCommsCall tests defend through commsCall.
type convCall struct {
	account        store.AccountID
	sessionID      string
	idempotencyKey string
	posted         *compassv1.MessagePosted
	updated        *compassv1.MessageUpdated
}

// fakeConversationSink records the conversation write-throughs Deliver drives so
// a test can assert which variant reached the comms surface, under which account
// and session id. Concurrency-safe: Deliver can run from a PublishEvents handler
// goroutine.
type fakeConversationSink struct {
	mu    sync.Mutex
	calls []convCall
	err   error // returned by PostAgentMessage when set (a write-through failure)
}

func (f *fakeConversationSink) PostAgentMessage(_ context.Context, account store.AccountID, sessionID string, idempotencyKey string, posted *compassv1.MessagePosted, updated *compassv1.MessageUpdated) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, convCall{account: account, sessionID: sessionID, idempotencyKey: idempotencyKey, posted: posted, updated: updated})
	return f.err
}

func (f *fakeConversationSink) snapshot() []convCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]convCall(nil), f.calls...)
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

// newHub builds a hub over three fresh fake sinks and returns them, so a test
// asserts on exactly the sink it targets.
func newHub() (*Hub, *fakeConversationSink, *fakeLifecycleSink, *fakeTailSink) {
	conv := &fakeConversationSink{}
	life := &fakeLifecycleSink{}
	tail := &fakeTailSink{}
	return NewHub(conv, life, tail, nil, discardLogger()), conv, life, tail
}

// newHubOnly builds a hub over three fresh fake sinks and returns just the hub,
// for a test that drives Deliver/enroll/routing and asserts through the wire or
// the hub's own accessors rather than reaching into a specific sink.
func newHubOnly() *Hub {
	return NewHub(&fakeConversationSink{}, &fakeLifecycleSink{}, &fakeTailSink{}, nil, discardLogger())
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
	account store.AccountID
	post    *compassv1.PostMessageRequest
	list    *compassv1.ListMessagesRequest
	// Keyed-commit invocations (CommitConversationFrame path): exactly one of
	// commitPost/commitUpdate is set, alongside the forwarded idempotency key.
	commitPost   *compassv1.MessagePosted
	commitUpdate *compassv1.MessageUpdated
	commitKey    string
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

	// Keyed-commit canned responses (CommitConversationFrame path). commitPost
	// / commitUpdate drive the fresh-commit id; commitErr drives the
	// Connect-coded refusal both keyed methods return.
	commitPost   *compassv1.PostMessageResponse
	commitUpdate *compassv1.MessageUpdated
	commitErr    error
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

func (f *fakeCommsCaller) ListAsAccount(_ context.Context, account store.AccountID, req *compassv1.ListMessagesRequest) (*compassv1.ListMessagesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, commsCall{account: account, list: req})
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResp, nil
}

func (f *fakeCommsCaller) CommitAgentPostKeyed(_ context.Context, account store.AccountID, posted *compassv1.MessagePosted, idempotencyKey string) (*compassv1.PostMessageResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, commsCall{account: account, commitPost: posted, commitKey: idempotencyKey})
	if f.commitErr != nil {
		return nil, f.commitErr
	}
	return f.commitPost, nil
}

func (f *fakeCommsCaller) CommitAgentUpdateKeyed(_ context.Context, account store.AccountID, updated *compassv1.MessageUpdated, idempotencyKey string) (*compassv1.MessageUpdated, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, commsCall{account: account, commitUpdate: updated, commitKey: idempotencyKey})
	if f.commitErr != nil {
		return nil, f.commitErr
	}
	return f.commitUpdate, nil
}

func (f *fakeCommsCaller) snapshot() []commsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]commsCall(nil), f.calls...)
}

// newHubWithComms builds a hub whose CommsCaller is the returned fake, so a
// RelayCommsCall test drives the resolve->attribute->execute path and asserts on
// the account the fake was called with. Like newHubOnly otherwise (the three
// write-through sinks are unused stubs).
func newHubWithComms() (*Hub, *fakeCommsCaller) {
	comms := &fakeCommsCaller{}
	return NewHub(&fakeConversationSink{}, &fakeLifecycleSink{}, &fakeTailSink{}, comms, discardLogger()), comms
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

// convPostedFrame wraps a ConversationPosted variant carrying one text block.
func convPostedFrame(text string) *compassv1internal.AgentFrame {
	return &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_ConversationPosted{
			ConversationPosted: &compassv1.MessagePosted{
				Message: &compassv1.Message{
					Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: text}}},
				},
			},
		},
	}
}

// convUpdatedFrame wraps a ConversationUpdated variant carrying one text block
// and an ADDRESSED message id — the shape an update must have to name the row it
// edits. No production emitter produces this yet (see convUpdatedFrameIDLess
// below); it is the shape the relay will carry once Runner-side id
// reconciliation lands, and the shape every test that expects a commit needs.
func convUpdatedFrame(text string) *compassv1internal.AgentFrame {
	return &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_ConversationUpdated{
			ConversationUpdated: &compassv1.MessageUpdated{
				Message: &compassv1.Message{
					Id:     "msg-addressed",
					Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: text}}},
				},
			},
		},
	}
}

// convUpdatedFrameIDLess wraps a ConversationUpdated variant carrying one text
// block and NO message id — byte-for-byte the frame the first-party agent
// actually emits today (EventMapper.#appendBlock,
// packages/compass-agent/src/mapping.ts:386-393, which builds a Message from
// `blocks` alone because the agent has no server id to mint). Nothing between
// the agent and this hub stamps one, so this — not convUpdatedFrame — is the
// production shape on the current base.
func convUpdatedFrameIDLess(text string) *compassv1internal.AgentFrame {
	return &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_ConversationUpdated{
			ConversationUpdated: &compassv1.MessageUpdated{
				Message: &compassv1.Message{
					Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: text}}},
				},
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
	path, handler := NewMountedHandler(hub, resolve, resolver)
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

// firstTextBlock returns the text of a wire Message's first text block, or "".
func firstTextBlock(m *compassv1.Message) string {
	for _, b := range m.GetBlocks() {
		if t := b.GetText(); t != "" {
			return t
		}
	}
	return ""
}

// mustContain fails the test unless haystack contains needle.
func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%q does not contain %q", haystack, needle)
	}
}
