package store

import (
	"context"
	"fmt"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// PutTokenHash stores a token's SHA-256 hash with its subject (design.md:
// 1177-1179). The plaintext token is minted and returned to the caller once by
// the auth layer; only its hash is ever stored, so the store never holds a
// usable credential. A re-used hash is ErrConflict.
func (s *Store) PutTokenHash(ctx context.Context, hash [32]byte, subj Subject) error {
	if subj.ID == "" {
		return fmt.Errorf("%w: token subject id is required", ErrInvalidArgument)
	}
	if err := s.q.InsertTokenHash(ctx, db.InsertTokenHashParams{
		Hash:        hash[:],
		SubjectKind: int16(subj.Kind), //nolint:gosec // G115: SubjectKind is a CHECK-constrained 0/1 enum (tokens.subject_kind), always within int16
		SubjectID:   subj.ID,
	}); err != nil {
		if pgErrIs(err, pgUniqueViolation) {
			return fmt.Errorf("%w: token hash already stored", ErrConflict)
		}
		return fmt.Errorf("store: put token hash: %w", err)
	}
	return nil
}

// ResolveTokenHash returns the subject a token hash authenticates, with its
// kind, so a door can reject a cross-kind token (design.md:1180-1183). A hash
// that was never issued is ErrNotFound; one that was issued then revoked is
// ErrTokenRevoked — the two are distinct so the door can tell a withdrawn
// credential from an unknown one.
func (s *Store) ResolveTokenHash(ctx context.Context, hash [32]byte) (Subject, error) {
	row, err := s.q.ResolveTokenHash(ctx, hash[:])
	if err != nil {
		if noRows(err) {
			return Subject{}, fmt.Errorf("%w: token hash", ErrNotFound)
		}
		return Subject{}, fmt.Errorf("store: resolve token hash: %w", err)
	}
	if row.Revoked {
		return Subject{}, ErrTokenRevoked
	}
	return Subject{Kind: SubjectKind(row.SubjectKind), ID: row.SubjectID}, nil
}

// RevokeToken marks a token hash revoked (design.md:1183). Idempotent: revoking
// an already-revoked token is a no-op success. Revoking a hash that was never
// issued is ErrNotFound, so a caller learns a bad revoke target rather than
// silently succeeding.
func (s *Store) RevokeToken(ctx context.Context, hash [32]byte) error {
	affected, err := s.q.RevokeToken(ctx, hash[:])
	if err != nil {
		return fmt.Errorf("store: revoke token: %w", err)
	}
	if affected == 0 {
		// Either the hash is unknown, or it was already revoked. Distinguish so
		// an unknown target is an error but a repeat revoke is a no-op success.
		exists, err := s.q.TokenHashExists(ctx, hash[:])
		if err != nil {
			return fmt.Errorf("store: check token exists: %w", err)
		}
		if !exists {
			return fmt.Errorf("%w: token hash", ErrNotFound)
		}
	}
	return nil
}
