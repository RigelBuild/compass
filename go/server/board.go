//go:build unix

// The agent-initiated board-write leg: boardService implements
// runnerhub.BoardCaller (relay_board.go), the seam the RunnerHub delegates a
// resolved-caller issue-state write into (agent primary lifecycle T3-a). It runs
// the ONE transition executor every state producer shares — the frozen
// compare-and-transition (compass-issue-model/design.md:513-521) — so a
// tracker- or auto-sourced transition (PR-B/PR-C) reuses SetIssueState unchanged,
// only supplying a different TransitionSource. The hub depends only on the narrow
// BoardCaller surface and never pulls the whole board service in.
//
// Trust model (mirrors lifecycleService). The caller AccountID is resolved
// Server-side by the hub from its own session binding and passed in; the Runner
// never asserts it. Per Resolved decision 2 (MVP, single-trust-domain), the
// caller is recorded for attribution but NO scope rejection ships — the
// signature carries it so the later hierarchical scope check has its input
// without a migration.
//
// A tool-level failure (unknown issue, UNSPECIFIED target) is returned as a
// Connect-coded error the hub renders IN-BAND (boardCallError); only a
// resolution miss / no-caller is a transport error, and that is the hub's job.
package server

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"connectrpc.com/connect"

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

// TransitionSource carries WHO/WHAT moved an issue's state, threaded through the
// executor to record+publish so a later reader can attribute the transition.
// Actor is the caller/actor account when the source has one (SourceAgent sets
// it; a tracker/auto producer may leave it empty). Attribution now; the input
// for the later hierarchical scope check (Resolved decision 2).
type TransitionSource struct {
	Kind  SourceKind
	Actor store.AccountID
}

// issueStore is the narrow store surface the transition executor needs: read the
// current row and commit the new lifecycle state. Satisfied by *store.Store; a
// narrow interface (the CommsCaller/LifecycleCaller pattern) so the
// compare-and-transition is provable in the default lane against a fake, not only
// behind the pgtest tag.
type issueStore interface {
	GetIssue(ctx context.Context, id string) (store.Issue, error)
	SetIssueState(ctx context.Context, id string, state store.IssueState) error
}

// trackerMirror is the outbound tracker-write seam (design.md:438): on a real
// transition the executor mirrors the new state out to the issue's tracker. It
// is a NIL-SAFE injected seam in THIS PR — no real forge-tracker write seam
// exists yet, so the field stays nil and a nil mirror is a clean no-op (the
// pervasive post-construction-setter idiom: hub.SetLifecycleCaller /
// SetTranscriptStore). PR-C wires a real mirror and owns its echo-suppression +
// tracker-status mapping (Resolved decision 1).
type trackerMirror interface {
	MirrorIssueState(ctx context.Context, committed store.Issue) error
}

// boardService is the BoardCaller implementation and the home of the shared
// transition executor. It holds the store (of record for issue state), the issue
// projection (the durable-cache + issue=16 fan-out the transition records+
// publishes onto), and the nil-safe outbound tracker mirror.
type boardService struct {
	store    issueStore
	issueBrd *board.IssueProjection
	mirror   trackerMirror

	// transitionMu serializes the compare-and-transition so read→validate→commit
	// is atomic — a concurrent transition on the SAME issue cannot make the
	// same-state decision stale (design.md:515-521: validation is part of the
	// serialized transition, NOT a pre-lock step). The frozen record's cleanest
	// form is a per-ISSUE mutex; a single executor mutex is chosen for the MVP
	// (justified): it serializes ALL transitions, which is strictly stronger than
	// per-issue atomicity (never wrong), and the agent-initiated write volume is
	// low, so the throughput a per-issue keyed mutex would buy does not matter —
	// while a refcounted keyed-mutex map is a real lifecycle-bug surface to get
	// right. The upgrade to a per-issue lock (or a Postgres row-level
	// SELECT ... FOR UPDATE in one tx) is a drop-in if write contention ever
	// warrants it.
	transitionMu sync.Mutex
}

// newBoardService constructs the board caller over the store and the issue
// projection. Wired at serve assembly with hub.SetBoardCaller after both exist,
// breaking the hub<->boardService construction cycle (sinks.go). The mirror is
// left nil in THIS PR (no real forge-tracker write seam yet).
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
	return &compassv1internal.SetIssueStateResponse{Issue: board.IssueToProto(committed)}, nil
}

// errUnspecifiedTarget is the in-band cause for an ISSUE_STATE_UNSPECIFIED
// target: the proto zero is not a real lifecycle, so it is refused rather than
// persisted. CodeInvalidArgument (design.md:506-507).
var errUnspecifiedTarget = errors.New("issue state target is UNSPECIFIED")

// SetIssueState is the ONE transition executor every state producer shares: the
// frozen compare-and-transition (design.md:513-521). Under the serialized
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
		return store.Issue{}, connect.NewError(connect.CodeInvalidArgument, errUnspecifiedTarget)
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

	// Record + fan out the committed transition on the projection (the issue=16
	// live stream + the durable-cache snapshot). State-only: the executor already
	// owns the durable commit, so this never touches the store (a forge-only
	// upsert would demand forge fields and could not carry the state column).
	b.issueBrd.RecordAndPublish(committed)

	// Outbound tracker mirror on a real transition. Nil-safe (unset this PR).
	// ARCHIVED has no tracker status, so it is elided from the outbound mirror
	// (design.md:438-439 / compass-issue-model:558-563). The mirror runs AFTER
	// the state is durable + published; PR-C's real mirror owns echo-suppression,
	// tracker-status mapping, and any retry/failure refinement (Resolved decision 1).
	// PR-C deferrals (do NOT change behavior in this PR):
	//   (a) error-after-commit ordering: the mirror runs AFTER the state is
	//       committed AND published, so PR-C's real mirror must not surface a
	//       mirror failure as the transition's failure code (outbox/async or
	//       log-and-continue) — the transition already succeeded here.
	//   (b) lock scope: the mirror runs under transitionMu; PR-C's real
	//       (network) mirror must move this call after releasing the lock so a
	//       round-trip does not serialize the whole board.
	if b.mirror != nil && target != store.IssueStateArchived {
		if err := b.mirror.MirrorIssueState(ctx, committed); err != nil {
			return store.Issue{}, connect.NewError(connect.CodeInternal, fmt.Errorf("mirroring issue state: %w", err))
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
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, store.ErrInvalidArgument):
		return connect.NewError(connect.CodeInvalidArgument, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
