//go:build unix

package runnerhub

// FetchAgentConfig handler: the RunnerService server-streaming surface the Runner
// dials out to for the fleet config bundle. Every case pins a behavior a
// plausible bug would break, driven over the real wire through
// newMountedH2CServerWithConfig + the raw generated client:
//   - a handler built with NO config store fails CodeFailedPrecondition (the
//     no-config-surface server the Runner tolerates), distinct from a transport
//     fault — never a silent empty stream.
//   - a declared bundle streams as a version frame FIRST, then the tarball in
//     chunks the client reassembles byte-identically (including a bundle larger
//     than one chunk).
//   - an unconfigured fleet (store.ErrNotFound) streams an EMPTY version frame and
//     no chunks — a valid state, not an error.
//   - an if_version MATCH ends the stream after the version frame (no chunks).

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"connectrpc.com/connect"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// fakeConfigStore is a hand-written AgentConfigStore: CurrentAgentConfig returns
// a fixed (version, bundle) or a scripted error, and records that it was called.
type fakeConfigStore struct {
	version string
	bundle  []byte
	err     error
	calls   int
}

func (f *fakeConfigStore) CurrentAgentConfig(_ context.Context) (string, []byte, error) {
	f.calls++
	if f.err != nil {
		return "", nil, f.err
	}
	return f.version, f.bundle, nil
}

// drainConfigStream reads the whole FetchAgentConfig stream into (version, bundle)
// the way the real Runner client does: the first version frame, then chunks in
// receive order. Fails the test on a chunk before the version frame.
func drainConfigStream(t *testing.T, stream *connect.ServerStreamForClient[compassv1internal.FetchAgentConfigResponse]) (string, []byte) {
	t.Helper()
	var (
		version    string
		bundle     []byte
		gotVersion bool
	)
	for stream.Receive() {
		switch frame := stream.Msg().GetFrame().(type) {
		case *compassv1internal.FetchAgentConfigResponse_Version:
			version = frame.Version
			gotVersion = true
		case *compassv1internal.FetchAgentConfigResponse_Chunk:
			if !gotVersion {
				t.Fatal("received a chunk frame before the version frame")
			}
			bundle = append(bundle, frame.Chunk...)
		}
	}
	if err := stream.Err(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("stream error: %v", err)
	}
	if !gotVersion {
		t.Fatal("stream ended without a version frame")
	}
	return version, bundle
}

// TestFetchAgentConfigNoConfigStoreFailsPrecondition pins the no-config-surface
// posture: a handler built with nil configStore fails CodeFailedPrecondition —
// the Runner reads that as "no config to inject" and provisions anyway, distinct
// from the CodeUnavailable of a transient transport fault.
func TestFetchAgentConfigNoConfigStoreFailsPrecondition(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	url := newMountedH2CServerWithConfig(t, hub, runnerResolverForFetch().resolve, nil)
	client := newRawRunnerClient(t, url, "runner-tok")

	stream, err := client.FetchAgentConfig(context.Background(), connect.NewRequest(&compassv1internal.FetchAgentConfigRequest{}))
	if err == nil {
		// Server-streaming: the error may surface on first Receive, not on open.
		stream.Receive()
		err = stream.Err()
		_ = stream.Close()
	}
	if err == nil {
		t.Fatal("FetchAgentConfig with no config store = nil, want CodeFailedPrecondition")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("FetchAgentConfig no-store code = %v, want FailedPrecondition", got)
	}
}

// TestFetchAgentConfigStreamsVersionThenChunks pins the happy path over the wire:
// a declared bundle arrives as a version frame first, then the tarball reassembled
// byte-identically — including a bundle LARGER than one chunk, so the chunking
// loop is exercised (a bug that sent one frame, or dropped the tail, reds).
func TestFetchAgentConfigStreamsVersionThenChunks(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	// A bundle spanning multiple chunk frames.
	want := bytes.Repeat([]byte("compass-config-"), configChunkBytes/10)
	cfg := &fakeConfigStore{version: "v-1", bundle: want}
	url := newMountedH2CServerWithConfig(t, hub, runnerResolverForFetch().resolve, cfg)
	client := newRawRunnerClient(t, url, "runner-tok")

	stream, err := client.FetchAgentConfig(context.Background(), connect.NewRequest(&compassv1internal.FetchAgentConfigRequest{}))
	if err != nil {
		t.Fatalf("FetchAgentConfig = %v, want nil", err)
	}
	gotVersion, gotBundle := drainConfigStream(t, stream)
	_ = stream.Close()
	if gotVersion != "v-1" {
		t.Fatalf("version = %q, want v-1", gotVersion)
	}
	if !bytes.Equal(gotBundle, want) {
		t.Fatalf("reassembled bundle (%d bytes) != want (%d bytes)", len(gotBundle), len(want))
	}
	if cfg.calls != 1 {
		t.Fatalf("CurrentAgentConfig called %d times, want 1", cfg.calls)
	}
}

// TestFetchAgentConfigUnconfiguredEmptyVersion pins the ErrNotFound path: an
// unconfigured fleet streams an EMPTY version frame with no chunks — a valid
// state (the Runner materializes an empty dir), never an error.
func TestFetchAgentConfigUnconfiguredEmptyVersion(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	cfg := &fakeConfigStore{err: store.ErrNotFound}
	url := newMountedH2CServerWithConfig(t, hub, runnerResolverForFetch().resolve, cfg)
	client := newRawRunnerClient(t, url, "runner-tok")

	stream, err := client.FetchAgentConfig(context.Background(), connect.NewRequest(&compassv1internal.FetchAgentConfigRequest{}))
	if err != nil {
		t.Fatalf("FetchAgentConfig = %v, want nil (unconfigured is valid)", err)
	}
	gotVersion, gotBundle := drainConfigStream(t, stream)
	_ = stream.Close()
	if gotVersion != "" {
		t.Fatalf("version = %q, want empty on an unconfigured fleet", gotVersion)
	}
	if len(gotBundle) != 0 {
		t.Fatalf("bundle len = %d, want 0", len(gotBundle))
	}
}

// TestFetchAgentConfigStoreErrorMapsToInternal pins that a real store failure
// (not ErrNotFound) surfaces as CodeInternal — a genuine fault the Runner must
// not read as "no config".
func TestFetchAgentConfigStoreErrorMapsToInternal(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	cfg := &fakeConfigStore{err: errors.New("db boom")}
	url := newMountedH2CServerWithConfig(t, hub, runnerResolverForFetch().resolve, cfg)
	client := newRawRunnerClient(t, url, "runner-tok")

	stream, err := client.FetchAgentConfig(context.Background(), connect.NewRequest(&compassv1internal.FetchAgentConfigRequest{}))
	if err == nil {
		stream.Receive()
		err = stream.Err()
		_ = stream.Close()
	}
	if err == nil {
		t.Fatal("FetchAgentConfig on a store error = nil, want CodeInternal")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("store-error code = %v, want Internal", got)
	}
}

// TestFetchAgentConfigIfVersionMatchStreamsVersionOnly pins the reconnect
// optimization: when if_version matches the current version, the Server ends the
// stream after the version frame with NO chunks — the version-only fetch. A bug
// that streamed the bundle anyway (wasting the reconnect) reds.
func TestFetchAgentConfigIfVersionMatchStreamsVersionOnly(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	cfg := &fakeConfigStore{version: "v-held", bundle: []byte("should-not-be-sent")}
	url := newMountedH2CServerWithConfig(t, hub, runnerResolverForFetch().resolve, cfg)
	client := newRawRunnerClient(t, url, "runner-tok")

	stream, err := client.FetchAgentConfig(context.Background(), connect.NewRequest(&compassv1internal.FetchAgentConfigRequest{IfVersion: "v-held"}))
	if err != nil {
		t.Fatalf("FetchAgentConfig = %v, want nil", err)
	}
	gotVersion, gotBundle := drainConfigStream(t, stream)
	_ = stream.Close()
	if gotVersion != "v-held" {
		t.Fatalf("version = %q, want v-held", gotVersion)
	}
	if len(gotBundle) != 0 {
		t.Fatalf("bundle len = %d, want 0 on an if_version match", len(gotBundle))
	}
}

// TestFetchAgentConfigIfVersionMismatchStreamsBundle pins the complement: an
// if_version that does NOT match the current version streams the full bundle, so
// a stale Runner reconnecting with an old version still gets the new bytes.
func TestFetchAgentConfigIfVersionMismatchStreamsBundle(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	cfg := &fakeConfigStore{version: "v-new", bundle: []byte("new-bytes")}
	url := newMountedH2CServerWithConfig(t, hub, runnerResolverForFetch().resolve, cfg)
	client := newRawRunnerClient(t, url, "runner-tok")

	stream, err := client.FetchAgentConfig(context.Background(), connect.NewRequest(&compassv1internal.FetchAgentConfigRequest{IfVersion: "v-old"}))
	if err != nil {
		t.Fatalf("FetchAgentConfig = %v, want nil", err)
	}
	gotVersion, gotBundle := drainConfigStream(t, stream)
	_ = stream.Close()
	if gotVersion != "v-new" {
		t.Fatalf("version = %q, want v-new", gotVersion)
	}
	if string(gotBundle) != "new-bytes" {
		t.Fatalf("bundle = %q, want new-bytes", gotBundle)
	}
}
