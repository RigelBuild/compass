//go:build unix

package stack

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"time"
)

const (
	// GatewayTokenMountPath is the read-only in-container gateway token path.
	GatewayTokenMountPath = "/run/compass/gateway.token" //nolint:gosec // G101: a mount path, not a credential
	gatewayStopTimeout    = 25 * time.Second
	gatewayRestartRetries = 5
	// gatewayHostEndpoint avoids 4000, the common local LLM-proxy port; the container binds 4000.
	gatewayHostEndpoint = "127.0.0.1:4100"
)

// GatewayContainerSpec resolves the gateway run arguments and readiness target.
type GatewayContainerSpec struct {
	Name           string
	Image          string
	TokenDir       string
	TokenFile      string
	Endpoint       string
	HealthEndpoint string
	Env            map[string]string
	StopTimeout    time.Duration
	RestartRetries int
}

func gatewayContainerSpec(cfg Config) (GatewayContainerSpec, error) {
	if cfg.StateDir == "" {
		return GatewayContainerSpec{}, errors.New("gateway container: StateDir is required")
	}
	if cfg.GatewayImage == "" {
		return GatewayContainerSpec{}, errors.New("gateway container: image is required; pass --gateway-image or --gateway-external")
	}
	tokenDir := filepath.Join(cfg.StateDir, "gateway")
	return GatewayContainerSpec{
		Name: gatewayContainerName(cfg.StateDir), Image: cfg.GatewayImage,
		TokenDir: tokenDir, TokenFile: filepath.Join(tokenDir, "gateway.token"),
		Endpoint: gatewayHostEndpoint, HealthEndpoint: gatewayHostEndpoint,
		Env:         map[string]string{"COMPASS_GATEWAY_BIND": "0.0.0.0:4000", "COMPASS_GATEWAY_DRAIN_MS": "20000", "COMPASS_GATEWAY_TOKEN_FILE": GatewayTokenMountPath},
		StopTimeout: gatewayStopTimeout, RestartRetries: gatewayRestartRetries,
	}, nil
}

func gatewayContainerName(stateDir string) string {
	h := sha256.Sum256([]byte(filepath.Clean(stateDir)))
	return "compass-gateway-" + hex.EncodeToString(h[:6])
}
