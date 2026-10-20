//go:build (linux && gtk4) || darwin

package main

import (
	"bytes"
	"image/png"
	"testing"
)

// TestAppOptionsCarryIdentity: the Dock icon on darwin comes from Options.Icon, and
// a non-empty Linux.ApplicationID would rename prgname and move WebKitGTK's data.
func TestAppOptionsCarryIdentity(t *testing.T) {
	opts := appOptions()
	if opts.Name != "Compass" {
		t.Errorf("Name = %q, want Compass", opts.Name)
	}
	if opts.Linux.ApplicationID != "" {
		t.Errorf("Linux.ApplicationID = %q, want unset", opts.Linux.ApplicationID)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(opts.Icon))
	if err != nil {
		t.Fatalf("Icon is not a PNG: %v", err)
	}
	if cfg.Width != 1024 || cfg.Height != 1024 {
		t.Errorf("Icon is %dx%d, want 1024x1024", cfg.Width, cfg.Height)
	}
}
