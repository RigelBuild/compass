//go:build unix

package delivery

import (
	"errors"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/store"
)

// An ERRORED loss wakes only an account with owed work; a refused deliver always
// wakes. Without the gate, an agent that crashes on boot loops through wake and resume.
func TestLostSessionWakeGatedOnOwedWorkForErrored(t *testing.T) {
	const ch store.ChannelID = "chan-1"
	cases := []struct {
		name     string
		errored  bool
		seed     func(*fakeReads, store.AccountID)
		wantWake int
	}{
		{"errored, nothing owed", true, func(*fakeReads, store.AccountID) {}, 0},
		{"errored, cursor owes a message", true, func(r *fakeReads, a store.AccountID) {
			r.owed[a] = map[store.ChannelID][]store.Message{ch: {textMessage("m1", "human-1", "owed")}}
		}, 1},
		{"errored, owed mention", true, func(r *fakeReads, a store.AccountID) {
			r.seedOwedMention(a, ch, textMessage("m2", "human-1", "@agent paged"))
		}, 1},
		{"errored, owed read fails", true, func(r *fakeReads, _ store.AccountID) {
			r.undeliveredErr = errors.New("store down")
		}, 1},
		{"refused deliver, nothing owed", false, func(*fakeReads, store.AccountID) {}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _, reads := newTestConsumer(t)
			w := withWaker(c)
			const agent store.AccountID = "agent-a"
			tc.seed(reads, agent)

			c.OnSessionLost("sess-a", agent, tc.errored)
			c.drainLost(t.Context())

			if got := w.count(agent); got != tc.wantWake {
				t.Fatalf("wakes = %d, want %d", got, tc.wantWake)
			}
		})
	}
}

// A message that crashes the agent stays owed, so each ERRORED would re-wake at
// once. Repeat ERRORED wakes of one account back off; a refused deliver does not.
func TestErroredWakeBacksOffOnRepeat(t *testing.T) {
	const agent store.AccountID = "agent-a"
	c, _, _, reads := newTestConsumer(t)
	w := withWaker(c)
	reads.owed[agent] = map[store.ChannelID][]store.Message{"chan-1": {textMessage("m1", "human-1", "poison")}}
	now := time.Unix(1_000_000, 0)
	c.now = func() time.Time { return now }
	var delays []time.Duration
	var fire []func()
	c.afterFunc = func(d time.Duration, f func()) {
		delays = append(delays, d)
		fire = append(fire, f)
	}
	lose := func(errored bool) {
		c.OnSessionLost("sess-a", agent, errored)
		c.drainLost(t.Context())
	}

	lose(true)
	if got := w.count(agent); got != 1 || len(delays) != 0 {
		t.Fatalf("first errored: wakes=%d timers=%d, want 1 and 0", got, len(delays))
	}
	lose(true)
	if got := w.count(agent); got != 1 || len(delays) != 1 || delays[0] != erroredWakeBaseDelay {
		t.Fatalf("second errored: wakes=%d delays=%v, want 1 wake and one %v timer", got, delays, erroredWakeBaseDelay)
	}
	lose(true)
	if len(delays) != 1 {
		t.Fatalf("errored while a wake is pending scheduled another timer: %v", delays)
	}
	lose(false)
	if got := w.count(agent); got != 2 {
		t.Fatalf("refused deliver during backoff: wakes=%d, want 2", got)
	}
	fire[0]()
	c.drainLost(t.Context())
	if got := w.count(agent); got != 3 {
		t.Fatalf("deferred wake fired: wakes=%d, want 3", got)
	}
	lose(true)
	if len(delays) != 2 || delays[1] != 2*erroredWakeBaseDelay {
		t.Fatalf("third strike delays=%v, want second timer %v", delays, 2*erroredWakeBaseDelay)
	}
	fire[1]()
	c.drainLost(t.Context())

	now = now.Add(erroredWakeResetAfter)
	lose(true)
	if got := w.count(agent); got != 5 || len(delays) != 2 {
		t.Fatalf("errored after quiet window: wakes=%d timers=%d, want 5 and 2", got, len(delays))
	}
}

// A deferred wake re-checks owed work: if the poisoned message was delivered or
// dropped meanwhile, it does not wake, and the next ERRORED can schedule again.
func TestDeferredErroredWakeSkipsWhenNothingOwed(t *testing.T) {
	const agent store.AccountID = "agent-a"
	c, _, _, reads := newTestConsumer(t)
	w := withWaker(c)
	reads.owed[agent] = map[store.ChannelID][]store.Message{"chan-1": {textMessage("m1", "human-1", "poison")}}
	var fire []func()
	c.afterFunc = func(_ time.Duration, f func()) { fire = append(fire, f) }
	for range 2 {
		c.OnSessionLost("sess-a", agent, true)
		c.drainLost(t.Context())
	}
	delete(reads.owed, agent)
	fire[0]()
	c.drainLost(t.Context())
	if got := w.count(agent); got != 1 {
		t.Fatalf("deferred wake with nothing owed: wakes=%d, want 1", got)
	}
	reads.owed[agent] = map[store.ChannelID][]store.Message{"chan-1": {textMessage("m2", "human-1", "again")}}
	c.OnSessionLost("sess-a", agent, true)
	c.drainLost(t.Context())
	if len(fire) != 2 {
		t.Fatalf("pending flag not cleared: timers=%d, want 2", len(fire))
	}
}

// The delay doubles from the base and holds at the cap, which stays below the
// reset window so a steady crash loop cannot clear its own strikes.
func TestErroredWakeDelaySchedule(t *testing.T) {
	if erroredWakeMaxDelay >= erroredWakeResetAfter {
		t.Fatalf("cap %v must be below reset window %v", erroredWakeMaxDelay, erroredWakeResetAfter)
	}
	want := make([]time.Duration, 0, 16)
	want = append(want, 0, 30*time.Second, time.Minute, 2*time.Minute, 4*time.Minute,
		8*time.Minute, 15*time.Minute, 15*time.Minute)
	for range 8 {
		want = append(want, erroredWakeMaxDelay)
	}
	c := NewConsumer(newFakeReads(), newFakeDispatcher(), newFakeResolver(), newFakeFabric(), discardLogger())
	now := time.Unix(1_000_000, 0)
	c.now = func() time.Time { return now }
	const agent store.AccountID = "agent-a"
	for strikes, w := range want {
		if got := c.nextErroredWakeDelay(agent); got != w {
			t.Fatalf("strike %d delay = %v, want %v", strikes, got, w)
		}
		c.clearPendingErroredWake(agent)
		now = now.Add(time.Minute)
	}
}

// A loop at the cap keeps adding strikes, so the shift must never overflow into
// a non-positive delay that would wake at once.
func TestErroredWakeDelayHoldsCapAtLargeStrikes(t *testing.T) {
	for _, n := range []int{11, 40, 64, 1 << 20} {
		if got := erroredWakeDelay(n); got != erroredWakeMaxDelay {
			t.Fatalf("erroredWakeDelay(%d) = %v, want %v", n, got, erroredWakeMaxDelay)
		}
	}
}

// The recovery pass drops backoff state past its reset window but keeps an
// account with a pending deferred wake.
func TestRecoveryPrunesStaleErroredBackoff(t *testing.T) {
	c := NewConsumer(newFakeReads(), newFakeDispatcher(), newFakeResolver(), newFakeFabric(), discardLogger())
	now := time.Unix(1_000_000, 0)
	c.now = func() time.Time { return now }
	for range 2 {
		c.nextErroredWakeDelay("stale")
	}
	c.clearPendingErroredWake("stale")
	for range 2 {
		c.nextErroredWakeDelay("pending")
	}
	now = now.Add(erroredWakeResetAfter)
	c.mu.Lock()
	c.recoveryPending = true
	c.mu.Unlock()
	c.drainRecovery(t.Context())
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.erroredWakes["stale"]; ok {
		t.Fatal("stale backoff entry was not pruned")
	}
	if _, ok := c.erroredWakes["pending"]; !ok {
		t.Fatal("pending backoff entry was pruned")
	}
}
