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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cannedChatPath is the sole route the stub serves — the path the
// openai-completions provider appends to its configured baseUrl.
const cannedChatPath = "/chat/completions"

// These are the SSE object and finish_reason values shared by the text and
// tool-call branches.
const (
	chunkObject    = "chat.completion.chunk"
	finishStop     = "stop"
	finishToolCall = "tool_calls"
)

// hostRoutableAddr returns the source address the container's host-gateway
// traffic reaches. A UDP route probe sends no packet; callers choose fatality.
func hostRoutableAddr(ctx context.Context) (string, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", "1.1.1.1:80")
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
	// toolName and toolArgs are the function name and its JSON-string arguments.
	// The handler assigns each call id deterministically.
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

// cannedMarker routes body-matching requests to off-script turns without
// advancing the positional counter. It supports single- and multi-turn scripts.
type cannedMarker struct {
	marker string
	// turns is the ordered marker-route script; its terminal turn repeats after
	// exhaustion so a multi-round tool call can settle on its follow-up request.
	turns []CannedTurn
}

// newCannedMarker builds a text route that settles matching requests off-script
// and leaves positional slots untouched. It repeats one turn; use
// newCannedMarkerScript when the route needs multiple turns.
func newCannedMarker(marker, reply string) cannedMarker {
	return cannedMarker{marker: marker, turns: []CannedTurn{CannedText(reply)}}
}

// newCannedMarkerScript serves an ordered off-script sequence, then repeats its
// terminal turn without advancing the positional counter. Repeating is required
// because a tool-call follow-up matches the same marker; serving turn zero again
// would reissue the call instead of settling it.
func newCannedMarkerScript(marker string, turns ...CannedTurn) cannedMarker {
	return cannedMarker{marker: marker, turns: turns}
}

// cannedModelServer is a running canned model backend. It owns its listener and
// http.Server; Close stops both. The zero value is not usable — build one with
// startCannedModelServer.
type cannedModelServer struct {
	srv    *http.Server
	ln     net.Listener
	script []CannedTurn
	// markers are off-script routes with independent counters; the built-in Setup
	// route is checked first.
	markers  []cannedMarker
	port     int
	closeErr error
	closeMu  sync.Mutex
	closed   bool
	// served counts claimed positional requests. servedMu prevents races if an
	// unexpected concurrent request reaches the handler.
	servedMu sync.Mutex
	served   int
	// markerServed stores each route's request count, independently of served, and
	// clamps exhausted scripts to their terminal turn. markerMu protects the counts.
	markerMu     sync.Mutex
	markerServed []int
}

// setupTurnMarker identifies the root supervisor's first-launch Setup request,
// which would otherwise race test agents for the positional script. Routing it
// separately preserves each leg's turns; a test pins the marker to setup_thread.md
// so content changes cannot silently unroute it.
const setupTurnMarker = "you are the root Manager for this Compass workspace"

// setupReply is the clean text turn the canned backend settles the supervisor's
// Setup turn on. Its content is irrelevant to every leg's assertions (which read
// the TEST agent's transcript), so it only needs to settle the turn cleanly.
const setupReply = "canned setup turn settled OK"

// boardRepromptText is the agent's turn-end board re-prompt (compass-agent
// agent.ts #flushTurnEnd); a test pins it to that source.
const boardRepromptText = "Update your status board with compass_update_board. Do nothing else."

const boardRepromptReply = "canned board re-prompt settled OK"

// lastUserText returns the text of the request's final user message, or "".
func lastUserText(body []byte) string {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	for _, m := range slices.Backward(req.Messages) {
		if m.Role == "user" {
			return messageText(m.Content)
		}
	}
	return ""
}

// messageText reads an OpenAI content field: a string or an array of text parts.
func messageText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// startCannedModelServer serves the ordered script and optional marker routes.
// Marker requests leave positional turns untouched; empty scripts and routes are
// rejected at construction. bindAddr must be reachable by the model client.
func startCannedModelServer(ctx context.Context, bindAddr string, script []CannedTurn, markers ...cannedMarker) (*cannedModelServer, error) {
	if len(script) == 0 {
		return nil, errors.New("canned model server requires a non-empty script")
	}
	for _, m := range markers {
		if len(m.turns) == 0 {
			return nil, fmt.Errorf("canned model server marker %q requires at least one turn", m.marker)
		}
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", bindAddr)
	if err != nil {
		return nil, fmt.Errorf("canned model server listen on %q: %w", bindAddr, err)
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close() // failed construction; release the listener we just opened
		return nil, fmt.Errorf("canned model server listener has unexpected addr type %T", ln.Addr())
	}
	c := &cannedModelServer{
		ln:      ln,
		script:  script,
		markers: markers,
		// One counter per marker, allocated up front so the handler indexes it
		// without ever growing the slice under the lock.
		markerServed: make([]int, len(markers)),
		port:         tcpAddr.Port,
	}
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

// chatChunk is the minimal completion shape the SDK accepts: choices[0] carries
// a delta or the terminal finish_reason.
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

// claimMarkerTurn advances a route's separate counter and returns its sequence.
// Exhausted scripts repeat their terminal turn; the sequence still gives
// tool-call IDs unique values.
func (c *cannedModelServer) claimMarkerTurn(i int) (CannedTurn, int) {
	c.markerMu.Lock()
	seq := c.markerServed[i]
	c.markerServed[i]++
	c.markerMu.Unlock()
	turns := c.markers[i].turns
	if seq >= len(turns) {
		return turns[len(turns)-1], seq
	}
	return turns[seq], seq
}

// handleChatCompletions routes Setup first, then marker scripts, then positional
// turns. Marker scripts clamp to their terminal turn; positional exhaustion is a
// loud 500. Each SSE event is flushed immediately for the client's first-event
// watchdog.
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

	// The root-supervisor Setup turn races test agents on this shared backend. Serve
	// its fixed reply without advancing the positional script counter.
	if strings.Contains(string(body), setupTurnMarker) {
		c.writeTextTurn(w, flusher, setupReply)
		return
	}

	// An agent's turn-end board re-prompt also carries its leg's marker in history;
	// settling it off-script keeps every leg's slots aligned to its triggers. The
	// re-prompt runs alone, so only an exact final message matches.
	if strings.TrimSpace(lastUserText(body)) == boardRepromptText {
		c.writeTextTurn(w, flusher, boardRepromptReply)
		return
	}

	// Marker routes run before the positional script and advance only their own
	// counters. They clamp to the terminal turn because a tool-call follow-up matches
	// the same marker and must settle instead of reissuing the call forever.
	for i, m := range c.markers {
		if strings.Contains(string(body), m.marker) {
			turn, seq := c.claimMarkerTurn(i)
			c.writeCannedTurn(w, flusher, turn, seq)
			return
		}
	}

	// Claim the positional index under servedMu. Exhaustion returns 500 before the
	// SSE header, while the mutex keeps unexpected concurrent hits race-free.
	c.servedMu.Lock()
	idx := c.served
	c.served++
	c.servedMu.Unlock()
	if idx >= len(c.script) {
		http.Error(w, fmt.Sprintf("canned model backend script exhausted: request %d past the end of a %d-turn script (an unscripted turn is a test bug)", idx, len(c.script)), http.StatusInternalServerError)
		return
	}
	turn := c.script[idx]
	c.writeCannedTurn(w, flusher, turn, idx)
}

// writeCannedTurn emits one text or tool-call turn with its finish reason.
// callSeq only gives tool calls unique IDs; both route types share this path.
func (c *cannedModelServer) writeCannedTurn(w http.ResponseWriter, flusher http.Flusher, turn CannedTurn, callSeq int) {
	if !turn.isToolCall {
		c.writeTextTurn(w, flusher, turn.text)
		return
	}
	const id = "canned-completion"
	finish := finishToolCall
	callID := fmt.Sprintf("call_%d", callSeq)
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

// writeTurn flushes each SSE frame immediately, then writes [DONE]. A marshal
// failure aborts the stream; a mid-stream write error means the client left.
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
