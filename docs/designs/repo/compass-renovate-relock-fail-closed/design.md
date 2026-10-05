# Fail-closed Renovate relock for the devenv locks (RIG-3253)

## Problem / Intent

The devenv-lock relock family (`tools/renovate/refresh-devenv-lock.ts`, `refresh-devenv-nixpkgs.ts`, `refresh-agent-image-nixpkgs.ts`) "fails loud" by exiting non-zero when a relock goes wrong. That exit does not stop anything. Renovate still commits the regex-bumped rev, and nothing that `main` requires turns red. A half-relocked lock (new `rev`, old `narHash`) can reach a mergeable PR, held back only by human review.

Intent: make an inconsistent devenv lock turn the required check red, whoever wrote it (Renovate, a hand edit, or a bad merge). Then correct the in-code comments that claim this already happens.

### Evidence (verified this session)

Renovate is pinned at `bunx renovate@44.46.2` (`.github/workflows/renovate.yml`). These claims were read from that exact npm tarball (`dist/`), not from a nearby version:

- A failing postUpgradeTask is caught and added to `artifactErrors`, and the run continues (`postUpgradeCommandsExecutor`, `workers/repository/update/branch/execute-post-upgrade-commands.js:112-116`).
- The only branch abort is `throw new Error(MANAGER_LOCKFILE_ERROR)`. It sits inside `if (config.releaseTimestamp)`, so it fires only when a release timestamp exists, the branch does not exist yet, and the release is less than 2 h old. Otherwise the code logs `"PR has no releaseTimestamp"` and continues (`workers/repository/update/branch/index.js:408-417`).
- A `git-refs` release is `{version, gitRef, newDigest}`. It never has a `releaseTimestamp` (`modules/datasource/git-refs/index.js`, no match for `releaseTimestamp`). `github-tags` does have one (`modules/datasource/github-tags/index.js:14,51-56`).
- The commit is built from the in-memory `updatedPackageFiles.concat(updatedArtifacts)` (`commitFilesToBranch`, `branch/commit.js`). `prepareCommit` (`util/git/index.js`) runs `reset --hard` and `clean -fd`, then writes those files again. So a task cannot undo the regex bump.
- The status `renovate/artifacts` is set only on failure. `setArtifactErrorStatus` returns early when `mode === "failed" && !hasErrors` (`branch/artifacts.js:12`). The mode defaults to `statusCheckWhen.artifactError: "failed"` (`config/options/index.js:372-382`), and compass overrides neither `statusCheckWhen` nor `statusCheckNames`.

On the repo and GitHub side:

- Compass ruleset `20090117` ("main") requires exactly one status context, `rollup` (`gh api repos/RigelBuild/compass/rulesets/20090117`). That is the `rollup` job in `.github/workflows/ci.yml:2526`. The status `renovate/artifacts` is not required. RIG-3253 names the required context `CI`. That is wrong: the context is `rollup`.
- Line 29 of the `refresh-devenv-lock.ts` header says the opposite: "fail-closed rests on that required check plus human review (no automerge)."
- No existing gate reads the fork node's `narHash`. `devenvSource` (`tools/toolchain/devenv-cli/core.ts:23-79`) reads only `{owner, repo, rev}`. `nix run github:<owner>/<repo>/<rev>#devenv` resolves the rev without the lock. `flake-parity` compares only the nixpkgs rev.

Nix behaviour, measured locally (nix 2.34.8) with the real fork revs (root `984a4a42…`, agent-image `15a81f3e…`):

| Probe | Stale `narHash` result |
|---|---|
| `builtins.fetchTree { type = "github"; …; narHash; }`, clean fetcher cache | `error: NAR hash mismatch in input 'github:RigelBuild/devenv/984a…'` (exit 102). A wrong `lastModified` also fails (`'lastModified' attribute mismatch`). |
| `builtins.fetchTarball { sha256 = node.narHash; }` (the `tools/toolchain/*-env.nix` pattern), warm store | Silent pass. Returns the other rev's store path. |
| Flake lock with a `flake = false` input that has a stale `narHash`, warm store | Silent pass. It also writes a fetcher-cache row mapping rev → stale hash. After that, `nix flake prefetch` (even with `--refresh`) and `fetchTree` both return the stale hash and exit 0. |
| `nix flake prefetch --json github:<o>/<r>/<rev>` with a fresh absolute `XDG_CACHE_HOME` | Correct `hash` and `locked.lastModified`, even when the default cache is poisoned. |

Every github node in both locks (26 unique refs) reproduces its lock's `narHash` and `lastModified` exactly. A cold sweep took 54 s run one at a time, 64 s with 8 in parallel, and used a 172 MB cache.

## Approach

**Recommendation: option (b).** Add a CI gate that recomputes each locked node's `narHash` and `lastModified` from its `(owner, repo, rev)`. The gate runs inside the required `rollup`.

**What counts as "inconsistent".** Take a lock `L` in `DEVENV_LOCK_PATHS` (`tools/renovate/refresh-devenv-lock.core.ts:35-37`; today `devenv.lock` and `agent-image/devenv.lock`) and a node `N` in `L.nodes` that has a `locked` field. `N` is inconsistent when `N.locked.narHash` differs from the prefetched `hash`, or `N.locked.lastModified` differs from the prefetched `locked.lastModified`. The prefetch is `nix flake prefetch --json github:<owner>/<repo>/<rev>`, run with a fresh fetcher cache. A node that cannot be verified is a failure, never a skip. That covers a `locked.type` other than `github` (every node is `github` today), a missing field, a rev that is not 40-hex, and a prefetch that fails. A half-relock has exactly this shape: the regex manager moves `rev`, while `narHash` and `lastModified` stay at the base values.

**How the expected hash is computed.** The gate cannot compute it offline. A NAR hash is a hash of the source tree, so the tree has to be fetched. Each unique ref costs one GitHub tarball fetch. The gate gets `nix` and network the same way the existing jobs do. Every `moon` CI leg runs `cachix/install-nix-action` (`ci.yml:347`), whose default `access-tokens = github.com=$GITHUB_TOKEN` avoids rate limits. Locally the pre-push hook runs `moon ci` (`hk.pkl:34`), so a dev box needs network only when a lock changed.

**Why a fresh cache.** The table shows that a warm fetcher cache can map a rev to the wrong hash, and then nix's own verification passes silently. The gate sets `XDG_CACHE_HOME` to a fresh absolute `mkdtemp` directory for each run and deletes it afterwards. CI runners start empty anyway. This protects the local pre-push run.

**Scope: every github node, not only the nodes Renovate bumps.** Renovate's regex managers move only the `devenv` and `nixpkgs` nodes. Checking every node also catches hand edits and any future regex manager, without coupling the gate to the manager list in `config.json5`. A cold run took about 1 minute locally; CI timing is not measured. Once a PR schedules `renovate:ci`, `Moon battery` runs all of its dependencies, this gate included. That happens on PRs touching `tools/renovate`, either lock, `/package.json`, `/bun.lock`, `/bunfig.toml` or `renovate.yml` (the `renovate:test` inputs), plus every `main` push and the nightly run. `/package.json` and `/bun.lock` change often, so most dependency PRs pay the minute too. The shell deduplicates refs that appear in both locks.

**Where it lives.** It follows the `flake-parity` split: a pure, unit-tested core and a thin shell that does the I/O (`tools/toolchain/flake-parity-core.ts`, `flake-parity.ts`). It sits in the existing `renovate` moon project (`tools/renovate`, `ci-group.bun`):

- It reuses `DEVENV_LOCK_PATHS` as the single lock registry.
- `renovate:test` already lists `/devenv.lock` and `/agent-image/devenv.lock` as inputs (`tools/renovate/moon.yml:38,41`), and the ci-matrix scheduler takes the union of affected projects and affected tasks (`tools/ci-matrix/index.ts`). A lock-only PR therefore already schedules `renovate:ci`, so no new trigger is needed.
- Unlike `flake-gate`, adding it here does not pull `nix flake check` into agent-image lock PRs. Unlike `toolchain-parity`, this project does not depend on `root`, so the gate does not run on nearly every PR.
- No new project or abstraction is added.

**Wiring:**

- Moon task: `renovate:lock-integrity`, added to the dependencies of `renovate:ci`.
- CI step: job `moon`, leg `moon (bun)`, step `Moon battery` (`moon run ${{ join(matrix.targets, ' ') }}`, `ci.yml:474`).
- Required check: the `moon` job rolls up into `rollup`, the only context ruleset 20090117 requires. CI runs on Renovate PRs; for example, #651 shows `rollup`, `moon (bun)` and `moon (nix)`.

**What this does not cover.** It checks that a lock is internally consistent. It does not check that a commit is trustworthy. A malicious but correctly relocked fork commit passes. That is the posture question in OQ2.

## Alternatives considered

- **(a) Make `renovate/artifacts` a required context on ruleset 20090117.** Rejected. Renovate posts that status only on failure (`artifacts.js:12`). Requiring it would leave every non-Renovate PR waiting forever on "Expected — Waiting for status to be reported". Setting `statusCheckWhen.artifactError: "always"` fixes only the PRs Renovate opens. The status also reflects only what the relock script itself detected, so it misses hand edits. And rulesets have no IaC rail (OQ-N3, `docs/designs/infra/release/compass-unified-release-lane/design.md`), so this would be a manual settings change.
- **(b) A lock-integrity gate inside the required `rollup`.** CHOSEN, for the reasons above. It is declarative and reviewed in a PR, covers hand edits, and is one task in an existing project.
- **(c) Make Renovate refuse to open the branch or PR.** Rejected, because it cannot fail closed:
  - The only abort sits inside the `releaseTimestamp` arm and stops applying once the release is 2 h old (`branch/index.js:408-417`).
  - `git-refs` has no timestamp.
  - `prepareCommit` writes the regex bump again whatever the task does.
  - `prCreation: "status-success"` (`update/pr/index.js:118-126`) only withholds the PR. The branch is still pushed, and hand edits are untouched.

## Global Constraints

- **Renovate and nix versions.** Renovate claims are checked against `renovate@44.46.2`, the pinned version. Nix in CI is `cachix/install-nix-action` v31 (default nix 2.35.2); the local measurements used 2.34.8.
- **Nix invocation.** Call nix exactly as `nix flake prefetch --json --extra-experimental-features "nix-command flakes" github:<owner>/<repo>/<rev>`, with env `XDG_CACHE_HOME=<fresh absolute mkdtemp dir>`. Relative paths are rejected (`error: not an absolute path`). Delete the dir in a `finally` block.
- **Fail-closed rules.** Unverifiable means failure. No retries, and no fallback to a warm cache.
  - Exit `0`: every node is consistent.
  - Exit `1`: any node is inconsistent or unverifiable.
- **Code shape.**
  - Pure core: no I/O, no `process`, no `Bun`.
  - Thin shell: reads files, spawns nix, prints the report, exits.
  - File names follow the `<name>.core.ts` / `<name>.core.test.ts` convention used in `tools/renovate`.
- **Comments.** 1-2 lines, 4 at most. No issue IDs or planning metadata in source.
- **Relock family scope.** Do not change behaviour, only comments and messages. `refresh-devenv-lock.test.ts` asserts that stderr contains `"byte-identical"`, so keep that substring.
- **Lint.** Run `biome check` on the touched TS/JSON files only, and `rumdl check` on touched Markdown.
- **Ledger.** A PR that touches this record must also touch `docs/designs/DECISIONS.md`, or declare `Ledger-impact:` in its body (touch-coupling in `tools/design-ledger-gate/index.ts`).

## Plan

### T1: Pure core and unit tests

Files: `tools/renovate/devenv-lock-integrity.core.ts` and `tools/renovate/devenv-lock-integrity.core.test.ts`. They run under `renovate:test`, whose inputs already include `*.ts` and both locks.

Interfaces:

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

/** Every node with a `locked` field. Throws naming the node on a non-github type or a missing/ill-formed field. */
export function lockedGithubNodes(lockText: string): readonly LockedGithubNode[];
/** `github:<owner>/<repo>/<rev>` (the dedupe key and the prefetch argument). */
export function prefetchRef(n: LockedGithubNode): string;
/** Parses `nix flake prefetch --json` stdout: `.hash`, `.locked.lastModified`. Throws on missing fields. */
export function parsePrefetch(stdout: string): Prefetched;
/** One line per node; failing lines name lock path, node, field, expected and got. `ok` iff no mismatch and no unverified node. */
export function integrityReport(
  checks: readonly { lock: string; node: LockedGithubNode; observed: Prefetched | { error: string } }[],
): IntegrityReport;
```

Test cycle: write the tests first, run `bun test tools/renovate/devenv-lock-integrity.core.test.ts`, and see them fail because the module is missing. Then implement until they pass. Cases:

- Both real locks parse. Every node is `github` with a 40-hex rev, and the root node with no `locked` field is skipped.
- A wrong `narHash` gives `ok: false`, and the report names `devenv.lock`, the node and both hashes.
- A wrong `lastModified` alone gives `ok: false`.
- An `{error}` result gives `ok: false` (unverifiable, not skipped).
- A `locked.type` of `"path"` throws, naming the node.
- A missing `narHash` throws.
- `parsePrefetch` rejects JSON with no `hash` field.
- Everything matching gives `ok: true`.

### T2: Thin shell and moon task

Files:

- `tools/renovate/devenv-lock-integrity.ts`: the shell, modelled on `tools/toolchain/flake-parity.ts`.
- `tools/renovate/moon.yml`: add the task and a one-line header note.

The shell:

1. Find `repoRoot` from `import.meta.url`.
2. For each path in `DEVENV_LOCK_PATHS`, call `lockedGithubNodes`.
3. Build a map keyed by `prefetchRef` so each unique ref is fetched once, one at a time.
4. Use `mkdtempSync(join(tmpdir(), "devenv-lock-integrity-"))` as `XDG_CACHE_HOME`.
5. For each ref, run `execFileSync("nix", [...])` with stdio `["ignore", "pipe", "pipe"]`. A non-zero exit becomes `{error: <stderr tail>}`.
6. In a `finally` block, call `rmSync(dir, {recursive: true, force: true})`.
7. Print `integrityReport(...).report` and exit `ok ? 0 : 1`.

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

Smoke test:

1. On the clean tree, `bun tools/renovate/devenv-lock-integrity.ts` exits 0.
2. Put the agent-image fork `narHash` (`sha256-zTzdShjH…`) into the root `devenv` node. The script exits 1, and the report names `devenv.lock` / `devenv`.
3. Restore the lock with `jj restore devenv.lock`.
4. Run `moon run renovate:ci` locally. All three dependencies run.

### T3: Correct the fail-closed claims in the relock family

Change only comments and messages, so they name the required gate instead of "required check" or "human review stops the merge":

- `refresh-devenv-lock.ts`: header lines 26-29 and the byte-identical error message (lines 122-125).
- `refresh-devenv-lock.core.ts`: the `changedDevenvLock` doc comment (lines 54-57) and its error message (lines 75-77).
- `refresh-devenv-lock.test.ts`: the comment at lines 311-313.
- `refresh-agent-image-nixpkgs.ts`: header lines 20-22 and its byte-identical message (lines 157-160).

New wording: the exit turns `renovate/artifacts` red, which is advisory, and `renovate:lock-integrity` in the required `rollup` is what blocks the merge. Leave the `config.json5` `minimumReleaseAge: null` compensating-control notes alone. They describe soak, not integrity, which is OQ2.

Test cycle: `bun test tools/renovate/refresh-devenv-lock.test.ts tools/renovate/refresh-devenv-lock.core.test.ts tools/renovate/refresh-agent-image-nixpkgs.test.ts` stays green. That proves `"byte-identical"` and the other asserted substrings survived. Then run `biome check` on the touched files.

## Tasks

- [ ] T1: `devenv-lock-integrity.core.ts` plus core tests (red, then green).
- [ ] T2: `devenv-lock-integrity.ts` shell, the `renovate:lock-integrity` task, wiring into `renovate:ci`, and the smoke test (clean exits 0; stale `narHash` exits 1).
- [ ] T3: Relock-family comments and messages name the gate; `refresh-devenv-lock.test.ts` stays green.

## Open Questions

**OQ1. Which blocking mechanism?** (Matt)

- (a) A ruleset context only.
- (b) The gate only.
- (a) and (b) together.

Recommendation: **(b) only**. (a) cannot be required without blocking every PR that Renovate did not open (see Alternatives), and it needs a manual ruleset edit. (b) costs about 1 minute per scheduled run.

**OQ2. Should the fork keep tracking HEAD?** (Matt) Both fork rules track `RigelBuild/devenv` `main` HEAD every day, with `minimumReleaseAge: null`. A git-refs digest has no timestamp to age.

- **Keep as is.** Human review of the compass PR is the control. The fork is first-party, and its own ruleset `21184706` requires:
  - a PR with 1 approval and last-push approval;
  - the `CI` check;
  - no bypass actors.

  Cost: none.
- **Track a tag.** Switch the managers to `github-tags`, which has `releaseTimestamp`, so the global 5-day `minimumReleaseAge` applies. Cost: the fork has 0 tags today, so someone must start cutting releases, and each fork fix waits 5 days to reach compass.
- **`dependencyDashboardApproval: true` on the two fork rules.** Renovate returns `"needs-approval"` and creates no branch until someone ticks the box (`branch/index.js:139-143`). Cost: a second manual click that adds no information beyond the PR review.

Recommendation: **keep as is**. Lock integrity was the real gap, and (b) closes it. The rest is trust in first-party commits. A soak delay or an extra click does not improve that trust; the fork's own ruleset is what guards it.
