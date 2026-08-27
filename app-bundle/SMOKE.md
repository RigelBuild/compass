# Compass client-app dev-box smoke (T-4)

Acceptance for the packaged client tarball: the packaged `compass-app` — resolved
off the unpacked bundle `bin/` (the `/nix/store` symlink the tarball ships),
never a `go build` output and never the devenv PATH — launches, connects to a
**live headless stack** over its TLS network door, renders the board, drives one
agent session to a running container the stack owns, and reconnects from the OS
keychain on relaunch with no bearer re-entry.

The client bundle ships only the shell (`compass-app` + `dist/` + desktop file +
LICENSE, DL-238). It spawns, supervises, and tears down **no** stack: the stack
is a separate headless deployment the app dials. So this runbook has no embedded
bring-up — the client never spawns the stack. The stack is stood up on its own by
`compass-stack` (step 1), and `compass-app` only dials it.

Run it on **one dev box** that plays both roles: `compass-stack` brings the
headless stack (its private postgres + a podman-run agent container) up locally,
and `compass-app` dials it over the **loopback** TLS door (`https://127.0.0.1`).
That exercises the whole client path — connect screen → TLS probe → keychain →
board → one agent session to a real container — end to end.

> **Why loopback, not a second machine.** `compass-stack` self-generates its TLS
> anchor with **loopback-only SANs** (`127.0.0.1`, `::1`, `localhost` —
> `go/internal/stack/adapters/cert.go:25`) and accepts no external cert, so a
> client on *another* host cannot verify the door's cert. A true cross-machine
> smoke would need `compass-stack` to serve a cert with the remote host in its
> SANs (via `compass-gen-cert`, which exists but is not yet wired into
> `compass-stack`) — that gap is the headless-stack-distribution open question
> (OQ-3). The loopback bring-up drives the identical `compass-app` code path; the
> only untested delta is cross-host networking, which is not client-app logic.

## What the automated gates cover

- **`dogfood-e2e`** stands up a real headless stack (`compass-stack` /
  `compass-postgres` / a podman-run agent container) and drives it end to end on
  every PR. That is the stack-side regression check; a green PR has already
  proven the headless bring-up.
- The **multi-window gtk4 e2e** step compiles and runs `compass-app` under a
  virtual framebuffer and exercises the shell/windowing surface.

Neither drives the **packaged tarball's** GUI webview against a live stack and a
real agent container — a rendered webview over a TLS door plus a podman container
is the leg a bare CI runner cannot reach, so it stays manual.

## What stays manual (this runbook)

Run this on a Linux dev box with the build's `/nix/store` realized (the build
box, or one sharing its store — the bundle is a non-relocatable dev-box artifact,
its `bin` entry being a store symlink), and with **rootless podman** available
(the stack runs the agent container).

### 1. Bring up a headless stack to connect to

The stack runs the agent container over rootless podman; pre-pull the image so
bring-up does not cold-pull:

```bash
podman info --format '{{.Host.Security.Rootless}}'   # → true
# The bundle does NOT ship the agent image (DL-112); compass-stack pulls
# ghcr.io/rigelbuild/compass-agent:latest from GHCR at first run. Pre-pull to
# avoid a cold-pull timeout on bring-up.
podman pull ghcr.io/rigelbuild/compass-agent:latest
```

Stand up the stack with its **TLS network door** on the loopback port (the client
dials `https://`, never cleartext — `go/internal/appconfig/appconfig.go:132-135`).
`compass-stack` self-manages the door's cert — it generates a loopback keypair
under `--state-dir` (`tls.crt`/`tls.key`,
`go/internal/stack/adapters/cert.go:53-55`) and threads it into the
`compass-server` it spawns (`go/internal/stack/spec.go:25-33`), so you pass **no**
cert flags:

```bash
STATE=$(mktemp -d); RT=$(mktemp -d)
compass-stack up \
  --state-dir "$STATE" --socket "$RT/server.sock" \
  --listen 127.0.0.1:50052 --linger \
  --image ghcr.io/rigelbuild/compass-agent:latest
# returns once the stack is Ready (private postgres + compass-server +
# compass-runner up); the children linger.
```

The spawned `compass-server` writes a **bootstrap-admin token** (0600) at startup
(`go/cmd/compass-server/main.go:277-283`). `compass-stack` does not pass the
server a `--state-dir`, so the server defaults it to the socket's parent
(`main.go:279`) — i.e. `$RT` here — and the token lands at `$RT/admin-token`. Read
it out as the bearer the client will paste:

```bash
cat "$RT/admin-token"      # the bearer for step 3's connect screen
```

The client also needs the door's cert as its `ca_cert` trust anchor: it is the
`compass-stack`-generated `$STATE/tls.crt` (step 2). The loopback `server_url` is
`https://127.0.0.1:50052`.

### 2. Build + unpack the client bundle

```bash
moon run compass-app-bundle:build            # → app-bundle/compass-app-<version>-linux-amd64.tar.gz
PREFIX=$(mktemp -d)
tar -xzf app-bundle/compass-app-<version>-linux-amd64.tar.gz -C "$PREFIX"
BUNDLE="$PREFIX/compass-app-<version>-linux-amd64"
```

`<version>` is `0.1.0+g<short-sha>`.

Point the client's `app.toml` at the stack: `mode = "client"`, `server_url =
"https://127.0.0.1:50052"` (a validated https URL), and `ca_cert =
"$STATE/tls.crt"` (the stack's self-signed door cert)
(`compass-native-client-mode/design.md:67-71`). The bearer goes in the connect
screen, **never** in `app.toml` (DL-109).

### 3. Launch, connect, and render the board

A virtual framebuffer is required on a headless box:

```bash
# realize xvfb the same way CI's gtk4 e2e gate does (path is repo-root-relative)
BINENV=$(nix build --no-link --print-out-paths \
  -f tools/toolchain/gtk-e2e-env.nix bin)
PATH="$BINENV/bin:$BUNDLE/bin:$PATH" \
  xvfb-run -a "$BUNDLE/bin/compass-app"
```

With no stored token, the app paints the **connect screen**: the `server_url`
shown read-only (it comes from `app.toml`, not user entry) and one input for the
**bearer** (the step-1 `admin-token`). Paste it and connect. The Go shell probes
`GetServerInfo` → `apiVersion == "compass.v1"` → `WhoAmI`, writes the token to
the OS keychain, arms the bearer injector, and boots into the board
(`compass-native-client-mode/design.md:185-226`). The board renders live over
the TLS door — "connected as `<account>`".

### 4. Drive one agent session to a running container

From the board, start one agent session. Confirm the session reaches a
**running container** — the container comes up under the stack's podman runtime
(the client only renders). This exercises the full client → TLS door → server →
runner → agent-container path against the live stack.

### 5. Quit + relaunch reconnects from the keychain

Quit the app, then relaunch it exactly as in step 3:

```bash
PATH="$BINENV/bin:$BUNDLE/bin:$PATH" \
  xvfb-run -a "$BUNDLE/bin/compass-app"
```

Auto-connect reads the stored bearer from the OS keychain, the probe succeeds,
and boot proceeds **straight to the board** — no connect screen, no bearer
re-entry (`compass-native-client-mode/design.md:185-186`, the restart-survives
gate requirement). The keychain entry is keyed by `service "compass-app"` + the
server URL, so it binds to this exact stack.

### 6. Cleanup

```bash
rm -rf "$PREFIX"
compass-stack down \
  --state-dir "$STATE" --socket "$RT/server.sock" \
  --listen 127.0.0.1:50052 \
  --image ghcr.io/rigelbuild/compass-agent:latest
# stops the compass-server + private postgres + agent container.
rm -rf "$STATE" "$RT"
```

## Manual checklist

What a human confirms on the dev box (the headless bring-up itself is the
`dogfood-e2e` gate's job — not on this list):

- [ ] client `app.toml` is client-only (`mode = "client"`, https `server_url`,
      `ca_cert` = the stack's `tls.crt`); no token in it (§2)
- [ ] `compass-app` launches and paints the connect screen with the server URL
      read-only and one bearer input (§3)
- [ ] pasting the stack's bearer connects and the board renders live over the
      TLS door — "connected as `<account>`" (§3)
- [ ] one agent session driven from the board reaches a running container under
      the stack's podman runtime (§4)
- [ ] quit + relaunch auto-connects from the OS keychain — straight to the
      board, no connect screen, no bearer re-entry (§5)
