//go:build podman

package e2e

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

// PostMessage posts one text block to a channel's topic over
// CommsService.PostMessage and returns the server-assigned message id. It
// addresses the channel by id (the PostMessageRequest.container oneof) and the
// topic by name (the topic oneof — get-or-create by name), matching the
// agent-initiated post shape the in-process suite exercises
// (integration_pgtest_test.go:162-163). Returns an error rather than panicking
// so the caller (a test) decides fatality; the per-call deadline is threaded
// from ctx.
func (f *Fixture) PostMessage(ctx context.Context, channelID, topicName, text string) (messageID string, err error) {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Comms().PostMessage(rctx, connect.NewRequest(&compassv1.PostMessageRequest{
		Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: channelID},
		Topic:     &compassv1.PostMessageRequest_TopicName{TopicName: topicName},
		Blocks:    []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: text}}},
	}))
	if err != nil {
		return "", fmt.Errorf("PostMessage RPC: %w", err)
	}
	return resp.Msg.GetMessage().GetId(), nil
}

// SubscribeMember adds accountID to channelID's membership AND marks it
// subscribed over CommsService.UpdateChannelMembers, so the account joins the
// channel's DELIVER set. It is the leg-4 second-recipient join: the reused leg-3
// spawner is subscribed-but-unmentioned onto the mentioned peer's home channel,
// making it a plain deliver target while the mentioned peer is steered. Both the
// add and the subscribe lists are set in the one request because the deliver set
// (store SubscribedAgents, delivery_reads.go) requires the member's subscribed
// flag on a non-home, non-mandatory channel — a bare add inserts subscribed=FALSE
// and the account is filtered out of the deliver set; subscribe_account_ids
// requires the account already be a current or added member (comms.proto:644-645),
// so the two travel together. Returns an error rather than panicking so the
// caller (a test) decides fatality; the per-call deadline is threaded from ctx.
func (f *Fixture) SubscribeMember(ctx context.Context, channelID, accountID string) error {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	if _, err := f.Comms().UpdateChannelMembers(rctx, connect.NewRequest(&compassv1.UpdateChannelMembersRequest{
		ChannelId:           channelID,
		AddMemberAccountIds: []string{accountID},
		SubscribeAccountIds: []string{accountID},
	})); err != nil {
		return fmt.Errorf("UpdateChannelMembers RPC: %w", err)
	}
	return nil
}

// SubscribeComms opens the CommsService.SubscribeComms server-stream and returns
// the opened stream for the caller to consume and Close. sinceSeq is the replay
// cursor: 0 snapshots current state as events then tails live, >0 replays only
// events past that seq (comms.proto:888-895). The stream's lifetime is bound to
// ctx — pass a bounded ctx (or bound the wait with AwaitDelivery) so a wedged
// stream fails visibly rather than hanging; the caller MUST Close the returned
// stream. Returns an error rather than panicking so the caller (a test) decides
// fatality.
func (f *Fixture) SubscribeComms(ctx context.Context, sinceSeq uint64) (*connect.ServerStreamForClient[compassv1.SubscribeCommsResponse], error) {
	stream, err := f.Comms().SubscribeComms(ctx, connect.NewRequest(&compassv1.SubscribeCommsRequest{
		SinceSeq: sinceSeq,
	}))
	if err != nil {
		return nil, fmt.Errorf("SubscribeComms RPC: %w", err)
	}
	return stream, nil
}

// AwaitDelivery blocks until a MessagePosted whose Message satisfies match fans
// onto stream, returning that message; it fails fast on ctx deadline or stream
// close. It is FULLY EVENT-GATED: a goroutine pumps stream.Receive() and the
// select races each event against ctx — no sleeps, no polling, no retry loops.
// It derives its own deadline from ctx (deliverTimeout) so a subscription that
// never carries a matching post fails visibly here rather than blocking to the
// go-test timeout. The deliver-side observation counterpart to PostMessage; the
// caller still owns Close on stream (see SubscribeComms).
func (f *Fixture) AwaitDelivery(ctx context.Context, stream *connect.ServerStreamForClient[compassv1.SubscribeCommsResponse], match func(*compassv1.Message) bool) (*compassv1.Message, error) {
	ctx, cancel := context.WithTimeout(ctx, deliverTimeout)
	defer cancel()

	type received struct {
		msg *compassv1.Message
		err error
	}
	// Buffered so the pump goroutine never blocks writing its terminal result
	// after this method has already returned on the ctx deadline — it sends
	// once and exits, no leak past the caller's Close.
	out := make(chan received, 1)
	go func() {
		for stream.Receive() {
			if m := stream.Msg().GetMessagePosted().GetMessage(); m != nil && match(m) {
				out <- received{msg: m}
				return
			}
		}
		if err := stream.Err(); err != nil {
			out <- received{err: fmt.Errorf("SubscribeComms stream: %w", err)}
			return
		}
		out <- received{err: fmt.Errorf("SubscribeComms stream ended before a matching MessagePosted arrived")}
	}()

	select {
	case r := <-out:
		return r.msg, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("awaiting matching MessagePosted: %w", ctx.Err())
	}
}
