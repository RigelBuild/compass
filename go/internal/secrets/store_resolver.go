package secrets

// StoreResolver is the DB-backed user-secret resolver (record T3/A4). Unlike
// SpecResolver it implements NO interface: its write methods (Upsert/Remove)
// carry an actor plus a scope coordinate the four-method Resolver never had, and
// its read (ResolveFor) is scoped to one agent — so a shared interface would be
// weightless (record A4/A9). It owns the crypto: the store sees only ciphertext
// and never decrypts, keeping store a leaf (secrets → store, no cycle).

import (
	"context"
	"fmt"

	"github.com/RigelBuild/compass/go/internal/envelope"
	"github.com/RigelBuild/compass/go/internal/store"
)

// StoreResolver resolves user secrets from the encrypted DB store. key owns the
// AES-256-GCM master key (held unexported, redacted under fmt/slog by envelope);
// keyVersion is the generation stamped on rows this resolver writes.
type StoreResolver struct {
	st         *store.Store
	key        envelope.Key
	keyVersion int16
}

// Compile-time seam assertions. SpecResolver still satisfies the wide Resolver
// interface at this task (its write half is deleted in T6). StoreResolver
// deliberately does NOT: its Upsert/Remove/ResolveFor names and signatures share
// nothing with Resolver's Resolve/Set/Delete/Statuses (record A4), so there is
// no positive assertion to make for it — the absence is the contract, and adding
// `var _ Resolver = (*StoreResolver)(nil)` here would fail the build.
var _ Resolver = (*SpecResolver)(nil)

// NewStoreResolver constructs a StoreResolver over the store and the master key.
// keyVersion is the active generation (record A3): every Upsert stamps it, and
// it is bound into the write-path AAD so a row authenticates its own generation
// claim.
func NewStoreResolver(st *store.Store, key envelope.Key, keyVersion int16) *StoreResolver {
	return &StoreResolver{st: st, key: key, keyVersion: keyVersion}
}

// ResolveFor resolves the ONE most-specific row per name visible to agent (A9:
// agent > user > tenant, collapsed in SQL by SecretRecordsForAgent), decrypts
// each under its own row-bound AAD, and maps delivery/kind via the edge maps —
// the same []ResolvedSecret shape SpecResolver.Resolve produces, so the T4
// handler swap is a type match. reason is accepted for call-site symmetry with
// the Resolve seam; the DB path has no provider audit log to record it in.
//
// Fail-closed: a single undecryptable row fails the WHOLE resolve, never a
// partial set. This follows Resolve's established precedent (it hard-errors on a
// declared-but-unset name), and it is the safe direction — an injected
// environment silently missing one secret is a harder-to-diagnose failure than a
// loud one, and a decrypt failure means tampering or a wrong key, never a benign
// absence.
func (r *StoreResolver) ResolveFor(ctx context.Context, agent store.AccountID, reason string) ([]ResolvedSecret, error) {
	_ = reason // no provider audit log on the DB path; kept for seam symmetry
	recs, err := r.st.SecretRecordsForAgent(ctx, agent)
	if err != nil {
		return nil, fmt.Errorf("secrets: read agent secret records: %w", err)
	}
	out := make([]ResolvedSecret, 0, len(recs))
	for _, rec := range recs {
		// The AAD is built from the ROW's own coordinate — including its tenant, so
		// a ciphertext moved to another row (a different scope, name, OR tenant)
		// authenticates against a different AAD and fails to decrypt. Never from a
		// caller request.
		aad, err := envelope.UserSecretAAD(rec.TenantID, rec.ScopeKind, rec.ScopeID, rec.Name, rec.KeyVersion)
		if err != nil {
			// A stored row with a NUL in a bound field cannot have been written
			// through Upsert (which builds the identical AAD and rejects it), so
			// this is a corrupt row, not a benign miss — fail closed. Names the
			// field only; the value never reaches this error.
			return nil, fmt.Errorf("secrets: build AAD for %q: %w", rec.Name, err)
		}
		value, err := r.key.Decrypt(rec.ValueNonce, rec.ValueCiphertext, aad)
		if err != nil {
			// envelope.ErrDecrypt is opaque and carries no plaintext or key
			// material; wrap it with the row name only, never the value.
			return nil, fmt.Errorf("secrets: decrypt %q: %w", rec.Name, err)
		}
		out = append(out, ResolvedSecret{
			Name:     rec.Name,
			Value:    string(value),
			Version:  Version(string(value)),
			Delivery: deliveryFromStore(rec.Delivery),
			Kind:     kindFromStore(rec.Kind),
			Host:     rec.Host,
			Provider: rec.Provider,
		})
	}
	return out, nil
}

// Upsert encrypts value under the row's coordinate-bound AAD — tenant included —
// and transactionally upserts declaration+value at (name, scopeKind, scopeID)
// through the store door (record A4). Named Upsert, not Set: its signature carries
// an actor and a scope coordinate the old Resolver write half never had. The store
// validates name grammar, the reserved-prefix partition, kind routing, and the
// scope shape; a NUL in a bound AAD field is rejected here before any encryption,
// propagating envelope.ErrAADField.
//
// The AAD tenant is read from ctx via EffectiveTenant BEFORE encrypt. This is the
// tenant UpsertSecret's own transaction stamps the row's tenant_id with (both
// derive from resolveTenant(ctx)), so the bound tenant provably equals the
// landed tenant — the invariant a later ResolveFor depends on to decrypt. It is
// safe to read outside the write tx: the value is ctx-derived, not read from a
// row, so no row yet exists to race.
func (r *StoreResolver) Upsert(ctx context.Context, actor store.AccountID, name string, scopeKind int16, scopeID, value string, delivery DeliveryKind, kind SecretKind, provider, host string) error {
	aad, err := envelope.UserSecretAAD(string(r.st.EffectiveTenant(ctx)), scopeKind, scopeID, name, r.keyVersion)
	if err != nil {
		// Fail the operation on a NUL-bearing field rather than let it silently
		// produce a colliding AAD; the error names the field, never the value.
		return fmt.Errorf("secrets: build AAD for %q: %w", name, err)
	}
	nonce, ciphertext, err := r.key.Encrypt([]byte(value), aad)
	if err != nil {
		// Encrypt failure is a key-wiring fault (ErrUnsetKey) or a rand read; it
		// carries no plaintext, and value is never formatted into this error.
		return fmt.Errorf("secrets: encrypt %q: %w", name, err)
	}
	if err := r.st.UpsertSecret(ctx, actor, name, scopeKind, scopeID,
		deliveryToStore(delivery), kindToStore(kind), provider, host,
		ciphertext, nonce, r.keyVersion); err != nil {
		return fmt.Errorf("secrets: upsert %q: %w", name, err)
	}
	return nil
}

// Remove deletes the row at (name, scopeKind, scopeID) — declaration and value
// are the same row post-A1, so one delete removes both. An absent coordinate is
// store.ErrNotFound, surfaced to the caller.
func (r *StoreResolver) Remove(ctx context.Context, actor store.AccountID, name string, scopeKind int16, scopeID string) error {
	if err := r.st.DeleteSecretDeclaration(ctx, actor, name, scopeKind, scopeID); err != nil {
		return fmt.Errorf("secrets: remove %q: %w", name, err)
	}
	return nil
}
