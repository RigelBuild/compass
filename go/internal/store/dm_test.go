package store

import "testing"

// DMChannelName must be order-independent and injective. The `:` separator is
// load-bearing: a hyphen would derive one name for the distinct pairs
// {a, b--c} and {a--b, c}, cross-adding members into a same-owner private DM.
func TestDMChannelName(t *testing.T) {
	cases := []struct {
		name   string
		h1, h2 string
		want   string
	}{
		{name: "unsorted input swaps to sorted name", h1: "bob", h2: "alice", want: "dm:alice:bob"},
		{name: "already-sorted input keeps order", h1: "alice", h2: "bob", want: "dm:alice:bob"},
		{name: "double-hyphen handle, first pair", h1: "a", h2: "b--c", want: "dm:a:b--c"},
		{name: "double-hyphen handle, second pair", h1: "a--b", h2: "c", want: "dm:a--b:c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DMChannelName(tc.h1, tc.h2); got != tc.want {
				t.Fatalf("DMChannelName(%q, %q) = %q, want %q", tc.h1, tc.h2, got, tc.want)
			}
			if reversed := DMChannelName(tc.h2, tc.h1); reversed != tc.want {
				t.Fatalf("DMChannelName(%q, %q) = %q, want %q (order-independent)", tc.h2, tc.h1, reversed, tc.want)
			}
		})
	}

	// The two `--` rows above pin distinct names, which is the injectivity
	// property; a hyphen delimiter would collide them onto `dm--a--b--c`.
}
