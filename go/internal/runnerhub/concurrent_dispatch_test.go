//go:build unix

package runnerhub

// A concurrent-Send data-race regression for the command router over the REAL
// connect Sessions stream. dispatch() enqueues each command onto the router's
// bounded outbound queue, which a single per-router sender goroutine drains as
// the SOLE caller of the server-side BidiStream.Send — connect-go does NOT make
// that Send safe for concurrent use. In production, many client-facing session
// RPCs dispatch onto the ONE shared Sessions stream at once; the single-sender
// invariant is what keeps two of them from entering Send concurrently and
// corrupting frames on the stream's write path. This test drives many concurrent
// dispatches over the real mounted handler under -race, so a regression that
// broke the single-sender invariant (e.g. a direct Send from a caller goroutine)
// reddens here (WARNING: DATA RACE) rather than silently shipping corrupted
// frames. See docs/designs/platform/compass-runnerhub-send-queue/design.md.
//
// Unlike the router_test.go cases (a fake in-process send), this exercises the
// real wire: the Runner-side loop drains every command the Server pushes and
// echoes a correlated result, so each dispatch completes on the observed result
// — event-gated on that completion (wg.Wait), never a sleep or a retry.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
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

	if _, err := client.Enroll(ctx, connect.NewRequest(&compassv1internal.EnrollRequest{RunnerId: "runner-1"})); err != nil {
		t.Fatalf("Enroll = %v, want success", err)
	}

	// Open the Sessions stream; the Runner-side loop echoes each Start command's
	// request id back as a correlated session id, so a dispatch completes iff its
	// own result returned — and returned carrying its own id in the payload.
	stream := client.Sessions(ctx)
	loopErr := make(chan error, 1)
	go echoSessionsLoop(stream, loopErr)

	// Gate on the server binding the router to the live stream — dispatching
	// before router.attach ran would (correctly) find no live send.
	waitRouterAttached(t, hub)

	router, _, err := hub.routerFor("runner-1")
	if err != nil {
		t.Fatalf("routerFor after enroll+attach = %v, want the live router", err)
	}

	// Fire many concurrent dispatches with DISTINCT request ids: the normal
	// concurrent case, each id pushing its own command through the one shared
	// Send. Without sendMu two of these race inside connect's stream write. Each
	// goroutine writes a distinct slice index, so the slices themselves are not a
	// shared-write race — only the stream Send is.
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

	// Every dispatch completed with its own correlated result, uncorrupted: the
	// echoed session-id payload must match this caller's id. A frame interleave
	// from concurrent Send corrupts the bytes on the wire, so the client either
	// fails to decode (the dispatch errors or never completes → ctx timeout) or
	// delivers a garbled payload — both reddening these assertions.
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
	if err := stream.CloseRequest(); err != nil {
		t.Fatalf("CloseRequest = %v", err)
	}
	if err := recvLoopErr(t, loopErr); err != nil {
		t.Fatalf("runner sessions loop ended with %v, want clean EOF", err)
	}
}

// echoSessionsLoop is a Runner-side dispatch loop for the concurrency test: a
// bootstrap Send opens the stream (see runnerSessionsLoop for why the request is
// not sent until the client's first Send), then it drains every command the
// Server pushes and answers each Start with a session id encoding the command's
// request id, so the dispatcher can prove per-call correlation of the payload.
func echoSessionsLoop(stream *connect.BidiStreamForClient[compassv1internal.SessionsRequest, compassv1internal.SessionsResponse], done chan<- error) {
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
