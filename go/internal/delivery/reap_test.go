//go:build unix

package delivery

// RIG-1569 T3 — OnSessionsReaped (the hub's SessionReapSink) drops the
// held-deliver registry entries for sessions whose hub bindings were cleared at
// a Runner (re-)enroll, so a no-frame author death's entry does not leak until
// process restart (design.md:172-175). White-box (package delivery) so the test
// drives c.hold and reads c.held via isHeld directly. Sleep-free: the reap is a
// synchronous in-memory delete, so the assertion reads a recorded fact.

import "testing"

// OnSessionsReaped drops exactly the held entries for the reaped session ids and
// leaves a DIFFERENT author's held entry untouched — the enroll-bounded reap the
// design promises.
func TestOnSessionsReapedDropsHeldEntries(t *testing.T) {
	c, _, _, _ := newTestConsumer(t) //nolint:dogsled // this test needs only the consumer; the fakes (dispatcher/resolver/reads) are unused here — the reap is a pure in-memory delete with no dispatch/resolve/read path.

	// Two authors hold pending delivers; a no-frame death would strand both.
	c.hold("sess-dead", "m1", "")
	c.hold("sess-dead", "m2", "")
	c.hold("sess-live", "m3", "")

	if !c.isHeld("sess-dead", "m1") || !c.isHeld("sess-dead", "m2") {
		t.Fatal("precondition: sess-dead should hold m1 and m2")
	}
	if !c.isHeld("sess-live", "m3") {
		t.Fatal("precondition: sess-live should hold m3")
	}

	// The Runner re-enrolls; the hub clears sess-dead's binding and fires the
	// reap edge for it (not sess-live, which stays bound in this scenario).
	c.OnSessionsReaped([]string{"sess-dead"})

	if c.isHeld("sess-dead", "m1") || c.isHeld("sess-dead", "m2") {
		t.Fatal("sess-dead held entries survived the reap, want dropped")
	}
	if !c.isHeld("sess-live", "m3") {
		t.Fatal("sess-live held entry was dropped by the reap, want it to survive (only reaped ids drop)")
	}
}

// OnSessionsReaped with an empty slice (a first-ever enroll clears nothing) is a
// no-op that touches no held entry.
func TestOnSessionsReapedEmptyIsNoop(t *testing.T) {
	c, _, _, _ := newTestConsumer(t) //nolint:dogsled // this test needs only the consumer; the fakes are unused — an empty-slice reap touches no dispatch/resolve/read path.
	c.hold("sess-a", "m1", "")

	c.OnSessionsReaped(nil)

	if !c.isHeld("sess-a", "m1") {
		t.Fatal("empty reap dropped a held entry, want no-op")
	}
}

// OnSessionsReaped of a session id with NO held entry (a session that died with
// its held queue already drained, or never held anything) is a safe no-op that
// leaves every unrelated held entry intact — delete of an absent map key.
func TestOnSessionsReapedAbsentIDIsNoop(t *testing.T) {
	c, _, _, _ := newTestConsumer(t) //nolint:dogsled // this test needs only the consumer; the fakes are unused — reaping an absent id touches no dispatch/resolve/read path.
	c.hold("sess-live", "m1", "")

	c.OnSessionsReaped([]string{"sess-never-held"})

	if !c.isHeld("sess-live", "m1") {
		t.Fatal("reap of an absent id dropped an unrelated held entry, want it intact")
	}
}
