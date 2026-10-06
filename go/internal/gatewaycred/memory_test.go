package gatewaycred_test

import (
	"context"
	"testing"

	"github.com/RigelBuild/compass/go/internal/gatewaycred"
	"github.com/RigelBuild/compass/go/internal/gatewaycred/gatewaycredtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestMemoryContract(t *testing.T) {
	var memory *gatewaycred.Memory
	gatewaycredtest.Run(t, gatewaycredtest.Harness{
		New: func(t *testing.T) (gatewaycred.CredentialStore, gatewaycred.PoolResolver) {
			t.Helper()
			memory = gatewaycred.NewMemory()
			return memory, memory
		},
		Ctx: func(_ *testing.T, tenant int) context.Context {
			return store.WithTenant(context.Background(), store.TenantID(string(rune('a'+tenant))))
		},
		Agent: func(t *testing.T, _ context.Context, name string) (store.AccountID, store.AccountID) {
			t.Helper()
			owner := store.AccountID("owner-" + name)
			agent := store.AccountID("agent-" + name)
			memory.SetAgentOwner(agent, owner)
			return agent, owner
		},
	})
}
