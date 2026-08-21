//go:build unix

package gateway

// post_conversation_frame.go is the durable-frame ingest: the delivered-or-
// erred handler that carries a conversation_posted / conversation_updated frame
// (and the SEA-1570 transcript_entry tee variant) off the lossy Publish spine
// (transport-consolidation record OQ-2(c), P1 #1).
// It commits the frame request/response via the dedicated
// RunnerService.CommitConversationFrame unary — the durable counterpart to the
// loss-tolerant PublishEvents stream — and returns success ONLY after the Server
// acknowledges the commit. A Server-side loss is a Connect error the agent
// retries, never a silent gapless loss of a durable message (OQ-3): the old path
// acked on a mere PublishEvents buffer-accept, which delivered-or-erred forbids.
//
// Idempotency: the agent retries under a stable idempotency_key, so a
// committed-but-response-lost retry must not duplicate the frame. The durability
// boundary is the atomic commit at the comms Message store keyed on that key
// (store AppendMessage clientRequestID, carried on CommitConversationFrameRequest
// .idempotency_key); the in-process committedKeys set here is an advisory
// fast-path that short-circuits a retry without a redundant commit. A Runner
// crash that loses the map is safe: the store commit is the real boundary, so a
// post-crash retry hits the committed key at the store.

import (
	"context"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// PostConversationFrame commits ONE durable conversation frame to the Server via
// the dedicated CommitConversationFrame unary and returns only once the Server
// acknowledges the commit (delivered-or-erred). Unlike the loss-tolerant Publish
// spine, this does NOT ride the shared PublishEvents publisher: a durable frame
// leaves that spine entirely and commits request/response, so a Server-side loss
// is a Connect error the agent retries, never a silent gapless drop. It rejects
// a frame the durable unary does not carry with CodeInvalidArgument (the lane
// carries conversation_posted / conversation_updated and the SEA-1570
// transcript_entry tee variant), and fails closed
// CodePermissionDenied when no session is bound. Dedups on idempotency_key: a
// key already committed in this process returns success without re-committing
// (advisory fast-path); the durable boundary is the store's atomic at-most-once
// commit on the same key.
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
	// original commit succeeded — return success without a redundant upstream
	// commit. An empty key never short-circuits (nothing to dedup on); it still
	// commits, and the store applies no dedup for an empty key.
	if key != "" && g.keyCommitted(key) {
		return connect.NewResponse(&compassv1internal.PostConversationFrameResponse{}), nil
	}

	// Commit the frame request/response on THIS request's ctx: the commit is a
	// unary bound to this call (unlike the shared Publish stream, which rides the
	// socket-lifetime baseCtx). The Runner is a pure forwarder — it sends the
	// session_id it structurally owns and the frame verbatim, and passes the
	// Server's retryability-split Connect status straight back (mirror Comms). A
	// transient failure surfaces retryable so the sink retries the SAME key; a
	// permanent failure surfaces terminal so the agent drops.
	if _, err := g.committer.CommitConversationFrame(ctx, connect.NewRequest(&compassv1internal.CommitConversationFrameRequest{
		SessionId:      sessionID,
		Frame:          frame,
		IdempotencyKey: key,
	})); err != nil {
		return nil, err
	}

	// The commit was acknowledged. Record the key so a retry whose response was
	// lost short-circuits here (advisory; the store commit is the real boundary).
	// A transient failure returned above, so the key is NOT marked — a retry
	// re-commits under the same key.
	if key != "" {
		g.markKeyCommitted(key)
	}
	return connect.NewResponse(&compassv1internal.PostConversationFrameResponse{}), nil
}

// isConversationFrame reports whether frame is the durable transcript frame the
// CommitConversationFrame unary carries: the SEA-1570 transcript_entry variant.
// A conversation frame, a session frame, an ack, or an unset oneof is rejected.
// (The conversation_posted / conversation_updated write-through was removed with
// the Zulip threading model; only the transcript lane survives.)
func isConversationFrame(frame *compassv1internal.AgentFrame) bool {
	if frame == nil {
		return false
	}
	switch frame.GetFrame().(type) {
	case *compassv1internal.AgentFrame_TranscriptEntry:
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
