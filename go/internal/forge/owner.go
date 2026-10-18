package forge

// This file is the sole non-test owner-header stamp/strip chokepoint.

// StampOwner replaces existing headers before stamping. Parsed headers are
// display claims only and must never affect authorization or routing.

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Author is the attribution a stamped body carries: the Compass agent handle,
// the human owner it acts for, and the session it ran in. VERBATIM #995:1677.
// It carries NO verified bit — a parsed Author is a display claim, never a
// checked fact (DL-094).
type Author struct{ AgentHandle, OwnerHandle, SessionID string }

// ErrBodyTooLarge is returned when len(body) > bodyLimit - len(header): the
// stamped header's bytes are reserved against the provider's limit FIRST, so an
// over-limit agent body is a hard error, never a silently-truncated unattributed
// artifact. The Service maps it to an in-band error naming the overage so the
// model shortens and retries.
var ErrBodyTooLarge = errors.New("forge: body exceeds the limit once the owner header is reserved")

// handleGrammar prevents fields from containing whitespace, comment delimiters,
// or newlines that could escape the stamped HTML comment.
var handleGrammar = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}$`)

// commentRe matches one owner-header comment line (up to the first "-->" on the
// line). Used to COUNT headers — the exactly-one / duplicated distinction — and
// to detect a forged bare comment on write.
var commentRe = regexp.MustCompile(`<!-- compass:owner [^\n]*?-->`)

// fullBlockRe matches the stamped scaffold. Its separators accept LF and CRLF
// because forge bodies can use CRLF.
var fullBlockRe = regexp.MustCompile(`<!-- compass:owner [^\n]*?-->\r?\n🧭 Written by [^\n]*\r?\n\r?\n---\r?\n\r?\n`)

// bareCommentRe matches a lone owner-header comment (a forged or human-mangled
// one without the scaffold) plus an immediately following newline, so removing
// it leaves no dangling blank line. The `\r?\n?` trailer tolerates both LF and
// CRLF line endings (forge bodies arrive CRLF).
var bareCommentRe = regexp.MustCompile(`<!-- compass:owner [^\n]*?-->\r?\n?`)

// parseRe extracts the version and the three fields from a single well-formed
// comment. Fields are captured as non-space runs; a field carrying a space (a
// mangled header) fails the whole match, so no partial parse is possible.
var parseRe = regexp.MustCompile(`<!-- compass:owner (\S+) agent=(\S+) owner=(\S+) session=(\S+) -->`)

// header builds the byte-exact owner-header scaffold for author (#995:531-538):
// the sentinel comment, the human-readable attribution line, a blank line, the
// "---" rule, and a trailing blank line — the agent body is appended after it.
func header(author Author) string {
	return "<!-- compass:owner v1 agent=" + author.AgentHandle +
		" owner=" + author.OwnerHandle +
		" session=" + author.SessionID + " -->\n" +
		"🧭 Written by **@" + author.AgentHandle +
		"** (Compass agent, owned by **@" + author.OwnerHandle + "**)\n\n---\n\n"
}

// MaxOwnerHeaderLen is the longest header StampOwner can prepend for session, so a
// caller can budget body bytes before stamping with handles it has not resolved yet.
func MaxOwnerHeaderLen(session string) int {
	longest := strings.Repeat("a", 39)
	return len(header(Author{AgentHandle: longest, OwnerHandle: longest, SessionID: session}))
}

// validateField reports whether a header field is legal, naming the field in the
// error so a refusal is diagnosable.
func validateField(name, value string) error {
	if !handleGrammar.MatchString(value) {
		return fmt.Errorf("forge: owner-header %s %q violates the field grammar %s", name, value, handleGrammar)
	}
	return nil
}

// removeBlocks strips every owner-header block from body — the full scaffold
// where present, then any lone forged/mangled comment — and reports how many
// header comments the original body carried.
func removeBlocks(body string) (clean string, count int) {
	count = len(commentRe.FindAllStringIndex(body, -1))
	clean = fullBlockRe.ReplaceAllString(body, "")
	clean = bareCommentRe.ReplaceAllString(clean, "")
	return clean, count
}

// StampOwner returns body with exactly one owner header at the TOP, removing any
// pre-existing owner block (of ANY version) first. Idempotent:
// StampOwner(StampOwner(b,a,n),a,n) == StampOwner(b,a,n). Re-stamping with a
// DIFFERENT author REPLACES (never appends) — exactly one header in the output.
// bodyLimit is the provider's max body size: the stamped header's bytes are
// RESERVED against it FIRST, so an over-limit agent body is an error, never a
// silently-truncated unattributed artifact. bodyLimit <= 0 means unlimited.
// Refuses (returns an error + EMPTY string) rather than escaping when a field
// violates its grammar. This strip-then-stamp is THE load-bearing security
// property: an agent cannot forge attribution (last stamp wins, only the Server
// stamps). A consequence of this one-chokepoint / refuse-to-choose design: any
// `<!-- compass:owner ... -->` comment appearing in the agent's own content is
// also removed by the strip — the Server never distinguishes a forged header
// from a coincidental one, it simply strips all of them and writes its own.
func StampOwner(body string, author Author, bodyLimit int) (string, error) {
	if err := validateField("agent", author.AgentHandle); err != nil {
		return "", err
	}
	if err := validateField("owner", author.OwnerHandle); err != nil {
		return "", err
	}
	if err := validateField("session", author.SessionID); err != nil {
		return "", err
	}

	// Strip any pre-existing/forged header BEFORE reserving and writing our own.
	clean, _ := removeBlocks(body)

	hdr := header(author)
	if bodyLimit > 0 && len(clean) > bodyLimit-len(hdr) {
		return "", ErrBodyTooLarge
	}
	return hdr + clean, nil
}

// StripOwner removes EVERY owner block from body and returns the parsed Author
// ONLY when exactly ONE well-formed, understood-version (v1) header was present.
// ok=false for missing, mangled, future-versioned, OR DUPLICATED (>1) headers —
// never a partial parse, never a choice between competing claims. ok=true means
// "one well-formed header was present", NOT "this author wrote this body": a
// forge body is untrusted, so the returned Author is a display CLAIM only
// (DL-050 / DL-094 — it never reaches an authz/routing/ownership decision).
func StripOwner(body string) (clean string, author Author, ok bool) {
	clean, count := removeBlocks(body)
	if count != 1 {
		return clean, Author{}, false
	}

	m := parseRe.FindStringSubmatch(body)
	if m == nil {
		return clean, Author{}, false
	}
	version, agent, owner, session := m[1], m[2], m[3], m[4]

	// Forward-compat: an unknown version means "not attributable by me", never a
	// guess.
	if version != "v1" {
		return clean, Author{}, false
	}
	// Defense in depth: a field that somehow slipped past the write door must
	// still satisfy the grammar to be surfaced as a claim.
	if validateField("agent", agent) != nil ||
		validateField("owner", owner) != nil ||
		validateField("session", session) != nil {
		return clean, Author{}, false
	}
	return clean, Author{AgentHandle: agent, OwnerHandle: owner, SessionID: session}, true
}
