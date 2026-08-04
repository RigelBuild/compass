//go:build unix

package adapters

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakePuller records the ref it was asked to pull and returns a configured
// error, so the ensure logic can be exercised without a real podman.
type fakePuller struct {
	err     error
	calls   int
	lastRef string
}

func (f *fakePuller) Pull(_ context.Context, image string) error {
	f.calls++
	f.lastRef = image
	return f.err
}

func TestEnsureImage(t *testing.T) {
	sentinel := errors.New("podman: manifest unknown")

	tests := []struct {
		name       string
		image      string
		pullErr    error
		wantCalls  int
		wantErr    bool
		wantInMsg  string
		wantUnwrap error
	}{
		{
			name:      "happy path pulls the exact ref once",
			image:     "ghcr.io/x/agent:git-abc",
			wantCalls: 1,
		},
		{
			name:      "empty image errors without pulling",
			image:     "",
			wantCalls: 0,
			wantErr:   true,
		},
		{
			name:       "pull error is wrapped with the ref",
			image:      "ghcr.io/x/agent:git-abc",
			pullErr:    sentinel,
			wantCalls:  1,
			wantErr:    true,
			wantInMsg:  "ghcr.io/x/agent:git-abc",
			wantUnwrap: sentinel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakePuller{err: tt.pullErr}
			err := newImageEnsurer(fake).EnsureImage(t.Context(), tt.image)

			if tt.wantErr && err == nil {
				t.Fatalf("EnsureImage(%q) = nil, want error", tt.image)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("EnsureImage(%q) = %v, want nil", tt.image, err)
			}
			if fake.calls != tt.wantCalls {
				t.Fatalf("Pull calls = %d, want %d", fake.calls, tt.wantCalls)
			}
			if tt.wantCalls == 1 && fake.lastRef != tt.image {
				t.Fatalf("Pull ref = %q, want %q", fake.lastRef, tt.image)
			}
			if tt.wantInMsg != "" && !strings.Contains(err.Error(), tt.wantInMsg) {
				t.Fatalf("error %q does not contain ref %q", err, tt.wantInMsg)
			}
			if tt.wantUnwrap != nil && !errors.Is(err, tt.wantUnwrap) {
				t.Fatalf("errors.Is(%v, sentinel) = false, want true", err)
			}
		})
	}
}
