// Package vfs is the source-of-tree seam: the one place the Runner turns a
// requested repo tree into a filesystem path an agent session can work in. A
// provision request names a tree (a repo at a ref, optionally sparse; later a
// volume snapshot or a mounted customer VFS), and VirtualFS materializes it to a
// root path and, when the session ends, releases it
// (docs/designs/platform/compass-elastic-session-runtime/design.md §S1).
//
// The materialization DESTINATION is binding state of the VirtualFS instance,
// not a Materialize parameter: an instance is constructed with the target root
// it materializes into. Today that root is the clone-dir workspace (the trivial
// fused configuration); a later phase swaps it for a session-scoped persistent
// volume behind this same frozen signature, so no caller changes when the
// destination does.
//
// The layering mirrors the container-runtime seam (internal/runtime):
//   - virtualfs.go — the VirtualFS interface plus the frozen TreeSource /
//     VolumeSnapshotID types. Callers depend only on the interface, so a
//     volume-backed backend can replace the checkout one without touching them.
//   - checkout.go — GitCheckout, the S1 backend: a plain git checkout (with
//     git sparse-checkout as a parameter of the same backend, not a second
//     one) into the instance's root. The only place a subprocess is spawned.
//
// A TreeSource selecting a snapshot or a customer mount is frozen in the type
// but not implemented at S1: such a source returns ErrSourceNotImplemented
// rather than silently succeeding, so a caller reaching for an unbuilt tree
// learns the truth instead of getting an empty root.
package vfs

import "context"

// VirtualFS is the source-of-tree seam. An implementation materializes a
// requested tree to a filesystem root and releases it when the session is done.
// An interface so the Runner can hold a VirtualFS and tests can substitute a
// fake.
type VirtualFS interface {
	// Materialize realizes src as a filesystem tree and returns its root path.
	// The destination is the instance's binding state (its configured root), so
	// src carries only WHAT tree to build, never WHERE.
	Materialize(ctx context.Context, src TreeSource) (root string, err error)
	// Release removes a tree previously returned by Materialize.
	Release(ctx context.Context, root string) error
}

// TreeSource selects the tree to materialize. The fields are a discriminated
// choice: a checkout (Repo + Ref, optionally Sparse) at S1, or a volume
// snapshot or a mounted customer VFS in a later phase.
type TreeSource struct {
	// Repo is the git remote to check out (any git-understood location,
	// including a file:// path).
	Repo string
	// Ref is the ref (branch, tag, or commit) to check out.
	Ref string
	// Sparse lists sparse-checkout paths; empty means a full checkout. When
	// non-empty the checkout backend restricts the working tree to these paths.
	Sparse []string
	// Snapshot selects a volume snapshot as the source. Opaque at S1 and not
	// yet implemented — a source selecting it returns ErrSourceNotImplemented.
	Snapshot VolumeSnapshotID
	// CustomerMount is a mounted customer VFS path to use as the source. Not
	// yet implemented — a source selecting it returns ErrSourceNotImplemented.
	CustomerMount string
}

// VolumeSnapshotID identifies a volume snapshot. Frozen as an opaque string at
// S1; its production shape belongs to the phase that implements volume-backed
// materialization.
type VolumeSnapshotID string

// String returns the raw snapshot id.
func (v VolumeSnapshotID) String() string { return string(v) }
