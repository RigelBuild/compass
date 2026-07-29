//go:build unix

package gateway

// publisher.go is the ordered per-session upstream publisher shared by the two
// telemetry-ingest handlers: Publish (client-stream, trace/session frames) and
// PostConversationFrame (unary, durable conversation frames). Both forward
// frames up the one RunnerService.PublishEvents client-stream the Runner
// holds for this session, Runner-sequenced — the exact stamping relay.go does for
// the stdout relay (relay.go:157-160), minus the scanner and protojson decode.
//
// The load-bearing property is ordering under concurrency. A durable unary can
// race a stream frame; if two goroutines allocated the sequence and then sent
// independently, allocation order could diverge from emission order and the hub's
// gap detector (seq > lastSeq+1) would record a false gap. So one mutex guards
// BOTH the sequence allocation AND the matching Send as a single critical
// section: the goroutine that allocates seq N is the one that sends seq N, before
// any other goroutine can allocate N+1. Emission order == allocation order by
// construction, across both paths (transport-consolidation record).
//
// Scope mirrors relay.go's eventPublisher: the sequence is per-session (one
// publisher per live Publish stream), exact for the single-Runner / single-live-
// session MVP. The per-Runner hoist stays deferred (relay.go:113-119).

import (
	"context"
	"sync"

	"connectrpc.com/connect"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
)

// EventRelay is the narrow slice of the generated RunnerServiceClient the
// publisher needs — just PublishEvents. The real client satisfies it; a test
// supplies a fake. Mirrors CommsRelay's narrowing of RelayCommsCall.
type EventRelay interface {
	PublishEvents(ctx context.Context) *connect.ClientStreamForClient[compassv1internal.PublishEventsRequest, compassv1internal.PublishEventsResponse]
}

// seqCounter is the RunnerSeq allocator shared by every publisher a Gateway
// builds for its socket. It is a struct rather than a bare uint64 because its
// mutex is the publishers' critical section: the goroutine that allocates
// sequence N is the one that sends N, before any concurrent sender allocates
// N+1, so allocation order == emission order. Sharing the LOCK as well as the
// counter also serializes a replacement publisher's first Send against the
// outgoing publisher's close, so a swap cannot interleave two upstream writers.
type seqCounter struct {
	mu sync.Mutex
	n  uint64
}

// sessionPublisher owns the one PublishEvents client-stream for a session and
// stamps a monotonic RunnerSeq on every frame it forwards. It is opened by the
// Publish handler at stream entry and closed at stream end; PostConversationFrame
// unaries forward through the SAME publisher for the life of that stream, so both
// paths share one sequence and one ordered upstream.
//
// The counter is NOT owned here. A publisher is replaceable within one session
// (the durable path releases on an upstream failure so a dead stream cannot
// wedge the session), and a per-publisher counter restarts at 0 on that swap —
// replaying low seqs under the hub's high-water mark and silently disabling gap
// detection for the replayed range. The counter belongs to the Gateway; see its
// seq field.
type sessionPublisher struct {
	sessionID string

	// seq is the Gateway's shared allocator. Its mutex guards this publisher's
	// stream too: connect client-streams are not safe for concurrent Send, and
	// one lock covering allocate-and-send is what makes allocation order equal
	// emission order.
	seq    *seqCounter
	stream *connect.ClientStreamForClient[compassv1internal.PublishEventsRequest, compassv1internal.PublishEventsResponse]
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
// concurrent caller. idempotencyKey is empty for trace/session frames (Publish)
// and set for durable conversation frames (PostConversationFrame); it rides the
// PublishEventsRequest.idempotency_key envelope field to the Server, which commits
// the durable frame at-most-once per key at the comms Message store. A send error
// is returned to the caller: the Publish handler ends its stream, the
// PostConversationFrame unary surfaces it delivered-or-erred.
func (p *sessionPublisher) forward(frame *compassv1internal.AgentFrame, idempotencyKey string) error {
	p.seq.mu.Lock()
	defer p.seq.mu.Unlock()
	p.seq.n++
	return p.stream.Send(&compassv1internal.PublishEventsRequest{
		RunnerSeq:      p.seq.n,
		SessionId:      p.sessionID,
		Frame:          frame,
		IdempotencyKey: idempotencyKey,
	})
}

// close closes the upstream stream and awaits its ack, mirroring the stdout
// relay's CloseAndReceive at EOF (relay.go:168-171). Called by the Publish
// handler when the agent's client-stream ends. Returns the ack error (nil on a
// clean close) so the handler can classify it. Takes the shared lock, so a close
// never interleaves with another publisher's Send.
func (p *sessionPublisher) close() error {
	p.seq.mu.Lock()
	defer p.seq.mu.Unlock()
	_, err := p.stream.CloseAndReceive()
	return err
}

// acquirePublisher returns the session's shared ordered publisher, creating it on
// first use. Both Publish and PostConversationFrame call it so they share one
// sequence and one upstream stream. The upstream PublishEvents stream is opened
// against the socket-lifetime context (g.baseCtx), NOT the calling handler's
// request context: a PostConversationFrame unary may be the first to open it,
// and binding the shared stream to that unary's request context would tear it
// down the instant the unary returned (net/http cancels a request context on
// handler return), wedging every later forward. The Publish handler is the
// stream owner and closes it at stream end via releasePublisher.
//
// Session-change reset: a Gateway is retained for the container socket across
// Stop→Start (host.go), so a publisher opened for a prior session can linger
// (e.g. one a PostConversationFrame created that no Publish stream ever
// released). If the resolved sessionID no longer matches the lingering
// publisher's, that publisher belongs to a dead session — replace it, and close
// the orphan's upstream stream (best-effort, outside the lock, since a new
// session's frames must never be stamped with the stopped session's id).
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

// releasePublisher closes the shared publisher's upstream stream and clears it so
// a later Publish reconnect opens a fresh one. Called by the Publish handler (the
// stream owner) at stream end, and by the durable path when a forward fails.
// Returns the upstream close/ack error.
//
// pubMu is held ACROSS the close, not just around the field clear. Clearing
// first and closing after opens a window where g.pub is nil while the old
// stream is still draining: a concurrent PostConversationFrame acquires, sees
// nil, and builds a SECOND publisher for the same session whose seq restarts at
// 0. RunnerSeq is contractually monotonic across the Runner's whole event
// stream (runner.proto), and the hub's gap detector is one shared high-water
// mark — a restarted counter replays low seqs under a high lastSeq, so the
// detector goes deaf for that range and a genuine in-transit loss inside it is
// no longer detectable. acquirePublisher takes the same mutex, so it now blocks
// until the close completes and then builds exactly one fresh publisher.
func (g *Gateway) releasePublisher() error {
	g.pubMu.Lock()
	defer g.pubMu.Unlock()
	pub := g.pub
	g.pub = nil
	if pub == nil {
		return nil
	}
	return pub.close()
}
