//go:build unix

package server

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/store"
)

// runnerVerifierStore is the store surface the Runner-cluster startup needs.
type runnerVerifierStore interface {
	BootstrapTenant(ctx context.Context) (store.TenantID, error)
	CountRunnerTokenIDsWithSlash(ctx context.Context) (int64, error)
}

// buildRunnerVerifier loads cfg.RunnerClustersPath into a verifier, or returns
// nil when no clusters are registered. The caller starts it under the serve ctx.
func buildRunnerVerifier(ctx context.Context, cfg ServeConfig, st runnerVerifierStore) (*auth.RunnerVerifier, error) {
	if cfg.RunnerClustersPath == "" {
		return nil, nil //nolint:nilnil // nil verifier means projected tokens are off
	}
	data, err := os.ReadFile(cfg.RunnerClustersPath)
	if err != nil {
		return nil, fmt.Errorf("reading runner clusters: %w", err)
	}
	clusters, err := auth.ParseRunnerClusters(data)
	if err != nil {
		return nil, err
	}
	// A pre-reservation minted ID with "/" could equal a node's ID, so refuse to
	// let the two kinds share an identity.
	n, err := st.CountRunnerTokenIDsWithSlash(ctx)
	if err != nil {
		return nil, fmt.Errorf("checking minted runner ids: %w", err)
	}
	if n > 0 {
		return nil, fmt.Errorf("runner clusters configured but %d live minted runner token(s) have a %q in the runner id; revoke them before enabling projected tokens", n, "/")
	}
	tenant, err := st.BootstrapTenant(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolving runner tenant: %w", err)
	}
	v, err := auth.NewRunnerVerifier(clusters, tenant, nil, time.Now)
	if err != nil {
		return nil, fmt.Errorf("building runner verifier: %w", err)
	}
	return v, nil
}
