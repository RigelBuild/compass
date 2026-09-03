//go:build unix

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/runtime"
)

// Compile-time regression guard on the reorder: the real backend types must
// satisfy the unexported probe interfaces, so runtime.SelectBackend's default
// *PodmanCLI provably routes to the podman branch of verifyBackendPreflight and
// the microVM backend to the microVM branch. If a future change drops a probe
// method, this fails to compile rather than silently falling through to the
// fail-closed default at runtime.
var (
	_ microVMPreflighter = (*runtime.MicroVMRuntime)(nil)
	_ podmanPreflighter  = (*runtime.PodmanCLI)(nil)
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

// podmanOnlyEngine exposes only the podman probe; its embedded nil
// ContainerRuntime satisfies the param type but is never called.
type podmanOnlyEngine struct {
	runtime.ContainerRuntime
	called *bool
	err    error
}

func (e podmanOnlyEngine) VerifyUsernsRemapSupport(context.Context) error {
	*e.called = true
	return e.err
}

// microVMOnlyEngine exposes only the microVM probe.
type microVMOnlyEngine struct {
	runtime.ContainerRuntime
	called *bool
	err    error
}

func (e microVMOnlyEngine) VerifyMicroVMSupport(context.Context) error {
	*e.called = true
	return e.err
}

// neitherEngine exposes no probe: only the embedded (nil) ContainerRuntime.
type neitherEngine struct {
	runtime.ContainerRuntime
}

// verifyBackendPreflight dispatches on the selected engine's concrete type
// (RIG-2496): microVM first, then podman, first match wins; the matched probe
// runs and its error is returned verbatim; an engine exposing neither probe is a
// fail-closed startup error naming the type — never a silent skip.
func TestVerifyBackendPreflight(t *testing.T) {
	sentinel := errors.New("preflight refused")

	t.Run("podman probe dispatched", func(t *testing.T) {
		called := false
		err := verifyBackendPreflight(context.Background(), podmanOnlyEngine{called: &called})
		if err != nil {
			t.Fatalf("verifyBackendPreflight = %v, want nil", err)
		}
		if !called {
			t.Error("podman probe was not called")
		}
	})

	t.Run("microvm probe dispatched", func(t *testing.T) {
		called := false
		err := verifyBackendPreflight(context.Background(), microVMOnlyEngine{called: &called})
		if err != nil {
			t.Fatalf("verifyBackendPreflight = %v, want nil", err)
		}
		if !called {
			t.Error("microVM probe was not called")
		}
	})

	t.Run("neither probe is fail-closed naming the type", func(t *testing.T) {
		err := verifyBackendPreflight(context.Background(), neitherEngine{})
		if err == nil {
			t.Fatal("verifyBackendPreflight = nil, want a fail-closed refusal")
		}
		if !strings.Contains(err.Error(), "neitherEngine") {
			t.Errorf("error %q does not name the engine type", err)
		}
	})

	t.Run("probe error returned verbatim", func(t *testing.T) {
		called := false
		err := verifyBackendPreflight(context.Background(), podmanOnlyEngine{called: &called, err: sentinel})
		if !errors.Is(err, sentinel) {
			t.Errorf("verifyBackendPreflight = %v, want the sentinel error", err)
		}
	})

	t.Run("default backend is a podman probe type", func(t *testing.T) {
		engine, err := runtime.SelectBackend(runtime.BackendConfig{})
		if err != nil {
			t.Fatalf("SelectBackend = %v, want the default podman engine", err)
		}
		// Do NOT invoke the real probe (it shells out to `podman version`);
		// only assert it routes to the podman branch by type.
		if _, ok := engine.(podmanPreflighter); !ok {
			t.Errorf("default backend %T does not satisfy podmanPreflighter", engine)
		}
	})
}
