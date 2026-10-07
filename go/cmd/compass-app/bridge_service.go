//go:build unix

// The Compass desktop shell's IPC bridge service. It binds two methods to the
// webview — compass_rpc and compass_rpc_cancel — and proxies a single gRPC-Web
// call to the daemon through the already-merged bridge pump (go/internal/bridge).
//
// A webview fetch cannot dial the daemon's Unix socket, so the UI issues
// compass_rpc({requestId, path, headers, body}); this service forwards the call
// through the pump and streams each ordered response frame back as a Wails
// runtime event named "compass_rpc:"+requestId, carrying the JS ResponseFrame
// shape (apps/ui/src/daemon-transport.ts): head/body/end/error. compass_rpc_cancel
// cancels the in-flight call for a requestId.
//
// The service emits through a small eventEmitter seam rather than *application.App
// directly, so the whole frame/stream/cancel path is testable without a live
// webview: the real Wails app.Event satisfies the seam, and a test supplies a
// fake that captures frames. Per rule://go-no-panic-in-lib the service never
// panics or fatals — transport failures surface as error frames, and the pump's
// own contract (exactly one terminal frame, nothing after cancel) is preserved.
package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"sync"
	"sync/atomic"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/appconfig"
	"github.com/RigelBuild/compass/go/internal/bridge"
	"github.com/RigelBuild/compass/go/internal/tokenstore"
)

// eventEmitter is the seam the bridge service emits response frames through. Its
// signature matches *application.EventManager.Emit exactly, so the real Wails
// app.Event satisfies it directly (no adapter) and a test can substitute a fake
// that records every (name, data) pair without standing up a webview.
type eventEmitter interface {
	Emit(name string, data ...any) bool
}

// windowDispatcher is the per-window analogue of the eventEmitter seam: it
// delivers one response frame to a SINGLE originating webview window, rather
// than app-wide. The real Wails *application.WebviewWindow satisfies it via
// DispatchWailsEvent (webview_window.go:1372); a test substitutes a fake that
// records deliveries with no webview.
//
// The seam is why this file stays //go:build unix and imports no
// github.com/wailsapp/wails/v3/pkg/application: that package only compiles under
// the GTK4/WebKitGTK 6.0 stack, which the untagged toolchain has no pkg-config
// for (main_nogtk4.go documents it repo-wide), so a direct import
// would break the untagged module build and the unix-tagged tests. The concrete
// window handle is captured from the bound-method ctx by windowFromContext, a
// build-tagged helper (bridge_service_window_gtk4.go reads application.WindowKey
// and returns the window; the nogtk4 stub returns nil), mirroring the
// main.go / main_nogtk4.go split. §A4/§M3 mandate exactly this: per-window
// routing behind the existing eventEmitter seam.
type windowDispatcher interface {
	// dispatch delivers one frame to this window's webview under the given event
	// name. It no-ops for a destroyed window (the real DispatchWailsEvent guards
	// isDestroyed(), webview_window.go:1373), so a frame for a closed window's
	// call is dropped rather than broadcast (A4).
	dispatch(name string, resp responseFrame)
}

// bridgeService owns the shell's live connection and its IPC request state.
type bridgeService struct {
	conn   atomic.Pointer[connection]
	events eventEmitter
	tokens tokenstore.Store
	setup  *setupWiring

	// connectMu makes each Connect single-flight: the target bearer is one shared
	// slot, so overlapping probes could carry each other's token or disarm it.
	connectMu sync.Mutex
	// phase is "setup" or "reopen" while no connection is installed. It is never
	// a connection, so CompassRPC stays on the no-connection error.
	phase atomic.Pointer[string]

	// accountID is set once before app.Run and only read after, so it takes no
	// lock. Connect returns its id in the result rather than writing here.
	accountID string

	mu       sync.Mutex
	inflight map[string]*inflightCall
}

// connection is an immutable pump-and-target snapshot, installed as one value.
type connection struct {
	mode      string
	serverURL string
	target    *bridge.Target
	pump      *bridge.Pump
}

type shellStateResult struct {
	Mode      string `json:"mode"`
	ServerURL string `json:"serverUrl"`
}

// inflightCall is one live compass_rpc call's teardown handle. It is stored in
// the in-flight map by requestId and compared by pointer identity so a call only
// ever deletes/cancels its OWN entry — a re-registered id (same key, new call)
// never has its live entry mis-deleted by a prior call's deferred finish.
type inflightCall struct {
	cancel context.CancelFunc

	// window is the originating webview window for this call, captured from the
	// bound-method ctx by windowFromContext at register time — it is gone from
	// ctx by the time the pump goroutine emits, so it is captured up front and
	// stored here (record §M3:354). This call's response frames route to this
	// window only, via the windowDispatcher seam; nil means no originating window
	// was in context (a windowless transport, a non-gtk4 build, or a direct test
	// call), and the frames fall back to the app-wide eventEmitter. The interface
	// value is comparable, so it is also the index M3b's close-time
	// cancel-all-for-window keys on.
	window windowDispatcher
}

// newBridgeService installs one immutable pump-and-target connection snapshot.
func newBridgeService(conn *connection, events eventEmitter, tokens tokenstore.Store) *bridgeService {
	s := &bridgeService{
		events:   events,
		tokens:   tokens,
		inflight: make(map[string]*inflightCall),
	}
	if conn != nil {
		s.conn.Store(conn)
	}
	return s
}

func newSetupBridgeService(events eventEmitter, tokens tokenstore.Store, setup *setupWiring) *bridgeService {
	if setup == nil {
		setup = &setupWiring{}
	}
	if setup.gate == nil {
		setup.gate = &firstRunGate{}
	}
	if setup.picks == nil {
		setup.picks = &caPicks{}
	}
	if setup.saveClient == nil {
		setup.saveClient = appconfig.SaveClient
	}
	s := &bridgeService{
		events:   events,
		tokens:   tokens,
		setup:    setup,
		inflight: make(map[string]*inflightCall),
	}
	s.setPhase("setup")
	return s
}

// ShellState lets a new window re-read startup state after subscribing to setup.
func (s *bridgeService) ShellState() shellStateResult {
	mode, serverURL := s.shellState()
	return shellStateResult{Mode: mode, ServerURL: serverURL}
}

// AccountID is the bound IPC getter the webview calls to learn the caller
// account id resolved via WhoAmI (DL-111). The JS side (compass-ui zone) reads
// it over Wails IPC to build the native ConnectionProvider; the account id is
// server-derived, never client-supplied. It returns the empty string when no
// identity was resolved, which the JS treats as "not yet identified".
func (s *bridgeService) AccountID(_ context.Context) string {
	return s.accountID
}

// headerPair is one request/response header as the JS side models it: an ordered
// {name, value} object (apps/ui/src/daemon-transport.ts). Order is preserved.
type headerPair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// rpcRequest is the compass_rpc argument the webview sends. Fields are tagged
// for camelCase to match the JS caller (daemon-transport.ts: {requestId, path,
// headers:[{name,value}], body:number[]}). Body is raw request bytes; Go
// json.Unmarshal decodes a JS number[] into []byte element-wise.
type rpcRequest struct {
	RequestID string       `json:"requestId"`
	Path      string       `json:"path"`
	Headers   []headerPair `json:"headers"`
	Body      []byte       `json:"body"`
}

// cancelRequest is the compass_rpc_cancel argument: the requestId to tear down.
type cancelRequest struct {
	RequestID string `json:"requestId"`
}

// responseFrame is the JS ResponseFrame payload emitted per pump frame
// (daemon-transport.ts:19-23): a tagged head/body/end/error union. Kind selects
// which of the optional fields is populated; head headers are [name,value] tuple
// arrays (marshalling to [["name","value"],...]) matching the JS [string,string][]
// contract, and body chunks are standard base64 so they ride the JSON event
// channel as strings (the JS decodeChunk does atob).
type responseFrame struct {
	Kind    string      `json:"kind"`
	Status  int         `json:"status,omitempty"`
	Headers [][2]string `json:"headers,omitempty"`
	Chunk   string      `json:"chunk,omitempty"`
	Message string      `json:"message,omitempty"`
}

// Frame kinds tag the JS ResponseFrame union emitted per pump frame — the shared
// vocabulary with apps/ui/src/daemon-transport.ts (head|body|end|error).
const (
	frameKindHead  = "head"
	frameKindBody  = "body"
	frameKindEnd   = "end"
	frameKindError = "error"
)

// CompassRPC forwards one gRPC-Web call to the daemon and streams the response
// back as runtime events keyed by requestId. It returns as soon as the call is
// launched; the pump runs on its own goroutine and each ordered frame is emitted
// as "compass_rpc:"+requestId in frame order.
//
// The forwarding context is derived from the caller's ctx with WithoutCancel so
// request-scoped values propagate, but detached from the bound-method
// invocation's lifetime — the stream must outlive this method's return and is
// torn down only by a terminal frame or an explicit compass_rpc_cancel, never by
// Wails reclaiming the call context. WithCancel supplies the cancel stored for
// compass_rpc_cancel.
func (s *bridgeService) CompassRPC(ctx context.Context, req rpcRequest) {
	callCtx, call := s.register(ctx, req.RequestID)
	go s.run(callCtx, call, req)
}

// CompassRPCCancel cancels the in-flight call for a requestId and drops its
// entry. A canceled pump stops silently (no further frames), matching the pump
// contract. A cancel for an unknown or already-finished id is a no-op.
func (s *bridgeService) CompassRPCCancel(_ context.Context, req cancelRequest) {
	s.mu.Lock()
	call, ok := s.inflight[req.RequestID]
	delete(s.inflight, req.RequestID)
	s.mu.Unlock()
	if ok {
		call.cancel()
	}
}

// Connect probes and arms the configured server, then stores the candidate token.
// An empty token reads the stored token for that server. A server choice uses
// first-run wiring to validate, probe, and save the selected remote before it is
// installed. Failed probes leave their target disarmed; tokens are never logged.
func (s *bridgeService) Connect(ctx context.Context, req connectRequest) connectResult {
	if req.Server != nil {
		return s.connectServerChoice(ctx, req)
	}
	conn := s.conn.Load()
	if conn == nil || conn.target == nil || s.tokens == nil {
		return connectResult{Kind: connectKindOther, Message: "Connect is not available: no remote target is configured"}
	}

	s.connectMu.Lock()
	defer s.connectMu.Unlock()

	_, serverURL := conn.target.Client()
	candidate := req.Token
	if candidate == "" {
		stored, err := s.tokens.Read(serverURL)
		if err != nil {
			if errors.Is(err, tokenstore.ErrNotFound) {
				return connectResult{Kind: connectKindBadToken, Message: "No stored token; enter one to connect"}
			}
			return connectResult{Kind: connectKindOther, Message: "Could not read the stored token"}
		}
		candidate = stored
	}

	result := s.probe(ctx, conn.target, candidate)
	if !result.OK {
		return result
	}
	if err := s.tokens.Write(serverURL, candidate); err != nil {
		conn.target.SetBearer("")
		return connectResult{Kind: connectKindOther, Message: "Connected, but could not save the token"}
	}
	return result
}

func (s *bridgeService) connectServerChoice(ctx context.Context, req connectRequest) connectResult {
	if s.setup == nil {
		return connectResult{Kind: connectKindOther, Message: "The server is set in app.toml."}
	}
	setup := s.setup
	if err := setup.gate.begin(); err != nil {
		return connectResult{Kind: connectKindOther, Message: err.Error()}
	}
	finished := false
	defer func() {
		if !finished {
			setup.gate.end(false)
		}
	}()

	serverURL, err := appconfig.NormalizeServerURL(req.Server.URL)
	if err != nil {
		if urlErr, ok := errors.AsType[*appconfig.URLError](err); ok {
			return connectResult{Kind: connectKindInvalidURL, Message: urlErr.Reason}
		}
		return connectResult{Kind: connectKindInvalidURL, Message: err.Error()}
	}

	var caPEM []byte
	if req.Server.CARef != "" {
		var ok bool
		caPEM, ok = setup.picks.get(req.Server.CARef)
		if !ok {
			return connectResult{Kind: connectKindInvalidCA, Message: "Choose the certificate again."}
		}
	}
	candidate, err := bridge.NewTLSTarget(serverURL, caPEM)
	if err != nil {
		return connectResult{Kind: connectKindInvalidCA, Message: "The file is not a PEM certificate."}
	}

	if s.tokens == nil {
		return connectResult{Kind: connectKindOther, Message: "Connect is not available: no remote target is configured"}
	}
	token := req.Token
	if token == "" {
		token, err = s.tokens.Read(serverURL)
		if err != nil {
			if errors.Is(err, tokenstore.ErrNotFound) {
				return connectResult{Kind: connectKindBadToken, Message: "No stored token; enter one to connect"}
			}
			return connectResult{Kind: connectKindOther, Message: "Could not read the stored token"}
		}
	}

	result := s.probe(ctx, candidate, token)
	if !result.OK {
		return result
	}
	_, err = setup.saveClient(
		setup.configPath,
		appconfig.Config{Mode: appconfig.ModeClient, ServerURL: serverURL},
		caPEM,
	)
	if errors.Is(err, appconfig.ErrConfigExists) {
		candidate.SetBearer("")
		decide(setup.gate, s, false)
		finished = true
		return connectResult{Kind: connectKindOther, Message: setupDecidedMessage}
	}
	if err != nil {
		candidate.SetBearer("")
		return connectResult{Kind: connectKindOther, Message: "Connected, but the settings could not be saved: " + err.Error()}
	}
	if err := s.tokens.Write(serverURL, token); err != nil {
		candidate.SetBearer("")
		setup.picks.clear()
		decide(setup.gate, s, false)
		finished = true
		return connectResult{Kind: connectKindOther, Message: "Saved, but could not store the token. Quit and reopen Compass, then enter it again."}
	}

	s.conn.Store(&connection{
		mode:      "client",
		serverURL: serverURL,
		target:    candidate,
		pump:      bridge.NewPump(candidate),
	})
	setup.picks.clear()
	decide(setup.gate, s, true)
	finished = true
	return result
}

// probe arms target with token and runs GetServerInfo, the API-version check, and
// WhoAmI. It disarms on any failure, leaves the target armed on success, and
// stores nothing.
func (s *bridgeService) probe(ctx context.Context, target *bridge.Target, token string) connectResult {
	// SetBearer is the only way to send the token: the RoundTripper strips any
	// request-level Authorization header (DL-107).
	target.SetBearer(token)
	ok := false
	defer func() {
		if !ok {
			target.SetBearer("")
		}
	}()

	client, serverURL := target.Client()
	cc := compassv1connect.NewCompassServiceClient(client, serverURL)
	infoResp, err := cc.GetServerInfo(ctx, connect.NewRequest(&compassv1.GetServerInfoRequest{}))
	if err != nil {
		kind, message := classifyConnectErr(err)
		return connectResult{Kind: kind, Message: message}
	}
	serverVersion := infoResp.Msg.GetVersion()
	serverAPIVersion := infoResp.Msg.GetApiVersion()
	if serverAPIVersion != clientAPIVersion {
		return connectResult{
			Kind:          connectKindVersionMismatch,
			Message:       "The app speaks " + clientAPIVersion + "; the server speaks " + serverAPIVersion,
			ServerVersion: serverVersion,
			APIVersion:    serverAPIVersion,
		}
	}

	whoResp, err := cc.WhoAmI(ctx, connect.NewRequest(&compassv1.WhoAmIRequest{}))
	if err != nil {
		kind, message := classifyConnectErr(err)
		return connectResult{Kind: kind, Message: message}
	}
	accountID := whoResp.Msg.GetAccountId()
	if accountID == "" {
		return connectResult{Kind: connectKindOther, Message: "The server returned an empty account id"}
	}

	ok = true
	return connectResult{
		OK:            true,
		AccountID:     accountID,
		ServerVersion: serverVersion,
		APIVersion:    serverAPIVersion,
		ServerURL:     serverURL,
	}
}

// cancelWindow cancels every in-flight call registered to a closing window and
// drops their entries, driving the same id-keyed teardown as compass_rpc_cancel
// (a canceled pump stops silently — no further frames — so the server-side
// subscription terminates). It is the close-time leak gate (record §M3b): a
// window closing without this leaves its calls' pump goroutines and server
// subscriptions live for the app's lifetime. Matching is by the comparable
// windowDispatcher the call captured at register time (bridge_service.go window
// field); a nil win matches nothing (fallback/windowless calls are never swept
// by a close). Cancels run outside the lock, matching CompassRPCCancel.
func (s *bridgeService) cancelWindow(win windowDispatcher) {
	if win == nil {
		return
	}
	s.mu.Lock()
	var doomed []*inflightCall
	for id, call := range s.inflight {
		if call.window == win {
			doomed = append(doomed, call)
			delete(s.inflight, id)
		}
	}
	s.mu.Unlock()
	for _, call := range doomed {
		call.cancel()
	}
}

// setPhase publishes the setup phase without coupling window startup to Connect.
func (s *bridgeService) setPhase(phase string) {
	s.phase.Store(&phase)
}

func (s *bridgeService) shellState() (mode, serverURL string) {
	if conn := s.conn.Load(); conn != nil {
		return conn.mode, conn.serverURL
	}
	if phase := s.phase.Load(); phase != nil {
		return *phase, ""
	}
	return "", ""
}

// register derives the forwarding context for a call and records the call's
// teardown handle under requestID, cancelling any prior call already under that
// id first (so a stale forwarder can never keep emitting onto the same event).
// It returns the created *inflightCall so run/finish can guard deletion by
// pointer identity — a re-registered id never has its live entry mis-deleted by
// a prior call's deferred finish. The context is derived from the caller's ctx
// with WithoutCancel so request-scoped values propagate, but detached from the
// bound-method invocation's lifetime — the stream must outlive CompassRPC's
// return and is torn down only by a terminal frame or an explicit
// compass_rpc_cancel, never by Wails reclaiming the call context.
//
// The originating window is captured HERE, off the still-live bound-method ctx,
// by windowFromContext (a build-tagged helper: the gtk4 build reads
// application.WindowKey, set by Wails at messageprocessor_call.go:136, and
// returns the window; the nogtk4 build returns nil). WithoutCancel would preserve
// the value on callCtx too, but the frames emit on the pump goroutine after
// CompassRPC has returned, so the handle is read synchronously up front and
// stored (record §M3:354). A nil result (no window in ctx — a windowless
// transport, a non-gtk4 build, or a direct test call) routes frames to the
// app-wide fallback.
func (s *bridgeService) register(ctx context.Context, requestID string) (context.Context, *inflightCall) {
	win := windowFromContext(ctx)
	callCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	call := &inflightCall{cancel: cancel, window: win}
	s.mu.Lock()
	if prev, ok := s.inflight[requestID]; ok {
		prev.cancel()
	}
	s.inflight[requestID] = call
	s.mu.Unlock()
	return callCtx, call
}

// run loads once so a request cannot switch transports midway through setup.
func (s *bridgeService) run(callCtx context.Context, call *inflightCall, req rpcRequest) {
	defer s.finish(req.RequestID, call)
	eventName := "compass_rpc:" + req.RequestID
	conn := s.conn.Load()
	if conn == nil {
		// A canceled call stays silent, as a canceled pump does.
		if callCtx.Err() == nil {
			s.emitFrame(call, eventName, responseFrame{Kind: frameKindError, Message: "Not connected to a server"})
		}
		return
	}
	rpc := bridge.Call{
		Path:    req.Path,
		Headers: headerSlice(req.Headers),
		Body:    req.Body,
	}
	conn.pump.Do(callCtx, rpc, func(f bridge.Frame) {
		s.emitFrame(call, eventName, frameToResponse(f))
	})
}

// emitFrame is the per-call response-frame sink: it routes one frame to the
// call's originating window when one was captured, else to the app-wide emitter.
//
// A captured window gets the frame via the windowDispatcher seam — the real one
// is *application.WebviewWindow.DispatchWailsEvent (per-window delivery,
// webview_window.go:1372), so the app-wide broadcast (transport_event_ipc.go) is
// bypassed and a frame never reaches a non-owning window. That method internally
// no-ops once the window isDestroyed() (webview_window.go:1373), so a frame for a
// call whose window has since closed is silently dropped rather than broadcast —
// M3 routes and inherits that drop; it adds no destroyed check of its own (A4).
//
// A nil window (no window in the caller's ctx — a windowless transport, a
// non-gtk4 build, or a direct test call) falls back to the app-wide eventEmitter
// seam (main.go:104), preserving both non-window callers and the fake-emitter
// test path (§M3).
func (s *bridgeService) emitFrame(call *inflightCall, name string, resp responseFrame) {
	if call.window != nil {
		call.window.dispatch(name, resp)
		return
	}
	s.events.Emit(name, resp)
}

// finish drops the in-flight entry for a completed call, then cancels its own
// context (idempotent). Deletion is guarded by pointer identity: it removes the
// entry only if the CURRENT entry under requestID is still THIS call, so a call
// whose id was re-registered by a later call leaves the live entry untouched and
// only cancels its own (already-finished) context.
func (s *bridgeService) finish(requestID string, call *inflightCall) {
	s.mu.Lock()
	if cur, ok := s.inflight[requestID]; ok && cur == call {
		delete(s.inflight, requestID)
	}
	s.mu.Unlock()
	call.cancel()
}

// headerSlice converts the JS {name,value} header objects into the pump's
// ordered [][2]string, preserving order (including repeated names).
func headerSlice(pairs []headerPair) [][2]string {
	if len(pairs) == 0 {
		return nil
	}
	out := make([][2]string, len(pairs))
	for i, p := range pairs {
		out[i] = [2]string{p.Name, p.Value}
	}
	return out
}

// frameToResponse maps a pump frame to the JS ResponseFrame payload. The switch
// is exhaustive over the sealed bridge.Frame union (exhaustive/gochecksumtype
// gate); body chunk bytes become a standard-base64 string for the JSON channel.
func frameToResponse(f bridge.Frame) responseFrame {
	switch frame := f.(type) {
	case bridge.HeadFrame:
		return responseFrame{Kind: frameKindHead, Status: frame.Status, Headers: frame.Headers}
	case bridge.BodyFrame:
		return responseFrame{Kind: frameKindBody, Chunk: base64.StdEncoding.EncodeToString(frame.Chunk)}
	case bridge.EndFrame:
		return responseFrame{Kind: frameKindEnd}
	case bridge.ErrorFrame:
		return responseFrame{Kind: frameKindError, Message: frame.Message}
	}
	// Unreachable: bridge.Frame is a sealed union and the switch above is
	// exhaustive. Returning an error frame keeps go-no-panic-in-lib clean rather
	// than panicking on a hypothetical new variant.
	return responseFrame{Kind: frameKindError, Message: "compass bridge: unknown response frame"}
}

// clientAPIVersion is the compass API version the app speaks. The server's own
// apiVersion constant is unexported (go/server/service.go), so the app pins its
// OWN literal and a drift-guard test cross-checks it against a live
// GetServerInfo. Keep in sync with go/server/service.go apiVersion.
const clientAPIVersion = "compass.v1"

// connectKind* is the sealed failure-kind vocabulary a Connect probe returns
// (empty Kind = success). It is the contract the connect screen renders one
// visual state per (T5.5/T5.6).
const (
	connectKindBadURL          = "bad-url"
	connectKindBadCert         = "bad-cert"
	connectKindBadToken        = "bad-token"
	connectKindVersionMismatch = "version-mismatch"
	connectKindOther           = "other"
	connectKindInvalidURL      = "invalid-url"
	connectKindInvalidCA       = "invalid-ca"
)

type serverChoice struct {
	URL   string `json:"url"`
	CARef string `json:"caRef"`
}

type connectRequest struct {
	Token  string        `json:"token"`
	Server *serverChoice `json:"server,omitempty"`
}

// connectResult is the Connect outcome the webview renders. Kind is the sealed
// failure vocabulary. Message NEVER contains the token, but it is NOT fully
// app-controlled: the version-mismatch Message, and the ServerVersion /
// APIVersion fields, echo strings the remote server reported — untrusted input
// (a spoofed daemon can return any bytes). The renderer (T5.5/T5.6) MUST treat
// Message, ServerVersion and APIVersion as plain text — escaped, never injected
// as HTML — so a hostile server cannot script the webview.
// AccountID rides in the result only — it is deliberately NOT written to the
// service's set-once accountID field, which is read without a lock and would
// race a webview-goroutine Connect (bridge_service.go accountID doc).
type connectResult struct {
	OK            bool   `json:"ok"`
	Kind          string `json:"kind"`    // "" | "bad-url" | "bad-cert" | "bad-token" | "version-mismatch" | "other"
	Message       string `json:"message"` // safe from the token; MAY echo untrusted server text — render escaped
	AccountID     string `json:"accountId"`
	ServerVersion string `json:"serverVersion"` // untrusted server-reported string
	APIVersion    string `json:"apiVersion"`    // untrusted server-reported string
	ServerURL     string `json:"serverUrl"`
}

// classifyConnectErr maps a probe error to a sealed connect kind + safe message.
// connect wraps transport failures, so a TLS/dial cause surfaces as a
// *connect.Error (CodeUnavailable) wrapping the net/tls error; errors.As reaches
// the underlying cause THROUGH the connect wrapper.
func classifyConnectErr(err error) (kind, message string) {
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return connectKindBadCert, "The server's certificate is not trusted"
	}

	var dnsErr *net.DNSError
	var opErr *net.OpError
	if errors.As(err, &dnsErr) || errors.As(err, &opErr) || errors.Is(err, context.DeadlineExceeded) {
		return connectKindBadURL, "Could not reach the server at this URL"
	}

	// The sealed mapping follows the design record (T5.3): only
	// CodeUnauthenticated is bad-token, deadline is folded into bad-url above,
	// and every other code — CodePermissionDenied (403 on a revoked token),
	// CodeUnavailable with no net/tls cause, etc. — is the explicit `other`
	// residual, never a silent fallthrough. Widening it is a contract change
	// (a new design record), not an inline tweak.
	if connect.CodeOf(err) == connect.CodeUnauthenticated {
		return connectKindBadToken, "The server rejected this token"
	}

	return connectKindOther, "Could not connect to the server"
}
