package compute

import (
	"context"
	"errors"

	"github.com/RigelBuild/compass/go/internal/runtime"
)

// ErrExecStreamingNotImplemented is the honest sentinel the S1 in-place backend
// returns from the reserved ExecStreaming method: the streaming variant is
// declared in the ComputeRuntime interface now but has no backend until its
// consumer lands. Callers detect it with errors.Is rather than reading a nil
// result as success.
var ErrExecStreamingNotImplemented = errors.New("compute: ExecStreaming not implemented")

// InPlace is the S1 ComputeRuntime backend: an in-environment local-exec
// passthrough. The op runs in the session's own environment at its current size,
// by delegating to the injected container-runtime engine against the session's
// container id — the "fused passthrough" the design lands S1 with. It is the
// backend that serves ClassInner; the resize-in-place and burst backends for the
// heavier classes land later.
//
// InPlace is session-scoped: it holds the session's engine, container handle,
// and egress policy so a backend has the run-in-place, burst, and egress-arming
// context without threading it through every Exec call. At S1 only the container
// handle is exercised (the passthrough runs in place, arming nothing new); the
// engine and egress are held for the heavier backends behind the same seam.
type InPlace struct {
	engine    runtime.ContainerRuntime
	container runtime.ContainerID
	egress    runtime.EgressPolicy
}

// NewInPlace builds the S1 in-place backend bound to a session's container-
// runtime engine, its container handle, and its egress policy.
func NewInPlace(engine runtime.ContainerRuntime, container runtime.ContainerID, egress runtime.EgressPolicy) *InPlace {
	return &InPlace{engine: engine, container: container, egress: egress}
}

// Exec runs spec in the session's own container by mapping it to a
// runtime.ExecSpec and delegating to the engine. Command and Env map straight
// across; a non-empty Dir becomes the exec's Workdir. The op runs as the
// container's default user (the baked agent uid), so User is left nil. A
// non-zero exit is carried in the returned runtime.ExecOutput, not folded into
// an error — matching the engine's Exec contract.
//
// A positive spec.Timeout bounds the op: the engine's ExecSpec carries no
// timeout of its own (the engine applies its own per-command cap around the
// subprocess), so the caller's per-op limit is applied here as a context
// deadline. The effective deadline is the tighter of the two — the caller can
// shorten the op's wall-clock bound but not extend it past the engine's cap, an
// accepted S1 limit until a backend that runs longer heavy ops lands. A zero
// spec.Timeout leaves the inherited ctx deadline untouched.
func (r *InPlace) Exec(ctx context.Context, spec ComputeSpec) (runtime.ExecOutput, error) {
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	exec := runtime.ExecSpec{
		Command: spec.Command,
		Env:     spec.Env,
	}
	if spec.Dir != "" {
		dir := spec.Dir
		exec.Workdir = &dir
	}
	return r.engine.Exec(ctx, r.container, exec)
}

// ExecStreaming returns the not-implemented sentinel: the streaming variant is
// reserved in the interface but has no S1 backend. Honest failure over a silent
// no-op — a caller must not read a nil handle as a live stream.
func (r *InPlace) ExecStreaming(ctx context.Context, spec ComputeSpec) (*runtime.StreamingExec, error) {
	return nil, ErrExecStreamingNotImplemented
}
