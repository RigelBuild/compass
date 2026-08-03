//go:build unix

package stack

import "fmt"

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
	return ProcessSpec{
		Component: ComponentRunner,
		Args: []string{
			"--runner-id", embeddedRunnerID,
			"--server", "https://" + cfg.ListenAddr,
			"--ca", cert.CertPath,
			"--image", cfg.AgentImage,
			"--runtime-dir", cfg.RuntimeDir,
		},
		Env: []string{fmt.Sprintf("%s=%s", tokenEnvVar, token)},
	}
}
