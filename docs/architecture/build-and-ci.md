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
project's `ci` task. Affected detection decides which projects a change
actually touches — CI's PR gate drives it through `moon ci :ci` (the
CI-environment form, which reads the base from the provider), while a local
`moon run :ci --affected` is the same detection on demand.

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

`.github/workflows/ci.yml` runs on every pull request, every push to `main`,
and a nightly schedule. One job, `CI`, with two parts:

- **The moon battery** — the whole battery over the moon task graph. It runs
  one of two ways by event. On a **pull request** it is `moon ci :ci`, which
  runs only the projects the PR affects — a Go, UI, or docs change never pays
  for the vendored forks' nix builds. On a **push to `main`** and on the
  **nightly schedule** it is the full `moon run :ci`: every task, every project,
  no affected filter. Affected detection trusts each task's `inputs` globs, so
  the full sweep on everything that reaches `main` is the backstop — an
  incomplete glob that let a task be skipped on a PR is caught the moment the
  change lands (and re-checked nightly), named rather than hidden. Either way
  nothing about the workspace is enumerated in the workflow, so a new project
  (or a newly vendored fork) is gated the moment it is registered in
  `.moon/workspace.yml`. This is the same task graph the local gate and the
  `hk` pre-push hook run.
- **The real-Postgres suites** — build-tagged `pgtest`, and therefore never
  compiled by the moon battery's `go test ./...`. They run as a step in this
  same job, unconditionally (no affected filter and no event filter, so they
  run on every trigger including the nightly), against a Postgres service
  container attached to the job. A step afterwards asserts they actually ran:
  the harness *skips* when it finds no database, so a service that never came
  up would otherwise pass silently. These suites were once a separate `pgtest`
  job, to keep a Postgres-service outage from redding the hermetic gate; they
  were folded in so there is one required check to gate `main` on, at the cost
  that a service-container flake now reds `CI` (a re-run clears it).

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

## Publishing the agent image

The `compass-agent` runtime image (the base every agent workstream runs in) is
published to GHCR by a workflow separate from the gate above. For the full
rationale see the design record
`docs/designs/platform/compass-agent-image-publish.md`; the durable operational
shape is here.

**Ref and tags.** The image is `ghcr.io/sealedsecurity/compass-agent`. Every
closure-affecting main build publishes two tags:

- `:git-<sha12>` — the 12-hex short commit sha, **immutable**. This is the pin
  the native app bakes in and hands the runner via `--image` /
  `$COMPASS_AGENT_IMAGE`; it is the real consumption path. The publish refuses
  to overwrite an existing `:git-<sha>` whose content differs and re-inspects
  after each push to assert the digest landed, so the tag is immutable by
  enforcement, not convention.
- `:latest` — moving, documented **first-run fallback only**, never the default.

The git-sha tag is pushed before `:latest`, so the immutable pin always exists
before the moving tag moves. Platform is `linux/amd64` single-arch (the dogfood
milestone target; macOS/`aarch64` multi-arch is a GA follow-up). The package is
**public** — compass is open-source, the image payload is public source, and it
carries no runtime secrets (those are runner-supplied per-exec) — so the
first-run pull needs no credential anywhere.

**One derivation, two destinations.** The published `:git-<sha>` and the local
`dogfood:agent-image` load are copies of the *same* nix derivation — both flow
through the fork's `container build agent`. They diverge only in the skopeo
destination (a registry ref versus `containers-storage:`), so what CI publishes
is byte-for-byte what a developer loads locally.

**A separate least-privilege workflow.** Publishing lives in
`.github/workflows/publish-agent-image.yml`, **not** a step in the `CI` gate and
**not** a required check. It runs main-only plus `workflow_dispatch`,
path-filtered to the image's nix closure, with its own concurrency group set to
`cancel-in-progress: false` (publishes serialize rather than tear a tag pair
mid-push) and a `packages: write` token the gate job never gets. This is a
principled exception to the [ONE-JOB doctrine](#ci): the doctrine exists to stop
a second source of truth for *what the gate covers*, and this workflow
enumerates no moon projects (`agent-image/` is a standalone devenv, not a moon
project) — so it recreates none of the silent-staleness failure the doctrine
guards against. A published tag is the source of truth; a missing one (paths
filtered it out, or a superseding push skipped it under the serialized
concurrency group) is **not** a failure — `workflow_dispatch` republishes any
HEAD on demand.

**Smoke.** On a runner host, pull the immutable tag and drive the consumer seam:

```sh
podman pull ghcr.io/sealedsecurity/compass-agent:git-<sha12>
compass-runner --image ghcr.io/sealedsecurity/compass-agent:git-<sha12>
# then drive one provision
```

**One-time setup.** The first `GITHUB_TOKEN` push creates the package
private-by-default (and only if the org policy permits `GITHUB_TOKEN`-created
packages, else the push 403s and an owner must pre-create it). An owner sets the
package **public** once in its settings after that first push; the repo linkage
grants the workflow write access thereafter. Pre-creating the empty package
also settles the immutability guard's first-publish edge: the guard inspects
`:git-<sha>` *before* the creating copy and only an authoritative
`manifest unknown` frees the tag, so an owner-pre-created (hence
authenticatable) package guarantees the absent-tag inspect classifies cleanly
rather than on a not-yet-existent repository's error shape.

### Pre-merge build check

`agent-image/` is registered as the `compass-agent-image` moon project
(`.moon/workspace.yml`), so the CI gate builds the image on any PR that
affects its closure. Before this the image was outside moon and had zero
pre-merge coverage — an image-build break surfaced only post-merge in the
publish workflow, while a consumer waited on a tag.

The project's `build` task realises the image with the same fork-pinned
derivation the publish lane ships
(`nix run path:../forks/devenv#devenv -- container build agent`), so a green
build proves the exact artifact that publishes still builds — both an
eval-time break (a bun-pin drift against the `agent-image/toolchain.nix`
assert) and a realise-time break (an `agent-image/entrypoint.nix` FOD-hash
invalidation or a broken bundle), the full class.

The build is heavy — the image closure is the dominant CI cost, the reason
the gate's timeout is 90m — but it is not paid on every PR. `moon ci` runs a
PR's *affected* projects only, and the task's `inputs` scope it to the image
closure: the `agent-image/` tree, the two vendored forks, `packages/compass-agent/`,
the root `package.json` and `bun.lock`, and `.prototools`. A PR that touches
none of those never builds the image; every push to main runs it
unconditionally in the full sweep. Its `inputs` mirror the publish workflow's
`on.push.paths` — the reviewed source of truth for what changes the published
artifact — plus `.prototools` (which publish excludes because a pin move there
cannot change the output, but which reddens the toolchain assert, exactly what
this gate exists to catch at PR time). As a project in the one-job gate it is a
required check: a build break blocks merge, the same posture as the vendored
forks' nix builds.

## Caching

**moon task cache** — whole-task-output caching, keyed by an inputs hash.
