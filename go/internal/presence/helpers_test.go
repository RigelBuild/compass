//go:build unix

package presence

// Hand-written fakes + a recording bus subscriber for the presence publisher's
// collaborators, mirroring delivery's helpers_test.go discipline: a real
// events.Bus (so tests exercise the true Subscribe/Publish tail), an in-memory
// PresenceReads (the open-ask overlay input), and an in-memory
// LifecycleStatusResolver (the reconciliation input). Every test drives the
// publisher through these and event-gates on the observed AgentPresenceChanged
// publishes, never a sleep, never a retry (rule://no-retries).
// context.Background() is the test root (rule://go-thread-context exemption for
// _test.go); the publisher's Run(ctx) is fed a cancelable child of it.

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// testTimeout bounds every event-gated wait so a wedged publisher fails fast
// rather than hanging the suite. A deadline safety net, never a synchronization
// device: tests gate on the recorder's observed publish count, not elapsed time.
const testTimeout = 10 * time.Second

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fakeReads is an in-memory PresenceReads: a per-agent open-ask flag a test
// seeds. Concurrency-safe — the publisher reads it from its own loop goroutine
// while a test flips it.
type fakeReads struct {
	mu   sync.Mutex
	open map[store.AccountID]bool
}

func newFakeReads() *fakeReads {
	return &fakeReads{open: map[store.AccountID]bool{}}
}

func (f *fakeReads) AgentHasOpenAsk(_ context.Context, agent store.AccountID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open[agent], nil
}

func (f *fakeReads) setOpen(agent store.AccountID, open bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open[agent] = open
}

// fakeStatus is an in-memory LifecycleStatusResolver: a per-session live state a
// test seeds, modeling the Runner Status relay the reconciliation edge consults.
type fakeStatus struct {
	mu     sync.Mutex
	states map[string]compassv1.AgentSessionState
}

func newFakeStatus() *fakeStatus {
	return &fakeStatus{states: map[string]compassv1.AgentSessionState{}}
}

func (f *fakeStatus) SessionState(_ context.Context, sessionID string) (compassv1.AgentSessionState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.states[sessionID]
	return s, ok
}

func (f *fakeStatus) set(sessionID string, state compassv1.AgentSessionState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[sessionID] = state
}

// presenceRecord is one observed AgentPresenceChanged publish.
type presenceRecord struct {
	account  store.AccountID
	presence compassv1.AgentPresence
}

// recorder subscribes to the bus and records every AgentPresenceChanged the
// publisher emits, signaling each on a channel so a test event-gates on the
// observed publish rather than sleeping. It tails on its own goroutine, started
// before the publisher publishes anything (the test seeds then publishes).
type recorder struct {
	mu      sync.Mutex
	records []presenceRecord
	signal  chan struct{}
}

// startRecorder subscribes to bus at the live tail and records presence
// publishes until the test ends. It skips the AgentPresenceChanged the publisher
// emits on the replay of pre-subscribe events (there are none — the recorder
// subscribes before any publish), tailing only live.
func startRecorder(t *testing.T, bus *events.Bus[*compassv1.SubscribeCommsResponse]) *recorder {
	t.Helper()
	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("recorder subscribe: %v", err)
	}
	r := &recorder{signal: make(chan struct{}, 64)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range sub.Live {
			pc := ev.Payload.GetAgentPresenceChanged()
			if pc == nil {
				continue
			}
			r.mu.Lock()
			r.records = append(r.records, presenceRecord{
				account:  store.AccountID(pc.GetAgentAccountId()),
				presence: pc.GetPresence(),
			})
			r.mu.Unlock()
			select {
			case r.signal <- struct{}{}:
			default:
			}
		}
	}()
	t.Cleanup(func() {
		sub.Cancel()
		<-done
	})
	return r
}

// snapshot returns a copy of the recorded presence publishes.
func (r *recorder) snapshot() []presenceRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]presenceRecord, len(r.records))
	copy(out, r.records)
	return out
}

// waitForPublishes blocks until at least n presence publishes have been
// recorded, or fails at the deadline. It event-gates on the recorder's per-call
// signal, never a sleep.
func (r *recorder) waitForPublishes(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(testTimeout)
	for {
		r.mu.Lock()
		got := len(r.records)
		r.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-r.signal:
		case <-deadline:
			t.Fatalf("timed out waiting for %d presence publishes; saw %d: %+v", n, got, r.snapshot())
		}
	}
}

// newTestPublisher builds a publisher over fresh fakes and a real bus, and
// returns them. The bus is a real events.Bus so the ask arm exercises the true
// Subscribe/Publish tail.
func newTestPublisher(t *testing.T) (*Publisher, *fakeReads, *fakeStatus, *events.Bus[*compassv1.SubscribeCommsResponse]) {
	t.Helper()
	reads := newFakeReads()
	status := newFakeStatus()
	bus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	p := NewPublisher(bus, reads, status, discardLogger())
	return p, reads, status, bus
}

// startPublisher runs p.Run in the background on a cancelable child of the test
// root and registers cancellation + drain on cleanup, so every test ends the
// loop deterministically.
func startPublisher(t *testing.T, p *Publisher) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// Run returns nil on ctx cancel (serve shutdown); the loop's error paths
		// are asserted directly elsewhere, so the background return is discarded
		// deliberately here.
		_ = p.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// askPosted builds a MessagePosted bus payload carrying an unanswered ask
// authored by author — the Ask-open trigger the ask arm keys on.
func askPosted(author store.AccountID) *compassv1.SubscribeCommsResponse {
	return &compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_MessagePosted{
			MessagePosted: &compassv1.MessagePosted{Message: &compassv1.Message{
				Id:              "msg-ask",
				Container:       &compassv1.Message_ChannelId{ChannelId: "chan-1"},
				AuthorAccountId: string(author),
				Blocks: []*compassv1.MessageBlock{{
					Block: &compassv1.MessageBlock_Ask{Ask: &compassv1.Ask{AskId: "ask-1"}},
				}},
			}},
		},
	}
}

// askAnsweredUpdate builds a MessageUpdated bus payload for author's ask message
// — the Ask-answered trigger. The wire message alone does not decide the overlay
// (the store does), so its block contents are immaterial; the arm recomputes
// against the store.
func askAnsweredUpdate(author store.AccountID) *compassv1.SubscribeCommsResponse {
	return &compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_MessageUpdated{
			MessageUpdated: &compassv1.MessageUpdated{Message: &compassv1.Message{
				Id:              "msg-ask",
				Container:       &compassv1.Message_ChannelId{ChannelId: "chan-1"},
				AuthorAccountId: string(author),
			}},
		},
	}
}

// textPosted builds a MessagePosted bus payload with only a text block — NOT an
// ask, so the ask arm ignores it.
func textPosted(author store.AccountID) *compassv1.SubscribeCommsResponse {
	return &compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_MessagePosted{
			MessagePosted: &compassv1.MessagePosted{Message: &compassv1.Message{
				Id:              "msg-text",
				Container:       &compassv1.Message_ChannelId{ChannelId: "chan-1"},
				AuthorAccountId: string(author),
				Blocks:          []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "hi"}}},
			}},
		},
	}
}
