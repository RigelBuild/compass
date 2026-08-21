# Compass: migrate Dependabot → self-hosted Renovate in GitHub Actions

Status: Draft

## Problem / Intent

Compass runs GitHub Dependabot for its three ecosystems (`.github/dependabot.yml`:
github-actions at `/`, bun at `/`, gomod at `/go`, all weekly single-group). Matt
wants Dependabot off — it carries hidden GitHub-billed features, and the fleet
should run ONE dependency manager, not two. Orion already runs self-hosted
Renovate (SEA-847), proven through the catalog, devenv-nixpkgs, and toolchain-pin
lockstep machinery. Migrate compass onto the same Renovate, adapted to compass's
layout — with the hard constraint that it runs in **GitHub Actions** (compass has
no Woodpecker; all its CI is GHA). The repo is pre-prepped: the design-ledger
gate already exempts `renovate/` branches (`tools/design-ledger-gate/index.ts:87`,
`EXEMPT_BRANCH_PREFIXES = ["renovate/"]`, comment "prepared for future Renovate").
Note "Dependabot off" has TWO halves: deleting `.github/dependabot.yml` stops
VERSION updates only; Dependabot security updates/alerts — the hidden
GitHub-billed feature driving this migration — is a repo Settings toggle the
yml deletion does not touch. The plan disables it explicitly (T8's runbook),
with `osvVulnerabilityAlerts: true` replacing the coverage.

## Global Constraints

- **NEVER `vulnerabilityAlerts: { enabled: true }`** — a vuln fix injects a
  packageRule with `force.enabled` truthy, which clears `skipReason` and CANCELS
  the fork fence (orion `ci/renovate/config.json5:42-47`). Use
  `osvVulnerabilityAlerts: true` only. `config.test.ts` must guard both facts,
  as orion's does.
- **`minimumReleaseAge: "5 days"` + `internalChecksFilter: "strict"`** —
  consistent with compass's `bunfig.toml:6` `minimumReleaseAge = 432000` (5
  days). Mirror bunfig's exact-name exemptions (`bunfig.toml:20-24`:
  `@tanstack/virtual-core`, `@types/bun`, `bun-types`) so Renovate and bun
  install agree on what soaks — implemented by a catalog-scoped packageRule
  (`matchPackageNames: ["@types/bun", "bun-types"], minimumReleaseAge: null`,
  see packageRules) paired to bunfig's list by a `config.test.ts` guard;
  `@tanstack/virtual-core` needs no rule (an `overrides` pin, outside the
  catalog manager's reach).
- **Fork trees disabled** — compass vendors `forks/{devenv,nix2container,oh-my-pi}`
  (root `forks/`, not orion's `oss/forks/`). A packageRule
  `matchFileNames: ["forks/*/**"], enabled: false` (a scoped disable, never
  `ignorePaths`, which replaces Renovate's safe defaults — orion
  `config.json5:494-504`).
- **Toolchain pins auto-open solo branches** — Matt's standing ruling: every
  toolchain bump (bun/node/moon/go) opens its own un-grouped PR (orion
  `config.json5:304-326`).
- **Every postUpgradeTasks command in bot-config `allowedCommands`,
  `^…$`-anchored** — a repo config can't self-authorize a command; `config.test.ts`
  pins the two lists together (orion `bot-config.json5:48-79`).
- **TypeScript `<7` cap (SEA-1867)** — compass's catalog pins
  `"typescript": "^6.0.3"` (`package.json:21`), so the Project Corsa cap applies:
  TS 7.0 ships no stable programmatic API (orion `config.json5:327-343`).
- **Timezone/schedule alignment (SEA-1220)** — `timezone: "America/New_York"` in
  the repo config, and the GHA cron (UTC) must land inside the `schedule:daily`
  before-4am-ET window WITH margin: GHA scheduled runs are best-effort and
  routinely start 5-30+ minutes late, so the cron must not sit near the window
  edge or a delayed start silently opens zero PRs forever. `0 6 * * *` UTC =
  02:00 EDT / 01:00 EST — 2-3h of margin in both DST phases.
- **`@tanstack/virtual-core` stays pinned at 3.17.5** — a reviewed,
  record-mandated floor via root `overrides` (`package.json:24-26`,
  `bunfig.toml:7-12`), not a float Renovate may bump. (The catalog regex manager
  scopes to the `"catalog": {…}` block only, so the `overrides` block is
  structurally out of reach — no extra rule needed; `config.test.ts` should pin
  this.)
- **`RENOVATE_X_IGNORE_RE2=true`** on the runner — `bunx renovate` installs no
  native re2 addon; take the RegExp fallback deliberately (orion
  `ci/workflows/meta.ts:154-160`).
- **Writable HOME for postUpgradeTasks** — `customEnvVariables: { HOME: … }` in
  bot-config (RIG-2245: `devenv update nixpkgs` panics on an unwritable
  `$HOME/.local/share/devenv`; orion `bot-config.json5:31-46`). GHA runners have
  a writable `$HOME` natively, but keep the declaration versioned and testable.
- **A new `tools/*` test package is inert until registered in
  `.moon/workspace.yml`** — moon discovers projects ONLY from the explicit map
  there (`.moon/workspace.yml:10`); an unregistered moon.yml "is silently inert
  — moon never discovers it, so [it] ships a functional-CI registration that
  gates nothing while reading as covered, which is worse than no gate"
  (`.moon/workspace.yml:77-83`). T5/T6 register `tools/renovate` and
  `tools/renovate-preflight` and verify with `moon query projects`.

## Approach

Port orion's proven self-hosted Renovate (repo config + bot config + lockstep
scripts + config tests) into compass, adapted to compass's paths and ecosystems,
and run it as a plain GitHub Actions workflow that provisions the language
toolchains the same way compass's other CI jobs do, plus the `devenv` CLI
built from the vendored fork via a PATH shim (compass CI provides no PATH
devenv — see Runner shape). Delete `.github/dependabot.yml` in the same PR —
one manager, no overlap window.

### Runner shape: a plain GHA job (option B)

The workflow (`.github/workflows/renovate.yml`) is: `actions/checkout` →
`cachix/install-nix-action` → put the language toolchains on PATH via
`tools/toolchain/gate-tools.nix` (the exact two-phase bootstrap compass's `ci.yml`
already uses at lines 144-212: `nix eval -f tools/toolchain/gate-tools.nix langs`,
`nix build`, append `$out/bin` to `$GITHUB_PATH`) → build the VENDORED devenv
fork and shim it onto PATH (`nix build path:forks/devenv#devenv`, symlink
`$out/bin/devenv` into a dir prepended to `$GITHUB_PATH` — see T6) →
`RENOVATE_CONFIG_FILE=<bot-config> RENOVATE_X_IGNORE_RE2=true bunx
renovate@44.33.1` (exact pin — see below).

Why B: compass's postUpgradeTasks need `nix` (toolchain-hash prefetch), `devenv`
(devenv-nixpkgs relock shells `devenv update nixpkgs` from PATH — orion
`refresh-devenv-nixpkgs.ts:18-21,129`), and `bun` (all three scripts +
`bun install --lockfile-only`). Compass's GHA CI already provisions the
language toolchains per job via `cachix/install-nix-action@630ae543…`
(`.github/workflows/ci.yml:150`) + gate-tools.nix — the Renovate job composes
with that idiom instead of inventing a second toolchain path. `devenv` is the
one binary that idiom does NOT provide: compass CI never puts a `devenv` on
PATH — its only devenv invocations run the vendored fork's CLI by path
(`ci.yml:812` `nix run path:../forks/devenv#devenv -- container copy agent`;
`agent-image/moon.yml:44`; `devenv.nix:469`). The Renovate job therefore
builds that same fork (the flake exports the CLI as
`packages.<system>.devenv`, `forks/devenv/flake.nix:113-115`) and shims it
onto PATH. This is FORCED by the frozen fork posture, not a fresh choice: the
image pipeline pins to "the vendored fork's own CLI … so it cannot diverge
from the fork source" (`devenv.nix:450-453`;
`docs/designs/platform/compass-forks-reversal/design.md:125-134` — "The
fork's own CLI is invoked by path everywhere the image is built"). A nixpkgs
devenv doing the relock would be a SECOND, divergent devenv — the exact thing
the fork posture eliminates (see Alternatives §D). Nix's store caching plus
the declared substituters (`ci.yml:165-168`) keep setup cost bounded.
Alternatives A and C lose (see Alternatives considered).

**Renovate itself is pinned.** Bare `bunx renovate` resolves LATEST from npm
on every scheduled run — fresh registry code executing with a repo:write
token, an unpinned dependency whose job is managing pinned dependencies,
bypassing every soak defense this record mandates (`minimumReleaseAge` and
bunfig's cooldown govern `bun install`, not `bunx`). The GitHub App auth
(see Secrets) bounds this exposure: the token is minted per run and expires
~1h later — an ephemeral credential rather than a long-lived stored PAT —
but a per-run token doesn't excuse running unpinned third-party code; the
pin stands.
The workflow runs `bunx renovate@44.33.1` (npm's latest stable at design time;
the initial pin). The pin line is itself a managed dependency: a `custom.regex`
manager on `.github/workflows/renovate.yml` (datasource `npm`, depName
`renovate`) bumps it through a reviewable PR under the normal soak, and a
`config.test.ts` guard asserts the workflow pins an exact version (no bare
`bunx renovate`). Note orion has the same exposure — its meta job runs bare
`bunx renovate` (`ci/workflows/meta.ts:161`; the publish image bakes
devenv/skopeo, NOT Renovate) — fix it there as a fleet follow-up, out of scope
here.

Triggers: `on: schedule: - cron: "0 6 * * *"` (06:00 UTC = 02:00 EDT / 01:00
EST — inside the before-4am-ET `schedule:daily` window with 2-3h margin per
the SEA-1220 constraint; GHA cron is best-effort and routinely 5-30+ minutes
late, so a tighter cron like `0 7` — 60 min of EDT margin — risks a delayed
start past 04:00 ET reproducing the SEA-1220 silent-zero-PR symptom) +
`workflow_dispatch` for manual runs (the GHA analogue of orion's Woodpecker
`{event: manual}` trigger, `ci/workflows/meta.ts:136`; it also revives the
schedule if GHA auto-disables it after 60 days of repo inactivity — see T6).
Cadence: daily (resolved decision, OQ5 — Matt 2026-08-21), dropping
dependabot's weekly.

Secrets/Auth: a **GitHub App** (resolved decision, OQ2 — see Resolved
decisions). The
workflow mints a per-run installation token with
`actions/create-github-app-token@<pinned-sha> # vX` (SHA-pinned with a
version comment per the repo invariant, `dependabot.yml:1-8`), reading the
App's client-id from the repo variable `RENOVATE_APP_CLIENT_ID` and its
private key from the ONE stored secret, `secrets.RENOVATE_APP_PRIVATE_KEY`.
The minted token is exported as `RENOVATE_TOKEN` for `bunx renovate` and as
`GH_TOKEN` (with `REPO`) for the preflight. There is NO long-lived PAT — no
`secrets.RENOVATE_TOKEN` exists; the token is minted fresh each run and
expires ~1h later. **Workflows permission (LOAD-BEARING):** the
github-actions manager (see Managers) edits files under `.github/workflows/`,
and a GitHub App can only push workflow-file changes if it holds the
**Workflows** repository permission (GitHub Docs: "if your app specifically
needs to access or edit Actions files in the .github/workflows directory,
request the Workflows repository permission") — without it every
github-actions bump PR fails to push with a workflows-scope error. Full App
permission set: Contents (read/write — git access + non-workflow commits),
Pull requests (read/write), Workflows (read/write), Issues (read/write — the
dependency dashboard is an issue). Orion's second secret
`RENOVATE_GITHUB_COM_TOKEN` is a read-only github.com PAT for release-notes
lookups against github.com from a non-github.com platform host; compass IS on
github.com, so the App token covers it — do not port the second secret.
Registering/installing the App is a human action (T8;
skill://human-action-handoff).

Port orion's `tools/renovate-preflight` probe (orion `ci/workflows/meta.ts:128-152`)
so an expired/unscoped token fails with a named diagnosis instead of Renovate's
opaque `platform-unknown-error`. The ported preflight reads `REPO` (owner/name)
from the environment and exits fail-closed (exit 2) when it is missing (orion
`tools/renovate-preflight/index.ts:9,15` — "REPO - owner/name (from CI_REPO)";
"2 - could not evaluate (missing REPO env) — fail closed"); GHA has no
`CI_REPO`, so T6's workflow sets `REPO: ${{ github.repository }}`.

### Managers

`enabledManagers`: `bun`, `npm`, `gomod`, `github-actions`, `custom.regex`.
Dropped from orion's list (`config.json5:63-72`): `cargo`, `rust-toolchain`
(compass has no Rust), `woodpecker` (no Woodpecker), and `nix` — Renovate's
nix manager tracks `flake.lock`, and compass has NO root flake: the only
`flake.lock` files in the tree live under `forks/devenv/` and
`forks/nix2container/` (glob-verified), both inside the `forks/*/**` fence
this record mandates `enabled: false`; `devenv.lock`/`devenv.yaml` are not
`flake.lock` (the custom git-refs manager covers them), so a ported nix
manager would be dead config. Added: **`github-actions`** — orion deliberately
omits it (its meta jobs moved off GHA, `config.json5:58-59`), but compass
keeps every workflow `uses:` pinned to a commit SHA precisely so a reviewable
PR moves the pin forward (`.github/dependabot.yml:1-8`). Renovate's
`github-actions` manager natively updates an existing SHA pin and keeps the
`# vX.Y.Z` comment current — dependabot parity for maintained pins — and,
going beyond dependabot (which never pinned NEW actions), `extends:
["helpers:pinGitHubActionDigests"]` pins any future un-pinned `uses:` on
sight (guarded in `config.test.ts`). Group all actions bumps into one PR
("GitHub Actions", mirroring dependabot's single `actions` group), with one
exclusion: the `postgres` CI service image (see packageRules — the pgtest.go
coupling).

Also intentionally omitted: the **`dockerfile`/`docker` manager** — compass
has no first-party Dockerfile. Glob-verified against the clone: the only
Dockerfiles in the tree are `forks/oh-my-pi/Dockerfile`,
`forks/oh-my-pi/Dockerfile.robomp`, and
`forks/devenv/containers/devcontainer/Dockerfile`, all inside the
`forks/*/**` fence this record disables — a dockerfile manager would be dead
config, same reasoning as the nix-manager drop above. (Auto-updating orion's
harvester `oven/bun` base image is an orion follow-up, filed separately.)

### customManagers: 5 of orion's 7 port, +1 compass-new

| # | Orion manager (`ci/renovate/config.json5`) | Compass disposition |
| --- | --- | --- |
| 1 | Root `package.json` catalog regex (`:140-158`) | **Port unchanged.** Compass has the same unmanaged-catalog gap: `workspaces.catalog` (`package.json:12-22`, 9 pins) with `catalog:` consumers; Renovate's bun manager doesn't extract it. Keep `versioningTemplate: "npm"` (range preservation) and the recursive two-stage matchStrings; port the truncation-guard tests. |
| 2 | devenv-nixpkgs channel git-refs digest (`:159-192`) | **Port unchanged.** Compass has the same shape: `devenv.yaml:9-10` → `github:cachix/devenv-nixpkgs/rolling`, locked in `devenv.lock`; `devenv.nix:75-81` bakes `biome` + `markdownlint-cli2` from that channel while `@biomejs/biome` is also a catalog pin (`package.json:15`) — the SEA-1870 lockstep applies. Compass difference: only **biome** is dual-sourced (markdownlint-cli2 has no catalog pin — `grep markdownlint compass/package.json` → none), so the ported relock script rewrites one catalog pin, not two. |
| 3-5 | bun/node/moon toolchain pins (`:193-232`) | **Port with path change**: `ci/toolchain/versions/*.nix` → `tools/toolchain/versions/*.nix` (compass pin files confirmed: `tools/toolchain/versions/{bun,node,moon,go}.nix`; same `rec { version; srcs.{x86_64-linux,aarch64-linux,aarch64-darwin} }` shape, e.g. `bun.nix:2-17`). |
| 6 | Go version attr in devenv.nix (`:233-255`) | **Port, retargeted at `tools/toolchain/versions/go.nix`** — see "Go source of truth" below. |
| 7 | googleworkspace provider lockstep (`:256-287`) | **Drop.** Compass has no pulumi and no `provider.lock.json`. |

Plus one compass-new customManager (not an orion port): the Renovate self-pin
regex on `.github/workflows/renovate.yml`'s `bunx renovate@<version>` line
(datasource `npm`, depName `renovate`) — see Approach §Runner shape.

### Go source of truth: `go.nix`, one regex manager

Compass differs from orion: orion's go version lives ONLY in `devenv.nix` as the
`"go_1_26_5"` attr string, so orion's manager regexes `devenv.nix`. Compass
single-sources the version in `tools/toolchain/versions/go.nix`
(`{ version = "1.26.6"; }`, version-only — hashes come from go-overlay) and
**derives** the attr name in `devenv.nix:30-31`:

```nix
goPin = import ./tools/toolchain/versions/go.nix;
goToolchain = inputs.go-overlay.packages.${pkgs.stdenv.system}."go_${lib.replaceStrings [ "." ] [ "_" ] goPin.version}";
```

So `devenv.nix` contains no literal `go_X_Y_Z` string — orion's regex would match
nothing there. Track `go.nix` instead: one regex manager,
`managerFilePatterns: ["/^tools/toolchain/versions/go\\.nix$/"]`,
`matchStrings: ["version = \"(?<currentValue>[^\"]+)\""]`,
`datasourceTemplate: "golang-version"`, `depTypeTemplate: "toolchain"`. No
dots↔underscores gymnastics (the version is dotted in the file), no
postUpgradeTasks leg (go-overlay ships the hashes; the refresh script must
self-gate past `go.nix` exactly as orion's no-ops on go —
`refresh-toolchain-hashes.ts:14-17`), and `devenv.nix` updates automatically at
eval time. One bump PR touches one line. This was OQ4, now decided (see
Resolved decisions). Note the `go.nix:8-9` floor policy: the `go` directive in
`go/go.mod` "tracks the tools/toolchain/versions/go.nix pin minus at most one
minor, so an upstream Go security patch never blocks on a mod edit"
(`go/go.mod:10-12`) — a MANUAL policy by design. Renovate's gomod manager
extracts that directive from the same golang-version datasource and could bump
it ahead of the pin, so the gomod `go`-directive update is disabled by
packageRule (see packageRules); a pin bump PR may occasionally need a manual
go.mod follow-up.

### packageRules

Port from orion (`config.json5:290-518`), adapted:

- "TypeScript dependencies" rollup: `bun`/`npm`/`custom.regex` patch+minor
  (`:299-303`). Drop the Rust rollup (no cargo).
- "Go dependencies": gomod patch+minor (`:344-348`).
- "GitHub Actions" group (compass-specific add): `matchManagers:
  ["github-actions"]`, one rollup PR — dependabot parity.
- **Postgres service image excluded** (compass-specific): `matchDepNames:
  ["postgres"], enabled: false`, with the coupling rationale in a config
  comment. The github-actions manager WOULD bump the `ci.yml:133` service
  image digest (`image: postgres:16-alpine@sha256:57c72fd2…`), but
  `go/internal/pgtest/pgtest.go:50` hard-codes the SAME digest as a Go const
  (`const pgImage = "docker.io/library/postgres:16-alpine@sha256:57c72fd2…"`)
  that Renovate cannot see, and `ci.yml:124` declares the parity load-bearing
  ("Matches pgtest.go's pinned image"). A one-sided bump silently desyncs the
  CI↔local Postgres the suites assert against, so this digest moves only via
  a manual two-file PR.
- **gomod `go` directive disabled**: `matchManagers: ["gomod"], matchDepNames:
  ["go"], enabled: false` + a `config.test.ts` assertion — keeps the
  `go/go.mod:10-12` floor policy manual (see Go source of truth).
- **bunfig soak exemptions implemented**: `matchPackageNames: ["@types/bun",
  "bun-types"], minimumReleaseAge: null`, scoped to the catalog `custom.regex`
  manager. `@types/bun` is a catalog pin (`package.json:16`, `"@types/bun":
  "^1.4.0"`) that would otherwise soak 5 days behind every bun-runtime bump —
  exactly the stranding `bunfig.toml:13-19` documents exempting ("it would
  strand the types packages behind the pin for 5 days on every bun upgrade";
  `minimumReleaseAgeExcludes`, `bunfig.toml:20-24`). A `config.test.ts` guard
  pairs the rule's names to bunfig's exclude list so the two files can't
  drift. (`@tanstack/virtual-core` needs no rule — an `overrides` pin, out of
  the catalog manager's reach, as Global Constraints already argue.)
- Toolchain un-grouping: `matchFileNames: ["tools/toolchain/versions/*.nix"],
  groupName: null` (orion `:304-313`, path adapted). Because the go manager now
  targets `go.nix` under the same glob, this one rule un-groups all four pins —
  orion's separate go un-group rule (`:314-326`) is NOT needed; note this in the
  config comment and pin it in config.test.ts.
- TypeScript `<7` cap (`:327-343`): port as-is.
- devenv-nixpkgs solo branch (`:353-403`): own groupName, `schedule: ["before
  4am"]` — DAILY, not orion's weekly-Monday `["before 4am on monday"]`
  (resolved decision, OQ5 — Matt: nixpkgs also daily; the deliberate
  divergence from orion `:353-403` gets a config comment), aligned with the
  `0 6 * * *` UTC cron inside the before-4am-ET window;
  `minimumReleaseAge: null` (a moving-branch digest never clears a
  release-age window — the SEA-1220 silent-pending shape), branch-mode
  postUpgradeTasks running the ported relock script with `fileFilters:
  ["devenv.lock", "package.json", "bun.lock"]`.
- Catalog lockfile coupling (`:450-493`): `matchDepTypes: ["workspaces.catalog"]`,
  `postUpgradeTasks: { commands: ["bun install --lockfile-only"], fileFilters:
  ["bun.lock"], executionMode: "update" }`. `executionMode` MUST stay `"update"`
  — orion's comment (`:460-481`) documents the one-branch-mode-task-per-branch
  collision this avoids; port that rationale.
- Fork fence: `matchFileNames: ["forks/*/**"], enabled: false` (orion's
  `oss/forks/*/**` at `:494-504`, path adapted to compass's root `forks/`).
- Drop: "Nix flake inputs" group (`:349-352` — dead config with the nix
  manager omitted; see Managers), provider solo branch (`:404-445`), pulumi
  SDK disable (`:505-518`), container-images group (`:446-449` — compass has
  no docker-datasource deps; the postgres service image is deliberately
  EXCLUDED above, not covered).
- Top-level postUpgradeTasks (`:541-550`): `bun <dir>/refresh-toolchain-hashes.ts`
  branch-mode, `fileFilters: ["tools/toolchain/versions/bun.nix", …/node.nix,
  …/moon.nix]` — no rust-manifest leg (compass has no `rust-toolchain.toml`).

### Supporting scripts: port 3, drop 1

All live beside the config in the renovate directory (`tools/renovate/` — a
resolved decision, see Resolved decisions):

- **`refresh-toolchain-hashes.ts` + test** — port with compass paths
  (`BUN_NIX/NODE_NIX/MOON_NIX = "tools/toolchain/versions/*.nix"`) and the
  entire Rust FOD leg removed (`TOOLCHAIN_TOML`/`MANIFEST_HASH_NIX` constants,
  `readChannel`, `channelManifestUrl`, `renderManifestHashFile` — orion
  `refresh-toolchain-hashes.ts:54-61,112-183`). Keep the self-gate, per-leg
  rewrite, fail-loud, idempotence contracts (`:29-42`).
- **`refresh-devenv-nixpkgs.ts` + `.core.ts` + tests** — port; compass
  adaptation: only the biome catalog pin is rewritten (markdownlint-cli2 has no
  catalog pin in compass — `package.json:12-22`), and compass's baked-vs-catalog
  coupling is the dev-shell parity story, not orion's SEA-1128 image gate; the
  relock still must refresh `devenv.lock` consistently (rev + narHash + inner
  nixpkgs-src) and re-resolve `bun.lock`.
- **`config.test.ts`** — port the guard suite: allowedCommands ↔ postUpgradeTasks
  pinning, the no-`vulnerabilityAlerts.enabled` invariant, real-manifest catalog
  extraction recovery + the two truncation mutation tests, solo-branch grouping
  outcomes, adapted to compass's config. Compass-specific additions: the
  workflow pins an exact Renovate version (no bare `bunx renovate`); the
  `helpers:pinGitHubActionDigests` preset is extended; the postgres-image
  disable rule exists; the gomod `go`-directive disable rule exists; the
  `@types/bun`/`bun-types` soak-exemption rule's names match `bunfig.toml`'s
  `minimumReleaseAgeExcludes` (minus the `overrides`-pinned
  `@tanstack/virtual-core`).
- **Drop** `refresh-provider-manifest.ts` + test (no pulumi).

### File locations

Repo config + bot config + scripts in **`tools/renovate/`** (resolved decision).
Compass has no `ci/` directory — every first-party tool lives under `tools/*`
(`tools/{toolchain,design-ledger-gate,stamp-gate,…}`), and `tools/*` is a bun
workspace member (`package.json:10`), giving the scripts the standard
tsconfig/test wiring. The bot config's `configFileNames:
["tools/renovate/config.json5"]` makes the repo-config path free (orion
`bot-config.json5:12-15`). The workflow itself is `.github/workflows/renovate.yml`
(GHA requires that location). The preflight probe ports to
`tools/renovate-preflight/` (orion's own location, already `tools/`-shaped).

### Bot config

Port `bot-config.json5` with: `configFileNames: ["tools/renovate/config.json5"]`;
`repositories: ["RigelBuild/compass"]` (must match the live slug — a renamed repo
is silently skipped, orion `bot-config.json5:17-24`); `platform: github`;
`gitAuthor` = the App's `[bot]` noreply identity
(`<app-id>+<app-slug>[bot]@users.noreply.github.com` — Renovate autodetects
it from the installation token; pin it explicitly here once T8 registers the
App and the slug is known); `onboarding: false`; `requireConfig: "required"`;
`customEnvVariables: { HOME: "/tmp/renovate-home" }`; `allowedCommands` with
exactly the three anchored entries compass's config declares:
`^bun tools/renovate/refresh-toolchain-hashes\.ts$`,
`^bun install --lockfile-only$`,
`^bun tools/renovate/refresh-devenv-nixpkgs\.ts$`.

Fleet note: orion's own `ci/renovate/bot-config.json5:27` still pins the
retired pre-RigelBuild-rename bot identity as its gitAuthor — a separate
fleet cleanup, not fixed by this record. (For non-App contexts the fleet
agent identity is `mintaka <mintaka@rigel.build>`, GitHub `rigel-mintaka`,
per `~/.config/jj/config.toml:21,34-35`; for THIS workflow the committer is
the App bot.)

## Alternatives considered

### A — `renovatebot/github-action` (official action)

Runs Renovate inside the official `renovate/renovate` container. Clean for a
config-only repo, but compass's postUpgradeTasks execute INSIDE that container,
which ships none of `nix`, `devenv`, or the pinned `bun` — the toolchain-hash
prefetch (`nix store prefetch-file`), the devenv relock (`devenv update
nixpkgs`), and the lockfile regeneration all fail. Making it work means either a
custom Renovate image (that's option C) or mounting a host toolchain into the
container (fragile, and nix store paths don't relocate). Loses to B: same
workflow-trigger surface, strictly less toolchain access.

### C — bake a compass-ci image with renovate + devenv (orion's approach)

Orion runs Renovate in its Woodpecker publish monolith
(`ghcr.io/rigelbuild/orion-ci-publish:latest`, which bakes devenv —
`ci/workflows/meta.ts:29-31`). Compass has no equivalent image: its only
published image is the agent image (`publish-agent-image.yml`), not a CI
toolchain image — compass CI provisions per-job via install-nix-action +
gate-tools.nix instead. Building and publishing a dedicated Renovate image adds
a whole image-publish pipeline (registry, staleness, rebuild triggers on
devenv.lock bumps) to save per-run nix setup that the nix cache already bounds.
Heavier for no correctness gain. Loses to B.

### B — plain GHA job on compass's existing toolchain bootstrap + a vendored-devenv shim (CHOSEN)

See Approach. Reuses `.github/workflows/ci.yml`'s exact idiom for the language
toolchains (`ci.yml:144-212`); `devenv` — which no compass CI job puts on PATH
(the only devenv invocations in CI run the vendored fork's CLI by path,
`ci.yml:812` `nix run path:../forks/devenv#devenv -- …`) — is built from the
vendored fork and shimmed onto `$GITHUB_PATH`, so the relock script runs the
one devenv the fork posture allows; zero new infrastructure.

### D — provision devenv from nixpkgs (`nix profile install nixpkgs#devenv` or a gate-tools attr) — REJECTED

Rejected: a nixpkgs-built devenv doing the relock would be a SECOND devenv,
divergent from the vendored fork whose CLI compass pins by path everywhere
devenv runs ("pinned to the vendored fork's own CLI … so it cannot diverge
from the fork source", `devenv.nix:450-453`; "The fork's own CLI is invoked
by path everywhere the image is built",
`docs/designs/platform/compass-forks-reversal/design.md:125-134`) — exactly
the divergence the frozen fork posture exists to eliminate. The vendored-fork
shim in B costs one `nix build` (cache-bounded) and keeps a single devenv.

## Plan

Task order is dependency order: configs and scripts (T1-T5) are pure additions
testable in isolation; the workflow (T6) consumes them; the dependabot removal
(T7) lands only once Renovate is runnable; the App provisioning (T8) is the
one human action and gates first live run, not the merge.

### T1 — Port the repo config: `tools/renovate/config.json5`

Adapt orion `ci/renovate/config.json5` per Approach: extends
`config:recommended` + `schedule:daily` + `helpers:pinGitHubActionDigests`;
`timezone: "America/New_York"`; `dependencyDashboard: true`; `rebaseWhen:
"behind-base-branch"`; `osvVulnerabilityAlerts: true`; `minimumReleaseAge: "5
days"` + `internalChecksFilter: "strict"`; `labels: ["dependencies"]`;
`enabledManagers: [bun, npm, gomod, github-actions, custom.regex]` (no `nix` —
compass has no root flake; see Managers); the 7 customManagers (catalog,
devenv-nixpkgs, bun/node/moon pins at `tools/toolchain/versions/`, go at
`tools/toolchain/versions/go.nix`, the Renovate self-pin regex on
`.github/workflows/renovate.yml`); the packageRules set from Approach
(TS rollup + TS<7 cap, Go rollup, GitHub Actions group, postgres-image
disable, gomod `go`-directive disable, `@types/bun`/`bun-types` soak
exemption, toolchain un-group, devenv solo branch + relock task, catalog
lockfile coupling (`executionMode: "update"`), `forks/*/**` fence); top-level
branch-mode postUpgradeTasks running the hash refresh with the three compass
pin-file fileFilters.

Interfaces:

- Produces: `tools/renovate/config.json5`.
- Consumes (paths referenced in config): `package.json` (catalog block),
  `devenv.lock`, `tools/toolchain/versions/{bun,node,moon,go}.nix`,
  `forks/*/**` (fence), `.github/workflows/*.yml` (github-actions manager +
  the Renovate self-pin regex), `go/go.mod` (gomod; `go` directive disabled),
  `bunfig.toml` (soak-exemption parity, enforced by T5's guard).
- Commands declared (must match T2's allowlist exactly):
  `bun tools/renovate/refresh-toolchain-hashes.ts`,
  `bun install --lockfile-only`,
  `bun tools/renovate/refresh-devenv-nixpkgs.ts`.

### T2 — Port the bot config: `tools/renovate/bot-config.json5`

Per Approach §Bot config: `configFileNames: ["tools/renovate/config.json5"]`,
`repositories: ["RigelBuild/compass"]`, `platform: "github"`, gitAuthor = the
App's `[bot]` noreply identity (autodetected from the installation token;
pinned explicitly once T8 yields the App slug), `onboarding: false`,
`requireConfig: "required"`, `customEnvVariables: { HOME: "/tmp/renovate-home" }`,
`allowedCommands` = the three anchored regexes from T1.

Interfaces:

- Produces: `tools/renovate/bot-config.json5`.
- Consumed by: T6's workflow (`RENOVATE_CONFIG_FILE=tools/renovate/bot-config.json5`)
  and T5's config.test.ts (allowlist ↔ postUpgradeTasks pinning).

### T3 — Port `refresh-toolchain-hashes.ts` + test

Port orion `ci/renovate/refresh-toolchain-hashes.ts` (+ `.test.ts`) to
`tools/renovate/`: path constants become `BUN_NIX/NODE_NIX/MOON_NIX =
"tools/toolchain/versions/{bun,node,moon}.nix"`; DELETE the Rust FOD leg
entirely (`TOOLCHAIN_TOML`, `MANIFEST_HASH_NIX`, `readChannel`,
`channelManifestUrl`, `renderManifestHashFile` and their main() wiring + tests —
compass has no `rust-toolchain.toml`). Preserve: per-file self-gate against the
base branch, marker-anchored `rewriteHash` fail-loud contract, `readVersion`,
`sriForUrl` via `nix store prefetch-file`, idempotence.

Interfaces:

- Produces: `tools/renovate/refresh-toolchain-hashes.ts`,
  `tools/renovate/refresh-toolchain-hashes.test.ts`.
- Exports (consumed by its test): `BUN_NIX: string`, `NODE_NIX: string`,
  `MOON_NIX: string`, `rewriteHash(fileText, marker, newSri, file): string`,
  `readVersion(nixText, file): string`.
- Reads/writes at runtime: `tools/toolchain/versions/{bun,node,moon}.nix`.
- Requires on PATH: `nix` (nix-command), `bun`, `git` (base-branch self-gate).

### T4 — Port `refresh-devenv-nixpkgs.ts` + `.core.ts` + tests

Port orion `ci/renovate/refresh-devenv-nixpkgs{.ts,.core.ts,.test.ts,.core.test.ts}`
to `tools/renovate/`. Compass adaptation: rewrite ONLY the `@biomejs/biome`
catalog pin (compass's catalog has no markdownlint-cli2 entry — `package.json:12-22`;
drop `MARKDOWNLINT_CATALOG_KEY` and its rewrite leg). Preserve: devenv.lock
self-gate, `devenv update nixpkgs` relock, baked-version eval from the inner
nixpkgs-src rev, `bun install --lockfile-only` re-resolve, fail-loud exit
contract.

Interfaces:

- Produces: `tools/renovate/refresh-devenv-nixpkgs.ts`,
  `tools/renovate/refresh-devenv-nixpkgs.core.ts`, both test files.
- Core exports: `BIOME_CATALOG_KEY: string`,
  `innerNixpkgsRev(lockJson: string): string`,
  `rewriteCatalogPin(packageJsonText, key, version): string`.
- Reads/writes at runtime: `devenv.lock`, `package.json`, `bun.lock`.
- Requires on PATH: `nix`, `devenv` (the VENDORED fork CLI via T6's shim —
  the script shells `devenv update nixpkgs` from PATH, orion
  `refresh-devenv-nixpkgs.ts:129`, and a nixpkgs devenv is rejected per
  Alternatives §D), `bun`, `git`; writable `$HOME` (bot config sets
  `/tmp/renovate-home`).

### T5 — Port `config.test.ts`

Port orion `ci/renovate/config.test.ts` guards, adapted: (1) every
postUpgradeTasks command in config.json5 has an anchored allowedCommands entry
in bot-config.json5 and vice versa; (2) `vulnerabilityAlerts.enabled` is absent
and `osvVulnerabilityAlerts` is true; (3) real-manifest catalog extraction —
the regex recovers every pin the JSON parser sees in compass's actual
`package.json`, plus the two truncation mutation tests; (4) solo-branch
grouping outcomes (toolchain pins and devenv-nixpkgs never land in the TS
rollup); (5) the TS `<7` cap rule exists; (6) the `forks/*/**` fence rule
exists with `enabled: false`; (7) devenv-nixpkgs extraction pinned against the
real `devenv.lock`. Compass-new guards: (8) the workflow pins an exact
Renovate version — no bare `bunx renovate`; (9) `extends` includes
`helpers:pinGitHubActionDigests`; (10) the postgres-image disable rule exists
(`matchDepNames: ["postgres"], enabled: false`); (11) the gomod
`go`-directive disable rule exists; (12) the `@types/bun`/`bun-types`
`minimumReleaseAge: null` rule's names equal `bunfig.toml:20-24`'s
`minimumReleaseAgeExcludes` minus `@tanstack/virtual-core`. Wire into a
`tools/renovate` workspace package (package.json + moon.yml + tsconfig per
sibling `tools/*` convention) AND register it in `.moon/workspace.yml`'s
project map — moon discovers projects only from that explicit map
(`.moon/workspace.yml:10`, and its own warning at `:77-83`: an unregistered
moon.yml "is silently inert … gates nothing while reading as covered");
without the entry, none of guards (1)-(12) ever runs. Verify: `moon query
projects` lists `tools/renovate`.

Interfaces:

- Produces: `tools/renovate/config.test.ts`, `tools/renovate/package.json`,
  `tools/renovate/moon.yml`, `tools/renovate/tsconfig.json`.
- Modifies: `.moon/workspace.yml` (project-map entry for `tools/renovate`).
- Consumes: `tools/renovate/config.json5`, `tools/renovate/bot-config.json5`,
  `package.json`, `devenv.lock`, `bunfig.toml`,
  `.github/workflows/renovate.yml` (guards 8-9, 12).

### T6 — Author the workflow: `.github/workflows/renovate.yml` (+ preflight port)

Per Approach §Runner shape. Jobs: one `renovate` job on `ubuntu-latest` (or the
repo's standard runner label per `ci.yml`), steps:

1. `actions/checkout@3d3c42e5…` (SHA-pinned, same pin as `ci.yml:144`) with
   `fetch-depth: 0` (the refresh scripts' self-gates diff against the base
   branch).
2. `cachix/install-nix-action@630ae543…` with the same `extra_nix_config` block
   as `ci.yml:165-168` (nix-command flakes + the two substituters).
3. Toolchain on PATH via `nix build -f tools/toolchain/gate-tools.nix` (the
   `ci.yml:182-212` idiom) — provides bun/node/moon/go.
4. Vendored devenv on PATH: `nix build path:forks/devenv#devenv` (from the
   repo root — the flake exports the CLI as `packages.<system>.devenv`,
   `forks/devenv/flake.nix:113-115`; `ci.yml:812` writes it
   `path:../forks/devenv#devenv` only because that step's cwd is
   `agent-image/`), then symlink `$out/bin/devenv` into a directory prepended
   to `$GITHUB_PATH`. This is the ONLY devenv the job gets — compass CI never
   provisions one — and the relock script requires it on PATH (T4).
5. Assert devenv on PATH: `command -v devenv` as its own fail-loud step, so a
   missing shim reds at setup instead of exit-127ing silently on the first
   channel-bump branch (orion's regression class: on an image without devenv
   the relock "exits 127 (`devenv: command not found`) on every channel-bump
   branch, shipping a half-refreshed lock", orion `ci/pipeline.test.ts:954-963`,
   SEA-1304/RIG-2245).
6. Mint the App installation token:
   `actions/create-github-app-token@<pinned-sha> # vX` (SHA-pin + version
   comment per the repo invariant) with `client-id: ${{
   vars.RENOVATE_APP_CLIENT_ID }}` and `private-key: ${{
   secrets.RENOVATE_APP_PRIVATE_KEY }}`. Its `token` output feeds every later
   step — no long-lived PAT exists (see Approach §Secrets/Auth).
7. Port `tools/renovate-preflight/` from orion and run it with
   `GH_TOKEN=${{ steps.<mint>.outputs.token }}` and
   `REPO: ${{ github.repository }}` — the preflight reads `REPO` and exits
   fail-closed when missing (orion `tools/renovate-preflight/index.ts:9,15`;
   GHA has no `CI_REPO`, so the workflow must set it or every run dies at
   preflight). Adapt the ported index.ts comment (`REPO - owner/name (from
   github.repository)`). Register `tools/renovate-preflight` in
   `.moon/workspace.yml`'s project map (same inertness trap as T5; verify
   `moon query projects` lists it).
8. `env: RENOVATE_CONFIG_FILE: tools/renovate/bot-config.json5,
   RENOVATE_X_IGNORE_RE2: "true",
   RENOVATE_TOKEN: ${{ steps.<mint>.outputs.token }}`
   → `bunx renovate@44.33.1` (exact pin per Approach; the self-pin
   customManager bumps it).
Triggers: `schedule: [{cron: "0 6 * * *"}]` (margin per the SEA-1220
constraint) + `workflow_dispatch`. Two GHA scheduled-workflow caveats, in a
workflow comment: (a) cron is best-effort — starts are routinely 5-30+ minutes
late, which the 06:00 UTC margin absorbs; (b) GHA auto-disables a scheduled
workflow after 60 days without repo activity — `workflow_dispatch` (or any
commit) revives it, but someone must notice; the dependency dashboard going
stale is the tell. Every `uses:` SHA-pinned with a `# vX` comment (repo
invariant, `dependabot.yml:1-8`).

Interfaces:

- Produces: `.github/workflows/renovate.yml`, `tools/renovate-preflight/*`
  (ported: `index.ts`, `preflight.ts`, `preflight.test.ts`, `package.json`,
  `moon.yml`, `tsconfig.json`).
- Modifies: `.moon/workspace.yml` (project-map entry for
  `tools/renovate-preflight`).
- Consumes: `tools/renovate/bot-config.json5` (T2),
  `secrets.RENOVATE_APP_PRIVATE_KEY` + `vars.RENOVATE_APP_CLIENT_ID` (T8),
  `tools/toolchain/gate-tools.nix`, `forks/devenv/` (the vendored
  devenv flake).

### T7 — Delete `.github/dependabot.yml`

Remove the file. Its SHA-pin rationale header (lines 1-8) moves to a comment on
the github-actions section of `tools/renovate/config.json5` so the invariant's
rationale survives the migration. Same PR as T1-T6: no window with two version
managers, and — because T8's App registration + secret/variable are
provisioned during PR review, before merge — a ~zero window with none
(merge ≈ first live-capable run; if provisioning lags, the gap is loud, not
silent: the token-mint step and preflight fail each scheduled run with a
named diagnosis).

Interfaces:

- Deletes: `.github/dependabot.yml`.
- Rationale text lands in: `tools/renovate/config.json5` (github-actions
  manager comment).

### T8 — Human action: register + install the Renovate GitHub App; disable Dependabot in Settings

File a Linear issue assigned to Matt, label `human-action`, per
skill://human-action-handoff, **when the design PR opens** — the App can be
registered and its secret/variable created before the workflow that reads
them exists, so provisioning during PR review collapses the zero-manager
window to ~zero (no second PR, merge ≈ live). Runbook, copy-pasteable:

1. Register a GitHub App under the RigelBuild org (name e.g.
   `rigel-renovate`) with repository permissions: **Contents (read/write)**,
   **Pull requests (read/write)**, **Workflows (read/write)**, **Issues
   (read/write)**. Workflows is load-bearing: the github-actions manager
   pushes commits to `.github/workflows/*`, which a GitHub App may only do
   with the Workflows permission (GitHub Docs: "if your app specifically
   needs to access or edit Actions files in the .github/workflows directory,
   request the Workflows repository permission"); Issues because the
   dependency dashboard is an issue.
2. Generate a private key for the App; store it as the Actions repo secret
   `RENOVATE_APP_PRIVATE_KEY` on RigelBuild/compass. Store the App's
   client-id as the repo variable `RENOVATE_APP_CLIENT_ID`.
3. Install the App on RigelBuild/compass (single-repo installation — the
   blast radius stays compass-only).
4. Disable Dependabot alerts + security updates in RigelBuild/compass repo
   Settings — the `.github/dependabot.yml` deletion (T7) stops version
   updates only; the hidden-billed-features driver (Problem / Intent) is the
   Settings-side security feature, replaced by `osvVulnerabilityAlerts: true`.
   Without this step "Dependabot fully off" is not achieved.
Steps 1-3 are blocking for the first live run (not the merge — the workflow
fails loud at token-mint/preflight until provisioned); step 4 is non-blocking
but required to close the migration's driver. Once the App is registered,
record the App slug and pin `gitAuthor` in `tools/renovate/bot-config.json5`
to the App's `[bot]` noreply identity (T2).

Interfaces:

- Produces: a Linear issue (`human-action`); no repo files.
- Unblocks: first successful run of `.github/workflows/renovate.yml`.

### T9 — Design-ledger delta

This record freezes with its ledger delta in the same PR. Compass's ledger gate
is product-scoped (`tools/design-ledger-gate/index.ts:45` `PRODUCT_DIR =
"docs/designs/product"`; `touchesRecord` at `:204-210` matches only that tree),
and `docs/designs/platform/` has no `DECISIONS.md` — so the touch-coupling leg
does not fire for this record. Declare `Ledger-impact: platform record; product
ledger untouched` in the design PR body (the gate accepts an explicit
declaration), or append to a platform `DECISIONS.md` if one exists by
implementation time. The IMPLEMENTATION PR body carries the same declaration if
it touches this record.

Interfaces:

- Produces: `Ledger-impact:` line in the design PR body (and a
  `docs/designs/platform/DECISIONS.md` row iff that ledger exists by then).
- Consumed by: `tools/design-ledger-gate` on the PR event.

## Tasks

- [ ] T1 — `tools/renovate/config.json5` (managers, packageRules, postUpgradeTasks)
- [ ] T2 — `tools/renovate/bot-config.json5` (slug, allowedCommands, HOME)
- [ ] T3 — `tools/renovate/refresh-toolchain-hashes.ts` + test (compass paths, no Rust leg)
- [ ] T4 — `tools/renovate/refresh-devenv-nixpkgs{.ts,.core.ts}` + tests (biome-only pin)
- [ ] T5 — `tools/renovate/config.test.ts` + workspace package wiring + `.moon/workspace.yml` registration
- [ ] T6 — `.github/workflows/renovate.yml` (vendored-devenv shim, pinned renovate, `REPO` env) + `tools/renovate-preflight/` port + `.moon/workspace.yml` registration
- [ ] T7 — delete `.github/dependabot.yml` (rationale moves to config comment)
- [ ] T8 — human-action Linear issue (filed at PR-open): register/install the GitHub App, `RENOVATE_APP_PRIVATE_KEY` secret + `RENOVATE_APP_CLIENT_ID` variable, Settings-side Dependabot disable
- [ ] T9 — ledger delta / `Ledger-impact:` declaration

## Resolved decisions

Formerly open questions; resolved in design critique or by Matt's answers and
folded into the record as decisions:

- **Runner shape: B** (was OQ1) — plain GHA job: checkout → install-nix-action
  → gate-tools toolchain → vendored-devenv shim → pinned `bunx renovate`. A's
  container lacks nix/devenv/bun for postUpgradeTasks; C requires a compass CI
  image publish pipeline that doesn't exist; a nixpkgs devenv is rejected on
  fork-posture grounds (Alternatives §D). See Approach §Runner shape.
- **File location: `tools/renovate/`** (was OQ3) — compass has no `ci/` tree;
  all first-party tooling is `tools/*`, a bun workspace glob
  (`package.json:10`), so the scripts and tests get standard wiring;
  `.github/` would strand TypeScript outside the workspace. The preflight
  keeps orion's own `tools/renovate-preflight/` naming. The bot config's
  `configFileNames` makes any choice mechanically workable — convention only.
- **Go source of truth: `go.nix`** (was OQ4) — compass derives the `go_X_Y_Z`
  attr at eval time (`devenv.nix:30-31`); there is no literal attr string for
  orion's devenv.nix regex to match, and `go.nix` is the declared single
  source (`go.nix:1-9`). One regex manager on `go.nix`; `devenv.nix` untouched
  by Renovate; the gomod `go`-directive update disabled (see packageRules).
- **Grouping: orion parity** (was OQ6) — TS rollup, Go rollup, GitHub Actions
  group; majors solo; toolchain pins solo. Same review granularity across the
  fleet; dependabot's old single-group-per-ecosystem shape maps 1:1
  (actions→"GitHub Actions", bun→"TypeScript dependencies", gomod→"Go
  dependencies").
- **Auth: GitHub App, per-run minted token** (was OQ2; Matt 2026-08-21) —
  chosen over widening orion's shared bot PAT or a compass-scoped PAT: zero
  long-lived secret and single-repo blast radius. No shared cross-repo
  credential lives on two CI secret surfaces; the only stored material is
  compass's own App private key, scoped to compass's installation, and the
  token Renovate receives is minted fresh each run (~1h expiry). Permission
  set: Contents, Pull requests, Workflows, Issues — all read/write; Workflows
  is load-bearing for the github-actions manager (see Approach §Secrets/Auth,
  T8). gitAuthor is the App's `[bot]` noreply identity, autodetected from the
  installation token. This also bounds the F1 self-pin exposure (an ephemeral
  token instead of a stored PAT) without relaxing the `bunx renovate@<pin>`
  requirement.
- **Cadence: daily — top-level AND devenv-nixpkgs** (was OQ5; Matt
  2026-08-21) — `schedule:daily` with the `0 6 * * *` UTC cron, and the
  devenv-nixpkgs solo branch drops orion's weekly-Monday restriction to
  `["before 4am"]` daily (a deliberate divergence from orion's
  `config.json5:353-403`). `minimumReleaseAge: null` stays on that branch —
  a moving-branch digest never clears a release-age window.
