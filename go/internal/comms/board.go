//go:build unix

package comms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/refs"
	"github.com/RigelBuild/compass/go/internal/store"
	"google.golang.org/protobuf/proto"
)

const (
	boardStaleAfter = 30 * time.Minute
	boardAskWindow  = 24 * time.Hour
)

// UpdateBoardAsAccount validates and applies an agent patch before publishing the committed board.
func (c *Comms) UpdateBoardAsAccount(
	ctx context.Context,
	caller store.AccountID,
	req *compassv1.UpdateAgentBoardRequest,
) (*compassv1.UpdateAgentBoardResponse, error) {
	if caller == "" {
		return nil, errNoActor
	}
	if req == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("board update request is required"))
	}
	if req.GetHalt() != nil && req.GetClearHalt() {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("board update cannot set and clear halt together"))
	}
	patch, err := boardPatchFromWire(req)
	if err != nil {
		return nil, err
	}
	account, err := c.store.GetAccount(ctx, caller)
	if err != nil {
		return nil, edgeError(err)
	}
	if !account.IsAgent() {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only an agent can update its board"))
	}
	row, err := c.store.ApplyBoardPatch(ctx, caller, patch)
	if err != nil {
		return nil, edgeError(err)
	}
	if c.driver != nil {
		go c.driver.DriveAgent(context.WithoutCancel(ctx), caller)
	}
	authored := boardAuthoredToWire(account, row, nil)
	if authored.decodeErr != nil {
		slog.ErrorContext(ctx, "comms: board decode failed after committed update", "agent_account_id", string(caller), "error", authored.decodeErr)
	}
	if (!patch.Automatic || !emptyBoardPatch(patch)) && c.bus != nil {
		eventBoard, ok := proto.Clone(authored.wire).(*compassv1.AgentBoard)
		if !ok {
			slog.ErrorContext(ctx, "comms: board event clone has unexpected type", "agent_account_id", string(caller))
		} else {
			c.bus.Publish(&compassv1.SubscribeCommsResponse{
				Payload: &compassv1.SubscribeCommsResponse_AgentBoardChanged{
					AgentBoardChanged: &compassv1.AgentBoardChanged{
						AgentAccountId: string(caller),
						Board:          eventBoard,
					},
				},
			})
		}
	}
	wire := authored.wire
	completed, completeErr := c.completeBoardWire(ctx, caller, account, row, authored.wire, authored.refs)
	if completeErr != nil {
		slog.WarnContext(ctx, "comms: board refs unavailable after update", "agent_account_id", string(caller), "error", completeErr)
	} else {
		wire = completed
	}
	c.fillAskMirrors(ctx, []*compassv1.AgentBoard{wire})
	return &compassv1.UpdateAgentBoardResponse{Board: wire}, nil
}

func boardPatchFromWire(req *compassv1.UpdateAgentBoardRequest) (store.BoardPatch, error) {
	if len(req.GetUpsertTasks()) > store.MaxBoardTasks {
		return store.BoardPatch{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("board task patch count exceeds maximum of %d", store.MaxBoardTasks))
	}
	if len(req.GetRemoveTaskKeys()) > store.MaxBoardTasks {
		return store.BoardPatch{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("board task removal count exceeds maximum of %d", store.MaxBoardTasks))
	}
	for _, key := range req.GetRemoveTaskKeys() {
		if err := validateBoardWireString("board task key", key, store.MaxBoardTaskKeyChars); err != nil {
			return store.BoardPatch{}, err
		}
	}
	patch := store.BoardPatch{Remove: req.GetRemoveTaskKeys(), Turn: req.GetTurnSequence(), Automatic: req.GetAutomatic(), NowUnixMs: time.Now().UnixMilli()}
	patch.Focus = req.FocusTaskKey
	patch.ClearHalt = req.GetClearHalt()
	if req.FocusTaskKey != nil {
		if err := validateBoardWireString("board focus task key", req.GetFocusTaskKey(), store.MaxBoardTaskKeyChars); err != nil {
			return store.BoardPatch{}, err
		}
	}
	if req.GetHalt() != nil {
		halt, err := haltFromWire(req.GetHalt())
		if err != nil {
			return store.BoardPatch{}, err
		}
		patch.Halt = &halt
	}
	patch.Upsert = make([]store.BoardTaskPatch, 0, len(req.GetUpsertTasks()))
	for _, task := range req.GetUpsertTasks() {
		if task == nil {
			return store.BoardPatch{}, connect.NewError(connect.CodeInvalidArgument, errors.New("board task patch is required"))
		}
		row, err := boardTaskPatchFromWire(task)
		if err != nil {
			return store.BoardPatch{}, err
		}
		patch.Upsert = append(patch.Upsert, row)
	}
	return patch, nil
}

func boardTaskPatchFromWire(task *compassv1.BoardTaskPatch) (store.BoardTaskPatch, error) {
	if err := validateBoardTaskPatchWire(task); err != nil {
		return store.BoardTaskPatch{}, err
	}
	var state *int16
	if task.State != nil {
		value, valid := boardTaskStateToStore(task.GetState())
		if !valid {
			return store.BoardTaskPatch{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("board task %q has invalid state %d", task.GetKey(), task.GetState()))
		}
		state = &value
	}
	subagents, err := boardSubagentsFromWire(task.GetUpsertSubagents())
	if err != nil {
		return store.BoardTaskPatch{}, err
	}
	row := store.BoardTaskPatch{Key: task.GetKey(), Title: task.Title, Note: task.Note, UpsertSubagents: subagents, State: state}
	if err := boardTaskRefsFromWire(task, &row); err != nil {
		return store.BoardTaskPatch{}, err
	}
	if task.GetWaiting() != nil {
		encoded, err := marshalBoardWaiting(task.GetWaiting().GetValues())
		if err != nil {
			if _, ok := errors.AsType[*connect.Error](err); ok {
				return store.BoardTaskPatch{}, err
			}
			return store.BoardTaskPatch{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("encode board waiting entries: %w", err))
		}
		row.Waiting = &encoded
	}
	return row, nil
}

func validateBoardTaskPatchWire(task *compassv1.BoardTaskPatch) error {
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{"board task key", task.GetKey(), store.MaxBoardTaskKeyChars},
		{"board task title", task.GetTitle(), store.MaxBoardTitleChars},
		{"board task note", task.GetNote(), store.MaxBoardNoteChars},
	} {
		if err := validateBoardWireString(field.name, field.value, field.limit); err != nil {
			return err
		}
	}
	if task.GetRefs() != nil && len(task.GetRefs().GetValues()) > store.MaxBoardRefs {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("board task %q ref count exceeds maximum of %d", task.GetKey(), store.MaxBoardRefs))
	}
	if task.GetWaiting() != nil && len(task.GetWaiting().GetValues()) > store.MaxBoardWaiting {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("board task %q waiting entry count exceeds maximum of %d", task.GetKey(), store.MaxBoardWaiting))
	}
	if len(task.GetUpsertSubagents()) > store.MaxBoardSubagents {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("board task %q subagent count exceeds maximum of %d", task.GetKey(), store.MaxBoardSubagents))
	}
	return validateBoardSubagentsWire(task.GetUpsertSubagents())
}

func validateBoardSubagentsWire(subagents []*compassv1.BoardSubagent) error {
	for _, subagent := range subagents {
		if subagent == nil {
			continue
		}
		for _, field := range []struct {
			name  string
			value string
			limit int
		}{
			{"subagent id", subagent.GetId(), store.MaxBoardSubagentIDChars},
			{"subagent agent", subagent.GetAgent(), store.MaxBoardSubagentAgentChars},
			{"subagent description", subagent.GetDescription(), store.MaxBoardSubagentDescriptionChars},
			{"subagent status", subagent.GetStatus(), store.MaxBoardSubagentStatusChars},
		} {
			if err := validateBoardWireString(field.name, field.value, field.limit); err != nil {
				return err
			}
		}
	}
	return nil
}

func boardSubagentsFromWire(subagents []*compassv1.BoardSubagent) ([]byte, error) {
	if len(subagents) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(subagents)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("encode board subagents: %w", err))
	}
	return encoded, nil
}

func boardTaskRefsFromWire(task *compassv1.BoardTaskPatch, row *store.BoardTaskPatch) error {
	if task.PrimaryRef != nil {
		if strings.ContainsRune(task.GetPrimaryRef(), '\x00') {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("board primary ref contains a NUL character"))
		}
		value, err := canonicalOptionalRef(task.GetPrimaryRef())
		if err != nil {
			return err
		}
		row.PrimaryRef = &value
	}
	if task.GetRefs() == nil {
		return nil
	}
	values := make([]string, 0, len(task.GetRefs().GetValues()))
	for _, value := range task.GetRefs().GetValues() {
		if strings.ContainsRune(value, '\x00') {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("board task ref contains a NUL character"))
		}
		canonical, err := canonicalRef(value)
		if err != nil {
			return err
		}
		values = append(values, canonical)
	}
	row.Refs = &values
	return nil
}

func boardTaskStateToStore(state compassv1.BoardTaskState) (int16, bool) {
	switch state {
	case compassv1.BoardTaskState_BOARD_TASK_STATE_ACTIVE:
		return 1, true
	case compassv1.BoardTaskState_BOARD_TASK_STATE_BLOCKED:
		return 2, true
	case compassv1.BoardTaskState_BOARD_TASK_STATE_WAITING:
		return 3, true
	case compassv1.BoardTaskState_BOARD_TASK_STATE_PARKED:
		return 4, true
	case compassv1.BoardTaskState_BOARD_TASK_STATE_DONE:
		return 5, true
	default:
		return 0, false
	}
}

func boardWaitingFilter(value compassv1.BoardWaitingOn) int16 {
	switch value {
	case compassv1.BoardWaitingOn_BOARD_WAITING_ON_UNSPECIFIED:
		return 0
	case compassv1.BoardWaitingOn_BOARD_WAITING_ON_OPERATOR:
		return 1
	case compassv1.BoardWaitingOn_BOARD_WAITING_ON_AGENT:
		return 2
	case compassv1.BoardWaitingOn_BOARD_WAITING_ON_EXTERNAL:
		return 3
	default:
		return -1
	}
}

func boardStateFilter(value compassv1.BoardTaskState) int16 {
	if value == compassv1.BoardTaskState_BOARD_TASK_STATE_UNSPECIFIED {
		return 0
	}
	state, valid := boardTaskStateToStore(value)
	if !valid {
		return -1
	}
	return state
}

func boardHaltFilter(value compassv1.BoardHaltKind) int16 {
	switch value {
	case compassv1.BoardHaltKind_BOARD_HALT_KIND_UNSPECIFIED:
		return 0
	case compassv1.BoardHaltKind_BOARD_HALT_KIND_DONE:
		return 1
	case compassv1.BoardHaltKind_BOARD_HALT_KIND_BLOCKED:
		return 2
	case compassv1.BoardHaltKind_BOARD_HALT_KIND_AWAITING_USER:
		return 3
	default:
		return -1
	}
}

func canonicalOptionalRef(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	return canonicalRef(value)
}

func canonicalRef(value string) (string, error) {
	ref, err := refs.Parse(value)
	if err != nil {
		return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid board ref %q: %w", value, err))
	}
	return ref.String(), nil
}

// validateBoardFilterText rejects NUL before filter text reaches Postgres.
func validateBoardFilterText(req *compassv1.ListAgentBoardsRequest) error {
	if strings.ContainsRune(req.GetQuery(), '\x00') || strings.ContainsRune(req.GetRef(), '\x00') {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("board filter contains a NUL character"))
	}
	for _, handle := range req.GetAgentHandles() {
		if strings.ContainsRune(handle, '\x00') {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("board filter contains a NUL character"))
		}
	}
	return nil
}

func validateBoardWireString(field, value string, maxChars int) error {
	if strings.ContainsRune(value, '\x00') {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s contains a NUL character", field))
	}
	if utf8.RuneCountInString(value) > maxChars {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s exceeds maximum of %d characters", field, maxChars))
	}
	return nil
}

func marshalBoardWaiting(values []*compassv1.BoardWaiting) ([]byte, error) {
	if len(values) > store.MaxBoardWaiting {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("board task waiting entry count exceeds maximum of %d", store.MaxBoardWaiting))
	}
	type waitingEntry struct {
		On          string   `json:"on"`
		AgentHandle string   `json:"agent_handle,omitempty"`
		What        string   `json:"what,omitempty"`
		Ref         string   `json:"ref,omitempty"`
		Blocks      []string `json:"blocks,omitempty"`
	}
	encoded := make([]waitingEntry, len(values))
	for i, value := range values {
		if value == nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("board waiting entry is required"))
		}
		if len(value.GetBlocks()) > store.MaxBoardRefs {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("board waiting block count exceeds maximum of %d", store.MaxBoardRefs))
		}
		if value.GetOn() < compassv1.BoardWaitingOn_BOARD_WAITING_ON_OPERATOR || value.GetOn() > compassv1.BoardWaitingOn_BOARD_WAITING_ON_EXTERNAL {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("board waiting entry has invalid on value"))
		}
		if err := validateBoardWireString("waiting agent_handle", value.GetAgentHandle(), store.MaxBoardHandleChars); err != nil {
			return nil, err
		}
		if err := validateBoardWireString("waiting what", value.GetWhat(), store.MaxBoardWhatChars); err != nil {
			return nil, err
		}
		ref, err := canonicalOptionalRef(value.GetRef())
		if err != nil {
			return nil, err
		}
		blocks := make([]string, len(value.GetBlocks()))
		for i, blockRef := range value.GetBlocks() {
			blocks[i], err = canonicalRef(blockRef)
			if err != nil {
				return nil, err
			}
		}
		encoded[i] = waitingEntry{On: value.GetOn().String(), AgentHandle: value.GetAgentHandle(), What: value.GetWhat(), Ref: ref, Blocks: blocks}
	}
	return json.Marshal(encoded)
}

func unmarshalBoardWaiting(raw []byte) ([]*compassv1.BoardWaiting, error) {
	var values []struct {
		On          string   `json:"on"`
		AgentHandle string   `json:"agent_handle"`
		What        string   `json:"what"`
		Ref         string   `json:"ref"`
		Blocks      []string `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	out := make([]*compassv1.BoardWaiting, len(values))
	for i, value := range values {
		on, ok := compassv1.BoardWaitingOn_value[value.On]
		if !ok {
			return nil, fmt.Errorf("unknown waiting on value %q", value.On)
		}
		out[i] = &compassv1.BoardWaiting{
			On: compassv1.BoardWaitingOn(on), AgentHandle: value.AgentHandle,
			What: value.What, Ref: value.Ref, Blocks: value.Blocks,
		}
	}
	return out, nil
}

func emptyBoardPatch(patch store.BoardPatch) bool {
	return len(patch.Upsert) == 0 && len(patch.Remove) == 0 && patch.Focus == nil && patch.Halt == nil && !patch.ClearHalt
}

// ListBoardsAsAccount returns matching boards from the caller's roster clip.
func (c *Comms) ListBoardsAsAccount(
	ctx context.Context,
	caller store.AccountID,
	req *compassv1.ListAgentBoardsRequest,
) (*compassv1.ListAgentBoardsResponse, error) {
	if caller == "" {
		return nil, errNoActor
	}
	if req == nil {
		req = &compassv1.ListAgentBoardsRequest{}
	}
	if len(req.GetAgentHandles()) > store.MaxBoardSearchAgents {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("board agent handle count exceeds maximum of %d", store.MaxBoardSearchAgents))
	}
	if err := validateBoardFilterText(req); err != nil {
		return nil, err
	}
	if req.GetState() != compassv1.BoardTaskState_BOARD_TASK_STATE_UNSPECIFIED && boardStateFilter(req.GetState()) < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid board state filter %d", req.GetState()))
	}
	if req.GetWaitingOn() != compassv1.BoardWaitingOn_BOARD_WAITING_ON_UNSPECIFIED && boardWaitingFilter(req.GetWaitingOn()) < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid board waiting_on filter %d", req.GetWaitingOn()))
	}
	if req.GetHalt() != compassv1.BoardHaltKind_BOARD_HALT_KIND_UNSPECIFIED && boardHaltFilter(req.GetHalt()) < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid board halt filter %d", req.GetHalt()))
	}
	handleSet := make(map[string]struct{}, min(len(req.GetAgentHandles()), store.MaxBoardSearchAgents))
	handles := make([]string, 0, min(len(req.GetAgentHandles()), store.MaxBoardSearchAgents))
	for _, handle := range req.GetAgentHandles() {
		if _, found := handleSet[handle]; found {
			continue
		}
		handleSet[handle] = struct{}{}
		handles = append(handles, handle)
	}
	matchedAgents := make(map[store.AccountID]struct{}, len(handles))
	for _, handle := range handles {
		resolved, err := c.resolveAgentAccount(ctx, caller, handle)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, edgeError(err)
		}
		matchedAgents[resolved.ID] = struct{}{}
	}
	limit := boardLimit(req.GetLimit())
	visible, err := c.store.ListAccounts(ctx, caller)
	if err != nil {
		return nil, edgeError(err)
	}
	accounts := make(map[store.AccountID]store.Account, len(visible))
	ids := make([]store.AccountID, 0, len(visible))
	for _, account := range visible {
		_, matched := matchedAgents[account.ID]
		if account.IsAgent() && (len(handles) == 0 || matched) {
			accounts[account.ID] = account
			ids = append(ids, account.ID)
		}
	}
	ref := req.GetRef()
	if ref != "" {
		ref, err = canonicalRef(ref)
		if err != nil {
			return nil, err
		}
	}
	if len(ids) == 0 {
		return &compassv1.ListAgentBoardsResponse{}, nil
	}
	matches, rows, err := c.store.SearchAndReadAgentBoards(ctx, store.BoardTaskFilter{
		Agents: ids, Query: req.GetQuery(), Ref: ref,
		WaitingOn: boardWaitingFilter(req.GetWaitingOn()), State: boardStateFilter(req.GetState()),
		Halt: boardHaltFilter(req.GetHalt()), Limit: limit,
	})
	if err != nil {
		return nil, edgeError(err)
	}
	matchingIDs := make([]store.AccountID, 0, len(matches))
	for id := range matches {
		matchingIDs = append(matchingIDs, id)
	}
	slices.Sort(matchingIDs)
	boards := make([]*compassv1.AgentBoard, 0, len(matches))

	for _, id := range matchingIDs {
		account, ok := accounts[id]
		if !ok {
			continue
		}
		authored := boardAuthoredToWire(account, rows[id], matches[id])
		if authored.decodeErr != nil {
			slog.WarnContext(ctx, "comms: board decode failed while listing", "agent_account_id", string(id), "error", authored.decodeErr)
		}
		wire := authored.wire
		completed, err := c.completeBoardWire(ctx, caller, account, rows[id], wire, authored.refs)
		if err != nil {
			slog.WarnContext(ctx, "comms: board refs unavailable while listing", "agent_account_id", string(id), "error", err)
		} else {
			wire = completed
		}
		boards = append(boards, wire)
	}
	c.fillAskMirrors(ctx, boards)
	return &compassv1.ListAgentBoardsResponse{Boards: boards}, nil
}

func boardLimit(limit uint32) int {
	if limit == 0 {
		return 20
	}
	if limit > 100 {
		return 100
	}
	return int(limit)
}

// ListAgentBoards adapts the RPC request to the account-scoped board read.
func (c *Comms) ListAgentBoards(
	ctx context.Context,
	req *connect.Request[compassv1.ListAgentBoardsRequest],
) (*connect.Response[compassv1.ListAgentBoardsResponse], error) {
	if req == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("board list request is required"))
	}
	response, err := c.ListBoardsAsAccount(ctx, c.actorFromContext(ctx), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(response), nil
}

// boardVisible uses the same account clip as RosterAsAccount.
func (c *Comms) boardVisible(ctx context.Context, viewer, agent store.AccountID) (bool, error) {
	visible, err := c.store.AccountVisibleTo(ctx, viewer, agent)
	if err != nil {
		return false, err
	}
	if !visible {
		return false, nil
	}
	return c.store.IsAgentAccount(ctx, agent)
}

func authoredBoardIdentity(account store.Account, row store.BoardRow, matched []string) *compassv1.AgentBoard {
	return &compassv1.AgentBoard{
		AgentAccountId: string(account.ID), Handle: account.Handle, FocusTaskKey: row.Focus,
		Halt: haltToWire(row.Halt), WrittenTurn: row.WrittenTurn, WrittenAtUnixMs: row.WrittenAtUnixMs,
		MatchedTaskKeys: matched, Tasks: []*compassv1.BoardTask{},
	}
}

// authoredBoard keeps a decode failure beside a usable board: callers serve the
// authored data even when some stored task fields fail to decode.
type authoredBoard struct {
	wire      *compassv1.AgentBoard
	refs      []string
	decodeErr error
}

func boardAuthoredToWire(account store.Account, row store.BoardRow, matched []string) authoredBoard {
	out := authoredBoardIdentity(account, row, matched)
	allRefs := make([]string, 0, len(row.Tasks)*2)
	var decodeErr error
	for _, task := range row.Tasks {
		wireTask := &compassv1.BoardTask{
			Key: task.Key, Title: task.Title, PrimaryRef: task.PrimaryRef, Refs: task.Refs,
			State: compassv1.BoardTaskState(task.State), Note: task.Note, WrittenAtUnixMs: task.WrittenAtUnixMs,
		}
		out.Tasks = append(out.Tasks, wireTask)
		waiting, err := unmarshalBoardWaiting(task.Waiting)
		if err != nil {
			decodeErr = errors.Join(decodeErr, fmt.Errorf("decode stored waiting entries for %q: %w", task.Key, err))
		} else {
			wireTask.Waiting = waiting
		}
		var subagents []*compassv1.BoardSubagent
		if err := json.Unmarshal(task.Subagents, &subagents); err != nil {
			decodeErr = errors.Join(decodeErr, fmt.Errorf("decode stored subagents for %q: %w", task.Key, err))
		} else {
			wireTask.Subagents = subagents
		}
		allRefs = append(allRefs, task.Refs...)
		if task.PrimaryRef != "" {
			allRefs = append(allRefs, task.PrimaryRef)
		}
	}
	return authoredBoard{wire: out, refs: allRefs, decodeErr: decodeErr}
}

func (c *Comms) completeBoardWire(ctx context.Context, viewer store.AccountID, account store.Account, row store.BoardRow, out *compassv1.AgentBoard, allRefs []string) (*compassv1.AgentBoard, error) {
	presence := compassv1.AgentPresence_AGENT_PRESENCE_OFFLINE
	if c.presence != nil {
		if live := c.presence.PresenceFor([]store.AccountID{account.ID}); live[account.ID] != compassv1.AgentPresence_AGENT_PRESENCE_UNSPECIFIED {
			presence = live[account.ID]
		}
	}
	out.Stale = presence == compassv1.AgentPresence_AGENT_PRESENCE_WORKING && row.WrittenAtUnixMs < time.Now().Add(-boardStaleAfter).UnixMilli()
	asks, err := c.boardAsks(ctx, viewer, account.ID)
	if err != nil {
		slog.WarnContext(ctx, "comms: board asks unavailable", "agent_account_id", string(account.ID), "error", err)
	} else {
		out.Asks = asks
	}
	derived, untracked, err := board.DeriveRefs(ctx, c.store, c.boardForges, account.ID, allRefs)
	if err != nil {
		return out, err
	}
	out.Refs, out.UntrackedRefs = derived, untracked
	return out, nil
}

func (c *Comms) boardAsks(ctx context.Context, viewer, agent store.AccountID) ([]*compassv1.BoardAsk, error) {
	rows, err := c.store.AsksByAuthor(ctx, viewer, agent, time.Now().Add(-boardAskWindow), 0)
	if err != nil {
		return nil, edgeError(err)
	}
	out := make([]*compassv1.BoardAsk, len(rows))
	for i, r := range rows {
		out[i] = &compassv1.BoardAsk{
			AskId: r.AskID, MessageId: r.MessageID, ChannelId: r.ChannelID,
			Ref: r.Ref, Header: r.Header, Answered: r.Answered, PostedAtUnixMs: r.PostedAtUnixMs,
		}
	}
	return out, nil
}

// fillAskMirrors sets each ask's forge mirror state with one lookup for all
// boards; a failed lookup leaves the asks without mirror state.
func (c *Comms) fillAskMirrors(ctx context.Context, boards []*compassv1.AgentBoard) {
	var askIDs []string
	for _, b := range boards {
		for _, ask := range b.GetAsks() {
			askIDs = append(askIDs, ask.GetAskId())
		}
	}
	if len(askIDs) == 0 {
		return
	}
	mirrors, err := c.store.AskForgeMirrors(ctx, askIDs)
	if err != nil {
		slog.WarnContext(ctx, "comms: board ask mirrors unavailable", "error", err)
		return
	}
	byAsk := make(map[string]store.ForgeMirrorRow, len(askIDs))
	for _, mirror := range mirrors {
		current, found := byAsk[mirror.AskID]
		// The agent's automatic row wins over a user's click row, then the newest row.
		auto, curAuto := mirror.Agent != "", current.Agent != ""
		if !found || (auto && !curAuto) || (auto == curAuto && mirror.UpdatedAt.After(current.UpdatedAt)) {
			byAsk[mirror.AskID] = mirror
		}
	}
	for _, b := range boards {
		for _, ask := range b.GetAsks() {
			mirror, ok := byAsk[ask.GetAskId()]
			if !ok {
				continue
			}
			ask.MirrorState = compassv1.BoardMirrorState(mirror.State)
			if ask.GetMirrorState() == compassv1.BoardMirrorState_BOARD_MIRROR_STATE_DONE {
				ask.MirrorUrl = mirror.CommentURL
			}
		}
	}
}

func haltToWire(halt store.BoardHalt) *compassv1.BoardHalt {
	if halt.Kind == 0 && halt.Ref == "" && halt.Reason == "" {
		return nil
	}
	return &compassv1.BoardHalt{Kind: compassv1.BoardHaltKind(halt.Kind), Ref: halt.Ref, Reason: halt.Reason}
}

func haltFromWire(halt *compassv1.BoardHalt) (store.BoardHalt, error) {
	kind := halt.GetKind()
	if kind <= compassv1.BoardHaltKind_BOARD_HALT_KIND_UNSPECIFIED || kind > compassv1.BoardHaltKind_BOARD_HALT_KIND_AWAITING_USER {
		return store.BoardHalt{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid board halt kind %d", kind))
	}
	if err := validateBoardWireString("board halt reason", halt.GetReason(), store.MaxBoardHaltReasonChars); err != nil {
		return store.BoardHalt{}, err
	}
	ref := halt.GetRef()
	if strings.ContainsRune(ref, '\x00') {
		return store.BoardHalt{}, connect.NewError(connect.CodeInvalidArgument, errors.New("board halt ref contains a NUL character"))
	}
	if ref != "" {
		canonical, err := canonicalRef(ref)
		if err != nil {
			return store.BoardHalt{}, err
		}
		ref = canonical
	}
	return store.BoardHalt{Kind: int16(kind), Ref: ref, Reason: halt.GetReason()}, nil
}
