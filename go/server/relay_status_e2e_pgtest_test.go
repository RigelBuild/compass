//go:build pgtest && unix

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"connectrpc.com/connect/v2"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/fabric"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// TestAgentGatewayPreservesUpstreamRelayStatus pins the AgentGateway socket's
// real Connect wire: upstream status codes and messages survive the Runner and
// the gateway unchanged. The three relay classes are protocol contracts, not
// locally generated gateway statuses.
func TestAgentGatewayPreservesUpstreamRelayStatus(t *testing.T) {
	w := newE2EWire(t)
	ctx := w.ctx

	// The session stays live on the socket while each upstream relay outcome
	// changes independently underneath it.
	sessionID, ok := w.hub.SessionForAccount(ctx, w.supervisor.ID)
	if !ok {
		t.Fatal("supervisor has no live session")
	}
	if _, err := w.supervisorClient.Comms(ctx, &compassv1internal.CommsCallRequest{
		CallId: "relay-status-bound",
		Call:   &compassv1internal.CommsCallRequest_List{List: &compassv1.ListMessagesRequest{}},
	}); err != nil {
		t.Fatalf("Comms on bound supervisor: %v", err)
	}

	// The real store has no object backend in this fixture. RelaySessionBlob
	// maps its FailedPrecondition sentinel to Unimplemented; the gateway socket
	// must preserve that upstream code and message.
	data := []byte("relay status blob")
	digest := sha256.Sum256(data)
	_, unimplementedErr := w.supervisorClient.PutSessionBlob(ctx, &compassv1internal.PutSessionBlobRequest{
		Sha256: hex.EncodeToString(digest[:]),
		Data:   data,
	})
	assertRelayStatus(t, unimplementedErr, connect.CodeUnimplemented, "store: failed precondition: session blob object store is not configured")

	// Evict the hub's live binding while leaving the agent's socket/session up.
	// The next Runner relay therefore reaches the upstream FailedPrecondition.
	if err := w.store.DeleteSessionBinding(ctx, sessionID); err != nil {
		t.Fatalf("DeleteSessionBinding: %v", err)
	}
	w.hub.OnBindingChange(fabric.BindingChange{SessionID: sessionID})
	_, failedPreconditionErr := w.supervisorClient.Comms(ctx, &compassv1internal.CommsCallRequest{
		CallId: "relay-status-unbound",
		Call:   &compassv1internal.CommsCallRequest_List{List: &compassv1.ListMessagesRequest{}},
	})
	assertRelayStatus(t, failedPreconditionErr, connect.CodeFailedPrecondition, "runnerhub: caller session not bound to an agent account")

	// The request-scoped binding lookup now has a real injected store fault. The
	// hub cache was evicted above, so the next call must consult this read-through.
	w.hub.SetSessionBindingStore(unavailableSessionBindingStore{SessionBindingStore: w.store})
	_, unavailableErr := w.supervisorClient.Comms(ctx, &compassv1internal.CommsCallRequest{
		CallId: "relay-status-unavailable",
		Call:   &compassv1internal.CommsCallRequest_List{List: &compassv1.ListMessagesRequest{}},
	})
	assertRelayStatus(t, unavailableErr, connect.CodeUnavailable, "runnerhub: caller session binding cannot be verified")
}

type unavailableSessionBindingStore struct {
	runnerhub.SessionBindingStore
}

func (unavailableSessionBindingStore) SessionBindingTenant(context.Context, string, string) (store.TenantID, error) {
	return "", errors.New("injected session-binding lookup failure")
}

func assertRelayStatus(t *testing.T, err error, code connect.Code, message string) {
	t.Helper()
	if err == nil {
		t.Fatalf("gateway status = nil, want %s: %q", code, message)
	}
	if got := connect.CodeOf(err); got != code {
		t.Fatalf("gateway status code = %s, want %s (error %v)", got, code, err)
	}
	want := connect.NewError(code, message)
	got, ok := errors.AsType[*connect.Error](err)
	if !ok || got.Message() != want.Message() {
		t.Fatalf("gateway status message = %q, want %q", err.Error(), want.Error())
	}
}
