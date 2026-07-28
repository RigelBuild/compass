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

// sessionPublisher owns the one PublishEvents client-stream for a session and
// stamps a monotonic RunnerSeq on every frame it forwards. It is opened by the
// Publish handler at stream entry and closed at stream end; PostConversationFrame
// unaries forward through the SAME publisher for the life of that stream, so both
// paths share one sequence and one ordered upstream.
type sessionPublisher struct {
	sessionID string

	// mu serializes the allocate-seq-and-send critical section. It covers seq so
	// the goroutine that allocates a sequence is the one that sends it, before
	// any concurrent sender allocates the next — allocation order == emission
	// order. It also guards stream: connect client-streams are not safe for
	// concurrent Send.
	mu     sync.Mutex
	seq    uint64
	stream *connect.ClientStreamForClient[compassv1internal.PublishEventsRequest, compassv1internal.PublishEventsResponse]
}

// newSessionPublisher opens the upstream PublishEvents client-stream for
// sessionID and returns the publisher that drives it. ctx bounds the stream's
// life (the Publish handler's inbound ctx).
func newSessionPublisher(ctx context.Context, relay EventRelay, sessionID string) *sessionPublisher {
	return &sessionPublisher{
		sessionID: sessionID,
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
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	return p.stream.Send(&compassv1internal.PublishEventsRequest{
		RunnerSeq:      p.seq,
		SessionId:      p.sessionID,
		Frame:          frame,
		IdempotencyKey: idempotencyKey,
	})
}

// close closes the upstream stream and awaits its ack, mirroring the stdout
// relay's CloseAndReceive at EOF (relay.go:168-171). Called by the Publish
// handler when the agent's client-stream ends. Returns the ack error (nil on a
// clean close) so the handler can classify it.
func (p *sessionPublisher) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
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
		g.pub = newSessionPublisher(g.baseCtx, g.events, sessionID)
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
// stream owner) at stream end. Returns the upstream close/ack error.
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
