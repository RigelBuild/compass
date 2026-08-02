//go:build unix

// The SEA-1577 ask-answer wake arm: another rider on the SEA-1569 T3 control
// dispatch rail, alongside deliver / steer / the T6 sweep. When a participant
// answers an agent's Ask (comms.RespondToAsk), the hub resolves the asking
// agent's live session and pushes an AgentControl.ask_answer op carrying the
// answers, correlated by ask_id. It satisfies comms.AskAnswerWaker structurally
// (the compile-time assertion lives in the server package, which imports both);
// runnerhub never imports comms — comms defines the interface it needs, exactly
// as it does for CommsCaller.
package runnerhub

import (
	"context"
	"log/slog"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// WakeAskAnswer builds the ask-answer control op and dispatches it send-only to
// the asking agent's live session over the T3 rail. Best-effort by contract (the
// answer is already durably recorded + fanned out by comms): a missing live
// session is a silent no-op (the agent reads the answer on its next turn via the
// normal delivery path), and a synchronous dispatch refusal is logged, never
// returned — the RPC path must not surface it (comms.AskAnswerWaker is void).
//
// answers is [] *compassv1.AskQuestionAnswer (PUBLIC gen), which is exactly what
// AskAnswerControl.Answers takes (agent.pb.go:696), so it passes straight through
// with no remap. DispatchControl stamps a fresh request id, so none is minted
// here. ctx threads from RespondToAsk (rule://go-thread-context).
func (h *Hub) WakeAskAnswer(ctx context.Context, agent store.AccountID, askID string, answers []*compassv1.AskQuestionAnswer) {
	sessionID, ok := h.SessionForAccount(agent)
	if !ok {
		return // no live session to wake; the agent reads the answer on its next turn
	}
	op := &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_AskAnswer{
			AskAnswer: &compassv1internal.AskAnswerControl{AskId: askID, Answers: answers},
		},
	}
	if err := h.DispatchControl(ctx, sessionID, op); err != nil {
		h.log.WarnContext(ctx, "ask-answer wake: dispatch failed, agent reads on next turn",
			slog.Any("error", err),
			slog.String("session_id", sessionID),
			slog.String("ask_id", askID))
	}
}
