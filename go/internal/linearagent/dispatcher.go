package linearagent

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// ackThoughtBody is the receipt the dispatcher emits on a `created` event — the
// 10-second liveness SLA leg (linear.app/developers/agent-interaction §Session
// webhooks): Linear marks a session unresponsive unless the agent emits within
// 10s. It tells the human what happened and pairs with the stable session link
// as the whole Option B return path (§Part 3).
const ackThoughtBody = "Compass received the session; opening in Compass\u2026"

// externalURLLabel is the label on the stable session return link, which resolves
// to the current Compass channel when the human opens it (§Part 3).
const externalURLLabel = "Open in Compass"

// replyResponseBody is the `response` emitted on the Manager's first reply after
// a prompt; a `response` is what ends Linear's "Thinking" state (RIG-4163).
const replyResponseBody = "Compass replied in the conversation; open it in Compass to continue."

// clientRequestIDPrefix namespaces the comms-rail idempotency key the dispatcher
// stamps on every PostAsAccount so a redelivered webhook never double-posts
// (§Part 1 message-level dedup). The full key is "linear-delivery:<Linear-Delivery id>".
const clientRequestIDPrefix = "linear-delivery:"

// ErrQueueFull is returned by Enqueue when the bounded channel is full. The HTTP
// handler maps it to a 500 so Linear retries the delivery rather than the event
// being silently dropped (§T6: full -> 500 so Linear retries).
var ErrQueueFull = errors.New("linearagent: dispatch queue full")

// ResolveFunc is the routing seam (T4's ResolveResponder): it maps a verified
// session event to the Manager account that owns the delegated work and that
// Manager's home channel. Injected as a func so T6 never imports T4's concrete
// routing type — the driver wires the real ResolveResponder at assembly. The
// signature matches the shared contract byte-for-byte.
type ResolveFunc func(ctx context.Context, ev *SessionEvent) (managerAccountID store.AccountID, homeChannelID string, err error)

// CommsPoster posts a message as an account into the resolved topic. *comms.Comms
// satisfies it via PostAsAccount.
type CommsPoster interface {
	PostAsAccount(ctx context.Context, account store.AccountID, req *compassv1.PostMessageRequest) (*compassv1.PostMessageResponse, error)
}

// Memberships ensures the @linear bridge account is a member of a channel — the
// postSetupThread precondition. *store.Store satisfies it via EnsureChannelMember
// (which takes a store.ChannelID; the dispatcher converts the resolver's string
// home-channel id at the call site).
type Memberships interface {
	EnsureChannelMember(ctx context.Context, channelID store.ChannelID, account store.AccountID) error
}

// Topics get-or-creates the comms topic the Linear conversation lands in,
// returning its id. Named for the issue identifier (else the session id).
type Topics interface {
	GetOrCreateTopic(ctx context.Context, channelID, name string, author store.AccountID) (topicID string, err error)
}

// Associations is the T3 store seam: the durable link between a Linear session
// and the Compass conversation it routed to. *store.Store satisfies it.
type Associations interface {
	UpsertLinearAgentSession(ctx context.Context, row store.LinearAgentSessionRow) (created bool, err error)
	LinearAgentSession(ctx context.Context, linearSessionID string) (store.LinearAgentSessionRow, error)
}

// Deliveries reports whether a delivery's post is already stored, so a replay
// repeats no step the first delivery finished. *store.Store satisfies it.
type Deliveries interface {
	MessageRequestRecorded(ctx context.Context, author store.AccountID, clientRequestID string) (bool, error)
}

// DispatcherParams carries every dependency the Dispatcher needs, all narrow
// seams (never concrete server types) so the drain loop depends on behavior, not
// packages. The driver wires the concrete implementations at assembly.
type DispatcherParams struct {
	// Buffer is the bounded channel capacity. A full channel makes Enqueue
	// return ErrQueueFull (-> HTTP 500 -> Linear retries).
	Buffer int
	// Resolve is T4's ResolveResponder.
	Resolve ResolveFunc
	// Poster is the comms post seam (*comms.Comms).
	Poster CommsPoster
	// Members is the channel-membership seam (*store.Store).
	Members Memberships
	// Topics is the topic get-or-create seam.
	Topics Topics
	// Associations is the T3 store association seam (*store.Store).
	Associations Associations
	// Deliveries is the replay probe (*store.Store).
	Deliveries Deliveries
	// Client is the T2 Linear API client (CreateActivity + UpdateSession).
	Client Client
	// SessionLinkFor builds the stable return link from the Linear session id.
	SessionLinkFor func(linearSessionID string) string
	// Bridge is the seeded @linear bridge system account id (T3a).
	Bridge store.AccountID
}

// Dispatcher drains a bounded channel of verified session events on a single
// goroutine, routing each to a Manager, emitting the Linear-side return path,
// and posting into Compass. It never crashes on a per-event failure — a bad
// event is logged (and an `error` activity emitted to Linear) and the loop
// moves on. It does not mirror agent output: beyond the two `created` emits, it
// emits one `response` on the Manager's first reply after each prompt.
type Dispatcher struct {
	ch             chan *SessionEvent
	resolve        ResolveFunc
	poster         CommsPoster
	members        Memberships
	topics         Topics
	assoc          Associations
	deliveries     Deliveries
	client         Client
	sessionLinkFor func(linearSessionID string) string
	bridge         store.AccountID

	// awaiting maps a session's topic to the reply that ends its "Thinking"
	// state. In memory: a restart drops it, leaving that one session in Thinking.
	mu       sync.Mutex
	awaiting map[string]awaitedReply
}

// awaitedReply is the Manager reply a session's topic is armed for.
type awaitedReply struct {
	sessionID string
	manager   store.AccountID
}

// NewDispatcher builds a Dispatcher from params. Buffer defaults to 1 when
// non-positive.
func NewDispatcher(p DispatcherParams) *Dispatcher {
	buf := p.Buffer
	if buf <= 0 {
		buf = 1
	}
	return &Dispatcher{
		ch:             make(chan *SessionEvent, buf),
		resolve:        p.Resolve,
		poster:         p.Poster,
		members:        p.Members,
		topics:         p.Topics,
		assoc:          p.Associations,
		deliveries:     p.Deliveries,
		client:         p.Client,
		sessionLinkFor: p.SessionLinkFor,
		bridge:         p.Bridge,
		awaiting:       make(map[string]awaitedReply),
	}
}

// OnCommsEvent emits one `response` on the Manager's first post in an armed
// session topic, then disarms it. Called from the comms bus tail; the Linear
// emit is best-effort and only logged on failure.
func (d *Dispatcher) OnCommsEvent(ctx context.Context, resp *compassv1.SubscribeCommsResponse) {
	msg := resp.GetMessagePosted().GetMessage()
	if msg == nil {
		return
	}
	d.mu.Lock()
	want, ok := d.awaiting[msg.GetTopicId()]
	if !ok || string(want.manager) != msg.GetAuthorAccountId() {
		d.mu.Unlock()
		return
	}
	delete(d.awaiting, msg.GetTopicId())
	d.mu.Unlock()
	if err := d.client.CreateActivity(ctx, want.sessionID, ActivityContent{Type: "response", Body: replyResponseBody}); err != nil {
		slog.ErrorContext(ctx, "linearagent dispatcher: response activity failed",
			"linear_session_id", want.sessionID, "error", err)
	}
}

// Enqueue offers a verified event to the bounded channel without blocking. A
// full channel returns ErrQueueFull so the HTTP handler returns 500 and Linear
// retries — an event is never silently dropped.
func (d *Dispatcher) Enqueue(ev *SessionEvent) error {
	select {
	case d.ch <- ev:
		return nil
	default:
		return ErrQueueFull
	}
}

// Run drains the channel until ctx is cancelled, handling one event at a time.
// It returns ctx.Err() on cancel. A per-event failure never stops the loop.
func (d *Dispatcher) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-d.ch:
			d.handle(ctx, ev)
		}
	}
}

// armReply waits for the Manager's next post in topicID to end sessionID's
// "Thinking" state. Armed before the prompt post so a fast reply cannot slip by.
func (d *Dispatcher) armReply(topicID, sessionID string, manager store.AccountID) {
	d.mu.Lock()
	d.awaiting[topicID] = awaitedReply{sessionID: sessionID, manager: manager}
	d.mu.Unlock()
}

// handle processes one event, converting any failure (including a panic in a
// seam) into a logged `error` activity to Linear so a single bad event can never
// crash the drain loop.
func (d *Dispatcher) handle(ctx context.Context, ev *SessionEvent) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "linearagent dispatcher: recovered from panic",
				"linear_session_id", ev.AgentSession.ID, "action", ev.Action, "panic", r)
			d.emitError(ctx, ev.AgentSession.ID, errors.New("internal error handling event"))
		}
	}()
	if err := d.process(ctx, ev); err != nil {
		slog.ErrorContext(ctx, "linearagent dispatcher: event failed",
			"linear_session_id", ev.AgentSession.ID, "action", ev.Action, "error", err)
		d.emitError(ctx, ev.AgentSession.ID, err)
	}
}

// process routes one event by action. Unknown actions are ignored (no error) —
// only created/prompted drive the responder, and only they cost the replay probe.
func (d *Dispatcher) process(ctx context.Context, ev *SessionEvent) error {
	if ev.Action != "created" && ev.Action != "prompted" {
		return nil
	}
	// A replay whose post is stored already finished every step; re-running would
	// repeat the ack thought and session update, and re-arm a spent reply.
	key := clientRequestID(ctx, ev)
	if key != "" {
		done, err := d.deliveries.MessageRequestRecorded(ctx, d.bridge, key)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	if ev.Action == "created" {
		return d.handleCreated(ctx, ev, key)
	}
	return d.handlePrompted(ctx, ev, key)
}

// handleCreated runs the `created` chain: resolve -> ensure @linear membership
// -> get-or-create the topic -> upsert the association -> emit the ack thought
// AND the session external URL (the 10s SLA leg, BEFORE the post) -> post the
// prompt context into the topic with the dedup client_request_id.
func (d *Dispatcher) handleCreated(ctx context.Context, ev *SessionEvent, key string) error {
	manager, homeChannel, err := d.resolve(ctx, ev)
	if err != nil {
		return err
	}
	if err := d.members.EnsureChannelMember(ctx, store.ChannelID(homeChannel), d.bridge); err != nil {
		return err
	}
	topicID, err := d.topics.GetOrCreateTopic(ctx, homeChannel, topicName(ev), d.bridge)
	if err != nil {
		return err
	}
	if _, err := d.assoc.UpsertLinearAgentSession(ctx, store.LinearAgentSessionRow{
		LinearSessionID:       ev.AgentSession.ID,
		ManagerAccountID:      manager,
		ChannelID:             store.ChannelID(homeChannel),
		TopicID:               topicID,
		LinearIssueID:         ev.AgentSession.Issue.ID,
		LinearIssueIdentifier: ev.AgentSession.Issue.Identifier,
	}); err != nil {
		return err
	}
	// The 10s SLA leg: ack thought + session external URL, BEFORE the post.
	if err := d.client.CreateActivity(ctx, ev.AgentSession.ID, ActivityContent{Type: "thought", Body: ackThoughtBody}); err != nil {
		return err
	}
	if err := d.client.UpdateSession(ctx, ev.AgentSession.ID, []ExternalURL{{
		Label: externalURLLabel,
		URL:   d.sessionLinkFor(ev.AgentSession.ID),
	}}); err != nil {
		return err
	}
	d.armReply(topicID, ev.AgentSession.ID, manager)
	return d.post(ctx, homeChannel, topicID, ev.PromptContext, key)
}

// handlePrompted routes a follow-up to the recorded conversation: look up the
// association and post into its channel/topic. On a miss (a prompted event with
// no `created` on record) it synthesizes the association via the resolver from
// the payload's agentSession, then posts.
func (d *Dispatcher) handlePrompted(ctx context.Context, ev *SessionEvent, key string) error {
	row, err := d.assoc.LinearAgentSession(ctx, ev.AgentSession.ID)
	switch {
	case err == nil:
		d.armReply(row.TopicID, ev.AgentSession.ID, row.ManagerAccountID)
		return d.post(ctx, string(row.ChannelID), row.TopicID, ev.AgentActivity.Content.Body, key)
	case errors.Is(err, store.ErrNotFound):
		manager, homeChannel, resErr := d.resolve(ctx, ev)
		if resErr != nil {
			return resErr
		}
		if memErr := d.members.EnsureChannelMember(ctx, store.ChannelID(homeChannel), d.bridge); memErr != nil {
			return memErr
		}
		topicID, topErr := d.topics.GetOrCreateTopic(ctx, homeChannel, topicName(ev), d.bridge)
		if topErr != nil {
			return topErr
		}
		if _, upErr := d.assoc.UpsertLinearAgentSession(ctx, store.LinearAgentSessionRow{
			LinearSessionID:       ev.AgentSession.ID,
			ManagerAccountID:      manager,
			ChannelID:             store.ChannelID(homeChannel),
			TopicID:               topicID,
			LinearIssueID:         ev.AgentSession.Issue.ID,
			LinearIssueIdentifier: ev.AgentSession.Issue.Identifier,
		}); upErr != nil {
			return upErr
		}
		d.armReply(topicID, ev.AgentSession.ID, manager)
		return d.post(ctx, homeChannel, topicID, ev.AgentActivity.Content.Body, key)
	default:
		return err
	}
}

// post writes one message as the @linear bridge account into channel/topic under
// the event's dedup client_request_id.
func (d *Dispatcher) post(ctx context.Context, channelID, topicID, body, clientRequestID string) error {
	_, err := d.poster.PostAsAccount(ctx, d.bridge, &compassv1.PostMessageRequest{
		Container:       &compassv1.PostMessageRequest_ChannelId{ChannelId: channelID},
		Topic:           &compassv1.PostMessageRequest_TopicId{TopicId: topicID},
		Blocks:          []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: body}}},
		ClientRequestId: clientRequestID,
	})
	return err
}

// emitError best-effort posts an `error` activity to Linear for a failed event.
// It never propagates: the drain loop keeps going regardless.
func (d *Dispatcher) emitError(ctx context.Context, sessionID string, cause error) {
	if err := d.client.CreateActivity(ctx, sessionID, ActivityContent{Type: "error", Body: cause.Error()}); err != nil {
		slog.ErrorContext(ctx, "linearagent dispatcher: emitting error activity failed",
			"linear_session_id", sessionID, "error", err)
	}
}

// clientRequestID keys an event's one post on its Linear-Delivery id, so a replayed
// delivery collapses onto the stored row. No id means an empty key: the post is not deduped.
func clientRequestID(ctx context.Context, ev *SessionEvent) string {
	if ev.DeliveryID == "" {
		slog.WarnContext(ctx, "linearagent dispatcher: session event has no valid Linear-Delivery id; replay dedup off",
			"linear_session_id", ev.AgentSession.ID)
		return ""
	}
	return clientRequestIDPrefix + ev.DeliveryID
}

// topicName is the issue identifier when present, else the session id.
func topicName(ev *SessionEvent) string {
	if ev.AgentSession.Issue.Identifier != "" {
		return ev.AgentSession.Issue.Identifier
	}
	return ev.AgentSession.ID
}
