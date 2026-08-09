//go:build unix

package main

// Unit test for resolveForge: the five forge CLI knobs (repos seed, poll enable,
// interval, secret name, host) turned into server.ForgeConfig, mirroring
// resolveNetworkDoor's pure input->output shape (no I/O, no store). Covers the
// flag->config mapping, repo-format validation, seed case-normalization, the
// polling-enabled predicate, and the interval-parse error path. The flag-then-env
// precedence itself is firstNonEmpty/envTrue (covered by resolveNetworkDoor's
// suite and exercised inline in run()); resolveForge is fed the already-resolved
// strings, so this test drives the resolution logic that is unique to forge.

import (
	"strings"
	"testing"
	"time"
)

func TestResolveForgeMapping(t *testing.T) {
	t.Run("disabled default: no repos, no poll", func(t *testing.T) {
		fc, err := resolveForge("", false, "", "", "")
		if err != nil {
			t.Fatalf("resolveForge: %v", err)
		}
		if len(fc.SeedRepos) != 0 {
			t.Fatalf("SeedRepos = %v, want empty", fc.SeedRepos)
		}
		if fc.Poll {
			t.Fatal("Poll = true, want false")
		}
		// Empty host/secret/interval are left zero for server-side defaulting —
		// resolveForge must not bake defaults the ServeConfig owns.
		if fc.Host != "" || fc.SecretName != "" || fc.PollInterval != 0 {
			t.Fatalf("empty inputs should stay zero, got host=%q secret=%q interval=%v",
				fc.Host, fc.SecretName, fc.PollInterval)
		}
	})

	t.Run("full flag mapping", func(t *testing.T) {
		fc, err := resolveForge("owner/repo, foo/bar", true, "2m", "MY_TOKEN", "ghe.example.com")
		if err != nil {
			t.Fatalf("resolveForge: %v", err)
		}
		if !fc.Poll {
			t.Fatal("Poll = false, want true")
		}
		if fc.Host != "ghe.example.com" {
			t.Fatalf("Host = %q, want ghe.example.com", fc.Host)
		}
		if fc.SecretName != "MY_TOKEN" {
			t.Fatalf("SecretName = %q, want MY_TOKEN", fc.SecretName)
		}
		if fc.PollInterval != 2*time.Minute {
			t.Fatalf("PollInterval = %v, want 2m", fc.PollInterval)
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
		fc, err := resolveForge("Owner/Name", false, "", "", "")
		if err != nil {
			t.Fatalf("resolveForge: %v", err)
		}
		if len(fc.SeedRepos) != 1 || fc.SeedRepos[0] != "owner/name" {
			t.Fatalf("SeedRepos = %v, want [owner/name] (lowercased)", fc.SeedRepos)
		}
	})

}

func TestResolveForgeEnableSignals(t *testing.T) {
	t.Run("a non-empty seed is the enable signal", func(t *testing.T) {
		fc, err := resolveForge("owner/repo", false, "", "", "")
		if err != nil {
			t.Fatalf("resolveForge: %v", err)
		}
		// resolveForge maps flags faithfully; the enable predicate (Poll ||
		// len(SeedRepos) > 0) is the server's, tested there. Here: a seed with
		// Poll false must still carry the seed so the server can enable on it.
		if fc.Poll {
			t.Fatal("Poll = true, want false (seed alone, flag unset)")
		}
		if len(fc.SeedRepos) != 1 {
			t.Fatalf("SeedRepos = %v, want one entry", fc.SeedRepos)
		}
	})

	t.Run("poll flag with an empty seed", func(t *testing.T) {
		fc, err := resolveForge("", true, "", "", "")
		if err != nil {
			t.Fatalf("resolveForge: %v", err)
		}
		if !fc.Poll {
			t.Fatal("Poll = false, want true (--forge-poll set)")
		}
		if len(fc.SeedRepos) != 0 {
			t.Fatalf("SeedRepos = %v, want empty", fc.SeedRepos)
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
			_, err := resolveForge(tc.repos, false, "", "", "")
			if err == nil {
				t.Fatalf("resolveForge(%q) = nil error, want a startup error", tc.repos)
			}
			if !strings.Contains(err.Error(), "owner/name") {
				t.Fatalf("error %q should name the owner/name format", err.Error())
			}
		})
	}
}

func TestResolveForgeRejectsBadInterval(t *testing.T) {
	for _, iv := range []string{"nonsense", "0s", "-5m"} {
		t.Run(iv, func(t *testing.T) {
			_, err := resolveForge("owner/repo", false, iv, "", "")
			if err == nil {
				t.Fatalf("resolveForge with interval %q = nil error, want a startup error", iv)
			}
			if !strings.Contains(err.Error(), "forge-poll-interval") {
				t.Fatalf("error %q should name --forge-poll-interval", err.Error())
			}
		})
	}
}
