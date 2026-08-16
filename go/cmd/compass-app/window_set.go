//go:build unix && gtk3

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// windowSetFileName is the state-dir file holding the ordered set of open window
// names, so a relaunch reopens the same set (Compass multi-window M1).
const windowSetFileName = "windows.json"

// defaultWindowName is the name of the single window opened on a first-ever run
// (an empty/absent/corrupt persisted set), per the multi-window M1 default.
const defaultWindowName = "bridge"

// windowSet is the on-disk record: the ordered list of open window names.
type windowSet struct {
	Windows []string `json:"windows"`
}

// windowSetPath is the state-dir path of the persisted window set.
func windowSetPath(stateDir string) string {
	return filepath.Join(stateDir, windowSetFileName)
}

// loadWindowSet reads the persisted window names in order. It degrades to the
// empty set (nil) on first-ever run (ErrNotExist) OR on any read/parse error: a
// corrupt file must never crash startup — run() then opens exactly one default
// window. Unlike the token file this holds no secret, but the same discipline
// applies: the raw file bytes are never surfaced.
func loadWindowSet(stateDir string) []string {
	raw, err := os.ReadFile(windowSetPath(stateDir))
	if err != nil {
		return nil
	}
	var set windowSet
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil
	}
	return set.Windows
}

// saveWindowSet atomically persists the ordered window names to the state dir: a
// temp file in the same directory is written and renamed over the final path, so
// a reader never observes a partial file. Mirrors tokenstore's writeAtomic
// (go/internal/tokenstore/tokenstore.go:194-229); 0o644 since this is not a
// secret. An empty/nil names still writes a valid empty-list file.
func saveWindowSet(stateDir string, names []string) error {
	if names == nil {
		names = []string{}
	}
	data, err := json.Marshal(windowSet{Windows: names})
	if err != nil {
		return fmt.Errorf("window_set: marshaling window set: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("window_set: ensuring state dir %q: %w", stateDir, err)
	}
	tmp, err := os.CreateTemp(stateDir, windowSetFileName+".*")
	if err != nil {
		return fmt.Errorf("window_set: creating temp window set file in %q: %w", stateDir, err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close() // cleanup close; the chmod error is the real failure.
		_ = os.Remove(tmpName)
		return fmt.Errorf("window_set: chmod 0644 temp window set file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("window_set: writing window set file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("window_set: syncing window set file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("window_set: closing temp window set file: %w", err)
	}
	if err := os.Rename(tmpName, windowSetPath(stateDir)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("window_set: renaming window set file into place: %w", err)
	}
	return nil
}
