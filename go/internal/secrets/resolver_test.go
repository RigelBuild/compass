package secrets

// Contracts for the resolver: manifest generation (value-free TOML, sorted,
// every name required, name re-validated as defense in depth), the
// empty-registry short-circuit, and the secretspec CLI seam, driven by a fake
// CLI script that records its argv and replays canned stdout/stderr/exit code.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

func decl(name string, kind store.SecretKind) store.SecretDeclaration {
	return store.SecretDeclaration{Name: name, Kind: kind, DeclaredBy: "acct-1"}
}

func TestBuildManifest(t *testing.T) {
	// Given unsorted declarations, the manifest lists names sorted.
	decls := []store.SecretDeclaration{
		decl("ZED", store.SecretKindGeneric),
		decl("API_KEY", store.SecretKindProvider),
		decl("MID", store.SecretKindGeneric),
	}
	out, err := buildManifest("default", decls)
	if err != nil {
		t.Fatalf("buildManifest: %v", err)
	}

	// Structural anchors: a [project] block naming the single compass project,
	// and the [profiles.<profile>] block the resolver resolves under.
	for _, want := range []string{"[project]", `name = "compass"`, "[profiles.default]"} {
		if !strings.Contains(out, want) {
			t.Errorf("manifest missing %q:\n%s", want, out)
		}
	}

	// One required key per declared name, value-free.
	for _, name := range []string{"API_KEY", "MID", "ZED"} {
		line := name + " = {"
		if !strings.Contains(out, line) {
			t.Errorf("manifest missing declaration line for %q:\n%s", name, out)
		}
	}
	if strings.Count(out, "required = true") != len(decls) {
		t.Errorf("want %d required keys, got %d:\n%s", len(decls), strings.Count(out, "required = true"), out)
	}

	// Names appear sorted (API_KEY < MID < ZED).
	iAPI := strings.Index(out, "API_KEY")
	iMID := strings.Index(out, "MID")
	iZED := strings.Index(out, "ZED")
	if !(iAPI < iMID && iMID < iZED) {
		t.Errorf("names not sorted in manifest (API_KEY@%d MID@%d ZED@%d):\n%s", iAPI, iMID, iZED, out)
	}

	// An empty profile falls back to "default".
	outDefault, err := buildManifest("", decls)
	if err != nil {
		t.Fatalf("buildManifest empty profile: %v", err)
	}
	if !strings.Contains(outDefault, "[profiles.default]") {
		t.Errorf("empty profile did not fall back to default:\n%s", outDefault)
	}

	// Defense in depth: an invalid name makes manifest generation fail rather
	// than emit a malformed key.
	if _, err := buildManifest("default", []store.SecretDeclaration{decl("bad-name", store.SecretKindGeneric)}); err == nil {
		t.Error("buildManifest accepted an invalid name; want an error")
	}
}

// fakeDeclarations is a hand-written declarations fake: it records whether it
// was asked for the declared set and returns a fixed result, so the
// empty-registry short-circuit is provable without a Postgres store.
type fakeDeclarations struct {
	called bool
	decls  []store.SecretDeclaration
	err    error
}

func (f *fakeDeclarations) DeclaredSecrets(context.Context) ([]store.SecretDeclaration, error) {
	f.called = true
	return f.decls, f.err
}

func TestResolveEmptyRegistry(t *testing.T) {
	fake := &fakeDeclarations{decls: nil}
	r := NewSpecResolver(fake, "/tmp/state-does-not-need-to-exist")

	out, err := r.Resolve(context.Background(), "test")
	if err != nil {
		t.Fatalf("Resolve on empty registry: %v", err)
	}
	if out != nil {
		t.Errorf("Resolve on empty registry = %v, want nil", out)
	}
	if !fake.called {
		t.Error("Resolve did not read the declared set")
	}
	// The short-circuit means the CLI was never reached (no manifest written) —
	// proven by the fact this test runs at all with the state dir absent.
}

// TestStatusesEmptyRegistry mirrors TestResolveEmptyRegistry for the status
// path: an empty registry short-circuits to an empty report with no provider
// call, so listing a fleet that has declared nothing never spawns the CLI or
// writes a manifest.
func TestStatusesEmptyRegistry(t *testing.T) {
	fake := &fakeDeclarations{decls: nil}
	r := NewSpecResolver(fake, "/tmp/state-does-not-need-to-exist")

	out, err := r.Statuses(context.Background(), "test")
	if err != nil {
		t.Fatalf("Statuses on empty registry: %v", err)
	}
	if out != nil {
		t.Errorf("Statuses on empty registry = %v, want nil", out)
	}
	if !fake.called {
		t.Error("Statuses did not read the declared set")
	}
}

// TestStatusesRegistryReadFailurePropagates asserts a registry fault surfaces as
// an error rather than an empty status set. An empty list is indistinguishable
// from "nothing declared", which would read as a healthy fleet with no secrets.
func TestStatusesRegistryReadFailurePropagates(t *testing.T) {
	fake := &fakeDeclarations{err: errors.New("registry boom")}
	r := NewSpecResolver(fake, "/tmp/state-does-not-need-to-exist")

	out, err := r.Statuses(context.Background(), "test")
	if err == nil {
		t.Fatal("Statuses swallowed a registry read failure; an empty set reads as a healthy empty fleet")
	}
	if out != nil {
		t.Errorf("Statuses = %v on error, want nil", out)
	}
}

// TestWriteManifestConcurrentDistinctPaths guards the F4 fix: each writeManifest
// call must produce its OWN file, so concurrent resolves never share one path
// and race the write-to-Load interval. The prior implementation wrote a single
// fixed "secretspec.toml", so concurrent callers clobbered each other; this
// asserts N concurrent calls yield N distinct, well-formed manifests.
func TestWriteManifestConcurrentDistinctPaths(t *testing.T) {
	dir := t.TempDir()
	r := NewSpecResolver(nil, dir)
	decls := []store.SecretDeclaration{decl("API_KEY", store.SecretKindGeneric)}

	const n = 16
	paths := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			paths[i], errs[i] = r.writeManifest(defaultProfile, decls)
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("writeManifest[%d]: %v", i, errs[i])
		}
		if seen[paths[i]] {
			t.Fatalf("writeManifest returned a shared path %q — concurrent resolves would race", paths[i])
		}
		seen[paths[i]] = true
		body, err := os.ReadFile(paths[i])
		if err != nil {
			t.Fatalf("read manifest %q: %v", paths[i], err)
		}
		if !strings.Contains(string(body), "API_KEY = {") {
			t.Errorf("manifest %q missing the declared key:\n%s", paths[i], body)
		}
	}
}

func TestBuildManifestInvalidProfile(t *testing.T) {
	// A profile that would corrupt the [profiles.<profile>] header fails
	// manifest generation rather than emitting broken TOML.
	decls := []store.SecretDeclaration{decl("API_KEY", store.SecretKindGeneric)}
	for _, bad := range []string{"a]b", "a.b", "a\nb", "[x]"} {
		if _, err := buildManifest(bad, decls); err == nil {
			t.Errorf("buildManifest(profile=%q) = nil error, want rejection", bad)
		}
	}
}

// fakeCLI is a stand-in secretspec binary: a shell script that records its argv
// (one per line) and a copy of the --file manifest, then replays canned output.
type fakeCLI struct {
	path string
	dir  string
}

func newFakeCLI(t *testing.T, stdout, stderr string, rc int) *fakeCLI {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"stdout": stdout, "stderr": stderr} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	script := fmt.Sprintf(`#!/bin/sh
d=%q
printf '%%s\n' "$@" >"$d/argv"
for a in "$@"; do
	case "$a" in --file=*) cp "${a#--file=}" "$d/manifest.seen" ;; esac
done
cat "$d/stdout"
cat "$d/stderr" >&2
exit %d
`, dir, rc)
	path := filepath.Join(dir, "secretspec")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake CLI: %v", err)
	}
	return &fakeCLI{path: path, dir: dir}
}

// argv returns the arguments of the CLI's last invocation.
func (f *fakeCLI) argv(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "argv"))
	if err != nil {
		t.Fatalf("fake CLI was not invoked: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func declsNamed(names ...string) *fakeDeclarations {
	f := &fakeDeclarations{}
	for _, n := range names {
		f.decls = append(f.decls, decl(n, store.SecretKindGeneric))
	}
	return f
}

// manifestArg returns the --file value of argv, failing when there is none.
func manifestArg(t *testing.T, argv []string) string {
	t.Helper()
	for _, a := range argv {
		if p, ok := strings.CutPrefix(a, "--file="); ok {
			return p
		}
	}
	t.Fatalf("argv %q has no --file", argv)
	return ""
}

func TestSpecResolverArgv(t *testing.T) {
	const reason = "-starts-with-dash"
	tests := []struct {
		name     string
		opts     []SpecOption
		call     func(*SpecResolver) error
		sub      string
		provider string // "" = no --provider token expected
		profile  string
		tail     string
	}{
		{
			name: "export with provider",
			opts: []SpecOption{WithProvider("dotenv:///x.env"), WithProfile("prod")},
			call: func(r *SpecResolver) error { _, err := r.Resolve(context.Background(), reason); return err },
			sub:  "export", provider: "dotenv:///x.env", profile: "prod", tail: "--format=json",
		},
		{
			name: "export without provider",
			call: func(r *SpecResolver) error { _, err := r.Resolve(context.Background(), reason); return err },
			sub:  "export", profile: defaultProfile, tail: "--format=json",
		},
		{
			name: "check with provider",
			opts: []SpecOption{WithProvider("keyring://")},
			call: func(r *SpecResolver) error { _, err := r.Statuses(context.Background(), reason); return err },
			sub:  "check", provider: "keyring://", profile: defaultProfile, tail: "--json",
		},
		{
			name: "check without provider",
			call: func(r *SpecResolver) error { _, err := r.Statuses(context.Background(), reason); return err },
			sub:  "check", profile: defaultProfile, tail: "--json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := `{"API_KEY":"v"}`
			if tt.sub == "check" {
				out = `{"schema_version":1,"secrets":[{"name":"API_KEY","status":"resolved"}]}`
			}
			cli := newFakeCLI(t, out, "", 0)
			r := NewSpecResolver(declsNamed("API_KEY"), t.TempDir(), append(tt.opts, WithCLI(cli.path))...)
			if err := tt.call(r); err != nil {
				t.Fatalf("call: %v", err)
			}
			argv := cli.argv(t)
			want := []string{tt.sub, "--file=" + manifestArg(t, argv)}
			if tt.provider != "" {
				want = append(want, "--provider="+tt.provider)
			}
			want = append(want, "--profile="+tt.profile, "--reason="+reason, tt.tail)
			if !slices.Equal(argv, want) {
				t.Fatalf("argv = %q, want %q", argv, want)
			}
		})
	}
}

func TestResolveMapsCLIValues(t *testing.T) {
	// Quotes, a backslash and newlines must survive the JSON round trip intact.
	pem := "-----BEGIN KEY-----\nline \"two\" \\ end\n-----END KEY-----\n"
	cli := newFakeCLI(t, `{"API_KEY":"-----BEGIN KEY-----\nline \"two\" \\ end\n-----END KEY-----\n","EMPTY":""}`, "", 0)
	r := NewSpecResolver(declsNamed("API_KEY", "EMPTY"), t.TempDir(), WithCLI(cli.path))

	out, err := r.Resolve(context.Background(), "test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got := map[string]string{}
	for _, s := range out {
		got[s.Name] = s.Value
		if s.Version != Version(s.Value) {
			t.Errorf("%s Version = %q, want the content hash of its value", s.Name, s.Version)
		}
	}
	if got["API_KEY"] != pem {
		t.Errorf("API_KEY = %q, want %q", got["API_KEY"], pem)
	}
	// An empty string is a present value (SDK Usable semantics), not a miss.
	if v, ok := got["EMPTY"]; !ok || v != "" {
		t.Errorf("EMPTY = %q (present %v), want a present empty value", v, ok)
	}
}

func TestResolveErrors(t *testing.T) {
	tests := []struct {
		name    string
		stdout  string
		stderr  string
		rc      int
		wantSub string
	}{
		{name: "declared name missing from output", stdout: `{"API_KEY":"v"}`, wantSub: `"OTHER" not in resolver output`},
		{name: "null value", stdout: `{"API_KEY":"v","OTHER":null}`, wantSub: `"OTHER" resolved with no value`},
		{name: "CLI failure carries stderr", stderr: "Error: Secret 'OTHER' is required but not set", rc: 1, wantSub: "Secret 'OTHER' is required"},
		{name: "non-JSON output", stdout: `API_KEY=hunter2`, wantSub: "not valid JSON"},
		{name: "non-string value", stdout: `{"API_KEY":12345,"OTHER":"v"}`, wantSub: "non-string value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli := newFakeCLI(t, tt.stdout, tt.stderr, tt.rc)
			r := NewSpecResolver(declsNamed("API_KEY", "OTHER"), t.TempDir(), WithCLI(cli.path))
			out, err := r.Resolve(context.Background(), "test")
			if err == nil {
				t.Fatalf("Resolve = %v, want an error", out)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not contain %q", err, tt.wantSub)
			}
			// A value from stdout must never reach the error text.
			for _, leak := range []string{"hunter2", "12345"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error %q leaks stdout content %q", err, leak)
				}
			}
		})
	}
}

func TestStatusesParsesReportOnNonZeroExit(t *testing.T) {
	// check exits 1 when a required secret is missing but still prints the report.
	report := `{"schema_version":1,"provider":"p","profile":"default","secrets":[
		{"name":"SET","status":"resolved"},
		{"name":"MISSING","status":"missing_required"}]}`
	cli := newFakeCLI(t, report, "", 1)
	r := NewSpecResolver(declsNamed("SET", "MISSING", "UNLISTED"), t.TempDir(), WithCLI(cli.path))

	out, err := r.Statuses(context.Background(), "test")
	if err != nil {
		t.Fatalf("Statuses: %v", err)
	}
	want := []SecretStatus{{Name: "SET", IsSet: true}, {Name: "MISSING"}, {Name: "UNLISTED"}}
	if !slices.Equal(out, want) {
		t.Fatalf("Statuses = %+v, want %+v", out, want)
	}
}

func TestStatusesProviderFaultIsAnError(t *testing.T) {
	cli := newFakeCLI(t, "", "Error: Provider backend 'bogus' not found", 1)
	r := NewSpecResolver(declsNamed("API_KEY"), t.TempDir(), WithCLI(cli.path))

	out, err := r.Statuses(context.Background(), "test")
	if err == nil {
		t.Fatalf("Statuses = %+v, want an error; a broken provider must not read as all-unset", out)
	}
	if !strings.Contains(err.Error(), "Provider backend 'bogus' not found") {
		t.Errorf("error %q does not carry the CLI stderr", err)
	}
}

func TestCLIManifestRemovedAfterCall(t *testing.T) {
	calls := map[string]func(*SpecResolver) error{
		"Resolve":  func(r *SpecResolver) error { _, err := r.Resolve(context.Background(), "test"); return err },
		"Statuses": func(r *SpecResolver) error { _, err := r.Statuses(context.Background(), "test"); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			// rc 1 with no output fails both paths: cleanup must hold on error too.
			cli := newFakeCLI(t, "", "Error: boom", 1)
			stateDir := t.TempDir()
			r := NewSpecResolver(declsNamed("API_KEY"), stateDir, WithCLI(cli.path))
			if err := call(r); err == nil {
				t.Fatal("call succeeded, want the CLI failure")
			}
			// The CLI saw a real manifest declaring the name...
			seen, err := os.ReadFile(filepath.Join(cli.dir, "manifest.seen"))
			if err != nil {
				t.Fatalf("CLI did not see the manifest: %v", err)
			}
			if !strings.Contains(string(seen), "API_KEY = {") {
				t.Errorf("manifest the CLI saw lacks the declared name:\n%s", seen)
			}
			// ...and it is gone from the state dir afterwards.
			if _, err := os.Stat(manifestArg(t, cli.argv(t))); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("manifest still present after the call (stat err %v)", err)
			}
			entries, err := os.ReadDir(stateDir)
			if err != nil {
				t.Fatalf("read state dir: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("state dir holds %d leftover entries, want 0", len(entries))
			}
		})
	}
}
