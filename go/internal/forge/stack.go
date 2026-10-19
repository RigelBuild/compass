package forge

import (
	"strconv"
	"strings"
)

const (
	compassStackStart = "<!-- compass:stack -->"  //nolint:gosec // G101: HTML marker, not a credential
	compassStackEnd   = "<!-- /compass:stack -->" //nolint:gosec // G101: HTML marker, not a credential
	jjVineStackStart  = "<!-- start jj-vine stack -->"
	jjVineStackEnd    = "<!-- end jj-vine stack -->"
)

// StackLine is one pull request in the rendered stack.
type StackLine struct {
	Number uint64
	Title  string
}

// RenderStack formats one Compass-owned stack block and marks the current PR.
func RenderStack(lines []StackLine, self uint64) string {
	if len(lines) == 0 {
		return ""
	}

	var rendered strings.Builder
	rendered.WriteString(compassStackStart)
	rendered.WriteString("\nThis PR is part of a stack containing ")
	rendered.WriteString(strconv.Itoa(len(lines)))
	rendered.WriteString(" PRs:\n")
	for i, line := range lines {
		if i > 0 {
			rendered.WriteByte('\n')
		}
		rendered.WriteString(strconv.Itoa(i + 1))
		rendered.WriteString(". ")
		if line.Number == self {
			rendered.WriteString("**\"")
			rendered.WriteString(escapeStackTitle(line.Title))
			rendered.WriteString("\" (this PR)**")
			continue
		}
		rendered.WriteByte('#')
		rendered.WriteString(strconv.FormatUint(line.Number, 10))
		rendered.WriteString(" \"")
		rendered.WriteString(escapeStackTitle(line.Title))
		rendered.WriteByte('"')
	}
	rendered.WriteByte('\n')
	rendered.WriteString(compassStackEnd)
	return rendered.String()
}

// StripStack removes every complete Compass or jj-vine stack pair.
func StripStack(body string) string {
	for {
		clean, removed := stripStackPair(body, compassStackStart, compassStackEnd)
		if !removed {
			break
		}
		body = clean
	}
	for {
		clean, removed := stripStackPair(body, jjVineStackStart, jjVineStackEnd)
		if !removed {
			break
		}
		body = clean
	}
	return strings.TrimSpace(body)
}

// escapeStackTitle keeps a title from forming an HTML comment or a stack marker.
func escapeStackTitle(title string) string {
	return strings.ReplaceAll(title, "<", "&lt;")
}

// stripStackPair removes the first end-marker line and the nearest start-marker line above it.
// Markers count only as whole lines; an unmatched start marker stays as text.
func stripStackPair(body, startMarker, endMarker string) (string, bool) {
	start, end := -1, -1
	for offset := 0; offset < len(body); {
		lineEnd := strings.IndexByte(body[offset:], '\n')
		next := len(body)
		if lineEnd >= 0 {
			next = offset + lineEnd + 1
		}
		switch strings.TrimSpace(body[offset:next]) {
		case startMarker:
			start = offset
		case endMarker:
			if start >= 0 {
				end = next
			}
		}
		if end >= 0 {
			break
		}
		offset = next
	}
	if end < 0 {
		return body, false
	}

	before := body[:start]
	after := strings.TrimLeft(body[end:], "\r\n")
	if strings.HasSuffix(before, "\n\n---\n\n") {
		return before + after, true
	}
	before = strings.TrimRight(before, "\r\n")
	switch {
	case before == "":
		return after, true
	case after == "":
		return before, true
	default:
		return before + "\n\n" + after, true
	}
}

// StampStack replaces old stack blocks, then writes the new block below the owner header.
func StampStack(body, block string) string {
	clean := StripStack(body)
	if block == "" {
		return clean
	}

	if ownerHeader := fullBlockRe.FindStringIndex(clean); ownerHeader != nil && ownerHeader[0] == 0 {
		return clean[:ownerHeader[1]] + block + "\n\n" + clean[ownerHeader[1]:]
	}
	if clean == "" {
		return block
	}
	return block + "\n\n" + clean
}
