//go:build unix && gtk3

package main

// Compass multi-window M1 window-set store gate. The real invariants are the
// round-trip (persisted N names reload in order → N windows on relaunch) and
// degrade-to-empty (absent OR corrupt file → nil set → run() opens exactly one
// default window). No test launches a real webview.

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWindowSetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := []string{"bridge", "bridge-2"}
	if err := saveWindowSet(dir, want); err != nil {
		t.Fatalf("saveWindowSet: %v", err)
	}
	got := loadWindowSet(dir)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loadWindowSet = %v, want %v", got, want)
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
