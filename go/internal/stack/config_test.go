//go:build unix

package stack

import (
	"strings"
	"testing"
)

func TestConfigValidate(t *testing.T) {
	base := Config{
		StateDir:    "/state",
		SocketPath:  "/run/server.sock",
		ListenAddr:  "127.0.0.1:50052",
		DatabaseDSN: "postgres:///compass",
		AgentImage:  "img:latest",
		RuntimeDir:  "/run/user/1000/compass",
	}

	tests := []struct {
		name       string
		mutate     func(*Config)
		wantErr    bool
		errSubstrs []string
	}{
		{name: "valid", mutate: func(*Config) {}, wantErr: false},
		{
			name:       "empty listen addr",
			mutate:     func(c *Config) { c.ListenAddr = "" },
			wantErr:    true,
			errSubstrs: []string{"ListenAddr"},
		},
		{
			name:       "ephemeral port rejected",
			mutate:     func(c *Config) { c.ListenAddr = "127.0.0.1:0" },
			wantErr:    true,
			errSubstrs: []string{":0", "discovery"},
		},
		{
			name: "runtime dir over sun_path budget",
			mutate: func(c *Config) {
				// Budget is sunPathMax - agentSocketTailWidth (38 on Linux); pad
				// well past it.
				c.RuntimeDir = "/run/" + strings.Repeat("x", 120)
			},
			wantErr:    true,
			errSubstrs: []string{"sun_path", "RuntimeDir", "bytes"},
		},
		{
			name: "runtime dir exactly at budget accepted",
			mutate: func(c *Config) {
				budget := sunPathMax - agentSocketTailWidth
				c.RuntimeDir = "/" + strings.Repeat("x", budget-1)
			},
			wantErr: false,
		},
		{
			name: "collector fields set are accepted (additive, no new Validate rule)",
			mutate: func(c *Config) {
				c.CollectorImage = "docker.io/otel/opentelemetry-collector-contrib@sha256:abc"
				c.ExternalOTLPEndpoint = "otlp.example.com:4317"
			},
			wantErr: false,
		},
		{
			name: "collector fields zero are the D3 default (bundle), still valid",
			mutate: func(c *Config) {
				c.CollectorImage = ""
				c.ExternalOTLPEndpoint = ""
			},
			wantErr: false,
		},
		{
			name: "guest fields require microvm backend",
			mutate: func(c *Config) {
				c.RuntimeBackend = "container"
				c.GuestDir = "/state/guest"
			},
			wantErr:    true,
			errSubstrs: []string{"GuestDir", "RuntimeBackend"},
		},
		{
			name:    "microvm with neither guest field",
			mutate:  func(c *Config) { c.RuntimeBackend = "microvm" },
			wantErr: false,
		},
		{
			name: "microvm with guest dir",
			mutate: func(c *Config) {
				c.RuntimeBackend = "microvm"
				c.GuestDir = "/state/guest"
			},
			wantErr: false,
		},
		{
			name: "guest artifact requires microvm backend",
			mutate: func(c *Config) {
				c.RuntimeBackend = "container"
				c.GuestArtifact = testGuestArtifact
			},
			wantErr:    true,
			errSubstrs: []string{"GuestArtifact", "RuntimeBackend"},
		},
		{
			name: "both guest knobs are mutually exclusive",
			mutate: func(c *Config) {
				c.RuntimeBackend = "microvm"
				c.GuestArtifact = testGuestArtifact
				c.GuestDir = "/state/guest"
			},
			wantErr:    true,
			errSubstrs: []string{"mutually exclusive"},
		},
		{
			name: "tag-pinned guest artifact is refused",
			mutate: func(c *Config) {
				c.RuntimeBackend = "microvm"
				c.GuestArtifact = "ghcr.io/rigelbuild/compass-guest-image:latest"
			},
			wantErr:    true,
			errSubstrs: []string{"digest-pinned"},
		},
		{
			// Validate shares the fetcher's parser, so the grammar cases it
			// rejects are refused here too rather than mid-startup.
			name: "malformed guest artifact is refused",
			mutate: func(c *Config) {
				c.RuntimeBackend = "microvm"
				c.GuestArtifact = "ghcr.io/Rigel/Guest@sha256:" + strings.Repeat("a", 64)
			},
			wantErr:    true,
			errSubstrs: []string{"repository path"},
		},
		{
			name: "microvm with a digest-pinned guest artifact",
			mutate: func(c *Config) {
				c.RuntimeBackend = "microvm"
				c.GuestArtifact = testGuestArtifact
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate() = nil, want error")
				}
				for _, sub := range tc.errSubstrs {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q missing substring %q", err.Error(), sub)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

// The sun_path budget must name the byte figures so an operator can act on it.
func TestConfigValidateBudgetError(t *testing.T) {
	cfg := Config{
		ListenAddr: "127.0.0.1:50052",
		RuntimeDir: "/run/" + strings.Repeat("x", 150),
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("want over-budget error")
	}
	budget := sunPathMax - agentSocketTailWidth
	for _, want := range []string{"AF_UNIX", "limit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
	if budget <= 0 || budget >= sunPathMax {
		t.Fatalf("computed budget %d is implausible (sunPathMax=%d, tail=%d)", budget, sunPathMax, agentSocketTailWidth)
	}
}
