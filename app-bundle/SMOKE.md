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

The resolved `app.toml` path is `${XDG_CONFIG_HOME:-$HOME/.config}/compass/app.toml`. Do not create that file
for this part, and remove one left by an earlier client smoke. An absent file
resolves to embedded mode, the zero-config default
(`Load` in `go/internal/appconfig/appconfig.go`: "zero-config default"), so a leftover client config
silently makes this part launch in the wrong mode:

```bash
APP_CONFIG="${XDG_CONFIG_HOME:-$HOME/.config}/compass/app.toml"
# no app.toml: this part is the zero-config path
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

Pin the stack's state directory and socket for the smoke. Left unset they
default under `$HOME/.compass` (`resolveStateDir` in
`go/cmd/compass-app/client.go`), which mixes smoke state into the real
dev-box install and leaves nothing safe to delete afterwards:

```bash
ESTATE=$(mktemp -d); ERT=$(mktemp -d)
BINENV=$(nix build --no-link --print-out-paths \
  -f tools/toolchain/gtk-e2e-env.nix bin)
PATH="$BINENV/bin:$BUNDLE/bin:$PATH" \
  xvfb-run -a "$BUNDLE/bin/compass-app" \
    --state-dir "$ESTATE" --socket "$ERT/server.sock" \
    2>"$ERT/app.log"
```

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
podman ps -a --filter name='^compass-(postgres|otel-collector|nats|agent)-'
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
rm -rf "$PREFIX" "$ESTATE" "$ERT"
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
CSTATE=$(mktemp -d); CRT=$(mktemp -d)
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

The resolved client `app.toml` path is `${XDG_CONFIG_HOME:-$HOME/.config}/compass/app.toml`. Create that file
with `mode = "client"`, the HTTPS `server_url`, and `ca_cert` set to
`$CSTATE/tls.crt` (`Parse` in `go/internal/appconfig/appconfig.go`: "mode" and "ca_cert").
Put the bearer in the connect screen, never in `app.toml` (DL-109).

```bash
APP_CONFIG="${XDG_CONFIG_HOME:-$HOME/.config}/compass/app.toml"
mkdir -p "$(dirname "$APP_CONFIG")"
cat >"$APP_CONFIG" <<EOF
mode = "client"
server_url = "https://127.0.0.1:50052"
ca_cert = "$CSTATE/tls.crt"
EOF
```

### 3. Launch, connect, and render the board

```bash
BINENV=$(nix build --no-link --print-out-paths \
  -f tools/toolchain/gtk-e2e-env.nix bin)
PATH="$BINENV/bin:$BUNDLE/bin:$PATH" \
  xvfb-run -a "$BUNDLE/bin/compass-app" \
    --state-dir "$CSTATE" --socket "$CRT/server.sock" 2>"$CRT/app.log"
```

With no stored token, the app paints the connect screen. The server URL is
read-only and comes from `app.toml`; the bearer is the `$CRT/admin-token` value.
Paste it and connect. The shell probes `GetServerInfo`, calls `WhoAmI`, writes
the token to the OS keychain, arms the bearer injector, and boots into the
board (`bridgeService.Connect` in `go/cmd/compass-app/bridge_service.go`: "tokenstore"). Confirm
the board renders live over the TLS door.

### 4. Drive one agent session to a running container

From the board, start one agent session. Confirm the session reaches a running
container under the stack's podman runtime.

### 5. Quit and relaunch from the keychain

Quit the app, then relaunch it:

```bash
PATH="$BINENV/bin:$BUNDLE/bin:$PATH" \
  xvfb-run -a "$BUNDLE/bin/compass-app" \
    --state-dir "$CSTATE" --socket "$CRT/server.sock" \
    2>>"$CRT/app.log"
```

Auto-connect reads the stored bearer from the OS keychain and boots straight to
the board with no connect screen or bearer re-entry
(`bridgeService.Connect` in `go/cmd/compass-app/bridge_service.go`:
"use the stored one"). The keychain entry is keyed
by service `compass-app` and the server URL.

### 6. Cleanup

```bash
compass-stack down \
  --state-dir "$CSTATE" --socket "$CRT/server.sock" \
  --listen 127.0.0.1:50052 \
  --image ghcr.io/rigelbuild/compass-agent:latest
```

The bearer outlives all of that. Step 3 stored it under the OS keychain service
`compass-app`, keyed by the server URL, or in a 0600 `remote-token` file under
the state dir when no keychain backend is available (`tokenFileName` and `New` in
`go/internal/tokenstore/tokenstore.go`: "fallback file under the caller-supplied
state dir"). The packaged app has no logout action, so use the bundled
`compass-clear-token` helper. Its `run` function in
`go/cmd/compass-clear-token/main.go` reads the stored entry only to confirm the
URL matches, discarding the token, then passes the URL to `Store.Delete`. The
credential is never printed. That read is what makes the helper URL-scoped: a
matching URL deletes its token; a mismatched URL or an absent token leaves the
stored file intact. Use the exact URL from `app.toml`:

```bash
"$BUNDLE/bin/compass-clear-token" \
  --server-url "https://127.0.0.1:50052" \
  --state-dir "$CSTATE"
```

The helper is silent on success, and exits 0 whether it deleted a token, found a
mismatched URL, or found nothing at all. So confirm the outcome yourself rather
than reading exit 0 as proof. On the file-fallback path the entry is a file:

```bash
test ! -e "$CSTATE/remote-token" && echo "file-backend token cleared"
```

Which backend is bound depends on whether a Secret Service is reachable. A
headless smoke box usually has none, so the file check above is the one that
applies. If a keychain is bound instead, the entry lives under service
`compass-app` keyed by the server URL, outside the state directory, so the
`rm -rf` below cannot clear it. Probe it by exact key, and discard the output:

```bash
secret-tool lookup service compass-app username "https://127.0.0.1:50052" \
  >/dev/null && echo "keychain entry STILL PRESENT" || echo "keychain entry cleared"
```

Use `lookup`, never `secret-tool search`: `search` loads and prints the secret
itself, which would dump a still-live bearer into the terminal on exactly the
path this check exists to catch. `lookup` needs the exact key, so it also
confirms the URL scoping. A typo in `--server-url` is a silent no-op, and that
is what this check catches.

After this check, remove the client configuration and the pinned smoke state:

```bash
rm -f "$APP_CONFIG"
rm -rf "$PREFIX" "$CSTATE" "$CRT"
```

## Manual checklist

### Embedded mode

- [ ] no `app.toml` is present, so launch selects embedded mode (§Part (a), 3)
- [ ] rootless podman and podman 4.3 or newer are available, and the agent image
      is pulled so bring-up does not cold-pull (§Part (a), 1)
- [ ] the bundle contains the shell and four sidecars (§Part (a), 2)
- [ ] **Quit and stop stack** (not plain close) closes the app; `podman ps -a
      --filter name='^compass-(postgres|otel-collector|nats|agent)-'` is empty
      and `$ERT/app.log` reports no teardown failure (§Part (a), 5)
- [ ] the pinned `--state-dir`/`--socket` paths are removed and `$HOME/.compass`
      was not touched (§Part (a), 5)

### Client mode

- [ ] the resolved client `app.toml` has `mode = "client"`, an HTTPS
      `server_url`, and `ca_cert`; it has no bearer (§Part (b), 2)
- [ ] the app launches with a read-only server URL and one bearer input (§Part
      (b), 3)
- [ ] pasting the bearer connects and the board renders over the TLS door (§Part
      (b), 3)
- [ ] one agent session reaches a running container (§Part (b), 4)
- [ ] quit and relaunch auto-connects from the OS keychain, with no connect
      screen or bearer re-entry (§Part (b), 5)
- [ ] the stored bearer for the matching server URL is cleared, confirmed by an
      observation and not by the helper's exit code: `remote-token` is absent on
      the file-fallback path, or the out-of-band keychain lookup finds no
      `compass-app` entry for that URL; a mismatched or absent URL leaves the
      stored file intact, and the client `app.toml` is removed (§Part (b), 6)
