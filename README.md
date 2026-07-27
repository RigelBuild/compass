# Compass

Compass is an open-source desktop application for running, supervising, and
orchestrating AI coding agents. Bring your own agent: Compass owns the
privileged surface — agent processes, terminals, version control, and the
security layer around them — and gives you one place to drive them.

> **Status: early.** This repository currently holds the foundation — the
> toolchain, the workspace, the `compass.v1` contract pipeline, and the CI
> scaffold. It is not yet installable. The server, the desktop shell, and the
> UI are being built against the contract defined here.

## Architecture

Compass is a long-lived **server** plus a thin desktop **shell** and a **web
UI**, all speaking one typed contract:

```text
┌─────────────────────────────────────────────┐
│ Desktop shell (Tauri) + web UI (SolidJS)     │
│   renders the board, agent panes, terminals  │
└───────────────────────┬─────────────────────┘
                        │ compass.v1 (Connect / gRPC-Web)
┌───────────────────────▼─────────────────────┐
│ compass-server (Go)                          │
│   owns agent processes, PTYs, VCS, security  │
│   serves compass.v1 over a local transport   │
└──────────────────────────────────────────────┘
```

- **`compass.v1` is the single, owned door.** Every UI reaches the server only
  through the generated contract client — never a raw socket or hand-written
  stub. The schema lives at `proto/compass/v1/`; the generated Go and
  TypeScript clients are checked in and CI drift-gated, so a stale client fails
  the build.
- **The server owns everything privileged.** The shell holds no logic — it
  launches and supervises the server and points the webview at the contract.
- Native (gRPC over the local transport) and browser (gRPC-Web) clients share
  the same contract.

## Repository layout

```text
proto/
  compass/v1/            the schema — the owned door
go/
  cmd/compass-server/    the compass-server binary
  server/                the server implementation
  events/                the event bus
  gen/                   generated Go client/server stubs (checked in)
packages/
  compass-client/        generated TypeScript client (checked in)
apps/
  ui/                    web UI (SolidJS + Vite)
docs/architecture/       architecture notes
```

The build and toolchain config (`Cargo.toml`, `package.json`, `.moon/`,
`buf.*`, `devenv.nix`, `.prototools`, `deny.toml`, `biome.json`)
lives at the repository root.

## Toolchain

A strict split, three layers. **[proto](https://moonrepo.dev/proto)** pins the
bun/node/moon runtimes (`.prototools`).
**[fenix](https://github.com/nix-community/fenix)** builds the exact Rust
toolchain from `rust-toolchain.toml`. **[devenv](https://devenv.sh)** provides
the rest (protobuf/contract tooling, Rust dev tools, a C linker, the linters)
and emits the CI image. The dev shell is the supported path; the no-nix route
is below. Detail in
[`docs/architecture/build-and-ci.md`](./docs/architecture/build-and-ci.md).

## Quickstart

With [direnv](https://direnv.net) + devenv (recommended):

```bash
direnv allow      # loads the devenv shell (toolchain on PATH)
bun install       # install the workspace JS deps
moon run :ci      # the full local gate: build, lint, test, contract drift
```

The devenv shell is the supported path. Without nix you can still build: install
proto (it bootstraps bun, node, and moon from `.prototools`), install Rust with
rustup (it reads `rust-toolchain.toml`), and supply the rest the dev shell
otherwise provides — `buf`, `protoc`, `protoc-gen-prost`, `protoc-gen-tonic`,
and a C compiler — then `bun install` and `moon run :ci`.

`moon run :ci` is the entire CI gate — the same task graph runs locally and in
CI, so "passes locally" and "passes in CI" are the same check.

## Changing the contract

The `compass.v1` schema is the seam the whole app is built against. To change
it:

1. Edit the `.proto` files under `proto/compass/v1`.
2. Regenerate the clients: `moon run compass-proto:gen`.
3. Commit the regenerated `go/gen` and `packages/compass-client/src/gen`
   alongside the schema change.

CI runs `buf lint`, a backward-compatibility check (`buf breaking`), and a
drift gate (regenerate + `git diff`) — so the checked-in clients can never
silently fall out of sync with the schema.

## License

Compass is **AGPL-3.0-only** — see [LICENSE](./LICENSE).

The protocol is the exception. To let third-party UIs and closed-source
consumers link the contract without taking on the workspace's copyleft, the
`compass.v1` schema and the generated TypeScript client (`@compass/client`)
are licensed permissively as **`MIT OR Apache-2.0`** — the protocol is
permissive, the implementation is copyleft. See [LICENSE-MIT](./LICENSE-MIT)
and [LICENSE-APACHE](./LICENSE-APACHE).

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md) for development setup, the VCS
workflow, and pull-request conventions.
