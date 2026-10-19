//go:build pgtest && unix

package server

import (
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// undeliveredIDs lists the message ids an agent's undelivered sweep would return.
func undeliveredIDs(t *testing.T, w *dmE2EWire, agent store.AccountID) []string {
	t.Helper()
	pending, err := w.store.UndeliveredMessages(w.ctx, agent)
	if err != nil {
		t.Fatalf("UndeliveredMessages: %v", err)
	}
	var ids []string
	for _, msgs := range pending {
		for _, m := range msgs {
			ids = append(ids, string(m.ID))
		}
	}
	return ids
}

// The foreign rebase notice rides real comms: authored by the submitting agent
// in its DM with the owning agent, published live, and kept for an offline sweep.
func TestForgeRebaseNoticeReachesLiveAndOfflineAgent(t *testing.T) {
	w := newDME2EWire(t)
	submitter := w.seedAgent(t, "rebase-submitter")
	owner := w.seedAgent(t, "rebase-owner")
	svc := newForgeService(w.store, nil, newForgeProviderRegistry())
	svc.notifier = w.comms

	sub, err := w.commsBus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	const text = "I rebased your bookmark base (PR #9) onto main."
	if err := svc.notifyAgent(w.ctx, submitter.ID, owner.ID, "k:notice:base", text); err != nil {
		t.Fatalf("notifyAgent: %v", err)
	}

	var posted *compassv1.Message
	for posted == nil {
		mp := awaitCommsEvent(t, sub.Live).GetMessagePosted()
		if mp == nil {
			continue // the DM's ChannelChanged comes first
		}
		posted = mp.GetMessage()
	}
	if posted.GetAuthorAccountId() != string(submitter.ID) || len(posted.GetBlocks()) == 0 || posted.GetBlocks()[0].GetText() != text {
		t.Fatalf("MessagePosted = %v, want the notice authored by the submitter", posted)
	}

	// The owner never ran, so the DM's born-mandatory cursor holds the notice for its sweep.
	if ids := undeliveredIDs(t, w, owner.ID); len(ids) != 1 || ids[0] != posted.GetId() {
		t.Fatalf("owner undelivered = %v, want only the notice %s", ids, posted.GetId())
	}

	// A retry of the same submit posts no second notice.
	if err := svc.notifyAgent(w.ctx, submitter.ID, owner.ID, "k:notice:base", text); err != nil {
		t.Fatalf("notifyAgent retry: %v", err)
	}
	if ids := undeliveredIDs(t, w, owner.ID); len(ids) != 1 {
		t.Fatalf("owner undelivered after retry = %v, want only the notice", ids)
	}
}
