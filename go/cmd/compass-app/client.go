//go:build (linux && gtk4) || darwin

// Native launch services: first-run opens without a connection, embedded mode
// supervises a local stack, and client mode dials a headless stack over TLS.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/RigelBuild/compass/go/internal/appconfig"
	"github.com/RigelBuild/compass/go/internal/bridge"
	"github.com/RigelBuild/compass/go/internal/tokenstore"
)

// runClient shares one TLS-anchored bridge target between the pump and service,
// so Connect's bearer reaches every forwarded RPC. It stores the remote token
// by server URL and skips a pre-window probe so slow servers cannot delay launch.
// CA or target construction failures abort launch with an error, not a panic.
func runClient(cfg appconfig.Config, stateDir string) (*bridgeService, error) {
	var caPEM []byte
	if cfg.CACert != "" {
		pem, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("reading client CA cert %q: %w", cfg.CACert, err)
		}
		caPEM = pem
	}

	target, err := bridge.NewTLSTarget(cfg.ServerURL, caPEM)
	if err != nil {
		return nil, fmt.Errorf("building client TLS target for %q: %w", cfg.ServerURL, err)
	}

	// The pump and service share this target so Connect bearers reach every RPC.
	conn := &connection{mode: cfg.Mode.String(), serverURL: cfg.ServerURL, target: target, pump: bridge.NewPump(target)}
	svc := newBridgeService(conn, nil, tokenstore.New(stateDir))
	return svc, nil
}

// resolveStateDir picks the app state directory: the --state-dir flag, else
// $COMPASS_STATE_DIR, else $XDG_STATE_HOME/compass, else $HOME/.compass. A
// relative XDG_STATE_HOME is treated as unset, so the fallback is deterministic.
func resolveStateDir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("COMPASS_STATE_DIR"); env != "" {
		return env
	}
	if stateHome := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(stateHome) {
		return filepath.Join(stateHome, "compass")
	}
	return filepath.Join(os.Getenv("HOME"), ".compass")
}
