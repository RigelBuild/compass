//go:build unix

package runner

// The Runner-side FetchAgentConfig client: ServerLink.FetchAgentConfig calls the
// server-streaming RunnerService RPC and reassembles the frames into one bundle —
// the first frame the version, the rest tarball chunks. Every case pins a
// contract a plausible bug would break: a dropped chunk loses config bytes; a
// mis-ordered frame (chunk before version, or a second version) is a contract
// skew the client must reject, not silently accept; an RPC failure must surface
// loud so the caller recovers on the next signal.

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// fakeConfigFetchServer is a RunnerService handler whose FetchAgentConfig streams
// a scripted sequence of frames (and records the requested if_version), so the
// client's reassembly is asserted over the real transport without a Server. When
// err is set the RPC fails before any frame.
type fakeConfigFetchServer struct {
	compassv1internalconnect.UnimplementedRunnerServiceHandler
	frames        []*compassv1internal.FetchAgentConfigResponse
	lastIfVersion string
	err           error
}

func (f *fakeConfigFetchServer) FetchAgentConfig(_ context.Context, req *connect.Request[compassv1internal.FetchAgentConfigRequest], stream *connect.ServerStream[compassv1internal.FetchAgentConfigResponse]) error {
	f.lastIfVersion = req.Msg.GetIfVersion()
	if f.err != nil {
		return f.err
	}
	for _, frame := range f.frames {
		if err := stream.Send(frame); err != nil {
			return err
		}
	}
	return nil
}

func versionFrame(v string) *compassv1internal.FetchAgentConfigResponse {
	return &compassv1internal.FetchAgentConfigResponse{Frame: &compassv1internal.FetchAgentConfigResponse_Version{Version: v}}
}

func chunkFrame(b []byte) *compassv1internal.FetchAgentConfigResponse {
	return &compassv1internal.FetchAgentConfigResponse{Frame: &compassv1internal.FetchAgentConfigResponse_Chunk{Chunk: b}}
}

// TestFetchAgentConfigReassemblesVersionAndChunks pins the core contract: the
// version frame sets Version and every subsequent chunk frame is appended in
// receive order, so the reassembled Tarball is the exact concatenation. A bug
// that dropped a chunk, reordered them, or ignored the version reds.
func TestFetchAgentConfigReassemblesVersionAndChunks(t *testing.T) {
	server := &fakeConfigFetchServer{frames: []*compassv1internal.FetchAgentConfigResponse{
		versionFrame("v-abc"),
		chunkFrame([]byte("hello ")),
		chunkFrame([]byte("world")),
	}}
	link := newLink(newRunnerServiceServer(t, server))

	got, err := link.FetchAgentConfig(context.Background(), "")
	if err != nil {
		t.Fatalf("FetchAgentConfig = %v, want nil", err)
	}
	if got.Version != "v-abc" {
		t.Fatalf("Version = %q, want v-abc", got.Version)
	}
	if string(got.Tarball) != "hello world" {
		t.Fatalf("Tarball = %q, want %q", got.Tarball, "hello world")
	}
	if server.lastIfVersion != "" {
		t.Fatalf("server saw if_version %q, want empty on a cold fetch", server.lastIfVersion)
	}
}

// TestFetchAgentConfigVersionOnly pins the if_version reconnect path: the Runner
// sends the version it holds, and on a match the Server streams only the version
// frame (no chunks), so Tarball comes back empty. A bug that treated a
// no-chunk stream as an error, or synthesized a bundle, reds.
func TestFetchAgentConfigVersionOnly(t *testing.T) {
	server := &fakeConfigFetchServer{frames: []*compassv1internal.FetchAgentConfigResponse{
		versionFrame("v-held"),
	}}
	link := newLink(newRunnerServiceServer(t, server))

	got, err := link.FetchAgentConfig(context.Background(), "v-held")
	if err != nil {
		t.Fatalf("FetchAgentConfig = %v, want nil", err)
	}
	if got.Version != "v-held" {
		t.Fatalf("Version = %q, want v-held", got.Version)
	}
	if len(got.Tarball) != 0 {
		t.Fatalf("Tarball len = %d, want 0 on a version-only fetch", len(got.Tarball))
	}
	if server.lastIfVersion != "v-held" {
		t.Fatalf("server saw if_version %q, want v-held (the held version is sent)", server.lastIfVersion)
	}
}

// TestFetchAgentConfigEmptyVersionUnconfigured pins the unconfigured-fleet path:
// a version frame with an empty version and no chunks is a VALID empty bundle
// (the Runner materializes an empty dir), not an error. A bug that errored on the
// empty version reds.
func TestFetchAgentConfigEmptyVersionUnconfigured(t *testing.T) {
	server := &fakeConfigFetchServer{frames: []*compassv1internal.FetchAgentConfigResponse{
		versionFrame(""),
	}}
	link := newLink(newRunnerServiceServer(t, server))

	got, err := link.FetchAgentConfig(context.Background(), "")
	if err != nil {
		t.Fatalf("FetchAgentConfig = %v, want nil (empty config is valid)", err)
	}
	if got.Version != "" {
		t.Fatalf("Version = %q, want empty", got.Version)
	}
	if len(got.Tarball) != 0 {
		t.Fatalf("Tarball len = %d, want 0", len(got.Tarball))
	}
}

// TestFetchAgentConfigNoVersionFrameErrs pins the contract skew: a stream that
// ends without any version frame is not a valid empty bundle (that is a version
// frame with an empty version) — the client must error rather than materialize an
// unversioned bundle.
func TestFetchAgentConfigNoVersionFrameErrs(t *testing.T) {
	server := &fakeConfigFetchServer{frames: nil}
	link := newLink(newRunnerServiceServer(t, server))

	_, err := link.FetchAgentConfig(context.Background(), "")
	if err == nil {
		t.Fatal("FetchAgentConfig on a version-less stream = nil, want an error")
	}
}

// TestFetchAgentConfigChunkBeforeVersionErrs pins the ordering contract: a chunk
// that arrives before the version frame is a contract skew — the client rejects
// it rather than appending bytes it cannot version.
func TestFetchAgentConfigChunkBeforeVersionErrs(t *testing.T) {
	server := &fakeConfigFetchServer{frames: []*compassv1internal.FetchAgentConfigResponse{
		chunkFrame([]byte("orphan")),
	}}
	link := newLink(newRunnerServiceServer(t, server))

	_, err := link.FetchAgentConfig(context.Background(), "")
	if err == nil {
		t.Fatal("FetchAgentConfig with a chunk before the version = nil, want an error")
	}
}

// TestFetchAgentConfigSecondVersionFrameErrs pins the ordering contract's other
// half: a second version frame (after the leading one) is a contract skew — the
// client rejects it rather than silently overwriting the version it already read.
// A regression that dropped the gotVersion guard would ship green without this.
func TestFetchAgentConfigSecondVersionFrameErrs(t *testing.T) {
	server := &fakeConfigFetchServer{frames: []*compassv1internal.FetchAgentConfigResponse{
		versionFrame("v1"),
		versionFrame("v2"),
	}}
	link := newLink(newRunnerServiceServer(t, server))

	_, err := link.FetchAgentConfig(context.Background(), "")
	if err == nil {
		t.Fatal("FetchAgentConfig with a second version frame = nil, want an error")
	}
	if !strings.Contains(err.Error(), "second version frame") {
		t.Fatalf("FetchAgentConfig second-version error = %q, want it to name the second version frame", err)
	}
}

// TestFetchAgentConfigUnknownFrameErrs pins the frame-variant contract: a
// response whose oneof is unset (neither version nor chunk) is a contract skew —
// the client rejects it rather than treating an empty frame as a valid no-op. A
// regression that dropped the default guard would ship green without this.
func TestFetchAgentConfigUnknownFrameErrs(t *testing.T) {
	server := &fakeConfigFetchServer{frames: []*compassv1internal.FetchAgentConfigResponse{
		versionFrame("v1"),
		{}, // Frame oneof unset — an unrecognized variant.
	}}
	link := newLink(newRunnerServiceServer(t, server))

	_, err := link.FetchAgentConfig(context.Background(), "")
	if err == nil {
		t.Fatal("FetchAgentConfig with an unset frame variant = nil, want an error")
	}
	if !strings.Contains(err.Error(), "unrecognized frame variant") {
		t.Fatalf("FetchAgentConfig unknown-frame error = %q, want it to name the unrecognized variant", err)
	}
}

// TestFetchAgentConfigPropagatesError: an RPC failure surfaces as an error, never
// a silent empty bundle — a wiring/authz failure must be loud so the caller
// recovers on the next signal or reconnect.
func TestFetchAgentConfigPropagatesError(t *testing.T) {
	server := &fakeConfigFetchServer{err: connect.NewError(connect.CodeFailedPrecondition, errTestDenied)}
	link := newLink(newRunnerServiceServer(t, server))

	_, err := link.FetchAgentConfig(context.Background(), "")
	if err == nil {
		t.Fatal("FetchAgentConfig on a failing RPC = nil, want an error")
	}
}
