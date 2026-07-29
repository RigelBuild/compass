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
	"errors"
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
func TestServeShutdownWithLiveCommsSubscriberReturnsClean(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "compass.sock")

	serveCtx, cancel := context.WithCancel(context.Background())
	// Idempotent with the explicit cancel() on the happy path below: the gate's
	// failure paths t.Fatalf before reaching it, and without this Serve would
	// keep running against a live socket and pg pool for the rest of the binary.
	defer cancel()
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

	// Drive the stream on a goroutine: assert the snapshot-boundary frame, gate
	// on the seeded event (registration proof), then keep receiving so the
	// stream stays open on the comms bus until the server's shutdown drain
	// closes it. The gate therefore needs TWO frames, boundary then event. The
	// final Receive returning false (clean EOF) is what proves the server-side
	// close released it.
	gate := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Frame 1 on since_seq=0 is the snapshot boundary, sent unconditionally
		// ahead of any event (subscribe.go:72-80). Assert it rather than discard
		// it: a bare Receive() here would also go green against a server that
		// stopped sending the boundary at all, and that frame is ratified
		// contract (SEA-1333 OQ4).
		//
		// The boundary's documented discriminators are its zero seq and absent
		// payload (subscribe.go:220-224); both are asserted below. The other two
		// checks are weaker on purpose and are NOT discriminators — see each.
		if !stream.Receive() {
			gate <- fmt.Errorf("stream ended before the snapshot boundary (Err=%v)", stream.Err())
			return
		}
		boundary := stream.Msg()
		switch {
		// Discriminator 1: an event frame always carries a payload oneof.
		case boundary.GetPayload() != nil:
			gate <- fmt.Errorf("frame 1 carries payload %T, want the payload-less snapshot boundary", boundary.GetPayload())
			return
		// Discriminator 2: the boundary is a control frame at seq 0
		// (commsResyncSeq), never positioned in bus-seq space, where a real event
		// carries the nonzero seq stamped at publish (events.go:169-172, off a
		// nextSeq that starts at 1 — events.go:155 — so no event is ever seq 0).
		case boundary.GetSeq() != 0:
			gate <- fmt.Errorf("snapshot boundary seq = %d, want 0 (control frame, not positioned)", boundary.GetSeq())
			return
		// Not a discriminator — every event carries the epoch too
		// (subscribe.go:201). This is a field-stamping check: it catches a
		// boundary built with a literal 0 instead of sub.Epoch (subscribe.go:77).
		// A nonzero-but-wrong epoch gets caught below, against frame 2.
		case boundary.GetInstanceEpoch() == 0:
			gate <- errors.New("snapshot boundary instance_epoch = 0, want the per-boot nonce")
			return
		// A no-drift check on the empty-store head, NOT a presence check: the
		// getter returns 0 for an unset field, and this fixture's true head is
		// also 0 (it seeds a channel, never a message), so a server that dropped
		// the SnapshotSeq assignment entirely would still pass here. That is
		// deliberate scope, not an ordering constraint: this fixture is a
		// shutdown-drain regression, and pinning a nonzero head against a
		// populated store is subscribe_test.go:243-244's job, where
		// non_empty_ring already asserts it. Kept single-purpose.
		case boundary.GetSnapshotSeq() != 0:
			gate <- fmt.Errorf("snapshot boundary snapshot_seq = %d, want 0 (empty store: COALESCE(MAX(seq), 0))", boundary.GetSnapshotSeq())
			return
		}
		wantEpoch := boundary.GetInstanceEpoch()
		if !stream.Receive() {
			gate <- fmt.Errorf("stream ended before the seeded event (Err=%v)", stream.Err())
			return
		}
		if got := stream.Msg().GetChannelChanged().GetChannel().GetId(); got != wantChannelID {
			gate <- fmt.Errorf("first event channel id = %q, want the seeded %q", got, wantChannelID)
			return
		}
		// The boundary and the events after it come from one Bus, so they carry
		// one per-boot nonce: Publish stamps b.instanceEpoch (events.go:174) and
		// Subscription.Epoch returns the same field (events.go:242, :288). Both
		// read one immutable field of one Bus, so this is narrow by construction:
		// it catches a boundary stamped from a literal or from a different Bus —
		// which a != 0 test passes — and nothing wider. Compared by value,
		// captured before this Receive: connect reuses no message across Receives
		// (client_stream.go:184-195), but a uint64 copy makes that irrelevant
		// rather than a fact to re-verify.
		if got := stream.Msg().GetInstanceEpoch(); got != wantEpoch {
			gate <- fmt.Errorf("seeded event instance_epoch = %d, want the boundary's %d (one bus, one nonce)", got, wantEpoch)
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
		t.Fatal("subscriber never completed the boundary + seeded ChannelChanged handshake")
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
