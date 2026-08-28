package store

import (
	"context"
	"fmt"
	"regexp"
	"time"
)

// SecretDelivery is how a declared secret is delivered into a container — the
// load-bearing file-vs-env split that determines how it rotates (T5/T6). Stored
// as the small int the secrets resolve surface uses (secrets.DeliveryKind),
// mapped at that package's edge like every other store↔proto enum (types.go).
type SecretDelivery int32

const (
	// SecretDeliveryFile writes the secret to a 0600 file under the agent's
	// scoped $HOME — the rotatable delivery, rewritten in place on rotation.
	SecretDeliveryFile SecretDelivery = 0
	// SecretDeliveryEnv delivers the secret as an environment value (via the
	// aggregate 0600 env file each wrapped exec reads at spawn).
	SecretDeliveryEnv SecretDelivery = 1
)

// SecretKind is the routing class the T5 materializer switches on: a generic
// declared secret, a provider (LLM) credential that rides the OMP SDK auth
// surface, or a gh credential placed into ~/.config/gh/hosts.yml.
type SecretKind int32

const (
	// SecretKindGeneric is a plain declared secret (DB URL, API token) placed
	// by DeliveryKind (file under $HOME/.compass/secrets/<NAME> or env).
	SecretKindGeneric SecretKind = 0
	// SecretKindProvider is an LLM provider credential routed to the AuthStorage
	// seed (never the generic env/file channels); carries a Provider id.
	SecretKindProvider SecretKind = 1
	// SecretKindGH is a gh credential routed to the gh hosts.yml placement
	// (runtime.GHHostsScript); carries a Host (default github.com).
	SecretKindGH SecretKind = 2
)

// secretNamePattern is SecretSpec's env-var-name grammar. A declared name is
// validated against it at the store door (DeclareSecret) — before it can reach
// a row — because it later becomes a path segment under $HOME/.compass/secrets/
// and a line in a root-adjacent setup script (T5): constrained at the door, not
// escaped downstream. The identical grammar is re-exported and re-checked by
// internal/secrets (defense in depth at materialization).
var secretNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SecretDeclaration is one names-only registry row: a declared secret's name,
// how it is delivered/routed, and who declared it — NEVER its value. The value
// lives only in the SecretSpec provider; the Server resolves it at fetch time
// (internal/secrets) and never persists it.
type SecretDeclaration struct {
	Name string
	// Delivery is the file-vs-env split (T5/T6 rotation shape).
	Delivery SecretDelivery
	// Kind is the materializer routing class.
	Kind SecretKind
	// Provider is the SDK provider id, set only for SecretKindProvider (else "").
	Provider string
	// Host is the forge host, set only for SecretKindGH (default github.com,
	// else "").
	Host string
	// DeclaredBy is the account that declared the secret (write path is
	// user-only, enforced at the T7 RPC edge).
	DeclaredBy AccountID
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// DeclareSecret adds a names-only registry row (RIG-1327 T3). It stores NO
// value — the value lives in the SecretSpec provider. name is validated against
// SecretSpec's env-var-name grammar at the door (a bad name is
// ErrInvalidArgument before touching Postgres, since the name becomes a
// filesystem path and script token downstream). A duplicate name is
// ErrConflict; an unknown actor account is ErrInvalidArgument (the declared_by
// FK). provider is meaningful only for a provider kind and host only for a gh
// kind; callers pass "" otherwise.
func (s *Store) DeclareSecret(ctx context.Context, actor AccountID, name string, delivery SecretDelivery, kind SecretKind, provider, host string) error {
	if !secretNamePattern.MatchString(name) {
		return fmt.Errorf("%w: secret name %q must match %s", ErrInvalidArgument, name, secretNamePattern.String())
	}
	if actor == "" {
		return fmt.Errorf("%w: declaring account id is required", ErrInvalidArgument)
	}
	if err := validateKindRouting(kind, provider, host); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO secrets (name, delivery, kind, provider, host, declared_by)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		name, int32(delivery), int32(kind), provider, host, string(actor),
	); err != nil {
		if pgErrIs(err, pgUniqueViolation) {
			return fmt.Errorf("%w: secret %q already declared", ErrConflict, name)
		}
		if pgErrIs(err, pgForeignKeyViolation) {
			return fmt.Errorf("%w: declaring account %q does not exist", ErrInvalidArgument, actor)
		}
		return fmt.Errorf("store: declare secret: %w", err)
	}
	return nil
}

// validateKindRouting enforces the kind↔provider/host invariant at the store
// door, mirroring the secrets_kind_routing CHECK: a provider row (kind=1)
// carries a non-empty provider and no host, a gh row (kind=2) a non-empty host
// and no provider, a generic row (kind=0) neither. A caller that violates it
// gets an actionable ErrInvalidArgument here rather than a raw constraint
// violation from the INSERT — and an out-of-invariant row can never reach the
// T5 materializer, where an empty provider id would silently misroute.
func validateKindRouting(kind SecretKind, provider, host string) error {
	switch kind {
	case SecretKindGeneric:
		if provider != "" || host != "" {
			return fmt.Errorf("%w: generic secret carries no provider or host", ErrInvalidArgument)
		}
	case SecretKindProvider:
		if provider == "" {
			return fmt.Errorf("%w: provider secret requires a non-empty provider", ErrInvalidArgument)
		}
		if host != "" {
			return fmt.Errorf("%w: provider secret carries no host", ErrInvalidArgument)
		}
	case SecretKindGH:
		if host == "" {
			return fmt.Errorf("%w: gh secret requires a non-empty host", ErrInvalidArgument)
		}
		if provider != "" {
			return fmt.Errorf("%w: gh secret carries no provider", ErrInvalidArgument)
		}
	default:
		return fmt.Errorf("%w: unknown secret kind %d", ErrInvalidArgument, kind)
	}
	return nil
}

// DeleteSecretDeclaration removes a names-only registry row. Deleting a name
// that was never declared is ErrNotFound, so a caller learns a bad delete
// target rather than silently succeeding (matching RevokeToken's unknown-target
// posture). The provider-side value deletion is a separate write path
// (internal/secrets Resolver.Delete); this only drops the declaration.
//
// The registry is a single global namespace (name is the PRIMARY KEY, inject-all
// MVP — no per-declaration owner, the frozen record's D-decisions), so a row is
// keyed by name alone, not (actor, name): any declared name is a legal delete
// target regardless of who declared it. This is contract-correct only under the
// single-user Server MVP (OQ7, Matt-ruled): the secrets table has no user
// dimension, so no other user's declaration exists for a name-keyed delete to
// cross; per-owner scoping (an owner_user_id column + a scoped delete) is the
// named post-MVP seam, not a gap here. actor is carried for the audit trail and
// so the signature matches DeclareSecret; write authorization is user-only and
// enforced at the T7 RPC edge, not re-litigated per row here.
func (s *Store) DeleteSecretDeclaration(ctx context.Context, actor AccountID, name string) error {
	_ = actor // see doc: name-keyed global registry; actor is audit context, not a filter
	tag, err := s.pool.Exec(ctx, "DELETE FROM secrets WHERE name = $1", name)
	if err != nil {
		return fmt.Errorf("store: delete secret declaration: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: secret %q", ErrNotFound, name)
	}
	return nil
}

// DeclaredSecrets returns every registry row, name-ordered. This is the whole
// declared set the secrets Resolver generates its SecretSpec manifest from
// (inject-all: no per-agent filter in the MVP). It never returns a value —
// there is none stored.
func (s *Store) DeclaredSecrets(ctx context.Context) ([]SecretDeclaration, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT name, delivery, kind, provider, host, declared_by, created_at, updated_at
		 FROM secrets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list declared secrets: %w", err)
	}
	defer rows.Close()

	var out []SecretDeclaration
	for rows.Next() {
		var (
			d          SecretDeclaration
			delivery   int32
			kind       int32
			declaredBy string
		)
		if err := rows.Scan(&d.Name, &delivery, &kind, &d.Provider, &d.Host, &declaredBy, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan declared secret: %w", err)
		}
		d.Delivery = SecretDelivery(delivery)
		d.Kind = SecretKind(kind)
		d.DeclaredBy = AccountID(declaredBy)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate declared secrets: %w", err)
	}
	return out, nil
}
