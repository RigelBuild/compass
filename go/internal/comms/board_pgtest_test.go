//go:build pgtest

package comms

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestUpdateAgentBoardPresenceMergeAndInvalidState(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "board-owner")
	agent := mustAgent(t, st, owner.ID, "board-agent")
	state := compassv1.BoardTaskState_BOARD_TASK_STATE_ACTIVE

	first, err := svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{
		UpsertTasks: []*compassv1.BoardTaskPatch{{
			Key: "task", Title: new("Board task"), Note: new("first note"), State: &state,
			Refs: &compassv1.BoardRefsOp{Values: []string{"RIG-12"}},
		}},
		FocusTaskKey: new("task"), TurnSequence: 1,
	})
	if err != nil {
		t.Fatalf("initial UpdateBoardAsAccount: %v", err)
	}
	if len(first.GetBoard().GetTasks()) != 1 || first.GetBoard().GetTasks()[0].GetRefs()[0] != "RIG-12" {
		t.Fatalf("initial board = %v", first.GetBoard())
	}

	unspecified := compassv1.BoardTaskState_BOARD_TASK_STATE_UNSPECIFIED
	_, err = svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{
		UpsertTasks: []*compassv1.BoardTaskPatch{{Key: "task", State: &unspecified}},
	})
	connectCodeIs(t, err, connect.CodeInvalidArgument, "unspecified state")
	rows, err := st.AgentBoards(ctx, []store.AccountID{agent.ID})
	if err != nil {
		t.Fatalf("AgentBoards after invalid state: %v", err)
	}
	if got := rows[agent.ID].Tasks[0].State; got != int16(compassv1.BoardTaskState_BOARD_TASK_STATE_ACTIVE) {
		t.Fatalf("invalid state changed stored state to %d", got)
	}
	unknown := compassv1.BoardTaskState(math.MaxInt32)
	_, err = svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{
		UpsertTasks: []*compassv1.BoardTaskPatch{{Key: "task", State: &unknown}},
	})
	connectCodeIs(t, err, connect.CodeInvalidArgument, "unknown state")
	rows, err = st.AgentBoards(ctx, []store.AccountID{agent.ID})
	if err != nil {
		t.Fatalf("AgentBoards after unknown state: %v", err)
	}
	if got := rows[agent.ID].Tasks[0].State; got != int16(compassv1.BoardTaskState_BOARD_TASK_STATE_ACTIVE) {
		t.Fatalf("unknown state changed stored state to %d", got)
	}
	clear := ""
	emptyRefs := &compassv1.BoardRefsOp{}
	merged, err := svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{
		UpsertTasks: []*compassv1.BoardTaskPatch{{Key: "task", Note: &clear, Refs: emptyRefs}}, TurnSequence: 2,
	})
	if err != nil {
		t.Fatalf("presence-aware UpdateBoardAsAccount: %v", err)
	}
	task := merged.GetBoard().GetTasks()[0]
	if task.GetNote() != "" || len(task.GetRefs()) != 0 || task.GetTitle() != "Board task" {
		t.Fatalf("merged task = %v, want empty note/refs and kept title", task)
	}
}

func TestUpdateBoardWaitingRefsCanonicalAndValidated(t *testing.T) {
	svc, st := newHandler(t)
	ctx := t.Context()
	owner := mustUser(t, st, "board-waiting-ref-owner")
	agent := mustAgent(t, st, owner.ID, "board-waiting-ref-agent")
	on := compassv1.BoardWaitingOn_BOARD_WAITING_ON_EXTERNAL
	_, err := svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{
		UpsertTasks: []*compassv1.BoardTaskPatch{{
			Key: "task", Title: new("Waiting task"),
			Waiting: &compassv1.BoardWaitingOp{Values: []*compassv1.BoardWaiting{{
				On: on, Ref: "Owner/Repo#12", Blocks: []string{"Other/Repo#13"},
			}}},
		}},
	})
	if err != nil {
		t.Fatalf("UpdateBoardAsAccount(canonical waiting refs): %v", err)
	}
	rows, err := st.AgentBoards(ctx, []store.AccountID{agent.ID})
	if err != nil {
		t.Fatalf("AgentBoards: %v", err)
	}
	waiting, err := unmarshalBoardWaiting(rows[agent.ID].Tasks[0].Waiting)
	if err != nil {
		t.Fatalf("unmarshalBoardWaiting: %v", err)
	}
	if waiting[0].GetRef() != "owner/repo#12" || len(waiting[0].GetBlocks()) != 1 || waiting[0].GetBlocks()[0] != "other/repo#13" {
		t.Fatalf("stored waiting refs = %v, want canonical refs", waiting[0])
	}

	for _, tc := range []struct {
		name string
		item *compassv1.BoardWaiting
	}{
		{name: "ref", item: &compassv1.BoardWaiting{On: on, Ref: "invalid ref"}},
		{name: "blocks", item: &compassv1.BoardWaiting{On: on, Blocks: []string{"invalid ref"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{
				UpsertTasks: []*compassv1.BoardTaskPatch{{Key: "task", Waiting: &compassv1.BoardWaitingOp{Values: []*compassv1.BoardWaiting{tc.item}}}},
			})
			connectCodeIs(t, err, connect.CodeInvalidArgument, "invalid waiting "+tc.name)
		})
	}
}

func TestUpdateBoardRejectsUnspecifiedHalt(t *testing.T) {
	svc, st := newHandler(t)
	owner := mustUser(t, st, "board-unspecified-halt-owner")
	agent := mustAgent(t, st, owner.ID, "board-unspecified-halt-agent")
	_, err := svc.UpdateBoardAsAccount(t.Context(), agent.ID, &compassv1.UpdateAgentBoardRequest{
		Halt: &compassv1.BoardHalt{Kind: compassv1.BoardHaltKind_BOARD_HALT_KIND_UNSPECIFIED},
	})
	connectCodeIs(t, err, connect.CodeInvalidArgument, "present unspecified halt")
	rows, err := st.AgentBoards(t.Context(), []store.AccountID{agent.ID})
	if err != nil {
		t.Fatalf("AgentBoards after rejected halt: %v", err)
	}
	if _, ok := rows[agent.ID]; ok {
		t.Fatalf("unspecified halt created a board: %+v", rows[agent.ID])
	}
}

func TestUpdateBoardRejectsConflictingHaltOperations(t *testing.T) {
	svc, st := newHandler(t)
	owner := mustUser(t, st, "board-halt-owner")
	agent := mustAgent(t, st, owner.ID, "board-halt-agent")
	_, err := svc.UpdateBoardAsAccount(t.Context(), agent.ID, &compassv1.UpdateAgentBoardRequest{
		Halt: &compassv1.BoardHalt{Kind: compassv1.BoardHaltKind_BOARD_HALT_KIND_BLOCKED}, ClearHalt: true,
	})
	connectCodeIs(t, err, connect.CodeInvalidArgument, "conflicting halt and clear_halt")
	rows, err := st.AgentBoards(t.Context(), []store.AccountID{agent.ID})
	if err != nil {
		t.Fatalf("AgentBoards after rejected patch: %v", err)
	}
	if _, ok := rows[agent.ID]; ok {
		t.Fatalf("conflicting halt patch created board: %+v", rows[agent.ID])
	}
}

func TestListAgentBoardsRejectsInvalidFilters(t *testing.T) {
	svc, st := newHandler(t)
	owner := mustUser(t, st, "board-invalid-filter-owner")
	filters := []struct {
		name string
		req  *compassv1.ListAgentBoardsRequest
	}{
		{name: "waiting enum", req: &compassv1.ListAgentBoardsRequest{WaitingOn: compassv1.BoardWaitingOn(99)}},
		{name: "state enum", req: &compassv1.ListAgentBoardsRequest{State: compassv1.BoardTaskState(99)}},
		{name: "halt enum", req: &compassv1.ListAgentBoardsRequest{Halt: compassv1.BoardHaltKind(99)}},
		{name: "ref grammar", req: &compassv1.ListAgentBoardsRequest{Ref: "not a ref"}},
		{name: "query NUL", req: &compassv1.ListAgentBoardsRequest{Query: "a\x00b"}},
		{name: "handle NUL", req: &compassv1.ListAgentBoardsRequest{AgentHandles: []string{"a\x00b"}}},
	}
	for _, tc := range filters {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.ListBoardsAsAccount(t.Context(), owner.ID, tc.req)
			connectCodeIs(t, err, connect.CodeInvalidArgument, tc.name)
		})
	}
}

func TestUpdateBoardRejectsNonAgentBeforeWrite(t *testing.T) {
	svc, st := newHandler(t)
	owner := mustUser(t, st, "board-non-agent")
	_, err := svc.UpdateBoardAsAccount(context.Background(), owner.ID, &compassv1.UpdateAgentBoardRequest{})
	connectCodeIs(t, err, connect.CodePermissionDenied, "non-agent board writer")
	rows, err := st.AgentBoards(context.Background(), []store.AccountID{owner.ID})
	if err != nil {
		t.Fatalf("AgentBoards after rejected write: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("non-agent write created board: %+v", rows[owner.ID])
	}
}

func TestListAgentBoardsVisibilityAndFilters(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "board-visible-owner")
	stranger := mustUser(t, st, "board-visible-stranger")
	agent := mustAgent(t, st, owner.ID, "board-visible-agent")
	peer := mustChildAgent(t, st, owner.ID, "board-visible-peer", agent.ID)
	state := compassv1.BoardTaskState_BOARD_TASK_STATE_ACTIVE
	blocked := compassv1.BoardTaskState_BOARD_TASK_STATE_BLOCKED
	waitingOn := compassv1.BoardWaitingOn_BOARD_WAITING_ON_OPERATOR
	if _, err := svc.UpdateBoardAsAccount(ctx, peer.ID, &compassv1.UpdateAgentBoardRequest{
		UpsertTasks: []*compassv1.BoardTaskPatch{
			{
				Key: "peer-task", Title: new("Needs operator review"), Note: new("deploy release"), State: &blocked,
				PrimaryRef: new("RIG-42"), Waiting: &compassv1.BoardWaitingOp{Values: []*compassv1.BoardWaiting{{On: waitingOn}}},
			},
			{Key: "other-task", Title: new("Unrelated work"), Note: new("do not match"), State: &state},
		},
		Halt: &compassv1.BoardHalt{Kind: compassv1.BoardHaltKind_BOARD_HALT_KIND_BLOCKED},
	}); err != nil {
		t.Fatalf("UpdateBoardAsAccount(peer): %v", err)
	}
	if _, err := svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{
		UpsertTasks: []*compassv1.BoardTaskPatch{{Key: "task", Title: new("Visible task"), State: &state}},
	}); err != nil {
		t.Fatalf("UpdateBoardAsAccount: %v", err)
	}
	visible, err := svc.ListBoardsAsAccount(ctx, peer.ID, &compassv1.ListAgentBoardsRequest{})
	if err != nil {
		t.Fatalf("peer ListBoardsAsAccount: %v", err)
	}
	if len(visible.GetBoards()) != 2 {
		t.Fatalf("peer boards = %v, want both same-owner roster-visible agents", visible.GetBoards())
	}
	if !slices.ContainsFunc(visible.GetBoards(), func(got *compassv1.AgentBoard) bool {
		return got.GetAgentAccountId() == string(agent.ID)
	}) {
		t.Fatalf("peer boards omitted roster-visible agent %q: %v", agent.ID, visible.GetBoards())
	}
	qualifiedVisible, err := svc.ListBoardsAsAccount(ctx, peer.ID, &compassv1.ListAgentBoardsRequest{AgentHandles: []string{owner.Handle + "/" + agent.Handle}})
	if err != nil || len(qualifiedVisible.GetBoards()) != 1 || qualifiedVisible.GetBoards()[0].GetAgentAccountId() != string(agent.ID) {
		t.Fatalf("qualified handle filter = %v, %v; want visible target agent", qualifiedVisible, err)
	}
	filtered, err := svc.ListBoardsAsAccount(ctx, peer.ID, &compassv1.ListAgentBoardsRequest{AgentHandles: []string{"missing-agent"}})
	if err != nil {
		t.Fatalf("filtered ListBoardsAsAccount: %v", err)
	}
	if len(filtered.GetBoards()) != 0 {
		t.Fatalf("handle filter returned boards: %v", filtered.GetBoards())
	}
	filters := []struct {
		name        string
		req         *compassv1.ListAgentBoardsRequest
		matchedKeys []string
	}{
		{name: "query", req: &compassv1.ListAgentBoardsRequest{Query: "deploy"}, matchedKeys: []string{"peer-task"}},
		{name: "ref", req: &compassv1.ListAgentBoardsRequest{Ref: "RIG-42"}, matchedKeys: []string{"peer-task"}},
		{name: "waiting", req: &compassv1.ListAgentBoardsRequest{WaitingOn: waitingOn}, matchedKeys: []string{"peer-task"}},
		{name: "state", req: &compassv1.ListAgentBoardsRequest{State: blocked}, matchedKeys: []string{"peer-task"}},
		{name: "halt", req: &compassv1.ListAgentBoardsRequest{Halt: compassv1.BoardHaltKind_BOARD_HALT_KIND_BLOCKED}, matchedKeys: []string{"peer-task", "other-task"}},
	}
	for _, tc := range filters {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.ListBoardsAsAccount(ctx, peer.ID, tc.req)
			if err != nil {
				t.Fatalf("ListBoardsAsAccount(%s): %v", tc.name, err)
			}
			if len(got.GetBoards()) != 1 || got.GetBoards()[0].GetAgentAccountId() != string(peer.ID) {
				t.Fatalf("%s filter returned boards %v, want peer board", tc.name, got.GetBoards())
			}
			board := got.GetBoards()[0]
			if !slices.Equal(board.GetMatchedTaskKeys(), tc.matchedKeys) {
				t.Errorf("%s matched task keys = %v, want %v", tc.name, board.GetMatchedTaskKeys(), tc.matchedKeys)
			}
			if len(board.GetTasks()) != 2 || board.GetTasks()[1].GetKey() != "other-task" {
				t.Errorf("%s board tasks = %v, want whole two-task board", tc.name, board.GetTasks())
			}
		})
	}
	invisible, err := svc.ListBoardsAsAccount(ctx, stranger.ID, &compassv1.ListAgentBoardsRequest{})
	if err != nil {
		t.Fatalf("stranger ListBoardsAsAccount: %v", err)
	}
	if len(invisible.GetBoards()) != 0 {
		t.Fatalf("stranger saw invisible agent board: %v", invisible.GetBoards())
	}
}

func TestListAgentBoardsIgnoresUnknownHandles(t *testing.T) {
	svc, st := newHandler(t)
	ctx := t.Context()
	owner := mustUser(t, st, "board-unknown-handle-owner")
	agent := mustAgent(t, st, owner.ID, "board-unknown-handle-agent")
	if _, err := svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{
		UpsertTasks: []*compassv1.BoardTaskPatch{{Key: "task", Title: new("Task")}},
	}); err != nil {
		t.Fatalf("UpdateBoardAsAccount: %v", err)
	}
	got, err := svc.ListBoardsAsAccount(ctx, owner.ID, &compassv1.ListAgentBoardsRequest{
		AgentHandles: []string{"not-an-agent", agent.Handle},
	})
	if err != nil {
		t.Fatalf("ListBoardsAsAccount with unknown handle: %v", err)
	}
	if len(got.GetBoards()) != 1 || got.GetBoards()[0].GetAgentAccountId() != string(agent.ID) {
		t.Fatalf("ListBoardsAsAccount returned %v, want only the visible agent board", got.GetBoards())
	}
}

func TestListAgentBoardsSurfacesResolverOperationalErrors(t *testing.T) {
	svc, st := newHandler(t)
	owner := mustUser(t, st, "board-resolver-error-owner")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := svc.ListBoardsAsAccount(ctx, owner.ID, &compassv1.ListAgentBoardsRequest{AgentHandles: []string{"possibly-an-agent"}})
	connectCodeIs(t, err, connect.CodeInternal, "canceled handle resolution")
}

func TestListAgentBoardsDeduplicatesNonadjacentHandles(t *testing.T) {
	svc, st := newHandler(t)
	ctx := t.Context()
	owner := mustUser(t, st, "board-handle-dedupe-owner")
	first := mustAgent(t, st, owner.ID, "board-handle-dedupe-first")
	second := mustAgent(t, st, owner.ID, "board-handle-dedupe-second")
	for _, agent := range []store.Account{first, second} {
		if _, err := svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{
			UpsertTasks: []*compassv1.BoardTaskPatch{{Key: "task", Title: new(agent.Handle)}},
		}); err != nil {
			t.Fatalf("UpdateBoardAsAccount(%s): %v", agent.Handle, err)
		}
	}
	got, err := svc.ListBoardsAsAccount(ctx, owner.ID, &compassv1.ListAgentBoardsRequest{
		AgentHandles: []string{first.Handle, second.Handle, first.Handle},
	})
	if err != nil {
		t.Fatalf("ListBoardsAsAccount: %v", err)
	}
	if len(got.GetBoards()) != 2 {
		t.Fatalf("boards = %v, want both matching boards once", got.GetBoards())
	}
}

func TestListAgentBoardsRejectsMoreThan100UniqueHandles(t *testing.T) {
	svc, st := newHandler(t)
	owner := mustUser(t, st, "board-handle-limit-owner")
	handles := make([]string, 101)
	for i := range handles {
		handles[i] = fmt.Sprintf("missing-agent-%d", i)
	}
	_, err := svc.ListBoardsAsAccount(t.Context(), owner.ID, &compassv1.ListAgentBoardsRequest{AgentHandles: handles})
	connectCodeIs(t, err, connect.CodeInvalidArgument, "maximum")
}
func TestListAgentBoardsRejectsMoreThan100RawHandles(t *testing.T) {
	svc, st := newHandler(t)
	owner := mustUser(t, st, "board-raw-handle-limit-owner")
	handles := make([]string, 101)
	for i := range handles {
		handles[i] = "same-missing-agent"
	}
	_, err := svc.ListBoardsAsAccount(t.Context(), owner.ID, &compassv1.ListAgentBoardsRequest{AgentHandles: handles})
	connectCodeIs(t, err, connect.CodeInvalidArgument, "maximum")
}

func TestListAgentBoardsIncludesEmptyHaltedBoards(t *testing.T) {
	svc, st := newHandler(t)
	ctx := t.Context()
	owner := mustUser(t, st, "board-empty-halted-owner")
	agent := mustAgent(t, st, owner.ID, "board-empty-halted-agent")
	if _, err := st.ApplyBoardPatch(ctx, agent.ID, store.BoardPatch{
		Halt: &store.BoardHalt{Kind: int16(compassv1.BoardHaltKind_BOARD_HALT_KIND_BLOCKED)}, NowUnixMs: 1,
	}); err != nil {
		t.Fatalf("ApplyBoardPatch(empty halted): %v", err)
	}
	resp, err := svc.ListBoardsAsAccount(ctx, owner.ID, &compassv1.ListAgentBoardsRequest{
		Halt: compassv1.BoardHaltKind_BOARD_HALT_KIND_BLOCKED,
	})
	if err != nil {
		t.Fatalf("ListBoardsAsAccount: %v", err)
	}
	if len(resp.GetBoards()) != 1 || len(resp.GetBoards()[0].GetTasks()) != 0 || resp.GetBoards()[0].GetHalt().GetKind() != compassv1.BoardHaltKind_BOARD_HALT_KIND_BLOCKED {
		t.Fatalf("halted empty board result = %v, want one zero-task halted board", resp.GetBoards())
	}
}

func TestListAgentBoardsRejectsInvalidHaltFilter(t *testing.T) {
	svc, st := newHandler(t)
	owner := mustUser(t, st, "board-invalid-halt-filter-owner")
	_, err := svc.ListBoardsAsAccount(t.Context(), owner.ID, &compassv1.ListAgentBoardsRequest{
		Halt: compassv1.BoardHaltKind(math.MaxInt32),
	})
	connectCodeIs(t, err, connect.CodeInvalidArgument, "invalid board halt filter")
}

func TestAutomaticEmptyBoardPatchDoesNotPublish(t *testing.T) {
	h := newStreamHarness(t)
	ctx := t.Context()
	owner := mustUser(t, h.store, "board-empty-auto-owner")
	agent := mustAgent(t, h.store, owner.ID, "board-empty-auto-agent")
	if _, err := h.svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{
		UpsertTasks: []*compassv1.BoardTaskPatch{{Key: "task", Title: new("Existing task")}},
	}); err != nil {
		t.Fatalf("seed board: %v", err)
	}
	before, err := h.bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe(before): %v", err)
	}
	defer before.Cancel()
	if _, err := h.svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{Automatic: true}); err != nil {
		t.Fatalf("empty automatic update: %v", err)
	}
	after, err := h.bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe(after): %v", err)
	}
	defer after.Cancel()
	if after.HeadSeq != before.HeadSeq {
		t.Fatalf("event head after empty automatic patch = %d, want unchanged %d", after.HeadSeq, before.HeadSeq)
	}
}

func TestBoardChangedUsesBoardVisibilityAndHasNoAsks(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()
	owner := mustUser(t, h.store, "board-event-owner")
	stranger := mustUser(t, h.store, "board-event-stranger")
	peer := mustUser(t, h.store, "board-event-peer")
	agent := mustAgent(t, h.store, owner.ID, "board-event-agent")
	if _, _, err := h.store.UpdateChannelMembers(ctx, owner.ID, agent.Agent.HomeChannelID, []store.MemberUpdate{{AccountID: peer.ID}}, store.MemberUpdatesOptions{}); err != nil {
		t.Fatalf("UpdateChannelMembers(peer visibility): %v", err)
	}
	state := compassv1.BoardTaskState_BOARD_TASK_STATE_ACTIVE
	stream := firstEventAfterBoundary(t, h, owner.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})
	peerEvents := firstEventAfterBoundary(t, h, peer.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})
	if _, err := h.svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{
		UpsertTasks: []*compassv1.BoardTaskPatch{{Key: "task", Title: new("Task"), PrimaryRef: new("RIG-42"), Note: new("Keep this authored note"), State: &state}},
	}); err != nil {
		t.Fatalf("UpdateBoardAsAccount: %v", err)
	}
	got := awaitFirst(t, stream)
	if peerEvent := awaitFirst(t, peerEvents).GetAgentBoardChanged(); peerEvent == nil || peerEvent.GetAgentAccountId() != string(agent.ID) {
		t.Fatalf("peer-visible event = %v, want board event for agent", peerEvent)
	}
	event := got.GetAgentBoardChanged()
	if event == nil || event.GetBoard() == nil || event.GetAgentAccountId() != string(agent.ID) || event.GetBoard().GetHandle() != agent.Handle {
		t.Fatalf("board event = %v, want a board change for agent %q", got, agent.Handle)
	}
	boardEvent := event.GetBoard()
	if len(boardEvent.GetRefs()) != 0 || len(boardEvent.GetUntrackedRefs()) != 0 || boardEvent.GetStale() || len(boardEvent.GetAsks()) != 0 {
		t.Fatalf("board event contains derived fields: %v", boardEvent)
	}
	if len(boardEvent.GetTasks()) != 1 || boardEvent.GetTasks()[0].GetPrimaryRef() != "RIG-42" || boardEvent.GetTasks()[0].GetNote() != "Keep this authored note" {
		t.Fatalf("board event omitted authored task fields: %v", boardEvent.GetTasks())
	}

	other, err := h.svc.ListBoardsAsAccount(ctx, stranger.ID, &compassv1.ListAgentBoardsRequest{})
	if err != nil {
		t.Fatalf("stranger board list: %v", err)
	}
	if len(other.GetBoards()) != 0 {
		t.Fatalf("stranger saw board: %v", other.GetBoards())
	}
	strangerEvents := firstEventAfterBoundary(t, h, stranger.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})
	if _, err := h.svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{
		UpsertTasks: []*compassv1.BoardTaskPatch{{Key: "task", Note: new("second update")}},
	}); err != nil {
		t.Fatalf("second UpdateBoardAsAccount: %v", err)
	}
	canary, err := h.store.CreateChannel(ctx, stranger.ID, store.NewChannel{Name: "board-event-canary", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel(canary): %v", err)
	}
	h.bus.Publish(&compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_ChannelChanged{
			ChannelChanged: &compassv1.ChannelChanged{Channel: &compassv1.Channel{Id: string(canary.ID)}},
		},
	})
	strangerFirst := awaitFirst(t, strangerEvents)
	peerVisible, err := h.svc.ListBoardsAsAccount(ctx, peer.ID, &compassv1.ListAgentBoardsRequest{})
	if err != nil || len(peerVisible.GetBoards()) != 1 {
		t.Fatalf("peer board list = %v, %v; want roster-visible board", peerVisible, err)
	}
	if strangerFirst.GetAgentBoardChanged() != nil {
		t.Fatalf("stranger received board event: %v", strangerFirst)
	}
	if strangerFirst.GetChannelChanged() == nil || strangerFirst.GetChannelChanged().GetChannel().GetId() != string(canary.ID) {
		t.Fatalf("stranger first event = %v, want visible canary channel", strangerFirst)
	}
}
func TestBoardChangedPublishedWhenDerivedResponseFails(t *testing.T) {
	h := newStreamHarness(t)
	ctx := t.Context()
	owner := mustUser(t, h.store, "board-event-failure-owner")
	agent := mustAgent(t, h.store, owner.ID, "board-event-failure-agent")
	if _, err := h.store.UpsertPullRequest(ctx, store.PullRequestRow{
		Coord: store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: 12},
		State: "open", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), PR: []byte(`{"number":"invalid"}`),
	}, nil); err != nil {
		t.Fatalf("UpsertPullRequest(invalid protobuf JSON): %v", err)
	}
	h.svc.SetBoardForges(board.Forges{GitHubHost: "github.com"})
	sub, err := h.bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()
	waitingOn := compassv1.BoardWaitingOn_BOARD_WAITING_ON_OPERATOR
	updated, err := h.svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{
		UpsertTasks: []*compassv1.BoardTaskPatch{{
			Key: "task", Title: new("Authored task"), PrimaryRef: new("owner/repo#12"),
			Waiting:         &compassv1.BoardWaitingOp{Values: []*compassv1.BoardWaiting{{On: waitingOn, What: "waiting"}}},
			UpsertSubagents: []*compassv1.BoardSubagent{{Id: "child", Agent: "implement", Description: "checking", Status: "running"}},
		}},
	})
	if err != nil {
		t.Fatalf("UpdateBoardAsAccount: %v", err)
	}
	assertAuthoredOnlyBoard(t, updated.GetBoard())
	task := updated.GetBoard().GetTasks()[0]
	if len(task.GetWaiting()) != 1 || task.GetWaiting()[0].GetWhat() != "waiting" {
		t.Fatalf("decoded waiting entries were lost after ref derivation failed: %v", task.GetWaiting())
	}
	if len(task.GetSubagents()) != 1 || task.GetSubagents()[0].GetId() != "child" {
		t.Fatalf("decoded subagents were lost after ref derivation failed: %v", task.GetSubagents())
	}
	listed, err := h.svc.ListBoardsAsAccount(ctx, owner.ID, &compassv1.ListAgentBoardsRequest{})
	if err != nil {
		t.Fatalf("ListBoardsAsAccount: %v", err)
	}
	if len(listed.GetBoards()) != 1 {
		t.Fatalf("listed boards = %v, want the authored board", listed.GetBoards())
	}
	assertAuthoredOnlyBoard(t, listed.GetBoards()[0])
	event := <-sub.Live
	if changed := event.Payload.GetAgentBoardChanged(); changed == nil || changed.GetBoard() == nil {
		t.Fatalf("published event = %v, want authored board event", event.Payload)
	} else {
		assertAuthoredOnlyBoard(t, changed.GetBoard())
	}
}

func TestUpdateBoardMalformedStoredSubagentsPreservesLaterTasks(t *testing.T) {
	ctx := context.Background()
	st, dsn := newTestStoreDSN(t)
	owner := mustUser(t, st, "board-malformed-owner")
	agent := mustAgent(t, st, owner.ID, "board-malformed-agent")
	bus := newBus(t)
	svc := NewComms(t.Context(), st, bus, nil, owner.ID)
	if _, err := svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{
		UpsertTasks: []*compassv1.BoardTaskPatch{
			{Key: "first", Title: new("First task")},
			{Key: "task", Title: new("Stored title"), Note: new("Stored note")},
			{Key: "third", Title: new("Third task")},
		},
	}); err != nil {
		t.Fatalf("seed board: %v", err)
	}
	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()
	oldWrittenAt := corruptStoredBoardTask(t, ctx, st, dsn, agent.ID, "task", "subagents")
	svc.SetPresenceSource(fakePresenceSource{presence: map[store.AccountID]compassv1.AgentPresence{
		agent.ID: compassv1.AgentPresence_AGENT_PRESENCE_WORKING,
	}})
	response, err := svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{
		UpsertTasks:  []*compassv1.BoardTaskPatch{{Key: "task", Title: new("Updated title")}},
		FocusTaskKey: new("task"),
		Automatic:    true,
	})
	if err != nil {
		t.Fatalf("UpdateBoardAsAccount after committed patch: %v", err)
	}
	assertMalformedBoardUpdateFallback(t, response, oldWrittenAt)
	event := <-sub.Live
	assertMalformedBoardEventFallback(t, event.Payload)
	assertMalformedBoardReadFallbacks(t, svc, ctx, owner.ID, agent, "Updated title")
}

// The store rejects writes over a board whose stored waiting is invalid, so
// this decode branch is only reachable from the read paths.
func TestListAndRosterMalformedStoredWaitingPreservesLaterTasks(t *testing.T) {
	for _, shape := range []string{"waiting", "waiting-object"} {
		t.Run(shape, func(t *testing.T) { testMalformedStoredWaiting(t, shape) })
	}
}

func testMalformedStoredWaiting(t *testing.T, shape string) {
	t.Helper()
	ctx := context.Background()
	st, dsn := newTestStoreDSN(t)
	owner := mustUser(t, st, "board-malformed-waiting-owner")
	agent := mustAgent(t, st, owner.ID, "board-malformed-waiting-agent")
	svc := NewComms(t.Context(), st, newBus(t), nil, owner.ID)
	if _, err := svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{
		UpsertTasks: []*compassv1.BoardTaskPatch{
			{Key: "first", Title: new("First task")},
			{Key: "task", Title: new("Updated title"), Note: new("Stored note"), Waiting: &compassv1.BoardWaitingOp{
				Values: []*compassv1.BoardWaiting{{On: compassv1.BoardWaitingOn_BOARD_WAITING_ON_OPERATOR, What: "review"}},
			}},
			{Key: "third", Title: new("Third task")},
		},
		FocusTaskKey: new("task"),
	}); err != nil {
		t.Fatalf("seed board: %v", err)
	}
	corruptStoredBoardTask(t, ctx, st, dsn, agent.ID, "task", shape)
	filtered, err := svc.ListBoardsAsAccount(ctx, owner.ID, &compassv1.ListAgentBoardsRequest{WaitingOn: compassv1.BoardWaitingOn_BOARD_WAITING_ON_OPERATOR})
	if err != nil {
		t.Fatalf("waiting_on filter over malformed stored waiting: %v", err)
	}
	if len(filtered.GetBoards()) != 0 {
		t.Fatalf("waiting_on filter matched malformed waiting: %v", filtered.GetBoards())
	}
	svc.SetPresenceSource(fakePresenceSource{presence: map[store.AccountID]compassv1.AgentPresence{
		agent.ID: compassv1.AgentPresence_AGENT_PRESENCE_WORKING,
	}})
	assertMalformedBoardReadFallbacks(t, svc, ctx, owner.ID, agent, "Updated title")
}

func corruptStoredBoardTask(t *testing.T, ctx context.Context, st *store.Store, dsn string, agent store.AccountID, taskKey, column string) int64 {
	t.Helper()
	oldWrittenAt := time.Now().Add(-31 * time.Minute).UnixMilli()
	if _, err := st.ApplyBoardPatch(ctx, agent, store.BoardPatch{NowUnixMs: oldWrittenAt}); err != nil {
		t.Fatalf("age board: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test schema: %v", err)
	}
	defer pool.Close()
	malformed := map[string]string{
		"subagents":      `[{"id":"child","started_at_unix_ms":"invalid"}]`,
		"waiting":        `[{"on":"NOT_A_WAITING_KIND"}]`,
		"waiting-object": `{"on":"BOARD_WAITING_ON_OPERATOR"}`,
	}[column]
	target := strings.TrimSuffix(column, "-object")
	tag, err := pool.Exec(ctx, "UPDATE agent_board_tasks SET "+target+" = $4::jsonb, written_at_unix_ms = $3 WHERE agent_account_id = $1 AND task_key = $2", string(agent), taskKey, oldWrittenAt, malformed)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("corrupt and age stored %s data: rows=%d err=%v", column, tag.RowsAffected(), err)
	}
	return oldWrittenAt
}

func assertMalformedBoardUpdateFallback(t *testing.T, response *compassv1.UpdateAgentBoardResponse, oldWrittenAt int64) {
	t.Helper()
	got := response.GetBoard()
	if got == nil || got.GetFocusTaskKey() != "task" || len(got.GetTasks()) != 3 || got.GetTasks()[1].GetTitle() != "Updated title" || got.GetTasks()[2].GetKey() != "third" {
		t.Fatalf("board response after malformed stored data = %v", got)
	}
	if len(got.GetTasks()[1].GetSubagents()) != 0 || len(got.GetTasks()[1].GetWaiting()) != 0 || got.GetWrittenAtUnixMs() != oldWrittenAt || len(got.GetAsks()) != 0 {
		t.Fatalf("board fallback has malformed fields: %v", got)
	}
	if !got.GetStale() {
		t.Fatalf("board fallback lost derived stale state: %v", got)
	}
}

func assertMalformedBoardEventFallback(t *testing.T, event *compassv1.SubscribeCommsResponse) {
	t.Helper()
	changed := event.GetAgentBoardChanged()
	if changed == nil {
		t.Fatalf("fallback event after malformed stored data = %v", event.GetPayload())
	}
	got := changed.GetBoard()
	if got == nil || got.GetFocusTaskKey() != "task" || len(got.GetTasks()) != 3 || got.GetTasks()[1].GetTitle() != "Updated title" || got.GetTasks()[1].GetNote() != "Stored note" || got.GetTasks()[2].GetKey() != "third" {
		t.Fatalf("fallback event after malformed stored data = %v", event.GetPayload())
	}
	if len(got.GetTasks()[1].GetSubagents()) != 0 || len(got.GetRefs()) != 0 || len(got.GetAsks()) != 0 {
		t.Fatalf("fallback event includes malformed or derived fields: %v", got)
	}
}

func assertMalformedBoardReadFallbacks(t *testing.T, svc *Comms, ctx context.Context, owner store.AccountID, agent store.Account, focusTitle string) {
	t.Helper()
	listed, err := svc.ListBoardsAsAccount(ctx, owner, &compassv1.ListAgentBoardsRequest{AgentHandles: []string{agent.Handle}, Query: "Updated"})
	if err != nil {
		t.Fatalf("ListBoardsAsAccount with malformed stored data: %v", err)
	}
	if len(listed.GetBoards()) != 1 || listed.GetBoards()[0].GetFocusTaskKey() != "task" || len(listed.GetBoards()[0].GetTasks()) != 3 || listed.GetBoards()[0].GetTasks()[1].GetTitle() != "Updated title" || listed.GetBoards()[0].GetTasks()[2].GetKey() != "third" {
		t.Fatalf("board list after malformed stored data = %v, want all authored tasks", listed.GetBoards())
	}
	if !slices.Equal(listed.GetBoards()[0].GetMatchedTaskKeys(), []string{"task"}) {
		t.Fatalf("board list fallback matched task keys = %v, want task", listed.GetBoards()[0].GetMatchedTaskKeys())
	}
	if len(listed.GetBoards()[0].GetAsks()) != 0 || !listed.GetBoards()[0].GetStale() {
		t.Fatalf("board list fallback = %v, want derived stale state and no asks", listed.GetBoards()[0])
	}
	roster, err := svc.RosterAsAccount(ctx, owner, &compassv1.GetRosterRequest{Scope: compassv1.RosterScope_ROSTER_SCOPE_OWNER})
	if err != nil {
		t.Fatalf("RosterAsAccount with malformed stored data: %v", err)
	}
	rosterEntry := rosterByID(roster.GetEntries())[string(agent.ID)]
	if rosterEntry == nil || rosterEntry.GetBoard() == nil || rosterEntry.GetBoard().GetTaskCount() != 3 || rosterEntry.GetBoard().GetFocus() != focusTitle || !rosterEntry.GetBoard().GetStale() {
		t.Fatalf("roster summary after malformed stored data = %v, want all tasks, focus, and stale fields", rosterEntry)
	}
	if len(listed.GetBoards()[0].GetTasks()[1].GetWaiting()) != 0 || rosterEntry.GetBoard().GetWaitingOnOperator() != 0 {
		t.Fatalf("malformed waiting surfaced: list task %v, roster %v", listed.GetBoards()[0].GetTasks()[1], rosterEntry.GetBoard())
	}
}

func assertAuthoredOnlyBoard(t *testing.T, got *compassv1.AgentBoard) {
	t.Helper()
	if got == nil || len(got.GetRefs()) != 0 || len(got.GetUntrackedRefs()) != 0 || got.GetStale() || len(got.GetAsks()) != 0 {
		t.Fatalf("board = %v, want authored-only fallback without derived fields", got)
	}
	if len(got.GetTasks()) != 1 || got.GetTasks()[0].GetTitle() != "Authored task" || got.GetTasks()[0].GetPrimaryRef() != "owner/repo#12" {
		t.Fatalf("board tasks = %v, want authored task fields", got.GetTasks())
	}
}
