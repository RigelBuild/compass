package store

import (
	"context"
	"fmt"
)

// PutTokenHash stores a token's SHA-256 hash with its subject (design.md:
// 1177-1179). The plaintext token is minted and returned to the caller once by
// the auth layer; only its hash is ever stored, so the store never holds a
// usable credential. A re-used hash is ErrConflict.
func (s *Store) PutTokenHash(ctx context.Context, hash [32]byte, subj Subject) error {
	if subj.ID == "" {
		return fmt.Errorf("%w: token subject id is required", ErrInvalidArgument)
	}
	if _, err := s.pool.Exec(ctx,
		"INSERT INTO tokens (hash, subject_kind, subject_id) VALUES ($1, $2, $3)",
		hash[:], int32(subj.Kind), subj.ID,
	); err != nil {
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
	var (
		kind      int32
		subjectID string
		revoked   bool
	)
	err := s.pool.QueryRow(ctx,
		"SELECT subject_kind, subject_id, revoked_at IS NOT NULL FROM tokens WHERE hash = $1",
		hash[:],
	).Scan(&kind, &subjectID, &revoked)
	if err != nil {
		if noRows(err) {
			return Subject{}, fmt.Errorf("%w: token hash", ErrNotFound)
		}
		return Subject{}, fmt.Errorf("store: resolve token hash: %w", err)
	}
	if revoked {
		return Subject{}, ErrTokenRevoked
	}
	return Subject{Kind: SubjectKind(kind), ID: subjectID}, nil
}

// RevokeToken marks a token hash revoked (design.md:1183). Idempotent: revoking
// an already-revoked token is a no-op success. Revoking a hash that was never
// issued is ErrNotFound, so a caller learns a bad revoke target rather than
// silently succeeding.
func (s *Store) RevokeToken(ctx context.Context, hash [32]byte) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE tokens SET revoked_at = now() WHERE hash = $1 AND revoked_at IS NULL",
		hash[:],
	)
	if err != nil {
		return fmt.Errorf("store: revoke token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either the hash is unknown, or it was already revoked. Distinguish so
		// an unknown target is an error but a repeat revoke is a no-op success.
		var exists bool
		if err := s.pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM tokens WHERE hash = $1)", hash[:],
		).Scan(&exists); err != nil {
			return fmt.Errorf("store: check token exists: %w", err)
		}
		if !exists {
			return fmt.Errorf("%w: token hash", ErrNotFound)
		}
	}
	return nil
}
