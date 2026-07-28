# forks/

> **Vendored into this repo (SEA-1512).** These two trees — `devenv/` and
> `nix2container/` — were copied byte-identically out of the sealed monorepo's
> `oss/forks/` at `origin/main`, per Matt's ruling that Compass carries them as
> its own trees rather than consuming them from a public spoke. They live at
> `forks/` (repo root), not `oss/forks/`, because the agent image's devenv
> inputs pin them by a path relative to `agent-image/`. The monorepo remains the
> other home for both; the sections below describing Copybara spokes, the
> monorepo import runbook, and sync policy are carried over as provenance and
> describe the monorepo-side machinery, not a process that runs here.

Vendored upstream forks. Each `forks/<name>/` subtree is the tree of a fork
sealed maintains on top of a public upstream — the code the fleet actually runs
— collapsed out of its own GitHub repo into one repo, one `main`, one review
flow, one CI, atomic with the changes that consume it.

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

Branch name is data, never hardcoded: `nix2container` defaults to `master`,
`devenv` to `main`.

## Provenance

Per-fork customization, consumer, and sync policy.

### devenv

- **Upstream:** `cachix/devenv` (`main`). **Spoke:** `sealedsecurity/devenv`.
- **Sealed changes:** the container module's baked identity (passwd row,
  `$HOME`, image `USER`/`HOME`) is per-container rather than upstream's
  hardcoded `user` + `/env`; upstream's values are kept as DEFAULTS, so every
  other consumer is byte-identical.
- **Consumer:** the Compass agent image's `devenv.yaml` `devenv` input, pointed
  at this subtree by relative path.
- **Sync policy:** Copybara inbound from `cachix/devenv` (monorepo side);
  outbound only if an upstreamable diff ever accumulates.

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

## nix-source forks build through the flake, not `nix-build -A`

Both forks are nix-source class, so their functional-CI task is a nix build —
`nix` is on the PATH wherever this runs, no per-fork language toolchain needed
(devenv's Rust compiles via its flake's fenix pin; nix2container's Go via
`buildGoModule` inside the nix build, which also runs Go's own checks). **Use
the flake build (`nix build .#<attr>`), never `nix-build -A`.**
nix2container's `default.nix` pins its source with
`lib.fileset.gitTracked ./.`, which requires a git-repo
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
