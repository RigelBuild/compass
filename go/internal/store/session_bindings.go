package store

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// Session bindings: the durable record of WHICH LIVE SESSION speaks for an agent
// account, and the Runner that session is attached to (RIG-3108 / RIG-2861 §T4).
// Until now the RunnerHub held this only in RAM (sessionAccounts, accountSessions),
// so a Server restart lost every binding and the relay resolved nothing for a
// session it had itself minted moments earlier.
//
// A binding is NOT authorization, exactly as agent_placements is not:
// SubscribeAgentSession authorizes through agent_sessions -> agent_accounts ->
// channel_members and never reads this table. What a binding is for is the two
// reads below — the relay resolving the account that owns an inbound session
// (accountForSession), and the delivery consumer resolving the live session of a
// recipient account it has already authorized (SessionForAccount).
//
// Nor is a binding cross-checked against agent_sessions: there is deliberately
// no FK from session_id, so a binding may name a session with no agent_sessions
// row, or disagree with one about the owner. agent_sessions remains the authz
// root, and nothing read from here may stand in for it.
//
// PR2 adds the table and these methods only. Demoting the hub's in-RAM maps to
// caches over this table is PR3; nothing in internal/runnerhub reads this yet.

// SessionBinding is one live binding: the session, the agent account it speaks
// for, and the Runner it is attached to. Returned by
// DeleteSessionBindingsForRunner, which is the reconnect sweep — its caller
// needs every field of each binding it just removed.
type SessionBinding struct {
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
// So the bind is THREE statements in one explicit transaction, and that is the
// whole reason this method opens a tx at all: a per-account advisory lock, then
// the prior-value read under FOR UPDATE, then the write. A concurrent bind for
// the same account parks on the advisory lock until this transaction commits, so
// it cannot interleave between this read and this write — the two binds
// serialize, and each caller is told the session IT actually displaced.
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
// An unknown agent_account_id is ErrInvalidArgument (the FK).
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
func (s *Store) RecordSessionBinding(ctx context.Context, sessionID string, accountID AccountID, runnerID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("%w: session id is required", ErrInvalidArgument)
	}
	if accountID == "" {
		return "", fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	// Unlike agent_placements.runner_id, '' is NOT an accepted unknown-runner
	// sentinel here. A placement must OUTLIVE its Runner's attachment (it is
	// where the agent runs, and the next provision self-heals the sentinel), but
	// a binding exists ONLY while a Runner is attached, and runner_id is the
	// sweep key that retires it. A binding stamped '' could never be swept by
	// any real Runner's re-enroll, so it would linger as a stale session that
	// outlives its Runner — the exact leak the sweep exists to prevent.
	if runnerID == "" {
		return "", fmt.Errorf("%w: runner id is required", ErrInvalidArgument)
	}

	// beginTenantTx, not s.pool.Begin: it arms SET LOCAL ROLE + the
	// compass.tenant_id GUC at BEGIN, so every statement below is tenant-scoped
	// by RLS exactly as the single-statement scopedDBTX path is. A raw Begin here
	// would run as the owner with no GUC and silently disable tenant isolation.
	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin record session binding: %w", err)
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
		return "", fmt.Errorf("store: lock session binding account: %w", err)
	}

	displaced, err := qtx.SessionBindingForUpdate(ctx, string(accountID))
	if err != nil && !noRows(err) {
		return "", fmt.Errorf("store: read prior session binding: %w", err)
	}
	if noRows(err) {
		// No prior binding: nothing to displace. Scan left displaced at its zero
		// value, which is already the empty session id this method reports for
		// "the account held none".
		displaced = ""
	}

	if err := qtx.RecordSessionBinding(ctx, db.RecordSessionBindingParams{
		SessionID:      sessionID,
		AgentAccountID: string(accountID),
		RunnerID:       runnerID,
	}); err != nil {
		if pgErrIs(err, pgForeignKeyViolation) {
			return "", fmt.Errorf("%w: agent account %q does not exist", ErrInvalidArgument, accountID)
		}
		if pgErrIs(err, pgUniqueViolation) && pgConstraintName(err) == "session_bindings_session_key" {
			return "", fmt.Errorf("%w: session %q is already bound to a different agent", ErrConflict, sessionID)
		}
		return "", fmt.Errorf("store: record session binding: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit record session binding: %w", err)
	}
	return displaced, nil
}

// ResolveSessionAccount resolves the agent account a live session speaks for —
// the relay's read on every inbound comms call, where the request carries only
// the session id.
//
// An unbound session is ErrNotFound, and that is FAIL-CLOSED by design: this
// resolves the scope a comms call runs under, so a miss must never surface as an
// empty AccountID with a nil error. A zero-value account id would flow onward as
// a real (wrong) principal instead of stopping the call.
func (s *Store) ResolveSessionAccount(ctx context.Context, sessionID string) (AccountID, error) {
	if sessionID == "" {
		return "", fmt.Errorf("%w: session id is required", ErrInvalidArgument)
	}
	accountID, err := s.q.SessionBindingAccount(ctx, sessionID)
	if err != nil {
		if noRows(err) {
			return "", fmt.Errorf("%w: session %q is not bound", ErrNotFound, sessionID)
		}
		return "", fmt.Errorf("store: resolve session account: %w", err)
	}
	return AccountID(accountID), nil
}

// SessionForAccount resolves the live session bound to an agent account — the
// REVERSE of ResolveSessionAccount, and the direction the delivery consumer
// needs to dispatch a deliver to an already-resolved subscriber
// (runnerhub/relay_comms.go, Hub.SessionForAccount). Exactly one row can answer PER TENANT: the
// table's key is (tenant_id, agent_account_id), so the account alone is not
// unique and the query is single-valued only because RLS has already narrowed
// the visible rows to the acting tenant's. Under WithSystemRole (BYPASSRLS, no
// tenant GUC) that narrowing is gone, several tenants' rows can match, and pgx
// takes whichever comes first — so this method is a REQUEST-PATH read. Nothing
// calls it under the system role today; a PR3 caller that wants to must scope it
// itself.
//
// An account with no live session is ErrNotFound — never started, stopped, or
// dropped on a Runner reconnect. Fail-closed for the same reason as above: an
// empty session id with a nil error would be dispatched to as if it were a live
// session. The consumer's own contract turns this into "push nothing now, let
// the cursor sweep deliver on the recipient's next start".
func (s *Store) SessionForAccount(ctx context.Context, accountID AccountID) (string, error) {
	if accountID == "" {
		return "", fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	sessionID, err := s.q.SessionBindingForAccount(ctx, string(accountID))
	if err != nil {
		if noRows(err) {
			return "", fmt.Errorf("%w: agent %q has no live session", ErrNotFound, accountID)
		}
		return "", fmt.Errorf("store: resolve session for account: %w", err)
	}
	return sessionID, nil
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

// DeleteSessionBindingsForRunner is the reconnect sweep: it releases every
// binding attached to runnerID and RETURNS the bindings it removed. Hub.enroll
// (runnerhub/hub.go:905-957) clears all bindings when a Runner (re-)enrolls,
// because a reconnecting Runner has no live sessions and a surviving binding
// would resolve a re-minted session id to a stale account.
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
