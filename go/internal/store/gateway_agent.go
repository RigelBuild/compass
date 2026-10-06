package store

import (
	"context"
	"fmt"
)

// GatewayAgentTenant uses the system role to resolve agent tenants across RLS
// boundaries. It is the sole cross-tenant lookup for the gateway credential door.
func (s *Store) GatewayAgentTenant(ctx context.Context, agent AccountID) (TenantID, error) {
	if agent == "" {
		return "", fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}

	tenant, err := s.q.GatewayAgentTenant(WithSystemRole(ctx), string(agent))
	if err != nil {
		if noRows(err) {
			return "", fmt.Errorf("%w: gateway agent", ErrNotFound)
		}
		return "", fmt.Errorf("store: gateway agent tenant: %w", err)
	}
	return TenantID(tenant), nil
}
