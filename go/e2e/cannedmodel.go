// The harness-side CANNED model backend (SEA-1787 H3): a tiny host-side HTTP
// server that speaks the openai-completions streaming SSE contract the agent's
// SDK provider dials, so a leg-2 turn settles on a deterministic scripted reply
// with ZERO live-model egress. It is the model backend the custom
// `openai-completions` models.yml provider's baseUrl points at.
//
// This file is DELIBERATELY UNTAGGED (no `//go:build podman`): the stub is pure
// net/http with no container dependency, so it compiles in the hermetic
// (non-podman) unit lane where cannedmodel_test.go exercises it, AND is consumed
// by the podman-tagged fixture. A Go package freely mixes tagged and untagged
// files; keeping the stub untagged is what lets its red->green unit test run
// under `moon run compass-go:test` without podman.
//
// Grounded against the SDK parser firsthand (forks/oh-my-pi):
//   - The provider POSTs to `baseUrl + /chat/completions` with
//     `Accept: text/event-stream` (ai/src/utils/openai-http.ts:64-90).
//   - The response is decoded by readSseJson: each `data:` line is JSON-parsed
//     into a ChatCompletionChunk, and the literal `data: [DONE]` ends the stream
//     (utils/src/stream.ts:251-273).
//   - Each chunk's `choices[0].delta.content` string is appended as assistant
//     text, and `choices[0].finish_reason` maps the stop reason; "stop" is a
//     clean settle (providers/openai-completions.ts:1068-1091, mapStopReason
//     :2337-2345). A single text chunk + a terminal finish chunk + [DONE] is the
//     minimal well-formed leg-2 turn (no tool calls).
package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// cannedChatPath is the sole route the stub serves — the path the
// openai-completions provider appends to its configured baseUrl.
const cannedChatPath = "/chat/completions"

// hostRoutableAddr returns the host's default-route source address — the
// interface pasta forwards a container's host-gateway traffic to. A stub bound
// to loopback (127.0.0.1) is unreachable from the container; it must bind THIS
// address. Discovered by opening a UDP socket toward a public address (no
// packet is sent) and reading the kernel-selected local IP, the same trick
// `ip route get` performs. Returns an error rather than panicking so the caller
// (a test) decides fatality.
func hostRoutableAddr() (string, error) {
	conn, err := net.Dial("udp", "1.1.1.1:80")
	if err != nil {
		return "", fmt.Errorf("resolving host routable address: %w", err)
	}
	defer func() {
		_ = conn.Close() // no I/O performed on this probe socket; close error is not actionable
	}()
	udpAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "", fmt.Errorf("resolving host routable address: unexpected local addr type %T", conn.LocalAddr())
	}
	return udpAddr.IP.String(), nil
}

// cannedModelServer is a running canned model backend. It owns its listener and
// http.Server; Close stops both. The zero value is not usable — build one with
// startCannedModelServer.
type cannedModelServer struct {
	srv      *http.Server
	ln       net.Listener
	reply    string
	port     int
	closeErr error
	closeMu  sync.Mutex
	closed   bool
}

// startCannedModelServer binds a listener on bindAddr (host:port; port 0 lets
// the kernel assign a free one) and serves the canned streaming SSE turn on
// /chat/completions until Close. reply is the exact assistant text every turn
// settles on. bindAddr must be an interface the model client can reach: in the
// container leg it is the host's routable address (pasta forwards the container
// to it via the host-gateway); in the hermetic unit test it is loopback. It
// returns an error rather than panicking (rule://go-no-panic-in-lib) so the
// caller — a test — decides fatality.
func startCannedModelServer(bindAddr, reply string) (*cannedModelServer, error) {
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return nil, fmt.Errorf("canned model server listen on %q: %w", bindAddr, err)
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close() // failed construction; release the listener we just opened
		return nil, fmt.Errorf("canned model server listener has unexpected addr type %T", ln.Addr())
	}
	c := &cannedModelServer{ln: ln, reply: reply, port: tcpAddr.Port}
	mux := http.NewServeMux()
	mux.HandleFunc(cannedChatPath, c.handleChatCompletions)
	// ReadHeaderTimeout bounds a slow-header client (gosec G112); the canned
	// backend only ever serves the local test client, so a generous bound is
	// ample and never clips a legitimate request.
	c.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second}
	// Serve in the background; a post-Close Serve returns ErrServerClosed, which
	// is the expected clean-shutdown outcome, not a fault.
	go func() {
		if serveErr := c.srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			c.closeMu.Lock()
			c.closeErr = errors.Join(c.closeErr, fmt.Errorf("canned model server serve: %w", serveErr))
			c.closeMu.Unlock()
		}
	}()
	return c, nil
}

// Port is the kernel-assigned TCP port the stub listens on — the port the
// fixture stitches into the provider baseUrl (dialed through the host-gateway,
// not this bind address). Captured at construction from the bound listener.
func (c *cannedModelServer) Port() int {
	return c.port
}

// BaseURL is the openai-completions provider baseUrl for a client that reaches
// the stub at dialHost — i.e. `http://<dialHost>:<port>`. The provider appends
// /chat/completions to it. dialHost is the address the CLIENT uses, which
// differs from the stub's bind address whenever a NAT sits between them (the
// container leg dials the host-gateway alias; the hermetic test dials the same
// address it bound).
func (c *cannedModelServer) BaseURL(dialHost string) string {
	return "http://" + net.JoinHostPort(dialHost, strconv.Itoa(c.port))
}

// Close stops the server and its listener. It is idempotent and safe to call
// from a t.Cleanup even after a t.Fatal. It returns any error the background
// Serve recorded (other than the clean ErrServerClosed) joined with the
// shutdown error.
func (c *cannedModelServer) Close() error {
	c.closeMu.Lock()
	if c.closed {
		err := c.closeErr
		c.closeMu.Unlock()
		return err
	}
	c.closed = true
	c.closeMu.Unlock()
	closeErr := c.srv.Close()
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if closeErr != nil {
		c.closeErr = errors.Join(c.closeErr, fmt.Errorf("canned model server close: %w", closeErr))
	}
	return c.closeErr
}

// chatChunk is the minimal ChatCompletionChunk the SDK's openai-completions
// parser accepts: an object with a choices array whose first entry carries a
// delta (with the streamed content) and, on the terminal chunk, a
// finish_reason. Fields the parser tolerates as absent are omitempty so each
// emitted chunk is exactly one concern (a content delta OR the finish), matching
// how real hosts frame a stream.
type chatChunk struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Choices []chatChoice `json:"choices"`
}

type chatChoice struct {
	Index        int       `json:"index"`
	Delta        chatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason,omitempty"`
}

type chatDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// handleChatCompletions serves the deterministic streaming turn. It rejects a
// non-POST with 405 (the provider only ever POSTs) and writes an HTTP error —
// never panics — on any failure (rule://go-no-panic-in-lib). The body is three
// SSE events: a content chunk carrying the whole canned reply, a terminal chunk
// with finish_reason "stop", and the literal `data: [DONE]` sentinel. Each event
// is flushed immediately so the client's first-event watchdog sees bytes without
// waiting on the handler to return.
func (c *cannedModelServer) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "canned model backend only serves POST", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "canned model backend requires a flushable ResponseWriter", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	const id = "canned-completion"
	stop := "stop"
	events := []chatChunk{
		{ID: id, Object: "chat.completion.chunk", Choices: []chatChoice{{
			Index: 0,
			Delta: chatDelta{Role: "assistant", Content: c.reply},
		}}},
		{ID: id, Object: "chat.completion.chunk", Choices: []chatChoice{{
			Index:        0,
			Delta:        chatDelta{},
			FinishReason: &stop,
		}}},
	}
	for _, ev := range events {
		payload, err := json.Marshal(ev)
		if err != nil {
			// Marshaling a fixed in-memory struct cannot realistically fail;
			// surface it rather than swallow it, then stop the stream.
			http.Error(w, fmt.Sprintf("canned model backend marshal: %v", err), http.StatusInternalServerError)
			return
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			// The client hung up mid-stream; nothing actionable remains.
			return
		}
		flusher.Flush()
	}
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return
	}
	flusher.Flush()
}
