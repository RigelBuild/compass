package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

const (
	// MaxBoardTasks is the maximum number of tasks an agent board stores.
	MaxBoardTasks = 30
	// MaxBoardRefs is the maximum number of refs on one board task or waiting entry.
	MaxBoardRefs = 10
	// MaxBoardWaiting is the maximum number of waiting entries on one board task.
	MaxBoardWaiting = 5
	// MaxBoardSubagents is the maximum number of subagents on one board task.
	MaxBoardSubagents = 20
	// MaxBoardSearchAgents is the maximum number of agents returned by a board search.
	MaxBoardSearchAgents = 100
	maxBoardAsks         = 20
	// MaxBoardTaskKeyChars limits task keys.
	MaxBoardTaskKeyChars = 64
	// MaxBoardHandleChars limits waiting-entry agent handles.
	MaxBoardHandleChars = 64
	// MaxBoardSubagentIDChars limits subagent identifiers.
	MaxBoardSubagentIDChars = 64
	// MaxBoardSubagentAgentChars limits subagent agent names.
	MaxBoardSubagentAgentChars = 64
	// MaxBoardSubagentDescriptionChars limits subagent descriptions.
	MaxBoardSubagentDescriptionChars = 200
	// MaxBoardSubagentStatusChars limits subagent status strings.
	MaxBoardSubagentStatusChars = 64
	// MaxBoardTitleChars limits task titles.
	MaxBoardTitleChars = 120
	// MaxBoardNoteChars limits task notes.
	MaxBoardNoteChars = 500
	// MaxBoardHaltReasonChars limits board halt reasons.
	MaxBoardHaltReasonChars = 200
	// MaxBoardWhatChars limits waiting-entry explanations.
	MaxBoardWhatChars = 200
)

// BoardTaskPatch updates only fields that are present in the patch.
type BoardTaskPatch struct {
	Key             string
	Title           *string
	PrimaryRef      *string
	Note            *string
	State           *int16
	Refs            *[]string
	Waiting         *[]byte
	UpsertSubagents []byte
}

// BoardPatch is a presence-aware update to one agent's board.
type BoardPatch struct {
	Upsert    []BoardTaskPatch
	Remove    []string
	Focus     *string
	Halt      *BoardHalt
	ClearHalt bool
	Turn      uint64
	Automatic bool
	NowUnixMs int64
}

// BoardHalt describes why the agent has stopped or is waiting.
type BoardHalt struct {
	Kind   int16
	Ref    string
	Reason string
}

// BoardRow is the stored board metadata and its ordered tasks.
type BoardRow struct {
	Agent           AccountID
	Focus           string
	Halt            BoardHalt
	WrittenTurn     uint64
	WrittenAtUnixMs int64
	Tasks           []BoardTaskRow
}

// BoardTaskRow is one stored task and its agent-authored annotations.
type BoardTaskRow struct {
	Key             string
	Title           string
	PrimaryRef      string
	Note            string
	Refs            []string
	State           int16
	Waiting         []byte
	Subagents       []byte
	WrittenAtUnixMs int64
}

// BoardTaskFilter selects tasks across an optional agent set.
type BoardTaskFilter struct {
	Agents    []AccountID
	Query     string
	Ref       string
	WaitingOn int16
	State     int16
	Halt      int16
	Limit     int
}

// AskRow is a visible ask message authored by one agent.
type AskRow struct {
	AskID          string
	MessageID      string
	ChannelID      string
	Ref            string
	Header         string
	Answered       bool
	PostedAtUnixMs int64
}

// AuthoredRef is an open forge coordinate owned by an agent.
type AuthoredRef struct {
	Coord ForgeCoord
	Kind  ForgeArtifactKind
}

// ApplyBoardPatch validates and applies one complete patch atomically.
func (s *Store) ApplyBoardPatch(ctx context.Context, agent AccountID, p BoardPatch) (result BoardRow, retErr error) {
	if agent == "" {
		return BoardRow{}, fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	if !p.Automatic && p.Turn > math.MaxInt64 {
		return BoardRow{}, fmt.Errorf("%w: board turn %d exceeds signed BIGINT range", ErrInvalidArgument, p.Turn)
	}

	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return BoardRow{}, fmt.Errorf("store: begin board patch: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			retErr = errors.Join(retErr, fmt.Errorf("store: roll back board patch: %w", rollbackErr))
		}
	}()
	qtx := s.q.WithTx(tx)
	if err := lockBoardAgent(ctx, qtx, agent); err != nil {
		return BoardRow{}, err
	}
	current, positions, err := boardForAgent(ctx, qtx, agent)
	if err != nil {
		return BoardRow{}, err
	}
	if p.Automatic && emptyBoardPatch(p) {
		return current, nil
	}

	next, positions, err := mergeBoardPatch(current, positions, p)
	if err != nil {
		return BoardRow{}, err
	}
	if len(next.Tasks) > MaxBoardTasks {
		return BoardRow{}, fmt.Errorf("%w: board has %d tasks; maximum is %d", ErrInvalidArgument, len(next.Tasks), MaxBoardTasks)
	}
	if next.Focus != "" && !boardHasTask(next.Tasks, next.Focus) {
		return BoardRow{}, fmt.Errorf("%w: focus task %q is not on the board", ErrInvalidArgument, next.Focus)
	}
	if err := upsertBoardMetadata(ctx, qtx, agent, current.Focus, next, p); err != nil {
		return BoardRow{}, err
	}
	if err := persistBoardTasks(ctx, qtx, agent, next, positions, p); err != nil {
		return BoardRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BoardRow{}, fmt.Errorf("store: commit board patch: %w", err)
	}
	return next, nil
}

func emptyBoardPatch(p BoardPatch) bool {
	return len(p.Upsert) == 0 && len(p.Remove) == 0 && p.Focus == nil && p.Halt == nil && !p.ClearHalt
}

func lockBoardAgent(ctx context.Context, q *db.Queries, agent AccountID) error {
	_, err := q.LockBoardAgent(ctx, string(agent))
	if noRows(err) {
		return fmt.Errorf("%w: agent account %q does not exist", ErrInvalidArgument, agent)
	}
	if err != nil {
		return fmt.Errorf("store: lock board agent: %w", err)
	}
	return nil
}

func boardForAgent(ctx context.Context, q *db.Queries, agent AccountID) (BoardRow, map[string]int32, error) {
	out := BoardRow{Agent: agent, Tasks: make([]BoardTaskRow, 0)}
	positions := make(map[string]int32)
	boards, err := q.GetAgentBoards(ctx, []string{string(agent)})
	if err != nil {
		return BoardRow{}, nil, fmt.Errorf("store: read agent board: %w", err)
	}
	if len(boards) > 0 {
		r := boards[0]
		out.Focus = r.FocusTaskKey
		out.Halt = BoardHalt{Kind: r.HaltKind, Ref: r.HaltRef, Reason: r.HaltReason}
		out.WrittenTurn = uint64(r.WrittenTurn) //nolint:gosec // schema writes nonnegative turns within signed BIGINT
		out.WrittenAtUnixMs = r.WrittenAtUnixMs
	}
	tasks, err := q.GetAgentBoardTasks(ctx, []string{string(agent)})
	if err != nil {
		return BoardRow{}, nil, fmt.Errorf("store: read agent board tasks: %w", err)
	}
	for _, row := range tasks {
		refs := row.Refs
		if len(refs) == 0 {
			refs = nil
		}
		positions[row.TaskKey] = row.Position
		out.Tasks = append(out.Tasks, BoardTaskRow{
			Key: row.TaskKey, Title: row.Title, PrimaryRef: row.PrimaryRef, Note: row.Note,
			Refs: refs, State: row.State, Waiting: row.Waiting, Subagents: row.Subagents,
			WrittenAtUnixMs: row.WrittenAtUnixMs,
		})
	}
	return out, positions, nil
}

func mergeBoardPatch(current BoardRow, positions map[string]int32, p BoardPatch) (BoardRow, map[string]int32, error) {
	if err := validateHalt(current.Halt); err != nil {
		return BoardRow{}, nil, err
	}
	next := current
	next.Tasks = append([]BoardTaskRow(nil), current.Tasks...)
	for _, row := range current.Tasks {
		if err := validateTask(row); err != nil {
			return BoardRow{}, nil, fmt.Errorf("store: stored board task %q is invalid: %w", row.Key, err)
		}
	}
	if err := removeBoardTasks(&next, positions, p.Remove); err != nil {
		return BoardRow{}, nil, err
	}
	if err := applyBoardTaskUpserts(&next, positions, p); err != nil {
		return BoardRow{}, nil, err
	}
	if p.Focus != nil {
		if err := validateBoardTaskKey(*p.Focus); err != nil {
			return BoardRow{}, nil, err
		}
		next.Focus = *p.Focus
	}
	if err := applyBoardHalt(&next, p); err != nil {
		return BoardRow{}, nil, err
	}
	if !p.Automatic {
		next.WrittenTurn = p.Turn
		next.WrittenAtUnixMs = p.NowUnixMs
	}
	return next, positions, nil
}

func applyBoardTaskUpserts(next *BoardRow, positions map[string]int32, p BoardPatch) error {
	for _, patch := range p.Upsert {
		if patch.Key == "" {
			return fmt.Errorf("%w: task key is required", ErrInvalidArgument)
		}
		if err := validateBoardTaskKey(patch.Key); err != nil {
			return err
		}
		index := boardTaskIndex(next.Tasks, patch.Key)
		if index < 0 {
			if patch.Title == nil {
				return fmt.Errorf("%w: new task %q requires a title", ErrInvalidArgument, patch.Key)
			}
			if len(next.Tasks) == MaxBoardTasks {
				return fmt.Errorf("%w: board exceeds %d tasks", ErrInvalidArgument, MaxBoardTasks)
			}
			position, err := nextBoardPosition(positions)
			if err != nil {
				return err
			}
			positions[patch.Key] = position
			next.Tasks = append(next.Tasks, BoardTaskRow{
				Key: patch.Key, State: 1, Refs: []string{}, Waiting: []byte("[]"), Subagents: []byte("[]"),
			})
			index = len(next.Tasks) - 1
		}
		updated, err := applyTaskPatch(next.Tasks[index], patch)
		if err != nil {
			return err
		}
		if !p.Automatic {
			updated.WrittenAtUnixMs = p.NowUnixMs
		}
		next.Tasks[index] = updated
	}
	return nil
}

func boardTaskIndex(tasks []BoardTaskRow, key string) int {
	for i := range tasks {
		if tasks[i].Key == key {
			return i
		}
	}
	return -1
}

func applyBoardHalt(next *BoardRow, p BoardPatch) error {
	if p.Halt != nil {
		if err := validateHalt(*p.Halt); err != nil {
			return err
		}
		next.Halt = *p.Halt
	}
	if p.ClearHalt {
		next.Halt = BoardHalt{}
	}
	return nil
}

func removeBoardTasks(next *BoardRow, positions map[string]int32, keys []string) error {
	remove := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if key == "" {
			return fmt.Errorf("%w: removed task key is required", ErrInvalidArgument)
		}
		if err := validateBoardTaskKey(key); err != nil {
			return err
		}
		remove[key] = struct{}{}
		delete(positions, key)
		if next.Focus == key {
			next.Focus = ""
		}
	}
	kept := next.Tasks[:0]
	for _, task := range next.Tasks {
		if _, removed := remove[task.Key]; !removed {
			kept = append(kept, task)
		}
	}
	next.Tasks = kept
	return nil
}

func nextBoardPosition(positions map[string]int32) (int32, error) {
	var highest int32 = -1
	for _, position := range positions {
		if position > highest {
			highest = position
		}
	}
	if highest == math.MaxInt32 {
		return 0, fmt.Errorf("%w: task position exceeds signed INT range", ErrInvalidArgument)
	}
	return highest + 1, nil
}
func applyTaskPatch(row BoardTaskRow, patch BoardTaskPatch) (BoardTaskRow, error) {
	if patch.Title != nil {
		row.Title = *patch.Title
	}
	if patch.PrimaryRef != nil {
		row.PrimaryRef = *patch.PrimaryRef
	}
	if patch.Note != nil {
		row.Note = *patch.Note
	}
	if patch.State != nil {
		row.State = *patch.State
	}
	if patch.Refs != nil {
		row.Refs = append([]string(nil), (*patch.Refs)...)
	}
	if err := validateBoardNoNUL("board task primary ref", row.PrimaryRef, ErrInvalidArgument); err != nil {
		return BoardTaskRow{}, err
	}
	for _, ref := range row.Refs {
		if err := validateBoardNoNUL("board task ref", ref, ErrInvalidArgument); err != nil {
			return BoardTaskRow{}, err
		}
	}
	if patch.Waiting != nil {
		waiting, err := validateWaiting(*patch.Waiting)
		if err != nil {
			return BoardTaskRow{}, err
		}
		row.Waiting = waiting
	}
	if len(patch.UpsertSubagents) != 0 {
		subagents, err := mergeSubagents(row.Subagents, patch.UpsertSubagents)
		if err != nil {
			return BoardTaskRow{}, err
		}
		row.Subagents = subagents
	}
	if err := validateTask(row); err != nil {
		return BoardTaskRow{}, err
	}
	return row, nil
}

func validateTask(row BoardTaskRow) error {
	if err := validateBoardTaskKey(row.Key); err != nil {
		return err
	}
	if row.Title == "" {
		return fmt.Errorf("%w: task %q title is required", ErrInvalidArgument, row.Key)
	}
	if err := validateBoardString("board task title", row.Title, MaxBoardTitleChars, ErrInvalidArgument); err != nil {
		return err
	}
	if err := validateBoardString("board task note", row.Note, MaxBoardNoteChars, ErrInvalidArgument); err != nil {
		return err
	}
	if len(row.Refs) > MaxBoardRefs {
		return fmt.Errorf("%w: task %q has %d refs; maximum is %d", ErrInvalidArgument, row.Key, len(row.Refs), MaxBoardRefs)
	}
	if row.State < 1 || row.State > 5 {
		return fmt.Errorf("%w: task %q has invalid state %d", ErrInvalidArgument, row.Key, row.State)
	}
	if _, err := validateWaiting(row.Waiting); err != nil {
		return err
	}
	subagents, err := validateSubagents(row.Subagents, false)
	if err != nil {
		return err
	}
	if len(subagents) > MaxBoardSubagents {
		return fmt.Errorf("%w: task %q has %d subagents; maximum is %d", ErrInvalidArgument, row.Key, len(subagents), MaxBoardSubagents)
	}
	return nil
}

func validateBoardTaskKey(key string) error {
	return validateBoardString("board task key", key, MaxBoardTaskKeyChars, ErrInvalidArgument)
}

func validateHalt(halt BoardHalt) error {
	if halt.Kind < 0 || halt.Kind > 3 {
		return fmt.Errorf("%w: invalid halt kind %d", ErrInvalidArgument, halt.Kind)
	}
	if err := validateBoardNoNUL("board halt ref", halt.Ref, ErrInvalidArgument); err != nil {
		return err
	}
	return validateBoardString("board halt reason", halt.Reason, MaxBoardHaltReasonChars, ErrInvalidArgument)
}

func validateWaiting(raw []byte) ([]byte, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		return nil, fmt.Errorf("%w: waiting must be a JSON array", ErrInvalidArgument)
	}
	if len(entries) > MaxBoardWaiting {
		return nil, fmt.Errorf("%w: task has %d waiting entries; maximum is %d", ErrInvalidArgument, len(entries), MaxBoardWaiting)
	}
	for _, entry := range entries {
		if err := validateWaitingEntry(entry); err != nil {
			return nil, err
		}
	}
	if len(entries) == 0 {
		return []byte("[]"), nil
	}
	return append([]byte(nil), raw...), nil
}

func validateWaitingEntry(entry json.RawMessage) error {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(entry, &value); err != nil || value == nil {
		return fmt.Errorf("%w: waiting entry must be an object", ErrInvalidArgument)
	}
	if err := validateOptionalWaitingString(value, "agent_handle", "waiting agent_handle", MaxBoardHandleChars, true); err != nil {
		return err
	}
	if err := validateWaitingOn(value); err != nil {
		return err
	}
	if err := validateOptionalWaitingString(value, "what", "waiting what", MaxBoardWhatChars, true); err != nil {
		return err
	}
	if err := validateOptionalWaitingString(value, "ref", "waiting ref", 0, false); err != nil {
		return err
	}
	return validateWaitingBlocks(value["blocks"])
}

func validateOptionalWaitingString(value map[string]json.RawMessage, key, label string, maxChars int, bounded bool) error {
	raw, found := value[key]
	if !found {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return fmt.Errorf("%w: %s must be a string", ErrInvalidArgument, key)
	}
	if bounded {
		return validateBoardString(label, text, maxChars, ErrInvalidArgument)
	}
	return validateBoardNoNUL(label, text, ErrInvalidArgument)
}

func validateWaitingOn(value map[string]json.RawMessage) error {
	raw, found := value["on"]
	var on string
	if !found || json.Unmarshal(raw, &on) != nil {
		return fmt.Errorf("%w: waiting on is required", ErrInvalidArgument)
	}
	if on != "BOARD_WAITING_ON_OPERATOR" && on != "BOARD_WAITING_ON_AGENT" && on != "BOARD_WAITING_ON_EXTERNAL" {
		return fmt.Errorf("%w: waiting on must be a recognized BOARD_WAITING_ON value", ErrInvalidArgument)
	}
	return nil
}

func validateWaitingBlocks(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var blocks []string
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return fmt.Errorf("%w: waiting blocks must be strings", ErrInvalidArgument)
	}
	for _, ref := range blocks {
		if err := validateBoardNoNUL("waiting block ref", ref, ErrInvalidArgument); err != nil {
			return err
		}
	}
	return nil
}

func mergeSubagents(existing, updates []byte) ([]byte, error) {
	current, err := validateSubagents(existing, false)
	if err != nil {
		return nil, fmt.Errorf("store: decode stored subagents: %w", err)
	}
	incoming, err := validateSubagents(updates, true)
	if err != nil {
		return nil, err
	}
	positions := make(map[string]int, len(current)+len(incoming))
	for i, item := range current {
		id, err := subagentID(item)
		if err != nil {
			return nil, fmt.Errorf("store: decode stored subagent id: %w", err)
		}
		positions[id] = i
	}
	for _, item := range incoming {
		id, err := subagentID(item)
		if err != nil {
			return nil, fmt.Errorf("%w: subagent id is required", ErrInvalidArgument)
		}
		if index, found := positions[id]; found {
			current[index] = item
			continue
		}
		positions[id] = len(current)
		current = append(current, item)
	}
	for len(current) > MaxBoardSubagents {
		oldest := -1
		oldestAt := int64(math.MaxInt64)
		for i, item := range current {
			settled, startedAt := subagentSettled(item)
			if settled && (oldest < 0 || startedAt < oldestAt) {
				oldest = i
				oldestAt = startedAt
			}
		}
		if oldest < 0 {
			return nil, fmt.Errorf("%w: task has more than %d unsettled subagents", ErrInvalidArgument, MaxBoardSubagents)
		}
		current = append(current[:oldest], current[oldest+1:]...)
	}
	if current == nil {
		return []byte("[]"), nil
	}
	out, err := json.Marshal(current)
	if err != nil {
		return nil, fmt.Errorf("store: encode subagents: %w", err)
	}
	return out, nil
}

func validateSubagents(raw []byte, callerInput bool) ([]json.RawMessage, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		if callerInput {
			return nil, fmt.Errorf("%w: subagents must be a JSON array", ErrInvalidArgument)
		}
		return nil, errors.New("subagents must be a JSON array")
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		var value map[string]json.RawMessage
		if err := json.Unmarshal(entry, &value); err != nil || value == nil {
			if callerInput {
				return nil, fmt.Errorf("%w: subagent entry must be an object", ErrInvalidArgument)
			}
			return nil, errors.New("subagent entry must be an object")
		}
		id, err := subagentID(entry)
		if err := validateSubagentStrings(value, callerInput); err != nil {
			return nil, err
		}
		if err != nil || id == "" {
			if callerInput {
				return nil, fmt.Errorf("%w: subagent id is required", ErrInvalidArgument)
			}
			return nil, errors.New("subagent id is required")
		}
		if callerInput {
			if _, duplicate := seen[id]; duplicate {
				return nil, fmt.Errorf("%w: duplicate subagent id %q", ErrInvalidArgument, id)
			}
			seen[id] = struct{}{}
		}
	}
	return entries, nil
}

func subagentID(raw json.RawMessage) (string, error) {
	var value struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value.ID, nil
}

func validateSubagentStrings(value map[string]json.RawMessage, callerInput bool) error {
	for _, field := range []struct {
		name string
		max  int
	}{{"id", MaxBoardSubagentIDChars}, {"agent", MaxBoardSubagentAgentChars}, {"description", MaxBoardSubagentDescriptionChars}, {"status", MaxBoardSubagentStatusChars}} {
		raw, found := value[field.name]
		if !found {
			continue
		}
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			if callerInput {
				return fmt.Errorf("%w: subagent %s must be a string", ErrInvalidArgument, field.name)
			}
			return fmt.Errorf("subagent %s must be a string", field.name)
		}
		if callerInput {
			if err := validateBoardString("subagent "+field.name, text, field.max, ErrInvalidArgument); err != nil {
				return err
			}
			continue
		}
		if err := validateBoardNoNUL("subagent "+field.name, text, errors.New("invalid stored subagent")); err != nil {
			return err
		}
		if utf8.RuneCountInString(text) > field.max {
			return fmt.Errorf("subagent %s exceeds %d characters", field.name, field.max)
		}
	}
	return nil
}

func validateBoardString(field, value string, maxChars int, sentinel error) error {
	if err := validateBoardNoNUL(field, value, sentinel); err != nil {
		return err
	}
	if n := utf8.RuneCountInString(value); n > maxChars {
		return fmt.Errorf("%w: %s has %d characters; maximum is %d", sentinel, field, n, maxChars)
	}
	return nil
}

func validateBoardNoNUL(field, value string, sentinel error) error {
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%w: %s contains a NUL character", sentinel, field)
	}
	return nil
}

func subagentSettled(raw json.RawMessage) (bool, int64) {
	var value struct {
		Status               string          `json:"status"`
		EndedAtUnixMs        json.RawMessage `json:"endedAtUnixMs"`
		EndedAtUnixMsSnake   json.RawMessage `json:"ended_at_unix_ms"`
		StartedAtUnixMs      json.RawMessage `json:"startedAtUnixMs"`
		StartedAtUnixMsSnake json.RawMessage `json:"started_at_unix_ms"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, 0
	}
	started, _ := jsonInt64(value.StartedAtUnixMs)
	if started == 0 {
		started, _ = jsonInt64(value.StartedAtUnixMsSnake)
	}
	ended, _ := jsonInt64(value.EndedAtUnixMs)
	if ended == 0 {
		ended, _ = jsonInt64(value.EndedAtUnixMsSnake)
	}
	switch value.Status {
	case "completed", "failed", "aborted":
		return true, started
	default:
		return ended != 0, started
	}
}

func jsonInt64(raw json.RawMessage) (int64, error) {
	var value int64
	if err := json.Unmarshal(raw, &value); err == nil {
		return value, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, err
	}
	return strconv.ParseInt(text, 10, 64)
}

func boardHasTask(tasks []BoardTaskRow, key string) bool {
	for _, task := range tasks {
		if task.Key == key {
			return true
		}
	}
	return false
}

func upsertBoardMetadata(ctx context.Context, q *db.Queries, agent AccountID, currentFocus string, next BoardRow, p BoardPatch) error {
	if err := q.UpsertAgentBoard(ctx, db.UpsertAgentBoardParams{
		AgentAccountID: string(agent),
		Column2:        p.Focus != nil || next.Focus != currentFocus,
		Column3:        next.Focus,
		Column4:        p.Halt != nil,
		Column5:        next.Halt.Kind,
		Column6:        next.Halt.Ref,
		Column7:        next.Halt.Reason,
		Column8:        p.ClearHalt,
		Column9:        p.Automatic,
		Column10:       int64(p.Turn), //nolint:gosec // validated against MaxInt64 before tx
		Column11:       p.NowUnixMs,
	}); err != nil {
		return fmt.Errorf("store: upsert board metadata: %w", err)
	}
	return nil
}

func persistBoardTasks(ctx context.Context, q *db.Queries, agent AccountID, next BoardRow, positions map[string]int32, p BoardPatch) error {
	if len(p.Remove) > 0 {
		if err := q.DeleteAgentBoardTasks(ctx, db.DeleteAgentBoardTasksParams{AgentAccountID: string(agent), Column2: p.Remove}); err != nil {
			return fmt.Errorf("store: delete board tasks: %w", err)
		}
	}
	if len(p.Upsert) == 0 {
		return nil
	}
	rows := make(map[string]BoardTaskRow, len(next.Tasks))
	for _, task := range next.Tasks {
		rows[task.Key] = task
	}
	for _, patch := range p.Upsert {
		task, ok := rows[patch.Key]
		if !ok {
			continue
		}
		if task.Refs == nil {
			task.Refs = []string{}
		}
		if task.Waiting == nil {
			task.Waiting = []byte("[]")
		}
		if task.Subagents == nil {
			task.Subagents = []byte("[]")
		}
		if err := q.UpsertAgentBoardTask(ctx, db.UpsertAgentBoardTaskParams{
			AgentAccountID: string(agent), TaskKey: task.Key, Title: task.Title,
			PrimaryRef: task.PrimaryRef, Refs: task.Refs, State: task.State,
			Note: task.Note, Waiting: task.Waiting, Subagents: task.Subagents,
			Position: positions[task.Key], WrittenAtUnixMs: task.WrittenAtUnixMs,
		}); err != nil {
			return fmt.Errorf("store: upsert board task %q: %w", task.Key, err)
		}
	}
	return nil
}

// AgentBoards returns stored board rows for the requested agents.
func (s *Store) AgentBoards(ctx context.Context, agents []AccountID) (map[AccountID]BoardRow, error) {
	ids := uniqueAccountStrings(agents)
	if len(ids) == 0 {
		return map[AccountID]BoardRow{}, nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("store: begin board snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := armTx(ctx, s, tx); err != nil {
		return nil, err
	}
	rows, err := readAgentBoardRows(ctx, s.q.WithTx(tx), ids, s.afterBoardMetaRead)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit board snapshot: %w", err)
	}
	return rows, nil
}

func readAgentBoardRows(ctx context.Context, q *db.Queries, ids []string, afterMetaRead func()) (map[AccountID]BoardRow, error) {
	out := make(map[AccountID]BoardRow, len(ids))
	boards, err := q.GetAgentBoards(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("store: read agent boards: %w", err)
	}
	if afterMetaRead != nil {
		afterMetaRead()
	}
	for _, row := range boards {
		id := AccountID(row.AgentAccountID)
		if row.WrittenTurn < 0 {
			return nil, fmt.Errorf("store: board %q has negative written turn %d", id, row.WrittenTurn)
		}
		out[id] = BoardRow{
			Agent: id, Focus: row.FocusTaskKey,
			Halt:        BoardHalt{Kind: row.HaltKind, Ref: row.HaltRef, Reason: row.HaltReason},
			WrittenTurn: uint64(row.WrittenTurn), WrittenAtUnixMs: row.WrittenAtUnixMs,
			Tasks: make([]BoardTaskRow, 0),
		}
	}
	tasks, err := q.GetAgentBoardTasks(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("store: read agent board tasks: %w", err)
	}
	for _, row := range tasks {
		id := AccountID(row.AgentAccountID)
		board, found := out[id]
		if !found {
			continue
		}
		refs := row.Refs
		if len(refs) == 0 {
			refs = nil
		}
		board.Tasks = append(board.Tasks, BoardTaskRow{
			Key: row.TaskKey, Title: row.Title, PrimaryRef: row.PrimaryRef,
			Refs: refs, State: row.State, Note: row.Note, Waiting: row.Waiting,
			Subagents: row.Subagents, WrittenAtUnixMs: row.WrittenAtUnixMs,
		})
		out[id] = board
	}
	return out, nil
}

// SearchAndReadAgentBoards returns matching task keys and complete boards from one snapshot.
func (s *Store) SearchAndReadAgentBoards(ctx context.Context, f BoardTaskFilter) (map[AccountID][]string, map[AccountID]BoardRow, error) {
	params := boardSearchParams(f)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, fmt.Errorf("store: begin board search snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := armTx(ctx, s, tx); err != nil {
		return nil, nil, err
	}
	qtx := s.q.WithTx(tx)
	searchRows, err := qtx.SearchAgentBoardTasks(ctx, params)
	if err != nil {
		return nil, nil, fmt.Errorf("store: search agent boards: %w", err)
	}
	matches := make(map[AccountID][]string)
	matchedIDs := make([]string, 0, len(searchRows))
	seen := make(map[string]struct{}, len(searchRows))
	for _, row := range searchRows {
		id := AccountID(row.AgentAccountID)
		if _, found := matches[id]; !found {
			matches[id] = nil
		}
		if row.TaskKey != "" {
			matches[id] = append(matches[id], row.TaskKey)
		}
		if _, found := seen[row.AgentAccountID]; !found {
			seen[row.AgentAccountID] = struct{}{}
			matchedIDs = append(matchedIDs, row.AgentAccountID)
		}
	}
	if len(matchedIDs) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, fmt.Errorf("store: commit board search snapshot: %w", err)
		}
		return matches, map[AccountID]BoardRow{}, nil
	}
	rows, err := readAgentBoardRows(ctx, qtx, matchedIDs, s.afterBoardMetaRead)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("store: commit board search snapshot: %w", err)
	}
	return matches, rows, nil
}

func boardSearchParams(f BoardTaskFilter) db.SearchAgentBoardTasksParams {
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	} else if limit > MaxBoardSearchAgents {
		limit = MaxBoardSearchAgents
	}
	return db.SearchAgentBoardTasksParams{
		AgentAccountIds: uniqueAccountStrings(f.Agents), Query: strings.TrimSpace(f.Query), AgentLimit: int32(limit),
		Ref: f.Ref, WaitingOn: f.WaitingOn, State: f.State, Halt: f.Halt,
	}
}

func uniqueAccountStrings(agents []AccountID) []string {
	out := make([]string, 0, len(agents))
	seen := make(map[AccountID]struct{}, len(agents))
	for _, agent := range agents {
		if agent == "" {
			continue
		}
		if _, found := seen[agent]; found {
			continue
		}
		seen[agent] = struct{}{}
		out = append(out, string(agent))
	}
	return out
}

// AsksByAuthor returns recent answered asks and every pending ask visible to viewer.
func (s *Store) AsksByAuthor(ctx context.Context, viewer, author AccountID, answeredSince time.Time, limit int) ([]AskRow, error) {
	if viewer == "" || author == "" {
		return nil, fmt.Errorf("%w: viewer and author account ids are required", ErrInvalidArgument)
	}
	if limit <= 0 || limit > maxBoardAsks {
		limit = maxBoardAsks
	}
	rows, err := s.q.ListAsksByAuthor(ctx, db.ListAsksByAuthorParams{
		AccountID: string(viewer), AuthorAccountID: string(author),
		AnsweredSince: answeredSince.UnixMilli(), RowLimit: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("store: list asks by author: %w", err)
	}
	out := make([]AskRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, AskRow{
			AskID: row.AskID, MessageID: row.MessageID, ChannelID: row.ChannelID,
			Ref: row.Ref, Header: row.Header, Answered: row.Answered, PostedAtUnixMs: row.AtUnixMs,
		})
	}
	return out, nil
}

// AuthoredCoordsForAgents returns authored coordinates for agents that intersect a batch.
func (s *Store) AuthoredCoordsForAgents(ctx context.Context, agents []AccountID, coords []ForgeCoord) (map[AccountID][]AuthoredRef, error) {
	if len(agents) == 0 {
		return map[AccountID][]AuthoredRef{}, nil
	}
	if slices.Contains(agents, "") {
		return nil, fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	want, err := normalizeCoords(coords)
	if err != nil {
		return nil, err
	}
	if len(want) == 0 {
		return map[AccountID][]AuthoredRef{}, nil
	}
	arg := db.AuthoredCoordsForAgentsParams{
		AgentAccountIds: uniqueAccountStrings(agents),
		ForgeProviders:  make([]int16, 0, len(want)),
		ForgeHosts:      make([]string, 0, len(want)),
		ForgeRepos:      make([]string, 0, len(want)),
		ForgeNumbers:    make([]int64, 0, len(want)),
	}
	for _, coord := range want {
		value := coord.db()
		arg.ForgeProviders = append(arg.ForgeProviders, value.provider)
		arg.ForgeHosts = append(arg.ForgeHosts, value.host)
		arg.ForgeRepos = append(arg.ForgeRepos, value.repo)
		arg.ForgeNumbers = append(arg.ForgeNumbers, value.number)
	}
	rows, err := s.q.AuthoredCoordsForAgents(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("store: authored coordinates for agents: %w", err)
	}
	out := make(map[AccountID][]AuthoredRef, len(agents))
	for _, row := range rows {
		agent := AccountID(row.AgentAccountID)
		out[agent] = append(out[agent], AuthoredRef{
			Coord: coordFromDB(row.ForgeProvider, row.ForgeHost, row.Repo, row.Number),
			Kind:  ForgeArtifactKind(row.Kind),
		})
	}
	return out, nil
}

// OpenAuthoredRefsForAgents returns each agent's authored issues and PRs that remain open.
func (s *Store) OpenAuthoredRefsForAgents(ctx context.Context, agents []AccountID) (map[AccountID][]AuthoredRef, error) {
	if len(agents) == 0 {
		return map[AccountID][]AuthoredRef{}, nil
	}
	if slices.Contains(agents, "") {
		return nil, fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	ids := uniqueAccountStrings(agents)
	rows, err := s.q.ListOpenAuthoredRefsForAgents(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("store: list open authored refs for agents: %w", err)
	}
	out := make(map[AccountID][]AuthoredRef, len(ids))
	for _, row := range rows {
		agent := AccountID(row.AgentAccountID)
		out[agent] = append(out[agent], AuthoredRef{
			Coord: coordFromDB(row.ForgeProvider, row.ForgeHost, row.Repo, row.Number),
			Kind:  ForgeArtifactKind(row.Kind),
		})
	}
	return out, nil
}

// PullRequestsByCoords reads stored pull request rows for normalized coordinates.
func (s *Store) PullRequestsByCoords(ctx context.Context, coords []ForgeCoord) ([]PullRequestRow, error) {
	want, err := normalizeCoords(coords)
	if err != nil {
		return nil, err
	}
	if len(want) == 0 {
		return nil, nil
	}
	arg := db.PullRequestsByCoordsParams{
		ForgeProviders: make([]int16, 0, len(want)), ForgeHosts: make([]string, 0, len(want)),
		ForgeRepos: make([]string, 0, len(want)), ForgeNumbers: make([]int64, 0, len(want)),
	}
	for _, coord := range want {
		value := coord.db()
		arg.ForgeProviders = append(arg.ForgeProviders, value.provider)
		arg.ForgeHosts = append(arg.ForgeHosts, value.host)
		arg.ForgeRepos = append(arg.ForgeRepos, value.repo)
		arg.ForgeNumbers = append(arg.ForgeNumbers, value.number)
	}
	rows, err := s.q.PullRequestsByCoords(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("store: pull requests by coordinates: %w", err)
	}
	out := make([]PullRequestRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, PullRequestRow{
			Coord: coordFromDB(row.ForgeProvider, row.ForgeHost, row.Repo, row.Number),
			State: row.ForgeState, CreatedAt: row.ForgeCreatedAt.Time,
			UpdatedAt: row.ForgeUpdatedAt.Time, PR: row.Pr,
		})
	}
	return out, nil
}

// IssuesByCoords reads stored issue rows for normalized coordinates.
func (s *Store) IssuesByCoords(ctx context.Context, coords []ForgeCoord) ([]Issue, error) {
	want, err := normalizeCoords(coords)
	if err != nil {
		return nil, err
	}
	if len(want) == 0 {
		return nil, nil
	}
	arg := db.IssuesByCoordsParams{
		ForgeProviders: make([]int16, 0, len(want)), ForgeHosts: make([]string, 0, len(want)),
		ForgeRepos: make([]string, 0, len(want)), ForgeNumbers: make([]int64, 0, len(want)),
	}
	for _, coord := range want {
		value := coord.db()
		arg.ForgeProviders = append(arg.ForgeProviders, value.provider)
		arg.ForgeHosts = append(arg.ForgeHosts, value.host)
		arg.ForgeRepos = append(arg.ForgeRepos, value.repo)
		arg.ForgeNumbers = append(arg.ForgeNumbers, value.number)
	}
	rows, err := s.q.IssuesByCoords(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("store: issues by coordinates: %w", err)
	}
	out := make([]Issue, 0, len(rows))
	for _, row := range rows {
		out = append(out, issueFromColumns(
			row.ID, row.ForgeProvider, row.ForgeHost, row.Repo, row.Number,
			row.Title, row.Body, row.ForgeState, row.Url, row.ForgeAccount, row.Labels, row.AgentHandle,
			row.ForgeUpdatedAt, row.State, row.Priority, row.Assignee, row.Summary, row.Branch,
		))
	}
	return out, nil
}
