# Compass packaged-app smoke

This runbook validates the packaged `compass-app` in embedded and client modes.
Build the tarball with `moon run compass-app-bundle:build`, then unpack the
result. The bundle is a non-relocatable dev-box artifact. Its `bin` entry is a
`/nix/store` symlink. The bundle ships the `compass-app` shell and three
sidecars: `compass-stack`, `compass-server`, and `compass-runner`
(`app-bundle/build.sh:88-97`). It does not ship postgres tooling; embedded mode
runs stock postgres:18 in a rootless podman container (`app-bundle/build.sh:6-7`).

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
(`go/cmd/compass-app/embedded.go:111-114`).

### 1. Check embedded prerequisites

Embedded mode requires Linux or macOS, rootless podman, and podman 4.3 or
newer. These are fatal host checks. The agent image is checked locally but is
pulled from GHCR by the stack when it is missing
(`go/internal/preflight/preflight.go:96-101`,
`go/internal/preflight/preflight.go:156-174`,
`go/cmd/compass-app/embedded.go:407-423`). Confirm rootless podman and the
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

`<version>` is `0.1.0+g<short-sha>`. Keep the bundle's `bin/compass-app`,
`bin/compass-stack`, `bin/compass-server`, and `bin/compass-runner` together.
The build stages all three sidecars into that directory
(`app-bundle/build.sh:88-97`).

### 3. Launch with no `app.toml`

The resolved `app.toml` path is `${XDG_CONFIG_HOME:-$HOME/.config}/compass/app.toml`. Do not create that file
for this part, and remove one left by an earlier client smoke. An absent file
resolves to embedded mode, the zero-config default
(`go/internal/appconfig/appconfig.go:170-208`), so a leftover client config
silently makes this part launch in the wrong mode:

```bash
APP_CONFIG="${XDG_CONFIG_HOME:-$HOME/.config}/compass/app.toml"
# no app.toml: this part is the zero-config path
rm -f "$APP_CONFIG"
```

The stack binary resolution order is the `--compass-stack` flag,
`COMPASS_STACK_BIN`, a `compass-stack` sibling of the running `compass-app`,
then `PATH` (`go/cmd/compass-app/embedded.go:297-323`). For this smoke, do not
pass `--compass-stack` and require `COMPASS_STACK_BIN` to be unset:

```bash
unset COMPASS_STACK_BIN
```

The app resolves `compass-stack` as a sibling of the running `compass-app`
executable, preferred over PATH (`go/cmd/compass-app/embedded.go:297-323`), and
prepends that same `bin/` directory for the supervised sidecars
(`go/cmd/compass-app/embedded.go:325-355`). So the bundle's staged
`compass-stack` wins even when an ambient one is on PATH, which is what the
launch below relies on.

Pin the stack's state directory and socket for the smoke. Left unset they
default under `$HOME/.compass` (`go/cmd/compass-app/client.go:56-67`,
`go/cmd/compass-app/main.go:359-370`), which mixes smoke state into the real
dev-box install and leaves nothing safe to delete afterwards:

```bash
ESTATE=$(mktemp -d); ERT=$(mktemp -d)
BINENV=$(nix build --no-link --print-out-paths \
  -f tools/toolchain/gtk-e2e-env.nix bin)
PATH="$BINENV/bin:$BUNDLE/bin:$PATH" \
  xvfb-run -a "$BUNDLE/bin/compass-app" \
    --state-dir "$ESTATE" --socket "$ERT/server.sock"
```

`xvfb-run` is the same virtual-framebuffer setup the client part uses, needed on
a headless box.

With those flags the app invokes this stack command:

```text
compass-stack up --state-dir <state-dir> --image ghcr.io/rigelbuild/compass-agent:latest --socket <socket>
```

`stackUpArgs` passes only `up`, `--state-dir`, `--image`, and `--socket`
(`go/cmd/compass-app/embedded.go:134-150`). It deliberately does not pass
`--database`, `--postgres-image`, `--collector-image`, or `--listen`. The image
ref is the locked GHCR default unless `--image` or `$COMPASS_AGENT_IMAGE`
overrides it (`go/cmd/compass-app/embedded.go:358-369`).

### 4. Confirm the embedded board and run one session

Wait for the app to bring the stack to Ready. It then resolves the caller with
`WhoAmI` over the local socket (`go/cmd/compass-app/embedded.go:268-273`).
Confirm that the app opens the board directly, without a client connect screen
or bearer entry. Embedded mode has no client `server_url` or `ca_cert`
configuration (`go/internal/appconfig/appconfig.go:78-85`), and its identity
comes from that local-socket call.

From the board, start one agent session. Confirm that it reaches a running
agent container under the stack's podman runtime.

### 5. Quit and stop the embedded stack

Use the explicit **Quit and stop stack** action, not a plain window close or
OS quit. Plain close exits the app but leaves the detached stack running for a
later relaunch; **Quit and stop stack** runs `compass-stack down` and then quits
the app (`go/cmd/compass-app/lifecycle.go:6-16`, `:38-74`). Do not run a manual
`compass-stack down` for this part. After the app closes, confirm that no stack containers or
private postgres container remain:

```bash
podman ps -a
```

A lingering stack here is a real failure, but the app exits either way: if
`compass-stack down` fails the app still quits and logs the error, because
trapping the user in a live window is worse and a lingering stack is the safe
failure (OQ-6, `go/cmd/compass-app/lifecycle.go:57-60`). So read the app's log
as well as `podman ps -a` before calling this step green.

Remove the unpacked bundle and the pinned stack state after confirming
teardown. Pinning them in step 3 is what makes this safe to delete: an unpinned
run writes into the real `$HOME/.compass` install instead.

```bash
rm -rf "$PREFIX" "$ESTATE" "$ERT"
```

Embedded mode stores no bearer, so there is no keychain entry to clear here
(the local socket is a filesystem-permission boundary, not a bearer door).

## Part (b): client mode

This part points the packaged app at a live headless stack. The client uses the
stack's TLS network door and does not spawn or supervise that stack.

> **Why loopback, not a second machine.** `compass-stack` self-generates its TLS
> anchor with **loopback-only SANs** (`127.0.0.1`, `::1`, `localhost` —
> `go/internal/stack/adapters/cert.go:25`) and accepts no external cert, so a
> client on *another* host cannot verify the door's cert. A true cross-machine
> smoke would need `compass-stack` to serve a cert with the remote host in its
> SANs (via `compass-gen-cert`, which exists but is not yet wired into
> `compass-stack`) — that gap is the headless-stack-distribution open question
> (OQ-3). The loopback bring-up drives the identical `compass-app` code path; the
> only untested delta is cross-host networking, which is not client-app logic.

### 1. Bring up a headless stack to connect to

The stack runs the agent container over rootless podman. Pre-pull the image so
bring-up does not cold-pull:

```bash
podman info --format '{{.Host.Security.Rootless}}'   # -> true
podman pull ghcr.io/rigelbuild/compass-agent:latest
```

Stand up the stack with its TLS network door on the loopback port. The client
dials `https://`, never cleartext (`go/internal/appconfig/appconfig.go:130-168`).
`compass-stack` generates the loopback certificate under `--state-dir` and
passes it to `compass-server` (`go/internal/stack/adapters/cert.go:53-55`,
`go/internal/stack/spec.go:25-33`).

This step runs the stack as a standalone headless deployment, before the bundle
is built, so `compass-stack` here is any working build on PATH rather than the
bundle's staged sidecar. That is the point of client mode: the stack is a
separate deployment the app only dials.

```bash
STATE=$(mktemp -d); RT=$(mktemp -d)
compass-stack up \
  --state-dir "$STATE" --socket "$RT/server.sock" \
  --listen 127.0.0.1:50052 --linger \
  --image ghcr.io/rigelbuild/compass-agent:latest
```

The spawned server writes a bootstrap-admin token at `$RT/admin-token`.
`adminTokenFile` names that file (`go/server/network_door.go:35-38`); when
`--state-dir` is omitted for the network door, the socket parent is the state
directory (`go/server/network_door.go:267-276`), and the token is minted and
written there with the writer (`go/server/network_door.go:364-388`). Read it
for the connect screen:

```bash
cat "$RT/admin-token"
```

The client trust anchor is `$STATE/tls.crt`, and its server URL is
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
`$STATE/tls.crt` (`docs/designs/ui/compass-native-client-mode/design.md:66-73`).
Put the bearer in the connect screen, never in `app.toml` (DL-109).

```bash
APP_CONFIG="${XDG_CONFIG_HOME:-$HOME/.config}/compass/app.toml"
mkdir -p "$(dirname "$APP_CONFIG")"
cat >"$APP_CONFIG" <<EOF
mode = "client"
server_url = "https://127.0.0.1:50052"
ca_cert = "$STATE/tls.crt"
EOF
```

### 3. Launch, connect, and render the board

```bash
BINENV=$(nix build --no-link --print-out-paths \
  -f tools/toolchain/gtk-e2e-env.nix bin)
PATH="$BINENV/bin:$BUNDLE/bin:$PATH" \
  xvfb-run -a "$BUNDLE/bin/compass-app"
```

With no stored token, the app paints the connect screen. The server URL is
read-only and comes from `app.toml`; the bearer is the `$RT/admin-token` value.
Paste it and connect. The shell probes `GetServerInfo`, calls `WhoAmI`, writes
the token to the OS keychain, arms the bearer injector, and boots into the
board (`docs/designs/ui/compass-native-client-mode/design.md:185-226`). Confirm the board
renders live over the TLS door.

### 4. Drive one agent session to a running container

From the board, start one agent session. Confirm the session reaches a running
container under the stack's podman runtime.

### 5. Quit and relaunch from the keychain

Quit the app, then relaunch it:

```bash
PATH="$BINENV/bin:$BUNDLE/bin:$PATH" \
  xvfb-run -a "$BUNDLE/bin/compass-app"
```

Auto-connect reads the stored bearer from the OS keychain and boots straight to
the board with no connect screen or bearer re-entry
(`docs/designs/ui/compass-native-client-mode/design.md:185-186`). The keychain entry is keyed
by service `compass-app` and the server URL.

### 6. Cleanup

```bash
rm -rf "$PREFIX"
compass-stack down \
  --state-dir "$STATE" --socket "$RT/server.sock" \
  --listen 127.0.0.1:50052 \
  --image ghcr.io/rigelbuild/compass-agent:latest
rm -rf "$STATE" "$RT"
```

The bearer outlives all of that. Step 3 stored it under the OS keychain service
`compass-app`, keyed by the server URL, or in a 0600 `remote-token` file under
the state dir when no keychain backend is available
(`go/internal/tokenstore/tokenstore.go:27-32`, `:34-54`). The stack it
authenticates is gone, but the credential is not. The packaged app has no
logout action, so delete the token with the supported store API from a one-off
helper; `Store.Delete` selects the bound backend and treats an absent token as
success (`go/internal/tokenstore/tokenstore.go:111-118`, `:180-184`). This
prints no token:

```bash
cleanup_test=go/internal/tokenstore/smoke_cleanup_test.go
trap 'rm -f "$cleanup_test"' EXIT
cat >"$cleanup_test" <<'EOF'
package tokenstore_test

import (
  "os"
  "testing"

  "github.com/RigelBuild/compass/go/internal/tokenstore"
)

func TestSmokeCleanup(t *testing.T) {
  if err := tokenstore.New(os.Getenv("COMPASS_SMOKE_STATE")).Delete(os.Getenv("COMPASS_SMOKE_URL")); err != nil {
    t.Fatal(err)
  }
}
EOF
COMPASS_SMOKE_STATE="$STATE" \
  COMPASS_SMOKE_URL="https://127.0.0.1:50052" \
  go -C go test ./internal/tokenstore -run '^TestSmokeCleanup$' -count=1
rm "$cleanup_test"
trap - EXIT
rm -f "${XDG_CONFIG_HOME:-$HOME/.config}/compass/app.toml"
```

The temporary test calls the supported `tokenstore.Store.Delete` API, which
selects the same keychain-or-file backend as the client and prints no token.
Run it from the repository root so the internal import is allowed. Removing
`app.toml` also matters for part (a): a leftover client config stops that part
from taking the absent-file embedded path.

## Manual checklist

### Embedded mode

- [ ] no `app.toml` is present, so launch selects embedded mode (§Part (a), 3)
- [ ] rootless podman and podman 4.3 or newer are available, and the agent image
      is pulled so bring-up does not cold-pull (§Part (a), 1)
- [ ] the bundle contains the shell and three sidecars (§Part (a), 2)
- [ ] the app brings the stack to Ready and opens the board without a connect
      screen or bearer (§Part (a), 4)
- [ ] one agent session reaches a running container (§Part (a), 4)
- [ ] **Quit and stop stack** (not plain close) closes the app, `podman ps -a`
      shows no stack or postgres container, and the app log reports no teardown
      failure (§Part (a), 5)
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
- [ ] the stored bearer is cleared from the keychain (or `remote-token`) and the
      client `app.toml` is removed (§Part (b), 6)
