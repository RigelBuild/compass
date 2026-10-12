//go:build pgtest && unix

package runnerhub

import (
	"context"
	"errors"
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

// hungObjectStore holds every PUT until the pass ctx ends, like an object store
// that accepts the connection and never answers.
type hungObjectStore struct {
	started chan struct{}
}

func (h *hungObjectStore) PutSegment(ctx context.Context, _ string, _ []byte) error {
	select {
	case h.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func (h *hungObjectStore) GetSegment(context.Context, string) ([]byte, error) {
	return nil, errors.New("hung object store: no reads")
}

// A checkpoint commit returns committed while its flush PUT is hung.
func TestCommitConversationFrameReturnsWhileFlushPutHangs(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	st, agent, ctxB := openTenantBSession(t, ctx)
	const sessionID = "sess-b-hung"
	if err := st.RecordAgentSession(ctxB, sessionID, agent.ID); err != nil {
		t.Fatalf("RecordAgentSession: %v", err)
	}
	hub := newHubOnly()
	hub.SetSessionBindingStore(st)
	hub.SetTranscriptStore(st)
	if _, err := hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED); err != nil {
		t.Fatalf("enroll Runner: %v", err)
	}
	if _, _, err := st.RecordSessionBinding(ctxB, sessionID, agent.ID, "runner-1"); err != nil {
		t.Fatalf("RecordSessionBinding after enroll: %v", err)
	}
	if _, err := hub.CommitConversationFrame(ctx, "runner-1", transcriptReq(sessionID, "k-1", 1, false, `{"e":1}`)); err != nil {
		t.Fatalf("commit delta: %v", err)
	}
	objects := &hungObjectStore{started: make(chan struct{}, 1)}
	st.SetObjectStore(objects)

	resp, err := hub.CommitConversationFrame(ctx, "runner-1", transcriptReq(sessionID, "k-2", 2, true, `{"cp":2}`))
	if err != nil || !resp.GetCommitted() {
		t.Fatalf("checkpoint commit = %v, %v; want committed while the PUT hangs", resp, err)
	}
	select {
	case <-objects.started:
	case <-ctx.Done():
		t.Fatalf("flush PUT never started: %v", ctx.Err())
	}
}
