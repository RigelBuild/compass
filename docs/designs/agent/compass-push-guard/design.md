# Compass client push guard

> Freezes on merge; later changes supersede by citation, never rewrite.

Issue: RIG-1882. Decisions DL-507..DL-510. Ruling (Matt, 2026-08-07): two layers. The client OMP
extension in the agent image is load-bearing, because Compass cannot control a
user's own repo settings. Server-side branch protection plus the scoped
per-agent forge token is a recommended backstop, not a guarantee.

## Problem / Intent

The `jj` skill admits the never-push-`main` invariant is unenforced
(`config/skills/jj/SKILL.md`: "the invariant is **behavioral**: you enforce
it; nothing blocks you"). The brokered GitHub App token cannot write outside
the agent's granted repos, but it can push a granted repo's `main` and merge
its PRs. This record adds an in-process guard that blocks three things before
the tool runs: a push to a protected branch, a PR merge by any path, and a
write to an owner or host outside the agent's allowlist.

## Approach

### D1 Two layers; the client guard is the one Compass owns

Matt ruled the layering; this record does not reopen it. The guard (D2-D7)
ships in every agent image. Branch protection (D8) is advice to users. The
guard stops a cooperative agent's mistaken or habitual command. It is not a
sandbox: a command assembled at run time inside a script file or an `eval`
cell can evade it, and so can state the persistent `bash` session keeps
between calls (functions, aliases, an exported `GH_REPO`). The token scope
and branch protection bound those cases.

### D2 An inline extension compiled into the binary

- `main` in `packages/compass-agent/src/cli.ts` passes
  `extensions: [pushGuard(readPushGuardConfig(env))]` to `createAgentSession`
  ("Inline extensions (merged with discovery)", `CreateAgentSessionOptions` in
  the SDK's `sdk.ts`). Mounted extensions come from the user's bundle
  (`enumerateMountedExtensions` in `src/config-reader.ts`), which could omit a
  mounted guard. The `bun build --compile` binary
  (`agent-image/entrypoint.nix`) cannot drop a compiled-in factory.
- The handler is `pi.on("tool_call", ...)` returning `{ block: true, reason }`;
  `ToolCallEventResult.reason` is "Reason for blocking (returned to LLM as
  error)". `ExtensionRunner.emitToolCall` returns on the first block, and a
  handler error or timeout blocks too ("On-timeout policy: **fail-closed**").
- Subagents run `bash` in the same container. The locked SDK 18.0.11 drops
  inline factories for child sessions: `createAgentSession` sets
  `toolSession.preparedExtensions = extensionsResult.preparedExtensions` before
  its inline loop. 18.3.5 keeps them ("Caller-provided factories are safe to
  rebind, so preserve them with file-backed prepared extensions for `/tan` and
  other child sessions"), and `structured-subagent.ts` passes
  `preloadedPreparedExtensions: session.preparedExtensions`. The SDK floor
  rises to 18.3.5 (T1).
- Isolated tasks still lose it: `runIsolatedSubprocess`
  (`task/isolation-runner.ts`) passes `preloadedPreparedExtensions: undefined`.
  Isolation is off by default, but a user settings bundle can turn it on, so
  `cli.ts` forces `task.isolation.enabled` to `false` and T3 tests it.

### D3 Inspected tools

- `bash` and any tool whose `input.command` is a string, parsed per D6, in
  `input.cwd` or else `ctx.cwd`.
- `eval`: `input.code` gets the D6 text check only.
- The SDK `github` tool (present when `gh` is on `PATH`): ops `pr_create` and
  `pr_push`.
- GitHub MCP tools, `/^mcp__(?:[a-z0-9]+_)?github_/`. The SDK names MCP tools
  `mcp__${server}_${tool}` (`mcp/tool-bridge.ts`); a server under another name
  is not recognized, and D8 bounds it.
- Not `forge_*`: they have no merge verb (`TransitionPullRequestStateRequest`
  in `proto/compass/v1/agent_gateway.proto`: "merge is a separate concern never
  expressed as a transition"), and with scope enforcement on,
  `forgeService.requireForgeScope` (`go/server/forge.go`) checks their writes
  against the same grants as D4.

### D4 Allowlist from the agent's forge grants (assumes OQ-1 option a)

- At provision the server calls `Store.ListForgeScopeRepos(ctx, agent,
  store.ForgeProviderGitHub, host)` ("returns the exact repo grants and
  wildcard for an account and its owner") for the configured forge host
  (`ForgeConfig.Host`, default `github.com`). `gitCredentialBroker.credential`
  (`go/server/git_credential.go`) mints the token from the same call.
- Owners: the distinct lowercased owners of the `owner/name` grants; a `*`
  grant gives `*`, meaning any owner. Hosts: the forge host.
- New `ProvisionAgentWorkspaceRequest` fields carry both, and the server
  overwrites any client value, as it does `persona`. The Runner exports
  `COMPASS_FORGE_ALLOW_OWNERS` and `COMPASS_FORGE_ALLOW_HOSTS`; the guard reads
  them once at session start. A grant change applies only at the next
  provision, which is a new container: `ReloadAgentSession` does not
  re-provision, while the token broker picks up grant changes live. OQ-1 (d)
  avoids that gap.
- An empty owner or host list blocks every write, with a reason that names the
  variable. This holds for every OQ-1 option.

The token already cannot reach ungranted repos. The guard repeats the check
because a user-mounted MCP server may carry its own GitHub token (DL-441 keeps
user-declared GitHub secrets out of the agent, not MCP servers' own config),
and an in-process block gives a reason, not a 403.

### D5 Protected branches, independent of the allowlist

Protected names are `main`, `master`, and the target repo's default branch.
The default branch is read locally first (`refs/remotes/<remote>/HEAD`, or
jj's `trunk()`), then from `gh api --hostname <host> repos/<owner>/<repo>
--jq .default_branch`, memoized per repo; a failed read blocks a push but not
other commands. They apply to every owner.

- `git push`: a refspec destination after stripping `+` and `refs/heads/`
  (`main`, `+main`, `HEAD:main`, `refs/heads/main`, `:main`). `--all`,
  `--branches` and `--mirror` block. With no refspec the destination depends
  on `push.default` and `remote.<name>.push`, so the guard blocks a bare
  `git push` unless the current branch is not protected and neither setting
  is present; OQ-3 covers asking git itself.
- `jj git push`, `jj-hp push`, `jj-vine submit`: a protected name given as a
  positional, `-b`/`--bookmark`, or `--named <name>=<rev>` blocks. So does a
  protected bookmark whose local and remote targets differ, because `--all`,
  `--tracked`, `--deleted`, `-r`, a default push, or a stack submit can carry
  it. T2 gives the template. The remote is `--remote` or else `origin`.

### D6 Shell text is parsed, not pattern-matched

The SDK's `tokenizeShellSegments` (`tools/shell-tokenize.ts`) leaves
"heredocs, command substitution, and arithmetic expansion" out of scope, so
the guard adds a small parser, `push-guard/parse.ts`, the one new abstraction
here. It returns a flat list of simple commands, each with its words and
leading `NAME=value` assignments:

- quotes, backslash and `$'...'` escapes; the operators `;`, `&&`, `||`, `|`,
  `&`, `(`, `)` and newline;
- `$(...)`, backticks and `<(...)` parsed as nested commands; a word that holds
  one is marked dynamic;
- a heredoc body parsed as a script when it feeds `bash`, `sh` or `zsh`;
- wrappers removed: `env` (its assignments kept), `sudo`, `command`, `exec`,
  `nice`, `nohup`, `timeout`, `time`, `xargs`, `eval`, and `bash`, `sh` or
  `zsh` with `-c`;
- `cd <literal>` sets the directory of later commands; a dynamic target makes
  it unknown.

Fail closed: a command whose first word is dynamic and a later word is `push`,
`merge`, `submit` or `api` blocks (`$(echo git) push origin main`). So do text
that does not parse and names a guarded program (`git`, `gh`, `jj`,
`jj-vine`, `jj-hp`), `xargs` running a guarded program, and a guarded write in
an unknown directory. `eval` code gets one text check: a line holding `git`
or `jj`, `push`, and a protected name blocks. No other raw-text match blocks,
so `rg 'gh pr merge'` or a commit message naming a merge is allowed.

The SDK's natives bundle tree-sitter-bash (`THIRD-PARTY-NOTICES.txt` in
`@oh-my-pi/pi-natives`), but no exported bash parse API was found. T2 uses
one if it exists and keeps only the unwrap and evaluate logic.

### D7 Writes, decision order, and rule calls

A **write** is a push (`git push`, `jj git push`, `jj-hp push`,
`jj-vine submit`, `github` `pr_push`); a `gh` command under `pr`, `issue`,
`repo`, `release`, `label`, `workflow`, `run`, `secret` or `variable` whose
verb is not `view`, `list`, `status`, `diff`, `checks`, `search`, `browse`,
`watch`, `download` or `checkout`; a `gh api` mutation; or a GitHub MCP tool
whose name holds none of `get_`, `list_`, `search_`, `_read`. Its target repo
is the first found of: the push remote (a URL, or a name through
`git remote get-url`), `-R`/`--repo`, `GH_REPO`/`GH_HOST`, a `gh repo`
positional `OWNER/REPO`, the `gh api` path, and the MCP `owner` argument.
Otherwise a `gh` write follows gh's own choice: the `gh repo set-default`
repo, else remotes ranked `upstream`, `github`, `origin` (`remoteNameSortScore`
in gh's `context/remote.go`). The guard checks every remote of the clone and
blocks if any is outside the allowlist, so a fork's `upstream` cannot be
written by accident.

First match wins:

1. **Merge:** a parsed `gh pr merge` (verb found past flags; `--auto` too); a
   `gh api` path ending `/pulls/<n>/merge` or `/merges`; a `gh api graphql`
   body whose `mutation` names `mergePullRequest`,
   `enablePullRequestAutoMerge` or `enqueuePullRequest`; a GitHub MCP tool
   name holding `merge`.
2. **No allowlist:** any write while owners or hosts is empty.
3. **Owner** unresolved or not allowed (`*` allows any).
4. **Host** not allowed (`api.github.com` counts as `github.com`).
5. **`gh api` mutation** (`-X`/`--method` not `GET`, or `-f`/`-F`/`--field`/
   `--raw-field`/`--input` on a REST path) whose path is not
   `repos/<allowed owner>/...`. A `gh api graphql` call is a mutation only if
   its query text holds `mutation`; then it blocks, because GraphQL names node
   ids, not owners, and the reason points to the `forge_*` tools. A GraphQL
   query (review threads, check state) is allowed.
6. **PR create:** `gh pr create`, `github` `pr_create` and `pr_push` block with
   "use `jj-vine submit <bookmark>`".
7. **Protected push** (D5).

Calls on the rules the guard the operator's own agent fleet runs today
enforces:

| Rule | Call | Reason |
| --- | --- | --- |
| Broad process kill (`pkill`, `killall`, `kill -1`) | Drop | Scope here is push authorization; a separate guard can own it. |
| `gh pr create` redirect | Keep | `jj-vine submit` is the documented Compass push path (DL-142). |
| Block pushes to the `upstream` remote | Drop | A user's agent may work a fork; rules 3 and 4 bound the target. |
| Fail-closed rules | Keep | An unknown target or unparsed text must never read as consent. |
| `gh issue create` needs an allowlisted `-R` | Fold into rule 3 | The target resolves from `origin` when `-R` is absent. |
| First push to an empty repo is allowed | Drop | That push lands on `main`; the user seeds an empty repo. |
| `ssh` and `recipe` tool names | Generalize | Not Compass tools; any tool with a string `command` is inspected. |

### D8 Server backstop: advice, not built here

Users should protect `main` with a ruleset: require a pull request approved by
a human code owner, block force pushes, and block deletion. The reviewer App
(`ForgeConfig.ReviewerApp`) must not count as that approval. Then the token's
`contents: write` and `pull_requests: write` (`gitCredentialPermissions` in
`go/server/git_credential.go`) cannot land on `main` without a human. The
token is already repo-scoped by grants. This record adds no server code for it.

## Alternatives considered

- **A mounted extension in the config bundle.** Rejected: the user's bundle can
  omit it, and the guard must hold for every bundle.
- **Git hooks or wrapper binaries on `PATH`.** Rejected: `--no-verify`, an
  absolute binary path, `curl`, and MCP tools all bypass them, and none sees a
  merge made through the API.
- **A narrower App token** (no `pull_requests: write`). Rejected: it breaks
  `jj-vine submit` PR creation, and `contents: write` still pushes `main`.
- **Regex on the raw command text only.** Rejected: it misses `bash -c` and
  substitutions and blocks harmless text such as `echo "git push origin main"`.
  D6 keeps a text check only as the merge backstop.

## Global Constraints

- Every `@oh-my-pi/*` dependency of `packages/compass-agent` is `^18.3.5` or later.
- The guard only blocks. It never returns `input` or `additionalContext`. A
  user-mounted handler that rewrites `input` is outside its view
  (`ToolCallEventResult.input`: "each sees the original `event.input`"); the
  user owns that code (OQ-2).
- Every reason starts with `push-guard:`, names the rule, and names the allowed
  path (`jj-vine submit <bookmark>`, a `forge_*` tool, or a human).
- Env vars are `COMPASS_FORGE_ALLOW_OWNERS` and `COMPASS_FORGE_ALLOW_HOSTS`:
  comma-separated, lowercased, omitted when empty, like `COMPASS_PERSONA` in
  `AgentEnv.execSpec`. Absent and empty mean the same thing: block every write.
- Unit tests use `bun test`, colocated, with injected probes and no network.
- This repo is public: no reference to any private repo.

## Plan

### T1 SDK floor 18.3.5 (lane agent, RIG-1882)

- Raise the five `@oh-my-pi/*` ranges in `packages/compass-agent/package.json`,
  refresh `bun.lock`, and update both `outputHashBySystem` hashes in
  `agent-image/entrypoint.nix`.
- Acceptance: the existing `packages/compass-agent` tests pass, and the image
  builds on both systems.
- Interfaces: none new.

### T2 Guard module (lane agent, RIG-1882)

- New `packages/compass-agent/src/push-guard/`: `parse.ts` (D6), `evaluate.ts`
  (D3, D5, D7), `probes.ts` (process-backed probes), `index.ts` (config and
  factory), each with a colocated test.
- `probes.movedBookmarks` runs the template below. A protected name blocks when
  its `@local` row is `false`, or no row for the push remote is `true`. A jj
  error blocks. The test runs it against a real jj repo.
- Acceptance: blocks `git push origin main`, `git push -f origin HEAD:main`,
  `jj-vine submit` with a moved local `main`, `gh pr merge 12`,
  `gh api -X PUT repos/o/r/pulls/1/merge`, a GraphQL `mergePullRequest`,
  `gh pr create`, a push and `gh issue create -R other/repo` to an owner not
  allowed, a push to a host not allowed, an MCP merge tool,
  `bash -c 'git push origin main'`, `$(echo git) push origin main`, and a
  heredoc into `bash` that pushes `main`. Allows an allowed feature-branch
  `git push`, `jj-vine submit feat-x`, `gh api repos/o/r/pulls`, and
  `gh pr view 12`.
- Interfaces:

```text
jj bookmark list --all-remotes -T 'name ++ "\t" ++ if(remote, remote, "@local") ++ "\t" ++ if(remote, tracked(), synced()) ++ "\n"'
```

```ts
// parse.ts
export interface ShellCommand { words: string[]; dynamic: boolean[]; assigns: Record<string, string>; cwd: string | null }
export type ParseResult = { ok: true; commands: ShellCommand[] } | { ok: false };
export function parseShell(text: string, cwd: string): ParseResult;
// evaluate.ts
export interface PushGuardConfig { owners: string[]; hosts: string[] }
export interface Probes {
	remoteUrl(cwd: string, remote: string): Promise<string | null>;
	currentBranch(cwd: string): Promise<string | null>;
	defaultBranch(host: string, owner: string, repo: string): Promise<string | null>;
	movedBookmarks(cwd: string, remote: string): Promise<string[] | null>;
}
export type Verdict = { block: true; reason: string } | null;
export function evaluate(toolName: string, input: Record<string, unknown>, cwd: string, config: PushGuardConfig, probes: Probes): Promise<Verdict>;
// index.ts
export function readPushGuardConfig(env: Record<string, string | undefined>): PushGuardConfig;
export function pushGuard(config: PushGuardConfig, probes?: Probes): ExtensionFactory;
```

### T3 Wire the guard into the agent (lane agent, RIG-1882)

- `main` in `src/cli.ts` adds `extensions: [pushGuard(readPushGuardConfig(env))]`
  to its `createAgentSession` call.
- Acceptance: a session test built like `native-tools.test.ts` shows a blocked
  `bash` call returns the `push-guard:` reason to the model as a tool error,
  that a `task` child session blocks the same call, and that a settings bundle
  enabling task isolation does not turn it on.
- Interfaces: consumes T2 `pushGuard` and `readPushGuardConfig`; depends on T1.

### T4 Allowlist delivery (lane server, RIG-1882)

- Add fields to `ProvisionAgentWorkspaceRequest` in
  `proto/compass/v1/compass.proto`, with comments in the `persona` style.
- A server helper derives them (D4). All three provision sites call it and
  overwrite the client value: `service.provisionAgent`,
  `lifecycleService.provisionAndStart`, `lifecycleService.reprovision`. A
  store error fails the provision.
- The Runner threads them like `Persona`: `configSpecBuilder.BuildSpec` into
  `runtime.AgentSpec`, `AgentHandle` accessors, `agentHost.agentEnv`, and
  `AgentEnv.execSpec`.
- Acceptance: a client-sent value is overwritten, as
  `service_placement_pgtest_test.go` proves for `persona`; `execSpec` omits
  both vars when the lists are empty.
- Interfaces:

```proto
repeated string forge_allow_owners = 5;
repeated string forge_allow_hosts = 6;
```

```go
func forgeAllowlist(ctx context.Context, st *store.Store, agent store.AccountID, host string) (owners, hosts []string, err error)
```

### T5 Skill text (lane agent, RIG-1882)

- After T3 and T4 merge, replace the `[TODO RIG-1882]` paragraph in
  `config/skills/jj/SKILL.md` with the enforced rule: the guard blocks a push
  to `main`, `master` or the default branch, every merge, and writes outside
  the allowlist.
- Interfaces: none.

## Tasks

- [ ] T1 SDK floor 18.3.5 and image hashes
- [ ] T2 `push-guard` module and unit tests
- [ ] T3 `cli.ts` wiring and session test
- [ ] T4 Allowlist from forge grants to the agent env
- [ ] T5 `jj` skill paragraph

## Decisions

- DL-507: two layers; the guard is an inline extension compiled into the
  agent binary and reaches task subagents (D1, D2).
- DL-508: writes outside the owner and host allowlist block; an empty list
  blocks every write (D4, D7). The allowlist source is OQ-1.
- DL-509: `main`, `master` and the default branch are protected for every
  owner, and every merge path is blocked (D5, D7).
- DL-510: `gh pr create` is redirected to `jj-vine submit` (D7).

## Open Questions

- **OQ-1 Allowlist source (load-bearing, RIG-5298).** (a) Server-derived from forge
  grants at provision (D4): one source with the token; costs two proto fields
  and wiring at three provision sites; a grant change applies only in a new
  container. (b) Runner-wide config, like model and batching: simple, but per
  deployment, not per user, and it can drift from grants. (c) A file in the
  user's config bundle (#2230 adds tenant and user overlays to that bundle):
  per user, but a second source that can disagree with grants. (d) The guard
  asks for the grants over the agent's gateway socket at session start and on
  a short cache: same source as (a), live with grant changes, no provision
  fields; costs a new gateway call and a block when the server is unreachable.
  **Recommendation: (a)** for the first cut, as the smallest change; (d) if
  live grant changes matter. Under every option an empty list blocks every
  write, which locks out a user who brings their own credential with no forge
  grants.
- **OQ-2 Input rewrites by mounted extensions (non-load-bearing).** Accept the
  limit above, or ask the SDK for a `tool_call` view of the final input.
  Recommendation: accept it; the bundle is the user's own code.
- **OQ-3 How a push's target refs are found (load-bearing, RIG-5298).** (a) As D5:
  re-implement git and jj ref selection (refspecs, `push.default`, moved
  bookmarks). No network; drifts from the tools. (b) Run the same command with
  `--dry-run` (`git push --dry-run --porcelain`, `jj git push --dry-run`) and
  read the refs it would update. Exact, but one network round trip per push,
  and `jj-vine submit` has no known dry-run, so it keeps (a)'s bookmark check.
  **Recommendation: (b)** for `git push` and `jj git push`, (a) for
  `jj-vine submit` and `jj-hp push`.
