# microVM Runner V7 — teardown, crash recovery, observability

Status: PROPOSED — details the V7 milestone under the frozen parent
[microvm-runner.md](./microvm-runner.md) (its Plan § V7,
microvm-runner.md:583-598; Approach (f) "Teardown and mid-session death",
microvm-runner.md:238-261; Approach (g) "Observability + kill switch",
microvm-runner.md:263-277). Authoritative issue scope: RIG-2498. V7 is
designed against V6's supervision rework (PR #912, in review): the one-reaper-
per-child core V7's death detection rides is #912's shape, cited below from
its workspace; V7 must land after V6 merges.

Ledger impact: none. V7 details failure handling and metrics the parent's
§(f)/(g) already froze — "the backend supervises its per-session process set
(VMM, virtiofsd, net backend, guest …)" (microvm-runner.md:240-241), "Healthy
VMs found at restart are **killed and rebooted on next request, not adopted**"
(microvm-runner.md:258-259) — plus a metric-naming translation (§(d)) that is
an implementation-convention fact, not a new cross-cutting decision. There is
no `DECISIONS.md` under `docs/designs/infra/` (V5's precedent), and
`docs/designs/DECISIONS.md` is untouched.

## Problem / Intent

The microVM backend can boot, exec, and tear down a session it owns — but only
along paths where the Runner stays alive and the VM's processes die when told
to. Three gaps remain, and they are the last V-milestone before the V8
acceptance suite:

1. **Nothing survives a Runner crash.** Each session's VMM/virtiofsd/passt are
   direct children with a best-effort `PR_SET_PDEATHSIG` guard — "The real
   teardown guarantee is Shutdown, not Pdeathsig"
   (`go/internal/runtime/microvm/launch.go:453-458`) — and the per-session
   runtime dir records only passt's pid: `Launch` passes
   `"--pid", vm.pidfile` for passt alone (`launch.go:178-196`), while the VMM
   and virtiofsd leave no on-disk identity. The parent requires startup
   orphan-reaping "by their per-session runtime dir (pidfiles + process-
   liveness check)" (microvm-runner.md:254-256); with no VMM/virtiofsd
   pidfiles there is nothing to reap them by. §(a) adds the pidfiles, §(b)
   the reaper.

2. **Mid-session death is undetected.** V6's supervision core observes every
   child's exit through its sole reaper (`c.exited` closed after the single
   `c.cmd.Wait()`, `wt-v6 launch.go:486-495`), but nothing *acts* on a death
   after boot: a VMM that dies mid-session leaves in-flight `Exec`s failing
   with an undifferentiated transport error, live peer daemons, and a session
   entry that looks startable. The parent's matrix (microvm-runner.md:244-253)
   demands a distinguishable error, peer teardown, and an idempotent `Remove`;
   virtiofsd death is "fatal to the session (no remount-and-hope)". §(c).

3. **The backend is invisible.** The whole tree carries exactly one OTel
   metric instrument (`compass.delivery.dispatched`,
   `go/internal/delivery/consumer.go:269-272`, verified by a tree-wide grep
   for instrument constructors this session); the Runner and both backends
   emit none. The parent's §(g) metric set — boot latency, per-VM RSS, vsock
   RPC latency, virtiofsd restarts, quota utilization, canary result, plus a
   `backend` label on session metrics — does not exist. Its
   `compass_microvm_*` names are illustrative Prometheus-style; the codebase
   convention is dotted OTel names via the global meter. §(d) translates and
   specifies them concretely.

**Non-goals.** The transitional kill switch already exists and is not
redesigned: `SelectBackend` defaults to podman — "an unset backend never
silently switches an operator onto the unfinished path"
(`go/internal/runtime/microvm.go:110-116`) — which IS the parent's
`runtime.backend: podman` safety valve (microvm-runner.md:269-276). The
acceptance suite and the boot/RSS benchmark are V8
(microvm-runner.md:600-621). The Q-budget threshold VALUES stay deferred to
V8 measurement — "Set from V2a/V8 measurements on real hardware, not invented
here. The mechanism (canary + …)" (microvm-runner.md:839-841); V7 ships the
mechanism that produces the numbers and invents no threshold.

## Approach

Each subsection resolves one concern the parent's V7 plan leaves to detailing.
Every fork is also listed in `## Open Questions` for the pre-freeze batch, and
the body designs against the recommended option. Line numbers into the V6
supervision core cite PR #912's branch (the shape V7 lands on); everything
else cites main.

### (a) Per-session pidfiles for all three children, with PID-reuse defense

Today only passt records a pid on disk, and it records it itself: `Launch`
passes `"--pid", vm.pidfile` with `vm.pidfile =
filepath.Join(dir, "passt.pid")`
(`go/internal/runtime/microvm/launch.go:178-196`), and `Shutdown` removes it
(`launch.go:385-389`). The VMM and virtiofsd leave no on-disk identity, so the
parent's "reaps orphaned VMM/virtiofsd/net-backend processes by their
per-session runtime dir (pidfiles + process-liveness check)"
(microvm-runner.md:254-256) has nothing to work with. §(a) makes the runtime
dir the durable record of the session's process set.

**Layout.** Three host-written pidfiles in the existing per-session runtime
dir (`<RunRoot>/microvm/<id>/`, created 0700 by `Create`,
`go/internal/runtime/microvm_lifecycle.go:225-228`):

| File | Process | Writer |
| --- | --- | --- |
| `vmm.pid` | cloud-hypervisor | host (`launch`) |
| `virtiofsd.pid` | virtiofsd | host (`launch`) |
| `passt.pid` | passt | host (`launch`) |

passt's `--pid` flag is retired: with `-f` in the argv "this *exec.Cmd IS the
passt process" (`launch.go:182-191`), so the host knows passt's pid exactly as
it knows the other two (`cmd.Process.Pid` after `startChild`), and a
host-written file can carry the reuse defense below where passt's own
bare-pid file cannot. One writer, one format, three files. This diverges from
the parent's Interfaces sketch
(`{vmm.pid,virtiofsd.pid,netbackend.pid,vsock.port}`,
microvm-runner.md:589-590) in two flagged ways (OQ-1): the file keeps the
process's real name (`passt.pid`, matching the existing on-disk name) rather
than the role name `netbackend.pid`, and there is no `vsock.port` file — the
control-plane port is a fixed per-host constant ("Per-session uniqueness is
carried entirely by the AF_UNIX socket paths under the session runtime dir,
never the port", `microvm_lifecycle.go:48-52`), so a per-session port record
would be a copy of a constant.

**Format: pid + start time + boot id, because PID reuse is real — across
reboots too.** Each pidfile holds one line, `<pid> <starttime> <bootid>`:
`<starttime>` is field 22 of `/proc/<pid>/stat` — the kernel's process start
time in clock ticks since boot, immutable for the process's life, so
(pid, starttime) is unique across reuse within a boot — and `<bootid>` is
the host's boot UUID, read once per Runner process from
`/proc/sys/kernel/random/boot_id`. A liveness check compares the boot id
first: a mismatch means the file predates a reboot and nothing it names can
still exist — verdict "gone" before any proc read or signal. Within the same
boot it re-reads `/proc/<pid>/stat` and compares starttime: mismatch or
ENOENT ⇒ the recorded process is gone and the pid, if live, belongs to an
unrelated process that must NOT be killed. A bare-pid file can make neither
distinction, which is exactly the kill-an-innocent hazard §(b)'s reaper has
to defend against. The boot id is load-bearing, not belt-and-braces:
starttime is ticks SINCE BOOT, so after a reboot a long-uptime host
re-issues low pids and a stale pidfile's (pid, starttime) can legitimately
MATCH an unrelated process on the new boot — and a §(b) match kills. One
extra token per line buys a structural short-circuit where the alternative
is a plausible wrong kill.

**Write lifecycle.** `launch` writes each pidfile immediately after that
child's `startChild` succeeds (the pid exists only then), before the next
child is started — so at every instant the set of live children is a subset
of {written pidfiles} ∪ {the child being started this instant}. Each write
is atomic: the line goes to a temp file in the same runtime dir, then
`os.Rename` into place — a same-directory rename is atomic, so a reader
sees the complete record or no file, never a torn prefix. A plain
`os.WriteFile` is not atomic, and the torn-write window is EXACTLY the
Runner-crash window `ReapOrphans` exists for: a half-written pidfile would
demote a recorded live child to §(b)'s no-pidfile arm — never killed, its
dir age-gate-removed, the orphan leaked with its only record destroyed.
`PR_SET_PDEATHSIG` does not rescue that case; it is explicitly best-effort —
"The real teardown guarantee is Shutdown, not Pdeathsig"
(`launch.go:453-458`). A pidfile write failure fails the boot (the existing
error path: `launch`'s deferred `vm.Shutdown` tears down what started,
`launch.go:146-152`): orphan reapability is load-bearing, so a session that
cannot be recorded must not run. `Shutdown` removes all three alongside the
sockets (extending the existing pidfile removal, `launch.go:379-389`), and
`Remove`'s `os.RemoveAll(session.runtimeDir)`
(`microvm_lifecycle.go:674-676`) is the backstop — a pidfile structurally
cannot outlive its runtime dir.

**Invariant preservation.** The pidfile write is plain file I/O
(temp-write-then-rename) after `startChild` returns; it does not touch the
child's reaper, `exited` channel,
or `Wait` discipline (one reaper per child installed by `startChild`, "exactly
one goroutine per child owning exactly one cmd.Wait", PR #912
`launch.go:460-465`; the single `c.cmd.Wait()` at `launch.go:493` stays the
package's only Wait, verified by grep this session).

### (b) `ReapOrphans(ctx) error`: kill-and-remove at startup, never adopt

The parent is explicit: "on startup the backend reaps orphaned
VMM/virtiofsd/net-backend processes by their per-session runtime dir …
Healthy VMs found at restart are **killed and rebooted on next request, not
adopted**: the supervisor handshake state is in-process and not
reconstructable across a Runner restart" (microvm-runner.md:254-261). V7
honors that verbatim — the reaper has no adoption path, no health probe, no
attempt to re-dial a found VM's vsock socket.

```go
// ReapOrphans scans <RunRoot>/microvm/*/ for runtime dirs left by a previous
// Runner process, kills every recorded process that is verifiably the one
// the pidfile named (boot id + pid + starttime match), and removes the dir.
// Healthy VMs are killed, never adopted (microvm-runner.md:258-261).
// Idempotent and safe to re-run: a partial reap leaves the dir for the next
// attempt.
func (m *MicroVMRuntime) ReapOrphans(ctx context.Context) error
```

**Per-directory procedure**, for each `<RunRoot>/microvm/<id>/`:

1. **Skip live sessions.** Under `m.mu`, a dir whose leaf matches a key in
   `m.sessions` belongs to a session this process owns and is skipped. At the
   startup call site the table is empty (`NewMicroVMRuntime` builds it empty,
   `go/internal/runtime/microvm.go:96-103`), so this guard is inert there —
   it exists so a later or concurrent invocation can never race a Runner
   that is simultaneously creating sessions. `Create` inserts the session
   into the table under the same `m.mu` it checks duplicates under
   (`microvm_lifecycle.go:258-274`), but it creates the runtime dir BEFORE
   taking the lock (`microvm_lifecycle.go:225-228`), so a concurrent scan
   could see a fresh dir with no table entry and no pidfiles. The
   no-pidfile arm below therefore does NOT remove such a dir (see 3).
   This guard is INTRA-process only: it cannot see a SECOND live Runner's
   sessions over the same RunRoot, whose processes would pass the §(a)
   identity check and be killed with full confidence — the cross-process
   exclusion is OQ-8 (load-bearing), and the body assumes its
   recommendation: an exclusive `flock` on RunRoot taken before any reap,
   making a scan over a live Runner's dirs unreachable.
2. **Kill recorded processes, VMM first.** For each of the three pidfiles
   present: parse `<pid> <starttime> <bootid>`; a boot id differing from
   the current boot's short-circuits the entry as dead — nothing from a
   previous boot survives a reboot — with no proc read and no signal (the
   §(a) cross-reboot defense). Same boot: read `/proc/<pid>/stat`; on
   starttime match, SIGTERM, poll for disappearance (starttime re-check)
   up to the package's `reapGrace` (5s, `launch.go:45-47`), then SIGKILL
   and poll again; `ctx` bounds the whole SIGTERM → grace → SIGKILL
   escalation (checked between polls), and a cancellation leaves the dir
   in place behind a named per-dir error. Signal errors are
   differentiated, not lumped: ESRCH means the process died between the
   probe and the signal — benign, counted as confirmed-gone; EPERM means
   the pid now belongs to a process this Runner may not signal, which is
   necessarily NOT the recorded child (the rootless Runner's children run
   as its own uid and are always signalable) — a named per-dir error, the
   dir kept, and NEVER a blind SIGKILL retry. The reaper is NOT the parent
   of these processes — a restarted Runner cannot `Wait` them — so "gone"
   is observed by the proc probe, and the one-reaper/single-`Wait`
   invariant is untouched (these are not `child`-managed processes; no
   `*exec.Cmd` exists for them). A starttime mismatch or ENOENT means the
   process already died (or the pid was reused): no kill, the stale file
   is simply part of the dir removal. VMM before daemons mirrors
   `Shutdown`'s order ("The VMM is killed first (a VM gets no graceful
   drain)", PR #912 `launch.go:557-569`).
3. **Remove the dir only when everything recorded is dead.** All recorded
   processes confirmed gone ⇒ `os.RemoveAll` the dir (sockets, logs,
   pidfiles — everything; the session volume is untouched, it lives outside
   RunRoot on the durable volume, microvm-runner.md:251-253). A process that
   would not die within the bounded escalation leaves the dir in place and
   contributes a named error to the joined return — the next startup
   retries; a partial reap is re-runnable by construction because every step
   is idempotent. A dir with NO pidfiles is removed only when it is older
   than a small grace (mtime > `orphanDirGrace = 1m`); a younger one may be
   a concurrent `Create`'s pre-insert window (see 1) and is left alone.
   Unparseable pidfile content is treated as no-pidfile for that entry
   (logged at WARN with the content): nothing identifiable to kill. With
   §(a)'s atomic temp-write-then-rename a torn write cannot produce this
   state — it is reachable only through external tampering — and that
   atomicity is what makes the arm's removal defensible at all: the
   remove-vs-quarantine ruling for such dirs is OQ-7 (load-bearing, ruled
   together with the atomic write), and the body assumes its
   recommendation, remove after the grace.

**Call site.** `main.go` runs it once at startup, after the backend-gated
preflight, via a fourth unexported single-method probe interface in
`package main` — the V3/V4-ratified discipline the existing three probes
follow (`microVMPreflighter`/`podmanPreflighter`/`canaryBooter`,
`go/cmd/compass-runner/main.go:185-203`):

```go
type orphanReaper interface{ ReapOrphans(ctx context.Context) error }
```

A reap failure is a startup WARNING, not an abort (OQ-5): the un-reaped
processes hold stale resources but cannot corrupt new sessions — every new
session gets a fresh random id and dir (`mintSessionID`,
`microvm_lifecycle.go:332-341`), so nothing collides; refusing startup would
turn one wedged orphan into a fleet outage. The error is logged with the
per-dir detail and the reap re-runs at next startup.

### (c) Mid-session death: detect via the reaper channels, fail distinguishably, tear down peers

The parent's matrix (microvm-runner.md:244-253): VMM death ⇒ mark the handle
dead, fail in-flight `Exec`s with a distinguishable error, tear down the peer
daemons, release vsock/mount state, keep `Remove` idempotent; virtiofsd death
⇒ "treated as fatal to the session (no remount-and-hope): kill the VMM, same
teardown path".

**Detection: a per-session monitor over the existing reaper channels.** V6's
supervision core already publishes every child's exit: "exited is closed by
this child's SOLE reaper … Running and PSS all observe the exit THROUGH this
channel instead of calling Wait themselves" (PR #912 `launch.go:83-94`).
Nothing new needs to probe processes; the microvm package grows one method:

```go
// DeathCause names which child died first, for the session-level monitor.
type DeathCause string

const (
    DeathVMM       DeathCause = "vmm"
    DeathVirtiofsd DeathCause = "virtiofsd"
)

// DeathWatch returns a channel that receives exactly one DeathCause when the
// VMM or virtiofsd exits, then closes. SINGLE-CONSUMER: the one send is
// drained by exactly one receiver, the session monitor below; a second
// receiver would observe only the close and read the zero value
// DeathCause(""). The method cannot be unexported instead — the monitor
// lives in package runtime and reaches it through the guestVM seam
// (microvm_lifecycle.go:99-113) — so the contract is this comment plus the
// hermetic test, not visibility. It observes the children through their
// sole reapers' exited channels — it never calls Wait — so the one-reaper-
// per-child invariant holds. passt death is deliberately not fatal on its
// own: a dead net backend surfaces as the guest losing egress, while the
// control plane (vsock, served by the VMM) stays up; §(c) treats only
// VMM/virtiofsd as session-fatal, matching the parent's matrix.
func (vm *VM) DeathWatch() <-chan DeathCause
```

Internally one goroutine per VM `select`s on `vm.vmm.exited` and
`vm.virtiofsd.exited` (nil-guarded like every other consumer of those
channels). It always terminates: any teardown path kills the VMM, closing
`vmm.exited`.

**The session monitor.** `Start`, after ownership transfers to the session
table (`booted = false`, `microvm_lifecycle.go:401-415`), spawns one monitor
goroutine per session: receive from `DeathWatch()`; if the session is being
deliberately torn down (below), exit silently; otherwise record the death on
the session and run the one teardown path that already exists —
`vm.Shutdown` ("the VMM is killed first … then virtiofsd and passt are
reaped … and finally the AF_UNIX sockets and passt's pidfile are removed",
`launch.go:352-357`) — which is exactly "kill the VMM, same teardown path"
for the virtiofsd arm, kills the peers for the VMM arm, and removes the
socket/pidfile state. The vsock "port" needs no separate release: the port is
a fixed constant and per-session identity rides the AF_UNIX socket paths
Shutdown removes (`microvm_lifecycle.go:48-52`).

State on `microvmSession` (all under `m.mu`, the existing discipline —
"All fields are read/written under MicroVMRuntime.mu",
`microvm_lifecycle.go:141-144`):

```go
// deadCause is non-empty once the monitor observed a mid-session child death
// and tore the session down; read by Exec/ExecStreaming/Start to refuse with
// a *SessionDeadError.
deadCause microvm.DeathCause
// tearingDown is set (under mu) by Stop and Remove before they call
// vm.Shutdown, so the monitor can tell a deliberate teardown's exit from a
// crash and not misclassify it as a death. Start CLEARS it in the same
// locked critical section that stores the new VM
// (microvm_lifecycle.go:401-415): the flag describes the session's CURRENT
// VM life, and start-after-stop is a lifecycle the podman parity allows —
// Stop keeps the session entry (microvm_lifecycle.go:607-634) — so without
// the reset a restarted session's second life would inherit
// tearingDown=true and permanently suppress death detection.
tearingDown bool
```

The flag stays a session field reset by `Start` rather than moving onto
per-VM state: every `microvmSession` field already lives under `m.mu` (the
discipline quoted above, `microvm_lifecycle.go:141-144`), and a one-line
reset at the ownership-transfer point keeps that single synchronization
domain, where mutable per-VM state would open a second one behind the
`guestVM` seam for no added safety.

**The distinguishable error**, mirroring the `CommandError`/`TimeoutError`
shape (exported error structs with contextual fields in `package runtime`,
`go/internal/runtime/podman.go:318-341`; `Exec` already maps the microvm
package's timeout onto `*runtime.TimeoutError` at this exact seam,
`microvm_lifecycle.go:486-494`):

```go
// SessionDeadError is an operation refused (or an in-flight exec failed)
// because the session's VM died mid-session — the VMM exited or virtiofsd
// died (fatal, no remount-and-hope; microvm-runner.md:249-253).
type SessionDeadError struct {
    ID    ContainerID
    Cause string // "vmm" | "virtiofsd"
}

func (e *SessionDeadError) Error() string
```

Two paths produce it: (i) `Exec`/`ExecStreaming`/`Stop` entered after the
death check `deadCause` under the lock (extending `startedExec`,
`microvm_lifecycle.go:742-756`) and refuse immediately; (ii) an exec already
in flight when the VM dies fails with a transport error from the broken
vsock stream — `Exec`'s error arm (the non-timeout branch,
`microvm_lifecycle.go:486-495`) re-checks `deadCause` under the lock and,
when the monitor has not yet taken it (the death may be microseconds old),
probes the VMM directly via the seam's `VMMExited()` — a nil-safe export of
the VMM child's `hasExited()`, a non-blocking read of the sole reaper's
channel, "NOT a signal-0 probe" (PR #912 `launch.go:96-107`) — and wraps the
transport error in `*SessionDeadError` on either signal, so the caller sees
the cause, not connection noise (`deadCause` when recorded; `"vmm"` from the
direct probe, attributable because the VMM serves the vsock transport). The
residual window — virtiofsd freshly dead, the VMM not yet killed by the
monitor, the in-flight exec failing anyway — stays best-effort by design:
that one caller sees the raw transport error, and every subsequent call
refuses with the fully-attributed error via (i). `errors.As` reaches
`*SessionDeadError` through the wrap chain either way.

**Idempotent Remove, unchanged.** `Remove` already tolerates a dead VM:
`vm.Shutdown` is `sync.Once`-guarded ("safe to call twice",
`microvm_lifecycle.go:652-656`), the monitor's teardown ran it first, and
`Remove`'s own call returns the recorded `shutdownErr` and proceeds to
`os.RemoveAll` + table delete. A dead session's entry stays in the table
(with `deadCause` set) until `Remove` — deliberately, so the Runner's Remove
path and `Exists` behave identically to a live session's, and the death is
reportable rather than silently vanished.

**Deviation surfaced: the guest is not in V7's supervised set.** The
parent's §(f) preamble supervises the process set including "the guest via
the supervisor channel's liveness" (microvm-runner.md:240-241);
`DeathWatch` observes host children only, so a guest that kernel-panics and
hangs under a live VMM is a zombie session nothing detects except per-exec
timeouts on sessions actively in use. That is a deliberate V7 re-scope
carried as OQ-10 (load-bearing), not a silent drop; the body assumes its
recommendation — per-exec timeout coverage suffices for V7, active guest
health-probing lands with V8's acceptance evidence.

### (d) Observability: concrete dotted OTel metrics, warn-and-disable, no per-session labels

**The convention is OTel, not Prometheus.** The parent's
`compass_microvm_boot_seconds`-style names (microvm-runner.md:592-594) are
illustrative. The tree's one existing instrument fixes the real pattern
(`go/internal/delivery/consumer.go`): a package-level scope const
("`const instrumentationScope =
"github.com/RigelBuild/compass/go/internal/delivery"`", `consumer.go:165`),
creation once at construction from the global meter
(`otel.Meter(instrumentationScope).Int64Counter("compass.delivery.dispatched",
metric.WithDescription(…))`, `consumer.go:269-272`), and the failure posture:
"On error, leave it nil and log — a metric miss must never fail consumer
construction or block a delivery" (`consumer.go:266-276`). The cardinality
rule is equally explicit: "NEVER labelled per-session/channel/message — that
is a cardinality hazard" (`consumer.go:249-254`; enforced by test,
`trace_test.go:204-208`). V7 adopts all three verbatim: a `microvmMetrics`
struct built in `NewMicroVMRuntime` from the global meter under
`const microvmInstrumentationScope =
"github.com/RigelBuild/compass/go/internal/runtime"`, nil instruments on
creation error (warn + disable, construction never fails), every record
nil-guarded and off the hot path's critical section.

**The metric set** (translating parent §(g), microvm-runner.md:265-268):

| OTel name | Instrument | Unit | Attributes | Meaning |
| --- | --- | --- | --- | --- |
| `compass.microvm.boot.duration` | Float64Histogram | `s` | `outcome` = `ok`\|`error` | Wall time of `Start` (Launch → Health-OK → nonce → Provision), the "VMM start → supervisor handshake" latency; same basis as `CanaryReport.BootLatency` (`microvm_preflight.go:351-355`) |
| `compass.microvm.teardowns` | Int64Counter | `{teardown}` | `cause` = `remove`\|`stop`\|`vmm_death`\|`virtiofsd_death`\|`orphan_reap` | Session teardowns by cause; the `virtiofsd_death` series IS the parent's "virtiofsd restarts" number (OQ-4: under §(f)'s fatal-no-restart posture a virtiofsd death is a session teardown, never a restart) |
| `compass.microvm.vsock.rpc.duration` | Float64Histogram | `s` | `rpc` = `exec`\|`health`\|`provision`\|`signal`, `outcome` = `ok`\|`error` | Host-side wall time of guest control-plane RPCs, recorded in `Exec`/`awaitHealthy`/`Start`'s Provision/`stopGuest` (`microvm_lifecycle.go:476-502,426-458,393-399,641-650`) |
| `compass.microvm.guest.memory.pss` | Int64ObservableGauge | `By` | `process` = `vmm`\|`virtiofsd`\|`passt` | Sum over live sessions of per-process PSS via the existing `VM.PSS()` ("PSS, NOT summed VmHWM … PSS divides shared pages among their mappers", `launch.go:458-463`), summed per process kind so no per-session label exists; kB→bytes at record |
| `compass.microvm.quota.used_ratio` | Float64ObservableGauge | `1` | none | Observed byte utilization of the session-volume quota, recorded ONLY when `QuotaReading.Active()` — V6's single-meaning discipline ("the caller must gate on Active() … V7 inherits a single-meaning number", PR #912 `microvm_quota.go:134-147`); silent (no point) when inactive |
| `compass.microvm.canary.runs` | Int64Counter | `{run}` | `outcome` = `ok`\|`error` | Boot-canary executions by outcome — the parent's `compass_microvm_canary_ok` as a countable series |
| `compass.microvm.orphans.reaped` | Int64Counter | `{process}` | `process` = `vmm`\|`virtiofsd`\|`passt` | Orphan processes killed by `ReapOrphans` |

The PSS gauge is fleet-summed per process kind while the parent's frozen
line reads "per-VM RSS" (microvm-runner.md:266) — whether that sentence
means the aggregate or a per-VM series is OQ-9 (load-bearing, a ruling on
frozen text). The body assumes OQ-9's recommendation: the aggregate gauge
stays, per-session PSS rides a low-frequency INFO log line (logs are exempt
from the cardinality rule, below), and V8's benchmark sources per-VM
numbers from the harness calling `PSS()` directly, never from a metric.

Every attribute value is drawn from a closed enumeration named in this table;
no session id, container name, or path ever becomes a label. The observable
gauges register one callback at construction that snapshots the live
session/VM list under `m.mu`, RELEASES the lock, and only then calls
`PSS()` on each handle: `PSS` reads `/proc/<pid>/smaps_rollup` for up to
three processes per session (PR #912 `launch.go:669-676`), a kernel-side
VMA walk costing milliseconds on a large-memory process, and holding `m.mu`
across N×3 of those walks would block `Create`/`Start`/`Exec` on every
collection — the lock is held for a slice copy only, so collection stays
off the session hot path in fact, not just in intent. PSS is already
best-effort (a failed read is "expected and leave[s] no entry rather than
failing", PR #912 `launch.go:678-683`), so a session torn down between
snapshot and read contributes nothing and harms nothing.

**The `backend` label on "every existing session metric" is vacuous today —
flagged, not silently satisfied (OQ-2).** A tree-wide search for OTel
instrument constructors this session found exactly one,
`compass.delivery.dispatched` in the server-side delivery consumer
(`consumer.go:269-272`) — a message-fan-out metric, not a session metric; the
Runner and both backends define none (the only other counters are two
server-side `expvar` webhook-drop counts, `go/internal/ingest/
board_webhook.go:33`, `notify_webhook.go:26`). There is no existing metric to
retrofit. V7 therefore delivers the obligation's intent by minting the first
session-lifecycle metric WITH the label from birth, in the backend-agnostic
Runner host (`agentHost.Start`, `go/internal/runner/host.go:306`):

| OTel name | Instrument | Unit | Attributes | Meaning |
| --- | --- | --- | --- | --- |
| `compass.runner.session.starts` | Int64Counter | `{session}` | `backend` = `podman`\|`microvm`, `outcome` = `ok`\|`error` | Session starts through the Runner host, by backend |

The label is threaded without cardinality risk because it has exactly two
values, resolved once: the host asks its engine which backend it is via an
unexported single-method probe (`interface{ BackendName() string }`, the same
discipline as `main.go`'s probes) at construction, never per-call.

**INFO transition logs.** Boot and teardown transitions log at INFO with
session id and timings (parent, microvm-runner.md:268): `Start` logs
`microvm session booted` (`session_id`, `boot_duration`); the monitor logs
`microvm session died` (`session_id`, `cause`, `uptime`); `Stop`/`Remove` log
`microvm session stopped`/`removed` (`session_id`, `teardown_duration`);
`ReapOrphans` logs one line per reaped dir (`session_id`, `reaped`
process list). Session id in LOGS is fine — the cardinality rule constrains
metric labels, not log fields (the delivery consumer logs ids while metering
without them, `dispatch.go:363-377`).

**Meter-setup ordering (OQ-3).** `main.go` today installs the meter provider
AFTER the preflight: `verifyBackendPreflight` (which runs `BootCanary`) is at
`main.go:110-112` while `setupOtel` runs at `main.go:167-172`, so a canary
metric recorded at preflight time would hit the default no-op global and
vanish. V7 moves `setupOtel` ahead of `verifyBackendPreflight` (env-only
config, no operator-input check displaced; its disabled path installs nothing
and costs nothing, `go/internal/otel/provider.go` noop path) and constructs
`MicroVMRuntime`'s instruments lazily on first record if the engine was built
before the provider — recommendation in OQ-3 is the reorder, which keeps
construction-time creation.

## Alternatives considered

- **Adopt healthy orphan VMs at restart** ((b)). Foreclosed by the parent:
  "the supervisor handshake state is in-process and not reconstructable
  across a Runner restart" (microvm-runner.md:258-261) — the exec-gate nonce
  binding, the `guestVM` seam handle, and the reaper channels all live in the
  dead process. Rejected without an OQ; the parent froze it.
- **Keep passt's `--pid` self-written pidfile and host-write only the other
  two** ((a)). Two formats in one dir, and passt's bare-pid file cannot carry
  the starttime the reuse defense needs — the reaper would have a
  kill-an-innocent hole for exactly one of three processes. Rejected.
- **`pidfd_open` instead of pid+starttime** ((b)). A pidfd pins identity only
  for the holding process's life; a restarted Runner cannot mint a pidfd that
  refers to the process *as recorded before the crash*. The (pid, starttime)
  pair is the durable equivalent of a pidfd across process death. Rejected.
- **A per-session cgroup or process group as the identity token** ((a)/(b)).
  Strictly stronger where available: `cgroup.procs` enumerates CURRENT
  members exactly, immune to pid reuse entirely, and would sweep
  grandchildren the three-pidfile model cannot see (a process a daemon
  forked); a `setpgid` group similarly enables `kill(-pgid)`. Rejected on
  the dependency and the need: a rootless Runner owns no writable cgroupfs
  subtree without systemd delegation (`systemd-run --user --scope` or a
  `Delegate=` unit), coupling session lifecycle to a host service manager
  the design otherwise never touches; a bare process group carries the same
  pid-reuse hazard as a pid (a pgid IS a pid) with no durable identity; and
  the processes to reap ARE the three directly-spawned leaves — none forks
  further, so grandchild coverage buys nothing today. Revisit if a
  supervised child ever gains children of its own.
- **A `vsock.port` file per the parent's sketch** ((a), OQ-1). The port is a
  per-host constant (`guestVsockPort`, `microvm_lifecycle.go:48-52`); a
  per-session copy of a constant is drift surface with no reader. Rejected.
- **Restart a dead virtiofsd under a live VM** ((c)). "no remount-and-hope"
  (microvm-runner.md:249-251): the guest's mount is stale and the handshake
  state unprovable; the parent froze fatal-to-the-session. Rejected.
- **Prometheus-style underscore metric names** ((d)). The parent's names are
  illustrative; the codebase convention is dotted OTel through the global
  meter (`consumer.go:269-272`). An executor following the parent literally
  would mint a second convention. Rejected.
- **Per-session metric attributes** ((d)). Session ids are unbounded;
  the delivery consumer's hard rule ("NEVER labelled per-session/channel/
  message", `consumer.go:252-253`) applies unchanged. Session identity goes
  to INFO logs, aggregates to metrics. Rejected.
- **Abort startup on a `ReapOrphans` failure** ((b), OQ-5). Fresh random
  session ids mean orphans cannot collide with new sessions
  (`mintSessionID`, `microvm_lifecycle.go:332-341`); an abort converts one
  wedged process into a box-wide refusal with no operator gain over a loud
  WARN + next-startup retry. Rejected.

## Global Constraints

Every task below inherits these.

- **Lands after V6.** The supervision core V7 rides (per-child `exited`
  reapers, `hasExited`, the single `cmd.Wait`) is PR #912's shape; W-slices
  rebase onto merged main, never onto the review branch.
- **Metrics are OTel, never Prometheus-style.** Dotted names under the
  package instrumentation scope via the global meter; creation failure logs a
  warning and disables the instrument (nil + skip), never fails construction
  or blocks the hot path (`consumer.go:266-276`). No per-session/name/path
  attribute anywhere; every attribute value comes from a closed enum.
- **One reaper per child, one `cmd.Wait` per package.** Every new death/exit
  observation goes through `c.exited`/`hasExited` (PR #912
  `launch.go:83-105,460-465`); no V7 code calls `Wait`, and `ReapOrphans`
  operates on non-child processes via proc-probe + signal only.
- **The podman path is byte-identical.** No podman argv, check, metric, or
  message changes; the one shared touchpoint (the `backend`-labelled session
  counter in the Runner host) reads the backend name through an unexported
  probe and changes no podman behavior.
- **Frozen `ContainerRuntime` interface untouched.** `ReapOrphans` and the
  backend-name probe are `*MicroVMRuntime` methods / unexported interface
  assertions — the V3/V4-ratified single-method-probe discipline
  (`main.go:185-203`).
- **Two-tier test split.** Everything booting or killing a real VM carries
  `//go:build microvm && unix`, calls `microvmtest.Require(t)` first, and
  lives in `*_microvm_test.go`; pidfile parsing/liveness verdicts, reaper
  directory logic (fake proc probes), monitor sequencing (seam-faked VM),
  error mapping, and metric emission (`sdkmetric.NewManualReader` installed
  as the global before construction, `trace_test.go:209-220`) are hermetic.
- **Lint gates.** golangci-lint 2.13.2 clean under BOTH tag sets (untagged
  and `--build-tags microvm`); nilaway under `GOTOOLCHAIN=go1.27.1`;
  `funlen` ≤ 120 per function.
- **No panics in library code.** Every failure is an error value; the new
  error type follows the `CommandError` shape (`podman.go:318-341`).
- **V8 owns proof and thresholds.** The acceptance suite, the benchmark, and
  the Q-budget threshold VALUES are V8 (microvm-runner.md:600-621,839-841);
  V7's tests prove mechanism, not budget.

## Plan

Dependency order: W1 (pidfiles) precedes W2 (the reaper reads them). W3's
hermetic tier is independent of W1/W2 and may run in parallel; its KVM tier
depends on W1 — both rewrite `Shutdown`'s cleanup surface (`VM.pidfile`
becomes W1's three-element set) and W3's KVM test asserts the three-pidfile
removal after a death teardown, so the assertion differs by whether W1
landed. W4 (backend metrics + logs) consumes W3's death causes; W5 (startup
wiring + Runner-host counter) consumes W2 and W4.

### W1 — host-written pidfiles with boot-id + starttime identity (hermetic + KVM)

The §(a) layout: three pidfiles, one writer, atomic writes, PID-reuse
defense within and across boots.

- **Interfaces:** produces, in `go/internal/runtime/microvm`
  (`//go:build unix` files):
  - `func writePidfile(path string, pid int) error` — reads
    `/proc/<pid>/stat` field 22 and the boot id (from
    `/proc/sys/kernel/random/boot_id`, read once per process and cached),
    writes `<pid> <starttime> <bootid>\n` (0600) via
    temp-file-then-`os.Rename` in the same dir — atomic, so a Runner crash
    mid-write can never leave a torn record for `ReapOrphans` to misread
    as no-pidfile (§(a));
  - `type pidRecord struct { PID int; StartTime uint64; BootID string }`,
    `func readPidfile(path string) (pidRecord, error)`,
    `func (r pidRecord) alive() (bool, error)` — boot-id short-circuit,
    then starttime-compared liveness (stale boot id ⇒ false, nil; ENOENT ⇒
    false, nil);
  - `launch` extended: after each successful `startChild`, write that
    child's pidfile into the runtime dir (`vmm.pid`/`virtiofsd.pid`/
    `passt.pid`); a write failure returns an error (the existing deferred
    `vm.Shutdown` cleans up, `launch.go:146-152`);
  - passt argv loses `--pid` (`launch.go:187-196`); `VM.pidfile` becomes the
    three-element cleanup set removed by `Shutdown` beside the sockets
    (`launch.go:379-389`).
- **Test cycle:**
  - *Hermetic:* pidfile round-trip (write → read → fields match proc and
    the live boot id); `alive()` false on ENOENT, on a starttime mismatch
    (fake a record with a perturbed starttime for the test's own pid), and
    on a perturbed boot id; the write leaves no temp file behind;
    unparseable content errors; a spawn-failure boot leaves no pidfile
    behind. These read `/proc/self/stat` and the boot id, so they are
    Linux-hermetic under the package's `//go:build unix` tag — the
    precedent is `readPSS`, which reads `/proc/<pid>/smaps_rollup` from
    the same unix-tagged `launch.go` (PR #912 `launch.go:1,695-697`).
  - *KVM (`launch_teardown` extension, `//go:build microvm && unix`):* after
    `Launch`, all three pidfiles exist and each names a live process whose
    starttime matches; after `Shutdown`, all three are gone.

### W2 — `ReapOrphans` + startup wiring (hermetic + KVM)

The §(b) reaper. Depends on W1's file format.

- **Interfaces:** produces
  - `func (m *MicroVMRuntime) ReapOrphans(ctx context.Context) error` in
    `go/internal/runtime` (`//go:build unix`): scan
    `<RunRoot>/microvm/*/`; skip table-live dirs (under `m.mu`); per §(b)
    kill VMM-first via SIGTERM → `reapGrace` poll → SIGKILL with boot-id
    short-circuit and starttime re-verification before every signal; `ctx`
    bounds the whole escalation loop (checked between polls; cancellation
    ⇒ named per-dir error, dir kept); signal errors differentiated —
    ESRCH benign (already gone, counts as confirmed-dead), EPERM a named
    per-dir error with no blind retry and the dir kept; `os.RemoveAll`
    fully-dead dirs; age-gate (`orphanDirGrace = time.Minute`) dirs with
    no readable pidfiles; join per-dir errors;
  - an injected probe seam
    `type reapProbes struct { readStat func(pid int) (uint64, error); signal func(pid int, sig syscall.Signal) error }`
    defaulted to the real proc/kill, overridden in hermetic tests;
  - `main.go`: `type orphanReaper interface{ ReapOrphans(ctx context.Context) error }`
    probed after `verifyBackendPreflight` passes; a non-nil error is a
    `slog.Warn` with the joined detail, never an abort (OQ-5);
  - consumes W1's `readPidfile`/`pidRecord.alive`.
- **Test cycle:**
  - *Hermetic (fake probes, temp RunRoot):* planted live-match pidfiles ⇒
    signalled in VMM-first order and dir removed; starttime-mismatch
    pidfile ⇒ NO signal issued, dir removed; stale-boot-id pidfile ⇒ no
    proc read, no signal, dir removed; fake signal returns ESRCH ⇒ treated
    as confirmed-gone, dir removed; fake signal returns EPERM ⇒ named
    error, dir kept, no SIGKILL follow-up; cancelled ctx mid-escalation ⇒
    named error, dir kept; a process that never dies ⇒ dir kept, named
    error returned, second run retries; young no-pidfile dir kept, aged
    one removed; table-live dir untouched; concurrent `Create` during a
    scan never loses its dir.
  - *KVM:* boot a real VM, drop the `*MicroVMRuntime` without teardown
    (simulated crash: construct a FRESH runtime over the same RunRoot),
    `ReapOrphans` ⇒ all three processes dead (proc-verified), dir gone.

### W3 — `DeathWatch` + session monitor + `SessionDeadError` (hermetic + KVM)

The §(c) mid-session death path. Hermetic tier independent of W1/W2; KVM
tier depends on W1 (`Shutdown`'s cleanup set and the three-pidfile
assertion; see the Plan preamble).

- **Interfaces:** produces
  - `type DeathCause string` (`DeathVMM`, `DeathVirtiofsd`) and
    `func (vm *VM) DeathWatch() <-chan DeathCause` in the microvm package —
    select over the children's `exited` channels, one send then close,
    nil-device tolerant, no `Wait` calls, single-consumer by documented
    contract (§(c): the session monitor is the one receiver);
  - `func (vm *VM) VMMExited() bool` — nil-safe delegation to the VMM
    child's `hasExited()` (PR #912 `launch.go:96-107`; a channel read, not
    a zombie-blind signal-0 probe), for `Exec`'s in-flight wrap (§(c));
  - `type SessionDeadError struct { ID ContainerID; Cause string }` with
    `Error()` in `go/internal/runtime` (untagged file, beside
    `CommandError`);
  - `microvmSession.deadCause` + `microvmSession.tearingDown` (under
    `m.mu`); `Stop`/`Remove` set `tearingDown` before calling
    `vm.Shutdown`; `Start` CLEARS it in the same locked critical section
    that stores the new VM (`microvm_lifecycle.go:401-415`), so a
    start-after-stop session's second life detects deaths again (§(c));
  - `Start` spawns the monitor goroutine after ownership transfer
    (`microvm_lifecycle.go:401-415`): on a death, record `deadCause`, run
    `vm.Shutdown(context.WithoutCancel(…))`, log + count (W4 hooks);
  - `startedExec`/`Exec`/`ExecStreaming`/`Stop` refuse a dead session with
    `*SessionDeadError`; `Exec`'s error arm re-checks `deadCause` and,
    when it is unset, probes `VMMExited()` before wrapping in-flight
    transport failures (`microvm_lifecycle.go:486-495`; §(c));
  - the `guestVM` seam gains `DeathWatch() <-chan microvm.DeathCause` and
    `VMMExited() bool` (`microvm_lifecycle.go:99-113` lists the seam's
    method-set rule).
- **Test cycle:**
  - *Hermetic (seam-faked VM):* fake fires a VMM death ⇒ monitor records
    cause, Shutdown called once, next Exec refuses with `errors.As`
    `*SessionDeadError{Cause:"vmm"}`; virtiofsd death ⇒ same with
    `"virtiofsd"`; deliberate Stop/Remove ⇒ monitor exits silently, no
    death recorded; Stop → Start → fake death ⇒ the second life's death IS
    recorded (the `tearingDown` reset proven); in-flight exec failing
    while `deadCause` is still unset but the fake's `VMMExited()` reports
    true ⇒ wrapped `*SessionDeadError{Cause:"vmm"}`; a second receive on a
    drained `DeathWatch` channel observes only the close (the
    single-consumer contract's failure mode pinned, never relied on);
    Remove after death ⇒ nil (idempotent), table entry gone.
  - *KVM:* boot a session, `SIGKILL` the VMM pid mid-exec ⇒ the in-flight
    `Exec` fails with `*SessionDeadError`, peers reaped (proc-verified),
    sockets/pidfiles removed, `Remove` returns nil; kill virtiofsd ⇒
    session torn down, the workspace share content intact on the host —
    the parent's V7 test-cycle rows verbatim (microvm-runner.md:595-598).

### W4 — backend metrics + INFO transition logs (hermetic)

The §(d) instrument set inside the microVM backend.

- **Interfaces:** produces
  - `const microvmInstrumentationScope = "github.com/RigelBuild/compass/go/internal/runtime"`;
  - `type microvmMetrics struct { bootDuration metric.Float64Histogram; teardowns metric.Int64Counter; vsockRPCDuration metric.Float64Histogram; canaryRuns metric.Int64Counter; orphansReaped metric.Int64Counter; guestPSS metric.Int64ObservableGauge; quotaUsedRatio metric.Float64ObservableGauge }`
    built in `NewMicroVMRuntime` from the global meter, each instrument nil
    plus one `slog.Warn` on creation error, every record nil-guarded;
  - record points: `Start` (boot duration + INFO `microvm session booted`),
    the W3 monitor (`teardowns{cause}` + INFO `microvm session died`),
    `Stop`/`Remove` (`teardowns` + INFO), `Exec`/`awaitHealthy`/Provision/
    `stopGuest` (`vsock.rpc.duration{rpc,outcome}`), `BootCanary`
    (`canary.runs{outcome}`), `ReapOrphans` (`orphans.reaped{process}`);
  - observable callbacks registered at construction: `guestPSS` snapshots
    the live VM handles under `m.mu`, RELEASES the lock, then sums
    `vm.PSS()` per process kind outside it — never holding `m.mu` across a
    `smaps_rollup` read (§(d); best-effort, errors dropped as `PSS` itself
    does, PR #912 `launch.go:678-683`); `quotaUsedRatio` records only when
    the cached `QuotaReading.Active()` (V6's gate, PR #912
    `microvm_quota.go:138-145`);
  - consumes W2/W3 hooks; no new public API.
- **Test cycle (hermetic):** `sdkmetric.NewManualReader` +
  `NewMeterProvider` installed as the global BEFORE `NewMicroVMRuntime`
  (the `trace_test.go:209-220` harness); seam-faked boot ⇒
  `compass.microvm.boot.duration` has one point with `outcome=ok` and no
  other attribute; faked death ⇒ `teardowns{cause=vmm_death}` = 1; every
  data point asserted to carry ONLY enum attributes (the
  `dispatchedCounts`-style attribute audit, `trace_test.go:253-256`); a
  no-op global meter ⇒ construction succeeds, records are skipped, nothing
  panics; a seam-faked `PSS()` that blocks ⇒ a concurrent `m.mu`
  acquisition still proceeds (the callback held the lock only for the
  snapshot).

### W5 — startup wiring: `setupOtel` reorder + Runner-host session counter (hermetic)

The §(d) main.go ordering fix and the `backend`-labelled session metric.

- **Interfaces:** produces
  - `main.go`: `setupOtel(ctx)` moved ahead of `verifyBackendPreflight`
    (currently `main.go:167-172` vs `main.go:110-112`) so canary/preflight
    metrics hit a real provider (OQ-3); the `orphanReaper` probe call (W2)
    lands here too;
  - `go/internal/runtime`: `func (m *MicroVMRuntime) BackendName() string { return "microvm" }`
    and `func (p *PodmanCLI) BackendName() string { return "podman" }` —
    NOT on the frozen interface;
  - `go/internal/runner/host.go`: `agentHost` resolves the backend name
    once at construction via `interface{ BackendName() string }` (unknown ⇒
    `"unknown"`, never a per-call probe) and mints
    `compass.runner.session.starts` (Int64Counter,
    `backend` + `outcome` attributes) recorded in `Start`
    (`host.go:306-444`), nil-on-error posture as everywhere;
  - consumes W4's conventions; no proto or interface change.
- **Test cycle (hermetic):** ManualReader over a fake-engine `agentHost`
  `Start` ⇒ one `session.starts` point with `backend` set from the probe
  and no other attribute beyond the enum pair; an engine without the probe
  ⇒ `backend="unknown"`, no error; a `main`-level dispatch test asserting
  `setupOtel` precedes the preflight (ordering pinned via the existing
  startup-dispatch test seam, `main.go` fake engines).

## Tasks

- [ ] W1 — host-written `vmm.pid`/`virtiofsd.pid`/`passt.pid` with
      `<pid> <starttime> <bootid>` identity and atomic temp-then-rename
      writes; retire passt `--pid`; write-after-spawn, remove-on-Shutdown,
      boot fails on write failure
- [ ] W2 — `(*MicroVMRuntime) ReapOrphans(ctx) error`: scan runtime dirs,
      boot-id short-circuit + starttime-verified SIGTERM→SIGKILL bounded
      by ctx, ESRCH/EPERM differentiated, VMM first, remove fully-dead
      dirs, age-gate empty dirs, skip table-live sessions; `main.go` probe
      wiring, warn-never-abort
- [ ] W3 — `VM.DeathWatch()` (single-consumer) + `VM.VMMExited()` over the
      reaper channels; per-session monitor with `tearingDown`
      disambiguation set by Stop/Remove and CLEARED by Start;
      `*runtime.SessionDeadError` on refused and in-flight execs;
      idempotent Remove proven on a dead VM
- [ ] W4 — `microvmMetrics` (boot/teardown/vsock-RPC/canary/orphans/PSS/
      quota instruments, warn-and-disable), INFO transition logs with
      session id + timings
- [ ] W5 — `setupOtel` before the preflight; `BackendName()` probes;
      `compass.runner.session.starts{backend,outcome}` in the Runner host

## Open Questions

Batched for the pre-freeze ruling; the body designs against each
recommendation.

- **OQ-1 (non-load-bearing) — the runtime-dir file set diverges from the
  parent's Interfaces sketch.** The parent sketches
  `{vmm.pid,virtiofsd.pid,netbackend.pid,vsock.port}`
  (microvm-runner.md:589-590). V7 keeps `passt.pid` (the name already on
  disk, `launch.go:178`) over `netbackend.pid`, and ships NO `vsock.port`
  file: the control-plane port is a fixed per-host constant with per-session
  identity on the socket paths (`microvm_lifecycle.go:48-52`), so the file
  would be a reader-less copy of a constant. **Recommendation:** ratify both
  divergences as (benign, naming/dead-file) deviations from a sketch, not
  from a frozen behavior.
- **OQ-2 (load-bearing) — "every existing session metric gains a `backend`
  label" is vacuous today; V7 mints the first such metric instead.** A
  tree-wide instrument-constructor search this session found exactly one
  OTel metric, `compass.delivery.dispatched` (server-side fan-out,
  `consumer.go:269-272`) — not a session metric — and two server `expvar`
  webhook counters; the Runner emits no metrics at all. Options: (i) mint
  `compass.runner.session.starts{backend,outcome}` in the backend-agnostic
  host as the first labelled session metric (delivers the intent; the label
  exists from birth for every future session metric to copy); (ii) declare
  the obligation satisfied-by-vacuity and ship nothing (honest to the
  letter, but V8's benchmark then has no per-backend series to compare);
  (iii) retrofit `compass.delivery.dispatched` (wrong process — the server
  doesn't know the backend). **Recommendation:** (i). If the parent meant a
  wider session-metric set, that set does not exist to label, and inventing
  it is not V7 scope.
- **OQ-3 (non-load-bearing) — `setupOtel` moves ahead of the backend
  preflight.** Today the preflight (and its canary) runs at
  `main.go:110-112`, the meter provider installs at `main.go:167-172`, so
  preflight-time metrics would hit the no-op global and vanish. The reorder
  is safe: `setupOtel` is env-only and its disabled path is a no-op
  (`provider.go`). **Recommendation:** reorder; keep instrument creation at
  construction time.
- **OQ-4 (non-load-bearing) — the parent's "virtiofsd restarts" metric is
  translated to a teardown-cause series.** Under §(f)'s frozen
  fatal-no-restart posture (microvm-runner.md:249-251) a virtiofsd death is
  a session teardown, never a restart, so a restart counter would be
  constant zero by design. `compass.microvm.teardowns{cause=virtiofsd_death}`
  carries the actionable number (how often virtiofsd kills sessions).
  **Recommendation:** ratify the translation; if a restart path ever lands,
  it mints its own counter.
- **OQ-5 (load-bearing) — a `ReapOrphans` failure is a startup WARN, not an
  abort.** Un-reaped orphans hold stale resources but cannot collide with
  new sessions (fresh random ids, `microvm_lifecycle.go:332-341`); aborting
  turns one wedged process into a box outage. The D3 hard-fail posture
  governs *capability* preflights (can this host run VMs), not cleanup.
  Options: (i) warn + retry next startup; (ii) abort startup (symmetric
  with D3 but over-broad); (iii) warn but refuse only microVM-backend
  session creation until a clean reap (complexity without a demonstrated
  need). **Recommendation:** (i), with the per-dir error detail in the WARN
  and the `orphans.reaped` counter making silent rot visible.
- **OQ-6 (non-load-bearing) — passt death is not session-fatal.** The
  parent's failure matrix names VMM death and virtiofsd death only
  (microvm-runner.md:244-253); passt dying leaves the vsock control plane
  (served by the VMM over AF_UNIX) intact, so Stop/Remove still work while
  the guest loses egress — the agent's own calls fail visibly and the
  session lifecycle handles it. `DeathWatch` therefore excludes passt.
  Alternative: extend the matrix and treat passt as fatal too (symmetric,
  but extends a frozen matrix). **Recommendation:** matrix-literal
  (non-fatal); revisit with V8 evidence if egress-less zombie sessions show
  up in practice.
- **OQ-7 (load-bearing) — what the reaper does with a no-readable-pidfile
  dir, ruled together with §(a)'s atomic write.** A dir with no readable
  pidfiles is a crashed pre-spawn `Create` (safe to remove), a concurrent
  `Create`'s pre-insert window (must not remove, §(b) step 1), or — the
  case that makes this a correctness question, not a latency knob — a dir
  whose pidfiles were destroyed while the processes they named still run:
  REMOVING it destroys the only trace of a possibly-leaked live process,
  while QUARANTINE-and-WARN preserves the evidence at the cost of
  accumulating rot. The two rulings are inseparable: without §(a)'s atomic
  temp-write-then-rename, a torn `os.WriteFile` during the exact crash
  window `ReapOrphans` exists for produces this state organically — and
  removal then leaks a recorded live child while erasing its record; WITH
  the atomic write (as the body now specifies), the state is reachable
  only via external tampering and removal becomes defensible again. The
  mtime age gate (`orphanDirGrace = 1m`) stays regardless — it separates
  the Create pre-insert window (microseconds of lock work) from everything
  else by orders of magnitude and bounds cleanup latency — but it is
  subordinate to the remove-vs-quarantine ruling. Options: (i) remove
  after the grace, given the atomic write; (ii) quarantine-and-WARN (leave
  or rename aside, log loudly, never auto-remove). **Recommendation:**
  (i) — the atomic write closes the organic path, tampering is outside any
  threat model the reaper can meaningfully defend, and quarantine converts
  a self-healing startup into an operator chore; ratify 1m as the named
  constant alongside. Rule the atomic write and the removal arm together.
- **OQ-8 (load-bearing) — RunRoot single-owner exclusion: nothing prevents
  two Runners over one RunRoot, and the reaper would then kill LIVE
  sessions.** The most serious pre-freeze finding. §(b)'s table-skip guard
  is intra-process; the realistic breach is not operator error but a
  restart racing a still-draining predecessor: the new Runner's
  `ReapOrphans` sees the old Runner's live sessions as orphan dirs, and
  the §(a) identity check — pid, starttime, boot id all genuinely
  matching — CONFIRMS the kill rather than preventing it. The defense
  built to stop kill-an-innocent makes this wrong kill MORE certain, one
  layer up. D8's one-Runner-per-box is a deployment intention, not an
  enforced invariant. Options: (i) an `flock`ed lockfile at the RunRoot
  root, taken exclusively at startup before any reap and held for the
  Runner's life — a second Runner fails the lock and refuses startup; the
  kernel drops the lock on ANY holder death, SIGKILL included, so a
  crashed Runner never wedges its successor; (ii) declare D8 plus
  deployment tooling the guarantee and document `ReapOrphans` as unsafe
  under concurrent Runners; (iii) record the owning Runner's own
  (pid, starttime, bootid) in RunRoot and have `ReapOrphans` skip dirs
  while that recorded owner is alive. **Recommendation:** (i),
  refuse-not-wait: the standard single-owner mechanism, one file and one
  syscall, turning the race into a loud startup failure that a
  supervisor's restart backoff resolves naturally once the drain
  finishes — and, unlike (iii), it has no stale-record arm to reason
  about (the lock IS liveness). (ii) leaves the kill reachable; (iii)
  rebuilds half of `flock` by hand. The body assumes (i): §(b) never runs
  while another Runner holds the RunRoot.
- **OQ-9 (load-bearing) — "per-VM RSS" vs the fleet-summed PSS gauge: a
  ruling on a frozen sentence.** The parent freezes "per-VM RSS"
  (microvm-runner.md:266); §(d) delivers `compass.microvm.guest.memory.pss`
  summed per process kind with no per-session label (the cardinality rule,
  `consumer.go:249-254`). The aggregate cannot identify a runaway VM and
  cannot feed V8's per-VM RSS benchmark comparison — whether the frozen
  line means fleet-sum or per-VM is not the designer's call. Options: (i)
  accept the aggregate gauge and source V8's per-VM numbers from the
  benchmark harness calling `PSS()` directly (no metric); (ii) periodic
  INFO log lines carrying per-session PSS — logs are exempt from the
  cardinality rule, the record's own argument (§(d); the delivery
  consumer logs ids while metering without them, `dispatch.go:363-377`);
  (iii) an exemplar/high-watermark scheme (e.g. a max-per-session gauge).
  **Recommendation:** (i) + (ii) combined: keep the aggregate gauge as
  the fleet trend, add per-session PSS to a low-frequency INFO line for
  runaway identification, and let V8's benchmark read `PSS()` directly —
  no per-session metric label is ever minted and every consumer of the
  frozen sentence gets a number. The body designs against this assumption
  (§(d)).
- **OQ-10 (load-bearing) — the guest is dropped from V7's supervised set;
  the parent's sentence says it is in it.** Parent §(f): the backend
  supervises the per-session process set including "the guest via the
  supervisor channel's liveness" (microvm-runner.md:240-241). V7's
  `DeathWatch` covers host children only; a guest that kernel-panics and
  hangs under a live VMM is a zombie session detected by nothing except
  per-exec timeouts (`execDefaultTimeout`, `microvm_lifecycle.go:95-97`)
  on sessions actively being used. Options: (i) declare per-exec timeout
  coverage sufficient for V7, explicitly re-scope the parent's sentence,
  and defer active guest health-probing to V8 alongside the acceptance
  suite; (ii) add a low-frequency `Health`-poll arm to the session monitor
  now (the RPC exists, PR #912 `launch.go:548-555`).
  **Recommendation:** (i): an idle-guest zombie holds quota but corrupts
  nothing, a poll arm mints a new false-positive teardown class (a loaded
  guest missing a poll deadline) with no V8 evidence to tune against, and
  V8's acceptance suite is where liveness probing earns its thresholds.
  The deviation is surfaced in §(c), not silent; either option needs the
  human's explicit ruling on the parent's sentence.
