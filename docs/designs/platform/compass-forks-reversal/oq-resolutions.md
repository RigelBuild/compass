# Compass forks reversal — Open-Question resolutions (RIG-2336)

Status: Active

Amends [`design.md`](./design.md) (the frozen forks-reversal record, PR #434,
merged as `9d48f9a9`). That record froze with its two **load-bearing** Open
Questions unresolved — the pre-freeze ruling gate (`skill://design` § "No merge
with open questions") did not run before merge. A frozen record is never edited
in place (`skill://design`: "there is no folding-in after merge — a later change
ADDS a new record"), so the binding rulings are recorded here. The frozen
record's `## Open Questions` §1 and §2 are **superseded by this file** and read
as history; the L1/L2/L3 executors build against the decisions below.

## Decision 1 — oh-my-pi: consume a Rigel-tagged npm release (supersedes OQ1)

**Ruled (Matt, 2026-08-21): consume — not drop, and not a `github:` repo pin.**
Cut a Rigel-owned tagged **npm** release of the oh-my-pi coding-agent package
and consume *that* from compass; repoint the credential-denylist generator to
read the schema from the Rigel npm release rather than the vendored subtree.

This is a third shape distinct from both options the frozen record framed (its
recommended "drop + repoint to the upstream-`npm`-installed schema" and its
fallback "pin `github:RigelBuild/oh-my-pi`"): the schema source becomes a
**Rigel-controlled npm release**, so the denylist tracks a release Rigel cuts,
not upstream's latest resolved version and not a raw git rev.

Impact on **L3** (`design.md:437-490`):

- The generator repoint target (`go/internal/store/gen_credential_keys.go:42`
  `schemaRelPath`) resolves to the Rigel npm package's installed
  `settings-schema.ts` (pin the Rigel release in
  `packages/compass-agent/package.json:19` in place of the current
  `@oh-my-pi/pi-coding-agent` pin), not the vendored
  `forks/oh-my-pi/...` path and not upstream npm.
- The hardcoded generated-header literal (`gen_credential_keys.go:74`, which
  emits `credential_keys_gen.go:3`) and the provenance at
  `agent_config.go:656-659` + `agent_config_test.go:505-518` update to cite the
  Rigel npm release, not `forks/oh-my-pi/...`.
- **Expect a denylist diff, re-review `TestCredentialKeysMatchSchema`'s `want`
  set (`agent_config_test.go:508-516`) in the same PR** — the exact schema the
  Rigel release ships is what the denylist must track. Version-gap handling is
  unchanged from the frozen record: a diff is the expected, correct outcome, not
  drift to paper over.
- The deletion set (`forks/oh-my-pi/` incl. `moon.yml`; the `oh-my-pi-fork`
  entry and its dedicated comment at `.moon/workspace.yml:86-89`) and the prose
  sweep from the frozen record's L3 still apply unchanged. **One line-range
  correction to the frozen record's L3:** it cites this entry as
  `.moon/workspace.yml:80-83`, but at the current tree that range is the tail of
  the *shared* `VENDORING A NEW FORK` comment block (`:70-83`, not oh-my-pi
  specific — it must NOT be deleted by L3; it belongs to L4's shared-machinery
  teardown, `design.md:500-503`). The oh-my-pi-specific lines are the comment at
  `:86-88` and the `oh-my-pi-fork: 'forks/oh-my-pi'` entry at `:89`. L3 deletes
  `:86-89` only.
- **Prerequisite:** the Rigel npm release must exist and be published before L3
  repoints. Coordinate the release cut with forge (shared RigelBuild fork
  convention) the same way L0a/L1 coordinate the devenv/nix2container repos.

## Decision 2 — raw `nix run` sites: install the fork tools, pinned (supersedes OQ2)

**Ruled (Matt, 2026-08-21): install the fork tools as pinned packages; do not
keep raw `nix run` literals.** The flake-input half of OQ2 was already settled
(converge on orion's lockfile-pinned `github:RigelBuild/<fork>` default). For
the raw-CLI half — the six sites that invoke a fork CLI with `nix run` and so
bypass `devenv.lock` — the resolution is to **install** the two fork tools
(devenv's patched CLI carrying compass's container module, and
`skopeo-nix2container`) from the same lockfile-pinned `github:RigelBuild/<fork>`
input and invoke them by explicit name, rather than the frozen record's
recommended compass-side wrapper flake or the alternative
literals-plus-CI-assert.

Why this is strictly cleaner than either framed option: the pin rev then lives
in **one** place — the lockfile input, exactly like every other dependency — so
there is nothing to drift, no wrapper flake to maintain, and no CI
rev-equality assert to keep. The "six raw sites bypass the lockfile" problem
dissolves because no raw `nix run path:...` / `github:.../<rev>` literal remains.

The reason the sites use `nix run` today is a PATH collision, not a hard
requirement: the dev shell is itself booted by an **upstream** devenv, so a bare
`devenv container build` would run upstream's modules; `nix run
path:../forks/devenv#devenv` was the escape hatch to force the fork's modules
(`agent-image/devenv.yaml` comment). Installing the fork CLI explicitly (a
distinctly-named binary from the pinned input) removes the collision without a
raw invocation.

Impact on **L1** (`design.md:348-386`) and **L2** (`design.md:388-435`):

- The six raw-`nix run` sites — `agent-image/publish.sh:32,50-51`,
  `agent-image/moon.yml:44`, `devenv.nix:461`, `.github/workflows/ci.yml:812`,
  `.github/workflows/publish-agent-image.yml:139,160`,
  `tools/agent-image-env-gate/index.ts:100,118` — install and invoke the fork
  tools by explicit name from the `github:RigelBuild/{devenv,nix2container}`
  input pinned in `agent-image/devenv.lock`; no `path:...` or `/<rev>` literal
  remains at any of them.
- The frozen L1/L2 gate lines "the OQ2 pin shape applied (wrapper flake, or a
  lock-vs-URL rev-consistency assert)" resolve to: **no raw literal present; the
  installed tool resolves the lockfile-pinned rev; single source of truth is
  `agent-image/devenv.lock`.**
- The executor validates the devenv-CLI-vs-modules mechanics (how to force the
  fork's container module through the installed CLI) at implementation time —
  that is an execution detail, not a further design fork.
- Everything else in L1/L2 (the `devenv.yaml`/`devenv.lock` repoints, the
  deletion sets, the mirrored env-gate globs, the comment sweeps) is unchanged
  from the frozen record.

## Not changed

- **OQ3** (secret-scanning re-widening) was non-load-bearing; the merge ratified
  its deferral. Unchanged — handled in L4 as the frozen record describes.
- **L0a / L0b / L4** carry no OQ dependency and are unchanged from the frozen
  record. L0b is already satisfied (`RigelBuild/nix2container` `main` =
  `8f4a6fd7`).
