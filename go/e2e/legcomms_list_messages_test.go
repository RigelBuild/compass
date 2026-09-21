//go:build podman

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

const (
	listMessagesAgent          = "list-messages-agent"
	listMessagesName           = "List Messages Agent"
	listMessagesChannel        = "list-messages-named"
	listMessagesTopic          = "general"
	listMessagesMarker         = "list-messages-resolution"
	listMessagesHomeBody       = "list-messages home-only seed"
	listMessagesNamedBody      = "list-messages named-only seed"
	listMessagesSettleExplicit = "list messages explicit read done"
	listMessagesSettleOmitted  = "list messages omitted read done"
)

// TestCommsListMessagesChannelResolution proves comms_list_messages resolves its
// channel argument: an explicit name reads that channel, an omitted one falls
// back to HOME, and neither leaks the other's messages.
func TestCommsListMessagesChannelResolution(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}
	ctx := context.Background()
	// The positional fallback both activates the canned backend (WithCannedMarkerScript
	// only registers routes) and absorbs the session-start sweep turn, which draws an
	// unmarked request before either trigger.
	f := NewFixture(ctx, t, WithCannedScript(
		CannedText("list messages positional fallback"),
		CannedText("list messages positional fallback"),
		CannedText("list messages positional fallback"),
	), WithCannedMarkerScript(listMessagesMarker,
		CannedToolCall("comms_list_messages", fmt.Sprintf(`{"channel":%q}`, listMessagesChannel)),
		CannedText(listMessagesSettleExplicit),
		CannedToolCall("comms_list_messages", `{}`),
		CannedText(listMessagesSettleOmitted),
	))

	agentID, err := f.CreateAgent(ctx, listMessagesAgent, listMessagesName)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	// Seeding happens before the first Provision, so no placement exists yet and
	// the posts cannot wake the agent — they stay owed for the session-start sweep.
	st, err := store.Open(ctx, f.DSN())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	agent, err := adminAgentByHandle(ctx, st, listMessagesAgent)
	if err != nil {
		t.Fatalf("AgentByHandle: %v", err)
	}
	home := string(agent.Agent.HomeChannelID)
	named, err := f.CreateChannel(ctx, agentID, listMessagesChannel, true)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, err := f.PostMessage(ctx, home, listMessagesTopic, listMessagesHomeBody); err != nil {
		t.Fatalf("PostMessage(home seed): %v", err)
	}
	if _, err := f.PostMessage(ctx, named, listMessagesTopic, listMessagesNamedBody); err != nil {
		t.Fatalf("PostMessage(named seed): %v", err)
	}
	// The explicit assertion rests on the named seed being reachable ONLY through the
	// tool result: in the sweep set it would also arrive as a delivery and the check
	// would go vacuous.
	inSweep, err := st.InSweepSet(ctx, agent.ID, store.ChannelID(named))
	if err != nil {
		t.Fatalf("InSweepSet(named): %v", err)
	}
	if inSweep {
		t.Fatal("named channel is in the agent's sweep set, so its seed would be delivered rather than read")
	}

	container, err := f.Provision(ctx, agentID, "list-messages-provision")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() { _ = f.RemoveWorkspace(ctx, container, "list-messages-teardown") })
	sessionID, err := f.StartSession(ctx, container)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	tail, err := f.OpenSessionTail(ctx, sessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail: %v", err)
	}
	defer tail.Close()

	if _, err := f.PostMessage(ctx, home, listMessagesTopic, listMessagesMarker+": run explicit read"); err != nil {
		t.Fatalf("PostMessage(explicit trigger): %v", err)
	}
	if err := f.AwaitTurnSettled(ctx, tail); err != nil {
		t.Fatalf("AwaitTurnSettled(explicit): %v", err)
	}
	if _, err := f.awaitTranscriptPersisted(ctx, st, sessionID, listMessagesSettleExplicit); err != nil {
		t.Fatalf("awaitTranscriptPersisted (explicit): %v", err)
	}
	if _, err := f.PostMessage(ctx, home, listMessagesTopic, listMessagesMarker+": run omitted read"); err != nil {
		t.Fatalf("PostMessage(omitted trigger): %v", err)
	}
	if err := f.AwaitTurnSettled(ctx, tail); err != nil {
		t.Fatalf("AwaitTurnSettled(omitted): %v", err)
	}
	// Each barrier waits on its OWN turn's closing text, the last entry that turn
	// writes, so the tool result ahead of it is already committed.
	transcript, err := f.awaitTranscriptPersisted(ctx, st, sessionID, listMessagesSettleOmitted)
	if err != nil {
		t.Fatalf("awaitTranscriptPersisted: %v", err)
	}

	// Checkpoint entries carry the whole session body, so they alias every turn into
	// one string and match any probe. Only deltas can scope an assertion to one turn.
	var explicit, omitted []string
	for _, entry := range transcript {
		if entry.Checkpoint {
			continue
		}
		if !strings.Contains(entry.EntryJSON, "comms_list_messages") {
			continue
		}
		if strings.Contains(entry.EntryJSON, listMessagesNamedBody) {
			explicit = append(explicit, entry.EntryJSON)
		}
		if strings.Contains(entry.EntryJSON, listMessagesHomeBody) {
			omitted = append(omitted, entry.EntryJSON)
		}
	}
	if len(explicit) != 1 {
		t.Fatalf("matched %d delta entries carrying the named seed beside a comms_list_messages call, want exactly 1: the explicit read either resolved to another channel or never committed", len(explicit))
	}
	if strings.Contains(explicit[0], listMessagesHomeBody) {
		t.Fatal("explicit channel result includes the home-only seed")
	}
	if len(omitted) != 1 {
		t.Fatalf("matched %d delta entries carrying the home seed beside a comms_list_messages call, want exactly 1: the omitted read either resolved to another channel or never committed", len(omitted))
	}
	if strings.Contains(omitted[0], listMessagesNamedBody) {
		t.Fatal("omitted-channel result includes the named-channel seed")
	}
}
