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
(`Pipeline.Run` in `go/internal/embedded/embedded.go`: "preflight → stack up → WhoAmI").

### 1. Check embedded prerequisites

Embedded mode requires Linux or macOS, rootless podman, and podman 4.3 or
newer. These are fatal host checks. The agent image is checked locally but is
pulled from GHCR by the stack when it is missing
(`Deps.Run` in `go/internal/preflight/preflight.go`, `Pipeline.Run` in
`go/internal/embedded/embedded.go`). Confirm rootless podman and the
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
APP_CONFIG="$ECONFIG/compass/app.toml"
rm -f "$APP_CONFIG"
```

The socket defaults to `$XDG_RUNTIME_DIR/compass/server.sock`, falling back to
`$HOME/.compass/server.sock`; inspect both locations before and after the run
so residue from an unpinned launch is not mistaken for a clean teardown
(`resolveSocket` in `go/cmd/compass-app/main.go`).

The stack binary resolution order is the `--compass-stack` flag,
`COMPASS_STACK_BIN`, a `compass-stack` sibling of the running `compass-app`,
then `PATH` (`ResolveStackBin` in `go/internal/embedded/embedded.go`). For this smoke, do not
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
executable, preferred over PATH (`ResolveStackBin` in `go/internal/embedded/embedded.go`), and
prepends that same `bin/` directory for the supervised sidecars
(`prependExecDirToPath` in `go/internal/embedded/embedded.go`). So the bundle's staged
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
```

Launch the app with the pinned paths:

```bash
XDG_CONFIG_HOME="$ECONFIG" PATH="$BINENV/bin:$BUNDLE/bin:$PATH" \
  xvfb-run -a "$BUNDLE/bin/compass-app" \
    --state-dir "$ESTATE" --socket "$ERT/server.sock" \
    2>>"$ERT/app.log"
```

Quit Compass, then rerun only the launch block above with the same pinned paths.
The append redirection preserves the first launch's log. The next launch starts
the embedded stack under those paths.

`xvfb-run` is the same virtual-framebuffer setup the client part uses, needed on
a headless box.

With those flags the app invokes this stack command:

```text
compass-stack up --state-dir <state-dir> --image ghcr.io/rigelbuild/compass-agent:latest --socket <socket>
```

`stackUpArgs` passes only `up`, `--state-dir`, `--image`, and `--socket`
(`stackUpArgs` in `go/internal/embedded/embedded.go`). It deliberately does not pass
`--database`, `--postgres-image`, `--collector-image`, or `--listen`. The image
ref is the locked GHCR default unless `--image` or `$COMPASS_AGENT_IMAGE`
overrides it (`ResolveImage` in `go/internal/embedded/embedded.go`).

### 4. Confirm the embedded board and run one session

Wait for the app to bring the stack to Ready. It then resolves the caller with
`WhoAmI` over the local socket (`Pipeline.Run` in `go/internal/embedded/embedded.go`).
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

### 3. Launch, validate setup, connect, and render the board

```bash
BINENV=$(nix build --no-link --print-out-paths \
  -f tools/toolchain/gtk-e2e-env.nix bin)
XDG_CONFIG_HOME="$CCONFIG" PATH="$BINENV/bin:$BUNDLE/bin:$PATH" \
  xvfb-run -a "$BUNDLE/bin/compass-app" \
    --state-dir "$CAPPSTATE" --socket "$CRT/server.sock" \
    2>>"$CRT/app.log" &
```

The app runs in the background so you can use the same interactive shell for
the checks below while its window stays open. With no config file or mode
override, it opens the first-run chooser. It writes the config and copied CA
only after a successful connection, and never rewrites an existing file.
Tokenstore saves the bearer under the exact URL, using the OS keychain or its
0600-file fallback; the token is never written to `app.toml` (DL-109).

`NormalizeServerURL` removes a root trailing slash. A token stored under the
slash-terminated URL is not found under the normalized origin because tokenstore
matches the exact URL. Paste it once more to save it under the origin
(`NormalizeServerURL` in `go/internal/appconfig/appconfig.go` and `Store.Read`
in `go/internal/tokenstore/tokenstore.go`).

Before connecting, open a second window via **Window → New Window** while the
first window still shows the chooser. Confirm the second window also shows the
chooser. In the first window, choose **Connect to a server**.

Test each rejected URL in the first window's form. Enter `http://x` and a
temporary non-secret value in the token field, then click **Connect**. Confirm
the form shows a validation message, then check that no config was written:

```bash
test ! -e "$APP_CONFIG" || echo "FAIL: invalid URL wrote app.toml" >&2
```

Enter a temporary non-secret token value again, since the form clears the token
after each attempt. Repeat with `https://h/p`. Confirm its validation message,
then check again:

```bash
test ! -e "$APP_CONFIG" || echo "FAIL: invalid URL wrote app.toml" >&2
```

In the first window, enter `https://127.0.0.1:50052`, choose
`$CSTATE/tls.crt`, paste the token from `$CRT/admin-token`, then connect. The
shell probes `GetServerInfo`, calls `WhoAmI`, writes the config and CA copy,
stores the token, arms the bearer injector, and boots into the board
(`connectServerChoice` in `go/cmd/compass-app/bridge_service.go`). Confirm the
board renders live over the TLS door. The still-open second window receives
the setup decision and boots as the configured client: confirm it leaves the
chooser and reaches the board without another connect. The refusal of a late
embedded choice is covered by `TestSetupServiceChooseEmbeddedGateRefusals`.

Inspect the saved config in this same shell while the app is running. Confirm
that it contains the origin and that `ca_cert` names the copied
`server-ca-*.pem` file beside `app.toml`:

```bash
cat "$APP_CONFIG"
CA_CERT=$(sed -n 's/^ca_cert = "\(.*\)"$/\1/p' "$APP_CONFIG")
test "$(sed -n 's/^server_url = "\(.*\)"$/\1/p' "$APP_CONFIG")" = \
  "https://127.0.0.1:50052" || echo "FAIL: unexpected server_url" >&2
test "$(dirname "$CA_CERT")" = "$(dirname "$APP_CONFIG")" || \
  echo "FAIL: CA file is not beside app.toml" >&2
test -f "$CA_CERT" || echo "FAIL: CA file is missing" >&2
case "$(basename "$CA_CERT")" in
  server-ca-*.pem) ;;
  *) echo "FAIL: unexpected CA filename: $CA_CERT" >&2 ;;
esac
```

### 4. Drive one agent session to a running container

From the board, start one agent session. Confirm the session reaches a running
container under the stack's podman runtime.

### 5. Quit and relaunch from tokenstore

Quit the app, then relaunch it with the same isolated config home and pinned
state paths:

```bash
XDG_CONFIG_HOME="$CCONFIG" PATH="$BINENV/bin:$BUNDLE/bin:$PATH" \
  xvfb-run -a "$BUNDLE/bin/compass-app" \
    --state-dir "$CAPPSTATE" --socket "$CRT/server.sock" \
    2>>"$CRT/app.log" &
```

Auto-connect reads the stored bearer from tokenstore's selected backend (the OS
keychain or 0600-file fallback) and boots straight to the board, with no connect
screen or bearer re-entry (`bridgeService.Connect` in
`go/cmd/compass-app/bridge_service.go`: `tokens.Read(serverURL)`). The keyring
entry is keyed by service `compass-app` and the exact server URL.

Clear the stored origin token with the helper:

```bash
"$BUNDLE/bin/compass-clear-token" \
  --server-url "https://127.0.0.1:50052" \
  --state-dir "$CAPPSTATE"
```

Quit the app after the helper completes. Relaunch it with the same launch
command above. The configured-client form should show the server URL as text
and one token input. Paste the token again to reconnect, then confirm the board
renders over TLS. Reconnecting stores the token under the origin again; §6
clears it after the remaining checks.

### 6. Cleanup

Quit Compass after the configured-client check. Clear the token stored under
the normalized origin; this step is required on both tokenstore backends:

```bash
"$BUNDLE/bin/compass-clear-token" \
  --server-url "https://127.0.0.1:50052" \
  --state-dir "$CAPPSTATE"
```

If the keyring backend is in use and an older build may have stored the token
under a trailing-slash URL, clear that optional legacy key too:

```bash
"$BUNDLE/bin/compass-clear-token" \
  --server-url "https://127.0.0.1:50052/" \
  --state-dir "$CAPPSTATE"
```

The helper is silent on success and exits 0 if it found a mismatched URL or
nothing, so verify the result below. On the file-fallback path, confirm that
the token file is absent:

```bash
if test -e "$CAPPSTATE/remote-token"; then echo "FAIL: remote-token still present" >&2
else echo "file-backend token cleared"; fi
```

Which backend is bound depends on whether a Secret Service is reachable. On a
keyring backend, probe both exact URL keys without printing either secret:

```bash
if err=$(secret-tool lookup service compass-app \
     username "https://127.0.0.1:50052" 2>&1 >/dev/null); then
  echo "keychain entry STILL PRESENT for normalized origin"
elif [ -n "$err" ]; then
  echo "LOOKUP FAILED, entry state UNKNOWN: $err"
else
  echo "keychain entry cleared for normalized origin"
fi

if err=$(secret-tool lookup service compass-app \
     username "https://127.0.0.1:50052/" 2>&1 >/dev/null); then
  echo "keychain entry STILL PRESENT for trailing-slash URL"
elif [ -n "$err" ]; then
  echo "LOOKUP FAILED, entry state UNKNOWN: $err"
else
  echo "keychain entry absent for trailing-slash URL"
fi
```

`secret-tool lookup` exits 1 both when the entry is absent and when it cannot
reach a Secret Service. The three-way checks separate those cases, and treat
an absent `secret-tool` as UNKNOWN. The `2>&1 >/dev/null` order captures stderr
first and then sends stdout to `/dev/null`, so the token is never captured.
Use `lookup`, never `secret-tool search`: `search` loads and prints the secret.

The client stack runs independently of the app. Stop it with `compass-stack down`
before removing the pinned state:

```bash
compass-stack down \
  --state-dir "$CSTATE" --socket "$CRT/server.sock" \
  --listen 127.0.0.1:50052 \
  --image ghcr.io/rigelbuild/compass-agent:latest
```

Remove the isolated client config home and pinned smoke state:

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
      the first-run chooser (§Part (b), 3)
- [ ] open **Window → New Window** while both windows show the chooser; complete
      the invalid-URL checks and connect in the first window, then confirm the
      second window leaves the chooser and reaches the board (§Part (b), 3)
- [ ] before the successful connection, `http://x` and `https://h/p` each show a
      validation message and leave `app.toml` absent (§Part (b), 3)
- [ ] a valid connection writes client `app.toml` with the normalized origin and
      copied `server-ca-*.pem`; the bearer is not written (§Part (b), 3)
- [ ] one agent session reaches a running container (§Part (b), 4)
- [ ] quit and relaunch auto-connects from tokenstore, with no connect screen or
      bearer re-entry (§Part (b), 5)
- [ ] after clearing the origin token and relaunching, the configured-client form
      shows the server URL as text and one token input; pasting the token connects
      (§Part (b), 5)
- [ ] clear the normalized-origin token; on a keyring backend, check both the
      normalized and trailing-slash URL entries with `secret-tool lookup`; on the
      file fallback, confirm its token file is absent (§Part (b), 6)
- [ ] the isolated config home and pinned smoke state are removed (§Part (b), 6)
