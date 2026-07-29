//go:build unix

package runnerhub

// The CONTRACT-DEFECT classification (SEA-1364 T3 review): the frame class that
// is not one bad frame but a broken relay. Deliver already had two buckets — a
// per-frame refusal (drop, count, carry on) and an infrastructure fault (tear the
// stream down so the Runner retries). Neither fits a frame the agent and the
// Server disagree about the SHAPE of, and collapsing the two is what let a 100%
// frame-loss wiring skew look identical to a healthy relay refusing the odd
// cross-account edit.
//
// A contract defect is therefore its own bucket with its own counter: still
// non-fatal (every frame carries the same defect, so tearing the stream down
// would just kill the relay outright), but logged as "the relay is misconfigured
// and nothing is committing" rather than "one frame refused".
//
// The tests below pin BOTH halves of the distinction — that a defect increments
// contractDefects and NOT refusedFrames, and that an ordinary refusal still
// increments refusedFrames and NOT contractDefects. Without the negative half a
// bug that counted every drop in both buckets would pass.

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
)

// THE gap test. This drives the frame the production agent ACTUALLY emits today
// and asserts what genuinely happens: nothing commits.
//
// The first-party agent's only conversation-frame emitter is
// EventMapper.#appendBlock (packages/compass-agent/src/mapping.ts:386-393). It
// builds a Message from `blocks` alone and never stamps an id — the comment there
// states outright that the agent has no server id to mint — and nothing between
// the agent and this hub stamps one either (relay.go forwards the frame verbatim;
// the Runner's PostConversationFrame is unimplemented). No production path emits
// conversationPosted at all. So EVERY real agent turn arrives here as an id-less
// conversation_updated, and an update with no id addresses no row.
//
// This test therefore asserts the DEFECT, deliberately: zero sink calls, the
// contract-defect counter at 1, the refusal counter still 0, and the stream alive.
// It is the pin that keeps the gap a known, asserted state instead of a latent
// surprise — the write-through committing 0% of agent turns is currently
// indistinguishable, from the outside, from it committing 100%.
//
// WHEN RUNNER-SIDE ID RECONCILIATION LANDS (compass-runner lane, not this one),
// FLIP THIS TEST: the Runner will stamp the server id it learned from the first
// commit onto each subsequent update, the frame will address a real row, and the
// assertions below become "the sink saw one call and a row committed". Until
// then, a green run of this test means the gap is still open and still known.
func TestDeliverIDLessConversationUpdateIsAContractDefectAndCommitsNothing(t *testing.T) {
	hub, conv, life, tail := newHub()
	bindSession(hub, "sess-1")

	// Exactly the shape #appendBlock produces: blocks, no id.
	err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1,
		SessionID: "sess-1",
		Frame:     convUpdatedFrameIDLess("the agent speaks, and it is lost"),
	})
	if err != nil {
		t.Fatalf("Deliver(id-less update) = %v, want nil — a contract defect must not tear the stream down (every frame carries it, so a teardown would kill the relay outright)", err)
	}

	// Nothing commits. The sink is never even reached: the hub can see the
	// defect in the frame itself, so it refuses before attempting a write.
	if got := len(conv.snapshot()); got != 0 {
		t.Fatalf("conversation sink saw %d calls for an id-less update, want 0 — an update with no id addresses no row, so there is nothing to attempt", got)
	}
	if got := len(life.snapshot()) + len(tail.snapshot()); got != 0 {
		t.Fatalf("an id-less conversation frame reached %d other sink calls, want 0", got)
	}

	// It lands in the CONTRACT-DEFECT bucket...
	if got := hub.ContractDefects(); got != 1 {
		t.Fatalf("ContractDefects = %d, want 1 — an id-less relayed update is a wiring/contract skew between agent and Server, not routine per-frame garbage", got)
	}
	// ...and NOT the ordinary-refusal bucket. This is the load-bearing negative:
	// if the defect were counted as a refusal, total frame loss would read as a
	// healthy relay refusing frames and the operator would have no signal.
	if got := hub.RefusedFrames(); got != 0 {
		t.Fatalf("RefusedFrames = %d, want 0 — a contract defect must NOT be indistinguishable from an authz refusal", got)
	}

	// The stream survives: the same hub carries the next frame.
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 2,
		SessionID: "sess-1",
		Frame:     convPostedFrame("the next frame still flows"),
	}); err != nil {
		t.Fatalf("Deliver(following frame) = %v, want nil — the stream must survive a contract defect", err)
	}
	if got := len(conv.snapshot()); got != 1 {
		t.Fatalf("conversation sink saw %d calls after the following frame, want 1", got)
	}
	if got := hub.ContractDefects(); got != 1 {
		t.Fatalf("ContractDefects = %d after a good frame, want still 1", got)
	}
}

// Every real agent turn carries the same defect, so the counter must ACCUMULATE
// rather than latch — a rising count is the only way an operator distinguishes
// "the relay is dead" from "one frame was malformed once". A latching bool or a
// once-only log would make sustained total loss look like a single hiccup.
func TestDeliverIDLessConversationUpdatesAccumulateTheDefectCount(t *testing.T) {
	hub, conv, _, _ := newHub()
	bindSession(hub, "sess-1")

	for seq := uint64(1); seq <= 3; seq++ {
		if err := hub.Deliver(context.Background(), RunnerEvent{
			RunnerSeq: seq,
			SessionID: "sess-1",
			Frame:     convUpdatedFrameIDLess("streaming turn"),
		}); err != nil {
			t.Fatalf("Deliver(id-less update, seq %d) = %v, want nil", seq, err)
		}
	}

	if got := hub.ContractDefects(); got != 3 {
		t.Fatalf("ContractDefects = %d after three id-less updates, want 3 — the count must accumulate so sustained total loss is visible", got)
	}
	if got := len(conv.snapshot()); got != 0 {
		t.Fatalf("conversation sink saw %d calls, want 0", got)
	}
	if got := hub.RefusedFrames(); got != 0 {
		t.Fatalf("RefusedFrames = %d, want 0", got)
	}
}

// The other half of the distinction, from the error side: a sink error carrying
// CodeFailedPrecondition is a contract defect too (the comms layer's vocabulary
// for a structurally impossible request — e.g. a session bound to a NON-AGENT
// account, comms/agent_caller.go homeChannel), while NotFound/InvalidArgument
// stay ordinary refusals. Without this the hub would have only the frame-shape
// check and a bad binding would read as routine garbage.
func TestDeliverClassifiesSinkErrorsIntoRefusalVersusContractDefect(t *testing.T) {
	for _, tc := range []struct {
		name            string
		err             error
		wantRefused     uint64
		wantDefects     uint64
		wantStreamAlive bool
	}{
		{
			name:            "cross-account update is an ordinary refusal",
			err:             connect.NewError(connect.CodeNotFound, errors.New("message not found")),
			wantRefused:     1,
			wantDefects:     0,
			wantStreamAlive: true,
		},
		{
			name:            "malformed block set is an ordinary refusal",
			err:             connect.NewError(connect.CodeInvalidArgument, errors.New("ask has no ask_id")),
			wantRefused:     1,
			wantDefects:     0,
			wantStreamAlive: true,
		},
		{
			name:            "non-agent binding is a contract defect",
			err:             connect.NewError(connect.CodeFailedPrecondition, errors.New("comms: account is not an agent account")),
			wantRefused:     0,
			wantDefects:     1,
			wantStreamAlive: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, conv, _, _ := newHub()
			bindSession(hub, "sess-1")
			conv.err = tc.err

			err := hub.Deliver(context.Background(), RunnerEvent{
				RunnerSeq: 1,
				SessionID: "sess-1",
				Frame:     convPostedFrame("classify me"),
			})
			if tc.wantStreamAlive && err != nil {
				t.Fatalf("Deliver = %v, want nil (both classes are non-fatal drops)", err)
			}
			if got := hub.RefusedFrames(); got != tc.wantRefused {
				t.Fatalf("RefusedFrames = %d, want %d", got, tc.wantRefused)
			}
			if got := hub.ContractDefects(); got != tc.wantDefects {
				t.Fatalf("ContractDefects = %d, want %d", got, tc.wantDefects)
			}
		})
	}
}

// THE fail-safe default, pinned. isFrameRefusal keys on the Connect code, so the
// drop-vs-teardown decision silently depends on every ConversationSink routing
// its errors through edgeError. A sink that returns a BARE error (an
// errors.New that never met the mapping) must therefore tear the stream down —
// connect.CodeOf reports CodeUnknown for it, which is neither a refusal nor a
// defect, so it falls through to the fault path.
//
// That is the right default (an unclassified error is not evidence the frame was
// bad), but it was only implicit in connect.CodeOf's behavior. This test makes
// it a contract: an unmapped sink error is FATAL and counts as neither a refusal
// nor a defect, so a future sink that forgets edgeError fails loudly at the
// stream rather than quietly shredding frames into a counter.
func TestDeliverBareSinkErrorTearsDownTheStreamAndCountsNothing(t *testing.T) {
	hub, conv, _, _ := newHub()
	bindSession(hub, "sess-1")
	conv.err = errors.New("boom")

	err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1,
		SessionID: "sess-1",
		Frame:     convPostedFrame("unclassified failure"),
	})
	if err == nil {
		t.Fatal("Deliver(bare sink error) = nil, want an error — an error that never met edgeError is unclassified, and an unclassified error must not be assumed to be the frame's fault")
	}
	if got := hub.RefusedFrames(); got != 0 {
		t.Fatalf("RefusedFrames = %d, want 0 — an unmapped error is not a per-frame refusal", got)
	}
	if got := hub.ContractDefects(); got != 0 {
		t.Fatalf("ContractDefects = %d, want 0 — an unmapped error is not a contract defect either", got)
	}
}

// The diagnostic surface: the three frame-loss counters plus the gap flag are
// readable as one snapshot, and the hub emits that snapshot on demand. Before
// this, total frame loss was visible ONLY in per-frame warn lines — there was no
// way to ask the hub "how much have you dropped", which is exactly the question
// a wedged relay raises.
func TestFrameDiagnosticsSnapshotReportsEveryLossCounter(t *testing.T) {
	hub, conv, _, _ := newHub()
	bindSession(hub, "sess-1")

	// One of each loss class: an unknown variant, an ordinary refusal, a
	// contract defect, and a sequence gap.
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1, SessionID: "sess-1", Frame: &compassv1internal.AgentFrame{},
	}); err != nil {
		t.Fatalf("Deliver(unknown frame) = %v, want nil", err)
	}
	conv.err = connect.NewError(connect.CodeNotFound, errors.New("message not found"))
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 2, SessionID: "sess-1", Frame: convPostedFrame("refused"),
	}); err != nil {
		t.Fatalf("Deliver(refused frame) = %v, want nil", err)
	}
	conv.err = nil
	// Seq 4 skips 3: an in-transit gap.
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 4, SessionID: "sess-1", Frame: convUpdatedFrameIDLess("id-less"),
	}); err != nil {
		t.Fatalf("Deliver(id-less update) = %v, want nil", err)
	}

	got := hub.FrameDiagnostics()
	want := FrameDiagnostics{SeenGap: true, UnknownFrames: 1, RefusedFrames: 1, ContractDefects: 1}
	if got != want {
		t.Fatalf("FrameDiagnostics() = %+v, want %+v", got, want)
	}
	// The snapshot agrees with the individual accessors — they read the same
	// state under the same lock, so a drift here means one of them is stale.
	if got.UnknownFrames != hub.UnknownFrames() ||
		got.RefusedFrames != hub.RefusedFrames() ||
		got.ContractDefects != hub.ContractDefects() ||
		got.SeenGap != hub.SeenGap() {
		t.Fatalf("FrameDiagnostics() %+v disagrees with the individual accessors", got)
	}
}
