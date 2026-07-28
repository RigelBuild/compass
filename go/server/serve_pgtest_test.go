//go:build pgtest && unix

package server

// Store-gated Serve-loop integration tests: Serve opens the store of record (T1)
// and bootstraps the admin before serving, so every test here needs a real
// Postgres. They are behind the `pgtest` tag (with `unix` for the socket door)
// so the default `go test ./...` lane — which passes no DatabaseDSN — never runs
// them and never hangs on store.Open(""). DatabaseDSN comes from the shared
// pgtest harness (COMPASS_TEST_DATABASE_DSN or a throwaway container; SKIP when
// neither is available).
//
// The socket-bound-and-served, clean-shutdown, and shutdown-with-a-live-
// SubscribeComms-subscriber contracts all live here because each drives a full
// Serve against the database. Hermetic: t.TempDir() socket paths, no fixed ports.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/pgtest"
)

func TestServeBindsSocketServesClientAndCleansUpOnCancel(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "compass.sock")

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Serve(ctx, ServeConfig{
			SocketPath:  socketPath,
			Version:     "serve-test",
			DatabaseDSN: pgtest.RequireDSN(t),
		})
	}()

	// Event-gate on the socket being bound, then assert its mode and that a real
	// client can round-trip GetServerInfo over it.
	waitListening(t, socketPath)

	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat bound socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 0600 (owner-only)", perm)
	}

	client := newUDSClient(t, socketPath)
	resp, err := client.GetServerInfo(context.Background(), connect.NewRequest(&compassv1.GetServerInfoRequest{}))
	if err != nil {
		t.Fatalf("GetServerInfo over socket: %v", err)
	}
	if resp.Msg.GetVersion() != "serve-test" {
		t.Fatalf("Version = %q, want serve-test", resp.Msg.GetVersion())
	}
	if resp.Msg.GetApiVersion() != apiVersion {
		t.Fatalf("ApiVersion = %q, want %q", resp.Msg.GetApiVersion(), apiVersion)
	}

	// Cancel: Serve must drain and return nil (clean shutdown), and remove the
	// socket file it bound.
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve returned %v on ctx cancel, want nil (clean shutdown)", err)
		}
	case <-timeAfter():
		t.Fatal("Serve did not return after ctx cancel")
	}

	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket not removed after shutdown (stat err = %v)", err)
	}
}

func TestServeShutdownIsClean(t *testing.T) {
	// A normal ctx-cancel shutdown with no wedged handler must return nil: the
	// drain completes well within the 5s deadline. This guards the drain-error
	// propagation added to Serve — it must surface a real overrun, never turn a
	// clean shutdown into a spurious "draining ... on shutdown" error.
	//
	// The drain-deadline-overrun path (a wedged slow-client stream) is not
	// forced here: making it deterministic needs a handler stalled mid-replay,
	// tracked separately in SEA-1263. Forcing a server self-exit error cleanly
	// is likewise not deterministic from this harness (both servers exit only on
	// ctx cancel or a bind fault that Serve rejects up front), so that sub-case
	// is intentionally omitted rather than added as a flaky test.
	socketPath := filepath.Join(t.TempDir(), "compass.sock")

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Serve(ctx, ServeConfig{
			SocketPath:  socketPath,
			Version:     "serve-test",
			DatabaseDSN: pgtest.RequireDSN(t),
		})
	}()

	// Gate on a SERVED RPC, not just the socket being connectable: Serve binds
	// the socket before it opens the store, so a connectable socket does not yet
	// mean the server is serving. A successful GetServerInfo proves store.Open
	// completed and the HTTP server is up, so the ctx cancel below exercises the
	// graceful-drain path rather than racing store.Open (which would fail with a
	// context-canceled ping, not the clean shutdown this test asserts).
	waitListening(t, socketPath)
	if _, err := newUDSClient(t, socketPath).GetServerInfo(context.Background(), connect.NewRequest(&compassv1.GetServerInfoRequest{})); err != nil {
		t.Fatalf("GetServerInfo before shutdown: %v", err)
	}
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve on clean ctx-cancel shutdown = %v, want nil", err)
		}
	case <-timeAfter():
		t.Fatal("Serve did not return after ctx cancel")
	}
}

// TestServeShutdownWithLiveCommsSubscriberReturnsClean is the P2 regression: a
// shutdown while a SubscribeComms stream is still held open must still return
// nil within the drain deadline. connect's http.Server.Shutdown does NOT cancel
// an in-flight streaming handler's context, so the ONLY thing that unblocks a
// live SubscribeComms handler is the comms bus closing its Live channel. Serve's
// drain therefore must close commsBus (not only the CompassService bus); the
// applied fix does. Against a drain missing commsBus.Close() the SubscribeComms
// handler stays wedged, udsServer.Shutdown blocks to the 5s deadline, and Serve
// returns the wrapped "draining ... on shutdown" error (non-nil) — reddening the
// nil assertion below — or, if Shutdown blocks past the test deadline, the
// timeout fires. Either way the pre-fix drain is caught; the fix returns nil
// promptly.
//
// That claim was untrue for as long as this test failed at its subscriber gate:
// it never reached the drain, so the regression it names went undefended while
// the test reported red for an unrelated reason. A red test is not
// self-evidently doing its job — it has to fail at the assertion it exists for.
// Verified by deleting the drain's commsBus.Close() and confirming this test
// then fails at the Serve-returns-nil assertion rather than at the gate.
func TestServeShutdownWithLiveCommsSubscriberReturnsClean(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "compass.sock")

	serveCtx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Serve(serveCtx, ServeConfig{
			SocketPath:  socketPath,
			Version:     "serve-test",
			DatabaseDSN: pgtest.RequireDSN(t),
		})
	}()
	waitListening(t, socketPath)

	commsClient := newUDSCommsClient(t, socketPath)

	// Seed one comms event the subscriber is guaranteed to see: create a channel
	// (attributed to the bootstrap admin over the socket door, who is thus a
	// founding member), so the resulting ChannelChanged is visible to that same
	// admin on the stream. A current member always sees its own channel's
	// ChannelChanged, so this gate is stable regardless of how the per-variant
	// visibility filter treats other event kinds.
	created, err := commsClient.CreateChannel(context.Background(), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name: "shutdown-room", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
	}))
	if err != nil {
		t.Fatalf("CreateChannel over socket: %v", err)
	}
	wantChannelID := created.Msg.GetChannel().GetId()

	// Open a SubscribeComms subscriber on its OWN background context — so what
	// releases the held-open stream is the SERVER closing the comms bus on
	// shutdown, never the client cancelling. sinceSeq=0 replays the ring under
	// the same lock that registers the live subscriber, so the seeded
	// ChannelChanged is delivered exactly once and its receipt proves the
	// subscriber is registered and now blocked on the live tail.
	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	stream, err := commsClient.SubscribeComms(subCtx, connect.NewRequest(&compassv1.SubscribeCommsRequest{SinceSeq: 0}))
	if err != nil {
		t.Fatalf("SubscribeComms over socket: %v", err)
	}
	defer func() { _ = stream.Close() }()

	// Drive the stream on a goroutine: consume the preamble, gate on the seeded
	// event (registration proof), then keep receiving so the stream stays open
	// on the comms bus until the server's shutdown drain closes it. The final
	// Receive returning false (clean EOF) is what proves the server-side close
	// released it.
	//
	// A since_seq=0 subscribe receives commsSnapshotBoundary FIRST, before any
	// event (internal/comms/subscribe.go, which documents the frame's shape).
	// Assert that frame rather than skipping one blindly: a bare skip silently
	// re-breaks the moment the preamble grows a second frame, which is how this
	// gate broke in the first place — it was written before the boundary
	// existed and read frame one as the seeded event, failing here with an
	// empty channel id and never reaching the shutdown drain that is its
	// actual subject.
	gate := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if !stream.Receive() {
			// Describe the observable, not a cause: all this proves is that the
			// stream ended before any frame arrived. Naming the boundary as the
			// missing one would mis-attribute a stream that ended early.
			gate <- fmt.Errorf("stream ended before any frame (expected the snapshot boundary first): %w", stream.Err())
			return
		}
		if seq, payload := stream.Msg().GetSeq(), stream.Msg().GetPayload(); seq != 0 || payload != nil {
			gate <- fmt.Errorf("first frame = seq %d payload %T, want the snapshot boundary (seq 0, no payload)", seq, payload)
			return
		}
		if !stream.Receive() {
			gate <- fmt.Errorf("stream ended before the seeded event: %w", stream.Err())
			return
		}
		if got := stream.Msg().GetChannelChanged().GetChannel().GetId(); got != wantChannelID {
			gate <- fmt.Errorf("first event channel id = %q, want the seeded %q", got, wantChannelID)
			return
		}
		gate <- nil
		// Block on the live tail until the server closes the comms bus.
		for stream.Receive() { //nolint:revive // draining until the server-side close ends the stream
		}
	}()

	// Wait for the registration gate before cancelling, so the subscriber is
	// provably live on the comms bus when shutdown begins.
	select {
	case err := <-gate:
		if err != nil {
			t.Fatalf("subscriber gate: %v", err)
		}
	case <-timeAfter():
		t.Fatal("subscriber never received the seeded ChannelChanged")
	}

	// Shutdown with the subscriber still live: Serve must drain and return nil
	// within the deadline — the commsBus.Close() in the drain is what unblocks
	// the held-open SubscribeComms handler.
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve with a live SubscribeComms subscriber = %v, want nil (drain must close commsBus)", err)
		}
	case <-timeAfter():
		t.Fatal("Serve did not return after ctx cancel with a live SubscribeComms subscriber (drain hung on the open stream)")
	}

	// The subscriber's stream ended because the server closed the comms bus.
	<-done
}
