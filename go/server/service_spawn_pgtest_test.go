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

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
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
