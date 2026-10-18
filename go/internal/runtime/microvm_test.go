package runtime

// Backend-selection tests pin defaults, microVM opt-in, and unknown-backend rejection.
// The package can run them on any platform without subprocesses.

import (
	"strings"
	"testing"
)

// SelectBackend defaults to podman: an empty or explicit "podman" backend must
// return the container path (a *PodmanCLI), so an unset backend never moves an
// operator onto microVM. Surrounding whitespace (stray from env/CI wiring) is tolerated.
func TestSelectBackendDefaultsToPodman(t *testing.T) {
	for _, backend := range []string{"", "podman", "  podman  ", "\tpodman\n"} {
		rt, err := SelectBackend(BackendConfig{Backend: backend})
		if err != nil {
			t.Fatalf("SelectBackend(%q) err = %v, want nil", backend, err)
		}
		if _, ok := rt.(*PodmanCLI); !ok {
			t.Fatalf("SelectBackend(%q) = %T, want *PodmanCLI", backend, rt)
		}
	}
}

// SelectBackend("microvm") returns the microVM runtime.
func TestSelectBackendMicroVM(t *testing.T) {
	rt, err := SelectBackend(BackendConfig{Backend: "microvm"})
	if err != nil {
		t.Fatalf("SelectBackend(microvm) err = %v, want nil", err)
	}
	if _, ok := rt.(*MicroVMRuntime); !ok {
		t.Fatalf("SelectBackend(microvm) = %T, want *MicroVMRuntime", rt)
	}
}

// An unknown backend is a loud refusal: a nil runtime plus an error naming the
// bad value, never a silent fallback to a default backend.
func TestSelectBackendUnknown(t *testing.T) {
	rt, err := SelectBackend(BackendConfig{Backend: "bogus"})
	if rt != nil {
		t.Fatalf("SelectBackend(bogus) runtime = %v, want nil", rt)
	}
	if err == nil {
		t.Fatal("SelectBackend(bogus) err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("SelectBackend(bogus) err = %q, want it to name the bad value", err)
	}
}

func TestValidBackend(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "default", input: "", want: ""},
		{name: "podman", input: "podman", want: "podman"},
		{name: "trimmed microvm", input: " microvm ", want: "microvm"},
		{name: "apple-container", input: "apple-container", want: "apple-container"},
		{name: "host", input: "host", want: "host"},
		{name: "unknown", input: "micorvm", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidBackend(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidBackend(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("ValidBackend(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
