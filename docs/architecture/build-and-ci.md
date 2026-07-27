# Build & CI

How a change goes from an editor to a verified build. Each layer is configured
in a real format and composes cleanly:

```text
proto       bun/node/moon version pins
fenix       the exact Rust toolchain (rust-toolchain.toml)
devenv      the dev shell (local) and the CI image (one definition)
moon        the task graph: what to build/test, caching, affected detection
Woodpecker  the control plane that schedules CI across machines
```

The headline property: **the same task graph runs CI and local CI.** Only the
scheduler differs — Woodpecker across machines remotely, moon on one box
locally.

## Toolchains: proto + fenix

[proto](https://moonrepo.dev/proto) pins the bun, node, and moon runtimes in
`.prototools`; it bootstraps standalone, so contributors get those without nix.
Rust is pinned in `rust-toolchain.toml` and built to that exact version by
[fenix](https://github.com/nix-community/fenix), which feeds both the dev shell
and the CI image (`devenv.nix`). On the no-nix path, rustup reads the same
`rust-toolchain.toml`. Tool versions live in those two files only.

## Dev environment: devenv

[devenv](https://devenv.sh) (nix underneath) provides everything that is not a
runtime or the Rust compiler: the protobuf/contract tooling (buf, protoc, the
Rust codegen plugins), the Rust dev tools (`cargo-deny`, `cargo-nextest`,
sccache), a C linker for cargo, and the linters. The split is strict — proto
owns the runtimes, fenix owns Rust, devenv owns the rest — so PATH order never
silently decides which copy of a tool wins.

- **Local:** `direnv allow` puts the toolchain on PATH. devenv injects tools,
  not a whole shell — you keep your own prompt and dotfiles.
- **CI parity:** the same `devenv.nix` emits an OCI image (the `ci` container,
  built from `ci/toolchain/ci-toolchain.nix`), so CI runs in the same toolchain
  developers use locally. `ci/image/publish-ci-image.ts` builds and pushes it.

## Task graph: moon

[moon](https://moonrepo.dev) owns the task graph (`deps:`), result caching
(`inputs`/`outputs`), affected-target detection, and local parallel execution.
`moon run <project>:<task>` is the interface; `moon run :ci` runs every
project's `ci` task. Affected detection (`moon run :ci --affected`) decides
which projects a change actually touches.

moon runs `cargo` and `bun` as **system tasks** — it execs the toolchain on
PATH (fenix Rust, proto runtimes) rather than managing its own. moon's
graph/caching layer stays toolchain-agnostic; `rust-toolchain.toml` and
`.prototools` remain the version sources.

## The contract gate

The `compass.v1` schema is the sole, owned door between any UI and the daemon,
and it is gated three ways:

- **`buf lint`** — schema style and consistency.
- **`buf breaking`** — rejects backward-incompatible schema edits.
- **Drift** — regenerate the clients, then `git diff --exit-code`. A checked-in
  client that no longer matches the schema fails the build, so generated code
  can never silently fall out of sync.

The generated Rust and TypeScript clients are checked in (not generated at build
time by consumers), so a normal `cargo build` / `bun install` needs no codegen
step.

## CI: Woodpecker

[Woodpecker CI](https://woodpecker-ci.org) (OSS, Apache-2) is the control
plane: webhook ingest, the job queue, and cross-machine scheduling. The pipeline
in `ci/woodpecker/` runs `moon run :ci` inside the CI image, so a remote run is
byte-identical to a local one.

This is the static bootstrap pipeline. Per-task affected fan-out — one job per
affected moon task, each body `moon run <project>:<task>`, so caching and
affected-skip apply remotely too — is produced by a pipeline generator once the
repository is wired into a Woodpecker control plane.

## Caching

- **sccache** — Rust compile caching, per compilation unit, inside the cargo
  task (set as `RUSTC_WRAPPER`). Independent of moon's cache.
- **moon task cache** — whole-task-output caching, keyed by an inputs hash.

## Test reporting

`cargo nextest` emits JUnit XML via the `ci` profile in `.config/nextest.toml`
(JUnit is config-only — there is no `--junit` flag), for CI ingestion.
