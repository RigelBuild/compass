package vfs

// The P2 VolumeManager backend: one directory subtree per session on local
// storage. It is the whole volume lifecycle at P2 — create, resolve, attach,
// stamp, reconcile, expire — with snapshot/archive/restore reserved behind honest
// sentinels (vfs.go).

// The load-bearing part is the close-stamp: a small JSON marker written by
// teardown (Stamp) and read by the reaper (Expire), which bounds the storage leak
// the 14-day expiry exists to bound. The backend holds three invariants over it.

// (a) Attach clears the stamp before returning the path, so a reopened
// closed-but-unexpired session never carries a past-deadline stamp into new life.

// (b) Expire takes a per-volume advisory lock and RE-READS the stamp under it, so
// a volume is never reaped in the window between the reaper reading a stamp and a
// concurrent Attach clearing it. Attach takes the same lock, RE-VERIFIES the
// volume exists, and the lock file lives OUTSIDE the volume root on a stable inode.
// Lock waits are bounded by the caller's context.

// (c) The stamp carries close-vs-suspend intent from the caller. A suspended
// session's volume is never eligible however old, because suspend uses the same
// stop+remove teardown a close does — "the container is gone" cannot distinguish them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	// metaDirSuffix names the per-volume metadata dir, a sibling of the root:
	// <baseDir>/<sessionID><metaDirSuffix>. It holds the close-stamp and is the
	// volume-identity token scanBaseDir keys on. It sits outside the root because
	// keep-id makes the root agent-owned, and an agent must not be able to hide
	// its volume from the reaper by deleting what lives inside it.
	metaDirSuffix = ".compass-vfs.meta"
	// stampFileName is the close-stamp marker. Written atomically (temp +
	// rename) so the reaper can never observe a half-written stamp and
	// mis-decide eligibility.
	stampFileName = "close-stamp.json"
	// lockFileSuffix names the per-volume advisory lock file (invariant (b)),
	// appended to the volume root so the lock is a FILE SIBLING of the volume root
	// dir: <baseDir>/<sessionID><lockFileSuffix>. Outside the reaped subtree by
	// construction — see lockVolume for why that placement is load-bearing.
	lockFileSuffix = ".compass-vfs.lock"
	reapingSuffix  = ".compass-vfs.reaping"
	// stampTempPattern names the staging file for an atomic stamp write. It
	// lives in the same dir as its target so the rename is same-filesystem.
	stampTempPattern = "close-stamp-*.json.tmp"
)

// volumeDirMode is the mode of the base dir and of every per-session volume
// root: private to the invoking host user, who owns the whole subtree. The
// container's keep-id remap maps that user to the agent uid in-container, so
// owner-only is exactly right — no other host user has any business in a
// session's tree.
const volumeDirMode os.FileMode = 0o700

// stampFileMode is the mode of the close-stamp and lock files: owner-only,
// matching the volume root. The stamp is Runner state, never agent-readable
// content.
const stampFileMode os.FileMode = 0o600

// closeStamp is the on-disk close-stamp: why the session's volume was stamped
// and when. Both fields are read by Expire — the intent decides eligibility at
// all, the timestamp decides whether the retention window has passed.
type closeStamp struct {
	// Intent is the caller-supplied close-vs-suspend bit (invariant (c)).
	Intent CloseIntent `json:"intent"`
	// StampedAt is when the stamp was written. For a discovered orphan this is
	// the DISCOVERY time, not the lost close time — the deadline deliberately
	// restarts from discovery (see ReconcileOrphans).
	StampedAt time.Time `json:"stampedAt"`
}

// LocalManager is the P2 VolumeManager backend: a local-directory volume store.
// Every session's volume is the subtree <baseDir>/<sessionID>, so a volume's
// host path is a pure function of the base dir and the session id — which is
// what makes the mount path stable across every launch, resume, and burst of
// that session (P2-GC-d). It holds no per-session state: a fresh LocalManager
// on the same base dir after a Runner restart resolves and re-attaches exactly
// the same volumes.
//
// keep-id ownership invariant (load-bearing): the base dir and every
// per-session subtree are created by the Runner as its OWN invoking host user.
// The container's rootless keep-id remap
// (--userns=keep-id:uid=<agent-uid>,gid=<agent-gid>) maps a Runner-created root
// to agent-owned in-container, which is what satisfies ensureCheckoutDir's
// precondition — "CheckoutDir's parent must be writable by the agent uid"
// (go/internal/runtime/agent.go:354-357). A base dir placed outside the
// Runner's own ownership (a root-owned /var path, a differently-privileged
// installer's dir) breaks every launch on the volume path. This backend does no
// uid remapping itself: it creates dirs as the current process user, and the
// remap is the container runtime's.
type LocalManager struct {
	baseDir string
}

// LocalManager is the P2 backend behind the VolumeManager seam; the assertion keeps the
// two in lockstep at compile time.
var _ VolumeManager = (*LocalManager)(nil)

// NewLocalManager establishes the operator-configured base dir under which one
// subtree per session lives, keyed by session id, and returns the backend bound
// to it. The base dir is created if absent with plain os.MkdirAll — owned by the
// invoking host user, per the keep-id ownership invariant documented on
// LocalManager; the Runner runs as its own user and never chowns into another.
// It is an error if the base dir is empty, cannot be created, or exists as
// something other than a directory: a misconfigured base dir must fail at
// construction, not at the first launch on the volume path.
func NewLocalManager(baseDir string) (*LocalManager, error) {
	if baseDir == "" {
		return nil, errors.New("vfs: base dir must not be empty")
	}
	if err := os.MkdirAll(baseDir, volumeDirMode); err != nil {
		return nil, fmt.Errorf("vfs: creating volume base dir %q: %w", baseDir, err)
	}
	info, err := os.Stat(baseDir)
	if err != nil {
		return nil, fmt.Errorf("vfs: inspecting volume base dir %q: %w", baseDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("vfs: volume base dir %q is not a directory", baseDir)
	}
	return &LocalManager{baseDir: baseDir}, nil
}

// BaseDir is the base dir this manager places volumes under. Exposed so an
// operator-facing diagnostic can report the configured location without the
// caller re-deriving it.
func (m *LocalManager) BaseDir() string { return m.baseDir }

// CreateVolume creates the session's volume subtree (and its metadata dir) and
// returns the resolved Volume. It is idempotent: for a session that already has
// a volume it returns the existing one untouched, because volume destruction is
// Expire's alone (P2-GC-c) — a re-create must never clear a tree.
//
// It runs under the per-volume lock so Expire's slot cleanup cannot delete the
// metadata dir between the two mkdirs. A fresh root drops any stamp a crashed
// reap left in the metadata dir; that stamp belonged to the reaped volume.
//
// The subtree is created as the invoking host user, per the keep-id ownership
// invariant documented on LocalManager: the container's keep-id remap makes this
// Runner-owned root agent-owned in-container, satisfying ensureCheckoutDir's
// "CheckoutDir's parent must be writable by the agent uid" precondition
// (go/internal/runtime/agent.go:354-357).
func (m *LocalManager) CreateVolume(ctx context.Context, sessionID string) (Volume, error) {
	root, err := m.volumeRoot(sessionID)
	if err != nil {
		return Volume{}, err
	}
	if err := ctx.Err(); err != nil {
		return Volume{}, err
	}
	lock, err := lockVolume(ctx, root)
	if err != nil {
		return Volume{}, err
	}
	createErr := createLocked(root)
	if err := errors.Join(createErr, lock.release()); err != nil {
		return Volume{}, err
	}
	return Volume{SessionID: sessionID, HostRoot: root}, nil
}

// createLocked makes the metadata dir before the root, so a crash between the
// two leaves a metadata dir Expire can reclaim, never a root it cannot see.
func createLocked(root string) error {
	rootExists, err := pathExists(root)
	if err != nil {
		return err
	}
	if !rootExists {
		if err := clearStamp(root); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(metaDir(root), volumeDirMode); err != nil {
		return fmt.Errorf("vfs: creating volume metadata dir %q: %w", metaDir(root), err)
	}
	if err := os.MkdirAll(root, volumeDirMode); err != nil {
		return fmt.Errorf("vfs: creating volume root %q: %w", root, err)
	}
	return nil
}

// Lookup resolves a session's existing volume or returns ErrVolumeNotFound. It
// is the resolve half of the provision path's resolve-or-create: Attach needs a
// resolved Volume, so a caller cannot produce one from a bare session id
// without this verb. A not-found is an error-shaped signal the provision path
// converts into CreateVolume plus a cold materialize — never a silent recreate
// here, so the cold path stays observable.
func (m *LocalManager) Lookup(ctx context.Context, sessionID string) (Volume, error) {
	root, err := m.volumeRoot(sessionID)
	if err != nil {
		return Volume{}, err
	}
	if err := ctx.Err(); err != nil {
		return Volume{}, err
	}
	info, err := os.Stat(root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Volume{}, fmt.Errorf("vfs: session %q: %w", sessionID, ErrVolumeNotFound)
	case err != nil:
		return Volume{}, fmt.Errorf("vfs: inspecting volume root %q: %w", root, err)
	case !info.IsDir():
		return Volume{}, fmt.Errorf("vfs: volume root %q is not a directory: %w", root, ErrVolumeNotFound)
	}
	return Volume{SessionID: sessionID, HostRoot: root}, nil
}

// Attach makes the resolved volume available for mounting and returns its host
// path. The path is derived solely from the base dir and the session id, so it
// is identical on every launch, resume, and burst of that session (P2-GC-d) —
// which is what keeps `target/` and sccache valid across a container's death.
//
// Attach also clears any close-stamp before returning (invariant (a)), under
// the per-volume lock Expire uses (invariant (b)): the clear happens BEFORE the
// caller can mount the volume, so no window exists in which the volume is both
// attached-live and still carrying a past-deadline stamp. A stamp that is
// already absent is already-clear, not an error.
//
// Existence is decided UNDER the lock. An Attach at its expiry deadline waits
// for the context-bounded lock. Expire may acquire it between polls, reap, and
// release; Attach then returns ErrVolumeNotFound and the provision path cold-
// materializes. The unlocked pre-check is only a fast path; the under-lock check
// is authoritative because the lock file remains outside the reaped subtree.
//
// Both error paths JOIN the lock-release error, exactly as Stamp does. A
// release that failed would leave the flock held for this process's lifetime,
// so every later Expire pass would skip this volume forever — an unbounded
// leak in the mechanism built to bound one, and a silent contradiction of
// release()'s own contract. The typed ErrVolumeNotFound stays
// errors.Is-detectable through such a join, so the provision path's
// cold-materialize branch is preserved.
func (m *LocalManager) Attach(ctx context.Context, v Volume) (string, error) {
	// Fast pre-check: an Attach of a session that never had a volume fails here
	// without touching the lock namespace. Not the authority — see above.
	resolved, err := m.Lookup(ctx, v.SessionID)
	if err != nil {
		return "", err
	}
	// The wait is context-bounded; a relaunch may lose to an Expire poll and take
	// the ErrVolumeNotFound cold path.
	lock, err := lockVolume(ctx, resolved.HostRoot)
	if err != nil {
		return "", err
	}
	existErr := requireVolumeRoot(resolved.HostRoot, v.SessionID)
	var clearErr error
	if existErr == nil {
		clearErr = clearStamp(resolved.HostRoot)
	}
	releaseErr := lock.release()
	if existErr != nil {
		return "", errors.Join(existErr, releaseErr)
	}
	if clearErr != nil {
		return "", errors.Join(clearErr, releaseErr)
	}
	if releaseErr != nil {
		return "", releaseErr
	}
	return resolved.HostRoot, nil
}

// requireVolumeRoot checks existence under the per-volume lock. A missing root
// means Expire reaped it while the caller waited and must return typed not-found.
func requireVolumeRoot(root, sessionID string) error {
	info, err := os.Stat(root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("vfs: session %q: volume %q was reaped: %w", sessionID, root, ErrVolumeNotFound)
	case err != nil:
		return fmt.Errorf("vfs: inspecting volume root %q: %w", root, err)
	case !info.IsDir():
		return fmt.Errorf("vfs: volume root %q is not a directory: %w", root, ErrVolumeNotFound)
	}
	return nil
}

// Stamp records the caller's close-vs-suspend intent on the volume, with the
// current time as the stamp timestamp. This is the teardown path's half of the
// close-stamp mechanism: the W5 teardown calls it after stopping and removing
// the container, and Expire reads what it wrote.
//
// The intent MUST come from the caller (invariant (c)). D4's suspend uses the
// same stop+remove teardown path a close does, so "the container is gone"
// cannot distinguish them; only the caller knows whether the session is closed
// for good or expected back. Stamping IntentSuspended pins the volume against
// the reaper however old it gets.
//
// The write is atomic (temp file + rename in the metadata dir), so a reaper
// reading concurrently sees either the old stamp or the new one, never a torn
// record.
//
// The write happens under the per-volume lock, like every other stamp mutation
// (invariant (b)): Attach's clear and the reaper's read already serialize on
// that lock, and stamping under it too closes the one remaining gap — a
// teardown stamp landing in the middle of a reap could otherwise re-create the
// metadata dir of a volume Expire just reaped. Existence is re-checked under the lock for the
// same reason Attach re-checks it: a volume reaped while this call was blocked
// must surface as ErrVolumeNotFound, not leave orphan metadata behind.
func (m *LocalManager) Stamp(ctx context.Context, v Volume, intent CloseIntent) error {
	resolved, err := m.Lookup(ctx, v.SessionID)
	if err != nil {
		return err
	}
	lock, err := lockVolume(ctx, resolved.HostRoot)
	if err != nil {
		return err
	}
	writeErr := requireVolumeRoot(resolved.HostRoot, v.SessionID)
	if writeErr == nil {
		writeErr = writeStamp(resolved.HostRoot, closeStamp{Intent: intent, StampedAt: time.Now()})
	}
	return errors.Join(writeErr, lock.release())
}

// ReadStamp reports the volume's current close-stamp: ok is false when the
// volume is unstamped (a live session's volume, or a crash orphan not yet
// reconciled). Exported for the teardown/expiry driver's diagnostics and for
// the tests that assert the three invariants; it takes no lock, so a caller
// deciding to REAP must re-read under the lock as Expire does.
func (m *LocalManager) ReadStamp(ctx context.Context, v Volume) (intent CloseIntent, stampedAt time.Time, ok bool, err error) {
	resolved, lookupErr := m.Lookup(ctx, v.SessionID)
	if lookupErr != nil {
		return 0, time.Time{}, false, lookupErr
	}
	stamp, readErr := readStamp(resolved.HostRoot)
	if readErr != nil {
		return 0, time.Time{}, false, readErr
	}
	if stamp == nil {
		return 0, time.Time{}, false, nil
	}
	return stamp.Intent, stamp.StampedAt, true, nil
}

// ReconcileOrphans is the startup pass over the base dir: it stamps every
// UNSTAMPED volume closed at discovery time. A crash between container-remove
// and stamp-write leaves an unstamped closed volume, which Expire — which
// treats an unstamped volume as a live session's — would never reach; this pass
// makes every volume reachable by the reaper.
//
// The deadline of a discovered-orphan stamp runs from DISCOVERY, not from the
// lost close, so the volume survives one full retention window past this pass.
// That is the deliberate trade: a crash fails SAFE (reaped one window late),
// never OPEN (some volume the reaper can never see) and never WRONG (invariant
// (a) undoes a discovery stamp for free the moment that session is
// re-provisioned and Attached before the deadline; and a suspended session that
// never crashed was stamped IntentSuspended by its normal teardown, so this
// pass — which only touches UNSTAMPED volumes — leaves it alone).
//
// This needs no Server query and by design cannot want one: the Runner, not the
// Server, is authoritative for live-session truth, RunnerService exposes no
// session-query verb, and the Server's session bindings are cleared at every
// enroll — so the Server's live-session map is empty exactly when a restart
// would consult it. This package therefore takes no RPC or server dependency.
//
// PRECONDITION (normative): this is a STARTUP-ONLY pass, and it MUST complete
// before the manager serves ANY Attach — not merely before the first Expire. A
// crash orphan is by definition a volume with no live session, so running this
// pass while live Attaches are in flight is a CALLER error. Attach holds the
// per-volume lock only across its under-lock existence re-check and its stamp
// clear, so a pass that reaches a volume immediately AFTER that release sees an
// unstamped volume, wins the lock, and stamps a RUNNING session's volume
// IntentClosed — which invariant (a) cannot undo, because the Attach that
// would have cleared it has already happened. Enforcing this at the type level
// (the manager refusing Attach until the pass has run, or folding the pass into
// construction) is the W5/W6 wiring's concern; this method's precondition is
// simply "no concurrent Attach".
//
// The pass is SEPARATE from Expire rather than folded into it, and the ordering
// contract is: the expiry driver (W6) calls ReconcileOrphans once at startup,
// before it serves any Attach and before its first Expire. Keeping them apart
// is what makes "unstamped means live" a single, honest rule inside Expire —
// folding the scan in would make every Expire pass able to stamp a volume it is
// simultaneously judging, and would make a mid-session Expire (the ticker's
// steady state, when unstamped volumes ARE live sessions) stamp live sessions
// closed.
//
// A volume whose lock is held (a concurrent Attach) is skipped: it is being
// attached-live, which is the opposite of orphaned. Like Expire, the pass locks
// first and reads the stamp only under the lock, so it can never stamp a volume
// closed on the strength of a read an in-flight Attach has already invalidated.
func (m *LocalManager) ReconcileOrphans(ctx context.Context) error {
	discoveredAt := time.Now()
	return m.eachVolume(ctx, func(root string) error {
		lock, err := tryLockVolume(ctx, root)
		if err != nil {
			return err
		}
		if lock == nil {
			return nil // held by a live Attach; not an orphan.
		}
		writeErr := stampOrphanLocked(root, discoveredAt)
		releaseErr := lock.release()
		return errors.Join(writeErr, releaseErr)
	})
}

// stampOrphanLocked stamps only an existing, unstamped volume under its lock.
// It must not recreate a reaped root or overwrite a suspended volume's stamp.
func stampOrphanLocked(root string, discoveredAt time.Time) error {
	// A volume reaped between scanBaseDir's root stat and this lock acquisition
	// is not an orphan; stamping it would recreate metadata for a gone volume.
	if err := requireVolumeRoot(root, filepath.Base(root)); err != nil {
		if errors.Is(err, ErrVolumeNotFound) {
			return nil
		}
		return err
	}
	stamp, err := readStamp(root)
	if err != nil {
		return err
	}
	if stamp != nil {
		return nil
	}
	return writeStamp(root, closeStamp{Intent: IntentClosed, StampedAt: discoveredAt})
}

// Expire reaps volumes whose session is closed AND whose close-stamp is older
// than olderThan. It is the ONLY path that destroys volume contents (P2-GC-c):
// Release, Teardown, eviction, crash, and failed launches never do.
//
// Three volumes are ineligible by construction. An UNSTAMPED volume belongs to
// a live session (a crash orphan is made stamped by ReconcileOrphans, not
// here — see that method for why the passes are separate). A volume stamped
// IntentSuspended is never eligible however old, because its session is
// expected to resume onto exactly this volume at exactly this path. And a
// volume whose stamp is within olderThan is inside its retention window.
//
// Every eligibility decision is made UNDER the per-volume lock (invariant (b)):
// the pass locks a volume first and only then reads its stamp, so there is no
// unlocked read whose verdict could go stale. That ordering is what closes the
// window between reading a stamp and a concurrent Attach clearing it — Attach
// takes the same lock around its clear — and it is structural rather than a
// discipline this call site could lose: there is no code path here that can
// decide to reap from an unlocked read, because no unlocked read exists.
//
// A volume whose lock is held is SKIPPED this pass, not an error: contention
// means someone is attaching it, which is precisely the signal not to reap, and
// the next pass revisits it.
//
// A per-volume failure does not abort the pass: errors are accumulated and
// joined, so one unreadable volume cannot pin the storage of every volume
// behind it.
// Expire also sweeps reaping leftovers and reclaims orphan lock files.
func (m *LocalManager) Expire(ctx context.Context, olderThan time.Duration) error {
	now := time.Now()
	return m.scanBaseDir(ctx, func(kind entryKind, root string) error {
		switch kind {
		case entryVolume:
			lock, err := tryLockVolume(ctx, root)
			if err != nil || lock == nil {
				return err
			}
			reapErr := reapLocked(root, now, olderThan)
			return errors.Join(reapErr, lock.release())
		case entryReaping:
			lock, err := tryLockVolume(ctx, root)
			if err != nil || lock == nil {
				return err
			}
			removeErr := os.RemoveAll(reapingPath(root))
			if removeErr == nil {
				removeErr = reclaimLockLocked(root)
			} else {
				removeErr = fmt.Errorf("vfs: sweeping reaping leftover %q: %w", reapingPath(root), removeErr)
			}
			return errors.Join(removeErr, lock.release())
		case entryLock, entryMeta:
			rootExists, err := pathExists(root)
			if err != nil {
				return err
			}
			leftoverExists, err := pathExists(reapingPath(root))
			if err != nil {
				return err
			}
			if rootExists || leftoverExists {
				return nil
			}
			lock, err := tryLockVolume(ctx, root)
			if err != nil || lock == nil {
				return err
			}
			reclaimErr := reclaimLockLocked(root)
			return errors.Join(reclaimErr, lock.release())
		default:
			return nil
		}
	})
}

// reapLocked checks eligibility under the held lock, then renames the root.
// Rename is the destruction point; RemoveAll only cleans the unreachable tree.
func reapLocked(root string, now time.Time, olderThan time.Duration) error {
	stamp, err := readStamp(root)
	if err != nil {
		return err
	}
	if !eligible(stamp, now, olderThan) {
		return nil
	}
	if err := os.RemoveAll(reapingPath(root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("vfs: clearing stale reaping leftover %q: %w", reapingPath(root), err)
	}
	if err := os.Rename(root, reapingPath(root)); err != nil {
		return fmt.Errorf("vfs: reaping expired volume %q: %w", root, err)
	}
	if err := os.RemoveAll(reapingPath(root)); err != nil {
		return fmt.Errorf("vfs: removing reaped volume %q: %w", reapingPath(root), err)
	}
	return reclaimLockLocked(root)
}

// reclaimLockLocked removes the metadata dir and then the lock, only after
// confirming both session paths are absent under the lock. The lock unlink is
// the critical section's final mutation.
func reclaimLockLocked(root string) error {
	rootExists, err := pathExists(root)
	if err != nil {
		return err
	}
	leftoverExists, err := pathExists(reapingPath(root))
	if err != nil {
		return err
	}
	if rootExists || leftoverExists {
		return nil
	}
	if err := os.RemoveAll(metaDir(root)); err != nil {
		return fmt.Errorf("vfs: removing orphan volume metadata %q: %w", metaDir(root), err)
	}
	err = os.Remove(root + lockFileSuffix)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("vfs: reclaiming orphan volume lock %q: %w", root+lockFileSuffix, err)
	}
	return nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("vfs: checking session path %q: %w", path, err)
	}
	return true, nil
}

// eligible requires an existing IntentClosed stamp older than the retention
// window; unknown intents stay pinned rather than risking an incorrect reap.
func eligible(stamp *closeStamp, now time.Time, olderThan time.Duration) bool {
	if stamp == nil || stamp.Intent != IntentClosed {
		return false
	}
	return now.Sub(stamp.StampedAt) > olderThan
}

// Snapshot returns the reserved verb's sentinel; an empty ID is not a stored
// snapshot.
func (m *LocalManager) Snapshot(ctx context.Context, v Volume) (VolumeSnapshotID, error) {
	return "", ErrSnapshotNotImplemented
}

// Archive returns the reserved verb's sentinel; the object-store implementation
// is not part of this backend.
func (m *LocalManager) Archive(ctx context.Context, v Volume) (ArchiveRef, error) {
	return "", ErrArchiveNotImplemented
}

// Restore returns the reserved verb's sentinel, not a rehydrated volume.
func (m *LocalManager) Restore(ctx context.Context, ref ArchiveRef) (Volume, error) {
	return Volume{}, ErrRestoreNotImplemented
}

// volumeRoot rejects IDs that escape the base dir or collide with a sibling
// lock-file, metadata, or reaping path; callers already provide sanitized internal IDs.
func (m *LocalManager) volumeRoot(sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidSessionID)
	}
	if sessionID == "." || sessionID == ".." ||
		strings.ContainsRune(sessionID, '/') ||
		strings.ContainsRune(sessionID, os.PathSeparator) ||
		strings.ContainsRune(sessionID, 0) {
		return "", fmt.Errorf("%w: %q contains a path separator or traversal element", ErrInvalidSessionID, sessionID)
	}
	if strings.HasSuffix(sessionID, lockFileSuffix) {
		return "", fmt.Errorf("%w: %q collides with the volume lock-file namespace %q", ErrInvalidSessionID, sessionID, lockFileSuffix)
	}
	if strings.HasSuffix(sessionID, reapingSuffix) {
		return "", fmt.Errorf("%w: %q collides with the volume reaping namespace %q", ErrInvalidSessionID, sessionID, reapingSuffix)
	}
	if strings.HasSuffix(sessionID, metaDirSuffix) {
		return "", fmt.Errorf("%w: %q collides with the volume metadata namespace %q", ErrInvalidSessionID, sessionID, metaDirSuffix)
	}
	return filepath.Join(m.baseDir, sessionID), nil
}

// entryKind classifies base-dir entries by reserved name before structure.
type entryKind int

const (
	entryForeign entryKind = iota
	entryVolume
	entryLock
	entryReaping
	entryMeta // a metadata dir whose root is gone
)

// reapingPath returns the fixed sibling name for a renamed volume tree.
func reapingPath(root string) string { return root + reapingSuffix }

// scanBaseDir visits package-owned volume, lock, and reaping entries.
func (m *LocalManager) scanBaseDir(ctx context.Context, visit func(entryKind, string) error) error {
	entries, err := os.ReadDir(m.baseDir)
	if err != nil {
		return fmt.Errorf("vfs: scanning volume base dir %q: %w", m.baseDir, err)
	}
	var errs []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		name := entry.Name()
		var kind entryKind
		var root string
		switch {
		case strings.HasSuffix(name, lockFileSuffix) && !entry.IsDir():
			kind, root = entryLock, filepath.Join(m.baseDir, strings.TrimSuffix(name, lockFileSuffix))
		case strings.HasSuffix(name, reapingSuffix) && entry.IsDir():
			kind, root = entryReaping, filepath.Join(m.baseDir, strings.TrimSuffix(name, reapingSuffix))
		case strings.HasSuffix(name, metaDirSuffix) && entry.IsDir():
			root = filepath.Join(m.baseDir, strings.TrimSuffix(name, metaDirSuffix))
			info, statErr := os.Stat(root)
			switch {
			case statErr == nil && info.IsDir():
				kind = entryVolume
			case statErr == nil || errors.Is(statErr, os.ErrNotExist):
				kind = entryMeta
			default:
				errs = append(errs, fmt.Errorf("vfs: inspecting volume root %q: %w", root, statErr))
				continue
			}
		default:
			continue
		}
		if err := visit(kind, root); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// eachVolume filters scanBaseDir to volume roots that have a metadata dir.
func (m *LocalManager) eachVolume(ctx context.Context, fn func(root string) error) error {
	return m.scanBaseDir(ctx, func(kind entryKind, root string) error {
		if kind == entryVolume {
			return fn(root)
		}
		return nil
	})
}

// metaDir stores volume metadata beside the root, out of the agent's reach.
func metaDir(root string) string { return root + metaDirSuffix }

// stampPath is the volume's close-stamp file.
func stampPath(root string) string { return filepath.Join(metaDir(root), stampFileName) }

// readStamp decodes the volume's close-stamp. A nil stamp with a nil error
// means UNSTAMPED — the volume belongs to a live session, or is a crash orphan
// ReconcileOrphans has not yet discovered. Any other read or decode failure is
// returned: a stamp that exists but cannot be understood must not silently
// collapse into "unstamped" (which would look live) or into a defaulted
// IntentClosed (which would look reapable).
func readStamp(root string) (*closeStamp, error) {
	path := stampPath(root)
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is confined to the operator-configured base dir — either a traversal-checked session id or a ReadDir entry name of that dir, never caller-supplied
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil //nolint:nilnil // an absent stamp is the UNSTAMPED signal (documented on readStamp), not an error — every caller branches on the nil stamp
	}
	if err != nil {
		return nil, fmt.Errorf("vfs: reading close stamp %q: %w", path, err)
	}
	var stamp closeStamp
	if err := json.Unmarshal(data, &stamp); err != nil {
		return nil, fmt.Errorf("vfs: decoding close stamp %q: %w", path, err)
	}
	return &stamp, nil
}

// writeStamp writes the volume's close-stamp atomically AND durably: encode,
// stage to a temp file in the same metadata dir, fsync+rename over the target,
// then fsync the containing dir so the rename itself survives a host crash.
// The rename is what makes a concurrent reader see either the old stamp or the
// new one and never a torn record, and staging in the same dir keeps it a
// same-filesystem rename. The durability is load-bearing rather than belt-and-
// braces: losing an IntentClosed stamp in a crash writeback window is benign
// (ReconcileOrphans re-stamps it), but losing an IntentSuspended stamp fails
// WRONG — the volume returns UNSTAMPED, gets stamped IntentClosed at the next
// discovery, and the suspended session's volume that invariant (c) exists to
// pin forever becomes reap-eligible.
func writeStamp(root string, stamp closeStamp) error {
	dir := metaDir(root)
	if err := os.MkdirAll(dir, volumeDirMode); err != nil {
		return fmt.Errorf("vfs: creating volume metadata dir %q: %w", dir, err)
	}
	data, err := json.Marshal(stamp)
	if err != nil {
		return fmt.Errorf("vfs: encoding close stamp for %q: %w", root, err)
	}
	tmp, err := os.CreateTemp(dir, stampTempPattern)
	if err != nil {
		return fmt.Errorf("vfs: staging close stamp in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	writeErr := writeAndClose(tmp, data)
	if writeErr != nil {
		// Best-effort cleanup of the staging file; the write error is the
		// actionable one, so a failed unlink is joined rather than masking it.
		if rmErr := os.Remove(tmpName); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return errors.Join(writeErr, fmt.Errorf("vfs: removing staged close stamp %q: %w", tmpName, rmErr))
		}
		return writeErr
	}
	if err := os.Rename(tmpName, stampPath(root)); err != nil {
		if rmErr := os.Remove(tmpName); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return errors.Join(
				fmt.Errorf("vfs: committing close stamp for %q: %w", root, err),
				fmt.Errorf("vfs: removing staged close stamp %q: %w", tmpName, rmErr),
			)
		}
		return fmt.Errorf("vfs: committing close stamp for %q: %w", root, err)
	}
	return syncDir(dir)
}

// syncDir makes a committed rename durable. The file bytes are synced by
// writeAndClose; reporting close errors prevents claiming durability on failure.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: path is this package's own metadata dir under the operator-configured base dir, derived from a volume root, never caller-supplied
	if err != nil {
		return fmt.Errorf("vfs: opening volume metadata dir %q to fsync: %w", dir, err)
	}
	var errs []error
	if syncErr := d.Sync(); syncErr != nil {
		errs = append(errs, fmt.Errorf("vfs: fsyncing volume metadata dir %q: %w", dir, syncErr))
	}
	if closeErr := d.Close(); closeErr != nil {
		errs = append(errs, fmt.Errorf("vfs: closing volume metadata dir %q: %w", dir, closeErr))
	}
	return errors.Join(errs...)
}

// writeAndClose applies owner-only mode, syncs the staged file, and reports
// write, sync, and close failures so an incomplete stamp cannot be mistaken as durable.
func writeAndClose(f *os.File, data []byte) error {
	name := f.Name()
	writeErr := func() error {
		if _, err := f.Write(data); err != nil {
			return fmt.Errorf("vfs: writing close stamp %q: %w", name, err)
		}
		if err := f.Chmod(stampFileMode); err != nil {
			return fmt.Errorf("vfs: pinning close stamp mode %q: %w", name, err)
		}
		if err := f.Sync(); err != nil {
			return fmt.Errorf("vfs: fsyncing close stamp %q: %w", name, err)
		}
		return nil
	}()
	closeErr := f.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("vfs: closing close stamp %q: %w", name, closeErr)
	}
	return errors.Join(writeErr, closeErr)
}

// clearStamp removes the volume's close-stamp (invariant (a)), atomically AND
// durably. An absent stamp is already-clear, not a failure: Attach of a volume
// that was never stamped — the common case, a resume of a still-live session —
// must succeed.
//
// The unlink is fsynced (its containing metadata dir) with the same durability
// writeStamp gives the write, because invariant (a) is exactly as load-bearing
// as (c): a host crash shortly after an Attach that lost the clear would
// resurrect the past-deadline stamp with its ORIGINAL timestamp, and the next
// Expire would reap a volume belonging to the session that just re-attached —
// the loss of the warm tree this package exists to preserve.
func clearStamp(root string) error {
	path := stampPath(root)
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // already clear; nothing to make durable.
		}
		return fmt.Errorf("vfs: clearing close stamp %q: %w", path, err)
	}
	return syncDir(filepath.Dir(path))
}

// volumeLock is a held per-volume advisory file lock. It owns the open fd the
// lock lives on, so it must be released exactly once (release is idempotent
// against a double call).
type volumeLock struct {
	f *os.File
}

// lockVolume acquires the session's sibling lock. Acquisitions use non-blocking
// flock and verify the held inode against the current path, so a waiter cannot
// keep using an inode unlinked by Expire. Lock-file unlink is terminal: it requires
// an under-lock absence proof and is the final mutation before release.
const (
	lockPollInitial = time.Millisecond
	lockPollMax     = 50 * time.Millisecond
)

func tryLockVolume(ctx context.Context, root string) (*volumeLock, error) {
	path := root + lockFileSuffix
	for {
		l, mismatch, err := lockAttempt(ctx, path)
		if err != nil || l != nil {
			return l, err
		}
		if !mismatch {
			return nil, nil //nolint:nilnil // non-blocking callers use nil,nil to skip a contended volume.
		}
	}
}

func lockVolume(ctx context.Context, root string) (*volumeLock, error) {
	path := root + lockFileSuffix
	var f *os.File
	delay := lockPollInitial
	for {
		if err := ctx.Err(); err != nil {
			if f != nil {
				return nil, errors.Join(err, closeLockFile(path, f))
			}
			return nil, err
		}
		if f == nil {
			var err error
			f, err = openLockFile(path)
			if err != nil {
				return nil, err
			}
		}
		l, mismatch, err := lockAttemptOnFile(ctx, path, f)
		if err != nil {
			return nil, err
		}
		if l != nil {
			return l, nil
		}
		if mismatch {
			if err := closeLockFile(path, f); err != nil {
				return nil, err
			}
			f = nil
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errors.Join(ctx.Err(), closeLockFile(path, f))
		case <-timer.C:
		}
		if delay < lockPollMax {
			delay *= 2
			if delay > lockPollMax {
				delay = lockPollMax
			}
		}
	}
}

func lockAttempt(ctx context.Context, path string) (*volumeLock, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	f, err := openLockFile(path)
	if err != nil {
		return nil, false, err
	}
	l, mismatch, err := lockAttemptOnFile(ctx, path, f)
	if l == nil && err == nil {
		if err := closeLockFile(path, f); err != nil {
			return nil, false, err
		}
	}
	return l, mismatch, err
}

func openLockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, stampFileMode) //nolint:gosec // path derives from a traversal-checked session ID under the configured base dir
	if err != nil {
		return nil, fmt.Errorf("vfs: opening volume lock %q: %w", path, err)
	}
	return f, nil
}

func lockAttemptOnFile(ctx context.Context, path string, f *os.File) (*volumeLock, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, errors.Join(err, closeLockFile(path, f))
	}
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			if ctx.Err() != nil {
				return nil, false, errors.Join(ctx.Err(), closeLockFile(path, f))
			}
			return nil, false, nil
		}
		return nil, false, errors.Join(fmt.Errorf("vfs: locking volume %q: %w", path, err), closeLockFile(path, f))
	}
	fdInfo, err := f.Stat()
	if err != nil {
		return nil, false, errors.Join(fmt.Errorf("vfs: verifying volume lock %q: %w", path, err), closeLockFile(path, f))
	}
	pathInfo, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, errors.Join(fmt.Errorf("vfs: verifying volume lock %q: %w", path, err), closeLockFile(path, f))
	}
	if !os.SameFile(fdInfo, pathInfo) {
		return nil, true, nil
	}
	return &volumeLock{f: f}, false, nil
}

func closeLockFile(path string, f *os.File) error {
	if err := f.Close(); err != nil {
		return fmt.Errorf("vfs: closing volume lock %q: %w", path, err)
	}
	return nil
}

// release drops the advisory lock and closes its fd. Both failures are
// reported: an unreleased lock would make every later Expire pass skip this
// volume, and a leaked fd is a real defect — neither is "not actionable".
func (l *volumeLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	var errs []error
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		errs = append(errs, fmt.Errorf("vfs: unlocking volume lock %q: %w", f.Name(), err))
	}
	if err := f.Close(); err != nil {
		errs = append(errs, fmt.Errorf("vfs: closing volume lock %q: %w", f.Name(), err))
	}
	return errors.Join(errs...)
}
