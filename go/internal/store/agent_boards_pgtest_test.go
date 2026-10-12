//go:build pgtest

package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func boardTask(key, title string) BoardTaskPatch {
	return BoardTaskPatch{Key: key, Title: &title}
}

func boardJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal board json: %v", err)
	}
	return data
}

func boardWaitingEntries(t *testing.T, count, whatLength int) []byte {
	t.Helper()
	entries := make([]map[string]any, count)
	for i := range entries {
		entries[i] = map[string]any{"on": "BOARD_WAITING_ON_OPERATOR", "what": strings.Repeat("w", whatLength)}
	}
	return boardJSON(t, entries)
}

func boardSubagentEntries(t *testing.T, count, settled int) []byte {
	t.Helper()
	entries := make([]map[string]any, count)
	for i := range entries {
		settledAt := int64(0)
		status := "running"
		if i < settled {
			settledAt = int64(i + 1)
			status = "completed"
		}
		entries[i] = map[string]any{
			"id": fmt.Sprintf("sub-%02d", i), "agent": "implement", "description": fmt.Sprintf("task %d", i),
			"status": status, "started_at_unix_ms": int64(i + 1), "ended_at_unix_ms": settledAt,
		}
	}
	return boardJSON(t, entries)
}

func boardRowCount(t *testing.T, s *Store, agent AccountID) (int, int) {
	t.Helper()
	var boards, tasks int
	if err := s.pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_boards WHERE agent_account_id = $1", string(agent)).Scan(&boards); err != nil {
		t.Fatalf("count board rows: %v", err)
	}
	if err := s.pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_board_tasks WHERE agent_account_id = $1", string(agent)).Scan(&tasks); err != nil {
		t.Fatalf("count task rows: %v", err)
	}
	return boards, tasks
}

func TestApplyBoardPatchMergesFieldsAndDefaultsState(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-merge-owner")
	agent := mustAgent(t, s, owner.ID, "board-merge-agent")
	title, primary, note := "Status board", "RIG-12", "first note"
	state := int16(2)
	refs := []string{"RIG-12", "owner/repo#4"}
	waiting := boardWaitingEntries(t, 1, 12)
	subagents := boardSubagentEntries(t, 1, 0)
	got, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{
		Upsert: []BoardTaskPatch{{Key: "build", Title: &title, PrimaryRef: &primary, Note: &note,
			State: &state, Refs: &refs, Waiting: &waiting, UpsertSubagents: subagents}},
		Focus: new("build"), Halt: &BoardHalt{Kind: 2, Ref: "RIG-9", Reason: "blocked"}, Turn: 7, NowUnixMs: 100,
	})
	if err != nil {
		t.Fatalf("ApplyBoardPatch(initial): %v", err)
	}
	if got.WrittenTurn != 7 || got.WrittenAtUnixMs != 100 || got.Focus != "build" || got.Halt.Kind != 2 {
		t.Fatalf("initial board metadata = %+v", got)
	}

	if len(got.Tasks) != 1 || got.Tasks[0].State != 2 || !reflect.DeepEqual(got.Tasks[0].Refs, refs) {
		t.Fatalf("initial task = %+v", got.Tasks)
	}

	newPrimary, newNote := "", "updated note"
	emptyRefs := []string{}
	merged, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{
		Upsert: []BoardTaskPatch{{Key: "build", PrimaryRef: &newPrimary, Note: &newNote, Refs: &emptyRefs,
			UpsertSubagents: boardJSON(t, []map[string]any{{"id": "sub-00", "agent": "review", "description": "reviewing", "status": "running"}})}},
		Turn: 8, NowUnixMs: 200,
	})
	if err != nil {
		t.Fatalf("ApplyBoardPatch(merge): %v", err)
	}
	if len(merged.Tasks) != 1 {
		t.Fatalf("merged task count = %d, want 1", len(merged.Tasks))
	}
	task := merged.Tasks[0]
	if task.Title != title || task.PrimaryRef != "" || task.Note != newNote || task.State != state || len(task.Refs) != 0 {
		t.Fatalf("merged task fields = %+v", task)
	}
	var gotWaiting, gotSubagents []map[string]any
	if err := json.Unmarshal(task.Waiting, &gotWaiting); err != nil {
		t.Fatalf("decode waiting: %v", err)
	}
	if err := json.Unmarshal(task.Subagents, &gotSubagents); err != nil {
		t.Fatalf("decode subagents: %v", err)
	}
	if len(gotWaiting) != 1 || gotWaiting[0]["what"] != strings.Repeat("w", 12) {
		t.Fatalf("waiting list was not kept: %s", task.Waiting)
	}
	if len(gotSubagents) != 1 || gotSubagents[0]["agent"] != "review" {
		t.Fatalf("subagent id upsert did not replace existing entry: %s", task.Subagents)
	}
	if merged.WrittenTurn != 8 || merged.WrittenAtUnixMs != 200 || merged.Halt.Reason != "blocked" {
		t.Fatalf("merged board metadata = %+v", merged)
	}

	defaultStateTitle := "default state"
	withDefault, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{
		Upsert: []BoardTaskPatch{boardTask("default", defaultStateTitle)}, Turn: 9, NowUnixMs: 300,
	})
	if err != nil {
		t.Fatalf("ApplyBoardPatch(default state): %v", err)
	}
	if len(withDefault.Tasks) != 2 || withDefault.Tasks[1].Key != "default" || withDefault.Tasks[1].State != 1 {
		t.Fatalf("new task without state = %+v, want ACTIVE(1)", withDefault.Tasks)
	}
}

func TestApplyBoardPatchRemovingFocusedTaskClearsFocus(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-remove-owner")
	agent := mustAgent(t, s, owner.ID, "board-remove-agent")
	title := "focused task"
	got, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{
		Upsert: []BoardTaskPatch{boardTask("focus", title)}, Focus: new("focus"), NowUnixMs: 10,
	})
	if err != nil {
		t.Fatalf("ApplyBoardPatch: %v", err)
	}
	if got.Focus != "focus" {
		t.Fatalf("focus = %q, want focus", got.Focus)
	}
	got, err = s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{Remove: []string{"focus"}, NowUnixMs: 20})
	if err != nil {
		t.Fatalf("ApplyBoardPatch(remove): %v", err)
	}
	if got.Focus != "" || len(got.Tasks) != 0 {
		t.Fatalf("board after focused task removal = %+v", got)
	}
	boards, err := s.AgentBoards(t.Context(), []AccountID{agent.ID})
	if err != nil {
		t.Fatalf("AgentBoards after removal: %v", err)
	}
	if boards[agent.ID].Focus != "" {
		t.Fatalf("persisted focus = %q after removing focused task", boards[agent.ID].Focus)
	}
	title = "later task"
	if _, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{
		Upsert: []BoardTaskPatch{boardTask("later", title)}, NowUnixMs: 30,
	}); err != nil {
		t.Fatalf("ApplyBoardPatch after removal: %v", err)
	}
}

func TestApplyBoardPatchExplicitEmptyFocusClearsFocus(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-focus-clear-owner")
	agent := mustAgent(t, s, owner.ID, "board-focus-clear-agent")
	title := "focused"
	if _, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{
		Upsert: []BoardTaskPatch{boardTask("focus", title)}, Focus: new("focus"), NowUnixMs: 1,
	}); err != nil {
		t.Fatalf("ApplyBoardPatch(initial): %v", err)
	}
	if _, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{Focus: new(""), NowUnixMs: 2}); err != nil {
		t.Fatalf("ApplyBoardPatch(clear focus): %v", err)
	}
	boards, err := s.AgentBoards(t.Context(), []AccountID{agent.ID})
	if err != nil {
		t.Fatalf("AgentBoards: %v", err)
	}
	if boards[agent.ID].Focus != "" {
		t.Fatalf("focus = %q, want empty", boards[agent.ID].Focus)
	}
}

func TestApplyBoardPatchRejectsUnknownFocusAndClearsWaiting(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-focus-validation-owner")
	agent := mustAgent(t, s, owner.ID, "board-focus-validation-agent")
	title := "waiting task"
	waiting := boardWaitingEntries(t, 1, 8)
	if _, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{
		Upsert: []BoardTaskPatch{{Key: "task", Title: &title, Waiting: &waiting}}, NowUnixMs: 1,
	}); err != nil {
		t.Fatalf("ApplyBoardPatch(initial): %v", err)
	}
	_, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{Focus: new("missing"), NowUnixMs: 2})
	sentinelIs(t, err, ErrInvalidArgument, "focus task not on board")
	emptyWaiting := []byte("[]")
	got, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{
		Upsert: []BoardTaskPatch{{Key: "task", Waiting: &emptyWaiting}}, NowUnixMs: 3,
	})
	if err != nil {
		t.Fatalf("ApplyBoardPatch(clear waiting): %v", err)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(got.Tasks[0].Waiting, &entries); err != nil || len(entries) != 0 {
		t.Fatalf("waiting after clear = %s, err=%v", got.Tasks[0].Waiting, err)
	}
}

func TestApplyBoardPatchReplaceTaskAtCapacity(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-replace-full-owner")
	agent := mustAgent(t, s, owner.ID, "board-replace-full-agent")
	upserts := make([]BoardTaskPatch, MaxBoardTasks)
	for i := range upserts {
		upserts[i] = boardTask(fmt.Sprintf("task-%02d", i), "task")
	}
	if _, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{Upsert: upserts, NowUnixMs: 1}); err != nil {
		t.Fatalf("ApplyBoardPatch(fill board): %v", err)
	}
	title := "replacement"
	if _, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{
		Remove: []string{"task-00"}, Upsert: []BoardTaskPatch{boardTask("replacement", title)}, NowUnixMs: 2,
	}); err != nil {
		t.Fatalf("ApplyBoardPatch(replace at capacity): %v", err)
	}
	boards, err := s.AgentBoards(t.Context(), []AccountID{agent.ID})
	if err != nil {
		t.Fatalf("AgentBoards after replacement: %v", err)
	}
	got := boards[agent.ID].Tasks
	if len(got) != MaxBoardTasks || got[0].Key != "task-01" || got[len(got)-1].Key != "replacement" {
		t.Fatalf("replacement board has unexpected tasks: count=%d first=%q last=%q", len(got), got[0].Key, got[len(got)-1].Key)
	}
	for i, task := range got {
		want := fmt.Sprintf("task-%02d", i+1)
		if i == len(got)-1 {
			want = "replacement"
		}
		if task.Key != want {
			t.Fatalf("stored task %d = %q, want %q", i, task.Key, want)
		}
	}
}

func TestApplyBoardPatchCapBreachPreservesExistingBoard(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-existing-cap-owner")
	agent := mustAgent(t, s, owner.ID, "board-existing-cap-agent")
	title := "existing"
	initial, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{
		Upsert: []BoardTaskPatch{boardTask("existing", title)}, Focus: new("existing"), NowUnixMs: 1,
	})
	if err != nil {
		t.Fatalf("ApplyBoardPatch(initial): %v", err)
	}
	tooMany := make([]BoardTaskPatch, MaxBoardTasks)
	for i := range tooMany {
		tooMany[i] = boardTask(fmt.Sprintf("extra-%02d", i), "extra")
	}
	_, err = s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{Upsert: tooMany, NowUnixMs: 2})
	sentinelIs(t, err, ErrInvalidArgument, "existing board task cap")
	boards, err := s.AgentBoards(t.Context(), []AccountID{agent.ID})
	if err != nil {
		t.Fatalf("AgentBoards after rejected patch: %v", err)
	}
	got := boards[agent.ID]
	if len(got.Tasks) != 1 || got.Focus != initial.Focus || got.WrittenTurn != initial.WrittenTurn || got.WrittenAtUnixMs != initial.WrittenAtUnixMs ||
		got.Tasks[0].Key != "existing" || got.Tasks[0].Title != "existing" || got.Tasks[0].WrittenAtUnixMs != initial.Tasks[0].WrittenAtUnixMs {
		t.Fatalf("board changed after rejected patch: got %+v, want %+v", got, initial)
	}
}

func TestApplyBoardPatchValidationWritesNothing(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-invalid-owner")
	tooLong := strings.Repeat("x", 121)
	tooLongNote := strings.Repeat("x", 501)
	tooLongReason := strings.Repeat("x", 201)
	tooLongWhat := boardWaitingEntries(t, 1, 201)
	tooManyRefs := make([]string, 11)
	tooManyWaits := boardWaitingEntries(t, 6, 1)
	tooManyActive := boardSubagentEntries(t, 21, 0)
	tooManyTasks := make([]BoardTaskPatch, 31)
	for i := range tooManyTasks {
		tooManyTasks[i] = boardTask(fmt.Sprintf("task-%02d", i), "task")
	}
	missingWaitingOn := []byte(`[{"what":"review"}]`)
	numericWaitingOn := []byte(`[{"on":1,"what":"review"}]`)
	unknownWaitingOn := []byte(`[{"on":"BOARD_WAITING_ON_UNKNOWN","what":"review"}]`)
	cases := []struct {
		name  string
		patch BoardPatch
	}{
		{name: "waiting on missing", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "waiting-missing-on", Title: new("task"), Waiting: &missingWaitingOn}}}},
		{name: "waiting on numeric", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "waiting-numeric-on", Title: new("task"), Waiting: &numericWaitingOn}}}},
		{name: "waiting on unknown", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "waiting-unknown-on", Title: new("task"), Waiting: &unknownWaitingOn}}}},
		{name: "new key requires title", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "missing-title"}}}},
		{name: "title cap", patch: BoardPatch{Upsert: []BoardTaskPatch{boardTask("title", tooLong)}}},
		{name: "note cap", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "note", Title: new("task"), Note: &tooLongNote}}}},
		{name: "refs cap", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "refs", Title: new("task"), Refs: &tooManyRefs}}}},
		{name: "waiting cap", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "waiting", Title: new("task"), Waiting: &tooManyWaits}}}},
		{name: "waiting what cap", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "what", Title: new("task"), Waiting: &tooLongWhat}}}},
		{name: "active subagent cap", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "subagents", Title: new("task"), UpsertSubagents: tooManyActive}}}},
		{name: "task cap", patch: BoardPatch{Upsert: tooManyTasks}},
		{name: "halt reason cap", patch: BoardPatch{Halt: &BoardHalt{Kind: 2, Reason: tooLongReason}}},
		{name: "invalid state", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "state", Title: new("task"), State: new(int16(0))}}}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caseAgent := mustAgent(t, s, owner.ID, fmt.Sprintf("board-invalid-agent-%02d", i))
			_, err := s.ApplyBoardPatch(t.Context(), caseAgent.ID, tc.patch)
			sentinelIs(t, err, ErrInvalidArgument, tc.name)
			boards, tasks := boardRowCount(t, s, caseAgent.ID)
			if boards != 0 || tasks != 0 {
				t.Fatalf("invalid patch wrote %d board rows and %d task rows", boards, tasks)
			}
		})
	}
}

func TestApplyBoardPatchDropsOldestSettledSubagent(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-subagent-owner")
	agent := mustAgent(t, s, owner.ID, "board-subagent-agent")
	title := "subagents"
	entries := make([]map[string]any, 21)
	for i := range entries {
		entries[i] = map[string]any{
			"id": fmt.Sprintf("sub-%02d", i), "agent": "implement", "description": fmt.Sprintf("task %d", i),
			"status": "running", "startedAtUnixMs": strconv.Itoa(1000 - i),
		}
	}
	entries[0]["status"] = "completed"
	entries[0]["startedAtUnixMs"] = "200"
	entries[0]["endedAtUnixMs"] = "300"
	entries[10]["status"] = "aborted"
	entries[10]["startedAtUnixMs"] = "100"
	entries[20]["status"] = "started"
	entries[20]["startedAtUnixMs"] = "1"
	got, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{
		Upsert: []BoardTaskPatch{{Key: "workers", Title: &title, UpsertSubagents: boardJSON(t, entries)}}, NowUnixMs: 10,
	})
	if err != nil {
		t.Fatalf("ApplyBoardPatch: %v", err)
	}
	var subagents []map[string]any
	if err := json.Unmarshal(got.Tasks[0].Subagents, &subagents); err != nil {
		t.Fatalf("decode subagents: %v", err)
	}
	if len(subagents) != 20 {
		t.Fatalf("retained %d subagents, want 20", len(subagents))
	}
	for _, entry := range subagents {
		if entry["id"] == "sub-10" {
			t.Fatalf("oldest settled subagent remained: %+v", entry)
		}
	}
	if subagents[len(subagents)-1]["id"] != "sub-20" {
		t.Fatalf("oldest live subagent was not retained: %+v", subagents[len(subagents)-1])
	}
	for _, status := range []string{"", "running", "pending", "started", "queued", "unknown", "cancelled", "error"} {
		settled, _ := subagentSettled(json.RawMessage(fmt.Sprintf(`{"status":%q}`, status)))
		if settled {
			t.Errorf("subagent status %q was treated as settled", status)
		}
	}
	for _, status := range []string{"completed", "failed", "aborted"} {
		settled, _ := subagentSettled(json.RawMessage(fmt.Sprintf(`{"status":%q}`, status)))
		if !settled {
			t.Errorf("subagent status %q was not treated as settled", status)
		}
	}
}

func TestAutomaticBoardPatchPreservesFreshnessAndEmptyPatchWritesNothing(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-auto-owner")
	agent := mustAgent(t, s, owner.ID, "board-auto-agent")
	title := "automatic task"
	initial, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{
		Upsert: []BoardTaskPatch{boardTask("automatic", title)}, Turn: 4, NowUnixMs: 123,
	})
	if err != nil {
		t.Fatalf("ApplyBoardPatch(initial): %v", err)
	}
	updated, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{
		Upsert:    []BoardTaskPatch{{Key: "automatic", UpsertSubagents: boardSubagentEntries(t, 1, 0)}},
		Automatic: true, Turn: 99, NowUnixMs: 999,
	})
	if err != nil {
		t.Fatalf("ApplyBoardPatch(automatic): %v", err)
	}
	if updated.WrittenTurn != 4 || updated.WrittenAtUnixMs != 123 || updated.Tasks[0].WrittenAtUnixMs != initial.Tasks[0].WrittenAtUnixMs {
		t.Fatalf("automatic patch changed freshness: %+v", updated)
	}
	noOp, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{Automatic: true, Turn: 100, NowUnixMs: 1000})
	if err != nil {
		t.Fatalf("ApplyBoardPatch(empty automatic): %v", err)
	}
	if noOp.Focus != updated.Focus || len(noOp.Tasks) != 1 || noOp.Tasks[0].Key != updated.Tasks[0].Key {
		t.Fatalf("empty automatic patch changed board content: %+v", noOp)
	}
	other := mustAgent(t, s, owner.ID, "board-empty-auto-agent")
	empty, err := s.ApplyBoardPatch(t.Context(), other.ID, BoardPatch{Automatic: true, Turn: 77, NowUnixMs: 77})
	if err != nil {
		t.Fatalf("ApplyBoardPatch(empty automatic without row): %v", err)
	}
	boards, tasks := boardRowCount(t, s, other.ID)
	if boards != 0 || tasks != 0 || empty.Agent != other.ID {
		t.Fatalf("empty automatic patch created rows or lost agent: board=%+v rows=%d/%d", empty, boards, tasks)
	}
}

func TestSearchAgentBoardsCombinesFilters(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-search-owner")
	agent := mustAgent(t, s, owner.ID, "board-search-agent")
	excluded := mustAgent(t, s, owner.ID, "board-search-excluded-agent")
	other := mustAgent(t, s, owner.ID, "board-search-other")
	title, note := "Status board storage", "migration details"
	state := int16(2)
	refs := []string{"RIG-12"}
	wrongRefs := []string{"RIG-99"}
	waiting := boardJSON(t, []map[string]any{{"on": "BOARD_WAITING_ON_OPERATOR", "what": "review"}})
	wrongWaiting := boardJSON(t, []map[string]any{{"on": "BOARD_WAITING_ON_AGENT", "what": "review"}})
	wrongState := int16(3)
	setup := []struct {
		agent   AccountID
		key     string
		note    string
		refs    []string
		waiting []byte
		state   int16
		halt    int16
	}{
		{agent: agent.ID, key: "match", note: note, refs: refs, waiting: waiting, state: state, halt: 2},
		{agent: agent.ID, key: "wrong-query", note: "release notes", refs: refs, waiting: waiting, state: state, halt: 2},
		{agent: agent.ID, key: "wrong-ref", note: note, refs: wrongRefs, waiting: waiting, state: state, halt: 2},
		{agent: agent.ID, key: "wrong-waiting", note: note, refs: refs, waiting: wrongWaiting, state: state, halt: 2},
		{agent: agent.ID, key: "wrong-state", note: note, refs: refs, waiting: waiting, state: wrongState, halt: 2},
		{agent: excluded.ID, key: "wrong-agent", note: note, refs: refs, waiting: waiting, state: state, halt: 2},
		{agent: other.ID, key: "wrong-halt", note: note, refs: refs, waiting: waiting, state: state, halt: 1},
	}
	for i := range setup {
		item := &setup[i]
		if _, err := s.ApplyBoardPatch(t.Context(), item.agent, BoardPatch{
			Upsert: []BoardTaskPatch{{Key: item.key, Title: &title, Note: &item.note, State: &item.state, Refs: &item.refs, Waiting: &item.waiting}},
			Halt:   &BoardHalt{Kind: item.halt}, NowUnixMs: int64(i + 1),
		}); err != nil {
			t.Fatalf("ApplyBoardPatch(%s): %v", item.key, err)
		}
	}
	got, _, err := s.SearchAndReadAgentBoards(t.Context(), BoardTaskFilter{
		Agents: []AccountID{agent.ID, other.ID}, Query: "migration", Ref: "RIG-12", WaitingOn: 1, State: 2, Halt: 2,
	})
	if err != nil {
		t.Fatalf("SearchAgentBoards: %v", err)
	}
	if !reflect.DeepEqual(got, map[AccountID][]string{agent.ID: {"match"}}) {
		t.Fatalf("SearchAgentBoards = %v, want only the task matching every filter", got)
	}
}

func TestSearchAgentBoardsIncludesEmptyBoardsAndFiltersByBoardHalt(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-empty-search-owner")
	agent := mustAgent(t, s, owner.ID, "board-empty-search-agent")
	if _, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{
		Halt: &BoardHalt{Kind: 2}, NowUnixMs: 1,
	}); err != nil {
		t.Fatalf("ApplyBoardPatch(empty halted board): %v", err)
	}

	matches, _, err := s.SearchAndReadAgentBoards(t.Context(), BoardTaskFilter{Agents: []AccountID{agent.ID}, Halt: 2})
	if err != nil {
		t.Fatalf("SearchAgentBoards(blocked): %v", err)
	}
	if keys, ok := matches[agent.ID]; !ok || len(keys) != 0 {
		t.Fatalf("SearchAgentBoards(blocked) = %v, want empty board with no matched tasks", matches)
	}

	matches, _, err = s.SearchAndReadAgentBoards(t.Context(), BoardTaskFilter{Agents: []AccountID{agent.ID}, Halt: 1})
	if err != nil {
		t.Fatalf("SearchAgentBoards(done): %v", err)
	}
	if _, ok := matches[agent.ID]; ok {
		t.Fatalf("SearchAgentBoards(done) returned blocked board: %v", matches)
	}
}

func TestSearchAgentBoardsTitleAndPrimaryRefOnly(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-search-title-ref-owner")
	agent := mustAgent(t, s, owner.ID, "board-search-title-ref-agent")
	titleOnly := "Quartz milestone"
	quietNote := "ordinary notes"
	noRefs := []string{}
	primaryOnly := "No matching words"
	primaryRef := "RIG-884"
	if _, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{Upsert: []BoardTaskPatch{
		{Key: "title-only", Title: &titleOnly, Note: &quietNote, Refs: &noRefs},
		{Key: "primary-only", Title: &primaryOnly, PrimaryRef: &primaryRef, Refs: &noRefs},
	}, NowUnixMs: 1}); err != nil {
		t.Fatalf("ApplyBoardPatch: %v", err)
	}
	t.Run("title contains query", func(t *testing.T) {
		got, _, err := s.SearchAndReadAgentBoards(t.Context(), BoardTaskFilter{Agents: []AccountID{agent.ID}, Query: "quartz"})
		if err != nil {
			t.Fatalf("SearchAgentBoards(title query): %v", err)
		}
		if !reflect.DeepEqual(got[agent.ID], []string{"title-only"}) {
			t.Fatalf("title query results = %v, want title-only", got)
		}
	})
	t.Run("primary ref matches without refs entry", func(t *testing.T) {
		got, _, err := s.SearchAndReadAgentBoards(t.Context(), BoardTaskFilter{Agents: []AccountID{agent.ID}, Ref: primaryRef})
		if err != nil {
			t.Fatalf("SearchAgentBoards(primary ref): %v", err)
		}
		if !reflect.DeepEqual(got[agent.ID], []string{"primary-only"}) {
			t.Fatalf("primary ref results = %v, want primary-only", got)
		}
	})
}

func TestSearchAgentBoardsClampsLimit(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-search-limit-owner")
	agents := make([]AccountID, MaxBoardSearchAgents+1)
	taskKeys := []string{"task-a", "task-b", "task-c"}
	title := "searchable"
	for i := range agents {
		agent := mustAgent(t, s, owner.ID, fmt.Sprintf("board-search-limit-%03d", i))
		agents[i] = agent.ID
		patches := make([]BoardTaskPatch, len(taskKeys))
		for j, key := range taskKeys {
			patches[j] = boardTask(key, title)
		}
		if _, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{Upsert: patches, NowUnixMs: 1}); err != nil {
			t.Fatalf("ApplyBoardPatch(%d): %v", i, err)
		}
	}
	got, _, err := s.SearchAndReadAgentBoards(t.Context(), BoardTaskFilter{Agents: agents, Limit: MaxBoardSearchAgents + 1})
	if err != nil {
		t.Fatalf("SearchAgentBoards: %v", err)
	}
	if len(got) != MaxBoardSearchAgents {
		t.Fatalf("SearchAgentBoards returned %d agents, want cap %d", len(got), MaxBoardSearchAgents)
	}
	wantStoreIDs := append([]AccountID(nil), agents...)
	slices.Sort(wantStoreIDs)
	wantStoreIDs = wantStoreIDs[:MaxBoardSearchAgents]
	gotStoreIDs := make([]AccountID, 0, MaxBoardSearchAgents)
	for agentID, keys := range got {
		gotStoreIDs = append(gotStoreIDs, agentID)
		if !reflect.DeepEqual(keys, taskKeys) {
			t.Fatalf("agent %q returned task keys %v, want %v", agentID, keys, taskKeys)
		}
	}
	slices.Sort(gotStoreIDs)
	if !reflect.DeepEqual(gotStoreIDs, wantStoreIDs) {
		t.Fatalf("SearchAgentBoards returned unexpected capped agents: got %v, want %v", gotStoreIDs, wantStoreIDs)
	}
}

func TestSearchAgentBoardsZeroLimitDefaultsToTwenty(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-search-zero-limit-owner")
	agents := make([]AccountID, 25)
	taskKeys := []string{"task-a", "task-b"}
	title := "searchable"
	for i := range agents {
		agent := mustAgent(t, s, owner.ID, fmt.Sprintf("board-search-zero-limit-%02d", i))
		agents[i] = agent.ID
		patches := make([]BoardTaskPatch, len(taskKeys))
		for j, key := range taskKeys {
			patches[j] = boardTask(key, title)
		}
		if _, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{Upsert: patches, NowUnixMs: 1}); err != nil {
			t.Fatalf("ApplyBoardPatch(%d): %v", i, err)
		}
	}
	got, _, err := s.SearchAndReadAgentBoards(t.Context(), BoardTaskFilter{Limit: 0})
	if err != nil {
		t.Fatalf("SearchAgentBoards(zero limit): %v", err)
	}
	if len(got) != 20 {
		t.Fatalf("SearchAgentBoards(zero limit) returned %d agents, want 20", len(got))
	}
	wantAgents := append([]AccountID(nil), agents...)
	slices.Sort(wantAgents)
	wantAgents = wantAgents[:20]
	gotAgents := make([]AccountID, 0, len(got))
	for agent, keys := range got {
		gotAgents = append(gotAgents, agent)
		if !reflect.DeepEqual(keys, taskKeys) {
			t.Fatalf("agent %q returned keys %v, want %v", agent, keys, taskKeys)
		}
	}
	slices.Sort(gotAgents)
	if !reflect.DeepEqual(gotAgents, wantAgents) {
		t.Fatalf("SearchAgentBoards(zero limit) agents = %v, want %v", gotAgents, wantAgents)
	}
}

func TestAgentBoardsUsesOneSnapshotAcrossConcurrentPatch(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-snapshot-owner")
	agent := mustAgent(t, s, owner.ID, "board-snapshot-agent")
	oldTitle, newTitle := "before patch", "after patch"
	if _, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{
		Upsert: []BoardTaskPatch{boardTask("old", oldTitle)}, Focus: new("old"), NowUnixMs: 1,
	}); err != nil {
		t.Fatalf("ApplyBoardPatch(initial): %v", err)
	}
	var patchErr error
	s.afterBoardMetaRead = func() {
		s.afterBoardMetaRead = nil
		_, patchErr = s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{
			Upsert: []BoardTaskPatch{boardTask("new", newTitle)}, Focus: new("new"), NowUnixMs: 2,
		})
	}
	got, err := s.AgentBoards(t.Context(), []AccountID{agent.ID})
	if err != nil {
		t.Fatalf("AgentBoards: %v", err)
	}
	if patchErr != nil {
		t.Fatalf("ApplyBoardPatch(concurrent): %v", patchErr)
	}
	board := got[agent.ID]
	if board.Focus != "old" || len(board.Tasks) != 1 || board.Tasks[0].Key != "old" || board.Tasks[0].Title != oldTitle {
		t.Fatalf("AgentBoards mixed snapshots: %+v", board)
	}
	after, err := s.AgentBoards(t.Context(), []AccountID{agent.ID})
	if err != nil {
		t.Fatalf("AgentBoards(after patch): %v", err)
	}
	board = after[agent.ID]
	if board.Focus != "new" || len(board.Tasks) != 2 || board.Tasks[1].Key != "new" || board.Tasks[1].Title != newTitle {
		t.Fatalf("committed patch missing from next snapshot: %+v", board)
	}
}

func TestSearchAndReadAgentBoardsUsesOneSnapshotAcrossConcurrentPatch(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-search-snapshot-owner")
	agent := mustAgent(t, s, owner.ID, "board-search-snapshot-agent")
	if _, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{
		Upsert: []BoardTaskPatch{boardTask("old", "match old")}, Focus: new("old"), NowUnixMs: 1,
	}); err != nil {
		t.Fatalf("ApplyBoardPatch(initial): %v", err)
	}
	var patchErr error
	s.afterBoardMetaRead = func() {
		s.afterBoardMetaRead = nil
		_, patchErr = s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{
			Upsert: []BoardTaskPatch{boardTask("new", "match new")}, Focus: new("new"), NowUnixMs: 2,
		})
	}
	matches, boards, err := s.SearchAndReadAgentBoards(t.Context(), BoardTaskFilter{Agents: []AccountID{agent.ID}, Query: "match"})
	if err != nil {
		t.Fatalf("SearchAndReadAgentBoards: %v", err)
	}
	if patchErr != nil {
		t.Fatalf("ApplyBoardPatch(concurrent): %v", patchErr)
	}
	if !reflect.DeepEqual(matches[agent.ID], []string{"old"}) {
		t.Fatalf("matched task keys = %v, want old snapshot", matches[agent.ID])
	}
	board := boards[agent.ID]
	if board.Focus != "old" || len(board.Tasks) != 1 || board.Tasks[0].Key != "old" {
		t.Fatalf("board rows came from a different snapshot: %+v", board)
	}
}

func TestBoardCoordinateReadsAndOpenAuthoredRefs(t *testing.T) {
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "board-coords")
	openIssue, closedIssue := ghCoord("Owner/Repo", 41), ghCoord("Owner/Repo", 42)
	for _, tc := range []struct {
		coord ForgeCoord
		state string
	}{{openIssue, "open"}, {closedIssue, "closed"}} {
		if _, err := s.UpsertIssueForgeFields(t.Context(), IssueForgeFields{
			ForgeProvider: tc.coord.Provider, ForgeHost: tc.coord.Host, Repo: tc.coord.Repo,
			Number: uint32(tc.coord.Number), Title: fmt.Sprintf("issue %d", tc.coord.Number), ForgeState: tc.state,
		}); err != nil {
			t.Fatalf("UpsertIssueForgeFields(%d): %v", tc.coord.Number, err)
		}
		if err := s.RecordAuthoredArtifact(t.Context(), AuthoredArtifact{
			Provider: tc.coord.Provider, Host: tc.coord.Host, Repo: tc.coord.Repo,
			Kind: ForgeArtifactKindIssue, Number: tc.coord.Number, AgentAccountID: agent,
			OwnerUserID: owner, CreatedAtUnixMS: int64(tc.coord.Number),
		}); err != nil {
			t.Fatalf("RecordAuthoredArtifact(issue %d): %v", tc.coord.Number, err)
		}
	}
	openPR, closedPR, mergedPR := ghCoord("OWNER/REPO", 51), ghCoord("OWNER/REPO", 52), ghCoord("OWNER/REPO", 53)
	for _, tc := range []struct {
		coord ForgeCoord
		state string
	}{{openPR, "open"}, {closedPR, "closed"}, {mergedPR, "merged"}} {
		pr := prRow(tc.coord.Repo, tc.coord.Number, tc.state, prBase, prBase.Add(time.Hour))
		if _, err := s.UpsertPullRequest(t.Context(), pr, nil); err != nil {
			t.Fatalf("UpsertPullRequest(%d): %v", tc.coord.Number, err)
		}
		if err := s.RecordAuthoredArtifact(t.Context(), authoredPR(agent, owner, pr)); err != nil {
			t.Fatalf("RecordAuthoredArtifact(PR %d): %v", tc.coord.Number, err)
		}
	}
	open, err := s.OpenAuthoredRefs(t.Context(), agent)
	if err != nil {
		t.Fatalf("OpenAuthoredRefs: %v", err)
	}
	if len(open) != 2 || open[0].Coord.Number != openIssue.Number || open[0].Kind != ForgeArtifactKindIssue ||
		open[1].Coord.Number != openPR.Number || open[1].Kind != ForgeArtifactKindPullRequest {
		t.Fatalf("open authored refs = %+v, want open issue and PR only", open)
	}
	owned, err := s.AuthoredCoordsForAgent(t.Context(), agent, []ForgeCoord{openIssue, closedIssue, openPR, closedPR, mergedPR})
	if err != nil {
		t.Fatalf("AuthoredCoordsForAgent: %v", err)
	}
	ownedByArtifact := make(map[uint64]ForgeArtifactKind, len(owned))
	for _, ref := range owned {
		coord := ref.Coord.Normalized()
		ownedByArtifact[coord.Number] = ref.Kind
	}
	for _, number := range []uint64{openIssue.Number, closedIssue.Number} {
		if ownedByArtifact[number] != ForgeArtifactKindIssue {
			t.Errorf("authored issue %d = %v, want ownership regardless of forge state", number, ownedByArtifact[number])
		}
	}
	for _, number := range []uint64{openPR.Number, closedPR.Number, mergedPR.Number} {
		if ownedByArtifact[number] != ForgeArtifactKindPullRequest {
			t.Errorf("authored PR %d = %v, want ownership regardless of forge state", number, ownedByArtifact[number])
		}
	}
	mixedCasePRs, err := s.PullRequestsByCoords(t.Context(), []ForgeCoord{ghCoord("OWNER/REPO", openPR.Number)})
	if err != nil {
		t.Fatalf("PullRequestsByCoords(mixed-case repo): %v", err)
	}
	if len(mixedCasePRs) != 1 || mixedCasePRs[0].Coord.Number != openPR.Number {
		t.Fatalf("PullRequestsByCoords(mixed-case repo) = %+v, want PR %d", mixedCasePRs, openPR.Number)
	}
	prs, err := s.PullRequestsByCoords(t.Context(), []ForgeCoord{openPR, closedPR, openPR})
	if err != nil {
		t.Fatalf("PullRequestsByCoords: %v", err)
	}
	if len(prs) != 2 || prs[0].Coord.Number != openPR.Number || prs[1].Coord.Number != closedPR.Number {
		t.Fatalf("PullRequestsByCoords = %+v", prs)
	}
	issues, err := s.IssuesByCoords(t.Context(), []ForgeCoord{openIssue, closedIssue})
	if err != nil {
		t.Fatalf("IssuesByCoords: %v", err)
	}
	if len(issues) != 2 || issues[0].Number != uint32(openIssue.Number) || issues[1].Number != uint32(closedIssue.Number) {
		t.Fatalf("IssuesByCoords = %+v", issues)
	}
}

func TestAsksByAuthorClipsInvisibleChannels(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-asks-owner")
	viewer := mustUser(t, s, "board-asks-viewer")
	author := mustAgent(t, s, owner.ID, "board-asks-author")
	private, err := s.CreateChannel(t.Context(), owner.ID, NewChannel{
		Name: "board-private", MemberAccountIDs: []AccountID{owner.ID, author.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel(private): %v", err)
	}
	group, err := s.CreateChannelGroup(t.Context(), owner.ID, NewChannelGroup{Name: "board-shared", Visibility: VisibilityShared})
	if err != nil {
		t.Fatalf("CreateChannelGroup(shared): %v", err)
	}
	shared, err := s.CreateChannel(t.Context(), owner.ID, NewChannel{
		Name: "board-shared-room", Kind: ChannelKindChannel, GroupID: group.ID, MemberAccountIDs: []AccountID{author.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel(shared): %v", err)
	}
	for _, tc := range []struct {
		channel ChannelID
		ref     string
	}{{private.ID, "RIG-13"}, {shared.ID, "RIG-12"}} {
		_, _, err := s.AppendMessage(t.Context(), Message{
			AuthorAccountID: author.ID,
			Blocks:          []MessageBlock{{Ask: &Ask{Ref: tc.ref, Questions: []AskQuestion{{QuestionID: "q", Question: "Need a decision?"}}}}},
		}, string(tc.channel), TopicRef{Name: "general", Create: true}, "")
		if err != nil {
			t.Fatalf("AppendMessage(ask %s): %v", tc.ref, err)
		}
	}
	tasks, err := s.AsksByAuthor(t.Context(), viewer.ID, author.ID, time.Unix(1, 0), 20)
	if err != nil {
		t.Fatalf("AsksByAuthor: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Ref != "RIG-12" || tasks[0].ChannelID != string(shared.ID) || tasks[0].Header != "Need a decision?" {
		t.Fatalf("AsksByAuthor = %+v, want only shared-channel ask", tasks)
	}
}

func TestAsksByAuthorUsesAnswerMessageTimestamp(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-ask-time-owner")
	author := mustAgent(t, s, owner.ID, "board-ask-time-author")
	channel, err := s.CreateChannel(t.Context(), owner.ID, NewChannel{
		Name: "board-ask-time", MemberAccountIDs: []AccountID{owner.ID, author.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	answerIDs := make([]string, maxBoardAsks)
	askIDs := make([]string, maxBoardAsks)
	askTimes := make([]int64, maxBoardAsks)
	for i := range answerIDs {
		answerAt := int64(2_000 + i)
		askTimes[i] = int64(1_000 + i)
		ask, _, err := s.AppendMessage(t.Context(), Message{
			AuthorAccountID: author.ID,
			Blocks:          []MessageBlock{{Ask: &Ask{Questions: []AskQuestion{{QuestionID: "q", Question: "Question"}}}}},
		}, string(channel.ID), TopicRef{Name: "general", Create: true}, "")
		if err != nil {
			t.Fatalf("AppendMessage(%d): %v", i, err)
		}
		askIDs[i] = ask.Blocks[0].Ask.AskID
		_, answer, err := s.AnswerAsk(t.Context(), owner.ID, askIDs[i], []AskAnswer{{QuestionID: "q"}})
		if err != nil {
			t.Fatalf("AnswerAsk(%d): %v", i, err)
		}
		answerIDs[i] = string(answer.ID)
		if _, err := s.pool.Exec(t.Context(), "UPDATE messages SET at_unix_ms = $1 WHERE id = $2", askTimes[i], string(ask.ID)); err != nil {
			t.Fatalf("age ask %d: %v", i, err)
		}
		if _, err := s.pool.Exec(t.Context(), "UPDATE messages SET at_unix_ms = $1 WHERE id = $2", answerAt, answerIDs[i]); err != nil {
			t.Fatalf("set answer time %d: %v", i, err)
		}
	}
	oldPending, _, err := s.AppendMessage(t.Context(), Message{
		AuthorAccountID: author.ID,
		Blocks:          []MessageBlock{{Ask: &Ask{Questions: []AskQuestion{{QuestionID: "q", Question: "Old pending ask"}}}}},
	}, string(channel.ID), TopicRef{Name: "general", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage(old pending ask): %v", err)
	}
	if _, err := s.pool.Exec(t.Context(), "UPDATE messages SET at_unix_ms = $1 WHERE id = $2", int64(100), string(oldPending.ID)); err != nil {
		t.Fatalf("age pending ask: %v", err)
	}
	tasks, err := s.AsksByAuthor(t.Context(), owner.ID, author.ID, time.UnixMilli(1_200), 20)
	if err != nil {
		t.Fatalf("AsksByAuthor: %v", err)
	}
	if len(tasks) != maxBoardAsks {
		t.Fatalf("AsksByAuthor returned %d asks, want pending ask and %d answered asks", len(tasks), maxBoardAsks-1)
	}
	if tasks[0].AskID != oldPending.Blocks[0].Ask.AskID || tasks[0].Answered || tasks[0].PostedAtUnixMs != 100 {
		t.Fatalf("first ask = %+v, want old pending ask %q", tasks[0], oldPending.Blocks[0].Ask.AskID)
	}
	for i := 1; i < len(tasks); i++ {
		answerIndex := maxBoardAsks - i
		if tasks[i].AskID != askIDs[answerIndex] || tasks[i].PostedAtUnixMs != askTimes[answerIndex] || !tasks[i].Answered {
			t.Fatalf("answer %d = %+v, want ask %q posted at %d", i, tasks[i], askIDs[answerIndex], askTimes[answerIndex])
		}
	}
}

func TestAgentBoardsTenantIsolation(t *testing.T) {
	s := newTestStore(t)
	tenantA := s.EffectiveTenant(t.Context())
	tenantB := seedTenant(t, s, "board-tenant-b")
	ctxA := WithTenant(t.Context(), tenantA)
	owner, err := s.CreateUser(ctxA, NewUser{Handle: "board-tenant-owner", DisplayName: "Owner"})
	if err != nil {
		t.Fatalf("CreateUser(tenant A): %v", err)
	}
	agent, err := s.CreateAgent(ctxA, owner.ID, NewAgent{Handle: "board-tenant-agent", DisplayName: "Agent"})
	if err != nil {
		t.Fatalf("CreateAgent(tenant A): %v", err)
	}
	title := "tenant board"
	if _, err := s.ApplyBoardPatch(ctxA, agent.ID, BoardPatch{
		Upsert: []BoardTaskPatch{boardTask("private", title)}, NowUnixMs: 10,
	}); err != nil {
		t.Fatalf("ApplyBoardPatch(tenant A): %v", err)
	}
	boards, err := s.AgentBoards(WithTenant(t.Context(), tenantB), []AccountID{agent.ID})
	if err != nil {
		t.Fatalf("AgentBoards(tenant B): %v", err)
	}
	if len(boards) != 0 {
		t.Fatalf("tenant B saw tenant A board: %+v", boards)
	}
	matches, _, err := s.SearchAndReadAgentBoards(WithTenant(t.Context(), tenantB), BoardTaskFilter{Agents: []AccountID{agent.ID}})
	if err != nil {
		t.Fatalf("SearchAgentBoards(tenant B): %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("tenant B saw tenant A task matches: %+v", matches)
	}
}

func TestApplyBoardPatchRejectsBoardFieldCharacterLimits(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "board-field-limit-owner")
	agent := mustAgent(t, s, owner.ID, "board-field-limit-agent")
	title := "boundary task"
	waiting := boardJSON(t, []map[string]any{{"on": "BOARD_WAITING_ON_OPERATOR", "agent_handle": strings.Repeat("h", 64)}})
	overlongWaiting := boardJSON(t, []map[string]any{{"on": "BOARD_WAITING_ON_OPERATOR", "agent_handle": strings.Repeat("h", 65)}})
	subagents := boardJSON(t, []map[string]any{{
		"id": strings.Repeat("i", 64), "agent": strings.Repeat("a", 64),
		"description": strings.Repeat("d", 200), "status": strings.Repeat("s", 64),
	}})
	if _, err := s.ApplyBoardPatch(t.Context(), agent.ID, BoardPatch{Upsert: []BoardTaskPatch{
		{Key: strings.Repeat("k", 64), Title: &title, Waiting: &waiting, UpsertSubagents: subagents},
		boardTask("probe", title),
	}, NowUnixMs: 1}); err != nil {
		t.Fatalf("ApplyBoardPatch(at size limits): %v", err)
	}
	badNULWaiting := boardJSON(t, []map[string]any{{"on": "BOARD_WAITING_ON_OPERATOR", "what": "bad\u0000text"}})
	badNULWaitingRef := boardJSON(t, []map[string]any{{"on": "BOARD_WAITING_ON_OPERATOR", "ref": "bad\u0000ref"}})
	badNULWaitingBlocks := boardJSON(t, []map[string]any{{"on": "BOARD_WAITING_ON_OPERATOR", "blocks": []string{"bad\u0000ref"}}})
	badNULSubagentID := boardJSON(t, []map[string]any{{"id": "bad\u0000id"}})
	badNULSubagentAgent := boardJSON(t, []map[string]any{{"id": "child-agent-nul", "agent": "bad\u0000agent"}})
	badNULSubagentDescription := boardJSON(t, []map[string]any{{"id": "child-description-nul", "description": "bad\u0000text"}})
	badNULSubagentStatus := boardJSON(t, []map[string]any{{"id": "child-status-nul", "status": "bad\u0000text"}})
	overlongSubagentStatus := boardJSON(t, []map[string]any{{"id": "child-status", "status": strings.Repeat("s", 65)}})
	badNULHaltReason := "bad\u0000text"
	badNULHaltRef := "bad\u0000ref"
	badNULFocus := "bad\u0000key"
	badNULPrimaryRef := "bad\u0000ref"
	badNULTaskTitle := "bad\u0000title"
	badNULTaskNote := "bad\u0000note"
	tests := []struct {
		name  string
		patch BoardPatch
	}{
		{name: "task key", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: strings.Repeat("k", 65), Title: &title}}}},
		{name: "waiting agent handle", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "probe", Waiting: &overlongWaiting}}}},
		{name: "subagent id length", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "probe", UpsertSubagents: boardJSON(t, []map[string]any{{"id": strings.Repeat("i", 65)}})}}}},
		{name: "subagent agent length", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "probe", UpsertSubagents: boardJSON(t, []map[string]any{{"id": "child", "agent": strings.Repeat("a", 65)}})}}}},
		{name: "subagent description length", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "probe", UpsertSubagents: boardJSON(t, []map[string]any{{"id": "child", "description": strings.Repeat("d", 201)}})}}}},
		{name: "subagent status length", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "probe", UpsertSubagents: overlongSubagentStatus}}}},
		{name: "waiting NUL", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "probe", Waiting: &badNULWaiting}}}},
		{name: "subagent id NUL", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "probe", UpsertSubagents: badNULSubagentID}}}},
		{name: "subagent agent NUL", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "probe", UpsertSubagents: badNULSubagentAgent}}}},
		{name: "subagent description NUL", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "probe", UpsertSubagents: badNULSubagentDescription}}}},
		{name: "subagent status NUL", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "probe", UpsertSubagents: badNULSubagentStatus}}}},
		{name: "halt reason NUL", patch: BoardPatch{Halt: &BoardHalt{Kind: 1, Reason: badNULHaltReason}}},
		{name: "focus NUL", patch: BoardPatch{Focus: &badNULFocus}},
		{name: "task title NUL", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "probe", Title: &badNULTaskTitle}}}},
		{name: "task note NUL", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "probe", Title: &title, Note: &badNULTaskNote}}}},
		{name: "focus key NUL", patch: BoardPatch{Focus: &badNULFocus}},
		{name: "task refs NUL", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "probe", Title: &title, Refs: &[]string{"bad\u0000ref"}}}}},
		{name: "waiting ref NUL", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "probe", Waiting: &badNULWaitingRef}}}},
		{name: "waiting blocks NUL", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "probe", Waiting: &badNULWaitingBlocks}}}},
		{name: "halt ref NUL", patch: BoardPatch{Halt: &BoardHalt{Kind: 1, Ref: badNULHaltRef}}},
		{name: "primary ref NUL", patch: BoardPatch{Upsert: []BoardTaskPatch{{Key: "probe", Title: &title, PrimaryRef: &badNULPrimaryRef}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.patch.NowUnixMs = 2
			_, err := s.ApplyBoardPatch(t.Context(), agent.ID, tc.patch)
			sentinelIs(t, err, ErrInvalidArgument, "invalid "+tc.name)
		})
	}
}
