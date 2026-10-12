//go:build pgtest

package comms

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/store"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestGetRosterBoardSummaryCountsOperatorGatesAndStaleness(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "board-summary-owner")
	agent := mustAgent(t, st, owner.ID, "board-summary-agent")
	fresh := mustAgent(t, st, owner.ID, "board-summary-fresh")
	oldOffline := mustAgent(t, st, owner.ID, "board-summary-old-offline")

	const writtenAt = int64(1_700_000_000_000)
	prs := []*compassv1.PullRequest{
		{Forge: &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com"}, Repo: "owner/repo", Number: 12, Title: "Review me", ForgeState: "open", Checks: &compassv1.ChecksSummary{State: "success"}},
		{Forge: &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com"}, Repo: "owner/repo", Number: 13, Title: "Queue me", ForgeState: "open", HeadSha: "head-13", Checks: &compassv1.ChecksSummary{State: "success"}, Reviews: []*compassv1.Review{{Author: "reviewer", Verdict: "approved", CommitId: "head-13"}}},
	}
	prAt := time.Now().UTC()
	for _, pr := range prs {
		prJSON, err := protojson.Marshal(pr)
		if err != nil {
			t.Fatalf("marshal pull request %d: %v", pr.GetNumber(), err)
		}
		if _, err := st.UpsertPullRequest(ctx, store.PullRequestRow{
			Coord: store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: uint64(pr.GetNumber())},
			State: "open", CreatedAt: prAt, UpdatedAt: prAt, PR: prJSON,
		}, nil); err != nil {
			t.Fatalf("UpsertPullRequest(%d): %v", pr.GetNumber(), err)
		}
	}
	state := compassv1.BoardTaskState_BOARD_TASK_STATE_ACTIVE
	for _, target := range []struct {
		account store.Account
		patch   *compassv1.UpdateAgentBoardRequest
	}{
		{account: agent, patch: &compassv1.UpdateAgentBoardRequest{
			UpsertTasks: []*compassv1.BoardTaskPatch{
				{Key: "focus", Title: new("Review task"), PrimaryRef: new("owner/repo#12"), State: &state,
					Waiting: &compassv1.BoardWaitingOp{Values: []*compassv1.BoardWaiting{{On: compassv1.BoardWaitingOn_BOARD_WAITING_ON_OPERATOR}}}},
				{Key: "enqueue", Title: new("Enqueue task"), PrimaryRef: new("owner/repo#13"), State: &state},
				{Key: "operator", Title: new("Operator task"), State: &state, Waiting: &compassv1.BoardWaitingOp{Values: []*compassv1.BoardWaiting{{On: compassv1.BoardWaitingOn_BOARD_WAITING_ON_OPERATOR}}}},
			}, FocusTaskKey: new("focus"), TurnSequence: 1,
		}},
		{account: fresh, patch: &compassv1.UpdateAgentBoardRequest{UpsertTasks: []*compassv1.BoardTaskPatch{{Key: "fresh", Title: new("Fresh board"), State: &state}}}},
		{account: oldOffline, patch: &compassv1.UpdateAgentBoardRequest{UpsertTasks: []*compassv1.BoardTaskPatch{{Key: "old", Title: new("Old offline board"), State: &state}}}},
	} {
		if _, err := svc.UpdateBoardAsAccount(ctx, target.account.ID, target.patch); err != nil {
			t.Fatalf("UpdateBoardAsAccount(%s): %v", target.account.Handle, err)
		}
	}
	if _, err := st.ApplyBoardPatch(ctx, agent.ID, store.BoardPatch{NowUnixMs: writtenAt}); err != nil {
		t.Fatalf("ApplyBoardPatch(at old time): %v", err)
	}
	if _, err := st.ApplyBoardPatch(ctx, oldOffline.ID, store.BoardPatch{NowUnixMs: writtenAt}); err != nil {
		t.Fatalf("ApplyBoardPatch(old offline at old time): %v", err)
	}
	svc.SetPresenceSource(fakePresenceSource{presence: map[store.AccountID]compassv1.AgentPresence{
		agent.ID: compassv1.AgentPresence_AGENT_PRESENCE_WORKING,
		fresh.ID: compassv1.AgentPresence_AGENT_PRESENCE_WORKING,
	}})
	svc.SetBoardForges(board.Forges{GitHubHost: "github.com"})
	resp, err := svc.GetRoster(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.GetRosterRequest{
		Scope: compassv1.RosterScope_ROSTER_SCOPE_OWNER, VantageHandle: agent.Handle,
	}))
	if err != nil {
		t.Fatalf("GetRoster: %v", err)
	}
	entries := rosterByID(resp.Msg.GetEntries())
	summary := entries[string(agent.ID)].GetBoard()
	if summary == nil || summary.GetFocus() != "owner/repo#12 Review task" || summary.GetTaskCount() != 3 || summary.GetWaitingOnOperator() != 3 {
		t.Fatalf("board summary = %v, want focus, three tasks, and three distinct gate waits", summary)
	}
	if summary.GetWrittenAtUnixMs() != writtenAt || !summary.GetStale() {
		t.Fatalf("working board freshness = %v, want written_at %d and stale=true", summary, writtenAt)
	}
	if got := entries[string(fresh.ID)].GetBoard(); got == nil || got.GetStale() {
		t.Fatalf("fresh working board = %v, want stale=false", got)
	}
	if got := entries[string(oldOffline.ID)].GetBoard(); got == nil || got.GetStale() {
		t.Fatalf("old offline board = %v, want stale=false", got)
	}
}

func TestBoardSummaryStalenessUsesStrictThirtyMinuteBoundary(t *testing.T) {
	svc, st := newHandler(t)
	ctx := t.Context()
	owner := mustUser(t, st, "board-summary-boundary-owner")
	accounts := []store.Account{
		mustAgent(t, st, owner.ID, "board-summary-29-minutes"),
		mustAgent(t, st, owner.ID, "board-summary-31-minutes"),
	}
	for _, account := range accounts {
		if _, err := svc.UpdateBoardAsAccount(ctx, account.ID, &compassv1.UpdateAgentBoardRequest{
			UpsertTasks: []*compassv1.BoardTaskPatch{{Key: "task", Title: new("Task")}},
		}); err != nil {
			t.Fatalf("UpdateBoardAsAccount(%s): %v", account.Handle, err)
		}
	}
	if _, err := st.ApplyBoardPatch(ctx, accounts[0].ID, store.BoardPatch{NowUnixMs: time.Now().Add(-29 * time.Minute).UnixMilli()}); err != nil {
		t.Fatalf("ApplyBoardPatch(29 minutes): %v", err)
	}
	if _, err := st.ApplyBoardPatch(ctx, accounts[1].ID, store.BoardPatch{NowUnixMs: time.Now().Add(-31 * time.Minute).UnixMilli()}); err != nil {
		t.Fatalf("ApplyBoardPatch(31 minutes): %v", err)
	}
	svc.SetPresenceSource(fakePresenceSource{presence: map[store.AccountID]compassv1.AgentPresence{
		accounts[0].ID: compassv1.AgentPresence_AGENT_PRESENCE_WORKING,
		accounts[1].ID: compassv1.AgentPresence_AGENT_PRESENCE_WORKING,
	}})
	resp, err := svc.GetRoster(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.GetRosterRequest{
		Scope: compassv1.RosterScope_ROSTER_SCOPE_OWNER,
	}))
	if err != nil {
		t.Fatalf("GetRoster: %v", err)
	}
	entries := rosterByID(resp.Msg.GetEntries())
	for i, wantStale := range []bool{false, true} {
		summary := entries[string(accounts[i].ID)].GetBoard()
		if summary == nil {
			t.Fatalf("%d-minute board summary is missing", []int{29, 31}[i])
		}
		if summary.GetStale() != wantStale {
			t.Fatalf("%d-minute board stale = %t, want %t", []int{29, 31}[i], summary.GetStale(), wantStale)
		}
	}
}

func TestGetRosterKeepsSummariesWhenBoardRefDerivationFails(t *testing.T) {
	svc, st := newHandler(t)
	ctx := t.Context()
	owner := mustUser(t, st, "board-summary-isolation-owner")
	bad := mustAgent(t, st, owner.ID, "board-summary-isolation-bad")
	good := mustAgent(t, st, owner.ID, "board-summary-isolation-good")
	if _, err := svc.UpdateBoardAsAccount(ctx, bad.ID, &compassv1.UpdateAgentBoardRequest{
		UpsertTasks: []*compassv1.BoardTaskPatch{{Key: "bad", Title: new("Bad board"), PrimaryRef: new("owner/repo#12")}},
	}); err != nil {
		t.Fatalf("UpdateBoardAsAccount(bad): %v", err)
	}
	if _, err := st.ApplyBoardPatch(ctx, good.ID, store.BoardPatch{NowUnixMs: 1}); err != nil {
		t.Fatalf("ApplyBoardPatch(good): %v", err)
	}
	if _, err := st.UpsertPullRequest(ctx, store.PullRequestRow{
		Coord: store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: 12},
		State: "open", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), PR: []byte(`{"number":"invalid"}`),
	}, nil); err != nil {
		t.Fatalf("UpsertPullRequest(invalid protobuf JSON): %v", err)
	}
	svc.SetBoardForges(board.Forges{GitHubHost: "github.com"})
	resp, err := svc.GetRoster(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.GetRosterRequest{
		Scope: compassv1.RosterScope_ROSTER_SCOPE_OWNER,
	}))
	if err != nil {
		t.Fatalf("GetRoster: %v", err)
	}
	entries := rosterByID(resp.Msg.GetEntries())
	badEntry := entries[string(bad.ID)]
	if badEntry == nil || badEntry.GetBoard() == nil || badEntry.GetBoard().GetTaskCount() != 1 {
		t.Fatalf("malformed-ref board entry = %v, want roster row with its stored task summary", badEntry)
	}
	if entries[string(good.ID)] == nil || entries[string(good.ID)].GetBoard() == nil {
		t.Fatalf("valid board entry = %v, want an independent summary", entries[string(good.ID)])
	}
}

func TestBoardSummaryKeepsStoredCountsWhenRefDerivationFails(t *testing.T) {
	svc, st := newHandler(t)
	ctx := t.Context()
	owner := mustUser(t, st, "board-summary-derive-owner")
	agent := mustAgent(t, st, owner.ID, "board-summary-derive-agent")
	if _, err := svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{
		UpsertTasks: []*compassv1.BoardTaskPatch{{Key: "task", Title: new("Stored task"), PrimaryRef: new("owner/repo#12"), Waiting: &compassv1.BoardWaitingOp{Values: []*compassv1.BoardWaiting{{On: compassv1.BoardWaitingOn_BOARD_WAITING_ON_OPERATOR}}}}},
	}); err != nil {
		t.Fatalf("UpdateBoardAsAccount: %v", err)
	}
	if _, err := st.ApplyBoardPatch(ctx, agent.ID, store.BoardPatch{NowUnixMs: time.Now().Add(-31 * time.Minute).UnixMilli()}); err != nil {
		t.Fatalf("ApplyBoardPatch(stale timestamp): %v", err)
	}
	if _, err := st.UpsertPullRequest(ctx, store.PullRequestRow{
		Coord: store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: 12},
		State: "open", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), PR: []byte(`{"number":"invalid"}`),
	}, nil); err != nil {
		t.Fatalf("UpsertPullRequest(invalid protobuf JSON): %v", err)
	}
	svc.SetBoardForges(board.Forges{GitHubHost: "github.com"})
	svc.SetPresenceSource(fakePresenceSource{presence: map[store.AccountID]compassv1.AgentPresence{
		agent.ID: compassv1.AgentPresence_AGENT_PRESENCE_WORKING,
	}})
	resp, err := svc.GetRoster(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.GetRosterRequest{
		Scope: compassv1.RosterScope_ROSTER_SCOPE_OWNER,
	}))
	if err != nil {
		t.Fatalf("GetRoster: %v", err)
	}
	summary := rosterByID(resp.Msg.GetEntries())[string(agent.ID)].GetBoard()
	if summary == nil || summary.GetTaskCount() != 1 || summary.GetWaitingOnOperator() != 1 || !summary.GetStale() {
		t.Fatalf("stored summary after ref derivation failure = %v, want one stale operator-waiting task", summary)
	}
}
