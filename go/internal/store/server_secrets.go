package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// Reserved server-secret name prefixes. A `server_secrets` row REQUIRES one of
// these, and the user-facing `secrets` path REJECTS them — so the two
// keyspaces are disjoint by NAME, and a reserved-prefix name can only ever
// live in `server_secrets` while an unprefixed name can only ever live in
// `secrets`. This is the F1 structural partition (design record D6): a pure
// string check at each door, no cross-table read and no transaction spanning
// both tables.
const (
	// ServerSecretPrefix marks a deployment-owned server secret (the six forge
	// secrets: App PEMs, webhook secrets, Linear credentials).
	ServerSecretPrefix = "SERVER_"
	// GatewayCredentialsPrefix marks the master-key family for the
	// gateway_credentials at-rest encryption.
	GatewayCredentialsPrefix = "GATEWAY_CREDENTIALS_"
)

// serverSecretPrefixes is the reserved set both doors check against.
var serverSecretPrefixes = [...]string{ServerSecretPrefix, GatewayCredentialsPrefix}

// HasServerSecretPrefix reports whether name carries a reserved server-secret
// prefix. Both secret doors consult it: `server_secrets` requires it, and the
// user-facing `secrets` path rejects it.
func HasServerSecretPrefix(name string) bool {
	for _, p := range serverSecretPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ServerSecretDeclaration is a names-only server-secret registry row. It
// carries no value (the value lives in the SecretSpec provider) and no
// delivery/kind — a server secret is never container-delivered and never
// reaches the materializer.
type ServerSecretDeclaration struct {
	Name string
	// DeclaredBy is the account that declared this secret, or "" for a
	// server-provisioned row (the master key, declared at boot with no human
	// actor).
	DeclaredBy AccountID
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// DeclareServerSecret adds a names-only row to the SEPARATE server-secret
// registry, mirroring DeclareSecret minus delivery/kind/provider/host. It
// stores NO value.
//
// name is validated against SecretSpec's env-var-name grammar AND is REQUIRED
// to carry a reserved server-secret prefix — so no writer (the admin RPC, or
// any future second writer) can create a `server_secrets` row under a name the
// user keyspace owns. A duplicate name is ErrConflict.
//
// actor is the declaring account, or "" for the server-provisioned path (the
// boot master-key provisioner), which writes declared_by as NULL rather than
// attributing the row to a human. A non-empty unknown actor is
// ErrInvalidArgument (the declared_by FK).
func (s *Store) DeclareServerSecret(ctx context.Context, actor AccountID, name string) error {
	if !secretNamePattern.MatchString(name) {
		return fmt.Errorf("%w: server secret name %q must match %s", ErrInvalidArgument, name, secretNamePattern.String())
	}
	if !HasServerSecretPrefix(name) {
		return fmt.Errorf("%w: server secret name %q must carry a reserved prefix (%s or %s)",
			ErrInvalidArgument, name, ServerSecretPrefix, GatewayCredentialsPrefix)
	}
	// declared_by is NULL for the server-provisioned path: honest provenance
	// for a row no human declared.
	declaredBy := pgtype.Text{}
	if actor != "" {
		declaredBy = pgtype.Text{String: string(actor), Valid: true}
	}
	if err := s.q.InsertServerSecret(ctx, db.InsertServerSecretParams{
		Name:       name,
		DeclaredBy: declaredBy,
	}); err != nil {
		if pgErrIs(err, pgUniqueViolation) {
			return fmt.Errorf("%w: server secret %q already declared", ErrConflict, name)
		}
		if pgErrIs(err, pgForeignKeyViolation) {
			return fmt.Errorf("%w: declaring account %q does not exist", ErrInvalidArgument, actor)
		}
		return fmt.Errorf("store: declare server secret: %w", err)
	}
	return nil
}

// DeleteServerSecretDeclaration removes a server-secret registry row, mirroring
// DeleteSecretDeclaration. It does NOT touch the provider value. An absent name
// is ErrNotFound.
func (s *Store) DeleteServerSecretDeclaration(ctx context.Context, name string) error {
	rows, err := s.q.DeleteServerSecret(ctx, name)
	if err != nil {
		return fmt.Errorf("store: delete server secret declaration: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("%w: server secret %q not declared", ErrNotFound, name)
	}
	return nil
}

// DeclaredServerSecrets returns the whole server-secret registry, ordered by
// name.
func (s *Store) DeclaredServerSecrets(ctx context.Context) ([]ServerSecretDeclaration, error) {
	rows, err := s.q.DeclaredServerSecrets(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: declared server secrets: %w", err)
	}
	out := make([]ServerSecretDeclaration, 0, len(rows))
	for _, r := range rows {
		out = append(out, ServerSecretDeclaration{
			Name:       r.Name,
			DeclaredBy: AccountID(r.DeclaredBy.String),
			CreatedAt:  r.CreatedAt.Time,
			UpdatedAt:  r.UpdatedAt.Time,
		})
	}
	return out, nil
}

// ServerDeclaredSecrets is the thin store view the SERVER SpecResolver reads.
// Its DeclaredSecrets method satisfies the resolver's one-method `declarations`
// interface (secrets/resolver.go), mapping server-secret rows onto
// SecretDeclaration with a generic kind and zero delivery — the resolver uses
// only the NAME to build its manifest, so NewSpecResolver is reused UNCHANGED
// against a different table.
type ServerDeclaredSecrets struct {
	Store *Store
}

// DeclaredSecrets reads the server-secret registry, not the user one.
func (v ServerDeclaredSecrets) DeclaredSecrets(ctx context.Context) ([]SecretDeclaration, error) {
	rows, err := v.Store.DeclaredServerSecrets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SecretDeclaration, 0, len(rows))
	for _, r := range rows {
		out = append(out, SecretDeclaration{
			Name:       r.Name,
			Kind:       SecretKindGeneric,
			DeclaredBy: r.DeclaredBy,
			CreatedAt:  r.CreatedAt,
			UpdatedAt:  r.UpdatedAt,
		})
	}
	return out, nil
}
