//go:build podman

package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A detached stand-up's failure text is unreadable on its T, so runDetached
// must surface it in the error every leg reports.
func TestRunDetachedCarriesFailureText(t *testing.T) {
	var failedAfterFail bool
	cases := []struct {
		name    string
		fn      func(testing.TB)
		wantErr []string
	}{
		{
			name: "completes",
			fn:   func(testing.TB) {},
		},
		{
			name: "fatalf stops the goroutine and keeps the message",
			fn: func(tb testing.TB) {
				tb.Helper()
				tb.Fatalf("opening store: %s", "duplicate migration version 4")
				tb.Errorf("unreachable after Fatalf")
			},
			wantErr: []string{"opening store: duplicate migration version 4"},
		},
		{
			name: "errorf without fatal still fails",
			fn: func(tb testing.TB) {
				tb.Helper()
				tb.Errorf("canned model server Close: %s", "closed")
			},
			wantErr: []string{"canned model server Close: closed"},
		},
		{
			name: "fail without a message still fails",
			fn: func(tb testing.TB) {
				tb.Helper()
				tb.Fail()
				failedAfterFail = tb.Failed()
			},
			wantErr: []string{"Fail called"},
		},
		{
			name: "panic is reported",
			fn: func(testing.TB) {
				var step func()
				step()
			},
			wantErr: []string{"panic during shared stand-up"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runDetached(tc.fn)
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("runDetached = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("runDetached = nil, want error containing %q", tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("runDetached error %q does not contain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "unreachable") {
				t.Errorf("runDetached error %q includes text logged after Fatalf", err)
			}
		})
	}
	if !failedAfterFail {
		t.Errorf("Failed() = false after Fail(); a bare Fail must read as failed")
	}
}

// The detached T's cleanups never run, so the stand-up's TempDirs must come
// back to the caller, and a TempDir failure must reach the recorder.
func TestRunDetachedReturnsTempDirs(t *testing.T) {
	var made []string
	dirs, err := runDetached(func(tb testing.TB) {
		tb.Helper()
		made = append(made, tb.TempDir(), tb.TempDir())
	})
	if err != nil {
		t.Fatalf("runDetached = %v, want nil", err)
	}
	defer removeAll(dirs)
	if !slices.Equal(dirs, made) {
		t.Fatalf("runDetached dirs = %v, want %v", dirs, made)
	}
	for _, d := range dirs {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("TempDir %q missing before cleanup: %v", d, err)
		}
	}

	t.Setenv("GOTMPDIR", filepath.Join(t.TempDir(), "absent"))
	_, err = runDetached(func(tb testing.TB) {
		tb.Helper()
		tb.TempDir()
	})
	if err == nil || !strings.Contains(err.Error(), "TempDir:") {
		t.Fatalf("runDetached with an unusable GOTMPDIR = %v, want the TempDir error", err)
	}
}
