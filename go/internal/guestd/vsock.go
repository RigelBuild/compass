//go:build linux

package guestd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/mdlayher/vsock"

	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// shutdownTimeout bounds the graceful drain of the vsock server once the boot
// context is cancelled. The host tears the VMM down, so this is a courtesy
// drain, not a hard requirement.
const shutdownTimeout = 5 * time.Second

// hostCID is the only allowed AF_VSOCK peer; other CIDs are refused before reading HTTP bytes.
const hostCID = 2

// peerAllowed is the guest-authenticates-host boundary; only the host CID is accepted.
func peerAllowed(remoteCID uint32) bool {
	return remoteCID == hostCID
}

// peerCIDListener closes non-host connections before HTTP Serve sees them, then continues accepting.
type peerCIDListener struct {
	net.Listener
}

func (l *peerCIDListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if remoteAllowed(conn) {
			return conn, nil
		}
		// Non-host peer (incl. loopback CID 1): refuse before any HTTP byte.
		_ = conn.Close() // refused peer; a close error on it is not actionable
	}
}

// remoteAllowed extracts the peer CID from a vsock connection's remote address
// and applies peerAllowed. A non-vsock RemoteAddr (a hermetic net.Pipe/TCP test
// listener) is allowed — the CID gate is meaningful only over AF_VSOCK, which
// serveVsock is the sole producer of; hermetic serve paths are trusted.
func remoteAllowed(conn net.Conn) bool {
	addr, ok := conn.RemoteAddr().(*vsock.Addr)
	if !ok {
		return true
	}
	return peerAllowed(addr.ContextID)
}

// serveVsock is the production serve step (§(d)): it listens on AF_VSOCK at the
// guest CID and the given port, refuses non-host peers at the listener, serves
// the GuestControl handler, and blocks until ctx is cancelled, then drains.
func serveVsock(ctx context.Context, port uint32, svc *supervisor) error {
	ln, err := vsock.Listen(port, nil)
	if err != nil {
		return fmt.Errorf("listening on vsock port %d: %w", port, err)
	}
	return serveHandshake(ctx, &peerCIDListener{Listener: ln}, svc)
}

// serveHandshake mounts the GuestControl handler on an h2c server over the given
// listener and serves until ctx is cancelled. It is split from serveVsock so the
// AF_VSOCK bind and peer-CID gate stay in serveVsock. Its explicit 16 MiB
// request read limit lets large agent-file stdin writes exceed Connect's default.
func serveHandshake(ctx context.Context, ln net.Listener, svc *supervisor) error {
	mux := http.NewServeMux()
	server := connect.NewServer()
	compassv1internalconnect.RegisterGuestControlHandler(server, svc)
	connecthttp.Mount(mux, server, connecthttp.WithReadMaxBytes(16<<20))

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
