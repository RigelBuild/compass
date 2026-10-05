//go:build unix

package runnerhub

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/agentmsg"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// The mounted RunnerService door caps one inbound message at runnerMaxReadBytes:
// a request just past it is refused with ResourceExhausted before the handler
// runs, while one carrying a full-size agent message still gets through.
func TestRunnerDoorCapsInboundMessageSize(t *testing.T) {
	hub := newHubOnly()
	resolver := &fakeResolver{tokens: map[string]resolverEntry{
		"runner-tok": {subj: store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}},
	}}
	client := newRawRunnerClient(t, newMountedH2CServer(t, hub, resolver.resolve), "runner-tok")

	commit := func(entryJSON string) error {
		_, err := client.CommitConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.CommitConversationFrameRequest{
			SessionId: "sess-unknown",
			Frame: &compassv1internal.AgentFrame{Frame: &compassv1internal.AgentFrame_TranscriptEntry{
				TranscriptEntry: &compassv1internal.TranscriptEntry{EntryJson: entryJSON},
			}},
		}))
		return err
	}

	if err := commit(strings.Repeat("A", runnerMaxReadBytes)); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("over-cap request: code = %v (err %v), want ResourceExhausted", connect.CodeOf(err), err)
	}
	// A full agent message reaches the handler, which rejects the unknown session
	// on its own terms; anything but ResourceExhausted means the cap let it in.
	if err := commit(strings.Repeat("A", agentmsg.MaxBytes)); connect.CodeOf(err) == connect.CodeResourceExhausted {
		t.Fatalf("full-size agent message: code = ResourceExhausted, want it to pass the read cap (err %v)", err)
	}
}
