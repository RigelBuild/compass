//go:build pgtest && unix

package comms

// The agent author-update path has no production caller; it lives here so the
// store's author-update refusals stay covered until that path is deleted or
// given a caller.

import (
	"context"
	"fmt"

	"connectrpc.com/connect/v2"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// commitAgentUpdate replaces the block set of the agent's own message through
// store.UpdateMessageBlocksAsAuthor, then fans out MessageUpdated.
func commitAgentUpdate(
	ctx context.Context,
	c *Comms,
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
	if err := reconcileUpdateAskIDs(ctx, c, account, store.MessageID(msg.GetId()), blocks); err != nil {
		return nil, err
	}
	stored, err := c.store.UpdateMessageBlocksAsAuthor(ctx, account, store.MessageID(msg.GetId()), blocks)
	if err != nil {
		return nil, edgeError(err)
	}
	// Write first, then fan out — the same write-through order PostMessage and
	// RespondToAsk use, so a subscriber never sees an event for a row that did
	// not commit.
	c.publishMessageUpdated(ctx, stored)
	return &compassv1.MessageUpdated{Message: MessageToWire(stored)}, nil
}

// reconcileUpdateAskIDs fills an id-less ask from the stored row and refuses a
// wire ask_id that disagrees with it or a surplus ask the row does not have.
// Matching is positional among ask blocks.
func reconcileUpdateAskIDs(ctx context.Context, c *Comms, actor store.AccountID, id store.MessageID, blocks []store.MessageBlock) error {
	storedAskIDs, err := c.store.MessageAskIDs(ctx, actor, id)
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
			// A surplus ask block has no stored counterpart to reconcile, and an
			// UPDATE cannot introduce a new ask (minted only on POST): an id-less one
			// is unanswerable, a NON-empty one is the forgery the POST path strips (a
			// shared ask_id matches multiple rows). Reject here.
			return connect.NewError(connect.CodeInvalidArgument,
				surplusAskError{block: i}.Error())
		}
		stored := storedAskIDs[askIdx]
		switch {
		case ask.AskID == "":
			ask.AskID = stored
		case ask.AskID != stored:
			return connect.NewError(connect.CodeInvalidArgument,
				askIDMismatchError{block: i, wire: ask.AskID, stored: stored}.Error())
		}
		askIdx++
	}
	return nil
}

// updateBlocksFromWire is blocksFromWire except that an ask keeps its wire
// ask_id: an update carries back the id the append minted. reconcileUpdateAskIDs
// checks that id against the stored row before the write.
func updateBlocksFromWire(blocks []*compassv1.MessageBlock) ([]store.MessageBlock, error) {
	out := make([]store.MessageBlock, 0, len(blocks))
	for _, b := range blocks {
		switch body := b.GetBlock().(type) {
		case *compassv1.MessageBlock_Text:
			if body == nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, errEmptyBlock.Error()).WithCause(errEmptyBlock)
			}
			text := body.Text
			out = append(out, store.MessageBlock{Text: &text})
		case *compassv1.MessageBlock_Ask:
			if body == nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, errEmptyBlock.Error()).WithCause(errEmptyBlock)
			}
			ask, err := askFromWire(body.Ask, "")
			if err != nil {
				return nil, err
			}
			ask.AskID = body.Ask.GetAskId()
			out = append(out, store.MessageBlock{Ask: ask})
		case *compassv1.MessageBlock_AskAnswer:
			return nil, connect.NewError(connect.CodeInvalidArgument, errServerOwnedBlock.Error()).WithCause(errServerOwnedBlock)
		default:
			return nil, connect.NewError(connect.CodeInvalidArgument,
				errEmptyBlock.Error()).WithCause(errEmptyBlock)
		}
	}
	return out, nil
}

// askIDMismatchError: an update's ask_id disagrees with the stored one.
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

// surplusAskError: an update carries an ask the stored row does not have.
type surplusAskError struct {
	block int
}

func (e surplusAskError) Error() string {
	return fmt.Sprintf(
		"comms: block %d is an ask the stored message does not have; an update cannot introduce a new ask (ask_id is minted only when the ask is first posted)",
		e.block)
}
