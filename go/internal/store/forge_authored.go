package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The DL-055 forge ownership index (design
// docs/designs/product/compass-forge-write-path/design.md §T7): the durable
// record of every forge artifact Compass AUTHORED on behalf of an agent. The
// write chokepoint (T4) records the row AND the F3 idempotency memo in one
// statement on a create success; a provider error records nothing. The dedup
// lookup (AuthoredArtifactByRequestID) reads the memo before a write to collapse
// a retry carrying the same client_request_id onto the already-authored artifact.

// ForgeArtifactKind is the store-side artifact kind on the forge coordinate:
// an issue or a pull request. Mirrors the wire kind (issue=1, pull_request=2)
// and the migration's kind CHECK IN (1, 2); never 0 on a persisted row.
type ForgeArtifactKind int32

const (
	ForgeArtifactKindUnspecified ForgeArtifactKind = 0 // never persisted; the wire zero
	ForgeArtifactKindIssue       ForgeArtifactKind = 1
	ForgeArtifactKindPullRequest ForgeArtifactKind = 2
)

// AuthoredArtifact is one row of the ownership index: the forge coordinate an
// authored write minted, the agent it was authored for and that agent's owning
// user, the session that drove it, the F3 idempotency memo key, and the birth
// time. ClientRequestID "" means the caller supplied no key — stored as SQL
// NULL so null-key rows never collide under the partial unique memo index.
type AuthoredArtifact struct {
	Provider ForgeProvider
	Host     string
	Repo     string
	Kind     ForgeArtifactKind
	Number   uint64

	AgentAccountID AccountID
	OwnerUserID    AccountID
	SessionID      string

	ClientRequestID string // F3 idempotency memo key; "" = no key supplied
	CreatedAtUnixMS int64
}

// validArtifact rejects the zero/empty fields RecordAuthoredArtifact guards on
// before any DB round trip: the coordinate (via validCoordinate), a zero kind
// (never UNSPECIFIED(0), the CHECK's job in Go space), and the required
// account ids. A caller bug is ErrInvalidArgument.
func (a AuthoredArtifact) valid() error {
	if err := validCoordinate(a.Provider, a.Host, a.Repo); err != nil {
		return err
	}
	if a.Kind == ForgeArtifactKindUnspecified {
		return fmt.Errorf("%w: artifact kind is required", ErrInvalidArgument)
	}
	if a.AgentAccountID == "" {
		return fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	if a.OwnerUserID == "" {
		return fmt.Errorf("%w: owner user id is required", ErrInvalidArgument)
	}
	return nil
}

// RecordAuthoredArtifact idempotently upserts the ownership row at a's forge
// coordinate, writing the row AND the F3 memo in one statement. A retry of the
// same authored create (same coordinate) re-lands on the PK. Authorship is
// WRITE-ONCE: the DO UPDATE SET deliberately omits agent_account_id and
// owner_user_id, so a re-land never rewrites who authored the artifact (matching
// the issues.go UpsertIssueForgeFields "never clobber owned fields on conflict"
// precedent). A real forge never reuses a coordinate, so the only re-land is a
// same-agent crash-replay where author/owner are invariant anyway — omitting
// them changes nothing on the real path while closing the silent
// ownership-transfer seam. ClientRequestID "" is stored as SQL NULL so null-key
// rows never collide under the partial unique memo index. A duplicate (agent,
// client_request_id) non-null key is ErrConflict; an unknown agent/owner (or a
// mismatched pair) is ErrInvalidArgument (the composite FK RESTRICT). Zero/empty
// fields -> ErrInvalidArgument.
func (s *Store) RecordAuthoredArtifact(ctx context.Context, a AuthoredArtifact) error {
	if err := a.valid(); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO forge_authored_artifacts
		     (forge_provider, forge_host, repo, kind, number,
		      agent_account_id, owner_user_id, session_id, client_request_id, created_at_unix_ms)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 ON CONFLICT (forge_provider, forge_host, repo, kind, number) DO UPDATE
		    SET session_id         = EXCLUDED.session_id,
		        client_request_id  = EXCLUDED.client_request_id,
		        created_at_unix_ms = EXCLUDED.created_at_unix_ms`,
		int32(a.Provider), a.Host, a.Repo, int32(a.Kind), int64(a.Number), //nolint:gosec // G115: number is a canonical forge artifact number (a positive issue/PR number) written to a BIGINT, always well within the int64 domain — never near the uint64 ceiling.
		string(a.AgentAccountID), string(a.OwnerUserID), a.SessionID,
		nullIfEmpty(a.ClientRequestID), a.CreatedAtUnixMS,
	); err != nil {
		if pgErrIs(err, pgUniqueViolation) {
			return fmt.Errorf("%w: client request id %q already authored for agent %q", ErrConflict, a.ClientRequestID, a.AgentAccountID)
		}
		if pgErrIs(err, pgForeignKeyViolation) {
			return fmt.Errorf("%w: unknown agent %q or owner %q", ErrInvalidArgument, a.AgentAccountID, a.OwnerUserID)
		}
		return fmt.Errorf("store: record authored artifact: %w", err)
	}
	return nil
}

// AuthoredArtifactByRequestID is the F3 dedup lookup: the artifact the agent
// authored under clientRequestID, or ok=false on a miss. An empty
// clientRequestID is always a miss (it is never stored — a null-key row carries
// no key to match), never returning a NULL-key row. Zero agent ->
// ErrInvalidArgument.
func (s *Store) AuthoredArtifactByRequestID(ctx context.Context, agent AccountID, clientRequestID string) (AuthoredArtifact, bool, error) {
	if agent == "" {
		return AuthoredArtifact{}, false, fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	if clientRequestID == "" {
		return AuthoredArtifact{}, false, nil
	}
	row := s.pool.QueryRow(ctx,
		`SELECT forge_provider, forge_host, repo, kind, number,
		        agent_account_id, owner_user_id, session_id, client_request_id, created_at_unix_ms
		   FROM forge_authored_artifacts
		  WHERE agent_account_id = $1 AND client_request_id = $2`,
		string(agent), clientRequestID,
	)
	a, err := scanAuthoredArtifact(row)
	if err != nil {
		if noRows(err) {
			return AuthoredArtifact{}, false, nil
		}
		return AuthoredArtifact{}, false, fmt.Errorf("store: read authored artifact by request id: %w", err)
	}
	return a, true, nil
}

// ListAuthoredArtifactsByAgent reads every artifact the agent authored, ordered
// deterministically by created_at then coordinate. No rows is a nil slice, not
// an error. Zero agent -> ErrInvalidArgument.
func (s *Store) ListAuthoredArtifactsByAgent(ctx context.Context, agent AccountID) ([]AuthoredArtifact, error) {
	if agent == "" {
		return nil, fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT forge_provider, forge_host, repo, kind, number,
		        agent_account_id, owner_user_id, session_id, client_request_id, created_at_unix_ms
		   FROM forge_authored_artifacts
		  WHERE agent_account_id = $1
		  ORDER BY created_at_unix_ms ASC, forge_provider ASC, forge_host ASC, repo ASC, kind ASC, number ASC`,
		string(agent),
	)
	if err != nil {
		return nil, fmt.Errorf("store: list authored artifacts by agent: %w", err)
	}
	defer rows.Close()

	var out []AuthoredArtifact
	for rows.Next() {
		a, err := scanAuthoredArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan authored artifact: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate authored artifacts: %w", err)
	}
	return out, nil
}

// scanAuthoredArtifact scans one row into an AuthoredArtifact, mapping the
// nullable client_request_id column to "" (no key) via a pgx-native scan.
func scanAuthoredArtifact(row pgx.Row) (AuthoredArtifact, error) {
	var (
		a        AuthoredArtifact
		provider int32
		kind     int32
		number   int64
		agent    string
		owner    string
		reqID    *string
	)
	if err := row.Scan(&provider, &a.Host, &a.Repo, &kind, &number,
		&agent, &owner, &a.SessionID, &reqID, &a.CreatedAtUnixMS); err != nil {
		return AuthoredArtifact{}, err
	}
	a.Provider = ForgeProvider(provider)
	a.Kind = ForgeArtifactKind(kind)
	a.Number = uint64(number) //nolint:gosec // G115: number is a BIGINT written only from a canonical uint64 artifact number (RecordAuthoredArtifact narrows nothing), so the stored value is always within the uint64 domain.
	a.AgentAccountID = AccountID(agent)
	a.OwnerUserID = AccountID(owner)
	if reqID != nil {
		a.ClientRequestID = *reqID
	}
	return a, nil
}

// nullIfEmpty maps the empty client_request_id (no key supplied) to a typed nil
// so it stores as SQL NULL — the partial unique memo index only constrains
// non-NULL keys, so null-key rows never collide.
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
