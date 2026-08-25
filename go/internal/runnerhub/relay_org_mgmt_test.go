//go:build unix

package runnerhub

// The org-management relay arms (RIG-2673 T3): RelayCommsCall dispatches a
// create_channel call to CreateChannelAsAccount, an update_members call to
// UpdateChannelMembersAsAccount, and a create_channel_group call to
// CreateChannelGroupAsAccount — each under the bound account, wrapping the
// matching result oneof, with call_id round-tripped. A tool error on an arm is
// rendered in-band as a CommsCallError, never a transport teardown. Driven
// through the fakeCommsCaller, no store. context.Background() is the test root
// (test-root ctx exemption).

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// relayCreateChannel builds a RelayCommsCallRequest carrying a create_channel variant.
func relayCreateChannel(sessionID, callID string, req *compassv1.CreateChannelRequest) *compassv1internal.RelayCommsCallRequest {
	return &compassv1internal.RelayCommsCallRequest{
		SessionId: sessionID,
		Call: &compassv1internal.CommsCallRequest{
			CallId: callID,
			Call:   &compassv1internal.CommsCallRequest_CreateChannel{CreateChannel: req},
		},
	}
}

// relayUpdateMembers builds a RelayCommsCallRequest carrying an update_members variant.
func relayUpdateMembers(sessionID, callID string, req *compassv1.UpdateChannelMembersRequest) *compassv1internal.RelayCommsCallRequest {
	return &compassv1internal.RelayCommsCallRequest{
		SessionId: sessionID,
		Call: &compassv1internal.CommsCallRequest{
			CallId: callID,
			Call:   &compassv1internal.CommsCallRequest_UpdateMembers{UpdateMembers: req},
		},
	}
}

// relayCreateChannelGroup builds a RelayCommsCallRequest carrying a create_channel_group variant.
func relayCreateChannelGroup(sessionID, callID string, req *compassv1.CreateChannelGroupRequest) *compassv1internal.RelayCommsCallRequest {
	return &compassv1internal.RelayCommsCallRequest{
		SessionId: sessionID,
		Call: &compassv1internal.CommsCallRequest{
			CallId: callID,
			Call:   &compassv1internal.CommsCallRequest_CreateChannelGroup{CreateChannelGroup: req},
		},
	}
}

// TestRelayCommsCallCreateChannelArmForwardsUnderBoundAccount: a create_channel
// call forwards the request under the bound account and wraps the create_channel
// result oneof, with call_id round-tripped.
func TestRelayCommsCallCreateChannelArmForwardsUnderBoundAccount(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.createChannelResp = &compassv1.CreateChannelResponse{Channel: &compassv1.Channel{Id: "ch-1"}}
	bindLiveSession(hub)

	req := &compassv1.CreateChannelRequest{Name: "room"}
	resp, err := hub.RelayCommsCall(context.Background(), relayCreateChannel("sess-1", "tc-cc", req))
	if err != nil {
		t.Fatalf("RelayCommsCall(create_channel) = %v, want success", err)
	}
	calls := comms.snapshot()
	if len(calls) != 1 {
		t.Fatalf("caller invoked %d times, want 1", len(calls))
	}
	if calls[0].account != "acct-agent" {
		t.Fatalf("create_channel attributed to %q, want bound acct-agent", calls[0].account)
	}
	if calls[0].createChannel != req {
		t.Fatalf("caller received a different CreateChannelRequest than relayed")
	}
	if resp.GetResult().GetCreateChannel() != comms.createChannelResp {
		t.Fatalf("result oneof = %T, want the caller's create_channel response", resp.GetResult().GetResult())
	}
	if got := resp.GetResult().GetCallId(); got != "tc-cc" {
		t.Fatalf("response call_id = %q, want tc-cc", got)
	}
}

// TestRelayCommsCallUpdateMembersArmForwardsUnderBoundAccount: an update_members
// call forwards under the bound account and wraps the update_members result oneof.
func TestRelayCommsCallUpdateMembersArmForwardsUnderBoundAccount(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.updateMembersResp = &compassv1.UpdateChannelMembersResponse{Channel: &compassv1.Channel{Id: "ch-1"}}
	bindLiveSession(hub)

	req := &compassv1.UpdateChannelMembersRequest{ChannelId: "ch-1", AddMemberHandles: []string{"a-2"}}
	resp, err := hub.RelayCommsCall(context.Background(), relayUpdateMembers("sess-1", "tc-um", req))
	if err != nil {
		t.Fatalf("RelayCommsCall(update_members) = %v, want success", err)
	}
	calls := comms.snapshot()
	if len(calls) != 1 {
		t.Fatalf("caller invoked %d times, want 1", len(calls))
	}
	if calls[0].account != "acct-agent" {
		t.Fatalf("update_members attributed to %q, want bound acct-agent", calls[0].account)
	}
	if calls[0].updateMembers != req {
		t.Fatalf("caller received a different UpdateChannelMembersRequest than relayed")
	}
	if resp.GetResult().GetUpdateMembers() != comms.updateMembersResp {
		t.Fatalf("result oneof = %T, want the caller's update_members response", resp.GetResult().GetResult())
	}
	if got := resp.GetResult().GetCallId(); got != "tc-um" {
		t.Fatalf("response call_id = %q, want tc-um", got)
	}
}

// TestRelayCommsCallCreateChannelGroupArmForwardsUnderBoundAccount: a
// create_channel_group call forwards under the bound account and wraps the
// create_channel_group result oneof.
func TestRelayCommsCallCreateChannelGroupArmForwardsUnderBoundAccount(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.createChannelGroupResp = &compassv1.CreateChannelGroupResponse{Group: &compassv1.ChannelGroup{Id: "grp-1"}}
	bindLiveSession(hub)

	req := &compassv1.CreateChannelGroupRequest{Name: "team"}
	resp, err := hub.RelayCommsCall(context.Background(), relayCreateChannelGroup("sess-1", "tc-cg", req))
	if err != nil {
		t.Fatalf("RelayCommsCall(create_channel_group) = %v, want success", err)
	}
	calls := comms.snapshot()
	if len(calls) != 1 {
		t.Fatalf("caller invoked %d times, want 1", len(calls))
	}
	if calls[0].account != "acct-agent" {
		t.Fatalf("create_channel_group attributed to %q, want bound acct-agent", calls[0].account)
	}
	if calls[0].createChannelGroup != req {
		t.Fatalf("caller received a different CreateChannelGroupRequest than relayed")
	}
	if resp.GetResult().GetCreateChannelGroup() != comms.createChannelGroupResp {
		t.Fatalf("result oneof = %T, want the caller's create_channel_group response", resp.GetResult().GetResult())
	}
	if got := resp.GetResult().GetCallId(); got != "tc-cg" {
		t.Fatalf("response call_id = %q, want tc-cg", got)
	}
}

// TestRelayCommsCallCreateChannelToolErrorIsInBandNotStreamError: a tool-level
// failure on the create_channel arm is rendered IN-BAND as a CommsCallError, not
// as a Connect stream error — the "tool failure != transport teardown" invariant
// on a new org-management arm (mirrors the post-arm invariant).
func TestRelayCommsCallCreateChannelToolErrorIsInBandNotStreamError(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.createChannelErr = connect.NewError(connect.CodeNotFound, errors.New("group not found"))
	bindLiveSession(hub)

	resp, err := hub.RelayCommsCall(context.Background(), relayCreateChannel("sess-1", "tc-cc-err", &compassv1.CreateChannelRequest{Name: "room"}))
	if err != nil {
		t.Fatalf("RelayCommsCall returned a stream error %v, want in-band tool error", err)
	}
	toolErr := resp.GetResult().GetError()
	if toolErr == nil {
		t.Fatal("response has no in-band CommsCallError, want the tool failure rendered in-band")
	}
	if toolErr.GetCode() != "not_found" {
		t.Fatalf("in-band error code = %q, want not_found (the D9 collapse token)", toolErr.GetCode())
	}
	if got := resp.GetResult().GetCallId(); got != "tc-cc-err" {
		t.Fatalf("response call_id = %q, want tc-cc-err", got)
	}
}

// TestRelayCommsCallUpdateMembersToolErrorIsInBandNotStreamError: a tool-level
// failure on the update_members arm renders IN-BAND as a CommsCallError, not a
// Connect stream error — the same tool-failure-is-not-a-teardown invariant, on
// the update_members arm.
func TestRelayCommsCallUpdateMembersToolErrorIsInBandNotStreamError(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.updateMembersErr = connect.NewError(connect.CodeNotFound, errors.New("channel not found"))
	bindLiveSession(hub)

	resp, err := hub.RelayCommsCall(context.Background(), relayUpdateMembers("sess-1", "tc-um-err", &compassv1.UpdateChannelMembersRequest{ChannelId: "ch-x", AddMemberHandles: []string{"a-2"}}))
	if err != nil {
		t.Fatalf("RelayCommsCall returned a stream error %v, want in-band tool error", err)
	}
	toolErr := resp.GetResult().GetError()
	if toolErr == nil {
		t.Fatal("response has no in-band CommsCallError, want the tool failure rendered in-band")
	}
	if toolErr.GetCode() != "not_found" {
		t.Fatalf("in-band error code = %q, want not_found (the D9 collapse token)", toolErr.GetCode())
	}
	if got := resp.GetResult().GetCallId(); got != "tc-um-err" {
		t.Fatalf("response call_id = %q, want tc-um-err", got)
	}
}

// TestRelayCommsCallCreateChannelGroupToolErrorIsInBandNotStreamError: a
// tool-level failure on the create_channel_group arm renders IN-BAND as a
// CommsCallError, not a Connect stream error.
func TestRelayCommsCallCreateChannelGroupToolErrorIsInBandNotStreamError(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.createChannelGroupErr = connect.NewError(connect.CodeNotFound, errors.New("parent group not found"))
	bindLiveSession(hub)

	resp, err := hub.RelayCommsCall(context.Background(), relayCreateChannelGroup("sess-1", "tc-cg-err", &compassv1.CreateChannelGroupRequest{Name: "team"}))
	if err != nil {
		t.Fatalf("RelayCommsCall returned a stream error %v, want in-band tool error", err)
	}
	toolErr := resp.GetResult().GetError()
	if toolErr == nil {
		t.Fatal("response has no in-band CommsCallError, want the tool failure rendered in-band")
	}
	if toolErr.GetCode() != "not_found" {
		t.Fatalf("in-band error code = %q, want not_found (the D9 collapse token)", toolErr.GetCode())
	}
	if got := resp.GetResult().GetCallId(); got != "tc-cg-err" {
		t.Fatalf("response call_id = %q, want tc-cg-err", got)
	}
}
