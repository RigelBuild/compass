# Build & CI

How a change goes from an editor to a verified build:

```text
devenv      the dev shell — owns every toolchain (Go/bun/node/moon + the rest)
moon        the task graph: what to build/test, caching, affected detection
```

**The same task graph runs remotely and locally.** GitHub Actions schedules
remote runs; moon schedules local ones. The parity gate fails a build when the
toolchains differ. What `release.yml` publishes, and how a release is cut, is in
[Release and images](release.md).

## Toolchains: devenv/nix

[devenv](https://devenv.sh) (nix underneath) owns the entire toolchain, so PATH
order never decides which copy of a tool wins.

- The language runtimes (Go, bun, node, moon) are pinned in
  `tools/toolchain/versions/*.nix`.
- The linters (biome, rumdl), rumdl's base policy, and the Go analysis tools
  (golangci-lint, govulncheck, go-licenses, nilaway) come from the locked
  `meissa` input. Meissa builds them with the consumer's Go toolchain, through
  its `devenvModules.go` module and `lib.goAnalysis`.
- Everything else, including the contract tooling (buf, protoc, the Go codegen
  plugins), comes from the pinned nixpkgs revision.

**Local:** `direnv allow` puts the toolchain on PATH. devenv injects tools, not a
whole shell, so you keep your own prompt and dotfiles.

**CI:** nix builds the same derivations from the same pinned sources, through
`tools/toolchain/gate-tools.nix`, which also yields `RUMDL_BASE_CONFIG`. Their
`bin/` dirs go on PATH. There are no `setup-*` actions.

## Task graph: moon

[moon](https://moonrepo.dev) owns the task graph (`deps:`), result caching
(`inputs`/`outputs`), affected-target detection, and local parallel execution.
`moon run <project>:<task>` is the interface; `moon run :ci` runs every
project's `ci` task, and `moon run :ci --affected` runs only what a change
touches.

moon runs `go` and `bun` as **system tasks**: it execs the toolchain on PATH
rather than managing its own. The nix pin files stay the version source.

## The contract gate

The `compass.v1` schema is the only door between any UI and the daemon. It is
gated three ways:

- **`buf lint`**: schema style and consistency.
- **`buf breaking`**: rejects backward-incompatible schema edits.
- **Drift**: regenerate the clients, then `git diff --exit-code`. A checked-in
  client that no longer matches the schema fails the build.

The generated Go and TypeScript clients are checked in, so a normal
`go build` / `bun install` needs no codegen step.

## CI

`.github/workflows/ci.yml` runs on every pull request, every push to `main`, and
a nightly schedule. `pr-base-repoint.yml` re-runs it when a stacked PR's base
moves. Branch protection requires one check, `rollup`. It passes only when every
job it needs succeeded, or skipped because `setup` marked it unaffected, so jobs
can be added or renamed without touching branch protection.

- **`setup`** asks moon which projects to run: the affected set on a pull
  request, every project on `main` and nightly. Some special-job flags come from
  a changed-paths diff instead. `tools/ci-matrix` turns the answer into the
  `moon` matrix and one affected flag per special job. The workflow lists no
  projects, so a new project is gated once it is in `.moon/workspace.yml` with a
  `ci-group.<name>` tag. On a pull request, `setup` also fails when the change
  adds or edits a file under `docs/designs/`; design records live in
  [RigelBuild/docs](https://github.com/RigelBuild/docs). Deleting one here is
  allowed.
- **`moon`** runs one leg per concern group, each its own check: `moon (go)`,
  `moon (bun)` and `moon (nix)`. The local gate and the `hk` pre-push hook run
  the same task graph.
- **Special jobs** need an environment the `moon` legs lack. On a pull request
  each runs only when its flag says the change affects it. The Go suites derive
  their flags from tagged test roots and first-party dependency closures; gate
  changes and declared owner-task inputs widen selection. On `main` and nightly
  each always runs.
  - `pgtest`: the real-Postgres suites (`go test -tags pgtest -race ./...`)
    against a Postgres service container. `COMPASS_REQUIRE_LIVE=1` turns a
    missing database into a failure.
  - `e2e`: the podman tier, in a privileged container so rootless podman can
    load and run a tree-built agent image. The stack starts its own Postgres.
    The command is
    `go test -tags podman -race -count=1 -p 1 ./e2e/... ./cmd/compass-stack/...`.
    It builds `compass-postgres`, `compass-server` and `compass-runner`; the
    `compass-stack` package is tested as an E2E root but is not a built child.
  - `microvm`, `forge` and `gtk4` cover the microVM suites, the live forge
    contract and the GTK UI.
  - `darwin` builds the macOS shell and the ad-hoc signed `.app`/`.dmg` with
    `tools/macos-bundle` on `macos-14`. Its PR flag, `darwin_affected`, comes
    from the changed paths.
  - `agent-fod-arm64` re-runs `renovate:agent-fod` on `ubuntu-24.04-arm`.
    `agent-image/entrypoint.nix` pins one node_modules `outputHash` per system,
    and the moon task checks only the host's. Its PR flag, `agent_fod_affected`,
    is set when moon reports `renovate:agent-fod` affected.
    `refresh-fod-hashes.ts` rewrites both pins from any host, since each FOD
    picks its arch with `bun install --cpu`.
- **`bun-audit`** checks bun dependencies against the live advisory database on
  every run.

A tagged suite skips when its dependency is missing, so `pgtest`, `e2e`,
`microvm` and `forge` each end with a step that fails if the suite skipped.

**Affected on pull requests, full on `main`.** Affected detection trusts each
task's `inputs` globs and the affected closure, so an incomplete glob can skip a
task on a pull request. The full run on every push to `main` and the nightly
sweep catch it once the change lands.

Which test tier to write at, and how the Go test harness is built:
[Go testing tiers](testing-tiers.md).

### The parity gate

Reading a version from a file does not prove the runner got it.
`tools/toolchain/parity.ts`, scheduled by `:ci` as `toolchain-parity:parity`,
resolves the `realpath` of each command on PATH. It **fails the build** unless
the path is inside the derivation the pinned sources build. This store-path
check catches an ambient binary of the same version, and it works for tools with
no version flag (`go-licenses`, `nilaway`). A tool it cannot check is reported
`UNVERIFIABLE` and also fails the build; skipping it would make green mean
nothing.

### What CI does not gate

- **A live UI↔server path.** Every `compass-ui` task runs against fixtures.
- **The native app shell and the microVM boot lane** in the moon battery.
  `compass-app-dev`'s `build`/`run` and `compass-go:test-microvm` are
  `runInCI: false`, because they need a GUI toolchain and KVM. The `gtk4` and
  `microvm` jobs cover those paths instead.

## Caching

**moon task cache** — whole-task-output caching, keyed by an inputs hash.
