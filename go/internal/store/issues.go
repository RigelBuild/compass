package store

import (
	"context"
	"fmt"
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
// server-owned field a later producer wrote. Empty Repo/ForgeHost →
// ErrInvalidArgument.
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
	var id string
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO issues
		     (id, forge_provider, forge_host, repo, number,
		      title, body, forge_state, url, forge_account, labels, agent_handle)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		 ON CONFLICT (forge_provider, forge_host, repo, number) DO UPDATE
		    SET title = EXCLUDED.title, body = EXCLUDED.body,
		        forge_state = EXCLUDED.forge_state, url = EXCLUDED.url,
		        forge_account = EXCLUDED.forge_account, labels = EXCLUDED.labels,
		        agent_handle = EXCLUDED.agent_handle
		 RETURNING id`,
		newID(), int32(in.ForgeProvider), in.ForgeHost, in.Repo, int64(in.Number),
		in.Title, in.Body, in.ForgeState, in.URL, in.ForgeAccount, labels, in.AgentHandle,
	).Scan(&id); err != nil {
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
	tag, err := s.pool.Exec(ctx,
		`UPDATE issues SET state = $2 WHERE id = $1`,
		id, int32(state),
	)
	if err != nil {
		return fmt.Errorf("store: set issue state: %w", err)
	}
	if tag.RowsAffected() == 0 {
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
	row := s.pool.QueryRow(ctx,
		`SELECT id, forge_provider, forge_host, repo, number,
		        title, body, forge_state, url, forge_account, labels, agent_handle,
		        state, priority, assignee, summary, branch
		   FROM issues
		  WHERE id = $1`,
		id,
	)
	iss, err := scanIssue(row)
	if err != nil {
		if noRows(err) {
			return Issue{}, fmt.Errorf("%w: issue %q does not exist", ErrNotFound, id)
		}
		return Issue{}, fmt.Errorf("store: get issue: %w", err)
	}
	return iss, nil
}

// ListIssues reads every issue, ordered by id for a deterministic result (like
// ListAgentPlacementsForRunner). It is the projection's rehydrate read (part
// 4). An empty table yields a non-nil empty slice, not an error.
func (s *Store) ListIssues(ctx context.Context) ([]Issue, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, forge_provider, forge_host, repo, number,
		        title, body, forge_state, url, forge_account, labels, agent_handle,
		        state, priority, assignee, summary, branch
		   FROM issues
		  ORDER BY id`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list issues: %w", err)
	}
	defer rows.Close()

	issues := []Issue{}
	for rows.Next() {
		iss, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan issue: %w", err)
		}
		issues = append(issues, iss)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate issues: %w", err)
	}
	return issues, nil
}

// scanRow is the subset of pgx.Row/pgx.Rows scanIssue needs, so it serves both
// the single-row GetIssue and the ListIssues loop.
type scanRow interface {
	Scan(dest ...any) error
}

// scanIssue scans one issues row into an Issue. forge_provider/state are scanned
// through int32 then converted to their named types; an empty labels array is
// normalized to nil to match the module's empty→nil contract.
func scanIssue(row scanRow) (Issue, error) {
	var (
		iss           Issue
		forgeProvider int32
		number        int64
		state         int32
	)
	if err := row.Scan(
		&iss.ID, &forgeProvider, &iss.ForgeHost, &iss.Repo, &number,
		&iss.Title, &iss.Body, &iss.ForgeState, &iss.URL, &iss.ForgeAccount, &iss.Labels, &iss.AgentHandle,
		&state, &iss.Priority, &iss.Assignee, &iss.Summary, &iss.Branch,
	); err != nil {
		return Issue{}, err
	}
	iss.ForgeProvider = ForgeProvider(forgeProvider)
	iss.Number = uint32(number) //nolint:gosec // G115: number is a BIGINT written only from a canonical uint32 (UpsertIssueForgeFields narrows in.Number), so it is always within the uint32 domain
	iss.State = IssueState(state)
	if len(iss.Labels) == 0 {
		iss.Labels = nil
	}
	return iss, nil
}
