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
	"encoding/base64"
	"sync"

	"github.com/sealedsecurity/compass/go/internal/bridge"
)

// eventEmitter is the seam the bridge service emits response frames through. Its
// signature matches *application.EventManager.Emit exactly, so the real Wails
// app.Event satisfies it directly (no adapter) and a test can substitute a fake
// that records every (name, data) pair without standing up a webview.
type eventEmitter interface {
	Emit(name string, data ...any) bool
}

// bridgeService binds the compass_rpc / compass_rpc_cancel IPC methods and
// forwards each call through the pump. In-flight calls are tracked by requestId
// so compass_rpc_cancel can tear one down; the map is mutex-guarded because the
// bound methods are invoked from the webview's goroutines concurrently with the
// per-call forwarding goroutines that delete their own entry on terminal frame.
type bridgeService struct {
	pump   *bridge.Pump
	events eventEmitter

	// accountID is the caller account id resolved by the embedded launch
	// pipeline via WhoAmI (DL-111), exposed to the JS/UI through the bound
	// AccountID method so it can build the native ConnectionProvider. It is set
	// once at construction (before app.Run) and only read thereafter, so it needs
	// no lock. Empty in client mode or when identity was not resolved.
	accountID string

	mu       sync.Mutex
	inflight map[string]*inflightCall
}

// inflightCall is one live compass_rpc call's teardown handle. It is stored in
// the in-flight map by requestId and compared by pointer identity so a call only
// ever deletes/cancels its OWN entry — a re-registered id (same key, new call)
// never has its live entry mis-deleted by a prior call's deferred finish.
type inflightCall struct {
	cancel context.CancelFunc
}

// newBridgeService builds a bridge service that forwards against pump and emits
// response frames through events.
func newBridgeService(pump *bridge.Pump, events eventEmitter) *bridgeService {
	return &bridgeService{
		pump:     pump,
		events:   events,
		inflight: make(map[string]*inflightCall),
	}
}

// AccountID is the bound IPC getter the webview calls to learn the caller
// account id the embedded launch resolved via WhoAmI (DL-111). The JS side
// (compass-ui zone) reads it over Wails IPC to build the native
// ConnectionProvider; the account id is server-derived, never client-supplied.
// It returns the empty string when no identity was resolved (client mode, or a
// shell started against a hand-run daemon), which the JS treats as "not yet
// identified".
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
func (s *bridgeService) register(ctx context.Context, requestID string) (context.Context, *inflightCall) {
	callCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	call := &inflightCall{cancel: cancel}
	s.mu.Lock()
	if prev, ok := s.inflight[requestID]; ok {
		prev.cancel()
	}
	s.inflight[requestID] = call
	s.mu.Unlock()
	return callCtx, call
}

// run forwards the call through the pump synchronously, emitting each ordered
// frame as "compass_rpc:"+requestId, and drops the in-flight entry when the pump
// returns (terminal frame emitted, or silent stop on cancel). CompassRPC runs it
// on its own goroutine; it is called directly (synchronously) only by
// deterministic single-shot tests, where its return means every frame is emitted
// and the entry is cleared.
func (s *bridgeService) run(callCtx context.Context, call *inflightCall, req rpcRequest) {
	defer s.finish(req.RequestID, call)
	eventName := "compass_rpc:" + req.RequestID
	rpc := bridge.Call{
		Path:    req.Path,
		Headers: headerSlice(req.Headers),
		Body:    req.Body,
	}
	s.pump.Do(callCtx, rpc, func(f bridge.Frame) {
		s.events.Emit(eventName, frameToResponse(f))
	})
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
