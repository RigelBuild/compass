package usage_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
	"github.com/RigelBuild/compass/go/internal/usage"
	"github.com/RigelBuild/compass/go/internal/usage/usagetest"
)

func TestMemory(t *testing.T) {
	usagetest.Run(t, usagetest.Harness{
		New: func(*testing.T) usage.Store { return usage.NewMemory() },
		Ctx: func(t *testing.T, tenant int) context.Context {
			t.Helper()
			return store.WithTenant(t.Context(), store.TenantID(fmt.Sprint("t", tenant)))
		},
		CorruptRollups: func(_ *testing.T, s usage.Store) { usage.ClearMemoryRollups(s) },
		AppendComputeInterval: func(t *testing.T, s usage.Store, ctx context.Context, intervals ...usage.ComputeInterval) {
			t.Helper()
			memory := s.(*usage.Memory)
			for _, interval := range intervals {
				if err := memory.AppendComputeInterval(ctx, interval); err != nil {
					t.Fatalf("AppendComputeInterval: %v", err)
				}
			}
		},
	})
}
