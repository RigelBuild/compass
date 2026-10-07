package vfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// newManager builds a LocalManager over a fresh tempdir base. Every test is
// hermetic: no shared state, no fixed paths, no wall-clock waits — stamp
// timestamps are constructed directly so eligibility is a pure function of
// values the test chose.
func newManager(t *testing.T) *LocalManager {
	t.Helper()
	m, err := NewLocalManager(filepath.Join(t.TempDir(), "volumes"))
	if err != nil {
		t.Fatalf("NewLocalManager: %v", err)
	}
	return m
}

// mustCreate creates a session's volume, failing the test on error.
func mustCreate(t *testing.T, m *LocalManager, sessionID string) Volume {
	t.Helper()
	v, err := m.CreateVolume(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("CreateVolume(%q): %v", sessionID, err)
	}
	return v
}

// mustAttach attaches a volume, failing the test on error.
func mustAttach(t *testing.T, m *LocalManager, v Volume) string {
	t.Helper()
	path, err := m.Attach(t.Context(), v)
	if err != nil {
		t.Fatalf("Attach(%q): %v", v.SessionID, err)
	}
	return path
}

// stampAged writes a close-stamp with an explicitly-constructed timestamp, so
// the test controls eligibility without sleeping. This is the on-disk shape the
// teardown path's Stamp writes; only the clock is the test's.
func stampAged(t *testing.T, v Volume, intent CloseIntent, age time.Duration) {
	t.Helper()
	if err := writeStamp(v.HostRoot, closeStamp{Intent: intent, StampedAt: time.Now().Add(-age)}); err != nil {
		t.Fatalf("writing %s stamp aged %s: %v", intent, age, err)
	}
}

// exists reports whether a path is present, failing the test on any stat error
// other than not-exist (which would otherwise read as a successful reap).
func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true
	case errors.Is(err, os.ErrNotExist):
		return false
	default:
		t.Fatalf("stat %q: %v", path, err)
		return false
	}
}

// TestNewLocalManagerEstablishesBaseDir pins construction: the base dir is
// created if absent, and a base dir that cannot be a directory fails at
// construction rather than at the first launch on the volume path.
func TestNewLocalManagerEstablishesBaseDir(t *testing.T) {
	t.Run("creates an absent base dir", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), "nested", "volumes")
		m, err := NewLocalManager(base)
		if err != nil {
			t.Fatalf("NewLocalManager: %v", err)
		}
		if m.BaseDir() != base {
			t.Errorf("BaseDir() = %q, want %q", m.BaseDir(), base)
		}
		info, err := os.Stat(base)
		if err != nil {
			t.Fatalf("base dir not created: %v", err)
		}
		if !info.IsDir() {
			t.Errorf("base dir %q is not a directory", base)
		}
	})

	t.Run("rejects a base dir that is a file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("writing fixture: %v", err)
		}
		if _, err := NewLocalManager(file); err == nil {
			t.Fatal("NewLocalManager over a regular file succeeded, want an error")
		}
	})

	t.Run("rejects an empty base dir", func(t *testing.T) {
		if _, err := NewLocalManager(""); err == nil {
			t.Fatal("NewLocalManager(\"\") succeeded, want an error")
		}
	})
}

// TestAttachReturnsAStablePath is the P2-GC-d contract: the host path of a
// session's volume is identical on every attach, so `target/` and sccache stay
// valid. A backend that derived any part of the path per-launch would fail here.
func TestAttachReturnsAStablePath(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-stable")

	first := mustAttach(t, m, v)
	second := mustAttach(t, m, v)

	if first != second {
		t.Errorf("Attach twice returned %q then %q, want one stable path", first, second)
	}
	if first != v.HostRoot {
		t.Errorf("Attach returned %q, want the volume's HostRoot %q", first, v.HostRoot)
	}
	if !exists(t, first) {
		t.Errorf("attached path %q does not exist", first)
	}
}

// TestLookupRoundTripsAndTypesNotFound verifies resolution and typed not-found
// behavior without silently creating a missing volume.
func TestLookupRoundTripsAndTypesNotFound(t *testing.T) {
	m := newManager(t)
	created := mustCreate(t, m, "sess-lookup")

	resolved, err := m.Lookup(t.Context(), "sess-lookup")
	if err != nil {
		t.Fatalf("Lookup of a created volume: %v", err)
	}
	if resolved != created {
		t.Errorf("Lookup returned %+v, want %+v", resolved, created)
	}

	_, err = m.Lookup(t.Context(), "sess-never-created")
	if !errors.Is(err, ErrVolumeNotFound) {
		t.Errorf("Lookup of an absent session = %v, want ErrVolumeNotFound", err)
	}

	// Attach cannot invent a volume either: an unresolvable session id fails
	// with the same typed error rather than creating a subtree.
	if _, err := m.Attach(t.Context(), Volume{SessionID: "sess-never-created"}); !errors.Is(err, ErrVolumeNotFound) {
		t.Errorf("Attach of an absent session = %v, want ErrVolumeNotFound", err)
	}
}

// TestVolumeRootRejectsTraversal verifies invalid IDs cannot escape the base
// dir or collide with another session's lock file.
func TestVolumeRootRejectsTraversal(t *testing.T) {
	m := newManager(t)
	badIDs := []string{
		"", "..", ".", "../escape", "a/b", "sess/../../etc",
		// The lock-namespace collision cases: without volumeRoot's
		// HasSuffix check these resolve to a path in the lock namespace.
		"sess-x" + lockFileSuffix,
		"sess-x" + reapingSuffix,
		"sess-x" + metaDirSuffix,
		reapingSuffix,
		lockFileSuffix,
		metaDirSuffix,
	}
	for _, bad := range badIDs {
		if _, err := m.CreateVolume(t.Context(), bad); !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("CreateVolume(%q) = %v, want ErrInvalidSessionID", bad, err)
		}
		if _, err := m.Lookup(t.Context(), bad); !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("Lookup(%q) = %v, want ErrInvalidSessionID", bad, err)
		}
	}
}

// TestCreateVolumeIsIdempotent pins P2-GC-c at the create verb: re-creating a
// session's volume returns the existing one with its contents intact. Volume
// destruction is Expire's alone, so a re-create must never clear a tree.
func TestCreateVolumeIsIdempotent(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-idem")
	marker := filepath.Join(v.HostRoot, "tree-file")
	if err := os.WriteFile(marker, []byte("derived state"), 0o600); err != nil {
		t.Fatalf("writing tree fixture: %v", err)
	}

	again := mustCreate(t, m, "sess-idem")
	if again != v {
		t.Errorf("re-CreateVolume returned %+v, want the existing %+v", again, v)
	}
	if !exists(t, marker) {
		t.Error("re-CreateVolume cleared existing volume contents; only Expire may destroy a volume (P2-GC-c)")
	}
}

// TestReattachAfterRunnerRestartIsStable simulates a Runner restart with a
// fresh manager over the same base dir. The manager holds no per-session state,
// so the same session must resolve and attach to the same path — the stable-path
// invariant across a process boundary, not just across calls.
func TestReattachAfterRunnerRestartIsStable(t *testing.T) {
	base := filepath.Join(t.TempDir(), "volumes")
	first, err := NewLocalManager(base)
	if err != nil {
		t.Fatalf("NewLocalManager: %v", err)
	}
	v := mustCreate(t, first, "sess-restart")
	before := mustAttach(t, first, v)

	restarted, err := NewLocalManager(base)
	if err != nil {
		t.Fatalf("NewLocalManager after restart: %v", err)
	}
	resolved, err := restarted.Lookup(t.Context(), "sess-restart")
	if err != nil {
		t.Fatalf("Lookup after restart: %v", err)
	}
	after := mustAttach(t, restarted, resolved)

	if after != before {
		t.Errorf("path after restart = %q, want the pre-restart %q", after, before)
	}
}

// TestMountedRootIsWritableByCreatingUser verifies the invoking user can
// create the checkout directory and its contents under the volume root.
func TestMountedRootIsWritableByCreatingUser(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-writable")
	root := mustAttach(t, m, v)

	// The agent's first act on the volume is an mkdir of its checkout dir
	// (ensureCheckoutDir), then writing the tree into it — so both a dir and a
	// file under the mounted root must succeed.
	checkout := filepath.Join(root, "checkout")
	if err := os.Mkdir(checkout, 0o700); err != nil {
		t.Fatalf("creating a checkout dir under the mounted root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "file"), []byte("tree bytes"), 0o600); err != nil {
		t.Fatalf("writing under the mounted root: %v", err)
	}
}

// TestStampRecordsCallerIntent pins invariant (c) at the write side: the stamp
// carries the intent the CALLER supplied, because D4's suspend uses the same
// stop+remove teardown path a close does and "the container is gone" cannot
// distinguish them.
func TestStampRecordsCallerIntent(t *testing.T) {
	m := newManager(t)
	for _, intent := range []CloseIntent{IntentClosed, IntentSuspended} {
		v := mustCreate(t, m, "sess-intent-"+intent.String())

		if _, _, ok, err := m.ReadStamp(t.Context(), v); err != nil || ok {
			t.Fatalf("fresh volume ReadStamp ok=%v err=%v, want unstamped", ok, err)
		}
		before := time.Now()
		if err := m.Stamp(t.Context(), v, intent); err != nil {
			t.Fatalf("Stamp(%s): %v", intent, err)
		}
		got, stampedAt, ok, err := m.ReadStamp(t.Context(), v)
		if err != nil || !ok {
			t.Fatalf("ReadStamp after Stamp: ok=%v err=%v", ok, err)
		}
		if got != intent {
			t.Errorf("stamped intent = %s, want %s", got, intent)
		}
		if stampedAt.Before(before.Add(-time.Second)) || stampedAt.After(time.Now().Add(time.Second)) {
			t.Errorf("stampedAt = %s, want a timestamp from this Stamp call", stampedAt)
		}
	}
}

// TestAttachClearsAPastDeadlineStamp verifies a reopened volume survives the
// next expiry pass.
func TestAttachClearsAPastDeadlineStamp(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-reopened")
	stampAged(t, v, IntentClosed, 30*24*time.Hour)

	// Pre-condition: without the Attach this volume is reap-eligible.
	if stamp, err := readStamp(v.HostRoot); err != nil || stamp == nil {
		t.Fatalf("fixture stamp not written: stamp=%v err=%v", stamp, err)
	}

	mustAttach(t, m, v)

	if _, _, ok, err := m.ReadStamp(t.Context(), v); err != nil || ok {
		t.Fatalf("stamp still present after Attach: ok=%v err=%v", ok, err)
	}
	if err := m.Expire(t.Context(), 14*24*time.Hour); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if !exists(t, v.HostRoot) {
		t.Error("reopened volume was reaped: Attach did not clear the past-deadline stamp (invariant (a))")
	}
}

// TestExpireReapsOnlyClosedPastDeadline covers each eligibility state; each
// survivor distinguishes a separate plausible reaper bug.
func TestExpireReapsOnlyClosedPastDeadline(t *testing.T) {
	const retention = 14 * 24 * time.Hour
	old := 30 * 24 * time.Hour
	fresh := 1 * time.Hour

	setups := []struct {
		name      string
		sessionID string
		// setup puts the volume into its state.
		setup      func(t *testing.T, m *LocalManager, v Volume)
		wantReaped bool
		// why names the bug this row catches if the verdict flips.
		why string
	}{
		{
			name:      "live session, never stamped",
			sessionID: "sess-live",
			setup:     func(*testing.T, *LocalManager, Volume) {},
			why:       "an unstamped volume belongs to a LIVE session; reaping it destroys a running session's tree",
		},
		{
			name:      "suspended past the deadline",
			sessionID: "sess-suspended",
			setup: func(t *testing.T, _ *LocalManager, v Volume) {
				t.Helper()
				stampAged(t, v, IntentSuspended, old)
			},
			why: "a suspended session is never eligible however old (invariant (c)); its resume expects this exact volume at this exact path",
		},
		{
			name:      "closed inside the retention window",
			sessionID: "sess-recent",
			setup: func(t *testing.T, _ *LocalManager, v Volume) {
				t.Helper()
				stampAged(t, v, IntentClosed, fresh)
			},
			why: "a recently-closed volume is inside its retention window; reaping it breaks the reopen-a-closed-session path",
		},
		{
			name:      "closed past the deadline then reopened",
			sessionID: "sess-reopened",
			setup: func(t *testing.T, m *LocalManager, v Volume) {
				t.Helper()
				stampAged(t, v, IntentClosed, old)
				mustAttach(t, m, v)
			},
			why: "Attach cleared the stamp (invariant (a)); reaping it means the reaper acted on a stale read",
		},
		{
			name:      "closed past the deadline",
			sessionID: "sess-expired",
			setup: func(t *testing.T, _ *LocalManager, v Volume) {
				t.Helper()
				stampAged(t, v, IntentClosed, old)
			},
			wantReaped: true,
			why:        "the ONLY eligible state: closed intent, stamp older than the retention window",
		},
	}

	m := newManager(t)
	roots := make(map[string]string, len(setups))
	for _, s := range setups {
		v := mustCreate(t, m, s.sessionID)
		s.setup(t, m, v)
		roots[s.sessionID] = v.HostRoot
	}

	if err := m.Expire(t.Context(), retention); err != nil {
		t.Fatalf("Expire: %v", err)
	}

	for _, s := range setups {
		t.Run(s.name, func(t *testing.T) {
			gone := !exists(t, roots[s.sessionID])
			if gone != s.wantReaped {
				t.Errorf("reaped = %v, want %v: %s", gone, s.wantReaped, s.why)
			}
			if !s.wantReaped {
				// A survivor must still be resolvable — Expire must not have
				// left it half-deleted.
				if _, err := m.Lookup(t.Context(), s.sessionID); err != nil {
					t.Errorf("surviving volume no longer resolves: %v", err)
				}
			} else if _, err := m.Lookup(t.Context(), s.sessionID); !errors.Is(err, ErrVolumeNotFound) {
				t.Errorf("Lookup of a reaped volume = %v, want ErrVolumeNotFound", err)
			}
		})
	}
}

// TestReconcileOrphansStampsUnstampedVolumesAtDiscovery verifies crash orphans
// receive a full retention window from discovery before they can be reaped.
func TestReconcileOrphansStampsUnstampedVolumesAtDiscovery(t *testing.T) {
	m := newManager(t)
	orphan := mustCreate(t, m, "sess-orphan")
	// A suspended volume, stamped by its normal teardown, must be untouched by
	// the pass: it is already stamped, so reconciliation has no business in it.
	suspended := mustCreate(t, m, "sess-suspended")
	stampAged(t, suspended, IntentSuspended, 30*24*time.Hour)
	suspendedBefore, _, _, err := m.ReadStamp(t.Context(), suspended)
	if err != nil {
		t.Fatalf("ReadStamp(suspended): %v", err)
	}

	before := time.Now()
	if err := m.ReconcileOrphans(t.Context()); err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}

	intent, stampedAt, ok, err := m.ReadStamp(t.Context(), orphan)
	if err != nil || !ok {
		t.Fatalf("orphan unstamped after reconcile: ok=%v err=%v", ok, err)
	}
	if intent != IntentClosed {
		t.Errorf("orphan intent = %s, want closed", intent)
	}
	if stampedAt.Before(before.Add(-time.Second)) {
		t.Errorf("orphan stampedAt = %s, want a DISCOVERY-time stamp (>= %s), not the lost close time", stampedAt, before)
	}
	if got, _, _, err := m.ReadStamp(t.Context(), suspended); err != nil || got != suspendedBefore {
		t.Errorf("reconcile rewrote an already-stamped volume: intent %s -> %s (err=%v)", suspendedBefore, got, err)
	}

	// Fails safe: the discovery deadline has not passed, so a retention-window
	// Expire leaves the orphan alone.
	if err := m.Expire(t.Context(), 14*24*time.Hour); err != nil {
		t.Fatalf("Expire within the discovery window: %v", err)
	}
	if !exists(t, orphan.HostRoot) {
		t.Fatal("orphan reaped inside its discovery-based retention window; the deadline must run from discovery")
	}

	// Invariant (a) still applies to a discovery stamp: re-provisioning the
	// session before its deadline undoes it for free.
	mustAttach(t, m, orphan)
	if _, _, ok, err := m.ReadStamp(t.Context(), orphan); err != nil || ok {
		t.Fatalf("Attach did not clear the discovery stamp: ok=%v err=%v", ok, err)
	}
	// And with the stamp cleared the orphan is now indistinguishable from a
	// live session: even a zero-window Expire must not touch it.
	if err := m.Expire(t.Context(), 0); err != nil {
		t.Fatalf("Expire after re-attach: %v", err)
	}
	if !exists(t, orphan.HostRoot) {
		t.Fatal("re-attached orphan reaped; a cleared stamp means live")
	}

	// Re-crash: reconcile stamps it again, and once the deadline has passed it
	// is reaped. A negative window is the deterministic way to say "the
	// discovery deadline has passed" without sleeping — it makes
	// now-stampedAt > olderThan true for the just-written discovery stamp.
	if err := m.ReconcileOrphans(t.Context()); err != nil {
		t.Fatalf("ReconcileOrphans (second pass): %v", err)
	}
	if err := m.Expire(t.Context(), -time.Second); err != nil {
		t.Fatalf("Expire past the discovery deadline: %v", err)
	}
	if exists(t, orphan.HostRoot) {
		t.Error("orphan survived an Expire past its discovery deadline; a crash orphan must always be reachable by the reaper")
	}
}

// TestReconcileOrphansSkipsAVolumeBeingAttached pins the pass's own contention
// rule: a volume whose per-volume lock is held is being attached-live, which is
// the opposite of orphaned, so the pass must leave it unstamped rather than
// stamping a live session closed.
func TestReconcileOrphansSkipsAVolumeBeingAttached(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-attaching")

	lock, err := tryLockVolume(t.Context(), v.HostRoot)
	if err != nil {
		t.Fatalf("acquiring the stand-in Attach lock: %v", err)
	}
	if lock == nil {
		t.Fatal("stand-in Attach lock reported contention on a fresh volume")
	}

	if err := m.ReconcileOrphans(t.Context()); err != nil {
		t.Fatalf("ReconcileOrphans with a held lock returned an error, want a silent skip: %v", err)
	}
	if _, _, ok, err := m.ReadStamp(t.Context(), v); err != nil || ok {
		t.Errorf("reconcile stamped a volume held by a live Attach: ok=%v err=%v", ok, err)
	}

	if err := lock.release(); err != nil {
		t.Fatalf("releasing the stand-in Attach lock: %v", err)
	}
	// Once the Attach completes, the same pass does stamp it.
	if err := m.ReconcileOrphans(t.Context()); err != nil {
		t.Fatalf("ReconcileOrphans after release: %v", err)
	}
	if _, _, ok, err := m.ReadStamp(t.Context(), v); err != nil || !ok {
		t.Errorf("reconcile skipped an unlocked orphan: ok=%v err=%v", ok, err)
	}
}

// TestReaperIgnoresNonVolumeDirs verifies only marked volume directories are
// reaped; unrelated directories under the base dir must remain untouched.
func TestReaperIgnoresNonVolumeDirs(t *testing.T) {
	m := newManager(t)

	// A foreign directory in the base dir, with no marker dir — stands in for
	// W2's snapshot store.
	stray := filepath.Join(m.BaseDir(), "snapshots")
	if err := os.Mkdir(stray, 0o700); err != nil {
		t.Fatalf("creating the non-volume dir: %v", err)
	}
	sentinel := filepath.Join(stray, "snapshot-payload")
	if err := os.WriteFile(sentinel, []byte("another subtree's bytes"), 0o600); err != nil {
		t.Fatalf("writing the non-volume sentinel: %v", err)
	}

	// A genuine crash orphan beside it, so the pass is proven to still WORK
	// rather than passing because it did nothing at all.
	orphan := mustCreate(t, m, "sess-orphan")

	if err := m.ReconcileOrphans(t.Context()); err != nil {
		t.Fatalf("ReconcileOrphans with a non-volume dir present: %v", err)
	}
	// Never stamped: the pass must not have created a metadata dir for it.
	if exists(t, metaDir(stray)) {
		t.Error("ReconcileOrphans stamped a directory with no volume marker; volume identity must be structural")
	}
	if _, _, ok, err := m.ReadStamp(t.Context(), orphan); err != nil || !ok {
		t.Fatalf("the genuine orphan beside it went unstamped, so this test proves nothing: ok=%v err=%v", ok, err)
	}

	// And a zero-window Expire — which reaps anything it considers an eligible
	// volume — must leave the stray subtree and its contents untouched.
	if err := m.Expire(t.Context(), 0); err != nil {
		t.Fatalf("Expire with a non-volume dir present: %v", err)
	}
	if !exists(t, stray) {
		t.Fatal("Expire deleted a directory with no volume marker; the base dir is not this package's exclusively (W2's snapshot store is a sibling subtree)")
	}
	if !exists(t, sentinel) {
		t.Error("Expire emptied a non-volume directory; its contents must survive untouched")
	}
	// The genuine orphan, by contrast, IS reachable by the reaper.
	if exists(t, orphan.HostRoot) {
		t.Error("the marked crash orphan survived a zero-window Expire; the marker check must not have made genuine volumes invisible")
	}
}

// TestReconcileOrphansDoesNotResurrectAReapedRoot verifies a volume removed
// after discovery is not recreated by reconciliation.
func TestReconcileOrphansDoesNotResurrectAReapedRoot(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-reaped-mid-reconcile")

	// The reap that landed between discovery and this pass's lock acquisition.
	if err := os.RemoveAll(v.HostRoot); err != nil {
		t.Fatalf("reaping the volume root: %v", err)
	}

	// The mutator the pass would run on the discovered-then-reaped root. A
	// reaped volume is not an orphan to stamp: the typed not-found is swallowed,
	// so the pass reports success without touching the filesystem.
	if err := stampOrphanLocked(v.HostRoot, time.Now()); err != nil {
		t.Fatalf("stampOrphanLocked on a reaped root = %v, want nil (a reaped volume is not an orphan to stamp)", err)
	}

	// The load-bearing consequence: the root stays absent, so Lookup still
	// reports the reap and the provision path cold-materializes rather than
	// warm-attaching an empty shell.
	if exists(t, v.HostRoot) {
		t.Fatal("stampOrphanLocked resurrected a reaped volume root; a reconcile pass must never recreate a volume it did not find live")
	}
	if _, err := m.Lookup(t.Context(), v.SessionID); !errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("Lookup after a reaped-root reconcile = %v, want ErrVolumeNotFound (else provision warm-attaches an empty volume)", err)
	}
}

// TestExpireSkipsALockedVolume is invariant (b)'s observable half: the
// per-volume advisory lock is what closes the window between the reaper reading
// a stamp and a concurrent Attach clearing it. Holding the lock stands in for
// that in-flight Attach — an eligible-looking volume must be SKIPPED, and the
// skip is not an error (contention is exactly the signal not to reap).
func TestExpireSkipsALockedVolume(t *testing.T) {
	m := newManager(t)
	held := mustCreate(t, m, "sess-held")
	free := mustCreate(t, m, "sess-free")
	stampAged(t, held, IntentClosed, 30*24*time.Hour)
	stampAged(t, free, IntentClosed, 30*24*time.Hour)

	lock, err := tryLockVolume(t.Context(), held.HostRoot)
	if err != nil {
		t.Fatalf("acquiring the stand-in Attach lock: %v", err)
	}
	if lock == nil {
		t.Fatal("stand-in Attach lock reported contention on a fresh volume")
	}

	if err := m.Expire(t.Context(), 14*24*time.Hour); err != nil {
		t.Fatalf("Expire with a contended volume returned an error, want a silent skip: %v", err)
	}
	if !exists(t, held.HostRoot) {
		t.Error("Expire reaped a volume whose per-volume lock was held by a live Attach (invariant (b))")
	}
	if exists(t, free.HostRoot) {
		t.Error("Expire skipped the uncontended eligible volume; one contended volume must not pin the pass")
	}

	// With the stand-in Attach finished, the next pass reaps it — the skip is a
	// deferral, not a permanent pin.
	if err := lock.release(); err != nil {
		t.Fatalf("releasing the stand-in Attach lock: %v", err)
	}
	if err := m.Expire(t.Context(), 14*24*time.Hour); err != nil {
		t.Fatalf("Expire after release: %v", err)
	}
	if exists(t, held.HostRoot) {
		t.Error("volume survived the pass after its lock was released; the skip must be a deferral")
	}
}

// TestExpireRejectsACorruptStamp pins the fail-safe reading of an
// unintelligible stamp: it is an error, never a silent collapse into
// "unstamped" (which would look live forever) or into a defaulted IntentClosed
// (which would reap on a stamp nobody can read).
func TestExpireRejectsACorruptStamp(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-corrupt")
	if err := os.WriteFile(stampPath(v.HostRoot), []byte("{not json"), stampFileMode); err != nil {
		t.Fatalf("writing corrupt stamp: %v", err)
	}

	if err := m.Expire(t.Context(), 0); err == nil {
		t.Error("Expire over a corrupt stamp succeeded, want a surfaced decode error")
	}
	if !exists(t, v.HostRoot) {
		t.Error("Expire reaped a volume whose stamp could not be decoded")
	}
}

// TestReservedVerbsReturnHonestSentinels pins the reserved-not-implemented
// surface: each verb fails with its own errors.Is-detectable sentinel and a
// zero result, so no caller can read a nil error or an empty id as success.
// W2 replaces Snapshot's body; D4 replaces Archive's and Restore's (OQ-2).
func TestReservedVerbsReturnHonestSentinels(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-reserved")

	if id, err := m.Snapshot(t.Context(), v); !errors.Is(err, ErrSnapshotNotImplemented) || id != "" {
		t.Errorf("Snapshot = (%q, %v), want (\"\", ErrSnapshotNotImplemented)", id, err)
	}
	if ref, err := m.Archive(t.Context(), v); !errors.Is(err, ErrArchiveNotImplemented) || ref != "" {
		t.Errorf("Archive = (%q, %v), want (\"\", ErrArchiveNotImplemented)", ref, err)
	}
	if got, err := m.Restore(t.Context(), ArchiveRef("ref-x")); !errors.Is(err, ErrRestoreNotImplemented) || got != (Volume{}) {
		t.Errorf("Restore = (%+v, %v), want (Volume{}, ErrRestoreNotImplemented)", got, err)
	}
}

// TestCloseIntentZeroValueIsClosed pins the enum's deliberate zero value: a
// stamp written with a defaulted intent expires (a bounded storage leak) rather
// than pinning the volume forever (an unbounded one), and a discovered orphan —
// which by construction has no caller intent — wants exactly IntentClosed.
func TestCloseIntentZeroValueIsClosed(t *testing.T) {
	var zero CloseIntent
	if zero != IntentClosed {
		t.Errorf("zero CloseIntent = %v, want IntentClosed", zero)
	}
	if IntentClosed.String() != "closed" || IntentSuspended.String() != "suspended" {
		t.Errorf("intent names = %q/%q, want closed/suspended", IntentClosed, IntentSuspended)
	}
}

// TestAttachRacingAReapDoesNotReturnAReapedPath gates on Attach being parked in
// its context-aware poll before the reaper removes the tree.
func TestAttachRacingAReapDoesNotReturnAReapedPath(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-relaunch-at-deadline")
	sentinel := filepath.Join(v.HostRoot, "target-artifact")
	if err := os.WriteFile(sentinel, []byte("warm build cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	stampAged(t, v, IntentClosed, 30*24*time.Hour)
	synctest.Test(t, func(t *testing.T) {
		reaper, err := tryLockVolume(t.Context(), v.HostRoot)
		if err != nil || reaper == nil {
			t.Fatalf("acquiring stand-in reaper lock: %v", err)
		}
		type result struct {
			path string
			err  error
		}
		done := make(chan result, 1)
		go func() { path, err := m.Attach(t.Context(), v); done <- result{path, err} }()
		synctest.Wait()
		if err := os.RemoveAll(v.HostRoot); err != nil {
			t.Fatal(err)
		}
		if err := reaper.release(); err != nil {
			t.Fatal(err)
		}
		got := <-done
		if !errors.Is(got.err, ErrVolumeNotFound) && (got.err != nil || exists(t, filepath.Join(got.path, "target-artifact"))) {
			t.Fatalf("Attach = (%q, %v), want not-found or intact path", got.path, got.err)
		}
	})
}

func TestAttachReturnsCtxErrWhileLockIsHeld(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-cancel-lock")
	stampAged(t, v, IntentClosed, 30*24*time.Hour)
	synctest.Test(t, func(t *testing.T) {
		held, err := tryLockVolume(t.Context(), v.HostRoot)
		if err != nil || held == nil {
			t.Fatalf("acquire lock: %v", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { _, err := m.Attach(ctx, v); done <- err }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Attach error = %v, want context.Canceled", err)
		}
		stamp, err := readStamp(v.HostRoot)
		if err != nil || stamp == nil || stamp.Intent != IntentClosed {
			t.Fatalf("stamp changed during canceled Attach: %#v, %v", stamp, err)
		}
		if !exists(t, v.HostRoot) {
			t.Fatal("canceled Attach removed root")
		}
		ctx2, cancel2 := context.WithCancel(t.Context())
		done2 := make(chan error, 1)
		go func() { done2 <- m.Stamp(ctx2, v, IntentClosed) }()
		synctest.Wait()
		cancel2()
		if err := <-done2; !errors.Is(err, context.Canceled) {
			t.Fatalf("Stamp error = %v, want context.Canceled", err)
		}
		stamp, err = readStamp(v.HostRoot)
		if err != nil || stamp == nil {
			t.Fatalf("Stamp changed disk state: %#v, %v", stamp, err)
		}
		if err := held.release(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAttachWithCancelledCtxTouchesNoLock(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-cancel-before-lock")
	// CreateVolume leaves its lock file; drop it so a recreated one is visible.
	if err := os.Remove(v.HostRoot + lockFileSuffix); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := m.Attach(ctx, v); !errors.Is(err, context.Canceled) {
		t.Fatalf("Attach error = %v, want context.Canceled", err)
	}
	if exists(t, v.HostRoot+lockFileSuffix) {
		t.Fatal("canceled Attach created a lock file")
	}
}

func TestLockAcquisitionConvergesOnTheLiveInode(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-inode-reclaim")
	synctest.Test(t, func(t *testing.T) {
		held, err := tryLockVolume(t.Context(), v.HostRoot)
		if err != nil || held == nil {
			t.Fatalf("acquire holder: %v", err)
		}
		done := make(chan *volumeLock, 1)
		go func() { l, _ := lockVolume(t.Context(), v.HostRoot); done <- l }()
		synctest.Wait()
		if err := os.Remove(v.HostRoot + lockFileSuffix); err != nil {
			t.Fatal(err)
		}
		if err := held.release(); err != nil {
			t.Fatal(err)
		}
		waiter := <-done
		if waiter == nil {
			t.Fatal("waiter returned nil lock")
		}
		defer waiter.release()
		fdInfo, err := waiter.f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		pathInfo, err := os.Stat(v.HostRoot + lockFileSuffix)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(fdInfo, pathInfo) {
			t.Fatal("waiter holds an unlinked inode")
		}
		if l, err := tryLockVolume(t.Context(), v.HostRoot); err != nil || l != nil {
			t.Fatalf("fresh acquisition = (%v, %v), want contention", l, err)
		}
	})
}

func TestReapIsAtomicUnderPartialRemoveFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission failure cannot be forced as root")
	}
	m := newManager(t)
	v := mustCreate(t, m, "sess-partial-reap")
	pinned := filepath.Join(v.HostRoot, "pinned")
	if err := os.Mkdir(pinned, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pinned, "held"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		for _, p := range []string{pinned, filepath.Join(reapingPath(v.HostRoot), "pinned")} {
			if exists(t, p) {
				if err := os.Chmod(p, 0o700); err != nil {
					t.Error(err)
				}
			}
		}
	}
	t.Cleanup(cleanup)
	if err := os.Chmod(pinned, 0o500); err != nil {
		t.Fatal(err)
	}
	stampAged(t, v, IntentClosed, 30*24*time.Hour)
	if err := m.Expire(t.Context(), 14*24*time.Hour); err == nil {
		t.Fatal("Expire succeeded despite pinned subtree")
	}
	if _, err := m.Lookup(t.Context(), v.SessionID); !errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("Lookup = %v, want not-found", err)
	}
	if _, err := m.Attach(t.Context(), v); !errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("Attach = %v, want not-found", err)
	}
	if !exists(t, reapingPath(v.HostRoot)) || !exists(t, filepath.Join(reapingPath(v.HostRoot), "pinned", "held")) {
		t.Fatal("partial removal did not leave inert renamed tree")
	}
	if err := m.ReconcileOrphans(t.Context()); err != nil {
		t.Fatal(err)
	}
	if exists(t, reapingPath(v.HostRoot)+lockFileSuffix) {
		t.Fatal("ReconcileOrphans treated the reaping leftover as a volume")
	}
	if err := os.Chmod(filepath.Join(reapingPath(v.HostRoot), "pinned"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.Expire(t.Context(), 14*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if exists(t, reapingPath(v.HostRoot)) {
		t.Fatal("leftover survived retry")
	}
}

func TestReapRenameFailureLeavesVolumeIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission failure cannot be forced as root")
	}
	m := newManager(t)
	v := mustCreate(t, m, "sess-rename-failure")
	lock, err := tryLockVolume(t.Context(), v.HostRoot)
	if err != nil || lock == nil {
		t.Fatalf("lock: %v", err)
	}
	if err := lock.release(); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(v.HostRoot, "sentinel")
	if err := os.WriteFile(sentinel, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stampAged(t, v, IntentClosed, 30*24*time.Hour)
	if err := os.Chmod(m.BaseDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(m.BaseDir(), 0o700); err != nil {
			t.Error(err)
		}
	})
	if err := m.Expire(t.Context(), 14*24*time.Hour); err == nil {
		t.Fatal("Expire succeeded with read-only base dir")
	}
	if !exists(t, v.HostRoot) || !exists(t, sentinel) || !exists(t, stampPath(v.HostRoot)) {
		t.Fatal("failed rename modified volume")
	}
	if _, err := m.Lookup(t.Context(), v.SessionID); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
}

func TestExpireSweepsAReapingLeftoverWhateverItsStamp(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-sweep-leftover")
	stampAged(t, v, IntentSuspended, time.Second)
	if err := os.Rename(v.HostRoot, reapingPath(v.HostRoot)); err != nil {
		t.Fatal(err)
	}
	if err := m.Expire(t.Context(), 14*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if exists(t, reapingPath(v.HostRoot)) {
		t.Fatal("suspended leftover survived sweep")
	}
	v = mustCreate(t, m, v.SessionID)
	if err := os.Rename(v.HostRoot, reapingPath(v.HostRoot)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateVolume(t.Context(), v.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := m.Expire(t.Context(), 14*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if !exists(t, v.HostRoot) || exists(t, reapingPath(v.HostRoot)) {
		t.Fatal("sweep removed live volume or left stale tree")
	}
	stampAged(t, v, IntentClosed, 30*24*time.Hour)
	if err := os.Rename(v.HostRoot, reapingPath(v.HostRoot)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateVolume(t.Context(), v.SessionID); err != nil {
		t.Fatal(err)
	}
	stampAged(t, v, IntentClosed, 30*24*time.Hour)
	if err := m.Expire(t.Context(), 14*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if exists(t, v.HostRoot) || exists(t, reapingPath(v.HostRoot)) {
		t.Fatal("clear-leftover/reap did not remove both trees")
	}
}

func TestReconcileOrphansIgnoresAReapingLeftover(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-reconcile-leftover")
	if err := os.Rename(v.HostRoot, reapingPath(v.HostRoot)); err != nil {
		t.Fatal(err)
	}
	if err := m.ReconcileOrphans(t.Context()); err != nil {
		t.Fatal(err)
	}
	stamp, err := readStamp(reapingPath(v.HostRoot))
	if err != nil {
		t.Fatal(err)
	}
	if stamp != nil {
		t.Fatal("ReconcileOrphans stamped leftover")
	}
}

// TestVolumeLockSurvivesAReap pins the placement the fix rests on: the
// per-volume lock file lives OUTSIDE the volume root, so reaping the root
// cannot unlink the inode the lock is held on.
//
// That is what makes mutual exclusion hold across a reap+recreate. flock locks
// an inode; a lock file inside the reaped subtree would be unlinked by the
// reap, so a later lockVolume — after a cold CreateVolume recreated the root —
// would open a NEW inode and lock that, letting two actors hold "the" volume
// lock at once and making any under-lock existence check worthless. Holding the
// lock across a reap must therefore still exclude a second acquisition.
func TestVolumeLockSurvivesAReap(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-reaped-under-lock")

	held, err := tryLockVolume(t.Context(), v.HostRoot)
	if err != nil {
		t.Fatalf("acquiring the volume lock: %v", err)
	}
	if held == nil {
		t.Fatal("volume lock reported contention on a fresh volume")
	}

	if err := os.RemoveAll(v.HostRoot); err != nil {
		t.Fatalf("reaping the volume root: %v", err)
	}

	// Still held: a second non-blocking acquisition must report contention
	// ((nil, nil)), proving it reached the same surviving inode.
	second, err := tryLockVolume(t.Context(), v.HostRoot)
	if err != nil {
		t.Fatalf("second lockVolume after the reap: %v", err)
	}
	if second != nil {
		if releaseErr := second.release(); releaseErr != nil {
			t.Errorf("releasing the unexpectedly-acquired second lock: %v", releaseErr)
		}
		t.Error("a second lockVolume acquired the lock while it was still held across a reap: the lock file was destroyed with the volume root, so mutual exclusion is broken across reap+recreate")
	}

	// And a recreate by the holder does not split the lock either: the
	// recreated volume's lock is the same inode, so it is still contended.
	if err := createLocked(v.HostRoot); err != nil {
		t.Fatalf("recreating the volume: %v", err)
	}
	afterRecreate, err := tryLockVolume(t.Context(), v.HostRoot)
	if err != nil {
		t.Fatalf("lockVolume after the recreate: %v", err)
	}
	if afterRecreate != nil {
		if releaseErr := afterRecreate.release(); releaseErr != nil {
			t.Errorf("releasing the unexpectedly-acquired post-recreate lock: %v", releaseErr)
		}
		t.Error("lockVolume acquired the lock after a reap+recreate while it was still held: the recreate produced a second lock inode")
	}

	if err := held.release(); err != nil {
		t.Fatalf("releasing the held volume lock: %v", err)
	}
}

// A contended tryLockVolume must close the fd it opened; Expire hits this on
// every pass that meets a held lock.
func TestTryLockVolumeClosesItsFdOnContention(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-contended-fd")
	held, err := tryLockVolume(t.Context(), v.HostRoot)
	if err != nil || held == nil {
		t.Fatalf("holding lock: %v", err)
	}
	t.Cleanup(func() {
		if err := held.release(); err != nil {
			t.Error(err)
		}
	})
	openFds := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Skipf("fd count unavailable: %v", err)
		}
		return len(entries)
	}
	before := openFds()
	for range 50 {
		if l, err := tryLockVolume(t.Context(), v.HostRoot); err != nil || l != nil {
			t.Fatalf("tryLockVolume = (%v, %v), want contention", l, err)
		}
	}
	// Slack for runtime-owned fds; a leak adds one per attempt.
	if after := openFds(); after-before > 5 {
		t.Fatalf("open fds grew from %d to %d over 50 contended attempts", before, after)
	}
}

func TestExpireReclaimsOrphanLockFilesOnlyWhenUncontended(t *testing.T) {
	m := newManager(t)
	makeLock := func(id string) Volume {
		v := mustCreate(t, m, id)
		l, err := tryLockVolume(t.Context(), v.HostRoot)
		if err != nil || l == nil {
			t.Fatalf("lock %s: %v", id, err)
		}
		if err := l.release(); err != nil {
			t.Fatal(err)
		}
		return v
	}
	a := makeLock("sess-lock-orphan-a")
	if err := os.RemoveAll(a.HostRoot); err != nil {
		t.Fatal(err)
	}
	b := makeLock("sess-lock-orphan-b")
	c := makeLock("sess-lock-orphan-c")
	if err := os.RemoveAll(c.HostRoot); err != nil {
		t.Fatal(err)
	}
	held, err := tryLockVolume(t.Context(), c.HostRoot)
	if err != nil || held == nil {
		t.Fatalf("hold c: %v", err)
	}
	if err := m.Expire(t.Context(), 14*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if exists(t, a.HostRoot+lockFileSuffix) {
		t.Fatal("orphan lock A remains")
	}
	if !exists(t, b.HostRoot) || !exists(t, b.HostRoot+lockFileSuffix) {
		t.Fatal("live lock B was reclaimed")
	}
	if !exists(t, c.HostRoot+lockFileSuffix) {
		t.Fatal("contended lock C was reclaimed")
	}
	if err := held.release(); err != nil {
		t.Fatal(err)
	}
	if err := m.Expire(t.Context(), 14*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if exists(t, c.HostRoot+lockFileSuffix) {
		t.Fatal("uncontended orphan C remains")
	}
}

func TestReapLeavesNoOrphanLockInTheCommonCase(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-lock-reap")
	l, err := tryLockVolume(t.Context(), v.HostRoot)
	if err != nil || l == nil {
		t.Fatalf("lock: %v", err)
	}
	if err := l.release(); err != nil {
		t.Fatal(err)
	}
	stampAged(t, v, IntentClosed, 30*24*time.Hour)
	if err := m.Expire(t.Context(), 14*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if exists(t, v.HostRoot) || exists(t, reapingPath(v.HostRoot)) || exists(t, v.HostRoot+lockFileSuffix) {
		t.Fatal("reap left root, leftover, or lock")
	}
}

func TestExpireReclaimsTheLockAfterSweepingALeftover(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-lock-leftover")
	l, err := tryLockVolume(t.Context(), v.HostRoot)
	if err != nil || l == nil {
		t.Fatalf("lock: %v", err)
	}
	if err := l.release(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(v.HostRoot, reapingPath(v.HostRoot)); err != nil {
		t.Fatal(err)
	}
	if err := m.Expire(t.Context(), 14*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if exists(t, reapingPath(v.HostRoot)) || exists(t, v.HostRoot+lockFileSuffix) {
		t.Fatal("leftover or orphan lock survived sweep")
	}
}

// TestVolumeLockFileIsOutsideTheVolumeRoot pins the placement structurally, and
// pins that acquiring a lock creates nothing at the volume root. A lockVolume
// that recreated a reaped root would make Attach's under-lock existence check
// see a live volume with no contents.
func TestVolumeLockFileIsOutsideTheVolumeRoot(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-lock-placement")
	if err := os.RemoveAll(v.HostRoot); err != nil {
		t.Fatalf("reaping the volume root: %v", err)
	}

	lock, err := tryLockVolume(t.Context(), v.HostRoot)
	if err != nil {
		t.Fatalf("acquiring the volume lock on a reaped volume: %v", err)
	}
	if lock == nil {
		t.Fatal("volume lock reported contention on an uncontended volume")
	}
	if exists(t, v.HostRoot) {
		t.Error("lockVolume created something inside the volume root; acquiring a lock must not resurrect a reaped root")
	}
	if !strings.HasPrefix(lock.f.Name(), v.HostRoot+".") {
		t.Errorf("lock file %q is not a sibling of the volume root %q", lock.f.Name(), v.HostRoot)
	}
	if err := lock.release(); err != nil {
		t.Fatalf("releasing the volume lock: %v", err)
	}

	// The stamp is also a sibling, out of the agent-owned root, and the
	// sibling lock file is never mistaken for a volume.
	live := mustCreate(t, m, "sess-stamped")
	stampAged(t, live, IntentClosed, 30*24*time.Hour)
	if strings.HasPrefix(stampPath(live.HostRoot), live.HostRoot+string(os.PathSeparator)) {
		t.Errorf("stamp path %q is inside the agent-owned volume root %q", stampPath(live.HostRoot), live.HostRoot)
	}
	// Materialize the sibling lock file, then release it: a still-held lock
	// would make Expire SKIP the volume, which would pass this check for the
	// wrong reason.
	siblingLock, err := tryLockVolume(t.Context(), live.HostRoot)
	if err != nil {
		t.Fatalf("creating the sibling lock file for the iteration check: %v", err)
	}
	if siblingLock == nil {
		t.Fatal("sibling lock reported contention on a fresh volume")
	}
	if err := siblingLock.release(); err != nil {
		t.Fatalf("releasing the sibling lock: %v", err)
	}
	if !exists(t, live.HostRoot+lockFileSuffix) {
		t.Fatalf("no sibling lock file at %q; the iteration check would prove nothing", live.HostRoot+lockFileSuffix)
	}
	// The sibling .lock FILES must be skipped by volume iteration (eachVolume
	// takes directories only) while the eligible volume is still reaped.
	if err := m.Expire(t.Context(), 14*24*time.Hour); err != nil {
		t.Fatalf("Expire with sibling lock files present: %v", err)
	}
	if exists(t, live.HostRoot) {
		t.Error("Expire did not reap the eligible volume")
	}
	if exists(t, live.HostRoot+lockFileSuffix) {
		t.Error("Expire left the reclaimed volume lock")
	}
	if exists(t, metaDir(live.HostRoot)) {
		t.Error("Expire left the reaped volume's metadata dir")
	}
}

// wipeRoot deletes every entry inside a volume root, as an agent's rm -rf of
// its own tree (dotfiles included) would.
func wipeRoot(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("listing %q: %v", root, err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			t.Fatalf("wiping %q: %v", e.Name(), err)
		}
	}
}

// An agent owns its volume root in-container, so nothing it deletes there may
// hide the volume from the reaper: a crash orphan it wiped must still be reaped.
func TestAWipedVolumeRootStaysReapable(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-wiped")
	wipeRoot(t, v.HostRoot)

	if err := m.ReconcileOrphans(t.Context()); err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if _, _, ok, err := m.ReadStamp(t.Context(), v); err != nil || !ok {
		t.Fatalf("wiped orphan unstamped after reconcile: ok=%v err=%v", ok, err)
	}
	if err := m.Expire(t.Context(), -time.Second); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if exists(t, v.HostRoot) {
		t.Fatal("a volume whose root the agent wiped survived Expire past its deadline")
	}
}

// A crash after a reap's rename leaves the slot's metadata behind. Recreating
// the volume must not inherit the reaped volume's past-deadline closed stamp.
func TestCreateAfterInterruptedReapDropsTheOldStamp(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-interrupted-reap")
	stampAged(t, v, IntentClosed, 30*24*time.Hour)
	if err := os.Rename(v.HostRoot, reapingPath(v.HostRoot)); err != nil {
		t.Fatal(err)
	}

	mustCreate(t, m, v.SessionID)
	if _, _, ok, err := m.ReadStamp(t.Context(), v); err != nil || ok {
		t.Fatalf("recreated volume carries the reaped volume's stamp: ok=%v err=%v", ok, err)
	}
	if err := m.Expire(t.Context(), 14*24*time.Hour); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if !exists(t, v.HostRoot) || exists(t, reapingPath(v.HostRoot)) {
		t.Fatal("Expire reaped the recreated live volume or left the interrupted reap's tree")
	}
}
