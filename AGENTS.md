# AGENTS.md

Instructions for AI coding agents working in this repository. Humans should
skim this too — the conventions are not agent-specific. See
[CONTRIBUTING.md](./CONTRIBUTING.md) for the full contributor guide.

## The agent system

Compass runs, supervises, and orchestrates AI coding agents — so before the
build conventions below, orient on the model the code and prompts assume. It
lives under [`docs/concepts/`](./docs/concepts/README.md):

- **[Handles, accounts, and attribution](./docs/concepts/handle-vs-account.md)**
  — a handle names one running agent; `mintaka` is the shared forge *account*
  every agent bills through, not a handle. Read an agent's identity off a
  written `Owner:`/stamp, never a forge author/assignee field.
- **[The persona convention](./docs/concepts/persona.md)** — a persona is the
  agent's *stable* working context (repos / projects / lanes), layered over the
  role prompt; the churning per-issue detail lives in the tracker, never the
  persona.
- **[The agent tool set](./docs/concepts/tools.md)** — the native comms,
  presence, and lifecycle tools an agent drives Compass through, and the flow of
  using them.
- **[No human clicks](./docs/concepts/no-human-clicks.md)** — the org is
  standupable by agents through tools; the human holds only the security
  boundary (a secret's value for a named slot). See also [read-only
  inspection](./docs/concepts/read-only-inspection.md) and [review
  flow](./docs/concepts/review-flow.md).

## Toolchain and the gate

The toolchain is [devenv](https://devenv.sh) (nix underneath): it owns every
language toolchain (bun, node, moon, go — pinned in
`tools/toolchain/versions/*.nix`) plus everything else (the contract tooling,
the Go analysis tools, the linters). Enter the dev shell with `direnv allow`,
then `bun install`.

`moon run :ci` is the entire gate — build, lint, test, and contract drift across
the workspace. **Run it and get it green before declaring a change ready.** Use
`moon run <project>:<task>` to run one piece. The same task graph runs locally
and in CI.

## The compass.v1 contract: the owned door

`compass.v1` is the single, sole, owned door between any UI and the server.

- **Never hand-edit generated code** under `go/gen` or
  `packages/compass-client/src/gen`. It is generated and checked in.
- To change the contract: edit the schema under `proto/compass/v1`, run
  `moon run compass-proto:gen`, and commit the regenerated clients with the
  schema change. CI's drift gate (regenerate + `git diff`) fails if they
  disagree.
- UI code reaches the server only through the generated client
  (`@compass/client`) — never a raw socket or hand-written stub.

## Tests

Add tests for new behavior; a bug fix gets a regression test that fails before
the fix and passes after. Go: `go test ./...` in `go/`. TypeScript:
`bun test`.

## Code style

- Edit existing files over creating new ones; keep changes scoped.
- Comments explain non-obvious **why**, not **what**. No multi-paragraph
  docstrings.
- No backwards-compatibility shims, feature flags, or dead-code placeholders.
  If something is unused, delete it.

## Version control

- git-backed; jujutsu (jj) works against it too if you prefer.
- Conventional Commits subjects (`feat`, `fix`, `chore`, `refactor`, `docs`, … —
  optional `(scope)`). Reference a tracked issue in the PR body, not in source.
- During review, add a new commit per round of feedback rather than rewriting
  pushed history.

## Hygiene

- Describe behavior directly in code, commits, and docs. Do not name AI
  coding-agent products, and do not embed planning metadata (issue IDs, phase
  numbers, "as discussed") in source — those belong in commit subjects and PR
  bodies.
