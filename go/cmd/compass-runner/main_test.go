//go:build unix

package main

import (
	"strings"
	"testing"

	"github.com/sealedsecurity/compass/go/internal/runtime"
)

// parseMount is the operator surface for --mount: a malformed value must be
// rejected at flag-parse with a message an operator can act on (it names the bad
// input and the host:container[:ro] shape), and a well-formed value must reach
// SpecDefaults.Mounts intact — the ':ro' suffix is the load-bearing bit that
// makes a mount read-only, so ReadOnly must be exact.
func TestParseMount(t *testing.T) {
	okCases := []struct {
		name string
		in   string
		want runtime.Mount
	}{
		{"read-write", "host:container", runtime.Mount{HostPath: "host", ContainerPath: "container", ReadOnly: false}},
		{"read-only", "/host/mirror:/workspace/mirror:ro", runtime.Mount{HostPath: "/host/mirror", ContainerPath: "/workspace/mirror", ReadOnly: true}},
	}
	for _, tc := range okCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMount(tc.in)
			if err != nil {
				t.Fatalf("parseMount(%q) = unexpected error %v, want %+v", tc.in, err, tc.want)
			}
			if got != tc.want {
				t.Errorf("parseMount(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}

	errCases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"one field", "host"},
		{"empty container", "host:"},
		{"empty host", ":container"},
		{"bad mode", "host:container:rw"},
		{"four fields", "host:container:ro:extra"},
		{"comma in path", "/ho,st:/container"},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseMount(tc.in)
			if err == nil {
				t.Fatalf("parseMount(%q) = nil error, want a rejection", tc.in)
			}
			if !strings.Contains(err.Error(), "host:container[:ro]") {
				t.Errorf("parseMount(%q) error %q does not name the accepted shape host:container[:ro]", tc.in, err)
			}
		})
	}
}
