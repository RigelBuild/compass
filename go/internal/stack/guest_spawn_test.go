//go:build unix

package stack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testGuestArtifact = "ghcr.io/rigelbuild/compass-guest-image@sha256:" +
	"1111111111111111111111111111111111111111111111111111111111111111"

// stubGuestMaterializer replaces the fetch seam for the spawn-chain tests, so
// no stack test reaches a registry. It records every call and can inject a
// failure — the load-bearing case, since a fetch failure must stop the runner
// from launching at all.
type stubGuestMaterializer struct {
	rec   *recorder
	paths GuestPaths
	err   error
	refs  []string
}

// install swaps the package's materializeGuest seam for this stub and restores
// the real fetcher when the test ends (the readStartTime pattern).
func (m *stubGuestMaterializer) install(t *testing.T) {
	t.Helper()
	prev := materializeGuest
	materializeGuest = func(_ context.Context, ref, stateDir string) (GuestPaths, error) {
		m.refs = append(m.refs, ref)
		m.rec.add("materialize-guest")
		if m.err != nil {
			return GuestPaths{}, m.err
		}
		if m.paths != (GuestPaths{}) {
			return m.paths, nil
		}
		return guestPathsIn(filepath.Join(stateDir, guestImageDirName, "stub")), nil
	}
	t.Cleanup(func() { materializeGuest = prev })
}

// TestUpMaterializesArtifactBeforeRunner is the positive spawn-integration
// case: the artifact is materialised exactly once, the resolved paths reach the
// runner's --microvm-* flags, and it happens BEFORE the runner starts (a later
// fetch would leave a window where the runner boots against absent paths).
func TestUpMaterializesArtifactBeforeRunner(t *testing.T) {
	cfg, h := newHarness(t)
	cfg.RuntimeBackend = "microvm"
	cfg.GuestArtifact = testGuestArtifact
	want := guestPathsIn("/state/guest-image/abc")
	mat := &stubGuestMaterializer{rec: h.rec, paths: want}
	mat.install(t)

	if _, err := Up(context.Background(), cfg, h.deps); err != nil {
		t.Fatalf("Up() = %v, want nil", err)
	}

	if len(mat.refs) != 1 || mat.refs[0] != cfg.GuestArtifact {
		t.Fatalf("materialiser calls = %q, want exactly [%q]", mat.refs, cfg.GuestArtifact)
	}
	events := filterEvents(h.rec.snapshot())
	mi := slices.Index(events, "materialize-guest")
	ri := slices.Index(events, "start compass-runner")
	if mi < 0 || ri < 0 || mi > ri {
		t.Fatalf("event order %v: want materialize-guest before start compass-runner", events)
	}

	args := h.sup.lastArgs(t, ComponentRunner)
	for flag, wantValue := range map[string]string{
		"--backend":                "microvm",
		"--microvm-kernel":         want.Kernel,
		"--microvm-rootfs":         want.Rootfs,
		"--microvm-initrd":         want.Initrd,
		"--microvm-image-manifest": want.Manifest,
	} {
		got, ok := flagValue(args, flag)
		if !ok {
			t.Errorf("runner args %q carry no %s", args, flag)
			continue
		}
		if got != wantValue {
			t.Errorf("runner %s = %q, want %q", flag, got, wantValue)
		}
	}
}

// TestUpDoesNotStartRunnerWhenMaterializationFails is the fail-closed spawn
// invariant: a fetch or verification failure must prevent the runner launch
// outright. A runner started against unresolved paths would boot, fail its own
// preflight, bury the real cause, and be a live child the failed Up must drain.
func TestUpDoesNotStartRunnerWhenMaterializationFails(t *testing.T) {
	cfg, h := newHarness(t)
	cfg.RuntimeBackend = "microvm"
	cfg.GuestArtifact = testGuestArtifact
	mat := &stubGuestMaterializer{rec: h.rec, err: errors.New("blob digest mismatch")}
	mat.install(t)

	s, err := Up(context.Background(), cfg, h.deps)
	if err == nil {
		t.Fatal("Up() = nil, want the materialisation failure surfaced")
	}
	if s != nil {
		t.Fatal("Up() returned a Stack on failure; want nil (no half-started leak)")
	}
	if !strings.Contains(err.Error(), "blob digest mismatch") {
		t.Errorf("error = %v, want it to carry the materialisation cause", err)
	}
	if slices.Contains(filterEvents(h.rec.snapshot()), "start compass-runner") {
		t.Fatal("compass-runner started despite a failed materialisation")
	}
	// The children that did start are drained and the lock released, so a retry
	// after fixing the artifact can acquire.
	assertDrainedCleanly(t, h)
	assertLockFree(t, cfg.StateDir)
}

// TestUpGuestDirBypassDoesNotFetch: the air-gapped path must produce the same
// explicit paths with NO materialiser call at all — the promise that no pull is
// ever mandatory.
func TestUpGuestDirBypassDoesNotFetch(t *testing.T) {
	cfg, h := newHarness(t)
	cfg.RuntimeBackend = "microvm"
	cfg.GuestDir = seedGuestDir(t)
	mat := &stubGuestMaterializer{rec: h.rec}
	mat.install(t)

	if _, err := Up(context.Background(), cfg, h.deps); err != nil {
		t.Fatalf("Up() = %v, want nil", err)
	}
	if len(mat.refs) != 0 {
		t.Fatalf("guest dir bypass called the materialiser %d times, want 0", len(mat.refs))
	}

	args := h.sup.lastArgs(t, ComponentRunner)
	want := guestPathsIn(cfg.GuestDir)
	for flag, wantValue := range map[string]string{
		"--microvm-kernel":         want.Kernel,
		"--microvm-rootfs":         want.Rootfs,
		"--microvm-initrd":         want.Initrd,
		"--microvm-image-manifest": want.Manifest,
	} {
		if got, _ := flagValue(args, flag); got != wantValue {
			t.Errorf("runner %s = %q, want %q", flag, got, wantValue)
		}
	}
}

// TestUpGuestDirMissingAssetDoesNotStartRunner: the CLI deliberately does not
// check the guest dir's files, so startup owns that check — an incomplete
// directory must stop the launch here rather than reach the runner.
func TestUpGuestDirMissingAssetDoesNotStartRunner(t *testing.T) {
	cfg, h := newHarness(t)
	cfg.RuntimeBackend = "microvm"
	cfg.GuestDir = seedGuestDir(t)
	if err := os.Remove(filepath.Join(cfg.GuestDir, guestRootfsFile)); err != nil {
		t.Fatalf("remove rootfs: %v", err)
	}

	s, err := Up(context.Background(), cfg, h.deps)
	if err == nil {
		t.Fatal("Up() = nil, want the incomplete guest dir rejected")
	}
	if s != nil {
		t.Fatal("Up() returned a Stack on failure; want nil")
	}
	if slices.Contains(filterEvents(h.rec.snapshot()), "start compass-runner") {
		t.Fatal("compass-runner started against an incomplete guest dir")
	}
	assertDrainedCleanly(t, h)
	assertLockFree(t, cfg.StateDir)
}

// TestUpWithoutGuestConfigNeverFetches pins the preserved default: a microVM
// stack with neither knob set keeps the Runner image's baked assets live — no
// fetch, and no --microvm-* flags for the runner to prefer over them.
func TestUpWithoutGuestConfigNeverFetches(t *testing.T) {
	tests := []struct {
		name    string
		backend string
	}{
		{name: "microvm with neither knob", backend: "microvm"},
		{name: "non-microvm", backend: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, h := newHarness(t)
			cfg.RuntimeBackend = tt.backend
			mat := &stubGuestMaterializer{rec: h.rec}
			mat.install(t)

			if _, err := Up(context.Background(), cfg, h.deps); err != nil {
				t.Fatalf("Up() = %v, want nil", err)
			}
			if len(mat.refs) != 0 {
				t.Fatalf("materialiser called %d times with no guest config, want 0", len(mat.refs))
			}
			args := h.sup.lastArgs(t, ComponentRunner)
			for _, flag := range []string{"--microvm-kernel", "--microvm-rootfs", "--microvm-initrd", "--microvm-image-manifest"} {
				if _, ok := flagValue(args, flag); ok {
					t.Errorf("runner args %q carry %s with no guest config; the baked assets must stay live", args, flag)
				}
			}
		})
	}
}

// The microVM backend runs the agent from the guest rootfs, so there is no
// agent image to pull. Pulling one anyway is not a harmless no-op: on a host
// with no registry access for the agent image, EnsureImage fails and takes the
// whole Up down for an image nothing reads.
func TestUpSkipsAgentImagePullUnderMicroVM(t *testing.T) {
	t.Run("microvm skips the pull", func(t *testing.T) {
		cfg, h := newHarness(t)
		cfg.RuntimeBackend = "microvm"
		cfg.GuestDir = seedGuestDir(t)
		mat := &stubGuestMaterializer{rec: h.rec}
		mat.install(t)
		// Fails the pull the way a host with no registry access would: if the
		// skip regresses, Up returns this error instead of starting.
		h.image.err = errors.New("no such image in the local store")

		if _, err := Up(context.Background(), cfg, h.deps); err != nil {
			t.Fatalf("Up() = %v, want nil: the agent image must not be pulled under microVM", err)
		}
		if slices.Contains(filterEvents(h.rec.snapshot()), "ensure-image") {
			t.Error("Up pulled the agent image under microVM; the agent ships in the guest rootfs")
		}
	})

	// The guard is scoped to the one backend that cannot apply an image: every
	// container backend must still pull, and still fail when the pull fails.
	t.Run("container backends still pull", func(t *testing.T) {
		cfg, h := newHarness(t)
		h.image.err = errors.New("no such image in the local store")

		if _, err := Up(context.Background(), cfg, h.deps); err == nil {
			t.Fatal("Up() = nil error with a failing pull, want the ensure-agent-image failure")
		}
		if !slices.Contains(filterEvents(h.rec.snapshot()), "ensure-image") {
			t.Error("Up skipped the agent image pull on a container backend")
		}
	})
}
