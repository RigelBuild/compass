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
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/board"
	"github.com/sealedsecurity/compass/go/internal/comms"
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
	store     *store.Store
	hub       *runnerhub.Hub
	svc       *service
	commsSvc  *comms.Comms
	commsBus  *events.Bus[*compassv1.SubscribeCommsResponse]
	dsn       string
	adminID   store.AccountID
	compassID store.AccountID
	seedDone  chan struct{}
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
	// The reserved @compass system sender authors the Setup post; seed it as the
	// production boot does (seedBootstrapAccounts).
	system, err := st.EnsureSystemAccount(ctx)
	if err != nil {
		t.Fatalf("EnsureSystemAccount: %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	brd := board.NewProjection(bus)
	tail := newSessionTail()
	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(commsBus.Close)
	commsSvc := comms.NewComms(st, commsBus, admin.ID)
	hub := newRunnerHub(st, brd, tail, commsSvc, slog.New(slog.DiscardHandler))
	svc := newService("test", bus, st, hub, brd, nil, tail)

	h := &seedHarness{
		store:     st,
		hub:       hub,
		svc:       svc,
		commsSvc:  commsSvc,
		commsBus:  commsBus,
		dsn:       dsn,
		adminID:   admin.ID,
		compassID: system.ID,
		seedDone:  make(chan struct{}),
	}
	hub.SetRunnerReadyHook(func() {
		seedRootSupervisor(context.Background(), st, svc, commsSvc, admin.ID, system.ID, slog.New(slog.DiscardHandler))
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

// setupMessageCount counts the Setup-thread messages @compass has posted into
// channelID: rows in topic "Setup" authored by compassID. Read directly off the
// store of record (the topic namespace + author are what the assertions turn
// on), in this test's isolated schema.
func setupMessageCount(t *testing.T, ctx context.Context, dsn string, channelID store.ChannelID, compassID store.AccountID) int {
	t.Helper()
	conn := connectPG(t, ctx, dsn)
	var n int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM messages m JOIN topics t ON t.id = m.topic_id
		  WHERE t.channel_id = $1 AND t.name = $2 AND m.author_account_id = $3`,
		string(channelID), setupTopicName, string(compassID),
	).Scan(&n); err != nil {
		t.Fatalf("count Setup messages: %v", err)
	}
	return n
}

// TestSeedPostsSetupThreadAsCompass pins the T4 acceptance path: the first-launch
// seed posts exactly ONE Setup message into the supervisor's home channel,
// authored by the reserved @compass system account, in topic "Setup". @compass
// is made a channel member first (PostMessage D9-gates on membership), so the
// post lands rather than collapsing to CodeNotFound.
//
// Mutation: dropping the EnsureChannelMember call reddens (the post collapses to
// CodeNotFound, so no Setup message exists). Dropping the postSetupThread call
// leaves zero Setup messages.
func TestSeedPostsSetupThreadAsCompass(t *testing.T) {
	ctx := context.Background()
	h := newSeedHarness(t)

	attachFakeRunner(t, h.store, h.hub, false)
	h.awaitSeed(t)

	supervisor, err := h.store.AgentByHandle(ctx, rootSupervisorHandle)
	if err != nil {
		t.Fatalf("AgentByHandle(supervisor) after seed = %v, want the seeded root", err)
	}
	home := supervisor.Agent.HomeChannelID

	// @compass is a member of the home channel (the pre-post membership insert).
	if member, err := h.store.IsChannelMember(ctx, h.compassID, home); err != nil || !member {
		t.Fatalf("IsChannelMember(@compass, home) = (%v, %v), want (true, nil) — the post's D9 gate", member, err)
	}
	// Exactly one Setup message, authored by @compass, in topic "Setup".
	if n := setupMessageCount(t, ctx, h.dsn, home, h.compassID); n != 1 {
		t.Fatalf("Setup messages by @compass in the home channel = %d, want exactly 1", n)
	}
}

// TestSeedSetupThreadIdempotentOnReFire pins the OQ-7 supervisor-scoped
// idempotency: a re-fire of the seed on the already-live arm posts NOTHING new —
// the (author, client_request_id) unique index dedups the supervisor-scoped key
// through AppendMessage's ON CONFLICT DO NOTHING, so the home channel still holds
// exactly one Setup message.
//
// Mutation: a global fixed key would still dedup here (same supervisor), so this
// pins the re-fire no-op, not the scope; the scope's payoff (a recreated
// supervisor) is covered by the record's OQ-7 rationale.
func TestSeedSetupThreadIdempotentOnReFire(t *testing.T) {
	ctx := context.Background()
	h := newSeedHarness(t)

	attachFakeRunner(t, h.store, h.hub, false)
	h.awaitSeed(t)

	supervisor, err := h.store.AgentByHandle(ctx, rootSupervisorHandle)
	if err != nil {
		t.Fatalf("AgentByHandle(supervisor) after seed = %v, want the seeded root", err)
	}
	home := supervisor.Agent.HomeChannelID
	if n := setupMessageCount(t, ctx, h.dsn, home, h.compassID); n != 1 {
		t.Fatalf("Setup messages after first seed = %d, want 1", n)
	}

	// Re-fire the seed directly (the supervisor is now live, so SpawnAgent joins
	// the completed spawn or rejects on-live — both arms reach postSetupThread).
	seedRootSupervisor(ctx, h.store, h.svc, h.commsSvc, h.adminID, h.compassID, slog.New(slog.DiscardHandler))

	if n := setupMessageCount(t, ctx, h.dsn, home, h.compassID); n != 1 {
		t.Fatalf("Setup messages after a seed re-fire = %d, want still 1 (the scoped key dedups)", n)
	}
}

// TestSeedSetupThreadPublishesOneMessagePosted pins the fan-out: the genuine
// first insert publishes exactly ONE MessagePosted on the comms bus. Event-gated
// (design.md:599-600): subscribe BEFORE the seed, then assert the received event
// off the live tail — no poll, no sleep. The seed's PostAsAccount publishes
// synchronously before the seed goroutine returns (awaitSeed), so the event is
// buffered on Live by the time this reads it.
//
// Mutation: dropping publishMessagePosted's inserted-gate, or re-firing a
// duplicate publish, would surface a second MessagePosted the drain would catch.
func TestSeedSetupThreadPublishesOneMessagePosted(t *testing.T) {
	ctx := context.Background()
	h := newSeedHarness(t)

	// Subscribe before the seed fires so the post's MessagePosted lands on Live.
	sub, err := h.commsBus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("commsBus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	attachFakeRunner(t, h.store, h.hub, false)
	h.awaitSeed(t)

	supervisor, err := h.store.AgentByHandle(ctx, rootSupervisorHandle)
	if err != nil {
		t.Fatalf("AgentByHandle(supervisor) after seed = %v, want the seeded root", err)
	}

	// Exactly one MessagePosted for the Setup post. The first event off Live must
	// be a MessagePosted authored by @compass; then no further MessagePosted
	// arrives (a short drain window bounds the "exactly one").
	first := awaitCommsEvent(t, sub.Live)
	mp := first.GetMessagePosted()
	if mp == nil {
		t.Fatalf("first comms event payload = %T, want a MessagePosted", first.GetPayload())
	}
	if got := mp.GetMessage().GetAuthorAccountId(); got != string(h.compassID) {
		t.Fatalf("MessagePosted author = %q, want @compass %q", got, h.compassID)
	}
	assertNoMoreMessagePosted(t, sub.Live)

	_ = supervisor
}

// awaitCommsEvent reads one event off the comms live tail, failing on a deadline.
func awaitCommsEvent(t *testing.T, live <-chan events.Stamped[*compassv1.SubscribeCommsResponse]) *compassv1.SubscribeCommsResponse {
	t.Helper()
	select {
	case ev, ok := <-live:
		if !ok {
			t.Fatal("comms live tail closed before any event")
		}
		return ev.Payload
	case <-time.After(30 * time.Second):
		t.Fatal("no comms event on the live tail")
		return nil
	}
}

// assertNoMoreMessagePosted drains a short window and fails on a second
// MessagePosted — bounding the Setup post to exactly one fan-out.
func assertNoMoreMessagePosted(t *testing.T, live <-chan events.Stamped[*compassv1.SubscribeCommsResponse]) {
	t.Helper()
	for {
		select {
		case ev, ok := <-live:
			if !ok {
				return
			}
			if ev.Payload.GetMessagePosted() != nil {
				t.Fatalf("a second MessagePosted arrived, want exactly one for the Setup post")
			}
		case <-time.After(500 * time.Millisecond):
			return
		}
	}
}
