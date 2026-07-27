package runtime

// Image build front door: the container-ready probe, the early NoDevenv reject,
// the container-attribute selection, and the repo-path digest that keeps two
// repos' images from clobbering. The digest and reject paths are hermetic; the
// full Build shells out to devenv+podman (exercised in the podman-tagged
// lifecycle), so the container-name reflection is pinned at its only hermetic
// surface — the attribute the builder targets.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsContainerReadyRequiresDevenvNix(t *testing.T) {
	dir := t.TempDir()

	// A bare directory is not container-ready.
	if IsContainerReady(dir) {
		t.Fatalf("IsContainerReady(%q) = true before devenv.nix exists, want false", dir)
	}

	// A devenv.nix makes it ready.
	if err := os.WriteFile(filepath.Join(dir, "devenv.nix"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("writing devenv.nix: %v", err)
	}
	if !IsContainerReady(dir) {
		t.Fatalf("IsContainerReady(%q) = false after devenv.nix exists, want true", dir)
	}
}

func TestBuildRejectsNonContainerReadyRepo(t *testing.T) {
	dir := t.TempDir() // no devenv.nix

	_, err := NewImageBuilder().Build(t.Context(), dir)

	var noDevenv *NoDevenvError
	if !errors.As(err, &noDevenv) {
		t.Fatalf("Build(non-container-ready repo) error = %v, want *NoDevenvError", err)
	}
	if noDevenv.RepoPath != dir {
		t.Fatalf("NoDevenvError.RepoPath = %q, want %q", noDevenv.RepoPath, dir)
	}
}

func TestImageBuilderForContainerTargetsTheNamedAttribute(t *testing.T) {
	// The container-name selection's only observable effect is which devenv
	// container attribute the builder targets (and, downstream, the ImageRef tag
	// prefix once a real Build runs — see the podman-tagged lifecycle). Asserted
	// white-box here because Build shells out to devenv, so the ImageRef isn't
	// hermetically reachable. This catches a constructor that ignores its
	// argument and silently builds the default `agent` attribute instead.
	if got := ImageBuilderForContainer("compass-agent").devenvContainer; got != "compass-agent" {
		t.Fatalf("ImageBuilderForContainer(%q).devenvContainer = %q, want %q", "compass-agent", got, "compass-agent")
	}
}

func TestRepoDigestDiffersByPathAndIsStable(t *testing.T) {
	a := repoDigest("/repos/alpha")
	b := repoDigest("/repos/beta")

	// Distinct repos get distinct tags — no cross-repo image clobbering.
	if a == b {
		t.Fatalf("repoDigest(/repos/alpha) == repoDigest(/repos/beta) == %q, want distinct digests", a)
	}
	// Stable for the same path, so a rebuild reuses the same ref.
	if again := repoDigest("/repos/alpha"); a != again {
		t.Fatalf("repoDigest(/repos/alpha) not stable: %q then %q", a, again)
	}
}

// TestImageRunTimeoutBoundsWedgedBuild pins the reliability contract on the
// image-build seam: ImageBuilder.run must not block the caller forever on a
// wedged toolchain command. The Rust ImageBuilder has no per-command timeout
// (image.rs:59-67,160-163); this Go fold ADDS one — a `timeout` field defaulting
// to 120s, overridable via WithTimeout — so a hung `devenv container build`
// (stalled Nix eval, wedged builder) surfaces as an error rather than hanging
// the launch task.
//
// This test REQUIRES two seams ross must add to image.go (see the report to
// Main): ImageBuilder.WithTimeout(time.Duration) *ImageBuilder and
// ImageBuilder.WithProgram(string) *ImageBuilder (exposing the existing
// devenvProgram field). It will not compile until those land — that is the
// red→green contract, not a defect in the test.
//
// Behavioral (not structural): it drives Build → run with a program stub that
// sleeps past the timeout and asserts the OBSERVABLE result — Build returns a
// *TimeoutError within a bounded wall-clock — not that a field was assigned.
// Build gates on IsContainerReady, so the temp repo carries a devenv.nix; the
// stub stands in for `devenv` (the first command Build runs) and ignores its
// argv, so the wedge is hit on the very first run() call.
func TestImageRunTimeoutBoundsWedgedBuild(t *testing.T) {
	dir := t.TempDir()
	// devenv.nix makes the repo container-ready so Build proceeds past the early
	// NoDevenv reject into run(), where the timeout is exercised.
	if err := os.WriteFile(filepath.Join(dir, "devenv.nix"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("writing devenv.nix: %v", err)
	}

	// A stub standing in for `devenv`: hang well past the timeout, ignoring the
	// `container build agent` argv. The 30s sleep exceeds the timeout + safety
	// deadline, so a missing timeout is caught rather than tolerated.
	stub := "#!/bin/sh\nexec sleep 30\n"
	prog := filepath.Join(dir, "devenv-stub.sh")
	if err := os.WriteFile(prog, []byte(stub), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}

	builder := NewImageBuilder().WithProgram(prog).WithTimeout(200 * time.Millisecond)
	ctx := t.Context()

	done := make(chan error, 1)
	go func() {
		_, err := builder.Build(ctx, dir)
		done <- err
	}()

	const safety = 10 * time.Second
	select {
	case <-time.After(safety):
		t.Fatalf("Build did not return within %s — ImageBuilder.run has no per-command timeout; a wedged devenv hangs the caller", safety)
	case err := <-done:
		var timeout *TimeoutError
		if !errors.As(err, &timeout) {
			t.Fatalf("Build error = %v (%T), want *TimeoutError", err, err)
		}
	}
}
