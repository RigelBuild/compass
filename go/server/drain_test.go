//go:build unix

package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
)

// TestDrainDoorsOverrunCancelsWedgedHandlers: a handler that outlives the drain
// deadline must have its request context cancelled, so it releases whatever it
// holds (a pool connection) before Serve's deferred store close waits on it.
// HTTP/1 and h2c cancel through different paths; the doors speak both.
func TestDrainDoorsOverrunCancelsWedgedHandlers(t *testing.T) {
	prev := drainTimeout
	drainTimeout = 50 * time.Millisecond
	t.Cleanup(func() { drainTimeout = prev })

	for _, tc := range []struct {
		name      string
		transport func() *http.Transport
	}{
		{"http1", func() *http.Transport { return &http.Transport{DisableKeepAlives: true} }},
		{"h2c", func() *http.Transport {
			return h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan struct{})
			released := make(chan struct{})
			srv := &http.Server{
				Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					close(entered)
					<-r.Context().Done() // wedged until the server cancels the request
					close(released)
				}),
				Protocols:         cleartextHTTP2(),
				ReadHeaderTimeout: time.Second,
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() }) // a fatal before the drain must not leak the server
			// A private transport: no proxy env and no pooled state shared with other tests.
			client := &http.Client{Transport: tc.transport()}
			t.Cleanup(client.CloseIdleConnections)
			done := make(chan struct{})
			go func() {
				defer close(done)
				req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String(), nil)
				if resp, err := client.Do(req); err == nil {
					_ = resp.Body.Close()
				}
			}()
			select {
			case <-entered:
			case <-timeAfter():
				t.Fatal("handler never entered")
			}

			err = drainDoors(drainSet{
				bus:      events.NewBus[busPayload](),
				commsBus: events.NewBus[*compassv1.SubscribeCommsResponse](),
				uds:      srv,
				hub:      runnerhub.NewHub(nil, nil, nil, slog.Default()),
				log:      slog.Default(),
			})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("drainDoors = %v, want the overrun reported", err)
			}
			select {
			case <-released:
			case <-timeAfter():
				t.Fatal("wedged handler's request context was never cancelled after the drain overran")
			}
			select {
			case <-done:
			case <-timeAfter():
				t.Fatal("client request never returned after the doors closed")
			}
		})
	}
}

// TestAwaitReadyHooksOverrunStillJoins: past the drain deadline the seed is
// reported as an overrun, but the drain must keep waiting for it, or Serve's
// deferred store close would race the seed's store calls.
func TestAwaitReadyHooksOverrunStillJoins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), drainTimeout)
		defer cancel()
		hooksDone := make(chan struct{})
		var got error
		returned := make(chan struct{})
		go func() {
			got = awaitReadyHooks(ctx, hooksDone)
			close(returned)
		}()
		time.Sleep(drainTimeout) //nolint:forbidigo // synctest advance: fake clock passes the deadline
		synctest.Wait()
		select {
		case <-returned:
			t.Fatal("awaitReadyHooks returned past the deadline while the seed was still running")
		default:
		}
		close(hooksDone)
		<-returned
		if !errors.Is(got, context.DeadlineExceeded) {
			t.Fatalf("awaitReadyHooks = %v, want the overrun reported", got)
		}
	})
}

// TestAwaitReadyHooksFinishedSeedIsNotOverrun: a seed that finished by the time
// the deadline passed is clean, even though both select cases are ready.
func TestAwaitReadyHooksFinishedSeedIsNotOverrun(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	hooksDone := make(chan struct{})
	close(hooksDone)
	for range 100 { // select picks a ready case at random
		if err := awaitReadyHooks(ctx, hooksDone); err != nil {
			t.Fatalf("awaitReadyHooks with the seed finished = %v, want nil", err)
		}
	}
}
