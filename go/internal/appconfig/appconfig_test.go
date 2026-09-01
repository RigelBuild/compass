package appconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name       string
		data       string
		want       Config
		wantErr    bool
		errSubstrs []string
	}{
		{
			name: "empty file → error (server_url required)",
			data: "",
			// The app is a client and requires server_url; an empty file
			// therefore fails legibly rather than resolving.
			wantErr:    true,
			errSubstrs: []string{"server_url"},
		},
		{
			name: "server_url only → client",
			data: "server_url = \"https://host:8443\"\n",
			want: Config{ServerURL: "https://host:8443"},
		},
		{
			name: "ca_cert parsed through",
			data: "server_url = \"https://host:8443\"\nca_cert = \"/etc/anchor.pem\"\n",
			want: Config{ServerURL: "https://host:8443", CACert: "/etc/anchor.pem"},
		},
		{
			// Regression: the mode key was hard-removed (RIG-3111). A stale
			// mode = "client" line is now an ordinary unknown-key rejection
			// that names the offending key — that IS the migration message.
			name:       "stale mode key → unknown-key rejection",
			data:       "mode = \"client\"\nserver_url = \"https://host:8443\"\n",
			wantErr:    true,
			errSubstrs: []string{"unknown key", "mode"},
		},
		{
			// A populated file that omits server_url still errors (distinct from
			// the empty-file case: a non-empty config carrying only other valid
			// keys is not a pass).
			name:       "populated file missing server_url → error",
			data:       "ca_cert = \"/etc/anchor.pem\"\n",
			wantErr:    true,
			errSubstrs: []string{"server_url"},
		},
		{
			name:       "http server_url → error",
			data:       "server_url = \"http://host:8443\"\n",
			wantErr:    true,
			errSubstrs: []string{"https", "cleartext"},
		},
		{
			name:       "relative server_url → error",
			data:       "server_url = \"host:8443\"\n",
			wantErr:    true,
			errSubstrs: []string{"server_url"},
		},
		{
			name:       "malformed toml → error",
			data:       "server_url = ",
			wantErr:    true,
			errSubstrs: []string{"app.toml"},
		},
		{
			name:       "whitespace-only server_url → error",
			data:       "server_url = \"   \"\n",
			wantErr:    true,
			errSubstrs: []string{"server_url"},
		},
		{
			name:       "server_url with embedded credentials → error",
			data:       "server_url = \"https://user:pass@host:8443\"\n",
			wantErr:    true,
			errSubstrs: []string{"credentials", "keychain"},
		},
		{
			name:       "unknown key → error",
			data:       "server_url = \"https://host:8443\"\ncacert = \"/etc/anchor.pem\"\n",
			wantErr:    true,
			errSubstrs: []string{"unknown key", "cacert"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse([]byte(tc.data))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got config %+v", got)
				}
				for _, sub := range tc.errSubstrs {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q missing substring %q", err, sub)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestLoadAbsentFileIsFirstRunError: an absent app.toml is a legible first-run
// error (the app is a client and needs a server_url), naming the config path
// and pointing at the client setup.
func TestLoadAbsentFileIsFirstRunError(t *testing.T) {
	dir := t.TempDir()
	got, err := Load(dir, "")
	if err == nil {
		t.Fatalf("absent file: want a first-run error, got config %+v", got)
	}
	for _, sub := range []string{"app.toml", "server_url", "client"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("first-run error %q missing substring %q", err, sub)
		}
	}
}

func TestLoadReadsPresentFile(t *testing.T) {
	dir := writeConfig(t, "server_url = \"https://host:8443\"\n")
	got, err := Load(dir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := (Config{ServerURL: "https://host:8443"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestLoadHomeFallbackPath(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".config", "compass", "app.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("server_url = \"https://host:8443\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load("", home)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := (Config{ServerURL: "https://host:8443"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// writeConfig writes app.toml under a fresh temp configHome and returns that
// configHome dir (so Load resolves configHome/compass/app.toml).
func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "compass", "app.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}
