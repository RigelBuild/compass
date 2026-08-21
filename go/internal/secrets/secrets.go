// Package secrets is the Server-side secret resolve surface for the agent
// container runtime (SEA-1327 T3). It wraps SecretSpec resolution behind a
// Resolver interface and owns the resolve-surface value types the Runner fetch
// (T4) and materializer (T5) consume.
//
// The split of concerns:
//   - internal/store owns the persisted NAMES registry (SecretDeclaration) —
//     which secrets are declared and how each is delivered/routed, never a
//     value.
//   - this package reads that registry, generates the SecretSpec manifest the
//     resolver resolves against, calls SecretSpec to resolve the actual values
//     from the configured provider (keyring/1Password/Vault/…), and hands back
//     ResolvedSecrets (name + value + content-hash version + delivery/kind).
//
// It maps store enums to its own resolve-surface enums at this edge, exactly as
// the comms service maps store↔proto (store/types.go) — so store stays a leaf
// and the two evolve independently. The dependency runs one way: secrets →
// store (no cycle).
//
// Values live only in the provider and this process's memory during a resolve;
// they are never persisted by Compass and never logged. Every value-bearing
// type here redacts under %s/%v/%#v (the store.Credentials pattern).
package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"

	"github.com/RigelBuild/compass/go/internal/store"
)

// DeliveryKind is how a resolved secret is delivered into a container — the
// load-bearing file-vs-env split that fixes how it rotates (T5/T6). It mirrors
// store.SecretDelivery; the two are mapped at this package's edge.
type DeliveryKind uint8

const (
	// DeliveryFile writes the secret to a 0600 file under the agent's scoped
	// $HOME — the rotatable delivery.
	DeliveryFile DeliveryKind = iota
	// DeliveryEnv delivers the secret through the aggregate 0600 env file each
	// wrapped exec reads at spawn.
	DeliveryEnv
)

// SecretKind is the routing class the T5 materializer switches on. It mirrors
// store.SecretKind; the two are mapped at this package's edge.
type SecretKind uint8

const (
	// SecretGeneric is a plain declared secret placed by DeliveryKind.
	SecretGeneric SecretKind = iota
	// SecretProvider is an LLM provider credential routed to the AuthStorage
	// seed; carries a Provider id.
	SecretProvider
	// SecretGH is a gh credential routed to ~/.config/gh/hosts.yml; carries a
	// Host.
	SecretGH
)

// nameGrammar is SecretSpec's env-var-name grammar. A declared secret name must
// match it: it becomes both a manifest key and, downstream, a path segment
// under $HOME/.compass/secrets/ and a token in a root-adjacent setup script
// (T5). Validated at the store door (store.DeclareSecret) and re-checked here as
// defense in depth before a name is ever emitted into a generated manifest.
var nameGrammar = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// maxNameLen caps a secret name's length. A name becomes an env-var name and a
// filesystem path segment (under $HOME/.compass/secrets/), so it must stay a
// sane segment; 255 bytes is the common path-segment limit. The write path is
// user-only (T7 RPC edge), so this bounds store bloat, not an exploit.
const maxNameLen = 255

// ValidateName reports whether name is a legal secret name (SecretSpec's
// env-var-name grammar: a leading letter or underscore, then letters, digits,
// or underscores). A name that fails — empty, leading digit, or containing a
// path separator, whitespace, newline, dash, or any other byte — is rejected so
// it can never become a manifest key, a filesystem path, or a script token.
func ValidateName(name string) error {
	if len(name) > maxNameLen {
		return fmt.Errorf("secrets: invalid name: length %d exceeds max %d", len(name), maxNameLen)
	}
	if !nameGrammar.MatchString(name) {
		return fmt.Errorf("secrets: invalid name %q: must match %s", name, nameGrammar.String())
	}
	return nil
}

// profileGrammar is the grammar a SecretSpec profile name must match to be a
// safe TOML bare key. A profile is operator-configured (WithProfile), but it is
// interpolated into the manifest's [profiles.<profile>] table header, so a value
// containing ']', '.', or a newline could restructure or invalidate the TOML.
// Restricting it to TOML bare-key bytes (letters, digits, underscore, dash)
// keeps the generated header well-formed regardless of configuration.
var profileGrammar = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ValidateProfile reports whether profile is a legal SecretSpec profile name: a
// non-empty run of letters, digits, underscores, or dashes. Anything else — a
// bracket, dot, whitespace, or newline — is rejected so it can never corrupt the
// [profiles.<profile>] table header of a generated manifest.
func ValidateProfile(profile string) error {
	if !profileGrammar.MatchString(profile) {
		return fmt.Errorf("secrets: invalid profile %q: must match %s", profile, profileGrammar.String())
	}
	return nil
}

// Version is the content-hash version of a resolved secret value: the SHA-256
// of the value, hex-encoded. The registry stores no values and SecretSpec
// resolve returns values (not versions), so a content hash is the only
// deterministic version producer. A same-value re-set hashes identically, so
// T6's rotation diff sees no change and does nothing — correct, since nothing
// the container holds is stale.
//
// It is NEVER logged: String/GoString redact the value AND omit the version, so
// the hash cannot serve as an offline confirmation oracle for a low-entropy
// secret. T6 diffs the struct field directly, not a log line, so dropping it
// from the log surface costs nothing. (A keyed hash — HMAC under a server key —
// is a post-MVP defense-in-depth option, redundant once the version is unlogged;
// it would amend the frozen SHA-256 algorithm, so it is deferred, not folded.)
func Version(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// ResolvedSecret is one secret resolved from the registry plus the provider: a
// declared name, its resolved value, the content-hash version, and the
// delivery/kind routing the materializer needs. Value is redacted under every
// fmt verb so a struct dump, log line, or formatted error can never leak it.
type ResolvedSecret struct {
	Name    string
	Value   string
	Version string
	// Delivery is the file-vs-env split (T5/T6 rotation shape).
	Delivery DeliveryKind
	// Kind is the materializer routing class.
	Kind SecretKind
	// Host is the forge host, set only for SecretGH (else "").
	Host string
	// Provider is the SDK provider id, set only for SecretProvider (else "").
	Provider string
}

// String redacts Value AND omits Version so a resolved secret never lands in a
// log line or a formatted error; the version is a hash of the value, so logging
// it would be a confirmation oracle for a low-entropy secret. Only the routing
// metadata (safe to show) is kept for diagnosis.
func (s ResolvedSecret) String() string {
	return fmt.Sprintf("ResolvedSecret{name: %q, kind: %d, delivery: %d, value: <redacted>}",
		s.Name, s.Kind, s.Delivery)
}

// GoString redacts Value under %#v as well, so a struct dump can't leak it.
func (s ResolvedSecret) GoString() string { return s.String() }

// deliveryFromStore maps the persisted store delivery enum to this package's.
func deliveryFromStore(d store.SecretDelivery) DeliveryKind {
	if d == store.SecretDeliveryEnv {
		return DeliveryEnv
	}
	return DeliveryFile
}

// kindFromStore maps the persisted store kind enum to this package's.
func kindFromStore(k store.SecretKind) SecretKind {
	switch k {
	case store.SecretKindProvider:
		return SecretProvider
	case store.SecretKindGH:
		return SecretGH
	default:
		return SecretGeneric
	}
}
