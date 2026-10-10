//go:build unix

// The Runner-side FetchAgentConfig client: pull the fleet config bundle from the
// Server and reassemble the streamed frames into one in-memory bundle (first
// frame version, rest tarball chunks, so a bundle over the unary recv cap still
// rides). Security caps (size, file count) are enforced downstream at unpack.
package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

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

const (
	maxResumeBlobBytes     = 128 << 20
	maxResumeBlobCount     = 256
	resumeBlobFetchTimeout = 30 * time.Second
)

// SessionBlob is one content-addressed blob verified against SHA256 before return.
type SessionBlob struct {
	SHA256 string
	Data   []byte
}

// SessionBlobFetch separates available files from Server-confirmed absences.
type SessionBlobFetch struct {
	Blobs  []SessionBlob
	Absent map[string]struct{}
}

// FetchSessionBlobs streams blobs for a resumed session under the fixed budget.
func (l *ServerLink) FetchSessionBlobs(ctx context.Context, containerName, sessionID string, sha256s []string) (result SessionBlobFetch, err error) {
	fetchCtx, cancel := context.WithTimeout(ctx, resumeBlobFetchTimeout)
	defer cancel()
	stream, err := l.client.FetchSessionBlobs(fetchCtx, connect.NewRequest(&compassv1internal.FetchSessionBlobsRequest{
		ContainerName: containerName,
		SessionId:     sessionID,
		Sha256:        sha256s,
	}))
	if err != nil {
		return SessionBlobFetch{}, fmt.Errorf("fetching session blobs: %w", err)
	}
	defer func() {
		if closeErr := stream.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("closing session blobs stream: %w", closeErr))
		}
	}()

	result = SessionBlobFetch{Absent: make(map[string]struct{})}
	var (
		current *compassv1internal.SessionBlobHeader
		data    []byte
		budget  uint64
		count   int
		drop    bool
	)
	finish := func() {
		if current == nil || drop || current.GetAbsent() || uint64(len(data)) != current.GetSizeBytes() {
			return
		}
		digest := sha256.Sum256(data)
		got := hex.EncodeToString(digest[:])
		if got != current.GetSha256() {
			return
		}
		result.Blobs = append(result.Blobs, SessionBlob{SHA256: got, Data: data})
	}
	for stream.Receive() {
		frameVariant := stream.Msg().GetFrame()
		if frameVariant == nil {
			return SessionBlobFetch{}, errors.New("FetchSessionBlobs stream sent an unrecognized frame variant")
		}
		switch frame := frameVariant.(type) {
		case *compassv1internal.FetchSessionBlobsResponse_Header:
			finish()
			current = frame.Header
			if current == nil {
				return SessionBlobFetch{}, errors.New("FetchSessionBlobs stream sent a nil header")
			}
			data = nil
			drop = false
			if current.GetAbsent() {
				result.Absent[current.GetSha256()] = struct{}{}
				continue
			}
			if count >= maxResumeBlobCount || current.GetSizeBytes() > uint64(maxResumeBlobBytes)-budget {
				drop = true
				continue
			}
			budget += current.GetSizeBytes()
			count++
			data = nil
		case *compassv1internal.FetchSessionBlobsResponse_Chunk:
			if current == nil {
				return SessionBlobFetch{}, errors.New("FetchSessionBlobs stream sent a chunk before a header")
			}
			if !drop && !current.GetAbsent() {
				data = append(data, frame.Chunk...)
				if uint64(len(data)) > current.GetSizeBytes() {
					drop = true
					data = nil
				}
			}
		default:
			return SessionBlobFetch{}, errors.New("FetchSessionBlobs stream sent an unrecognized frame variant")
		}
	}
	finish()
	if err := stream.Err(); err != nil && !errors.Is(err, io.EOF) {
		return SessionBlobFetch{}, fmt.Errorf("receiving session blobs stream: %w", err)
	}
	return result, nil
}
