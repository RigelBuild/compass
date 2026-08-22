//go:build pgtest && unix

package server

// RIG-1641 T4 — the end-to-end offline-mention redelivery pgtest: T1's real
// owed_mentions SQL + T2's routeMentions/fanOut arms + T3's resume-based
// AgentWaker composed over a REAL Postgres store and the REAL runnerhub.Hub, the
// way production assembles them (sinks.go:142-155 startDeliveryConsumer). Every
// existing delivery test (internal/delivery/*_test.go) drives the consumer over
// hand-written fakes (fakeReads / fakeWaker); NONE exercises the arms against
// the real store SQL and the real resume waker, so the fakes could silently
// diverge from the real InSweepSet disjunct, RecordOwedMention idempotency, the
// AckDelivery owed-clear txn, and OwedMentions ordering. This closes that gap.
//
// The wire the test stands up, inline, is production's:
//
//	c := delivery.NewConsumer(commsBus, st, hub, hub, log)
//	c.SetAgentWaker(newLifecycleService(st, hub))   // the REAL resume waker
//	hub.SetSettleSink(c); hub.SetSessionStartSink(c); hub.SetDeliveryStore(st)
//	go c.Run(ctx)
//
// It REUSES the full-stack placement harness (attachFakeRunner + recordingRunner
// from this package's service_placement_pgtest_test.go): a real store.Store + a
// real runnerhub.Hub + a recordingRunner attached over the mounted RunnerService
// door that records every pushed command as a WIRE fact. That harness is the
// only one that (a) lets a mention post through the real comms bus / settle edge,
// (b) lets a wake's fresh-start push a Start observed on the recording door, and
// (c) exposes newLifecycleService in-package — so it is the home the brief's
// package/location analysis lands on (no new package, no import cycle: server
// already imports delivery, sinks.go:27).
//
// Why the wake pushes a Start but does NOT itself make the agent live: the real
// waker's freshStart relays hub.Start (a Start on the wire — the observable "wake
// fired"), but promoteSession is a no-op there because the container was never
// bindContainer'd (no Provision precedes a wake), so no OnSessionStarted fires.
// That is exactly why the recoverable path needs the durable owed row AND a
// later start edge: the test drives OnSessionStarted (the exported consumer hook)
// to model the agent actually coming live, and the owed mention is swept then.
// DispatchControl routes to the enrolled Runner regardless of session id, so the
// swept steer/deliver is a recorded wire command on any session id.
//
// Human posts settle at post (IsAgentAccount==false → route/deliver now), so the
// test posts as the human admin through comms.PostAsAccount and the routing runs
// synchronously off that post — there is no author-settle edge to drive.
//
// context.Background() is the test root (the _test.go thread-context exemption,
// rule://go-thread-context): the one root ctx threads into Run and every store /
// hub / consumer call below, never re-rooted mid-tree. Every wait is event-gated
// on an observed wire fact (a pushed Start, a pushed steer/deliver) — never a
// sleep. Reads are scoped to THIS test's seeded agents / messages, so the
// shared-container isolated schema needs no empty-global-table assumption.

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"testing"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/delivery"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// mentionE2EWire is the production delivery wire assembled over a real store, a
// real hub with a recordingRunner door, a real comms service on the comms bus,
// and the delivery consumer driven by the REAL resume-based AgentWaker — the
// whole RIG-1641 spine end to end.
type mentionE2EWire struct {
	ctx      context.Context
	store    *store.Store
	hub      *runnerhub.Hub
	comms    *comms.Comms
	consumer *delivery.Consumer
	runner   *recordingRunner
	dsn      string
	adminID  store.AccountID // the human author of every posted message
	channel  store.ChannelID // the shared channel the test posts into
}

// newMentionE2EWire stands up the store + hub + fake Runner door (via the shared
// placement harness's attachFakeRunner), a comms service on a fresh comms bus,
// and the delivery consumer wired exactly as startDeliveryConsumer does — real
// waker included — with its Run loop on a goroutine bounded by the test ctx. It
// also creates one shared OPEN channel the admin authors into, admin a member so
// PostMessage's D9 membership gate admits the post.
func newMentionE2EWire(t *testing.T) *mentionE2EWire {
	t.Helper()
	ctx := context.Background() // the test root context (rule://go-thread-context _test.go exemption)
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)

	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	// The Runner's bearer, resolvable through the production resolver, so the
	// mounted door authenticates the fake Runner exactly as a real one.
	if err := st.PutTokenHash(ctx, sha256.Sum256([]byte(fakeRunnerToken)),
		store.Subject{Kind: store.SubjectRunner, ID: fakeRunnerID}); err != nil {
		t.Fatalf("PutTokenHash(runner): %v", err)
	}

	// The hub over a discard board + tail, no CommsCaller (the wake path never
	// relays a comms call) — the same shape newPlacementFixtureWith builds.
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	brd := board.NewProjection(bus)
	tail := newSessionTail()
	hub := newRunnerHub(st, brd, tail, nil, slog.New(slog.DiscardHandler))
	runner := attachFakeRunner(t, st, hub, false)

	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(commsBus.Close)
	commsSvc := comms.NewComms(st, commsBus, admin.ID)

	// The production delivery wire (sinks.go:142-155), assembled inline with the
	// REAL resume-based waker (newLifecycleService), not a fake.
	c := delivery.NewConsumer(commsBus, st, hub, hub, slog.New(slog.DiscardHandler))
	c.SetAgentWaker(newLifecycleService(st, hub))
	hub.SetSettleSink(c)
	hub.SetSessionStartSink(c)
	hub.SetDeliveryStore(st)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		_ = c.Run(runCtx) // Run returns nil on ctx cancel; the loop's fault path is covered by delivery unit tests
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// One shared OPEN channel the admin authors into; the admin must be a member
	// for PostMessage's D9 gate to admit the post.
	ch, err := st.CreateChannel(ctx, admin.ID, store.NewChannel{
		Name: "mention-e2e", Kind: store.ChannelKindChannel,
		MemberAccountIDs: []store.AccountID{admin.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	return &mentionE2EWire{
		ctx:      ctx,
		store:    st,
		hub:      hub,
		comms:    commsSvc,
		consumer: c,
		runner:   runner,
		dsn:      dsn,
		adminID:  admin.ID,
		channel:  ch.ID,
	}
}

// seedAgentMember creates an agent under the admin owner, adds it to the shared
// channel (subscribed or not), and records a placement so the wake's fresh-start
// path can act. A subscribed add is in the sweep set (cursor backstop, no owed
// row on a mention); an unsubscribed add is the gap population (owed row).
func (w *mentionE2EWire) seedAgentMember(t *testing.T, handle string, subscribed bool) store.Account {
	t.Helper()
	agent, err := w.store.CreateAgent(w.ctx, w.adminID, store.NewAgent{Handle: handle, DisplayName: handle})
	if err != nil {
		t.Fatalf("CreateAgent(%q): %v", handle, err)
	}
	if _, _, err := w.store.UpdateChannelMembers(w.ctx, w.adminID, w.channel, []store.MemberUpdate{
		{AccountID: agent.ID, Subscribed: subscribed},
	}); err != nil {
		t.Fatalf("UpdateChannelMembers(add %q, subscribed=%v): %v", handle, subscribed, err)
	}
	if err := w.store.RecordAgentPlacement(w.ctx, agent.ID, fakeRunnerID, containerFor(handle)); err != nil {
		t.Fatalf("RecordAgentPlacement(%q): %v", handle, err)
	}
	return agent
}

// containerFor derives a per-agent container name so distinct seeded agents get
// distinct placements (the fake Runner answers any container with fakeSessionID,
// so the name only needs to be unique, not resolved).
func containerFor(handle string) string { return "compass-agent-" + handle }

// post authors a message as the admin (a human — settled at post, so it
// routes/delivers synchronously) into the shared channel and returns its id.
func (w *mentionE2EWire) post(t *testing.T, body string) string {
	t.Helper()
	resp, err := w.comms.PostAsAccount(w.ctx, w.adminID, &compassv1.PostMessageRequest{
		Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(w.channel)},
		Topic:     &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
		Blocks:    []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: body}}},
	})
	if err != nil {
		t.Fatalf("PostAsAccount(%q): %v", body, err)
	}
	return resp.GetMessage().GetId()
}

// TestOfflineMentionRedeliveryEndToEnd is the RIG-1641 T4 e2e: both cycles over
// the real spine.
func TestOfflineMentionRedeliveryEndToEnd(t *testing.T) {
	t.Run("cycle A: offline unsubscribed mention records owed + wakes + steers on start", func(t *testing.T) {
		w := newMentionE2EWire(t)

		// A member, NOT subscribed (out of the sweep set), offline (no live
		// session), with a placement so the wake's fresh start can act.
		agent := w.seedAgentMember(t, "gapmember", false)

		// Post a settled mention at the offline out-of-sweep-set member.
		msgID := w.post(t, "@gapmember you were paged")

		// The wake fired: a Start reached the recording Runner door (a WIRE fact —
		// the real resume-waker's fresh-start push; the agent has a placement but
		// no prior session, so it takes the fresh-start arm).
		waitForStartCount(t, w.runner, 1)

		// A durable owed row exists for THIS agent (scoped read, not the global
		// count) — the no-loss backstop the gap population needs.
		if n := owedRowsFor(t, w.ctx, w.dsn, agent.ID); n != 1 {
			t.Fatalf("owed_mentions rows for %q = %d, want 1 (out-of-sweep-set offline mention records durably)", agent.ID, n)
		}
		// And the real OwedMentions read resolves that message on this channel.
		owed, err := w.store.OwedMentions(w.ctx, agent.ID)
		if err != nil {
			t.Fatalf("OwedMentions(%q): %v", agent.ID, err)
		}
		if got := owedMessageIDs(owed); len(got) != 1 || got[0] != msgID {
			t.Fatalf("OwedMentions message ids = %v, want [%s]", got, msgID)
		}
		// NO immediate dispatch: the wake's fresh start does not promote the
		// unbound container, so no start edge fired and nothing was steered yet.
		// The only dispatch path (the start-edge sweep) has not been driven.
		if got := allControlDelivers(w.runner); len(got) != 0 {
			t.Fatalf("control dispatches before OnSessionStarted = %d, want 0 (offline: nothing live to steer)", len(got))
		}

		// The agent comes live: drive the exported start edge (the hub's real
		// promotion fires this in production; the wake could not, container
		// unbound). sweepOwedMentions steers the owed mention.
		const sessA = "sess-gapmember-1"
		w.consumer.OnSessionStarted(sessA, agent.ID)

		// The owed mention arrives as exactly ONE STEER on that session.
		got := waitForControlDelivers(t, w.runner, sessA, 1)
		if len(got) != 1 {
			t.Fatalf("dispatches on start = %d, want exactly 1 (only the owed mention)", len(got))
		}
		if got[0].kind != controlSteer || got[0].messageID != msgID {
			t.Fatalf("dispatch = %+v, want {steer, %s}", got[0], msgID)
		}

		// Ack the delivery through the REAL T1 owed-clear txn.
		if err := w.store.AckDelivery(w.ctx, agent.ID, w.channel, msgID); err != nil {
			t.Fatalf("AckDelivery: %v", err)
		}
		if n := owedRowsFor(t, w.ctx, w.dsn, agent.ID); n != 0 {
			t.Fatalf("owed_mentions rows for %q after ack = %d, want 0 (the owed-clear txn cleared it)", agent.ID, n)
		}

		// A SECOND start sweeps NOTHING. Enqueue gapmember's re-start, then a
		// subscribed sentinel's start whose ONE cursor-owed message delivers on
		// the wire: drainStarts is single-goroutine FIFO, so the sentinel's
		// deliver arriving proves gapmember's re-start (enqueued first) already
		// drained — and swept nothing.
		sentinel := w.seedAgentMember(t, "sentinelsub", true)
		sentMsg := w.post(t, "sentinel barrier message")
		waitForStartCount(t, w.runner, 2) // the sentinel's own deliver-arm wake

		const sessA2 = "sess-gapmember-2"
		const sentSess = "sess-sentinel-1"
		w.consumer.OnSessionStarted(sessA2, agent.ID)
		w.consumer.OnSessionStarted(sentSess, sentinel.ID)

		barrier := waitForControlDelivers(t, w.runner, sentSess, 1)
		if barrier[0].messageID != sentMsg {
			t.Fatalf("sentinel deliver = %q, want the barrier message %q", barrier[0].messageID, sentMsg)
		}
		if got := controlDeliversForSession(w.runner, sessA2); len(got) != 0 {
			t.Fatalf("re-start dispatches to %q = %d, want 0 (the acked owed row is cleared)", sessA2, len(got))
		}
	})

	t.Run("cycle B: offline subscribed member wakes, no owed row, delivers on start", func(t *testing.T) {
		w := newMentionE2EWire(t)

		// A SUBSCRIBED member (in the sweep set), offline, with a placement.
		agent := w.seedAgentMember(t, "submember", true)

		// A plain (non-mention) message: the deliver arm wakes the offline
		// subscriber; the cursor sweep is its durable backstop, so no owed row.
		msgID := w.post(t, "standup in five, no mentions here")

		// The wake fired (a Start on the wire).
		waitForStartCount(t, w.runner, 1)

		// NO owed row: the in-sweep-set member's backstop is the cursor sweep.
		if n := owedRowsFor(t, w.ctx, w.dsn, agent.ID); n != 0 {
			t.Fatalf("owed_mentions rows for subscribed %q = %d, want 0 (cursor sweep is the backstop)", agent.ID, n)
		}

		// The agent comes live: the start-edge cursor sweep (sweepSession over
		// UndeliveredMessages) delivers the message as a plain DELIVER.
		const sessB = "sess-submember-1"
		w.consumer.OnSessionStarted(sessB, agent.ID)

		got := waitForControlDelivers(t, w.runner, sessB, 1)
		if len(got) != 1 {
			t.Fatalf("dispatches on start = %d, want exactly 1 (the cursor-swept message)", len(got))
		}
		if got[0].kind != controlDeliver || got[0].messageID != msgID {
			t.Fatalf("dispatch = %+v, want {deliver, %s} (a plain deliver via the cursor sweep, not a steer)", got[0], msgID)
		}
	})
}

// controlKind distinguishes a steer from a deliver in a recorded DeliverControl
// push, so the e2e can assert a mention arrives as a STEER and a plain message
// as a DELIVER — the only recipient-side difference the two ops carry.
type controlKind int

const (
	controlDeliver controlKind = iota
	controlSteer
	controlOther
)

// controlRecord is one observed send-only control push on the wire: the session
// it targeted, the carried message id, and the op kind.
type controlRecord struct {
	sessionID string
	messageID string
	kind      controlKind
}

// classifyControl reports the kind and carried message id of a dispatched op
// (both steer and deliver carry a Message).
func classifyControl(op *compassv1internal.AgentControl) (controlKind, string) {
	switch {
	case op.GetSteer() != nil:
		return controlSteer, op.GetSteer().GetMessage().GetId()
	case op.GetDeliver() != nil:
		return controlDeliver, op.GetDeliver().GetMessage().GetId()
	default:
		return controlOther, ""
	}
}

// allControlDelivers snapshots every send-only control (DeliverControl) command
// the Server pushed to the recording Runner, across all sessions — the wire
// facts the steer/deliver assertions read.
func allControlDelivers(r *recordingRunner) []controlRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []controlRecord
	for _, c := range r.seen {
		if dc := c.GetDeliverControl(); dc != nil {
			kind, id := classifyControl(dc.GetOp())
			out = append(out, controlRecord{sessionID: dc.GetSessionId(), messageID: id, kind: kind})
		}
	}
	return out
}

// controlDeliversForSession filters allControlDelivers to one session id.
func controlDeliversForSession(r *recordingRunner, sessionID string) []controlRecord {
	var out []controlRecord
	for _, rec := range allControlDelivers(r) {
		if rec.sessionID == sessionID {
			out = append(out, rec)
		}
	}
	return out
}

// waitForControlDelivers event-gates until at least n control pushes to
// sessionID are recorded, then returns them — never a sleep. Bounded by
// testTimeout so a wedged sweep fails fast rather than hanging.
func waitForControlDelivers(t *testing.T, r *recordingRunner, sessionID string, n int) []controlRecord {
	t.Helper()
	deadline := timeAfter()
	for {
		if got := controlDeliversForSession(r, sessionID); len(got) >= n {
			return got
		}
		select {
		case <-deadline:
			t.Fatalf("session %q saw %d control dispatches, want >= %d (commands: %v)",
				sessionID, len(controlDeliversForSession(r, sessionID)), n, r.commands())
			return nil
		default:
		}
	}
}

// waitForStartCount event-gates until the Runner has recorded at least n Start
// commands — the wake's wire fact — never a sleep.
func waitForStartCount(t *testing.T, r *recordingRunner, n int) {
	t.Helper()
	deadline := timeAfter()
	for {
		if r.startCount() >= n {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("Runner saw %d Starts, want >= %d (the wake never pushed one; commands: %v)", r.startCount(), n, r.commands())
			return
		default:
		}
	}
}

// owedRowsFor counts owed_mentions rows for one agent in this test's isolated
// schema — a scoped read (never the global CountOwedMentions), so a shared
// container needs no empty-global-table assumption.
func owedRowsFor(t *testing.T, ctx context.Context, dsn string, agent store.AccountID) int {
	t.Helper()
	conn := connectPG(t, ctx, dsn)
	var n int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM owed_mentions WHERE agent_account_id = $1`, string(agent),
	).Scan(&n); err != nil {
		t.Fatalf("count owed_mentions for %q: %v", agent, err)
	}
	return n
}

// owedMessageIDs flattens the OwedMentions channel-keyed map into the message ids
// it carries, so a scoped assertion names the exact owed message.
func owedMessageIDs(owed map[store.ChannelID][]store.Message) []string {
	var out []string
	for _, msgs := range owed {
		for _, m := range msgs {
			out = append(out, string(m.ID))
		}
	}
	return out
}
