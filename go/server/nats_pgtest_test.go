//go:build pgtest && unix

package server

import (
	"testing"

	natsd "github.com/nats-io/nats-server/v2/server"
	natsserver "github.com/nats-io/nats-server/v2/test"

	"github.com/RigelBuild/compass/go/internal/fabric"
)

// startTestNats runs an in-process JetStream NATS server on loopback for one
// test and returns its client URL; the server and its store dir die with t.
func startTestNats(t *testing.T) string {
	t.Helper()
	srv := natsserver.RunServer(&natsd.Options{
		Host:      "127.0.0.1",
		Port:      natsd.RANDOM_PORT,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	})
	t.Cleanup(srv.Shutdown)
	return srv.ClientURL()
}

// provisionNats points a full-Serve test at its own NATS unless it chose one:
// Serve connects the event fabric at boot and fails closed without it.
func provisionNats(t *testing.T, cfg *ServeConfig) {
	t.Helper()
	if cfg.NatsURL == "" {
		cfg.NatsURL = startTestNats(t)
	}
}

// newTestFabric connects a fabric to a fresh per-test NATS, the wiring Serve
// threads into comms and delivery. Its cleanup runs before the server stops.
func newTestFabric(t *testing.T) *fabric.Fabric {
	t.Helper()
	fab, err := fabric.New(fabric.Config{URL: startTestNats(t)})
	if err != nil {
		t.Fatalf("fabric.New: %v", err)
	}
	t.Cleanup(func() {
		if err := fab.Close(); err != nil {
			t.Errorf("fabric Close: %v", err)
		}
	})
	return fab
}
