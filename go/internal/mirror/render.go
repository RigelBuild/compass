package mirror

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/RigelBuild/compass/go/internal/store"
)

// Link names the Compass source and its optional public URL.
type Link struct {
	Channel string
	Topic   string
	URL     string
}

// RenderAsk formats one ask and its recorded answer, when present.
func RenderAsk(ask store.Ask, answeredBy string, link Link) string {
	lines := make([]string, 1, 1+len(ask.Questions)*3)
	lines[0] = header(link)
	lines = append(lines, askLines(ask, answeredBy)...)
	return strings.Join(lines, "\n")
}

func truncateUTF8(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

// Render formats the source header and message history within a byte limit.
// Callers pass the limit net of any owner header; the driver reserves the row marker.
// renderAskWithinLimit keeps the source header and newest ask details in budget.
func renderAskWithinLimit(ask store.Ask, answeredBy string, link Link, limit int) string {
	head := header(link)
	body := askLines(ask, answeredBy)
	if limit <= 0 {
		return strings.Join(append([]string{head}, body...), "\n")
	}
	if len(head) >= limit {
		return truncateUTF8(head, limit)
	}
	used := len(head)
	kept := make([]string, 0, len(body))
	for _, line := range slices.Backward(body) {
		available := limit - used - 1
		if available <= 0 {
			break
		}
		if len(line) > available {
			kept = append(kept, truncateUTF8(line, available))
			break
		}
		kept = append(kept, line)
		used += len(line) + 1
	}
	result := []string{head}
	for _, line := range slices.Backward(kept) {
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

func Render(msgs []store.Message, authors map[store.AccountID]string, link Link, note string, limit int) string {
	prefix := []string{header(link)}
	if note != "" {
		prefix = append(prefix, neutralize(note))
	}
	if limit <= 0 {
		lines := append([]string(nil), prefix...)
		for _, msg := range msgs {
			lines = append(lines, renderMessage(msg, authors))
		}
		return strings.Join(lines, "\n\n")
	}
	if renderedLength(prefix) > limit {
		if len(prefix) == 2 && limit > len(prefix[0])+2 {
			prefix[1] = truncateUTF8(prefix[1], limit-len(prefix[0])-2)
		} else {
			prefix = []string{truncateUTF8(prefix[0], limit)}
		}
		if renderedLength(prefix) > limit {
			return truncateUTF8(prefix[0], limit)
		}
		return strings.Join(prefix, "\n\n")
	}
	if len(msgs) == 0 {
		return strings.Join(prefix, "\n\n")
	}

	messageBodies := make([]string, len(msgs))
	for i, msg := range msgs {
		messageBodies[i] = renderMessage(msg, authors)
	}
	baseLength := renderedLength(prefix)
	suffixLength := 0
	var best []string
	for kept := 1; kept <= len(messageBodies); kept++ {
		message := messageBodies[len(messageBodies)-kept]
		suffixLength += len(message)
		if kept > 1 {
			suffixLength += 2
		}
		dropped := len(messageBodies) - kept
		dropLine := ""
		dropLength := 0
		if dropped > 0 {
			dropLine = fmt.Sprintf("Dropped %d older messages.", dropped)
			dropLength = len(dropLine) + 2
		}
		if baseLength+dropLength+suffixLength <= limit {
			best = make([]string, 0, len(prefix)+1+kept)
			best = append(best, prefix...)
			if dropLine != "" {
				best = append(best, dropLine)
			}
			best = append(best, messageBodies[len(messageBodies)-kept:]...)
		}
	}
	if best != nil {
		return strings.Join(best, "\n\n")
	}

	dropped := len(messageBodies) - 1
	lines := append([]string(nil), prefix...)
	used := baseLength
	if dropped > 0 {
		dropLine := fmt.Sprintf("Dropped %d older messages.", dropped)
		if used+2+len(dropLine) < limit {
			lines = append(lines, dropLine)
			used += len(dropLine) + 2
		}
	}
	if used < limit {
		available := limit - used
		if len(lines) > 0 {
			available -= 2
		}
		if available > 0 {
			lines = append(lines, truncateUTF8(messageBodies[len(messageBodies)-1], available))
		}
	}
	return strings.Join(lines, "\n\n")
}

func renderedLength(lines []string) int {
	if len(lines) == 0 {
		return 0
	}
	length := (len(lines) - 1) * 2
	for _, line := range lines {
		length += len(line)
	}
	return length
}

// WithMarker appends the row marker after the rendered body.
func WithMarker(body, rowID string) string {
	return strings.TrimRight(body, "\n") + "\n\n<!-- compass-mirror:" + rowID + " -->"
}

// HasMarker reports whether body contains the row marker.
func HasMarker(body, rowID string) bool {
	return strings.Contains(body, "<!-- compass-mirror:"+rowID+" -->")
}

func header(link Link) string {
	label := "#" + neutralize(link.Channel) + " › " + neutralize(link.Topic)
	if link.URL == "" {
		return label
	}
	return "[" + label + "](" + link.URL + ")"
}

func askLines(ask store.Ask, answeredBy string) []string {
	var lines []string
	for _, question := range ask.Questions {
		if question.Header != "" {
			lines = append(lines, "### "+neutralize(question.Header))
		}
		lines = append(lines, neutralize(question.Question))
		for index, option := range question.Options {
			label := neutralize(option.Label)
			if question.Recommended != nil && int(*question.Recommended) == index {
				label += " (recommended)"
			}
			lines = append(lines, "- "+label)
		}
	}
	if !ask.Answered {
		return lines
	}

	var answers []string
	allSkipped := true
	for _, question := range ask.Questions {
		var selected []string
		for _, chosenID := range question.ChosenOptionIDs {
			for _, option := range question.Options {
				if option.ID == chosenID {
					selected = append(selected, neutralize(option.Label))
					allSkipped = false
					break
				}
			}
		}
		if question.CustomText != "" {
			selected = append(selected, neutralize(question.CustomText))
			allSkipped = false
		}
		if len(selected) > 0 {
			answers = append(answers, "- "+neutralize(question.Question)+": "+strings.Join(selected, "; "))
		}
	}
	if allSkipped {
		lines = append(lines, "Skipped.")
	} else {
		lines = append(lines, "Answers:")
		lines = append(lines, answers...)
	}
	lines = append(lines, "Answered by @"+neutralize(answeredBy))
	return lines
}

func renderMessage(msg store.Message, authors map[store.AccountID]string) string {
	author := authors[msg.AuthorAccountID]
	if author == "" {
		author = string(msg.AuthorAccountID)
	}
	lines := []string{fmt.Sprintf("> @%s · %s", neutralize(author), msg.At.UTC().Format("2006-01-02T15:04Z"))}
	for _, block := range msg.Blocks {
		switch {
		case block.Text != nil:
			lines = appendQuoted(lines, neutralize(*block.Text))
		case block.Ask != nil:
			lines = appendQuoted(lines, strings.Join(askLines(*block.Ask, ""), "\n"))
		case block.AskAnswer != nil:
			lines = appendQuoted(lines, strings.Join(askLines(block.AskAnswer.Ask, author), "\n"))
		}
	}
	return strings.Join(lines, "\n")
}

func appendQuoted(lines []string, text string) []string {
	for line := range strings.SplitSeq(text, "\n") {
		lines = append(lines, "> "+line)
	}
	return lines
}

// neutralize escapes quoted mirror markers so a quoted body cannot be adopted as
// another row's comment. Forged owner headers are left to forge.StampOwner's strip.
func neutralize(text string) string {
	return strings.ReplaceAll(text, "<!-- compass-mirror:", "&lt;!-- compass-mirror:")
}
