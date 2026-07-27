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

CI runs on GitHub Actions, reproducing the same task battery the local
`moon run :ci` gate runs.

## Caching

**moon task cache** — whole-task-output caching, keyed by an inputs hash.
