package forge

import (
	"strings"
	"testing"
)

func TestRenderStackMarksSelf(t *testing.T) {
	got := RenderStack([]StackLine{
		{Number: 41, Title: "First change"},
		{Number: 42, Title: "Second change"},
	}, 42)
	want := "<!-- compass:stack -->\n" +
		"This PR is part of a stack containing 2 PRs:\n" +
		"1. #41 \"First change\"\n" +
		"2. **\"Second change\" (this PR)**\n" +
		"<!-- /compass:stack -->"
	if got != want {
		t.Fatalf("RenderStack = %q, want %q", got, want)
	}
}

func TestStripStackRemovesAllPairs(t *testing.T) {
	body := "before\n\n" +
		"<!-- start jj-vine stack -->\nold one\n<!-- end jj-vine stack -->\n\n" +
		"<!-- compass:stack -->\nold two\n<!-- /compass:stack -->\n\n" +
		"<!-- start jj-vine stack -->\nold three\n<!-- end jj-vine stack -->\n\n" +
		"after"

	got := StripStack(body)
	if got != "before\n\nafter" {
		t.Fatalf("StripStack = %q, want %q", got, "before\n\nafter")
	}
}

func TestStripStackKeepsUnclosedMarkers(t *testing.T) {
	body := "before\n<!-- compass:stack -->unclosed\n" +
		"<!-- start jj-vine stack -->also unclosed"
	if got := StripStack(body); got != body {
		t.Fatalf("StripStack = %q, want unclosed markers preserved in %q", got, body)
	}
}

func TestStripStackKeepsLoneStartMarkerBeforeRealBlock(t *testing.T) {
	body := "intro\n<!-- compass:stack -->\nuser text\n\n" +
		"<!-- compass:stack -->\nold\n<!-- /compass:stack -->\n\nafter"
	want := "intro\n<!-- compass:stack -->\nuser text\n\nafter"
	if got := StripStack(body); got != want {
		t.Fatalf("StripStack = %q, want %q", got, want)
	}
}

func TestStripStackIgnoresInlineMarkers(t *testing.T) {
	body := "see `<!-- compass:stack -->` and `<!-- /compass:stack -->` in docs"
	if got := StripStack(body); got != body {
		t.Fatalf("StripStack = %q, want inline markers kept in %q", got, body)
	}
}

func TestStripStackDropsWholeEndMarkerLine(t *testing.T) {
	for _, body := range []string{
		"before\n\n<!-- compass:stack -->\nold\n<!-- /compass:stack --> \t\nafter",
		"before\r\n\r\n<!-- compass:stack -->\r\nold\r\n<!-- /compass:stack -->\t \r\nafter",
	} {
		if got := StripStack(body); got != "before\n\nafter" {
			t.Fatalf("StripStack(%q) = %q, want %q", body, got, "before\n\nafter")
		}
	}
}

func TestStampStackTitleWithEndMarkerStaysInBlock(t *testing.T) {
	block := RenderStack([]StackLine{
		{Number: 41, Title: "<!-- /compass:stack -->\ninjected"},
		{Number: 42, Title: "Current"},
	}, 42)
	once := StampStack("body", block)
	twice := StampStack(once, block)
	if once != twice || strings.Count(twice, "<!-- /compass:stack -->") != 1 || strings.Count(twice, "injected") != 1 {
		t.Fatalf("title closed the block early:\nonce:  %q\ntwice: %q", once, twice)
	}
}

func TestStampStackReplacesExistingPairs(t *testing.T) {
	body := "before\n\n" +
		"<!-- start jj-vine stack -->\nold one\n<!-- end jj-vine stack -->\n\n" +
		"<!-- compass:stack -->\nold two\n<!-- /compass:stack -->\n\n" +
		"<!-- start jj-vine stack -->\nold three\n<!-- end jj-vine stack -->\n\n" +
		"after"
	block := RenderStack([]StackLine{{Number: 42, Title: "Current"}}, 42)

	got := StampStack(body, block)
	if strings.Count(got, "<!-- compass:stack -->") != 1 || strings.Count(got, "<!-- /compass:stack -->") != 1 {
		t.Fatalf("StampStack did not leave exactly one Compass block: %q", got)
	}
	if strings.Contains(got, "jj-vine stack") || !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Fatalf("StampStack did not replace old blocks and preserve the body: %q", got)
	}
}

func TestStampStackIdempotentAndBelowOwnerHeader(t *testing.T) {
	body, err := StampOwner("body text", goldenAuthor, 0)
	if err != nil {
		t.Fatalf("StampOwner: %v", err)
	}
	block := RenderStack([]StackLine{{Number: 42, Title: "Current"}}, 42)

	once := StampStack(body, block)
	twice := StampStack(once, block)
	if once != twice {
		t.Fatalf("StampStack is not idempotent\nonce:  %q\ntwice: %q", once, twice)
	}
	if !strings.HasPrefix(once, "<!-- compass:owner v1 ") {
		t.Fatalf("owner header is not first: %q", once)
	}
	separator := "\n\n---\n\n"
	separatorAt := strings.Index(once, separator)
	if separatorAt < 0 {
		t.Fatalf("owner-header scaffold is missing: %q", once)
	}
	stackAt := strings.Index(once, "<!-- compass:stack -->")
	if stackAt < separatorAt+len(separator) {
		t.Fatalf("stack block is not below the owner header: %q", once)
	}
}
