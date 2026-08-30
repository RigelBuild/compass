//go:build pgtest && unix

package comms

// The agent-TOOL comms entries (peer-DM R1/R2): PostAsAccountByName /
// ListAsAccountByName resolve the request's channel NAME to an id within the
// caller's visible set (ChannelByNameForViewer) and delegate to the id-typed
// PostAsAccount / ListAsAccount. Every test here defends one arm of the
// name-addressing contract the tool edge now enforces:
//
//   - a visible name resolves and the post/list lands in that channel;
//   - an unknown OR invisible name is CodeNotFound (the D9 merge — the agent
//     never learns a channel it cannot see exists);
//   - an ambiguous name is CodeInvalidArgument (there is no ErrAmbiguous);
//   - post/ask have NO home default: an empty channel name is CodeNotFound, not
//     a silent home fallback (R2);
//   - list KEEPS omit-=home: an empty channel name lists the caller's home.
//
// The id-typed PostAsAccount / ListAsAccount and defaultChannel are unchanged and
// covered by agent_caller_pgtest_test.go; these tests exercise only the ByName
// resolution layer above them.

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
