//go:build unix

package runnerhub

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// relayPin builds a RelayCommsCallRequest carrying a pin variant under callID.
func relayPin(sessionID, callID string, req *compassv1.UpdatePinnedBoardRequest) *compassv1internal.RelayCommsCallRequest {
	return &compassv1internal.RelayCommsCallRequest{
		SessionId: sessionID,
		Call: &compassv1internal.CommsCallRequest{
			CallId: callID,
			Call:   &compassv1internal.CommsCallRequest_Pin{Pin: req},
		},
	}
}

// TestRelayCommsPinDispatchesAsBoundAccount: a pin call forwards the exact
// request under the bound account and wraps the pin result with call_id intact.
func TestRelayCommsPinDispatchesAsBoundAccount(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.pinResp = &compassv1.UpdatePinnedBoardResponse{}
	bindLiveSession(hub)

	req := &compassv1.UpdatePinnedBoardRequest{ChannelId: "ch-1"}
	resp, err := hub.RelayCommsCall(context.Background(), relayPin("sess-1", "tc-pin", req))
	if err != nil {
		t.Fatalf("RelayCommsCall(pin) = %v, want success", err)
	}
	calls := comms.snapshot()
	if len(calls) != 1 {
		t.Fatalf("caller invoked %d times, want 1", len(calls))
	}
	if calls[0].account != testAgentAccount {
		t.Fatalf("pin attributed to %q, want bound %q", calls[0].account, testAgentAccount)
	}
	if calls[0].pin != req {
		t.Fatalf("caller received a different UpdatePinnedBoardRequest than relayed")
	}
	if resp.GetResult().GetPin() != comms.pinResp {
		t.Fatalf("result oneof = %T, want the caller's pin response", resp.GetResult().GetResult())
	}
	if got := resp.GetResult().GetCallId(); got != "tc-pin" {
		t.Fatalf("response call_id = %q, want tc-pin", got)
	}
}

// TestRelayCommsPinToolErrorIsInBandNotStreamError: a pin tool failure is
// rendered as a CommsCallError while RelayCommsCall itself remains successful.
func TestRelayCommsPinToolErrorIsInBandNotStreamError(t *testing.T) {
	hub, comms := newHubWithComms()
	comms.pinErr = connect.NewError(connect.CodePermissionDenied, errors.New("pin denied"))
	bindLiveSession(hub)

	resp, err := hub.RelayCommsCall(context.Background(), relayPin("sess-1", "tc-pin-err", &compassv1.UpdatePinnedBoardRequest{ChannelId: "ch-1"}))
	if err != nil {
		t.Fatalf("RelayCommsCall returned a stream error %v, want in-band tool error", err)
	}
	toolErr := resp.GetResult().GetError()
	if toolErr == nil {
		t.Fatal("response has no in-band CommsCallError, want the tool failure rendered in-band")
	}
	if got := toolErr.GetCode(); got != connect.CodePermissionDenied.String() {
		t.Fatalf("in-band error code = %q, want %q", got, connect.CodePermissionDenied.String())
	}
	if got := resp.GetResult().GetCallId(); got != "tc-pin-err" {
		t.Fatalf("response call_id = %q, want tc-pin-err", got)
	}
}

// armCase is one relay arm's coverage row: the request to relay, the response to
// seed on the fake, and the accessors reading back what the hub recorded and
// returned.
//
// seed installs the response the fake returns for this arm, and wantResp is that
// same instance. gotResp reads the arm the hub actually wrapped. Together they
// make the seed load-bearing: without the identity assertion an arm could wrap
// nil and still pass, because WhichOneof reports an arm as set whenever the
// wrapper struct exists even when the inner message pointer is nil.
//
// A row omits seed/gotResp/wantResp ONLY when the production arm returns a FRESH
// response rather than the caller's, so there is no instance to be identical to
// — today that is set_status alone, whose CommsCaller method returns a string.
// That opt-out is EARNED, not taken on trust: the sweep rejects a row that
// declines the pair while its result type has any field, because IsValid alone
// cannot tell a fresh empty message from the caller's and such a row could drop
// a real payload silently. set_status qualifies because SetAgentStatusResponse
// has zero fields.
type armCase struct {
	name     string
	request  *compassv1internal.RelayCommsCallRequest
	seed     func(*fakeCommsCaller)
	field    func(commsCall) any
	want     any
	gotResp  func(*compassv1internal.CommsCallResult) any
	wantResp any
}

// commsArmCases is the per-arm coverage table. It lives beside the test rather
// than inside it so the table reads as data and the assertions read as logic;
// commsCallOneofArms gates it against the oneof so a new arm cannot be missed.
func commsArmCases() []armCase {
	post := &compassv1.PostMessageRequest{}
	list := &compassv1.ListMessagesRequest{}
	roster := &compassv1.GetRosterRequest{}
	pin := &compassv1.UpdatePinnedBoardRequest{}
	createChannel := &compassv1.CreateChannelRequest{}
	updateMembers := &compassv1.UpdateChannelMembersRequest{}
	createChannelGroup := &compassv1.CreateChannelGroupRequest{}
	openDM := &compassv1.OpenDMRequest{}

	postResp := &compassv1.PostMessageResponse{}
	listResp := &compassv1.ListMessagesResponse{}
	rosterResp := &compassv1.GetRosterResponse{}
	pinResp := &compassv1.UpdatePinnedBoardResponse{}
	createChannelResp := &compassv1.CreateChannelResponse{}
	updateMembersResp := &compassv1.UpdateChannelMembersResponse{}
	createChannelGroupResp := &compassv1.CreateChannelGroupResponse{}
	openDMResp := &compassv1.OpenDMResponse{}

	return []armCase{
		{
			name:     "post",
			request:  relayPost("sess-1", "tc-post", post),
			seed:     func(c *fakeCommsCaller) { c.postResp = postResp },
			field:    func(c commsCall) any { return c.post },
			want:     post,
			gotResp:  func(r *compassv1internal.CommsCallResult) any { return r.GetPost() },
			wantResp: postResp,
		},
		{
			name:     "list",
			request:  relayList("sess-1", "tc-list", list),
			seed:     func(c *fakeCommsCaller) { c.listResp = listResp },
			field:    func(c commsCall) any { return c.list },
			want:     list,
			gotResp:  func(r *compassv1internal.CommsCallResult) any { return r.GetList() },
			wantResp: listResp,
		},
		{
			name:     "roster",
			request:  relayRoster("sess-1", "tc-roster", roster),
			seed:     func(c *fakeCommsCaller) { c.rosterResp = rosterResp },
			field:    func(c commsCall) any { return c.roster },
			want:     roster,
			gotResp:  func(r *compassv1internal.CommsCallResult) any { return r.GetRoster() },
			wantResp: rosterResp,
		},
		{
			// set_status is the one arm with no canned response to seed: the
			// production arm returns the server-truncated activity string and
			// wraps a FRESH empty response, so its identity check is against
			// that empty value's presence, not the caller's instance. `want`
			// is a string VALUE here, compared through `any` — which catches a
			// production bug forwarding "" or the call_id instead.
			name:    "set_status",
			request: relaySetStatus("sess-1", "tc-status", "status"),
			seed:    func(c *fakeCommsCaller) {},
			field:   func(c commsCall) any { return c.setStatus },
			want:    "status",
		},
		{
			name:     "pin",
			request:  relayPin("sess-1", "tc-pin", pin),
			seed:     func(c *fakeCommsCaller) { c.pinResp = pinResp },
			field:    func(c commsCall) any { return c.pin },
			want:     pin,
			gotResp:  func(r *compassv1internal.CommsCallResult) any { return r.GetPin() },
			wantResp: pinResp,
		},
		{
			name:     "create_channel",
			request:  relayCreateChannel("sess-1", "tc-channel", createChannel),
			seed:     func(c *fakeCommsCaller) { c.createChannelResp = createChannelResp },
			field:    func(c commsCall) any { return c.createChannel },
			want:     createChannel,
			gotResp:  func(r *compassv1internal.CommsCallResult) any { return r.GetCreateChannel() },
			wantResp: createChannelResp,
		},
		{
			name:     "update_members",
			request:  relayUpdateMembers("sess-1", "tc-members", updateMembers),
			seed:     func(c *fakeCommsCaller) { c.updateMembersResp = updateMembersResp },
			field:    func(c commsCall) any { return c.updateMembers },
			want:     updateMembers,
			gotResp:  func(r *compassv1internal.CommsCallResult) any { return r.GetUpdateMembers() },
			wantResp: updateMembersResp,
		},
		{
			name:     "create_channel_group",
			request:  relayCreateChannelGroup("sess-1", "tc-group", createChannelGroup),
			seed:     func(c *fakeCommsCaller) { c.createChannelGroupResp = createChannelGroupResp },
			field:    func(c commsCall) any { return c.createChannelGroup },
			want:     createChannelGroup,
			gotResp:  func(r *compassv1internal.CommsCallResult) any { return r.GetCreateChannelGroup() },
			wantResp: createChannelGroupResp,
		},
		{
			name:     "open_dm",
			request:  relayOpenDM("sess-1", "tc-dm", openDM),
			seed:     func(c *fakeCommsCaller) { c.openDMResp = openDMResp },
			field:    func(c commsCall) any { return c.openDM },
			want:     openDM,
			gotResp:  func(r *compassv1internal.CommsCallResult) any { return r.GetOpenDm() },
			wantResp: openDMResp,
		},
	}
}

// requireEveryArmCovered gates the hand-maintained table against the oneof
// descriptor in BOTH directions: every declared arm has a case, and no case
// names an arm the oneof no longer declares. Both are needed — the forward loop
// alone passes a table that lost an arm to a duplicate name, and the count alone
// passes a table covering the wrong nine.
func requireEveryArmCovered(t *testing.T, cases []armCase) {
	t.Helper()
	arms := commsCallOneofArms(t)
	for i := range arms.Len() {
		name := string(arms.Get(i).Name())
		found := false
		for _, arm := range cases {
			if arm.name == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("CommsCallRequest oneof arm %q is not covered; add a table case", name)
		}
	}
	if len(cases) != arms.Len() {
		t.Fatalf("table covers %d arms but the oneof declares %d — remove the stale case(s)", len(cases), arms.Len())
	}
}

// TestRelayCommsEveryArmAttributesToBoundAccount: every CommsCallRequest arm
// forwards its exact request under the session's bound account and returns the
// caller's own response wrapped in the matching result arm. Coverage is gated on
// the oneof descriptor in both directions, so a newly added arm fails here until
// it is listed in commsArmCases.
func TestRelayCommsEveryArmAttributesToBoundAccount(t *testing.T) {
	cases := commsArmCases()
	requireEveryArmCovered(t, cases)

	for _, arm := range cases {
		t.Run(arm.name, func(t *testing.T) {
			hub, comms := newHubWithComms()
			arm.seed(comms)
			bindLiveSession(hub)
			resp, err := hub.RelayCommsCall(context.Background(), arm.request)
			if err != nil {
				t.Fatalf("RelayCommsCall(%s) = %v, want success", arm.name, err)
			}
			calls := comms.snapshot()
			if len(calls) != 1 {
				t.Fatalf("caller invoked %d times, want 1", len(calls))
			}
			if calls[0].account != testAgentAccount {
				t.Fatalf("%s attributed to %q, want bound %q", arm.name, calls[0].account, testAgentAccount)
			}
			// One verb cannot serve both row shapes: the eight message rows are
			// pointer-identity, and %#v renders two zero-valued proto pointers
			// byte-identically (a 446-char wall reading "got = X, want = X");
			// set_status's row is a string VALUE, for which %p is an error
			// token. Split on the row's kind so the failure names the mismatch.
			if got := arm.field(calls[0]); got != arm.want {
				if want, ok := arm.want.(string); ok {
					t.Fatalf("%s recorded activity = %#v, want %#v", arm.name, got, want)
				}
				t.Fatalf("%s recorded request %p, want the relayed instance %p", arm.name, got, arm.want)
			}
			// The result must be wrapped in the arm MATCHING the request.
			// Attribution and forwarding both pass under a mis-wrapped
			// response, so without this a swapped result oneof is invisible.
			// CommsCallResult reuses CommsCallRequest.call's arm names
			// (agent_gateway.proto, `message CommsCallResult`), so the check is
			// generic over the oneof rather than per-arm.
			result := resp.GetResult().ProtoReflect()
			resultOneof := result.Descriptor().Oneofs().ByName("result")
			if resultOneof == nil {
				t.Fatal(`CommsCallResult has no oneof named "result" — the arm-coverage gate lost its descriptor and is measuring nothing`)
			}
			set := result.WhichOneof(resultOneof)
			if set == nil {
				t.Fatalf("%s returned no result arm set", arm.name)
			}
			if got := string(set.Name()); got != arm.name {
				t.Fatalf("%s wrapped its response in the %q result arm, want %q", arm.name, got, arm.name)
			}
			// The floor EVERY arm pays, including one that declines wantResp:
			// WhichOneof reports an arm as set whenever the wrapper struct
			// exists, even wrapping a NIL message, so the name check alone
			// passes an arm that drops the caller's response. IsValid is what
			// separates a real empty message from a nil pointer — Has cannot.
			if !result.Get(set).Message().IsValid() {
				t.Fatalf("%s wrapped a NIL message in the %q result arm", arm.name, arm.name)
			}
			// The opt-out must be EARNED, not asserted in a comment: IsValid
			// separates non-nil from nil and cannot tell the caller's instance
			// from a fresh empty one, so a row that declines the identity check
			// while its result type HAS fields could drop the whole response
			// silently. Only a zero-field response type has nothing to lose,
			// which is why set_status alone qualifies — and a future arm that
			// forgets the pair fails here instead of going uncovered.
			if arm.gotResp == nil && set.Message().Fields().Len() != 0 {
				t.Fatalf("%s declines the identity check but its result type %s has %d field(s) that could be silently dropped; add gotResp/wantResp",
					arm.name, set.Message().FullName().Name(), set.Message().Fields().Len())
			}
			// The ceiling the eight arms with a caller-owned response reach:
			// the payload is the seeded instance, which is what makes each
			// seed load-bearing and kills a wrong-instance swap.
			if arm.gotResp != nil {
				if got := arm.gotResp(resp.GetResult()); got != arm.wantResp {
					t.Fatalf("%s returned response %p, want the caller's seeded instance %p", arm.name, got, arm.wantResp)
				}
			}
		})
	}
}

// commsCallOneofArms resolves the CommsCallRequest `call` oneof's fields, and is
// shared so every structural test over the oneof applies ONE standard for the
// lookup. A nil descriptor is caught before any method call on it: ranging a nil
// oneof panics, which reads as a confusing crash rather than "this gate stopped
// measuring". Both vacuity guards live here for the same reason.
func commsCallOneofArms(t *testing.T) protoreflect.FieldDescriptors {
	t.Helper()
	oneof := (&compassv1internal.CommsCallRequest{}).ProtoReflect().Descriptor().Oneofs().ByName("call")
	if oneof == nil {
		t.Fatal(`CommsCallRequest has no oneof named "call" — the arm-coverage gate lost its descriptor and is measuring nothing`)
	}
	arms := oneof.Fields()
	if arms.Len() == 0 {
		t.Fatal(`CommsCallRequest "call" oneof yielded zero fields — the arm-coverage gate is vacuous`)
	}
	return arms
}

// TestCommsCallRequestHasNoAskAnsweringArm: the relay oneof cannot answer an ask
// because agents raise asks, while operators answer through
// CommsService.RespondToAsk (comms.proto:108). This guards the LITERAL
// RespondToAsk shape — a field named respond_to_ask, or any arm carrying a
// RespondToAsk-named message — not every conceivable ask-answering spelling; an
// arm named answer_ask would pass. It is an executable statement of the
// structural claim, not an airtight semantic gate. The oneof's arm count is
// pinned separately by the sweep above, which is what catches an unreviewed new
// arm under any name.
func TestCommsCallRequestHasNoAskAnsweringArm(t *testing.T) {
	arms := commsCallOneofArms(t)
	for i := range arms.Len() {
		field := arms.Get(i)
		if field.Name() == "respond_to_ask" {
			t.Fatalf("CommsCallRequest oneof contains forbidden arm %q", field.Name())
		}
		if field.Message() != nil && strings.Contains(string(field.Message().Name()), "RespondToAsk") {
			t.Fatalf("CommsCallRequest oneof arm %q uses forbidden message type %q", field.Name(), field.Message().Name())
		}
	}
}
