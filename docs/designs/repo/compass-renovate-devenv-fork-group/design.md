# Compass Renovate: one PR per devenv fork bump (RIG-5045)

## Problem / Intent

Compass pins the `RigelBuild/devenv` fork rev in two locks, `devenv.lock` and `agent-image/devenv.lock`. Each lock has its own regex manager and its own packageRule, so one fork commit opens two PRs: #2064 and #2065 both moved the fork to `a9d9df8`. Matt ruled option B on RIG-5045: one Renovate PR per fork bump. Its branch task relocks every changed lock, root first, then runs the FOD refresh once.

## Approach

### Today

`tools/renovate/config.json5` carries two fork packageRules. They differ only in depName, groupName and `fileFilters`:

```json5
matchDepNames: ["RigelBuild/devenv"],
groupName: "devenv fork (root)",
// …
fileFilters: ["devenv.lock", "agent-image/entrypoint.nix"],
```

```json5
matchDepNames: ["RigelBuild/devenv-agent-image"],
groupName: "devenv fork (agent-image)",
// …
fileFilters: ["agent-image/devenv.lock", "agent-image/entrypoint.nix"],
```

Both run `bun tools/renovate/refresh-devenv-lock.ts`, then `bun tools/renovate/refresh-fod-hashes.ts`. The relock picks one scope with `changedDevenvLock` in `tools/renovate/refresh-devenv-lock.core.ts`, which throws when both locks changed:

```ts
if (changed.length > 1) {
	throw new Error(
		`refresh-devenv-lock: ${changed.length} devenv locks changed vs the branch point ` +
```

### Why one rule, not two rules sharing a groupName

Renovate builds the branch config from the first upgrade only. `generateBranchConfig` (`renovate@44.46.2`, `dist/workers/repository/updates/generate.js`) sorts the upgrades by `fileReplacePosition`, then `depName`, and then spreads the first one:

```js
config = {
	...config,
	...config.upgrades[0],
```

`executePostUpgradeCommands` (`dist/workers/repository/update/branch/execute-post-upgrade-commands.js`) builds exactly one branch-mode task from that config:

```js
postUpgradeTasks: config.postUpgradeTasks.executionMode === "branch" ? config.postUpgradeTasks : void 0
```

With two rules in one group, the task would come from whichever upgrade sorts first, and its `fileFilters` would name one lock. Renovate would drop the other lock's relock from the commit. One packageRule that matches both depNames gives both upgrades the same `postUpgradeTasks`, so sort order cannot matter.

### Change

1. **One packageRule.** The two fork rules become one, in the root rule's position:

   ```json5
   matchManagers: ["custom.regex"],
   matchDepNames: ["RigelBuild/devenv", "RigelBuild/devenv-agent-image"],
   groupName: "devenv fork",
   minimumReleaseAge: null,
   postUpgradeTasks: {
     commands: [
       "bun tools/renovate/refresh-devenv-lock.ts",
       "bun tools/renovate/refresh-fod-hashes.ts",
     ],
     fileFilters: ["devenv.lock", "agent-image/devenv.lock", "agent-image/entrypoint.nix"],
     executionMode: "branch",
   },
   ```

   The branch is `renovate/devenv-fork` (the slug rule in `generateBranchName`, `dist/workers/repository/updates/branch-name.js`; "devenv fork (root)" gave #2064's `renovate/devenv-fork-(root)`). `minimumReleaseAge: null` and daily `main` HEAD tracking carry over unchanged (DL-415).

2. **Keep both managers.** The two regex managers keep their file patterns, matchStrings and distinct depNames, so each still extracts one rev from one file and the extraction tests stay valid. The distinct depName now only keeps the two updates apart in the PR body and on the dashboard; it no longer splits them into two groups. Only the comments that say otherwise change.

3. **Relock every changed scope.** `changedDevenvLock` becomes `changedDevenvLocks`. It returns every changed scope in `DEVENV_LOCK_SCOPES` key order (root, then agent-image), whatever order the diff lists them in, and `[]` when none changed. It never throws. `refresh-devenv-lock.ts` loops over the result and runs today's per-scope body unchanged: read the lock, `nix run <flakeref(devenvSource(lock))> -- update devenv` in the scope's `cwd`, the byte-identical check, and the post-relock shape guard. Each lock is still written by the devenv CLI it pins.

4. **Fail fast.** The first scope that fails throws, and the script exits non-zero before it touches the next scope. Two facts make this right:

   - The failures are correlated. `flakeref` in `tools/toolchain/devenv-cli/core.ts` is `` `github:${src.owner}/${src.repo}/${src.rev}#devenv` ``, and after the regex bump both locks hold the same rev. A build or fetch that fails for root will almost always fail again for agent-image, so a second attempt spends a second cold build and fixes nothing.
   - The budget is shared. `getRawExecOptions` (`dist/util/exec/index.js`) applies `executionTimeout` to each child process: `timeout ??= defaultExecutionTimeout * 60 * 1e3`. Both `nix run`s now share one 45-minute budget inside one `bun tools/renovate/refresh-devenv-lock.ts` process. A second attempt after a slow first failure risks the hard kill.

   The cost: if root fails, agent-image is never attempted, so both locks ship rev-bumped and not relocked. Renovate catches each command's failure separately, so it still runs the FOD refresh and still commits the regex bumps (`postUpgradeCommandsExecutor`, same file). `renovate:lock-integrity` in the required `rollup` then names every half-relocked lock (DL-413), and a re-run of the branch retries both.

5. **FOD refresh once, last.** In `FOD_ENTRIES` (`tools/renovate/refresh-fod-hashes.ts`), only the `agent-node-modules-agent-image-pkgs` entry is triggered by either lock:

   ```ts
   file: "agent-image/entrypoint.nix",
   // …
   triggers: ["bun.lock", "devenv.lock", "agent-image/devenv.lock"],
   ```

   It has no `mirrorFiles`, so `agent-image/entrypoint.nix` is the only FOD file the rule must commit. The refresh's per-entry gate fires once and realises the pin once, against both written locks. Today the two PRs pay that realise twice.

### Cost and limits

- After the regex bump both locks name the same fork rev, so both `nix run`s use one flakeref. Both managers set `packageNameTemplate: "https://github.com/RigelBuild/devenv"`, and the git-refs datasource caches by that name (`getRawRefs` in `dist/modules/datasource/git-refs/base.js`: ``withCache({ namespace: `datasource-${gitId}`, key: config.packageName }, …)``). One run therefore resolves `main` once for both upgrades. T2 pins the two `packageNameTemplate`s equal, because a split would break this.
- `executionTimeout` (45 minutes, `tools/renovate/bot-config.json5`) bounds each child process, and the relock is one process. So one 45-minute ceiling now covers both relocks together, and it does not change. That is safe only because the second relock is cheap. [INFERENCE] The second `nix run` reuses the store path the first one built. T3 records each relock's wall time, but a warm local store cannot show cold-runner timing, and the PR body says so.
- No command string changes, so `allowedCommands` in `bot-config.json5` keeps its eleven entries.
- Nothing forces equal revs. If a hand relock moves one lock alone, the lagging lock gets its own one-member `renovate/devenv-fork` branch at the next daily run, and the script relocks only that scope.
- No fork PR is open today; #2064 and #2065 are merged. `pruneStaleBranches` defaults to `true` (`dist/config/options/index.js`), so Renovate deletes any leftover `renovate/devenv-fork-(root)` or `renovate/devenv-fork-(agent-image)` branch.

### Relation to RD-1

RD-1 in `docs/designs/infra/ci/compass-devenv-source-dry/design.md` (§"Resolved decisions") rules:

> unify root onto `github:RigelBuild/devenv`, WITHOUT lock reconciliation — AND keep both locks updated through Renovate bumps

and gives this reason for no reconciliation:

> reconciling would couple an image-motivated fork bump to a dev-shell relock.

Ruling B creates that coupling for the `devenv` input. **This record supersedes RD-1's independent-cadence clause for the `devenv` fork input only.** The rest of RD-1 stands:

- One source: both locks resolve `github:RigelBuild/devenv`.
- Two lock files, each written by its own devenv CLI in its own directory.
- Independent cadence for every other input. The two nixpkgs channel rules ("devenv nixpkgs channel" and "devenv nixpkgs channel (agent-image)") keep separate groups.
- Both locks stay current through Renovate.

No decision file records RD-1: no file under `docs/designs/decisions/` points at the devenv-source record, so there is no row to flip. DL-446 records this ruling. The frozen RD-1 record is not edited.

DL-415 ("The two `RigelBuild/devenv` fork rules keep tracking `main` HEAD daily with no soak") stays Active. Its ruling applies unchanged to the one merged rule; only its rule count is out of date.

## Alternatives considered

- **Two rules with one shared groupName.** Rejected: the branch task comes from `config.upgrades[0]`, so one lock's relock would be dropped, depending on sort order.
- **One manager with a widened `managerFilePatterns` and one depName.** Saves one manager block, but rewrites the manager and its extraction tests for no gain. Rejected as churn.
- **Relock every scope, then report all failures.** It would relock agent-image when only root fails. But both scopes run one flakeref, so their failures are correlated, and a second attempt eats into the one 45-minute per-process budget both relocks share. The PR is red either way, and lock-integrity names every unrelocked lock. Rejected (Change item 4).

## Plan

### Global Constraints

- **Versions.** Every Renovate claim is checked against `renovate@44.46.2`, the pin in `.github/workflows/renovate.yml`.
- **Commands.** Both command strings stay byte-identical: `bun tools/renovate/refresh-devenv-lock.ts` and `bun tools/renovate/refresh-fod-hashes.ts`. `bot-config.json5` `allowedCommands` keeps its eleven entries.
- **Scope order.** Relock order is `DEVENV_LOCK_SCOPES` key order, root then agent-image. Never the order `git diff` prints.
- **Per-lock writer.** Each scope builds its flakeref from its own lock (`flakeref(devenvSource(before))`) and runs in its own `cwd`. No scope reuses another scope's flakeref.
- **Exit codes and messages.** Exit `0` means relocked or no-op; exit `1` means a step failed. Keep the substrings the tests assert: `nothing to do`, `byte-identical`, `devenv fork rev`, `base ref does not resolve`, `now at`, `changed vs <ref>;`.
- **Clean cutover.** `changedDevenvLock` is deleted, with no alias. Its only importers are `refresh-devenv-lock.ts` and `refresh-devenv-lock.core.test.ts`. `devenv-lock-integrity.ts` imports only `DEVENV_LOCK_PATHS`, which does not change.
- **Comments.** Explain why, not what (`AGENTS.md`). Rewrite only the sentences that describe two fork PRs or independent fork cadences. Leave the rest of each block alone.
- **Frozen records.** Do not edit `docs/designs/infra/ci/compass-devenv-source-dry/design.md`, `docs/designs/repo/compass-renovate-relock-fail-closed/design.md` or `docs/designs/infra/runtime/compass-guest-image-artifact/design.md`.
- **Landing.** One implementation PR, with T1 then T2 as commits. T2 needs T1, because today's script throws on a two-lock branch. T1 alone changes nothing live, so it does not ship alone.
- **Lint.** Run `biome check` on the touched TS and JSON5 files only.

### T1: Relock every changed scope

Files: `tools/renovate/refresh-devenv-lock.core.ts`, `refresh-devenv-lock.core.test.ts`, `refresh-devenv-lock.ts`, `refresh-devenv-lock.test.ts`, all in `tools/renovate/`.

Interfaces:

```ts
// refresh-devenv-lock.core.ts, unchanged
export type DevenvLockScope = "root" | "agent-image";
export const DEVENV_LOCK_SCOPES: Record<DevenvLockScope, { readonly lock: string; readonly cwd: string }>;
export const DEVENV_LOCK_PATHS: readonly string[];
export function devenvForkLockedRev(devenvLockText: string): string;
// replaces changedDevenvLock
/** Every scope whose lock is in changedPaths (exact match), in DEVENV_LOCK_SCOPES key order; [] when none. Never throws. */
export function changedDevenvLocks(changedPaths: readonly string[]): readonly DevenvLockScope[];

// refresh-devenv-lock.ts, module-private
/** Today's Step 3 body for one scope; throws on any failed step. */
async function relockScope(scope: DevenvLockScope, baseRef: string): Promise<void>;
```

The CLI does not change: `bun tools/renovate/refresh-devenv-lock.ts`, optional env `RENOVATE_BASE_BRANCH`, exit `0` or `1`. In `main()`, Step 2 becomes `const scopes = changedDevenvLocks(changedPaths)`. An empty list keeps today's `nothing to do` return. Then `for (const scope of scopes) await relockScope(scope, baseRef)`. A throw ends `main()` before the next scope.

Test helpers in `refresh-devenv-lock.test.ts`:

```ts
const AGENT_BUMPED_REV = "4444444444444444444444444444444444444444";
async function applyRegexBump(repo: string, lockRel: string, narHash: string, rev = BUMPED_REV): Promise<void>;
/** Every flakeref rev the stub was run with, in call order. Replaces provisionedRev. */
async function provisionedRevs(repo: string): Promise<string[]>;
```

Test cycle: edit the tests first, run `bun test tools/renovate/refresh-devenv-lock.core.test.ts tools/renovate/refresh-devenv-lock.test.ts`, and see them fail. Then implement until they pass.

`refresh-devenv-lock.core.test.ts`:

- Rename the describe to `changedDevenvLocks`.
- "selects the root scope when only the root lock changed" expects `["root"]`. "selects the agent-image scope when only that lock changed" expects `["agent-image"]`.
- "returns null when no devenv lock changed (the self-gate no-op)" becomes "returns [] when no devenv lock changed (the self-gate no-op)". It and "does not mistake a same-named lock elsewhere for a governed scope" assert `[]`.
- Replace "throws when BOTH locks changed (the groupName isolation broke)" with "returns both scopes, root first, whatever the diff order". `[AGENT, ROOT]`, `[ROOT, AGENT]` and `["bun.lock", AGENT, ROOT]` all give `["root", "agent-image"]`.
- Rename "the two scopes are distinct files (independent cadences, not reconciled)" to "the two scopes are distinct lock files". Its comment says the fork rev now moves in one PR, but the locks stay two files.

`refresh-devenv-lock.test.ts`:

- Replace "exits non-zero when BOTH locks changed on one branch" with "relocks BOTH locks, root first, each in its own directory under its own devenv". Bump root to `BUMPED_REV` and agent-image to `AGENT_BUMPED_REV`. Expect exit `0`, both locks at `RELOCKED_REV`, and `.devenv-update-cwds` equal to `[repo, join(repo, "agent-image")]`. `provisionedRevs` equals `[BUMPED_REV, AGENT_BUMPED_REV]`, which proves each lock used its own flakeref. Stdout has two `now at` lines.
- Add "stops at the first failing scope and leaves the second untouched". Bump both and write the root scope's fail marker, `join(repo, ".force-fail-relock")`. Expect a non-zero exit, exactly one line in `.devenv-update-cwds` (the repo root), and `agent-image/devenv.lock` still exactly `devenvLock(AGENT_BUMPED_REV, "BBBB")`. The agent-image scope has no marker, so a loop that ran on past the failure would relock it and fail this test.
- Add "a second-scope failure exits non-zero after relocking the first", as a `test.each` over two markers written to `join(repo, "agent-image", …)`: `.force-fail-relock`, and `.force-noop-relock` with stderr containing `byte-identical`. Bump both. Expect a non-zero exit, `devenv.lock` exactly `devenvLock(RELOCKED_REV, "CCCC")`, `agent-image/devenv.lock` exactly `devenvLock(AGENT_BUMPED_REV, "BBBB")`, and `.devenv-update-cwds` equal to `[repo, join(repo, "agent-image")]`. A loop that swallowed the second error and returned `0` fails here.
- In the per-scope happy path, `provisionedRev(repo)` becomes `provisionedRevs(repo)` equal to `[BUMPED_REV]`. Its title still holds: with one lock changed, the sibling is not relocked.
- `STUB_NIX` reads its markers per scope. Today it reads `$REPO_ROOT/.force-…`, and the marker applies to every run:

  ```bash
  if [ -f "$REPO_ROOT/.force-fail-relock" ]; then
  ```

  All three markers (`.force-noop-relock`, `.force-fail-relock`, `.force-corrupt-relock`) move to `$PWD/.force-…`, the directory the stub runs in. The root scope runs in the repo root, so the existing single-scope tests keep their marker paths unchanged. The comment above the stub ("Sentinel files in the repo root") says the markers live in each scope's own directory. The cwd and argv logs stay at `$REPO_ROOT`.

Comments to rewrite, in these files only:

- `refresh-devenv-lock.core.ts`: the file header ("which of the two devenv locks a branch touched"; a wrong scope's write "the fileFilters allowlist drops"), the `DevenvLockScope` doc ("its own packageRule, and its own branch"), and the `changedDevenvLock` doc and throw, which go away.
- `refresh-devenv-lock.core.test.ts`: the file header and the `DEVENV_LOCK_SCOPES` cwd-test comment. The failure they name becomes "a wrong `cwd` relocks the sibling and leaves this lock unrelocked".
- `refresh-devenv-lock.ts`: the header lines that say one packageRule and branch per lock, and "a two-lock branch fails loud"; the Step 2 comment; and, in Step 3, "the two revs differ by design — RD-1 unifies the source, not the locks". The reason that still holds is that a hand relock can leave the revs different.
- `refresh-devenv-lock.test.ts`: the header ("pick scope" becomes "pick scopes, relock each"); the happy-path comments "(fileFilters admits only the one)" and "the two locks are independent (RD-1)"; and the comment on the replaced both-locks test.

### T2: One fork rule

Files: `tools/renovate/config.json5`, `tools/renovate/config.test.ts`, `tools/renovate/bot-config.json5` (comments only), `tools/renovate/refresh-fod-hashes.ts` (header comment only).

Interfaces: the merged packageRule in the Approach, placed where the root fork rule is today. The agent-image fork packageRule (`groupName: "devenv fork (agent-image)"`) is deleted. Both fork customManagers keep every key.

Test cycle: edit `config.test.ts` first, run `bun test tools/renovate/config.test.ts`, and see it fail. Then edit `config.json5` until it passes.

`config.test.ts`:

- `FOD_COMMAND` comment: "SEVEN task sites" becomes six.
- "the fod-hash refresh is declared at all seven task sites" becomes "…at all six task sites", with `toHaveLength(6)`. Renumber its list: 1 top-level, 2 devenv-nixpkgs channel, 3 devenv fork (relocks both locks, each a declared trigger), 4 go lockstep, 5 catalog, 6 Meissa. "Sites 3, 4 and 6 carry it fail-safe" becomes "Sites 3 and 4".
- The allowlist describe comment says the relock rides the one devenv-fork rule. "declares eleven DISTINCT postUpgrade commands and eleven allowlist entries" does not change.
- The `coupled` population drops from 12 to 11, because two fork sites become one. "the coupled (site, entry) set has its expected shape (guard is not vacuous)" asserts `toBe(11)`, and its comment says six sites name a trigger of the entrypoint.nix entry. The guard then checks the merged rule: refresh last, `agent-image/entrypoint.nix` in `fileFilters`.
- In describe "tools/renovate devenv fork currency (RIG-2815, RIG-2546 T7)":
  - `forkScopes` keeps `label`, `depName`, `lock` and `patternLiteral`, and drops `groupName`, `taskCommands` and `taskFileFilters`. Add `const GROUP = "devenv fork"` and `const forkRule = cfg.packageRules.find((r) => r.groupName === GROUP)`.
  - Keep "declares a git-refs regex manager for the $label lock's fork rev" and "matchString extracts the fork rev from the real $label lock". Reword the comment that says the depName keeps the rules "in separate branches". The comment on its `packageNameTemplate` assertion now gives the reason the two scopes must share that value: it is the git-refs cache key, so one run gives both upgrades the same `newDigest` (Cost and limits). That per-scope `toBe("https://github.com/RigelBuild/devenv")` already pins the two managers equal, so no separate equality test is added.
  - Replace "the $label fork rule is solo-grouped and cooldown-exempt" with "one fork rule matches both depNames, solo-grouped and cooldown-exempt". Assert that `matchDepNames` equals both depNames, that exactly one rule has `GROUP`, that exactly one rule names either depName (so no later rule can win last-match), and that `minimumReleaseAge` is `null`.
  - Replace "the $label relock postUpgradeTask is branch-mode over the files it writes" with "the fork task relocks, then refreshes the FOD pin, branch-mode, over the three files it writes". Assert `commands` equals `[RELOCK, FOD_COMMAND]` and `fileFilters` equals `["devenv.lock", "agent-image/devenv.lock", "agent-image/entrypoint.nix"]`.
  - "both fork rules declare the SAME relock command (one allowlist entry)" becomes "one rule declares the relock command (one allowlist entry)", with `declaring` of length `1`.
  - "the $label fork digest resolves to its own solo branch, not the TS rollup" becomes "the $label fork digest resolves to the one fork group, not the TS rollup". It is the same `resolveGroupName` replay, and both scopes now expect `GROUP`.
  - Delete "the two fork scopes carry DISTINCT groupNames (independent cadences)".
  - Reword the describe header comment, and the "RD-1 forbids reconciling the rules" line on "both fork managers declare the IDENTICAL matchString literal".
- "its group differs from the root channel and agent-image fork groups" does not change. It now reads "devenv fork" for the fork pin, which is still one of three distinct groups.

`config.json5` comments:

- The merged rule's header says why one group (one fork commit moves both locks) and why one rule (the branch task comes from the first upgrade only). Its `fileFilters` paragraph lists the agent-image lock and drops "Listing the agent-image lock would be dead surface".
- The agent-image fork manager drops "each tracks the fork on its own cadence" and "land in DIFFERENT groups/branches".
- The agent-image channel manager drops the agent-image FORK manager from its list of fencing examples.
- The guest-rootfs rule says "the devenv-fork rule above", not "rules".

`bot-config.json5` comments: the FOD-refresh site list names "devenv fork rule" once and says six sites. Item 6 says the one devenv-fork rule runs the relock, and the script relocks every changed lock, root first. `executionTimeout` stays `45`.

`refresh-fod-hashes.ts` header: "FIVE sites … the devenv fork (root) rule" becomes the six sites in the renumbered list above.

### T3: Smoke

1. `bun test tools/renovate/` passes.
2. `bunx -p renovate@44.46.2 renovate-config-validator --no-global tools/renovate/config.json5` prints "Config validated successfully". Its migration diff has only today's hunk, `excludeDepNames` on the GitHub Actions rule (measured on `main` while writing this record).
3. Renovate lookup and live relock, in a throwaway colocated clone at the PR head (`jj git clone --colocate`). The scripts call `git rev-parse --show-toplevel`, so a non-colocated jj workspace cannot run them.
   1. Precondition: fork `main` HEAD is still `a9d9df8e…` (`git ls-remote https://github.com/RigelBuild/devenv refs/heads/main`), and `jj log -r '1e4e1fe1098a..main@origin' -- devenv.lock agent-image/devenv.lock` prints nothing. The step-6 oracle is valid only while both hold. If either has moved, skip step 6. Instead, require both locks' `nodes.devenv.locked.rev` to equal the fork HEAD and step 7 to pass.
   2. Rewind each lock to the commit just before its own fork bump, so every other input still matches `main`. Start with `jj new <PR head>`, then run `jj restore --from 07d356f2f964- devenv.lock` (before #2064) and `jj restore --from 1e4e1fe1098a- agent-image/devenv.lock` (after #2041, before #2065). Then run `jj commit -m "smoke: rewind fork revs"`, `jj bookmark create smoke-base -r @-` and `git update-ref refs/remotes/origin/smoke-base smoke-base`. `07d356f2f964-` itself is wrong for the agent-image lock: it predates #2041, so it would also rewind that lock's nixpkgs channel, and the FOD refresh would realise against the old channel.
   3. Lookup, before any edit. This puts both upgrades through Renovate's own grouping, which the validator and the hand-run scripts never touch:

      ```sh
      RENOVATE_PLATFORM=local RENOVATE_CONFIG_FILE_NAMES=tools/renovate/config.json5 \
        RENOVATE_INCLUDE_PATHS=devenv.lock,agent-image/devenv.lock RENOVATE_X_IGNORE_RE2=true \
        LOG_LEVEL=debug LOG_FORMAT=json bunx -p renovate@44.46.2 renovate \
        | jq -c 'select(.msg == "packageFiles with updates") | .config.regex[].deps[]
                 | select(.depName | startswith("RigelBuild/devenv"))
                 | {depName, branchName: [.updates[]?.branchName]}'
      ```

      Expect both `RigelBuild/devenv` and `RigelBuild/devenv-agent-image` with `branchName` `["renovate/devenv-fork"]`. Every input reaches Renovate through env, not a global config file. `tools/renovate/bot-config.json5` cannot be used here: it sets `repositories`, and `autodiscover.js` throws "repositories list not supported when platform=local". `configFileNames` is global-only but has no `env: false`, so `getEnvName` maps it to `RENOVATE_CONFIG_FILE_NAMES`, and `parseConfigs` passes it to `setUserConfigFileNames`. On the local platform, `initPlatform` (`dist/modules/platform/local/index.js`) forces `dryRun` to `"lookup"`, so the run never writes a branch. `lookup` still calls `branchifyUpgrades` and logs "packageFiles with updates", and `flattenUpdates` stamps `update.branchName` on each dep's update.
   4. Regex bump: in the working tree, set only `nodes.devenv.locked.rev` in each lock to the fork `main` HEAD.
   5. Run `RENOVATE_BASE_BRANCH=smoke-base bun tools/renovate/refresh-devenv-lock.ts`, then `RENOVATE_BASE_BRANCH=smoke-base bun tools/renovate/refresh-fod-hashes.ts`. Expect exit `0` from both, the root relock logged before the agent-image one, and a wall time for each relock.
   6. Oracle: `git diff --exit-code origin/main -- devenv.lock agent-image/devenv.lock agent-image/entrypoint.nix` exits `0`. Both locks and the FOD pin are then byte-equal to what #2064, #2065 and the FOD refresh produced on `main`.
   7. `GITHUB_EVENT_NAME=pull_request GITHUB_BASE_REF=smoke-base bun tools/renovate/devenv-lock-integrity.ts` exits `0` and prints `PR mode (base origin/smoke-base …)`. PR mode checks only the nodes changed since the merge-base, as in CI.

   Measured while writing this record, on a clone at `main` (`1e4e1fe`) built as in steps 1-2:

   - Step 3 control: today's two rules print `renovate/devenv-fork-(root)` and `renovate/devenv-fork-(agent-image)`, both at `newDigest` `a9d9df8e…`. With the merged rule applied to the clone, both print `renovate/devenv-fork`.
   - Steps 5-7: today's script still throws on two locks, so the two per-scope commands T1 will run were run by hand, root first: `nix run github:RigelBuild/devenv/a9d9df8e…#devenv -- update devenv`, in `.` and then in `agent-image/`. Each took about 1.3 s on a store that already held the fork build. The FOD refresh realised the `agent-image/entrypoint.nix` pin in 11 s. The oracle exited `0`, and lock-integrity checked 18 nodes in PR mode and exited `0`.
   - Negative control: with the root lock rev-bumped but not relocked, the oracle exits `1` and lock-integrity exits `1` with "FAIL devenv.lock devenv … narHash expected …".

## Tasks

- [ ] T1: `changedDevenvLocks` plus the fail-fast per-scope loop in `refresh-devenv-lock.ts`; per-scope stub markers; core and harness tests (both-locks, first-scope and second-scope failure) red, then green.
- [ ] T2: One `devenv fork` packageRule; `config.test.ts` at six FOD sites, eleven coupled pairs and the merged-rule assertions; `bot-config.json5` and `refresh-fod-hashes.ts` comments.
- [ ] T3: Smoke steps 1-3: tests, validator, then the local-platform lookup, live relock, byte-exact oracle and PR-mode lock-integrity. Results go in the PR body, with the note that a warm store hides cold-runner timing.

## Resolved decisions (Matt, 2026-10-10)

Ruled on RIG-5045: **option B**. One Renovate PR per devenv fork bump covers both locks. Its branch task relocks every changed lock, root first, and runs the FOD refresh once, last. Recorded as DL-446. It supersedes RD-1's independent-cadence clause for the `devenv` fork input only (see Relation to RD-1).
