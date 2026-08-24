//go:build linux

package guestd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/mdlayher/vsock"

	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// shutdownTimeout bounds the graceful drain of the vsock server once the boot
// context is cancelled. The host tears the VMM down, so this is a courtesy
// drain, not a hard requirement.
const shutdownTimeout = 5 * time.Second

// serveVsock is the production serve step (§(d) step 4-5): it listens on
// AF_VSOCK at the guest CID and the given port, serves the GuestControl
// Connect/h2c handler, and blocks until ctx is cancelled, then drains. Reaching
// this step is the fail-closed proof that net + mount succeeded.
func serveVsock(ctx context.Context, port uint32, svc *healthService) error {
	ln, err := vsock.Listen(port, nil)
	if err != nil {
		return fmt.Errorf("listening on vsock port %d: %w", port, err)
	}
	return serveHandshake(ctx, ln, svc)
}

// serveHandshake mounts the GuestControl handler on an h2c server over the given
// listener and serves until ctx is cancelled. It is split from serveVsock so the
// h2c wiring is exercisable over any net.Listener; the AF_VSOCK bind is
// serveVsock's job. The h2c enabling mirrors the gateway's socket door exactly
// (internal/runner/gateway/socket.go cleartextHTTP2) — the house pattern, no
// x/net/http2 dependency.
func serveHandshake(ctx context.Context, ln net.Listener, svc *healthService) error {
	mux := http.NewServeMux()
	path, handler := compassv1internalconnect.NewGuestControlHandler(svc)
	mux.Handle(path, handler)

	srv := &http.Server{Handler: mux, Protocols: cleartextHTTP2()} //nolint:gosec // vsock-only door (never internet-facing), so the Slowloris ReadHeaderTimeout does not apply

	// A cancelled ctx drives a graceful shutdown; the serve goroutine reports
	// the terminal serve error (or nil on a clean shutdown) back on errCh.
	errCh := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		// The caller's ctx is already cancelled, so it cannot bound the drain;
		// derive the shutdown deadline from an uncancelled copy of it
		// (preserving its values) rather than re-rooting at Background().
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutting down vsock server: %w", err)
		}
		return <-errCh
	case err := <-errCh:
		// The server stopped on its own (a serve fault) before ctx was
		// cancelled — fail-closed: the handshake is no longer answered.
		if err != nil {
			return fmt.Errorf("serving vsock handshake: %w", err)
		}
		return nil
	}
}

// cleartextHTTP2 enables HTTP/1.1 and prior-knowledge cleartext HTTP/2 (h2c) on
// the vsock door, mirroring the gateway's socket door
// (internal/runner/gateway/socket.go) so native Connect/gRPC clients reach the
// handler over the transparent vsock byte stream.
func cleartextHTTP2() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}
