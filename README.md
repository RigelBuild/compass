# Compass

Compass is an open-source desktop application for running, supervising, and
orchestrating AI coding agents. Bring your own agent: Compass owns the
privileged surface — agent processes, terminals, version control, and the
security layer around them — and gives you one place to drive them.

> **Status: early.** This repository currently holds the foundation — the
> toolchain, the workspace, the `compass.v1` contract pipeline, and the CI
> pipeline that gates them. It is not yet installable. The server, the desktop
> shell, and the UI are being built against the contract defined here.

## Architecture

Compass is a long-lived **server** plus a thin desktop **shell** and a **web
UI**, all speaking one typed contract:

```text
┌──────────────────────────────────────────────┐
│ Desktop shell (Wails v3) + web UI (SolidJS)  │
│   renders the board and agent panes          │
└───────────────────────┬──────────────────────┘
                        │ compass.v1 (Connect / gRPC-Web)
┌───────────────────────▼──────────────────────┐
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
  compass-agent/         the first-party agent
apps/
  ui/                    web UI (SolidJS + Vite)
forks/                   vendored upstream fork subtrees, each nix-built
tools/toolchain/         the CI/dev-shell version-parity gate
docs/architecture/       architecture notes
docs/concepts/           the agent-system model — read to orient
```

The build and toolchain config (`package.json`, `.moon/`, `buf.*`,
`devenv.nix`, `tools/toolchain/versions/*.nix`, `biome.json`) lives at the
repository root.

## Agent concepts

Compass runs, supervises, and orchestrates AI coding agents, so a reader
orienting in this repo needs the model behind that — how agents are named and
billed, how they are prompted, what tools they drive, and the principles the
design holds to. That model lives under
[`docs/concepts/`](./docs/concepts/README.md):

- [Handles, accounts, and attribution](./docs/concepts/handle-vs-account.md) —
  a handle names one agent; agents share one forge account to bill
  through.
- [The persona convention](./docs/concepts/persona.md) — role vs persona; a
  persona is the agent's stable working context.
- [The agent tool set](./docs/concepts/tools.md) — the native tools agents
  drive Compass through, and the flow of using them.
- [No human clicks](./docs/concepts/no-human-clicks.md) and [read-only
  inspection](./docs/concepts/read-only-inspection.md) — agents stand up the org
  through tools; the human holds only the security boundary.
- [Durable agents, disposable compute](./docs/concepts/durable-agents-disposable-compute.md)
  and [isolation and egress](./docs/concepts/isolation-and-egress.md) — the agent
  and its session are durable and Server-owned; the container/microVM it runs in
  is disposable and contained.

## Toolchain

One owner, [devenv](https://devenv.sh) (nix underneath). It provides every
language toolchain — Go, bun, node, moon, pinned in
`tools/toolchain/versions/*.nix` — plus the rest (protobuf/contract tooling,
the Go analysis tools, the linters). The dev shell is the supported path; the
no-nix route is below. Detail in
[`docs/architecture/build-and-ci.md`](./docs/architecture/build-and-ci.md).

## Quickstart

With [direnv](https://direnv.net) + devenv (recommended):

```bash
direnv allow      # loads the devenv shell (toolchain on PATH)
bun install       # install the workspace JS deps
moon run :ci      # the full local gate: build, lint, test, contract drift
```

The devenv shell is the supported path. Without nix you can still build:
install the pinned Go, bun, node, and moon versions by hand from the pin files
under `tools/toolchain/versions/*.nix`, and supply the rest the dev shell
otherwise provides — `buf`, `protoc`, `protoc-gen-go`, `protoc-gen-connect-go`,
`protoc-gen-es` — then `bun install` and `moon run :ci`.

`protoc-gen-es` is on that list rather than arriving with `bun install`: the
codegen tasks resolve it from PATH, not from `node_modules`, so its version is
pinned by the toolchain that stamps it into every generated header instead of by
a lockfile that can float underneath it. Install the version the generated
headers are stamped with — `grep '@generated by protoc-gen-es' packages/*/src/gen`
(currently `v2.12.0`). A locally-consistent wrong version (regenerate + drift
with the same off-version plugin) passes on your machine, but CI's stamp gate
compares the checked-in stamp against the nix-pinned plugin, so it rejects any
version other than the pinned one there — where it gates the merge.

`moon run :ci` is the entire gate. CI (`.github/workflows/ci.yml`) runs that
same command on every pull request, so "passes locally" and "passes in CI" are
the same check — and a version-parity gate fails the build if CI's toolchain
has drifted from the dev shell's, so that equivalence is enforced rather than
assumed. One further job runs the real-Postgres suites (build-tagged `pgtest`,
so they are excluded from the default `go test` lane) against a service
container.

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
