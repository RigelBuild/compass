//go:build unix

package delivery

// Hand-written fakes for the delivery consumer's collaborators, mirroring the
// runnerhub seam-test fakes (helpers_test.go fakeLifecycleSink /
// integration_pgtest_test.go noopLifecycleSink): a recording ControlDispatcher,
// an in-memory SessionResolver, and an in-memory DeliveryReads. Every test drives
// the consumer through these and asserts on the recorded dispatches. Tests root
// on context.Background() as the test root (rule://go-thread-context exemption
// for _test.go); the consumer's Run(ctx) is fed that same root, so no fresh
// context is minted mid-tree.

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// testTimeout bounds every event-gated wait so a wedged consumer fails fast
// rather than hanging the suite. A deadline safety net, never a synchronization
// device: tests gate on the recorder's observed dispatch count, not elapsed time.
const testTimeout = 10 * time.Second

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// opKind distinguishes a deliver op from a steer op in a recorded dispatch, so a
// mention-routing test can assert the mentioned agent got a STEER and a plain
// subscriber got a DELIVER.
type opKind int

const (
	opDeliver opKind = iota
	opSteer
	opOther
)

// dispatchRecord is one observed DispatchControl call: the session it targeted,
// the delivered message's id, and the op kind (deliver vs steer), each pulled
// from the control op.
type dispatchRecord struct {
	sessionID string
	messageID string
	kind      opKind
}

// classifyOp reports the op kind and carried message id of a dispatched control,
// so the recorder can tell a steer from a deliver (both carry a Message).
func classifyOp(op *compassv1internal.AgentControl) (opKind, string) {
	switch {
	case op.GetSteer() != nil:
		return opSteer, op.GetSteer().GetMessage().GetId()
	case op.GetDeliver() != nil:
		return opDeliver, op.GetDeliver().GetMessage().GetId()
	default:
		return opOther, ""
	}
}

// fakeDispatcher records every DispatchControl call and can be configured to
// return a synchronous refusal for a given session (the "no live stream" edge).
// Concurrency-safe: the consumer dispatches from its own goroutine and from the
// settle drain. It signals each recorded call on a channel so a test event-gates
// on the observed dispatch rather than sleeping.

// errNoStream is the synchronous-refusal a dispatcher returns for a session with
// no live stream — the edge the consumer treats as "fall to the sweep".
var errNoStream = errors.New("no live runner sessions stream")

type fakeDispatcher struct {
	mu       sync.Mutex
	calls    []dispatchRecord
	refuse   map[string]error // sessionID -> synchronous refusal to return
	recorded chan struct{}    // buffered; one token per recorded call

	// First-call barrier: when armed, the FIRST DispatchControl blocks after
	// signaling entry, so a test can hold a dispatch in-flight (a sweep's first
	// re-dispatch holding the session gate) and observe that a concurrent live
	// deliver for the same session queues behind it. Deterministic, no sleep.
	armed        bool
	enteredFirst chan struct{}
	releaseFirst chan struct{}
}

func newFakeDispatcher() *fakeDispatcher {
	return &fakeDispatcher{
		refuse:   map[string]error{},
		recorded: make(chan struct{}, 1024),
	}
}

func (d *fakeDispatcher) DispatchControl(_ context.Context, sessionID string, op *compassv1internal.AgentControl) error {
	d.mu.Lock()
	if d.armed {
		d.armed = false
		entered, release := d.enteredFirst, d.releaseFirst
		d.mu.Unlock()
		close(entered)
		<-release
		d.mu.Lock()
	}
	if err := d.refuse[sessionID]; err != nil {
		d.mu.Unlock()
		return err
	}
	kind, messageID := classifyOp(op)
	d.calls = append(d.calls, dispatchRecord{sessionID: sessionID, messageID: messageID, kind: kind})
	d.mu.Unlock()
	d.recorded <- struct{}{}
	return nil
}

// armFirstBlock makes the next (first) DispatchControl block until releaseFirst
// is closed, after signaling entry on enteredFirst.
func (d *fakeDispatcher) armFirstBlock() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.armed = true
	d.enteredFirst = make(chan struct{})
	d.releaseFirst = make(chan struct{})
}

func (d *fakeDispatcher) snapshot() []dispatchRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]dispatchRecord, len(d.calls))
	copy(out, d.calls)
	return out
}

// waitForDispatches blocks until at least n dispatches have been recorded, or
// fails at the deadline. It event-gates on the recorder's per-call signal, never
// a sleep.
func (d *fakeDispatcher) waitForDispatches(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(testTimeout)
	for {
		if len(d.snapshot()) >= n {
			return
		}
		select {
		case <-d.recorded:
		case <-deadline:
			t.Fatalf("waited for %d dispatches, got %d", n, len(d.snapshot()))
		}
	}
}

// fakeResolver is an in-memory SessionResolver: an account -> live session map a
// test seeds to model which recipients are live.
type fakeResolver struct {
	mu       sync.Mutex
	sessions map[store.AccountID]string
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{sessions: map[store.AccountID]string{}}
}

func (r *fakeResolver) SessionForAccount(account store.AccountID) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[account]
	return s, ok
}

func (r *fakeResolver) LiveAgentSessions() map[store.AccountID]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[store.AccountID]string, len(r.sessions))
	maps.Copy(out, r.sessions)
	return out
}

func (r *fakeResolver) bind(account store.AccountID, sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[account] = sessionID
}

// fakeReads is an in-memory DeliveryReads: the subscriber set per channel, the
// channel agent-member set (for mention→steer routing), the agent-account set, a
// message-id -> message table, a handle -> account resolution map, and a
// per-agent owed-message set for the sweep. A test seeds exactly what the case
// under test needs.
type fakeReads struct {
	mu          sync.Mutex
	subscribers map[store.ChannelID][]store.AccountID // channel -> subscribed agents (author NOT pre-excluded)
	members     map[store.ChannelID][]store.AccountID // channel -> agent members (author NOT pre-excluded)
	agents      map[store.AccountID]bool
	handles     map[string]store.Account // lowercased handle -> resolved account (unknown -> ErrNotFound)
	messages    map[string]store.Message
	owed        map[store.AccountID]map[store.ChannelID][]store.Message

	// beforeUndelivered, when set, is called with the swept agent id at the TOP
	// of UndeliveredMessages BEFORE f.mu is acquired — a TEST-ONLY seam (nil in
	// production) so a test can block a sweep's owed-read mid-flight and publish
	// a message INTO the sweep/subscribe window without deadlocking other reads
	// on the fake's lock.
	beforeUndelivered func(store.AccountID)
}

func newFakeReads() *fakeReads {
	return &fakeReads{
		subscribers: map[store.ChannelID][]store.AccountID{},
		members:     map[store.ChannelID][]store.AccountID{},
		agents:      map[store.AccountID]bool{},
		handles:     map[string]store.Account{},
		messages:    map[string]store.Message{},
		owed:        map[store.AccountID]map[store.ChannelID][]store.Message{},
	}
}

func (f *fakeReads) SubscribedAgents(_ context.Context, channel store.ChannelID, author store.AccountID) ([]store.AccountID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.AccountID
	for _, a := range f.subscribers[channel] {
		if a == author {
			continue // author excluded, mirroring the SQL's cm.account_id <> $2
		}
		out = append(out, a)
	}
	return out, nil
}

// ChannelAgentMembers mirrors the store query: every agent member of channel,
// author excluded, subscribe-state irrelevant (the members map, not subscribers).
func (f *fakeReads) ChannelAgentMembers(_ context.Context, channel store.ChannelID, author store.AccountID) ([]store.AccountID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.AccountID
	for _, a := range f.members[channel] {
		if a == author {
			continue // author excluded, mirroring the SQL's cm.account_id <> $2
		}
		out = append(out, a)
	}
	return out, nil
}

// AgentByHandle resolves a lowercased handle to its seeded agent account; an
// unseeded handle is store.ErrNotFound, mirroring the store's fail-closed
// treatment of an unknown or human handle.
func (f *fakeReads) AgentByHandle(_ context.Context, handle string) (store.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	acc, ok := f.handles[handle]
	if !ok {
		return store.Account{}, store.ErrNotFound
	}
	return acc, nil
}

func (f *fakeReads) IsAgentAccount(_ context.Context, account store.AccountID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.agents[account], nil
}

func (f *fakeReads) MessageByID(_ context.Context, messageID string) (store.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.messages[messageID]
	if !ok {
		return store.Message{}, store.ErrNotFound
	}
	return m, nil
}

func (f *fakeReads) UndeliveredMessages(_ context.Context, agent store.AccountID) (map[store.ChannelID][]store.Message, error) {
	if f.beforeUndelivered != nil {
		f.beforeUndelivered(agent)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.owed[agent], nil
}

// seedMessage registers a stored message a re-read (settle / sweep) resolves.
func (f *fakeReads) seedMessage(m store.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages[string(m.ID)] = m
}

// agentAccount builds a resolved agent store.Account for the handle→account map,
// so AgentByHandle resolves a mention to it. The Agent subtype is what makes
// IsAgent() true (the store's non-agent handles are ErrNotFound, so only agents
// are ever seeded here).
func agentAccount(id store.AccountID, handle string) store.Account {
	return store.Account{ID: id, Handle: handle, Agent: &store.AgentAccount{}}
}

// textMessage builds a store.Message with one text block on the shared test
// channel ("chan-1", the const ch every test declares).
func textMessage(id string, author store.AccountID, body string) store.Message {
	return store.Message{
		ID:              store.MessageID(id),
		Container:       store.ContainerRef{ChannelID: "chan-1"},
		AuthorAccountID: author,
		Blocks:          []store.MessageBlock{{Text: &body}},
	}
}

// postedResponse builds a MessagePosted bus payload for msg.
func postedResponse(msg *compassv1.Message) *compassv1.SubscribeCommsResponse {
	return &compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_MessagePosted{
			MessagePosted: &compassv1.MessagePosted{Message: msg},
		},
	}
}

// wireText builds a wire Message with one text block on the shared test channel
// ("chan-1", the const ch every test declares).
func wireText(id string, author store.AccountID, body string) *compassv1.Message {
	return &compassv1.Message{
		Id:              id,
		Container:       &compassv1.Message_ChannelId{ChannelId: "chan-1"},
		AuthorAccountId: string(author),
		Blocks:          []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: body}}},
	}
}

// wireTextBlocks builds a wire Message with one text block per body on the
// shared test channel ("chan-1"), so a test can mention the same @handle across
// separate blocks and assert global dedup.
func wireTextBlocks(id string, author store.AccountID, bodies ...string) *compassv1.Message {
	blocks := make([]*compassv1.MessageBlock, 0, len(bodies))
	for _, body := range bodies {
		blocks = append(blocks, &compassv1.MessageBlock{Block: &compassv1.MessageBlock_Text{Text: body}})
	}
	return &compassv1.Message{
		Id:              id,
		Container:       &compassv1.Message_ChannelId{ChannelId: "chan-1"},
		AuthorAccountId: string(author),
		Blocks:          blocks,
	}
}

// newTestConsumer builds a consumer over fresh fakes and returns them. The bus
// is a real events.Bus so the tests exercise the true Subscribe/Publish tail.
func newTestConsumer(t *testing.T) (*Consumer, *fakeDispatcher, *fakeResolver, *fakeReads) {
	t.Helper()
	disp := newFakeDispatcher()
	res := newFakeResolver()
	reads := newFakeReads()
	c := NewConsumer(events.NewBus[*compassv1.SubscribeCommsResponse](), reads, disp, res, discardLogger())
	return c, disp, res, reads
}
