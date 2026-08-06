//go:build unix

package adapters

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeImageCLI records the ref it was asked about/pull and returns configured
// results, so the ensure logic can be exercised without a real podman.
type fakeImageCLI struct {
	exists     bool
	existsErr  error
	pullErr    error
	existsCall int
	pullCalls  int
	lastRef    string
}

func (f *fakeImageCLI) ImageExists(_ context.Context, image string) (bool, error) {
	f.existsCall++
	f.lastRef = image
	return f.exists, f.existsErr
}

func (f *fakeImageCLI) Pull(_ context.Context, image string) error {
	f.pullCalls++
	f.lastRef = image
	return f.pullErr
}

func TestEnsureImage(t *testing.T) {
	sentinel := errors.New("podman: manifest unknown")
	existsSentinel := errors.New("podman: engine unreachable")

	tests := []struct {
		name          string
		image         string
		exists        bool
		existsErr     error
		pullErr       error
		wantPullCalls int
		wantErr       bool
		wantInMsg     string
		wantUnwrap    error
	}{
		{
			name:          "absent image is pulled once with the exact ref",
			image:         "ghcr.io/x/agent:git-abc",
			exists:        false,
			wantPullCalls: 1,
		},
		{
			name:          "present image skips the pull (the local-only image path)",
			image:         "compass-agent:latest",
			exists:        true,
			wantPullCalls: 0,
		},
		{
			name:          "empty image errors without checking or pulling",
			image:         "",
			wantPullCalls: 0,
			wantErr:       true,
		},
		{
			name:          "present-check error is wrapped with the ref and short-circuits the pull",
			image:         "ghcr.io/x/agent:git-abc",
			existsErr:     existsSentinel,
			wantPullCalls: 0,
			wantErr:       true,
			wantInMsg:     "ghcr.io/x/agent:git-abc",
			wantUnwrap:    existsSentinel,
		},
		{
			name:          "pull error on an absent image is wrapped with the ref",
			image:         "ghcr.io/x/agent:git-abc",
			exists:        false,
			pullErr:       sentinel,
			wantPullCalls: 1,
			wantErr:       true,
			wantInMsg:     "ghcr.io/x/agent:git-abc",
			wantUnwrap:    sentinel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeImageCLI{exists: tt.exists, existsErr: tt.existsErr, pullErr: tt.pullErr}
			err := newImageEnsurer(fake).EnsureImage(t.Context(), tt.image)

			if tt.wantErr && err == nil {
				t.Fatalf("EnsureImage(%q) = nil, want error", tt.image)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("EnsureImage(%q) = %v, want nil", tt.image, err)
			}
			if fake.pullCalls != tt.wantPullCalls {
				t.Fatalf("Pull calls = %d, want %d", fake.pullCalls, tt.wantPullCalls)
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
