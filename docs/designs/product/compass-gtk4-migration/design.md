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
linked against the frozen SEA-1172 closure
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
   imported by two consumers so they cannot drift" (devenv.nix:251,
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
  and event plumbing on GTK4, but only under X11/Xvfb (see the Wayland
  tradeoff below). It is not a full runtime proof.
+ **The ledger.** A new DL row records the GTK4 default and amends the
  SEA-1172 closure definition; DL-110 (Wails v3) is untouched. See
  §Ledger delta.

### The honest tradeoffs (why this is a real fork, not a rubber-stamp)

+ **Experimental upstream.** GTK4 support in Wails v3-beta is explicitly
  feedback-stage (wails#4957, "[Feedback Wanted] Experimental GTK4 + WebKitGTK
  6.0 Support"). The pinned `v3.0.0-beta.0` carries the GTK4 files, but GTK4
  fixes have landed in later v3.0.x betas — the migration should ride a Wails
  pin bump (OQ1).
+ **Distro floor bump.** The default GTK4 stack requires Ubuntu 24.04+ /
  Debian 13+; distros shipping only webkit2gtk-4.1 (Ubuntu 22.04 LTS,
  Debian 12, Fedora ≤ 39, RHEL 9.x) can only build the legacy tag
  (v3.wails.io/quick-start/installation). For Compass this bites **source
  builds against system libs and any future non-nix packaging (the A5
  installer follow-up)** — the shipped tarball is immune, because its ELFs are
  store-rpathed against the nix closure and self-contained
  (`compass-native-packaging/design.md:168-169`: "a box with no nix store
  cannot run this tarball" is the existing, unchanged limit).
+ **Closure growth.** The bundle and the e2e runner realize the WebKitGTK
  closure; GTK4 adds gtk4 (+graphene, +gst plugins pulled by webkitgtk_6_0's
  propagations) while dropping gtk3/atk. webkitgtk_6_0 and webkitgtk_4_1 are
  the same 2.52.5 source, so the WebKit half is roughly size-neutral; the
  delta must be MEASURED, not assumed (Plan G2), because it re-opens the
  image/artifact size-budget concern the notes file under SEA-1101 (see the
  grounding caveat in §Ledger delta — the id does not appear in this tree; the
  closest in-tree artifacts are the packaging record's "WebKitGTK-closure size
  per artifact" rejection ground (`compass-native-packaging/design.md:317-318`)
  and `agent-image/devenv.nix:116-127`'s layer-budget machinery, which does
  NOT carry GTK and is unaffected).
+ **Wayland-first runtime behavior.** GTK4 prefers the Wayland backend; the CI
  e2e runs under Xvfb (X11), and T4 pins `GDK_BACKEND=x11` there. GTK4 retains
  the X11 backend, so the gate stays green — but CI then permanently exercises
  a backend real users won't use. Under GTK3 the same X11-only gap existed, yet
  GTK3-on-X11 was the mainstream, decade-hardened path; after the flip the
  *untested* Wayland path becomes the default user path, on a stack upstream
  still calls experimental. Residual risk, mitigated (not closed) by the
  mandatory manual Wayland smoke in T5's test cycle; the weston-headless CI
  fallback is OQ4.

### Alternatives considered

#### Option (a) — stay on gtk3 until Wails v3.1 forces the move (rejected)

Zero work now; the gtk3 variant is supported "through the v3.0.x line". But:
the deadline is upstream-controlled and lands as a forced migration coupled to
whatever else v3.1 changes (worst time to absorb an experimental-stack flip);
every month on GTK3 deepens the SEA-1172 freeze around a stack upstream calls
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
The GTK3 pin lives in the SEA-1172 toolchain-closure freeze
(`gtk-closure.nix:1-3`: "the frozen SEA-1172 closure
(docs/designs/platform/ci-toolchain-shared-defs.md)" — note that path does not
exist in this repo; SEA-1172 is a sealed-monorepo artifact referenced from
here) and in code/CI, not in a compass DL row. Therefore:

+ **No DL row is Superseded.** DL-110 stays Active unchanged (Wails v3 is
  unchanged); DL-214/DL-216 (packaging) stay Active (the bundle shape is
  unchanged, only its closure contents move).
+ **One NEW DL row** (id assigned at freeze): "The Compass Linux shell builds
  Wails' default GTK4 + webkitgtk-6.0 stack (repo tag `gtk4`; closure
  `gtk4`/`webkitgtk_6_0` in gtk-closure.nix), retiring the legacy `gtk3` +
  webkit2gtk-4.1 variant ahead of its Wails v3.1 removal; amends the SEA-1172
  frozen GTK closure definition in place (one definition, all consumers)."
+ **SEA-1172 / sealed-image amendment (merge-blocking, see OQ5)**: the sealed
  toolchain record AND the hand-synced sealed CI step image
  (`devenv.nix:233-235`) both carry the GTK3 closure out of this repo — kept in
  step by hand, so the flip re-triggers exactly the drift the one-definition
  closure prevents in-repo. Amending them is a HARD PREDECESSOR of the T2–T5
  merge (or the coordinator first confirms no sealed lane consumes the
  closure's GTK `-dev` outputs) — not same-wave coordination.

## Global Constraints

+ **Wails floor:** `github.com/wailsapp/wails/v3` ≥ the latest v3.0.x beta at
  implementation time (currently pinned `v3.0.0-beta.0`, `go/go.mod:32`).
  GTK4 is experimental in-beta; the migration PR bumps the pin first (T1) so
  it carries upstream GTK4 fixes. Never v3.1 (that removes gtk3 and lands
  unknown breaking changes; the point is to migrate before it).
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
  DL-214). Docs that name system prerequisites must say so.
+ **Size budget:** the closure delta (bundle tarball + e2e runner realization)
  is measured and recorded in the migration PR body; a regression > ~15% on
  the app-bundle tarball re-opens the size-budget question for Matt before
  merge (SEA-1101 concern; id not resolvable in this tree — see §Ledger
  delta).
+ **markdownlint:** MD004 `+` bullets, MD040 fenced languages, blank lines
  around blocks — this record and any doc edits comply.

## Plan

Ordering: T1 (pin bump) → T2 (closure swap) → T3 (tag flip) → T4 (CI gate) →
T5 (packaging) → T6 (docs + ledger). T2–T5 land as ONE PR (the tag and the
closure are load-bearing together: `-tags gtk4` against a gtk3 closure fails
the cgo link, and vice versa); T1 and T6 may be separate PRs.

**Merge-blocking predecessor (W1/OQ5):** before the T2–T5 PR merges, the
sealed CI step image's hand-synced GTK closure (`devenv.nix:233-235`) and the
out-of-repo SEA-1172 record must be amended to GTK4 — OR the coordinator must
confirm no sealed lane consumes the closure's GTK `-dev` outputs. The flip
drifts the hand-synced image the moment it lands, and that breakage surfaces in
a repo the executors cannot fix.

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
  the new list consumed unchanged by `devenv.nix:251` (PKG_CONFIG_PATH),
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

### T4 — CI e2e lane flip + X11-under-Xvfb verification

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
  diff). Verify GTK4-under-Xvfb: pin `GDK_BACKEND=x11` in the step env (GTK4
  keeps the X11 backend; Wayland preference is runtime-selected). Keep the
  PASS-line guard (`ci.yml:1214`) verbatim.
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
  (mandatory, W2 — the CI gate only ever exercises X11, so a human run of the
  shipped tarball under Wayland before merge is the only proof of the default
  user backend; the dev box can do this); tarball size delta recorded (Global
  Constraint 5).

### T6 — Docs sweep + ledger encode

+ **Do:** sweep remaining `gtk3` prose: `devenv.nix:106,153-160,225-251`
  comments, `gtk-e2e-env.nix:1-27` comments, native-app record's
  system-libs constraint (`compass-native-app/design.md:384-391` names
  `webkit2gtk-4.1` — annotate, don't rewrite frozen prose, per the repo's
  banner-amendment convention seen at `compass-native-app/design.md:25-29`).
  Coordinator encodes the new DL row (§Ledger delta) and files the SEA-1172
  amendment coordination.
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
  GTK4-under-Xvfb verified (GDK_BACKEND=x11 if needed); PASS-line guard kept.
+ [ ] **T5** app-bundle/build.sh + flake.nix tag flip; `nix flake check` +
  bundle smoke green on GTK4.
+ [ ] **T6** Docs sweep + amendment banners; coordinator encodes the new DL
  row and the SEA-1172 amendment.

## Open Questions

+ **OQ1 (load-bearing): Wails pin target.** Which v3.0.x beta to bump to, and
  does Matt accept riding an upstream-experimental GTK4 path now vs waiting
  for upstream to drop the "experimental" label within v3.0.x?
  **Recommendation:** bump to the newest v3.0.x at implementation time and
  proceed — the e2e gate is the safety net, and waiting recreates option (a)'s
  forced-migration risk.
+ **OQ2 (load-bearing): size-budget threshold.** Global Constraint 5 proposes
  "> ~15% tarball growth re-opens the question with Matt". The SEA-1101
  image-layer-budget decision this is said to touch is NOT resolvable in this
  repo (no match for `SEA-1101` in the tree; `agent-image/devenv.nix:116-127`'s
  layer budget carries no GTK). Matt should confirm (i) the actual SEA-1101
  constraint text from the sealed monorepo and (ii) the acceptable growth
  threshold. **Recommendation:** treat the measured delta as a PR-body fact
  gated at 15% until the sealed constraint says otherwise.
+ **OQ3 (non-load-bearing): repo tag name.** `gtk4` (proposed; symmetric,
  self-describing) vs `desktop` (version-neutral, no rename at GTK5).
  **Recommendation:** `gtk4` — it must be spelled at every cgo boundary
  anyway, and a neutral name would hide which Wails variant is selected.
+ **OQ4 (non-load-bearing): Xvfb vs wayland headless in CI.** If GTK4-on-Xvfb
  proves flaky, the fallback is a headless Wayland compositor (`weston
  --backend=headless` or `wlheadless-run`) in the e2e env.
  **Recommendation:** keep Xvfb + `GDK_BACKEND=x11` first (smallest diff to a
  known-good lane); switch only on observed flake.
+ **OQ5 (load-bearing): SEA-1172 / sealed-image amendment sequencing.** The
  frozen toolchain closure is defined in the sealed monorepo's
  `ci-toolchain-shared-defs.md` (referenced at `gtk-closure.nix:3`, not present
  here) AND the sealed CI step image stages the same closure's `-dev` outputs
  "kept in step by hand" (`devenv.nix:233-235`). This is the one cross-repo
  coupling the flip re-triggers: if the sealed image stays GTK3 when T2–T5
  merges, any sealed lane that builds or links compass-app breaks in a repo
  this record's executors cannot fix. Resolve BEFORE the T2–T5 merge: either
  (a) confirm no sealed lane consumes the closure's GTK `-dev` outputs (then
  this demotes), or (b) land the sealed-image + SEA-1172 amendment as a HARD
  PREDECESSOR of that merge. **Recommendation:** (b) unless (a) is confirmed;
  see the Plan's merge-blocking-predecessor constraint.
