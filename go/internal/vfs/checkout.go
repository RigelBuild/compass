package vfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// CheckoutFS prepares the tree rooted in a session volume or customer mount.
type CheckoutFS struct {
	store            SnapshotStore
	vol              Volume
	root             string
	expected         TreeSource
	materializedRoot string
	active           bool
}

var _ VirtualFS = (*CheckoutFS)(nil)

// NewCheckoutFS binds tree preparation to an existing session volume.
func NewCheckoutFS(store SnapshotStore, vol Volume) (*CheckoutFS, error) {
	if store == nil {
		return nil, errors.New("vfs: snapshot store must not be nil")
	}
	if vol.HostRoot == "" {
		return nil, errors.New("vfs: volume host root must not be empty")
	}
	info, err := os.Stat(vol.HostRoot)
	if err != nil {
		return nil, fmt.Errorf("vfs: inspecting volume root %q: %w", vol.HostRoot, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("vfs: volume root %q is not a directory", vol.HostRoot)
	}
	return &CheckoutFS{store: store, vol: vol, root: vol.HostRoot}, nil
}

// Expected returns a copy of the checkout source recorded by Materialize.
func (f *CheckoutFS) Expected() TreeSource {
	expected := f.expected
	expected.Sparse = append([]string(nil), expected.Sparse...)
	return expected
}

// Materialize prepares the destination for the agent to complete in-container.
func (f *CheckoutFS) Materialize(ctx context.Context, src TreeSource) (string, error) {
	if err := validateTreeSource(src); err != nil {
		return "", err
	}
	if f.active {
		return "", ErrAlreadyMaterialized
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	root := f.root
	switch {
	case src.CustomerMount != "":
		root = src.CustomerMount
	case src.Snapshot != "":
		if err := f.restoreOrCold(ctx, src); err != nil {
			return "", err
		}
	case src.Repo != "":
		if err := os.MkdirAll(f.root, volumeDirMode); err != nil {
			return "", fmt.Errorf("vfs: preparing volume root %q: %w", f.root, err)
		}
	}

	f.expected = TreeSource{Repo: src.Repo, Ref: src.Ref, Sparse: append([]string(nil), src.Sparse...)}
	f.materializedRoot = root
	f.active = true
	return root, nil
}

// Release ends the materialized session without deleting its tree.
func (f *CheckoutFS) Release(_ context.Context, root string) error {
	if !f.active || root != f.materializedRoot {
		return ErrNotMaterialized
	}
	f.active = false
	f.materializedRoot = ""
	return nil
}

func (f *CheckoutFS) restoreOrCold(ctx context.Context, src TreeSource) error {
	err := f.store.RestoreSnapshot(ctx, src.Snapshot, f.vol)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrVolumeNotEmpty):
		// A warm reattached tree beats a snapshot; restore never overwrites volume contents.
		return nil
	case !errors.Is(err, ErrSnapshotNotFound):
		return fmt.Errorf("vfs: restoring source snapshot: %w", err)
	}
	warm, warmErr := f.hasWarmTree()
	if warmErr != nil {
		return warmErr
	}
	if warm {
		return nil
	}
	if src.Repo == "" {
		// Without a repo the agent has nothing to complete an empty root from.
		return fmt.Errorf("vfs: restoring source snapshot: %w", err)
	}
	if err := os.MkdirAll(f.root, volumeDirMode); err != nil {
		return fmt.Errorf("vfs: preparing cold volume root %q: %w", f.root, err)
	}
	return nil
}

// hasWarmTree reports a reattached tree; a half-restored root (marker present) is not warm.
func (f *CheckoutFS) hasWarmTree() (bool, error) {
	marked, err := pathExists(filepath.Join(metaDir(f.vol.HostRoot), restoreMarkerName))
	if err != nil || marked {
		return false, err
	}
	entries, err := os.ReadDir(f.vol.HostRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("vfs: reading volume root %q: %w", f.vol.HostRoot, err)
	}
	return len(entries) > 0, nil
}

func validateTreeSource(src TreeSource) error {
	if src.CustomerMount != "" {
		if src.Repo != "" || src.Snapshot != "" || len(src.Sparse) != 0 {
			return fmt.Errorf("%w: customer mount cannot be combined with repo, snapshot, or sparse paths", ErrInvalidTreeSource)
		}
		if !filepath.IsAbs(src.CustomerMount) || filepath.Clean(src.CustomerMount) != src.CustomerMount {
			return fmt.Errorf("%w: customer mount must be an absolute clean path", ErrInvalidTreeSource)
		}
		// A symlinked mount root would hand the agent whatever directory the link names.
		info, err := os.Lstat(src.CustomerMount)
		if err != nil {
			return fmt.Errorf("%w: inspecting customer mount: %w", ErrInvalidTreeSource, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: customer mount is not a directory (symlinks are rejected)", ErrInvalidTreeSource)
		}
		return nil
	}
	if src.Snapshot == "" && src.Repo == "" {
		return fmt.Errorf("%w: repo or snapshot is required", ErrInvalidTreeSource)
	}
	for _, path := range src.Sparse {
		if filepath.IsAbs(path) || filepath.Clean(path) != path || slices.Contains(strings.Split(filepath.ToSlash(path), "/"), "..") {
			return fmt.Errorf("%w: sparse path %q must be relative, clean, and not traverse parents", ErrInvalidTreeSource, path)
		}
	}
	return nil
}
