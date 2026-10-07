# Compass packaged-app smoke

This runbook validates the packaged `compass-app` in embedded and client modes.
Build the tarball with `moon run compass-app-bundle:build`, then unpack the
result. The bundle is a non-relocatable dev-box artifact. Its `bin` entry is a
`/nix/store` symlink. The bundle ships the `compass-app` shell and four
sidecars: `compass-stack`, `compass-server`, `compass-runner`, and
`compass-clear-token` (the sidecar build loop in `app-bundle/build.sh`). It does
not ship postgres tooling; embedded mode runs stock postgres:18 in a rootless
podman container (the bundle header and sidecar build loop in
`app-bundle/build.sh`).

Run this on one Linux dev box with the build's `/nix/store` realized. What is
under test is the packaged `compass-app`, so always launch it from the unpacked
bundle's `bin/`, never from a `go build` output. Part (b) step 1 is the one
exception, and it is not the app: it stands up a standalone headless stack
before the bundle exists.

## What the automated gates cover

- **`ci / e2e`** stands up a real headless stack with `compass-stack`,
  `compass-postgres`, and a podman-run agent container. It gates the stack-side
  bring-up.
- The **multi-window gtk4 e2e** lane compiles and runs `compass-app` under a
  virtual framebuffer and exercises the shell and windowing surface.

Neither gate drives the packaged tarball's webview against a live stack and a
real agent container. The packaged-app smoke therefore remains manual.

## Part (a): embedded mode

Embedded mode is the zero-config path. The app runs host preflight, brings up
the local stack, resolves the caller identity, and then opens the board. The
pipeline order is preflight, `compass-stack up`, then `WhoAmI`
(`runEmbedded` in `go/cmd/compass-app/embedded.go`: "pipeline ... WhoAmI").

### 1. Check embedded prerequisites

Embedded mode requires Linux or macOS, rootless podman, and podman 4.3 or
newer. These are fatal host checks. The agent image is checked locally but is
pulled from GHCR by the stack when it is missing
(`Deps.Run` in `go/internal/preflight/preflight.go`, `runEmbedded` in
`go/cmd/compass-app/embedded.go`). Confirm rootless podman and the
image before the smoke to avoid a cold pull:

```bash
podman info --format '{{.Host.Security.Rootless}}'   # -> true
podman version --format '{{.Client.Version}}'         # -> 4.3 or newer
podman pull ghcr.io/rigelbuild/compass-agent:latest
```

### 2. Build and unpack the bundle

```bash
moon run compass-app-bundle:build
PREFIX=$(mktemp -d)
tar -xzf app-bundle/compass-app-<version>-linux-amd64.tar.gz -C "$PREFIX"
BUNDLE="$PREFIX/compass-app-<version>-linux-amd64"
```

`<version>` is the value from `version.txt`, followed by `+g<short-sha>`. Keep
the bundle's `bin/compass-app`,
`bin/compass-stack`, `bin/compass-server`, `bin/compass-runner`, and
`bin/compass-clear-token` together. The build stages all four sidecars into that
directory (the sidecar build loop in `app-bundle/build.sh`).

### 3. Launch with no `app.toml`

Use a temporary config home so the smoke does not remove or replace a user
configuration. With no `app.toml` and no `--mode` or `COMPASS_APP_MODE`
override, launch opens the first-run chooser. Choose **Run Compass on this
computer**; the app runs preflight in the window. After preflight succeeds, it
writes `mode = "embedded"` once and asks you to quit and reopen Compass. It
never rewrites an existing config file.

Deleting `app.toml` does not stop a lingering embedded stack; use **Quit and
stop stack** before removing the config file (Approach A1: deleting `app.toml`
brings back the chooser but leaves the stack running).

```bash
ECONFIG=$(mktemp -d)
export XDG_CONFIG_HOME="$ECONFIG"
APP_CONFIG="$ECONFIG/compass/app.toml"
rm -f "$APP_CONFIG"
```

The socket defaults to `$XDG_RUNTIME_DIR/compass/server.sock`, falling back to
`$HOME/.compass/server.sock`; inspect both locations before and after the run
so residue from an unpinned launch is not mistaken for a clean teardown
(`resolveSocket` in `go/cmd/compass-app/main.go`).

The stack binary resolution order is the `--compass-stack` flag,
`COMPASS_STACK_BIN`, a `compass-stack` sibling of the running `compass-app`,
then `PATH` (`resolveStackBin` in `go/cmd/compass-app/embedded.go`). For this smoke, do not
pass `--compass-stack` and require all launch overrides to be unset:

```bash
unset COMPASS_STACK_BIN COMPASS_APP_MODE COMPASS_AGENT_IMAGE \
  COMPASS_STATE_DIR COMPASS_SOCKET COMPASS_ASSETS_DIR COMPASS_DATABASE_DSN
find "${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/compass" "$HOME/.compass" \
  -maxdepth 2 \( -type s -o -type f \) 2>/dev/null || true
```

`COMPASS_ASSETS_DIR` must be clear in particular, or the app can serve a UI `dist`
from outside the bundle. `COMPASS_DATABASE_DSN` must also be clear so the smoke
uses the bundle's state-directory database configuration.

The app resolves `compass-stack` as a sibling of the running `compass-app`
executable, preferred over PATH (`resolveStackBin` in `go/cmd/compass-app/embedded.go`), and
prepends that same `bin/` directory for the supervised sidecars
(`prependExecDirToPath` in `go/cmd/compass-app/embedded.go`). So the bundle's staged
`compass-stack` wins even when an ambient one is on PATH, which is what the
launch below relies on.

Pin the embedded stack's state directory and socket. Left unset they default
under `$HOME/.compass` (`resolveStateDir` in
`go/cmd/compass-app/client.go`), which mixes smoke state into the real dev-box
install and leaves nothing safe to delete afterwards:

```bash
ESTATE=$(mktemp -d); ERT=$(mktemp -d)
BINENV=$(nix build --no-link --print-out-paths \
  -f tools/toolchain/gtk-e2e-env.nix bin)
PATH="$BINENV/bin:$BUNDLE/bin:$PATH" \
  xvfb-run -a "$BUNDLE/bin/compass-app" \
    --state-dir "$ESTATE" --socket "$ERT/server.sock" \
    2>"$ERT/app.log"
```

Quit Compass, then rerun the command above with the same `--state-dir`
and `--socket` paths. The next launch starts the embedded stack under those
pinned paths.

`xvfb-run` is the same virtual-framebuffer setup the client part uses, needed on
a headless box.

With those flags the app invokes this stack command:

```text
compass-stack up --state-dir <state-dir> --image ghcr.io/rigelbuild/compass-agent:latest --socket <socket>
```

`stackUpArgs` passes only `up`, `--state-dir`, `--image`, and `--socket`
(`stackUpArgs` in `go/cmd/compass-app/embedded.go`). It deliberately does not pass
`--database`, `--postgres-image`, `--collector-image`, or `--listen`. The image
ref is the locked GHCR default unless `--image` or `$COMPASS_AGENT_IMAGE`
overrides it (`resolveImage` in `go/cmd/compass-app/embedded.go`).

### 4. Confirm the embedded board and run one session

Wait for the app to bring the stack to Ready. It then resolves the caller with
`WhoAmI` over the local socket (`runEmbedded` in `go/cmd/compass-app/embedded.go`).
Confirm that the app opens the board directly, without a client connect screen
or bearer entry. Embedded mode has no client `server_url` or `ca_cert`
configuration (`Parse` in `go/internal/appconfig/appconfig.go`:
"client-only fields"), and its identity
comes from that local-socket call.

From the board, start one agent session. Confirm that it reaches a running
agent container under the stack's podman runtime.

### 5. Quit and stop the embedded stack

Use the explicit **Quit and stop stack** action, not a plain window close or
OS quit. Plain close exits the app but leaves the detached stack running for a
later relaunch; **Quit and stop stack** runs `compass-stack down` and then quits
the app (`stopStackAndQuit` in `go/cmd/compass-app/lifecycle.go`). Do not run a manual
`compass-stack down` for this part. After the app closes, confirm that no stack
containers or private postgres container remain:

```bash
podman ps -a --filter name='^compass-(postgres|otel-collector|nats|gateway|agent)-'
```

The filter should match the stack's containers while it is up. After teardown,
an empty result is meaningful.

A lingering stack here is a real failure, but the app exits either way: if
`compass-stack down` fails the app still quits and logs the error, because
trapping the user in a live window is worse and a lingering stack is the safe
failure (OQ-6, `stopStackAndQuit` in `go/cmd/compass-app/lifecycle.go`). Check both the app
stderr log and podman output; teardown is green only when the log has no
teardown error and no stack containers remain.

Remove the unpacked bundle and the pinned stack state after confirming teardown.
Pinning them in step 3 is what makes this safe to delete: an unpinned run writes
into the real `$HOME/.compass` install instead.

```bash
rm -rf "$ECONFIG" "$PREFIX" "$ESTATE" "$ERT"
```

Embedded mode stores no bearer, so there is no keychain entry to clear here
(the local socket is a filesystem-permission boundary, not a bearer door).

## Part (b): client mode

### 1. Bring up a headless stack to connect to

The stack runs the agent container over rootless podman. Pre-pull the image so
bring-up does not cold-pull:

```bash
podman info --format '{{.Host.Security.Rootless}}'   # -> true
podman pull ghcr.io/rigelbuild/compass-agent:latest
```

Stand up the stack with its TLS network door on the loopback port. The client
https-only (`Parse` in `go/internal/appconfig/appconfig.go`: "https" validation).
`compass-stack` generates the loopback certificate under `--state-dir` and
passes it to `compass-server` (`EnsureCert` in `go/internal/stack/adapters/cert.go`,
`serverSpec` in `go/internal/stack/spec.go`).

This step runs the stack as a standalone headless deployment, before the bundle
is built, so `compass-stack` here is any working build on PATH rather than the
bundle's staged sidecar. Use a separate pinned state directory for both client
launches; the packaged app must never share the standalone stack's state:

```bash
CSTATE=$(mktemp -d); CAPPSTATE=$(mktemp -d); CRT=$(mktemp -d)
compass-stack up \
  --state-dir "$CSTATE" --socket "$CRT/server.sock" \
  --listen 127.0.0.1:50052 --linger \
  --image ghcr.io/rigelbuild/compass-agent:latest
```

The spawned server writes a bootstrap-admin token at `$CRT/admin-token`.
`adminTokenFile` names that file (`go/server/network_door.go`: "admin-token"); when
`--state-dir` is omitted for the network door, the socket parent is the state
directory (`buildNetworkServer` in `go/server/network_door.go`), and the token is minted and
written there with the writer (`writeTokenFile` in `go/server/network_door.go`). Read it
for the connect screen:

```bash
cat "$CRT/admin-token"
```

The client trust anchor is `$CSTATE/tls.crt`, and its server URL is
`https://127.0.0.1:50052`.

### 2. Build and unpack the client bundle

```bash
moon run compass-app-bundle:build
PREFIX=$(mktemp -d)
tar -xzf app-bundle/compass-app-<version>-linux-amd64.tar.gz -C "$PREFIX"
BUNDLE="$PREFIX/compass-app-<version>-linux-amd64"
```

The resolved client `app.toml` path is
`${XDG_CONFIG_HOME:-$HOME/.config}/compass/app.toml`. For this smoke, use an
isolated temporary config home so the app config and copied CA are safe to
remove during cleanup:

```bash
CCONFIG=$(mktemp -d)
export XDG_CONFIG_HOME="$CCONFIG"
APP_CONFIG="$CCONFIG/compass/app.toml"
rm -f "$APP_CONFIG"
```

Launch with no file and no `--mode` or `COMPASS_APP_MODE` override. The app
opens the first-run chooser. It writes the config and CA copy only after a
successful connection, and never rewrites an existing file. Tokenstore saves
the bearer under the exact URL, using the OS keychain or its 0600-file fallback;
the token is never written to `app.toml` (DL-109).

`NormalizeServerURL` removes a root trailing slash. A token stored under the
slash-terminated URL is not found under the normalized origin because tokenstore
matches the exact URL. Paste it once more to save it under the origin
(`NormalizeServerURL` in `go/internal/appconfig/appconfig.go` and `Store.Read`
in `go/internal/tokenstore/tokenstore.go`).

### 3. Launch, connect, and render the board

```bash
BINENV=$(nix build --no-link --print-out-paths \
  -f tools/toolchain/gtk-e2e-env.nix bin)
PATH="$BINENV/bin:$BUNDLE/bin:$PATH" \
  xvfb-run -a "$BUNDLE/bin/compass-app" \
    --state-dir "$CAPPSTATE" --socket "$CRT/server.sock" 2>"$CRT/app.log"
```

After the chooser opens, choose **Connect to a server**. In the form, enter
`https://127.0.0.1:50052`, choose `$CSTATE/tls.crt`, paste the token from
`$CRT/admin-token`, then connect. The shell probes `GetServerInfo`, calls
`WhoAmI`, writes the config and CA copy, stores the token, arms the bearer
injector, and boots into the board (`connectServerChoice` in
`go/cmd/compass-app/bridge_service.go`). Confirm the board renders live over
the TLS door.

Inspect the saved config. Confirm that it contains the origin and that
`ca_cert` names the copied `server-ca-*.pem` file beside `app.toml`:

```bash
cat "$APP_CONFIG"
CA_CERT=$(sed -n 's/^ca_cert = "\(.*\)"$/\1/p' "$APP_CONFIG")
test "$(sed -n 's/^server_url = "\(.*\)"$/\1/p' "$APP_CONFIG")" = \
  "https://127.0.0.1:50052"
test "$(dirname "$CA_CERT")" = "$(dirname "$APP_CONFIG")"
test -f "$CA_CERT"
case "$(basename "$CA_CERT")" in
  server-ca-*.pem) ;;
  *) echo "FAIL: unexpected CA filename: $CA_CERT" >&2; exit 1 ;;
esac
```

### 4. Drive one agent session to a running container

From the board, start one agent session. Confirm the session reaches a running
container under the stack's podman runtime.

### 5. Quit and relaunch from the keychain

Quit the app, then relaunch it:

```bash
PATH="$BINENV/bin:$BUNDLE/bin:$PATH" \
  xvfb-run -a "$BUNDLE/bin/compass-app" \
    --state-dir "$CAPPSTATE" --socket "$CRT/server.sock" \
    2>>"$CRT/app.log"
```

Auto-connect reads the stored bearer from tokenstore's selected backend (the OS
keychain or 0600-file fallback) and boots straight to the board, with no connect
screen or bearer re-entry (`bridgeService.Connect` in
`go/cmd/compass-app/bridge_service.go`: `tokens.Read(serverURL)`). The keyring
entry is keyed by service `compass-app` and the exact server URL.

To check the configured-client form, clear the stored origin token with the
helper, quit the app, and relaunch:

```bash
"$BUNDLE/bin/compass-clear-token" \
  --server-url "https://127.0.0.1:50052" \
  --state-dir "$CAPPSTATE"
```

The configured client should show a read-only server URL and one bearer input.
Paste the token again to reconnect. The restart from step 5 already cleared the
normalized token. Continue below to clear a legacy trailing-slash entry if needed.

The bearer outlives all of that. Step 3 stored it under the OS keychain service
`compass-app`, keyed by the exact server URL, or in a 0600 `remote-token` file
under the state dir when no keychain backend is available (`tokenFileName` and
`New` in `go/internal/tokenstore/tokenstore.go`: "fallback file under the
caller-supplied state dir"). The packaged app has no logout action, so use the
bundled `compass-clear-token` helper. Its `run` function in
`go/cmd/compass-clear-token/main.go` reads the stored entry only to confirm the
URL matches, discarding the token, then passes the URL to `Store.Delete`. The
credential is never printed. Use the exact normalized URL from `app.toml`:

```bash
"$BUNDLE/bin/compass-clear-token" \
  --server-url "https://127.0.0.1:50052" \
  --state-dir "$CAPPSTATE"
```

If an older build stored the token under a trailing-slash URL, the normalized
URL above is a different key. On the keychain backend, clear that old key too:

```bash
"$BUNDLE/bin/compass-clear-token" \
  --server-url "https://127.0.0.1:50052/" \
  --state-dir "$CAPPSTATE"
```

The helper is silent on success. It also exits 0 if it found a mismatched URL or
nothing, so confirm the outcome yourself rather than treating exit 0 as proof.
On the file-fallback path the entry is a file:

```bash
if test -e "$CAPPSTATE/remote-token"; then echo "FAIL: remote-token still present" >&2
else echo "file-backend token cleared"; fi
```

Which backend is bound depends on whether a Secret Service is reachable. A
headless smoke box usually has none, so the file check above is the one that
applies. If a keychain is bound, its entry lives outside the state directory.
Probe the keyring by exact key, keeping the secret off the capture and
separating a real miss from a failed probe:

```bash
if err=$(secret-tool lookup service compass-app \
     username "https://127.0.0.1:50052" 2>&1 >/dev/null); then
  echo "keychain entry STILL PRESENT"
elif [ -n "$err" ]; then
  echo "LOOKUP FAILED, entry state UNKNOWN: $err"
else
  echo "keychain entry cleared"
fi
```

`secret-tool lookup` exits 1 both when the entry is absent and when it cannot
reach a Secret Service, so a bare `||` would report "cleared" for a probe that
never ran — the same false all-clear this step exists to prevent. The three-way
form above separates them, and treats an absent `secret-tool` as UNKNOWN too.
The `2>&1 >/dev/null` order matters: stderr is duplicated onto the capture
first, then fd 1 is sent to `/dev/null`, so the secret is never captured.

Use `lookup`, never `secret-tool search`: `search` loads and prints the secret
itself, which would dump a still-live bearer into the terminal on exactly the
path this check exists to catch. `lookup` needs the exact key, so it also
confirms URL scoping. If you cleared an old trailing-slash key, probe that exact
key separately. A typo in `--server-url` is a silent no-op, and this check
catches that.

The client stack runs independently of the app. Stop it with `compass-stack down`
before removing the pinned state:

```bash
compass-stack down \
  --state-dir "$CSTATE" --socket "$CRT/server.sock" \
  --listen 127.0.0.1:50052 \
  --image ghcr.io/rigelbuild/compass-agent:latest
```

Deleting the embedded config file does not stop its stack. Use **Quit and stop
stack** before removing `app.toml` (§Part (a), 3).

After stopping the client stack and clearing its token, remove the isolated
client config home and pinned smoke state. Leave `$ECONFIG`, `$ESTATE`, and
`$ERT` until the embedded checklist has completed (§Part (a), 5):

```bash
rm -rf "$CCONFIG" "$PREFIX" "$CSTATE" "$CAPPSTATE" "$CRT"
```

## Manual checklist

### Embedded mode

- [ ] choosing **Run Compass on this computer** completes preflight, writes
      `mode = "embedded"` once, then asks for a reopen; the same pinned launch
      command starts the stack on the next run (§Part (a), 3)
- [ ] **Quit and stop stack** closes the app; `podman ps -a
      --filter name='^compass-(postgres|otel-collector|nats|gateway|agent)-'` is empty
      and `$ERT/app.log` reports no teardown failure (§Part (a), 5)
- [ ] the pinned `--state-dir`/`--socket` paths are removed and `$HOME/.compass`
      was not touched (§Part (a), 5)

### Client mode

- [ ] with no `app.toml`, `--mode`, or `COMPASS_APP_MODE` override, the app opens
      chooser; choosing **Connect to a server** accepts an editable HTTPS
      origin, optional CA file, and bearer, then writes client `app.toml` with
      the origin URL and copied `server-ca-*.pem`; it never writes the bearer
- [ ] a configured-client relaunch with the stored token cleared shows the
      read-only server URL and one bearer input; pasting the token connects and
      the board renders over TLS (§Part (b), 5)
- [ ] after a valid connection, `server_url` is the origin and `ca_cert` names
      a copied `server-ca-*.pem` beside `app.toml` (§Part (b), 3)
- [ ] with no config, `http://x` and `https://h/p` each show an error and leave
      `app.toml` absent; try each input separately (§T-5 smoke cycle)
- [ ] open a second chooser window, connect from the first, then confirm the
      second window's embedded choice is refused (§T-5 smoke cycle)
- [ ] one agent session reaches a running container (§Part (b), 4)
- [ ] quit and relaunch auto-connects from tokenstore, with no connect screen or
      bearer re-entry (§Part (b), 5)
- [ ] clear the token stored under the normalized origin; on a keyring backend,
      clear the older trailing-slash key too, then verify both URL entries are
      absent; on the file fallback, confirm its token file is absent (§Part (b), 6)
- [ ] the isolated config home and pinned smoke state are removed (§Part (b), 6)
