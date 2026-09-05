//go:build unix

// Package bridge implements a framework-neutral gRPC-Web-over-h2c request pump
// for the Compass native desktop shell.
//
// The pump is a dumb byte forwarder: it POSTs a single [Call] (path + headers +
// raw request body) to the Compass daemon over cleartext-HTTP/2 (h2c) on a Unix
// domain socket and streams the response back as an ordered union of [Frame]s.
// It has NO knowledge of Wails, webkit, or the UI, and it never parses gRPC-Web
// framing — the response body bytes (including gRPC-Web trailers) are streamed
// through verbatim. Base64 encoding of body chunks is a later serialization
// concern (the shell's Channel layer), not the pump's.
//
// Frame ordering is invariant: on success the pump emits exactly one [HeadFrame],
// then zero or more [BodyFrame]s (flushed per read, never buffered whole), then
// exactly one [EndFrame]. On failure it emits exactly one [ErrorFrame] (possibly
// before any head) and nothing after. On context cancellation mid-stream the
// pump stops promptly and emits no further frames — a canceled subscription is a
// torn-down call, not an error to surface.
package bridge

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
)

// Frame is a sealed sum type: the ordered response events the pump emits. The
// unexported marker method seals the union so exhaustiveness is enforceable
// (exhaustive + gochecksumtype); switch over it exhaustively.
type Frame interface {
	isFrame()
}

// HeadFrame carries the response status and ordered header pairs. Exactly one is
// emitted on the success path, before any body.
type HeadFrame struct {
	Status  int
	Headers [][2]string
}

// BodyFrame carries one raw response body chunk, emitted per read as bytes
// arrive so server-streaming RPCs stream incrementally.
type BodyFrame struct {
	Chunk []byte
}

// EndFrame is the success terminus. Exactly one follows a HeadFrame + its bodies.
type EndFrame struct{}

// ErrorFrame is the failure terminus, carrying a non-empty message. It is the
// only frame emitted on a dial/transport/read failure (possibly before any head),
// and nothing follows it.
type ErrorFrame struct {
	Message string
}

func (HeadFrame) isFrame()  {}
func (BodyFrame) isFrame()  {}
func (EndFrame) isFrame()   {}
func (ErrorFrame) isFrame() {}

// Call is a single request the pump forwards. Method is always POST (gRPC-Web),
// so there is no method field. Path is the URL path + query (origin dropped; the
// daemon is same-origin behind the socket). Body is the raw gRPC-Web request bytes.
type Call struct {
	Path    string
	Headers [][2]string
	Body    []byte
}

// Target is a resolved daemon endpoint the pump forwards against. It holds the
// HTTP client wired to reach the daemon and the base URL to build requests from.
//
// Two production wirings build a Target. Embedded mode wires a
// cleartext-HTTP/2-over-UDS target ([NewUnixTarget]) at the private stack's Unix
// socket; native-client mode wires a TLS/network-door target
// ([NewTLSTarget]) at an https base URL. The pump forwarding logic is identical
// for both.
type Target struct {
	client  *http.Client
	baseURL string
}

// NewUnixTarget builds a Target that dials the daemon's cleartext-HTTP/2 (h2c)
// listener on the Unix domain socket at socketPath. The base URL is a
// placeholder host ("http://unix"); the transport's DialContext routes every
// dial to the socket regardless of the URL authority.
func NewUnixTarget(socketPath string) *Target {
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{
		Protocols: p,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Target{
		client:  &http.Client{Transport: tr},
		baseURL: "http://unix",
	}
}

// Pump forwards Calls against a Target and delivers ordered response Frames.
type Pump struct {
	target *Target
}

// NewPump builds a Pump that forwards against target.
func NewPump(target *Target) *Pump {
	return &Pump{target: target}
}

// Do forwards a single Call against the pump's Target and invokes emit for each
// response Frame in order until completion or ctx cancellation. Do is
// synchronous: it returns only after emitting the terminal frame (EndFrame or
// ErrorFrame), or after stopping silently on ctx cancellation. emit is called
// from Do's goroutine, so it need not be safe for concurrent use.
//
// Frame ordering honors the invariant: at most one HeadFrame, then zero or more
// BodyFrames, then exactly one terminal EndFrame on success; OR exactly one
// ErrorFrame on failure with nothing after; OR — on ctx cancellation mid-stream
// — no further frames at all.
func (p *Pump) Do(ctx context.Context, call Call, emit func(Frame)) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.target.baseURL+call.Path, bodyReader(call.Body))
	if err != nil {
		if ctx.Err() == nil {
			emit(ErrorFrame{Message: err.Error()})
		}
		return
	}
	req.Header = headerFrom(call.Headers)

	resp, err := p.target.client.Do(req)
	if err != nil {
		// A canceled context is a torn-down call, not an error to surface.
		if ctx.Err() != nil {
			return
		}
		emit(ErrorFrame{Message: err.Error()})
		return
	}
	// Close is drain-side cleanup; a read-side error is already surfaced as an
	// ErrorFrame, so the close error is not separately actionable here.
	defer func() { _ = resp.Body.Close() }()

	emit(HeadFrame{Status: resp.StatusCode, Headers: headerPairs(resp.Header)})

	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			// Copy: buf is reused across reads, so the chunk must own its bytes.
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			emit(BodyFrame{Chunk: chunk})
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				emit(EndFrame{})
				return
			}
			// A canceled context tears the stream down silently.
			if ctx.Err() != nil {
				return
			}
			emit(ErrorFrame{Message: readErr.Error()})
			return
		}
	}
}

// bodyReader returns an io.Reader over body, or nil for an empty body so the
// request carries no entity when there is nothing to send.
func bodyReader(body []byte) io.Reader {
	if len(body) == 0 {
		return nil
	}
	return &sliceReader{b: body}
}

// sliceReader is a minimal io.Reader over a byte slice (avoids pulling in bytes
// solely for a reader; keeps the request body seekable-free and single-use).
type sliceReader struct {
	b   []byte
	off int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}

// headerFrom builds an http.Header from ordered name/value pairs, preserving
// multiple values for a repeated name in order.
func headerFrom(pairs [][2]string) http.Header {
	h := make(http.Header, len(pairs))
	for _, kv := range pairs {
		h.Add(kv[0], kv[1])
	}
	return h
}

// headerPairs flattens an http.Header into ordered name/value pairs. Header
// names are sorted for determinism; values keep their per-name order.
func headerPairs(h http.Header) [][2]string {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sortStrings(names)
	pairs := make([][2]string, 0, len(h))
	for _, name := range names {
		for _, v := range h[name] {
			pairs = append(pairs, [2]string{name, v})
		}
	}
	return pairs
}

// sortStrings sorts s in place (insertion sort; header sets are tiny, and this
// avoids a sort import for a hot-path-irrelevant helper).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
