//go:build unix

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/runtime"
)

// Compile-time regression guard: the real backend types must satisfy their
// probe interfaces, so a future change that drops a probe method fails to
// compile here rather than silently falling through to verifyBackendPreflight's
// fail-closed default at runtime. Routing of the default *PodmanCLI to the
// podman branch also depends on it NOT satisfying microVMPreflighter; that
// precedence is exercised by TestVerifyBackendPreflight, not asserted here.
var (
	_ microVMPreflighter = (*runtime.MicroVMRuntime)(nil)
	_ podmanPreflighter  = (*runtime.PodmanCLI)(nil)
	_ canaryBooter       = (*runtime.MicroVMRuntime)(nil)
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

// microVMOnlyEngine exposes the microVM static probe AND the canary probe — a
// real microVM engine satisfies both, and the gate now runs the canary after the
// static check passes, so a fake missing BootCanary would trip the fail-closed
// canary assertion rather than exercise the static-probe dispatch.
type microVMOnlyEngine struct {
	runtime.ContainerRuntime
	called       *bool
	canaryCalled *bool
	err          error
	report       runtime.CanaryReport
	canaryErr    error
}

func (e microVMOnlyEngine) VerifyMicroVMSupport(context.Context) error {
	*e.called = true
	return e.err
}

func (e microVMOnlyEngine) BootCanary(context.Context) (runtime.CanaryReport, error) {
	if e.canaryCalled != nil {
		*e.canaryCalled = true
	}
	return e.report, e.canaryErr
}

// microVMNoCanaryEngine exposes ONLY the static microVM probe, not the canary —
// a microVM backend that cannot boot-canary. The gate must fail closed on it,
// naming the type, never silently skipping the canary.
type microVMNoCanaryEngine struct {
	runtime.ContainerRuntime
	called *bool
	err    error
}

func (e microVMNoCanaryEngine) VerifyMicroVMSupport(context.Context) error {
	*e.called = true
	return e.err
}

// neitherEngine exposes no probe: only the embedded (nil) ContainerRuntime.
type neitherEngine struct {
	runtime.ContainerRuntime
}

// bothProbesEngine exposes BOTH probes. No real engine does today, but it locks
// the microVM-first precedence of verifyBackendPreflight's type switch: the
// microVM branch must win and the podman branch must not run.
type bothProbesEngine struct {
	runtime.ContainerRuntime
	microVMCalled *bool
	podmanCalled  *bool
	err           error
}

func (e bothProbesEngine) VerifyMicroVMSupport(context.Context) error {
	*e.microVMCalled = true
	return e.err
}

func (e bothProbesEngine) VerifyUsernsRemapSupport(context.Context) error {
	*e.podmanCalled = true
	return e.err
}

func (e bothProbesEngine) BootCanary(context.Context) (runtime.CanaryReport, error) {
	return runtime.CanaryReport{}, nil
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

	t.Run("microvm static probe then canary dispatched", func(t *testing.T) {
		called, canaryCalled := false, false
		err := verifyBackendPreflight(context.Background(),
			microVMOnlyEngine{called: &called, canaryCalled: &canaryCalled})
		if err != nil {
			t.Fatalf("verifyBackendPreflight = %v, want nil", err)
		}
		if !called {
			t.Error("microVM static probe was not called")
		}
		if !canaryCalled {
			t.Error("boot canary was not called after the static probe passed")
		}
	})

	t.Run("static probe error skips the canary", func(t *testing.T) {
		called, canaryCalled := false, false
		err := verifyBackendPreflight(context.Background(),
			microVMOnlyEngine{called: &called, canaryCalled: &canaryCalled, err: sentinel})
		if !errors.Is(err, sentinel) {
			t.Fatalf("verifyBackendPreflight = %v, want the sentinel error", err)
		}
		if canaryCalled {
			t.Error("boot canary ran after the static probe failed")
		}
	})

	t.Run("canary error returned verbatim", func(t *testing.T) {
		called := false
		err := verifyBackendPreflight(context.Background(),
			microVMOnlyEngine{called: &called, canaryErr: sentinel})
		if !errors.Is(err, sentinel) {
			t.Errorf("verifyBackendPreflight = %v, want the canary sentinel error", err)
		}
	})

	t.Run("microVM without canary is fail-closed naming the type", func(t *testing.T) {
		called := false
		err := verifyBackendPreflight(context.Background(), microVMNoCanaryEngine{called: &called})
		if err == nil {
			t.Fatal("verifyBackendPreflight = nil, want a fail-closed canary refusal")
		}
		if !called {
			t.Error("microVM static probe was not called")
		}
		if !strings.Contains(err.Error(), "microVMNoCanaryEngine") {
			t.Errorf("error %q does not name the engine type", err)
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

	t.Run("microVM probe wins when an engine satisfies both", func(t *testing.T) {
		microVMCalled, podmanCalled := false, false
		err := verifyBackendPreflight(context.Background(), bothProbesEngine{
			microVMCalled: &microVMCalled,
			podmanCalled:  &podmanCalled,
		})
		if err != nil {
			t.Fatalf("verifyBackendPreflight = %v, want nil", err)
		}
		if !microVMCalled {
			t.Error("microVM probe was not called")
		}
		if podmanCalled {
			t.Error("podman probe was called; microVM-first precedence broken")
		}
	})
}
