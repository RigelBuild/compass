//go:build pgtest && unix

package comms

// The agent-TOOL comms entries (peer-DM R1/R2): PostAsAccountByName /
// ListAsAccountByName resolve a channel NAME within the caller's visible set.
// Contract: a visible name resolves; unknown or invisible is CodeNotFound (D9);
// ambiguous is CodeInvalidArgument; post/ask have no home default, list keeps it.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// TestPostAsAccountByNameResolvesVisibleChannel: a post naming a channel the
// agent is a member of resolves to that channel's id and lands there, authored by
// the agent. create_topic threads through so the named topic is minted.
func TestPostAsAccountByNameResolvesVisibleChannel(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	ch, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name:             "war-room",
		Kind:             store.ChannelKindChannel,
		MemberAccountIDs: []store.AccountID{agent.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	resp, err := svc.PostAsAccountByName(ctx, agent.ID, &compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: "war-room"},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
		CreateTopic: true,
		Blocks:      textBlocks("named post"),
	})
	if err != nil {
		t.Fatalf("PostAsAccountByName: %v", err)
	}
	if got := resp.GetMessage().GetAuthorAccountId(); got != string(agent.ID) {
		t.Fatalf("author = %q, want the agent %q", got, agent.ID)
	}

	// Read it back through the resolved channel (by id) to prove it landed there.
	listed, err := svc.ListAsAccount(ctx, agent.ID, &compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: string(ch.ID)},
	})
	if err != nil {
		t.Fatalf("ListAsAccount(read-back): %v", err)
	}
	var found bool
	for _, m := range listed.GetMessages() {
		if m.GetId() == resp.GetMessage().GetId() {
			found = true
		}
	}
	if !found {
		t.Fatalf("named post %q not found in the resolved channel", resp.GetMessage().GetId())
	}
}

// TestPostAsAccountByNamePreservesTurnSequence verifies the channel-name adapter
// does not drop the sequence before the shared post handler persists it.
func TestPostAsAccountByNamePreservesTurnSequence(t *testing.T) {

	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	ch, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name:             "war-room",
		Kind:             store.ChannelKindChannel,
		MemberAccountIDs: []store.AccountID{agent.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	resp, err := svc.PostAsAccountByName(ctx, agent.ID, &compassv1.PostMessageRequest{
		Container:    &compassv1.PostMessageRequest_ChannelId{ChannelId: "war-room"},
		Topic:        &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
		CreateTopic:  true,
		TurnSequence: 7,
		Blocks:       textBlocks("turn seven"),
	})
	if err != nil {
		t.Fatalf("PostAsAccountByName: %v", err)
	}
	if got := resp.GetMessage().GetTurnSequence(); got != 7 {
		t.Fatalf("PostAsAccountByName response turn_sequence = %d, want 7", got)
	}

	stored, err := st.MessageByID(ctx, resp.GetMessage().GetId())
	if err != nil {
		t.Fatalf("MessageByID: %v", err)
	}
	if stored.TurnSequence != 7 {
		t.Fatalf("PostAsAccountByName stored turn_sequence = %d, want 7 (channel %q)", stored.TurnSequence, ch.ID)
	}
}

// TestPostAsAccountByNameUnknownChannelIsNotFound: a name no visible channel
// carries is CodeNotFound — the resolver miss surfaced at the tool edge.
func TestPostAsAccountByNameUnknownChannelIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")

	_, err := svc.PostAsAccountByName(ctx, agent.ID, &compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: "no-such-channel"},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
		CreateTopic: true,
		Blocks:      textBlocks("into the void"),
	})
	connectCodeIs(t, err, connect.CodeNotFound, "PostAsAccountByName(unknown channel)")
}

// TestPostAsAccountByNameInvisibleChannelIsNotFound: a channel that exists but
// the agent cannot see resolves to the SAME CodeNotFound an unknown name gets —
// the D9 not-found/forbidden merge carried through the tool edge.
func TestPostAsAccountByNameInvisibleChannelIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	// A private channel the agent is NOT a member of.
	if _, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{Name: "private", Kind: store.ChannelKindChannel}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	_, err := svc.PostAsAccountByName(ctx, agent.ID, &compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: "private"},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
		CreateTopic: true,
		Blocks:      textBlocks("sneaking in by name"),
	})
	connectCodeIs(t, err, connect.CodeNotFound, "PostAsAccountByName(invisible channel)")
}

// TestPostAsAccountByNameAmbiguousChannelIsInvalidArgument: two channels the
// agent's owner can see sharing a name surface as CodeInvalidArgument — the
// caller must disambiguate; the server never silently picks one. Ungrouped
// channels are name-unconstrained, so two same-named ones both visible to the
// agent (member of both) is the achievable collision.
func TestPostAsAccountByNameAmbiguousChannelIsInvalidArgument(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	for range 2 {
		if _, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{
			Name: "dupe", Kind: store.ChannelKindChannel,
			MemberAccountIDs: []store.AccountID{agent.ID},
		}); err != nil {
			t.Fatalf("CreateChannel(dupe): %v", err)
		}
	}

	_, err := svc.PostAsAccountByName(ctx, agent.ID, &compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: "dupe"},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
		CreateTopic: true,
		Blocks:      textBlocks("which dupe?"),
	})
	connectCodeIs(t, err, connect.CodeInvalidArgument, "PostAsAccountByName(ambiguous channel)")
}

// TestPostAsAccountByNameEmptyChannelHasNoHomeDefault: R2 drops the home default
// at the tool level for post/ask — an empty channel name is NOT filled from home,
// so it resolves like any other miss to CodeNotFound. (The agent must NAME its
// channel, even its own home; TS schema-requires a non-blank channel, so this is
// the defense-in-depth for a wire that somehow arrives empty.)
func TestPostAsAccountByNameEmptyChannelHasNoHomeDefault(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")

	_, err := svc.PostAsAccountByName(ctx, agent.ID, &compassv1.PostMessageRequest{
		// No channel name — under R2 this is NOT a home post; it is a miss.
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
		CreateTopic: true,
		Blocks:      textBlocks("no channel named"),
	})
	connectCodeIs(t, err, connect.CodeNotFound, "PostAsAccountByName(empty channel, no home default)")

	// The agent's home channel stayed empty — no silent home fallback wrote there.
	listed, err := svc.ListAsAccount(ctx, agent.ID, &compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: string(agent.Agent.HomeChannelID)},
	})
	if err != nil {
		t.Fatalf("ListAsAccount(home): %v", err)
	}
	if n := len(listed.GetMessages()); n != 0 {
		t.Fatalf("home channel holds %d messages, want 0 (no home fallback for post/ask)", n)
	}
}

// TestListAsAccountByNameEmptyChannelKeepsHomeDefault: R2 KEEPS omit-=home for
// list (a read has no misroute hazard) — an empty channel name lists the agent's
// home channel, while a named channel resolves through the viewer-scoped resolver.
func TestListAsAccountByNameEmptyChannelKeepsHomeDefault(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")

	// Seed one message in the agent's home channel (id-typed post — internal).
	seed, err := svc.PostAsAccount(ctx, agent.ID, &compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: string(agent.Agent.HomeChannelID)},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
		CreateTopic: true,
		Blocks:      textBlocks("home seed"),
	})
	if err != nil {
		t.Fatalf("PostAsAccount(home seed): %v", err)
	}

	// An empty channel name on the LIST tool entry defaults to home and finds it.
	listed, err := svc.ListAsAccountByName(ctx, agent.ID, &compassv1.ListMessagesRequest{})
	if err != nil {
		t.Fatalf("ListAsAccountByName(empty=home): %v", err)
	}
	var found bool
	for _, m := range listed.GetMessages() {
		if m.GetId() == seed.GetMessage().GetId() {
			found = true
		}
	}
	if !found {
		t.Fatalf("home seed %q not found via empty-name list (omit-=home not honored)", seed.GetMessage().GetId())
	}
}

// TestListAsAccountByNameUnknownChannelIsNotFound: a NON-empty list channel name
// that no visible channel carries is CodeNotFound — omit-=home applies only to the
// empty case; a named-but-unknown channel still misses.
func TestListAsAccountByNameUnknownChannelIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")

	_, err := svc.ListAsAccountByName(ctx, agent.ID, &compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: "no-such-channel"},
	})
	connectCodeIs(t, err, connect.CodeNotFound, "ListAsAccountByName(unknown channel)")
}

func TestListAsAccountByNameTopicFilter(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	ch, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name: "war-room", Kind: store.ChannelKindChannel,
		MemberAccountIDs: []store.AccountID{agent.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	post := func(topic, text string) string {
		t.Helper()
		resp, err := svc.PostAsAccount(ctx, agent.ID, &compassv1.PostMessageRequest{
			Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
			Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: topic},
			CreateTopic: true,
			Blocks:      textBlocks(text),
		})
		if err != nil {
			t.Fatalf("PostAsAccount(%s): %v", topic, err)
		}
		return resp.GetMessage().GetId()
	}
	deployID := post("Deploy", "deploy message")
	post("Design", "design message")

	deploy, err := st.MessageByID(ctx, deployID)
	if err != nil {
		t.Fatalf("MessageByID(%s): %v", deployID, err)
	}
	listed, err := svc.ListAsAccountByName(ctx, agent.ID, &compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: "war-room"},
		TopicId:   deploy.TopicID,
	})
	if err != nil {
		t.Fatalf("ListAsAccountByName: %v", err)
	}
	if got := listed.GetMessages(); len(got) != 1 || got[0].GetId() != deployID {
		t.Fatalf("topic-filtered messages = %+v, want only %q", got, deployID)
	}
}

func TestListTopicsAsAccountByNameUsesHomeChannel(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	agent := mustAgent(t, st, mustUser(t, st, "owner").ID, "agent")
	posted, err := svc.PostAsAccount(ctx, agent.ID, &compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: string(agent.Agent.HomeChannelID)},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: "home topic"},
		CreateTopic: true, Blocks: textBlocks("home"),
	})
	if err != nil {
		t.Fatalf("PostAsAccount: %v", err)
	}
	resp, err := svc.ListTopicsAsAccountByName(ctx, agent.ID, &compassv1.ListTopicsRequest{})
	if err != nil {
		t.Fatalf("ListTopicsAsAccountByName(home): %v", err)
	}
	if len(resp.GetTopics()) != 1 || resp.GetTopics()[0].GetId() != posted.GetMessage().GetTopicId() {
		t.Fatalf("home topics = %+v, want posted topic %q", resp.GetTopics(), posted.GetMessage().GetTopicId())
	}
	if got := resp.GetTopics()[0].GetMessageCount(); got != 1 {
		t.Fatalf("home topic message_count = %d, want 1", got)
	}
}

func TestListTopicsAsAccountByNameResolvesNamedChannel(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	ch, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name: "war-room", Kind: store.ChannelKindChannel, MemberAccountIDs: []store.AccountID{agent.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	posted, err := svc.PostAsAccount(ctx, agent.ID, &compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: "deploy"},
		CreateTopic: true, Blocks: textBlocks("deployment"),
	})
	if err != nil {
		t.Fatalf("PostAsAccount: %v", err)
	}
	resp, err := svc.ListTopicsAsAccountByName(ctx, agent.ID, &compassv1.ListTopicsRequest{ChannelId: "war-room"})
	if err != nil {
		t.Fatalf("ListTopicsAsAccountByName(named): %v", err)
	}
	if len(resp.GetTopics()) != 1 || resp.GetTopics()[0].GetId() != posted.GetMessage().GetTopicId() {
		t.Fatalf("named topics = %+v, want posted topic %q", resp.GetTopics(), posted.GetMessage().GetTopicId())
	}
	if got := resp.GetTopics()[0].GetMessageCount(); got != 1 {
		t.Fatalf("topic message_count = %d, want 1", got)
	}
	if got := resp.GetTopics()[0].GetLastMessageAtUnixMs(); got != posted.GetMessage().GetAtUnixMs() {
		t.Fatalf("topic last_message_at = %d, want %d", got, posted.GetMessage().GetAtUnixMs())
	}
}

func TestListTopicsAsAccountByNameRejectsNonMember(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	if _, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{Name: "private", Kind: store.ChannelKindChannel}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	_, err := svc.ListTopicsAsAccountByName(ctx, agent.ID, &compassv1.ListTopicsRequest{ChannelId: "private"})
	connectCodeIs(t, err, connect.CodeNotFound, "ListTopicsAsAccountByName(non-member)")
}

func TestListTopicsAsAccountByNameHonorsIncludeArchived(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	posted, err := svc.PostAsAccount(ctx, agent.ID, &compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: string(agent.Agent.HomeChannelID)},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: "archived"},
		CreateTopic: true, Blocks: textBlocks("old"),
	})
	if err != nil {
		t.Fatalf("PostAsAccount: %v", err)
	}
	archived := true
	if _, err := svc.UpdateTopic(WithActor(ctx, agent.ID), connect.NewRequest(&compassv1.UpdateTopicRequest{
		TopicId: posted.GetMessage().GetTopicId(), Archived: &archived,
	})); err != nil {
		t.Fatalf("UpdateTopic(archive): %v", err)
	}
	for _, tc := range []struct {
		name            string
		includeArchived bool
		wantCount       int
	}{{name: "default excludes", wantCount: 0}, {name: "include archived", includeArchived: true, wantCount: 1}} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := svc.ListTopicsAsAccountByName(ctx, agent.ID, &compassv1.ListTopicsRequest{IncludeArchived: tc.includeArchived})
			if err != nil {
				t.Fatalf("ListTopicsAsAccountByName: %v", err)
			}
			if len(resp.GetTopics()) != tc.wantCount {
				t.Fatalf("topics = %d, want %d", len(resp.GetTopics()), tc.wantCount)
			}
			if tc.wantCount == 1 && !resp.GetTopics()[0].GetArchived() {
				t.Fatal("included topic is not marked archived")
			}
			if tc.wantCount == 1 && (resp.GetTopics()[0].MessageCount == nil || resp.GetTopics()[0].LastMessageAtUnixMs == nil) {
				t.Fatal("included topic lacks computed stats")
			}
		})
	}
}
