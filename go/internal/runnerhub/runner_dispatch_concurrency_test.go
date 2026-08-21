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
	"fmt"
	"sync"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runner"
	"github.com/RigelBuild/compass/go/internal/store"
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
	refreshCalls   int

	provisionEntered chan struct{} // one value per Provision as it parks (buffered by the test)
	provisionRelease chan struct{} // Provision returns once this is closed

	refreshEntered chan struct{} // one value per RefreshSecrets as it parks (buffered by the test)
	refreshRelease chan struct{} // RefreshSecrets returns once this is closed

	statusEntered chan struct{} // one value per Status as it parks (buffered by the test)
	statusRelease chan struct{} // Status returns once this is closed
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

func (f *fakeSessionHost) Remove(context.Context, string) error { return nil }
func (f *fakeSessionHost) Reload(context.Context, string) error { return nil }
func (f *fakeSessionHost) Deliver(context.Context, string, *compassv1internal.AgentControl) error {
	return nil
}

// RefreshSecrets is the rotation (SecretsVersion-signal) path. Like Provision it
// can park on a channel so a test can hold a slow rotation in flight and observe
// whether it head-of-line-blocks the dispatch loop. With nil gates it returns
// immediately (the common case for tests that do not exercise rotation).
func (f *fakeSessionHost) RefreshSecrets(ctx context.Context, _ string) error {
	f.mu.Lock()
	f.refreshCalls++
	entered, release := f.refreshEntered, f.refreshRelease
	f.mu.Unlock()

	if entered != nil {
		select {
		case entered <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (f *fakeSessionHost) RefreshConfig(context.Context) error { return nil }
func (f *fakeSessionHost) Status(ctx context.Context, _ string) ([]*compassv1.AgentSessionStatus, error) {
	f.mu.Lock()
	entered, release := f.statusEntered, f.statusRelease
	f.mu.Unlock()

	if entered != nil {
		select {
		case entered <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, nil
}

func (f *fakeSessionHost) leaveProvision() {
	f.mu.Lock()
	f.provisionLive--
	f.mu.Unlock()
}

// runnerLoopFixture stands up the hub + mounted h2c wire, dials a real Runner in,
// and drives runner.RunSessions over the live Sessions stream against host. It
// returns the hub (to push commands via its exported relay methods), the loop's
// cancel, and a channel carrying RunSessions' return. The caller gates on host
// park channels; waitRouterAttached ensures the router is bound before the first
// command is pushed.
func runnerLoopFixture(t *testing.T, host runner.SessionHost) (hub *Hub, cancel context.CancelFunc, loopDone <-chan error) {
	t.Helper()
	hub = newHubOnly()
	resolver := &fakeResolver{tokens: map[string]resolverEntry{
		"runner-tok": {subj: store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}},
	}}
	url := newMountedH2CServer(t, hub, resolver.resolve)

	ctx, cancel := context.WithCancel(context.Background())
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
	return hub, cancel, done
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
	hub, cancel, loopDone := runnerLoopFixture(t, host)
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

// A slow SecretsVersion rotation does not head-of-line-block a concurrent Stop.
// SecretsVersion is signal-only (no request id, no result frame), but its
// handler re-fetches and re-materializes synchronously — a real network+exec
// cost — so it must run through the per-command goroutine, not inline on the
// receive loop, or a wedged rotation for one session stalls every other command.
//
// RED (SecretsVersion executed inline on the receive loop): the loop calls
// RefreshSecrets inline and parks in it, so the Stop frame is never Received and
// hub.Stop blocks until the testTimeout ceiling. GREEN: the rotation runs in its
// own goroutine, the loop reads on, and the Stop completes while it is parked.
func TestSlowSecretsRefreshDoesNotBlockConcurrentStop(t *testing.T) {
	host := &fakeSessionHost{
		refreshEntered: make(chan struct{}, 1),
		refreshRelease: make(chan struct{}),
	}
	hub, cancel, loopDone := runnerLoopFixture(t, host)
	t.Cleanup(func() { cancel(); <-loopDone })

	// A live session must be bound for SignalSecretsVersion to push a frame at it.
	bindSession(hub, "sess-a")

	// Signal a rotation; the Runner dispatches it to RefreshSecrets, which parks.
	if err := hub.SignalSecretsVersion(); err != nil {
		t.Fatalf("SignalSecretsVersion = %v, want nil", err)
	}
	select {
	case <-host.refreshEntered:
	case <-timeAfter():
		t.Fatal("SecretsVersion rotation never reached the host over the wire")
	}

	// With the rotation parked, a concurrent Stop must still complete.
	stopDone := make(chan error, 1)
	go func() {
		_, err := hub.Stop(context.Background(), "", &compassv1.StopAgentSessionRequest{SessionId: "sess-a"})
		stopDone <- err
	}()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop while a SecretsVersion rotation was parked = %v, want success", err)
		}
	case <-timeAfter():
		t.Fatal("Stop did not complete while a SecretsVersion rotation was parked; the dispatch loop is head-of-line blocked behind the slow rotation (SecretsVersion still runs inline on the receive loop)")
	}

	// Release the rotation so the loop can drain cleanly on cancel.
	close(host.refreshRelease)
}

// Concurrent per-command result Sends over the REAL client Sessions stream do
// not race. connect's client BidiStream.Send is NOT safe for concurrent use, and
// per-command goroutine dispatch has many commands completing and calling d.send
// (the loop's serialized Send closure) at once. runSessions guards every Send
// with sendMu; this test forces N result frames to hit d.send simultaneously and
// runs under -race, so a regression that dropped the runner-side sendMu reddens
// here (WARNING: DATA RACE on connect's stream write path), mirroring the
// server-side TestConcurrentDispatchOverRealStreamNoDataRace.
//
// The barrier is load-bearing. An instant host arm lets the receive loop pace
// Receive->spawn->execute->send so the sends effectively serialize and the race
// never fires. So every Status arm PARKS on entry: the test waits until all N
// are parked (all N goroutines poised past Receive+spawn, each holding a result
// to send), then releases them at once — now the N d.send calls actually overlap.
//
// This lives in package runnerhub because it needs the real mounted h2c wire and
// a live client stream driving the production runSessions; the runner package's
// own seam tests drive a fake stream whose Send is mutex-guarded, so a dropped
// runner sendMu stays green there. The merge gate therefore MUST run
// ./internal/runnerhub/... under -race for this invariant to be covered.
func TestConcurrentResultSendsOverRealStreamNoDataRace(t *testing.T) {
	const n = 64
	host := &fakeSessionHost{
		statusEntered: make(chan struct{}, n),
		statusRelease: make(chan struct{}),
	}
	hub, cancel, loopDone := runnerLoopFixture(t, host)
	t.Cleanup(func() { cancel(); <-loopDone })

	// Fan out N concurrent Status commands with DISTINCT request ids (distinct ids
	// avoid the in-flight dedup join — a same-id retry would wait on the first,
	// not Send a second frame). Each parks in the host.
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("req-%d", i)
			_, errs[i] = hub.Status(context.Background(), id, &compassv1.GetAgentStatusRequest{SessionId: "sess-x"})
		}(i)
	}

	// Wait until all N are parked inside the host — every goroutine is now poised
	// with a result to send. Releasing them together makes the N d.send calls
	// overlap, which is what exercises sendMu.
	for range n {
		select {
		case <-host.statusEntered:
		case <-timeAfter():
			t.Fatal("not all Status commands reached the host; dispatch is not concurrent")
		}
	}
	close(host.statusRelease)
	wg.Wait()

	// Every dispatch completed with a correlated result: a concurrent-Send frame
	// interleave corrupts the wire bytes, so a caller either fails to decode or
	// never completes (ctx timeout) — both reddening here in addition to -race.
	for i := range n {
		if errs[i] != nil {
			t.Errorf("Status %d = %v, want a correlated result", i, errs[i])
		}
	}
}
