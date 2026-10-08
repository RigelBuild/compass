//go:build unix

package stack

import (
	"os"
	"slices"
	"testing"
)

// baseRunnerArgs is the unconditional flags runnerSpec always forwards
// (--runner-id, --server, --ca, --runtime-dir), plus --image on every backend
// that reads one. The AgentModel and EgressAllow flags are appended AFTER
// these, and only when set — so the base vector is the exact prefix every case
// shares and the zero-value case's whole expected output.
//
// --image is omitted under microVM because the runner REFUSES a configured
// agent image there (runner.ResolveAgentImage): the agent is pinned by the
// guest rootfs, so forwarding one would fail every microVM runner at startup.
func baseRunnerArgs(cfg Config, cert CertResult) []string {
	args := []string{
		"--runner-id", embeddedRunnerID,
		"--server", "https://" + cfg.ListenAddr,
		"--ca", cert.CertPath,
	}
	if !cfg.microVM() {
		args = append(args, "--image", cfg.AgentImage)
	}
	args = append(args, "--runtime-dir", cfg.RuntimeDir)
	if cfg.microVM() {
		args = append(args, "--microvm-runroot", cfg.RuntimeDir)
	}
	return args
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

			spec := runnerSpec(cfg, cert, token, GuestPaths{}, "")

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

func TestRunnerSpecGuestArgs(t *testing.T) {
	base := Config{ListenAddr: "127.0.0.1:50052", AgentImage: "agent:latest", RuntimeDir: "/run/compass"}
	cert := CertResult{CertPath: "/state/tls.crt"}
	resolved := guestPathsIn("/state/guest-image/abc")
	tests := []struct {
		name    string
		cfg     Config
		guest   GuestPaths
		wantEnd []string
	}{
		{name: "non-microvm remains unchanged", cfg: base, wantEnd: nil},
		{name: "backend without guest paths", cfg: Config{ListenAddr: base.ListenAddr, AgentImage: base.AgentImage, RuntimeDir: base.RuntimeDir, RuntimeBackend: "container"}, wantEnd: []string{"--backend", "container"}},
		// A microVM backend with no resolved paths is the baked-Runner-image
		// default: --backend alone, no guest flags.
		{name: "microvm without resolved paths", cfg: Config{ListenAddr: base.ListenAddr, AgentImage: base.AgentImage, RuntimeDir: base.RuntimeDir, RuntimeBackend: "microvm"}, wantEnd: []string{"--backend", "microvm"}},
		{
			name:  "microvm with resolved paths",
			cfg:   Config{ListenAddr: base.ListenAddr, AgentImage: base.AgentImage, RuntimeDir: base.RuntimeDir, RuntimeBackend: "microvm"},
			guest: resolved,
			wantEnd: []string{
				"--backend", "microvm",
				"--microvm-kernel", "/state/guest-image/abc/kernel",
				"--microvm-rootfs", "/state/guest-image/abc/rootfs.erofs",
				"--microvm-initrd", "/state/guest-image/abc/initrd",
				"--microvm-image-manifest", "/state/guest-image/abc/manifest.sha256",
			},
		},
		// Resolved paths without a backend forward nothing: --backend gates the
		// whole guest arg set, so a caller that resolved paths but selected no
		// backend still gets a byte-identical argv.
		{name: "resolved paths without a backend forward nothing", cfg: base, guest: resolved, wantEnd: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runnerSpec(tt.cfg, cert, "token", tt.guest, "").Args
			want := append(baseRunnerArgs(tt.cfg, cert), tt.wantEnd...)
			if !slices.Equal(got, want) {
				t.Fatalf("runnerSpec Args = %q, want %q", got, want)
			}
		})
	}
}

// The runner REFUSES a configured agent image under microVM
// (runner.ResolveAgentImage: the agent is pinned by the guest rootfs), so
// forwarding --image there fails the runner at startup. This asserts the flag
// literally rather than through baseRunnerArgs, which mirrors the production
// branch and so cannot fail if that branch is wrong.
func TestRunnerSpecOmitsAgentImageUnderMicroVM(t *testing.T) {
	cert := CertResult{CertPath: "/state/tls.crt"}
	cfg := Config{ListenAddr: "127.0.0.1:50052", AgentImage: "agent:latest", RuntimeDir: "/run/compass"}

	container := runnerSpec(cfg, cert, "token", GuestPaths{}, "").Args
	if i := slices.Index(container, "--image"); i < 0 || container[i+1] != "agent:latest" {
		t.Fatalf("container-backend args %q must still carry --image agent:latest", container)
	}

	cfg.RuntimeBackend = "microvm"
	micro := runnerSpec(cfg, cert, "token", GuestPaths{}, "").Args
	if slices.Contains(micro, "--image") {
		t.Errorf("microVM args %q carry --image, which the runner refuses", micro)
	}
	if slices.Contains(micro, "agent:latest") {
		t.Errorf("microVM args %q leak the agent image value", micro)
	}
}

func TestRunnerSpecMicroVMRunRoot(t *testing.T) {
	cert := CertResult{CertPath: "/state/tls.crt"}
	base := Config{
		ListenAddr:     "127.0.0.1:50052",
		RuntimeDir:     "/run/compass",
		RuntimeBackend: runtimeBackendMicroVM,
	}
	tests := []struct {
		name        string
		backend     string
		envRunRoot  string
		wantRunRoot string
	}{
		{name: "microVM defaults to RuntimeDir", backend: runtimeBackendMicroVM, wantRunRoot: base.RuntimeDir},
		{name: "microVM environment override wins", backend: runtimeBackendMicroVM, envRunRoot: "/operator/runroot"},
		{name: "container backend omits microVM run root", backend: "container", envRunRoot: "/operator/runroot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(microVMRunRootEnvVar, tt.envRunRoot)
			cfg := base
			cfg.RuntimeBackend = tt.backend
			args := runnerSpec(cfg, cert, "token", GuestPaths{}, os.Getenv(microVMRunRootEnvVar)).Args
			index := slices.Index(args, "--microvm-runroot")
			if tt.wantRunRoot == "" {
				if index >= 0 {
					t.Fatalf("runner args %q include --microvm-runroot, want it omitted", args)
				}
				return
			}
			if index < 0 || index+1 >= len(args) || args[index+1] != tt.wantRunRoot {
				t.Fatalf("runner args %q, want --microvm-runroot %q", args, tt.wantRunRoot)
			}
		})
	}
}

// The empty arm is the load-bearing one: an unset SecretProvider must yield a
// byte-identical argv, since the embedded supervisor and compass-stack's
// resolveConfig both leave it zero.
func TestServerSpecForwardsSecretProviderConditionally(t *testing.T) {
	base := Config{
		SocketPath:     "/state/compass.sock",
		DatabaseDSN:    "host=/state/pg dbname=compass",
		ListenAddr:     "127.0.0.1:50052",
		NatsClientPort: DefaultNatsClientPort,
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
		{name: "bundled nats passes the configured loopback client port", want: "nats://127.0.0.1:14222"},
		{name: "external nats passes the operator URL", external: "nats://nats.example.com:4222", want: "nats://nats.example.com:4222"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				SocketPath:      "/state/compass.sock",
				DatabaseDSN:     "host=/state/pg dbname=compass",
				ListenAddr:      "127.0.0.1:50052",
				ExternalNatsURL: tt.external,
				NatsClientPort:  14222,
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
