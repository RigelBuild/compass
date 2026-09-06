# Renovate-manage the Go analysis battery pins (RIG-3306)

Status: Draft

## Problem / Intent

`tools/toolchain/versions/go-analysis.nix` carries two hand-maintained source
pins — nilaway (untagged git rev on `uber-go/nilaway` main) and golangci-lint
(release tag) — that no automation watches. The file says so itself
(go-analysis.nix:27-29):

> `# MANUALLY MAINTAINED — these pins are NOT yet Renovate-managed (the other`
> `# versions/*.nix files each have a customManager in tools/renovate/config.json5;`
> `# this file has none — tracked as a follow-up).`

That follow-up is this design. **This is ROT-prevention, not skew-coupling**:
compass #904 (RIG-3303, merged in ab56c273) already closed Go compiler skew
structurally — `tools/toolchain/go-analysis.nix:23` rebuilds the whole battery
with the go-overlay toolchain (`buildGoModule' = pkgs.buildGoModule.override {
go = goToolchain; };`), so an analyzer can never again be a go1.26 build parsing
go1.27 code. The residual risk is pure pin staleness: the nix build fails LOUD
on a wrong `hash`/`vendorHash` but never on a stale-but-consistent pin
(go-analysis.nix pin-file header, lines 30-33), so nothing ever tells anyone
nilaway's rev is months behind main. Deliverable: surface both pins to
Renovate so a bump PR opens when upstream moves, and refresh the coupled Nix
hashes in the same branch so the PR lands green.

govulncheck and go-licenses carry no pin (rebuild-only, pin-file lines 23-26)
— out of scope. This design does NOT re-couple the analyzers to the go
toolchain pin (see Global Constraints, "Solo-branch per pin").

## Approach

### What a bump must move (the coupling)

The builder (`tools/toolchain/go-analysis.nix:48-52`) consumes each pin as:

```nix
base.overrideAttrs (_old: {
  inherit (pin) version vendorHash;
  src = pkgs.fetchFromGitHub ({
    inherit (pin) owner repo hash;
  } // (if pin ? tag then { inherit (pin) tag; } else { inherit (pin) rev; }));
});
```

So one bump must move, consistently: `version` + (`tag` for golangci-lint OR
`rev` for nilaway) + `hash` (fetchFromGitHub source FOD) + `vendorHash` (Go
module vendor set). Renovate's regex manager rewrites exactly ONE captured
token per manager (`currentValue` or `currentDigest`); everything else is the
postUpgradeTask refresher's job.

### Piece 1 — two customManagers in `tools/renovate/config.json5`

Both managers target `/^tools/toolchain/versions/go-analysis\.nix$/` (two
managers over one file is the established devenv.lock precedent —
config.json5:172 and :219 both target `/^devenv\.lock$/`). Because the file
contains two `version = "…"` lines, each matchString is anchored to its tool's
attribute opener (the adjacency-anchor idiom the devenv managers use at
config.json5:174 and :221).

**golangci-lint** — mirrors the moon manager (config.json5:287-293:
`datasourceTemplate: "github-releases"`, `extractVersionTemplate:
"^v(?<version>.+)$"`, `depTypeTemplate: "toolchain"`):

```json5
{
  customType: "regex",
  managerFilePatterns: ["/^tools/toolchain/versions/go-analysis\\.nix$/"],
  matchStrings: ["golangci-lint = \\{\\s*version = \"(?<currentValue>[^\"]+)\""],
  depNameTemplate: "golangci/golangci-lint",
  datasourceTemplate: "github-releases",
  extractVersionTemplate: "^v(?<version>.+)$",
  depTypeTemplate: "go-analysis",
}
```

The manager owns the `version = "2.13.2"` line; the refresher derives and
rewrites the sibling `tag = "v${version}"` line (go-analysis.nix:47) — see
Piece 3 and OQ-1 for why version-line (not tag-line) matching is recommended.

`depTypeTemplate` is `"go-analysis"`, deliberately NOT `"toolchain"`: the
toolchain age-gate exemption (config.json5:419-420, `matchDepTypes:
["toolchain"], minimumReleaseAge: null`) would otherwise null the 5-day
cooldown for these pins, and Global Constraints says they inherit it. Nothing
else keys on the `toolchain` depType that these pins need (solo-branching
comes from `matchFileNames`, not depType — see Piece 2).

**nilaway** — mirrors the devenv git-refs managers (config.json5:218-226),
with the distinct-depName idiom (config.json5:235-238) already used by
`RigelBuild/devenv-agent-image` and `postgres-stack` to keep a rule
independently governed:

```json5
{
  customType: "regex",
  managerFilePatterns: ["/^tools/toolchain/versions/go-analysis\\.nix$/"],
  matchStrings: ["rev = \"(?<currentDigest>[a-f0-9]{40})\""],
  depNameTemplate: "uber-go/nilaway",
  packageNameTemplate: "https://github.com/uber-go/nilaway",
  currentValueTemplate: "main",
  datasourceTemplate: "git-refs",
  depTypeTemplate: "go-analysis",
}
```

The `rev = "…"` anchor needs no tool-scoping: nilaway is the only pin with a
`rev` attribute (golangci-lint pins `tag`, go-analysis.nix:39 vs :47), and
config.test.ts pins the extraction against the real file so a future second
`rev` fails the guard.

### Piece 2 — solo-branching: already delivered, verify only

The existing un-group rule (config.json5:405-406):

```json5
matchFileNames: ["tools/toolchain/versions/*.nix"],
groupName: null,
```

already matches `go-analysis.nix` and nulls the groupName for anything a
manager surfaces from it, and it sits AFTER the "TypeScript dependencies"
rollup in `packageRules` (later rules win), so each pin resolves to its own
solo branch. nilaway's update type is `digest` and never enters the rollup
anyway (the rollup admits only patch/minor). **No new grouping config.**
config.test.ts already proves this resolution shape for the go pin and the
generic versions/*.nix case (config.test.ts:858-931, "a versions/*.nix
toolchain pin minor bump un-groups (null), not the TS rollup"); this design
adds the same resolution assertions for the two new depNames.

Solo-branching is also what makes branch-mode postUpgradeTasks safe here: a
dep that never shares a branch owns the single branch-mode task slot Renovate
builds per branch (the argument config.json5:576-585 makes for the
devenv-nixpkgs rule).

### Piece 3 — hash refresh: a new dedicated `refresh-go-analysis-hashes.ts` (option b)

**The fork.** The pins carry BOTH a fetchFromGitHub source `hash` (a FOD over
the unpacked tree) AND a `vendorHash` (Go module vendor set). Neither existing
refresher handles a file carrying both:

- `refresh-toolchain-hashes.ts` recomputes `fetchurl` hashes via `nix store
  prefetch-file` (refresh-toolchain-hashes.ts:13-14, :92-93). A
  fetchFromGitHub `hash` is NOT a flat-file hash — it content-addresses the
  unpacked source tree — so `prefetch-file` cannot produce it, and the script
  has no concept of a vendorHash at all.
- `refresh-fod-hashes.ts` recomputes vendorHash/outputHash by faking the pin,
  realising ONE fixed build vehicle, and parsing the `got:` SRI
  (refresh-fod-hashes.ts:72-73 `BUILD_FILE = "guest-image/default.nix"`,
  :211 `nix build -f ${BUILD_FILE} ${BUILD_TARGET}`). Its `FodEntry` table
  (:104-111) models exactly one hash per entry, one marker, one global build
  vehicle — and the go-analysis derivations are NOT in the guest-rootfs
  closure it builds, so the vehicle can never surface their mismatches.

**Decision: (b)** — a new `tools/renovate/refresh-go-analysis-hashes.ts`
scoped to `go-analysis.nix`, reusing the proven fake→realise→parse-`got:`
mechanism (and mirroring `refresh-fod-hashes.ts`'s `rewriteInlineHash` /
`parseGotForFragment` shapes), wired into the TOP-LEVEL branch-mode
postUpgradeTasks alongside the two existing self-gating refreshers. Rationale
against the alternatives is below; the mechanism:

1. **Self-gate** (mirrors refresh-fod-hashes.ts main(), :233-269): no-op with
   exit 0 unless `tools/toolchain/versions/go-analysis.nix` differs from the
   base branch. Determine WHICH tool block changed (diff the nilaway vs
   golangci-lint attrset text vs base) and act on that tool only.
2. **Derived-field rewrite, before any hash work:**
   - golangci-lint: rewrite `tag = "v${version}"` from the (already
     Renovate-bumped) `version` line — the two lines are couples by
     construction (go-analysis.nix:44,47).
   - nilaway: rewrite `version = "0-unstable-<YYYY-MM-DD>"` from the new
     rev's commit date via a TOKEN-FREE shallow fetch (`git init` + `git
     fetch --depth=1 https://github.com/uber-go/nilaway <rev>` + `git log -1
     --format=%cs FETCH_HEAD` in a temp dir). NOT a REST call: the
     postUpgradeTask child env is allowlist-filtered and carries no token,
     while git transport IS authenticated by Renovate — see OQ-3 for the
     mechanism. Fail loud (exit 1) on non-zero exit or a non-`YYYY-MM-DD`
     result.
3. **Two-pass realise per changed tool**, building that tool's derivation via
   the small vehicle from Piece 4:
   - fake BOTH `hash` and `vendorHash` in the CHANGED tool's block only. The
     imported `rewriteInlineHash` rewrites the first line matching its marker
     in whatever text it is handed (single-line `includes()`,
     refresh-fod-hashes.ts:150-162), and go-analysis.nix has TWO `hash =` and
     TWO `vendorHash =` lines — so the refresher MUST slice the tool's
     attrset block, rewrite the SLICE, and splice it back, never handing the
     whole file to `rewriteInlineHash` (see T3 contract; this is the one seam
     where a wrong implementation silently corrupts the sibling pin);
   - `nix build -f tools/toolchain/gate-tools.nix analysis.<tool> --no-link
     --keep-going` → fails at the source FOD first → parse `got:` for the
     tool-scoped `source` drv fragment → write the real `hash`;
   - build again → fails at the `<name>-go-modules` vendor FOD → parse
     `got:` for the tool-scoped `go-modules` fragment → write the real
     `vendorHash`.
   **Fragment attribution must be tool-scoped, not the generic word
   `source`.** `parseGotForFragment` matches `line.includes(fragment)`
   against the whole mismatch header (which carries the full store path), and
   `--keep-going` can surface a SECOND mismatch block from any co-failing FOD
   in the closure — so a bare `source` fragment could bind a foreign
   `…-source.drv`. T1 records the two literal drv-name fragments per tool
   (`nix derivation show`), scoped by pname (e.g. `nilaway-…-source`,
   `…-go-modules`), and `refresh-go-analysis-hashes.ts` carries the same
   load-time disjointness assertion refresh-fod-hashes.ts:118-128 does,
   extended across all four fragments (2 tools × src+vendor) so no fragment
   is a substring of another. The source-fails-first ordering is guaranteed
   by nix semantics, not luck: the go-modules vendor FOD takes the
   fetchFromGitHub `source` drv as an input and nix cannot attempt a
   derivation before its inputs realise, so with both faked the source
   mismatch is the only line pass 1 can surface. `--keep-going` is kept
   despite that making it inert on the expected path — it is defensive (costs
   nothing, preserves the sibling script's idiom) so a surprise co-failing
   FOD surfaces its `got:` rather than masking this one; it is precisely
   because `--keep-going` CAN surface a second block that the fragment must
   be tool-scoped (above). Trusted-by-argument, not tested: the stub-nix test
   emits the two-invocation sequence, so it exercises the parse/splice, not
   the nix ordering. (One inherited theoretical hole from the sibling script:
   if a store path with the all-`A` FAKE_SRI output hash already existed, a
   faked source would "succeed" with wrong content — vanishingly unlikely,
   worth a code comment, not a mechanism change.)
4. **Fail loud** (exit 1) on any missing marker, missing `got:`, or
   commit-date fetch failure; restore the original file text on the failure
   path (the `recompute()` restore-on-every-path shape,
   refresh-fod-hashes.ts:203-226). Idempotent: re-running rewrites the same
   values. Wrap the imported helpers' errors (both prefixed `renovate-fod:`)
   under a `renovate-go-analysis:` prefix so a failure greps to this script,
   not the guest-image one.

### Piece 4 — build vehicle: expose the analysis derivations from gate-tools.nix

`tools/toolchain/gate-tools.nix` already assembles exactly the right context —
the devenv.lock-pinned nixpkgs + go-overlay + `goPin`, imported once and passed
to `go-analysis.nix` (gate-tools.nix:100-109 lists
`golangci-lint = identityOf goAnalysis.golangci-lint; … nilaway = identityOf
goAnalysis.nilaway;`) — but its `langs` output holds identity ATTRSETS
(`{version, store, bins}`), not derivations, so `nix build` cannot target them.
Add one output beside `langs`:

```nix
analysis = goAnalysis;  # the raw derivations, for the hash refresher
```

so the refresher builds `nix build -f tools/toolchain/gate-tools.nix
analysis.nilaway` with zero duplicated nixpkgs/overlay plumbing. `attrs` has a
default (`{ attrs ? [ ] }:`, gate-tools.nix:34), so `-f` works without
`--arg` — the same property the `langs` verdict relies on.

### Piece 5 — wiring, allowlist, cooldown rule

- **Top-level postUpgradeTasks** (config.json5:909-925): append the command
  `"bun tools/renovate/refresh-go-analysis-hashes.ts"` and add
  `"tools/toolchain/versions/go-analysis.nix"` to `fileFilters` (an INCLUDE
  allowlist — omit it and Renovate runs the refresh but silently DROPS the
  rewrite from the commit, the failure mode config.json5:648-655 documents).
  Top-level (not rule-level) wiring is the established shape for self-gating
  refreshers (refresh-toolchain-hashes + refresh-fod-hashes both ride it,
  config.json5:884-908) and both existing commands self-gate to no-ops on a
  go-analysis branch; no rule with rule-level postUpgradeTasks matches these
  two depNames, so the top-level task is never evicted on their branches.
- **bot-config.json5**: add
  `"^bun tools/renovate/refresh-go-analysis-hashes\\.ts$"` to
  `allowedCommands` (a repo config cannot self-authorize a command;
  config.test.ts pins the config↔allowlist pair together,
  config.test.ts:188-204).
- **nilaway cooldown rule**: one packageRule `matchManagers:
  ["custom.regex"], matchDepNames: ["uber-go/nilaway"], minimumReleaseAge:
  null` — see OQ-2. The repo-wide `minimumReleaseAge: "5 days"` +
  `internalChecksFilter: "strict"` (config.json5:57-58) measures age from a
  release timestamp; a git-refs digest to a moving branch HEAD carries none,
  so strict cooldown marks it permanently `pending` and ZERO PRs are ever cut
  — the exact silent-rot outcome this design exists to prevent. All three
  existing git-refs rules null the cooldown for precisely this reason, calling
  it "mechanically INAPPLICABLE to a git-refs digest" (config.json5:670-681).
  golangci-lint keeps the 5-day cooldown untouched (github-releases carries
  release timestamps; the distinct `go-analysis` depType keeps it clear of
  the toolchain exemption).

## Alternatives considered

### Hash refresh (the central fork)

**(a) Extend `refresh-fod-hashes.ts`** — generalise `FodEntry` to carry a
per-entry build vehicle, MULTIPLE hashes per entry (src `hash` + `vendorHash`),
tool-scoped markers, and a multi-pass realise loop. Rejected: every one of
those is a structural change to a shipped, tested script whose current table
shape (one hash, one marker, one global `BUILD_FILE`/`BUILD_TARGET` constant —
refresh-fod-hashes.ts:72-73, :104-111) is load-bearing for its test
(refresh-fod-hashes.test.ts derives its fixtures from the exported table). The
guest-image FOD path is on the critical line for every gomod/catalog bump;
entangling it with a second, differently-shaped concern risks the green paths
to buy nothing — the two scripts would share only ~40 lines of parse/rewrite
helpers, which (b) imports directly from `refresh-fod-hashes.ts`'s exported
`rewriteInlineHash`/`parseGotForFragment` instead of duplicating.

**(b) New dedicated `refresh-go-analysis-hashes.ts`** — CHOSEN. One script per
coupling class is the existing decomposition: toolchain URL hashes
(refresh-toolchain-hashes), image FOD hashes (refresh-fod-hashes), devenv
locks (refresh-devenv-lock), go-overlay lockstep (refresh-go-overlay). Each
self-gates and rides a task slot; each has its own red-green test. A
go-analysis refresher is a fifth coupling class (two hashes + two derived
fields in one pin file), not a variant of an existing one.

**(c) Split: prefetch the src `hash` + realise for the `vendorHash`** —
Rejected. The src hash is NOT `prefetch-file`-able (fetchFromGitHub hashes the
unpacked tree, not the tarball bytes); it would need a second mechanism
(`nix flake prefetch github:…` narHash, or nix-prefetch-github). But the
realise mechanism already surfaces a wrong src hash as a `got:` on the FIRST
build pass at near-zero cost (the source FOD fails before any Go compilation
starts), so the split buys one cheap build iteration at the price of a whole
second mechanism, a second failure mode, and a hash-kind asymmetry in one
script. Uniform fake→realise→parse for both hash kinds is strictly simpler.

### Manager shape for golangci-lint (see OQ-1)

**Match the `tag = "v2.13.2"` line instead of `version`** — workable
(github-releases returns v-prefixed tags, so `currentValue` = `"v2.13.2"`
with no extractVersion), and it would make Renovate own the line the builder
actually consumes. Rejected in favour of version-line matching: (i) it breaks
the uniform idiom all four existing versions/*.nix managers use
(`matchStrings: ["version = \"(?<currentValue>[^\"]+)\""]` + extractVersion,
config.json5:263-293), which config.test.ts guards collectively; (ii) either
choice leaves exactly one sibling line refresher-owned (`tag` from `version`
or `version` from `tag` — both trivially derivable), so the tie-break is
idiom-consistency; (iii) a stale `version` is the worse residue (it feeds
`inherit (pin) version` → the drv name and any versionCheckHook —
go-analysis.nix builder comment, lines 31-35), so the field the REFRESHER
must never miss is `tag`, whose derivation (`"v" + version`) is the simpler
of the two.

### Grouping with the go toolchain pin

Rejected (and the brief's framing confirms): grouping would only defend a
"new-go-breaks-old-analyzer-source" case, which fails LOUD (red build on the
go bump PR) and which Renovate could not auto-resolve a compatible rev for
anyway. It buys nothing but a larger blast radius per bump. The un-group rule
keeps every versions/*.nix pin solo.

## Global Constraints

- **Solo-branch per pin — NO grouping with the go toolchain pin.** The
  `versions/*.nix` un-group rule (config.json5:405-406) already enforces this;
  do not add a group or groupName. Grouping with go would only defend a
  new-go-breaks-old-analyzer-source case, which fails loud (red build) and
  which Renovate cannot auto-resolve a compatible rev for.
- **5-day cooldown inherited for golangci-lint** — repo-wide
  `minimumReleaseAge: "5 days"` + `internalChecksFilter: "strict"`
  (config.json5:57-58) applies; do NOT stamp `depTypeTemplate: "toolchain"`
  (its exemption at config.json5:419-420 would null the cooldown). The
  nilaway cooldown-null is not an exemption — the cooldown is mechanically
  inapplicable to a git-refs digest (no release timestamp; see OQ-2).
- **Fail-loud, never fail-open** — the refresher exits 1 on any hash it
  cannot compute (missing marker, missing `got:`, failed commit-date fetch),
  mirroring both existing refreshers. A silent no-op ships the stale pin the
  task exists to fix. Restore original file text on every failure path.
  Observable surface of an exit 1 (verified against renovate@44.46.2): a
  throwing postUpgradeTask is caught and pushed onto `artifactErrors`
  (execute-post-upgrade-commands.js), then routed through a
  releaseTimestamp-conditioned gate (branch/index.js) three ways —
  (i) **nilaway** (git-refs digest, NO releaseTimestamp) FORCE-OPENS the PR
  with an artifact-error notice appended to the body (pr/index.js
  `forcePr`), bypassing the normal branch-status wait; (ii) **golangci-lint**
  (github-releases) when the release is <2h old AND no branch yet exists
  throws `MANAGER_LOCKFILE_ERROR` and opens NO PR that run — the silent-no-PR
  shape this design exists to kill — self-healing on the next daily run once
  the branch exists; (iii) otherwise the PR opens with the notice. So the
  primary compensating control is the artifact-error notice on the PR body
  PLUS the grep-able `renovate-go-analysis:`-prefixed log line — the nix
  build's red CI is a SECOND control that only exists once a PR is open, and
  on shape (ii) the log line is the ONLY surface for a whole day. T4 asserts
  the refresher's error text is greppable and carries that prefix.
- **No rule-level postUpgradeTasks may match the two go-analysis depNames** —
  Renovate builds ONE branch-mode task per branch and a rule-level
  `postUpgradeTasks` REPLACES the top-level one for matching branches
  (config.json5:701-702, :772-773). A future rule matching
  `golangci/golangci-lint` or `uber-go/nilaway` would evict the go-analysis
  refresher, shipping stale hashes (which under the fail-loud surface above
  may not even open a PR). True today (all four rule-level-task rules are
  scoped to other depNames); T2 pins it as a red test so a future violation
  fails loud.
- **Two managed pins only** — nilaway (`rev`) + golangci-lint (`tag`).
  govulncheck / go-licenses are rebuild-only (no pin attrset,
  go-analysis.nix pin-file lines 23-26): no manager, no refresher entry. The
  T1 `analysis` output still exposes all four derivations verbatim (it is the
  raw `goAnalysis` import — a filtered attrset would be a second place the
  two-vs-four split must be maintained); the extra two are buildable but
  unmanaged by design.
- **config.test.ts guard is mandatory** — both managers, the coupled task
  wiring, the branch-resolution shape, and the allowlist pairing all pinned.
- **bot-config.json5 allowlist** — the new command string added to
  `allowedCommands` with `^…$` anchors (bot-config.json5:128-135 shape);
  config.test.ts pins config.json5 commands ↔ allowlist 1:1.
- **Naming** — script: `tools/renovate/refresh-go-analysis-hashes.ts`; test:
  `tools/renovate/refresh-go-analysis-hashes.test.ts`; depNames:
  `golangci/golangci-lint`, `uber-go/nilaway`; depType: `go-analysis`.
- **Runner environment** — compass CI is GitHub Actions; Renovate runs on the
  daily GHA cron with `nix` (nix-command) + `bun` + `git` on PATH and network
  (the same envelope refresh-fod-hashes.ts documents in its header). No new
  runner requirement.
- **TypeScript, not bash** (`rule://scripts-ts-over-bash`); `===`/`!==` only
  (`ts-no-loose-equality`, `== null` carve-out); Biome-clean; bun:test.
- **Version floors** — none introduced. Pins move wherever upstream moves
  (golangci-lint majors open individual PRs per the default
  non-major-batching posture; nilaway is digest-only). Renovate stays at its
  existing self-pin; no renovate feature beyond what the four shipped
  customManagers + postUpgradeTasks already use.
- **No VCS actions in this record's PR** — the record ships as its own PR and
  freezes on merge; implementation is a separate PR.

## Plan

Four tasks, ordered by dependency. T1 (vehicle) and T2 (managers/config) are
independent; T3 (refresher) needs T1's attrpath; T4 (guards/wiring tests)
closes the loop. Each carries its own test cycle (red-green where a new
observable contract appears).

### T1 — Expose the analysis derivations as a buildable gate-tools output

Add `analysis = goAnalysis;` beside `langs` in
`tools/toolchain/gate-tools.nix` so `nix build -f tools/toolchain/gate-tools.nix
analysis.<tool>` realises one analysis derivation with the devenv.lock-pinned
nixpkgs + go-overlay context already assembled there (gate-tools.nix:34-66).
No behaviour change to `env`/`identity`/`langs`, and no NEW-sibling-output
breakage: every live gate-tools.nix consumer names its output explicitly
(`nix eval … langs` in ci.yml/eng-docs-deploy.yml/release.yml/renovate.yml,
`identity`+`langs` in parity.ts:78/:102, `langs` in release-notes:293), so a
new top-level `analysis` attr cannot perturb them (verified across all nine
call sites). `analysis` intentionally exposes all four derivations (verbatim
`goAnalysis`, not a filtered attrset — one place, not two, to maintain); only
the two pinned tools get a refresher entry (Global Constraints).

Verify: `nix build -f tools/toolchain/gate-tools.nix analysis.nilaway
--no-link` and `analysis.golangci-lint --no-link` succeed at the current pins
(optionally all four `analysis.*` — costs nothing beyond the `langs` verdict
and keeps the unmanaged pair non-rotting); `nix eval` of `langs` unchanged.
Record the two literal drv-name fragments per tool for T3: run `nix
derivation show -f tools/toolchain/gate-tools.nix analysis.nilaway` (and
`.golangci-lint`) and capture the pname-scoped `…-source` / `…-go-modules`
fragment strings.

Interfaces:

- `tools/toolchain/gate-tools.nix` — new top-level output `analysis`:
  attrset `{ golangci-lint, govulncheck, go-licenses, nilaway }` of
  derivations (the verbatim `goAnalysis` import result).
- Consumed by T3 as build target `-f tools/toolchain/gate-tools.nix
  analysis.${tool}`; the captured per-tool fragment strings feed T3's
  `srcFragment`/`vendorFragment`.

### T2 — The two customManagers + packageRule in config.json5

Add to `customManagers` (after the go manager, config.json5:317-323):

- golangci-lint manager: `customType: "regex"`, `managerFilePatterns:
  ["/^tools/toolchain/versions/go-analysis\\.nix$/"]`, `matchStrings:
  ["golangci-lint = \\{\\s*version = \"(?<currentValue>[^\"]+)\""]`,
  `depNameTemplate: "golangci/golangci-lint"`, `datasourceTemplate:
  "github-releases"`, `extractVersionTemplate: "^v(?<version>.+)$"`,
  `depTypeTemplate: "go-analysis"`.
- nilaway manager: `customType: "regex"`, same `managerFilePatterns`,
  `matchStrings: ["rev = \"(?<currentDigest>[a-f0-9]{40})\""]`,
  `depNameTemplate: "uber-go/nilaway"`, `packageNameTemplate:
  "https://github.com/uber-go/nilaway"`, `currentValueTemplate: "main"`,
  `datasourceTemplate: "git-refs"`, `depTypeTemplate: "go-analysis"`.
- packageRule: `matchManagers: ["custom.regex"], matchDepNames:
  ["uber-go/nilaway"], minimumReleaseAge: null` with the
  mechanically-inapplicable rationale comment (mirror config.json5:670-681).
- Top-level postUpgradeTasks: append command
  `"bun tools/renovate/refresh-go-analysis-hashes.ts"` and fileFilter
  `"tools/toolchain/versions/go-analysis.nix"` (config.json5:909-925).
- `tools/toolchain/versions/go-analysis.nix`: replace the "MANUALLY
  MAINTAINED … tracked as a follow-up" header paragraph (lines 27-33) with
  one documenting the Renovate management + refresher coupling.

Test cycle (RED first, in config.test.ts, mirroring the manager/rule guards
at config.test.ts:449-475 and :858-931):

- both managers extract against the REAL go-analysis.nix (regex replayed on
  the live file: golangci-lint yields `currentValue: "2.13.2"`, nilaway
  yields the 40-hex `currentDigest`); assert `matchAll` length is EXACTLY 1
  for each AND that the nilaway capture equals the `rev` inside the sliced
  `nilaway = { … }` block specifically (proves WHICH pin it bound, not merely
  that one matched — mirrors the devenv-nixpkgs extraction guard,
  config.test.ts:479-492), so a future second `rev` pin retargeting nilaway
  fails the guard;
- golangci-lint manager fields (datasource/depName/extractVersion/depType)
  and nilaway fields (datasource/depName/packageName/currentValue/depType);
- branch resolution: a golangci-lint minor bump and a nilaway digest bump
  each resolve `groupName: null` (solo branch), not the TS rollup;
- golangci-lint inherits `minimumReleaseAge: "5 days"` (NOT nulled by the
  toolchain depType exemption); nilaway resolves `minimumReleaseAge: null`.
  This requires generalizing the config.test.ts rule-replay resolver's
  ACCUMULATOR from `groupName` to an arbitrary key (`resolveGroupName` body,
  config.test.ts:134-186; the groupName write is :183) so it can replay
  `minimumReleaseAge` through the same last-match-wins pass — the
  `matchDepTypes` filter it needs is already implemented (config.test.ts:146-151);
  asserting the packageRule object merely exists would miss a mis-ordered
  rule and does not suffice;
- **eviction guard** — iterate every `cfg.packageRules` entry carrying
  `postUpgradeTasks` and assert none matches a synthetic dep `{ manager:
  "custom.regex", fileName: "tools/toolchain/versions/go-analysis.nix",
  depName: "golangci/golangci-lint" | "uber-go/nilaway", depType:
  "go-analysis" }` (reuse the rule-replay matcher, config.test.ts:134-186), so
  a future rule-level task that would evict the top-level refresher fails red;
- the top-level postUpgradeTasks carries the new command + fileFilter, and
  the command matches a bot-config allowedCommands entry (extends the
  existing pairing loop, config.test.ts:188-204).

Interfaces:

- `tools/renovate/config.json5` — two customManagers entries, one
  packageRule, top-level postUpgradeTasks command + fileFilter as above.
- `tools/renovate/config.test.ts` — new describe block
  `"go-analysis pins (RIG-3306)"`.
- `tools/toolchain/versions/go-analysis.nix` — header comment update only.

### T3 — `tools/renovate/refresh-go-analysis-hashes.ts` (red-green)

The dedicated refresher (Approach Piece 3). Imports `rewriteInlineHash` and
`parseGotForFragment` from `./refresh-fod-hashes.ts` (both exported today).
CONTRACT (load-bearing — `rewriteInlineHash` rewrites the first marker line
in the text it is given, and this file carries two of each marker): the
refresher SLICES the changed tool's attrset block out of the file, runs the
imported rewrite on the slice, and splices it back — it NEVER hands
`rewriteInlineHash` the whole file. This is what makes a golangci-lint bump
incapable of touching nilaway's `hash`/`vendorHash` and vice versa; T3's
test asserts the sibling block is byte-identical after each bump.

Flow per invocation: chdir to `git rev-parse --show-toplevel`; gate on
`git diff --quiet $baseRef -- tools/toolchain/versions/go-analysis.nix`
(baseRef = `origin/$RENOVATE_BASE_BRANCH` fallback `main`, the
refresh-fod-hashes.ts:239-247 shape); per changed tool block:
derived-field rewrite (golangci-lint `tag` from `version`; nilaway `version`
date from the rev's commit date via a token-free `git fetch --depth=1` — NOT
a REST call, see OQ-3), then fake both hashes → build `analysis.<tool>` →
write src `hash` from the source-FOD `got:` → rebuild → write `vendorHash`
from the `go-modules` `got:`. Exit 1 + restore original text on any missing
marker/`got:`/commit-date fetch failure. On load, assert the four
fragment strings (2 tools × src+vendor) are mutually non-substring (the
refresh-fod-hashes.ts:118-128 disjointness idiom, widened) so a
`--keep-going` co-failing FOD cannot cross-bind a tool's hash.

Test cycle (RED first): `tools/renovate/refresh-go-analysis-hashes.test.ts`
mirroring refresh-fod-hashes.test.ts — drive the ACTUAL shipped script in a
throwaway git repo with a stub `nix` on PATH emitting the
`hash mismatch … got:` shape (two sequential invocations: source-FOD
mismatch, then go-modules mismatch) and a stubbed `git` on PATH for the
commit-date fetch (the same stub-on-PATH shape the test uses for `nix`,
refresh-fod-hashes.test.ts:183); fixtures derived from the script's exported
table. Assert:

- a deliberately-stale golangci-lint bump (version line moved) rewrites
  `tag`, `hash`, `vendorHash` to the stub values, nilaway block untouched;
- a nilaway rev bump rewrites `version` (stub date), `hash`, `vendorHash`,
  golangci-lint block untouched;
- a stub-nix invocation emitting a FOREIGN `…-source.drv` mismatch block
  alongside the tool's own must NOT bind the foreign one (defends the READ
  site, complementing the slice/splice contract that defends the WRITE site);
- no-op branch (file unchanged vs base) exits 0 with no build;
- missing `got:` exits 1 and leaves the file at its original text;
- idempotence: a second run is a no-net-change.

Interfaces:

- New `tools/renovate/refresh-go-analysis-hashes.ts`; exports (for the test):
  `PIN_FILE = "tools/toolchain/versions/go-analysis.nix"`,
  `BUILD_FILE = "tools/toolchain/gate-tools.nix"`,
  `TOOL_ENTRIES: ToolEntry[]` where `ToolEntry = { tool: "nilaway" |
  "golangci-lint"; attr: string; srcFragment: string; vendorFragment:
  string; derived: "tag-from-version" | "version-from-rev-date" }`.
  `srcFragment`/`vendorFragment` are the literal pname-scoped drv-name
  fragments captured in T1 (`…-source`, `…-go-modules`), NOT the generic
  word `source`; a module-load assertion proves all four are mutually
  non-substring.
- Imports from `tools/renovate/refresh-fod-hashes.ts`: `rewriteInlineHash`,
  `parseGotForFragment`.
- Consumes T1's `analysis.<tool>` attrpath.
- New `tools/renovate/refresh-go-analysis-hashes.test.ts` (bun:test).

### T4 — bot-config allowlist + end-to-end wiring closure

Add `"^bun tools/renovate/refresh-go-analysis-hashes\\.ts$"` to
`allowedCommands` in `tools/renovate/bot-config.json5` (seventh entry;
update the "Six entries, all load-bearing" count/comment,
bot-config.json5:86-127) AND the config.test.ts `toHaveLength(6)` count
assertions (config.test.ts:210-212), which go red the moment the seventh
command lands. The config.test.ts pairing loop from T2 goes green here (it is
the same PR as T2/T3, so T4 introduces no new observable contract of its own
— it is the closure + full-suite verification step, not a fourth red-green
cycle). The `toHaveLength(6)`→`(7)` count edit, the "Six entries" prose
count, and the seventh allowlist entry are one easy-to-split triple — land
all three together. Run the full affected-area suite: `bun test
tools/renovate/` + Biome + a live `nix build -f
tools/toolchain/gate-tools.nix analysis.golangci-lint --no-link` smoke
(proves T1's vehicle + current pins coherent).

Interfaces:

- `tools/renovate/bot-config.json5` — one `allowedCommands` entry + comment.
- No other files.

## Tasks

- [ ] T1: `analysis` output on `tools/toolchain/gate-tools.nix`; smoke-build
      both pinned tools.
- [ ] T2: two customManagers + nilaway cooldown packageRule + top-level task
      wiring in `tools/renovate/config.json5`; pin-file header update;
      config.test.ts guards (red → green).
- [ ] T3: `tools/renovate/refresh-go-analysis-hashes.ts` + red-green test
      against the shipped script with stub nix.
- [ ] T4: bot-config.json5 allowlist entry; full `bun test tools/renovate/`
      + Biome + live vehicle smoke-build green.

## Open Questions

### OQ-1 (deferred) — golangci-lint: match the `version` line or the `tag` line?

Recommendation: **match `version`** (designed against). Keeps the uniform
versions/*.nix manager idiom (all four existing managers match `version =` —
config.json5:263-319) and leaves the refresher deriving `tag = "v" + version`,
the simpler derivation. The alternative (match `tag`, drop extractVersion,
refresher derives `version`) is equivalent in outcome; this is a taste call,
not load-bearing — either way one sibling line is refresher-owned and the
config.test.ts extraction guard pins whichever is chosen.

### OQ-2 (load-bearing) — null the 5-day cooldown for nilaway?

Recommendation: **yes, null it** (designed against). With
`internalChecksFilter: "strict"`, a git-refs digest on a moving `main` HEAD
carries no release timestamp, so the strict cooldown marks it permanently
`pending` and Renovate cuts ZERO nilaway PRs forever — the silent-rot
outcome this issue exists to fix, in the RIG-1220 shape. All three existing
git-refs rules null it with the explicit "mechanically INAPPLICABLE"
rationale (config.json5:670-681). No `automerge` key exists anywhere in
tools/renovate/ (verified; config.json5:30 records the related
branch-protection posture, "main's branch protection does not require PRs
to be up to date"), so merging a nilaway digest PR requires a human action
— the identical compensating control the three shipped git-refs
cooldown-nulls rely on (config.json5:678-680, :737-739). (Whether branch
protection additionally ENFORCES a review is a repo-settings fact not
visible in-repo; the primary control is that nothing auto-merges.) The
practical review is the PR's upstream-diff link plus the red/green
analysis-battery build; OQ-4's optional weekly schedule is the honest place
to add de-facto soak. Flagged load-bearing because the brief
says "5-day cooldown applies — inherit it; do NOT exempt these pins":
golangci-lint fully inherits it, but a literal reading for nilaway
contradicts the zero-PRs mechanics. If Matt wants nilaway soaked anyway, the
alternative is dropping `internalChecksFilter` strictness for this dep — a
worse, wider knob — or accepting dashboard-only surfacing (no auto-PR),
which still beats today's nothing but fails the "open a bump PR" intent.

### OQ-3 (resolved — token-free shallow fetch) — nilaway `version` date source

The `0-unstable-<date>` version must track the new rev's commit date. It MUST
be a token-free `git fetch`, NOT a REST call: a postUpgradeTask child cannot
see any API token. Renovate filters the task env to a fixed `basicEnvVars`
allowlist + `customEnvVariables` (renovate@44.46.2 `getChildProcessEnv`,
dist/util/exec/env.js) — carrying no `RENOVATE_TOKEN`/`GH_TOKEN`/`GITHUB_TOKEN`
— and compass deliberately does NOT set `exposeAllEnv` (bot-config.json5:45).
The ONLY credential Renovate injects into a task child is git-transport auth
(GIT_CONFIG url.insteadOf rewrite rules, dist/util/git/auth.js), so a REST
`GET /repos/uber-go/nilaway/commits/<rev>` returns 401/403 on 100% of runs,
while a `git fetch` against github.com is exactly the transport Renovate DOES
authenticate (corroborated in-tree: every shipped refresher reads only
`RENOVATE_BASE_BRANCH`, never a token — none can). Mandated: in a temp dir,
`git init` + `git fetch --depth=1 https://github.com/uber-go/nilaway <rev>` +
`git log -1 --format=%cs FETCH_HEAD`; fail loud (exit 1) on non-zero exit or a
non-`YYYY-MM-DD` result. The stub in T3 becomes a stub `git` on PATH (cheaper
— the test already stubs `nix` the same way). Rejected alternative: adding a
token to `customEnvVariables` — it widens the bot's secret surface for one
date lookup git transport already answers. Unauthenticated `api.github.com`
(60/hr per shared runner IP) is moot: the sandbox forbids the authenticated
REST variant that would have fixed its rate limit. Leaving `version` stale is
rejected — it feeds the drv name via `inherit (pin) version` and misleads
humans reading the pin.

### OQ-4 (deferred) — nilaway bump cadence

The nilaway digest manager follows `main` daily (the git-refs default under
the daily cron; no `schedule` key proposed, matching no existing versions/*
manager carrying one). uber-go/nilaway averages a few commits/month, so
expected PR volume is low. If it proves noisy, add `schedule: ["before 4am on
monday"]` on the nilaway packageRule later — config-only, no design change.
