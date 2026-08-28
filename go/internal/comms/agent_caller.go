//go:build unix

// The agent-comms *AsAccount family: execute one agent-originated comms
// operation as a resolved agent account. Two callers, both in the RunnerHub
// (internal/runnerhub), both resolving the relayed session_id to its bound
// account first — the Runner asserts no account, the Server attributes
// in-process here (transport design Decision #3 / OQ-2, comms-tools design T2):
//
//   - PostAsAccount / ListAsAccount serve RelayCommsCall — a comms call the
//     agent made deliberately, as a tool.
//   - CommitAgentPost / CommitAgentUpdate turn a relayed conversation FRAME (the
//     agent's own turn, streamed out as it speaks) into a durable comms row
//     (RIG-1364 T3). They survive only as test helpers now: their production
//     caller (the ConversationSink write-through) was removed with the sink, so
//     no non-test path reaches them.
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
	"fmt"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// agentConversationTopic is the topic name a relayed agent conversation frame is
// committed under (CommitAgentPost, a test-only helper). The landed store has no
// home-topic default, so an append must name a topic; a relayed frame carries no
// routing of its own, so its turns collect under this one get-or-created topic in
// the agent's channel. "general" matches the store's own test convention.
const agentConversationTopic = "general"

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

// askIDMismatchError is the cause when a relayed UPDATE frame's ask block carries
// a non-empty ask_id that DISAGREES with the id stored for that block — a
// forged/malformed frame, distinct from an ask block that simply arrived id-less
// (which reconcileUpdateAskIDs fills from the stored row). It maps to
// CodeInvalidArgument, but its own message so the mismatch reads as the specific
// defect it is, never conflated with the generic "no ask_id" refusal.
type askIDMismatchError struct {
	block  int
	wire   string
	stored string
}

func (e askIDMismatchError) Error() string {
	return fmt.Sprintf(
		"comms: block %d ask_id %q from the update frame does not match the stored ask_id %q; ask_id is immutable and an update must carry the stored id, not a different one",
		e.block, e.wire, e.stored)
}

// surplusAskError is the cause when a relayed UPDATE frame carries MORE ask
// blocks than the stored message has — a surplus ask with no stored counterpart
// to reconcile against. An update cannot introduce a new ask (a fresh ask_id is
// minted only on POST), so a surplus ask is always illegitimate: rejecting it
// keeps a caller-chosen ask_id off the UPDATE path, the same no-forged-ask_id
// invariant askFromWire enforces for POST. Maps to CodeInvalidArgument.
type surplusAskError struct {
	block int
}

func (e surplusAskError) Error() string {
	return fmt.Sprintf(
		"comms: block %d is an ask the stored message does not have; an update cannot introduce a new ask (ask_id is minted only when the ask is first posted)",
		e.block)
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

// UpdatePinnedBoardAsAccount executes one agent-initiated UpdatePinnedBoard as
// account, mirroring PostAsAccount: WithActor + the shared UpdatePinnedBoard
// handler path, so the board authz (post_policy), the pure-pointer store ops,
// and the ChannelChanged fan-out are identical to a human caller's. A non-member
// or non-owner (on OWNER_ONLY) channel collapses to the same CodeNotFound a
// human gets — the agent never learns a board it cannot mutate exists. The pin
// request always names its channel explicitly (the board is not the agent's home
// channel by default), so there is no home-channel defaulting here.
func (c *Comms) UpdatePinnedBoardAsAccount(
	ctx context.Context,
	account store.AccountID,
	req *compassv1.UpdatePinnedBoardRequest,
) (*compassv1.UpdatePinnedBoardResponse, error) {
	if account == "" {
		return nil, errNoActor
	}
	resp, err := c.UpdatePinnedBoard(WithActor(ctx, account), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// CommitAgentPost commits one relayed MessagePosted frame as a durable comms row
// under account. It builds a PostMessageRequest from the frame's blocks and
// delegates to PostAsAccount, so this is the SAME PostMessage handler path a
// human takes: same store calls, same D9 write-authz, same idempotency, same
// MessagePosted fan-out. No new authz code exists here to drift from the human
// path.
//
// The Container is deliberately left UNSET — defaultChannel fills an empty
// channel_id from the account's home channel, so a relayed frame lands in the
// agent's own channel for free (the agent mints no server ids and the Runner
// plumbs no channel). The topic, by contrast, MUST be named: the landed store
// requires exactly one of topic id or name on every append (there is no
// home-topic default), so this routes the frame to the agent channel's
// conversation topic by name.
//
// This unkeyed post sets no ClientRequestId, so it is loss-tolerant, not
// at-most-once. It has no production caller — the ConversationSink write-through
// that drove it was removed — and survives only as a test helper for the
// PostMessage-through-an-agent-account path.
func (c *Comms) CommitAgentPost(
	ctx context.Context,
	account store.AccountID,
	posted *compassv1.MessagePosted,
) (*compassv1.PostMessageResponse, error) {
	if account == "" {
		return nil, errNoActor
	}
	return c.PostAsAccount(ctx, account, &compassv1.PostMessageRequest{
		// Container unset: routes to the agent's home channel (defaultChannel).
		// Topic named: the store has no home-topic default, so the frame's
		// conversation is addressed by topic name.
		Topic:  &compassv1.PostMessageRequest_TopicName{TopicName: agentConversationTopic},
		Blocks: posted.GetMessage().GetBlocks(),
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
// empty message.id, an empty block set, or an ask block whose ask_id cannot be
// reconciled against the stored row — see reconcileUpdateAskIDs). Those are the
// refusals the hub treats as non-fatal drops rather than stream teardowns.
//
// Ask_id handling is the one place the UPDATE frame differs from a POST. A POST
// strips any wire ask_id and lets the store mint one; an update must instead
// carry back the id the append already minted, so this path maps blocks with
// updateBlocksFromWire (which PRESERVES the wire ask_id) and then reconciles that
// id against the stored row before the write. The stored ask_id is authoritative
// (it is immutable once minted), so reconciliation fills an id-less update ask
// from the stored ask and rejects a non-empty wire ask_id that disagrees with
// the stored one as a forged/malformed frame — never blindly trusting the wire
// value, which would reopen the forgery/collision risk askFromWire guards.
func (c *Comms) CommitAgentUpdate(
	ctx context.Context,
	account store.AccountID,
	updated *compassv1.MessageUpdated,
) (*compassv1.MessageUpdated, error) {
	if account == "" {
		return nil, errNoActor
	}
	msg := updated.GetMessage()
	blocks, err := updateBlocksFromWire(msg.GetBlocks())
	if err != nil {
		return nil, err
	}
	if err := c.reconcileUpdateAskIDs(ctx, store.MessageID(msg.GetId()), blocks); err != nil {
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
	return &compassv1.MessageUpdated{Message: MessageToWire(stored)}, nil
}

// reconcileUpdateAskIDs makes the ask blocks of a relayed UPDATE frame carry the
// stored, server-owned ask_id — the safe alternative to trusting the wire value.
// It reads the stored row's ask_ids (an immutable field, so a separate read from
// the authz UPDATE that follows is race-free — see store.MessageAskIDs) and, for
// the k-th ask block of the frame, reconciles it against the k-th stored ask:
//
//   - an id-LESS update ask is filled from the stored ask_id (the common case —
//     the id the append minted, which the store then requires);
//   - a non-empty wire ask_id that MATCHES the stored one is left as-is;
//   - a non-empty wire ask_id that DISAGREES with the stored one is a
//     forged/malformed frame, rejected CodeInvalidArgument and distinctly
//     messaged (never conflated with the generic id-less case, and never
//     silently overwriting the stored id).
//
// Matching is POSITIONAL among ask blocks: a streaming turn re-sends its FULL
// current block set in stable order (comms.proto:388-390), so the k-th ask of
// the frame is the k-th ask of the row. If the frame carries MORE ask blocks
// than the stored row, the surplus ask has no stored counterpart and is
// REJECTED (CodeInvalidArgument): an update cannot introduce a brand-new ask —
// a fresh ask is minted only on POST — so a surplus ask is always illegitimate,
// whether id-less (unanswerable) or carrying a caller-chosen id (the forgery the
// POST path strips to keep RespondToAsk's containment SELECT unambiguous). The
// store guards only the empty-id case, so rejecting here is what closes the
// non-empty forged-id surplus.
func (c *Comms) reconcileUpdateAskIDs(ctx context.Context, id store.MessageID, blocks []store.MessageBlock) error {
	storedAskIDs, err := c.store.MessageAskIDs(ctx, id)
	if err != nil {
		return edgeError(err)
	}
	askIdx := 0
	for i := range blocks {
		ask := blocks[i].Ask
		if ask == nil {
			continue
		}
		if askIdx >= len(storedAskIDs) {
			// A surplus ask block (beyond the stored ask count) has no stored
			// counterpart to reconcile against. An UPDATE cannot legitimately
			// introduce a new ask — a fresh ask is minted only on the POST path
			// (mintAskIDs) — so any surplus ask is illegitimate regardless of its
			// ask_id: an id-less one could not be answered (no minted id), and a
			// NON-empty one is a caller-chosen id, exactly the forgery the POST
			// path strips to prevent (askFromWire: a shared ask_id makes
			// RespondToAsk's containment SELECT match multiple rows). Reject it
			// here rather than passing a wire id through to the store, which only
			// guards the empty case.
			return connect.NewError(connect.CodeInvalidArgument,
				surplusAskError{block: i})
		}
		stored := storedAskIDs[askIdx]
		switch {
		case ask.AskID == "":
			ask.AskID = stored
		case ask.AskID != stored:
			return connect.NewError(connect.CodeInvalidArgument,
				askIDMismatchError{block: i, wire: ask.AskID, stored: stored})
		}
		askIdx++
	}
	return nil
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
		Topic:           req.GetTopic(),
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
