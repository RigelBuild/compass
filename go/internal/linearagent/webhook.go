// Package linearagent models Linear's Agent Session webhook payloads and the
// pure verification helpers the responder uses to authenticate and freshness-
// check an inbound webhook. This file (RIG-2717 T1) carries only the envelope
// types plus the signature/timestamp checks; the HTTP handler, the Linear API
// client, and the store live in sibling tasks.
package linearagent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// SessionEvent models Linear's AgentSessionEventWebhookPayload envelope.
// json tags mirror Linear's camelCase payload keys.
type SessionEvent struct {
	Type             string        `json:"type"`
	Action           string        `json:"action"`
	WebhookTimestamp int64         `json:"webhookTimestamp"`
	AgentSession     AgentSession  `json:"agentSession"`
	PromptContext    string        `json:"promptContext"`
	AgentActivity    AgentActivity `json:"agentActivity"`
}

// AgentSession is the session subject of the event.
type AgentSession struct {
	ID               string    `json:"id"`
	Issue            Issue     `json:"issue"`
	Comment          Comment   `json:"comment"`
	PreviousComments []Comment `json:"previousComments"`
	Guidance         string    `json:"guidance"`
}

// Issue is the Linear issue the session is attached to.
type Issue struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
}

// Comment is a Linear comment (the triggering comment or a prior one).
type Comment struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

// AgentActivity carries the activity body on prompt-style events.
type AgentActivity struct {
	Body string `json:"body"`
}

// ParseSessionEvent unmarshals a raw webhook body into a SessionEvent.
func ParseSessionEvent(raw []byte) (*SessionEvent, error) {
	var ev SessionEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, fmt.Errorf("linearagent: parse session event: %w", err)
	}
	return &ev, nil
}

// VerifySignature reports whether headerHex is the HMAC-SHA256 of rawBody under
// secret. It is constant-time and returns false on any hex-decode error.
func VerifySignature(secret []byte, rawBody []byte, headerHex string) bool {
	want, err := hex.DecodeString(headerHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(rawBody)
	return hmac.Equal(want, mac.Sum(nil))
}

// CheckTimestamp reports whether the ms-epoch webhookTimestamp is within skew of
// now (in either direction).
func CheckTimestamp(webhookTimestamp int64, now time.Time, skew time.Duration) bool {
	ts := time.UnixMilli(webhookTimestamp)
	delta := now.Sub(ts)
	if delta < 0 {
		delta = -delta
	}
	return delta <= skew
}
