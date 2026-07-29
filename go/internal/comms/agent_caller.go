//go:build unix

// The agent-comms *AsAccount family: execute one agent-originated comms
// operation as a resolved agent account. Two callers, both in the RunnerHub
// (internal/runnerhub), both resolving the relayed session_id to its bound
// account first — the Runner asserts no account, the Server attributes
// in-process here (transport design Decision #3 / OQ-2, comms-tools design T2):
//
//   - PostAsAccount / ListAsAccount serve RelayCommsCall — a comms call the
//     agent made deliberately, as a tool.
//   - CommitAgentPost / CommitAgentUpdate serve the ConversationSink — the
//     write-through that turns a relayed conversation FRAME (the agent's own
//     turn, streamed out as it speaks) into a durable comms row (SEA-1364 T3).
//     They are built on the first pair and the authorizing store update rather
//     than being a second write path.
//
// These are deliberately NOT new CommsService RPCs: an agent-initiated call
// never reaches a network door (it rides the per-container socket to the Runner,
// which relays over RelayCommsCall). They reuse the exact PostMessage /
// ListMessages handler paths a human caller takes — same store calls, same D9
// authz, same idempotency, same event fan-out — by setting the acting account on
// the context via WithActor and delegating. So a comms call the agent makes is
// indistinguishable downstream from one its account made by hand, and no new
// authz code exists to drift.
//
// Fail-closed identity (security-critical). Every method requires a non-empty
// resolved account and errors CodeInvalidArgument on an empty one, so a wiring
// bug that reached here without a resolved caller can never fall through to the
// bootstrap-admin fallback actorFromContext applies on an unattributed context
// (comms.go:330-334): a missing actor is a hard error, never silent admin
// attribution.
package comms

import (
	"context"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// errNoActor is the fail-closed cause when PostAsAccount/ListAsAccount is called
// without a resolved account. It maps to CodeInvalidArgument — a server-side
// wiring fault, never a silent bootstrap-admin attribution.
var errNoActor = connect.NewError(
	connect.CodeInvalidArgument,
	noActorError{},
)

type noActorError struct{}

func (noActorError) Error() string {
	return "comms: agent-initiated call requires a resolved account; refusing to attribute to the bootstrap admin"
}

// errNotAgentAccount is the fail-closed cause when an agent-initiated call
// resolves to an account that is NOT an agent — a user account, whose
// Account.Agent is nil (scanAccount sets the subtype only for the side of the
// join that populated, store/accounts.go:313-322). Only an agent account has a
// home channel, so there is no channel to default to and nothing to post into.
//
// It maps to CodeFailedPrecondition, NOT CodeInvalidArgument, and the difference
// is load-bearing at the relay. The RunnerHub classifies a sink error by its
// Connect code: InvalidArgument is a routine per-frame refusal, counted among
// the frames a healthy relay occasionally rejects, while FailedPrecondition is a
// CONTRACT DEFECT — counted separately and logged as a misconfiguration
// (runnerhub/hub.go, isContractDefect). A session bound to a user account is the
// latter: no retry and no better-formed frame can fix it, and every frame from
// that session will fail identically, so it must not read as routine garbage.
var errNotAgentAccount = connect.NewError(
	connect.CodeFailedPrecondition,
	notAgentAccountError{},
)

type notAgentAccountError struct{}

func (notAgentAccountError) Error() string {
	return "comms: agent-initiated call resolved to a non-agent account, which has no home channel; the session->account binding is wrong"
}

// PostAsAccount executes one agent-initiated PostMessage as account. It sets the
// account on the context (WithActor) and delegates to the same PostMessage
// handler path a human caller takes, so authz (D9), idempotency
// (client_request_id), and MessagePosted fan-out are identical. An empty
// channel_id resolves to the account's home channel before the call. A
// non-member channel collapses to the same CodeNotFound a human gets — the
// agent never learns a channel it cannot see exists.
func (c *Comms) PostAsAccount(
	ctx context.Context,
	account store.AccountID,
	req *compassv1.PostMessageRequest,
) (*compassv1.PostMessageResponse, error) {
	if account == "" {
		return nil, errNoActor
	}
	req, err := c.defaultChannel(ctx, account, req)
	if err != nil {
		return nil, err
	}
	resp, err := c.PostMessage(WithActor(ctx, account), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// ListAsAccount executes one agent-initiated ListMessages as account, mirroring
// PostAsAccount: WithActor + the shared ListMessages handler path, empty
// channel_id defaulting to the account's home channel, non-member channel →
// CodeNotFound.
func (c *Comms) ListAsAccount(
	ctx context.Context,
	account store.AccountID,
	req *compassv1.ListMessagesRequest,
) (*compassv1.ListMessagesResponse, error) {
	if account == "" {
		return nil, errNoActor
	}
	channelID, err := c.defaultListChannel(ctx, account, req.GetChannelId())
	if err != nil {
		return nil, err
	}
	listReq := &compassv1.ListMessagesRequest{
		Container:       &compassv1.ListMessagesRequest_ChannelId{ChannelId: channelID},
		Limit:           req.GetLimit(),
		BeforeMessageId: req.GetBeforeMessageId(),
		SnapshotSeq:     req.GetSnapshotSeq(),
	}
	resp, err := c.ListMessages(WithActor(ctx, account), connect.NewRequest(listReq))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// CommitAgentPost commits one relayed MessagePosted frame as a durable comms row
// under account — the write-through the RunnerHub's ConversationSink drives
// (SEA-1364 T3). It builds a PostMessageRequest from the frame's blocks and
// delegates to PostAsAccount, so this is the SAME PostMessage handler path a
// human takes: same store calls, same D9 write-authz, same idempotency, same
// MessagePosted fan-out. No new authz code exists here to drift from the human
// path.
//
// The Container oneof is deliberately left UNSET. defaultChannel (:109) fills an
// empty channel_id from the account's home channel, so a relayed frame routes to
// the agent's own channel for free — the agent mints no server ids and the
// Runner plumbs no channel id, which is exactly the contract (the frame's
// Message.id / channel are server-assigned, comms.proto:234-242, so any value
// the frame carried would be meaningless here).
//
// parent_message_id IS threaded, unlike the two fields above. It is plumbed end
// to end on both the wire Message (comms.proto:250) and PostMessageRequest
// (comms.proto:565), and it is the ONE piece of routing the frame legitimately
// carries: the id it names is a SERVER id, from a row that already committed, so
// forwarding it asserts nothing the agent minted. Dropping it would silently
// flatten every threaded agent reply to a root message — no error, no log, just
// a conversation that lost its shape. The store validates the parent exists, and
// AppendMessage's membership gate applies to it exactly as it does for a human
// reply, so a frame naming a parent it may not see is refused, not honored.
//
// Idempotency: ClientRequestId is left unset because the relayed frame carries
// no key on this base. PublishEventsRequest is {runner_seq, session_id, frame}
// (proto/compass/v1/runner.proto:169-183), and the agent-minted key lives one hop
// earlier on PostConversationFrameRequest.idempotency_key
// (proto/compass/v1/agent_gateway.proto:113-120), where it terminates at the
// Runner's C2 dedup and is not forwarded upstream. This is NOT a deliberate
// decision to go unkeyed: #894/T2 adds idempotency_key to PublishEventsRequest
// and threads it to this seam, at which point it populates ClientRequestId below
// and the store's (author_account_id, client_request_id) unique constraint
// (messages.go:82) makes the commit genuinely at-most-once. Until then a retried
// frame would commit twice — the gap is the missing key, and the hook is the one
// field on the request built here.
func (c *Comms) CommitAgentPost(
	ctx context.Context,
	account store.AccountID,
	posted *compassv1.MessagePosted,
) (*compassv1.PostMessageResponse, error) {
	if account == "" {
		return nil, errNoActor
	}
	return c.PostAsAccount(ctx, account, &compassv1.PostMessageRequest{
		// Container unset on purpose — see the home-channel note above.
		Blocks:          posted.GetMessage().GetBlocks(),
		ParentMessageId: posted.GetMessage().GetParentMessageId(),
		// ClientRequestId: threaded from the frame's idempotency_key by #894/T2.
	})
}

// CommitAgentUpdate applies one relayed MessageUpdated frame to the row it
// addresses, under account. A streaming turn re-sends its FULL current block set
// (comms.proto:388-390), so this replaces the block set rather than appending.
//
// It goes through store.UpdateMessageBlocksAsAuthor rather than the store's
// unauthorizing core: that core takes a bare MessageID with no membership and no
// authorship check, which is safe for AnswerAsk's already-gated call but is a
// privilege hole for an id that arrived on a relayed frame. The authorizing
// variant requires the actor to be both a member of the message's channel and its
// author, and collapses every refusal — unknown id, foreign message, revoked
// membership — to one ErrNotFound, so a relayed frame cannot enumerate messages
// it may not touch (the D9 not-found/forbidden merge).
//
// Errors map through edgeError to the same Connect codes a human caller gets:
// CodeNotFound for a refused row, CodeInvalidArgument for a malformed frame (an
// empty message.id, an empty block set, an ask block missing its immutable
// ask_id). Those are the refusals the hub treats as non-fatal drops rather than
// stream teardowns.
func (c *Comms) CommitAgentUpdate(
	ctx context.Context,
	account store.AccountID,
	updated *compassv1.MessageUpdated,
) (*compassv1.MessageUpdated, error) {
	if account == "" {
		return nil, errNoActor
	}
	msg := updated.GetMessage()
	blocks, err := blocksFromWire(msg.GetBlocks())
	if err != nil {
		return nil, err
	}
	stored, err := c.store.UpdateMessageBlocksAsAuthor(ctx, account, store.MessageID(msg.GetId()), blocks)
	if err != nil {
		return nil, edgeError(err)
	}
	// Write first, then fan out — the same write-through order PostMessage and
	// RespondToAsk use, so a subscriber never sees an event for a row that did
	// not commit.
	c.publishMessageUpdated(stored)
	return &compassv1.MessageUpdated{Message: messageToWire(stored)}, nil
}

// defaultChannel returns req with an empty channel_id filled from the account's
// home channel, so the common "post in my own channel" case needs no id plumbed
// into the container. A non-empty channel_id is left untouched. The returned
// request is a shallow copy — the caller's message is never mutated.
func (c *Comms) defaultChannel(
	ctx context.Context,
	account store.AccountID,
	req *compassv1.PostMessageRequest,
) (*compassv1.PostMessageRequest, error) {
	if req.GetChannelId() != "" {
		return req, nil
	}
	home, err := c.homeChannel(ctx, account)
	if err != nil {
		return nil, err
	}
	return &compassv1.PostMessageRequest{
		Container:       &compassv1.PostMessageRequest_ChannelId{ChannelId: home},
		Blocks:          req.GetBlocks(),
		ParentMessageId: req.GetParentMessageId(),
		ClientRequestId: req.GetClientRequestId(),
	}, nil
}

// defaultListChannel returns channelID unchanged when set, else the account's
// home channel.
func (c *Comms) defaultListChannel(ctx context.Context, account store.AccountID, channelID string) (string, error) {
	if channelID != "" {
		return channelID, nil
	}
	return c.homeChannel(ctx, account)
}

// homeChannel resolves the account's home channel id. An agent account always
// has one, minted at CreateAgent (store/accounts.go:156-158); a store failure
// surfaces mapped to its Connect code, never leaked verbatim.
//
// A NON-AGENT account is refused rather than dereferenced. scanAccount populates
// Account.Agent only for the agent side of the join (store/accounts.go:313-322),
// so for a user account it is nil and reading acc.Agent.HomeChannelID panicked —
// inside the RunnerHub's PublishEvents handler goroutine, where a panic takes the
// relay down and no error ever reaches the classification the hub does on the
// return value (rule://go-no-panic-in-lib). Failing closed with
// errNotAgentAccount both keeps the process up and gives the hub a code it can
// route: FailedPrecondition marks it a contract defect, so a bad binding is
// reported as the wiring fault it is instead of hiding among per-frame refusals.
//
// This is the call-site guard only. The deeper question — that a container can be
// bound to a non-agent account at all, i.e. the ordering of bindContainer's
// validation against RecordAgentContainer — belongs to that flow, not here.
func (c *Comms) homeChannel(ctx context.Context, account store.AccountID) (string, error) {
	acc, err := c.store.GetAccount(ctx, account)
	if err != nil {
		return "", edgeError(err)
	}
	if acc.Agent == nil {
		return "", errNotAgentAccount
	}
	return string(acc.Agent.HomeChannelID), nil
}
