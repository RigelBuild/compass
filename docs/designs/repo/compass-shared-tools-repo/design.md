# Share repo tools from one public repo (RIG-4351)

## Problem / Intent

Compass and one private consumer repo each keep their own copy of the same repo
tools, and the copies drift. A fix lands in one copy and not the other. Example:
the private consumer's design-ledger gate exempts Trunk merge-queue branches
(`trunk-merge/`), but compass's copy does not, though compass also lands through
the Trunk queue.

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

## Approach

### One public repo, one package per tool

Create `RigelBuild/repo-tools` (working name, OQ2). It is a bun workspace with
one package per tool under `packages/<tool>/`, published as `@rigelbuild/<tool>`
with a bin of the same name. Each consumer pins exact versions and deletes its
own copy in the same PR. There is no shim and no re-export.

The port base is the compass copy. It is already public, so starting from it
moves nothing private. Features only the private copy has (per-surface ledgers,
the citation, errata, and record-link legs, malformed-row reporting) are written
again as new public code on top of it, subject to OQ3.

### Config, not literals

The two repos differ in real ways. Compass has one ledger and a fixed bucket
list. The private consumer has several per-surface ledgers. Each repo claims DL
IDs from its own counter partition. So no consumer value is a literal in shared
code. Each consumer keeps one config file, `docs/designs/ledger.config.json`,
which the gate, claim, and reconcile tools all read. The reference gate reads
its own config file.

The CLIs keep their current environment inputs (`GATE_ROOT`, `REPO`,
`PR_NUMBER`, `GH_TOKEN`, `DL_CLAIM_TOKEN`, `RENOVATE_TOKEN`) and exit codes (0
pass, 1 violations, 2 usage or internal error). CI wiring then changes only the
command it runs.

### Pinning (OQ1)

| Option | How a consumer pins | For | Against |
| --- | --- | --- | --- |
| npm package (recommended) | Exact version in the bun catalog; `bun.lock` keeps the integrity hash | Both repos already take tools as bun dependencies. Compass's Renovate catalog manager already reads npm versions. The release-age cooldown applies. One package per tool. | Needs a publish lane in the shared repo and a one-time npm org setup |
| bun git dependency | `github:RigelBuild/repo-tools#<sha>` in the root `package.json` | No registry and no publish lane | One package for the whole repo (bun installs a git repo root, not a subdirectory). Compass's catalog manager reads only npm versions. A git ref has no release timestamp, so the cooldown cannot apply. |
| Rev-pin JSON | A `{repo, ref, rev}` file plus a fetch step before each run. The private consumer already pins compass this way. | Precedent exists, and compass's Renovate config already bumps git revs with a regex manager | Each consumer writes its own fetch step. The tools' own npm dependencies need a separate install. No typed imports. Local runs need the fetch too. Same cooldown gap as the git dependency. |
| Nix flake input | Flake input; `flake.lock` keeps the narHash | Content-addressed and nix-native | The tools run under bun with npm dependencies (micromark), so each needs a nix package build. moon and `tsc` cannot typecheck against a store path. |

Recommendation: npm. A tool bump is then an ordinary catalog PR with release
notes, under the same release-age cooldown as every other dependency. Compass's
Renovate config nulls that cooldown on its git-refs rules because a git ref has
no release timestamp (`tools/renovate/config.json5`), so both git options lose
it. Publishing uses npm trusted publishing (OIDC from the shared repo's `main`
release workflow), so no long-lived publish token exists. The only manual step
is creating the npm org and its trusted-publisher entry. That step has no IaC
path, so it goes to Matt as a human-action issue.

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
pins, so the consumers converge on the newest release within one Renovate
cycle. Skew between them is a version number, not a fork. A fix lands as a
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
- Licence: dual MIT and Apache-2.0, matching compass.
- Runtime and checks: bun, TypeScript `strict` plus `noUncheckedIndexedAccess`,
  biome, `bun test`, rumdl for markdown.
- Each tool is the package `@rigelbuild/<tool>` under `packages/<tool>/`, with
  bin `<tool>` pointing at `./index.ts` (shebang `#!/usr/bin/env bun`).
- CLIs keep their current environment inputs and exit codes 0 / 1 / 2.
- Consumers pin an exact version (or an exact SHA if OQ1 picks a git option),
  never a range. Bumps arrive only by Renovate PR.
- A consumer's switch PR deletes its local copy in the same PR. No vendored
  copy, wrapper, or re-export stays behind.
- No consumer literal in shared code, tests, comments, or docs.

## Plan

Order: T1, T2, T3, then T4, T5, and T6 in parallel, then T7, T8, T9. T3 comes
first among the tools because it guards every later shared-repo PR. RIG-4184's
scope is T4, T5, and the ledger part of T7 and T8.

### T1 — Create the repo

Lands in: the org's GitHub IaC. A public repo `RigelBuild/repo-tools` with
default branch `main`, a ruleset that requires a PR, Matt's CODEOWNERS review,
and green CI, merging through the Trunk queue as compass does.

Interfaces: consumes OQ2; produces the empty repo.

### T2 — Scaffold and release lane

Lands in: the shared repo. A bun workspace over `packages/*`, moon, biome,
rumdl, the licence files, a GitHub Actions CI that runs typecheck, lint, and
test per package, and the release lane per OQ1. For npm: release-please per
package, then `npm publish --provenance` through trusted publishing.

Interfaces: each `packages/<tool>/package.json` has
`"name": "@rigelbuild/<tool>"` and `"bin": { "<tool>": "./index.ts" }`.

### T3 — `ref-gate`

Lands in: the shared repo. Merge compass's `sea-ref-gate` and private-name gate
into one config-driven gate. Turn it on as the shared repo's own required check
in the same PR.

Interfaces:

```ts
export interface RefGatePattern { source: string; flags: string } // RegExp parts
export interface RefGateConfig {
  patterns: readonly RefGatePattern[];
  ignore: readonly string[]; // compound names stripped before matching, e.g. a gate's own name
  carveOutPaths: readonly string[];
  carveOutPrefixes: readonly string[];
  remediationDoc?: string;
}
export function loadRefGateConfig(path: string): RefGateConfig; // throws on unknown keys
export function findViolations(config: RefGateConfig, grepHits: readonly string[]): Reference[];
export async function runOnce(deps: Deps, config: RefGateConfig): Promise<number>;
```

CLI: `ref-gate --config <path>`.

### T4 — `design-ledger-gate`

Lands in: the shared repo. Port compass's gate. Add `layout: "per-surface"`
(ledger at `<designsDir>/<surface>/DECISIONS.md`, found by glob), malformed-row
reporting, and the three extra legs behind config (OQ3). Tests: compass's tests
plus synthetic per-surface fixtures.

Interfaces:

```ts
export interface LedgerConfig {
  designsDir: string; // "docs/designs"
  layout: "single" | "per-surface";
  governedRoots?: readonly string[]; // single layout: the bucket list
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

Lands in: the shared repo. Both read `counter` and the ledger layout from
`LedgerConfig`. Reconcile keeps compass's guards (raw-row cross-count,
empty-frontier refusal, `--check`) and adds the stale and duplicate claim
report.

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
`--surface` is required in the per-surface layout; the single layout sends
`designs`. `repo` is always `counter.partition`.

### T6 — `renovate-preflight`

Lands in: the shared repo. Move compass's copy unchanged except package
metadata.

Interfaces: CLI `renovate-preflight`, env `REPO` and `RENOVATE_TOKEN`, exit
codes as today; `classify(probe: ProbeResult): PreflightResult` stays exported.

### T7 — Compass cutover

Lands in: compass. Add the five packages at exact versions. Add
`docs/designs/ledger.config.json`: single layout, the seven current buckets
(`ui`, `agent`, `server`, `meta`, `infra`, `observability`, `repo`), an empty
historical chain, `renovate/` and `trunk-merge/` exempt, all legs off,
partition `compass`, url `https://dl.rigel.build`. Add a code-free moon project
`tools/ref-gates/` holding `moon.yml` and the two configs `sea.json` and
`private-name.json`. Point the moon tasks, `.github/workflows/dl-reconcile.yml`,
the `design-ledger-gate:ci` target injected in `.github/workflows/ci.yml`,
`.moon/workspace.yml`, and `docs/designs/CONTRIBUTING.md` §7 at the package
bins. Delete `tools/design-ledger-gate/`, `tools/dl-claim/`,
`tools/dl-reconcile/`, `tools/sea-ref-gate/`, `tools/orion-ref-gate/`, and
`tools/renovate-preflight/`.

Acceptance: on the cutover commit, the old and new gates give the same exit
code and the same violation lines on compass's tree. Run both once and record
the result in the PR body.

Interfaces: consumes the T3–T6 releases; produces the config files above.

### T8 — Private consumer cutover

Lands in: the private consumer. The same shape as T7: its own
`ledger.config.json` (per-surface layout, all legs on, its own partition), its
own reference-gate config, CI pointed at the bins, and its local copies
deleted. The same parity acceptance as T7.

Interfaces: consumes the T3–T6 releases.

### T9 — Compass turns on the extra ledger legs

Lands in: compass. Run the citation, errata, and record-link legs over the
corpus, fix every finding, and turn the legs on in the same PR.

Interfaces: consumes T4; edits `legs` in `docs/designs/ledger.config.json`.

### Out of scope

Later records: a shared Renovate preset, the moon task templates, and the root
checks. The DL counter service (`dl.rigel.build`) does not move.

## Tasks

- [ ] T1 — Create `RigelBuild/repo-tools` through the org's GitHub IaC.
- [ ] T2 — Scaffold the shared repo and its release lane.
- [ ] T3 — Ship `ref-gate` and make it the shared repo's required check.
- [ ] T4 — Port `design-ledger-gate` with both ledger layouts.
- [ ] T5 — Port `dl-claim` and `dl-reconcile` onto `LedgerConfig`.
- [ ] T6 — Move `renovate-preflight`.
- [ ] T7 — Cut compass over and delete its six local tools.
- [ ] T8 — Cut the private consumer over and delete its local copies.
- [ ] T9 — Turn on compass's extra ledger legs.

## Open Questions

- **OQ1 (load-bearing; blocks T2, T7, T8) — pin mechanism.** npm package
  (recommended), bun git dependency, or rev-pin JSON. See "Pinning".
- **OQ2 (load-bearing; blocks T1) — repo and scope names.** Working names:
  `RigelBuild/repo-tools` and `@rigelbuild/<tool>`.
- **OQ3 (load-bearing; blocks the extra legs in T4) — publish the private-only
  ledger-gate features.** The per-surface layout, the citation, errata, and
  record-link legs, and malformed-row reporting exist only in the private
  copy, and publishing private code is Matt's call. Recommendation: yes. They
  are generic checks over a markdown corpus; write them again as public code
  with synthetic fixtures. If no, T4 ships compass's feature set only, and the
  private consumer keeps those legs locally, so part of the ledger gate stays
  duplicated.
