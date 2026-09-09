//go:build podman

package e2e

// T4 (RIG-3531): the multi-tenant TRANSPORT proof over the real stack. Zero
// container agents — pure observer-scoped RPC actors, which is what makes it the
// fast leg: two owner users, one (never-started) agent under each, a private
// channel under owner-1, and every assertion driven over the real TLS door with
// per-account bearers minted by AsObserver (fixture.go:227).
//
// The observer credential is the whole point. Every other fixture RPC rides the
// bootstrap-admin bearer (clients.go:63) and an admin sees everything, so a
// NEGATIVE visibility claim is unassertable through it; the per-event D9 filter
// runs against the STREAM's authenticated account (comms/subscribe.go:281
// visibleToActor), so an observer stream is the only vantage that can carry
// "what this tenant CANNOT see".
//
// Deliberately a TRANSPORT proof: the full visibility leak matrix stays at the
// DB tier (internal/comms/visibility_filter_test.go) per the frozen record
// (design.md:132). What is proven HERE, and nowhere else, is that the filter
// survives the real door — real bearers, real HTTP/2 server-streams, real
// cross-process seq ordering.
//
// podmanUsable-guarded with the byte-identical harness skip literal
// (harness_test.go:29) so the e2e CI guard's skip-string grep matches.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

// The two tenants, their agents, and the two channels. Handles obey the account
// handle grammar (store/handle.go:15 `^[a-z0-9][a-z0-9._-]*$`) and are t4-scoped
// so nothing collides with a sibling leg's fixture.
const (
	t4Owner1Handle = "t4-owner-1"
	t4Owner2Handle = "t4-owner-2"
	t4Agent1Handle = "t4-agent-1"
	t4Agent2Handle = "t4-agent-2"

	// t4GhostHandle names no account. Bare, so it resolves in the CALLER's own
	// owner namespace (comms/resolve.go:120 agentOwnerNamespace) and misses
	// there — the unknown-handle arm of the OpenDM indistinguishability pair.
	t4GhostHandle = "t4-ghost-agent"

	t4PrivateChannel = "t4-tenant-private"
	t4CanaryChannel  = "t4-tenant-canary"

	t4Topic       = "general"
	t4PrivateBody = "t4: owner-1 private traffic, never owner-2's"
	t4CanaryBody  = "t4: globally-visible canary owner-2 IS entitled to"
)

// TestCommsTenantVisibilityTransport is the three-assertion multi-tenant
// transport leg:
//
//  1. POSITIVE — owner-1's observer stream receives a post to owner-1's own
//     private channel on the LIVE tail (subscribed before the post).
//  2. NEGATIVE (load-bearing) — owner-2, subscribed at sinceSeq=0 AFTER the
//     private post, receives the globally-visible canary as its FIRST matching
//     event and never the private post.
//  3. Cross-owner OpenDM collapses to NOT_FOUND byte-identically to an unknown
//     handle — BOTH arms asserted, because indistinguishability is the contract.
//
// Every wait is event-gated and ctx-bounded (AwaitDelivery, comms_ops.go:132) —
// no sleeps, no polling, no retry loops.
func TestCommsTenantVisibilityTransport(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into NewFixture + every primitive

	f := NewFixture(ctx, t)

	// ---- setup: two owner tenants ----

	owner1ID, err := f.CreateUser(ctx, t4Owner1Handle, "T4 Owner One")
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", t4Owner1Handle, err)
	}
	owner2ID, err := f.CreateUser(ctx, t4Owner2Handle, "T4 Owner Two")
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", t4Owner2Handle, err)
	}

	// Observer bearers for both tenants. These are the credentials every
	// assertion below rides; the admin bearer would make all three vacuous.
	_, owner1Comms, err := f.AsObserver(ctx, owner1ID)
	if err != nil {
		t.Fatalf("AsObserver(owner-1 %s): %v", owner1ID, err)
	}
	_, owner2Comms, err := f.AsObserver(ctx, owner2ID)
	if err != nil {
		t.Fatalf("AsObserver(owner-2 %s): %v", owner2ID, err)
	}

	// One agent under EACH owner, never started (no Provision, no StartSession —
	// an account plus its home channel needs no container). f.CreateAgent
	// (agent_ops.go:22) cannot serve here: it rides the ADMIN bearer, and
	// CreateAgent creates the agent under the CALLER's resolved owner
	// (comms/comms.go:109 ResolveOwner), so it would put both agents under the
	// bootstrap admin and there would be no cross-owner pair to prove anything
	// about. So each agent is created over its OWN owner's observer client — the
	// same "call the generated client AsObserver returned" shape assertion 3
	// uses for OpenDM, not a new fixture primitive.
	agent1ID := createAgentAs(ctx, t, owner1Comms, t4Agent1Handle, "T4 Agent One")
	agent2ID := createAgentAs(ctx, t, owner2Comms, t4Agent2Handle, "T4 Agent Two")
	if agent1ID == agent2ID {
		t.Fatalf("the two owners' agents share account id %q; the per-owner agent namespaces are not distinct", agent1ID)
	}

	// owner-1's PRIVATE channel: ungrouped, so membership-only visibility
	// (fixture.go:302-307). owner-2 is not a member and cannot reach it through
	// any group arm.
	privateID, err := f.CreateChannel(ctx, owner1ID, t4PrivateChannel, true)
	if err != nil {
		t.Fatalf("CreateChannel(%s, private): %v", t4PrivateChannel, err)
	}
	// The canary channel: SHARED-grouped AND owner-2 is a founding member. Both
	// halves matter — a MessagePosted is gated on channel MEMBERSHIP
	// (subscribe.go:294 IsTopicChannelMember), so a merely globally-VISIBLE
	// channel owner-2 was not a member of would carry no message to it and the
	// high-water mark would never arrive.
	canaryID, err := f.CreateChannel(ctx, owner2ID, t4CanaryChannel, false)
	if err != nil {
		t.Fatalf("CreateChannel(%s, shared canary): %v", t4CanaryChannel, err)
	}
	if privateID == canaryID {
		t.Fatalf("CreateChannel returned the same id %q for the private and canary channels", privateID)
	}

	// ---- assertion 1: owner-1's stream receives its own private post (LIVE) ----

	// Subscribed BEFORE the post, so the event travels the live tail rather than
	// the replay snapshot: the two paths are filtered by separate loops
	// (forwardComms, subscribe.go:118), and a regression that filtered only
	// replay would ship green against a replay-only positive.
	owner1Live, err := f.SubscribeCommsAsObserver(ctx, owner1Comms, 0)
	if err != nil {
		t.Fatalf("SubscribeCommsAsObserver(owner-1, live): %v", err)
	}
	defer owner1Live.Close()

	privMsgID, err := f.PostMessageAsObserver(ctx, owner1Comms, privateID, t4Topic, t4PrivateBody)
	if err != nil {
		t.Fatalf("PostMessageAsObserver(owner-1 -> private channel %s): %v", privateID, err)
	}
	if privMsgID == "" {
		t.Fatal("PostMessageAsObserver(private) returned an empty message id")
	}

	gotPriv, err := f.AwaitDelivery(ctx, owner1Live, func(m *compassv1.Message) bool {
		return m.GetId() == privMsgID
	})
	if err != nil {
		t.Fatalf("owner-1's own stream never carried the private post %q (channel %s): %v",
			privMsgID, privateID, err)
	}
	if got := gotPriv.GetId(); got != privMsgID {
		t.Fatalf("owner-1 received message id %q, want the private post %q", got, privMsgID)
	}

	// ---- assertion 2: owner-2 never sees the private post; proven by canary ----

	// The vacuity guard for the negative below, and it must run FIRST. It opens a
	// SECOND owner-1 subscription at sinceSeq=0 AFTER the private post committed,
	// so the post can reach it only through the REPLAY snapshot — exactly the
	// path owner-2 is about to be denied on. An entitled account pulling it out
	// of replay proves the event is genuinely in the ring and genuinely
	// deliverable there, so owner-2's absence below is the visibility FILTER at
	// work and not the post being unreachable via replay at all. Without this,
	// a bug that dropped the private post from the ring entirely would satisfy
	// the negative for the wrong reason.
	owner1Replay, err := f.SubscribeCommsAsObserver(ctx, owner1Comms, 0)
	if err != nil {
		t.Fatalf("SubscribeCommsAsObserver(owner-1, replay): %v", err)
	}
	defer owner1Replay.Close()
	replayed, err := f.AwaitDelivery(ctx, owner1Replay, func(m *compassv1.Message) bool {
		return m.GetId() == privMsgID
	})
	if err != nil {
		t.Fatalf("owner-1's post-hoc sinceSeq=0 subscription did not replay the private post %q: %v — the negative below would be vacuous (the event is not in the replay window at all)",
			privMsgID, err)
	}
	if got := replayed.GetId(); got != privMsgID {
		t.Fatalf("owner-1 replay delivered message id %q, want the private post %q", got, privMsgID)
	}

	// owner-2 subscribes at sinceSeq=0 AFTER the private post, so that post is in
	// the replay window it is about to be served: the filter has to actively drop
	// it. The leading control frame is drained synchronously, which is a
	// SERVER-GUARANTEED happens-before — a sinceSeq=0 subscribe sends the
	// snapshot-boundary frame from the handler AFTER bus.Subscribe registered the
	// subscriber (subscribe.go:36-80), so once that frame is in hand owner-2's
	// stream is provably live and the canary post below cannot be published to
	// zero subscribers.
	owner2Stream, err := f.SubscribeCommsAsObserver(ctx, owner2Comms, 0)
	if err != nil {
		t.Fatalf("SubscribeCommsAsObserver(owner-2): %v", err)
	}
	defer owner2Stream.Close()
	awaitSubscriptionLive(t, owner2Stream)

	// The high-water mark: a post owner-2 IS entitled to, published strictly
	// after its subscription went live. Posted by the admin, a founding member of
	// the canary channel by construction (fixture.go:317).
	canaryMsgID, err := f.PostMessage(ctx, canaryID, t4Topic, t4CanaryBody)
	if err != nil {
		t.Fatalf("PostMessage(canary channel %s): %v", canaryID, err)
	}
	if canaryMsgID == privMsgID {
		t.Fatalf("the canary post and the private post share message id %q; the negative cannot discriminate them", canaryMsgID)
	}

	// THE negative. The match admits EITHER message, so AwaitDelivery returns
	// whichever fans FIRST — it does not skip past a leak. In-order
	// per-subscriber delivery (replay snapshot drained oldest-first, then the
	// live tail — forwardComms, subscribe.go:118) means a leaked private post
	// would necessarily arrive AHEAD of the canary: it is in the replay window,
	// the canary is on the live tail behind it. So "the canary came first" is a
	// positive ordering fact, not a wall-clock bet. NO sleep and NO timeout
	// proves this absence — the assertion settles on an event that DID arrive.
	first, err := f.AwaitDelivery(ctx, owner2Stream, func(m *compassv1.Message) bool {
		return m.GetId() == privMsgID || m.GetId() == canaryMsgID
	})
	if err != nil {
		t.Fatalf("owner-2's stream carried neither the canary %q nor the private post %q: %v — the high-water mark never arrived, so the negative is unproven",
			canaryMsgID, privMsgID, err)
	}
	if got := first.GetId(); got == privMsgID {
		t.Fatalf("LEAK: owner-2's first matching event is message %q — the private post in owner-1's channel %s; want the canary %q in channel %s. Cross-tenant visibility filtering is not holding over the real door",
			got, privateID, canaryMsgID, canaryID)
	}
	if got := first.GetId(); got != canaryMsgID {
		t.Fatalf("owner-2's first matching event = message %q, want the canary %q (channel %s)", got, canaryMsgID, canaryID)
	}

	// ---- assertion 3: cross-owner OpenDM == unknown OpenDM == NOT_FOUND ----

	// From owner-1's AGENT vantage, mirroring the DB-tier contract
	// (dm_open_pgtest_test.go:102 TestOpenDMCrossOwnerIsIndistinguishableNotFound):
	// OpenDM's same-owner check bites an owner-QUALIFIED handle naming another
	// owner's agent (comms/comms.go:683-685) and remaps it to the NOT_FOUND an
	// unknown handle gets. Called directly on the generated client AsObserver
	// returns — no fixture wrapper.
	_, agent1Comms, err := f.AsObserver(ctx, agent1ID)
	if err != nil {
		t.Fatalf("AsObserver(owner-1's agent %s): %v", agent1ID, err)
	}

	crossOwnerPeer := t4Owner2Handle + "/" + t4Agent2Handle
	crossCode := openDMCode(ctx, t, agent1Comms, crossOwnerPeer)
	if crossCode != connect.CodeNotFound {
		t.Fatalf("OpenDM(cross-owner peer %q) = %v, want %v (a foreign owner's agent must never be reachable)",
			crossOwnerPeer, crossCode, connect.CodeNotFound)
	}

	// The second arm is what makes the first one mean anything: asserting only
	// the cross-owner code would pass even if an unknown handle returned
	// something else, and the contract is that the two are INDISTINGUISHABLE —
	// a caller must not be able to probe a foreign peer's existence by
	// comparing codes.
	unknownCode := openDMCode(ctx, t, agent1Comms, t4GhostHandle)
	if unknownCode != connect.CodeNotFound {
		t.Fatalf("OpenDM(unknown peer %q) = %v, want %v", t4GhostHandle, unknownCode, connect.CodeNotFound)
	}
	if crossCode != unknownCode {
		t.Fatalf("OpenDM cross-owner (%q) = %v but unknown (%q) = %v; the two must be indistinguishable so a foreign peer's existence never leaks",
			crossOwnerPeer, crossCode, t4GhostHandle, unknownCode)
	}
}

// createAgentAs creates an agent over an EXPLICIT comms client (an observer's),
// so the agent is owned by that client's account rather than by the fixture's
// bootstrap admin — CreateAgent places the new agent under the CALLER's resolved
// owner (comms/comms.go:109). Fatal on failure: a setup miss makes every
// assertion below meaningless. The per-call deadline is derived from ctx, the
// same shape every fixture RPC uses.
func createAgentAs(ctx context.Context, t *testing.T, comms commsServiceClient, handle, displayName string) string {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := comms.CreateAgent(rctx, connect.NewRequest(&compassv1.CreateAgentRequest{
		Handle:      handle,
		DisplayName: displayName,
	}))
	if err != nil {
		t.Fatalf("CreateAgent(%s) as the owning tenant: %v", handle, err)
	}
	id := resp.Msg.GetAccount().GetId()
	if id == "" {
		t.Fatalf("CreateAgent(%s) returned an empty account id", handle)
	}
	return id
}

// awaitSubscriptionLive drains the leading control frame a sinceSeq=0
// SubscribeComms sends and asserts it is one — the snapshot-boundary frame
// (subscribe.go:226 commsSnapshotBoundary) carries no payload. It is a
// happens-before gate, not a content assertion: the handler sends it only after
// bus.Subscribe has registered the subscriber, so returning from here means the
// stream is provably live server-side and a post published afterwards cannot be
// missed. That is what lets the canary ordering below stand on a server
// guarantee rather than on a race the client hopes to win.
func awaitSubscriptionLive(t *testing.T, stream *connect.ServerStreamForClient[compassv1.SubscribeCommsResponse]) {
	t.Helper()
	if !stream.Receive() {
		t.Fatalf("subscription ended before its leading control frame: %v", stream.Err())
	}
	if payload := stream.Msg().GetPayload(); payload != nil {
		t.Fatalf("first frame of a sinceSeq=0 subscription carries payload %T, want the payload-free snapshot-boundary control frame", payload)
	}
}

// openDMCode calls OpenDM for peerHandle on comms and returns the connect code
// of the result, failing the test if the call unexpectedly SUCCEEDED — a
// successful open is not a code and must not be silently reported as
// CodeUnknown, which would let a real cross-tenant DM pass the code comparison.
func openDMCode(ctx context.Context, t *testing.T, comms commsServiceClient, peerHandle string) connect.Code {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := comms.OpenDM(rctx, connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: peerHandle}))
	if err == nil {
		t.Fatalf("OpenDM(peer %q) succeeded and opened channel %q, want a NOT_FOUND rejection",
			peerHandle, resp.Msg.GetChannel().GetId())
	}
	return connect.CodeOf(err)
}
