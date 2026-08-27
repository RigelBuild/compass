//go:build pgtest && unix

package server

// The lifecycleService orchestration seam, against a real Postgres AND a real
// Runner door. lifecycleService is an INTERNAL seam (runnerhub.LifecycleCaller),
// not a wire RPC, so these drive newLifecycleService(f.hub, f.store) DIRECTLY
// under a resolved caller AccountID — the same way the hub's RelayLifecycleCall
// delegates into it — rather than through the connect client. The RemoveAgentWorkspace
// operator door IS a wire RPC, so that one test drives it through f.client.
//
// The fake Runner (service_placement_pgtest_test.go) answers Provision with a
// fixed container name and Start with a fixed session id, and now also answers
// Remove and can refuse Start on demand (the mid-chain failure the rollback test
// drives). Every command the Server pushes is recorded on the wire, so
// "the rollback removed the container" is an observed fact, not a mock
// expectation.
//
// Each authz/idempotency test carries a mutation comment: the plausible
// regression that reddens it. The load-bearing security legs are F2 ownership
// (spawn inherits the CALLER'S owner) and the despawn same-owner/indistinguishable
// not-found merge.

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// lifecycleFixture wraps placementFixture with a constructed lifecycleService and
// a second user (a DIFFERENT owner) for the cross-owner authz tests. The fixture
// agent (f.agentID, owned by admin) is the default CALLER.
type lifecycleFixture struct {
	placementFixture
	lc         *lifecycleService
	ownerAdmin store.AccountID // the fixture agent's owner (admin)
}

// newLifecycleFixture builds the placement fixture, constructs the lifecycleService
// over its real store + hub, and resolves the fixture agent's owner for the
// ownership assertions.
func newLifecycleFixture(t *testing.T) lifecycleFixture {
	t.Helper()
	pf := newPlacementFixture(t)
	pf.runner.forget() // discard the attach probe; assertions see only driven commands
	ctx := context.Background()
	owner, err := pf.store.AgentOwner(ctx, pf.agentID)
	if err != nil {
		t.Fatalf("AgentOwner(fixture agent) = %v", err)
	}
	return lifecycleFixture{
		placementFixture: pf,
		lc:               newLifecycleService(pf.store, pf.hub),
		ownerAdmin:       owner,
	}
}

// TestSpawnInheritsCallerOwner is THE core F2 authz test: a peer spawned by the
// fixture caller agent is owned by the CALLER'S OWNER (admin), never the caller
// agent itself and never any admin-literal. This is the ownership frame the whole
// spawn security model rests on.
//
// Mutation: creating the agent under `caller` (the spawning agent's id) or under a
// hard-coded admin id instead of the resolved callerOwner reddens the AgentOwner
// assertion below.
func TestSpawnInheritsCallerOwner(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()

	resp, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-1",
		DisplayName:     "Peer One",
		ClientRequestId: "spawn-1",
	})
	if err != nil {
		t.Fatalf("SpawnAsAccount = %v, want success", err)
	}
	newID := store.AccountID(resp.GetAgentAccountId())
	if newID == "" || newID == f.agentID {
		t.Fatalf("spawned agent id = %q, want a fresh id distinct from the caller %q", newID, f.agentID)
	}

	owner, err := f.store.AgentOwner(ctx, newID)
	if err != nil {
		t.Fatalf("AgentOwner(spawned) = %v", err)
	}
	if owner != f.ownerAdmin {
		t.Fatalf("spawned peer owner = %q, want the caller's owner %q (never the caller agent, never admin-literal)", owner, f.ownerAdmin)
	}
	if owner == f.agentID {
		t.Fatalf("spawned peer is owned by the CALLER AGENT %q — the F2 ownership frame is broken", f.agentID)
	}

	// The chain completed on the wire: provisioned, placed, started, recorded.
	if resp.GetContainerName() != fakeContainer {
		t.Fatalf("spawned container = %q, want %q", resp.GetContainerName(), fakeContainer)
	}
	if resp.GetSessionId() != fakeSessionID {
		t.Fatalf("spawned session = %q, want %q", resp.GetSessionId(), fakeSessionID)
	}
	if owner := sessionOwner(t, ctx, f.dsn, fakeSessionID); owner != string(newID) {
		t.Fatalf("session owner = %q, want the spawned peer %q", owner, newID)
	}
	if _, container, err := f.store.PlacementForAgent(ctx, newID); err != nil || container != fakeContainer {
		t.Fatalf("PlacementForAgent(spawned) = (%q, %v), want (%q, nil)", container, err, fakeContainer)
	}
}

// TestSpawnSetsParentToCaller pins the T3 set-at-creation edge: a peer spawned
// by the fixture caller agent has its parent_agent_id set to the CALLER'S
// account id (its spawner), so the agent tree records who spawned whom.
//
// Mutation: dropping ParentAgentID from the SpawnAsAccount NewAgent (or setting
// it to callerOwner instead of caller) leaves the spawned account a root and
// reddens the parent assertion below.
func TestSpawnSetsParentToCaller(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()

	resp, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-parent",
		DisplayName:     "Peer Parent",
		ClientRequestId: "spawn-parent",
	})
	if err != nil {
		t.Fatalf("SpawnAsAccount = %v, want success", err)
	}
	newID := store.AccountID(resp.GetAgentAccountId())

	acc, err := f.store.GetAccount(ctx, newID)
	if err != nil {
		t.Fatalf("GetAccount(spawned) = %v", err)
	}
	if acc.Agent == nil {
		t.Fatalf("spawned account is not an agent: %+v", acc)
	}
	if acc.Agent.ParentAgentID != f.agentID {
		t.Fatalf("spawned peer parent = %q, want the spawning caller %q", acc.Agent.ParentAgentID, f.agentID)
	}
}

// TestSpawnSameClientRequestIdRetryJoins pins the idempotent-resume contract: a
// second SpawnAsAccount for the SAME handle by the SAME owner, once the first is
// live and placed, returns the SAME container/session and creates NO second agent
// and NO second Provision on the wire.
//
// NOTE (idempotency shape, flagged to the driver): the record's brief framed this
// as a client_request_id in-flight JOIN. The hub's request-id dedup (router.go)
// only joins a CONCURRENTLY in-flight call — a completed call's entry is deleted
// on complete — so a SEQUENTIAL retry cannot join it. The durable idempotency for
// a completed spawn is the resume-or-reject path keyed on (handle, owner) +
// placement: a same-owner already-placed handle returns its existing
// container/session without a second Provision. That is the property this test
// pins; it holds regardless of the client_request_id and is the stronger,
// deterministic guarantee.
//
// Mutation: dropping the resume-or-reject placement short-circuit (always
// re-provisioning on a taken handle) reddens the "exactly one Provision" and
// "no second agent" assertions.
func TestSpawnSameClientRequestIdRetryJoins(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()

	first, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-dup",
		ClientRequestId: "spawn-dup",
	})
	if err != nil {
		t.Fatalf("first SpawnAsAccount = %v, want success", err)
	}

	second, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-dup",
		ClientRequestId: "spawn-dup",
	})
	if err != nil {
		t.Fatalf("retry SpawnAsAccount = %v, want idempotent success", err)
	}

	if second.GetAgentAccountId() != first.GetAgentAccountId() {
		t.Fatalf("retry agent id = %q, want the first %q (a second agent was created)", second.GetAgentAccountId(), first.GetAgentAccountId())
	}
	if second.GetContainerName() != first.GetContainerName() {
		t.Fatalf("retry container = %q, want the first %q", second.GetContainerName(), first.GetContainerName())
	}
	if second.GetSessionId() != first.GetSessionId() {
		t.Fatalf("retry session = %q, want the first %q", second.GetSessionId(), first.GetSessionId())
	}

	// Exactly one Provision reached the Runner: the retry short-circuited on the
	// live placement rather than provisioning a duplicate container.
	provisions := 0
	for _, c := range f.runner.commands() {
		if len(c) >= 9 && c[:9] == "provision" {
			provisions++
		}
	}
	if provisions != 1 {
		t.Fatalf("Runner saw %d Provision commands, want 1 (the retry must not re-provision); commands: %v", provisions, f.runner.commands())
	}
}

// TestSpawnMidChainFailureRollsBack pins the anti-burn rollback: when Start fails
// after Provision, the container is torn down (Remove on the wire) and its
// placement deleted, so the account is left UNPLACED — and a re-spawn of the SAME
// handle then succeeds (resumes) rather than hitting a burned handle.
//
// Mutation: skipping the rollback DeleteAgentPlacement leaves a live placement,
// so the re-spawn's resume path sees "already placed" and returns the stale
// (empty-session) result instead of a fresh successful spawn — or, if the
// placement's container name were still held, the re-provision would conflict.
// Either way the "re-spawn succeeds with a real session" assertion reddens.
func TestSpawnMidChainFailureRollsBack(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()

	f.runner.setFailStart(true) // the Runner refuses Start: mid-chain failure
	_, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-roll",
		ClientRequestId: "spawn-roll",
	})
	if err == nil {
		t.Fatal("SpawnAsAccount with a failing Start = nil error, want the failure surfaced")
	}

	// The container was torn down on the wire.
	if !f.runner.sawRemove(fakeContainer) {
		t.Fatalf("rollback never sent a Remove for %q; the container is stranded (commands: %v)", fakeContainer, f.runner.commands())
	}
	// The account exists but is UNPLACED — the handle is not burned.
	created, err := f.store.AgentByHandle(ctx, f.ownerAdmin, "peer-roll")
	if err != nil {
		t.Fatalf("AgentByHandle(peer-roll) after rollback = %v, want the durable account", err)
	}
	if _, _, err := f.store.PlacementForAgent(ctx, created.ID); err == nil {
		t.Fatal("PlacementForAgent(rolled-back agent) = success, want ErrNotFound (placement must be released)")
	}

	// Re-spawn the SAME handle: the Runner now answers Start, and the resume path
	// re-provisions/starts the existing UNPLACED account to a real session.
	f.runner.setFailStart(false)
	resp, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-roll",
		ClientRequestId: "spawn-roll-2",
	})
	if err != nil {
		t.Fatalf("re-spawn of the same handle after rollback = %v, want success (handle must not be burned)", err)
	}
	if resp.GetAgentAccountId() != string(created.ID) {
		t.Fatalf("re-spawn agent id = %q, want the resumed existing account %q", resp.GetAgentAccountId(), created.ID)
	}
	if resp.GetSessionId() != fakeSessionID {
		t.Fatalf("re-spawn session = %q, want a real session %q (the resume must Start)", resp.GetSessionId(), fakeSessionID)
	}
}

// TestSpawnSameHandleDifferentOwnerIsAlreadyExists pins that a foreign owner's
// handle is never resumed or stolen: a second user's agent spawning a peer whose
// handle collides with the first owner's peer gets in-band already_exists, never
// a resume of the other owner's account.
//
// Mutation: dropping the owner check on the resume path (resuming any same-handle
// unplaced/placed agent regardless of owner) reddens this — a foreign caller would
// resume someone else's agent and get a success.
func TestSpawnSameHandleDifferentOwnerIsAlreadyExists(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()

	// Owner A's caller spawns peer-shared.
	if _, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-shared",
		ClientRequestId: "spawn-a",
	}); err != nil {
		t.Fatalf("owner-A spawn = %v, want success", err)
	}

	// A DIFFERENT owner (user B) with its own caller agent.
	userB, err := f.store.CreateUser(ctx, store.NewUser{Handle: "userb", DisplayName: "User B"})
	if err != nil {
		t.Fatalf("CreateUser(userb) = %v", err)
	}
	callerB, err := f.store.CreateAgent(ctx, userB.ID, store.NewAgent{Handle: "caller-b", DisplayName: "Caller B"})
	if err != nil {
		t.Fatalf("CreateAgent(caller-b) = %v", err)
	}

	_, err = f.lc.SpawnAsAccount(ctx, callerB.ID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-shared",
		ClientRequestId: "spawn-b",
	})
	if err == nil {
		t.Fatal("owner-B spawn of owner-A's handle = success, want in-band already_exists (never steal)")
	}
	if got := connect.CodeOf(err); got != connect.CodeAlreadyExists {
		t.Fatalf("owner-B spawn code = %v, want CodeAlreadyExists", got)
	}
}

// TestDespawnDifferentOwnerIsIndistinguishableNotFound pins the load-bearing
// despawn authz merge: a caller despawning a peer owned by a DIFFERENT user gets
// the EXACT same CodeNotFound as despawning an unknown id — so a foreign peer's
// existence can never be probed.
//
// Mutation: returning a distinct code (e.g. PermissionDenied) for a
// foreign-but-existing target reddens the "same code as unknown" assertion — the
// existence probe the merge exists to prevent.
func TestDespawnDifferentOwnerIsIndistinguishableNotFound(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()

	// Owner B's peer, spawned by owner B's caller.
	userB, err := f.store.CreateUser(ctx, store.NewUser{Handle: "userb", DisplayName: "User B"})
	if err != nil {
		t.Fatalf("CreateUser(userb) = %v", err)
	}
	callerB, err := f.store.CreateAgent(ctx, userB.ID, store.NewAgent{Handle: "caller-b", DisplayName: "Caller B"})
	if err != nil {
		t.Fatalf("CreateAgent(caller-b) = %v", err)
	}
	peerB, err := f.lc.SpawnAsAccount(ctx, callerB.ID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-b",
		ClientRequestId: "spawn-peer-b",
	})
	if err != nil {
		t.Fatalf("owner-B spawn = %v, want success", err)
	}

	// The fixture caller (owner A) tries to despawn owner B's peer.
	_, foreignErr := f.lc.DespawnAsAccount(ctx, f.agentID, &compassv1internal.DespawnPeerRequest{AgentHandle: peerB.GetAgentAccountId()})
	if foreignErr == nil {
		t.Fatal("despawn of a foreign-owner peer = success, want CodeNotFound (never touch a foreign peer)")
	}

	// The same caller despawns an entirely unknown id.
	_, unknownErr := f.lc.DespawnAsAccount(ctx, f.agentID, &compassv1internal.DespawnPeerRequest{AgentHandle: "acct-does-not-exist"})
	if unknownErr == nil {
		t.Fatal("despawn of an unknown id = success, want CodeNotFound")
	}

	if fc, uc := connect.CodeOf(foreignErr), connect.CodeOf(unknownErr); fc != uc || fc != connect.CodeNotFound {
		t.Fatalf("foreign code = %v, unknown code = %v, want both CodeNotFound (indistinguishable)", fc, uc)
	}
	if fm, um := foreignErr.Error(), unknownErr.Error(); fm != um {
		t.Fatalf("foreign message = %q, unknown message = %q, want identical (indistinguishable even by text)", fm, um)
	}
}

// TestDespawnSelfIsInvalidArgument pins the self-despawn refusal through the real
// store path (the store-free lane covers the guard ordering; this covers it end
// to end): a caller despawning ITS OWN id gets CodeInvalidArgument.
//
// Mutation: removing the target==caller guard makes this fall through to the
// owner check and (since caller owns itself) attempt teardown — reddening the
// invalid_argument assertion.
func TestDespawnSelfIsInvalidArgument(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()

	_, err := f.lc.DespawnAsAccount(ctx, f.agentID, &compassv1internal.DespawnPeerRequest{AgentHandle: string(f.agentID)})
	if err == nil {
		t.Fatal("despawn of self = success, want CodeInvalidArgument")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("despawn-self code = %v, want CodeInvalidArgument", got)
	}
}

// TestDespawnSameOwnerSiblingSucceeds pins that authority is the OWNER's, not the
// spawner's: the fixture caller spawns a sibling, then a SECOND same-owner agent
// (which did NOT spawn it) despawns it successfully.
//
// Mutation: gating despawn on spawner identity rather than shared ownership
// reddens this — the non-spawning sibling would be refused.
func TestDespawnSameOwnerSiblingSucceeds(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()

	// The fixture caller (owner admin) spawns the target sibling.
	target, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle:          "sibling",
		ClientRequestId: "spawn-sibling",
	})
	if err != nil {
		t.Fatalf("spawn sibling = %v, want success", err)
	}

	// A SECOND agent under the SAME owner, which never spawned the target.
	other, err := f.store.CreateAgent(ctx, f.ownerAdmin, store.NewAgent{Handle: "other-sib", DisplayName: "Other"})
	if err != nil {
		t.Fatalf("CreateAgent(other-sib) = %v", err)
	}

	if _, err := f.lc.DespawnAsAccount(ctx, other.ID, &compassv1internal.DespawnPeerRequest{AgentHandle: target.GetAgentAccountId()}); err != nil {
		t.Fatalf("same-owner sibling despawn = %v, want success (owner authority, not spawner)", err)
	}

	// The container was torn down and its placement released.
	if !f.runner.sawRemove(fakeContainer) {
		t.Fatalf("despawn never sent a Remove for %q (commands: %v)", fakeContainer, f.runner.commands())
	}
	if _, _, err := f.store.PlacementForAgent(ctx, store.AccountID(target.GetAgentAccountId())); err == nil {
		t.Fatal("PlacementForAgent(despawned) = success, want ErrNotFound (placement released)")
	}
}

// TestDespawnSecondTimeIsIdempotentSuccess pins that a repeat despawn of an
// already-torn-down peer SUCCEEDS (placement-absent idempotent path), never
// not_found — the same already-stopped-succeeds contract StopAgentSession has.
//
// Mutation: treating a placement-absent target as not_found reddens the second
// despawn's success assertion.
func TestDespawnSecondTimeIsIdempotentSuccess(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()

	target, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-twice",
		ClientRequestId: "spawn-twice",
	})
	if err != nil {
		t.Fatalf("spawn = %v, want success", err)
	}
	req := &compassv1internal.DespawnPeerRequest{AgentHandle: target.GetAgentAccountId()}

	if _, err := f.lc.DespawnAsAccount(ctx, f.agentID, req); err != nil {
		t.Fatalf("first despawn = %v, want success", err)
	}
	if _, err := f.lc.DespawnAsAccount(ctx, f.agentID, req); err != nil {
		t.Fatalf("second despawn of an already-torn-down peer = %v, want idempotent success (not not_found)", err)
	}
}

// TestRemoveAgentWorkspaceHandler pins the operator door (admin path, through the
// connect client): it Removes the container and releases the placement, an unknown
// container succeeds (idempotent), and a nil-hub server is Unavailable (the same
// contract TestAgentSessionRPCsWithoutRunnerHubAreUnavailable pins).
//
// Mutation: dropping the DeleteAgentPlacement leaves the placement live after
// Remove, reddening the "placement released" assertion; dropping the nil-hub guard
// panics instead of Unavailable.
func TestRemoveAgentWorkspaceHandler(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background()

	// Provision a real placement to release.
	if _, err := f.client.ProvisionAgentWorkspace(ctx, connect.NewRequest(&compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: string(f.agentID), ClientRequestId: "prov-rm"})); err != nil {
		t.Fatalf("ProvisionAgentWorkspace = %v, want success", err)
	}
	if _, _, err := f.store.PlacementForAgent(ctx, f.agentID); err != nil {
		t.Fatalf("PlacementForAgent after provision = %v, want a live placement", err)
	}

	if _, err := f.client.RemoveAgentWorkspace(ctx, connect.NewRequest(&compassv1.RemoveAgentWorkspaceRequest{
		ContainerName:   fakeContainer,
		ClientRequestId: "rm-1",
	})); err != nil {
		t.Fatalf("RemoveAgentWorkspace = %v, want success", err)
	}
	if !f.runner.sawRemove(fakeContainer) {
		t.Fatalf("Runner never received a Remove for %q (commands: %v)", fakeContainer, f.runner.commands())
	}
	if _, _, err := f.store.PlacementForAgent(ctx, f.agentID); err == nil {
		t.Fatal("PlacementForAgent after Remove = success, want ErrNotFound (placement must be released)")
	}

	// Idempotent: removing an unknown container succeeds.
	if _, err := f.client.RemoveAgentWorkspace(ctx, connect.NewRequest(&compassv1.RemoveAgentWorkspaceRequest{
		ContainerName:   "compass-agent-unknown",
		ClientRequestId: "rm-2",
	})); err != nil {
		t.Fatalf("RemoveAgentWorkspace(unknown container) = %v, want idempotent success", err)
	}
}

// TestRemoveAgentWorkspaceWithoutRunnerHubIsUnavailable pins that the operator
// door on a server with no Runner door (hub nil) is CodeUnavailable — never a
// panic, mirroring TestAgentSessionRPCsWithoutRunnerHubAreUnavailable.
func TestRemoveAgentWorkspaceWithoutRunnerHubIsUnavailable(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("test", bus, nil, nil, nil, nil, nil)
	client := newH2CClient(t, newH2CTestServer(t, svc))

	_, err := client.RemoveAgentWorkspace(context.Background(), connect.NewRequest(&compassv1.RemoveAgentWorkspaceRequest{
		ContainerName: "c1",
	}))
	if err == nil {
		t.Fatal("RemoveAgentWorkspace on a hubless server = nil error, want CodeUnavailable")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("RemoveAgentWorkspace code = %v, want CodeUnavailable", got)
	}
}

// TestDespawnCallerNotAnAgentIsInternal pins the caller-first fail-closed check:
// after the caller is resolved BEFORE the target, a caller that is NOT an agent
// account (a plain user) deterministically hits errCallerNotAgent -> CodeInternal
// regardless of the target — the wiring-invariant violation the hub should never
// delegate.
//
// Mutation: resolving the target before the caller (the old ordering) would let a
// non-agent caller against an unknown target return CodeNotFound instead, and also
// reintroduce the latency side-channel — reddening the CodeInternal assertion.
func TestDespawnCallerNotAnAgentIsInternal(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()

	// A plain USER account — NOT an agent — used as the caller.
	user, err := f.store.CreateUser(ctx, store.NewUser{Handle: "not-an-agent", DisplayName: "Not An Agent"})
	if err != nil {
		t.Fatalf("CreateUser(not-an-agent) = %v", err)
	}

	// Target differs from the caller so the self-despawn guard does not fire first.
	_, err = f.lc.DespawnAsAccount(ctx, user.ID, &compassv1internal.DespawnPeerRequest{AgentHandle: "acct-some-other-id"})
	if err == nil {
		t.Fatal("despawn by a non-agent caller = success, want CodeInternal (errCallerNotAgent)")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("non-agent-caller despawn code = %v, want CodeInternal", got)
	}
	if !errors.Is(err, errCallerNotAgent) {
		t.Fatalf("non-agent-caller despawn err = %v, want wrapping errCallerNotAgent", err)
	}
}

// TestSpawnHandleCollidesWithUserAccountIsAlreadyExists pins that a spawn Handle
// colliding with a NON-agent (user) account collapses to CodeAlreadyExists —
// NOT a resume, and NOT a leak that the handle belongs to a user. CreateAgent
// conflicts on the taken handle, AgentByHandle fails closed to ErrNotFound for a
// non-agent handle, and resumeOrReject maps that to errHandleTaken — the same
// answer a human gets for any taken handle.
//
// Mutation: revealing the account kind (e.g. a distinct code for a user-held
// handle) or resuming against a non-agent account reddens this.
func TestSpawnHandleCollidesWithUserAccountIsAlreadyExists(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()

	// A plain USER holds the handle the spawn will request.
	if _, err := f.store.CreateUser(ctx, store.NewUser{Handle: "taken-handle", DisplayName: "Human"}); err != nil {
		t.Fatalf("CreateUser(taken-handle) = %v", err)
	}

	_, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle:          "taken-handle",
		ClientRequestId: "spawn-collides-user",
	})
	if err == nil {
		t.Fatal("spawn onto a user-held handle = success, want CodeAlreadyExists (never resume/steal, never leak account kind)")
	}
	if got := connect.CodeOf(err); got != connect.CodeAlreadyExists {
		t.Fatalf("user-handle-collision spawn code = %v, want CodeAlreadyExists", got)
	}
	if !errors.Is(err, errHandleTaken) {
		t.Fatalf("user-handle-collision spawn err = %v, want wrapping errHandleTaken", err)
	}
}
