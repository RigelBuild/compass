//go:build unix && gtk4

package main

// Compass multi-window M2 name-suffix allocator gate. firstFreeName is the pure
// seam behind nextWindowName: it returns the first name not reported taken by
// the exists predicate, starting at the base then base-2, base-3, … No test
// constructs a real *application.App or opens a window (no display in CI).

import "testing"

func TestFirstFreeName(t *testing.T) {
	tests := []struct {
		name  string
		taken []string
		want  string
	}{
		{"empty set returns base", nil, "bridge"},
		{"base taken returns base-2", []string{"bridge"}, "bridge-2"},
		{"base and two suffixes taken returns base-4", []string{"bridge", "bridge-2", "bridge-3"}, "bridge-4"},
		{"only suffix taken returns base", []string{"bridge-2"}, "bridge"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			taken := make(map[string]bool, len(tt.taken))
			for _, n := range tt.taken {
				taken[n] = true
			}
			exists := func(n string) bool { return taken[n] }
			if got := firstFreeName(defaultWindowName, exists); got != tt.want {
				t.Fatalf("firstFreeName = %q, want %q", got, tt.want)
			}
		})
	}
}
