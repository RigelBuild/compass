package compute

// The fail-closed routing-policy shell (Global Constraint 3). Route is a pure
// function deciding the ResourceClass an op runs at. Two load-bearing invariants:
// (1) Fail closed — an unknown op escalates to the HEAVY path (ClassBurst), never
// the cheap one. (2) The agent hint is upgrade-only — it may only RAISE the class.

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
// raise the class above the policy floor, but a hint below the floor or outside
// the recognized class set is refused. So the returned class is always one of
// the recognized classes and is never cheaper than policy chose.
func Route(op OpClass, hint ResourceClass) ResourceClass {
	base := baseClass(op)
	// Upgrade-only, within the recognized range: a hint above the floor raises the
	// class, but only when it names a real class (<= ClassBurst). An out-of-range or
	// garbage hint collapses to the policy floor, so Route's verdict is always a
	// recognized class and a malformed hint can neither lower isolation nor escape it.
	if hint > base && hint <= ClassBurst {
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
