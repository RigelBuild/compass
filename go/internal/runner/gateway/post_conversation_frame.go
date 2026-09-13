//go:build unix

package gateway

// The durable-frame ingest: the delivered-or-erred handler carrying a
// conversation frame off the lossy Publish spine. It commits via the
// CommitConversationFrame unary and returns success ONLY after the Server acks —
// a loss is a retryable Connect error, never a silent gapless loss (OQ-3).

// Idempotency: the agent retries under a stable idempotency_key. The durability
// boundary is the atomic Message-store commit on that key; the in-process
// committedKeys set is an advisory fast-path. A crash that loses the map is safe —
// the store commit is the real boundary, so a post-crash retry hits the key there.

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
// carries conversation_posted / conversation_updated and the RIG-1570
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

	// Commit on THIS request's ctx: the commit is a unary bound to this call
	// (unlike the Publish stream on baseCtx). The Runner forwards the session_id
	// and frame verbatim and passes the Server's retryability-split status back — a
	// transient failure retries the SAME key, a permanent one drops.
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
// CommitConversationFrame unary carries: the RIG-1570 transcript_entry variant.
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
