//go:build unix

package runnerhub

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// With a durable binding store wired, a cached binding whose tenant cannot be
// resolved must not reach any relay's service under the bootstrap tenant.
func TestRunnerRelaysFailClosedWithoutSessionTenant(t *testing.T) {
	for name, tenantErr := range map[string]error{
		"not found":   store.ErrNotFound,
		"store fault": errors.New("db down"),
		"ambiguous":   store.ErrConflict,
	} {
		t.Run(name, func(t *testing.T) {
			comms := &fakeCommsCaller{postResp: &compassv1.PostMessageResponse{}}
			hub := NewHub(&fakeLifecycleSink{}, &fakeTailSink{}, comms, discardLogger())
			lifecycle := &fakeLifecycleCaller{spawnResp: &compassv1internal.SpawnPeerResponse{}}
			board := &fakeBoardCaller{resp: &compassv1internal.SetIssueStateResponse{}}
			transcripts := &fakeTranscriptStore{}
			hub.SetLifecycleCaller(lifecycle)
			hub.SetBoardCaller(board)
			hub.SetTranscriptStore(transcripts)
			bindings := newFakeBindingStore()
			bindings.seed("sess-1")
			bindings.tenantErr = tenantErr
			hub.SetSessionBindingStore(bindings)
			bindLiveSession(hub)

			ctx := context.Background()
			_, commsErr := hub.RelayCommsCall(ctx, testRunnerID, relayPost("sess-1", "c-1", &compassv1.PostMessageRequest{}))
			_, lifecycleErr := hub.RelayLifecycleCall(ctx, testRunnerID, relaySpawn("sess-1", "l-1", &compassv1internal.SpawnPeerRequest{}))
			_, boardErr := hub.RelayBoardCall(ctx, testRunnerID, relaySetIssueState("sess-1", "b-1", &compassv1internal.SetIssueStateRequest{}))
			_, commitErr := hub.CommitConversationFrame(ctx, testRunnerID, transcriptReq("sess-1", "k-1", 1, false, `{"e":1}`))
			for relay, err := range map[string]error{"comms": commsErr, "lifecycle": lifecycleErr, "board": boardErr, "commit": commitErr} {
				if connect.CodeOf(err) != connect.CodeNotFound {
					t.Errorf("%s relay error = %v, want CodeNotFound", relay, err)
				}
			}
			if n := len(comms.calls) + len(lifecycle.snapshot()) + len(board.snapshot()) + len(transcripts.calls); n != 0 {
				t.Fatalf("services called %d times without a session tenant, want 0", n)
			}
		})
	}
}
