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

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// testTimeout bounds every event-gated wait so a wedged consumer fails fast
// rather than hanging the suite. A deadline safety net, never a synchronization
// device: tests gate on the recorder's observed dispatch count, not elapsed time.
const testTimeout = 10 * time.Second

// busLagFloodCount is how many messages the bus-lag tests publish to force a
// live-buffer overrun: it must exceed the events bus's per-subscriber live-tail
// buffer (events.liveBufferCapacity == events.ringCapacity == 1024) so the
// subscriber's channel latches lagged and closes — the exact condition the
// resync/sweep path under test triggers on. The events caps are unexported, so
// this constant restates the coupling explicitly with margin: if those caps ever
// rise, this must rise past them, or the overrun stops firing and the RIG-2514
// regression guard silently degrades to a no-op (the tests would still pass
// while guarding nothing).
const busLagFloodCount = 1100

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// signalObserved does a NON-BLOCKING send of a per-call token on a test
// observation channel (the recorded/wake signals below). The token only wakes a
// waiter (waitForMessage / waitForDispatches / waitForWakes / waitFor) to
// re-check the fake's recorded set; the recorded FACT already lives in the fake's
// mutex-guarded calls slice BEFORE this send, so the token is a wakeup hint,
// never the source of truth. The send must not block: a blocking send turns the
// observation channel into backpressure on the code under test, so a test that
// produces more dispatches than the buffer holds (the bus-lag floods publish
// 1100 past the 1024-token buffer) wedges the consumer's Run goroutine on a full
// channel the moment a waiter stops draining — a deadlock that passes in
// isolation but hangs the whole package to the -timeout under cross-test load
// (RIG-2514). Dropping a token is safe: a drop happens only when the buffer is
// full (hence non-empty), so a blocked waiter still has a token to drain and loop
// back to re-check the snapshot; when the buffer is empty the send always lands.
func signalObserved(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

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
	sessionID   string
	messageID   string
	kind        opKind
	fromHandle  string
	channelName string
	topicName   string
	traceparent string
}

// classifyOp reports the op kind, carried message id, denormalized author
// from_handle, denormalized source channel+topic names, and traceparent of a
// dispatched control, so the recorder can tell a steer from a deliver and assert
// on the fields both ops carry.
func classifyOp(op *compassv1internal.AgentControl) (kind opKind, messageID, fromHandle, channelName, topicName, traceparent string) {
	switch {
	case op.GetSteer() != nil:
		s := op.GetSteer()
		return opSteer, s.GetMessage().GetId(), s.GetFromHandle(), s.GetChannelName(), s.GetTopicName(), s.GetTraceparent()
	case op.GetDeliver() != nil:
		d := op.GetDeliver()
		return opDeliver, d.GetMessage().GetId(), d.GetFromHandle(), d.GetChannelName(), d.GetTopicName(), d.GetTraceparent()
	default:
		return opOther, "", "", "", "", ""
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
	kind, messageID, fromHandle, channelName, topicName, traceparent := classifyOp(op)
	d.calls = append(d.calls, dispatchRecord{sessionID: sessionID, messageID: messageID, kind: kind, fromHandle: fromHandle, channelName: channelName, topicName: topicName, traceparent: traceparent})
	d.mu.Unlock()
	signalObserved(d.recorded)
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

// fakeWaker is a counting AgentWaker: it records WakeAgent calls per agent so a
// test can assert wake-all across offline mentioned members. An optional onWake
// hook lets a test model a wake that resumes the agent (e.g. bind a session) —
// used by the record-vs-wake race case is instead driven at the resolver, so
// onWake stays a general seam.
type fakeWaker struct {
	mu       sync.Mutex
	calls    map[store.AccountID]int
	onWake   func(store.AccountID)
	recorded chan struct{} // buffered; one token per WakeAgent call
}

func newFakeWaker() *fakeWaker {
	return &fakeWaker{calls: map[store.AccountID]int{}, recorded: make(chan struct{}, 1024)}
}

func (w *fakeWaker) WakeAgent(_ context.Context, agent store.AccountID) {
	w.mu.Lock()
	w.calls[agent]++
	hook := w.onWake
	w.mu.Unlock()
	if hook != nil {
		hook(agent)
	}
	signalObserved(w.recorded)
}

func (w *fakeWaker) count(agent store.AccountID) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls[agent]
}

func (w *fakeWaker) total() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, c := range w.calls {
		n += c
	}
	return n
}

// waitForWakes blocks until at least n WakeAgent calls have been recorded, or
// fails at the deadline — event-gated on the per-call signal, never a sleep.
func (w *fakeWaker) waitForWakes(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(testTimeout)
	for {
		if w.total() >= n {
			return
		}
		select {
		case <-w.recorded:
		case <-deadline:
			t.Fatalf("waited for %d wakes, got %d", n, w.total())
		}
	}
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
	handles     map[string]store.Account          // lowercased handle -> resolved account (unknown -> ErrNotFound)
	accounts    map[store.AccountID]store.Account // account id -> account (GetAccount; unknown -> ErrNotFound)
	messages    map[string]store.Message
	// topicNames resolves a topic id to its (channelName, topicName) — the source
	// denorm the deliver/steer op carries (TopicChannelNames). Absent -> the fake
	// returns store.ErrNotFound, so a test can exercise the log-and-empty name-miss
	// path (a name miss never blocks a delivery).
	topicNames map[string]struct{ channelName, topicName string }
	owed       map[store.AccountID]map[store.ChannelID][]store.Message
	// sweepChannels is the D1 disjunct channel set per agent the pin sweep
	// enumerates (SweepChannels). A test seeds the channels a fresh session must
	// visit for pins, independent of owed (which omits channels with no owed
	// messages).
	sweepChannels map[store.AccountID][]store.ChannelID
	// pins is the pinned board per channel (PinnedEntries), ordered as seeded.
	pins map[store.ChannelID][]store.PinnedEntry

	// beforeUndelivered, when set, is called with the swept agent id at the TOP
	// of UndeliveredMessages BEFORE f.mu is acquired — a TEST-ONLY seam (nil in
	// production) so a test can block a sweep's owed-read mid-flight and publish
	// a message INTO the sweep/subscribe window without deadlocking other reads
	// on the fake's lock.
	beforeUndelivered func(store.AccountID)
	// owedMentions is the OwedMentions read per agent (channel -> messages),
	// distinct from `owed` (UndeliveredMessages / the cursor sweep). T2's
	// sweepOwedMentions reads this map. RecordOwedMention appends into it and
	// ClearOwedMention deletes from it, so a test can assert recorded/cleared
	// state after routing.
	owedMentions map[store.AccountID]map[store.ChannelID][]store.Message
	// sweepSet reports InSweepSet per (agent, channel). Absent -> false (out of
	// sweep set), so a test seeds only the in-sweep-set memberships it needs.
	sweepSet map[store.AccountID]map[store.ChannelID]bool
	// recordErr, when set, makes RecordOwedMention fail — a test drives the
	// loud-log-never-fail record edge with it.
	recordErr error
	// afterRecord, when set, is called (with agent, channel, messageID) right
	// after a successful RecordOwedMention returns — a TEST-ONLY seam so a test
	// can flip the recipient live BETWEEN the record and the post-record resolve,
	// exercising the record-vs-wake race's now-live steer.
	afterRecord func(store.AccountID, store.ChannelID, string)
	// unrouted is the ordered committed-message set the recovery scan reads
	// (UnroutedMentionMessages): each row is a message whose settle-edge mention
	// pass never completed (mentions_routed_at IS NULL), ascending seq. A test
	// seeds it with seedUnrouted; MarkMentionsRouted removes the marked id, so
	// the fake mirrors the store's WHERE mentions_routed_at IS NULL predicate.
	unrouted []store.MessageWithChannel
	// marked records every id passed to MarkMentionsRouted, so a test asserts a
	// message's mention pass was marked complete.
	marked map[string]int
	// unroutedCalls records the afterSeq argument of every UnroutedMentionMessages
	// call, so a batch-walk test asserts the scan-local floor advanced.
	unroutedCalls []int64
	// unroutedErr, when set, makes UnroutedMentionMessages fail — a test drives
	// the batch-read-fault-stops-the-scan edge with it.
	unroutedErr error
}

func newFakeReads() *fakeReads {
	return &fakeReads{
		subscribers:   map[store.ChannelID][]store.AccountID{},
		members:       map[store.ChannelID][]store.AccountID{},
		agents:        map[store.AccountID]bool{},
		handles:       map[string]store.Account{},
		accounts:      map[store.AccountID]store.Account{},
		messages:      map[string]store.Message{},
		topicNames:    map[string]struct{ channelName, topicName string }{},
		owed:          map[store.AccountID]map[store.ChannelID][]store.Message{},
		sweepChannels: map[store.AccountID][]store.ChannelID{},
		pins:          map[store.ChannelID][]store.PinnedEntry{},
		owedMentions:  map[store.AccountID]map[store.ChannelID][]store.Message{},
		sweepSet:      map[store.AccountID]map[store.ChannelID]bool{},
		marked:        map[string]int{},
	}
}

// MarkMentionsRouted stamps messageID's mention pass complete: records the mark
// and removes the id from the unrouted set, mirroring the store's UPDATE that
// makes the WHERE mentions_routed_at IS NULL scan skip it thereafter.
func (f *fakeReads) MarkMentionsRouted(_ context.Context, messageID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marked[messageID]++
	kept := f.unrouted[:0:0]
	for _, row := range f.unrouted {
		if string(row.ID) != messageID {
			kept = append(kept, row)
		}
	}
	f.unrouted = kept
	return nil
}

// UnroutedMentionMessages returns the seeded unrouted rows with seq > afterSeq,
// ascending seq, capped at limit — mirroring the store's batched scan read. It
// records afterSeq so a batch-walk test asserts the scan-local floor advanced,
// and returns unroutedErr when set (the batch-read-fault edge).
func (f *fakeReads) UnroutedMentionMessages(_ context.Context, afterSeq int64, limit int) ([]store.MessageWithChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unroutedCalls = append(f.unroutedCalls, afterSeq)
	if f.unroutedErr != nil {
		return nil, f.unroutedErr
	}
	var out []store.MessageWithChannel
	for _, row := range f.unrouted {
		if row.Seq <= afterSeq {
			continue
		}
		out = append(out, row)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// OwedMentions returns the seeded owed-mention set for agent (T2 sweep read).
func (f *fakeReads) OwedMentions(_ context.Context, agent store.AccountID) (map[store.ChannelID][]store.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[store.ChannelID][]store.Message{}
	for ch, msgs := range f.owedMentions[agent] {
		out[ch] = append([]store.Message(nil), msgs...)
	}
	return out, nil
}

// RecordOwedMention appends messageID's message into the owed-mention set for
// agent, keyed by channel — idempotent on message id, mirroring the store's
// ON CONFLICT DO NOTHING. Returns the injected recordErr when set.
func (f *fakeReads) RecordOwedMention(_ context.Context, agent store.AccountID, channel store.ChannelID, messageID string) error {
	f.mu.Lock()
	if f.recordErr != nil {
		err := f.recordErr
		f.mu.Unlock()
		return err
	}
	if f.owedMentions[agent] == nil {
		f.owedMentions[agent] = map[store.ChannelID][]store.Message{}
	}
	dup := false
	for _, m := range f.owedMentions[agent][channel] {
		if string(m.ID) == messageID {
			dup = true
			break
		}
	}
	if !dup {
		m, ok := f.messages[messageID]
		if !ok {
			m = store.Message{ID: store.MessageID(messageID)}
		}
		f.owedMentions[agent][channel] = append(f.owedMentions[agent][channel], m)
	}
	hook := f.afterRecord
	f.mu.Unlock()
	if hook != nil {
		hook(agent, channel, messageID) // outside the lock: the hook flips the resolver, not the fake
	}
	return nil
}

// InSweepSet reports the seeded sweep-set membership for (agent, channel);
// absent is false (out of sweep set).
func (f *fakeReads) InSweepSet(_ context.Context, agent store.AccountID, channel store.ChannelID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sweepSet[agent][channel], nil
}

// ClearOwedMention deletes the owed-mention row for (agent, messageID) across
// all channels — the pool-based sweep-path clear (T2).
func (f *fakeReads) ClearOwedMention(_ context.Context, agent store.AccountID, messageID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch, msgs := range f.owedMentions[agent] {
		kept := msgs[:0:0]
		for _, m := range msgs {
			if string(m.ID) != messageID {
				kept = append(kept, m)
			}
		}
		if len(kept) == 0 {
			delete(f.owedMentions[agent], ch)
		} else {
			f.owedMentions[agent][ch] = kept
		}
	}
	return nil
}

// CountOwedMentions returns the total owed-mention row count across all agents
// (T2 startup observability).
func (f *fakeReads) CountOwedMentions(_ context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, byChan := range f.owedMentions {
		for _, msgs := range byChan {
			n += len(msgs)
		}
	}
	return n, nil
}

// SweepChannels returns the seeded D1 disjunct channel set for agent — the pin
// sweep's channel enumeration, mirroring the store read.
func (f *fakeReads) SweepChannels(_ context.Context, agent store.AccountID) ([]store.ChannelID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sweepChannels[agent], nil
}

// PinnedEntries returns the seeded pinned board for channel, ordered as seeded.
func (f *fakeReads) PinnedEntries(_ context.Context, channel store.ChannelID) ([]store.PinnedEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pins[channel], nil
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
// treatment of an unknown or human handle. The owner param (RIG-2751 handle
// cutover: agent handles are per-owner) is ignored here — the fake models one
// owner namespace, so a handle resolves regardless of the owner passed.
func (f *fakeReads) AgentByHandle(_ context.Context, _ store.AccountID, handle string) (store.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	acc, ok := f.handles[handle]
	if !ok {
		return store.Account{}, store.ErrNotFound
	}
	return acc, nil
}

// ResolveOwner returns the caller itself — the single-owner fake namespace, so
// the author's mention-resolution owner is stable and every seeded handle
// resolves under it (mirrors the store's user-owns-itself fallback).
func (f *fakeReads) ResolveOwner(_ context.Context, caller store.AccountID) (store.AccountID, error) {
	return caller, nil
}

// GetAccount resolves an account by id from the seeded accounts map — the store
// read that denormalizes the author's handle onto the deliver/steer control
// (RIG-2486 T1). An unseeded id is store.ErrNotFound, mirroring the store's
// fail-closed lookup, so a test can exercise the log-and-empty handle-miss path.
func (f *fakeReads) GetAccount(_ context.Context, id store.AccountID) (store.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	acc, ok := f.accounts[id]
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

// MessageChannel resolves a message's channel through its topic. A message
// seeded into the unrouted set (seedUnrouted) carries its own channel, so the
// fake returns THAT — modeling the store's topic->channel join faithfully, incl.
// the multi-channel case the recovery scan must route each message against. Any
// other message id resolves to the single shared channel ("chan-1", the const ch
// most tests declare): the live-post path calls this before any re-read, and the
// real store always resolves a committed message's channel.
func (f *fakeReads) MessageChannel(_ context.Context, messageID string) (store.ChannelID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.unrouted {
		if string(row.ID) == messageID {
			return row.Channel, nil
		}
	}
	return "chan-1", nil
}

// TopicChannelNames resolves a topic id to its seeded (topicName, channelName)
// — the source denorm the deliver/steer op carries (RIG-2956 T0). An unseeded
// topic id is store.ErrNotFound, mirroring the store's fail-closed lookup, so a
// test can exercise the log-and-empty name-miss path.
func (f *fakeReads) TopicChannelNames(_ context.Context, topicID string) (topicName, channelName string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	names, ok := f.topicNames[topicID]
	if !ok {
		return "", "", store.ErrNotFound
	}
	return names.topicName, names.channelName, nil
}

func (f *fakeReads) UndeliveredMessages(_ context.Context, agent store.AccountID) (map[store.ChannelID][]store.Message, error) {
	if f.beforeUndelivered != nil {
		f.beforeUndelivered(agent)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.owed[agent], nil
}

// seedTopicNames registers a topic id's source channel+topic names the
// deliver/steer op resolves through TopicChannelNames.
func (f *fakeReads) seedTopicNames(topicID, channelName, topicName string) { //nolint:unparam // read-clarity signature: topicID names WHICH topic's source names to seed at each call site, though every current caller happens to seed the textMessage default "topic-1" — not dead code.
	f.mu.Lock()
	defer f.mu.Unlock()
	f.topicNames[topicID] = struct{ channelName, topicName string }{channelName: channelName, topicName: topicName}
}

// seedMessage registers a stored message a re-read (settle / sweep) resolves.
func (f *fakeReads) seedMessage(m store.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages[string(m.ID)] = m
}

// seedOwedMention registers a durable owed mention (agent, channel, message) the
// sweepOwedMentions read returns — directly, bypassing RecordOwedMention, so a
// start-edge test can pre-seed an owed row independent of the routing arm. The
// message itself must be seeded separately (seedMessage) for the sweep's re-read
// to resolve it; omit it to model a permanently-unreadable owed row.
func (f *fakeReads) seedOwedMention(agent store.AccountID, channel store.ChannelID, m store.Message) { //nolint:unparam // read-clarity signature: agent names WHICH recipient's owed row to seed at each call site, though every current caller happens to seed the same recipient — not dead code.
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.owedMentions[agent] == nil {
		f.owedMentions[agent] = map[store.ChannelID][]store.Message{}
	}
	f.owedMentions[agent][channel] = append(f.owedMentions[agent][channel], m)
}

// waitForOwed blocks until agent has exactly n owed-mention rows, or fails at the
// deadline — a polling barrier for cases where the owed-row count is the only
// observable effect to gate on, e.g. a nil-waker record (no dispatch/wake
// signal) or a start-edge sweep's ClearOwedMention (owed -> 0 after the sweep).
func (f *fakeReads) waitForOwed(t *testing.T, agent store.AccountID, n int) {
	t.Helper()
	deadline := time.After(testTimeout)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if f.owedCount(agent) == n {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("owed rows for %s = %d, want %d", agent, f.owedCount(agent), n)
		}
	}
}

// owedCount reports how many owed-mention rows are recorded for agent — a test
// accessor over the fake's owed-mention state.
func (f *fakeReads) owedCount(agent store.AccountID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, msgs := range f.owedMentions[agent] {
		n += len(msgs)
	}
	return n
}

// seedUnrouted registers one committed-but-unmarked message the recovery scan
// reads, with its channel and seq. The message itself is also registered
// (seedMessage) so the scan's storeMessageToWire re-read resolves it.
func (f *fakeReads) seedUnrouted(m store.Message, channel store.ChannelID, seq int64) {
	f.seedMessage(m)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unrouted = append(f.unrouted, store.MessageWithChannel{Message: m, Channel: channel, Seq: seq})
}

// markCount reports how many times messageID was marked complete.
func (f *fakeReads) markCount(messageID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.marked[messageID]
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
		TopicID:         "topic-1",
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
		TopicId:         "topic-1",
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
		TopicId:         "topic-1",
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
