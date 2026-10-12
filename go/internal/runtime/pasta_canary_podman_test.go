//go:build podman

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerifyNoPastaForwardsRefusesConfOptions pins the canary against real
// podman: a containers.conf pasta_options forward refuses, and none passes.
func TestVerifyNoPastaForwardsRefusesConfOptions(t *testing.T) {
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman not usable in this environment")
	}
	const image = "docker.io/library/alpine:latest"
	conf := filepath.Join(t.TempDir(), "containers.conf")
	for _, tc := range []struct {
		name, conf string
		wantErr    bool
	}{
		{name: "no forward passes", conf: "[network]\n"},
		{name: "tcp forward refuses", conf: "[network]\npasta_options = [\"-T\", \"24460\"]\n", wantErr: true},
		{name: "udp forward refuses", conf: "[network]\npasta_options = [\"-U\", \"5353\"]\n", wantErr: true},
		{name: "bridge default network skips a forward", conf: "[containers]\nnetns = \"bridge\"\n[network]\npasta_options = [\"-T\", \"24460\"]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(conf, []byte(tc.conf), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CONTAINERS_CONF_OVERRIDE", conf)
			err := NewPodmanCLI().VerifyNoPastaForwards(t.Context(), image)
			if tc.wantErr && (err == nil || !strings.Contains(err.Error(), "pasta_options")) {
				t.Fatalf("VerifyNoPastaForwards() = %v, want a pasta_options refusal", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyNoPastaForwards() = %v, want nil", err)
			}
		})
	}
}
