package secrets

// Pure contracts for the resolver's testable seams: manifest generation
// (value-free TOML, sorted, every name required, name re-validated as defense
// in depth) and the empty-registry short-circuit (no provider/SDK call, so no
// FFI lib needed). A non-empty Resolve dlopens the SecretSpec cdylib, which is
// not staged here; that live path is the env-gated integration tier (T8).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	// The short-circuit means the SDK/provider was never reached (no FFI lib
	// dlopen, no manifest written) — proven by the fact this test runs at all
	// without the cdylib staged.
}

// TestStatusesEmptyRegistry mirrors TestResolveEmptyRegistry for the status
// path: an empty registry short-circuits to an empty report with no provider
// call, so listing a fleet that has declared nothing never touches the FFI
// library or writes a manifest.
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

// TestSecretSpecVersionPin is a drift guard for the secretspec-go SDK pin in
// go.mod, which governs the read path (the builder API and the native lib it
// dlopens). The resolve/report contract and FFI dlopen were verified against
// secretspec v0.20.0 source; if a devenv fork-sync moves the pin, this fails
// loudly so those semantics are re-checked rather than silently drifting.
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
		t.Fatalf("secretspec-go pinned at %s, want %s — the resolve/report contract was verified against secretspec v0.20.0 source; re-verify those semantics against the new source before moving the pin", got, wantVersion)
	}
}
