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
	"reflect"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/server"
)

func TestResolveForgeMapping(t *testing.T) {
	t.Run("disabled default: no repos, no Apps", func(t *testing.T) {
		fc, err := resolveForge(forgeInputs{})
		if err != nil {
			t.Fatalf("resolveForge: %v", err)
		}
		// Everything stays zero: no seed, no Apps, and — crucially — empty host /
		// secret NAMEs are NOT defaulted here (server-side ServeConfig owns those
		// defaults). A zero-value ForgeConfig is exactly that contract.
		if want := (server.ForgeConfig{}); !reflect.DeepEqual(fc, want) {
			t.Fatalf("empty inputs should map to a zero ForgeConfig\n got %+v\nwant %+v", fc, want)
		}
	})

	t.Run("full flag mapping", func(t *testing.T) {
		fc, err := resolveForge(forgeInputs{
			repos:                  "owner/repo, foo/bar",
			host:                   "ghe.example.com",
			appID:                  "12345",
			installationID:         "678",
			appKeySecret:           "APP_KEY",
			appWebhook:             "WEBHOOK_SECRET",
			reviewerAppID:          "222",
			reviewerInstallationID: "333",
			reviewerAppKeySecret:   "REVIEWER_APP_KEY",
			linearClientID:         "LINEAR_CID",
			linearClientSecret:     "LINEAR_CSECRET",
			linearWebhook:          "LINEAR_WEBHOOK_SECRET",
		})
		if err != nil {
			t.Fatalf("resolveForge: %v", err)
		}
		// The whole mapping in one comparison: ids parsed to int64, secret NAMEs
		// threaded verbatim, repos split + lowercased, and the reviewer App's
		// webhook NAME left empty (it registers no webhook).
		want := server.ForgeConfig{
			Host:                     "ghe.example.com",
			SeedRepos:                []string{"owner/repo", "foo/bar"},
			LinearClientIDSecretName: "LINEAR_CID",
			LinearClientSecretName:   "LINEAR_CSECRET",
			LinearWebhookSecretName:  "LINEAR_WEBHOOK_SECRET",
			App: server.ForgeAppConfig{
				AppID:                12345,
				InstallationID:       678,
				AppPrivateKeySecret:  "APP_KEY",
				AppWebhookSecretName: "WEBHOOK_SECRET",
			},
			ReviewerApp: server.ForgeAppConfig{
				AppID:               222,
				InstallationID:      333,
				AppPrivateKeySecret: "REVIEWER_APP_KEY",
			},
		}
		if !reflect.DeepEqual(fc, want) {
			t.Fatalf("full flag mapping mismatch\n got %+v\nwant %+v", fc, want)
		}
	})

	t.Run("case normalization: Owner/Name lowercases to one target", func(t *testing.T) {
		fc, err := resolveForge(forgeInputs{repos: "Owner/Name"})
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
			_, err := resolveForge(forgeInputs{repos: tc.repos})
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
		name, wantFlag string
		in             forgeInputs
	}{
		{"non-numeric app id", "--forge-app-id", forgeInputs{repos: "owner/repo", appID: "notanumber"}},
		{"non-numeric installation id", "--forge-installation-id", forgeInputs{repos: "owner/repo", appID: "123", installationID: "nope"}},
		{"non-numeric reviewer app id", "--forge-reviewer-app-id", forgeInputs{repos: "owner/repo", reviewerAppID: "nope"}},
		{"non-numeric reviewer installation id", "--forge-reviewer-app-installation-id", forgeInputs{repos: "owner/repo", reviewerAppID: "123", reviewerInstallationID: "nope"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveForge(tc.in)
			if err == nil {
				t.Fatalf("resolveForge = nil error, want a startup error")
			}
			if !strings.Contains(err.Error(), tc.wantFlag) {
				t.Fatalf("error %q should name %s", err.Error(), tc.wantFlag)
			}
		})
	}
}
