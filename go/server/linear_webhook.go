//go:build unix

// The Linear webhook ingress: a single shared http.Handler for
// POST /webhooks/linear (symmetric with /webhooks/github, DL-302) that
// type-dispatches the top-level envelope. "AgentSessionEvent" routes to the
// RIG-2717 responder seam (a local SessionEventSink satisfied by
// *linearagent.Dispatcher); "Issue"/"Comment" data-change events route to an
// injected ForgeEventSink. It mirrors github_webhook.go's fail-closed order
// (secret -> HMAC -> parse) and its ack-fast discipline; T7d mounts it and
// supplies the sinks. The data sink is injected-and-nil-for-now (DL-302): a
// Linear-provider-bound notify lane wires the real sink later, so a nil sink
// acks-and-drops rather than mis-routing into the GitHub-coordinate fanout.
//
// Dedup is NOT done here: RIG-2717's design (design.md:167-170) rides the comms
// rail's own idempotency for sessions (the dispatcher's client_request_id) and
// the notify router's idempotency for data, so — unlike the GitHub handler's
// X-GitHub-Delivery LRU — this mount adds no delivery-id LRU.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/RigelBuild/compass/go/internal/linearagent"
)

const (
	// linearWebhookPath is the shared Linear ingress path this handler mounts at
	// — /webhooks/linear, for symmetry with /webhooks/github (DL-302, Matt's
	// 2026-08-30 amendment).
	linearWebhookPath = "/webhooks/linear"

	// linearSignatureHeader carries the HMAC-SHA256 hex of the raw body under
	// the webhook secret (RIG-2717 design §134).
	linearSignatureHeader = "Linear-Signature"

	// linearWebhookSkew bounds how stale a webhookTimestamp may be before the
	// delivery is acked-and-dropped (RIG-2717 §145): the timestamp is inside the
	// signed body, so a replay is already signature-valid; a stale timestamp is
	// dropped with a 200, never a 400.
	linearWebhookSkew = 60 * time.Second

	// Envelope type discriminants (RIG-2717 §134, data arm design.md:660-666).
	linearTypeSession = "AgentSessionEvent"
	linearTypeIssue   = "Issue"
	linearTypeComment = "Comment"
)

// SessionEventSink receives a verified Linear session event for asynchronous
// dispatch. *linearagent.Dispatcher satisfies it via Enqueue (dispatcher.go:145).
// Defined locally so the server can wire a real dispatcher OR pass nil when the
// RIG-2717 responder is not assembled — the handler logs-and-drops in that case.
type SessionEventSink interface {
	// Enqueue offers a verified session event to the dispatcher without
	// blocking; a full queue returns linearagent.ErrQueueFull, which the handler
	// maps to a 500 so Linear retries the delivery.
	Enqueue(ev *linearagent.SessionEvent) error
}

// linearWebhookHandler serves POST /webhooks.
type linearWebhookHandler struct {
	secret      func(ctx context.Context) ([]byte, error)
	dataSink    ForgeEventSink
	sessionSink SessionEventSink
	maxBody     int64
	skew        time.Duration
	now         func() time.Time
	log         *slog.Logger
}

// NewLinearWebhookHandler returns the POST /webhooks handler and the path it
// mounts at. secret lazily resolves the Linear webhook secret (TTL-cached by the
// caller); dataSink receives every accepted data-change event; sessionSink
// receives session events (nil when the responder is unassembled — session
// events are then logged-and-dropped). Mirrors NewGitHubWebhookHandler's
// nil-log default and field init.
func NewLinearWebhookHandler(
	secret func(ctx context.Context) ([]byte, error),
	dataSink ForgeEventSink,
	sessionSink SessionEventSink,
	log *slog.Logger,
) (string, http.Handler) {
	if log == nil {
		log = slog.Default()
	}
	return linearWebhookPath, &linearWebhookHandler{
		secret:      secret,
		dataSink:    dataSink,
		sessionSink: sessionSink,
		maxBody:     githubWebhookMaxBody,
		skew:        linearWebhookSkew,
		now:         time.Now,
		log:         log,
	}
}

// linearEnvelope is the minimal peek this handler routes on: the top-level type
// (Issue/Comment/AgentSessionEvent) and the webhookTimestamp both envelopes
// carry. The full body is re-parsed by the branch parser after routing.
type linearEnvelope struct {
	Type             string `json:"type"`
	WebhookTimestamp int64  `json:"webhookTimestamp"`
}

func (h *linearWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	// Bound the body read before buffering it (oversized-body rejection). A body
	// over the cap trips MaxBytesReader mid-read, so a hostile Content-Length
	// never forces the full allocation.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Fail-closed order: secret -> HMAC -> parse. An attacker who reaches this
	// unauthenticated endpoint must clear the signature before any body parsing.
	secret, err := h.secret(ctx)
	if err != nil {
		h.log.ErrorContext(ctx, "linear webhook secret unavailable", "err", err)
		http.Error(w, "webhook unavailable", http.StatusServiceUnavailable)
		return
	}
	if !linearagent.VerifySignature(secret, body, r.Header.Get(linearSignatureHeader)) {
		// Fail-closed: an unverifiable delivery is a 400, never processed.
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	// Peek just enough to route (type) and freshness-check (timestamp); the
	// branch parser re-parses the full body.
	var env linearEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		h.log.WarnContext(ctx, "linear webhook envelope parse error", "err", err)
		w.WriteHeader(http.StatusOK) // verified-but-unparseable: ack-and-drop.
		return
	}

	// Stale timestamp -> 200-with-drop, NOT 400 (RIG-2717 §145-155): the
	// timestamp is inside the signed body, so a replay is signature-valid; a 400
	// would burn Linear's retry ladder on a legitimate late retry.
	if !linearagent.CheckTimestamp(env.WebhookTimestamp, h.now(), h.skew) {
		h.log.WarnContext(ctx, "linear webhook stale timestamp, dropped",
			"type", env.Type, "webhook_timestamp", env.WebhookTimestamp)
		w.WriteHeader(http.StatusOK)
		return
	}

	switch env.Type {
	case linearTypeSession:
		h.serveSession(ctx, w, body)
	case linearTypeIssue, linearTypeComment:
		h.serveData(ctx, w, body)
	default:
		// An envelope type this mount does not handle (e.g. "Reaction"): ignored.
		w.WriteHeader(http.StatusOK)
	}
}

// serveSession routes a verified AgentSessionEvent to the responder seam. Unlike
// the data branch, Enqueue is itself non-blocking (a bounded try-send) and its
// return maps to the status code, so the status is written AFTER Enqueue: a full
// queue is a 500 (Linear retries), everything else a 200.
func (h *linearWebhookHandler) serveSession(ctx context.Context, w http.ResponseWriter, body []byte) {
	ev, err := linearagent.ParseSessionEvent(body)
	if err != nil {
		h.log.WarnContext(ctx, "linear session event parse error", "err", err)
		w.WriteHeader(http.StatusOK) // verified-but-malformed: ack-and-drop.
		return
	}
	if h.sessionSink == nil {
		// The RIG-2717 responder assembly wires a real dispatcher here; until
		// then session events are acked-and-dropped so Linear does not retry.
		h.log.WarnContext(ctx, "linear session responder not wired, dropping event",
			"action", ev.Action, "session", ev.AgentSession.ID)
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := h.sessionSink.Enqueue(ev); err != nil {
		if errors.Is(err, linearagent.ErrQueueFull) {
			// A full queue is a 500 so Linear retries rather than the event
			// being silently dropped (dispatcher.go:30-33).
			http.Error(w, "queue full", http.StatusInternalServerError)
			return
		}
		h.log.ErrorContext(ctx, "linear session enqueue failed", "err", err)
		http.Error(w, "enqueue failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// serveData routes a verified Issue/Comment data-change event to the shared
// fanout sink. It acks 200 (and flushes) BEFORE the non-blocking Enqueue so the
// ack is never on the sink's latency path (mirrors github_webhook.go:181-192).
func (h *linearWebhookHandler) serveData(ctx context.Context, w http.ResponseWriter, body []byte) {
	ev, ok, perr := linearagent.ParseLinearDataEvent(body)
	if perr != nil {
		h.log.WarnContext(ctx, "linear data event parse error", "err", perr)
		w.WriteHeader(http.StatusOK) // verified-but-malformed: ack-and-drop.
		return
	}
	// Ack fast, enqueue after: the sink must never be on the ack's latency path.
	w.WriteHeader(http.StatusOK)
	if !ok {
		return // ignored action (e.g. Issue remove): counted-and-dropped.
	}
	// Flush the ack onto the wire before handing off: WriteHeader only records
	// the status, so a blocking Enqueue would otherwise delay the client-visible
	// 200 until ServeHTTP returns.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if h.dataSink == nil {
		// dataSink is injected-and-nil-for-now by driver decision (DL-302): the
		// GitHub-bound fanout sink would mis-route Linear events (Linear subs
		// looked up under a GitHub coordinate), so the data branch acks-and-drops
		// until a Linear-provider-bound notify lane injects a real sink here.
		h.log.WarnContext(ctx, "linear data-change routing pending the Linear notify lane, dropping event")
		return
	}
	h.dataSink.Enqueue(ctx, ev)
}
