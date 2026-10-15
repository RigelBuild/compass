package store

import (
	"context"
	"fmt"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// ForgeMirrorAsk is the stored ask and its source names for rendering a mirror.
type ForgeMirrorAsk struct {
	Message    Message
	ChannelID  ChannelID
	Channel    string
	Topic      string
	AnsweredBy string
}

// AskForForgeMirror reads an ask and its answer attribution as a visible participant.
func (s *Store) AskForForgeMirror(ctx context.Context, actor AccountID, askID string) (ForgeMirrorAsk, error) {
	if actor == "" || askID == "" {
		return ForgeMirrorAsk{}, fmt.Errorf("%w: actor and ask id are required", ErrInvalidArgument)
	}

	row, err := s.q.AskForForgeMirror(ctx, db.AskForForgeMirrorParams{
		AskID:     askID,
		AccountID: string(actor),
	})
	if noRows(err) {
		return ForgeMirrorAsk{}, fmt.Errorf("%w: ask %q", ErrNotFound, askID)
	}
	if err != nil {
		return ForgeMirrorAsk{}, fmt.Errorf("store: read ask for forge mirror: %w", err)
	}
	message, err := messageFromParts(row.ID, row.TopicID, row.AuthorAccountID, row.AuthorHandle, row.AtUnixMs, row.Blocks, row.TurnSequence)
	if err != nil {
		return ForgeMirrorAsk{}, fmt.Errorf("store: decode ask for forge mirror: %w", err)
	}
	return ForgeMirrorAsk{
		Message:    message,
		ChannelID:  ChannelID(row.ChannelID),
		Channel:    row.ChannelName,
		Topic:      row.TopicName,
		AnsweredBy: row.AnsweredBy,
	}, nil
}
