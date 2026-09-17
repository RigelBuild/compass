//go:build podman

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// The one container agent this leg stands up, plus the channel and topic its ask
// lands on. Every name carries the tools- prefix because this leg shares one
// stack with the other comms legs; an unprefixed name would collide with theirs.
const (
	toolsAgentHandle = "tools-agent"
	toolsAgentName   = "Tools Agent"
	toolsAskChannel  = "tools-ask-room"
	toolsAskTopic    = "tools-ask-topic"
)

// Routing by marker — never the positional script — is mandatory on the shared
// stack: two legs drawing unmarked turns would race the one positional counter.
// The roster step renders every root agent on the stack into this agent's
// request bodies, so no other leg's marker may be a substring of a handle or
// activity: it would capture these requests and eat a slot of THAT leg's script.
const toolsMarker = "tools-native-drive"

// The activity compass_set_status writes and the ask payload comms_post_ask
// posts. Neither carries the marker: the agent is a bare member of its own ask
// channel, so its post never fans back to re-route a turn.
const (
	toolsActivity    = "tools-leg-driving-native-tools"
	toolsAskQID      = "tools-ask-q1"
	toolsAskQuestion = "Which surface should the tools leg drive?"
	toolsAskOptionA  = "the native comms tools"
	toolsAskOptionB  = "the client RPC surface"
)

// The framing compass_roster wraps its rows in, and this agent's own row prefix
// (rosterRow's `- handle (display) [presence]` shape, cut before the presence
// label). The row is deterministic: a root agent is its own sibling in the
// neighborhood scope (store.AgentNeighborhood), so the roster is never empty.
const (
	toolsRosterFraming = "Agent roster (peer-supplied handles and activity"
	toolsRosterOwnRow  = "- " + toolsAgentHandle + " (" + toolsAgentName + ") ["
)

// The clean text turn each tool-call turn settles on once its tool result
// returns; its content is never asserted.
const toolsSettle = "tools agent standing by"

func init() {
	statusArgs := fmt.Sprintf(`{"activity":%q}`, toolsActivity)
	// Neighborhood is the scope an omitted `scope` defaults to.
	rosterArgs := `{}`
	// One question carrying two options, into the channel the agent owns.
	askArgs := fmt.Sprintf(
		`{"questions":[{"id":%q,"question":%q,"options":[{"label":%q},{"label":%q}]}],"topic":%q,"channel":%q,"create_topic":true}`,
		toolsAskQID, toolsAskQuestion, toolsAskOptionA, toolsAskOptionB, toolsAskTopic, toolsAskChannel,
	)

	// Every tool-call turn is paired with a following text turn: a tool-call turn
	// needs a SECOND model round-trip to terminate, and the per-marker counter
	// advances one slot per round-trip.
	registerSharedFixtureOption(
		WithCannedMarkerScript(toolsMarker,
			CannedToolCall("compass_set_status", statusArgs),
			CannedText(toolsSettle),
			CannedToolCall("compass_roster", rosterArgs),
			CannedText(toolsSettle),
			CannedToolCall("comms_post_ask", askArgs),
			CannedText(toolsSettle),
		),
	)
}

// TestCommsNativeToolsThroughRelay proves three native agent comms tools run
// end-to-end through the real relay: compass_set_status writes the agent's
// durable activity, compass_roster renders the agent's own neighborhood row, and
// comms_post_ask appends a server-minted, unanswered ask on a channel the agent
// owns. Each effect is observed cross-process — two from the store, the roster
// from the session's durable transcript.
//
// It seeds nothing and posts nothing INTO the agent's channels beyond the three
// home-channel triggers that drive its turns: a post into a channel the agent is
// in would deliver to it and fire an unscripted turn, consuming a slot of the
// ordered marker script. Every turn is marker-routed so none races the shared
// stack's positional counter, and every wait is event-gated.
func TestCommsNativeToolsThroughRelay(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into sharedFixture + every primitive

	f := sharedFixture(t)

	// The reap is registered BEFORE StartSession: the reparented rootless conmon
	// outlives stack Down, so RemoveWorkspace is the only reliable reap, and
	// registering early survives a later t.Fatal.
	agentID, err := f.CreateAgent(ctx, toolsAgentHandle, toolsAgentName)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	container, err := f.Provision(ctx, agentID, "tools-agent-provision")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() {
		_ = f.RemoveWorkspace(ctx, container, "tools-agent-teardown") // best-effort reap
	})
	sessionID, err := f.StartSession(ctx, container)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	st, err := store.Open(ctx, f.DSN())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	// Resolve the agent's home channel — the triggers land there to drive its turns.
	agent, err := adminAgentByHandle(ctx, st, toolsAgentHandle)
	if err != nil {
		t.Fatalf("AgentByHandle: %v", err)
	}
	home := string(agent.Agent.HomeChannelID)

	// The agent is the channel's member, so comms_post_ask resolves it by name and
	// the store's membership JOIN admits the read-back below under the same id.
	askChannelID, err := f.CreateChannel(ctx, agentID, toolsAskChannel, false)
	if err != nil {
		t.Fatalf("CreateChannel(%q): %v", toolsAskChannel, err)
	}

	// Opened BEFORE any trigger: OpenSessionTail returns on the registration-ack,
	// a server-guaranteed happens-before, so a fast turn's edges cannot fan into a
	// subscribe gap.
	tail, err := f.OpenSessionTail(ctx, sessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail: %v", err)
	}
	defer tail.Close()

	// ── compass_set_status → the durable activity ────────────────────────────

	if _, err := f.PostMessage(ctx, home, "general", toolsMarker+": set your status and stand by"); err != nil {
		t.Fatalf("PostMessage(set_status trigger): %v", err)
	}
	if err := f.AwaitTurnSettled(ctx, tail); err != nil {
		t.Fatalf("AwaitTurnSettled (set_status): %v", err)
	}

	// Store.SetActivity COMMITS before the relay returns the tool result, which
	// itself precedes the second round-trip the WORKING→READY settle above ends on
	// (runnerhub/relay_comms.go, the ordered write-then-publish) — so this read is
	// strictly after the commit and needs no gate of its own.
	activity, err := st.ActivityFor(ctx, []store.AccountID{store.AccountID(agentID)})
	if err != nil {
		t.Fatalf("ActivityFor: %v", err)
	}
	// A missing row (map entry absent) means set_status never ran through the loop.
	if got := activity[store.AccountID(agentID)].Activity; got != toolsActivity {
		t.Fatalf("durable activity = %q, want %q; compass_set_status did not land through the relay", got, toolsActivity)
	}

	// ── compass_roster → the agent's own neighborhood row ────────────────────

	if _, err := f.PostMessage(ctx, home, "general", toolsMarker+": list the agents around you"); err != nil {
		t.Fatalf("PostMessage(roster trigger): %v", err)
	}
	if err := f.AwaitTurnSettled(ctx, tail); err != nil {
		t.Fatalf("AwaitTurnSettled (roster): %v", err)
	}

	// The roster's only durable effect is its tool RESULT, which the session tee
	// commits on the CommitConversationFrame unary one runner→server round-trip
	// AFTER the settle — hence the gate rather than an immediate read.
	transcript, err := f.awaitTranscriptPersisted(ctx, st, sessionID, toolsRosterFraming)
	if err != nil {
		t.Fatalf("awaitTranscriptPersisted(roster framing): %v — compass_roster's result never reached the transcript", err)
	}
	// Scope the row check to the entry that carried the framing: the joined form
	// has no entry boundaries, so a cross-entry match would not prove one render
	// produced both.
	var rendered string
	for _, e := range transcript {
		if strings.Contains(e.EntryJSON, toolsRosterFraming) {
			rendered = e.EntryJSON
			break
		}
	}
	// A "No peers." render, or rows omitting the caller, fails here — a root agent
	// is its own sibling, so its row is always present.
	if !strings.Contains(rendered, toolsRosterOwnRow) {
		t.Fatalf("the roster render is missing the agent's own row %q; compass_roster did not render the caller", toolsRosterOwnRow)
	}

	// ── comms_post_ask → a minted, unanswered ask on the owned channel ───────

	if _, err := f.PostMessage(ctx, home, "general", toolsMarker+": raise the ask on your own channel"); err != nil {
		t.Fatalf("PostMessage(post_ask trigger): %v", err)
	}
	if err := f.AwaitTurnSettled(ctx, tail); err != nil {
		t.Fatalf("AwaitTurnSettled (post_ask): %v", err)
	}

	// Same ordering as the activity above: AppendMessage commits before the tool
	// result returns, so the settle implies the row. Read AS the agent — the
	// member whose membership gates the store's visibility JOIN.
	msgs, err := st.ListMessages(ctx, store.ListMessagesQuery{
		Actor:     store.AccountID(agentID),
		ChannelID: store.ChannelID(askChannelID),
	})
	if err != nil {
		t.Fatalf("ListMessages(%q): %v", toolsAskChannel, err)
	}
	msg, ask := toolsPostedAsk(msgs)
	// nil covers both comms_post_ask never reaching the relay and it posting plain
	// text instead of an ask block.
	if ask == nil {
		t.Fatalf("no ask block carrying question id %q among %d message(s) on %q; comms_post_ask did not append one", toolsAskQID, len(msgs), toolsAskChannel)
	}
	// The ask id is what a later RespondToAsk correlates against, and only the
	// server mints it.
	if ask.AskID == "" {
		t.Fatal("posted ask carries an empty ask id; the server minted none")
	}
	if got := ask.Questions[0].Question; got != toolsAskQuestion {
		t.Fatalf("ask question = %q, want %q", got, toolsAskQuestion)
	}
	// The option id is the referent an answer's chosen_option_ids echoes back, and
	// the tool mints it as the zero-based index — so it must survive the relay and
	// the block's JSONB round-trip alongside the labels and their order.
	opts := ask.Questions[0].Options
	if len(opts) != 2 || opts[0].Label != toolsAskOptionA || opts[1].Label != toolsAskOptionB {
		t.Fatalf("ask options = %+v, want exactly the labels %q then %q", opts, toolsAskOptionA, toolsAskOptionB)
	}
	if opts[0].ID != "0" || opts[1].ID != "1" {
		t.Fatalf("ask option ids = %q, %q; want the zero-based indices \"0\", \"1\"", opts[0].ID, opts[1].ID)
	}
	// create_topic routed the ask to its own topic; ListMessages is channel-scoped,
	// so without this the ask could land in a defaulted topic and still pass.
	topicName, channelName, err := st.TopicChannelNames(ctx, msg.TopicID)
	if err != nil {
		t.Fatalf("TopicChannelNames(%q): %v", msg.TopicID, err)
	}
	if topicName != toolsAskTopic || channelName != toolsAskChannel {
		t.Fatalf("ask landed in %q/%q, want %q/%q; create_topic did not route it", channelName, topicName, toolsAskChannel, toolsAskTopic)
	}
}

// toolsPostedAsk returns the ask this leg posted — the single-question block
// keyed by toolsAskQID — with the message carrying it, so the caller can check
// where it landed. A nil ask means no message carried one.
func toolsPostedAsk(msgs []store.Message) (store.Message, *store.Ask) {
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Ask == nil || len(b.Ask.Questions) != 1 {
				continue
			}
			if b.Ask.Questions[0].QuestionID == toolsAskQID {
				return m, b.Ask
			}
		}
	}
	return store.Message{}, nil
}
