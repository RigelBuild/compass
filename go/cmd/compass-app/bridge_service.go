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

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

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

// windowDispatcher routes response frames to their originating webview.
// Keeping Wails types behind this seam lets unix builds and tests avoid the
// GTK-only application package while preserving per-window delivery.
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

	// connectMu serializes configured-server Connect probes that share one bearer
	// slot. Server choices are serialized by firstRunGate.
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

	// window is captured from the bound-method context before the pump goroutine
	// outlives that call. Frames route to this window; nil uses app-wide emission.
	// Its comparable value also keys close-time cancellation.
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
	if setup.newTarget == nil {
		setup.newTarget = bridge.NewTLSTarget
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

// responseFrame is the JS payload emitted per pump frame: a tagged
// head/body/end/error union. Headers are [name,value] pairs, and body chunks
// are base64 strings decoded with atob in the JS client.
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

// CompassRPC launches a gRPC-Web call and emits its ordered frames as
// "compass_rpc:" + requestId. Its detached context preserves caller values but
// outlives the Wails method call; only a terminal frame or explicit cancel ends
// the stream.
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
		if len(caPEM) == 0 {
			return connectResult{Kind: connectKindInvalidCA, Message: "The file is not a PEM certificate."}
		}
	}
	candidate, err := setup.newTarget(serverURL, caPEM)
	if err != nil {
		return connectResult{Kind: connectKindInvalidCA, Message: "The file is not a PEM certificate."}
	}
	installed := false
	defer func() {
		if !installed {
			client, _ := candidate.Client()
			client.CloseIdleConnections()
		}
	}()

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
		ctx,
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
	installed = true
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
	cc := compassv1connect.NewCompassServiceClient(connect.NewClient(connecthttp.NewTransport(client, serverURL, connecthttp.WithReadMaxBytes(0))))
	infoResp, err := cc.GetServerInfo(ctx, &compassv1.GetServerInfoRequest{})
	if err != nil {
		kind, message := classifyConnectErr(err)
		return connectResult{Kind: kind, Message: message}
	}
	serverVersion := infoResp.GetVersion()
	serverAPIVersion := infoResp.GetApiVersion()
	if serverAPIVersion != clientAPIVersion {
		return connectResult{
			Kind:          connectKindVersionMismatch,
			Message:       "The app speaks " + clientAPIVersion + "; the server speaks " + serverAPIVersion,
			ServerVersion: serverVersion,
			APIVersion:    serverAPIVersion,
		}
	}

	whoResp, err := cc.WhoAmI(ctx, &compassv1.WhoAmIRequest{})
	if err != nil {
		kind, message := classifyConnectErr(err)
		return connectResult{Kind: kind, Message: message}
	}
	accountID := whoResp.GetAccountId()
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

// cancelWindow cancels and removes calls for a closing window. Without this,
// their pump goroutines and server subscriptions outlive the window. Matching
// uses the comparable windowDispatcher captured at registration; nil matches
// nothing. Cancellation runs outside the lock.
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

// register detaches the pump context from the bound-method lifetime while
// preserving request values; the stream ends only on a terminal frame or
// explicit cancellation. It captures the originating window before the call
// returns so emitted frames route correctly.
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

// run snapshots the connection so setup cannot switch transports mid-request.
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

// emitFrame routes each response to its originating window; closed-window
// dispatchers drop frames. Calls without a window use the app-wide emitter.
func (s *bridgeService) emitFrame(call *inflightCall, name string, resp responseFrame) {
	if call.window != nil {
		call.window.dispatch(name, resp)
		return
	}
	s.events.Emit(name, resp)
}

// finish removes the in-flight entry only if it still points to this call,
// so an older call cannot delete a replacement registered under the same id.
// It then cancels this call's context.
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
	// The sealed bridge.Frame union makes this unreachable; return an error frame
	// rather than panic if a future variant is added.
	return responseFrame{Kind: frameKindError, Message: "compass bridge: unknown response frame"}
}

// clientAPIVersion is pinned because the server's constant is unexported. A
// live GetServerInfo test guards drift.
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

// connectResult is rendered by the webview. Message, ServerVersion and
// APIVersion can contain untrusted server text and MUST be escaped, not
// injected as HTML. AccountID is returned here, not stored on the service,
// whose unlocked field would race concurrent Connect calls.
type connectResult struct {
	OK            bool   `json:"ok"`
	Kind          string `json:"kind"`    // "" | "bad-url" | "bad-cert" | "bad-token" | "invalid-url" | "invalid-ca" | "version-mismatch" | "other"
	Message       string `json:"message"` // safe from the token; MAY echo untrusted server text — render escaped
	AccountID     string `json:"accountId"`
	ServerVersion string `json:"serverVersion"` // untrusted server-reported string
	APIVersion    string `json:"apiVersion"`    // untrusted server-reported string
	ServerURL     string `json:"serverUrl"`
}

// classifyConnectErr maps wrapped TLS/dial failures to sealed connect kinds;
// errors.AsType reaches their causes through the Connect wrapper.
func classifyConnectErr(err error) (kind, message string) {
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return connectKindBadCert, "The server's certificate is not trusted"
	}

	badURL := false
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		badURL = true
	} else if _, ok := errors.AsType[*net.OpError](err); ok {
		badURL = true
	} else if errors.Is(err, context.DeadlineExceeded) {
		badURL = true
	}
	if badURL {
		return connectKindBadURL, "Could not reach the server at this URL"
	}

	// Only CodeUnauthenticated maps to bad-token; deadlines map to bad-url and
	// other codes remain the explicit residual. Widening this mapping changes the
	// contract and requires a design record.
	if connect.CodeOf(err) == connect.CodeUnauthenticated {
		return connectKindBadToken, "The server rejected this token"
	}

	return connectKindOther, "Could not connect to the server"
}
