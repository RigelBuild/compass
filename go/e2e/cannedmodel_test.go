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
	srv, err := startCannedModelServer(host+":0", reply)
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
	srv, err := startCannedModelServer(host+":0", "unused")
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
