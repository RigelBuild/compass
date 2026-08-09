---
name: jj
description: "Jujutsu (jj) for Compass agents: the working-copy-is-a-commit model, the per-agent colocated clone, bookmark-per-PR stacking through jj-vine submit, per-worker workspaces for parallel subagents, the rebase-before-submit and additive-review-fix invariants, and the never-push-main / never-merge human gate."
---

# Jujutsu (`jj`) + stacked PRs on Compass

Use this for every version-control operation in your clone. You drive commits and
branches with **`jj`** (a commit + bookmark model, no staging area) and open/update
PRs with **`jj-vine submit`** — one PR per bookmark, with correct stacked bases and
a navigable stack diagram. The remote is your repo's forge (GitHub), reached under
your agent's own forge identity.

You may commit, create bookmarks, and push/submit your own feature branches, then
run the review loop (`skill://review`) to merge-ready. Hard limits: **never push or
force-push `main`, never merge** — the human operator holds the merge gate.

## Mental model

- **Your clone is yours.** Each agent works in its own container with its own
  colocated clone: its own `.jj/` beside its own `.git/`. There is no shared
  op-store across agents — no cross-agent rebase, no divergent change IDs. You
  coordinate with other agents at PR/merge time, not on a shared working copy.
- **The working copy is a commit.** jj has no staging area and no "unstaged
  changes." The working copy is a live commit, `@`; every edit you make is
  *already* in `@`. You do not `add` — you `describe` (give `@` a message) and,
  when done, seal it by starting a fresh `@` on top with `jj new` / `jj commit`.
- **jj is colocated over git.** `.jj/` and `.git/` share one object store, so
  `git` tooling, the forge, and CI see your commits normally. git sits in
  detached-HEAD state (jj drives, git follows) — expected, not a problem.
- **Change ID vs commit ID.** Every commit has a stable **change ID** (survives
  rewrites — the left column of `jj log`) and a git **commit ID** (changes when
  you amend). Address commits by change ID or by revset (`@`, `@-`,
  `main@origin`), never by pasting a commit hash a later edit invalidates.
- **A bookmark is a branch.** A **bookmark** is a named pointer at a commit — the
  branch that becomes one PR. jj does **not** advance a bookmark automatically when
  you commit on top; you move it yourself with `jj bookmark move`.
- **One bookmark = one PR.** A stack is a chain of bookmarked commits, each based
  on the one below; `jj-vine submit` submits them with correct stacked bases.
- **The core verbs:** `jj new` (start a fresh commit), `jj describe -m` (set the
  message), `jj commit -m` (describe `@` and start a new commit on top),
  `jj bookmark create/move` (name/advance the branch), `jj-vine submit`
  (open/update the PR + render the stack diagram).

## Invariants

These hold on every change, no exceptions.

- **Never push/force-push `main`; never merge.** Merge is the human operator's
  gate. You open PRs and drive them to merge-ready; the operator merges from the
  Compass PR panel. Work only on feature branches.
- **Always non-interactive.** Pass `-m` to `jj describe` / `jj commit` — a bare
  invocation opens `$EDITOR` and hangs a headless agent. Never use interactive
  flags (`jj split -i`, `jj squash -i`, `jj resolve` without a tool): they open a
  TUI. jj paginates by default, so run read commands with `--no-pager` (e.g.
  `jj --no-pager log`) or expect a pager.
- **Review fixes are a NEW commit, never an amend.** jj's native idiom is
  amend + force-push; do **not** adopt it. Amending a pushed commit rewrites
  history, drops the correspondence between a review thread and its code, and
  shows no interdiff on the PR. Land each fix as a new commit and advance the
  bookmark (see the review-fix loop). This matches the Compass convention: add a
  new commit per round of feedback rather than rewriting pushed history.
- **The auto-amend footgun: `jj new <bookmark>` before you edit.** Editing files
  while `@` sits on a bookmark commit silently **amends** that commit. Always
  start work on a fresh commit — `jj new 'main@origin'` for a new change, or
  `jj new <bookmark>` for a fix on top of an in-review head — before touching any
  file.
- **Rebase onto latest `main` before every submit.** `jj git fetch` then rebase
  your work onto `main@origin` before you submit, so the PR is against current
  trunk and CI runs on a live base. Compass CI checks out full history; a stale
  base can green locally and fail the merge gate.
- **Stack aggressively, but re-verify each head after a rebase.** Chain dependent
  work into a stack instead of one fat PR. After you rebase a stack onto new
  `main`, a lower commit can shift what an upper commit sees — re-inspect each
  head's diff (`jj --no-pager diff -r <bookmark> --stat`) and re-run the affected
  checks on it before you re-submit. A stack is only as merge-ready as its
  weakest re-verified head.
- **The commit message IS the PR.** `jj-vine` derives the PR **title from the
  commit subject** and the **body from the commit body** — no separate authoring
  step. Write the message like a good PR: a Conventional Commits subject
  (`feat(scope):`, `fix(scope):`, …), then a GitHub-Flavored-Markdown body that is
  **not** hard-wrapped (one line per paragraph, blank line between — a 72-col wrap
  becomes a wall of forced breaks on the PR). Reference the tracked issue in the
  body. Get it right *before* the first submit; the body is written once.

## Clone setup

Each agent clones its own colocated copy of the repo in one step:

```sh
jj git clone --colocate <repo-url> <dest>
cd <dest>
```

- **`--colocate` makes `.jj/` and `.git/` share one store**, so `git`, the forge,
  and CI see your commits. Pass it explicitly — it is self-documenting and immune
  to a config change.
- **`jj git clone` imports the default remote bookmark** (`main`) and sets it as
  `trunk()`; your local `main` tracks `main@origin` with no extra step. Start work
  off `main@origin`.
- Auth flows through the git credential helper for your agent's forge account on
  the HTTPS remote, so pushes need no extra setup.

## Everyday workflow

```sh
jj git fetch                                  # pull latest main@origin
jj new 'main@origin'                          # fresh commit off latest main (sidesteps auto-amend)
# ... edit files ...
jj describe -m "type(scope): summary" \
  -m "what changed and why — GFM, length tracks the change; reference the issue"
jj bookmark create <issue>-<slug> -r @        # name the branch on your commit
jj-vine submit <issue>-<slug>                 # push (through the CI gate) + open the PR
```

- **Start every change with `jj new 'main@origin'`.** It bases you on the latest
  fetched `main` and it sidesteps the auto-amend hazard. Verify with
  `jj --no-pager diff --stat` before you submit — it must list only your files.
- **Every push runs the local gate first, through `jj-hp`.** `jj-vine submit` is
  your push path, and Compass routes its push through **`jj-hp`** (which runs the
  hooks before pushing) — so every submit runs the `hk` gate (`moon ci` over the
  affected subset) before the push completes, the same task graph CI runs on the
  PR. Never bypass it with a bare `jj git push`, which skips the gate. A red gate
  blocks the push; get it green first.
- **`@` accumulates all your edits into one commit** — right for a single-purpose
  PR. To split a change into multiple commits, seal each step with
  `jj commit -m "…"` (describes `@`, starts a fresh `@` on top) rather than
  editing one fat `@`; to carve an already-too-big commit by file,
  `jj split <file>...` (passing filesets keeps it non-interactive — a bare
  `jj split` opens a diff editor).
- **Run a jj sequence in one shell.** jj snapshots the working copy on each
  command; splitting `describe` / `bookmark` / `submit` across separate subshells
  can strand a stale working copy. Chain them.
- **Branch naming: one bookmark per PR, named for its slice.** Lead with the
  issue reference and a short kebab-case description (`sea-1732-jj-skill`); no
  `user/` prefix. In multi-agent work, keep the name descriptive of the lane so
  peers can tell stacks apart.

`[TODO SEA-1882]` Distinct from the `jj-hp` CI gate above, the
push-*authorization* guard that enforces the never-push-`main` and
owner-allowlist invariant is a bundled OMP extension that intercepts your
push/merge commands in-container and hard-blocks a violation — load-bearing,
because Compass cannot rely on a user's own repo branch protection (recommended
as a server-side backstop, never guaranteed). Until that extension ships, the
invariant is **behavioral**: you enforce it; nothing blocks you. Do not invent
or run a push-*authorization* guard wrapper that is not provisioned in your
clone.
`jj-vine submit` (gated through `jj-hp`) remains the push path.

`[TODO SEA-1734]` Reading PR and review state (checks, threads, merge status) uses
the Compass forge tools, which land pre-Dogfood as an operator-provisioned
surface. Name and use the concrete tools once they land; until then, drive the
review loop through `skill://review`.

## The review-fix loop

Never amend a pushed commit. Land each review fix as a NEW commit and advance the
bookmark:

```sh
jj new <bookmark>                             # start the fix ON the PR head (avoids auto-amend)
# ... edit files to address a finding ...
jj describe -m "fix(scope): address review"
jj bookmark move <bookmark> --to @            # advance the bookmark to the fix commit
jj-vine submit <bookmark>                     # push + refresh the PR on the new head
```

The new commit re-triggers CI and the review bots on the new head SHA (an amend
would not, and would break thread anchoring). Full loop — spawn the reviewer,
auto-fix the mechanical, surface the judgment calls, iterate to merge-ready:
`skill://review`.

## Workspaces — one per parallel subagent

`jj workspace` attaches **additional working copies to the same clone** — each with
its own `@` working-copy commit, all sharing one op-store, bookmark set, and
`.git`. This is how you run implementation in **parallel subagents** (the `task`
mechanism) without colliding on the single working copy: give each subagent its
**own workspace** so they edit different slices concurrently, each on an isolated
`@`, with no cross-workspace auto-amend.

```sh
jj workspace add ../ws-router -r 'main@origin'   # subagent A works in ../ws-router
jj workspace add ../ws-query  -r 'main@origin'   # subagent B works in ../ws-query, concurrently
# ... each subagent edits + describes + bookmarks IN ITS OWN workspace dir ...
jj --no-pager diff -r 'ws-router@' --stat        # review each by <name>@ , from the default workspace
jj --no-pager diff -r 'ws-query@'  --stat
jj-vine submit <router-bookmark>                 # submit each slice as its own PR / stack entry
jj workspace forget ws-router && rm -rf ../ws-router   # teardown: forget, then delete on disk
```

- **Base with `-r <rev>`.** `jj workspace add <path> -r 'main@origin'` puts the new
  `@` on latest main (or a stack base). With no `-r`, the new workspace shares the
  *current* workspace's parents — rarely what you want for an independent slice.
- **Address a workspace by `<name>@`.** The default workspace is `@`; each added
  one is `<name>@` (the destination's basename, or `--name`). Inspect a subagent's
  work with `jj --no-pager diff -r '<name>@'` without leaving your own workspace.
  `<name>@` reflects that workspace's *last jj snapshot*, and jj snapshots a
  working copy only when a jj command runs **in that workspace** — so review after
  the subagent has described/bookmarked; a peek before any jj command ran there
  shows a stale or empty `@`.
- **Each `@` is isolated; bookmarks are not.** An edit in one workspace lands in
  *its* `@` only. But all workspaces in the clone **share one bookmark set** — give
  each subagent a distinct bookmark name and never `jj bookmark move` a bookmark
  another workspace owns.
- **Teardown is two steps.** `jj workspace forget <name>` stops tracking that
  working-copy commit; the directory on disk is **not** deleted — `rm -rf` it
  separately. Forgetting a workspace never touches your bookmarks.
  `jj workspace list` shows every attached working copy; `jj workspace update-stale`
  repairs one whose working copy fell behind a concurrent op.

## Command reference

| Task | Command |
| --- | --- |
| Clone (colocated, first time) | `jj git clone --colocate <url> <dest>` |
| Fetch latest main | `jj git fetch` |
| Start a change off latest main | `jj new 'main@origin'` |
| Set the commit message (= PR title+body) | `jj describe -m "subject" -m "body"` |
| Seal `@` and start a new commit on top | `jj commit -m "…"` |
| Name the branch | `jj bookmark create <name> -r @` |
| Advance a bookmark to a new commit | `jj bookmark move <name> --to @` |
| Local gate (runs inside `jj-vine submit`, not run directly) | `jj-hp push` *(automatic)* |
| Open/update the PR (+ stack diagram) | `jj-vine submit <name>` |
| Submit the whole stack | `jj-vine submit --tracked` |
| Preview a submit (no side effects) | `jj-vine submit <name> --dry-run` |
| Inspect your PRs / stack state | `jj-vine status --tracked` |
| Show your commits + bookmarks | `jj --no-pager log` |
| Review a change's diff | `jj --no-pager diff --stat` |
| Rebase your work onto latest main | `jj git fetch && jj rebase -b @ -d 'main@origin'` |
| Split an over-large commit by file | `jj split <file>...` |
| Add a workspace for a subagent (own `@`) | `jj workspace add <path> -r 'main@origin'` |
| Review a workspace's working copy | `jj --no-pager diff -r '<name>@' --stat` |
| Tear down a workspace (then `rm -rf` it) | `jj workspace forget <name>` |
| Undo the last jj operation | `jj undo` |
| Rewind the repo to an earlier state | `jj --no-pager op log` then `jj op restore <op-id>` |
