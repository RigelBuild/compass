# Compass distribution & install surface

Status: Draft
Linear: RIG-2477 (client app per-OS matrix + install channels), RIG-2608
(headless self-host stack distribution). Scope expansion of RIG-1746 ruled by
Matt 2026-08-24.

> **Companion record, same PR — composes, never duplicates:**
> [`../compass-release-bundling.md`](../compass-release-bundling.md) (the
> binary/image Release lane — per-build `build-<sha12>` prereleases + the
> reserved `v*` semver lane on compass GHA, per-arch binary assets, GHCR image
> by digest, nix-outputs manifest). The two records ship and freeze together
> in one PR because they are tightly coupled: this record CONSUMES that lane as
> its publishing rail and decides what the release-bundling record deliberately
> deferred: its OQ-1 ruling ("the desktop lane (compass-app, compass-stack) is
> DEFERRED … decided in the native-packaging lane", filed as RIG-2477) is
> resolved HERE. (That pointer names the *native-packaging* lane as the
> deciding record; the client-only pivot DL-238 re-homed app distribution into
> THIS record, so the deferral chain is repaired via DL-238 — the decision
> lands here, not in `compass-native-packaging/design.md`.)
> Where this record extends a release-bundling decision (the release asset-set
> boundary, Fork 2(i)), it says so explicitly.

## Problem / Intent

Compass has a release lane (the release-bundling record) but no install surface: the thin client app
(`compass-app`, post client-only pivot DL-235/DL-238) has no per-OS artifacts
and no install channel beyond a Linux tarball built locally, and the headless
self-host stack (`compass-stack` + `compass-server` + `compass-runner` +
postgres) has no distribution home at all — the client-only record's OQ-3
recorded exactly this orphan ("`compass-stack`/`compass-postgres` have no
release-artifact home today",
`../../product/compass-native-client-only/design.md:447-462`). This record
designs both surfaces: the client's full OS matrix (Linux AND macOS together)
with the install channels users actually reach for (homebrew, nix flakes, a
tarball), and the self-hoster's host-level KVM-stack bring-up with postgres
provisioned as a dedicated container image out of the box.

## Decisions encoded (Matt's rulings — not open questions)

Two rulings from Matt (2026-08-24) are the frozen premises of this record;
nothing below re-litigates them:

1. **Client app (RIG-2477): full per-OS matrix + install channels.** The
   client ships on Linux AND macOS together (not Linux-first), and beyond a
   tarball it ships through OS install channels — **nix flakes** and
   **homebrew (especially for macOS)**. This is an app-distribution surface,
   not a bare artifact attach.
2. **Self-host stack (RIG-2608): host-level KVM stack + postgres-as-container.**
   The headless stack STAYS a host-level bring-up on a KVM-capable machine —
   it cannot ship as a plain compose/Swarm config droppable on any host,
   because the microVM runtime hard-requires `/dev/kvm`
   (`../compass-elastic-session-runtime/microvm-runner.md:230-236`, quoted in
   Global Constraint 1). **Postgres moves OUT of the stack's host-process
   tree**: the simple path runs a **dedicated postgres container image** out
   of the box with zero thought; a user can always point the stack at their
   own Postgres instead. This SUPERSEDES DL-217's bundle answer (already
   `Superseded by DL-238`) AND the client-only record's interim OQ-3
   recommendation of "postgres tooling = host prerequisite on a dedicated
   machine" (`compass-native-client-only/design.md:456-460`).

## Global Constraints

1. **The KVM floor is consumed, not designed.** The stack host MUST expose
   `/dev/kvm` to the Runner uid; the runtime hard-fails without it —
   `../compass-elastic-session-runtime/microvm-runner.md:230-233`: "**KVM-absent
   ⇒ hard-fail (D3):** with no container fallback, `/dev/kvm` absence (or any
   preflight failure) aborts Runner startup with an error naming the missing
   capability". Consequence: every stack install channel targets **Linux
   x86_64 with KVM**; there is no macOS or no-KVM stack story, ever, in this
   record.
2. **The publishing rail is the release-bundling record's Release lane.** All downloadable artifacts
   attach to the `build-<sha12>` prerelease / `v*` semver Releases on
   `RigelBuild/compass` GHA (`../compass-release-bundling.md` Forks 1/3);
   this record adds assets and channels, never a second Release mechanism.
   Its posture rules inherit: no PR-event trigger on any workflow holding
   `contents: write`; immutable identities first, moving pointers last;
   `v*` tag creation enforced by ruleset + ancestry guard.
3. **Module path + toolchain.** One Go module
   `github.com/RigelBuild/compass/go` (`go/go.mod:13`), Go pinned via
   `tools/toolchain/versions/go.nix`; builds use the pinned toolchain, never
   a `setup-go` drift (release-bundling record Global Constraint 6).
4. **One version stamp across every stack binary in a bundle/release.** The
   attach check hard-fails on mismatch — `go/cmd/compass-stack/main.go:35-38`:
   "It feeds Deps.ExpectedVersion, so the attach mismatch check compares a
   live server's version against this build's". Every channel MUST install
   stack binaries from the SAME release tag.
5. **macOS does not cross-compile from an ubuntu runner.** The app on macOS
   links the system WebKit framework — `devenv.nix:237-238`: "on macOS the app
   links the system WebKit framework, so the closure is Linux's alone" — so
   the macOS lane runs on a **macOS arm64 runner** (GHA `macos-14`/`macos-15`)
   with cgo against system frameworks.
6. **macOS artifacts are signed + notarized before any PUBLIC channel
   carries them.** Gatekeeper quarantines an unsigned downloaded `.app`/dmg;
   homebrew-cask installs of unsigned apps are a broken first-run. Public
   signing = a Developer ID Application certificate + `codesign --options
   runtime`; notarization = `xcrun notarytool submit` + staple. Secrets (cert
   p12 + App Store Connect API key) live as GHA repo secrets on the release
   workflow only (never a PR-triggered workflow, per Constraint 2). The
   Developer ID identity is GA-gated on Apple Developer Program org enrollment
   (OQ-2 / DL-261): dev/internal builds self-sign in the interim — ad-hoc
   `codesign --sign -` (mandatory on Apple Silicon) or a local self-signed
   cert, neither needing an Apple account — so T2/T3 proceed unblocked and
   only the public-channel T4/T5-cask leg waits on the enrollment secrets.
7. **The postgres major is pinned and matches the repo's parity pin.** The
   dev shell pins bare `postgresql` "for strict parity" (`devenv.nix:123-128`,
   postgresql 18.x at the current lock); the postgres container image is the
   stock upstream `postgres:18` pinned BY DIGEST (OQ-5 / DL-260), the same
   major, so the private cluster's on-disk format never skews between a
   dev-box stack and an installed stack — and the digest pin (not a mutable
   tag) matches the discipline `ci.yml:138-144` already applies to the pgtest
   service image, since generated-tsvector behavior is major-version-sensitive.
8. **Rootless podman is the stack host's container runtime for the postgres
   container.** The stack host already speaks podman (the agent image is
   `podman pull`ed, DL-112) and the repo's container tooling is podman
   throughout; the postgres container reuses it — no docker, no compose file
   as the product interface.
9. **Thin-client bundle content is frozen by DL-238**: `compass-app` + `dist`
   + `.desktop` + LICENSE on Linux (`app-bundle/build.sh:2-5`: "the thin
   CLIENT bundle: the gtk3 shell (compass-app) + the UI dist + the desktop
   file + LICENSE. No sidecar binaries, no postgres tooling"); the macOS
   bundle is the `.app` equivalent (binary + dist inside the app bundle).
   This record distributes that content; it does not reopen its shape.
10. **Scripts with real logic are TypeScript, not bash**
    (`rule://scripts-ts-over-bash`); nix + build orchestration glue may stay
    bash per the `build.sh:7-10` precedent.

## Approach

Two surfaces, one publishing rail.

### S1 — Client app: per-OS matrix

**Linux (x86_64).** The artifact exists: the thin client tarball
`compass-app-<version>-linux-amd64.tar.gz`, store-rpathed against the
devenv.lock-pinned GTK closure (DL-214 stays Active for it;
`app-bundle/build.sh:54-60` builds it with the pinned cc + PKG_CONFIG_PATH).
S1 attaches it to the release-bundling Releases: the release workflow gains an
`app-bundle` build step (or invokes `app-bundle/build.sh`) and uploads the
tarball + its checksum. **This extends the release-bundling record's Fork 2(i) asset boundary** —
that record excluded `compass-app` from v1 "to keep the asset set meaning
something" and deferred it to exactly this record (its OQ-1 ruling). The
honest limit DL-214 named stays: a box with no nix store cannot run the
tarball's rpathed binary — which is precisely why the *channels* below (nix
flake first) matter more than the raw asset on Linux.

**macOS (arm64).** The new target. Three facts shape it:

+ The shell today is gated `//go:build unix && gtk3`
  (`go/cmd/compass-app/main.go:1`) and the non-gtk3 entrypoint is an
  exit-1 stub (`main_nogtk3.go:24-28`); GTK3 is "the only Linux stack
  available in this repo's frozen toolchain" (`main_nogtk3.go:5-7`). A darwin
  build therefore needs a **build-tag restructure**: the Wails v3 shell
  compiled on darwin without the gtk3 tag (Wails selects the system WebKit
  backend on darwin), i.e. the gate becomes
  `(linux && gtk3) || darwin` on the real entrypoint, with the stub covering
  the remaining untagged-linux module build. The client-only pivot already
  removed every Linux-only runtime dependency from the app (the runner was
  the Linux-only piece, `compass-native-client-only/design.md:216-220`:
  "the Linux-only-ness was the runner's … and the runner no longer ships in
  the app"), so this is a build-surface change, not a feature port.
+ No cross-compile (Global Constraint 5): a `macos-14` (arm64) GHA job in the
  release workflow builds the `.app` bundle — `Compass.app/Contents/MacOS/
  compass-app` + `Contents/Resources/dist/` (the same beside-the-executable
  dist resolution the shell already does, `main.go:313-321` per the packaging
  record) + `Info.plist` — then signs, notarizes, staples, and wraps it in a
  `.dmg` (the homebrew-cask-native format).
+ Signing is a hard gate for the public channels (Global Constraint 6); the
  lane lands in two steps — unsigned ad-hoc build proving the runner + bundle
  (T3), then signing/notarization (T4) — so the mac build isn't blocked on
  Apple-account provisioning.

Arch matrix: `linux-amd64` + `darwin-arm64` only, matching the release-bundling record's darwin
choice for the CLI ("shipping darwin daemon builds would assert a support
surface nothing consumes" — same logic here for darwin-amd64; OQ-4).

### S2 — Client app: install channels

Three channels in priority order; each consumes the SAME release assets
(never rebuilds from divergent sources):

1. **Homebrew (macOS-first, the ruling's emphasis).** A
   `RigelBuild/homebrew-tap` repo carrying:
   + a **cask** `compass` installing the notarized `.dmg` (macOS app), and
   + a **formula** `compass-cli` installing the per-arch CLI binaries the release-bundling record
     already attaches (`compass_<tag>_darwin-arm64` / `linux-amd64`) — so
     `brew install rigelbuild/tap/compass-cli` works on macOS AND Linux with
     zero new build machinery.
   The semver (`v*`) release workflow bumps the tap automatically (a small
   step templating version + sha256 into the cask/formula and pushing to the
   tap repo with a tap-scoped fine-grained PAT — the one new standing
   *write* credential to an external repo this record adds, scoped to
   `RigelBuild/homebrew-tap` contents only; the Apple signing secrets
   Constraint 6 / T4 add are read-only material consumed on the release
   workflow, not push credentials).
   Per-build prereleases do NOT bump the tap: brew users track semver.
2. **Nix flake.** The compass repo gains a `flake.nix` (it has none today —
   the repo is devenv-based, `devenv.yaml:1-9`) exposing packages:
   `compass` (CLI), `compass-app` (Linux gtk3 client, reusing
   `tools/toolchain/gtk-closure.nix` — the same 13-package set the bundle
   links against), and the stack set (S3). `nix profile install
   github:RigelBuild/compass#compass-app` (or a NixOS/home-manager module
   consuming the flake) is the first-class Linux channel — it sidesteps
   DL-214's no-nix-store limit entirely, because the flake install IS the nix
   store. The flake pins nixpkgs to the devenv.lock revision (the
   `gtk-e2e-env.nix:9-13` precedent: "Pins nixpkgs to the SAME devenv.lock
   revision the dev shell and gate-tools.nix resolve") so flake-built and
   bundle-built binaries link one closure.
3. **Tarball / dmg direct download.** The raw release assets (S1) remain the
   channel of last resort and the substrate the other two wrap.

Native Linux distro packages (deb/rpm/AUR) are explicitly deferred (OQ-1):
flake + brew-on-Linux + tarball cover the Linux install matrix without
taking on per-distro packaging debt.

### S3 — Self-host stack: host-level KVM bring-up

The stack stays exactly what the client-only record kept it as: "the
standalone headless single-user bring-up CLI"
(`compass-native-client-only/design.md:112-119`), running host-level because
the Runner opens `/dev/kvm` and drives cloud-hypervisor/virtiofsd/passt as
ordinary user processes (`devenv.nix:198-201`: "All three are ordinary user
binaries that open(2) /dev/kvm but need no capability or device node of their
own; the host-level /dev/kvm enablement is a separate, out-of-repo concern").
A Docker-Swarm/compose packaging of the stack is rejected structurally: it
would either demand privileged `/dev/kvm` device passthrough into a container
(a worse posture than host processes) or silently lose the microVM boundary —
and D3 forbids degrading (Global Constraint 1).

Distribution, three legs:

+ **Release binaries.** `compass-stack` joins the release-bundling binary asset set
  (linux-amd64), **extending Fork 2(i)'s boundary** the same way S1 does for
  the app: the v1 exclusion of `compass-stack` was the OQ-1 deferral this
  record resolves. `compass-server`/`compass-runner` are already attached.
  `compass-postgres` (the Go wrapper) is NOT attached as a host binary — it
  moves into the postgres container image (S4), which is its only shipped
  home; the host-level LookPath spawn remains only for the dev-box/devenv
  path.
+ **Nix flake (the recommended channel).** The same `flake.nix` (S2 leg 2)
  exposes `compass-stack`, `compass-server`, `compass-runner`, and a
  `compass-stack-env` convenience (the microVM userspace trio —
  cloud-hypervisor, virtiofsd, passt — at the devenv.lock pin, which the
  Runner preflight version-floors: `devenv.nix:204-206`: "the shell provides
  one pinned version from devenv.lock; the runtime preflight (V2a+) will
  enforce the floor"). A NixOS module (`services.compass-stack`) wrapping
  `compass-stack up` in a systemd unit is the polished endgame for this leg
  (OQ-3 sizes it).
+ **Bring-up UX.** `compass-stack up` stays the one entry point; this record
  adds (a) a host preflight surfacing the KVM/podman prerequisites at
  install-time rather than first-`up` (its own minimal checks — no Runner
  preflight function exists to reuse today; T9 carries the dependency
  honesty), and (b) an operator doc
  (`docs/self-host.md`) covering the two supported shapes — dedicated
  KVM machine, or one-box localhost-TLS (sanctioned by the client-only
  record's OQ-6 ruling).

### S4 — Postgres-as-container (the DL-217 supersession)

> **T8 errata (RIG-2759, PR #652) — additive; the frozen §S4 prose below is
> intact.** T8's container-backed adapter ratified two contract-fidelity
> deviations from the literal run contract below, both preserving the frozen
> socket-only / `trust`-auth / byte-identical-DSN invariant:
>
> + **`POSTGRES_USER=<os-user>` is set in the container env** (the env list at
>   §S4 below names only `POSTGRES_DB=compass` and
>   `POSTGRES_HOST_AUTH_METHOD=trust`). Under `--userns=keep-id` the container
>   runs as the host OS user, and the frozen DSN is user-less
>   (`host=<dir> port=<p> dbname=compass sslmode=disable` — no `user=`), which
>   pgx resolves to that OS user. The stock `postgres:18` image otherwise
>   bootstraps `POSTGRES_USER=postgres`, so a user-less DSN would fail
>   (`role "<osuser>" does not exist`); setting `POSTGRES_USER=<os-user>` makes
>   the created superuser role match the DSN identity §S4 froze. Carries an
>   in-code fork note at `go/internal/stack/adapters/postgres_container.go:43-52`
>   (set at `runArgs`, `postgres_container.go:175`).
> + **`unix_socket_directories` lists BOTH the image's compiled-in
>   `/var/run/postgresql` AND the bind-mounted DSN socket dir**, where §S4 below
>   points the server's socket dir at the DSN dir only. The stock image's
>   entrypoint bootstrap (`docker_temp_server_start`) connects its setup psql
>   over the compiled-in `/var/run/postgresql` with `PGHOST` unset; dropping
>   that dir wedges the entrypoint's initdb/createdb before it ever binds the
>   DSN socket. So the run contract lists both
>   (`postgres_container.go:180`, `defaultSocketDir` at
>   `postgres_container.go:34`); `compass-server` still opens only the DSN dir,
>   bind-mounted host↔container at the identical path
>   (`postgres_container.go:178`) — the frozen `host=<socket-dir>` DSN is
>   unchanged.

**Decision (Matt's ruling 2):** the simple path provisions postgres as a
dedicated container image the stack runs out of the box; a user-supplied DSN
opts out entirely.

This ratifies the dominant self-hosted convention — GitLab's omnibus model:
a server product that hard-requires postgres (no embedded-SQLite escape)
bundles a zero-config postgres by default and exposes a clean external opt-out
(`postgresql['enable'] = false` + a connection DSN), reserving mandatory-external
for the managed/enterprise tier. Immich (bundled custom postgres image) and
Sentry self-hosted (bundled docker-local postgres, documented external opt-out)
follow the same shape. `--database-external` is our opt-out; the bundled
container is our zero-config default. The corollary for future external deps
(Redis/NATS, if ever): bundle each by default with its own opt-out — the
compounding-standup-pain risk comes from making deps BYO, not from having them.

Mechanics, grounded in the current seams:

+ **The image (Matt's OQ-5 ruling): the stock upstream `postgres` image,
  pinned to `postgres:18` by digest — NOT a nix2container build of nixpkgs
  `postgresql`, and NOT a custom wrapper-entrypoint image.** nix2container is
  the repo's mechanism for the NixOS+devenv agent shell, not for bundling a
  stock third-party service; the community `postgres` image is the boring,
  correct base. `18` is the current stable major (19 is beta until ~GA). The
  pin is by DIGEST, not tag — the same discipline `ci.yml:138-144` applies to
  the pgtest service image, because a mutable tag would ship unreviewed
  database behavior and generated-tsvector / `websearch_to_tsquery` behavior
  is major-version-sensitive. The official entrypoint already does what the
  `compass-postgres` wrapper does on the host path — initdb-if-needed +
  createdb + a SIGTERM smart-shutdown drain — so on the container path the
  wrapper collapses into it, configured by environment: `POSTGRES_DB=compass`
  (the createdb), `POSTGRES_HOST_AUTH_METHOD=trust` (the loopback-only
  private-store posture, `compass-postgres/main.go:16-18`), PGDATA on the
  bind-mounted `<StateDir>/postgres` volume, and `unix_socket_directories`
  pointed at the bind-mounted DSN socket dir so `compass-server` opens the
  identical `host=<socket-dir>` DSN. The `compass-postgres` wrapper stays the
  host/dev-path (`ProcessSupervisor` LookPath) bring-up unchanged; only the
  container path swaps it for the stock entrypoint + env. No custom image
  build lane is needed: the release body references the upstream digest
  directly (an optional GHCR mirror for pull-reliability is a convenience,
  not a build — the same way the agent image is `podman pull`ed, DL-112).
+ **The supervisor seam.** The stack starts postgres through
  `stack.ProcessSupervisor` today (`go/internal/stack/stack.go:193-196`:
  `Component: ComponentPostgres, Args: ["--state-dir", …, "--database", …]`),
  resolved by bare name on PATH (`adapters/process.go:31-44`). S4 adds a
  **container-backed postgres adapter**: when configured with a postgres
  image ref (default: the pinned `compass-postgres` image), the supervisor
  runs `podman run` with the state dir and the DSN's socket directory
  bind-mounted, instead of LookPathing a host binary. The DSN shape is
  unchanged — `host=<socket-dir> port=<port> dbname=compass sslmode=disable`
  (`compass-postgres/main.go:9-11`) — because the unix socket directory is
  bind-mounted host↔container, so `compass-server` opens the identical DSN
  and the cluster stays loopback-free and network-invisible (the
  "socket-only (no TCP), trust auth on the local socket" posture,
  `main.go:16-18`, survives containerization intact). *Readiness* reuses the
  existing probe unchanged: `waitPostgres` polls the full DSN
  (`stack.go:306-311`: "polls DBProber.ProbeDB until postgres accepts
  connections on the full DSN") and the bind-mounted socket is
  byte-identical to it.
+ **Teardown is NOT process-shaped — the container needs its own identity
  (pgid format v2).** The naive claim "the adapter satisfies `stack.Process`
  so teardown needs no change" is FALSE for the fresh-`down` (linger) path,
  and this record designs the fix rather than papering over it. Today's
  teardown identity is a *process-group* record: `recordChild` persists
  `pgidEntry{Component, Pgid, StartTime}` with "pgid == pid, plus the leader
  start-time token read at spawn" (`go/internal/stack/stack.go:258-269`;
  `pgidfile.go:30-34`), and a fresh `compass-stack down` reads
  `<StateDir>/stack.pgids` and tears each entry down by group signal —
  `syscall.Kill(-pgid, sysSig)` (`adapters/groupsignal.go:54`). But a
  `podman run` client's `Pid()` does not describe the container: under
  rootless podman the containerized postgres runs beneath `conmon`,
  *outside* the podman-client's process group, so group-signalling the
  recorded pgid would orphan the container — postgres survives,
  `DownDetached`'s socket-quiescence confirm ("postgres stops accepting on
  the DSN socket", `downdetached.go:188-191`) reports a genuine survivor,
  and `down` fails while leaking the container. **Resolution (Matt ruled
  OQ-7 2026-08-25, DL-262 — the contract change is blessed):** the containerized
  postgres stays a *supervised stack component* — preserving the
  one-command up/down lifecycle that is the reason postgres is a supervised
  child at all — and the pgid record format grows a first-class **container
  teardown identity**:
  + **Format v2.** `pgidFileVersion` bumps `"1"` → `"2"`
    (`pgidfile.go:20-22`, the frozen DL-183 contract). Entries become a
    discriminated union, tagged by a kind field on the entry line:
    + *process entry* — today's `{Component, Pgid, StartTime}`, torn down by
      group signal exactly as now (`proc <component> <pgid> <starttime>`);
    + *container entry* — `{Component, ContainerName}`, torn down by
      `podman stop -t <budget> <name>` with `podman rm -f <name>` as the
      SIGKILL-tier escalation (`ctr <component> <name>`).
    `readPgidFile` dispatches on the tag; the hard-error-on-malformed
    discipline is unchanged ("signaling off a half-understood record is
    exactly the blast radius the design forbids", `pgidfile.go:100-103`).
  + **Cross-version rule.** A v1-only binary never half-parses a v2 record —
    but by the *entry-line grammar*, not a header-version check: shipped v1
    `readPgidFile` stores `header[0]` as `Version` and never compares it to
    `pgidFileVersion` (`pgidfile.go:124`), so the refusal comes from
    `parsePgidLine` hard-erroring on a v2 entry line (a `proc` line is 4
    fields where v1 demands exactly 3, `pgidfile.go:144`; a `ctr` line's
    leading token is not a known component, `pgidfile.go:149`) under the same
    hard-error-on-malformed discipline ("signaling off a half-understood
    record is exactly the blast radius the design forbids"). The T8a
    `refuse-unknown-version` clause is the *forward* guard — it protects a v2
    reader from a future v3, not v1 from v2. A v2 binary reads v1 records as
    all-process entries (v2 is a strict superset). In practice the window is
    nil: Global Constraint 4's one-version-stamp invariant means every
    channel installs a matched build set, so the up that wrote the record
    and the down that reads it are the same build — but the rule is stated
    so a mixed-build accident degrades to a legible refusal, not a blind
    signal.
  + **`DownDetached` dispatch.** `liveTargets`/`drainTargets` dispatch per
    entry kind: process entries keep the identity-checked group-signal path;
    the container entry's SIGTERM tier is `podman stop` (bounded by the
    existing `postgresDrainBudget`, `downdetached.go:31`) and its SIGKILL
    tier is `podman rm -f`. The socket-quiescence confirm survives intact:
    the socket dir is bind-mounted from the host, so the DSN socket goes
    quiet when the container's wrapper stops — the confirm channel needs no
    change, only the signal delivery does.
  + **Stable container name.** The name is derived from the state dir alone
    (e.g. `compass-postgres-<short-hash(StateDir)>`), so a fresh `down` with
    no in-memory handle reconstructs it from config — and it is *also*
    persisted in the v2 container entry, which is the authoritative copy the
    teardown uses (derivation is the collision-avoidance scheme for
    concurrent state dirs, the record is the teardown identity).
+ **The container run contract (interface, not implementation).** Two seams
  break the zero-thought default if left unstated, so they are pinned here:
  + *uid mapping.* postgres refuses to run as uid 0, and the bind-mounted
    `<StateDir>/postgres` data dir is created 0700 host-user-owned by the
    wrapper's initdb path; under rootless podman the container root maps to
    the host user, so the run MUST use `--userns=keep-id` (host uid ↔ same
    uid in-container) with the image running the wrapper as that non-root
    user — otherwise the data dir is unreadable across the uid map.
  + *stop timeout — two distinct knobs, not one.* postgres never force-kills
    itself: the wrapper's drain grace is 30s —
    `compass-postgres/main.go:256-259`: "shutdownGrace bounds how long we
    wait after forwarding SIGTERM … We never force-kill; escalation is the
    supervisor's job" (`const shutdownGrace = 30 * time.Second`) — while
    `podman stop`'s default SIGKILLs after 10s. So (1) the `podman run` pins
    `--stop-timeout` ≥ the 30s wrapper grace, the safe default for any stop
    that passes no explicit `-t` (podman never hard-kills before the wrapper
    would). (2) The detached-`down` teardown path instead passes an explicit
    `podman stop -t <postgresDrainBudget>` = 10s (`downdetached.go:31`): this
    is deliberate behavior parity with today's process model, where the
    detached path already caps the postgres drain at `postgresDrainBudget`
    and group-SIGKILLs at that bound rather than waiting the wrapper's full
    30s. The two knobs serve different callers; they are not equal and must
    not be conflated.
  + *mounts.* `<StateDir>/postgres` (data dir) and the DSN's socket dir are
    bind-mounted read-write; nothing else from the host is mounted.
+ **Bring-your-own postgres.** `Config.DatabaseDSN` is already caller-provided
  (`go/internal/stack/config.go:29-31`); a new `--database-external` (or
  DSN-shape detection: a `host=` pointing outside the state dir) makes the
  supervisor skip the postgres component entirely and just `waitPostgres` the
  given DSN. Zero-thought default = container; escape hatch = your DSN.
+ **Dev-box path unchanged.** devenv keeps providing host postgres tooling
  for the e2e suites (`devenv.nix:114-128`); the container is the *installed*
  stack's default, selected by config, not a repo-wide replacement of the
  process adapter.

## Alternatives considered

+ **Compose/Swarm-packaged stack** — rejected in S3: `/dev/kvm` + microVM
  userspace as host processes is the designed boundary
  (`microvm-runner.md:230-236` hard-fail; `devenv.nix:198-201`); a
  containerized Runner needs privileged device passthrough and still can't
  degrade (D3).
+ **Postgres as a host prerequisite** (the client-only OQ-3 interim
  recommendation) — superseded by Matt's ruling: "install postgres 18
  yourself" fails the zero-thought bar; the container path is one `podman
  pull` the stack performs itself, on a host that already runs podman
  (DL-112).
+ **Vanilla `docker.io/postgres` image + wrapper on the host** — rejected:
  it splits the private-cluster brain (wrapper) from the server binary
  (postgres) across a host/container seam, reintroducing the host binary the
  ruling removes; the dedicated image keeps wrapper+postgres one artifact
  with one version.
+ **TCP loopback instead of bind-mounted socket for the container cluster** —
  rejected: it would change the DSN contract, expose a port, and forfeit the
  socket-only/trust-auth posture (`compass-postgres/main.go:16-18`); the
  bind-mount keeps every consumer byte-identical.
+ **systemd/quadlet-managed postgres container outside the child tree** (the
  stack only probes the DSN, never owns the container) — rejected: it would
  sidestep the pgid-format-v2 change (the container would carry no stack
  teardown identity at all), but at the cost of adding unit provisioning
  (a quadlet file, `systemctl --user` wiring, lingering) to the
  zero-thought install story and breaking the stack's
  up-starts-all / down-tears-all lifecycle — `compass-stack up` would bring
  up a stack whose database it neither started nor can stop. The supervised
  container + v2 teardown identity keeps the one-command lifecycle; the
  format change it costs is the DL-262 pgid-v2 extension Matt blessed (OQ-7).
+ **homebrew-core / distro-official packages** — rejected for v1: core/distro
  inclusion has review latency and policy floors (notarization, popularity)
  that a young project fails; an org tap ships today and migrates later
  without user-visible change beyond the tap prefix.
+ **goreleaser for the matrix** — re-rejected (same grounds as the release-bundling record's Fork
  2(i)): tag-driven model fights the per-build lane; the repo convention is
  dependency-free glue.
+ **Electron-style auto-update in the app** — out of scope; deferred with
  macOS follow-ups per `compass-native-app/design.md:275-277` ("Deferred to
  follow-up issues: … signed installers with auto-update"). Channels here are
  pull-based (brew upgrade / nix profile upgrade / re-download).

## Plan

Dependency order: T1 → T2 → T3 → T4 → T5; T6 → T7 → T8; T8a → T8
(T8a is the pgid-format-v2 teardown identity T8's fresh-`down` needs, per the
DL-262 ruling); T9 after T6+T8
(T9's preflight ships its own checks — see T9 — so it does not block on the
runtime lane); T10 last. S1/S2 (client) and S3/S4 (stack) are independent
lanes until T9. **Cross-lane impl order:** T1-T5 hard-depend on the
release-bundling record's T1 impl landing first — T1 edits
`.github/workflows/release.yml`, which is that record's T1 deliverable and does
not exist until its impl lane merges. (Both records freeze together in this
PR; the dependency is between their *impl* lanes, not their design records.)

### T1 — Linux client bundle joins the Release asset set

+ **Do:** extend `.github/workflows/release.yml` (the release-bundling record's T1 workflow): a
  step running `app-bundle/build.sh` (nix + gtk3 link, per its own header)
  and uploading it renamed to the release-bundling asset grammar
  `compass-app_<tag>_linux-amd64.tar.gz` (the produced asset, see Interfaces)
  to the same Release;
  fold its checksum into `SHA256SUMS`. Trigger paths gain `app-bundle/**`,
  `apps/ui/**`, `tools/toolchain/gtk-closure.nix`, `devenv.lock` (the bundle's
  moon inputs, packaging record §A4).
+ **Interfaces:** consumes `app-bundle/build.sh` (existing; emits the tarball
  in `app-bundle/`), the release workflow's tag + upload step. Produces one
  new asset per Release: `compass-app_<tag>_linux-amd64.tar.gz` (renamed to
  the release-bundling asset grammar `<name>_<tag>_<os>-<arch>`).
+ **Test cycle:** a main push touching `app-bundle/**` mints a prerelease
  whose tarball unpacks, `bin/compass-app --version` prints the stamp, and
  the DL-238 smoke (`app-bundle/SMOKE.md`) passes from the downloaded asset.

### T2 — Darwin build path for compass-app

+ **Do:** restructure the shell's build tags so darwin compiles the real
  entrypoint AND does not double-compile the non-gtk3 stubs. The stub pairs
  are load-bearing: each gtk3 file that gains darwin forces its `!gtk3` stub
  pair to narrow to `linux && !gtk3` — otherwise a darwin build (which is
  `unix` with no gtk3 tag) compiles BOTH the retagged real file and its
  `unix && !gtk3` stub, a duplicate-symbol compile break (e.g.
  `windowFromContext` is defined in `bridge_service_window_gtk3.go:28` AND
  `bridge_service_window_nogtk3.go:19`). Full per-file table (current tags
  verified against every `go/cmd/compass-app/*.go:1` header):

  | File | Before | After |
  | --- | --- | --- |
  | `main.go` | `unix && gtk3` | `(linux && gtk3) \|\| darwin` |
  | `client.go` | `unix && gtk3` | `(linux && gtk3) \|\| darwin` |
  | `window_set.go` | `unix && gtk3` | `(linux && gtk3) \|\| darwin` |
  | `bridge_service_window_gtk3.go` | `unix && gtk3` | `(linux && gtk3) \|\| darwin` |
  | `main_nogtk3.go` (stub pair of `main.go`) | `unix && !gtk3` | `linux && !gtk3` |
  | `bridge_service_window_nogtk3.go` (stub pair of `bridge_service_window_gtk3.go`) | `unix && !gtk3` | `linux && !gtk3` |
  | `bridge_service.go` | `unix` | `unix` (unchanged — compiles on darwin already) |
  | `version.go` | untagged | untagged (unchanged) |
  | `client_test.go`, `window_set_test.go`, `window_name_test.go`, `multiwindow_e2e_test.go`, `multiwindow_e2e_helpers_test.go` | `unix && gtk3` | `unix && gtk3` (unchanged — Linux-only suite, see note) |
  | `bridge_service_test.go`, `bridge_service_connect_test.go` | `unix` | `unix` (unchanged) |

  Wails v3 selects the system-WebKit backend on darwin (no pkg-config, no
  gtk closure — `devenv.nix:110-111`: "harmless on macOS, where the app
  links the system WebKit framework and pkg-config goes unused"). Verify
  the keychain tokenstore path (DL-109) uses the darwin keychain backend.
  **Honesty note:** the gtk3-tagged test suite stays `unix && gtk3` — the
  darwin binary ships with ZERO of the shell's gtk3-tagged tests compiled
  for it; darwin coverage is the manual launch smoke (below) plus whatever
  the untagged/`unix` suites exercise.
+ **Interfaces:** consumes the existing gtk3-tagged shell sources + Wails v3
  (`v3.0.0-beta.0`, `go/go.mod:29` per the packaging record). Produces a
  darwin-arm64 `compass-app` binary buildable on a mac with
  `go build -o compass-app ./cmd/compass-app` (cgo, system frameworks, no
  tag).
+ **Test cycle:** `go build ./...` (untagged, on Linux) still green — this
  is what a Linux box CAN verify; a full darwin compile is NOT runnable on
  Linux (the darwin entrypoint's cgo needs the macOS SDK), so the real
  darwin compile gate is T3's `macos-14` job (cadence per DL-263: main +
  nightly always, plus a moon-affected-gated PR leg). On a mac: the build
  launches, loads `dist`, and completes the
  client connect flow against a live stack.

### T3 — macOS app bundle + CI lane (unsigned)

+ **Do:** a `scripts/macos-bundle.ts` (TypeScript per Global Constraint 10 —
  it templates Info.plist, stages `Compass.app/Contents/{MacOS,Resources}`,
  ad-hoc signs `codesign -s -`, wraps a `.dmg` via `hdiutil`); a
  `macos-14` job in `release.yml` building it per Release (paths-gated like
  T1). Asset: `compass-app_<tag>_darwin-arm64.dmg`.
+ **Interfaces:** consumes T2's darwin binary + `apps/ui` dist (built by
  `bunx vite build`, `apps/ui/moon.yml:19-23` per the packaging record).
  Produces the `.app`-in-`.dmg` asset (ad-hoc signed; NOT yet a public
  channel input — T4 gates that).
+ **Test cycle:** the CI-built dmg mounts, the app launches on a mac
  (quarantine cleared manually — expected pre-notarization), connects to a
  stack, board renders.

### T4 — macOS signing + notarization

+ **Do:** add Developer ID signing to T3's lane: import the cert p12 from a
  GHA secret into a throwaway keychain, `codesign --options runtime
  --timestamp` the app, `xcrun notarytool submit --wait` with an App Store
  Connect API-key secret, `xcrun stapler staple`, re-wrap the dmg and sign
  it. Secrets scoped to the release workflow (no PR trigger exists on it —
  Global Constraint 2).
+ **Interfaces:** consumes T3's bundle step + three GHA secrets
  (`MACOS_CERT_P12`, `MACOS_CERT_PASSWORD`, `NOTARY_API_KEY`). Produces a
  Gatekeeper-clean dmg: `spctl -a -t open --context context:primary-signature`
  passes; first launch shows no quarantine dialog.
+ **Test cycle:** download the release dmg on a clean mac (no dev tools),
  drag-install, launch — zero Gatekeeper friction. `codesign -dv` +
  `stapler validate` in CI as the mechanical gate.
+ **Prerequisite:** an Apple Developer Program membership + Developer ID
  certificate (OQ-2 — the one human-action prerequisite in this record).

### T5 — Homebrew tap + auto-bump

+ **Do:** create `RigelBuild/homebrew-tap`: `Casks/compass.rb` (installs the
  T4 dmg) + `Formula/compass-cli.rb` (installs the release-bundling CLI binaries,
  per-arch url/sha blocks). Add a semver-lane step to `release.yml` that
  renders both files from templates (version + asset sha256s) and pushes to
  the tap via a fine-grained PAT scoped to that one repo (`TAP_PUSH_TOKEN`).
+ **Interfaces:** consumes the `vX.Y.Z` Release's asset URLs + `SHA256SUMS`.
  Produces `brew install rigelbuild/tap/compass` (macOS app) and
  `brew install rigelbuild/tap/compass-cli` (macOS/Linux CLI).
+ **Test cycle:** `brew install` + `brew upgrade` against a cut `v*` release
  on both a mac and a Linux box; `brew audit --cask compass` clean; a
  prerelease `build-*` demonstrably does NOT bump the tap.

### T6 — Nix flake

+ **Do:** add `flake.nix` at the repo root: inputs pinned to the devenv.lock
  nixpkgs revision (the `gtk-e2e-env.nix:9-13` single-pin discipline);
  packages `compass`, `compass-server`, `compass-runner`, `compass-stack`
  (buildGoModule over `go/`, `-ldflags -X main.version=` stamped from the
  flake's `self.rev`), `compass-app` (Linux: the gtk3 cgo build against
  `gtk-closure.nix`), and `compass-stack-env` (cloud-hypervisor + virtiofsd
  + passt at the pinned rev). **Pin-parity gate:** a flake has its OWN
  `flake.lock`, so the repo would carry TWO independent nixpkgs locks — the
  "one closure" claim (flake-built ≡ bundle-built binaries) holds only if
  they resolve the same rev, and nothing enforces that by construction. So
  T6 adds a named parity check (the `parity.ts` discipline the repo already
  runs — `devenv.nix:118-119`: CI's gate-tools are "fed by `parity.ts
  --print-nix-attrs` off THIS list") verifying `flake.lock`'s nixpkgs rev ==
  `devenv.lock`'s nixpkgs rev, failing CI on skew. A CI check (`nix flake
  check` + the parity check, moon-registered, affected-gated on
  `flake.nix`/`flake.lock`/`devenv.lock`/`go/**` — `devenv.lock` is in the
  trigger set precisely because a devenv pin bump is the event that causes
  the drift) keeps it green.
+ **Interfaces:** consumes `go/`, `tools/toolchain/gtk-closure.nix`,
  `devenv.lock`'s nixpkgs rev. Produces
  `nix profile install github:RigelBuild/compass#<pkg>` for every package;
  `nix run .#compass-stack -- status` works from a bare checkout.
+ **Test cycle:** `nix build .#compass-stack .#compass-app` from a clean
  clone; the built `compass-stack --version` matches the flake rev stamp;
  the four stack binaries carry ONE stamp (Global Constraint 4); the parity
  check red on a deliberately skewed `flake.lock`, green after `nix flake
  lock --override-input` back to the devenv.lock rev.

### T7 — Pin the postgres container image (stock `postgres:18` by digest)

+ **Do (Matt's OQ-5 ruling):** pin the stock upstream `postgres:18` image by
  digest as the default `PostgresImage` — NOT a nix2container or custom
  wrapper-entrypoint build. Record the digest beside the pgtest image pin so
  the two stay legible together, and document the bump procedure (advance the
  digest when the postgres minor/major moves, re-running the T8 integration
  test). No `publish-*-image` workflow: there is no image to build. An
  OPTIONAL GHCR re-tag/mirror of the pinned digest (for pull-rate reliability
  on self-host hosts) may ride the release lane later; it copies, never
  builds, and is not load-bearing for v1.
+ **Interfaces:** produces the pinned `postgres:18@sha256:…` reference the S4
  container-backed adapter (T8) runs, configured by the env contract in S4
  (`POSTGRES_DB`, `POSTGRES_HOST_AUTH_METHOD=trust`, PGDATA volume,
  `unix_socket_directories`). Consumes nothing from `go/` — the stock image
  needs no compass code on the container path.
+ **Test cycle:** `podman run` the pinned image with a tmp state dir + a
  socket-dir bind-mount and the S4 env; `psql 'host=<socket-dir>
  dbname=compass'` connects; a forwarded SIGTERM drains cleanly (postgres
  smart-shutdown).

### T8 — Stack: container-backed postgres component + external-DSN opt-out

+ **Do:** in `go/internal/stack`: a container-backed postgres start path —
  config gains `PostgresImage string` (default: the pinned stock
  `postgres:18` digest, T7; empty on the dev path = today's
  `ProcessSupervisor` LookPath spawn of the `compass-postgres` wrapper) and
  `ExternalDatabase bool` (skip the postgres component, probe
  `Config.DatabaseDSN` as-is). The container adapter runs the S4 run
  contract (`--userns=keep-id`; the S4 env — `POSTGRES_DB=compass`,
  `POSTGRES_HOST_AUTH_METHOD=trust`, PGDATA + `unix_socket_directories` on
  the bind-mounts; `--stop-timeout` covering postgres's smart-shutdown drain;
  bind-mounts of `<StateDir>/postgres` and the DSN's `host=` socket dir) with
  the stable per-state-dir container name (S4). Readiness is unchanged:
  `waitPostgres` probes the full DSN (`stack.go:306-311`) over the
  bind-mounted socket.
  Teardown for the *attached/in-process* stop path maps to `podman stop`;
  the fresh-`down` (linger) path is NOT covered by the `stack.Process`
  contract and is T8a's pgid-format-v2 work — T8 depends on T8a for a
  correct `down`. `compass-stack up` flags: `--postgres-image`,
  `--database-external`.
+ **Interfaces:** consumes T7's image ref, T8a's container-entry teardown,
  `Config.DatabaseDSN` (`config.go:29-31`), rootless podman on the host
  (Global Constraint 8). Produces: default `compass-stack up` on a clean
  KVM host brings up containerized postgres with zero flags;
  `--database-external --database <dsn>` runs against user postgres.
+ **Test cycle:** unit tests on the config/dispatch logic (fake supervisor);
  a podman-tagged integration test mirroring
  `compass-stack/cross_process_podman_test.go`'s harness: up → probe DSN →
  fresh-process `down` → container gone (`podman ps -a` empty) → pgid file
  removed; the external-DSN path green against a throwaway host postgres.

### T8a — pgid record format v2: container teardown identity

+ **Do:** implement the S4 teardown design in `go/internal/stack`:
  `pgidFileVersion` `"1"` → `"2"` (`pgidfile.go:20-22`), the discriminated
  entry kinds (`proc <component> <pgid> <starttime>` /
  `ctr <component> <name>`), `readPgidFile` v1-compat (v1 records parse as
  all-process entries) + refuse-unknown-version, `recordChild` gains a
  container-entry variant, and `DownDetached`'s `liveTargets`/`drainTargets`
  dispatch per kind — container entries: liveness by `podman container
  exists <name>`, SIGTERM tier `podman stop -t <budget> <name>`, SIGKILL
  tier `podman rm -f <name>`, confirm unchanged (DSN socket quiescence,
  `downdetached.go:188-191`).
+ **Interfaces:** consumes the DL-183 pgid file seams (`pgidfile.go`,
  `downdetached.go`) and podman on the host. Produces: a v2 record format;
  `writePgidFile`/`readPgidFile` round-tripping both entry kinds; a fresh
  `down` that tears down a containerized postgres by name. Implements the
  DL-262 format extension (OQ-7, blessed by Matt 2026-08-25).
+ **Test cycle:** unit tests: v2 round-trip both kinds; a v1 record parses
  (compat); an unknown-version record refuses legibly (the
  `ErrNoTeardownRecord` posture, `downdetached.go:47-51`); the existing
  `pgidfile_test.go` prefix/atomicity suites green on v2. Integration:
  T8's up → fresh down → container gone.

### T9 — Self-host bring-up surface: preflight + docs

+ **Do:** `compass-stack preflight` (or fold into `up`'s first phase): KVM
  openable, podman present + rootless-capable, microVM userspace trio found
  at/above floors, legible per-check pass/fail. **Dependency honesty:** the
  Runner has NO `VerifyMicroVMSupport` function today — the name occurs only
  in a forward-looking comment (`go/internal/runtime/microvm.go:126-128`:
  "the default collapses to microVM guarded by a VerifyMicroVMSupport hard
  gate at startup") and in the elastic-session design record, not in code.
  T9 therefore ships its own minimal checks — open `/dev/kvm`,
  `exec.LookPath` the microVM userspace trio (cloud-hypervisor, virtiofsd,
  passt), `podman info` — explicitly marked in-code as to-be-replaced by
  the runtime lane's eventual preflight gate when it lands. This keeps T9
  off the runtime lane's critical path (no cross-lane blocks-on).
  `docs/self-host.md`: the dedicated-KVM-machine shape and the one-box
  localhost-TLS shape (client-only OQ-6), each channel's install one-liner
  (flake, release tarball), postgres default + BYO-DSN, systemd unit example.
+ **Interfaces:** consumes T6 (flake install path), T8 (postgres default).
  Produces the documented, preflight-guarded install story RIG-2608 asks for.
+ **Test cycle:** follow `docs/self-host.md` verbatim on a clean KVM VM:
  flake install → preflight green → `compass-stack up` → client connects
  from another machine over TLS → one agent session runs.

### T10 — Docs, ledger, tracker

+ **Do:** update `docs/architecture/build-and-ci.md` with the distribution
  surfaces; land the §Ledger delta rows + status flips in
  `docs/designs/DECISIONS.md` (driver lands them in the design PR);
  re-scope/close RIG-2477 and RIG-2608 against the frozen record; file the
  per-task impl issues per the freeze→file→dispatch gate.
+ **Interfaces:** consumes the frozen record. Produces ledger rows +
  dispatched impl issues.
+ **Test cycle:** `tools/design-ledger-gate` green on the design PR; every
  DL id verified free at landing.

## Tasks

+ [ ] T1 — Linux client bundle attached to Releases (release.yml + build.sh)
+ [ ] T2 — darwin build path for compass-app (build-tag restructure)
+ [ ] T3 — macOS `.app`/dmg bundle + macos-14 CI lane (ad-hoc signed)
+ [ ] T4 — Developer ID signing + notarization + stapling in the release lane
+ [ ] T5 — RigelBuild/homebrew-tap (cask + CLI formula) + semver auto-bump
+ [ ] T6 — `flake.nix`: client + stack + stack-env packages, `nix flake check` CI
+ [ ] T7 — pin stock `postgres:18` container image by digest (no build lane)
+ [ ] T8 — stack container-backed postgres component + `--database-external`
+ [ ] T8a — pgid record format v2 (container teardown identity, DL-262)
+ [ ] T9 — `compass-stack` preflight + `docs/self-host.md`
+ [ ] T10 — build-and-ci docs, ledger rows, RIG-2477/RIG-2608 follow-through

## Ledger delta (intended)

Ledger-impact: new rows (DL-257..263) + no status flips (DL-217 is already
`Superseded by DL-238`; DL-238/DL-214/DL-183 stay Active — this record
*extends* them, per the DL-213 partial-supersession-by-citation pattern).
Rows land in `docs/designs/DECISIONS.md` in this PR, written by the driver.
IDs verified free at landing: the observed highest on main is DL-240, so
DL-257..263 are the next free block.

| ID | Decision | Status | Record |
| --- | --- | --- | --- |
| DL-257 | The client app ships a full per-OS matrix from day one — Linux x86_64 (thin-client tarball, DL-238 content) AND macOS arm64 (signed+notarized `.app` in a dmg, built on a macOS runner, never cross-compiled) — attached to the RIG-1746 release lanes, extending the release-bundling record's Fork 2(i) asset boundary and resolving its OQ-1 deferral (RIG-2477) | Active (Matt, 2026-08-24) | this record §S1 |
| DL-258 | Client install channels are homebrew (RigelBuild/homebrew-tap: cask for the macOS app + a cross-OS CLI formula, semver-bumped by the release workflow) and a repo `flake.nix` (client + stack packages pinned to the devenv.lock nixpkgs), over the raw release assets; distro-native packages (deb/rpm/AUR) deferred | Active (Matt, 2026-08-24) | this record §S2 |
| DL-259 | The self-host stack stays a host-level bring-up on a KVM-capable Linux machine (`compass-stack up`; microVM D3 hard-fail consumed, no compose/Swarm packaging); `compass-stack` joins the release binary matrix, and the flake + preflight + self-host doc are its install surface — resolving the client-only record's OQ-3 (RIG-2608) | Active (Matt, 2026-08-24) | this record §S3 |
| DL-260 | Postgres leaves the installed stack's host-process tree: the zero-config default is a dedicated postgres container run by the supervisor via rootless podman (socket-dir bind-mounted so the DSN contract is unchanged); a user-supplied DSN (`--database-external`) opts out. The image is the STOCK upstream `postgres:18` pinned by DIGEST — NOT a nix2container build of nixpkgs `postgresql` (that is the agent-shell mechanism, not a bundled stock service) and NOT a custom wrapper-entrypoint image; the official entrypoint does initdb+createdb+SIGTERM drain, so the `compass-postgres` wrapper collapses into env config (`POSTGRES_DB`, `POSTGRES_HOST_AUTH_METHOD=trust`, PGDATA volume, `unix_socket_directories`) on the container path and stays the host/dev-path bring-up. Supersedes the host-prerequisite interim answer (client-only OQ-3) atop the already-superseded DL-217; resolves OQ-5 | Active (Matt, 2026-08-24; image mechanism 2026-08-25) | this record §S4, §T7 |
| DL-261 | macOS signing identity provisioning waits for the company rename (Sealed Security Inc → Rigel AI Software Inc, same DE entity so its D-U-N-S carries over as an update, not a new request); enroll the Apple Developer Program org clean as Rigel AI Software Inc post-rename. Nothing pre-GA needs public signing, so dev/internal builds self-sign meanwhile (ad-hoc `codesign --sign -`, mandatory on Apple Silicon, or a local self-signed cert — no Apple account); the Developer ID cert + notarization (the distribute-to-other-Macs leg) is GA-gated on the enrollment. Resolves OQ-2 | Active (Matt, 2026-08-25) | this record §GC6, §T4 |
| DL-262 | Containerized-postgres teardown extends the DL-183 pgid record to format v2: `pgidFileVersion` `"1"`→`"2"`, entries become a kind-tagged discriminated union (`proc <component> <pgid> <starttime>` torn down by group signal as today; `ctr <component> <name>` torn down by `podman stop`/`rm -f`), `readPgidFile` dispatches on the tag and a shipped v1 binary hard-errors on a v2 entry line by the entry-grammar (not a header check) under the unchanged signal-off-a-half-understood-record discipline. The per-agent microVMs are the runner's sandbox one layer below the stack and are untouched (the stack tears the runner down by pgid as a plain host process); what forces v2 is postgres containerizing, not microVMs. Extends DL-183 (which stays Active); resolves OQ-7 | Active (Matt, 2026-08-25) | this record §S4, §T8a |
| DL-263 | darwin CI cadence mirrors the shipped affected-on-PR + full-sweep-on-main + nightly shape (`ci.yml:25-36`): a `macos-14` compile+bundle sweep runs on every push to main AND nightly ALWAYS (the backstop), plus an affected-on-PR leg gated by a small ubuntu pre-job asking moon which projects the PR affects (moon's own affected-detection — the signal `moon ci` uses — never a GitHub `paths:` filter, so moon stays the single source of affected-truth per the ci.yml header's rejection of a YAML project list), so a scarce mac runner spins up on a PR only when a darwin-relevant project is affected. Resolves OQ-8 | Active (Matt, 2026-08-25) | this record §T3 |

## Open Questions

Per the batched-clarifications rule; each carries a recommendation. Matt's
two rulings above are NOT here — they are decided. The **load-bearing set**
— OQ-2 (Apple account), OQ-5 (postgres image mechanism), OQ-7 (pgid-v2
contract change), OQ-8 (darwin CI cadence) — is now RESOLVED (Matt,
2026-08-25; DL-260..263). Each carries its resolution inline below.

### OQ-1 [non-load-bearing] — native Linux distro packages (deb/rpm/AUR)

Flake + brew-on-Linux + tarball cover the Linux matrix; distro packages add
per-format packaging debt (postinst scripts, repo hosting/signing) for
uncertain reach. **Recommendation:** defer; revisit on demand signal. The
channels in S2 are additive — a later `.deb` wraps the same bundle content.

### OQ-2 [load-bearing] — Apple Developer account provisioning

T4 requires an Apple Developer Program membership (org or individual), a
Developer ID Application certificate, and an App Store Connect API key — a
human/console prerequisite no agent can perform. **RESOLVED (Matt,
2026-08-25 — DL-261):** WAIT for the company rename (Sealed Security Inc →
Rigel AI Software Inc, same DE entity, D-U-N-S carries over as an update),
then enroll the org clean as Rigel AI Software Inc. Nothing pre-GA needs
public signing, so dev/internal builds self-sign in the interim (ad-hoc
`codesign --sign -`, mandatory on Apple Silicon, or a local self-signed
cert — no Apple account); the Developer ID cert + notarization is GA-gated
on the enrollment. T2/T3 proceed unblocked; the public-channel T4/T5-cask
leg gates on the post-rename enrollment secrets. A human-action issue files
the enrollment runbook at GA.

### OQ-3 [non-load-bearing] — NixOS module (`services.compass-stack`)

The flake's packages are the load-bearing channel; a NixOS module wrapping
`up` in systemd (with the KVM group + podman socket wiring declared) is the
polished self-host endgame but not required for T9's documented systemd-unit
example. **Recommendation:** defer to a follow-up on the flake once T6/T9
land; the module is additive.

### OQ-4 [non-load-bearing] — darwin-amd64 (Intel mac) support

The release-bundling record ships darwin-arm64 only for the CLI; the app matrix here matches.
Intel macs lack nested-virt relevance (client-only anyway) but are a
shrinking install base. **Recommendation:** arm64-only now; brew cask can
grow an `on_intel` block later without a design change (GHA `macos-13` for
an amd64 leg if demanded).

### OQ-5 [load-bearing] — postgres image build mechanism

T7 leaves nix2container-vs-Containerfile to the executor, but the fork has a
real tradeoff: nix2container reuses the `agent-image/` publish machinery and
the repo's pinning discipline (`agent-image/publish.sh` + the vendored
skopeo), while a Containerfile over the official `postgres:18` base is
simpler and inherits upstream security updates by rebuild.
**RESOLVED (Matt, 2026-08-25 — DL-260):** the STOCK upstream `postgres:18`
image pinned by DIGEST — NOT nix2container (that mechanism is for the
NixOS+devenv agent shell, not a bundled stock service). The official
entrypoint does initdb+createdb+SIGTERM drain, so the `compass-postgres`
wrapper collapses into env config on the container path (`POSTGRES_DB`,
`POSTGRES_HOST_AUTH_METHOD=trust`, PGDATA volume, `unix_socket_directories`)
and stays the host/dev-path bring-up; no custom image build lane is needed.
See S4 and T7. Non-blocking for every other task.

### OQ-6 [non-load-bearing] — prerelease channel exposure

Should `build-<sha12>` prereleases feed any channel beyond raw assets (e.g.
a `compass-cli@head` formula or a flake `packages.head`)? The flake tracks
`main` by construction (`github:RigelBuild/compass` is a moving ref), which
already IS the head channel. **Recommendation:** no extra head channels;
brew stays semver-only, flake-at-main is the sanctioned bleeding edge.

### OQ-7 [load-bearing] — containerized postgres teardown extends the frozen DL-183 pgid format to v2

The container has no process-group teardown identity (S4: a rootless
`podman run` client's `Pid()` does not describe the container, which runs
under conmon outside the client's group), so the fresh-`down` path needs a
container entry kind in the `stack.pgids` record — a `"1"` → `"2"` bump of
`pgidFileVersion` (`pgidfile.go:20-22`), the format DL-183 froze. The
alternative that avoids the format change is the systemd/quadlet-managed
container (§Alternatives): the stack only probes the DSN, but the install
story gains unit provisioning and `up`/`down` stop owning the database
lifecycle. **RESOLVED (Matt, 2026-08-25 — DL-262):** the pgid-format-v2
contract change (the S4/T8a design), NOT the systemd/quadlet lifecycle
change. The one-command up/down lifecycle is the reason postgres is a
supervised child at all; the v2 format is a strict superset (v1 records
parse as all-process entries), and Global Constraint 4's matched-build-set
invariant makes the cross-version window nil in practice. The per-agent
microVMs are the RUNNER's sandbox lifecycle one layer below the stack and
are untouched — the stack tears the runner down by pgid as a plain host
process; what forces v2 is postgres containerizing, not microVMs.

### OQ-8 [load-bearing] — darwin CI cadence (recurring macOS runner spend)

Nothing keeps the darwin build green between releases: the per-PR gate is
`go build ./...` untagged on Linux, which never compiles the darwin arm —
a PR can silently break darwin (exactly the T2 duplicate-symbol class of
error) and it surfaces on release day inside the signing-gated semver lane,
the worst place. macOS runners cost ~10x Linux minutes, so this is a
recurring-spend policy call. **RESOLVED (Matt, 2026-08-25 — DL-263):**
mirror the shipped CI shape (`ci.yml:25-36`, affected-on-PR + full-sweep-on-
main + nightly): a `macos-14` compile+bundle sweep on every push to main AND
nightly, ALWAYS (the backstop Matt wants for everything), PLUS an
affected-on-PR leg gated by a small ubuntu pre-job asking moon which projects
the PR affects (moon's own affected-detection, the signal `moon ci` uses —
never a GitHub `paths:` filter, so moon stays the single source of
affected-truth, per the ci.yml header's rejection of a YAML project list). A
scarce mac runner spins up on a PR only when a darwin-relevant project is
affected; main + nightly catch a mac-only regression regardless.
