package comms

import "testing"

// dmChannelName must be order-independent and injective. The `:` separator is
// load-bearing: a hyphen would derive one name for the distinct pairs
// {a, b--c} and {a--b, c}, cross-adding members into a same-owner private DM.
func TestDmChannelName(t *testing.T) {
	cases := []struct {
		name   string
		h1, h2 string
		want   string
	}{
		// lo > hi: exercises the swap branch (bob sorts after alice).
		{name: "unsorted input swaps to sorted name", h1: "bob", h2: "alice", want: "dm:alice:bob"},
		// lo <= hi: exercises the no-swap branch.
		{name: "already-sorted input keeps order", h1: "alice", h2: "bob", want: "dm:alice:bob"},
		{name: "double-hyphen handle, first pair", h1: "a", h2: "b--c", want: "dm:a:b--c"},
		{name: "double-hyphen handle, second pair", h1: "a--b", h2: "c", want: "dm:a--b:c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dmChannelName(tc.h1, tc.h2); got != tc.want {
				t.Fatalf("dmChannelName(%q, %q) = %q, want %q", tc.h1, tc.h2, got, tc.want)
			}
			// Order independence: the reversed pair must map to the same name.
			if rev := dmChannelName(tc.h2, tc.h1); rev != tc.want {
				t.Fatalf("dmChannelName(%q, %q) = %q, want %q (order-independent)", tc.h2, tc.h1, rev, tc.want)
			}
		})
	}

	// The two `--` rows above pin distinct names, which is the injectivity
	// property; a hyphen delimiter would collide them onto `dm--a--b--c`.
}
