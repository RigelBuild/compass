# Compass-native T5 — native-client mode end to end (RIG-1686)

Status: Draft

Refines the frozen parent record `docs/designs/ui/compass-native-app/design.md` §T5 + §A4. Design only; no implementation.

## Problem / Intent

The same `compass-app` binary that runs embedded mode (T4, done) must also run
**native-client mode**: connect to an already-established remote
`compass-server` instead of supervising a local stack. Today client mode is a
parked sentinel — `go/cmd/compass-app/embedded.go:101-102`:

```go
 case appconfig.ModeClient:
  return "", errClientNotImplemented
```

T5 wires it end to end: mode-select → connect screen (bearer token ONLY) →
keychain store + bearer injection over the shell IPC → remote TLS dial with
optional private-CA anchor → apiVersion compat check → `WhoAmI` resolves the
caller account → board renders live. Refines parent §T5 (design.md:493-518)
against frozen rows DL-107 (shell-side CA trust + bearer injection), DL-109
(mode-select + keychain-first creds, never config/argv), DL-111 (WhoAmI
supplies caller identity).

## Global Constraints

- **Framework = Wails v3** (DL-110). The shell binds services via
  `application.NewService` and emits frames through the Wails event manager
  (`go/cmd/compass-app/bridge_service.go:30-36` — "Its signature matches
  `*application.EventManager.Emit` exactly").
- **Go go1.26.5; proto-shimmed `go`** — build/vet only via
  `direnv exec <workspace-root> go …`, never bare `go`.
- **Scripts-over-bash**; tests are Go/TS test code.
- **No cleartext**: `server_url` is already https-only —
  `go/internal/appconfig/appconfig.go:129-132`:

  ```go
   if u.Scheme != "https" {
    return fmt.Errorf(
     "appconfig: server_url %q must use https (got scheme %q): cleartext connections are not allowed",
  ```

  The server side independently refuses a cleartext door and pins TLS 1.3
  (`go/server/network_door.go:99-103`, `:115` `MinVersion: tls.VersionTLS13`).
- **Secret hygiene (DL-109)**: the bearer lives ONLY in the OS keychain (0600
  file fallback) and in the shell-injected `Authorization` header. NEVER in
  app.toml (enforced: `appconfig.go:137-140` rejects URL-embedded creds and
  `Config` "carries neither the bearer token (OS keychain, DL-109) nor the
  caller account id (WhoAmI RPC, DL-111)", `appconfig.go:48-50`), never in
  argv, never in the UI-side `Connection`, never in the process table, never
  logged (mirror `network_door.go:258-259` "Log the path, never the token").
- **Cross-lane contracts are consumed, not redefined**: the server's bearer
  interceptors, WhoAmI, and apiVersion are compass-server's surface (grounded
  below). Any needed server-side change is an Open Question, not a task here.
- **UI transport-boundary discipline (§A1)**: no Wails/shell import above the
  `ShellIpc` / `ConnectionProvider` seams (`apps/ui/src/live/provider.ts:12-14`
  — "NO shell/Wails dependency reaching this module or anything above the
  transport boundary").

## Approach

### What already exists (consumed, not re-designed)

**Client-mode config parsing is DONE.** `go/internal/appconfig/appconfig.go`
parses `mode="client"` (`modeStrClient` :32), requires a validated https
`server_url` (`parseClient` :106-119, `validateServerURL` :124-143), carries an
optional `ca_cert` trust-anchor path (`Config.CACert` :57-60 — "optional path
to a private trust anchor (PEM) … Empty means use the system roots"), rejects
unknown keys (:85-91), and applies the `--mode` override with re-validation
(`applyOverride` :200-218). T5 consumes `appconfig.Config{Mode, ServerURL,
CACert}` as-is.

**The mode flag plumbing is DONE.** `go/cmd/compass-app/main.go:57-59`:

```go
 modeFlag := flag.String("mode", "",
  "Operating mode override (embedded|client). Defaults to $COMPASS_APP_MODE, "+
   "then app.toml, then embedded.")
```

and `launchByMode` dispatches on the resolved mode
(`embedded.go:97-105`) — the `ModeClient` branch is the sentinel T5 replaces.

**The bridge pump is transport-agnostic by design.** `go/internal/bridge/pump.go:77-81`:

```go
// Today the only constructor is [NewUnixTarget] (embedded/Dogfood mode: h2c over
// a Unix domain socket). The type is deliberately transport-agnostic so a
// TLS/network-door target (native-client mode) is a clean later drop-in: a new
// constructor supplies a client with a TLS-dialing transport and an https base
// URL, and the pump forwarding logic is unchanged.
```

T5 adds that constructor; `Pump.Do` (:117-127) is untouched.

**The shell IPC bridge service is DONE (Wails v3).** `bridge_service.go` binds
`compass_rpc`/`compass_rpc_cancel` and streams frames as Wails runtime events
(`bridge_service.go:9-11` — "streams each ordered response frame back as a
Wails runtime event named \"compass_rpc:\"+requestId"). It already exposes an
`AccountID` bound getter whose doc anticipates client mode
(`bridge_service.go:81-83` — "It returns the empty string when no identity was
resolved (client mode, …)").

**The UI seams are DONE — with one stale-binding caveat.** The
`ConnectionProvider.resolve()` seam (`apps/ui/src/live/provider.ts:28-30`) and
`envConnectionProvider` (:37-43) are the boot seam; `ShellIpc`
(`apps/ui/src/daemon-transport.ts:32`) + `createDaemonFetch` (:66) are the
frame seam, deliberately framework-agnostic (:13-15 — "any shell can supply
its own `ShellIpc` binding"). **Caveat**: the only in-tree binding is still the
stale Tauri one — `daemon-transport.ts:188` `import { Channel, invoke } from
"@tauri-apps/api/core";` and `tauriShellIpc` (:190-200), with
`@tauri-apps/api": "^2"` still in `apps/ui/package.json:17` — while the Go
shell is Wails v3 emitting `"compass_rpc:"+requestId` events. A Wails-events
`ShellIpc` binding does not exist in the UI tree yet (grep for `wails` under
`apps/ui/src` matches only the provider.ts comment). T5 designs strictly
against the `ShellIpc` interface; the Wails JS binding is its own small task
(T5.4) and a coordination point with the compass-ui lane (OQ-5).

**The server contracts (compass-server's; cited, consumed).**

- Bearer interceptors on the network door — `go/server/network_door.go:263-267`:

  ```go
   interceptors := connect.WithInterceptors(
    auth.BearerInterceptor(st),
    auth.BearerStreamInterceptor(st),
    auth.NewAdminGate(adminID),
   )
  ```

- The token the operator reads — `issueAndWriteAdminToken`
  (`network_door.go:333-339`) mints and writes it 0600 as `admin-token` under
  the state dir (`adminTokenFile` :36; atomic 0600 write, `writeTokenFile`
  :341-347).
- `GetServerInfo` returns `Version` + `ApiVersion`
  (`go/server/service.go:377-386`), with the contract constant
  `const apiVersion = "compass.v1"` (`service.go:36-37`).
- `WhoAmI` reflects the bearer's subject and fails closed —
  `go/server/service.go:398-402`:

  ```go
   caller, ok := auth.CallerFrom(ctx)
   if !ok {
    return nil, connect.NewError(connect.CodeUnauthenticated, errNoCaller)
   }
   return connect.NewResponse(&compassv1.WhoAmIResponse{AccountId: string(caller)}), nil
  ```

- UI probe: `probeServer` returns `{version, apiVersion}` —
  `apps/ui/src/live/client.ts:65-68` (the parent's `client.ts:48-51` citation
  has drifted; current lines are 65-68). `resolveCaller` (:76-84) already
  rejects an empty WhoAmI id.
- The parked caller-id gap is **already retired**: `apps/ui/src/live/connection.ts:3-6`
  — "The caller's own account id is NOT part of this config: it is learned
  from the server via the WhoAmI RPC right after the transport is up"; the
  `connection.ts:28-35` lines the parent cites are now the `CompassEnv`
  interface (:30-33), no caller-id field anywhere. Nothing to remove; T5 just
  must not reintroduce one.

### The T5 shape: shell owns secrets and dial; UI owns the screen

The load-bearing split: **everything secret or transport-shaped lives in the
Go shell; the UI renders states and forwards one pasted string.** This is the
direct composition of DL-107 (shell-side CA trust + bearer injection), DL-109
(keychain-first, UI `Connection` carries no bearer), and DL-111 (identity is
server-resolved) with the existing seams.

Client-mode flow:

1. **Launch (Go)** — `run()` resolves `cfg.Mode == ModeClient`. Unlike
   embedded mode, whose whole bring-up runs before the window opens under a
   bounded bring-up context (`main.go:84-86`), client mode opens the window
   IMMEDIATELY: bring-up may require the user to paste a token, so the window
   IS the bring-up surface and there is no pre-window probe that could wedge
   launch on a slow or unreachable remote. The shell builds the remote TLS
   `bridge.Target` from `cfg.ServerURL` + `cfg.CACert` and constructs the
   bridge service wired with the tokenstore and the connect prober.
2. **Auto-connect (UI-driven, ONE probe path)** — boot calls the `Connect` IPC
   method with an empty token (see step 4, "use the stored one"), and the UI
   renders a `connecting` state while the call is in flight. This is the ONLY
   auto-connect probe: the shell runs no separate pre-window probe, so there is
   exactly one probe chain per launch (closes the duplicate-probe gap). If a
   stored token exists and the probe succeeds, boot proceeds straight to the
   board — token survives restart via the keychain (gate requirement).
3. **Connect screen (UI)** — if there is no stored token, or the stored-token
   probe returns a failure kind, the UI renders the connect screen: the server
   URL displayed read-only (it comes from app.toml, not user entry), ONE input
   — the bearer token — and one visual state per failure kind. No caller-id
   field (DL-111). Submit is disabled on empty input: the empty-token sentinel
   is a boot-internal call, never a user submit.
4. **Connect probe (Go, one bound IPC method)** — the token (pasted, or empty
   meaning "use the stored one") goes over a single `Connect` IPC call. Go-side,
   using a connect-go client over the same TLS-anchored `http.Client` the bridge
   target uses, the shell: `GetServerInfo` → exact-match
   `apiVersion == "compass.v1"` → `WhoAmI` → classify any failure into a sealed
   error-kind enum (`bad-url` / `bad-cert` / `bad-token` / `version-mismatch`
   / `other`). On success it writes the keychain, arms the pump's bearer
   injector, and returns `{accountId, serverVersion, apiVersion}` (`accountId`
   used for the "connected as …" confirmation, not written to the shell's
   set-once `accountID` field — see step 6 / OQ-7). On failure it returns the
   kind + a safe message and stores nothing.
5. **Token dropped by the UI** — the connect screen hands the token to
   `Connect` and discards it; the native `ConnectionProvider` resolves a
   `Connection` with `token: undefined` (assertable). Note this supersedes the
   parent's "handed an empty token" phrasing (`compass-native-app/design.md:152-153`):
   the client factory rejects an EMPTY STRING loudly as a misconfiguration
   (`apps/ui/src/live/connection.ts:17-19`), so the correct no-bearer value is
   `undefined`, not `""` — an executor following the parent's prose must not
   ship the rejected empty string. The gRPC-Web transport is built without a
   bearer (`packages/compass-client/src/index.ts:63-66`
   `createCompassWebTransport(baseUrl, token?, opts?)` — token simply not
   passed); the shell injects `Authorization: Bearer …` into every forwarded
   call at the pump target and strips any inbound `authorization` header so
   the UI cannot inject one even by bug.
6. **Board renders** — the UI boots through the existing chain
   (`index.tsx:32-47` `bootConnection` → `main` → `bootCaller`), with ONLY the
   native provider substituted for `envConnectionProvider`. The caller id is
   resolved UNIFORMLY by the existing `bootCaller(root, () =>
   resolveCaller(clients.compass))` path (`index.tsx:64`) — the same WhoAmI
   round-trip both modes already run, now tunneled over the IPC fetch with the
   shell-injected bearer. This keeps the mode difference confined to the boot
   provider choice (§A1's one sanctioned divergence, no second mode-conditional
   above the transport seam) and lets a token revoked between `Connect` and boot
   land on `bootCaller`'s failure screen rather than a stale cached id. The
   Go-side `AccountID` binding stays empty in client mode (its existing
   documented behavior, `bridge_service.go:81-83`). OQ-7 parks the alternative
   (seed `bootCaller` from a Go-side `shellAccountID()` to save one round-trip)
   for Matt's ruling.

**Why Go-side classification, not UI-side.** The frame seam's failure channel
is a bare string (`pump.go:56-58` `ErrorFrame{Message string}`;
`daemon-transport.ts:23` `{ kind: "error"; message: string }`). Distinguishing
bad-URL / bad-cert / bad-token / version-mismatch by parsing error prose in the
UI would be brittle and would push TLS/x509 vocabulary above the transport
boundary. Go sees the typed causes natively (`*net.OpError`/DNS errors,
`*tls.CertificateVerificationError`, `connect.CodeOf(err) ==
connect.CodeUnauthenticated`, and a successful probe with a mismatched
`ApiVersion`), so the one `Connect` bound method returns a sealed kind and the
frame contract stays untouched.

**Failure-state legibility (the gate's four states).**

| State | Go-side signal | UI copy theme |
| --- | --- | --- |
| bad URL | dial error: DNS/refused/timeout (`*net.OpError`, `*net.DNSError`, deadline) | "Can't reach the host" + check server_url |
| bad cert | `*tls.CertificateVerificationError` (or hostname mismatch) | "Can't verify the server's certificate" + ca_cert hint |
| bad token | probe/WhoAmI returns `connect.CodeUnauthenticated` | "The server rejected this token" |
| version mismatch | probe OK, `ApiVersion != "compass.v1"` | "App speaks compass.v1; server speaks X" |

Each is a distinct enum value with its own test row; `other` is the explicit
residual (never a silent fallthrough).

**Secret-hygiene assertions (gate requirements, each a test):**

- UI: the resolved client-mode `Connection` has `token === undefined`
  (provider unit test).
- Go: `Connect` never logs the token; the keychain fallback file is 0600
  (mirroring `writeTokenFile`'s atomic 0600 pattern,
  `network_door.go:341-347`).
- e2e script: after connect, `app.toml` contains no token substring and
  `/proc/<pid>/cmdline` of every compass process contains no token substring.

## Alternatives considered

- **UI-side probe + error-string parsing** (UI calls `probeServer`/`WhoAmI`
  over the IPC fetch and pattern-matches `ErrorFrame.message`): rejected —
  brittle string matching, TLS vocabulary above the transport boundary, and
  the token would transit the UI twice. The Go-side `Connect` method gives
  typed causes for free.
- **Extending `ResponseFrame.error` with a machine-readable `code` field** so
  the UI can classify transport failures generally: rejected for T5 — it
  changes the frozen §A2 frame contract for a need the single `Connect`
  method covers; revisit only if post-connect mid-session error legibility
  demands it (Open Question OQ-6).
- **Connect screen as a router route** (`/connect` in `AppRoutes`): rejected in
  favor of a boot gate — the screen must render before the store/QueryClient
  exist (boot cannot proceed without a working transport, `index.tsx:49-57`),
  exactly where `renderBootError`-style boot surfaces already live (OQ-2).
- **Storing the token in app.toml or passing via argv/env**: forbidden by
  DL-109 and already actively rejected by `validateServerURL`
  (`appconfig.go:137-140`).

## Plan

Dependency order: T5.1 → T5.2 → T5.3 → (T5.4 ∥ T5.5) → T5.6 → T5.7. T5.1/T5.2/
T5.3 are Go-shell slices; T5.4/T5.5 are UI slices; T5.6 wires launch; T5.7 is
the gate. The `T5.4 ∥ T5.5` parallelism is against a STUBBED seam: T5.5 is
developed against a stubbed `shellConnect`/`nativeConnectionProvider` and
integrates T5.4's real outputs at its gate (T5.5 *consumes* them — see its
Interfaces block), so the two UI slices are concurrently developable but
T5.5's gate depends on T5.4's outputs landing.

### T5.1 — Remote TLS bridge target

New constructor in `go/internal/bridge` beside `NewUnixTarget`
(`pump.go:87-105`), per the package's own drop-in note (`pump.go:77-81`).

- **Do:** `NewTLSTarget(serverURL string, caPEM []byte) (*Target, error)`
  builds a `Target` whose `http.Client` uses an HTTP/2-over-TLS transport:
  system roots when `caPEM` is empty, else a `x509.CertPool` seeded with the
  PEM (the appconfig `ca_cert` anchor, `appconfig.go:57-60`). Base URL =
  `serverURL` (already validated https-absolute by
  `validateServerURL`, `appconfig.go:124-143`). `Pump.Do` is untouched.
  Additionally a header-injection hook on `Target` (or a thin
  `Pump`-level option) that sets `Authorization: Bearer <token>` on every
  forwarded request and DROPS any caller-supplied `authorization` header —
  the DL-107 shell-injection point. The token is held in the target via a
  setter armed by T5.3's `Connect`, guarded for concurrent reads.
- **Interfaces:**
  - consumes `appconfig.Config.ServerURL/.CACert` (read by caller), crypto/tls,
    net/http.
  - produces `func NewTLSTarget(serverURL string, caPEM []byte) (*Target, error)`
    and `func (t *Target) SetBearer(token string)` (empty = no injection).
- **Test cycle:** Go unit tests against `httptest.NewUnstartedServer` with a
  self-signed cert: (a) dial succeeds with the anchor PEM, fails
  `tls.CertificateVerificationError` without it; (b) every forwarded request
  carries exactly one `Authorization` header with the armed bearer; (c) a
  caller-supplied `authorization` header is stripped; (d) empty bearer injects
  nothing.

### T5.2 — Keychain store (keychain-first, 0600-file fallback)

New package `go/internal/tokenstore`.

- **Do:** OS-keychain read/write/delete of the remote bearer, keyed by
  service `"compass-app"` + the server URL (so two remotes never collide).
  Wails v3 ships no keychain API (nothing under `github.com/wailsapp/wails/v3`
  in `go/go.mod:29` provides one — OQ-1), so use `zalando/go-keyring`
  (Secret Service/D-Bus on Linux, Keychain on macOS). Fallback when the
  keyring is unavailable: an 0600 file `$XDG_STATE_HOME/compass/remote-token`
  (else `$HOME/.compass/remote-token`), written atomically exactly like the
  server's token file (temp file born 0600 + rename, `writeTokenFile`,
  `go/server/network_door.go:341-347`). Never log the token
  (`network_door.go:258-259` pattern).
- **The fallback file is URL-bound, not last-writer-wins.** The keyring path
  is already keyed by server URL; the file fallback MUST carry the same
  binding or it becomes a cross-remote bearer-replay hole: a bare token file
  - a later app.toml edit to `server_url = <remote B>` would make auto-connect
  (T5.6) read remote A's bearer and SEND it to B in the `Authorization`
  header — bearer disclosure to an arbitrary server, the exact class the
  server treats as critical (`network_door.go:258-259` — a leaked bearer lets
  anyone impersonate the admin). So the fallback file stores the pair
  `{serverURL, token}` (single JSON object, 0600); `Read(serverURL)` returns
  `ErrNotFound` when the stored URL does not match the requested one, and
  `Write` replaces the whole pair. Single-remote MVP is preserved (one file,
  one pair); the cross-remote replay is closed.
- **Interfaces:**
  - consumes `github.com/zalando/go-keyring` (new dep), os, path/filepath.
  - produces:

    ```go
    type Store interface {
        Read(serverURL string) (token string, err error)  // ErrNotFound when absent OR stored URL != serverURL
        Write(serverURL, token string) error
        Delete(serverURL string) error
    }
    func New(stateDir string) Store // keyring-first, file-fallback
    var ErrNotFound = errors.New(...)
    ```

- **Test cycle:** Go unit tests on the file fallback (temp dir): round-trip,
  0600 mode asserted, `ErrNotFound` on absent, `ErrNotFound` on a URL-mismatch
  read (the F1 replay guard), atomic overwrite. Keyring path covered by a
  build-tagged integration test (D-Bus present) — CI runs the file-fallback
  suite unconditionally.

### T5.3 — `Connect` bound method + failure classification (shell)

Extends `bridgeService` (`go/cmd/compass-app/bridge_service.go`).

- **Do:** a `Connect(ctx, req)` bound IPC method: given a pasted token (or
  `""` meaning "use the stored one" — a boot-internal auto-connect call, never
  a user submit, T5.5), run the probe chain over a connect-go
  `CompassServiceClient` built on the SAME `http.Client`/CA anchor as the
  T5.1 target: `GetServerInfo` → exact-match `ApiVersion` against the app's
  pinned apiVersion literal → `WhoAmI` (subject id, DL-111; empty id rejected
  as the UI's `resolveCaller` does, `apps/ui/src/live/client.ts:78-84`). The
  server's `apiVersion` constant is UNEXPORTED (`const apiVersion =
  "compass.v1"`, `go/server/service.go:36-37`) so the app cannot import it: it
  pins its OWN `const clientAPIVersion = "compass.v1"` literal, cross-checked
  by a test that compares it against a live `GetServerInfo` response from the
  in-repo server (drift then reddens CI here, not silently at a customer).
  On success: `tokenstore.Write`, `target.SetBearer(token)`, and return the
  summary (including the resolved `accountId`) — it does NOT write the
  service's `accountID` field: that field is documented set-once before
  `app.Run` and read without a lock (`bridge_service.go:48-54`), and `Connect`
  is a webview-goroutine bound method (`bridge_service.go:39-42`) so writing it
  at runtime is a data race; the caller id is instead resolved uniformly by the
  UI's `bootCaller`/`resolveCaller` path over the IPC fetch (Approach step 6),
  and the `AccountID` binding stays empty in client mode as documented
  (`bridge_service.go:81-83`). On failure: classify into the sealed kind enum
  and return it; store/arm nothing. A `Disconnect`-shaped forget path is OUT
  of T5 scope (no gate requirement).
- **Interfaces:**
  - consumes T5.1 `NewTLSTarget`/`SetBearer`, T5.2 `tokenstore.Store`,
    `compassv1connect.CompassServiceClient`, `connect.CodeOf`.
  - produces (Go, bound to the webview; camelCase JSON tags like `rpcRequest`,
    `bridge_service.go:95-104`):

    ```go
    type connectRequest struct{ Token string `json:"token"` }
    type connectResult struct {
        OK            bool   `json:"ok"`
        Kind          string `json:"kind"` // "" | "bad-url" | "bad-cert" | "bad-token" | "version-mismatch" | "other"
        Message       string `json:"message"`       // safe prose, never the token
        AccountID     string `json:"accountId"`      // for the "connected as …" confirmation; NOT written to the service's set-once accountID field (see Do / OQ-7)
        ServerVersion string `json:"serverVersion"`
        APIVersion    string `json:"apiVersion"`
    }
    func (s *bridgeService) Connect(ctx context.Context, req connectRequest) connectResult
    ```

  - classification: `*net.DNSError`/`*net.OpError`/deadline → `bad-url`;
    `*tls.CertificateVerificationError` (via `errors.As` on the dial error) →
    `bad-cert`; `connect.CodeUnauthenticated` → `bad-token`; probe OK with
    `ApiVersion != clientAPIVersion` → `version-mismatch`; else `other`.
- **Test cycle:** table-driven Go tests against `httptest` TLS servers, one
  row per kind: unreachable host, self-signed-untrusted cert, a stub
  CompassService returning Unauthenticated, a stub returning
  `ApiVersion:"compass.v2"`, and the success path asserting keychain write +
  `SetBearer` armed + account id recorded. Assert the token string never
  appears in `Message` or the log output.

### T5.4 — Wails `ShellIpc` binding + native ConnectionProvider (UI)

**Cross-lane: needs compass-ui coordination (decided, OQ-5).**
`apps/ui/src/daemon-transport.ts`, `apps/ui/src/components/MarkdownText.tsx`,
`apps/ui/src/components/MarkdownText.test.tsx`, and `apps/ui/package.json` are
the compass-ui lane's zone; this task edits all four, so it MUST be coordinated
with (and ack'd by) the compass-ui lane before
it starts. Parent §A2 anticipates the `ShellIpc` swap
(`compass-native-app/design.md:127-130` — "swaps only the two framework calls
(`invoke`, `Channel`, today Tauri-shaped in the UI) for the Wails runtime
behind a thin `ShellIpc` shim"), so it is contract-legitimate; the gate is
ownership/scheduling, not the frozen contract.

Replaces the stale Tauri binding (`apps/ui/src/daemon-transport.ts:185-207`,
`@tauri-apps/api/core`) with the Wails v3 one matching the Go shell's actual
contract: bound-method calls + `"compass_rpc:"+requestId` runtime events
(`bridge_service.go:9-12`).

- **Do:** a `wailsShellIpc: ShellIpc` binding — `rpc` subscribes to the
  `"compass_rpc:"+requestId` event before invoking the bound `CompassRPC`,
  delivering each `ResponseFrame` to `onFrame` and unsubscribing on terminal
  frame; `cancel` invokes `CompassRPCCancel`. Plus a
  `nativeConnectionProvider(): ConnectionProvider` that resolves
  `{ baseUrl, token: undefined, fetchImpl: createDaemonFetch(wailsShellIpc) }`
  — the DL-109 assertion point: the UI-side `Connection` NEVER carries a
  bearer in client mode. Also a thin wrapper `shellConnect(token)` →
  `Connect`. (No `shellAccountID` wrapper: the caller id is resolved uniformly
  by the existing `resolveCaller` path over the IPC fetch — Approach step 6 /
  OQ-7 — so the shell binding surface stays minimal.) All Wails imports live
  in this one module (provider.ts:12-14 discipline). Remove the
  `@tauri-apps/api` dependency and `tauriShellIpc`/`daemonFetch` export.
- **Both Tauri deps leave in this task (decided, OQ-5).** T5.4 removes the two
  independent Tauri runtime imports so the native shell carries no Tauri
  dependency: (1) the `ShellIpc` binding — `@tauri-apps/api/core`
  (`daemon-transport.ts:188`), swapped for the Wails events binding above; and
  (2) the link opener — `apps/ui/src/components/MarkdownText.tsx:1`
  `import { openUrl } from "@tauri-apps/plugin-opener";` (+ `package.json:18`
  `"@tauri-apps/plugin-opener": "^2"`), which cannot open links under the Wails
  webview (external links in rendered markdown are broken in both native modes
  today, pre-existing from T4). Swap it for the Wails runtime's
  browser-open call (`@wailsio/runtime` `Browser.OpenURL`), keeping the
  browser-dev-build path (a plain anchor / `window.open`) intact behind the
  same seam. In `package.json`: remove BOTH `@tauri-apps/*` entries AND ADD
  `@wailsio/runtime` (pinned) to `dependencies` — the runtime this task's
  binding (`Call.ByName`/`Events.On`/`Browser.OpenURL`) imports, not currently
  a UI dep. (Flagged as a new JS dep at review, mirroring the go-keyring
  Ledger-impact call-out.) The T5.7 gate's "board renders" is extended to
  assert a rendered-markdown external link actually opens, so a
  silently-broken opener cannot pass.
- **Interfaces:**
  - consumes `ShellIpc`/`createDaemonFetch` (`daemon-transport.ts:32,66`),
    `ConnectionProvider`/`ResolvedConnection` (`provider.ts:22-30`), the Wails
    v3 JS runtime (`@wailsio/runtime`: `Call.ByName`, `Events.On` — exact API
    OQ-5).
  - produces (TS):

    ```ts
    export function wailsShellIpc(): ShellIpc;
    export function nativeConnectionProvider(baseUrl: string): ConnectionProvider;
    export function shellConnect(token: string): Promise<ConnectResult>;
    export type ConnectResult = { ok: boolean; kind: "" | "bad-url" | "bad-cert" | "bad-token" | "version-mismatch" | "other"; message: string; accountId: string; serverVersion: string; apiVersion: string };
    ```

- **Test cycle:** unit tests with a fake Wails runtime (the same
  fake-the-seam style as `FakeShellIpc`, `daemon-transport.test.ts:42`):
  frame ordering, unsubscribe-on-terminal, cancel forwarding; provider test
  asserting `resolve()` yields `token === undefined` and a defined
  `fetchImpl`. **Plus:** rewrite `MarkdownText.test.tsx`'s link-safety suite,
  which today couples to the removed dep (`import * as realOpener from
  "@tauri-apps/plugin-opener"` at line 3; `mock.module("@tauri-apps/plugin-opener",
  …)` at lines 429/434/472/499) — remock the new Wails `Browser.OpenURL` seam
  instead, PRESERVING every `javascript:`/`file:`/`data:` scheme-neutralization
  assertion (a dangerous href must never reach the opener) and the
  safe-`https:`-still-opens assertion. Removing the Tauri opener without this
  rewrite breaks the suite's module mocks; the suite must not be deleted or
  skipped to green CI (that would drop live-href injection coverage).

### T5.5 — Connect screen as a boot gate (UI)

- **Do:** a boot-gate screen (not a router route — OQ-2) driven by the native
  boot path. Boot calls `shellConnect("")` ONCE (the single auto-connect probe,
  Approach step 2) and branches on the result: while it is in flight the UI
  shows a `connecting` state; on `ok` it proceeds into the board; on any
  failure kind it renders the connect screen — read-only server URL, one token
  input, a connect button driving `shellConnect(token)`, and one visual state
  per failure kind (distinct copy per the Approach table; `other` shows the
  safe message). Submit is DISABLED on empty input, so the empty-token
  "use-the-stored-one" sentinel is only ever the boot-internal call, never a
  user action. `shellConnect("")` is also the armed-ness channel (no separate
  `Connected()` getter needed): a successful result means the shell armed a
  connection, a failure means render the gate. On success, proceed into the
  existing boot chain (`index.tsx:32-47`): resolve via
  `nativeConnectionProvider`, and resolve the caller id UNIFORMLY through the
  existing `bootCaller(root, () => resolveCaller(clients.compass))` path
  (`index.tsx:64`) — no mode-conditional, no `shellAccountID` seeding (OQ-7).
  The token input's value is cleared after the call; nothing stores it
  UI-side.
- **Interfaces:**
  - consumes T5.4 `shellConnect`/`nativeConnectionProvider`,
    `renderBootError`-style painting (`apps/ui/src/boot.ts:48-53`), the boot
    chain (`index.tsx:58-69`).
  - produces `bootNativeClient(root: HTMLElement): Promise<ResolvedConnection | undefined>`
    — the client-mode sibling of `bootConnection`, returning undefined only
    when the user cannot proceed (it keeps the screen up and retries in
    place, so in practice it resolves on success).
- **Test cycle:** component/unit tests with a stubbed `shellConnect`: the
  in-flight `connecting` state renders; each failure kind renders its distinct
  state; success resolves the provider; submit is disabled on empty input; the
  token variable is not retained after submit (spy on the stub, assert the
  input is cleared and no module-scope binding holds it).

### T5.6 — Client-mode launch wiring (shell)

- **Do:** replace `errClientNotImplemented` (`embedded.go:56-59,101-102`).
  Today `run()` (`main.go:48-151`) resolves the stack binary and builds the
  quit controller UNCONDITIONALLY before dispatch: `resolveStackBin` hard-fails
  launch on error (`main.go:89-92` — "`stackBin, err := resolveStackBin(...)`;
  `if err != nil { return err }`") and the quit controller/"Quit and stop
  stack" menu is built at `main.go:122-140`. Both move UNDER the embedded
  branch: a client-only install has no `compass-stack` binary and no stack to
  stop, so neither may gate client launch. In the client branch: build
  `NewTLSTarget(cfg.ServerURL, caPEM)` (caPEM read from `cfg.CACert` when set,
  with a legible read/parse error) instead of `NewUnixTarget(socket)`
  (`main.go:105`), construct the bridge service wired with the tokenstore +
  connect prober, and open the window IMMEDIATELY. There is NO pre-window
  auto-connect probe (that reconciles the earlier draft's contradiction): the
  single auto-connect probe is the UI's boot-time `shellConnect("")`
  (Approach step 2, T5.5), so a slow/unreachable remote never wedges launch.
  The launch mode is handed to the UI as a shell-injected startup global
  (`window.__COMPASS_MODE__`, set at window creation — OQ-8) that the UI reads
  synchronously at entry to pick env-provider vs native-client path, so boot
  needs no IPC round-trip to learn its mode.
- **Interfaces:**
  - consumes T5.1/T5.2/T5.3, `appconfig.Load` (`appconfig.go:158`),
    `launchByMode` (`embedded.go:97-105`).
  - produces the reshaped launch. `launchByMode` today is
    `func(ctx, mode, pipeline, params) (string, error)` returning the account
    id (`embedded.go:97-105`). Retire the single-return shape in favour of two
    explicit mode paths in `run()` (no sum-type gymnastics): an
    `runEmbedded(ctx, params) (accountID string, quit *quitController, err error)`
    and a `runClient(cfg, deps) (*bridgeService, err error)` that wires the TLS
    target + tokenstore + prober and leaves `accountID` empty (client identity
    is UI-resolved, OQ-7). `run()` switches on `cfg.Mode` and only the embedded
    arm touches `resolveStackBin`/the quit controller. Plus: inject
    `window.__COMPASS_MODE__` (`"embedded" | "client"`) into the webview at
    window creation (the `application.WebviewWindowOptions` in `run()`,
    `main.go:142-147`), the boot-dispatch signal the UI reads with no IPC
    (OQ-8). No bound `Mode()` IPC getter: the injected global is the single
    source of truth for the mode.

- **Test cycle:** replace `embedded_test.go`'s `launchByMode` rows with the two
  split paths: `runClient` no longer errors, invokes no pipeline/stack-bin
  effect, produces a TLS target, and attempts auto-connect iff the store has a
  token for the configured URL (fake store + fake prober); `runEmbedded`
  retains its existing behavior.

### T5.7 — e2e gate (parent §T5 gate, design.md:513-518)

- **Do:** a scripted e2e against a locally-started `compass-server --listen`
  with its self-signed anchor and minted `admin-token`
  (`network_door.go:327-339`): app in client mode (`app.toml`: `mode="client"`,
  `server_url`, `ca_cert`) → connect screen → paste token → board renders;
  wrong token → `bad-token` state; server stubbed to a different apiVersion →
  `version-mismatch` state; relaunch → auto-connect from keychain (no screen);
  assert no token substring in `app.toml` nor in `/proc/*/cmdline`; UI
  `Connection.token === undefined` (covered by the T5.4 unit assertion,
  referenced in the checklist). Manual QA on the dev box + a headless CI
  variant covering the Go-side chain (T5.3's tests are the CI proxy for the
  webview-dependent steps, mirroring T4's split, parent design.md:490-491).
- **Interfaces:** consumes everything above + `compass-server --listen`.
  Produces the T5 gate evidence.
- **Test cycle:** the script itself, kept under the repo's scripts convention.

## Tasks

- [ ] **T5.1** `bridge.NewTLSTarget` (CA anchor + `SetBearer` header
  injection/strip); TLS + injection unit tests.
- [ ] **T5.2** `go/internal/tokenstore` keyring-first store with atomic 0600
  file fallback; round-trip + mode + `ErrNotFound` tests.
- [ ] **T5.3** `bridgeService.Connect` probe chain (GetServerInfo →
  apiVersion exact-match → WhoAmI) with the sealed failure-kind enum; one
  test row per kind + success side-effects + no-token-in-logs assertion.
- [ ] **T5.4** (coordinate with compass-ui — edits their zone) Wails
  `ShellIpc` binding + `nativeConnectionProvider` + `shellConnect`; BOTH Tauri
  deps removed (`@tauri-apps/api` ShellIpc swap + `@tauri-apps/plugin-opener`
  link-opener → Wails browser-open); fake-runtime frame/cancel tests + `token
  === undefined` provider assertion.
- [ ] **T5.5** Connect-screen boot gate (`bootNativeClient`): boot-driven
  `shellConnect("")` probe, `connecting` + four failure states, read-only URL,
  token-only input with submit-disabled-on-empty; stubbed-shell component
  tests incl. token-not-retained.
- [ ] **T5.6** Client-mode launch wiring: TLS target instead of UDS,
  `resolveStackBin`/quit-controller moved embedded-only, boot-driven
  auto-connect (no pre-window probe), `window.__COMPASS_MODE__` injected at
  window creation (no `Mode()` IPC getter); split `runEmbedded`/`runClient`
  dispatch tests.
- [ ] **T5.7** e2e gate script vs `compass-server --listen` (token accepted /
  wrong token / version mismatch / restart-survives-keychain /
  no-secret-in-config-or-cmdline); manual QA + headless CI variant.

## Open Questions

OQ-1/OQ-5/OQ-7/OQ-8 resolved by Matt's ruling before freeze (the genuine forks
— a new third-party dep, a cross-lane zone edit, and two A-vs-B boot/identity
choices with viable alternatives; OQ-8 was surfaced by the design review);
OQ-2/OQ-3/OQ-4/OQ-6 by the adopted recommendation, each confirmed sound by the
design red-team. Kept here as the decision record.

- **OQ-1 — Keychain API under Wails v3. RESOLVED (Matt): `zalando/go-keyring`.**
  Wails v3 (beta, `github.com/wailsapp/wails/v3 v3.0.0-beta.0`, `go/go.mod:29`)
  ships no keychain service. The existing `secretspec-go` dep (`go.mod:21`,
  v0.15.0) does NOT fit: its Go SDK is resolve-only (`New()` →
  `WithProvider/WithProfile` → `Load()/Report()`), with no `Set`/`Write`
  primitive — writing a value into a provider is a CLI action, not an SDK one.
  This is grounded in-repo by the seal-side wrapper of that same SDK:
  `go/internal/secrets/resolver.go:80-81` documents `WithCLI` as pinning "the
  secretspec CLI binary used for the write path", and `resolver.go`'s resolve
  path uses only `b.WithProvider`/`b.WithProfile`/`b.Load()` (`resolver.go:162-164`),
  never a `Set`. And it is a SERVER-side manifest-driven
  resolve-a-declared-set surface (DL-026, `internal/secrets`), the wrong layer
  for a client-side single-token write. So T5 takes a direct keyring dep:
  `zalando/go-keyring` (pure-Go, no CGO), which wraps the same OS backends
  (Linux Secret Service via D-Bus, macOS Keychain) with the `Set/Get/Delete`
  write API the resolve-only SDK lacks. The URL-bound 0600 file fallback
  (OQ-4) covers keyring-less hosts per DL-109. *(affects T5.2)*
- **OQ-2 — Connect screen: new router route vs boot gate.**
  **Recommendation: boot gate** (a `bootConnection`-sibling surface painted
  before store/QueryClient construction): the transport does not exist yet,
  so no route below `main()` can render (`index.tsx:49-57` — boot cannot
  proceed without the connection), and the boot-error surfaces already live
  there (`boot.ts:48-53`). A route would drag half-initialized store state
  into every screen. *(affects T5.5)*
- **OQ-3 — apiVersion policy beyond MVP.** MVP is exact-match against
  `"compass.v1"` (`go/server/service.go:36-37`). **Recommendation: keep
  exact-match for T5** and defer any semver-floor/range policy until a
  `compass.v2` actually exists — the constant is a contract name, not a
  semver, so ordering semantics are undefined today. Revisit at the first
  contract rev. *(affects T5.3)*
- **OQ-4 — 0600 fallback path.** **Recommendation:
  `$XDG_STATE_HOME/compass/remote-token` (else `$HOME/.compass/remote-token`)**
  — the same state-dir resolution family the embedded stack already uses
  (`main.go:64-66`: `$COMPASS_STATE_DIR`, then `$XDG_STATE_HOME/compass`,
  then `$HOME/.compass`), and the same directory hygiene as the server's
  `admin-token` (0600 under the state dir, `network_door.go:327-331`). The
  file stores the `{serverURL, token}` pair (single JSON object) and is
  URL-bound, NOT last-writer-wins: `Read(serverURL)` returns `ErrNotFound` on
  a URL mismatch so a re-pointed `server_url` never replays remote A's bearer
  to remote B (the F1 guard). Multiple concurrent remotes are still deferred:
  one file, one pair, matching the single-remote MVP. *(affects T5.2)*
- **OQ-5 — Wails JS runtime API + the cross-lane Tauri removal (compass-ui).
  RESOLVED (Matt): T5.4 removes BOTH Tauri deps; coordinate with compass-ui.**
  Two coupled parts on `apps/ui` (compass-ui's zone). (a) JS surface: the
  in-tree binding is still Tauri (`daemon-transport.ts:188`) while the Go shell
  emits Wails runtime events (`bridge_service.go:9-12`) — use `@wailsio/runtime`
  `Events.On`/`Call.ByName` directly (no generated bindings, keeps the seam
  hand-rolled). (b) The second Tauri dep, `@tauri-apps/plugin-opener` in
  `MarkdownText.tsx:1` (+ `package.json:18`), which cannot open links under the
  Wails webview — swapped in the SAME task (T5.4) for the Wails browser-open
  call, so the native shell carries no Tauri dependency after T5.4. Both edits
  are in compass-ui's zone (`daemon-transport.ts`, `MarkdownText.tsx`,
  `package.json`), so **T5.4 must be coordinated with and ack'd by the
  compass-ui lane before it starts**. *(affects T5.4)*
- **OQ-6 — Mid-session failure legibility.** After connect, a revoked token
  or server restart surfaces as `ErrorFrame` prose
  (`pump.go:56-58`) — the connect-time kinds don't apply. **Recommendation:
  out of T5 scope** (the gate only requires connect-time legibility); if
  needed later, extend the frame contract with a `code` field as its own
  §A2-touching record. *(affects nothing in T5; parked)*
- **OQ-7 — Caller-id resolution at boot: uniform `resolveCaller` vs Go-side
  seeding. RESOLVED (Matt): uniform `resolveCaller` over the IPC fetch.** After
  a successful `Connect`, run the existing
  `bootCaller(root, () => resolveCaller(clients.compass))` path
  (`index.tsx:64`) in BOTH modes, tunneled over the IPC fetch with the
  shell-injected bearer. Costs one extra WhoAmI round-trip at boot; in return
  there is no second mode-conditional above the transport seam (§A1's one
  sanctioned divergence is the provider choice; the parent forbids another
  above the fetch/IPC seam, `compass-native-app/design.md:335-337`), a token
  revoked between `Connect` and boot lands on `bootCaller`'s failure screen
  rather than a stale cached id, and the racy runtime `accountID` write (F2) is
  eliminated along with the `shellAccountID` binding. The rejected alternative
  (seed `bootCaller` from a Go-side `shellAccountID()`) saved the round-trip at
  the cost of that mode-branch, staleness window, and concurrency hazard.
  *(affects T5.3 accountID handling, T5.4 binding surface, T5.5 boot path)*
- **OQ-8 — Entry-point mode dispatch: how boot picks env-provider vs
  native-client BEFORE it can call a Go getter. RESOLVED (Matt): a
  shell-injected startup global.** `index.tsx:32` today unconditionally boots
  `envConnectionProvider` (`provider.ts:36-44`), and any Go-side getter is an
  IPC call only available inside the Wails shell — so the entry point needs a
  mode signal it can read synchronously, with NO IPC, before either boot path
  starts. **The shell injects the mode as a startup global**
  (`window.__COMPASS_MODE__ = "embedded" | "client"`) at window creation (the
  `application.WebviewWindowOptions` in `run()`, `main.go:142-147`); the UI
  reads it synchronously at entry: `"client"` → `bootNativeClient`; `"embedded"`
  → the embedded native provider; absent (browser dev build, no shell) → the
  unchanged `bootConnection(envConnectionProvider)` path. The mode difference
  stays confined to which provider boot installs (`provider.ts:1-14`). The
  rejected alternatives were a synchronous `@wailsio/runtime`-presence sniff
  (implicit — infers mode from runtime presence rather than an explicit signal)
  and a Vite build flag (splits the bundle by target and adds a build-config
  surface the shell must set correctly); the injected global is the most
  explicit shell→UI contract and needs no runtime inference. **Consequence for
  T5.6:** this REPLACES the previously-planned bound `Mode()` IPC getter, whose
  only purpose was boot dispatch — the record carries the injected global, not
  the getter, so there is one source of truth for the mode. *(affects the T5.6
  launch wiring and the T5.4/T5.5 UI boot entry)*

## Ledger-impact

**None — with one PR-flagged dependency.** This record is a pure refinement of
the parent's frozen rows: DL-107 (the bearer-injection + CA-trust point lands
in the T5.1 target exactly as the row states), DL-109 (keychain-first store +
connect screen + no-config/argv, T5.2/T5.5), DL-110 (Wails v3 binding,
T5.4), DL-111 (WhoAmI subject, T5.3). No new decision class is introduced:
the failure-kind enum, the boot-gate placement, and the tokenstore package
shape are implementation structure under those rows, not new contracts.
Dependency to flag in the impl PR: OQ-1 resolved to `zalando/go-keyring`, a new
third-party Go-module dep — it IMPLEMENTS DL-109's existing "keychain-first"
ruling (it is not a new decision and does not amend the ledger), but a new dep
warrants an explicit call-out at review. `docs/designs/ui/DECISIONS.md`
stays untouched (single-writer rule; no new row owed).
