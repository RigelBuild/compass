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

// TestRunnerSpecForwardsAgentModelAndEgressConditionally is the load-bearing
// red→green for the A4 Config plumbing (SEA-1785 Part 1). It pins the hard
// invariant a wrong diff violates: the two new Config fields reach the runner's
// flags EXACTLY when set, and NEITHER flag appears when they are zero — an
// embedded supervisor that leaves both unset must get a byte-identical Args to
// today (forwarding `--agent-model ""` would break it).
//
// EgressAllow is asserted comma-JOINED into ONE flag value, never repeated
// flags: the runner's parseEgress splits a single --egress-allow on ",".
func TestRunnerSpecForwardsAgentModelAndEgressConditionally(t *testing.T) {
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
		wantExtra   []string // the flags appended after the base five
	}{
		{
			name:      "both zero forwards neither flag (the embedded-supervisor invariant)",
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := cfg
			cfg.AgentModel = tt.agentModel
			cfg.EgressAllow = tt.egressAllow

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
