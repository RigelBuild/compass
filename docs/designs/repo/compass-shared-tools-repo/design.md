# Share repo tools from one public repo (RIG-4351)

## Problem / Intent

Compass and one private consumer repo each keep their own copy of the same repo
tools, and the copies drift. A fix lands in one copy and not the other. Example:
the private consumer's design-ledger gate exempts Trunk merge-queue branches
(`trunk-merge/`), but compass's copy does not, though compass also lands through
the Trunk queue. That gap has no effect in compass today: the gate's
touch-coupling leg needs `REPO` and `PR_NUMBER`, and no compass CI job sets them.

Intent: move every tool both repos run into one new public repo, and have each
consumer pin it, so each tool has one copy. This record folds in RIG-4184
("share the design-ledger tools from one source"). The ledger tools are its
first slice.

## Inventory

Measured on 2026-10-04 with `git diff --no-index --stat` between the compass
copy and the private consumer's copy (changes are compass relative to the
private copy). "Private copy" says only whether a copy exists. This public
record names no path in the private repo.

| Tool | Compass path | Private copy | Drift | Verdict |
| --- | --- | --- | --- | --- |
| Design-ledger gate | `tools/design-ledger-gate/` | Yes | Large: 6 files, +1,143 / −6,735. The private copy finds per-surface ledgers by glob, reports malformed rows, has citation, errata, and record-link legs, exempts `trunk-merge/`, and parses with micromark. Compass has one ledger and a fixed bucket list. | Move (T4) |
| DL reconcile | `tools/dl-reconcile/` | Yes | Yes: 6 files, +1,088 / −501. Compass reads one ledger, cross-counts raw rows, refuses an empty frontier, and has `--check`. The private copy reads several ledgers and reports stale and duplicate claims. | Move (T5) |
| DL claim | `tools/dl-claim/` | No | None to measure. Compass-only client; the request type hard-codes `repo: "compass"`. | Move (T5) |
| SEA reference gate | `tools/sea-ref-gate/` | Yes | Yes: 8 files, +334 / −611. The private copy splits the core into its own module. | Move as `ref-gate` (T3) |
| Private-name reference gate | `tools/orion-ref-gate/` | No | None to measure. Same four exported functions as the SEA gate (`isCarveOut`, `lineHasToken`, `findViolations`, `runOnce`). | Becomes a `ref-gate` config (T3) |
| Renovate preflight | `tools/renovate-preflight/` | Yes | Small: 6 files, +47 / −43 (comments, task and package config). | Move (T6) |
| Bun moon task template | `.moon/tasks/tag-bun.yml` | Yes | Comments only. No task differs. | Stay local |
| Root checks | `lint`, `format`, `markdownlint` tasks in the root `moon.yml` | Different tool | Not a copy. The private consumer runs a separate tool that runs its checks outside the moon cache and gates baked linter versions. | Stay local |
| Agent extensions | None | Yes | None to measure. Personal agent config, not repo tooling. | Stay private |
| Renovate config | `tools/renovate/config.json5` | Yes | Different files that share ideas (git-refs cooldown, catalog handling). | Later record |
| Renovate upgrade scripts | `tools/renovate/refresh-go-overlay[.core].ts`, `refresh-devenv-nixpkgs[.core].ts`, `refresh-toolchain-hashes.ts` | Yes, same names | Not measured. Each bot config's `allowedCommands` regex names these paths, so moving them changes that config too. | Later record |

## Approach

### One public repo, one package per tool

Create `RigelBuild/repo-tools`. It is a bun workspace with one package per tool
under `packages/<tool>/`, published as `@rigelbuild/<tool>` with a bin of the
same name. It is also a nix flake that exports the shared Nix tooling (see
"Nix tooling"). Each consumer pins exact versions and deletes its own copy in
the same PR. There is no shim and no re-export.

The port base is the compass copy. It is already public, so starting from it
moves nothing private. Features only the private copy has (per-surface ledgers,
the citation, errata, and record-link legs, malformed-row reporting) are written
again as new public code on top of it (OQ3: yes).

### Config, not literals

The two repos differ in real ways. Compass has one ledger and a fixed bucket
list. The private consumer has several per-surface ledgers. Each repo claims DL
IDs from its own counter partition. So no consumer value is a literal in shared
code. Each consumer keeps one config file, `docs/designs/ledger.config.json`,
which the gate, claim, and reconcile tools all read. The reference gate reads
its own config file.

The CLIs keep their environment inputs (`REPO`, `PR_NUMBER`, `GH_TOKEN`,
`DL_CLAIM_TOKEN`, `RENOVATE_TOKEN`). Every tool also reads `GATE_ROOT`, default
the git toplevel, as the consumer root. A tool under `node_modules` cannot find
the ledger relative to its own file, as `dl-reconcile` does today
(`resolve(import.meta.dir, …)`). The counter URL comes only from
`counter.url`; no env var overrides it.

Exit codes are normalized to 0 pass, 1 violations, 2 usage or internal error.
This is a change, not a carry-over: `dl-claim` and `dl-reconcile` exit 1 for
every error today. CI only checks for non-zero, so no workflow changes.

CI wiring changes more than the command. `dl-reconcile.yml` and the Renovate
preflight step in `renovate.yml` run the tool from the checkout with no
`bun install`, and both jobs hold a secret. A package bin needs an install
first. Per OQ4, those jobs run the full `bun install --frozen-lockfile`, so
every install is checked against `bun.lock`.

### Pinning (OQ1: npm, plus nix)

| Option | How a consumer pins | For | Against |
| --- | --- | --- | --- |
| npm package (chosen for TS tools) | Exact version in the bun catalog; `bun.lock` keeps the integrity hash | Both repos already take tools as bun dependencies. Compass's Renovate catalog manager already reads npm versions. One package per tool. | Needs a publish lane in the shared repo and a one-time bootstrap publish per package |
| bun git dependency | `github:RigelBuild/repo-tools#<sha>` in the root `package.json` | No registry and no publish lane | One package for the whole repo (bun installs a git repo root, not a subdirectory). Compass's catalog manager reads only npm versions. |
| Rev-pin JSON | A `{repo, ref, rev}` file plus a fetch step before each run. The private consumer already pins compass this way. | Precedent exists, and compass's Renovate config already bumps git revs with a regex manager | Each consumer writes its own fetch step. The tools' own npm dependencies need a separate install. No typed imports. Local runs need the fetch too. |
| Nix flake input (chosen for Nix tooling) | Flake input; `flake.lock` keeps the narHash | Content-addressed and nix-native | Wrong fit for the bun tools: each would need a nix package build, and moon and `tsc` cannot typecheck against a store path. Right fit for Nix code. |

TS tools ship as npm packages, so a tool bump is an ordinary catalog PR with
release notes. Nix tooling ships as flake outputs, pinned by the consumer's
`devenv.lock` (compass takes its RigelBuild forks as `devenv.yaml` inputs, and
Renovate's nix manager is off by design). Per OQ5, first-party pins skip the
release-age cooldown in every toolchain.

Publishing uses npm trusted publishing (OIDC from the shared repo's `main`
release workflow), so no long-lived publish token exists. The `@rigelbuild`
npm scope already exists (compass resolves `@rigelbuild/solid-virtual` from
npm). npm can set a trusted publisher only on a package that already exists,
so each new package needs one bootstrap publish and one trusted-publisher entry.
These steps have no IaC path, so they go to Matt as one human-action issue.

### Nix tooling

Matt's OQ1 ruling adds Nix: the shared repo is also a flake for the Nix code
both repos carry. This record reads that as shared Nix modules and helpers
(gate-tool sets, image-tool environments), not a nix build of the bun tools.
Toolchain version pins stay local: the toolchain-parity gate checks them
against each consumer's own dev shell, and the Renovate upgrade scripts that
rewrite them are a later record. T10 inventories the rest and moves each shared
file under `nix/` as a flake output. The public boundary applies unchanged.

### What may move (the public boundary)

A tool qualifies only if all three conditions hold:

1. It runs in compass today, or it is a generic check that any public repo could
   run unchanged.
2. It holds no consumer literal: no private repo name, path, surface name,
   counter partition, hostname, or record citation. Those come from consumer
   config.
3. Its tests use synthetic fixtures only.

Enforcement:

- The shared repo runs `ref-gate` on itself as a required check. It fails closed
  on the private repo's name and on retired SEA issue ids, the same as compass.
  The name appears only in the gate's own config file, which is carved out, as
  compass carves out its gate source today.
- Every port PR starts from public code. A private-only feature is written again
  and reviewed against the three conditions. It is never copied file to file.
- Matt is CODEOWNER and approves every PR, as in compass.

### Drift prevention after migration

No copy is left to drift. A consumer cannot patch a tool locally without
adding a copy back, and the Global Constraints forbid that. Renovate bumps both
pins, so skew between the consumers is a version number, not a fork. Because
first-party pins skip the cooldown (OQ5), the skew lasts from release to the
next Renovate run plus Matt's review of each bump PR. A fix lands as a
shared-repo PR and reaches each consumer by pin bump.

### Alternatives considered

- Keep the tools in compass and have the private consumer pin compass: rejected
  by the ask. It ties the consumer to compass's `main` and mixes product
  releases with tool releases.
- Keep vendored copies and add a CI check that they match the pin: rejected. It
  keeps two copies to edit, and the check only reports drift after it happens.
- Git submodule: rejected. jj, the house VCS, does not manage submodules.

## Global Constraints

- The shared repo is public and follows the boundary in "What may move".
- Licence: `MIT OR Apache-2.0` (OQ6). This relicenses tools that are
  AGPL-3.0-only in compass, so T2 first checks that no ported file has an
  outside contribution. A file that has one stays out until its author agrees.
- Runtime and checks: bun, TypeScript `strict` plus `noUncheckedIndexedAccess`,
  biome, `bun test`, rumdl for markdown.
- Each tool is the package `@rigelbuild/<tool>` under `packages/<tool>/`, with
  bin `<tool>` pointing at `./index.ts` (shebang `#!/usr/bin/env bun`).
- CLIs read `GATE_ROOT` (default git toplevel) and exit 0 / 1 / 2 as defined
  in "Config, not literals".
- Consumers pin an exact npm version or a locked flake input, never a range.
  Bumps arrive only by Renovate PR.
- First-party pins skip the release-age cooldown wherever one applies (OQ5):
  `@rigelbuild/*` npm packages in `bunfig.toml` (exact names, one entry each)
  and in Renovate, and `RigelBuild/*` git-refs pins in Renovate. Go modules and
  nix inputs have no package-manager cooldown, and compass has no first-party
  Go dependency today.
- A consumer's switch PR deletes its local copy in the same PR. No vendored
  copy, wrapper, or re-export stays behind.
- No consumer literal in shared code, tests, comments, or docs.

## Plan

Order: T1, T2, T3, then T4, T6, and T10 in parallel, then T5, then T7, T8, T9.
T3 comes first among the tools because it guards every later shared-repo PR.
T5 follows T4 because it imports `LedgerConfig` from the T4 package. RIG-4184's
scope is T4, T5, and the ledger part of T7 and T8.

### T1 — Create the repo

Lands in: the org's GitHub IaC. A public repo `RigelBuild/repo-tools` with
default branch `main`, a ruleset that requires a PR, Matt's CODEOWNERS review,
and green CI, merging through the Trunk queue as compass does.

Interfaces: produces the empty repo.

### T2 — Scaffold and release lane

Lands in: the shared repo. A bun workspace over `packages/*`, a `flake.nix`,
moon, biome, rumdl, the licence files (after the outside-contribution check),
a GitHub Actions CI that runs typecheck, lint, and test per package plus
`nix flake check`, and the release lane: release-please per package, then
`npm publish --provenance` through trusted publishing, after the bootstrap
publish of each package.

Interfaces: each `packages/<tool>/package.json` has
`"name": "@rigelbuild/<tool>"` and `"bin": { "<tool>": "./index.ts" }`.

### T3 — `ref-gate`

Lands in: the shared repo. Merge compass's `sea-ref-gate` and private-name gate
into one config-driven gate. Turn it on as the shared repo's own required check
in the same PR.

Interfaces:

```ts
export interface RefGateConfig {
  prefilter: { ere: string; ignoreCase: boolean }; // POSIX ERE handed to git grep
  patterns: readonly { source: string; flags: string }[]; // JS RegExp, applied per hit
  ignore: readonly string[]; // compound names stripped before matching, e.g. a gate's own name
  carveOutPaths: readonly string[];
  carveOutPrefixes: readonly string[];
  allowlist: Readonly<Record<string, string>>; // path -> reason
  remediationDoc?: string;
}
export function loadRefGateConfig(path: string): RefGateConfig; // throws on unknown keys
export function findViolations(config: RefGateConfig, grepHits: readonly string[]): Reference[];
export async function runOnce(deps: Deps, config: RefGateConfig): Promise<number>;
```

The coarse git-grep search is its own POSIX ERE, not derived from the JS
patterns. A JS-only construct such as `\b` in a git-grep ERE can match nothing,
and the gate then passes when it should fail.

CLI: `ref-gate --config <path>`.

### T4 — `design-ledger-gate`

Lands in: the shared repo. Port compass's gate. Add a list of ledgers (each
with its own surface), malformed-row reporting, and the three extra legs behind
config (OQ3: yes). Tests: compass's tests plus synthetic multi-ledger fixtures. The
private copy's test cases are the specification for those fixtures, written
again as synthetic cases.

Interfaces:

```ts
export interface LedgerEntry {
  path: string; // e.g. "docs/designs/DECISIONS.md"
  surface: string; // counter surface for IDs in this ledger
  governedRoots: readonly string[]; // record dirs this ledger governs
}
export interface LedgerConfig {
  ledgers: readonly LedgerEntry[]; // compass: one entry
  historicalChain?: readonly string[]; // record paths that must be Historical
  exemptBranchPrefixes?: readonly string[]; // default ["renovate/", "trunk-merge/"]
  legs?: { citations?: boolean; errata?: boolean; recordLinks?: boolean }; // each default false
  counter: { url: string; partition: string };
}
export function loadLedgerConfig(path: string): LedgerConfig; // throws on unknown keys
export async function runOnce(deps: Deps, config: LedgerConfig): Promise<number>;
```

CLI: `design-ledger-gate --config docs/designs/ledger.config.json`.

### T5 — `dl-claim` and `dl-reconcile`

Lands in: the shared repo. Both depend on `@rigelbuild/design-ledger-gate` for
`LedgerConfig` and `loadLedgerConfig`, so the loader has one copy. Reconcile
keeps compass's guards (raw-row cross-count, empty-frontier refusal, `--check`)
and adds the stale and duplicate claim report. Messages that name the counter
take its URL from config.

Interfaces:

```ts
// dl-claim
export interface ClaimRequest { repo: string; surface: string; ref: string; lane: string; count: number }
export function buildClaimBody(config: LedgerConfig, args: ClaimArgs): ClaimRequest;
// dl-reconcile
export interface LandedDecision { id: string; surface: string; ref: string }
export interface ReconcileRequest { repo: string; landed: LandedDecision[] }
export function assertReconcilableLedgers(
  config: LedgerConfig,
  ledgers: ReadonlyMap<string, string>, // ledger path -> text
): ReconcileRequest;
```

CLIs: `dl-claim --config <path> --ref <RIG-n|none> --lane <branch> [--count 1..10] [--surface <s>]`
and `dl-reconcile --config <path> [--check]`, both reading `DL_CLAIM_TOKEN`.
`--surface` is required when the config lists more than one ledger. `repo` is
always `counter.partition`. Consumes the T4 release.

### T6 — `renovate-preflight`

Lands in: the shared repo. Move compass's copy unchanged except package
metadata.

Interfaces: CLI `renovate-preflight`, env `REPO` and `RENOVATE_TOKEN`, exit
codes as today; `classify(probe: ProbeResult): PreflightResult` stays exported.

### T7 — Compass cutover

Lands in: compass. Add the five packages at exact versions. Add
`docs/designs/ledger.config.json`: one ledger (`docs/designs/DECISIONS.md`,
surface `designs`, the seven current buckets `ui`, `agent`, `server`, `meta`,
`infra`, `observability`, `repo`), an empty historical chain, `renovate/` and
`trunk-merge/` exempt, all legs off, partition `compass`, url
`https://dl.rigel.build`.

Add two code-free moon projects:

- `tools/design-ledger/`: `check` and `ci` tasks call the `design-ledger-gate`
  and `dl-reconcile --check` bins. `tools/ci-matrix/index.ts` injects
  `ALWAYS_RUN_ON_PR` by moon project id, and it injects nothing, with no error,
  when the project is missing. So keep the id `design-ledger-gate`, or update
  `ALWAYS_RUN_ON_PR` and its test in the same PR.
- `tools/ref-gates/`: `moon.yml` plus the configs `sea.json` and
  `private-name.json`. Each config carves out `tools/ref-gates/`. The
  private-name config keeps the deleted tool's compound name in `ignore`,
  because existing records still cite that path.

Point the moon tasks, `.github/workflows/dl-reconcile.yml`, the Renovate
preflight step, `.moon/workspace.yml`, `docs/designs/CONTRIBUTING.md` §7, and
`docs/concepts/self-host-and-managed.md` (which names the old ref-gate task) at
the new bins and projects. The two secret-holding jobs gain a
`bun install --frozen-lockfile` step (OQ4). Exempt first-party pins from the
cooldown (OQ5). In `bunfig.toml`, add each new `@rigelbuild/*` package by
exact name to `minimumReleaseAgeExcludes`. In Renovate, add those names to the
catalog soak-exemption rule, which `config.test.ts` requires to match the
catalog part of the bunfig list. Add a rule exempting the
`@rigelbuild/solid-*` pins in `apps/ui/package.json`, which still soak today.
Confirm every `RigelBuild/*` git-refs rule already sets
`minimumReleaseAge: null`. Delete
`tools/design-ledger-gate/`, `tools/dl-claim/`, `tools/dl-reconcile/`,
`tools/sea-ref-gate/`, `tools/orion-ref-gate/`, and `tools/renovate-preflight/`.

Acceptance:

- A seeded corpus, as its own git repo, with at least one known violation per
  gate leg and one branch per exempt prefix. Old and new tools report the same
  set of `file:line` pairs and the same pass or fail result. Only the old ledger
  gate reads `GATE_ROOT`. Run the old ref gates with cwd set to the corpus, and
  compare old reconcile through its exported `assertReconcilableLedger` on the
  corpus ledger. The intended changes (the `trunk-merge/` exemption, the tool
  name in output) are stated exceptions. A clean tree cannot fail this check,
  so it does not count.
- On a docs-only PR, the ci-matrix output still contains the ledger gate target.

Record both results in the PR body.

Interfaces: consumes the T3–T6 releases; produces the config files above.

### T8 — Private consumer cutover

Lands in: the private consumer. The same shape as T7: its own
`ledger.config.json` (one entry per surface ledger, all legs on, its own
partition), its own reference-gate config, CI pointed at the bins, and its
local copies deleted. The same seeded-corpus acceptance as T7.

Interfaces: consumes the T3–T6 releases.

### T9 — Compass turns on the extra ledger legs

Lands in: compass. Run the citation, errata, and record-link legs over the
corpus, fix every finding, and turn the legs on in the same PR.

Interfaces: consumes T4; edits `legs` in `docs/designs/ledger.config.json`.

### T10 — Shared Nix tooling

Lands in: the shared repo, then each consumer. Inventory the Nix files both
repos carry and measure their drift, as the Inventory table does for the TS
tools. Move each shared file under `nix/` in the shared flake as an output. A
file that holds a consumer literal takes it as a function argument instead.
In compass, add the shared repo as a `devenv.yaml` input. Then add a
`custom.regex` git-refs Renovate rule with a `devenv.lock` relock
postUpgradeTask, the same shape as the existing devenv-fork rule, with
`minimumReleaseAge: null`. Switch the imports and delete each local copy in the
same PR.

Acceptance: the consumer's nixos/devenv evaluation and its toolchain-parity
and flake-parity gates pass unchanged, and the store paths of the moved
outputs match those from before the move.

Interfaces: consumes T2's `flake.nix`; produces flake outputs under `nix/`.

### Out of scope

Later records: a shared Renovate preset and the Renovate upgrade scripts. The
moon task template and the root checks stay local (see Inventory). Wiring the
touch-coupling leg into compass CI is separate work. The DL counter service
(`dl.rigel.build`) does not move.

## Tasks

- [ ] T1 — Create `RigelBuild/repo-tools` through the org's GitHub IaC.
- [ ] T2 — Scaffold the shared repo and its release lane.
- [ ] T3 — Ship `ref-gate` and make it the shared repo's required check.
- [ ] T4 — Port `design-ledger-gate` onto a list of ledgers.
- [ ] T5 — Port `dl-claim` and `dl-reconcile` onto `LedgerConfig`.
- [ ] T6 — Move `renovate-preflight`.
- [ ] T7 — Cut compass over and delete its six local tools.
- [ ] T8 — Cut the private consumer over and delete its local copies.
- [ ] T9 — Turn on compass's extra ledger legs.
- [ ] T10 — Move the shared Nix tooling into the flake and cut both consumers over.

## Decisions (Matt, 2026-10-04, RIG-4440)

- **OQ1 — pin mechanism:** npm packages for the TS tools. Matt added Nix: the
  shared repo is also a flake for shared Nix tooling (T10).
- **OQ2 — names:** npm scope `@rigelbuild/<tool>`. The repo name
  `RigelBuild/repo-tools` was not ruled separately; it stands unless Matt
  changes it at review.
- **OQ3 — publish the private-only ledger-gate features:** yes, written again
  as public code with synthetic fixtures.
- **OQ4 — install in secret-holding jobs:** a full
  `bun install --frozen-lockfile`.
- **OQ5 — cooldown:** every first-party pin skips the release-age cooldown, in
  every toolchain (TS, Go, Nix).
- **OQ6 — licence:** relicense to `MIT OR Apache-2.0`.
