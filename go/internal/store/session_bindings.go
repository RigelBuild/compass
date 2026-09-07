package store

import (
	"cmp"
	"context"
	"fmt"
	"slices"

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

// RecordSessionBinding persists the binding for a live session. It is an UPSERT
// keyed on the session, because a session id names exactly one binding: a rebind
// REPLACES it, updating agent_account_id and runner_id TOGETHER so a row can
// never pair a fresh Runner with the account from a previous attachment.
//
// updated_at is maintained by the set_updated_at() trigger, never here
// (RIG-3495) — the query file assigns it nowhere.
//
// An unknown agent_account_id is ErrInvalidArgument (the FK).
//
// An account that already holds a DIFFERENT live session is ErrConflict (the
// unique index on agent_account_id), and unlike agent_placements' container
// guard this one is a live safety property, not a latent one. The in-RAM
// accountSessions map this replaces is 1:1, so a second concurrent session for
// one account would split that account's deliveries across two sessions and let
// SessionForAccount resolve whichever row Postgres happened to return. Refusing
// the write makes the double-bind unrepresentable. The caller's remedy is to
// release the stale binding first — DeleteSessionBinding for a session it knows
// is gone, or the runner sweep on a re-enroll.
func (s *Store) RecordSessionBinding(ctx context.Context, sessionID string, accountID AccountID, runnerID string) error {
	if sessionID == "" {
		return fmt.Errorf("%w: session id is required", ErrInvalidArgument)
	}
	if accountID == "" {
		return fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	// Unlike agent_placements.runner_id, '' is NOT an accepted unknown-runner
	// sentinel here. A placement must OUTLIVE its Runner's attachment (it is
	// where the agent runs, and the next provision self-heals the sentinel), but
	// a binding exists ONLY while a Runner is attached, and runner_id is the
	// sweep key that retires it. A binding stamped '' could never be swept by
	// any real Runner's re-enroll, so it would linger as a stale session that
	// outlives its Runner — the exact leak the sweep exists to prevent.
	if runnerID == "" {
		return fmt.Errorf("%w: runner id is required", ErrInvalidArgument)
	}
	if err := s.q.RecordSessionBinding(ctx, db.RecordSessionBindingParams{
		SessionID:      sessionID,
		AgentAccountID: string(accountID),
		RunnerID:       runnerID,
	}); err != nil {
		if pgErrIs(err, pgForeignKeyViolation) {
			return fmt.Errorf("%w: agent account %q does not exist", ErrInvalidArgument, accountID)
		}
		if pgErrIs(err, pgUniqueViolation) {
			return fmt.Errorf("%w: agent %q is already bound to another live session", ErrConflict, accountID)
		}
		return fmt.Errorf("store: record session binding: %w", err)
	}
	return nil
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
// (runnerhub/relay_comms.go:179-184). At most one row can answer, because the
// unique index makes the mapping 1:1.
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
// release path a normal session end takes, freeing the account's unique slot for
// its next session. It is IDEMPOTENT: a session teardown may be retried, and a
// second release of an already-released session must succeed, so deleting an
// absent row is not an error (the same posture as DeleteAgentPlacement).
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
