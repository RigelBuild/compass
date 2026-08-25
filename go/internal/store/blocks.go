package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Message blocks are stored as JSONB (the messages.blocks column) and their
// text is extracted into messages.text_content for the full-text index. The
// on-disk JSON is this package's own tagged shape, not the compass.v1 wire
// encoding: the store owns its column format, and T2 maps store ↔ proto at the
// service edge. The block oneof is narrowed to text + ask (OQ-A) — the trace
// variants left the comms surface — so exactly one of the two fields is set per
// stored block.

// blockKind tags a stored block's variant so the JSON round-trips the oneof
// unambiguously.
type blockKind string

const (
	blockKindText      blockKind = "text"
	blockKindAsk       blockKind = "ask"
	blockKindAskAnswer blockKind = "ask_answer"
)

// storedBlock is the JSONB shape of one MessageBlock. Kind is the discriminant;
// exactly one of Text / Ask / AskAnswer is populated to match it.
type storedBlock struct {
	Kind      blockKind        `json:"kind"`
	Text      *string          `json:"text,omitempty"`
	Ask       *storedAsk       `json:"ask,omitempty"`
	AskAnswer *storedAskAnswer `json:"ask_answer,omitempty"`
}

// storedAskAnswer is the JSONB shape of an ask_answer block: the answered ask
// snapshot plus the asking agent's account id.
type storedAskAnswer struct {
	Ask            storedAsk `json:"ask"`
	AskerAccountID string    `json:"asker_account_id"`
}

// storedAsk is the JSONB shape of an Ask block.
type storedAsk struct {
	AskID     string              `json:"ask_id"`
	Questions []storedAskQuestion `json:"questions,omitempty"`
	// Answered mirrors Ask.Answered: true once answered, so the pending/answered
	// distinction survives the JSONB round-trip even for a fully-skipped ask.
	Answered bool `json:"answered,omitempty"`
}

// storedAskQuestion is the JSONB shape of one question within an Ask.
type storedAskQuestion struct {
	QuestionID      string            `json:"question_id"`
	Question        string            `json:"question"`
	Header          string            `json:"header,omitempty"`
	Options         []storedAskOption `json:"options,omitempty"`
	AllowMultiple   bool              `json:"allow_multiple,omitempty"`
	Recommended     *int32            `json:"recommended,omitempty"`
	ChosenOptionIDs []string          `json:"chosen_option_ids,omitempty"`
	CustomText      string            `json:"custom_text,omitempty"`
	TimedOut        bool              `json:"timed_out,omitempty"`
}

// storedAskOption is the JSONB shape of one Ask option.
type storedAskOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Preview     string `json:"preview,omitempty"`
}

// marshalBlocks serializes a message's blocks to JSONB for storage, rejecting a
// malformed block (not exactly one of text/ask/ask_answer set, an ask with zero
// questions or a duplicate/empty question_id, or a malformed ask_answer) as
// ErrInvalidArgument so a bad oneof never reaches a row. It is pure: ask_id is
// assigned once at append (mintAskIDs), never here, so re-serializing an
// existing message (the update path) can never re-mint an id and break
// RespondToAsk correlation. It does NOT reject the server-owned ask_answer
// variant on caller identity (it has none — it receives only the block slice);
// server-ownership is enforced at the wire edge (T4). It accepts and
// structurally validates the variant so AnswerAsk's own answer persist works.
func marshalBlocks(blocks []MessageBlock) ([]byte, error) {
	stored := make([]storedBlock, 0, len(blocks))
	for i, b := range blocks {
		switch {
		case b.Text != nil && b.Ask == nil && b.AskAnswer == nil:
			stored = append(stored, storedBlock{Kind: blockKindText, Text: b.Text})
		case b.Ask != nil && b.Text == nil && b.AskAnswer == nil:
			if err := validateAskQuestions(b.Ask); err != nil {
				return nil, err
			}
			stored = append(stored, storedBlock{Kind: blockKindAsk, Ask: toStoredAsk(b.Ask)})
		case b.AskAnswer != nil && b.Text == nil && b.Ask == nil:
			if err := validateAskAnswer(b.AskAnswer); err != nil {
				return nil, err
			}
			stored = append(stored, storedBlock{Kind: blockKindAskAnswer, AskAnswer: toStoredAskAnswer(b.AskAnswer)})
		default:
			return nil, fmt.Errorf("%w: block %d must set exactly one of text/ask/ask_answer", ErrInvalidArgument, i)
		}
	}
	out, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("store: marshal blocks: %w", err)
	}
	return out, nil
}

// validateAskQuestions enforces the write-path half of the ask totality
// invariant (the store mirror of the T5 mapper's malformed-id guard): an ask
// must carry at least one question, every question_id must be non-empty AND
// unique within the ask (a duplicate or empty key would make an
// AskQuestionAnswer unaddressable), and every non-nil Recommended must be a
// valid index into that question's Options. Caller-supplied bad input, so the
// ErrInvalidArgument family — same as marshalBlocks' exactly-one-of check.
func validateAskQuestions(a *Ask) error {
	if err := askQuestionsWellFormed(a); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidArgument, err)
	}
	for _, q := range a.Questions {
		// Recommended is a zero-based index into Options (types.go), agent-
		// supplied. This is a write-path input-hygiene check: reject a
		// malformed agent-supplied index on write rather than store it. It is
		// NOT a load-bearing read invariant, so read-back does not re-enforce
		// it — the frozen design (compass-ask-typed-derivation.md: the UI
		// treats an out-of-range recommended as "no highlight", T6) tolerates
		// an OOB index at render, so a stored OOB value is not a client hazard.
		// A free-text-only question (no Options) with any Recommended set is
		// therefore invalid on write, which is correct.
		if q.Recommended != nil && (*q.Recommended < 0 || int(*q.Recommended) >= len(q.Options)) {
			return fmt.Errorf("%w: ask question %q recommended index %d out of range for %d options", ErrInvalidArgument, q.QuestionID, *q.Recommended, len(q.Options))
		}
	}
	return nil
}

// validateAskAnswer enforces the ask_answer variant invariant on the write
// path: a non-nil answered Ask snapshot (a non-empty AskID, Answered=true, and
// well-formed questions — the same structural integrity every stored ask must
// have), plus a non-empty AskerAccountID. Caller-supplied shape, so the
// ErrInvalidArgument family — same as marshalBlocks' exactly-one-of check. The
// server-ownership rule (only AnswerAsk may construct one) is a wire-edge
// concern (T4), NOT enforced here.
func validateAskAnswer(b *AskAnswerBlock) error {
	if b.Ask.AskID == "" {
		return fmt.Errorf("%w: ask_answer snapshot has empty ask_id", ErrInvalidArgument)
	}
	if !b.Ask.Answered {
		return fmt.Errorf("%w: ask_answer snapshot must be answered", ErrInvalidArgument)
	}
	if err := askQuestionsWellFormed(&b.Ask); err != nil {
		return fmt.Errorf("%w: ask_answer snapshot: %w", ErrInvalidArgument, err)
	}
	if b.AskerAccountID == "" {
		return fmt.Errorf("%w: ask_answer has empty asker_account_id", ErrInvalidArgument)
	}
	return nil
}

// askQuestionsWellFormed checks the structural integrity of an ask's question
// set that BOTH the write and read paths require: at least one question, and
// every question_id non-empty and unique (the correlation key for an
// AskQuestionAnswer and applyAskAnswer's byID map). It returns a bare error
// naming the defect; callers wrap it in the class that fits their trust
// boundary — ErrInvalidArgument for caller input on the write path, a
// corrupt-row error for a stored row on the read path.
func askQuestionsWellFormed(a *Ask) error {
	if len(a.Questions) == 0 {
		return errors.New("ask has no questions")
	}
	seen := make(map[string]struct{}, len(a.Questions))
	for _, q := range a.Questions {
		if q.QuestionID == "" {
			return errors.New("ask question has empty question_id")
		}
		if _, dup := seen[q.QuestionID]; dup {
			return fmt.Errorf("ask has duplicate question_id %q", q.QuestionID)
		}
		seen[q.QuestionID] = struct{}{}
	}
	return nil
}

// mintAskIDs assigns a server-minted ask_id to every ask block that lacks one,
// in place on the caller's Ask so the returned message carries the same value
// RespondToAsk will correlate against (comms.proto:278-280: ask_id is
// server-assigned and globally unique). Called only at append: ask_id is
// assigned once and immutable thereafter, so the update path preserves it rather
// than reassigning.
func mintAskIDs(blocks []MessageBlock) {
	for _, b := range blocks {
		// Guard the full valid-arm predicate (b.Text == nil too), so a
		// malformed block that sets both text and ask is not stamped with a
		// server id before marshalBlocks rejects it — mint only touches a
		// well-formed, id-less ask.
		if b.Ask != nil && b.Text == nil && b.Ask.AskID == "" {
			b.Ask.AskID = newID()
		}
	}
}

// askIDContainmentFilter builds the JSONB @> probe that finds the message
// carrying an ask with askID. It is a MINIMAL projection — {"kind":"ask",
// "ask":{"ask_id":X}} — deliberately NOT json.Marshal(storedBlock{...}): a full
// storedBlock serializes the ask's questions array (never omitted for a real
// ask), which @> would then require the stored ask to match those questions
// element-for-element, so no real ask would ever match. Containment cares
// only about the discriminating fields, so the probe carries only them.
func askIDContainmentFilter(askID string) ([]byte, error) {
	probe := []map[string]any{{
		"kind": string(blockKindAsk),
		"ask":  map[string]any{"ask_id": askID},
	}}
	return json.Marshal(probe)
}

// unmarshalBlocks decodes stored JSONB back into MessageBlocks. A block whose
// kind and payload disagree is a corrupted row, surfaced as an error rather
// than a silently-dropped variant (the totality discipline: a missing arm must
// never pass silently).
func unmarshalBlocks(data []byte) ([]MessageBlock, error) {
	var stored []storedBlock
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("store: unmarshal blocks: %w", err)
	}
	blocks := make([]MessageBlock, 0, len(stored))
	for i, sb := range stored {
		switch sb.Kind {
		case blockKindText:
			if sb.Text == nil {
				return nil, fmt.Errorf("store: block %d kind=text but no text payload", i)
			}
			blocks = append(blocks, MessageBlock{Text: sb.Text})
		case blockKindAsk:
			if sb.Ask == nil {
				return nil, fmt.Errorf("store: block %d kind=ask but no ask payload", i)
			}
			// A stored ask must satisfy the same structural integrity the write
			// path guarantees: at least one question, every question_id non-empty
			// and unique. A row that fails this is corrupt or pre-reshape (the old
			// {"question":…,"options":…} shape decodes to Questions:nil since Go
			// ignores unknown keys; a duplicate/empty id would collapse
			// applyAskAnswer's byID map into an unanswerable ask) — fail loud
			// rather than decode a broken ask (the totality discipline). This is a
			// stored-row defect, not caller input, so it is NOT ErrInvalidArgument.
			ask := fromStoredAsk(sb.Ask)
			if err := askQuestionsWellFormed(ask); err != nil {
				return nil, fmt.Errorf("store: block %d ask is corrupt or pre-reshape: %w", i, err)
			}
			blocks = append(blocks, MessageBlock{Ask: ask})
		case blockKindAskAnswer:
			if sb.AskAnswer == nil {
				return nil, fmt.Errorf("store: block %d kind=ask_answer but no ask_answer payload", i)
			}
			// A stored ask_answer must satisfy the same invariant the write
			// path guarantees: an answered snapshot (non-empty ask_id,
			// answered=true, well-formed questions) and a non-empty
			// asker_account_id. A row that fails this is corrupt — fail loud
			// rather than decode a broken answer (the totality discipline). A
			// stored-row defect, not caller input, so NOT ErrInvalidArgument.
			ask := fromStoredAsk(&sb.AskAnswer.Ask)
			if sb.AskAnswer.Ask.AskID == "" {
				return nil, fmt.Errorf("store: block %d ask_answer snapshot has empty ask_id", i)
			}
			if !sb.AskAnswer.Ask.Answered {
				return nil, fmt.Errorf("store: block %d ask_answer snapshot is not answered", i)
			}
			if err := askQuestionsWellFormed(ask); err != nil {
				return nil, fmt.Errorf("store: block %d ask_answer snapshot is corrupt: %w", i, err)
			}
			if sb.AskAnswer.AskerAccountID == "" {
				return nil, fmt.Errorf("store: block %d ask_answer has empty asker_account_id", i)
			}
			blocks = append(blocks, MessageBlock{AskAnswer: &AskAnswerBlock{
				Ask:            *ask,
				AskerAccountID: AccountID(sb.AskAnswer.AskerAccountID),
			}})
		default:
			return nil, fmt.Errorf("store: block %d has unknown kind %q", i, sb.Kind)
		}
	}
	return blocks, nil
}

// textContent concatenates the text of a message's text blocks (with an ask's
// question folded in, and an ask_answer's snapshot questions + custom answer
// text folded in, so both are searchable) into the string the generated
// tsvector indexes. Newline-joined so distinct blocks don't merge words across
// a boundary.
func textContent(blocks []MessageBlock) string {
	var parts []string
	for _, b := range blocks {
		switch {
		case b.Text != nil:
			parts = append(parts, *b.Text)
		case b.Ask != nil:
			for _, q := range b.Ask.Questions {
				parts = append(parts, q.Question)
			}
		case b.AskAnswer != nil:
			for _, q := range b.AskAnswer.Ask.Questions {
				parts = append(parts, q.Question)
				if q.CustomText != "" {
					parts = append(parts, q.CustomText)
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

// toStoredAsk maps the domain Ask onto its JSONB shape.
func toStoredAsk(a *Ask) *storedAsk {
	qs := make([]storedAskQuestion, 0, len(a.Questions))
	for _, q := range a.Questions {
		opts := make([]storedAskOption, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, storedAskOption(o))
		}
		qs = append(qs, storedAskQuestion{
			QuestionID:      q.QuestionID,
			Question:        q.Question,
			Header:          q.Header,
			Options:         opts,
			AllowMultiple:   q.AllowMultiple,
			Recommended:     q.Recommended,
			ChosenOptionIDs: q.ChosenOptionIDs,
			CustomText:      q.CustomText,
			TimedOut:        q.TimedOut,
		})
	}
	return &storedAsk{AskID: a.AskID, Questions: qs, Answered: a.Answered}
}

// fromStoredAsk maps a stored Ask back to the domain type.
func fromStoredAsk(s *storedAsk) *Ask {
	qs := make([]AskQuestion, 0, len(s.Questions))
	for _, q := range s.Questions {
		opts := make([]AskOption, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, AskOption(o))
		}
		qs = append(qs, AskQuestion{
			QuestionID:      q.QuestionID,
			Question:        q.Question,
			Header:          q.Header,
			Options:         opts,
			AllowMultiple:   q.AllowMultiple,
			Recommended:     q.Recommended,
			ChosenOptionIDs: q.ChosenOptionIDs,
			CustomText:      q.CustomText,
			TimedOut:        q.TimedOut,
		})
	}
	return &Ask{AskID: s.AskID, Questions: qs, Answered: s.Answered}
}

// toStoredAskAnswer maps the domain AskAnswerBlock onto its JSONB shape,
// reusing toStoredAsk for the answered snapshot.
func toStoredAskAnswer(b *AskAnswerBlock) *storedAskAnswer {
	return &storedAskAnswer{
		Ask:            *toStoredAsk(&b.Ask),
		AskerAccountID: string(b.AskerAccountID),
	}
}
