//go:build unix

package stack

import (
	"slices"
	"testing"
)

// baseRunnerArgs is the five unconditional flags runnerSpec always forwards
// (--runner-id, --server, --ca, --image, --runtime-dir). The AgentModel and
// EgressAllow flags are appended AFTER these, and only when set — so the base
// vector is the exact prefix every case shares and the zero-value case's whole
// expected output.
func baseRunnerArgs(cfg Config, cert CertResult) []string {
	return []string{
		"--runner-id", embeddedRunnerID,
		"--server", "https://" + cfg.ListenAddr,
		"--ca", cert.CertPath,
		"--image", cfg.AgentImage,
		"--runtime-dir", cfg.RuntimeDir,
	}
}

// TestRunnerSpecForwardsOptionalFlagsConditionally is the load-bearing red→green
// for the A4 Config plumbing (RIG-1785). It pins the hard invariant a wrong diff
// violates: the optional Config fields reach the runner's flags EXACTLY when set,
// and NONE appears when they are zero — an embedded supervisor that leaves them
// unset must get a byte-identical Args to today (forwarding `--agent-model ""`,
// or a `--checkout-dir` the caller never asked for, would break it).
//
// EgressAllow is asserted comma-JOINED into ONE flag value, never repeated
// flags: the runner's parseEgress splits a single --egress-allow on ",".
func TestRunnerSpecForwardsOptionalFlagsConditionally(t *testing.T) {
	cfg := Config{
		ListenAddr: "127.0.0.1:50052",
		AgentImage: "compass-agent:latest",
		RuntimeDir: "/run/compass",
	}
	cert := CertResult{CertPath: "/state/tls.crt", KeyPath: "/state/tls.key"}
	const token = "runner-token"

	tests := []struct {
		name        string
		agentModel  string
		egressAllow []string
		checkoutDir string
		mounts      []string
		wantExtra   []string // the flags appended after the base five
	}{
		{
			name:      "all zero forwards no optional flag (the embedded-supervisor invariant)",
			wantExtra: nil,
		},
		{
			name:       "agent model set forwards a single --agent-model",
			agentModel: "anthropic/claude-opus",
			wantExtra:  []string{"--agent-model", "anthropic/claude-opus"},
		},
		{
			name:        "egress allow set forwards ONE comma-joined --egress-allow",
			egressAllow: []string{"api.anthropic.com", "10.0.0.1"},
			wantExtra:   []string{"--egress-allow", "api.anthropic.com,10.0.0.1"},
		},
		{
			name:        "both set forwards agent-model then comma-joined egress",
			agentModel:  "anthropic/claude-opus",
			egressAllow: []string{"api.anthropic.com", "10.0.0.1"},
			wantExtra:   []string{"--agent-model", "anthropic/claude-opus", "--egress-allow", "api.anthropic.com,10.0.0.1"},
		},
		{
			name:        "single egress host is still one flag with no trailing comma",
			egressAllow: []string{"api.anthropic.com"},
			wantExtra:   []string{"--egress-allow", "api.anthropic.com"},
		},
		{
			name:        "checkout dir set forwards a single --checkout-dir",
			checkoutDir: "/home/agent/repo",
			wantExtra:   []string{"--checkout-dir", "/home/agent/repo"},
		},
		{
			name:      "mounts set forward one --mount per spec, in order",
			mounts:    []string{"/host/a:/home/agent/.omp/agent:ro", "/host/b:/cache"},
			wantExtra: []string{"--mount", "/host/a:/home/agent/.omp/agent:ro", "--mount", "/host/b:/cache"},
		},
		{
			name:        "all set forward in order: agent-model, egress, checkout-dir, mounts",
			agentModel:  "anthropic/claude-opus",
			egressAllow: []string{"api.anthropic.com", "10.0.0.1"},
			checkoutDir: "/home/agent/repo",
			mounts:      []string{"/host/a:/home/agent/.omp/agent:ro"},
			wantExtra:   []string{"--agent-model", "anthropic/claude-opus", "--egress-allow", "api.anthropic.com,10.0.0.1", "--checkout-dir", "/home/agent/repo", "--mount", "/host/a:/home/agent/.omp/agent:ro"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := cfg
			cfg.AgentModel = tt.agentModel
			cfg.EgressAllow = tt.egressAllow
			cfg.CheckoutDir = tt.checkoutDir
			cfg.Mounts = tt.mounts

			spec := runnerSpec(cfg, cert, token)

			want := append(baseRunnerArgs(cfg, cert), tt.wantExtra...)
			if !slices.Equal(spec.Args, want) {
				t.Fatalf("runnerSpec Args =\n  %q\nwant\n  %q", spec.Args, want)
			}

			// The token rides in Env only, never the process table — unchanged
			// by this feature, asserted so a refactor cannot silently move it.
			wantEnv := []string{tokenEnvVar + "=" + token}
			if !slices.Equal(spec.Env, wantEnv) {
				t.Fatalf("runnerSpec Env = %q, want %q", spec.Env, wantEnv)
			}
		})
	}
}

// The empty arm is the load-bearing one: an unset SecretProvider must yield a
// byte-identical argv, since the embedded supervisor and compass-stack's
// resolveConfig both leave it zero.
func TestServerSpecForwardsSecretProviderConditionally(t *testing.T) {
	base := Config{
		SocketPath:  "/state/compass.sock",
		DatabaseDSN: "host=/state/pg dbname=compass",
		ListenAddr:  "127.0.0.1:50052",
	}
	cert := CertResult{CertPath: "/state/tls.crt", KeyPath: "/state/tls.key"}

	tests := []struct {
		name           string
		secretProvider string
		want           []string
	}{
		{
			name: "empty provider preserves six-flag argv",
			want: []string{
				"--socket", base.SocketPath,
				"--database", base.DatabaseDSN,
				"--nats-url", "nats://127.0.0.1:4222",
				"--listen", base.ListenAddr,
				"--tls-cert", cert.CertPath,
				"--tls-key", cert.KeyPath,
			},
		},
		{
			name:           "provider set appends flag and value",
			secretProvider: "dotenv:///state/secrets.env",
			want: []string{
				"--socket", base.SocketPath,
				"--database", base.DatabaseDSN,
				"--nats-url", "nats://127.0.0.1:4222",
				"--listen", base.ListenAddr,
				"--tls-cert", cert.CertPath,
				"--tls-key", cert.KeyPath,
				"--secret-provider", "dotenv:///state/secrets.env",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.SecretProvider = tt.secretProvider
			if got := serverSpec(cfg, cert).Args; !slices.Equal(got, tt.want) {
				t.Fatalf("serverSpec Args = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestServerSpecAlwaysForwardsNatsURL pins --nats-url on both NATS postures:
// compass-server refuses to boot without a fabric, so a bundled stack must pass
// the container's loopback endpoint and --nats-external the operator's URL.
func TestServerSpecAlwaysForwardsNatsURL(t *testing.T) {
	cert := CertResult{CertPath: "/state/tls.crt", KeyPath: "/state/tls.key"}
	tests := []struct {
		name, external, want string
	}{
		{name: "bundled nats passes the loopback client endpoint", want: "nats://127.0.0.1:4222"},
		{name: "external nats passes the operator URL", external: "nats://nats.example.com:4222", want: "nats://nats.example.com:4222"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				SocketPath:      "/state/compass.sock",
				DatabaseDSN:     "host=/state/pg dbname=compass",
				ListenAddr:      "127.0.0.1:50052",
				ExternalNatsURL: tt.external,
			}
			args := serverSpec(cfg, cert).Args
			i := slices.Index(args, "--nats-url")
			if i < 0 || i+1 == len(args) {
				t.Fatalf("serverSpec Args = %q, want --nats-url with a value", args)
			}
			if got := args[i+1]; got != tt.want {
				t.Fatalf("--nats-url = %q, want %q", got, tt.want)
			}
		})
	}
}
