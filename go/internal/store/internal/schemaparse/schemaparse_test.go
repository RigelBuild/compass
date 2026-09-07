package schemaparse

import (
	"fmt"
	"strings"
	"testing"
)

// TestIsLiteralTrue pins both directions of the credential-marker test.
//
// The asymmetry matters: a false NEGATIVE (a real marker read as false) drops a
// path from the door's denylist, and a false POSITIVE marks a path the SDK never
// marked. Both are silent, so every shape below is pinned explicitly rather than
// left to the bare string compare this replaced.
func TestIsLiteralTrue(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want bool
	}{
		// The bare literal, and the decorations ordinary authoring adds to it.
		// Each of these was silently dropped by the previous `== "true"` compare.
		{"bare literal", "true", true},
		{"trailing line comment", "true // yes", true},
		{"trailing block comment", "true /* yes */", true},
		{"as const assertion", "true as const", true},
		{"as const then comment", "true as const // yes", true},
		{"as const multi space", "true  as   const", true},
		{"as const tab delimited", "true as\tconst", true},

		// Not the literal. `trueconst` is a regression guard: stripping the
		// "const" suffix without requiring a delimiter turned this identifier
		// into a false positive.
		{"identifier ending in const", "trueconst", false},
		{"as const with no delimiters", "trueasconst", false},
		{"quoted string", `"true"`, false},
		{"incomplete assertion", "true as", false},
		{"misspelled assertion", "true as cons", false},
		{"widening assertion", "true as unknown as boolean", false},
		{"identifier prefixed by true", "truex", false},
		{"identifier containing true", "untrue", false},
		{"capitalized", "True", false},
		{"the false literal", "false", false},
		{"false with a true comment", "false // true", false},
		{"helper call", `secretUi("Label")`, false},
		{"bare variable", "CREDENTIAL", false},
		{"empty", "", false},

		// A nested object arrives as a map, never a string.
		{"non-string value", map[string]any{"secret": "true"}, false},
		{"nil value", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsLiteralTrue(tc.in); got != tc.want {
				t.Fatalf("IsLiteralTrue(%#v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// schemaSrc wraps defs in the minimal shape ExtractCredentialKeys seeks.
func schemaSrc(defs string) string {
	return "export const SETTINGS_SCHEMA = {\n" + defs + "\n} as const;\n"
}

// TestExtractCredentialKeys covers the two marker channels isCredential uses
// (`credential: true` at the def level, `ui.secret === true`) and the shapes
// that must NOT be collected.
func TestExtractCredentialKeys(t *testing.T) {
	cases := []struct {
		name string
		defs string
		want []string
	}{
		{
			name: "def-level credential marker",
			defs: `	"a.token": { type: "string", default: "", credential: true },
	"b.plain": { type: "string", default: "" },`,
			want: []string{"a.token"},
		},
		{
			name: "ui secret marker",
			defs: `	"a.key": { type: "string", ui: { secret: true } },
	"b.plain": { type: "string", ui: { secret: false } },`,
			want: []string{"a.key"},
		},
		{
			name: "marker decorated with a comment is still collected",
			defs: `	"a.token": { type: "string", credential: true // keep secret
	},`,
			want: []string{"a.token"},
		},
		{
			name: "a quoted marker value is not a marker",
			defs: `	"a.token": { type: "string", credential: "true" },`,
			want: nil,
		},
		{
			name: "generic cast commas do not split defs",
			defs: `	"a.map": { default: {} as Partial<Record<string, string>>, credential: true },
	"b.plain": { type: "string" },`,
			want: []string{"a.map"},
		},
		{
			name: "record-valued credential leaf",
			defs: `	"images.urls.credentials": { type: "record", default: {}, credential: true },`,
			want: []string{"images.urls.credentials"},
		},
		{
			name: "nested ui object without secret is not collected",
			defs: `	"a.plain": { type: "string", ui: { label: "A" } },`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, total, err := ExtractCredentialKeys(schemaSrc(tc.defs))
			if err != nil {
				t.Fatalf("ExtractCredentialKeys: %v", err)
			}
			if strings.Join(sorted(got), ",") != strings.Join(tc.want, ",") {
				t.Fatalf("keys = %v, want %v", got, tc.want)
			}
			if total == 0 {
				t.Fatal("total top-level key count = 0, want the defs walked")
			}
		})
	}
}

// TestExtractCredentialKeysReportsTotal pins the count the generator's
// plausibility floor is computed from: it must be every top-level path walked,
// not just the credential-marked ones.
func TestExtractCredentialKeysReportsTotal(t *testing.T) {
	defs := make([]string, 0, 41)
	for i := range 40 {
		defs = append(defs, fmt.Sprintf("\t%q: { type: \"string\" },", fmt.Sprintf("p%d.value", i)))
	}
	defs = append(defs, `	"z.token": { type: "string", credential: true },`)
	keys, total, err := ExtractCredentialKeys(schemaSrc(strings.Join(defs, "\n")))
	if err != nil {
		t.Fatalf("ExtractCredentialKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("credential keys = %v, want exactly 1", keys)
	}
	if total != 41 {
		t.Fatalf("total = %d, want 41 (every top-level path, not just credentials)", total)
	}
}

// TestExtractCredentialKeysErrors pins the loud-failure paths: a malformed or
// absent schema must error rather than return a short list, since a short list
// would silently shrink the door's denylist.
func TestExtractCredentialKeysErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"schema symbol absent", "export const OTHER = {};\n"},
		{"no assignment", "export const SETTINGS_SCHEMA;\n"},
		{"unterminated object", "export const SETTINGS_SCHEMA = {\n\t\"a.token\": { credential: true },\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ExtractCredentialKeys(tc.src); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
