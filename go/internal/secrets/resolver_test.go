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
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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

func TestSetArgs(t *testing.T) {
	const reason = "compass: unit test write"
	const manifest = "/tmp/state/secretspec-123.toml"
	const setVerb = "set"

	// An explicit WithProfile("") still resolves to defaultProfile, and --profile
	// is emitted unconditionally: the CLI acts under exactly the profile the
	// generated manifest declares, rather than agreeing only because the CLI's
	// own built-in default happens to match. No provider flag (none pinned), and
	// crucially no VALUE anywhere in the argv.
	bare := NewSpecResolver(nil, "/tmp/state", WithProfile(""))
	got := bare.setArgs("API_KEY", reason, manifest, bare.resolvedProfile())
	want := []string{
		"--file=" + manifest, "--reason=" + reason, setVerb, "API_KEY",
		"--profile=" + defaultProfile,
	}
	if !equalArgs(got, want) {
		t.Errorf("setArgs bare = %v, want %v", got, want)
	}

	// Provider + profile set → their flags appear; the name is still the only
	// positional after the verb, and both globals still lead the argv.
	full := NewSpecResolver(nil, "/tmp/state", WithProvider("keyring://"), WithProfile("production"))
	gotFull := full.setArgs("API_KEY", reason, manifest, full.resolvedProfile())
	wantFull := []string{
		"--file=" + manifest, "--reason=" + reason, setVerb, "API_KEY",
		"--provider=keyring://", "--profile=production",
	}
	if !equalArgs(gotFull, wantFull) {
		t.Errorf("setArgs full = %v, want %v", gotFull, wantFull)
	}

	// A caller-supplied reason beginning with a dash stays one joined argument.
	hostile := bare.setArgs("API_KEY", "--provider=evil://", manifest, bare.resolvedProfile())
	if !slices.Contains(hostile, "--reason=--provider=evil://") || slices.Contains(hostile, "--provider=evil://") || slices.Contains(hostile, "--provider") {
		t.Errorf("setArgs hostile reason = %v, want one joined reason token and no provider flag", hostile)
	}

	// Same guarantee for the operator-configured flags: ValidateProfile admits a
	// leading dash, and the provider string is unvalidated, so a dash-leading
	// value must stay bound to its own flag rather than being parsed as the next
	// one (which the CLI rejects with a bare exit 2).
	dashCfg := NewSpecResolver(nil, "/tmp/state", WithProvider("--reason=evil"), WithProfile("-prod"))
	gotDash := dashCfg.setArgs("API_KEY", reason, manifest, dashCfg.resolvedProfile())
	if !slices.Contains(gotDash, "--profile=-prod") || !slices.Contains(gotDash, "--provider=--reason=evil") {
		t.Errorf("setArgs dash-leading config = %v, want joined --profile/--provider tokens", gotDash)
	}
	for _, a := range gotDash {
		if a == "-prod" || a == "--reason=evil" {
			t.Errorf("dash-leading config value became its own argv token: %v", gotDash)
		}
	}

	// The value-never-in-argv invariant is NOT asserted here: setArgs takes no
	// value parameter, so no mutation of it could put the value in this argv and
	// any check would pass vacuously. It is defended at the exec boundary by
	// TestSetFeedsValueOnStdinNeverArgv, which spawns a real process, captures
	// the child's actual argv, and asserts the value arrived on stdin instead.
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
		if err := r.Set(context.Background(), "API_KEY", empty, "compass: unit test write"); err == nil {
			t.Errorf("Set with empty value %q = nil, want an error", empty)
		}
	}
	// Both empty: the value guard runs FIRST, so the error names the value, not
	// the reason. The precedence is load-bearing, not cosmetic — server.SetSecret
	// maps a Set failure to CodeUnavailable on the stated premise that the value
	// was already screened non-empty, so a caller that sent neither must still be
	// told about the value. Swapping the two guard blocks reddens this.
	err := r.Set(context.Background(), "API_KEY", "", "")
	if err == nil {
		t.Fatal("Set with empty value and empty reason = nil, want an error")
	}
	if !strings.Contains(err.Error(), "value is empty") {
		t.Errorf("Set with both empty = %q, want the value guard to fire first (\"value is empty\")", err)
	}
	// A bad name is still rejected first, independent of value.
	if err := r.Set(context.Background(), "bad-name", "value", "compass: unit test write"); err == nil {
		t.Error("Set with invalid name = nil, want an error")
	}
}

// TestSecretSpecVersionPin is a drift guard for the SDK HALF of the secretspec
// seam only — the module version in go.mod, which governs the read path (the
// builder API and the native lib it dlopens). It says nothing about the CLI the
// write path spawns; TestSecretSpecCLIVersionFloor guards that half, and the
// two can drift independently because the read and write paths cross different
// seams (SDK vs shelled binary).
//
// The resolver's stdin/trim/empty-reject write contract and the runtime FFI
// dlopen were verified against secretspec v0.20.0 source (secrets.rs:4423-4427
// for the piped-stdin branch and trim, :4430-4433 for empty-value rejection).
// If a devenv fork-sync moves the pin, this fails loudly so the set() contract
// is re-checked against the new source rather than silently drifting.
func TestSecretSpecVersionPin(t *testing.T) {
	const wantVersion = "v0.20.0"
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
		t.Fatalf("secretspec-go pinned at %s, want %s — the write-path contract was verified against secretspec v0.20.0 source (secrets.rs:4423-4427 for piped stdin and trim, :4430-4433 for empty rejection); re-verify set() semantics against the new source before moving the pin", got, wantVersion)
	}
}

// TestSecretSpecCLIVersionFloor guards the CLI half of the seam: the write path
// spawns `secretspec` by name, so the binary the shell resolves — not go.mod —
// decides whether `--reason` is accepted, whether the require_reason policy
// exists, and whether the `age` provider is compiled in at all. Those are the
// behaviors the write path depends on, and none of them are visible to the SDK
// pin, so without this assertion the CLI could drift arbitrarily far while
// every other test stayed green.
//
// This guard is dev-shell-only: no CI lane stages the secretspec binary. It is
// resolved from a pinned input outside the parsed `packages` literal, so this
// test always skips in CI; the CLI half of the seam is asserted on a developer's
// machine instead.
func TestSecretSpecCLIVersionFloor(t *testing.T) {
	const minMajor, minMinor = 0, 20

	bin, err := exec.LookPath(defaultCLI)
	if err != nil {
		t.Skipf("%s not on PATH; skipping the CLI floor guard", defaultCLI)
	}

	out, err := exec.CommandContext(context.Background(), bin, "--version").Output()
	if err != nil {
		t.Fatalf("%s --version: %v", bin, err)
	}
	// `secretspec --version` prints "secretspec <semver>".
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		t.Fatalf("%s --version = %q, want \"secretspec <version>\"", bin, strings.TrimSpace(string(out)))
	}
	version := fields[len(fields)-1]

	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		t.Fatalf("%s reported version %q, want a dotted semver", bin, version)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		t.Fatalf("%s reported version %q: parse major: %v", bin, version, err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("%s reported version %q: parse minor: %v", bin, version, err)
	}

	if major < minMajor || (major == minMajor && minor < minMinor) {
		t.Fatalf("%s is version %s, want >= %d.%d — the floor is parity with the secretspec-go SDK pin in go.mod, so the SDK read half and the CLI write half act under one release rather than skewing; %d.%d also subsumes the older, separate 0.17 `age`-provider floor (age:// was added in 0.17.0), below which encrypted-at-rest writes fail with \"Provider backend 'age' not found\"", bin, version, minMajor, minMinor, minMajor, minMinor)
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
		// argument boundary is ambiguous), the entire piped stdin verbatim, and
		// the body of the manifest --file points at — read HERE, while the
		// parent's temp file still exists, so the parent can assert the manifest
		// was really on disk and really declared the name at exec time.
		stdin, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(2)
		}
		capture := os.Getenv(helperCaptureEnv)
		var manifest []byte
		if i := slices.IndexFunc(os.Args, func(arg string) bool { return strings.HasPrefix(arg, "--file=") }); i >= 0 {
			manifest, err = os.ReadFile(strings.TrimPrefix(os.Args[i], "--file="))
			if err != nil {
				os.Exit(2)
			}
		}
		// Sentinels separate the argv record, the raw stdin bytes and the manifest.
		payload := strings.Join(os.Args, "\x00") + "\x1e" + string(stdin) + "\x1e" + string(manifest)
		if err := os.WriteFile(capture, []byte(payload), 0o600); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestSetFeedsValueOnStdinNeverArgv defends finding #1 (GATING): the value→stdin,
// never→argv invariant on the REAL exec boundary in Set, the audit-reason
// contract the provider's require_reason policy enforces, and the explicit
// manifest the CLI is pointed at. setArgs is pure and cannot regress the exec
// wiring; this drives Set through an actual process spawn (the test binary
// re-exec'd as the pinned CLI) and asserts what the child truly received. A
// future edit that appends the value as a positional arg, breaks cmd.Stdin,
// drops --reason or --file, or moves either after the `set` subcommand reddens
// this.
func TestSetFeedsValueOnStdinNeverArgv(t *testing.T) {
	const value = "the-secret-value"
	const reason = "compass: operator secret write via SetSecret RPC"
	capture := filepath.Join(t.TempDir(), "capture")

	// Pin the CLI to this test binary and route it into the TestMain stand-in
	// branch via env. os.Args[0] is the running test executable; Set execs it as
	// `<bin> --file=<m> --reason=<r> set API_KEY --provider ...`, and TestMain
	// (guarded) plays the CLI.
	stateDir := t.TempDir()
	r := NewSpecResolver(nil, stateDir,
		WithCLI(os.Args[0]),
		WithProvider("keyring://"),
		WithProfile("production"),
	)
	t.Setenv(helperProcessEnv, "1")
	t.Setenv(helperCaptureEnv, capture)

	if err := r.Set(context.Background(), "API_KEY", value, reason); err != nil {
		t.Fatalf("Set = %v, want nil", err)
	}

	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read capture file (stand-in CLI never ran or never wrote): %v", err)
	}
	parts := strings.SplitN(string(raw), "\x1e", 3)
	if len(parts) != 3 {
		t.Fatalf("malformed capture payload: %q", raw)
	}
	argv := strings.Split(parts[0], "\x00")
	stdin, manifest := parts[1], parts[2]

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

	// (b) --reason is present as a joined token and sits ahead of `set` in
	// argv: it is a global flag the CLI accepts on either side of the
	// subcommand, and before it is the canonical position this test pins so the
	// argv shape stays stable. argv[0] is the binary itself, so index comparison
	// is over the real invocation the child received.
	setIdx := slices.Index(argv, "set")
	reasonIdx := slices.IndexFunc(argv, func(arg string) bool { return strings.HasPrefix(arg, "--reason=") })
	if reasonIdx < 0 {
		t.Fatalf("argv %v missing the joined global --reason flag; the provider's require_reason policy fails such a write", argv)
	}
	if got := strings.TrimPrefix(argv[reasonIdx], "--reason="); got != reason {
		t.Errorf("argv --reason value = %q, want %q", got, reason)
	}
	if reasonIdx > setIdx {
		t.Errorf("argv %v places --reason (index %d) after the 'set' subcommand (index %d); pin it before, the canonical position", argv, reasonIdx, setIdx)
	}

	// (c) --file points the CLI at a generated manifest in the resolver's state
	// dir, ahead of `set` for the same reason. Without it the CLI walks up from
	// the process cwd looking for a secretspec.toml the repo deliberately never
	// commits, so every production write fails "No secretspec.toml found".
	fileIdx := slices.IndexFunc(argv, func(arg string) bool { return strings.HasPrefix(arg, "--file=") })
	if fileIdx < 0 {
		t.Fatalf("argv %v missing the joined global --file flag; without a manifest the CLI fails 'No secretspec.toml found'", argv)
	}
	if got := strings.TrimPrefix(argv[fileIdx], "--file="); filepath.Dir(got) != stateDir {
		t.Errorf("argv --file = %q, want a manifest under the resolver state dir %q", got, stateDir)
	}
	if fileIdx > setIdx {
		t.Errorf("argv %v places --file (index %d) after the 'set' subcommand (index %d); pin it before, the canonical position", argv, fileIdx, setIdx)
	}

	// ...and that manifest really existed at exec time, declaring exactly the
	// name being written under the resolver's profile.
	for _, want := range []string{"[profiles.production]", "API_KEY = {", "required = true"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest handed to the CLI missing %q:\n%s", want, manifest)
		}
	}

	// (d) the value rides stdin exactly, with the trailing newline the CLI trims.
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

	if err := r.Set(context.Background(), "API_KEY", "", "compass: unit test write"); err == nil {
		t.Fatal("Set with empty value = nil, want an error")
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Errorf("capture file exists (err=%v): the CLI was invoked for an empty value; it must be rejected before exec", err)
	}
}

// TestSetEmptyReasonNeverInvokesCLI pins the audit-reason contract at the same
// pre-exec boundary as the empty value: the CLI's own require_reason policy is
// an environment heuristic (it gates on agent-env detection), so a reasonless
// write succeeds on one host and is refused on another. Set screens it instead,
// and the capture file's absence proves no process was spawned.
func TestSetEmptyReasonNeverInvokesCLI(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "capture")
	r := NewSpecResolver(nil, t.TempDir(), WithCLI(os.Args[0]))
	t.Setenv(helperProcessEnv, "1")
	t.Setenv(helperCaptureEnv, capture)

	for _, reason := range []string{"", "   \t\n"} {
		if err := r.Set(context.Background(), "API_KEY", "the-secret-value", reason); err == nil {
			t.Errorf("Set with reason %q = nil, want an error", reason)
		}
		if _, err := os.Stat(capture); !os.IsNotExist(err) {
			t.Errorf("capture file exists (err=%v): the CLI was invoked with reason %q; an empty reason must be rejected before exec", err, reason)
		}
	}
}
