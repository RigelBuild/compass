//go:build microvm && unix

package runtime

// The KVM-gated virtio-fs ISOLATION suite (record §Plan V6 Test cycle). This is
// the slice that PROVES inter-tenant isolation rather than exercising a happy
// path, so every assertion here drives a real MicroVMRuntime through
// Create→Start and then execs inside the live guest to ATTEMPT an escape,
// asserting confinement from both sides of the boundary (what the guest can
// name, and what actually landed on the host).
//
// Every test calls microvmtest.Require(t) FIRST, mirroring
// microvm_lifecycle_microvm_test.go: on a KVM-less box it SKIPS, and under
// COMPASS_REQUIRE_MICROVM=1 that skip becomes a hard failure — so a green run
// proves the suite really booted guests.
//
// The three legs, and what each actually proves:
//
//  1. Path traversal (TestMicroVMVolumeTraversalConfined) — the guest attempts
//     to read and write outside its volume via `..`, an absolute host path, and
//     a symlink planted inside the volume pointing at a host path outside it.
//     Confinement is asserted on BOTH sides: the guest cannot read the outside
//     canary's content, and the host-side outside tree is byte-for-byte
//     unchanged afterwards. The host-side half is the load-bearing one — a guest
//     read failing could be a missing file, but an unchanged host tree after a
//     write attempt is confinement.
//  2. Cross-session unreachability (TestMicroVMCrossSessionVolumeUnreachable) —
//     two sessions boot with distinct volumes; guest A cannot read B's secret by
//     any path, and nothing A writes appears in B's host-side volume.
//  3. Host-ownership parity (TestMicroVMHostOwnershipParity) — a file the guest
//     agent creates on the shared volume must land with the SAME host-side
//     (uid,gid) the podman `--userns=keep-id:uid=N,gid=N` path produces: the
//     INVOKING host user, not the in-guest agent id (podman.go createArgs).
//     This is the test that decided whether launch.go needed virtiofsd uid/gid
//     translation (record §(d)) — see parityTargetUID/GID below.
//
// The quota-enforcement-in-guest leg is gated separately and skips honestly; see
// TestMicroVMVolumeQuotaEnforcedInGuest.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/agentuid"
	"github.com/RigelBuild/compass/go/internal/microvmtest"
)

// outsideCanaryBody is the content planted in a host file OUTSIDE the session
// volume. A guest exec that ever prints this string has escaped the volume
// subtree, so it is a distinctive sentinel rather than a generic word.
const outsideCanaryBody = "HOST-ONLY-CANARY-8f3ac1d0-must-never-be-readable-from-a-guest"

// The bounds on the quota-enforcement leg's guest write. It must cross the
// project byte bound to observe EDQUOT, but the cost of crossing it is the
// operator's quota size, which the test does not control — and the 120s
// per-exec cap (execDefaultTimeout) is hard, so an unbounded fill fails as a
// transport timeout rather than with the verdict the leg exists to assert.
//
// quotaFillMarginMiB is how far past the remaining headroom to write: enough to
// be unambiguously over the bound even if usage shifts between the pre-read and
// the write, small enough to be free.
//
// quotaFillCeilingMiB is the largest fill this leg will attempt. 1 GiB over
// virtio-fs is comfortably inside the cap on any box that can run this suite at
// all, while a 10 GiB production-sized quota is not — so a volume with more
// headroom than this SKIPS with the reason named rather than timing out. The
// operator's quota volume for this leg is expected to be purpose-sized (a few
// hundred MiB), not a production project.
const (
	quotaFillMarginMiB  = 64
	quotaFillCeilingMiB = 1024
)

// isolationSession boots one session against a fresh volume dir and returns the
// runtime, its id, and the host-side volume path. Teardown is registered so a
// failed assertion still tears the VM down. The volume is a t.TempDir() child so
// it is removed with the test; the SHORT runroot comes from e2eConfig (the
// AF_UNIX sun_path budget, microvm_lifecycle_microvm_test.go).
func isolationSession(t *testing.T, env microvmtest.Env, name string) (*MicroVMRuntime, ContainerID, string) {
	t.Helper()
	m := NewMicroVMRuntime(e2eConfig(t, env))
	volume := filepath.Join(t.TempDir(), "volume")
	if err := os.MkdirAll(volume, 0o700); err != nil {
		t.Fatalf("creating session volume %s: %v", volume, err)
	}
	id, err := m.Create(t.Context(), ContainerSpec{
		Name:   name,
		UID:    agentuid.AgentUID,
		Mounts: []Mount{{HostPath: volume, ContainerPath: workspaceMountPath}},
	})
	if err != nil {
		t.Fatalf("Create(%s): %v", name, err)
	}
	t.Cleanup(func() {
		if err := m.Remove(t.Context(), id); err != nil {
			t.Errorf("Remove(%s): %v", name, err)
		}
	})
	if err := m.Start(t.Context(), id); err != nil {
		t.Fatalf("Start(%s): %v", name, err)
	}
	return m, id, volume
}

// guestSh runs a shell snippet in the guest as the agent uid and returns its
// combined stdout+stderr and exit code. A transport/refusal error is fatal; a
// NON-ZERO EXIT IS NOT — a denied escape attempt is expected to exit non-zero,
// and that is the outcome under test (mirrors rowExecExitCodes' posture).
func guestSh(t *testing.T, m *MicroVMRuntime, id ContainerID, script string) (string, int) {
	t.Helper()
	out, err := m.Exec(t.Context(), id,
		NewExecSpec("sh", "-s").WithStdin(script).AsUser(strconv.Itoa(int(agentuid.AgentUID))))
	if err != nil {
		t.Fatalf("guest exec failed at the transport/refusal layer (not the escape itself): %v", err)
	}
	return out.Stdout + out.Stderr, out.ExitCode
}

// crossSessionAttempt is one way tenant A could try to reach tenant B's volume,
// paired with what its OUTPUT must not contain. The secret body is the
// discriminator for a `cat`; a command that prints names rather than content
// (`ls`, `grep -r` on paths) needs its own forbidden strings, or the assertion
// is vacuously true and the row proves nothing.
type crossSessionAttempt struct {
	script string
	forbid []string
}

// sweepBatchSize is how many paths one awk invocation is handed. The scan's
// dominant cost is process creation, against the HARD 120s per-exec cap
// (execDefaultTimeout, microvm_lifecycle.go). One-spawn-per-file over roots
// including "/" ran the cross-tenant row at ~72s — ~60% of the cap, i.e. a flake
// waiting for a loaded box. Batching collapses that fork cost to per-batch,
// since awk takes many FILENAMEs per run and reports which file each match came
// from itself.
//
// Measured in-guest on this box over an identical 425-file tree, needle in the
// last file: 644ms at batch size 1 versus 47ms at 200 — a 13.7x reduction, i.e.
// batching removes essentially all of the ~1.5ms/file spawn overhead.
//
// 200 is bounded by the guest's ARG_MAX rather than by taste: paths average well
// under 128 bytes, so a 200-path argv stays far inside the limit while cutting
// spawns by two orders of magnitude.
const sweepBatchSize = 200

// sweepScript builds a recursive content search the guest can actually run. The
// guest image ships bash 5.3 and awk but NO grep and NO find, so a `grep -r`
// row exits 127 without searching anything — a vacuous pass that looks like
// confinement. bash's globstar walks the trees and awk does the matching, and
// the exit status mirrors grep's: 0 when the needle was found, 1 when it was
// not, so a caller can still assert the non-zero exit a confined command owes.
//
// Matching files are printed as `<path>:<line>`, so BOTH discriminators are
// live: the secret body appears in the output if any file's content was read,
// and the path appears if a file under another tenant's volume was reachable at
// all.
//
// BATCHED, one awk per sweepBatchSize files rather than one per file: the
// per-file variant spent nearly all its time forking (see sweepBatchSize) and
// sat at ~60% of the 120s exec cap. awk is handed many FILENAMEs at once and
// reports the matching one itself, so the output contract is unchanged.
//
// Two things stop the sweep from finding ITS OWN needle, which would be a false
// escape report rather than a real one:
//
//   - The needle travels in an EXPORTED ENV VAR, never in argv. Passed as
//     `awk -v`, it would land in the searcher's own /proc/self/cmdline, so the
//     sweep would match the string it is looking for in its own command line.
//   - /proc, /sys and /dev are skipped. They are synthetic kernel interfaces
//     that cannot hold another tenant's volume, so excluding them removes the
//     self-match surface (the environ/cmdline of the running searcher) without
//     narrowing what the row actually probes.
func sweepScript(needle, roots string) string {
	// The awk program: scan every FILENAME handed to this invocation, print
	// `<path>:<line>` per match, and exit non-zero when the batch had none — so
	// the caller's `found` accumulator keeps grep's semantics across batches.
	const awkProg = `index($0, ENVIRON["SWEEP_NEEDLE"]) { print FILENAME ":" $0; hit=1 } END { exit !hit }`
	return "export SWEEP_NEEDLE='" + needle + "'; " +
		"shopt -s globstar nullglob dotglob; found=1; batch=(); " +
		// scan() runs one awk over the accumulated batch and clears it. Guarded
		// on a non-empty batch so a trailing flush with nothing pending does not
		// invoke awk on zero files (which would read stdin and hang).
		"scan() { ((${#batch[@]})) || return 0; " +
		"if awk '" + awkProg + "' \"${batch[@]}\" 2>/dev/null; then found=0; fi; batch=(); }; " +
		"for root in " + roots + "; do " +
		"for f in \"$root\"/**/*; do " +
		// Collapse repeated slashes before matching: a "/" root globs to
		// "//proc/self/environ", which a /proc/* pattern does NOT match — the
		// sweep would then read its own environ and report finding the needle
		// it was given, a false escape.
		"n=$f; while [[ $n == //* ]]; do n=${n#/}; done; " +
		"case $n in /proc/*|/sys/*|/dev/*) continue;; esac; " +
		"[[ -f $f && -r $f ]] || continue; " +
		"batch+=(\"$f\"); " +
		"((${#batch[@]} >= " + strconv.Itoa(sweepBatchSize) + ")) && scan; " +
		"done; done 2>/dev/null; scan; exit $found"
}

// TestMicroVMSweepScriptFindsItsNeedle is the non-vacuity control for
// sweepScript itself: pointed at a tree that DOES contain the needle, it must
// find it and exit 0. Without this the cross-tenant sweep row could pass
// because the search is broken rather than because the volume is unreachable —
// exactly the failure mode that made the original `grep -r` row worthless.
func TestMicroVMSweepScriptFindsItsNeedle(t *testing.T) {
	env := microvmtest.Require(t)
	m, id, _ := isolationSession(t, env, "iso-sweep-control")

	const needle = "SWEEP-CONTROL-CANARY-2d7f4a91"
	if out, code := guestSh(t, m, id, "mkdir -p /workspace/deep/nested && printf '%s' '"+needle+"' > /workspace/deep/nested/planted.txt"); code != 0 {
		t.Fatalf("planting the sweep control canary: exit %d, %q", code, out)
	}
	out, code := guestSh(t, m, id, sweepScript(needle, "/workspace"))
	if code != 0 {
		t.Fatalf("sweepScript did not find a needle planted in its own search root (exit %d, %q); "+
			"the cross-tenant sweep row would pass vacuously", code, truncate(out))
	}
	if !strings.Contains(out, needle) {
		t.Fatalf("sweepScript exited 0 but its output %q does not carry the needle; the content check would be vacuous", truncate(out))
	}
	if !strings.Contains(out, "planted.txt") {
		t.Errorf("sweepScript output %q does not name the matching path; the path check would be vacuous", truncate(out))
	}
	t.Logf("sweep control: found the planted canary -> exit %d, %q", code, strings.TrimSpace(truncate(out)))
}

// TestMicroVMSweepScriptFindsANeedleAcrossBatches is the control for the
// BATCHING specifically, which the single-file control above cannot reach: with
// only a handful of files the sweep never fills a batch, so the mid-loop scan
// and the accumulator that carries a hit across batches are both dead code in
// that run — and the cross-tenant row now completes in ~26ms over the targeted
// roots, so it does not exercise them either.
//
// Planting well over sweepBatchSize files and putting the needle ONLY in the
// last one forces two things the batched form must get right:
//
//  1. the mid-loop `scan` flushes full batches instead of accumulating an argv
//     past ARG_MAX;
//  2. the trailing flush runs, and `found` survives the batch that matched —
//     a batched sweep whose exit status came from the LAST awk alone would
//     report "not found" whenever the needle sat in any earlier batch, so the
//     early-needle case below pins the other side of the same accumulator.
func TestMicroVMSweepScriptFindsANeedleAcrossBatches(t *testing.T) {
	env := microvmtest.Require(t)
	m, id, _ := isolationSession(t, env, "iso-sweep-batch")

	// Comfortably more than two full batches, so at least two mid-loop flushes
	// happen before the trailing one.
	fileCount := sweepBatchSize*2 + 25
	plant := "mkdir -p /workspace/many && for i in $(seq 1 " + strconv.Itoa(fileCount) + "); do " +
		"printf 'filler line %s\\n' \"$i\" > /workspace/many/f$i.txt; done && ls /workspace/many | wc -l"
	out, code := guestSh(t, m, id, plant)
	if code != 0 {
		t.Fatalf("planting %d filler files: exit %d, %q", fileCount, code, truncate(out))
	}
	if got := strings.TrimSpace(out); got != strconv.Itoa(fileCount) {
		t.Fatalf("planted %q files, want %d; the batch boundary would not be crossed", got, fileCount)
	}

	for _, tt := range []struct{ name, file, needle string }{
		// Last file: only the TRAILING flush can find it.
		{"in the final batch", "/workspace/many/f" + strconv.Itoa(fileCount) + ".txt", "SWEEP-BATCH-LAST-6c1e8f30"},
		// First file: found by a MID-LOOP flush, so `found` must survive every
		// later batch that matched nothing.
		{"in the first batch", "/workspace/many/f1.txt", "SWEEP-BATCH-FIRST-91ad47b2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if out, code := guestSh(t, m, id,
				"printf '%s\\n' '"+tt.needle+"' >> "+tt.file); code != 0 {
				t.Fatalf("planting the needle in %s: exit %d, %q", tt.file, code, truncate(out))
			}
			start := time.Now()
			out, code := guestSh(t, m, id, sweepScript(tt.needle, "/workspace"))
			elapsed := time.Since(start)
			if code != 0 {
				t.Fatalf("the batched sweep did not find a needle planted %s of %d files (exit %d, %q); "+
					"the cross-tenant sweep row would pass vacuously", tt.name, fileCount, code, truncate(out))
			}
			if !strings.Contains(out, tt.needle) {
				t.Fatalf("the batched sweep exited 0 but its output %q does not carry the needle; "+
					"the content check would be vacuous", truncate(out))
			}
			if !strings.Contains(out, filepath.Base(tt.file)) {
				t.Errorf("the batched sweep output %q does not name the matching file %s; awk must report the "+
					"FILENAME each match came from, which is what makes a batched scan as diagnostic as a per-file one",
					truncate(out), tt.file)
			}
			t.Logf("batched sweep over %d files (batch size %d) found the needle %s in %v: %q",
				fileCount, sweepBatchSize, tt.name, elapsed.Round(time.Millisecond), strings.TrimSpace(truncate(out)))
		})
	}
}

// TestMicroVMVolumeTraversalConfined is the path-traversal leg: the guest tries
// to escape /workspace three ways and is confined every time, proven from both
// sides of the boundary.
func TestMicroVMVolumeTraversalConfined(t *testing.T) {
	env := microvmtest.Require(t)
	m, id, volume := isolationSession(t, env, "iso-traversal")

	// The host tree OUTSIDE the volume: a sibling of the volume dir, so a
	// `/workspace/..` that actually escaped the share would land right in it.
	outside := filepath.Join(filepath.Dir(volume), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("creating outside dir: %v", err)
	}
	canary := filepath.Join(outside, "canary.txt")
	if err := os.WriteFile(canary, []byte(outsideCanaryBody), 0o600); err != nil {
		t.Fatalf("planting host canary: %v", err)
	}

	// A symlink INSIDE the volume pointing at the outside host path. This is the
	// sharpest probe: virtio-fs passes the link through verbatim, so if the
	// guest could resolve it against the host's namespace the escape would
	// succeed. It cannot — the target is resolved inside the guest's own mount
	// namespace, where that path does not exist.
	if err := os.Symlink(canary, filepath.Join(volume, "escape-link")); err != nil {
		t.Fatalf("planting escape symlink: %v", err)
	}
	// A symlink to the outside DIRECTORY too, so a traversal through a link
	// (rather than a direct read of one) is covered.
	if err := os.Symlink(outside, filepath.Join(volume, "escape-dir")); err != nil {
		t.Fatalf("planting escape dir symlink: %v", err)
	}

	// Read attempts. Each prints nothing on success-of-confinement; the
	// assertion is that the canary body never appears in ANY of them.
	reads := map[string]string{
		"dot-dot traversal":       "cat /workspace/../canary.txt /workspace/../outside/canary.txt",
		"absolute host path":      "cat " + canary,
		"symlink to host file":    "cat /workspace/escape-link",
		"symlink to host dir":     "cat /workspace/escape-dir/canary.txt",
		"deep dot-dot to fs root": "cat /workspace/../../../../../.." + canary,
	}
	for name, script := range reads {
		t.Run("read: "+name, func(t *testing.T) {
			out, code := guestSh(t, m, id, script)
			if strings.Contains(out, outsideCanaryBody) {
				t.Fatalf("guest READ a host file outside its volume via %s — VOLUME ESCAPE.\noutput: %q", name, out)
			}
			if code == 0 {
				t.Errorf("escape attempt %q exited 0 (output %q); a confined read must fail", name, out)
			}
			t.Logf("confined: %s -> exit %d, %q", name, code, strings.TrimSpace(out))
		})
	}

	// Write attempts. The guest's own root filesystem is writable (an overlay),
	// so a `/workspace/../pwned` may well succeed INSIDE the guest — that proves
	// nothing either way. The real question is whether any byte landed on the
	// HOST outside the volume, which the snapshot comparison below answers.
	before := snapshotTree(t, outside)
	writes := []string{
		"echo pwned > /workspace/../pwned.txt",
		"echo pwned > /workspace/../outside/pwned.txt",
		"echo pwned > " + filepath.Join(outside, "pwned-abs.txt"),
		"echo pwned > /workspace/escape-dir/pwned-link.txt",
		"echo overwritten > /workspace/escape-link",
		"rm -f " + canary,
		"rm -rf " + outside,
	}
	for _, script := range writes {
		out, code := guestSh(t, m, id, script)
		t.Logf("write attempt %q -> exit %d, %q", script, code, strings.TrimSpace(out))
	}
	after := snapshotTree(t, outside)
	if before != after {
		t.Fatalf("the host tree OUTSIDE the session volume changed after guest write attempts — VOLUME ESCAPE.\nbefore: %s\nafter:  %s", before, after)
	}
	// And the canary's content specifically: an in-place overwrite through the
	// symlink would keep the tree shape identical while corrupting the file.
	body, err := os.ReadFile(canary)
	if err != nil {
		t.Fatalf("reading the host canary after the guest write attempts: %v", err)
	}
	if string(body) != outsideCanaryBody {
		t.Fatalf("the host canary was REWRITTEN by the guest (now %q) — VOLUME ESCAPE", string(body))
	}
	t.Logf("host tree outside the volume unchanged after %d write attempts: %s", len(writes), after)
}

// TestMicroVMCrossSessionVolumeUnreachable is the multi-tenant leg: two live
// sessions, and guest A cannot reach guest B's volume by any path, nor write
// into it. This is the assertion that makes "another tenant's volume is not
// merely unreadable but unnameable" (record §(d)) a tested property.
func TestMicroVMCrossSessionVolumeUnreachable(t *testing.T) {
	env := microvmtest.Require(t)
	const tenantBSecret = "TENANT-B-SECRET-4b91e7c2-must-never-be-readable-from-tenant-A"

	mA, idA, volumeA := isolationSession(t, env, "iso-tenant-a")
	mB, idB, volumeB := isolationSession(t, env, "iso-tenant-b")

	// B's secret, planted host-side inside B's volume, and also written by B's
	// OWN guest — so the test covers both a host-authored and a guest-authored
	// file (the latter is what a real tenant's workspace actually contains).
	if err := os.WriteFile(filepath.Join(volumeB, "host-secret.txt"), []byte(tenantBSecret), 0o600); err != nil {
		t.Fatalf("planting tenant B's host secret: %v", err)
	}
	if out, code := guestSh(t, mB, idB, "printf '%s' '"+tenantBSecret+"' > /workspace/guest-secret.txt"); code != 0 {
		t.Fatalf("tenant B could not write its own workspace file: exit %d, %q", code, out)
	}
	// A's own volume is populated and readable by A. Without this the
	// cross-tenant negatives below could pass because A's share is broken
	// rather than because B's is unreachable.
	if err := os.WriteFile(filepath.Join(volumeA, "a-own.txt"), []byte("tenant-a-own"), 0o600); err != nil {
		t.Fatalf("planting tenant A's own file: %v", err)
	}
	if out, code := guestSh(t, mA, idA, "cat /workspace/a-own.txt"); code != 0 || !strings.Contains(out, "tenant-a-own") {
		t.Fatalf("tenant A cannot read its OWN volume (exit %d, %q); the cross-tenant negatives would be vacuous", code, out)
	}
	// Sanity: B can read its own secret. Without this, the cross-tenant
	// negative below could pass vacuously (e.g. if the share were broken for
	// everyone).
	if out, code := guestSh(t, mB, idB, "cat /workspace/host-secret.txt /workspace/guest-secret.txt"); code != 0 || !strings.Contains(out, tenantBSecret) {
		t.Fatalf("tenant B cannot read its OWN volume (exit %d, %q); the cross-tenant negative would be vacuous", code, out)
	}

	// Every way A could try to name B's volume. Each row's success condition
	// matches WHAT ITS COMMAND EMITS, which the secret-body check alone does
	// not: `ls` prints names and never file content, so a Contains(secret) on
	// an `ls` is unconditionally true and proves nothing whether B's volume is
	// reachable or not. Every row also asserts the NON-ZERO EXIT the traversal
	// leg asserts — a confined command must fail, not merely print nothing.
	attempts := map[string]crossSessionAttempt{
		"B's absolute host volume path": {
			script: "cat " + filepath.Join(volumeB, "host-secret.txt") + " " + filepath.Join(volumeB, "guest-secret.txt"),
		},
		"B's volume dir listing": {
			// An `ls` that SUCCEEDED and listed B's secrets would pass a
			// content check; the discriminator here is the FILENAMES plus the
			// exit code.
			script: "ls -la " + volumeB,
			forbid: []string{"host-secret.txt", "guest-secret.txt"},
		},
		"traversal toward B": {
			script: "cat /workspace/../volume/host-secret.txt; cat /workspace/../../*/volume/*secret*",
		},
		"a symlink A plants to B": {
			script: "ln -sf " + volumeB + " /workspace/b-link && cat /workspace/b-link/host-secret.txt",
		},
		"a content sweep of every tree A can name": {
			// NOT `grep -r`: the guest image ships no grep (and no find), so
			// that row exited 127 without ever searching — vacuous twice over,
			// once for printing paths instead of content and once for never
			// running. This is the same sweep in what the guest DOES have
			// (bash globstar + awk), and sweepScript keeps grep's exit
			// semantics: non-zero when nothing matched.
			//
			// "/" is deliberately NOT a root. The discriminating question is
			// whether B's volume path is REACHABLE from A, not whether the
			// read-only nix store holds the needle — and sweeping "/" pulled
			// the guest's entire ~21.8k-file rootfs through the scan, which
			// (even batched) buys nothing this targeted list plus B's own
			// volume parent does not already answer. The parent of volume B is
			// included explicitly so the one tree that COULD hold the secret is
			// definitely walked; a sweep that skipped it would be the vacuous
			// pass this row exists to avoid.
			script: sweepScript(tenantBSecret,
				"/tmp /mnt /media /run /var /home /workspace "+filepath.Dir(volumeB)),
			forbid: []string{volumeB},
		},
	}
	for name, attempt := range attempts {
		t.Run("A cannot reach "+name, func(t *testing.T) {
			// Timed: the sweep row is the expensive one, and its cost is only
			// bounded relative to the HARD 120s per-exec cap (execDefaultTimeout)
			// — a row creeping back toward that cap is a flake on a loaded box,
			// so the margin is reported rather than left to be rediscovered.
			start := time.Now()
			out, code := guestSh(t, mA, idA, attempt.script)
			elapsed := time.Since(start)
			if strings.Contains(out, tenantBSecret) {
				t.Fatalf("tenant A READ tenant B's secret via %s — CROSS-TENANT ESCAPE.\noutput: %q", name, out)
			}
			for _, forbidden := range attempt.forbid {
				if strings.Contains(out, forbidden) {
					t.Fatalf("tenant A's %s NAMED %q — tenant B's volume is reachable from A.\noutput: %q",
						name, forbidden, truncate(out))
				}
			}
			if code == 0 {
				t.Errorf("cross-tenant attempt %q exited 0 (output %q); a confined command must fail",
					name, truncate(out))
			}
			// Half the cap is the flake line: past it, a slower box turns this
			// confinement assertion into a transport TimeoutError, which guestSh
			// treats as fatal — a failure that says nothing about isolation.
			if elapsed > execDefaultTimeout/2 {
				t.Errorf("attempt %q took %v, over half the %v per-exec cap; it is a flake on a loaded box — "+
					"narrow its roots or increase the scan batch size", name, elapsed, execDefaultTimeout)
			}
			t.Logf("unreachable: %s -> exit %d in %v (cap %v), %q",
				name, code, elapsed.Round(time.Millisecond), execDefaultTimeout, strings.TrimSpace(truncate(out)))
		})
	}

	// A's writes must not land in B's volume. Snapshot B's host-side tree,
	// let A try, and compare.
	before := snapshotTree(t, volumeB)
	for _, script := range []string{
		"echo from-a > " + filepath.Join(volumeB, "pwned.txt"),
		"echo from-a > /workspace/b-link/pwned-link.txt",
		"rm -f " + filepath.Join(volumeB, "host-secret.txt"),
		"rm -rf " + volumeB,
	} {
		out, code := guestSh(t, mA, idA, script)
		t.Logf("A write-into-B attempt %q -> exit %d, %q", script, code, strings.TrimSpace(out))
	}
	if after := snapshotTree(t, volumeB); before != after {
		t.Fatalf("tenant B's host-side volume changed after tenant A's write attempts — CROSS-TENANT ESCAPE.\nbefore: %s\nafter:  %s", before, after)
	}
	// B must still be able to read its own secret: a "confinement" that worked
	// by breaking B's share would otherwise pass the comparison above.
	if out, code := guestSh(t, mB, idB, "cat /workspace/host-secret.txt"); code != 0 || !strings.Contains(out, tenantBSecret) {
		t.Fatalf("tenant B's own volume is broken after A's attempts (exit %d, %q)", code, out)
	}
	t.Logf("tenant B's volume intact and unreachable from tenant A: %s", before)
}

// TestMicroVMHostOwnershipParity is the ownership leg (record §(d)): a file the
// guest agent creates on the shared volume must land with the same host-side
// (uid,gid) the podman backend's `--userns=keep-id:uid=N,gid=N` produces — the
// INVOKING HOST user's ids, so files stay identical between backends and the
// invoking user still owns its own workspace tree.
//
// The gid axis is the discriminating one on a uid-1000 dev box: the in-guest
// agent runs as uid==gid==1000 (guestd linuxCredential), while the invoking host
// user's gid is typically NOT 1000 (here: 100/users). So an untranslated
// virtiofsd stamps gid 1000 where podman's keep-id would stamp the host gid —
// which is exactly what this test detects, and why launch.go carries virtiofsd's
// uid/gid translation.
func TestMicroVMHostOwnershipParity(t *testing.T) {
	env := microvmtest.Require(t)
	m, id, volume := isolationSession(t, env, "iso-parity")

	// The podman keep-id target: the invoking host user's own ids
	// (podman.go createArgs `--userns=keep-id:uid=%d,gid=%d` maps the invoking
	// host user onto the baked agent id, so the host-side owner is the invoker).
	wantUID, wantGID := os.Getuid(), os.Getgid()

	if out, code := guestSh(t, m, id, "umask 022; echo guest-authored > /workspace/from-guest.txt && mkdir -p /workspace/from-guest-dir && id -u && id -g"); code != 0 {
		t.Fatalf("guest could not write its own workspace: exit %d, %q", code, out)
	} else {
		t.Logf("in-guest identity for the write: %q", strings.TrimSpace(out))
	}

	for _, entry := range []string{"from-guest.txt", "from-guest-dir"} {
		path := filepath.Join(volume, entry)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("guest-authored %s is absent host-side: %v", entry, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("stat %s: unexpected Sys type %T", entry, info.Sys())
		}
		gotUID, gotGID := int(stat.Uid), int(stat.Gid)
		t.Logf("host-side ownership of guest-authored %s: uid=%d gid=%d (podman keep-id target: uid=%d gid=%d)",
			entry, gotUID, gotGID, wantUID, wantGID)
		if gotUID != wantUID || gotGID != wantGID {
			t.Errorf("host-ownership PARITY BROKEN for %s: guest-authored file is %d:%d, "+
				"but the podman --userns=keep-id path yields %d:%d — virtiofsd's uid/gid mapping "+
				"(launch.go --uid-map/--gid-map) must map the in-guest agent id to the invoking host user",
				entry, gotUID, gotGID, wantUID, wantGID)
		}
	}

	// The other direction: a file the HOST creates must be owned by the agent
	// in-guest, or the agent cannot write its own workspace tree — the same
	// property keep-id gives the container path.
	hostAuthored := filepath.Join(volume, "from-host.txt")
	if err := os.WriteFile(hostAuthored, []byte("host-authored"), 0o600); err != nil {
		t.Fatalf("writing host-authored file: %v", err)
	}
	out, code := guestSh(t, m, id, "stat -c '%u %g' /workspace/from-host.txt && echo appended >> /workspace/from-host.txt")
	if code != 0 {
		t.Fatalf("the guest agent cannot read/append a host-authored workspace file (exit %d, %q); "+
			"the translation must leave the invoking user's files owned by the in-guest agent", code, out)
	}
	t.Logf("in-guest view of a host-authored file (uid gid): %q", strings.TrimSpace(out))
	// A malformed probe must FAIL, not skip: a `stat` variant emitting fewer
	// than two fields would otherwise leave this whole direction unasserted
	// while the test passed green.
	fields := strings.Fields(strings.TrimSpace(out))
	// The guest's stat output is followed by nothing on success, but the append
	// leg shares the exec, so take exactly the first line's two fields.
	if len(fields) < 2 {
		t.Fatalf("stat output %q did not yield uid+gid; the host->guest ownership direction was NOT asserted", out)
	}
	agentID := strconv.Itoa(int(agentuid.AgentUID))
	if fields[0] != agentID || fields[1] != agentID {
		t.Errorf("a host-authored workspace file appears in-guest as %s:%s, want the agent id %s:%s — "+
			"the agent would not own its own checkout", fields[0], fields[1], agentID, agentID)
	}
	// The host-side owner of that file must be unchanged by the guest's append:
	// a translation that rewrote ownership on write would silently reassign the
	// invoking user's files.
	info, err := os.Stat(hostAuthored)
	if err != nil {
		t.Fatalf("re-stat host-authored file: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("re-stat: unexpected Sys type %T", info.Sys())
	}
	if int(stat.Uid) != wantUID || int(stat.Gid) != wantGID {
		t.Errorf("a host-authored file changed owner to %d:%d after a guest append, want %d:%d",
			stat.Uid, stat.Gid, wantUID, wantGID)
	}
}

// requireQuotaFS gates the real in-guest quota-enforcement leg on a capability
// this box structurally cannot have: a prjquota-ACTIVE filesystem, whose
// provisioning needs root (loop file + `mkfs.xfs`/`mkfs.ext4 -O quota` + mount
// -o prjquota + a project id + limits — every step CAP_SYS_ADMIN, D7 / Global
// Constraint "Rootless is hard"). It mirrors microvmtest.Require's shape: SKIP
// with the reason named when absent, and HARD FAIL when the operator asserts the
// capability is present via COMPASS_REQUIRE_QUOTA_FS=1, so a managed/CI profile
// that is supposed to have it cannot silently stop exercising it.
//
// $COMPASS_TEST_QUOTA_VOLUME names a directory on a project-quota'd filesystem.
// Returning it only after verifyVolumeQuota confirms an ACTIVE bound is the
// honesty gate: a path on an unquota'd filesystem skips (or hard-fails under the
// require flag) rather than running a test that can never observe EDQUOT and
// passing as if it proved quota.
func requireQuotaFS(t *testing.T) string {
	t.Helper()
	required := os.Getenv("COMPASS_REQUIRE_QUOTA_FS") != ""
	refuse := func(format string, args ...any) string {
		t.Helper()
		if required {
			t.Fatalf("COMPASS_REQUIRE_QUOTA_FS is set but "+format, args...)
		}
		t.Skipf("no project-quota'd filesystem available: "+format+
			" — provisioning one needs root (loop + mkfs -o prjquota + project id + limits), which the rootless "+
			"Runner and this suite do not have (D7). The verification LOGIC is proven hermetically in "+
			"microvm_quota_test.go; set $COMPASS_TEST_QUOTA_VOLUME to a quota'd dir to run this leg for real.", args...)
		return ""
	}

	volume := os.Getenv("COMPASS_TEST_QUOTA_VOLUME")
	if volume == "" {
		return refuse("$COMPASS_TEST_QUOTA_VOLUME is unset")
	}
	reading, err := verifyVolumeQuota(volume, VolumeQuota{}, readVolumeQuota)
	if err != nil {
		return refuse("%v", err)
	}
	t.Logf("quota'd volume %s: %s (used ratio %.4f)", volume, reading, reading.UsedRatio())
	return volume
}

// TestMicroVMVolumeQuotaEnforcedInGuest is the real resource-exhaustion leg
// (record §Plan V6: "writes past the byte bound and creates past the inode bound
// fail inside the guest with ENOSPC/EDQUOT while the host filesystem stays
// healthy"). It runs ONLY on a genuinely project-quota'd filesystem and skips
// with the reason named otherwise — it never fake-proves quota.
func TestMicroVMVolumeQuotaEnforcedInGuest(t *testing.T) {
	env := microvmtest.Require(t)
	quotaRoot := requireQuotaFS(t)

	volume, err := os.MkdirTemp(quotaRoot, "iso-quota-") //nolint:usetesting // the volume MUST live on the operator-provided quota'd filesystem, not the test's own TMPDIR — that is the whole capability under test
	if err != nil {
		t.Fatalf("creating a session volume on the quota'd filesystem: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(volume); err != nil {
			t.Errorf("removing quota'd volume %s: %v", volume, err)
		}
	})

	before, err := readVolumeQuota(volume)
	if err != nil {
		t.Fatalf("reading the quota before the fill: %v", err)
	}

	m := NewMicroVMRuntime(e2eConfig(t, env))
	id, err := m.Create(t.Context(), ContainerSpec{
		Name:   "iso-quota",
		UID:    agentuid.AgentUID,
		Mounts: []Mount{{HostPath: volume, ContainerPath: workspaceMountPath}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Remove(t.Context(), id); err != nil {
			t.Errorf("Remove: %v", err)
		}
	})
	if err := m.Start(t.Context(), id); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Write past the byte bound: dd until it fails. The guest MUST hit
	// ENOSPC/EDQUOT rather than consuming the whole host filesystem.
	//
	// Only the REMAINING HEADROOM plus a margin, never the whole limit. The old
	// `LimitBytes/MiB + 64` wrote the entire project limit again on top of
	// whatever was already used, so against a realistically-sized operator quota
	// (10GiB) it was a 10GiB guest write over virtio-fs — past the 120s per-exec
	// cap, which surfaces as a transport TimeoutError (fatal at guestSh) instead
	// of the ENOSPC/EDQUOT verdict this leg exists to prove. Headroom+margin
	// crosses the bound by exactly the same amount while writing only what is
	// actually needed to cross it.
	fillMiB := (before.LimitBytes-before.UsedBytes)/(1<<20) + quotaFillMarginMiB
	if fillMiB > quotaFillCeilingMiB {
		t.Skipf("the quota'd volume %s has %d MiB of headroom, over this leg's %d MiB ceiling: crossing the "+
			"bound would be a %d MiB guest write over virtio-fs, past the %v per-exec cap (execDefaultTimeout), "+
			"so it would fail as a transport timeout rather than with the ENOSPC/EDQUOT verdict it exists to "+
			"assert. Point $COMPASS_TEST_QUOTA_VOLUME at a purpose-sized test project (a few hundred MiB); the "+
			"operator's quota volume for this leg is expected to be small. Observed: %s",
			volume, fillMiB-quotaFillMarginMiB, quotaFillCeilingMiB, fillMiB, execDefaultTimeout, before)
	}
	t.Logf("filling %d MiB (headroom %d MiB + %d MiB margin) against a %v exec cap; limit %d B, used %d B",
		fillMiB, fillMiB-quotaFillMarginMiB, quotaFillMarginMiB, execDefaultTimeout, before.LimitBytes, before.UsedBytes)

	fill := "dd if=/dev/zero of=/workspace/fill bs=1M count=" + strconv.FormatInt(fillMiB, 10) + " 2>&1"
	start := time.Now()
	out, code := guestSh(t, m, id, fill)
	elapsed := time.Since(start)
	if code == 0 {
		t.Fatalf("the guest wrote past the project byte bound (%d B) without failing — the quota is not enforced.\noutput: %q",
			before.LimitBytes, out)
	}
	if !strings.Contains(out, "No space left") && !strings.Contains(out, "Disk quota exceeded") {
		t.Errorf("the over-bound write failed with %q, want an ENOSPC/EDQUOT diagnostic", strings.TrimSpace(out))
	}
	// The ceiling above is a static estimate; this is the measured check that
	// the chosen byte count really fit the budget rather than nearly missing it.
	if elapsed > execDefaultTimeout/2 {
		t.Errorf("the %d MiB fill took %v, over half the %v per-exec cap; lower quotaFillCeilingMiB or use a "+
			"smaller test project", fillMiB, elapsed, execDefaultTimeout)
	}
	t.Logf("over-bound write confined: exit %d in %v (cap %v), %q",
		code, elapsed.Round(time.Millisecond), execDefaultTimeout, strings.TrimSpace(truncate(out)))

	// The HOST filesystem must stay healthy: the mount root's free space is
	// still ample, i.e. the guest exhausted its project, not the filesystem.
	after, err := readVolumeQuota(volume)
	if err != nil {
		t.Fatalf("reading the quota after the fill: %v", err)
	}
	if after.FilesystemBytes-after.UsedBytes <= 0 {
		t.Fatalf("the host filesystem is exhausted after the guest fill (%s); the quota failed to contain it", after)
	}
	t.Logf("host filesystem healthy after the guest fill: %s (used ratio %.4f)", after, after.UsedRatio())
}

// snapshotTree renders a stable, comparable description of every entry under
// root: relative path, mode, size, and host uid/gid. It is the host-side half of
// each escape assertion — an unchanged snapshot after a batch of guest write
// attempts is what "confined" means, in a form a diff can show.
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("stat %s: unexpected Sys type %T", path, info.Sys())
		}
		size := info.Size()
		if entry.IsDir() {
			// A directory's size is filesystem bookkeeping that shifts as
			// entries are added and removed and then re-added, so it is not a
			// stable identity — the entry SET (rendered by the walk itself) is.
			size = -1
		}
		fmt.Fprintf(&b, "%s mode=%s size=%d owner=%d:%d\n", rel, info.Mode(), size, stat.Uid, stat.Gid)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotting %s: %v", root, err)
	}
	return b.String()
}

// truncate bounds a guest output for a log line, so a `grep -r /` that returned
// a lot does not flood the test log.
func truncate(out string) string {
	const max = 512
	if len(out) <= max {
		return out
	}
	return out[:max] + "…(truncated)"
}
