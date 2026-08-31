package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// IssueState mirrors compass.v1 IssueState (UNSPECIFIED=0 .. ARCHIVED=8). A
// PERSISTED issue is never UNSPECIFIED — a boarded issue always has a real
// lifecycle (the CHECK enforces 1..8). The zero value is not a valid stored
// state.
type IssueState int32

const (
	IssueStateUnspecified IssueState = 0 // never persisted; the proto zero
	IssueStateBacklog     IssueState = 1
	IssueStateTodo        IssueState = 2
	IssueStateQueued      IssueState = 3
	IssueStateBlocked     IssueState = 4
	IssueStateInProgress  IssueState = 5
	IssueStateInReview    IssueState = 6
	IssueStateDone        IssueState = 7
	IssueStateArchived    IssueState = 8
)

// ForgeProvider mirrors compass.v1 ForgeProvider (UNSPECIFIED=0, GITHUB=1,
// GITLAB=2, FORGEJO=3, LINEAR=4). Part of the forge coordinate.
type ForgeProvider int32

const (
	ForgeProviderUnspecified ForgeProvider = 0
	ForgeProviderGitHub      ForgeProvider = 1
	ForgeProviderGitLab      ForgeProvider = 2
	ForgeProviderForgejo     ForgeProvider = 3
	ForgeProviderLinear      ForgeProvider = 4
)

// Issue is the store-native canonical board issue: forge-derived fields plus
// Compass-owned machinery, durable in Postgres (DL-019). Mirrors compass.v1
// Issue's persistable fields; the projection maps this <-> the wire type at its
// edge (this package imports no generated code). Prs and Tracker are NOT here
// in this slice — PR ingestion and the write-path tracker mirror own them and
// add their storage in their own slices.
type Issue struct {
	ID string // surrogate Compass-local id, the stable join key

	// Forge coordinate — the idempotency key (UNIQUE). Re-poll of the same
	// coordinate updates the existing row, keeping ID stable.
	ForgeProvider ForgeProvider
	ForgeHost     string
	Repo          string
	Number        uint32

	// Forge fields (written by UpsertIssueForgeFields; owner header already
	// stripped upstream by the Service, DL-050 — this layer stores verbatim).
	Title        string
	Body         string
	ForgeState   string // "open" | "closed" — forge truth, NOT the lifecycle
	URL          string
	ForgeAccount string // the native forge login; always set
	Labels       []string
	AgentHandle  string // parsed Compass attribution; "" = non-Compass (human) author

	// Compass machinery (Compass-owned; not on the forge). State is set by the
	// write path (part 5); the other four by their own producers (assignee =
	// Dispatcher, summary = events, branch = VCS, priority = tracker-ingest).
	// No setter for them in THIS slice — a setter lands with its producer.
	State    IssueState
	Priority string
	Assignee string
	Summary  string
	Branch   string
}

// IssueForgeFields is the ingestion write input: the forge coordinate plus the
// forge-derived fields, and NOTHING Compass-owned. A dedicated type so an
// ingestion caller cannot pass a machinery field into the forge-only upsert.
type IssueForgeFields struct {
	ForgeProvider ForgeProvider
	ForgeHost     string
	Repo          string
	Number        uint32
	Title         string
	Body          string
	ForgeState    string
	URL           string
	ForgeAccount  string
	Labels        []string
	AgentHandle   string
	// ForgeUpdatedAt is the forge's last-updated timestamp for the artifact,
	// the OQ-6(a) recency-guard input (RIG-2883 T4a). The zero value means
	// "unset": it stores SQL NULL and the ON CONFLICT guard's NULL arm keeps
	// the write additive, so a writer that does not populate it still upserts.
	ForgeUpdatedAt time.Time
}

// UpsertIssueForgeFields inserts-or-updates the issue at in's forge coordinate,
// minting a surrogate id on first insert and returning the stable id. It is the
// ingestion write: a re-poll of the same coordinate UPDATES the existing row,
// so the returned id is stable across polls (RETURNING id yields the EXISTING
// id on conflict — id is not in the SET).
//
// The ON CONFLICT DO UPDATE sets ONLY the forge columns — never state or any
// machinery column. This is the load-bearing property: a re-poll must not
// clobber a human-set lifecycle state (part 5's SetIssueState) or any
// server-owned field a later producer wrote.
//
// The OQ-6(a) recency guard (RIG-2883 T4a) makes the UPDATE conditional: when
// both the stored row and the incoming write carry a forge_updated_at, a write
// whose timestamp is OLDER than the stored one is skipped, so an out-of-order
// (stale) re-sink never overwrites a fresher row. The guard is ADDITIVE — a
// write that leaves forge_updated_at unset (SQL NULL on either side) always
// applies, so an unthreaded writer is never regressed. A guard-SKIPPED write is
// NOT an error: the coordinate already exists, so the id is still returned (the
// CTE's fallback SELECT), keeping the returned id stable across polls. Empty
// Repo/ForgeHost → ErrInvalidArgument.
func (s *Store) UpsertIssueForgeFields(ctx context.Context, in IssueForgeFields) (string, error) {
	if in.Repo == "" {
		return "", fmt.Errorf("%w: repo is required", ErrInvalidArgument)
	}
	if in.ForgeHost == "" {
		return "", fmt.Errorf("%w: forge host is required", ErrInvalidArgument)
	}
	// A nil []string marshals to SQL NULL, which the labels NOT NULL column
	// rejects; coerce to an empty slice so an issue with no labels stores '{}'.
	labels := in.Labels
	if labels == nil {
		labels = []string{}
	}
	// A zero time stores SQL NULL so the recency guard's NULL arm keeps the
	// write additive; a set time drives the >= comparison in ON CONFLICT.
	var forgeUpdatedAt pgtype.Timestamptz
	if !in.ForgeUpdatedAt.IsZero() {
		forgeUpdatedAt = pgtype.Timestamptz{Time: in.ForgeUpdatedAt, Valid: true}
	}
	id, err := s.q.UpsertIssueForgeFields(ctx, db.UpsertIssueForgeFieldsParams{
		ID:             newID(),
		ForgeProvider:  int16(in.ForgeProvider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum (issues.forge_provider), always within int16
		ForgeHost:      in.ForgeHost,
		Repo:           in.Repo,
		Number:         int64(in.Number),
		Title:          in.Title,
		Body:           in.Body,
		ForgeState:     in.ForgeState,
		Url:            in.URL,
		ForgeAccount:   in.ForgeAccount,
		Labels:         labels,
		AgentHandle:    in.AgentHandle,
		ForgeUpdatedAt: forgeUpdatedAt,
	})
	if err != nil {
		return "", fmt.Errorf("store: upsert issue forge fields: %w", err)
	}
	return id, nil
}

// SetIssueState durably writes the lifecycle column for the issue with the
// given id. It is the raw store write: the compare-and-transition semantics
// (reject UNSPECIFIED, no-op on same, return truth) are the part-5 executor's
// job, not here. An unknown id is ErrNotFound (0 rows affected). Empty id →
// ErrInvalidArgument. The CHECK 1..8 rejects a stray 0 at the DB as a backstop.
func (s *Store) SetIssueState(ctx context.Context, id string, state IssueState) error {
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidArgument)
	}
	affected, err := s.q.SetIssueState(ctx, db.SetIssueStateParams{
		ID:    id,
		State: int16(state), //nolint:gosec // G115: IssueState is a CHECK-constrained 1..8 enum (issues.state), always within int16
	})
	if err != nil {
		return fmt.Errorf("store: set issue state: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: issue %q does not exist", ErrNotFound, id)
	}
	return nil
}

// GetIssue reads the issue with the given id. An unknown id is ErrNotFound.
// Empty id → ErrInvalidArgument. An empty labels array round-trips as nil
// (the module empty→nil contract), not []string{}.
func (s *Store) GetIssue(ctx context.Context, id string) (Issue, error) {
	if id == "" {
		return Issue{}, fmt.Errorf("%w: id is required", ErrInvalidArgument)
	}
	row, err := s.q.GetIssue(ctx, id)
	if err != nil {
		if noRows(err) {
			return Issue{}, fmt.Errorf("%w: issue %q does not exist", ErrNotFound, id)
		}
		return Issue{}, fmt.Errorf("store: get issue: %w", err)
	}
	return issueFromGetRow(row), nil
}

// ListIssues reads every issue, ordered by id for a deterministic result (like
// ListAgentPlacementsForRunner). It is the projection's rehydrate read (part
// 4). An empty table yields a non-nil empty slice, not an error.
func (s *Store) ListIssues(ctx context.Context) ([]Issue, error) {
	rows, err := s.q.ListIssues(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list issues: %w", err)
	}
	issues := make([]Issue, 0, len(rows))
	for _, r := range rows {
		issues = append(issues, issueFromListRow(r))
	}
	return issues, nil
}

// issueFromGetRow maps a generated GetIssue row into a domain Issue. The
// forge_provider/state int16 columns convert to their named types; number is a
// BIGINT written only from a canonical uint32; an empty labels array normalizes
// to nil to match the module's empty→nil contract.
func issueFromGetRow(r db.GetIssueRow) Issue {
	return issueFromColumns(r.ID, r.ForgeProvider, r.ForgeHost, r.Repo, r.Number,
		r.Title, r.Body, r.ForgeState, r.Url, r.ForgeAccount, r.Labels, r.AgentHandle,
		r.State, r.Priority, r.Assignee, r.Summary, r.Branch)
}

// issueFromListRow maps a generated ListIssues row into a domain Issue (identical
// column set to GetIssue; sqlc emits a distinct row type per query).
func issueFromListRow(r db.ListIssuesRow) Issue {
	return issueFromColumns(r.ID, r.ForgeProvider, r.ForgeHost, r.Repo, r.Number,
		r.Title, r.Body, r.ForgeState, r.Url, r.ForgeAccount, r.Labels, r.AgentHandle,
		r.State, r.Priority, r.Assignee, r.Summary, r.Branch)
}

// issueFromColumns builds an Issue from the shared issue projection both reads
// select, folding the int16→named-type conversions, the uint32 number narrowing,
// and the empty-labels→nil normalization into one place.
func issueFromColumns(
	id string, forgeProvider int16, forgeHost, repo string, number int64,
	title, body, forgeState, url, forgeAccount string, labels []string, agentHandle string,
	state int16, priority, assignee, summary, branch string,
) Issue {
	iss := Issue{
		ID:            id,
		ForgeProvider: ForgeProvider(forgeProvider),
		ForgeHost:     forgeHost,
		Repo:          repo,
		Number:        uint32(number), //nolint:gosec // G115: number is a BIGINT written only from a canonical uint32 (UpsertIssueForgeFields narrows in.Number), so it is always within the uint32 domain
		Title:         title,
		Body:          body,
		ForgeState:    forgeState,
		URL:           url,
		ForgeAccount:  forgeAccount,
		Labels:        labels,
		AgentHandle:   agentHandle,
		State:         IssueState(state),
		Priority:      priority,
		Assignee:      assignee,
		Summary:       summary,
		Branch:        branch,
	}
	if len(iss.Labels) == 0 {
		iss.Labels = nil
	}
	return iss
}
