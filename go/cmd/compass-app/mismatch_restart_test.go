//go:build (linux && gtk4) || darwin

package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/embedded"
)

func TestHandleStackVersionMismatch(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "confirm stops stack then reexecutes",
			run: func(t *testing.T) {
				t.Helper()
				var calls []string
				err := handleStackVersionMismatch(
					context.Background(),
					baseParams,
					func() (bool, error) {
						calls = append(calls, "confirm")
						return true, nil
					},
					func(ctx context.Context, args []string) error {
						calls = append(calls, "down")
						if _, ok := ctx.Deadline(); !ok {
							t.Error("down context has no timeout deadline")
						}
						if want := embedded.StackDownArgs(baseParams); !reflect.DeepEqual(args, want) {
							t.Errorf("down args = %v, want %v", args, want)
						}
						return nil
					},
					func() error {
						calls = append(calls, "reexec")
						return nil
					},
					func(message string) { t.Errorf("unexpected error dialog: %q", message) },
				)
				if err != nil {
					t.Fatalf("handleStackVersionMismatch() error = %v", err)
				}
				if want := []string{"confirm", "down", "reexec"}; !reflect.DeepEqual(calls, want) {
					t.Errorf("calls = %v, want %v", calls, want)
				}
			},
		},
		{
			name: "decline touches no stack",
			run: func(t *testing.T) {
				t.Helper()
				downCalled := false
				reexecCalled := false
				err := handleStackVersionMismatch(
					context.Background(), baseParams,
					func() (bool, error) { return false, nil },
					func(context.Context, []string) error { downCalled = true; return nil },
					func() error { reexecCalled = true; return nil },
					func(string) { t.Error("decline showed an error dialog") },
				)
				if err == nil || !strings.Contains(err.Error(), "user declined") {
					t.Errorf("error = %v, want clear decline error", err)
				}
				if downCalled || reexecCalled {
					t.Errorf("decline invoked down=%v reexec=%v", downCalled, reexecCalled)
				}
			},
		},
		{
			name: "down failure shows error and skips reexec",
			run: func(t *testing.T) {
				t.Helper()
				downErr := errors.New("compass-stack down failed: locked")
				reexecCalled := false
				var shown string
				err := handleStackVersionMismatch(
					context.Background(), baseParams,
					func() (bool, error) { return true, nil },
					func(context.Context, []string) error { return downErr },
					func() error { reexecCalled = true; return nil },
					func(message string) { shown = message },
				)
				if !errors.Is(err, downErr) {
					t.Errorf("error = %v, want wrapped down error", err)
				}
				if shown != downErr.Error() {
					t.Errorf("shown error = %q, want %q", shown, downErr.Error())
				}
				if reexecCalled {
					t.Error("reexec called after down failure")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
