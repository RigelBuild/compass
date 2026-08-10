//go:build podman

package e2e

import (
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

// TestClassifyEnrollProbe pins the pure classification of a StopAgentSession
// enrollment probe result into (ready, retry, cerr) without a live client.
func TestClassifyEnrollProbe(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantReady bool
		wantRetry bool
		wantErr   bool
	}{
		{
			name:      "nil error means enrolled",
			err:       nil,
			wantReady: true,
			wantRetry: false,
			wantErr:   false,
		},
		{
			name:      "unavailable no runner enrolled retries",
			err:       connect.NewError(connect.CodeUnavailable, errors.New("unavailable: no runner enrolled to serve session")),
			wantReady: false,
			wantRetry: true,
			wantErr:   false,
		},
		{
			name:      "internal error is surfaced",
			err:       connect.NewError(connect.CodeInternal, errors.New("boom")),
			wantReady: false,
			wantRetry: false,
			wantErr:   true,
		},
		{
			name:      "unavailable with other message is surfaced not retried",
			err:       connect.NewError(connect.CodeUnavailable, errors.New("some other unavailable")),
			wantReady: false,
			wantRetry: false,
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, retry, cerr := classifyEnrollProbe(tt.err)
			if ready != tt.wantReady {
				t.Errorf("ready = %v, want %v", ready, tt.wantReady)
			}
			if retry != tt.wantRetry {
				t.Errorf("retry = %v, want %v", retry, tt.wantRetry)
			}
			if tt.wantErr {
				if cerr == nil {
					t.Fatalf("cerr = nil, want non-nil")
				}
				if !errors.Is(cerr, tt.err) {
					t.Errorf("cerr = %v, want to wrap %v", cerr, tt.err)
				}
				if !strings.Contains(cerr.Error(), "runner enrollment probe (StopAgentSession)") {
					t.Errorf("cerr = %q, want probe prefix", cerr.Error())
				}
			} else if cerr != nil {
				t.Errorf("cerr = %v, want nil", cerr)
			}
		})
	}
}
