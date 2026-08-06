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

	mu       sync.Mutex
	inflight map[string]context.CancelFunc
}

// newBridgeService builds a bridge service that forwards against pump and emits
// response frames through events.
func newBridgeService(pump *bridge.Pump, events eventEmitter) *bridgeService {
	return &bridgeService{
		pump:     pump,
		events:   events,
		inflight: make(map[string]context.CancelFunc),
	}
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
// which of the optional fields is populated; body chunks are standard base64 so
// they ride the JSON event channel as strings (the JS decodeChunk does atob).
type responseFrame struct {
	Kind    string       `json:"kind"`
	Status  int          `json:"status,omitempty"`
	Headers []headerPair `json:"headers,omitempty"`
	Chunk   string       `json:"chunk,omitempty"`
	Message string       `json:"message,omitempty"`
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
	callCtx := s.register(ctx, req.RequestID)
	go s.run(callCtx, req)
}

// CompassRPCCancel cancels the in-flight call for a requestId and drops its
// entry. A canceled pump stops silently (no further frames), matching the pump
// contract. A cancel for an unknown or already-finished id is a no-op.
func (s *bridgeService) CompassRPCCancel(_ context.Context, req cancelRequest) {
	s.mu.Lock()
	cancel, ok := s.inflight[req.RequestID]
	delete(s.inflight, req.RequestID)
	s.mu.Unlock()
	if ok {
		cancel()
	}
}

// register derives the forwarding context for a call and records its cancel
// under requestID, cancelling any prior call already under that id first (so a
// stale forwarder can never keep emitting onto the same event). The context is
// derived from the caller's ctx with WithoutCancel so request-scoped values
// propagate, but detached from the bound-method invocation's lifetime — the
// stream must outlive CompassRPC's return and is torn down only by a terminal
// frame or an explicit compass_rpc_cancel, never by Wails reclaiming the call
// context.
func (s *bridgeService) register(ctx context.Context, requestID string) context.Context {
	//nolint:gosec // G118: cancel is stored in s.inflight and invoked by CompassRPCCancel or finish; gosec's intraprocedural flow cannot see the keyed/deferred call
	callCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.mu.Lock()
	if prev, ok := s.inflight[requestID]; ok {
		prev()
	}
	s.inflight[requestID] = cancel
	s.mu.Unlock()
	return callCtx
}

// run forwards the call through the pump synchronously, emitting each ordered
// frame as "compass_rpc:"+requestId, and drops the in-flight entry when the pump
// returns (terminal frame emitted, or silent stop on cancel). CompassRPC runs it
// on its own goroutine; it is called directly (synchronously) only by
// deterministic single-shot tests, where its return means every frame is emitted
// and the entry is cleared.
func (s *bridgeService) run(callCtx context.Context, req rpcRequest) {
	defer s.finish(req.RequestID)
	eventName := "compass_rpc:" + req.RequestID
	call := bridge.Call{
		Path:    req.Path,
		Headers: headerSlice(req.Headers),
		Body:    req.Body,
	}
	s.pump.Do(callCtx, call, func(f bridge.Frame) {
		s.events.Emit(eventName, frameToResponse(f))
	})
}

// finish drops the in-flight entry for a completed call. It runs after the pump
// returns (terminal frame emitted, or silent stop on cancel); the guard avoids
// racing a compass_rpc_cancel that already deleted and canceled this id.
func (s *bridgeService) finish(requestID string) {
	s.mu.Lock()
	if cancel, ok := s.inflight[requestID]; ok {
		delete(s.inflight, requestID)
		s.mu.Unlock()
		cancel()
		return
	}
	s.mu.Unlock()
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

// headerObjects converts the pump's ordered header pairs into the JS
// {name,value} objects, preserving order.
func headerObjects(pairs [][2]string) []headerPair {
	if len(pairs) == 0 {
		return nil
	}
	out := make([]headerPair, len(pairs))
	for i, p := range pairs {
		out[i] = headerPair{Name: p[0], Value: p[1]}
	}
	return out
}

// frameToResponse maps a pump frame to the JS ResponseFrame payload. The switch
// is exhaustive over the sealed bridge.Frame union (exhaustive/gochecksumtype
// gate); body chunk bytes become a standard-base64 string for the JSON channel.
func frameToResponse(f bridge.Frame) responseFrame {
	switch frame := f.(type) {
	case bridge.HeadFrame:
		return responseFrame{Kind: frameKindHead, Status: frame.Status, Headers: headerObjects(frame.Headers)}
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
