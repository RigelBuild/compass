//go:build unix

package gateway

// The ordered per-session upstream publisher for the Publish handler. It forwards
// each frame up the one PublishEvents client-stream, Runner-sequenced. Durable
// conversation frames do NOT ride it — they commit via CommitConversationFrame.

// The load-bearing property is ordering under concurrency: a publisher can be
// replaced mid-session and a slow Send must not let allocation order diverge from
// emission order, else the hub's gap detector records a false gap. So a publisher
// holds its stream mutex across BOTH the seq allocation AND the Send.

// Scope: the counter is Runner-link-wide (shared by every container's Gateway) and
// survives a publisher replacement, since a per-publisher counter would restart the
// sequence on a swap.

import (
	"context"
	"log/slog"
	"sync"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
)

// EventRelay is the narrow slice of the generated RunnerServiceClient the
// publisher needs — just PublishEvents. The real client satisfies it; a test
// supplies a fake. Mirrors CommsRelay's narrowing of RelayCommsCall.
type EventRelay interface {
	PublishEvents(ctx context.Context) (compassv1internalconnect.RunnerServicePublishEventsClientStream, error)
}

// SeqCounter is the RunnerSeq allocator shared by every publisher of every
// Gateway on one Runner link. It carries ONLY the counter and the lock that
// guards the counter — deliberately not the publishers' stream lock.
//
// Sharing the counter is required: a publisher is replaceable within one session,
// and a per-publisher counter restarts the sequence on that swap. Sharing the
// STREAM lock as well is not, and is actively harmful: acquirePublisher installs
// a replacement and then closes the stale publisher outside pubMu, so a
// CloseAndReceive round-trip against an unresponsive-but-connected Server would
// block every forward on the live replacement's separate upstream stream. Two
// distinct streams may therefore deliver out of seq order; the hub tolerates a
// late lower seq, so the coupling would buy nothing and cost unbounded liveness.
type SeqCounter struct {
	mu sync.Mutex
	n  uint64
}

// next allocates and returns the next sequence. Called with the publisher's own
// stream lock held, so allocation order still equals emission order.
func (c *SeqCounter) next() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.n
}

// rollback reclaims the latest allocated-but-unsent sequence. Leaving a hole
// makes the hub report loss that did not occur; older allocations stay burned.
func (c *SeqCounter) rollback(seq uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == seq {
		c.n--
	}
}

// sessionPublisher owns a PublishEvents stream for a session. Lifecycle reports
// use one-shot publishers; durable frames use CommitConversationFrame instead.
// The Gateway owns the sequence counter so replacing a publisher cannot restart
// RunnerSeq below the hub's high-water mark.
type sessionPublisher struct {
	sessionID string
	// traceparent is ctx's trace, read once; empty for the socket-lifetime publisher.
	traceparent string

	// mu guards this publisher's stream: connect client-streams are not safe for
	// concurrent Send, and holding it across allocate-and-send makes allocation
	// order equal emission order. Per-publisher, NOT shared — a close on an outgoing
	// publisher must not block a forward on its live replacement.
	mu      sync.Mutex
	openErr error
	stream  compassv1internalconnect.RunnerServicePublishEventsClientStream
	// seq is the Gateway's shared allocator, carried across publishers so the
	// sequence survives a replacement. Only the counter is shared; its lock is
	// held just long enough to allocate.
	seq *SeqCounter
	// admit, when set, allocates under the Gateway's publish gate so a sealed
	// session refuses frames instead of sequencing them after its ERRORED report.
	admit func() (uint64, error)
	// afterAdmit is the Gateway's test seam, run between admission and Send.
	afterAdmit func(ctx context.Context)

	// ctx is the stream's own child context; cancel lets a lifecycle report
	// unstick a Send that holds mu past its bound.
	ctx    context.Context //nolint:containedctx // the stream's lifetime, cancelled to unstick its Send
	cancel context.CancelFunc

	// closed and closeErr make close idempotent under mu: a lifecycle report can
	// close the shared stream before its owning handler releases it.
	closed   bool
	closeErr error
}

// newSessionPublisher opens a PublishEvents stream for the given context and
// session. Its context bounds the stream lifetime, and its sequence counter
// survives publisher replacement.
func newSessionPublisher(ctx context.Context, relay EventRelay, sessionID string, seq *SeqCounter) *sessionPublisher {
	traceparent := otelx.Traceparent(ctx)
	ctx, cancel := context.WithCancel(ctx)
	stream, err := relay.PublishEvents(ctx)
	if err != nil {
		cancel()
	}
	return &sessionPublisher{
		sessionID:   sessionID,
		traceparent: traceparent,
		seq:         seq,
		ctx:         ctx,
		cancel:      cancel,
		stream:      stream,
		openErr:     relayedError(err),
	}
}

// forward serializes sequence allocation and Send so sequence order matches
// emission order. Only loss-tolerant Publish frames use this stream; durable
// conversation frames use CommitConversationFrame. A failed Send rolls back its
// allocation to avoid a false gap in the socket-lifetime sequence.
func (p *sessionPublisher) forward(frame *compassv1internal.AgentFrame) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var seq uint64
	if p.admit != nil {
		var err error
		if seq, err = p.admit(); err != nil {
			return err
		}
	} else {
		seq = p.seq.next()
	}
	if p.afterAdmit != nil {
		p.afterAdmit(p.ctx)
	}
	return p.sendLocked(seq, frame, p.traceparent)
}

// forwardState sends a lifecycle state past the publish gate, which is already
// sealed for it. Taking mu orders it after every frame admitted on this stream.
func (p *sessionPublisher) forwardState(frame *compassv1internal.AgentFrame, traceparent string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// A cancelled or closed stream cannot carry the state; skip the allocation.
	if err := p.ctx.Err(); err != nil {
		return err
	}
	return p.sendLocked(p.seq.next(), frame, traceparent)
}

// sendLocked sends one allocated seq and rolls it back on failure. Caller holds mu.
func (p *sessionPublisher) sendLocked(seq uint64, frame *compassv1internal.AgentFrame, traceparent string) error {
	if p.openErr != nil {
		p.seq.rollback(seq)
		return p.openErr
	}
	if err := p.stream.Send(&compassv1internal.PublishEventsRequest{
		RunnerSeq:   seq,
		SessionId:   p.sessionID,
		Frame:       frame,
		Traceparent: traceparent,
	}); err != nil {
		p.seq.rollback(seq)
		return relayedError(err)
	}
	return nil
}

// close ends the upstream stream and waits for its ack. Shared agent streams
// close at EOF; lifecycle reports close their one-shot streams after forwarding.
func (p *sessionPublisher) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return p.closeErr
	}
	if p.openErr != nil {
		p.closeErr = p.openErr
	} else {
		_, p.closeErr = p.stream.CloseAndReceive()
	}
	p.closed = true
	p.cancel()
	return p.closeErr
}

// acquirePublisher returns or creates the ordered shared publisher. Its stream
// uses the socket-lifetime context and is closed by the Publish handler; lifecycle
// reports use bounded one-shot streams. A session change replaces a stale
// publisher so new frames cannot carry the stopped session's id.
func (g *Gateway) acquirePublisher(sessionID string) *sessionPublisher {
	g.pubMu.Lock()
	var stale *sessionPublisher
	// A lifecycle report that timed out cancels the shared stream; a reopened
	// session must not inherit that dead stream.
	if g.pub != nil && (g.pub.sessionID != sessionID || g.pub.ctx.Err() != nil) {
		stale = g.pub
		g.pub = nil
	}
	if g.pub == nil {
		g.pub = newSessionPublisher(g.baseCtx, g.events, sessionID, g.seq)
		g.pub.admit = func() (uint64, error) { return g.admitFrame(sessionID) }
		g.pub.afterAdmit = g.afterAdmit
	}
	pub := g.pub
	g.pubMu.Unlock()
	if stale != nil {
		if err := stale.close(); err != nil {
			slog.WarnContext(g.baseCtx, "closing replaced gateway publisher", "session_id", stale.sessionID, "error", err)
		}
	}
	return pub
}

// sharedPublisher returns the live shared publisher for sessionID, or nil. It
// never opens one: a lifecycle report only needs the stream frames rode.
func (g *Gateway) sharedPublisher(sessionID string) *sessionPublisher {
	g.pubMu.Lock()
	defer g.pubMu.Unlock()
	if g.pub == nil || g.pub.sessionID != sessionID {
		return nil
	}
	return g.pub
}

// detachPublisher clears g.pub only if it is still pub, so a caller never
// removes a replacement installed after pub was detached.
func (g *Gateway) detachPublisher(pub *sessionPublisher) {
	g.pubMu.Lock()
	defer g.pubMu.Unlock()
	if g.pub == pub {
		g.pub = nil
	}
}

// releasePublisher detaches pub and closes it outside pubMu because awaiting the
// Server ack has no timeout and must not stall new Publish calls. pub.mu serializes
// close against Send. Compare-and-clear protects a replacement publisher, and
// close is idempotent when a lifecycle report already closed it.
func (g *Gateway) releasePublisher(pub *sessionPublisher) error {
	g.detachPublisher(pub)
	return pub.close()
}
