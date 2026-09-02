# Compass

Compass is the open-source **Agentic Software Factory**. You manage a tree of
long-lived **Manager** agents — each owns a lane of work, drives its own issues
and pull requests, coordinates with you and with other agents in chat threads,
and ships only what you review and merge. The agent inside every node is
[Oh My Pi](./forks/oh-my-pi); Compass is the full system around it — the
server, the runtime, the security boundary, and the surfaces you drive it all
through.

The product walkthrough — the Manager tree, the three surfaces, the comms
layer, multiplayer — lives at
**[rigel.build/compass](https://rigel.build/compass)**. This README is the
repository front door: what the code is, how it fits together, and how to build
and self-host it.

## Architecture

Compass is a three-tier system speaking one typed contract, `compass.v1`:

```text
┌────────────────────────────────────────────────┐
│ Desktop shell (Wails v3) + web UI (SolidJS)    │
│   renders the board, threads, and agent panes  │
└─────────────────────────┬──────────────────────┘
                          │ compass.v1 (Connect / gRPC-Web)
┌─────────────────────────▼──────────────────────┐
│ Server (Go) — the control plane                │
│   identity, session lifecycle, secrets, the    │
│   forge relay; serves compass.v1               │
└─────────────────────────┬──────────────────────┘
                          │ provisions + relays
┌─────────────────────────▼──────────────────────┐
│ Runner — host substrate                        │
│   one disposable, egress-sealed sandbox per    │
│   session; a resident agent process inside     │
└────────────────────────────────────────────────┘
```

- **The Server is the control plane.** It owns identity, persona/role, session
  lifecycle, secrets brokering, and the forge relay — and holds the sole forge
  write credential. It serves the `compass.v1` contract and places nothing
  itself.
- **The Runner is the host substrate.** It provisions, starts, and stops one
  disposable sandbox per session (a rootless-podman container today, a
  hardware-virtualized microVM in the end state), and forwards agent-initiated
  privileged calls to the Server as a pure relay.
- **The agent is resident and egress-sealed.** It holds no server credential.
  A privileged operation it appears to perform travels a fixed hop chain —
  agent → Runner → Server — and the Server resolves the session's authority
  from state it owns, so a `session_id` on the wire *selects* an account, it
  never carries one.

The agent and its session are **durable and Server-owned** (a Postgres hot tail
plus an S3 cold archive); the sandbox it runs in is **disposable** and dies with
the session. Resume rebuilds the compute and reconstructs the transcript into a
fresh sandbox. The full model is in
[`docs/concepts/architecture.md`](./docs/concepts/architecture.md).

## `compass.v1` — the single, owned door

Every client reaches the Server only through the generated contract client,
never a raw socket or a hand-written stub. The schema lives at
[`proto/compass/v1/`](./proto/compass/v1); the generated Go and TypeScript
clients are checked in and CI drift-gated, so a stale client fails the build.
Two services live in the package: **`CompassService`** (server liveness, the
event stream, the agent-session lifecycle) and **`CommsService`** (the
communication layer — accounts, channels, threaded messages, and their event
stream). Native clients (gRPC over the local transport) and browsers (gRPC-Web)
share one contract.

## Repository layout

```text
proto/compass/            the compass.v1 schema — the owned door
go/
  cmd/                    nine binaries — server, runner, stack, app, CLI, …
  internal/               server, runtime, runnerhub, store, comms, …
  gen/                    generated Go client/server stubs (checked in)
  e2e/                    the cross-process end-to-end suites
packages/
  compass-client/         generated TypeScript client (checked in)
  compass-agent/          the first-party Oh My Pi agent bundle
apps/
  ui/                     web UI (SolidJS + Vite)
config/                   agent-facing skills, rules, prompts, personas
docs/
  concepts/               the agent-system model — read to orient
  architecture/           build, CI, and toolchain notes
  designs/                frozen design records + the decision ledger
  specs/                  the living product/behavior spec
  self-host.md            the self-hosting guide
forks/                    vendored upstream subtrees (Oh My Pi), each nix-built
agent-image/ guest-image/ the sandbox image builds
app-bundle/               the desktop application bundle
tools/toolchain/          the CI/dev-shell version-parity gate
```

The `go/cmd/` binaries are `compass-server`, `compass-runner`, `compass-stack`
(the self-host supervisor), `compass-app` (the desktop shell), `compass` (the
CLI), plus `compass-postgres`, `compass-guestd`, `compass-mint-runner-token`,
and `compass-gen-cert`. Build and toolchain config (`package.json`, `.moon/`,
`buf.*`, `devenv.nix`, `tools/toolchain/versions/*.nix`, `biome.json`) lives at
the repository root.

## Concepts

Compass runs, supervises, and orchestrates AI coding agents, so orienting in
this repo needs the model behind that — how agents are named and billed, how
they communicate, and the principles the design holds to. That model lives
under [`docs/concepts/`](./docs/concepts/README.md):

- **[The comms model](./docs/concepts/comms-model.md)** — threads for
  conversation, the session log for work: an agent's two surfaces and why they
  are split. You never prompt into a running session.
- **[The architecture](./docs/concepts/architecture.md)** — the three-tier
  topology and the two load-bearing paths across it (the privileged-op relay
  and the durability tee).
- **[Durable agents, disposable compute](./docs/concepts/durable-agents-disposable-compute.md)**
  and **[isolation and egress](./docs/concepts/isolation-and-egress.md)** — the
  agent is durable and Server-owned; the sandbox is disposable and contained.
- **[Handles, accounts, and attribution](./docs/concepts/handle-vs-account.md)**
  and **[the persona convention](./docs/concepts/persona.md)** — how agents are
  identified, attributed, and given a stable working context.
- **[Self-hosted and managed](./docs/concepts/self-host-and-managed.md)** and
  **[tokens and billing](./docs/concepts/tokens-and-billing.md)** — two products
  over one core; you bring the tokens, and how the split shapes the design.
- **[No human clicks](./docs/concepts/no-human-clicks.md)**,
  **[read-only inspection](./docs/concepts/read-only-inspection.md)**, and
  **[review flow](./docs/concepts/review-flow.md)** — agents stand up the org
  through tools; the human holds the merge and the security boundary.

## Self-hosting

Compass self-hosts as a small set of binaries you run on a KVM-capable Linux
host. `compass-stack up` supervises the server, a bundled PostgreSQL, and the
agent runner as one stack; per-agent sessions run in microVMs. Install the
binaries from the nix flake or a release tarball, run
`compass-stack preflight`, and bring the stack up under systemd. The full guide
— deployment shapes, the bundled vs. external database, and the systemd unit —
is in [`docs/self-host.md`](./docs/self-host.md).

## Building from source

One toolchain owner: [devenv](https://devenv.sh) (nix underneath). It provides
every language toolchain — Go, bun, node, moon, pinned in
`tools/toolchain/versions/*.nix` — plus the contract tooling, the Go analysis
tools, and the linters. With [direnv](https://direnv.net) + devenv (the
supported path):

```bash
direnv allow      # loads the devenv shell (toolchain on PATH)
bun install       # install the workspace JS deps
moon run :ci      # the full local gate: build, lint, test, contract drift
```

`moon run :ci` is the entire gate, and CI
([`.github/workflows/ci.yml`](./.github/workflows/ci.yml)) runs that same
command on every pull request — so "passes locally" and "passes in CI" are the
same check, enforced by a version-parity gate that fails the build if CI's
toolchain has drifted from the dev shell's. One further job runs the
real-Postgres suites (build-tagged `pgtest`, excluded from the default
`go test` lane) against a service container.

Without nix you can still build: install the pinned Go, bun, node, and moon
versions by hand from `tools/toolchain/versions/*.nix`, and supply the contract
tooling the dev shell otherwise provides — `buf`, `protoc`, `protoc-gen-go`,
`protoc-gen-connect-go`, `protoc-gen-es`. `protoc-gen-es` resolves from `PATH`
(not `node_modules`), so install the version the generated headers are stamped
with — `grep '@generated by protoc-gen-es' packages/*/src/gen`. CI's stamp gate
rejects any other version, so a locally-consistent wrong version passes on your
machine but fails the merge. Detail in
[`docs/architecture/build-and-ci.md`](./docs/architecture/build-and-ci.md).

## Changing the contract

`compass.v1` is the seam the whole app is built against. To change it:

1. Edit the `.proto` files under [`proto/compass/v1`](./proto/compass/v1).
2. Regenerate the clients: `moon run compass-proto:gen`.
3. Commit the regenerated `go/gen` and `packages/compass-client/src/gen`
   alongside the schema change.

CI runs `buf lint`, a backward-compatibility check (`buf breaking`), and a
drift gate (regenerate + `git diff`), so the checked-in clients can never
silently fall out of sync with the schema.

## License

Compass is **AGPL-3.0-only** — see [LICENSE](./LICENSE).

The protocol is the exception. So third-party UIs and closed-source consumers
can link the contract without taking on the workspace's copyleft, the
`compass.v1` schema and the generated TypeScript client (`@compass/client`) are
licensed permissively as **`MIT OR Apache-2.0`** — the protocol is permissive,
the implementation is copyleft. See [LICENSE-MIT](./LICENSE-MIT) and
[LICENSE-APACHE](./LICENSE-APACHE).

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md) for development setup, the version-
control workflow, and pull-request conventions.
