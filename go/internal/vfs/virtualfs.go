package vfs

import (
	"context"
	"errors"
)

// TreeSource selects where a session's tree comes from.
type TreeSource struct {
	Repo          string           // forge clone URL (empty with Snapshot/CustomerMount)
	Ref           string           // branch/commit to check out
	Sparse        []string         // non-empty => git sparse-checkout paths
	Snapshot      VolumeSnapshotID // non-empty => restore this snapshot
	CustomerMount string           // non-empty => interop: tree pre-mounted here
}

// VirtualFS prepares its construction-time destination from a snapshot or as an empty owned root.
// The agent completes the tree in-container, so an empty directory is success.
// The destination is set at construction and is never passed to Materialize.
// Implementations hold per-session state; callers serialize calls on one instance.
type VirtualFS interface {
	Materialize(ctx context.Context, src TreeSource) (root string, err error)
	Release(ctx context.Context, root string) error
}

// ErrInvalidTreeSource reports a source that does not select a valid materialization mode.
// ErrAlreadyMaterialized reports a second Materialize call before Release.
// ErrNotMaterialized reports Release without a matching active materialization.
var (
	ErrInvalidTreeSource   = errors.New("vfs: invalid tree source")
	ErrAlreadyMaterialized = errors.New("vfs: already materialized")
	ErrNotMaterialized     = errors.New("vfs: not materialized")
)
