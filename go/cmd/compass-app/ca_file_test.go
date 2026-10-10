//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReadCAFile(t *testing.T) {
	t.Run("regular file reads exactly", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ca.pem")
		want := []byte("-----BEGIN CERTIFICATE-----\ncertificate\n-----END CERTIFICATE-----\n")
		if err := os.WriteFile(path, want, 0o600); err != nil {
			t.Fatalf("write CA fixture: %v", err)
		}
		got, err := readCAFile(path)
		if err != nil {
			t.Fatalf("readCAFile: %v", err)
		}
		if string(got) != string(want) {
			t.Errorf("readCAFile = %q, want exact file bytes %q", got, want)
		}
	})

	t.Run("oversized file is rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "large.pem")
		if err := os.WriteFile(path, make([]byte, maxCAFileBytes+1), 0o600); err != nil {
			t.Fatalf("write oversized CA fixture: %v", err)
		}
		if _, err := readCAFile(path); err == nil || !strings.Contains(err.Error(), "larger than 1 MiB") {
			t.Fatalf("readCAFile error = %v, want oversized CA rejection", err)
		}
	})

	t.Run("FIFO is rejected without blocking", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pipe.pem")
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatalf("create FIFO: %v", err)
		}

		done := make(chan error, 1)
		go func() {
			_, err := readCAFile(path)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("readCAFile error = %v, want FIFO rejection", err)
			}
		case <-time.After(time.Second):
			t.Fatal("readCAFile blocked opening a FIFO")
		}
	})
}
