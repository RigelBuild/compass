//go:build unix

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/linearagent"
	"github.com/RigelBuild/compass/go/internal/store"
)

type linearSessionLookupFunc func(context.Context, string) (store.LinearAgentSessionRow, error)

func (f linearSessionLookupFunc) LinearAgentSession(ctx context.Context, id string) (store.LinearAgentSessionRow, error) {
	return f(ctx, id)
}

func TestLinearSessionLinkRedirectsToResolvedHome(t *testing.T) {
	const (
		sessionID = "session-123"
		issueID   = "linear-issue-123"
		issueKey  = "RIG-123"
	)
	var resolvedEvent *linearagent.SessionEvent
	handler := newLinearSessionLinkHandler(linearSessionLookupFunc(func(_ context.Context, id string) (store.LinearAgentSessionRow, error) {
		if id != sessionID {
			t.Fatalf("lookup id = %q, want %q", id, sessionID)
		}
		return store.LinearAgentSessionRow{
			LinearIssueID:         issueID,
			LinearIssueIdentifier: issueKey,
			ChannelID:             "stale-created-channel",
			ManagerAccountID:      "stale-created-manager",
		}, nil
	}), func(_ context.Context, ev *linearagent.SessionEvent) (store.AccountID, string, error) {
		resolvedEvent = ev
		return "current-manager", "current-home-channel", nil
	}, "https://compass.example.com/", nil)

	rec := serveLinearSessionLink(t, handler, http.MethodGet, sessionID)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusFound, rec.Body.String())
	}
	if got, want := rec.Header().Get("Location"), "https://compass.example.com/#/channel/current-home-channel"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if resolvedEvent == nil || resolvedEvent.AgentSession.ID != sessionID ||
		resolvedEvent.AgentSession.Issue.ID != issueID || resolvedEvent.AgentSession.Issue.Identifier != issueKey {
		t.Fatalf("resolved event = %+v, want session %q and issue %q/%q", resolvedEvent, sessionID, issueID, issueKey)
	}
}

type fakeLinkOwnership map[string]store.AccountID

func (f fakeLinkOwnership) AuthoredArtifactByCoordinate(_ context.Context, _ store.ForgeProvider, _, repo string, _ store.ForgeArtifactKind, number uint64) (store.AuthoredArtifact, error) {
	agent, ok := f[fmt.Sprintf("%s-%d", repo, number)]
	if !ok {
		return store.AuthoredArtifact{}, store.ErrNotFound
	}
	return store.AuthoredArtifact{AgentAccountID: agent}, nil
}

type fakeLinkRouting struct{}

func (fakeLinkRouting) OwningManager(_ context.Context, agent store.AccountID) (store.AccountID, string, error) {
	return "mgr", "home-of-" + string(agent), nil
}

func (fakeLinkRouting) RoutingTarget(context.Context) (store.AccountID, string, error) {
	return "root-supervisor", "routing-channel", nil
}

// TestLinearSessionLinkResolvesThroughOwnership drives the real Resolver: only a
// recorded owner for the row's identifier reaches a home channel.
func TestLinearSessionLinkResolvesThroughOwnership(t *testing.T) {
	resolver := linearagent.NewResolver(fakeLinkOwnership{"RIG-7": "peer-7"}, fakeLinkRouting{}, "linear.app", fakeLinkRouting{})
	tests := []struct {
		name, identifier, want string
	}{
		{name: "routed issue", identifier: "RIG-7", want: "home-of-peer-7"},
		{name: "unrouted issue", identifier: "RIG-8", want: "routing-channel"},
		{name: "no identifier", identifier: "", want: "routing-channel"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := newLinearSessionLinkHandler(linearSessionLookupFunc(func(context.Context, string) (store.LinearAgentSessionRow, error) {
				return store.LinearAgentSessionRow{LinearIssueID: "uuid", LinearIssueIdentifier: tc.identifier, ChannelID: "stale"}, nil
			}), resolver.ResolveResponder, "https://compass.example.com", nil)

			rec := serveLinearSessionLink(t, handler, http.MethodGet, "sess")
			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
			}
			if got, want := rec.Header().Get("Location"), "https://compass.example.com/#/channel/"+tc.want; got != want {
				t.Errorf("Location = %q, want %q", got, want)
			}
		})
	}
}

func TestLinearSessionLinkNotFound(t *testing.T) {
	tests := []struct {
		name string
		id   string
		err  error
	}{
		{name: "unknown session", id: "unknown-session", err: store.ErrNotFound},
		{name: "empty session id"},
		{name: "session id contains slash", id: "session/child"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := newLinearSessionLinkHandler(linearSessionLookupFunc(func(context.Context, string) (store.LinearAgentSessionRow, error) {
				return store.LinearAgentSessionRow{}, tc.err
			}), func(context.Context, *linearagent.SessionEvent) (store.AccountID, string, error) {
				t.Fatal("resolve called for an invalid session id")
				return "", "", nil
			}, "https://compass.example.com", nil)

			rec := serveLinearSessionLink(t, handler, http.MethodGet, tc.id)
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestLinearSessionLinkRejectsUnsupportedMethod(t *testing.T) {
	handler := newLinearSessionLinkHandler(linearSessionLookupFunc(func(context.Context, string) (store.LinearAgentSessionRow, error) {
		t.Fatal("lookup called for unsupported method")
		return store.LinearAgentSessionRow{}, nil
	}), nil, "https://compass.example.com", nil)

	rec := serveLinearSessionLink(t, handler, http.MethodPost, "session-123")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if got, want := rec.Header().Get("Allow"), "GET, HEAD"; got != want {
		t.Errorf("Allow = %q, want %q", got, want)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestLinearSessionLinkErrors(t *testing.T) {
	tests := []struct {
		name       string
		lookupErr  error
		resolveErr error
		wantStatus int
	}{
		{name: "unseeded routing fallback", resolveErr: fmt.Errorf("resolve: %w", errRoutingSupervisor), wantStatus: http.StatusServiceUnavailable},
		{name: "unexpected resolver error", resolveErr: errors.New("internal resolver detail"), wantStatus: http.StatusInternalServerError},
		{name: "unexpected store error", lookupErr: errors.New("internal store detail"), wantStatus: http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := newLinearSessionLinkHandler(linearSessionLookupFunc(func(context.Context, string) (store.LinearAgentSessionRow, error) {
				return store.LinearAgentSessionRow{}, tc.lookupErr
			}), func(context.Context, *linearagent.SessionEvent) (store.AccountID, string, error) {
				return "", "", tc.resolveErr
			}, "https://compass.example.com", nil)

			rec := serveLinearSessionLink(t, handler, http.MethodGet, "session-123")
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body = %q", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "internal") || strings.Contains(rec.Body.String(), "linear routing") {
				t.Errorf("response body exposes internal error: %q", rec.Body.String())
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func serveLinearSessionLink(t *testing.T, handler http.Handler, method, id string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/l/session/" + id
	if id == "" {
		path = "/l/session/"
	}
	pattern := http.NewServeMux()
	pattern.Handle(linearSessionLinkPath, handler)
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	pattern.ServeHTTP(rec, req)
	return rec
}
