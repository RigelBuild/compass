//go:build unix

package runnerhub

import (
	"context"
	"errors"
	"testing"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// The RunnerHub's RIG-2732 W3 forge-ack arm: the RECEIVE side of the forge
// notification lane. A forge_notification_ack frame (the agent's turn-end flush
// receipt) advances that subscription's delivered_revision through the delivery
// store, resolving session->agent from the hub's own binding exactly as
// deliverAck does. Each test pins the observable contract a plausible regression
// would break, reusing the fakeDeliveryStore in deliveryarm_test.go.

// testForgeRevision is the revision every forge-ack frame in these tests carries
// — the value the arm advances delivered_revision to. Fixed because these tests
// exercise the resolve/advance/drop contract, not revision variation.
const testForgeRevision = "rev-abc"

// forgeAckFrame wraps a ForgeNotificationAck variant carrying subscriptionID and
// the fixed testForgeRevision.
func forgeAckFrame(subscriptionID string) *compassv1internal.AgentFrame {
	return &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_ForgeNotificationAck{
			ForgeNotificationAck: &compassv1internal.ForgeNotificationAck{
				SubscriptionId: subscriptionID,
				Revision:       testForgeRevision,
			},
		},
	}
}

// The forge-ack arm resolves the acking session's agent and advances the
// subscription's delivered_revision with the acked (subscription_id, revision)
// under that agent. RED before the arm existed: the frame hits Deliver's default
// and is counted unknown, never reaching the store.
func TestForgeNotificationAckAdvancesDeliveredRevision(t *testing.T) {
	hub := newHubOnly()
	del := newFakeDeliveryStore()
	hub.SetDeliveryStore(del)
	bindSession(hub, "sess-1") // binds sess-1 -> testAgentAccount

	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1, SessionID: "sess-1", Frame: forgeAckFrame("sub-1"),
	}); err != nil {
		t.Fatalf("Deliver(forge_notification_ack) = %v, want nil (never a teardown)", err)
	}
	adv := del.forgeSnapshot()
	if len(adv) != 1 {
		t.Fatalf("forge advances = %d, want 1", len(adv))
	}
	if adv[0].agent != testAgentAccount || adv[0].subscriptionID != "sub-1" || adv[0].revision != testForgeRevision {
		t.Fatalf("advance = %+v, want {%s, sub-1, rev-abc}", adv[0], testAgentAccount)
	}
	// A clean advance is not an ack drop.
	if got := hub.DroppedAcks(); got != 0 {
		t.Fatalf("DroppedAcks = %d, want 0 on a clean advance", got)
	}
}

// An ack for a session with no bound agent is a fail-closed no-op — never a
// cursor advance under a wrong/absent account — mirroring deliverAck's unbound
// posture. The store must NOT be touched: the binding resolution precedes the
// advance, so an unbound session advances nothing and the drop is counted.
func TestForgeNotificationAckUnboundSessionIsNoOp(t *testing.T) {
	hub := newHubOnly()
	del := newFakeDeliveryStore()
	hub.SetDeliveryStore(del)

	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1, SessionID: "never-bound", Frame: forgeAckFrame("sub-1"),
	}); err != nil {
		t.Fatalf("Deliver(forge ack, unbound) = %v, want nil", err)
	}
	if got := len(del.forgeSnapshot()); got != 0 {
		t.Fatalf("forge advances = %d, want 0 (unbound session advances nothing)", got)
	}
	if got := hub.DroppedAcks(); got != 1 {
		t.Fatalf("DroppedAcks = %d, want 1 (the unbound ack is counted)", got)
	}
}

// An ack carrying no subscription id is a fail-closed no-op: nothing to advance,
// counted and dropped, never a store call (the empty-id guard precedes the
// binding resolution, mirroring deliverAck's empty-message-id guard).
func TestForgeNotificationAckEmptySubscriptionIDIsNoOp(t *testing.T) {
	hub := newHubOnly()
	del := newFakeDeliveryStore()
	hub.SetDeliveryStore(del)
	bindSession(hub, "sess-1")

	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1, SessionID: "sess-1", Frame: forgeAckFrame(""),
	}); err != nil {
		t.Fatalf("Deliver(forge ack, empty sub id) = %v, want nil", err)
	}
	if got := len(del.forgeSnapshot()); got != 0 {
		t.Fatalf("forge advances = %d, want 0 (an empty subscription id advances nothing)", got)
	}
	if got := hub.DroppedAcks(); got != 1 {
		t.Fatalf("DroppedAcks = %d, want 1 (the empty-id ack is counted)", got)
	}
}

// A store fault (or the ErrNotFound of a subscription unsubscribed mid-flight /
// owned by a different agent) advancing the cursor is a non-fatal drop — a
// missed advance costs a redundant re-notify on the next reconciliation sweep,
// never a teardown — and is counted. This is the fail-closed ownership seam: the
// store scopes the UPDATE to (id, agent_account_id), so a foreign subscription
// returns zero rows -> ErrNotFound here.
func TestForgeNotificationAckStoreFaultIsNonFatalAndCounted(t *testing.T) {
	hub := newHubOnly()
	del := newFakeDeliveryStore()
	del.forgeErr = errors.New("subscription not found for this agent")
	hub.SetDeliveryStore(del)
	bindSession(hub, "sess-1")

	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1, SessionID: "sess-1", Frame: forgeAckFrame("sub-foreign"),
	}); err != nil {
		t.Fatalf("Deliver(forge ack, store fault) = %v, want nil (non-fatal drop, not a teardown)", err)
	}
	if got := hub.DroppedAcks(); got != 1 {
		t.Fatalf("DroppedAcks = %d, want 1 (the advance fault is counted)", got)
	}
}

// A nil delivery store (a Deliver-only hub) drops the forge ack silently: no
// cursor exists to advance, so it is not even counted — the exact nil-store
// posture deliverAck takes. No SetDeliveryStore call here.
func TestForgeNotificationAckNilStoreIsSilentNoOp(t *testing.T) {
	hub := newHubOnly() // no delivery store wired
	bindSession(hub, "sess-1")

	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1, SessionID: "sess-1", Frame: forgeAckFrame("sub-1"),
	}); err != nil {
		t.Fatalf("Deliver(forge ack, nil store) = %v, want nil", err)
	}
	if got := hub.DroppedAcks(); got != 0 {
		t.Fatalf("DroppedAcks = %d, want 0 (a nil store drops silently, uncounted)", got)
	}
}
