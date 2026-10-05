//go:build unix

package runner

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// BindLifetime asks the Server to bind sessionID's transcript rebase base for a
// new lifetime in containerName. The host calls it under the container lock after
// accepting a resume or reload and before the agent runs.
func (l *ServerLink) BindLifetime(ctx context.Context, containerName, sessionID string) error {
	_, err := l.client.BindLifetime(ctx, connect.NewRequest(&compassv1internal.BindLifetimeRequest{
		ContainerName: containerName,
		SessionId:     sessionID,
	}))
	if err != nil {
		return fmt.Errorf("binding lifetime for session %q in container %q: %w", sessionID, containerName, err)
	}
	return nil
}
