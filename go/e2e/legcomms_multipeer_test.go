//go:build podman

package e2e

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
)

// Namespaced: this leg shares one stack with every other shared-fixture leg.
const (
	mpChannel      = "mp-room"
	mpTopic        = "mp-topic"
	mpMemberHandle = "mp-member"
	mpMarker       = "mp-multipeer-drive"
)

// Every agent turn in this leg carries mpMarker, so none draws the positional script.
const (
	mpMentionBody  = "@mp-a please steer " + mpMarker
	mpRejectedBody = "mp non-owner post " + mpMarker
	mpCanaryBody   = "mp owner canary " + mpMarker
)

var mpAgentHandles = []string{"mp-a", "mp-b", "mp-c"}

func registerMultiPeerFixtureOptions() {
	registerSharedFixtureOption(WithCannedMarkerReply(mpMarker, "mp turn settled"))
}

// TestCommsMultiPeerOwnerOnly drives N=3 live sessions on one OWNER_ONLY channel:
// the owner's @mention steers the mentioned agent and delivers to the two other
// subscribers, and a non-owner member's post is refused without fanning out.
func TestCommsMultiPeerOwnerOnly(t *testing.T) {
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}
	ctx := context.Background()
	f := sharedFixture(t)

	sessions := make(map[string]string, len(mpAgentHandles))
	var firstID string
	for _, h := range mpAgentHandles {
		id, err := f.CreateAgent(ctx, h, "Multi-peer "+h)
		if err != nil {
			t.Fatalf("CreateAgent(%s): %v", h, err)
		}
		if firstID == "" {
			firstID = id
		}
		container, err := f.Provision(ctx, id, h+"-provision")
		if err != nil {
			t.Fatalf("Provision(%s): %v", h, err)
		}
		t.Cleanup(func() { _ = f.RemoveWorkspace(ctx, container, h+"-teardown") })
		if sessions[h], err = f.StartSession(ctx, container); err != nil {
			t.Fatalf("StartSession(%s): %v", h, err)
		}
	}
	if _, err := f.CreateUser(ctx, mpMemberHandle, "Multi-peer member"); err != nil {
		t.Fatalf("CreateUser(%s): %v", mpMemberHandle, err)
	}

	// Private (ungrouped) channel: only members see it, so the admin stream below
	// is the sole observer and a leaked refusal would surface there.
	chID, err := f.CreateChannel(ctx, firstID, mpChannel, true)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	for _, h := range append(append([]string{}, mpAgentHandles...), mpMemberHandle) {
		if err := f.SubscribeMember(ctx, chID, h); err != nil {
			t.Fatalf("SubscribeMember(%s): %v", h, err)
		}
	}
	mpSetOwnerOnly(ctx, t, f, chID)

	mpAssertSteerDeliverSplit(ctx, t, f, sessions, chID)
	mpAssertNonOwnerRefused(ctx, t, f, chID)
}

// mpSetOwnerOnly makes the admin the channel owner under OWNER_ONLY and checks the echo.
func mpSetOwnerOnly(ctx context.Context, t *testing.T, f *Fixture, chID string) {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Comms().SetChannelPolicy(rctx, &compassv1.SetChannelPolicyRequest{
		ChannelId:   chID,
		PostPolicy:  compassv1.ChannelPostPolicy_CHANNEL_POST_POLICY_OWNER_ONLY,
		OwnerHandle: bootstrapAdminHandle,
	})
	if err != nil {
		t.Fatalf("SetChannelPolicy(OWNER_ONLY): %v", err)
	}
	ch := resp.GetChannel()
	if ch.GetPostPolicy() != compassv1.ChannelPostPolicy_CHANNEL_POST_POLICY_OWNER_ONLY {
		t.Fatalf("post_policy = %v, want OWNER_ONLY", ch.GetPostPolicy())
	}
	admin, err := f.lookupAccount(ctx, bootstrapAdminHandle)
	if err != nil {
		t.Fatalf("lookupAccount(admin): %v", err)
	}
	if ch.GetOwnerAccountId() != admin.GetId() {
		t.Fatalf("owner_account_id = %q, want the admin %q", ch.GetOwnerAccountId(), admin.GetId())
	}
}

// mpAssertSteerDeliverSplit posts one owner @mention and requires the first
// injection for it to be STEER on the mentioned session and DELIVER on the others.
// The agent dedups by message id before emitting a frame, so a second op is never
// visible here; steer-only precedence itself is pinned by the delivery unit tests.
func mpAssertSteerDeliverSplit(ctx context.Context, t *testing.T, f *Fixture, sessions map[string]string, chID string) {
	t.Helper()
	tails := make(map[string]compassv1connect.CompassServiceSubscribeAgentSessionClientStream, len(sessions))
	for _, h := range mpAgentHandles {
		tail, err := f.OpenSessionTail(ctx, sessions[h])
		if err != nil {
			t.Fatalf("OpenSessionTail(%s): %v", h, err)
		}
		defer tail.Close()
		tails[h] = tail
	}

	msgID, err := f.PostMessage(ctx, chID, mpTopic, mpMentionBody)
	if err != nil {
		t.Fatalf("PostMessage(@mention): %v", err)
	}
	for _, h := range mpAgentHandles {
		want := "DELIVER"
		if h == "mp-a" {
			want = "STEER"
		}
		got, err := f.AwaitControlDispatchOn(ctx, tails[h], func(_, id string) bool { return id == msgID })
		if err != nil {
			t.Fatalf("%s: awaiting the injection for %s: %v", h, msgID, err)
		}
		if !strings.Contains(got, want) {
			t.Fatalf("%s first injection for %s = %q, want %s", h, msgID, got, want)
		}
	}
}

// mpAssertNonOwnerRefused checks that a member's post is PERMISSION_DENIED and
// never fans: the owner's later canary must reach the admin stream first.
func mpAssertNonOwnerRefused(ctx context.Context, t *testing.T, f *Fixture, chID string) {
	t.Helper()
	live, err := f.SubscribeComms(ctx, 0)
	if err != nil {
		t.Fatalf("SubscribeComms: %v", err)
	}
	defer live.Close()
	awaitSubscriptionLive(ctx, t, live)

	_, memberComms, err := f.AsObserver(ctx, mpMemberHandle)
	if err != nil {
		t.Fatalf("AsObserver(%s): %v", mpMemberHandle, err)
	}
	if _, err := f.PostMessageAsObserver(ctx, memberComms, chID, mpTopic, mpRejectedBody); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("non-owner post on OWNER_ONLY channel: err = %v, want PERMISSION_DENIED", err)
	}
	if _, err := f.PostMessage(ctx, chID, mpTopic, mpCanaryBody); err != nil {
		t.Fatalf("PostMessage(owner canary): %v", err)
	}
	first, err := f.AwaitDelivery(ctx, live, func(m *compassv1.Message) bool {
		body := firstBlockText(m)
		return body == mpRejectedBody || body == mpCanaryBody
	})
	if err != nil {
		t.Fatalf("admin stream carried neither the refused post nor the canary: %v", err)
	}
	if got := firstBlockText(first); got != mpCanaryBody {
		t.Fatalf("first event = %q, want the owner canary: the refused post fanned out", got)
	}
}
