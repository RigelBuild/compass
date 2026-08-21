//go:build pgtest && unix

package comms

// ReparentAgent + CreateAgent-with-parent handler contracts (Record C, T3):
// the happy path emits AccountChanged and returns the mutated account, and each
// §Server validation clause maps to its exact gRPC code at the edge —
// PERMISSION_DENIED (caller authority, same-owner), FAILED_PRECONDITION (cycle),
// NOT_FOUND (missing parent). CreateAgent threads and validates the optional
// parent. Driven in-process via WithActor against a real store + bus.

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestReparentAgentHappyPathEmitsAccountChanged(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()
	owner := mustUser(t, h.store, "owner")
	a := mustAgent(t, h.store, owner.ID, "a")
	b := mustAgent(t, h.store, owner.ID, "b")

	events := firstEventAfterBoundary(t, h, owner.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})

	resp, err := h.svc.ReparentAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.ReparentAgentRequest{
		AgentAccountId:   string(b.ID),
		NewParentAgentId: string(a.ID),
	}))
	if err != nil {
		t.Fatalf("ReparentAgent: %v", err)
	}
	if got := resp.Msg.GetAccount().GetAgent().GetParentAgentId(); got != string(a.ID) {
		t.Fatalf("returned account parent = %q, want %q", got, a.ID)
	}

	got := awaitFirst(t, events)
	ac := got.GetAccountChanged()
	if ac == nil {
		t.Fatalf("event payload = %T, want AccountChanged", got.GetPayload())
	}
	if ac.GetAccount().GetId() != string(b.ID) {
		t.Fatalf("AccountChanged id = %q, want the moved agent %q", ac.GetAccount().GetId(), b.ID)
	}
	if p := ac.GetAccount().GetAgent().GetParentAgentId(); p != string(a.ID) {
		t.Fatalf("AccountChanged parent = %q, want %q", p, a.ID)
	}
}

func TestReparentAgentForeignCallerPermissionDenied(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	other := mustUser(t, st, "other")
	a := mustAgent(t, st, owner.ID, "a")
	b := mustAgent(t, st, owner.ID, "b")
	intruder := mustAgent(t, st, other.ID, "intruder")

	// Clause 0: a caller under a different owner cannot re-parent the target.
	_, err := svc.ReparentAgent(WithActor(ctx, intruder.ID), connect.NewRequest(&compassv1.ReparentAgentRequest{
		AgentAccountId:   string(b.ID),
		NewParentAgentId: string(a.ID),
	}))
	connectCodeIs(t, err, connect.CodePermissionDenied, "foreign caller")
}

func TestReparentAgentCrossOwnerParentPermissionDenied(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	other := mustUser(t, st, "other")
	a := mustAgent(t, st, owner.ID, "a")
	foreign := mustAgent(t, st, other.ID, "foreign")

	// Clause 1: a parent under a different owner → PermissionDenied.
	_, err := svc.ReparentAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.ReparentAgentRequest{
		AgentAccountId:   string(a.ID),
		NewParentAgentId: string(foreign.ID),
	}))
	connectCodeIs(t, err, connect.CodePermissionDenied, "cross-owner parent")
}

func TestReparentAgentCycleFailedPrecondition(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	a := mustAgent(t, st, owner.ID, "a")
	b, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "b", DisplayName: "b", ParentAgentID: a.ID})
	if err != nil {
		t.Fatalf("create b under a: %v", err)
	}

	// Clause 2: cycle → FailedPrecondition.
	_, err = svc.ReparentAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.ReparentAgentRequest{
		AgentAccountId:   string(a.ID),
		NewParentAgentId: string(b.ID),
	}))
	connectCodeIs(t, err, connect.CodeFailedPrecondition, "cycle")
}

func TestReparentAgentMissingParentNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	a := mustAgent(t, st, owner.ID, "a")

	// Clause 3: non-existent parent → NotFound.
	_, err := svc.ReparentAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.ReparentAgentRequest{
		AgentAccountId:   string(a.ID),
		NewParentAgentId: "no-such-agent",
	}))
	connectCodeIs(t, err, connect.CodeNotFound, "missing parent")
}

func TestCreateAgentWithParentValidatesAndPersists(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	parent := mustAgent(t, st, owner.ID, "parent")

	// Happy path: a parent the caller's owner owns is accepted and persisted.
	resp, err := svc.CreateAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateAgentRequest{
		Handle:        "child",
		DisplayName:   "Child",
		ParentAgentId: string(parent.ID),
	}))
	if err != nil {
		t.Fatalf("CreateAgent with parent: %v", err)
	}
	if got := resp.Msg.GetAccount().GetAgent().GetParentAgentId(); got != string(parent.ID) {
		t.Fatalf("created child parent = %q, want %q", got, parent.ID)
	}

	// A parent that does not exist → NotFound (clause 3 on the create path).
	_, err = svc.CreateAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateAgentRequest{
		Handle:        "orphan",
		DisplayName:   "Orphan",
		ParentAgentId: "no-such-agent",
	}))
	connectCodeIs(t, err, connect.CodeNotFound, "create with missing parent")

	// A parent under a different owner → PermissionDenied (clauses 0/1).
	other := mustUser(t, st, "other")
	foreign := mustAgent(t, st, other.ID, "foreign")
	_, err = svc.CreateAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateAgentRequest{
		Handle:        "cross",
		DisplayName:   "Cross",
		ParentAgentId: string(foreign.ID),
	}))
	connectCodeIs(t, err, connect.CodePermissionDenied, "create with cross-owner parent")
}

// TestCreateAgentByAgentCallerResolvesOwner is the RIG-1644 red-green teeth:
// agents spawning agents is core product, so an AGENT caller creating a child
// under a same-owner parent must be authorized against its resolved USER owner,
// not its own agent id. Pre-fix CreateAgent used the raw caller id both as the
// parent same-owner key (→ spurious PermissionDenied) and as the store owner (→
// owner_user_id FK rejects a non-user), so this call errored. The child must be
// created and owned by the resolved user owner. The cross-owner case still fails
// closed, proving the resolution did not open a hole.
func TestCreateAgentByAgentCallerResolvesOwner(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	parentAgent := mustAgent(t, st, owner.ID, "parent")
	callerAgent := mustAgent(t, st, owner.ID, "caller")

	resp, err := svc.CreateAgent(WithActor(ctx, callerAgent.ID), connect.NewRequest(&compassv1.CreateAgentRequest{
		Handle:        "child",
		DisplayName:   "Child",
		ParentAgentId: string(parentAgent.ID),
	}))
	if err != nil {
		t.Fatalf("CreateAgent by agent caller: %v", err)
	}
	if got := resp.Msg.GetAccount().GetAgent().GetParentAgentId(); got != string(parentAgent.ID) {
		t.Fatalf("created child parent = %q, want %q", got, parentAgent.ID)
	}
	childID := store.AccountID(resp.Msg.GetAccount().GetId())
	gotOwner, err := st.AgentOwner(ctx, childID)
	if err != nil {
		t.Fatalf("AgentOwner(child): %v", err)
	}
	if gotOwner != owner.ID {
		t.Fatalf("child owner = %q, want resolved user owner %q", gotOwner, owner.ID)
	}

	// An agent caller under a DIFFERENT owner still cannot create under this
	// parent: resolution maps it to `other`, so the same-owner check denies it.
	other := mustUser(t, st, "other")
	intruder := mustAgent(t, st, other.ID, "intruder")
	_, err = svc.CreateAgent(WithActor(ctx, intruder.ID), connect.NewRequest(&compassv1.CreateAgentRequest{
		Handle:        "hijack",
		DisplayName:   "Hijack",
		ParentAgentId: string(parentAgent.ID),
	}))
	connectCodeIs(t, err, connect.CodePermissionDenied, "cross-owner agent caller")
}
