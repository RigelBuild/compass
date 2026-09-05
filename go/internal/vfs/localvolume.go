package vfs

// This file is the P2 VolumeManager backend: one directory subtree per session
// on the box's fast local storage, under an operator-configured base dir. It is
// the whole of the volume lifecycle at P2 — create, resolve, attach, stamp,
// reconcile, expire — with the snapshot/archive/restore verbs reserved behind
// honest sentinels (see vfs.go).
//
// The load-bearing part is the close-stamp mechanism, which is what bounds the
// storage leak the parent record's 14-day expiry policy exists to bound. A
// stamp is a small JSON marker in the volume's metadata dir written by the
// teardown path (Stamp) and read by the reaper (Expire), and the backend holds
// three invariants over it:
//
//	(a) Attach clears the stamp before returning the path, so a reopened
//	    closed-but-unexpired session never carries a past-deadline stamp into
//	    its new life.
//	(b) Expire takes a per-volume advisory file lock and RE-READS the stamp
//	    under it, so a volume is never reaped in the window between the reaper
//	    reading a stamp and a concurrent Attach clearing it. Attach takes the
//	    same lock around its clear, and RE-VERIFIES under it that the volume
//	    still exists — an Attach that was blocked behind the Expire which reaped
//	    the volume returns ErrVolumeNotFound rather than a path to a deleted
//	    tree. The lock file lives OUTSIDE the volume root, on a stable inode a
//	    reap cannot unlink, which is what makes that under-lock verdict
//	    authoritative across reap+recreate (see lockVolume).
//	(c) The stamp carries close-vs-suspend intent, supplied by the caller. A
//	    suspended session's volume is never eligible however old, because D4's
//	    suspend uses the same stop+remove teardown path a close does — "the
//	    container is gone" cannot distinguish them.

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

// metaDirName is the per-volume metadata dir inside a volume root: it holds the
// close-stamp. It lives INSIDE the volume root so that reaping the volume reaps
// its stamp in one os.RemoveAll — no orphan stamp can outlive the volume it
// describes. The per-volume lock file deliberately does NOT live here (see
// lockFileSuffix): a lock inside the reaped subtree cannot serialize against
// the reap itself. The dotted, package-prefixed name keeps it clear of any path
// a checkout would write.
const metaDirName = ".compass-vfs-meta"

const (
	// stampFileName is the close-stamp marker. Written atomically (temp +
	// rename) so the reaper can never observe a half-written stamp and
	// mis-decide eligibility.
	stampFileName = "close-stamp.json"
	// lockFileSuffix names the per-volume advisory lock file (invariant (b)),
	// appended to the volume root path so the lock is a FILE SIBLING of the
	// volume root dir: <baseDir>/<sessionID><lockFileSuffix>. Outside the
	// reaped subtree by construction — see lockVolume for why that placement is
	// load-bearing rather than incidental.
	lockFileSuffix = ".compass-vfs.lock"
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

// LocalManager is the P2 backend behind the frozen seam; the assertion keeps the
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
	if err := os.MkdirAll(filepath.Join(root, metaDirName), volumeDirMode); err != nil {
		return Volume{}, fmt.Errorf("vfs: creating volume root %q: %w", root, err)
	}
	return Volume{SessionID: sessionID, HostRoot: root}, nil
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
// Existence is decided UNDER the lock, and that is the load-bearing part. An
// Attach that arrives exactly at a volume's expiry deadline blocks on the lock
// Expire is holding, and the Expire it is waiting behind may reap the volume
// before releasing. The unlocked pre-check below is therefore a fast path only,
// NOT the authority: its verdict is already stale by the time the lock is won.
// Re-stating the check under the lock is what turns that race into an honest
// ErrVolumeNotFound — which the provision path converts into a cold
// CreateVolume plus materialize — rather than a nil error carrying the path of
// a directory that no longer exists. It is only authoritative because the lock
// file lives OUTSIDE the reaped subtree (see lockVolume): a lock inside the
// volume root would be unlinked by the reap, so the winning Attach would hold a
// lock on a dead inode that excludes nobody.
func (m *LocalManager) Attach(ctx context.Context, v Volume) (string, error) {
	// Fast pre-check: an Attach of a session that never had a volume fails here
	// without touching the lock namespace. Not the authority — see above.
	resolved, err := m.Lookup(ctx, v.SessionID)
	if err != nil {
		return "", err
	}
	// Block rather than skip: an Attach racing the reaper must win the volume,
	// not abandon a launch. Expire holds the lock only for the duration of one
	// volume's re-verify-and-reap, so the wait is bounded.
	lock, err := lockVolume(resolved.HostRoot, true)
	if err != nil {
		return "", err
	}
	if lock == nil {
		// Unreachable with block=true, which never reports contention; guard
		// anyway so a future non-blocking caller cannot nil-deref.
		return "", fmt.Errorf("vfs: session %q: volume lock unavailable", v.SessionID)
	}
	existErr := requireVolumeRoot(resolved.HostRoot, v.SessionID)
	var clearErr error
	if existErr == nil {
		clearErr = clearStamp(resolved.HostRoot)
	}
	releaseErr := lock.release()
	if existErr != nil {
		return "", existErr
	}
	if clearErr != nil {
		return "", clearErr
	}
	if releaseErr != nil {
		return "", releaseErr
	}
	return resolved.HostRoot, nil
}

// requireVolumeRoot reports ErrVolumeNotFound unless root is still a directory.
// Called under the per-volume lock by Attach and Stamp, it is the authoritative
// existence check: the volume cannot be reaped while the caller holds the lock,
// so a root that is present here stays present for the rest of the critical
// section. A root that is ABSENT here was reaped by the Expire the caller was
// blocked behind, and must surface as the typed not-found signal so the
// provision path cold-materializes instead of mounting a deleted path.
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
// metadata dir writeStamp needs inside a subtree Expire is deleting, leaving an
// empty resurrected root behind. Existence is re-checked under the lock for the
// same reason Attach re-checks it: a volume reaped while this call was blocked
// must surface as ErrVolumeNotFound, not be silently resurrected as an empty
// stamped shell the reaper would then have to re-reap.
func (m *LocalManager) Stamp(ctx context.Context, v Volume, intent CloseIntent) error {
	resolved, err := m.Lookup(ctx, v.SessionID)
	if err != nil {
		return err
	}
	lock, err := lockVolume(resolved.HostRoot, true)
	if err != nil {
		return err
	}
	if lock == nil {
		// Unreachable with block=true; guarded so a future non-blocking caller
		// cannot nil-deref.
		return fmt.Errorf("vfs: session %q: volume lock unavailable", v.SessionID)
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
// The pass is SEPARATE from Expire rather than folded into it, and the ordering
// contract is: the expiry driver (W6) calls ReconcileOrphans once at startup,
// before its first Expire. Keeping them apart is what makes "unstamped means
// live" a single, honest rule inside Expire — folding the scan in would make
// every Expire pass able to stamp a volume it is simultaneously judging, and
// would make a mid-session Expire (the ticker's steady state, when unstamped
// volumes ARE live sessions) stamp live sessions closed.
//
// A volume whose lock is held (a concurrent Attach) is skipped: it is being
// attached-live, which is the opposite of orphaned. Like Expire, the pass locks
// first and reads the stamp only under the lock, so it can never stamp a volume
// closed on the strength of a read an in-flight Attach has already invalidated.
func (m *LocalManager) ReconcileOrphans(ctx context.Context) error {
	discoveredAt := time.Now()
	return m.eachVolume(ctx, func(root string) error {
		lock, err := lockVolume(root, false)
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

// stampOrphanLocked writes the discovery stamp for an orphan under the held
// per-volume lock, after confirming under that lock that the volume is still
// unstamped. An already-stamped volume is left exactly as its teardown wrote
// it: this pass exists only to make UNSTAMPED volumes reachable by the reaper,
// and rewriting a suspended session's stamp would make it reapable.
func stampOrphanLocked(root string, discoveredAt time.Time) error {
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
func (m *LocalManager) Expire(ctx context.Context, olderThan time.Duration) error {
	now := time.Now()
	return m.eachVolume(ctx, func(root string) error {
		lock, err := lockVolume(root, false)
		if err != nil {
			return err
		}
		if lock == nil {
			return nil // a live Attach holds it; skip, do not reap.
		}
		reapErr := reapLocked(root, now, olderThan)
		releaseErr := lock.release()
		return errors.Join(reapErr, releaseErr)
	})
}

// reapLocked reads the stamp under the held per-volume lock, and deletes the
// volume subtree only if that under-lock read says it is eligible. This is the
// only place a volume is destroyed (P2-GC-c), and it is reachable only with the
// lock held (invariant (b)): reading the stamp here rather than at the call
// site is what guarantees the verdict cannot be stale, because a concurrent
// Attach must hold this same lock to clear a stamp.
//
// It removes ONLY the volume root. The lock file the caller is holding is a
// sibling of that root, not a path inside it, so this removal cannot unlink the
// inode the lock lives on — which is what keeps the lock excluding a blocked
// Attach across the reap, and lets that Attach observe the absence and return
// ErrVolumeNotFound (see lockVolume and Attach).
func reapLocked(root string, now time.Time, olderThan time.Duration) error {
	stamp, err := readStamp(root)
	if err != nil {
		return err
	}
	if !eligible(stamp, now, olderThan) {
		return nil
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("vfs: reaping expired volume %q: %w", root, err)
	}
	return nil
}

// eligible reports whether a stamp makes its volume reap-eligible: it must
// exist (an unstamped volume is a live session's), carry IntentClosed exactly
// (IntentSuspended — and any unrecognized intent, which a corrupt stamp could
// carry — pins the volume rather than risking a wrong reap), and be older than
// the retention window.
func eligible(stamp *closeStamp, now time.Time, olderThan time.Duration) bool {
	if stamp == nil || stamp.Intent != IntentClosed {
		return false
	}
	return now.Sub(stamp.StampedAt) > olderThan
}

// Snapshot returns the not-implemented sentinel: the verb is reserved in
// VolumeManager, but the snapshot store, the reflink/rsync copy primitive, and
// the (AgentAccountID, repo) index the provision path reads are W2's to land
// behind this signature. Honest failure over a silent no-op — a caller must not
// read an empty VolumeSnapshotID as a stored snapshot.
func (m *LocalManager) Snapshot(ctx context.Context, v Volume) (VolumeSnapshotID, error) {
	return "", ErrSnapshotNotImplemented
}

// Archive returns the not-implemented sentinel: the verb's consumer is D4's
// cold-idle (OQ-2), so P2 freezes the signature and defers the object-store
// implementation.
func (m *LocalManager) Archive(ctx context.Context, v Volume) (ArchiveRef, error) {
	return "", ErrArchiveNotImplemented
}

// Restore returns the not-implemented sentinel, for the same reason as Archive
// (OQ-2): a caller must not read a zero Volume as a rehydrated one.
func (m *LocalManager) Restore(ctx context.Context, ref ArchiveRef) (Volume, error) {
	return Volume{}, ErrRestoreNotImplemented
}

// volumeRoot resolves a session's volume root, rejecting any session id that
// could escape the base dir or collide with the lock-file namespace. Session
// ids reaching this package are already sanitized internal ids, so this is
// defense in depth on the one operation that deletes a subtree: the base dir is
// the only place this package may ever create or reap, and a `..` or a
// separator in a session id would break that. A session id ENDING in
// lockFileSuffix is rejected too: its volume root would be another session's
// sibling lock-file path, so a reap of one would target the other's lock.
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
	return filepath.Join(m.baseDir, sessionID), nil
}

// eachVolume runs fn against every volume root under the base dir, joining
// per-volume errors instead of aborting on the first: one unreadable or locked
// volume must not pin every volume behind it. Non-directory entries in the base
// dir are skipped — the base dir holds volume subtrees, and a stray file is not
// one.
func (m *LocalManager) eachVolume(ctx context.Context, fn func(root string) error) error {
	entries, err := os.ReadDir(m.baseDir)
	if err != nil {
		return fmt.Errorf("vfs: scanning volume base dir %q: %w", m.baseDir, err)
	}
	var errs []error
	for _, entry := range entries {
		if ctxErr := ctx.Err(); ctxErr != nil {
			errs = append(errs, ctxErr)
			break
		}
		if !entry.IsDir() {
			continue
		}
		if err := fn(filepath.Join(m.baseDir, entry.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// metaDir is the volume's metadata dir, where the close-stamp lives. The
// per-volume lock file is deliberately NOT here — it is a sibling of the volume
// root (see lockVolume).
func metaDir(root string) string { return filepath.Join(root, metaDirName) }

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
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is this package's own stamp file under the operator-configured base dir, derived from a traversal-checked session id, never caller input
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

// writeStamp writes the volume's close-stamp atomically: encode, stage to a
// temp file in the same metadata dir, fsync-free rename over the target. The
// rename is what makes a concurrent reader see either the old stamp or the new
// one and never a torn record, and staging in the same dir keeps it a
// same-filesystem rename.
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
	return nil
}

// writeAndClose writes data to f, pins the owner-only mode independent of the
// Runner's umask, and closes it — reporting the first failure. The Close error
// is handled, not discarded: on a written file it can carry the flush failure
// that means the bytes never landed, which for a stamp would mean a volume the
// reaper mis-judges.
func writeAndClose(f *os.File, data []byte) error {
	name := f.Name()
	writeErr := func() error {
		if _, err := f.Write(data); err != nil {
			return fmt.Errorf("vfs: writing close stamp %q: %w", name, err)
		}
		if err := f.Chmod(stampFileMode); err != nil {
			return fmt.Errorf("vfs: pinning close stamp mode %q: %w", name, err)
		}
		return nil
	}()
	closeErr := f.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("vfs: closing close stamp %q: %w", name, closeErr)
	}
	return errors.Join(writeErr, closeErr)
}

// clearStamp removes the volume's close-stamp (invariant (a)). An absent stamp
// is already-clear, not a failure: Attach of a volume that was never stamped —
// the common case, a resume of a still-live session — must succeed.
func clearStamp(root string) error {
	path := stampPath(root)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("vfs: clearing close stamp %q: %w", path, err)
	}
	return nil
}

// volumeLock is a held per-volume advisory file lock. It owns the open fd the
// lock lives on, so it must be released exactly once (release is idempotent
// against a double call).
type volumeLock struct {
	f *os.File
}

// lockVolume acquires the volume's per-volume advisory lock (invariant (b)) on
// a dedicated lock file that is a FILE SIBLING of the volume root —
// <baseDir>/<sessionID><lockFileSuffix>, never a path inside the root. An OS
// advisory lock — not an in-process mutex — because the two contenders may be
// different processes: a restarted Runner's expiry driver and a live Runner's
// Attach, or a hand-run reaper beside the service.
//
// The placement OUTSIDE the volume root is load-bearing, not cosmetic. flock is
// a lock on an INODE, reached through an open fd, and reapLocked's
// os.RemoveAll(root) unlinks everything inside the root. With the lock file in
// the root's metadata dir, a reap would unlink the very inode a blocked Attach
// was waiting on: that Attach would then unblock holding an exclusive lock on a
// dead inode, and the next lockVolume — after a cold CreateVolume recreated the
// root — would open a NEW inode and lock that, so two actors would hold "the"
// volume lock simultaneously and mutual exclusion would be gone across a
// reap+recreate. On the sibling path the inode is stable across any number of
// reap/recreate cycles, which is what lets Attach and Stamp treat an under-lock
// os.Stat of the root as authoritative.
//
// For the same reason lockVolume creates ONLY the lock file. Its parent, the
// base dir, already exists from NewLocalManager; it must never MkdirAll
// anything inside the volume root, because acquiring a lock that RESURRECTS an
// empty metadata dir inside a reaped root would make the under-lock existence
// check see a live volume that no longer has any contents.
//
// Accepted tradeoff: a sibling lock file outlives its volume's reap — one
// empty, zero-content, owner-only file per session id ever seen on this box,
// which eachVolume skips (it iterates directories only) and which a later
// lockVolume simply reuses. That leak is the price of a stable lock inode, and
// it is the right trade: the alternative loses mutual exclusion. A future
// maintenance pass could unlink orphan lock files whose volume root is absent,
// but only while not under contention (unlinking a lock file someone holds
// recreates exactly the two-inode split described above); that pass is out of
// P2 scope.
func lockVolume(root string, block bool) (*volumeLock, error) {
	path := root + lockFileSuffix
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, stampFileMode) //nolint:gosec // G304: path is this package's own lock file under the operator-configured base dir, derived from a traversal-checked session id, never caller input
	if err != nil {
		return nil, fmt.Errorf("vfs: opening volume lock %q: %w", path, err)
	}
	how := syscall.LOCK_EX
	if !block {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		closeErr := f.Close()
		if closeErr != nil {
			closeErr = fmt.Errorf("vfs: closing volume lock %q: %w", path, closeErr)
		}
		if !block && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
			// Contended: the caller skips this volume. Report the close failure
			// if there was one — a leaked fd is a real defect — but not the
			// expected contention.
			return nil, closeErr
		}
		return nil, errors.Join(fmt.Errorf("vfs: locking volume %q: %w", path, err), closeErr)
	}
	return &volumeLock{f: f}, nil
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
