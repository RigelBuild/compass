package compute

// This file is the fail-closed routing-policy shell (Global Constraint 3). Route
// is a pure function — separate from any backend, mirroring how the podman argv
// builders are split out for hermetic testing — that decides the ResourceClass
// an op runs at. Two invariants are load-bearing security properties, not
// conveniences:
//
//   - Fail closed. An unknown or unclassified op escalates to the HEAVY path
//     (ClassBurst), never the cheap one. The routing verdict must never default
//     to the most-privileged-but-cheapest boundary for an op the classifier did
//     not recognize; the most-isolated, most-provisioned path is the safe
//     default when in doubt.
//   - The agent hint is upgrade-only. An op may carry an agent-supplied hint,
//     but the hint may only RAISE the class (Inner -> Resized -> Burst); a hint
//     asking for a cheaper class than policy chose is refused. The agent can
//     never lower isolation or sizing below what Runner policy assigns.

// OpClass is the Runner classifier's verdict for an op — the input to the
// routing policy. Its zero value is OpUnknown, so an op that was never
// classified routes through Route to the fail-closed heavy path rather than
// silently taking the cheap one.
type OpClass int

const (
	// OpUnknown is an unclassified op. The zero value, and any OpClass the
	// policy does not recognize, routes fail-closed to ClassBurst.
	OpUnknown OpClass = iota
	// OpInnerLoop is a classified inner-loop op: it runs in place unresized
	// (ClassInner).
	OpInnerLoop
	// OpResize is a classified heavy op that resize-in-place satisfies
	// (ClassResized).
	OpResize
	// OpBurst is a classified heavy op that needs a transient environment
	// (ClassBurst).
	OpBurst
)

// Route resolves the ResourceClass an op runs at from the classifier's verdict
// and an optional agent hint. It fails closed — an unknown or unrecognized op
// escalates to ClassBurst — and applies the hint upgrade-only: the hint may
// raise the class above the policy floor but a hint below the floor is refused,
// so the returned class is never cheaper than policy chose.
func Route(op OpClass, hint ResourceClass) ResourceClass {
	base := baseClass(op)
	// Upgrade-only: a hint above the floor raises the class; a hint at or below
	// the floor leaves it untouched. max encodes both — a downgrade hint can
	// never win because base is never below it in that case.
	if hint > base {
		return hint
	}
	return base
}

// baseClass is the policy floor for an op's classification, before any agent
// hint. An unknown op — and any OpClass the policy does not recognize — fails
// closed to ClassBurst.
func baseClass(op OpClass) ResourceClass {
	switch op {
	case OpInnerLoop:
		return ClassInner
	case OpResize:
		return ClassResized
	case OpBurst:
		return ClassBurst
	default:
		// OpUnknown and any out-of-range value: fail closed to the heavy path.
		return ClassBurst
	}
}
