package secrets

// Pure contracts for the resolver's testable seams: manifest generation
// (value-free TOML, sorted, every name required, name re-validated as defense
// in depth), the write-path argv construction (value never in argv — it rides
// stdin), and the empty-registry short-circuit (no provider/SDK call, so no FFI
// lib needed). A non-empty Resolve dlopens the SecretSpec cdylib, which is not
// staged here; that live path is the env-gated integration tier (T8).

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sealedsecurity/compass/go/internal/store"
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

func TestSetArgs(t *testing.T) {
	// Bare resolver: just the verb and name, no provider/profile flags, and
	// crucially no VALUE anywhere in the argv.
	bare := NewSpecResolver(nil, "/tmp/state", WithProfile(""))
	got := bare.setArgs("API_KEY")
	const setVerb = "set"
	if want := []string{setVerb, "API_KEY"}; !equalArgs(got, want) {
		t.Errorf("setArgs bare = %v, want %v", got, want)
	}

	// Provider + profile set → their flags appear; the name is still the only
	// positional after the verb.
	full := NewSpecResolver(nil, "/tmp/state", WithProvider("keyring://"), WithProfile("production"))
	gotFull := full.setArgs("API_KEY")
	if want := []string{setVerb, "API_KEY", "--provider", "keyring://", "--profile", "production"}; !equalArgs(gotFull, want) {
		t.Errorf("setArgs full = %v, want %v", gotFull, want)
	}

	// The value must NEVER be in the constructed argv — it rides stdin.
	for _, a := range full.setArgs("API_KEY") {
		if strings.Contains(a, "the-secret-value") {
			t.Errorf("value leaked into argv: %v", gotFull)
		}
	}
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
	// The short-circuit means the SDK/provider was never reached (no FFI lib
	// dlopen, no manifest written) — proven by the fact this test runs at all
	// without the cdylib staged.
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

func TestSetEmptyValueRejected(t *testing.T) {
	// The CLI trims the piped value and rejects an empty one; Set rejects it up
	// front as a deterministic caller error, before shelling out.
	r := NewSpecResolver(nil, "/tmp/state")
	for _, empty := range []string{"", "   ", "\t", "\n", "  \n\t "} {
		if err := r.Set(context.Background(), "API_KEY", empty); err == nil {
			t.Errorf("Set with empty value %q = nil, want an error", empty)
		}
	}
	// A bad name is still rejected first, independent of value.
	if err := r.Set(context.Background(), "bad-name", "value"); err == nil {
		t.Error("Set with invalid name = nil, want an error")
	}
}

// TestSecretSpecVersionPin is a drift guard: the resolver's stdin/trim/empty-
// reject write contract and the runtime FFI dlopen were verified against
// secretspec-go v0.15.0 source (compass ruling SEA-1327 f63edea3). If a devenv
// fork-sync moves the pin, this fails loudly so the set() contract is re-checked
// against the new source rather than silently drifting.
func TestSecretSpecVersionPin(t *testing.T) {
	const wantVersion = "v0.15.0"
	const modulePath = "github.com/cachix/secretspec/secretspec-go"

	// Assert the pin at its source of truth, the module's go.mod — deterministic
	// and independent of build-info population (which go test does not reliably
	// fill). The test file lives at internal/secrets/, so go.mod is two dirs up.
	data, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	var got string
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == modulePath {
			got = fields[1]
			break
		}
	}
	if got == "" {
		t.Fatalf("%s not found in go.mod; expected it pinned at %s", modulePath, wantVersion)
	}
	if got != wantVersion {
		t.Fatalf("secretspec-go pinned at %s, want %s — the write-path contract (stdin/trim/empty-reject) was verified against %s; re-verify set() semantics against the new source before moving the pin (SEA-1327 f63edea3)", got, wantVersion, wantVersion)
	}
}

// TestDeleteValidatesName pins the Delete write-path seam's one observable
// contract: it gates on ValidateName before its no-op success. An invalid name
// (dash — fails the grammar) must surface a non-nil error; a valid name returns
// nil. Delete needs no store or FFI lib (it never reads the registry or shells
// the CLI), so a bare resolver over a temp state dir exercises it fully.
func TestDeleteValidatesName(t *testing.T) {
	ctx := context.Background()
	r := NewSpecResolver(nil, t.TempDir())

	if err := r.Delete(ctx, "bad-name"); err == nil {
		t.Error("Delete with invalid name = nil, want an error")
	}
	if err := r.Delete(ctx, "API_KEY"); err != nil {
		t.Errorf("Delete with valid name = %v, want nil", err)
	}
}

// helperProcessEnv guards the self-re-exec stand-in below: when set, TestMain
// does not run the suite — it plays the pinned CLI instead, capturing what Set
// actually handed the spawned process.
const helperProcessEnv = "GO_WANT_HELPER_PROCESS"

// helperCaptureEnv carries the capture-file path into the re-exec'd stand-in.
const helperCaptureEnv = "GO_HELPER_CAPTURE_FILE"

// TestMain lets this test binary stand in for the secretspec CLI. When Set execs
// os.Args[0] (via WithCLI) with helperProcessEnv=1 set, we are the spawned
// "CLI": record the real argv and the full stdin the parent piped, then exit 0.
// This exercises the ACTUAL exec boundary in Set — argv assembly + stdin wiring
// — not the pure setArgs layer, so a regression that leaks the value into argv
// or drops the stdin pipe is caught. Otherwise run the suite normally.
func TestMain(m *testing.M) {
	if os.Getenv(helperProcessEnv) == "1" {
		// We are the re-exec'd stand-in CLI. Capture argv (\x00-joined so no
		// argument boundary is ambiguous) and the entire piped stdin verbatim.
		stdin, _ := io.ReadAll(os.Stdin)
		capture := os.Getenv(helperCaptureEnv)
		// Sentinel separates the argv record from the raw stdin bytes.
		payload := strings.Join(os.Args, "\x00") + "\x1e" + string(stdin)
		if err := os.WriteFile(capture, []byte(payload), 0o600); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestSetFeedsValueOnStdinNeverArgv defends finding #1 (GATING): the value→stdin,
// never→argv invariant on the REAL exec boundary in Set. setArgs is pure and
// cannot regress the exec wiring; this drives Set through an actual process
// spawn (the test binary re-exec'd as the pinned CLI) and asserts what the child
// truly received. A future edit that appends the value as a positional arg, or
// breaks cmd.Stdin, reddens this.
func TestSetFeedsValueOnStdinNeverArgv(t *testing.T) {
	const value = "the-secret-value"
	capture := filepath.Join(t.TempDir(), "capture")

	// Pin the CLI to this test binary and route it into the TestMain stand-in
	// branch via env. os.Args[0] is the running test executable; Set execs it as
	// `<bin> set API_KEY --provider ...`, and TestMain (guarded) plays the CLI.
	r := NewSpecResolver(nil, t.TempDir(),
		WithCLI(os.Args[0]),
		WithProvider("keyring://"),
		WithProfile("production"),
	)
	t.Setenv(helperProcessEnv, "1")
	t.Setenv(helperCaptureEnv, capture)

	if err := r.Set(context.Background(), "API_KEY", value); err != nil {
		t.Fatalf("Set = %v, want nil", err)
	}

	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read capture file (stand-in CLI never ran or never wrote): %v", err)
	}
	parts := strings.SplitN(string(raw), "\x1e", 2)
	if len(parts) != 2 {
		t.Fatalf("malformed capture payload: %q", raw)
	}
	argv := strings.Split(parts[0], "\x00")
	stdin := parts[1]

	// (a) argv carries the verb and the name...
	if !slices.Contains(argv, "set") {
		t.Errorf("argv %v missing the 'set' verb", argv)
	}
	if !slices.Contains(argv, "API_KEY") {
		t.Errorf("argv %v missing the 'API_KEY' name", argv)
	}
	// ...and the value NEVER appears anywhere in argv (not as a positional, not
	// embedded in a flag) — the whole point of the stdin write path.
	for _, a := range argv {
		if strings.Contains(a, value) {
			t.Errorf("value leaked into argv: %v", argv)
		}
	}

	// (b) the value rides stdin exactly, with the trailing newline the CLI trims.
	if want := value + "\n"; stdin != want {
		t.Errorf("captured stdin = %q, want %q", stdin, want)
	}
}

// TestSetEmptyValueNeverInvokesCLI pins the other half of the write-path
// contract: an empty value is rejected up front (deterministic caller error),
// before any process is spawned. If Set ever shelled out first and let the CLI
// reject the empty value, the stand-in would run and write the capture file —
// so its absence proves the CLI was never invoked.
func TestSetEmptyValueNeverInvokesCLI(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "capture")
	r := NewSpecResolver(nil, t.TempDir(), WithCLI(os.Args[0]))
	t.Setenv(helperProcessEnv, "1")
	t.Setenv(helperCaptureEnv, capture)

	if err := r.Set(context.Background(), "API_KEY", ""); err == nil {
		t.Fatal("Set with empty value = nil, want an error")
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Errorf("capture file exists (err=%v): the CLI was invoked for an empty value; it must be rejected before exec", err)
	}
}
