//go:build unix

package runnerhub

// PresenceFor returns the in-memory enum, defaults missing agents to OFFLINE, and publishes status changes when wired.
// Tests use a fake presence source.

import (
	"context"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// fakePresenceSourceHub is a hand-written presenceSource: a seeded enum map plus
// a recorded PublishActivity call, so a test asserts the hub reads the map and
// forwards the publish verbatim.
type fakePresenceSourceHub struct {
	presence  map[store.AccountID]compassv1.AgentPresence
	published []publishedActivity
}

type publishedActivity struct {
	account  store.AccountID
	activity string
}

func (f *fakePresenceSourceHub) PresenceFor(accountIDs []store.AccountID) map[store.AccountID]compassv1.AgentPresence {
	out := make(map[store.AccountID]compassv1.AgentPresence, len(accountIDs))
	for _, id := range accountIDs {
		if p, ok := f.presence[id]; ok {
			out[id] = p
		}
	}
	return out
}

func (f *fakePresenceSourceHub) PublishActivity(_ context.Context, account store.AccountID, activity string) {
	f.published = append(f.published, publishedActivity{account: account, activity: activity})
}

// TestHubPresenceForDefaultsAbsentToOffline: an agent in the source map reports
// its enum; an agent absent from it defaults to OFFLINE.
func TestHubPresenceForDefaultsAbsentToOffline(t *testing.T) {
	hub := newHubOnly()
	hub.SetPresenceSource(&fakePresenceSourceHub{presence: map[store.AccountID]compassv1.AgentPresence{
		"a-working": compassv1.AgentPresence_AGENT_PRESENCE_WORKING,
	}})

	got := hub.PresenceFor([]store.AccountID{"a-working", "a-absent"})
	if p := got["a-working"].Presence; p != compassv1.AgentPresence_AGENT_PRESENCE_WORKING {
		t.Errorf("a-working presence = %v, want WORKING", p)
	}
	if p := got["a-absent"].Presence; p != compassv1.AgentPresence_AGENT_PRESENCE_OFFLINE {
		t.Errorf("a-absent presence = %v, want OFFLINE default", p)
	}
}

// TestHubPresenceForNilSourceIsAllOffline: a hub with no presence source wired
// reports every requested agent OFFLINE — the un-wired / hub-less-test posture.
func TestHubPresenceForNilSourceIsAllOffline(t *testing.T) {
	hub := newHubOnly()
	got := hub.PresenceFor([]store.AccountID{"x", "y"})
	for _, id := range []store.AccountID{"x", "y"} {
		if p := got[id].Presence; p != compassv1.AgentPresence_AGENT_PRESENCE_OFFLINE {
			t.Errorf("%s presence = %v, want OFFLINE (no source wired)", id, p)
		}
	}
}

// TestHubPublishActivityForwardsToSource: PublishActivity forwards the account +
// activity to the wired source verbatim.
func TestHubPublishActivityForwardsToSource(t *testing.T) {
	hub := newHubOnly()
	src := &fakePresenceSourceHub{presence: map[store.AccountID]compassv1.AgentPresence{}}
	hub.SetPresenceSource(src)

	hub.PublishActivity(t.Context(), "acct-a", "building the world")

	if len(src.published) != 1 {
		t.Fatalf("published count = %d, want 1", len(src.published))
	}
	if src.published[0] != (publishedActivity{account: "acct-a", activity: "building the world"}) {
		t.Errorf("published = %+v, want {acct-a, building the world}", src.published[0])
	}
}

// TestHubPublishActivityNilSourceIsSafe: a hub with no source wired drops the
// publish without panicking (best-effort; the durable table is the record).
func TestHubPublishActivityNilSourceIsSafe(t *testing.T) {
	hub := newHubOnly()
	hub.PublishActivity(t.Context(), "acct-a", "no source wired") // must not panic
}
