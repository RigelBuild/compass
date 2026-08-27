//go:build unix && gtk4

package main

// Compass multi-window M1 window-set store gate. The real invariants are the
// round-trip (persisted N names reload as N windows on relaunch, canonically
// sorted — the contract is a SET, not an order, §A2), degrade-to-empty (absent
// OR corrupt file → nil set → run() opens exactly one default window), and the
// empty→default substitution that decides the window count. No test launches a
// real webview.

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWindowSetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// Feed unsorted input: the store persists a SET canonically sorted, so the
	// reload is deterministic regardless of the caller's (map-derived) order.
	if err := saveWindowSet(dir, []string{"bridge-2", "bridge"}); err != nil {
		t.Fatalf("saveWindowSet: %v", err)
	}
	got := loadWindowSet(dir)
	want := []string{"bridge", "bridge-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loadWindowSet = %v, want %v (canonically sorted)", got, want)
	}
}

func TestLoadWindowSetAbsentFile(t *testing.T) {
	// Fresh temp dir with no windows.json → first-ever run → empty set.
	if got := loadWindowSet(t.TempDir()); len(got) != 0 {
		t.Fatalf("loadWindowSet on absent file = %v, want empty", got)
	}
}

func TestLoadWindowSetCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, windowSetFileName), []byte("{ garbage"), 0o644); err != nil {
		t.Fatalf("seeding corrupt file: %v", err)
	}
	// A corrupt file must degrade to the empty set, never crash startup.
	if got := loadWindowSet(dir); len(got) != 0 {
		t.Fatalf("loadWindowSet on corrupt file = %v, want empty", got)
	}
}

func TestSaveWindowSetEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := saveWindowSet(dir, nil); err != nil {
		t.Fatalf("saveWindowSet(nil): %v", err)
	}
	// An empty set writes a valid empty-list file that reloads as empty.
	if got := loadWindowSet(dir); len(got) != 0 {
		t.Fatalf("loadWindowSet after empty save = %v, want empty", got)
	}
}

func TestWindowOptions(t *testing.T) {
	opts := windowOptions("bridge", "Compass", "globalThis.x=1")
	if opts.Name != "bridge" {
		t.Errorf("Name = %q, want %q", opts.Name, "bridge")
	}
	if opts.Title != "Compass" {
		t.Errorf("Title = %q, want %q", opts.Title, "Compass")
	}
	if opts.URL != "/" {
		t.Errorf("URL = %q, want %q (every window is a Bridge window)", opts.URL, "/")
	}
	if opts.JS != "globalThis.x=1" {
		t.Errorf("JS = %q, want the injected startup script unchanged", opts.JS)
	}
}

func TestWindowNamesOrDefault(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil set opens one default window", nil, []string{defaultWindowName}},
		{"empty set opens one default window", []string{}, []string{defaultWindowName}},
		{"populated set passes through", []string{"bridge", "bridge-2"}, []string{"bridge", "bridge-2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := windowNamesOrDefault(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("windowNamesOrDefault(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
