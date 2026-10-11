//go:build pgtest && unix

package server

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWaitConnectable(t *testing.T) {
	t.Run("bound socket is ready", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "up.sock")
		ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		if err := waitConnectable(t.Context(), path, time.Second); err != nil {
			t.Fatalf("waitConnectable on a bound socket = %v, want nil", err)
		}
	})
	t.Run("never-bound socket fails at the budget", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "never.sock")
		err := waitConnectable(t.Context(), path, 50*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "never became connectable within 50ms") {
			t.Fatalf("waitConnectable on a never-bound socket = %v, want the budget error", err)
		}
	})
}
