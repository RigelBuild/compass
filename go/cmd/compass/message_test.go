//go:build unix

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
)

// fakeComms is a fake CommsService handler recording the PostMessage request the
// post verb constructs and returning a canned message id, so the message
// subcommand RPC wiring is tested without a live Server or Postgres.
type fakeComms struct {
	compassv1connect.UnimplementedCommsServiceHandler
	gotPost *compassv1.PostMessageRequest
	gotAuth string
}

func (f *fakeComms) PostMessage(_ context.Context, req *connect.Request[compassv1.PostMessageRequest]) (*connect.Response[compassv1.PostMessageResponse], error) {
	f.gotPost = req.Msg
	f.gotAuth = req.Header().Get("Authorization")
	return connect.NewResponse(&compassv1.PostMessageResponse{
		Message: &compassv1.Message{Id: "msg-123"},
	}), nil
}

// startFakeCommsServer stands up the fake CommsService over a plain-HTTP httptest
// server and returns a client wired to it with the bearer interceptor.
func startFakeCommsServer(t *testing.T, fake *fakeComms) compassv1connect.CommsServiceClient {
	t.Helper()
	path, handler := compassv1connect.NewCommsServiceHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := newCommsClient(connConfig{serverAddr: srv.URL, token: "test-token"})
	if err != nil {
		t.Fatalf("newCommsClient: %v", err)
	}
	return client
}

// TestRunMessagePost asserts post reads the body from the injected stdin (never
// argv), maps channel/topic onto the request's oneofs, lands the body as a text
// block, prints the returned message id, and stamps the bearer token.
func TestRunMessagePost(t *testing.T) {
	fake := &fakeComms{}
	client := startFakeCommsServer(t, fake)

	var out strings.Builder
	in := strings.NewReader("hello fleet\n")
	args := messagePostArgs{channel: "chan-1", topic: "general"}
	if err := runMessagePost(context.Background(), client, args, in, &out); err != nil {
		t.Fatalf("runMessagePost: %v", err)
	}
	if fake.gotPost == nil {
		t.Fatal("PostMessage was not called")
	}
	if fake.gotPost.GetChannelId() != "chan-1" {
		t.Errorf("channel = %q, want chan-1", fake.gotPost.GetChannelId())
	}
	if fake.gotPost.GetTopicName() != "general" {
		t.Errorf("topic = %q, want general", fake.gotPost.GetTopicName())
	}
	blocks := fake.gotPost.GetBlocks()
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	if got := blocks[0].GetText(); got != "hello fleet" {
		t.Errorf("block text = %q, want %q (trailing newline trimmed, from stdin)", got, "hello fleet")
	}
	if got := strings.TrimSpace(out.String()); got != "msg-123" {
		t.Errorf("stdout = %q, want the returned message id msg-123", got)
	}
	if fake.gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want Bearer test-token", fake.gotAuth)
	}
}

// TestRunMessagePostMentions asserts each --mention prepends `@<handle> ` to the
// body in flag order (the server parses @-mentions from the raw text; there is
// no mention field on the wire).
func TestRunMessagePostMentions(t *testing.T) {
	fake := &fakeComms{}
	client := startFakeCommsServer(t, fake)

	var out strings.Builder
	args := messagePostArgs{channel: "chan-1", topic: "general", mentions: []string{"alice", "bob"}}
	if err := runMessagePost(context.Background(), client, args, strings.NewReader("ping"), &out); err != nil {
		t.Fatalf("runMessagePost: %v", err)
	}
	blocks := fake.gotPost.GetBlocks()
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	if got := blocks[0].GetText(); got != "@alice @bob ping" {
		t.Errorf("block text = %q, want %q", got, "@alice @bob ping")
	}
}

// TestRunMessagePostRejections covers the client-side validation that fails
// before any RPC: a missing channel, a missing topic, and an empty stdin body.
func TestRunMessagePostRejections(t *testing.T) {
	tests := []struct {
		name string
		args messagePostArgs
		in   string
		want string
	}{
		{
			name: "missing channel",
			args: messagePostArgs{channel: "", topic: "general"},
			in:   "body",
			want: "--channel",
		},
		{
			name: "missing topic",
			args: messagePostArgs{channel: "chan-1", topic: ""},
			in:   "body",
			want: "--topic",
		},
		{
			name: "empty stdin body",
			args: messagePostArgs{channel: "chan-1", topic: "general"},
			in:   "\n",
			want: "body is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeComms{}
			client := startFakeCommsServer(t, fake)
			var out strings.Builder
			err := runMessagePost(context.Background(), client, tt.args, strings.NewReader(tt.in), &out)
			if err == nil {
				t.Fatalf("runMessagePost(%+v) = nil error, want rejection", tt.args)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.want)
			}
			if fake.gotPost != nil {
				t.Error("PostMessage was called despite validation failure")
			}
		})
	}
}

// TestNewMessagePostFlagParsing asserts the post verb wires its flags: --channel,
// --topic and a repeatable --mention parse off the command line into the args the
// RunE closure forwards, exercised through cobra's flag parser.
func TestNewMessagePostFlagParsing(t *testing.T) {
	cmd := newMessagePostCmd()
	if err := cmd.Flags().Parse([]string{"--channel", "chan-1", "--topic", "general", "--mention", "alice", "--mention", "bob"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	channel, err := cmd.Flags().GetString("channel")
	if err != nil {
		t.Fatalf("GetString channel: %v", err)
	}
	if channel != "chan-1" {
		t.Errorf("channel flag = %q, want chan-1", channel)
	}
	topic, err := cmd.Flags().GetString("topic")
	if err != nil {
		t.Fatalf("GetString topic: %v", err)
	}
	if topic != "general" {
		t.Errorf("topic flag = %q, want general", topic)
	}
	mentions, err := cmd.Flags().GetStringArray("mention")
	if err != nil {
		t.Fatalf("GetStringArray mention: %v", err)
	}
	if len(mentions) != 2 || mentions[0] != "alice" || mentions[1] != "bob" {
		t.Errorf("mention flags = %v, want [alice bob]", mentions)
	}
}

// TestRunMessagePostBound asserts a stdin body over maxMessageBytes is rejected
// before any RPC, while a body exactly at the cap (with or without a trailing
// newline) is accepted and lands as the full text block. Mirrors
// secret_test.go's TestRunSecretSetBound over the shared stdin-cap discipline.
func TestRunMessagePostBound(t *testing.T) {
	args := messagePostArgs{channel: "chan-1", topic: "general"}

	t.Run("over the cap", func(t *testing.T) {
		fake := &fakeComms{}
		client := startFakeCommsServer(t, fake)
		var out strings.Builder
		in := strings.NewReader(strings.Repeat("a", maxMessageBytes+1))
		err := runMessagePost(context.Background(), client, args, in, &out)
		if err == nil {
			t.Fatal("runMessagePost with oversized stdin = nil error, want rejection")
		}
		if !strings.Contains(err.Error(), "limit") {
			t.Errorf("error %q does not mention the limit", err.Error())
		}
		if fake.gotPost != nil {
			t.Error("PostMessage was called despite oversized body")
		}
	})

	t.Run("exactly at the cap", func(t *testing.T) {
		fake := &fakeComms{}
		client := startFakeCommsServer(t, fake)
		var out strings.Builder
		in := strings.NewReader(strings.Repeat("a", maxMessageBytes))
		if err := runMessagePost(context.Background(), client, args, in, &out); err != nil {
			t.Fatalf("runMessagePost at cap: %v", err)
		}
		if fake.gotPost == nil {
			t.Fatal("PostMessage was not called for a body at the cap")
		}
		if got := len(fake.gotPost.GetBlocks()[0].GetText()); got != maxMessageBytes {
			t.Errorf("body length = %d, want %d", got, maxMessageBytes)
		}
	})

	t.Run("content at the cap with a trailing newline", func(t *testing.T) {
		fake := &fakeComms{}
		client := startFakeCommsServer(t, fake)
		var out strings.Builder
		in := strings.NewReader(strings.Repeat("a", maxMessageBytes) + "\n")
		if err := runMessagePost(context.Background(), client, args, in, &out); err != nil {
			t.Fatalf("runMessagePost at cap with trailing newline: %v", err)
		}
		if fake.gotPost == nil {
			t.Fatal("PostMessage was not called for cap-sized content with a trailing newline")
		}
		if got := len(fake.gotPost.GetBlocks()[0].GetText()); got != maxMessageBytes {
			t.Errorf("body length = %d, want %d (newline trimmed, content at cap accepted)", got, maxMessageBytes)
		}
	})
}
