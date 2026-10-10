//go:build unix

package adapters

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/stack"
)

// infoService answers GetServerInfo with configured Runner enrollments.
type infoService struct {
	compassv1connect.UnimplementedCompassServiceHandler
	version         string
	enrolledRunners []*compassv1.EnrolledRunner
}

func (s *infoService) GetServerInfo(_ context.Context, _ *connect.Request[compassv1.GetServerInfoRequest]) (*connect.Response[compassv1.GetServerInfoResponse], error) {
	return connect.NewResponse(&compassv1.GetServerInfoResponse{Version: s.version, EnrolledRunners: s.enrolledRunners}), nil
}

// serveInfo serves GetServerInfo over a temporary Unix socket.
func serveInfo(t *testing.T, version string, enrolledRunners ...*compassv1.EnrolledRunner) string {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "server.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen unix = %v", err)
	}
	mux := http.NewServeMux()
	path, handler := compassv1connect.NewCompassServiceHandler(&infoService{version: version, enrolledRunners: enrolledRunners})
	mux.Handle(path, handler)
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: protocols}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx) // Test cleanup: no caller can act on shutdown errors.
	})
	return sockPath
}

func TestProbeLiveServerReturnsVersion(t *testing.T) {
	sockPath := serveInfo(t, "9.9.9")

	info, err := NewHealthProber().Probe(t.Context(), sockPath)
	if err != nil {
		t.Fatalf("Probe = %v, want nil", err)
	}
	if info.Version != "9.9.9" {
		t.Fatalf("Version = %q, want %q", info.Version, "9.9.9")
	}
}
func TestProbeLiveServerReturnsVersionAndRunnerEnrollments(t *testing.T) {
	sockPath := serveInfo(t, "9.9.9", &compassv1.EnrolledRunner{Id: "runner-a", Enrollment: 3}, &compassv1.EnrolledRunner{Id: "runner-b", Enrollment: 8})

	info, err := NewHealthProber().Probe(t.Context(), sockPath)
	if err != nil {
		t.Fatalf("Probe = %v, want nil", err)
	}
	if info.Version != "9.9.9" {
		t.Fatalf("Version = %q, want %q", info.Version, "9.9.9")
	}
	want := []stack.EnrolledRunner{{ID: "runner-a", Enrollment: 3}, {ID: "runner-b", Enrollment: 8}}
	if len(info.EnrolledRunners) != len(want) {
		t.Fatalf("EnrolledRunners = %v, want %v", info.EnrolledRunners, want)
	}
	for i := range want {
		if info.EnrolledRunners[i] != want[i] {
			t.Fatalf("EnrolledRunners = %v, want %v", info.EnrolledRunners, want)
		}
	}
}

func TestProbeNoServerReturnsError(t *testing.T) {
	// A path under a temp dir with nothing listening: the dial fails
	// deterministically (ENOENT / connection refused), no timing race.
	deadPath := filepath.Join(t.TempDir(), "absent.sock")

	info, err := NewHealthProber().Probe(t.Context(), deadPath)
	if err == nil {
		t.Fatalf("Probe against dead socket = nil error, want non-nil (readiness 'not yet')")
	}
	if info.Version != "" || len(info.EnrolledRunners) != 0 {
		t.Fatalf("ServerInfo = %+v, want zero value on error", info)
	}
}
