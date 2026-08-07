# Cross-process teardown for the embedded native-app stack

Status: Draft

Refines **DL-108** (embedded lifecycle: Go stack supervisor, linger-by-default,
lockfiled attach — [native app §A3](../compass-native-app/design.md#a3--embedded-lifecycle-a-go-stack-supervisor-spawned-by-the-shell)).
DL-108 stays Active; this record decides ONE thing DL-108 left implicit and the
implementation left unrealized: **how `compass-stack down`, invoked as a fresh
process, tears down a stack that a prior, now-exited `compass-stack up`
spawned.** It realizes the frozen native-app record's teardown claim
(`compass-native-app/design.md:206` — "runs `compass-stack down`, which SIGTERMs
the tree and waits out the server's drain") **for the three supervised stack
children** (postgres, compass-server, compass-runner), which is currently false
across the process boundary. Live agent *containers* the runner hosts are NOT in
that tree — conmon double-forks them out of the runner's process group — and are
scoped OUT of this mechanism; see **Open Question 0** (parked for Matt).

Trackers: parent **SEA-1685** (embedded native app T4); the teardown fork is
**SEA-1880**; the container-teardown companion gap is **SEA-1884**.

## Problem / Intent

`compass-stack down` invoked as a FRESH process cannot stop a stack that a
prior, now-exited `compass-stack up` spawned. `up` fire-and-returns, the spawner
exits, its children reparent to init; no later process holds the in-memory child
handles, so a fresh `down` is a silent no-op. This:

1. makes the merged T4.2 embedded **"Quit and stop stack"** action a silent
   no-op that leaves the stack running,
2. blocks the **T4.3 (SEA-1685) e2e gate** from proving teardown or
   process-safely cleaning up the stack it starts, and
3. contradicts the frozen design's `down`-SIGTERMs-the-tree claim.

**Scope of "the tree".** This mechanism signals the three supervised stack
children — postgres, compass-server, compass-runner — each a process-group
leader (`process.go:67-69`). It does NOT reach the agent *containers* the runner
hosts: conmon double-forks to daemonize, so a container escapes the runner's
process group, and the runner does not stop its containers on its own shutdown
(`internal/runner/host.go:204-217`: `Close` drops only the per-container socket
*listeners*; "every container lives until the Runner process ends" and teardown
otherwise lives solely on the `Remove` RPC path). Stopping live containers is a
distinct, runner-lane concern — **Open Question 0** and **SEA-1884**.

### Evidence (all re-verified in the current tree, this session)

**`down` reaches the stack only via `Up`'s attach path; there is no
attach-then-signal constructor.** `go/cmd/compass-stack/main.go:267-270`:

> ```go
> // runDown attaches to the live stack (stack.Up attaches if a server is already
> // answering) and stops its children, releasing the lock. There is no separate
> // public "attach then Down" constructor — Down is a method on the *Stack that Up
> // returns — so down goes through Up's attach path, then calls Down.
> ```

**`up` is fire-and-return.** `go/cmd/compass-stack/main.go:235-238`:

> ```go
> // runUp resolves config, wires deps, and brings the stack up. stack.Up returns
> // once the stack is Ready (or attached); the children keep running, so up prints
> // status and returns rather than blocking (lingering vs quit is Config.Linger's
> // concern).
> ```

**`Down` only signals in-memory handles; an attached stack owns none.**
`go/internal/stack/stack.go:141-142`:

> ```go
> // Down stops the stack's children in reverse start order and releases the lock.
> // An attached stack owns no children, so Down only releases (a no-op lock, since
> ```

and `drainChildren` iterates only the `Process` handles this process holds
(`go/internal/stack/stack.go:281-294`):

> ```go
> func (s *Stack) drainChildren(ctx context.Context) error {
>     var errs error
>     for _, c := range []struct {
>         name string
>         p    Process
>     }{
>         {"compass-runner", s.runner},
>         {"compass-server", s.server},
>         {"postgres", s.pg},
>     } {
>         if c.p == nil {
>             continue
>         }
>         if err := c.p.Signal(SignalTerm); err != nil {
> ```

**The lockfile records only the HOLDER's pid** — for staleness, never the
children's pids/pgids — so a fresh `down` has no on-disk source of anything to
signal. `go/internal/stack/lockfile.go:29-33`:

> ```go
> // acquireLock takes the state-dir lock via O_CREATE|O_EXCL. On contention it
> // distinguishes a live holder (another Up in flight — return errLockHeld) from a
> // stale lockfile whose writer is gone: a stale file is removed and the acquire
> // retried once, so a crashed Up does not wedge the state dir forever. The
> // lockfile records the holder's pid so staleness is decidable.
> ```

**Each child already leads its own process group** — the primitive Option A
signals. `go/internal/stack/adapters/process.go:67-69`:

> ```go
>     // Own process group: Signal/escalation target the group (negative PID), so a
>     // child that forks its own workers takes them down too — no orphan escapes.
>     cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
> ```

The "no orphan escapes" comment holds for a child's *in-group* forks, but two
process classes leave the group by design and this mechanism does not reach
them: setsid'd postgres backends (postgres postmaster sets each backend up as
its own group leader — benign here, they self-exit on postmaster death so
teardown still completes) and — load-bearingly — the agent *containers* the
runner hosts (conmon double-forks, so a container is not in the runner's process
group; **Open Question 0**).

The pid doubles as the pgid (`process.go:78-79`):

> ```go
> // process is a handle to a started child. It holds the *exec.Cmd; the child's
> // PID doubles as its process-group ID (set via Setpgid at Start).
> ```

**T2's own test knew this**: teardown works only IN-PROCESS, from the spawner's
handle. `go/cmd/compass-stack/integration_podman_test.go:288-289`:

> ```go
>     // children). Down on the attached stack is a no-op; Down on the spawner
>     // tears the real stack down.
> ```

**The T4.2 quit path builds on the false claim.**
`go/cmd/compass-app/lifecycle.go:13-15` (file comment):

> ```go
> //   - Explicit "Quit and stop stack": a distinct user action that runs
> //     `compass-stack down` (attach → SIGTERM the child tree → wait the server
> //     drain → release the lock) and THEN quits the app.
> ```

and `lifecycle.go:72` runs it: `if err := c.stackDown(downCtx,
stackDownArgs(c.params)); err != nil {`. Across the process boundary the
"SIGTERM the child tree" step signals nothing.

**The runner has no listening socket and exits when its server link drops.**
`go/cmd/compass-runner/main.go:4-5` ("It dials OUT to the Server over gRPC …");
`go/internal/runner/run.go:108-119`:

> ```go
>     // The per-container agent sockets the host serves live until the Runner
>     // process ends (no per-container Deprovision RPC in the single-Runner MVP);
>     // close them all on shutdown, draining any in-flight call.
>     …
>     // The Sessions loop blocks until the stream ends (ctx cancel = clean
>     // shutdown; any other end is the link dropping). …
>     if err := link.RunSessions(ctx, host, log); err != nil {
> ```

So the runner cannot be confirmed dead by a socket probe (it has none), and
killing the *server* first makes a surviving runner exit on its own when its
link drops — both facts the teardown sequence below relies on.

**The frozen record's claim this realizes.**
`docs/designs/product/compass-native-app/design.md:204-206`:

> ```text
> On app quit, per DL-108: the stack lingers by default;
> quit closes the window, an explicit "Quit and stop stack" action runs
> `compass-stack down`, which SIGTERMs the tree and waits out the server's drain.
> ```

## Approach — Option A: persist child pgids at `up`; `down` signals the persisted groups

**Persist the child process-group IDs (with a per-child identity token) at
`up`; a fresh `down` reads them, SIGTERMs each pgid (bounded escalation to
SIGKILL), then confirms teardown.** Children already run as their own
process-group leaders (`Setpgid: true`, pid == pgid), so the pgids are available
at spawn for free. This is the standard daemon-pidfile pattern, self-contained
in this lane (`go/internal/stack` + `compass-stack` + `compass-app`), and it
makes the frozen "SIGTERM the tree" sentence literally true **for the three
supervised children** (containers scoped out — Open Question 0).

### The pgid record file

- **Location**: `<StateDir>/stack.pgids` — beside `stack.lock` /
  `stack.lock.guard` (the state dir is already "the O_EXCL lockfile, the private
  postgres data dir, and the TLS anchor" home, `go/internal/stack/config.go:17-19`).
- **Format**: a version/writer header line, then one line per child,
  `<component> <pgid> <starttime>`, in **start order** (`postgres`,
  `compass-server`, `compass-runner`). `<starttime>` is the group leader's
  process start time (Linux: field 22 of `/proc/<pid>/stat`, read behind a small
  package-internal seam so the file need not be Linux-only in principle). The
  start time is the **identity token**: it turns the down-side check from "does a
  group with this pgid exist" (which a recycled pid passes falsely) into "does a
  group with this pgid AND this leader start time exist", reducing the
  pid-recycling window to a same-pid-same-starttime collision (effectively
  zero). The header still carries the writer pid + stack version, but ONLY as a
  format/version guard and provenance — NOT as a staleness signal (see below):
  because `up` always exits after a successful spawn (`main.go:235-238`), the
  writer pid is dead in every linger teardown, so it discriminates nothing about
  whether the *children* are alive. Plain text, trailing newline, 0600.
- **Write timing**: rewritten **atomically (temp + rename in the state dir)
  after each successful child spawn** in `spawnChain`
  (`go/internal/stack/stack.go:171-228`), i.e. the file always reflects the set
  of children actually started so far. Reusing the lockfile's
  publish-complete-content discipline (`lockfile.go:111-117` publishes via
  temp + link so "a concurrent acquirer never observes a created-but-empty
  file") means a reader never sees a torn record.
- **Removal**: deleted by the code path that releases the lock at the end of a
  **fully** successful `Down` — teardown that tore every recorded group down
  leaves no pgid file behind. A **partial** teardown does NOT remove it (see
  the partial-failure policy below).
- **Attach path writes nothing**: an attached `Stack` spawned nothing and owns
  nothing; the file belongs to the spawner alone.

### What `down` does

1. Resolve config. **Refuse to race a live `up`**: before touching the pgid
   file, apply the lockfile's own live-holder test — `lockHolderLive` on
   `stack.lock` (`lockfile.go:62-66`). A live holder means an `up` is mid-flight
   (it may not have written the runner line yet, and may be parked in
   `waitReady`); `down` returns a legible "a stack is starting; retry once it is
   up" rather than signaling a half-spawned set. The guard flock alone does NOT
   cover this — `acquireLock` releases the guard before `spawnChain`
   (`lockfile.go:41-48`, `defer guard.release()`), so the spawn runs outside any
   lock a `down` could contend on; the lockfile-holder check is the real
   interlock.
2. Read `<StateDir>/stack.pgids`. Absent file + no answering socket → nothing to
   do, report "no stack". Absent file + answering socket → legible error (a
   stack is live but this build has no teardown record — see Open Question 1).
3. **Identity + liveness check per recorded group** (not bare existence): a
   recorded pgid is a live teardown target only if a group with that pgid exists
   (`kill(-pgid, 0)` ≠ ESRCH) AND its leader's current start time matches the
   recorded token. Mismatch or ESRCH → the original group is gone (exited, or
   the pid was recycled by an unrelated process); skip it, never an error, never
   signal it.
4. Signal each **live, identity-matched** pgid `SIGTERM`, in **reverse start
   order** (runner → server → postgres) — the same order `drainChildren` uses
   (`stack.go:287-289`), so the server drains before its database goes away.
   Signaling the server also makes a surviving runner exit on its own
   (its link drops — `run.go:115-119`), a belt-and-suspenders alongside directly
   signaling the runner group.
5. Per-pgid bounded wait for group death (poll `kill(-pgid, 0)` re-checking
   identity until ESRCH), then **escalate to `SIGKILL` on the group** after the
   per-child budget — the exact escalation shape the in-process path already has
   (`process.go:157`: `syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)`).
6. **Confirm teardown per component, by the channel each actually has**:
   - **compass-server** and **postgres** — socket quiescence: the server UDS
     stops answering `GetServerInfo` and the postgres socket stops accepting, the
     same probes `Health`/`waitPostgres` already use (`stack.go:157`,
     `stack.go:264`). Signal delivery is not proof of death; the sockets going
     dark is.
   - **compass-runner** — it has NO listening socket (`main.go:4-5`), so its only
     confirmation channel is its group going ESRCH (step 5's poll). The record
     names this explicitly: the runner is confirmed by group-ESRCH, never by a
     socket.
   - **Post-SIGKILL zombie window**: `kill(-pgid, 0)` still succeeds while the
     group leader is a zombie awaiting reap by init/subreaper, so a group can be
     genuinely killed yet non-ESRCH for a short window. `down` treats
     `SIGKILL-sent` + `socket dark (server/pg)` — or, for the runner, the group
     going ESRCH — as a **successful** teardown of that component. Because
     `SIGKILL` is unblockable, a runner group still non-ESRCH *after* the group
     SIGKILL can only be zombies awaiting init's reap (a guaranteed-terminal
     state), so it is treated as success; a live-and-non-zombie group can occur
     only *before* SIGKILL escalation, never after. `down` does NOT report
     failure merely because a just-SIGKILLed group has not yet been reaped. Only
     a group still *answering* (server/pg socket live) or still
     *live-and-non-zombie* at budget expiry is a real teardown failure.
7. **Removal / partial-failure policy**: on a fully successful teardown (every
   recorded group confirmed gone), remove the pgid file and clear the lockfile.
   On a **partial** teardown (some groups torn, at least one still live at budget
   expiry), do NOT remove the pgid file — removing it would orphan the survivors
   with no on-disk record to retry against. Instead rewrite it to the surviving
   set (same atomic temp+rename) and report which components survived, so a
   retried `down` (or a human) can finish the job. In the cross-process path the
   lockfile holder (the `up` process) is already dead, so the survivor-rewritten
   pgid file — not the lockfile — is the retry record; leaving the lockfile is
   harmless but is not itself an interlock.

### Process-safety invariants (non-negotiable)

- **Only the exact persisted pgids are ever signaled.** Never a pattern kill,
  never a scan of the process table, never a pgid not read from this stack's
  own state-dir file.
- **Identity is verified immediately before each signal** (pgid group exists AND
  leader start time matches the recorded token). A recorded pgid whose group is
  gone, or whose leader start time no longer matches, is treated as already-gone
  and skipped — never signaled. This start-time gate, not a bare ESRCH check, is
  what closes the pid-recycling window: without it, a recycled pid reused by an
  unrelated process would be SIGKILLed. (The blast radius of a wrong
  staleness call is precisely why the identity token is mandatory: a wrong
  lockfile-staleness call costs a spurious lock reclaim; a wrong pgid-staleness
  call SIGKILLs an innocent process group — categorically worse, so the pgid
  path must be strictly more careful than the lockfile, not "the same exposure".)
- **The verify→signal gap is an irreducible residual**, inherent to pgid
  signaling (Linux has no pidfd for a process group, so no atomic
  check-and-signal): the identity check and the `SIGTERM`/`SIGKILL` are separate
  syscalls. The start-time token narrows the residual to a same-pid *and*
  same-start-time collision inside that microsecond window — the accepted
  irreducible floor, not a closable hole.
- **Bounded escalation**: SIGTERM, per-child drain budget, then group SIGKILL —
  never an unbounded wait, never a first-resort SIGKILL.

### Crash / partial-write / concurrency handling

- **Crash between a child spawning and the file landing**: the file is
  rewritten after *each* spawn, so the window is one child wide; the atomic
  rename means the file on disk is always a complete earlier prefix of the
  start sequence. A crash inside that window leaks at most the newest child —
  the same exposure `spawnChain`'s own mid-sequence drain has today. The newest
  child is most often the runner (spawned last, `stack.go:222-227`), which has
  no socket; its recovery is that it exits on its own when its server link
  drops (`run.go:115-119`) once the server is later torn down. A leaked
  server/postgres is additionally findable via its socket for a manual stop.
- **Concurrent `down` invocations**: serialize the read→consume-decision under
  the existing `stack.lock.guard` advisory flock (`lockfile.go:83-87` — "blocking
  until any concurrent acquirer's decision completes"), so two `down`s cannot
  both consume the file or double-signal; the loser re-reads, finds no
  file/dead groups, and no-ops. The guard is held ONLY for the read + decision,
  and **released before** the long signal→wait→SIGKILL sequence, so a `down`
  never blocks a concurrent `up`'s `acquireGuard` for the full (up to ~55s)
  drain budget — matching how `acquireLock` scopes the guard to the decision,
  not the spawn.
- **`down` racing a live `up`**: the guard flock does NOT serialize these — the
  guard is released before `spawnChain` runs (`lockfile.go:41-48`), so a `down`
  holding the guard could still run concurrently with a live `up`'s spawn and
  signal a half-started set. The real interlock is step 1's `lockHolderLive`
  check on `stack.lock`: a live lock holder means an `up` is in flight, and
  `down` refuses (retry once up) rather than tearing down a stack that is still
  being built.

## Alternatives considered

### B — Control-RPC shutdown (rejected for this fork)

Add a `Shutdown` RPC to `compass-server`; `down` dials the UDS and asks the
server to tear down runner + itself + postgres. Cleaner on the surface (no
pidfile), but:

- **Cross-lane cost, named explicitly**: it needs a `compass.v1` proto addition,
  and the proto surface is **compass-repo's sole-writer lane** — this fork
  cannot land self-contained; it queues behind another lane's writer.
- **It reproduces the same problem one level down**: postgres is NOT the
  server's child (all three are siblings spawned by `up`,
  `stack.go:174/197/222`), so the server would have to stop a process it didn't
  spawn — needing exactly the pgid-persistence mechanism A builds, plus an RPC.
- A half-dead stack (server crashed, postgres lingering) has no one to answer
  the RPC; A handles that case for free via the staleness skip.

Kept as a **future additive** with one advantage A lacks: B is the only option
that could gracefully drain live agent *sessions* before teardown (the server
can instruct the runner over the existing link to stop its containers). That
advantage becomes load-bearing if Open Question 0 resolves to "stop stack must
stop containers" — a server-driven drain is a clean way to reach the containers
A cannot signal. Not the mechanism of record either way.

### C — App supervises `up` as a long-lived foreground child (rejected)

Add a foreground/blocking mode to `compass-stack up` so the app holds the
process and SIGTERMs its group on quit. Rejected because it **breaks two frozen
contracts**:

- T2's fire-and-return CLI contract (`main.go:235-238`, quoted above) — a
  foregrounded `up` is a different program.
- DL-108's **linger-by-default / relaunch-re-attaches** model: a stack tied to
  the app's process lifetime is not detached, so plain quit would kill it and
  "relaunch re-attaches to a lingering stack" stops existing. The whole point
  of DL-108's linger posture (`lifecycle.go:6-12`) is that the app holds no
  supervisory handle.

### D — cgroup / systemd-transient-scope teardown (rejected as the base mechanism)

On Linux, launch the stack under a systemd user transient scope
(`systemd-run --user --scope`) or a dedicated cgroup, and tear down by killing
the scope. This is strictly stronger than pgid signaling: a cgroup kill catches
**every** descendant regardless of `setsid`/double-fork — including conmon and
the agent containers that escape a process group, subsuming both the
pid-recycling class and the container-escape class on Linux. Rejected as the
**base** mechanism because it forks per-OS: the native app is a Wails app and
the stack package is generic `//go:build unix` (`stack.go:1`), so a
systemd/cgroup path cannot be the one cross-platform answer, and standing up a
user-scope/cgroup supervisor is a larger surface than the self-contained pidfile
A needs. Noted as a **possible Linux-only hardening** that, if Open Question 0
resolves to "stop containers", would be the cleanest Linux answer — layered
under A's portable path, not replacing it.

## Global Constraints

1. **Process-safety**: only the exact pgids read from this stack's own
   `<StateDir>/stack.pgids` are ever signaled; identity (pgid + leader start
   time) re-verified at signal time; no pattern kills, no process-table scans,
   ever.
2. **Bounded escalation**: SIGTERM first, a per-child drain budget, then group
   SIGKILL; total `down` stays inside the app's existing 60s window
   (`lifecycle.go:35`, `stackDownTimeout = 60 * time.Second`).
3. **The pgid file lives under the state dir** beside `stack.lock`, is written
   atomically (temp + rename), mode 0600, and is removed on **fully** successful
   teardown (rewritten to the survivor set on partial failure).
4. **Teardown order is reverse start order** (runner → server → postgres),
   matching `drainChildren` (`stack.go:287-289`).
5. **Teardown is confirmed per component by the channel it has**: socket
   quiescence for compass-server and postgres, group-ESRCH for the socketless
   runner — never signal delivery, and never treating a just-SIGKILled but
   not-yet-reaped (zombie) group as a failure.
6. **Scope**: the mechanism signals the three supervised stack children only.
   Live agent containers are out of scope (Open Question 0 / SEA-1884); this
   record does not silently claim to stop them.
7. **No frozen contract changes**: `up` stays fire-and-return; linger stays the
   default; no proto changes; DL-108 stays Active.
8. Unix-only, like the rest of the package (`//go:build unix`,
   `stack.go:1`).

## Plan

### T1 — Capture + persist child pgids (with identity token) at `up`

The pgid record file: define its format (version/writer header, then
`<component> <pgid> <starttime>` lines), write it atomically after each
successful spawn in `spawnChain`, delete it at the end of a fully successful
in-process `Down`, write nothing on the attach path. The per-child start time is
captured at spawn (a package-internal seam over `/proc/<pid>/stat` field 22 on
Linux) and carried as the identity token. Includes the read side (parse +
identity/staleness predicate) as a package-internal type so T2 consumes it, plus
unit tests (format round-trip incl. start-time column, partial-sequence rewrite,
torn-write impossibility via rename, attach writes nothing, identity-token
mismatch detected).

- **Interfaces:** produces `go/internal/stack/pgidfile.go`:
  `writePgidFile(stateDir string, entries []pgidEntry) error`,
  `readPgidFile(stateDir string) (pgidRecord, error)`,
  `removePgidFile(stateDir string) error`, `type pgidEntry struct { Component
  Component; Pgid int; StartTime uint64 }`; consumes `(*Stack).spawnChain`
  (`go/internal/stack/stack.go:171`) and the `Process` handle's pid
  (`go/internal/stack/adapters/process.go:78-79`, pid == pgid), exposed via a
  new `Pid() int` on the `stack.Process` seam plus a start-time reader.
- **Lands under:** SEA-1880 (parent SEA-1685).

### T2 — `down` reads the pgid file, refuses a live `up`, signals, confirms

The cross-process teardown routine: a new `stack.DownDetached(ctx, cfg, deps)
error` (name final at review) that (1) refuses when `lockHolderLive` on
`stack.lock` reports a live holder (an `up` in flight), (2) takes the guard flock
only for the read + consume decision and releases it before signaling, (3) reads
the pgid file, identity/ESRCH-skips dead-or-recycled groups, SIGTERMs live
identity-matched pgids in reverse start order, polls each group dead with
bounded SIGKILL escalation, (4) confirms per component (server/pg sockets dark,
runner group ESRCH), tolerating the post-SIGKILL zombie window, and (5) removes
the pgid file on full success or rewrites it to the survivor set on partial
failure. `runDown` calls it instead of (or before falling back to) the
attach-path `Up`+`Down`. Unit tests over a fake signaller + fake prober:
ordering, escalation, zombie-window tolerance, partial-failure survivor rewrite,
live-`up` refusal, concurrent-down serialization, absent-file × answering-socket
behavior.

- **Interfaces:** produces `stack.DownDetached(ctx context.Context, cfg Config,
  deps Deps) error` in `go/internal/stack/`, plus a `GroupSignaller` seam
  (`Signal(pgid int, sig ProcessSignal) error`, `Alive(pgid int, startTime
  uint64) bool`) in `Deps` with the syscall adapter in
  `go/internal/stack/adapters/`; consumes T1's
  `readPgidFile`/`removePgidFile`/`writePgidFile`, `lockHolderLive`
  (`go/internal/stack/lockfile.go:62`), `acquireGuard`
  (`go/internal/stack/lockfile.go:87`), `Deps.Prober`/`Deps.DBProber`
  (`stack.go:157`, `stack.go:264`); rewires `runDown`
  (`go/cmd/compass-stack/main.go:271-298`).
- **Lands under:** SEA-1880 (parent SEA-1685).

### T3 — Staleness / crash / recycling handling

Harden the record: the identity-token (start-time) check as the primary
staleness/recycling gate, ESRCH-as-already-gone, the one-child-wide crash window
documented and bounded, stale pgid file + stale lockfile cleared together on
full success, `down` on a half-spawned stack (file holds a prefix of the
sequence) drains exactly that prefix, and the partial-failure survivor-rewrite
policy. The header writer-pid/version is verified as a format/provenance guard
only, NOT relied on as a child-liveness signal. Table-driven tests for every
combination (dead group, recycled pid, zombie window, partial teardown,
half-spawned prefix).

- **Interfaces:** extends T1's `pgidRecord` with `WriterPid int` /
  `Version string` (header, provenance only) and per-entry `StartTime`; adds a
  `matches(pgid int, startTime uint64) bool` identity predicate; consumes
  `lockHolderLive`'s pattern (`go/internal/stack/lockfile.go:62-66`) for the
  live-`up` refusal only; touches only `go/internal/stack/`.
- **Lands under:** SEA-1880 (parent SEA-1685).

### T4 — Wire the app quit path; correct the now-false comment

No behavioral change in `compass-app` (it already execs `compass-stack down`,
`lifecycle.go:72`), but: verify the 60s `stackDownTimeout` still bounds the new
SIGTERM→wait→SIGKILL sequence with margin over the per-child budgets (Open
Question 2), and — because `stopStackAndQuit` quits anyway on a `down` error
(`lifecycle.go:35`) — assert what the app **reports** when the budget overruns:
a budget overrun must surface a loud `slog.Error` distinguishing "stack torn
down" from "quit with the stack possibly still partly up", so a silently
half-torn stack behind a clean-looking quit is impossible. Rewrite the
`lifecycle.go:13-15` file comment (and `embedded.go:207-214`'s `stackDownArgs`
comment) so the described mechanism is the pgid-file teardown, not the false
"attach → SIGTERM the child tree". Re-run + extend the existing quit-path unit
tests (`lifecycle_test.go`) for the overrun-reporting case.

- **Interfaces:** consumes the T2-fixed `compass-stack down` CLI (argv unchanged
  — `stackDownArgs`, `go/cmd/compass-app/embedded.go:207`); produces corrected
  comments + the overrun-report assertion in `go/cmd/compass-app/lifecycle.go`;
  no signature changes.
- **Lands under:** SEA-1685 T4.2 follow-up, referenced from SEA-1880.

### T5 — T4.3 e2e down-assertion + scripted headless CI variant

The e2e proof: extend the T4.3 embedded e2e so it (a) runs `compass-stack up`
as a real subprocess that exits, (b) runs `compass-stack down` as a second
fresh subprocess, and (c) asserts the server UDS and postgres socket go dark
and the runner group is ESRCH (the assertion shape `integration_podman_test.go`
already has in-process: `assertServerGone`/`assertPostgresGone`,
`integration_podman_test.go:281-282`). Plus a scripted headless variant (no
display, no Wails) runnable in CI against a real stack, doubling as the e2e
suite's own process-safe cleanup (cleanup signals only the pgids its own state
dir recorded — never a sweep). **Container caveat**: this test proves teardown
of the three supervised children only; it structurally cannot see conmon or the
agent containers (they are not started from the test's bin dir). If Open
Question 0 resolves to "stop containers", T5 gains a `podman ps` (by
compass-owned label) assertion; until then the test asserts the three-children
scope and the record says the container assertion is deliberately absent.

- **Interfaces:** consumes the `compass-stack` binary (subprocess exec), T1's
  file (asserts it exists while up, gone after full down), the socket probes +
  runner group-ESRCH check; produces a new cross-process subtest in
  `go/cmd/compass-stack/integration_podman_test.go` (or a sibling
  `crossprocess_test.go`) + a CI script entry; test-only, no production
  signatures.
- **Lands under:** SEA-1685 T4.3, referenced from SEA-1880.

## Tasks

- [ ] T1 — pgid record file (with start-time identity token): capture + persist at `up` (SEA-1880)
- [ ] T2 — `DownDetached`: refuse live `up`, read, signal, escalate, confirm per component; rewire `runDown` (SEA-1880)
- [ ] T3 — staleness/crash/recycling handling; partial-failure survivor rewrite (SEA-1880)
- [ ] T4 — app quit path wiring + false-comment correction + overrun reporting (SEA-1685 T4.2 / SEA-1880)
- [ ] T5 — T4.3 e2e down-assertion (three-children scope) + headless CI variant (SEA-1685 T4.3 / SEA-1880)

## Open Questions

0. **[LOAD-BEARING — parked for Matt] Does "Quit and stop stack" include
   stopping live agent containers?** Process-group signaling (this mechanism)
   cannot reach the agent containers the runner hosts: conmon double-forks to
   daemonize, so a container escapes the runner's process group and survives
   both the group SIGTERM and the group SIGKILL, and the runner does not stop
   its containers on its own shutdown (`internal/runner/host.go:204-217`: `Close`
   drops only socket listeners; teardown lives on the `Remove` RPC path). Every
   confirmation channel this record defines (server UDS, postgres socket, runner
   group-ESRCH) is blind to containers. So under any answer, Option A is the
   right mechanism for the three supervised children; the fork is **scope**, not
   mechanism:
   - **(a) No** — containers are the runner lane's teardown concern; "the tree"
     is scoped to the three supervised children (this record's default), and
     leaving containers running on quit is acceptable / handled elsewhere.
   - **(b) Yes, via a runner drain-on-SIGTERM contract** — the runner stops its
     containers before exiting (a runner-lane change; SEA-1884).
   - **(c) Yes, via engine-level teardown in `DownDetached`** — `podman stop` by
     a compass-owned *label* (process-safe: the label, never a pattern, selects
     the set). Adds a container-engine dependency to `down`.
   - **(d) Yes, via Linux cgroup-scoped supervision** (Alternative D) — subsumes
     the container-escape and pid-recycling classes on Linux, but forks the
     mechanism per-OS.

   **Stated assumption this record designs against (overnight, pending Matt):
   (a)** — "the tree" = the three supervised stack children; live agent
   containers are OUT of Option A's scope and tracked as a distinct runner-lane
   gap (**SEA-1884**). This is the conservative default: it keeps the teardown
   fork self-contained in this lane (no cross-lane proto/runner change gating it),
   matches the literal frozen text (written about the stack, before containers
   were a teardown consideration), and treats the runner-not-stopping-its-own-
   containers gap as the separate bug it is. If Matt rules (b)/(c)/(d), the
   companion mechanism lands under SEA-1884 (and, for (c), T2/T5 gain the
   label-scoped `podman stop` + `podman ps` assertion); the assumption is a park
   point, not a silent decision.
1. **[non-load-bearing] Absent pgid file + answering socket.** A live stack
   with no teardown record (spawned by an older build, or the file manually
   removed). Recommendation: `down` fails legibly ("stack is live but this
   build holds no teardown record; stop it with the build that started it, or
   manually"), mirroring the existing version-mismatch message shape
   (`main.go:288-289`). Never guess at pids. Fallback attach-`Down` still
   releases a stale lock.
2. **[non-load-bearing] Per-child drain budget split.** The app's total window
   is 60s (`lifecycle.go:35`); the server's graceful drain is the long pole.
   Recommendation: server 30s, runner 15s, postgres 10s before group SIGKILL,
   summing to 55s — under the window, but tight against exec overhead, so T4
   asserts the overrun-reporting path (above) rather than assuming the budget
   always fits. Tune at implementation.
3. **[non-load-bearing] Should in-process `Down` ALSO route through the pgid
   file?** Unifying the two paths (spawner's `Down` = `DownDetached` against
   its own file) would delete `drainChildren`'s handle-based path but loses
   `cmd.Wait`'s precise exit-status reaping (`process.go:125-144`).
   Recommendation: keep both for now — handles in-process, pgids cross-process
   — and note unification as cleanup once T5 proves the pgid path.
