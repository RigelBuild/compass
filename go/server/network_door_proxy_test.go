//go:build unix

package server

// Proxied spellings of Runner stream paths must remain bounded before authentication:
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

	"github.com/RigelBuild/compass/go/internal/bodydeadline"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// deadlineRecorder records armed and cleared deadlines while forwarding calls so
// the real per-request deadline still applies.
type deadlineRecorder struct {
	http.ResponseWriter
	armed    *atomic.Bool
	released *atomic.Bool
}

func (d deadlineRecorder) SetReadDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		d.released.Store(true)
	} else {
		d.armed.Store(true)
	}
	return http.NewResponseController(d.ResponseWriter).SetReadDeadline(deadline)
}

// probeDeadline serves one raw request through withBodyReadDeadline on a fresh door
// and reports the decoded path, deadline state, and status.
func probeDeadline(t *testing.T, target string) (path string, armed, released bool, status int) {
	t.Helper()
	var armedFlag, releasedFlag atomic.Bool
	seen := make(chan string, 1)
	door := withBodyReadDeadline(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.Path
		bodydeadline.Release(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}), networkBodyReadTimeout)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		door.ServeHTTP(deadlineRecorder{
			ResponseWriter: w,
			armed:          &armedFlag,
			released:       &releasedFlag,
		}, r)
	}))
	t.Cleanup(srv.Close)

	status = sendRawTarget(t, srv.Listener.Addr().String(), target)
	select {
	case path = <-seen:
	default:
		t.Fatalf("target %q: handler did not run (status %d); deadline arm and release were not observed", target, status)
	}
	return path, armedFlag.Load(), releasedFlag.Load(), status
}

func TestNetworkDoorProxyPathSpellingsKeepBodyDeadline(t *testing.T) {
	sessions := compassv1internalconnect.RunnerServiceSessionsProcedure
	publishEvents := compassv1internalconnect.RunnerServicePublishEventsProcedure

	for _, tc := range []struct {
		name        string
		target      string
		wantRelease bool
	}{
		{"canonical Sessions", sessions, true},
		{"canonical PublishEvents", publishEvents, true},
		{"trailing slash", sessions + "/", false},
		{"double leading slash", "/" + sessions, false},
		{"dot segment", "/compass.v1.RunnerService/x/../Sessions", false},
		{"encoded trailing slash", sessions + "%2F", false},
		{"case-folded", strings.ToLower(sessions), false},
		{"absolute-form double slash", "https://foreign.example/" + sessions, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, armed, released, status := probeDeadline(t, tc.target)
			if !armed {
				t.Fatalf("target %q decoded to %q (status %d): handler ran but the read deadline was not armed", tc.target, path, status)
			}
			if released != tc.wantRelease {
				t.Fatalf("target %q decoded to %q (status %d): read deadline release granted = %t, want %t", tc.target, path, status, released, tc.wantRelease)
			}
		})
	}
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
