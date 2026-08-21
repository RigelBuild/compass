//go:build unix

// The Server->Runner control-relay dispatch arm (SEA-1569 T3 §5). Unlike the
// client-facing session command relay (commands.go), a control deliver is
// SEND-ONLY: a successful deliver returns NO synchronous result — success rides
// a later AgentFrame.delivery_ack (Runner->Server), handled by the hub's ack
// arm (hub.go deliverAck). So this path must NOT register a blocking inflight
// call (router.dispatch / relay would hang on waitCall for a result that never
// arrives); it uses the router's send-only send1. A refusal rides the Sessions
// request stream as a RunnerError and is observed asynchronously by
// router.complete (§5), which leaves the cursor unadvanced for the D2 sweep.
package runnerhub

import (
	"context"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// DispatchControl relays a fully-formed control op (for SEA-1569, a message
// deliver) down to sessionID over the owning Runner's Sessions stream, WITHOUT
// blocking for a result — the ControlDispatcher the delivery consumer holds
// (design.md:737-739). It wraps op in a DeliverControl/DispatchControl envelope,
// stamps a fresh request id, and pushes it send-only via the router's send1.
//
// The error return is a SYNCHRONOUS refusal only: no Runner enrolled, or no live
// Sessions stream / an immediate push failure — the consumer treats it as "no
// live session" and falls to the D2 cursor sweep. A Runner-side ASYNC refusal (a
// RunnerError landing later on the send-only id) does NOT surface here; it is
// observed by router.complete and leaves the cursor unadvanced (§5). The cursor
// is never advanced on send — it advances only later on the recipient's
// delivery_ack (deliverAck).
//
// ctx threads from the consumer's dispatch call (rule://go-thread-context); it
// is not currently consulted by the non-blocking send path, but it is accepted
// and passed so a future retention-aware or timeout-bounded push inherits the
// caller's cancellation rather than re-rooting.
func (h *Hub) DispatchControl(ctx context.Context, sessionID string, op *compassv1internal.AgentControl) error {
	_ = ctx
	router, _, err := h.routerFor(sessionID)
	if err != nil {
		return err
	}
	cmd := &compassv1internal.SessionsResponse{
		RequestId: orNewRequestID(""),
		Command: &compassv1internal.SessionsResponse_DeliverControl{
			DeliverControl: &compassv1internal.DispatchControl{
				SessionId: sessionID,
				Op:        op,
			},
		},
	}
	return router.send1(cmd)
}
