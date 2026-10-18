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
