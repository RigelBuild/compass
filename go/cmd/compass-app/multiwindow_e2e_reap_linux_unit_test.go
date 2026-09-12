//go:build linux && gtk4

package main

import "testing"

// The chain walk decides which pids get signalled, so a regression here would
// aim SIGKILL at something we do not own. Every non-descendant case must answer
// false: under-signalling leaks a process, over-signalling kills a stranger.
func TestDescendsFrom(t *testing.T) {
	const self = 100

	tests := []struct {
		name  string
		pid   int
		ppids map[int]int
		want  bool
	}{
		{"direct child", 200, map[int]int{200: self, self: 1}, true},
		{"deep grandchild", 400, map[int]int{400: 300, 300: 200, 200: self, self: 1}, true},
		{"reparented to init", 200, map[int]int{200: 1, self: 1}, false},
		{"sibling under a subreaper", 200, map[int]int{200: 50, 50: 1, self: 1}, false},
		{"broken link", 400, map[int]int{400: 300, self: 1}, false},
		{"cycle terminates", 200, map[int]int{200: 300, 300: 200, self: 1}, false},
		{"self is not its own descendant", self, map[int]int{self: 1}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := descendsFrom(tc.pid, self, tc.ppids); got != tc.want {
				t.Fatalf("descendsFrom(%d, %d) = %v, want %v", tc.pid, self, got, tc.want)
			}
		})
	}
}

// A comm holding spaces or parens would break a naive field split, and a
// misparsed ppid is a wrong signal target.
func TestParseStat(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantPPID int
		wantComm string
		wantOK   bool
	}{
		{"plain", "42 (bash) S 7 42 42 0 -1 4194304", 7, "bash", true},
		{"comm with spaces", "42 (Web Content) S 7 42", 7, "Web Content", true},
		{"comm with parens", "42 (odd (name)) S 7 42", 7, "odd (name)", true},
		{"comm with a space and a paren", "42 (a b) c) R 9 42", 9, "a b) c", true},
		{"truncated after comm", "42 (bash)", 0, "", false},
		{"no parens", "42 bash S 7", 0, "", false},
		{"non-numeric ppid", "42 (bash) S x 42", 0, "", false},
		{"empty", "", 0, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ppid, comm, ok := parseStat(tc.line)
			if ok != tc.wantOK || ppid != tc.wantPPID || comm != tc.wantComm {
				t.Fatalf("parseStat(%q) = (%d, %q, %v), want (%d, %q, %v)",
					tc.line, ppid, comm, ok, tc.wantPPID, tc.wantComm, tc.wantOK)
			}
		})
	}
}
