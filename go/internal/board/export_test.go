//go:build unix

package board

import (
	"testing"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

type BusPayload = busPayload

// NewBoardForTest exposes the existing board test setup to external integration tests.
func NewBoardForTest(t *testing.T) (*Projection, *events.Bus[BusPayload]) {
	t.Helper()
	return newBoard(t)
}

// RecvStatusForTest exposes the existing event assertion helper to external tests.
func RecvStatusForTest(t *testing.T, ch <-chan events.Stamped[BusPayload]) *compassv1.AgentSessionStatus {
	t.Helper()
	return recvStatus(t, ch)
}
