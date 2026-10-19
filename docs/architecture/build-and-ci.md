# Build & CI

How a change goes from an editor to a verified build. Each layer is configured
in a real format and composes cleanly:

```text
devenv      the dev shell — owns every toolchain (Go/bun/node/moon + the rest)
moon        the task graph: what to build/test, caching, affected detection
```

The headline property: **the same task graph runs remotely and locally.**
GitHub Actions schedules remote runs; moon schedules local ones. The
version-parity gate below fails builds when toolchains diverge.

## Toolchains: devenv/nix

[devenv](https://devenv.sh) (nix underneath) owns the entire toolchain — one
owner, one activation path. The language runtimes (Go, bun, node, moon) are
pinned in `tools/toolchain/versions/*.nix`; the linters (biome, rumdl), rumdl's
base policy, and the Go analysis tools (golangci-lint, govulncheck, go-licenses,
nilaway) come from the locked `meissa` input, built by Meissa with the consumer's
Go toolchain. Everything else — protobuf/contract tooling (buf, protoc, and the
Go codegen plugins) — comes from the pinned nixpkgs revision. A single owner
means PATH order never silently decides which copy of a tool wins.

- **Local:** `direnv allow` puts the toolchain on PATH. devenv injects tools,
  not a whole shell — you keep your own prompt and dotfiles.
- **CI:** the identical derivations the dev shell builds are resolved from the
  same pinned sources and put on PATH — no `setup-*` actions. See CI below.

## Task graph: moon

[moon](https://moonrepo.dev) owns the task graph (`deps:`), result caching
(`inputs`/`outputs`), affected-target detection, and local parallel execution.
`moon run <project>:<task>` is the interface; `moon run :ci` runs every
project's `ci` task. Affected detection decides which projects a change
actually touches. In CI, the `setup` job queries moon's affected set and hands
each `moon` leg its targets; a local `moon run :ci --affected` is the same
detection on demand.

moon runs `go` and `bun` as **system tasks** — it execs the toolchain on PATH
rather than managing its own. moon's graph/caching layer stays
toolchain-agnostic; the nix pin files under `tools/toolchain/versions/*.nix`
remain the version source.

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

`.github/workflows/ci.yml` runs on every pull request, every push to `main`, and
a nightly schedule, and is re-run automatically (by `pr-base-repoint.yml`) when
a stacked PR's base moves. It is a set of jobs behind one required check,
`rollup`, which passes only when every job it needs succeeded, or skipped
because `setup` marked it unaffected. Branch protection requires only `rollup`,
so jobs can be added or renamed without touching it.

- **`setup`** asks moon which projects to run: the affected set on a pull
  request, every project on `main` and nightly. Some special-job flags come from
  a changed-paths diff instead. `tools/ci-matrix` turns that answer into the
  matrix for the `moon` job and an affected flag for each special job. Nothing
  about the workspace is listed in the workflow, so a new project is gated the
  moment it is registered in `.moon/workspace.yml` and tagged with a
  `ci-group.<name>` tag. On a pull request it also fails when the change adds
  or edits a file under `docs/designs/`. Design records live in
  [RigelBuild/docs](https://github.com/RigelBuild/docs); deleting a record here
  is allowed.
- **`moon`** runs the moon battery, one leg per concern group, each its own
  check, today `moon (go)`, `moon (bun)` and `moon (nix)`. This is the same task
  graph the local gate and the `hk` pre-push hook run.
- **Special jobs** need an environment the moon battery does not have. On a
  pull request each runs only when its flag says the change affects it. The Go
  suites derive those flags from tagged test roots and first-party dependency
  closures; gate changes and declared owner-task inputs widen selection. On
  `main` and nightly each always runs.
  - `pgtest`: the real-Postgres suites (`go test -tags pgtest -race ./...`)
    against a Postgres service container. `COMPASS_REQUIRE_LIVE=1` turns a
    missing database into a failure.
  - `e2e`: the podman tier in a privileged container, so rootless podman can
    load and run a tree-built agent image. On a pull request it runs for changes
    in its Go dependency closures or when an owner task widens the suite. The
    stack starts its own Postgres. The command is
    `go test -tags podman -race -count=1 -p 1 ./e2e/... ./cmd/compass-stack/...`.
    It builds `compass-postgres`, `compass-server` and `compass-runner`; the
    `compass-stack` package is tested as an E2E root but is not a built child.
  - `microvm`, `forge`, `gtk4`, `darwin` and `agent-fod-arm64` cover the
    microVM suites, the live forge contract, the GTK UI, the macOS bundle, and
    the arm64 agent dependency pin.
- **`bun-audit`** checks bun dependencies against the live advisory database,
  whatever the affected set.

A tagged suite skips when its dependency is missing, so `pgtest`, `e2e`,
`microvm` and `forge` each end with a step that fails if the suite skipped
instead of running.

**Affected on pull requests, full on `main`.** Affected detection trusts each
task's `inputs` globs and the affected closure, so an incomplete glob can skip
a task on a pull request. The full run on every push to `main` and the nightly
sweep catch that as soon as the change lands.

Which test tier to write at, and how the Go test harness is built:
[Go testing tiers](testing-tiers.md).

### Where CI's toolchain comes from

CI's toolchain is the local one — no remote-only seam. Nix builds the identical
derivations the dev shell builds, from the same pinned sources: the language
runtimes (bun, node, moon, go) from `tools/toolchain/versions/*.nix`; the Go
analysis battery (golangci-lint, govulncheck, go-licenses, nilaway), supplied by
Meissa's `devenvModules.go` module and `lib.goAnalysis` using that Go toolchain;
and biome and rumdl from the locked `meissa` input (all via
`tools/toolchain/gate-tools.nix`, which also yields `RUMDL_BASE_CONFIG`). Their
`bin/` dirs go on PATH; there are no `setup-*` actions. One owner, one activation
path, remotely and locally.

### The parity gate

Reading a version from a file does not prove the runner got it. A step before
the gate (`tools/toolchain/parity.ts`, also scheduled by `:ci` as
`toolchain-parity:parity`) asserts that the toolchain actually on PATH is the
one the dev shell defines, and **fails the build** — never warns — when it is
not. Every tool is pinned the same way now — as a nix derivation — so the gate
checks every one the same way: it resolves `realpath` of each command on PATH
and asserts it lands inside the derivation the pinned sources build. That is a
store-path identity check, stronger than comparing version strings: it catches
an ambient binary of the same version shadowing the pinned one, and it works
for the tools (`go-licenses`, `nilaway`) that implement no version flag at all.

A tool the gate cannot check is reported `UNVERIFIABLE` and fails the build.
Skipping what it cannot verify would make its green mean nothing.

### What CI does not gate

- **A live UI↔server path.** Every `compass-ui` task runs against fixtures, so
  no check exercises the UI against a running server.
- **The native app shell and the microVM boot lane.** `compass-app-dev`'s
  `build`/`run` and `compass-go:test-microvm` are `runInCI: false` — they need a
  GUI toolchain and KVM respectively. The `gtk4` and `microvm` jobs in
  `ci.yml` cover those paths instead, so neither is unguarded; they are just
  not gated through the moon battery.

## Publishing the agent image

Open [the agent image publishing guide](agent-image-publishing.md) for the tags, release workflow, and pre-merge build check.

## Release assets and install channels

Open [Release assets and install channels](release-assets.md) for the release artifacts and install channels.

## Caching

**moon task cache** — whole-task-output caching, keyed by an inputs hash.
