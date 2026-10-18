//go:build unix

package stack

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// tokenEnvVar passes the enrollment token only through the runner environment, never its process arguments.
const tokenEnvVar = "COMPASS_RUNNER_TOKEN"

const microVMRunRootEnvVar = "COMPASS_MICROVM_RUNROOT"

// secretProviderEnvVar is read by compass-server itself when no
// --secret-provider flag is passed; the child inherits the stack's env.
const secretProviderEnvVar = "COMPASS_SECRET_PROVIDER"

// runtimeBackendHost is the RuntimeBackend value that selects the host tier.
const runtimeBackendHost = "host"

// hostSecretProvider is the at-rest provider the host tier pins: its agents
// run on the operator's own desktop, where the OS keyring is available.
const hostSecretProvider = "keyring://"

// embeddedRunnerID fixes the single embedded runner identity shared by token minting and process spawn.
const embeddedRunnerID = "embedded"

// serverSpec builds the server invocation with the stack-owned network listener on inherited fd 3.
// --nats-url is required, and COMPASS_SECRET_PROVIDER is inherited through the environment.
func serverSpec(cfg Config, cert CertResult, listen *os.File, secretProviderEnv string) ProcessSpec {
	args := []string{
		"--socket", cfg.SocketPath,
		"--database", cfg.DatabaseDSN,
		"--nats-url", natsURL(cfg),
		"--listen-fd", "3",
		"--tls-cert", cert.CertPath,
		"--tls-key", cert.KeyPath,
	}
	// Omit an empty provider so the server can use its own env-based resolution.
	if provider := serverSecretProvider(cfg, secretProviderEnv); provider != "" {
		args = append(args, "--secret-provider", provider)
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

// serverSecretProvider returns the --secret-provider value: an explicit cfg or env
// provider wins, and only an unset host-tier stack defaults to the keyring.
func serverSecretProvider(cfg Config, secretProviderEnv string) string {
	if cfg.SecretProvider != "" || secretProviderEnv != "" || cfg.RuntimeBackend != runtimeBackendHost {
		return cfg.SecretProvider
	}
	return hostSecretProvider
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
