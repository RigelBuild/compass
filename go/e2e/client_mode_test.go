//go:build podman

package e2e

// TestClientModeHeadlessChain exercises the importable client connection against
// a real podman stack, its TLS anchor, and its minted admin token. The app's
// runClient requires GTK/webview and cannot run in headless CI, so this proves the
// reusable bridge, Connect client, and tokenstore without spawning the app.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/appconfig"
	"github.com/RigelBuild/compass/go/internal/bridge"
	"github.com/RigelBuild/compass/go/internal/tokenstore"
)

func TestClientModeHeadlessChain(t *testing.T) {
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into every RPC below

	f := sharedFixture(t)

	// Build the app's client target with the fixture's TLS anchor and bearer, then
	// call GetServerInfo and WhoAmI over the real server connection.
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
	cc := compassv1connect.NewCompassServiceClient(connect.NewClient(connecthttp.NewTransport(httpClient, baseURL, connecthttp.WithReadMaxBytes(0))))

	infoCtx, cancelInfo := context.WithTimeout(ctx, rpcTimeout)
	defer cancelInfo()
	infoResp, err := cc.GetServerInfo(infoCtx, &compassv1.GetServerInfoRequest{})
	if err != nil {
		t.Fatalf("GetServerInfo over client bridge against real server: %v", err)
	}
	if infoResp.GetApiVersion() == "" {
		t.Fatal("GetServerInfo returned an empty api_version over the client bridge")
	}

	whoCtx, cancelWho := context.WithTimeout(ctx, rpcTimeout)
	defer cancelWho()
	whoResp, err := cc.WhoAmI(whoCtx, &compassv1.WhoAmIRequest{})
	if err != nil {
		t.Fatalf("WhoAmI over client bridge against real server: %v", err)
	}
	if whoResp.GetAccountId() == "" {
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
	uc := compassv1connect.NewCompassServiceClient(connect.NewClient(connecthttp.NewTransport(unarmedClient, unarmedBase, connecthttp.WithReadMaxBytes(0))))
	unauthCtx, cancelUnauth := context.WithTimeout(ctx, rpcTimeout)
	defer cancelUnauth()
	if _, err := uc.WhoAmI(unauthCtx, &compassv1.WhoAmIRequest{}); err == nil {
		t.Fatal("WhoAmI over an unarmed target unexpectedly succeeded; the real door did not enforce the bearer")
	} else if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Fatalf("unarmed WhoAmI: got code %v, want %v", got, connect.CodeUnauthenticated)
	}

	// Write and reopen the real admin token under this server URL. On keyring-backed
	// hosts cleanup deletes the OS credential; TempDir removes file fallbacks, whose
	// mode is also asserted.
	storeDir := t.TempDir()
	store := tokenstore.New(storeDir)
	if err := store.Write(f.ServerURL(), f.AdminToken()); err != nil {
		t.Fatalf("tokenstore Write: %v", err)
	}
	// On keyring-backed hosts the token is stored outside TempDir, so delete it on
	// cleanup; the file fallback is already reaped with TempDir.
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

	// Scope the /proc scan to this fixture's runtimeDir and ensure no child cmdline
	// carries the admin token; it must travel only as a bearer.
	assertTokenNotInCmdlines(t, f.RuntimeDir(), f.AdminToken())

	assertClientAppToml(t, f.ServerURL(), f.AdminToken(), caPEM)
}

// assertClientAppToml saves a client-mode app.toml through the production
// SaveClient writer and checks the mode, the normalized URL, the CA copy, and
// that the token never lands in the file.
func assertClientAppToml(t *testing.T, serverURL, token string, caPEM []byte) {
	t.Helper()
	appTomlPath := filepath.Join(t.TempDir(), "app.toml")
	if _, err := appconfig.SaveClient(t.Context(), appTomlPath, appconfig.Config{
		Mode:      appconfig.ModeClient,
		ServerURL: serverURL,
	}, caPEM); err != nil {
		t.Fatalf("save client-mode app.toml: %v", err)
	}
	raw, err := os.ReadFile(appTomlPath)
	if err != nil {
		t.Fatalf("read client-mode app.toml: %v", err)
	}
	saved, err := appconfig.Parse(raw)
	if err != nil {
		t.Fatalf("parse client-mode app.toml: %v", err)
	}
	if saved.Mode != appconfig.ModeClient {
		t.Fatalf("saved mode = %s, want client", saved.Mode)
	}
	wantURL, err := appconfig.NormalizeServerURL(serverURL)
	if err != nil {
		t.Fatalf("normalize fixture server URL: %v", err)
	}
	if saved.ServerURL != wantURL {
		t.Fatalf("saved server URL = %q, want %q", saved.ServerURL, wantURL)
	}
	if filepath.Dir(saved.CACert) != filepath.Dir(appTomlPath) {
		t.Fatalf("saved CA path = %q, want a file beside app.toml", saved.CACert)
	}
	caName := filepath.Base(saved.CACert)
	if match, err := filepath.Match("server-ca-*.pem", caName); err != nil || !match {
		t.Fatalf("saved CA filename = %q, want server-ca-*.pem", caName)
	}
	savedCA, err := os.ReadFile(saved.CACert)
	if err != nil {
		t.Fatalf("read saved CA copy: %v", err)
	}
	if !bytes.Equal(savedCA, caPEM) {
		t.Fatal("saved CA copy does not match fixture CA")
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("client-mode app.toml contains the admin token substring; the token must never be persisted in config")
	}
}

// assertTokenNotInCmdlines scans processes scoped by runtimeDir and rejects any
// cmdline carrying the token. It skips processes that exit mid-scan and requires
// at least one scoped process so the check is not vacuous.
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
