//go:build unix

package gateway

// The retirement seam between the socket and the control producer.
//
// control_test.go proves controlProducer.Retire drops a session's state. That is
// only worth anything if the session lifecycle can REACH it: the producer is
// created inside Serve and is otherwise unreachable, so the single line
// `l.control = control` at the end of Serve, plus SocketListener.RetireSession's
// delegation, are the whole path from "the session stopped" to "its control
// state is gone". Either one silently dropped in a later refactor restores the
// leak — a socket outlives every session it hosts (Stop/Start reuses the
// container), so an unretired session pins a controlSession and up to
// maxRetainedOps retained ops for the life of the Runner process.
//
// This is the package that can see the leak: `sessions` is unexported and a
// merely-leaked entry has no other observable surface, so the count is asserted
// directly here while the package-runner half (e2e_retire_test.go) pins that
// agentHost.Stop is what drives it.
//
// Deterministic and sleep-free: the op is retained BEFORE the subscription binds,
// so the drainer's redelivery is the event that unblocks the client — the
// server writes no response headers until it has something to send, which is
// exactly why subscribing first would park the dial.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
)

// producer returns the control producer Serve handed the listener. A nil one is
// named rather than nil-deref'd: it is the exact regression this file exists to
// catch, so it deserves to say so.
func (l *SocketListener) producer(t *testing.T) *controlProducer {
	t.Helper()
	if l.control == nil {
		t.Fatal("listener has no control producer: Serve built one for the Gateway but never handed it to the listener, so RetireSession is a permanent no-op and every session's control state leaks for the life of the socket")
	}
	return l.control
}

// TestRetireSessionReachesTheServedProducer is the leak assertion.
//
// Two things must hold and both are asserted. First, the listener's producer is
// THE producer this socket's Gateway drains from — not merely some producer: an
// op handed to l.control comes back out of a Control stream dialed over the
// socket, which is only possible if they are one object. Second, RetireSession
// drops that session's entry.
//
// Reverting `l.control = control` in Serve reddens the first (there is no
// producer to retire through at all); deleting the `l.control.Retire(sessionID)`
// body of RetireSession reddens the second — the entry survives, which IS the
// leak.
func TestRetireSessionReachesTheServedProducer(t *testing.T) {
	path := socketPath(t)
	l, err := Serve(context.Background(), path, "cont-1",
		staticSessions{sessionID: testSession, ok: true}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Serve = %v, want a live listener", err)
	}
	t.Cleanup(func() { _ = l.Close(context.Background()) })

	p := l.producer(t)

	// The lifecycle binds the session at Start, through the same listener seam
	// the retirement uses at Stop.
	l.BindSession(testSession)

	// Retain an op before anyone subscribes: Send succeeds with no subscription
	// bound (that is the retention contract), and a subscription starts draining
	// from the ack cursor, so this op is owed to whoever binds next.
	if err := p.Send(testSession, promptOp("bound")); err != nil {
		t.Fatalf("Send through the listener's producer = %v", err)
	}
	if got := p.sessionCount(); got != 1 {
		t.Fatalf("sessions on the served producer = %d, want 1 (the fixture must create the state the retirement reclaims)", got)
	}

	// t.Context is cancelled at test end, so a subscription that outlives the
	// assertions is reaped rather than leaked into the rest of the package's run.
	stream, err := agentClient(t, path).Control(t.Context(),
		connect.NewRequest(&compassv1internal.ControlSubscribeRequest{}))
	if err != nil {
		t.Fatalf("Control over the socket = %v, want a bound subscription", err)
	}
	defer func() { _ = stream.Close() }()

	// Identity gate: the op went IN at the listener's producer and comes OUT of
	// the Gateway's drain over the real socket. A listener holding a DIFFERENT
	// producer than the Gateway serves would deliver nothing here, so retiring
	// through it could never touch the served session.
	if !stream.Receive() {
		t.Fatalf("no op reached the agent over the socket (stream err %v): the listener's producer is not the one the Gateway serves", stream.Err())
	}
	if input := stream.Msg().GetPrompt().GetInput(); input != "bound" {
		t.Fatalf("op off the socket = %q, want %q", input, "bound")
	}

	l.RetireSession(testSession)

	if got := p.sessionCount(); got != 0 {
		t.Fatalf("sessions after RetireSession = %d, want 0: the stopped session's control state is still pinned on a socket that outlives it, which is the per-Stop/Start leak the retirement exists to close", got)
	}
}

// TestRetireSessionOnUnwiredListenerIsANoOp pins the nil-producer guard.
//
// listenAgentSocket is what CONSTRUCTS a SocketListener and it never sets
// control; only Serve does, afterwards. An unwired listener is therefore a real
// shape, not a fabricated zero value — and RetireSession is called from a
// teardown path (agentHost.Stop) against whatever listener the container has.
// Dropping the `if l.control == nil` guard turns a routine session stop into a
// nil-deref that takes the whole Runner down. The socket is exercised afterwards
// because the other way to get this wrong is for the no-op branch to disturb the
// live listener.
func TestRetireSessionOnUnwiredListenerIsANoOp(t *testing.T) {
	path := socketPath(t)
	l, err := listenAgentSocket(context.Background(), path, stubHandler(t), func() {})
	if err != nil {
		t.Fatalf("listenAgentSocket = %v, want a live listener", err)
	}
	t.Cleanup(func() { _ = l.Close(context.Background()) })

	l.RetireSession(testSession)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if code := connect.CodeOf(callComms(ctx, agentClient(t, path))); code != connect.CodeUnimplemented {
		t.Fatalf("Comms after retiring on an unwired listener = %v, want %v (the socket must still be serving)", code, connect.CodeUnimplemented)
	}
}

// The same shape on the bind side, and on the hotter path: BindSession is
// called from agentHost.Start against whatever listener the container has, so
// dropping its `if l.control == nil` guard turns a routine session START into
// the same nil-deref. Start runs far more often than the teardown its
// counterpart guards.
func TestBindSessionOnUnwiredListenerIsANoOp(t *testing.T) {
	path := socketPath(t)
	l, err := listenAgentSocket(context.Background(), path, stubHandler(t), func() {})
	if err != nil {
		t.Fatalf("listenAgentSocket = %v, want a live listener", err)
	}
	t.Cleanup(func() { _ = l.Close(context.Background()) })

	l.BindSession(testSession)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if code := connect.CodeOf(callComms(ctx, agentClient(t, path))); code != connect.CodeUnimplemented {
		t.Fatalf("Comms after binding on an unwired listener = %v, want %v (the socket must still be serving)", code, connect.CodeUnimplemented)
	}
}
