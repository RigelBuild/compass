//go:build unix

package runnerhub

// The roster + set_status relay arms (SEA-1721 T2): RelayCommsCall dispatches a
// roster call to RosterAsAccount and a set_status call to SetStatusAsAccount
// under the bound account, wraps the matching result oneof, and — for set_status
// — fires the best-effort PublishActivity carrying the SERVER-TRUNCATED value
// AFTER the durable write returned. Driven through the fakeCommsCaller + a
// fakePresenceSourceHub, no store. context.Background() is the test root
// (test-root ctx exemption).

import (
	"context"
	"errors"
	"testing"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// relayRoster builds a RelayCommsCallRequest carrying a roster variant.
func relayRoster(sessionID, callID string, roster *compassv1.GetRosterRequest) *compassv1internal.RelayCommsCallRequest {
	return &compassv1internal.RelayCommsCallRequest{
		SessionId: sessionID,
		Call: &compassv1internal.CommsCallRequest{
			CallId: callID,
			Call:   &compassv1internal.CommsCallRequest_Roster{Roster: roster},
		},
	}
}

// relaySetStatus builds a RelayCommsCallRequest carrying a set_status variant.
func relaySetStatus(sessionID, callID, activity string) *compassv1internal.RelayCommsCallRequest {
	return &compassv1internal.RelayCommsCallRequest{
		SessionId: sessionID,
		Call: &compassv1internal.CommsCallRequest{
			CallId: callID,
			Call:   &compassv1internal.CommsCallRequest_SetStatus{SetStatus: &compassv1internal.SetAgentStatusRequest{Activity: activity}},
		},
	}
}

// TestRelayCommsCallRosterArmForwardsUnderBoundAccount: a roster call forwards
// the request under the bound account and wraps the roster result oneof.
func TestRelayCommsCallRosterArmForwardsUnderBoundAccount(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.rosterResp = &compassv1.GetRosterResponse{Entries: []*compassv1.RosterEntry{{AgentAccountId: "a-1"}}}
	bindLiveSession(hub)

	req := &compassv1.GetRosterRequest{Scope: compassv1.RosterScope_ROSTER_SCOPE_SUBTREE}
	resp, err := hub.RelayCommsCall(context.Background(), relayRoster("sess-1", "tc-r", req))
	if err != nil {
		t.Fatalf("RelayCommsCall(roster) = %v, want success", err)
	}
	calls := comms.snapshot()
	if len(calls) != 1 {
		t.Fatalf("caller invoked %d times, want 1", len(calls))
	}
	if calls[0].account != "acct-agent" {
		t.Fatalf("roster attributed to %q, want bound acct-agent", calls[0].account)
	}
	if calls[0].roster != req {
		t.Fatalf("caller received a different GetRosterRequest than relayed")
	}
	if resp.GetResult().GetRoster() == nil {
		t.Fatalf("result oneof = %T, want a roster result", resp.GetResult().GetResult())
	}
	if got := resp.GetResult().GetCallId(); got != "tc-r" {
		t.Fatalf("response call_id = %q, want tc-r", got)
	}
}

// TestRelayCommsCallSetStatusArmWritesThenPublishesTruncated: a set_status call
// forwards the activity to SetStatusAsAccount (the durable write) under the bound
// account, and THEN publishes the SERVER-TRUNCATED value returned by that write —
// the ordered write-then-publish. The fake truncates to 5 runes, so the published
// activity must be the truncated string, not the raw input.
func TestRelayCommsCallSetStatusArmWritesThenPublishesTruncated(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.setStatusTruncateTo = 5
	src := &fakePresenceSourceHub{presence: map[store.AccountID]compassv1.AgentPresence{}}
	hub.SetPresenceSource(src)
	bindLiveSession(hub)

	resp, err := hub.RelayCommsCall(context.Background(), relaySetStatus("sess-1", "tc-s", "abcdefghij"))
	if err != nil {
		t.Fatalf("RelayCommsCall(set_status) = %v, want success", err)
	}
	calls := comms.snapshot()
	if len(calls) != 1 {
		t.Fatalf("caller invoked %d times, want 1", len(calls))
	}
	if calls[0].account != "acct-agent" {
		t.Fatalf("set_status attributed to %q, want bound acct-agent", calls[0].account)
	}
	if calls[0].setStatus != "abcdefghij" {
		t.Fatalf("SetStatusAsAccount received %q, want the raw relayed activity", calls[0].setStatus)
	}
	if resp.GetResult().GetSetStatus() == nil {
		t.Fatalf("result oneof = %T, want a set_status result", resp.GetResult().GetResult())
	}
	// The publish carries the TRUNCATED value (what landed in the table), fired
	// after the write returned.
	if len(src.published) != 1 {
		t.Fatalf("published count = %d, want exactly 1 (write-then-publish)", len(src.published))
	}
	if got := src.published[0].activity; got != "abcde" {
		t.Fatalf("published activity = %q, want the truncated %q", got, "abcde")
	}
	if got := src.published[0].account; got != "acct-agent" {
		t.Fatalf("published account = %q, want acct-agent", got)
	}
}

// TestRelayCommsCallSetStatusArmNoPresenceSourceStillSucceeds: a set_status call
// on a hub with no presence source wired still commits the durable write and
// returns success — the publish is best-effort, never gating the call.
func TestRelayCommsCallSetStatusArmNoPresenceSourceStillSucceeds(t *testing.T) {
	hub, comms := newHubWithComms()
	bindLiveSession(hub)

	_, err := hub.RelayCommsCall(context.Background(), relaySetStatus("sess-1", "tc-s", "status"))
	if err != nil {
		t.Fatalf("RelayCommsCall(set_status, no presence source) = %v, want success", err)
	}
	if calls := comms.snapshot(); len(calls) != 1 {
		t.Fatalf("caller invoked %d times, want 1 (the durable write still ran)", len(calls))
	}
}

// TestRelayCommsCallSetStatusArmWriteErrorDoesNotPublish: when the durable
// SetStatusAsAccount write fails, the set_status arm returns before
// PublishActivity — never publishing a status that did not land in the table.
// The failure surfaces in-band as a CommsCallError (the transport survives);
// asserting that error is rendered AND the presence source published NOTHING
// proves the publish is gated behind a successful write.
//
// Mutation: publish before checking the write error → src.published gains an
// entry; this test fails.
func TestRelayCommsCallSetStatusArmWriteErrorDoesNotPublish(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.setStatusErr = errors.New("write failed")
	src := &fakePresenceSourceHub{presence: map[store.AccountID]compassv1.AgentPresence{}}
	hub.SetPresenceSource(src)
	bindLiveSession(hub)

	resp, err := hub.RelayCommsCall(context.Background(), relaySetStatus("sess-1", "tc-s", "status"))
	if err != nil {
		t.Fatalf("RelayCommsCall(set_status, write error) = %v, want the failure rendered in-band", err)
	}
	if resp.GetResult().GetError() == nil {
		t.Fatalf("result oneof = %T, want an in-band CommsCallError for the failed write", resp.GetResult().GetResult())
	}
	if len(src.published) != 0 {
		t.Fatalf("published count = %d, want 0 (no publish of an unpersisted status)", len(src.published))
	}
}
