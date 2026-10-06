//go:build podman

package e2e

import (
	"strings"
	"testing"
)

// A detached stand-up's failure text is unreadable on its T, so runDetached
// must surface it in the error every leg reports.
func TestRunDetachedCarriesFailureText(t *testing.T) {
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
				if !tb.Failed() {
					tb.Errorf("Failed() = false after Fail()")
				}
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
			err := runDetached(tc.fn)
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
}
