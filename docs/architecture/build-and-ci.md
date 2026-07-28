# Build & CI

How a change goes from an editor to a verified build. Each layer is configured
in a real format and composes cleanly:

```text
proto       Go/bun/node/moon version pins
devenv      the dev shell
moon        the task graph: what to build/test, caching, affected detection
```

The headline property: **the same task graph runs remotely and locally.** Only
the scheduler differs — GitHub Actions remotely, moon on one box locally. What
keeps that true rather than aspirational is the version-parity gate below,
which fails the build when the two toolchains diverge.

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
- **CI:** the runtimes come from the native `setup-*` actions reading
  `.prototools`; the devenv-provided tools are resolved from the same pinned
  nixpkgs revision. See CI below.

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

`.github/workflows/ci.yml` runs on every pull request and every push to `main`.
Two jobs:

- **`gate`** — the whole battery, as one `moon run :ci`. Nothing about the
  workspace is enumerated in the workflow, so a new project (or a newly
  vendored fork) is gated the moment it is registered in
  `.moon/workspace.yml`. This is the same command, over the same task graph,
  that the local gate and the `hk` pre-push hook run.
- **`pgtest`** — the real-Postgres suites, which are build-tagged `pgtest` and
  therefore never compiled by the `gate` job's `go test ./...`. It runs them
  against an Actions Postgres service container, and asserts afterwards that
  they actually ran: the harness *skips* when it finds no database, so a
  service that never came up would otherwise pass silently. It is a separate
  job so a Postgres outage cannot red the hermetic gate.

### Where CI's toolchain comes from

The split mirrors the local one, with one seam that only exists remotely.

The language runtimes come from the native `setup-*` actions, which read their
versions **out of `.prototools`** at run time rather than repeating them in
YAML. The nixpkgs-provided tools — buf, protoc, the Go analysis battery, biome,
markdownlint — have no `setup-*` equivalent that could reproduce a nixpkgs pin,
so CI resolves the identical derivations the dev shell does, from the nixpkgs
revision `devenv.lock` pins (`tools/toolchain/gate-tools.nix`), and puts them on
PATH. Nix is the mechanism for that one step; it is not the toolchain strategy.

### The parity gate

Reading a version from a file does not prove the runner got it. A step before
the gate (`tools/toolchain/parity.ts`, also scheduled by `:ci` as
`toolchain-parity:parity`) asserts that the toolchain actually on PATH is the
one the dev shell defines, and **fails the build** — never warns — when it is
not. The two halves of the toolchain are checked two ways, because they are
pinned two ways:

- **`.prototools` runtimes** (bun, node, moon, go) carry a literal version, so
  the check is that the binary reports exactly it.
- **`devenv.nix` packages** carry no version literal anywhere in the repo —
  their version is whatever the pinned nixpkgs resolves — and two of them
  (`go-licenses`, `nilaway`) implement no version flag at all. So the check is
  that `realpath` of each command lands inside the derivation that nixpkgs
  revision builds. That is stricter than comparing version strings: it catches
  an ambient binary of the same version shadowing the pinned one.

A tool the gate cannot check is reported `UNVERIFIABLE` and fails the build.
Skipping what it cannot verify would make its green mean nothing.

### What CI does not gate

- **Upstream test suites inside the vendored forks.** Each fork's registered
  task is its own `nix build`; the upstream suites it vendors are not run.
- **A live UI↔server path.** Every `compass-ui` task runs against fixtures, so
  no check exercises the UI against a running server.

## Caching

**moon task cache** — whole-task-output caching, keyed by an inputs hash.
