package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// The two Postgres roles the store's queries run under (0002_rls.sql).
//
//   - appRole is the NON-owner, non-BYPASSRLS request-path role. Every
//     request-path statement runs as this role so RLS + FORCE actually apply
//     (a superuser owner bypasses even FORCE — design.md:703-708), gated by the
//     per-transaction compass.tenant_id GUC.
//   - systemRole is the narrowly-scoped BYPASSRLS role the cross-tenant
//     background loops (N5/OQ-4: delivery-cursor sweep, deliver-ack advance,
//     reattach recovery, lag-resync) run under, and ONLY those. It carries no
//     tenant GUC — it is cross-tenant by design.
const (
	appRole    = "compass_app"
	systemRole = "compass_system"
)

// tenantGUC is the transaction-local setting the RLS policies read. Set via
// set_config(tenantGUC, <tenant>, true) — the `true` making it SET LOCAL
// (transaction-scoped), never a session SET that would leak across a
// transaction-mode pooler's connection checkouts (design.md:663-673).
const tenantGUC = "compass.tenant_id"

// systemRoleKey marks a context as running the cross-tenant system path. When
// present, the store arms statements with SET LOCAL ROLE compass_system
// (BYPASSRLS) and no tenant GUC, instead of the tenant-scoped app role. Only the
// four N5 background loops set it (WithSystemRole); every other path is
// tenant-scoped and fail-closed.
type systemRoleKey struct{}

// WithSystemRole marks ctx as the cross-tenant background/system path: store
// calls made under it run as the BYPASSRLS compass_system role and see every
// tenant's rows. It is the OQ-4 (Matt-ruled option 1) exemption, applied ONLY at
// the four named background-loop entrypoints (the delivery consumer's Run, the
// hub's deliver-ack / forge-notification-ack arms, and reattach recovery) — a
// request-path call NEVER sets it, so the request path stays tenant-scoped and
// fail-closed under RLS.
func WithSystemRole(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemRoleKey{}, true)
}

// isSystemRole reports whether ctx is the cross-tenant system path.
func isSystemRole(ctx context.Context) bool {
	v, _ := ctx.Value(systemRoleKey{}).(bool)
	return v
}

// scopedDBTX is the db.DBTX the store's *db.Queries is bound to in place of the
// bare pool. It wraps the pgxpool and, on EVERY statement, prepends the tenant
// scoping — SET LOCAL ROLE + (on the request path) set_config(tenantGUC, ...) —
// into ONE pgx batch with the actual query, so scoping rides a single network
// round-trip rather than doubling every call's latency (design.md:726-734).
//
// Because a pooled SendBatch runs as one implicit transaction, the SET LOCAL
// applies to the batched query and resets when the batch's implicit transaction
// ends — so a transaction-mode pooler reusing the physical connection for the
// next checkout inherits no leftover tenant scoping (verified by the
// pooler-reuse test). Multi-statement store methods that need one explicit tx
// across several queries use beginTenantTx instead (which arms once at BEGIN);
// this wrapper is the single-statement (pool-path) arm.
//
// The tenant is resolved from ctx exactly as a write's stamp is
// (resolveTenant): the context tenant if set, else the bootstrap tenant (the
// OSS single-tenant degenerate path). The system path (isSystemRole) arms only
// the BYPASSRLS role and issues no GUC.
type scopedDBTX struct {
	store *Store
}

func (d scopedDBTX) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	b := &pgx.Batch{}
	n := d.armQueue(ctx, b)
	b.Queue(sql, args...)
	br := d.store.pool.SendBatch(ctx, b)
	defer func() { _ = br.Close() }() // no-op after the final Exec drains the batch; safe on every path.
	for range n {
		if _, err := br.Exec(); err != nil {
			return pgconn.CommandTag{}, err
		}
	}
	return br.Exec()
}

func (d scopedDBTX) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	b := &pgx.Batch{}
	n := d.armQueue(ctx, b)
	b.Queue(sql, args...)
	br := d.store.pool.SendBatch(ctx, b)
	for range n {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return nil, err
		}
	}
	rows, err := br.Query()
	if err != nil {
		_ = br.Close()
		return nil, err
	}
	// The BatchResults owns the pooled connection until it is Closed; closing
	// the rows must also close it, or the connection leaks. batchRows.Close does
	// both (sqlc always defers rows.Close()).
	return &batchRows{Rows: rows, br: br}, nil
}

func (d scopedDBTX) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	b := &pgx.Batch{}
	n := d.armQueue(ctx, b)
	b.Queue(sql, args...)
	br := d.store.pool.SendBatch(ctx, b)
	// Arm-statement errors are not discarded: they surface on the row's
	// Scan/Close below (batchRow.Scan joins the BatchResults Close error), so a
	// failed SET LOCAL ROLE / set_config fails the caller's Scan rather than
	// vanishing. Draining them here advances the batch to the query's result.
	for range n {
		_, _ = br.Exec()
	}
	return &batchRow{row: br.QueryRow(), br: br}
}

// armQueue prepends the scoping statements to b and returns how many leading
// results they produce (which the caller must drain before the query's own
// result). Role identifiers are fixed constants, never interpolated user input,
// so the string concatenation is injection-safe.
func (d scopedDBTX) armQueue(ctx context.Context, b *pgx.Batch) int {
	if isSystemRole(ctx) {
		b.Queue("SET LOCAL ROLE " + systemRole)
		return 1
	}
	b.Queue("SET LOCAL ROLE " + appRole)
	b.Queue("SELECT set_config($1, $2, true)", tenantGUC, string(d.store.resolveTenant(ctx)))
	return 2
}

// batchRows couples the query rows to the BatchResults so Close releases both —
// the pooled connection is held by the BatchResults, not the rows.
type batchRows struct {
	pgx.Rows
	br pgx.BatchResults
}

func (r *batchRows) Close() {
	r.Rows.Close()
	_ = r.br.Close() // the query error, if any, already surfaced via Rows.Err(); Close's own error is not actionable here.
}

// batchRow couples a single-row result to the BatchResults. Scan closes the
// BatchResults (releasing the connection) and joins any close error, so a failed
// arm statement or a connection fault is not swallowed.
type batchRow struct {
	row pgx.Row
	br  pgx.BatchResults
}

func (r *batchRow) Scan(dest ...any) error {
	scanErr := r.row.Scan(dest...)
	closeErr := r.br.Close()
	if scanErr != nil {
		return scanErr
	}
	return closeErr
}

// beginTenantTx begins an explicit transaction and arms it with the tenant
// scoping — SET LOCAL ROLE + (request path) the compass.tenant_id GUC — in one
// batch at BEGIN, so every subsequent statement on the returned tx is
// tenant-scoped without re-arming. It is the multi-statement counterpart to
// scopedDBTX: store methods that run several queries in one transaction (the
// former s.pool.Begin sites) use it so the whole transaction shares one tenant
// scope. The tenant resolves via resolveTenant(ctx) (context tenant, else
// bootstrap); a system-path ctx arms the BYPASSRLS role with no GUC.
//
// On any arm failure it rolls back and returns the error, so a caller never gets
// a half-armed tx. The caller owns Commit/Rollback exactly as before.
func (s *Store) beginTenantTx(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin tenant tx: %w", err)
	}
	if err := armTx(ctx, s, tx); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

// armTx issues the scoping statements on an already-open tx in one batch. Shared
// by beginTenantTx and WithTenantTx.
func armTx(ctx context.Context, s *Store, tx pgx.Tx) error {
	b := &pgx.Batch{}
	var n int
	if isSystemRole(ctx) {
		b.Queue("SET LOCAL ROLE " + systemRole)
		n = 1
	} else {
		b.Queue("SET LOCAL ROLE " + appRole)
		b.Queue("SELECT set_config($1, $2, true)", tenantGUC, string(s.resolveTenant(ctx)))
		n = 2
	}
	br := tx.SendBatch(ctx, b)
	defer func() { _ = br.Close() }()
	for range n {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("store: arm tenant tx: %w", err)
		}
	}
	return nil
}

// scopedPool returns the tenant-scoping db.DBTX the store's *db.Queries binds
// to. Named so store.go's Open reads clearly.
func (s *Store) scopedPool() db.DBTX {
	return scopedDBTX{store: s}
}
