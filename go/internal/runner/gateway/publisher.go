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
	"sync"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// EventRelay is the narrow slice of the generated RunnerServiceClient the
// publisher needs — just PublishEvents. The real client satisfies it; a test
// supplies a fake. Mirrors CommsRelay's narrowing of RelayCommsCall.
type EventRelay interface {
	PublishEvents(ctx context.Context) *connect.ClientStreamForClient[compassv1internal.PublishEventsRequest, compassv1internal.PublishEventsResponse]
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

// rollback returns an allocated-but-unsent sequence. A forward that fails must
// not burn a number: the counter is socket-lifetime, so a burned number is a
// permanent hole, and the hub flags a skipped number as in-transit loss
// (runnerhub/hub.go:230). A durable frame erring back to the agent is correct,
// expected behaviour — it must not make the Server report a loss that did not
// happen. Only the latest seq is reclaimed: if another Gateway allocated since,
// the number stays burned and the hub's gap diagnostic reports it.
func (c *SeqCounter) rollback(seq uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == seq {
		c.n--
	}
}

// sessionPublisher owns the one PublishEvents client-stream for a session and
// stamps a monotonic RunnerSeq on every frame it forwards. It is opened by the
// Publish handler at stream entry and closed at stream end. Runner lifecycle
// reports also use one-shot publishers for transitions the agent cannot report;
// durable conversation frames commit via CommitConversationFrame, off this spine.
//
// The counter is NOT owned here. A publisher is replaceable within one Gateway
// (the session-change reset in acquirePublisher closes an orphan bound to a
// stopped session and opens a fresh one), and a per-publisher counter restarts
// at 0 on that swap — replaying low seqs under the hub's high-water mark and
// silently disabling gap detection for the replayed range. The counter belongs
// to the Gateway; see its seq field.
type sessionPublisher struct {
	sessionID string

	// mu guards this publisher's stream: connect client-streams are not safe for
	// concurrent Send, and holding it across allocate-and-send makes allocation
	// order equal emission order. Per-publisher, NOT shared — a close on an outgoing
	// publisher must not block a forward on its live replacement.
	mu     sync.Mutex
	stream *connect.ClientStreamForClient[compassv1internal.PublishEventsRequest, compassv1internal.PublishEventsResponse]

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

// newSessionPublisher opens the upstream PublishEvents client-stream for
// sessionID and returns the publisher that drives it. ctx bounds the stream's
// life: the Publish handler uses the socket-lifetime context, while lifecycle
// reports use a bounded caller ctx. seq is the Gateway counter, carried across
// publishers so the sequence never restarts.
func newSessionPublisher(ctx context.Context, relay EventRelay, sessionID string, seq *SeqCounter) *sessionPublisher {
	ctx, cancel := context.WithCancel(ctx)
	return &sessionPublisher{
		sessionID: sessionID,
		seq:       seq,
		ctx:       ctx,
		cancel:    cancel,
		stream:    relay.PublishEvents(ctx),
	}
}

// forward allocates the next RunnerSeq and sends the frame upstream under one
// critical section, so allocation order matches emission order across every
// concurrent caller. This is the loss-tolerant Publish path only (trace/session
// frames); durable conversation frames no longer ride the publisher — they
// commit request/response via CommitConversationFrame — so no idempotency_key
// rides this envelope. A send error is returned to the caller: the Publish
// handler ends its stream and the agent reconnects per the loss model.
//
// A failed Send rolls the allocation back. The counter is socket-lifetime, so an
// unsent number would be a permanent hole, and the hub reads a skipped number as
// in-transit loss — a stream frame erring back must not make the Server report a
// loss that never happened.
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
	return p.sendLocked(seq, frame)
}

// forwardState sends a lifecycle state past the publish gate, which is already
// sealed for it. Taking mu orders it after every frame admitted on this stream.
func (p *sessionPublisher) forwardState(frame *compassv1internal.AgentFrame) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// A cancelled or closed stream cannot carry the state; skip the allocation.
	if err := p.ctx.Err(); err != nil {
		return err
	}
	return p.sendLocked(p.seq.next(), frame)
}

// sendLocked sends one allocated seq and rolls it back on failure. Caller holds mu.
func (p *sessionPublisher) sendLocked(seq uint64, frame *compassv1internal.AgentFrame) error {
	if err := p.stream.Send(&compassv1internal.PublishEventsRequest{
		RunnerSeq: seq,
		SessionId: p.sessionID,
		Frame:     frame,
	}); err != nil {
		p.seq.rollback(seq)
		return err
	}
	return nil
}

// close closes the upstream stream and awaits its ack, mirroring the stdout
// relay's CloseAndReceive at EOF (relay.go:168-171). The Publish handler closes
// its shared stream when the agent's client-stream ends; Runner lifecycle reports
// close their one-shot streams after forwarding a terminal frame. Returns the
// ack error (nil on a clean close) so the caller can classify it. Takes only this
// publisher's own lock, so a slow ack cannot stall another publisher's Send.
func (p *sessionPublisher) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return p.closeErr
	}
	_, p.closeErr = p.stream.CloseAndReceive()
	p.closed = true
	p.cancel()
	return p.closeErr
}

// acquirePublisher returns the session's ordered publisher, creating it on first
// use. Only the Publish handler calls it now (durable frames commit off this
// spine). The upstream PublishEvents stream is opened against the socket-lifetime
// context (g.baseCtx), NOT the calling handler's request context, so it outlives
// any one request; the Publish handler owns the stream and closes it at stream
// end via releasePublisher. Runner lifecycle reports bypass this shared publisher
// and use bounded one-shot streams so they cannot close it.
//
// Session-change reset: a Gateway is retained for the container socket across
// Stop→Start (host.go), so a publisher opened for a prior session can linger. If
// the resolved sessionID no longer matches the lingering publisher's, that
// publisher belongs to a dead session — replace it, and close the orphan's
// upstream stream (best-effort, outside the lock, since a new session's frames
// must never be stamped with the stopped session's id).
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
		// Close the orphan (a stopped session's, or one a lifecycle report
		// cancelled) to end its upstream stream rather than leak it. Close takes
		// the orphan's mu, so a straggling forward on it finishes first.
		// Discarded: the new publisher is installed, so a close error changes nothing.
		_ = stale.close()
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

// releasePublisher detaches pub (compare-and-clear under pubMu) and closes its
// upstream stream OUTSIDE the lock. Called by the Publish handler with the
// publisher it acquired. Returns the upstream close/ack error.
//
// The close is outside pubMu because CloseAndReceive awaits the Server's ack
// with no timeout; holding pubMu would stall every Publish forward behind one
// unresponsive Server. pub.mu still serializes the close against any Send on the
// same stream, and close is idempotent, so a lifecycle report that already closed
// pub hands back its result. Compare-and-clear matters after that report: a
// handler reopened by BindSession may own g.pub, and the stale handler must not
// clear or close it.
func (g *Gateway) releasePublisher(pub *sessionPublisher) error {
	g.detachPublisher(pub)
	return pub.close()
}
