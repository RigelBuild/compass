package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// oversizedAckFrame is a ControlAck whose packed applied_above is one-byte varints,
// the cheapest wire shape per decoded uint64, just past maxControlAckBytes.
func oversizedAckFrame() *compassv1internal.AgentFrame {
	above := make([]uint64, maxControlAckBytes)
	for i := range above {
		above[i] = 1
	}
	return controlAckFrame(0, above)
}

// A hostile agent's ack under the 16MiB read cap but over the ack bound fails the
// Publish stream before AckControl runs.
// RED without the codec on the mount: the ack decodes and reaches the router.
func TestPublishRejectsOversizedControlAck(t *testing.T) {
	capture := newCapturePublish()
	events := newRunnerServiceServer(t, capture)
	g := NewGateway(context.Background(), "cont-1", Deps{Sessions: boundSessions(), Events: events})
	router := &fakeControlRouter{}
	g.SetControlRouter(router)
	client := newAgentGatewayServer(t, g)

	stream := client.Publish(context.Background())
	// Send may not surface the read rejection; CloseAndReceive is authoritative.
	if err := stream.Send(&compassv1internal.PublishFrameRequest{Frame: oversizedAckFrame()}); err != nil && !errors.Is(err, context.Canceled) {
		t.Logf("send oversized ack: %v", err)
	}
	if _, err := stream.CloseAndReceive(); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("CloseAndReceive err = %v, want an InvalidArgument decode rejection", err)
	}
	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.ackCalls) != 0 {
		t.Fatalf("AckControl called %d times for an oversized ack, want 0", len(router.ackCalls))
	}
}

// The pre-scan must count every ControlAck occurrence: proto merges repeated
// message fields, so an ack split across frame fields, or across ack fields within
// one frame, decodes whole. Both AgentFrame-carrying request types are checked.
func TestAckBoundCodecCountsSplitOccurrences(t *testing.T) {
	half := make([]uint64, maxControlAckBytes/2+1)
	for i := range half {
		half[i] = 1
	}
	ack, err := proto.Marshal(&compassv1internal.ControlAck{AppliedAbove: half})
	if err != nil {
		t.Fatalf("marshal ack: %v", err)
	}
	ackField := protowire.AppendBytes(protowire.AppendTag(nil, agentFrameAckField, protowire.BytesType), ack)
	frameWith := func(frame []byte) []byte {
		return protowire.AppendBytes(protowire.AppendTag(nil, requestFrameField, protowire.BytesType), frame)
	}
	oneAckFrame := frameWith(ackField)
	cases := map[string][]byte{
		"split across frame fields": append(append([]byte{}, oneAckFrame...), oneAckFrame...),
		"split within one frame":    frameWith(append(append([]byte{}, ackField...), ackField...)),
	}
	for name, data := range cases {
		for _, msg := range []proto.Message{
			&compassv1internal.PublishFrameRequest{},
			&compassv1internal.PostConversationFrameRequest{},
		} {
			if err := (ackBoundCodec{}).Unmarshal(data, msg); !errors.Is(err, errControlAckTooLarge) {
				t.Errorf("%s into %T: err = %v, want errControlAckTooLarge", name, msg, err)
			}
		}
	}
}

// An honest worst case, a full-retention legacy list of large seqs, still decodes.
func TestAckBoundCodecAcceptsHonestAck(t *testing.T) {
	above := make([]uint64, maxRetainedOps)
	for i := range above {
		above[i] = ^uint64(0) - uint64(i)
	}
	data, err := proto.Marshal(&compassv1internal.PublishFrameRequest{Frame: controlAckFrame(^uint64(0), above)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got compassv1internal.PublishFrameRequest
	if err := (ackBoundCodec{}).Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal(honest ack) = %v", err)
	}
	if n := len(got.GetFrame().GetControlAck().GetAppliedAbove()); n != maxRetainedOps {
		t.Fatalf("decoded %d seqs, want %d", n, maxRetainedOps)
	}
}

func TestAckBoundCodecAcceptsBinaryPutSessionBlob(t *testing.T) {
	want := &compassv1internal.PutSessionBlobRequest{
		Sha256: "abababababababababababababababababababababababababababababababab",
		Data:   []byte{0, 1, 0xfe, 0xff},
	}
	wire, err := (ackBoundCodec{}).Marshal(want)
	if err != nil {
		t.Fatalf("Marshal(PutSessionBlobRequest) = %v", err)
	}
	var got compassv1internal.PutSessionBlobRequest
	if err := (ackBoundCodec{}).Unmarshal(wire, &got); err != nil {
		t.Fatalf("Unmarshal(PutSessionBlobRequest) = %v", err)
	}
	if !proto.Equal(&got, want) {
		t.Fatalf("binary round trip = %v, want %v", &got, want)
	}
	if _, err := (frameJSONCodec{name: "json"}).Marshal(want); err != nil {
		t.Fatalf("blob request JSON marshal = %v, want success", err)
	}
}

// JSON would bypass the binary pre-scan, so the mount refuses it outright.
// RED without the JSON codec overrides: protojson decodes the ack and routes it.
func TestAgentGatewayRejectsJSON(t *testing.T) {
	capture := newCapturePublish()
	events := newRunnerServiceServer(t, capture)
	g := NewGateway(context.Background(), "cont-1", Deps{Sessions: boundSessions(), Events: events})
	router := &fakeControlRouter{}
	g.SetControlRouter(router)
	path, handler := compassv1internalconnect.NewAgentGatewayHandler(g, agentGatewayHandlerOptions()...)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	client := compassv1internalconnect.NewAgentGatewayClient(h2cHTTPClient(t), srv.URL, connect.WithProtoJSON())

	stream := client.Publish(context.Background())
	if err := stream.Send(&compassv1internal.PublishFrameRequest{Frame: controlAckFrame(1, []uint64{2})}); err != nil {
		t.Logf("send over JSON: %v", err)
	}
	if _, err := stream.CloseAndReceive(); err == nil {
		t.Fatal("JSON Publish accepted, want a decode rejection")
	}
	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.ackCalls) != 0 {
		t.Fatalf("AckControl called %d times over JSON, want 0", len(router.ackCalls))
	}
}

// The pre-scan must not allocate per wire field: millions of empty frame fields
// fit under the read cap.
func TestControlAckBytesDoesNotAllocate(t *testing.T) {
	data := make([]byte, 0, 2*100_000)
	for range 100_000 {
		data = protowire.AppendTag(data, requestFrameField, protowire.BytesType)
		data = protowire.AppendVarint(data, 0)
	}
	if allocs := testing.AllocsPerRun(5, func() { controlAckBytes(data) }); allocs != 0 {
		t.Fatalf("controlAckBytes allocated %.0f times over empty fields, want 0", allocs)
	}
}

// Both JSON content types Connect registers by default refuse a frame request.
// RED without either override: protojson decodes and the handler answers instead.
func TestAgentGatewayRejectsJSONContentTypes(t *testing.T) {
	g := NewGateway(context.Background(), "cont-1", Deps{Sessions: boundSessions()})
	path, handler := compassv1internalconnect.NewAgentGatewayHandler(g, agentGatewayHandlerOptions()...)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, contentType := range []string{"application/json", "application/json; charset=utf-8"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			srv.URL+compassv1internalconnect.AgentGatewayPostConversationFrameProcedure,
			strings.NewReader(`{"frame":{"controlAck":{"appliedAbove":["1"]}}}`))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", contentType)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: post: %v", contentType, err)
		}
		body, err := io.ReadAll(resp.Body)
		if cerr := resp.Body.Close(); cerr != nil {
			t.Fatalf("%s: close body: %v", contentType, cerr)
		}
		if err != nil {
			t.Fatalf("%s: read body: %v", contentType, err)
		}
		if !strings.Contains(string(body), errJSONUnsupported.Error()) {
			t.Errorf("%s: status %d body %s, want the binary-only rejection", contentType, resp.StatusCode, body)
		}
	}
}

// Non-frame unaries keep JSON: the in-guest microVM probe calls Comms as a JSON
// POST. RED if the JSON override refuses every request type.
func TestAgentGatewayKeepsJSONForNonFrameCalls(t *testing.T) {
	// Unbound: Comms answers PermissionDenied after decoding, with no relay needed.
	g := NewGateway(context.Background(), "cont-1", Deps{Sessions: staticSessions{}})
	path, handler := compassv1internalconnect.NewAgentGatewayHandler(g, agentGatewayHandlerOptions()...)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		srv.URL+compassv1internalconnect.AgentGatewayCommsProcedure, strings.NewReader(`{"callId":"c1"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); cerr != nil {
		t.Fatalf("close body: %v", cerr)
	}
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "permission_denied") {
		t.Fatalf("JSON Comms: status %d body %s, want the post-decode permission_denied", resp.StatusCode, body)
	}
}
