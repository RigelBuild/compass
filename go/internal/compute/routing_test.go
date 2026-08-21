package compute

import "testing"

// Route is the fail-closed routing-policy shell (Global Constraint 3). These
// tests pin the two load-bearing security invariants across every (op, hint)
// combination: an unknown op escalates to the heavy path, and an agent hint may
// only raise the class, never lower it. A regression that defaulted an unknown
// op to the cheap path, or honored a downgrade hint, would silently lower a
// session's isolation — so each row is RED-provable against that bug.
func TestRouteFailClosedAndUpgradeOnly(t *testing.T) {
	cases := []struct {
		name string
		op   OpClass
		hint ResourceClass
		want ResourceClass
	}{
		// Fail-closed default: an unclassified op escalates to the heavy path,
		// never the cheap one. This is the security invariant a wrong default
		// would silently break.
		{"unknown op fails closed to burst", OpUnknown, ClassInner, ClassBurst},
		{"unknown op with no upgrade still burst", OpUnknown, ClassBurst, ClassBurst},
		{"out-of-range op fails closed to burst", OpClass(99), ClassInner, ClassBurst},

		// Classified ops route to their policy floor when the hint asks for no
		// more than the floor.
		{"inner op floor is inner", OpInnerLoop, ClassInner, ClassInner},
		{"resize op floor is resized", OpResize, ClassInner, ClassResized},
		{"burst op floor is burst", OpBurst, ClassInner, ClassBurst},

		// Upgrade-only: a hint above the floor raises the class.
		{"hint raises inner to resized", OpInnerLoop, ClassResized, ClassResized},
		{"hint raises inner to burst", OpInnerLoop, ClassBurst, ClassBurst},
		{"hint raises resized to burst", OpResize, ClassBurst, ClassBurst},

		// Refused: a hint below the floor is ignored, and a hint outside the
		// recognized class set (> ClassBurst) is likewise refused to the floor —
		// so the class never drops beneath what policy chose and Route never
		// emits an unrecognized class.
		{"downgrade hint on resize refused", OpResize, ClassInner, ClassResized},
		{"downgrade hint on burst refused", OpBurst, ClassInner, ClassBurst},
		{"downgrade hint on burst to resized refused", OpBurst, ClassResized, ClassBurst},
		{"downgrade hint on unknown refused", OpUnknown, ClassInner, ClassBurst},
		{"out-of-range hint refused to inner floor", OpInnerLoop, ResourceClass(99), ClassInner},
		{"out-of-range hint refused to resize floor", OpResize, ResourceClass(99), ClassResized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Route(tc.op, tc.hint); got != tc.want {
				t.Fatalf("Route(%v, %v) = %v, want %v", tc.op, tc.hint, got, tc.want)
			}
		})
	}
}

// ResourceClass's zero value must be the cheapest boundary (ClassInner), which
// is exactly why an unclassified op must never be allowed to default to it — the
// routing policy fails such an op closed to ClassBurst instead. This pins the
// ordering the upgrade-only comparison in Route relies on.
func TestResourceClassOrdering(t *testing.T) {
	if ClassInner != 0 {
		t.Fatalf("ClassInner = %d, want 0 (the zero value must be the cheapest class)", ClassInner)
	}
	if !(ClassInner < ClassResized && ClassResized < ClassBurst) {
		t.Fatalf("resource classes must order cheap->heavy: inner=%d resized=%d burst=%d",
			ClassInner, ClassResized, ClassBurst)
	}
}
