package compute

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/runtime"
)

// contractCase is one behavior every ComputeRuntime must satisfy. The same
// suite runs against a fake and against the real S1 in-place backend so the two
// stay in lockstep on the seam's observable contract.
type contractCase struct {
	name string
	run  func(t *testing.T, cr ComputeRuntime)
}

var contractCases = []contractCase{
	{
		// A ComputeRuntime.Exec returns the backend's output for a command that
		// exited zero, surfaced as a successful call (not an error).
		name: "exec returns output for a successful command",
		run: func(t *testing.T, cr ComputeRuntime) {
			t.Helper()
			out, err := cr.Exec(context.Background(), ComputeSpec{Command: []string{"true"}})
			if err != nil {
				t.Fatalf("Exec returned error: %v", err)
			}
			if !out.Success() {
				t.Fatalf("Exec output not success: exit=%d", out.ExitCode)
			}
		},
	},
	{
		// ExecStreaming is reserved: every S1 ComputeRuntime returns the honest
		// not-implemented sentinel and a nil handle, never a silent success.
		name: "exec streaming returns the not-implemented sentinel",
		run: func(t *testing.T, cr ComputeRuntime) {
			t.Helper()
			se, err := cr.ExecStreaming(context.Background(), ComputeSpec{Command: []string{"true"}})
			if !errors.Is(err, ErrExecStreamingNotImplemented) {
				t.Fatalf("ExecStreaming error = %v, want ErrExecStreamingNotImplemented", err)
			}
			if se != nil {
				t.Fatalf("ExecStreaming returned a non-nil handle alongside the sentinel")
			}
		},
	},
}

// fakeCompute is a hand-written ComputeRuntime standing in for a backend. It
// returns a canned success from Exec and the reserved sentinel from
// ExecStreaming, so the shared contract suite exercises the seam's behavior
// without any engine.
type fakeCompute struct{}

func (fakeCompute) Exec(context.Context, ComputeSpec) (runtime.ExecOutput, error) {
	return runtime.ExecOutput{ExitCode: 0}, nil
}

func (fakeCompute) ExecStreaming(context.Context, ComputeSpec) (*runtime.StreamingExec, error) {
	return nil, ErrExecStreamingNotImplemented
}

func TestFakeSatisfiesContract(t *testing.T) {
	for _, tc := range contractCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t, fakeCompute{})
		})
	}
}

func TestInPlaceSatisfiesContract(t *testing.T) {
	for _, tc := range contractCases {
		t.Run(tc.name, func(t *testing.T) {
			eng := &recordingEngine{output: runtime.ExecOutput{ExitCode: 0}}
			cr := NewInPlace(eng, runtime.ContainerID("sess-container"), runtime.EgressPolicy{})
			tc.run(t, cr)
		})
	}
}

// recordingEngine is a runtime.ContainerRuntime that records the id and ExecSpec
// its Exec receives, so the in-place backend's mapping (command, workdir, env)
// and its target container can be asserted without a real container. Every other
// method is an unused stub — the in-place backend only ever calls Exec.
type recordingEngine struct {
	gotID   runtime.ContainerID
	gotSpec runtime.ExecSpec
	gotCtx  context.Context
	execN   int
	output  runtime.ExecOutput
	execErr error
}

func (e *recordingEngine) Exec(ctx context.Context, id runtime.ContainerID, spec runtime.ExecSpec) (runtime.ExecOutput, error) {
	e.gotID = id
	e.gotSpec = spec
	e.gotCtx = ctx
	e.execN++
	return e.output, e.execErr
}

func (e *recordingEngine) Create(context.Context, runtime.ContainerSpec) (runtime.ContainerID, error) {
	return "", errors.New("recordingEngine: Create unused")
}
func (e *recordingEngine) Start(context.Context, runtime.ContainerID) error {
	return errors.New("recordingEngine: Start unused")
}
func (e *recordingEngine) ExecStreaming(context.Context, runtime.ContainerID, runtime.StreamingExecSpec) (*runtime.StreamingExec, error) {
	return nil, errors.New("recordingEngine: ExecStreaming unused")
}
func (e *recordingEngine) Stop(context.Context, runtime.ContainerID, time.Duration) error {
	return errors.New("recordingEngine: Stop unused")
}
func (e *recordingEngine) Remove(context.Context, runtime.ContainerID) error {
	return errors.New("recordingEngine: Remove unused")
}
func (e *recordingEngine) Exists(context.Context, string) (bool, error) {
	return false, errors.New("recordingEngine: Exists unused")
}
func (e *recordingEngine) MountLabel(context.Context, runtime.ContainerID) (string, error) {
	return "", errors.New("recordingEngine: MountLabel unused")
}

// Resize is a forward-declared stub: the container-runtime seam reserves a
// Resize verb the in-place backend never calls, so the fake carries it to stay
// a total implementation of the engine interface as that interface freezes the
// verb in.
func (e *recordingEngine) Resize(context.Context, runtime.ContainerID, runtime.ResourceLimits) error {
	return errors.New("recordingEngine: Resize unused")
}

// The in-place backend must delegate to the injected engine's Exec against the
// session's own container, mapping the ComputeSpec's command, dir, and env onto
// the ExecSpec. A regression that dropped the workdir, mangled the env, or
// targeted the wrong container would fail here.
func TestInPlaceExecMapsSpecAndDelegatesToSessionContainer(t *testing.T) {
	eng := &recordingEngine{output: runtime.ExecOutput{Stdout: "ok", ExitCode: 0}}
	cr := NewInPlace(eng, runtime.ContainerID("sess-container"), runtime.EgressPolicy{})

	spec := ComputeSpec{
		Command: []string{"go", "test", "./..."},
		Dir:     "/work/repo",
		Env:     map[string]string{"CGO_ENABLED": "0", "HOME": "/home/agent"},
	}
	out, err := cr.Exec(context.Background(), spec)
	if err != nil {
		t.Fatalf("Exec returned error: %v", err)
	}
	if out.Stdout != "ok" {
		t.Fatalf("Exec did not return the engine's output: got %q", out.Stdout)
	}
	if eng.execN != 1 {
		t.Fatalf("engine.Exec called %d times, want 1", eng.execN)
	}
	if eng.gotID != runtime.ContainerID("sess-container") {
		t.Fatalf("delegated to container %q, want session container", eng.gotID)
	}
	if !slices.Equal(eng.gotSpec.Command, spec.Command) {
		t.Fatalf("Command = %v, want %v", eng.gotSpec.Command, spec.Command)
	}
	if eng.gotSpec.Workdir == nil || *eng.gotSpec.Workdir != "/work/repo" {
		t.Fatalf("Workdir = %v, want /work/repo", eng.gotSpec.Workdir)
	}
	if !maps.Equal(eng.gotSpec.Env, spec.Env) {
		t.Fatalf("Env = %v, want %v", eng.gotSpec.Env, spec.Env)
	}
	// The in-place passthrough runs as the container's default user, so User is
	// left unset for the engine to resolve.
	if eng.gotSpec.User != nil {
		t.Fatalf("User = %v, want nil (container default user)", *eng.gotSpec.User)
	}
}

// An empty ComputeSpec.Dir must leave the ExecSpec's Workdir nil rather than
// pinning the command to the container root — the backend's default working
// directory is the intended behavior.
func TestInPlaceExecOmitsWorkdirWhenDirEmpty(t *testing.T) {
	eng := &recordingEngine{}
	cr := NewInPlace(eng, runtime.ContainerID("sess-container"), runtime.EgressPolicy{})

	if _, err := cr.Exec(context.Background(), ComputeSpec{Command: []string{"true"}}); err != nil {
		t.Fatalf("Exec returned error: %v", err)
	}
	if eng.gotSpec.Workdir != nil {
		t.Fatalf("Workdir = %v, want nil when Dir is empty", *eng.gotSpec.Workdir)
	}
}

// A positive ComputeSpec.Timeout must bound the delegated exec: the engine's
// ExecSpec carries no timeout field, so the backend applies the per-op limit as
// a context deadline on the ctx it passes the engine. A regression that dropped
// spec.Timeout would hand the engine a deadline-less ctx — an unbounded heavy op
// — which this asserts against; a zero Timeout must leave the ctx untouched.
func TestInPlaceExecAppliesTimeoutAsContextDeadline(t *testing.T) {
	t.Run("positive timeout sets a deadline", func(t *testing.T) {
		eng := &recordingEngine{}
		cr := NewInPlace(eng, runtime.ContainerID("sess-container"), runtime.EgressPolicy{})

		if _, err := cr.Exec(context.Background(), ComputeSpec{Command: []string{"go", "test"}, Timeout: 30 * time.Second}); err != nil {
			t.Fatalf("Exec returned error: %v", err)
		}
		deadline, ok := eng.gotCtx.Deadline()
		if !ok {
			t.Fatal("engine ctx has no deadline; spec.Timeout was dropped")
		}
		if remaining := time.Until(deadline); remaining <= 0 || remaining > 30*time.Second {
			t.Fatalf("deadline %s out; want (0, 30s] from spec.Timeout", remaining)
		}
	})
	t.Run("zero timeout leaves ctx untouched", func(t *testing.T) {
		eng := &recordingEngine{}
		cr := NewInPlace(eng, runtime.ContainerID("sess-container"), runtime.EgressPolicy{})

		if _, err := cr.Exec(context.Background(), ComputeSpec{Command: []string{"true"}}); err != nil {
			t.Fatalf("Exec returned error: %v", err)
		}
		if _, ok := eng.gotCtx.Deadline(); ok {
			t.Fatal("engine ctx has a deadline for a zero Timeout; want the inherited deadline-less ctx")
		}
	})
	t.Run("caller deadline shorter than timeout wins", func(t *testing.T) {
		eng := &recordingEngine{}
		cr := NewInPlace(eng, runtime.ContainerID("sess-container"), runtime.EgressPolicy{})

		// A caller ctx already bounded tighter than spec.Timeout: the effective
		// deadline must stay the caller's, never be pushed out to now+Timeout.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := cr.Exec(ctx, ComputeSpec{Command: []string{"go", "test"}, Timeout: 30 * time.Second}); err != nil {
			t.Fatalf("Exec returned error: %v", err)
		}
		deadline, ok := eng.gotCtx.Deadline()
		if !ok {
			t.Fatal("engine ctx has no deadline; caller deadline was dropped")
		}
		if remaining := time.Until(deadline); remaining <= 0 || remaining > 5*time.Second {
			t.Fatalf("deadline %s out; want (0, 5s] from the tighter caller ctx, not the 30s spec.Timeout", remaining)
		}
	})
}

// Guard the seam type identities: ComputeRuntime must be satisfied by both the
// fake and the real backend (a compile-time check that would break if the
// interface drifted from the implementations).
var (
	_ ComputeRuntime           = fakeCompute{}
	_ ComputeRuntime           = (*InPlace)(nil)
	_ runtime.ContainerRuntime = (*recordingEngine)(nil)
)
