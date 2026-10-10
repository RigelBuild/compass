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

func TestParseRunnerClustersRejectsInvalidFiles(t *testing.T) {
	const second = `
  - name: b
    issuer: https://b.example.test`
	tests := []struct {
		name    string
		yaml    string
		wantErr string // empty: must parse
	}{
		{"600s parses", `
clusters:
  - name: a
    issuer: https://a.example.test
    maxTokenLifetime: 600s`, ""},
		{"599s is below the TokenRequest minimum", `
clusters:
  - name: a
    issuer: https://a.example.test
    maxTokenLifetime: 599s`, "maxTokenLifetime"},
		{"duplicate names", `
clusters:
  - name: a
    issuer: https://a.example.test
  - name: a
    issuer: https://other.example.test`, "duplicate name"},
		{"issuers differing only by a trailing slash", `
clusters:
  - name: a
    issuer: https://a.example.test/id
  - name: b
    issuer: https://a.example.test/id/`, "issuer already used"},
		{"non-https issuer", `
clusters:
  - name: a
    issuer: http://a.example.test`, "https://"},
		{"both jwksURI and jwksFile", `
clusters:
  - name: a
    issuer: https://a.example.test
    jwksURI: https://a.example.test/keys
    jwksFile: /etc/keys.json`, "mutually exclusive"},
		{"name with an upper-case letter", `
clusters:
  - name: Prod
    issuer: https://a.example.test`, "name must match"},
		{"name longer than 40", `
clusters:
  - name: ` + strings.Repeat("a", 41) + `
    issuer: https://a.example.test`, "name must match"},
		{"empty name", `
clusters:
  - issuer: https://a.example.test` + second, "name must match"},
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
