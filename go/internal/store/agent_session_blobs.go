package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/RigelBuild/compass/go/internal/agentmsg"
	"github.com/RigelBuild/compass/go/internal/store/db"
)

// SessionBlobRow describes one content-addressed image indexed for a session.
type SessionBlobRow struct {
	SHA256    string
	SizeBytes int64
}

// PutSessionBlob writes verified bytes before indexing them, so retries can repair
// an object-store write whose database insert did not commit.
func (s *Store) PutSessionBlob(ctx context.Context, sessionID, sha256Hex string, data []byte) error {
	if s.objectStore == nil {
		return fmt.Errorf("%w: session blob object store is not configured", ErrFailedPrecondition)
	}
	if sessionID == "" || len(data) == 0 || len(data) > agentmsg.MaxSessionBlobBytes || !validSessionBlobHash(sha256Hex) {
		return fmt.Errorf("%w: invalid session blob", ErrInvalidArgument)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != sha256Hex {
		return fmt.Errorf("%w: session blob digest does not match data", ErrInvalidArgument)
	}
	exists, err := s.q.SessionBlobRowExists(ctx, db.SessionBlobRowExistsParams{SessionID: sessionID, Sha256: sha256Hex})
	if err != nil {
		return fmt.Errorf("store: check session blob index: %w", err)
	}
	if exists {
		return nil
	}
	if err := s.objectStore.PutSegment(ctx, "blobs/"+sha256Hex, data); err != nil {
		return fmt.Errorf("store: put session blob: %w", err)
	}
	if err := s.q.InsertSessionBlob(ctx, db.InsertSessionBlobParams{
		SessionID: sessionID,
		Sha256:    sha256Hex,
		SizeBytes: int64(len(data)),
	}); err != nil {
		return fmt.Errorf("store: index session blob: %w", err)
	}
	return nil
}

// ResumeSessionBlobs returns requested hashes indexed for the owned session.
func (s *Store) ResumeSessionBlobs(ctx context.Context, sessionID string, account AccountID, sha256s []string) ([]SessionBlobRow, error) {
	if sessionID == "" || account == "" || len(sha256s) == 0 {
		return nil, nil
	}
	rows, err := s.q.ResumeSessionBlobs(ctx, db.ResumeSessionBlobsParams{
		SessionID:      sessionID,
		AgentAccountID: string(account),
		Column3:        sha256s,
	})
	if err != nil {
		return nil, fmt.Errorf("store: resume session blobs: %w", err)
	}
	out := make([]SessionBlobRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, SessionBlobRow{SHA256: row.Sha256, SizeBytes: row.SizeBytes})
	}
	return out, nil
}

// ReadSessionBlob checks the tenant-scoped index before reaching a global object key.
func (s *Store) ReadSessionBlob(ctx context.Context, sessionID, sha256Hex string) ([]byte, error) {
	if sessionID == "" || !validSessionBlobHash(sha256Hex) {
		return nil, fmt.Errorf("%w: invalid session blob selector", ErrInvalidArgument)
	}
	exists, err := s.q.SessionBlobRowExists(ctx, db.SessionBlobRowExistsParams{SessionID: sessionID, Sha256: sha256Hex})
	if err != nil {
		return nil, fmt.Errorf("store: check session blob index: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: session blob %q for session %q", ErrNotFound, sha256Hex, sessionID)
	}
	if s.objectStore == nil {
		return nil, fmt.Errorf("%w: session blob object store is not configured", ErrFailedPrecondition)
	}
	data, err := s.objectStore.GetSegment(ctx, "blobs/"+sha256Hex)
	if err != nil {
		return nil, fmt.Errorf("store: read session blob: %w", err)
	}
	return data, nil
}

func validSessionBlobHash(hash string) bool {
	if len(hash) != sha256.Size*2 {
		return false
	}
	for _, c := range hash {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
