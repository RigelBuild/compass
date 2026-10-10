# Renovate nix PRs: clear titles and a package-change body (RIG-2327)

## Problem / Intent

- A Renovate PR that moves a nix lock shows only a rev move. Compass #580 has the title `chore(deps): update cachix/devenv-nixpkgs digest to c2f38fe`. Its body is Renovate's table row `| cachix/devenv-nixpkgs | digest | \`c946ff3\` → \`c2f38fe\` |` plus the stock Configuration block.
- That bump moved protoc-gen-go from 1.36.11 to 1.36.12 and Chromium from 150 to 153. Neither the title nor the body says so. A reviewer has to build both shells by hand to learn what changes.
- The title also does not say which shell moves. The root channel and the agent-image channel have near-identical titles.

Intent: every Renovate PR that moves a nix-built tool gets a CI-written `## Packages that change` section. It leads with the direct packages we use (`name: old → new`) and puts the full transitive closure diff below that, collapsed. The title names the thing that moves, for example `nixpkgs channel (dev shell)`. Matt ruled option B plus clearer titles on RIG-2327 (2026-10-10).

### Evidence (verified this session)

Renovate is pinned at `bunx renovate@44.46.2` (`.github/workflows/renovate.yml`). These claims were read from that exact npm tarball (`dist/`):

- `hashBody` (`modules/platform/pr-body.js`) drops the debug comment, then cuts the body at a Reviewable marker before hashing: `const reviewableRegex = regEx(/\s*<!-- Reviewable:start -->/);` and `if (reviewableIndex > -1) result = result.slice(0, reviewableIndex);`.
- `ensurePr` (`workers/repository/update/pr/index.js`) skips the update only when `existingPrBodyHash === newPrBodyHash` (and title, base, labels match). Otherwise it calls `platform.updatePr(updatePrConfig)` with the whole new `prBody`.
- `repositoryCache` defaults to `"disabled"` (`config/options/index.js`), and neither `tools/renovate/bot-config.json5` nor `renovate.yml` sets it. So no PR cache carries over, and `ensurePr` compares body hashes on every run. [INFERENCE: the null-cache code path was not read this session.]
- `getPrBody` (`workers/repository/update/pr/body/index.js`) compiles `prBodyTemplate` against a fixed `content` object (`header`, `table`, `warnings`, `notes`, `changelogs`, `configDescription`, `controls`, `footer`). `prBodyNotes` compile against the upgrade config. No input reads the live PR body or a CI result.
- The `group` option defaults `commitMessageTopic: "{{{groupName}}}"`, but `generate.js` applies group settings to a one-dep branch only when `groupSingleUpdates === true`. Every nix-lock rule is a one-dep group today, so the `digest` default `commitMessageTopic: "{{{depName}}} digest"` wins. That default is the #580 title. A multi-dep group would apply `upgrade.group` and its `{{{groupName}}}` topic instead.
- `flatten.js` merges the `digest` object (`updateConfig[updateConfig.updateType]`) and then re-runs `applyPackageRules(updateConfig, "update-type-merge")`. A packageRule `commitMessageTopic` therefore overrides the `digest` default.
- `compileCommitMessage` lowercases the first line when `toLowerCase` is set. Topic case therefore never reaches the title.
- `GitHubMaxPrBodyLen = 58e3` (`modules/platform/github/index.js`). GitHub itself rejects a body over 65536 characters.

GitHub, devenv and repo facts:

- `ci.yml` sets top-level `permissions: contents: read` and its `pull_request` trigger is `types: [opened, synchronize, reopened]`. `pr-base-repoint.yml` records why: `edited` fires on every body edit, and the run it produced "self-skipped and double-listed every check on the PR".
- `pr-base-repoint.yml` quotes the loop guard this design relies on: "an event created with the default `GITHUB_TOKEN` triggers no new workflow run (the sole exceptions are `workflow_dispatch` and `repository_dispatch`)".
- `PATCH /repos/{owner}/{repo}/pulls/{pull_number}` needs the "Pull requests" repository permission (write). The Renovate App token is minted in `renovate.yml` inside the `main` environment, which a PR run cannot enter.
- A skipped required check stays Pending ("checks ... will remain in a "Pending" state"). The new jobs must not be required.
- The devenv fork's `eval` builds the attr `devenv.config.<attr>` and returns `{ "<attr>": <value> }` as JSON (`Devenv::eval`, `devenv/src/devenv/mod.rs`). `build` builds `devenv.config.<attr>` the same way.
- `devenv container build <name>` builds `devenv.perSystem.<system>.containerBuilds.<name>.derivation` (`container_build`, `devenv/src/devenv/container.rs`) and prints the path (`CommandResult::Print(format!("{path}\n"))`, `main.rs`). `mkContainerBuilds` (`bootstrapLib.nix`) sets `container.isBuilding = lib.mkForce true` there, and `agent-image/devenv.nix` switches `DEVENV_PROFILE` on that flag.
- `bootstrapLib.nix` loads a `devenv.local.nix` next to `devenv.nix` when present. `.gitignore` already lists `devenv.local.nix`.
- `devenv.nix` puts the dev tools in `packages`, including `pkgs.chromium` (Linux) and `secretspec`. It also has store paths outside `packages`: `env` (`E2E_FONTCONFIG_FILE`, `PLAYWRIGHT_CHROMIUM_PATH`), `processes.nats.exec` (`pkgs.nats-server`) and `services.postgres`.
- `agent-image/devenv.nix` has `packages = [ ]`. Its toolchain is a `pkgs.buildEnv` (`agent-image/toolchain.nix`) in `containers.agent.layers`. A `buildEnv` exposes its inputs as `.paths`: on the pinned channel, `map (p: p.name) (buildEnv { paths = [ hello jq ]; }).paths` evaluates to `["hello-2.12.3","jq-1.8.2"]`.
- `tools/toolchain/toolchain-tools.nix` reads `versions/bun.nix`, `node.nix` and `moon.nix`. `agent-image/toolchain.nix` takes its bun from there.
- `nix path-info -r --json --json-format 1 <path>` prints one object keyed by store path (probed on nix 2.34.8). `builtins.parseDrvName` splits `protoc-gen-go-1.36.12` into name `protoc-gen-go` and version `1.36.12`, and `compass-agent-toolchain` into a name with an empty version.

## Approach

**Recommendation: a two-job advisory workflow that renders a direct-package list plus a collapsed closure diff into a Reviewable-tail section, and a `commitMessageTopic` on each nix-lock rule.**

1. **Own workflow, not `ci.yml`.** `.github/workflows/renovate-nix-package-diff.yml` needs `pull-requests: write` and the `edited` trigger. `ci.yml` has neither on purpose. It is never a required check.
2. **Trigger.** `pull_request` with `types: [opened, synchronize, reopened, edited]`. Jobs run only when `github.head_ref` starts with `renovate/` and the head repo is this repo. Every other PR's `edited` makes a skipped run, the same cost `pr-base-repoint.yml` already pays.
3. **Targets are keyed by changed file, not by Renovate rule.** This stays correct when RIG-5045 merges the devenv fork rules.

   | Label | Dir | Triggers | Closure root |
   | --- | --- | --- | --- |
   | dev shell | `.` | `devenv.lock`, `tools/toolchain/versions/*.nix` | `devenv build packageDiff.closure` |
   | agent image | `agent-image` | `agent-image/devenv.lock`, `tools/toolchain/versions/bun.nix` | `devenv container build agent` |

4. **A CI-only module.** The job copies `tools/renovate/nix-package-diff.local.nix` to `devenv.local.nix` in each target dir, on both sides. It declares one option, `packageDiff`, with two fields:
   - `direct`: the names of `config.packages`, plus the `.paths` names of every `copyToRoot` entry in `config.containers.*.layers`. This is the agent toolchain's inputs.
   - `closure`: a `pkgs.writeText` over `builtins.toJSON` of the profile, `config.env` and each process's `exec`. Its closure covers what the shell uses, not just `packages`.

   This is the one new abstraction. No existing attr exposes the toolchain inputs or a whole-shell closure root, and the local-module hook needs no tracked change to either `devenv.nix`.
5. **Direct list (eval only).** `devenv eval packageDiff.direct` runs at the merge base and at the head, with each side's lock-pinned devenv (the `devenv-cli --mode flakeref` pattern). The core splits each name with the `parseDrvName` rule and diffs the versions. This needs no build.
6. **Transitive block (built).** For each target, the shell builds the head closure root and writes `nix path-info -r --json --json-format 1` to a file. It then deletes the target's gc roots and runs `nix store gc`, builds the base, and writes the base list. The bootstrap tools and both devenv CLIs are rooted with `--out-link` first. Peak disk is one target closure. The core groups both lists by name and keeps the names whose version set changed.
7. **Two jobs.** `collect` has read-only permissions and runs head-chosen code. It uploads the rendered section as an artifact. `publish` has `pull-requests: write`, checks out the **base** ref only, and runs the upsert from base code on that inert text.
8. **Body write.** The section starts with `<!-- Reviewable:start -->`, then `<!-- nix-package-diff head=<sha> -->`. Renovate's hash ignores everything after the marker, so Renovate never rewrites the section. When Renovate rewrites its own region, the tail is dropped and `edited` fires as the App. The job then restores the section from cache.
9. **No loop, no lost update.** `publish` PATCHes with `GITHUB_TOKEN`, which starts no new run. It reads the body and head SHA again right before the PATCH. It skips the write if the head moved, and otherwise upserts into that fresh body.
10. **Titles.** Each nix-lock packageRule sets a `commitMessageTopic` that names what moves and the target label. #580's title becomes `chore(deps): update nixpkgs channel (dev shell) to c2f38fe`.

`prBodyNotes` and `prBodyTemplate` cannot carry the list or a placeholder. They compile only from Renovate's own config (Evidence, `getPrBody`), and any placeholder in Renovate's hashed region would be reverted on the next run.

## Alternatives considered

- **Closure build vs eval-only (decided: hybrid, within ruling B).** An eval-only diff of direct packages is fast but drops transitive moves. A closure-only diff buries protoc-gen-go and chromium among many versioned libraries, in alphabetical order. The hybrid leads with the eval-only direct list, which is what Matt asked for, and puts the closure diff in `<details>`. Truncation applies to that block only.
- **One job vs two (decided: two).** The build runs code chosen by upstream lock revisions (`nix run "$src"`, the head's `devenv.nix`). In one job an earlier step can tamper with what the token step runs, for example through `$GITHUB_PATH`. Splitting keeps `pull-requests: write` away from that code. The cost is one more nix bootstrap in `publish`.
- **`nix store diff-closures` text.** It has no JSON mode, and the critique reports colour codes in piped output (NixOS/nix#4626). It also needs both closures in the store at once. Rejected for `path-info` JSON.
- **`devenv build containers.agent.derivation` or `devenv.profile` as roots.** The first misses `isBuilding`, so it is not the shipped image. The second omits `env` and process paths such as the pinned fonts. Rejected for item 4's roots.
- **Own markers inside Renovate's hashed region.** Renovate would rewrite the body on every run, and each rewrite fires `edited`. Rejected for the Reviewable tail.
- **A job in `ci.yml`, the App token, a `paths:` filter.** `ci.yml` omits `edited` on purpose. The App token cannot leave `main`. The file check inside the job already covers `paths:`. All rejected.

## Plan

### Global Constraints

- Renovate `renovate@44.46.2`. CI nix is `cachix/install-nix-action` v31 (nix 2.35.2), at the SHA `ci.yml` pins.
- Both jobs copy `ci.yml`'s `install-nix-action` `extra_nix_config` block (substituters and trusted keys, never `accept-flake-config`) and its phase-one "Put the language toolchains on PATH" step. `collect` changes `--no-link` to `--out-link "$RUNNER_TEMP/langs"` so `nix store gc` keeps bun.
- Workflow `permissions: {}`. `collect`: `contents: read`, `pull-requests: read`. `publish`: `contents: read`, `pull-requests: write`. Every checkout sets `persist-credentials: false`.
- `actions/cache/restore`, `actions/cache/save`, `actions/upload-artifact` and `actions/download-artifact` at the SHAs `ci.yml` and `release.yml` already pin.
- Concurrency group per PR, `cancel-in-progress: false`.
- Advisory only. Never add either job to the required checks or to `ci.yml`'s `rollup`.
- The section's first line is exactly `<!-- Reviewable:start -->`.
- The PATCHed body is at most 65536 characters. Only the `<details>` block truncates, and it says how many lines it dropped.
- Code shape: pure core `tools/renovate/nix-package-diff.core.ts` (no I/O, no `process`, no `Bun`) with `nix-package-diff.core.test.ts`. Thin I/O shell `tools/renovate/nix-package-diff.ts` with `collect` and `publish` subcommands. TypeScript run by bun; bash only for one-liners.
- GitHub calls use `gh api` (the `design-ledger-gate/index.ts` idiom). The body PATCH passes the body by file (`-F body=@<file>`), never argv.
- Comments are 1–2 lines, 4 at most, with no issue IDs.
- Lint: `biome check` on touched TS and JSON; `rumdl check` on touched Markdown.
- Compass only. A port to other repos is a follow-up.

### T0: Measure the unknowns (no code)

Run on a branch against #580's two revs (`c946ff3`, `c2f38fe`), with a draft `devenv.local.nix`:

1. `devenv eval packageDiff.direct` at each rev. Expected: protoc-gen-go and chromium differ. This also confirms the fork loads an untracked `devenv.local.nix`.
2. `devenv build packageDiff.closure` and `devenv container build agent` at each rev. Record wall time and peak disk with gc between sides.
3. Check that the agent closure contains no path named `devenv-profile`. This check can fail; if it does, the root is wrong.
4. Capture both `path-info` outputs as test fixtures, unedited.
5. Confirm a Renovate App body edit fires `pull_request.edited`.

Interfaces: produces fixtures for T1 and the `timeout-minutes` figure for T3.

### T1: Core module

Pure functions in `tools/renovate/nix-package-diff.core.ts`. Tests are table-driven from T0's fixtures and #580.

Interfaces:

```ts
export interface ClosureTarget {
	label: string;
	dir: string;
	triggers: readonly string[];
	closure: { kind: "build"; attr: string } | { kind: "container"; name: string };
}
export const CLOSURE_TARGETS: readonly ClosureTarget[];
export function targetsFor(changedFiles: readonly string[]): ClosureTarget[];

export interface NameVersion {
	name: string;
	version: string;
}
export function splitDrvName(storeName: string): NameVersion;
export function parseDevenvEval(json: string, attr: string): string[];
export function parsePathInfo(json: string): string[];

export interface PackageChange {
	name: string;
	from: string[];
	to: string[];
}
export function diffVersions(
	base: readonly NameVersion[],
	head: readonly NameVersion[],
): PackageChange[];

export type TargetResult =
	| { target: ClosureTarget; direct: PackageChange[]; transitive: PackageChange[] }
	| { target: ClosureTarget; error: string };

export const REVIEWABLE_MARKER = "<!-- Reviewable:start -->";
export function renderSection(
	headSha: string,
	results: readonly TargetResult[],
	maxChars: number,
	runUrl: string,
): string;
export function sectionHeadSha(body: string): string | undefined;
export function upsertSection(body: string, section: string): string;
```

- `splitDrvName` drops the `/nix/store/<hash>-` prefix. The version starts at the first `-` followed by a non-letter. A trailing output suffix (`-bin`, `-dev`, `-lib`, `-out`, `-man`, `-doc`) is removed from the version.
- `diffVersions` keeps a name only when its version sets differ and at least one side has a non-empty version.
- `renderSection` prints, per target, the direct list, then `<details><summary>Transitive closure: N changes</summary>`. An empty list prints "No package version changes." An error prints "Diff unavailable: <error>, see <runUrl>."
- `upsertSection` removes our tail from `REVIEWABLE_MARKER` to the end and appends the section. It never touches text before the marker.

### T2: CI-only module and `collect`

1. Add `tools/renovate/nix-package-diff.local.nix` (item 4 of Approach).
2. `nix-package-diff.ts collect` reads `PR_NUMBER`, `HEAD_SHA`, `RUN_URL` and `GH_REPO`. It lists the changed files, picks `targetsFor`, and adds a `git worktree` at the merge base.
3. For each target and side, it copies the module in, resolves devenv with `devenv-cli --mode flakeref`, roots that CLI with `nix build --out-link`, and runs the eval, the closure build and `path-info` (Approach items 5 and 6).
4. It writes `section.md`. It exits non-zero after writing if any target errored.

Interfaces: consumes T1. Produces `section.md`.

### T3: Workflow and `publish`

`collect` job steps:

1. Skip check, before nix: `gh pr view` one-liner. If the body has `head=<HEAD_SHA>`, the job sets `skip` and stops.
2. `actions/cache/restore` with key `nix-package-diff-v1-<head sha>`.
3. On a miss: checkout of the head (`fetch-depth: 0`), install-nix, phase one, `collect`, `actions/cache/save`.
4. Upload `section.md` as the `nix-package-diff` artifact, even when `collect` failed.

`publish` job (`needs: collect`, `if: !cancelled() && needs.collect.outputs.skip != 'true'`): checkout of `base.sha`, install-nix, phase one, download the artifact, then `nix-package-diff.ts publish`. `publish` reads `SECTION_FILE`, `PR_NUMBER`, `HEAD_SHA` and `GH_REPO`.

- It rejects a section that does not start with `REVIEWABLE_MARKER` or whose `sectionHeadSha` is not `HEAD_SHA`.
- It reads the body and head SHA again, and exits 0 if the head moved.
- It upserts, and PATCHes only if the body changed.

Interfaces: consumes T1 and T2. Produces the PR body section and two advisory checks.

### T4: Titles

In `tools/renovate/config.json5`, add `commitMessageTopic` to each packageRule whose dep comes from a nix-lock manager:

| matchDepNames | commitMessageTopic |
| --- | --- |
| `cachix/devenv-nixpkgs` | `nixpkgs channel (dev shell)` |
| `cachix/devenv-nixpkgs-agent-image` | `nixpkgs channel (agent image)` |
| `RigelBuild/meissa` | `meissa lint toolchain (dev shell)` |
| `RigelBuild/devenv` | `devenv modules (dev shell)` |
| `RigelBuild/devenv-agent-image` | `devenv modules (agent image)` |

The last two rows follow whatever rules RIG-5045 leaves. If it puts two nix-lock deps under one `groupName`, that rule also sets `group: { commitMessageTopic }`.

`config.test.ts` gains two checks:

- For every custom manager whose `managerFilePatterns` match a lock in `CLOSURE_TARGETS` triggers, a packageRule matching its `depNameTemplate` sets a `commitMessageTopic` containing that target's `label`. When the rule's `groupName` covers more than one such dep, `group.commitMessageTopic` must be set and must contain the label.
- The `bunx renovate@` pin in `renovate.yml` equals `REVIEWABLE_CUT_VERIFIED_AT` in the core (`"44.46.2"`). The failure message says to re-read `hashBody` for the marker cut, then bump the constant.

Interfaces: consumes `CLOSURE_TARGETS` from T1. The titles take effect on Renovate's next run, which retitles open PRs.

## Tasks

- [ ] T0: Measure eval, closures, disk, the container root and the `edited` event (no code)
- [ ] T1: Core module and fixture tests
- [ ] T2: CI-only module and `collect`
- [ ] T3: Two-job workflow and `publish`
- [ ] T4: `commitMessageTopic` per nix-lock rule and the two `config.test.ts` checks

## Resolved decisions (Matt, 2026-10-10)

- Option B on RIG-2327: a CI job diffs the closures into the PR body, and titles get clearer. Recorded as DL-447.

## Open Questions

- The `REVIEWABLE_CUT_VERIFIED_AT` tripwire turns every Renovate self-bump PR red until someone re-reads `hashBody`. That is the intended cost. If it proves too noisy, the fallback is a self-pin PR checklist item.
- Reusing Reviewable's marker borrows its convention. If a Reviewable integration is ever installed, the two tails would collide. Accepted for now.
