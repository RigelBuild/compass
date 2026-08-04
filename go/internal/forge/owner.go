package forge

// The owner-header stamp/strip chokepoint (#995 Decision 2 + T3, DL-050). This
// is the ONLY non-test file in the module that carries the literal owner-header
// sentinel — the one place attribution is written and the one place it is read.
//
// The load-bearing security property: an agent CANNOT forge attribution.
// StampOwner strips any pre-existing header (of any version) from an agent body
// before writing exactly one header of its own at the top, so a hand-written
// header naming a victim never survives — last stamp wins, and only the Server
// stamps. On read, a parsed header is a plain display CLAIM only: a forge body is
// untrusted, no verification exists, and the returned Author MUST NOT reach any
// authz / routing / ownership decision (DL-094 supersedes the older "verified"
// language; DL-050 keeps display out of decisions).

import (
	"errors"
	"fmt"
	"regexp"
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

// handleGrammar is the grammar every header field (agent, owner, session) must
// match. It admits a leading lowercase letter or digit, then up to 38 more
// lowercase letters, digits, or dashes — so a hostile field can contain no
// space, no '>', no "-->" run beyond a bare dash, and no newline, and therefore
// cannot break out of the HTML comment it is interpolated into.
var handleGrammar = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}$`)

// commentRe matches one owner-header comment line (up to the first "-->" on the
// line). Used to COUNT headers — the exactly-one / duplicated distinction — and
// to detect a forged bare comment on write.
var commentRe = regexp.MustCompile(`<!-- compass:owner [^\n]*?-->`)

// fullBlockRe matches the complete stamped scaffold: the comment line, the
// human-readable attribution line, the blank line, the "---" rule, and the
// trailing blank line — everything StampOwner prepends before the agent body.
var fullBlockRe = regexp.MustCompile(`<!-- compass:owner [^\n]*?-->\n🧭 Written by [^\n]*\n\n---\n\n`)

// bareCommentRe matches a lone owner-header comment (a forged or human-mangled
// one without the scaffold) plus an immediately following newline, so removing
// it leaves no dangling blank line.
var bareCommentRe = regexp.MustCompile(`<!-- compass:owner [^\n]*?-->\n?`)

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
// stamps).
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
