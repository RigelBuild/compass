//go:build unix && gtk3

package main

// T6.2 sidecar PATH threading: prependExecDirToPath prepends the resolved
// compass-stack's bundle dir to the child PATH so staged sidecars win
// exec.LookPath, without mutating the caller's env slice.

import (
	"os"
	"slices"
	"testing"
)

func TestPrependExecDirToPath(t *testing.T) {
	sep := string(os.PathListSeparator)

	t.Run("prepends to an existing PATH", func(t *testing.T) {
		in := []string{"PATH=/usr/bin" + sep + "/bin", "HOME=/h"}
		got := prependExecDirToPath(in, "/opt/x")
		want := []string{"PATH=/opt/x" + sep + "/usr/bin" + sep + "/bin", "HOME=/h"}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("appends PATH when none present", func(t *testing.T) {
		in := []string{"HOME=/h"}
		got := prependExecDirToPath(in, "/opt/x")
		want := []string{"HOME=/h", "PATH=/opt/x"}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("empty execDir returns input unchanged", func(t *testing.T) {
		in := []string{"PATH=/usr/bin", "HOME=/h"}
		got := prependExecDirToPath(in, "")
		want := []string{"PATH=/usr/bin", "HOME=/h"}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("does not mutate the input slice", func(t *testing.T) {
		in := []string{"PATH=/usr/bin" + sep + "/bin", "HOME=/h"}
		_ = prependExecDirToPath(in, "/opt/x")
		if in[0] != "PATH=/usr/bin"+sep+"/bin" {
			t.Fatalf("input mutated: in[0] = %q", in[0])
		}
	})
}
