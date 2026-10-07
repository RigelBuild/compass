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
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/appconfig"
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

// TestWindowOptions pins the Bridge window shape and that each window's startup
// globals come from live shell state, not a script captured at launch.
func TestWindowOptions(t *testing.T) {
	svc := newSetupBridgeService(nil, nil, &setupWiring{gate: &firstRunGate{}, picks: &caPicks{}})
	opts, err := windowOptions(svc, "bridge", "Compass")
	if err != nil {
		t.Fatalf("windowOptions in setup: %v", err)
	}
	if opts.Name != "bridge" {
		t.Errorf("Name = %q, want %q", opts.Name, "bridge")
	}
	if opts.Title != "Compass" {
		t.Errorf("Title = %q, want %q", opts.Title, "Compass")
	}
	if opts.URL != "/" {
		t.Errorf("URL = %q, want %q (every window is a Bridge window)", opts.URL, "/")
	}
	if !strings.Contains(opts.JS, `window.__COMPASS_MODE__="setup";`) || strings.Contains(opts.JS, "__COMPASS_SERVER_URL__") {
		t.Fatalf("setup JS = %q, want setup mode and no URL global", opts.JS)
	}

	const serverURL = "https://live.example:8443"
	svc.conn.Store(&connection{mode: appconfig.ModeClient.String(), serverURL: serverURL})
	opts, err = windowOptions(svc, "bridge-2", "Compass")
	if err != nil {
		t.Fatalf("windowOptions after client install: %v", err)
	}
	if !strings.Contains(opts.JS, `window.__COMPASS_SERVER_URL__="`+serverURL+`";`) {
		t.Errorf("client JS = %q, want current server URL %q", opts.JS, serverURL)
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
