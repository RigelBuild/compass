//go:build podman

package e2e

// TestClientModeHeadlessChain is the T5.7 headless CI variant
// (docs/designs/product/compass-native-client-mode/design.md §T5.7, lines
// 582-598). The webview-dependent steps (connect screen renders, paste-token,
// board renders, auto-connect-no-screen) are manual QA on the dev box with
// T5.3's TestConnectClassification as the CI proxy, mirroring T4's split — they
// are NOT automated here. This test drives the IMPORTABLE Go-side chain against
// a REAL running stack, which is genuinely additive over T5.3: T5.3 proved the
// bridge+tokenstore contract against an httptest stub, this proves it against
// the fixture's real compass-server binary over its real self-signed TLS door
// with its real minted admin token.
//
// runClient / bridgeService.Connect live in cmd/compass-app package main under
// //go:build unix && gtk4 and need a webview/display, so they cannot run in
// headless podman CI and are not importable — this test therefore composes the
// importable pieces (bridge.NewTLSTarget, the generated compass.v1 connect
// client, tokenstore) against the real server, never calling runClient nor
// spawning the app binary.
//
// podmanUsable-guarded so a container-less sandbox SKIPS rather than fails. The
// only waits are the fixture's own event-gated readiness (NewFixture blocks on
// runner enrollment) and per-RPC deadlines derived from the test-root ctx. No
// time.Sleep, no polling, no retries.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/bridge"
	"github.com/RigelBuild/compass/go/internal/tokenstore"
)

func TestClientModeHeadlessChain(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into NewFixture + every RPC below

	f := NewFixture(ctx, t)

	// ── Item 1: real-server happy path over the client bridge ──
	//
	// Build the SAME client-mode connection the app builds in runClient
	// (embedded.go:137 NewTLSTarget + SetBearer), but against the fixture's REAL
	// compass-server rather than T5.3's httptest stub: pin the real self-signed
	// anchor, arm the real admin bearer, and make the two authenticated
	// compass.v1 RPCs the Connect probe makes (bridge_service.go:229-250:
	// GetServerInfo then WhoAmI). This exercises TLS-anchor-trust +
	// bearer-injection end-to-end against the real server binary.
	caPEM, err := os.ReadFile(f.CAPath())
	if err != nil {
		t.Fatalf("read fixture CA PEM %q: %v", f.CAPath(), err)
	}

	target, err := bridge.NewTLSTarget(f.ServerURL(), caPEM)
	if err != nil {
		t.Fatalf("NewTLSTarget: %v", err)
	}
	target.SetBearer(f.AdminToken())

	httpClient, baseURL := target.Client()
	cc := compassv1connect.NewCompassServiceClient(httpClient, baseURL)

	infoCtx, cancelInfo := context.WithTimeout(ctx, rpcTimeout)
	defer cancelInfo()
	infoResp, err := cc.GetServerInfo(infoCtx, connect.NewRequest(&compassv1.GetServerInfoRequest{}))
	if err != nil {
		t.Fatalf("GetServerInfo over client bridge against real server: %v", err)
	}
	if infoResp.Msg.GetApiVersion() == "" {
		t.Fatal("GetServerInfo returned an empty api_version over the client bridge")
	}

	whoCtx, cancelWho := context.WithTimeout(ctx, rpcTimeout)
	defer cancelWho()
	whoResp, err := cc.WhoAmI(whoCtx, connect.NewRequest(&compassv1.WhoAmIRequest{}))
	if err != nil {
		t.Fatalf("WhoAmI over client bridge against real server: %v", err)
	}
	if whoResp.Msg.GetAccountId() == "" {
		t.Fatal("WhoAmI returned an empty account id over the client bridge (bearer not accepted by the real door)")
	}

	// Negative half of the anchor+bearer contract: an UNARMED target over the
	// same trusted anchor must be rejected Unauthenticated by the real door,
	// proving the door enforces the bearer rather than incidentally passing (and
	// that item 1's success is due to the injected bearer, not an open door).
	unarmed, err := bridge.NewTLSTarget(f.ServerURL(), caPEM)
	if err != nil {
		t.Fatalf("NewTLSTarget (unarmed): %v", err)
	}
	unarmedClient, unarmedBase := unarmed.Client()
	uc := compassv1connect.NewCompassServiceClient(unarmedClient, unarmedBase)
	unauthCtx, cancelUnauth := context.WithTimeout(ctx, rpcTimeout)
	defer cancelUnauth()
	if _, err := uc.WhoAmI(unauthCtx, connect.NewRequest(&compassv1.WhoAmIRequest{})); err == nil {
		t.Fatal("WhoAmI over an unarmed target unexpectedly succeeded; the real door did not enforce the bearer")
	} else if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Fatalf("unarmed WhoAmI: got code %v, want %v", got, connect.CodeUnauthenticated)
	}

	// ── Item 2: tokenstore round-trip against the real flow ──
	//
	// The keychain-fallback store the client mode relies on (T5.2, DL-109): key
	// the real admin token by the real server URL, read it back over a FRESH
	// store instance (item 4: a simulated relaunch — a re-opened store re-reads
	// its persisted credential with no re-entry), and — when the file fallback
	// is the bound backend (keyring absent in headless CI) — assert the 0600
	// fallback-file mode. If the host keyring is available the store binds to it
	// and no fallback file exists; the round-trip is the always-on contract, the
	// 0600 assertion rides the fallback path per the record.
	storeDir := t.TempDir()
	store := tokenstore.New(storeDir)
	if err := store.Write(f.ServerURL(), f.AdminToken()); err != nil {
		t.Fatalf("tokenstore Write: %v", err)
	}
	// The real admin bearer just landed in the store; on a keyring-equipped box
	// (which podmanUsable() implies) that is the host OS keyring, not a temp dir,
	// so delete it on teardown rather than leaking one dead credential entry per
	// run — the file fallback already auto-reaps under t.TempDir(). A Delete
	// error during cleanup is not actionable.
	t.Cleanup(func() { _ = store.Delete(f.ServerURL()) })

	relaunched := tokenstore.New(storeDir) // a fresh store == the client relaunching against the same state dir
	got, err := relaunched.Read(f.ServerURL())
	if err != nil {
		t.Fatalf("tokenstore Read after relaunch: %v", err)
	}
	if got != f.AdminToken() {
		t.Fatal("tokenstore Read after relaunch returned a token that does not match what was written")
	}

	// The F1 replay guard end-to-end: the same store must NOT return the token
	// for a different server URL.
	if _, err := relaunched.Read(f.ServerURL() + "/other"); err != tokenstore.ErrNotFound {
		t.Fatalf("tokenstore Read for a mismatched url: got err %v, want ErrNotFound", err)
	}

	// 0600 fallback-file mode, only when the file backend is the bound one.
	fallbackFile := filepath.Join(storeDir, "remote-token")
	if info, statErr := os.Stat(fallbackFile); statErr == nil {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("fallback token file mode = %o, want 600", perm)
		}
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("stat fallback token file: %v", statErr)
	} else {
		t.Log("tokenstore bound to the OS keyring (no fallback file); 0600-mode assertion is the file-fallback path per the record")
	}

	// ── Item 3: process-level secret hygiene (DL-109, the process-hygiene half) ──
	//
	// The fixture spawned real compass child processes; scope the /proc scan to
	// this fixture's own processes via its unique runtimeDir (fixture.go:44-47),
	// so unrelated host processes are never matched, and assert the admin token
	// substring appears in NONE of their cmdlines — the credential travels over
	// the door as a bearer, never on a command line.
	assertTokenNotInCmdlines(t, f.RuntimeDir(), f.AdminToken())

	// A client-mode app.toml carries mode/server_url/ca_cert but NEVER the token
	// (the token lives in the tokenstore, DL-109). NOTE: this is a
	// design-conformance placeholder, not regression coverage — compass-app has
	// no app.toml WRITER yet (embedded.go/main.go only Load it), so this
	// constructs the TOML the client setup would write and asserts the shape.
	// The real hygiene coverage is the /proc scan + tokenstore legs above; when a
	// production client-mode config writer lands, point this at it instead of a
	// test-authored literal so it catches a real leak.
	appToml := "mode = \"client\"\n" +
		"server_url = " + strconv.Quote(f.ServerURL()) + "\n" +
		"ca_cert = " + strconv.Quote(f.CAPath()) + "\n"
	appTomlPath := filepath.Join(t.TempDir(), "app.toml")
	if err := os.WriteFile(appTomlPath, []byte(appToml), 0o600); err != nil {
		t.Fatalf("write client-mode app.toml: %v", err)
	}
	raw, err := os.ReadFile(appTomlPath)
	if err != nil {
		t.Fatalf("read client-mode app.toml: %v", err)
	}
	if strings.Contains(string(raw), f.AdminToken()) {
		t.Fatal("client-mode app.toml contains the admin token substring; the token must never be persisted in config")
	}
}

// assertTokenNotInCmdlines scans /proc/<pid>/cmdline for every process whose
// command line references scopeDir (this fixture's unique runtime-dir, so only
// this fixture's own children match — never an unrelated host process) and fails
// if any of them carries token on its command line. cmdline is NUL-separated;
// the NULs are normalized to spaces for the substring check. A pid that exits
// between the readdir and the read is skipped (ESRCH → the file vanishes), not a
// failure. At least one scoped process must be found, else the assertion would
// be vacuous.
func assertTokenNotInCmdlines(t *testing.T, scopeDir, token string) {
	t.Helper()

	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}

	matched := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a pid dir
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue // process exited or unreadable; skip, not a failure
		}
		cmdline := strings.ReplaceAll(string(raw), "\x00", " ")
		if !strings.Contains(cmdline, scopeDir) {
			continue // not one of this fixture's own processes
		}
		matched++
		if strings.Contains(cmdline, token) {
			t.Fatalf("admin token substring leaked into /proc/%s/cmdline", e.Name())
		}
	}

	if matched == 0 {
		t.Fatalf("no fixture process cmdline referenced scope dir %q; the /proc hygiene assertion would be vacuous", scopeDir)
	}
}
