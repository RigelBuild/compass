# forks/

> **Vendored into this repo (SEA-1512).** Two of these trees — `devenv/` and
> `nix2container/` — were copied byte-identically out of the sealed monorepo's
> `oss/forks/` at `origin/main`, per Matt's ruling that Compass carries them as
> its own trees rather than consuming them from a public spoke. They live at
> `forks/` (repo root), not `oss/forks/`, because the agent image's devenv
> inputs pin them by a path relative to `agent-image/`. The monorepo remains the
> other home for both; the sections below describing Copybara spokes, the
> monorepo import runbook, and sync policy are carried over as provenance and
> describe the monorepo-side machinery, not a process that runs here.
> `oh-my-pi/` arrived later and by a different route (SEA-1514): imported
> straight from public upstream at a tag, never via the monorepo.

Vendored upstream forks. A `forks/<name>/` subtree is upstream code carried in
this repo — usually a fork sealed maintains on top of a public upstream (the
code the fleet actually runs), collapsed out of its own GitHub repo into one
repo, one `main`, one review flow, one CI, atomic with the changes that consume
it. A subtree may also be plain upstream with no sealed delta at all
(`oh-my-pi`); the Provenance section below is authoritative per fork.

The public fork repos (`sealedsecurity/<name>`) stay alive, demoted from
canonical home to **Copybara spoke**: the GitHub-side staging ground upstream
PRs are cut from. Nothing in the fleet builds from a spoke anymore — every
consumer repoints to the vendored subtree at its fork's import.

Design record (the contract this tree executes) lives in the sealed monorepo
(`sealedsecurity/sealed`), not here:
`docs/designs/platform/oss-fork-consolidation.md` in that repo.

## What a fork subtree is

A fork subtree is **vendored upstream code**, not a first-party project. That
distinction drives every rule here:

- **Style-gate-exempt.** Fork subtrees face upstream's own style, not this
  repo's. A locally-formatted fork can never round-trip a diff to its upstream
  without reformat noise upstream maintainers reject, and every upstream pull
  would re-conflict against the reformat. So the vendored trees are excluded
  from both style gates — but each dialect spells it differently:
  **markdownlint** (`.markdownlint-cli2.jsonc`) ignores `forks/*/**`, where the
  `*/**` is load-bearing: it exempts the fork trees while keeping the
  first-party `forks/README.md` (this file) linted. **biome** (`biome.json`)
  carries `!forks/*` in `files.includes` — biome's folder-ignore form since
  2.2.0, where a trailing `/**` trips its own `useBiomeIgnoreFolder` rule.
  Biome does not process markdown at all, so this file's linting does not
  depend on the biome glob.
- **Functionally tested.** Style exemption is **not** test exemption. Each fork
  carries its own `forks/<name>/moon.yml` registering its native build/test/lint
  tasks (upstream's own toolchain) as moon tasks with `inputs` scoped to
  `forks/<name>/**` and `options.runInCI: true`, **and** a matching entry in the
  `.moon/workspace.yml` `projects:` map — moon discovers projects from that
  explicit list, not by globbing for `moon.yml`, so the registration is what
  makes the fork a moon project at all.
- **Self-contained, out of the root workspace.** Fork trees keep their own
  lockfiles and package managers. `forks/` carries no `package.json`, so nothing
  under it joins the root bun workspace or the version catalog.

Directory names are lowercase.

## Per-fork inventory

| Fork | Upstream | Default branch | Spoke | Release-artifact class |
| --- | --- | --- | --- | --- |
| `devenv` | `cachix/devenv` | `main` | `sealedsecurity/devenv` | nix-source (flake) |
| `nix2container` | `nlewo/nix2container` | `master` | `sealedsecurity/nix2container` | nix-source (flake input) |
| `oh-my-pi` | `can1357/oh-my-pi` | `main` | — (no spoke) | bun/TypeScript + Rust source |

Branch name is data, never hardcoded: `nix2container` defaults to `master`,
`devenv` to `main`.

## Provenance

Per-fork customization, consumer, and sync policy.

### devenv

- **Upstream:** `cachix/devenv` (`main`). **Spoke:** `sealedsecurity/devenv`.
- **Sealed changes:** one patch set, all in `src/modules/containers.nix`, added
  for the Compass agent base image:
  - Per-container `user` / `group` / `homeDir` options. Upstream hardcodes user
    `user` with `$HOME=/env` module-wide; these move the passwd/group/shadow
    rows, the file ownership `perms`, and the image config's `User`/`HOME`/`USER`
    together per container. **Upstream's values are kept as the defaults**, so a
    consumer that sets none of them resolves to a byte-identical `mkEtc` store
    path and image config. That identity is asserted by construction, not by a
    build-and-diff: the parameterized script bodies keep upstream's exact bytes
    (including line breaking — a comment inside `runCommand`'s text is part of
    the build command and shifts the derivation hash on its own).
  - Conditional `$HOME` staging plus its `perms` entry, guarded on the home
    actually moving off the `/env` default. nix, direnv and devenv all write into
    `$HOME`, and an absent or root-owned home makes nix fall back with "$HOME is
    not owned by you"; the guard keeps the default path byte-identical to
    upstream, which never created this directory.
  - The `DEVENV_`-prefix `imageEnv` filter. `top-level.nix` merges
    `DEVENV_PROFILE`/`STATE`/`RUNTIME`/`DOTFILE`/`ROOT` into `config.env`
    unconditionally; those are build-host coordinates that do not exist inside
    the image, and a baked `DEVENV_ROOT` is precisely the sentinel devenv's shell
    hook reads to conclude a shell is already active — so an image carrying it
    silently refuses to activate. Filtering at serialization is the only place a
    project can suppress them.
  - Removal of `envContainerName = builtins.getEnv "DEVENV_CONTAINER"` and the
    two config blocks it drove (`container.isBuilding` and
    `containers.<name>.isBuilding`), replaced by a config-only `buildingContainer`
    lookup. The env path is dead: the nix backend already forces both
    (`devenv-nix-backend/bootstrap/bootstrapLib.nix:440-441`, `lib.mkForce true`),
    and nothing in the tree writes `DEVENV_CONTAINER`. Reading identity out of an
    impure `getEnv` while the options carry it is two sources of truth; the
    lookup keeps one. Listed because `fork-sync` 3-way-merges with `ours` = the
    current subtree: the change survives the merge either way, but an undeclared
    one is indistinguishable from upstream drift at a conflict marker.

  Plus the `forks/devenv/moon.yml` functional-CI registration (absent upstream),
  a declared export-exclusion excluded from the import byte-fidelity diff.
- **Base ref & rebase policy:** the functional diff is confined to
  `src/modules/containers.nix`; everything else tracks upstream `main` so rebases
  stay cheap. Do not accumulate local changes outside that one module.
- **Consumer:** the Compass agent image's `devenv.yaml` `devenv` input, pointed
  at this subtree by relative path — the base image is built against this fork's
  module tree, not the installed CLI's bundled modules.
- **Sync policy:** Copybara inbound from `cachix/devenv` (monorepo side). The
  subtree carries local patches, so an inbound sync 3-way-merges upstream over
  them: preserve the three `containers.nix` changes above when resolving
  conflicts. Outbound only if the diff becomes upstreamable (the per-container
  identity options plausibly are; the `imageEnv` filter is shaped for this
  repo's use).

### nix2container

- **Upstream:** `nlewo/nix2container` (`master`). **Spoke:**
  `sealedsecurity/nix2container`.
- **Sealed changes:** one patch — drops relocated paths from the initialized nix
  DB so a container's in-image nix DB does not claim store paths the image does
  not carry (which breaks the image self-rebuild with a failed lstat).
- **Consumer:** the Compass agent image's `devenv.yaml` `nix2container` input,
  pointed at this subtree by relative path.
- **Sync policy:** Copybara inbound from `nlewo/nix2container`; outbound to the
  spoke (the fix is upstreamable — track back to `nlewo/nix2container` if
  merged).

### oh-my-pi

- **Upstream:** `can1357/oh-my-pi` (`main`), imported at tag **`v17.1.8`**.
  **Spoke:** none — this fork has no `sealedsecurity/oh-my-pi` staging repo.
- **Sealed changes: NONE.** This tree is **plain public upstream**, verified
  byte-identical to `can1357/oh-my-pi` at `v17.1.8` (5891 files, compared
  git-natively — blob OIDs *and* file modes — via `git ls-files -s` against
  `git ls-tree -r` of the tag). The **only** exception is the sealed-added
  `forks/oh-my-pi/moon.yml`, the functional-CI registration every fork carries.
- **Deliberately NOT present — read this before importing or "restoring"
  anything.** The sealed monorepo carries its *own* fork of oh-my-pi (at
  17.0.3), and that fork has a real sealed delta this tree does not:
  a Prometheus `/metrics` exposition in the auth-broker; a tool-call-id pairing
  fix; a `refresh`/`restart` tool surface; two `biome.json` compat edits; and a
  nix build surface (`flake.nix`, `flake.lock`, `nix/`, root `bun.nix`). Per
  Matt's ruling on SEA-1514, Compass ships **plain upstream first** and re-adds
  what it actually needs later, as a separate change. So their absence here is
  a decision, not an import defect — **do not** treat this tree as evidence
  that those features never existed, and do not conclude from a diff against
  the monorepo's fork that work was lost. It lives on, in the monorepo.
- **Consumer:** none yet — the tree is vendored ahead of the consumers that
  will build on it.
- **Sync policy:** re-import from public upstream at a tag, as a plain-copy
  squash. No Copybara, inbound or outbound, while the sealed delta is empty.
- **Gating caveat:** unlike the two nix-source forks, this one's functional-CI
  task covers its **TypeScript surface only** (upstream's own
  `bun run ci:check:full`). Upstream's ~415-file Rust tree under `crates/` is
  gated upstream by bazel against a rust nightly pin, and this repo's dev shell
  has neither bazel nor a Rust toolchain — so that half is ungated here.
  `forks/oh-my-pi/moon.yml` says so at the point of registration.

## nix-source forks build through the flake, not `nix-build -A`

`devenv` and `nix2container` are nix-source class, so their functional-CI task
is a nix build — `nix` is on CI's PATH, no per-fork language toolchain needed
(devenv's Rust compiles via its flake's fenix pin; nix2container's Go via
`buildGoModule` inside the nix build, which also runs Go's own checks).
`oh-my-pi` is not in this class: it ships no flake and gates through bun
instead. **Use the flake build
(`nix build .#<attr>`), never `nix-build -A`.** nix2container's `default.nix`
pins its source with `lib.fileset.gitTracked ./.`, which requires a git-repo
root — satisfied only by the store-copied flake tree, not the in-repo
subdirectory; `nix-build -A` fails there with "not a local working tree of a
Git repository." The flake path is also exactly how a downstream consumer
resolves the fork.

## Bot findings on vendored code

Review bots (Greptile, CodeRabbit, …) will flag lines inside the vendored tree.
**Never edit vendored files to satisfy a bot.** The `forks/<name>/` tree is a
byte-for-byte copy of its upstream, and byte-identity to the source is the
verification basis that stands in for a line-by-line review of a tree too large
to read. An edit to vendored code breaks that identity and poisons the Copybara
round-trip. So for a finding on a vendored line: decline it (reply that the tree
is vendored and edits route upstream), resolve the thread, and — only if the
finding is a genuine upstream bug worth carrying — open it against the public
upstream. The only review-worthy surface on an import PR is the **first-party
additions** (`moon.yml`, `.moon/workspace.yml`, any consumer repoint); review
those normally.

## Gotcha: fork dev-shell git hooks

Entering a fork's own dev shell (e.g. `direnv exec` in a context that loads the
fork's `.envrc`, or running its tooling) can install a pre-commit hook pointing
at the fork's config. It is clone-local (never committed) and harmless, but
remove it **only after confirming it is the fork's hook, not a later root
hook** — a single hooks path holds one `pre-commit`, so an unconditional `rm`
would silently disable the repo's own pre-commit checks if one was installed
since. Resolve the **active** hooks dir first, don't assume `.git/hooks`: `git
rev-parse --git-path hooks` honors `core.hooksPath` and, in a linked worktree,
resolves to the common dir. Check that the installed hook references the subtree
(`grep -l forks "$(git rev-parse --git-path hooks)/pre-commit"`), then remove it
there, and reinstall the root hook if the repo has one.

## Escape hatch: a fork that stops tracking upstream

A fork whose upstream relationship ends (upstream archived, or the divergence
becomes permanent) can opt into full gating, take the one-time reformat, and
gain first-party style gating. It then needs no inbound Copybara.

Opt one fork in without touching the others: both exclusions are wildcards over
every fork, so do **not** simply delete them. Replace each with the explicit
per-fork entries, omitting the graduating fork — in
`.markdownlint-cli2.jsonc`, replace `forks/*/**` with one `forks/<name>/**` per
still-vendored fork; in `biome.json`, replace `!forks/*` with one `!forks/<name>`
per still-vendored fork. That expansion also narrows the biome exclusion to the
fork directories themselves rather than every direct child of `forks/`, which is
what is intended. Deleting the wildcards outright would de-exempt every vendored
tree at once and reformat them all.

This is the per-fork opt-in that keeps the vendored-by-default posture from
trapping a fork that has become de-facto sealed-first.
