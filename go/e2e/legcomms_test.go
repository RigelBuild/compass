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
// guard first, context.Background() as the test root, NewFixture(ctx, t,
// WithCannedScript(...)), a container-reaping t.Cleanup registered before
// StartSession, store-side reads via store.Open(ctx, f.DSN()), tail-before-post
// ordering, and a subscribe-before-post live-fan observation.
//
// It is PRESENT-BUT-SKIPPED on a container-less sandbox, exactly as the sibling
// leg is: without rootless podman able to run compass-agent:latest the full
// agent lane cannot come up, so podmanUsable() SKIPs it here. Every wait it
// makes is ctx-bounded (AwaitTurnSettled / AwaitDelivery) — no sleeps, no
// polling, no retries.
func TestCommsPostMessageThroughAgentLoop(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into NewFixture + every primitive

	// The distinctive post the canned turn will make through the agent loop. A
	// unique topic + body so the fan-out assertion selects THIS message off the
	// comms bus and cannot collide with the human trigger post.
	const postTopic = "e2e-comms-leg"
	const postBody = "comms leg: posting from the agent loop"
	// The poster's handle. Its home channel is minted under the same name
	// (store/accounts.go:337 names the home channel for the agent's handle), so
	// this const doubles as the channel NAME the scripted post must target.
	const posterHandle = "comms-leg-poster"
	// The comms tool's arguments, serialized JSON (the OpenAI tool-call
	// contract). Built from the consts above so the asserted values cannot drift
	// from what the script issues. Field names are the postParameters wire schema
	// (comms.ts): text, topic, channel, create_topic. As of the peer-DM cutover
	// (record R2/R5) post has NO home default — `channel` is REQUIRED and carries
	// the target channel NAME (here the poster's own home channel, whose name is
	// its handle), and a name-miss topic needs create_topic:true (postTopic names
	// no existing topic on the freshly-minted home channel, so it must mint).
	postArgsJSON := fmt.Sprintf(
		`{"text":%q,"topic":%q,"channel":%q,"create_topic":true}`,
		postBody, postTopic, posterHandle,
	)
	// The assistant text the closing turn settles on after the tool result
	// returns — a clean text settle, mirroring the sibling's settleReply. Unlike
	// the sibling, it is deliberately NOT asserted in the transcript: the comms
	// fan-out + author assertion below is a strictly stronger proof that the tool
	// executed, so it is the proof of record here. This const only gives turn 1
	// something to settle on.
	const settleReply = "posted, standing by"

	// A 2-turn script: turn 0 issues the comms_post_message tool-call (so the
	// agent loop runs the real PostMessage); turn 1 is a clean text settle after
	// the tool result returns.
	f := NewFixture(ctx, t,
		WithCannedScript(
			CannedToolCall("comms_post_message", postArgsJSON),
			CannedText(settleReply),
		),
	)

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
	// Reap the poster's container (see TestLegThreeFourSpawnAndMessaging): the
	// reparented rootless conmon outlives stack Down, so an explicit
	// RemoveWorkspace is the only reliable reap. Registered before StartSession so
	// a later failure still tears it down. Best-effort teardown, not an assertion,
	// so the error is deliberately discarded.
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

	// Open the poster session tail BEFORE the post drives the turn: the frame
	// stream is live-fan with no replay ring, but OpenSessionTail returns on the
	// leading registration-ack, so the subscription is provably live before the
	// post — a server-guaranteed happens-before that keeps a fast canned turn's
	// WORKING/READY edges from being lost into the post→subscribe gap.
	tail, err := f.OpenSessionTail(ctx, sessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail (poster): %v", err)
	}
	defer tail.Close()

	// Resolve the poster to get its home channel id (the channel the trigger post
	// lands on and the channel the agent's own post — targeting its home channel
	// by name — fans onto).
	poster, err := adminAgentByHandle(ctx, st, posterHandle)
	if err != nil {
		t.Fatalf("AgentByHandle(poster): %v", err)
	}

	// Open one comms subscription BEFORE the trigger post so the agent's own
	// posted message is observed on the live fan. sinceSeq 0 snapshots then tails.
	sub, err := f.SubscribeComms(ctx, 0)
	if err != nil {
		t.Fatalf("SubscribeComms: %v", err)
	}
	defer sub.Close()

	// Post to the poster's home channel: this post lands on the already-live
	// session and is delivered via the live fan-out, which fires the poster's
	// first turn (the one that issues the comms_post_message tool-call) — the turn
	// AwaitTurnSettled waits on. Must precede the settle wait.
	if _, err := f.PostMessage(ctx, string(poster.Agent.HomeChannelID), "general", "post a message and stand by"); err != nil {
		t.Fatalf("PostMessage(home trigger): %v", err)
	}

	// Event-gated settle on the poster's already-open tail: the scripted
	// comms_post_message tool-call executes and the closing text turn settles — no
	// sleeps.
	if err := f.AwaitTurnSettled(ctx, tail); err != nil {
		t.Fatalf("AwaitTurnSettled (poster): %v", err)
	}

	// The observable server-side effect: the agent's scripted comms_post_message
	// fanned out on the comms bus. Match on the distinctive body substring to
	// select the agent's own post (not the human trigger), then assert its author
	// is the poster agent — the two together prove the tool ran through the loop
	// AND the post was authored by the agent, not the human.
	posted, err := f.AwaitDelivery(ctx, sub, func(m *compassv1.Message) bool {
		// mutation: catches the comms tool not running at all (no agent-authored
		// post ever fans out, so AwaitDelivery times out) — selects the agent's
		// post by its distinctive scripted body.
		return firstBlockText(m) == postBody
	})
	if err != nil {
		t.Fatalf("AwaitDelivery: %v — the scripted comms_post_message did not fan out on the bus", err)
	}
	// mutation: catches a regression that posts the agent's message under the
	// wrong author (e.g. the human trigger's identity, or an empty author) —
	// GetAuthorAccountId must equal the poster agent's account id.
	if got := posted.GetAuthorAccountId(); got != posterID {
		t.Fatalf("posted message author = %q, want the poster agent's account id %q (the agent posted it, not the human trigger)", got, posterID)
	}
	// The scripted post names its target channel explicitly (the poster's own
	// home channel by name — post has no home default post-cutover) and mints the
	// topic via create_topic. Neither the resolved channel nor the topic NAME is
	// separately asserted off the wire Message: it carries no channel container
	// (F9 removed it), and GetTopicId() is a server-minted id, not the scripted
	// topic NAME, so there is nothing on the wire to compare against
	// poster.Agent.HomeChannelID or postTopic. Body + author is the correct
	// assertion ceiling here.
}
