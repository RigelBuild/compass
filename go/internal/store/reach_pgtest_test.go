//go:build pgtest

package store

import (
	"context"
	"slices"
	"testing"
)

func TestDeliveryReachRecipientReads(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	ownerA := mustUser(t, s, "reach-owner-a")
	ownerB := mustUser(t, s, "reach-owner-b")
	agentA := mustAgent(t, s, ownerA.ID, "x")
	agentB := mustAgent(t, s, ownerB.ID, "x")

	shared := mustPolicyChannel(t, s, ownerA.ID, "reach-shared", ChannelPolicy{}, agentA.ID, agentB.ID)
	mandatory := mustPolicyChannel(t, s, ownerA.ID, "reach-mandatory", ChannelPolicy{
		MandatorySubscription: true,
	}, agentA.ID, agentB.ID)
	dm, _ := openDM(t, s, ownerA.ID, "reach-cross-owner-dm", []AccountID{agentA.ID, agentB.ID})
	channels := []struct {
		name string
		id   ChannelID
	}{
		{name: "shared", id: shared.ID},
		{name: "mandatory", id: mandatory.ID},
		{name: "dm", id: dm},
	}
	for _, channel := range channels {
		subscribeAgent(t, s, ownerA.ID, channel.id, agentA.ID)
		subscribeAgent(t, s, ownerA.ID, channel.id, agentB.ID)
	}

	assertState := func(name string, wantB bool) {
		t.Helper()
		for _, channel := range channels {
			t.Run(name+"/"+channel.name, func(t *testing.T) {
				wantAgentAuthor := []AccountID{}
				wantUserAuthor := []AccountID{agentA.ID}
				if wantB {
					wantAgentAuthor = []AccountID{agentB.ID}
					wantUserAuthor = append(wantUserAuthor, agentB.ID)
				}
				assertReachReadSet(t, s, channel.id, agentA.ID, wantAgentAuthor)
				assertReachReadSet(t, s, channel.id, ownerA.ID, wantUserAuthor)
				assertReachReadSet(t, s, channel.id, mustSystemAccount(t, s).ID, []AccountID{agentA.ID, agentB.ID})
			})
		}
	}

	assertState("unpeered", false)
	approvePeer(t, s, ownerA.ID, ownerB.ID)
	assertState("only-a-approved", false)
	if _, err := s.RevokePeer(ctx, ownerA.ID, ownerB.ID); err != nil {
		t.Fatalf("RevokePeer(A, B): %v", err)
	}
	approvePeer(t, s, ownerB.ID, ownerA.ID)
	assertState("only-b-approved", false)
	approvePeer(t, s, ownerA.ID, ownerB.ID)
	assertState("mutual", true)

	if _, err := s.RevokePeer(ctx, ownerA.ID, ownerB.ID); err != nil {
		t.Fatalf("RevokePeer(A, B) after mutual approval: %v", err)
	}
	assertState("a-revoked", false)
	approvePeer(t, s, ownerA.ID, ownerB.ID)
	if _, err := s.RevokePeer(ctx, ownerB.ID, ownerA.ID); err != nil {
		t.Fatalf("RevokePeer(B, A) after mutual approval: %v", err)
	}
	assertState("b-revoked", false)
}

func TestDeliveryReachFiltersUndeliveredAndOwedMentions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	ownerA := mustUser(t, s, "reach-backlog-a")
	ownerB := mustUser(t, s, "reach-backlog-b")
	agentA := mustAgent(t, s, ownerA.ID, "author")
	agentB := mustAgent(t, s, ownerB.ID, "recipient")
	channel := mustPolicyChannel(t, s, ownerA.ID, "reach-backlog", ChannelPolicy{}, agentA.ID, agentB.ID)
	subscribeAgent(t, s, ownerA.ID, channel.ID, agentA.ID)
	subscribeAgent(t, s, ownerA.ID, channel.ID, agentB.ID)
	msgID, _ := postAs(t, s, channel.ID, agentA.ID, "replay after approval")
	assertUndelivered(t, s, agentB.ID, channel.ID, nil)
	approvePeer(t, s, ownerA.ID, ownerB.ID)
	assertUndelivered(t, s, agentB.ID, channel.ID, nil)

	approvePeer(t, s, ownerB.ID, ownerA.ID)
	assertUndelivered(t, s, agentB.ID, channel.ID, []string{msgID})
	if err := s.RecordOwedMention(ctx, agentB.ID, channel.ID, msgID); err != nil {
		t.Fatalf("RecordOwedMention: %v", err)
	}
	assertOwedMentionIDs(t, s, agentB.ID, channel.ID, []string{msgID})

	if _, err := s.RevokePeer(ctx, ownerA.ID, ownerB.ID); err != nil {
		t.Fatalf("RevokePeer(A, B): %v", err)
	}
	assertUndelivered(t, s, agentB.ID, channel.ID, nil)
	assertOwedMentionIDs(t, s, agentB.ID, channel.ID, nil)
	if count, err := s.CountOwedMentions(ctx); err != nil || count != 1 {
		t.Fatalf("CountOwedMentions after revoke = (%d, %v), want (1, nil)", count, err)
	}

	approvePeer(t, s, ownerA.ID, ownerB.ID)
	assertUndelivered(t, s, agentB.ID, channel.ID, []string{msgID})
	assertOwedMentionIDs(t, s, agentB.ID, channel.ID, []string{msgID})
}

func TestAckDeliverySkipsOutOfReachSeq(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	ownerA := mustUser(t, s, "reach-ack-a")
	ownerB := mustUser(t, s, "reach-ack-b")
	agentA := mustAgent(t, s, ownerA.ID, "author")
	agentB := mustAgent(t, s, ownerB.ID, "recipient")
	channel := mustPolicyChannel(t, s, ownerA.ID, "reach-ack", ChannelPolicy{}, agentA.ID, agentB.ID)
	subscribeAgent(t, s, ownerA.ID, channel.ID, agentB.ID)

	postAs(t, s, channel.ID, agentA.ID, "gated seq")
	ackedMessage, ackedSeq := postAs(t, s, channel.ID, ownerB.ID, "same-owner seq")
	if err := s.AckDelivery(ctx, agentB.ID, channel.ID, ackedMessage); err != nil {
		t.Fatalf("AckDelivery: %v", err)
	}
	acked, above, ok := readCursor(t, s, agentB.ID, channel.ID)
	if !ok || acked != ackedSeq || len(above) != 0 {
		t.Fatalf("cursor after ack across a gated seq = (acked=%d, above=%v, exists=%v), want (%d, [], true)",
			acked, above, ok, ackedSeq)
	}
}

func assertReachReadSet(t *testing.T, s *Store, channel ChannelID, author AccountID, want []AccountID) {
	t.Helper()
	ctx := context.Background()
	deliver, err := s.SubscribedAgents(ctx, channel, author)
	if err != nil {
		t.Fatalf("SubscribedAgents(%s): %v", author, err)
	}
	mentions, err := s.ChannelAgentMembers(ctx, channel, author)
	if err != nil {
		t.Fatalf("ChannelAgentMembers(%s): %v", author, err)
	}
	if !sameAccountSet(deliver, want) {
		t.Errorf("SubscribedAgents(%s) = %v, want %v", author, deliver, want)
	}
	mentionIDs := make([]AccountID, len(mentions))
	for i, m := range mentions {
		mentionIDs[i] = m.ID
	}
	if !sameAccountSet(mentionIDs, want) {
		t.Errorf("ChannelAgentMembers(%s) = %v, want %v", author, mentions, want)
	}
}

func assertUndelivered(t *testing.T, s *Store, agent AccountID, channel ChannelID, want []string) {
	t.Helper()
	messages, err := s.UndeliveredMessages(context.Background(), agent)
	if err != nil {
		t.Fatalf("UndeliveredMessages(%s): %v", agent, err)
	}
	got := make([]string, 0, len(messages[channel]))
	for _, message := range messages[channel] {
		got = append(got, string(message.ID))
	}
	if !slices.Equal(got, want) {
		t.Errorf("UndeliveredMessages(%s)[%s] = %v, want %v", agent, channel, got, want)
	}
}

func assertOwedMentionIDs(t *testing.T, s *Store, agent AccountID, channel ChannelID, want []string) {
	t.Helper()
	messages, err := s.OwedMentions(context.Background(), agent)
	if err != nil {
		t.Fatalf("OwedMentions(%s): %v", agent, err)
	}
	got := make([]string, 0, len(messages[channel]))
	for _, message := range messages[channel] {
		got = append(got, string(message.ID))
	}
	if !slices.Equal(got, want) {
		t.Errorf("OwedMentions(%s)[%s] = %v, want %v", agent, channel, got, want)
	}
}

func sameAccountSet(got, want []AccountID) bool {
	if len(got) != len(want) {
		return false
	}
	for _, id := range want {
		if !slices.Contains(got, id) {
			return false
		}
	}
	return true
}

func approvePeer(t *testing.T, s *Store, user, peer AccountID) {
	t.Helper()
	if _, err := s.ApprovePeer(context.Background(), user, peer); err != nil {
		t.Fatalf("ApprovePeer(%s, %s): %v", user, peer, err)
	}
}

func mustSystemAccount(t *testing.T, s *Store) Account {
	t.Helper()
	account, err := s.EnsureSystemAccount(context.Background())
	if err != nil {
		t.Fatalf("EnsureSystemAccount: %v", err)
	}
	return account
}
