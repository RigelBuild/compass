package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// fkViolationDB fails every Exec with a foreign-key violation, the error a
// grant INSERT returns when its user is deleted between SELECT and FK check.
type fkViolationDB struct{}

func (fkViolationDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, &pgconn.PgError{Code: pgForeignKeyViolation}
}

func (fkViolationDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected Query")
}

func (fkViolationDB) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("unexpected QueryRow")
}

func TestGrantForgeScopeMapsForeignKeyViolationToInvalidArgument(t *testing.T) {
	s := &Store{q: db.New(fkViolationDB{})}
	err := s.GrantForgeScope(context.Background(), ForgeScope{
		AccountID: "acct-gone", Provider: ForgeProviderGitHub, Host: "github.com", Repo: "o/r",
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("GrantForgeScope error = %v, want ErrInvalidArgument", err)
	}
}
