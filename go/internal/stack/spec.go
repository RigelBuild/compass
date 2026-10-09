//go:build unix

package stack

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// tokenEnvVar is the environment variable the runner reads its enrollment token
// from. It is passed via env only, never a flag, so it never reaches the process
// table (cmd/compass-runner/main.go:105-110).
const tokenEnvVar = "COMPASS_RUNNER_TOKEN"

const microVMRunRootEnvVar = "COMPASS_MICROVM_RUNROOT"

// embeddedRunnerID is the fixed identity of the single embedded runner. Embedded
// mode is single-user/single-runner by design (DL-106), so the id is an internal
// constant rather than a Config knob; it is cross-checked against the minted
// token's subject, so mint and spawn must agree on this one value (mirrors
// devenv's fixed `--runner-id dogfood`, devenv.nix:498 for the spawn and :547
// for the mint side).
const embeddedRunnerID = "embedded"

// serverSpec builds the compass-server child spec from the resolved config and
// cert paths, mirroring the devenv dogfood invocation except that the network
// door arrives as inherited fd 3 (--listen-fd): the stack holds the bound
// listener, so no other process can take the port before the server starts.
// --nats-url is unconditional because the server refuses to boot without it.
func serverSpec(cfg Config, cert CertResult, listen *os.File) ProcessSpec {
	args := []string{
		"--socket", cfg.SocketPath,
		"--database", cfg.DatabaseDSN,
		"--nats-url", natsURL(cfg),
		"--listen-fd", "3",
		"--tls-cert", cert.CertPath,
		"--tls-key", cert.KeyPath,
	}
	// Omit an empty provider so the server can use its own env-based resolution.
	if cfg.SecretProvider != "" {
		args = append(args, "--secret-provider", cfg.SecretProvider)
	}
	env := []string{}
	if cfg.S3Endpoint != "" {
		env = append(env, "COMPASS_S3_ENDPOINT="+cfg.S3Endpoint, "COMPASS_S3_BUCKET="+cfg.S3Bucket,
			"COMPASS_S3_ACCESS_KEY="+cfg.S3AccessKey, "COMPASS_S3_SECRET_KEY="+cfg.S3SecretKey,
			"COMPASS_S3_REGION="+cfg.S3Region)
		if cfg.S3UseTLS {
			env = append(env, "COMPASS_S3_USE_TLS=true")
		}
	}
	if cfg.TranscriptSafetyValveCapBytes > 0 {
		env = append(env, "COMPASS_TRANSCRIPT_SAFETY_VALVE_CAP_BYTES="+strconv.Itoa(cfg.TranscriptSafetyValveCapBytes))
	}
	return ProcessSpec{Component: ComponentServer, Args: args, Env: env, ExtraFiles: []*os.File{listen}}
}

// runnerSpec builds the compass-runner child spec (devenv.nix:497-502): it dials
// the server's TLS door over https, trusts the same cert as its --ca anchor,
// and mints per-container sockets under cfg.RuntimeDir. The token rides in Env
// only; guest is resolved by the caller (zero = the Runner image's baked copy).
func runnerSpec(cfg Config, cert CertResult, token string, guest GuestPaths, microVMRunRootEnv string, listenAddr string) ProcessSpec {
	// The four unconditional flags every runner spawn carries. Each optional
	// flag below is appended only when set, so a caller that leaves them zero
	// (the embedded supervisor, the compass-stack CLI's resolveConfig) gets a
	// byte-identical Args to before those features existed.
	args := []string{
		"--runner-id", embeddedRunnerID,
		"--server", "https://" + listenAddr,
		"--ca", cert.CertPath,
	}

	// AgentImage: the microVM backend runs the agent from the guest rootfs and
	// REFUSES a configured --image (runner.ResolveAgentImage), so forwarding one
	// would make every microVM runner fail at startup. Omit it there; every
	// container backend still requires it exactly as before.
	if !cfg.microVM() {
		args = append(args, "--image", cfg.AgentImage)
	}
	args = append(args, "--runtime-dir", cfg.RuntimeDir)
	if cfg.microVM() && microVMRunRootEnv == "" {
		// The flag overrides the inherited env, so only default it when unset.
		args = append(args, "--microvm-runroot", cfg.RuntimeDir)
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
	// CheckoutDir: forward --checkout-dir only when set. Empty keeps the runner
	// on its own default (/workspace), so a caller that leaves it unset gets a
	// byte-identical Args to before this field existed.
	if cfg.CheckoutDir != "" {
		args = append(args, "--checkout-dir", cfg.CheckoutDir)
	}
	// Mounts: forward one --mount per spec only when set. The runner's --mount is a
	// repeatable flag, so each spec travels as its own flag, in order. Empty (nil)
	// appends nothing, so a caller that leaves it unset gets byte-identical Args —
	// the same additive/zero-value-omit guarantee as the fields above.
	for _, m := range cfg.Mounts {
		args = append(args, "--mount", m)
	}
	if cfg.RuntimeBackend != "" {
		args = append(args, "--backend", cfg.RuntimeBackend)
		// The four guest flags travel as a set or not at all: a partially
		// configured triple would fail the runner's own preflight, and an
		// unverified path set is worse than the baked default.
		if guest != (GuestPaths{}) {
			args = append(args,
				"--microvm-kernel", guest.Kernel,
				"--microvm-rootfs", guest.Rootfs,
				"--microvm-initrd", guest.Initrd,
				"--microvm-image-manifest", guest.Manifest,
			)
		}
	}
	return ProcessSpec{
		Component: ComponentRunner,
		Args:      args,
		Env:       []string{fmt.Sprintf("%s=%s", tokenEnvVar, token)},
	}
}
