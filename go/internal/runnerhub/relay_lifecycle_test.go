//go:build unix

package runnerhub

// The agent-lifecycle Server leg (spawn/despawn record T4, fail-closed authz —
// the load-bearing security leg, exactly as RelayCommsCall). Every test here
// defends one invariant of the session->account resolution + RelayLifecycleCall
// handler: the Runner asserts no account, so the SERVER's binding is the sole
// authority for whose account a relayed lifecycle call runs under. A regression
// that let an unbound, stopped, or reconnect-dropped session resolve to ANY
// account — or delegated under the wrong account, or turned a tool failure into
// a transport teardown — must redden a test below.
//
// White-box (package runnerhub) so the tests drive the unexported binding
// lifecycle and the resolution edge directly, asserting the account attribution
// through the fake LifecycleCaller. Sleep-free: the hub calls the caller inline,
// so every assertion reads a synchronously-recorded fact.

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// relaySpawn builds a RelayLifecycleCallRequest carrying a spawn variant under callID.
func relaySpawn(sessionID, callID string, spawn *compassv1internal.SpawnPeerRequest) *compassv1internal.RelayLifecycleCallRequest {
	return &compassv1internal.RelayLifecycleCallRequest{
		SessionId: sessionID,
		Call: &compassv1internal.LifecycleCallRequest{
			CallId: callID,
			Call:   &compassv1internal.LifecycleCallRequest_Spawn{Spawn: spawn},
		},
	}
}

// relayDespawn builds a RelayLifecycleCallRequest carrying a despawn variant under callID.
func relayDespawn(sessionID, callID string, despawn *compassv1internal.DespawnPeerRequest) *compassv1internal.RelayLifecycleCallRequest {
	return &compassv1internal.RelayLifecycleCallRequest{
		SessionId: sessionID,
		Call: &compassv1internal.LifecycleCallRequest{
			CallId: callID,
			Call:   &compassv1internal.LifecycleCallRequest_Despawn{Despawn: despawn},
		},
	}
}

// 1. An unbound session fails closed CodeNotFound and NEVER reaches the caller —
// no delegation is attempted for a session the hub has no binding for. This is
// the core fail-closed guard: a session_id on the wire selects an account, it
// never carries one, so an id the hub never bound resolves to nothing.
//
// Mutation: hardcode accountForSession to return a fixed account (ok=true) and
// this test fails twice over — the error becomes nil and the caller records a
// call.
func TestRelayLifecycleCallUnboundSessionFailsClosedNotFound(t *testing.T) {
	hub, fake := newHubWithLifecycle()

	_, err := hub.RelayLifecycleCall(context.Background(), relaySpawn("never-bound", "lc-1", &compassv1internal.SpawnPeerRequest{Handle: "peer"}))
	if err == nil {
		t.Fatal("RelayLifecycleCall for an unbound session = nil error, want CodeNotFound (fail closed)")
	}
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("unbound-session error code = %v, want NotFound", got)
	}
	if calls := fake.snapshot(); len(calls) != 0 {
		t.Fatalf("caller was invoked %d times for an unbound session, want 0 (no delegation attempt)", len(calls))
	}
}

// 2. A hub with no LifecycleCaller wired fails RelayLifecycleCall closed with
// CodeUnavailable — the lifecycle leg is not mounted, never a silent success.
// This is checked BEFORE session resolution, so even a bound session gets
// Unavailable on a caller-less hub.
//
// Mutation: reordering the checks so resolution runs first would change the code
// to NotFound for an unbound+nil case — the second sub-assertion (unbound session
// + nil caller still Unavailable) reddens that reorder.
func TestRelayLifecycleCallNilCallerIsUnavailableBeforeResolution(t *testing.T) {
	t.Run("bound session still Unavailable", func(t *testing.T) {
		hub := newHubOnly()  // no LifecycleCaller wired
		bindLiveSession(hub) // a live binding exists, proving the nil guard precedes resolution

		_, err := hub.RelayLifecycleCall(context.Background(), relaySpawn("sess-1", "lc-2", &compassv1internal.SpawnPeerRequest{Handle: "peer"}))
		if err == nil {
			t.Fatal("RelayLifecycleCall on a caller-less hub = nil error, want CodeUnavailable")
		}
		if got := connect.CodeOf(err); got != connect.CodeUnavailable {
			t.Fatalf("nil-caller (bound session) error code = %v, want Unavailable", got)
		}
	})
	t.Run("unbound session still Unavailable (nil-check precedes resolution)", func(t *testing.T) {
		hub := newHubOnly() // no LifecycleCaller wired, no binding

		_, err := hub.RelayLifecycleCall(context.Background(), relaySpawn("never-bound", "lc-2b", &compassv1internal.SpawnPeerRequest{Handle: "peer"}))
		if err == nil {
			t.Fatal("RelayLifecycleCall on a caller-less hub (unbound) = nil error, want CodeUnavailable")
		}
		if got := connect.CodeOf(err); got != connect.CodeUnavailable {
			t.Fatalf("nil-caller (unbound session) error code = %v, want Unavailable, not NotFound (proves nil-check precedes resolution)", got)
		}
	})
}

// 3. THE core authz test: the RESOLVED caller account (the hub's own binding)
// reaches the caller — never a request field, never a literal admin id. A spawn
// for the bound session_id delegates under acct-agent, the account the hub bound.
//
// Mutation: passing a request field or a literal admin id instead of the
// resolved account reddens the account assertion.
func TestRelayLifecycleCallDelegatesUnderResolvedCallerAccount(t *testing.T) {
	hub, fake := newHubWithLifecycle()
	fake.spawnResp = &compassv1internal.SpawnPeerResponse{AgentAccountId: "acct-new", ContainerName: "c-new", SessionId: "sess-new"}
	bindLiveSession(hub) // sess-1 -> acct-agent

	_, err := hub.RelayLifecycleCall(context.Background(), relaySpawn("sess-1", "lc-3", &compassv1internal.SpawnPeerRequest{Handle: "peer"}))
	if err != nil {
		t.Fatalf("RelayLifecycleCall(spawn) = %v, want success", err)
	}
	calls := fake.snapshot()
	if len(calls) != 1 {
		t.Fatalf("caller invoked %d times, want exactly 1", len(calls))
	}
	if calls[0].account != "acct-agent" {
		t.Fatalf("caller attributed to %q, want the bound caller account acct-agent (never request-asserted, never admin)", calls[0].account)
	}
}

// 4. A caller (tool-level) error surfaces IN-BAND in a SUCCESSFUL (nil-err)
// response as the LifecycleCallError variant — the agent renders it and the
// transport survives. Only a resolution miss / no-caller is a Connect error.
//
// Mutation: returning the caller error as a Connect error (instead of in-band)
// reddens the err==nil assertion.
func TestRelayLifecycleCallToolErrorIsInBandNotStreamError(t *testing.T) {
	hub, fake := newHubWithLifecycle()
	fake.spawnErr = connect.NewError(connect.CodeAlreadyExists, errors.New("handle already taken"))
	bindLiveSession(hub)

	resp, err := hub.RelayLifecycleCall(context.Background(), relaySpawn("sess-1", "lc-4", &compassv1internal.SpawnPeerRequest{Handle: "dup"}))
	if err != nil {
		t.Fatalf("RelayLifecycleCall with a tool error returned a Go error %v, want nil (in-band render)", err)
	}
	toolErr := resp.GetResult().GetError()
	if toolErr == nil {
		t.Fatal("response has no in-band LifecycleCallError, want the tool failure rendered in-band")
	}
	if toolErr.GetCode() != "already_exists" {
		t.Fatalf("in-band error code = %q, want already_exists", toolErr.GetCode())
	}
	if toolErr.GetMessage() != "already_exists: handle already taken" {
		t.Fatalf("in-band error message = %q, want the caller's rendered error", toolErr.GetMessage())
	}
	// The call_id still round-trips on the error variant so the agent correlates
	// the failed call.
	if got := resp.GetResult().GetCallId(); got != "lc-4" {
		t.Fatalf("in-band error call_id = %q, want lc-4", got)
	}
}

// 4b. The despawn tool-error path is in-band too — the same contract as spawn,
// on the one variant carrying a request-controlled account. A DespawnAsAccount
// error (e.g. an indistinguishable not-found for a foreign/unknown target)
// surfaces as the LifecycleCallError variant in a nil-err response, never a
// transport error that would tear the stream down.
//
// Mutation: returning the caller error as a Connect error (instead of in-band)
// reddens the err==nil assertion; it also exercises the despawnErr field, so a
// caller that dropped despawn error propagation would surface here.
func TestRelayLifecycleCallDespawnToolErrorIsInBand(t *testing.T) {
	hub, fake := newHubWithLifecycle()
	fake.despawnErr = connect.NewError(connect.CodeNotFound, errors.New("peer not found"))
	bindLiveSession(hub)

	resp, err := hub.RelayLifecycleCall(context.Background(), relayDespawn("sess-1", "lc-4b", &compassv1internal.DespawnPeerRequest{AgentHandle: "acct-victim"}))
	if err != nil {
		t.Fatalf("RelayLifecycleCall with a despawn tool error returned a Go error %v, want nil (in-band render)", err)
	}
	toolErr := resp.GetResult().GetError()
	if toolErr == nil {
		t.Fatal("response has no in-band LifecycleCallError, want the despawn failure rendered in-band")
	}
	if toolErr.GetCode() != "not_found" {
		t.Fatalf("in-band error code = %q, want not_found", toolErr.GetCode())
	}
	if got := resp.GetResult().GetCallId(); got != "lc-4b" {
		t.Fatalf("in-band error call_id = %q, want lc-4b", got)
	}
}

// 5. On a successful spawn the minted call_id is echoed onto the result so the
// agent correlates its call.
func TestRelayLifecycleCallEchoesCallIDOnSuccess(t *testing.T) {
	hub, fake := newHubWithLifecycle()
	fake.spawnResp = &compassv1internal.SpawnPeerResponse{AgentAccountId: "acct-new"}
	bindLiveSession(hub)

	resp, err := hub.RelayLifecycleCall(context.Background(), relaySpawn("sess-1", "lc-5", &compassv1internal.SpawnPeerRequest{Handle: "peer"}))
	if err != nil {
		t.Fatalf("RelayLifecycleCall(spawn) = %v, want success", err)
	}
	if got := resp.GetResult().GetCallId(); got != "lc-5" {
		t.Fatalf("response call_id = %q, want the request's lc-5", got)
	}
	if resp.GetResult().GetSpawn() != fake.spawnResp {
		t.Fatalf("response spawn result is not the caller's response")
	}
}

// 6. Spawn and despawn dispatch to the matching caller method — a spawn reaches
// SpawnAsAccount (a spawn variant recorded, never a despawn) and a despawn
// reaches DespawnAsAccount.
//
// Mutation: dispatching both variants to one method (or swapping them) reddens
// the recorded-variant assertions.
func TestRelayLifecycleCallDispatchesSpawnVsDespawn(t *testing.T) {
	t.Run("spawn", func(t *testing.T) {
		hub, fake := newHubWithLifecycle()
		fake.spawnResp = &compassv1internal.SpawnPeerResponse{AgentAccountId: "acct-new"}
		bindLiveSession(hub)

		resp, err := hub.RelayLifecycleCall(context.Background(), relaySpawn("sess-1", "lc-6a", &compassv1internal.SpawnPeerRequest{Handle: "peer"}))
		if err != nil {
			t.Fatalf("RelayLifecycleCall(spawn) = %v, want success", err)
		}
		calls := fake.snapshot()
		if len(calls) != 1 || calls[0].spawn == nil || calls[0].despawn != nil {
			t.Fatalf("spawn call did not reach SpawnAsAccount exclusively: %+v", calls)
		}
		if resp.GetResult().GetDespawn() != nil {
			t.Fatal("spawn produced a despawn result variant")
		}
	})
	t.Run("despawn", func(t *testing.T) {
		hub, fake := newHubWithLifecycle()
		fake.despawnResp = &compassv1internal.DespawnPeerResponse{}
		bindLiveSession(hub)

		resp, err := hub.RelayLifecycleCall(context.Background(), relayDespawn("sess-1", "lc-6b", &compassv1internal.DespawnPeerRequest{AgentHandle: "acct-victim"}))
		if err != nil {
			t.Fatalf("RelayLifecycleCall(despawn) = %v, want success", err)
		}
		calls := fake.snapshot()
		if len(calls) != 1 || calls[0].despawn == nil || calls[0].spawn != nil {
			t.Fatalf("despawn call did not reach DespawnAsAccount exclusively: %+v", calls)
		}
		// The teardown is attributed to the RESOLVED caller (acct-agent, bound to
		// sess-1), never the request-named target acct-victim — DespawnPeerRequest
		// is the one variant with a request-controlled account, so this pins that
		// the hub never lets a caller name whose authority a despawn runs under.
		//
		// Mutation: forwarding c.Despawn.GetAgentAccountId() as the caller (a
		// confusion attributing the teardown under the victim) reddens this.
		if calls[0].account != "acct-agent" {
			t.Fatalf("despawn attributed to %q, want the bound caller acct-agent (never the request target acct-victim)", calls[0].account)
		}
		if resp.GetResult().GetDespawn() == nil {
			t.Fatal("despawn produced no despawn result variant")
		}
		if got := resp.GetResult().GetCallId(); got != "lc-6b" {
			t.Fatalf("despawn response call_id = %q, want lc-6b", got)
		}
	})
}

// 7. A call whose spawn/despawn oneof is unset is an invalid request. The
// dispatch's default arm returns a Connect CodeInvalidArgument error, which
// RelayLifecycleCall renders IN-BAND (a non-resolution error is always in-band)
// — so the Go error is nil, result.error.code == "invalid_argument", and the
// caller is NEVER invoked (a malformed call reaches no execution method).
//
// Mutation: returning a Connect error for the unset oneof (instead of in-band)
// reddens the err==nil assertion; invoking a caller method for it reddens the
// len==0 assertion.
func TestRelayLifecycleCallUnsetOneofIsInBandInvalidArgument(t *testing.T) {
	hub, fake := newHubWithLifecycle()
	bindLiveSession(hub)

	resp, err := hub.RelayLifecycleCall(context.Background(), &compassv1internal.RelayLifecycleCallRequest{
		SessionId: "sess-1",
		Call:      &compassv1internal.LifecycleCallRequest{CallId: "lc-7"}, // no spawn/despawn variant set
	})
	if err != nil {
		t.Fatalf("RelayLifecycleCall with an unset oneof returned a Go error %v, want nil (in-band render)", err)
	}
	toolErr := resp.GetResult().GetError()
	if toolErr == nil {
		t.Fatal("response has no in-band LifecycleCallError, want the malformed call rendered in-band")
	}
	if toolErr.GetCode() != "invalid_argument" {
		t.Fatalf("in-band error code = %q, want invalid_argument", toolErr.GetCode())
	}
	if got := resp.GetResult().GetCallId(); got != "lc-7" {
		t.Fatalf("in-band error call_id = %q, want lc-7", got)
	}
	if calls := fake.snapshot(); len(calls) != 0 {
		t.Fatalf("caller invoked %d times for a malformed call, want 0", len(calls))
	}
}
