//go:build (linux && gtk4) || darwin

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// windowSetFileName is the state-dir file holding the persisted set of open
// window names, so a relaunch reopens the same set (Compass multi-window M1).
const windowSetFileName = "windows.json"

// defaultWindowName is the name of the single window opened on a first-ever run
// (an empty/absent/corrupt persisted set), per the multi-window M1 default.
const defaultWindowName = "bridge"

// windowSet is the on-disk record: the persisted set of open window names.
type windowSet struct {
	Windows []string `json:"windows"`
}

// windowSetPath is the state-dir path of the persisted window set.
func windowSetPath(stateDir string) string {
	return filepath.Join(stateDir, windowSetFileName)
}

// loadWindowSet treats missing or corrupt state as empty. Persisted names form
// a set, and raw bytes are never surfaced.
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

// saveWindowSet writes atomically and sorts names for stable output. Mode 0644
// is safe because window names are not secret.
func saveWindowSet(stateDir string, names []string) error {
	names = slices.Clone(names)
	slices.Sort(names)
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

// windowNamesOrDefault returns the default window for an empty set, or passes
// populated names through. This makes restore-count selection testable without
// a display.
func windowNamesOrDefault(names []string) []string {
	if len(names) == 0 {
		return []string{defaultWindowName}
	}
	return names
}
