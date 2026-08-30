//go:build pgtest

package comms

// UpdatePinnedBoard handler contracts at the RPC edge (RIG-1723 T6,
// design.md:626-637): the handler authorizes a board mutation against the
// channel's post_policy (any member on OPEN, only the owner on OWNER_ONLY, a
// non-owner collapsing to the SAME CodeNotFound a non-member gets — no oracle),
// maps the request's op oneof to the pure-pointer store call (pin / pin-with-
// replace CAS / unpin — never a message write), and emits ChannelChanged
// carrying the updated board. Driven in-process via connect.NewRequest +
// WithActor against a real store and bus.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// pinnableMessage posts a real message under (ch, "general") as author through
// the store and returns its id — a valid pin target living in ch. Pins point at
// existing messages, so the handler tests need genuine message rows.
func pinnableMessage(t *testing.T, st *store.Store, ch store.ChannelID, author store.AccountID, body string) string {
	t.Helper()
	msg, _, err := st.AppendMessage(context.Background(),
		store.Message{AuthorAccountID: author, Blocks: []store.MessageBlock{{Text: &body}}},
		string(ch), store.TopicRef{Name: "general", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage(%q): %v", body, err)
	}
	return string(msg.ID)
}

// wireBoardIDs projects the ordered message ids of a wire board.
func wireBoardIDs(entries []*compassv1.PinnedEntry) []string {
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.GetMessageId()
	}
	return ids
}

// pinReq builds an UpdatePinnedBoard pin request (replace optional).
func pinReq(channelID, messageID, replace string) *compassv1.UpdatePinnedBoardRequest {
	return &compassv1.UpdatePinnedBoardRequest{
		ChannelId: channelID,
		Op: &compassv1.UpdatePinnedBoardRequest_Pin{
			Pin: &compassv1.PinMessage{MessageId: messageID, ReplaceMessageId: replace},
		},
	}
}

// TestUpdatePinnedBoardPinFromAnotherChannelRejectedInBand: a pin naming a
// message that lives in ANOTHER channel's topic is refused with CodeNotFound at
// the edge — the store's join guard (not-found/forbidden merge) mapped through
// edgeError.
func TestUpdatePinnedBoardPinFromAnotherChannelRejectedInBand(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")

	chA, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{Name: "room-a", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel(A): %v", err)
	}
	chB, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{Name: "room-b", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel(B): %v", err)
	}
	msgB := pinnableMessage(t, st, chB.ID, owner.ID, "over in B")

	_, err = svc.UpdatePinnedBoard(WithActor(ctx, owner.ID), connect.NewRequest(pinReq(string(chA.ID), msgB, "")))
	connectCodeIs(t, err, connect.CodeNotFound, "pin of a message from another channel")
}

// TestUpdatePinnedBoardRepointSwapsAtomically: pin-with-replace atomically swaps
// the board entry — the old id is gone, the new id present, and position is
// preserved (the repoint CAS success path, surfaced through the handler).
func TestUpdatePinnedBoardRepointSwapsAtomically(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	ch, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	m1 := pinnableMessage(t, st, ch.ID, owner.ID, "first")
	m2 := pinnableMessage(t, st, ch.ID, owner.ID, "second")
	m3 := pinnableMessage(t, st, ch.ID, owner.ID, "replacement")

	if _, err := svc.UpdatePinnedBoard(WithActor(ctx, owner.ID), connect.NewRequest(pinReq(string(ch.ID), m1, ""))); err != nil {
		t.Fatalf("pin m1: %v", err)
	}
	if _, err := svc.UpdatePinnedBoard(WithActor(ctx, owner.ID), connect.NewRequest(pinReq(string(ch.ID), m2, ""))); err != nil {
		t.Fatalf("pin m2: %v", err)
	}

	// Repoint m1 -> m3: m3 takes m1's slot (position 0), m2 stays at position 1.
	resp, err := svc.UpdatePinnedBoard(WithActor(ctx, owner.ID), connect.NewRequest(pinReq(string(ch.ID), m3, m1)))
	if err != nil {
		t.Fatalf("repoint m1->m3: %v", err)
	}
	got := wireBoardIDs(resp.Msg.GetChannel().GetPinnedEntries())
	if len(got) != 2 || got[0] != m3 || got[1] != m2 {
		t.Fatalf("board = %v, want [%s %s] (m3 at m1's preserved position, m2 unchanged)", got, m3, m2)
	}
}

// TestUpdatePinnedBoardRepointStaleFailsInBand: a replace naming a message that
// is no longer pinned loses the compare-and-swap and fails in-band
// (CodeAlreadyExists — the store's ErrConflict "board changed, re-read").
func TestUpdatePinnedBoardRepointStaleFailsInBand(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	ch, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	m1 := pinnableMessage(t, st, ch.ID, owner.ID, "pinned")
	m2 := pinnableMessage(t, st, ch.ID, owner.ID, "never pinned")
	m3 := pinnableMessage(t, st, ch.ID, owner.ID, "incoming")

	if _, err := svc.UpdatePinnedBoard(WithActor(ctx, owner.ID), connect.NewRequest(pinReq(string(ch.ID), m1, ""))); err != nil {
		t.Fatalf("pin m1: %v", err)
	}
	// m2 was never pinned, so the CAS that names it as replace is lost.
	_, err = svc.UpdatePinnedBoard(WithActor(ctx, owner.ID), connect.NewRequest(pinReq(string(ch.ID), m3, m2)))
	connectCodeIs(t, err, connect.CodeAlreadyExists, "repoint naming a no-longer-pinned id")
}

// TestUpdatePinnedBoardCapRejectedInBand: with the board already at the
// per-channel cap, the cap+1-th fresh pin is refused in-band
// (CodeFailedPrecondition — the store's ErrFailedPrecondition cap guard).
func TestUpdatePinnedBoardCapRejectedInBand(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	ch, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	// Fill the board to the cap (store.maxChannelPins is 5).
	const boardCap = 5
	for i := range boardCap {
		m := pinnableMessage(t, st, ch.ID, owner.ID, "pin")
		if _, err := svc.UpdatePinnedBoard(WithActor(ctx, owner.ID), connect.NewRequest(pinReq(string(ch.ID), m, ""))); err != nil {
			t.Fatalf("pin %d: %v", i, err)
		}
	}
	overflow := pinnableMessage(t, st, ch.ID, owner.ID, "one too many")
	_, err = svc.UpdatePinnedBoard(WithActor(ctx, owner.ID), connect.NewRequest(pinReq(string(ch.ID), overflow, "")))
	connectCodeIs(t, err, connect.CodeFailedPrecondition, "cap+1 fresh pin")
}

// TestUpdatePinnedBoardNonOwnerOnOwnerOnlyIsNotFound: a member who is not the
// owner mutating an OWNER_ONLY board is refused with CodeNotFound — the exact
// code a non-member gets, so the policy leaks no oracle (mirrors PostMessage's
// OWNER_ONLY enforcement).
func TestUpdatePinnedBoardNonOwnerOnOwnerOnlyIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	other := mustUser(t, st, "other")

	created, err := svc.CreateChannel(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name: "room", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
		MemberHandles: []string{other.Handle},
	}))
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	chID := created.Msg.GetChannel().GetId()
	// A real pin target in the channel (posted by the owner, who may post).
	msg := pinnableMessage(t, st, store.ChannelID(chID), owner.ID, "target")

	if _, err := svc.SetChannelPolicy(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.SetChannelPolicyRequest{
		ChannelId:   chID,
		PostPolicy:  compassv1.ChannelPostPolicy_CHANNEL_POST_POLICY_OWNER_ONLY,
		OwnerHandle: owner.Handle,
	})); err != nil {
		t.Fatalf("SetChannelPolicy: %v", err)
	}

	// The non-owner member is refused with the same NotFound a non-member gets.
	_, err = svc.UpdatePinnedBoard(WithActor(ctx, other.ID), connect.NewRequest(pinReq(chID, msg, "")))
	connectCodeIs(t, err, connect.CodeNotFound, "non-owner pin on OWNER_ONLY channel")

	// The owner, by contrast, may mutate the OWNER_ONLY board.
	if _, err := svc.UpdatePinnedBoard(WithActor(ctx, owner.ID), connect.NewRequest(pinReq(chID, msg, ""))); err != nil {
		t.Fatalf("owner pin on OWNER_ONLY channel: %v", err)
	}
}

// TestUpdatePinnedBoardNonMemberIsNotFound: a non-member mutating any board is
// refused with CodeNotFound (the not-found/forbidden merge), never a hint the
// channel exists.
func TestUpdatePinnedBoardNonMemberIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	outsider := mustUser(t, st, "outsider")
	ch, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	msg := pinnableMessage(t, st, ch.ID, owner.ID, "target")

	_, err = svc.UpdatePinnedBoard(WithActor(ctx, outsider.ID), connect.NewRequest(pinReq(string(ch.ID), msg, "")))
	connectCodeIs(t, err, connect.CodeNotFound, "non-member board mutation")
}

// TestUpdatePinnedBoardUnpinRemovesEntry: an unpin op removes the entry and the
// response board reflects the removal (a pure pointer op, no message write).
func TestUpdatePinnedBoardUnpinRemovesEntry(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	ch, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	m1 := pinnableMessage(t, st, ch.ID, owner.ID, "keep")
	m2 := pinnableMessage(t, st, ch.ID, owner.ID, "drop")
	if _, err := svc.UpdatePinnedBoard(WithActor(ctx, owner.ID), connect.NewRequest(pinReq(string(ch.ID), m1, ""))); err != nil {
		t.Fatalf("pin m1: %v", err)
	}
	if _, err := svc.UpdatePinnedBoard(WithActor(ctx, owner.ID), connect.NewRequest(pinReq(string(ch.ID), m2, ""))); err != nil {
		t.Fatalf("pin m2: %v", err)
	}

	resp, err := svc.UpdatePinnedBoard(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.UpdatePinnedBoardRequest{
		ChannelId: string(ch.ID),
		Op:        &compassv1.UpdatePinnedBoardRequest_UnpinMessageId{UnpinMessageId: m2},
	}))
	if err != nil {
		t.Fatalf("unpin m2: %v", err)
	}
	got := wireBoardIDs(resp.Msg.GetChannel().GetPinnedEntries())
	if len(got) != 1 || got[0] != m1 {
		t.Fatalf("board = %v after unpin m2, want [%s]", got, m1)
	}
}

// TestUpdatePinnedBoardEmptyOpIsInvalidArgument: a request with no op oneof set
// is a malformed request (CodeInvalidArgument).
func TestUpdatePinnedBoardEmptyOpIsInvalidArgument(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	ch, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	_, err = svc.UpdatePinnedBoard(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.UpdatePinnedBoardRequest{
		ChannelId: string(ch.ID),
	}))
	connectCodeIs(t, err, connect.CodeInvalidArgument, "request with no op set")
}

// TestUpdatePinnedBoardEmitsChannelChangedWithBoard: a successful pin fans out a
// ChannelChanged carrying the updated board (design.md:637) — the live
// projection stays current without a re-read.
func TestUpdatePinnedBoardEmitsChannelChangedWithBoard(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()
	owner := mustUser(t, h.store, "owner")
	ch, err := h.store.CreateChannel(ctx, owner.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	msg := pinnableMessage(t, h.store, ch.ID, owner.ID, "target")

	events := firstEventAfterBoundary(t, h, owner.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})

	if _, err := h.svc.UpdatePinnedBoard(WithActor(ctx, owner.ID), connect.NewRequest(pinReq(string(ch.ID), msg, ""))); err != nil {
		t.Fatalf("UpdatePinnedBoard: %v", err)
	}

	got := awaitFirst(t, events)
	cc := got.GetChannelChanged()
	if cc == nil {
		t.Fatalf("event payload = %T, want ChannelChanged", got.GetPayload())
	}
	board := wireBoardIDs(cc.GetChannel().GetPinnedEntries())
	if len(board) != 1 || board[0] != msg {
		t.Fatalf("ChannelChanged board = %v, want [%s]", board, msg)
	}
}

// TestUpdatePinnedBoardMembershipRevokedMidFlightIsNotFound: a member removed
// from the channel (membership revoked and committed) can no longer mutate the
// board — the pin is refused with CodeNotFound. This pins the in-tx membership
// gate the store now enforces under the FOR UPDATE lock (mirroring PostMessage):
// the who-may-act decision is made inside the board txn against the committed
// membership, so a just-removed member cannot slip a mutation through on a stale
// edge snapshot (the closed TOCTOU). Reverting the store's in-tx
// requireBoardMutator membership gate lets the pin succeed → this test fails.
func TestUpdatePinnedBoardMembershipRevokedMidFlightIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	other := mustUser(t, st, "other")

	ch, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name: "room", Kind: store.ChannelKindChannel,
		MemberAccountIDs: []store.AccountID{other.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	msg := pinnableMessage(t, st, ch.ID, owner.ID, "target")

	// `other` is a member first — a pin would succeed. Now revoke and commit.
	if _, _, err := st.UpdateChannelMembers(ctx, owner.ID, ch.ID, []store.MemberUpdate{
		{AccountID: other.ID, Remove: true},
	}); err != nil {
		t.Fatalf("UpdateChannelMembers(remove other): %v", err)
	}

	// The removed member's pin is refused with the same NotFound a non-member
	// gets — the in-tx membership gate, not a stale edge snapshot, decides.
	_, err = svc.UpdatePinnedBoard(WithActor(ctx, other.ID), connect.NewRequest(pinReq(string(ch.ID), msg, "")))
	connectCodeIs(t, err, connect.CodeNotFound, "pin by a member whose membership was revoked")
}

// TestUpdatePinnedBoardOwnerOnlyNonOwnerInTxIsNotFound: on an OWNER_ONLY channel
// a non-owner member's pin is refused with CodeNotFound by the store's in-tx
// post_policy gate (read under the FOR UPDATE lock, mirroring PostMessage), the
// in-tx analogue of the edge-authz OWNER_ONLY test. Reverting the store's in-tx
// post_policy check lets the non-owner pin succeed → this test fails.
func TestUpdatePinnedBoardOwnerOnlyNonOwnerInTxIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	other := mustUser(t, st, "other")

	created, err := svc.CreateChannel(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name: "room", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
		MemberHandles: []string{other.Handle},
	}))
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	chID := created.Msg.GetChannel().GetId()
	msg := pinnableMessage(t, st, store.ChannelID(chID), owner.ID, "target")

	if _, err := svc.SetChannelPolicy(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.SetChannelPolicyRequest{
		ChannelId:   chID,
		PostPolicy:  compassv1.ChannelPostPolicy_CHANNEL_POST_POLICY_OWNER_ONLY,
		OwnerHandle: owner.Handle,
	})); err != nil {
		t.Fatalf("SetChannelPolicy: %v", err)
	}

	// The non-owner member is refused by the in-tx post_policy gate.
	_, err = svc.UpdatePinnedBoard(WithActor(ctx, other.ID), connect.NewRequest(pinReq(chID, msg, "")))
	connectCodeIs(t, err, connect.CodeNotFound, "non-owner pin on OWNER_ONLY channel (in-tx gate)")
}

// TestUpdatePinnedBoardNonOwnerMemberOnOpenSucceeds: on an OPEN channel any
// member — not only the owner — may mutate the board. A second member `other`
// pins a message and the returned board contains the pin. This pins the OPEN
// half of the store's in-tx gate (member, not owner-only): a regression that
// narrowed board authz to owner-only on OPEN would fail here (every other
// success test pins as the owner, so none would catch that narrowing).
func TestUpdatePinnedBoardNonOwnerMemberOnOpenSucceeds(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	other := mustUser(t, st, "other")

	ch, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name: "room", Kind: store.ChannelKindChannel,
		MemberAccountIDs: []store.AccountID{other.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	msg := pinnableMessage(t, st, ch.ID, owner.ID, "target")

	// `other` is a plain member of an OPEN channel (default policy): the pin
	// succeeds and the returned board carries it.
	resp, err := svc.UpdatePinnedBoard(WithActor(ctx, other.ID), connect.NewRequest(pinReq(string(ch.ID), msg, "")))
	if err != nil {
		t.Fatalf("non-owner member pin on OPEN channel: %v", err)
	}
	got := wireBoardIDs(resp.Msg.GetChannel().GetPinnedEntries())
	if len(got) != 1 || got[0] != msg {
		t.Fatalf("board = %v, want [%s] (non-owner member's pin present)", got, msg)
	}
}
