//go:build unix

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/mirror"
	"github.com/RigelBuild/compass/go/internal/refs"
	"github.com/RigelBuild/compass/go/internal/store"
)

// agentMirrorPayload is an agent message mirror's render inputs, stored on the
// row so a redrive after restart rebuilds the same body from the message store.
type agentMirrorPayload struct {
	ChannelID  string   `json:"channel_id"`
	TopicID    string   `json:"topic_id,omitempty"`
	MessageIDs []string `json:"message_ids,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// mirrorMessages is the agent mirror tool: it checks the source and target,
// claims a row keyed by the call's client_request_id, and drives it. A retry
// with the same key returns the first row's outcome and never posts again.
func (s *forgeService) mirrorMessages(ctx context.Context, caller store.AccountID, sessionID string, call *compassv1internal.ForgeCallRequest, req *compassv1internal.MirrorMessagesRequest) *compassv1internal.ForgeCallResult {
	topicID, messageIDs := req.GetTopicId(), req.GetMessageIds()
	if (topicID == "") == (len(messageIDs) == 0) {
		return forgeErrorResult(forgeErr(connect.CodeInvalidArgument, "forge mirror: exactly one of message_ids or topic_id is required"))
	}
	if len(messageIDs) > 50 {
		return forgeErrorResult(forgeErr(connect.CodeInvalidArgument, "forge mirror: message_ids must contain 1 to 50 ids"))
	}
	ref, err := refs.Parse(req.GetRef())
	if err != nil {
		return forgeErrorResult(forgeErr(connect.CodeInvalidArgument, "forge mirror: "+err.Error()))
	}
	if s.mirrorStoreDB == nil {
		return forgeErrorResult(forgeErr(connect.CodeUnavailable, "forge mirror: message store is not configured"))
	}
	account, err := s.store.GetAccount(ctx, caller)
	if err != nil {
		return forgeErrorResult(storeForgeError(err))
	}
	if !account.IsAgent() {
		return forgeErrorResult(forgeErr(connect.CodeInvalidArgument, "forge mirror: caller must be an agent"))
	}
	rf, repo, _, fe := s.mirrorAllowed(ctx, account, store.ForgeMirrorAuto, ref)
	if fe != nil {
		return forgeErrorResult(fe)
	}
	channelID, fe := s.mirrorSourceChannel(ctx, caller, req.GetChannel(), topicID, messageIDs)
	if fe != nil {
		return forgeErrorResult(fe)
	}
	payload := agentMirrorPayload{ChannelID: string(channelID), TopicID: topicID, Note: req.GetNote()}
	if len(messageIDs) > 0 {
		payload.MessageIDs = slices.Sorted(slices.Values(messageIDs))
	}
	resolved, err := s.resolveAgentMirror(ctx, caller, payload)
	if err != nil {
		return forgeErrorResult(mirrorSourceError(err))
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return forgeErrorResult(forgeErr(connect.CodeInternal, "forge mirror: encode source payload: "+err.Error()))
	}
	key := call.GetClientRequestId()
	row, created, err := s.mirrorStoreDB.ClaimForgeMirror(ctx, store.ForgeMirrorRow{
		Source: 2, SourceKey: resolved.sourceKey, Actor: caller, Agent: caller, ClientRequestID: key,
		TargetRef: ref.String(), BodyRev: 1, State: 1, SessionID: sessionID, SourcePayload: raw,
	})
	if err != nil {
		return forgeErrorResult(storeForgeError(err))
	}
	if !created {
		if row.Source != 2 || row.Actor != caller || row.Agent != caller {
			return forgeErrorResult(forgeErr(connect.CodeFailedPrecondition, "forge mirror: source conflicts with another actor"))
		}
		// Only the claiming call posts; a retry reports the row as it stands.
		if key != "" && row.ClientRequestID == key {
			return s.mirrorRowResult(ctx, rf, repo, ref, row, nil)
		}
		if row.SourceKey != resolved.sourceKey || row.TargetRef != ref.String() {
			return forgeErrorResult(forgeErr(connect.CodeFailedPrecondition, "forge mirror: source conflicts with another mirror"))
		}
	}
	result, ok := s.awaitMirrorDrive(ctx, row, s.agentMessagesBodyForRow())
	if !ok {
		return forgeErrorResult(forgeErr(connect.CodeUnavailable, "forge mirror: still delivering"))
	}
	return s.mirrorRowResult(ctx, rf, repo, ref, result.row, result.err)
}

// mirrorSourceChannel returns the channel holding the source. A named channel
// resolves for the caller as the comms tools resolve names, and must hold the
// source; every miss is the same not_found, so an invisible channel reads as
// an unknown one.
func (s *forgeService) mirrorSourceChannel(ctx context.Context, caller store.AccountID, name, topicID string, messageIDs []string) (store.ChannelID, *compassv1internal.ForgeCallError) {
	var derived store.ChannelID
	var err error
	switch {
	case topicID != "":
		derived, err = s.mirrorStoreDB.TopicChannel(ctx, topicID)
	case len(messageIDs) > 0:
		derived, err = s.mirrorStoreDB.MessageChannel(ctx, messageIDs[0])
	default:
		return "", forgeErr(connect.CodeInvalidArgument, "forge mirror: provide exactly one of topic_id or message_ids")
	}
	if err != nil {
		return "", mirrorSourceError(err)
	}
	if name != "" {
		ch, err := s.mirrorStoreDB.ChannelByNameForViewer(ctx, caller, name)
		if err != nil {
			return "", mirrorSourceError(err)
		}
		if ch.ID != derived {
			return "", mirrorSourceNotFound()
		}
	}
	return derived, nil
}

// resolveAgentMirror runs the shared source resolution with the agent as
// actor, so both the call and every redrive re-check visibility.
func (s *forgeService) resolveAgentMirror(ctx context.Context, agent store.AccountID, payload agentMirrorPayload) (resolvedMirrorMessages, error) {
	resolved, err := s.resolveMirrorMessages(ctx, agent, payload.ChannelID, "", payload.TopicID, payload.MessageIDs)
	if err != nil {
		return resolvedMirrorMessages{}, err
	}
	channel, err := s.mirrorStoreDB.GetChannel(ctx, store.ChannelID(payload.ChannelID))
	if err != nil {
		return resolvedMirrorMessages{}, mirrorStoreError(err)
	}
	resolved.link.Channel = channel.Name
	return resolved, nil
}

// agentMessagesBodyForRow renders an agent message row from its stored
// payload. A source the agent can no longer see is a refusal: the row fails
// and is not posted.
func (s *forgeService) agentMessagesBodyForRow() mirror.BodyFunc {
	return func(ctx context.Context, row store.ForgeMirrorRow, limit int) (string, error) {
		var payload agentMirrorPayload
		if err := json.Unmarshal(row.SourcePayload, &payload); err != nil {
			return "", fmt.Errorf("mirror: decode source payload: %w", err)
		}
		resolved, err := s.resolveAgentMirror(ctx, row.Agent, payload)
		if connect.CodeOf(err) == connect.CodeNotFound {
			return "", &mirrorRefusalError{code: connect.CodeNotFound, message: "mirror: source is no longer visible to the agent"}
		}
		if err != nil {
			return "", err
		}
		note := payload.Note
		if resolved.note != "" {
			if note != "" {
				note += "\n\n"
			}
			note += resolved.note
		}
		body := mirror.Render(resolved.messages, messageAuthors(resolved.messages), resolved.link, note, limit)
		return truncateBody(body, limit), nil
	}
}

// mirrorRowResult maps a row to the tool result: its comment once done, the
// stored error once failed, and unavailable while it is still pending.
func (s *forgeService) mirrorRowResult(ctx context.Context, rf resolvedForge, repo string, ref refs.Ref, row store.ForgeMirrorRow, driveErr error) *compassv1internal.ForgeCallResult {
	switch row.State {
	case 2:
		isPR, err := s.mirrorTargetIsPullRequest(ctx, rf, repo, ref)
		if err != nil {
			return forgeErrorResult(forgeErr(connect.CodeInternal, err.Error()))
		}
		comment := &compassv1internal.CommentRef{Url: row.CommentURL, CommentId: row.CommentID}
		if isPR {
			return &compassv1internal.ForgeCallResult{Result: &compassv1internal.ForgeCallResult_PrComment{PrComment: comment}}
		}
		return &compassv1internal.ForgeCallResult{Result: &compassv1internal.ForgeCallResult_IssueComment{IssueComment: comment}}
	case 3:
		code := connect.CodeFailedPrecondition
		if driveErr != nil {
			code = mirrorFailureCode(driveErr)
		}
		return forgeErrorResult(forgeErr(code, row.LastError))
	default:
		return forgeErrorResult(forgeErr(connect.CodeUnavailable, "forge mirror: still delivering"))
	}
}

// mirrorSourceError maps a source lookup error; every not-found collapses to
// one message so an unknown and an invisible source are indistinguishable.
func mirrorSourceError(err error) *compassv1internal.ForgeCallError {
	if errors.Is(err, store.ErrNotFound) || connect.CodeOf(err) == connect.CodeNotFound {
		return mirrorSourceNotFound()
	}
	if connectErr, ok := errors.AsType[*connect.Error](err); ok {
		return forgeErr(connectErr.Code(), connectErr.Message())
	}
	return storeForgeError(err)
}

func mirrorSourceNotFound() *compassv1internal.ForgeCallError {
	return forgeErr(connect.CodeNotFound, "forge mirror: source not found")
}
