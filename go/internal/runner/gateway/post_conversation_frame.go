//go:build unix

package gateway

// The durable-frame ingest carries conversation frames off the lossy Publish
// spine. It returns success only after the Server acknowledges the commit.

// Idempotency: the agent retries under a stable idempotency_key. The durability
// boundary is the atomic Message-store commit on that key; the in-process
// committedKeys set is an advisory fast-path. A crash that loses the map is safe —
// the store commit is the real boundary, so a post-crash retry hits the key there.

import (
	"context"

	"connectrpc.com/connect/v2"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// PostConversationFrame commits a durable frame through the Server unary.
// It rejects unsupported variants and an unbound local session before forwarding.
// The Runner forwards every status unchanged; the agent sink retries on its
// bounded schedule regardless of status code.
func (g *Gateway) PostConversationFrame(
	ctx context.Context,
	req *compassv1internal.PostConversationFrameRequest,
) (*compassv1internal.PostConversationFrameResponse, error) {
	sessionID, ok := g.sessions.Session(g.containerName)
	if !ok || sessionID == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errNoSessionForConversation.Error()).WithCause(errNoSessionForConversation)
	}

	frame := req.GetFrame()
	if !isConversationFrame(frame) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errNotConversationFrame.Error()).WithCause(errNotConversationFrame)
	}

	key := req.GetIdempotencyKey()
	// Advisory fast-path: a key already committed in this process is a retry whose
	// original commit succeeded — return success without a redundant upstream
	// commit. An empty key never short-circuits (nothing to dedup on); it still
	// commits, and the store applies no dedup for an empty key.
	if key != "" && g.keyCommitted(key) {
		return &compassv1internal.PostConversationFrameResponse{}, nil
	}

	// Commit on THIS request's ctx: the commit is a unary bound to this call
	// (unlike the Publish stream on baseCtx). The Runner forwards the session_id
	// and frame verbatim and passes every status unchanged.
	if _, err := g.committer.CommitConversationFrame(ctx, &compassv1internal.CommitConversationFrameRequest{
		SessionId:      sessionID,
		Frame:          frame,
		IdempotencyKey: key,
	}); err != nil {
		return nil, relayedError(err)
	}

	// The commit was acknowledged. Record the key so a retry whose response was
	// lost short-circuits here (advisory; the store commit is the real boundary).
	// Failed commits leave the key unmarked.
	if key != "" {
		g.markKeyCommitted(key)
	}
	return &compassv1internal.PostConversationFrameResponse{}, nil
}

// isConversationFrame accepts only durable transcript-entry frames. Other frame
// variants are rejected.
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

// keyCommitted checks the synchronized bounded LRU without changing recency.
// An evicted retry may be re-forwarded; the store performs durable deduplication.
func (g *Gateway) keyCommitted(key string) bool {
	return g.committedKeys.Contains(key)
}

// markKeyCommitted records idempotency key as forwarded+accepted. Add evicts the
// least-recently-used key past committedKeysMax; an evicted key merely costs one
// redundant re-forward the store dedups, never a correctness loss.
func (g *Gateway) markKeyCommitted(key string) {
	_ = g.committedKeys.Add(key, struct{}{})
}
