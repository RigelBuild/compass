//go:build (linux && gtk3) || darwin

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

// loadWindowSet reads the persisted window names. It degrades to the empty set
// (nil) on first-ever run (ErrNotExist) OR on any read/parse error: a corrupt
// file must never crash startup — run() then opens exactly one default window.
// The persisted contract is a SET (window identities/count, design §A2); order
// carries no meaning since every window is an identical Bridge window, so the
// names are stored canonically sorted (see saveWindowSet) purely for a stable,
// diffable file. Unlike the token file this holds no secret, but the same
// discipline applies: the raw file bytes are never surfaced.
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

// saveWindowSet atomically persists the window-name set to the state dir: a temp
// file in the same directory is written and renamed over the final path, so a
// reader never observes a partial file. Mirrors writeTokenFile
// (go/server/network_door.go); 0o644 since this is not a secret. The names are
// sorted before marshaling so the file is canonical and deterministic — the
// caller (the OnShutdown hook) derives them from app.Window.GetAll(), whose map
// iteration order is randomized; the persisted contract is a set, not an order,
// so sorting loses nothing. An empty/nil names still writes a valid empty file.
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

// windowNamesOrDefault substitutes the single default window when the persisted
// set is empty or absent: a first-ever run (or an empty/corrupt file, which
// loadWindowSet degrades to nil) opens exactly one defaultWindowName window,
// while a populated set passes through unchanged. Factored out of run()'s
// restore loop so the branch that decides the window count is unit-testable
// without a display.
func windowNamesOrDefault(names []string) []string {
	if len(names) == 0 {
		return []string{defaultWindowName}
	}
	return names
}
