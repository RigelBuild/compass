# Share Nix tooling from the shared tools repo (RIG-4540)

## Problem / Intent

The shared tools record (repo/compass-shared-tools-repo, T10) rules that the
shared tools repo is also a flake for the Nix code that compass and the private
consumer both carry, but it does not say which Nix code that is. This record
measures it. It plans each move to a flake output and plans how compass pins
that output: a `devenv.yaml` input, a Renovate rule, and a `devenv.lock`
relock. The measurement found less shared Nix than the ruling expected, so
whether to ship it at all goes back to Matt (Open Question 1). The shared
repo's name is still open (RIG-4469), so this record uses the working name
`RigelBuild/repo-tools` and the input name `repo-tools`.

## Inventory

Measured on 2026-10-05 at compass `main` 8277cb8268e8, against the private
consumer's `main`. "Private copy" says only whether a copy exists and what role
it has. This public record names no path in the private repo.

Method, in three steps:

1. **Pairing.** Every tracked `.nix` file in compass (24) was scored against
   every tracked `.nix` file in the private consumer (155). The score is the
   Jaccard similarity of the two files' code lines, after comments and blank
   lines are removed and whitespace is collapsed. Each compass file keeps its
   best match.
2. **Text drift.** `git diff --no-index --stat` between the private copy and
   the compass copy (changes are compass relative to the private copy). The
   same diff again after comment lines and blank lines are removed.
3. **Build drift.** Both builders were evaluated with equal inputs: one nixpkgs
   (the rev that compass's root `devenv.lock` pins) and one pin set (compass's
   `versions/*.nix`). Each output's `drvPath` was compared on `x86_64-linux`,
   `aarch64-linux`, and `aarch64-darwin`. Equal inputs isolate code drift from
   pin skew.

### Files both repos carry

| Compass path | Private copy | Drift | Consumer literal | Flake output | Acceptance for the output |
| --- | --- | --- | --- | --- | --- |
| `tools/toolchain/toolchain-tools.nix` | Yes: the builder in its CI toolchain tree (score 0.78) | Structural. Text 17+/18−; code 6+/6−. Two changes: the node attr and `pname` are `node` in compass and `nodejs` in the private copy (this changes the node drv); four lines use `stdenv.isLinux` in compass and `stdenv.hostPlatform.isLinux` in the private copy (no drv change) | None in code. Both copies read pins with `import ./versions/<tool>.nix`, a layout literal. Each copy's comments name paths in its own repo | `lib.toolchainTools` | With equal inputs, `bun`, `node`, and `moon` have the same `drvPath` as compass's builder today, on all three systems |
| `tools/toolchain/versions/bun.nix` | Yes: its bun pin (0.56) | Version skew: 1.4.0 here, 1.4.2 there. Text 5+/5−; code 4+/4− (version and three hashes). Same structure after version and hashes are masked | None | None: stays local | Not changed by this record |
| `tools/toolchain/versions/node.nix` | Yes: its node pin (0.56) | Version skew: 24.20.0 here, 24.21.0 there. Text 5+/5−; code 4+/4−. Same structure | None | None: stays local | Not changed by this record |
| `tools/toolchain/versions/moon.nix` | Yes: its moon pin (0.56) | Version skew: 2.5.3 here, 2.5.4 there. Text 8+/6− (compass has two more comment lines); code 4+/4−. Same structure | None | None: stays local | Not changed by this record |

All three pin files hold `srcs` for the same three systems in both repos:
`x86_64-linux`, `aarch64-linux`, `aarch64-darwin`.

Build drift of the builder, with equal inputs. Store hashes are shortened to
their first eight characters:

| Output | `x86_64-linux` | `aarch64-linux` | `aarch64-darwin` |
| --- | --- | --- | --- |
| `bun` | Equal (`jiv0c4pb…-bun-1.4.0.drv`) | Equal (`sv9l9q6r…-bun-1.4.0.drv`) | Equal (`i9hmhyb0…-bun-1.4.0.drv`) |
| `moon` | Equal (`7g78kqbs…-moon-2.5.3.drv`) | Equal (`33wnm1fi…-moon-2.5.3.drv`) | Equal (`l6kcsl12…-moon-2.5.3.drv`) |
| `node` | Differs: `node-24.20.0` against `nodejs-24.20.0` | Differs, same cause | Differs, same cause |

When a staged private copy had only its node attr and `pname` renamed to
`node`, all nine `drvPath`s were equal. So the `isLinux` spelling does not
change any drv. The name is the only code drift that changes a build.

### Files that look shared but stay local

These scored low, or match only on generic lines. None is a shared output.

| Compass path | Best match | Why it stays local |
| --- | --- | --- |
| `tools/toolchain/versions/go.nix`, `tools/toolchain/versions/go-analysis.nix` | 0.00, 0.14 | The private consumer has no go pin file. Its go comes straight from its go-overlay input. |
| `agent-image/toolchain.nix` | 0.29, its CI base-image module | The 22 shared lines are generic: `let`, `in`, package names, and the `nixConf = pkgs.writeTextDir` header. The `nix.conf` bodies differ (compass: single-user nix with no sandbox and a pinned CA bundle; private: flakes plus `accept-flake-config`). They are different images. This file consumes `lib.toolchainTools`. |
| `tools/toolchain/gate-tools.nix`, `tools/toolchain/go-analysis.nix` | 0.11, 0.10 | Compass's parity-gate plumbing. `gate-tools.nix` consumes `lib.toolchainTools`. |
| `tools/toolchain/chromium-e2e-env.nix` (0.15), `gtk-e2e-env.nix` (0.21), `gtk-closure.nix` (0.16), `secretspec-env.nix` (0.23), `skopeo-nix2container-env.nix` (0.22), `microvm-vmm-env.nix` (0.25) | Unrelated host or CI modules | Compass-only test and image environments. They share only the lock-reading header. |
| `tools/renovate/agent-image-fod-vehicle.nix` (0.26), `tools/renovate/ui-fod-vehicle.nix` (0.23) | Unrelated host modules | Compass-only FOD hash vehicles. |
| `devenv.nix` (0.13), `agent-image/devenv.nix` (0.17), `flake.nix` (0.09), `agent-image/entrypoint.nix` (0.10), `apps/ui/dist.nix` (0.11), `guest-image/default.nix` (0.04), `guest-image/assembly-tests.nix` (0.02) | No real match | Compass's own shells, images, and builds. The two `devenv.nix` files consume `lib.toolchainTools`. |

The toolchain pins stay local, as the parent record rules. Each repo moves its
own versions on its own cadence (they differ today), each repo's parity gate
checks its own pins, and each repo's Renovate scripts rewrite them.

### Private-consumer files that call or copy the builder

Described by role only. Compass has no copy of these. The private consumer has
three locks that would carry the shared input: its dev-shell `devenv.lock` and
two host-configuration flakes, each with its own `flake.lock`.

| Role | What it does today | Plan |
| --- | --- | --- |
| CI base-image module | Imports its builder copy and installs `bun`, `nodejs`, `moon` | Calls `lib.toolchainTools` with its own pins (T4a) |
| Dev shell | Installs `toolchainTools.bun`, `.nodejs`, `.moon` | Calls `lib.toolchainTools` with its own pins (T4a) |
| CI image tasks | 42 image-task `inputs` entries name the builder file. A README names it too | Drop the deleted path. The dev-shell lock, which these tasks already list, now carries the builder rev (T4a) |
| bun overlay | A plain `_final: prev:` overlay. It sets `pkgs.bun` to the vendored bun on the three pin systems and is a no-op on other systems. Both host flakes apply it | Stays local (one user). Takes the builder as its first argument (T4b) |
| Host moon derivation | An `aarch64-linux`-only hand copy of the builder's moon leg, in one host flake. A runbook names the builder file | Deleted. With compass pins its `drvPath` equals the builder's moon on `aarch64-linux` (`33wnm1fi…-moon-2.5.3.drv`), so the builder's `moon` replaces it with no change (T4b) |

## Approach

This section is the assumed design. Two load-bearing Open Questions can change
it: whether to ship a Nix output at all (Open Question 1), and how consumers
pin it (Open Question 2). It assumes "ship" and "tagged release", the
recommended options.

### One shared output: the toolchain builder

The shared repo gets one Nix output, `lib.toolchainTools`. It is the
vendored-binary builder that both repos carry today, with one change: it takes
the pins as an argument instead of importing `./versions/*.nix`. That import is
the builder's only repo-layout literal, and the pins stay local, so the
argument removes the literal and keeps the pins where they are. The port base
is compass's copy (78 lines, 56 of code), as the parent rules. The private copy's
comments name private hosts, so it is not a port base.

Schematic interface (each leg's body is compass's existing derivation for that
tool, specified in T1's Interfaces):

```nix
# In the shared repo, nix/toolchain-tools.nix
{ pkgs, pins }:
{
  bun = ...;   # pins.bun:  { version; srcs.<system> = { url; hash; }; }
  node = ...;  # pins.node: same shape
  moon = ...;  # pins.moon: same shape
}
```

The shared repo's `flake.nix` (from the parent's T2) exposes it as
`lib.toolchainTools = import ./nix/toolchain-tools.nix;`. A `lib` output whose
value is a function passes `nix flake check` (measured with nix 2.34.8:
"checking flake output 'lib'... all checks passed!").

The shared flake declares a `nixpkgs` input, used only by its `checks`.
`lib.toolchainTools` never reads it: the caller passes `pkgs`. A consumer's
`follows: nixpkgs` then overrides a real input, so the shared repo adds no
second nixpkgs to the consumer's lock.

Two spelling choices close the measured drift:

- The node attr and `pname` are `node`. Compass's pin file, Renovate dep name,
  and parity-gate key already say `node`, and compass's drvs stay unchanged.
  The private consumer's node store path changes name once. See Open Question 3.
- `stdenv.hostPlatform.isLinux` is the one Linux test. It is the newer spelling,
  and it builds the same drv as `stdenv.isLinux` (measured).

Each leg reads only its own pin, and Nix is lazy. So a caller that needs only
bun passes only `pins.bun`. The agent image uses this to keep its trigger lists
as they are.

### Pin cadence: a tagged Nix release

The shared repo's `main` moves on every TS package change, release PR, and
dependency bump. Few of those touch `nix/`. A consumer that tracks `main` would
bump its locks about daily with no builder change. In compass each bump is
costly:

- A root `devenv.lock` change runs the agent-image build, because the env gate
  task lists `/devenv.lock` in its `inputs` and depends on
  `compass-agent-image:build`.
- An `agent-image/devenv.lock` change republishes the agent image, because
  `agent-image/**` is in `release.yml`'s `IMAGE_CLOSURE_PATHS`.

So consumers pin a tag, not `main`. The shared repo's release-please config
gets one more component, `nix`, over the `nix/` tree and `flake.nix`. It cuts
`nix-v<X.Y.Z>` tags. A consumer names the tag in its `devenv.yaml`:

```yaml
# devenv.yaml and agent-image/devenv.yaml
  repo-tools:
    url: github:RigelBuild/repo-tools/nix-v1.0.0
    inputs:
      nixpkgs:
        follows: nixpkgs
```

A lock then moves only when the Nix output is released.

### How compass consumes it

Compass has two devenv scopes, root and `agent-image/`, each with its own lock.
Both get the input. This follows the devenv fork's two-scope pattern: one
source, two locks, two cadences.

Call sites:

| Compass file | Today | After |
| --- | --- | --- |
| `devenv.nix` | `toolchainTools = import ./tools/toolchain/toolchain-tools.nix { inherit pkgs; };` | `toolchainTools = inputs.repo-tools.lib.toolchainTools { inherit pkgs; pins = { bun = import ./tools/toolchain/versions/bun.nix; node = import ./tools/toolchain/versions/node.nix; moon = import ./tools/toolchain/versions/moon.nix; }; };` The binding keeps the name `toolchainTools`, so the parity parser's dotted references (`toolchainTools.bun`) and its fixtures do not change |
| `tools/toolchain/gate-tools.nix` | `toolchainTools = import ./toolchain-tools.nix { inherit pkgs; };` | `toolchainTools = (builtins.getFlake (builtins.flakeRefToString { inherit (n) type owner repo rev narHash; })).lib.toolchainTools { inherit pkgs; pins = { bun = import ./versions/bun.nix; node = import ./versions/node.nix; moon = import ./versions/moon.nix; }; };` with `n = lock.nodes.${lock.nodes.root.inputs.repo-tools}.locked` from the root `devenv.lock` the file already reads. `pkgs` is the file's existing root-lock nixpkgs |
| `agent-image/devenv.nix` | Passes `{ pkgs, compassAgent }` to `toolchain.nix` | Also passes `toolchainTools = inputs.repo-tools.lib.toolchainTools` |
| `agent-image/toolchain.nix` | `bun = (import ../tools/toolchain/toolchain-tools.nix { inherit pkgs; }).bun;` | `bun = (toolchainTools { inherit pkgs; pins = { bun = import ../tools/toolchain/versions/bun.nix; }; }).bun;` where `toolchainTools` is the new argument and `pkgs` is the agent-image scope's nixpkgs it already receives |

`gate-tools.nix` is evaluated with `nix eval -f`, outside devenv, so it has no
`inputs`. It reads the lock node through `nodes.root.inputs`, as
`tools/toolchain/version-guard.ts` does for nixpkgs, because a node key can
differ from its input name (the root lock already has `git-hooks_2`). A bare
`nodes.repo-tools` would read the wrong node once a second, transitive
`repo-tools` node appears. The parity gate would then build from a different
rev than the dev shell and still pass.

`builtins.flakeRefToString` builds the reference and encodes the `narHash`.
Measured: the root lock's go-overlay node resolves through `nodes.root.inputs`,
the reference it builds loads with `builtins.getFlake`, and a `narHash` with
`+` survives `flakeRefToString` then `parseFlakeRef` unchanged. The locked rev
and `narHash` make the fetch reproducible. CI's `nix eval -f` is impure anyway,
so purity is not what this buys.

The pins stay where they are. So `.envrc`'s `watch_file` lines, `release.yml`'s
`IMAGE_CLOSURE_PATHS`, the `versions/bun.nix` inputs of `agent-image/moon.yml`
and `tools/agent-image-env-gate/moon.yml`, `refresh-toolchain-hashes.ts`, and
the bun/node/moon Renovate managers do not change.

### Accepted skew between the two scopes

The two scopes lock the shared input separately, so the root dev shell and the
agent image can build bun from different builder releases for a while. This
skew is accepted. It is the same kind of skew that exists today: the two scopes
already pin different nixpkgs revs (root `c946ff36bf19`, agent image
`6004ea8c229f`), so the image's bun drv already differs from the dev shell's.
What the two share is the fetched content (version and hash), and that stays
shared through `versions/bun.nix`. The parity gate compares the dev shell with
`gate-tools.nix`. Both read the root lock, so the skew never reaches a gate.

`agent-image/toolchain.nix` says today that "the image IS the pin byte for
byte". That is true for the fetched bun, not for the drv. T3 rewords it to say
the image uses the same pinned bun release as the dev shell.

### Renovate: tag bump plus relock

Renovate's `nix` manager is off in compass, and `devenv.lock` is not a
`flake.lock`. So the shared input follows the shape of the devenv fork's
existing rule: a `custom.regex` manager surfaces the pin, and a branch-mode
`postUpgradeTasks` relock rewrites the whole lock. With a tag pin, the manager
reads the tag from `devenv.yaml` instead of a rev from `devenv.lock`.

Two managers, one per scope, in `tools/renovate/config.json5`:

```json5
{
  customType: "regex",
  managerFilePatterns: ["/^devenv\\.yaml$/"],  // agent-image twin: "/^agent-image\\/devenv\\.yaml$/"
  matchStrings: [
    "url: github:RigelBuild/repo-tools/nix-v(?<currentValue>\\d+\\.\\d+\\.\\d+)",
  ],
  depNameTemplate: "RigelBuild/repo-tools",     // twin: "RigelBuild/repo-tools-agent-image"
  packageNameTemplate: "RigelBuild/repo-tools",
  datasourceTemplate: "github-tags",
  extractVersionTemplate: "^nix-v(?<version>.+)$",
}
```

Two package rules, one per scope:

```json5
{
  matchManagers: ["custom.regex"],
  matchDepNames: ["RigelBuild/repo-tools"],   // twin: "RigelBuild/repo-tools-agent-image"
  groupName: "shared tools flake (root)",     // twin: "shared tools flake (agent-image)"
  minimumReleaseAge: null,
  postUpgradeTasks: {
    commands: [
      "bun tools/renovate/refresh-devenv-lock.ts --input repo-tools",
      "bun tools/renovate/refresh-fod-hashes.ts",
    ],
    fileFilters: ["devenv.yaml", "devenv.lock", "agent-image/entrypoint.nix"],  // twin: the agent-image yaml and lock, and the FOD file
    executionMode: "branch",
  },
}
```

- `minimumReleaseAge: null`: first-party pins skip the cooldown (parent OQ5).
- No `schedule`: a tag is cut only when the Nix output changes, so the
  repo-wide daily schedule is enough.
- Its own `groupName` gives it a solo branch, so it owns the one branch-mode
  task slot, and the two scopes keep independent cadences.
- Relock first, FOD refresh last. Both locks are declared triggers of the
  `agent-image/entrypoint.nix` FOD entries in `FOD_ENTRIES`, so the trigger
  coverage test requires the refresh and the FOD file in `fileFilters`. The
  refresh's write is a no-op here: the FOD's builder is nixpkgs' bun, which this
  relock does not move. The run is still paid for, once per release.

### The relock script takes its input name

`tools/renovate/refresh-devenv-lock.ts` hard-codes `DEVENV_INPUT = "devenv"`,
and its shape guard `devenvForkLockedRev` reads only `nodes.devenv.locked.rev`.
Both become input-generic:

- The script requires `--input <name>`, read from `process.argv` as
  `tools/guest-image/pin-agent-image.ts` reads its flags. It parses argv first,
  before the base-ref self-gate. So a missing `--input` exits 1 even on a
  branch where no lock changed. No default: the two devenv-fork rules change to
  `--input devenv` in the same change.
- `devenvForkLockedRev(text)` becomes `lockedInputRev(text, input)`. It resolves
  the node key through `nodes.root.inputs[input]`, reads `locked.rev`, and
  throws a named error unless that is 40-hex.
- Scope selection (`changedDevenvLock`, `DEVENV_LOCK_SCOPES`), the base-ref
  self-gate, the byte-identical check, and the scope-correct devenv CLI from the
  lock's own `devenv` node do not change.

"Input-generic" means the input name only. The script still selects exactly one
scope and still throws if both locks changed. That is correct for every caller:
each rule's manager matches one scope's file, so no bump branch changes both
locks. Do not widen it.

Each distinct command string needs its own anchored entry in
`tools/renovate/bot-config.json5` `allowedCommands`:

```json5
"^bun tools/renovate/refresh-devenv-lock\\.ts --input devenv$",      // replaces the bare entry
"^bun tools/renovate/refresh-devenv-lock\\.ts --input repo-tools$",  // new
```

Two literal entries, not one alternation: `config.test.ts` checks that every
entry is anchored and matches a declared command.

`config.test.ts` pins that change:

| Test | Today | After T2 | After T3 |
| --- | --- | --- | --- |
| "declares eight DISTINCT postUpgrade commands and eight allowlist entries" | 8 / 8 | 8 / 8 | 9 / 9 (rename the test) |
| "permits exactly the eight declared commands" | bare relock string | `--input devenv` string | adds the `--input repo-tools` string |
| "the fod-hash refresh is declared at all six task sites" | 6 | 6 | 8 (rename the test; add sites 7 and 8 to its comment) |
| "the coupled (site, entry) set has its expected shape" | 14 | 14 | 18 (each new site names `devenv.lock` or `agent-image/devenv.lock`, a trigger of both `entrypoint.nix` entries) |
| devenv fork `RELOCK` constant and its allowlist `toEqual` | bare string | `--input devenv` string | unchanged |
| New describe: shared tools flake currency | none | none | a table of the two scopes: manager file pattern anchored to one scope's file, `github-tags` datasource, the regex recovers exactly one version from each real `devenv.yaml`, solo `groupName`, `minimumReleaseAge` null, task order and `fileFilters` |

## Alternatives considered

- **Share the pins too.** One pin set in the shared repo. Rejected: the parent
  record rules that pins stay local, the repos sit at different versions today,
  and each repo's parity gate and hash-refresh script own its own pins.
- **Keep a vendored copy and add a drift gate.** Rejected by the parent.
- **Import the shared file by path.** Fetch the locked source with
  `builtins.fetchTarball` (compass's existing idiom) and import
  `nix/toolchain-tools.nix` directly. It works, but it makes a file path inside
  the shared repo a second interface. `builtins.getFlake` on the same locked
  node goes through the flake output, so the output is the only interface.
- **Track `main` by git-refs digest.** The devenv fork's pattern. Rejected as
  the default: see "Pin cadence" and Open Question 2.
- **Agent-image reads the root lock.** `agent-image/toolchain.nix` could read
  the `repo-tools` node of `../devenv.lock` and skip the second input. That
  saves one manager and one rule. But the root lock would then join
  `IMAGE_CLOSURE_PATHS` and both moon `inputs` lists, so every root-lock change
  (nixpkgs, go-overlay, the devenv fork) would republish the agent image.
- **Ship the bun overlay as a shared output** (`lib.bunOverlay pins`). Rejected
  for now: only the private consumer uses it, and it is three lines once the
  builder is shared.

## Global Constraints

- The shared repo is public. Shared code, tests, comments, and docs carry no
  consumer literal: no compass path, no private-consumer path, host, or name.
- Licence `MIT OR Apache-2.0`. The builder is AGPL-3.0-only in compass, so the
  parent T2's outside-contribution check runs on its history first.
- Pins stay in each consumer. The shared builder never imports a pin file. Its
  pin record shape is `{ version; srcs.<system> = { url; hash; }; }`, the shape
  both repos use today.
- Supported systems: `x86_64-linux`, `aarch64-linux`, `aarch64-darwin`. A leg
  whose pin lacks the evaluating system throws when it is forced.
- `lib.toolchainTools` takes the caller's `pkgs` and never forces the shared
  flake's own `nixpkgs` input.
- Consumers pin the shared flake by tag in their `devenv.yaml` (or by flake
  input in a host flake) and lock it in their lock file. Bumps arrive only by
  Renovate PR, with `minimumReleaseAge: null`.
- A lock node is always found through `nodes.root.inputs.<input>`, never by
  bare node name.
- Each `allowedCommands` entry is anchored `^…$`, one per distinct command.
- A consumer's switch PR deletes its local builder in the same PR.
- A compass cutover must not change any compass drv. Check with the equal-input
  `drvPath` method from the Inventory, on all three systems.
- This record and every compass PR it spawns name the private consumer only by
  role.

## Plan

Order: T2 can start now. T1 is blocked on RIG-4469 (the repo name), on the
parent's T1 and T2 (the repo and its `flake.nix`), and on Open Questions 1 and
2. T3 needs a T1 release and T2 merged in compass. T4a and T4b need a T1
release only, and run in parallel with T3. If Open Question 1 rules "reconcile
by hand", T1, T3, and T4 are replaced by one task, described there, and T2 is
dropped.

### T1 — Shared `lib.toolchainTools`

Lands in: the shared repo. Adds `nix/toolchain-tools.nix`, the `nixpkgs` input,
the `lib.toolchainTools` output and `checks` in `flake.nix`, a check under
`nix/tests/`, a README section, and (if Open Question 2 rules "tagged release")
a `nix` component in the release-please config.

Interfaces:

- `lib.toolchainTools :: { pkgs, pins } -> { bun, node, moon }`. Each leg reads
  `pins.<tool>.version` and `pins.<tool>.srcs.${pkgs.stdenv.hostPlatform.system}`
  (`{ url; hash; }`, passed to `pkgs.fetchurl`). `bun` installs `bin/bun` and a
  `bin/bunx` symlink, unzips its source, and runs `autoPatchelfHook` on Linux.
  `node` has `pname = "node"`, copies `bin`, `lib`, `include`, and `share`, and
  runs `autoPatchelfHook` on Linux. `moon` installs `bin/moon`.
- Tagged release (assumed): release-please component `nix`, tag
  `nix-v<X.Y.Z>`, covering `nix/**` and `flake.nix`. Consumers reference
  `github:RigelBuild/repo-tools/nix-v<X.Y.Z>`.
- Git-refs on `main` (if ruled): no release component. Consumers reference
  `github:RigelBuild/repo-tools`.
- Port base: compass's `tools/toolchain/toolchain-tools.nix`, with its comments
  rewritten to name no consumer path.

Acceptance:

- `nix flake check --all-systems` passes.
- The check builds the outputs from a fixture pin set with synthetic URLs and
  hashes, then asserts at eval time each leg's `pname`, `version`, and `src.url`
  for the evaluating system.
- Missing system: a fixture leg whose `srcs` lacks the evaluating system throws
  on access, and the other legs still evaluate.
- Partial pins: `(lib.toolchainTools { inherit pkgs; pins = { bun = fixture; }; }).bun`
  evaluates.
- With compass's pins and compass's root-lock nixpkgs, `bun`, `node`, and `moon`
  have the same `drvPath` as compass's builder at the T1 merge, on all three
  systems. Run this once by hand and record the result in the PR body.
- No comment names a consumer path.
- Tagged release: the first `nix-v1.0.0` tag exists.

### T2 — `refresh-devenv-lock.ts --input <name>`

Lands in: compass. `tools/renovate/refresh-devenv-lock.ts`,
`refresh-devenv-lock.core.ts`, both of their test files, the two devenv-fork
rules in `tools/renovate/config.json5`, `tools/renovate/bot-config.json5`, and
`tools/renovate/config.test.ts`.

Interfaces:

- CLI: `bun tools/renovate/refresh-devenv-lock.ts --input <name>`. Exit 0 means
  relocked or no-op. Exit 1 means a step failed, including a missing or empty
  `--input`. Argv is parsed before the self-gate.
- `export function inputArg(argv: readonly string[]): string` in the core file.
  It returns the value after `--input` and throws a usage error when it is
  missing, empty, or given twice.
- `export function lockedInputRev(devenvLockText: string, input: string): string`
  replaces `devenvForkLockedRev`. It resolves `nodes.root.inputs[input]` to a
  node key, reads `nodes[key].locked.rev`, and throws a named error if the input
  or a 40-hex rev is absent.
- The relock runs `nix run <src> -- update <input>` in the scope's directory.

Acceptance:

- The existing relock tests pass when they invoke `--input devenv`.
- New tests: a run with no `--input` exits 1 and runs no nix, on a branch with
  no lock change; an `--input` that is not a root input exits 1 before any
  relock; `lockedInputRev` resolves an input whose node key differs from its
  name.
- Both devenv-fork rules declare
  `bun tools/renovate/refresh-devenv-lock.ts --input devenv`. The allowlist
  entry is `^bun tools/renovate/refresh-devenv-lock\\.ts --input devenv$`.
  `bun test tools/renovate` passes with the T2 column of the pins table.

### T3 — Compass cutover

Lands in: compass. `devenv.yaml`, `devenv.lock`, `agent-image/devenv.yaml`, and
`agent-image/devenv.lock` (add the `repo-tools` input, then lock it with
`devenv update repo-tools` in each scope); `devenv.nix`;
`agent-image/devenv.nix`; `agent-image/toolchain.nix`;
`tools/toolchain/gate-tools.nix`; the comments in `.github/workflows/ci.yml`
that name the builder; `tools/renovate/config.json5`,
`tools/renovate/bot-config.json5`, and `tools/renovate/config.test.ts`. Deletes
`tools/toolchain/toolchain-tools.nix`.

Interfaces:

- Consumes T1's `lib.toolchainTools` and T2's `--input` flag.
- `agent-image/toolchain.nix` changes its signature to
  `{ pkgs, compassAgent, toolchainTools }`.
- The `devenv.nix` binding keeps the name `toolchainTools`.
- Tagged release (assumed): `url: github:RigelBuild/repo-tools/nix-v<X.Y.Z>`;
  managers match `devenv.yaml` and `agent-image/devenv.yaml` with the
  `github-tags` datasource; `fileFilters` include the scope's yaml and lock.
- Git-refs on `main` (if ruled): `url: github:RigelBuild/repo-tools`; managers
  match the two locks with the anchor
  `"repo": "repo-tools",\s*"rev": "(?<currentDigest>[a-f0-9]{40})"`,
  `currentValueTemplate: "main"`, the `git-refs` datasource, and the schedule
  the ruling picks; `fileFilters` hold the lock only.

Acceptance:

- With the same `nixpkgs` and `go-overlay` lock nodes before and after,
  `bun`, `node`, and `moon` from the dev shell and from
  `nix eval --json -f tools/toolchain/gate-tools.nix langs` have the same
  `drvPath` as before, on all three systems.
- `bun tools/toolchain/parity.ts` passes.
- `compass-agent-image:build` and the `tools/agent-image-env-gate` check pass,
  and the image's bun store path is unchanged.
- `gate-tools.nix` finds the node through `nodes.root.inputs`.
- The "byte for byte" comment in `agent-image/toolchain.nix` is reworded as in
  "Accepted skew between the two scopes".
- `bun test tools/renovate` passes with the T3 column of the pins table.
- No file outside `docs/designs/` names `toolchain-tools.nix`.

### T4a — Private consumer: dev shell and CI images

Lands in: the private consumer. Described here by role only.

- Its dev-shell `devenv.yaml` takes the shared input; its `devenv.lock` locks it.
- Its dev shell and CI base-image module call `lib.toolchainTools` with its own
  pins. Their `nodejs` references become `node`.
- Its builder copy is deleted. The image tasks' `inputs` entries and the README
  line that name it are removed.
- Its Renovate config tracks the input the way compass does (per Open Question
  2), with `minimumReleaseAge: null`, and relocks with its own lock tooling.

Interfaces: consumes T1's `lib.toolchainTools`.

Acceptance:

- With equal inputs, `bun` and `moon` keep their `drvPath` on all three systems.
- `node` changes only by name (`nodejs-<v>` to `node-<v>`). Each CI step image
  that installs node republishes once. That is expected.
- No reference to the deleted builder path remains.

### T4b — Private consumer: hosts, overlay, host moon copy

Lands in: the private consumer. Described here by role only.

- Both host-configuration flakes take the shared repo as a flake input. Each
  `flake.lock` locks it.
- The bun pin passed below is the private consumer's own bun pin file,
  imported unchanged. Its value is an attrset with `version` (a string such as
  `"1.4.2"`) and `srcs`, which maps each of `x86_64-linux`, `aarch64-linux`,
  and `aarch64-darwin` to `{ url; hash; }`: the release asset URL and its SRI
  hash. The moon pin is its own moon pin file, with the same shape. This is the
  shape T1's `pins.<tool>` expects.
- The bun overlay changes from `_final: prev:` to
  `toolchainTools: _final: prev:`. It stays a plain overlay and keeps its
  supported-system check. On a supported system it returns
  `{ bun = (toolchainTools { pkgs = prev; pins = { bun = import ./versions/bun.nix; }; }).bun; }`,
  where the import path is schematic: the overlay imports the bun pin file
  beside it, as it does today. It still passes `prev`, never `final`, so it
  does not recurse. Each host flake applies it as
  `(import <overlay path> inputs.repo-tools.lib.toolchainTools)`; the path is
  schematic and stays the one each flake uses today.
- The host moon derivation is deleted. The host module that holds it already
  receives the host flake's `inputs` through `specialArgs`. It uses
  `moon = (inputs.repo-tools.lib.toolchainTools { inherit pkgs; pins = { moon = moonPin; }; }).moon;`,
  where `moonPin` is the binding that already imports its moon pin file. The
  runbook line that names the builder is updated.
- Rev policy across the three locks (dev shell plus two host flakes): a
  same-rev guard in its Renovate config tests, modelled on its existing guard
  that keeps one other overlay input on one rev across its dev-shell lock and a
  host lock. All three locks pin the same shared rev, so CI-image bun and host
  bun always come from one builder release.

Interfaces: consumes T1's `lib.toolchainTools`. Produces the overlay
`toolchainTools: _final: prev:`. On the three pin systems it returns an
attribute set whose `bun` is the builder's bun derivation; elsewhere it returns `{ }`.

Acceptance:

- Host `pkgs.bun` and the host moon keep their `drvPath`s.
- The overlay is still a no-op on systems outside the three pin systems.
- The same-rev guard fails when one of the three locks names a different rev.

### Out of scope

Moving the pins; the Renovate hash-refresh scripts (a later record, per the
parent); a shared Renovate preset; the go toolchain; compass's env helpers and
FOD vehicles; the private consumer's other Nix modules.

## Tasks

- [ ] T1 — Ship `lib.toolchainTools` in the shared repo with its check and, if
  ruled, its `nix` release component.
- [ ] T2 — Make `refresh-devenv-lock.ts` take `--input <name>`.
- [ ] T3 — Cut compass over to the shared builder and delete its copy.
- [ ] T4a — Cut the private consumer's dev shell and CI images over.
- [ ] T4b — Cut the private consumer's hosts, bun overlay, and host moon copy
  over.

## Open Questions

1. **Ship the Nix output or reconcile by hand (load-bearing).** The parent
   ruled that Nix tooling is shared before it was measured. The measurement
   found one shared file: a 78-line builder (56 lines of code) with 6 lines of
   code drift. Nothing else matches above 0.29.
   - Option A, ship it: one source of truth for the builder. The wiring cost,
     counted from this record: in compass, two `devenv.yaml` inputs and locks,
     two Renovate managers and two rules, a CLI change to
     `refresh-devenv-lock.ts`, two more allowlist entries, four changed
     `config.test.ts` pins and a new describe, and a `getFlake` path in
     `gate-tools.nix`. In the shared repo, a Nix release component and checks.
     In the private consumer, three locks and their Renovate rules, a same-rev
     guard, an overlay signature change, and 42 image-task inputs. After that,
     each builder change is one release plus up to five relock PRs (two in
     compass, three in the private consumer).
   - Option B, reconcile by hand: one PR in the private consumer renames
     `nodejs` to `node` and uses `stdenv.hostPlatform.isLinux`; one compass PR
     uses `stdenv.hostPlatform.isLinux`. Each repo keeps its own copy. The
     parent's "no vendored copy" constraint does not apply, because nothing
     switches. The accepted risk: the pin hashes protect only the fetched
     release assets. They do not protect the builder logic: which asset a
     system selects, how it is unpacked, which files are installed, the
     `bunx` link, and the Linux-only `autoPatchelfHook`. A fix to that logic in
     one repo can be missed in the other, and nothing would flag it. The
     result can be a binary that runs in one repo and fails in the other, or
     one that silently differs in which files it installs. The measured history
     bounds this risk: 6 lines of code drift over the file's life, two of them
     a rename and four a spelling. Both repos' CI runs their own builder on
     every pin bump, so a fix that breaks a run shows up there, but a fix that
     changes behaviour without failing does not.
   - Recommendation: Option B still holds with that risk stated. The builder's
     logic has barely changed, and Option A adds about ten wiring pieces and a
     release lane to remove the risk. Option A is right if more Nix code is
     expected to become shared later, or if Matt weights silent logic drift
     higher than the wiring cost. In that case, take Option A with Open
     Question 2's tagged release, so the wiring cost is paid once and bumps
     happen only on change. This record is written as Option A so it is ready
     if Matt picks it. If he picks Option B, it collapses to the two reconcile
     PRs above, and T1–T4 are not filed.
2. **Pin cadence (load-bearing).** How consumers pin the shared flake. This
   changes T1 (whether the shared repo needs a Nix release lane) and T3 (which
   file the managers match, the datasource, the regex, and `fileFilters`).
   - Option A, git-refs on `main`: the devenv fork's pattern. No release lane.
     The shared repo's `main` moves on every TS change, so both compass locks
     bump about daily with no builder change. Each root bump runs the heavy
     agent-image build, and each agent-image bump republishes the image.
   - Option B, tagged Nix release (recommended): a release-please `nix`
     component cuts `nix-v<X.Y.Z>` tags; a `github-tags` regex manager on
     `devenv.yaml` tracks them, then the same `--input` relock runs. Locks move
     only when the Nix output changes. The cost is one more release-please
     component.
   - Option C, git-refs on a weekly or monthly schedule: cheaper than A, no
     release lane, but each bump still rebuilds and republishes with no
     builder change.
3. **Node attr name (not load-bearing).** `node` (recommended) or `nodejs`.
   With `node`, compass's drvs and names stay the same, and the private
   consumer's node store path changes name once. With `nodejs`, compass renames
   its pin key, its parity key, and its call sites, and its node drv changes.
4. **Repo name (not load-bearing for this record).** RIG-4469 is still open. It
   blocks T1, and the name fixes the input name, dep names, regex, and
   allowlist string in T3 and T4. A different pick from the working name
   `RigelBuild/repo-tools` is a mechanical rename.
