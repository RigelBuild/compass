//go:build unix

package stack

import (
	"fmt"
	"strings"
)

// tokenEnvVar is the environment variable the runner reads its enrollment token
// from. It is passed via env only, never a flag, so it never reaches the process
// table (cmd/compass-runner/main.go:105-110).
const tokenEnvVar = "COMPASS_RUNNER_TOKEN"

// embeddedRunnerID is the fixed identity of the single embedded runner. Embedded
// mode is single-user/single-runner by design (DL-106), so the id is an internal
// constant rather than a Config knob; it is cross-checked against the minted
// token's subject, so mint and spawn must agree on this one value (mirrors
// devenv's fixed `--runner-id dogfood`, devenv.nix:278).
const embeddedRunnerID = "embedded"

// serverSpec builds the compass-server child spec from the resolved config and
// cert paths, mirroring the devenv dogfood invocation (devenv.nix:199-205):
// --socket / --database / --listen / --tls-cert / --tls-key.
func serverSpec(cfg Config, cert CertResult) ProcessSpec {
	return ProcessSpec{
		Component: ComponentServer,
		Args: []string{
			"--socket", cfg.SocketPath,
			"--database", cfg.DatabaseDSN,
			"--listen", cfg.ListenAddr,
			"--tls-cert", cert.CertPath,
			"--tls-key", cert.KeyPath,
		},
	}
}

// runnerSpec builds the compass-runner child spec (devenv.nix:277-282): it dials
// the server's TLS door over https, trusts the same cert as its --ca anchor,
// runs cfg.AgentImage, and mints per-container sockets under cfg.RuntimeDir. The
// token rides in Env only.
func runnerSpec(cfg Config, cert CertResult, token string) ProcessSpec {
	// The five unconditional flags every runner spawn carries. The two A4 flags
	// below are appended only when set, so a caller that leaves both zero (the
	// embedded supervisor, the compass-stack CLI's resolveConfig) gets a
	// byte-identical Args to before this feature existed.
	args := []string{
		"--runner-id", embeddedRunnerID,
		"--server", "https://" + cfg.ListenAddr,
		"--ca", cert.CertPath,
		"--image", cfg.AgentImage,
		"--runtime-dir", cfg.RuntimeDir,
	}
	// AgentModel: forward a single --agent-model only when pinned. Forwarding
	// --agent-model "" would break an embedded supervisor that relies on the
	// runner's own default, so an empty selector must omit the flag entirely.
	if cfg.AgentModel != "" {
		args = append(args, "--agent-model", cfg.AgentModel)
	}
	// EgressAllow: forward ONE comma-joined --egress-allow only when non-empty.
	// The runner's parseEgress splits this single value on ",", so the allowlist
	// travels as one flag, never repeated flags. Empty (nil) omits the flag and
	// leaves the runner on its default-deny policy.
	if len(cfg.EgressAllow) > 0 {
		args = append(args, "--egress-allow", strings.Join(cfg.EgressAllow, ","))
	}
	return ProcessSpec{
		Component: ComponentRunner,
		Args:      args,
		Env:       []string{fmt.Sprintf("%s=%s", tokenEnvVar, token)},
	}
}
