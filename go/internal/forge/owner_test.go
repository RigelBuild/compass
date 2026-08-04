package forge

// The security core: the owner-header stamp/strip chokepoint (#995 Decision 2 +
// T3, DL-050). Every case here defends a real contract — the load-bearing
// property is that an agent cannot forge attribution (strip-then-stamp, last
// stamp wins, only the Server stamps) and that a parsed header is a plain
// display CLAIM, never a verified fact (DL-094). No network, no I/O: pure
// functions of their inputs (the source-guard walk excepted).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenAuthor / goldenBody / goldenStamped are the byte-exact fixture from
// #995 design.md:531-538 — the golden bytes the format must reproduce.
var goldenAuthor = Author{AgentHandle: "atlas", OwnerHandle: "matt", SessionID: "sess-7f3a9c1e"}

const goldenBody = "Hello, world.\n\nSecond paragraph."

const goldenStamped = "<!-- compass:owner v1 agent=atlas owner=matt session=sess-7f3a9c1e -->\n" +
	"🧭 Written by **@atlas** (Compass agent, owned by **@matt**)\n" +
	"\n" +
	"---\n" +
	"\n" +
	goldenBody

// 1. Golden bytes: the fixture reproduces the exact expected string.
func TestStampOwnerGoldenBytes(t *testing.T) {
	got, err := StampOwner(goldenBody, goldenAuthor, 0)
	if err != nil {
		t.Fatalf("StampOwner returned error: %v", err)
	}
	if got != goldenStamped {
		t.Errorf("golden mismatch\n got: %q\nwant: %q", got, goldenStamped)
	}
	// Structural assertions so a regression names the missing part.
	if !strings.Contains(got, "\n\n---\n\n") {
		t.Error("stamped body missing the blank/rule/blank separator")
	}
	if !strings.HasPrefix(got, "<!-- compass:owner v1 ") {
		t.Error("stamped body missing the top-placed owner comment")
	}
	if !strings.Contains(got, "🧭 Written by **@atlas**") {
		t.Error("stamped body missing the human-readable attribution line")
	}
}

// 2. Idempotence + replacement: double-stamp == single-stamp; re-stamp with a
// different author REPLACES (exactly one header in output, never appends).
func TestStampOwnerIdempotentAndReplaces(t *testing.T) {
	once, err := StampOwner(goldenBody, goldenAuthor, 0)
	if err != nil {
		t.Fatalf("first stamp: %v", err)
	}
	twice, err := StampOwner(once, goldenAuthor, 0)
	if err != nil {
		t.Fatalf("second stamp: %v", err)
	}
	if once != twice {
		t.Errorf("StampOwner not idempotent\nonce:  %q\ntwice: %q", once, twice)
	}

	other := Author{AgentHandle: "nomad", OwnerHandle: "alex", SessionID: "sess-00000000"}
	replaced, err := StampOwner(once, other, 0)
	if err != nil {
		t.Fatalf("re-stamp with different author: %v", err)
	}
	if n := strings.Count(replaced, "<!-- compass:owner"); n != 1 {
		t.Fatalf("re-stamp produced %d headers, want exactly 1", n)
	}
	if strings.Contains(replaced, "agent=atlas") {
		t.Error("re-stamp did not replace the prior author (agent=atlas still present)")
	}
	if !strings.Contains(replaced, "agent=nomad") {
		t.Error("re-stamp did not install the new author (agent=nomad missing)")
	}
}

// 3. Forgery on WRITE (load-bearing): an agent body that hand-writes a header
// naming a victim comes out stamped for the CALLING agent, the forged claim gone.
func TestStampOwnerStripsForgedHeaderOnWrite(t *testing.T) {
	forged := "<!-- compass:owner v1 agent=victim owner=someone session=x -->\nreal agent content"
	got, err := StampOwner(forged, goldenAuthor, 0)
	if err != nil {
		t.Fatalf("StampOwner: %v", err)
	}
	if strings.Contains(got, "agent=victim") {
		t.Error("forged agent=victim survived the stamp")
	}
	if n := strings.Count(got, "<!-- compass:owner"); n != 1 {
		t.Fatalf("output has %d headers, want exactly 1 (the caller's)", n)
	}
	if !strings.Contains(got, "agent=atlas") {
		t.Error("caller's header (agent=atlas) not installed")
	}
	if !strings.Contains(got, "real agent content") {
		t.Error("agent's real content lost")
	}
}

// 4. Forgery on READ — containment: a body the Server never wrote, carrying a
// well-formed header for another agent, parses as ok=true but is a display CLAIM
// only (no verified concept exists — DL-094). The type carries no verified bit.
func TestStripOwnerReadClaimIsDisplayOnly(t *testing.T) {
	planted := "<!-- compass:owner v1 agent=impostor owner=boss session=sess-deadbeef -->\n" +
		"🧭 Written by **@impostor** (Compass agent, owned by **@boss**)\n\n---\n\nbody the server never stamped"
	clean, author, ok := StripOwner(planted)
	if !ok {
		t.Fatal("well-formed header not recognized as a (display-only) claim")
	}
	if author.AgentHandle != "impostor" || author.OwnerHandle != "boss" || author.SessionID != "sess-deadbeef" {
		t.Errorf("claim not surfaced verbatim: %+v", author)
	}
	if clean != "body the server never stamped" {
		t.Errorf("clean = %q, want the raw body", clean)
	}
}

// 5. Two headers: duplicated blocks → ok=false, author unset, and clean contains
// NEITHER block (not first-wins, not last-wins — refuse to choose).
func TestStripOwnerTwoHeadersRefused(t *testing.T) {
	a, _ := StampOwner("first", goldenAuthor, 0)
	other := Author{AgentHandle: "nomad", OwnerHandle: "alex", SessionID: "sess-11111111"}
	// Concatenate two full blocks (a hostile / corrupted body).
	b := a + "\n" + mustStamp(t, "second", other)
	clean, author, ok := StripOwner(b)
	if ok {
		t.Error("two headers accepted; want ok=false")
	}
	if author != (Author{}) {
		t.Errorf("author must be unset on refusal, got %+v", author)
	}
	if strings.Contains(clean, "<!-- compass:owner") {
		t.Errorf("clean still contains an owner block: %q", clean)
	}
}

// 6. Human edits: prose edited around an intact header parses; a deleted header
// yields ok=false; a mangled header yields ok=false — never a partial parse.
func TestStripOwnerHumanEdits(t *testing.T) {
	stamped, _ := StampOwner("original prose", goldenAuthor, 0)

	// Prose edited (after the rule) but header intact → still parses.
	edited := strings.Replace(stamped, "original prose", "edited prose by a human", 1)
	if _, _, ok := StripOwner(edited); !ok {
		t.Error("intact header with edited prose failed to parse")
	}

	// Header deleted entirely → ok=false.
	if _, _, ok := StripOwner("just prose, no header at all"); ok {
		t.Error("body with no header parsed ok=true")
	}

	// Header mangled (stray space breaks the field grammar) → ok=false.
	mangled := strings.Replace(stamped, "agent=atlas", "agent=at las", 1)
	if _, _, ok := StripOwner(mangled); ok {
		t.Error("mangled header parsed ok=true")
	}
}

// 7. Forward compat: a v2 (unknown) header → ok=false, author unset. Unknown
// version means 'not attributable by me', never a guess.
func TestStripOwnerFutureVersionRefused(t *testing.T) {
	future := "<!-- compass:owner v2 agent=atlas owner=matt session=sess-7f3a9c1e -->\n" +
		"🧭 Written by **@atlas** (Compass agent, owned by **@matt**)\n\n---\n\nbody"
	_, author, ok := StripOwner(future)
	if ok {
		t.Error("v2 header parsed ok=true; want forward-compat refusal")
	}
	if author != (Author{}) {
		t.Errorf("author must be unset for an unknown version, got %+v", author)
	}
}

// 8. Grammar refusal (case table): a field containing a forbidden byte → error
// AND empty output string (never a stamped body).
func TestStampOwnerGrammarRefusal(t *testing.T) {
	forbidden := []struct {
		name  string
		field string
	}{
		{"space", "bad handle"},
		{"gt", "bad>handle"},
		{"comment-close", "bad-->x"},
		{"newline", "bad\nhandle"},
		{"uppercase", "BadHandle"},
		{"empty", ""},
	}
	for _, tc := range forbidden {
		t.Run("agent/"+tc.name, func(t *testing.T) {
			out, err := StampOwner("body", Author{AgentHandle: tc.field, OwnerHandle: "matt", SessionID: "sess-1"}, 0)
			if err == nil {
				t.Errorf("agent=%q accepted; want an error", tc.field)
			}
			if out != "" {
				t.Errorf("agent=%q returned non-empty output %q; want empty", tc.field, out)
			}
		})
		t.Run("owner/"+tc.name, func(t *testing.T) {
			out, err := StampOwner("body", Author{AgentHandle: "atlas", OwnerHandle: tc.field, SessionID: "sess-1"}, 0)
			if err == nil {
				t.Errorf("owner=%q accepted; want an error", tc.field)
			}
			if out != "" {
				t.Errorf("owner=%q returned non-empty output %q; want empty", tc.field, out)
			}
		})
		t.Run("session/"+tc.name, func(t *testing.T) {
			out, err := StampOwner("body", Author{AgentHandle: "atlas", OwnerHandle: "matt", SessionID: tc.field}, 0)
			if err == nil {
				t.Errorf("session=%q accepted; want an error", tc.field)
			}
			if out != "" {
				t.Errorf("session=%q returned non-empty output %q; want empty", tc.field, out)
			}
		})
	}
}

// 9. Byte budget: a body of exactly bodyLimit-len(header) stamps; one byte more
// → ErrBodyTooLarge and empty output. The header is reserved FIRST.
func TestStampOwnerByteBudget(t *testing.T) {
	// The reserved header length for this author = the output of stamping an
	// empty body under an unlimited budget.
	headerOnly, err := StampOwner("", goldenAuthor, 0)
	if err != nil {
		t.Fatalf("empty-body stamp: %v", err)
	}
	headerLen := len(headerOnly)

	const slack = 16
	limit := headerLen + slack

	atCap := strings.Repeat("x", slack)
	if _, err := StampOwner(atCap, goldenAuthor, limit); err != nil {
		t.Errorf("body at exactly the cap rejected: %v", err)
	}

	over := strings.Repeat("x", slack+1)
	out, err := StampOwner(over, goldenAuthor, limit)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("one byte over: err = %v, want ErrBodyTooLarge", err)
	}
	if out != "" {
		t.Errorf("over-limit returned non-empty output %q; want empty", out)
	}
}

// 10. Round trip: StripOwner(StampOwner(b,a,0)) returns b and a exactly.
func TestOwnerRoundTrip(t *testing.T) {
	stamped, err := StampOwner(goldenBody, goldenAuthor, 0)
	if err != nil {
		t.Fatalf("StampOwner: %v", err)
	}
	clean, author, ok := StripOwner(stamped)
	if !ok {
		t.Fatal("round-trip StripOwner ok=false")
	}
	if clean != goldenBody {
		t.Errorf("round-trip body = %q, want %q", clean, goldenBody)
	}
	if author != goldenAuthor {
		t.Errorf("round-trip author = %+v, want %+v", author, goldenAuthor)
	}
}

// 11. Source guard: the literal owner sentinel appears in exactly ONE non-test
// file in the module — owner.go — the one-chokepoint constraint.
func TestOwnerSentinelSingleChokepoint(t *testing.T) {
	// Build the sentinel without writing it as one literal token, so scanning
	// this very test file for it does not itself become a chokepoint concern.
	sentinel := "compass" + ":owner"

	moduleRoot := filepath.Join("..", "..") // internal/forge -> go/ (module root)
	var hits []string
	err := filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), sentinel) {
			hits = append(hits, filepath.Base(path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module tree: %v", err)
	}
	if len(hits) != 1 || hits[0] != "owner.go" {
		t.Fatalf("sentinel %q found in %v non-test file(s); want exactly [owner.go]", sentinel, hits)
	}
}

// 12. CRLF round trip: a forge that normalizes bodies to CRLF must still be
// fully stripped. The load-bearing assertion is that the 'clean' body handed to
// the model contains NO residual attribution scaffold (🧭, sentinel, or rule).
func TestStripOwnerCRLFRoundTrip(t *testing.T) {
	stamped := mustStamp(t, goldenBody, goldenAuthor)
	crlf := strings.ReplaceAll(stamped, "\n", "\r\n")

	clean, author, ok := StripOwner(crlf)
	if !ok {
		t.Fatalf("StripOwner(CRLF) ok = false, want true")
	}
	if author != goldenAuthor {
		t.Errorf("author = %+v, want %+v", author, goldenAuthor)
	}
	if want := strings.ReplaceAll(goldenBody, "\n", "\r\n"); clean != want {
		t.Errorf("clean mismatch\n got: %q\nwant: %q", clean, want)
	}
	// Load-bearing: no residual attribution scaffold leaks into the clean body.
	if strings.Contains(clean, "🧭") {
		t.Errorf("clean still contains the 🧭 attribution line: %q", clean)
	}
	if strings.Contains(clean, "<!-- compass:owner") {
		t.Errorf("clean still contains the owner sentinel comment: %q", clean)
	}
	if strings.Contains(clean, "---") {
		t.Errorf("clean still contains the '---' rule line: %q", clean)
	}
}

// 13. CRLF idempotence: re-stamping a CRLF-stamped body leaves EXACTLY ONE
// header — no stale accumulated block survives the strip on CRLF line endings.
func TestStampOwnerCRLFIdempotent(t *testing.T) {
	stamped := mustStamp(t, goldenBody, goldenAuthor)
	crlf := strings.ReplaceAll(stamped, "\n", "\r\n")

	got, err := StampOwner(crlf, goldenAuthor, 0)
	if err != nil {
		t.Fatalf("StampOwner(CRLF): %v", err)
	}
	if n := strings.Count(got, "<!-- compass:owner"); n != 1 {
		t.Errorf("owner comment count = %d, want 1 (stale block accumulated): %q", n, got)
	}
	if n := strings.Count(got, "🧭 Written by"); n != 1 {
		t.Errorf("attribution line count = %d, want 1 (stale block accumulated): %q", n, got)
	}
}

func mustStamp(t *testing.T, body string, a Author) string {
	t.Helper()
	out, err := StampOwner(body, a, 0)
	if err != nil {
		t.Fatalf("mustStamp(%q): %v", body, err)
	}
	return out
}
