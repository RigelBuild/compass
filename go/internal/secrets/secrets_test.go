package secrets

// Pure contracts for the secret resolve surface: the name grammar that gates
// what can become a manifest key / filesystem path / script token, the
// content-hash version producer, the D14 value-redaction guard on every fmt
// verb, and the store→resolve enum mapping at this package's edge. No Postgres,
// no FFI resolver — all of this is a pure function of its inputs.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

func TestValidateName(t *testing.T) {
	valid := []string{"API_KEY", "_x", "a", "Db1", "DATABASE_URL", "X"}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil (valid name)", name, err)
		}
	}

	// Empty, leading digit, and any byte outside [A-Za-z0-9_] (dash, path
	// separator, whitespace, newline, tab, dot, traversal, non-ASCII) must be
	// rejected — the name later becomes a path segment and a script token.
	invalid := []string{
		"",
		"1abc",
		"a-b",
		"a/b",
		"a b",
		"../x",
		"a\nb",
		"a\tb",
		"café",
		"a.b",
		".",
		"$X",
		"a$b",
	}
	for _, name := range invalid {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error (invalid name)", name)
		}
	}

	// The 255-byte cap (maxNameLen): a name of exactly 255 valid chars is
	// accepted, 256 is rejected. Boundary must be exact — 255 in, 256 out.
	at255 := "A" + strings.Repeat("a", 254)
	if err := ValidateName(at255); err != nil {
		t.Errorf("ValidateName(255 valid chars) = %v, want nil (at cap)", err)
	}
	over := "A" + strings.Repeat("a", 255)
	if err := ValidateName(over); err == nil {
		t.Errorf("ValidateName(256 valid chars) = nil, want an error (over cap)")
	}
}

func TestVersion(t *testing.T) {
	const value = "super-secret-xyz"

	// Deterministic: a same-value re-set hashes identically, so T6's rotation
	// diff sees no change.
	first, second := Version(value), Version(value)
	if first != second {
		t.Fatal("Version is not deterministic for the same value")
	}
	// Distinct values hash distinctly.
	if Version(value) == Version(value+"!") {
		t.Fatal("distinct values produced the same version hash")
	}
	// 64-char lowercase hex (SHA-256 is 32 bytes), for a few values incl. the
	// empty string.
	for _, v := range []string{value, "", "another"} {
		h := Version(v)
		if len(h) != 64 {
			t.Errorf("Version(%q) length = %d, want 64", v, len(h))
		}
		if strings.ToLower(h) != h {
			t.Errorf("Version(%q) = %q, want lowercase hex", v, h)
		}
		for _, c := range h {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Errorf("Version(%q) = %q, contains non-hex rune %q", v, h, c)
				break
			}
		}
	}
	// Positively pin the frozen SHA-256 algorithm: the version of "" is the
	// bare SHA-256 of the empty string. Safe to pin now that the version is
	// never logged (no confirmation-oracle exposure).
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := Version(""); got != emptySHA256 {
		t.Errorf("Version(\"\") = %q, want %q (frozen SHA-256 of empty)", got, emptySHA256)
	}
}

func TestResolvedSecretRedaction(t *testing.T) {
	const secret = "super-secret-xyz"
	rs := ResolvedSecret{
		Name:     "DATABASE_URL",
		Value:    secret,
		Version:  Version(secret),
		Delivery: DeliveryEnv,
		Kind:     SecretGeneric,
	}

	// D14: the value must never appear under ANY formatting path — a struct
	// dump, a log line, or a formatted error.
	renders := map[string]string{
		//nolint:staticcheck // S1025: deliberately formats via the %s verb (not a direct String() call) to assert the Stringer dispatch path redacts.
		"%s":          fmt.Sprintf("%s", rs),
		"%v":          fmt.Sprintf("%v", rs),
		"%#v":         fmt.Sprintf("%#v", rs),
		"%+v":         fmt.Sprintf("%+v", rs),
		".String()":   rs.String(),
		".GoString()": rs.GoString(),
		"slice %v":    fmt.Sprintf("%v", []ResolvedSecret{rs}),
		"pointer %v":  fmt.Sprintf("%v", &rs),
	}
	for path, out := range renders {
		if strings.Contains(out, secret) {
			t.Errorf("%s leaked the value: %q", path, out)
		}
		// Safe metadata IS kept for diagnosis.
		if !strings.Contains(out, "DATABASE_URL") {
			t.Errorf("%s dropped the name (want it kept for diagnosis): %q", path, out)
		}
		// The version is a hash of the value, so it must NOT appear on the log
		// surface (String/GoString omit it). rs.Version is a 64-hex SHA-256 of a
		// non-empty secret, so it is non-empty — a false "not contains" pass is
		// impossible.
		if strings.Contains(out, rs.Version) {
			t.Errorf("%s leaked the version (must NOT appear on the log surface): %q", path, out)
		}
		if !strings.Contains(out, "redacted") {
			t.Errorf("%s missing the <redacted> marker: %q", path, out)
		}
	}
}

func TestEnumMappingFromStore(t *testing.T) {
	deliveries := map[store.SecretDelivery]DeliveryKind{
		store.SecretDeliveryFile: DeliveryFile,
		store.SecretDeliveryEnv:  DeliveryEnv,
	}
	for in, want := range deliveries {
		if got := deliveryFromStore(in); got != want {
			t.Errorf("deliveryFromStore(%d) = %d, want %d", in, got, want)
		}
	}

	kinds := map[store.SecretKind]SecretKind{
		store.SecretKindGeneric:  SecretGeneric,
		store.SecretKindProvider: SecretProvider,
		store.SecretKindGH:       SecretGH,
	}
	for in, want := range kinds {
		if got := kindFromStore(in); got != want {
			t.Errorf("kindFromStore(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestValidateProfile(t *testing.T) {
	valid := []string{"default", "production", "staging", "dev-1", "a_b", "X"}
	for _, p := range valid {
		if err := ValidateProfile(p); err != nil {
			t.Errorf("ValidateProfile(%q) = %v, want nil (valid profile)", p, err)
		}
	}
	// Bytes that could restructure or invalidate the [profiles.<profile>] TOML
	// header must be rejected: brackets, dots, whitespace, newline, quotes.
	invalid := []string{"", "a.b", "a]b", "[x]", "a b", "a\nb", "a\tb", `a"b`, "a/b", "café"}
	for _, p := range invalid {
		if err := ValidateProfile(p); err == nil {
			t.Errorf("ValidateProfile(%q) = nil, want an error (invalid profile)", p)
		}
	}
}
