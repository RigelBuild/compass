//go:build unix

package gateway

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

type recordedBoardCall struct {
	req *compassv1internal.RelayBoardCallRequest
}

type fakeBoardRelay struct {
	resp  *compassv1internal.RelayBoardCallResponse
	err   error
	calls []recordedBoardCall
}

func (f *fakeBoardRelay) RelayBoardCall(
	_ context.Context, req *connect.Request[compassv1internal.RelayBoardCallRequest],
) (*connect.Response[compassv1internal.RelayBoardCallResponse], error) {
	f.calls = append(f.calls, recordedBoardCall{req: req.Msg})
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(f.resp), nil
}

func boardSetIssueStateCall() *compassv1internal.BoardCallRequest {
	return &compassv1internal.BoardCallRequest{
		CallId: testCallID,
		Call: &compassv1internal.BoardCallRequest_SetIssueState{
			SetIssueState: &compassv1internal.SetIssueStateRequest{
				IssueId: "iss-1",
				State:   compassv1.IssueState_ISSUE_STATE_DONE,
			},
		},
	}
}

func TestBoardHappyPathForwardsUnderBoundSessionAndReturnsResult(t *testing.T) {
	sessions := &fakeSessions{sessionID: "sess-7", ok: true}
	wantResult := &compassv1internal.BoardCallResult{
		CallId: testCallID,
		Result: &compassv1internal.BoardCallResult_SetIssueState{
			SetIssueState: &compassv1internal.SetIssueStateResponse{
				Issue: &compassv1.Issue{Id: "iss-1", State: compassv1.IssueState_ISSUE_STATE_DONE},
			},
		},
	}
	relay := &fakeBoardRelay{resp: &compassv1internal.RelayBoardCallResponse{Result: wantResult}}
	g := NewGateway(context.Background(), "cnt-A", Deps{Sessions: sessions, Board: relay})

	resp, err := g.Board(context.Background(), connect.NewRequest(boardSetIssueStateCall()))
	if err != nil {
		t.Fatalf("Board = %v, want success", err)
	}
	if len(relay.calls) != 1 {
		t.Fatalf("relay forwarded %d calls, want exactly 1", len(relay.calls))
	}
	got := relay.calls[0].req
	if got.GetSessionId() != "sess-7" {
		t.Fatalf("forwarded session id = %q, want sess-7", got.GetSessionId())
	}
	if got.GetCall().GetCallId() != testCallID {
		t.Fatalf("forwarded call id = %q, want %q", got.GetCall().GetCallId(), testCallID)
	}
	if got.GetCall().GetSetIssueState().GetIssueId() != "iss-1" {
		t.Fatalf("forwarded issue id = %q, want iss-1", got.GetCall().GetSetIssueState().GetIssueId())
	}
	if got.GetCall().GetSetIssueState().GetState() != compassv1.IssueState_ISSUE_STATE_DONE {
		t.Fatalf("forwarded state = %v, want ISSUE_STATE_DONE", got.GetCall().GetSetIssueState().GetState())
	}
	if resp.Msg.GetCallId() != testCallID || resp.Msg.GetSetIssueState().GetIssue().GetId() != "iss-1" {
		t.Fatalf("returned result = %+v, want the Server's board result", resp.Msg)
	}
}

func TestBoardInBandErrorPassesThrough(t *testing.T) {
	sessions := &fakeSessions{sessionID: "sess-7", ok: true}
	relay := &fakeBoardRelay{resp: &compassv1internal.RelayBoardCallResponse{
		Result: &compassv1internal.BoardCallResult{
			CallId: testCallID,
			Result: &compassv1internal.BoardCallResult_Error{Error: &compassv1internal.BoardCallError{
				Code:    "not_found",
				Message: "issue does not exist",
			}},
		},
	}}
	g := NewGateway(context.Background(), "cnt-A", Deps{Sessions: sessions, Board: relay})

	resp, err := g.Board(context.Background(), connect.NewRequest(boardSetIssueStateCall()))
	if err != nil {
		t.Fatalf("Board with an in-band error = %v, want nil Go error", err)
	}
	if got := resp.Msg.GetError().GetCode(); got != "not_found" {
		t.Fatalf("in-band error code = %q, want not_found", got)
	}
}

func TestBoardTransportFailurePropagatesConnectError(t *testing.T) {
	sessions := &fakeSessions{sessionID: "sess-7", ok: true}
	wantErr := connect.NewError(connect.CodeUnavailable, errors.New("server unreachable"))
	relay := &fakeBoardRelay{err: wantErr}
	g := NewGateway(context.Background(), "cnt-A", Deps{Sessions: sessions, Board: relay})

	resp, err := g.Board(context.Background(), connect.NewRequest(boardSetIssueStateCall()))
	if err != wantErr {
		t.Fatalf("Board error = %v, want the upstream error %v", err, wantErr)
	}
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("propagated error code = %v, want Unavailable", got)
	}
	if resp != nil {
		t.Fatalf("transport-failure response = %+v, want nil", resp)
	}
}

func TestBoardNilResultIsInternalError(t *testing.T) {
	sessions := &fakeSessions{sessionID: "sess-7", ok: true}
	relay := &fakeBoardRelay{resp: &compassv1internal.RelayBoardCallResponse{}}
	g := NewGateway(context.Background(), "cnt-A", Deps{Sessions: sessions, Board: relay})

	resp, err := g.Board(context.Background(), connect.NewRequest(boardSetIssueStateCall()))
	if err == nil {
		t.Fatal("Board with a nil relay result = nil error, want CodeInternal")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("nil-result error code = %v, want Internal", got)
	}
	if resp != nil {
		t.Fatalf("nil-result response = %+v, want nil", resp)
	}
}

func TestBoardUnboundSessionFailsClosedWithoutForwarding(t *testing.T) {
	for name, sessions := range map[string]*fakeSessions{
		"no session":       {ok: false},
		"empty session id": {sessionID: "", ok: true},
	} {
		t.Run(name, func(t *testing.T) {
			relay := &fakeBoardRelay{}
			g := NewGateway(context.Background(), "cnt-A", Deps{Sessions: sessions, Board: relay})

			resp, err := g.Board(context.Background(), connect.NewRequest(boardSetIssueStateCall()))
			if got := connect.CodeOf(err); err == nil || got != connect.CodePermissionDenied {
				t.Fatalf("unbound Board error = %v (code %v), want PermissionDenied", err, got)
			}
			if resp != nil {
				t.Fatalf("unbound Board response = %+v, want nil", resp)
			}
			if len(relay.calls) != 0 {
				t.Fatalf("relay forwarded %d calls with no bound session, want 0", len(relay.calls))
			}
		})
	}
}
