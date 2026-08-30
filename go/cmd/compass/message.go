//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
)

// maxMessageBytes caps the stdin read in `message post`. A channel message is
// kilobytes of markdown at most; the cap only stops a stray
// `compass message post ... < largefile` from allocating an entire file into
// memory (mirrors maxSecretBytes at secret.go:36).
const maxMessageBytes = 1 << 20 // 1 MiB

// newMessageCmd builds the message noun: the operator surface for posting into a
// channel/topic over CommsService/PostMessage. Like the secret noun it carries
// no logic of its own; the post verb dials the Server and drives one RPC. The
// message body is read from stdin, never argv, so it cannot leak into the
// process table (the load-bearing convention shared with the bearer token and
// secret values).
func newMessageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "message",
		Short: "Post messages into fleet channels (post)",
	}
	cmd.AddCommand(newMessagePostCmd())
	return cmd
}

// newMessagePostCmd builds `message post --channel <id> --topic <name> [--mention
// <handle>]`: post one message into a channel's topic, the body read from stdin.
// --topic is a get-or-create-by-name: this operator surface is a trusted minter,
// so it sets CreateTopic on the request and an unknown name mints the topic (the
// get-or-create gate is store.resolveTopicForAppend, keyed on
// PostMessageRequest.create_topic). --mention prepends
// `@<handle> ` to the body; the server parses @-mentions from the raw text, so
// there is no separate mention field on the wire (PostMessageRequest carries
// only container/topic/blocks). The body is read from stdin, never a flag or
// positional, so it cannot leak into the process table.
func newMessagePostCmd() *cobra.Command {
	var channel, topic string
	var mentions []string
	cmd := &cobra.Command{
		Use:   "post --channel <id> --topic <name>",
		Short: "Post a message into a channel topic (body read from stdin, admin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := dialCommsClient(cmd)
			if err != nil {
				return err
			}
			return runMessagePost(cmd.Context(), client, messagePostArgs{
				channel:  channel,
				topic:    topic,
				mentions: mentions,
			}, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&channel, "channel", "",
		"Channel id to post into (required).")
	cmd.Flags().StringVar(&topic, "topic", "",
		"Topic name within the channel (required; an unknown name creates the topic).")
	cmd.Flags().StringArrayVar(&mentions, "mention", nil,
		"Handle to @-mention; prepended to the body as `@<handle> `. Repeatable.")
	return cmd
}

// errEmptyMessageBody names the empty-stdin rejection: a message body is
// required and it is read from stdin.
var errEmptyMessageBody = errors.New("a message body is required: pipe it on stdin (it is never taken from the command line)")

// messagePostArgs is the resolved `message post` input: the channel and topic to
// address and the optional mentions to prepend, validated before any RPC.
type messagePostArgs struct {
	channel  string
	topic    string
	mentions []string
}

// runMessagePost validates the required channel/topic, reads the body from in
// (trimming a single trailing newline and rejecting an empty body), prepends any
// --mention handles as `@<handle> `, and calls PostMessage. The body is never
// taken from argv, so it cannot leak into the process table.
func runMessagePost(ctx context.Context, client compassv1connect.CommsServiceClient, args messagePostArgs, in io.Reader, out io.Writer) error {
	if args.channel == "" {
		return errors.New("--channel is required: the channel id to post into")
	}
	if args.topic == "" {
		return errors.New("--topic is required: the topic name within the channel")
	}
	raw, err := io.ReadAll(io.LimitReader(in, maxMessageBytes+2))
	if err != nil {
		return fmt.Errorf("reading message body from stdin: %w", err)
	}
	body := strings.TrimSuffix(string(raw), "\n")
	if len(body) > maxMessageBytes {
		return fmt.Errorf("message body exceeds the %d-byte limit: pipe a smaller body on stdin", maxMessageBytes)
	}
	if body == "" {
		return errEmptyMessageBody
	}
	// The server parses @-mentions from the raw text (there is no mention field
	// on the wire), so a --mention becomes a literal `@<handle> ` prefix. Multiple
	// mentions prepend in flag order.
	if len(args.mentions) > 0 {
		var b strings.Builder
		for _, m := range args.mentions {
			fmt.Fprintf(&b, "@%s ", m)
		}
		b.WriteString(body)
		body = b.String()
	}

	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	// No ClientRequestId: an operator re-running `compass message post` is a
	// genuine second post, not a retry of a lost one (connect-go does not
	// auto-retry), so the (author, client_request_id) idempotency lane is left
	// unused deliberately rather than minting a per-invocation key.
	resp, err := client.PostMessage(ctx, connect.NewRequest(&compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: args.channel},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: args.topic},
		CreateTopic: true,
		Blocks: []*compassv1.MessageBlock{
			{Block: &compassv1.MessageBlock_Text{Text: body}},
		},
	}))
	if err != nil {
		return fmt.Errorf("posting message to channel %s topic %s: %w", args.channel, args.topic, err)
	}
	_, err = fmt.Fprintln(out, resp.Msg.GetMessage().GetId())
	return err
}
