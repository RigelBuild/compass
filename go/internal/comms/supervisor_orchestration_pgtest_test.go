//go:build pgtest

package comms

// The T8 acceptance E2E (design.md:1600-1604): the supervisor+bridge MVP
// orchestration loop is "a supervisor assigns work to two workers over channels
// and all coordination is auditable via SearchMessages" — implemented with the
// existing comms RPCs, no bespoke assignment RPC. This drives the real
// CommsService handler against a real Postgres store + real event bus (no
// mocks), modelling the supervisor and the two workers as first-class agent
// accounts and each assignment as a channel message. It defends four contracts
// the record names explicitly:
//
//   - delivery: each worker, as a channel member, reads its assignment back
//     through ListMessages with the supervisor as author and the assignment text
//     intact;
//   - audit: a search for a word common to both assignments returns BOTH from
//     the owner's authorized view (design.md:1604 — all coordination auditable
//     via SearchMessages);
//   - visibility scoping (D9): an outsider account not in the coordination
//     channel searching the same word gets ZERO hits — the audit trail never
//     leaks past the channel's membership (mirrors
//     TestSearchMessagesAuthorizationScoped);
//   - per-assignment distinctness: a search for a word unique to one worker's
//     assignment returns exactly that one message, so a single collapsed post
//     could not pass for two.

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// TestSupervisorAssignsToTwoWorkersAuditable is the T8 acceptance E2E. A
// supervisor agent assigns distinct work to worker A and worker B over one
// coordination channel; both assignments are delivered to their targets and
// auditable via SearchMessages, and the audit respects channel membership.
func TestSupervisorAssignsToTwoWorkersAuditable(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	// The human owner owns the three agents and the coordination channel; the
	// supervisor + two workers are agent accounts (the record's "supervisor
	// agent account assigning to worker agent accounts").
	owner := mustUser(t, st, "owner")
	supervisor := mustAgent(t, st, owner.ID, "supervisor")
	workerA := mustAgent(t, st, owner.ID, "worker-a")
	workerB := mustAgent(t, st, owner.ID, "worker-b")

	// The coordination channel, created through the handler by the owner, with
	// the supervisor and both workers as founding members. (The owner is a
	// founding member by construction — expandOwnerMembership adds the actor.)
	created, err := svc.CreateChannel(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name:          "coordination",
		Kind:          compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
		MemberHandles: []string{supervisor.Handle, workerA.Handle, workerB.Handle},
	}))
	if err != nil {
		t.Fatalf("CreateChannel(coordination): %v", err)
	}
	chID := created.Msg.GetChannel().GetId()

	// The two assignments share the word "implement" (the audit query) and
	// carry a target-unique word each ("parser" / "serializer") so a search can
	// tell them apart — a single collapsed post cannot answer both.
	const (
		assignA = "@worker-a: implement the parser"
		assignB = "@worker-b: implement the serializer"
	)

	// The supervisor posts each assignment as a channel message, authored as the
	// supervisor account.
	postedA, err := svc.PostMessage(WithActor(ctx, supervisor.ID), connect.NewRequest(&compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: chID}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, CreateTopic: true, Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: assignA}}}}))
	if err != nil {
		t.Fatalf("PostMessage(assign A): %v", err)
	}
	postedB, err := svc.PostMessage(WithActor(ctx, supervisor.ID), connect.NewRequest(&compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: chID}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, CreateTopic: true, Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: assignB}}}}))
	if err != nil {
		t.Fatalf("PostMessage(assign B): %v", err)
	}
	if postedA.Msg.GetMessage().GetId() == postedB.Msg.GetMessage().GetId() {
		t.Fatalf("the two assignments share message id %q; they must be distinct posts", postedA.Msg.GetMessage().GetId())
	}

	// Delivery: each worker, as a channel member, reads the assignment history
	// and sees BOTH assignments authored by the supervisor, with the assignment
	// text intact. Reading as the worker (not the owner) proves membership — not
	// ownership — grants the read.
	for _, worker := range []struct {
		name string
		id   store.AccountID
	}{
		{"worker-a", workerA.ID},
		{"worker-b", workerB.ID},
	} {
		listed, err := svc.ListMessages(WithActor(ctx, worker.id), connect.NewRequest(&compassv1.ListMessagesRequest{
			Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: chID},
		}))
		if err != nil {
			t.Fatalf("ListMessages(%s): %v", worker.name, err)
		}
		texts := assignmentTexts(listed.Msg.GetMessages(), string(supervisor.ID))
		if !texts[assignA] {
			t.Fatalf("%s did not receive assignment A from the supervisor; got supervisor texts %v", worker.name, texts)
		}
		if !texts[assignB] {
			t.Fatalf("%s did not receive assignment B from the supervisor; got supervisor texts %v", worker.name, texts)
		}
	}

	// Audit (design.md:1604): the owner searches the common word and finds BOTH
	// assignment messages — all coordination is auditable via SearchMessages.
	auditHits, err := svc.SearchMessages(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.SearchMessagesRequest{
		Query: "implement",
	}))
	if err != nil {
		t.Fatalf("SearchMessages(owner, common word): %v", err)
	}
	if got := len(auditHits.Msg.GetMessages()); got != 2 {
		t.Fatalf("audit search found %d messages, want both assignments (2)", got)
	}
	foundBySupervisor := 0
	for _, m := range auditHits.Msg.GetMessages() {
		if m.GetAuthorAccountId() == string(supervisor.ID) {
			foundBySupervisor++
		}
	}
	if foundBySupervisor != 2 {
		t.Fatalf("audit search returned %d supervisor-authored messages, want both assignments (2)", foundBySupervisor)
	}

	// Per-assignment distinctness: a target-unique word returns exactly its one
	// assignment — so the two posts are genuinely separate, not one text that
	// happened to match twice.
	distinctA, err := svc.SearchMessages(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.SearchMessagesRequest{
		Query: "parser",
	}))
	if err != nil {
		t.Fatalf("SearchMessages(owner, parser): %v", err)
	}
	if got := len(distinctA.Msg.GetMessages()); got != 1 {
		t.Fatalf("search for the A-only word found %d messages, want exactly assignment A (1)", got)
	}
	if id := distinctA.Msg.GetMessages()[0].GetId(); id != postedA.Msg.GetMessage().GetId() {
		t.Fatalf("A-only search matched message %q, want the posted assignment A %q", id, postedA.Msg.GetMessage().GetId())
	}

	// Visibility scoping (D9): an outsider account — a second human, not a member
	// of the coordination channel — searching the audit word gets ZERO hits. The
	// audit trail is scoped to the caller's visible set; coordination never leaks
	// past the channel's membership.
	outsider := mustUser(t, st, "outsider")
	outsiderHits, err := svc.SearchMessages(WithActor(ctx, outsider.ID), connect.NewRequest(&compassv1.SearchMessagesRequest{
		Query: "implement",
	}))
	if err != nil {
		t.Fatalf("SearchMessages(outsider): %v", err)
	}
	if got := len(outsiderHits.Msg.GetMessages()); got != 0 {
		t.Fatalf("an outsider audited %d coordination messages in a channel it cannot see, want 0", got)
	}
}

// assignmentTexts collects the set of text-block strings from messages authored
// by wantAuthor. A message with no text block (or a different author) is
// skipped, so a caller can assert the exact assignment strings a member sees.
func assignmentTexts(msgs []*compassv1.Message, wantAuthor string) map[string]bool {
	out := map[string]bool{}
	for _, m := range msgs {
		if m.GetAuthorAccountId() != wantAuthor {
			continue
		}
		for _, b := range m.GetBlocks() {
			if txt, ok := b.GetBlock().(*compassv1.MessageBlock_Text); ok {
				out[txt.Text] = true
			}
		}
	}
	return out
}

// TestSupervisorPerTopicThreadIsolation drives the many-conversations shape: one
// supervisor fans out to four workers over per-worker topics in ONE channel,
// posting INTERLEAVED across topics. A topic-scoped ListMessages must return
// exactly that worker's thread, in order — the interleaving is what makes it
// bite, since grouped posts would pass even if topic routing were broken.
func TestSupervisorPerTopicThreadIsolation(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	supervisor := mustAgent(t, st, owner.ID, "supervisor")
	workers := make([]store.Account, 4)
	handles := make([]string, 4)
	for i := range workers {
		workers[i] = mustAgent(t, st, owner.ID, fmt.Sprintf("worker-%d", i+1))
		handles[i] = workers[i].Handle
	}

	created, err := svc.CreateChannel(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name:          "coordination",
		Kind:          compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
		MemberHandles: append([]string{supervisor.Handle}, handles...),
	}))
	if err != nil {
		t.Fatalf("CreateChannel(coordination): %v", err)
	}
	chID := created.Msg.GetChannel().GetId()

	// One topic per worker; capture the server-assigned topic id so the later
	// ListMessages scopes by id, not by re-resolving a name.
	topicIDs := make([]string, 4)
	topicNames := make([]string, 4)
	for i := range workers {
		topicNames[i] = fmt.Sprintf("topic-%d", i+1)
	}

	// Three interleaved rounds: post to topic 1, then 2, 3, 4, then back to 1…
	// Each worker's thread is its three posts in send order. wantTexts records
	// the expected per-topic thread as it is posted.
	const rounds = 3
	wantTexts := make([][]string, 4)
	for r := range rounds {
		for i := range workers {
			text := fmt.Sprintf("assignment for worker %d round %d", i+1, r+1)
			posted, err := svc.PostMessage(WithActor(ctx, supervisor.ID), connect.NewRequest(&compassv1.PostMessageRequest{
				Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: chID},
				Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: topicNames[i]},
				CreateTopic: true,
				Blocks:      []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: text}}},
			}))
			if err != nil {
				t.Fatalf("PostMessage(worker %d round %d): %v", i+1, r+1, err)
			}
			if topicIDs[i] == "" {
				topicIDs[i] = posted.Msg.GetMessage().GetTopicId()
			}
			wantTexts[i] = append(wantTexts[i], text)
		}
	}

	for i, w := range workers {
		listed, err := svc.ListMessages(WithActor(ctx, w.ID), connect.NewRequest(&compassv1.ListMessagesRequest{
			Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: chID},
			TopicId:   topicIDs[i],
		}))
		if err != nil {
			t.Fatalf("ListMessages(worker %d, topic-scoped): %v", i+1, err)
		}
		// ListMessages pages newest-first (messages.sql.go ORDER BY seq DESC), so
		// reverse the expected thread to send order for the equality check.
		got := textsInOrder(listed.Msg.GetMessages())
		want := reversed(wantTexts[i])
		// mutation: a topic filter that leaks another worker's post (or drops the
		// TopicId clause) reddens this — the thread would carry a foreign or extra
		// message, so the exact-slice compare fails.
		if !slices.Equal(got, want) {
			t.Fatalf("worker %d topic thread = %v, want exactly its own %v", i+1, got, want)
		}
	}
}

// TestSupervisorUpwardReportKeepsAuthor pins the upward-report direction: a
// worker posts a report on its own topic, and the report lands on that topic
// with the WORKER as author — not the supervisor and not the owner. The audit
// trail must attribute a report to who wrote it.
func TestSupervisorUpwardReportKeepsAuthor(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	supervisor := mustAgent(t, st, owner.ID, "supervisor")
	worker := mustAgent(t, st, owner.ID, "worker-1")

	created, err := svc.CreateChannel(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name:          "coordination",
		Kind:          compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
		MemberHandles: []string{supervisor.Handle, worker.Handle},
	}))
	if err != nil {
		t.Fatalf("CreateChannel(coordination): %v", err)
	}
	chID := created.Msg.GetChannel().GetId()

	const report = "worker-1 report: parser landed"
	posted, err := svc.PostMessage(WithActor(ctx, worker.ID), connect.NewRequest(&compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: chID},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: "topic-1"},
		CreateTopic: true,
		Blocks:      []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: report}}},
	}))
	if err != nil {
		t.Fatalf("PostMessage(worker report): %v", err)
	}
	topicID := posted.Msg.GetMessage().GetTopicId()

	listed, err := svc.ListMessages(WithActor(ctx, supervisor.ID), connect.NewRequest(&compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: chID},
		TopicId:   topicID,
	}))
	if err != nil {
		t.Fatalf("ListMessages(supervisor, report topic): %v", err)
	}
	if got := len(listed.Msg.GetMessages()); got != 1 {
		t.Fatalf("report topic carries %d messages, want the single report (1)", got)
	}
	msg := listed.Msg.GetMessages()[0]
	// mutation: PostMessage stamping the actor wrong (e.g. the channel owner or a
	// hardcoded supervisor) reddens this — a report must be attributed to the
	// worker that authored it, or the audit trail lies about who reported.
	if got := msg.GetAuthorAccountId(); got != string(worker.ID) {
		t.Fatalf("report author = %q, want the worker %q (not supervisor %q / owner %q)", got, worker.ID, supervisor.ID, owner.ID)
	}
}

// TestSupervisorAuditScopedAtFourWorkers extends the membership-scoped audit to
// the four-worker shape: the owner's authorized SearchMessages finds the
// coordination traffic, while an account NOT in the channel gets ZERO hits for
// the same term. Zero, not "fewer" — a leaked count would be an existence
// oracle (D9).
func TestSupervisorAuditScopedAtFourWorkers(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	supervisor := mustAgent(t, st, owner.ID, "supervisor")
	handles := make([]string, 4)
	for i := range handles {
		handles[i] = mustAgent(t, st, owner.ID, fmt.Sprintf("worker-%d", i+1)).Handle
	}

	created, err := svc.CreateChannel(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name:          "coordination",
		Kind:          compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
		MemberHandles: append([]string{supervisor.Handle}, handles...),
	}))
	if err != nil {
		t.Fatalf("CreateChannel(coordination): %v", err)
	}
	chID := created.Msg.GetChannel().GetId()

	// One assignment per worker, all sharing the audit word "dispatch".
	for i := range handles {
		if _, err := svc.PostMessage(WithActor(ctx, supervisor.ID), connect.NewRequest(&compassv1.PostMessageRequest{
			Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: chID},
			Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: fmt.Sprintf("topic-%d", i+1)},
			CreateTopic: true,
			Blocks:      []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: fmt.Sprintf("dispatch task %d", i+1)}}},
		})); err != nil {
			t.Fatalf("PostMessage(assign %d): %v", i+1, err)
		}
	}

	auditHits, err := svc.SearchMessages(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.SearchMessagesRequest{
		Query: "dispatch",
	}))
	if err != nil {
		t.Fatalf("SearchMessages(owner): %v", err)
	}
	// mutation: a search that under-scopes the owner's authorized view drops
	// coordination traffic the owner is entitled to audit.
	if got := len(auditHits.Msg.GetMessages()); got != len(handles) {
		t.Fatalf("owner audit found %d messages, want all four assignments (%d)", got, len(handles))
	}

	outsider := mustUser(t, st, "outsider")
	outsiderHits, err := svc.SearchMessages(WithActor(ctx, outsider.ID), connect.NewRequest(&compassv1.SearchMessagesRequest{
		Query: "dispatch",
	}))
	if err != nil {
		t.Fatalf("SearchMessages(outsider): %v", err)
	}
	// mutation: a search that scopes by the query alone instead of the caller's
	// membership turns the audit into an existence oracle — a non-member would
	// learn the coordination traffic exists. Must be exactly zero.
	if got := len(outsiderHits.Msg.GetMessages()); got != 0 {
		t.Fatalf("outsider audited %d messages in a channel it cannot see, want 0", got)
	}
}

// textsInOrder collects the text-block string of each message, in the slice's
// order, so a caller can assert an exact topic thread.
func textsInOrder(msgs []*compassv1.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		for _, b := range m.GetBlocks() {
			if txt, ok := b.GetBlock().(*compassv1.MessageBlock_Text); ok {
				out = append(out, txt.Text)
			}
		}
	}
	return out
}

// reversed returns a copy of in with the order flipped (send order ⇄ the
// newest-first order ListMessages returns).
func reversed(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[len(in)-1-i] = s
	}
	return out
}
