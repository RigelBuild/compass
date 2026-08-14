# Drop proto: devenv/nix as the single toolchain owner

Status: Draft

Tracker: SEA-1983. Template: orion PR #1266 (SEA-1897, merge `c9185039`) —
"refactor(ci): drop proto; pin bun/node/moon via nix, go via go-overlay".
Compass diverges from that template wherever the CI substrate does: orion runs
Woodpecker against a nix-built CI step image; compass runs GitHub Actions with
`setup-*` actions fed by a `.prototools`-parsing pins step. Every decision
below is grounded in this clone.

## Problem / Intent

Compass pins its language toolchains (bun, node, moon, go) through the proto
version manager while devenv/nix owns everything else — two managers, two pin
formats, and a parity gate built around the asymmetry. `devenv.nix:9-13`:

> ```nix
> #   proto  — owns the language/runtime toolchains (bun, node, moon, go),
> #            pinned in .prototools. Activated on shell entry below.
> #   devenv — provides proto itself plus everything non-language: the contract
> #            codegen tools, the Go analysis battery, and the linters the moon
> #            gate runs.
> ```

Drop proto entirely and make devenv/nix the single owner of every toolchain,
as orion already did — one pin format, one activation path, one parity method,
and no `proto` shim quirks (the `PROTO_REPORTER` NDJSON-banner workaround,
`devenv.nix:90-108`, exists only because proto's shims corrupt `go list`
output in agent shells).

## Approach

### The pin source (fork 5 prerequisite): `tools/toolchain/versions/*.nix`

Four per-language pin files under `tools/toolchain/versions/` — orion's shape
(`ci/toolchain/versions/{bun,node,moon}.nix`, orion #1266), relocated to
compass's existing toolchain-gate directory:

- `bun.nix`, `node.nix`, `moon.nix` — `rec { version = "…"; srcs = { "<system>" = { url, hash }; }; }`
  vendored-release pins, exactly orion's file shape (orion
  `ci/toolchain/versions/bun.nix`: `version = "1.3.14"; srcs = { "x86_64-linux" = { url = "https://github.com/oven-sh/bun/releases/download/bun-v${version}/…`).
  Compass pins stay at today's versions — this is a pure manager cutover, no
  version bumps: bun `1.3.13`, node `24.18.0`, moon `2.4.2` (`.prototools:6-8`:
  `bun = "1.3.13"` / `node = "24.18.0"` / `moon = "2.4.2"`). Systems covered:
  `x86_64-linux`, `aarch64-linux`, `aarch64-darwin` (orion's set; compass
  devenv.nix already branches on `pkgs.stdenv.isLinux` for the app closure,
  `devenv.nix:127`, so darwin is a supported dev platform).
- `go.nix` — version only (`{ version = "1.26.6"; }`); the derivation comes
  from the go-overlay input (next section), which carries its own per-platform
  hashes. Designed against the post-#298 tree: SEA-1982 bumps the pin to
  1.26.6 (`.prototools:13`: `go = "1.26.6"`), and go-overlay ships a
  `manifests/go/1.26.6.nix` (verified against
  `purpleclay/go-overlay@main:manifests/go/`).

A sibling `tools/toolchain/toolchain-tools.nix` builds bun/node/moon
derivations from the pin files (orion's `ci/toolchain/toolchain-tools.nix`
role): fetch the release artifact for `pkgs.stdenv.system`, unpack, install
`bin/`. Both the dev shell (`devenv.nix`) and CI (via `gate-tools.nix`, below)
import this one file, so they cannot drift.

Why vendored releases and not nixpkgs attrs: moon's rationale is already in
the repo — `.prototools:4-5`: "moon is pinned HERE rather than taken from
nixpkgs (which lags on the 1.x line) so it tracks the 2.x series" — and bun
needs an *exact* version (the agent image asserts on it,
`agent-image/toolchain.nix:51`), which a rolling nixpkgs can't promise. Node
rides the same mechanism for uniformity: one pin shape, one bump procedure,
per-language exactness independent of the devenv-nixpkgs roll.

### Fork 1 — how GHA CI obtains the toolchains: nix, replacing the setup-* actions

**Decision: (b)** — extend the existing "dev shell's nixpkgs tools on PATH"
mechanism to also provide the language toolchains, and delete the pins step,
`setup-bun`/`setup-node`/`setup-go`, and the npm moon install.

Today ci.yml installs the runtimes through four independent non-nix
mechanisms fed by a sed parse of `.prototools`
(`.github/workflows/ci.yml:154-156`: `pins=$(sed -n '/^\[/q; s/…/p' .prototools)`;
`:180-193` setup-bun/node/go from `steps.pins.outputs.*`; `:207`
`npm install --global "@moonrepo/cli@$MOON_VERSION"`), while the nixpkgs half
already comes from nix at the devenv.lock revision (`:241-245`, via
`tools/toolchain/gate-tools.nix`). Post-proto there is no `.prototools` to
parse; option (a) — keep `setup-*` but read versions out of the new nix pin
files — would keep four install mechanisms and the two-substrate drift risk
the parity gate exists to police, purely to preserve GH's toolchain caches.
Option (b) makes CI run the identical derivations the dev shell runs —
the single-owner point of SEA-1983 — and shrinks ci.yml. The nix step's own
comment already frames nix as the mechanism that reproduces dev-shell pins
where setup-* can't (`ci.yml:230-236`); post-proto that applies to every
toolchain.

Mechanically, `gate-tools.nix` gains a `langs` output (bun/node/moon from
`toolchain-tools.nix`, go single-sourced with the dev shell — Open Question
2), alongside the existing `env`/`identity` outputs
(`tools/toolchain/gate-tools.nix:45-61`). Its head today is `{ attrs }:`
with NO default (`gate-tools.nix:25`), so a bare
`nix build -f tools/toolchain/gate-tools.nix langs` would fail at eval; the
file is restructured to `{ attrs ? [ ] }:` — `langs` never consumes `attrs`
(the language set is closed), so phase 1 below needs no `--arg attrs`.

**The bootstrap ordering constraint (compass-specific, absent in orion):**
today CI needs bun *before* the nix step, because that step runs
`bun tools/toolchain/parity.ts --print-nix-attrs` to derive the attr list
(`ci.yml:242`) — and bun currently arrives from setup-bun. Orion never hit
this: its Woodpecker image pre-bakes everything. The fix is a two-phase nix
bootstrap: phase 1 `nix build`s the fixed `langs` output (no attr parse
needed — the language set is closed, and `attrs` now has a default) and puts
its `bin/` on PATH; phase 2 runs the existing `--print-nix-attrs` flow with
the now-available bun to build the nixpkgs `env`. One semantics point is
load-bearing: `$GITHUB_PATH` only affects *subsequent* steps, never the
remainder of the step that writes it — the current nix step already relies
on exactly that, appending for the later parity step to consume
(`ci.yml:245`: `echo "$out/bin" >>"$GITHUB_PATH"`, consumed by the separate
`Toolchain parity` step at `:247-251`). So the two phases are either two
separate `run:` steps or one step that additionally `export`s PATH in-shell
— Open Question 3, Matt's call. Cost: phase 1 is fetch-and-unpack of
prebuilt release artifacts (the `versions/*.nix` pins are vendored release
binaries, not from-source builds), so it stays cheap even on a substituter
miss; both phases hit the substituters already configured
(`ci.yml:226-227`).

### Fork 2 — Go: adopt the purpleclay/go-overlay input

**Decision: go-overlay**, orion's choice (orion `devenv.nix`: `goToolchain =
inputs.go-overlay.packages.${pkgs.stdenv.system}."go_1_26_5"`; orion
`devenv.yaml`: "go-overlay builds the exact go pin for the dev shell + the CI
step image"). Compass adds the input to `devenv.yaml` (which today declares
nixpkgs as "the only input this shell needs" precisely because "the toolchain
is nixpkgs derivations plus the proto-managed runtimes", `devenv.yaml:3-4`)
and version-selects in `devenv.nix` from `versions/go.nix`:
`goToolchain = inputs.go-overlay.packages.${pkgs.stdenv.system}."go_${lib.replaceStrings ["."] ["_"] goPin.version}"`.

Why not `actions/setup-go` with a nix-sourced version string: that keeps Go as
the one toolchain CI installs outside nix — the exact split this change
retires — and leaves the dev shell needing a *second* Go source anyway (the
shell can't run a GitHub action; today it gets Go from `proto install`,
`devenv.nix:161`). Why not nixpkgs' `go`: the pin must be exact and
promptly bumpable for security releases (the floor-policy comment,
`.prototools:9-12`), and a rolling nixpkgs controls neither. go-overlay is
purpose-built for exact Go pins, proven in orion, and its manifest already
carries `1.26.6`.

One constraint is load-bearing and open: the dev shell's go and the parity
gate's `langs` go must be the SAME derivation, or the store-path verdict for
go fails structurally — how both sides single-source it is Open Question 2.

The floor policy is unchanged in substance and moves its reference:
`go/go.mod:10-12` ("The `go` directive tracks the .prototools pin minus at
most one minor, so an upstream Go security patch never blocks on a mod edit")
re-points at `tools/toolchain/versions/go.nix`. `go 1.25.0` (`go/go.mod:15`)
stays valid against a 1.26.6 pin.

### Fork 3 — the toolchain-parity gate: rework, not retire

**Decision: rework.** The gate's two-method asymmetry is documented as a
consequence of the two pin shapes (`tools/toolchain/parity-core.ts:6-29`:
".prototools — bun/node/moon/go, each a literal version string … devenv.nix —
… bare nixpkgs attribute names with NO version literal"). Post-proto every
toolchain resolves to a nix derivation, so the asymmetry collapses and
**store-path becomes the single, uniform method**:

- **nixpkgs half**: unchanged in substance — `parseDevenvPackages`
  (`parity-core.ts:119`) still extracts the attrs and `verifyStorePath`
  (`parity-core.ts:203`) still compares against `gate-tools.nix` `identity`.
  The parser is re-keyed to fork 4's re-shaped packages list: today it keys
  on the literal `packages = with pkgs; [` (`parity-core.ts:120`:
  `source.indexOf("packages = with pkgs; [")`) and slices to the first `];`
  (`:123`); it moves to keying on `packages = (with pkgs; [` and slicing to
  the matching `])`, parsing the nixpkgs half exactly as before and never
  touching the appended language list.
- **language half**: `verifyStorePath` against the `langs` derivations
  (gate-tools.nix's new output) — covered by the store-path verdict ONLY.
  This is the method the file itself ranks strongest (`parity-core.ts:25-27`:
  "STRICTLY STRONGER than a version-string match (it identifies the exact
  derivation, not a coincidence of version numbers)"), and it subsumes what
  self-report caught: an ambient runtime shadowing the pinned one resolves
  outside the expected store path and fails.
- **self-report retires with proto.** `parseProtoTools`
  (`parity-core.ts:75`), `verifySelfReport` (`:172`), `extractVersion`
  (`:159`) and the probe table (`parity.ts:38-43`, `PROTO_PROBES`) are
  deleted, not re-pointed: their premise was a pin that is only a version
  literal with no derivation identity (`parity-core.ts:9-10`: "The pin IS
  the text"). Post-cutover the `versions/*.nix` literal cannot silently
  drift from the artifact — `srcs.<system>.hash` pins the exact release
  bytes (a wrong literal mislabels, it cannot substitute), which T1's
  build-and-assert verify catches once at creation, and go's selector fails
  nix eval on a nonexistent overlay attribute.

Retiring the gate was rejected: in CI the store-path half becomes
near-tautological (PATH is built from the same derivations), but the gate also
runs in every dev shell via the pre-push hook — `tools/toolchain/moon.yml:10-12`:
"running it through `:ci` means the pre-push hook checks parity too, so a
contributor whose PATH has drifted finds out before pushing" — where
store-path still catches an ambient runtime shadowing the pinned one.
The zero-parse refusal (`parity.ts:110-119`, "Refusing to report a pass over
nothing") is re-pointed: it now refuses when `parseDevenvPackages` returns
empty OR the `langs` identity set is empty (the `.prototools` read at
`parity.ts:103-105` is deleted with the file).

The gate's sed-parse counterpart in ci.yml dies with the pins step. The test
suite (`parity-core.test.ts:55-88` `parseProtoTools` block, and the devenv
packages fixture at `:92-96` which includes the `proto` package line) is
rewritten: the `parseProtoTools` suite is deleted with its function, the
fixture moves to the fork-4 concatenated shape, and new cases assert that
the appended language list is NOT parsed and that a dotted token INSIDE the
`(with pkgs; [ … ])` literal still throws (`parity-core.ts:140-145`).

### Fork 4 — devenv.nix / devenv.yaml deltas

Grounded in the current file, the complete delta set:

**Remove:**

- The `proto` package (`devenv.nix:26-27`: "# Language/runtime manager. Pins
  bun/node/moon/go via .prototools." / `proto`).
- `PROTO_REPORTER = "text";` and its 18-line rationale block
  (`devenv.nix:90-108`) — the proto-shim NDJSON-banner bug it works around
  cannot exist without shims.
- The enterShell activation (`devenv.nix:156-161`: `export PROTO_HOME=…`,
  `export PATH="$PROTO_HOME/shims:…"`, `${pkgs.proto}/bin/proto install`).
  `bun install --frozen-lockfile` and the `hk install` lines (`:165-171`)
  stay.
- All four `PROTO_HOME`/`PATH` shim exports in the process/task exec blocks:
  compass-server (`devenv.nix:237-244`: "Go is proto-managed (.prototools);
  make its shim resolvable even when this process is launched outside the
  enterShell PATH mutation"), compass-runner (`:323-324`), gen-cert
  (`:357-358`), mint-runner-token (`:377-378`). With go a `packages` entry it
  is on the devenv profile PATH these processes inherit; no shim dir exists to
  export.
- The header split comment (`devenv.nix:7-16`) rewritten to the
  nix-owns-everything model.

**Add:**

- `devenv.yaml`: the `go-overlay` input (`url: github:purpleclay/go-overlay`,
  `inputs.nixpkgs.follows: nixpkgs` — orion's exact block) and a comment
  update (`devenv.yaml:3-5` currently: "the toolchain is nixpkgs derivations
  plus the proto-managed runtimes (.prototools)"). `devenv.lock` gains the
  input's nodes.
- `devenv.nix`: `inputs` added to the function head (`devenv.nix:1-6` today
  destructures only `pkgs, lib, config`); let-bindings `goPin` (import
  `versions/go.nix`), `goToolchain` (go-overlay selector, pending OQ2),
  `toolchainTools` (import `tools/toolchain/toolchain-tools.nix`); and the
  packages list re-shaped to a concatenation that keeps the parsed literal
  language-free:

  ```nix
  packages = (with pkgs; [
    # …the existing nixpkgs attrs, unchanged…
  ]) ++ [
    toolchainTools.bun
    toolchainTools.node
    toolchainTools.moon
    goToolchain
  ];
  ```

  The language derivations can NEVER go inside the `with pkgs; [ … ]`
  literal: `parseDevenvPackages` THROWS on any token that is not a bare
  attribute name (`parity-core.ts:136-145` — a token failing
  `/^[A-Za-z][A-Za-z0-9_-]*$/` hits "is not a bare nixpkgs attribute name,
  so this gate cannot resolve it"), and its contract makes throwing
  deliberate (`parity-core.ts:103`: "Anything that is not a bare attribute
  name THROWS rather than being skipped") — a dotted `toolchainTools.bun` is
  exactly the rejected shape. The concatenation keeps the nixpkgs half a
  clean literal, but it also moves the opening text out from under the
  current `indexOf("packages = with pkgs; [")` (`parity-core.ts:120`), which
  would then return `[]` and trip the zero-parse refusal
  (`parity.ts:110-118`) — so the fork-3 parser re-key is a hard prerequisite
  of this shape change, and the two land atomically (Plan: T2+T4). Coverage
  of the appended language list is the store-path `langs` verdict alone
  (fork 3); it is never parsed.
- `.envrc`: `watch_file .prototools` (`.envrc:8-10`: "watch it to pick up
  bun/node/moon/go bumps") becomes `watch_file` over the four
  `tools/toolchain/versions/*.nix` files.

### Fork 5 — the full proto surface

Every file referencing `.prototools`/proto-the-manager in this clone
(grep over `prototools|proto install|PROTO_HOME|PROTO_REPORTER|pkgs.proto|moonrepo.dev/proto`;
protobuf/`proto/` schema hits are unrelated and excluded):

| File | Reference | Action |
| --- | --- | --- |
| `.prototools` | the pin file itself | delete |
| `.envrc:8-10` | `watch_file .prototools` | re-point at `versions/*.nix` |
| `devenv.yaml:3-5` | "proto-managed runtimes" comment | rewrite + add go-overlay input |
| `devenv.nix` | see fork 4 | fork 4 deltas |
| `devenv.lock` | — | gains go-overlay nodes |
| `.github/workflows/ci.yml:136-207` | pins step, setup-bun/node/go, npm moon | replace per fork 1 |
| `.github/workflows/ci.yml:229-245` | nix tools step | two-phase per fork 1 |
| `.github/workflows/publish-agent-image.yml:64-67` | ".prototools is deliberately EXCLUDED" paths comment | re-point: include `tools/toolchain/versions/bun.nix` (it now changes the image — see agent-image task) |
| `agent-image/toolchain.nix:5-6,48,50-56` | `protoTools = builtins.fromTOML (builtins.readFile ../.prototools)` + bun assert | consume the shared vendored bun (task 3) |
| `agent-image/moon.yml:51-54,73` | `/.prototools` in inputs + rationale | swap input to the bun pin file |
| `tools/toolchain/parity-core.ts` | header, `ProtoPin`, `parseProtoTools` | fork 3 rework |
| `tools/toolchain/parity.ts:33-43,103-105,180` | `PROTO_PROBES`, `.prototools` read, report title | fork 3 rework |
| `tools/toolchain/parity-core.test.ts:55-96` | parseProtoTools suite + fixture | rewrite |
| `tools/toolchain/package.json:5` | description mentions `.prototools` | reword |
| `tools/toolchain/moon.yml:3-6` | header mentions `.prototools` literals | reword |
| `tools/toolchain/gate-tools.nix` | — | gains `langs` output |
| `.moon/workspace.yml:4-7` | "bun/node/go/moon via proto … keeps .prototools the single version source" | reword |
| `go/go.mod:10-12` | floor-policy comment | re-point at `versions/go.nix` |
| `go/moon.yml:11-14` | "proto installs the .prototools Go pin" | reword |
| `package.json:5` | "proto (.prototools) owns the bun/node toolchains" | reword |
| `AGENTS.md:9-11` | "The toolchain is proto … plus devenv" | reword |
| `CONTRIBUTING.md:8-18` | proto install path incl. the no-nix route | reword (no-nix route: install the pinned versions by hand from `versions/*.nix`) |
| `README.md:61-93` | toolchain split + no-nix route | reword |
| `docs/architecture/build-and-ci.md:19-21,34-36,49-50,100-127,221-228` | proto/parity/CI-toolchain sections | reword |
| `apps/eng-docs/moon.yml:5`, `apps/ui/moon.yml:2-3` | "devenv/proto toolchain on PATH" | reword |
| `forks/oh-my-pi/moon.yml:12-22` | "bun, which this repo already pins (.prototools) … BUN PIN DIVERGENCE" | re-point at `versions/bun.nix`; divergence note stays (pin remains 1.3.13 vs fork's 1.3.14) |

Deliberately NOT touched: merged design records that mention `.prototools`
(`docs/designs/platform/compass-agent-image-publish.md:174,214`,
`docs/designs/repo/compass-eng-docs/design.md:51,353`) — frozen contracts, a
later change adds records rather than rewriting merged ones (repo AGENTS.md
convention, restated in skill://design: "There is no folding-in after
merge").

## Alternatives considered

### Keep setup-* actions reading versions from the nix pin files (fork 1a)

Smallest ci.yml diff and keeps GH-hosted toolchain caches, but preserves the
two-substrate split (four non-nix installers + one nix step) that SEA-1983
exists to end, keeps a sed/eval parse of a pin file in YAML
(the injection-hardening the current step needs, `ci.yml:149-153,200-204`, is
a cost of exactly this pattern), and leaves the parity gate policing a
substrate seam that no longer needs to exist. Rejected: the nix path is
already proven in this workflow for the harder half, and substituters bound
the runtime cost.

### nixpkgs-sourced bun/node/moon instead of vendored pin files

One less file shape, but forfeits exact pinning: moon "nixpkgs … lags"
(`.prototools:4-5`), and the agent image's bun assert exists because nixpkgs
bun drifts (`agent-image/toolchain.nix:51-55`). Rejected; orion reached the
same verdict (its versions/ files are the artifact of that rejection).

### Retire the parity gate

Rejected — see fork 3: the pre-push dev-shell leg and ambient-shadowing
detection retain value independent of CI's install path.

## Global Constraints

1. **Gated on SEA-1982 / PR #298.** Implementation starts only after the Go
   1.26.5→1.26.6 govulncheck bump merges — this change edits `.prototools`
   (deleting it) and `devenv.nix`, both touched by #298, and must not race
   it. All pins here assume the post-#298 tree (Go pin = `1.26.6`,
   `.prototools:13`).
2. **Go floor policy (unchanged in substance).** The `go` directive in
   `go/go.mod` tracks the Go pin minus at most one minor
   (`go/go.mod:10-12`); the pin source becomes
   `tools/toolchain/versions/go.nix`.
3. **GitHub Actions substrate, not Woodpecker.** No CI step image exists or
   is introduced; everything lands in `.github/workflows/ci.yml` steps.
   Orion's `ci/toolchain` layout is a template only.
4. **Pure manager cutover — zero version bumps.** bun `1.3.13`, node
   `24.18.0`, moon `2.4.2`, go `1.26.6` before and after.
5. **Frozen design records are not edited** (see fork 5 table footnote).
6. **`tools/toolchain/*` stays `MIT OR Apache-2.0`**
   (`tools/toolchain/package.json:6`) and dependency-free ("it must be able
   to run before `bun install` has", `tools/toolchain/package.json:5`).
7. **One required check.** ci.yml stays one job (`ci.yml:4` "ONE JOB, NOT A
   MATRIX"); no new jobs. Steps may be replaced or split (OQ3 option A adds
   one).

## Plan

Six implementation tasks. T1 is the root; **T2 and T4 are coupled and land
as one change** — the gate reads `.prototools` and the packages literal at
startup (`parity.ts:103-108`) and refuses or throws if either half moves
alone: deleting `.prototools` breaks the read at `parity.ts:103-105`, the
re-shaped packages list empties `parseDevenvPackages` until the T4 re-key
lands (`parity-core.ts:120-121`), and the `langs` store-path verdicts cannot
pass while the shell's languages still come from proto shims. T3 fans out
from T1; T5 consumes T1–T4; T6 sweeps. Every Verify line below is stated at
a boundary where it can actually pass.

**T1 — pin source + derivations.** Create
`tools/toolchain/versions/{bun,node,moon,go}.nix` and
`tools/toolchain/toolchain-tools.nix` (fetch/unpack derivations for
bun/node/moon over `x86_64-linux`, `aarch64-linux`, `aarch64-darwin`).
Verify: `nix build -f tools/toolchain/toolchain-tools.nix bun` (etc.)
produces binaries reporting exactly `1.3.13` / `24.18.0` / `2.4.2`.

**T2 — dev shell cutover (lands with T4 as one change).** Fork 4's
devenv.yaml/devenv.nix/.envrc deltas + `devenv.lock` refresh. Verify (at the
combined T2+T4 boundary — not achievable standalone, see Plan intro): fresh
`direnv allow` shell has `bun|node|moon|go` resolving into `/nix/store` at
the pinned versions; `moon run :ci` green locally; `devenv up` processes
build Go binaries without the deleted shims.

**T3 — agent image.** `agent-image/toolchain.nix` drops the
`fromTOML ../.prototools` read + assert (`:48-56`) and takes bun from
`toolchain-tools.nix` (the assert's reason — "close enough is not a pin",
`:44-45` — is satisfied structurally by consuming the pinned derivation).
`agent-image/moon.yml` swaps `/.prototools` (`:73`) for
`/tools/toolchain/versions/bun.nix` and reworks the `:51-54` rationale;
`publish-agent-image.yml` paths gain the bun pin file (it now changes the
output, inverting the `:65-67` exclusion rationale). Verify:
`moon run agent-image:build` green; pin-file edit reddens the build.

**T4 — parity gate rework (lands with T2 as one change).** Fork 3:
`parseDevenvPackages` re-keyed to the concatenated shape, the self-report
machinery deleted, store-path verdicts for the language half via
`gate-tools.nix`'s new `langs` output (and `attrs` given a default), test
rewrite, zero-parse refusal against the new sources, comment sweep in
`tools/toolchain/{moon.yml,package.json}`. Verify (at the combined T2+T4
boundary): `moon run toolchain-parity:ci` green in the T2 shell; an ambient
bun ahead on PATH fails store-path; a dotted token inside the
`(with pkgs; [ … ])` literal still throws in the unit suite.

**T5 — ci.yml cutover.** Delete the pins step (`:136-178`), setup-bun/node/go
(`:180-193`), npm moon (`:195-207`); replace the nix tools step (`:229-245`)
with the two-phase bootstrap (langs → PATH → `--print-nix-attrs` → env →
PATH; step shape per OQ3).
Verify: full CI green on the PR; the parity step's report shows every tool
verified; no `setup-` action remains in the workflow.

**T6 — docs/comment sweep.** Every remaining fork-5 row: workspace.yml,
go/go.mod, go/moon.yml, package.json, AGENTS.md, CONTRIBUTING.md, README.md,
build-and-ci.md, apps/*/moon.yml, forks/oh-my-pi/moon.yml. Verify:
the fork-5 grep set returns zero live-manager hits outside frozen design
records; `moon run root:markdownlint` green.

## Tasks

- [ ] **T1: pin files + toolchain-tools.nix**
  Interfaces: produces `tools/toolchain/versions/{bun,node,moon,go}.nix`
  (`rec { version; srcs.<system>.{url,hash}; }`; go.nix version-only) and
  `tools/toolchain/toolchain-tools.nix` (`{ pkgs }: { bun; node; moon; }`,
  each a derivation with `bin/`). Consumes: release URLs/hashes for bun
  1.3.13, node 24.18.0, moon 2.4.2 (orion `versions/*.nix` as the shape
  reference).
- [ ] **T2: devenv cutover** *(one landing unit with T4)*
  Interfaces: consumes T1; edits `devenv.yaml` (go-overlay input, pending
  OQ2), `devenv.nix` (fork-4 removes + adds; `inputs` in the arg set;
  packages re-shaped to `(with pkgs; [ … ]) ++ [ …languages ]`), `.envrc`,
  `devenv.lock`. Produces a dev shell whose bun/node/moon/go are nix store
  paths at the pinned versions.
- [ ] **T3: agent image on the shared bun pin**
  Interfaces: consumes T1 (`toolchain-tools.nix` bun); edits
  `agent-image/toolchain.nix` (drop `protoTools`/assert, import shared bun),
  `agent-image/moon.yml` inputs, `.github/workflows/publish-agent-image.yml`
  paths.
- [ ] **T4: parity gate rework** *(one landing unit with T2)*
  Interfaces: consumes T1 pin files + T2 devenv.nix; edits
  `tools/toolchain/{parity-core.ts,parity.ts,parity-core.test.ts,package.json,moon.yml,gate-tools.nix}`.
  Produces: `parseDevenvPackages` re-keyed to `packages = (with pkgs; [` /
  `])`; `parseProtoTools`, `verifySelfReport`, `extractVersion`, and
  `PROTO_PROBES` deleted; `gate-tools.nix` head becomes `{ attrs ? [ ] }:`
  (today `{ attrs }:` with no default, `gate-tools.nix:25`) with a `langs`
  output that never consumes `attrs` (go source per OQ2); store-path
  verdicts for the language half against `langs`.
- [ ] **T5: ci.yml cutover**
  Interfaces: consumes T1+T4 (`langs` output, `--print-nix-attrs` unchanged);
  edits `.github/workflows/ci.yml` only (steps `:136-207` deleted, `:229-245`
  reworked, header comments at `:103-107` in devenv.nix already handled by
  T2).
- [ ] **T6: docs + comment sweep**
  Interfaces: edits `.moon/workspace.yml`, `go/go.mod`, `go/moon.yml`,
  `package.json`, `AGENTS.md`, `CONTRIBUTING.md`, `README.md`,
  `docs/architecture/build-and-ci.md`, `apps/eng-docs/moon.yml`,
  `apps/ui/moon.yml`, `forks/oh-my-pi/moon.yml` — comment/prose only, no
  behavior.

## Open Questions

1. **Pin-bump automation (non-load-bearing — deferred).** Compass has no
   renovate (orion #1266 added four `custom.regex` managers there); dependabot
   covers only github-actions/bun-lockfile/gomod
   (`.github/dependabot.yml:12,28,40`) and has no nix ecosystem, so
   bun/node/moon/go pin bumps become manual PRs. This is no regression —
   `.prototools` bumps are manual today (SEA-1982 is one) — and the design is
   correct without automation. Recommendation: accept manual bumps now;
   revisit if/when compass adopts a renovate config, reusing orion's regex
   managers re-targeted at `tools/toolchain/versions/*.nix`.
2. **LOAD-BEARING — go derivation single-sourcing (Matt to rule at the
   design PR).** The dev shell's go and the parity gate's `langs` go MUST be
   the same derivation: `verifyStorePath` compares the resolved binary
   against the expected store path (`parity-core.ts:23-25`: "`realpath` of
   the binary on PATH must be inside the store path that the
   devenv.lock-pinned nixpkgs resolves that attribute to"), so two different
   go derivations make the go verdict fail structurally. A `langs`-side
   vendored-go fallback is therefore NOT contract-invisible or local to T4
   (as an earlier draft claimed): triggering it forces devenv.yaml/devenv.nix
   (T2) to also drop the go-overlay input and vendor go — a cross-fork
   change. Options:
   - **A — both sides consume go-overlay.** devenv.nix via the flake input
     (fork 2); gate-tools.nix via `fetchTarball` of go-overlay at the
     devenv.lock-pinned rev + the `versions/go.nix` selector (go-overlay has
     a root `default.nix` — verified against `purpleclay/go-overlay@main`).
     Keeps fork 2's decision and orion's shape. Risk: the two consumption
     routes must demonstrably resolve the same store path, so the combined
     T2+T4 verify includes the go store-path verdict passing in a fresh
     shell.
   - **B — both sides consume a vendored go** from `versions/go.nix` +
     per-platform hashes, the bun/node/moon shape; the go-overlay input is
     dropped and fork 2 is reversed. Trivially one derivation, but
     re-implements the per-release hash maintenance go-overlay exists to
     carry, and diverges from orion.
   Recommendation: **A**, with B as the pre-agreed fallback — applied to
   BOTH consumers in the same change, never to `langs` alone. The invariant
   either way: dev-shell go and `langs` go are the same derivation, or
   store-path parity is a false gate.
3. **LOAD-BEARING — two-phase CI bootstrap step semantics (Matt to rule at
   the design PR).** `$GITHUB_PATH` affects only *subsequent* steps, never
   the remainder of the step that writes it — today's nix step depends on
   exactly this (`ci.yml:245` appends; the separate parity step at
   `:247-251` consumes) — so fork 1's phase 2 cannot see phase 1's
   `$GITHUB_PATH` append if both share one `run:`. Options:
   - **A — two separate `run:` steps.** Phase 1 builds `langs` and appends
     `$GITHUB_PATH`; phase 2 is a later step and inherits it. Matches the
     workflow's existing PATH idiom, gives per-phase timing and log
     attribution; costs one extra step (allowed — Global Constraint 7 bars
     new *jobs*, not steps).
   - **B — one step, in-shell export.** Phase 1 ends with
     `export PATH="$langs/bin:$PATH"` so phase 2 sees it in the same shell,
     plus the `$GITHUB_PATH` append for later steps. One step fewer, but
     PATH is constructed through two mechanisms in one script and a phase-2
     failure is harder to attribute in the step log.
   Recommendation: **A**.
