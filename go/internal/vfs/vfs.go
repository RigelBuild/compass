// Package vfs manages persistent session volumes and immutable snapshots.
// It owns the local volume lifecycle and account-scoped snapshot index.
// virtualfs.go defines the session tree materialization interface.
// checkout.go prepares local volume and snapshot destinations.
package vfs

import (
	"context"
	"errors"
	"time"
)

// ErrVolumeNotFound is returned by Lookup for a session with no volume on this
// box, and by Attach for a volume that no longer exists. It is an
// error-shaped signal, never a silent recreate: the provision path converts it
// into a fresh CreateVolume plus a cold materialize, so the cold path is
// observable. Callers detect it with errors.Is.
var ErrVolumeNotFound = errors.New("vfs: volume not found")

// ErrInvalidSessionID rejects a session id that cannot key a volume subtree.
// Session ids reaching this package are already-sanitized internal ids, so this
// is a defense-in-depth guard: a path separator or a `..` element in a session
// id would escape the base dir, and the base dir is the only subtree this
// package is ever allowed to create or reap.
var ErrInvalidSessionID = errors.New("vfs: invalid session id")

// ErrSnapshotNotFound reports a missing current snapshot or unknown ID so callers can distinguish absent state.
var ErrSnapshotNotFound = errors.New("vfs: snapshot not found")

// ErrVolumeNotEmpty prevents overwriting unmarked volume contents during restore.
var ErrVolumeNotEmpty = errors.New("vfs: volume not empty")

// ErrInvalidSnapshotKey prevents snapshot indexes from mixing account or repo scopes.
var ErrInvalidSnapshotKey = errors.New("vfs: invalid snapshot key")

// ErrArchiveNotImplemented marks the archive verb reserved for D4.
var ErrArchiveNotImplemented = errors.New("vfs: Archive is reserved at P2 and implemented in D4")

// ErrRestoreNotImplemented marks archived-volume restore reserved for D4.
var ErrRestoreNotImplemented = errors.New("vfs: Restore is reserved at P2 and implemented in D4")

// Volume is a live per-session persistent volume with a host-side root.
type Volume struct {
	SessionID string
	HostRoot  string
}

// VolumeSnapshotID is the opaque key of a stored volume snapshot (opaque per
// the parent record; never parsed by callers).
type VolumeSnapshotID string

// SnapshotKey scopes an index to one account and repo, preventing cross-scope snapshot lookup.
type SnapshotKey struct {
	AgentAccountID string
	Repo           string
}

// ArchiveRef is the opaque reference to an archived volume in the object store
// (consumed by D4's cold-idle; signature reserved here, implementation deferred —
// see OQ-2).
type ArchiveRef string

// CloseIntent is why a session's volume was last stamped: the intent bit that
// decides whether the expiry reaper may ever touch it. It comes from the
// caller — the teardown path knows whether it is closing or suspending a
// session — and is never inferred from "the container is gone", because D4's
// suspend uses the same stop+remove teardown path a close does. Without the
// caller-supplied bit, every suspended session's volume would look closed and
// be reaped one expiry window into a suspend.
//
// The zero value is IntentClosed, the reap-eligible intent. That direction is
// deliberate: a stamp written with a defaulted intent expires (a bounded
// storage leak) rather than pinning the volume forever (an unbounded one), and
// a discovered orphan — which by construction has no caller intent — wants
// exactly IntentClosed.
type CloseIntent int

const (
	// IntentClosed marks a session closed for good. Its volume becomes eligible
	// for Expire once the stamp is older than the configured retention.
	IntentClosed CloseIntent = iota
	// IntentSuspended marks a session suspended, not closed. Its volume is
	// NEVER eligible for Expire however old the stamp is — the session is
	// expected to resume onto exactly this volume, at exactly this path.
	IntentSuspended
)

// String names the intent for diagnostics. An unrecognized value renders
// visibly rather than as a bare integer.
func (i CloseIntent) String() string {
	switch i {
	case IntentClosed:
		return "closed"
	case IntentSuspended:
		return "suspended"
	default:
		return "unknown"
	}
}

// VolumeManager owns the session-volume lifecycle Runner-side, beside the
// container lifecycle. An interface so the Runner can hold a VolumeManager and
// tests can substitute a fake, and so a later network-volume backend slots in
// behind it. A backend is constructed with the operator-configured base dir
// (see NewLocalManager), so it has the placement context every verb needs
// without threading it through each call.
type VolumeManager interface {
	// CreateVolume creates the session's volume subtree and returns the
	// resolved Volume. It is idempotent: creating a volume for a session that
	// already has one returns the existing volume rather than clearing it —
	// volume destruction is Expire's alone (P2-GC-c).
	CreateVolume(ctx context.Context, sessionID string) (Volume, error)
	// Lookup resolves a session's existing volume (with its HostRoot) or
	// returns ErrVolumeNotFound. It is the "resolve" half of the provision
	// path's resolve-or-create: Attach needs a resolved Volume, so a caller
	// cannot produce one from a bare session id without this verb. A Runner
	// never resolves a volume it does not host — the box-local invariant.
	Lookup(ctx context.Context, sessionID string) (Volume, error)
	// Attach makes the resolved volume available for mounting and returns its
	// host path; it also atomically clears any close-stamp, so a reopened
	// closed-but-unexpired session never carries a past-deadline stamp into its
	// new life. The returned path depends only on the session id and the base
	// dir, which is what makes it stable across every launch (P2-GC-d).
	Attach(ctx context.Context, v Volume) (path string, err error)
	// Snapshot copies the volume without freezing the agent, so the copy can be torn by concurrent writes.
	// Callers must verify the copied tree (e.g. git-clean) before PromoteSnapshot makes it current.
	Snapshot(ctx context.Context, v Volume) (VolumeSnapshotID, error)
	// Archive moves the volume to cold object storage and returns its opaque
	// reference. Reserved at P2 (OQ-2): the backend returns
	// ErrArchiveNotImplemented until D4's cold-idle consumes it.
	Archive(ctx context.Context, v Volume) (ArchiveRef, error)
	// Restore rehydrates an archived volume and returns the live Volume.
	// Reserved at P2 (OQ-2): the backend returns ErrRestoreNotImplemented.
	Restore(ctx context.Context, ref ArchiveRef) (Volume, error)
	// Expire reaps volumes whose session is closed and whose close-stamp is
	// older than olderThan. Never touches live or suspended sessions: an
	// unstamped volume belongs to a live session and a stamp carrying
	// IntentSuspended is ineligible however old. This is the ONLY path that
	// destroys volume contents (P2-GC-c).
	Expire(ctx context.Context, olderThan time.Duration) error
	// Stamp records the caller's close-vs-suspend intent on the volume. It is
	// on the seam because Expire is only correct on a backend that can stamp.
	Stamp(ctx context.Context, v Volume, intent CloseIntent) error
	// ReconcileOrphans stamps every unstamped volume closed at discovery time.
	// Startup only: it must finish before the backend serves any Attach.
	ReconcileOrphans(ctx context.Context) error
}

// SnapshotStore separates snapshot indexing and restore from the volume lifecycle.
type SnapshotStore interface {
	// PromoteSnapshot makes a captured tree current for one account and repo.
	PromoteSnapshot(ctx context.Context, key SnapshotKey, id VolumeSnapshotID) error
	// CurrentSnapshot returns the snapshot currently promoted for a key.
	CurrentSnapshot(ctx context.Context, key SnapshotKey) (VolumeSnapshotID, error)
	// RestoreSnapshot materializes a stored tree into an empty or interrupted volume.
	RestoreSnapshot(ctx context.Context, id VolumeSnapshotID, v Volume) error
}
