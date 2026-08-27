//go:build pgtest

package comms

// SetChannelPolicy handler + policy enforcement at the RPC edge (SEA-1722 T4):
// the handler sets the policy and echoes the updated channel; an OWNER_ONLY
// non-owner post maps to CodeNotFound (the no-oracle in-band rejection); an
// unsubscribe on a mandatory channel maps to CodeInvalidArgument. Driven
// in-process via connect.NewRequest + WithActor against a real store and bus.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

// TestSetChannelPolicyUpdatesAndEchoes pins the handler happy path: setting
// OWNER_ONLY + owner + mandatory returns the updated channel carrying the new
// policy fields.
func TestSetChannelPolicyUpdatesAndEchoes(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")

	created, err := svc.CreateChannel(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name: "room", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
	}))
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	chID := created.Msg.GetChannel().GetId()

	resp, err := svc.SetChannelPolicy(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.SetChannelPolicyRequest{
		ChannelId:             chID,
		PostPolicy:            compassv1.ChannelPostPolicy_CHANNEL_POST_POLICY_OWNER_ONLY,
		OwnerHandle:           owner.Handle,
		MandatorySubscription: true,
	}))
	if err != nil {
		t.Fatalf("SetChannelPolicy: %v", err)
	}
	ch := resp.Msg.GetChannel()
	if ch.GetPostPolicy() != compassv1.ChannelPostPolicy_CHANNEL_POST_POLICY_OWNER_ONLY {
		t.Fatalf("post_policy = %v, want OWNER_ONLY", ch.GetPostPolicy())
	}
	if ch.GetOwnerAccountId() != string(owner.ID) {
		t.Fatalf("owner_account_id = %q, want %q", ch.GetOwnerAccountId(), owner.ID)
	}
	if !ch.GetMandatorySubscription() {
		t.Fatalf("mandatory_subscription = false, want true")
	}
}

// TestPostMessageOwnerOnlyNonOwnerIsNotFound: a non-owner member posting to an
// OWNER_ONLY channel gets CodeNotFound at the edge — the same code a non-member
// gets, so the policy leaks no oracle.
func TestPostMessageOwnerOnlyNonOwnerIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	other := mustUser(t, st, "other")

	created, err := svc.CreateChannel(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name: "room", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
		MemberHandles: []string{other.Handle},
	}))
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	chID := created.Msg.GetChannel().GetId()

	if _, err := svc.SetChannelPolicy(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.SetChannelPolicyRequest{
		ChannelId:   chID,
		PostPolicy:  compassv1.ChannelPostPolicy_CHANNEL_POST_POLICY_OWNER_ONLY,
		OwnerHandle: owner.Handle,
	})); err != nil {
		t.Fatalf("SetChannelPolicy: %v", err)
	}

	_, err = svc.PostMessage(WithActor(ctx, other.ID), connect.NewRequest(&compassv1.PostMessageRequest{
		Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: chID},
		Topic:     &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
		Blocks:    []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "blocked"}}},
	}))
	connectCodeIs(t, err, connect.CodeNotFound, "non-owner post on OWNER_ONLY channel")
}

// TestUpdateChannelMembersUnsubscribeMandatoryIsInvalidArgument: an unsubscribe
// on a mandatory channel maps to CodeInvalidArgument at the edge.
func TestUpdateChannelMembersUnsubscribeMandatoryIsInvalidArgument(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	member := mustUser(t, st, "member")

	created, err := svc.CreateChannel(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name: "room", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
		MemberHandles: []string{member.Handle},
	}))
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	chID := created.Msg.GetChannel().GetId()

	if _, err := svc.SetChannelPolicy(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.SetChannelPolicyRequest{
		ChannelId:             chID,
		MandatorySubscription: true,
	})); err != nil {
		t.Fatalf("SetChannelPolicy: %v", err)
	}

	_, err = svc.UpdateChannelMembers(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.UpdateChannelMembersRequest{
		ChannelId:          chID,
		UnsubscribeHandles: []string{member.Handle},
	}))
	connectCodeIs(t, err, connect.CodeInvalidArgument, "unsubscribe on mandatory channel")
}
