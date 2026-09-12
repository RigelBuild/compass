//go:build pgtest && unix

package server

// Store-gated SUCCESS-case proof for the armed forge-secret boot path:
// buildBoardWebhookWiring driven with a configured App against the
// REAL secrets.SpecResolver — the SecretSpec FFI read path — over a REAL
// Postgres, not a &fakeResolver{}. Every other forge pgtest injects a fake, so
// the real resolver (which fail-closes a configured deployment when a declared
// secret is unreadable) is never exercised at boot. This test closes that gap:
// declarations are written by the production declareServerSecretNames, read back
// to derive the provider fixture, and resolved through NewSpecResolver so the
// value the webhook-secret closure returns proves it flowed from the provider
// through the real resolver, not a hand-set fake.
//
// The read path dlopens the libsecretspec cdylib, which the dev/CI shell stages
// only as the write-path CLI. The test SKIPs cleanly when that library is
// absent, so a runtime-less sandbox stays green while the assertions are real
// wherever the library is present.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/events"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
)

// armedForgeConfig is the configured-App config that makes boardIngestionEnabled
// TRUE, so buildBoardWebhookWiring arms both lanes instead of early-returning
// all-nil. The two secret NAMES are operator-facing (unprefixed); the store and
// the resolver see them serverSecretName-wrapped.
func armedForgeConfig() ServeConfig {
	return ServeConfig{Forge: ForgeConfig{
		Host: forgeTestHost,
		App: ForgeAppConfig{
			AppID:                42,
			InstallationID:       7,
			AppPrivateKeySecret:  "APP_KEY",
			AppWebhookSecretName: "APP_WEBHOOK",
		},
	}}
}

// testAppPEM generates a real PKCS#1 RSA private key in PEM form. A real,
// parseable PEM matters: the armed path threads it into forge.NewAppTokenSource,
// so it proves the path builds a real App token source rather than passing a
// string around. 2048 bits keeps generation off the test's critical path.
func testAppPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

// dotenvValue renders a value as one double-quoted dotenv value. dotenvy (the
// SecretSpec dotenv provider) interprets \\, \" and \n inside double quotes, so
// a multiline PEM survives on a single logical line. Escaping backslash first,
// then quote, then newline is the only correct order.
func dotenvValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return `"` + v + `"`
}

// writeDeclaredDotenv writes one dotenv line per DECLARED server-secret name,
// taking values from want (keyed by the wrapped, stored name). Deriving the
// fixture from the declared rows — never a hand-list — is load-bearing:
// buildManifest marks every declaration required, so Load fails wholesale if the
// fixture under-provides. A future declaration flows into the fixture
// automatically; a hand-list would silently drift and redden for the wrong
// reason. A declared name with no fixture value fails setup rather than writing
// an empty value that would mask the miss.
func writeDeclaredDotenv(t *testing.T, path string, declared []store.ServerSecretDeclaration, want map[string]string) {
	t.Helper()
	var b strings.Builder
	for _, d := range declared {
		v, ok := want[d.Name]
		if !ok {
			t.Fatalf("declared name %q has no fixture value; provide one for every declared row", d.Name)
		}
		b.WriteString(d.Name)
		b.WriteString("=")
		b.WriteString(dotenvValue(v))
		b.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write dotenv fixture: %v", err)
	}
}

// newArmedResolver builds a SpecResolver over the server-secret registry view
// exactly as production buildSecretResolvers does (ServerDeclaredSecrets view, a
// Server-owned state dir), pointed at the dotenv fixture.
func newArmedResolver(t *testing.T, st *store.Store, dotenvPath string) secrets.Resolver {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), "server")
	return secrets.NewSpecResolver(
		store.ServerDeclaredSecrets{Store: st},
		stateDir,
		secrets.WithProvider("dotenv://"+dotenvPath),
	)
}

// ffiLocateError is the substring the SecretSpec SDK returns when the
// libsecretspec cdylib is neither embedded, on SECRETSPEC_FFI_LIB, nor in a
// nearby Cargo target dir. Dev/CI stage only the write-path CLI, so the read
// path's cdylib may be absent — the success case must SKIP on that, not fail.
const ffiLocateError = "could not locate the libsecretspec library"

// TestBuildBoardWebhookWiringArmedWithRealResolver drives the armed boot path
// against the REAL SpecResolver. It declares the App secrets through the
// production declareServerSecretNames, derives the dotenv fixture from the
// declared rows, and calls buildBoardWebhookWiring: it asserts the wiring arms
// (both lanes + sink + GitHub client non-nil, NOT merely err == nil), and that
// the returned webhook-secret closure returns exactly the fixture value — the
// assertion that proves the value flowed from the provider through the real
// resolver rather than the path merely not erroring.
func TestBuildBoardWebhookWiringArmedWithRealResolver(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root

	cfg := armedForgeConfig()
	if !cfg.Forge.boardIngestionEnabled() {
		t.Fatal("armedForgeConfig must enable board ingestion, else the wiring early-returns all-nil and the test proves nothing")
	}

	// Declare through the production path, then read the declarations back — the
	// same rows the resolver's ServerDeclaredSecrets view reads.
	if err := declareServerSecretNames(ctx, st, cfg); err != nil {
		t.Fatalf("declareServerSecretNames: %v", err)
	}
	declared, err := st.DeclaredServerSecrets(ctx)
	if err != nil {
		t.Fatalf("DeclaredServerSecrets: %v", err)
	}
	if len(declared) == 0 {
		t.Fatal("no server secrets declared; the armed path would fail its first validateForgeSecret")
	}

	const webhookValue = "armed-webhook-secret-value"
	want := map[string]string{
		serverSecretName(cfg.Forge.App.AppPrivateKeySecret):  testAppPEM(t),
		serverSecretName(cfg.Forge.App.AppWebhookSecretName): webhookValue,
	}
	dotenvPath := filepath.Join(t.TempDir(), "provider.env")
	writeDeclaredDotenv(t, dotenvPath, declared, want)

	resolver := newArmedResolver(t, st, dotenvPath)

	// FFI-availability guard AFTER forgeTestStore: a container-less sandbox
	// skips at the store gate; only past it does the missing read-path cdylib
	// warrant its own skip. A probe resolve exercises the real dlopen; a
	// locate-failure SKIPs, any other error fails.
	if _, err := resolver.Resolve(ctx, "armed boot probe"); err != nil {
		if strings.Contains(err.Error(), ffiLocateError) {
			t.Skipf("libsecretspec FFI read path unavailable: %v", err)
		}
		t.Fatalf("probe Resolve through the real resolver: %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	issueBrd := board.NewIssueProjection(bus, st)

	lane, notifyLane, sink, webhookSecret, client, err := buildBoardWebhookWiring(ctx, cfg, st, issueBrd, nil, resolver, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildBoardWebhookWiring armed with the real resolver = %v, want nil", err)
	}
	// Assert the wiring ARMED, not just that it did not error: the early-return
	// path also returns nil error, so nil components would silently pass an
	// err-only check.
	if lane == nil {
		t.Error("board ingest lane is nil; the armed path must build it")
	}
	if notifyLane == nil {
		t.Error("forge notify lane is nil; the armed path must build it")
	}
	if sink == nil {
		t.Error("fan-out sink is nil; the armed path must build it")
	}
	if client == nil {
		t.Error("shared GitHub client is nil; the armed path must build it")
	}
	if webhookSecret == nil {
		t.Fatal("webhook-secret closure is nil; the armed path must build it")
	}

	// The closure resolves through the REAL resolver to the provider. Its value
	// must equal the fixture byte-for-byte — this is what proves the real
	// resolver read the dotenv provider, not that the path merely constructed.
	got, err := webhookSecret(ctx)
	if err != nil {
		t.Fatalf("webhook-secret closure = %v, want it to resolve the fixture value", err)
	}
	if string(got) != webhookValue {
		t.Fatalf("webhook secret = %q, want %q (the provider value, resolved through the real SpecResolver)", got, webhookValue)
	}
}
