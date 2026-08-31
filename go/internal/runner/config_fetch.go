//go:build unix

// The Runner-side FetchAgentConfig client: pull the fleet config bundle from the
// Server over the RunnerService connection and reassemble the server-streamed
// frames into one in-memory bundle. The first frame carries the version; every
// subsequent frame carries a tarball byte chunk, so a bundle larger than the
// connect/gRPC unary recv cap still rides the wire (RIG-1568 T3). The bundle
// bytes ride in memory only; the security caps (decompressed size, file count)
// are enforced downstream at unpack (T4's ConfigMaterializer), never here.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// AgentConfigBundle is the reassembled fleet config bundle: the content-hash
// version and the raw tarball bytes (empty on an unconfigured fleet or an
// if_version match). T4's ConfigMaterializer validates and unpacks Tarball; this
// package never inspects it.
type AgentConfigBundle struct {
	// Version is the bundle's content hash. Empty means the fleet has no config.
	Version string
	// Tarball is the raw bundle bytes, reassembled from the chunk frames in
	// receive order. Empty on the version-only fetch (if_version match) or an
	// unconfigured fleet.
	Tarball []byte
}

// errConfigStreamNoVersion is the fail-closed cause when the server-stream ends
// without the leading version frame the contract guarantees — a contract skew,
// not a valid empty bundle (an empty bundle is a version frame with an empty
// version and no chunks). Surfaced so the caller fails the fetch rather than
// materializing an unversioned bundle.
var errConfigStreamNoVersion = errors.New("FetchAgentConfig stream ended before the version frame")

// FetchAgentConfig fetches the fleet config bundle from the Server and
// reassembles the stream into one bundle. ifVersion, when non-empty, is the
// version the Runner already holds: on a match the Server ends the stream after
// the version frame with no chunks, so Tarball comes back empty (the version-only
// reconnect fetch, T6). A transport/authz failure surfaces as an error (never a
// silent empty bundle), so the caller can log it and recover on the next signal
// or reconnect.
//
// The contract is order-bearing: the FIRST frame MUST carry the version, and
// every later frame a chunk. A version frame arriving after a chunk, or a stream
// that ends before any version frame, is a contract skew and errors.
func (l *ServerLink) FetchAgentConfig(ctx context.Context, ifVersion string) (AgentConfigBundle, error) {
	stream, err := l.client.FetchAgentConfig(ctx, connect.NewRequest(&compassv1internal.FetchAgentConfigRequest{
		IfVersion: ifVersion,
	}))
	if err != nil {
		return AgentConfigBundle{}, fmt.Errorf("fetching agent config: %w", err)
	}
	// ServerStreamForClient must be closed to release the underlying response
	// body; Close also surfaces a stream error not seen via Receive.
	defer func() { _ = stream.Close() }()

	var (
		bundle     AgentConfigBundle
		gotVersion bool
	)
	for stream.Receive() {
		frameVariant := stream.Msg().GetFrame()
		if frameVariant == nil {
			// A frame with no variant set — the same contract skew as an
			// unrecognized variant; reject it identically.
			return AgentConfigBundle{}, errors.New("FetchAgentConfig stream sent an unrecognized frame variant")
		}
		switch frame := frameVariant.(type) {
		case *compassv1internal.FetchAgentConfigResponse_Version:
			if gotVersion {
				return AgentConfigBundle{}, errors.New("FetchAgentConfig stream sent a second version frame")
			}
			bundle.Version = frame.Version
			gotVersion = true
		case *compassv1internal.FetchAgentConfigResponse_Chunk:
			if !gotVersion {
				return AgentConfigBundle{}, errors.New("FetchAgentConfig stream sent a chunk before the version frame")
			}
			bundle.Tarball = append(bundle.Tarball, frame.Chunk...)
		default:
			// An unset/unknown frame variant — a contract skew.
			return AgentConfigBundle{}, errors.New("FetchAgentConfig stream sent an unrecognized frame variant")
		}
	}
	if err := stream.Err(); err != nil && !errors.Is(err, io.EOF) {
		return AgentConfigBundle{}, fmt.Errorf("receiving agent config stream: %w", err)
	}
	if !gotVersion {
		return AgentConfigBundle{}, errConfigStreamNoVersion
	}
	return bundle, nil
}
