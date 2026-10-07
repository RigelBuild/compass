# Fail-closed Renovate relock for the devenv locks (RIG-3253)

## Problem / Intent

The devenv-lock relock family (`tools/renovate/refresh-devenv-lock.ts`, `refresh-devenv-nixpkgs.ts`, `refresh-agent-image-nixpkgs.ts`) "fails loud" by exiting non-zero when a relock goes wrong. Renovate does not stop on that exit. It still commits the regex-bumped rev, so a half-relocked lock (new `rev`, old `narHash` and `lastModified`) reaches a PR. Whether `rollup`, the one check `main` requires, then turns red depends on which node was half-relocked and on what the nix store holds:

- **Root `devenv` fork node.** Nothing in CI reads its `narHash` (see Evidence). A half-relock there passes every required check.
- **Other bumped nodes.** A fixed-output fetch in a required leg reads each of them. [INFERENCE] On a cold CI runner they fail with a hash mismatch, unless a substituter already holds the old source path. A warm store, as on a dev box, passes them silently (table rows 2-3). This is not measured in CI; T0 measures it.

Intent: make an inconsistent devenv lock turn `rollup` red, whoever wrote it (Renovate, a hand edit, a bad merge) and whatever the store holds. The error must name the lock, the node and the field. Then correct the in-code comments that say the relock exit already blocks the merge.

### Evidence (verified this session)

Renovate is pinned at `bunx renovate@44.46.2` (`.github/workflows/renovate.yml`). These claims were read from that exact npm tarball (`dist/`):

- A failing postUpgradeTask is caught and added to `artifactErrors`, and the run continues (`postUpgradeCommandsExecutor`, `workers/repository/update/branch/execute-post-upgrade-commands.js:112-116`).
- The only branch abort is `throw new Error(MANAGER_LOCKFILE_ERROR)`, inside `if (config.releaseTimestamp)`. It fires only for a release under 2 h old on a branch that does not exist yet. Otherwise the code logs `"PR has no releaseTimestamp"` and continues (`workers/repository/update/branch/index.js:408-417`).
- A `git-refs` release is `{version, gitRef, newDigest}` and never has a `releaseTimestamp` (`modules/datasource/git-refs/index.js`). `github-tags` does have one (`modules/datasource/github-tags/index.js:14,51-56`).
- The commit is `updatedPackageFiles.concat(updatedArtifacts)` from memory (`commitFilesToBranch`, `branch/commit.js`). `prepareCommit` (`util/git/index.js`) runs `reset --hard` and `clean -fd`, then writes those files again, so a task cannot undo the regex bump.
- `renovate/artifacts` is set only on failure: `setArtifactErrorStatus` returns early when `mode === "failed" && !hasErrors` (`branch/artifacts.js:12`). The default mode is `statusCheckWhen.artifactError: "failed"` (`config/options/index.js:372-382`). Compass overrides neither `statusCheckWhen` nor `statusCheckNames`.

Repo and GitHub side:

- Compass ruleset `20090117` ("main") requires exactly one context, `rollup` (`gh api repos/RigelBuild/compass/rulesets/20090117`), which is the `rollup` job (`.github/workflows/ci.yml:2526`). `renovate/artifacts` is not required. RIG-3253 names the required context `CI`, which is wrong. `CI` is the fork's required context.
- Line 29 of the `refresh-devenv-lock.ts` header says the opposite: "fail-closed rests on that required check plus human review (no automerge)."
- `ci.yml` runs on `pull_request` with no `paths:` filter. Every running `moon` leg first builds `tools/toolchain/gate-tools.nix` (step `Put the language toolchains on PATH`). On every PR at least the bun leg runs (`injectAlwaysRunTarget`, `tools/ci-matrix/index.ts`).

What already reads each bumped node's `narHash` in a required leg:

| Relock script → node | Reader |
|---|---|
| `refresh-devenv-nixpkgs.ts` → root `nixpkgs` | `gate-tools.nix`: `builtins.fetchTarball { …; sha256 = node.narHash; }` |
| `refresh-go-overlay.ts` → root `go-overlay` | `gate-tools.nix`: `goOverlayNode`, same pattern |
| `refresh-devenv-lock.ts` → root `devenv` | **None.** `devenvSource` (`tools/toolchain/devenv-cli/core.ts`) reads only `{owner, repo, rev}`, and "compass CI never provisions a PATH `devenv`" (`renovate.yml` header). |
| `refresh-devenv-lock.ts` → agent-image `devenv` | `compass-agent-image:build` (`agent-image/moon.yml`: `ci-group.nix`, inputs `**/*`) runs devenv in `agent-image/`. [INFERENCE] devenv verifies each node's `narHash` there. |
| `refresh-agent-image-nixpkgs.ts` → agent-image `nixpkgs` | The same `build` task |

Nix behaviour, measured locally (nix 2.34.8) with the real fork revs (root `984a4a42…`, agent-image `15a81f3e…`):

| Probe | Stale `narHash` result |
|---|---|
| `builtins.fetchTree { type = "github"; …; narHash; }`, clean fetcher cache | `error: NAR hash mismatch in input 'github:RigelBuild/devenv/984a…'` (exit 102). A wrong `lastModified` also fails. |
| `builtins.fetchTarball { sha256 = node.narHash; }` (the `tools/toolchain/*-env.nix` pattern), warm store | Silent pass. Returns the other rev's store path. |
| Flake lock with a `flake = false` input that has a stale `narHash`, warm store | Silent pass. It also writes a fetcher-cache row mapping rev → stale hash. After that, `nix flake prefetch` (even with `--refresh`) and `fetchTree` both return the stale hash and exit 0. |
| `nix flake prefetch --json github:<o>/<r>/<rev>` with a fresh absolute cache dir | Correct `hash` and `locked.lastModified`, even when the default cache is poisoned. |

More measurements:

- Every github node in both locks (26 unique refs) reproduces its lock's `narHash` and `lastModified` exactly. A cold sweep took 54 s one at a time, 64 s with 8 in parallel, and used a 172 MB cache.
- `NIX_CACHE_HOME` takes precedence over `XDG_CACHE_HOME`. With both set to different dirs, nix wrote only to `NIX_CACHE_HOME`.
- `cachix/install-nix-action` v31 configures `access-tokens = github.com=$GITHUB_TOKEN`. With a token configured, nix fetches `https://api.github.com/repos/<o>/<r>/tarball/<rev>`. Appending `access-tokens =` to `NIX_CONFIG` makes it fetch `https://github.com/<o>/<r>/archive/<rev>.tar.gz` instead. Measured with a fake token in a scratch `NIX_CONF_DIR`: the API path gave `HTTP error 401`, the cleared run gave 200. The REST limit for `GITHUB_TOKEN` is "1,000 requests per hour per repository" (docs.github.com, "Rate limits for the REST API").

## Approach

**Recommendation: option (b).** Add a CI gate, `renovate:lock-integrity`, that recomputes each locked github node's `narHash` and `lastModified` from its `(owner, repo, rev)`. It runs inside the required `rollup`.

**What the gate adds, given the Evidence.** (1) It covers the root `devenv` fork node, which nothing checks today, and any node no fetch consumes. (2) It does not depend on the store: a cold runner, a warm dev box and a substituter holding the old path all give the same answer. (3) Its diagnostic names the lock, node, field, expected value and actual value; today a cold failure surfaces as a fetch error in an unrelated leg. (4) It covers hand edits and future relock scripts without coupling to the manager list in `config.json5`. If T0 shows cold CI already reds every other shape, (2)-(4) still hold. Alternative (d) is the narrow version.

**What counts as "inconsistent".** Take a lock `L` in `DEVENV_LOCK_PATHS` (`tools/renovate/refresh-devenv-lock.core.ts:35-37`; today `devenv.lock` and `agent-image/devenv.lock`) and a node `N` in `L.nodes` that has a `locked` field. `N` is inconsistent when `N.locked.narHash` differs from the prefetched `hash`, or `N.locked.lastModified` differs from the prefetched `locked.lastModified`.

A node that cannot be verified is a failure, never a skip: a missing field, a rev that is not 40-hex, a failed prefetch, or a `locked.type` other than `github` (every node is `github` today). For a non-github type the error says so: a `path:` or `git+` input fails every `renovate:ci` run until the gate is extended to verify that type.

**How the expected hash is computed.** A NAR hash covers the source tree, so the gate must fetch it; it cannot work offline. Each unique ref costs one GitHub download. Every `moon` CI leg already has nix and network (`cachix/install-nix-action`, `ci.yml:347`).

The gate appends `access-tokens =` to `NIX_CONFIG` for its own nix calls. Fetches then use the public archive URL, not the metered REST API, which stops the gate spending the repo's 1,000/hour `GITHUB_TOKEN` budget. [INFERENCE] The archive URLs have their own limits, but GitHub does not publish them. Every node is public today. A private node would fail as unverifiable, which is the safe direction.

**Retry posture.** There is one bounded retry, and only for a transient signature in nix's stderr: `HTTP error 429`, `HTTP error 5xx`, or `rate limit`. The gate waits 60 s and retries once. A hash mismatch, a 401/404, or any other error is never retried. That keeps the gate failing closed on integrity, without failing closed on a GitHub blip. Nix also retries transient downloads itself (`download-attempts`, 5 locally); [INFERENCE] which statuses it treats as transient is not verified.

**Which nodes on which run** (OQ3, ruled diff-only on PRs):

- **PR run** (`GITHUB_EVENT_NAME=pull_request` and `GITHUB_BASE_REF` set): find the merge-base with `git merge-base origin/$GITHUB_BASE_REF HEAD`. Check nodes whose whole `locked` object differs from the same-named node there, plus nodes that are new. The diff is not keyed on `rev`, so a `narHash`-only edit is still checked. A lock that is absent at the merge-base counts as all-new. An unresolvable base is a failure. A PR that leaves both locks byte-identical makes no network call. The moon job checks out with `fetch-depth: 0` (`ci.yml:341`).
- **Every other run** (push to `main`, nightly, `workflow_dispatch`, local): full sweep. A full sweep took about 1 minute locally; CI time is not measured. It also catches drift on `main`, such as a fork rev that became unfetchable.

**Why a fresh cache.** A warm fetcher cache can map a rev to the wrong hash, and nix's own check then passes silently (table row 3). The gate sets both `NIX_CACHE_HOME` and `XDG_CACHE_HOME` to a fresh absolute `mkdtemp` dir for each run, and deletes it afterwards. `NIX_CACHE_HOME` takes precedence, so a dev box that exports it cannot route the gate to a poisoned cache.

**Where it lives.** The shape follows `flake-parity`: a pure, unit-tested core plus a thin I/O shell (`tools/toolchain/flake-parity-core.ts`, `flake-parity.ts`). The gate goes in the existing `renovate` project (`tools/renovate`, `ci-group.bun`):

- It reuses `DEVENV_LOCK_PATHS` as the single lock registry.
- Scheduling needs no new trigger. On a PR, `tools/ci-matrix/index.ts` `main()` unions `moon query projects --affected` with `moon query tasks --affected` (`unionAffectedIds`, `parseTaskAffectedIds`). Two task inputs already schedule it: the gate's own inputs and `renovate:test`'s (`/devenv.lock`, `/agent-image/devenv.lock`, `/package.json`, `/bun.lock`, `/bunfig.toml`, `renovate.yml`). `Moon battery` then runs every dependency of `renovate:ci`.
- Unlike `flake-gate`, it does not pull `nix flake check` into agent-image lock PRs. Unlike `toolchain-parity`, it does not depend on `root`. No new project or abstraction is added.

**Wiring.** Moon task `renovate:lock-integrity`, added to the dependencies of `renovate:ci`. CI: job `moon`, leg `moon (bun)`, step `Moon battery` (`ci.yml:474`), rolled up into `rollup`. Locally, hk's pre-push `moon ci` (`hk.pkl:34`) runs the gate whenever it or `renovate:ci` is affected. With no `GITHUB_BASE_REF`, that is a full sweep: about 1 minute, network, and a 172 MB temp cache.

**What this does not cover.** It checks that a lock is internally consistent, not that a commit is trustworthy. A malicious but correctly relocked fork commit passes. That is OQ2.

## Alternatives considered

- **(a) Make `renovate/artifacts` a required context on ruleset 20090117.** Rejected. Renovate posts that status only on failure (`artifacts.js:12`), so every non-Renovate PR would wait forever on "Expected — Waiting for status to be reported". `statusCheckWhen.artifactError: "always"` fixes only Renovate's own PRs and misses hand edits. Rulesets have no IaC rail (OQ-N3, `docs/designs/infra/release/compass-unified-release-lane/design.md`).
- **(b) A lock-integrity gate inside the required `rollup`.** CHOSEN. It is declarative and reviewed in a PR, does not depend on store state, covers hand edits, and is one task in an existing project.
- **(c) Make Renovate refuse to open the branch or PR.** Rejected; it cannot fail closed. The only abort sits in the `releaseTimestamp` arm and lapses at 2 h (`branch/index.js:408-417`), `git-refs` has no timestamp, and `prepareCommit` rewrites the regex bump. `prCreation: "status-success"` (`update/pr/index.js:118-126`) only withholds the PR; the branch is still pushed.
- **(d) A narrow gate on the root `devenv` node only.** Rejected. It closes the one gap no reader covers, but leaves the other shapes dependent on store and substituter state (unmeasured until T0) and their opaque errors. The general gate is the same code without a node filter, and under OQ3's PR diff it fetches nothing for unchanged nodes.

## Global Constraints

- **Versions.** Renovate claims are checked against `renovate@44.46.2`. CI nix is `cachix/install-nix-action` v31 (nix 2.35.2). The local measurements used 2.34.8.
- **Nix invocation.** `nix flake prefetch --json --extra-experimental-features "nix-command flakes" github:<owner>/<repo>/<rev>`, with this env:
  - `NIX_CACHE_HOME` and `XDG_CACHE_HOME`: the same fresh absolute `mkdtemp` dir. Relative paths are rejected (`error: not an absolute path`). Delete the dir in `finally`.
  - `NIX_CONFIG`: the inherited value plus `\naccess-tokens =`.
- **Fail-closed rules.** Unverifiable means failure. There is no fallback to a warm cache, and the only retry is the single transient-signature retry. Exit `0` when every checked node is consistent, `1` otherwise.
- **Code shape.** Pure core: no I/O, no `process`, no `Bun`. Thin shell: reads files, runs git and nix, prints, exits. Files follow the `<name>.core.ts` / `<name>.core.test.ts` convention in `tools/renovate`.
- **Comments.** 1-2 lines, 4 at most, with no issue IDs. Relock-family changes touch comments and messages only, and keep the `"byte-identical"` substring the tests assert.
- **Lint.** Run `biome check` on the touched TS/JSON files only, and `rumdl check` on touched Markdown.
- **Ledger.** A PR touching this record also touches `docs/designs/DECISIONS.md`, or declares `Ledger-impact:` (touch-coupling in `tools/design-ledger-gate/index.ts`).

## Plan

### T0: Measure existing coverage (no code)

Open one draft PR off `main` for each relock shape in the Evidence table. Each PR changes only that node's `rev`, to a real rev of the same repo, and leaves `narHash` and `lastModified` alone:

- root `nixpkgs` → `6004ea…` (the agent-image nixpkgs rev);
- root `devenv` → `15a81f3e…`;
- agent-image `devenv` → `984a4a42…`;
- agent-image `nixpkgs` → `c946ff36…` (the root nixpkgs rev);
- root `go-overlay` → any other go-overlay commit.

For each PR, record which `moon` legs ran, whether `rollup` went red, and the first error line. Never promote these drafts. Close each one unmerged and delete its branch. Post the five-row result as one comment on RIG-3253. T0 does not block T1-T3; it confirms or corrects the [INFERENCE] in the Problem.

### T1: Pure core and unit tests

Files: `tools/renovate/devenv-lock-integrity.core.ts` and `.core.test.ts`. They run under `renovate:test`.

```ts
export interface LockedGithubNode {
  readonly node: string;         // key in lock.nodes, e.g. "devenv"
  readonly owner: string;
  readonly repo: string;
  readonly rev: string;          // 40-hex, validated
  readonly narHash: string;      // "sha256-…"
  readonly lastModified: number;
}
export interface Prefetched { readonly narHash: string; readonly lastModified: number }
export interface IntegrityReport { readonly ok: boolean; readonly report: string }

/** Every node with `locked`. Throws naming the node on an ill-formed field, or on a non-github type ("extend lock-integrity to verify it"). */
export function lockedGithubNodes(lockText: string): readonly LockedGithubNode[];
/** Nodes whose `locked` object is absent from or not deep-equal to base's; `baseText === null` returns all. */
export function changedNodeNames(baseText: string | null, headText: string): ReadonlySet<string>;
/** `github:<owner>/<repo>/<rev>`: the dedupe key and the prefetch argument. */
export function prefetchRef(n: LockedGithubNode): string;
/** Parses `nix flake prefetch --json` stdout (`.hash`, `.locked.lastModified`); throws on missing fields. */
export function parsePrefetch(stdout: string): Prefetched;
/** True only for `HTTP error 429`, `HTTP error 5xx` or `rate limit`; never for a hash mismatch. */
export function isTransientFetchError(stderr: string): boolean;
/** One line per node; failures name lock, node, field, expected and got. `ok` iff nothing mismatched or unverified. */
export function integrityReport(
  checks: readonly { lock: string; node: LockedGithubNode; observed: Prefetched | { error: string } }[],
): IntegrityReport;
```

Test cycle: write the tests first, run `bun test tools/renovate/devenv-lock-integrity.core.test.ts`, and see them fail. Then implement until they pass. Cases:

- Both real locks parse. The `root` node, which has no `locked` field, is skipped.
- A wrong `narHash`, or a wrong `lastModified` alone, gives `ok: false`, and the report names the lock, the node and both values.
- An `{error}` result gives `ok: false`.
- A `"path"` type throws, and the message names the node and says to extend the gate.
- A missing `narHash` throws. `parsePrefetch` rejects JSON with no `hash` field.
- `changedNodeNames`: identical texts give an empty set; a rev-only change, a `narHash`-only change and a new node are each included; a `null` base gives every node.
- `isTransientFetchError`: `HTTP error 429` and `HTTP error 503` give true; `NAR hash mismatch`, `HTTP error 401` and `HTTP error 404` give false.

### T2: Thin shell, moon task, project header

Files: `tools/renovate/devenv-lock-integrity.ts` (modelled on `tools/toolchain/flake-parity.ts`) and `tools/renovate/moon.yml`.

The shell:

1. Choose PR mode when `GITHUB_EVENT_NAME === "pull_request"` and `GITHUB_BASE_REF` is non-empty; otherwise use the full sweep.
2. In PR mode, run `git merge-base origin/<base> HEAD`; if that fails, exit 1. For each lock, `git cat-file -e <mb>:<path>` decides between `git show` text and `null`.
3. Parse each lock with `lockedGithubNodes`. In PR mode, keep only the `changedNodeNames`. Deduplicate by `prefetchRef`, then fetch serially with the env from Global Constraints.
4. For a failure with `isTransientFetchError`, wait 60 s and retry once. Otherwise record `{error: <stderr tail>}`.
5. In `finally`, `rmSync` the dir. Print the mode, the count of nodes checked, and the report, then exit `ok ? 0 : 1`.

The moon task:

```yaml
  lock-integrity:
    command: 'bun tools/renovate/devenv-lock-integrity.ts'
    options:
      runFromWorkspaceRoot: true
      cache: false
      runInCI: true
    inputs: ['/devenv.lock', '/agent-image/devenv.lock', 'devenv-lock-integrity*.ts', 'refresh-devenv-lock.core.ts']
  ci:
    deps: ['typecheck', 'test', 'lock-integrity']
```

Also fix the stale header in `tools/renovate/moon.yml`. Today it says "Compass CI is a single moon-driven `CI` job (.github/workflows/ci.yml runs `moon run :ci`)". Change it to: CI runs per-group `moon (<group>)` legs, rolled up into the required `rollup`, and this project's `ci` runs in `moon (bun)` when affected.

Smoke test:

1. Clean tree, full sweep: exits 0.
2. Put the agent-image fork `narHash` (`sha256-zTzdShjH…`) into the root `devenv` node. The gate exits 1 and names `devenv.lock` / `devenv`. Then `jj restore devenv.lock`.
3. Run with `GITHUB_EVENT_NAME=pull_request GITHUB_BASE_REF=no-such-branch`: exits 1 on the unresolvable base.
4. Run with `NIX_CONF_DIR` and `NIX_USER_CONF_FILES` pointing at scratch files that hold a fake `access-tokens` entry. The gate still exits 0, which proves the clear overrides a configured token (the API path would return 401).
5. `moon run renovate:ci` runs all three dependencies. On the gate's own PR, the `moon (bun)` log shows PR mode with 0 nodes checked.

### T3: Correct the fail-closed claims in the relock family

Change only comments and messages, so they name the gate instead of "required check" or "human review stops the merge":

- `refresh-devenv-lock.ts`: header lines 26-29 and the byte-identical message (lines 122-125).
- `refresh-devenv-lock.core.ts`: the `changedDevenvLock` doc comment (lines 54-57) and its error (lines 75-77).
- `refresh-devenv-lock.test.ts`: the comment at lines 311-313.
- `refresh-agent-image-nixpkgs.ts`: header lines 20-22 and its byte-identical message (lines 157-160).

New wording: the exit turns `renovate/artifacts` red, which is advisory, and `renovate:lock-integrity` in the required `rollup` blocks the merge. Leave the `config.json5` `minimumReleaseAge: null` notes alone; they are about soak (OQ2).

Test cycle: `bun test tools/renovate/refresh-devenv-lock.test.ts tools/renovate/refresh-devenv-lock.core.test.ts tools/renovate/refresh-agent-image-nixpkgs.test.ts` stays green. Then run `biome check` on the touched files.

## Tasks

- [ ] T0: Five half-relocked draft PRs; the result goes in one RIG-3253 comment; close them unmerged.
- [ ] T1: `devenv-lock-integrity.core.ts` plus core tests (red, then green).
- [ ] T2: Shell, `renovate:lock-integrity` task wired into `renovate:ci`, `moon.yml` header fix, and smoke steps 1-5.
- [ ] T3: Relock-family comments and messages name the gate; the three relock tests stay green.

## Resolved decisions (Matt, 2026-10-06)

Ruled on RIG-4591: OQ1 (b), OQ2 keep, OQ3 diff-only on PRs. The record follows each recommendation below.

**OQ1. Which blocking mechanism?** Ruled: **(b) only**.

- (a) A ruleset context only.
- (b) The general gate only.
- (a) and (b) together.
- (d) A narrow gate on the root `devenv` node only.

Recommendation: **(b) only**. (a) cannot be required without blocking every PR Renovate did not open, and it needs a manual ruleset edit. (d) leaves the other shapes dependent on store state, and costs the same code. Under OQ3's recommendation, (b) costs nothing on PRs that leave both locks unchanged, and about 1 minute on a full sweep.

**OQ2. Should the fork keep tracking HEAD?** Ruled: **keep as is**. Both fork rules track `RigelBuild/devenv` `main` HEAD every day, with `minimumReleaseAge: null`. A git-refs digest has no timestamp to age.

- **Keep as is.** Human review of the compass PR is the control. The fork is first-party, and its ruleset `21184706` requires a PR with 1 approval and last-push approval, the `CI` check, and no bypass actors. Code-owner review is also set, but the fork has no CODEOWNERS file, so it adds nothing. Cost: none.
- **Track a tag.** Switch to `github-tags`, which has `releaseTimestamp`, so the global 5-day `minimumReleaseAge` applies. Cost: the fork has 0 tags, so someone must cut releases, and each fork fix waits 5 days.
- **`dependencyDashboardApproval: true` on the two fork rules.** No branch is created until someone ticks the box (`branch/index.js:139-143`). Cost: a second manual click that adds nothing beyond the PR review.

Recommendation: **keep as is**. Lock integrity was the gap, and (b) closes it. The rest is trust in first-party commits, which the fork's ruleset guards. A delay or an extra click does not strengthen that.

**OQ3. On PRs, which nodes does the gate check?** Ruled: **diff-only on PRs**.

- **Diff-only on PRs, full sweep otherwise.** On a PR, check nodes whose whole `locked` object differs from the merge-base, plus new nodes. Push, nightly, `workflow_dispatch` and local runs do the full sweep. Cost: drift already on `main` is caught by the next push or nightly run, not by PRs, and the gate depends on resolving the base ref (failing if it can't).
- **Every node, every run.** Simpler, with no git dependency. Cost: every `renovate:ci` PR (most `bun.lock` / `package.json` PRs) pays about 1 minute and about 26 network fetches on the required check.

Recommendation: **diff-only on PRs**. It puts network exposure only on PRs that change a lock, and the push/nightly sweep covers drift.
