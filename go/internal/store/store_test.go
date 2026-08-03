//go:build pgtest

package store

// Store lifecycle contracts: durability across a process restart (the headline
// property of a store of record — a graph committed by one Store is read back
// identical by a second Store opened on the same database), idempotent migration
// re-runs (a second Open is a no-op, not a re-application), and the recorded
// schema version the refuse-to-serve guard reads. These are the properties an
// in-memory substitute could never prove, so they run against a real Postgres.

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// Shared setup helpers, defined once for the package's tests. They fail fast so
// a test body reads as the behavior under test, not its scaffolding.

// mustUser creates a human account or fails the test.
func mustUser(t *testing.T, s *Store, handle string) Account {
	t.Helper()
	u, err := s.CreateUser(context.Background(), NewUser{Handle: handle, DisplayName: handle})
	if err != nil {
		t.Fatalf("CreateUser(%q): %v", handle, err)
	}
	return u
}

// mustAgent creates an owned agent account or fails the test.
func mustAgent(t *testing.T, s *Store, owner AccountID, handle string) Account {
	t.Helper()
	a, err := s.CreateAgent(context.Background(), owner, NewAgent{Handle: handle, DisplayName: handle})
	if err != nil {
		t.Fatalf("CreateAgent(%q): %v", handle, err)
	}
	return a
}

// memberSubscribed reads the per-member subscribed flag directly, the one
// channel-membership invariant (RT-1 subscribe, RT-2 home-channel always-on)
// with no public read surface on Channel — so it is asserted through the pool.
func memberSubscribed(t *testing.T, s *Store, ch ChannelID, acct AccountID) bool {
	t.Helper()
	var sub bool
	err := s.pool.QueryRow(context.Background(),
		"SELECT subscribed FROM channel_members WHERE channel_id = $1 AND account_id = $2",
		string(ch), string(acct),
	).Scan(&sub)
	if err != nil {
		t.Fatalf("read subscribed for (%s,%s): %v", ch, acct, err)
	}
	return sub
}

// containsAccount reports whether ids includes want.
func containsAccount(ids []AccountID, want AccountID) bool {
	return slices.Contains(ids, want)
}

func TestRestartDurabilityReadsBackFullGraph(t *testing.T) {
	ctx := context.Background()
	s1, dsn := newTestStoreDSN(t)

	// Build a full graph through the first store: user, agent (mints home
	// channel), a shared group, a grouped channel with the agent as a member, a
	// message carrying both block variants, and a token.
	user := mustUser(t, s1, "matt")
	agent := mustAgent(t, s1, user.ID, "helper")
	homeCh := agent.Agent.HomeChannelID

	group, err := s1.CreateChannelGroup(ctx, user.ID, NewChannelGroup{Name: "proj", Visibility: VisibilityShared})
	if err != nil {
		t.Fatalf("CreateChannelGroup: %v", err)
	}
	channel, err := s1.CreateChannel(ctx, user.ID, NewChannel{
		Name: "coord", GroupID: group.ID, Kind: ChannelKindChannel,
		MemberAccountIDs: []AccountID{agent.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	wantBlocks := sampleBlocks()
	msg, _, err := s1.AppendMessage(ctx, Message{AuthorAccountID: user.ID, Blocks: wantBlocks}, string(channel.ID), TopicRef{Name: "general"}, "req-durable")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	hash := tokenHash("durable-token")
	if err := s1.PutTokenHash(ctx, hash, Subject{Kind: SubjectAccount, ID: string(user.ID)}); err != nil {
		t.Fatalf("PutTokenHash: %v", err)
	}

	// Simulate a restart: close the first store and open a fresh one on the same
	// database WITHOUT resetting. Everything committed above must survive.
	s1.Close()
	s2 := reopenStore(t, dsn)

	gotUser, err := s2.GetAccount(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetAccount(user) after restart: %v", err)
	}
	if gotUser.Handle != "matt" || gotUser.User == nil {
		t.Fatalf("user round-trip = %+v, want handle matt with User set", gotUser)
	}

	gotAgent, err := s2.GetAccount(ctx, agent.ID)
	if err != nil {
		t.Fatalf("GetAccount(agent) after restart: %v", err)
	}
	if gotAgent.Agent == nil {
		t.Fatalf("agent round-trip lost Agent subtype: %+v", gotAgent)
	}
	if gotAgent.Agent.HomeChannelID != homeCh {
		t.Fatalf("home channel = %q, want %q", gotAgent.Agent.HomeChannelID, homeCh)
	}
	if gotAgent.Agent.OwnerUserID != user.ID {
		t.Fatalf("owner = %q, want %q", gotAgent.Agent.OwnerUserID, user.ID)
	}

	// Home channel and its membership + always-subscribed flag survive.
	gotHome, err := s2.getChannel(ctx, homeCh)
	if err != nil {
		t.Fatalf("getChannel(home) after restart: %v", err)
	}
	if !containsAccount(gotHome.MemberAccountIDs, user.ID) || !containsAccount(gotHome.MemberAccountIDs, agent.ID) {
		t.Fatalf("home channel members = %v, want both %s and %s", gotHome.MemberAccountIDs, user.ID, agent.ID)
	}
	if !memberSubscribed(t, s2, homeCh, agent.ID) {
		t.Fatalf("agent not subscribed to its home channel after restart")
	}

	// Grouped channel and its expanded membership survive.
	gotChannel, err := s2.getChannel(ctx, channel.ID)
	if err != nil {
		t.Fatalf("getChannel(channel) after restart: %v", err)
	}
	for _, want := range []AccountID{user.ID, agent.ID} {
		if !containsAccount(gotChannel.MemberAccountIDs, want) {
			t.Fatalf("channel members = %v, missing %s", gotChannel.MemberAccountIDs, want)
		}
	}

	// The owning user still sees the shared group.
	groups, err := s2.ListChannelGroups(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListChannelGroups after restart: %v", err)
	}
	if !containsGroup(groups, group.ID) {
		t.Fatalf("group %s not listed after restart: %v", group.ID, groups)
	}

	// The message and its blocks round-trip byte-for-byte through JSONB.
	msgs, err := s2.ListMessages(ctx, ListMessagesQuery{Actor: user.ID, ChannelID: channel.ID, Page: Page{}})
	if err != nil {
		t.Fatalf("ListMessages after restart: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages after restart, want 1", len(msgs))
	}
	if msgs[0].ID != msg.ID {
		t.Fatalf("message id = %q, want %q", msgs[0].ID, msg.ID)
	}
	assertBlocksEqual(t, msgs[0].Blocks, wantBlocks)

	// The token resolves to the same subject, kind intact.
	subj, err := s2.ResolveTokenHash(ctx, hash)
	if err != nil {
		t.Fatalf("ResolveTokenHash after restart: %v", err)
	}
	if subj.Kind != SubjectAccount || subj.ID != string(user.ID) {
		t.Fatalf("resolved subject = %+v, want {Account, %s}", subj, user.ID)
	}
}

func TestOpenIsIdempotentAcrossRestart(t *testing.T) {
	ctx := context.Background()
	s1, dsn := newTestStoreDSN(t)

	// The first Open applied every embedded migration; count the bookkeeping
	// rows it recorded. A second Open on the same database must be a pure no-op:
	// migrate skips already-applied versions rather than re-running them (a
	// re-run would duplicate-PK-error and fail Open, or inflate this count).
	before := migrationRowCount(t, ctx, s1)
	if before < 1 {
		t.Fatalf("first Open recorded %d migration rows, want >= 1", before)
	}

	s2 := reopenStore(t, dsn) // reopen fatals if Open errors; success proves the no-op path
	after := migrationRowCount(t, ctx, s2)
	if after != before {
		t.Fatalf("migration rows changed on second Open: before=%d after=%d (not idempotent)", before, after)
	}
}

func TestSchemaVersionRecorded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// The refuse-to-serve guard compares the database's recorded schema version
	// against the binary's embedded set; Open having succeeded means they match.
	// The recorded version must be present and positive — a store with no
	// recorded version would fail the guard rather than serve.
	var version int
	if err := s.pool.QueryRow(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM schema_migrations",
	).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version < 1 {
		t.Fatalf("recorded schema version = %d, want >= 1", version)
	}
}

// migrationRowCount reads how many migrations the database records as applied.
func migrationRowCount(t *testing.T, ctx context.Context, s *Store) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	return n
}

// containsGroup reports whether groups includes the group id.
func containsGroup(groups []ChannelGroup, want ChannelGroupID) bool {
	for _, g := range groups {
		if g.ID == want {
			return true
		}
	}
	return false
}

// sentinelIs is a readability wrapper asserting err matches a store sentinel.
func sentinelIs(t *testing.T, err, want error, ctx string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: err = %v, want errors.Is(_, %v)", ctx, err, want)
	}
}

// isSentinel reports whether err matches a sentinel, for the distinctness check
// (a revoked token must NOT also match ErrNotFound).
func isSentinel(err, want error) bool {
	return errors.Is(err, want)
}
