//go:build pgtest && unix

package comms

// ReparentAgent + CreateAgent-with-parent handler contracts (Record C, T3),
// after the RIG-2751 handle cutover: the oracle-safe error contract (DL-269)
// collapses every post-resolution failure on a handle target into the SAME
// NOT_FOUND an unknown handle gets. Cycle is FAILED_PRECONDITION. Via WithActor.

import (
	"context"
	"errors"
	"strings"
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

	// Bare handles resolve in the caller-owner's agent namespace.
	resp, err := h.svc.ReparentAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.ReparentAgentRequest{
		AgentHandle:     "b",
		NewParentHandle: "a",
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

// TestReparentAgentForeignCallerNotFound: a foreign caller's authority failure must
// read as an unknown target. Its parent is itself, so the call gets past the
// parent pre-check to the target check.
func TestReparentAgentForeignCallerNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	other := mustUser(t, st, "other")
	mustAgent(t, st, owner.ID, "b")
	intruder := mustAgent(t, st, other.ID, "intruder")

	// The intruder owner-qualifies the target into owner's namespace; the
	// resolver resolves it (AgentByHandle is not viewer-scoped), but the store's
	// clause-0 authority check then fails and is remapped to NOT_FOUND.
	_, foreignErr := svc.ReparentAgent(WithActor(ctx, intruder.ID), connect.NewRequest(&compassv1.ReparentAgentRequest{
		AgentHandle:     "owner/b",
		NewParentHandle: "intruder",
	}))
	connectNotFoundFor(t, foreignErr, "owner/b", "foreign caller")

	// An unknown target from the same vantage with the same parent: a resolver
	// miss carrying the same template for its own spelling.
	_, unknownErr := svc.ReparentAgent(WithActor(ctx, intruder.ID), connect.NewRequest(&compassv1.ReparentAgentRequest{
		AgentHandle:     "owner/ghost",
		NewParentHandle: "intruder",
	}))
	connectNotFoundFor(t, unknownErr, "owner/ghost", "unknown target")
}

// TestReparentAgentCrossOwnerParentNotFound: a parent under a different owner is
// remapped to NOT_FOUND (was PermissionDenied) AND is byte-identical to an
// UNKNOWN parent — the oracle-safe merge on the new_parent_handle target
// (DL-269). Asserting the message, not just the code, is load-bearing: a
// code-only check passes even if the foreign case names the agent handle while
// the unknown case names the parent handle, which is the exact existence-probe
// (enumerate another owner's agents by owner-qualified handle) the invariant
// forbids.
func TestReparentAgentCrossOwnerParentNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	other := mustUser(t, st, "other")
	mustAgent(t, st, owner.ID, "a")
	mustAgent(t, st, other.ID, "foreign")

	// Existing-but-foreign parent: resolves (AgentByHandle is not viewer-scoped),
	// then the edge same-owner pre-check rejects it as NOT_FOUND naming the
	// submitted new_parent_handle.
	_, foreignErr := svc.ReparentAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.ReparentAgentRequest{
		AgentHandle:     "a",
		NewParentHandle: "other/foreign",
	}))
	connectCodeIs(t, foreignErr, connect.CodeNotFound, "cross-owner parent")

	// Unknown parent under the same owner-qualifier: misses at resolution,
	// NOT_FOUND naming the same submitted spelling.
	_, unknownErr := svc.ReparentAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.ReparentAgentRequest{
		AgentHandle:     "a",
		NewParentHandle: "other/ghost",
	}))
	connectCodeIs(t, unknownErr, connect.CodeNotFound, "unknown parent")

	// The oracle invariant: a foreign parent must be indistinguishable from an
	// unknown one — both NOT_FOUND naming the SUBMITTED new_parent_handle. Before
	// the same-owner pre-check the foreign case named the AGENT handle, the exact
	// divergence a caller uses to enumerate another owner's agents.
	if got := connect.CodeOf(foreignErr); got != connect.CodeOf(unknownErr) {
		t.Fatalf("foreign vs unknown parent code differs: foreign=%v unknown=%v", got, connect.CodeOf(unknownErr))
	}
	if !strings.Contains(foreignErr.Error(), "other/foreign") {
		t.Fatalf("foreign-parent error must name the submitted parent handle, got %q", foreignErr.Error())
	}
	if strings.Contains(foreignErr.Error(), `"a"`) {
		t.Fatalf("oracle leak: foreign-parent error names the AGENT handle, distinguishing it from an unknown parent: %q", foreignErr.Error())
	}
	if !strings.Contains(unknownErr.Error(), "other/ghost") {
		t.Fatalf("unknown-parent error must name the submitted parent handle, got %q", unknownErr.Error())
	}
	// Byte-identical modulo the caller's own submitted spelling: swapping the
	// parent handle in the foreign error for the unknown one's yields the same
	// string, proving the only difference is the input the caller already knows.
	normalizedForeign := strings.ReplaceAll(foreignErr.Error(), "other/foreign", "PARENT")
	normalizedUnknown := strings.ReplaceAll(unknownErr.Error(), "other/ghost", "PARENT")
	if normalizedForeign != normalizedUnknown {
		t.Fatalf("oracle leak: foreign vs unknown parent errors differ beyond the submitted handle.\n foreign: %q\n unknown: %q", foreignErr.Error(), unknownErr.Error())
	}
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
	_ = b

	// Clause 2: cycle → FailedPrecondition. Both handles resolve (same owner), so
	// the cycle check runs and its distinct code survives (it is not an
	// authority/visibility failure, so DL-269's merge does not apply).
	_, err = svc.ReparentAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.ReparentAgentRequest{
		AgentHandle:     "a",
		NewParentHandle: "b",
	}))
	connectCodeIs(t, err, connect.CodeFailedPrecondition, "cycle")
}

func TestReparentAgentMissingParentNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	mustAgent(t, st, owner.ID, "a")

	// A non-existent parent handle misses at resolution → NotFound.
	_, err := svc.ReparentAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.ReparentAgentRequest{
		AgentHandle:     "a",
		NewParentHandle: "no-such-agent",
	}))
	connectCodeIs(t, err, connect.CodeNotFound, "missing parent")
}

func TestCreateAgentWithParentValidatesAndPersists(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	parent := mustAgent(t, st, owner.ID, "parent")

	// Happy path: a parent the caller's owner owns (bare handle) is accepted.
	resp, err := svc.CreateAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateAgentRequest{
		Handle:       "child",
		DisplayName:  "Child",
		ParentHandle: "parent",
		Role:         "manager",
	}))
	if err != nil {
		t.Fatalf("CreateAgent with parent: %v", err)
	}
	if got := resp.Msg.GetAccount().GetAgent().GetParentAgentId(); got != string(parent.ID) {
		t.Fatalf("created child parent = %q, want %q", got, parent.ID)
	}

	// A parent that does not exist → NotFound (resolver miss).
	_, err = svc.CreateAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateAgentRequest{
		Handle:       "orphan",
		DisplayName:  "Orphan",
		ParentHandle: "no-such-agent",
		Role:         "manager",
	}))
	connectNotFoundFor(t, err, "no-such-agent", "create with missing parent")

	// A foreign parent resolves, but the same-owner check must read as an unknown
	// parent under the same qualifier, code AND message.
	other := mustUser(t, st, "other")
	mustAgent(t, st, other.ID, "foreign")
	_, err = svc.CreateAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateAgentRequest{
		Handle:       "cross",
		DisplayName:  "Cross",
		ParentHandle: "other/foreign",
		Role:         "manager",
	}))
	connectNotFoundFor(t, err, "other/foreign", "create with cross-owner parent")
	_, err = svc.CreateAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateAgentRequest{
		Handle:       "cross",
		DisplayName:  "Cross",
		ParentHandle: "other/ghost",
		Role:         "manager",
	}))
	connectNotFoundFor(t, err, "other/ghost", "create with unknown parent under a real owner")
}

func TestCreateAgentRequiresTaxonomyRole(t *testing.T) {
	tests := []struct {
		name         string
		handle       string
		role         string
		parentHandle string
	}{
		{name: "empty", handle: "role-empty"},
		{name: "off taxonomy", handle: "role-worker", role: "worker"},
		{name: "parented empty", handle: "role-parented-empty", parentHandle: "parent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, st := newHandler(t)
			ctx := context.Background()
			owner := mustUser(t, st, "owner")
			if tt.parentHandle != "" {
				mustAgent(t, st, owner.ID, tt.parentHandle)
			}

			_, err := svc.CreateAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateAgentRequest{
				Handle:       tt.handle,
				DisplayName:  tt.handle,
				ParentHandle: tt.parentHandle,
				Role:         tt.role,
			}))
			if err == nil {
				t.Fatal("CreateAgent with invalid role = nil error, want CodeInvalidArgument")
			}
			if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
				t.Fatalf("CreateAgent code = %v, want CodeInvalidArgument (err: %v)", got, err)
			}
			if !errors.Is(err, store.ErrUnknownRole) {
				t.Fatalf("CreateAgent error = %v, want store.ErrUnknownRole", err)
			}
			if _, err := st.AgentByHandle(ctx, owner.ID, tt.handle); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("AgentByHandle(%q) = %v, want ErrNotFound (no account created)", tt.handle, err)
			}
		})
	}
}

func TestCreateAgentPersistsRole(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")

	for _, role := range []string{"supervisor", "owner", "manager"} {
		t.Run(role, func(t *testing.T) {
			handle := "role-" + role
			if _, err := svc.CreateAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateAgentRequest{
				Handle:      handle,
				DisplayName: handle,
				Role:        role,
			})); err != nil {
				t.Fatalf("CreateAgent(role=%q): %v", role, err)
			}

			created, err := st.AgentByHandle(ctx, owner.ID, handle)
			if err != nil {
				t.Fatalf("AgentByHandle(%q): %v", handle, err)
			}
			if created.Agent == nil {
				t.Fatal("stored account is not an agent")
			}
			if created.Agent.Role != role {
				t.Fatalf("stored agent role = %q, want %q", created.Agent.Role, role)
			}
		})
	}
}

// TestCreateAgentByAgentCallerResolvesOwner is the RIG-1644 red-green teeth:
// agents spawning agents is core product, so an AGENT caller creating a child
// under a same-owner parent must be authorized against its resolved USER owner,
// not its own agent id, and the bare parent handle must resolve in that owner's
// namespace. The child must be created and owned by the resolved user owner. The
// cross-owner case still fails closed (now NOT_FOUND), proving the resolution did
// not open a hole.
func TestCreateAgentByAgentCallerResolvesOwner(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	parentAgent := mustAgent(t, st, owner.ID, "parent")
	callerAgent := mustAgent(t, st, owner.ID, "caller")

	resp, err := svc.CreateAgent(WithActor(ctx, callerAgent.ID), connect.NewRequest(&compassv1.CreateAgentRequest{
		Handle:       "child",
		DisplayName:  "Child",
		ParentHandle: "parent",
		Role:         "manager",
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

	// An agent caller under a DIFFERENT owner cannot create under this parent:
	// its bare `parent` resolves in `other`'s namespace, where no such agent
	// exists → NOT_FOUND (resolver miss).
	other := mustUser(t, st, "other")
	intruder := mustAgent(t, st, other.ID, "intruder")
	_, err = svc.CreateAgent(WithActor(ctx, intruder.ID), connect.NewRequest(&compassv1.CreateAgentRequest{
		Handle:       "hijack",
		DisplayName:  "Hijack",
		ParentHandle: "parent",
		Role:         "manager",
	}))
	connectCodeIs(t, err, connect.CodeNotFound, "cross-owner agent caller")
}
