//go:build unix

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/RigelBuild/compass/go/internal/linearagent"
	"github.com/RigelBuild/compass/go/internal/secrets"
)

// TestBuildLinearWiringSessionSink pins where a verified session event lands: the
// dispatcher queue when Linear is configured, else a logged drop, never a typed-nil panic.
func TestBuildLinearWiringSessionSink(t *testing.T) {
	ctx := context.Background() // test root
	const secretName = "LINEAR_WEBHOOK_SECRET"
	secret := []byte("shh")
	res := &fakeResolver{resolved: []secrets.ResolvedSecret{{Name: serverSecretName(secretName), Value: string(secret)}}}
	cfg := ServeConfig{PublicURL: "https://compass.example.com", Forge: ForgeConfig{LinearWebhookSecretName: secretName}}
	now := time.Unix(1_700_000_000, 0)

	mount := func(t *testing.T, cfg ServeConfig, tokens *linearagent.TokenSource) (linearWiring, *linearWebhookHandler) {
		t.Helper()
		w, err := buildLinearWiring(ctx, cfg, nil, nil, nil, res, "acct-admin", "acct-bridge", tokens)
		if err != nil {
			t.Fatalf("buildLinearWiring: %v", err)
		}
		lh, ok := w.webhook.(*linearWebhookHandler)
		if !ok {
			t.Fatalf("webhook = %T, want a mounted *linearWebhookHandler", w.webhook)
		}
		lh.now = func() time.Time { return now }
		return w, lh
	}
	post := func(lh *linearWebhookHandler, session string) int {
		body := fmt.Appendf(nil, `{"type":"AgentSessionEvent","action":"created","webhookTimestamp":%d,"agentSession":{"id":%q}}`, freshTS(now), session)
		return linPost(lh, linSign(secret, body), body).Code
	}

	t.Run("configured: events reach the dispatcher queue", func(t *testing.T) {
		w, lh := mount(t, cfg, linearagent.NewTokenSource("cid", "csecret", nil, ""))
		if w.responder == nil {
			t.Fatal("responder == nil with Linear configured, want a dispatcher")
		}
		if lh.sessionSink != SessionEventSink(w.responder) {
			t.Fatalf("session sink = %v, want the built dispatcher", lh.sessionSink)
		}
		// Nothing drains, so the queue fills: every POST up to the buffer is
		// accepted, and the next one is the 500 that makes Linear retry.
		for i := range linearResponderBuffer {
			if code := post(lh, fmt.Sprintf("s%d", i)); code != http.StatusOK {
				t.Fatalf("POST %d code = %d, want 200 (enqueued)", i, code)
			}
		}
		if code := post(lh, "overflow"); code != http.StatusInternalServerError {
			t.Fatalf("overflow POST code = %d, want 500 (queue full)", code)
		}
	})

	t.Run("off: nil interface sink, event dropped", func(t *testing.T) {
		w, lh := mount(t, cfg, nil)
		if w.responder != nil || w.notify != nil {
			t.Fatalf("Linear lanes built while off: responder=%v notify=%v", w.responder, w.notify)
		}
		// A typed-nil *Dispatcher would compare non-nil here and panic on Enqueue.
		if lh.sessionSink != nil {
			t.Fatalf("session sink = %#v, want a nil interface", lh.sessionSink)
		}
		if code := post(lh, "s1"); code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (logged and dropped)", code)
		}
	})

	t.Run("configured without a public URL fails boot", func(t *testing.T) {
		noURL := cfg
		noURL.PublicURL = ""
		_, err := buildLinearWiring(ctx, noURL, nil, nil, nil, res, "acct-admin", "acct-bridge", linearagent.NewTokenSource("cid", "csecret", nil, ""))
		if !errors.Is(err, errNoPublicURL) {
			t.Fatalf("err = %v, want errNoPublicURL (deep links would be relative)", err)
		}
	})
}

// TestStartLinearResponderShutdownIsClean pins that the drain ending with the
// serve context is a clean exit, else every graceful shutdown returns an error.
func TestStartLinearResponderShutdownIsClean(t *testing.T) {
	tests := []struct {
		name   string
		parent func() (context.Context, context.CancelFunc)
	}{
		{"cancelled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}},
		{"deadline passed", func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Unix(0, 0))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancel := tc.parent()
			defer cancel()
			g, gctx := errgroup.WithContext(parent)
			startLinearResponder(gctx, g, linearagent.NewDispatcher(linearagent.DispatcherParams{Buffer: 1}))
			if err := g.Wait(); err != nil {
				t.Fatalf("serve group = %v, want nil (shutdown is not a serve error)", err)
			}
		})
	}
}
