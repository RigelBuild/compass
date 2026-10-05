package store

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// Session bindings: the durable record of WHICH LIVE SESSION speaks for an
// agent account, and the Runner it is attached to (RIG-3108/RIG-2861 §T4).
// Formerly RAM-only in the RunnerHub, so a Server restart lost every binding.

// A binding is NOT authorization (like agent_placements): SubscribeAgentSession
// authorizes through agent_sessions and never reads this table. It serves the
// relay's inbound-session owner lookup (accountForSession) and the delivery
// consumer's live-session lookup for an authorized recipient (SessionForAccount).

// Nor is a binding cross-checked against agent_sessions: no FK from session_id,
// so it may name a session with no agent_sessions row. agent_sessions remains
// the authz root, and nothing here may stand in for it.

// PR2 adds the table and methods only; demoting the hub's in-RAM maps is PR3.

// SessionBinding is one live binding, including its tenant. Returned by the
// reconnect sweep so callers can scope follow-up work to the deleted row.
type SessionBinding struct {
	TenantID  TenantID
	SessionID string
	AccountID AccountID
	RunnerID  string
}

// RecordSessionBinding points an agent account at the live session that speaks
// for it. It is an UPSERT keyed on the ACCOUNT, not the session: this table
// replaces the hub's 1:1 in-RAM accountSessions map, where re-pointing an
// account at a newer session is an assignment, so a re-point here is a write
// that always lands rather than a conflict to refuse. That matters concretely —
// the hub promotes a new session onto an account BEFORE unbinding the stale one,
// and promoteSession returns nothing, so it has nowhere to put a refusal.
//
// It returns the session id this bind DISPLACED — empty when the account held
// none. That value is load-bearing, not diagnostic: PR3 must reap the displaced
// session from the delivery held-deliver registry, the same side-effect
// DeleteSessionBindingsForRunner's returned rows exist for. A displaced session
// left unreaped holds deliveries for an account that has already moved on.
//
// The bind uses one explicit transaction: a per-account advisory lock, a prior
// row read, any interval close/start events, and the new binding write. Concurrent
// binds serialize at the advisory lock, so each caller gets the correct displaced
// session and no event can commit without its binding transition.
//
// A single statement could not give that. The `prev` CTE this replaces read the
// statement-start snapshot while the upsert's ON CONFLICT DO UPDATE blocked on
// the row lock and re-read the latest committed row — different versions, so
// under READ COMMITTED two concurrent re-points away from sess-A both reported
// sess-A and the genuinely displaced sess-B was never reaped.
//
// The advisory lock is what covers the FIRST bind, and it is not redundant with
// FOR UPDATE. FOR UPDATE on a row that does not yet exist locks NOTHING, so two
// concurrent first binds would both read no prior value; the PRIMARY KEY does
// serialize their WRITES (the second INSERT waits on the first's uncommitted
// tuple, then resolves as ON CONFLICT DO UPDATE), but both have already READ by
// then, so both would report "displaced nothing" while the second had in fact
// destroyed the first's live binding — reported to nobody, deliveries stranded.
// Measured, not assumed. The PK orders writes; the displaced value is a read, so
// only a lock taken BEFORE the read can make it correct.
//
// Both locks are kept. The advisory lock alone serializes binds for one account,
// and FOR UPDATE alone cannot cover a first bind; together the read is correct
// whether or not a row already exists, and the row lock still guards against a
// writer that reaches the row by some path not holding the advisory lock.
//
// updated_at is maintained by the set_updated_at() trigger, never here
// (RIG-3495) — the query file assigns it nowhere.
//
// An unknown agent_account_id is ErrInvalidArgument (the FK), and so is an
// agent of another tenant: the FK ignores RLS, but the interval start cannot see
// that agent's row and inserts nothing, so the bind would commit unbilled.
//
// ErrConflict means ONE thing, and it is not about the account: the account path
// is an upsert and cannot conflict. Both unique indexes on this table can raise
// 23505, though — the PK (tenant_id, agent_account_id) as well as
// session_bindings_session_key (tenant_id, session_id) — and the SQLSTATE alone
// does not say which did, so the mapping branches on the CONSTRAINT NAME rather
// than assuming. Only the session key is expected here (the PK is the ON CONFLICT
// arbiter, so it resolves instead of raising); a hit on it means this session id
// is ALREADY BOUND TO A DIFFERENT ACCOUNT. Refusing that keeps
// ResolveSessionAccount single-valued — two accounts sharing a live session id
// would make the relay's answer depend on which row Postgres returned, and it
// resolves the principal a comms call runs under. Any OTHER unique violation is
// unexpected, so it falls through to the generic wrap with its own message
// intact rather than being relabelled as a session collision.
func (s *Store) RecordSessionBinding(ctx context.Context, sessionID string, accountID AccountID, runnerID string) (displaced, version string, err error) {
	if sessionID == "" {
		return "", "", fmt.Errorf("%w: session id is required", ErrInvalidArgument)
	}
	if accountID == "" {
		return "", "", fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	// Unlike agent_placements.runner_id, '' is NOT an accepted unknown sentinel:
	// a placement outlives its Runner, but a binding exists only while a Runner
	// is attached, and runner_id is the sweep key that retires it. A '' binding
	// no real Runner's re-enroll could sweep would linger as a stale session.
	if runnerID == "" {
		return "", "", fmt.Errorf("%w: runner id is required", ErrInvalidArgument)
	}

	// beginTenantTx, not s.pool.Begin: it arms SET LOCAL ROLE + the
	// compass.tenant_id GUC at BEGIN, so every statement below is tenant-scoped
	// by RLS exactly as the single-statement scopedDBTX path is. A raw Begin here
	// would run as the owner with no GUC and silently disable tenant isolation.
	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return "", "", fmt.Errorf("store: begin record session binding: %w", err)
	}
	// No-op after a successful commit; the rollback that matters is on every
	// error path below, where there is nothing further to report about it.
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	// FIRST, before reading anything: serialize every bind for this account.
	// Keyed on (tenant, account) because two tenants may hold bindings for one
	// account id and those binds are independent. Auto-released at tx end.
	if err := qtx.LockSessionBindingAccount(ctx, db.LockSessionBindingAccountParams{
		Column1: pgtype.Text{String: string(s.resolveTenant(ctx)), Valid: true},
		Column2: pgtype.Text{String: string(accountID), Valid: true},
	}); err != nil {
		return "", "", fmt.Errorf("store: lock session binding account: %w", err)
	}

	prior, err := qtx.SessionBindingForUpdate(ctx, string(accountID))
	if err != nil && !noRows(err) {
		return "", "", fmt.Errorf("store: read prior session binding: %w", err)
	}
	if noRows(err) {
		prior = db.SessionBindingForUpdateRow{}
	}
	displaced = prior.SessionID

	intervalID := prior.UsageIntervalID
	if intervalID != "" {
		// An older server may have written the prior row without a start event.
		if err := qtx.EnsureComputeUsageIntervalStart(ctx, string(accountID)); err != nil {
			return "", "", fmt.Errorf("store: ensure compute usage interval start: %w", err)
		}
	}
	if prior.SessionID != sessionID {
		if intervalID != "" {
			if err := qtx.EndComputeUsageInterval(ctx, db.EndComputeUsageIntervalParams{
				IntervalID:     intervalID,
				AgentAccountID: string(accountID),
				SessionID:      prior.SessionID,
				RunnerID:       prior.RunnerID,
			}); err != nil {
				return "", "", fmt.Errorf("store: end displaced compute usage interval: %w", err)
			}
		}
		intervalID = uuid.NewString()
		started, err := qtx.StartComputeUsageInterval(ctx, db.StartComputeUsageIntervalParams{
			IntervalID:     intervalID,
			AgentAccountID: string(accountID),
			SessionID:      sessionID,
			RunnerID:       runnerID,
		})
		if err != nil {
			return "", "", fmt.Errorf("store: start compute usage interval: %w", err)
		}
		if started == 0 {
			return "", "", fmt.Errorf("%w: agent account %q does not exist in this tenant", ErrInvalidArgument, accountID)
		}
	}

	version = uuid.NewString()
	if err := qtx.RecordSessionBinding(ctx, db.RecordSessionBindingParams{
		SessionID:       sessionID,
		AgentAccountID:  string(accountID),
		RunnerID:        runnerID,
		UsageIntervalID: intervalID,
		BindingVersion:  version,
	}); err != nil {
		if pgErrIs(err, pgForeignKeyViolation) {
			return "", "", fmt.Errorf("%w: agent account %q does not exist", ErrInvalidArgument, accountID)
		}
		if pgErrIs(err, pgUniqueViolation) && pgConstraintName(err) == "session_bindings_session_key" {
			return "", "", fmt.Errorf("%w: session %q is already bound to a different agent", ErrConflict, sessionID)
		}
		return "", "", fmt.Errorf("store: record session binding: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", fmt.Errorf("store: commit record session binding: %w", err)
	}
	return displaced, version, nil
}

// ResolveSessionBinding resolves the account and Runner a live session speaks
// for — the relay's read on every inbound Runner-originated call.
//
// An unbound session is ErrNotFound, and that is FAIL-CLOSED by design: this
// resolves the scope a call runs under, so a miss must never surface as an
// empty AccountID with a nil error. A zero-value account id would flow onward as
// a real (wrong) principal instead of stopping the call.
func (s *Store) ResolveSessionBinding(ctx context.Context, sessionID string) (account AccountID, runnerID, version string, err error) {
	if sessionID == "" {
		return "", "", "", fmt.Errorf("%w: session id is required", ErrInvalidArgument)
	}
	row, err := s.q.SessionBinding(ctx, sessionID)
	if err != nil {
		if noRows(err) {
			return "", "", "", fmt.Errorf("%w: session %q is not bound", ErrNotFound, sessionID)
		}
		return "", "", "", fmt.Errorf("store: resolve session binding: %w", err)
	}
	return AccountID(row.AgentAccountID), row.RunnerID, row.BindingVersion, nil
}

// SessionForAccount resolves the live session bound to an agent account — the
// REVERSE of ResolveSessionAccount, and the direction the delivery consumer
// needs to dispatch a deliver to an already-resolved subscriber
// (runnerhub/relay_comms.go, Hub.SessionForAccount). Returns the owning Runner
// id with the session id. Exactly one row can answer PER TENANT: the table's
// key is (tenant_id, agent_account_id), so the account alone is not unique and
// the query is single-valued only because RLS has already narrowed the visible
// rows to the acting tenant's. Under WithSystemRole (BYPASSRLS, no tenant GUC)
// that narrowing is gone, several tenants' rows can match, and pgx takes
// whichever comes first — so this method is a REQUEST-PATH read. Nothing calls
// it under the system role today; a PR3 caller that wants to must scope it
// itself.
//
// An account with no live session is ErrNotFound — never started, stopped, or
// dropped on a Runner reconnect. Fail-closed for the same reason as above: an
// empty session id with a nil error would be dispatched to as if it were a live
// session. The consumer's own contract turns this into "push nothing now, let
// the cursor sweep deliver on the recipient's next start". version names the
// row's write, as ResolveSessionBinding returns it.
func (s *Store) SessionForAccount(ctx context.Context, accountID AccountID) (sessionID, runnerID, version string, err error) {
	if accountID == "" {
		return "", "", "", fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	row, err := s.q.SessionBindingForAccount(ctx, string(accountID))
	if err != nil {
		if noRows(err) {
			return "", "", "", fmt.Errorf("%w: agent %q has no live session", ErrNotFound, accountID)
		}
		return "", "", "", fmt.Errorf("store: resolve session for account: %w", err)
	}
	return row.SessionID, row.RunnerID, row.BindingVersion, nil
}

// DeleteSessionBinding releases the binding for sessionID — the single-session
// release path a normal session end takes. It is IDEMPOTENT: a session teardown
// may be retried, and a second release of an already-released session must
// succeed, so deleting an absent row is not an error (the same posture as
// DeleteAgentPlacement).
//
// Note it deletes by SESSION, not by account, which is what makes it safe to
// call on a session RecordSessionBinding has already displaced: the row now
// names the newer session, so the stale release matches nothing and leaves the
// live binding alone.
func (s *Store) DeleteSessionBinding(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("%w: session id is required", ErrInvalidArgument)
	}
	if err := s.q.DeleteSessionBinding(ctx, sessionID); err != nil {
		return fmt.Errorf("store: delete session binding: %w", err)
	}
	return nil
}

// DeleteSessionBindingVersion is DeleteSessionBinding limited to the write that
// returned version (from Record or Resolve): a re-bind of the same session id
// since then has a new version and stays. removed reports whether a row went.
func (s *Store) DeleteSessionBindingVersion(ctx context.Context, sessionID, version string) (removed bool, err error) {
	if sessionID == "" {
		return false, fmt.Errorf("%w: session id is required", ErrInvalidArgument)
	}
	n, err := s.q.DeleteSessionBindingVersion(ctx, db.DeleteSessionBindingVersionParams{SessionID: sessionID, BindingVersion: version})
	if err != nil {
		return false, fmt.Errorf("store: delete session binding version: %w", err)
	}
	return n > 0, nil
}

// DeleteSessionBindingsForRunner is the enroll sweep: it releases every binding
// attached to runnerID and RETURNS the bindings it removed. Hub.enroll clears all
// bindings on every Runner enroll, because an enrolling Runner has no live sessions
// and a surviving binding would resolve a dead or re-minted session id to a stale account.
//
// The returned slice is load-bearing, not diagnostic. Each released binding
// drives a presence DISCONNECTED edge for its account (RIG-1569 T8) and each
// released session id must be reaped from the delivery held-deliver registry
// (RIG-1569 T3); enroll emits no lifecycle frames of its own, so a caller that
// deleted without reading the rows back would leave a long-WORKING agent stuck
// WORKING in the projection forever. That is why the query is :many with
// RETURNING rather than a bare DELETE.
//
// Sorted by session id: a DELETE ... RETURNING has no ORDER BY, and a sweep pass
// should be deterministic and its logs diffable across runs. A Runner holding no
// bindings yields an empty slice, not an error — a first-ever enroll sweeps
// nothing, which is a normal outcome.
func (s *Store) DeleteSessionBindingsForRunner(ctx context.Context, runnerID string) ([]SessionBinding, error) {
	if runnerID == "" {
		return nil, fmt.Errorf("%w: runner id is required", ErrInvalidArgument)
	}
	rows, err := s.q.DeleteSessionBindingsForRunner(ctx, runnerID)
	if err != nil {
		return nil, fmt.Errorf("store: delete session bindings for runner: %w", err)
	}
	bindings := make([]SessionBinding, 0, len(rows))
	for _, row := range rows {
		bindings = append(bindings, SessionBinding{
			TenantID:  TenantID(row.TenantID),
			SessionID: row.SessionID,
			AccountID: AccountID(row.AgentAccountID),
			RunnerID:  runnerID,
		})
	}
	slices.SortFunc(bindings, func(a, b SessionBinding) int {
		return cmp.Compare(a.SessionID, b.SessionID)
	})
	return bindings, nil
}

// SessionBindingTenant names the tenant whose binding maps sessionID to runnerID.
// It is the system-role read a Runner-originated call needs first: the Runner
// door carries no tenant, so the hub resolves it here, then acts under WithTenant.
// ErrNotFound when no tenant binds the pair; ErrConflict when several do, so an
// ambiguous session never resolves to an arbitrary tenant.
func (s *Store) SessionBindingTenant(ctx context.Context, sessionID, runnerID string) (TenantID, error) {
	if sessionID == "" || runnerID == "" {
		return "", fmt.Errorf("%w: session id and runner id are required", ErrInvalidArgument)
	}
	tenants, err := s.q.SessionBindingTenants(ctx, db.SessionBindingTenantsParams{SessionID: sessionID, RunnerID: runnerID})
	if err != nil {
		return "", fmt.Errorf("store: resolve session binding tenant: %w", err)
	}
	switch len(tenants) {
	case 0:
		return "", fmt.Errorf("%w: session %q is not bound to runner %q", ErrNotFound, sessionID, runnerID)
	case 1:
		return TenantID(tenants[0]), nil
	default:
		return "", fmt.Errorf("%w: session %q is bound in %d tenants", ErrConflict, sessionID, len(tenants))
	}
}
