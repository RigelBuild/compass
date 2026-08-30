package linearagent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// The fakes below are the narrow seams the Dispatcher depends on. Each records
// its calls and (where a test gates on completion) signals on a channel so the
// test blocks on the real event, never a clock.

// fakeResolver records calls and returns a fixed manager/home channel or a
// preset error.
type fakeResolver struct {
	manager     store.AccountID
	homeChannel string
	err         error
	calls       int
}

func (f *fakeResolver) resolve(_ context.Context, _ *SessionEvent) (store.AccountID, string, error) {
	f.calls++
	if f.err != nil {
		return "", "", f.err
	}
	return f.manager, f.homeChannel, nil
}

// recordingComms records each PostAsAccount and signals on posted. reqIDs
// captures the client_request_id of every post.
type recordingComms struct {
	mu     sync.Mutex
	reqIDs []string
	topics []string
	bodies []string
	err    error
	posted chan struct{}
}

func (c *recordingComms) PostAsAccount(_ context.Context, _ store.AccountID, req *compassv1.PostMessageRequest) (*compassv1.PostMessageResponse, error) {
	c.mu.Lock()
	c.reqIDs = append(c.reqIDs, req.GetClientRequestId())
	c.topics = append(c.topics, req.GetTopicId())
	if len(req.GetBlocks()) > 0 {
		c.bodies = append(c.bodies, req.GetBlocks()[0].GetText())
	}
	c.mu.Unlock()
	if c.posted != nil {
		c.posted <- struct{}{}
	}
	return &compassv1.PostMessageResponse{}, c.err
}

// fakeMembers records EnsureChannelMember calls.
type fakeMembers struct {
	mu       sync.Mutex
	channels []string
	accounts []store.AccountID
	err      error
}

func (m *fakeMembers) EnsureChannelMember(_ context.Context, channelID store.ChannelID, account store.AccountID) error {
	m.mu.Lock()
	m.channels = append(m.channels, string(channelID))
	m.accounts = append(m.accounts, account)
	m.mu.Unlock()
	return m.err
}

// fakeTopics returns a fixed topic id.
type fakeTopics struct {
	topicID string
	names   []string
	err     error
}

func (tk *fakeTopics) GetOrCreateTopic(_ context.Context, _, name string, _ store.AccountID) (string, error) {
	tk.names = append(tk.names, name)
	if tk.err != nil {
		return "", tk.err
	}
	return tk.topicID, nil
}

// fakeAssoc is the T3 association seam. rows holds every upsert; lookup returns
// lookupRow/lookupErr on LinearAgentSession.
type fakeAssoc struct {
	mu        sync.Mutex
	rows      []store.LinearAgentSessionRow
	lookupRow store.LinearAgentSessionRow
	lookupErr error
}

func (a *fakeAssoc) UpsertLinearAgentSession(_ context.Context, row store.LinearAgentSessionRow) (bool, error) {
	a.mu.Lock()
	a.rows = append(a.rows, row)
	a.mu.Unlock()
	return true, nil
}

func (a *fakeAssoc) LinearAgentSession(_ context.Context, _ string) (store.LinearAgentSessionRow, error) {
	if a.lookupErr != nil {
		return store.LinearAgentSessionRow{}, a.lookupErr
	}
	return a.lookupRow, nil
}

// recordingClient records the ordered sequence of Linear-side emits ("thought",
// "external-url", "error") and signals thoughts/errors on channels a test gates
// on.
type recordingClient struct {
	mu     sync.Mutex
	events []string
	bodies []string
	errCh  chan struct{}
}

func (c *recordingClient) CreateActivity(_ context.Context, _ string, content ActivityContent) error {
	c.mu.Lock()
	c.events = append(c.events, content.Type)
	c.bodies = append(c.bodies, content.Body)
	c.mu.Unlock()
	if content.Type == "error" && c.errCh != nil {
		c.errCh <- struct{}{}
	}
	return nil
}

func (c *recordingClient) UpdateSession(_ context.Context, _ string, _ []ExternalURL) error {
	c.mu.Lock()
	c.events = append(c.events, "external-url")
	c.mu.Unlock()
	return nil
}

func (c *recordingClient) seq() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

const testBridge store.AccountID = "acct-linear"

// runDispatcher starts d.Run on a fresh goroutine and returns a stop func that
// cancels it and waits for exit — the deterministic lifecycle every test uses.
func runDispatcher(t *testing.T, d *Dispatcher) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = d.Run(ctx) // returns ctx.Err() on cancel; not an assertion target
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
}

// TestDispatcherCreatedHappyPath pins the created chain's ORDER: membership
// first, then the two Linear-side emits (thought, external-url) BEFORE the post,
// and the dedup client_request_id format.
func TestDispatcherCreatedHappyPath(t *testing.T) {
	res := &fakeResolver{manager: "mgr-1", homeChannel: "chan-1"}
	comms := &recordingComms{posted: make(chan struct{}, 1)}
	members := &fakeMembers{}
	topics := &fakeTopics{topicID: "topic-1"}
	assoc := &fakeAssoc{}
	client := &recordingClient{}

	d := NewDispatcher(DispatcherParams{
		Buffer:       4,
		Resolve:      res.resolve,
		Poster:       comms,
		Members:      members,
		Topics:       topics,
		Associations: assoc,
		Client:       client,
		DeepLinkFor:  func(ch string) string { return "https://compass.rigel.build/c/" + ch },
		Bridge:       testBridge,
		NewRequestID: func() string { return "fixed-uuid" },
	})
	stop := runDispatcher(t, d)
	defer stop()

	if err := d.Enqueue(&SessionEvent{
		Action:        "created",
		PromptContext: "please do the thing",
		AgentSession:  AgentSession{ID: "sess-1", Issue: Issue{ID: "iss-1", Identifier: "RIG-9"}},
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	<-comms.posted // gate on the post completing

	// Membership ensured for @linear into the manager's home channel.
	if len(members.channels) != 1 || members.channels[0] != "chan-1" || members.accounts[0] != testBridge {
		t.Fatalf("EnsureChannelMember = %v/%v, want chan-1/%s", members.channels, members.accounts, testBridge)
	}
	// Emit order: thought, then external-url, then the post (post is gated, so
	// both emits must already be recorded).
	if got := client.seq(); len(got) != 2 || got[0] != "thought" || got[1] != "external-url" {
		t.Fatalf("emit sequence = %v, want [thought external-url] before the post", got)
	}
	// Association upserted with the resolved manager/channel/topic.
	if len(assoc.rows) != 1 {
		t.Fatalf("association rows = %d, want 1", len(assoc.rows))
	}
	row := assoc.rows[0]
	if row.ManagerAccountID != "mgr-1" || row.ChannelID != "chan-1" || row.TopicID != "topic-1" || row.LinearIssueID != "iss-1" {
		t.Fatalf("association row = %+v, want mgr-1/chan-1/topic-1/iss-1", row)
	}
	// Topic named for the issue identifier.
	if len(topics.names) != 1 || topics.names[0] != "RIG-9" {
		t.Fatalf("topic names = %v, want [RIG-9]", topics.names)
	}
	// Post carried the prompt context into the topic with the dedup key.
	if comms.topics[0] != "topic-1" || comms.bodies[0] != "please do the thing" {
		t.Fatalf("post topic/body = %q/%q, want topic-1/please do the thing", comms.topics[0], comms.bodies[0])
	}
	assertReqID(t, comms.reqIDs[0], "fixed-uuid")
}

// TestDispatcherCreatedTopicNameFallsBackToSession pins the topic-name fallback:
// no issue identifier → the session id.
func TestDispatcherCreatedTopicNameFallsBackToSession(t *testing.T) {
	comms := &recordingComms{posted: make(chan struct{}, 1)}
	topics := &fakeTopics{topicID: "topic-x"}
	d := newTestDispatcher(t, dispatcherDeps{
		res:    &fakeResolver{manager: "mgr", homeChannel: "chan"},
		comms:  comms,
		topics: topics,
		assoc:  &fakeAssoc{},
		client: &recordingClient{},
	})
	stop := runDispatcher(t, d)
	defer stop()

	if err := d.Enqueue(&SessionEvent{Action: "created", AgentSession: AgentSession{ID: "sess-noissue"}}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	<-comms.posted
	if len(topics.names) != 1 || topics.names[0] != "sess-noissue" {
		t.Fatalf("topic names = %v, want [sess-noissue] (session-id fallback)", topics.names)
	}
}

// TestDispatcherPromptedFollowUp pins the prompted path: a hit posts the
// agentActivity body into the RECORDED channel/topic, with a fresh dedup key,
// and never re-runs the created-side emits.
func TestDispatcherPromptedFollowUp(t *testing.T) {
	comms := &recordingComms{posted: make(chan struct{}, 1)}
	client := &recordingClient{}
	assoc := &fakeAssoc{lookupRow: store.LinearAgentSessionRow{
		LinearSessionID: "sess-1", ManagerAccountID: "mgr-1", ChannelID: "chan-1", TopicID: "topic-1",
	}}
	d := newTestDispatcher(t, dispatcherDeps{
		res:    &fakeResolver{},
		comms:  comms,
		topics: &fakeTopics{topicID: "should-not-be-used"},
		assoc:  assoc,
		client: client,
		reqID:  func() string { return "uuid-2" },
	})
	stop := runDispatcher(t, d)
	defer stop()

	if err := d.Enqueue(&SessionEvent{
		Action:        "prompted",
		AgentSession:  AgentSession{ID: "sess-1"},
		AgentActivity: AgentActivity{Body: "the follow-up prompt"},
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	<-comms.posted

	if comms.topics[0] != "topic-1" || comms.bodies[0] != "the follow-up prompt" {
		t.Fatalf("post topic/body = %q/%q, want topic-1/the follow-up prompt", comms.topics[0], comms.bodies[0])
	}
	assertReqID(t, comms.reqIDs[0], "uuid-2")
	// No created-side emits on a follow-up.
	if got := client.seq(); len(got) != 0 {
		t.Fatalf("prompted emitted Linear activities %v, want none", got)
	}
}

// TestDispatcherPromptedMissSynthesizes pins the prompted-miss path: a lookup
// miss (ErrNotFound) synthesizes the association via the resolver, then posts.
func TestDispatcherPromptedMissSynthesizes(t *testing.T) {
	comms := &recordingComms{posted: make(chan struct{}, 1)}
	res := &fakeResolver{manager: "mgr-9", homeChannel: "chan-9"}
	assoc := &fakeAssoc{lookupErr: fmt.Errorf("%w: no such session", store.ErrNotFound)}
	d := newTestDispatcher(t, dispatcherDeps{
		res:    res,
		comms:  comms,
		topics: &fakeTopics{topicID: "topic-9"},
		assoc:  assoc,
		client: &recordingClient{},
	})
	stop := runDispatcher(t, d)
	defer stop()

	if err := d.Enqueue(&SessionEvent{
		Action:        "prompted",
		AgentSession:  AgentSession{ID: "sess-orphan"},
		AgentActivity: AgentActivity{Body: "orphaned follow-up"},
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	<-comms.posted

	if res.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1 (synthesis on miss)", res.calls)
	}
	if len(assoc.rows) != 1 || assoc.rows[0].ChannelID != "chan-9" || assoc.rows[0].TopicID != "topic-9" {
		t.Fatalf("synthesized association = %+v, want chan-9/topic-9", assoc.rows)
	}
	if comms.topics[0] != "topic-9" || comms.bodies[0] != "orphaned follow-up" {
		t.Fatalf("post topic/body = %q/%q, want topic-9/orphaned follow-up", comms.topics[0], comms.bodies[0])
	}
}

// TestDispatcherEnqueueWhenFull pins the backpressure contract: a full bounded
// channel makes Enqueue return ErrQueueFull (→ HTTP 500 → Linear retries).
func TestDispatcherEnqueueWhenFull(t *testing.T) {
	// Buffer 1, no Run goroutine draining: the first Enqueue fills the channel,
	// the second must fail rather than block.
	d := NewDispatcher(DispatcherParams{
		Buffer:       1,
		Resolve:      (&fakeResolver{}).resolve,
		Poster:       &recordingComms{},
		Members:      &fakeMembers{},
		Topics:       &fakeTopics{},
		Associations: &fakeAssoc{},
		Client:       &recordingClient{},
		DeepLinkFor:  func(string) string { return "" },
		Bridge:       testBridge,
	})
	if err := d.Enqueue(&SessionEvent{Action: "created"}); err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	if err := d.Enqueue(&SessionEvent{Action: "created"}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("second Enqueue error = %v, want ErrQueueFull", err)
	}
}

// TestDispatcherPerEventFailureKeepsDraining pins the never-crash contract: a
// failing event emits an `error` activity AND the loop keeps draining the next,
// good event. A panic/return on the bad event would hang the good event's post
// and fail the test.
func TestDispatcherPerEventFailureKeepsDraining(t *testing.T) {
	comms := &recordingComms{posted: make(chan struct{}, 1)}
	client := &recordingClient{errCh: make(chan struct{}, 1)}
	// First event resolves with an error; the resolver flips to success after.
	res := &flakyResolver{errFor: "bad", manager: "mgr", homeChannel: "chan"}
	d := newTestDispatcher(t, dispatcherDeps{
		res:    res,
		comms:  comms,
		topics: &fakeTopics{topicID: "topic"},
		assoc:  &fakeAssoc{},
		client: client,
	})
	stop := runDispatcher(t, d)
	defer stop()

	if err := d.Enqueue(&SessionEvent{Action: "created", AgentSession: AgentSession{ID: "bad"}}); err != nil {
		t.Fatalf("Enqueue(bad): %v", err)
	}
	<-client.errCh // the failing event emitted an `error` activity

	if err := d.Enqueue(&SessionEvent{Action: "created", AgentSession: AgentSession{ID: "good", Issue: Issue{Identifier: "RIG-1"}}}); err != nil {
		t.Fatalf("Enqueue(good): %v", err)
	}
	<-comms.posted // the loop kept draining and handled the good event

	if got := lastErrorBody(client); got == "" {
		t.Fatal("failing event did not emit an `error` activity")
	}
}

// flakyResolver errors for the event whose session id equals errFor, else
// returns manager/homeChannel.
type flakyResolver struct {
	errFor      string
	manager     store.AccountID
	homeChannel string
}

func (f *flakyResolver) resolve(_ context.Context, ev *SessionEvent) (store.AccountID, string, error) {
	if ev.AgentSession.ID == f.errFor {
		return "", "", errors.New("routing failed")
	}
	return f.manager, f.homeChannel, nil
}

func lastErrorBody(c *recordingClient) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range slices.Backward(c.events) {
		if c.events[i] == "error" {
			return c.bodies[i]
		}
	}
	return ""
}

// assertReqID checks the dedup client_request_id scheme: "linear-delivery:<uuid>".
func assertReqID(t *testing.T, got, wantUUID string) {
	t.Helper()
	want := clientRequestIDPrefix + wantUUID
	if got != want {
		t.Fatalf("client_request_id = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "linear-delivery:") {
		t.Fatalf("client_request_id = %q, want the linear-delivery: dedup prefix", got)
	}
}

// dispatcherDeps + newTestDispatcher cut the boilerplate for the common wiring.
type dispatcherDeps struct {
	res interface {
		resolve(ctx context.Context, ev *SessionEvent) (store.AccountID, string, error)
	}
	comms  CommsPoster
	topics Topics
	assoc  Associations
	client Client
	reqID  func() string
}

func newTestDispatcher(t *testing.T, deps dispatcherDeps) *Dispatcher {
	t.Helper()
	reqID := deps.reqID
	if reqID == nil {
		reqID = func() string { return "fixed-uuid" }
	}
	return NewDispatcher(DispatcherParams{
		Buffer:       4,
		Resolve:      deps.res.resolve,
		Poster:       deps.comms,
		Members:      &fakeMembers{},
		Topics:       deps.topics,
		Associations: deps.assoc,
		Client:       deps.client,
		DeepLinkFor:  func(ch string) string { return "https://compass.rigel.build/c/" + ch },
		Bridge:       testBridge,
		NewRequestID: reqID,
	})
}
