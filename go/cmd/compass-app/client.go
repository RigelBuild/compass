//go:build (linux && gtk4) || darwin

// The native-client launch: the app dials a headless Compass stack over the
// authenticated TLS door (design §T5.6). Embedded mode — the in-process stack
// supervisor — was retired in RIG-2554, so this is the only launch arm: there
// is no stack to spawn, monitor, or tear down.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/RigelBuild/compass/go/internal/appconfig"
	"github.com/RigelBuild/compass/go/internal/bridge"
	"github.com/RigelBuild/compass/go/internal/tokenstore"
)

// runClient wires the native-client launch: it builds the ONE TLS-anchored
// bridge target (design §T5.6) and hands that SAME *bridge.Target to BOTH the
// pump and the bridge service, so an armed bearer (Connect → target.SetBearer)
// reaches every forwarded RPC (CompassRPC → pump → target.client.Do). The
// tokenstore persists the remote bearer keyed by server URL. accountID is left
// empty (client identity is UI-resolved, OQ-7). There is NO pre-window probe —
// the single auto-connect is the UI's boot-time shellConnect("") (T5.5) — so a
// slow/unreachable remote never wedges launch.
//
// A CA read/parse failure (bad ca_cert path or contents) or a NewTLSTarget
// failure returns a legible error that aborts launch; per rule://go-no-panic-in-lib
// this path never panics.
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

	// The ONE-target invariant: the pump and the service MUST share this single
	// *bridge.Target instance, or a bearer armed on the service's target never
	// reaches the pump's forwarded requests.
	svc := newBridgeService(bridge.NewPump(target), nil, target, tokenstore.New(stateDir))
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
