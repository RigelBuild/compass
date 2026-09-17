//go:build podman

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// The one container agent this leg stands up, plus the channel and topic its ask
// and the operator's answer land on. Every name carries the ask- prefix because
// this leg shares one stack with the other comms legs; an unprefixed name would
// collide with theirs.
const (
	askAgentHandle = "ask-agent"
	askAgentName   = "Ask Agent"
	askChannel     = "ask-round-trip-room"
	askTopic       = "ask-round-trip-topic"
)

// Marker routing keeps this leg's turns off the shared positional counter. The
// marker and the other legs' markers must not be substrings of each other's
// handles or payloads, or one route would capture the other's requests.
const askMarker = "ask-round-trip-drive"

// The ask payload comms_post_ask posts. It carries no marker: the answer the
// operator posts back snapshots these questions and options, so a marker here
// would ride the answer the agent receives.
const (
	askQID      = "ask-q1"
	askQuestion = "Which surface answers this ask?"
	askOptionA  = "the operator over RespondToAsk"
	askOptionB  = "the agent itself"
)

// The option id the operator chooses, and the label it resolves to. The tool
// mints ids as zero-based indices, so "1" is the SECOND option — a defaulted
// first choice cannot pass for it.
const (
	askChosenOptionID = "1"
	askChosenLabel    = askOptionB
)

// The framing the agent's answer render wraps its questions in
// (formatAskAnswerForPrompt). Unique to the answer lane: the question text also
// reaches the transcript via the comms_post_ask arguments, so gating on the
// question alone would settle on the ask turn.
const askAnswerFraming = "Answer received for ask"

// The clean text turn each turn settles on once its work returns; its content is
// never asserted.
const askSettle = "ask agent standing by"

func init() {
	askArgs := fmt.Sprintf(
		`{"questions":[{"id":%q,"question":%q,"options":[{"label":%q},{"label":%q}]}],"topic":%q,"channel":%q,"create_topic":true}`,
		askQID, askQuestion, askOptionA, askOptionB, askTopic, askChannel,
	)

	// Three round-trips: the tool call, the text turn that settles it, then the
	// turn the operator's answer fires. The terminal element clamps, so a
	// redelivered answer settles rather than exhausting the route.
	registerSharedFixtureOption(
		WithCannedMarkerScript(askMarker,
			CannedToolCall("comms_post_ask", askArgs),
			CannedText(askSettle),
			CannedText(askSettle),
		),
	)
}

// TestCommsAskRoundTripThroughAgentLoop closes the ask round-trip across all
// three actors: the agent raises an ask through comms_post_ask, the operator
// answers over CommsService.RespondToAsk — the RPC no agent tool can reach — and
// the agent receives the answer on a later turn, because RespondToAsk fans it out
// on the ordinary message rail rather than a bespoke ask wake.
//
// The durable answered state is read from the STORE, never a live tail. Delivery
// evidence is independent: the DELIVER injection the agent's own session
// dispatched, and the answer's render in its durable transcript.
func TestCommsAskRoundTripThroughAgentLoop(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into sharedFixture + every primitive

	f := sharedFixture(t)

	// The reap is registered BEFORE StartSession: the reparented rootless conmon
	// outlives stack Down, so RemoveWorkspace is the only reliable reap, and
	// registering early survives a later t.Fatal.
	agentID, err := f.CreateAgent(ctx, askAgentHandle, askAgentName)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	container, err := f.Provision(ctx, agentID, "ask-agent-provision")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() {
		_ = f.RemoveWorkspace(ctx, container, "ask-agent-teardown") // best-effort reap
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

	// Resolve the agent's home channel — the trigger lands there to drive its turn.
	agent, err := adminAgentByHandle(ctx, st, askAgentHandle)
	if err != nil {
		t.Fatalf("AgentByHandle: %v", err)
	}
	home := string(agent.Agent.HomeChannelID)

	// The agent joins SUBSCRIBED so it is in the ask channel's deliver set, which
	// is what carries the answer back to it. Its own ask never fans back: the
	// deliver set excludes the author.
	channelID, err := f.CreateChannel(ctx, agentID, askChannel, false)
	if err != nil {
		t.Fatalf("CreateChannel(%q): %v", askChannel, err)
	}
	if err := f.SubscribeMember(ctx, channelID, askAgentHandle); err != nil {
		t.Fatalf("SubscribeMember(agent → ask channel): %v", err)
	}

	// Opened BEFORE any trigger: OpenSessionTail returns on the registration-ack,
	// a server-guaranteed happens-before, so neither the ask turn's settle edge
	// nor the answer's injection can fan into a subscribe gap.
	tail, err := f.OpenSessionTail(ctx, sessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail: %v", err)
	}
	defer tail.Close()

	// ── Phase 1: the agent raises the ask ────────────────────────────────────

	if _, err := f.PostMessage(ctx, home, "general", askMarker+": raise the ask on your own channel"); err != nil {
		t.Fatalf("PostMessage(post_ask trigger): %v", err)
	}
	if err := f.AwaitTurnSettled(ctx, tail); err != nil {
		t.Fatalf("AwaitTurnSettled (post_ask): %v", err)
	}

	// AppendMessage commits before the relay returns the tool result, which itself
	// precedes the second round-trip the WORKING→READY settle above ends on — so
	// this read is strictly after the commit and needs no gate of its own. Read AS
	// the agent, the member whose membership gates the store's visibility JOIN.
	raisedMsg, raised := askPostedAsk(askChannelMessages(ctx, t, st, agentID, channelID))
	// nil covers both comms_post_ask never reaching the relay and it posting plain
	// text instead of an ask block.
	if raised == nil {
		t.Fatalf("no ask block carrying question id %q on %q; comms_post_ask did not append one", askQID, askChannel)
	}
	// The ask id is what RespondToAsk correlates against, and only the server
	// mints it — an empty one leaves the operator nothing to address.
	if raised.AskID == "" {
		t.Fatal("raised ask carries an empty ask id; the server minted none")
	}
	// Pending at birth: without this the answered flip below could pass against an
	// ask that was already answered, proving nothing about RespondToAsk.
	if raised.Answered {
		t.Fatal("raised ask is already answered before any RespondToAsk; the pending pre-condition the round-trip rests on does not hold")
	}
	// The chosen id must be one the question actually offered, else AnswerAsk
	// rejects the answer as an unoffered option and the phase below fails for that
	// reason rather than the one under test.
	if !askOffersOption(raised.Questions[0].Options, askChosenOptionID) {
		t.Fatalf("raised ask offers options %+v, none carrying the operator's chosen id %q", raised.Questions[0].Options, askChosenOptionID)
	}

	// ── Phase 2: the operator answers over RespondToAsk ──────────────────────

	// Answered on the ADMIN bearer: RespondToAsk is the operator's surface and no
	// agent tool reaches it, so the fixture's own client IS the operator here.
	if err := f.RespondToAsk(ctx, raised.AskID, askQID, askChosenOptionID); err != nil {
		t.Fatalf("RespondToAsk(%q): %v", raised.AskID, err)
	}

	// Both halves of the answer commit in ONE store transaction, so re-reading the
	// channel after the RPC returns sees the flip AND the answer message.
	answered := askChannelMessages(ctx, t, st, agentID, channelID)
	_, flipped := askPostedAsk(answered)
	if flipped == nil {
		t.Fatalf("the raised ask vanished from %q after RespondToAsk; want it preserved and marked answered", askChannel)
	}
	// Answered is the only reliable answered-signal: a fully-skipped ask leaves
	// every question's answer fields empty, indistinguishable from pending.
	if !flipped.Answered {
		t.Fatal("the ask is still unanswered in the store after RespondToAsk returned")
	}
	// The chosen id is the durable record of WHICH option the operator picked; a
	// bare Answered flip would pass with the choice dropped.
	if got := flipped.Questions[0].ChosenOptionIDs; len(got) != 1 || got[0] != askChosenOptionID {
		t.Fatalf("answered question chose %v, want exactly the operator's option id %q", got, askChosenOptionID)
	}

	// The answer landed as its OWN message, not as a mutation of the ask: that is
	// what puts it on the normal message rail the phase below rides.
	answerMsg, answer := askAnswerBlock(answered)
	if answer == nil {
		t.Fatalf("no ask_answer block on %q; RespondToAsk posted no answer message", askChannel)
	}
	if answerMsg.ID == raisedMsg.ID {
		t.Fatalf("the answer block rides the ask's own message %s; RespondToAsk must post the answer as a distinct message", raisedMsg.ID)
	}
	// The snapshot correlates the answer to the ask it answers, and names the
	// asking agent so the delivery consumer targets it without a lookup.
	if answer.Ask.AskID != raised.AskID {
		t.Fatalf("answer snapshot correlates ask id %q, want the raised %q", answer.Ask.AskID, raised.AskID)
	}
	if answer.AskerAccountID != store.AccountID(agentID) {
		t.Fatalf("answer names asker %q, want the asking agent %q", answer.AskerAccountID, agentID)
	}
	// The operator authored the answer, not the agent: an agent-attributed answer
	// would mean the asymmetry this leg rests on had collapsed.
	if answerMsg.AuthorAccountID == store.AccountID(agentID) {
		t.Fatalf("the answer is authored by the asking agent %q; RespondToAsk must attribute it to the answering operator", agentID)
	}

	// ── Phase 3: the agent receives the answer on a later turn ───────────────

	// The agent's OWN session dispatched a DELIVER for the answer message, which
	// proves it received the answer rather than that the server fanned it out.
	// Read off the tail already open at the ask turn's READY edge.
	if _, err := f.AwaitControlDispatchOn(ctx, tail, func(kind, mid string) bool {
		return mid == string(answerMsg.ID) && strings.Contains(kind, "DELIVER")
	}); err != nil {
		t.Fatalf("AwaitControlDispatchOn(answer DELIVER): %v — the answer never reached the agent's session", err)
	}

	// The DELIVER injection lands at the START of the turn it fires, so settle
	// that turn before reading: transcriptPollBudget is sized for the commit
	// round-trip alone, not for a whole turn.
	if err := f.AwaitTurnSettled(ctx, tail); err != nil {
		t.Fatalf("AwaitTurnSettled (answer turn): %v", err)
	}

	// The turn rendered the answer into the DURABLE transcript, which commits on
	// an independent CommitConversationFrame round-trip after the settle.
	transcript, err := f.awaitTranscriptPersisted(ctx, st, sessionID, askAnswerFraming)
	if err != nil {
		t.Fatalf("awaitTranscriptPersisted(answer framing): %v — the answer never reached the agent's prompt", err)
	}
	// Scope to the entry carrying the framing: the option labels also reach the
	// transcript through the comms_post_ask arguments, so an unscoped match would
	// prove nothing about the answer render.
	var rendered string
	for _, e := range transcript {
		if strings.Contains(e.EntryJSON, askAnswerFraming) {
			rendered = e.EntryJSON
			break
		}
	}
	if rendered == "" {
		t.Fatalf("no single transcript entry carries %q; the framing matched only across entry boundaries", askAnswerFraming)
	}
	// The render resolves the chosen id to its label, and must resolve it to THAT
	// option: asserting presence alone would pass a render listing every option.
	if !strings.Contains(rendered, askChosenLabel) {
		t.Fatalf("the answer render is missing the chosen option label %q; the agent saw the answer without the operator's choice", askChosenLabel)
	}
	if strings.Contains(rendered, askOptionA) {
		t.Fatalf("the answer render carries the unchosen option label %q; the render does not resolve the chosen id to its own option", askOptionA)
	}
}

// RespondToAsk answers one single-question ask over the fixture's admin client —
// the operator identity RespondToAsk attributes the answer to. Answering must
// cover the ask's question set exactly, so this takes one chosen option.
func (f *Fixture) RespondToAsk(ctx context.Context, askID, questionID, chosenOptionID string) error {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	if _, err := f.Comms().RespondToAsk(rctx, connect.NewRequest(&compassv1.RespondToAskRequest{
		AskId: askID,
		Answers: []*compassv1.AskQuestionAnswer{{
			QuestionId:      questionID,
			ChosenOptionIds: []string{chosenOptionID},
		}},
	})); err != nil {
		return fmt.Errorf("RespondToAsk RPC: %w", err)
	}
	return nil
}

// askChannelMessages reads the ask channel AS the agent, the member whose
// membership gates the store's visibility JOIN. Fatal on error: this leg's every
// assertion reads through it, so a failed read makes them all meaningless.
func askChannelMessages(ctx context.Context, t *testing.T, st *store.Store, agentID, channelID string) []store.Message {
	t.Helper()
	msgs, err := st.ListMessages(ctx, store.ListMessagesQuery{
		Actor:     store.AccountID(agentID),
		ChannelID: store.ChannelID(channelID),
	})
	if err != nil {
		t.Fatalf("ListMessages(%q): %v", askChannel, err)
	}
	return msgs
}

// askPostedAsk returns the ask this leg raised — the single-question block keyed
// by askQID — with the message carrying it. A nil ask means no message carried
// one.
func askPostedAsk(msgs []store.Message) (store.Message, *store.Ask) {
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Ask == nil || len(b.Ask.Questions) != 1 {
				continue
			}
			if b.Ask.Questions[0].QuestionID == askQID {
				return m, b.Ask
			}
		}
	}
	return store.Message{}, nil
}

// askAnswerBlock returns the answer to this leg's ask — the ask_answer block
// whose snapshot is keyed by askQID — with the message carrying it, so the
// caller can check it is a message of its own. A nil block means no message
// carried one.
func askAnswerBlock(msgs []store.Message) (store.Message, *store.AskAnswerBlock) {
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.AskAnswer == nil || len(b.AskAnswer.Ask.Questions) != 1 {
				continue
			}
			if b.AskAnswer.Ask.Questions[0].QuestionID == askQID {
				return m, b.AskAnswer
			}
		}
	}
	return store.Message{}, nil
}

// askOffersOption reports whether the question offered optionID — the referent
// an answer's chosen ids must name.
func askOffersOption(opts []store.AskOption, optionID string) bool {
	for _, o := range opts {
		if o.ID == optionID {
			return true
		}
	}
	return false
}
