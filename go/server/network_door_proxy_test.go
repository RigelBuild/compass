//go:build pgtest && unix

package server

// Proxied path spellings against the network door: none may escape the slow-body
// read deadline by being classified as an exempt Runner stream path.

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// proxyForeignHost is the Host a fronting proxy may pass through unchanged.
const proxyForeignHost = "foreign.example"

// deadlineRecorder records whether the middleware armed a read deadline and
// forwards the call so the real per-request deadline still applies.
type deadlineRecorder struct {
	http.ResponseWriter
	armed *bool
}

func (d deadlineRecorder) SetReadDeadline(deadline time.Time) error {
	*d.armed = true
	return http.NewResponseController(d.ResponseWriter).SetReadDeadline(deadline)
}

// pathProbe is what the inner handler saw: the parsed path and whether the
// deadline was armed before the handler ran.
type pathProbe struct {
	path  string
	armed bool
}

// startPathProbeDoor serves withBodyReadDeadline over real net/http parsing, so
// each request-target reaches the middleware exactly as the server decoded it.
func startPathProbeDoor(t *testing.T) (string, <-chan pathProbe) {
	t.Helper()
	probes := make(chan pathProbe, 1)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec, ok := w.(deadlineRecorder)
		probes <- pathProbe{path: r.URL.Path, armed: ok && *rec.armed}
		w.WriteHeader(http.StatusNoContent)
	})
	door := withBodyReadDeadline(inner, networkBodyReadTimeout)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		door.ServeHTTP(deadlineRecorder{ResponseWriter: w, armed: new(bool)}, r)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), probes
}

// sendRawTarget writes target verbatim as the HTTP/1.1 request-target; the Go
// client would normalize several of these spellings before sending.
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
	raw := "POST " + target + " HTTP/1.1\r\nHost: " + proxyForeignHost +
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
	addr, probes := startPathProbeDoor(t)
	info := compassv1connect.CompassServiceGetServerInfoProcedure
	sessions := compassv1internalconnect.RunnerServiceSessionsProcedure

	// probe sends target and returns what the handler saw, or ok=false when the
	// server rejected the request before the handler ran (the handler answers 204).
	probe := func(t *testing.T, target string) (pathProbe, int, bool) {
		t.Helper()
		status := sendRawTarget(t, addr, target)
		if status != http.StatusNoContent {
			return pathProbe{}, status, false
		}
		select {
		case p := <-probes:
			return p, status, true
		case <-timeAfter():
			t.Fatalf("target %q answered 204 but the handler reported nothing", target)
			return pathProbe{}, status, false
		}
	}

	t.Run("control: canonical exempt path is not armed", func(t *testing.T) {
		p, status, ran := probe(t, sessions)
		if !ran || p.armed {
			t.Fatalf("exempt path: ran=%v armed=%v status=%d, want the handler reached with no deadline", ran, p.armed, status)
		}
	})

	nonExempt := []struct{ name, target string }{
		{"canonical", info},
		{"trailing slash", info + "/"},
		{"double leading slash", "/" + info},
		{"percent-encoded dot in service name", strings.Replace(info, "compass.v1", "compass%2Ev1", 1)},
		{"percent-encoded slash before method", strings.Replace(info, "Service/", "Service%2F", 1)},
		{"absolute-form foreign host", "https://" + proxyForeignHost + info},
		{"absolute-form double slash", "https://" + proxyForeignHost + "/" + info},
		{"query string", info + "?x=" + sessions},
		{"exempt near-miss: trailing slash", sessions + "/"},
		{"exempt near-miss: double leading slash", "/" + sessions},
		{"exempt near-miss: case-folded", strings.ToLower(sessions)},
		{"exempt near-miss: dot segment", "/compass.v1.RunnerService/x/../Sessions"},
		{"exempt near-miss: encoded trailing slash", sessions + "%2F"},
	}
	for _, tc := range nonExempt {
		t.Run(tc.name, func(t *testing.T) {
			p, status, ran := probe(t, tc.target)
			if !ran {
				if status < 400 {
					t.Fatalf("target %q: handler not reached but status=%d, want a rejection", tc.target, status)
				}
				t.Logf("target %q rejected before the handler (status %d)", tc.target, status)
				return
			}
			if !p.armed {
				t.Fatalf("target %q parsed as path %q with no read deadline armed: a non-exempt spelling was classified exempt", tc.target, p.path)
			}
			t.Logf("target %q -> path %q, deadline armed", tc.target, p.path)
		})
	}
}
