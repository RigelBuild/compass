# Dogfood door provisioning runbook

Bring a Compass **server + runner** pair up on a single host, over the
authenticated TLS network door, from the binaries this repository already
ships. No new tooling: cert generation, runner-token minting, and runner trust
are all existing `go/cmd/` binaries.

This is the loopback bring-up. Serving it onto a tailnet (a `tailscale serve`
front end) and baking the UI bundle are separate steps that consume the
`https://127.0.0.1:8443` door this runbook produces.

## Prerequisites

All of these before the first command — each is fail-fast, so a missing one
stops the bring-up at a named step rather than at the first RPC.

- **A Go toolchain on the serving host.** The four binaries below exist only as
  `main` packages under `go/cmd/`; nothing in the tree installs them, and the
  only build the repo carries (devenv's dev process) compiles `compass-server`
  alone. On a fresh host the runbook's first real command is `command not
  found` until you build them — which Shell 1 does. The pinned version is
  `go = 1.26.5` (`.prototools`).
- **A reachable Postgres, and a DSN in the environment.** Both `compass-server`
  and `compass-mint-runner-token` refuse to start without one, with the same
  message — `a Postgres DSN is required: pass --database or set
  $COMPASS_DATABASE_DSN` (`go/cmd/compass-server/main.go:160-163`;
  `resolveDSN`, `go/cmd/compass-mint-runner-token/main.go:103-112`). The DSN is
  not a formality: `store.Open` builds a pgx pool, forces a real connection
  with `pool.Ping`, and applies the embedded migrations under an advisory lock
  before the caller may serve (`go/internal/store/store.go:76-94`), so an
  unreachable database fails the bring-up here rather than later. This
  repository provisions no Postgres on a standalone host — a reachable
  Postgres with a `compass` database is an explicit operator prerequisite.
- **The mint CLI and the server MUST resolve the SAME store.** The runner
  token's SHA-256 hash is *written* by the mint path (`MintRunnerToken` →
  `StoreRunnerTokenHash`, `go/internal/runnerhub/mint.go:103-115`) and
  *resolved* by the server's runner door via `auth.ResolveToken` →
  `st.ResolveTokenHash` (`go/internal/auth/token.go:102-116`); against two
  different databases the runner's token is simply not found and enrollment
  fails `Unauthenticated`. Exporting one `$COMPASS_DATABASE_DSN` for both
  binaries is what guarantees this — `resolveDSN` "mirrors compass-server's
  precedence exactly (flag wins, else `$COMPASS_DATABASE_DSN`, else error)"
  (`go/cmd/compass-mint-runner-token/main.go:100-102`).

## Bring-up

Two shells: the server and the runner are both long-lived and hold the
foreground, so each gets its own. Shell 2 is a **new** shell and inherits
nothing from Shell 1 — re-export the same three literals before anything else.

The state dir is created and its mode pinned up front. `$STATE` holds the
admin-token file (0600, `go/server/network_door.go`), the socket (bound under
`umask 0177`, `go/server/socket.go`), and the runner token (0600), so nothing
inside it is world-readable — but the directory itself, left at the operator's
umask, is world-*traversable* (0755 under the default `umask 0022`). Pin it to
0700 so a private-by-name state dir is actually private. Use `mkdir -p` then
`chmod`, not `mkdir -m 700`: `-m` sets the mode on the leaf only (a missing
`~/.compass` ancestor lands 0755) and is a no-op on an existing dir, whereas
`chmod` is idempotent and pins the mode on every run.

```bash
# Shell 1 — prerequisites, then the server (which stays in the foreground).
export COMPASS_DATABASE_DSN=postgres://…/compass   # see the DSN prerequisite
export STATE="$HOME/.compass/dogfood"

mkdir -p "$STATE" && chmod 700 "$STATE"

# Build all four binaries — nothing in the tree installs them (see above).
cd <repo>/go && go build -o "$STATE/bin/" \
  ./cmd/compass-gen-cert ./cmd/compass-server \
  ./cmd/compass-mint-runner-token ./cmd/compass-runner
export PATH="$STATE/bin:$PATH"

# One self-signed cert, its own CA: it is the server's --tls-cert AND the
# runner's --ca trust anchor. SANs default to 127.0.0.1,::1,localhost, and
# generation is skip-if-present so a restart never swaps the cert out from
# under a live server/runner pair (go/cmd/compass-gen-cert/main.go).
compass-gen-cert --cert-out "$STATE/tls.crt" --key-out "$STATE/tls.key"

# Long-lived: run() installs a SIGINT/SIGTERM signal context and server.Serve
# blocks on it, returning only on a termination signal. Leave this running — a
# literal copy-paste of the whole runbook into one shell never reaches the
# steps below. --socket is passed explicitly so the socket path is a known
# literal Shell 2 can reuse verbatim.
compass-server --listen 127.0.0.1:8443 \
  --tls-cert "$STATE/tls.crt" --tls-key "$STATE/tls.key" --state-dir "$STATE" \
  --socket "$STATE/server.sock"
```

```bash
# Shell 2 — a NEW shell, so it inherits nothing from Shell 1. Re-export the
# three values verbatim (same literals as above) before anything else.
export COMPASS_DATABASE_DSN=postgres://…/compass
export STATE="$HOME/.compass/dogfood"
export PATH="$STATE/bin:$PATH"

# Mint the runner's enrollment token (SubjectRunner keyspace), written 0600
# and idempotent against the store — not just against file presence. The admin
# account token does NOT work on the runner door.
compass-mint-runner-token --runner-id <id> --token-out "$STATE/runner.token"

# The UI needs no account id from this bring-up. It learns the caller's own
# account via the server's `WhoAmI` RPC right after the transport is up
# (`go/server/service.go:394`, server-derived and not admin-gated; the UI's
# `resolveCaller`, `apps/ui/src/live/client.ts:76`), so nothing bakes a caller
# id into the bundle and there is no `--admin-handle`-driven rebake. The only
# server-side value the UI build consumes is the admin bearer for
# `VITE_COMPASS_TOKEN` — the contents of the admin-token file `--listen` wrote
# (`$STATE/admin-token`, 0600), the same file the Verify step reads below.

# Also long-lived: the runner holds its Sessions bidi stream open for its life.
# The token is env only, never a flag (a flag leaks into the process table).
# --ca replaces the system root pool with exactly the generated cert.
COMPASS_RUNNER_TOKEN=$(cat "$STATE/runner.token") \
  compass-runner --runner-id <id> --server https://127.0.0.1:8443 \
  --ca "$STATE/tls.crt" --image <agent-image>
```

`compass-runner` fail-fasts without `--runner-id`/`$COMPASS_RUNNER_ID`,
`--server`/`$COMPASS_SERVER_ADDR`, `$COMPASS_RUNNER_TOKEN`, and
`--image`/`$COMPASS_AGENT_IMAGE` (`go/cmd/compass-runner/main.go:101-118`) — so
a missing one names itself rather than half-starting.

## Verify

From Shell 2, with the server still running. Do **not** use `grpcurl`: the
module registers no reflection service, so a grpcurl probe fails at descriptor
resolution before it reaches the auth interceptor — an unfalsifiable check.
Use `curl` against the connect handlers.

```bash
# 1. admin bearer, trusting the generated cert -> a GetServerInfo payload.
curl --cacert "$STATE/tls.crt" -X POST \
  -H 'content-type: application/json' \
  -H "authorization: Bearer $(cat "$STATE/admin-token")" \
  -d '{}' https://127.0.0.1:8443/compass.v1.CompassService/GetServerInfo

# 2. same call, no authorization header -> connect code `unauthenticated`.
curl --cacert "$STATE/tls.crt" -X POST \
  -H 'content-type: application/json' \
  -d '{}' https://127.0.0.1:8443/compass.v1.CompassService/GetServerInfo
```

Plus the cross-door assertion: the `SubjectRunner` token enrolls over the
runner door (the `compass-runner` launch above succeeds and holds its stream)
while the admin account token is rejected there (`Unauthenticated`).

## Token lifetime (accepted risk)

`--listen` mints a fresh admin token on every start (`issueAndWriteAdminToken`
→ `auth.IssueAccountToken`, `go/internal/auth/token.go:41-65`, which INSERTs a
new hash and revokes nothing; `ResolveToken` accepts any persisted hash). Two
consequences:

- A baked UI bundle's bearer stays valid **indefinitely** across restarts — a
  restart forces no rebake, and a rebake does not rotate the old bearer out.
- Every `--listen` restart accumulates an additional never-revoked live admin
  token in the store. Rotating a leaked or superseded bundle's bearer needs an
  explicit revoke; the store primitive exists and is tested
  (`store.RevokeToken`, `go/internal/store/tokens.go:55-82`) but no operator
  surface wires it yet (tracked as SEA-1490). That surface must land before the
  bundle is served to anyone but its single operator.
