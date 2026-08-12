//go:build pgtest && unix

package server

// The first-launch root-supervisor seed (serve_seed.go), against a real Postgres
// and a real Runner door. The seed hangs off the hub's runner-ready hook, fired
// when a Runner's Sessions command stream attaches — not at enroll, because the
// stream (and so the ability to serve Provision/Start) comes up only after Enroll
// returns. These tests drive a real fake-Runner enrollment + Sessions stream
// (attachFakeRunner) to fire the hook through the PRODUCTION path, then assert
// the observable outcome on the store: an agent row and a durable session
// ownership row, not a mock expectation.
//
// The seed runs on the ready-hook goroutine, so each harness wraps the hook to
// close a done channel when the seed returns: every assertion gates on that
// completion, deterministically, never on a sleep or a poll-until-timeout. The
// "went live" proof reads the agent_sessions ownership row StartAgentSession
// writes (race-free once the seed has returned), not the fake Runner's in-memory
// command tally, which the harness's attach-probe reset races.

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sealedsecurity/compass/go/events"
	"github.com/sealedsecurity/compass/go/internal/board"
	"github.com/sealedsecurity/compass/go/internal/pgtest"
	"github.com/sealedsecurity/compass/go/internal/runnerhub"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// seedHarness is the seed seam reduced to what serve_seed touches: a store, a
// hub with the root-supervisor seed wired as its runner-ready hook, a service
// behind it, and a bootstrap admin. seedDone closes when the seed goroutine
// returns (fired once, when the Runner's command stream attaches), so a test
// gates on the seed having run rather than racing it. dsn is captured ONCE (each
// pgtest.RequireDSN call mints a fresh schema) so raw store reads land in the
// same schema store.Open migrated into.
type seedHarness struct {
	store    *store.Store
	hub      *runnerhub.Hub
	dsn      string
	adminID  store.AccountID
	seedDone chan struct{}
	// seedOnce guards the seedDone close: fireRunnerReady fires on EVERY Sessions
	// attach, and close() on an already-closed channel panics, so the harness
	// tolerates a second hook fire (the re-fire idempotency the feature is built
	// around) as a safe no-op rather than crashing the test.
	seedOnce sync.Once
}

// newSeedHarness wires the seam but does NOT attach a Runner — the caller does
// that (attachFakeRunner) after optionally pre-seeding the tree, so the
// enrollment that fires the hook sees the tree the test intends.
func newSeedHarness(t *testing.T) *seedHarness {
	t.Helper()
	ctx := context.Background()
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)

	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	if err := st.PutTokenHash(ctx, sha256.Sum256([]byte(fakeRunnerToken)),
		store.Subject{Kind: store.SubjectRunner, ID: fakeRunnerID}); err != nil {
		t.Fatalf("PutTokenHash(runner): %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	brd := board.NewProjection(bus)
	tail := newSessionTail()
	hub := newRunnerHub(st, brd, tail, nil, slog.New(slog.DiscardHandler))
	svc := newService("test", bus, st, hub, brd, nil, tail)

	h := &seedHarness{store: st, hub: hub, dsn: dsn, adminID: admin.ID, seedDone: make(chan struct{})}
	hub.SetRunnerReadyHook(func() {
		seedRootSupervisor(context.Background(), st, svc, admin.ID, slog.New(slog.DiscardHandler))
		h.seedOnce.Do(func() { close(h.seedDone) })
	})
	return h
}

// awaitSeed blocks until the seed goroutine has returned, or fails on a deadline
// (a wedged seed, never a silent pass).
func (h *seedHarness) awaitSeed(t *testing.T) {
	t.Helper()
	select {
	case <-h.seedDone:
	case <-time.After(30 * time.Second):
		t.Fatal("seed never completed after the Runner command stream attached")
	}
}

// TestServeSeedsRootSupervisorOnEmptyTree pins the acceptance path: on an empty
// agent tree, the first Runner's command stream attaching seeds exactly one root
// agent named "supervisor" with role "manager" under the bootstrap admin, and
// drives it live (a recorded session it owns).
//
// Mutation: dropping the ready hook (or the CreateAgent) leaves no supervisor —
// the AgentByHandle lookup after awaitSeed fails. Dropping the SpawnAgent leaves
// the row but no live session — the ownership-row assertion reddens.
func TestServeSeedsRootSupervisorOnEmptyTree(t *testing.T) {
	ctx := context.Background()
	h := newSeedHarness(t)

	attachFakeRunner(t, h.store, h.hub, false) // enroll + Sessions attach -> ready hook -> seed on an empty tree
	h.awaitSeed(t)

	supervisor, err := h.store.AgentByHandle(ctx, rootSupervisorHandle)
	if err != nil {
		t.Fatalf("AgentByHandle(supervisor) after seed = %v, want the seeded root", err)
	}
	if supervisor.Agent == nil {
		t.Fatalf("seeded supervisor is not an agent: %+v", supervisor)
	}
	if supervisor.Agent.Role != rootSupervisorRole {
		t.Fatalf("supervisor role = %q, want %q (the Manager block-0 selector)", supervisor.Agent.Role, rootSupervisorRole)
	}
	if supervisor.Agent.ParentAgentID != "" {
		t.Fatalf("supervisor parent = %q, want empty (a root)", supervisor.Agent.ParentAgentID)
	}
	if n, err := h.store.CountRootAgents(ctx, h.adminID); err != nil || n != 1 {
		t.Fatalf("CountRootAgents after seed = (%d, %v), want (1, nil)", n, err)
	}
	// The seed drove Provision->Start to completion: the fake Runner answered
	// Start with fakeSessionID and StartAgentSession recorded a durable ownership
	// row for it, owned by the supervisor. Reading that row (not the Runner's
	// in-memory tally) is the race-free proof the supervisor is live.
	if owner := sessionOwner(t, ctx, h.dsn, fakeSessionID); owner != string(supervisor.ID) {
		t.Fatalf("seeded session %q owned by %q, want the supervisor %q", fakeSessionID, owner, supervisor.ID)
	}
}

// TestServeSeedsNothingOnNonEmptyTree pins idempotency: when a root already
// exists when the Runner's stream attaches, the seed's create half is gated off —
// no "supervisor" is created, no session is spawned, and the operator's tree is
// untouched. Deterministic via awaitSeed (the absence assertions run only after
// the seed goroutine has actually returned, so they prove the gate, not a race
// won by checking too early).
//
// Mutation: dropping the CountRootAgents empty-tree gate creates a "supervisor"
// alongside the existing root and spawns it — both the AgentByHandle absence and
// the no-session assertion redden.
func TestServeSeedsNothingOnNonEmptyTree(t *testing.T) {
	ctx := context.Background()
	h := newSeedHarness(t)

	// Pre-seed a DIFFERENT root BEFORE the Runner attaches, so the tree is not
	// empty when the hook fires.
	if _, err := h.store.CreateAgent(ctx, h.adminID, store.NewAgent{Handle: "operator-root", DisplayName: "Operator Root"}); err != nil {
		t.Fatalf("pre-seed root: %v", err)
	}

	attachFakeRunner(t, h.store, h.hub, false) // enroll + Sessions attach -> ready hook -> seed on a NON-empty tree
	h.awaitSeed(t)

	if _, err := h.store.AgentByHandle(ctx, rootSupervisorHandle); err == nil {
		t.Fatal("a supervisor was seeded on a non-empty tree, want none")
	} else if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("AgentByHandle(supervisor) = %v, want ErrNotFound (never seeded)", err)
	}
	if n, err := h.store.CountRootAgents(ctx, h.adminID); err != nil || n != 1 {
		t.Fatalf("CountRootAgents = (%d, %v), want (1, nil) — the operator root, no supervisor added", n, err)
	}
	// The gated seed never spawned, so no session ownership row exists.
	if n := sessionRowCount(t, ctx, h.dsn, fakeSessionID); n != 0 {
		t.Fatalf("session ownership rows for %q = %d, want 0 (the seed must not spawn on a non-empty tree)", fakeSessionID, n)
	}
}

// TestServeReDrivesNeverStartedSupervisor pins the ticket-scoped find-then-start
// path (serve_seed.go `case err == nil`): a "supervisor" root ROW that exists
// but was never started (a prior boot created the row but its Start failed)
// is re-driven live when a later Runner's command stream attaches. The row is
// pre-created BEFORE the Runner attaches, so AgentByHandle resolves it (the find
// half), the empty-tree create gate is never reached, and the seed drives
// Provision->Start against the EXISTING agent. Deterministic via awaitSeed.
//
// It also proves the find half does not RE-create: the supervisor read back after
// the seed is the same id + display name the test pre-created, and the ownership
// row is owned by it.
//
// Mutation: a regression that skipped the start on the find path (`case err ==
// nil: return`) leaves the pre-created row but no session — the ownership-row
// assertion reddens. A regression that mishandled the find path by re-creating
// would change the display name (or ErrConflict-fail the seed).
func TestServeReDrivesNeverStartedSupervisor(t *testing.T) {
	ctx := context.Background()
	h := newSeedHarness(t)

	// A prior boot created the supervisor root row under the bootstrap admin but
	// never started it. Distinct display name so the read-back proves this exact
	// row was re-driven, not re-created.
	const priorBootDisplayName = "Prior-Boot Supervisor"
	precreated, err := h.store.CreateAgent(ctx, h.adminID, store.NewAgent{
		Handle:      rootSupervisorHandle,
		DisplayName: priorBootDisplayName,
		Role:        rootSupervisorRole,
		// ParentAgentID empty => root.
	})
	if err != nil {
		t.Fatalf("pre-create never-started supervisor: %v", err)
	}

	attachFakeRunner(t, h.store, h.hub, false) // enroll + Sessions attach -> ready hook -> seed finds the existing row
	h.awaitSeed(t)

	// The find half re-drove the EXISTING row: same id + display name, never
	// re-created, and still the admin's single root.
	supervisor, err := h.store.AgentByHandle(ctx, rootSupervisorHandle)
	if err != nil {
		t.Fatalf("AgentByHandle(supervisor) after re-drive = %v, want the pre-created root", err)
	}
	if supervisor.ID != precreated.ID {
		t.Fatalf("supervisor id after re-drive = %q, want the pre-created %q (find must not re-create)", supervisor.ID, precreated.ID)
	}
	if supervisor.DisplayName != priorBootDisplayName {
		t.Fatalf("supervisor display name = %q, want %q (find must not re-create)", supervisor.DisplayName, priorBootDisplayName)
	}
	if n, err := h.store.CountRootAgents(ctx, h.adminID); err != nil || n != 1 {
		t.Fatalf("CountRootAgents after re-drive = (%d, %v), want (1, nil) — the same root, none added", n, err)
	}
	// The seed drove Provision->Start on the found row: the durable ownership row
	// for fakeSessionID is owned by the pre-created supervisor.
	if owner := sessionOwner(t, ctx, h.dsn, fakeSessionID); owner != string(precreated.ID) {
		t.Fatalf("re-driven session %q owned by %q, want the pre-created supervisor %q", fakeSessionID, owner, precreated.ID)
	}
}
