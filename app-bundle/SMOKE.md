# Compass native-app dev-box smoke (T6.5)

Acceptance for the packaged release tarball: the packaged binaries — resolved off
the unpacked bundle `bin/` (the `/nix/store` symlinks the tarball ships), never a
`go build` output and never the devenv PATH — bring the embedded stack up and
serve the board.

Most of that smoke is now an **automated gate**; this runbook covers only the
part a machine on the CI runner cannot reach.

## What the automated gate covers (`compass-app-bundle:smoke`)

`moon run compass-app-bundle:smoke` builds this commit's tarball and drives the
**packaged** `compass-postgres` + `compass-server` to `GetServerInfo`-ready —
`stack.spawnChain` steps 1-4 (private postgres up + reachable → `compass-server`
→ readiness probe), the T4 embedded smoke **minus the two podman legs** (agent
image present + `compass-runner` enroll). It runs in the `gates` CI job on every
PR that touches a packaging input and unconditionally on `main` + nightly (the
bundle's affected/full split, `app-bundle/moon.yml`), so a packaging regression
reds CI with no human in the loop. The gate itself is `go/bundlesmoke` (a
`bundlesmoke`-tagged test reusing the real stack adapters).

It defends the exact defect the first T6.5 smoke found: `compass-go:test` and
`dogfood-e2e` bring the stack up from devenv-PATH binaries, where postgres
`share/` and `lib/` sit beside `bin/` in one nix store — so a bundle that staged
only `bin/` still passes there. The gate drives the **packaged** tree (`share` +
`lib` as separate store symlinks) from the tarball, so a staging gap fails there
and only there.

**So the packaged bring-up to server-ready is no longer a manual step** — do not
re-verify it by hand. The gate is the regression check; a green PR has already
proven it.

## What stays manual (this runbook)

Two legs the automated gate cannot reach on the bare CI runner, both requiring a
real dev box:

1. **The `compass-runner` leg** — needs rootless podman + the agent image pulled
   from GHCR. On this repo's CI, podman lives only in the privileged
   `dogfood-e2e` job, not the `gates` runner the bundle rides; wiring the bundle
   there would be a `ci.yml` edit the frozen packaging design forbids (A4).
2. **The GTK board** — the `bin/compass-app` shell rendering over the socket, a
   human eyeball check.

Run this on a Linux dev box with the build's `/nix/store` realized (the build
box, or one sharing its store — the bundle is a non-relocatable dev-box artifact,
its `bin`/`lib`/`share` entries being store symlinks).

### 1. Build + unpack

```bash
moon run compass-app-bundle:build            # → app-bundle/compass-app-<version>-linux-amd64.tar.gz
PREFIX=$(mktemp -d)
tar -xzf app-bundle/compass-app-<version>-linux-amd64.tar.gz -C "$PREFIX"
BUNDLE="$PREFIX/compass-app-<version>-linux-amd64"
```

`<version>` is `0.1.0+g<short-sha>`.

### 2. Preflight the podman leg

```bash
podman info --format '{{.Host.Security.Rootless}}'   # → true
# The bundle does NOT ship the agent image (DL-112); compass-stack pulls
# ghcr.io/rigelbuild/compass-agent:latest from GHCR at first run. Pre-pull (or
# tag a local build as the ref) to avoid a cold-pull timeout on bring-up.
podman pull ghcr.io/rigelbuild/compass-agent:latest
```

### 3. Full embedded bring-up (with the runner) + the board

Fresh state + a free loopback port (the default 50052 may be contended on a
shared box; pass an explicit port):

```bash
STATE=$(mktemp -d); RT=$(mktemp -d)
PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])')
PATH="$BUNDLE/bin:$PATH" "$BUNDLE/bin/compass-stack" up \
  --state-dir "$STATE" --socket "$RT/server.sock" \
  --listen "127.0.0.1:$PORT" --linger \
  --image ghcr.io/rigelbuild/compass-agent:latest
# → "ready: server version <version>"
```

Reaching `ready` here — unlike the automated gate, which stops at server-ready —
transitively verifies the **runner** leg too: `compass-runner` boots on the
pulled agent image (`GetServerInfo` answering after a full `up` requires it —
`compass-native-app/design.md:189-191`).

Now drive the **GTK shell** (`bin/compass-app`) against the **same** stack, so it
attaches rather than spawning a second one. Embedded `compass-app` always
self-supervises a stack (`compass-app/embedded.go` `runEmbedded` →
`compass-stack up`), but `up` **attaches** when a live server already answers the
resolved socket (`internal/stack/stack.go` `upLocked`). So pass the step-3 socket
explicitly with `-socket "$RT/server.sock"` — omit it and `compass-app` resolves
the default socket (`$XDG_RUNTIME_DIR/compass/server.sock`), misses the running
stack, and tries to spawn a second postgres on the same `$STATE` datadir (which
fails on the held `postmaster.pid`). A virtual framebuffer is required on a
headless box:

```bash
# realize xvfb the same way CI's gtk3 e2e gate does (path is repo-root-relative)
BINENV=$(nix build --no-link --print-out-paths \
  -f tools/toolchain/gtk-e2e-env.nix bin)
PATH="$BINENV/bin:$BUNDLE/bin:$PATH" XDG_RUNTIME_DIR="$RT" \
  xvfb-run -a "$BUNDLE/bin/compass-app" \
  -mode embedded -state-dir "$STATE" -socket "$RT/server.sock" \
  -image ghcr.io/rigelbuild/compass-agent:latest
```

(Attaching binds no new listener — the board renders over the step-3 stack's
socket and `$PORT`, so the fixed embedded door `127.0.0.1:50052` is never bound
and needs no freeing. Omitting `-socket` is what would trigger a spawn on 50052.)

### 4. Quit + cleanup

```bash
PATH="$BUNDLE/bin:$PATH" "$BUNDLE/bin/compass-stack" down \
  --state-dir "$STATE" --socket "$RT/server.sock" \
  --listen "127.0.0.1:$PORT" \
  --image ghcr.io/rigelbuild/compass-agent:latest
# → "stack down"; the port frees and the postgres child stops.
rm -rf "$PREFIX" "$STATE" "$RT"
```

## Manual checklist

Packaged bring-up to server-ready + the postgres staging are the automated gate's
job — not on this list. What a human still confirms on a dev box:

- [ ] rootless podman + agent image reachable (§2)
- [ ] full `compass-stack up` reaches `ready` **with the runner** — i.e.
      `compass-runner` booted on the pulled agent image (§3)
- [ ] GTK shell renders the board over the socket (under xvfb on a headless box)
- [ ] to exercise a live container end-to-end, drive one agent session from the
      GTK shell and observe the container come up
- [ ] `compass-stack down` stops the stack and frees the port (§4)
