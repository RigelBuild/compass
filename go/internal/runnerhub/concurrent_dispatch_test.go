//go:build unix

package runnerhub

// Concurrent dispatch over a real Sessions stream verifies the single-sender invariant under -race.
// The Runner echoes request ids so each result can be checked for correlation and payload integrity.
import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestConcurrentDispatchOverRealStreamNoDataRace(t *testing.T) {
	hub := newHubOnly()
	resolver := &fakeResolver{tokens: map[string]resolverEntry{
		"runner-tok": {subj: store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}},
	}}
	url := newMountedH2CServer(t, hub, resolver.resolve)
	client := newRawRunnerClient(t, url, "runner-tok")

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)

	if _, err := client.Enroll(ctx, &compassv1internal.EnrollRequest{RunnerId: "runner-1"}); err != nil {
		t.Fatalf("Enroll = %v, want success", err)
	}

	// Open the Sessions stream; the Runner-side loop echoes each Start command's
	// request id back as a correlated session id, so a dispatch completes iff its
	// own result returned — and returned carrying its own id in the payload.
	stream, err := client.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	loopErr := make(chan error, 1)
	go echoSessionsLoop(stream, loopErr)

	// Gate on the server binding the router to the live stream — dispatching
	// before router.attach ran would (correctly) find no live send.
	waitRouterAttached(t, hub)

	router, _, err := hub.routerFor("runner-1")
	if err != nil {
		t.Fatalf("routerFor after enroll+attach = %v, want the live router", err)
	}

	// Distinct ids exercise concurrent Send through the shared stream; each goroutine owns its result slot.
	const n = 64
	var wg sync.WaitGroup
	results := make([]*compassv1internal.SessionsRequest, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("req-%d", i)
			results[i], errs[i] = router.dispatch(ctx, startCmd(id))
		}(i)
	}
	wg.Wait()

	// Each result must preserve its caller's id and payload; interleaving would fail decoding or garble it.
	for i := range n {
		id := fmt.Sprintf("req-%d", i)
		if errs[i] != nil {
			t.Errorf("dispatch %s = error %v, want a correlated result", id, errs[i])
			continue
		}
		if got := results[i].GetRequestId(); got != id {
			t.Errorf("dispatch %s returned result for id %q, want %q (correlation crossed)", id, got, id)
		}
		want := "sess-" + id
		if got := results[i].GetStart().GetSessionId(); got != want {
			t.Errorf("dispatch %s payload session id = %q, want %q (frame corrupted/interleaved)", id, got, want)
		}
	}

	// Clean teardown: close the stream and confirm the runner loop saw EOF.
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseRequest = %v", err)
	}
	if err := recvLoopErr(t, loopErr); err != nil {
		t.Fatalf("runner sessions loop ended with %v, want clean EOF", err)
	}
}

// echoSessionsLoop replies to each command with its request id encoded in the session id.
func echoSessionsLoop(stream compassv1internalconnect.RunnerServiceSessionsClientStream, done chan<- error) {
	if err := stream.Send(&compassv1internal.SessionsRequest{}); err != nil {
		done <- err
		return
	}
	for {
		cmd, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				done <- nil
				return
			}
			done <- err
			return
		}
		result := &compassv1internal.SessionsRequest{
			RequestId: cmd.GetRequestId(),
			Result: &compassv1internal.SessionsRequest_Start{
				Start: &compassv1.StartAgentSessionResponse{SessionId: "sess-" + cmd.GetRequestId()},
			},
		}
		if err := stream.Send(result); err != nil {
			done <- err
			return
		}
	}
}
