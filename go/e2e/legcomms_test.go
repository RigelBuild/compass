//go:build podman

package e2e

import (
	"context"
	"fmt"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// TestCommsPostMessageThroughAgentLoop is the comms analog of the legs-3/4
// scenario over the real stack: a canned turn issues the native comms tool
// (comms_post_message) so the agent's loop runs the real PostMessage against the
// live Server, then settles on a text reply. It proves that the native comms
// tool is registered in the agent's SDK, that a scripted tool-call reaches the
// server, and that the resulting message fans out on the comms bus authored by
// the agent itself (not the human trigger). Modeled EXACTLY on
// TestLegThreeFourSpawnAndMessaging: //go:build podman, the podmanUsable() skip
// guard first, context.Background() as the test root, sharedFixture(t) with
// TestMain-registered canned route, a container-reaping t.Cleanup
// registered before StartSession, store-side reads via store.Open(ctx, f.DSN()), tail-before-post
// ordering, and a subscribe-before-post live-fan observation.
//
// It is PRESENT-BUT-SKIPPED on a container-less sandbox, exactly as the sibling
// leg is: without rootless podman able to run compass-agent:latest the full
// agent lane cannot come up, so podmanUsable() SKIPs it here. Every wait it
// makes is ctx-bounded (AwaitTurnSettled / AwaitDelivery) — no sleeps, no
// polling, no retries.
func TestCommsPostMessageThroughAgentLoop(t *testing.T) {
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into every primitive

	f := sharedFixture(t)

	// The poster agent: created, provisioned, and started exactly as the
	// sibling's spawner. Its turn issues the comms post.
	posterID, err := f.CreateAgent(ctx, posterHandle, "Comms Leg Poster")
	if err != nil {
		t.Fatalf("CreateAgent (poster): %v", err)
	}

	posterContainer, err := f.Provision(ctx, posterID, "comms-leg-poster-provision")
	if err != nil {
		t.Fatalf("Provision (poster): %v", err)
	}
	// Reap the container at test end: rootless conmon outlives stack Down. Register
	// cleanup before StartSession so failures still remove it; cleanup is best-effort.
	t.Cleanup(func() {
		_ = f.RemoveWorkspace(ctx, posterContainer, "comms-leg-poster-teardown")
	})

	sessionID, err := f.StartSession(ctx, posterContainer)
	if err != nil {
		t.Fatalf("StartSession (poster): %v", err)
	}

	st, err := store.Open(ctx, f.DSN())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	// Open the live tail before the trigger post. Its registration ack proves the
	// subscriber is ready, so fast turn-state frames cannot fall into a gap.
	tail, err := f.OpenSessionTail(ctx, sessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail (poster): %v", err)
	}
	defer tail.Close()

	// Resolve the poster's home channel for both the trigger and agent-authored post.
	poster, err := adminAgentByHandle(ctx, st, posterHandle)
	if err != nil {
		t.Fatalf("AgentByHandle(poster): %v", err)
	}

	// Subscribe before the trigger so the agent's post is observed on the live tail.
	sub, err := f.SubscribeComms(ctx, 0)
	if err != nil {
		t.Fatalf("SubscribeComms: %v", err)
	}
	defer sub.Close()

	// The trigger reaches the already-live session and starts its first scripted
	// turn; post before waiting for settle.
	if _, err := f.PostMessage(ctx, string(poster.Agent.HomeChannelID), "general", "post a message and stand by "+commsToolMarker); err != nil {
		t.Fatalf("PostMessage(home trigger): %v", err)
	}

	// Wait for the scripted tool call and closing text turn on the open tail.
	if err := f.AwaitTurnSettled(ctx, tail); err != nil {
		t.Fatalf("AwaitTurnSettled (poster): %v", err)
	}

	// Match the agent-authored post by its distinctive body, then verify its author.
	posted, err := f.AwaitDelivery(ctx, sub, func(m *compassv1.Message) bool {
		// No delivery means the tool did not publish its scripted post.
		return firstBlockText(m) == postBody
	})
	if err != nil {
		t.Fatalf("AwaitDelivery: %v — the scripted comms_post_message did not fan out on the bus", err)
	}
	// Verify the post belongs to the agent, not the human trigger.
	if got := posted.GetAuthorAccountId(); got != posterID {
		t.Fatalf("posted message author = %q, want the poster agent's account id %q (the agent posted it, not the human trigger)", got, posterID)
	}
	// The wire message has no channel container or topic name, so body and author
	// are the observable assertions for this post.
}

// The distinctive post the canned turn will make through the agent loop. A
// unique topic + body so the fan-out assertion selects THIS message off the
// comms bus and cannot collide with the human trigger post.
const postTopic = "e2e-comms-leg"
const postBody = "comms leg: posting from the agent loop"

// The handle also names the agent's home channel, which the scripted post targets.
const posterHandle = "comms-leg-poster"

// postArgsJSON follows the tool's JSON schema. The target channel is required,
// and this new topic needs create_topic:true.
var postArgsJSON = fmt.Sprintf(
	`{"text":%q,"topic":%q,"channel":%q,"create_topic":true}`,
	postBody, postTopic, posterHandle,
)

// This closing text only settles the turn; the fan-out and author assertions
// prove the tool ran.
const settleReply = "posted, standing by"

// commsToolMarker routes this leg's prompt to its 2-turn script on the shared
// canned stub: turn 0 issues the comms_post_message tool-call (so the agent loop
// runs the real PostMessage); turn 1 is a clean text settle after the tool
// result returns.
const commsToolMarker = "e2e-route-comms-post-tool"

func registerCommsToolFixtureOptions() {
	registerSharedFixtureOption(
		WithCannedMarkerScript(commsToolMarker,
			CannedToolCall("comms_post_message", postArgsJSON),
			CannedText(settleReply),
		),
	)
}
