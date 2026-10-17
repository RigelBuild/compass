//go:build unix

package runner

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// sessionsResumeServer sends an authorized resume command after the Runner's
// empty bootstrap request and records the correlated result on the real stream.
type sessionsResumeServer struct {
	recordingEnroll
	resumeBody string
	results    chan *compassv1internal.SessionsRequest
}

func (s *sessionsResumeServer) Sessions(_ context.Context, stream *connect.BidiStream[compassv1internal.SessionsRequest, compassv1internal.SessionsResponse]) error {
	if _, err := stream.Receive(); err != nil {
		return fmt.Errorf("receive Runner bootstrap: %w", err)
	}
	if err := stream.Send(&compassv1internal.SessionsResponse{
		RequestId: "resume-over-default-limit",
		Command: &compassv1internal.SessionsResponse_Start{
			Start: &compassv1.StartAgentSessionRequest{
				ContainerName:   "container-1",
				ResumeSessionId: "session-1",
			},
		},
		ResumeBody: &compassv1internal.ResumeBody{SessionBody: s.resumeBody},
	}); err != nil {
		return fmt.Errorf("send resume command: %w", err)
	}
	result, err := stream.Receive()
	if err != nil {
		return fmt.Errorf("receive resume result: %w", err)
	}
	s.results <- result
	return nil
}

type resumeBodyCaptureHost struct {
	SessionHost
	bodies chan string
}

func (h *resumeBodyCaptureHost) Start(_ context.Context, _ *compassv1.StartAgentSessionRequest, resumeBody, _ string) (string, error) {
	h.bodies <- resumeBody
	return "session-1", nil
}

// RunSessions uses Dial's production RunnerService client. The resume body is
// larger than Connect v2's 4 MiB default and must reach SessionHost intact.
func TestRunSessionsAcceptsResumeBodyAbove4MiB(t *testing.T) {
	const resumeBodyBytes = 4<<20 + 1024
	resumeBody := strings.Repeat("resume-body-", (resumeBodyBytes+len("resume-body-")-1)/len("resume-body-"))[:resumeBodyBytes]
	serverHandler := &sessionsResumeServer{
		resumeBody: resumeBody,
		results:    make(chan *compassv1internal.SessionsRequest, 1),
	}
	path, handler := compassv1internalconnect.NewRunnerServiceHandler(serverHandler)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewUnstartedServer(mux)
	server.Config.Protocols = cleartextHTTP2()
	server.Start()
	t.Cleanup(server.Close)

	link, err := Dial(t.Context(), RunnerConfig{
		ServerAddr: server.URL,
		Token:      StaticToken("test-token"),
		HTTPClient: h2cHTTPClient(t),
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	host := &resumeBodyCaptureHost{bodies: make(chan string, 1)}
	if err := link.RunSessions(t.Context(), host, discardLoggerRunner()); err != nil {
		t.Fatalf("RunSessions with resume body of %d bytes: %v", len(resumeBody), err)
	}
	if got := <-host.bodies; got != resumeBody {
		t.Fatalf("SessionHost received resume body of %d bytes, want the exact %d-byte body", len(got), len(resumeBody))
	}
	result := <-serverHandler.results
	if result.GetRequestId() != "resume-over-default-limit" || result.GetStart().GetSessionId() != "session-1" {
		t.Fatalf("resume result = %v, want request id resume-over-default-limit and session id session-1", result)
	}
}
