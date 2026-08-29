//go:build unix

package server

// The one lifecycleService branch that fails BEFORE any store or hub call, so it
// runs in the default lane with no Postgres: DespawnAsAccount's self-despawn
// refusal, which is checked on target==caller before AgentOwner is ever read.
// Every other branch (spawn ownership, resume, rollback, owner authz) reads the
// store, so it lives behind the pgtest tag in lifecycle_pgtest_test.go — the same
// split service_agentsession_test.go vs its pgtest sibling uses.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// TestDespawnSelfIsRefusedBeforeAnyStoreCall pins that an agent cannot despawn
// itself: DespawnAsAccount returns CodeInvalidArgument when the target equals the
// resolved caller, and it does so before touching the store or hub — proven by
// passing a nil store and nil hub, which any later call would nil-dereference.
//
// Mutation: dropping the target==caller guard (or ordering it AFTER the AgentOwner
// read) reddens this — a nil-store AgentOwner call panics rather than returning
// the clean invalid_argument.
func TestDespawnSelfIsRefusedBeforeAnyStoreCall(t *testing.T) {
	lc := newLifecycleService(nil, nil) // nil store + hub: any store/hub call would panic
	const self = store.AccountID("agent-self")

	_, err := lc.DespawnAsAccount(context.Background(), self, &compassv1internal.DespawnPeerRequest{AgentHandle: string(self)})
	if err == nil {
		t.Fatal("DespawnAsAccount(self) = nil error, want CodeInvalidArgument (no self-despawn)")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("DespawnAsAccount(self) code = %v, want CodeInvalidArgument", got)
	}
}
