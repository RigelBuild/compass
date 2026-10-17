package comms

import (
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

func TestBoardWireLimitsRejectOversizedCollections(t *testing.T) {
	waitingOn := compassv1.BoardWaitingOn_BOARD_WAITING_ON_OPERATOR
	tooManyRefs := make([]string, 11)
	tooManyBlocks := make([]string, 11)
	tooManyWaiting := make([]*compassv1.BoardWaiting, 6)
	for i := range tooManyRefs {
		tooManyRefs[i] = "not-a-ref"
		tooManyBlocks[i] = "not-a-ref"
	}
	for i := range tooManyWaiting {
		tooManyWaiting[i] = &compassv1.BoardWaiting{On: waitingOn}
	}
	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "upsert tasks",
			call: func() error {
				_, err := boardPatchFromWire(&compassv1.UpdateAgentBoardRequest{UpsertTasks: make([]*compassv1.BoardTaskPatch, 31)})
				return err
			},
		},
		{
			name: "remove task keys",
			call: func() error {
				_, err := boardPatchFromWire(&compassv1.UpdateAgentBoardRequest{RemoveTaskKeys: make([]string, 31)})
				return err
			},
		},
		{
			name: "task refs",
			call: func() error {
				_, err := boardTaskPatchFromWire(&compassv1.BoardTaskPatch{Refs: &compassv1.BoardRefsOp{Values: tooManyRefs}})
				return err
			},
		},
		{
			name: "waiting entries",
			call: func() error {
				_, err := marshalBoardWaiting(tooManyWaiting)
				return err
			},
		},
		{
			name: "waiting blocks",
			call: func() error {
				_, err := marshalBoardWaiting([]*compassv1.BoardWaiting{{On: waitingOn, Blocks: tooManyBlocks}})
				return err
			},
		},
		{
			name: "subagents",
			call: func() error {
				_, err := boardTaskPatchFromWire(&compassv1.BoardTaskPatch{UpsertSubagents: make([]*compassv1.BoardSubagent, 21)})
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "maximum") {
				t.Fatalf("oversized %s error = %v, want invalid_argument naming the maximum", tc.name, err)
			}
		})
	}
}

func TestBoardWireRejectsNULAndOverlongSubagentStatus(t *testing.T) {
	validOn := compassv1.BoardWaitingOn_BOARD_WAITING_ON_OPERATOR
	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "task title NUL",
			call: func() error {
				_, err := boardTaskPatchFromWire(&compassv1.BoardTaskPatch{Title: new("bad\x00text")})
				return err
			},
		},
		{
			name: "halt reason NUL",
			call: func() error {
				_, err := haltFromWire(&compassv1.BoardHalt{Reason: "bad\x00text"})
				return err
			},
		},
		{
			name: "waiting what NUL",
			call: func() error {
				_, err := marshalBoardWaiting([]*compassv1.BoardWaiting{{On: validOn, What: "bad\x00text"}})
				return err
			},
		},
		{
			name: "primary ref NUL",
			call: func() error {
				_, err := boardTaskPatchFromWire(&compassv1.BoardTaskPatch{PrimaryRef: new("bad\x00ref")})
				return err
			},
		},
		{
			name: "refs NUL",
			call: func() error {
				_, err := boardTaskPatchFromWire(&compassv1.BoardTaskPatch{Refs: &compassv1.BoardRefsOp{Values: []string{"bad\x00ref"}}})
				return err
			},
		},
		{
			name: "halt ref NUL",
			call: func() error {
				_, err := haltFromWire(&compassv1.BoardHalt{Kind: compassv1.BoardHaltKind_BOARD_HALT_KIND_BLOCKED, Ref: "bad\x00ref"})
				return err
			},
		},
		{
			name: "subagent id NUL",
			call: func() error {
				_, err := boardTaskPatchFromWire(&compassv1.BoardTaskPatch{UpsertSubagents: []*compassv1.BoardSubagent{{Id: "bad\x00id"}}})
				return err
			},
		},
		{
			name: "task key NUL",
			call: func() error {
				_, err := boardTaskPatchFromWire(&compassv1.BoardTaskPatch{Key: "bad\x00key"})
				return err
			},
		},
		{
			name: "subagent status NUL",
			call: func() error {
				_, err := boardTaskPatchFromWire(&compassv1.BoardTaskPatch{UpsertSubagents: []*compassv1.BoardSubagent{{Status: "bad\x00status"}}})
				return err
			},
		},
		{
			name: "subagent status length",
			call: func() error {
				_, err := boardTaskPatchFromWire(&compassv1.BoardTaskPatch{UpsertSubagents: []*compassv1.BoardSubagent{{Status: strings.Repeat("s", 65)}}})
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("error = %v, want invalid_argument", err)
			}
		})
	}
}

func TestMarshalBoardWaitingRejectsNilEntry(t *testing.T) {
	_, err := marshalBoardWaiting([]*compassv1.BoardWaiting{nil})
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "board waiting entry is required") {
		t.Fatalf("marshalBoardWaiting(nil): %v, want invalid_argument for required entry", err)
	}
	_, err = boardTaskPatchFromWire(&compassv1.BoardTaskPatch{
		Waiting: &compassv1.BoardWaitingOp{Values: []*compassv1.BoardWaiting{nil}},
	})
	if connect.CodeOf(err) != connect.CodeInvalidArgument || strings.Contains(err.Error(), "encode board waiting entries") {
		t.Fatalf("boardTaskPatchFromWire(nil waiting entry): %v, want original connect error", err)
	}
}
