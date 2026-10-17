//go:build unix

package gateway

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/otelconnect"
	"google.golang.org/protobuf/proto"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
)

const expectedMalformedPublishCode = "internal"

func newRawPublishGatewayServer(t *testing.T) *httptest.Server {
	t.Helper()
	upstream := newRunnerServiceServer(t, newCapturePublish())
	g := NewGateway(t.Context(), "container-1", Deps{Sessions: boundSessions(), Events: upstream})
	ic, err := otelconnect.NewServerInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewServerInterceptor: %v", err)
	}
	server := connect.NewServer(otelx.HandlerInterceptors(ic))
	compassv1internalconnect.RegisterAgentGatewayHandler(server, g)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server, agentGatewayHandlerOptions()...)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func publishEnvelope(payload []byte) []byte {
	body := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(body[1:5], uint32(len(payload)))
	copy(body[5:], payload)
	return body
}

type publishWireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func postRawPublish(t *testing.T, server *httptest.Server, body []byte) publishWireError {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		server.URL+compassv1internalconnect.AgentGatewayPublishProcedure, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new Publish request: %v", err)
	}
	request.Header.Set("Content-Type", "application/connect+proto")
	response, err := h2cHTTPClient(t).Do(request)
	if err != nil {
		t.Fatalf("post raw Publish body: %v", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read Publish response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Publish HTTP status = %d, want 200 with a Connect end-stream error; body %q", response.StatusCode, responseBody)
	}
	if len(responseBody) < 5 || responseBody[0]&0b10 == 0 {
		t.Fatalf("Publish response envelope = %x, want a Connect end-stream envelope", responseBody)
	}
	responseSize := int(binary.BigEndian.Uint32(responseBody[1:5]))
	if responseSize != len(responseBody)-5 {
		t.Fatalf("Publish end-stream envelope declares %d bytes, got %d", responseSize, len(responseBody)-5)
	}
	var end struct {
		Error *publishWireError `json:"error"`
	}
	if err := json.Unmarshal(responseBody[5:], &end); err != nil {
		t.Fatalf("decode Publish end-stream envelope: %v (body %q)", err, responseBody[5:])
	}
	if end.Error == nil {
		t.Fatalf("Publish end-stream = %s, want a wire error", responseBody[5:])
	}
	return *end.Error
}

func TestPublishOverLimitRawFrameReturnsResourceExhausted(t *testing.T) {
	frame := traceFrame(strings.Repeat("x", maxAgentMessageBytes))
	payload, err := proto.Marshal(&compassv1internal.PublishFrameRequest{Frame: frame})
	if err != nil {
		t.Fatalf("marshal oversized Publish frame: %v", err)
	}
	if len(payload) <= maxAgentMessageBytes {
		t.Fatalf("Publish payload size = %d, want above maxAgentMessageBytes %d", len(payload), maxAgentMessageBytes)
	}

	wireErr := postRawPublish(t, newRawPublishGatewayServer(t), publishEnvelope(payload))
	if wireErr.Code != "resource_exhausted" {
		t.Fatalf("Publish error code = %q, want resource_exhausted (message %q)", wireErr.Code, wireErr.Message)
	}
	if want := fmt.Sprintf("message size %d is larger than configured max %d", len(payload), maxAgentMessageBytes); wireErr.Message != want {
		t.Fatalf("Publish error message = %q, want %q", wireErr.Message, want)
	}
}

func TestPublishMalformedRawFramesReturnInvalidArgument(t *testing.T) {
	tests := []struct {
		name        string
		body        []byte
		wantMessage string
	}{
		{
			name:        "incomplete three-byte prefix",
			body:        []byte{0, 0, 0},
			wantMessage: "protocol error: incomplete envelope: unexpected EOF",
		},
		{
			name: "promised payload is short",
			body: func() []byte {
				body := make([]byte, 6)
				binary.BigEndian.PutUint32(body[1:5], 4)
				body[5] = 0x01
				return body
			}(),
			wantMessage: "protocol error: promised 4 bytes in enveloped message, got 1 bytes",
		},
	}

	server := newRawPublishGatewayServer(t)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wireErr := postRawPublish(t, server, test.body)
			if wireErr.Code != expectedMalformedPublishCode {
				t.Fatalf("Publish error code = %q, want %q (message %q)", wireErr.Code, expectedMalformedPublishCode, wireErr.Message)
			}
			if wireErr.Message != test.wantMessage {
				t.Fatalf("Publish error message = %q, want %q", wireErr.Message, test.wantMessage)
			}
		})
	}
}
