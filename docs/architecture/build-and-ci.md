# Build & CI

How a change goes from an editor to a verified build. Each layer is configured
in a real format and composes cleanly:

```text
proto       language-toolchain version pins
devenv      the dev shell (local) and the CI image (one definition)
moon        the task graph: what to build/test, caching, affected detection
Woodpecker  the control plane that schedules CI across machines
```

The headline property: **the same task graph runs CI and local CI.** Only the
scheduler differs — Woodpecker across machines remotely, moon on one box
locally.

## Toolchains: proto

[proto](https://moonrepo.dev/proto) pins every language toolchain in
`.prototools` (bun, node, moon) and `rust-toolchain.toml` (Rust). It is the
single place tool versions live. proto bootstraps standalone, so contributors
get the right toolchains without nix.

## Dev environment: devenv

[devenv](https://devenv.sh) (nix underneath) provides everything that is not a
language toolchain: the protobuf/contract tooling (buf, protoc, the Rust
codegen plugins), the Rust dev tools (`cargo-deny`, `cargo-nextest`, sccache),
and the linters. The split with proto is strict — **proto owns language
toolchains; devenv owns the rest** — so PATH order never silently decides which
of two copies of a tool wins.

- **Local:** `direnv allow` puts the toolchain on PATH. devenv injects tools,
  not a whole shell — you keep your own prompt and dotfiles.
- **CI parity:** the same `devenv.nix` emits an OCI image (the `ci` container,
  built from `ci/ci-toolchain.nix`), so CI runs in the same toolchain
  developers use locally. `ci/publish-ci-image.sh` builds and pushes it.

## Task graph: moon

[moon](https://moonrepo.dev) owns the task graph (`deps:`), result caching
(`inputs`/`outputs`), affected-target detection, and local parallel execution.
`moon run <project>:<task>` is the interface; `moon run :ci` runs every
project's `ci` task. Affected detection (`moon run :ci --affected`) decides
which projects a change actually touches.

moon runs `cargo` and `bun` as **system tasks** — it execs the toolchain proto
puts on PATH rather than managing its own. This keeps moon's graph/caching layer
(which is toolchain-agnostic) without moon owning the Rust version; proto stays
the single version source.

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
