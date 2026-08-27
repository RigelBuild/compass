//go:build unix

// Unit tests for the GitHub App webhook ingress handler: signature fail-closed,
// oversized-body rejection, delivery-id dedup, and the ack-then-enqueue shape
// (design.md:667-674 — "oversized body rejected by the mount").
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/RigelBuild/compass/go/internal/forge"
)

type recordingSink struct {
	mu     sync.Mutex
	events []forge.ForgeEvent
}

func (s *recordingSink) Enqueue(_ context.Context, ev forge.ForgeEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func ghSign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newTestHandler(t *testing.T, secret []byte) (http.Handler, *recordingSink) {
	t.Helper()
	sink := &recordingSink{}
	_, h := NewGitHubWebhookHandler(
		func(context.Context) ([]byte, error) { return secret, nil },
		sink, nil,
	)
	return h, sink
}

func doPost(h http.Handler, event, delivery, sig string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, githubWebhookPath, strings.NewReader(string(body)))
	if event != "" {
		req.Header.Set(githubEventHeader, event)
	}
	if delivery != "" {
		req.Header.Set(githubDeliveryHeader, delivery)
	}
	if sig != "" {
		req.Header.Set(githubSignatureHeader, sig)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGitHubWebhookHandler_ValidEnqueues(t *testing.T) {
	secret := []byte("shh")
	h, sink := newTestHandler(t, secret)
	body := []byte(`{"action":"opened","issue":{"number":1,"html_url":"u"},"repository":{"full_name":"o/r"}}`)

	rec := doPost(h, "issues", "d1", ghSign(secret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if sink.count() != 1 {
		t.Fatalf("enqueued = %d, want 1", sink.count())
	}
	if got := sink.events[0].DeliveryID; got != "d1" {
		t.Errorf("DeliveryID = %q, want d1", got)
	}
}

func TestGitHubWebhookHandler_BadSignature(t *testing.T) {
	secret := []byte("shh")
	h, sink := newTestHandler(t, secret)
	body := []byte(`{"action":"opened","issue":{"number":1},"repository":{"full_name":"o/r"}}`)

	rec := doPost(h, "issues", "d1", "sha256=deadbeef", body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
	if sink.count() != 0 {
		t.Errorf("enqueued = %d, want 0 (fail-closed)", sink.count())
	}
}

func TestGitHubWebhookHandler_OversizedBody(t *testing.T) {
	secret := []byte("shh")
	sink := &recordingSink{}
	_, base := NewGitHubWebhookHandler(
		func(context.Context) ([]byte, error) { return secret, nil }, sink, nil)
	// Shrink the body cap on the concrete handler so the over-cap path fires
	// without materializing a 25 MiB body; the cap enforcement is identical.
	h := base.(*githubWebhookHandler)
	h.maxBody = 16
	big := []byte(`{"action":"opened","issue":{"number":1},"repository":{"full_name":"o/r"}}`)
	rec := doPost(h, "issues", "d1", ghSign(secret, big), big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, want 413", rec.Code)
	}
	if sink.count() != 0 {
		t.Errorf("enqueued = %d, want 0", sink.count())
	}
}

func TestGitHubWebhookHandler_Dedup(t *testing.T) {
	secret := []byte("shh")
	h, sink := newTestHandler(t, secret)
	body := []byte(`{"action":"opened","issue":{"number":1,"html_url":"u"},"repository":{"full_name":"o/r"}}`)
	sig := ghSign(secret, body)

	first := doPost(h, "issues", "dup", sig, body)
	second := doPost(h, "issues", "dup", sig, body)
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("codes = %d,%d, want 200,200", first.Code, second.Code)
	}
	if sink.count() != 1 {
		t.Errorf("enqueued = %d, want 1 (second is a dedup drop)", sink.count())
	}
}

func TestGitHubWebhookHandler_IgnoredEventAcks(t *testing.T) {
	secret := []byte("shh")
	h, sink := newTestHandler(t, secret)
	body := []byte(`{"ref":"refs/heads/main"}`)

	rec := doPost(h, "push", "d1", ghSign(secret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if sink.count() != 0 {
		t.Errorf("enqueued = %d, want 0 (ignored event)", sink.count())
	}
}

func TestGitHubWebhookHandler_RejectsGET(t *testing.T) {
	h, _ := newTestHandler(t, []byte("shh"))
	req := httptest.NewRequest(http.MethodGet, githubWebhookPath, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405", rec.Code)
	}
}
