package store

// T2 (RIG-2257) pure-Go block-model contracts, needing no Postgres: the
// ask_answer variant round-trips through JSONB unchanged; the exactly-one-of
// invariant rejects every two-set combination; a stored row whose kind and
// payload disagree fails loud on read; and textContent folds the answered
// snapshot's custom-text answers into the searchable string. The real-Postgres
// AnswerAsk row contracts live in the pgtest siblings.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// answeredAsk builds a well-formed answered Ask snapshot for the ask_answer
// tests: one multi-question ask carrying recorded answers.
func answeredAskSnapshot() Ask {
	rec := int32(0)
	return Ask{
		AskID:    "ask-1",
		Answered: true,
		Questions: []AskQuestion{
			{
				QuestionID:      "q1",
				Question:        "Which environment?",
				Options:         []AskOption{{ID: "opt-a", Label: "staging"}, {ID: "opt-b", Label: "prod"}},
				Recommended:     &rec,
				ChosenOptionIDs: []string{"opt-b"},
			},
			{
				QuestionID: "q2",
				Question:   "Any notes?",
				CustomText: "ship it after the migration",
			},
		},
	}
}

func askAnswerBlock() MessageBlock {
	return MessageBlock{AskAnswer: &AskAnswerBlock{
		Ask:            answeredAskSnapshot(),
		AskerAccountID: "agent-asker",
	}}
}

// TestAskAnswerBlockRoundTrip pins that an ask_answer block survives a
// marshal/unmarshal round trip byte-for-value: the answered snapshot (ask_id,
// answered flag, questions with their recorded answers) and the asker account
// id all decode back identically.
func TestAskAnswerBlockRoundTrip(t *testing.T) {
	in := []MessageBlock{askAnswerBlock()}
	data, err := marshalBlocks(in)
	if err != nil {
		t.Fatalf("marshalBlocks: %v", err)
	}
	out, err := unmarshalBlocks(data)
	if err != nil {
		t.Fatalf("unmarshalBlocks: %v", err)
	}
	if len(out) != 1 || out[0].AskAnswer == nil {
		t.Fatalf("round trip = %+v, want one ask_answer block", out)
	}
	got := out[0].AskAnswer
	if got.AskerAccountID != "agent-asker" {
		t.Fatalf("asker_account_id = %q, want agent-asker", got.AskerAccountID)
	}
	if got.Ask.AskID != "ask-1" || !got.Ask.Answered {
		t.Fatalf("snapshot ask = %+v, want ask-1 answered", got.Ask)
	}
	if len(got.Ask.Questions) != 2 {
		t.Fatalf("snapshot questions = %d, want 2", len(got.Ask.Questions))
	}
	if q := got.Ask.Questions[0]; len(q.ChosenOptionIDs) != 1 || q.ChosenOptionIDs[0] != "opt-b" {
		t.Fatalf("q1 chosen = %v, want [opt-b]", q.ChosenOptionIDs)
	}
	if q := got.Ask.Questions[1]; q.CustomText != "ship it after the migration" {
		t.Fatalf("q2 custom_text = %q, want the recorded answer", q.CustomText)
	}
}

// TestMarshalBlocksExactlyOneOf pins the exactly-one-of invariant across the
// three-variant oneof: every two-set combination (and the empty block) is
// ErrInvalidArgument, so a malformed oneof never reaches a row.
func TestMarshalBlocksExactlyOneOf(t *testing.T) {
	txt := "hi"
	ask := &Ask{AskID: "ask-x", Questions: []AskQuestion{{QuestionID: "q1", Question: "?"}}}
	ans := &AskAnswerBlock{Ask: answeredAskSnapshot(), AskerAccountID: "agent-asker"}
	tests := []struct {
		name string
		b    MessageBlock
	}{
		{"none set", MessageBlock{}},
		{"text+ask", MessageBlock{Text: &txt, Ask: ask}},
		{"text+ask_answer", MessageBlock{Text: &txt, AskAnswer: ans}},
		{"ask+ask_answer", MessageBlock{Ask: ask, AskAnswer: ans}},
		{"all three", MessageBlock{Text: &txt, Ask: ask, AskAnswer: ans}},
	}
	for _, tc := range tests {
		if _, err := marshalBlocks([]MessageBlock{tc.b}); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s: err = %v, want errors.Is(_, ErrInvalidArgument)", tc.name, err)
		}
	}
}

// TestMarshalBlocksAskAnswerValidation pins the write-path structural checks on
// the ask_answer variant: an empty ask_id, a not-answered snapshot, malformed
// questions, and an empty asker_account_id are each ErrInvalidArgument.
func TestMarshalBlocksAskAnswerValidation(t *testing.T) {
	tests := []struct {
		name string
		mut  func(b *AskAnswerBlock)
	}{
		{"empty ask_id", func(b *AskAnswerBlock) { b.Ask.AskID = "" }},
		{"not answered", func(b *AskAnswerBlock) { b.Ask.Answered = false }},
		{"no questions", func(b *AskAnswerBlock) { b.Ask.Questions = nil }},
		{"empty question_id", func(b *AskAnswerBlock) { b.Ask.Questions[0].QuestionID = "" }},
		{"empty asker", func(b *AskAnswerBlock) { b.AskerAccountID = "" }},
	}
	for _, tc := range tests {
		ans := &AskAnswerBlock{Ask: answeredAskSnapshot(), AskerAccountID: "agent-asker"}
		tc.mut(ans)
		if _, err := marshalBlocks([]MessageBlock{{AskAnswer: ans}}); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s: err = %v, want errors.Is(_, ErrInvalidArgument)", tc.name, err)
		}
	}
}

// TestUnmarshalBlocksAskAnswerCorruptRow pins the read-path totality: a stored
// ask_answer row whose kind and payload disagree — the kind declared but no
// payload, or a payload that fails the answered-snapshot invariant — is a
// corrupt row surfaced as a loud error, never a silently-decoded ghost.
func TestUnmarshalBlocksAskAnswerCorruptRow(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"kind but no payload", `[{"kind":"ask_answer"}]`},
		{"empty ask_id", `[{"kind":"ask_answer","ask_answer":{"ask":{"ask_id":"","answered":true,"questions":[{"question_id":"q1","question":"?"}]},"asker_account_id":"a"}}]`},
		{"not answered", `[{"kind":"ask_answer","ask_answer":{"ask":{"ask_id":"ask-1","questions":[{"question_id":"q1","question":"?"}]},"asker_account_id":"a"}}]`},
		{"zero questions", `[{"kind":"ask_answer","ask_answer":{"ask":{"ask_id":"ask-1","answered":true},"asker_account_id":"a"}}]`},
		{"empty asker", `[{"kind":"ask_answer","ask_answer":{"ask":{"ask_id":"ask-1","answered":true,"questions":[{"question_id":"q1","question":"?"}]},"asker_account_id":""}}]`},
	}
	for _, tc := range tests {
		if _, err := unmarshalBlocks([]byte(tc.json)); err == nil {
			t.Fatalf("%s: unmarshalBlocks returned nil error, want a corrupt-row error", tc.name)
		}
	}
}

// TestTextContentIncludesAnsweredCustomText pins that a delivered answer is
// searchable: textContent folds both the snapshot's question prompts and its
// recorded custom-text answers into the indexed string.
func TestTextContentIncludesAnsweredCustomText(t *testing.T) {
	got := textContent([]MessageBlock{askAnswerBlock()})
	for _, want := range []string{"Which environment?", "Any notes?", "ship it after the migration"} {
		if !strings.Contains(got, want) {
			t.Fatalf("textContent = %q, want it to contain %q", got, want)
		}
	}
}

// TestMintAskIDsSkipsAskAnswer pins that mintAskIDs never re-mints the snapshot
// id on an ask_answer block: the snapshot carries the original ask's id, which
// must survive append untouched.
func TestMintAskIDsSkipsAskAnswer(t *testing.T) {
	b := askAnswerBlock()
	mintAskIDs([]MessageBlock{b})
	if b.AskAnswer.Ask.AskID != "ask-1" {
		t.Fatalf("ask_answer snapshot ask_id = %q, want ask-1 (never re-minted)", b.AskAnswer.Ask.AskID)
	}
}

// assert the storedAskAnswer JSON shape is stable (kind discriminant present),
// so a future rename of the JSON tag is caught here rather than at read time.
func TestAskAnswerStoredShape(t *testing.T) {
	data, err := marshalBlocks([]MessageBlock{askAnswerBlock()})
	if err != nil {
		t.Fatalf("marshalBlocks: %v", err)
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("raw = %v, want one block", raw)
	}
	var kind string
	if err := json.Unmarshal(raw[0]["kind"], &kind); err != nil {
		t.Fatalf("kind: %v", err)
	}
	if kind != string(blockKindAskAnswer) {
		t.Fatalf("kind = %q, want %q", kind, blockKindAskAnswer)
	}
	if _, ok := raw[0]["ask_answer"]; !ok {
		t.Fatalf("stored block missing ask_answer payload: %v", raw[0])
	}
}
