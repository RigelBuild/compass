//go:build unix

package server

// Proxied spellings of the exempt Runner stream paths must not be classified exempt:
// each near-miss has to reach the handler with the slow-body read deadline armed.

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// deadlineRecorder records that the middleware armed a read deadline and forwards
// the call so the real per-request deadline still applies.
type deadlineRecorder struct {
	http.ResponseWriter
	armed *atomic.Bool
}

func (d deadlineRecorder) SetReadDeadline(deadline time.Time) error {
	d.armed.Store(true)
	return http.NewResponseController(d.ResponseWriter).SetReadDeadline(deadline)
}

// probeDeadline serves one raw request through withBodyReadDeadline on a fresh door
// and reports the decoded path, whether the deadline was armed, and the status.
func probeDeadline(t *testing.T, target string) (path string, armed bool, status int) {
	t.Helper()
	var armedFlag atomic.Bool
	seen := make(chan string, 1)
	door := withBodyReadDeadline(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}), networkBodyReadTimeout)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		door.ServeHTTP(deadlineRecorder{ResponseWriter: w, armed: &armedFlag}, r)
	}))
	t.Cleanup(srv.Close)

	status = sendRawTarget(t, srv.Listener.Addr().String(), target)
	select {
	case path = <-seen:
	default:
		t.Fatalf("target %q: handler never ran (status %d); the row no longer tests the exemption", target, status)
	}
	return path, armedFlag.Load(), status
}

// sendRawTarget writes target verbatim as the HTTP/1.1 request-target; the Go
// client would normalize these spellings before sending.
func sendRawTarget(t *testing.T, addr, target string) int {
	t.Helper()
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial probe door: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close probe conn: %v", err)
		}
	}()
	if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatalf("set conn safety deadline: %v", err)
	}
	raw := "POST " + target + " HTTP/1.1\r\nHost: foreign.example" +
		"\r\nContent-Type: application/proto\r\nContent-Length: 1\r\nConnection: close\r\n\r\nx"
	if _, err := io.WriteString(conn, raw); err != nil {
		t.Fatalf("write raw request %q: %v", target, err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response for %q: %v", target, err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close response body: %v", err)
	}
	return resp.StatusCode
}

func TestNetworkDoorProxyPathSpellingsKeepBodyDeadline(t *testing.T) {
	sessions := compassv1internalconnect.RunnerServiceSessionsProcedure

	t.Run("control: canonical exempt path is not armed", func(t *testing.T) {
		if _, armed, _ := probeDeadline(t, sessions); armed {
			t.Fatal("canonical Sessions path armed the deadline, want it exempt")
		}
	})

	for _, tc := range []struct{ name, target string }{
		{"trailing slash", sessions + "/"},
		{"double leading slash", "/" + sessions},
		{"dot segment", "/compass.v1.RunnerService/x/../Sessions"},
		{"encoded trailing slash", sessions + "%2F"},
		{"case-folded", strings.ToLower(sessions)},
		{"absolute-form double slash", "https://foreign.example/" + sessions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, armed, status := probeDeadline(t, tc.target)
			if !armed {
				t.Fatalf("target %q decoded to %q (status %d) with no read deadline: a near-miss was classified exempt", tc.target, path, status)
			}
		})
	}
}
