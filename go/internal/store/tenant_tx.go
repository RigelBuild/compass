package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// appRole applies request-path RLS with a tenant GUC. systemRole is reserved for
// explicit cross-tenant background work and runs without a tenant GUC.
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
// named entrypoints below use it; every other path is tenant-scoped and fail-closed.
type systemRoleKey struct{}

// WithSystemRole marks ctx as the cross-tenant background/system path: store
// calls made under it run as the BYPASSRLS compass_system role and see every
// tenant's rows. It is the OQ-4 (Matt-ruled option 1) exemption, applied ONLY at
// named entrypoints (the delivery consumer's Run, the hub's deliver-ack /
// forge-notification-ack arms, reattach recovery, the compute-usage orphan
// sweep, Hub.runnerSessionCtx's tenant lookup, Hub.BindLifetime's tenant
// lookup, and Hub.enroll's reap keyed by the authenticated Runner id). Every
// other request-path call stays tenant-scoped.
func WithSystemRole(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemRoleKey{}, true)
}

// WithoutSystemRole clears the system-role mark, so store calls on the returned
// ctx are tenant-scoped again. Pair it with WithTenant.
func WithoutSystemRole(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemRoleKey{}, false)
}

// isSystemRole reports whether ctx is the cross-tenant system path.
func isSystemRole(ctx context.Context) bool {
	v, _ := ctx.Value(systemRoleKey{}).(bool)
	return v
}

// IsSystemRole reports whether ctx is the cross-tenant system path (the exported
// form of isSystemRole). A caller that must NOT run a request-scoped read under
// the system role — the RIG-3108 binding cache, whose reads resolve the
// principal a comms call runs under and must stay tenant-scoped — gates on this
// rather than reading the table unscoped under BYPASSRLS.
func IsSystemRole(ctx context.Context) bool {
	return isSystemRole(ctx)
}

// scopedDBTX batches SET LOCAL ROLE and, for requests, the tenant GUC with each
// pool query. The implicit transaction resets scope before a pooled connection
// is reused; multi-statement calls use beginTenantTx instead.
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

// beginTenantTx arms one explicit transaction with role and tenant scope. It is
// the multi-statement counterpart to scopedDBTX and rolls back if arming fails.
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
