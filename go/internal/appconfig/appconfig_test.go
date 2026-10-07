package appconfig

import (
	"bytes"
	"errors"
	"log/slog"
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

func TestLoadAbsentFileReturnsErrNoConfig(t *testing.T) {
	_, err := Load(t.TempDir(), "", "")
	if !errors.Is(err, ErrNoConfig) {
		t.Fatalf("Load absent file error = %v, want ErrNoConfig", err)
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
		if _, err := Load(dir, "", modeStrClient); err == nil || errors.Is(err, ErrNoConfig) {
			t.Fatalf("client override without a server_url error = %v, want parseClient validation error", err)
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

	t.Run("no override without file returns ErrNoConfig", func(t *testing.T) {
		_, err := Load(t.TempDir(), "", "")
		if !errors.Is(err, ErrNoConfig) {
			t.Fatalf("Load absent file error = %v, want ErrNoConfig", err)
		}
	})

	t.Run("embedded override without file remains embedded", func(t *testing.T) {
		got, err := Load(t.TempDir(), "", modeStrEmbedded)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := (Config{Mode: ModeEmbedded}); got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("unknown override remains an error", func(t *testing.T) {
		_, err := Load(t.TempDir(), "", "proxy")
		if err == nil || !strings.Contains(err.Error(), "proxy") {
			t.Fatalf("unknown override error = %v, want error naming proxy", err)
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

func TestNormalizeServerURL(t *testing.T) {
	const (
		reasonHTTPS = "The server URL must use https."
		reasonHost  = "The server URL must include a host."
		reasonCreds = "The server URL must not include credentials."
		reasonPath  = "The server URL must not include a path, query, or fragment."
	)
	tests := []struct {
		name       string
		raw        string
		want       string
		wantReason string
		wantError  string
	}{
		{name: "trim and trailing slash", raw: " https://h:8443/ ", want: "https://h:8443"},
		{
			name:       "http scheme keeps legacy error",
			raw:        "http://h",
			wantReason: reasonHTTPS,
			wantError:  `appconfig: server_url "http://h" must use https (got scheme "http"): cleartext connections are not allowed`,
		},
		{
			name:       "relative host-form keeps legacy error",
			raw:        "h:8443",
			wantReason: reasonHTTPS,
			wantError:  `appconfig: server_url "h:8443" must use https (got scheme "h"): cleartext connections are not allowed`,
		},
		{
			name:       "relative path keeps legacy error",
			raw:        "/x",
			wantReason: reasonHTTPS,
			wantError:  `appconfig: server_url "/x" must use https (got scheme ""): cleartext connections are not allowed`,
		},
		{
			name:       "credentials keep legacy error",
			raw:        "https://u:p@h",
			wantReason: reasonCreds,
			wantError:  `appconfig: server_url "https://u:p@h" must not embed credentials; the bearer token is entered in the connect screen and stored in the OS keychain (DL-109)`,
		},
		{
			name:       "path",
			raw:        "https://h/p",
			wantReason: reasonPath,
			wantError:  `appconfig: server_url "https://h/p" must not include a path, query, or fragment (e.g. https://host:8443)`,
		},
		{name: "trailing path", raw: "https://h/p/", wantReason: reasonPath},
		{name: "query", raw: "https://h?x=1", wantReason: reasonPath},
		{name: "fragment", raw: "https://h#f", wantReason: reasonPath},
		{name: "malformed escape", raw: "%", wantReason: "The server URL is not valid."},
		{name: "missing host", raw: "https:///x", wantReason: reasonHost},
		{name: "port without hostname", raw: "https://:443", wantReason: reasonHost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeServerURL(tt.raw)
			if tt.want != "" {
				if err != nil {
					t.Fatalf("NormalizeServerURL(%q): %v", tt.raw, err)
				}
				if got != tt.want {
					t.Errorf("NormalizeServerURL(%q) = %q, want %q", tt.raw, got, tt.want)
				}
				return
			}
			var urlErr *URLError
			if !errors.As(err, &urlErr) {
				t.Fatalf("NormalizeServerURL(%q) error = %v (%T), want *URLError", tt.raw, err, err)
			}
			if urlErr.URL != tt.raw || urlErr.Reason != tt.wantReason {
				t.Errorf("URLError = {URL: %q, Reason: %q}, want {%q, %q}", urlErr.URL, urlErr.Reason, tt.raw, tt.wantReason)
			}
			if tt.wantError != "" && err.Error() != tt.wantError {
				t.Errorf("Error() = %q, want %q", err, tt.wantError)
			}
		})
	}
}

// A dangling app.toml symlink blocks the exclusive save, so Load must not
// report it as a first run the chooser could then never complete.
func TestLoadDanglingConfigSymlinkIsNotNoConfig(t *testing.T) {
	dir := t.TempDir()
	path, err := ConfigPath(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "gone.toml"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "", ""); err == nil || errors.Is(err, ErrNoConfig) {
		t.Fatalf("Load with a dangling app.toml symlink = %v, want a read error that is not ErrNoConfig", err)
	}
	if err := SaveEmbedded(path); !errors.Is(err, ErrConfigExists) {
		t.Fatalf("SaveEmbedded over a dangling symlink = %v, want ErrConfigExists", err)
	}
	// An explicit override still resolves, as with any unreadable-as-absent file.
	if got, err := Load(dir, "", "embedded"); err != nil || got.Mode != ModeEmbedded {
		t.Fatalf("Load with a dangling symlink and the embedded override = (%v, %v), want embedded", got.Mode, err)
	}
}

func TestLoadNormalizesAndRejectsServerURLPath(t *testing.T) {
	t.Run("trailing slash is normalized", func(t *testing.T) {
		dir := writeConfig(t, "mode = \"client\"\nserver_url = \"https://h:8443/\"\n")
		got, err := Load(dir, "", "")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.ServerURL != "https://h:8443" {
			t.Errorf("ServerURL = %q, want https://h:8443", got.ServerURL)
		}
	})

	t.Run("path is rejected", func(t *testing.T) {
		dir := writeConfig(t, "mode = \"client\"\nserver_url = \"https://h/p\"\n")
		if _, err := Load(dir, "", ""); err == nil {
			t.Fatal("Load pathful server URL: want error, got nil")
		}
	})
}

func TestConfigPath(t *testing.T) {
	tests := []struct {
		name       string
		configHome string
		home       string
		want       string
		wantErr    bool
	}{
		{
			name:       "config home takes precedence",
			configHome: "/config",
			home:       "/home/user",
			want:       filepath.Join("/config", "compass", "app.toml"),
		},
		{
			name: "home fallback",
			home: "/home/user",
			want: filepath.Join("/home/user", ".config", "compass", "app.toml"),
		},
		{name: "missing homes", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ConfigPath(tt.configHome, tt.home)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ConfigPath error = %v, wantErr %t", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ConfigPath = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSaveClient(t *testing.T) {
	tests := []struct {
		name  string
		caPEM []byte
	}{
		{name: "with CA", caPEM: []byte("test CA bytes")},
		{name: "system trust"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertSavedClient(t, tt.caPEM)
		})
	}
}

func assertSavedClient(t *testing.T, caPEM []byte) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "compass", "app.toml")
	cfg, err := SaveClient(path, Config{
		Mode:      ModeClient,
		ServerURL: " https://h:8443/ ",
		CACert:    "input path is not persisted",
	}, caPEM)
	if err != nil {
		t.Fatalf("SaveClient: %v", err)
	}
	if cfg.Mode != ModeClient || cfg.ServerURL != "https://h:8443" {
		t.Errorf("SaveClient returned %+v, want normalized client config", cfg)
	}
	loaded, err := Load(root, "", "")
	if err != nil {
		t.Fatalf("Load saved config: %v", err)
	}
	if loaded.Mode != ModeClient || loaded.ServerURL != "https://h:8443" {
		t.Errorf("Load returned %+v, want normalized client config", loaded)
	}
	assertSavedCACert(t, path, cfg, loaded, caPEM)
	assertFileMode(t, filepath.Dir(path), 0o700)
	assertFileMode(t, path, 0o600)
	if loaded.CACert != "" {
		assertFileMode(t, loaded.CACert, 0o600)
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".app.toml-*.tmp"))
	if err != nil || len(temps) != 0 {
		t.Errorf("temp files = %v, error %v; want none", temps, err)
	}
}

func assertSavedCACert(t *testing.T, path string, cfg, loaded Config, caPEM []byte) {
	t.Helper()
	if len(caPEM) == 0 {
		if cfg.CACert != "" || loaded.CACert != "" {
			t.Errorf("without CA, SaveClient CACert = %q and Load CACert = %q", cfg.CACert, loaded.CACert)
		}
		certs, err := filepath.Glob(filepath.Join(filepath.Dir(path), "server-ca-*.pem"))
		if err != nil || len(certs) != 0 {
			t.Errorf("CA files = %v, error %v; want none", certs, err)
		}
		return
	}
	if filepath.Dir(loaded.CACert) != filepath.Dir(path) || !strings.HasPrefix(filepath.Base(loaded.CACert), "server-ca-") || filepath.Ext(loaded.CACert) != ".pem" {
		t.Fatalf("CACert path %q is not a server-ca-*.pem beside app.toml", loaded.CACert)
	}
	gotPEM, err := os.ReadFile(loaded.CACert)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotPEM, caPEM) {
		t.Errorf("CA file = %q, want %q", gotPEM, caPEM)
	}
	if cfg.CACert != loaded.CACert {
		t.Errorf("SaveClient CACert = %q, Load CACert = %q", cfg.CACert, loaded.CACert)
	}
}

func TestSaveEmbedded(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "compass", "app.toml")
	if err := SaveEmbedded(path); err != nil {
		t.Fatalf("SaveEmbedded: %v", err)
	}
	got, err := Load(root, "", "")
	if err != nil {
		t.Fatalf("Load saved config: %v", err)
	}
	if want := (Config{Mode: ModeEmbedded}); got != want {
		t.Errorf("Load returned %+v, want %+v", got, want)
	}
	assertFileMode(t, filepath.Dir(path), 0o700)
	assertFileMode(t, path, 0o600)
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".app.toml-*.tmp"))
	if err != nil || len(temps) != 0 {
		t.Errorf("temp files = %v, error %v; want none", temps, err)
	}
}

func TestSaveFailuresLeaveNoArtifacts(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "invalid URL", cfg: Config{Mode: ModeClient, ServerURL: "http://h"}},
		{name: "wrong mode", cfg: Config{Mode: ModeEmbedded, ServerURL: "https://h"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "compass", "app.toml")
			if _, err := SaveClient(path, tt.cfg, []byte("new CA")); err == nil {
				t.Fatal("SaveClient: want error, got nil")
			}
			assertNoSaveArtifacts(t, path)
		})
	}

}

func TestSaveExistingConfigDoesNotReplaceFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compass", "app.toml")
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	wantConfig := []byte("hand written config")
	caPath := filepath.Join(dir, "server-ca-existing.pem")
	wantCA := []byte("existing CA bytes")
	if err := os.WriteFile(path, wantConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, wantCA, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, save := range []struct {
		name string
		run  func() error
	}{
		{name: "client", run: func() error {
			_, err := SaveClient(path, Config{Mode: ModeClient, ServerURL: "https://h"}, []byte("new CA"))
			return err
		}},
		{name: "embedded", run: func() error { return SaveEmbedded(path) }},
	} {
		t.Run(save.name, func(t *testing.T) {
			if err := save.run(); !errors.Is(err, ErrConfigExists) {
				t.Fatalf("save error = %v, want ErrConfigExists", err)
			}
			gotConfig, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			gotCA, err := os.ReadFile(caPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(gotConfig) != string(wantConfig) || string(gotCA) != string(wantCA) {
				t.Errorf("existing config or CA changed: config %q, CA %q", gotConfig, gotCA)
			}
			certs, err := filepath.Glob(filepath.Join(dir, "server-ca-*.pem"))
			if err != nil || len(certs) != 1 || certs[0] != caPath {
				t.Errorf("CA files = %v, error %v; want only %q", certs, err, caPath)
			}
			temps, err := filepath.Glob(filepath.Join(dir, ".app.toml-*.tmp"))
			if err != nil || len(temps) != 0 {
				t.Errorf("temp files = %v, error %v; want none", temps, err)
			}
		})
	}
}

func TestSaveDirectorySyncFailureAfterLinkSucceeds(t *testing.T) {
	previousSync := syncDirectory
	syncDirectory = func(string) error { return errors.New("injected directory sync failure") }
	t.Cleanup(func() { syncDirectory = previousSync })

	previousLogger := slog.Default()
	var logOutput bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logOutput, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	root := t.TempDir()
	path := filepath.Join(root, "compass", "app.toml")
	if err := SaveEmbedded(path); err != nil {
		t.Fatalf("SaveEmbedded after directory sync failure: %v", err)
	}
	if !strings.Contains(logOutput.String(), "appconfig: syncing config directory after save") {
		t.Errorf("directory sync failure log = %q, want error message", logOutput.String())
	}
	got, err := Load(root, "", "")
	if err != nil {
		t.Fatalf("Load linked config: %v", err)
	}
	if got.Mode != ModeEmbedded {
		t.Errorf("Load mode = %v, want embedded", got.Mode)
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".app.toml-*.tmp"))
	if err != nil || len(temps) != 0 {
		t.Errorf("temp files = %v, error %v; want none", temps, err)
	}
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("mode of %q = %04o, want %04o", path, got, want)
	}
}

func assertNoSaveArtifacts(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("app.toml Lstat error = %v, want not-exist", err)
	}
	dir := filepath.Dir(path)
	for _, pattern := range []string{
		filepath.Join(dir, ".app.toml-*.tmp"),
		filepath.Join(dir, "server-ca-*.pem"),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) != 0 {
			t.Errorf("matches for %q = %v, error %v; want none", pattern, matches, err)
		}
	}
}
