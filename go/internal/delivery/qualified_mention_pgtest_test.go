//go:build pgtest && unix

package delivery

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestQualifiedMentionRoutesWithinTenant(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.RequireDSN(t)
	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(s.Close)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to seed tenant: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close tenant seed connection: %v", err)
		}
	})
	tenantB := store.TenantID("tenant-b")
	if _, err := conn.Exec(ctx,
		"INSERT INTO tenants (id, slug, display_name, created_at_unix_ms) VALUES ($1, $2, $3, $4)",
		string(tenantB), string(tenantB), string(tenantB), time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}

	tenantA := s.EffectiveTenant(ctx)
	ctxA := store.WithTenant(ctx, tenantA)
	ctxB := store.WithTenant(ctx, tenantB)
	ownerA, err := s.CreateUser(ctxA, store.NewUser{Handle: "bob", DisplayName: "Bob A"})
	if err != nil {
		t.Fatalf("CreateUser(tenant A): %v", err)
	}
	ownerB, err := s.CreateUser(ctxB, store.NewUser{Handle: "bob", DisplayName: "Bob B"})
	if err != nil {
		t.Fatalf("CreateUser(tenant B): %v", err)
	}
	agentA, err := s.CreateAgent(ctxA, ownerA.ID, store.NewAgent{Handle: "x", DisplayName: "X A"})
	if err != nil {
		t.Fatalf("CreateAgent(tenant A): %v", err)
	}
	agentB, err := s.CreateAgent(ctxB, ownerB.ID, store.NewAgent{Handle: "x", DisplayName: "X B"})
	if err != nil {
		t.Fatalf("CreateAgent(tenant B): %v", err)
	}
	channelA, err := s.CreateChannel(ctxA, ownerA.ID, store.NewChannel{
		Name: "room", Kind: store.ChannelKindChannel, MemberAccountIDs: []store.AccountID{agentA.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel(tenant A): %v", err)
	}
	if _, _, err := s.UpdateChannelMembers(ctxA, ownerA.ID, channelA.ID,
		[]store.MemberUpdate{{AccountID: agentA.ID, Subscribed: true}}, store.MemberUpdatesOptions{}); err != nil {
		t.Fatalf("subscribe tenant A agent: %v", err)
	}
	if _, err := s.CreateChannel(ctxB, ownerB.ID, store.NewChannel{
		Name: "room", Kind: store.ChannelKindChannel, MemberAccountIDs: []store.AccountID{agentB.ID},
	}); err != nil {
		t.Fatalf("CreateChannel(tenant B): %v", err)
	}
	body := "@bob/x"
	message, _, err := s.AppendMessage(ctxA, store.Message{
		AuthorAccountID: ownerA.ID,
		Blocks:          []store.MessageBlock{{Text: &body}},
	}, string(channelA.ID), store.TopicRef{Name: "general", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	c, disp, res := newPgConsumer(t, s)
	res.bind(agentA.ID, "tenant-a-session")
	res.bind(agentB.ID, "tenant-b-session")
	startConsumer(t, c)
	fab := fakeFabricOf(c)
	fab.waitSubscribed(t)
	publishRef(t, ctxA, c, tenantA, string(message.ID))
	fab.waitAcked(t, string(message.ID))

	// The message predates the consumer, so the start sweep may steer it again;
	// delivery is at-least-once, so count targets rather than records.
	got := disp.snapshot()
	if len(got) == 0 {
		t.Fatalf("no dispatch for %s, want a steer to tenant A's bob/x", message.ID)
	}
	for _, rec := range got {
		if rec.sessionID != "tenant-a-session" || rec.kind != opSteer {
			t.Fatalf("dispatch = %+v, want only steers to tenant A's bob/x", rec)
		}
	}
	if len(recordsFor(got, "tenant-b-session")) != 0 {
		t.Fatalf("tenant B agent received a cross-tenant mention: %+v", got)
	}
}
