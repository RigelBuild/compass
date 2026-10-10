package auth

import (
	"strings"
	"testing"
	"time"
)

func TestParseRunnerClustersAppliesDefaults(t *testing.T) {
	clusters, err := ParseRunnerClusters([]byte(`
clusters:
  - name: prod-eks
    issuer: https://oidc.example.test/id/ONE
    namespace: compass-runner
    serviceAccount: compass-runner
`))
	if err != nil {
		t.Fatalf("ParseRunnerClusters: %v", err)
	}
	if len(clusters) != 1 {
		t.Fatalf("got %d clusters, want 1", len(clusters))
	}
	c := clusters[0]
	if c.Audience != "compass-runner" || c.MaxTokenLifetime != 600*time.Second {
		t.Fatalf("defaults = aud %q, max %s; want compass-runner, 10m0s", c.Audience, c.MaxTokenLifetime)
	}
}

// entry renders one cluster with the required fields; extra lines override or add.
func entry(name, issuer string, extra ...string) string {
	lines := []string{"  - name: " + name, "    issuer: " + issuer}
	set := map[string]bool{}
	for _, e := range extra {
		set[strings.SplitN(e, ":", 2)[0]] = true
		lines = append(lines, "    "+e)
	}
	if !set["namespace"] {
		lines = append(lines, "    namespace: compass-runner")
	}
	if !set["serviceAccount"] {
		lines = append(lines, "    serviceAccount: compass-runner")
	}
	return strings.Join(lines, "\n")
}

func file(entries ...string) string {
	return "clusters:\n" + strings.Join(entries, "\n") + "\n"
}

func TestParseRunnerClustersRejectsInvalidFiles(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string // empty: must parse
	}{
		{"600s parses", file(entry("a", "https://a.example.test", "maxTokenLifetime: 600s")), ""},
		{"https jwksURI parses", file(entry("a", "https://a.example.test", "jwksURI: https://keys.example.test/jwks")), ""},
		{
			"599s is below the TokenRequest minimum",
			file(entry("a", "https://a.example.test", "maxTokenLifetime: 599s")), "maxTokenLifetime",
		},
		{
			"duplicate names",
			file(entry("a", "https://a.example.test"), entry("a", "https://other.example.test")), "duplicate name",
		},
		{
			"issuers differing only by a trailing slash",
			file(entry("a", "https://a.example.test/id"), entry("b", "https://a.example.test/id/")), "issuer already used",
		},
		{"non-https issuer", file(entry("a", "http://a.example.test")), "https://"},
		{
			"both jwksURI and jwksFile",
			file(entry("a", "https://a.example.test", "jwksURI: https://a.example.test/keys", "jwksFile: /etc/keys.json")),
			"mutually exclusive",
		},
		{"name with an upper-case letter", file(entry("Prod", "https://a.example.test")), "name must match"},
		{"name longer than 40", file(entry(strings.Repeat("a", 41), "https://a.example.test")), "name must match"},
		{"empty name", file(entry(`""`, "https://a.example.test")), "name must match"},
		{"empty namespace", file(entry("a", "https://a.example.test", `namespace: ""`)), "namespace"},
		{"empty serviceAccount", file(entry("a", "https://a.example.test", `serviceAccount: ""`)), "serviceAccount"},
		{"non-https jwksURI", file(entry("a", "https://a.example.test", "jwksURI: http://a.example.test/keys")), "jwksURI"},
		{"jwksURI without a host", file(entry("a", "https://a.example.test", "jwksURI: https:///keys")), "jwksURI"},
		{"unknown field", file(entry("a", "https://a.example.test", "audiences: [x]")), "audiences"},
		{"unknown top-level field", "clusterz: []\n", "clusterz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRunnerClusters([]byte(tt.yaml))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseRunnerClusters: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ParseRunnerClusters error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}
