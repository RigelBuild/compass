//go:build unix

package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/RigelBuild/compass/go/internal/linearagent"
	"github.com/RigelBuild/compass/go/internal/store"
)

// linearSessionLinkPath is the stable link the responder hands Linear; the
// handler resolves its target at click time, so the stored URL never goes stale.
const linearSessionLinkPath = "/l/session/"

type linearAgentSessionReader interface {
	LinearAgentSession(ctx context.Context, linearSessionID string) (store.LinearAgentSessionRow, error)
}

// linearSessionLinkHandler 302s a session link to the channel the ownership walk
// picks now, ignoring the row's created-time channel. It is read-only.
type linearSessionLinkHandler struct {
	sessions linearAgentSessionReader
	resolve  linearagent.ResolveFunc
	base     string
	log      *slog.Logger
}

func newLinearSessionLinkHandler(sessions linearAgentSessionReader, resolve linearagent.ResolveFunc, base string, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return &linearSessionLinkHandler{sessions: sessions, resolve: resolve, base: base, log: log}
}

func (h *linearSessionLinkHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, linearSessionLinkPath)
	if !strings.HasPrefix(r.URL.Path, linearSessionLinkPath) || id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}

	row, err := h.sessions.LinearAgentSession(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.log.Error("linear session link lookup failed", "error", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	ev := &linearagent.SessionEvent{AgentSession: linearagent.AgentSession{
		ID: id,
		Issue: linearagent.Issue{
			ID:         row.LinearIssueID,
			Identifier: row.LinearIssueIdentifier,
		},
	}}
	_, homeChannel, err := h.resolve(r.Context(), ev)
	if err != nil {
		status := http.StatusInternalServerError
		if isLinearRoutingNotSeeded(err) {
			status = http.StatusServiceUnavailable
		}
		h.log.Error("linear session link resolution failed", "error", err)
		http.Error(w, http.StatusText(status), status)
		return
	}

	http.Redirect(w, r, deepLinkFor(h.base, homeChannel), http.StatusFound)
}

func isLinearRoutingNotSeeded(err error) bool {
	return errors.Is(err, errRoutingSupervisor) || errors.Is(err, errRoutingChannel)
}
