//go:build unix

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
)

func TestNoTokenFlag(t *testing.T) {
	root := newRootCmd()

	// Exact-set guard: the root's persistent flags must be EXACTLY this
	// known-good set. Any newly-added credential-bearing flag (--admin-token,
	// --bearer, --token) at the root trips this — the bearer token must never
	// be a flag (it would leak into the process table).
	wantFlags := map[string]bool{
		"server-addr": true,
		"ca":          true,
		"token-file":  true,
		"config":      true,
	}
	got := map[string]bool{}
	root.PersistentFlags().VisitAll(func(f *pflag.Flag) {
		got[f.Name] = true
	})
	for name := range got {
		if !wantFlags[name] {
			t.Errorf("root registers unexpected persistent flag %q; a credential-bearing flag must never be added", name)
		}
	}
	for name := range wantFlags {
		if !got[name] {
			t.Errorf("root is missing expected persistent flag %q", name)
		}
	}

	// Belt and suspenders: no command anywhere in the tree names a --token flag.
	check := func(name string, has bool) {
		if has {
			t.Errorf("command %q registers a --token flag; the bearer token must never be a flag", name)
		}
	}
	var visit func(path string, cmds []*cobra.Command)
	visit = func(path string, cmds []*cobra.Command) {
		for _, c := range cmds {
			full := strings.TrimSpace(path + " " + c.Name())
			check(full, c.Flags().Lookup("token") != nil)
			check(full, c.PersistentFlags().Lookup("token") != nil)
			visit(full, c.Commands())
		}
	}
	check(root.Name(), root.Flags().Lookup("token") != nil)
	check(root.Name(), root.PersistentFlags().Lookup("token") != nil)
	visit(root.Name(), root.Commands())
}

// mountPersistent gives an isolated subcommand the shared connection flags on
// its own flag set, so resolveConn/resolveToken can be exercised without
// building the whole root tree.
func mountPersistent(cmd *cobra.Command) {
	addConnFlags(cmd.Flags())
}

// TestServerAddrPrecedence asserts resolveConn honors flag > env for the server
// address. The flag wins over the env; the env is the fallback; neither set is a
// clear error.
func TestServerAddrPrecedence(t *testing.T) {
	t.Setenv("COMPASS_ADMIN_TOKEN", "tok") // so token resolution never blocks addr resolution

	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv("COMPASS_SERVER_ADDR", "https://env.example")
		cmd := newShowCmd()
		mountPersistent(cmd)
		if err := cmd.Flags().Set("server-addr", "https://flag.example"); err != nil {
			t.Fatalf("Set server-addr: %v", err)
		}
		cfg, err := resolveConn(cmd)
		if err != nil {
			t.Fatalf("resolveConn: %v", err)
		}
		if cfg.serverAddr != "https://flag.example" {
			t.Errorf("serverAddr = %q, want the flag value", cfg.serverAddr)
		}
	})

	t.Run("env fallback when flag unset", func(t *testing.T) {
		t.Setenv("COMPASS_SERVER_ADDR", "https://env.example")
		cmd := newShowCmd()
		mountPersistent(cmd)
		cfg, err := resolveConn(cmd)
		if err != nil {
			t.Fatalf("resolveConn: %v", err)
		}
		if cfg.serverAddr != "https://env.example" {
			t.Errorf("serverAddr = %q, want the env value", cfg.serverAddr)
		}
	})

	t.Run("env wins over config file", func(t *testing.T) {
		t.Setenv("COMPASS_SERVER_ADDR", "https://env.example")
		cfgPath := filepath.Join(t.TempDir(), "compass.json")
		if err := os.WriteFile(cfgPath, []byte(`{"server-addr":"https://config.example"}`), 0o600); err != nil {
			t.Fatalf("WriteFile config: %v", err)
		}
		cmd := newShowCmd()
		mountPersistent(cmd)
		if err := cmd.Flags().Set("config", cfgPath); err != nil {
			t.Fatalf("Set config: %v", err)
		}
		cfg, err := resolveConn(cmd)
		if err != nil {
			t.Fatalf("resolveConn: %v", err)
		}
		if cfg.serverAddr != "https://env.example" {
			t.Errorf("serverAddr = %q, want the env value (env outranks the config file)", cfg.serverAddr)
		}
	})

	t.Run("config file fallback when flag and env unset", func(t *testing.T) {
		t.Setenv("COMPASS_SERVER_ADDR", "")
		cfgPath := filepath.Join(t.TempDir(), "compass.json")
		if err := os.WriteFile(cfgPath, []byte(`{"server-addr":"https://config.example"}`), 0o600); err != nil {
			t.Fatalf("WriteFile config: %v", err)
		}
		cmd := newShowCmd()
		mountPersistent(cmd)
		if err := cmd.Flags().Set("config", cfgPath); err != nil {
			t.Fatalf("Set config: %v", err)
		}
		cfg, err := resolveConn(cmd)
		if err != nil {
			t.Fatalf("resolveConn: %v", err)
		}
		if cfg.serverAddr != "https://config.example" {
			t.Errorf("serverAddr = %q, want the config value", cfg.serverAddr)
		}
	})

	t.Run("missing addr is an error", func(t *testing.T) {
		t.Setenv("COMPASS_SERVER_ADDR", "")
		cmd := newShowCmd()
		mountPersistent(cmd)
		if _, err := resolveConn(cmd); err == nil {
			t.Fatal("resolveConn with no addr = nil error, want a rejection")
		}
	})
}

// TestServerAddrScheme asserts resolveConn rejects a remote http:// address (so
// the admin bearer token is never sent in cleartext), accepts https://, and
// allows http:// only for a loopback host (the local dogfood door).
func TestServerAddrScheme(t *testing.T) {
	t.Setenv("COMPASS_ADMIN_TOKEN", "tok") // so token resolution never blocks addr resolution

	cases := []struct {
		name    string
		addr    string
		wantErr bool
	}{
		{"remote http rejected", "http://server.example:443", true},
		{"https accepted", "https://server.example:443", false},
		{"http localhost accepted", "http://localhost:8080", false},
		{"http 127.0.0.1 accepted", "http://127.0.0.1:8080", false},
		{"http ipv6 loopback accepted", "http://[::1]:8080", false},
		{"http loopback with userinfo accepted", "http://u:p@127.0.0.1:8080", false},
		{"uppercase scheme remote rejected", "HTTP://server.example:443", true},
		{"loopback suffix spoof rejected", "http://127.0.0.1.evil.com:8080", true},
		{"localhost suffix spoof rejected", "http://localhost.evil:8080", true},
		{"userinfo host spoof rejected", "http://127.0.0.1@evil.com:8080", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COMPASS_SERVER_ADDR", tc.addr)
			cmd := newShowCmd()
			mountPersistent(cmd)
			_, err := resolveConn(cmd)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveConn(%q) = nil error, want a cleartext-token rejection", tc.addr)
				}
				if !strings.Contains(err.Error(), "cleartext") {
					t.Errorf("resolveConn(%q) error %q does not name the cleartext risk", tc.addr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveConn(%q) = %v, want nil", tc.addr, err)
			}
		})
	}
}

// TestTokenResolution asserts the env token wins, the file is the fallback, and
// neither set is an error — never a flag path.
func TestTokenResolution(t *testing.T) {
	t.Run("env wins", func(t *testing.T) {
		t.Setenv("COMPASS_ADMIN_TOKEN", "envtok")
		cmd := newShowCmd()
		mountPersistent(cmd)
		tok, err := resolveToken(cmd)
		if err != nil {
			t.Fatalf("resolveToken: %v", err)
		}
		if tok != "envtok" {
			t.Errorf("token = %q, want envtok", tok)
		}
	})

	t.Run("file fallback", func(t *testing.T) {
		t.Setenv("COMPASS_ADMIN_TOKEN", "")
		path := filepath.Join(t.TempDir(), "tok")
		if err := os.WriteFile(path, []byte("filetok\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		cmd := newShowCmd()
		mountPersistent(cmd)
		if err := cmd.Flags().Set("token-file", path); err != nil {
			t.Fatalf("Set token-file: %v", err)
		}
		tok, err := resolveToken(cmd)
		if err != nil {
			t.Fatalf("resolveToken: %v", err)
		}
		if tok != "filetok" {
			t.Errorf("token = %q, want filetok (trimmed)", tok)
		}
	})

	t.Run("missing both is an error", func(t *testing.T) {
		t.Setenv("COMPASS_ADMIN_TOKEN", "")
		cmd := newShowCmd()
		mountPersistent(cmd)
		if _, err := resolveToken(cmd); err == nil {
			t.Fatal("resolveToken with no env and no file = nil error, want a rejection")
		}
	})
}

// fakeCompass is a fake CompassService handler recording the requests each verb
// constructs, so the subcommand RPC wiring is tested without a live Server or
// Postgres.
type fakeCompass struct {
	compassv1connect.UnimplementedCompassServiceHandler
	gotBundle   []byte
	putVersion  string
	info        *compassv1.GetAgentConfigInfoResponse
	deleteCalls int
	gotAuth     string
}

func (f *fakeCompass) PutAgentConfig(_ context.Context, req *connect.Request[compassv1.PutAgentConfigRequest]) (*connect.Response[compassv1.PutAgentConfigResponse], error) {
	f.gotBundle = req.Msg.GetBundle()
	f.gotAuth = req.Header().Get("Authorization")
	return connect.NewResponse(&compassv1.PutAgentConfigResponse{Version: f.putVersion}), nil
}

func (f *fakeCompass) GetAgentConfigInfo(_ context.Context, req *connect.Request[compassv1.GetAgentConfigInfoRequest]) (*connect.Response[compassv1.GetAgentConfigInfoResponse], error) {
	f.gotAuth = req.Header().Get("Authorization")
	info := f.info
	if info == nil {
		info = &compassv1.GetAgentConfigInfoResponse{}
	}
	return connect.NewResponse(info), nil
}

func (f *fakeCompass) DeleteAgentConfig(_ context.Context, req *connect.Request[compassv1.DeleteAgentConfigRequest]) (*connect.Response[compassv1.DeleteAgentConfigResponse], error) {
	f.deleteCalls++
	f.gotAuth = req.Header().Get("Authorization")
	return connect.NewResponse(&compassv1.DeleteAgentConfigResponse{}), nil
}

// startFakeServer stands up the fake CompassService over a plain-HTTP httptest
// server and returns a client wired to it with the bearer interceptor.
func startFakeServer(t *testing.T, fake *fakeCompass) compassv1connect.CompassServiceClient {
	t.Helper()
	path, handler := compassv1connect.NewCompassServiceHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := newClient(connConfig{serverAddr: srv.URL, token: "test-token"})
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return client
}

// TestRunPush asserts push builds a bundle, stamps the bearer token, and prints
// the returned version.
func TestRunPush(t *testing.T) {
	fake := &fakeCompass{putVersion: "abc123"}
	client := startFakeServer(t, fake)
	root := writeBundleDir(t, map[string]string{"skills/alpha/SKILL.md": "# a"})

	var out strings.Builder
	if err := runPush(context.Background(), client, root, &out); err != nil {
		t.Fatalf("runPush: %v", err)
	}
	if len(fake.gotBundle) == 0 {
		t.Error("PutAgentConfig received an empty bundle")
	}
	if fake.gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want Bearer test-token", fake.gotAuth)
	}
	if !strings.Contains(out.String(), "abc123") {
		t.Errorf("push output %q does not report the version", out.String())
	}
}

// TestRunShow covers a populated fleet and the unconfigured (empty) fleet, which
// must render as a clear message, not an error.
func TestRunShow(t *testing.T) {
	t.Run("populated", func(t *testing.T) {
		fake := &fakeCompass{info: &compassv1.GetAgentConfigInfoResponse{
			Version:    "v1",
			Skills:     []string{"alpha"},
			Extensions: []string{"beta"},
			McpServers: []string{"gamma"},
		}}
		client := startFakeServer(t, fake)
		var out strings.Builder
		if err := runShow(context.Background(), client, &out); err != nil {
			t.Fatalf("runShow: %v", err)
		}
		for _, want := range []string{"v1", "alpha", "beta", "gamma"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("show output %q missing %q", out.String(), want)
			}
		}
	})

	t.Run("unconfigured", func(t *testing.T) {
		fake := &fakeCompass{info: &compassv1.GetAgentConfigInfoResponse{}}
		client := startFakeServer(t, fake)
		var out strings.Builder
		if err := runShow(context.Background(), client, &out); err != nil {
			t.Fatalf("runShow(empty) = %v, want nil (empty is valid, not an error)", err)
		}
		if !strings.Contains(out.String(), "no config") {
			t.Errorf("show output %q does not report the unconfigured state", out.String())
		}
	})
}

// TestRenderConfigInfoAllBuckets asserts renderConfigInfo surfaces every
// category the door accepts: the multi-member name buckets (rules, subagents)
// and the singleton presence flags (settings, AGENTS.md, models.yml), alongside
// the existing skills/extensions/mcp buckets. It asserts the EXACT present/absent
// rendering for the singleton flags across both branches.
func TestRenderConfigInfoAllBuckets(t *testing.T) {
	t.Run("all present", func(t *testing.T) {
		msg := &compassv1.GetAgentConfigInfoResponse{
			Version:     "v9",
			Skills:      []string{"alpha"},
			Extensions:  []string{"beta"},
			McpServers:  []string{"gamma"},
			Rules:       []string{"delta", "epsilon"},
			Subagents:   []string{"zeta"},
			HasSettings: true,
			HasAgentsMd: true,
			HasModels:   true,
		}
		var out strings.Builder
		if err := renderConfigInfo(&out, msg); err != nil {
			t.Fatalf("renderConfigInfo: %v", err)
		}
		for _, want := range []string{"v9", "alpha", "beta", "gamma", "delta", "epsilon", "zeta"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("render output %q missing %q", out.String(), want)
			}
		}
		for _, want := range []string{"settings: present", "AGENTS.md: present", "models.yml: present"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("render output %q missing line %q", out.String(), want)
			}
		}
	})
	t.Run("all absent", func(t *testing.T) {
		msg := &compassv1.GetAgentConfigInfoResponse{
			Version:     "v9",
			HasSettings: false,
			HasAgentsMd: false,
			HasModels:   false,
		}
		var out strings.Builder
		if err := renderConfigInfo(&out, msg); err != nil {
			t.Fatalf("renderConfigInfo: %v", err)
		}
		for _, want := range []string{"settings: absent", "AGENTS.md: absent", "models.yml: absent"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("render output %q missing line %q", out.String(), want)
			}
		}
	})
}

// TestRunDelete asserts delete calls DeleteAgentConfig and confirms.
func TestRunDelete(t *testing.T) {
	fake := &fakeCompass{}
	client := startFakeServer(t, fake)
	var out strings.Builder
	if err := runDelete(context.Background(), client, &out); err != nil {
		t.Fatalf("runDelete: %v", err)
	}
	if fake.deleteCalls != 1 {
		t.Errorf("DeleteAgentConfig calls = %d, want 1", fake.deleteCalls)
	}
	if !strings.Contains(out.String(), "cleared") {
		t.Errorf("delete output %q does not confirm", out.String())
	}
}
