//go:build unix

// The agent-comms execution leg: PostAsAccount / ListAsAccount execute one
// agent-initiated comms call as a resolved agent account. The RunnerHub's
// RelayCommsCall handler (internal/runnerhub) calls these after resolving the
// relayed session_id to its bound account — the Runner asserts no account, the
// Server attributes in-process here (transport design Decision #3 / OQ-2,
// comms-tools design T2).
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

// homeChannel resolves the account's home channel id. The account must be an
// agent account (it always has a home channel, minted at CreateAgent —
// store/accounts.go:156-158); a store failure surfaces mapped to its Connect
// code, never leaked verbatim.
func (c *Comms) homeChannel(ctx context.Context, account store.AccountID) (string, error) {
	acc, err := c.store.GetAccount(ctx, account)
	if err != nil {
		return "", edgeError(err)
	}
	return string(acc.Agent.HomeChannelID), nil
}
