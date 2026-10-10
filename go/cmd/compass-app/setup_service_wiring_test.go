//go:build unix && gtk4

package main

import (
	"testing"

	"github.com/RigelBuild/compass/go/internal/appconfig"
)

func TestNewSetupServicesShareGateAndCAPicks(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("HOME", "")

	svc, setup, dialog, err := newSetupServices(t.TempDir(), "example:image")
	if err != nil {
		t.Fatalf("newSetupServices: %v", err)
	}
	if svc.setup == nil {
		t.Fatal("bridge service has no setup wiring")
	}
	if svc.tokens == nil {
		t.Fatal("bridge service has no tokenstore")
	}
	if svc.conn.Load() != nil {
		t.Error("setup bridge service has a connection, want no connection")
	}
	mode, serverURL := svc.shellState()
	if mode != "setup" || serverURL != "" {
		t.Errorf("shellState() = (%q, %q), want (setup, empty URL)", mode, serverURL)
	}
	if setup.gate != svc.setup.gate {
		t.Error("setup service and bridge service do not share the first-run gate")
	}
	if setup.gate == nil || dialog.picks == nil {
		t.Fatal("setup gate or CA picks are nil")
	}
	if dialog.picks != svc.setup.picks {
		t.Error("dialog service and bridge service do not share CA picks")
	}
	if dialog.app != nil {
		t.Error("dialog service app must be assigned after application.New")
	}
	if err := setup.save(); err != nil {
		t.Fatalf("setup save: %v", err)
	}
	cfg, err := appconfig.Load(configHome, "", "")
	if err != nil {
		t.Fatalf("load saved setup config: %v", err)
	}
	if cfg.Mode != appconfig.ModeEmbedded {
		t.Errorf("saved config mode = %s, want embedded", cfg.Mode)
	}
}
