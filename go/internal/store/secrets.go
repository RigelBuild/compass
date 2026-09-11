package store

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/RigelBuild/compass/go/internal/store/db"
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
	// F1 (design record D6): the user keyspace REJECTS reserved server-secret
	// prefixes. Without this a user-path declare could mint a shadow `secrets`
	// row under a server-secret name, which the inject-all delivery path then
	// hands to every agent container. With it, the two doors partition the
	// keyspace by name: a reserved-prefix name can only live in
	// `server_secrets`, an unprefixed one only in `secrets`.
	if HasServerSecretPrefix(name) {
		return fmt.Errorf("%w: secret name %q uses a reserved server-secret prefix", ErrInvalidArgument, name)
	}
	if actor == "" {
		return fmt.Errorf("%w: declaring account id is required", ErrInvalidArgument)
	}
	if err := validateKindRouting(kind, provider, host); err != nil {
		return err
	}
	if err := s.q.InsertSecret(ctx, db.InsertSecretParams{
		Name:       name,
		Delivery:   int16(delivery), //nolint:gosec // G115: SecretDelivery is a CHECK-constrained 0/1 enum (secrets.delivery), always within int16
		Kind:       int16(kind),     //nolint:gosec // G115: SecretKind is a CHECK-constrained 0/1/2 enum (secrets.kind), always within int16
		Provider:   provider,
		Host:       host,
		DeclaredBy: string(actor),
	}); err != nil {
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

// DeleteSecretDeclaration removes the secret row at (name, scopeKind, scopeID).
// Deleting a coordinate that was never declared is ErrNotFound, so a caller
// learns a bad delete target rather than silently succeeding. Deleting the row
// deletes its value with it (declaration and value are the same row post-A1).
//
// The scope pair is required because a name alone no longer identifies a row
// (composite PK, A9): the same name may hold a distinct value at tenant, user,
// and agent scope, so a name-keyed delete would be ambiguous. actor is carried
// for the audit trail and so the signature matches the write door; write
// authorization is enforced at the RPC edge, not re-litigated per row here.
func (s *Store) DeleteSecretDeclaration(ctx context.Context, actor AccountID, name string, scopeKind int16, scopeID string) error {
	_ = actor // audit context, not a filter — see doc
	affected, err := s.q.DeleteSecret(ctx, db.DeleteSecretParams{
		Name:      name,
		ScopeKind: scopeKind,
		ScopeID:   scopeID,
	})
	if err != nil {
		return fmt.Errorf("store: delete secret declaration: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: secret %q", ErrNotFound, name)
	}
	return nil
}

// DeclaredSecrets returns every registry row, name-ordered. This is the whole
// declared set the secrets Resolver generates its SecretSpec manifest from
// (inject-all: no per-agent filter in the MVP). It never returns a value —
// there is none stored.
func (s *Store) DeclaredSecrets(ctx context.Context) ([]SecretDeclaration, error) {
	rows, err := s.q.DeclaredSecrets(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list declared secrets: %w", err)
	}
	var out []SecretDeclaration
	for _, r := range rows {
		out = append(out, SecretDeclaration{
			Name:       r.Name,
			Delivery:   SecretDelivery(r.Delivery),
			Kind:       SecretKind(r.Kind),
			Provider:   r.Provider,
			Host:       r.Host,
			DeclaredBy: AccountID(r.DeclaredBy),
			CreatedAt:  r.CreatedAt.Time,
			UpdatedAt:  r.UpdatedAt.Time,
		})
	}
	return out, nil
}

// Secret scope tiers (A9): a secret resolves most-specific-wins, agent > user >
// tenant. The int16 encoding IS the resolution precedence (SecretRecordsForAgent
// orders by scope_kind DESC), so the values are load-bearing, not arbitrary.
const (
	// SecretScopeTenant is a shared value several users resolve; scope_id is "".
	SecretScopeTenant int16 = 0
	// SecretScopeUser is owned by a user; scope_id is that user's account id.
	SecretScopeUser int16 = 1
	// SecretScopeAgent is owned by an agent; scope_id is that agent's account id.
	SecretScopeAgent int16 = 2
)

// SecretRecord is a SecretDeclaration plus the at-rest value columns and the
// scope coordinate. It carries CIPHERTEXT only — the store never sees plaintext
// (crypto lives in the envelope/secrets layer).
type SecretRecord struct {
	SecretDeclaration
	ScopeKind       int16
	ScopeID         string
	ValueCiphertext []byte
	ValueNonce      []byte
	KeyVersion      int16
}

// validateScopeShape enforces the A9 scope↔id shape at the store door, mirroring
// the secrets_scope_shape CHECK: a tenant row carries no id; a user/agent row
// must. A caller that violates it gets ErrInvalidArgument here rather than a raw
// constraint violation from the write.
func validateScopeShape(scopeKind int16, scopeID string) error {
	switch scopeKind {
	case SecretScopeTenant:
		if scopeID != "" {
			return fmt.Errorf("%w: tenant-scoped secret carries no scope id", ErrInvalidArgument)
		}
	case SecretScopeUser, SecretScopeAgent:
		if scopeID == "" {
			return fmt.Errorf("%w: user/agent-scoped secret requires a scope id", ErrInvalidArgument)
		}
	default:
		return fmt.Errorf("%w: unknown secret scope kind %d", ErrInvalidArgument, scopeKind)
	}
	return nil
}

// validateDelivery enforces the delivery range at the store door, mirroring the
// secrets.delivery CHECK (0=file, 1=env). Without it an out-of-range delivery
// sails past the door and surfaces as a bare wrapped error at the RPC edge
// (CodeInternal) rather than the ErrInvalidArgument UpsertSecret's doc promises.
func validateDelivery(delivery SecretDelivery) error {
	switch delivery {
	case SecretDeliveryFile, SecretDeliveryEnv:
		return nil
	default:
		return fmt.Errorf("%w: unknown secret delivery %d", ErrInvalidArgument, delivery)
	}
}

// UpsertSecret validates name grammar, the reserved-prefix partition, kind
// routing, and the A9 scope shape at the door, resolves the scope_id against the
// right account subtype in the writing transaction (no FK exists, A9), then
// transactionally upserts declaration+value at (name, scopeKind, scopeID). A
// fresh coordinate inserts; an existing one is a value rewrite. It carries
// CIPHERTEXT — the caller encrypts before this door.
func (s *Store) UpsertSecret(ctx context.Context, actor AccountID, name string, scopeKind int16, scopeID string, delivery SecretDelivery, kind SecretKind, provider, host string, ciphertext, nonce []byte, keyVersion int16) error {
	if !secretNamePattern.MatchString(name) {
		return fmt.Errorf("%w: secret name %q must match %s", ErrInvalidArgument, name, secretNamePattern.String())
	}
	// F1: the user keyspace rejects reserved server-secret prefixes case-fold
	// (ShadowsServerSecretPrefix), the wide reject side of the partition.
	if ShadowsServerSecretPrefix(name) {
		return fmt.Errorf("%w: secret name %q uses a reserved server-secret prefix", ErrInvalidArgument, name)
	}
	if actor == "" {
		return fmt.Errorf("%w: writing account id is required", ErrInvalidArgument)
	}
	if err := validateKindRouting(kind, provider, host); err != nil {
		return err
	}
	if err := validateScopeShape(scopeKind, scopeID); err != nil {
		return err
	}
	if err := validateDelivery(delivery); err != nil {
		return err
	}

	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return fmt.Errorf("store: begin upsert secret: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit; safe on every non-commit path
	qtx := s.q.WithTx(tx)

	// Referential integrity for a user/agent scope_id, in lieu of an FK (A9): the
	// scope_id must name a real account of the scope's subtype. Resolved in this
	// transaction so a concurrent account delete cannot race the write.
	switch scopeKind {
	case SecretScopeUser:
		ok, err := qtx.IsUserAccount(ctx, scopeID)
		if err != nil {
			return fmt.Errorf("store: resolve user scope: %w", err)
		}
		if !ok {
			return fmt.Errorf("%w: user scope %q is not a user account", ErrInvalidArgument, scopeID)
		}
	case SecretScopeAgent:
		ok, err := qtx.IsAgentAccount(ctx, scopeID)
		if err != nil {
			return fmt.Errorf("store: resolve agent scope: %w", err)
		}
		if !ok {
			return fmt.Errorf("%w: agent scope %q is not an agent account", ErrInvalidArgument, scopeID)
		}
	}

	if err := qtx.UpsertSecret(ctx, db.UpsertSecretParams{
		Name:            name,
		ScopeKind:       scopeKind,
		ScopeID:         scopeID,
		Delivery:        int16(delivery), //nolint:gosec // G115: SecretDelivery is a CHECK-constrained 0/1 enum, always within int16
		Kind:            int16(kind),     //nolint:gosec // G115: SecretKind is a CHECK-constrained 0/1/2 enum, always within int16
		Provider:        provider,
		Host:            host,
		ValueCiphertext: ciphertext,
		ValueNonce:      nonce,
		KeyVersion:      keyVersion,
		DeclaredBy:      string(actor),
	}); err != nil {
		if pgErrIs(err, pgForeignKeyViolation) {
			return fmt.Errorf("%w: writing account %q does not exist", ErrInvalidArgument, actor)
		}
		// Backstop the door checks: a CHECK violation (e.g. a delivery/kind out of
		// range) is an invalid argument, not an internal fault.
		if pgErrIs(err, pgCheckViolation) {
			return fmt.Errorf("%w: secret write violates a table constraint", ErrInvalidArgument)
		}
		return fmt.Errorf("store: upsert secret: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit upsert secret: %w", err)
	}
	return nil
}

// SecretRecordsForAgent returns the one most-specific row per name visible to
// agent (the A9 DISTINCT ON collapse), name-ordered, ciphertext only — the
// StoreResolver.ResolveFor read. Shadowed rows never leave Postgres.
func (s *Store) SecretRecordsForAgent(ctx context.Context, agent AccountID) ([]SecretRecord, error) {
	rows, err := s.q.SecretRecordsForAgent(ctx, string(agent))
	if err != nil {
		return nil, fmt.Errorf("store: secret records for agent: %w", err)
	}
	out := make([]SecretRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, SecretRecord{
			SecretDeclaration: SecretDeclaration{
				Name:       r.Name,
				Delivery:   SecretDelivery(r.Delivery),
				Kind:       SecretKind(r.Kind),
				Provider:   r.Provider,
				Host:       r.Host,
				DeclaredBy: AccountID(r.DeclaredBy),
				CreatedAt:  r.CreatedAt.Time,
				UpdatedAt:  r.UpdatedAt.Time,
			},
			ScopeKind:       r.ScopeKind,
			ScopeID:         r.ScopeID,
			ValueCiphertext: r.ValueCiphertext,
			ValueNonce:      r.ValueNonce,
			KeyVersion:      r.KeyVersion,
		})
	}
	return out, nil
}
