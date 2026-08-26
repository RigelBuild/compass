package linearagent

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// ackThoughtBody is the receipt the dispatcher emits on a `created` event — the
// 10-second liveness SLA leg (linear.app/developers/agent-interaction §Session
// webhooks): Linear marks a session unresponsive unless the agent emits within
// 10s. It tells the human what happened and pairs with the session external URL
// (the "Open in Compass" deep link) as the whole Option B return path (§Part 3).
const ackThoughtBody = "Compass received the session; opening in Compass\u2026"

// externalURLLabel is the label on the session external URL entry — the
// "Open in Compass" deep link to the resolved Manager's home channel (§Part 3).
const externalURLLabel = "Open in Compass"

// clientRequestIDPrefix namespaces the comms-rail idempotency key the dispatcher
// stamps on every PostAsAccount so a redelivered webhook never double-posts
// (§Part 1 message-level dedup). The full key is "linear-delivery:<uuid>".
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
	// Client is the T2 Linear API client (CreateActivity + UpdateSession).
	Client Client
	// DeepLinkFor is T5's deep-link builder, taken as a func seam because T5's
	// builder lives in go/server (no import from this package).
	DeepLinkFor func(channelID string) string
	// Bridge is the seeded @linear bridge system account id (T3a).
	Bridge store.AccountID
	// NewRequestID mints the uuid half of a client_request_id. Injectable for
	// deterministic tests; defaults to uuid.NewString.
	NewRequestID func() string
}

// Dispatcher drains a bounded channel of verified session events on a single
// goroutine, routing each to a Manager, emitting the Linear-side return path,
// and posting into Compass. It never crashes on a per-event failure — a bad
// event is logged (and an `error` activity emitted to Linear) and the loop
// moves on. There is NO relay: the dispatcher does not observe or mirror agent
// output; the return path is the two `created` emits only (§Part 3).
type Dispatcher struct {
	ch           chan *SessionEvent
	resolve      ResolveFunc
	poster       CommsPoster
	members      Memberships
	topics       Topics
	assoc        Associations
	client       Client
	deepLinkFor  func(channelID string) string
	bridge       store.AccountID
	newRequestID func() string
}

// NewDispatcher builds a Dispatcher from params. Buffer defaults to 1 when
// non-positive; NewRequestID defaults to uuid.NewString.
func NewDispatcher(p DispatcherParams) *Dispatcher {
	buf := p.Buffer
	if buf <= 0 {
		buf = 1
	}
	newRequestID := p.NewRequestID
	if newRequestID == nil {
		newRequestID = uuid.NewString
	}
	return &Dispatcher{
		ch:           make(chan *SessionEvent, buf),
		resolve:      p.Resolve,
		poster:       p.Poster,
		members:      p.Members,
		topics:       p.Topics,
		assoc:        p.Associations,
		client:       p.Client,
		deepLinkFor:  p.DeepLinkFor,
		bridge:       p.Bridge,
		newRequestID: newRequestID,
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
// only created/prompted drive the responder.
func (d *Dispatcher) process(ctx context.Context, ev *SessionEvent) error {
	switch ev.Action {
	case "created":
		return d.handleCreated(ctx, ev)
	case "prompted":
		return d.handlePrompted(ctx, ev)
	default:
		return nil
	}
}

// handleCreated runs the `created` chain: resolve -> ensure @linear membership
// -> get-or-create the topic -> upsert the association -> emit the ack thought
// AND the session external URL (the 10s SLA leg, BEFORE the post) -> post the
// prompt context into the topic with the dedup client_request_id.
func (d *Dispatcher) handleCreated(ctx context.Context, ev *SessionEvent) error {
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
		LinearSessionID:  ev.AgentSession.ID,
		ManagerAccountID: manager,
		ChannelID:        store.ChannelID(homeChannel),
		TopicID:          topicID,
		LinearIssueID:    ev.AgentSession.Issue.ID,
	}); err != nil {
		return err
	}
	// The 10s SLA leg: ack thought + session external URL, BEFORE the post.
	if err := d.client.CreateActivity(ctx, ev.AgentSession.ID, ActivityContent{Type: "thought", Body: ackThoughtBody}); err != nil {
		return err
	}
	if err := d.client.UpdateSession(ctx, ev.AgentSession.ID, []ExternalURL{{
		Label: externalURLLabel,
		URL:   d.deepLinkFor(homeChannel),
	}}); err != nil {
		return err
	}
	return d.post(ctx, homeChannel, topicID, ev.PromptContext)
}

// handlePrompted routes a follow-up to the recorded conversation: look up the
// association and post into its channel/topic. On a miss (a prompted event with
// no `created` on record) it synthesizes the association via the resolver from
// the payload's agentSession, then posts.
func (d *Dispatcher) handlePrompted(ctx context.Context, ev *SessionEvent) error {
	row, err := d.assoc.LinearAgentSession(ctx, ev.AgentSession.ID)
	switch {
	case err == nil:
		return d.post(ctx, string(row.ChannelID), row.TopicID, ev.AgentActivity.Body)
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
			LinearSessionID:  ev.AgentSession.ID,
			ManagerAccountID: manager,
			ChannelID:        store.ChannelID(homeChannel),
			TopicID:          topicID,
			LinearIssueID:    ev.AgentSession.Issue.ID,
		}); upErr != nil {
			return upErr
		}
		return d.post(ctx, homeChannel, topicID, ev.AgentActivity.Body)
	default:
		return err
	}
}

// post writes one message as the @linear bridge account into channel/topic with
// a fresh dedup client_request_id.
func (d *Dispatcher) post(ctx context.Context, channelID, topicID, body string) error {
	_, err := d.poster.PostAsAccount(ctx, d.bridge, &compassv1.PostMessageRequest{
		Container:       &compassv1.PostMessageRequest_ChannelId{ChannelId: channelID},
		Topic:           &compassv1.PostMessageRequest_TopicId{TopicId: topicID},
		Blocks:          []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: body}}},
		ClientRequestId: d.clientRequestID(),
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

// clientRequestID mints the comms-rail idempotency key: "linear-delivery:<uuid>".
func (d *Dispatcher) clientRequestID() string {
	return clientRequestIDPrefix + d.newRequestID()
}

// topicName is the issue identifier when present, else the session id.
func topicName(ev *SessionEvent) string {
	if ev.AgentSession.Issue.Identifier != "" {
		return ev.AgentSession.Issue.Identifier
	}
	return ev.AgentSession.ID
}
