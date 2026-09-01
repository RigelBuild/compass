package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// The DL-055 forge ownership index (design
// docs/designs/server/compass-forge-write-path/design.md §T7): the durable
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
	if err := s.q.RecordAuthoredArtifact(ctx, db.RecordAuthoredArtifactParams{
		ForgeProvider:   int16(a.Provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum (forge_authored_artifacts.forge_provider), always within int16
		ForgeHost:       a.Host,
		Repo:            a.Repo,
		Kind:            int16(a.Kind),   //nolint:gosec // G115: ForgeArtifactKind is a CHECK-constrained 1/2 enum (forge_authored_artifacts.kind), always within int16
		Number:          int64(a.Number), //nolint:gosec // G115: number is a canonical forge artifact number (a positive issue/PR number) written to a BIGINT, always well within the int64 domain — never near the uint64 ceiling.
		AgentAccountID:  string(a.AgentAccountID),
		OwnerUserID:     string(a.OwnerUserID),
		SessionID:       a.SessionID,
		ClientRequestID: textOrNull(a.ClientRequestID),
		CreatedAtUnixMs: a.CreatedAtUnixMS,
	}); err != nil {
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
	row, err := s.q.AuthoredArtifactByRequestID(ctx, db.AuthoredArtifactByRequestIDParams{
		AgentAccountID:  string(agent),
		ClientRequestID: pgtype.Text{String: clientRequestID, Valid: true},
	})
	if err != nil {
		if noRows(err) {
			return AuthoredArtifact{}, false, nil
		}
		return AuthoredArtifact{}, false, fmt.Errorf("store: read authored artifact by request id: %w", err)
	}
	return authoredArtifactFromRow(row), true, nil
}

// AuthoredArtifactByCoordinate reads the ownership row at a forge coordinate —
// the by-coordinate lookup T4 uses to resolve a delegated issue's recorded
// authoring agent (design compass-linear-agent-responder §Part 2). The forge
// coordinate (provider, host, repo, kind, number) IS the
// forge_authored_artifacts PK, so this is a trivial PK lookup: an unknown
// coordinate is ErrNotFound; zero/empty coordinate fields (or a zero kind) are
// ErrInvalidArgument, mirroring the validation RecordAuthoredArtifact applies.
func (s *Store) AuthoredArtifactByCoordinate(ctx context.Context, provider ForgeProvider, host, repo string, kind ForgeArtifactKind, number uint64) (AuthoredArtifact, error) {
	if err := validCoordinate(provider, host, repo); err != nil {
		return AuthoredArtifact{}, err
	}
	if kind == ForgeArtifactKindUnspecified {
		return AuthoredArtifact{}, fmt.Errorf("%w: artifact kind is required", ErrInvalidArgument)
	}
	row, err := s.q.AuthoredArtifactByCoordinate(ctx, db.AuthoredArtifactByCoordinateParams{
		ForgeProvider: int16(provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum, always within int16
		ForgeHost:     host,
		Repo:          repo,
		Kind:          int16(kind),   //nolint:gosec // G115: ForgeArtifactKind is a CHECK-constrained 1/2 enum, always within int16
		Number:        int64(number), //nolint:gosec // G115: number is a canonical forge artifact number (a positive issue/PR number) written to a BIGINT, always well within the int64 domain.
	})
	if err != nil {
		if noRows(err) {
			return AuthoredArtifact{}, fmt.Errorf("%w: authored artifact at coordinate %d/%s/%s kind %d number %d", ErrNotFound, provider, host, repo, kind, number)
		}
		return AuthoredArtifact{}, fmt.Errorf("store: read authored artifact by coordinate: %w", err)
	}
	return authoredArtifactFromRow(row), nil
}

// ListAuthoredArtifactsByAgent reads every artifact the agent authored, ordered
// deterministically by created_at then coordinate. No rows is a nil slice, not
// an error. Zero agent -> ErrInvalidArgument.
func (s *Store) ListAuthoredArtifactsByAgent(ctx context.Context, agent AccountID) ([]AuthoredArtifact, error) {
	if agent == "" {
		return nil, fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	rows, err := s.q.ListAuthoredArtifactsByAgent(ctx, string(agent))
	if err != nil {
		return nil, fmt.Errorf("store: list authored artifacts by agent: %w", err)
	}
	var out []AuthoredArtifact
	for _, r := range rows {
		out = append(out, authoredArtifactFromRow(r))
	}
	return out, nil
}

// authoredArtifactFromRow maps a generated forge_authored_artifacts row into an
// AuthoredArtifact, mapping the nullable client_request_id column to "" (no key)
// and the int16/BIGINT columns back to their named/uint types.
func authoredArtifactFromRow(r db.ForgeAuthoredArtifact) AuthoredArtifact {
	a := AuthoredArtifact{
		Provider:        ForgeProvider(r.ForgeProvider),
		Host:            r.ForgeHost,
		Repo:            r.Repo,
		Kind:            ForgeArtifactKind(r.Kind),
		Number:          uint64(r.Number), //nolint:gosec // G115: number is a BIGINT written only from a canonical uint64 artifact number, so the stored value is always within the uint64 domain.
		AgentAccountID:  AccountID(r.AgentAccountID),
		OwnerUserID:     AccountID(r.OwnerUserID),
		SessionID:       r.SessionID,
		CreatedAtUnixMS: r.CreatedAtUnixMs,
	}
	if r.ClientRequestID.Valid {
		a.ClientRequestID = r.ClientRequestID.String
	}
	return a
}

// textOrNull maps an empty string (no value supplied) to an invalid pgtype.Text
// so it stores as SQL NULL. Used where a generated query parameter is a
// pgtype.Text: client_request_id (the partial unique memo index constrains only
// non-NULL keys, so null-key rows never collide) and linear_issue_id (plain
// nullable provenance, no unique index — NULL is faithful "none" storage).
func textOrNull(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}
