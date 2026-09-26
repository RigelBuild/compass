//go:build pgtest && unix

package server

// The admin door's handle resolver must report a store fault as Internal. A
// fault mapped to NotFound would tell an operator that a real account is missing.

import (
	"context"
	"log/slog"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// TestHandleLookupStoreFaultIsInternal drives each admin-door handle resolver
// against a closed store, so the first lookup (the owner or user handle) fails
// with a fault rather than a miss. The hub is live so the handlers reach resolution.
//
// Mutation: mapping every resolver error to handleNotFound in handleLookupError
// turns each call into CodeNotFound.
func TestHandleLookupStoreFaultIsInternal(t *testing.T) {
	ctx := context.Background() // test root context
	st, err := store.Open(ctx, pgtest.RequireDSN(t))
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	if _, err := st.CreateAgent(ctx, admin.ID, store.NewAgent{Handle: "atlas", DisplayName: "Atlas"}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	brd := board.NewProjection(bus)
	tail := newSessionTail()
	hub := newRunnerHub(st, brd, tail, nil, slog.New(slog.DiscardHandler))
	svc := newService("test", bus, st, hub, brd, nil, tail)

	// The accounts exist, so only the closed pool can fail the lookups.
	st.Close()

	calls := map[string]func() error{
		"SpawnAgent admin/atlas": func() error {
			_, err := svc.SpawnAgent(ctx, connect.NewRequest(&compassv1.SpawnAgentRequest{AgentHandle: "admin/atlas"}))
			return err
		},
		"ProvisionAgentWorkspace admin/atlas": func() error {
			_, err := svc.ProvisionAgentWorkspace(ctx, connect.NewRequest(&compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "admin/atlas"}))
			return err
		},
		"IssueToken admin/atlas": func() error {
			_, err := svc.IssueToken(ctx, connect.NewRequest(&compassv1.IssueTokenRequest{AccountHandle: "admin/atlas"}))
			return err
		},
		"IssueToken admin": func() error {
			_, err := svc.IssueToken(ctx, connect.NewRequest(&compassv1.IssueTokenRequest{AccountHandle: "admin"}))
			return err
		},
	}
	for name, call := range calls {
		if code := connect.CodeOf(call()); code != connect.CodeInternal {
			t.Errorf("%s on a closed store: code = %v, want CodeInternal", name, code)
		}
	}
}
