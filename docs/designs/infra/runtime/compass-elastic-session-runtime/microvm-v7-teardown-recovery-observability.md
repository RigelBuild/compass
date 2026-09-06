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
`docs/designs/DECISIONS.md` is untouched. Two rulings DO land on that frozen
supervised-set sentence rather than merely detailing it — the guest (OQ-10)
and the net backend (OQ-6) both leave V7's session-fatal handling — and both
are graded load-bearing and carried to the human for an explicit ruling
rather than absorbed here.

## Problem / Intent

The microVM backend can boot, exec, and tear down a session it owns — but only
along paths where the Runner stays alive and the VM's processes die when told
to. Three gaps remain, and they are the last V-milestone before the V8
acceptance suite:

1. **Nothing survives a Runner crash.** Each session's VMM/virtiofsd/passt are
   direct children with a best-effort `PR_SET_PDEATHSIG` guard — "The real
   teardown guarantee is Shutdown, not Pdeathsig"
   (`go/internal/runtime/microvm/launch.go:284-289`) — and the per-session
   runtime dir records only passt's pid: `Launch` passes
   `"--pid", vm.pidfile` for passt alone (`launch.go:178-196`), while the VMM
   and virtiofsd leave no on-disk identity. The parent requires startup
   orphan-reaping "by their per-session runtime dir (pidfiles + process-
   liveness check)" (microvm-runner.md:254-256); with no VMM/virtiofsd
   pidfiles there is nothing to reap them by. §(a) adds the pidfiles (with
   a pre-spawn intent record, so a crash mid-spawn cannot leave a live
   orphan unrecorded), §(b) the reaper — behind a RunRoot lock, because a
   reaper that cannot tell a live Runner's sessions from a dead one's
   orphans kills the wrong VMs with full confidence.

2. **Mid-session death is undetected.** V6's supervision core observes every
   child's exit through its sole reaper (`c.exited` closed after the single
   `c.cmd.Wait()`, PR #912 `launch.go:486-495`), but nothing *acts* on a death
   after boot: a VMM that dies mid-session leaves in-flight `Exec`s failing
   with an undifferentiated transport error, live peer daemons, and a session
   entry that looks startable. The parent's matrix (microvm-runner.md:244-253)
   demands a distinguishable error, peer teardown, and an idempotent `Remove`;
   virtiofsd death is "fatal to the session (no remount-and-hope)". A
   session can have two VM lives (`Stop` keeps the entry), so detection is
   stamped per VM life rather than per session. §(c).

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
`go/internal/runtime/microvm_lifecycle.go:225-228`), plus one Runner-scoped
lockfile a level up (§(b) step 0):

| File | Process | Writer |
| --- | --- | --- |
| `<id>/vmm.pid` | cloud-hypervisor | host (`launch`) |
| `<id>/virtiofsd.pid` | virtiofsd | host (`launch`) |
| `<id>/passt.pid` | passt | host (`launch`) |
| `.runner.lock` | (none — the RunRoot owner's `flock`) | host (`lockRunRoot`) |

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
reboots too.** A pidfile holds one line in one of two forms — the SETTLED
record `<pid> <starttime> <bootid>`, or the pre-spawn INTENT record
`intent <bootid>` the write lifecycle below explains:
`<starttime>` is field 22 of `/proc/<pid>/stat` — the kernel's process start
time in clock ticks since boot, immutable for the process's life, so
(pid, starttime) is unique across reuse within a boot for any pid-wrap
slower than one clock tick (`USER_HZ`, conventionally 100 Hz ⇒ 10 ms): the
same identity pair systemd and every pidfd-less supervisor relies on, a
bound rather than a kernel guarantee — and `<bootid>` is
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

**Write lifecycle, and the spawn→write window.** `launch` records each child
in TWO steps, because the pid does not exist until the spawn returns and a
crash can land between the two:

1. Before `startChild`, write an INTENT record — the single line
   `intent <bootid>` — to that child's pidfile path.
2. After `startChild` succeeds, replace it with `<pid> <starttime> <bootid>`.

Both writes are atomic: the line goes to a temp file in the same runtime dir,
then `os.Rename` into place — a same-directory rename is atomic, so a reader
sees the complete record or no file, never a torn prefix, and step 2's rename
supersedes step 1's record in one operation. A plain `os.WriteFile` is not
atomic, and the torn-write window is EXACTLY the Runner-crash window
`ReapOrphans` exists for: a half-written pidfile would demote a recorded live
child to §(b)'s no-pidfile arm — never killed, its dir age-gate-removed, the
orphan leaked with its only record destroyed.

**The intent record exists because atomicity alone does NOT close the
spawn→write window.** Spawn order is virtiofsd, passt, then the VMM LAST
(`startChild(vm.virtiofsd)` at `launch.go:168`, `startChild(vm.passt)` at
`launch.go:197`, `startChild(vm.vmm)` at `launch.go:222`), so a Runner dying
between `startChild(vm.vmm)` returning and `vmm.pid` being renamed into place
would leave a dir holding two perfectly parseable pidfiles naming two
now-dead helpers and NO record at all of a live orphan VMM — whereupon §(b)
step 3 reads "everything recorded is dead", `os.RemoveAll`s the dir, and
erases the only evidence of the leak. That is not a torn write and no
atomicity buys it back; it is an ORGANIC under-recording path, and it is the
exact outcome OQ-7 calls a correctness question rather than a latency knob.
`PR_SET_PDEATHSIG` does not rescue it either — it is explicitly best-effort,
"The real teardown guarantee is Shutdown, not Pdeathsig"
(`launch.go:284-289`). Step 1 closes the window instead of reasoning around
it: the dir names every child that may be live BEFORE it can be live, so
§(b) treats a leftover same-boot intent record as a possibly-live but
unidentifiable orphan (WARN, counted, dir KEPT — never a kill, since there
is no pid to verify), and a leftover intent record from a PREVIOUS boot as
dead by the same boot-id short-circuit a pidfile gets. At most one intent
record can dangle per dir, because the writes are sequential, and it names
WHICH child — so the WARN is actionable rather than a shrug.

A pidfile write failure (either step) fails the boot — the existing error
path, `launch`'s deferred `vm.Shutdown` tearing down what started
(`launch.go:146-152`): orphan reapability is load-bearing, so a session that
cannot be recorded must not run. `Shutdown` removes all three alongside the
sockets (extending the existing pidfile removal, `launch.go:379-389`), and
`Remove`'s `os.RemoveAll(session.runtimeDir)`
(`microvm_lifecycle.go:674-676`) is the backstop — a pidfile structurally
cannot outlive its runtime dir.

**Invariant preservation.** The two-step write makes the on-disk record
CONSERVATIVE by construction: at every instant the set of live children is a
subset of {children named by a complete pidfile} ∪ {the child named by a
dangling intent record} — and BOTH terms now live on disk, so a crash at any
instant leaves a dir that names every process it may have started. (With a
write-after-spawn-only scheme the second term existed solely in the crashed
process's memory, which is the defect above.) The record may OVER-name — a
child that never spawned, or one that already died — and §(b) is built to
tolerate exactly that (starttime mismatch ⇒ no kill; stale boot id ⇒ no
kill); it can never UNDER-name a live child, which is the failure §(b)
cannot recover from.

The writes themselves are plain file I/O (temp-write-then-rename) around
`startChild`; they do not touch the child's reaper, `exited` channel, or
`Wait` discipline (one reaper per child installed by `startChild`, "exactly
one goroutine per child owning exactly one cmd.Wait", PR #912
`launch.go:460-465`; the single `c.cmd.Wait()` at PR #912 `launch.go:493`
stays the package's only Wait, verified by grep this session).

### (b) `ReapOrphans(ctx) error`: kill-and-remove at startup, never adopt

The parent is explicit: "on startup the backend reaps orphaned
VMM/virtiofsd/net-backend processes by their per-session runtime dir …
Healthy VMs found at restart are **killed and rebooted on next request, not
adopted**: the supervisor handshake state is in-process and not
reconstructable across a Runner restart" (microvm-runner.md:254-261). V7
honors that verbatim — the reaper has no adoption path, no health probe, no
attempt to re-dial a found VM's vsock socket.

```go
// ReapOrphans takes the exclusive RunRoot lock (step 0 — refused ⇒ error,
// no scan), then scans <RunRoot>/microvm/*/ for runtime dirs left by a
// previous Runner process, kills every recorded process that is verifiably
// the one the pidfile named (boot id + pid + starttime match), and removes
// the dir. Healthy VMs are killed, never adopted (microvm-runner.md:258-261).
// Idempotent and safe to re-run: a partial reap leaves the dir for the next
// attempt.
func (m *MicroVMRuntime) ReapOrphans(ctx context.Context) error
```

**Step 0 — the RunRoot lock, a DELIVERED mechanism, not an assumption.**
Before any scan, `ReapOrphans` takes an exclusive non-blocking `flock` on a
lockfile at the RunRoot root and holds it for the Runner's life. This is a
W2 deliverable in its own right, not a property inherited from OQ-8's
recommendation: the reaper's whole defense against killing a second live
Runner's sessions is this lock, and a safety property that exists only as
an Open Question's preference is not implemented. OQ-8 still rules on WHICH
mechanism; the body ships one so that W1-W5 implemented verbatim cannot
produce a reaper with no cross-process exclusion at all.

```go
// lockRunRoot takes LOCK_EX|LOCK_NB on <RunRoot>/microvm/.runner.lock and
// returns the release func. Refuse-not-wait: EWOULDBLOCK means another
// Runner owns this RunRoot, and ReapOrphans returns that error WITHOUT
// scanning a single dir — a scan there would kill live sessions whose §(a)
// identity check matches perfectly. The lock is held for the Runner's life
// (release runs at shutdown), and the kernel drops it on ANY holder death,
// SIGKILL included, so a crashed Runner never wedges its successor. That
// release-on-death property is why OQ-8 recommends flock over a recorded
// (pid, starttime, bootid) owner file: the lock IS liveness, with no stale
// record to reason about.
func (m *MicroVMRuntime) lockRunRoot() (release func(), err error)
```

**What a refused lock does to startup is a sub-fork, and it is OQ-5's, not
OQ-8's.** OQ-8's option (i) is worded "a second Runner fails the lock and
refuses startup", while OQ-5 rules that a reap failure is a WARN and never
an abort — and a refused lock arrives as a `ReapOrphans` error, so the two
wordings collide. The body takes OQ-5's arm: WARN and continue, having
scanned NOTHING. The reasoning is that the lock's job is preventing the
wrong reap, which a refused lock has already accomplished; the second
Runner's own sessions are freshly-minted ids in fresh dirs
(`mintSessionID`, `microvm_lifecycle.go:332-341`) and cannot collide with
the predecessor's, so refusing startup over a still-draining predecessor
would convert a handled race into the box outage OQ-5 rejects. Ruling OQ-8
(i) with refuse-startup semantics instead is coherent — it enforces
one-Runner-per-box outright rather than only protecting the reap — but it
is a STRICTER posture than OQ-5 grants, so it needs saying explicitly
rather than inheriting. Both OQs should be ruled in one pass.

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
   identity check and be killed with full confidence. Step 0's RunRoot lock
   is what makes that unreachable; OQ-8 (load-bearing) rules on the
   mechanism, and the body ships `lockRunRoot` as the one it designs
   against. If the human rules OQ-8 another way, THIS clause changes with
   it — the reaper never scans without some cross-process exclusion in
   hand.
2. **Kill recorded processes, VMM first.** For each pidfile present: parse
   the line. An INTENT record (`intent <bootid>`, §(a)) names a child whose
   spawn may or may not have happened and carries no pid — from the current
   boot it is handled by step 3's possibly-live arm (WARN, dir kept, no
   kill, because there is nothing to verify or signal); from a previous boot
   it short-circuits dead like any stale record. A settled record parses as
   `<pid> <starttime> <bootid>`: a boot id differing from
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
3. **Remove the dir only when everything recorded is dead, and NOT when
   anything recorded is merely unidentifiable.** All recorded processes
   confirmed gone AND no same-boot intent record present ⇒ `os.RemoveAll`
   the dir (sockets, logs, pidfiles — everything; the session volume is
   untouched, it lives outside RunRoot on the durable volume,
   microvm-runner.md:251-253). A process that would not die within the
   bounded escalation leaves the dir in place and contributes a named error
   to the joined return — the next startup retries; a partial reap is
   re-runnable by construction because every step is idempotent.

   Three arms keep a dir that step 3 must NOT remove:

   - **A same-boot intent record** (§(a)) means `launch` crashed between
     writing the intent and completing the spawn-and-settle: the named child
     may be running right now with no pid on disk. The dir is KEPT with a
     WARN naming which child, and the `orphans.reaped` sibling counter gets
     a `possibly_live` series so the rot is visible rather than silent.
     Removal here is the one outcome §(a)'s two-step write exists to
     prevent, so no age gate promotes it to removable — an operator (or a
     reboot, which retires it via the boot id) resolves it.
   - **Unparseable pidfile content** — treated as no-pidfile for that entry
     and logged at WARN with the content: nothing identifiable to kill.
     §(a)'s atomic temp-write-then-rename means a torn write cannot produce
     this state, so it is reachable only through external tampering. That
     atomicity is what makes REMOVING such a dir defensible; it is NOT what
     makes the intent arm above unnecessary, because the spawn→write window
     is an organic under-recording path atomicity cannot touch (§(a)) — the
     two are different failures and OQ-7 rules on them separately.
   - **A dir with NO pidfiles at all**, younger than a small grace
     (mtime ≤ `orphanDirGrace = 1m`), which may be a concurrent `Create`'s
     pre-insert window (see 1). Older than the grace it is removed, per
     OQ-7's recommendation.

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
// DeathCause names which child died, for the session-level monitor.
type DeathCause string

const (
    DeathVMM       DeathCause = "vmm"
    DeathVirtiofsd DeathCause = "virtiofsd"
    // DeathPasst is REPORTED, never fatal (§(c)): the monitor logs and
    // counts it and keeps watching. It exists so the net backend does not
    // leave the parent's supervised set unobserved (microvm-runner.md:240-241).
    DeathPasst DeathCause = "passt"
)

// DeathWatch returns a channel that receives one DeathCause per child death
// among the VMM, virtiofsd, and passt, and closes once every watched child
// that can still die has died. SINGLE-CONSUMER: each send is drained by
// exactly one receiver, the session monitor below; a second receiver would
// steal events and then observe only the close, reading the zero value
// DeathCause(""). At most three sends occur, so the channel is buffered to
// three and the internal goroutine never blocks on a monitor that has
// stopped receiving after a fatal cause. The method cannot be unexported
// instead — the monitor lives in package runtime and reaches it through the
// guestVM seam (microvm_lifecycle.go:99-113) — so the contract is this
// comment plus the hermetic test, not visibility. It observes the children
// through their sole reapers' exited channels — it never calls Wait — so
// the one-reaper-per-child invariant holds.
func (vm *VM) DeathWatch() <-chan DeathCause
```

Internally one goroutine per VM `select`s over `vm.vmm.exited`,
`vm.virtiofsd.exited`, and `vm.passt.exited` (nil-guarded like every other
consumer of those channels), sending each exit as it is observed and
returning when all non-nil children have been reported. It always
terminates: any teardown path kills the VMM and reaps the daemons, closing
all three channels.

**The session monitor, stamped with its VM's epoch.** `Start`, in the same
locked critical section that transfers ownership to the session table
(`session.vm = vm` … `booted = false`, `microvm_lifecycle.go:401-415`),
increments the session's `epoch`, captures that value, and spawns one
monitor goroutine carrying it. The loop receives from `DeathWatch()` and,
under `m.mu`, applies two gates before acting: the death must belong to the
CURRENT VM life (`session.epoch == myEpoch`) and must not be a deliberate
teardown (`!session.tearingDown`). A stale-epoch event is discarded and the
monitor returns; a deliberate teardown's exit is discarded silently.

Past those gates the cause decides:

- `DeathPasst` — log INFO, count `peer.deaths{process=passt}`, and CONTINUE
  receiving. Not fatal, but never unobserved (§(c) deviation note below).
- `DeathVMM` / `DeathVirtiofsd` — record `deadCause` on the session and run
  the one teardown path that already exists, `vm.Shutdown` ("the VMM is
  killed first … then virtiofsd and passt are reaped … and finally the
  AF_UNIX sockets and passt's pidfile are removed", `launch.go:352-357`) —
  which is exactly "kill the VMM, same teardown path" for the virtiofsd
  arm, kills the peers for the VMM arm, and removes the socket/pidfile
  state. Then the monitor returns: the teardown's own cascade of child
  exits is not a second death to report.

The vsock "port" needs no separate release: the port is a fixed constant
and per-session identity rides the AF_UNIX socket paths Shutdown removes
(`microvm_lifecycle.go:48-52`).

State on `microvmSession` (all under `m.mu`, the existing discipline —
"All fields are read/written under MicroVMRuntime.mu",
`microvm_lifecycle.go:141-144`):

```go
// deadCause is non-empty once the monitor observed a mid-session child death
// and tore the session down; read by Exec/ExecStreaming/Start to refuse with
// a *SessionDeadError.
deadCause microvm.DeathCause
// epoch identifies the session's CURRENT VM life. Start increments it in the
// same locked critical section that stores the new VM
// (microvm_lifecycle.go:401-415) and hands the new value to that VM's
// monitor; a monitor whose captured epoch no longer equals this field is
// watching a VM that has already been replaced, and DISCARDS its death
// event. Monotonic, never reset, and only ever read/written under mu.
epoch uint64
// tearingDown is set (under mu) by Stop and Remove before they call
// vm.Shutdown, so the CURRENT epoch's monitor can tell a deliberate
// teardown's exit from a crash. Start clears it alongside the epoch bump.
tearingDown bool
```

**Why an epoch and not the boolean alone.** A boolean describes the SESSION
while a death describes a specific VM, and one session can have two VM lives
— `Stop` keeps the session entry (`microvm_lifecycle.go:607-634`), so
start-after-stop is a lifecycle the podman parity allows. That opens a race
the flag cannot close. `Stop` sets `tearingDown`, then `vm.Shutdown` kills
VMM#1 and waits on its exit itself ("VMM first: kill outright, then let the
sole reaper's single Wait complete via vmmExited" — `Process.Kill()` then
`<-vm.vmmExited`, `launch.go:361-369`; the VMM's `exited` is therefore
ALREADY closed when `Stop` returns), so `Stop` can return before VM#1's
`DeathWatch` goroutine is ever scheduled. If `Start` then runs — clearing
the flag and storing VM#2 — monitor#1 finally wakes, reads
`tearingDown == false`, concludes "crash", and marks a healthy just-booted
session dead; every subsequent `Exec` refuses with `*SessionDeadError` for a
VM that is fine. Nothing schedules `DeathWatch`'s selector before `Stop`'s
caller resumes, so this is ordinary Go scheduling, not an exotic
interleaving.

The epoch makes the stale monitor STRUCTURALLY inert rather than
timing-dependent: monitor#1 holds epoch 1, the session now reads epoch 2,
the event is discarded whatever `tearingDown` says. `tearingDown` keeps a
narrower job — suppressing the CURRENT epoch's expected exit during a
deliberate `Stop`/`Remove` — where a boolean is exactly right, because
within one epoch there is only one VM to describe.

Both fields stay on the session rather than on per-VM state: every
`microvmSession` field already lives under `m.mu` (the discipline quoted
above, `microvm_lifecycle.go:141-144`), so the epoch bump and the flag reset
are two lines inside a critical section the ownership transfer already
takes. Putting the generation on the VM instead would open a second
synchronization domain behind the `guestVM` seam AND would not help: the
monitor must compare its stamp against the SESSION's current value to learn
that it has been superseded, which is a session-level fact by definition.

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

Two paths produce it: (i) `Exec`/`ExecStreaming` entered after the death
check `deadCause` under the lock (extending `startedExec`,
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

**`Stop` and `Remove` are NOT in the refuse list — a dead session must
still be tearable-down.** Only the exec verbs refuse. The Runner's sole
teardown path is Stop-then-Remove with an early return on Stop's error:

```go
func (r *AgentRuntime) Teardown(ctx context.Context, handle *AgentHandle) error {
    if err := r.runtime.Stop(ctx, handle.id, stopTimeout); err != nil {
        return atStage("stop", err)
    }
    if err := r.runtime.Remove(ctx, handle.id); err != nil {
        return atStage("remove", err)
    }
    // …
}
```

(`go/internal/runtime/agent.go:216-222`, and `agentHost.Remove` tears a
session down "through the AgentRuntime (stop + remove + deregister)",
`host.go:537-546`.) A `Stop` that refused a dead session would return at
stage "stop", `Remove` would never be reached, and the session-table entry
and its runtime dir would leak PERMANENTLY — the one state the whole
reap-and-teardown design exists to prevent, reachable through the ordinary
API. It would also contradict three things at once: the frozen parent's
"`Remove` is idempotent on an already-dead VM" (microvm-runner.md:248), the
"Idempotent Remove, unchanged" paragraph above, whose premise is that a dead
session removes cleanly, and today's deliberately tolerant `Stop`, which
already answers success for a session with no VM to stop ("A session that
never started (no VM handle) is a no-op success" / `if vm == nil { return
nil }`, `microvm_lifecycle.go:600-617`).

So `Stop` on a dead session is a NO-OP SUCCESS, the same arm as
never-started: the monitor has already run `vm.Shutdown`, the VMM is gone,
the peers are reaped and the sockets removed, so there is nothing left for
`Stop` to do and nothing to report as a failure. `deadCause` stays set for
`Remove` to observe and report, exactly as before. Teardown is the one
operation a dead session must never refuse.

**Idempotent Remove, unchanged.** `Remove` already tolerates a dead VM:
`vm.Shutdown` is `sync.Once`-guarded ("safe to call twice",
`microvm_lifecycle.go:652-656`), the monitor's teardown ran it first, and
`Remove`'s own call returns the recorded `shutdownErr` and proceeds to
`os.RemoveAll` + table delete. A dead session's entry stays in the table
(with `deadCause` set) until `Remove` — deliberately, so the Runner's Remove
path and `Exists` behave identically to a live session's, and the death is
reportable rather than silently vanished.

**Deviation surfaced: the net backend leaves V7's supervised set for
FATALITY, not for observation.** The parent's §(f) preamble supervises the
process set including the "net backend" and "the guest via the supervisor
channel's liveness" (microvm-runner.md:240-241). V7 diverges on BOTH members
of that one sentence, and both are surfaced:

- **passt (net backend).** A passt death is not session-fatal — the vsock
  control plane is served by the VMM and stays up, so `Stop`/`Remove` keep
  working while the guest loses egress. But non-fatal must not mean
  UNOBSERVED: with no host-side detection an operator gets no signal at all
  that a session lost egress, which would remove passt from the supervised
  set outright rather than re-scoping its failure handling. So `DeathWatch`
  gains a `DeathPasst` cause that is REPORTED and NEVER fatal: the monitor
  records an INFO `microvm session peer died` line and a
  `compass.microvm.peer.deaths{process=passt}` counter, then keeps watching
  — it does not set `deadCause`, does not call `vm.Shutdown`, and does not
  make the session refuse anything. `DeathWatch` therefore no longer closes
  after one send; see its contract above. The fatality ruling is OQ-6
  (load-bearing: it rules on frozen text), and the body designs against its
  recommendation, matrix-literal non-fatality WITH observation.
- **The guest.** `DeathWatch` observes host children only, so a guest that
  kernel-panics and hangs under a live VMM is a zombie session nothing
  detects except per-exec timeouts on sessions actively in use. That is a
  deliberate V7 re-scope carried as OQ-10 (load-bearing), not a silent drop;
  the body assumes its recommendation — per-exec timeout coverage suffices
  for V7, active guest health-probing lands with V8's acceptance evidence.

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
| `compass.microvm.guest.memory.pss` | Int64ObservableGauge | `By` | `process` = `vmm`\|`virtiofsd`\|`passt` | Sum over live sessions of per-process PSS via the existing `VM.PSS()` ("PSS, NOT summed VmHWM … PSS divides shared pages among their mappers", PR #912 `launch.go:663-668`), summed per process kind so no per-session label exists; kB→bytes at record |
| `compass.microvm.quota.used.ratio` | Float64ObservableGauge | `1` | none | Observed byte utilization of the session-volume quota, read from `m.lastQuota` (the preflight-time snapshot; see below) and recorded ONLY when that reading's `Active()` is true — V6's single-meaning discipline ("the caller must gate on Active() … V7 inherits a single-meaning number", PR #912 `microvm_quota.go:138-145`); silent (no point) when inactive or unset |
| `compass.microvm.canary.runs` | Int64Counter | `{run}` | `outcome` = `ok`\|`error` | Boot-canary executions by outcome — the parent's `compass_microvm_canary_ok` as a countable series |
| `compass.microvm.orphans.reaped` | Int64Counter | `{process}` | `process` = `vmm`\|`virtiofsd`\|`passt`, `outcome` = `killed`\|`possibly_live` | Orphan processes killed by `ReapOrphans`; the `possibly_live` series counts §(b) step 3's dangling-intent arm, where a child may be running with no pid on disk |
| `compass.microvm.peer.deaths` | Int64Counter | `{death}` | `process` = `passt` | Non-fatal peer-daemon deaths observed by the session monitor (§(c)): the net backend's death is reported, never a teardown |

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

**The quota gauge reads a NAMED snapshot, not a live syscall.** V6 leaves no
quota state behind to read: the preflight binds the reading as a local
consumed only by its own `slog.Info` and drops it when `verifyQuota` returns
(`reading, err := verifyVolumeQuota(...)` at PR #912
`microvm_preflight.go:174`, logged at PR #912
`microvm_preflight.go:189-194`), and the runtime struct carries no quota
field at all (`config`, `mu`, `sessions`, `launchFunc`, `newGuestClient` —
PR #912 `microvm.go:96-111`). An observable gauge fires on every collection
interval, so leaving the source unnamed would force the implementer to
invent one. V7 names it:

- **The field.** `lastQuota QuotaReading` on `MicroVMRuntime`, written under
  `m.mu` by the preflight's `verifyQuota` at the point it already has the
  reading in hand, read under `m.mu` by the gauge callback.
- **The staleness posture: preflight-time snapshot, NEVER refreshed.** The
  callback does no `statfs` — so it has no syscall cost, no error posture,
  and no `Active()`-transition semantics to specify, because the reading
  cannot transition. The number therefore answers "what was the tenant's
  utilization when this Runner started", which is what a V7 mechanism
  slice can honestly deliver; a live-tracking gauge needs a refresh policy
  and a staleness bound that V8's measurement work is the place to set.
- **Inactive or unset ⇒ no point emitted.** A zero-value `lastQuota` (the
  preflight skipped, or no volume root configured) has `Active() == false`
  by construction (`Active` requires `LimitBytes > 0 && FilesystemBytes >
  0`, PR #912 `microvm_quota.go:117-125`), so the same gate covers both
  cases with no extra flag.

A monotonic gauge is a real limitation, stated rather than hidden: if V8
wants live utilization it adds the refresh, and this row's meaning changes
with an explicit ruling instead of an implementer's guess.

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
`microvm session died` (`session_id`, `cause`, `uptime`) on a fatal cause
and `microvm session peer died` (`session_id`, `process`, `uptime`) on the
non-fatal passt arm (§(c)); `Stop`/`Remove` log `microvm session
stopped`/`removed` (`session_id`, `teardown_duration`); `ReapOrphans` logs
one line per reaped dir (`session_id`, `reaped` process list) plus a WARN
per dangling-intent dir naming the child (§(b) step 3). Session id in LOGS
is fine — the cardinality rule constrains metric labels, not log fields
(the delivery consumer meters without ids, `dispatch.go:379-383`, while
carrying them in the adjacent WARN, `dispatch.go:392-393`).

**Meter-setup ordering (OQ-3).** `main.go` today installs the meter provider
AFTER the whole engine-and-preflight block: `engine, err :=
backends.selectEngine()` is at `main.go:106`, `verifyBackendPreflight`
(which runs `BootCanary`) at `main.go:110-112`, and `setupOtel` only at
`main.go:167-172` — so a canary metric recorded at preflight time would hit
the default no-op global and vanish.

**One posture: `setupOtel` moves ahead of `selectEngine()`, and instrument
creation stays at construction time.** The target is `selectEngine`, not
merely the preflight, because that is where the instruments are born:
`selectEngine` returns `runtime.SelectBackend(...)` (`main.go:332-345`)
whose `"microvm"` case returns `NewMicroVMRuntime(cfg.MicroVM)`
(`microvm.go:117-122`), and W4 builds `microvmMetrics` inside
`NewMicroVMRuntime`. Moving `setupOtel` only ahead of
`verifyBackendPreflight` would leave it BELOW `main.go:106` — the preflight
dispatches on the engine, so the engine must already exist — and instrument
construction would still run against the default global. Ahead of
`selectEngine` there is no such conflict: `setupOtel` is env-only, displaces
no operator-input check, and its disabled path installs nothing and costs
nothing (`go/internal/otel/provider.go` noop path).

There is no lazy-on-first-record path. An earlier draft of this section
prescribed one and then also claimed construction-time creation; only the
latter survives, and it is what W4 implements.

Worth recording so the reorder is not over-sold: instrument CREATION would
survive a late provider install anyway. The global meter's `setDelegate`
re-delegates both instruments and registered callbacks when a real provider
arrives (`for _, inst := range m.instruments { inst.setDelegate(meter) }`
and the same loop over `m.registry`,
`go.opentelemetry.io/otel@v1.46.0/internal/global/meter.go:126-147`; v1.46.0
is the pinned version, `go/go.mod:35`). What is genuinely lost is
measurements RECORDED before the install — the canary's own points, which
is exactly the case OQ-3 exists for. The reorder is about records, not
about instrument validity.

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
- **Write-after-spawn only, relying on the atomic rename** ((a)). The
  original shape: one write per child, after `startChild` returns. The
  rename makes each write torn-proof but leaves the spawn→write window open
  — a crash there leaves a live VMM with NO on-disk record, and §(b) then
  removes the dir as fully-dead (§(a)). The pre-spawn intent record costs
  one extra rename per child on the boot path and converts an
  under-recording failure into an over-recording one, which §(b) already
  tolerates. Rejected.
- **A single boolean `tearingDown` as the whole death-disambiguation**
  ((c)). Cannot distinguish two VM lives of one session, so a stale
  monitor's death event can mark a healthy just-booted VM dead (§(c), "Why
  an epoch"). The epoch stamp makes the stale monitor structurally inert for
  one `uint64` under the lock the transfer already holds. Rejected.
- **`Stop` refusing a dead session with `*SessionDeadError`** ((c)). It
  reads as consistent with the exec verbs, but `Teardown` early-returns on
  Stop's error (`agent.go:216-222`), so a dead session could never be torn
  down and its table entry and runtime dir would leak permanently.
  Rejected — teardown is the one path a dead session must always accept.
- **Deployment discipline (D8) as the RunRoot single-owner guarantee**
  ((b), OQ-8). One Runner per box is an intention, not an enforced
  invariant, and the failure mode it leaves open is the reaper killing a
  live predecessor's sessions with the §(a) identity check CONFIRMING each
  kill. A lockfile is one file and one syscall. Rejected as the primary
  defense.
- **A live `statfs` inside the quota gauge callback** ((d)). Puts a syscall
  on every collection interval and needs an error posture and
  `Active()`-transition semantics that no V7 slice produces; the
  preflight-time snapshot delivers a single-meaning number with none of
  that (§(d)). Rejected for V7, revisit with V8's measurement work.
- **A `main`-level ordering test over the existing fake-engine seam** ((d),
  W5). The existing harness calls `verifyBackendPreflight` directly and
  never invokes `run()`, so it cannot observe `setupOtel`'s position — a
  test written against it passes either way (W5's test cycle). Rejected in
  favor of an extracted `startup` unit with injected hooks.
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
- **The reaper NEVER scans without the RunRoot lock in hand.** `ReapOrphans`
  acquires `lockRunRoot` before touching a single dir and returns the
  refusal error otherwise; no code path, test seam, or fake may bypass it
  (§(b) step 0). This is the one invariant standing between the §(a)
  identity check and a confident kill of a live Runner's sessions.
- **The on-disk record never UNDER-names a live child.** `launch` writes
  each child's intent record before its spawn and settles it after, so a
  crash at any instant leaves a dir naming every process it may have
  started (§(a)). Over-naming is fine — `ReapOrphans` tolerates it by
  construction; under-naming is the failure mode nothing downstream can
  recover from.
- **A named test must be able to FAIL on the bug it names.** Every case in
  the W-slices asserts against a seam that can actually observe the
  property: no ordering claim over a seam that cannot see the ordering
  (W5), and no lock-release claim whose fixture can return before reaching
  the blocking call (W4). Where no such seam exists, the slice extracts one
  or states plainly that the property is review-guarded.
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

Dependency order: W1 (pidfiles) precedes W2 (the reaper reads them, intent
records included). W3's hermetic tier is independent of W1/W2 and may run in
parallel; its KVM tier depends on W1 — both rewrite `Shutdown`'s cleanup
surface (`VM.pidfile` becomes W1's three-element set) and W3's KVM test
asserts the three-pidfile removal after a death teardown, so the assertion
differs by whether W1 landed. W4 (backend metrics + logs) consumes W3's death
causes and W2's reap outcomes; W5 (startup wiring + Runner-host counter)
consumes W2 and W4. W2 carries TWO deliverables that must land together —
`lockRunRoot` and `ReapOrphans` — because a reaper without the lock is the
live-session-killing scan OQ-8 exists to prevent; do not split them across
PRs.

### W1 — host-written pidfiles with boot-id + starttime identity (hermetic + KVM)

The §(a) layout: three pidfiles, one writer, atomic two-step writes (intent
before the spawn, settled record after), PID-reuse defense within and across
boots.

- **Interfaces:** produces, in `go/internal/runtime/microvm`
  (`//go:build unix` files):
  - `func writePidIntent(path string) error` — writes `intent <bootid>\n`
    (0600) via temp-file-then-`os.Rename` in the same dir, called BEFORE
    the child's `startChild` so the dir names every child that may become
    live (§(a));
  - `func writePidfile(path string, pid int) error` — reads
    `/proc/<pid>/stat` field 22 and the boot id (from
    `/proc/sys/kernel/random/boot_id`, read once per process and cached),
    writes `<pid> <starttime> <bootid>\n` (0600) by the same
    temp-file-then-`os.Rename`, superseding the intent record in one atomic
    operation — so a reader sees the intent record or the settled record,
    never a torn prefix and never nothing (§(a));
  - `type pidRecord struct { Intent bool; PID int; StartTime uint64; BootID string }`,
    `func readPidfile(path string) (pidRecord, error)` — parses either
    form, `Intent` true for the pre-spawn line,
    `func (r pidRecord) alive() (bool, error)` — boot-id short-circuit,
    then starttime-compared liveness (stale boot id ⇒ false, nil; ENOENT ⇒
    false, nil). A SAME-BOOT intent record is neither alive nor dead: it
    returns a distinct `errPidUnknown` so §(b) routes it to the
    possibly-live arm rather than to a kill or a removal;
  - `launch` extended: before each `startChild`, `writePidIntent`; after it
    succeeds, `writePidfile` — both into the runtime dir
    (`vmm.pid`/`virtiofsd.pid`/`passt.pid`); a failure of either returns an
    error (the existing deferred `vm.Shutdown` cleans up,
    `launch.go:146-152`);
  - passt argv loses `--pid` (`launch.go:187-196`); `VM.pidfile` becomes the
    three-element cleanup set removed by `Shutdown` beside the sockets
    (`launch.go:379-389`).
- **Test cycle:**
  - *Hermetic:* pidfile round-trip (write → read → fields match proc and
    the live boot id); intent round-trip (`writePidIntent` → `readPidfile`
    reports `Intent`, and `alive()` returns `errPidUnknown` for the current
    boot but false-nil for a perturbed boot id); `writePidfile` over an
    existing intent record leaves ONLY the settled record; `alive()` false
    on ENOENT, on a starttime mismatch (fake a record with a perturbed
    starttime for the test's own pid), and on a perturbed boot id; neither
    write leaves a temp file behind; unparseable content errors; a
    spawn-failure boot leaves the intent record and no settled record —
    the state §(b)'s possibly-live arm is built for. These read
    `/proc/self/stat` and the boot id, so they are Linux-hermetic under the
    package's `//go:build unix` tag — the precedent is `readPSS`, which
    reads `/proc/<pid>/smaps_rollup` from the same unix-tagged `launch.go`
    (PR #912 `launch.go:1,695-697`).
  - *KVM (`launch_teardown` extension, `//go:build microvm && unix`):* after
    `Launch`, all three pidfiles hold settled records (no intent lines
    left) and each names a live process whose starttime matches; after
    `Shutdown`, all three are gone.

### W2 — `ReapOrphans` + startup wiring (hermetic + KVM)

The §(b) reaper. Depends on W1's file format.

- **Interfaces:** produces
  - `func (m *MicroVMRuntime) lockRunRoot() (release func(), err error)` in
    `go/internal/runtime` (`//go:build unix`) — `syscall.Flock` with
    `LOCK_EX|LOCK_NB` on `<RunRoot>/microvm/.runner.lock` (created 0600,
    the dir created if absent), returning the fd-closing release. On
    `EWOULDBLOCK` it returns a named "another Runner owns this RunRoot"
    error and NOTHING is scanned. Held for the Runner's life: `main.go`
    acquires it once and defers the release, so the kernel drops it on any
    holder death including SIGKILL (§(b) step 0, OQ-8). This is a NAMED
    W2 deliverable, not an OQ-8 assumption — `ReapOrphans` must be
    unable to scan without it;
  - `func (m *MicroVMRuntime) ReapOrphans(ctx context.Context) error` in
    `go/internal/runtime` (`//go:build unix`): take the RunRoot lock FIRST
    (refused ⇒ return the error, scan nothing), then scan
    `<RunRoot>/microvm/*/`; skip table-live dirs (under `m.mu`); per §(b)
    kill VMM-first via SIGTERM → `reapGrace` poll → SIGKILL with boot-id
    short-circuit and starttime re-verification before every signal; `ctx`
    bounds the whole escalation loop (checked between polls; cancellation
    ⇒ named per-dir error, dir kept); signal errors differentiated —
    ESRCH benign (already gone, counts as confirmed-dead), EPERM a named
    per-dir error with no blind retry and the dir kept; `os.RemoveAll`
    fully-dead dirs; KEEP dirs holding a same-boot intent record (WARN +
    `orphans.reaped{outcome=possibly_live}`, no kill, no removal, no age
    gate); age-gate (`orphanDirGrace = time.Minute`) dirs with no readable
    pidfiles; join per-dir errors;
  - an injected probe seam
    `type reapProbes struct { readStat func(pid int) (uint64, error); signal func(pid int, sig syscall.Signal) error }`
    defaulted to the real proc/kill, overridden in hermetic tests. The lock
    is deliberately NOT seamed — a fake would defeat the one property the
    hermetic contention case must prove;
  - `main.go`: `type orphanReaper interface{ ReapOrphans(ctx context.Context) error }`
    probed after `verifyBackendPreflight` passes; a non-nil error is a
    `slog.Warn` with the joined detail, never an abort (OQ-5) — a refused
    lock included;
  - consumes W1's `readPidfile`/`pidRecord.alive`.
- **Test cycle:**
  - *Hermetic (fake probes, temp RunRoot):* a SECOND `lockRunRoot` over the
    same RunRoot is REFUSED and the second `ReapOrphans` performs no scan
    and issues no signal — asserted by planting a live-match pidfile that
    the first holder's presence must protect, and by the fake signal probe
    recording zero calls. (`flock` conflicts across distinct open file
    descriptions, including two opens of the same file within one process,
    so one test process can exercise the contention path directly.) Then
    the reap behaviors, all with the lock held: planted live-match pidfiles
    ⇒ signalled in VMM-first order and dir removed; starttime-mismatch
    pidfile ⇒ NO signal issued, dir removed; stale-boot-id pidfile ⇒ no
    proc read, no signal, dir removed; same-boot intent record ⇒ no signal,
    dir KEPT, `possibly_live` counted, WARN naming the child;
    previous-boot intent record ⇒ no signal, dir removed; fake signal
    returns ESRCH ⇒ treated as confirmed-gone, dir removed; fake signal
    returns EPERM ⇒ named error, dir kept, no SIGKILL follow-up; cancelled
    ctx mid-escalation ⇒ named error, dir kept; a process that never dies
    ⇒ dir kept, named error returned, second run retries; young no-pidfile
    dir kept, aged one removed; table-live dir untouched; concurrent
    `Create` during a scan never loses its dir.
  - *KVM — a REAL crash, not a dropped pointer:* spawn the first Runner as
    a CHILD PROCESS, let it boot a real VM and hold the RunRoot lock, then
    `SIGKILL` the child so the kernel drops its flock, and only then run
    `ReapOrphans` from the surviving test process ⇒ the lock is acquired,
    all three processes dead (proc-verified), dir gone. Constructing a
    fresh in-process runtime over the same RunRoot would NOT be a
    simulated crash — the first runtime still holds the lock, so the reap
    would be correctly refused and prove nothing about the reap. This case
    exercises the release-on-death property OQ-8(i) rests on and the reap
    together, which is why it replaces the drop-the-pointer form.

### W3 — `DeathWatch` + session monitor + `SessionDeadError` (hermetic + KVM)

The §(c) mid-session death path. Hermetic tier independent of W1/W2; KVM
tier depends on W1 (`Shutdown`'s cleanup set and the three-pidfile
assertion; see the Plan preamble).

- **Interfaces:** produces
  - `type DeathCause string` (`DeathVMM`, `DeathVirtiofsd`, `DeathPasst`)
    and `func (vm *VM) DeathWatch() <-chan DeathCause` in the microvm
    package — select over all three children's `exited` channels, one send
    per observed exit on a buffered (cap 3) channel, closed when every
    non-nil child has been reported, nil-device tolerant, no `Wait` calls,
    single-consumer by documented contract (§(c): the session monitor is
    the one receiver);
  - `func (vm *VM) VMMExited() bool` — nil-safe delegation to the VMM
    child's `hasExited()` (PR #912 `launch.go:96-107`; a channel read, not
    a zombie-blind signal-0 probe), for `Exec`'s in-flight wrap (§(c));
  - `type SessionDeadError struct { ID ContainerID; Cause string }` with
    `Error()` in `go/internal/runtime` (untagged file, beside
    `CommandError`);
  - `microvmSession.deadCause` + `microvmSession.epoch` +
    `microvmSession.tearingDown` (all under `m.mu`); `Stop`/`Remove` set
    `tearingDown` before calling `vm.Shutdown`; `Start` INCREMENTS `epoch`
    and clears `tearingDown` in the same locked critical section that
    stores the new VM (`microvm_lifecycle.go:401-415`), and hands the new
    epoch to that VM's monitor (§(c));
  - `Start` spawns the monitor goroutine after ownership transfer
    (`microvm_lifecycle.go:401-415`) carrying its epoch: it DISCARDS any
    death whose `session.epoch != myEpoch` (a superseded VM life) and any
    death arriving while `tearingDown`; on a fatal cause it records
    `deadCause`, runs `vm.Shutdown(context.WithoutCancel(…))`, logs +
    counts (W4 hooks) and returns; on `DeathPasst` it logs + counts
    `peer.deaths{process=passt}` and KEEPS RECEIVING, never setting
    `deadCause` (§(c));
  - `startedExec`/`Exec`/`ExecStreaming` refuse a dead session with
    `*SessionDeadError`. `Stop` and `Remove` do NOT — `Stop` on a dead
    session is a no-op success (the same arm as never-started,
    `microvm_lifecycle.go:615-617`) so `AgentRuntime.Teardown`'s
    Stop-then-Remove reaches `Remove` instead of early-returning at stage
    "stop" and leaking the entry and dir forever (`agent.go:216-222`;
    §(c)); `Exec`'s error arm re-checks `deadCause` and, when it is unset,
    probes `VMMExited()` before wrapping in-flight transport failures
    (`microvm_lifecycle.go:486-495`; §(c));
  - the `guestVM` seam gains `DeathWatch() <-chan microvm.DeathCause` and
    `VMMExited() bool` (`microvm_lifecycle.go:99-113` lists the seam's
    method-set rule).
- **Test cycle:**
  - *Hermetic (seam-faked VM):* fake fires a VMM death ⇒ monitor records
    cause, Shutdown called once, next Exec refuses with `errors.As`
    `*SessionDeadError{Cause:"vmm"}`; virtiofsd death ⇒ same with
    `"virtiofsd"`; **passt death ⇒ NO `deadCause`, NO Shutdown, the next
    Exec still succeeds, and the peer-death counter and INFO line are
    both emitted** (the non-fatal-but-observed arm, §(c)); deliberate
    Stop/Remove ⇒ monitor exits silently, no death recorded;
    Stop → Start → fake death ⇒ the second life's death IS recorded (the
    per-epoch reset proven); **STALE-MONITOR CASE: hold monitor#1's death
    event undelivered across a `Stop` and a `Start` that stores VM#2, then
    deliver it ⇒ the session is NOT marked dead, `deadCause` stays empty,
    the next Exec succeeds, and monitor#2 still detects VM#2's own death**
    (the epoch stamp's whole purpose — this is the case a boolean
    `tearingDown` fails, §(c));
    **STOP-AFTER-DEATH CASE: fake fires a VMM death, then `Stop` ⇒ nil,
    then `Remove` ⇒ nil, and the session-table entry is gone** (a dead
    session must remain tearable-down, §(c)); in-flight exec failing while
    `deadCause` is still unset but the fake's `VMMExited()` reports true ⇒
    wrapped `*SessionDeadError{Cause:"vmm"}`; a second receive on a drained
    `DeathWatch` channel observes only the close (the single-consumer
    contract's failure mode pinned, never relied on); Remove after death ⇒
    nil (idempotent), table entry gone.
  - *KVM:* boot a session, `SIGKILL` the VMM pid mid-exec ⇒ the in-flight
    `Exec` fails with `*SessionDeadError`, peers reaped (proc-verified),
    sockets/pidfiles removed, `Stop` returns nil and `Remove` returns nil;
    kill virtiofsd ⇒ session torn down, the workspace share content intact
    on the host — the parent's V7 test-cycle rows verbatim
    (microvm-runner.md:595-598); `SIGKILL` passt ⇒ the session stays alive
    and a subsequent `Exec` still succeeds over vsock (egress lost, control
    plane intact), with the peer-death counter incremented.

### W4 — backend metrics + INFO transition logs (hermetic)

The §(d) instrument set inside the microVM backend.

- **Interfaces:** produces
  - `const microvmInstrumentationScope = "github.com/RigelBuild/compass/go/internal/runtime"`;
  - `type microvmMetrics struct { bootDuration metric.Float64Histogram; teardowns metric.Int64Counter; vsockRPCDuration metric.Float64Histogram; canaryRuns metric.Int64Counter; orphansReaped metric.Int64Counter; peerDeaths metric.Int64Counter; guestPSS metric.Int64ObservableGauge; quotaUsedRatio metric.Float64ObservableGauge }`
    built in `NewMicroVMRuntime` from the global meter, each instrument nil
    plus one `slog.Warn` on creation error, every record nil-guarded;
  - `lastQuota QuotaReading` on `MicroVMRuntime` — the quota gauge's ONLY
    source, written under `m.mu` by the preflight's `verifyQuota` where it
    already holds the reading (PR #912 `microvm_preflight.go:174,189-194`
    is where the reading is bound and logged today, and dropped on return;
    the struct carries no quota field at all, PR #912
    `microvm.go:96-111`). Preflight-time snapshot, never refreshed; the
    callback does NO `statfs` and therefore needs no error posture (§(d));
  - record points: `Start` (boot duration + INFO `microvm session booted`),
    the W3 monitor (`teardowns{cause}` + INFO `microvm session died` on a
    fatal cause; `peer.deaths{process=passt}` + INFO `microvm session peer
    died` on the non-fatal arm), `Stop`/`Remove` (`teardowns` + INFO),
    `Exec`/`awaitHealthy`/Provision/`stopGuest`
    (`vsock.rpc.duration{rpc,outcome}`), `BootCanary`
    (`canary.runs{outcome}`), `ReapOrphans`
    (`orphans.reaped{process,outcome}`, including the `possibly_live`
    series for §(b)'s dangling-intent arm);
  - observable callbacks registered at construction: `guestPSS` snapshots
    the live VM handles under `m.mu`, RELEASES the lock, then sums
    `vm.PSS()` per process kind outside it — never holding `m.mu` across a
    `smaps_rollup` read (§(d); best-effort, errors dropped as `PSS` itself
    does, PR #912 `launch.go:678-683`); `quotaUsedRatio` reads `lastQuota`
    under `m.mu` and emits a point ONLY when that reading's `Active()` is
    true — false for the zero value by construction, since `Active`
    requires positive limit and filesystem totals (PR #912
    `microvm_quota.go:117-125`), so an unset snapshot is silent with no
    extra flag;
  - consumes W2/W3 hooks; no new public API.
- **Test cycle (hermetic):** `sdkmetric.NewManualReader` +
  `NewMeterProvider` installed as the global BEFORE `NewMicroVMRuntime`
  (the `trace_test.go:209-220` harness); seam-faked boot ⇒
  `compass.microvm.boot.duration` has one point with `outcome=ok` and no
  other attribute; faked death ⇒ `teardowns{cause=vmm_death}` = 1; faked
  passt death ⇒ `peer.deaths{process=passt}` = 1 and `teardowns` unchanged;
  every data point asserted to carry ONLY enum attributes (the
  `dispatchedCounts`-style attribute audit, `trace_test.go:253-256`); a
  no-op global meter ⇒ construction succeeds, records are skipped, nothing
  panics; quota gauge with an unset `lastQuota` ⇒ NO point collected, with
  an `Active()` reading planted ⇒ exactly one point carrying its
  `UsedRatio()`.

  The PSS-callback lock test is written SELF-VERIFYING, because the naive
  form passes vacuously: with no session in `m.sessions` or a nil
  `guestPSS`, the callback returns before it ever reaches `PSS()` and any
  concurrent `m.mu` acquisition proceeds trivially. So the fixture
  registers a session carrying a seam-faked VM whose `PSS()` signals an
  entry channel and then blocks, and the test asserts BOTH that the entry
  signal was observed (proving the callback actually reached `PSS`) and
  that a concurrent `m.mu` acquisition completes while it is still
  blocked. A `Collect` on the ManualReader drives the callback.

### W5 — startup wiring: `setupOtel` reorder + Runner-host session counter (hermetic)

The §(d) main.go ordering fix and the `backend`-labelled session metric.

- **Interfaces:** produces
  - `main.go`: the startup sequence extracted into a testable unit

    ```go
    type startupHooks struct {
        setupOtel func(ctx context.Context) (func(), error)
        selectEngine func() (runtime.ContainerRuntime, error)
        preflight func(ctx context.Context, engine runtime.ContainerRuntime) error
        reap func(ctx context.Context, engine runtime.ContainerRuntime) error
    }

    func startup(ctx context.Context, h startupHooks) (runtime.ContainerRuntime, func(), error)
    ```

    which calls them in the order `setupOtel` → `selectEngine` →
    `preflight` → `reap` and returns the engine plus the OTel shutdown.
    `run()` builds the real hooks and calls it, replacing the inline
    sequence at `main.go:106-112` and the `setupOtel` call at
    `main.go:167-172`. `setupOtel` must precede `selectEngine`, not merely
    the preflight, because instrument construction happens inside
    `NewMicroVMRuntime` down `selectEngine`'s call path
    (`main.go:332-345` → `microvm.go:117-122`); §(d) has the argument. The
    `orphanReaper` probe (W2) is the `reap` hook;
  - `go/internal/runtime`: `func (m *MicroVMRuntime) BackendName() string { return "microvm" }`
    and `func (p *PodmanCLI) BackendName() string { return "podman" }` —
    NOT on the frozen interface;
  - `go/internal/runner/host.go`: `agentHost` resolves the backend name
    once at construction via `interface{ BackendName() string }` (unknown ⇒
    `"unknown"`, never a per-call probe) and mints
    `compass.runner.session.starts` (Int64Counter,
    `backend` + `outcome` attributes) recorded in `Start`
    (`host.go:306-499`; the two record points are the error return at
    `host.go:441-444` and the success path from `host.go:446` to the
    `return sessionID, nil` at `host.go:498`), nil-on-error posture as
    everywhere;
  - consumes W4's conventions; no proto or interface change.
- **Test cycle (hermetic):** ManualReader over a fake-engine `agentHost`
  `Start` ⇒ one `session.starts` point with `backend` set from the probe
  and no other attribute beyond the enum pair; an engine without the probe
  ⇒ `backend="unknown"`, no error; `startup` called with hooks that append
  their own name to a shared slice ⇒ the slice is exactly
  `["setupOtel", "selectEngine", "preflight", "reap"]`, and a hook
  returning an error short-circuits the rest.

  This replaces an earlier claim that the ordering could be pinned "via
  the existing startup-dispatch test seam, `main.go` fake engines". It
  cannot: that harness calls `verifyBackendPreflight` directly and never
  invokes `run()` (`main_test.go:167,178,193,205,214,227,238,258`; no
  `run(` call anywhere in the file), so it can observe nothing about
  `setupOtel`'s position and a test written against it would pass whether
  the reorder happened or not. The extracted `startup` unit exists
  precisely so the ordering has a test that can FAIL on the bug — the
  reorder is OQ-3's sole delivery mechanism, so review-only would leave it
  unguarded.

## Tasks

- [ ] W1 — host-written `vmm.pid`/`virtiofsd.pid`/`passt.pid` with
      `<pid> <starttime> <bootid>` identity and atomic temp-then-rename
      writes; retire passt `--pid`; remove-on-Shutdown, boot fails on
      write failure
- [ ] W1 — the two-step write that closes the spawn→write window:
      `writePidIntent` (`intent <bootid>`) BEFORE each `startChild`,
      superseded by the settled record after it, and `readPidfile`
      reporting the intent form as neither-alive-nor-dead
      (`errPidUnknown`)
- [ ] W2 — `(*MicroVMRuntime) lockRunRoot()`: `LOCK_EX|LOCK_NB` flock on
      `<RunRoot>/microvm/.runner.lock`, acquired before ANY scan and held
      for the Runner's life, refuse-not-wait on contention; a second
      acquisition over the same RunRoot is refused and scans nothing
- [ ] W2 — `(*MicroVMRuntime) ReapOrphans(ctx) error`: take the RunRoot
      lock first, scan runtime dirs, boot-id short-circuit +
      starttime-verified SIGTERM→SIGKILL bounded by ctx, ESRCH/EPERM
      differentiated, VMM first, remove fully-dead dirs, KEEP
      same-boot-intent dirs (WARN + `possibly_live` count), age-gate empty
      dirs, skip table-live sessions; `main.go` probe wiring,
      warn-never-abort
- [ ] W3 — `VM.DeathWatch()` (single-consumer, all three children,
      one send per exit) + `VM.VMMExited()` over the reaper channels;
      per-session monitor stamped with the session's `epoch` — a
      superseded VM life's death event is DISCARDED — plus `tearingDown`
      for the current epoch's deliberate teardown;
      `*runtime.SessionDeadError` on refused and in-flight execs
- [ ] W3 — teardown never refuses: `Stop` on a dead session is a no-op
      success (only `Exec`/`ExecStreaming` refuse), so
      `AgentRuntime.Teardown`'s Stop-then-Remove reaches `Remove`;
      idempotent Remove proven on a dead VM and the table entry gone
- [ ] W3 — passt death observed but never fatal: `DeathPasst` arm logging
      `microvm session peer died` and counting
      `peer.deaths{process=passt}` with no `deadCause` and no teardown
- [ ] W4 — `microvmMetrics` (boot/teardown/vsock-RPC/canary/orphans/
      peer-deaths/PSS/quota instruments, warn-and-disable), INFO
      transition logs with session id + timings
- [ ] W4 — `lastQuota QuotaReading` on `MicroVMRuntime`, written under
      `m.mu` by the preflight's `verifyQuota` and read by the
      `quota.used.ratio` callback: preflight-time snapshot, never
      refreshed, no point emitted when inactive or unset
- [ ] W5 — `startup(ctx, hooks)` extracted from `run()` so the order
      `setupOtel` → `selectEngine` → `preflight` → `reap` is pinned by a
      call-order test; `setupOtel` must precede `selectEngine` (not merely
      the preflight), because instruments are built inside
      `NewMicroVMRuntime`
- [ ] W5 — `BackendName()` probes;
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
  would be a reader-less copy of a constant. Two additions the sketch does
  not name: each pidfile can transiently hold §(a)'s pre-spawn `intent
  <bootid>` line instead of a pid (the same three paths, one extra record
  form — not a fourth file), and `<RunRoot>/microvm/.runner.lock` sits
  beside the per-session dirs as the Runner-scoped `flock` target (§(b)
  step 0). **Recommendation:** ratify all of it as (benign,
  naming/dead-file/mechanism) deviations from a sketch, not from a frozen
  behavior.
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
- **OQ-3 (non-load-bearing) — `setupOtel` moves ahead of `selectEngine`,
  not merely ahead of the backend preflight.** Today `selectEngine` is at
  `main.go:106`, the preflight (and its canary) at `main.go:110-112`, and
  the meter provider installs only at `main.go:167-172`, so preflight-time
  metrics would hit the no-op global and vanish. `selectEngine` is the
  right target because instrument construction happens on its call path
  (`main.go:332-345` → `microvm.go:117-122` → `NewMicroVMRuntime`), and the
  preflight dispatches on the engine so nothing can sit between them. The
  reorder is safe: `setupOtel` is env-only and its disabled path is a no-op
  (`provider.go`). Worth noting what is NOT at risk: the global meter's
  `setDelegate` re-creates and re-delegates both instruments and registered
  callbacks when a real provider installs
  (`go.opentelemetry.io/otel@v1.46.0/internal/global/meter.go:126-147`,
  the pinned version per `go/go.mod:35`), so instruments built pre-install
  keep working — only measurements RECORDED before the install are lost,
  which is exactly the canary's points. **Recommendation:** reorder ahead
  of `selectEngine`; keep instrument creation at construction time, with no
  lazy-on-first-record path (§(d) states the single posture).
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
  and the `orphans.reaped` counter making silent rot visible. A REFUSED
  RunRoot lock (§(b) step 0) takes the same arm: a Runner that cannot
  acquire the lock warns and starts, having scanned nothing — the wrong
  reap is what the lock prevents, and refusing startup over a
  still-draining predecessor would be the box outage this option rejects.
- **OQ-6 (LOAD-BEARING) — the net backend leaves the parent's supervised
  set; this rules on frozen text.** State the divergence plainly, because
  it is larger than a fatality question: the parent's §(f) preamble
  supervises the per-session process set including the "net backend"
  (microvm-runner.md:240-241), and V7 removes passt from session-FATAL
  handling entirely. That is the same class of change as OQ-10's dropping
  the guest — the other member of that one frozen sentence — which this
  record grades load-bearing and sends to the human; grading passt lower
  was an inconsistency, and it is corrected here.

  On the merits, non-fatality is right: the parent's failure matrix names
  VMM death and virtiofsd death only (microvm-runner.md:244-253), and passt
  dying leaves the vsock control plane (served by the VMM over AF_UNIX)
  intact, so Stop/Remove still work while the guest loses egress. But
  "non-fatal" must not silently become "unobserved": with no host-side
  detection there is no death signal, no metric series, and no log line, so
  an operator gets nothing when a session loses egress and OQ-6's own
  closing rationale ("the session lifecycle handles it") would rest on no
  mechanism at all. So the body ships observation WITHOUT fatality
  regardless of the ruling — a `DeathPasst` arm on `DeathWatch` that logs
  `microvm session peer died` and counts
  `compass.microvm.peer.deaths{process=passt}` and does NOT tear the
  session down (§(c), §(d)).

  Options: (i) matrix-literal non-fatal, WITH the observation arm (what
  the body designs against); (ii) extend the frozen matrix and treat passt
  as fatal too (symmetric with virtiofsd, but widens frozen text and
  converts a degraded session into a killed one); (iii) non-fatal and
  unobserved (what an earlier draft implied — rejected here as removing
  passt from the supervised set outright). **Recommendation:** (i); revisit
  fatality with V8 evidence if egress-less zombie sessions show up in
  practice.
- **OQ-7 (load-bearing) — what the reaper does with an unidentifiable dir,
  ruled together with §(a)'s write lifecycle.** Two distinct states get
  confused here, and the ruling needs them separated:

  1. **A dir with no readable pidfiles** — a crashed pre-spawn `Create`
     (safe to remove), a concurrent `Create`'s pre-insert window (must not
     remove, §(b) step 1), or content destroyed by external tampering.
  2. **A dir holding a same-boot INTENT record** (§(a)) — `launch` crashed
     between writing the intent and settling the pid, so a child may be
     running RIGHT NOW with no pid on disk.

  The correction to an earlier draft of this question: the atomic
  temp-write-then-rename closes the TORN-WRITE path ONLY. It does not close
  the spawn→write window, and that window is an ORGANIC under-recording
  path — spawn order puts the VMM last (`launch.go:222`), so a crash
  between its spawn and its record leaves two parseable pidfiles naming two
  dead helpers and no record of a live orphan VMM, whereupon a
  remove-if-all-recorded-dead reaper erases the only evidence of the leak.
  Claiming atomicity made that state "reachable only via external
  tampering" conflated the two paths and made removal look safer than it
  was.

  So the body CLOSES the window rather than reasoning around it (§(a)'s
  pre-spawn intent record), which makes state 2 both detectable and
  attributable to a named child — and state 2 is never removed: WARN, count
  `orphans.reaped{outcome=possibly_live}`, keep the dir, no age gate. With
  the window closed, state 1 is genuinely only pre-spawn-`Create`, the
  `Create` pre-insert window, or tampering, and removal after the grace is
  defensible again for the reason the earlier draft gave — the reasoning
  just had to be earned rather than assumed.

  The mtime age gate (`orphanDirGrace = 1m`) stays for state 1 regardless:
  it separates the `Create` pre-insert window (microseconds of lock work)
  from everything else by orders of magnitude and bounds cleanup latency.
  Options for state 1: (i) remove after the grace; (ii) quarantine-and-WARN
  (leave or rename aside, log loudly, never auto-remove). Options for state
  2: (a) keep-and-WARN forever (what the body ships); (b) age-gate it into
  removal like state 1 — rejected, since it re-opens exactly the leak the
  intent record exists to prevent. **Recommendation:** (i) + (a), and
  ratify 1m as the named constant. Rule the write lifecycle (intent record
  included) and the removal arms together.
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
  rebuilds half of `flock` by hand.

  **This question rules on WHICH mechanism; it does not supply the
  mechanism.** The body SHIPS (i) as a named W2 deliverable —
  `lockRunRoot` in W2's Interfaces, its own `## Tasks` bullet, and a
  hermetic case asserting a second acquisition over the same RunRoot is
  refused and scans nothing (§(b) step 0). An earlier draft left the
  defense as this question's recommendation only, which meant an executor
  implementing W1-W5 verbatim would have shipped `ReapOrphans` with zero
  cross-process exclusion — precisely the live-session-killing reaper this
  question exists to prevent. A safety property that lives only in an Open
  Question is not a delivered defense. If the human rules (ii) or (iii),
  W2's lock deliverable and §(b) step 1's clause change with the ruling.

  One sub-fork rides along: option (i)'s wording says a second Runner
  "refuses startup", while OQ-5 rules a reap failure a WARN and never an
  abort — and a refused lock surfaces as a `ReapOrphans` error. The body
  takes OQ-5's arm (WARN, continue, scan nothing), with the reasoning at
  §(b) step 0. Ruling (i) with refuse-startup semantics is coherent but
  strictly stronger, so rule OQ-5 and OQ-8 in one pass.
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
  consumer meters without ids, `dispatch.go:379-383`, while carrying them
  in the adjacent WARN, `dispatch.go:392-393`);
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
