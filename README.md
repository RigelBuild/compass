# Compass

Compass is an open-source desktop application for running, supervising, and
orchestrating AI coding agents. Bring your own agent: Compass owns the
privileged surface — agent processes, terminals, version control, and the
security layer around them — and gives you one place to drive them.

> **Status: early.** This repository currently holds the foundation — the
> toolchain, the workspace, the `compass.v1` contract pipeline, and the CI
> scaffold. It is not yet installable. The daemon, the desktop shell, and the
> UI are being built against the contract defined here.

## Architecture

Compass is a long-lived **daemon** plus a thin desktop **shell** and a **web
UI**, all speaking one typed contract:

```text
┌─────────────────────────────────────────────┐
│ Desktop shell (Tauri) + web UI (SolidJS)     │
│   renders the board, agent panes, terminals  │
└───────────────────────┬─────────────────────┘
                        │ compass.v1 (Connect / gRPC-Web)
┌───────────────────────▼─────────────────────┐
│ compass-daemon (Rust)                        │
│   owns agent processes, PTYs, VCS, security  │
│   serves compass.v1 over a local transport   │
└──────────────────────────────────────────────┘
```

- **`compass.v1` is the single, owned door.** Every UI reaches the daemon only
  through the generated contract client — never a raw socket or hand-written
  stub. The schema lives in [`proto/compass/v1`](./proto/compass/v1); the
  generated Rust and TypeScript clients are checked in and CI drift-gated, so a
  stale client fails the build.
- **The daemon owns everything privileged.** The shell holds no logic — it
  launches and supervises the daemon and points the webview at the contract.
- Native (gRPC over the local transport) and browser (gRPC-Web) clients share
  the same contract.

## Repository layout

```text
proto/compass/v1/        the compass.v1 schema — the owned door
crates/
  compass-proto/         generated Rust client/server stubs (checked in)
  compass-daemon/        the `compassd` daemon binary
packages/
  compass-client/        generated TypeScript client (checked in)
apps/
  ui/                    web UI (SolidJS + Vite)
ci/                      CI step image + publish script + Woodpecker pipeline
docs/architecture/       architecture notes
```

The build and toolchain config (`Cargo.toml`, `package.json`, `.moon/`,
`buf.*`, `devenv.nix`, `.prototools`, `deny.toml`, `biome.json`, `hk.pkl`)
lives at the repository root.

## Toolchain

Two layers, with a strict split: **[proto](https://moonrepo.dev/proto)** owns
the language toolchains (Rust, bun, node, moon — pinned in `.prototools` and
`rust-toolchain.toml`); **[devenv](https://devenv.sh)** provides everything
else (the protobuf/contract tooling, the Rust dev tools, the linters). proto
bootstraps standalone, so you do not need nix to build Compass — devenv is the
convenience path and the source of the CI image. The split is documented in
[`docs/architecture/build-and-ci.md`](./docs/architecture/build-and-ci.md).

## Quickstart

With [direnv](https://direnv.net) + devenv (recommended):

```bash
direnv allow      # loads the devenv shell (toolchain on PATH)
bun install       # install the workspace JS deps
moon run :ci      # the full local gate: build, lint, test, contract drift
```

The devenv shell is the supported path. Without nix you can still build: install
proto (it bootstraps the pinned Rust, bun, node, and moon toolchains from
`.prototools` and `rust-toolchain.toml`) and supply the protobuf toolchain the
dev shell otherwise provides — `buf`, `protoc`, `protoc-gen-prost`,
`protoc-gen-tonic` — then `bun install` and `moon run :ci`.

`moon run :ci` is the entire CI gate — the same task graph runs locally and in
CI, so "passes locally" and "passes in CI" are the same check.

## Changing the contract

The `compass.v1` schema is the seam the whole app is built against. To change
it:

1. Edit the `.proto` files under [`proto/compass/v1`](./proto/compass/v1).
2. Regenerate the clients: `moon run proto:gen`.
3. Commit the regenerated `crates/compass-proto/src/gen` and
   `packages/compass-client/src/gen` alongside the schema change.

CI runs `buf lint`, a backward-compatibility check (`buf breaking`), and a
drift gate (regenerate + `git diff`) — so the checked-in clients can never
silently fall out of sync with the schema.

## License

Compass is **AGPL-3.0-only** — see [LICENSE](./LICENSE).

The protocol crate is the exception. To let third-party UIs and closed-source
consumers link the contract without taking on the workspace's copyleft, the
**`compass-proto`** crate (and the generated TypeScript client,
`@compass/client`) is licensed permissively as **`MIT OR Apache-2.0`** — the
protocol is permissive, the implementation is copyleft. See
[LICENSE-MIT](./LICENSE-MIT) and [LICENSE-APACHE](./LICENSE-APACHE).

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md) for development setup, the VCS
workflow, and pull-request conventions.
