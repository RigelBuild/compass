# Build & CI

How a change goes from an editor to a verified build. Each layer is configured
in a real format and composes cleanly:

```text
proto       Go/bun/node/moon version pins
devenv      the dev shell
moon        the task graph: what to build/test, caching, affected detection
```

The headline property: **the same task graph runs CI and local CI.** Only the
scheduler differs — GitHub Actions remotely, moon on one box locally.

## Toolchains: proto

[proto](https://moonrepo.dev/proto) pins the Go, bun, node, and moon toolchains
in `.prototools`; it bootstraps standalone, so contributors get those without
nix. Tool versions live in that one file.

## Dev environment: devenv

[devenv](https://devenv.sh) (nix underneath) provides everything that is not a
language runtime: the protobuf/contract tooling (buf, protoc, the Go codegen
plugins), the Go analysis tools (golangci-lint, govulncheck, go-licenses,
nilaway), and the linters. The split is strict — proto owns the runtimes,
devenv owns the rest — so PATH order never silently decides which copy of a
tool wins.

- **Local:** `direnv allow` puts the toolchain on PATH. devenv injects tools,
  not a whole shell — you keep your own prompt and dotfiles.
- **The running stack:** `devenv up` starts Postgres and `compass-server`
  together (Linux). The server is ready only once it answers a real
  `GetServerInfo` — it binds its socket before running store migrations, so a
  socket-exists check would flip ready while a dial still blocks. It serves the
  Unix socket plus a loopback gRPC-Web door on `127.0.0.1:50051` for the UI dev
  server.

### Verifying the client against a live server

The UI suite runs against `comms-fake`/`compass-fake`, so it cannot catch a
break in the client↔server contract: a renamed proto field or a tightened
server-side validation leaves every UI test green and the shipped app broken.
The only check that covers it drives `@compass/client`'s
`createCompassWebClient`/`createCommsWebClient` — the doors
`apps/ui/src/live/connection.ts` itself uses — against `devenv up`.

Subscribe, post a message, and assert **the id the caller just minted arrives
back over `SubscribeComms`**. Correlating on that id is the whole point:
`SubscribeComms` replays history before it tails, so asserting merely that *a*
`messagePosted` arrived passes identically against a stream that only ever
replays and never delivers anything live.

Keep the correlation even if the replay behaviour changes. Matching a minted id
does not depend on knowing *how* the stream might break — it discriminates
against failure modes nobody has enumerated, which is most of them. Reasoning
about which broken cases a weaker assertion would catch requires already
knowing them; the replay above was only obvious in hindsight.

What the id settles is *attribution* — is this event the one I caused? It does
not settle *completeness* (were exactly the right events emitted, and no
others) or *path* (which mechanism carried it). This check needs only
attribution, so the id is sufficient; an assertion about how many events
arrive would need a different instrument.

## Task graph: moon

[moon](https://moonrepo.dev) owns the task graph (`deps:`), result caching
(`inputs`/`outputs`), affected-target detection, and local parallel execution.
`moon run <project>:<task>` is the interface; `moon run :ci` runs every
project's `ci` task. Affected detection (`moon run :ci --affected`) decides
which projects a change actually touches.

moon runs `go` and `bun` as **system tasks** — it execs the toolchain on PATH
rather than managing its own. moon's graph/caching layer stays
toolchain-agnostic; `.prototools` remains the version source.

## The contract gate

The `compass.v1` schema is the sole, owned door between any UI and the daemon,
and it is gated three ways:

- **`buf lint`** — schema style and consistency.
- **`buf breaking`** — rejects backward-incompatible schema edits.
- **Drift** — regenerate the clients, then `git diff --exit-code`. A checked-in
  client that no longer matches the schema fails the build, so generated code
  can never silently fall out of sync.

The generated Go and TypeScript clients are checked in (not generated at build
time by consumers), so a normal `go build` / `bun install` needs no codegen
step.

## CI

**There is no CI pipeline yet** (SEA-1507). `.github/` holds only
`secret_scanning.yml`, so an open PR's checks are external bots — no build, no
test, no drift gate. GitHub reports `mergeStateStatus: CLEAN` with an empty
check list, which is *no gate*, not a green one. Until a pipeline lands, the
local `moon run :ci` battery is the only run that ever happens, so quote its
output on the PR.

The wide run is occasionally flaky: `compass-proto:drift` can fail with
`wire unmarshal: cannot parse invalid wire-format data`, `protoc-gen-es`
having received a truncated request. It has only been observed inside the
full `:ci` fan-out, never when `drift` runs alone, and the mechanism is not
yet established (SEA-1526). `--concurrency 4` has not reproduced it and runs
the battery ~14x faster than serial, so it is a reasonable default — but it
is a mitigation, not a diagnosis. **A red `:ci` is worth re-running once on
the same tree before you treat it as your change's fault.**

## Caching

**moon task cache** — whole-task-output caching, keyed by an inputs hash.
