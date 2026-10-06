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

// fakeDeliveries is the replay probe: recorded reports a stored post, err a
// failed read. probes records each "author key" it was asked about.
type fakeDeliveries struct {
	mu       sync.Mutex
	recorded bool
	err      error
	probes   []string
}

func (f *fakeDeliveries) MessageRequestRecorded(_ context.Context, author store.AccountID, key string) (bool, error) {
	f.mu.Lock()
	f.probes = append(f.probes, string(author)+" "+key)
	f.mu.Unlock()
	return f.recorded, f.err
}

// recordingClient records the ordered sequence of Linear-side emits ("thought",
// "external-url", "error") and signals thoughts/errors on channels a test gates
// on.
type recordingClient struct {
	mu       sync.Mutex
	events   []string
	bodies   []string
	sessions []string
	urls     []ExternalURL
	errCh    chan struct{}
}

func (c *recordingClient) CreateActivity(_ context.Context, sessionID string, content ActivityContent) error {
	c.mu.Lock()
	c.events = append(c.events, content.Type)
	c.bodies = append(c.bodies, content.Body)
	c.sessions = append(c.sessions, sessionID)
	c.mu.Unlock()
	if content.Type == "error" && c.errCh != nil {
		c.errCh <- struct{}{}
	}
	return nil
}

func (c *recordingClient) UpdateSession(_ context.Context, sessionID string, urls []ExternalURL) error {
	c.mu.Lock()
	c.events = append(c.events, "external-url")
	c.sessions = append(c.sessions, sessionID)
	c.urls = append(c.urls, urls...)
	c.mu.Unlock()
	return nil
}

// count is how many activities of type kind were emitted.
func (c *recordingClient) count(kind string) int {
	return len(c.sessionsFor(kind))
}

// sessionsFor lists the session id of every activity of type kind, in order.
func (c *recordingClient) sessionsFor(kind string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for i, ev := range c.events {
		if ev == kind {
			out = append(out, c.sessions[i])
		}
	}
	return out
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
		Buffer:         4,
		Resolve:        res.resolve,
		Poster:         comms,
		Members:        members,
		Topics:         topics,
		Associations:   assoc,
		Deliveries:     &fakeDeliveries{},
		Client:         client,
		SessionLinkFor: func(id string) string { return "https://compass.rigel.build/l/session/" + id },
		Bridge:         testBridge,
	})
	stop := runDispatcher(t, d)
	defer stop()

	if err := d.Enqueue(&SessionEvent{
		Action:        "created",
		PromptContext: "please do the thing",
		DeliveryID:    "delivery-0",
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
	if len(client.urls) != 1 || client.urls[0].URL != "https://compass.rigel.build/l/session/sess-1" {
		t.Fatalf("external URLs = %+v, want the session link for sess-1", client.urls)
	}
	// Association upserted with the resolved manager/channel/topic.
	if len(assoc.rows) != 1 {
		t.Fatalf("association rows = %d, want 1", len(assoc.rows))
	}
	row := assoc.rows[0]
	if row.ManagerAccountID != "mgr-1" || row.ChannelID != "chan-1" || row.TopicID != "topic-1" ||
		row.LinearIssueID != "iss-1" || row.LinearIssueIdentifier != "RIG-9" {
		t.Fatalf("association row = %+v, want mgr-1/chan-1/topic-1/iss-1/RIG-9", row)
	}
	// Topic named for the issue identifier.
	if len(topics.names) != 1 || topics.names[0] != "RIG-9" {
		t.Fatalf("topic names = %v, want [RIG-9]", topics.names)
	}
	// Post carried the prompt context into the topic with the dedup key.
	if comms.topics[0] != "topic-1" || comms.bodies[0] != "please do the thing" {
		t.Fatalf("post topic/body = %q/%q, want topic-1/please do the thing", comms.topics[0], comms.bodies[0])
	}
	assertReqID(t, comms.reqIDs[0], "delivery-0")
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

// TestDispatcherReplayKeysOnDeliveryID pins the replay dedup: two deliveries of one
// Linear-Delivery id post with the same client_request_id, so the store collapses them.
func TestDispatcherReplayKeysOnDeliveryID(t *testing.T) {
	comms := &recordingComms{posted: make(chan struct{}, 2)}
	d := newTestDispatcher(t, dispatcherDeps{
		res:    &fakeResolver{manager: "mgr", homeChannel: "chan"},
		comms:  comms,
		topics: &fakeTopics{topicID: "topic"},
		assoc:  &fakeAssoc{},
		client: &recordingClient{},
	})
	stop := runDispatcher(t, d)
	defer stop()

	for range 2 {
		ev := &SessionEvent{Action: "created", DeliveryID: "delivery-1", AgentSession: AgentSession{ID: "sess-1"}}
		if err := d.Enqueue(ev); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		<-comms.posted
	}
	assertReqID(t, comms.reqIDs[0], "delivery-1")
	assertReqID(t, comms.reqIDs[1], "delivery-1")
}

// TestDispatcherRecordedDeliverySkipsEverything pins the replay skip: once a
// delivery's post is stored, a replayed created or prompted emits and posts
// nothing, and a failed probe reports an error instead.
func TestDispatcherRecordedDeliverySkipsEverything(t *testing.T) {
	for _, tc := range []struct {
		name       string
		action     string
		deliveries *fakeDeliveries
		wantErrors int
	}{
		{"recorded created", "created", &fakeDeliveries{recorded: true}, 0},
		{"recorded prompted", "prompted", &fakeDeliveries{recorded: true}, 0},
		{"probe error", "created", &fakeDeliveries{err: errors.New("db down")}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comms := &recordingComms{posted: make(chan struct{}, 1)}
			client := &recordingClient{errCh: make(chan struct{}, 1)}
			d := newTestDispatcher(t, dispatcherDeps{
				res:        &fakeResolver{manager: "mgr", homeChannel: "chan"},
				comms:      comms,
				topics:     &fakeTopics{topicID: "topic"},
				assoc:      &fakeAssoc{lookupRow: store.LinearAgentSessionRow{TopicID: "topic", ManagerAccountID: "mgr"}},
				client:     client,
				deliveries: tc.deliveries,
			})
			stop := runDispatcher(t, d)
			defer stop()

			for _, ev := range []*SessionEvent{
				{Action: tc.action, DeliveryID: "delivery-1", AgentSession: AgentSession{ID: "sess-1"}},
				{Action: "unknown"}, // ignored before the probe
				// Undeduped, so it always runs: once it posts, the events before it were handled.
				{Action: "created", AgentSession: AgentSession{ID: "sess-2"}},
			} {
				if err := d.Enqueue(ev); err != nil {
					t.Fatalf("Enqueue: %v", err)
				}
			}
			<-comms.posted
			if got := client.sessionsFor("thought"); !slices.Equal(got, []string{"sess-2"}) {
				t.Errorf("thought sessions = %v, want only sess-2", got)
			}
			if got := len(comms.reqIDs); got != 1 {
				t.Errorf("posts = %d, want 1 (sess-2 only)", got)
			}
			if got := client.sessionsFor("error"); len(got) != tc.wantErrors {
				t.Errorf("error activities = %v, want %d", got, tc.wantErrors)
			}
			// Only the deduped event is probed, under the bridge and the delivery key.
			if want := []string{string(testBridge) + " linear-delivery:delivery-1"}; !slices.Equal(tc.deliveries.probes, want) {
				t.Errorf("probes = %q, want %q", tc.deliveries.probes, want)
			}
		})
	}
}

// TestDispatcherNoDeliveryIDSkipsDedup pins the fallback: with no delivery id the
// key is empty, which the store never dedups, so distinct events are not folded together.
func TestDispatcherNoDeliveryIDSkipsDedup(t *testing.T) {
	comms := &recordingComms{posted: make(chan struct{}, 2)}
	d := newTestDispatcher(t, dispatcherDeps{
		res:    &fakeResolver{manager: "mgr", homeChannel: "chan"},
		comms:  comms,
		topics: &fakeTopics{topicID: "topic"},
		assoc:  &fakeAssoc{},
		client: &recordingClient{},
	})
	stop := runDispatcher(t, d)
	defer stop()

	for _, id := range []string{"sess-a", "sess-b"} {
		if err := d.Enqueue(&SessionEvent{Action: "created", AgentSession: AgentSession{ID: id}}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		<-comms.posted
	}
	if comms.reqIDs[0] != "" || comms.reqIDs[1] != "" {
		t.Fatalf("client_request_ids = %q, want both empty (no dedup without a delivery id)", comms.reqIDs)
	}
}

// TestDispatcherPromptedFollowUp pins the prompted path: a hit posts the
// agentActivity body into the RECORDED channel/topic, keyed on its delivery id,
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
	})
	stop := runDispatcher(t, d)
	defer stop()

	if err := d.Enqueue(&SessionEvent{
		Action:        "prompted",
		DeliveryID:    "delivery-2",
		AgentSession:  AgentSession{ID: "sess-1"},
		AgentActivity: AgentActivity{Content: ActivityBody{Body: "the follow-up prompt"}},
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	<-comms.posted

	if comms.topics[0] != "topic-1" || comms.bodies[0] != "the follow-up prompt" {
		t.Fatalf("post topic/body = %q/%q, want topic-1/the follow-up prompt", comms.topics[0], comms.bodies[0])
	}
	assertReqID(t, comms.reqIDs[0], "delivery-2")
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
		DeliveryID:    "delivery-9",
		AgentSession:  AgentSession{ID: "sess-orphan", Issue: Issue{ID: "iss-orphan", Identifier: "RIG-77"}},
		AgentActivity: AgentActivity{Content: ActivityBody{Body: "orphaned follow-up"}},
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
	if assoc.rows[0].LinearIssueIdentifier != "RIG-77" {
		t.Fatalf("synthesized linear issue identifier = %q, want RIG-77", assoc.rows[0].LinearIssueIdentifier)
	}
	if comms.topics[0] != "topic-9" || comms.bodies[0] != "orphaned follow-up" {
		t.Fatalf("post topic/body = %q/%q, want topic-9/orphaned follow-up", comms.topics[0], comms.bodies[0])
	}
	assertReqID(t, comms.reqIDs[0], "delivery-9")
}

// TestDispatcherEnqueueWhenFull pins the backpressure contract: a full bounded
// channel makes Enqueue return ErrQueueFull (→ HTTP 500 → Linear retries).
func TestDispatcherEnqueueWhenFull(t *testing.T) {
	// Buffer 1, no Run goroutine draining: the first Enqueue fills the channel,
	// the second must fail rather than block.
	d := NewDispatcher(DispatcherParams{
		Buffer:         1,
		Resolve:        (&fakeResolver{}).resolve,
		Poster:         &recordingComms{},
		Members:        &fakeMembers{},
		Topics:         &fakeTopics{},
		Associations:   &fakeAssoc{},
		Deliveries:     &fakeDeliveries{},
		Client:         &recordingClient{},
		SessionLinkFor: func(string) string { return "" },
		Bridge:         testBridge,
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

// assertReqID checks the dedup client_request_id scheme: "linear-delivery:<delivery id>".
func assertReqID(t *testing.T, got, wantDelivery string) {
	t.Helper()
	want := clientRequestIDPrefix + wantDelivery
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
	// deliveries defaults to a probe that reports nothing recorded.
	deliveries Deliveries
}

func newTestDispatcher(t *testing.T, deps dispatcherDeps) *Dispatcher {
	t.Helper()
	if deps.deliveries == nil {
		deps.deliveries = &fakeDeliveries{}
	}
	return NewDispatcher(DispatcherParams{
		Buffer:         4,
		Resolve:        deps.res.resolve,
		Poster:         deps.comms,
		Members:        &fakeMembers{},
		Topics:         deps.topics,
		Associations:   deps.assoc,
		Deliveries:     deps.deliveries,
		Client:         deps.client,
		SessionLinkFor: func(id string) string { return "https://compass.rigel.build/l/session/" + id },
		Bridge:         testBridge,
	})
}

// managerPost builds the comms bus event for a message in topic by author.
func managerPost(topic, author string) *compassv1.SubscribeCommsResponse {
	return &compassv1.SubscribeCommsResponse{Payload: &compassv1.SubscribeCommsResponse_MessagePosted{
		MessagePosted: &compassv1.MessagePosted{Message: &compassv1.Message{TopicId: topic, AuthorAccountId: author}},
	}}
}

// TestDispatcherManagerReplyEmitsResponse pins RIG-4163: the Manager's first post
// in a session's topic emits one `response`, so Linear leaves "Thinking". Posts by
// anyone else, in other topics, or after the response emit nothing more.
func TestDispatcherManagerReplyEmitsResponse(t *testing.T) {
	comms := &recordingComms{posted: make(chan struct{}, 1)}
	client := &recordingClient{}
	d := newTestDispatcher(t, dispatcherDeps{
		res:    &fakeResolver{manager: "mgr-1", homeChannel: "chan-1"},
		comms:  comms,
		topics: &fakeTopics{topicID: "topic-1"},
		assoc:  &fakeAssoc{},
		client: client,
	})
	stop := runDispatcher(t, d)
	defer stop()
	if err := d.Enqueue(&SessionEvent{Action: "created", AgentSession: AgentSession{ID: "sess-1"}}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	<-comms.posted

	d.OnCommsEvent(t.Context(), managerPost("topic-1", string(testBridge))) // the bridge's own prompt post
	d.OnCommsEvent(t.Context(), managerPost("topic-other", "mgr-1"))
	d.OnCommsEvent(t.Context(), managerPost("topic-1", "mgr-1"))
	d.OnCommsEvent(t.Context(), managerPost("topic-1", "mgr-1")) // a second reply
	if got := client.count("response"); got != 1 {
		t.Fatalf("response activities = %d, want exactly 1 (first Manager reply only)", got)
	}
	if got := client.sessionsFor("response"); len(got) != 1 || got[0] != "sess-1" {
		t.Fatalf("response sessions = %v, want [sess-1]", got)
	}
}

// TestDispatcherPromptRearmsResponse pins that a follow-up prompt re-arms the
// response: Linear goes back to "Thinking" on a prompt, so the next reply ends it.
func TestDispatcherPromptRearmsResponse(t *testing.T) {
	comms := &recordingComms{posted: make(chan struct{}, 1)}
	client := &recordingClient{}
	d := newTestDispatcher(t, dispatcherDeps{
		res:    &fakeResolver{manager: "mgr-1", homeChannel: "chan-1"},
		comms:  comms,
		topics: &fakeTopics{topicID: "topic-1"},
		assoc: &fakeAssoc{lookupRow: store.LinearAgentSessionRow{
			LinearSessionID: "sess-1", ManagerAccountID: "mgr-1", ChannelID: "chan-1", TopicID: "topic-1",
		}},
		client: client,
	})
	stop := runDispatcher(t, d)
	defer stop()

	d.OnCommsEvent(t.Context(), managerPost("topic-1", "mgr-1")) // not armed yet
	if err := d.Enqueue(&SessionEvent{Action: "prompted", AgentSession: AgentSession{ID: "sess-1"}}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	<-comms.posted
	d.OnCommsEvent(t.Context(), managerPost("topic-1", "mgr-1"))
	if got := client.count("response"); got != 1 {
		t.Fatalf("response activities = %d, want 1 (only the reply after the prompt)", got)
	}
}
