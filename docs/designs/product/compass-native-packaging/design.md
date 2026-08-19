# Compass native app — packaging + CI baseline (T6)

Status: Draft
Linear: RIG-1687

Details **T6** of the frozen native-app record
([compass-native-app §Plan T6](../compass-native-app/design.md#t6--packaging--ci-baseline),
`compass-native-app/design.md:520-534`): a reproducible Linux build of the app
bundle embedding the built UI and the `compass-stack`/`compass-server`/
`compass-runner` binaries, moon-registered `build`/`ci` lanes for it, and a
version-stamp path. macOS packaging, signed installers, and auto-update are
deferred per the epic's scope boundary (`compass-native-app/design.md:242-248`:
"Deferred to follow-up issues: … signed installers with auto-update"). The agent
image is NOT embedded — `compass-stack` `podman pull`s it from GHCR at first run
(DL-112), and the GHCR publish lane already exists
(`agent-image/moon.yml:5-8` names `agent-image/publish.sh` as "the publish lane"
whose derivation the moon build proves).

## Problem / Intent

The native app exists only as a developer build: `go build -tags gtk3` under
`go/` with the GTK closure's `PKG_CONFIG_PATH`, a hand-started `compass-stack`,
and a `vite`-built dist resolved by flag. There is no installable artifact, no
CI lane that builds one, and no version stamp on the shell (`go/cmd/compass-app`
has no `version` variable at all, unlike its four sibling commands). T6 turns
the working T4/T5 shell into ONE reproducible, versioned Linux bundle that CI
builds from a clean checkout and that launches on the dev box and passes the T4
smoke.

## Approach

One new moon project — **`compass-app-bundle`** (`app-bundle/`) — owns the
bundle: a build script that realizes the already-landed devenv.lock-pinned
GTK/toolchain closure, cgo-links the gtk3-tagged shell with the nix cc-wrapper,
pure-Go-builds the sidecar binaries, stages a fixed **sidecar directory
layout**, stamps one version string into every binary via the existing
`-ldflags -X main.version` convention, and emits a **versioned tarball**. It is
registered in `.moon/workspace.yml` so moon's affected-detection gates it per-PR
and the main/nightly full sweep builds it unconditionally — the exact precedent
`compass-agent-image` set for a heavy nix-backed build
(`agent-image/moon.yml:10-14`; `.moon/workspace.yml:29-33`).

### A0 — The as-built facts the bundle composes with

All re-verified in the current tree (main `f29e8049`) this session:

- **The shell is gtk3-tagged Wails v3.** `go/cmd/compass-app/main.go:1`:
  `//go:build unix && gtk3`; the dependency is
  `github.com/wailsapp/wails/v3 v3.0.0-beta.0` (`go/go.mod:29`). The untagged
  module build compiles a stub instead (`main_nogtk3.go:1`:
  `//go:build unix && !gtk3`; `:24-28` exits 1 with "rebuild with -tags gtk3").
- **The shell already resolves a packaged sidecar layout.** The UI dist is NOT
  embeddable — `main.go:14-16`: "the dist lives at repo apps/ui/dist, OUTSIDE
  this Go package's directory subtree, so //go:embed cannot reach it (embed
  forbids '..' patterns)" — so it is served from a runtime-resolved directory
  defaulting to "a 'dist' directory beside the executable (where a packaged
  build stages apps/ui dist)" (`main.go:313-321`). Likewise `resolveStackBin`
  falls back to "a compass-stack sibling of the running executable (where a
  packaged build stages it, mirroring resolveAssetsDir's beside-the-executable
  pattern)" (`embedded.go:342-359`).
- **The stack resolves its children by bare name on PATH.**
  `go/internal/stack/adapters/process.go:31-33`: "componentBinary resolves a
  Component to the binary name to look up on PATH. The binary location is a
  deployment concern"; the names are `compass-postgres`/`compass-server`/
  `compass-runner` (`process.go:36-41`), resolved via `exec.LookPath`
  (`process.go:58`). The app spawns `compass-stack` via `exec.CommandContext`
  with the parent environment (no `cmd.Env` override, `embedded.go:225-228`) —
  so today a packaged `compass-stack` sibling would still LookPath its children
  on the *user's* PATH, not in the bundle. A1 closes this.
- **The private Postgres shells out to real postgres tooling.**
  `go/cmd/compass-postgres/main.go:187` (`exec.LookPath("postgres")`), `:282`
  (`initdb`), `:317` (`createdb`). On the dev box these come from devenv
  (`devenv.nix:99-113`: "those binaries must be on PATH wherever the e2e suite
  runs … Bare `postgresql`, not a version-suffixed attr, for strict parity" —
  postgresql-18.4 at the current pin). A packaged app on a box without a
  postgres install has nothing to LookPath — Open Question 1.
- **The version-stamp convention exists on four of five stack commands.**
  `compass-server/main.go:25-28`: "override at build time with
  `-ldflags \"-X main.version=<v>\"`" / `var version = "0.1.0"`; same on
  `compass-runner/main.go:28-30`, `compass-postgres/main.go:40-42`, and
  `compass-stack/main.go:35-38` — where it is load-bearing beyond display: "It
  feeds Deps.ExpectedVersion, so the attach mismatch check compares a live
  server's version against this build's", and a mismatch is a hard
  restart-the-stack error (`compass-stack/main.go:253-255`). `compass-app` has
  no version variable (verified by grep over `go/cmd/compass-app/` this
  session). Consequence: every binary in one bundle MUST carry the SAME stamp,
  or the app's own stack refuses to attach across an app restart.
- **The GTK build environment is landed, shared, and pinned.**
  `tools/toolchain/gtk-closure.nix:16-32` is the one 13-package GTK3/WebKitGTK
  set; `tools/toolchain/gtk-e2e-env.nix:28-38` re-pins it to the devenv.lock
  nixpkgs and exports three `nix build`-realizable outputs: `bin` (xvfb-run +
  pkg-config, `:41-46`), `pkgConfig` (realized `.pc` closure, `:59-66`), and
  `cc` (the nixpkgs cc-wrapper, `:68-81`) — the cc-wrapper "rpaths the store
  lib dirs — so the test binary is self-contained (right libc + every
  GTK/WebKit .so at runtime) regardless of the runner's system libc"
  (`gtk-e2e-env.nix:74-77`). The M4 CI step already drives exactly this dance
  (`.github/workflows/ci.yml:364-412`).
- **CI shape.** One `gates` job runs `moon ci :ci` (affected) on PRs
  (`ci.yml:220-231`) and `moon run :ci` (full) on main pushes + nightly
  (`ci.yml:233-241`); a YAML project matrix is explicitly rejected because
  ".moon/workspace.yml already owns" the project list (`ci.yml:6-18`); the
  single required check is the rollup `CI` job, `needs: [gates, dogfood-e2e]`
  (`ci.yml:879-913`). The per-PR moon battery is "deliberately GTK-free"
  (`gtk-e2e-env.nix:3-4`) — only the dedicated, affected-guarded M4 e2e step
  pays the closure.
- **The UI dist is a moon output.** `apps/ui/moon.yml:19-23`: `build` =
  `bunx vite build`, `outputs: ['dist']`.

### A1 — Bundle format: a versioned tarball of a nix-closure-backed bundle dir (fork 1, resolved)

The artifact is `compass-app-<version>-linux-amd64.tar.gz` containing the A2
layout, built against the devenv.lock-pinned nix closure so every ELF inside is
store-rpathed and self-contained on any box whose nix store holds the (realized
at build) closure — which the dev/dogfood boxes and CI runners do by
construction, because the bundle build itself realizes it. A `compass.desktop`
(`Exec=compass-app`) ships inside as a *template* for the A5 installer to
consume — inert as shipped, since the tarball unpacks to an arbitrary prefix
and nothing puts `bin/` on PATH or installs the file into the host's
`share/applications`; activating it means an installer step (symlink `bin/`
into `~/.local/bin` + `desktop-file-install`).

Why this wins the fork:

- **Reproducibility** — every input is already pinned: Go via proto/gate-tools,
  the GTK closure + cc via devenv.lock (`gtk-e2e-env.nix:9-13`: "Pins nixpkgs to
  the SAME devenv.lock revision the dev shell and gate-tools.nix resolve"),
  `-trimpath` on the Go builds. No new pin surface.
- **CI cost** — the heavy WebKitGTK closure is exactly the one M4 already
  realizes in CI; the nix substituter cache makes repeat realizations cheap,
  and A4 gates the build per-PR anyway.
- **Runs where T6's gate runs** — the frozen gate is "the artifact launches on
  the dev box and passes the T4 smoke" (`compass-native-app/design.md:533-534`).
  Store-rpathed binaries launch there without any library staging, exactly as
  the M4 cc/rpath work proved on a non-NixOS runner (`gtk-e2e-env.nix:68-77`).
- **Does not foreclose GA** — the tarball's *content* is a format-agnostic
  bundle directory. AppImage, `nix bundle`, or a signed installer (all A5
  follow-ups) each wrap such a directory; adopting one later changes the final
  packaging step, not the layout, the stamp path, or the CI lane. That
  survival is uneven for the *binaries*, though: a wrapper that embeds the
  store subtree at its absolute paths keeps the ELFs as-is (rpaths intact),
  whereas one that does not — a conventional AppImage or a plain installer —
  must patchelf-rewrite their rpaths and replace OQ1(a)'s store symlinks. The
  layout, stamp, and CI lane survive either path; the binaries survive only
  the store-embedding one.

The honest limit, stated: a box with **no nix store** cannot run this tarball
(the rpaths dangle). That is a distribution problem T6 does not own — A5 defers
end-user installers — and the record names it as the seam the A5 follow-up
(AppImage or `nix bundle` over this same bundle dir) fills. See §Alternatives.

### A2 — Binary embedding: sidecar bin/ layout + PATH threading (fork 2, resolved)

The bundle stages sidecars beside the shell — the layout the shipped code
already resolves (A0):

```text
compass-app-<version>-linux-amd64/
  bin/
    compass-app            # gtk3-tagged Wails shell, CGO, store-rpathed
    compass-stack          # pure Go, CGO_ENABLED=0
    compass-server         # pure Go, CGO_ENABLED=0
    compass-runner         # pure Go, CGO_ENABLED=0
    compass-postgres       # pure Go, CGO_ENABLED=0
    postgres initdb createdb  # store symlinks, pinned postgresql 18.4 (OQ1 (a), DL-217)
    dist/                  # apps/ui vite build output (compass-ui:build)
  share/applications/compass.desktop
  LICENSE                  # AGPL-3.0-only
```

The set is *four* sidecars, not the three the parent record froze
(server/runner/stack): `compass-postgres` is a real sidecar the stack
LookPaths by bare name (`process.go:36-41`), so widening it to four is
intentional and grounded, not scope drift.

`resolveStackBin` finds `bin/compass-stack` as the executable's sibling
(`embedded.go:342-359`) and `resolveAssetsDir` finds `bin/dist`
(`main.go:313-321`) with zero code change to *assets*. But `resolveStackBin`
as shipped puts PATH before the sibling (flag → `$COMPASS_STACK_BIN` →
`exec.LookPath("compass-stack")` on PATH → sibling of the executable), so an
ambient `compass-stack` on PATH beats the bundle's own sibling — a
mixed-version attach that hard-fails (`compass-stack/main.go:253-257`).
**T6.2** reorders the precedence to flag → `$COMPASS_STACK_BIN` → sibling →
PATH (moving the sibling block above the PATH `LookPath` block). A bare
dev-box `go build ./cmd/compass-app` stages no sibling, so PATH still wins
there (dev-box behavior unchanged); only a packaged build has a sibling,
which is exactly where in-bundle resolution must win. A second gap remains
(A0): the stack's children resolve by bare name on the PATH `compass-stack`
inherits (`process.go:58`, `embedded.go:225-228`). **T6.2** also, when
spawning `compass-stack`, prepends the app executable's own directory to the
child's PATH (`cmd.Env` with a rewritten `PATH` entry), so the sidecar
`compass-postgres`/`compass-server`/`compass-runner` and the bundled postgres
tools (OQ1 ruled (a), DL-217) win LookPath inside the bundle. Stack and
children then resolve by one uniform "in-bundle wins" rule; a bare dev-box
`compass-stack` (no sibling staged) keeps resolving off the ambient PATH.

### A3 — Version stamp: one value, every binary, additive to the shell

The bundle build computes ONE version string — `0.1.0+g<short-sha>` (the
workspace version the five commands default to, plus build metadata from
`git rev-parse --short HEAD`) — and passes it as
`-ldflags "-X main.version=<v>"` to **all five** binaries in the same build.
Uniformity is load-bearing, not cosmetic: `compass-stack`'s attach path
hard-fails on a version mismatch against a live server
(`compass-stack/main.go:35-38`, `:253-255`), so a bundle whose stack and server
carry different stamps would refuse its own restart-attach.

The shell gains the missing convention piece (T6.1): an untagged
`go/cmd/compass-app/version.go` with `var version = "0.1.0"` (shared by the
gtk3 and nogtk3 entrypoints — both are `package main`) plus a `--version`
flag on the gtk3 entrypoint printing it to stdout, mirroring
`compass-stack/main.go:55-61`'s handle-before-dispatch pattern.

### A4 — CI cadence: affected-gated per-PR, unconditional on main + nightly (fork 3, resolved)

`compass-app-bundle` registers in `.moon/workspace.yml` with a `build` task
(deps: `compass-ui:build`) and a `ci` aggregate, its `inputs` naming the full
packaging closure: `/go/**` (excluding `**/*_test.go`, so `go/server/`,
`go/gen/`, and `go/events/` — the biggest bundled binaries — are covered, not
just cmd/internal) + `/go/go.{mod,sum}`, `/apps/ui/**` (the dist, via the
dependency), `/tools/toolchain/gtk-closure.nix`,
`/tools/toolchain/versions/go.nix` (a Go pin bump rebuilds every binary),
`/devenv.lock`, and the project's own script/nix files. That maps the frozen
gate onto the existing two-speed CI (`ci.yml:25-36`) with no ci.yml edit — moon
discovers registered projects (`ci.yml:14-18`):

- **Per-PR**: `moon ci :ci` schedules the bundle build only when a packaging
  input changed — a docs/UI-logic/forge PR never pays the GTK closure + link.
  This is the same affected-guard posture the M4 gtk3 e2e step takes
  (`ci.yml:354-361` skips unless the PR touches `go/cmd/compass-app/`) and the
  same heavy-nix-build-as-moon-project precedent `compass-agent-image` set
  (`agent-image/moon.yml:10-14`: "a PR that touches the image closure builds
  the image; every push to main runs it unconditionally").
- **Main + nightly**: `moon run :ci` runs every project unconditionally
  (`ci.yml:233-241`), so "CI green building the bundle from a clean checkout"
  (`compass-native-app/design.md:533`) is proven on every landing and every
  night — an inputs-glob gap a PR skipped cannot hide (the backstop
  `ci.yml:30-36` names).

**Deviation from the frozen parent — ratified by Matt (2026-08-19).** The parent
record's T6 Interfaces line reads "Produces an installable artifact CI builds
on **every PR**" (`compass-native-app/design.md:531-532`), which this
affected-gated-per-PR cadence does not honor literally. This record treats
the T6 Gate line — "CI green building the bundle from a clean checkout"
(`compass-native-app/design.md:533-534`) — as the normative intent, and on
that reading AMENDS the interface to affected-per-PR + unconditional
main/nightly. Matt ratified this amendment on 2026-08-19 (via `ask`); the
chosen approach is unchanged.

**Second deviation — lane shape.** The parent T6 also names "moon-registered
build/test/ci lanes for the shell project"; this record realizes them as a
`build`+`ci` pair on the NEW `compass-app-bundle` project (not the shell
project), with no standalone `test` lane by design — the bundle carries no Go
tests of its own (the shell's live in `compass-go:ci` via T6.1/T6.2), and its
test intent is the T6.3 sanity gate folded into `build`.

The build task carries its own sanity gate (T6.3): after staging, it asserts
every binary exists, the five compass binaries each print the stamped value from
`--version` (the bundled `postgres`/`initdb`/`createdb` tools carry PostgreSQL's
own 18.x version, so they are checked present + executable, not stamp-matched),
and `bin/dist/index.html` is present — so a green lane means a *complete*
bundle, not merely an exit-0 script.

### A5 — Scope boundary

In scope: the Linux bundle project, the sidecar layout + PATH threading, the
shell version stamp, the moon/CI registration, the bundle sanity gate, and the
dev-box T4-smoke procedure. Out of scope, each already deferred or owned
elsewhere: macOS packaging and signed installers/auto-update
(`compass-native-app/design.md:246-248`), no-nix-store end-user distribution
(the A1 seam, filed as the A5 installer follow-up's first concern), the agent
image (pulled from GHCR at first run, DL-112 — never embedded), and the GTK3→
GTK4 migration (RIG-1770, Backlog): the bundle script takes its build tag and
pkg-config set from `gtk-closure.nix`/the `-tags gtk3` invocation in ONE place,
so a GTK4 flip is a closure-list + tag edit, not a packaging redesign.

## Alternatives considered

### Fork 1 — AppImage (rejected for T6, the named A5 candidate)

The standard Linux answer for "runs on any end-user box". Rejected *now* on
three grounds: (1) an AppImage built the conventional way — linuxdeploy with
its excludelist — ships without WebKit/GTK and links the system libraries,
contradicting the repo's pinned-closure posture, where the binary links the
devenv.lock-pinned closure via the nix cc-wrapper precisely because a
system-glibc link fails (`gtk-e2e-env.nix:69-73`: "a stock GitHub runner's
system gcc links against its older system glibc, so `ld` fails … against
libwebkit2gtk-4.1.so"). An AppImage that instead carries the whole store
closure *is* viable, but it is `nix bundle` with a different skin — rejected
on the same no-consumer/size grounds below. (2) T6's gate runs on the dev
box, which the tarball already serves.
(3) Nothing is foreclosed: an AppImage wraps the same A2 bundle dir later.

### Fork 1 — `nix bundle` (rejected: cost without a consumer)

Produces a self-extracting executable embedding the whole closure — genuinely
runs on a no-nix box, but at WebKitGTK-closure size per artifact, a new
flake/bundler surface (the repo's nix helpers are deliberately flake-less,
plain `-f` files: `gtk-e2e-env.nix`, `gate-tools.nix`), and a single-file shape
that fights the sidecar layout the shell resolves (A0). No T6 consumer needs
its one advantage.

### Fork 1 — upstream `wails3 package` (rejected: beta tooling, wrong toolchain)

Wails v3 is a beta dependency (`go/go.mod:29`) and its Linux packager drives
its own .desktop/AppImage generation outside the pinned nix toolchain — the cgo
link would ride whatever compiler/pkg-config it finds, re-opening exactly the
glibc-skew failure the M4 cc-wrapper work closed (`gtk-e2e-env.nix:68-77`). It
also cannot know about the four non-Wails sidecar binaries, which are most of
the bundle.

### Fork 2 — Go `embed` of the binaries into the shell (rejected)

`//go:embed` cannot even reach the UI dist (`main.go:14-16`), and embedding the
four ELF sidecars means extracting them to a writable temp dir at every launch
to exec them — a self-modifying-install pattern that breaks the read-only
bundle, doubles disk footprint, and races concurrent launches. The shell
already implements sibling resolution (`embedded.go:342-359`,
`main.go:313-321`); embedding would delete working code to add worse code.

### Fork 2 — Wails asset bundling (rejected: wrong tool)

Wails' asset server carries *web* assets into the webview
(`application.BundledAssetFileServer`, already serving the dist per
`main.go:16-19`). It has no story for native sidecar executables the OS must
`exec` — the stack binaries are not assets.

### Fork 3 — full bundle build on every PR (rejected)

Simple and maximally safe, but it makes every docs/UI/forge PR pay the
WebKitGTK realization + cgo link. The repo's CI already rejected exactly this
shape twice: the moon battery is "deliberately GTK-free" per-PR
(`gtk-e2e-env.nix:3-4`) and the agent image builds affected-only on PRs
(`agent-image/moon.yml:10-14`). The main + nightly unconditional sweep
(`ci.yml:233-241`) is the designed backstop that keeps affected-gating honest.

### Fork 3 — a dedicated ci.yml job instead of a moon project (rejected)

A hand-rolled YAML job re-creates the enumeration drift ci.yml's header
forbids: "a project list in a workflow is a second source of truth for
something .moon/workspace.yml already owns. That list goes stale silently"
(`ci.yml:8-11`). The M4 e2e step is a ci.yml step only because it must *run*
the app under xvfb with a display; the bundle build has no such needs — it is a
plain heavy build, which is precisely what the moon-project pattern
(`compass-agent-image`) exists for.

## Global Constraints

Inherited from the frozen parent (`compass-native-app/design.md:325-362`) and
the epic batch context; every task below inherits them:

1. **Go 1.25 floor, one module** — `go/go.mod:15` (`go 1.25.0`); everything
   builds inside the existing `github.com/sealedsecurity/compass/go` module,
   never a second one.
2. **License AGPL-3.0-only** for the shell/bundle, matching `apps/ui`; the
   bundle carries the LICENSE file.
3. **moon-registered CI lanes** — the bundle project registers in
   `.moon/workspace.yml` in the SAME change that adds its tree (the silent-inert
   trap `workspace.yml:63-69` documents); nothing ships ungated.
4. **Linux-only for this record** — macOS is the A5 follow-up; the runner stays
   Linux/rootless-podman/uid-1000 (inherited, not solved here —
   `compass-native-app/design.md:346-349`).
5. **Bearer secrets never in argv** — the bundle build and .desktop introduce
   no new secret carriage; the stamp value is not secret.
6. **The agent image is never embedded** — GHCR pull at first run (DL-112);
   the publish lane exists (`agent-image/moon.yml:5-8`).
7. **golangci lints WITHOUT build tags** — gtk3-tagged files are outside the
   lint lane (`go/moon.yml:77-109` runs untagged), so any new gtk3-tagged Go
   stays gofmt/vet/lint-clean by review + the untagged twin files; new
   *untagged* code (version.go, PATH threading) is fully gated by
   `compass-go:ci` (`go/moon.yml:183-191`).
8. **Version stamp convention** — `-ldflags "-X main.version=<v>"` over a
   `var version = "0.1.0"` default (`compass-server/main.go:25-28`); one stamp
   value across all bundled binaries (A3's attach-mismatch rationale,
   `compass-stack/main.go:35-38`).
9. **The GTK closure has one definition** — any packaging-side GTK/pkg-config
   need imports `tools/toolchain/gtk-closure.nix` (its header:
   "ONE definition, imported by two consumers so they cannot drift",
   `gtk-closure.nix:3-4`) via the devenv.lock pin, never a third package list.
10. **Design-decision ledger** — this record's rulings land as DL rows in
    `docs/designs/product/DECISIONS.md` in the design PR (proposed rows in
    §Ledger-impact), with a `Ledger-impact:` commit trailer.

## Plan

### T6.1 — Shell version stamp (`compass-app` joins the convention)

- **Do:** add untagged `go/cmd/compass-app/version.go` with the standard
  `var version = "0.1.0"` + convention comment (mirroring
  `compass-server/main.go:25-28`); handle `--version` before flag dispatch in
  the gtk3 entrypoint (stdout, exit 0 — the `compass-stack/main.go:55-61`
  pattern) and log `version` in the nogtk3 stub's error line
  (`main_nogtk3.go:24-28`); include `version` in the shell's startup slog line.
- **Interfaces:** consumes nothing new. Produces `main.version` as the
  `-ldflags` stamp target for T6.3; `compass-app --version` as the sanity-gate
  probe.
- **Gate (test cycle):** untagged unit test asserting the default value and the
  `--version` output path (pure function over the arg slice, testable without
  gtk3); `moon run compass-go:ci` green (the file is untagged, so fmt/vet/lint/
  test all see it).

### T6.2 — Sidecar PATH threading in the embedded launch

- **Do:** two changes in `go/cmd/compass-app/embedded.go`. First, reorder
  `resolveStackBin`'s precedence to flag → `$COMPASS_STACK_BIN` → sibling →
  PATH (`embedded.go:342-359`) — move the sibling block above the
  `exec.LookPath` block — so a packaged build's sibling `compass-stack` wins
  over any ambient one, while a dev-box build (no sibling) still falls
  through to PATH. Second, build the `compass-stack` child environment by
  prepending `filepath.Dir(os.Executable())` to `PATH` (a pure
  `prependExecDirToPath(env []string, execDir string) []string` helper,
  applied to both the `up` and `down` exec closures at `embedded.go:225-228`
  / `:274-277`), so the sidecar `compass-postgres`/`compass-server`/
  `compass-runner` win `exec.LookPath` (`process.go:58`) inside a bundle
  while an ambient dev-box PATH keeps working unchanged (prepend, never
  replace).
- **Interfaces:** consumes `resolveStackBin`'s beside-the-executable contract
  (`embedded.go:342-359`). Produces a child PATH contract T6.3's layout
  relies on.
- **Gate (test cycle):** untagged unit tests on the helper (prepend
  semantics, no-PATH-var edge, idempotence) plus an embedded-pipeline test
  asserting the built `exec.Cmd.Env` carries the prepended entry — the
  existing stub-injection style (`embedded.go:59-62`); the precedence reorder
  must keep the existing cross-process sibling-resolution test green
  (`go/cmd/compass-app/cross_process_podman_test.go` stages sibling binaries
  and asserts resolution). `moon run compass-go:ci` green.

### T6.3 — The bundle project (`app-bundle/`)

- **Do:** new moon project `compass-app-bundle` at `app-bundle/` with:
  - `bundle-env.nix` — imports `tools/toolchain/gtk-e2e-env.nix` directly for
    its already-pinned `pkgConfig` + `cc` outputs, adding no re-pinned third
    copy of the devenv.lock boilerplate (GC 9). It carries ONLY the delta: a
    `postgresql` attr (the devenv.lock-pinned bare attr) for the three postgres
    tools (OQ1 ruled (a), DL-217).
  - `build.sh` — from a clean checkout: realize the pinned env outputs with
    `nix build -f tools/toolchain/gtk-e2e-env.nix pkgConfig cc.out` (reusing
    the already-pinned outputs directly, not a re-pinned copy), plus the
    `bundle-env.nix` `postgresql` delta (OQ1 (a), DL-217); keep the realized
    `result` GC-root symlink under `app-bundle/` (gitignored) so a
    `nix-collect-garbage` cannot delete the closure and dangle the dev-box
    tarball's rpaths and the postgres-tool `bin/` symlinks — a bare
    `nix build --no-link` (as the `ci.yml:372-379` dance uses) creates NO GC
    root, which never bites ephemeral CI runners but would bite the
    persistent dev box (T6.5). Compute
    `v=0.1.0+g$(git rev-parse --short HEAD)`; build
    `CGO_ENABLED=1 CC=<cc>/bin/cc PKG_CONFIG_PATH=<pcenv> go build -trimpath
    -tags gtk3 -ldflags "-X main.version=$v" ./cmd/compass-app` and
    `CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$v"` for
    `compass-stack`/`compass-server`/`compass-runner`/`compass-postgres`; stage
    the A2 layout (dist from `apps/ui/dist`, `compass.desktop`, LICENSE, and
    the `postgres`/`initdb`/`createdb` store symlinks into `bin/`); run
    the sanity assertions (all five compass binaries present, each printing `$v`
    from `--version`; the `postgres`/`initdb`/`createdb` tools present and
    executable; `bin/dist/index.html` exists); tar to
    `compass-app-$v-linux-amd64.tar.gz`.
  - `moon.yml` — `build` task (deps: `['compass-ui:build']`, `outputs` the
    tarball dir, `inputs` = `/go/**` (excluding `**/*_test.go`, so
    server/gen/events are covered, not just cmd/internal), `/go/go.{mod,sum}`,
    `/tools/toolchain/gtk-closure.nix` + `/tools/toolchain/gtk-e2e-env.nix`
    (both realized directly by build.sh, §435-444), `/tools/toolchain/versions/go.nix`,
    `/devenv.lock`, `/LICENSE` (copied into the artifact), project-local files
    — no `/apps/ui/src/**` belt: moon
    schedules dependents of affected projects, so a `compass-ui:build` input
    change re-runs this build through the `deps` edge (verified once on the
    PR)) + `ci: deps ['build']` with
    `cache: false`; `inheritedTasks.exclude: ['install','lint','format']` (the
    non-bun guard `agent-image/moon.yml:27-31` carries).
  - `compass.desktop` — `Exec=compass-app`, Name/Icon/Terminal per spec
    minimum; shipped as an A5-installer template, inert until an installer
    stages it (the sanity gate only asserts the file is present, not that it
    functions).
- **Interfaces:** consumes `compass-ui:build`'s `dist` output
  (`apps/ui/moon.yml:19-23`), `gtk-closure.nix` (GC 9), the T6.1 stamp target,
  T6.2's PATH contract. Produces the tarball artifact and the moon lane A4
  schedules.
- **Gate (test cycle):** `moon run compass-app-bundle:build` from a clean local
  checkout produces the tarball and passes its own sanity assertions;
  `moon query projects` lists the project (the `workspace.yml:63-69`
  registration check).

### T6.4 — CI registration + affected verification

- **Do:** register `compass-app-bundle: 'app-bundle'` in `.moon/workspace.yml`
  (same PR as T6.3's tree — GC 3). No ci.yml edit (A4). Verify the cadence
  empirically: the packaging PR (which touches the inputs) must schedule
  `compass-app-bundle:ci` in the `gates` job log. The negative — a change
  that must NOT schedule the bundle — must be observed on a SEPARATE
  docs-only PR (or any concurrent PR not touching the inputs), never as a
  follow-up commit on the packaging PR: moon diffs the PR's base..HEAD
  cumulatively (`ci.yml:221-231`), so once the PR has touched the inputs
  every later commit on it still schedules the task.
- **Interfaces:** consumes T6.3's project + the existing `ci.yml:220-241`
  affected/full split. Produces the frozen "CI green building the bundle from a
  clean checkout" gate on main + nightly.
- **Gate (test cycle):** the PR's `CI` rollup green with the bundle task
  scheduled and passing; after merge, the next main-push full sweep runs it
  unconditionally (observed in the run log).

### T6.5 — Dev-box smoke: the packaged artifact passes the T4 smoke

- **Do:** unpack the CI-built (or locally moon-built) tarball on the dev box
  into a fresh prefix with an empty `--state-dir`; launch `bin/compass-app`;
  run the T4 smoke against the *packaged* binaries: preflight passes (podman +
  GHCR image pull per DL-112), embedded bring-up via the sidecar
  `compass-stack` (T6.2 PATH threading proven end-to-end), board renders over
  the socket, one agent session reaches a running container, quit path stops
  the stack. Record the procedure as `app-bundle/SMOKE.md` so the gate is
  re-runnable, not tribal.
- **Interfaces:** consumes T6.3's tarball, the T4 smoke definition
  (`compass-native-app/design.md:533-534`). Produces the epic's T6 completion
  evidence.
- **Gate (test cycle):** the smoke checklist fully green on the dev box from
  the tarball alone (no `go build`, no devenv PATH for the stack children —
  the postgres tools ship in the bundle `bin/` per OQ1 (a), DL-217); failure
  of any step reopens the owning task.

## Tasks

- [ ] **T6.1** `version.go` + `--version` on the shell; unit tests;
  `compass-go:ci` green.
- [ ] **T6.2** `prependExecDirToPath` threading into the stack up/down execs;
  unit + pipeline-env tests; `compass-go:ci` green.
- [ ] **T6.3** `app-bundle/` project: `bundle-env.nix`, `build.sh` (build +
  stage + stamp + sanity + tar), `moon.yml`, `compass.desktop`; local clean
  build green.
- [ ] **T6.4** workspace-map registration; affected-cadence verified on the PR;
  main full-sweep observed post-merge.
- [ ] **T6.5** dev-box smoke from the tarball per `SMOKE.md`; T4 smoke passes
  end to end.

## Open Questions

### OQ1 (load-bearing, for Matt) — postgres tooling: bundled or host prerequisite?

`compass-postgres` LookPaths `postgres`/`initdb`/`createdb` at runtime
(`compass-postgres/main.go:187,282,317`); the bundle ships none of them, and
the frozen T6 spec is silent. Two coherent shapes:

- **(a) Bundle it (recommended):** `bundle-env.nix` adds the devenv.lock-pinned
  bare `postgresql` (the exact parity attr `devenv.nix:108-113` mandates) and
  `build.sh` stages store symlinks for the three tools into `bin/`, where
  T6.2's PATH prepend resolves them. Cost: ~tens of MB of store closure the
  build already has cached; the artifact honors the epic charter ("a single
  user runs the entire Compass stack as one native application with nothing
  else to stand up", quoted at `compass-native-app/design.md:265-267`) for the
  database, leaving podman as the sole host prerequisite.
- **(b) Host prerequisite, like podman:** preflight already degrades legibly —
  the DB check warns rather than fails pre-`up` (`embedded.go:453-455`) — and a
  missing `initdb` would surface as a legible stack error. Smaller artifact;
  but a fresh dev box then needs a postgres install *outside* the artifact,
  and version skew between host postgres and the pinned 18.4 the dev shell/CI
  exercise (`devenv.nix:108-112`) becomes a support surface.

**Resolved — Matt ruled (a) on 2026-08-19** (via `ask`): postgres tooling
ships in the bundle `bin/` from the devenv.lock-pinned bare `postgresql` attr,
leaving rootless podman as the sole host prerequisite. T6.3/T6.5 build under
(a); the ledgered ruling is DL-217.

## Ledger-impact (proposed rows — land with the design PR, not before)

Next free ID verified against `DECISIONS.md` this session (highest existing:
DL-213 — the RIG-1509 ask-comms rows landed on main during this pass).

| ID | Decision | Status | Record |
| --- | --- | --- | --- |
| DL-214 | The T6 Linux app artifact is a versioned tarball of a nix-closure-backed bundle directory (store-rpathed binaries via the devenv.lock-pinned GTK closure + cc-wrapper, `.desktop` inside), not AppImage/`nix bundle`/`wails3 package`; no-nix-store end-user distribution is the A5 installer follow-up's concern, which wraps this same bundle dir | Active (Matt, 2026-08-19) | [packaging §A1](compass-native-packaging/design.md#a1--bundle-format-a-versioned-tarball-of-a-nix-closure-backed-bundle-dir-fork-1-resolved) |
| DL-215 | Bundle binary carriage is the sidecar `bin/` layout the shell already resolves (stack sibling + `dist` beside the executable), completed by the app prepending its executable dir to the spawned `compass-stack`'s PATH so the stack's LookPath children resolve in-bundle — never Go `embed` of ELF sidecars, never Wails asset bundling | Active (Matt, 2026-08-19) | [packaging §A2](compass-native-packaging/design.md#a2--binary-embedding-sidecar-bin-layout--path-threading-fork-2-resolved) |
| DL-216 | The bundle build is a moon-registered project (`compass-app-bundle`) riding affected-detection per-PR and the unconditional main/nightly full sweep — the `compass-agent-image` heavy-build precedent — with a built-in completeness sanity gate (binaries present, uniform `--version` stamp, dist present); never a per-PR unconditional build, never a ci.yml-enumerated job | Active (Matt, 2026-08-19) | [packaging §A4](compass-native-packaging/design.md#a4--ci-cadence-affected-gated-per-pr-unconditional-on-main--nightly-fork-3-resolved) |
| DL-217 | Postgres tooling (`postgres`/`initdb`/`createdb`) ships in the bundle `bin/` from the devenv.lock-pinned bare `postgresql` attr, leaving rootless podman as the sole host prerequisite of the packaged embedded mode | Active (Matt, 2026-08-19) | [packaging §OQ1](compass-native-packaging/design.md#oq1-load-bearing-for-matt--postgres-tooling-bundled-or-host-prerequisite) |
