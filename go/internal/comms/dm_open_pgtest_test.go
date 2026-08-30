//go:build pgtest

package comms

// The Comms.OpenDM handler + OpenDMAsAccount adapter (RIG-2962 T3, design.md
// T3:745-773): resolve-or-create the two-party peer DM addressed by handle, with
// same-owner authz, the deterministic sorted-handle name, and the post-commit
// ChannelChanged emit on create. Driven in-process via connect.NewRequest +
// WithActor (and the AsAccount adapter) against a real store and bus, mirroring
// comms_test.go / org_mgmt_pgtest_test.go. context.Background() is the test root
// (test-root ctx exemption).

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

// TestOpenDMSameOwnerCreatesDMChannel: an agent opens a DM with a same-owner
// peer → the returned channel is a real DM (kind=DM, mandatory subscription,
// both parties members), created=true, and the deterministic sorted-handle name
// (dm:<lo>:<hi>) is used.
func TestOpenDMSameOwnerCreatesDMChannel(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	alice := mustAgent(t, st, owner.ID, "alice")
	bob := mustAgent(t, st, owner.ID, "bob")

	resp, err := svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "bob"}))
	if err != nil {
		t.Fatalf("OpenDM = %v, want success", err)
	}
	if !resp.Msg.GetCreated() {
		t.Fatalf("created = false, want true on a first open")
	}
	ch := resp.Msg.GetChannel()
	if ch.GetName() != "dm:alice:bob" {
		t.Fatalf("channel name = %q, want dm:alice:bob (deterministic sorted-handle)", ch.GetName())
	}
	if ch.GetKind() != compassv1.ChannelKind_CHANNEL_KIND_DM {
		t.Fatalf("channel kind = %v, want CHANNEL_KIND_DM", ch.GetKind())
	}
	if !ch.GetMandatorySubscription() {
		t.Fatalf("mandatory_subscription = false, want true (born-mandatory DM)")
	}
	if !containsString(ch.GetMemberAccountIds(), string(alice.ID)) || !containsString(ch.GetMemberAccountIds(), string(bob.ID)) {
		t.Fatalf("members = %v, want both alice %s and bob %s", ch.GetMemberAccountIds(), alice.ID, bob.ID)
	}
}

// TestOpenDMReopenResumesSameChannel: a second open of the same pair — in EITHER
// handle order — resumes the SAME channel (created=false, same id), proving the
// deterministic name is order-independent and the upsert resolves the existing row.
func TestOpenDMReopenResumesSameChannel(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	alice := mustAgent(t, st, owner.ID, "alice")
	bob := mustAgent(t, st, owner.ID, "bob")

	first, err := svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "bob"}))
	if err != nil {
		t.Fatalf("OpenDM(alice->bob) = %v, want success", err)
	}
	if !first.Msg.GetCreated() {
		t.Fatalf("first open created = false, want true")
	}

	// Reverse order (bob opens with alice): same deterministic name, so it must
	// resume the same channel, not create a second.
	second, err := svc.OpenDM(WithActor(ctx, bob.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "alice"}))
	if err != nil {
		t.Fatalf("OpenDM(bob->alice) = %v, want success", err)
	}
	if second.Msg.GetCreated() {
		t.Fatalf("reopen created = true, want false (resume)")
	}
	if second.Msg.GetChannel().GetId() != first.Msg.GetChannel().GetId() {
		t.Fatalf("reopen channel id = %q, want the first open's %q", second.Msg.GetChannel().GetId(), first.Msg.GetChannel().GetId())
	}
}

// TestOpenDMUnknownHandleIsNotFound: an unknown peer handle collapses to
// CodeNotFound naming the submitted handle — the oracle-safe resolve miss.
func TestOpenDMUnknownHandleIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	alice := mustAgent(t, st, owner.ID, "alice")

	_, err := svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "ghost"}))
	connectCodeIs(t, err, connect.CodeNotFound, "OpenDM(unknown handle)")
}

// TestOpenDMCrossOwnerIsIndistinguishableNotFound: an owner-qualified handle
// naming ANOTHER owner's agent collapses to the SAME CodeNotFound an unknown
// handle gets — the cross-owner authz is byte-identical to unknown, so a foreign
// peer's existence is never leaked (design.md T3:746-748).
func TestOpenDMCrossOwnerIsIndistinguishableNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	alice := mustAgent(t, st, owner.ID, "alice")
	other := mustUser(t, st, "other")
	mustAgent(t, st, other.ID, "foreign")

	// alice names other's agent by an owner-qualified handle: it resolves, but
	// the same-owner check remaps it to NOT_FOUND naming the submitted handle.
	_, err := svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "other/foreign"}))
	connectCodeIs(t, err, connect.CodeNotFound, "OpenDM(cross-owner owner-qualified handle)")
}

// TestOpenDMSelfIsInvalidArgument: a handle that resolves to the caller itself is
// not a peer — CodeInvalidArgument.
func TestOpenDMSelfIsInvalidArgument(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	alice := mustAgent(t, st, owner.ID, "alice")

	_, err := svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "alice"}))
	connectCodeIs(t, err, connect.CodeInvalidArgument, "OpenDM(self handle)")
}

// TestOpenDMAsAccountEmptyAccountIsNoActor: an empty account short-circuits to
// errNoActor (CodeInvalidArgument) before any handler work — the fail-closed
// guard mirroring the other AsAccount adapters.
func TestOpenDMAsAccountEmptyAccountIsNoActor(t *testing.T) {
	svc, _ := newHandler(t)
	_, err := svc.OpenDMAsAccount(context.Background(), "", &compassv1.OpenDMRequest{PeerHandle: "bob"})
	connectCodeIs(t, err, connect.CodeInvalidArgument, "OpenDMAsAccount(empty account)")
}

// TestOpenDMEmitsChannelChangedOnCreate: a create fans a post-commit
// ChannelChanged carrying the new DM channel to a member's stream; a resume emits
// nothing new. Driven over the real stream (newStreamHarness), subscribing before
// the mutation. The caller (alice) is a member of the DM, so it drains the event.
func TestOpenDMEmitsChannelChangedOnCreate(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()
	owner := mustUser(t, h.store, "owner")
	alice := mustAgent(t, h.store, owner.ID, "alice")
	mustAgent(t, h.store, owner.ID, "bob")

	events := firstEventAfterBoundary(t, h, alice.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})

	resp, err := h.svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "bob"}))
	if err != nil {
		t.Fatalf("OpenDM: %v", err)
	}
	wantID := resp.Msg.GetChannel().GetId()

	got := awaitFirst(t, events)
	cc := got.GetChannelChanged()
	if cc == nil {
		t.Fatalf("event payload = %T, want ChannelChanged", got.GetPayload())
	}
	if cc.GetChannel().GetId() != wantID {
		t.Fatalf("ChannelChanged id = %q, want the created DM %q", cc.GetChannel().GetId(), wantID)
	}
	if cc.GetChannel().GetKind() != compassv1.ChannelKind_CHANNEL_KIND_DM {
		t.Fatalf("ChannelChanged kind = %v, want CHANNEL_KIND_DM", cc.GetChannel().GetKind())
	}
}

// TestOpenDMAsAccountResolvesOrCreates: the agent-tool adapter runs the same
// handler path under the bound account — a first call creates, a second resumes
// the same channel — parity with the direct handler.
func TestOpenDMAsAccountResolvesOrCreates(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	alice := mustAgent(t, st, owner.ID, "alice")
	mustAgent(t, st, owner.ID, "bob")

	first, err := svc.OpenDMAsAccount(ctx, alice.ID, &compassv1.OpenDMRequest{PeerHandle: "bob"})
	if err != nil {
		t.Fatalf("OpenDMAsAccount(first) = %v, want success", err)
	}
	if !first.GetCreated() {
		t.Fatalf("first created = false, want true")
	}
	second, err := svc.OpenDMAsAccount(ctx, alice.ID, &compassv1.OpenDMRequest{PeerHandle: "bob"})
	if err != nil {
		t.Fatalf("OpenDMAsAccount(second) = %v, want success", err)
	}
	if second.GetCreated() {
		t.Fatalf("second created = true, want false (resume)")
	}
	if second.GetChannel().GetId() != first.GetChannel().GetId() {
		t.Fatalf("resume id = %q, want first %q", second.GetChannel().GetId(), first.GetChannel().GetId())
	}
}

// TestOpenDMDoubleHyphenHandlesResolveDistinctChannels pins the name-injectivity
// fix: with a `-`-delimited name, pair {a, b--c} and pair {a--b, c} would both
// derive the byte-identical `dm--a--b--c` and the second open would RESUME onto
// the first's channel, cross-adding members (a same-owner private-DM
// confidentiality break). The `:` separator is excluded from the handle grammar,
// so the two pairs map to distinct names (dm:a:b--c vs dm:a--b:c) and must
// resolve to DISTINCT channels. RED if dmChannelName reverts to a `-` delimiter.
func TestOpenDMDoubleHyphenHandlesResolveDistinctChannels(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	a := mustAgent(t, st, owner.ID, "a")
	bc := mustAgent(t, st, owner.ID, "b--c")
	ab := mustAgent(t, st, owner.ID, "a--b")
	c := mustAgent(t, st, owner.ID, "c")

	// Pair {a, b--c}.
	first, err := svc.OpenDM(WithActor(ctx, a.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "b--c"}))
	if err != nil {
		t.Fatalf("OpenDM(a->b--c) = %v, want success", err)
	}
	// Pair {a--b, c} — a DIFFERENT unordered pair. Must mint its OWN channel, not
	// resume the first pair's.
	second, err := svc.OpenDM(WithActor(ctx, ab.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "c"}))
	if err != nil {
		t.Fatalf("OpenDM(a--b->c) = %v, want success", err)
	}
	if !second.Msg.GetCreated() {
		t.Fatalf("second pair created = false, want true (a distinct pair must mint its own DM, not resume)")
	}
	if second.Msg.GetChannel().GetId() == first.Msg.GetChannel().GetId() {
		t.Fatalf("the two distinct pairs collided onto one channel %q — dmChannelName is not injective", first.Msg.GetChannel().GetId())
	}
	// And the first DM's membership is uncorrupted: exactly a + b--c (+ their
	// owner), never the second pair's ab or c.
	m := first.Msg.GetChannel().GetMemberAccountIds()
	if !containsString(m, string(a.ID)) || !containsString(m, string(bc.ID)) {
		t.Fatalf("first DM members = %v, want its own pair a=%s and b--c=%s", m, a.ID, bc.ID)
	}
	if containsString(m, string(ab.ID)) || containsString(m, string(c.ID)) {
		t.Fatalf("first DM members = %v, leaked the second pair's parties (ab=%s c=%s)", m, ab.ID, c.ID)
	}
}

// TestOpenDMResumeEmitsNoChannelChanged pins the paired half of the emit
// contract (emitDMCreated's created-only guard): a create fans a ChannelChanged,
// but a RESUME emits nothing new. Event-gated via a canary — a globally-visible
// AccountChanged published AFTER the resume; draining the caller's replay up to
// the canary must surface NO ChannelChanged for the DM (in-order per-subscriber
// delivery guarantees a resume event, if any, would precede the canary). RED if
// the created-only guard is dropped (a spurious per-resume re-publish).
func TestOpenDMResumeEmitsNoChannelChanged(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()
	owner := mustUser(t, h.store, "owner")
	alice := mustAgent(t, h.store, owner.ID, "alice")
	mustAgent(t, h.store, owner.ID, "bob")

	// Create the DM first (drains the create event out of the way).
	created, err := h.svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "bob"}))
	if err != nil {
		t.Fatalf("OpenDM(create): %v", err)
	}
	dmID := created.Msg.GetChannel().GetId()

	// Resume the SAME pair — must emit nothing.
	if _, err := h.svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "bob"})); err != nil {
		t.Fatalf("OpenDM(resume): %v", err)
	}

	// Canary published last; drain alice's replay up to it and assert no
	// ChannelChanged for the DM rode along (the create event is in the replay too,
	// so scope the check to events AFTER the create — any SECOND ChannelChanged
	// for dmID is the spurious resume emit).
	canary := mkCanary(t, h, "canary")
	evts := drainReplayAsActor(t, h, alice.ID, canary)
	var dmChannelChanges int
	for _, e := range evts {
		if cc := e.GetChannelChanged(); cc != nil && cc.GetChannel().GetId() == dmID {
			dmChannelChanges++
		}
	}
	if dmChannelChanges != 1 {
		t.Fatalf("ChannelChanged events for the DM = %d, want exactly 1 (the create only; a resume must emit nothing)", dmChannelChanges)
	}
}
