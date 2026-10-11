# Renovate nix PRs: clear titles and a package-change body (RIG-2327)

## Problem / Intent

- A Renovate PR that moves a nix lock shows only a rev move. Compass #580 has the title `chore(deps): update cachix/devenv-nixpkgs digest to c2f38fe`. Its body is Renovate's table row `| cachix/devenv-nixpkgs | digest | \`c946ff3\` → \`c2f38fe\` |` plus the stock Configuration block.
- That bump moved protoc-gen-go from 1.36.11 to 1.36.12 and Chromium from 150 to 153. Neither the title nor the body says so. A reviewer has to build both shells by hand to learn what changes.
- The title also does not say which shell moves. The root channel and the agent-image channel have near-identical titles.

Intent: every Renovate PR that moves a nix-built tool gets a CI-written `## Packages that change` section. It leads with the direct packages we use (`name: old → new`) and puts the full transitive closure diff below that, collapsed. The title names the thing that moves, for example `nixpkgs channel (dev shell)`. Matt ruled option B plus clearer titles on RIG-2327 (2026-10-10).

### Evidence (verified this session)

Renovate is pinned at `bunx renovate@44.46.2` (`.github/workflows/renovate.yml`). These quotes are from that npm tarball (`dist/`):

- `hashBody` (`modules/platform/pr-body.js`) cuts the body at a Reviewable marker before hashing: `const reviewableRegex = regEx(/\s*<!-- Reviewable:start -->/);` and `if (reviewableIndex > -1) result = result.slice(0, reviewableIndex);`.
- `ensurePr` (`workers/repository/update/pr/index.js`) leaves the PR alone only when `existingPrBodyHash === newPrBodyHash` (and title, base and labels match). Otherwise it sends the whole new body: `await platform.updatePr(updatePrConfig);`.
- The `repositoryCache` option (`config/options/index.js`) has `default: "disabled"`, and neither `tools/renovate/bot-config.json5` nor `renovate.yml` sets it. So `ensurePr` compares body hashes on every run. [INFERENCE: the null-cache code path was not read.]
- `getPrBody` (`workers/repository/update/pr/body/index.js`) builds the body only from Renovate's own `content` object: `prBody = compile(prBodyTemplate, content, false);`. No input reads the live PR body or a CI result.
- The `group` option has `commitMessageTopic: "{{{groupName}}}"`, but `generate.js` (`workers/repository/updates/`) applies it to a one-dep branch only on opt-in: `const useGroupSettings = hasGroupName && (groupEligible || singleUpdateGroup && branchUpgrades[0].groupSingleUpdates === true);`. So the `digest` default wins: `commitMessageTopic: "{{{depName}}} digest"`. That is #580's title.
- `flatten.js` (`workers/repository/updates/`) merges the `digest` object, then re-applies packageRules: `updateConfig = mergeChildConfig(updateConfig, updateConfig[updateConfig.updateType]);` then `applyPackageRules(updateConfig, "update-type-merge")`. A packageRule `commitMessageTopic` therefore wins.
- `compileCommitMessage` (`generate.js`) lowercases the title line: `splitMessage[0] = splitMessage[0].toLowerCase();`.
- `modules/platform/github/index.js` has `const GitHubMaxPrBodyLen = 58e3;`. GitHub itself rejects a body over 65536 characters.

GitHub docs ("Events that trigger workflows", "REST API endpoints for pull requests"):

- `pull_request_target`: "This event runs in the context of the default branch of the base repository, rather than in the context of the merge commit, as the `pull_request` event does." So a PR controls the YAML of a `pull_request` workflow. The same section warns: "Avoid using this event if you need to build or run code from the pull request."
- `workflow_run`: "The workflow started by the `workflow_run` event is able to access secrets and write tokens, even if the previous workflow was not." Also: "This event will only trigger a workflow run if the workflow file exists on the default branch." And: "A workflow run is triggered regardless of the conclusion of the previous workflow."
- "Update a pull request" (`PATCH /repos/{owner}/{repo}/pulls/{pull_number}`) takes only `title`, `body`, `state`, `base` and `maintainer_can_modify`. None is a precondition, so a body write cannot be conditional.
- `pr-base-repoint.yml` quotes the loop guard: "an event created with the default `GITHUB_TOKEN` triggers no new workflow run (the sole exceptions are `workflow_dispatch` and `repository_dispatch`)".
- A skipped required check stays Pending ("checks ... will remain in a "Pending" state").

devenv fork, at the rev `devenv.lock` pins:

- `Devenv::eval` (`devenv/src/devenv/mod.rs`): `let full_attr = format!("devenv.config.{attr}");` then `results.insert(attr.clone(), value);`. The output is `{ "<attr>": <value> }`. `Devenv::build` (same file) builds `.map(|a| format!("devenv.config.{a}"))`.
- `Devenv::container_build` (`devenv/src/devenv/container.rs`): `let attr = format!("devenv.perSystem.{target_system}.containerBuilds.{name}.derivation");`. `main.rs` prints the path: `Ok(CommandResult::Print(format!("{path}\n")))`.
- `mkContainerBuilds` (`devenv-nix-backend/bootstrap/bootstrapLib.nix`) sets `container.isBuilding = lib.mkForce true;`. `agent-image/devenv.nix` reacts: `// lib.optionalAttrs config.container.isBuilding {`.
- `bootstrapLib.nix` loads a local module: `localPath = devenv_root + "/devenv.local.nix";` and `lib.optional (builtins.pathExists localPath) localPath`. `.gitignore` lists `devenv.local.nix`.

Repo facts and probes:

- `ci.yml` has `permissions: contents: read` and `types: [opened, synchronize, reopened]`. `pr-base-repoint.yml` says an `edited` run "self-skipped and double-listed every check on the PR". The Renovate App token lives in the `main` environment, which a PR run cannot enter.
- `devenv.nix` has `++ lib.optionals pkgs.stdenv.isLinux [ pkgs.chromium ]` in `packages`. Outside `packages` it has `env` (`E2E_FONTCONFIG_FILE`), `processes.nats.exec` (`exec ${lib.getExe pkgs.nats-server}`) and `services.postgres`.
- `agent-image/devenv.nix` has `packages = [ ];` and `copyToRoot = [ toolchain ];` in `containers.agent.layers`. On the pinned channel, `map (p: p.name) (buildEnv { paths = [ hello jq ]; }).paths` gives `["hello-2.12.3","jq-1.8.2"]`.
- `tools/toolchain/toolchain-tools.nix` has `bunPin = import ./versions/bun.nix;`. `agent-image/toolchain.nix` has `bun = (import ../tools/toolchain/toolchain-tools.nix { inherit pkgs; }).bun;`.
- `nix path-info -r --json --json-format 1 <path>` prints one object keyed by store path (nix 2.34.8). `builtins.parseDrvName "protoc-gen-go-1.36.12"` gives `{"name":"protoc-gen-go","version":"1.36.12"}`.

## Approach

**Recommendation: an unprivileged `pull_request` workflow that collects the package diff as data, a `workflow_run` workflow from the default branch that renders and writes it after a Reviewable marker, and a `commitMessageTopic` on each nix-lock rule.**

1. **Two workflows, neither in `ci.yml`, neither required.**
   - `renovate-nix-package-diff.yml` (collect) runs on `pull_request` with read-only permissions. It runs head-chosen code and uploads a JSON artifact.
   - `renovate-nix-package-diff-publish.yml` (publish) runs on `workflow_run` (`completed`) of collect. Its YAML and code come from the default branch, and only it holds `pull-requests: write`.
2. **Collect trigger.** `types: [opened, synchronize, reopened, edited]`. Collect runs only when `github.head_ref` starts with `renovate/` and the head repo is this repo. These guards save cost only; a PR can edit them. Every other PR's `edited` makes a skipped run, the cost `pr-base-repoint.yml` already pays.
3. **Targets are keyed by changed file, not by Renovate rule.** This stays correct when RIG-5045 merges the devenv fork rules.

   | Label | Lock | Other triggers | Closure root |
   | --- | --- | --- | --- |
   | dev shell | `devenv.lock` | `tools/toolchain/versions/*.nix` | `devenv build packageDiff.closure` |
   | agent image | `agent-image/devenv.lock` | `tools/toolchain/versions/bun.nix` | `devenv container build agent` |

4. **A CI-only module.** Collect copies `tools/renovate/nix-package-diff.local.nix` to `devenv.local.nix` in each target dir, on both sides. It declares one option, `packageDiff`:
   - `direct`: the names of `config.packages`, plus the `.paths` names of every `copyToRoot` entry in `config.containers.*.layers`.
   - `closure`: a `pkgs.writeText` over `builtins.toJSON` of the profile, `config.env` and each process's `exec`.

   This is the one new abstraction. No existing attr exposes the toolchain inputs or a whole-shell closure root, and the local-module hook needs no tracked change to either `devenv.nix`.
5. **Direct list (eval only).** `devenv eval packageDiff.direct` runs at the merge base and at the head, with each side's lock-pinned devenv (the `devenv-cli --mode flakeref` pattern). The core splits each name with the `parseDrvName` rule and diffs the versions.
6. **Transitive block (built).** Per target, collect builds the head root and writes `nix path-info -r --json --json-format 1` to a file. It deletes that root, runs `nix store gc`, then does the same for the base. Bootstrap tools and both devenv CLIs are rooted with `--out-link` first. Peak disk is one target closure.
7. **The artifact is data.** Collect uploads `diff.json`: the head SHA and, per target, a label plus name/version lists or an error code. It carries no Markdown.
8. **Publish treats the artifact as untrusted.** Before any write it checks:
   - `workflow_run.event` is `pull_request` and `workflow_run.pull_requests` names exactly one open PR from this repo whose head ref starts with `renovate/`;
   - the artifact's head SHA equals `workflow_run.head_sha` and the PR's current `head.sha`;
   - the artifact is at most 1 MiB, every label is a known target label, every name and version matches `^[A-Za-z0-9._+-]{1,128}$`, and every error is a known code.

   It then renders the section with default-branch code, so the marker, heading and links are its own.
9. **Body write.** The section starts with `<!-- Reviewable:start -->`, then `<!-- nix-package-diff head=<sha> -->`. Renovate's hash ignores everything after the marker, so Renovate never rewrites the section. When Renovate rewrites its own region, the tail is dropped and `edited` fires. Collect restores the data from cache.
10. **The write is best-effort.** GitHub has no conditional PATCH (Evidence). Publishers are serialized per PR by a concurrency group. Publish re-reads the body right before the PATCH, keeps everything before the marker as read, replaces only the tail, and sends only `body`.
    - Residual race: an edit that lands between that GET and the PATCH is lost. The window is one API round trip.
    - Renovate's region heals: its next run sees a hash mismatch and rewrites, and the section returns.
    - A human edit in that window, such as ticking the rebase checkbox, must be redone.
    - No loop: publish PATCHes with `GITHUB_TOKEN`, which starts no new run.
11. **Titles.** Each packageRule for a dep of a `devenv.lock` or `agent-image/devenv.lock` manager sets a `commitMessageTopic` naming what moves and the target label. #580's title becomes `chore(deps): update nixpkgs channel (dev shell) to c2f38fe`.

`prBodyNotes` and `prBodyTemplate` cannot carry the list or a placeholder. They compile only from Renovate's own config (Evidence, `getPrBody`), and a placeholder in Renovate's hashed region would be reverted on the next run.

## Alternatives considered

- **Closure build vs eval-only (decided: hybrid, within ruling B).** Eval-only drops transitive moves. Closure-only buries protoc-gen-go and chromium among many libraries. The hybrid leads with the eval-only direct list, which is what Matt asked for, and collapses the closure diff. Truncation applies to the closure block only.
- **Where the write token lives (decided: separate `workflow_run` workflow).** Two jobs in one `pull_request` workflow are not enough: the PR controls that YAML, so it could add steps to the job holding the token. `pull_request_target` gives default-branch YAML, but its docs warn against building PR code there. `workflow_run` keeps the build unprivileged and the write in default-branch code.
- **Rendered Markdown as the artifact.** Publish would have to sanitize free text. JSON with a closed character set is simpler to validate. Rejected.
- **`nix store diff-closures` text.** It has no JSON mode and needs both closures in the store at once. Rejected for `path-info` JSON.
- **`devenv build containers.agent.derivation` or `devenv.profile` as roots.** The first skips `isBuilding`, so it is not the shipped image. The second omits `env` and process paths. Rejected.
- **Own markers inside Renovate's hashed region.** Renovate would rewrite the body on every run. Rejected.
- **A job in `ci.yml`, the App token, a `paths:` filter.** `ci.yml` omits `edited` on purpose. The App token cannot leave `main`. The in-job file check already covers `paths:`. All rejected.

## Plan

### Global Constraints

- Renovate `renovate@44.46.2`. CI nix is `cachix/install-nix-action` v31 (nix 2.35.2), at the SHA `ci.yml` pins.
- Both workflows copy `ci.yml`'s `install-nix-action` `extra_nix_config` block (never `accept-flake-config`) and its phase-one "Put the language toolchains on PATH" step. Collect changes `--no-link` to `--out-link "$RUNNER_TEMP/langs"` so `nix store gc` keeps bun.
- Collect permissions: `contents: read`, `pull-requests: read`. Publish permissions: `actions: read`, `contents: read`, `pull-requests: write`. Every checkout sets `persist-credentials: false`.
- Publish checks out only the default branch at `github.sha`. It downloads the artifact with `run-id: ${{ github.event.workflow_run.id }}` and `github-token: ${{ github.token }}` into `${{ runner.temp }}`, never the workspace, and never executes artifact content. `actions/download-artifact` reads only the current run unless it gets that token.
- `actions/cache/restore`, `actions/cache/save`, `actions/upload-artifact` and `actions/download-artifact` at the SHAs `ci.yml` and `release.yml` already pin.
- Concurrency, both `cancel-in-progress: false`: collect `nix-package-diff-<PR number>`; publish `nix-package-diff-publish-<workflow_run.head_branch>`, which exists even when `workflow_run.pull_requests` is empty.
- Advisory only. Never add either workflow to the required checks or to `ci.yml`'s `rollup`.
- The section's first line is exactly `<!-- Reviewable:start -->`. The PATCHed body is at most 65536 characters. Only the `<details>` block truncates, and it says how many lines it dropped.
- Code shape: pure core `tools/renovate/nix-package-diff.core.ts` (no I/O, no `process`, no `Bun`) with `nix-package-diff.core.test.ts`. Thin I/O shell `tools/renovate/nix-package-diff.ts` with `collect` and `publish` subcommands. TypeScript run by bun; bash only for one-liners.
- GitHub calls use `gh api` (the `design-ledger-gate/index.ts` idiom). The body PATCH passes the body by file (`-F body=@<file>`), never argv.
- Comments are 1–2 lines, 4 at most, with no issue IDs.
- Lint: `biome check` on touched TS and JSON; `rumdl check` on touched Markdown.
- Compass only. A port to other repos is a follow-up.

### T0: Measure the unknowns (no code)

Run on a branch against #580's two revs (`c946ff3`, `c2f38fe`), with a draft `devenv.local.nix`:

1. `devenv eval packageDiff.direct` at each rev. Expected: protoc-gen-go and chromium differ. This also confirms the fork loads an untracked `devenv.local.nix`.
2. `devenv build packageDiff.closure` and `devenv container build agent` at each rev. Record wall time and peak disk with gc between sides.
3. Check that the agent closure contains no path named `devenv-profile`. If one is there, the root is wrong.
4. Capture both `path-info` outputs as test fixtures, unedited.
5. Confirm that a Renovate App body edit fires `pull_request.edited`, and that a `workflow_run` run of a draft publisher sees the PR in `workflow_run.pull_requests`.

Interfaces: produces fixtures for T1 and the `timeout-minutes` figure for T3.

### T1: Core module

Pure functions in `tools/renovate/nix-package-diff.core.ts`. Tests are table-driven from T0's fixtures and #580, plus hostile artifacts for `parseArtifact`.

Interfaces:

```ts
export interface ClosureTarget {
	label: string;
	dir: string;
	lock: string;
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

export const ERROR_CODES: readonly ["eval-failed", "build-failed", "path-info-failed"];
export type TargetResult =
	| { label: string; direct: PackageChange[]; transitive: PackageChange[] }
	| { label: string; error: (typeof ERROR_CODES)[number] };
export interface DiffArtifact {
	headSha: string;
	skip: boolean;
	results: TargetResult[];
}
export const MAX_ARTIFACT_BYTES = 1_048_576;
export function parseArtifact(raw: string): DiffArtifact;

export const REVIEWABLE_MARKER = "<!-- Reviewable:start -->";
export const REVIEWABLE_CUT_VERIFIED_AT = "44.46.2";
export function renderSection(artifact: DiffArtifact, maxChars: number, runUrl: string): string;
export function sectionHeadSha(body: string): string | undefined;
export function upsertSection(body: string, section: string): string;
```

- `splitDrvName` drops the `/nix/store/<hash>-` prefix. The version starts at the first `-` followed by a non-letter. A trailing output suffix (`-bin`, `-dev`, `-lib`, `-out`, `-man`, `-doc`) is removed from the version.
- `diffVersions` keeps a name only when its version sets differ and one side has a non-empty version.
- `parseArtifact` throws on anything item 8 of Approach rejects: size, shape, a non-hex or wrong-length `headSha`, an unknown label or error code, or a name or version outside the character set.
- `renderSection` prints, per target, the direct list, then `<details><summary>Transitive closure: N changes</summary>`. An empty list prints "No package version changes." An error prints "Diff unavailable (`<code>`), see `<runUrl>`."
- `upsertSection` keeps the text before `REVIEWABLE_MARKER` as given, drops the rest, and appends the section.

### T2: CI-only module and `collect`

1. Add `tools/renovate/nix-package-diff.local.nix` (item 4 of Approach).
2. `nix-package-diff.ts collect` reads `PR_NUMBER`, `HEAD_SHA` and `GH_REPO`. It lists the changed files, picks `targetsFor`, and adds a `git worktree` at the merge base.
3. For each target and side, it copies the module in, resolves devenv with `devenv-cli --mode flakeref`, roots that CLI with `nix build --out-link`, and runs the eval, the closure build and `path-info` (Approach items 5 and 6).
4. It writes `diff.json`. It exits non-zero after writing if any target errored.

Interfaces: consumes T1. Produces `diff.json` (a `DiffArtifact`).

### T3: Workflows and `publish`

`renovate-nix-package-diff.yml` (collect), one job:

1. Skip check, before nix: a `gh pr view` one-liner. If the body has `head=<HEAD_SHA>`, write `{"headSha":"<sha>","skip":true,"results":[]}` and go to step 4.
2. `actions/cache/restore` with key `nix-package-diff-v1-<head sha>`.
3. On a miss: checkout of the head (`fetch-depth: 0`), install-nix, phase one, `collect`, `actions/cache/save`.
4. Upload `diff.json` as the `nix-package-diff` artifact, even when `collect` failed.

`renovate-nix-package-diff-publish.yml` (publish), one job. It runs on `workflow_run: { workflows: [renovate-nix-package-diff], types: [completed] }`. Its job-level `if:` requires `github.event.workflow_run.event == 'pull_request'`, a conclusion of `success` or `failure`, `startsWith(github.event.workflow_run.head_branch, 'renovate/')`, `workflow_run.head_repository.full_name == github.repository`, and `workflow_run.pull_requests` of length exactly 1. Other PRs skip collect, so their runs carry no artifact and must never reach the download. Steps: a `gh pr view` guard that re-checks the one PR is open, same-repo and `renovate/` (exit 0 otherwise), checkout of the default branch, install-nix, phase one, download the artifact, then `nix-package-diff.ts publish`. `publish` reads `ARTIFACT_FILE`, `RUN_HEAD_SHA`, `PR_NUMBERS` (JSON of `workflow_run.pull_requests[*].number`), `RUN_URL` and `GH_REPO`.

1. Read the PR and run the checks in Approach item 8, then `parseArtifact`. On a failed check it exits non-zero with no write. On `skip` it exits 0.
2. Render. Re-read the body and head SHA. Exit 0 if the head moved.
3. Upsert into that fresh body, and PATCH `body` only if it changed.

Interfaces: consumes T1 and T2. Produces the PR body section, a collect check on the PR, and a publish run in Actions.

### T4: Titles

In `tools/renovate/config.json5`, add `commitMessageTopic` to each packageRule whose dep comes from a `devenv.lock` or `agent-image/devenv.lock` manager:

| matchDepNames | commitMessageTopic |
| --- | --- |
| `cachix/devenv-nixpkgs` | `nixpkgs channel (dev shell)` |
| `cachix/devenv-nixpkgs-agent-image` | `nixpkgs channel (agent image)` |
| `RigelBuild/meissa` | `meissa lint toolchain (dev shell)` |
| `RigelBuild/devenv`, `RigelBuild/devenv-agent-image` | `devenv modules (dev shell and agent image)` |

The last row is the one `devenv fork` rule that RIG-5045 lands (`matchDepNames` lists both, `groupName: "devenv fork"`). That rule sets `commitMessageTopic: "devenv modules (dev shell and agent image)"` and the same value in `group: { commitMessageTopic }`.

`config.test.ts` gains two checks:

- For every custom manager whose `managerFilePatterns` match a `CLOSURE_TARGETS` `lock`, a packageRule matching its `depNameTemplate` sets a `commitMessageTopic` containing that target's `label`. When the rule's `groupName` covers more than one such dep, `group.commitMessageTopic` must be set and contain the label. Managers of other trigger files, such as `tools/toolchain/versions/bun.nix`, are out of scope.
- The `bunx renovate@` pin in `renovate.yml` equals `REVIEWABLE_CUT_VERIFIED_AT`. The failure message says to re-read `hashBody` for the marker cut, then bump the constant.

Interfaces: consumes `CLOSURE_TARGETS` and `REVIEWABLE_CUT_VERIFIED_AT` from T1. The titles take effect on Renovate's next run, which retitles open PRs.

## Tasks

- [ ] T0: Measure eval, closures, disk, the container root, `edited` and `workflow_run` (no code)
- [ ] T1: Core module, fixture tests and hostile-artifact tests
- [ ] T2: CI-only module and `collect`
- [ ] T3: Collect and publish workflows, and `publish`
- [ ] T4: `commitMessageTopic` per lock rule and the two `config.test.ts` checks

## Resolved decisions (Matt, 2026-10-10)

- Option B on RIG-2327: a CI job diffs the closures into the PR body, and titles get clearer. Recorded as DL-447.

## Open Questions

- The `REVIEWABLE_CUT_VERIFIED_AT` check turns every Renovate self-bump PR red until someone re-reads `hashBody`. That is intended. If it proves too noisy, the fallback is a self-pin PR checklist item.
- Reusing Reviewable's marker borrows its convention. If a Reviewable integration is ever installed, the two tails would collide. Accepted for now.
