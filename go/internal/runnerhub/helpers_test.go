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
		if router, err := hub.routerFor("gate"); err == nil {
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

// convCall is one recorded PostAgentMessage: exactly one of posted/updated is
// non-nil, mirroring the ConversationSink contract.
type convCall struct {
	sessionID string
	posted    *compassv1.MessagePosted
	updated   *compassv1.MessageUpdated
}

// fakeConversationSink records the conversation write-throughs Deliver drives so
// a test can assert which variant reached the comms surface, under which session
// id. Concurrency-safe: Deliver can run from a PublishEvents handler goroutine.
type fakeConversationSink struct {
	mu    sync.Mutex
	calls []convCall
	err   error // returned by PostAgentMessage when set (a write-through failure)
}

func (f *fakeConversationSink) PostAgentMessage(_ context.Context, sessionID string, posted *compassv1.MessagePosted, updated *compassv1.MessageUpdated) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, convCall{sessionID: sessionID, posted: posted, updated: updated})
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

// commsCall records one CommsCaller invocation: the account the hub resolved
// the session to, and exactly one of the post/list request it dispatched. It is
// the fake's proof of WHICH account attribution the hub applied — the security
// invariant the RelayCommsCall tests defend.
type commsCall struct {
	account store.AccountID
	post    *compassv1.PostMessageRequest
	list    *compassv1.ListMessagesRequest
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

// convUpdatedFrame wraps a ConversationUpdated variant carrying one text block.
func convUpdatedFrame(text string) *compassv1internal.AgentFrame {
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
// base URL. Torn down via t.Cleanup.
func newMountedH2CServer(t *testing.T, hub *Hub, resolve TokenResolver) string {
	t.Helper()
	path, handler := NewMountedHandler(hub, resolve)
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
