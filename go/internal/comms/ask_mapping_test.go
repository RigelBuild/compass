// The ask answer-state ownership contract at the proto <-> store edge
// (mapping.go askToWire/askFromWire), driven with NO database: both are pure
// functions of their argument, so the contract is fully observable without a
// store. This file is untagged, so it runs on the default `go test` lane — no
// pgtest, no COMPASS_TEST_DATABASE_DSN. The DB-backed
// TestPostMessageDropsCallerSuppliedAnswerState (comms_test.go) covers the same
// boundary end-to-end through PostMessage; this one covers it exhaustively.
//
// The invariant (mapping.go:268-292):
//   - Inbound, every server-owned field is dropped: Ask.ask_id and
//     Ask.answered, plus each question's chosen_option_ids / custom_text /
//     timed_out. An ask arriving over the wire is being POSTED, so it has by
//     definition not been answered.
//   - Inbound, every content field survives: over-dropping is as wrong as
//     under-dropping — it silently discards agent-supplied question content.
//   - Outbound, Ask.answered carries the store's flag, the only reliable
//     answered-signal a client has.
//
// askFromWire drops by OMITTING fields from a keyed composite literal, which
// compiles and vets clean with any subset set — so a field added to the proto
// and wired into askToWire would be silently honored inbound, reintroducing the
// defect. The guard is therefore written against the message DESCRIPTORS rather
// than a hand-written field list: every field must appear in exactly one of the
// two classification sets below, and a new one appearing in neither fails.

package comms

import (
	"strconv"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// askFieldOwnership classifies every field of one ask-related wire message as
// content (askFromWire must map it) or server-owned answer state (askFromWire
// must drop it). Adding a field to the proto without adding it here fails
// TestAskFromWireDropsEveryServerOwnedField, which is the whole point: the
// classification is a decision, and this is where it is recorded.
type askFieldOwnership struct {
	content     map[protoreflect.Name]struct{}
	serverOwned map[protoreflect.Name]struct{}
}

func fieldSet(names ...protoreflect.Name) map[protoreflect.Name]struct{} {
	out := make(map[protoreflect.Name]struct{}, len(names))
	for _, n := range names {
		out[n] = struct{}{}
	}
	return out
}

var (
	askOwnership = askFieldOwnership{
		content:     fieldSet("questions"),
		serverOwned: fieldSet("ask_id", "answered"),
	}
	askQuestionOwnership = askFieldOwnership{
		content:     fieldSet("question_id", "question", "header", "options", "allow_multiple", "recommended"),
		serverOwned: fieldSet("chosen_option_ids", "custom_text", "timed_out"),
	}
	askOptionOwnership = askFieldOwnership{
		content:     fieldSet("id", "label", "description", "preview"),
		serverOwned: fieldSet(),
	}
)

// TestAskFromWireDropsEveryServerOwnedField is the guard mapping.go:290 names:
// it walks the ask message descriptors so a field added to the proto cannot
// slip through askFromWire's omission-based drop unnoticed. It populates EVERY
// field of a wire Ask by reflection — content and answer state alike — maps it
// inbound, and asserts the store Ask carries only content.
func TestAskFromWireDropsEveryServerOwnedField(t *testing.T) {
	askDesc := (&compassv1.Ask{}).ProtoReflect().Descriptor()
	questionDesc := (&compassv1.AskQuestion{}).ProtoReflect().Descriptor()
	optionDesc := (&compassv1.AskOption{}).ProtoReflect().Descriptor()

	assertEveryFieldClassified(t, askDesc, askOwnership)
	assertEveryFieldClassified(t, questionDesc, askQuestionOwnership)
	assertEveryFieldClassified(t, optionDesc, askOptionOwnership)

	wire := &compassv1.Ask{}
	populateEveryField(t, wire.ProtoReflect())

	// Reflection populates repeated fields with two elements, so the assertions
	// below exercise the per-element loops rather than a single lucky index.
	if len(wire.GetQuestions()) != 2 {
		t.Fatalf("reflection populated %d questions, want 2", len(wire.GetQuestions()))
	}

	got := askFromWire(wire)

	// Server-owned: dropped.
	if got.AskID != "" {
		t.Errorf("AskID = %q, want empty (server-minted on append)", got.AskID)
	}
	if got.Answered {
		t.Error("Answered = true, want false (an ask being posted is by definition unanswered)")
	}

	// Content: survives, with the values reflection set.
	if len(got.Questions) != len(wire.GetQuestions()) {
		t.Fatalf("Questions length = %d, want %d", len(got.Questions), len(wire.GetQuestions()))
	}
	for i, q := range got.Questions {
		if want := nonZeroString("question_id", ""); q.QuestionID != want {
			t.Errorf("Questions[%d].QuestionID = %q, want %q", i, q.QuestionID, want)
		}
		if want := nonZeroString("question", ""); q.Question != want {
			t.Errorf("Questions[%d].Question = %q, want %q", i, q.Question, want)
		}
		if want := nonZeroString("header", ""); q.Header != want {
			t.Errorf("Questions[%d].Header = %q, want %q", i, q.Header, want)
		}
		if !q.AllowMultiple {
			t.Errorf("Questions[%d].AllowMultiple = false, want true", i)
		}
		if q.Recommended == nil || *q.Recommended != nonZeroInt32 {
			t.Errorf("Questions[%d].Recommended = %v, want %d", i, q.Recommended, nonZeroInt32)
		}
		if len(q.Options) != 2 {
			t.Fatalf("Questions[%d].Options length = %d, want 2", i, len(q.Options))
		}
		for j, o := range q.Options {
			if want := nonZeroString("id", ""); o.ID != want {
				t.Errorf("Questions[%d].Options[%d].ID = %q, want %q", i, j, o.ID, want)
			}
			if want := nonZeroString("label", ""); o.Label != want {
				t.Errorf("Questions[%d].Options[%d].Label = %q, want %q", i, j, o.Label, want)
			}
			if want := nonZeroString("description", ""); o.Description != want {
				t.Errorf("Questions[%d].Options[%d].Description = %q, want %q", i, j, o.Description, want)
			}
			if want := nonZeroString("preview", ""); o.Preview != want {
				t.Errorf("Questions[%d].Options[%d].Preview = %q, want %q", i, j, o.Preview, want)
			}
		}

		// Server-owned, per question: dropped.
		if len(q.ChosenOptionIDs) != 0 {
			t.Errorf("Questions[%d].ChosenOptionIDs = %v, want none (only RespondToAsk records an answer)", i, q.ChosenOptionIDs)
		}
		if q.CustomText != "" {
			t.Errorf("Questions[%d].CustomText = %q, want empty (only RespondToAsk records an answer)", i, q.CustomText)
		}
		if q.TimedOut {
			t.Errorf("Questions[%d].TimedOut = true, want false (only RespondToAsk records an answer)", i)
		}
	}
}

// TestAskAnsweredProjectsOutbound pins the outbound half: askToWire carries the
// store's Answered flag onto the wire. A client that cannot see it renders a
// spent ask as answerable and fires a RespondToAsk the server is guaranteed to
// reject, silently discarding the participant's click.
func TestAskAnsweredProjectsOutbound(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answered bool
	}{
		{name: "answered", answered: true},
		{name: "pending", answered: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := askToWire(&store.Ask{
				AskID:     "ask-1",
				Answered:  tc.answered,
				Questions: []store.AskQuestion{{QuestionID: "q1", Question: "Which environment?"}},
			})
			if got.GetAnswered() != tc.answered {
				t.Errorf("wire Ask.answered = %v, want %v", got.GetAnswered(), tc.answered)
			}
		})
	}
}

// assertEveryFieldClassified fails when a descriptor field appears in neither
// ownership set (a field was added to the proto without deciding whether
// askFromWire maps or drops it) or when a set names a field the descriptor no
// longer has (a stale entry, which would let a real field go unchecked).
func assertEveryFieldClassified(t *testing.T, md protoreflect.MessageDescriptor, own askFieldOwnership) {
	t.Helper()
	fds := md.Fields()
	for i := range fds.Len() {
		name := fds.Get(i).Name()
		_, isContent := own.content[name]
		_, isServerOwned := own.serverOwned[name]
		switch {
		case isContent && isServerOwned:
			t.Errorf("%s.%s is classified as BOTH content and server-owned; it is one or the other", md.Name(), name)
		case !isContent && !isServerOwned:
			t.Errorf("%s.%s is unclassified: a new ask field must be consciously classified as content "+
				"(map it in askFromWire and add it to the content set here) or as server-owned answer state "+
				"(omit it from askFromWire and add it to the serverOwned set here). askFromWire drops by "+
				"omission, so an unclassified field is silently honored from whatever the caller sent.",
				md.Name(), name)
		}
	}
	for _, set := range []map[protoreflect.Name]struct{}{own.content, own.serverOwned} {
		for name := range set {
			if fds.ByName(name) == nil {
				t.Errorf("%s.%s is classified here but no longer exists on the descriptor; drop the stale entry", md.Name(), name)
			}
		}
	}
}

// nonZeroInt32 is the value reflection writes into every int32 field.
const nonZeroInt32 = int32(7)

// nonZeroString is the value reflection writes into a string field, derived
// from the field name so an assertion can name what it expects. suffix
// distinguishes the elements of a repeated field.
func nonZeroString(name protoreflect.Name, suffix string) string {
	return "v-" + string(name) + suffix
}

// populateEveryField sets EVERY field of m — and, recursively, of every message
// it contains — to a non-zero value, driven by the descriptor rather than a
// hand-written list so a newly added field is populated automatically. An
// unhandled kind fails the test rather than leaving a field silently zero,
// which would make the drop assertions vacuously pass.
func populateEveryField(t *testing.T, m protoreflect.Message) {
	t.Helper()
	fds := m.Descriptor().Fields()
	for i := range fds.Len() {
		fd := fds.Get(i)
		switch {
		case fd.IsMap():
			t.Fatalf("%s is a map field; populateEveryField does not handle maps yet", fd.FullName())
		case fd.IsList():
			list := m.Mutable(fd).List()
			for n := range 2 {
				if fd.Kind() == protoreflect.MessageKind {
					elem := list.NewElement()
					populateEveryField(t, elem.Message())
					list.Append(elem)
					continue
				}
				list.Append(nonZeroScalar(t, fd, "-"+strconv.Itoa(n)))
			}
		case fd.Kind() == protoreflect.MessageKind:
			populateEveryField(t, m.Mutable(fd).Message())
		default:
			m.Set(fd, nonZeroScalar(t, fd, ""))
		}
	}
}

// nonZeroScalar returns a non-zero value for fd's kind.
func nonZeroScalar(t *testing.T, fd protoreflect.FieldDescriptor, suffix string) protoreflect.Value {
	t.Helper()
	switch fd.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(nonZeroString(fd.Name(), suffix))
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(true)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return protoreflect.ValueOfInt32(nonZeroInt32)
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return protoreflect.ValueOfInt64(int64(nonZeroInt32))
	case protoreflect.BytesKind:
		return protoreflect.ValueOfBytes([]byte(nonZeroString(fd.Name(), suffix)))
	default:
		t.Fatalf("%s has kind %s, which populateEveryField cannot set to a non-zero value; teach it that kind", fd.FullName(), fd.Kind())
		return protoreflect.Value{}
	}
}
