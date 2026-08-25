# AGENTS.md

Instructions for AI coding agents working in this repository. Humans should
skim this too — the conventions are not agent-specific. See
[CONTRIBUTING.md](./CONTRIBUTING.md) for the full contributor guide.

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

## Pre-GA posture: nothing is frozen

This is a pre-GA, greenfield prototype. Interfaces are **not** frozen —
reimagine them freely when a better shape presents itself; describe the current
contract as what it *is* today, never as a bar on new design. The one
change-controlled artifact is a **merged design record** under `docs/designs/`:
a merged record is amended by adding a new record, not rewritten in place. The
record is change-controlled; the interface it describes is fair game.

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
