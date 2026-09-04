# Compass self-host stack supervision: Quadlet vs hand-rolled pgid

Status: Draft
Issue: RIG-3239

## Problem / Intent

Decide the supervision mechanism for the long-lived self-host stack services
(compass-server, compass-runner, the containerized postgres, the bundled OTel
collector): adopt Podman **Quadlet** (systemd-native declarative unit files) or
keep the hand-rolled **DL-183/DL-262 pgid** mechanism `compass-stack` ships
today, with the Docker-socket engine recorded as considered-and-declined at the
stack layer. Scope is bring-up order, teardown, restart policy, crash recovery,
and rootless posture of the stack services ONLY — the per-session runner
backend is frozen out of scope (RIG-3070: podman permanent for self-host,
microVM behind the seam; see
`ui/compass-native-embedded-revival/design.md:71-74`), as is the macOS embedded
runner backend (sibling RIG-3238 Apple-container record).

## Approach

**Recommended ruling (Matt fork — see Open Questions OQ-1): keep the
hand-rolled DL-183/DL-262 pgid supervision as the SINGLE built-in mechanism;
do not adopt per-service Quadlet units. Optionally ship a documented THIN
systemd user-unit wrapper around the existing `compass-stack up`/`down` for
self-hosters who want boot-start and logout survival — systemd wrapping the
supervisor, never replacing it.** Docker-socket is declined at the stack layer,
mirroring its per-session-runner rejection.

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
up`. This is the one genuine capability Quadlet/systemd would add.

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

### Why keep the hand-rolled mechanism (the recommendation)

1. **Two supervision models vs one — the crux (a maintenance-cost argument,
   not a slice-size one).** Quadlet needs systemd, so it covers only
   self-host-on-systemd-Linux. Two other tiers fall outside it and keep the
   DL-183 mechanism regardless: the embedded front door (DL-319 dual-mode app,
   `DECISIONS.md:298`: the app "spawns/supervises a LOCAL stack via rootless
   podman on the user's own machine (macOS via podman machine, Linux native)")
   cannot ride systemd on macOS at all, and dev/devenv + non-systemd Linux
   hosts also can't. Note the Quadlet-covered slice is NOT marginal — by
   DL-259's own wording (`DECISIONS.md:292`, "a KVM-capable **Linux**
   machine") the whole named self-host tier IS Linux — so the argument is not
   "Quadlet only helps a corner"; it is that adopting Quadlet means
   maintaining, testing, and keeping behaviorally-equivalent TWO supervision
   models (unit-file bring-up/teardown semantics AND the pgid record) for the
   same four services, permanently, because the DL-183 path cannot retire while
   embedded and non-systemd tiers exist. One supervision model beats two.
   **Caveat (a cost the KEEP ruling also owns):** the pgid mechanism is not
   literally "works everywhere" today — its start-time identity token reads
   `/proc/<pid>/stat`, Linux-only, and on a non-Linux unix "this reader fails
   and up refuses" (`pgidfile.go:333-338`), so on macOS the runner + server run
   as darwin HOST processes (embedded-revival OQ-7,
   `ui/compass-native-embedded-revival/design.md:911-912`) that the current
   record cannot identity-token. The mechanism is PORTABLE to darwin (setpgid /
   SIGTERM / `kill(-pgid)` all exist; a `kern.proc` start-time read is a bounded
   swap behind the existing `readStartTime` var seam), but that port is unbuilt
   work the DL-319 / RIG-3238 embedded-macOS lane owns. This darwin-port cost
   exists under EITHER ruling (Quadlet can't run on macOS at all, so it never
   removes it) — which is exactly why it does not flip the decision: it is a
   cost of shipping embedded macOS, not a cost of choosing pgid over Quadlet.
2. **DL-183/DL-262 are load-bearing, frozen, and tested.** `DECISIONS.md:281`
   (DL-183, Active) and `DECISIONS.md:295` (DL-262, Active) freeze the record
   format, the identity-token verify-before-signal discipline, and the v1/v2
   cross-version grammar guard; the invariants are non-negotiable in the frozen
   teardown record
   (`ui/compass-stack-cross-process-teardown/design.md:299-301`):

   > ```text
   > - **Only the exact persisted pgids are ever signaled.** Never a pattern kill,
   >   never a scan of the process table, never a pgid not read from this stack's
   >   own state-dir file.
   > ```

   Retiring a working, tested mechanism for a declarative-standard win that
   only lands on one platform slice is churn without a capability gain the
   wrapper (below) doesn't also deliver.
3. **The bring-up chain is imperative, not declarative.** The cold sequence
   (`stack.go:89-93`, quoted above) interleaves containers with host-binary
   children AND side-effect steps: TLS anchor generation (expiry-aware), runner
   token minting (idempotent 0600), agent-image presence, readiness polls.
   Under Quadlet, compass-server/compass-runner are host binaries — plain
   systemd user services, not `.container` units — so "Quadlet" is really
   "Quadlet for 2 containers + 2 plain user units + oneshot pre-units for the
   TLS/token/image steps + sdnotify readiness plumbing in the server". That is
   a full re-plumb of `spawnChain`, not a swap of supervisors.
4. **DL-259 compatibility cuts both ways.** DL-259 (`DECISIONS.md:292`) froze:

   > ```text
   > The self-host stack stays a host-level bring-up on a KVM-capable Linux
   > machine (`compass-stack up`; microVM D3 hard-fail consumed, no
   > compose/Swarm packaging)
   > ```

   Quadlet unit files are systemd-native host-level configuration, NOT
   compose/Swarm packaging — adopting them would not violate DL-259's letter.
   But DL-259's operative clause names **`compass-stack up`** as the bring-up
   surface; per-service Quadlet units would displace that entry point (the
   operator's verbs become `systemctl --user start/stop`), which is a DL-259
   amendment, not a compatible extension. The thin wrapper below keeps
   `compass-stack up` as the bring-up verb and is squarely inside DL-259.

### The middle path: a documented thin systemd wrapper (recommended add-on)

A single systemd USER unit that wraps the existing supervisor verbatim —
`Type=oneshot` + `RemainAfterExit=yes`, `ExecStart=compass-stack up`,
`ExecStop=compass-stack down` — shipped as documentation in the self-host doc
(DL-259's install surface), not as installed machinery. It delivers the real
operational wins systemd offers (start at boot via `loginctl enable-linger` +
`WantedBy=default.target`, survive logout, one `systemctl --user` status verb)
with ZERO code change and ZERO second supervision model: DL-183/DL-262 remain
the sole bring-up/teardown mechanism, systemd merely invokes it.
[INFERENCE] The unit shape is standard systemd; the exact unit text is Plan
task T1's deliverable and must be smoke-tested on a real systemd host before
the doc ships. What the wrapper does NOT deliver: per-child restart-on-crash
(systemd sees one oneshot, not four services) — carried as OQ-3. **And a
caveat the T1 doc MUST carry:** with `RemainAfterExit=yes` systemd reports the
unit `active` from the instant `up` returns until an explicit stop, so a child
that crashes afterward is invisible to systemd — the unit's `active` state
means only "up-once-succeeded", NOT "stack healthy". The status source of
truth stays `compass-stack status`, never `systemctl --user status`; the fuller
wrapper failure modes are OQ-4.

### Alternatives considered

- **Podman Quadlet per-service units (declined, pending Matt — OQ-1).** What
  it is: `.container` units for postgres/collector, plain systemd user units
  for compass-server/compass-runner, `After=`/`Requires=` ordering, `Restart=`
  crash recovery, sdnotify readiness. Why it loses: Linux/systemd-only, so the
  DL-183 mechanism survives anyway (two models); retires a tested frozen
  mechanism; requires re-plumbing the imperative cold sequence into oneshot
  pre-units; displaces the DL-259-named `compass-stack up` entry point. What
  it would win: per-service `Restart=` crash recovery, boot-start, journald
  logs, a declarative standard operators already know.
- **Hand-rolled DL-183/DL-262 pgid supervision (recommended keep).** What it
  is: the shipped mechanism quoted above. Why it wins: ONE PORTABLE model
  across the tiers — native today on self-host + embedded Linux, and portable
  to embedded macOS via a bounded `readStartTime` seam swap the RIG-3238 lane
  owns (NOT running there today: `pgidfile.go:333-338` `up` refuses on non-Linux
  unix); frozen, tested, load-bearing; teardown-safety invariants proven. Known
  gap: no crash recovery (OQ-3) and no boot-start (covered by the wrapper).
- **Blocking `compass-stack up --supervise` under a `Type=exec` unit
  (surfaced by the red-team — OQ-3).** What it is: a NEW blocking foreground
  mode (up-to-Ready, then watch children / poll Health, exit non-zero on a
  child death, teardown on signal) paired with a `Type=exec`/`Type=notify`
  systemd unit using `Restart=on-failure` + `RestartSec=`. Division of labor:
  the stack keeps its single supervision model (DL-183 spawn order,
  identity-token teardown, drain), systemd supplies ONLY whole-unit
  restart/backoff. What it wins: systemd-grade crash recovery with ONE model
  and no per-service Quadlet — and it fixes the oneshot wrapper's status-lie
  (a dead stack is `failed`, not fake-`active`) and stop-what-you-didn't-start
  edge (systemd owns the pid it spawned). Cost: a real code change (blocking
  mode + child-exit watching, where `up` abandons children by design today),
  so unlike T1 it is not free. This is the strongest competitor to the drafted
  accept-the-gap posture — routed to Matt as OQ-3's second option, not decided
  here.
- **Non-systemd process supervisors (s6 / runit / supervisord) — declined.**
  Dominated by the same two-models argument (none covers embedded macOS or
  non-systemd-agnostic dev) plus a new third-party runtime dependency the
  self-host tier does not otherwise carry.
- **`compass-stack up` as a thin facade over `systemctl` (declined).** Keep the
  DL-259 verb but have it generate + `systemctl --user` Quadlet units under the
  hood. Rejected: it still stands up the full two-model machinery (the pgid
  path survives for embedded/non-systemd) while adding a generation layer, and
  it inherits every imperative-cold-sequence re-plumb of full Quadlet — the
  facade hides the second model without removing it.
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

The recommended ruling is keep-hand-rolled, so the plan is deliberately
minimal: no supervision code changes. The only implementation work is the
documented systemd wrapper (T1) and the explicit close-out (T2). If Matt rules
for Quadlet adoption instead (OQ-1), this plan is void and a new impl plan is
drafted under that ruling — the tasks below are NOT a Quadlet migration plan.

### Global Constraints

- Platform floor for the wrapper: Linux with systemd user sessions + cgroup v2
  (Quadlet/systemd requirement); the wrapper is optional documentation, never
  a prerequisite — `compass-stack up` alone remains fully supported.
- Rootless invariant: everything runs as the invoking user; no root units, no
  daemon, no rootful fallback (`go/internal/runtime/podman.go:23-24`).
- DL-259: `compass-stack up` stays the bring-up verb; no compose/Swarm
  packaging; the self-host doc + flake + preflight remain the install surface.
- DL-183 teardown-safety invariants survive untouched: only exact persisted
  identities are ever signaled; verify-before-signal; bounded escalation;
  survivor-rewrite on partial failure
  (`ui/compass-stack-cross-process-teardown/design.md:299-301`).
- ID allocation + freeze order (ledger-collision guard): this record claims
  DL-328. Main's ledger tail is DL-324; DL-325/DL-326/DL-327 are claimed by
  unlanded PRs (#804/#859 on DL-325, #859 on DL-326, #868 on DL-327), so DL-328
  is the first free number as of this writing. The driver MUST re-grep main's
  then-current ledger tail immediately before landing and take the next free id
  if DL-328 is taken. The row cites its own decision self-containedly (no
  cross-cite to an unlanded number), so merge order cannot falsify it.

### T1 — Documented systemd user-unit wrapper in the self-host doc

Add a "run at boot under systemd" section to the self-host doc (DL-259's
install surface) containing a complete, copy-pasteable systemd USER unit that
wraps the existing CLI, plus the `loginctl enable-linger <user>` step and the
`systemctl --user enable --now` / `status` / `stop` verbs. Documentation only;
no shipped unit file, no code change.

Interfaces:

- Consumes: `compass-stack up` / `compass-stack down` exactly as shipped
  (`go/cmd/compass-stack/main.go:8-10` — up returns at Ready and does not
  block; down is the DL-183 cross-process teardown), which is what makes
  `Type=oneshot` + `RemainAfterExit=yes` the correct service type.
- Produces: a documented unit of the shape
  `[Service] Type=oneshot / RemainAfterExit=yes / ExecStart=<abs>/compass-stack up --state-dir <dir> <flags> / ExecStop=<abs>/compass-stack down --state-dir <dir> <flags> / TimeoutStopSec=90`
  with `[Unit] After=network.target`, `[Install] WantedBy=default.target`, and
  an explicit `Environment=PATH=` (or `ExecSearchPath=`), placed at
  `~/.config/systemd/user/compass-stack.service`. The unit content MUST carry
  every OQ-4 item (explicit `--state-dir`, absolute paths + PATH, `After=`,
  `TimeoutStopSec>=90`) and the doc MUST carry the linger-mandatory step, the
  `journalctl --user -u compass-stack` log note, and the status-truth caveat
  (`compass-stack status`, not `systemctl status`).
- Test cycle: smoke-test the documented unit on a real systemd host —
  `systemctl --user start` reaches Ready (verify `compass-stack status`),
  `systemctl --user stop` tears down (verify the `stack.pgids` file is gone
  and sockets are dark), a reboot with linger enabled brings the stack back,
  AND the negative case: kill a child post-start and confirm systemd still
  reports `active` (proving the OQ-4 status-truth caveat the doc states). The
  doc ships only after this cycle passes.

### T2 — Close-out

Record the ruling's consequence honestly: no supervision code changes; RIG-3239
closes as a decision record + doc task. The main agent owns the DECISIONS.md
ledger row referencing this record.

Interfaces:

- Consumes: this record (frozen on merge) and Matt's OQ-1 ruling.
- Produces: the RIG-3239 close-out state; no code artifacts.
- Test cycle: none (no code); markdownlint on the record is the gate.

## Tasks

- [ ] T1 — self-host doc gains the systemd user-unit wrapper section
      (copy-pasteable unit + linger + verbs), smoke-tested on a systemd host.
- [ ] T2 — close-out: ruling recorded, no code changes, ledger row deferred to
      the main agent.

## Open Questions

### OQ-1 [load-bearing, Matt fork] — adopt Quadlet, or keep the hand-rolled supervision?

The central fork. **Recommendation: keep DL-183/DL-262 as the single
supervision mechanism; do not adopt per-service Quadlet units; add the T1
documented systemd wrapper for boot-start.** Grounds: Quadlet is
Linux/systemd-only so the pgid path survives regardless (two models vs one);
DL-183/DL-262 are frozen, tested, and load-bearing; the cold sequence is
imperative (TLS anchor, token mint, readiness polls) and would need oneshot
pre-units + sdnotify re-plumbing; per-service Quadlet displaces the
DL-259-named `compass-stack up` entry point. Quadlet's genuine wins
(per-service `Restart=` crash recovery, journald, a standard operators know)
are real — if Matt weighs crash recovery heavily, the fork reopens (see OQ-3).
Matt rules.

### OQ-2 [load-bearing, Matt fork] — is the T1 wrapper wanted at all?

The wrapper is an add-on, not a dependency of the ruling. If Matt prefers the
self-host doc stay minimal ("run `compass-stack up` in a tmux/session and be
done"), T1 drops and RIG-3239 closes as a pure decision record.
**Recommendation: ship T1** — boot-start/logout-survival is the most common
real self-host ask and costs one doc section.

### OQ-3 [load-bearing, Matt fork] — crash recovery: accept the gap, build a blocking `--supervise`, or reopen Quadlet?

Neither today's mechanism nor the T1 oneshot wrapper restarts a crashed child:
`Health` is probe-on-demand (`stack.go:198-200`), and systemd sees the oneshot
wrapper as one unit that stays `active` after `up` returns, not four services.
Per-service `Restart=` is exactly what full Quadlet adoption would buy. Three
options, in ascending code cost:

- **(a) Accept the gap (T1 oneshot wrapper as drafted).** A crashed stack
  surfaces legibly via `compass-stack status` / the client's connection
  failure, and the always-on-VPS steady state (DL-319) makes silent long-lived
  crashes operator-visible. Zero code. **Recommendation for v1.**
- **(b) Build a blocking `compass-stack up --supervise` under a `Type=exec`
  (or `Type=notify`) unit with `Restart=on-failure`.** The supervisor blocks
  after Ready, watches its children, and exits non-zero on a child death;
  systemd restarts the whole unit. This adds systemd-grade WHOLE-STACK crash
  recovery with ONE supervision model (DL-183 spawn/teardown unchanged; systemd
  supplies only restart/backoff) and simultaneously fixes the oneshot wrapper's
  status-lie and stop-what-you-didn't-start edges (OQ-4). Whole-stack (not
  per-service) restart is arguably the RIGHT granularity anyway — the cold
  sequence's ordering means a restarted postgres needs the server's readiness
  re-verified. Cost: a real code change (a blocking mode + child-exit watching,
  where `up` abandons children by design today).
- **(c) Reopen Quadlet (OQ-1).** Per-service `Restart=`, at the cost of two
  supervision models — the OQ-1 tradeoff.

**Recommendation: (a) for v1, with (b) as the named follow-up if field reports
show crash recovery matters** — (b) dominates the originally-drafted
in-supervisor-loop shape and delivers recovery WITHOUT a second model, so it,
not Quadlet, is the escalation path. If Matt rates crash recovery as a LAUNCH
requirement, choose (b) now (or, if per-service granularity is required, (c)
reopens OQ-1). Matt rules.

### OQ-4 [non-load-bearing] — wrapper failure modes + unit content, unverified until smoke-tested

The T1 oneshot unit shape wrapping a returns-at-Ready CLI is standard systemd
practice but was NOT executed this session. The T1 doc MUST address these,
each grounded, and the T1 smoke cycle resolves them (non-load-bearing: none
changes the ruling, all are doc-completeness for the executor):

- **Status lies (the RemainAfterExit trap).** With `RemainAfterExit=yes` the
  unit reads `active` from the instant `up` returns until an explicit stop, so
  a child crashing later is invisible to systemd. The doc MUST state the status
  source of truth is `compass-stack status`, and that the unit's `active` means
  only "up-once-succeeded". (Option (b) in OQ-3 eliminates this: a `Type=exec`
  unit tracks a live main pid, so a dead stack is `failed`.)
- **Stop-what-you-didn't-start.** `up` attaches to an already-live stack rather
  than failing (`stack.go:131-146` `upLocked` probe→attach), so a systemd
  `ExecStart` succeeds against a stack a manual `up`/dev session started; a
  later `stop`/unit-failure/shutdown then runs `ExecStop=compass-stack down`,
  which tears down whatever the shared state-dir's pgid record names
  (`downdetached.go:62-68` — per-state-dir identity-safe, but not per-invoker).
  The unit MUST pin an explicit `--state-dir` so the systemd stack and any
  interactive stack are distinct unless deliberately shared.
- **Environment + boot order.** systemd user units inherit no login-shell PATH,
  so `ExecStart` needs an absolute `compass-stack` path AND the stack's own
  `LookPath` children resolve (podman; the DL-321 PATH-threaded sidecars) — set
  an explicit `Environment=PATH=` or `ExecSearchPath=`. Add `After=network.target`
  (the TLS door bind); podman needs no daemon so no socket dependency
  (`podman.go:23-24`). `loginctl enable-linger` is MANDATORY, not optional —
  rootless podman hard-requires `XDG_RUNTIME_DIR`, which exists only in a
  lingering or logged-in user session (`defaultRuntimeDir` falls back to
  `<stateDir>/run` when it is unset, `main.go:284-290`, but podman itself
  still needs the real runtime dir).
- **`TimeoutStopSec`.** `ExecStop=compass-stack down` can take up to the 80s
  worst-case drain (`downdetached.go:15-24`: "sum to 65s; on the escalation
  path … 15 + (30+5) + (10+5) + (10+5) = 80s"), so set `TimeoutStopSec=` >= 90s
  or systemd SIGKILLs mid-teardown.

### Resolved within this record

- Rootless under Quadlet: confirmed — rootless Quadlet runs as systemd USER
  units from the rootless unit search paths (podman stays daemonless); it is
  explicitly not achievable via `User=` in a system unit. Source:
  <https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html>.
  Moot under the recommended keep ruling, load-bearing only if OQ-1 flips.
- DL-259 squaring: Quadlet units are host-level systemd config, not
  compose/Swarm packaging — no letter violation; but per-service units would
  displace the `compass-stack up` verb DL-259 names, so full adoption would
  need a DL-259 amendment. The T1 wrapper keeps the verb and needs none.
- Docker-socket: declined at the stack layer (daemon model vs the
  rootless/no-daemon hard requirement, no per-container keep-id equivalent),
  mirroring the embedded-revival OQ-9 rejection — see Alternatives.
