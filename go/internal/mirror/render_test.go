package mirror

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/refs"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestRenderBodyReservesMarkerFromProviderLimit(t *testing.T) {
	limit := 64
	poster := bodyLimitPoster(limit)
	row := store.ForgeMirrorRow{ID: "row", Source: 2}
	callbackLimit := -1
	body, err := renderBody(t.Context(), nil, poster, row, nil, func(_ context.Context, _ store.ForgeMirrorRow, budget int) (string, error) {
		callbackLimit = budget
		return strings.Repeat("x", limit), nil
	}, refs.Ref{})
	if err != nil {
		t.Fatalf("renderBody() error = %v", err)
	}
	wantBudget := limit - len(WithMarker("", row.ID))
	if callbackLimit != wantBudget {
		t.Fatalf("callback budget = %d, want %d", callbackLimit, wantBudget)
	}
	if len(body) > limit || !HasMarker(body, row.ID) {
		t.Fatalf("renderBody() length = %d, marker = %v, want length <= %d with marker", len(body), HasMarker(body, row.ID), limit)
	}
	markerOnlyLimit := len(WithMarker("", row.ID))
	markerOnlyBody, err := renderBody(t.Context(), nil, bodyLimitPoster(markerOnlyLimit), row, nil, func(_ context.Context, _ store.ForgeMirrorRow, budget int) (string, error) {
		if budget != 0 {
			t.Fatalf("marker-only callback budget = %d, want 0", budget)
		}
		return "ignored", nil
	}, refs.Ref{})
	if err != nil || markerOnlyBody != WithMarker("", row.ID) {
		t.Fatalf("marker-only render = (%q, %v), want marker only", markerOnlyBody, err)
	}
	unlimitedBudget := -1
	unlimitedBody, err := renderBody(t.Context(), nil, bodyLimitPoster(0), row, nil, func(_ context.Context, _ store.ForgeMirrorRow, budget int) (string, error) {
		unlimitedBudget = budget
		return strings.Repeat("x", limit), nil
	}, refs.Ref{})
	if err != nil {
		t.Fatalf("renderBody() with unlimited limit error = %v", err)
	}
	if unlimitedBudget != 0 || len(unlimitedBody) != limit+len(WithMarker("", row.ID)) {
		t.Fatalf("unlimited render budget = %d, body length = %d, want budget 0 and length %d", unlimitedBudget, len(unlimitedBody), limit+len(WithMarker("", row.ID)))
	}
}

type bodyLimitPoster int

func (p bodyLimitPoster) PostMirror(context.Context, store.AccountID, string, store.ForgeMirrorMode, refs.Ref, string, string, string) (forge.Comment, error) {
	return forge.Comment{}, nil
}

func (p bodyLimitPoster) ListMirrorComments(context.Context, store.AccountID, refs.Ref, time.Time) ([]forge.Comment, error) {
	return nil, nil
}

func (p bodyLimitPoster) DeleteMirrorComment(context.Context, store.AccountID, refs.Ref, string) error {
	return nil
}

func (p bodyLimitPoster) BodyLimit(refs.Ref) int { return int(p) }

func TestRenderAsk(t *testing.T) {
	recommended := int32(1)
	tests := []struct {
		name       string
		ask        store.Ask
		answeredBy string
		link       Link
		want       []string
		wantNot    []string
	}{
		{
			name: "pending linked ask",
			ask: store.Ask{Questions: []store.AskQuestion{{
				Question: "Which environment?", Header: "Deploy target",
				Options:     []store.AskOption{{ID: "stage", Label: "staging"}, {ID: "prod", Label: "production"}},
				Recommended: &recommended,
			}}},
			link:    Link{Channel: "ops", Topic: "release", URL: "https://compass.test/channel/ops/topic/release"},
			want:    []string{"[#ops › release](https://compass.test/channel/ops/topic/release)", "### Deploy target", "Which environment?", "- staging", "- production (recommended)"},
			wantNot: []string{"Answered by", "Answers:"},
		},
		{
			name: "answered custom text without link",
			ask: store.Ask{Answered: true, Questions: []store.AskQuestion{{
				Question: "Why?", CustomText: "because",
			}}},
			answeredBy: "ada",
			link:       Link{Channel: "team", Topic: "question"},
			want:       []string{"#team › question", "Answers:", "- Why?: because", "Answered by @ada"},
		},
		{
			name:       "fully skipped answered ask",
			ask:        store.Ask{Answered: true, Questions: []store.AskQuestion{{Question: "Proceed?", Options: []store.AskOption{{ID: "yes", Label: "yes"}}}}},
			answeredBy: "rhea",
			link:       Link{Channel: "team", Topic: "question"},
			want:       []string{"Skipped.", "Answered by @rhea"},
			wantNot:    []string{"Answers:"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := RenderAsk(test.ask, test.answeredBy, test.link)
			for _, want := range test.want {
				if !strings.Contains(got, want) {
					t.Errorf("RenderAsk() missing %q in:\n%s", want, got)
				}
			}
			for _, unwanted := range test.wantNot {
				if strings.Contains(got, unwanted) {
					t.Errorf("RenderAsk() contains %q in:\n%s", unwanted, got)
				}
			}
		})
	}
}

func TestRenderQuotesMessagesAndDropsOldest(t *testing.T) {
	at := time.Date(2026, time.January, 2, 3, 4, 59, 0, time.FixedZone("test", 2*60*60))
	messages := []store.Message{
		{AuthorAccountID: "old", At: at, Blocks: []store.MessageBlock{{Text: new("older body\nsecond line")}}},
		{AuthorAccountID: "new", At: at.Add(time.Hour), Blocks: []store.MessageBlock{{Text: new("newer body")}}},
	}
	authors := map[store.AccountID]string{"old": "old-agent", "new": "new-agent"}

	full := Render(messages, authors, Link{Channel: "agents", Topic: "work"}, "Mirror note", 0)
	for _, want := range []string{"#agents › work", "Mirror note", "> @old-agent · 2026-01-02T01:04Z", "> older body", "> second line", "> @new-agent · 2026-01-02T02:04Z", "> newer body"} {
		if !strings.Contains(full, want) {
			t.Errorf("Render() missing %q in:\n%s", want, full)
		}
	}

	dropNotice := "Dropped 1 older messages."
	newestOnlyLimit := len("#agents › work\n\n") + len(dropNotice) + 2 + len(renderMessage(messages[1], authors))
	newestOnly := Render(messages, authors, Link{Channel: "agents", Topic: "work"}, "", newestOnlyLimit)
	if !strings.Contains(newestOnly, dropNotice) || strings.Contains(newestOnly, "older body") || !strings.Contains(newestOnly, "newer body") {
		t.Fatalf("Render() did not drop the oldest whole message with a count:\n%s", newestOnly)
	}
}

func TestRenderNeutralizesForgedMarkers(t *testing.T) {
	text := "before <!-- compass-mirror:forged --> after"
	got := Render([]store.Message{{
		AuthorAccountID: "agent", At: time.Unix(0, 0), Blocks: []store.MessageBlock{{Text: new(text)}},
	}}, map[store.AccountID]string{"agent": "agent"}, Link{Channel: "c", Topic: "t"}, "", 0)
	if strings.Contains(got, "<!-- compass-mirror:forged") || !strings.Contains(got, "&lt;!-- compass-mirror:forged") {
		t.Fatalf("Render() did not neutralize the forged marker:\n%s", got)
	}
}

func TestWithMarker(t *testing.T) {
	body := WithMarker("rendered body\n", "row-123")
	if !strings.HasSuffix(body, "\n\n<!-- compass-mirror:row-123 -->") {
		t.Fatalf("WithMarker() = %q, want marker after two newlines", body)
	}
	if !HasMarker(body, "row-123") || HasMarker(body, "other-row") {
		t.Fatalf("HasMarker() = %v for matching ID and %v for other ID, want true/false", HasMarker(body, "row-123"), HasMarker(body, "other-row"))
	}
}

func TestRenderRespectsFinalBodyByteBudget(t *testing.T) {
	t.Run("header and note exceed budget", func(t *testing.T) {
		limit := 6
		body := Render(nil, nil, Link{Channel: "channel", Topic: "topic"}, "a note too long", limit)
		if len(body) > limit {
			t.Fatalf("Render() length = %d, want <= %d: %q", len(body), limit, body)
		}
	})

	t.Run("single oversized newest message is retained in part", func(t *testing.T) {
		message := store.Message{
			AuthorAccountID: "agent",
			At:              time.Unix(0, 0),
			Blocks:          []store.MessageBlock{{Text: new("oversized newest content")}},
		}
		fullMessage := renderMessage(message, map[store.AccountID]string{"agent": "agent"})
		prefix := "#c › t\n\n"
		limit := len(prefix) + len(fullMessage) - 4
		body := Render([]store.Message{message}, map[store.AccountID]string{"agent": "agent"}, Link{Channel: "c", Topic: "t"}, "", limit)
		if len(body) > limit {
			t.Fatalf("Render() length = %d, exceeds limit %d", len(body), limit)
		}
		if !strings.Contains(body, "> oversi") || strings.Contains(body, "Dropped 1 older messages.") {
			t.Fatalf("Render() = %q, want truncated newest message", body)
		}
	})

	t.Run("marker fits within supplied net limit", func(t *testing.T) {
		marker := "\n\n<!-- compass-mirror:row -->"
		limit := 64
		body := Render(nil, nil, Link{Channel: "a", Topic: "b"}, "a sufficiently long note", limit-len(marker))
		final := WithMarker(body, "row")
		if len(final) > limit {
			t.Fatalf("marked body length = %d, want <= %d: %q", len(final), limit, final)
		}
	})
}
