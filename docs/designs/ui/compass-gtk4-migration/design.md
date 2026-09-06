# Design: Compass native shell GTK4 / webkitgtk-6.0 migration (RIG-1770)

Status: Draft
Linear: RIG-1770 (GTK3 → GTK4/webkitgtk-6.0 migration of the Compass native
shell). Matt's steer 2026-08-26: migrate proactively (option b) ahead of the
Wails v3.1 gtk3 removal.

## Problem / Intent

The Compass Linux desktop shell (`go/cmd/compass-app`, Wails v3, DL-110) builds
against Wails' **legacy** GTK3 variant: the repo-local `gtk3` build tag selects
Wails' `linux && cgo && gtk3` cgo files
(`linux_cgo_gtk3.go:18` — `#cgo linux pkg-config: gtk+-3.0 webkit2gtk-4.1  gdk-3.0`),
linked against the repo's own GTK closure
(`tools/toolchain/gtk-closure.nix:22-23` — `gtk3`, `webkitgtk_4_1`). Upstream,
GTK4 + webkitgtk-6.0 is already the **default** Linux stack
(`linux_cgo.go:17` — `#cgo linux pkg-config: gtk4 webkitgtk-6.0`, tag gate
`linux && cgo && !gtk3 && !android && !server`), and the Wails v3 installation
docs state the legacy GTK3 path "is supported through the v3.0.x line and will
be removed in v3.1" (v3.wails.io/quick-start/installation). Staying on `gtk3`
means the shell dies with Wails v3.1.

This record designs the proactive migration of the shell, its nix closure, its
packaging, and its CI gates from GTK3 + webkit2gtk-4.1 onto GTK4 +
webkitgtk-6.0 (Matt's steer, 2026-08-26: plan the migration now rather than
wait for removal). Design record only — no implementation in this PR.

## Approach

### The chosen shape: flip the repo to the Wails default (GTK4), keep the repo-local tag discipline

The migration is a **coordinated four-surface flip**, not a rewrite. Grounding
for why it is this small:

1. **The app's Go code is GTK-version-neutral.** `go/cmd/compass-app` never
   touches GTK directly; every framework call goes through
   `github.com/wailsapp/wails/v3/pkg/application`'s portable API
   (`main.go:69-77` `application.New`/`application.NewService`/
   `BundledAssetFileServer`; `bridge_service_window_gtk3.go:29`
   `ctx.Value(application.WindowKey)`; `:49` `win.DispatchWailsEvent`). The
   GTK3-vs-GTK4 fork lives entirely inside Wails' own build-tagged cgo files.
2. **The bridge pump is framework-neutral — verified.** `go/internal/bridge/`
   `pump.go:3-9`: "Package bridge implements a framework-neutral
   gRPC-Web-over-h2c request pump … It has NO knowledge of Wails, webkit, or
   the UI". Zero GTK surface; untouched by this migration.
3. **The pinned nixpkgs already ships the target stack.** Verified by `nix
   eval` against devenv.lock's rev `c946ff36bf19`: `gtk4 = 4.22.4`,
   `webkitgtk_6_0 = 2.52.5` (same WebKit release as the current
   `webkitgtk_4_1 = 2.52.5`). No nixpkgs bump is required to get the packages.
4. **The closure has ONE definition.** `gtk-closure.nix:3-10`: "ONE definition,
   imported by two consumers so they cannot drift" (devenv.nix:266,
   gtk-e2e-env.nix:38) — plus the flake (`flake.nix:101`). Swapping the list
   swaps every consumer at once; the packaging record predicted exactly this:
   "a GTK4 flip is a closure-list + tag edit, not a packaging redesign"
   (`compass-native-packaging/design.md:292-296`).

The four surfaces:

+ **Build tags.** The repo keeps a repo-local desktop tag but renames it
  `gtk3` → `gtk4`. Wails selects GTK4 whenever its own `gtk3` tag is ABSENT,
  so the repo tag decouples from the Wails tag: `-tags gtk4` compiles the
  repo's shell files AND leaves Wails on its default GTK4 path. The
  tagged/stub file-pair discipline (`main.go` / `main_nogtk3.go`,
  `bridge_service_window_gtk3.go` / `_nogtk3.go`) is kept verbatim — it exists
  so the untagged `go build ./...` module gate never imports Wails' cgo
  (`main_nogtk3.go:5-12`), and that invariant is GTK-version-independent.
+ **The nix closure.** `gtk-closure.nix`: `gtk3` → `gtk4`, `webkitgtk_4_1` →
  `webkitgtk_6_0`; drop `atk` (merged into GTK4 as at-spi via GTK itself; keep
  only if the `.pc` Requires-walk demands it). Dev shell PKG_CONFIG_PATH,
  the e2e helper, and the flake package inherit the swap for free.
+ **Packaging + CI.** `app-bundle/build.sh:58` (`-tags gtk3`), `flake.nix:102`
  (`tags = [ "gtk3" ]`), and the ci.yml `gtk3-e2e` lane (`ci.yml:1203`
  `go test -tags 'unix gtk3'`) flip to the new tag. The e2e gate is the
  migration's compile-and-link proof — the ONE CI lane that compiles + runs
  the real shell (`gtk-e2e-env.nix:1-3`) exercises cgo link, window lifecycle,
  and event plumbing on GTK4, primarily under a headless Wayland compositor
  (GTK4's default user backend) with a secondary X11/Xvfb regression lane (see
  the Wayland tradeoff below and T4). It is not a full runtime proof.
+ **The ledger.** A new DL row records the GTK4 default and updates the
  closure definition; DL-110 (Wails v3) is untouched. See
  §Ledger delta.

### The honest tradeoffs (why this is a real fork, not a rubber-stamp)

+ **Upstream maturity.** GTK4 + webkitgtk-6.0 is now the DEFAULT documented
  Linux stack in the current Wails v3 install docs (v3.wails.io: "Linux
  requires ... gtk4 and webkitgtk-6.0"; supported platform Ubuntu 24.04),
  with GTK3 the explicit `-tags gtk3` legacy opt-out until v3.1 — so the
  default path reads GA, not an opt-in experiment. The only residual
  immaturity signal is the still-open tracking issue wails#4957 (its title
  still reads "[Feedback Wanted] Experimental GTK4 + WebKitGTK 6.0 Support")
  and the beta's newness; GTK4 fixes have landed across the v3.0.x betas, so
  the migration rides a Wails pin bump to the newest v3.0.x (T1/OQ1). Matt's
  ruling (2026-08-26): proceed with the flip regardless of the label.
+ **Distro floor bump (ACCEPTED — Matt 2026-08-26).** The default GTK4 stack
  requires Ubuntu 24.04+ / Debian 13+; distros shipping only webkit2gtk-4.1
  (Ubuntu 22.04 LTS, Debian 12, Fedora ≤ 39, RHEL 9.x) can only build the
  legacy tag (v3.wails.io/getting-started/installation). For Compass this
  bites only **source builds against system libs and any future non-nix
  packaging (the A5 installer follow-up)** — the shipped tarball is immune,
  because its ELFs are store-rpathed against the nix closure and
  self-contained (`compass-native-packaging/design.md:168-169`: "a box with
  no nix store cannot run this tarball" is the existing, unchanged limit).
  Matt's ruling: acceptable — Compass is a greenfield app with no legacy
  install base, so the floor costs us nothing. Recorded as a settled
  constraint (§Global Constraints), not an open tradeoff.
+ **Closure growth (measured, not gating — Matt 2026-08-26).** The bundle and
  the e2e runner realize the WebKitGTK closure; GTK4 adds gtk4 (+graphene,
  +gst plugins pulled by webkitgtk_6_0's propagations) while dropping
  gtk3/atk. webkitgtk_6_0 and webkitgtk_4_1 are the same 2.52.5 source, so the
  WebKit half is roughly size-neutral. The tarball delta is MEASURED and
  recorded in the migration PR body (Plan T2), but it does NOT gate the flip:
  Compass ships GTK4 regardless of the size change. Matt's ruling — surface
  the number only if it comes back surprisingly large.
+ **Wayland-first runtime — CI now exercises Wayland (Matt 2026-08-26).**
  GTK4's default user backend is Wayland. The original plan ran the e2e gate
  under Xvfb/X11 only (`GDK_BACKEND=x11`), so CI would permanently exercise a
  backend real users won't use — the migration's default user path would ship
  untested. Matt's ruling: switch CI to Wayland. The e2e gate runs the shell
  under a headless Wayland compositor (`weston --backend=headless`, in the
  pinned nixpkgs) with `GDK_BACKEND=wayland`, so the gate exercises the real
  default backend; a cheap X11/Xvfb run is kept as a secondary regression lane
  (GTK4 retains the X11 backend). The compositor lands in the e2e-runner
  closure ONLY, so the tarball size (OQ2) is unaffected. This closes the
  residual-risk gap the X11-only plan carried; the manual Wayland smoke in T5
  stays as belt-and-suspenders on the shipped tarball.

### Alternatives considered

#### Option (a) — stay on gtk3 until Wails v3.1 forces the move (rejected)

Zero work now; the gtk3 variant is supported "through the v3.0.x line". But:
the deadline is upstream-controlled and lands as a forced migration coupled to
whatever else v3.1 changes (worst time to absorb an experimental-stack flip);
every month on GTK3 deepens the freeze around a stack upstream calls
legacy; and the flip is cheap NOW precisely because the pinned nixpkgs already
carries gtk4/webkitgtk_6_0 at the same WebKit release — a later nixpkgs pin
may not be so aligned. Matt's steer (2026-08-26) is (b); (a) is recorded as
the do-nothing baseline it was weighed against.

#### Dual-variant transitional support (build both gtk3 and gtk4 lanes) (rejected)

Doubles the heavy WebKitGTK closure in CI and the bundle matrix for zero
consumer: the shipped artifact is nix-rpathed (users never link system GTK),
no released Compass artifact promises gtk3, and the repo's clean-cutover house
rule applies. Rollback is `jj` revert of one small PR, not a standing lane.

#### CI-only GTK4 canary before the flip (weighed, not adopted)

A narrower shape than dual-variant: add a `gtk4-e2e` CI lane that compiles
`go test -tags 'unix gtk4'` against a GTK4 closure variant while the shipped
artifact stays `gtk3`, running the experimental stack in a lane that ships
nothing for N weeks before the irreversible flip. This doubles only the one
affected-gated e2e lane (`ci.yml:1034-1051`, already out-of-band), not the
bundle matrix, and burns down the experimental-upstream risk (wails#4957)
ahead of the atomic PR. Rejected for adoption but recorded as the strongest
competitor to the chosen shape: it needs a SECOND closure definition,
temporarily violating the one-definition invariant (`gtk-closure.nix:3`) the
whole approach leans on, and Matt's steer is to migrate now while the
same-WebKit-2.52.5 alignment window is open. If GTK4-in-beta instability
surfaces in the migration PR's own e2e, this canary is the fallback (promote to
an optional T3.5), not a reason to hold the freeze.

### Ledger delta (for the coordinator to encode at freeze — NOT edited here)

No DECISIONS.md row pins GTK3 by name: `grep 'GTK3\|GTK4\|WebKit'
docs/designs/DECISIONS.md` returns nothing, and the DL-110 row
(`DECISIONS.md:240`) pins only "Wails v3 (Go)" — framework, not GTK variant.
The GTK3 pin lives in the repo's own GTK closure definition
(`tools/toolchain/gtk-closure.nix`, one definition imported by every in-repo
consumer) and in code/CI, not in a compass DL row. Therefore:

+ **No DL row is Superseded.** DL-110 stays Active unchanged (Wails v3 is
  unchanged); DL-214/DL-216 (packaging) stay Active (the bundle shape is
  unchanged, only its closure contents move).
+ **One NEW DL row** (id assigned at freeze): "The Compass Linux shell builds
  Wails' default GTK4 + webkitgtk-6.0 stack (repo tag `gtk4`; closure
  `gtk4`/`webkitgtk_6_0` in gtk-closure.nix), retiring the legacy `gtk3` +
  webkit2gtk-4.1 variant ahead of its Wails v3.1 removal; the closure edit is
  in place (one definition, all consumers)."

## Global Constraints

+ **Wails floor:** `github.com/wailsapp/wails/v3` ≥ the latest v3.0.x beta at
  implementation time (currently pinned `v3.0.0-beta.0`, `go/go.mod:32`).
  GTK4 is the default documented Linux stack across v3.0.x; the migration PR
  bumps the pin first (T1) so it carries the latest GTK4 fixes. Never v3.1
  (that removes gtk3 and lands unknown breaking changes; the point is to
  migrate before it). **Renovate does NOT auto-bump this pin (Matt
  2026-08-26).** The gomod manager sees `wails/v3` (unfenced,
  `tools/renovate/config.json5`), but the repo runs no automerge — every bump
  is a human-merged PR — and the pin is a `-beta` prerelease Renovate does not
  track reliably across betas. More importantly, T1 gates the pin move behind
  the gtk e2e (F3 re-runs it on any later pin move); an unattended bump would
  bypass that safety net. Keep the Wails pin MANUAL until upstream ships a
  stable (non-beta) tag; revisit auto-bump then.
+ **Package names:** pkg-config `gtk4`, `webkitgtk-6.0` (Wails
  `linux_cgo.go:17`); nixpkgs attrs `gtk4` (4.22.4), `webkitgtk_6_0` (2.52.5)
  — both present in the pinned devenv.lock/flake.lock rev `c946ff36bf19`
  (verified by nix eval this session). Includes `#include <webkit/webkit.h>`
  (new layout) vs GTK3's `<webkit2/webkit2.h>` — Wails' concern, not ours.
+ **Repo build tag:** `gtk4` replaces `gtk3` everywhere the repo spells it;
  Wails' own `gtk3` tag MUST NOT be passed anywhere after the flip (its
  absence is what selects GTK4 in Wails). The untagged `go build ./...`
  module gate keeps compiling zero Wails cgo (stub-pair invariant,
  `main_nogtk3.go:5-12`).
+ **Distro floor (system-lib builds only):** Ubuntu 24.04+/Debian 13+ for
  webkitgtk-6.0 dev packages. Shipped tarball unaffected (store-rpathed,
  DL-214). Docs that name system prerequisites must say so. ACCEPTED by Matt
  (2026-08-26): greenfield app, no legacy install base — the floor is free.
+ **Size (measured, not gating — Matt 2026-08-26):** the closure delta (bundle
  tarball + e2e runner realization) is measured and recorded in the migration
  PR body for the record, but it does NOT gate the flip — Compass ships GTK4
  regardless. Surface the number to Matt only if it comes back surprisingly
  large.
+ **markdownlint:** MD004 `+` bullets, MD040 fenced languages, blank lines
  around blocks — this record and any doc edits comply.

## Plan

Ordering: T1 (pin bump) → T2 (closure swap) → T3 (tag flip) → T4 (CI gate) →
T5 (packaging) → T6 (docs + ledger). T2–T5 land as ONE PR (the tag and the
closure are load-bearing together: `-tags gtk4` against a gtk3 closure fails
the cgo link, and vice versa); T1 and T6 may be separate PRs.

**Revert precondition (F3):** the T2–T5 revert path (revert one PR back to
new-pin + gtk3) is guaranteed green only while the T1-tested `(pin, gtk3)`
configuration is the revert target — re-run the gtk3 e2e if the Wails pin moves
after T1 lands.

### T1 — Wails v3 pin bump to the latest v3.0.x beta

+ **Do:** bump `github.com/wailsapp/wails/v3` from `v3.0.0-beta.0` to the
  newest v3.0.x tag; `go mod tidy`; re-run the full untagged suite AND the
  gtk3 e2e locally (the bump lands while the repo is still gtk3, proving the
  bump alone is behavior-neutral before the stack flips).
+ **Interfaces:** consumes `go/go.mod:32`; produces the new pin +
  `vendorHash` refresh in `flake.nix` (buildGoModule's `vendorHash` changes
  with go.mod) and a re-verified `linux_cgo.go` GTK4 tag-gate/pkg-config set
  at the new version (re-read the module cache; upstream may have renamed
  tags).
+ **Test cycle:** `go build ./...` untagged; `go test ./...`;
  `xvfb-run go test -tags 'unix gtk3' -run 'E2E' ./cmd/compass-app/` green
  pre-flip.

### T2 — Nix closure swap (gtk-closure.nix) + size measurement

+ **Do:** in `tools/toolchain/gtk-closure.nix` replace `gtk3` → `gtk4`,
  `webkitgtk_4_1` → `webkitgtk_6_0`; evaluate whether `atk` (GTK3-era
  accessibility) and `gdk-pixbuf` survive the `.pc` Requires-walk or drop.
  Measure: `nix path-info -S` on the old vs new `pkgConfig` buildEnv
  (gtk-e2e-env.nix) and the built app-bundle tarball sizes; record both
  deltas in the PR body per Global Constraint 5.
+ **Interfaces:** consumes `gtk-closure.nix:18-32` (the name list); produces
  the new list consumed unchanged by `devenv.nix:266` (PKG_CONFIG_PATH),
  `gtk-e2e-env.nix:38` (pcClosure), `flake.nix:101` (buildInputs). No
  nixpkgs pin change (Global Constraint 2: both attrs exist at
  `c946ff36bf19`).
+ **Test cycle:** `pkg-config --exists gtk4 webkitgtk-6.0` inside the dev
  shell; `nix build .#compass-app` (after T3's tag flip; in the combined PR
  this is the gate).

### T3 — Repo build-tag flip: `gtk3` → `gtk4` in go/cmd/compass-app

+ **Do:** rename the repo tag and the tag-carrying files. Per-file (tags
  verified against every current `*.go:1` header this session):

  | File | Before | After |
  | --- | --- | --- |
  | `main.go` | `(linux && gtk3) \|\| darwin` | `(linux && gtk4) \|\| darwin` |
  | `client.go` | `(linux && gtk3) \|\| darwin` | `(linux && gtk4) \|\| darwin` |
  | `window_set.go` | `(linux && gtk3) \|\| darwin` | `(linux && gtk4) \|\| darwin` |
  | `bridge_service_window_gtk3.go` → `bridge_service_window_gtk4.go` | `(linux && gtk3) \|\| darwin` | `(linux && gtk4) \|\| darwin` |
  | `main_nogtk3.go` → `main_nogtk4.go` | `linux && !gtk3` | `linux && !gtk4` |
  | `bridge_service_window_nogtk3.go` → `bridge_service_window_nogtk4.go` | `linux && !gtk3` | `linux && !gtk4` |
  | `client_test.go`, `window_set_test.go`, `window_name_test.go`, `multiwindow_e2e_test.go`, `multiwindow_e2e_helpers_test.go` | `unix && gtk3` | `unix && gtk4` |
  | `bridge_service.go`, `bridge_service_test.go`, `version.go` | `unix` / untagged | unchanged |

  Comment sweep: the `gtk3`-naming prose in `bridge_service.go:53-60`,
  `main_nogtk4.go`'s error string (`main_nogtk3.go:25-26` today: "requires
  the GTK3 + WebKit2GTK 4.1 stack") and `version.go:15-16` update to GTK4 +
  WebKitGTK 6.0 wording; while rewriting the error string, also fix
  `main_nogtk3.go:14`'s stale "go/moon.yml build lane" claim (`go/moon.yml`
  carries zero gtk references today — do not copy it verbatim into
  `main_nogtk4.go`). NOTE: the distribution record's frozen T2 darwin
  tag table (`compass-distribution/design.md:503-514`) spells `gtk3`; if
  darwin T2 has not landed when this executes, coordinate the rename with
  that lane (the table's `(linux && gtk3) || darwin` shapes are already
  reflected in HEAD, so this task renames post-T2 shapes).
+ **Interfaces:** consumes the Wails tag semantics (absence of `gtk3` =
  GTK4, `linux_cgo.go:1`); produces the `gtk4` tag consumed by T4 (ci.yml)
  and T5 (build.sh, flake.nix). windowFromContext/DispatchWailsEvent usage
  is portable `application.*` API (`bridge_service_window_gtk3.go:29,49`) —
  no code change beyond tags/comments expected; any GTK4 behavioral break
  surfaces in T4's e2e, not here.
+ **Test cycle:** untagged `go build ./...` + `go test ./...` (stub pair
  intact); `go build -tags gtk4 ./cmd/compass-app` links against the T2
  closure; duplicate-symbol check `go vet -tags gtk4 ./cmd/compass-app`.

### T4 — CI e2e lane flip + Wayland-first verification (X11 secondary)

+ **Do:** in `.github/workflows/ci.yml` rename the `gtk3-e2e` job → `gtk4-e2e`
  (job name, `gtk3_affected` output plumbing at `ci.yml:141,201-203,1041-1051`,
  and the step's `go test -tags 'unix gtk3'` → `'unix gtk4'` at `ci.yml:1203`);
  update the setup generator (`tools/ci-matrix/`, NOT `tools/ci`): the
  `gtk3Affected` spellings live in `index.ts:59,74-75,173-175,260,284`, its
  unit tests `index.test.ts:210-307`, and the `moon.yml:6` comment — rename all
  three, matching the T3 file-table precision. **Also extend the affected
  trigger** (F2): the e2e lane currently keys only on the `go/cmd/compass-app/`
  path prefix (`index.ts:74-75`, `ci.yml:1146`), so a closure-only follow-up PR
  (the T2 atk/gdk-pixbuf trim this plan schedules) would skip the ONE lane that
  compiles the shell — add `tools/toolchain/gtk-closure.nix` and
  `gtk-e2e-env.nix` to the trigger (both `index.ts` and the ci.yml in-step
  diff). **Run the e2e under Wayland (Matt 2026-08-26):** launch a headless
  compositor (`weston --backend=headless`, pinned nixpkgs) and set
  `GDK_BACKEND=wayland` so the gate exercises GTK4's default user backend, not
  X11. Keep a cheap secondary X11/Xvfb run (`GDK_BACKEND=x11`; GTK4 retains
  the X11 backend) as a regression lane. The compositor is an
  e2e-runner-only closure addition (not in the bundle — OQ2/size budget
  untouched). Keep the PASS-line guard (`ci.yml:1214`) verbatim.
+ **Interfaces:** consumes T3's `gtk4` tag + T2's closure via
  `gtk-e2e-env.nix` (unchanged file — it imports gtk-closure.nix); produces
  the renamed required-check plumbing under the `CI` rollup (branch
  protection requires only the rollup, `ci.yml:29-30`, so no repo-settings
  change).
+ **Test cycle:** the lane itself:
  `--- PASS: TestMultiWindowCloseCancelsOnlyClosingWindowE2E` on the PR.

### T5 — Packaging flip: app-bundle + flake

+ **Do:** `app-bundle/build.sh:58` `-tags gtk3` → `-tags gtk4` (+ its
  gtk3-naming comments at `build.sh:3,54-56`); `flake.nix:102`
  `tags = [ "gtk3" ]` → `[ "gtk4" ]` (+ comments at `flake.nix:82-86`);
  `app-bundle/moon.yml` + `SMOKE.md` prose sweep (`SMOKE.md:38,119`).
+ **Interfaces:** consumes T2's closure (both consumers resolve
  gtk-closure.nix) and T3's tag; produces the GTK4-linked
  `compass-app-<version>-linux-amd64.tar.gz` and `nix build .#compass-app`.
+ **Test cycle:** `nix flake check` (realizes every package,
  `flake.nix:121-128`); the DL-238 bundle smoke (`app-bundle/SMOKE.md`)
  against the GTK4 tarball, run **on both an X11 and a Wayland session**
  (mandatory, W2 — CI now exercises Wayland via the headless compositor (T4),
  but that runs the e2e binary, not the shipped tarball; a human run of the
  PACKAGED tarball under a real Wayland session before merge is the only proof
  the distributed artifact works on the default backend; the dev box can do
  this); tarball size delta recorded (Global Constraint 5).

### T6 — Docs sweep + ledger encode

+ **Do:** sweep remaining `gtk3` prose: `devenv.nix:106,153-160,225-251`
  comments, `gtk-e2e-env.nix:1-27` comments, native-app record's
  system-libs constraint (`compass-native-app/design.md:384-391` names
  `webkit2gtk-4.1` — annotate, don't rewrite frozen prose, per the repo's
  banner-amendment convention seen at `compass-native-app/design.md:25-29`).
  Coordinator encodes the new DL row (§Ledger delta).
+ **Interfaces:** consumes the landed T2–T5 state; produces the amendment
  banners + the DL row text handed to the coordinator (this record's §Ledger
  delta is the source).
+ **Test cycle:** markdownlint green tree-wide; `grep -ri 'gtk3' --include
  '*.nix' --include '*.yml' --include '*.sh' --include '*.go' .` returns only
  historical design-record prose (the `*.go` include catches
  `go/e2e/client_mode_test.go:17`, a gtk3 prose reference outside the T3
  cmd/compass-app sweep).

## Tasks

+ [ ] **T1** Wails pin bump to latest v3.0.x beta; untagged suite + gtk3 e2e
  green pre-flip; flake `vendorHash` refreshed.
+ [ ] **T2** gtk-closure.nix swap (`gtk4`, `webkitgtk_6_0`); atk/gdk-pixbuf
  re-evaluated; closure + tarball size deltas measured and recorded.
+ [ ] **T3** Repo tag flip `gtk3` → `gtk4` per the T3 file table; stub-pair
  invariant intact; comment/error-string sweep in cmd/compass-app.
+ [ ] **T4** ci.yml lane rename + tag flip; `gtk4_affected` plumbing;
  GTK4 e2e under headless Wayland (`weston --backend=headless`,
  `GDK_BACKEND=wayland`) with X11/Xvfb as secondary lane; PASS-line guard kept.
+ [ ] **T5** app-bundle/build.sh + flake.nix tag flip; `nix flake check` +
  bundle smoke green on GTK4.
+ [ ] **T6** Docs sweep + amendment banners; coordinator encodes the new DL
  row.

## Open Questions

+ **OQ1 (RESOLVED — Matt 2026-08-26): Wails pin target.** Matt: GTK4 reads GA
  in the current Wails docs (default documented Linux stack); proceed with the
  flip regardless of the still-open feedback issue. **Resolution:** bump to
  the newest v3.0.x beta at implementation time (T1), gated by the e2e;
  Renovate does NOT auto-track it (see §Global Constraints, Wails floor). The
  exact target tag is picked at T1 against the then-current release list.
+ **OQ2 (RESOLVED — Matt 2026-08-26): tarball size.** Matt: ship GTK4
  regardless of the bundle-size change — the size delta does not gate the
  flip. **Resolution:** measure the closure + tarball delta and record it in
  the migration PR body (Plan T2) for the record; surface the number to Matt
  only if it comes back surprisingly large. No threshold, no gate. (An earlier
  draft cited a private image-layer-budget constraint here; that citation was
  wrong and is removed — no such budget governs the app-bundle tarball.)
+ **OQ3 (non-load-bearing): repo tag name.** `gtk4` (proposed; symmetric,
  self-describing) vs `desktop` (version-neutral, no rename at GTK5).
  **Recommendation:** `gtk4` — it must be spelled at every cgo boundary
  anyway, and a neutral name would hide which Wails variant is selected.
+ **OQ4 (RESOLVED — Matt 2026-08-26): CI backend.** Matt: switch CI to
  Wayland. **Resolution:** the e2e gate runs GTK4 under a headless Wayland
  compositor (`weston --backend=headless`, pinned nixpkgs) with
  `GDK_BACKEND=wayland` as the PRIMARY lane (it exercises the real default
  user backend); an X11/Xvfb run stays as a cheap secondary regression lane.
  Folded into T4. Compositor is e2e-runner-closure-only, so OQ2/size budget is
  untouched.
+ **OQ5 (RESOLVED — verified against the managed-side build 2026-08-26): no
  cross-repo closure coupling.** An earlier draft asserted that a hand-synced
  managed-side CI image staged this GTK closure's `-dev` outputs, making a
  managed-side amendment a merge-blocking predecessor. That is verified false:
  nothing on the managed side builds or links `compass-app`, and its CI
  toolchain does not consume this GTK closure. So no out-of-tree lane depends on
  it — case (a) is confirmed and there is no predecessor.
  `tools/toolchain/gtk-closure.nix` is compass's own single definition,
  imported only by in-repo consumers (`devenv.nix`, `gtk-e2e-env.nix`,
  `flake.nix`); the flip is self-contained to this repo.
