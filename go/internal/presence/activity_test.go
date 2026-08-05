//go:build unix

package presence

// PresenceFor + PublishActivity (SEA-1721 T2), driven through the publisher's
// real bus + fakes: the enum snapshot projects the last-published subset, and
// PublishActivity emits an AgentPresenceChanged carrying the CURRENT presence
// plus the activity string on the AgentPresenceChanged.activity field.
// context.Background() is the test root (test-root ctx exemption).

import (
	"testing"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// TestPresenceForProjectsLastPublishedSubset: PresenceFor returns only the
// requested agents that have a published presence; an agent never published is
// omitted (the hub defaults it to OFFLINE).
func TestPresenceForProjectsLastPublishedSubset(t *testing.T) {
	p, _, _ := newTestPublisherWithStatus(t, newFakeStatus())
	p.publishIfChanged("a-1", compassv1.AgentPresence_AGENT_PRESENCE_WORKING)
	p.publishIfChanged("a-2", compassv1.AgentPresence_AGENT_PRESENCE_IDLE)

	got := p.PresenceFor([]store.AccountID{"a-1", "a-never"})
	if len(got) != 1 {
		t.Fatalf("PresenceFor returned %d entries, want 1 (a-never omitted)", len(got))
	}
	if got["a-1"] != compassv1.AgentPresence_AGENT_PRESENCE_WORKING {
		t.Errorf("a-1 = %v, want WORKING", got["a-1"])
	}
	if _, ok := got["a-never"]; ok {
		t.Errorf("a-never present, want omitted (never published)")
	}
}

// TestPublishActivityCarriesActivityAndCurrentPresence: PublishActivity emits an
// AgentPresenceChanged carrying the agent's current presence enum AND the
// activity string on the AgentPresenceChanged.activity field. Read straight off
// the bus so the activity field itself is asserted (the recorder tracks only
// presence).
func TestPublishActivityCarriesActivityAndCurrentPresence(t *testing.T) {
	p, _, _, bus := newTestPublisher(t)
	// Seed a current presence so PublishActivity carries a non-OFFLINE enum.
	p.publishIfChanged("a-1", compassv1.AgentPresence_AGENT_PRESENCE_WORKING)

	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	p.PublishActivity("a-1", "reviewing the diff")

	for ev := range sub.Live {
		pc := ev.Payload.GetAgentPresenceChanged()
		if pc == nil || pc.GetAgentAccountId() != "a-1" {
			continue
		}
		if pc.GetActivity() != "reviewing the diff" {
			t.Fatalf("published activity field = %q, want %q", pc.GetActivity(), "reviewing the diff")
		}
		if pc.GetPresence() != compassv1.AgentPresence_AGENT_PRESENCE_WORKING {
			t.Fatalf("published presence = %v, want WORKING (the current enum)", pc.GetPresence())
		}
		return
	}
	t.Fatal("no AgentPresenceChanged for a-1 observed on the bus")
}

// TestPublishActivityAbsentPresenceIsOffline: an agent with no published presence
// publishes OFFLINE alongside the activity — the absent-from-map posture.
func TestPublishActivityAbsentPresenceIsOffline(t *testing.T) {
	p, _, _, bus := newTestPublisher(t)
	rec := startRecorder(t, bus)

	p.PublishActivity("a-fresh", "just starting")
	rec.waitForPublishes(t, 1)

	last := rec.snapshot()[0]
	if last.account != "a-fresh" || last.presence != compassv1.AgentPresence_AGENT_PRESENCE_OFFLINE {
		t.Fatalf("publish = %+v, want {a-fresh, OFFLINE}", last)
	}
}
