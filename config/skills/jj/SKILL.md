---
name: jj
description: "Jujutsu (jj) for Compass agents: the working-copy-is-a-commit model, the per-agent colocated clone, the vcs_* tools that commit, sync, and submit one PR per bookmark, per-worker workspaces for parallel subagents, the issue-per-PR and additive-review-fix invariants, and the never-push-main / never-merge human gate."
---

# Jujutsu (`jj`) + stacked PRs on Compass

Use this for every version-control operation in your clone. Your clone is a
**jj** repo (a commit + bookmark model, no staging area). You drive it through
the **`vcs_*` tools**: they commit, rebase, push, and open or update one PR per
bookmark. Raw `jj` stays available for reads and for local history edits the
tools do not cover.

You never push. `vcs_submit` sends a git bundle of your commits to the Compass
Server, and the Server pushes them and writes the PRs. Your forge token is
read-only, so a raw `git push` or PR write is refused. Hard limits: **never push
or force-push `main`, never merge** — the human operator holds the merge gate.

## Mental model

- **Your clone is yours.** Each agent works in its own container with its own
  colocated clone: its own `.jj/` beside its own `.git/`. You coordinate with
  other agents at PR/merge time, not on a shared working copy.
- **The working copy is a commit.** jj has no staging area. The working copy is
  a live commit, `@`; every edit you make is *already* in `@`. `vcs_commit`
  describes `@`, sets its bookmark, and starts a fresh `@` on top.
- **jj is colocated over git.** `.jj/` and `.git/` share one object store. git
  sits in detached-HEAD state (jj drives, git follows) — expected.
- **Change ID vs commit ID.** Every commit has a stable **change ID** (survives
  rewrites) and a git **commit ID** (changes when the commit is rewritten).
  Address commits by change ID or by revset (`@`, `@-`, `main@origin`).
- **A bookmark is a branch, and one bookmark is one PR.** A stack is a chain of
  bookmarked commits, each based on the one below. It must be one linear line:
  `vcs_submit` refuses a fork.

## The tools

| Tool | Does |
| --- | --- |
| `vcs_status` | Your stack from the base to the tip: change, commit, bookmark, subject, conflict, empty. |
| `vcs_commit` | Describes `@` (or `<workspace>@`) from `subject` and `sections`, sets `bookmark`, then starts a new `@` on it. |
| `vcs_workspace` | `add`, `forget`, or `update-stale` one subagent working copy. |
| `vcs_sync` | Fetches `origin`, then rebases your own commits onto trunk or the foreign base they sit on. Reports conflicts. |
| `vcs_submit` | Sync, checks, the repo's pre-push hook, then pushes through the Server and creates or updates one draft PR per bookmark. |
| `vcs_pr_status` | PR, state, draft, checks, and authorship for each bookmark, bottom to top. |
| `vcs_retire` | Closes your PR for one bookmark, deletes its branch through the Server, then deletes the local bookmark. |

Issues, comments, reviews, and full PR reads stay on the `forge_*` tools
(`forge_list_issues`, `forge_create_issue`, `forge_get_pull_request`,
`forge_submit_review`, …).

## Invariants

These hold on every change, no exceptions.

- **Never push/force-push `main`; never merge.** You open PRs and drive them to
  merge-ready; the operator merges.
- **Every PR needs an issue.** `vcs_submit` takes `issues`, a map from bookmark
  to issue (`#12`, `owner/repo#12`, or a Linear key). It is required for every
  bookmark that has no PR yet. Find an issue that fits with
  `forge_list_issues`, or create one with `forge_create_issue` when none does.
  The Server checks that the issue exists and is open.
- **The first commit of a bookmark is the PR text.** `vcs_commit` on a new
  bookmark needs `sections`, one per heading of the repo's
  `.github/PULL_REQUEST_TEMPLATE.md`. That commit's subject becomes the PR
  title, with the issue appended, and its sections become the PR body. Later
  commits on the bookmark take `text` instead. The PR text is written once; to
  change it, call `vcs_submit` with `text`.
- **Review fixes are a NEW commit, never an amend.** Land each fix as a new
  commit on the bookmark with `vcs_commit` and submit again. An amend loses the
  link between a review thread and its code and shows no interdiff.
- **Always non-interactive.** When you run raw `jj`, pass `-m` to `describe` and
  `commit`, never use interactive flags (`split -i`, `squash -i`, a bare
  `resolve`), and pass `--no-pager` to read commands.
- **Do not edit a bookmark commit by hand.** Editing files while `@` sits on a
  bookmark commit silently amends it. `vcs_commit` always leaves a fresh `@`
  on top; after any raw `jj edit`, run `jj new <bookmark>` before you edit.
- **Stack changes with overlapping edits.** If two PRs edit the same lines of a
  file, put the later bookmark on top of the first PR's line.

## Everyday workflow

```text
vcs_sync                                    # fetch and rebase your commits onto trunk
# ... you or a subagent edit files ...
vcs_commit  { bookmark, subject, sections } # first commit: sections per template heading
vcs_submit  { issues: { <bookmark>: "#12" } }   # push through the Server, open a draft PR
vcs_pr_status                               # PR, checks, and authorship per bookmark
vcs_submit  { ready: true }                 # when review and CI are clean: tip leaves draft
```

- **Start a new line on trunk.** `vcs_sync` rebases your own commits onto the
  latest trunk. For a fresh change, run `jj --no-pager new 'main@origin'` first.
- **A stack is several bookmarks in one line.** Commit the first slice to
  bookmark `a`, then the next slice to bookmark `b`; `vcs_submit` with the top
  bookmark submits the whole line, bottom first, each PR based on the one below.
- **Your commits are pushed as you made them.** Nothing is squashed. Each PR
  shows each of its commits.
- **The pre-push hook runs inside `vcs_submit`.** It is the repo's own hook
  runner, with the same arguments and stdin a real push gives it. A failing hook
  stops the submit. A hook still running after 120 s is stopped and the submit
  goes on; the result says "pre-push hook timed out after 120 s". Then find out
  why the hook is slow and fix it in a change of its own. CI on the PR is the
  main gate.
- **Read before you submit.** `vcs_status` lists your stack; it must show only
  your commits. `vcs_submit` refuses conflicts, merge commits, empty
  descriptions, two bookmarks on one commit, commits above the tip that no
  bookmark publishes, and a forked stack.

## The review-fix loop

```text
vcs_commit { bookmark: <bookmark>, subject: "fix(scope): address review", text: "…" }
vcs_submit { bookmark: <top bookmark> }
```

The new commit re-triggers CI and the review bots on the new head. Full loop —
spawn the reviewer, fix the mechanical findings, surface the judgment calls,
iterate to merge-ready: `skill://review`.

## Workspaces — one per parallel subagent

A jj workspace is another working copy of the same clone, with its own `@`. Give
each parallel subagent its own workspace so they edit different slices without
colliding.

```text
vcs_workspace { action: "add", name: "ws-router", base: "main@origin" }
# ... the subagent edits files only, in ws-router ...
vcs_commit    { workspace: "ws-router", bookmark: "router", subject: "…", sections: {…} }
vcs_workspace { action: "forget", name: "ws-router" }   # then delete the directory
```

- **Subagents edit files only.** You commit their work with
  `vcs_commit { workspace }`, which runs in that workspace's root.
- **Bookmarks are shared across workspaces.** Give each subagent a distinct
  bookmark name.
- **A stale workspace** (it fell behind a concurrent operation) is repaired with
  `vcs_workspace { action: "update-stale" }`.

## Another agent's PR in your line

If your line sits on a PR another agent of the same owner authored, the Server
manages it: on submit it rebases and pushes that bookmark and tells the owning
agent. On a conflict it pushes nothing and asks that agent to resolve. You never
rebase, track, or push another agent's bookmark yourself.

## Retiring a PR

`vcs_retire { bookmark }` closes your PR and deletes its branch through the
Server, then deletes the local bookmark. It only works on a PR you authored, and
a retry after a partial run is safe. To replace a mixed or superseded PR, first
open its replacement and check that every hunk of the old PR is in it (or dropped
on purpose), then retire the old one with a comment on it that links the
replacement (`forge_comment_on_pull_request`).

## Moving an older PR onto the tools

A PR opened before the `vcs_*` tools carries a block between
`<!-- start jj-vine stack -->` and `<!-- end jj-vine stack -->` in its body.
The Server has no record that you wrote it, so the tools cannot update or close
it. Move each such line once:

1. Run `vcs_sync`, then `jj bookmark rename <old> <new>` for each bookmark in
   the line, so every PR gets a new head branch.
2. Run `vcs_submit` with each bookmark's issue in `issues`. Then, for each new
   PR, run `vcs_submit` with `text` naming its bookmark and a body line that
   links the PR it replaces; `text` covers one bookmark per call.
3. Check that the new line has every change in the old one. Then close each old
   PR with `gh pr close <number> --comment <new PR URL>`; the `vcs_*` tools
   cannot close a PR the Server has no record of.

Review history stays on the old PR. The move is done when none of your open
PRs carries that block.

## Command reference

| Task | Tool or command |
| --- | --- |
| Show your stack | `vcs_status` |
| Commit (first commit of a bookmark) | `vcs_commit { bookmark, subject, sections }` |
| Commit (later commit) | `vcs_commit { bookmark, subject, text }` |
| Rebase onto latest trunk | `vcs_sync` |
| Push and open or update the PRs | `vcs_submit { issues }` |
| Take the tip PR out of draft | `vcs_submit { ready: true }` |
| Replace one PR's title and body | `vcs_submit { text: { bookmark, subject, sections } }` |
| PR, checks, authorship | `vcs_pr_status` |
| Close a PR and delete its branch | `vcs_retire { bookmark }` |
| Add or remove a subagent workspace | `vcs_workspace` |
| Start a change off latest main | `jj --no-pager new 'main@origin'` |
| Review a change's diff | `jj --no-pager diff --stat` |
| Split an over-large commit by file | `jj split <file>...` |
| Undo the last jj operation | `jj undo` |
| Rewind the repo to an earlier state | `jj --no-pager op log` then `jj op restore <op-id>` |
