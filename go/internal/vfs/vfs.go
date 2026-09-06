// Package vfs is the per-session persistent-volume seam: where a session's
// working tree and derived state (`target/`, `node_modules`, build caches) live
// so they survive suspend / resume / eviction and mount at a stable absolute
// path on every launch. Today the tree dies with the container because it
// exists only inside it (the agent self-clones post-launch into a
// container-local dir, go/internal/runtime/agent.go:354-358); this package owns
// the volume lifecycle Runner-side, beside the container lifecycle
// (docs/designs/infra/runtime/compass-elastic-session-runtime/p2-persistent-session-volume.md,
// the volume under §Approach and the lifecycle API under W1).
//
// The layering mirrors the elastic-compute seam in internal/compute:
//   - vfs.go — the VolumeManager interface plus the value types that cross it
//     (Volume, VolumeSnapshotID, ArchiveRef, CloseIntent) and the package's
//     typed errors. Every consumer depends on the interface, so a
//     network-volume backend can replace the local-directory one without
//     touching a caller.
//   - localvolume.go — the P2 backend: a directory subtree on the box's fast
//     local storage, one per session under an operator-configured base dir.
//     Vendor-neutral (Global Constraint 1), no storage fabric; the accepted
//     tradeoff — a burst cannot land on a different box until a network-volume
//     backend exists — is the parent record's OQ 2 and is not re-opened here.
//
// Two load-bearing constraints shape the surface. Volume destruction is
// **only** via Expire (P2-GC-c): Release, Teardown, eviction, crash, and failed
// launches never delete volume contents, so the sole reclaim path is the policy
// reaper. And the in-container mount path of a session's volume is identical
// across every launch, resume, and burst environment of that session
// (P2-GC-d) — a path change invalidates `target/` and sccache and is a breaking
// bug, which is why Attach returns a path derived solely from the session id
// and the base dir, never from anything per-launch.
//
// Reserved-not-implemented surface: Snapshot, Archive, and Restore are declared
// in the interface now so their consumers land no interface change, but the P2
// backend returns honest not-implemented sentinels rather than silent no-ops
// (the ErrExecStreamingNotImplemented discipline,
// go/internal/compute/compute.go:26-29; the Resize precedent,
// go/internal/runtime/podman.go:387-396). Snapshot's real body — the snapshot
// store, the reflink/rsync copy primitive, and the (account, repo) index — is
// W2's; Archive/Restore's consumer is D4's cold-idle (OQ-2).
//
// This package imports nothing outside the standard library: the volume
// lifecycle is deliberately independent of the container runtime and of the
// Server. In particular the crash-reconciliation pass needs no session query
// and by design cannot want one — the Runner, not the Server, is authoritative
// for live-session truth, and RunnerService exposes no session-query verb (see
// LocalManager.ReconcileOrphans).
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

// ErrSnapshotNotImplemented is the honest sentinel the P2 backend returns from
// the reserved Snapshot method: the verb is declared in VolumeManager now, but
// the snapshot store, the reflink/rsync copy primitive, and the
// (AgentAccountID, repo) index the provision path reads are W2's to fill in
// behind this signature. W2 replaces the sentinel body; no interface change
// lands with it.
var ErrSnapshotNotImplemented = errors.New("vfs: Snapshot store is implemented in W2")

// ErrArchiveNotImplemented is the honest sentinel the P2 backend returns from
// the reserved Archive method: the verb's consumer is D4's cold-idle (OQ-2), so
// the signature is frozen here and the object-store implementation deferred.
var ErrArchiveNotImplemented = errors.New("vfs: Archive is reserved at P2 and implemented in D4")

// ErrRestoreNotImplemented is the honest sentinel the P2 backend returns from
// the reserved Restore method: like Archive, its consumer is D4's cold-idle
// (OQ-2).
var ErrRestoreNotImplemented = errors.New("vfs: Restore is reserved at P2 and implemented in D4")

// Volume is a live per-session persistent volume: the session it belongs to and
// its host-side root. Opaque to callers beyond these fields.
type Volume struct {
	SessionID string
	HostRoot  string
}

// VolumeSnapshotID is the opaque key of a stored volume snapshot (frozen opaque
// by the parent record; never parsed by callers).
type VolumeSnapshotID string

// ArchiveRef is the opaque reference to an archived volume in the object store
// (consumed by D4's cold-idle; signature frozen here, implementation deferred —
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
	// Snapshot captures the volume's tree into the snapshot store and returns
	// its opaque key. Reserved at P2: the backend returns
	// ErrSnapshotNotImplemented until W2 lands the store, the copy primitive,
	// and the (account, repo) index.
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
}
