package comms

import (
	"context"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// AskAnswerWaker pushes an ask-answer wake to a waiting agent's live session over
// the SEA-1569 T3 control-dispatch rail. runnerhub implements it; comms depends
// only on this narrow, public-typed surface — the internal AgentControl /
// AskAnswerControl envelope stays in the rail layer, off the public-gen comms
// package (comms.go:18-27 imports only public gen). Nil-safe: a Comms with no
// waker wired does not wake (today's behavior) — a comms handler with no hub (a
// unit test) still answers asks.
type AskAnswerWaker interface {
	// WakeAskAnswer dispatches an ask_answer control op to agent's live session
	// carrying answers, correlated by askID. A no-op (no error) when the agent
	// has no live session (nothing to wake now; the agent reads the answer on
	// its next turn/reconnect via the normal delivery path). Best-effort: it
	// must never fail the RPC, so it returns nothing — a dispatch fault is
	// logged in the rail layer, not surfaced.
	WakeAskAnswer(ctx context.Context, agent store.AccountID, askID string, answers []*compassv1.AskQuestionAnswer)
}
