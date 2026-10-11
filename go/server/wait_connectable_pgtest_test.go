//go:build pgtest && unix

package server

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dialUnix(ctx context.Context, network, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

func listenUnix(ctx context.Context, t *testing.T, path string) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
}

func TestWaitConnectable(t *testing.T) {
	t.Run("bound socket is ready", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "up.sock")
		listenUnix(t.Context(), t, path)
		if err := waitConnectable(t.Context(), path, time.Second, dialUnix); err != nil {
			t.Fatalf("waitConnectable on a bound socket = %v, want nil", err)
		}
	})
	t.Run("socket bound after a missed dial is ready", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "late.sock")
		// Bind inside the first failed dial, so success needs a second probe.
		misses := 0
		dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialUnix(ctx, network, addr)
			if err != nil {
				misses++
				if misses == 1 {
					listenUnix(ctx, t, path)
				}
			}
			return conn, err
		}
		if err := waitConnectable(t.Context(), path, 10*time.Second, dial); err != nil {
			t.Fatalf("waitConnectable on a late-bound socket = %v, want nil", err)
		}
		if misses != 1 {
			t.Fatalf("dial missed %d times, want exactly 1 before the bind", misses)
		}
	})
	t.Run("never-bound socket fails at the budget, not before", func(t *testing.T) {
		const budget = 200 * time.Millisecond
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		path := filepath.Join(t.TempDir(), "never.sock")
		start := time.Now()
		err := waitConnectable(ctx, path, budget, dialUnix)
		if elapsed := time.Since(start); elapsed < budget {
			t.Fatalf("waitConnectable returned after %s, before its %s budget", elapsed, budget)
		}
		if err == nil || !strings.Contains(err.Error(), "never became connectable within 200ms") {
			t.Fatalf("waitConnectable on a never-bound socket = %v, want the budget error", err)
		}
	})
	t.Run("canceled context ends the wait before the budget", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		path := filepath.Join(t.TempDir(), "cancel.sock")
		// Cancel inside the first missed dial, so the select sees ctx.Done, not the deadline.
		dial := func(dctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialUnix(dctx, network, addr)
			cancel()
			return conn, err
		}
		start := time.Now()
		err := waitConnectable(ctx, path, time.Minute, dial)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waitConnectable after cancel = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed >= 10*time.Second {
			t.Fatalf("waitConnectable took %s after cancel, want a prompt return", elapsed)
		}
	})
}
