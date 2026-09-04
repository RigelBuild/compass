# Compass self-host stack supervision: constant-on cross-platform service

Status: Draft
Issue: RIG-3239

## Problem / Intent

Decide how the long-lived self-host stack services (compass-server,
compass-runner, the containerized postgres, the bundled OTel collector) run
as a **constant-on, auto-restarting service on all platforms** — Linux and
macOS — with one-command install, auto-start at reboot, and whole-stack
crash recovery. The supervision-mechanism fork (Podman **Quadlet** vs the
hand-rolled **DL-183/DL-262 pgid** mechanism `compass-stack` ships today) is
part of this decision, with the Docker-socket engine recorded as
considered-and-declined at the stack layer. Scope is bring-up order,
teardown, restart policy, crash recovery, boot-start, and rootless posture
of the stack services ONLY — the per-session runner backend is frozen out of
scope (RIG-3070: podman permanent for self-host, microVM behind the seam;
see `ui/compass-native-embedded-revival/design.md:71-74`), as is the macOS
*embedded runner* backend (sibling RIG-3238 Apple-container record). One
COUPLING crosses that fence and is grounded below: "constant-on service on
macOS" shares the runner-on-darwin / VM-topology unknowns that RIG-3238
carries as its OQ-12 and the embedded-revival record carries as its OQ-7.

## Approach

**Ruled (Matt, 2026-09-04 — the OQ-1/OQ-2/OQ-3 forks are decided, see
Resolved decisions): keep the hand-rolled DL-183/DL-262 pgid supervision as
the SINGLE cross-platform supervision model; do not adopt per-service
Quadlet units (Quadlet is Linux/systemd-only and structurally cannot meet
the all-platforms bar). Build REAL whole-stack crash recovery as a blocking
`compass-stack up --supervise` foreground mode, and ship a one-command
`compass-stack service install` that writes + enables the platform-native
supervisor unit — a systemd USER unit on Linux, a launchd LaunchAgent on
macOS — with auto-start at reboot and restart-on-crash. The OS supervisor
supplies ONLY restart/backoff/boot-start; DL-183 spawn order and
identity-token teardown remain the sole bring-up/teardown mechanism.**
Docker-socket is declined at the stack layer, mirroring its
per-session-runner rejection.

### What exists today (grounded)

The supervisor is `compass-stack` (`go/cmd/compass-stack/main.go:8-9`):

> ```go
> //   - up:     bring the embedded stack to Ready (or attach to a live one) and
> //     return once ready; the children keep running (up does NOT block).
> ```

The bring-up is an ordered, partly IMPERATIVE chain
(`go/internal/stack/stack.go:89-93`):

> ```go
> // Cold sequence (devenv.nix:122-143): private postgres up+reachable → TLS anchor
> // (expiry-aware) → compass-server → poll GetServerInfo readiness → runner token
> // (idempotent 0600) → agent image present → compass-runner (token via env). On
> // any step failure the children started so far are drained and the lock
> // released, so no half-started stack leaks.
> ```

Cross-process teardown is the DL-183 pgid record
(`go/internal/stack/pgidfile.go:14-18`):

> ```go
> // pgidFileName is the state-dir record of the child process groups a successful
> // up spawned, beside stack.lock / stack.lock.guard. A fresh down (which holds no
> // in-memory Process handle for a stack a prior up spawned) reads it to learn
> // which groups to signal. It is removed on a fully successful teardown.
> ```

with the start-time identity token closing the pid-recycle window
(`pgidfile.go:71-74`):

> ```go
> //     the process-group id (== the child's pid, set via Setpgid at spawn) +
> //     the group leader's start time as read at spawn. StartTime is the identity
> //     token — it turns the down-side check from "does a group with this pgid
> //     exist" (which a recycled pid passes falsely) into "does a group with this
> ```

DL-262 extended the record to a v2 kind-tagged discriminated union for the
containerized children (`pgidfile.go:23-26`):

> ```go
> // v2 (this build) grows the entry line into a kind-tagged discriminated union
> // (proc / ctr, see pgidEntry): the container-backed postgres of S4 has no
> // process-group teardown identity, so it is recorded and torn down by container
> // name instead.
> ```

and the down side (`go/internal/stack/downdetached.go:62-68`) reads the record,
identity-checks each group, SIGTERMs in reverse start order with bounded
SIGKILL escalation, and confirms per component:

> ```go
> // reads the persisted pgid record, identity-checks each recorded group, SIGTERMs
> // the live ones in reverse start order with bounded SIGKILL escalation, and
> // confirms teardown per component by the channel each has (server/postgres by
> // socket quiescence, the socketless runner by group-ESRCH). Only pgids read from
> // this stack's own state-dir file are ever signaled, and each group's identity
> // (pgid + leader start-time token) is re-verified immediately before every
> // signal.
> ```

The mechanism is already Linux-anchored: the start-time reader is
`/proc`-backed (`pgidfile.go:333-336`):

> ```go
> // The wired implementation reads /proc/<pid>/stat, which exists
> // only on Linux — and the embedded stack is Linux/podman-only at runtime anyway
> ```

The substrate invariant is rootless podman
(`go/internal/runtime/podman.go:23-27`):

> ```go
> // PodmanCLI, its rootless-podman-CLI implementation. Rootless is a hard
> // requirement (design: architecture-lineage): no daemon, no root, no rootful fallback.
> // Containers run with --userns=keep-id:uid=<agent-uid>,gid=<agent-gid> so the
> // invoking host user is mapped to the baked agent uid; files the agent writes
> // in a bind-mount still map back to the invoking user on the host.
> ```

and the stack's own postgres container couples the keep-id user mapping to the
frozen DSN contract
(`go/internal/stack/adapters/postgres_container.go:44-48`):

> ```go
> // role a user-less DSN connects as so the frozen S4 DSN (host=<dir> port=<p>
> // dbname=compass sslmode=disable — no user=) authenticates: pgx resolves a
> // user-less DSN to the OS user, and under --userns=keep-id the container
> // runs as that same host user, so the createdb superuser must be it too.
> ```

**What today's mechanism does NOT do: crash recovery.** `Up` returns once the
stack is Ready and nothing supervises afterward — health is a probe-on-demand,
not a monitor loop (`stack.go:198-200`):

> ```go
> // Health probes current readiness by asking the server over the socket. An
> // answering probe is Ready (or Attached, for a stack that never spawned); a
> // failing probe is Failed with the probe error as the detail.
> ```

A crashed compass-server stays down until the operator reruns `compass-stack
up`. Matt's ruling rejects accepting that gap ("a VPS doesn't fix a crash"):
the supervise mode below closes it.

### The Quadlet option, factually

Quadlet is podman's systemd generator: `.container`/`.pod`/`.volume`/`.network`
unit files generated into regular systemd services, with `[Unit]` dependency
translation (`After=`/`Requires=` between Quadlet units), systemd `Restart=`
policy, sdnotify readiness (`Notify=true` → `--sdnotify container`), and
boot-start via a generator-applied `[Install] WantedBy=` section. Rootless is
supported as systemd USER units: unit files under
`~/.config/containers/systemd/` (and the other rootless search paths), run in
the user's session — explicitly NOT via `User=` in a system unit ("Quadlet
units do not support running as a non-root user by defining the User, Group, or
DynamicUser systemd options. If you want to run a rootless Quadlet, you will
need to create the user and add the unit file to one of the above rootless unit
search paths"). Quadlet requires cgroup v2. Source:
<https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html> (read
this session). So the rootless/no-daemon substrate invariant DOES hold under
Quadlet user units — podman stays daemonless; systemd (already pid 1) is the
supervisor, not a container daemon.

### Why pgid stays the single model (ruled)

1. **The all-platforms bar — Quadlet structurally fails it.** Matt's ruling
   requires a constant-on service "on all platforms": Linux AND macOS.
   Quadlet needs systemd, and macOS has no systemd — so Quadlet cannot even
   express the macOS half of the requirement, while the pgid mechanism is
   portable (setpgid / SIGTERM / `kill(-pgid)` all exist on darwin; the
   start-time identity read is a bounded per-OS swap behind the existing
   `readStartTime` var seam, `pgidfile.go:331-338` — T3 below). This is a
   second, independent reason on top of the two below.
2. **Two supervision models vs one — the maintenance crux.** Even on Linux,
   adopting Quadlet means maintaining, testing, and keeping
   behaviorally-equivalent TWO supervision models (unit-file
   bring-up/teardown semantics AND the pgid record) for the same four
   services, permanently: the embedded front door (DL-319 dual-mode app,
   `DECISIONS.md:298`: the app "spawns/supervises a LOCAL stack via rootless
   podman on the user's own machine (macOS via podman machine, Linux
   native)") cannot ride systemd on macOS at all, and dev/devenv +
   non-systemd Linux hosts also can't — so the DL-183 path cannot retire
   while those tiers exist. One supervision model beats two. DL-183/DL-262
   are load-bearing, frozen, and tested (`DECISIONS.md:281`, `:295`); the
   invariants are non-negotiable in the frozen teardown record
   (`ui/compass-stack-cross-process-teardown/design.md:299-301`):

   > ```text
   > - **Only the exact persisted pgids are ever signaled.** Never a pattern kill,
   >   never a scan of the process table, never a pgid not read from this stack's
   >   own state-dir file.
   > ```

3. **The bring-up chain is imperative, not declarative.** The cold sequence
   (`stack.go:89-93`, quoted above) interleaves containers with host-binary
   children AND side-effect steps: TLS anchor generation (expiry-aware),
   runner token minting (idempotent 0600), agent-image presence, readiness
   polls. Under Quadlet, compass-server/compass-runner are host binaries —
   plain systemd user services, not `.container` units — so "Quadlet" is
   really "Quadlet for 2 containers + 2 plain user units + oneshot
   pre-units for the TLS/token/image steps + sdnotify readiness plumbing in
   the server". That is a full re-plumb of `spawnChain`, not a swap of
   supervisors. And DL-259's operative clause (`DECISIONS.md:292`) names
   **`compass-stack up`** as the bring-up surface; per-service Quadlet units
   would displace that entry point (the operator's verbs become
   `systemctl --user start/stop`), a DL-259 amendment. The service verb
   below keeps `compass-stack` as the verb surface and is squarely inside
   DL-259 (a systemd/launchd unit is host-level configuration, not
   compose/Swarm packaging).

### The ruled design: `--supervise` + `service install`

Two additions, one supervision model.

**1. Blocking `compass-stack up --supervise` — whole-stack crash recovery.**
A new foreground mode: run the existing `Up` to Ready, then BLOCK — watch
the spawned children and exit non-zero when any child dies; tear the stack
down on SIGTERM/SIGINT and exit zero. It builds on the existing handle
contract — every child exposes `Wait(ctx)` / `Signal(sig)` / `Pid()`
(`go/internal/stack/deps.go:136-138`) and `Health` is an on-demand probe
(`stack.go:198-211`) — but the supervise loop is NOT a free consumer of the
core: `Process.Wait` is single-caller, the container children's `Wait` is
120s-bounded, `Wait`'s cancellation path group-SIGKILLs, and a partial
drain orphans a survivor across restart. T1 below specifies the bounded
core change these force (sole `Wait` ownership, container liveness-poll,
non-cancelable teardown ctx, pre-spawn record cleanup). Division of labor:
the stack keeps its single supervision model (DL-183 spawn order,
identity-token teardown, drain); the OS supervisor wraps the ONE blocking
process and supplies only `Restart=on-failure` + backoff (systemd
`Type=exec`; launchd `KeepAlive`). Whole-stack (not per-service) restart is
the right granularity: the cold sequence's ordering means a restarted
postgres needs the server's readiness re-verified anyway. This also
eliminates the two failure modes a returns-at-Ready oneshot wrapper has:
the status-lie (a `Type=exec` unit tracks a live main pid, so a dead stack
reads `failed`, never fake-`active`) and stop-what-you-didn't-start (the OS
supervisor owns exactly the pid it spawned). An ATTACHED supervise (a live
stack already answered the probe, `stack.go:131-146`) owns no child
handles, so the loop degrades to Health-polling with the same exit contract
(exiting only after N consecutive failures, T1).

**2. One-command `compass-stack service install` — the constant-on service.**
A new verb pair extending the `up|down|status|preflight` surface
(`go/cmd/compass-stack/main.go:8-13`): `service install` platform-detects
and renders + installs + enables the native unit; `service uninstall`
stops, disables, and removes it.

- **Linux (systemd user unit)** at
  `~/.config/systemd/user/compass-stack.service`: `Type=exec`,
  `ExecStart=<abs>/compass-stack up --supervise --state-dir <dir> <flags>`,
  `Restart=on-failure` + pinned `RestartSec=5`, `TimeoutStopSec=90` (the
  DL-262 teardown drain worst case is ~80s, `downdetached.go:15-24`),
  `KillMode=mixed` + an explicit start-limit posture (so the ordered drain
  and the crashloop ceiling are chosen, not left to defaults), explicit
  `Environment=PATH=` (user units inherit no login-shell PATH; podman + the
  DL-321 PATH-threaded sidecars must resolve), `[Install]
  WantedBy=default.target`; NO `After=network.target` (a system-manager
  unit the per-user manager ignores; the server binds loopback only); install
  checks/advises `loginctl enable-linger` (mandatory — rootless podman
  requires a real `XDG_RUNTIME_DIR`, which exists only in a lingering or
  logged-in session).
- **macOS (launchd LaunchAgent)** at
  `~/Library/LaunchAgents/com.rigelbuild.compass-stack.plist`:
  `ProgramArguments` = the same absolute `up --supervise` invocation,
  `RunAtLoad=true`, `KeepAlive={SuccessfulExit=false}` (restart on crash,
  stay down on clean exit), `ExitTimeOut=90` (launchd's SIGTERM→SIGKILL
  grace, sized like `TimeoutStopSec`), pinned `ThrottleInterval=` +
  `AbandonProcessGroup=true` (let the ordered drain finish before launchd
  group-kills), explicit `EnvironmentVariables` PATH; installed + started
  via `launchctl bootstrap gui/$UID` and enabled for boot.

Stopping the unit sends SIGTERM to the supervise process, which runs the
normal DL-183 teardown — one code path, no `ExecStop` verb split — provided
the unit template does NOT let the OS pre-empt it: systemd's default
`KillMode=control-group` and launchd's default group teardown would SIGKILL
the children in parallel before the ordered drain, so the templates pin
`KillMode=mixed` / `AbandonProcessGroup=true` (T2). The operator's
`compass-stack down` verb also becomes unit-aware so it stops through the
unit rather than being undone by a restart (T2). Status truth stays
`compass-stack status` (the unit's state is process-liveness, not stack
health); the T2 checklist (former OQ-4, below) is the unit content gate.

### The macOS runtime path (grounded)

Matt has committed macOS to scope ("support this on macOS as well"), so
macOS-support is DECIDED; what needed grounding is the implementation path,
because **the stack currently refuses to run on darwin**: the whole
`internal/stack` + `cmd/compass-stack` tree is `//go:build unix`
(`pgidfile.go:1`, `main.go:1` — it COMPILES on darwin), but the start-time
identity reader is `/proc/<pid>/stat`, Linux-only, and "on a non-Linux unix
this reader fails and up refuses" (`pgidfile.go:331-338`). Two candidate
paths were examined:

- **Native-darwin supervise (chosen).** The refusal lifts by giving the
  DL-183 identity token a darwin reader — `sysctl KERN_PROC` via
  `golang.org/x/sys/unix` (already in the module, `go/go.mod:96`) reading
  the `KinfoProc` start `timeval`. It is a bounded but TWO-site swap (T3
  below): the token is read both at spawn (`readStartTime`,
  `pgidfile.go:331-338`) and independently at teardown
  (`readGroupLeaderStartTime`, `groupsignal.go:93-98`), and
  `GroupSignaller.Alive` compares them for `uint64` equality — so both
  darwin readers must share ONE encoding (the token is a `uint64`, not an
  opaque string; Linux clock-ticks vs darwin timeval never compare ACROSS
  OSes, but the two readers on one OS must agree). With that swap the
  darwin topology is the one the embedded-revival record already names
  (OQ-7, `ui/compass-native-embedded-revival/design.md:911-912`):
  compass-server + compass-runner run as darwin HOST processes (proc
  entries, group-signal teardown works — setpgid/kill(-pgid) exist on
  darwin), while postgres + agent containers run inside the podman-machine
  Linux VM driven by the host podman CLI (ctr entries, `podman stop`/`rm`
  teardown unchanged).
- **In-VM supervise (declined).** Run the whole `compass-stack` inside the
  podman-machine VM and have launchd drive it over `podman machine ssh`.
  Declined: it puts compass-server inside a VM the client must then reach
  across a VM boundary, adds a second machine-lifecycle layer under the
  unit (launchd → machine → stack), forfeits the DL-319 darwin-host-process
  topology the embedded lane is building toward, and dies the moment the
  RIG-3238 apple-container lane removes podman-machine.

**What the native path does NOT resolve — the RIG-3238 coupling.** The
darwin start-time reader unblocks spawn/teardown identity, but a WORKING
macOS stack end-to-end still depends on unknowns the sibling lanes own:
embedded-revival OQ-7 (the DL-260 postgres DSN and the agent sockets are
AF_UNIX bind-mounts, and AF_UNIX does not cross the virtiofs/VM boundary —
unvalidated) and RIG-3238's OQ-12 ("the runner cannot run natively on
darwin once the podman-machine Linux VM is gone",
`docs/designs/platform/apple-container-macos-runner/design.md:713-733`,
which also notes the runner's podman host-capability preflight has no
darwin answer yet). Those are stack-TOPOLOGY unknowns, not supervision
unknowns — this record ships the supervision mechanics for macOS (T2
launchd template + T3 darwin identity reader) and leaves the topology
validation with the lane that owns it. Whether macOS `service install`
GA-gates on that lane is OQ-5 (the one remaining fork, below). (The
`apple-container-macos-runner/design.md:713-733` line range cited above
resolves only once that sibling RIG-3238 record lands on main — PR #869 is
not yet merged; the citation is a forward reference, not a
same-tree anchor.)

### Alternatives considered

- **Podman Quadlet per-service units (declined — ruled).** What it is:
  `.container` units for postgres/collector, plain systemd user units for
  compass-server/compass-runner, `After=`/`Requires=` ordering, `Restart=`
  crash recovery, sdnotify readiness. Why it loses: Linux/systemd-only, so
  it structurally cannot meet the ruled all-platforms bar (no systemd on
  macOS) AND the DL-183 mechanism survives anyway (two models); retires a
  tested frozen mechanism; requires re-plumbing the imperative cold
  sequence into oneshot pre-units; displaces the DL-259-named
  `compass-stack up` entry point. What it would win: per-service `Restart=`
  crash recovery, boot-start, journald logs, a declarative standard
  operators already know — all of which the ruled `--supervise` +
  `service install` design delivers at whole-stack granularity with one
  model.
- **Accept the crash-recovery gap for v1 (declined — ruled).** The
  originally-drafted posture: zero code, a crashed stack surfaces via
  `compass-stack status` / client connection failure, and the always-on-VPS
  steady state (DL-319) makes silent crashes operator-visible. Matt
  rejected it verbatim ("a VPS doesn't fix a crash … opt 1 reasoning makes
  no sense"): visibility is not recovery, and the macOS half of the
  requirement never had a VPS assumption to lean on.
- **Documented-only thin systemd oneshot wrapper (declined — ruled).** The
  originally-drafted add-on: a copy-pasteable `Type=oneshot` +
  `RemainAfterExit=yes` user unit (`ExecStart=compass-stack up`,
  `ExecStop=compass-stack down`) shipped as a doc section, Linux-only, no
  code change. Matt upgraded it to a first-class product feature ("ship
  with a one command way … on all platforms"). It also carries two edges
  the ruled design eliminates: the status-lie (with `RemainAfterExit=yes`
  the unit reads `active` from the instant `up` returns, so a later child
  crash is invisible to systemd) and stop-what-you-didn't-start (`up`
  attaches to an already-live stack rather than failing,
  `stack.go:131-146`, so the unit's `ExecStop` could tear down a stack a
  manual session started).
- **Non-systemd process supervisors (s6 / runit / supervisord) — declined.**
  Dominated by the same two-models argument (none covers embedded macOS or
  non-systemd-agnostic dev) plus a new third-party runtime dependency the
  self-host tier does not otherwise carry.
- **`compass-stack up` as a thin facade over `systemctl` (declined).** Keep
  the DL-259 verb but have it generate + `systemctl --user` Quadlet units
  under the hood. Rejected: it still stands up the full two-model machinery
  (the pgid path survives for embedded/non-systemd) while adding a
  generation layer, and it inherits every imperative-cold-sequence re-plumb
  of full Quadlet — the facade hides the second model without removing it.
  (Note `service install` is NOT this facade: it renders a unit that wraps
  the pgid supervisor; it never generates per-service units or delegates
  bring-up to systemd.)
- **Docker-socket engine (declined).** A daemon-model engine violates the
  substrate's rootless/no-daemon hard requirement (`podman.go:23-24`, quoted
  above), and docker's userns remapping is daemon-global with no
  `--userns=keep-id:uid=` per-container equivalent — the exact grounds the
  embedded-revival record rejected it on for the runner and the macOS runtime
  (`ui/compass-native-embedded-revival/design.md:963-966`):

  > ```text
  > (`podman.go:25-27`), on rootless podman which is "a hard requirement … no
  > daemon, no root, no rootful fallback" (`podman.go:24`). Docker's userns
  > remapping is DAEMON-GLOBAL (`daemon.json` `userns-remap`), not a per-container
  > keep-id, so that contract cannot be reproduced on docker.
  > ```

  At the stack layer the same holds: the postgres container's DSN
  authentication depends on keep-id (`postgres_container.go:44-48`, quoted
  above), and DL-262's teardown grammar is podman-verbed (`downdetached.go`
  `podman stop`/`rm -f`). Declined for the same "other engines" reasons,
  recorded here for completeness.

## Plan

### Global Constraints

- ONE supervision model only: DL-183/DL-262 pgid spawn + identity-token
  teardown, wrapped by `--supervise`. The OS supervisor (systemd/launchd)
  supplies ONLY restart/backoff/boot-start — never bring-up order, never
  teardown, never per-service units. Never two models.
- Cross-platform: Linux (systemd user unit) AND macOS (launchd
  LaunchAgent). All platforms is the ruled bar; a Linux-only deliverable
  does not close this record.
- Rootless invariant: everything runs as the invoking user; no root units,
  no daemon, no rootful fallback (`go/internal/runtime/podman.go:23-24`).
- DL-259 preserved: `compass-stack` stays the verb surface; no
  compose/Swarm packaging — a systemd/launchd unit is host-level
  configuration, not a packaging format; the self-host doc + flake +
  preflight remain the install surface.
- DL-183 teardown-safety invariants survive untouched: only exact persisted
  identities are ever signaled; verify-before-signal; bounded escalation;
  survivor-rewrite on partial failure
  (`ui/compass-stack-cross-process-teardown/design.md:299-301`).
- Stop grace: `TimeoutStopSec` (systemd) / `ExitTimeOut` (launchd) >= 90s —
  the DL-262 teardown drain worst case is ~80s
  (`downdetached.go:15-24`: "15 + (30+5) + (10+5) + (10+5) = 80s").
- Status truth stays `compass-stack status`: the OS unit expresses
  process-liveness of the supervise pid, never stack health semantics.
- ID allocation + freeze order (ledger-collision guard): this record claims
  DL-328. Main's ledger tail is DL-324; DL-325/DL-326/DL-327 are claimed by
  unlanded PRs (#804/#859 on DL-325, #859 on DL-326, #868 on DL-327), so
  DL-328 is the first free number as of this writing. The driver MUST
  re-grep main's then-current ledger tail immediately before landing and
  take the next free id if DL-328 is taken. The row cites its own decision
  self-containedly (no cross-cite to an unlanded number), so merge order
  cannot falsify it.

### T1 — Blocking `compass-stack up --supervise` mode

Add the foreground supervise mode: `up --supervise` runs the existing `Up`
to Ready, then blocks watching the stack; a child death exits non-zero
(after draining the survivors); SIGTERM/SIGINT runs `Down` and exits zero.

**T1 requires a bounded change to the stack core — NOT "the core
unchanged" (folded from the design-critic red-team, three interacting
contract facts).**

1. **`Process.Wait` is single-caller, so a fan-in Wait plus drain
   double-Waits.** Each `Wait` spawns its own `go cmd.Wait()`
   (`adapters/process.go:132-136`), and `exec.Cmd.Wait` errors on a second
   call; the core's contract is explicitly one sequential caller
   ("drainChildren: Signal then Wait per child", `process.go:84-88`). A
   supervise fan-in holding a `Wait` on EVERY child, then calling
   `drainChildren` (which Signals-then-Waits each survivor), double-Waits
   every survivor → spurious errors corrupt the joined drain result and can
   report a clean drain as failed, which leaves the pgid record in place
   (`stack.go:185-190` removes it only on a nil drain). So the supervise
   loop takes **sole `Wait` ownership**: it holds the one waiter per child
   and, on a death, drives the reverse-order teardown by `Signal` +
   awaiting its OWN already-held wait results, never a second
   `drainChildren` Wait. (Equivalent: make the handle's `Wait`
   idempotent/shared — one waiter goroutine, result fanned out over a closed
   channel. Either is a real core change; "unchanged" is dropped.)
2. **The container children's `Wait` is silently bounded to 120s** —
   `containerProcess.Wait` → `podmanExec.wait` → `fireAndCheck` under
   `context.WithTimeout(ctx, containerCommandTimeout=120s)`
   (`postgres_container.go:20-25,271-278,323-327`); that budget was sized
   for one-shot drain commands. A supervise fan-in Waiting on a HEALTHY
   postgres gets a deadline error after 120s → the loop reads it as a child
   death → drains the whole healthy stack and exits non-zero → OS supervisor
   restarts → a guaranteed whole-stack restart every ~2 minutes. So
   supervise does NOT Wait container children through the bounded adapter:
   it supervises them by liveness poll (`podman container exists` / Health)
   on the same interval, treating only a real exit / "no such container" as
   death, OR via an unbounded `podman wait` path that re-invokes on timeout.
3. **Context discipline (else SIGTERM hard-kills the group before graceful
   teardown).** `Process.Wait`'s cancellation path escalates to
   `syscall.Kill(-pid, SIGKILL)` of the whole group (`process.go:152-169`).
   If the fan-in Waits thread the CLI's `signal.NotifyContext` ctx, SIGTERM
   cancels every in-flight Wait and hard-kills every child in parallel
   BEFORE the DL-183 reverse-order SIGTERM drain runs. So the fan-in Waits
   run on a background / non-cancelable ctx, and on signal the loop runs
   `Down(context.WithoutCancel(ctx))` — the exact pattern `upLocked` already
   uses for its failure drain (`stack.go:153`) — optionally with a fresh
   deadline under the 90s unit stop grace.
4. **Pre-spawn survivor cleanup (else the restart loop systematizes an
   orphan leak).** If a child-death drain partially fails, supervise exits
   non-zero with the pgid record still describing the survivor; the
   restarting `up` never consults that record — `upLocked` probes the socket
   then spawns (`stack.go:136-155`), and the first `recordChild` REWRITES
   the whole file to reflect only the new children (`stack.go:377-400`),
   destroying the old survivor's teardown identity (nothing will ever signal
   it — DL-183 forbids signaling any pgid not read from the current record),
   and a half-dead old server still holding the socket can wedge the new
   spawn into a bind-fail restart loop. So supervise mode gains a pre-spawn
   step: before (re)spawning, run the `downdetached` record-consuming
   cleanup against any existing `stack.pgids` (identity-checked, so a
   post-reboot stale record is a safe no-op, `groupsignal.go:60-70`), or
   refuse to spawn while a record with a live identity-checked entry exists.

Division of labor is unchanged: the stack keeps its single supervision
model (DL-183 spawn order, identity-token teardown, drain); the OS
supervisor wraps the ONE blocking process and supplies only
`Restart=on-failure` + backoff (systemd `Type=exec`; launchd `KeepAlive`).
Whole-stack (not per-service) restart is the right granularity: the cold
sequence's ordering means a restarted postgres needs the server's readiness
re-verified anyway (the crashloop ceiling is the OS supervisor's
start-limit / throttle, pinned in the T2 templates). This also eliminates
the two failure modes a returns-at-Ready oneshot wrapper has: the
status-lie (a `Type=exec` unit tracks a live main pid, so a dead stack
reads `failed`, never fake-`active`) and stop-what-you-didn't-start (the OS
supervisor owns exactly the pid it spawned).

Interfaces:

- Consumes: `stack.Up` / `Stack.Down` (`stack.go:94,183`), the per-child
  `Wait(ctx)` process-handle contract (`deps.go:136-138`), `Stack.Health`
  (`stack.go:198-211`) for the attached-stack degradation and container
  liveness, and the `downdetached` record-consuming cleanup for the
  pre-spawn step — plus the ONE core change above (sole-Wait-ownership or an
  idempotent/shared `Wait`).
- Produces: a `--supervise` flag on the `up` verb in
  `go/cmd/compass-stack/main.go` plus a supervise loop in `internal/stack`
  (a method on `*Stack`, e.g. `Supervise(ctx) error`): fan-in the owned
  process children's held `Wait` results on a non-cancelable ctx, poll
  container-child + attached liveness on an interval; first death → drive
  the reverse-order teardown from the held results and return non-nil (CLI
  exits non-zero, OS supervisor restarts); ctx cancel → `Down`
  (`context.WithoutCancel`), return nil on clean teardown; pre-spawn
  consume any stale record. An attached stack (no owned children,
  `stack.go:131-146`) supervises by polling `Health`, exiting non-zero only
  after N consecutive probe failures (a single transient probe error must
  not tear the stack — the restart then takes ownership of the manual
  stack; interval + N stated in the impl).
- Test cycle (red → green, existing stub harness
  `internal/stack/harness_test.go`): (1) a stubbed child's `Wait` returning
  early causes `Supervise` to drain survivors and return non-nil — with NO
  double-Wait error on the survivors; (2) ctx cancel causes a clean `Down`
  (pgid record removed) and nil, with the drain running on a
  non-cancelable ctx (no group SIGKILL before graceful teardown); (3)
  attached supervise exits non-nil only after N consecutive health-probe
  failures, not one; (4) a partial-drain child death → restart → the
  surviving old child is torn down by the pre-spawn cleanup, not orphaned.
  Plus a process-level smoke on Linux: `up --supervise`, `kill` a child,
  assert non-zero exit and a clean survivor teardown.

### T2 — `compass-stack service install` / `uninstall` + unit templates

Add the `service` verb pair: platform-detect (`runtime.GOOS`), render the
unit from an embedded template with the resolved absolute binary path,
state dir, and flags, install it, enable + start it; `uninstall` stops,
disables, and removes it. Both idempotent.

Interfaces:

- Consumes: the T1 `--supervise` mode; the CLI's existing flag/config
  resolution (`main.go` — the rendered unit pins the SAME resolved
  `--state-dir` and flags so the service stack and an interactive stack
  are distinct unless deliberately shared).
- Produces: `service install` / `service uninstall` verbs extending the
  `up|down|status|preflight` dispatch (`main.go:6`); two embedded unit
  templates —
  - systemd user unit → `~/.config/systemd/user/compass-stack.service`:
    `Type=exec`, `ExecStart=<abs> up --supervise …`, `Restart=on-failure`,
    `RestartSec=5`, `TimeoutStopSec=90`, `KillMode=mixed` (folded from the
    red-team: the default `KillMode=control-group` SIGTERMs EVERY process in
    the unit cgroup — server, runner, the dev-path postgres wrapper, and
    depending on rootless podman's cgroup placement conmon — in PARALLEL,
    bypassing the DL-183 reverse-order graceful teardown; `mixed` sends
    SIGTERM to the main pid only and reserves the group SIGKILL for the
    `TimeoutStopSec` deadline, composing correctly with the 90s grace),
    explicit start-limit posture (`StartLimitIntervalSec=`/`StartLimitBurst=`
    stated, not left to the 5-in-10s default — the intended circuit breaker
    is chosen, not an accident of defaults), `Environment=PATH=…`,
    `WantedBy=default.target`; NO `After=network.target` (it is a
    system-manager unit the per-user manager silently ignores, and the
    server binds loopback only, `main.go:30-31` — no network-up ordering
    needed); install runs `systemctl --user daemon-reload` + `enable --now`
    and checks/advises `loginctl enable-linger` (mandatory for boot-start:
    rootless podman requires a real `XDG_RUNTIME_DIR`; the CLI's own
    fallback `defaultRuntimeDir` is not a substitute for podman's,
    `main.go:284-292`).
  - launchd plist → `~/Library/LaunchAgents/com.rigelbuild.compass-stack.plist`:
    `ProgramArguments` = `<abs> up --supervise …`, `RunAtLoad=true`,
    `KeepAlive={SuccessfulExit=false}`, `ExitTimeOut=90`,
    `ThrottleInterval=` stated (launchd's restart floor, the macOS analogue
    of `RestartSec`), `AbandonProcessGroup=true` (folded from the red-team:
    launchd's default teardown SIGKILLs remaining process-group members at
    job stop, which would race the DL-183 graceful drain — the plist must
    let the children survive long enough for the ordered reverse teardown),
    `EnvironmentVariables` PATH; install runs
    `launchctl bootstrap gui/$UID` then `enable`.
- Operator stop-truth — the `down` verb vs the installed unit (folded from
  the red-team): running `compass-stack down` against a service-supervised
  stack kills the children out from under the supervise process, which
  observes child death, drains, exits non-zero — and the OS supervisor
  RESTARTS the stack the operator just stopped, so `down` becomes a
  no-op-with-extra-steps and the only real stop verbs become
  `systemctl --user stop` / `launchctl bootout`, contradicting DL-259's
  one-verb surface. So `service install` makes `down` unit-aware: when an
  installed unit is active, `down` stops it THROUGH the unit
  (`systemctl --user stop` / `launchctl bootout`) so the supervise process
  performs the teardown and exits ZERO (no restart), preserving
  `compass-stack` as the stop surface. This is a fold, not a Matt fork.
- Unit-content gate: every item of the T2 checklist (former OQ-4, below) —
  explicit `--state-dir`, absolute paths + PATH, `KillMode=mixed` /
  `AbandonProcessGroup=true` ordered-teardown knobs, pinned `RestartSec` /
  `ThrottleInterval` + start-limit posture, stop grace >= 90s, log routing
  (journald / launchd `StandardErrorPath`), the linger step, the
  status-truth doc caveat.
- Test cycle: unit tests on the template rendering (golden files: absolute
  paths, the >= 90s stop grace, the pinned state dir, `KillMode=mixed` /
  `AbandonProcessGroup=true`). Smoke on a real systemd host: install → unit
  active + `compass-stack status` Ready → kill a child → unit enters
  `failed`/restarts and the stack comes back → `systemctl --user stop`
  produces the ORDERED reverse drain (runner exits before server, not a
  parallel cgroup massacre — the `KillMode=mixed` assertion) with the pgid
  record gone and sockets dark → `compass-stack down` while the unit is
  active stops it through the unit and exits zero (no restart) → uninstall
  removes the unit. The launchd leg smokes the same cycle on a darwin host
  (install → running → crash-kill → KeepAlive restart → stop → uninstall);
  it lands with T3 and is gated by OQ-5's end-to-end caveat.

### T3 — darwin start-time identity reader

Give the DL-183 identity token a darwin implementation so `up` no longer
refuses on macOS. **This is TWO swaps in two packages, not one** (folded
from the design-critic red-team): the token is read at spawn by the core's
`readStartTime` seam (`pgidfile.go:331-338`) AND, independently, at
teardown by the down-side identity check's own deliberately-duplicated
reader `readGroupLeaderStartTime` (`adapters/groupsignal.go:93-98`, which
"duplicates the core's parser ... because the parenthesized-comm gotcha is
the same on both sides"). `GroupSignaller.Alive` compares the two for
`uint64` equality (`groupsignal.go:90`, `got == startTime`), so on a single
OS the spawn-side and down-side darwin readers MUST produce the identical
encoding — the "encodings never need to agree" property holds ACROSS OSes
(Linux clock-ticks vs darwin timeval never compare) but is FALSE across the
two readers on one OS. A darwin `up` whose token the darwin `down` cannot
match would silently skip every live process child at teardown. So T3 adds
a darwin reader to BOTH seams sharing ONE encoding: `sysctl KERN_PROC` via
`golang.org/x/sys/unix` (already a module dependency, `go/go.mod:96`)
reading `KinfoProc`'s start `timeval`, packed as `sec*1e6 + usec` into the
`uint64` the pipeline already carries (`pgidEntry.StartTime uint64`,
`pgidfile.go:80-88`; `Alive`'s `startTime uint64`; serialized decimal —
correcting the record's earlier "opaque equality string": it is a `uint64`,
which darwin must pack a timeval into). Split each reader into
`_linux.go`/`_darwin.go` build-tagged files behind the unchanged seams.

Interfaces:

- Consumes: the `readStartTime` var seam (`pgidfile.go:331-338`) AND the
  down-side `readGroupLeaderStartTime` (`groupsignal.go:93-108`); the pgid
  record grammar unchanged (proc entries carry whatever `uint64` the host's
  reader produced at spawn).
- Produces: darwin readers for BOTH seams (four build-tagged files:
  `readstarttime_{linux,darwin}.go` + the groupsignal `_{linux,darwin}.go`
  split), sharing one darwin timeval-packing encoding; removal of the
  "refuses on non-Linux unix" behavior (the seam comments update from
  test-seam to cross-OS seam); no CLI surface change.
- Test cycle: the existing seam-stubbed tests keep passing unchanged; the
  existing spawn-vs-down mirror-test pair (`groupsignal_test.go:77-84`,
  which exists precisely because the duplication is load-bearing) EXTENDS
  to the darwin readers so the two encodings cannot drift; a new
  darwin-tagged unit test reads the test process's own start time twice
  (stable, non-empty) and verifies a dead/mismatched pid fails the identity
  check. Runs on the DL-263 darwin CI leg. The full macOS stack-up smoke is
  OQ-5-gated (topology, not supervision).

### T4 — Docs, ledger, close-out

Ship the operator surface and record the ruling.

Interfaces:

- Consumes: T1-T3 landed; this record (frozen on merge).
- Produces: the self-host doc (DL-259's install surface) gains a "run as a
  service" section documenting `compass-stack service install` for both
  platforms, the linger prerequisite, the log locations, and the
  status-truth caveat (`compass-stack status`, never
  `systemctl --user status` / `launchctl print` alone); the DECISIONS.md
  DL-328 row (rewritten to this ruling — the coordinator lands it with the
  record); RIG-3239 close-out.
- Test cycle: markdownlint on the record + doc; the doc's command sequence
  is walked once verbatim on a Linux host as part of the T2 smoke.

## Tasks

- [ ] T1 — `up --supervise` blocking mode: supervise loop on the stack core
      (child-`Wait` fan-in, drain + non-zero on child death, signal-driven
      `Down`), harness unit tests + Linux process smoke.
- [ ] T2 — `service install`/`uninstall` verbs + embedded systemd/launchd
      unit templates, rendered against the T2 unit-content checklist;
      golden-file tests + systemd-host smoke (launchd smoke with T3).
- [ ] T3 — darwin `readStartTime` (`sysctl KERN_PROC` via x/sys) behind the
      existing var seam; darwin-tagged unit test on the DL-263 CI leg.
- [ ] T4 — self-host doc "run as a service" section (both platforms +
      status-truth caveat), DL-328 ledger row, close-out.

## Open Questions

### OQ-4 [non-load-bearing] — service-unit content checklist (the T2 gate)

The unit shapes are standard systemd/launchd practice but were NOT executed
this session; the T2 smoke cycle resolves them. Each rendered unit MUST
carry, and the T2 tests MUST assert (non-load-bearing: none changes the
ruling, all are content-completeness for the executor):

- **Pinned identity.** Explicit `--state-dir` in the rendered invocation, so
  the service stack and any interactive stack are distinct unless
  deliberately shared — `up` attaches to an already-live stack rather than
  failing (`stack.go:131-146`), so an unpinned unit could adopt (and later
  tear down) a stack a manual session started.
- **Environment + boot order.** systemd user units and launchd agents
  inherit no login-shell PATH, so the `ExecStart`/`ProgramArguments` binary
  path is absolute AND an explicit PATH is set (the stack's own `LookPath`
  children — podman, the DL-321 sidecars — must resolve). Linux adds
  `After=network.target` (the TLS door bind); podman needs no daemon so no
  socket dependency (`podman.go:23-24`). `loginctl enable-linger` is
  MANDATORY on Linux — rootless podman hard-requires a real
  `XDG_RUNTIME_DIR`, which exists only in a lingering or logged-in session
  (`defaultRuntimeDir` falls back to `<stateDir>/run` when unset,
  `main.go:284-292`, but podman itself still needs the real runtime dir).
  launchd covers boot via `RunAtLoad` with no linger analogue needed.
- **Stop grace.** Teardown can take the ~80s worst-case drain
  (`downdetached.go:15-24`), so `TimeoutStopSec` / `ExitTimeOut` >= 90s or
  the OS supervisor SIGKILLs mid-teardown.
- **Log + status routing.** Linux: journald via the unit
  (`journalctl --user -u compass-stack`); macOS: explicit
  `StandardOutPath`/`StandardErrorPath`. Both docs state the status source
  of truth is `compass-stack status` — the unit's state is supervise-pid
  liveness only.

### OQ-5 [load-bearing, Matt fork] — macOS service: ship against podman-machine now, or gate GA on RIG-3238?

The supervision mechanics for macOS are shippable in this record (T3 darwin
identity reader — a bounded seam swap; T2 launchd template), but a WORKING
macOS stack end-to-end still hangs on topology unknowns owned by the
sibling lanes: embedded-revival OQ-7 (the postgres DSN + agent sockets are
AF_UNIX bind-mounts, unvalidated across the podman-machine virtiofs/VM
boundary, `ui/compass-native-embedded-revival/design.md:909-931`) and
RIG-3238's OQ-12 (runner-on-darwin once podman-machine goes,
`platform/apple-container-macos-runner/design.md:713-733`). The fork: (a)
ship T2/T3's macOS support now, labeled experimental until the topology
validates, or (b) hold the macOS half of `service install` behind the
RIG-3238 lane's resolution and ship Linux-only first. **The cost option (a)
must carry (folded from the design-critic red-team):** an "experimental"
doc label does NOT stop a crashloop — on a mac where the socket topology
cannot work, `up --supervise` never reaches Ready, and launchd's
`KeepAlive={SuccessfulExit=false}` (which cannot tell a never-Ready
bring-up failure from a post-Ready crash) restarts it every ~10s forever,
burning CPU + podman-machine churn. So option (a) is only safe bundled with
a bounded-crashloop guard: the darwin `service install` runs an
install-time preflight (podman machine reachable + one probe cycle) and
refuses with a legible error until it passes, AND/OR the supervise loop
self-disables after N consecutive bring-up failures (a start-failure
backoff launchd's `SuccessfulExit` key cannot express). **Recommendation:
(a) with that preflight + self-limit** — build and land the darwin
mechanics now (they are small, testable on the DL-263 CI leg, and required
under EVERY macOS outcome, including apple-container), gate only the "macOS
supported" doc claim + GA smoke on the sibling lane's socket-topology
validation, and let Linux ship independently either way. This is dependency
ordering only — macOS-in-scope is ruled, not open.

### Resolved decisions

- **OQ-1 (Quadlet vs pgid) — RULED: keep pgid, decline Quadlet** (Matt,
  2026-09-04). The all-platforms requirement adds a third independent
  ground: Quadlet is Linux/systemd-only and cannot express the macOS half
  at all. See Approach.
- **OQ-2 (ship the wrapper?) — RULED: yes, upgraded** (Matt, 2026-09-04):
  not an optional Linux-only doc snippet but a first-class one-command
  `compass-stack service install` on all platforms, auto-start at reboot.
  Verbatim: "we need to ship with a one command way to stand up the stack
  as a constant-on service on all platforms".
- **OQ-3 (crash recovery) — RULED: build it** (Matt, 2026-09-04): the
  accept-the-gap posture is rejected ("a VPS doesn't fix a crash … opt 1
  reasoning makes no sense"); the blocking `up --supervise` under the OS
  supervisor's restart policy is the design (option (b) of the original
  fork), keeping one supervision model.
- Rootless under Quadlet: confirmed — rootless Quadlet runs as systemd USER
  units from the rootless unit search paths (podman stays daemonless); it is
  explicitly not achievable via `User=` in a system unit. Source:
  <https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html>.
  Moot under the keep ruling; recorded for the declined alternative.
- DL-259 squaring: systemd/launchd units are host-level configuration, not
  compose/Swarm packaging — no letter violation; per-service Quadlet units
  would have displaced the `compass-stack up` verb DL-259 names (an
  amendment), but the ruled `service install` keeps `compass-stack` as the
  verb surface and needs none.
- Docker-socket: declined at the stack layer (daemon model vs the
  rootless/no-daemon hard requirement, no per-container keep-id equivalent),
  mirroring the embedded-revival OQ-9 rejection — see Alternatives.
- macOS in scope: ruled by Matt ("how do we support this on macOS as
  well?") — only the dependency ordering (OQ-5) remains open, never
  whether macOS is supported.
