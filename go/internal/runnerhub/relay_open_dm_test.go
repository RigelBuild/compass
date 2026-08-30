//go:build unix

package runnerhub

// The peer-DM relay arm (RIG-2962 T3): RelayCommsCall dispatches an open_dm call
// to OpenDMAsAccount under the bound account, wrapping the open_dm result oneof
// with call_id round-tripped. A tool error on the arm is rendered in-band as a
// CommsCallError, never a transport teardown. Driven through the fakeCommsCaller,
// no store. context.Background() is the test root (test-root ctx exemption).

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// relayOpenDM builds a RelayCommsCallRequest carrying an open_dm variant.
func relayOpenDM(sessionID, callID string, req *compassv1.OpenDMRequest) *compassv1internal.RelayCommsCallRequest {
	return &compassv1internal.RelayCommsCallRequest{
		SessionId: sessionID,
		Call: &compassv1internal.CommsCallRequest{
			CallId: callID,
			Call:   &compassv1internal.CommsCallRequest_OpenDm{OpenDm: req},
		},
	}
}

// TestRelayCommsCallOpenDMArmForwardsUnderBoundAccount: an open_dm call forwards
// the request under the bound account and wraps the open_dm result oneof, with
// call_id round-tripped.
func TestRelayCommsCallOpenDMArmForwardsUnderBoundAccount(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.openDMResp = &compassv1.OpenDMResponse{Channel: &compassv1.Channel{Id: "ch-dm"}, Created: true}
	bindLiveSession(hub)

	req := &compassv1.OpenDMRequest{PeerHandle: "peer"}
	resp, err := hub.RelayCommsCall(context.Background(), relayOpenDM("sess-1", "tc-dm", req))
	if err != nil {
		t.Fatalf("RelayCommsCall(open_dm) = %v, want success", err)
	}
	calls := comms.snapshot()
	if len(calls) != 1 {
		t.Fatalf("caller invoked %d times, want 1", len(calls))
	}
	if calls[0].account != "acct-agent" {
		t.Fatalf("open_dm attributed to %q, want bound acct-agent", calls[0].account)
	}
	if calls[0].openDM != req {
		t.Fatalf("caller received a different OpenDMRequest than relayed")
	}
	if resp.GetResult().GetOpenDm() != comms.openDMResp {
		t.Fatalf("result oneof = %T, want the caller's open_dm response", resp.GetResult().GetResult())
	}
	if resp.GetResult().GetOpenDm().GetCreated() != true {
		t.Fatalf("open_dm created = false, want the caller's created=true round-tripped")
	}
	if got := resp.GetResult().GetCallId(); got != "tc-dm" {
		t.Fatalf("response call_id = %q, want tc-dm", got)
	}
}

// TestRelayCommsCallOpenDMToolErrorIsInBandNotStreamError: a tool-level failure
// on the open_dm arm is rendered IN-BAND as a CommsCallError, not as a Connect
// stream error — the "tool failure != transport teardown" invariant on the
// peer-DM arm (mirrors the org-management arms). not_found is the oracle-safe
// collapse an unknown/cross-owner peer gets.
func TestRelayCommsCallOpenDMToolErrorIsInBandNotStreamError(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.openDMErr = connect.NewError(connect.CodeNotFound, errors.New("handle \"peer\" not found"))
	bindLiveSession(hub)

	resp, err := hub.RelayCommsCall(context.Background(), relayOpenDM("sess-1", "tc-dm-err", &compassv1.OpenDMRequest{PeerHandle: "peer"}))
	if err != nil {
		t.Fatalf("RelayCommsCall returned a stream error %v, want in-band tool error", err)
	}
	toolErr := resp.GetResult().GetError()
	if toolErr == nil {
		t.Fatal("response has no in-band CommsCallError, want the tool failure rendered in-band")
	}
	if toolErr.GetCode() != "not_found" {
		t.Fatalf("in-band error code = %q, want not_found (the oracle-safe collapse token)", toolErr.GetCode())
	}
	if got := resp.GetResult().GetCallId(); got != "tc-dm-err" {
		t.Fatalf("response call_id = %q, want tc-dm-err", got)
	}
}
