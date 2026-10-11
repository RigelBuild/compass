package vfs

// snapshot_store.go manages immutable snapshot trees and account-scoped indexes.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	snapshotStagingDir = "staging"
	snapshotTreesDir   = "trees"
	snapshotIndexDir   = "index"
	snapshotLockName   = "index"
	restoreMarkerName  = "restore-incomplete"
	snapshotIndexTemp  = "snapshot-*.json.tmp"
)

type snapshotIndexEntry struct {
	AgentAccountID string    `json:"agentAccountId"`
	Repo           string    `json:"repo"`
	SnapshotID     string    `json:"snapshotId"`
	PromotedAt     time.Time `json:"promotedAt"`
}

func (m *LocalManager) snapshotStoreDir() string {
	return filepath.Join(m.baseDir, storeDirName)
}

func (m *LocalManager) snapshotPath(id VolumeSnapshotID) string {
	return filepath.Join(m.snapshotStoreDir(), snapshotTreesDir, string(id))
}

func (m *LocalManager) snapshotIndexPath(key SnapshotKey) string {
	sum := sha256.Sum256([]byte(key.AgentAccountID + "\x00" + key.Repo))
	name := hex.EncodeToString(sum[:]) + ".json"
	return filepath.Join(m.snapshotStoreDir(), snapshotIndexDir, name)
}

func validateSnapshotKey(key SnapshotKey) error {
	if key.AgentAccountID == "" || key.Repo == "" {
		return fmt.Errorf("%w: account and repo must be non-empty", ErrInvalidSnapshotKey)
	}
	return nil
}

func validateSnapshotID(id VolumeSnapshotID) error {
	value := string(id)
	if value == "" || value == "." || value == ".." || strings.ContainsRune(value, '/') || strings.ContainsRune(value, 0) {
		return fmt.Errorf("%w: invalid snapshot id %q", ErrSnapshotNotFound, value)
	}
	return nil
}

func (m *LocalManager) storeLock(ctx context.Context) (*volumeLock, error) {
	return lockVolume(ctx, filepath.Join(m.snapshotStoreDir(), snapshotLockName))
}

// Snapshot captures an immutable volume tree for later promotion or restore.
func (m *LocalManager) Snapshot(ctx context.Context, v Volume) (VolumeSnapshotID, error) {
	resolved, err := m.Lookup(ctx, v.SessionID)
	if err != nil {
		return "", err
	}
	lock, err := lockVolume(ctx, resolved.HostRoot)
	if err != nil {
		return "", err
	}
	id, snapshotErr := m.snapshotLocked(ctx, resolved)
	return id, errors.Join(snapshotErr, lock.release())
}

func (m *LocalManager) snapshotLocked(ctx context.Context, v Volume) (id VolumeSnapshotID, resultErr error) {
	if err := requireVolumeRoot(v.HostRoot, v.SessionID); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("vfs: generating snapshot id: %w", err)
	}
	id = VolumeSnapshotID(hex.EncodeToString(random[:]))
	staging := filepath.Join(m.snapshotStoreDir(), snapshotStagingDir, string(id))
	tree := m.snapshotPath(id)
	lock, err := lockVolume(ctx, staging)
	if err != nil {
		return "", err
	}
	defer func() {
		resultErr = errors.Join(resultErr, removeSnapshotLockLocked(staging), lock.release())
	}()
	if err := os.Mkdir(staging, volumeDirMode); err != nil { //nolint:gosec // staging path is built from the fixed store root and a cryptographic snapshot ID
		return "", fmt.Errorf("vfs: creating snapshot staging tree %q: %w", staging, err)
	}
	if err := m.cloner.cloneTree(ctx, v.HostRoot, staging); err != nil {
		return "", errors.Join(fmt.Errorf("vfs: cloning volume %q to snapshot %q: %w", v.HostRoot, id, err), removeSnapshotPath(staging))
	}
	if err := syncTree(staging); err != nil {
		return "", errors.Join(fmt.Errorf("vfs: syncing snapshot staging tree %q: %w", id, err), removeSnapshotPath(staging))
	}
	if err := os.Rename(staging, tree); err != nil { //nolint:gosec // both paths use fixed store directories and a cryptographic snapshot ID
		return "", errors.Join(fmt.Errorf("vfs: committing snapshot %q: %w", id, err), removeSnapshotPath(staging))
	}
	now := time.Now()
	if err := os.Chtimes(tree, now, now); err != nil {
		return "", errors.Join(fmt.Errorf("vfs: refreshing snapshot tree time %q: %w", id, err), removeSnapshotPath(tree))
	}
	if err := syncDir(filepath.Dir(tree)); err != nil {
		return "", errors.Join(err, removeSnapshotPath(tree))
	}
	return id, nil
}

// PromoteSnapshot updates the current snapshot for one account and repo.
func (m *LocalManager) PromoteSnapshot(ctx context.Context, key SnapshotKey, id VolumeSnapshotID) error {
	if err := validateSnapshotKey(key); err != nil {
		return err
	}
	if err := validateSnapshotID(id); err != nil {
		return err
	}
	lock, err := m.storeLock(ctx)
	if err != nil {
		return err
	}
	promoteErr := m.promoteSnapshotLocked(key, id)
	return errors.Join(promoteErr, lock.release())
}

func (m *LocalManager) promoteSnapshotLocked(key SnapshotKey, id VolumeSnapshotID) error {
	info, err := os.Stat(m.snapshotPath(id))
	if errors.Is(err, os.ErrNotExist) || (err == nil && !info.IsDir()) {
		return fmt.Errorf("vfs: snapshot %q: %w", id, ErrSnapshotNotFound)
	}
	if err != nil {
		return fmt.Errorf("vfs: inspecting snapshot %q: %w", id, err)
	}
	indexPath := m.snapshotIndexPath(key)
	old, err := readSnapshotIndex(indexPath)
	if errors.Is(err, ErrSnapshotNotFound) {
		old = nil
	} else if err != nil {
		return err
	}
	if old != nil && old.AgentAccountID == key.AgentAccountID && old.Repo == key.Repo && old.SnapshotID == string(id) {
		return nil
	}
	entry := snapshotIndexEntry{AgentAccountID: key.AgentAccountID, Repo: key.Repo, SnapshotID: string(id), PromotedAt: time.Now()}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("vfs: encoding snapshot index entry: %w", err)
	}
	dir := filepath.Dir(indexPath)
	tmp, err := os.CreateTemp(dir, snapshotIndexTemp)
	if err != nil {
		return fmt.Errorf("vfs: staging snapshot index entry: %w", err)
	}
	tmpPath := tmp.Name()
	if err := writeAndClose(tmp, data); err != nil {
		return errors.Join(err, removeSnapshotPath(tmpPath))
	}
	if err := os.Rename(tmpPath, indexPath); err != nil {
		return errors.Join(fmt.Errorf("vfs: committing snapshot index entry: %w", err), removeSnapshotPath(tmpPath))
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	if old != nil && old.SnapshotID != string(id) {
		return m.removeTreeIfUnreferenced(old.SnapshotID)
	}
	return nil
}

// CurrentSnapshot returns the current snapshot for an account and repo.
func (m *LocalManager) CurrentSnapshot(ctx context.Context, key SnapshotKey) (id VolumeSnapshotID, resultErr error) {
	if err := validateSnapshotKey(key); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	lock, err := m.storeLock(ctx)
	if err != nil {
		return "", err
	}
	defer func() {
		resultErr = errors.Join(resultErr, lock.release())
	}()
	entry, err := readSnapshotIndex(m.snapshotIndexPath(key))
	if err != nil {
		return "", err
	}
	if entry == nil || entry.AgentAccountID != key.AgentAccountID || entry.Repo != key.Repo {
		return "", fmt.Errorf("vfs: current snapshot for account %q repo %q: %w", key.AgentAccountID, key.Repo, ErrSnapshotNotFound)
	}
	id = VolumeSnapshotID(entry.SnapshotID)
	if validateSnapshotID(id) != nil {
		return "", fmt.Errorf("vfs: invalid indexed snapshot id: %w", ErrSnapshotNotFound)
	}
	info, err := os.Stat(m.snapshotPath(id))
	if errors.Is(err, os.ErrNotExist) || (err == nil && !info.IsDir()) {
		return "", fmt.Errorf("vfs: indexed snapshot %q: %w", id, ErrSnapshotNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("vfs: inspecting indexed snapshot %q: %w", id, err)
	}
	return id, nil
}

// RestoreSnapshot restores the id from CurrentSnapshot for this session's account; the index key owns that boundary.
// A superseded id returns ErrSnapshotNotFound, so the caller takes the cold path.
func (m *LocalManager) RestoreSnapshot(ctx context.Context, id VolumeSnapshotID, v Volume) error {
	if err := validateSnapshotID(id); err != nil {
		return err
	}
	resolved, err := m.Lookup(ctx, v.SessionID)
	if err != nil {
		return err
	}
	volumeLock, err := lockVolume(ctx, resolved.HostRoot)
	if err != nil {
		return err
	}
	if err := requireVolumeRoot(resolved.HostRoot, resolved.SessionID); err != nil {
		return errors.Join(err, volumeLock.release())
	}
	storeLock, err := m.storeLock(ctx)
	if err != nil {
		return errors.Join(err, volumeLock.release())
	}
	restoreErr := m.restoreSnapshotLocked(ctx, id, resolved)
	return errors.Join(restoreErr, storeLock.release(), volumeLock.release())
}

func (m *LocalManager) restoreSnapshotLocked(ctx context.Context, id VolumeSnapshotID, v Volume) error {
	tree := m.snapshotPath(id)
	info, err := os.Stat(tree) //nolint:gosec // tree is derived from a validated snapshot ID below the fixed store root
	if errors.Is(err, os.ErrNotExist) || (err == nil && !info.IsDir()) {
		return fmt.Errorf("vfs: snapshot %q: %w", id, ErrSnapshotNotFound)
	}
	if err != nil {
		return fmt.Errorf("vfs: inspecting snapshot %q: %w", id, err)
	}
	marker := filepath.Join(metaDir(v.HostRoot), restoreMarkerName)
	marked, err := pathExists(marker)
	if err != nil {
		return err
	}
	if marked {
		if err := clearDirectory(v.HostRoot); err != nil {
			return err
		}
	} else {
		entries, err := os.ReadDir(v.HostRoot)
		if err != nil {
			return fmt.Errorf("vfs: reading volume root %q: %w", v.HostRoot, err)
		}
		if len(entries) != 0 {
			return fmt.Errorf("vfs: restoring snapshot into %q: %w", v.HostRoot, ErrVolumeNotEmpty)
		}
	}
	if err := writeRestoreMarker(marker); err != nil {
		return err
	}
	if err := m.cloner.cloneTree(ctx, tree, v.HostRoot); err != nil {
		cleanupErr := clearDirectory(v.HostRoot)
		if cleanupErr == nil {
			cleanupErr = removeSnapshotPath(marker)
		}
		cleanupErr = errors.Join(cleanupErr, syncDir(filepath.Dir(marker)))
		return errors.Join(fmt.Errorf("vfs: restoring snapshot %q into %q: %w", id, v.HostRoot, err), cleanupErr)
	}
	if err := syncTree(v.HostRoot); err != nil {
		return fmt.Errorf("vfs: syncing restored volume %q: %w", v.HostRoot, err)
	}
	if err := os.Remove(marker); err != nil {
		return fmt.Errorf("vfs: clearing restore marker %q: %w", marker, err)
	}
	return syncDir(filepath.Dir(marker))
}

func writeRestoreMarker(path string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "restore-*.tmp")
	if err != nil {
		return fmt.Errorf("vfs: staging restore marker: %w", err)
	}
	tmpPath := tmp.Name()
	if err := writeAndClose(tmp, []byte("incomplete")); err != nil {
		return errors.Join(err, removeSnapshotPath(tmpPath))
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return errors.Join(fmt.Errorf("vfs: committing restore marker: %w", err), removeSnapshotPath(tmpPath))
	}
	return syncDir(dir)
}

func clearDirectory(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("vfs: listing directory %q: %w", dir, err)
	}
	var errs []error
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("vfs: removing directory entry %q: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

func readSnapshotIndex(path string) (result *snapshotIndexEntry, resultErr error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("vfs: snapshot index %q: %w", path, ErrSnapshotNotFound)
		}
		return nil, fmt.Errorf("vfs: opening snapshot index root %q: %w", filepath.Dir(path), err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, root.Close())
	}()
	data, err := root.ReadFile(filepath.Base(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("vfs: snapshot index %q: %w", path, ErrSnapshotNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("vfs: reading snapshot index %q: %w", path, err)
	}
	var entry snapshotIndexEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, fmt.Errorf("vfs: decoding snapshot index %q: %w", path, err)
	}
	return &entry, nil
}

func (m *LocalManager) removeTreeIfUnreferenced(id string) error {
	if err := validateSnapshotID(VolumeSnapshotID(id)); err != nil {
		return fmt.Errorf("vfs: invalid superseded snapshot ID: %w", err)
	}
	referenced, err := m.referencedSnapshots()
	if err != nil {
		return err
	}
	if referenced[id] {
		return nil
	}
	path := m.snapshotPath(VolumeSnapshotID(id))
	if err := removeSnapshotPath(path); err != nil {
		return fmt.Errorf("vfs: removing superseded snapshot %q: %w", id, err)
	}
	return syncDir(filepath.Dir(path))
}

func (m *LocalManager) referencedSnapshots() (map[string]bool, error) {
	dir := filepath.Join(m.snapshotStoreDir(), snapshotIndexDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("vfs: scanning snapshot index %q: %w", dir, err)
	}
	referenced := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		indexed, err := readSnapshotIndex(path)
		if err != nil {
			return nil, err
		}
		if indexed != nil && validateSnapshotID(VolumeSnapshotID(indexed.SnapshotID)) == nil {
			referenced[indexed.SnapshotID] = true
		}
	}
	return referenced, nil
}

func (m *LocalManager) sweepSnapshots(ctx context.Context, olderThan time.Duration) error {
	lock, err := m.storeLock(ctx)
	if err != nil {
		return err
	}
	sweepErr := m.sweepSnapshotsLocked(ctx, time.Now(), olderThan)
	return errors.Join(sweepErr, lock.release())
}

func (m *LocalManager) sweepSnapshotsLocked(ctx context.Context, now time.Time, olderThan time.Duration) error {
	var errs []error
	errs = append(errs, m.sweepStaging(ctx, now, olderThan))
	referenced, err := m.referencedSnapshots()
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	errs = append(errs, m.sweepSnapshotTrees(ctx, now, olderThan, referenced))
	return errors.Join(errs...)
}

func (m *LocalManager) sweepStaging(ctx context.Context, now time.Time, olderThan time.Duration) error {
	staging := filepath.Join(m.snapshotStoreDir(), snapshotStagingDir)
	entries, err := os.ReadDir(staging)
	if err != nil {
		return fmt.Errorf("vfs: scanning snapshot staging dir %q: %w", staging, err)
	}
	var errs []error
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), lockFileSuffix) {
			continue
		}
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		path := filepath.Join(staging, entry.Name())
		errs = append(errs, sweepStagingEntry(ctx, path, now, olderThan))
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), lockFileSuffix) {
			continue
		}
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		path := filepath.Join(staging, entry.Name())
		errs = append(errs, sweepOrphanStagingLock(ctx, path))
	}
	return errors.Join(errs...)
}

func sweepStagingEntry(ctx context.Context, path string, now time.Time, olderThan time.Duration) error {
	lock, err := tryLockVolume(ctx, path)
	if err != nil || lock == nil {
		return err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return lock.release()
	}
	if err != nil {
		return errors.Join(fmt.Errorf("vfs: inspecting staged snapshot %q: %w", path, err), lock.release())
	}
	if now.Sub(info.ModTime()) <= olderThan {
		return lock.release()
	}
	if err := removeSnapshotPath(path); err != nil {
		return errors.Join(fmt.Errorf("vfs: sweeping staged snapshot %q: %w", path, err), lock.release())
	}
	return errors.Join(removeSnapshotLockLocked(path), lock.release())
}

func sweepOrphanStagingLock(ctx context.Context, path string) error {
	root := strings.TrimSuffix(path, lockFileSuffix)
	lock, err := tryLockVolume(ctx, root)
	if err != nil || lock == nil {
		return err
	}
	_, err = os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		err = removeSnapshotLockLocked(root)
	} else if err != nil {
		err = fmt.Errorf("vfs: inspecting staged snapshot for orphan lock %q: %w", root, err)
	}
	return errors.Join(err, lock.release())
}

func (m *LocalManager) sweepSnapshotTrees(ctx context.Context, now time.Time, olderThan time.Duration, referenced map[string]bool) error {
	trees := filepath.Join(m.snapshotStoreDir(), snapshotTreesDir)
	entries, err := os.ReadDir(trees)
	if err != nil {
		return fmt.Errorf("vfs: scanning snapshot trees %q: %w", trees, err)
	}
	var errs []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if referenced[entry.Name()] {
			continue
		}
		path := filepath.Join(trees, entry.Name())
		info, err := os.Stat(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("vfs: inspecting snapshot tree %q: %w", path, err))
			continue
		}
		if now.Sub(info.ModTime()) <= olderThan {
			continue
		}
		if err := removeSnapshotPath(path); err != nil {
			errs = append(errs, fmt.Errorf("vfs: sweeping snapshot tree %q: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

func removeSnapshotLockLocked(root string) error {
	rootExists, err := pathExists(root)
	if err != nil {
		return err
	}
	if rootExists {
		return nil
	}
	path := root + lockFileSuffix
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) { //nolint:gosec // path is the lock sibling of an internal store directory.
		return fmt.Errorf("vfs: removing snapshot staging lock %q: %w", path, err)
	}
	return nil
}

func removeSnapshotPath(path string) error {
	return os.RemoveAll(path) //nolint:gosec // callers pass only store paths derived from validated snapshot IDs or ReadDir entries
}
