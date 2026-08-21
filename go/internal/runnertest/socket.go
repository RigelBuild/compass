//go:build unix

// Package runnertest provides socket-serving test scaffolding shared by the
// runner integration tests: a sun_path-bounded runtime dir, a real
// AgentGateway client dialed over a unix socket, and the h2c plumbing those
// exercise. These helpers need only stdlib plus the generated AgentGateway
// client, so they deliberately import NOTHING from internal/runner — that keeps
// package runner's own test (which uses DialAgentSocket) cycle-free.
package runnertest

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// ShortRuntimeDir is a Runner RuntimeDir bounded to fit the AF_UNIX sun_path
// limit, replacing t.TempDir() for the tests that build a real agent socket.
// The Runner appends a 69-byte tail to its RuntimeDir
// (/containers/compass-agent-<32hex>/agent.sock, host.go:291) at the
// store-minted 32-hex id, leaving 38 bytes of a 107-byte cap on Linux.
// t.TempDir() derives its path from the TEST NAME, and every one of the
// integration package's tests exceeds that budget: the ceiling is a 19-character
// name and the shortest there is 26. Only the socket-opening test fails today
// because only that one opens a socket, so the name-length dependency is a trap
// for the next test that wires a SessionHost.
//
// A fixed short root removes the TEST-NAME dependency. It does not make the
// budget unconditional: the root still comes from TMPDIR, and a deep one (a CI
// work dir, or macOS's ~49-byte /var/folders/<2>/<hash>/T) re-inflates it. So
// the resulting path is asserted rather than assumed.
//
// This site FAILS rather than skips on an over-budget root: a skip would
// silently drop the only end-to-end coverage of the socket path, reporting `ok`
// for a test that asserted nothing. The gateway's padTo fails closed for the
// same reason. The cap is derived the way the production guard derives it
// (gateway.sunPathMax) rather than written down — sun_path is not one size
// across the platforms //go:build unix admits (108 on linux/solaris/illumos,
// 104 on darwin and the BSDs, 1023 on aix), and this file is //go:build unix.
// The derivation is duplicated below because that constant is unexported; the
// code below derives the cap rather than writing a literal. (The sizes quoted
// just above are for the reader, not values anything computes against.) The
// namePrefix and accountIDHexLen the caller passes model the same container name
// the Runner actually builds, so the budget assertion tracks the real path.
//
// A caller may open MULTIPLE sockets under the returned root (the e2e wire
// does); all share the RuntimeDir/containers/<name> layout and every name is the
// same length (namePrefix + accountIDHexLen hex chars), so this single
// longest-path budget covers them all.
func ShortRuntimeDir(t *testing.T, namePrefix string, accountIDHexLen int) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cr") //nolint:usetesting // t.TempDir embeds the test name, which is what put this path over the sun_path cap — the bug this helper exists to prevent
	if err != nil {
		t.Fatalf("MkdirTemp for runner runtime dir: %v", err)
	}
	// Longest path the Runner builds under dir. sun_path holds the path plus a
	// NUL, so the usable cap is one less than the platform's array.
	const sunPathMax = len(syscall.RawSockaddrUnix{}.Path) - 1
	longest := filepath.Join(dir, "containers", namePrefix+strings.Repeat("f", accountIDHexLen), "agent.sock")
	if len(longest) > sunPathMax {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			t.Errorf("removing over-budget runner runtime dir %q: %v", dir, rmErr)
		}
		t.Fatalf("runner runtime dir %q yields a %d-byte agent socket path, over the %d-byte sun_path cap (TMPDIR too deep)", dir, len(longest), sunPathMax)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("removing runner runtime dir %q: %v", dir, err)
		}
	})
	return dir
}

// DialAgentSocket builds a real generated AgentGatewayClient that dials the unix
// socket at path over prior-knowledge h2c — the same cleartext-HTTP/2 door the
// per-container listener serves — so the Gateway is exercised over the wire it
// ships on. The base URL is a placeholder; DialContext routes every dial to the
// socket.
func DialAgentSocket(t *testing.T, path string) compassv1internalconnect.AgentGatewayClient {
	t.Helper()
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{
		Protocols: p,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return compassv1internalconnect.NewAgentGatewayClient(&http.Client{Transport: tr}, "http://unix")
}

// CleartextH2 returns an http.Protocols that permits HTTP/1 and prior-knowledge
// h2c — the protocol set the per-container listener serves.
func CleartextH2() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

// H2CClient returns an http.Client speaking prior-knowledge h2c over ordinary
// TCP dials, for exercising a listener that serves cleartext HTTP/2.
func H2CClient(t *testing.T) *http.Client {
	t.Helper()
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{
		Protocols: p,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}
