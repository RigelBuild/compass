//go:build unix

package runnerhub

// T6 (SEA-1667): the resume handoff on the Server->Runner start relay.
// StartResume is the resume-carrying sibling of Start (commands.go): it relays
// the SAME StartAgentSession command, but additionally attaches the
// server-reconstructed session body onto the INTERNAL SessionsResponse.resume_body
// envelope — an internal field OUTSIDE the public request, so no client can
// supply a body. The public start request is relayed VERBATIM (only the
// authz-checked resume_session_id it already carries). Kept a distinct entry
// point rather than a new param on Start so the fresh-start relay path is
// untouched.

import (
	"context"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
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
// the same way a fresh one's do.
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
	h.promoteSession(req.GetContainerName(), resp.GetSessionId())
	return resp, nil
}
