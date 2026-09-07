# RIG-2546 — compass devenv source unify + CI devenv-CLI DRY

Status: Active

## Problem / Intent

The logic that materializes the devenv CLI from a lockfile exists twice with two
divergent shapes, and one copy hand-pins a rev. `.github/workflows/renovate.yml`
resolves the devenv source from the ROOT `devenv.lock` at runtime — its step
comment says it "tracks whatever devenv source the root lock names with no edit
here" (renovate.yml:90-91) — while `.github/workflows/ci.yml`'s agent-image seed
step hardcodes the fork rev inline:

> `( cd agent-image && nix run github:RigelBuild/devenv/15a81f3e15619187fcbe10c2eac40878e0b4ce28#devenv -- container copy agent -r "docker-archive:/tmp/compass-agent-seed.tar:" )` — ci.yml:1153

The same hand-pinned rev appears in **four more executable sites** (repo-wide
grep for `15a81f3e` this session): `agent-image/moon.yml:45` (`command: 'nix run
github:RigelBuild/devenv/15a81f3e…#devenv -- container build agent'`),
`agent-image/publish.sh:62` (`BUILD_OUT="$(nix run
github:RigelBuild/devenv/15a81f3e…#devenv -- container build agent)"`),
`devenv.nix:552` (the `dogfood:agent-image` task — whose own comment,
devenv.nix:534-536, claims the pin "cannot diverge from the fork source the
agent-image module set is pinned to", precisely the drift this record proves IS
possible), and `tools/agent-image-env-gate/index.ts:103` (the env gate's `nix
run … container build`, which runs in the CI moon graph:
`tools/agent-image-env-gate/moon.yml:45` `deps: ['install',
'compass-agent-image:build']`). That is **five executable hand-pins**
(ci.yml:1153 + these four) plus the one legitimate source-of-truth, the
`agent-image/devenv.lock` devenv node
(`"owner":"RigelBuild","repo":"devenv","rev":"15a81f3e15619187fcbe10c2eac40878e0b4ce28"`
at devenv.lock:71-74, read this session via `jq '.nodes.devenv.locked'`). Today
all five pins happen to match that lock node, but the next `devenv update` moves
the lock rev and none of the hand-pins move with it.

**Concrete drift failure mode:** after a lock update, CI seeds/builds/publishes
the agent image through a devenv CLI at a rev the lock no longer names — and,
worst, `agent-image-env-gate` (which runs in the CI moon graph) would *validate*
an image built at a different devenv rev than the one `agent-image/moon.yml:45`
builds, greening the wrong artifact class. Silent divergence, exactly the class
renovate.yml's lock-resolved pattern was built to prevent.

Separately, compass carries two devenv *sources*: the ROOT `devenv.yaml` has no
`devenv:` input override (verified this session: its `inputs:` block at
devenv.yaml:9-42 lists only `nixpkgs`, `go-overlay`, `nix2container`, `hk`), so
the root shell uses the framework default upstream `github:cachix/devenv` —
root `devenv.lock` devenv node: `"owner":"cachix","repo":"devenv","rev":"0bf6765ce7071d98ed137ecfe02d1e435007c971"`.
`agent-image/devenv.yaml:45-46` overrides it to the fork
(`devenv:` / `url: github:RigelBuild/devenv`). Unifying onto one source is ruled
(RD-1, Option A — unify); this record's CLI-binding DRY is designed to hold
under either source regardless.

## Global Constraints

- **No-bash-gate** (`rule://scripts-ts-over-bash`, enforced repo-wide): the
  extracted helper is a bun TypeScript tool with the construction/execution
  split + `bun test`, modeled on the existing
  `tools/toolchain/parity.ts` / `parity-core.ts` / `parity-core.test.ts` shape —
  parity.ts:5-8: "This is the thin execution shell — read files, run probes,
  exit. All parsing and comparison … lives in ./parity-core.ts, which is pure
  and unit-tested".
- **Dependency-free tool**: like `@compass/toolchain-parity`
  (tools/toolchain/package.json:5: "Dependency-free by design — it must be able
  to run before `bun install` has"), the helper runs in renovate.yml *before*
  Renovate's postUpgradeTasks do `bun install`, so it may import only bun/node
  builtins.
- **Design-ledger gate**: `tools/design-ledger-gate/index.ts:21` — a PR
  touching a governed record "MUST also touch DECISIONS.md, unless it declares
  `Ledger-impact:`". The PR landing this record (and the impl PR flipping
  decisions Active) must handle the ledger delta. Compass has NO spec-impact
  and NO root-checks gate (`jj file list -r main@origin tools` shows neither),
  so no Spec-impact line applies.
- **Fork is a strict superset of upstream devenv**:
  agent-image/devenv.yaml:43-44 — "The fork keeps upstream's values as
  DEFAULTS, so a consumer setting none of the new options is byte-identical."
  The root shell is therefore byte-identical under either FORK 1 outcome.
- **nix2container fork is out of scope**: both devenv.yaml files pin
  `github:RigelBuild/nix2container` (root devenv.yaml:28-29,
  agent-image/devenv.yaml:21) — only the `devenv` input/CLI binding is in play
  here.
- **bun availability at every call site**: renovate.yml bootstraps the language
  toolchains before the devenv step (renovate.yml:64-80, `nix eval … gate-tools.nix
  langs` → `$GITHUB_PATH`); ci.yml's dogfood-e2e job does the same
  (ci.yml:1032-1042) before the seed step at 1153. The publish workflow does
  NOT (publish-agent-image.yml:98-148 bootstraps only nix + skopeo) — relevant
  only if the consumer set extends to publish.sh (ruled RD-2 — publish.sh is in scope, T2a).

## Approach

One tool — `tools/toolchain/devenv-cli/` — becomes the single place that knows
how to turn "the devenv node of a named devenv.lock" into a usable devenv CLI.
Both workflows collapse to a call into it; no workflow (or moon task, if the
consumer set extends) carries a hand-pinned rev or its own `jq`/`nix build`
incantation. The design is deliberately source-agnostic: the tool reads
whatever `owner/repo/rev` the named lock's `.nodes.devenv.locked` carries, so
it is byte-for-byte the same under either FORK 1 outcome (root on upstream, or
root on the fork) — unifying the source later changes lock contents, never this
tool or its callers.

### Tool shape (parity-gate pattern)

Following the `tools/toolchain/parity.ts` split and the
`tools/renovate/refresh-devenv-nixpkgs.core.ts` precedent for lock parsing
(refresh-devenv-nixpkgs.core.ts:25 already parses a devenv.lock node — the
inner `nixpkgs-src` rev — "throwing loudly if the node or a 40-hex rev is
absent"):

```text
tools/toolchain/devenv-cli/
  index.ts        # thin execution shell: argv → resolve → nix build → print
  core.ts         # pure: lock JSON → flakeref; argv → parsed request
  core.test.ts    # bun test over core.ts
  moon.yml        # typecheck + test + ci tasks (parity-gate wiring)
  package.json    # dependency-free, @types/bun + typescript only
  tsconfig.json
```

(A new directory rather than more files in `tools/toolchain/` proper: every
existing compass tool is its own moon project — see `jj file list -r
main@origin tools` — and `tools/toolchain`'s moon.yml is scoped to the parity
gate. Matches `rule://file-directory-organization`.)

### Exact interface

`core.ts` (pure, unit-tested, no I/O):

```ts
/** The devenv node's locked coordinates, as a nix flakeref fragment. */
export interface DevenvSource {
  readonly owner: string;
  readonly repo: string;
  readonly rev: string; // 40-hex, validated
}

/**
 * Parse `.nodes.devenv.locked` out of a devenv.lock's text. Throws loudly on
 * missing node, missing/short rev, or non-github type — a shape change must
 * fail the caller, never resolve a stale or wrong source (the same posture as
 * refresh-devenv-nixpkgs.core.ts's innerNixpkgsRev).
 */
export function devenvSource(lockText: string): DevenvSource;

/** `github:<owner>/<repo>/<rev>#devenv` for the parsed node. */
export function flakeref(src: DevenvSource): string;

/** What the caller wants printed. */
export type Mode = "flakeref" | "bin-dir";

export interface Request {
  readonly lockPath: string; // e.g. "devenv.lock" | "agent-image/devenv.lock"
  readonly mode: Mode;
}

/** Parse argv (`--lock <path> --mode <flakeref|bin-dir>`); throws on anything else. */
export function parseArgs(argv: readonly string[]): Request;
```

`index.ts` (thin shell, mirrors parity.ts's exec style):

```ts
// bun tools/toolchain/devenv-cli/index.ts --lock <path> --mode <flakeref|bin-dir>
//   mode=flakeref → print `github:<owner>/<repo>/<rev>#devenv` (no build, no network)
//   mode=bin-dir  → `nix build --no-link --print-out-paths <flakeref>`, create a
//                   temp dir holding a single `devenv` symlink → its bin, print that dir
// stdout: exactly one line (the value); all diagnostics to stderr; exit 1 on any failure.
```

`mode=flakeref` is pure resolution — the caller owns the `nix run`. This keeps
the ci.yml seed step's execution semantics (a `nix run … -- container copy`
from `agent-image/`, cwd-dependent per agent-image/moon.yml:37-39) untouched:
the tool decides *which* devenv, the step decides *how* to run it.
`mode=bin-dir` does the build because the PATH caller needs a realized store
path, and it owns the single-binary shim (a temp dir with one `devenv` symlink,
not the raw `<out>/bin`) so appending it to `$GITHUB_PATH` cannot put devenv's
whole closure bin dir on PATH ahead of the parity-pinned toolchain — see RD-3
for why that invariant is load-bearing. Honesty note: `bin-dir` has exactly one
caller today (renovate.yml), so relocating its build step behind the tool is
*relocation into a tested seam*, not deduplication — the genuine dedup is the
lock-parse/flakeref-compose `core.ts`, shared by both modes and by pin 5's
direct `import`. bin-dir earns its keep once the PATH-consumer set grows (T2a).

### Call-site collapses

**renovate.yml "Put the devenv CLI on PATH" (today lines 92-103, a jq+nix+ln
blob) becomes:**

```yaml
run: |
  bindir=$(bun tools/toolchain/devenv-cli/index.ts --lock devenv.lock --mode bin-dir)
  echo "$bindir" >>"$GITHUB_PATH"
```

(This is the promised two lines. Today's step wraps the resolve in a
`$RUNNER_TEMP/devenv-shim/bin` symlink to one named `devenv` binary
(renovate.yml:100-102) — a deliberate single-binary exposure, not an accident of
the build step (`$out` was already in a variable at renovate.yml:99). The tool
preserves that invariant by owning the shim itself (§Exact interface, RD-3), so
`bindir` is a one-binary dir, safe to append to `$GITHUB_PATH`. The "Assert
devenv is on PATH" step at renovate.yml:105-109 stays as-is. bun is already on
PATH here: the toolchain bootstrap step precedes it, renovate.yml:64-80.)

**ci.yml agent-image seed (today line 1153, hand-pinned) becomes:**

```yaml
src=$(bun tools/toolchain/devenv-cli/index.ts --lock agent-image/devenv.lock --mode flakeref)
( cd agent-image && nix run "$src" -- container copy agent -r "docker-archive:/tmp/compass-agent-seed.tar:" )
```

No hand-pinned rev; the step now tracks `agent-image/devenv.lock` the same way
renovate.yml tracks the root lock. bun is on PATH in this job
(ci.yml:1032-1042's toolchain bootstrap precedes 1153).

**The four other hand-pins** (`agent-image/moon.yml:45`,
`agent-image/publish.sh:61-62`, `devenv.nix:552`, and
`tools/agent-image-env-gate/index.ts:103`) are the same drift class but
different execution contexts (moon and devenv.nix command strings can't shell
out to compose a flakeref; publish.sh runs in a workflow with no bun bootstrap,
publish-agent-image.yml:98-148). This record's scope covers them all —
RD-2 (full kill, T2a); the tool's interface already supports them (the shell-string sites use
`--lock agent-image/devenv.lock --mode flakeref`; `agent-image-env-gate` is
itself a bun tool and can `import { devenvSource, flakeref }` from `core.ts`
directly — no CLI hop).

### Fork-outcome independence

Under FORK 1 = unify, the root `devenv.lock`'s devenv node becomes
`RigelBuild/devenv/<new-rev>`; under keep-split it stays
`cachix/devenv/0bf6765c…`. In both cases every consumer reads its lock through
the same `devenvSource()` and neither workflow changes. The only FORK 1 code
delta is `devenv.yaml` + a relock (task T4).

## Alternatives considered

- **(a) Do nothing / keep the two bindings separate.** Rejected: the drift
  hazard is live, not hypothetical — ci.yml:1153's rev is only correct today by
  coincidence of the last manual sync, and the next `devenv update` in
  agent-image/ silently decouples the CI seed from the lock (the
  RIG-1304/RIG-2245 regression class renovate.yml:106-108 names is exactly
  "silent divergence at a setup seam").
- **(b) A shared bash script.** The resolve logic (JSON parse → validate →
  compose flakeref → build → locate bin, with loud failure on shape drift) is
  real logic with branches and error paths — `rule://scripts-ts-over-bash`
  routes it to a bun TS tool, and compass's no-bash-gate enforces that.
  publish.sh's bash carve-out (publish.sh:5-14) is explicitly for
  nix-orchestration glue in a zero-bun-infrastructure directory; a *shared
  repo-wide helper* has no such rationale.
- **(c) Make ci.yml resolve from the lock inline (copy renovate.yml's jq blob,
  pointed at agent-image/devenv.lock).** Removes the hand-pin but duplicates
  the resolve logic a second time in a second dialect — two copies of the same
  jq program that must be kept identical by review. That is the DRY failure
  this issue exists to close, minus the rev pin.
- **(d) Put the helper logic in `tools/renovate/` next to
  refresh-devenv-nixpkgs.core.ts.** The parsing precedent lives there, but the
  consumer set is CI-wide (renovate.yml AND ci.yml, potentially moon/publish),
  not Renovate-specific; `tools/toolchain/` is where cross-workflow toolchain
  materialization already lives (gate-tools.nix, parity). A Renovate-named home
  would misfile it.
- **(e) Single-mode tool (`flakeref` only); renovate.yml keeps a 2-line `nix
  build --print-out-paths` + shim inline.** Tempting because it makes `index.ts`
  fully pure (no `nix` subprocess in the thin, harder-to-unit-test shell) and
  `bin-dir` has just one caller today. Rejected: it re-splits the
  build+shim logic back out into yaml, and RD-2's ruled scope adds more
  PATH consumers (T2a), at which point the inline glue would itself need
  deduplicating. Keeping `bin-dir` in the tool localizes the shim-invariant
  (RD-3) in one tested place rather than replicating it per PATH caller.
  (Had RD-2 ruled the extra pins *out* of scope, flakeref-only would have been
  the leaner shape with renovate.yml the sole PATH consumer — but it did not.)
- **(f) A repo-wide "no literal devenv rev" gate** (fail if any tracked file
  outside a `devenv.lock` matches `github:.*/devenv/[0-9a-f]{40}`). Not an
  alternative to the tool — a *complement*, folded in as task T2b. The tool
  kills today's five instances; the gate kills the *class* (any future
  `nix run github:RigelBuild/devenv/<40hex>` in a new file re-opens it — pins 4
  and 5 are existence proof it recurs). Compass already has the gate idiom
  (design-ledger-gate greps PR-touched files; no-bash-gate is repo-wide), so
  this is a small, in-pattern addition.

## Plan

Dependency order: T1 → {T2, T3} → T5; T2a and T2b follow T1 (own PRs, after
T2/T3 land); T4 (root→fork, ruled RD-1) is independent of T1-T3; T6 is a
same-PR obligation on whichever PR lands; T7 (lockFileMaintenance, RD-1's
Renovate clause) is a separate follow-up issue, independent of the rest. Both
forks are ruled (see Resolved decisions) — no task remains gated on a decision.

### T1 — Build `tools/toolchain/devenv-cli/` (bun tool + tests)

New moon project, parity-gate shape (index.ts thin shell / core.ts pure /
core.test.ts), dependency-free package.

- Interfaces:
  - Consumes: a devenv.lock path given as `--lock <path>` (JSON with
    `.nodes.devenv.locked.{owner,repo,rev,type}`); `--mode flakeref|bin-dir`.
  - Produces (stdout, one line): `flakeref` → `github:<owner>/<repo>/<rev>#devenv`;
    `bin-dir` → path to a temp dir holding a single `devenv` symlink (after
    `nix build --no-link --print-out-paths` + shim creation — see RD-3 for why
    the shim, not the raw `<out>/bin`). Exit 1 + stderr diagnostic on missing
    node / invalid rev / non-github type / failed build.
  - Exports (core.ts): `devenvSource(lockText: string): DevenvSource`;
    `flakeref(src: DevenvSource): string`;
    `parseArgs(argv: readonly string[]): Request`;
    types `DevenvSource`, `Mode`, `Request` (signatures in §Approach). These are
    the same symbols `agent-image-env-gate` will `import` directly under T2a.
  - moon.yml: `typecheck` (`bunx tsc --noEmit`), `test` (`bun test`), `ci`
    (deps on both) — same task set as tools/toolchain/moon.yml:17-30 minus the
    parity task.
  - Nix cache config is inherited, not the tool's concern: the workflows write
    machine-level nix.conf at install (renovate.yml:50-62 `extra_nix_config`;
    ci.yml:1025-1034), so the `nix build` the tool spawns picks up the
    substituters/keys with no tool-side plumbing.
  - Tests: both lock fixtures (a cachix-shaped node and a RigelBuild-shaped
    node — the tool must be source-agnostic), missing-node throw, short-rev
    throw, non-github-type throw, argv parsing (both modes, unknown flag
    throws); a `bin-dir` test asserting the printed dir contains exactly one
    entry named `devenv` (the shim invariant, RD-3); and a static
    import-hygiene test asserting index.ts/core.ts import only `node:`/`bun:`
    specifiers + relative `./core` (turns the dependency-free convention,
    §Global Constraints, into a checked property rather than a comment).

### T2 — Rewire renovate.yml onto the tool

Replace the "Put the devenv CLI on PATH" run body (renovate.yml:92-103) with
the two-line call in §Approach; keep the step name, the WHY comment (updated to
name the tool), and the "Assert devenv is on PATH" step (renovate.yml:105-109)
unchanged.

- Interfaces:
  - Consumes: `bun tools/toolchain/devenv-cli/index.ts --lock devenv.lock
    --mode bin-dir` (bun on PATH from the toolchain bootstrap step,
    renovate.yml:64-80).
  - Produces: the devenv bin dir appended to `$GITHUB_PATH`; behavior contract
    unchanged (the assert step still passes, `devenv update nixpkgs` in
    refresh-devenv-nixpkgs.ts still resolves the lock-named CLI).

### T3 — Rewire ci.yml agent-image seed onto the tool

Replace the hand-pinned `nix run github:RigelBuild/devenv/15a81f3e…#devenv`
at ci.yml:1153 with the resolve-then-run pair in §Approach; update the step's
comment block (ci.yml:1120-1122) to describe lock-resolution.

- Interfaces:
  - Consumes: `bun tools/toolchain/devenv-cli/index.ts --lock
    agent-image/devenv.lock --mode flakeref` (bun on PATH, ci.yml:1032-1042).
  - Produces: the same `nix run "$src" -- container copy agent -r
    "docker-archive:/tmp/compass-agent-seed.tar:"` from `agent-image/` cwd —
    docker-archive output, `podman load` consumption (ci.yml:1154) unchanged.

### T2a — Extend to the four remaining hand-pins (ruled RD-2)

Convert all four (own PR, after T2/T3):

- `agent-image/moon.yml:45` and `devenv.nix:552` — command strings that cannot
  compose a flakeref inline; each becomes a small wrapper invocation (a
  `script:`/wrapper entry point that runs `bun … devenv-cli … --mode flakeref`
  then `nix run "$src"`; exact mechanism at impl).
- `agent-image/publish.sh:62` (the executable `BUILD_OUT="$(nix run …)"`; the
  `log "…"` echo at :61 is scrubbed in the same edit) — publish-agent-image.yml
  must first grow the toolchain bootstrap renovate.yml/ci.yml carry
  (publish-agent-image.yml:98-148 currently bootstraps only nix + skopeo) so
  that bun and the tool are on PATH.
- `tools/agent-image-env-gate/index.ts:103` — the cleanest: it is already a bun
  tool, so it drops the literal `nix run …#devenv` for
  `` `nix run ${flakeref(devenvSource(await Bun.file("agent-image/devenv.lock").text()))} …` `` —
  a direct `import` from `core.ts`, no CLI hop.

- Interfaces:
  - Consumes: `--lock agent-image/devenv.lock --mode flakeref` (shell sites) or
    the `core.ts` exports directly (env-gate).
  - Produces: `agent-image/moon.yml:45`, `agent-image/publish.sh:61` (log) and
    `:62` (executable), `devenv.nix:552`,
    `tools/agent-image-env-gate/index.ts:103` all free of literal revs.

### T2b — (from Alternative (f)) Repo-wide literal-devenv-rev gate

Add a gate (bun tool, no-bash-gate-compliant) that fails if a tracked file
carries a literal `github:[^/]+/devenv/[0-9a-f]{40}`, killing the drift *class*
so a future `nix run github:RigelBuild/devenv/<40hex>` in a new file can't
silently re-open it. Model on the existing gate idiom (design-ledger-gate greps
PR-touched files; no-bash-gate is repo-wide).

**The regex matches more than the five executable pins, so the gate needs a
carve-out from day one — not "if ever needed":** three non-executable full-rev
sites survive T1-T2a and would red a naive gate — `agent-image/moon.yml:5` (a
comment quoting the command), `agent-image/publish.sh:61` (a `log "…"` string,
unless T2a scrubs it — which it does, per T2a above), and **this design record
itself** (the Problem inventory quotes the full rev to name the hazard). So T2b
is specified as EITHER (1) a context-scoped regex — match only executable bodies
(workflow `run:` / moon `command:` / `.nix`/`.ts`/`.sh` exec lines), excluding
`docs/designs/**` and comment/log lines — OR (2) the blunt regex plus an
explicit allowlist that carves out `docs/designs/**` (for records that must name
a rev) and any surviving comment/log site. Either way the gate must be green on
the PR that introduces it; "lands after T2a" is necessary but not sufficient on
its own.

- Interfaces:
  - Consumes: the tracked file set (git ls-files or the moon/CI file list).
  - Produces: a CI gate task (own moon project) that reds on any literal
    devenv-rev pin in an executable context outside lockfiles.

### T4 — Move root devenv source onto the fork (ruled RD-1)

Add to root `devenv.yaml` an explicit `devenv:` input
(`url: github:RigelBuild/devenv`, `inputs.nixpkgs.follows: nixpkgs` — the same
shape as agent-image/devenv.yaml:45-49) with a WHY comment naming the
superset/byte-identical property; relock (`devenv update devenv` or full
relock) so root `devenv.lock`'s devenv node names the fork.

- Interfaces:
  - Consumes: root `devenv.yaml` (inputs block, devenv.yaml:9-42), network
    relock.
  - Produces: root `devenv.lock` `.nodes.devenv.locked` →
    `owner=RigelBuild repo=devenv rev=<relocked>`; renovate.yml picks it up
    with zero edits (its step already tracks the lock — that was the point).
  - NOT produced: lock *reconciliation* — root and agent-image keep separate
    locks/revs (ruled RD-1: unify without reconciliation). T4 moves root onto
    the fork; it does not collapse the two locks.

### T5 — Smoke

- Renovate path: run `bun tools/renovate/refresh-devenv-nixpkgs.ts` semantics
  via the workflow's own steps on a branch (or minimally: locally `bun
  tools/toolchain/devenv-cli/index.ts --lock devenv.lock --mode bin-dir` and
  assert `<out>/bin/devenv --version` executes), proving the postUpgradeTask
  relock still finds the lock-named CLI.
- Image path: `moon run agent-image:build` (the same fork-pinned derivation the
  seed step realizes, agent-image/moon.yml:34-47) plus, on the PR, the
  dogfood-e2e seed step exercising the rewritten ci.yml:1153.
- Tool gates: `moon run devenv-cli:ci` (typecheck + bun test) green.

### T6 — Ledger delta (same-PR obligation)

Whichever PR lands this record (and later the impl) must append the DL rows to
`docs/designs/DECISIONS.md` or declare `Ledger-impact:` in the PR body
(tools/design-ledger-gate/index.ts:21, :558-559). Handled by the driver, noted
here so no PR in this lane trips the gate.

### T7 — (RD-1 Renovate clause) Keep both devenv locks Renovate-current

Separate follow-up issue (not this record's impl PRs). Matt ruled both locks
should stay current through Renovate bumps, but compass's `devenv` input is not
Renovate-tracked today: no `custom.regex` manager points at it and the native
`nix` manager can't read `devenv.lock` (devenv's own lock format, no root
`flake.nix` to anchor on — a documented Renovate limitation for devenv locks).
So "stay current" = enable
Renovate `lockFileMaintenance` (runs `devenv update` on a schedule, opens the
PR), which sweeps both the root and agent-image fork revs forward
automatically.

- Interfaces:
  - Consumes: compass's Renovate config (the `renovate.yml`/config the daily
    run reads).
  - Produces: a scheduled `lockFileMaintenance` config that relocks
    `devenv.lock` + `agent-image/devenv.lock` (and other un-tracked lock
    inputs). Filed as its own RIG issue; gated on nothing in this record but
    best landed after T4 so root is already on the fork it will bump.

## Tasks

- [ ] T1 — `tools/toolchain/devenv-cli/` bun tool: core.ts + index.ts +
      core.test.ts + moon.yml + package.json + tsconfig.json
- [ ] T2 — renovate.yml "Put the devenv CLI on PATH" → tool call
      (`--lock devenv.lock --mode bin-dir`)
- [ ] T3 — ci.yml:1153 seed step → resolve via tool
      (`--lock agent-image/devenv.lock --mode flakeref`) + `nix run "$src"`
- [ ] T2a — (ruled RD-2) de-pin the four remaining sites via the tool:
      agent-image/moon.yml:45, publish.sh:61 (log) + :62 (exec), devenv.nix:552,
      tools/agent-image-env-gate/index.ts:103
- [ ] T2b — repo-wide literal-devenv-rev gate with a day-one carve-out for
      docs/designs/** + comment/log sites (kills the class; lands after T2a)
- [ ] T4 — (ruled RD-1) root devenv.yaml → `github:RigelBuild/devenv` + relock
- [ ] T5 — smoke: devenv-cli:ci green; renovate relock path; `moon run
      agent-image:build`; dogfood-e2e seed on the PR
- [ ] T6 — DECISIONS.md DL rows (or `Ledger-impact:`) on each landing PR
- [ ] T7 — (RD-1 Renovate clause) enable lockFileMaintenance so both devenv
      locks stay current; filed as its own RIG issue

## Resolved decisions

Both forks were ruled by Matt (2026-08-25, direct `ask`). Recorded here as the
frozen outcomes; the DL rows land in the same PR (T6).

### RD-1 (was OQ-1) — Unify the devenv source: YES (Option A), both locks kept Renovate-current

**Ruling: unify root onto `github:RigelBuild/devenv`, WITHOUT lock
reconciliation — AND keep both locks updated through Renovate bumps** (Matt:
"A but both should stay updated thru renovate bumps"). Root and agent-image both
track the fork at independently-pinned revs; the DRY'd tool reads each lock by
name so nothing in code cares.

- **Why A holds:** the fork keeps upstream's values as defaults
  (agent-image/devenv.yaml:43-44), so the root shell is byte-identical at the
  current rev; unifying matches fleet posture (agent-image and the fleet's other
  container-building repos already on the fork) and collapses two source stories
  into one.
- **Accepted cost, now mitigated by the Renovate clause:** "byte-identical" is
  asserted-at-the-current-rev, not a standing guarantee — the fork tracks its
  own `main` and no gate re-checks that a future fork commit leaves the root
  shell's evaluation unchanged. Keeping the lock *current* (rather than frozen
  and silently stale) is exactly what surfaces fork drift as a reviewable PR on
  a cadence instead of a surprise on the next manual relock.
- **The Renovate clause → a follow-up, because compass's fork input is NOT
  Renovate-tracked today.** Compass has no `custom.regex` manager on the
  `devenv` input and the native `nix` manager can't read `devenv.lock` (it is
  devenv's own lock format, not a `flake.lock`, with no root `flake.nix` to
  anchor on — a documented Renovate limitation for devenv locks). So "stay
  updated thru renovate bumps" means enabling
  `lockFileMaintenance` (Renovate runs `devenv update` on a schedule and opens
  the PR) — captured as task **T7** and a filed follow-up issue, not a
  same-PR change. Until T7 lands, both locks move on manual `devenv update`.
- **No lock reconciliation:** root and agent-image keep separate locks/revs —
  they pin genuinely different scopes (dev shell vs container image) on
  independent cadences; reconciling would couple an image-motivated fork bump to
  a dev-shell relock. (T4 moves root onto the fork; it does NOT collapse the
  locks.)

### RD-2 (was OQ-2a) — Consumer scope: FULL drift-class kill (all five pins + the T2b gate)

**Ruling: convert all five executable hand-pins AND add the repo-wide
literal-rev gate** (Matt: "Full kill: all 5 pins + a repo-wide literal-rev
gate"). The repo carries five executable hand-pins of the fork rev `15a81f3e…`
(repo-wide grep this session; the `agent-image/devenv.lock:73` node is the
source-of-truth lock, not a hand-pin):

1. `ci.yml:1153` — dogfood-e2e seed (task T3).
2. `agent-image/moon.yml:45` — `build.command` (task T2a).
3. `agent-image/publish.sh:62` — publish build (task T2a).
4. `devenv.nix:552` — the `dogfood:agent-image` task, whose own comment
   (`devenv.nix:534-536`) claims the pin "cannot diverge from the fork source
   the agent-image module set is pinned to" — a claim this record disproves.
5. `tools/agent-image-env-gate/index.ts:103` — the env gate's `nix run …
   container build`, which runs in the CI moon graph
   (`tools/agent-image-env-gate/moon.yml:45`, `deps: ['install',
   'compass-agent-image:build']`). Highest-severity: after a lock update it
   would validate an image built at a *different* devenv rev than the one
   `moon.yml:45` builds — a gate greening the wrong artifact class. It fits the
   tool most cleanly (already a bun tool → `import { devenvSource, flakeref }`
   from `core.ts` directly, no CLI hop).

T1-T3 convert renovate + ci; T2a converts pins 2-5 (own PR after T1-T3); T2b
adds the standing gate so the class stays dead.

### RD-3 (was OQ-2b) — `bin-dir` owns the single-binary shim

The first draft proposed dropping renovate.yml's `$RUNNER_TEMP/devenv-shim/bin`
symlink (renovate.yml:100-102) and appending the store `<out>/bin` to
`$GITHUB_PATH` directly. **Rejected on review:** the existing code links *one
named binary* (`ln -s "$out/bin/devenv" "$shimdir/devenv"`), not the directory,
and `$out` was already separable when it was written — so single-binary
exposure was a deliberate choice. Appending the whole `<out>/bin` would put
every binary devenv's closure exposes on PATH, and since `$GITHUB_PATH` entries
are prepended in later steps, a future fork rev adding a wrapped helper there
could shadow the parity-pinned toolchain binaries (gate-tools.nix langs) added
earlier — the drift class parity exists to reject, which `command -v devenv`
(renovate.yml:105-109) would not catch. **Decision:** `mode=bin-dir` owns the
shim — creates a temp dir with a single `devenv` symlink and prints that dir,
keeping the call site at two lines while preserving the single-binary exposure
invariant (§Approach reflects this).
