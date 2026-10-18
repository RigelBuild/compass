//go:build pgtest && unix

package server

// Store-gated Serve-loop integration tests: Serve opens the store and bootstraps the
// admin before serving, so each needs a real Postgres. Behind the `pgtest && unix`
// tag so the default lane never runs them and never hangs on store.Open(""). Covers
// the socket-bound-and-served, clean-shutdown, and live-subscriber-shutdown contracts.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/pgtest"
)

func TestServeBindsSocketServesClientAndCleansUpOnCancel(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "compass.sock")

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	cfg := ServeConfig{
		SocketPath:  socketPath,
		Version:     "serve-test",
		DatabaseDSN: pgtest.RequireDSN(t),
	}
	provisionMasterKeyProvider(t, &cfg)
	provisionNats(t, &cfg)
	go func() {
		errCh <- Serve(ctx, cfg)
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
	resp, err := client.GetServerInfo(context.Background(), &compassv1.GetServerInfoRequest{})
	if err != nil {
		t.Fatalf("GetServerInfo over socket: %v", err)
	}
	if resp.GetVersion() != "serve-test" {
		t.Fatalf("Version = %q, want serve-test", resp.GetVersion())
	}
	if resp.GetApiVersion() != apiVersion {
		t.Fatalf("ApiVersion = %q, want %q", resp.GetApiVersion(), apiVersion)
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
	// A clean cancel must return nil; this harness cannot deterministically
	// trigger slow-client overruns or server self-exits.
	socketPath := filepath.Join(t.TempDir(), "compass.sock")

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	cfg := ServeConfig{
		SocketPath:  socketPath,
		Version:     "serve-test",
		DatabaseDSN: pgtest.RequireDSN(t),
	}
	provisionMasterKeyProvider(t, &cfg)
	provisionNats(t, &cfg)
	go func() {
		errCh <- Serve(ctx, cfg)
	}()

	// Gate on a successful RPC; the socket binds before store.Open completes.
	// Cancellation now exercises graceful drain instead of racing startup.
	waitListening(t, socketPath)
	if _, err := newUDSClient(t, socketPath).GetServerInfo(context.Background(), &compassv1.GetServerInfoRequest{}); err != nil {
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

// Holding SubscribeComms open must not prevent clean shutdown. HTTP server
// shutdown does not cancel streaming handlers; closing commsBus releases this
// one. The registration gate proves the stream is live before cancellation.
func TestServeShutdownWithLiveCommsSubscriberReturnsClean(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "compass.sock")

	serveCtx, cancel := context.WithCancel(context.Background())
	// Defer idempotent cancellation so every failure path stops Serve and releases
	// its socket and database pool.
	defer cancel()
	errCh := make(chan error, 1)
	cfg := ServeConfig{
		SocketPath:  socketPath,
		Version:     "serve-test",
		DatabaseDSN: pgtest.RequireDSN(t),
	}
	provisionMasterKeyProvider(t, &cfg)
	provisionNats(t, &cfg)
	go func() {
		errCh <- Serve(serveCtx, cfg)
	}()
	waitListening(t, socketPath)

	commsClient := newUDSCommsClient(t, socketPath)

	// CreateChannel makes the bootstrap admin its founding member, so it sees the event.
	created, err := commsClient.CreateChannel(context.Background(), &compassv1.CreateChannelRequest{
		Name: "shutdown-room", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
	})
	if err != nil {
		t.Fatalf("CreateChannel over socket: %v", err)
	}
	wantChannelID := created.GetChannel().GetId()

	// A separate client context lets server shutdown release the stream.
	// Receiving the seeded event proves the subscriber is live.
	subCtx, subCancel := context.WithCancel(t.Context())
	defer subCancel()
	stream, err := commsClient.SubscribeComms(subCtx, &compassv1.SubscribeCommsRequest{SinceSeq: 0})
	if err != nil {
		t.Fatalf("SubscribeComms over socket: %v", err)
	}
	defer func() { _ = stream.Close() }()

	// Assert the snapshot boundary and seeded event before holding the live tail.
	gate := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		first, err := stream.Receive()
		if err != nil {
			// Report only that the stream ended before its first frame.
			gate <- fmt.Errorf("stream ended before any frame (expected the snapshot boundary first): %w", err)
			return
		}
		if seq, payload := first.GetSeq(), first.GetPayload(); seq != 0 || payload != nil {
			gate <- fmt.Errorf("first frame = seq %d payload %T, want the snapshot boundary (seq 0, no payload)", seq, payload)
			return
		}
		second, err := stream.Receive()
		if err != nil {
			gate <- fmt.Errorf("stream ended before the seeded event: %w", err)
			return
		}
		if got := second.GetChannelChanged().GetChannel().GetId(); got != wantChannelID {
			gate <- fmt.Errorf("first event channel id = %q, want the seeded %q", got, wantChannelID)
			return
		}
		gate <- nil
		// Keep receiving until shutdown closes the comms bus.
		for {
			_, receiveErr := stream.Receive()
			if receiveErr != nil {
				if errors.Is(receiveErr, io.EOF) {
					break
				}
				t.Errorf("stream ended with receive error: %v", receiveErr)
				return
			}
		}
	}()

	// Wait for registration so shutdown starts with a live subscriber.
	select {
	case err := <-gate:
		if err != nil {
			t.Fatalf("subscriber gate: %v", err)
		}
	case <-timeAfter():
		t.Fatal("subscriber never received the seeded ChannelChanged")
	}

	// Serve must drain cleanly; closing commsBus releases the held stream.
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
	select {
	case <-done:
	case <-timeAfter():
		t.Fatal("subscriber stream never ended after the drain closed the comms bus")
	}
}
