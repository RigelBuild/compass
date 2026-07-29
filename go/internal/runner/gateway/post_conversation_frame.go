//go:build unix

package gateway

// post_conversation_frame.go is the durable-frame ingest: the delivered-or-
// erred unary that carries a conversation_posted / conversation_updated frame off
// the lossy Publish spine (transport-consolidation record OQ-2(c), P1 #1). Unlike
// Publish's fire-and-forget stream, this returns success ONLY after the upstream
// PublishEvents send is accepted, so an agent-side drop is an error the agent
// retries — never a silent gapless loss of a durable message.
//
// Idempotency: the agent retries under a stable idempotency_key, so a
// committed-but-response-lost retry must not duplicate the frame. The durability
// boundary is the atomic commit at the comms Message store keyed on that key
// (store AppendMessage clientRequestID, threaded upstream via
// PublishEventsRequest.idempotency_key); the in-process committedKeys set here is
// an advisory fast-path that short-circuits a retry without a redundant forward.
// A Runner crash that loses the map is safe: the store commit is the real
// boundary, so a post-crash retry hits the committed key at the store.

import (
	"context"

	"connectrpc.com/connect"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
)

// PostConversationFrame forwards ONE durable conversation frame up PublishEvents
// through the shared ordered per-session publisher and returns only once the
// upstream accepts it (delivered-or-erred). It rejects a non-conversation frame
// with CodeInvalidArgument (the durable unary carries only conversation_posted /
// conversation_updated), and fails closed CodePermissionDenied when no session is
// bound. Dedups on idempotency_key: a key already committed in this process
// returns success without re-forwarding (advisory fast-path); the durable
// boundary is the store's atomic at-most-once commit on the same key.
func (g *Gateway) PostConversationFrame(
	ctx context.Context,
	req *connect.Request[compassv1internal.PostConversationFrameRequest],
) (*connect.Response[compassv1internal.PostConversationFrameResponse], error) {
	sessionID, ok := g.sessions.Session(g.containerName)
	if !ok || sessionID == "" {
		return nil, connect.NewError(connect.CodePermissionDenied, errNoSessionForConversation)
	}

	frame := req.Msg.GetFrame()
	if !isConversationFrame(frame) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errNotConversationFrame)
	}

	key := req.Msg.GetIdempotencyKey()
	// Advisory fast-path: a key already committed in this process is a retry whose
	// original forward succeeded — return success without a redundant upstream
	// send. An empty key never short-circuits (nothing to dedup on); it still
	// forwards, and the store applies no dedup for an empty key.
	if key != "" && g.keyCommitted(key) {
		return connect.NewResponse(&compassv1internal.PostConversationFrameResponse{}), nil
	}

	pub := g.acquirePublisher(sessionID)
	// Forward Runner-sequenced through the SAME publisher as Publish so allocation
	// order == emission order; the idempotency_key rides the envelope to the
	// Server's at-most-once store commit. A forward failure is the unary's error,
	// which the agent retries under the same key.
	if err := pub.forward(frame, key); err != nil {
		// Tear the dead publisher down, exactly as Publish does. A connect
		// client-stream caches its first error and every later Send short-
		// circuits on it, and acquirePublisher only replaces g.pub on a SESSION
		// change — never on stream death. Leaving it installed would make one
		// upstream failure wedge every subsequent durable frame for the life of
		// the session, turning a single erred frame into a whole-session outage.
		_ = g.releasePublisher()
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}

	// The forward was accepted upstream. Record the key so a retry whose response
	// was lost short-circuits here (advisory; the store commit is the real
	// boundary).
	if key != "" {
		g.markKeyCommitted(key)
	}
	return connect.NewResponse(&compassv1internal.PostConversationFrameResponse{}), nil
}

// isConversationFrame reports whether frame is a durable conversation variant
// (conversation_posted / conversation_updated) — the only frames the durable
// unary carries. A session frame, an ack, or an unset oneof is rejected.
func isConversationFrame(frame *compassv1internal.AgentFrame) bool {
	if frame == nil {
		return false
	}
	switch frame.GetFrame().(type) {
	case *compassv1internal.AgentFrame_ConversationPosted,
		*compassv1internal.AgentFrame_ConversationUpdated:
		return true
	default:
		return false
	}
}

// keyCommitted reports whether idempotency key has already been forwarded+accepted
// in this process (advisory fast-path). The bounded LRU is internally
// synchronized. Contains does not bump recency (by design: a dedup hit is a
// retry, not new traffic); recency is set when the key is first committed via
// markKeyCommitted. A key evicted before its retry lands merely costs one
// redundant re-forward the store dedups — never a correctness loss.
func (g *Gateway) keyCommitted(key string) bool {
	return g.committedKeys.Contains(key)
}

// markKeyCommitted records idempotency key as forwarded+accepted. Add evicts the
// least-recently-used key past committedKeysMax; an evicted key merely costs one
// redundant re-forward the store dedups, never a correctness loss.
func (g *Gateway) markKeyCommitted(key string) {
	_ = g.committedKeys.Add(key, struct{}{})
}
