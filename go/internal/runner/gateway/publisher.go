//go:build unix

package gateway

// publisher.go is the ordered per-session upstream publisher for the Publish
// telemetry-ingest handler (client-stream, trace/session frames). It forwards
// each frame up the one RunnerService.PublishEvents client-stream the Runner
// holds for this session, Runner-sequenced — the exact stamping relay.go does for
// the stdout relay (relay.go:157-160), minus the scanner and protojson decode.
// Durable conversation frames do NOT ride this publisher: they leave the
// loss-tolerant Publish spine and commit request/response via
// CommitConversationFrame (post_conversation_frame.go).
//
// The load-bearing property is ordering under concurrency. Although the Publish
// client-stream delivers frames serially, the publisher can be replaced within a
// session and a slow Send must not let allocation order diverge from emission
// order — else the hub's gap detector (seq > lastSeq+1) would record a false gap.
// So a publisher holds its own stream mutex across BOTH the sequence allocation
// AND the matching Send as a single critical section: the goroutine that
// allocates seq N is the one that sends seq N, before any other goroutine on that
// publisher can allocate N+1. Emission order == allocation order by construction
// (transport-consolidation record). The counter's own lock is separate and is held
// only to allocate, so one publisher's close can never stall another's Send.
//
// Scope: the counter is Gateway-scoped — per socket, i.e. per Runner link — and
// survives a publisher replacement, because a publisher is replaceable within one
// session and a per-publisher counter would restart the sequence on that swap.
// relay.go's eventPublisher still owns a SECOND counter, and both feed the hub's
// single high-water mark (runnerhub/hub.go:229-236), so gap detection is only
// meaningful while exactly one of them is live. Unifying the two into one truly
// per-Runner sequence is T9 (relay.go:113-119).

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

// seqCounter is the RunnerSeq allocator shared by every publisher a Gateway
// builds for its socket. It carries ONLY the counter and the lock that guards
// the counter — deliberately not the publishers' stream lock.
//
// Sharing the counter is required: a publisher is replaceable within one session,
// and a per-publisher counter restarts the sequence on that swap. Sharing the
// STREAM lock as well is not, and is actively harmful: acquirePublisher installs
// a replacement and then closes the stale publisher outside pubMu, so a
// CloseAndReceive round-trip against an unresponsive-but-connected Server would
// block every forward on the live replacement's separate upstream stream. Two
// distinct streams need no mutual ordering — the hub keeps one global high-water
// mark and cannot observe an interleaving between them — so the coupling would
// buy nothing and cost unbounded liveness.
type seqCounter struct {
	mu sync.Mutex
	n  uint64
}

// next allocates and returns the next sequence. Called with the publisher's own
// stream lock held, so allocation order still equals emission order.
func (c *seqCounter) next() uint64 {
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
// happen. Safe because the caller holds its stream lock across allocate-and-send,
// so no other goroutine can have sent past this value.
func (c *seqCounter) rollback(seq uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == seq {
		c.n--
	}
}

// sessionPublisher owns the one PublishEvents client-stream for a session and
// stamps a monotonic RunnerSeq on every frame it forwards. It is opened by the
// Publish handler at stream entry and closed at stream end; only Publish drives
// it (durable conversation frames commit via CommitConversationFrame, off this
// spine).
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
	// concurrent Send, and holding it across allocate-and-send is what makes
	// allocation order equal emission order. It is per-publisher, NOT shared with
	// the Gateway's other publishers — a close on an outgoing publisher must not
	// be able to block a forward on its live replacement.
	mu     sync.Mutex
	stream *connect.ClientStreamForClient[compassv1internal.PublishEventsRequest, compassv1internal.PublishEventsResponse]

	// seq is the Gateway's shared allocator, carried across publishers so the
	// sequence survives a replacement. Only the counter is shared; its lock is
	// held just long enough to allocate.
	seq *seqCounter
}

// newSessionPublisher opens the upstream PublishEvents client-stream for
// sessionID and returns the publisher that drives it. ctx bounds the stream's
// life (the socket-lifetime context, not a handler's request ctx). seq is the
// Gateway's counter, carried across publishers so the sequence never restarts.
func newSessionPublisher(ctx context.Context, relay EventRelay, sessionID string, seq *seqCounter) *sessionPublisher {
	return &sessionPublisher{
		sessionID: sessionID,
		seq:       seq,
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
	seq := p.seq.next()
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
// relay's CloseAndReceive at EOF (relay.go:168-171). Called by the Publish
// handler when the agent's client-stream ends. Returns the ack error (nil on a
// clean close) so the handler can classify it. Takes only this publisher's own
// lock, so a slow ack cannot stall another publisher's Send.
func (p *sessionPublisher) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.stream.CloseAndReceive()
	return err
}

// acquirePublisher returns the session's ordered publisher, creating it on first
// use. Only the Publish handler calls it now (durable frames commit off this
// spine). The upstream PublishEvents stream is opened against the socket-lifetime
// context (g.baseCtx), NOT the calling handler's request context, so it outlives
// any one request; the Publish handler owns the stream and closes it at stream
// end via releasePublisher.
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
	if g.pub != nil && g.pub.sessionID != sessionID {
		stale = g.pub
		g.pub = nil
	}
	if g.pub == nil {
		g.pub = newSessionPublisher(g.baseCtx, g.events, sessionID, &g.seq)
	}
	pub := g.pub
	g.pubMu.Unlock()
	if stale != nil {
		// The stopped session's handlers have already returned (lifecycle
		// dispatch is sequential across Stop→Start, host.go), so no caller still
		// holds the orphan; close it to end its upstream stream rather than leak
		// it until socket teardown. Best-effort: the new publisher is already
		// installed, so a close error changes nothing.
		_ = stale.close()
	}
	return pub
}

// releasePublisher clears the publisher under pubMu, then closes its upstream
// stream OUTSIDE the lock. Called by the Publish handler (the stream owner) at
// stream end. Returns the upstream close/ack error.
//
// The close is deliberately outside pubMu. pub.close() blocks on
// CloseAndReceive awaiting the Server's terminal ack on the socket-lifetime
// stream, with no per-close timeout; holding pubMu across it would let one
// unresponsive-but-connected Server stall every later Publish forward on the
// same mutex — a session-wide telemetry outage from one clean stream-end.
// Clearing g.pub under the lock first is enough. releasePublisher is only
// reached after the handler's Receive loop has drained (publish.go:75/82/88),
// so no forward() from that handler is in flight, and forward()'s per-publisher
// pub.mu serializes any Send against this close on the same stream — the drained
// publisher never Sends concurrently with its own close. A concurrent
// acquirePublisher that observes g.pub==nil just opens a fresh one, and the
// capture under the lock still gives single ownership, so pub is closed exactly
// once. The only residue is a cross-stream reorder of already-sent frames that
// the hub's loss-tolerant gap detector accepts by design: recordSeq flags only a
// forward jump, so a delayed low seq neither flags a gap nor rewinds lastSeq.
// This mirrors acquirePublisher, which already closes its stale publisher
// outside the lock.
func (g *Gateway) releasePublisher() error {
	g.pubMu.Lock()
	pub := g.pub
	g.pub = nil
	g.pubMu.Unlock()
	if pub == nil {
		return nil
	}
	return pub.close()
}
