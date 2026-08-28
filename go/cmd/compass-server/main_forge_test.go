//go:build unix

package main

// Unit test for resolveForge: the forge CLI knobs (repos seed, secret name,
// host, and the RIG-2883 App id / installation id / App key secret / webhook
// secret) turned into server.ForgeConfig, mirroring resolveNetworkDoor's pure
// input->output shape (no I/O, no store). Covers the flag->config mapping,
// repo-format validation, seed case-normalization, and the int-parse error path.
// The flag-then-env precedence itself is firstNonEmpty (covered by
// resolveNetworkDoor's suite and exercised inline in run()); resolveForge is fed
// the already-resolved strings, so this test drives the resolution logic that is
// unique to forge.

import (
	"strings"
	"testing"
)

func TestResolveForgeMapping(t *testing.T) {
	t.Run("disabled default: no repos, no App", func(t *testing.T) {
		fc, err := resolveForge("", "", "", "", "", "", "")
		if err != nil {
			t.Fatalf("resolveForge: %v", err)
		}
		if len(fc.SeedRepos) != 0 {
			t.Fatalf("SeedRepos = %v, want empty", fc.SeedRepos)
		}
		if fc.App.AppID != 0 || fc.App.InstallationID != 0 {
			t.Fatalf("App ids = %d/%d, want 0/0", fc.App.AppID, fc.App.InstallationID)
		}
		// Empty host/secret/App-secret NAMEs are left zero for server-side
		// defaulting — resolveForge must not bake defaults the ServeConfig owns.
		if fc.Host != "" || fc.SecretName != "" ||
			fc.App.AppPrivateKeySecret != "" || fc.App.AppWebhookSecretName != "" {
			t.Fatalf("empty inputs should stay zero, got host=%q secret=%q key=%q webhook=%q",
				fc.Host, fc.SecretName, fc.App.AppPrivateKeySecret, fc.App.AppWebhookSecretName)
		}
	})

	t.Run("full flag mapping", func(t *testing.T) {
		fc, err := resolveForge("owner/repo, foo/bar", "MY_TOKEN", "ghe.example.com",
			"12345", "678", "APP_KEY", "WEBHOOK_SECRET")
		if err != nil {
			t.Fatalf("resolveForge: %v", err)
		}
		if fc.Host != "ghe.example.com" {
			t.Fatalf("Host = %q, want ghe.example.com", fc.Host)
		}
		if fc.SecretName != "MY_TOKEN" {
			t.Fatalf("SecretName = %q, want MY_TOKEN", fc.SecretName)
		}
		if fc.App.AppID != 12345 || fc.App.InstallationID != 678 {
			t.Fatalf("App ids = %d/%d, want 12345/678", fc.App.AppID, fc.App.InstallationID)
		}
		if fc.App.AppPrivateKeySecret != "APP_KEY" || fc.App.AppWebhookSecretName != "WEBHOOK_SECRET" {
			t.Fatalf("App secrets = %q/%q, want APP_KEY/WEBHOOK_SECRET",
				fc.App.AppPrivateKeySecret, fc.App.AppWebhookSecretName)
		}
		want := []string{"owner/repo", "foo/bar"}
		if len(fc.SeedRepos) != len(want) {
			t.Fatalf("SeedRepos = %v, want %v", fc.SeedRepos, want)
		}
		for i, w := range want {
			if fc.SeedRepos[i] != w {
				t.Fatalf("SeedRepos[%d] = %q, want %q", i, fc.SeedRepos[i], w)
			}
		}
	})

	t.Run("case normalization: Owner/Name lowercases to one target", func(t *testing.T) {
		fc, err := resolveForge("Owner/Name", "", "", "", "", "", "")
		if err != nil {
			t.Fatalf("resolveForge: %v", err)
		}
		if len(fc.SeedRepos) != 1 || fc.SeedRepos[0] != "owner/name" {
			t.Fatalf("SeedRepos = %v, want [owner/name] (lowercased)", fc.SeedRepos)
		}
	})
}

func TestResolveForgeRejectsGarbage(t *testing.T) {
	garbage := []struct {
		name  string
		repos string
	}{
		{"no slash", "ownerrepo"},
		{"empty owner", "/name"},
		{"empty name", "owner/"},
		{"too many segments", "owner/name/extra"},
	}
	for _, tc := range garbage {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveForge(tc.repos, "", "", "", "", "", "")
			if err == nil {
				t.Fatalf("resolveForge(%q) = nil error, want a startup error", tc.repos)
			}
			if !strings.Contains(err.Error(), "owner/name") {
				t.Fatalf("error %q should name the owner/name format", err.Error())
			}
		})
	}
}

func TestResolveForgeRejectsBadAppID(t *testing.T) {
	for _, tc := range []struct {
		name, appID, installID, wantFlag string
	}{
		{"non-numeric app id", "notanumber", "", "--forge-app-id"},
		{"non-numeric installation id", "123", "nope", "--forge-installation-id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveForge("owner/repo", "", "", tc.appID, tc.installID, "", "")
			if err == nil {
				t.Fatalf("resolveForge = nil error, want a startup error")
			}
			if !strings.Contains(err.Error(), tc.wantFlag) {
				t.Fatalf("error %q should name %s", err.Error(), tc.wantFlag)
			}
		})
	}
}
