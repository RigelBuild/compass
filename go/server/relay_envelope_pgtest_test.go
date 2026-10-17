//go:build pgtest && unix

package server

import (
	"testing"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/comms"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

func TestRelayEnvelopeErrorContracts(t *testing.T) {
	w := newRelayEnvelopeE2EWire(t)
	testCommsEnvelopeErrors(t, w)
	testBoardEnvelopeErrors(t, w)
	testForgeEnvelopeErrors(t, w)
	testLifecycleEnvelopeErrors(t, w)
}

func newRelayEnvelopeE2EWire(t *testing.T) *e2eWire {
	t.Helper()
	w := newE2EWire(t)
	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(commsBus.Close)
	commsSvc := comms.NewComms(w.ctx, w.store, commsBus, nil, w.adminID)
	issueBus := events.NewBus[busPayload]()
	t.Cleanup(issueBus.Close)
	issueBrd := board.NewIssueProjection(issueBus, w.store)
	wireHubServiceCycles(w.hub, commsSvc, w.store, issueBrd)
	w.hub.SetForgeCaller(newForgeService(w.store, issueBrd, newForgeProviderRegistry()))
	return w
}

func testCommsEnvelopeErrors(t *testing.T, w *e2eWire) {
	t.Helper()
	t.Run("comms_not_found", func(t *testing.T) {
		resp, err := w.supervisorClient.Comms(w.ctx, &compassv1internal.CommsCallRequest{
			CallId: "comms-not-found",
			Call: &compassv1internal.CommsCallRequest_Post{Post: &compassv1.PostMessageRequest{
				Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: "relay-envelope-missing-channel"},
			}},
		})
		toolErr := requireCommsEnvelopeError(t, resp, err)
		assertEnvelopeError(t, toolErr.GetCode(), toolErr.GetMessage(), "not_found", `not_found: store: not found: channel "relay-envelope-missing-channel"`)
	})

	t.Run("comms_invalid_argument", func(t *testing.T) {
		resp, err := w.supervisorClient.Comms(w.ctx, &compassv1internal.CommsCallRequest{
			CallId: "comms-invalid-argument",
		})
		toolErr := requireCommsEnvelopeError(t, resp, err)
		assertEnvelopeError(t, toolErr.GetCode(), toolErr.GetMessage(), "invalid_argument", "invalid_argument: runnerhub: comms call has no recognized variant set (post/list/list_topics/roster/set_status/pin/create_channel/update_members/create_channel_group/open_dm)")
	})
}

func testBoardEnvelopeErrors(t *testing.T, w *e2eWire) {
	t.Helper()
	t.Run("board_not_found", func(t *testing.T) {
		resp, err := w.supervisorClient.Board(w.ctx, &compassv1internal.BoardCallRequest{
			CallId: "board-not-found",
			Call: &compassv1internal.BoardCallRequest_SetIssueState{SetIssueState: &compassv1internal.SetIssueStateRequest{
				IssueId: "relay-envelope-missing-issue",
				State:   compassv1.IssueState_ISSUE_STATE_DONE,
			}},
		})
		toolErr := requireBoardEnvelopeError(t, resp, err)
		assertEnvelopeError(t, toolErr.GetCode(), toolErr.GetMessage(), "not_found", `not_found: store: not found: issue "relay-envelope-missing-issue" does not exist`)
	})

	t.Run("board_invalid_argument", func(t *testing.T) {
		resp, err := w.supervisorClient.Board(w.ctx, &compassv1internal.BoardCallRequest{
			CallId: "board-invalid-argument",
			Call: &compassv1internal.BoardCallRequest_SetIssueState{SetIssueState: &compassv1internal.SetIssueStateRequest{
				IssueId: "relay-envelope-any-issue",
				State:   compassv1.IssueState_ISSUE_STATE_UNSPECIFIED,
			}},
		})
		toolErr := requireBoardEnvelopeError(t, resp, err)
		assertEnvelopeError(t, toolErr.GetCode(), toolErr.GetMessage(), "invalid_argument", "invalid_argument: issue state target is UNSPECIFIED")
	})
}

func testForgeEnvelopeErrors(t *testing.T, w *e2eWire) {
	t.Helper()
	t.Run("forge_not_found", func(t *testing.T) {
		resp, err := w.supervisorClient.Forge(w.ctx, &compassv1internal.ForgeCallRequest{
			CallId: "forge-not-found",
			Forge:  &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITLAB, Host: "gitlab.example"},
			Call: &compassv1internal.ForgeCallRequest_CreateIssue{CreateIssue: &compassv1internal.CreateIssueRequest{
				Repo:  "owner/repo",
				Title: "not dispatched",
			}},
		})
		toolErr := requireForgeEnvelopeError(t, resp, err)
		assertEnvelopeError(t, toolErr.GetCode(), toolErr.GetMessage(), "not_found", "forge: no provider configured for the requested coordinate")
	})

	t.Run("forge_invalid_argument", func(t *testing.T) {
		resp, err := w.supervisorClient.Forge(w.ctx, &compassv1internal.ForgeCallRequest{
			CallId: "forge-invalid-argument",
			Call: &compassv1internal.ForgeCallRequest_CreateIssue{CreateIssue: &compassv1internal.CreateIssueRequest{
				Title: "missing repo",
			}},
		})
		toolErr := requireForgeEnvelopeError(t, resp, err)
		assertEnvelopeError(t, toolErr.GetCode(), toolErr.GetMessage(), "invalid_argument", "forge: repo is required")
	})
}

func testLifecycleEnvelopeErrors(t *testing.T, w *e2eWire) {
	t.Helper()
	t.Run("lifecycle_not_found", func(t *testing.T) {
		resp, err := w.supervisorClient.Lifecycle(w.ctx, &compassv1internal.LifecycleCallRequest{
			CallId: "lifecycle-not-found",
			Call: &compassv1internal.LifecycleCallRequest_Despawn{Despawn: &compassv1internal.DespawnPeerRequest{
				AgentHandle: "relay-envelope-missing-peer",
			}},
		})
		toolErr := requireLifecycleEnvelopeError(t, resp, err)
		assertEnvelopeError(t, toolErr.GetCode(), toolErr.GetMessage(), "not_found", "not_found: peer not found")
	})

	t.Run("lifecycle_invalid_argument", func(t *testing.T) {
		resp, err := w.supervisorClient.Lifecycle(w.ctx, &compassv1internal.LifecycleCallRequest{
			CallId: "lifecycle-invalid-argument",
			Call: &compassv1internal.LifecycleCallRequest_Despawn{Despawn: &compassv1internal.DespawnPeerRequest{
				AgentHandle: "supervisor",
			}},
		})
		toolErr := requireLifecycleEnvelopeError(t, resp, err)
		assertEnvelopeError(t, toolErr.GetCode(), toolErr.GetMessage(), "invalid_argument", "invalid_argument: cannot despawn self")
	})
}

func requireCommsEnvelopeError(t *testing.T, resp *compassv1internal.CommsCallResult, err error) *compassv1internal.CommsCallError {
	t.Helper()
	if err != nil {
		t.Fatalf("AgentGateway.Comms returned transport error: %v", err)
	}
	if toolErr := resp.GetError(); toolErr != nil {
		return toolErr
	}
	t.Fatal("AgentGateway.Comms returned no in-band error")
	return nil
}

func requireBoardEnvelopeError(t *testing.T, resp *compassv1internal.BoardCallResult, err error) *compassv1internal.BoardCallError {
	t.Helper()
	if err != nil {
		t.Fatalf("AgentGateway.Board returned transport error: %v", err)
	}
	if toolErr := resp.GetError(); toolErr != nil {
		return toolErr
	}
	t.Fatal("AgentGateway.Board returned no in-band error")
	return nil
}

func requireForgeEnvelopeError(t *testing.T, resp *compassv1internal.ForgeCallResult, err error) *compassv1internal.ForgeCallError {
	t.Helper()
	if err != nil {
		t.Fatalf("AgentGateway.Forge returned transport error: %v", err)
	}
	if toolErr := resp.GetError(); toolErr != nil {
		return toolErr
	}
	t.Fatal("AgentGateway.Forge returned no in-band error")
	return nil
}

func requireLifecycleEnvelopeError(t *testing.T, resp *compassv1internal.LifecycleCallResult, err error) *compassv1internal.LifecycleCallError {
	t.Helper()
	if err != nil {
		t.Fatalf("AgentGateway.Lifecycle returned transport error: %v", err)
	}
	if toolErr := resp.GetError(); toolErr != nil {
		return toolErr
	}
	t.Fatal("AgentGateway.Lifecycle returned no in-band error")
	return nil
}

func assertEnvelopeError(t *testing.T, gotCode, gotMessage, wantCode, wantMessage string) {
	t.Helper()
	if gotCode != wantCode {
		t.Fatalf("in-band error code = %q, want %q", gotCode, wantCode)
	}
	if gotMessage != wantMessage {
		t.Fatalf("in-band error message = %q, want %q", gotMessage, wantMessage)
	}
}
