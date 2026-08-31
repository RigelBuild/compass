package store

// Config-bundle DOOR contracts (RIG-1624 T1), default gate — pure functions
// over []byte, no Postgres. validateAndHashConfigBundle is the security-critical
// store door: it validates every tar member and computes the canonical content
// version in one streamed pass. These tests exercise every rejection path
// (whitelist, path escapes, symlink/hardlink members, name grammar, the
// decompressed-size and file-count caps, invalid mcp JSON) and the version's
// content-hash stability (identical content re-packed any which way → same
// version; different content → different version). Fixtures are built in-test
// with archive/tar + compress/gzip so a test controls tar ordering, mtimes,
// gzip level, and typeflags directly. The pool round-trips live in the
// pgtest-tagged sibling.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// tarEntry is one member to write into a test bundle. A zero Typeflag means a
// regular file; set Typeflag/Linkname for symlink/hardlink/dir members.
type tarEntry struct {
	name     string
	content  string
	typeflag byte
	linkname string
}

// buildBundle gzip-tars entries with the given gzip level and per-member mtime,
// so a test can prove content-hash stability across transport re-packing.
func buildBundle(t *testing.T, level int, mtime time.Time, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		t.Fatalf("gzip writer: %v", err)
	}
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		typeflag := e.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: typeflag,
			Linkname: e.linkname,
			Mode:     0o644,
			Uid:      1000,
			Gid:      1000,
			ModTime:  mtime,
		}
		if typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		if typeflag == tar.TypeReg && len(e.content) > 0 {
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatalf("write body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// validBundle is a well-formed bundle spanning all three top dirs, used as the
// happy-path baseline and the base for mutation.
func validBundle(t *testing.T) []byte {
	t.Helper()
	return buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
		tarEntry{name: "skills/", typeflag: tar.TypeDir},
		tarEntry{name: "skills/review/SKILL.md", content: "# review skill"},
		tarEntry{name: "extensions/cotal/main.js", content: "console.log(1)"},
		tarEntry{name: "mcp/linear.json", content: `{"url":"https://x"}`},
	)
}

func TestValidateConfigBundleAccepts(t *testing.T) {
	version, err := validateAndHashConfigBundle(validBundle(t))
	if err != nil {
		t.Fatalf("valid bundle rejected: %v", err)
	}
	if version == "" {
		t.Fatal("valid bundle produced empty version")
	}
}

func TestValidateConfigBundleRejects(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{
			name:    "non-whitelisted top dir",
			entries: []tarEntry{{name: "config/thing.txt", content: "x"}},
		},
		{
			name:    "absolute path",
			entries: []tarEntry{{name: "/skills/review/SKILL.md", content: "x"}},
		},
		{
			name:    "dot-dot traversal",
			entries: []tarEntry{{name: "skills/../etc/passwd", content: "x"}},
		},
		{
			name:    "symlink member",
			entries: []tarEntry{{name: "skills/evil", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}},
		},
		{
			name:    "hardlink member",
			entries: []tarEntry{{name: "skills/evil", typeflag: tar.TypeLink, linkname: "skills/review/SKILL.md"}},
		},
		{
			name:    "bad name grammar",
			entries: []tarEntry{{name: "skills/bad name!/SKILL.md", content: "x"}},
		},
		{
			name:    "invalid mcp json",
			entries: []tarEntry{{name: "mcp/broken.json", content: "{not json"}},
		},
		{
			name:    "mcp non-json extension",
			entries: []tarEntry{{name: "mcp/thing.txt", content: "{}"}},
		},
		{
			name:    "mcp nested path",
			entries: []tarEntry{{name: "mcp/sub/a.json", content: "{}"}},
		},
		{
			name:    "mcp extra segment after .json",
			entries: []tarEntry{{name: "mcp/a.json/b", content: "{}"}},
		},
		{
			name:    "mcp empty base name",
			entries: []tarEntry{{name: "mcp/.json", content: "{}"}},
		},
		{
			name:    "mcp uppercase extension",
			entries: []tarEntry{{name: "mcp/thing.JSON", content: "{}"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), tc.entries...)
			_, err := validateAndHashConfigBundle(b)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("want ErrInvalidArgument, got %v", err)
			}
		})
	}
}

func TestValidateConfigBundleRejectsNonGzip(t *testing.T) {
	_, err := validateAndHashConfigBundle([]byte("this is not gzip"))
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument for non-gzip input, got %v", err)
	}
}

// TestValidateConfigBundleFileCountCap builds a bundle just over the file-count
// cap and asserts it is rejected; a bundle at the cap is accepted.
func TestValidateConfigBundleFileCountCap(t *testing.T) {
	atCap := make([]tarEntry, 0, maxFileCount)
	for i := range maxFileCount {
		atCap = append(atCap, tarEntry{name: "skills/s" + itoa(i) + "/f", content: "x"})
	}
	if _, err := validateAndHashConfigBundle(buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), atCap...)); err != nil {
		t.Fatalf("bundle at file-count cap rejected: %v", err)
	}
	overCap := make([]tarEntry, len(atCap), len(atCap)+1)
	copy(overCap, atCap)
	overCap = append(overCap, tarEntry{name: "skills/over/f", content: "x"})
	_, err := validateAndHashConfigBundle(buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), overCap...))
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument over file-count cap, got %v", err)
	}
}

// TestValidateConfigBundleGzipBomb is the gzip-bomb defense: a single member
// whose DECOMPRESSED size exceeds maxDecompressedBytes while its COMPRESSED form
// stays tiny (highly compressible zero bytes) must be aborted DURING streamed
// decompression via the cappedReader. This is the RED-first cap test: with the
// cappedReader guard removed the whole payload inflates and this passes
// validation; with it in place the read aborts at the cap.
func TestValidateConfigBundleGzipBomb(t *testing.T) {
	// One member of zeros larger than the decompressed cap. Zeros compress to a
	// few KiB, so the []byte fixture stays small while inflating past the cap.
	bomb := strings.Repeat("\x00", int(maxDecompressedBytes)+1<<20)
	b := buildBundle(t, gzip.BestCompression, time.Unix(1000, 0),
		tarEntry{name: "skills/bomb/data", content: bomb})
	if len(b) > 1<<20 {
		t.Fatalf("bomb fixture compressed to %d bytes; expected < 1 MiB (not exercising the streamed cap)", len(b))
	}
	_, err := validateAndHashConfigBundle(b)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument for gzip bomb, got %v", err)
	}
}

// TestCanonicalVersionStableAcrossRepacking proves version is a CONTENT hash:
// the same (name, content) set re-packed with different tar member ordering,
// different mtimes, and a different gzip level yields the SAME version.
func TestCanonicalVersionStableAcrossRepacking(t *testing.T) {
	a := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
		tarEntry{name: "skills/review/SKILL.md", content: "# review"},
		tarEntry{name: "mcp/linear.json", content: `{"a":1}`},
	)
	// Reversed member order, different mtime, different gzip level — same content.
	b := buildBundle(t, gzip.BestCompression, time.Unix(9999, 0),
		tarEntry{name: "mcp/linear.json", content: `{"a":1}`},
		tarEntry{name: "skills/review/SKILL.md", content: "# review"},
	)
	va, err := validateAndHashConfigBundle(a)
	if err != nil {
		t.Fatalf("bundle a: %v", err)
	}
	vb, err := validateAndHashConfigBundle(b)
	if err != nil {
		t.Fatalf("bundle b: %v", err)
	}
	if va != vb {
		t.Fatalf("re-packed identical content produced different versions: %s vs %s", va, vb)
	}
}

// TestCanonicalVersionDiffersOnContent proves distinct content hashes distinctly.
func TestCanonicalVersionDiffersOnContent(t *testing.T) {
	a := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
		tarEntry{name: "skills/review/SKILL.md", content: "# review v1"})
	b := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
		tarEntry{name: "skills/review/SKILL.md", content: "# review v2"})
	va, err := validateAndHashConfigBundle(a)
	if err != nil {
		t.Fatalf("bundle a: %v", err)
	}
	vb, err := validateAndHashConfigBundle(b)
	if err != nil {
		t.Fatalf("bundle b: %v", err)
	}
	if va == vb {
		t.Fatal("different content produced the same version")
	}
}

// TestCanonicalVersionDiffersOnName proves the framing separates name from
// content: moving a byte from name into content (or vice versa) cannot collide.
func TestCanonicalVersionDiffersOnName(t *testing.T) {
	a := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
		tarEntry{name: "skills/ab/f", content: "c"})
	b := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
		tarEntry{name: "skills/a/f", content: "bc"})
	va, err := validateAndHashConfigBundle(a)
	if err != nil {
		t.Fatalf("bundle a: %v", err)
	}
	vb, err := validateAndHashConfigBundle(b)
	if err != nil {
		t.Fatalf("bundle b: %v", err)
	}
	if va == vb {
		t.Fatal("length-prefixed framing failed: name/content shift collided")
	}
}

// TestValidateConfigBundleRejectsDuplicateMember defends version-stability
// (M1): tar permits duplicate entries, so two regular members at the SAME path
// with DIFFERENT content leave two equal-keyed members whose relative order an
// unstable sort cannot normalize — the same logical bundle re-packed in a
// different tar order could hash to a different version. The door rejects the
// duplicate outright, which both closes that ambiguity and keeps every sort key
// unique.
func TestValidateConfigBundleRejectsDuplicateMember(t *testing.T) {
	b := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
		tarEntry{name: "skills/a/f", content: "one"},
		tarEntry{name: "skills/a/f", content: "two"},
	)
	_, err := validateAndHashConfigBundle(b)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument for duplicate member, got %v", err)
	}
}

// TestValidateConfigBundleRejectsDeviceTypeflag defends version-coverage (M2):
// a non-regular/non-dir typeflag (here a character device) passes the path
// check and contributes nothing to the version hash, yet the store persists the
// original bundle bytes verbatim — so it would ride into the stored row while
// the version excludes it. The door's explicit allowlist rejects it.
func TestValidateConfigBundleRejectsDeviceTypeflag(t *testing.T) {
	b := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
		tarEntry{name: "skills/a/dev", typeflag: tar.TypeChar},
	)
	_, err := validateAndHashConfigBundle(b)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument for device typeflag, got %v", err)
	}
}

// TestValidateConfigBundleEmpty proves a zero-member bundle is accepted with a
// non-empty, stable version — the framing hashes the (empty) member set, so two
// empty bundles hash identically.
func TestValidateConfigBundleEmpty(t *testing.T) {
	a := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0))
	va, err := validateAndHashConfigBundle(a)
	if err != nil {
		t.Fatalf("empty bundle rejected: %v", err)
	}
	if va == "" {
		t.Fatal("empty bundle produced empty version")
	}
	b := buildBundle(t, gzip.BestCompression, time.Unix(9999, 0))
	vb, err := validateAndHashConfigBundle(b)
	if err != nil {
		t.Fatalf("empty bundle (repack): %v", err)
	}
	if va != vb {
		t.Fatalf("empty bundle version unstable: %s vs %s", va, vb)
	}
}

// TestConfigBundleMemberNames pins the value-free info view: member NAMES bucketed
// by top dir, deduplicated and sorted, never content. A skill spreading many files
// under skills/<name>/ contributes <name> once; mcp/<name>.json contributes
// <name> without the .json suffix; directory members declare no name.
func TestConfigBundleMemberNames(t *testing.T) {
	b := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
		tarEntry{name: "skills/", typeflag: tar.TypeDir},
		tarEntry{name: "skills/review/SKILL.md", content: "# review"},
		tarEntry{name: "skills/review/ref.md", content: "ref"},
		tarEntry{name: "skills/triage/SKILL.md", content: "# triage"},
		tarEntry{name: "extensions/cotal/main.js", content: "x"},
		tarEntry{name: "mcp/linear.json", content: `{"a":1}`},
		tarEntry{name: "mcp/github.json", content: `{"b":2}`},
	)
	info, err := configBundleMemberNames(b)
	if err != nil {
		t.Fatalf("configBundleMemberNames: %v", err)
	}
	// review appears in two files but the name set collapses it; sorted order.
	if got, want := strings.Join(info.Skills, ","), "review,triage"; got != want {
		t.Errorf("skills = %q, want %q", got, want)
	}
	if got, want := strings.Join(info.Extensions, ","), "cotal"; got != want {
		t.Errorf("extensions = %q, want %q", got, want)
	}
	// sorted, .json stripped.
	if got, want := strings.Join(info.McpServers, ","), "github,linear"; got != want {
		t.Errorf("mcp_servers = %q, want %q", got, want)
	}
}

// TestConfigBundleMemberNamesEmpty: an empty bundle declares no members — every
// bucket is empty and every presence flag false, not an error.
func TestConfigBundleMemberNamesEmpty(t *testing.T) {
	info, err := configBundleMemberNames(
		buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0)))
	if err != nil {
		t.Fatalf("configBundleMemberNames(empty): %v", err)
	}
	if len(info.Skills) != 0 || len(info.Extensions) != 0 || len(info.McpServers) != 0 ||
		len(info.Rules) != 0 || len(info.Subagents) != 0 || len(info.Prompts) != 0 {
		t.Fatalf("empty bundle names = %+v, want all empty", info)
	}
	if info.HasSettings || info.HasAgentsMD || info.HasModels {
		t.Fatalf("empty bundle flags = settings:%v agents:%v models:%v, want all false", info.HasSettings, info.HasAgentsMD, info.HasModels)
	}
}

// itoa avoids strconv churn in the file-count fixture loop.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// mkYAMLMap is a minimal valid YAML mapping body for a settings/models member.
const mkYAMLMap = "compaction:\n  enabled: true\n"

// TestValidateConfigBundleAcceptsNewMembers pins the RIG-1678 grammar accepts:
// settings/config.yml (YAML mapping), flat rules/*.md|.mdc, flat agents/*.md,
// and the two top-level files AGENTS.md and models.yml (models YAML-mapping).
func TestValidateConfigBundleAcceptsNewMembers(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{"settings/config.yml mapping", []tarEntry{{name: "settings/config.yml", content: mkYAMLMap}}},
		{"rules .md", []tarEntry{{name: "rules/a.md", content: "# rule a"}}},
		{"rules .mdc", []tarEntry{{name: "rules/b.mdc", content: "# rule b"}}},
		{"agents .md", []tarEntry{{name: "agents/design.md", content: "# design agent"}}},
		{"prompts/<role>/SYSTEM.md", []tarEntry{{name: "prompts/supervisor/SYSTEM.md", content: "# supervisor"}}},
		{"profiles/<name>/profile.yml empty models", []tarEntry{{name: "profiles/candidate/profile.yml", content: "models:\n  agents: {}\n"}}},
		{"profiles full superset", []tarEntry{{name: "profiles/candidate/profile.yml", content: "models:\n  manager: litellm/claude-opus:high\n  agents: {}\ncorpus:\n  prompts: null\n  skills: []\n  rules: []\nextensions:\n  mcp: null\nsettings: {}\n"}}},
		{"profiles empty document", []tarEntry{{name: "profiles/candidate/profile.yml", content: ""}}},
		{"top-level AGENTS.md", []tarEntry{{name: "AGENTS.md", content: "# fleet conventions"}}},
		{"top-level models.yml mapping", []tarEntry{{name: "models.yml", content: "providers:\n  x:\n    baseUrl: https://y\n"}}},
		{"models.yml headers env reference", []tarEntry{{name: "models.yml", content: "providers:\n  x:\n    headers:\n      X-Org: MY_ORG_ENV\n"}}},
		{"models.yml headers !command indirection", []tarEntry{{name: "models.yml", content: "providers:\n  x:\n    headers:\n      Authorization: \"!op read secret\"\n"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), tc.entries...)
			if _, err := validateAndHashConfigBundle(b); err != nil {
				t.Fatalf("valid new member rejected: %v", err)
			}
		})
	}
}

// TestValidateConfigBundleRejectsNewMembers pins the structural rejection matrix
// for the RIG-1678 grammar: settings variants/nesting/non-mapping, nested rules,
// non-.md agents, and a top-level file other than AGENTS.md/models.yml.
func TestValidateConfigBundleRejectsNewMembers(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{"settings/config.yaml variant", []tarEntry{{name: "settings/config.yaml", content: mkYAMLMap}}},
		{"settings/config.json variant", []tarEntry{{name: "settings/config.json", content: "{}"}}},
		{"settings other name", []tarEntry{{name: "settings/other.yml", content: mkYAMLMap}}},
		{"settings nested", []tarEntry{{name: "settings/a/b.yml", content: mkYAMLMap}}},
		{"settings non-mapping scalar", []tarEntry{{name: "settings/config.yml", content: "just a scalar\n"}}},
		{"settings non-mapping sequence", []tarEntry{{name: "settings/config.yml", content: "- a\n- b\n"}}},
		{"rules nested", []tarEntry{{name: "rules/nested/a.md", content: "x"}}},
		{"rules wrong ext", []tarEntry{{name: "rules/a.txt", content: "x"}}},
		{"agents non-md", []tarEntry{{name: "agents/a.txt", content: "x"}}},
		{"agents nested", []tarEntry{{name: "agents/sub/a.md", content: "x"}}},
		{"top-level other file", []tarEntry{{name: "README.md", content: "x"}}},
		{"models non-mapping", []tarEntry{{name: "models.yml", content: "- a\n"}}},
		{"prompts wrong filename", []tarEntry{{name: "prompts/supervisor/other.md", content: "x"}}},
		{"prompts nested too deep", []tarEntry{{name: "prompts/supervisor/sub/SYSTEM.md", content: "x"}}},
		{"prompts flat too shallow", []tarEntry{{name: "prompts/SYSTEM.md", content: "x"}}},
		{"prompts bad role name", []tarEntry{{name: "prompts/bad name/SYSTEM.md", content: "x"}}},
		{"profiles wrong filename", []tarEntry{{name: "profiles/candidate/other.yml", content: "models: {}\n"}}},
		{"profiles too deep", []tarEntry{{name: "profiles/candidate/sub/profile.yml", content: "models: {}\n"}}},
		{"profiles too shallow", []tarEntry{{name: "profiles/profile.yml", content: "models: {}\n"}}},
		{"profiles bad name", []tarEntry{{name: "profiles/bad name/profile.yml", content: "models: {}\n"}}},
		{"profiles malformed yaml", []tarEntry{{name: "profiles/candidate/profile.yml", content: "models: [unterminated\n"}}},
		{"profiles non-mapping", []tarEntry{{name: "profiles/candidate/profile.yml", content: "- a\n- b\n"}}},
		{"profiles unknown top-level key", []tarEntry{{name: "profiles/candidate/profile.yml", content: "models: {}\nbogus: 1\n"}}},
		{"profiles models.manager non-string", []tarEntry{{name: "profiles/candidate/profile.yml", content: "models:\n  manager:\n    nested: 1\n"}}},
		{"profiles models.agents value non-string", []tarEntry{{name: "agents/impl.md", content: "---\nname: impl\n---\nx"}, {name: "profiles/x/profile.yml", content: "models:\n  agents:\n    impl:\n      k: v\n"}}},
		{"profiles models.agents non-string key (numeric)", []tarEntry{{name: "agents/impl.md", content: "---\nname: implementer\n---\nx"}, {name: "profiles/x/profile.yml", content: "models:\n  agents:\n    123: sel\n"}}},
		{"profiles models.agents non-string key (bareword bool)", []tarEntry{{name: "agents/impl.md", content: "---\nname: implementer\n---\nx"}, {name: "profiles/x/profile.yml", content: "models:\n  agents:\n    on: sel\n"}}},
		{"profiles models non-string sibling key bypasses manager", []tarEntry{{name: "profiles/x/profile.yml", content: "models:\n  manager: litellm/x\n  0: y\n"}}},
		{"profiles settings credential key", []tarEntry{{name: "profiles/x/profile.yml", content: "settings:\n  auth:\n    broker:\n      token: sekret\n"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), tc.entries...)
			if _, err := validateAndHashConfigBundle(b); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("want ErrInvalidArgument, got %v", err)
			}
		})
	}
}

// TestValidateConfigBundleRejectsCredentialKeys pins the credential denylist at
// the store door: a settings/config.yml setting a credential-marked key, a
// models.yml with providers.<name>.apiKey, and a models.yml with a
// providers.<name>.headers.* literal secret are each rejected with the offending
// key path named in the error; an env-referenced header value passes.
func TestValidateConfigBundleRejectsCredentialKeys(t *testing.T) {
	cases := []struct {
		name    string
		member  tarEntry
		wantSub string // substring the error must name
	}{
		{
			name:    "settings credential key",
			member:  tarEntry{name: "settings/config.yml", content: "auth:\n  broker:\n    token: sekret\n"},
			wantSub: "auth.broker.token",
		},
		{
			name:    "settings credential key nested searxng",
			member:  tarEntry{name: "settings/config.yml", content: "searxng:\n  token: sk-abc\n"},
			wantSub: "searxng.token",
		},
		{
			name:    "models apiKey",
			member:  tarEntry{name: "models.yml", content: "providers:\n  x:\n    apiKey: sk-live-123\n"},
			wantSub: "providers.x.apiKey",
		},
		{
			name:    "models header literal secret",
			member:  tarEntry{name: "models.yml", content: "providers:\n  x:\n    headers:\n      Authorization: \"Bearer sk-live-123\"\n"},
			wantSub: "providers.x.headers.Authorization",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0), tc.member)
			_, err := validateAndHashConfigBundle(b)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("want ErrInvalidArgument, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not name offending key %q", err, tc.wantSub)
			}
		})
	}
}

// TestCredentialKeysMatchSchema is a change-detector on the generated denylist:
// the seven SDK isCredential paths (settings-schema.ts) are the load-bearing
// door policy, so a fork bump that adds or drops one must be caught. If this
// reds, regenerate credential_keys_gen.go (`go generate ./...`) and re-review.
func TestCredentialKeysMatchSchema(t *testing.T) {
	want := []string{
		"auth.broker.token",
		"dev.autoqaPush.token",
		"hindsight.apiToken",
		"mnemopi.embeddingApiKey",
		"mnemopi.llmApiKey",
		"searxng.basicPassword",
		"searxng.token",
	}
	if got := strings.Join(credentialKeys, ","); got != strings.Join(want, ",") {
		t.Fatalf("credentialKeys = %v, want %v (regenerate credential_keys_gen.go if the schema changed)", credentialKeys, want)
	}
}

// TestConfigBundleMemberNamesNewMembers pins the info view over the new members:
// rules/subagents name lists (sorted, extension-stripped) and the three presence
// flags for the singleton members.
func TestConfigBundleMemberNamesNewMembers(t *testing.T) {
	b := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
		tarEntry{name: "settings/config.yml", content: mkYAMLMap},
		tarEntry{name: "AGENTS.md", content: "# conventions"},
		tarEntry{name: "models.yml", content: "providers:\n  x:\n    baseUrl: https://y\n"},
		tarEntry{name: "rules/red-green.md", content: "x"},
		tarEntry{name: "rules/hold-lane.mdc", content: "x"},
		tarEntry{name: "agents/design.md", content: "x"},
		tarEntry{name: "agents/review.md", content: "x"},
		tarEntry{name: "prompts/supervisor/SYSTEM.md", content: "# sup"},
		tarEntry{name: "prompts/owner/SYSTEM.md", content: "# own"},
		tarEntry{name: "profiles/default/profile.yml", content: "models: {}\n"},
		tarEntry{name: "profiles/fast/profile.yml", content: "models: {}\n"},
	)
	info, err := configBundleMemberNames(b)
	if err != nil {
		t.Fatalf("configBundleMemberNames: %v", err)
	}
	if !info.HasSettings || !info.HasAgentsMD || !info.HasModels {
		t.Errorf("flags = settings:%v agents:%v models:%v, want all true", info.HasSettings, info.HasAgentsMD, info.HasModels)
	}
	if got, want := strings.Join(info.Rules, ","), "hold-lane,red-green"; got != want {
		t.Errorf("rules = %q, want %q", got, want)
	}
	if got, want := strings.Join(info.Subagents, ","), "design,review"; got != want {
		t.Errorf("subagents = %q, want %q", got, want)
	}
	if got, want := strings.Join(info.Prompts, ","), "owner,supervisor"; got != want {
		t.Errorf("prompts = %q, want %q", got, want)
	}
	if got, want := strings.Join(info.Profiles, ","), "default,fast"; got != want {
		t.Errorf("profiles = %q, want %q", got, want)
	}
}

// TestValidateConfigBundleProfileAgentKeyLint pins the cross-member
// models.agents key lint (RIG-2968 T1): a profile keying a subagent-role model
// is admitted ONLY when the key matches the FRONTMATTER name: of an agents/*.md
// def shipped in the same bundle — NOT the filename stem. The def in these
// fixtures has a frontmatter name that DIVERGES from its stem (stem "impl",
// frontmatter name "implementer"), so the two directions pin that the lint keys
// on the parsed frontmatter name, not the stem.
func TestValidateConfigBundleProfileAgentKeyLint(t *testing.T) {
	// A def whose frontmatter name (implementer) diverges from its stem (impl).
	divergentDef := tarEntry{name: "agents/impl.md", content: "---\nname: implementer\ndescription: d\n---\nROLE\n"}

	t.Run("frontmatter-name key ACCEPTED", func(t *testing.T) {
		b := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
			divergentDef,
			tarEntry{name: "profiles/candidate/profile.yml", content: "models:\n  agents:\n    implementer: litellm/claude-sonnet:medium\n"},
		)
		if _, err := validateAndHashConfigBundle(b); err != nil {
			t.Fatalf("profile keying the frontmatter name rejected: %v", err)
		}
	})

	t.Run("stem-only key REJECTED", func(t *testing.T) {
		b := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
			divergentDef,
			tarEntry{name: "profiles/candidate/profile.yml", content: "models:\n  agents:\n    impl: litellm/claude-sonnet:medium\n"},
		)
		_, err := validateAndHashConfigBundle(b)
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("want ErrInvalidArgument for a stem-only key, got %v", err)
		}
		if !strings.Contains(err.Error(), "impl") {
			t.Fatalf("error %q should name the offending key", err)
		}
	})

	t.Run("no matching def REJECTED", func(t *testing.T) {
		b := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
			divergentDef,
			tarEntry{name: "profiles/candidate/profile.yml", content: "models:\n  agents:\n    ghost: litellm/claude-sonnet:medium\n"},
		)
		if _, err := validateAndHashConfigBundle(b); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("want ErrInvalidArgument for a key matching no def, got %v", err)
		}
	})

	// F2: a def whose SIBLING frontmatter field (description) is a YAML-ambiguous
	// scalar (bare colon) must still have its name recovered so a profile keying
	// that name is ACCEPTED — the door must be at least as permissive as the SDK
	// loader. Reds before the agentDefFrontmatterName line-scan fallback (name
	// parses to "" -> lint rejects), greens after.
	t.Run("colon-bearing sibling field name recovered ACCEPTED", func(t *testing.T) {
		b := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
			tarEntry{name: "agents/impl.md", content: "---\nname: implementer\ndescription: A thing: with a colon\n---\nROLE\n"},
			tarEntry{name: "profiles/x/profile.yml", content: "models:\n  agents:\n    implementer: sel\n"},
		)
		if _, err := validateAndHashConfigBundle(b); err != nil {
			t.Fatalf("profile keying a name from a def with a colon-bearing sibling field rejected: %v", err)
		}
	})
}

// TestValidateConfigBundleAdmitsDefaultProfile pins that the shipped fleet
// default profile (config/profiles/default/profile.yml) passes the store door
// unchanged — the "the committed config IS the default profile" contract. It
// reads the real committed file so a drift that reds the door is caught here.
func TestValidateConfigBundleAdmitsDefaultProfile(t *testing.T) {
	content, err := os.ReadFile("../../../config/profiles/default/profile.yml")
	if err != nil {
		t.Fatalf("reading shipped default profile: %v", err)
	}
	b := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
		tarEntry{name: "profiles/default/profile.yml", content: string(content)},
	)
	if _, err := validateAndHashConfigBundle(b); err != nil {
		t.Fatalf("shipped default profile rejected at the door: %v", err)
	}
}
