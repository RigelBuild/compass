//go:build unix

// The GitHub App webhook ingress: a plain http.Handler for
// POST /webhooks/github, the DL-254 shape (verify signature -> ack 200 fast ->
// enqueue async), with an in-memory delivery-id LRU for dedup and an
// oversized-body guard. T7 mounts it and supplies the sink; this file only
// builds the handler.
package server

import (
	"container/list"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/RigelBuild/compass/go/internal/forge"
)

const (
	// githubWebhookPath is the ingress path T7 mounts this handler at.
	githubWebhookPath = "/webhooks/github"

	// githubWebhookMaxBody bounds a webhook request body (bytes). The 1 MiB
	// ceiling keeps the per-request buffered-body allocation small — these
	// events are single-digit KB — and rejects anything larger before
	// buffering it, closing the memory-amplification window the raw-body HMAC
	// read would otherwise open.
	githubWebhookMaxBody = 1 << 20

	// githubDeliveryLRUSize is the number of recent X-GitHub-Delivery ids the
	// dedup LRU retains. GitHub redelivers on our slow ack or its own retry;
	// the LRU drops a repeat within this window so the sink sees each delivery
	// once.
	githubDeliveryLRUSize = 4096

	githubEventHeader     = "X-Github-Event"
	githubDeliveryHeader  = "X-Github-Delivery"
	githubSignatureHeader = "X-Hub-Signature-256"
)

// ForgeEventSink receives a normalized event the webhook ingress accepted. T4's
// router satisfies it; the handler calls Enqueue after acking 200, so a slow
// downstream never delays the ack GitHub's delivery timeout depends on.
type ForgeEventSink interface {
	// Enqueue MUST NOT block: it hands the event off to the async drain loop
	// and returns immediately, so the ack is never on its latency path.
	Enqueue(ctx context.Context, ev forge.ForgeEvent)
}

// deliveryLRU is a fixed-capacity set of delivery ids with LRU eviction. seen
// reports whether id was already present and records it either way; it is the
// dedup gate. Safe for concurrent use.
type deliveryLRU struct {
	mu    sync.Mutex
	cap   int
	order *list.List
	index map[string]*list.Element
}

func newDeliveryLRU(capacity int) *deliveryLRU {
	return &deliveryLRU{
		cap:   capacity,
		order: list.New(),
		index: make(map[string]*list.Element, capacity),
	}
}

// seen records id and reports whether it was already present. An empty id is
// never deduped (returns false without recording) — an absent delivery header
// must not collapse distinct deliveries onto one empty key.
func (l *deliveryLRU) seen(id string) bool {
	if id == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, ok := l.index[id]; ok {
		l.order.MoveToFront(el)
		return true
	}
	l.index[id] = l.order.PushFront(id)
	if l.order.Len() > l.cap {
		oldest := l.order.Back()
		if oldest != nil {
			l.order.Remove(oldest)
			if key, ok := oldest.Value.(string); ok {
				delete(l.index, key)
			}
		}
	}
	return false
}

// githubWebhookHandler serves POST /webhooks/github.
type githubWebhookHandler struct {
	secret  func(ctx context.Context) ([]byte, error)
	sink    ForgeEventSink
	lru     *deliveryLRU
	maxBody int64
	log     *slog.Logger
}

// NewGitHubWebhookHandler returns the POST /webhooks/github handler and the
// path it mounts at. secret lazily resolves the App webhook secret (a
// server_only secret); sink receives every accepted event. T7 wires both and
// mounts the returned handler.
func NewGitHubWebhookHandler(
	secret func(ctx context.Context) ([]byte, error),
	sink ForgeEventSink,
	log *slog.Logger,
) (string, http.Handler) {
	if log == nil {
		log = slog.Default()
	}
	return githubWebhookPath, &githubWebhookHandler{
		secret:  secret,
		sink:    sink,
		lru:     newDeliveryLRU(githubDeliveryLRUSize),
		maxBody: githubWebhookMaxBody,
		log:     log,
	}
}

func (h *githubWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	// Bound the body read before buffering it (oversized-body rejection). A
	// body over the cap trips MaxBytesReader mid-read, so a hostile
	// Content-Length never forces the full allocation.
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

	secret, err := h.secret(ctx)
	if err != nil {
		h.log.ErrorContext(ctx, "github webhook secret unavailable", "err", err)
		http.Error(w, "webhook unavailable", http.StatusServiceUnavailable)
		return
	}
	if !forge.VerifyGitHubSignature(secret, body, r.Header.Get(githubSignatureHeader)) {
		// Fail-closed: an unverifiable delivery is a 400, never processed.
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	delivery := r.Header.Get(githubDeliveryHeader)
	if h.lru.seen(delivery) {
		// A redelivery of an already-accepted event: ack and drop.
		w.WriteHeader(http.StatusOK)
		return
	}

	event := r.Header.Get(githubEventHeader)
	ev, ok, perr := forge.ParseGitHubEvent(event, body)
	if perr != nil {
		h.log.WarnContext(ctx, "github webhook parse error", "event", event, "delivery", delivery, "err", perr)
		// A malformed payload we verified is still ack'd (retrying it will not
		// help); it is counted-and-dropped.
		w.WriteHeader(http.StatusOK)
		return
	}

	// Ack fast, enqueue after: GitHub's delivery timeout depends on a prompt
	// 200, and the sink must never be on that latency path (DL-254 shape).
	w.WriteHeader(http.StatusOK)
	if !ok {
		return // ignored event/action: counted-and-dropped.
	}
	// Flush the ack onto the wire before handing off: WriteHeader only records
	// the status, so a blocking Enqueue would otherwise delay the client-visible
	// 200 until ServeHTTP returns.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	ev.DeliveryID = delivery
	h.sink.Enqueue(ctx, ev)
}
