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
			name: "empty file → embedded",
			data: "",
			want: Config{Mode: ModeEmbedded},
		},
		{
			name: "explicit embedded",
			data: `mode = "embedded"`,
			want: Config{Mode: ModeEmbedded},
		},
		{
			name: "client with server_url",
			data: "mode = \"client\"\nserver_url = \"https://host:8443\"\n",
			want: Config{Mode: ModeClient, ServerURL: "https://host:8443"},
		},
		{
			name: "client with ca_cert parsed through",
			data: "mode = \"client\"\nserver_url = \"https://host:8443\"\nca_cert = \"/etc/anchor.pem\"\n",
			want: Config{Mode: ModeClient, ServerURL: "https://host:8443", CACert: "/etc/anchor.pem"},
		},
		{
			name:       "client missing server_url → error",
			data:       `mode = "client"`,
			wantErr:    true,
			errSubstrs: []string{"server_url", "client"},
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
			errSubstrs: []string{"proxy", "embedded", "client"},
		},
		{
			name:       "malformed toml → error",
			data:       "mode = ",
			wantErr:    true,
			errSubstrs: []string{"app.toml"},
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

func TestLoadAbsentFileIsEmbedded(t *testing.T) {
	dir := t.TempDir()
	got, err := Load(dir, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := (Config{Mode: ModeEmbedded}); got != want {
		t.Errorf("absent file: got %+v, want %+v", got, want)
	}
}

func TestLoadReadsPresentFile(t *testing.T) {
	dir := writeConfig(t, "mode = \"client\"\nserver_url = \"https://host:8443\"\n")
	got, err := Load(dir, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := (Config{Mode: ModeClient, ServerURL: "https://host:8443"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestLoadHomeFallbackPath(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".config", "compass", "app.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`mode = "embedded"`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load("", home, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := (Config{Mode: ModeEmbedded}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestLoadOverridePrecedence(t *testing.T) {
	tests := []struct {
		name     string
		file     string // empty means no file written
		override string
		want     Config
		wantErr  bool
	}{
		{
			name:     "override client forces client over embedded file",
			file:     `mode = "embedded"`,
			override: "client",
			wantErr:  true, // embedded file has no server_url to promote
		},
		{
			name:     "override client over a client file keeps server_url",
			file:     "mode = \"client\"\nserver_url = \"https://host:8443\"\n",
			override: "client",
			want:     Config{Mode: ModeClient, ServerURL: "https://host:8443"},
		},
		{
			name:     "override embedded forces embedded over client file",
			file:     "mode = \"client\"\nserver_url = \"https://host:8443\"\n",
			override: "embedded",
			want:     Config{Mode: ModeEmbedded},
		},
		{
			name:     "override embedded with no file present",
			override: "embedded",
			want:     Config{Mode: ModeEmbedded},
		},
		{
			name:     "unknown override → error",
			override: "proxy",
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var configHome string
			if tc.file != "" {
				configHome = writeConfig(t, tc.file)
			} else {
				configHome = t.TempDir()
			}
			got, err := Load(configHome, "", tc.override)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got config %+v", got)
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

func TestModeString(t *testing.T) {
	if got := ModeEmbedded.String(); got != "embedded" {
		t.Errorf("ModeEmbedded.String() = %q, want embedded", got)
	}
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
