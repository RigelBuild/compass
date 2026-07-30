//go:build unix

package server

// The conversation sink's error vocabulary. commsConversationSink is the one
// production ConversationSink, and the RunnerHub decides a relayed frame's fate
// purely from the CONNECT CODE this type returns (runnerhub/hub.go,
// ConversationSink's error-vocabulary contract): NotFound/InvalidArgument drop
// the frame and count it, FailedPrecondition drops it as a contract defect,
// anything else tears the Runner's PublishEvents stream down.
//
// So a code chosen here is not cosmetic — it is the difference between a frame
// quietly discarded and an operator finding out. This pins the one code this
// file mints itself. The other half of the linkage (that CodeInternal is what
// the hub tears down on, and that a bare unmapped error does too) is pinned on
// the hub side, where the predicates live:
// runnerhub.TestDeliverConversationStoreFailureTearsDownTheStream and
// TestDeliverBareSinkErrorTearsDownTheStreamAndCountsNothing.

import (
	"context"
	"testing"

	"connectrpc.com/connect"
)

// The no-variant guard tears the stream DOWN rather than being dropped as a
// refusal. Its own docstring says it is unreachable through Deliver's dispatch
// (Deliver switches on the oneof and passes exactly one variant), so if it ever
// fires, a Server-side dispatch defect has been introduced — the frame is fine
// and the Server is broken.
//
// CodeInvalidArgument, which this returned before, put it in the routine
// droppable bucket: the Server would have silently discarded frames on its own
// wiring bug, at the one moment loudness is worth the teardown. It must not be
// CodeFailedPrecondition either — that is the non-fatal contract-defect bucket,
// for a skew the relay keeps serving through, and an unreachable Server branch
// firing is not something to keep serving through.
//
// Mutation: revert to CodeInvalidArgument (or FailedPrecondition) → the frame
// becomes a silent drop on a Server bug, and this reddens.
func TestNoConversationVariantIsAnInternalDefectNotADroppableRefusal(t *testing.T) {
	// The sink is called with neither variant: the shape only a dispatch bug
	// produces. A nil comms handler is safe precisely because this branch must
	// return before touching it — reaching comms here would panic, which is
	// itself the assertion that the guard fires first rather than dereferencing.
	sink := commsConversationSink{comms: nil}

	err := sink.PostAgentMessage(context.Background(), "acct-agent", "sess-1", "", nil, nil)
	if err == nil {
		t.Fatal("PostAgentMessage(no variant) = nil, want an error — a silent success would ack a frame that committed nothing")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("PostAgentMessage(no variant) code = %v, want %v — an unreachable branch firing is a Server-side wiring defect, and it must tear the stream down rather than hide among expected per-frame refusals", got, connect.CodeInternal)
	}
}
