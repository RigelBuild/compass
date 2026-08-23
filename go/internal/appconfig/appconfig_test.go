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
			name: "empty file → client (server_url required)",
			data: "",
			// Absent mode defaults to client, which requires server_url; an
			// empty file therefore fails legibly rather than resolving.
			wantErr:    true,
			errSubstrs: []string{"server_url"},
		},
		{
			name: "client with server_url",
			data: "mode = \"client\"\nserver_url = \"https://host:8443\"\n",
			want: Config{Mode: ModeClient, ServerURL: "https://host:8443"},
		},
		{
			name: "absent mode with server_url → client",
			data: "server_url = \"https://host:8443\"\n",
			want: Config{Mode: ModeClient, ServerURL: "https://host:8443"},
		},
		{
			name: "client with ca_cert parsed through",
			data: "mode = \"client\"\nserver_url = \"https://host:8443\"\nca_cert = \"/etc/anchor.pem\"\n",
			want: Config{Mode: ModeClient, ServerURL: "https://host:8443", CACert: "/etc/anchor.pem"},
		},
		{
			name:       "embedded → legible retirement rejection",
			data:       `mode = "embedded"`,
			wantErr:    true,
			errSubstrs: []string{"embedded", "retired", "compass-stack up", "client"},
		},
		{
			name:       "client missing server_url → error",
			data:       `mode = "client"`,
			wantErr:    true,
			errSubstrs: []string{"server_url"},
		},
		{
			name:       "client with http server_url → error",
			data:       "mode = \"client\"\nserver_url = \"http://host:8443\"\n",
			wantErr:    true,
			errSubstrs: []string{"https", "cleartext"},
		},
		{
			name:       "client with relative server_url → error",
			data:       "mode = \"client\"\nserver_url = \"host:8443\"\n",
			wantErr:    true,
			errSubstrs: []string{"server_url"},
		},
		{
			name:       "unknown mode → error",
			data:       `mode = "proxy"`,
			wantErr:    true,
			errSubstrs: []string{"proxy", "client"},
		},
		{
			name:       "malformed toml → error",
			data:       "mode = ",
			wantErr:    true,
			errSubstrs: []string{"app.toml"},
		},
		{
			name:       "whitespace-only mode → client (server_url required)",
			data:       `mode = "  "`,
			wantErr:    true,
			errSubstrs: []string{"server_url"},
		},
		{
			name:       "client whitespace-only server_url → error",
			data:       "mode = \"client\"\nserver_url = \"   \"\n",
			wantErr:    true,
			errSubstrs: []string{"server_url"},
		},
		{
			name:       "client server_url with embedded credentials → error",
			data:       "mode = \"client\"\nserver_url = \"https://user:pass@host:8443\"\n",
			wantErr:    true,
			errSubstrs: []string{"credentials", "keychain"},
		},
		{
			name:       "unknown key → error",
			data:       "mode = \"client\"\nserver_url = \"https://host:8443\"\ncacert = \"/etc/anchor.pem\"\n",
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
// error (client mode needs a server_url; embedded's zero-config default was
// retired in RIG-2554), naming the config path and pointing at the client setup.
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
	dir := writeConfig(t, "mode = \"client\"\nserver_url = \"https://host:8443\"\n")
	got, err := Load(dir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := (Config{Mode: ModeClient, ServerURL: "https://host:8443"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestLoadEmbeddedFileIsRejected: a present file selecting the retired embedded
// mode is rejected through Load (not just Parse) with the retirement copy.
func TestLoadEmbeddedFileIsRejected(t *testing.T) {
	dir := writeConfig(t, `mode = "embedded"`)
	got, err := Load(dir, "")
	if err == nil {
		t.Fatalf("embedded file: want a rejection, got config %+v", got)
	}
	for _, sub := range []string{"embedded", "retired", "compass-stack up"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("embedded rejection %q missing substring %q", err, sub)
		}
	}
}

func TestLoadHomeFallbackPath(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".config", "compass", "app.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("mode = \"client\"\nserver_url = \"https://host:8443\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load("", home)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := (Config{Mode: ModeClient, ServerURL: "https://host:8443"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestModeString(t *testing.T) {
	if got := ModeClient.String(); got != "client" {
		t.Errorf("ModeClient.String() = %q, want client", got)
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
