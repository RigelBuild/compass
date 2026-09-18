//go:build unix

package runnerhub

// The resume handoff on the start relay. StartResume relays the SAME
// StartAgentSession command as Start, but attaches the server-reconstructed
// session body onto the INTERNAL resume_body envelope — outside the public
// request, so no client can supply a body. The public request is relayed VERBATIM.

import (
	"context"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// StartResume relays a StartAgentSession command carrying the reconstructed
// resume body on the internal envelope. resumeBody is the T5-reconstructed
// session-JSONL the Runner materializes into the container before exec
// (dispatch.go reads cmd.GetResumeBody().GetSessionBody()); it is attached ONLY
// here, never on the public request. The caller (the service handler) has
// already authorized the resume_session_id and bound the lifetime before this
// relay — the record's "authz BEFORE any Runner call" ordering.
//
// Post-relay it promotes the container's account binding onto the minted live
// session id exactly as Start does, so a resumed session's comms calls resolve
// the same way a fresh one's do — INCLUDING the RIG-3696 fail-closed rollback: a
// refused promotion (the session id is durably owned by another account) stops
// the session the Runner just started rather than leaving a live agent whose
// comms calls resolve nowhere. A resume deliberately REUSES the logical session
// id as the live id, so it is the leg where a durably-owned id is reachable at
// all — the conflict means the resumed id belongs to a different account, which
// is precisely the resume that must not proceed.
func (h *Hub) StartResume(ctx context.Context, requestID string, req *compassv1.StartAgentSessionRequest, resumeBody []byte) (*compassv1.StartAgentSessionResponse, error) {
	result, _, err := h.relay(ctx, req.GetContainerName(), &compassv1internal.SessionsResponse{
		RequestId:  orNewRequestID(requestID),
		Command:    &compassv1internal.SessionsResponse_Start{Start: req},
		ResumeBody: &compassv1internal.ResumeBody{SessionBody: string(resumeBody)},
	})
	if err != nil {
		return nil, err
	}
	resp := result.GetStart()
	if promoteErr := h.promoteSession(ctx, req.GetContainerName(), resp.GetSessionId()); promoteErr != nil {
		return nil, h.rollbackStartedSession(ctx, resp.GetSessionId(), promoteErr)
	}
	return resp, nil
}
