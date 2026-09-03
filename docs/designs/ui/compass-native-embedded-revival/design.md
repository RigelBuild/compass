# Compass native app — embedded-mode revival (dual-mode returns; client mode survives)

Status: Draft
Linear: RIG-3139 (design); RIG-1662 (epic)
Supersedes: DL-235 (the "client is the ONLY mode" thesis) and the
app-never-spawns half of DL-236 (whose standalone-`compass-stack` half stays
Active and load-bearing); DL-237 (client-only app.toml); DL-238 (thin-client
bundle). Restores the dual-mode SHAPE of DL-106 under a new (trust-model)
rationale and the sidecar mechanism of DL-215 by citation. DL-108/DL-109's
supervisor design + keychain bearer stay Active. See §Ledger-impact.

## Problem / Intent

Matt ruled (2026-09-01, relayed via the compass-obs topology lane): **support
BOTH podman and microVM runners going forward — bring back embedded app mode
and finish it for macOS.** This record designs the revival: `mode="embedded"`
returns to the native app as the low-friction onboarding / local-dev front
door, ADDED ALONGSIDE the fully-surviving client mode.

**The retirement's premise is falsified.** The client-only record (DL-235,
RIG-2542) was driven by one upstream constraint — quoted from the frozen
record, `compass-native-client-only/design.md:15-19`: "The driving constraint
is upstream: the container Runner is being dropped once the microVM Runner
(RIG-1717 task I1) lands, and the microVM path requires a KVM/nested-virt
floor — so running any agent locally becomes KVM-gated where today's
rootless-container path was not." That record itself flagged the premise's
contingency (`design.md:25-28`: "this is a forward-looking retirement driven
by the runtime direction and the macOS target, not a claim that embedded is
broken on today's boxes"). The direction has now changed: the podman
(rootless-container) Runner is a PERMANENT supported tier for single-tenant
deployments, not a transitional bootstrap being dropped. With the KVM floor
gone for the single-tenant case, every consequence chained off it — no
embedded on Macs, no embedded on locked-down laptops, "cost without a durable
user" — collapses, and the retirement reverses cleanly: the supervisor design
was never unbuilt (DL-108 stayed Active; DL-236 kept `compass-stack` whole),
only the app's invocation of it was deleted.

**What does NOT reverse: client mode.** This record removes the word "only"
from the client-only thesis, not the client. The client surface (connect
screen, keychain bearer, TLS dial, WhoAmI, board-over-bridge, multi-window) is
built, merged, and stays first-class — it is the RECOMMENDED steady-state for
a real self-host deployment (always-on stack on a VPS/EC2, app connects over
TLS) and the only mode for the managed multi-tenant deployment.

**Intent, in Matt's framing:** "the native app embedded mode is the EASY FRONT
DOOR to get users using the product right away — brew install the app, launch,
sign in with your Claude Code/Codex account, go, same as OMP/any harness." The
subscription-first cohort runs agents on their OWN machine (their IP, their
risk — the OMP posture), so restricted-tier subscription OAuth is allowed
locally, unlike the multi-tenant managed case. Embedded is the on-ramp; client
mode is where a serious self-host user lands.

## Topology — three funnel entries (consumed from compass-obs, PR #804)

The app is a 3-way funnel entry. This record builds entry 1 and keeps entry 2
untouched; entry 3 is a deployment this OSS-core repo does not detail (its
product-strategy specifics live outside this repo — see the scope note below).

Two axes are independent: the **app mode** (embedded vs client — this record's
concern) and the **runner backend** (podman vs microVM — compass-obs's #804
concern, consumed here). The backend column records the RECOMMENDED backend per
entry; the app-mode column is what this record builds.

| Entry | Recommended runner backend | App mode | Role |
| --- | --- | --- | --- |
| **embedded-local** | podman (container) is PRIMARY — the target is a user on their OWN machine, and the primary embedded target is macOS, where a microVM is not an option (no host `/dev/kvm`). microVM is usable where the host has KVM (raw Linux); WSL2 has no KVM out of the box, so it is podman there too — but that is DIRECTION only; v1 builds the container path (OQ-11). | **embedded** — the app spawns/supervises a local stack | Onboarding + local dev. Zero-config: install → launch → sign in → go. NOT the recommended production steady-state. |
| **self-host-stack** | microVM is RECOMMENDED (stronger isolation, more efficient); podman is fully usable and is the tier for cost-sensitive operators (cheapest VPS, no nested-virt). | **client** — the app dials the stack's TLS door | The SUGGESTED self-host steady-state. The stack is designed to run all the time, so it does not live on a personal laptop. Exactly the client mode already built. |
| **managed-hosted** | microVM | **client** | Hardware isolation against untrusted multi-tenant code. |

**The security boundary follows the trust model, not deployment uniformly**
(compass-obs, DL-318/RIG-3070, being frozen in PR #804 — exact citation is
OQ-5): a runtime isolating UNTRUSTED multi-tenant code needs the hardware
boundary (microVM over KVM); a single-tenant box running the operator's OWN
code has no untrusted tenant to isolate from, so rootless podman is a correct
boundary there — a permanent supported tier needing no `/dev/kvm`. Where a
single-tenant host DOES have KVM, microVM is the recommended backend too
(stronger isolation, more efficient); podman stays the fully-supported tier for
KVM-absent or cost-sensitive hosts. §Alternatives considered pre-empts the
"wasn't podman-fallback already rejected?" reading of the kvm-only amendment.

**Scope note (OSS-core boundary).** This is the OSS core repository; it
documents the self-hosted and embedded deployments users run themselves. The
managed multi-tenant offering's product-strategy specifics (pricing, capacity,
tenancy policy) are deliberately NOT detailed here — they live in the private
product-strategy repository. This record names the managed deployment only
where the trust model forces it (why it is microVM-only), never its product
shape.

## Global Constraints

1. **Go module + floor.** One module, `go/` root
   (`module github.com/RigelBuild/compass/go`, floor go 1.25.0); the toolchain
   is proto-shimmed — build/vet only via `direnv exec <workspace-root> go …`,
   never bare `go`.
2. **No cleartext.** Client-mode `server_url` stays https-only
   (`go/internal/appconfig/appconfig.go:124-126`: "validateServerURL enforces
   that a client server_url is an absolute https URL. http and relative URLs
   are rejected"); embedded mode's bridge rides the h2c-over-UDS socket (a
   filesystem-permission boundary, not a network one), exactly the
   pre-retirement contract (`embedded.go` header: "the h2c-UDS WhoAmI dial").
3. **Keychain-first bearer (DL-109's keychain half, Active).** The client-mode
   bearer lives only in the OS keychain (0600 fallback) + the shell-injected
   header; never in app.toml, argv, or the UI `Connection`. Embedded mode has
   no bearer (socket ambient-admin, DL-111).
4. **Clean cutover (AGENTS.md).** No shims: the revived embedded arm is a real
   mode, not a compatibility layer; the retirement-error copy in
   `appconfig.Parse` (`appconfig.go:96-100`) is deleted, not kept beside a
   working arm. Migrate every caller and every stale RIG-2554 comment
   (§Plan T-7 citation sweep).
5. **Do not blindly restore.** The tree moved since the retirement commit
   `efea2d29a1b9`: DL-260 landed containerized postgres (stock `postgres:18`
   by digest via rootless podman, `go/internal/stack/postgres_image.go:30`),
   DL-262 landed pgid record v2, DL-282 flipped the shell to gtk4
   (`go/cmd/compass-app/main.go:1`: `//go:build (linux && gtk4) || darwin`),
   and `compass-stack`'s default DSN moved to a `pgsock` SIBLING dir
   (`go/cmd/compass-stack/main.go:271-278`). Every revived file is reconciled
   against current main (§Approach A2/A3), never copied verbatim. The
   reconciliation pass now also covers the runner arbitrary-uid follow-up
   (`compass-runner-arbitrary-uid/design.md:7-10`): the old preflight
   host-uid==1000 gate is NOT revived (§A3 delta 3).
6. **The runtime direction is consumed, not designed.** The podman-permanent /
   microVM-recommended topology and the 3-way funnel belong to compass-obs
   (PR #804, DL-318). This record consumes that direction; it does not restate
   or re-derive it.
   **Freeze order:** this record merges AFTER #804 (DL-318 is pinned first;
   this record then rebases and re-verifies DL-319..DL-321 against the frozen
   row — the merge-order protocol locked with compass-obs). Degrade path if
   the order inverts: DL-319's rationale clause degrades to "Matt 2026-09-01
   ruling" and the DL-318 cite is added post-#804-freeze (OQ-5's load-bearing
   ordering half).
7. **Scripts-over-bash; tests are Go/TS test code; moon-registered CI lanes;
   TABS in JSON; RIG-NNN-in-subject commits** — inherited unchanged.
8. **Design-decision ledger.** This record's rulings land as DL rows in
   `docs/designs/DECISIONS.md` in the SAME PR (proposed rows in
   §Ledger-impact; the driver applies the ledger edit — the design-ledger-gate
   enforces record↔ledger touch-coupling, and the PR carries a
   `Ledger-impact:` line).

## Approach

### A1 — Mode-selection contract: dual-mode returns, embedded is the onboarding default

`go/internal/appconfig` regains `ModeEmbedded` beside the kept `ModeClient`.
Current main is single-mode by construction — `appconfig.go:14-17`: "Client is
the only mode — embedded mode was retired in RIG-2554 … Load/Parse only ever
yield ModeClient or a legible error", with `mode="embedded"` parsing to the
retirement rejection (`appconfig.go:96-100`). The new contract:

- **Absent app.toml → embedded** (pending OQ-2). This restores the original
  DL-109 zero-config posture: first launch of the installed app just works —
  no server URL, no bearer, the app brings up its own local stack. The current
  absent-file first-run error (`appconfig.go:155-157`: "An absent file is a
  legible first-run error: client mode requires a server_url and there is no
  zero-config default any more") is deleted with its premise.
- **`mode = "embedded"`** → `Config{Mode: ModeEmbedded}`. Embedded accepts no
  `server_url`/`ca_cert` (unknown-key–style rejection: a server_url under
  embedded is a mis-aimed config, not an ignorable extra — same rationale as
  the existing undecoded-key rejection, `appconfig.go:85-91`).
- **`mode = "client"`** → unchanged: `server_url` required, absolute https
  only (`parseClient`, `appconfig.go:109-122`), optional `ca_cert`. Client
  mode is byte-for-byte the built T5 surface; nothing above the transport seam
  changes.
- **The `--mode`/`$COMPASS_APP_MODE` override returns** (pending OQ-3), as the
  pre-retirement shape: flag wins, else env, else app.toml, else the embedded
  default (pre-retirement `embedded.go` `resolveMode`, `efea2d29a1b9-`: "An
  empty flag falls back
  to the env; both empty is 'no override' (Load then uses app.toml, else
  embedded)"). An override to embedded clears client-only fields; an override
  to client still requires a `server_url` from app.toml.
- **Graduation path embedded→client** is config, not code: the user stands up
  `compass-stack up` on an always-on box and writes
  `mode="client"` + `server_url` into app.toml (or passes the override). The
  self-host doc (DL-259's install surface) gains a "graduating from the local
  app" section; no in-app migration flow is built in this record.

Scope note: "sign in with your Claude Code/Codex account" is the agent
harness's own OAuth inside the agent container (the OMP posture — the
subscription-first cohort runs on their own box, their IP/their risk). It is
allowed LOCALLY under the trust model, and it involves no app-side work in
this record: the app neither proxies nor stores harness credentials.

### A2 — Supervisor re-wire: revive the launch pipeline against the CURRENT stack

The pre-retirement pipeline survives as design and mostly as code; this is
re-wiring, not redesign. The revived shape (from `efea2d29a1b9-`
`go/cmd/compass-app/embedded.go`, read this session):

- **`embeddedPipeline`** — "the embedded-mode launch pipeline over its
  injected external effects … (preflight run, the compass-stack up exec, the
  WhoAmI dial)" with `run(ctx, params)` executing "preflight → stack up →
  WhoAmI. A preflight failure short-circuits (the stack is never spawned)".
- **The exec seam supervises the BINARY, not the package** — the file header:
  "This file supervises the stack through the compass-stack BINARY (frozen
  design §T4 …), not by importing go/internal/stack: `compass-stack up`
  brings the stack to Ready and exits 0 while the children keep running
  (fire-and-return)". `runStackUp`/`runStackDown` + `captureStderr` (the
  *os.File-not-pipe stderr capture whose rationale — Wait would otherwise
  block on the lingering children's inherited pipe — is documented in the
  revived code) return verbatim.
- **`resolveStackBin`** — flag → `$COMPASS_STACK_BIN` → sibling-of-executable
  → `$PATH`, with the legible not-found error; **`prependExecDirToPath`** —
  the DL-215 PATH threading so "a bundle's sidecar binaries … win
  exec.LookPath inside the supervised stack". `resolveStackBin` returns
  verbatim; `prependExecDirToPath` returns with ONE reconciliation — its
  doc-comment sidecar list (`compass-postgres/compass-server/compass-runner`
  at `efea2d29a1b9- embedded.go:367`) updates to the DL-321 process children
  the stack still LookPaths (`compass-server/compass-runner`; postgres is a
  DL-260 container, not a PATH-resolved sidecar — §A4/T-2).
- **`whoAmIOverUDS`** — the h2c-UDS WhoAmI dial (DL-111) and
  **`bridge.NewUnixTarget(socket)`** as the embedded production bridge target
  again; its doc re-promotes from the current test-harness demotion
  (`go/internal/bridge/pump.go:80-82`: "its production caller was removed in
  T-1 (RIG-2554); its sole users are the pump test suite … and two
  compass-app tests").
- **`lifecycle.go`'s `quitController`** — linger-by-default plain quit (no
  teardown; relaunch re-attaches) + the explicit "Quit and stop stack" action
  running `compass-stack down` under `stackDownTimeout` with the quit-anyway
  posture (OQ-6). This rides DL-262's pgid-v2 `DownDetached` unchanged — the
  app only execs `compass-stack down`; the v2 container-aware teardown is the
  CLI's (`go/cmd/compass-stack/main.go:386-391`: "runDown tears the stack
  down across the process boundary via stack.DownDetached: it reads the pgid
  record a prior up persisted").

**Reconciliations against current main (Global Constraint 5) — the deltas
from a verbatim restore:**

1. **Drop the app-side DSN duplicate and the DB preflight probe.** The old
  `embeddedDatabaseDSN` was "a DELIBERATE second copy of cmd/compass-stack's
  defaultDSN" kept in lockstep by test, computing
  `<state-dir>/postgres/sock` — a formula that is now WRONG on main:
  `defaultDSN` moved to the `pgsock` SIBLING dir for the container bind-mount
  (`compass-stack/main.go:271-278`: "The socket dir is `<state-dir>/pgsock` — a
  SIBLING of the PGDATA dir … NOT nested under it"). Under DL-260 postgres is
  a container the stack itself starts, so a pre-`up` reachability probe has
  no signal on the cold-start path (the old code already classified it
  ADVISORY for exactly that reason — `classifyPreflight`: "on a fresh state
  dir the DB and image checks NECESSARILY fail before `up` has run").
  Revival drops `checkDB`, `embeddedDatabaseDSN`, the `DBReachable` seam and
  its pgx adapter entirely; `up`-Ready remains the DB verification
  ("reaching Ready transitively verifies both").
2. **The `compass-stack up` argv is unchanged in shape and correct as-is.**
  `stackUpArgs` passed `up --state-dir … --image … --socket …`; current
  `resolveConfig` still requires exactly `--state-dir` + `--image`
  (`main.go:189-194`) and defaults everything new: `--postgres-image`
  defaults to the pinned stock digest (`main.go:148-152`: "Defaults to the
  pinned stock postgres:18 digest"), `--collector-image` likewise, `--listen`
  to the fixed loopback door. The app deliberately passes NO postgres/
  collector/listen flags — the CLI's defaults are the contract, and the app
  re-learns nothing the stack already owns.
  DL-313's incoming NATS fourth service (a standalone stack service in EVERY
  deployment, `DECISIONS.md` DL-313, Active 2026-08-31) will join the
  embedded stack through this same contract: today `go/internal/stack` has
  zero NATS (grep this session — no matches), and when it lands the
  pass-no-flags posture absorbs it, so embedded's Ready/pgid/preflight
  scoping does not change under this record (cross-ref: Global Constraint 6).
3. **gtk4/darwin build shape.** The shell is now
  `//go:build (linux && gtk4) || darwin` (`main.go:1`); the pre-retirement
  embedded files were `unix && gtk3`. Revived files take the shell's current
  tag set, with the darwin-only podman-machine adapters split into
  `_darwin.go` files (§A5).
4. **Launch dispatch.** Current `launch` "is a thin wrapper over runClient"
  (`main.go:190-196`); it regains the two-arm switch on `cfg.Mode`: embedded
  resolves socket/image/stack-bin, runs the pipeline under `bringUpTimeout`,
  and hands `bridge.NewUnixTarget(socket)` + the WhoAmI account id to the
  bridge service; client stays `runClient(cfg, stateDir)`
  (`client.go`, kept verbatim). The `--socket`/`--image`/`--compass-stack`
  flags and `resolveSocket` return beside the kept `--assets`/`--state-dir`
  (`main.go:48-55`).

### A3 — Preflight v2: cross-OS host checks

`go/internal/preflight` revives with its inversion intact (package doc: "The
checker core is inverted over injected effect functions (see Deps) … every
genuine external effect … is a func the caller supplies") and four deltas:

1. **OS check widens.** The old check was `d.GOOS == "linux"` with detail
  "embedded mode is Linux-only, this host is …". v2 accepts
  `linux | darwin` (Windows/WSL is OQ-4).
2. **A darwin-only machine check joins the fatal set.** On macOS podman runs
  inside a Linux VM ("podman machine"); v2 adds `CheckMachine` — injected
  `MachineReady func(ctx) error` — fatal on darwin, absent on linux (§A5 owns
  the adapter and the provisioning story).
3. **The DB check is deleted** (§A2 delta 1); the image check stays ADVISORY
  ("it is pulled from GHCR at first run", DL-112). **The uid check is NOT
  revived.** The old fatal gate (`preflight.go` Run: "the embedded runner
  requires uid %d … the agent image bakes the agent user at that uid",
  `DefaultAgentUID = 1000` in `uid.go`) mirrored a runner-side uid refusal
  that current main has DELETED: the arbitrary-uid follow-up
  (`compass-runner-arbitrary-uid/design.md:7-10`) replaced it with a
  uid-agnostic remap — `go/internal/runtime/podman.go:471`
  `--userns=keep-id:uid=%d,gid=%d` — so the host uid carries no signal on
  ANY OS, and a verbatim revival would break embedded launch on every Linux
  host with uid ≠ 1000. The revived preflight ships NO `uid.go` (its "not
  importable" rationale is dead: `go/internal/agentuid/agentuid.go:13`
  `const AgentUID = 1000` is the importable single source of truth — any
  code still needing the value consumes it).
4. **A podman-version check joins the fatal set** (OQ-8 carries the
  recommendation for Matt's gate). The capability the arbitrary-uid remap
  depends on — podman ≥ 4.3 for `keep-id:uid=` (`VerifyUsernsRemapSupport`,
  `go/internal/runtime/podman.go:504`; floor consts `minUsernsRemapMajor=4`/
  `minUsernsRemapMinor=3` at `:493-494`; a hard floor, no fallback) — IS
  enforced by compass-runner at startup (`compass-runner/main.go:102`), but
  that refusal is NOT a legible error surface in embedded mode: the runner
  is the LAST step of `stack.Up` (`go/internal/stack/stack.go:283-291`),
  spawned via `Supervisor.Start` → `cmd.Start()`
  (`go/internal/stack/adapters/process.go:71`) — a LAUNCH that returns
  before the runner's startup gate runs, and `Up` already reached Ready at
  step 4 (the server poll, `stack.go:265-267`) so it returns `nil`
  regardless; the app's `runStackUp` reads the captured runner stderr ONLY
  on a non-zero exit and deletes it unread on exit 0
  (`embedded.go:224-246` at `efea2d29a1b9-`); and the pre-Ready postgres
  container uses PLAIN `--userns=keep-id`
  (`go/internal/stack/adapters/postgres_container.go:171`), not the `:uid=`
  form, so it does not surface the version floor earlier. Net: on a
  rootless-podman-present-but-<4.3 host (Ubuntu 22.04 LTS = podman 3.4.4;
  Debian 11; RHEL 8) the present+rootless-only `podman` check passes, the
  board renders, and embedded mode looks healthy while the runner has
  crashed at startup and no agent can ever run — the exact "deep failure
  inside the stack" the preflight package doc exists to prevent, defeating
  the record's easy-front-door thesis. So preflight probes the ≥ 4.3 floor
  itself (reusing the `VerifyUsernsRemapSupport` machinery / the
  compass-stack version-floor helpers) and fails FATAL BEFORE the app execs
  `compass-stack up`, surfacing the same "podman N.N or newer is required"
  copy the runner emits (`podman.go:504`) at the front door instead of deep
  inside a fire-and-return stack whose exit 0 hides it.

The severity split stays at the wiring boundary (`classifyPreflight` in the
shell): FATAL = OS, podman-present+rootless, podman-version (delta 4),
machine (darwin); ADVISORY = image. No uid entry (delta 3).

### A4 — Bundle re-scope: sidecars return, postgres stays a container

DL-238's thin client bundle (`app-bundle/build.sh:3-5`: "the thin CLIENT
bundle: the gtk4 shell (compass-app) + the UI dist + the desktop file +
LICENSE. No sidecar binaries, no postgres tooling") re-scopes to carry what
embedded needs. The fork the brief names — bundled-postgres-tooling (old
DL-217) vs container-postgres-via-podman (DL-260) — is ruled here for
**container-postgres** (OQ-1 carries the recommendation for Matt's gate):

- The bundle regains the DL-215 sidecar mechanism by citation: `bin/`
  carries `compass-app`, `compass-stack`, `compass-server`, `compass-runner`
  (+ `dist/` beside the executable, kept), PATH-threaded via
  `prependExecDirToPath` so the supervised stack's LookPath children resolve
  in-bundle.
- **No postgres tooling and no `compass-postgres` sidecar.** DL-260 made the
  installed-stack postgres "a dedicated postgres container run by the
  supervisor via rootless podman … the STOCK upstream `postgres:18` pinned by
  DIGEST", and the CLI defaults to it (`--postgres-image` default,
  `main.go:148-152`). The `compass-postgres` wrapper "stays the host/dev-path
  bring-up" (DL-260) — a devenv concern, not a bundle one. DL-217 therefore
  STAYS superseded; rootless podman is the packaged embedded mode's sole
  container prerequisite, checked by preflight, exactly the posture DL-217
  originally aimed at ("leaving rootless podman as the sole host
  prerequisite") minus the bundled tooling.
- The collector likewise ships as DL-260-style container defaults
  (`--collector-image`, `main.go:156-159`) — nothing bundled.
- Per-OS shape (DL-257 stays Active): Linux keeps the tarball layout
  (`bin/` + `share/applications` + LICENSE, `build.sh:66,85-89`) with the
  sidecars re-added to the staging loop and sanity assertions; macOS stages
  sidecars in the `.app` beside the shell binary (`Contents/MacOS/`), where
  `resolveStackBin`'s sibling probe and `distDirForExecutable`'s layout rule
  (`main.go:241-247`: "A macOS .app stages the binary at
  Contents/MacOS/compass-app and the UI dist at Contents/Resources/dist")
  already compose. Brew stays the channel (DL-258).

### A5 — macOS: podman-machine provisioning (the genuinely new work)

Linux podman is native; on macOS the podman CLI drives a Linux VM, and a
fresh Mac has no machine. The app owns making this invisible — otherwise the
"brew install → launch → go" promise dies at a terminal command. Design:

- **Detection**: `MachineReady` shells `podman machine inspect` and requires
  a running machine with a reachable socket. That `inspect` exposes
  `.ConnectionInfo.PodmanSocket.Path` and that `podman machine ls
  --format json` distinguishes no-machine from stopped-machine are
  **T-6 spike-verified assumptions** (external podman-CLI behavior, not read
  this session), per the record's designed-against-assumption convention.
- **Provisioning**: on no-machine or stopped-machine, embedded launch runs
  `podman machine init` (first run; downloads the VM image — minutes, so the
  UI shows a provisioning state, not a frozen window) / `podman machine
  start`, then re-probes. This lives in the darwin preflight adapter as an
  ENSURE step (mirroring how `up` ensures image/DB rather than gating on
  them), behind an injected seam so the orchestration is unit-testable.
- **Socket wiring**: rootless podman inside the machine serves the API socket
  the VM forwards to the host; `compass-stack`'s podman adapters address
  podman via the CLI (`podman …` subprocesses — the repo half, cited via
  `go/internal/runtime/podman.go`). That the CLI resolves the machine
  connection itself — so no socket path threads through the app — is a
  **T-6 spike-verified assumption** (external behavior), not established
  fact. The podman API socket is also NOT the whole story: the stack's OWN
  AF_UNIX sockets (the DL-260 postgres DSN `host=` socket-dir bind mount,
  `postgres_container.go:45-48`; the runner's per-container agent sockets,
  `spec.go:52` `--runtime-dir`) must survive the darwin host↔VM virtiofs
  boundary — OQ-7 carries that question. The spike validating the whole
  chain end-to-end (machine cold-init → stack up → agent container runs →
  socket topology) is the record's riskiest unknown; T-6 front-loads it,
  and compass-obs's #804 tracks the same spike as its mac OQ-3 (OQ-5 wires
  the cross-ref at freeze).
- **Resource floor**: `podman machine init` defaults are modest; the agent
  image + postgres + server want real memory. T-6 sets explicit
  `--memory`/`--disk-size` at init and records the chosen floor in the
  self-host doc.

### A6 — UI: the embedded boot arm

The shell's startup JS regains the embedded literal: `shellStartupJS`
currently pins `"client"` (`main.go:207-209`: "It assigns
`window.__COMPASS_MODE__` (\"client\" — the app is a native client only,
RIG-2554)"); it goes back to injecting the resolved mode. The UI fork today
is `shellMode() === "client" ? () => bootNativeClient(root) : () =>
bootConnection(root, () => envConnectionProvider().resolve())`
(`apps/ui/src/index.tsx:63-66`) — where the else-arm is the BROWSER-dev env
provider. Embedded gets its own arm: a shell embedded provider that resolves
the bridge connection directly (no probe, no connect screen, no bearer —
socket ambient-admin, DL-111), i.e. a three-way fork on
`"client" | "embedded" | undefined`. The `Window.__COMPASS_MODE__` TS union
already carries `"embedded" | "client"` (`apps/ui/src/shell-globals.ts:16`)
and the shell-globals test still exercises it ("distinguish embedded from
client mode", `shell-globals.test.ts:32-35`) — the narrowing the client-only
record deferred never landed, so no union work is needed; only the boot arm
is new.

## Alternatives considered

### "Wasn't podman-under-KVM-absence already rejected?" — no: different boundary, different trust model

`microvm-kvm-only-amendment.md:91-97` rejected "Keep the degrade-to-container
path as a self-host / KVM-absent convenience" (Matt, 2026-08-23): "A
permanent shared-kernel fallback for a boundary whose entire purpose is
hardware isolation against untrusted code is a standing hole in the security
posture the microVM exists to provide … A KVM-absent host does not get a
lesser boundary; it does not run." That rejection is about the
**untrusted-multi-tenant microVM boundary** — a managed host running OTHER
tenants' code must never quietly swap hardware isolation for a shared kernel.
It stands unmodified.

This record is the OTHER case: **single-tenant, the operator's own code, no
untrusted tenant to isolate from.** On the user's own machine (embedded) or
their own single-tenant box, rootless podman is not a "lesser boundary for
untrusted code" — it is the correct boundary for the operator's own code, the
same posture as running OMP or any harness directly. The security boundary
follows the trust model, not deployment uniformly (DL-318, §Topology).
Nothing here re-opens a degrade path in the managed runtime: the microVM
Runner stays KVM-only where it runs; the podman Runner is a distinct
permanent tier, not a fallback the microVM path degrades into.

The rejection had a SECOND ground the trust model does not answer — cost:
"it splits every downstream path (C3 burst, D4 density) into two runtime
shapes forever" (`microvm-kvm-only-amendment.md:91-97`, same rejection,
second clause). Making podman a PERMANENT tier accepts exactly that cost.
The answer here is explicit acceptance, not refutation: Matt's 2026-09-01
ruling ("support BOTH podman and microVM runners going forward") accepts
the permanent two-runtime-shapes cost, and the elastic-runtime tradeoff it
implies (burst/density paths carrying both shapes) lives with #804's
trust-model split, which owns the runtime direction (Global Constraint 6).
Two frozen artifacts still assert the falsified microVM-sole-runtime
end-state — `microvm-runner.md` D2 ("the container path is **removed** and
microVM becomes the sole runtime … a transitional bootstrap, not a
permanent second runtime") and the runner flag help
(`compass-runner/main.go:229-230`: "'podman' (default, transitional)") —
T-7 banners/defers both.

### Embedded as the self-host steady-state (rejected — corrected by Matt)

An earlier framing made embedded "the" self-host mode. Rejected: the
self-hosted stack is designed to run ALL THE TIME, and a personal laptop is
not an always-on host — the suggested self-host pattern is a cheap always-on
VPS/EC2 running `compass-stack up`, reached by the app in CLIENT mode
(exactly the built T5 surface). Embedded is onboarding + local dev; client
mode is the graduation path and stays the recommended steady-state. This is
why DL-236's standalone-CLI half SURVIVES (§Ledger-impact).

### Bundle the postgres tooling again (old DL-217 mechanism — rejected)

Reviving the nix `postgresql` closure in `bin/` would re-add the closure
weight and the store-symlink staging DL-238 deleted, to serve a path the
stack no longer defaults to: DL-260 made the container postgres the
installed-stack default with the CLI resolving the pinned digest
(`--postgres-image` default). Bundling tooling would either fight that
default (empty `--postgres-image` forces the dev wrapper path) or ship dead
bytes. Podman is already a hard prerequisite of embedded mode (the runner
runs agents in containers), so postgres-as-container adds no NEW host
requirement. Rejected in favor of §A4; OQ-1 carries this to Matt's gate.

### Docker Desktop / Docker Engine as the macOS container runtime (rejected)

The whole supervised stack is built on rootless podman (`compass-stack`'s
podman adapters, DL-260's rootless-podman postgres, the pgid-v2 `podman
stop`/`rm -f` teardown arm). A second runtime would fork every adapter and
the teardown grammar for zero product gain, and Docker Desktop adds a
commercial-licensing surface. podman-machine is podman's own supported macOS
shape; §A5 builds on it. OQ-9 carries the related "docker SOCKET in embedded?"
fork (also rejected — the isolation boundary is podman-specific); OQ-10 weighs
Apple's `container` framework as a macOS backend (deferred, pre-1.0).

### A separate "compass dev" CLI front door instead of app-embedded (rejected)

A CLI could bring up the same local stack, but the funnel promise is the
APP: install → launch → go, one artifact, no terminal. `compass-stack up` by
hand remains available to CLI-preferring users unchanged (DL-236's surviving
half); building a third front door would duplicate the supervisor invocation
this record is already reviving inside the app.

## Plan

Dependency order: **T-1 → T-2 → (T-3 ∥ T-6) → T-4 → T-5 → T-7**. T-1 and T-2
are the Go revival and MUST land in the same PR (mirroring the retirement's
T-1/T-2 coupling in reverse: T-1 alone would flip the absent-app.toml default
to a `ModeEmbedded` whose `launch` arm is only the sanctioned transient
error, failing the zero-config path on `main` illegibly; T-2 alone would
not compile — `embedded.go` consumes T-1's appconfig API). T-6 (the macOS
spike) starts as early
as possible — it is the riskiest unknown and can invalidate T-5's darwin
scope. One task per PR-sized slice; every task runs
`direnv exec . go build ./... && go vet ./...` + `moon run compass-go:ci` in
its cycle (elided below as "module gates green").

### T-1 — Revive appconfig dual-mode + the launch dispatch skeleton

- **Do:** in `go/internal/appconfig`: re-add `ModeEmbedded` AFTER the kept
  `ModeClient Mode = iota` — ModeClient KEEPS the zero value
  (`appconfig.go:20-24` today), and the embedded default is made EXPLICIT
  in `Load`/`Parse`, never via the zero value. Migration hazard, flagged:
  inserting `ModeEmbedded = iota` BEFORE ModeClient would silently flip
  ModeClient 0→1, sending every zero-valued `appconfig.Config{}` from the
  client arm to embedded (the launch switch at `main.go:196-199` sends
  zero-Mode to the client arm today). `Parse` regains the
  embedded arm (`case "", modeStrEmbedded: → Config{Mode: ModeEmbedded}`,
  deleting the RIG-2554 rejection copy at `appconfig.go:96-100`); embedded
  rejects a non-empty `server_url`/`ca_cert` legibly; `Load`'s absent-file
  first-run error (`appconfig.go:155-157` behavior) becomes
  absent-file → `Config{Mode: ModeEmbedded}`. Re-add `applyOverride` + the
  `--mode`/`$COMPASS_APP_MODE` resolution (the pre-retirement `resolveMode`
  shape, from `embedded.go` — not main.go) into `cmd/compass-app`. In
  `main.go`: `launch` regains the
  two-arm switch on `cfg.Mode` with the embedded arm returning a legible
  "embedded launch lands in T-2" error THIS task only (the one sanctioned
  transient: it is unreachable in a released build because T-1+T-2 share a
  PR; it exists so T-1's tests can pin the dispatch seam).
- **Interfaces:** produces
  `appconfig.Load(configHome, home, override string) (Config, error)` (the
  override parameter returns), `Config{Mode Mode; ServerURL, CACert string}`
  with `Mode ∈ {ModeEmbedded, ModeClient}`, `Mode.String() → "embedded" |
  "client"`. Consumes nothing new.
- **Test cycle:** appconfig table tests: absent → embedded; `mode="embedded"`
  → embedded; embedded+server_url → legible reject; `mode="client"` arms
  unchanged; override precedence flag>env>file>default; unknown-key
  rejection unchanged; a test pinning `ModeClient == 0` (the zero-value
  contract); caller sweep: grep zero-valued `appconfig.Config{…}`
  constructions and assert none relies on the zero Mode meaning embedded.
  Module gates green.

### T-2 — Revive the supervisor pipeline (same PR as T-1)

- **Do:** restore from `efea2d29a1b9-` into `go/cmd/compass-app` under the
  current build tags: `embedded.go` (`embeddedPipeline`, `embeddedParams`,
  `runEmbedded`, `stackUpArgs`/`stackDownArgs`, `runStackUp`/`runStackDown`,
  `captureStderr`, `whoAmIOverUDS`, `resolveStackBin`,
  `prependExecDirToPath`, `resolveImage`, `resolveSocket`, `bringUpTimeout`),
  `lifecycle.go` (`quitController`, `stackDownTimeout`),
  `preflight_adapters.go` (minus the pgx DB adapter), and the
  `go/internal/preflight` package (minus `uid.go`/the uid check, §A3
  delta 3, plus the delta-4 podman-version fatal check) — applying the §A2
  reconciliations: NO `embeddedDatabaseDSN`, NO `DBReachable`/`checkDB`,
  gtk4/darwin tags, `runEmbedded` wiring the quit controller into the Wails
  app menu/window close path. Two doc-comment reconciliations, not verbatim:
  (1) `prependExecDirToPath`'s sidecar list drops `compass-postgres`
  (`compass-server/compass-runner` only, §A2/§A4); (2) `bridge.NewUnixTarget`'s
  doc re-promotes from test-harness status (`pump.go:80-82`) to the embedded
  production target. Add the delta-4 preflight check — reuse
  `runtime.VerifyUsernsRemapSupport` / the compass-stack version-floor helpers
  to probe the podman ≥ 4.3 floor FATAL before the app execs `compass-stack up`
  (§A3 delta 4). Restore `shellStartupJS(mode, serverURL)` injecting the resolved
  mode (§A6's shell half). Add the "Quit and stop stack" menu item beside
  "New Window" (`main.go:107-113` is the menu seam).
- **Interfaces:** consumes `appconfig.Config` (T-1),
  `compass-stack up|down --state-dir S --image I --socket P` (current CLI,
  `resolveConfig` requiring exactly state-dir+image, `main.go:189-194`),
  `bridge.NewUnixTarget(socket string) *bridge.Target`. Produces
  `runEmbedded(ctx, pipeline embeddedPipeline, params embeddedParams,
  stackDown func(ctx, []string) error) (accountID string, *quitController,
  error)` and the two-arm `launch`.
- **Test cycle:** revived unit tests reconciled (argv builders, pipeline
  order/short-circuit, PATH threading, stack-bin resolution, quit
  controller); NEW: a table test pinning that `stackUpArgs` passes no
  `--postgres-image`/`--listen`/`--database` (the CLI-defaults contract,
  §A2 delta 2); preflight suite reconciled — minus the DB and uid checks
  (§A2 delta 1, §A3 delta 3), PLUS a table test for the delta-4
  podman-version gate (below-floor → FATAL with the "podman N.N or newer"
  copy; at/above-floor → pass), over the injected version-probe effect so it
  runs hermetically. Reconcile `bringUpTimeout` for the DL-260 cold start:
  the pre-retirement value (60s, `main.go:49` at `efea2d29a1b9-`) and its
  error copy ("a cold agent-image pull from GHCR can take longer on first
  run", `embedded.go:238`) assume ONE image — under DL-260 the cold path
  pulls agent + postgres + collector (three images); T-2 re-sizes the value
  and rewrites the copy to name all three (darwin machine-init time is
  separately owned by A5's ensure step, T-6). Manual smoke on a Linux box:
  absent app.toml → launch → stack Ready → board renders → **one agent
  session actually runs** (a green board with a dead runner is the delta-4
  failure mode; the smoke must exercise the runner, not just render) →
  Quit-and-stop → `compass-stack status` reports down. Module gates green.

### T-3 — UI embedded boot arm

- **Do:** in `apps/ui`: add the embedded connection arm to the
  `index.tsx:63-66` fork — `shellMode() === "embedded"` resolves a direct
  bridge connection (no probe/connect screen/bearer), `"client"` keeps
  `bootNativeClient`, `undefined` keeps the browser env provider. The
  `"embedded" | "client"` union in `shell-globals.ts:16` already carries the
  value.
- **Interfaces:** consumes `shellMode(): "embedded" | "client" | undefined`
  and the T-2 startup JS injecting `"embedded"`. Produces the three-way boot
  fork and an embedded `ConnectionProvider` over the existing shell IPC
  (`compass_rpc` frames are mode-agnostic, DL-107).
- **Test cycle:** bun tests for the fork's three arms (the
  `boot-native.test.ts` injection pattern); UI typecheck green; the gtk e2e
  suite green unchanged (it drives client mode).

### T-4 — Bundle re-scope: sidecars return (Linux), .app staging (macOS)

- **Do:** `app-bundle/build.sh`: re-add `compass-stack`, `compass-server`,
  `compass-runner` to the staging loop (the `for b in compass-app` sanity
  loop widens back), all stamped with the one version ldflag; NO
  `compass-postgres`, NO postgres store symlinks (§A4). Extend the macOS
  bundle lane (compass-distribution T3's macos-bundle tool) to stage the
  three sidecars in `Contents/MacOS/` beside the shell so
  `resolveStackBin`'s sibling probe hits. moon `inputs` re-widen to the
  sidecar sources.
- **Interfaces:** consumes T-2's `resolveStackBin` sibling contract and
  `prependExecDirToPath`. Produces
  `compass-app-<v>-linux-amd64.tar.gz` with `bin/{compass-app,
  compass-stack,compass-server,compass-runner,dist/}` and the macOS `.app`
  with the same four binaries under `Contents/MacOS/`.
- **Test cycle:** `moon run compass-app-bundle:build` green from a clean
  checkout; tarball sanity gate asserts all four binaries + uniform
  `--version`; unpacked-tarball smoke: launch with absent app.toml on a
  podman-capable Linux box → embedded stack Ready with NOTHING on $PATH
  (proves PATH threading + sibling resolution).

### T-5 — Smoke/e2e gate retarget

- **Do:** rewrite `app-bundle/SMOKE.md` (currently the client-only runbook:
  "It spawns, supervises, and tears down **no** stack", `SMOKE.md:10-14`)
  as a two-part smoke: (a) embedded — unpack → launch (no app.toml) → stack
  up → one agent session runs → quit-and-stop; (b) client — point app.toml
  at a live headless stack → connect → board renders (the existing
  procedure, kept). Add an embedded-launch e2e leg to the app's CI lane
  where podman exists (the dogfood `e2e` job,
  `.github/workflows/ci.yml:1614`, runs the full-stack tier under rootless
  podman in a privileged `quay.io/podman/stable` container
  (`ci.yml:1648-1649`) and "stands up its OWN private postgres
  (go/cmd/compass-postgres) inside the stack" (`ci.yml:1634-1635`); the
  new leg drives the same bring-up THROUGH the app's pipeline seams rather
  than the CLI). The dogfood e2e job's
  headless framing stays — it gates `compass-stack` itself.
- **Interfaces:** consumes T-2 seams + T-4 artifacts. Produces the smoke
  definitions both modes are judged by.
- **Test cycle:** CI green; SMOKE.md executed once end-to-end per OS target
  in the release checklist (Linux now, macOS after T-6).

### T-6 — macOS podman-machine provisioning (start early; gates darwin GA)

- **Do:** FIRST the spike (§A5): on a clean macOS box/runner, script
  `podman machine init --memory <floor> --disk-size <floor>` → `start` →
  `compass-stack up` → agent container runs; validate the stack's OWN
  socket topology end-to-end across the VM boundary (the DL-260 postgres
  DSN socket-dir bind mount and the runner `--runtime-dir` agent sockets
  over virtiofs — OQ-7), and verify the §A5 podman-CLI assumptions
  (`machine inspect` socket path, `machine ls --format json`
  no-machine-vs-stopped, CLI-side machine-connection resolution); record
  findings (timings, socket behavior, resource floor) in this directory as
  `macos-podman-machine-spike.md`. THEN productize: darwin preflight
  adapter (`MachineReady`), the ensure step (init/start + re-probe) behind
  an injected seam, the UI provisioning state for the minutes-long first
  init, and the §A3 darwin check wiring (machine fatal; no uid check, §A3
  delta 3).
- **Interfaces:** consumes `podman machine
  init|start|ls --format json|inspect`. Produces
  `preflight.Deps.MachineReady func(ctx) error` (+ the darwin adapter) and
  the ensure orchestration in the embedded pipeline's darwin path.
- **Test cycle:** unit tests over the injected machine seam (no-machine /
  stopped / running / init-fails); the spike script re-run green on the
  darwin CI runner (DL-263's macos-14 lane); SMOKE.md part (a) executed on
  a Mac.

### T-7 — Docs, banners, ledger, citation sweep

- **Do:** per the freeze rule (add, never rewrite frozen prose): append a
  supersession banner to `compass-native-client-only/design.md` (premise
  falsified; DL-235/237/238 superseded, DL-236 split — pointing here);
  annotate `compass-native-app/design.md`'s DL-106 banner (dual-mode shape
  restored under a new rationale); banner/defer the two
  microVM-sole-runtime artifacts (§Alternatives): a defer-to-#804 note on
  `microvm-runner.md` D2 ("the container path is removed and microVM
  becomes the sole runtime") and a fix for the runner flag-help comment
  (`compass-runner/main.go:229-230` "'podman' (default, transitional)") —
  no frozen artifact may keep asserting the falsified sole-runtime
  end-state. Land the §Ledger-impact rows +
  status-cell flips in `docs/designs/DECISIONS.md` (driver applies; same
  PR as the record per Global Constraint 8). Citation sweep: update the
  RIG-2554/DL-235..238 comments found this session —
  `go/cmd/compass-app/client.go:4-6`, `client_test.go:8,172`,
  `main.go:9-13,190-196,208-209`, `appconfig.go:14-17,96-100,155-157`,
  `appconfig_test.go:128-129`, `appconfig/doc.go:4-6`,
  `bridge/pump.go:80-82`, `app-bundle/SMOKE.md:10-14` — most die with
  T-1..T-5's edits; this task greps for stragglers
  (`DL-23[5-8]|RIG-2554`) and files a follow-up for any outside this
  record's lane. Update the self-host doc with the graduation section
  (§A1).
- **Interfaces:** consumes this record (frozen). Produces the ledger delta,
  banners, doc updates.
- **Test cycle:** design-ledger-gate green; banner links resolve; zero
  stale normative RIG-2554-retirement claims in the swept set; driver
  review.

## Tasks

- [ ] **T-1** appconfig dual-mode revived: embedded default, override
  plumbing back, client arm unchanged; dispatch skeleton; tests green.
  (Same PR as T-2.)
- [ ] **T-2** Supervisor pipeline revived and reconciled (no DSN duplicate,
  no DB probe, CLI-defaults contract pinned, gtk4/darwin tags, quit
  controller + menu); Linux manual smoke green. (Same PR as T-1.)
- [ ] **T-3** UI three-way boot fork with the embedded provider; bun tests
  and typecheck green.
- [ ] **T-4** Bundle carries the three sidecars (Linux tarball + macOS
  .app), no postgres tooling; clean-checkout build + PATH-threading smoke
  green.
- [ ] **T-5** SMOKE.md two-part rewrite + embedded e2e leg in CI.
- [ ] **T-6** macOS podman-machine spike recorded, then provisioning
  productized (detect/init/start/UI state); Mac smoke green.
- [ ] **T-7** Banners, ledger rows, citation sweep, self-host doc
  graduation section.

## Ledger-impact (proposed rows — the driver lands them in the same PR)

ID allocation: the ledger tail read this session is **DL-317** (the
per-organization handle-uniqueness row) and **DL-318 is reserved** by
compass-obs's #804 (trust-model boundary). New rows here are allocated
**DL-319..DL-321**. Final ids MUST be re-verified against main's
then-current ledger at merge — a stale-base ledger collision bit this lane
before (RIG-1746 #578); the driver re-greps the tail before landing.

New rows:

| ID | Decision | Status | Record |
| --- | --- | --- | --- |
| DL-319 | The Compass native app is DUAL-MODE again: `mode="embedded"` returns as the low-friction onboarding / local-dev front door — the app spawns/supervises a LOCAL stack via rootless podman on the user's own machine (macOS via podman machine, Linux native; Windows/WSL deferred) — ADDED ALONGSIDE the fully-surviving client mode, which stays first-class and is the RECOMMENDED steady-state for real self-host (always-on VPS/EC2 running `compass-stack up`, reached over TLS). Rationale is the trust model (DL-318): single-tenant operator-own-code needs no KVM isolation; podman is a permanent supported tier. Supersedes DL-235 (the "client is the ONLY mode" thesis — the exclusivity dies, the client surface survives whole) and the app-never-spawns half of DL-236 (whose standalone-`compass-stack`-CLI half stays load-bearing as client mode's target); partially supersedes DL-259 by citation (split, the DL-236 pattern): DL-259's "KVM-capable Linux machine" floor clause is superseded — the podman tier needs no KVM, including a KVM-absent VPS — while its host-level-bring-up/no-compose/install-surface clause stays Active and load-bearing; restores DL-106's dual-mode SHAPE under this new rationale (the old KVM-era premise is not restored) | Proposed | this record §Problem/§Topology/§A1 |
| DL-320 | app.toml is dual-mode: absent → embedded (the zero-config onboarding default returns); `mode="embedded"` accepts no server_url/ca_cert; `mode="client"` keeps the built contract (https-only server_url required, optional ca_cert, keychain-first bearer per DL-109); the `--mode`/`$COMPASS_APP_MODE` override returns (flag > env > file > default). Graduation embedded→client is a config edit documented in the self-host doc, not an in-app flow. Supersedes DL-237 | Proposed | this record §A1 |
| DL-321 | The app bundle carries embedded's sidecars again — `bin/{compass-app,compass-stack,compass-server,compass-runner}` + dist, PATH-threaded (DL-215's mechanism restored by citation) — but NO postgres tooling and no `compass-postgres` sidecar: the embedded stack's postgres is the DL-260 stock `postgres:18` container via rootless podman (the CLI's own default), leaving rootless podman (plus podman machine on macOS) as the packaged embedded mode's sole container prerequisite. DL-217 STAYS superseded. macOS stages the same four binaries in `Contents/MacOS/`. Supersedes DL-238 | Proposed | this record §A4 |

Status flips on existing rows (driver edits `DECISIONS.md`; partial
supersessions follow the DL-213/DL-183 stays-Active-with-citation pattern):

- **DL-235** (client-only thesis) → `Superseded by DL-319`. The client
  SURFACE it kept is carried forward whole by DL-319's text; only the
  exclusivity dies.
- **DL-236** → **stays Active** (partial supersession, split by citation in
  DL-319): the "`compass-stack` survives … as the standalone headless
  single-user bring-up CLI" half SURVIVES and is load-bearing (client mode's
  recommended self-host target); the "the app never spawns, supervises, or
  tears down a stack" half is superseded by DL-319. A blanket Superseded
  flip would misread the surviving CLI half as dead.
- **DL-237** (client-only app.toml) → `Superseded by DL-320`.
- **DL-238** (thin-client bundle) → `Superseded by DL-321`.
- **DL-106** (dual-mode charter) → stays `Superseded by DL-235`; DL-319's
  cell records the shape-restoration by citation (the row chain
  DL-106→DL-235→DL-319 reads correctly end-to-end; no resurrection edit).
- **DL-108** (supervisor design) → stays Active untouched — this record
  re-wires its invocation, exactly the half DL-236 said retired.
- **DL-109** → stays Active (keychain half live; the embedded-when-absent
  default its mode-selection half described is restored by DL-320's
  citation, unwinding DL-237's partial supersession).
- **DL-215** (sidecar bin/ + PATH threading) → stays `Superseded by
  DL-238`; DL-321 records the mechanism-restoration by citation (same
  no-resurrection pattern as DL-106).
- **DL-217** (bundled postgres tooling) → stays `Superseded by DL-238`;
  NOT restored — DL-260 is the postgres mechanism (DL-321 says so).
- **DL-257/DL-258** (per-OS matrix, brew channels) → stay Active; the
  matrix's artifact CONTENT changes per DL-321 (T-4), the matrix and
  channels do not.
- **DL-259** → **stays Active (partial supersession, split by citation in
  DL-319 — the DL-236 pattern)**. DL-259's cell reads verbatim: "The
  self-host stack stays a host-level bring-up on a KVM-capable Linux
  machine (`compass-stack up`; microVM D3 hard-fail consumed, no
  compose/Swarm packaging); `compass-stack` joins the release binary
  matrix, and the flake + preflight + self-host doc are its install
  surface". The "KVM-capable" floor clause is SUPERSEDED by DL-319 (funnel
  entry 2's podman tier runs on a KVM-absent VPS); the
  host-level-bring-up / no-compose / install-surface clause SURVIVES,
  load-bearing. The cell itself is immutable — "a new ruling is a new row
  plus a `Superseded`/`Retired` flip on the old, never an in-place reword"
  (DECISIONS.md Conventions) — so the split lives in DL-319's cell, never
  in a prose reinterpretation here. If merge order inverts (Global
  Constraint 6's freeze-order), the split may alternatively park on #804's
  DL-318 row.
- **DL-260** (containerized postgres) / **DL-262** (pgid v2) → stay Active
  — they are the mechanisms the revived supervisor rides (§A2/§A4).
- **DL-318** (trust-model boundary, compass-obs, #804) → consumed, not
  touched.

Citation-sweep obligation: §Plan T-7 (the RIG-2554/DL-235..238 comment sweep
enumerated there from this session's grep).

## Open Questions

All designed-against-assumption per the batched-clarifications rule; each
load-bearing OQ carries a recommendation for Matt's gate.

### OQ-1 [load-bearing] — postgres for embedded: container (DL-260) vs bundled tooling (old DL-217)

The record designs against **container-postgres** (§A4): the embedded stack
uses `compass-stack`'s own default — the DL-260 stock `postgres:18`-by-digest
container via rootless podman — and the bundle ships no postgres tooling.
Rationale: podman is already embedded's hard prerequisite (agents run in
containers), so this adds no new host requirement; bundling the nix
postgresql closure would fight the CLI's default and re-add the closure
weight/staging DL-238 deleted; and one postgres mechanism across installed
stack and embedded app means one teardown grammar (pgid v2) and one bump
procedure (the Renovate digest pin). Cost acknowledged: first embedded launch
pulls the postgres + collector images alongside the agent image (cold-start
bandwidth) — NOT covered by the existing bring-up timeout copy: the revived
copy names only "a cold agent-image pull from GHCR can take longer on first
run" and the 60s value assumed one image; T-2 reconciles both against the
three-image cold start. **This recommendation is CONDITIONAL on the T-6
spike's darwin socket findings (OQ-7): if the stack's own AF_UNIX sockets do
not survive the host↔VM virtiofs boundary and darwin needs TCP or
`--database-external`, the "one postgres mechanism, one teardown grammar"
rationale above is undercut and this OQ re-opens.** **Recommendation:
container (DL-260), conditional as stated. Confirm.**

### OQ-2 [load-bearing] — is embedded the absent-app.toml DEFAULT?

The record designs embedded as the zero-config default (§A1): absent app.toml
→ embedded, restoring the original DL-109 posture, because the funnel promise
is install → launch → go and a first-run error (today's behavior) is the
opposite of an easy front door. **The default's cost, stated honestly: it is
side-effectful.** On macOS an absent-app.toml first launch runs `podman
machine init` (§A5 — a multi-minute, multi-GB VM image download), then the
DL-260 three-image cold pull (agent + postgres + collector, OQ-1's
cold-start bandwidth), then postgres cluster init — with zero user consent.
On Linux, a client-mode user with an absent app.toml (fresh machine, moved
dotfiles) gets a full local stack instead of a legible "configure a server"
path: preflight's redirect copy fires only when podman is MISSING, so on any
podman-capable box the misfire is silent and heavyweight. The alternative —
absent → a chooser screen ("run locally / connect to a stack") — adds one
decision to first-run but avoids both. **Recommendation: embedded stays the
default, but with a one-time first-run CONFIRM before the side-effectful
work (one dialog gating machine-init/image-pull; a confirmed run never asks
again) — cheaper than a full chooser, and it converts the silent misfire
into a legible fork. Confirm the default + the first-run confirm (Matt
signalled "onboarding default" — recording it as ruled once confirmed at
the gate).**

### OQ-3 [load-bearing] — does the `--mode`/`$COMPASS_APP_MODE` override return?

The record designs it back in (§A1, T-1): with two real modes the override
has real users again — a dev pointing a laptop at a remote stack without
editing app.toml, CI driving client mode against a harness stack, and the
graduation trial run ("try client mode against my new VPS before I commit
the config edit"). DL-237 retired it only because "an override with one
valid value is dead weight" — a rationale that dies with the second mode.
**Recommendation: yes, restore flag > env > file > default. Confirm.**

### OQ-4 [load-bearing] — Windows/WSL scope

§A3/§A5 target macOS + Linux; Windows is out of this record's plan. Podman
on Windows also runs a machine (WSL2 backend), so the T-6 seam
(`MachineReady` + ensure) is the right shape for it later, but the shell
itself has no Windows build today (`main.go:1`:
`//go:build (linux && gtk4) || darwin` — no windows arm exists to put an
embedded mode in), making Windows-embedded gated on a whole Windows shell
lane, not on this record. **Recommendation: defer Windows to a follow-up
record once a Windows shell exists; note it in the funnel table as
"later". Confirm the deferral.**

### OQ-5 [load-bearing (ordering half), cross-lane] — #804 freeze order + citation wiring

**Ordering half — load-bearing.** The record's central premise (podman as a
PERMANENT supported tier) rests entirely on DL-318/#804, which is NOT yet
frozen — every load-bearing choice here cites an unratified row. This is a
freeze-ORDER dependency: per Global Constraint 6, this record merges AFTER #804
(DL-318 pinned first; this record rebases and re-verifies
DL-319..DL-321 against the frozen row). Degrade path if the order inverts:
DL-319's rationale clause degrades to "Matt 2026-09-01 ruling" with the
DL-318 cite added post-#804-freeze.

**Citation-wiring half — non-load-bearing.** compass-obs PR #804
(`compass-runner-adoption-strategy/design.md`) is being reshaped now (its
current text still carries the DL-235-era "there is no embedded mode");
this record drafts against the ruled direction. Once #804 freezes, wire in:
(a) the frozen **DL-318** row id + its section anchor for the trust-model
boundary (§Topology and §Alternatives cite it descriptively today), and (b)
the cross-ref between #804's mac podman-machine spike OQ-3 and this
record's T-6 spike (one spike, two consumers — whichever lane runs it first
records `macos-podman-machine-spike.md` and the other cites it).
compass-obs pings when it lands; only the MERGE ORDER blocks on it.

### OQ-6 [non-load-bearing] — quit-anyway on failed `compass-stack down`

The pre-retirement `quitController` shipped quit-anyway ("on a down error we
log at slog.Error and call quit() REGARDLESS … a lingering stack is the SAFE
failure (it is the plain-quit default anyway)") with the alternative
(abort the quit, surface the error) parked for Matt — a fork the retirement
mooted before it was ruled. The revival keeps quit-anyway as built.
**Recommendation: keep quit-anyway; close the old parked fork with this
record.**

### OQ-7 [load-bearing] — darwin DB/agent-socket transport under podman machine

On macOS, compass-server + compass-runner are darwin HOST processes while
postgres + the agent containers run INSIDE the podman-machine Linux VM. The
DL-260 postgres contract is a host socket-dir bind-mounted into the
container over the byte-identical AF_UNIX socket
(`go/internal/stack/postgres_container.go:45-48`: "the host unix-socket
directory (the DSN `host=`) bind-mounted into the container at the SAME
path … the identical `host=<SocketDir>` DSN over the byte-identical socket
the container's postgres binds"), with the DSN default at
`go/cmd/compass-stack/main.go:271-274` (the `pgsock` sibling dir); the
runner's per-container agent sockets ride the same shape
(`go/internal/stack/spec.go:52` `--runtime-dir`). On podman machine that
bind source is a virtiofs share, and AF_UNIX sockets do not work across a
virtiofs/VM boundary — §A5's only socket claim covers the podman API
socket, not the stack's own sockets. Question: do the stack's own AF_UNIX
sockets survive the host↔VM boundary, or does darwin need a different
transport (TCP over a forwarded port, or `--database-external`)? The T-6
spike answers it (an explicit spike goal, not just provisioning); OQ-1's
container-postgres recommendation is conditional on the answer — the
TCP/external-DB fallback would undercut its "one postgres mechanism, one
teardown grammar" rationale. **Recommendation: spike first (T-6); no
darwin GA before the socket topology is validated end-to-end.**

### OQ-8 [load-bearing] — add a podman-version FATAL preflight check?

The draft affirmatively chose NOT to preflight the podman ≥ 4.3 floor,
reasoning the runner already enforces it at startup. That reasoning is wrong
in embedded mode: the runner's refusal is swallowed by the fire-and-return
supervisor seam and the exit-0/stderr-unread path (§A3 delta 4 carries the
full reachability trace: `stack.go:283-291` runner-spawn-last, `stack.go:
265-267` Ready-before-runner, `process.go:71` `cmd.Start()`, `embedded.go:
224-246` exit-0 stderr-unread, `postgres_container.go:171` plain
`keep-id`). So on a rootless-podman-<4.3 host (Ubuntu 22.04 LTS = 3.4.4)
embedded launches apparently-green but agent-dead with no legible error.
§A3 now designs the check IN (delta 4) — a cheap host-capability probe,
exactly preflight's remit, reusing `runtime.VerifyUsernsRemapSupport`
(`podman.go:504`). This OQ flags that as a REVERSAL of the draft's earlier
no-check ruling, for Matt's gate. The alternative (leave it out, lean on
T-5's runner-alive smoke to catch it) only catches the break on whatever
podman version the CI smoke host runs, never on a user's old box.
**Recommendation: add the check (§A3 delta 4) — the failure is silent,
common, and defeats the easy-front-door thesis; the probe is one `podman
version` call the codebase already makes.**

### OQ-9 [load-bearing] — docker-socket support in embedded, or force podman?

Matt raised it: most users have Docker (or a docker-compatible engine) but may
not have podman — should embedded speak a docker socket, or force a podman
install? **Recommendation: force podman; no docker socket.** The agent-isolation
trust boundary this whole reversal rests on is podman-specific: each agent runs
`--userns=keep-id:uid=,gid=` (a podman 4.3+ option with NO fallback,
`go/internal/runtime/podman.go:487-495`) so the invoking host user maps to the
baked agent uid and files the agent writes map back to the host user
(`podman.go:25-27`), on rootless podman which is "a hard requirement … no
daemon, no root, no rootful fallback" (`podman.go:24`). Docker's userns
remapping is DAEMON-GLOBAL (`daemon.json` `userns-remap`), not a per-container
keep-id, so that contract cannot be reproduced on docker. `WithProgram("docker")`
exists only "in a pinch" (`podman.go:433-435`); the podman-preferred /
docker-fallback `containerCLI` in `go/internal/pgshare/pgshare.go:202-208` is
TEST infra (a shared-schema harness helper), not the production isolation path.
Supporting a docker socket would fork every runtime adapter and the pgid-v2
teardown grammar for a backend
that cannot supply the security boundary, and Docker Desktop adds a
commercial-licensing surface (already rejected as the macOS runtime,
§Alternatives). Cost of force-podman, acknowledged: a docker-only user must
install podman (`brew install podman` / `apt install podman`) — the §A3
podman-present FATAL preflight already surfaces that requirement legibly at the
front door rather than deep in a failed container create. **Recommendation:
force podman; the preflight names the requirement; no docker socket. Confirm.**

### OQ-10 [load-bearing] — apple/container as a macOS backend?

Matt flagged Apple's newer `container` framework
(`github.com/apple/container`) as a macOS option. It runs Linux containers as
lightweight VMs on Apple silicon and consumes/produces OCI images, but (read
this session from its README): **macOS 26 + Apple silicon ONLY** — it
explicitly does not support older macOS — and it is **pre-1.0 with declared
breaking changes between minor releases until 1.0.0**, shipping its own CLI and
system service. Adopting it as the v1 macOS backend would strand Intel Macs and
macOS earlier than 26 and chain the front-door promise onto a churning pre-1.0
API. podman-machine (§A5) is podman's own supported macOS shape, reuses the
ENTIRE existing rootless-podman adapter stack unchanged (the userns-remap
contract holds inside the machine's Linux VM), and works on Intel + Apple
silicon + older macOS. Note it is purely a macOS CONTAINER-backend option — it
does not touch the runner's Linux-KVM microVM backend (compass-obs's #804).
**Recommendation: ship podman-machine for v1 (§A5); track apple/container as a
post-1.0 alternative macOS backend — revisit when it reaches 1.0, in a
T-6-adjacent spike measuring init/startup latency + memory vs podman-machine
(out of this record's scope). Confirm the defer.**

### OQ-11 [load-bearing] — embedded microVM backend on Linux: v1 or follow-up?

Matt: for embedded, "microVM should be usable if the user is on raw Linux." The
runner already carries both backends (`SelectBackend`,
`go/internal/runtime/microvm.go:97-118`; `compass-runner --backend
podman|microvm`, `compass-runner/main.go:228-230`), so a KVM-capable Linux
embedded user COULD run the microVM runner locally. But the embedded supervisor
pipeline (§A2) execs `compass-stack up` with NO backend flag — the pass-no-flags
CLI-defaults contract (§A2 delta 2) — so the runner defaults to podman.
Making embedded microVM-capable means threading a backend choice through the
app → `compass-stack up` → `compass-runner` chain plus the microVM wiring flags
(vmm/virtiofsd/kernel/rootfs/initrd paths, `compass-runner/main.go:231-248`),
which the bundle would then have to carry or the user supply — real new surface.
**Recommendation: v1 embedded is CONTAINER-ONLY (podman)** — it is the
onboarding / local-dev front door where zero-config matters most, and the
PRIMARY embedded target (macOS) cannot do microVM anyway; embedded-microVM-on-
Linux is a follow-up record once the runner's microVM backend GAs and the
wiring/bundle story is designed. The §Topology matrix records microVM as
"usable where the host has KVM" as DIRECTION; v1 builds the container path.
**Confirm the v1 container-only embedded scope + the follow-up.**
