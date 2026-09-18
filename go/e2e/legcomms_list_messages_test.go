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
	listMessagesAgent     = "list-messages-agent"
	listMessagesName      = "List Messages Agent"
	listMessagesChannel   = "list-messages-named"
	listMessagesTopic     = "general"
	listMessagesMarker    = "list-messages-resolution"
	listMessagesHomeBody  = "list-messages home-only seed"
	listMessagesNamedBody = "list-messages named-only seed"
)

func TestCommsListMessagesChannelResolution(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}
	ctx := context.Background()
	const settle = "list messages standing by"
	f := NewFixture(ctx, t, WithCannedScript(CannedText("list messages positional fallback")), WithCannedMarkerScript(listMessagesMarker,
		CannedToolCall("comms_list_messages", fmt.Sprintf(`{"channel":%q}`, listMessagesChannel)),
		CannedText(settle),
		CannedToolCall("comms_list_messages", `{}`),
		CannedText(settle),
	))

	agentID, err := f.CreateAgent(ctx, listMessagesAgent, listMessagesName)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	firstContainer, err := f.Provision(ctx, agentID, "list-messages-seed-provision")
	if err != nil {
		t.Fatalf("Provision(seed): %v", err)
	}
	if err := f.RemoveWorkspace(ctx, firstContainer, "list-messages-seed-teardown"); err != nil {
		t.Fatalf("RemoveWorkspace(seed): %v", err)
	}
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
	if _, err := f.awaitTranscriptPersisted(ctx, st, sessionID, listMessagesNamedBody); err != nil {
		t.Fatalf("awaitTranscriptPersisted (explicit): %v", err)
	}
	if _, err := f.PostMessage(ctx, home, listMessagesTopic, listMessagesMarker+": run omitted read"); err != nil {
		t.Fatalf("PostMessage(omitted trigger): %v", err)
	}
	if err := f.AwaitTurnSettled(ctx, tail); err != nil {
		t.Fatalf("AwaitTurnSettled(omitted): %v", err)
	}
	transcript, err := f.awaitTranscriptPersisted(ctx, st, sessionID, listMessagesMarker+": run omitted read")
	if err != nil {
		t.Fatalf("awaitTranscriptPersisted: %v", err)
	}

	var explicit, omitted string
	for _, entry := range transcript {
		if strings.Contains(entry.EntryJSON, listMessagesNamedBody) && strings.Contains(entry.EntryJSON, "comms_list_messages") {
			explicit = entry.EntryJSON
		}
		if strings.Contains(entry.EntryJSON, listMessagesHomeBody) && strings.Contains(entry.EntryJSON, "comms_list_messages") {
			omitted = entry.EntryJSON
		}
	}
	if explicit == "" {
		t.Fatal("explicit comms_list_messages result did not reach the durable transcript")
	}
	if strings.Contains(explicit, listMessagesHomeBody) {
		t.Fatal("explicit channel result includes the home-only seed")
	}
	if omitted == "" {
		t.Fatal("omitted-channel comms_list_messages result did not reach the durable transcript")
	}
	if strings.Contains(omitted, listMessagesNamedBody) {
		t.Fatal("omitted-channel result includes the named-channel seed")
	}
}
