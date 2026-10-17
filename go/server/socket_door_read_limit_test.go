//go:build pgtest && unix

package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/envelope"
)

func TestSocketDoorPutAgentConfigReadLimit(t *testing.T) {
	ctx := t.Context()
	st, adminID, _ := newNetworkStore(t)
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(commsBus.Close)
	brd := board.NewProjection(bus)
	commsSvc := comms.NewComms(ctx, st, commsBus, nil, adminID)
	tail := newSessionTail()
	hub := newRunnerHub(st, brd, tail, commsSvc, nil)
	svc := newService("socket-read-limit-test", bus, st, hub, brd, nil, tail)
	secretsSvc := newSecretsService(st, nil, nil, nil)
	doors, err := buildDoors(ctx, ServeConfig{StateDir: t.TempDir()}, svc, commsSvc, secretsSvc, hub, st,
		adminID, "", nil, nil, nil, nil, nil, nil, nil, nil, envelope.Key{}, 0)
	if err != nil {
		t.Fatalf("buildDoors: %v", err)
	}
	srv := httptest.NewUnstartedServer(doors.uds.Handler)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	client := newH2CClient(t, srv.URL)
	for _, tc := range []struct {
		name        string
		messageSize int
		wantCode    connect.Code
	}{
		{name: "at limit", messageSize: compassServiceMaxReadBytes},
		{name: "one byte over", messageSize: compassServiceMaxReadBytes + 1, wantCode: connect.CodeResourceExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := make([]byte, tc.messageSize-5)
			request := &compassv1.PutAgentConfigRequest{Bundle: bundle}
			if got := proto.Size(request); got != tc.messageSize {
				t.Fatalf("PutAgentConfigRequest proto.Size = %d, want %d", got, tc.messageSize)
			}
			callCtx, cancel := context.WithTimeout(ctx, testTimeout)
			defer cancel()
			_, err := client.PutAgentConfig(callCtx, connect.NewRequest(request))
			if tc.wantCode == connect.CodeResourceExhausted {
				if got := connect.CodeOf(err); got != tc.wantCode {
					t.Fatalf("PutAgentConfig code = %v, want %v (error %v)", got, tc.wantCode, err)
				}
				return
			}
			if got := connect.CodeOf(err); got == connect.CodeResourceExhausted {
				t.Fatalf("PutAgentConfig at the read limit returned ResourceExhausted: %v", err)
			}
		})
	}
}
