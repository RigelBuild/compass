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
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

// The two tenants, their agents, and the two channels. Handles obey the account
// handle grammar (store/handle.go:15 `^[a-z0-9][a-z0-9._-]*$`) and are t4-scoped
// so they stay mutually distinct WITHIN this leg and name their subject in a
// failure message. Cross-leg isolation is not theirs to provide: each test gets
// its own NewFixture, hence its own stack, cluster and state dir.
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
//     event, with the private post never delivered AHEAD of it. Scoped to
//     MessagePosted; the full payload-arm matrix is the DB tier's.
//  3. Cross-owner OpenDM collapses to the SAME rejection an unknown handle gets
//     — same code AND, once the submitted handle is redacted, the same message.
//     Both arms asserted, because indistinguishability is the contract.
//
// Every wait is event-gated and ctx-bounded (AwaitDelivery, comms_ops.go:132;
// awaitSubscriptionLive below) — no sleeps, no polling, no retry loops.
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
	agent1ID, agent1Owner := createAgentAs(ctx, t, owner1Comms, t4Agent1Handle, "T4 Agent One")
	agent2ID, agent2Owner := createAgentAs(ctx, t, owner2Comms, t4Agent2Handle, "T4 Agent Two")
	if agent1ID == agent2ID {
		t.Fatalf("the two owners' agents share account id %q; the per-owner agent namespaces are not distinct", agent1ID)
	}
	// The load-bearing precondition for assertion 3, checked rather than assumed.
	// Distinct account ids hold for any two accounts, INCLUDING two agents under
	// one owner — so id-distinctness alone would let assertion 3's cross-owner arm
	// silently degrade into a second copy of its unknown-handle arm (both return
	// NOT_FOUND) and stay green while the cross-owner authz branch
	// (comms/comms.go:683) never executed. Assert the placement the arm depends on.
	if agent1Owner != owner1ID {
		t.Fatalf("agent %s landed under owner %q, want owner-1 %q; the cross-owner arm would not be cross-owner", t4Agent1Handle, agent1Owner, owner1ID)
	}
	if agent2Owner != owner2ID {
		t.Fatalf("agent %s landed under owner %q, want owner-2 %q; the cross-owner arm would not be cross-owner", t4Agent2Handle, agent2Owner, owner2ID)
	}

	// owner-1's PRIVATE channel: ungrouped, so membership-only visibility
	// (fixture.go:302-307). owner-2 is not a member and cannot reach it through
	// any group arm.
	privateID, err := f.CreateChannel(ctx, owner1ID, t4PrivateChannel, true)
	if err != nil {
		t.Fatalf("CreateChannel(%s, private): %v", t4PrivateChannel, err)
	}
	// The canary channel. What carries the canary to owner-2 is its founding
	// MEMBERSHIP: a MessagePosted is gated on channel membership
	// (subscribe.go:294 IsTopicChannelMember), and CreateChannel threads the
	// owner into MemberHandles (fixture.go:342). The SHARED grouping is
	// incidental to delivery here — membership alone suffices — and is kept only
	// to mirror T1's shared-channel surface.
	canaryID, err := f.CreateChannel(ctx, owner2ID, t4CanaryChannel, false)
	if err != nil {
		t.Fatalf("CreateChannel(%s, shared canary): %v", t4CanaryChannel, err)
	}
	if privateID == canaryID {
		t.Fatalf("CreateChannel returned the same id %q for the private and canary channels", privateID)
	}

	// ---- assertion 1: owner-1's stream receives its own private post (LIVE) ----

	// Subscribed BEFORE the post AND gated live, so the event travels the live
	// tail rather than the replay snapshot: the two paths are filtered by
	// separate loops (forwardComms, subscribe.go:118), and a regression that
	// filtered only replay would ship green against a replay-only positive.
	//
	// The gate is what makes that true. SubscribeCommsAsObserver returns as soon
	// as the request is sent — connect's CallServerStream does not wait for a
	// response frame — so returning from it establishes NOTHING about the server
	// having registered the subscriber. Without draining the snapshot-boundary
	// frame the post below could commit first and be served from the replay
	// snapshot, satisfying this assertion while proving the weaker property.
	owner1Live, err := f.SubscribeCommsAsObserver(ctx, owner1Comms, 0)
	if err != nil {
		t.Fatalf("SubscribeCommsAsObserver(owner-1, live): %v", err)
	}
	defer owner1Live.Close()
	awaitSubscriptionLive(ctx, t, owner1Live)

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
	awaitSubscriptionLive(ctx, t, owner2Stream)

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
	crossCode, crossMsg := openDMRejection(ctx, t, agent1Comms, crossOwnerPeer)
	if crossCode != connect.CodeNotFound {
		t.Fatalf("OpenDM(cross-owner peer %q) = %v, want %v (a foreign owner's agent must never be reachable)",
			crossOwnerPeer, crossCode, connect.CodeNotFound)
	}

	// The second arm is what makes the first one mean anything: asserting only
	// the cross-owner code would pass even if an unknown handle returned
	// something else, and the contract is that the two are INDISTINGUISHABLE —
	// a caller must not be able to probe a foreign peer's existence by
	// comparing rejections.
	unknownCode, unknownMsg := openDMRejection(ctx, t, agent1Comms, t4GhostHandle)
	if unknownCode != connect.CodeNotFound {
		t.Fatalf("OpenDM(unknown peer %q) = %v, want %v", t4GhostHandle, unknownCode, connect.CodeNotFound)
	}
	if crossCode != unknownCode {
		t.Fatalf("OpenDM cross-owner (%q) = %v but unknown (%q) = %v; the two must be indistinguishable so a foreign peer's existence never leaks",
			crossOwnerPeer, crossCode, t4GhostHandle, unknownCode)
	}

	// The MESSAGE axis, which the codes cannot cover. Both arms reach NOT_FOUND
	// through DIFFERENT branches — cross-owner through OpenDM's same-owner check
	// (comms.go:683), unknown through the resolver miss (resolve.go:86-90) — and
	// they share a code only by funnelling into the same edgeError arm
	// (context.go:58). What actually closes the oracle is notFoundHandle
	// re-keying both to the SUBMITTED handle (resolve.go:137). So a
	// regression that drops that re-key on either arm — leaking a resolved owner
	// id, the store's own handle spelling, or a "different owner" phrase — keeps
	// both codes NOT_FOUND and is invisible to a code compare. Normalize away the
	// submitted handle, the one field that legitimately differs, then require the
	// remainder to match exactly.
	//
	// Comparing the full remainder is deliberate: a leak's shape cannot be
	// enumerated in advance, so anything weaker (a prefix, or just asserting the
	// owner id is absent) reopens the hole. The coupling is to one shared
	// template, notFoundHandle at resolve.go:137 — reword that and update here.
	crossRedacted := strings.ReplaceAll(crossMsg, crossOwnerPeer, "<peer>")
	unknownRedacted := strings.ReplaceAll(unknownMsg, t4GhostHandle, "<peer>")
	if crossRedacted != unknownRedacted {
		t.Fatalf("OpenDM rejection messages are distinguishable once the submitted handle is redacted:\n cross-owner (%q): %q\n unknown     (%q): %q\nthe two must be byte-identical or a caller can probe a foreign peer's existence by comparing messages (shared template: notFoundHandle, resolve.go:137)",
			crossOwnerPeer, crossRedacted, t4GhostHandle, unknownRedacted)
	}
}

// createAgentAs creates an agent over an EXPLICIT comms client (an observer's),
// so the agent is owned by that client's account rather than by the fixture's
// bootstrap admin — CreateAgent places the new agent under the CALLER's resolved
// owner (comms/comms.go:109). Returns the new account id AND the owner the
// server actually resolved, because per-owner ownership is an emergent property
// of which client was passed: nothing in the request names an owner, so the
// caller cannot assume it landed where intended and must check. Fatal on
// failure: a setup miss makes every assertion below meaningless.
func createAgentAs(ctx context.Context, t *testing.T, comms commsServiceClient, handle, displayName string) (accountID, ownerID string) {
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
	account := resp.Msg.GetAccount()
	id := account.GetId()
	if id == "" {
		t.Fatalf("CreateAgent(%s) returned an empty account id", handle)
	}
	// mapping.go:33 populates OwnerUserId from the stored agent, so this is the
	// owner the server resolved for the caller — not an echo of the request.
	owner := account.GetAgent().GetOwnerUserId()
	if owner == "" {
		t.Fatalf("CreateAgent(%s) returned account %q with no agent owner id; cannot verify which tenant it landed under", handle, id)
	}
	return id, owner
}

// awaitSubscriptionLive drains the leading control frame a sinceSeq=0
// SubscribeComms sends and asserts it is one — the snapshot-boundary frame
// (subscribe.go:226 commsSnapshotBoundary) carries no payload. It is a
// happens-before gate, not a content assertion: the handler sends it only after
// bus.Subscribe has registered the subscriber, so returning from here means the
// stream is provably live server-side and a post published afterwards cannot be
// missed. That is what lets the canary ordering below stand on a server
// guarantee rather than on a race the client hopes to win.
//
// Receive is a blocking network read and the stream rides the test-root ctx,
// which has no deadline, so the read is pumped in a goroutine raced against a
// derived one — the shape AwaitDelivery uses (comms_ops.go:132) and for the
// same reason: a server that registers the subscriber but never sends the
// boundary frame fails legibly here instead of blocking to the go-test timeout.
func awaitSubscriptionLive(ctx context.Context, t *testing.T, stream *connect.ServerStreamForClient[compassv1.SubscribeCommsResponse]) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	type frame struct {
		payload any
		err     error
	}
	// Buffered: the pump sends once and exits, so it cannot block after this
	// helper has already returned on the ctx deadline.
	out := make(chan frame, 1)
	go func() {
		if !stream.Receive() {
			out <- frame{err: fmt.Errorf("subscription ended before its leading control frame: %w", stream.Err())}
			return
		}
		out <- frame{payload: stream.Msg().GetPayload()}
	}()

	select {
	case f := <-out:
		if f.err != nil {
			t.Fatalf("%v", f.err)
		}
		if f.payload != nil {
			t.Fatalf("first frame of a sinceSeq=0 subscription carries payload %T, want the payload-free snapshot-boundary control frame", f.payload)
		}
	case <-ctx.Done():
		t.Fatalf("awaiting the snapshot-boundary frame: %v — the subscription never went live", ctx.Err())
	}
}

// openDMRejection calls OpenDM for peerHandle on comms and returns BOTH the
// connect code and the wire message of the rejection, failing the test if the
// call unexpectedly SUCCEEDED — a successful open is not a rejection and must
// not be silently reported as CodeUnknown, which would let a real cross-tenant
// DM pass the comparison below.
//
// The message matters as much as the code: the oracle the two notFoundHandle
// call sites exist to close is a MESSAGE oracle (comms.go:677 "never leaking
// the peer's existence"), and both arms already share the code by arriving at
// the same edgeError branch. Comparing codes alone cannot see a regression that
// leaks the peer's existence through the message text.
func openDMRejection(ctx context.Context, t *testing.T, comms commsServiceClient, peerHandle string) (connect.Code, string) {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := comms.OpenDM(rctx, connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: peerHandle}))
	if err == nil {
		t.Fatalf("OpenDM(peer %q) succeeded and opened channel %q, want a NOT_FOUND rejection",
			peerHandle, resp.Msg.GetChannel().GetId())
	}
	return connect.CodeOf(err), rejectionMessage(t, peerHandle, err)
}

// rejectionMessage returns a connect error's message with its code prefix
// stripped (connect.Error.Message, unlike Error, omits it), so two rejections
// can be compared on text alone.
//
// A non-connect error is FATAL rather than degraded to err.Error(), because a
// degraded return would be worse than useless: two arms failing identically
// yield two equal strings containing neither submitted handle, the redaction
// removes nothing, and the message compare PASSES having observed no rejection.
// The branch should be unreachable — connect codes every client-side error
// (wrapIfUncoded, connect@v1.20.0/error.go:279-287, falling back to
// CodeUnknown) — so it fires only if that guarantee changes.
//
// Transport faults are NOT caught here; they arrive already coded
// (CodeUnavailable, CodeDeadlineExceeded, CodeInternal) and so pass errors.As.
// What stops them is the absolute NOT_FOUND assertion at each call site, which
// fatals before the message compare runs. Do not weaken those to a bare
// cross-vs-unknown code compare: two identical transport faults would satisfy
// it, and this helper would not save you.
func rejectionMessage(t *testing.T, peerHandle string, err error) string {
	t.Helper()
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("OpenDM(peer %q) failed with an uncoded error %T (%v); connect is expected to code every client error, and without a connect message the comparison below would be vacuous",
			peerHandle, err, err)
	}
	return cerr.Message()
}
