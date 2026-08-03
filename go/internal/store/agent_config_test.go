package store

// Config-bundle DOOR contracts (SEA-1624 T1), default gate — pure functions
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
	skills, extensions, mcpServers, err := configBundleMemberNames(b)
	if err != nil {
		t.Fatalf("configBundleMemberNames: %v", err)
	}
	// review appears in two files but the name set collapses it; sorted order.
	if got, want := strings.Join(skills, ","), "review,triage"; got != want {
		t.Errorf("skills = %q, want %q", got, want)
	}
	if got, want := strings.Join(extensions, ","), "cotal"; got != want {
		t.Errorf("extensions = %q, want %q", got, want)
	}
	// sorted, .json stripped.
	if got, want := strings.Join(mcpServers, ","), "github,linear"; got != want {
		t.Errorf("mcp_servers = %q, want %q", got, want)
	}
}

// TestConfigBundleMemberNamesEmpty: an empty bundle declares no members — every
// bucket is empty, not an error.
func TestConfigBundleMemberNamesEmpty(t *testing.T) {
	skills, extensions, mcpServers, err := configBundleMemberNames(
		buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0)))
	if err != nil {
		t.Fatalf("configBundleMemberNames(empty): %v", err)
	}
	if len(skills) != 0 || len(extensions) != 0 || len(mcpServers) != 0 {
		t.Fatalf("empty bundle names = %v/%v/%v, want all empty", skills, extensions, mcpServers)
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
