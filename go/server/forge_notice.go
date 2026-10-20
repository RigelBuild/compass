package server

import (
	"context"
	"errors"
	"fmt"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// agentNotifier is the comms seam the forge uses to tell an agent about a write
// another agent made to its work. *comms.Comms satisfies it.
type agentNotifier interface {
	OpenDMAsAccount(ctx context.Context, account store.AccountID, req *compassv1.OpenDMRequest) (*compassv1.OpenDMResponse, error)
	PostAsAccount(ctx context.Context, account store.AccountID, req *compassv1.PostMessageRequest) (*compassv1.PostMessageResponse, error)
}

// noticeTopic is the DM topic forge notices land in, so they stay apart from the agents' own threads.
const noticeTopic = "forge"

// notifyAgent posts text from one agent into its DM with a same-owner agent.
// PostAsAccount stores and publishes the message as PostMessage does, so a live
// agent gets MessagePosted and an offline one finds it in its undelivered sweep.
func (s *forgeService) notifyAgent(ctx context.Context, from, to store.AccountID, key, text string) error {
	if s.notifier == nil {
		return errors.New("forge: no agent notifier configured")
	}
	peer, err := s.store.GetAccount(ctx, to)
	if err != nil {
		return fmt.Errorf("read agent %s: %w", to, err)
	}
	dm, err := s.notifier.OpenDMAsAccount(ctx, from, &compassv1.OpenDMRequest{PeerHandle: peer.Handle})
	if err != nil {
		return fmt.Errorf("open DM with %s: %w", peer.Handle, err)
	}
	_, err = s.notifier.PostAsAccount(ctx, from, &compassv1.PostMessageRequest{
		Container:       &compassv1.PostMessageRequest_ChannelId{ChannelId: dm.GetChannel().GetId()},
		Topic:           &compassv1.PostMessageRequest_TopicName{TopicName: noticeTopic},
		CreateTopic:     true,
		Blocks:          []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: text}}},
		ClientRequestId: key,
	})
	if err != nil {
		return fmt.Errorf("post notice to %s: %w", peer.Handle, err)
	}
	return nil
}
