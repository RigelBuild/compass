// Hermetic (non-podman) unit test for the canned model SSE stub (SEA-1787 H3).
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
}

// readCannedTurn POSTs one /chat/completions request the way the SDK transport
// does (Accept: text/event-stream) and decodes the SSE frames into an sseTurn.
// It bounds the wait with the caller's ctx (rule://no-retries: no sleeps/polls).
func readCannedTurn(ctx context.Context, t *testing.T, url string) sseTurn {
	t.Helper()
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
