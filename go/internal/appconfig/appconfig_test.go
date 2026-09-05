package appconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type parseCase struct {
	name       string
	data       string
	want       Config
	wantErr    bool
	errSubstrs []string
}

func runParseCases(t *testing.T, tests []parseCase) {
	t.Helper()
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

func TestParseEmbedded(t *testing.T) {
	runParseCases(t, []parseCase{
		{
			name: "empty file → embedded (zero-config default)",
			data: "",
			want: Config{Mode: ModeEmbedded},
		},
		{
			name: "explicit embedded mode",
			data: `mode = "embedded"`,
			want: Config{Mode: ModeEmbedded},
		},
		{
			name: "whitespace-only mode → embedded",
			data: `mode = "  "`,
			want: Config{Mode: ModeEmbedded},
		},
		{
			name:       "embedded with server_url → legible reject",
			data:       "mode = \"embedded\"\nserver_url = \"https://host:8443\"\n",
			wantErr:    true,
			errSubstrs: []string{"server_url", "client-only", "embedded"},
		},
		{
			name:       "embedded with ca_cert → legible reject",
			data:       "mode = \"embedded\"\nca_cert = \"/etc/anchor.pem\"\n",
			wantErr:    true,
			errSubstrs: []string{"ca_cert", "client-only", "embedded"},
		},
		{
			name:       "absent mode with server_url → legible reject (embedded default is client-free)",
			data:       "server_url = \"https://host:8443\"\n",
			wantErr:    true,
			errSubstrs: []string{"server_url", "client-only"},
		},
	})
}

func TestParseClient(t *testing.T) {
	runParseCases(t, []parseCase{
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
			name:       "unknown mode → error naming both modes",
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
		{
			name:       "unknown key → error",
			data:       "mode = \"client\"\nserver_url = \"https://host:8443\"\ncacert = \"/etc/anchor.pem\"\n",
			wantErr:    true,
			errSubstrs: []string{"unknown key", "cacert"},
		},
	})
}

// TestModeClientIsZeroValue pins the zero-value contract: ModeClient MUST be the
// zero value so an unspelled Config{} means client, never embedded. Inserting
// ModeEmbedded before ModeClient in the const block would silently flip this and
// route every zero-valued Config to embedded.
func TestModeClientIsZeroValue(t *testing.T) {
	var zero Mode
	if zero != ModeClient {
		t.Fatalf("zero-value Mode = %v (%d), want ModeClient (0)", zero, int(zero))
	}
	if int(ModeClient) != 0 {
		t.Errorf("ModeClient = %d, want 0", int(ModeClient))
	}
	if int(ModeEmbedded) == 0 {
		t.Errorf("ModeEmbedded = 0, must not share the zero value with ModeClient")
	}
}

// TestLoadAbsentFileIsEmbeddedDefault: an absent app.toml is NOT an error — it
// resolves to the embedded zero-config onboarding default.
func TestLoadAbsentFileIsEmbeddedDefault(t *testing.T) {
	dir := t.TempDir()
	got, err := Load(dir, "", "")
	if err != nil {
		t.Fatalf("absent file: unexpected error: %v", err)
	}
	if want := (Config{Mode: ModeEmbedded}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
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

func TestLoadEmbeddedFile(t *testing.T) {
	dir := writeConfig(t, `mode = "embedded"`)
	got, err := Load(dir, "", "")
	if err != nil {
		t.Fatalf("embedded file: unexpected error: %v", err)
	}
	if want := (Config{Mode: ModeEmbedded}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
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
	got, err := Load("", home, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := (Config{Mode: ModeClient, ServerURL: "https://host:8443"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestLoadOverridePrecedence pins the override > file > default precedence
// (OQ-3). The override is the resolved --mode/$COMPASS_APP_MODE value; the
// caller resolves flag > env into that single string.
func TestLoadOverridePrecedence(t *testing.T) {
	t.Run("override embedded wins over client file", func(t *testing.T) {
		dir := writeConfig(t, "mode = \"client\"\nserver_url = \"https://host:8443\"\n")
		got, err := Load(dir, "", modeStrEmbedded)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := (Config{Mode: ModeEmbedded}); got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("override client with no file needs server_url", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := Load(dir, "", modeStrClient); err == nil {
			t.Fatal("override to client without a server_url: want error, got nil")
		}
	})

	t.Run("override client keeps file server_url", func(t *testing.T) {
		dir := writeConfig(t, "mode = \"client\"\nserver_url = \"https://host:8443\"\n")
		got, err := Load(dir, "", modeStrClient)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := (Config{Mode: ModeClient, ServerURL: "https://host:8443"}); got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("empty override falls through to file", func(t *testing.T) {
		dir := writeConfig(t, "mode = \"client\"\nserver_url = \"https://host:8443\"\n")
		got, err := Load(dir, "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := (Config{Mode: ModeClient, ServerURL: "https://host:8443"}); got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("empty override with absent file → embedded default", func(t *testing.T) {
		dir := t.TempDir()
		got, err := Load(dir, "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := (Config{Mode: ModeEmbedded}); got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("unknown override → error naming both modes", func(t *testing.T) {
		dir := t.TempDir()
		_, err := Load(dir, "", "proxy")
		if err == nil {
			t.Fatal("unknown override: want error, got nil")
		}
		for _, sub := range []string{"proxy", "embedded", "client"} {
			if !strings.Contains(err.Error(), sub) {
				t.Errorf("override error %q missing substring %q", err, sub)
			}
		}
	})
}

func TestModeString(t *testing.T) {
	if got := ModeClient.String(); got != "client" {
		t.Errorf("ModeClient.String() = %q, want client", got)
	}
	if got := ModeEmbedded.String(); got != "embedded" {
		t.Errorf("ModeEmbedded.String() = %q, want embedded", got)
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
