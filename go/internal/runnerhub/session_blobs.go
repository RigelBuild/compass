//go:build unix

package runnerhub

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/agentmsg"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// SessionBlobStore is the tenant-scoped durable index and object read surface.
type SessionBlobStore interface {
	AccountTenant(ctx context.Context, account store.AccountID) (store.TenantID, error)
	PutSessionBlob(ctx context.Context, sessionID, sha256Hex string, data []byte) error
	ResumeSessionBlobs(ctx context.Context, sessionID string, account store.AccountID, sha256s []string) ([]store.SessionBlobRow, error)
	ReadSessionBlob(ctx context.Context, sessionID, sha256Hex string) ([]byte, error)
}

// maxResumeBlobBytes and maxResumeBlobs bound one resume fetch to a fixed budget.
const (
	maxResumeBlobBytes = 128 << 20
	maxResumeBlobs     = 256
)

var sessionBlobHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var errSessionBlobStoreUnavailable = errors.New("runnerhub: no session blob store wired")

// SetSessionBlobStore wires the durable session blob surface after construction.
func (h *Hub) SetSessionBlobStore(blobs SessionBlobStore) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessionBlobs = blobs
}

// RelaySessionBlob stores one verified blob for the Runner-owned live session.
func (h *Hub) RelaySessionBlob(ctx context.Context, runnerID string, req *compassv1internal.RelaySessionBlobRequest) (*compassv1internal.RelaySessionBlobResponse, error) {
	h.mu.Lock()
	blobs := h.sessionBlobs
	h.mu.Unlock()
	if blobs == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errSessionBlobStoreUnavailable)
	}
	if req == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("runnerhub: session blob request is required"))
	}
	sessionID := req.GetSessionId()
	tenantCtx, scoped := h.runnerSessionCtx(ctx, runnerID, sessionID)
	if !scoped {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("runnerhub: no agent account bound to session"))
	}
	if _, ok := h.accountForRunnerSession(tenantCtx, runnerID, sessionID); !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("runnerhub: no agent account bound to session"))
	}
	blob := req.GetBlob()
	if blob == nil || !sessionBlobHashPattern.MatchString(blob.GetSha256()) || len(blob.GetData()) == 0 || len(blob.GetData()) > agentmsg.MaxSessionBlobBytes {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("runnerhub: invalid session blob"))
	}
	if err := blobs.PutSessionBlob(tenantCtx, sessionID, blob.GetSha256(), blob.GetData()); err != nil {
		switch {
		case errors.Is(err, store.ErrFailedPrecondition):
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		case errors.Is(err, store.ErrInvalidArgument):
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		default:
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("storing session blob: %w", err))
		}
	}
	return &compassv1internal.RelaySessionBlobResponse{}, nil
}

// FetchSessionBlobs streams absent headers first, then indexed blob bodies by request order.
func (h *Hub) FetchSessionBlobs(ctx context.Context, runnerID string, req *compassv1internal.FetchSessionBlobsRequest, send func(*compassv1internal.FetchSessionBlobsResponse) error) error {
	h.mu.Lock()
	blobs := h.sessionBlobs
	h.mu.Unlock()
	if blobs == nil {
		return connect.NewError(connect.CodeUnavailable, errSessionBlobStoreUnavailable)
	}
	if req == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("runnerhub: session blob request is required"))
	}
	account, ok := h.AccountForContainer(runnerID, req.GetContainerName())
	if !ok {
		return connect.NewError(connect.CodePermissionDenied, errors.New("runnerhub: container is not provisioned on this runner"))
	}
	if req.GetSessionId() == "" || len(req.GetSha256()) == 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("runnerhub: session blob request requires hashes and a session id"))
	}
	hashes := req.GetSha256()
	if len(hashes) > maxResumeBlobs {
		hashes = hashes[:maxResumeBlobs]
	}
	for _, hash := range hashes {
		if !sessionBlobHashPattern.MatchString(hash) {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("runnerhub: invalid session blob hash"))
		}
	}
	tenant, err := blobs.AccountTenant(store.WithSystemRole(ctx), account)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("resolving session blob tenant: %w", err))
	}
	tenantCtx := store.WithTenant(store.WithoutSystemRole(ctx), tenant)
	rows, err := blobs.ResumeSessionBlobs(tenantCtx, req.GetSessionId(), account, hashes)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("listing session blobs: %w", err))
	}
	return h.streamResumeSessionBlobs(ctx, blobs, tenantCtx, req.GetSessionId(), hashes, rows, send)
}

func (h *Hub) streamResumeSessionBlobs(ctx context.Context, blobs SessionBlobStore, tenantCtx context.Context, sessionID string, hashes []string, rows []store.SessionBlobRow, send func(*compassv1internal.FetchSessionBlobsResponse) error) error {
	indexed := make(map[string]int64, len(rows))
	for _, row := range rows {
		indexed[row.SHA256] = row.SizeBytes
	}
	seen := make(map[string]struct{}, len(hashes))
	uniqueHashes := make([]string, 0, len(hashes))
	for _, hash := range hashes {
		if _, ok := seen[hash]; !ok {
			seen[hash] = struct{}{}
			uniqueHashes = append(uniqueHashes, hash)
		}
	}
	for _, hash := range uniqueHashes {
		if _, ok := indexed[hash]; ok {
			continue
		}
		if err := send(&compassv1internal.FetchSessionBlobsResponse{Frame: &compassv1internal.FetchSessionBlobsResponse_Header{Header: &compassv1internal.SessionBlobHeader{Sha256: hash, Absent: true}}}); err != nil {
			return err
		}
	}
	var totalBytes int64
	var count int
	for _, hash := range uniqueHashes {
		size, ok := indexed[hash]
		if !ok {
			continue
		}
		if count >= maxResumeBlobs || size > maxResumeBlobBytes-totalBytes {
			break
		}
		data, err := blobs.ReadSessionBlob(tenantCtx, sessionID, hash)
		if err != nil {
			if errors.Is(err, store.ErrFailedPrecondition) {
				return connect.NewError(connect.CodeFailedPrecondition, err)
			}
			h.log.WarnContext(ctx, "read session blob failed; skipping resume blob", "session_id", sessionID, "sha256", hash, "error", err)
			continue
		}
		if int64(len(data)) != size {
			h.log.WarnContext(ctx, "session blob size does not match index; skipping resume blob", "session_id", sessionID, "sha256", hash)
			continue
		}
		sizeBytes := uint64(size) //nolint:gosec // size matched len(data) above and is therefore nonnegative.
		if err := send(&compassv1internal.FetchSessionBlobsResponse{Frame: &compassv1internal.FetchSessionBlobsResponse_Header{Header: &compassv1internal.SessionBlobHeader{Sha256: hash, SizeBytes: sizeBytes}}}); err != nil {
			return err
		}
		for off := 0; off < len(data); off += configChunkBytes {
			end := min(off+configChunkBytes, len(data))
			if err := send(&compassv1internal.FetchSessionBlobsResponse{Frame: &compassv1internal.FetchSessionBlobsResponse_Chunk{Chunk: data[off:end]}}); err != nil {
				return err
			}
		}
		totalBytes += size
		count++
	}
	return nil
}
