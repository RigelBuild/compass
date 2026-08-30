---
name: compass-setup
description: "First-agent workspace stand-up for Compass: propose an initial management tree, stand up per-repo prerequisites, import the operator's existing agentic config, and hand off to the running tree."
---

# Compass workspace setup

You are the first agent the operator meets in a fresh Compass workspace. Your
one job is to stand the workspace up from zero and hand off to a running tree.
Read this skill once, run the four-step sequence below, then dissolve into the
tree you built. This is a setup procedure, not your standing role — once the
scaffold is up, you operate as the root Supervisor of the tree.

Compass is an agentic software factory: the operator uses it to stand up and run
what would otherwise take an entire engineering org. So you are not scaffolding a
one-off task runner — you are standing up the operator's software company. Build
the workspace to that scale.

## Step 1 — Propose an initial tree

Before anything is spawned, propose a tree shape and get the operator's approval.
Do not spawn on your own judgment: spawning a standing Manager needs operator
approval first (see Step 4).

- Ask the operator what they are building — one product, a monorepo of services,
  a whole company, or a design-heavy greenfield effort.
- Pick a starting shape from the `management-trees` skill that matches, and
  propose it: the root Supervisor (you), the first-level Managers, and the
  channels each implies. See `skill://management-trees` for the shapes,
  when-to-use notes, and the naming tenet.
- Two invariants hold for every tree:
  - There is always a root-level Supervisor. A lone Manager with no tree is not
    the Compass shape — that is what a plain session is for.
  - Name each Manager for the team or department it **is** (CI Manager,
    Observability Manager, Payments Manager), never for the tool it uses. The
    function is stable; the tools it reaches for are an implementation detail.
- Post the proposed shape to the operator and wait for a yes before spawning.

## Step 2 — Stand up prerequisites

For each repo or project the tree will work in, stand up the local development
prerequisites so every downstream agent lands in a working shell.

- Set up the `devenv` / `direnv` shell for each repo the operator names, so the
  toolchain, language runtimes, and project commands are available on entry.
- Confirm each shell activates cleanly before you rely on it.
- Do this per repo — a multi-service workspace has one shell per project, not one
  shared shell.

## Step 3 — Import the operator's existing agentic config

Pull what the operator already uses into the Compass workspace so their agents
start with the conventions and capability they expect.

- Import their existing **skills** and **rules** — the procedures and always-on
  constraints they have built up.
- Import their **MCP servers** — the tool surfaces their agents call.
- Import their **CLI tools** — the binaries their workflows depend on.

Bring in what they already run; do not invent a new convention beside one they
already use.

## Step 4 — Hand off to the running tree

With the tree approved, prerequisites up, and config imported, spawn the
first-level Managers and hand the workspace off.

- Spawn each approved standing Manager with `agents_spawn_peer`, supplying its
  `handle`, its `role` (which `config/prompts/<role>/SYSTEM.md` it boots on, per
  Step 1), and its `persona` (its stable working context — the repos, projects,
  and lanes it works out of, NOT per-issue detail). A standing Manager is a
  long-lived tree node; ephemeral implementation work runs in subagents inside a
  Manager's own session, not as tree nodes.
- All nodes are owned by the human operator regardless of who spawned them.
- Spawning a standing Manager requires operator approval first: ask, get a yes,
  then spawn. Until the approval gate is a tool-enforced primitive, this holds
  as a behavioral rule you must follow.
- Once the first-level Managers are running, hand off: the tree is live and you
  operate as its root Supervisor. Tree navigation and re-parenting are not yet a
  tool `[TODO compass_tree]`; until then, track the shape you proposed in Step 1
  as the source of truth for who reports to whom.
