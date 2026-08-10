//go:build unix

package runnerhub

// Concurrency tests that drive the REAL Runner-side dispatch loop
// (runner.RunSessions) over the in-memory h2c Sessions wire against a fake
// runner.SessionHost, exercising the per-command goroutine dispatch, its
// leak-free shutdown join, and the Provision-arm concurrency cap
// (docs/designs/platform/compass-runner-concurrent-dispatch/design.md, Approach
// (a)/(e) + Plan T1 tests 1 & 4, T-cap).
//
// Why here and not package runner: the record's T1 note drives RunSessions "over
// the in-memory wire the existing dispatch tests use". The live server handler +
// command router + mounted h2c wire all live in runnerhub (router.go,
// handler.go, helpers_test.go), and the hub's exported Provision/Stop/Status
// methods are how a command is pushed down the Sessions stream and its result
// awaited. runnerhub does not import runner in non-test code, so a white-box test
// importing runner introduces no cycle. This also keeps the seam honest: these
// tests exercise the SAME runSessions the production RunSessions wraps.
//
// Every wait is event-gated on a channel — the fake host's park gates and the
// dispatch result channels — with testTimeout only as a fail-fast ceiling, never
// as synchronization (no sleeps, no polling, no retries).

import (
	"context"
	"sync"
	"testing"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/runner"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// fakeSessionHost is a runner.SessionHost whose Provision parks on a channel and
// whose other ops return immediately, so a test can hold a Provision in flight
// while it drives a concurrent Stop/Provision and observe whether the dispatch
// loop is blocked behind it, and whether shutdown joins it.
type fakeSessionHost struct {
	mu sync.Mutex

	provisionCalls int
	provisionLive  int // currently in Provision (between entered-signal and release)
	stopCalls      int

	provisionEntered chan struct{} // one value per Provision as it parks (buffered by the test)
	provisionRelease chan struct{} // Provision returns once this is closed
}

func (f *fakeSessionHost) Start(context.Context, *compassv1.StartAgentSessionRequest, string) (string, error) {
	return "sess-x", nil
}

func (f *fakeSessionHost) Provision(ctx context.Context, req *compassv1.ProvisionAgentWorkspaceRequest) (string, error) {
	f.mu.Lock()
	f.provisionCalls++
	f.provisionLive++
	entered, release := f.provisionEntered, f.provisionRelease
	f.mu.Unlock()

	if entered != nil {
		select {
		case entered <- struct{}{}:
		case <-ctx.Done():
			f.leaveProvision()
			return "", ctx.Err()
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			f.leaveProvision()
			return "", ctx.Err()
		}
	}
	f.leaveProvision()
	// Echo the account id back as the container name so the test can correlate.
	return "cont-" + req.GetAgentAccountId(), nil
}

func (f *fakeSessionHost) Stop(context.Context, string) error {
	f.mu.Lock()
	f.stopCalls++
	f.mu.Unlock()
	return nil
}

func (f *fakeSessionHost) Remove(context.Context, string) error         { return nil }
func (f *fakeSessionHost) Reload(context.Context, string) error         { return nil }
func (f *fakeSessionHost) RefreshSecrets(context.Context, string) error { return nil }
func (f *fakeSessionHost) RefreshConfig(context.Context) error          { return nil }
func (f *fakeSessionHost) Status(context.Context, string) ([]*compassv1.AgentSessionStatus, error) {
	return nil, nil
}

func (f *fakeSessionHost) leaveProvision() {
	f.mu.Lock()
	f.provisionLive--
	f.mu.Unlock()
}

// runnerLoopFixture stands up the hub + mounted h2c wire, dials a real Runner in,
// and drives runner.RunSessions over the live Sessions stream against host. It
// returns the hub (to push commands via its exported relay methods), a context
// bound to the loop, its cancel, and a channel carrying RunSessions' return. The
// caller gates on host park channels; waitRouterAttached ensures the router is
// bound before the first command is pushed.
func runnerLoopFixture(t *testing.T, host runner.SessionHost) (hub *Hub, ctx context.Context, cancel context.CancelFunc, loopDone <-chan error) {
	t.Helper()
	hub = newHubOnly()
	resolver := &fakeResolver{tokens: map[string]resolverEntry{
		"runner-tok": {subj: store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}},
	}}
	url := newMountedH2CServer(t, hub, resolver.resolve)

	ctx, cancel = context.WithCancel(context.Background())
	link, err := runner.Dial(ctx, runner.RunnerConfig{
		RunnerID:   "runner-1",
		ServerAddr: url,
		Token:      "runner-tok",
		HTTPClient: h2cHTTPClient(t),
	})
	if err != nil {
		cancel()
		t.Fatalf("runner.Dial = %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- link.RunSessions(ctx, host, discardLogger()) }()
	waitRouterAttached(t, hub)
	return hub, ctx, cancel, done
}

// A slow Provision does not head-of-line-block a concurrent Stop: while a
// Provision is parked in the host, a Stop dispatched on the same Sessions stream
// still completes. This is the whole point of per-command goroutine dispatch —
// the serial loop would be stuck inside handle(Provision) and never Receive the
// Stop.
//
// RED (serial receive loop, pre-T3): RunSessions' single Receive→handle→Send
// loop blocks inside handle(Provision) on the park, so the Stop frame is never
// read and hub.Stop blocks until the testTimeout ceiling — the Stop-completed
// gate never fires. GREEN: the Provision runs in its own goroutine, the loop
// reads on, and the Stop completes while Provision is still parked.
func TestSlowProvisionDoesNotBlockConcurrentStop(t *testing.T) {
	host := &fakeSessionHost{
		provisionEntered: make(chan struct{}, 1),
		provisionRelease: make(chan struct{}),
	}
	hub, _, cancel, loopDone := runnerLoopFixture(t, host)
	t.Cleanup(func() { cancel(); <-loopDone })

	// Dispatch a Provision; it parks in the host.
	provisionDone := make(chan error, 1)
	go func() {
		_, _, err := hub.Provision(context.Background(), "", &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "a"})
		provisionDone <- err
	}()
	select {
	case <-host.provisionEntered:
	case <-timeAfter():
		t.Fatal("Provision never reached the host over the wire")
	}

	// With the Provision parked, a concurrent Stop must still complete.
	stopDone := make(chan error, 1)
	go func() {
		_, err := hub.Stop(context.Background(), "", &compassv1.StopAgentSessionRequest{SessionId: "sess-1"})
		stopDone <- err
	}()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop while Provision parked = %v, want success", err)
		}
	case <-timeAfter():
		t.Fatal("Stop did not complete while a Provision was parked; the dispatch loop is head-of-line blocked behind the slow Provision (per-command goroutine dispatch missing)")
	}

	// Release the Provision so the loop can drain cleanly on cancel.
	close(host.provisionRelease)
	select {
	case <-provisionDone:
	case <-timeAfter():
		t.Fatal("Provision did not complete after release")
	}
}
