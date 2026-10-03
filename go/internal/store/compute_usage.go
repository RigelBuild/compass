package store

import (
	"context"
	"fmt"
)

// CloseOrphanedComputeIntervals closes intervals whose binding disappeared
// without its matching end event. The system role lets one pass cover all tenants.
func (s *Store) CloseOrphanedComputeIntervals(ctx context.Context) (int64, error) {
	closed, err := s.q.CloseOrphanedComputeIntervals(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: close orphaned compute intervals: %w", err)
	}
	return closed, nil
}
