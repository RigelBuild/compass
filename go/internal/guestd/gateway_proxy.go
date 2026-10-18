//go:build linux

package guestd

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/mdlayher/vsock"
)

// agentSocketDir is the fixed directory guestd creates for the agent rendezvous
// socket. The agent binary is byte-identical on podman and microVM: it always
// dials the fixed agentSocketPath, and on microVM guestd bridges that unix
// socket to the host gateway over vsock (§(d)).
const agentSocketDir = "/run/compass"

// agentSocketPath is the fixed AF_UNIX rendezvous the in-guest agent dials — a
// deliberate constant the agent takes no per-session configuration for
// (packages/compass-agent/src/cli.ts:86-89). guestd owns this socket on the
// microVM backend and forwards it to the host AgentGateway over vsock.
const agentSocketPath = agentSocketDir + "/agent.sock"

// gatewayProxy forwards each Unix client connection to a fresh host vsock connection.
// Closing it stops accepts and tears down every in-flight splice.
type gatewayProxy struct {
	ln     net.Listener
	cancel context.CancelFunc

	// closeOnce makes explicit and deferred Close calls safe.
	closeOnce sync.Once
	// done closes after the accept loop and all splice goroutines finish.
	done chan struct{}
}

// startGatewayProxy creates the owner-only socket and starts the Unix-to-vsock forwarder.
// Setup failures leave the exec gate closed; cancellation closes in-flight splices.
func (s *supervisor) startGatewayProxy(ctx context.Context, socketPath string, uid uint32, port uint32) (io.Closer, error) {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o711); err != nil { //nolint:gosec // G301: /run/compass must be world-traversable (o+x, not o+r) so the non-root exec uid reaches its own 0600 socket without letting other in-guest uids enumerate the dir; confinement is the VM + the socket's owner-only mode, not the dir bit
		return nil, fmt.Errorf("creating agent socket dir %s: %w", filepath.Dir(socketPath), err)
	}
	// A stale socket file from a crashed predecessor would fail the bind with
	// EADDRINUSE; remove it first. A missing file is not an error here.
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("removing stale agent socket %s: %w", socketPath, err)
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listening on agent socket %s: %w", socketPath, err)
	}
	// Owner-only, owned by the session's exec uid — the same posture the
	// container socket has on podman (0600, agent-owned). A chmod/chown failure
	// leaves a wider-than-intended socket, so tear the listener down and fail.
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = ln.Close() // best-effort cleanup on the failure path; the listen error is what we return
		return nil, fmt.Errorf("chmod agent socket %s: %w", socketPath, err)
	}
	chown := s.chownSocket
	if chown == nil {
		chown = chownAgentSocket
	}
	if err := chown(socketPath, uid); err != nil {
		_ = ln.Close() // best-effort cleanup on the failure path; the chown error is what we return
		return nil, fmt.Errorf("chown agent socket %s to uid %d: %w", socketPath, uid, err)
	}

	// dialGateway is a required construction seam (run sets realDialGateway),
	// but a nil here would panic inside the spawned acceptLoop's first forward —
	// and a panic in guestd (PID 1) is a kernel panic. Guard it the same way the
	// chownSocket seam above is guarded.
	dial := s.dialGateway
	if dial == nil {
		dial = realDialGateway
	}

	proxyCtx, cancel := context.WithCancel(ctx)
	p := &gatewayProxy{
		ln:     ln,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go p.acceptLoop(proxyCtx, dial, port)
	return p, nil
}

// Close stops the accept loop and closes the listener, then waits for the loop
// to drain every in-flight splice. It is idempotent (closeOnce) so an explicit
// Close plus a deferred cleanup does not double-close.
func (p *gatewayProxy) Close() error {
	p.closeOnce.Do(func() {
		p.cancel()
		_ = p.ln.Close() // stops Accept; the ctx AfterFunc may also close it, guarded by this once
	})
	<-p.done
	return nil
}

// acceptLoop forwards accepted clients to fresh gateway dials; one dial failure closes only that client.
// It waits for all splice goroutines before signaling completion.
func (p *gatewayProxy) acceptLoop(ctx context.Context, dial func(port uint32) (net.Conn, error), port uint32) {
	var wg sync.WaitGroup
	// Close the listener when the context is cancelled so a pending Accept
	// unblocks even if Close was driven by ctx rather than the explicit Close.
	stop := context.AfterFunc(ctx, func() {
		_ = p.ln.Close() // unblocks Accept on ctx cancel; a second close from Close is guarded by closeOnce
	})
	defer func() {
		stop()
		wg.Wait()
		close(p.done)
	}()
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			// The only expected Accept error here is the listener being closed
			// (by Close or the ctx AfterFunc): both are the shutdown signal, so
			// end the loop and let deferred wg.Wait drain in-flight splices.
			return
		}
		wg.Go(func() {
			p.forward(ctx, conn, dial, port)
		})
	}
}

// forward splices the client and gateway connections in both directions.
// Dial failure closes only the client; cancellation closes both connections to unblock copies.
func (p *gatewayProxy) forward(ctx context.Context, client net.Conn, dial func(port uint32) (net.Conn, error), port uint32) {
	upstream, err := dial(port)
	if err != nil || upstream == nil {
		// A dial before the host listener exists (or any dial fault) fails this
		// one connection visibly rather than wedging the proxy — the lazy-dial
		// property (§(d)). The upstream==nil arm also proves non-nil below for the
		// later Close, so a (nil, nil) seam can't nil-panic in the AfterFunc.
		_ = client.Close() // the dial failed, so there is nothing to splice; drop the client
		return
	}

	// Close both ends when the proxy context is cancelled, unblocking either
	// io.Copy that is parked in a read so both goroutines exit — the no-leak
	// invariant on shutdown.
	stop := context.AfterFunc(ctx, func() {
		_ = client.Close()   // ctx-cancel teardown; a later Close in the copy goroutine is a harmless no-op
		_ = upstream.Close() // ctx-cancel teardown; a later Close in the copy goroutine is a harmless no-op
	})
	defer stop()

	var wg sync.WaitGroup
	wg.Add(2)
	// client -> upstream, then half-close upstream's write so a peer blocked on
	// read sees EOF (§(d) half-close on either EOF).
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, client) // copy error IS the connection ending; the half-close below propagates EOF
		halfCloseWrite(upstream)
	}()
	// upstream -> client, symmetric half-close.
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, upstream) // copy error IS the connection ending; the half-close below propagates EOF
		halfCloseWrite(client)
	}()
	wg.Wait()
	// Both directions drained; close both ends fully. On the ctx-cancel path the
	// AfterFunc already closed them, so these are harmless no-ops.
	_ = client.Close()   // both directions drained; a double close after AfterFunc is a no-op
	_ = upstream.Close() // both directions drained; a double close after AfterFunc is a no-op
}

// halfCloseWrite shuts down only the write half of conn if it supports it (a
// unix or vsock or TCP conn does; a net.Pipe used in hermetic tests does not),
// so the copy in the opposite direction can still drain until its own EOF. A
// conn without CloseWrite is left for the full Close in forward.
func halfCloseWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite() // best-effort half-close; an already-closed conn errors here and full Close follows
	}
}

// realDialGateway is the production dialGateway seam: it dials the host
// AgentGateway at the well-known host CID (2) and the given port over AF_VSOCK
// (§(d)). Hermetic tests inject a seam returning one end of a net.Pipe so the
// forward loop runs with no AF_VSOCK.
func realDialGateway(port uint32) (net.Conn, error) {
	conn, err := vsock.Dial(hostCID, port, nil)
	if err != nil {
		return nil, fmt.Errorf("dialing host gateway at (CID %d, port %d): %w", hostCID, port, err)
	}
	return conn, nil
}

// chownAgentSocket is the production chownSocket seam: it chowns the forwarder
// socket to the exec uid (gid == uid, matching the baked agent user convention,
// §(d)). guestd runs as guest root, so this succeeds in production; the hermetic
// test injects a no-op because a non-root test cannot chown to a foreign uid.
func chownAgentSocket(path string, uid uint32) error {
	if err := os.Chown(path, int(uid), int(uid)); err != nil {
		return fmt.Errorf("chown %s to uid %d: %w", path, uid, err)
	}
	return nil
}
