//go:build unix

package delivery

import (
	"errors"
	"testing"

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
