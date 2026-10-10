//go:build (linux && gtk4) || darwin

package main

import (
	"path/filepath"
	"testing"
)

// TestResolveSocket: flag wins, then $COMPASS_SOCKET, then an ABSOLUTE
// $XDG_RUNTIME_DIR/compass/server.sock. A RELATIVE $XDG_RUNTIME_DIR is treated as
// unset and falls through to $HOME/.compass/server.sock — the determinism guard.
func TestResolveSocket(t *testing.T) {
	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("COMPASS_SOCKET", "/env/server.sock")
		if got := resolveSocket("/flag/server.sock"); got != "/flag/server.sock" {
			t.Errorf("got %q, want the flag value", got)
		}
	})
	t.Run("env wins", func(t *testing.T) {
		t.Setenv("COMPASS_SOCKET", "/env/server.sock")
		t.Setenv("XDG_RUNTIME_DIR", "/xdg/run")
		if got := resolveSocket(""); got != "/env/server.sock" {
			t.Errorf("got %q, want the env value", got)
		}
	})
	t.Run("absolute XDG_RUNTIME_DIR", func(t *testing.T) {
		t.Setenv("COMPASS_SOCKET", "")
		xdg := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", xdg)
		if got := resolveSocket(""); got != filepath.Join(xdg, "compass", "server.sock") {
			t.Errorf("got %q, want %q", got, filepath.Join(xdg, "compass", "server.sock"))
		}
	})
	t.Run("relative XDG_RUNTIME_DIR falls through to HOME/.compass", func(t *testing.T) {
		t.Setenv("COMPASS_SOCKET", "")
		t.Setenv("XDG_RUNTIME_DIR", "rel/run")
		home := t.TempDir()
		t.Setenv("HOME", home)
		if got := resolveSocket(""); got != filepath.Join(home, ".compass", "server.sock") {
			t.Errorf("got %q, want %q (relative XDG_RUNTIME_DIR must fall through)", got, filepath.Join(home, ".compass", "server.sock"))
		}
	})
}

// TestResolveMode: flag wins, then $COMPASS_APP_MODE, then "" (no override).
func TestResolveMode(t *testing.T) {
	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("COMPASS_APP_MODE", "client")
		if got := resolveMode("embedded"); got != "embedded" {
			t.Errorf("got %q, want the flag value", got)
		}
	})
	t.Run("env wins", func(t *testing.T) {
		t.Setenv("COMPASS_APP_MODE", "client")
		if got := resolveMode(""); got != "client" {
			t.Errorf("got %q, want the env value", got)
		}
	})
	t.Run("both empty", func(t *testing.T) {
		t.Setenv("COMPASS_APP_MODE", "")
		if got := resolveMode(""); got != "" {
			t.Errorf("got %q, want empty (no override)", got)
		}
	})
}
