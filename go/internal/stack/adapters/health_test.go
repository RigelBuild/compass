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
)

// infoService answers GetServerInfo with a configured version and enrolled ids.
type infoService struct {
	compassv1connect.UnimplementedCompassServiceHandler
	version           string
	enrolledRunnerIDs []string
}

func (s *infoService) GetServerInfo(_ context.Context, _ *connect.Request[compassv1.GetServerInfoRequest]) (*connect.Response[compassv1.GetServerInfoResponse], error) {
	return connect.NewResponse(&compassv1.GetServerInfoResponse{Version: s.version, EnrolledRunnerIds: s.enrolledRunnerIDs}), nil
}

// serveInfo stands up a real CompassService over h2c on a unix socket in a temp
// dir and returns the socket path. The server is torn down via t.Cleanup.
func serveInfo(t *testing.T, version string, enrolledRunnerIDs ...string) string {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "server.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen unix = %v", err)
	}

	mux := http.NewServeMux()
	path, handler := compassv1connect.NewCompassServiceHandler(&infoService{version: version, enrolledRunnerIDs: enrolledRunnerIDs})
	mux.Handle(path, handler)

	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: protocols}

	go func() { _ = srv.Serve(listener) }() // Serve returns ErrServerClosed on Shutdown; not actionable in-test.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx) // best-effort teardown; nothing actionable on test cleanup.
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
	sockPath := serveInfo(t, "9.9.9", "runner-a", "runner-b")

	info, err := NewHealthProber().Probe(t.Context(), sockPath)
	if err != nil {
		t.Fatalf("Probe = %v, want nil", err)
	}
	if info.Version != "9.9.9" {
		t.Fatalf("Version = %q, want %q", info.Version, "9.9.9")
	}
	want := []string{"runner-a", "runner-b"}
	if len(info.EnrolledRunnerIDs) != len(want) || info.EnrolledRunnerIDs[0] != want[0] || info.EnrolledRunnerIDs[1] != want[1] {
		t.Fatalf("EnrolledRunnerIDs = %v, want %v", info.EnrolledRunnerIDs, want)
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
	if info.Version != "" || len(info.EnrolledRunnerIDs) != 0 {
		t.Fatalf("ServerInfo = %+v, want zero value on error", info)
	}
}
