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

// gatewayProxy is the running unix→vsock forwarder: an AF_UNIX listener at
// agentSocketPath whose every accepted connection is spliced to a fresh
// AF_VSOCK dial of the host gateway (§(d)). Its Close stops the accept loop and
// closes the listener; per-connection lifetimes are bound to the proxy context,
// so a Close (or a ctx cancel) tears every in-flight splice down.
type gatewayProxy struct {
	ln     net.Listener
	cancel context.CancelFunc

	// closeOnce guards Close so the double-close from an explicit Close plus a
	// deferred cleanup is a no-op, not a second listener close error.
	closeOnce sync.Once
	// done is closed when the accept loop has returned, so Close blocks until
	// the loop observes the closed listener and no goroutine outlives Close.
	done chan struct{}
}

// startGatewayProxy creates /run/compass, listens at agentSocketPath owned 0600
// by uid, and starts the accept loop that per-connection dials the host gateway
// at (CID 2, port) via s.dialGateway and splices both directions (§(d)). A
// MkdirAll / listen / chmod / chown failure returns an error and starts nothing
// — the caller (Provision) surfaces it as CodeInternal with the exec gate still
// closed. The returned io.Closer stops the accept loop and closes the listener;
// on ctx cancel every in-flight connection is closed so no io.Copy goroutine
// leaks.
func (s *supervisor) startGatewayProxy(ctx context.Context, socketPath string, uid uint32, port uint32) (io.Closer, error) {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o711); err != nil { //nolint:gosec // G301: /run/compass must be world-traversable (o+x, not o+r) so the non-root exec uid reaches its own 0600 socket without letting other in-guest uids enumerate the dir; confinement is the VM + the socket's owner-only mode, not the dir bit
		return nil, fmt.Errorf("creating agent socket dir %s: %w", filepath.Dir(socketPath), err)
	}
	// A stale socket file from a crashed predecessor would fail the bind with
	// EADDRINUSE; remove it first. A missing file is not an error here.
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("removing stale agent socket %s: %w", socketPath, err)
	}
	ln, err := net.Listen("unix", socketPath)
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

// acceptLoop accepts client connections until the proxy context is cancelled or
// the listener is closed, forwarding each to a fresh gateway dial. A dial error
// closes ONLY that accepted connection and the loop continues; a fatal Accept
// error (the listener closed by Close) ends the loop. A goroutine per
// connection is tracked in wg so the loop's return waits for every splice to
// finish before signalling done — no splice outlives the proxy.
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

// forward splices one accepted client connection to a fresh gateway dial. A
// dial failure closes only this client connection and returns, leaving the
// accept loop running. On success both directions are copied in their own
// goroutines with a half-close on either EOF, and a ctx-cancel watcher closes
// BOTH conns so neither io.Copy goroutine can block forever — the whole splice
// tears down on proxy shutdown with no leak.
func (p *gatewayProxy) forward(ctx context.Context, client net.Conn, dial func(port uint32) (net.Conn, error), port uint32) {
	upstream, err := dial(port)
	if err != nil || upstream == nil {
		// A dial before the host listener exists (or any dial fault) fails this
		// one connection visibly rather than wedging the proxy — the lazy-dial
		// property (§(d)). Close only the client; the accept loop survives. The
		// upstream==nil arm also proves non-nil below for the later Close (a seam
		// that returned (nil, nil) would otherwise nil-panic in the AfterFunc).
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
