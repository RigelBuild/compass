package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/RigelBuild/compass/go/internal/store/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationsFS holds the ordered, versioned schema migrations, applied at Open.
// Embedding them in the binary (design.md:1136 "embedded migration files")
// means the schema the binary expects and the migrations that produce it can
// never drift apart across a deploy.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationLockKey is the pg_advisory_lock key Open holds while applying
// migrations, so two Servers starting against one database serialize their
// migration runs rather than racing. An arbitrary fixed constant unique to this
// store; any concurrent Open blocks here until the first finishes, then finds
// the schema already current.
const migrationLockKey int64 = 0x0C0A_5500_0000_0001

// Store is the Postgres store of record. It owns a pgx connection pool and is
// safe for concurrent use by every RPC handler; share it by pointer. Construct
// it with Open and release it with Close.
type Store struct {
	pool *pgxpool.Pool
	// q is the sqlc-generated typed query set bound to the pool (db.New(pool)),
	// the migrated read/write path (sqlc adoption, RIG-3034). Pool-scoped calls
	// go through s.q directly; a tx-scoped path rebinds it with s.q.WithTx(tx).
	// Set once in Open, immutable thereafter, so it is safe for concurrent use
	// exactly like the pool it wraps.
	q *db.Queries
	// objectStore is the archive-tier object-store seam (RIG-1667 T4), injected
	// via SetObjectStore. nil until slice B wires a real client (store tests
	// inject an in-memory fake); a flush against a nil store fails loudly.
	objectStore ObjectStore
	// safetyValveCapBytes is the high size cap on the post-checkpoint hot-tail
	// that triggers a safety_valve eviction; defaultSafetyValveCapBytes at Open,
	// tunable (lowered by tests to exercise the valve).
	safetyValveCapBytes int
	// coordinationHook is the manager-comms coordination-channel reconcile
	// (RIG-1722 T5), registered by the comms layer via SetCoordinationHook at
	// server assembly and invoked by the two parent-edge writers (CreateAgent,
	// ReparentAgent) on their own tx right after writing parent_agent_id. nil
	// until wired (a store with no hook — every store-only test — is a no-op).
	// No lock: set once before serving, so the write happens-before the first
	// concurrent parent-edge write (mirrors hub.SetSettleSink).
	coordinationHook CoordinationHook
	// bootstrapTenantID is the single OSS tenant seeded at Open, the fallback
	// tenant every write is stamped with when the request context carries no
	// resolved tenant (resolveTenant). Set once in Open before the store serves;
	// no lock, mirroring coordinationHook's set-once-before-serving discipline.
	bootstrapTenantID TenantID
}

// querier is the read surface shared by the pool and a transaction, so a scan
// helper (scanChannels) or an authorization probe (requireChannelMember) can
// run against either. Both *pgxpool.Pool and pgx.Tx satisfy it.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// execer is the write surface shared by the pool and a transaction, so a write
// helper (updateMessageBlocksExec) can run against either. Both *pgxpool.Pool
// and pgx.Tx satisfy it.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Open connects to Postgres at dsn (a pgx pool), applies any pending embedded
// migrations under an advisory lock, and verifies the resulting schema version
// matches what this binary expects — refusing to serve on a failed migration
// or a version mismatch (design.md:1136-1137, 1145-1146). A returned Store is
// ready to serve; a returned error means the caller must not serve.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	// A New pool is lazy; force one real connection so a bad DSN or an
	// unreachable database fails Open here rather than on the first query.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}

	s := &Store{pool: pool, q: db.New(pool), safetyValveCapBytes: defaultSafetyValveCapBytes}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	bt, err := s.BootstrapTenant(ctx)
	if err != nil {
		pool.Close()
		return nil, err
	}
	s.bootstrapTenantID = bt
	return s, nil
}

// Close releases the connection pool. Idempotent-safe to call once; after it
// the Store must not be used.
func (s *Store) Close() {
	s.pool.Close()
}

// migration is one parsed embedded migration file: its numeric version and the
// SQL that advances the schema to it.
type migration struct {
	version int
	name    string
	sql     string
}

// migrate applies every embedded migration not yet recorded in the database,
// each in its own transaction, holding a session advisory lock so concurrent
// Servers serialize. After applying, it verifies the recorded version equals
// the highest embedded version (the refuse-to-serve guard).
func (s *Store) migrate(ctx context.Context) error {
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	if len(migs) == 0 {
		return fmt.Errorf("%w: no embedded migrations", ErrSchemaVersion)
	}

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("store: acquire for migrate: %w", err)
	}
	defer conn.Release()

	// Serialize migration across processes: a session-level advisory lock a
	// concurrent Open blocks on until we release it, so two Servers never apply
	// the same migration twice. Released explicitly (not only on conn release)
	// so the lock is gone the moment migration finishes.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("store: acquire migration lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", migrationLockKey) }()

	if err := ensureMigrationsTable(ctx, conn); err != nil {
		return err
	}
	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}

	for _, m := range migs {
		if applied[m.version] {
			continue
		}
		if err := applyMigration(ctx, conn, m); err != nil {
			return err
		}
	}

	// Refuse-to-serve verify: the database's max recorded version must equal the
	// highest embedded version. A higher database version (this binary rolled
	// back below the schema) or a gap is a mismatch we do not serve.
	want := migs[len(migs)-1].version
	got, err := currentVersion(ctx, conn)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: database at v%d, binary expects v%d", ErrSchemaVersion, got, want)
	}
	return nil
}

// ensureMigrationsTable creates the schema-version bookkeeping table if absent.
// Kept outside the numbered migrations so the runner can record v1 itself.
func ensureMigrationsTable(ctx context.Context, conn *pgxpool.Conn) error {
	const ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`
	if _, err := conn.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("store: ensure schema_migrations: %w", err)
	}
	return nil
}

// appliedVersions reads the set of already-applied migration versions.
func appliedVersions(ctx context.Context, conn *pgxpool.Conn) (map[int]bool, error) {
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: scan applied migration: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate applied migrations: %w", err)
	}
	return applied, nil
}

// currentVersion returns the highest applied migration version, or 0 if none.
func currentVersion(ctx context.Context, conn *pgxpool.Conn) (int, error) {
	var v int
	err := conn.QueryRow(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("store: read current version: %w", err)
	}
	return v, nil
}

// applyMigration runs one migration's SQL and records it, atomically: the DDL
// and the schema_migrations insert commit together, so a crash mid-migration
// leaves the schema exactly at the last fully-applied version (never a
// half-applied one). A failure rolls back and refuses to serve.
func applyMigration(ctx context.Context, conn *pgxpool.Conn, m migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin migration v%d: %w", m.version, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, m.sql); err != nil {
		return fmt.Errorf("store: apply migration v%d (%s): %w", m.version, m.name, err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.version, m.name,
	); err != nil {
		return fmt.Errorf("store: record migration v%d: %w", m.version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit migration v%d: %w", m.version, err)
	}
	return nil
}

// loadMigrations parses the embedded migration files into version order. A file
// is named `NNNN_name.sql`; the leading integer is its version. A malformed
// name is a build-time authoring error surfaced as ErrSchemaVersion.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations dir: %w", err)
	}

	migs := make([]migration, 0, len(entries))
	seen := make(map[int]bool)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, err := parseVersion(e.Name())
		if err != nil {
			return nil, err
		}
		if seen[version] {
			return nil, fmt.Errorf("%w: duplicate migration version %d", ErrSchemaVersion, version)
		}
		seen[version] = true

		body, err := fs.ReadFile(migrationsFS, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read migration %s: %w", e.Name(), err)
		}
		migs = append(migs, migration{version: version, name: e.Name(), sql: string(body)})
	}

	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })

	// The embedded set must be a contiguous 1..N sequence: a gap (0001 + 0003,
	// missing 0002) would otherwise pass the max-version serve check while
	// silently deploying an incomplete schema. This is the build-time half of
	// the refuse-to-serve-on-a-gap contract (migrate() enforces the runtime
	// half against the database).
	if err := checkContiguous(migs); err != nil {
		return nil, err
	}
	return migs, nil
}

// checkContiguous verifies a version-sorted migration set is a gapless 1..N
// sequence, returning ErrSchemaVersion at the first gap. Split from
// loadMigrations so the invariant is unit-testable without the embedded FS.
func checkContiguous(migs []migration) error {
	for i, m := range migs {
		if want := i + 1; m.version != want {
			return fmt.Errorf("%w: embedded migrations not contiguous: expected v%d, found v%d (%s)", ErrSchemaVersion, want, m.version, m.name)
		}
	}
	return nil
}

// parseVersion extracts the leading integer version from a `NNNN_name.sql`
// migration filename.
func parseVersion(name string) (int, error) {
	base, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, fmt.Errorf("%w: migration %q missing version prefix", ErrSchemaVersion, name)
	}
	v, err := strconv.Atoi(base)
	if err != nil {
		return 0, fmt.Errorf("%w: migration %q bad version prefix: %s", ErrSchemaVersion, name, err.Error())
	}
	return v, nil
}

// pgErrIs reports whether err is a Postgres error with the given SQLSTATE code,
// so a method can map a unique-violation (23505) to ErrConflict or a
// foreign-key violation (23503) to ErrInvalidArgument without string-matching.
func pgErrIs(err error, code string) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == code
	}
	return false
}

// pgConstraintName returns the name of the violated constraint when err is a
// Postgres error, or "" otherwise — so a foreign-key handler can tell which of
// several FKs on a table fired (the parent-agent FK vs the owner-user FK).
func pgConstraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// noRows reports whether err is pgx's no-rows sentinel, which a lookup maps to
// ErrNotFound.
func noRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
