# Compass native: server URL entry from the UI

Linear: RIG-3102 (design); RIG-1746 (parent)
Refines: `ui/compass-native-client-mode/design.md` (the connect probe and its
failure kinds) and `ui/compass-native-embedded-revival/design.md` §A1 (the
app.toml contract). Supersedes DL-320 via DL-406 (Matt, 2026-10-05); see
§Ledger-impact.

## Problem / Intent

The native client learns its server only from
`$XDG_CONFIG_HOME/compass/app.toml` (`appconfig.Load`), so a beta user must
hand-write TOML before the app can reach a server. This record lets a
first-run user enter the server URL and an optional private CA in the app,
probe it with the bearer they already paste, and keep the result across
restarts, under the validation the file already has. app.toml stays the one
hand-editable source of truth; the bearer stays keychain-first with the 0600-file
fallback (DL-109).

## Global Constraints

1. **Toolchain and gate.** Go module `go/` (`go 1.26.0`) and Bun for
   `apps/ui`, both from the devenv shell. Go: `go test ./...` in `go/`; files
   gated `(linux && gtk4) || darwin` also need `-tags gtk4`. TS: `bun test` in
   `apps/ui`. `moon run :ci` is the whole gate.
2. **One validator.** `appconfig.NormalizeServerURL` is the only server-URL
   check, used by the file parse and by `Connect`. The UI only disables submit
   on an empty field.
3. **Bearer storage stays DL-109's: keychain-first, 0600-file fallback.** Never
   in app.toml, the CA copy, argv, env, or a log. In the UI it lives in the
   input for one call, as today.
4. **app.toml is the one source of truth.** No second store. Every write is
   re-read through `appconfig.Parse` before it is linked into place. The app
   writes only on a first-run choice, and the write fails if app.toml already
   exists, so a hand-written file is never rewritten.
5. **One server per process.** The server is chosen at most once per process,
   on first run, before any successful connect. Embedded never starts inside a
   process that opened without it (compass-native-app §A4: "switching modes is
   an edit-and-relaunch operation … never a runtime toggle").
6. **Untrusted server text renders as text.** `Message`, `ServerVersion`, and
   `APIVersion` keep the `textContent` rule in the `connectResult` doc
   (`go/cmd/compass-app/bridge_service.go`).
7. **Build tags.** `bridge_service.go` and its tests are `unix`. Only
   Wails-importing code is `(linux && gtk4) || darwin`; new logic sits behind
   injected funcs so it is testable under `unix`.
8. **Repo hygiene.** No shims or dead branches; comments say why; no issue IDs
   in source (AGENTS.md § Hygiene). Public repo: no private-repo references.

## Approach

### A1 — First run is a chooser

Today an absent app.toml resolves to embedded (`Load` in
`go/internal/appconfig/appconfig.go`: "Absent file → embedded zero-config
onboarding default; not an error"), and the bring-up runs before any window
exists. A user who wants a server gets a local stack they did not ask for or,
without podman, `exit 1` with no window (`run` in `go/cmd/compass-app/main.go`
returns the `launch` error before `application.New`).

Ruled (OQ-1 (a)): when `Load` reports no file and no override
(`appconfig.ErrNoConfig`), the window opens at once to two choices:

- **Run Compass on this computer** (primary). The shell runs the embedded
  preflight in the window behind a pending line; on a Mac the first check can
  take minutes because it may create the podman machine. On pass it writes
  `mode = "embedded"` and tells the user to quit and reopen Compass (OQ-6). The
  next launch takes today's embedded path unchanged. On failure the window
  shows the error and both choices again, and nothing is written.
- **Connect to a server.** The connect form with an empty URL, an optional CA
  file, and the token (A3, A4).

History for the OQ-1 ruling: embedded-revival OQ-2 weighed exactly this
("absent → a chooser screen ('run locally / connect to a stack')") and
recommended a cheaper first-run confirm instead. The confirm never shipped:
DL-320 has no confirm clause and `go/cmd/compass-app` has none. So the chooser
replaces a confirm-free default. It also reverses the compass-native-app §A4
charter ("the single-user charter forbids a question screen on first run"),
which expected the reversal in this shape: "A first-run wizard … is a UI
nicety layered on this same config later, not an alternative mechanism".

Unchanged: precedence is flag > env > file; `--mode embedded` and
`--mode client` behave as today; a present but invalid app.toml still fails
launch; graduation from embedded to client stays a config edit. Deleting
app.toml brings the chooser back but does not stop a lingering embedded stack
(DL-108 linger-by-default); "Quit and stop stack" first.

### A2 — Persist by writing app.toml

`appconfig.SaveClient` writes `mode = "client"`, the normalized `server_url`,
and `ca_cert` only when the user picked a CA file; `SaveEmbedded` writes
`mode = "embedded"`. Each encodes with `toml.NewEncoder` after a comment
header saying the app wrote the file, then re-`Parse`s the bytes. A picked CA
is written first to a new `server-ca-<16 hex>.pem` beside app.toml, created
with `O_EXCL`, so no existing trust anchor is ever replaced; `ca_cert` names
that copy (OQ-4), so a moved download cannot break a later launch. The TOML
goes to a same-directory temp file, is fsynced, and is linked to `app.toml`
with `os.Link`. The link fails if `app.toml` exists, so a file written
meanwhile by another process or by hand is never replaced; that failure
returns `ErrConfigExists`. The link is the commit point: after it the save has
succeeded, and a following directory fsync failure is logged, not returned.
Every exit removes the temp file, and every returned failure also removes the
new CA copy, so a returned error always means no `app.toml` was written. The
next launch reads the file through the unchanged parse path.

### A3 — The URL reaches the running shell (OQ-2)

`runClient` builds one TLS target that the pump and the service share
(`runClient` in `go/cmd/compass-app/client.go`: "The ONE-target invariant: the
pump and the service MUST share this single *bridge.Target instance"). In
setup there is no target yet, so `Connect` gains an optional server choice:

1. Normalize the URL (failure kind `invalid-url`).
2. Take the CA bytes the user picked in the native dialog; none means system
   trust. Build a candidate with `bridge.NewTLSTarget` (kind `invalid-ca` on
   its "CA PEM contained no usable certificate" error).
3. Run the existing probe (GetServerInfo → API version → WhoAmI) against the
   candidate.
4. On success: `SaveClient`, then store the token under the normalized URL,
   then install the candidate and a new pump on it as one atomic value. Saving
   first means the token is written only by the process that created
   app.toml, so a lost first-run race never touches another process's token.
   A token-store failure after the save asks the user to reopen and enter the
   token again, which the configured connect screen already supports.

The webview never names a file. `PickCACert` reads the picked file in the
shell and returns an opaque ref, which is all `Connect` accepts.

Every window boots its own UI (`index.tsx` runs `bootForMode` per window),
and a restored window set can open several chooser windows. One first-run gate
covers both choices across all windows: one attempt at a time, and once a save
lands the choice is final for the process. Outside setup mode a server choice
is refused. After setup, plain token connects use the installed connection.

**Normalization** lives in appconfig, so the file gets it too. It trims space
and one trailing `/` and rejects any path, query, or fragment. The probe's
client trims (`NewCompassServiceClient` in
`go/gen/compass/v1/compassv1connect/compass.connect.go`:
`baseURL = strings.TrimRight(baseURL, "/")`), but the pump concatenates
(`Pump.Do` in `go/internal/bridge/pump.go`: `p.target.baseURL+call.Path`), and
`call.Path` already carries the whole URL path (`createDaemonFetch` in
`apps/ui/src/daemon-transport.ts`: `const path = url.pathname + url.search`).
So `https://h:8443/` sends `//compass.v1…` and `https://h/p` sends
`/p/p/compass.v1…` on every bridged RPC after a passing probe. Rejecting a
path tightens the file contract: such a file now fails launch with a clear
message, where before it launched and failed every call. A token stored under
a trailing-slash URL is not found after the upgrade, because the tokenstore
key is the URL; the user pastes it once.

The validator returns `*appconfig.URLError`. `Error()` keeps today's text for
the file path; `Reason` is the sentence the UI shows.

### A4 — UI

`ShellMode` gains `"setup"`, and `bootForMode` routes it to a new `bootSetup`
screen with the two choices. "Connect to a server" opens the `boot-native.ts`
form in its setup entry:

- an editable URL field;
- "Choose CA certificate…" (native dialog through `PickCACert`), and a CA row
  reading "System trust" or the picked file's name, with "Use system trust" to
  clear it;
- the token field, and copy for `invalid-url` and `invalid-ca`.

On success the provider is built from the URL that connected. A configured
client keeps today's read-only "Server: <url>" line and token-only form
(OQ-5 deferred). New windows take their startup globals from the live
connection, so a window opened after setup boots as a client.

## Alternatives considered

The settings-store and CA alternatives are under OQ-3 and OQ-4.

- **Relaunch after a choice** (OQ-2, OQ-6). Rejected. For client it probes
  twice and reopens the window. For both it adds a detached child that escapes
  the `spawnSync` `tools/compass-app-dev` runs the app under, and a direct exec
  of the bundle binary is a GUI relaunch path the app has never exercised.
- **Validate the URL in TypeScript too.** Rejected: two validators drift, and
  the bound call is local IPC.
- **A `--server-url` flag or env var.** Rejected: still a hand step, and it
  does not persist.

## Plan

T-1, T-2a, and T-4 do not depend on each other. T-2b needs T-1 and T-2a; T-3
needs T-2b; T-5 lands last. T-4 codes against the method names and JSON shapes
fixed below.

### T-1 — appconfig: no-config signal, one validator, the writer

- **Do:**
  - `Load` returns `ErrNoConfig` when the file is absent and the override is
    empty. With an override and no file it resolves as today (`embedded` →
    embedded; `client` fails in `parseClient`). `Mode` keeps its two values.
  - Replace `validateServerURL` with `NormalizeServerURL`: trim space,
    `url.Parse`, require https, a host, no userinfo, a path of `""` or `/`, no
    query, no fragment; return `"https://" + u.Host`. `parseClient` stores the
    result.
  - Each failure is a `*URLError`. The four existing checks keep their exact
    `Error()` text. The new one reads
    `appconfig: server_url %q must not include a path, query, or fragment (e.g. https://host:8443)`.
  - Export `configPath` as `ConfigPath`.
  - `SaveClient` and `SaveEmbedded` per A2: directory 0700, files 0600, through
    a package-local create-exclusive helper (`certgen.atomicWrite` and
    tokenstore's `fileStore.writeAtomic` rename over the target, so they are
    the wrong primitive here). An existing `app.toml` returns
    `ErrConfigExists`. `SaveClient` refuses a `cfg.Mode` other than
    `ModeClient`. Any failure, including a failed re-`Parse`, leaves no
    `app.toml`, no new CA copy, and no temp file.
  - Update `doc.go` ("precedence is override > file > embedded-default",
    "the zero-config onboarding default").
- **Interfaces:**

  ```go
  var ErrNoConfig = errors.New("appconfig: no app.toml and no mode override")
  var ErrConfigExists = errors.New("appconfig: app.toml already exists")

  type URLError struct {
      URL    string // the raw input
      Reason string // UI sentence, e.g. "The server URL must use https."
  }
  func (e *URLError) Error() string

  func Load(configHome, home, override string) (Config, error) // unchanged signature
  func NormalizeServerURL(raw string) (string, error)          // error is *URLError
  func ConfigPath(configHome, home string) (string, error)
  func SaveClient(path string, cfg Config, caPEM []byte) (Config, error) // returns what it wrote
  func SaveEmbedded(path string) error
  ```

- **Test cycle** (red first, `appconfig_test.go`):
  - `NormalizeServerURL`: `" https://h:8443/ "` → `"https://h:8443"`.
    `http://h`, `h:8443`, `/x`, `https://u:p@h`, `https://h/p`, `https://h/p/`,
    `https://h?x=1`, and `https://h#f` each return a `*URLError`; the first
    four keep today's `Error()` text.
  - A file with `server_url = "https://h:8443/"` loads as `"https://h:8443"`;
    one with `"https://h/p"` fails.
  - No file, no override → `errors.Is(err, ErrNoConfig)`. No file with
    `"embedded"` → embedded.
  - `SaveClient` with `caPEM` → `Load` returns client mode, the URL, and
    `CACert` naming a `server-ca-*.pem` in the dir that holds the bytes.
    Without `caPEM`, no `ca_cert` and no CA file. `SaveEmbedded` → `Load`
    returns embedded. `SaveClient` with `http://h` fails.
  - Onto an existing `app.toml` and `server-ca-*.pem`, both savers return
    `ErrConfigExists` and leave both byte-identical.
  - Every failure above leaves no temp file and no new CA file in the dir. An
    injected directory-fsync failure after the link returns success, and
    `Load` reads the saved file.

### T-2a — Shell: one connection value (behaviour-preserving)

- **Do:**
  - Replace `bridgeService.pump` and `.target` with
    `conn atomic.Pointer[connection]`. `run` loads it once per call; with none
    it emits one `frameKindError` frame, "Not connected to a server", and
    finishes.
  - Factor `Connect`'s arm → GetServerInfo → API version → WhoAmI into
    `probe`, which arms the target, disarms on failure, leaves it armed on
    success, and stores nothing. Plain `Connect` keeps today's token lookup,
    store, messages, and fail-closed path when there is no target or
    tokenstore.
  - `shellState` reads the connection and a separate `setupPhase` without a
    lock, so a New Window click never waits on `connectMu`. An installed
    connection wins; with none it returns the phase. `setupPhase` is never a
    connection, so `CompassRPC` stays on the no-connection error in `"reopen"`.
  - Move the six `newBridgeService` call sites to `&connection{…}`:
    `runClient`, `launch`'s embedded arm, `bridge_service_test.go`,
    `bridge_service_connect_test.go` (two sites), and
    `multiwindow_e2e_test.go`.
- **Interfaces:**

  ```go
  type connection struct {
      mode      string         // appconfig mode string: "embedded" | "client"
      serverURL string         // client only
      target    *bridge.Target // client only; nil in embedded
      pump      *bridge.Pump
  }

  func newBridgeService(conn *connection, events eventEmitter, tokens tokenstore.Store) *bridgeService

  // bridgeService gains phase atomic.Pointer[string]: "setup" until a
  // first-run choice ends, then "reopen". Unused on a configured service.
  func (s *bridgeService) setPhase(phase string)

  // shellState returns the startup globals for a new window: the installed
  // connection's (mode, serverURL), else (phase, "").
  func (s *bridgeService) shellState() (mode, serverURL string)

  // ShellState is the bound form, so a window can re-check after it
  // subscribes to setup:decided.
  func (s *bridgeService) ShellState() shellStateResult // {mode, serverUrl}

  // probe runs the connect probe against target with token. It does not
  // store the token.
  func (s *bridgeService) probe(ctx context.Context, target *bridge.Target, token string) connectResult
  ```

- **Test cycle:** the existing `bridge_service_test.go` and
  `bridge_service_connect_test.go` assertions pass unchanged (they are the
  regression net). New: a service with no connection emits exactly one error
  frame for `CompassRPC`, including after `setPhase("reopen")`, and
  `shellState` returns `("reopen", "")` then.

### T-2b — Shell: `Connect` with a server choice

- **Do:**
  - `firstRunGate`: `begin` fails with "Another window is setting up Compass."
    while an attempt runs, and with "Compass is already set up. Quit and
    reopen it to change this." after a decision. `end(false)` releases it.
  - `decide(g, svc, clientInstalled bool)` is the one terminal step for every
    path that ends first run: it makes the gate final, then, unless
    `clientInstalled`, calls `svc.setPhase("reopen")`, then emits
    `setup:decided`. State is set before the event, so a window that reads
    `ShellState` after it subscribes cannot miss the decision.
  - `caPicks`: the shell-side store of picked CA bytes, keyed by a 128-bit
    random hex ref. `get` leaves the entry, so a failed probe can be retried
    with the same pick; `clear` drops every entry after a save.
  - `newSetupBridgeService` builds the setup-mode service: no connection, a
    tokenstore, and the setup wiring. `newBridgeService` services have no
    setup wiring.
  - `Connect` with `Server != nil`:
    1. no setup wiring → kind `other`, "The server is set in app.toml.",
       nothing built;
    2. `gate.begin()`; its error → kind `other`. Every later failure calls
       `end(false)`;
    3. `NormalizeServerURL` → `invalid-url`, message `URLError.Reason`;
    4. non-empty `CARef`: unknown → `invalid-ca`, "Choose the certificate
       again."; `NewTLSTarget` error → `invalid-ca`, "The file is not a PEM
       certificate.";
    5. an empty token reads the stored token for the normalized URL, as plain
       `Connect` does;
    6. `probe` the candidate; failure → its kind;
    7. `SaveClient(configPath, Config{Mode: ModeClient, ServerURL: url},
       caPEM)`, then store the token. The token is written only after this
       process owns `app.toml`, so a lost race never touches another
       process's token. `ErrConfigExists` disarms the candidate, calls
       `decide(g, svc, false)`, and returns kind `other` with the "already
       set up" message. Any other `SaveClient` failure disarms, calls
       `end(false)`, and returns kind `other` ("Connected, but the settings
       could not be saved: <err>"). A token-store failure after the save
       disarms the candidate, calls `picks.clear()` and
       `decide(g, svc, false)`, installs nothing, and returns kind `other`
       ("Saved, but could not store the token. Quit and reopen Compass, then
       enter it again.");
    8. install `&connection{mode: "client", serverURL: url, target: candidate,
       pump: bridge.NewPump(candidate)}`, then `picks.clear()` and
       `decide(g, svc, true)`. Other open setup windows read `ShellState` on
       the event and boot as the configured client.
  - Every successful `connectResult` carries `serverUrl`.
- **Interfaces:**

  ```go
  type firstRunGate struct {
      mu            sync.Mutex
      busy, decided bool
  }
  func (g *firstRunGate) begin() error
  func (g *firstRunGate) end(saved bool)
  // decide makes the gate final, sets "reopen" unless clientInstalled, then
  // emits setup:decided.
  func decide(g *firstRunGate, svc *bridgeService, clientInstalled bool)

  type caPicks struct {
      mu    sync.Mutex
      byRef map[string][]byte
  }
  func (p *caPicks) add(pem []byte) string
  func (p *caPicks) get(ref string) ([]byte, bool)
  func (p *caPicks) clear()

  type setupWiring struct {
      configPath string // appconfig.ConfigPath result
      gate       *firstRunGate
      picks      *caPicks
      saveClient func(path string, cfg appconfig.Config, caPEM []byte) (appconfig.Config, error) // appconfig.SaveClient; a seam for tests
  }
  func newSetupBridgeService(events eventEmitter, tokens tokenstore.Store, setup *setupWiring) *bridgeService

  type serverChoice struct {
      URL   string `json:"url"`
      CARef string `json:"caRef"` // "" = system trust; else a PickCACert ref
  }
  type connectRequest struct {
      Token  string        `json:"token"`
      Server *serverChoice `json:"server,omitempty"`
  }
  // connectResult gains:  ServerURL string `json:"serverUrl"`
  const (
      connectKindInvalidURL = "invalid-url"
      connectKindInvalidCA  = "invalid-ca"
  )
  ```

- **Test cycle** (`bridge_service_connect_test.go`, the httptest TLS server,
  its CA added through `picks.add`, a `t.TempDir()` config path):
  - Success → `ok`, `serverUrl` normalized; `appconfig.Load` returns client
    mode, that URL, and `CACert` naming the copy; the token is stored under the
    URL; a following `CompassRPC` reaches the server.
  - No `app.toml` and no install after: `http://…` → `invalid-url`; an unknown
    ref and a non-PEM pick → `invalid-ca`; a wrong token → `bad-token`; no
    CA → `bad-cert`.
  - A wrong token with a CA ref, then the right token with the same ref →
    `ok`.
  - After a success, a different URL → `other` and `app.toml` byte-identical;
    a plain token `Connect` → `ok`.
  - A server choice on a `newBridgeService` client service → `other`, nothing
    written.
  - Gate: a second `begin` while busy fails; `end(false)` reopens; `decide`
    is final.
  - An injected `saveClient` error: kind `other`, no connection installed, no
    token written, the gate open after; a following valid attempt → `ok`.
  - An injected token-store failure after a good save: kind `other`, no
    connection installed, the candidate disarmed, `picks` empty, `shellState`
    → `("reopen", "")`, and one `setup:decided` event.
  - An `app.toml` (and token) created after the chooser opened → the
    "already set up" message, both byte-identical, `shellState` →
    `("reopen", "")`, and one `setup:decided` event.
  - A successful client setup emits one `setup:decided`, and `ShellState`
    then returns `("client", url)`.
  - Under `-race`: `CompassRPC` concurrent with a successful setup `Connect`.

### T-3 — Shell: setup launch, live window globals, embedded choice, CA dialog

- **Do:**
  - In `run`, `errors.Is(err, appconfig.ErrNoConfig)` takes the setup arm:
    `ConfigPath`, `tokenstore.New(stateDir)`, `newSetupBridgeService`,
    `setupService`, and `dialogService` sharing one gate and one `caPicks`. It
    resolves no compass-stack binary and builds no quit controller. Any other
    `Load` error aborts as today. `setupService` and `dialogService` are bound
    only in setup. `dialogService.app` is assigned after `application.New`,
    as `svc.events = app.Event` is.
  - `newAppWindow` computes `shellStartupJS(svc.shellState())` per window; the
    launch-time `startupJS` local goes. `shellStartupJS` is unchanged:
    `"setup"` and `"reopen"` inject no URL global.
  - `setupService.ChooseEmbedded`: `gate.begin()` → preflight → on failure
    `end(false)` and return its message → `SaveEmbedded`. On success,
    `decide(gate, svc, false)` and return `ok` with the embedded reopen text.
    On `ErrConfigExists`, the same `decide(gate, svc, false)` and the neutral "already set up"
    message, which names no mode. Any other error → `end(false)`. No relaunch
    and no quit. `main.go` wires preflight as `realPreflight(image)` under
    `bringUpTimeout`, which is already sized for darwin machine creation.
  - `dialogService.PickCACert`:
    `app.Dialog.OpenFile().SetTitle("Choose CA certificate").AddFilter("Certificates", "*.pem;*.crt").PromptForSingleSelection()`.
    Cancel returns the zero `pickedCA`. Otherwise it reads the file, calls
    `picks.add`, and returns the ref and base name. A read error is returned.
  - Fix the `--mode` help ("then app.toml, then embedded" → "then app.toml,
    else the first-run chooser"), the `resolveMode` doc, and `client.go`'s
    stale package doc ("retired in RIG-2554").
- **Interfaces:**

  ```go
  // setup_service.go, //go:build unix
  type setupResult struct {
      OK      bool   `json:"ok"`
      Message string `json:"message"` // rendered via textContent
  }
  type setupService struct {
      gate      *firstRunGate
      svc       *bridgeService // for decide
      preflight func(ctx context.Context) error
      save      func() error   // appconfig.SaveEmbedded(path)
  }
  func (s *setupService) ChooseEmbedded(ctx context.Context) setupResult

  // dialog_service.go, //go:build (linux && gtk4) || darwin
  type pickedCA struct {
      Ref  string `json:"ref"`  // "" on cancel
      Name string `json:"name"` // base name, for display
  }
  type dialogService struct {
      app   *application.App
      picks *caPicks
  }
  func (d *dialogService) PickCACert(ctx context.Context) (pickedCA, error)

  func newAppWindow(app *application.App, svc *bridgeService, name, title string)
  ```

- **Test cycle:** `setup_service_test.go` with fakes:
  - preflight fails → no save, and the gate is open after;
  - preflight passes → one save, `ok`, `shellState` → `("reopen", "")`, and
    one `setup:decided` event;
  - save fails → the message, and the gate is open after;
  - save returns `ErrConfigExists` → the "already set up" message (no mode
    named), `shellState` → `("reopen", "")`, one `setup:decided` event, and
    the gate final;
  - gate held by an in-flight connect → refused without running preflight;
  - after a save → refused.

  In `client_test.go`, `shellStartupJS("setup", "")` and
  `shellStartupJS("reopen", "")` yield the mode global and no URL global. The
  dialog is covered by T-5's smoke run.

### T-4 — UI: setup screen and first-run connect form

- **Do:**
  - `shell-globals.ts`: `"setup"` and `"reopen"` join `ShellMode`.
  - `daemon-transport.ts`, still the only `@wailsio/runtime` importer:
    - `ConnectResult` gains `serverUrl` and the two kinds;
    - `shellConnect` sends `{token}` or `{token, server}`;
    - add `pickCACert` (`main.dialogService.PickCACert`), `chooseEmbedded`
      (`main.setupService.ChooseEmbedded`), `shellState`
      (`main.bridgeService.ShellState`), and `quitApp` (`Application.Quit`).
  - New `boot-setup.ts`, with the two choices:
    - Embedded shows "Checking this computer… The first check on a Mac can
      take several minutes." Success shows "Compass is set up to run on this
      computer. Quit and reopen it to start." with a Quit button. Any other
      result shows its message; when the gate is final that message is the
      neutral "already set up" text, otherwise both choices return.
    - The window subscribes to `setup:decided`, then calls `shellState` once,
      and again on each event. `"client"` hands off to
      `deps.bootNativeClient(root, "configured")`, which reuses the installed
      connection; `"reopen"` shows the neutral reopen screen ("Compass is
      already set up. Quit and reopen it to change this." and Quit). Reading
      after subscribing means a decision made before the subscription is
      still seen.
    - While this window's own action is in flight, and after it has handed
      off or shown its result, it unsubscribes and ignores events, so its own
      decision cannot trigger a second transition.
    - Connect hands off to `deps.bootNativeClient(root, "setup")`. The default
      deps wrap `bootNativeClient(root, undefined, entry)`.
  - `boot-native.ts` setup entry: no auto-probe; the form from A4; submit
    disabled while the URL is empty; it always sends `server`; `invalid-url`
    and `invalid-ca` show `message` via `textContent`; on `ok` the provider
    uses `result.serverUrl`. The configured entry is unchanged.
  - `boot-mode.ts`: `BootModeDeps` gains `bootSetup`; `bootForMode` routes
    `"setup"` to it and `"reopen"` to the neutral reopen screen.
- **Interfaces:**

  ```ts
  export type ShellMode = "embedded" | "client" | "setup" | "reopen";

  export type ServerChoice = { url: string; caRef: string };
  export type PickedCA = { ref: string; name: string };
  export type SetupResult = { ok: boolean; message: string };
  export function shellConnect(token: string, server?: ServerChoice): Promise<ConnectResult>;
  export function pickCACert(): Promise<PickedCA>;
  export function chooseEmbedded(): Promise<SetupResult>;
  export function quitApp(): Promise<void>;

  export type NativeBootDeps = {
    shellConnect: (token: string, server?: ServerChoice) => Promise<ConnectResult>;
    pickCACert: () => Promise<PickedCA>;
    nativeConnectionProvider: (baseUrl: string) => ConnectionProvider;
  };
  export async function bootNativeClient(
    root: HTMLElement,
    deps?: NativeBootDeps,
    entry?: "configured" | "setup", // default "configured"
  ): Promise<ResolvedConnection | undefined>;

  export type SetupBootDeps = {
    chooseEmbedded: () => Promise<SetupResult>;
    shellState: () => Promise<{ mode: ShellMode; serverUrl: string }>;
    onSetupDecided: (fn: () => void) => () => void; // returns unsubscribe
    quitApp: () => Promise<void>;
    bootNativeClient: (root: HTMLElement, entry: "setup" | "configured") => Promise<ResolvedConnection | undefined>;
  };
  export async function bootSetup(
    root: HTMLElement,
    deps?: SetupBootDeps,
  ): Promise<ResolvedConnection | undefined>;
  ```

- **Test cycle** (Bun, injected fakes):
  - `boot-native.test.ts`:
    - the setup entry makes no probe call and sends `{url, caRef: ""}`;
    - a picked CA sends its ref and the row shows its name, and "Use system
      trust" clears it;
    - an `invalid-url` message with `<b>` renders literally;
    - the provider gets `serverUrl`;
    - the existing configured-entry tests stay green.
  - New `boot-setup.test.ts`:
    - an embedded failure re-renders both choices with the message;
    - success shows the embedded reopen text, and Quit calls `quitApp`;
    - a `setup:decided` event with `shellState` → `"reopen"` shows the
      neutral reopen screen; with `"client"` it calls `bootNativeClient`
      with `"configured"`;
    - a decision made before the subscription (the first `shellState` already
      returns `"reopen"`) shows the reopen screen with no event;
    - the window that chose embedded keeps its embedded success text when its
      own `setup:decided` arrives, and the window that connected boots once;
    - connect hands off with `"setup"`.
  - `boot-mode`: `"setup"` routes to `bootSetup`; `"reopen"` renders the
    neutral reopen screen.

### T-5 — Docs, examples, smoke run

- **Do:** update every place that says an absent file means embedded, or that
  the URL is only hand-edited:
  - `app-bundle/SMOKE.md` §3, its checklist, and the client section;
  - `docs/onboarding.md`;
  - `docs/specs/runtime/runner-tiers.md`, which quotes DL-320's "absent →
    embedded";
  - `tools/compass-app-dev/app.toml.example` and its README ("REQUIRED",
    "there is no env var or CLI flag", the stale "Embedded mode was retired"
    comment). Add that `server_url` must be the origin only, and that the app
    writes the file on first run;
  - `go/e2e/client_mode_test.go` writes its app.toml through
    `appconfig.SaveClient` instead of a literal (its NOTE asks for exactly
    this).

  SMOKE.md also notes two things: a token stored under a trailing-slash URL
  must be pasted once more, and deleting app.toml does not stop a lingering
  embedded stack.
- **Interfaces:** none.
- **Test cycle:** a smoke run on a Linux GTK4 build with an empty config home:
  1. connect with the stack's CA, then quit;
  2. reopen: it auto-connects, and app.toml holds the origin URL and a
     `ca_cert` naming a `server-ca-*.pem` copy;
  3. delete app.toml and choose embedded: the reopen message appears, and
     reopening runs the embedded bring-up;
  4. `http://x` and `https://h/p` each show their message and write nothing;
  5. open a second chooser window: after the first connects, the second's
     embedded choice is refused.

## Tasks

- [ ] T-1 — appconfig: `ErrNoConfig`, `NormalizeServerURL` + `URLError`,
  `ConfigPath`, `SaveClient`/`SaveEmbedded`, `doc.go`
- [ ] T-2a — shell: `connection` value, `probe` factored out, `shellState`;
  no behaviour change
- [ ] T-2b — shell: `Connect` server choice, `firstRunGate`, `caPicks`, new
  failure kinds
- [ ] T-3 — shell: setup launch arm, per-window startup globals,
  `ChooseEmbedded`, `PickCACert`, doc fixes
- [ ] T-4 — UI: `"setup"` mode, `bootSetup`, first-run connect form
- [ ] T-5 — SMOKE.md, onboarding, runner-tiers spec, dev example + README, e2e
  writer; smoke run

## Decisions (ruled)

Matt ruled on 2026-10-05 (RIG-4480): OQ-1 **(a)** and OQ-6 **(d)**, and
OQ-2, OQ-3 and OQ-4 adopt the recommended option (a). OQ-5 (an
editable URL on a configured client) is deferred: it is outside RIG-3102's
acceptance, it would rewrite hand-written files, and it needs a keep / system
/ file CA choice.

### OQ-1 — Where the URL entry appears (conflicts with DL-320) — load-bearing

DL-320: "absent → embedded" and "Graduation embedded→client is a config edit
… not an in-app flow". The acceptance case, "a first-run user with no app.toml
can enter a URL in the UI", cannot pass while absent means embedded.

- **(a) A first-run chooser on absent app.toml** (A1). Embedded stays the
  primary choice and graduation stays a config edit. It supersedes DL-320,
  reverses the compass-native-app §A4 charter, and reopens the chooser
  embedded-revival OQ-2 recommended against.
- **(b) Keep absent → embedded and prompt only when `mode = "client"` lacks
  `server_url`.** No ledger change, but the user still hand-writes TOML to
  reach the prompt, so it fails acceptance.
- **(c) An in-app "connect to a server" switch inside running embedded.** It
  contradicts DL-320's graduation clause, and a client-only user must pass the
  embedded preflight first.

**Ruled: (a)** (Matt, 2026-10-05), the only option that meets acceptance
without making client users pass through embedded.

### OQ-2 — How the TLS target gets a URL that arrives after startup

- **(a) Build a candidate in `Connect` and install it after a good probe,
  first run only** (A3). One probe, no window churn, no in-flight streams to
  drain.
- **(b) Save, then relaunch.** The probe runs twice (or the URL is saved
  unverified), and it brings the relaunch costs under Alternatives.
- **(c) A hot swap at any time.** It must drain streams and rebuild the UI's
  provider; that is a separate design.

**Ruled: (a).**

### OQ-3 — Where the choice persists

- **(a) Write back to app.toml** (A2). One documented source of truth. The
  app writes only on first run, so no hand-written file is rewritten.
- **(b) A UI-owned store in the state dir.** Two sources and a precedence
  rule; a stale entry silently wins.
- **(c) Both, with the store used when app.toml is absent.** The same
  precedence problem, and deleting app.toml no longer resets.

**Ruled: (a).**

### OQ-4 — The private CA from the UI

- **(a) Pick a PEM file and copy it to `server-ca.pem` beside app.toml.**
- **(b) Point `ca_cert` at the picked path.** No copy, but it breaks when the
  file moves.
- **(c) No CA in the UI.** System trust only; fails for every self-minted
  stack.
- **(d) Trust on first use with a fingerprint check.** A trust-model design of
  its own.

**Ruled: (a)**; (d) deferred.

### OQ-6 — How the embedded choice starts the stack — load-bearing

- **(a) In-window preflight, write `mode = "embedded"`, relaunch.** It adds
  the relaunch costs under Alternatives. After a click that closes the window,
  the child runs the cold three-image pull with no window, so it looks like a
  crash.
- **(b) Write and relaunch with no preflight.** A failure becomes a silent
  `exit 1`, and the chooser cannot return because app.toml now exists.
- **(c) In-window preflight, then bring the stack up in the same process and
  install a Unix-socket connection** (T-2a's value). No relaunch, and the
  cold pull runs behind a visible "Starting Compass…" line. It costs a
  run-time "Quit and stop stack" menu item, moving `accountID` into the
  connection value, a hand-off from `bootSetup` to `bootConnection`, and a
  retry path over a half-started lingering stack. It also bends Constraint 5.
- **(d) In-window preflight, write `mode = "embedded"`, ask the user to quit
  and reopen** (A1). No relaunch, no detached child, no `spawnSync` caveat,
  and no new process code. The next launch takes today's embedded path
  untouched. Cost: one extra click, and the reopened app's cold pull is
  today's pre-window wait.

**Ruled: (d)** (Matt, 2026-10-05). It meets the need with the least new code and no new
failure modes. (c) is the better long-term experience, but it belongs with the
provisioning-state UI that the `bringUpTimeout` doc in `main.go` says is "not
built yet", which would fix the same wait for every embedded launch, not only
the first. Under every option, recovering from a present but invalid app.toml
in the app stays out of scope.

## Ledger-impact

This PR adds DL-406 to `DECISIONS.md` (claimed with
`bun tools/dl-claim --ref RIG-3102`); its Record cell links §A1. It flips
DL-320 to `Superseded by DL-406 (Matt, 2026-10-05)`, with its Decision cell
unchanged. DL-109 stays `Active` and unedited; the partial override lives in
DL-406's text, as DL-319 does for DL-259.
