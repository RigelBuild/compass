// Package compute is the elastic-compute seam: how a heavy operation acquires
// the compute it needs to run. A session's inner-loop tool-calls run in place in
// the session's own environment, but a heavy op (a full build, a test suite) may
// need more CPU/memory than the session is sized for — so the Runner routes it
// to a resize-in-place or a burst-to-transient-environment backend rather than
// starving it in place
// (docs/designs/infra/runtime/compass-elastic-session-runtime/design.md, the
// ComputeRuntime seam under S1 and the elastic backends under C3).
//
// The layering mirrors the container-runtime seam in internal/runtime:
//   - compute.go — the ComputeRuntime interface plus the value types that cross
//     it (ComputeSpec, ResourceClass). Every consumer depends on the interface,
//     so a resize-in-place or a burst backend can replace the S1 one without
//     touching a caller.
//   - inplace.go — the S1 backend: an in-environment local-exec passthrough that
//     runs the op in the session's own environment at its current size, by
//     delegating to the injected runtime.ContainerRuntime against the session
//     container. The genuinely trivial fused configuration the design lands S1
//     end to end with.
//   - routing.go — the fail-closed routing-policy shell (Global Constraint 3): a
//     pure function that classifies an op to a ResourceClass. Even though only
//     the in-place backend exists at S1, the routing shell, its fail-closed
//     default, and the hint-cannot-downgrade rule are load-bearing security
//     invariants and are implemented and tested now.
//
// Reserved-not-implemented surface: ExecStreaming is declared in the interface
// now (mirroring runtime.ContainerRuntime.ExecStreaming) so a later streaming
// consumer lands no interface change, but the S1 backend returns an honest
// not-implemented sentinel rather than a silent no-op.
//
// This package does not import internal/vfs (the two S1 seams are code-
// independent; a later provision-wiring change composes them). It imports
// internal/runtime for the shared exec value types and the container-engine
// handle the in-place backend delegates to.
package compute

import (
	"context"
	"time"

	"github.com/RigelBuild/compass/go/internal/runtime"
)

// ResourceClass is the compute boundary Runner policy assigns to an op. It is a
// policy enum — which backend runs the op and how much compute it gets — and is
// deliberately distinct from runtime.ResourceLimits, the concrete cgroup-limit
// struct a resize applies. The routing policy assigns it; an agent may only hint
// upward (see routing.go). The zero value is ClassInner, the cheapest boundary,
// so a ResourceClass must never be defaulted for an unclassified op — the
// routing policy fails such an op closed to ClassBurst rather than letting the
// zero value silently pick the cheap path.
type ResourceClass int

const (
	// ClassInner is the inner-loop boundary: run the op in place in the
	// session's own environment at its current size, unresized. The S1 in-place
	// backend serves this class.
	ClassInner ResourceClass = iota
	// ClassResized is the resize-in-place boundary: raise the session
	// environment's CPU/memory limits for the op, then restore them. No backend
	// implements this at S1 (it lands with C3).
	ClassResized
	// ClassBurst is the heavy boundary: burst the op to a transient environment
	// with its own compute. The fail-closed default for an unclassified op — the
	// most-isolated, most-provisioned path. No backend implements this at S1 (it
	// lands with C3).
	ClassBurst
)

// ComputeSpec is one heavy operation to run: the command and its environment,
// the resource class Runner policy assigned it, and a wall-clock timeout. The
// resource class is assigned by policy (routing.go), never authored by the
// agent; an agent hint may only raise it.
type ComputeSpec struct {
	// Command is the argv to run. Command[0] is the program.
	Command []string
	// Dir is the working directory the command runs in. Empty runs in the
	// backend's default working directory.
	Dir string
	// Env is the environment the command runs with.
	Env map[string]string
	// Resources is the compute boundary Runner policy assigned this op. Assigned
	// by Route, never by the agent.
	Resources ResourceClass
	// Timeout bounds the op's wall-clock. Zero leaves the backend's default cap
	// in force.
	Timeout time.Duration
}

// ComputeRuntime is the elastic-compute seam: it runs a heavy op on whatever
// backend the op's ResourceClass selects. An interface so the Runner can hold a
// ComputeRuntime and tests can substitute a fake. A ComputeRuntime is
// session-scoped — a backend is constructed with the session's container handle,
// its container-runtime engine, and its egress policy — so it has the run-in-
// place, burst, and egress-arming context a backend needs without threading it
// through every Exec call.
type ComputeRuntime interface {
	// Exec runs spec to completion, returning its buffered output. A non-zero
	// exit is a successful call returning a failed command
	// (runtime.ExecOutput.ExitCode), not an error; only a spawn failure,
	// timeout, or backend error is an error. Reuses runtime.ExecOutput — fully
	// buffered stdout/stderr, an accepted limit for whole-suite output until the
	// streaming variant lands.
	Exec(ctx context.Context, spec ComputeSpec) (runtime.ExecOutput, error)

	// ExecStreaming is reserved: it will run a long-lived streaming op returning
	// its live stdio pipes plus a kill/wait handle, mirroring
	// runtime.ContainerRuntime.ExecStreaming. It is declared in the interface now
	// so a later streaming consumer lands no interface change; the S1 backend
	// returns ErrExecStreamingNotImplemented rather than silently succeeding.
	ExecStreaming(ctx context.Context, spec ComputeSpec) (*runtime.StreamingExec, error)
}
