// The harness-side CANNED model backend (RIG-1787 H3): a tiny host-side HTTP
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
// Grounded against the SDK parser firsthand, at the 18.0.11 pin
// (`packages/compass-agent/package.json`) — @oh-my-pi/pi-ai, and
// @oh-my-pi/pi-utils via pi-ai's dependency edge (openai-http.ts:17), which is
// transitive rather than a declared compass dependency:
//   - The provider POSTs to `baseUrl + /chat/completions`
//     (providers/openai-completions.ts:752-753) with `Accept: text/event-stream`
//     (utils/openai-http.ts:93, in postOpenAIStream).
//   - The response is decoded by readSseJson: each `data:` line is JSON-parsed
//     into a ChatCompletionChunk, and the literal `data: [DONE]` ends the stream
//     (pi-utils src/stream.ts:239-262 — the [DONE] return at :248-249, the
//     JSON.parse at :253).
//   - Each chunk's `choices[0].delta.content` string is appended as assistant
//     text (providers/openai-completions.ts:1216), and `choices[0].finish_reason`
//     maps the stop reason (:1180-1181); "stop" is a clean settle (mapStopReason
//     :2504, its `case "stop"` at :2515-2517). A single text chunk + a terminal
//     finish chunk + [DONE] is the minimal well-formed leg-2 turn (no tool
//     calls).
//
// These coordinates are pinned to a mutable npm dependency, not an in-repo tree:
// re-derive them at an SDK bump rather than trusting them.
package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cannedChatPath is the sole route the stub serves — the path the
// openai-completions provider appends to its configured baseUrl.
const cannedChatPath = "/chat/completions"

// The openai-completions SSE wire values the stub emits, named so the two turn
// branches (text and tool-call) share one source of truth. chunkObject is the
// `object` every chat.completion.chunk carries; finishStop and finishToolCall
// are the two finish_reason values the SDK's mapStopReason keys on ("stop" is a
// clean settle, "tool_calls" maps to stopReason toolUse the agent loop gates
// tool execution on).
const (
	chunkObject    = "chat.completion.chunk"
	finishStop     = "stop"
	finishToolCall = "tool_calls"
)

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

// CannedTurn is one scripted model round-trip the canned backend serves: either
// a pure-text turn (delta.content + finish_reason "stop") or a single-tool-call
// turn (a one-element delta.tool_calls + finish_reason "tool_calls"). Build one
// with CannedText or CannedToolCall — the zero value is not a valid turn. The
// server serves an ordered script of these, one per request (request N serves
// script[N]), so a multi-round agent scenario advances one scripted turn per
// model round-trip.
type CannedTurn struct {
	// isToolCall selects the turn shape: false is a text turn, true a tool-call
	// turn. Distinguishing on a bool (not "text != ''") lets a text turn script
	// an empty reply without collapsing into the tool-call branch.
	isToolCall bool
	// text is the assistant reply a text turn settles on.
	text string
	// toolName / toolArgs script a tool-call turn: the function name and its
	// arguments already serialized as a JSON string (the OpenAI contract — the
	// SDK parser reads function.arguments as a string it JSON-parses in one shot,
	// openai-completions.ts:1208-1220). The call id is assigned deterministically
	// per served turn by the handler, not carried here.
	toolName string
	toolArgs string
}

// CannedText builds a pure-text scripted turn: the assistant settles on reply
// with a clean finish_reason "stop". This is the pre-script single-reply
// behavior, now expressed as one turn.
func CannedText(reply string) CannedTurn {
	return CannedTurn{text: reply}
}

// CannedToolCall builds a single-tool-call scripted turn: the assistant emits
// one tool call naming toolName with argsJSON as its arguments (passed already
// serialized as a JSON string, the OpenAI contract) and a finish_reason
// "tool_calls" (which maps to stopReason toolUse the agent loop gates tool
// execution on, openai-completions.ts:2364-2365). The server assigns a unique
// deterministic call id per served turn.
func CannedToolCall(toolName, argsJSON string) CannedTurn {
	return CannedTurn{isToolCall: true, toolName: toolName, toolArgs: argsJSON}
}

// cannedMarker is one off-script body-marker route the canned backend serves:
// any request whose body contains marker settles on reply as a clean text turn
// WITHOUT advancing the positional script counter. Build one with
// newCannedMarker (via the WithCannedMarkerReply fixture option). It generalizes
// the built-in setupTurnMarker mechanism to a
// caller-supplied list, so a leg can route a turn it does not want drawn off its
// ordered script (the mentioned peer's steer-driven turn and the subscriber's
// deliver-driven turn in leg-4) the same way the root-supervisor Setup turn is
// already routed.
type cannedMarker struct {
	marker string
	reply  string
}

// newCannedMarker builds an off-script body-marker route: a request whose body
// contains marker settles on reply as a clean text turn and does NOT consume a
// positional script slot. It mirrors the built-in setupTurnMarker routing (which
// keeps the root-supervisor Setup turn off every leg's script) but is
// caller-supplied and additive — the Setup marker still routes unconditionally.
// Use it via WithCannedMarkerReply to keep the ordered script drawn only by its
// own scripted turns when a shared-backend turn (e.g. a mention-driven steer or
// deliver) would otherwise race the counter.
func newCannedMarker(marker, reply string) cannedMarker {
	return cannedMarker{marker: marker, reply: reply}
}

// cannedModelServer is a running canned model backend. It owns its listener and
// http.Server; Close stops both. The zero value is not usable — build one with
// startCannedModelServer.
type cannedModelServer struct {
	srv    *http.Server
	ln     net.Listener
	script []CannedTurn
	// markers are the caller-supplied off-script body-marker routes (see
	// cannedMarker / newCannedMarker): a request whose body contains a
	// marker settles on its reply WITHOUT advancing the positional counter,
	// generalizing setupTurnMarker. Checked after the built-in Setup marker,
	// before the positional claim.
	markers  []cannedMarker
	port     int
	closeErr error
	closeMu  sync.Mutex
	closed   bool
	// served counts requests handled so far; request N is served script[N]. It
	// is read+incremented under servedMu so a pathological concurrent hit stays
	// race-free (go test -race clean) — a real agent serializes its round-trips,
	// but the stub guards the counter anyway.
	servedMu sync.Mutex
	served   int
}

// setupTurnMarker is a stable substring of the server's root-supervisor Setup
// thread (go/server/setup_thread.md, embedded as setupThreadBody). The e2e stack
// boots on an empty agent tree, so the server's first-launch seed always creates
// a root supervisor and posts that Setup thread into its home channel — driving
// a REAL first turn on the supervisor that dials THIS shared canned backend,
// concurrently with the test's own agent turn. That supervisor turn is not part
// of any leg's script, and it races the test agent on the positional counter, so
// left unrouted it would consume a scripted slot (or, for an ordered script,
// the WRONG slot). The handler classifies a request as the supervisor's Setup
// turn by this marker and serves it setupReply off a SEPARATE counter, leaving
// each leg's ordered script drawn only by the test agent.
//
// It is a substring, not the whole body, so incidental copy edits below the
// first line don't silently un-route the Setup turn; TestCannedSetupMarker
// guards against drift by asserting it stays a substring of setup_thread.md.
const setupTurnMarker = "you are the root Manager for this Compass workspace"

// setupReply is the clean text turn the canned backend settles the supervisor's
// Setup turn on. Its content is irrelevant to every leg's assertions (which read
// the TEST agent's transcript), so it only needs to settle the turn cleanly.
const setupReply = "canned setup turn settled OK"

// startCannedModelServer binds a listener on bindAddr (host:port; port 0 lets
// the kernel assign a free one) and serves the canned streaming SSE script on
// /chat/completions until Close. script is the ordered per-request turn list:
// request N settles on script[N] (a text or single-tool-call turn); a request
// past the end is a test bug the handler answers with a loud 500. An empty
// script is a construction error — a backend that can never settle a turn.
// bindAddr must be an interface the model client can reach: in the container leg
// it is the host's routable address (pasta forwards the container to it via the
// host-gateway); in the hermetic unit test it is loopback. It returns an error
// rather than panicking (rule://go-no-panic-in-lib) so the caller — a test —
// decides fatality. markers are optional off-script body-marker routes
// (newCannedMarker): a request whose body carries one settles on its reply
// without consuming a positional slot, additive to the built-in Setup marker.
func startCannedModelServer(bindAddr string, script []CannedTurn, markers ...cannedMarker) (*cannedModelServer, error) {
	if len(script) == 0 {
		return nil, errors.New("canned model server requires a non-empty script")
	}
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return nil, fmt.Errorf("canned model server listen on %q: %w", bindAddr, err)
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close() // failed construction; release the listener we just opened
		return nil, fmt.Errorf("canned model server listener has unexpected addr type %T", ln.Addr())
	}
	c := &cannedModelServer{ln: ln, script: script, markers: markers, port: tcpAddr.Port}
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
	// ToolCalls is the streamed tool-call array on a tool-call turn. It is
	// omitempty so a text-only chunk serializes exactly as before (no empty
	// tool_calls array on a text turn — the SDK parser keys on its presence).
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
}

// chatToolCall is one element of delta.tool_calls: the SDK reads index, id,
// type ("function"), and the function name+arguments (openai-completions.ts
// :1208-1220). A single chunk carrying the complete arguments string suffices —
// parseStreamingJsonThrottled parses it in one shot, so no fragmentation.
type chatToolCall struct {
	Index    int                  `json:"index"`
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function chatToolCallFunction `json:"function"`
}

// chatToolCallFunction is the function name and its arguments serialized as a
// JSON string (the OpenAI contract — arguments is a string the SDK JSON-parses,
// not a nested object).
type chatToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// handleChatCompletions serves one scripted turn per request. It rejects a
// non-POST with 405 (the provider only ever POSTs) and writes an HTTP error —
// never panics — on any failure (rule://go-no-panic-in-lib). It claims the next
// script index under servedMu; a request past the end of the script is a test
// bug answered with a loud 500 naming exhaustion (never a hang or a default
// turn). The turn's body is either a text turn (a content chunk + a terminal
// finish_reason "stop") or a single-tool-call turn (a chunk whose
// delta.tool_calls carries one entry + a terminal finish_reason "tool_calls"),
// then the literal `data: [DONE]` sentinel. Each event is flushed immediately so
// the client's first-event watchdog sees bytes without waiting on the handler to
// return.
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

	// Read the request body so the Setup turn can be routed off the script. The
	// body is small (one chat-completions request) and fully buffered before any
	// response byte is written.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("canned model backend read body: %v", err), http.StatusInternalServerError)
		return
	}

	// The root-supervisor Setup turn (see setupTurnMarker) is an out-of-script
	// turn-driver the e2e stack's first-launch seed always fires, racing the
	// test agent on the shared backend. Serve it a fixed clean reply WITHOUT
	// advancing the script counter, so each leg's ordered script is drawn only by
	// the test agent's turns — race-free regardless of which turn dials first.
	if strings.Contains(string(body), setupTurnMarker) {
		c.writeTextTurn(w, flusher, setupReply)
		return
	}

	// Caller-supplied off-script markers (newCannedMarker), checked after the
	// built-in Setup marker and BEFORE the positional claim: a request whose body
	// carries one settles on its reply without advancing the script counter, so a
	// shared-backend turn a leg does not want drawn off its ordered script (a
	// mention-driven steer or deliver) is routed the same way the Setup turn is.
	for _, m := range c.markers {
		if strings.Contains(string(body), m.marker) {
			c.writeTextTurn(w, flusher, m.reply)
			return
		}
	}

	// Claim the next script index under the counter mutex so a pathological
	// concurrent hit stays race-free. An index past the end is a test bug: fail
	// loudly with a 500 naming exhaustion BEFORE writing the 200 stream header,
	// so the client sees the error status rather than an empty stream.
	c.servedMu.Lock()
	idx := c.served
	c.served++
	c.servedMu.Unlock()
	if idx >= len(c.script) {
		http.Error(w, fmt.Sprintf("canned model backend script exhausted: request %d past the end of a %d-turn script (an unscripted turn is a test bug)", idx, len(c.script)), http.StatusInternalServerError)
		return
	}
	turn := c.script[idx]

	const id = "canned-completion"
	if turn.isToolCall {
		// A single tool-call turn: one delta.tool_calls entry with a unique
		// deterministic call id, then a terminal finish_reason "tool_calls" (maps
		// to stopReason toolUse the agent loop gates tool execution on).
		finish := finishToolCall
		callID := fmt.Sprintf("call_%d", idx)
		c.writeTurn(w, flusher, []chatChunk{
			{ID: id, Object: chunkObject, Choices: []chatChoice{{
				Index: 0,
				Delta: chatDelta{Role: "assistant", ToolCalls: []chatToolCall{{
					Index: 0,
					ID:    callID,
					Type:  "function",
					Function: chatToolCallFunction{
						Name:      turn.toolName,
						Arguments: turn.toolArgs,
					},
				}}},
			}}},
			{ID: id, Object: chunkObject, Choices: []chatChoice{{
				Index:        0,
				Delta:        chatDelta{},
				FinishReason: &finish,
			}}},
		})
		return
	}
	c.writeTextTurn(w, flusher, turn.text)
}

// writeTextTurn serves a pure-text turn: a content chunk carrying reply then a
// clean finish_reason "stop", the same shape a scripted CannedText turn emits.
// It is the shared path for both a scripted text turn and the out-of-script
// Setup turn (setupTurnMarker), so both settle identically on the wire.
func (c *cannedModelServer) writeTextTurn(w http.ResponseWriter, flusher http.Flusher, reply string) {
	const id = "canned-completion"
	stop := finishStop
	c.writeTurn(w, flusher, []chatChunk{
		{ID: id, Object: chunkObject, Choices: []chatChoice{{
			Index: 0,
			Delta: chatDelta{Role: "assistant", Content: reply},
		}}},
		{ID: id, Object: chunkObject, Choices: []chatChoice{{
			Index:        0,
			Delta:        chatDelta{},
			FinishReason: &stop,
		}}},
	})
}

// writeTurn writes the 200 SSE header, streams each event as a `data:` frame
// flushed immediately (so the client's first-event watchdog sees bytes without
// waiting on the handler to return), then the terminal `data: [DONE]` sentinel.
// A marshal failure of a fixed in-memory struct is surfaced as a 500 rather than
// swallowed; a mid-stream write error means the client hung up, so nothing
// actionable remains.
func (c *cannedModelServer) writeTurn(w http.ResponseWriter, flusher http.Flusher, events []chatChunk) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	for _, ev := range events {
		payload, err := json.Marshal(ev)
		if err != nil {
			http.Error(w, fmt.Sprintf("canned model backend marshal: %v", err), http.StatusInternalServerError)
			return
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return
		}
		flusher.Flush()
	}
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return
	}
	flusher.Flush()
}
