//go:build podman

package e2e

// TestCommsTenantVisibilityTransport is a real-door visibility proof with two
// owner-scoped observer bearers and no container agents. Admin credentials can
// see all tenants, so only observer streams can prove what another owner cannot
// see. The DB tier covers the full filter matrix; this leg proves it over TLS.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
)

// Two tenant owners, their agents, and channels are scoped to this test's data.
// Handles follow the account handle grammar.
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

// TestCommsTenantVisibilityTransport proves the owner's live stream sees its
// private post, another owner's sinceSeq=0 stream sees an entitled canary first,
// and cross-owner OpenDM is indistinguishable from an unknown handle.
// All waits are event-gated and bounded by ctx.
func TestCommsTenantVisibilityTransport(t *testing.T) {
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into every primitive

	f := sharedFixture(t)

	// ---- setup: two owner tenants ----

	owner1ID, owner2ID, owner1Comms, owner2Comms := t4SetupTenants(ctx, t, f)

	// Owner-1's ungrouped channel is private and member-only.
	privateID, err := f.CreateChannel(ctx, owner1ID, t4PrivateChannel, true)
	if err != nil {
		t.Fatalf("CreateChannel(%s, private): %v", t4PrivateChannel, err)
	}
	// The canary's owner is a founding member, so membership enables delivery;
	// shared grouping is incidental and mirrors the shared-channel case.
	canaryID, err := f.CreateChannel(ctx, owner2ID, t4CanaryChannel, false)
	if err != nil {
		t.Fatalf("CreateChannel(%s, shared canary): %v", t4CanaryChannel, err)
	}
	if privateID == canaryID {
		t.Fatalf("CreateChannel returned the same id %q for the private and canary channels", privateID)
	}

	// Subscribe before posting and drain the snapshot-boundary frame. The server
	// sends that frame only after registering the stream, proving the private post
	// arrives on the live tail rather than through replay.
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

	// Prove the private post is replayable before using it as the negative case:
	// owner-1 subscribes at sinceSeq=0 after the post and must receive it. Otherwise
	// owner-2's later absence could mean the post was never in the replay window.
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

	// Owner-2 subscribes after the private post, so it is in the replay window.
	// Draining the boundary frame proves the subscription is registered before the
	// canary is posted; the filter must actively suppress the private event.
	owner2Stream, err := f.SubscribeCommsAsObserver(ctx, owner2Comms, 0)
	if err != nil {
		t.Fatalf("SubscribeCommsAsObserver(owner-2): %v", err)
	}
	defer owner2Stream.Close()
	awaitSubscriptionLive(ctx, t, owner2Stream)

	// Post an entitled canary after owner-2's stream is live; this is the positive
	// high-water mark for the ordering assertion.
	canaryMsgID, err := f.PostMessage(ctx, canaryID, t4Topic, t4CanaryBody)
	if err != nil {
		t.Fatalf("PostMessage(canary channel %s): %v", canaryID, err)
	}
	if canaryMsgID == privMsgID {
		t.Fatalf("the canary post and the private post share message id %q; the negative cannot discriminate them", canaryMsgID)
	}

	// Await either the private post or canary. The private post is in replay and the
	// canary follows on the live tail, so a leak necessarily arrives first. Canary
	// first is a positive event-ordering proof; an absence timeout proves nothing.
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

	// From owner-1's agent vantage, OpenDM must map a foreign owner's agent to the
	// same NOT_FOUND as an unknown handle, without leaking the peer's existence.
	agent1Handle := t4Owner1Handle + "/" + t4Agent1Handle
	_, agent1Comms, err := f.AsObserver(ctx, agent1Handle)
	if err != nil {
		t.Fatalf("AsObserver(owner-1's agent %s): %v", agent1Handle, err)
	}

	assertTenantOpenDMIndistinguishable(t, ctx, agent1Comms)
}

// createAgentAs creates an agent under the caller's resolved owner. The request names no
// owner, so it returns the owner the server resolved for the caller to check, plus the home channel.
func createAgentAs(ctx context.Context, t *testing.T, comms commsServiceClient, handle, displayName string) (accountID, ownerID, homeChannelID string) {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := comms.CreateAgent(rctx, &compassv1.CreateAgentRequest{
		Handle:      handle,
		DisplayName: displayName,
		Role:        "manager",
	})
	if err != nil {
		t.Fatalf("CreateAgent(%s) as the owning tenant: %v", handle, err)
	}
	account := resp.GetAccount()
	id := account.GetId()
	if id == "" {
		t.Fatalf("CreateAgent(%s) returned an empty account id", handle)
	}
	owner := account.GetAgent().GetOwnerUserId()
	// OwnerUserId comes from the stored agent, so it verifies the server-resolved
	// tenant rather than echoing request data.
	if owner == "" {
		t.Fatalf("CreateAgent(%s) returned account %q with no agent owner id; cannot verify which tenant it landed under", handle, id)
	}
	return id, owner, account.GetAgent().GetHomeChannelId()
}

// awaitSubscriptionLive drains the sinceSeq=0 snapshot-boundary control frame.
// The server sends it after registering the subscriber, giving the canary test a
// happens-before guarantee. Receive is raced against a deadline because the
// stream otherwise has no ctx deadline.
func awaitSubscriptionLive(ctx context.Context, t *testing.T, stream compassv1connect.CommsServiceSubscribeCommsClientStream) {
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
		got, err := stream.Receive()
		if err != nil {
			out <- frame{err: fmt.Errorf("subscription ended before its leading control frame: %w", err)}
			return
		}
		out <- frame{payload: got.GetPayload()}
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

// openDMRejection returns the code and message, failing if OpenDM unexpectedly
// succeeds. The message is compared too because equal codes alone could still
// leak whether the peer exists.
func openDMRejection(ctx context.Context, t *testing.T, comms commsServiceClient, peerHandle string) (connect.Code, string) {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := comms.OpenDM(rctx, &compassv1.OpenDMRequest{PeerHandle: peerHandle})
	if err == nil {
		t.Fatalf("OpenDM(peer %q) succeeded and opened channel %q, want a NOT_FOUND rejection",
			peerHandle, resp.GetChannel().GetId())
	}
	return connect.CodeOf(err), rejectionMessage(t, peerHandle, err)
}

// rejectionMessage returns the coded error's message without its code prefix.
// An uncoded transport error is fatal: comparing two such errors could pass
// without observing either rejection. Transport failures are excluded by the
// callers' absolute NOT_FOUND assertions.
func rejectionMessage(t *testing.T, peerHandle string, err error) string {
	t.Helper()
	cerr, ok := errors.AsType[*connect.Error](err)
	if !ok {
		t.Fatalf("OpenDM(peer %q) failed with an uncoded error %T (%v); connect is expected to code every client error, and without a connect message the comparison below would be vacuous",
			peerHandle, err, err)
	}
	return cerr.Message()
}

func assertTenantOpenDMIndistinguishable(t *testing.T, ctx context.Context, agent1Comms commsServiceClient) {
	t.Helper()
	crossOwnerPeer := t4Owner2Handle + "/" + t4Agent2Handle
	crossCode, crossMsg := openDMRejection(ctx, t, agent1Comms, crossOwnerPeer)
	if crossCode != connect.CodeNotFound {
		t.Fatalf("OpenDM(cross-owner peer %q) = %v, want %v (a foreign owner's agent must never be reachable)",
			crossOwnerPeer, crossCode, connect.CodeNotFound)
	}

	// The unknown-handle arm proves the cross-owner rejection is indistinguishable;
	// checking only the first arm would not pin that contract.
	unknownCode, unknownMsg := openDMRejection(ctx, t, agent1Comms, t4GhostHandle)
	if unknownCode != connect.CodeNotFound {
		t.Fatalf("OpenDM(unknown peer %q) = %v, want %v", t4GhostHandle, unknownCode, connect.CodeNotFound)
	}
	if crossCode != unknownCode {
		t.Fatalf("OpenDM cross-owner (%q) = %v but unknown (%q) = %v; the two must be indistinguishable so a foreign peer's existence never leaks",
			crossOwnerPeer, crossCode, t4GhostHandle, unknownCode)
	}

	// Compare messages after redacting only the submitted handle. Cross-owner and
	// unknown handles take different branches, so matching codes cannot detect a
	// message oracle. Exact remainder equality keeps any leaked owner or handle
	// detail visible.
	crossRedacted := strings.ReplaceAll(crossMsg, crossOwnerPeer, "<peer>")
	unknownRedacted := strings.ReplaceAll(unknownMsg, t4GhostHandle, "<peer>")
	if crossRedacted != unknownRedacted {
		t.Fatalf("OpenDM rejection messages are distinguishable once the submitted handle is redacted:\n cross-owner (%q): %q\n unknown     (%q): %q\nthe two must be byte-identical or a caller can probe a foreign peer's existence by comparing messages (shared template: notFoundHandle, resolve.go:137)",
			crossOwnerPeer, crossRedacted, t4GhostHandle, unknownRedacted)
	}
}

// t4SetupTenants creates the two owner tenants and one never-started agent under each.
func t4SetupTenants(ctx context.Context, t *testing.T, f *Fixture) (owner1ID, owner2ID string, owner1Comms, owner2Comms commsServiceClient) {
	t.Helper()
	var err error
	owner1ID, err = f.CreateUser(ctx, t4Owner1Handle, "T4 Owner One")
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", t4Owner1Handle, err)
	}
	owner2ID, err = f.CreateUser(ctx, t4Owner2Handle, "T4 Owner Two")
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", t4Owner2Handle, err)
	}

	// Both tenant bearers are non-admin; admin credentials would make visibility
	// assertions vacuous.
	_, owner1Comms, err = f.AsObserver(ctx, t4Owner1Handle)
	if err != nil {
		t.Fatalf("AsObserver(owner-1 %s): %v", t4Owner1Handle, err)
	}
	_, owner2Comms, err = f.AsObserver(ctx, t4Owner2Handle)
	if err != nil {
		t.Fatalf("AsObserver(owner-2 %s): %v", t4Owner2Handle, err)
	}

	// Create one unstarted agent through each owner's observer client. The admin
	// fixture helper would place both under the bootstrap admin, eliminating the
	// cross-owner pair. Verify each stored owner below.
	agent1ID, agent1Owner, _ := createAgentAs(ctx, t, owner1Comms, t4Agent1Handle, "T4 Agent One")
	agent2ID, agent2Owner, _ := createAgentAs(ctx, t, owner2Comms, t4Agent2Handle, "T4 Agent Two")
	if agent1ID == agent2ID {
		t.Fatalf("the two owners' agents share account id %q; the per-owner agent namespaces are not distinct", agent1ID)
	}
	// Distinct account ids do not prove distinct owners. Check placement explicitly
	// so the cross-owner OpenDM arm cannot silently become an unknown-handle test.
	if agent1Owner != owner1ID {
		t.Fatalf("agent %s landed under owner %q, want owner-1 %q; the cross-owner arm would not be cross-owner", t4Agent1Handle, agent1Owner, owner1ID)
	}
	if agent2Owner != owner2ID {
		t.Fatalf("agent %s landed under owner %q, want owner-2 %q; the cross-owner arm would not be cross-owner", t4Agent2Handle, agent2Owner, owner2ID)
	}
	return owner1ID, owner2ID, owner1Comms, owner2Comms
}
