//go:build unix

// The agent-initiated board-write leg: boardService implements runnerhub.BoardCaller,
// running the ONE compare-and-transition executor every state producer shares. The
// caller AccountID is resolved Server-side (recorded, no scope rejection yet). A
// tool-level failure is returned IN-BAND; a resolution miss is a transport error.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"connectrpc.com/connect/v2"

	"github.com/RigelBuild/compass/go/internal/board"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// SourceKind names which producer moved an issue's canonical state. Attribution
// only in this slice; the type is defined fully (agent | tracker | auto) so the
// PR-B poll driver and PR-C tracker-status ingestion add producers without
// changing the executor signature (design.md:459-469, T3 Interfaces). Only
// SourceAgent is exercised in THIS PR.
type SourceKind int

const (
	SourceAgent   SourceKind = iota // an agent-initiated Board(SetIssueState) call
	SourceTracker                   // a tracker-status ingestion transition (PR-C)
	SourceAuto                      // an automatic transition, e.g. auto-archive (PR-C)
)

// TransitionSource identifies who or what moved issue state for later
// attribution. Actor may be empty for tracker and automatic transitions.
type TransitionSource struct {
	Kind  SourceKind
	Actor store.AccountID
}

// issueStore is the transition executor's read/commit surface. *store.Store
// satisfies it; the narrow interface makes ordering testable with a fake.
type issueStore interface {
	GetIssue(ctx context.Context, id string) (store.Issue, error)
	SetIssueState(ctx context.Context, id string, state store.IssueState) error
}

// trackerMirror mirrors committed state to the issue's tracker. It is an
// optional nil-safe seam: a nil mirror is a no-op until a writer is wired.
type trackerMirror interface {
	MirrorIssueState(ctx context.Context, committed store.Issue) error
}

// boardService implements BoardCaller and owns the shared transition executor.
// It commits through the store, publishes to the issue projection, and
// optionally mirrors committed state to the tracker.
type boardService struct {
	store    issueStore
	issueBrd *board.IssueProjection
	mirror   trackerMirror

	// transitionMu serializes read→validate→commit, so concurrent transitions
	// cannot invalidate the same-state check between validation and commit.
	// One mutex is stronger than per-issue locking and avoids keyed-lock lifecycle
	// complexity; expected write volume is low.
	transitionMu sync.Mutex
}

// newBoardService constructs the board caller. Serve assembly wires it after
// hub construction to break their cycle; the optional tracker mirror is nil
// until an outbound writer is configured.
func newBoardService(st *store.Store, issueBrd *board.IssueProjection) *boardService {
	return &boardService{store: st, issueBrd: issueBrd}
}

// Compile-time proof boardService satisfies the seam the hub delegates into.
var _ runnerhub.BoardCaller = (*boardService)(nil)

// SetIssueStateAsAccount is the BoardCaller entry point: it maps the agent's
// request onto the shared transition executor as an agent-sourced transition
// (the caller is the actor), then returns the post-transition truth on the wire.
// The store<->wire Issue mapping is borrowed from the board package (its owned
// edge) rather than re-implemented here.
func (b *boardService) SetIssueStateAsAccount(
	ctx context.Context,
	caller store.AccountID,
	req *compassv1internal.SetIssueStateRequest,
) (*compassv1internal.SetIssueStateResponse, error) {
	committed, err := b.SetIssueState(
		ctx,
		caller,
		req.GetIssueId(),
		store.IssueState(req.GetState()),
		TransitionSource{Kind: SourceAgent, Actor: caller},
	)
	if err != nil {
		return nil, err
	}
	wire, err := b.issueBrd.CommittedIssue(ctx, committed)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &compassv1internal.SetIssueStateResponse{Issue: wire}, nil
}

// errUnspecifiedTarget is the in-band cause for an ISSUE_STATE_UNSPECIFIED
// target: the proto zero is not a real lifecycle, so it is refused rather than
// persisted. CodeInvalidArgument (design.md:506-507).
var errUnspecifiedTarget = errors.New("issue state target is UNSPECIFIED")

// SetIssueState is the ONE transition executor every state producer shares: the
// compare-and-transition (design.md:513-521). Under the serialized
// transition lock it reads current truth, rejects an UNSPECIFIED target, no-ops
// (returning current, NO publish) when already at the target — ARCHIVED included,
// the any-to-any idempotent re-archive — else commits the new canonical state to
// Postgres, reads the committed row back, records+publishes it on the projection,
// and (nil-safe) mirrors it outbound. source carries WHO moved the card for
// attribution; in this PR only SourceAgent is supplied (no scope rejection ships,
// Resolved decision 2). Returns the post-transition (or unchanged) issue.
func (b *boardService) SetIssueState(
	ctx context.Context,
	caller store.AccountID,
	issueID string,
	target store.IssueState,
	source TransitionSource,
) (store.Issue, error) {
	_ = caller // recorded via source.Actor; MVP ships no scope rejection (Resolved decision 2)
	_ = source // attribution input for the later hierarchical scope check; no branch in this slice

	// The whole read→validate→commit is serialized so a concurrent transition on
	// the same issue cannot make the same-state decision stale (design.md:515-521).
	b.transitionMu.Lock()
	defer b.transitionMu.Unlock()

	// Argument validation, inside the serialized region (never a pre-lock step):
	// an UNSPECIFIED target is malformed regardless of current state.
	if target == store.IssueStateUnspecified {
		return store.Issue{}, connect.NewError(connect.CodeInvalidArgument, errUnspecifiedTarget.Error()).WithCause(errUnspecifiedTarget)
	}

	// Read current truth under the lock.
	current, err := b.store.GetIssue(ctx, issueID)
	if err != nil {
		return store.Issue{}, transitionStoreError(err)
	}

	// Already at the target (ARCHIVED included): a no-op returning current truth,
	// with NO commit, NO publish, and NO outbound mirror — nothing changed.
	if current.State == target {
		return current, nil
	}

	// Commit the new canonical state, then read the committed row back so the
	// recorded/fanned/returned Issue reflects committed truth (not the in-memory
	// pre-image).
	if err := b.store.SetIssueState(ctx, issueID, target); err != nil {
		return store.Issue{}, transitionStoreError(err)
	}
	committed, err := b.store.GetIssue(ctx, issueID)
	if err != nil {
		return store.Issue{}, transitionStoreError(err)
	}

	// Record + fan out the committed transition. It always publishes; an error
	// only means its re-read failed, and the transition is already durable.
	if err := b.issueBrd.RecordAndPublish(ctx, committed); err != nil {
		slog.WarnContext(ctx, "board: issue transition published from the caller's row", "issue", issueID, "error", err)
	}

	// Outbound tracker mirror on a real transition. Nil-safe. ARCHIVED has no
	// tracker status, so it is elided. The mirror runs AFTER the state is durable +
	// published and under transitionMu; a network mirror must not surface its
	// failure as the transition's code and must move off the lock to avoid serializing.
	if b.mirror != nil && target != store.IssueStateArchived {
		if err := b.mirror.MirrorIssueState(ctx, committed); err != nil {
			wrappedErr := fmt.Errorf("mirroring issue state: %w", err)
			return store.Issue{}, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
		}
	}

	return committed, nil
}

// transitionStoreError maps a store error from the transition path onto its
// Connect code so the hub renders it in-band (boardCallError): an unknown issue
// is CodeNotFound, a malformed input (e.g. empty id) is CodeInvalidArgument,
// anything else is CodeInternal. Mirrors lifecycleService's store-error mapping.
func transitionStoreError(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err.Error()).WithCause(err)
	case errors.Is(err, store.ErrInvalidArgument):
		return connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	default:
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
}
