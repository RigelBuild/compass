package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
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

// TestLookupRoundTripsAndTypesNotFound pins the resolve half of the provision
// path's resolve-or-create: a created volume round-trips with its HostRoot, and
// an absent session is an errors.Is-detectable ErrVolumeNotFound — an
// error-shaped signal the provision path turns into a cold create, never a
// silent recreate here.
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

// TestVolumeRootRejectsTraversal guards the one operation that deletes a
// subtree: a session id carrying a separator or a traversal element must never
// resolve to a path outside the base dir, and a session id ENDING in
// lockFileSuffix must never resolve at all — its volume root would BE another
// session's sibling lock-file path, so a reap of one would target the other's
// lock. Each case asserts the typed ErrInvalidSessionID rather than merely a
// non-nil error, so a rejection for an unrelated reason cannot pass for the
// guard.
func TestVolumeRootRejectsTraversal(t *testing.T) {
	m := newManager(t)
	badIDs := []string{
		"", "..", ".", "../escape", "a/b", "sess/../../etc",
		// The lock-namespace collision cases: without volumeRoot's
		// HasSuffix check these resolve to a path in the lock namespace.
		"sess-x" + lockFileSuffix,
		lockFileSuffix,
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

// TestMountedRootIsWritableByCreatingUser asserts the keep-id ownership
// invariant's observable half: the volume root is created by the invoking host
// user and is writable by it. Under the container's keep-id remap that user maps
// to the agent uid in-container, which is what satisfies ensureCheckoutDir's
// "CheckoutDir's parent must be writable by the agent uid" precondition
// (go/internal/runtime/agent.go:354-357). A base dir or subtree created with a
// mode the creating user cannot write would break every launch on the volume
// path.
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

// TestAttachClearsAPastDeadlineStamp is invariant (a): a reopened
// closed-but-unexpired session must not carry a past-deadline stamp into its
// new life. The observable contract is that the volume SURVIVES an Expire that
// would have reaped it before the Attach — a backend that returned the path
// without clearing the stamp would lose a live session's tree on the very next
// reaper pass.
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

// TestExpireReapsOnlyClosedPastDeadline is the reaper's whole eligibility
// contract in one table: five volumes covering every state a volume can be in,
// with exactly one eligible. Each surviving row fails on a distinct plausible
// bug — treating unstamped as closed, ignoring the intent bit, ignoring the
// retention window, or reading a stale stamp across an Attach.
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

// TestReconcileOrphansStampsUnstampedVolumesAtDiscovery is the crash-
// reconciliation contract. A crash between container-remove and stamp-write
// leaves an unstamped closed volume that Expire — which reads unstamped as
// live — could never reach. The startup pass stamps it closed at DISCOVERY
// time, so it survives one full retention window from discovery (fails safe,
// never open), invariant (a) still undoes the discovery stamp if the session is
// re-provisioned first, and it is reaped once the discovery deadline passes.
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

	lock, err := lockVolume(v.HostRoot, false)
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

// TestReaperIgnoresNonVolumeDirs pins volume identity as STRUCTURAL: only a
// directory carrying the metaDirName marker CreateVolume writes is one of this
// package's volumes. A directory under the base dir without that marker belongs
// to someone else and must never be stamped or reaped.
//
// This is not hypothetical. The frozen record places W2's snapshot store as a
// sibling subtree under the same base dir keyed by VolumeSnapshotID — a
// non-volume directory in exactly this position. A scan that treated every
// directory entry as a volume root would have ReconcileOrphans stamp that store
// closed at discovery and Expire silently delete it one retention window later:
// a W1-armed landmine that detonates when W2 lands.
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
	// Never stamped: the pass must not have created a metadata dir inside it.
	if exists(t, filepath.Join(stray, metaDirName)) {
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

// TestReconcileOrphansDoesNotResurrectAReapedRoot pins onto the third mutator
// the guard its two siblings (Attach, Stamp) already carry: a volume reaped out
// from under a reconcile pass — between eachVolume's marker stat and this pass's
// lock acquisition — must NOT be re-stamped. Without the guard, writeStamp's
// os.MkdirAll resurrects the reaped root's shell, Lookup then SUCCEEDS on a dead
// session, and the provision path warm-Attaches an EMPTY volume instead of
// cold-materializing — silently defeating the not-found-is-an-observable-signal
// contract Lookup documents.
//
// The reap is applied directly and stampOrphanLocked driven on the absent root:
// a fully-removed root is no longer a base-dir entry eachVolume would visit, so
// the window the guard closes is only reachable by running the mutator against a
// root that vanished after discovery — which is exactly the deterministic form
// of that race.
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

	lock, err := lockVolume(held.HostRoot, false)
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

// heldFlockField is the `MAJ:MIN:INO` field string the kernel prints for the
// held per-volume lock, read out of /proc/locks itself rather than recomputed
// from a path formula or reassembled from device-number math.
//
// Reading the kernel's own rendering is what makes the gate below an exact
// match. The held lock appears in /proc/locks as its own (non-`->`) FLOCK row
// carrying the very field string its blocked waiters' rows carry, so comparing
// that string end to end compares device AND inode without this test ever
// having to know how the kernel formats a dev_t. The row is identified by the
// inode off the lock's own fd — whatever path the implementation chose for
// it — plus this process's pid, which together cannot match another process's
// unrelated lock.
func heldFlockField(t *testing.T, l *volumeLock) string {
	t.Helper()
	info, err := l.f.Stat()
	if err != nil {
		t.Fatalf("stat held volume lock: %v", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skipf("no syscall.Stat_t for the lock file on this platform (%T); the race gate needs the lock's inode", info.Sys())
	}
	inoSuffix := ":" + strconv.FormatUint(st.Ino, 10)
	pid := strconv.Itoa(os.Getpid())

	data, err := os.ReadFile("/proc/locks")
	if err != nil {
		t.Skipf("cannot read /proc/locks (%v); the race gate needs the kernel's blocked-waiter record", err)
	}
	// A held row: `301: FLOCK  ADVISORY  WRITE 3173818 00:3c:775 0 EOF` — the
	// MAJ:MIN:INO field is index 5, the owning pid index 4.
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[1] != "FLOCK" || fields[4] != pid {
			continue
		}
		if strings.HasSuffix(fields[5], inoSuffix) {
			return fields[5]
		}
	}
	t.Fatalf("no held FLOCK row in /proc/locks for pid %s inode %s; the race gate cannot identify the lock it must wait on", pid, inoSuffix[1:])
	return ""
}

// waitForBlockedFlock blocks until the kernel reports a process WAITING on the
// flock identified by field (a `MAJ:MIN:INO` string from heldFlockField), and
// fails the test if that never happens.
//
// This is the event gate that makes TestAttachRacingAReapDoesNotReturnAReaped-
// Path deterministic instead of timing-dependent. A blocked flock has no
// user-space completion signal — the waiter is parked inside the syscall, so
// there is no channel to receive and no WaitGroup to wait — but Linux publishes
// the blocked waiter itself in /proc/locks as a `-> FLOCK` continuation row on
// the lock being contended. Polling until that row appears waits on the actual
// event ("the Attach goroutine is now parked on the flock"), so the test drives
// the exact blocked-then-reap interleaving every run rather than hoping a sleep
// was long enough; a genuine failure to reach that state fails loudly here
// instead of silently degrading into the other interleaving.
//
// The tick between polls is the bounded-poll tick, not a timing assumption:
// the loop re-reads the condition every iteration and still fails loudly at the
// deadline, so nothing here depends on a duration being "long enough". It only
// keeps this from being a core-burning spin whose own contention can delay the
// very scheduling it is waiting for under -race.
func waitForBlockedFlock(t *testing.T, field string) {
	t.Helper()
	const tick = 500 * time.Microsecond
	if _, err := os.ReadFile("/proc/locks"); err != nil {
		t.Skipf("cannot read /proc/locks (%v); the race gate needs the kernel's blocked-waiter record", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		data, err := os.ReadFile("/proc/locks")
		if err != nil {
			t.Fatalf("reading /proc/locks: %v", err)
		}
		if hasBlockedFlockWaiter(string(data), field) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no process ever blocked on an flock over %s; the race gate never engaged", field)
		}
		time.Sleep(tick) //nolint:forbidigo // irreducible poll-tick: a blocked flock is parked inside the syscall with NO user-space completion signal (no channel, no WaitGroup), so /proc/locks polling is the only observation of the event; the condition is re-read every iteration and the deadline fails loudly, so the tick paces the poll and is never itself the thing waited on
	}
}

// hasBlockedFlockWaiter reports whether /proc/locks content carries a blocked
// flock waiter for the lock whose kernel-printed MAJ:MIN:INO field is field. A
// waiter row is the `-> ` continuation of the lock it is queued behind, e.g.
//
//	292: -> FLOCK  ADVISORY  WRITE 1759984 00:20:507753071 0 EOF
//
// The comparison is over the WHOLE MAJ:MIN:INO field, never the inode alone:
// dropping MAJ:MIN would match an unrelated flock waiter on another device
// whose inode number happens to be equal, and a spurious early match here
// would release the reap before the Attach goroutine has parked — silently
// degrading this load-bearing gate into the other (still-passing) interleaving,
// a false green exactly as the test it gates promises it will not produce.
func hasBlockedFlockWaiter(locks, field string) bool {
	for line := range strings.SplitSeq(locks, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 || fields[1] != "->" || fields[2] != "FLOCK" {
			continue
		}
		if fields[6] == field {
			return true
		}
	}
	return false
}

// TestAttachRacingAReapDoesNotReturnAReapedPath is the regression test for the
// TOCTOU race between a relaunch and the reaper tick. A session that relaunches
// exactly at its expiry deadline runs Attach's pre-check (root present), then
// blocks on the per-volume lock the reaper holds; the reaper re-verifies
// eligibility under that lock, reaps the tree, and releases. The Attach that
// then wins the lock MUST NOT report success with the path of a directory that
// no longer exists — the provision path would mount a dead path and silently
// lose the warm `target/`/sccache tree with nothing signalling the loss. It
// must return ErrVolumeNotFound so the provision path cold-materializes.
//
// The interleaving is driven deterministically: the stand-in reaper takes the
// lock first, and the reap happens only once the kernel reports the Attach
// goroutine actually parked on that lock (see waitForBlockedFlock). Both
// interleavings are asserted correct, so the gate decides which bug this
// exercises, never whether correct code passes.
func TestAttachRacingAReapDoesNotReturnAReapedPath(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-relaunch-at-deadline")

	// The sentinel stands in for the warm derived state the whole volume
	// mechanism exists to preserve: if Attach hands back a path without this
	// file, the caller mounted a reaped tree.
	sentinel := filepath.Join(v.HostRoot, "target-artifact")
	if err := os.WriteFile(sentinel, []byte("warm build cache"), 0o600); err != nil {
		t.Fatalf("writing the warm-tree sentinel: %v", err)
	}
	stampAged(t, v, IntentClosed, 30*24*time.Hour)

	// Stand in for the reaper mid-pass: Expire holds exactly this lock across
	// its under-lock re-verify and its os.RemoveAll.
	reaper, err := lockVolume(v.HostRoot, false)
	if err != nil {
		t.Fatalf("acquiring the stand-in reaper lock: %v", err)
	}
	if reaper == nil {
		t.Fatal("stand-in reaper lock reported contention on a fresh volume")
	}
	lockField := heldFlockField(t, reaper)

	type attachResult struct {
		path string
		err  error
	}
	done := make(chan attachResult, 1)
	go func() {
		path, err := m.Attach(t.Context(), v)
		done <- attachResult{path: path, err: err}
	}()

	// Gate on the kernel's own record that the Attach goroutine is parked on
	// the lock, so the reap below lands in the window the bug lives in.
	waitForBlockedFlock(t, lockField)

	if err := os.RemoveAll(v.HostRoot); err != nil {
		t.Fatalf("reaping the volume root: %v", err)
	}
	if err := reaper.release(); err != nil {
		t.Fatalf("releasing the stand-in reaper lock: %v", err)
	}

	got := <-done
	switch {
	case errors.Is(got.err, ErrVolumeNotFound):
		// Correct: the reap is reported as an error-shaped signal the provision
		// path turns into a cold CreateVolume plus materialize.
	case got.err != nil:
		t.Fatalf("Attach racing a reap = %v, want ErrVolumeNotFound", got.err)
	case !exists(t, filepath.Join(got.path, "target-artifact")):
		t.Fatalf("Attach returned %q with a nil error, but the volume was reaped: the caller would mount a dead path and lose the warm tree with nothing signalling it", got.path)
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

	held, err := lockVolume(v.HostRoot, false)
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
	second, err := lockVolume(v.HostRoot, false)
	if err != nil {
		t.Fatalf("second lockVolume after the reap: %v", err)
	}
	if second != nil {
		if releaseErr := second.release(); releaseErr != nil {
			t.Errorf("releasing the unexpectedly-acquired second lock: %v", releaseErr)
		}
		t.Error("a second lockVolume acquired the lock while it was still held across a reap: the lock file was destroyed with the volume root, so mutual exclusion is broken across reap+recreate")
	}

	// And a recreate does not split the lock either: the recreated volume's
	// lock is the same inode, so it is still contended.
	if _, err := m.CreateVolume(t.Context(), v.SessionID); err != nil {
		t.Fatalf("recreating the volume: %v", err)
	}
	afterRecreate, err := lockVolume(v.HostRoot, false)
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

// TestVolumeLockFileIsOutsideTheVolumeRoot pins the placement structurally, and
// pins that acquiring a lock creates NOTHING inside the volume root. A
// lockVolume that resurrected the metadata dir inside a reaped root would make
// Attach's under-lock existence check see a live volume with no contents.
func TestVolumeLockFileIsOutsideTheVolumeRoot(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-lock-placement")
	if err := os.RemoveAll(v.HostRoot); err != nil {
		t.Fatalf("reaping the volume root: %v", err)
	}

	lock, err := lockVolume(v.HostRoot, false)
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

	// The stamp, by contrast, stays INSIDE the volume root (the frozen record
	// places it there), and eachVolume iterates directories only, so the
	// sibling lock file is never mistaken for a volume.
	live := mustCreate(t, m, "sess-stamped")
	stampAged(t, live, IntentClosed, 30*24*time.Hour)
	if !strings.HasPrefix(stampPath(live.HostRoot), live.HostRoot+string(os.PathSeparator)) {
		t.Errorf("stamp path %q is not inside the volume root %q", stampPath(live.HostRoot), live.HostRoot)
	}
	// Materialize the sibling lock file, then release it: a still-held lock
	// would make Expire SKIP the volume, which would pass this check for the
	// wrong reason.
	siblingLock, err := lockVolume(live.HostRoot, false)
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
}
