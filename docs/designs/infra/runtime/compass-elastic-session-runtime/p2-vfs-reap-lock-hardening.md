# P2 — VFS reap/lock hardening: atomic rename-then-delete reap, ctx-interruptible lock acquisition, orphan lock reclaim

Status: PROPOSED — a hardening pass over the merged W1 backend
(`go/internal/vfs/localvolume.go`, PR #898, RIG-3278) under the frozen P2
record [p2-persistent-session-volume.md](./p2-persistent-session-volume.md)
(RIG-2395; its Plan § W1). Authoritative issue scope: RIG-3310 — the three
findings W1's review deferred (MED-4, MED-5, LOW-2). Every `go/internal/*`
citation is a symbol in the `RigelBuild/compass` monorepo at main `d3205926`.

Ledger impact: none. This record changes no cross-cutting decision: the
`VolumeManager` interface (`go/internal/vfs/vfs.go`) is untouched, the
close-stamp invariants (a)/(b)/(c) and P2-GC-c/P2-GC-d are preserved as
written, and every change is internal to the `LocalManager` backend.
`docs/designs/DECISIONS.md` is untouched.

## Problem / Intent

W1 shipped a correct volume lifecycle whose reap and lock primitives are
**coarse in the failure cases**. Three coupled findings, all in
`go/internal/vfs/localvolume.go`:

1. **MED-5 — a reap is not atomic at the observer's view.** `reapLocked`
   destroys an eligible volume with one `os.RemoveAll(root)`. Go's
   `removeAllFrom` (`os/removeall_at.go`) deletes bottom-up and, on a
   per-entry failure (EACCES on a read-only subdir, EBUSY on a mount point
   inside the tree), keeps going, records the first error, and finally fails
   to `unlinkat` the root because it is non-empty — so a partial reap leaves
   the **root directory present** with a gutted tree beneath it. Every
   existence check this package has is a stat of the root
   (`Lookup`, `requireVolumeRoot`, `eachVolume`'s marker stat), so a gutted
   root reads as a live volume: a concurrent `Attach` mounts a half-tree, and
   because the close-stamp lives *inside* the removed subtree
   (`metaDirName`), a root whose stamp was deleted but whose shell survived
   reads as UNSTAMPED — a live session's — and is ineligible for re-reaping
   until the next `ReconcileOrphans` at Runner restart.
2. **MED-4 — a blocked lock acquisition ignores its context.** `Attach` and
   `Stamp` call `lockVolume(root, true)`, which parks in
   `syscall.Flock(fd, LOCK_EX)`. No context can interrupt that syscall, and
   the holder it waits behind may be `Expire` in the middle of a multi-GB
   `os.RemoveAll` under the same lock. Both verbs accept a `context.Context`
   that is inert past `Lookup`'s pre-check, so a Runner shutdown during a
   reap cannot drain its in-flight launches.
3. **LOW-2 — sibling lock files leak.** The per-volume lock file
   (`<baseDir>/<sessionID>.compass-vfs.lock`) deliberately outlives its
   volume (the stable-inode argument in `lockVolume`'s doc), so the base dir
   accumulates one empty file per session id ever seen on the box.
   `eachVolume` `ReadDir`s the base dir on every `Expire` and
   `ReconcileOrphans` tick, so the reaper's scan cost grows without bound.
   W1's doc names the fix — a maintenance pass that unlinks orphan lock files
   only while uncontended — and defers it.

This record lands the three together: rename-before-reap makes a reap
atomic at the observer's view (the `RemoveAll` stays under the lock, but
it now removes a tree no session can reach); context-interruptible
acquisition bounds every remaining wait; and the orphan-lock reclaim rides
the reap pass, made safe by a lock-inode re-verify that the reclaim's unlink
would otherwise break.

## Approach

Three internal changes to `LocalManager`, no interface change, one new
reserved name under the base dir. Each is stated with the code it replaces.

### Code reality this record grounds on

All in `go/internal/vfs/localvolume.go` unless noted.

- `reapLocked` is the sole destroyer: "It removes ONLY the volume root … `if
  err := os.RemoveAll(root); err != nil { return fmt.Errorf("vfs: reaping
  expired volume %q: %w", root, err) }`". It runs inside `Expire`'s
  per-volume closure between `lockVolume(root, false)` and `lock.release()`,
  so the whole `RemoveAll` executes with the per-volume lock held.
- `lockVolume(root string, block bool)` opens `root + lockFileSuffix` with
  `O_CREATE|O_RDWR` and calls `syscall.Flock(int(f.Fd()), how)` with
  `how = LOCK_EX` (plus `LOCK_NB` when `!block`). Contention in
  non-blocking mode returns `(nil, nil)`; blocking mode parks in the syscall.
  `Attach` and `Stamp` call it with `block = true` and carry a
  now-unreachable `if lock == nil` guard ("Unreachable with block=true").
  `Expire` and `ReconcileOrphans` call it with `block = false` and skip on
  `nil`.
- Every existence verdict is a stat of the root: `Lookup` (`os.Stat(root)`
  → `ErrVolumeNotFound` on not-exist or non-dir), `requireVolumeRoot` (the
  under-lock re-check `Attach`, `Stamp`, and `stampOrphanLocked` share), and
  `eachVolume` (an entry is a volume iff it is a directory containing the
  `metaDirName` marker dir).
- `eachVolume` is one `os.ReadDir(m.baseDir)` per call, skipping
  non-directories — which is how the sibling `*.compass-vfs.lock` files are
  kept out of the volume scan today.
- `volumeRoot` is the reserved-namespace guard: it rejects empty, `.`, `..`,
  separator- or NUL-bearing ids, and any id ending in `lockFileSuffix`
  ("its volume root would be another session's sibling lock-file path").
- `NewLocalManager` establishes the base dir with `os.MkdirAll(baseDir,
  volumeDirMode)` and fails construction if it is not a directory.
- The close-stamp lives inside the root (`stampPath(root) =
  <root>/.compass-vfs-meta/close-stamp.json`); `readStamp` returns
  `(nil, nil)` for an absent file — the UNSTAMPED (live) signal.
- Go's `os.RemoveAll` (`os/removeall_at.go`, `removeAllFrom`) recurses
  bottom-up, counts per-entry failures, keeps the first (`recurseErr`), and
  only then tries `removedirat` on the directory itself — so an EACCES on
  any leaf leaves every ancestor directory, including the root, in place.
- Go's `os.Rename` (`os/file_unix.go`, `rename`) refuses to rename onto an
  existing directory (`EEXIST` via its own `Lstat` check) and is otherwise a
  single `rename(2)`: atomic on a POSIX filesystem when source and target
  share it.
- The house event-gate for concurrency tests is `testing/synctest`
  (`go/internal/runnerhub/router_test.go`, `provision_dedup_test.go`;
  `go/internal/ingest/board_reconcile_test.go`) — `synctest.Wait()` observes
  "durably blocked", never a sleep. W1's race test instead gates on
  `/proc/locks` (`heldFlockField`, `waitForBlockedFlock`,
  `hasBlockedFlockWaiter` in `localvolume_test.go`) because a parked flock
  has no user-space completion signal. That gate depends on a *blocked*
  waiter existing, which the change below removes.
- `time.Sleep` is forbidden by `forbidigo` (`go/.golangci.yml`); the only
  sanctioned forms are an irreducible poll-tick or a synctest virtual-clock
  advance, each with `//nolint:forbidigo // <reason>`.
- `go.mod` floor is `go 1.26.0` at the cited main (`testing/synctest` is
  stable from 1.25); the package imports only the standard library (`vfs.go`
  header: "This package imports nothing outside the standard library").

### (a) Reap = rename under the lock, then remove the renamed tree

`reapLocked` keeps its contract (the sole destroyer, reachable only with the
per-volume lock held, stamp re-read under that lock) and changes its
destruction step from one `os.RemoveAll(root)` into:

1. `os.RemoveAll(reapingPath(root))` — clear any leftover from an earlier
   partial reap of this session id, so the rename below cannot fail with
   `EEXIST` (Go's `os.Rename` refuses to rename onto an existing directory).
   `ErrNotExist` is the common case and is success. Any other failure
   returns **before** the rename: the volume is untouched and still eligible
   (I-2 semantics), and a leftover that never converges therefore pins that
   session id's next reap until it is repaired (see OQ-2).
2. `os.Rename(root, reapingPath(root))` where
   `reapingPath(root) = root + reapingSuffix`,
   `reapingSuffix = ".compass-vfs.reaping"`. Same parent directory, so the
   same filesystem, so one atomic `rename(2)`. **This is the destruction
   point.** After it, `Lookup`, `requireVolumeRoot`, and the base-dir scan
   all see no root and report `ErrVolumeNotFound`; before it, the volume is
   byte-for-byte intact. There is no instant at which an observer can see a
   partially-deleted tree at the volume's path.
3. `os.RemoveAll(reapingPath(root))` — the expensive part, still under the
   lock (see § Alternatives for why not after release). A partial failure
   here leaves a `.compass-vfs.reaping` sibling with whatever survived; the
   session's root is already gone, so nothing can mount it.

The renamed tree still contains the `metaDirName` marker and the stamp, so
the base-dir scan must classify by **name** before it looks for the marker:
an entry named `<sessionID>.compass-vfs.reaping` is a reaping leftover, never
a volume, whatever it contains. `volumeRoot` rejects a session id ending in
`reapingSuffix` exactly as it rejects one ending in `lockFileSuffix`, so no
session can be keyed onto another session's reaping path.

**Leftover sweep.** `Expire` gains a second responsibility on the same scan:
for every reaping leftover, take the session's lock non-blocking, `RemoveAll`
it, release. Contended → skip (someone is attaching or stamping that session
id; the leftover is inert and the next pass revisits). This keeps P2-GC-c
literal: `Expire` is still the only path that destroys volume contents, now
including the contents it already renamed out. `ReconcileOrphans` never
touches a leftover — it is a stamping pass over live volumes only.

### (b) Lock acquisition: non-blocking flock in a ctx-aware poll, with an inode re-verify

`lockVolume(root, block bool)` splits into two functions with no boolean:

- `tryLockVolume(ctx, root) (*volumeLock, error)` — one `LOCK_EX|LOCK_NB`
  attempt; `(nil, nil)` on contention. Used by `Expire` and
  `ReconcileOrphans` (today's `block=false`).
- `lockVolume(ctx, root) (*volumeLock, error)` — loops `tryLockVolume`-style
  attempts on one open fd, waiting between attempts on
  `select { case <-ctx.Done(): …; case <-timer.C: }` with exponential
  backoff from `lockPollInitial = 1ms` to `lockPollMax = 50ms`. Returns
  `ctx.Err()` when cancelled, after closing the fd; never `(nil, nil)`. Used
  by `Attach` and `Stamp` (today's `block=true`), whose "unreachable with
  block=true" `lock == nil` guards are deleted with the reason for them.

Both share one acquisition core, and that core adds the step LOW-2 needs:
**after** `Flock` succeeds, `f.Stat()` and `os.Stat(path)` are compared with
`os.SameFile`. A match means the fd holds the lock on the inode currently at
the lock path — the real lock. A mismatch (the path is gone, or now names a
different inode) means the acquirer landed on an inode that a reclaim
unlinked while it was waiting; it closes the fd and re-opens the path, which
converges on the live inode. Re-opening is bounded by `ctx`. Without this
step, unlinking a held lock file re-creates the two-inode split
`lockVolume`'s W1 doc describes (a waiter that opened the old inode before
the unlink acquires it after the holder's release, while a newcomer creates
and acquires a fresh inode — two actors "hold" the lock). With it, the split
is a detected retry, and the reclaim in (c) becomes safe.

The polling trade is deliberate: a blocking `flock` wakes the instant the
holder releases; a poll wakes up to `lockPollMax` later. Holders are `Attach`
/ `Stamp` (a stat, an unlink or an fsync'd write — milliseconds) and `Expire`
(the rename plus `RemoveAll` of one tree — seconds for a large tree). Fifty
milliseconds of added latency on a relaunch that races the reaper is noise
beside a container launch, and during a seconds-long reap the poll costs
twenty syscalls per second. What it buys is that every wait this package
performs is now interruptible by the context every verb already accepts.

One W1 semantic is softened, not lost. W1's `Attach` comment says an Attach
racing the reaper "must win the volume, not abandon a launch": a waiter
parked in a blocking `flock` is woken at release and normally wins before a
later `LOCK_NB` attempt. A polling waiter holds nothing between attempts,
so an `Expire` `tryLockVolume` that lands in the gap after a holder's
release — up to `lockPollMax` wide — can win, judge the stamp, and reap
before the parked `Attach` retries. That `Attach` then re-verifies under
the lock, finds no root, and returns `ErrVolumeNotFound`; the provision
path cold-materializes. Correctness is the W1 race test's other
interleaving, already asserted; what changes is that a relaunch landing in
the same instant as its own expiry deadline may take the cold path where W1
would usually have won. H1 rewrites the comment to say so.
`ReconcileOrphans` is unaffected only because of its normative
no-concurrent-`Attach` precondition, which this record keeps.

### (c) Orphan lock reclaim rides the `Expire` scan

On the same base-dir scan, an entry named `<sessionID>.compass-vfs.lock` is
a lock file. `Expire` checks, **unlocked**, whether the session's root
exists: if it does the lock file is live — keep it, no lock taken (this is
the common case, so live volumes cost the pass one extra stat, not a lock
cycle). If the root is absent: `tryLockVolume`; contended → skip; else
re-check under the lock that both the root and the reaping leftover are
absent, and if so `os.Remove` the lock file — **the final act of the
critical section** — then release. `ErrNotExist` on the unlink is success
(another reaper got there first).

Why "final act" is a rule and not a style: after the unlink, a newcomer can
create and acquire a fresh inode at the path while this holder still holds
the old one. That is harmless only because this holder does nothing further
to the session's state. No future code may mutate a volume after unlinking
its lock.

Why the reclaim is in `Expire` and not `ReconcileOrphans`: the leak grows
for the Runner's lifetime, so a startup-only pass bounds nothing; `Expire`
is the periodic pass and already the only destroyer.

The unlocked fast path is safe in the conservative direction only: a root
present unlocked is kept without proof, and keeping is never wrong; a root
absent unlocked is re-proven under the lock before the unlink.

### The scan: one `ReadDir`, classified by name

`eachVolume` today filters `ReadDir` to "directory with the marker". With
two reserved suffixes and three passes wanting different entry kinds, the
scan becomes `scanBaseDir(ctx, visit func(kind entryKind, root string) error)`
over one `os.ReadDir(m.baseDir)`, classifying each entry by name first
(`entryLock` if the name ends in `lockFileSuffix` and is not a directory;
`entryReaping` if it ends in `reapingSuffix` and is a directory), then by
structure (`entryVolume` if a directory carrying the `metaDirName` marker),
else `entryForeign` (W2's snapshot store, stray files). `root` is always the
session's would-be volume root, `<baseDir>/<sessionID>`, so a visitor derives
the lock and reaping paths from it with the same two helpers every other
call site uses. `eachVolume(ctx, fn)` stays as the `entryVolume`-only filter
`ReconcileOrphans` uses; `Expire` visits `scanBaseDir` directly and switches
on kind. Per-entry errors join, as today; the `ctx.Err()` check per entry is
kept.

### What does not change

- The `VolumeManager` interface, `Volume`, `CloseIntent`, and every exported
  error in `go/internal/vfs/vfs.go`.
- Invariants (a), (b), (c) on the close-stamp; the stamp's location inside
  the root; `writeStamp` / `clearStamp` durability.
- The lock file's placement as a root sibling and the reason for it.
- `ReconcileOrphans`'s startup-only precondition and its stamping logic.
- `CreateVolume` takes no lock, as today; it creates only `<root>/<marker>`
  and never touches a reaping leftover or a lock file. It may race H3's
  reclaim — a root created after the under-lock "root absent" proof but
  before the unlink leaves a live volume with no lock file — and that is
  benign: the lock file is materialized on demand by the next acquisition,
  no one held the unlinked inode, and the reclaimer touches nothing after
  the unlink (GC-6).

## Alternatives considered

- **Blocking `flock` on a helper goroutine, `select` on `ctx.Done()`.** The
  helper stays parked in an uninterruptible syscall after the caller
  returns; when the holder finally releases, the orphaned helper acquires
  the lock and must release it again, briefly excluding a legitimate
  acquirer that arrived first — a phantom hold with no owner. It also leaks
  a goroutine per cancelled wait until then, which `-race` and any future
  goroutine-leak gate see. Rejected: the poll is boring, owner-less holds
  do not exist, and the latency cost is bounded and small.
- **`RemoveAll` the renamed tree after releasing the lock.** Shortens the
  hold to the rename and lets a relaunch racing the reap get its
  `ErrVolumeNotFound` immediately. Costs: two reapers (a hand-run one beside
  the service) can `RemoveAll` the same leftover concurrently (benign for
  `RemoveAll`, which tolerates `ErrNotExist`), but a second reap of a
  recreated volume for the same session id can rename onto a name whose
  removal is in flight, so its own removal and the in-flight one interleave
  and one reports a spurious `ENOTEMPTY`. With (b) the wait is
  interruptible, so the remaining benefit is relaunch-at-deadline latency,
  a rare event. Rejected for now; the rename-first structure makes it a
  local change if measured to matter (recorded as OQ-1).
- **Unique per-attempt reaping names** (`<sid>.compass-vfs.reaping.<nonce>`).
  Avoids the clear-leftover-first step, and with it the one way a stuck
  leftover can pin a session id's next reap. Costs prefix matching in the
  scan, an unbounded leftover set per session, and a nonce source. Rejected:
  one deterministic name per session keeps the leftover set at most one and
  the clear-first step is one `RemoveAll` that is `ErrNotExist` in every
  non-failure run. Note the coupling: nonce names are also what removes the
  concurrent-removal interleaving that keeps `RemoveAll` under the lock
  (OQ-1), so a future move to RemoveAll-after-release should adopt both
  together, not one without the other.
- **Reclaim lock files in `ReconcileOrphans`.** Startup-only; the leak
  grows between restarts. Rejected (see (c)).
- **One base-dir lock instead of per-volume locks.** Serializes every
  `Attach` behind every reap. Rejected; not what W1 froze.
- **`renameat2(RENAME_EXCHANGE)` / `O_TMPFILE` tricks.** Linux-only
  extensions outside `os`; the plain rename is atomic on every filesystem
  this package can run on. Rejected.

## Invariants

Numbered so a task brief and a test name can cite them.

- **I-1 (atomic destruction).** A volume is destroyed by exactly one
  `rename(2)` of its root. At every instant either the whole tree is at
  `<baseDir>/<sessionID>` or none of it is. `Lookup`/`Attach`/`Stamp` for
  that session return `ErrVolumeNotFound` from the rename onward, and never
  return a path whose tree is partially deleted.
- **I-2 (rename failure is non-destructive).** If the clear-leftover step
  or the rename fails (`EBUSY` on a leaked mount point, `EACCES` on the base
  dir, a non-converging stale leftover), the volume is untouched and the
  error is joined into the pass result; the next pass retries. Nothing is
  removed from a volume whose rename did not succeed.
- **I-3 (leftovers are inert and convergent).** A reaping leftover is never
  a volume: no pass stamps it, `Lookup` never resolves it, `CreateVolume`
  never reuses it. Every `Expire` pass attempts to remove every uncontended
  leftover, so a leftover persists only while its removal keeps failing,
  and each such pass surfaces the failure.
- **I-4 (every wait is ctx-bounded).** No code path in this package blocks
  on the per-volume lock past the caller's context. A cancelled wait
  returns `ctx.Err()` (via `errors.Is`), closes its fd, and has mutated
  nothing on disk.
- **I-5 (lock identity).** A returned `*volumeLock` holds `LOCK_EX` on the
  inode currently at `<root><lockFileSuffix>`. An acquisition that lands on
  any other inode is discarded and retried. Consequently, at any instant, at
  most one actor holds a verified lock for a session id.
- **I-6 (reclaim is terminal).** A lock file is unlinked only by its holder,
  only after an under-lock proof that the session has neither a root nor a
  reaping leftover, and as the last mutation of that critical section.
- **I-7 (W1 invariants intact).** (a) `Attach` clears the stamp under the
  lock before returning; (b) every reap decision is made under the lock
  from an under-lock stamp read, and `Attach`/`Stamp`/`stampOrphanLocked`
  re-verify existence under the lock; (c) `IntentSuspended` pins forever.
  P2-GC-c: `Expire` is the only destroyer, now including leftovers and
  orphan lock files. P2-GC-d: the returned path is unchanged.

## Failure behavior

| Event | Outcome |
| --- | --- |
| `RemoveAll` of the renamed tree fails partway (`EACCES` on a subdir, `EBUSY` on a leaked mount inside the tree) | Root absent; `Lookup`/`Attach` → `ErrVolumeNotFound`; leftover at `<root>.compass-vfs.reaping`; pass returns the joined error; next pass sweeps the leftover (I-3). |
| `os.Rename` fails | Volume intact, stamp intact, still eligible; joined error; retried next pass (I-2). |
| Crash between rename and `RemoveAll` | Leftover on disk; next `Expire` sweeps it. `ReconcileOrphans` ignores it (name classification). |
| Crash between the reclaim unlink and release | The kernel drops the lock with the process; the path is gone; the next acquirer creates a fresh inode. No state to repair. |
| `ctx` cancelled while `Attach`/`Stamp` waits | `ctx.Err()`; fd closed; no stamp change, no clear, no path returned (I-4). The provision path treats it as any other launch abort. |
| `ctx` already cancelled on entry | `Lookup`'s existing check returns `ctx.Err()` before the lock namespace is touched; no lock file is created. |
| Lock file unlinked from under a waiter | The waiter acquires the dead inode, fails the `SameFile` check, re-opens, and contends on the live inode (I-5). |
| `Attach` polling while `Expire` acquires in the gap after a holder's release | `Expire` wins the lock, reaps on its under-lock stamp read, releases; `Attach`'s next attempt wins, `requireVolumeRoot` finds no root, `ErrVolumeNotFound`; the provision path cold-materializes. Bounded by `lockPollMax`; W1's "must win" is softened to "may lose a same-instant race at the deadline" (§ (b)). |
| `CreateVolume` for a session id whose orphan lock is being reclaimed | Root appears after the under-lock absence proof; the unlink proceeds; the live volume has no lock file until the next `Attach`/`Stamp`/`Expire` materializes one. Benign (§ What does not change). |
| A session is recreated while its leftover is still being removed | Root and leftover coexist under different names; the live root is judged on its own stamp; the leftover is swept on its own. A later reap of the new root clears the leftover first, then renames. |
| Leftover removal never converges (an agent `chmod 000`'d a subdir) | Surfaced as a joined error on every pass; storage of that subtree leaks until repaired. Not silently absorbed. See OQ-2. |
| Corrupt or unknown-intent stamp | Unchanged from W1: error, no rename, no removal. |
| Two reapers on one base dir (service + hand-run) | Every mutation is under a verified lock (I-5), so they serialize per session; the loser skips on contention and revisits next pass. |

## Global Constraints

Inherited: the P2 record's constraints (P2-GC-a … P2-GC-f) and the parent's
nine, unmodified. Specific to this record:

- **GC-1 — no interface change.** `go/internal/vfs/vfs.go` is not touched.
  `VolumeManager`, `Volume`, `CloseIntent`, and every exported error keep
  their shape and doc. `LocalManager`'s exported surface (`NewLocalManager`,
  `BaseDir`, `Stamp`, `ReadStamp`, `ReconcileOrphans`) keeps its signatures.
  Interface promotion of `Stamp`/`ReconcileOrphans` is RIG-3309, not here.
- **GC-2 — standard library only.** The package header's "imports nothing
  outside the standard library" holds. No `golang.org/x/sys`, no
  `renameat2`, no third-party lock library.
- **GC-3 — `Expire` is the only destroyer, literally.** Volume roots,
  reaping leftovers, and orphan lock files are removed by `Expire` and by
  nothing else. `ReconcileOrphans`, `Attach`, `Stamp`, `CreateVolume`, and
  `lockVolume` never unlink anything except the close-stamp (`clearStamp`,
  as today).
- **GC-4 — no goroutine in the lock path.** Lock acquisition is a loop in
  the caller's goroutine that selects on `ctx.Done()`; the helper-goroutine
  design is rejected (§ Alternatives) and must not be reintroduced.
- **GC-5 — two reserved suffixes, guarded in two places.** `lockFileSuffix`
  and `reapingSuffix` are the only reserved names under the base dir. Each is
  rejected by `volumeRoot` and classified by `scanBaseDir`. Adding a third
  reserved name means editing both in the same change, with a
  `TestVolumeRootRejectsTraversal` row.
- **GC-6 — unlink-last.** Inside a critical section that unlinks a lock
  file, the unlink is the final mutation before release. No stat, stamp,
  rename, or remove follows it.
- **GC-7 — no sleeps, no timing assumptions in tests.** Concurrency tests
  gate on `testing/synctest` (`synctest.Wait`, virtual-clock advance with
  the house `//nolint:forbidigo // synctest virtual clock …` reason) or on a
  channel/`WaitGroup`. A wall-clock `time.Sleep` is a defect. Tests that
  make a filesystem operation fail through permissions skip under
  `os.Geteuid() == 0` (the `go/server/socket_test.go` idiom) and restore
  modes in `t.Cleanup` so `t.TempDir` cleanup succeeds.
- **GC-8 — docs move with the code.** Every doc comment that describes the
  replaced mechanism is rewritten in the commit that replaces it: the file
  header's "(b)" paragraph, `metaDirName` ("reaps its stamp in one
  os.RemoveAll"), `Attach` ("the wait is bounded"), `reapLocked`,
  `lockVolume` (the "Accepted tradeoff … out of P2 scope" paragraph), and
  `eachVolume`. No sentence promising a "future maintenance pass" survives.
- **GC-9 — the gate set.** RIG-3310's acceptance gate: `moon run
  compass-go:fmt`, `:vet`, `:lint` (`golangci-lint run --config
  .golangci.yml ./...`, 0 issues, additionally with `--build-tags microvm`),
  `:nilaway` (zero baseline; a finding is guarded at the deref, never a file
  opt-out), `:build`, `:test` (`go test -race ./...`), plus `go test -race
  -count=20 ./internal/vfs/...` for the concurrency tests. A nilaway finding
  on a deleted `lock == nil` guard is resolved by keeping the guard with a
  reason that names nilaway, never by widening the change.
- **GC-10 — public-repo hygiene.** This record and the code comments it
  drives cite only `RigelBuild/compass` paths and bare tracker ids.

## Plan

Three tasks, sequential, one bookmark, one PR (RIG-3310 is one PR-sized
change; each task is its own commit so the review reads three mechanisms,
not one diff). All edits are to `go/internal/vfs/localvolume.go` and
`go/internal/vfs/localvolume_test.go`. Every task ships its own tests and
its own doc-comment rewrite (GC-8). Lane hint: `implement-go`; H1 is the
`implement-hard` candidate (it rewrites the package's only race test).

### H1 — ctx-interruptible lock acquisition with inode re-verify (MED-4, and the I-5 base for LOW-2)

- **Interfaces (produces):**

  ```go
  // lockPollInitial / lockPollMax bound the backoff between non-blocking
  // acquisition attempts in lockVolume. Every attempt is LOCK_NB; no code
  // path in this package ever parks in a blocking flock (I-4, GC-4).
  const (
      lockPollInitial = 1 * time.Millisecond
      lockPollMax     = 50 * time.Millisecond
  )

  // tryLockVolume makes ONE non-blocking attempt on the session's sibling
  // lock file. (nil, nil) means contended — the caller skips this volume.
  // A successful flock is verified against the inode currently at the lock
  // path (I-5); a mismatch re-opens and re-attempts, bounded by ctx.
  // Replaces lockVolume(root, false).
  func tryLockVolume(ctx context.Context, root string) (*volumeLock, error)

  // lockVolume acquires the session's sibling lock file, waiting with
  // exponential backoff between non-blocking attempts and returning
  // ctx.Err() the moment ctx is done. Never returns (nil, nil).
  // Replaces lockVolume(root, true).
  func lockVolume(ctx context.Context, root string) (*volumeLock, error)

  // lockAttempt is the shared core: open (O_CREATE|O_RDWR, stampFileMode),
  // flock(LOCK_EX|LOCK_NB), then os.SameFile(f.Stat(), os.Stat(path)). On
  // EWOULDBLOCK/EAGAIN it returns (nil, nil) and keeps nothing open. A
  // mismatch is a different inode at path OR no file at path (the reclaim
  // unlinked it): it closes f, re-opens, and tries again (ctx-bounded).
  // Any other error closes f and is returned joined with the close error.
  func lockAttempt(ctx context.Context, path string) (*volumeLock, error)
  ```

  `lockVolume` keeps one fd across backoff iterations (a repeat `flock` on
  the same fd is the cheap path); it re-opens only after a `SameFile`
  mismatch. The wait is
  `select { case <-ctx.Done(): return nil, ctx.Err(); case <-timer.C: }` on a
  `time.Timer` created in the caller's goroutine — which is what makes the
  parked state durably-blocked under `testing/synctest`.
- **Consumes:** `volumeLock`, `release`, `lockFileSuffix`, `stampFileMode`
  unchanged.
- **Call-site edits:** `Attach` and `Stamp` call `lockVolume(ctx,
  resolved.HostRoot)` and drop their `if lock == nil { … "volume lock
  unavailable" }` guards (the reason for them, "unreachable with block=true",
  no longer exists). `Expire` and `ReconcileOrphans` call
  `tryLockVolume(ctx, root)`. `Attach`'s "Block rather than skip … must win
  the volume … the wait is bounded" comment is rewritten: the wait is
  ctx-bounded, and a relaunch landing in the same instant as its expiry
  deadline may lose the race to a reap that acquires between two poll
  attempts, taking the `ErrVolumeNotFound` cold path (§ (b), Failure
  behavior).
- **Doc edits:** `lockVolume`'s doc keeps the placement argument verbatim
  and replaces the "Accepted tradeoff … out of P2 scope" paragraph with the
  I-5 re-verify rule and a forward reference to H3's reclaim. The file
  header's (b) paragraph gains one sentence: waits are ctx-bounded.
- **Test edits (existing):** every `lockVolume(x, false)` in the test file
  becomes `tryLockVolume(t.Context(), x)` (`TestReconcileOrphansSkipsA-
  VolumeBeingAttached`, `TestExpireSkipsALockedVolume`,
  `TestVolumeLockSurvivesAReap`, `TestVolumeLockFileIsOutsideTheVolumeRoot`,
  `TestAttachRacingAReapDoesNotReturnAReapedPath`).
  `TestAttachRacingAReapDoesNotReturnAReapedPath` MUST be rewritten: its
  gate waits for a `-> FLOCK` waiter row in `/proc/locks`, and a `LOCK_NB`
  attempt never enqueues a waiter, so the W1 gate would time out at 30 s
  against this change. The rewrite runs inside `synctest.Test`: hold the
  stand-in reaper lock via `tryLockVolume`, `go m.Attach(t.Context(), v)`,
  `synctest.Wait()` (returns when Attach is parked in its backoff select),
  remove the root, release, receive the result on a bubble channel (the
  receive lets the virtual clock advance to Attach's timer). Assertion is
  unchanged. `heldFlockField`, `waitForBlockedFlock`, `hasBlockedFlockWaiter`
  and their doc comments are deleted (dead once no test observes a blocked
  waiter); the `strconv` and `syscall` imports go with them — on main those
  three helpers are their only users in the test file.
- **Tests (new):**
  - `TestAttachReturnsCtxErrWhileLockIsHeld` (I-4): in a bubble, hold the
    lock, age-stamp the volume, `ctx, cancel := context.WithCancel(t.Context())`,
    `go Attach(ctx, v)`, `synctest.Wait()`, `cancel()`, receive:
    `errors.Is(err, context.Canceled)`; then `readStamp(root)` still returns
    the aged stamp (nothing cleared) and the root is intact. The same shape
    for `Stamp` asserts no stamp was written.
  - `TestAttachWithCancelledCtxTouchesNoLock`: a pre-cancelled ctx returns
    `ctx.Err()` and `<root><lockFileSuffix>` does not exist afterwards.
  - `TestLockAcquisitionConvergesOnTheLiveInode` (I-5): in a bubble, hold
    via `tryLockVolume` (inode A); `go lockVolume(ctx, root)` — the waiter
    opens A and parks; `synctest.Wait()`; now, as the reclaim does under the
    held lock, `os.Remove` the lock path; release the holder; receive the
    waiter's lock (the receive advances the virtual clock to its timer).
    Assert `os.SameFile(waiter.f.Stat(), os.Stat(path))` — the waiter
    re-opened and holds the inode now at the path — and that a fresh
    `tryLockVolume` reports contention while the waiter holds it. Without
    the re-verify the waiter holds unlinked A, `os.Stat(path)` fails, and
    the fresh attempt creates B and succeeds — the two-inode split.
- **Depends:** nothing.
- **Gate:** `go test -race -count=20 ./internal/vfs/...` green; lint clean.

### H2 — rename-before-reap, name-classified scan, leftover sweep (MED-5)

- **Interfaces (produces):**

  ```go
  // reapingSuffix names the sibling a volume root is renamed to at the
  // instant it is destroyed: <baseDir>/<sessionID><reapingSuffix>. The
  // rename is the destruction (I-1); the RemoveAll that follows is cleanup
  // of a tree no session can reach. Reserved like lockFileSuffix (GC-5).
  const reapingSuffix = ".compass-vfs.reaping"

  // reapingPath is the leftover path for a volume root.
  func reapingPath(root string) string { return root + reapingSuffix }

  // entryKind classifies one base-dir entry by name first, structure second.
  type entryKind int
  const (
      entryForeign entryKind = iota // not this package's: W2's store, stray files
      entryVolume                   // directory carrying the metaDirName marker
      entryLock                     // <sessionID><lockFileSuffix>, a file
      entryReaping                  // <sessionID><reapingSuffix>, a directory
  )

  // scanBaseDir runs one os.ReadDir over the base dir and calls visit for
  // every non-foreign entry with its kind and the session's volume root
  // (<baseDir>/<sessionID> — the lock and reaping paths derive from it).
  // Per-entry errors are joined; ctx is checked per entry, as eachVolume
  // does today.
  func (m *LocalManager) scanBaseDir(ctx context.Context, visit func(kind entryKind, root string) error) error

  // eachVolume is unchanged in signature and contract: scanBaseDir filtered
  // to entryVolume. ReconcileOrphans keeps using it.
  func (m *LocalManager) eachVolume(ctx context.Context, fn func(root string) error) error
  ```

  `reapLocked(root, now, olderThan)` keeps its signature. Its body after the
  eligibility check becomes: `os.RemoveAll(reapingPath(root))` (clear a stale
  leftover; `ErrNotExist` is success; any other error is wrapped
  `"vfs: clearing stale reaping leftover %q: %w"` and returned **before**
  the rename, volume untouched), `os.Rename(root, reapingPath(root))`
  (wrapped: `"vfs: reaping expired volume %q: %w"`; failure returns with the
  volume untouched, I-2), then `os.RemoveAll(reapingPath(root))` (wrapped:
  `"vfs: removing reaped volume %q: %w"`; failure returns the error, the
  leftover stays for the sweep, I-3).

  `Expire` becomes a `scanBaseDir` visitor: `entryVolume` → today's
  lock-reap-release; `entryReaping` → `tryLockVolume`, skip on contention,
  `os.RemoveAll(reapingPath(root))`, release; `entryLock` → H3 (a no-op in
  this commit). `ReconcileOrphans` is unchanged (it uses `eachVolume`).

  `volumeRoot` gains
  `if strings.HasSuffix(sessionID, reapingSuffix) { return "", fmt.Errorf("%w: %q collides with the volume reaping namespace %q", ErrInvalidSessionID, sessionID, reapingSuffix) }`
  beside the `lockFileSuffix` check.
- **Consumes:** H1's `tryLockVolume`.
- **Doc edits:** `reapLocked` (rename is the destruction point; RemoveAll
  is cleanup), `metaDirName` ("reaps its stamp in one os.RemoveAll" → the
  stamp travels with the renamed tree and is never read again),
  `eachVolume` (identity is name-then-structure), `Expire` (three entry
  kinds; leftovers are swept every pass), `volumeRoot` (two reserved
  suffixes), the file header's "reconcile, expire" line.
- **Tests (new):**
  - `TestReapIsAtomicUnderPartialRemoveFailure` (I-1, I-3): skip if
    `os.Geteuid() == 0`. Create a volume, write `<root>/pinned/held` and
    `chmod 0o500` `pinned` (cleanup restores `0o700` on whichever of
    `<root>/pinned` or `<reaping>/pinned` exists — the rename moves it);
    age-stamp; `Expire` returns a non-nil error; `Lookup` and `Attach` both
    return `ErrVolumeNotFound`; `exists(reapingPath(root))` and
    `exists(filepath.Join(reapingPath(root), "pinned", "held"))`. Do NOT
    assert the leftover still carries its stamp or marker: `removeAllFrom`
    attempts every entry of a directory and only then fails on the
    directory itself, so the fully-deletable `.compass-vfs-meta` subtree is
    gone from the leftover while `pinned/held` survives — this leftover is
    marker-less, and an assertion that `readStamp(reapingPath(root))` still
    returns the aged stamp fails on correct code. Name-first classification
    is proven by the two manufactured-leftover tests below, not here.
    Restore the mode; `Expire` returns nil and the leftover is gone. Against
    W1 this test fails at the first `Lookup` (the gutted root is still a
    directory).
  - `TestReapRenameFailureLeavesVolumeIntact` (I-2): skip if euid 0.
    Materialize the lock file (acquire + release once), write a sentinel
    file, age-stamp, `chmod 0o500` the base dir (cleanup restores);
    `Expire` returns a non-nil error; root, sentinel, and stamp all still
    present; `Lookup` succeeds.
  - `TestExpireSweepsAReapingLeftoverWhateverItsStamp` (I-3): manufacture
    a leftover with `os.Rename(root, reapingPath(root))` after writing a
    **fresh `IntentSuspended`** stamp (the most protected state); `Expire`
    with a 14-day window removes it. This is the assertion that leftovers
    are classified by name, not judged as volumes. Then: `CreateVolume` the
    same id beside a fresh manufactured leftover; `Expire` sweeps the
    leftover and leaves the live unstamped root; age-stamp the root with a
    second manufactured leftover present; one `Expire` leaves neither (the
    clear-first step).
  - `TestVolumeRootRejectsTraversal` gains rows `"sess-x" + reapingSuffix`
    and `reapingSuffix`.
  - `TestReconcileOrphansIgnoresAReapingLeftover`: a leftover with **no**
    stamp (the marker dir alone) is not stamped by `ReconcileOrphans`
    (`readStamp(reapingPath(root))` stays nil) — the pass never resurrects
    a leftover as an orphan.
- **Depends:** H1.
- **Gate:** as H1.

### H3 — orphan lock reclaim (LOW-2)

- **Interfaces (produces):**

  ```go
  // reclaimLockLocked unlinks the session's sibling lock file if, under the
  // held lock, the session has neither a volume root nor a reaping
  // leftover. It MUST be the final mutation of the caller's critical
  // section (GC-6): after the unlink a newcomer may hold a fresh inode.
  // ErrNotExist is success. Returns nil (kept) when either path exists.
  func reclaimLockLocked(root string) error
  ```

  Call sites, each as the last statement before `lock.release()`:
  1. `reapLocked` after a successful final `RemoveAll` — the common case
     leaves no orphan at all;
  2. `Expire`'s `entryReaping` visitor after a successful `RemoveAll`;
  3. `Expire`'s `entryLock` visitor: unlocked `os.Stat(root)` — present →
     return nil without locking (live volume, common case); absent →
     `tryLockVolume`, skip on contention, `reclaimLockLocked(root)`,
     release.
- **Consumes:** H2's `scanBaseDir`/`entryLock`, H1's `tryLockVolume` and
  I-5 (the property that makes an unlink under a polling waiter safe).
- **Doc edits:** `lockVolume`'s doc gets the reclaim rule (unlink-last,
  under-lock proof, I-5 makes it safe); `Expire`'s doc names the third
  entry kind; the "one empty file per session id ever seen on this box"
  sentence is deleted.
- **Tests (new):**
  - `TestExpireReclaimsOrphanLockFilesOnlyWhenUncontended` (I-6): three
    sessions, each with a materialized lock file. A: root removed →
    after `Expire` the lock file is gone. B: live, unstamped → lock file
    kept, root kept. C: root removed, lock held by the test via
    `tryLockVolume` → kept; release; next `Expire` → gone.
  - `TestReapLeavesNoOrphanLockInTheCommonCase`: aged volume with a
    materialized lock file; one `Expire`; root, leftover, and lock file all
    absent.
  - `TestExpireReclaimsTheLockAfterSweepingALeftover`: manufactured
    leftover plus its lock file, no root; one `Expire`; both absent.
  - `TestVolumeLockFileIsOutsideTheVolumeRoot`'s tail (which materializes a
    sibling lock file and asserts `Expire` still reaps the eligible volume)
    is kept; its final assertion gains "and the lock file is now absent".
- **Depends:** H1, H2.
- **Gate:** as H1, plus the full RIG-3310 gate set (GC-9) on the stacked
  result.

## Test strategy

| Invariant | Test | Fails on |
| --- | --- | --- |
| I-1 atomic destruction | `TestReapIsAtomicUnderPartialRemoveFailure` | a reap that removes in place (W1) |
| I-2 rename failure non-destructive | `TestReapRenameFailureLeavesVolumeIntact` | a reap that removes before or without renaming |
| I-3 leftovers inert + convergent | `TestExpireSweepsAReapingLeftoverWhateverItsStamp`, `TestReconcileOrphansIgnoresAReapingLeftover`, the partial-failure test's second `Expire` | a scan that classifies by marker before name; a sweep that consults the stamp |
| I-4 ctx-bounded waits | `TestAttachReturnsCtxErrWhileLockIsHeld` (Attach + Stamp), `TestAttachWithCancelledCtxTouchesNoLock` | a blocking flock; a helper goroutine that mutates after the caller returned |
| I-5 lock identity | `TestLockAcquisitionConvergesOnTheLiveInode`, `TestVolumeLockSurvivesAReap` (migrated) | an acquire without the `SameFile` check |
| I-6 reclaim terminal + uncontended | `TestExpireReclaimsOrphanLockFilesOnlyWhenUncontended`, `TestExpireReclaimsTheLockAfterSweepingALeftover`, `TestReapLeavesNoOrphanLockInTheCommonCase` | a reclaim that ignores contention; one that unlinks a live volume's lock |
| I-7 W1 invariants intact | every existing W1 test, unchanged in assertion (only the lock-helper signature migrates); `TestAttachRacingAReapDoesNotReturnAReapedPath` re-gated on synctest | any regression of (a)/(b)/(c), P2-GC-c, P2-GC-d |
| GC-5 reserved names | `TestVolumeRootRejectsTraversal` (+2 rows) | a session id keyed onto a leftover path |

Mechanics the executor must not rediscover:

- **`testing/synctest` is the gate for every wait.** A `LOCK_NB` attempt
  never enqueues a kernel waiter, so W1's `/proc/locks` gate cannot observe
  the new parked state; the backoff `select` on a bubble-created timer is
  durably blocking, so `synctest.Wait()` returns exactly when the waiter is
  parked. `t.Context()` inside `synctest.Test` is bubble-associated; the
  test goroutine advances the virtual clock by blocking on a bubble channel
  (`<-done`), never by sleeping. `t.Run` is not allowed inside a bubble —
  Attach/Stamp variants are two loop iterations, not subtests. Flock,
  stat, rename, and remove are ordinary syscalls inside the bubble: not
  durably blocking, but they complete.
- **Permission-driven failures skip under root** (`os.Geteuid() == 0`,
  the `go/server/socket_test.go` idiom) and restore the mode in `t.Cleanup`
  so `t.TempDir` can clean up. The CI moon lane runs unprivileged on
  `ubuntu-latest`, so the skip never fires there.
- **Flock contention within one process is real**: two fds on the same
  inode contend, which is what lets a test hold the reaper's lock on the
  test goroutine while `Attach` polls on another — the same fact W1's race
  test already relies on.
- **The race trio**: `go test -race -count=20 ./internal/vfs/...` is run
  per task and on the stacked result; a flake at count 20 is a design bug
  in the test (a timing assumption crept in), not a retry.

## Tasks

- [ ] **H1** — `tryLockVolume`/`lockVolume` split, ctx-bounded backoff,
      `SameFile` inode re-verify; `Attach`/`Stamp`/`Expire`/`ReconcileOrphans`
      call-site migration; `TestAttachRacingAReapDoesNotReturnAReapedPath`
      re-gated on synctest and the `/proc/locks` helpers deleted; three new
      tests (I-4 ×2, I-5). Docs per GC-8.
- [ ] **H2** — `reapingSuffix`/`reapingPath`, `volumeRoot` guard,
      `scanBaseDir` + `entryKind`, `reapLocked` = clear-leftover → rename →
      remove, `Expire` sweeps `entryReaping`; five test changes (I-1, I-2,
      I-3 ×2, GC-5). Docs per GC-8.
- [ ] **H3** — `reclaimLockLocked` at its three call sites, `Expire`'s
      `entryLock` visitor with the unlocked fast path; three new tests plus
      one extended assertion (I-6). Docs per GC-8.
- [ ] Full gate set (GC-9) green on the stacked PR; PR body carries
      `Spec-impact: none` and `Ledger-impact: none`.

## Open Questions

All non-load-bearing: the record is correct and executable without a
ruling on any of them; each is a documented deferral.

- **OQ-1 (non-load-bearing) — `RemoveAll` after lock release.** The
  rename-first structure makes "release, then remove the leftover" a local
  change that would shorten the lock hold from tree-size to one rename.
  Deferred: with I-4 the hold is interruptible, the remaining benefit is
  latency on a relaunch that races the reap (rare), and the cost is a
  concurrent-removal interleaving on a recreated-then-re-reaped session id
  (§ Alternatives). Revisit if W6's driver measures reap holds that delay
  launches.
- **OQ-2 (non-load-bearing) — surfacing a non-converging leftover.** A
  leftover whose removal fails on every pass (an agent-`chmod 000`'d
  subtree, a leaked mount) is reported as a joined `Expire` error each
  tick, leaks its storage until an operator repairs it, and pins that
  session id's next reap (the clear-first step fails before the rename, so
  a recreated-then-expired volume of the same id stays on disk, intact and
  eligible, until the leftover is cleared). Whether that earns a metric or
  a log line with the leftover path is W6's (the expiry driver owns
  `Expire`'s error sink); this record only guarantees it is never silent.
- **OQ-3 (non-load-bearing) — poll bounds.** `lockPollInitial = 1ms`,
  `lockPollMax = 50ms` are chosen for a hold profile of milliseconds
  (`Attach`/`Stamp`) to seconds (a large reap). They are unexported
  constants; tuning them is a one-line change with no contract impact.

Spec-impact: none (no `docs/specs` clause names the reap or lock mechanism;
the `VolumeManager` contract is unchanged).
Ledger-impact: none (the P2 record's DL-326 row is unchanged; every decision
here is backend-internal and record-level).
Refs RIG-3310 RIG-2395 RIG-3278
