# Compass forks reversal — pinned GitHub imports (RIG-2336)

> **Design record.** Reverses compass's `forks/` vendoring: each fork returns to its
> public `RigelBuild/*` repo and compass pins it as an ordinary `github:` nix flake
> input, removing the vendored subtrees and their machinery.

Status: proposed. Owner: compass-repo.

## Problem / Intent

Compass vendors three upstream forks as subtrees under `forks/` — 7424 tracked
files (`git ls-files forks/ | wc -l`, this checkout), 5892 of them the
consumer-less `oh-my-pi` tree — dragging upstream toolchains, style-gate
carve-outs, moon registrations, and sync/provenance machinery into the repo.
This design reverses the vendoring: each fork returns to its public
`RigelBuild/<fork>` GitHub repo (which builds and gates on GitHub Actions), and
compass consumes it as an ordinary pinned dependency — a `github:RigelBuild/<fork>`
nix flake input locked to a rev — removing the subtrees and every piece of
machinery that existed only to carry them.

## Global Constraints

- **FROZEN (Matt, 2026-08-19): shared `RigelBuild/{devenv,nix2container,oh-my-pi}`
  repos + combined patch work.** Compass consumes the shared canonical fork
  repos — one canonical fork per upstream. Sealed patches useful to both the
  internal monorepo and compass land in the shared repos, never duplicated. Do
  not relitigate; tasks execute it.
- **Shared-repo patch ownership is disjoint (forge coordination, 2026-08-19).**
  On the shared canonical repos each lane owns a disjoint patch set: forge owns
  `RigelBuild/nix2container`'s nix-DB-drop fix and that repo's standup/CI/release
  scaffolding; compass owns `RigelBuild/devenv`'s `containers.nix`
  patch. Compass only *consumes* nix2container, never patches it; forge's devenv
  reversal (RIG-2216) repoints nothing, so there is zero overlap today. If a
  third shared repo ever needs both lanes' patches, forge lands the base
  standup/CI/release scaffolding first and compass stacks its specific patch on
  top, never rewriting forge's work — the cross-lane-stack rule.
- **Issue keys are RIG-NNN.** This record is RIG-2336; impl lanes are filed from
  the frozen record with their own RIG keys. The driver owns all VCS and issue
  filing.
- **planning-evidence:** every claim about compass code in this record carries
  file+line verified in this checkout (`main` @ `13a43cec`), with a quoted
  snippet where the claim is load-bearing.
- **No ledger delta.** Compass's design-ledger gate is product-scoped only:
  `tools/design-ledger-gate/index.ts:45` — `export const PRODUCT_DIR =
  "docs/designs/product"`. This record lives under `docs/designs/platform/`, so
  it needs no `DECISIONS.md` delta and no `Ledger-impact:` line.
- **markdownlint-clean.** This record is first-party prose under `docs/`; it
  passes `.markdownlint-cli2.jsonc` as-is.
- **Every import pinned + verifiable.** A `github:` flake input pins an exact
  rev in `agent-image/devenv.lock`. No floating `latest`, no branch-tracking
  ref reaches a build.
- **Main is never red between a fork PR and the import-bump PR.** Compass
  builds only against already-merged fork-repo revs; a fork-side change lands
  first, the compass lock bump follows as its own PR. The failure mode of the
  fork/lock split is staleness, never breakage (see Approach § Atomicity). The
  raw-CLI call sites that bypass the lock are a *separate* channel with their own
  consistency obligation — resolved by the one-lockfile shape (OQ2), not by this
  split.

## Approach

### The general pattern

One rule, per fork: **the fork repo builds and releases; compass pins the
release.** Each fork lives in its public `RigelBuild/<fork>` repo, gated by its
own GitHub Actions CI. All of compass's relevant forks are nix-source, so the
import mechanism is uniform: repoint the flake input from `path:../forks/<fork>`
to `github:RigelBuild/<fork>`, pinned to an exact rev via `agent-image/devenv.lock`.
That is a URL change plus a lock pin — there is no artifact-publishing pipeline
to stand up.

The `github:` pin also fixes a real build-hygiene problem the `path:` inputs
have today: a `path:../forks/<fork>` input is locked with no rev at all
(`agent-image/devenv.lock:66-73` — `"locked": { "path": "../forks/devenv",
"type": "path" }`), so its identity is the enclosing tree's narHash and any
repo change can invalidate it. A `github:` input pins the fork repo's own rev
and rebuilds only on an explicit bump.

Bumps are deliberate: a fork-repo change merges there first, then a compass PR
updates the lock (`devenv update` / `nix flake lock --update-input`). Between
the two, compass keeps building against the previous pinned rev.

**One rev, not two.** The module set is pinned in `agent-image/devenv.lock`, but
the fork's CLI is also invoked raw (`nix run path:../forks/<fork>#…`) in the
raw-CLI call sites (six files, incl. `tools/agent-image-env-gate/index.ts`) that
bypass the lock (see L1/L2 Interfaces). Today a single `path:` tree
makes CLI-rev == module-rev by construction (`devenv.nix:442-445` names exactly
this as the reason for the pin shape). The flake-input side follows the internal
monorepo's frozen default — `github:RigelBuild/<fork>` pinned via `devenv.lock` —
but the six raw-CLI sites bypass that lock, so the reversal MUST separately
preserve the CLI-rev == module-rev identity: the recommended shape is a tiny
compass-side flake re-exporting the locked inputs so every consumer resolves one
lockfile rev (OQ2). Scattering a
`github:RigelBuild/<fork>/<rev>` literal across the CLI call sites reintroduces a
silent divergence channel — a bump that edits the lock but not a literal builds
the image with the module set at one rev driven by a CLI at another, with no
gate to catch it. That intra-compass split is a breakage channel the two-PR
fork/lock split does not cover.

**Trust surface.** Vendoring meant every fork line changed only via a compass
PR, with byte-identity as the review basis (`forks/README.md:243-247`). After
the reversal, fork code that compass CI executes changes via `RigelBuild/*`
review, and the exact-rev lock pin is compass's sole control. The
`accept-flake-config` warnings that today reason about an in-repo, PR-reviewed
`forks/devenv/flake.nix` (`ci.yml:157-161`, `eng-docs-deploy.yml:49-53`,
`publish-agent-image.yml:111-115`) must be reworded in L2 to say the new true
thing: the `nixConfig`-carrying flake is fetched from `RigelBuild/devenv` at a
pinned rev, `RigelBuild/*` branch protection is the merge gate, and the bump PR
is compass's review point — not a mechanical path swap.

### devenv

**Consumer shape (verified this checkout):**

- `agent-image/devenv.yaml:44-45` — the agent image's devenv module source:

  ```yaml
  devenv:
    url: path:../forks/devenv
  ```

- Locked as a rev-less path input at `agent-image/devenv.lock:67,71`
  (`"path": "../forks/devenv"`).
- The fork's own CLI is invoked by path everywhere the image is built:
  `agent-image/moon.yml:44` (`command: 'nix run path:../forks/devenv#devenv --
  container build agent'`), `agent-image/publish.sh:50-51`, `devenv.nix:461`
  (`nix run path:../forks/devenv#devenv -- container copy agent`),
  `.github/workflows/ci.yml:812`, and
  `tools/agent-image-env-gate/index.ts:100` — the fail-closed image-env gate
  (`tools/agent-image-env-gate/`, a registered moon project at
  `.moon/workspace.yml:60`) builds the image through the same CLI, and its
  `moon.yml` check task carries `/forks/devenv/**` as an affected-detection
  input glob (`agent-image-env-gate/moon.yml:58`).

**The sealed patch is compass-specific and load-bearing.** All of it sits in
`forks/devenv/src/modules/containers.nix`:

- Per-container `user`/`group`/`homeDir` options (`containers.nix:373` —
  `homeDir = lib.mkOption { type = types.str; …` — upstream hardcodes
  `user = "user"; homeDir = "/env";` module-wide), keeping upstream's values as
  defaults so a no-op consumer resolves byte-identical.
- The `imageEnv` DEVENV_-prefix filter (`containers.nix:181` — `imageEnv =
  lib.filterAttrs (name: _: !(lib.hasPrefix "DEVENV_" name)) config.env;`).
- A config-only `buildingContainer` lookup (`containers.nix:76-81`) replacing
  upstream's impure `envContainerName = builtins.getEnv "DEVENV_CONTAINER"`.

Without this patch the agent image's `$HOME` is root-owned `/env` instead of
`/home/agent` and nix fails with "$HOME is not owned by you"
(`agent-image/devenv.yaml:33-40` documents exactly this).

**Landing sequence — load-bearing.** `RigelBuild/devenv` `main` is today PLAIN
UPSTREAM (verified 2026-08-19 against `RigelBuild/devenv@afed7bf3`
`src/modules/containers.nix`: `homeDir = "/env";` hardcoded in a module-scope
`let`, `envContainerName = builtins.getEnv "DEVENV_CONTAINER"` present, no
per-container identity options). The shared `RigelBuild/devenv` fork carries no
sealed diff for this and has no devenv consumer of its own. Per Matt's ruling,
compass's `containers.nix` patch set therefore **lands INTO `RigelBuild/devenv`
first** (harmless to other consumers, required by compass), and only then does
compass repoint to `github:RigelBuild/devenv`. A naive repoint before the patch
lands would build the agent image against upstream's module and break it. The
fork-repo PR must
carry the patch's provenance notes from `forks/README.md` §devenv (the
byte-identity-by-construction defaults argument) so the shared repo keeps the
rationale.

### nix2container

**Consumer shape (verified this checkout):**

- `agent-image/devenv.yaml:20` — `url: path:../forks/nix2container` (the
  container module builds through it); locked rev-less at
  `agent-image/devenv.lock:216,220`.
- The fork's patched skopeo is invoked by path:
  `.github/workflows/publish-agent-image.yml:139,160`, `agent-image/publish.sh:32`,
  and `tools/agent-image-env-gate/index.ts:118` — `nix run
  path:../forks/nix2container#skopeo-nix2container`. The env-gate also globs
  `/forks/nix2container/**` (`agent-image-env-gate/moon.yml:59`).

**The sealed patch is shared.** `forks/nix2container/default.nix:396-399` drops
relocated copyToRoot paths from the initialized nix DB:

```nix
nixDatabase = let
  ignore = [configFile] ++ copyToRootList ++ allLayers;
  closureGraphForAllLayers = closureGraph ([configFile] ++ copyToRootList ++ allLayers) ignore;
in makeNixDatabase closureGraphForAllLayers;
```

versus upstream's `ignore = [configFile]++allLayers;` — without it the in-image
nix DB claims store paths the image does not carry, breaking image self-rebuild
with a failed lstat (comment block at `default.nix:386-395`). This is the SAME
shared nix-DB-drop fix that landed onto `RigelBuild/nix2container`. That fork
repo's `main` now carries it: rev
`8f4a6fd7b10abaeeddff6c4d8bb4908c5123c90c` (verified 2026-08-19,
`git ls-remote https://github.com/RigelBuild/nix2container` — public, no auth)
is upstream `master` plus the one relocated-copyToRoot-paths patch, byte-identical
to the fix compass carries at `forks/nix2container/default.nix:386-395`. So
consuming that rev restores the fix by construction — compass never re-patches
it.

**Landing sequence — cleared.** The shared nix-DB-drop fix landed on
`RigelBuild/nix2container` (rev `8f4a6fd7`, **merged 2026-08-19**); the internal
monorepo deleted its own vendored `nix2container` subtree and now consumes the
fork via a rev-pinned `github:` input. The external RIG-2332 block (the sole
arm64 image-builder was
down) is therefore **resolved** — the fork content is live on
`RigelBuild/nix2container` `main`. Compass consumes the same canonical repo
(Matt's one-fork-per-upstream ruling) at rev `8f4a6fd7`, does **not** stand up a
duplicate, and if it ever needs a patch it combines onto
`RigelBuild/nix2container` `main` rather than re-forking. L1 was sequenced last
while this prerequisite was open; it is now unblocked and free to run once the
record freezes. (forge signalled the merge over the cross-lane channel; both the
merge and the rev were re-verified at source before this fold.)

### oh-my-pi

**Consumer shape: no *build* consumer, one *tooling* consumer.** The tree is
plain upstream at tag `v17.1.8` with no sealed diff (`forks/README.md:151-155` —
"Sealed changes: NONE … verified byte-identical to `can1357/oh-my-pi` at
`v17.1.8`"); no compass image or app build consumes it (`forks/README.md:214`).
But it is **not** consumer-free: the store door's credential-denylist generator
reads the subtree directly — `go/internal/store/gen_credential_keys.go:42`
(`const schemaRelPath = "../../../forks/oh-my-pi/packages/coding-agent/src/config/settings-schema.ts"`),
invoked via `//go:generate go run gen_credential_keys.go`
(`go/internal/store/agent_config.go:659`), with the generated output naming the
subtree as its source (`go/internal/store/credential_keys_gen.go:3`) and a test
that instructs regeneration on schema change
(`go/internal/store/agent_config_test.go:505-518`). The `@oh-my-pi/*` references
in `agent-image/entrypoint.nix` and `packages/compass-agent/package.json:17-19`
are npm-registry packages, not this subtree.

Deleting the tree does **not** red main — the generator is `//go:build ignore`
and no CI step runs `go generate` (verified: none in `.github/`, `go/moon.yml`,
`.moon/`) — but it silently breaks a security-relevant refresh: the next
`go generate ./...` at an SDK bump fails on a missing file, and the fork-bump
checklist that keeps the credential denylist tracking the SDK's `isCredential`
markers dangles. A latent break in a security mechanism is worse than a red
build; L3 must repoint the generator, not just delete the tree.

**Recommendation: drop the subtree, repoint the generator to the npm copy.**
There is no *build* consumer to repoint, so the tree deletion is clean and
removes 5892 of the 7424 vendored files in one move. The one real consumer —
the generator — should read the npm-installed schema the agent actually runs
(`packages/compass-agent/node_modules/@oh-my-pi/pi-coding-agent/src/config/settings-schema.ts`, pinned
at `packages/compass-agent/package.json:19`), so the denylist tracks the version
in production rather than a vendored snapshot. The sealed deltas catalogued in
`forks/README.md:156-197` live in the monorepo fork and are
`RigelBuild/oh-my-pi`'s concern, not compass's. Drop-vs-consume is a
load-bearing Open Question (OQ1) — but the one real consumer wanting the npm
schema *strengthens* drop; consuming `github:RigelBuild/oh-my-pi` would only be
for the generator alone and is heavier for no runtime gain.

### Atomicity

A fork change now spans two PRs: the fork-repo PR and the compass lock-bump PR.
The split is safe by construction — compass's lock keeps pinning the previous
rev until the bump merges, so the failure mode is **staleness, not breakage**;
main is never red between the two. The vendored posture's converse property
(one PR, atomic fork+consumer change) is given up deliberately: a change that
needs both sides lands fork-first with defaults that keep the old consumer
behavior, then the compass bump adopts it.

Compass runs **no first-party Renovate** (verified: the only `renovate.json` in
the tree is upstream's inside `forks/devenv/.github/`, which no compass workflow
runs and which L2 deletes with the subtree; no `.renovaterc*` anywhere), so
routine bumps are manual `devenv update` PRs until someone wires automation;
this record does not require it.

## Alternatives considered

- **Compass-own fork repos** (`RigelBuild/compass-devenv` etc., or reviving the
  per-upstream Copybara spoke repos) — rejected. Matt ruled shared canonical
  repos: one fork per upstream, patch work combined. Two forks of the same
  upstream would duplicate the sealed patches and re-create the divergence this
  reversal exists to end. Frozen; not relitigated here.
- **Defer until the internal monorepo's reversal fully lands** — rejected. Only
  the nix2container lane has a genuine cross-repo dependency (the shared
  nix-DB-drop fix); serializing the whole reversal behind that completion keeps
  7424 vendored files (and
  their machinery) in compass longer for no correctness gain. Lanes here
  sequence on their actual prerequisites only.

## Plan

Each lane is its own PR series. Per-fork lanes delete their own subtree, moon
entry, and per-fork triggers atomically with the repoint (so no PR leaves a
tree that nothing consumes but everything still gates); the teardown lane
removes only the machinery shared across forks, and runs after the last tree is
gone.

Dependency order: L0a → L2; L0b → L1 (L0b confirms the shared nix2container fix
landed on `RigelBuild/nix2container`,
which **merged 2026-08-19** — the earlier RIG-2332 arm64-builder block is
resolved); L3 independent (behind OQ1's answer); L1+L2+L3 → L4. **Execution
order:** L1's external prerequisite is now met, so L1 can run at any point after
L0b's trivial confirmation. L2 / L3 / L4-of-the-non-nix2container-machinery
still run ahead of L1 by the original plan, but L1 is no longer gated on an
open-ended external block — it locks the real rev `8f4a6fd7` immediately.

### L0 — Cross-repo prerequisites (fork-repo side; no compass PR)

Contribute compass's devenv patch set into `RigelBuild/devenv`, and confirm the
nix2container shared fix has landed.

- **L0a — land the `containers.nix` patch in `RigelBuild/devenv`.** Port the
  full sealed diff from `forks/devenv/src/modules/containers.nix` (per-container
  `user`/`group`/`homeDir` options with upstream values as defaults,
  `containers.nix:373-389`; the `$HOME`-staging guard; the `imageEnv`
  DEVENV_-filter, `containers.nix:181`; the config-only `buildingContainer`
  lookup replacing `getEnv "DEVENV_CONTAINER"`, `containers.nix:76-81`) as a PR
  against `RigelBuild/devenv` `main` (plain upstream at `afed7bf3` as of this
  record). Carry the provenance rationale from `forks/README.md:85-117` into
  the PR description.
  - **Base-rev drift — this is a rebase, not a cherry-pick.** The vendored
    tree's upstream base is `8dc0eea` (`forks/devenv/.upstream-sync:1`) but
    `RigelBuild/devenv` `main` is at `afed7bf3` — a different upstream rev. If
    upstream's `containers.nix` or the nix backend moved between the two, the
    byte-identity-by-construction property (`forks/README.md:85-95`, anchored to
    upstream's exact bytes) and the `buildingContainer` single-source-of-truth
    premise (`containers.nix:76-81` +
    `devenv-nix-backend/bootstrap/bootstrapLib.nix:439-441`) must be
    **re-established against `afed7bf3`'s bytes**, not assumed from the vendored
    diff. The L0a PR carries a nix-eval check that the upstream-default container
    module resolves identical store paths pre/post-patch at the new base;
    upstream devenv CI does not exercise compass's `$HOME` property, so this
    check — not the fork repo's own CI — is what proves the port.
  - **Scaffolding comes from forge's base fork-repo convention, not a one-off.**
    `RigelBuild/devenv` is greenfield (no CI/release scaffolding). Compass's L0a
    adopts the base scaffolding forge's fork-repo design lands (base devenv
    shell / golangci / moon / Actions-release), so the two canonical repos share
    one convention. If L0a stands up before forge's design merges, it lands the
    patch with minimal CI and reconciles to the base afterward (cheap —
    greenfield). Compass owns the `containers.nix` patch + the compass repoint;
    forge owns the scaffolding convention.
  - Gate: the fork repo's own CI green **and** the byte-identity nix-eval check.
- **L0b — confirm the shared nix-DB-drop fix landed on `RigelBuild/nix2container`.**
  **Done (2026-08-19):** `RigelBuild/nix2container` `main` is at rev
  `8f4a6fd7b10abaeeddff6c4d8bb4908c5123c90c`, upstream `master` plus the one
  relocated-copyToRoot-paths patch (the shared nix-DB-drop fix). This is the rev
  L1 pins. No compass-side work.

Interfaces:

- Consumes: `forks/devenv/src/modules/containers.nix` (the sealed diff, source
  of truth for L0a); the shared nix-DB-drop fix on `RigelBuild/nix2container`.
- Produces: `RigelBuild/devenv` `main` rev carrying the patch;
  `RigelBuild/nix2container` `master` rev carrying the shared fix. These two
  revs are the pins L1/L2 lock.

### L1 — nix2container: repoint + delete (behind L0b; prerequisite met)

One compass PR: repoint every nix2container consumer to
`github:RigelBuild/nix2container` pinned at the post-T1 rev, delete
`forks/nix2container/` (66 files), and remove its per-fork machinery.

Interfaces:

- Consumes: `RigelBuild/nix2container` at the L0b rev
  `8f4a6fd7b10abaeeddff6c4d8bb4908c5123c90c` (fork `main`, carries the shared fix).
- Repoints (pin format `github:RigelBuild/nix2container`, rev locked in
  `agent-image/devenv.lock`):
  - `agent-image/devenv.yaml:20` — `url: path:../forks/nix2container` →
    `url: github:RigelBuild/nix2container` (keep the `nixpkgs` follows).
  - `agent-image/devenv.lock:215-222` — regenerate; the input's `locked` block
    gains a `rev`.
  - `.github/workflows/publish-agent-image.yml:139,160`,
    `agent-image/publish.sh:32`, and `tools/agent-image-env-gate/index.ts:118` — `nix run
    path:../forks/nix2container#skopeo-nix2container` → `nix run
    github:RigelBuild/nix2container/<rev>#skopeo-nix2container` (rev-pinned
    literal; these are raw CLI refs with no lockfile, so the rev rides in the
    URL and bumps are explicit edits).
- Deletes: `forks/nix2container/` (incl. `.upstream-sync`, `moon.yml`);
  `.moon/workspace.yml:79` (`nix2container-fork: 'forks/nix2container'`);
  `agent-image/moon.yml:70` input glob `/forks/nix2container/**`;
  `tools/agent-image-env-gate/moon.yml:59` input glob `/forks/nix2container/**`
  (the env-gate's inputs MIRROR agent-image's per `agent-image/moon.yml:65`; a
  dead glob left behind makes the fail-closed gate silently stop running on
  image PRs — the "silently inert registration" failure `.moon/workspace.yml:72-77`
  warns of);
  `.github/workflows/publish-agent-image.yml:57` path-filter trigger
  `forks/nix2container/**`.
- Prose/comment sweep: `agent-image/devenv.yaml:12-19` ("Tracks
  forks/nix2container"), `agent-image/publish.sh:5-16` cwd rationale,
  `forks/README.md` §nix2container is deleted with the tree in L4 (or here if
  L1 runs last).
- Gate: `moon run agent-image:build` (the `agent-image/moon.yml:44` task)
  green; publish workflow dry-runnable; the OQ2 pin shape applied (wrapper flake,
  or a lock-vs-URL rev-consistency assert if literals are kept).

### L2 — devenv: repoint + delete (behind L0a)

One compass PR: repoint every devenv consumer to `github:RigelBuild/devenv`
pinned at the post-L0a rev, delete `forks/devenv/` (1465 files), and remove its
per-fork machinery.

Interfaces:

- Consumes: the L0a rev of `RigelBuild/devenv`.
- Repoints (pin format `github:RigelBuild/devenv`, rev locked in
  `agent-image/devenv.lock`):
  - `agent-image/devenv.yaml:45` — `url: path:../forks/devenv` →
    `url: github:RigelBuild/devenv`.
  - `agent-image/devenv.lock:66-73` — regenerate with the pinned rev.
  - CLI invocations `nix run path:../forks/devenv#devenv` → `nix run
    github:RigelBuild/devenv/<rev>#devenv` (rev-pinned literal, same rationale
    as L1): `agent-image/moon.yml:44`, `agent-image/publish.sh:50-51`,
    `devenv.nix:461`, `.github/workflows/ci.yml:812`,
    `tools/agent-image-env-gate/index.ts:100`.
- Deletes: `forks/devenv/` (incl. `.upstream-sync`, `moon.yml`);
  `.moon/workspace.yml:78` (`devenv-fork: 'forks/devenv'`);
  `agent-image/moon.yml:69` input glob `/forks/devenv/**` and the
  symlink-hash-warn comment `moon.yml:57-64`;
  `tools/agent-image-env-gate/moon.yml:58` input glob `/forks/devenv/**` (the
  mirrored env-gate glob — same silently-inert risk as L1);
  `.github/workflows/publish-agent-image.yml:55` trigger `forks/devenv/**`;
  `.gitignore:64` `!forks/**/.DS_Store` (exists only for
  `forks/devenv/logos/`, `.gitignore:43-44`).
- Comment sweep (fork-path references in prose): `agent-image/devenv.nix:7`,
  `agent-image/toolchain.nix:96`, `agent-image/moon.yml:5,38` (the
  `nix run path:../forks/devenv#devenv` example and the `path:../forks/*`
  cwd-rationale comment, adjacent to the repointed CLI), `devenv.nix:110,443`,
  `tools/agent-image-env-gate/env-check.ts:8`, `index.ts:15`, and
  `moon.yml:10,41` (all cite `forks/devenv/...` or `path:../forks/*`),
  `apps/ui/.env.development:30-32` (cites
  `forks/devenv/src/modules/processes.nix`, `devenv-core/src/ports.rs`,
  `devenv/src/main.rs` for port-allocation behavior — will actively misdirect a
  debugger if left dangling), `.github/workflows/ci.yml:158` and the second
  fork-path block at `ci.yml:779-782`,
  `.github/workflows/eng-docs-deploy.yml:50`,
  `.github/workflows/publish-agent-image.yml:96,112` (the `accept-flake-config`
  warnings reference `forks/devenv/flake.nix`; reword per §Approach "Trust
  surface" — the flake is now fetched from `RigelBuild/devenv` at a pinned rev,
  not an in-repo file).
- Gate: `moon run agent-image:build` green; `dogfood:agent-image`
  (`devenv.nix:456-464`) loads the image; agent container smoke ($HOME =
  `/home/agent`, nix usable) — the exact property the patch protects; the OQ2
  pin shape applied consistently with L1.

### L3 — oh-my-pi: drop (or repoint, per OQ1)

Assuming OQ1 resolves to **drop** (recommended): one compass PR that repoints
the credential-denylist generator, then deletes `forks/oh-my-pi/` (5892 files)
and its machinery.

Interfaces:

- Consumes: OQ1's answer.
- Repoints (the one real consumer): `go/internal/store/gen_credential_keys.go:42`
  `schemaRelPath` → the npm-installed schema. The generator resolves the path
  from `runtime.Caller` relative to `go/internal/store/`
  (`gen_credential_keys.go:39-42,52-56`), and the SDK is **not** top-level
  hoisted (`agent-image/entrypoint.nix:208-218`), so the concrete target is
  `../../../packages/compass-agent/node_modules/@oh-my-pi/pi-coding-agent/src/config/settings-schema.ts`
  (pinned at `packages/compass-agent/package.json:19`); confirm `go generate
  ./internal/store/...` resolves it from the store's cwd before freezing the lane.
- The generated header is emitted from a HARDCODED literal, not from
  `schemaRelPath`: `gen_credential_keys.go:74`
  (`fmt.Fprintln(&b, "// Source: forks/oh-my-pi/.../config/settings-schema.ts …")`)
  is what produces `credential_keys_gen.go:3`. Edit `:74` (and the `:9` comment)
  or regeneration re-emits the deleted `forks/oh-my-pi/...` path; `:3` updates
  only as a CONSEQUENCE of editing `:74` then regenerating — regeneration alone
  is necessary but not sufficient. Also update the provenance at
  `agent_config.go:656-659` and the regeneration instruction in
  `agent_config_test.go:505-518`.
- **Expect a denylist diff, not byte-identity.** The vendored tree is `v17.1.8`
  (`forks/oh-my-pi/packages/coding-agent/package.json:4`) but the npm pin
  `^16.4.8` resolves to `16.5.2` in this checkout
  (`packages/compass-agent/node_modules/@oh-my-pi/pi-coding-agent/package.json`)
  — a full major behind. Across that gap the two schemas almost certainly
  differ, so `TestCredentialKeysMatchSchema` (`agent_config_test.go:507`) will
  likely red on the repoint and its `want` set (`agent_config_test.go:508-516`)
  must be re-reviewed and updated in the SAME PR. This is the correct outcome —
  the repoint re-bases the denylist onto the version production actually runs,
  which the Approach argues for; a diff is the expected case, not drift to
  paper over.
- Deletes: `forks/oh-my-pi/` (incl. `moon.yml`; this tree has no
  `.upstream-sync` — only devenv and nix2container carry one);
  `.moon/workspace.yml:80-83` (the `oh-my-pi-fork` entry and its comment).
- Prose sweep: `forks/README.md` §oh-my-pi (deleted with the README in L4);
  `go/e2e/cannedmodel.go:14` ("Grounded against the SDK parser firsthand
  (forks/oh-my-pi)"); `go/internal/runner/config_delivery_e2e_test.go:78-80`
  (oh-my-pi startup-marker provenance, no path);
  `docs/designs/platform/compass-drop-proto.md:310,423,469` reference
  `forks/oh-my-pi/moon.yml` — merged records are frozen history and are NOT
  edited; new docs must not cite the tree.
- If OQ1 resolves to **consume** instead: the lane becomes "pin
  `github:RigelBuild/oh-my-pi` for the generator (and any future consumer)" and
  fetches the schema at the pinned rev; the deletion set above still applies.
- Gate: `moon query projects` no longer lists `oh-my-pi-fork`;
  `go generate ./internal/store/...` runs clean against the new source;
  `go test ./internal/store/ -run TestCredentialKeysMatchSchema` green; repo CI
  green.

### L4 — shared-machinery teardown (behind L1+L2+L3)

One compass PR removing everything that existed only because `forks/` did.

Interfaces:

- Consumes: an empty `forks/` (all three trees gone).
- Deletes/edits:
  - `.moon/workspace.yml:64-77` — delete the shared fork-projects comment block
    (`Vendored upstream fork subtrees …` through the `VENDORING A NEW FORK`
    note); it explains only the three `-fork` entries and dangles once L1/L2/L3
    have removed all of them.
  - `.markdownlint-cli2.jsonc:5-6,23` — remove `"forks/*/**"` from `ignores`
    (:23) and its exclusion-rationale comment (:5-6).
  - `biome.json:5` — remove `!forks/*` from `files.includes`.
  - `.gitignore:41-63` — remove the vendored-fork re-include comment block
    (the `!forks/**/.DS_Store` line itself goes in L2 if not already gone).
  - `.github/secret_scanning.yml` — delete the file (its only entry is
    `paths-ignore: ["forks/*/**"]`, line 25). **This re-widens secret scanning
    and push protection to the whole repo** — flag in the PR description;
    expect no findings since the exempted trees are gone.
  - `forks/README.md` — delete; `README.md:56` — remove the `forks/` layout
    line.
  - `apps/eng-docs/scripts/gather.ts:56` — remove `"forks/README.md"` from
    `CONTRIBUTING_FILES`.
  - Docs sweep: `docs/architecture/build-and-ci.md:199` (fork-path build
    invocation → pinned-input invocation); `CONTRIBUTING.md:40` ("Vendored
    forks: each fork's own nix build" gate row). Merged design records under
    `docs/designs/` that cite `forks/*` paths are frozen history — not edited.
- Test updates (contract changed, tests follow):
  - `apps/eng-docs/scripts/deploy.test.ts:217,283-328` — the fixture
    `ignores: ["forks/*/**"]` and the fork-exclusion cases update to a
    remaining exclusion (e.g. `config/prompts/**`).
  - `apps/eng-docs/scripts/gather.test.ts:191-205,269-322` — the
    `forks/README.md` slug-collision test drops with the file; glob tests
    re-target remaining ignores.
- Not applicable in compass (verified): there is no `tools/fork-sync/` project
  to remove — `forks/README.md:9-11` says sync machinery is "carried over as
  provenance … not a process that runs here", and no such tree exists in this
  checkout.
- Gate: `bun test apps/eng-docs/scripts`; `biome check` + markdownlint over the
  now-unexempted tree set (trivially — the trees are gone); repo CI green.

## Tasks

- [ ] L0a — land compass's `containers.nix` patch set in `RigelBuild/devenv`
      (fork-repo PR; provenance carried; fork CI green).
- [x] L0b — shared nix-DB-drop fix **landed 2026-08-19**; `RigelBuild/nix2container`
      `main` at `8f4a6fd7` carries the shared nix-DB fix (the rev L1 pins).
- [ ] L1 — repoint nix2container consumers to pinned
      `github:RigelBuild/nix2container`; delete `forks/nix2container/` + its
      moon entry, input glob (incl. the mirrored env-gate glob), workflow
      trigger, and the env-gate CLI ref.
- [ ] L2 — repoint devenv consumers to pinned `github:RigelBuild/devenv`;
      delete `forks/devenv/` + its moon entry, input glob (incl. the mirrored
      env-gate glob), workflow trigger, `.DS_Store` negation, and the env-gate
      CLI ref; agent-image smoke green.
- [ ] L3 — resolve OQ1, then (drop path) repoint the credential-keys generator
      (`gen_credential_keys.go:42`) to the npm schema, regenerate, and delete
      `forks/oh-my-pi/` + its moon entry — or pin `github:RigelBuild/oh-my-pi`
      if Matt rules consume.
- [ ] L4 — tear down shared machinery: style carve-outs, `.gitignore` block,
      `secret_scanning.yml`, `forks/README.md`, `README.md:56`, docs sweep,
      eng-docs test updates.

## Open Questions

1. **oh-my-pi: consume `github:RigelBuild/oh-my-pi`, or drop the subtree and
   repoint the one tooling consumer?** — **LOAD-BEARING** (L3 either pins the
   shared repo or deletes-and-repoints depending on the answer). Recommendation:
   **drop + repoint the generator to the npm schema.** There is no *build*
   consumer; the only consumer is the credential-denylist generator
   (`go/internal/store/gen_credential_keys.go:42`), which should read the
   npm-installed schema the agent actually runs
   (`packages/compass-agent/package.json:19`) rather than a vendored snapshot —
   so dropping the tree is clean, removes 5892 files, and leaves the denylist
   tracking production. Consume-now (pin the shared repo for the generator alone)
   is heavier for no runtime gain and only makes sense if a *build* consumer is
   imminent and named.
2. **Rev-pinned CLI literals vs one lockfile for the raw `nix run` invocations**
   (`agent-image/publish.sh:32,50-51`, `agent-image/moon.yml:44`,
   `devenv.nix:461`, `ci.yml:812`, `publish-agent-image.yml:139,160`,
   `tools/agent-image-env-gate/index.ts:100,118`) —
   **LOAD-BEARING, but narrower than first framed.** The *flake-input* half is
   settled: the internal monorepo's prior art froze the nix-flake-input class as
   `github:RigelBuild/<fork>` pinned **via lockfile** (the deliberate default
   plus a whole-repo narHash fix), so compass's one flake-input consumer,
   `agent-image/devenv.lock`, converges on that with no reason to diverge. What
   that precedent does **not** cover is the wrinkle: its frozen class table has
   four import classes — flake-input, standalone-executable-as-a-file,
   GHCR-image, and no-consumer — but **none is a raw-CLI bypass**, whereas
   compass invokes the fork CLI raw at the six sites above, which bypass
   `devenv.lock` entirely.
   Today a single `path:` tree makes the CLI rev and the locked
   module-set rev identical by construction (`devenv.nix:442-445` names this as
   the reason for the pin shape; the frozen dogfood-loop record makes the same
   argument, `docs/designs/platform/compass-dogfood-loop/design.md:225-229`).
   Scattering a `github:…/<rev>` literal across those six lockfile-bypass sites
   reintroduces a silent divergence: a bump editing the lock but not a literal
   (or vice versa) builds the image with the module set at one rev and the CLI at
   another, with no gate to catch it. **Recommendation: a tiny compass-side flake
   re-exporting the locked inputs**, so the raw CLIs resolve the same lockfile rev
   as the module set and the by-construction identity is restored — this option
   has no prior-art analog. The coherent alternative is the literal style — keep
   the `github:…/<rev>` literals but have L1/L2 add a CI assert that each
   literal's rev equals the corresponding `devenv.lock` rev, named in the lane
   gates; this mirrors the internal monorepo's terraform-provider class, a pinned
   **non-flake** consumption (tagged GitHub Release + committed sha256 manifest,
   verified at build time) — the prior-art shape for "pin and verify a path not
   resolved through a lockfile." This is the record's one genuine fork — Matt's
   call; note at review that the prior art has no raw-CLI-bypass class, so its
   flake-input precedent settles compass's `devenv.lock` consumer but not the
   raw-CLI shape.
3. **Secret-scanning re-widening fallout** — non-load-bearing. Deleting
   `.github/secret_scanning.yml:25` re-enables scanning + push protection
   repo-wide. Expected clean once the trees are gone; if historical alerts
   reopen on deleted paths, they are closed as historical, not redacted.
