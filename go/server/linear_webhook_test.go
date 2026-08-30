//go:build unix

// Unit tests for the shared Linear /webhooks ingress handler: signature
// fail-closed, the type-dispatch (Issue/Comment -> data sink, AgentSessionEvent
// -> session sink), the ErrQueueFull -> 500 retry signal, the 200-with-drop
// stale-timestamp rule, the nil-sessionSink logged-drop, and the ignored-type /
// ignored-action drops.
package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/linearagent"
	"github.com/RigelBuild/compass/go/internal/secrets"
)

// recordingSessionSink records enqueued session events; enqErr (when set) is
// returned by every Enqueue so the ErrQueueFull -> 500 path is exercisable.
type recordingSessionSink struct {
	mu     sync.Mutex
	events []*linearagent.SessionEvent
	enqErr error
}

func (s *recordingSessionSink) Enqueue(ev *linearagent.SessionEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enqErr != nil {
		return s.enqErr
	}
	s.events = append(s.events, ev)
	return nil
}

func (s *recordingSessionSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func linSign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// linHandler builds a handler with a fixed test secret and injectable now, plus
// the two recording sinks. sessionSink may be nil to exercise the logged-drop.
func linHandler(t *testing.T, secret []byte, now time.Time, sessionNil bool) (*linearWebhookHandler, *recordingSink, *recordingSessionSink) {
	t.Helper()
	data := &recordingSink{}
	session := &recordingSessionSink{}
	var sess SessionEventSink = session
	if sessionNil {
		sess = nil
	}
	_, h := NewLinearWebhookHandler(
		func(context.Context) ([]byte, error) { return secret, nil },
		data, sess, nil,
	)
	lh := h.(*linearWebhookHandler)
	lh.now = func() time.Time { return now }
	return lh, data, session
}

func linPost(h http.Handler, sig string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, linearWebhookPath, strings.NewReader(string(body)))
	if sig != "" {
		req.Header.Set(linearSignatureHeader, sig)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// fresh returns a webhookTimestamp (ms epoch) equal to now, so CheckTimestamp
// passes with room to spare.
func freshTS(now time.Time) int64 { return now.UnixMilli() }

func TestLinearWebhookHandler_BadSignature(t *testing.T) {
	secret := []byte("shh")
	now := time.Unix(1_700_000_000, 0)
	h, data, session := linHandler(t, secret, now, false)
	body := fmt.Appendf(nil, `{"type":"Issue","action":"create","webhookTimestamp":%d,"data":{"number":1,"team":{"key":"RIG"},"url":"u"}}`, freshTS(now))

	rec := linPost(h, "deadbeef", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	if data.count() != 0 || session.count() != 0 {
		t.Errorf("enqueued data=%d session=%d, want 0/0 (fail-closed)", data.count(), session.count())
	}
}

func TestLinearWebhookHandler_IssueCreate(t *testing.T) {
	secret := []byte("shh")
	now := time.Unix(1_700_000_000, 0)
	h, data, session := linHandler(t, secret, now, false)
	body := fmt.Appendf(nil, `{"type":"Issue","action":"create","webhookTimestamp":%d,"data":{"number":7,"team":{"key":"RIG"},"url":"https://linear.app/i/7"}}`, freshTS(now))

	rec := linPost(h, linSign(secret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if data.count() != 1 {
		t.Fatalf("data enqueued = %d, want 1", data.count())
	}
	ev := data.events[0]
	if ev.Provider != compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR {
		t.Errorf("Provider = %v, want LINEAR", ev.Provider)
	}
	if ev.Repo != "RIG" || ev.Number != 7 {
		t.Errorf("coordinate = %q/#%d, want RIG/#7", ev.Repo, ev.Number)
	}
	if ev.Change != compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_OPENED {
		t.Errorf("Change = %v, want OPENED", ev.Change)
	}
	if session.count() != 0 {
		t.Errorf("session enqueued = %d, want 0", session.count())
	}
}

func TestLinearWebhookHandler_CommentCreate(t *testing.T) {
	secret := []byte("shh")
	now := time.Unix(1_700_000_000, 0)
	h, data, session := linHandler(t, secret, now, false)
	body := fmt.Appendf(nil, `{"type":"Comment","action":"create","webhookTimestamp":%d,"data":{"id":"c1","body":"hello","user":{"displayName":"Ann"},"issue":{"number":3,"url":"https://linear.app/i/3","team":{"key":"RIG"}}}}`, freshTS(now))

	rec := linPost(h, linSign(secret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if data.count() != 1 {
		t.Fatalf("data enqueued = %d, want 1", data.count())
	}
	if ev := data.events[0]; ev.Change != compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_COMMENT {
		t.Errorf("Change = %v, want COMMENT", ev.Change)
	}
	if session.count() != 0 {
		t.Errorf("session enqueued = %d, want 0", session.count())
	}
}

func TestLinearWebhookHandler_SessionEvent(t *testing.T) {
	secret := []byte("shh")
	now := time.Unix(1_700_000_000, 0)
	h, data, session := linHandler(t, secret, now, false)
	body := fmt.Appendf(nil, `{"type":"AgentSessionEvent","action":"created","webhookTimestamp":%d,"agentSession":{"id":"s1"}}`, freshTS(now))

	rec := linPost(h, linSign(secret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if session.count() != 1 {
		t.Fatalf("session enqueued = %d, want 1", session.count())
	}
	if got := session.events[0].AgentSession.ID; got != "s1" {
		t.Errorf("session id = %q, want s1", got)
	}
	if data.count() != 0 {
		t.Errorf("data enqueued = %d, want 0", data.count())
	}
}

func TestLinearWebhookHandler_SessionQueueFull(t *testing.T) {
	secret := []byte("shh")
	now := time.Unix(1_700_000_000, 0)
	h, data, session := linHandler(t, secret, now, false)
	session.enqErr = linearagent.ErrQueueFull
	body := fmt.Appendf(nil, `{"type":"AgentSessionEvent","action":"created","webhookTimestamp":%d,"agentSession":{"id":"s1"}}`, freshTS(now))

	rec := linPost(h, linSign(secret, body), body)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500 (Linear retries on full queue)", rec.Code)
	}
	if data.count() != 0 {
		t.Errorf("data enqueued = %d, want 0", data.count())
	}
}

func TestLinearWebhookHandler_StaleTimestamp(t *testing.T) {
	secret := []byte("shh")
	now := time.Unix(1_700_000_000, 0)
	h, data, session := linHandler(t, secret, now, false)
	// A timestamp 5 minutes in the past: well beyond the 60s skew.
	stale := now.Add(-5 * time.Minute).UnixMilli()
	body := fmt.Appendf(nil, `{"type":"Issue","action":"create","webhookTimestamp":%d,"data":{"number":7,"team":{"key":"RIG"},"url":"u"}}`, stale)

	rec := linPost(h, linSign(secret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (stale -> 200-with-drop, not 400)", rec.Code)
	}
	if data.count() != 0 || session.count() != 0 {
		t.Errorf("enqueued data=%d session=%d, want 0/0 (stale drop)", data.count(), session.count())
	}
}

func TestLinearWebhookHandler_NilSessionSink(t *testing.T) {
	secret := []byte("shh")
	now := time.Unix(1_700_000_000, 0)
	h, data, _ := linHandler(t, secret, now, true) // sessionSink nil
	body := fmt.Appendf(nil, `{"type":"AgentSessionEvent","action":"created","webhookTimestamp":%d,"agentSession":{"id":"s1"}}`, freshTS(now))

	rec := linPost(h, linSign(secret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (logged-and-dropped)", rec.Code)
	}
	if data.count() != 0 {
		t.Errorf("data enqueued = %d, want 0", data.count())
	}
}

func TestLinearWebhookHandler_IgnoredType(t *testing.T) {
	secret := []byte("shh")
	now := time.Unix(1_700_000_000, 0)
	h, data, session := linHandler(t, secret, now, false)
	body := fmt.Appendf(nil, `{"type":"Reaction","action":"create","webhookTimestamp":%d}`, freshTS(now))

	rec := linPost(h, linSign(secret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (ignored type)", rec.Code)
	}
	if data.count() != 0 || session.count() != 0 {
		t.Errorf("enqueued data=%d session=%d, want 0/0", data.count(), session.count())
	}
}

func TestLinearWebhookHandler_IssueRemoveDrops(t *testing.T) {
	secret := []byte("shh")
	now := time.Unix(1_700_000_000, 0)
	h, data, session := linHandler(t, secret, now, false)
	body := fmt.Appendf(nil, `{"type":"Issue","action":"remove","webhookTimestamp":%d,"data":{"number":7,"team":{"key":"RIG"},"url":"u"}}`, freshTS(now))

	rec := linPost(h, linSign(secret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (remove -> ok=false drop)", rec.Code)
	}
	if data.count() != 0 || session.count() != 0 {
		t.Errorf("enqueued data=%d session=%d, want 0/0 (remove drop)", data.count(), session.count())
	}
}

func TestLinearWebhookHandler_NilDataSink(t *testing.T) {
	secret := []byte("shh")
	now := time.Unix(1_700_000_000, 0)
	session := &recordingSessionSink{}
	// dataSink injected nil (DL-302): the data branch must ack-and-drop, not
	// panic, until a Linear-provider-bound notify lane injects a real sink.
	_, h := NewLinearWebhookHandler(
		func(context.Context) ([]byte, error) { return secret, nil },
		nil, session, nil,
	)
	lh := h.(*linearWebhookHandler)
	lh.now = func() time.Time { return now }
	body := fmt.Appendf(nil, `{"type":"Issue","action":"create","webhookTimestamp":%d,"data":{"number":7,"team":{"key":"RIG"},"url":"u"}}`, freshTS(now))

	rec := linPost(lh, linSign(secret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (nil dataSink -> 200-with-drop)", rec.Code)
	}
	if session.count() != 0 {
		t.Errorf("session enqueued = %d, want 0", session.count())
	}
}

func TestLinearWebhookHandler_MethodNotAllowed(t *testing.T) {
	secret := []byte("shh")
	now := time.Unix(1_700_000_000, 0)
	h, data, session := linHandler(t, secret, now, false)

	req := httptest.NewRequest(http.MethodGet, linearWebhookPath, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405", rec.Code)
	}
	if data.count() != 0 || session.count() != 0 {
		t.Errorf("enqueued data=%d session=%d, want 0/0 (non-POST rejected)", data.count(), session.count())
	}
}

func TestLinearWebhookHandler_BodyTooLarge(t *testing.T) {
	secret := []byte("shh")
	now := time.Unix(1_700_000_000, 0)
	h, data, session := linHandler(t, secret, now, false)
	// A correctly-signed body just over the 1 MiB cap: MaxBytesReader must trip
	// mid-read and 413 BEFORE any parse, so the memory-amplification guard on
	// this unauthenticated endpoint has a regression test.
	body := bytes.Repeat([]byte("a"), (1<<20)+1)

	// No signature needed: MaxBytesReader trips during io.ReadAll, before the
	// HMAC verify is ever reached.
	rec := linPost(h, "", body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, want 413 (body over 1 MiB cap)", rec.Code)
	}
	if data.count() != 0 || session.count() != 0 {
		t.Errorf("enqueued data=%d session=%d, want 0/0 (over-cap rejected)", data.count(), session.count())
	}
}

func TestLinearWebhookHandler_SecretUnavailable(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	data := &recordingSink{}
	session := &recordingSessionSink{}
	// The secret resolver faults: fail-closed with a 503, never parsing the body.
	_, h := NewLinearWebhookHandler(
		func(context.Context) ([]byte, error) { return nil, errors.New("secretspec load failed") },
		data, session, nil,
	)
	lh := h.(*linearWebhookHandler)
	lh.now = func() time.Time { return now }
	body := fmt.Appendf(nil, `{"type":"Issue","action":"create","webhookTimestamp":%d,"data":{"number":7,"team":{"key":"RIG"},"url":"u"}}`, freshTS(now))

	// No signature needed: the secret resolver faults before the HMAC verify.
	rec := linPost(lh, "", body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 (secret resolve fault)", rec.Code)
	}
	if data.count() != 0 || session.count() != 0 {
		t.Errorf("enqueued data=%d session=%d, want 0/0 (fail-closed on secret fault)", data.count(), session.count())
	}
}

func TestLinearWebhookHandler_VerifiedUnparseable(t *testing.T) {
	secret := []byte("shh")
	now := time.Unix(1_700_000_000, 0)
	h, data, session := linHandler(t, secret, now, false)
	// A correctly-signed but non-JSON body: verified-but-unparseable envelope
	// acks 200-and-drops (Linear should not retry a malformed-but-authentic body).
	body := []byte("not json at all")

	rec := linPost(h, linSign(secret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (verified-but-unparseable -> ack-and-drop)", rec.Code)
	}
	if data.count() != 0 || session.count() != 0 {
		t.Errorf("enqueued data=%d session=%d, want 0/0 (unparseable drop)", data.count(), session.count())
	}
}

// TestBuildLinearWebhookWiring_DeliversToInjectedSink proves the RIG-2732 T7
// wiring seam this slice adds: buildLinearWebhookWiring threads the notify lane's
// dataSink all the way to the mounted handler, so a verified Issue event routes
// to a NON-nil sink instead of the prior injected-nil ack-and-drop. The secret
// is resolved via the real newCachedWebhookSecret path off a declared fakeResolver
// secret, exactly as Serve wires it.
func TestBuildLinearWebhookWiring_DeliversToInjectedSink(t *testing.T) {
	ctx := context.Background() // test root
	const secretName = "LINEAR_WEBHOOK_SECRET"
	secret := []byte("shh")
	res := &fakeResolver{resolved: []secrets.ResolvedSecret{{Name: secretName, Value: string(secret)}}}
	cfg := ServeConfig{Forge: ForgeConfig{LinearWebhookSecretName: secretName}}
	sink := &recordingSink{}

	handler, err := buildLinearWebhookWiring(ctx, cfg, res, sink, nil)
	if err != nil {
		t.Fatalf("buildLinearWebhookWiring: %v", err)
	}
	if handler == nil {
		t.Fatal("handler == nil with the webhook secret declared, want a mounted handler")
	}
	// Pin the handler's clock so the fresh timestamp check passes deterministically.
	now := time.Unix(1_700_000_000, 0)
	lh := handler.(*linearWebhookHandler)
	lh.now = func() time.Time { return now }

	body := fmt.Appendf(nil, `{"type":"Issue","action":"create","webhookTimestamp":%d,"data":{"number":7,"team":{"key":"RIG"},"url":"https://linear.app/i/7"}}`, freshTS(now))
	rec := linPost(lh, linSign(secret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if sink.count() != 1 {
		t.Fatalf("data enqueued = %d, want 1 (verified Issue routes to the injected sink)", sink.count())
	}
	if ev := sink.events[0]; ev.Provider != compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR {
		t.Errorf("Provider = %v, want LINEAR", ev.Provider)
	}
}
