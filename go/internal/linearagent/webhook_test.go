package linearagent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func signHex(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	secret := []byte("linear-webhook-secret")
	body := []byte(`{"type":"AgentSessionEvent","action":"created"}`)
	good := signHex(secret, body)

	if !VerifySignature(secret, body, good) {
		t.Fatal("valid signature rejected")
	}

	// Tampered body → false.
	if VerifySignature(secret, []byte(`{"type":"AgentSessionEvent","action":"prompted"}`), good) {
		t.Fatal("tampered body accepted")
	}

	// Tampered signature (flip last hex nibble) → false.
	tampered := []byte(good)
	if tampered[len(tampered)-1] == '0' {
		tampered[len(tampered)-1] = '1'
	} else {
		tampered[len(tampered)-1] = '0'
	}
	if VerifySignature(secret, body, string(tampered)) {
		t.Fatal("tampered signature accepted")
	}

	// Wrong secret → false.
	if VerifySignature([]byte("other-secret"), body, good) {
		t.Fatal("wrong secret accepted")
	}

	// Missing header → false.
	if VerifySignature(secret, body, "") {
		t.Fatal("empty header accepted")
	}

	// Non-hex / short header → false (decode error).
	if VerifySignature(secret, body, "zzzz") {
		t.Fatal("non-hex header accepted")
	}
	if VerifySignature(secret, body, "abcd") {
		t.Fatal("short header accepted")
	}
}

func TestCheckTimestamp(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	skew := 5 * time.Minute

	fresh := now.UnixMilli()
	if !CheckTimestamp(fresh, now, skew) {
		t.Fatal("current timestamp rejected")
	}

	// Within skew, past.
	if !CheckTimestamp(now.Add(-4*time.Minute).UnixMilli(), now, skew) {
		t.Fatal("timestamp within skew (past) rejected")
	}
	// Within skew, future (clock drift).
	if !CheckTimestamp(now.Add(4*time.Minute).UnixMilli(), now, skew) {
		t.Fatal("timestamp within skew (future) rejected")
	}

	// Stale beyond skew.
	if CheckTimestamp(now.Add(-6*time.Minute).UnixMilli(), now, skew) {
		t.Fatal("stale timestamp accepted")
	}
	// Future beyond skew.
	if CheckTimestamp(now.Add(6*time.Minute).UnixMilli(), now, skew) {
		t.Fatal("far-future timestamp accepted")
	}
}

func TestParseSessionEvent(t *testing.T) {
	created := []byte(`{
		"type": "AgentSessionEvent",
		"action": "created",
		"webhookTimestamp": 1756123200000,
		"agentSession": {
			"id": "sess_123",
			"issue": {"id": "iss_abc", "identifier": "RIG-2717"},
			"comment": {"id": "cmt_1", "body": "please look at this"},
			"previousComments": [],
			"guidance": "be concise"
		},
		"promptContext": "initial"
	}`)

	ev, err := ParseSessionEvent(created)
	if err != nil {
		t.Fatalf("parse created: %v", err)
	}
	if ev.Type != "AgentSessionEvent" || ev.Action != "created" {
		t.Fatalf("envelope mismatch: %+v", ev)
	}
	if ev.WebhookTimestamp != 1756123200000 {
		t.Fatalf("webhookTimestamp mismatch: %d", ev.WebhookTimestamp)
	}
	if ev.AgentSession.ID != "sess_123" {
		t.Fatalf("agentSession.id mismatch: %q", ev.AgentSession.ID)
	}
	if ev.AgentSession.Issue.Identifier != "RIG-2717" {
		t.Fatalf("issue.identifier mismatch: %q", ev.AgentSession.Issue.Identifier)
	}
	if ev.AgentSession.Comment.Body != "please look at this" {
		t.Fatalf("comment.body mismatch: %q", ev.AgentSession.Comment.Body)
	}
	if ev.AgentSession.Guidance != "be concise" {
		t.Fatalf("guidance mismatch: %q", ev.AgentSession.Guidance)
	}

	prompted := []byte(`{
		"type": "AgentSessionEvent",
		"action": "prompted",
		"webhookTimestamp": 1756123260000,
		"agentSession": {
			"id": "sess_123",
			"issue": {"id": "iss_abc", "identifier": "RIG-2717"},
			"comment": {"id": "cmt_2", "body": "follow up"},
			"previousComments": [{"id": "cmt_1", "body": "please look at this"}],
			"guidance": ""
		},
		"promptContext": "follow-up",
		"agentActivity": {"body": "user prompt text"}
	}`)

	ev2, err := ParseSessionEvent(prompted)
	if err != nil {
		t.Fatalf("parse prompted: %v", err)
	}
	if ev2.Action != "prompted" {
		t.Fatalf("prompted action mismatch: %q", ev2.Action)
	}
	if ev2.AgentActivity.Body != "user prompt text" {
		t.Fatalf("agentActivity.body mismatch: %q", ev2.AgentActivity.Body)
	}
	if len(ev2.AgentSession.PreviousComments) != 1 ||
		ev2.AgentSession.PreviousComments[0].ID != "cmt_1" {
		t.Fatalf("previousComments mismatch: %+v", ev2.AgentSession.PreviousComments)
	}

	if _, err := ParseSessionEvent([]byte("{not json")); err == nil {
		t.Fatal("expected parse error on malformed json")
	}
}
