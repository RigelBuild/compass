//go:build pgtest && unix

package server

// The SpawnAgent composite handler (spawn.go), against a real Postgres AND a
// real Runner door — the same placementFixture + recording fake Runner the
// Provision/Start seam tests use, so every assertion reads a wire fact (which
// commands the Server actually pushed), not a mock expectation. SpawnAgent is a
// wire RPC, so these drive it through f.client.
//
// Each test pins one T0 acceptance leg with its mutation comment: the happy
// Provision→Start path, end-to-end idempotency (exactly one Provision on a
// repeated client_request_id), and the pre-Provision reject-on-live short-circuit
// (zero Provision on the reject path — the teeth that separate a real
// pre-Provision reject from an implementation that collides mid-Provision).

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// TestSpawnAgentRunsProvisionThenStart pins the happy path: SpawnAgent
// provisions the container and starts its session in one call, returning the
// Start's session id and the Provision's container name, and pushes exactly one
// Provision and one Start on the wire.
//
// Mutation: an orchestration that skipped Provision or Start (or swapped the
// response fields) reddens the returned-id or the wire-command assertions.
func TestSpawnAgentRunsProvisionThenStart(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background()

	resp, err := f.client.SpawnAgent(ctx, connect.NewRequest(&compassv1.SpawnAgentRequest{
		AgentAccountId:  string(f.agentID),
		InitialPrompt:   "go",
		ClientRequestId: "spawn-happy",
	}))
	if err != nil {
		t.Fatalf("SpawnAgent = %v, want success", err)
	}
	if resp.Msg.GetSessionId() != fakeSessionID {
		t.Fatalf("session id = %q, want %q (from the internal Start)", resp.Msg.GetSessionId(), fakeSessionID)
	}
	if resp.Msg.GetContainerName() != fakeContainer {
		t.Fatalf("container = %q, want %q (from the internal Provision)", resp.Msg.GetContainerName(), fakeContainer)
	}

	if got := f.runner.provisionCount(); got != 1 {
		t.Fatalf("Provision commands = %d, want 1; commands: %v", got, f.runner.commands())
	}
	if !sawStartFor(f, fakeContainer) {
		t.Fatalf("no Start for %q on the wire; commands: %v", fakeContainer, f.runner.commands())
	}
}

// TestSpawnAgentIsIdempotentOnRepeatedClientRequestId is the end-to-end
// idempotency tooth: a second SpawnAgent with the SAME client_request_id returns
// the SAME session id and provisions NO second container — asserted on the wire
// as exactly ONE Provision across both calls. This is the composite span the
// three lower dedup primitives do not compose for a sequential completed retry.
//
// Mutation: dropping the client_request_id-keyed spawn memo (running the whole
// Provision→Start again on the retry) reddens the "exactly one Provision"
// assertion — two containers would be churned for one logical spawn.
func TestSpawnAgentIsIdempotentOnRepeatedClientRequestId(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background()

	first, err := f.client.SpawnAgent(ctx, connect.NewRequest(&compassv1.SpawnAgentRequest{
		AgentAccountId:  string(f.agentID),
		ClientRequestId: "spawn-dup",
	}))
	if err != nil {
		t.Fatalf("first SpawnAgent = %v, want success", err)
	}
	second, err := f.client.SpawnAgent(ctx, connect.NewRequest(&compassv1.SpawnAgentRequest{
		AgentAccountId:  string(f.agentID),
		ClientRequestId: "spawn-dup",
	}))
	if err != nil {
		t.Fatalf("retry SpawnAgent = %v, want idempotent success", err)
	}

	if second.Msg.GetSessionId() != first.Msg.GetSessionId() {
		t.Fatalf("retry session id = %q, want the first %q (a second spawn ran)", second.Msg.GetSessionId(), first.Msg.GetSessionId())
	}
	if got := f.runner.provisionCount(); got != 1 {
		t.Fatalf("Provision commands = %d, want 1 (the retry must join, not re-provision); commands: %v", got, f.runner.commands())
	}
}

// TestSpawnAgentRejectsWhenAgentAlreadyLive is the reject-on-live tooth: when the
// Runner's authoritative status scan reports the target agent already holds a
// live session, SpawnAgent returns CodeAlreadyExists BEFORE issuing any
// Provision — asserted on the wire as ZERO Provision commands, in addition to the
// code. Zero-Provision is what distinguishes a real pre-Provision short-circuit
// from an implementation that only collides on the container name mid-Provision
// (which would return an internal error AND churn a container per rejected spawn).
//
// Mutation: ordering the reject-on-live check AFTER Provision (or dropping it)
// reddens the zero-Provision assertion — a Provision would reach the wire before
// the collision surfaced; and returning the container-name collision's internal
// error rather than the pre-check's AlreadyExists reddens the code assertion.
func TestSpawnAgentRejectsWhenAgentAlreadyLive(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background()

	// The Runner reports a live session already bound to this agent account — the
	// all-sessions scan the reject-on-live check reads.
	f.runner.setStatuses(&compassv1.AgentSessionStatus{
		SessionId:      "sess-live",
		State:          compassv1.AgentSessionState_AGENT_SESSION_STATE_READY,
		AgentAccountId: string(f.agentID),
	})

	_, err := f.client.SpawnAgent(ctx, connect.NewRequest(&compassv1.SpawnAgentRequest{
		AgentAccountId:  string(f.agentID),
		ClientRequestId: "spawn-reject",
	}))
	if err == nil {
		t.Fatal("SpawnAgent for a live agent = nil error, want CodeAlreadyExists")
	}
	if got := connect.CodeOf(err); got != connect.CodeAlreadyExists {
		t.Fatalf("SpawnAgent code = %v, want CodeAlreadyExists", got)
	}
	if got := f.runner.provisionCount(); got != 0 {
		t.Fatalf("Provision commands = %d, want 0 (reject-on-live is a PRE-Provision short-circuit); commands: %v", got, f.runner.commands())
	}
}

// sawStartFor reports whether the Server pushed a Start for containerName.
func sawStartFor(f placementFixture, containerName string) bool {
	for _, c := range f.runner.commands() {
		if c == "start "+containerName {
			return true
		}
	}
	return false
}

// TestSpawnAgentFailedRetryReattempts is the DL-169 re-attempt tooth: when the
// first spawn's Start fails, a retry with the SAME (account, client_request_id)
// must RE-ATTEMPT the spawn — not replay the cached failure. The mechanism is
// settleSpawn dropping a FAILED memo entry (so the retry misses the memo and
// runs afresh) while retaining a success.
//
// Mutation: retaining the failed entry (removing settleSpawn's delete-on-error)
// makes the retry JOIN the settled failure and replay its error — so the retry
// would error instead of succeeding, reddening the second-call assertion. The
// retry additionally drives a fresh Start to the wire (the re-attempt), which a
// pure replay never would.
func TestSpawnAgentFailedRetryReattempts(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background()

	f.runner.setFailStart(true) // the Runner refuses Start: the first spawn fails mid-chain.
	_, err := f.client.SpawnAgent(ctx, connect.NewRequest(&compassv1.SpawnAgentRequest{
		AgentAccountId:  string(f.agentID),
		ClientRequestId: "spawn-reattempt",
	}))
	if err == nil {
		t.Fatal("first SpawnAgent with a failing Start = nil error, want the failure surfaced")
	}

	// The Runner now accepts Start; a retry of the SAME id must re-attempt and
	// succeed, proving the failed memo entry was dropped rather than replayed.
	f.runner.setFailStart(false)
	resp, err := f.client.SpawnAgent(ctx, connect.NewRequest(&compassv1.SpawnAgentRequest{
		AgentAccountId:  string(f.agentID),
		ClientRequestId: "spawn-reattempt",
	}))
	if err != nil {
		t.Fatalf("retry SpawnAgent after a failed first = %v, want re-attempt success (a retained failure would replay the error)", err)
	}
	if resp.Msg.GetSessionId() != fakeSessionID {
		t.Fatalf("retry session id = %q, want a real session %q (the re-attempt must Start)", resp.Msg.GetSessionId(), fakeSessionID)
	}
	if !sawStartFor(f, resp.Msg.GetContainerName()) {
		t.Fatalf("no Start for %q on the wire; the retry did not re-attempt (commands: %v)", resp.Msg.GetContainerName(), f.runner.commands())
	}
}

// TestSpawnAgentCrossAccountSameCridIsDistinct is the account-scoped-memo tooth:
// two spawns sharing a client_request_id but targeting DIFFERENT agent accounts
// are DISTINCT spawns — the second must NOT join the first's memo entry. The
// discriminator is the wire: a distinct spawn drives its OWN Provision (two
// Provisions reach the Runner), matching the account binding provisionDedupID
// enforces one layer down.
//
// Mutation: keying the memo on client_request_id ALONE (dropping the account
// from spawnKey) makes the second account JOIN the first's completed entry and
// return the first account's session, provisioning NOTHING for the second — so
// only ONE Provision reaches the wire and the call returns a (wrong) success.
// This test reddens that: it asserts a SECOND Provision hit the wire.
//
// (The fake Runner answers every Provision with the same fixed container name,
// so the second account's Provision then trips the placement store's
// one-container-per-name guard — an error the real per-account container name
// would not produce. That post-Provision outcome is irrelevant to the tooth:
// the memo-key bug is already caught by whether a second Provision reached the
// wire AT ALL, which happens before any placement write.)
func TestSpawnAgentCrossAccountSameCridIsDistinct(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background()

	// A second real account: Provision reads persona/role, so it must exist.
	admin, err := f.store.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	other, err := f.store.CreateAgent(ctx, admin.ID, store.NewAgent{Handle: "borealis", DisplayName: "Borealis"})
	if err != nil {
		t.Fatalf("CreateAgent(second): %v", err)
	}

	if _, err := f.client.SpawnAgent(ctx, connect.NewRequest(&compassv1.SpawnAgentRequest{
		AgentAccountId:  string(f.agentID),
		ClientRequestId: "spawn-shared",
	})); err != nil {
		t.Fatalf("first-account SpawnAgent = %v, want success", err)
	}
	// The second account reuses the SAME client_request_id. With correct
	// (account, id) keying it does NOT join, so it drives its own Provision; the
	// crid-alone bug would join and return the first's result with no Provision.
	_, err = f.client.SpawnAgent(ctx, connect.NewRequest(&compassv1.SpawnAgentRequest{
		AgentAccountId:  string(other.ID),
		ClientRequestId: "spawn-shared",
	}))
	// A distinct spawn reaching Provision is the tooth; the fake's shared
	// container name then trips a placement conflict, which is fine — a JOIN
	// (the bug) would instead have returned nil error with no second Provision.
	if err == nil {
		t.Fatal("second-account spawn returned success, want a distinct spawn that reached Provision — a nil error here means it JOINED the first account's memo (crid-only key)")
	}
	if got := f.runner.provisionCount(); got != 2 {
		t.Fatalf("Provision commands = %d, want 2 (a client_request_id shared across accounts must NOT join — the second account must drive its own Provision); commands: %v", got, f.runner.commands())
	}
}

// TestSpawnAgentMemoEvictsSuccessAfterTTL is the bounded-memo tooth: a settled
// SUCCESS entry is retained (for idempotent replay) then evicted after
// spawnMemoTTL, so the memo does not grow one permanent entry per successful
// spawn for the process lifetime. It drives the scheduleAfter seam directly, so
// the eviction is deterministic without wall-clock waiting.
//
// Mutation: dropping settleSpawn's scheduled eviction (retain-forever) leaves
// the entry in the map after the timer fires, reddening the post-eviction
// emptiness assertion. Scheduling with the wrong delay reddens the TTL assertion.
func TestSpawnAgentMemoEvictsSuccessAfterTTL(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background()

	var gotDelay time.Duration
	var evict func()
	f.svc.scheduleAfter = func(d time.Duration, fn func()) *time.Timer {
		gotDelay = d
		evict = fn
		return nil // the captured fn is fired by the test, not a real timer.
	}

	if _, err := f.svc.SpawnAgent(ctx, connect.NewRequest(&compassv1.SpawnAgentRequest{
		AgentAccountId:  string(f.agentID),
		ClientRequestId: "spawn-evict",
	})); err != nil {
		t.Fatalf("SpawnAgent = %v, want success", err)
	}

	if gotDelay != spawnMemoTTL {
		t.Fatalf("eviction scheduled after %v, want spawnMemoTTL %v", gotDelay, spawnMemoTTL)
	}
	key := spawnKey{account: string(f.agentID), crid: "spawn-evict"}
	if !f.svc.hasSpawn(key) {
		t.Fatal("settled success not retained in the memo before its TTL — the idempotency-replay window is gone")
	}
	if evict == nil {
		t.Fatal("no eviction was scheduled for a settled success — the memo grows unbounded")
	}
	evict()
	if f.svc.hasSpawn(key) {
		t.Fatal("settled success still in the memo after eviction fired — the memo is not bounded")
	}
}

// hasSpawn reports whether the memo holds an entry for key — the white-box view
// the eviction test asserts the bound with. Under spawnMu, like every memo read.
func (s *service) hasSpawn(key spawnKey) bool {
	s.spawnMu.Lock()
	defer s.spawnMu.Unlock()
	_, ok := s.spawns[key]
	return ok
}
