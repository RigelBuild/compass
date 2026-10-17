//go:build unix

package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
)

func TestStartAgentSessionNoRunnerOverHTTP(t *testing.T) {
	hub := runnerhub.NewHub(nil, nil, nil, nil)
	svc := newService("contract-test", nil, nil, hub, nil, nil, nil)
	client := newH2CClient(t, newH2CTestServer(t, svc))

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	_, err := client.StartAgentSession(ctx, &compassv1.StartAgentSessionRequest{
		ContainerName: "container-without-runner",
	})
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("StartAgentSession code = %v, want Unavailable (error %v)", got, err)
	}
	connectErr, ok := errors.AsType[*connect.Error](err)
	if !ok {
		t.Fatalf("StartAgentSession error %T = %v, want *connect.Error", err, err)
	}
	if !strings.Contains(connectErr.Message(), runnerhub.ErrMsgNoRunnerEnrolled) {
		t.Fatalf("StartAgentSession message = %q, want it to contain %q", connectErr.Message(), runnerhub.ErrMsgNoRunnerEnrolled)
	}
}
