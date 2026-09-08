// Hermetic (non-podman) unit test for the canned model SSE stub (RIG-1787 H3).
// It is DELIBERATELY UNTAGGED so it runs in the standard `moon run
// compass-go:test` lane with no container — the deterministic red->green proof
// that the stub emits SSE the openai-completions parser accepts, independent of
// the real-stack leg. It drives the stub the way the SDK's transport does: POST
// with `Accept: text/event-stream`, then decode the `data:` frames and assert
// the reassembled turn — a content delta carrying the canned reply, a terminal
// finish_reason "stop", and the [DONE] sentinel that ends the stream.
//
// The stub is the code under test; this needs no podman, so the harness's
// container path never runs here. Bounds every network wait with a context
// deadline (rule://no-retries: no sleeps, no polls).
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestCannedModelServerEmitsParseableSSE pins the stub's wire contract: a POST
// to /chat/completions returns text/event-stream whose frames decode to the
// canned assistant reply, a clean "stop" finish, and the [DONE] sentinel. A bug
// that dropped the content, mis-shaped a chunk (so JSON.parse would throw in the
// SDK), omitted the finish reason, or never sent [DONE] reddens here.
func TestCannedModelServerEmitsParseableSSE(t *testing.T) {
	const reply = "hello from the canned model"
	// Bind the host's routable interface (not 127.0.0.1): the test is both the
	// server and the client, so it dials the same address it binds — a
	// self-reachable interface with no NAT between the two. This mirrors the
	// container leg's requirement (pasta forwards only to the routable host
	// address, never loopback) so the stub is exercised the way production
	// reaches it.
	host, err := hostRoutableAddr()
	if err != nil {
		t.Fatalf("hostRoutableAddr: %v", err)
	}
	srv, err := startCannedModelServer(host+":0", []CannedTurn{CannedText(reply)})
	if err != nil {
		t.Fatalf("startCannedModelServer: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("canned model server Close: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	url := srv.BaseURL(host) + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"model":"x","messages":[]}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() {
		_ = resp.Body.Close() // response-body close in test cleanup; error not actionable
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// Decode the SSE frames the way readSseJson does: each `data:` line is a
	// frame; `data: [DONE]` is the terminal sentinel; every other frame's
	// payload JSON-parses into a chunk.
	var content strings.Builder
	var finish string
	sawDone := false
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("frame %q is not parseable JSON (the SDK parser would throw): %v", data, err)
		}
		if len(chunk.Choices) == 0 {
			t.Fatalf("frame %q has no choices; the openai-completions parser reads choices[0]", data)
		}
		content.WriteString(chunk.Choices[0].Delta.Content)
		if fr := chunk.Choices[0].FinishReason; fr != nil {
			finish = *fr
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading SSE stream: %v", err)
	}

	if got := content.String(); got != reply {
		t.Fatalf("reassembled content = %q, want %q", got, reply)
	}
	if finish != "stop" {
		t.Fatalf("finish_reason = %q, want stop (a clean settle)", finish)
	}
	if !sawDone {
		t.Fatal("stream never sent the [DONE] sentinel; the SDK would wait past settle")
	}
}

// TestCannedModelServerRejectsNonPost pins the fail-safe: the stub only serves
// POST (the sole method the provider uses) and answers anything else with 405
// rather than a panic or a bogus stream (rule://go-no-panic-in-lib).
func TestCannedModelServerRejectsNonPost(t *testing.T) {
	host, err := hostRoutableAddr()
	if err != nil {
		t.Fatalf("hostRoutableAddr: %v", err)
	}
	srv, err := startCannedModelServer(host+":0", []CannedTurn{CannedText("unused")})
	if err != nil {
		t.Fatalf("startCannedModelServer: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("canned model server Close: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.BaseURL(host)+"/chat/completions", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() {
		_ = resp.Body.Close() // response-body close in test cleanup; error not actionable
	}()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", resp.StatusCode)
	}
}

// sseTurn is a reassembled scripted turn decoded from the stub's SSE frames,
// mirroring how the SDK's openai-completions parser accumulates a stream: the
// concatenated content deltas, the tool_calls entry (if any), the final
// finish_reason, and whether the terminal [DONE] sentinel arrived.
type sseTurn struct {
	content   string
	finish    string
	sawDone   bool
	toolCalls []struct {
		id   string
		typ  string
		name string
		args string
	}
	// rawFrames holds each SSE `data:` payload verbatim (minus the [DONE]
	// sentinel), so a test can assert on the exact serialized wire shape — e.g.
	// that a text turn's chunk carries no "tool_calls" key at all (the omitempty
	// discipline), which the decoded struct alone cannot distinguish from an
	// empty/null array.
	rawFrames []string
}

// readCannedTurn POSTs the default (non-marker) request body. See
// readCannedTurnBody for the body-bearing variant used to drive the off-script
// Setup-turn routing.
func readCannedTurn(ctx context.Context, t *testing.T, url string) sseTurn {
	t.Helper()
	return readCannedTurnBody(ctx, t, url, `{"model":"x","messages":[]}`)
}

// readCannedTurnBody POSTs one /chat/completions request with the given body the
// way the SDK transport does (Accept: text/event-stream) and decodes the SSE
// frames into an sseTurn. It bounds the wait with the caller's ctx
// (rule://no-retries: no sleeps/polls).
func readCannedTurnBody(ctx context.Context, t *testing.T, url, body string) sseTurn {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() {
		_ = resp.Body.Close() // response-body close in test; error not actionable
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var turn sseTurn
	var content strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			turn.sawDone = true
			break
		}
		turn.rawFrames = append(turn.rawFrames, data)
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("frame %q is not parseable JSON (the SDK parser would throw): %v", data, err)
		}
		if len(chunk.Choices) == 0 {
			t.Fatalf("frame %q has no choices; the openai-completions parser reads choices[0]", data)
		}
		content.WriteString(chunk.Choices[0].Delta.Content)
		for _, tc := range chunk.Choices[0].Delta.ToolCalls {
			turn.toolCalls = append(turn.toolCalls, struct {
				id   string
				typ  string
				name string
				args string
			}{id: tc.ID, typ: tc.Type, name: tc.Function.Name, args: tc.Function.Arguments})
		}
		if fr := chunk.Choices[0].FinishReason; fr != nil {
			turn.finish = *fr
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading SSE stream: %v", err)
	}
	turn.content = content.String()
	return turn
}

// TestCannedModelServerEmitsToolCallTurn pins the tool-call turn wire contract:
// a scripted CannedToolCall turn emits a single-element delta.tool_calls entry
// (non-empty id, type "function", the tool name, and the args JSON STRING
// verbatim), a terminal finish_reason "tool_calls" (which maps to stopReason
// toolUse the agent loop gates on), and the [DONE] sentinel.
func TestCannedModelServerEmitsToolCallTurn(t *testing.T) {
	const (
		toolName = "my_tool"
		argsJSON = `{"k":"v"}`
	)
	host, err := hostRoutableAddr()
	if err != nil {
		t.Fatalf("hostRoutableAddr: %v", err)
	}
	srv, err := startCannedModelServer(host+":0", []CannedTurn{CannedToolCall(toolName, argsJSON)})
	if err != nil {
		t.Fatalf("startCannedModelServer: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("canned model server Close: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	turn := readCannedTurn(ctx, t, srv.BaseURL(host)+"/chat/completions")
	if len(turn.toolCalls) != 1 {
		t.Fatalf("tool_calls count = %d, want 1", len(turn.toolCalls))
	}
	tc := turn.toolCalls[0]
	if tc.name != toolName {
		t.Fatalf("tool call name = %q, want %q", tc.name, toolName)
	}
	if tc.id == "" {
		t.Fatal("tool call id is empty; the SDK requires a non-empty call id")
	}
	if tc.typ != "function" {
		t.Fatalf("tool call type = %q, want function", tc.typ)
	}
	if tc.args != argsJSON {
		t.Fatalf("tool call arguments = %q, want %q (verbatim JSON string)", tc.args, argsJSON)
	}
	if turn.finish != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", turn.finish)
	}
	if turn.content != "" {
		t.Fatalf("tool-call turn carried assistant content %q, want none (a tool-call turn must not stamp text the SDK would append alongside the call)", turn.content)
	}
	if !turn.sawDone {
		t.Fatal("stream never sent the [DONE] sentinel")
	}
}

// TestCannedModelServerServesMultiTurnScript pins the per-request script: a
// 2-turn script (a tool-call turn then a text turn) serves turn[0] on the first
// POST and turn[1] on the second, so a multi-round agent scenario advances one
// scripted turn per model round-trip.
func TestCannedModelServerServesMultiTurnScript(t *testing.T) {
	const (
		toolName = "spawn"
		argsJSON = `{"a":1}`
		reply    = "all done"
	)
	host, err := hostRoutableAddr()
	if err != nil {
		t.Fatalf("hostRoutableAddr: %v", err)
	}
	srv, err := startCannedModelServer(host+":0", []CannedTurn{
		CannedToolCall(toolName, argsJSON),
		CannedText(reply),
	})
	if err != nil {
		t.Fatalf("startCannedModelServer: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("canned model server Close: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := srv.BaseURL(host) + "/chat/completions"

	first := readCannedTurn(ctx, t, url)
	if len(first.toolCalls) != 1 || first.toolCalls[0].name != toolName {
		t.Fatalf("POST#1 = %+v, want a single %q tool call", first, toolName)
	}
	if first.finish != "tool_calls" {
		t.Fatalf("POST#1 finish_reason = %q, want tool_calls", first.finish)
	}

	second := readCannedTurn(ctx, t, url)
	if len(second.toolCalls) != 0 {
		t.Fatalf("POST#2 carried %d tool calls, want a pure text turn", len(second.toolCalls))
	}
	if second.content != reply {
		t.Fatalf("POST#2 content = %q, want %q", second.content, reply)
	}
	if second.finish != "stop" {
		t.Fatalf("POST#2 finish_reason = %q, want stop", second.finish)
	}
	// Pin the omitempty wire discipline the cannedmodel.go ToolCalls tag claims:
	// a text turn's frames must carry no "tool_calls" key at all — not an empty
	// or null array. The decoded len==0 above cannot tell an absent key from a
	// present-but-empty one, so assert on the raw serialized frame directly. If
	// someone dropped the omitempty tag, a text chunk would emit "tool_calls":null
	// and this reddens.
	for _, frame := range second.rawFrames {
		if strings.Contains(frame, "tool_calls") {
			t.Fatalf("POST#2 (text turn) frame %q contains a tool_calls key; a text turn must omit it entirely (omitempty)", frame)
		}
	}
}

// TestCannedModelServerExhaustionIs500 pins the loud-failure contract: a request
// past the end of the script is a test bug, so the stub answers HTTP 500 with a
// body naming exhaustion rather than hanging or serving a default turn.
func TestCannedModelServerExhaustionIs500(t *testing.T) {
	host, err := hostRoutableAddr()
	if err != nil {
		t.Fatalf("hostRoutableAddr: %v", err)
	}
	srv, err := startCannedModelServer(host+":0", []CannedTurn{CannedText("only turn")})
	if err != nil {
		t.Fatalf("startCannedModelServer: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("canned model server Close: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := srv.BaseURL(host) + "/chat/completions"

	// First POST drains the one scripted turn.
	_ = readCannedTurn(ctx, t, url)

	// Second POST is past the end: expect a loud 500 naming exhaustion.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"model":"x","messages":[]}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() {
		_ = resp.Body.Close() // response-body close in test; error not actionable
	}()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("exhausted POST status = %d, want 500", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(strings.ToLower(string(body)), "exhaust") {
		t.Fatalf("exhaustion body = %q, want it to name exhaustion", string(body))
	}
}

// TestCannedModelServerRejectsEmptyScript pins the construction guard: an empty
// script is a caller bug, so startCannedModelServer returns an error rather than
// serving a backend that can never settle a turn.
func TestCannedModelServerRejectsEmptyScript(t *testing.T) {
	host, err := hostRoutableAddr()
	if err != nil {
		t.Fatalf("hostRoutableAddr: %v", err)
	}
	srv, err := startCannedModelServer(host+":0", []CannedTurn{})
	if err == nil {
		if srv != nil {
			_ = srv.Close() // unexpected success; release the listener
		}
		t.Fatal("startCannedModelServer with an empty script returned nil error, want a construction error")
	}
}

// TestCannedSetupMarker guards the drift-critical coupling behind the canned
// backend's Setup-turn routing: setupTurnMarker MUST stay a substring of the
// server's root-supervisor Setup thread (go/server/setup_thread.md), because the
// handler classifies a request as the out-of-script supervisor Setup turn by
// exactly that substring. If the server edits the Setup copy so the marker no
// longer matches, the supervisor's turn would silently fall through to the
// scripted path again and re-introduce the race this routing fixes — this test
// reddens the moment that coupling breaks, in the hermitic (no-podman) lane.
func TestCannedSetupMarker(t *testing.T) {
	body, err := os.ReadFile("../server/setup_thread.md")
	if err != nil {
		t.Fatalf("read setup_thread.md: %v", err)
	}
	if !strings.Contains(string(body), setupTurnMarker) {
		t.Fatalf("setupTurnMarker %q is no longer a substring of go/server/setup_thread.md; the canned backend can no longer route the supervisor Setup turn off the script (update the marker to a stable phrase of the current Setup copy)", setupTurnMarker)
	}
}

// TestCannedSetupMarkerRoutesOffScript pins the load-bearing behavior behind the
// Setup-turn routing (TestCannedSetupMarker only guards the marker/copy string
// coupling, not the handler). A request whose body contains setupTurnMarker MUST
// draw the fixed setupReply WITHOUT advancing the script counter, so the ordered
// script is drawn only by the test agent's turns — race-free no matter which turn
// dials first. It drives the handler directly: a marker POST returns setupReply
// and does not consume the one scripted turn, which the following normal POST
// then draws at index 0. A regression that advanced the counter on the setup
// turn (re-introducing the exact race this fixes) or stopped matching the marker
// reddens here, in the hermetic (no-podman) lane.
func TestCannedSetupMarkerRoutesOffScript(t *testing.T) {
	const scripted = "the one scripted turn"
	host, err := hostRoutableAddr()
	if err != nil {
		t.Fatalf("hostRoutableAddr: %v", err)
	}
	srv, err := startCannedModelServer(host+":0", []CannedTurn{CannedText(scripted)})
	if err != nil {
		t.Fatalf("startCannedModelServer: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("canned model server Close: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := srv.BaseURL(host) + "/chat/completions"

	// A marker-bearing body is the supervisor Setup turn: it must settle on the
	// fixed setupReply, off the script.
	markerBody := `{"model":"x","messages":[{"role":"user","content":"` + setupTurnMarker + `"}]}`
	setup := readCannedTurnBody(ctx, t, url, markerBody)
	if setup.content != setupReply {
		t.Fatalf("marker POST content = %q, want the off-script setupReply %q", setup.content, setupReply)
	}
	if setup.finish != "stop" {
		t.Fatalf("marker POST finish_reason = %q, want stop", setup.finish)
	}

	// The counter must NOT have advanced: the single scripted turn is still at
	// index 0, so a normal POST draws it (not a 500 exhaustion).
	first := readCannedTurn(ctx, t, url)
	if first.content != scripted {
		t.Fatalf("post-marker normal POST content = %q, want the scripted turn %q (the setup turn advanced the counter — the race this routing fixes is back)", first.content, scripted)
	}
	if first.finish != "stop" {
		t.Fatalf("post-marker normal POST finish_reason = %q, want stop", first.finish)
	}
}

// TestCannedCustomMarkerRoutesOffScript pins the caller-supplied marker route
// (newCannedMarker, threaded in via the WithCannedMarkerReply fixture option and
// the startCannedModelServer markers variadic): a request whose body contains a
// registered marker settles on that marker's reply WITHOUT advancing the script
// counter — the same off-script discipline the built-in Setup marker uses, but
// caller-supplied. It is checked AFTER the built-in Setup marker and BEFORE the
// positional claim, so a leg can route a shared-backend turn it does not want
// drawn off its ordered script (the leg-4 mention-driven steer/deliver turns).
// This is the hermetic (no-podman) counterpart to the podman leg-4 e2e's use of
// the option. A regression that advanced the counter on a custom-marker turn, or
// stopped matching the marker, reddens here.
func TestCannedCustomMarkerRoutesOffScript(t *testing.T) {
	const (
		marker      = "please take a look"
		markerReply = "canned mention turn settled OK"
		scripted    = "the one scripted turn"
	)
	host, err := hostRoutableAddr()
	if err != nil {
		t.Fatalf("hostRoutableAddr: %v", err)
	}
	srv, err := startCannedModelServer(
		host+":0",
		[]CannedTurn{CannedText(scripted)},
		newCannedMarker(marker, markerReply),
	)
	if err != nil {
		t.Fatalf("startCannedModelServer: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("canned model server Close: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := srv.BaseURL(host) + "/chat/completions"

	// A body carrying the registered marker settles on the marker reply, off the
	// script. Non-vacuity: drop the markers slice and this draws the scripted
	// turn (or 500s once exhausted) instead of markerReply.
	markerBody := `{"model":"x","messages":[{"role":"user","content":"` + marker + `"}]}`
	got := readCannedTurnBody(ctx, t, url, markerBody)
	if got.content != markerReply {
		t.Fatalf("marker POST content = %q, want the off-script markerReply %q", got.content, markerReply)
	}
	if got.finish != "stop" {
		t.Fatalf("marker POST finish_reason = %q, want stop", got.finish)
	}

	// The counter must NOT have advanced: the single scripted turn is still at
	// index 0, so a normal POST draws it (not a 500 exhaustion).
	first := readCannedTurn(ctx, t, url)
	if first.content != scripted {
		t.Fatalf("post-marker normal POST content = %q, want the scripted turn %q (the custom marker turn advanced the counter)", first.content, scripted)
	}
	if first.finish != "stop" {
		t.Fatalf("post-marker normal POST finish_reason = %q, want stop", first.finish)
	}
}

// TestCannedMarkerScriptAdvancesAndTerminalRepeats is the LOAD-BEARING teeth for
// the marker-routed multi-turn script (newCannedMarkerScript, RIG-3528 T1), and
// it is the assertion the naive one-turn-per-marker implementation cannot pass.
//
// The hazard: a tool-call turn needs TWO model round-trips to settle (the
// tool-call turn, then the follow-up that settles on text), while a marker route
// matches a substring of the WHOLE request body and returns unconditionally. So
// a marker route that always serves its single turn NEVER TERMINATES — every
// POST re-matches and re-serves the tool call forever, and the agent loop spins.
//
// Three consecutive marker-matching POSTs against a [CannedToolCall, CannedText]
// marker script therefore must yield: POST 1 the tool call, POST 2 the text
// SETTLE (not a repeat of the tool call — this is what reddens on the naive
// implementation), and POST 3 still the settle (the terminal element repeats
// once exhausted, so a re-match is never a 500 and never rewinds to the call).
func TestCannedMarkerScriptAdvancesAndTerminalRepeats(t *testing.T) {
	const (
		marker   = "please post that for me"
		toolName = "comms_post_message"
		argsJSON = `{"channel":"c1","text":"hi"}`
		settle   = "posted it"
	)
	host, err := hostRoutableAddr()
	if err != nil {
		t.Fatalf("hostRoutableAddr: %v", err)
	}
	// The positional script is a single unrelated text turn; the marker script
	// carries the two-turn tool-call sequence. Both coexist on one backend, which
	// is the shape a leg uses (its own ordered script + a marker-routed peer).
	srv, err := startCannedModelServer(
		host+":0",
		[]CannedTurn{CannedText("the one positional turn")},
		newCannedMarkerScript(marker, CannedToolCall(toolName, argsJSON), CannedText(settle)),
	)
	if err != nil {
		t.Fatalf("startCannedModelServer: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("canned model server Close: %v", err)
		}
	})

	// context.Background() as the test root (rule://go-thread-context's test
	// exemption), matching every sibling test in this file.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := srv.BaseURL(host) + "/chat/completions"
	markerBody := `{"model":"x","messages":[{"role":"user","content":"` + marker + `"}]}`

	first := readCannedTurnBody(ctx, t, url, markerBody)
	if len(first.toolCalls) != 1 || first.toolCalls[0].name != toolName {
		t.Fatalf("marker POST#1 = %+v, want a single %q tool call", first, toolName)
	}
	if first.toolCalls[0].args != argsJSON {
		t.Fatalf("marker POST#1 tool args = %q, want %q verbatim", first.toolCalls[0].args, argsJSON)
	}
	if first.finish != "tool_calls" {
		t.Fatalf("marker POST#1 finish_reason = %q, want tool_calls", first.finish)
	}

	// THE assertion the naive implementation fails: the SAME marker body, POSTed
	// again (exactly what the agent's tool-result follow-up looks like on the
	// wire), must draw turns[1] — the text settle — not turns[0] again.
	second := readCannedTurnBody(ctx, t, url, markerBody)
	if len(second.toolCalls) != 0 {
		t.Fatalf("marker POST#2 carried %d tool calls, want the text SETTLE: a marker script that re-serves its tool call never terminates (the agent loop spins forever)", len(second.toolCalls))
	}
	if second.content != settle {
		t.Fatalf("marker POST#2 content = %q, want the settle %q", second.content, settle)
	}
	if second.finish != "stop" {
		t.Fatalf("marker POST#2 finish_reason = %q, want stop", second.finish)
	}

	// The terminal element repeats: a third match stays settled rather than
	// 500ing on exhaustion (the positional path's behaviour) or rewinding to the
	// tool call. A re-steer or an extra loop iteration must not error the agent
	// out.
	third := readCannedTurnBody(ctx, t, url, markerBody)
	if len(third.toolCalls) != 0 {
		t.Fatalf("marker POST#3 carried %d tool calls, want the repeated terminal settle", len(third.toolCalls))
	}
	if third.content != settle {
		t.Fatalf("marker POST#3 content = %q, want the terminal element repeated (%q)", third.content, settle)
	}
	if third.finish != "stop" {
		t.Fatalf("marker POST#3 finish_reason = %q, want stop", third.finish)
	}

	// The marker invariant survives a SCRIPT route as it does a reply route: none
	// of the three matches consumed a positional slot, so the single positional
	// turn is still at index 0 and an unmarked POST draws it (not a 500).
	positional := readCannedTurn(ctx, t, url)
	if positional.content != "the one positional turn" {
		t.Fatalf("post-marker unmarked POST content = %q, want the positional turn (a marker script must not consume a positional slot)", positional.content)
	}
	if positional.finish != "stop" {
		t.Fatalf("post-marker unmarked POST finish_reason = %q, want stop", positional.finish)
	}
}

// TestCannedMarkerScriptRejectsEmptyTurns pins the construction guard: a marker
// route with no turns can never settle a matching request, so it is a caller bug
// startCannedModelServer refuses rather than a 500 discovered mid-leg.
func TestCannedMarkerScriptRejectsEmptyTurns(t *testing.T) {
	host, err := hostRoutableAddr()
	if err != nil {
		t.Fatalf("hostRoutableAddr: %v", err)
	}
	srv, err := startCannedModelServer(
		host+":0",
		[]CannedTurn{CannedText("x")},
		newCannedMarkerScript("no-turns-marker"),
	)
	if err == nil {
		if closeErr := srv.Close(); closeErr != nil {
			t.Errorf("canned model server Close: %v", closeErr)
		}
		t.Fatal("startCannedModelServer accepted a marker with no turns, want a construction error")
	}
	if !strings.Contains(err.Error(), "no-turns-marker") {
		t.Fatalf("error = %v, want it to name the offending marker", err)
	}
}

// TestCannedMarkerScriptsAreIndependentPerMarker pins the PER-MARKER keying of
// the marker-script counter (RIG-3528 T1, review F3). Every other marker test
// registers exactly ONE marker, so markerServed is only ever exercised at i=0
// and a mis-keyed counter is invisible: with `seq := c.markerServed[0]` hard-coded
// (ignoring the route index) the whole canned suite still passes, while marker B's
// FIRST match silently serves B's SETTLE instead of its opening tool call — the
// design record's named hazard, a marker that "silently serves the wrong agent's
// turn and the test still passes".
//
// Two marker scripts on ONE backend is what the feature is FOR (fixture.go's
// WithCannedMarkerScript: "repeat the option to register several marker
// scripts") — one route per marker-driven agent in a multi-actor leg. So: drive
// marker A through its full [toolcall, settle] pair, then assert marker B's FIRST
// match is still B.turns[0], its own tool call. A shared or mis-keyed counter
// reddens here.
func TestCannedMarkerScriptsAreIndependentPerMarker(t *testing.T) {
	const (
		markerA   = "agent-a please post that"
		toolA     = "comms_post_message"
		argsA     = `{"channel":"a1","text":"from a"}`
		settleA   = "a-settle"
		markerB   = "agent-b please post that"
		toolB     = "comms_deliver_message"
		argsB     = `{"channel":"b1","text":"from b"}`
		settleB   = "b-settle"
		positiona = "the one positional turn"
	)
	host, err := hostRoutableAddr()
	if err != nil {
		t.Fatalf("hostRoutableAddr: %v", err)
	}
	srv, err := startCannedModelServer(
		host+":0",
		[]CannedTurn{CannedText(positiona)},
		newCannedMarkerScript(markerA, CannedToolCall(toolA, argsA), CannedText(settleA)),
		newCannedMarkerScript(markerB, CannedToolCall(toolB, argsB), CannedText(settleB)),
	)
	if err != nil {
		t.Fatalf("startCannedModelServer: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("canned model server Close: %v", err)
		}
	})

	// context.Background() as the test root (rule://go-thread-context's test
	// exemption), matching every sibling test in this file.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := srv.BaseURL(host) + "/chat/completions"
	bodyA := `{"model":"x","messages":[{"role":"user","content":"` + markerA + `"}]}`
	bodyB := `{"model":"x","messages":[{"role":"user","content":"` + markerB + `"}]}`

	// Marker A, driven through its whole pair: the tool call, then the settle.
	// This is what advances A's counter to 2 and would leak into B's first match
	// under a shared counter.
	a1 := readCannedTurnBody(ctx, t, url, bodyA)
	if len(a1.toolCalls) != 1 || a1.toolCalls[0].name != toolA {
		t.Fatalf("marker A POST#1 = %+v, want a single %q tool call", a1, toolA)
	}
	a2 := readCannedTurnBody(ctx, t, url, bodyA)
	if a2.content != settleA {
		t.Fatalf("marker A POST#2 content = %q, want A's settle %q", a2.content, settleA)
	}

	// THE assertion: marker B's FIRST match draws B.turns[0]. Under a counter
	// keyed by a constant index instead of the route index, A's two matches have
	// already advanced past B's tool call, so this POST serves b-settle with ZERO
	// tool calls and the agent's opening call never happens.
	b1 := readCannedTurnBody(ctx, t, url, bodyB)
	if len(b1.toolCalls) != 1 {
		t.Fatalf("marker B POST#1 carried %d tool calls, want B's OPENING tool call: marker B's counter must be independent of marker A's (A was driven through its full pair first)", len(b1.toolCalls))
	}
	if b1.toolCalls[0].name != toolB || b1.toolCalls[0].args != argsB {
		t.Fatalf("marker B POST#1 tool call = %q(%q), want B's own %q(%q) — a mis-keyed marker serves the wrong agent's turn", b1.toolCalls[0].name, b1.toolCalls[0].args, toolB, argsB)
	}
	if b1.finish != "tool_calls" {
		t.Fatalf("marker B POST#1 finish_reason = %q, want tool_calls", b1.finish)
	}

	// B's own counter then advances on its OWN matches: its second match is B's
	// settle (not A's), so the two routes neither share a counter nor cross-serve.
	b2 := readCannedTurnBody(ctx, t, url, bodyB)
	if b2.content != settleB {
		t.Fatalf("marker B POST#2 content = %q, want B's settle %q (not A's %q)", b2.content, settleB, settleA)
	}

	// Neither route consumed a positional slot, so the single positional turn is
	// still at index 0 — the invariant that keeps this non-vacuous (a 500 here
	// would mean the marker routing never engaged at all).
	positional := readCannedTurn(ctx, t, url)
	if positional.content != positiona {
		t.Fatalf("post-marker unmarked POST content = %q, want the positional turn %q", positional.content, positiona)
	}
}

// TestCannedMarkerScriptCallIDsStayDistinct pins the ONLY thing claimMarkerTurn's
// returned seq does (RIG-3528 T1, review F6): keep successive marker-served
// tool-call ids DISTINCT. Nothing else reads it, so passing a constant instead
// (`c.writeCannedTurn(w, flusher, turn, 0)`) is otherwise undetectable — and a
// transcript that cannot tell two served calls apart cannot pin which call a
// tool result answers.
//
// The distinctness the counter guarantees is WITHIN one marker route across
// successive matches, which is exactly why the counter keeps climbing past the
// end of the script (claimMarkerTurn). Two DIFFERENT routes both legitimately
// start at seq 0, so cross-marker ids are equal by design and asserting they
// differ would red on correct production. So: a single-turn tool-call marker
// script, whose terminal element repeats, matched twice.
func TestCannedMarkerScriptCallIDsStayDistinct(t *testing.T) {
	const (
		marker   = "please call the tool again"
		toolName = "comms_post_message"
		argsJSON = `{"channel":"c1","text":"hi"}`
	)
	host, err := hostRoutableAddr()
	if err != nil {
		t.Fatalf("hostRoutableAddr: %v", err)
	}
	srv, err := startCannedModelServer(
		host+":0",
		[]CannedTurn{CannedText("the one positional turn")},
		newCannedMarkerScript(marker, CannedToolCall(toolName, argsJSON)),
	)
	if err != nil {
		t.Fatalf("startCannedModelServer: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("canned model server Close: %v", err)
		}
	})

	// context.Background() as the test root (rule://go-thread-context's test
	// exemption), matching every sibling test in this file.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := srv.BaseURL(host) + "/chat/completions"
	body := `{"model":"x","messages":[{"role":"user","content":"` + marker + `"}]}`

	first := readCannedTurnBody(ctx, t, url, body)
	second := readCannedTurnBody(ctx, t, url, body)
	for i, turn := range []sseTurn{first, second} {
		if len(turn.toolCalls) != 1 {
			t.Fatalf("marker POST#%d carried %d tool calls, want 1 (the terminal tool-call turn repeats)", i+1, len(turn.toolCalls))
		}
		if turn.toolCalls[0].id == "" {
			t.Fatalf("marker POST#%d tool-call id is empty", i+1)
		}
	}
	// THE assertion: the second match's id must not repeat the first's. The
	// per-marker counter climbs past the end of the script for exactly this.
	if first.toolCalls[0].id == second.toolCalls[0].id {
		t.Fatalf("both marker-served tool calls carry id %q; successive marker-served call ids must be DISTINCT (claimMarkerTurn's seq), or a transcript cannot tell two served calls apart", first.toolCalls[0].id)
	}
}
