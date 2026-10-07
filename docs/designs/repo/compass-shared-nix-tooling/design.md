# Reconcile the shared Nix toolchain builder (RIG-4540)

## Problem / Intent

The shared tools record (repo/compass-shared-tools-repo, T10) asked which Nix
code compass and the private consumer both carry, so the shared public repo
could export it. This record
measures it. The measurement found one shared file, the vendored-binary
toolchain builder, with 6 lines of code drift. Matt ruled to reconcile that
drift by hand rather than ship a flake output (RIG-4548, see Resolved
decisions). This record plans the two reconcile changes and lists what stays
different, and why.

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

| Compass path | Private copy | Drift | Consumer literal | Reconcile |
| --- | --- | --- | --- | --- |
| `tools/toolchain/toolchain-tools.nix` | Yes: the builder in its CI toolchain tree (score 0.78) | Structural. Text 17+/18−; code 6+/6−. Two changes: the node attr and `pname` are `node` in compass and `nodejs` in the private copy (this changes the node drv); four lines use `stdenv.isLinux` in compass and `stdenv.hostPlatform.isLinux` in the private copy (no drv change). The comments differ too: the private copy's describe its own image and hosts | None in code. Both copies read pins with `import ./versions/<tool>.nix`. Each copy's header comment names its own pin directory | R1 and R2 |
| `tools/toolchain/versions/bun.nix` | Yes: its bun pin (0.56) | Version skew: 1.4.0 here, 1.4.2 there. Text 5+/5−; code 4+/4− (version and three hashes). Same structure after version and hashes are masked. Comments differ only in the header line naming the file's own path | None | Not reconciled (see below) |
| `tools/toolchain/versions/node.nix` | Yes: its node pin (0.56) | Version skew: 24.20.0 here, 24.21.0 there. Text 5+/5−; code 4+/4−. Same structure. Comments differ only in the header path line | None | Not reconciled (see below) |
| `tools/toolchain/versions/moon.nix` | Yes: its moon pin (0.56) | Version skew: 2.5.3 here, 2.5.4 there. Text 8+/6−; code 4+/4−. Same structure. Compass has a header path line; the private copy instead gives its reason for the 2.x pin, naming one of its own components | None | Not reconciled (see below) |

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

These scored low, or match only on generic lines. None is shared.

| Compass path | Best match | Why it stays local |
| --- | --- | --- |
| `tools/toolchain/versions/go.nix`, `tools/toolchain/versions/go-analysis.nix` | 0.00, 0.14 | The private consumer has no go pin file. Its go comes straight from its go-overlay input. |
| `agent-image/toolchain.nix` | 0.29, its CI base-image module | The 22 shared lines are generic: `let`, `in`, package names, and the `nixConf = pkgs.writeTextDir` header. The `nix.conf` bodies differ (compass: single-user nix with no sandbox and a pinned CA bundle; private: flakes plus `accept-flake-config`). They are different images. |
| `tools/toolchain/gate-tools.nix`, `tools/toolchain/go-analysis.nix` | 0.11, 0.10 | Compass's parity-gate plumbing. |
| `tools/toolchain/chromium-e2e-env.nix` (0.15), `gtk-e2e-env.nix` (0.21), `gtk-closure.nix` (0.16), `secretspec-env.nix` (0.23), `skopeo-nix2container-env.nix` (0.22), `microvm-vmm-env.nix` (0.25) | Unrelated host or CI modules | Compass-only test and image environments. They share only the lock-reading header. |
| `tools/renovate/agent-image-fod-vehicle.nix` (0.26), `tools/renovate/ui-fod-vehicle.nix` (0.23) | Unrelated host modules | Compass-only FOD hash vehicles. |
| `devenv.nix` (0.13), `agent-image/devenv.nix` (0.17), `flake.nix` (0.09), `agent-image/entrypoint.nix` (0.10), `apps/ui/dist.nix` (0.11), `guest-image/default.nix` (0.04), `guest-image/assembly-tests.nix` (0.02) | No real match | Compass's own shells, images, and builds. |

### Private-consumer files that call or copy the builder

Described by role only. Compass has no copy of these.

| Role | What it does today | After the reconcile |
| --- | --- | --- |
| CI base-image module | Imports its builder copy and installs `bun`, `nodejs`, `moon` | Inherits and installs `node` instead of `nodejs` (R2) |
| Dev shell | Installs `toolchainTools.bun`, `.nodejs`, `.moon` | Installs `toolchainTools.node` instead of `.nodejs` (R2) |
| bun overlay | Sets `pkgs.bun` to the builder's `bun` on the three pin systems; a no-op elsewhere | Unchanged: it reads only `bun` |
| Host moon derivation | An `aarch64-linux`-only hand copy of the builder's moon leg. With compass pins its `drvPath` equals the builder's moon (`33wnm1fi…-moon-2.5.3.drv`) | Unchanged (see Out of scope) |

## Approach

Each repo keeps its own copy of the builder, and the two copies become the
same file except for one comment line. Two changes do it:

- **R1, compass:** the builder's four `stdenv.isLinux` tests become
  `stdenv.hostPlatform.isLinux`, the private copy's spelling and the newer
  one. Measured: no drv changes.
- **R2, private consumer:** it replaces its builder with compass's post-R1
  builder, verbatim, except the one header comment line that names the pin
  directory, which names its own. That brings in compass's `node` attr and
  `pname`, and compass's comments, which name no host. Its two `nodejs` callers
  change to `node`: the CI base-image module's `inherit` and install list, and
  the dev-shell package list.

R1 lands first, so R2 copies a builder that already uses
`stdenv.hostPlatform.isLinux`.

After both, `bun` and `moon` keep every `drvPath` in both repos. Compass's
`node` keeps its `drvPath`. The private consumer's node store path changes
name once, from `nodejs-<v>` to `node-<v>`, so every private CI step image that
installs node republishes once. Nothing in the private consumer keys on the
`nodejs` name beyond the two callers: its Renovate config already names the dep
`node`, and no parity gate checks the node attr.

### Accepted risk: two copies

There is no shared source and no drift check, so the copies can drift again.
The pin hashes do not prevent that. They pin the downloaded release files, not
the builder logic: which file a system selects, how it is unpacked, which files
are installed, the `bunx` link, and the Linux-only `autoPatchelfHook`. A fix to
that logic in one repo can be missed in the other, and nothing would flag it.
A fix that breaks a build shows up in that repo's CI on its next pin bump. A
fix that changes behaviour without failing does not. The measured history
bounds this risk: 6 lines of code drift over the file's life, two of them a
name and four a spelling.

## Not reconciled

- **Pin versions.** Each repo's Renovate bumps its own pin files on its own
  schedule, and each repo's parity gate checks its own pins. So the versions
  stay per-repo. Today compass lags: bun 1.4.0 against 1.4.2, node 24.20.0
  against 24.21.0, moon 2.5.3 against 2.5.4. The skew closes when compass's
  pending Renovate bumps merge, and it can reopen at any time. The pin file
  structure already matches.
- **Comments that name a repo's own paths.** These stay repo-specific: the
  builder's header line naming its pin directory, and each pin file's header
  line naming its own path. The private moon pin's comment gives a reason that
  names one of its own components, so it stays as it is. All other builder
  comments match after R2.
- **The toolchain hash-refresh script.** `tools/renovate/refresh-toolchain-hashes.ts`
  has a private copy that has diverged: text 34+/216− (207 lines here, 389
  there). The private copy also refreshes a Rust toolchain channel pin and
  checks a lock rev against it. It is TypeScript, not Nix, so this record does
  not reconcile it. It belongs to the shared tools record, which lists moving
  the Renovate upgrade scripts as later work.

## Alternatives considered

- **Ship the builder as a shared flake output** (`lib.toolchainTools { pkgs,
  pins }` in the shared repo, pinned by each consumer's lock and bumped by a
  Renovate rule and relock). Declined by Matt (RIG-4548): the shared surface is
  one 78-line file with 6 lines of drift, and the wiring would have been about
  ten pieces across both repos plus a Nix release lane.
- **Share the pins too.** Rejected: each repo's Renovate and parity gate own
  its own pins, and the repos sit at different versions.
- **Keep both copies and add a drift check.** Rejected by the parent record.
- **Adopt the private copy in compass.** Its comments name private hosts and
  paths, which cannot enter a public repo, and its `nodejs` name would change
  compass's node drv and its pin, parity, and Renovate names.

## Global Constraints

- Compass is public. This record and the R1 change name the private consumer
  only by role, with no private path, host, or name.
- R1 changes no compass drv. Check it with the equal-input `drvPath` method
  from the Inventory, on all three systems.
- R2 changes no private `bun` or `moon` drv. The only drv change is the node
  name.
- Pin files are not edited by either change.
- No new shared code, flake input, Renovate rule, or ledger row. The PR
  carries `Ledger-impact: none`.

## Plan

Order: R1, then R2. R2 copies compass's builder as it is after R1.

### R1 — Compass: one `isLinux` spelling

Lands in: compass. `tools/toolchain/toolchain-tools.nix` only.

Change: the four `lib.optionals stdenv.isLinux` tests (two in `bun`, two in
`node`) become `lib.optionals stdenv.hostPlatform.isLinux`. The header's
second line becomes "and the CI toolchain import this one module", which R2
copies too. No other line changes.

Acceptance:

- With compass's root-lock nixpkgs and compass's pins, `bun`, `node`, and
  `moon` have the same `drvPath` before and after, on `x86_64-linux`,
  `aarch64-linux`, and `aarch64-darwin`.
- `bun tools/toolchain/parity.ts` passes.
- The file has no `stdenv.isLinux` left.

### R2 — Private consumer: adopt compass's builder

Lands in: the private consumer. Described here by role only.

Change:

- Its builder file becomes compass's post-R1 builder, verbatim, except the one
  header comment line that names the pin directory, which names its own pin
  directory.
- Its CI base-image module inherits and installs `node` instead of `nodejs`.
- Its dev shell lists `toolchainTools.node` instead of `toolchainTools.nodejs`.

Acceptance:

- `diff` between its builder and compass's builder shows exactly one changed
  line: the header line that names the pin directory.
- With equal inputs, `bun` and `moon` keep their `drvPath` on all three
  systems. The bun overlay's `pkgs.bun` keeps its `drvPath`.
- `node` changes only by name (`nodejs-<v>` to `node-<v>`). Each CI step image
  that installs node republishes once. That is expected.
- No `nodejs` reference to the builder's output remains.

### Out of scope

Pin versions and the hash-refresh script (see Not reconciled); the private
consumer's host moon copy, which is drv-identical to the builder's moon and is
a private cleanup; the go toolchain; compass's env helpers and FOD vehicles.

## Tasks

- [ ] R1 — Use `stdenv.hostPlatform.isLinux` in compass's builder.
- [ ] R2 — Adopt compass's builder in the private consumer and rename its
  `nodejs` callers to `node`.

## Resolved decisions

1. **Ship the Nix output or reconcile by hand.** Ruled by Matt on 2026-10-05
   (RIG-4548, option 1B): reconcile by hand so both repos use one setup, and
   list anything that cannot be reconciled. This record is that plan.
2. **Pin cadence for a shared flake.** Moot on 2026-10-05: no shared flake
   ships. Matt leaned toward tracking the shared repo's `main`.
3. **Node attr name.** `node`, through R2: compass's name is adopted, so
   compass's drvs and its pin, parity, and Renovate names stay the same.
