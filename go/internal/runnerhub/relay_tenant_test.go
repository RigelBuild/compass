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
		"not found":       store.ErrNotFound,
		"store fault":     errors.New("db down"),
		"tenant conflict": store.ErrConflict,
	} {
		t.Run(name, func(t *testing.T) {
			comms := &fakeCommsCaller{postResp: &compassv1.PostMessageResponse{}}
			hub := NewHub(&fakeLifecycleSink{}, &fakeTailSink{}, comms, discardLogger())
			lifecycle := &fakeLifecycleCaller{spawnResp: &compassv1internal.SpawnPeerResponse{}}
			board := &fakeBoardCaller{resp: &compassv1internal.SetIssueStateResponse{}}
			transcripts := &fakeTranscriptStore{}
			forge := &fakeForgeCaller{}
			blobs := &fakeSessionBlobStore{}
			hub.SetLifecycleCaller(lifecycle)
			hub.SetBoardCaller(board)
			hub.SetTranscriptStore(transcripts)
			hub.SetForgeCaller(forge)
			hub.SetSessionBlobStore(blobs)
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
			_, forgeErr := hub.RelayForgeCall(ctx, testRunnerID, relayCreateIssue("sess-1", "f-1", &compassv1internal.CreateIssueRequest{Repo: "o/r", Title: "t"}))
			blobData := []byte("tenant lookup guard")
			_, blobErr := hub.RelaySessionBlob(ctx, testRunnerID, &compassv1internal.RelaySessionBlobRequest{
				SessionId: "sess-1",
				Blob:      &compassv1internal.PutSessionBlobRequest{Sha256: blobHash(blobData), Data: blobData},
			})
			for relay, err := range map[string]error{"comms": commsErr, "lifecycle": lifecycleErr, "board": boardErr, "commit": commitErr, "forge": forgeErr, "session blob": blobErr} {
				want := connect.CodeFailedPrecondition
				if name == "store fault" {
					want = connect.CodeUnavailable
				}
				if connect.CodeOf(err) != want {
					t.Errorf("%s relay error = %v, want %s", relay, err, want)
				}
			}
			if name == "tenant conflict" && connect.CodeOf(commsErr) != connect.CodeFailedPrecondition {
				t.Fatalf("tenant conflict code = %v, want FailedPrecondition", connect.CodeOf(commsErr))
			}
			if n := len(comms.snapshot()) + len(lifecycle.snapshot()) + len(board.snapshot()) + len(transcripts.snapshot()) + len(forge.snapshot()) + blobs.putCalls; n != 0 {
				t.Fatalf("services called %d times without a session tenant, want 0", n)
			}
		})
	}
}
