//go:build pgtest && unix

package server

// RIG-2967 T6 — the peer-DM tasking loop end to end over the REAL spine: T3's
// OpenDMAsAccount resolve-or-create + T0's name-addressed post/list + T0's
// source-channel/topic denorm on the wrapped deliver/steer + T2's born-mandatory
// DM store (both parties members, cursors seeded) + T3's spawn auto-open — all
// composed over a real Postgres store, the real runnerhub.Hub with a
// recordingRunner door, a real comms service on the comms bus, and the delivery
// consumer driven by the REAL resume-based waker, the way production assembles
// them (sinks.go:142-155). No mocks, no production change (design.md T6:831 —
// "Interfaces: none new — consumes T0-T3").
//
// It proves the dogfood-symptom regression the record names (design.md
// T6:820-829): agent A opens a DM on B, posts (create_topic) naming the DM
// channel + topic, B receives it as a deliver whose recorded ChannelName/TopicName
// ARE the DM's (the T0 denorm the reply-routing fix hangs on), B replies NAMING
// that same DM channel + topic, and the reply lands in the SAME DM topic — NOT
// B's home channel. The home-misroute guard is a two-sided assertion: the reply
// MUST be readable in the DM AND absent from B's home; a reply that routed home
// (the old bug) reddens both halves.
//
// The wire the tests stand up, inline, is production's:
//
//	c := delivery.NewConsumer(commsBus, st, hub, hub, log)
//	c.SetAgentWaker(newLifecycleService(st, hub, commsSvc)) // REAL waker, real dm opener
//	hub.SetSettleSink(c); hub.SetSessionStartSink(c); hub.SetDeliveryStore(st)
//	go c.Run(ctx)
//
// It REUSES the full-stack placement harness (attachFakeRunner + recordingRunner
// from service_placement_pgtest_test.go) and the offline-mention e2e's control
// accessors (allControlDelivers / waitForControlDelivers / controlRecord /
// classifyControl / containerFor). Only what those don't carry is added here: a
// deliver snapshot that also reads the source ChannelName/TopicName off the op
// (the T0 denorm), which the plain controlRecord does not surface.
//
// Why a wake fresh-starts but does NOT itself make B live (same as the mention
// e2e): the waker's freshStart relays hub.Start (a Start on the wire), but
// promoteSession is a no-op because the container was never bindContainer'd (no
// Provision precedes a direct-placement wake), so no OnSessionStarted fires. The
// test therefore drives OnSessionStarted (the exported consumer hook) to model B
// actually coming live, and the DM message is cursor-swept then. The spawn leg is
// different: SpawnAsAccount runs the real hub Provision->Start, which DOES
// bindContainer then promoteSession, so the spawned peer is genuinely hub-live at
// the fake session id and a manager post reaches it via the live fan-out.
//
// context.Background() is the test root (the _test.go thread-context exemption,
// rule://go-thread-context): the one root ctx threads into Run and every store /
// hub / comms / consumer call, never re-rooted mid-tree. Every async wait is
// event-gated on an observed wire fact (a pushed deliver) — never a sleep. Reads
// are scoped to THIS test's seeded agents / channels, so the shared-container
// isolated schema needs no empty-global-table assumption.

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"testing"

	"connectrpc.com/connect"

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

// dmE2EWire is the peer-DM delivery wire assembled over a real store, a real hub
// with a recordingRunner door, a real comms service on the comms bus, and the
// delivery consumer driven by the REAL resume-based waker whose dm opener is the
// same comms service (so a spawn's auto-open runs through the real handler).
type dmE2EWire struct {
	ctx      context.Context
	store    *store.Store
	hub      *runnerhub.Hub
	comms    *comms.Comms
	consumer *delivery.Consumer
	lc       *lifecycleService
	runner   *recordingRunner
	adminID  store.AccountID // the human owner of every seeded/spawned agent
}

// newDME2EWire stands up the store + hub + fake Runner door (via attachFakeRunner),
// a comms service on a fresh comms bus, and the delivery consumer wired exactly as
// startDeliveryConsumer does — REAL waker with the comms service as its dm opener,
// so both the delivery sweep and the spawn auto-open exercise real code. The Run
// loop is on a goroutine bounded by the test ctx.
func newDME2EWire(t *testing.T) *dmE2EWire {
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
	// REAL resume-based waker whose dm opener is the comms service — so the spawn
	// auto-open (T3 R8) runs the real OpenDMAsAccount path, and the delivery sweep
	// runs the real waker.
	lc := newLifecycleService(st, hub, commsSvc)
	c := delivery.NewConsumer(commsBus, st, hub, hub, slog.New(slog.DiscardHandler))
	c.SetAgentWaker(lc)
	hub.SetSettleSink(c)
	hub.SetSessionStartSink(c)
	hub.SetDeliveryStore(st)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		// Run returns nil on ctx cancel; the loop's fault path is covered by
		// delivery unit tests. The error is intentionally not propagated here —
		// there is no caller to return it to on a cancelled background loop.
		if err := c.Run(runCtx); err != nil {
			t.Logf("delivery consumer Run returned: %v", err)
		}
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	return &dmE2EWire{
		ctx:      ctx,
		store:    st,
		hub:      hub,
		comms:    commsSvc,
		consumer: c,
		lc:       lc,
		runner:   runner,
		adminID:  admin.ID,
	}
}

// seedAgent creates an agent under the admin owner and records a placement so a
// wake's fresh-start path can act. DM membership and delivery cursors come from
// OpenDM (born-mandatory, both parties members, cursors seeded), not from here.
func (w *dmE2EWire) seedAgent(t *testing.T, handle string) store.Account {
	t.Helper()
	agent, err := w.store.CreateAgent(w.ctx, w.adminID, store.NewAgent{Handle: handle, DisplayName: handle})
	if err != nil {
		t.Fatalf("CreateAgent(%q): %v", handle, err)
	}
	if err := w.store.RecordAgentPlacement(w.ctx, agent.ID, fakeRunnerID, containerFor(handle)); err != nil {
		t.Fatalf("RecordAgentPlacement(%q): %v", handle, err)
	}
	return agent
}

// dmDeliver is one observed deliver/steer push that ALSO carries the denormalized
// source channel + topic names (RIG-2956 T0) — the fields the reply-routing fix
// hangs on and which the plain controlRecord does not surface.
type dmDeliver struct {
	sessionID   string
	messageID   string
	channelName string
	topicName   string
	kind        controlKind
}

// dmDeliversForSession snapshots every deliver/steer control pushed to sessionID
// with its carried source channel + topic names. Reads r.seen under r.mu, the
// same wire-fact source allControlDelivers uses.
func dmDeliversForSession(r *recordingRunner, sessionID string) []dmDeliver {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []dmDeliver
	for _, c := range r.seen {
		dc := c.GetDeliverControl()
		if dc == nil || dc.GetSessionId() != sessionID {
			continue
		}
		op := dc.GetOp()
		switch {
		case op.GetDeliver() != nil:
			d := op.GetDeliver()
			out = append(out, dmDeliver{sessionID, d.GetMessage().GetId(), d.GetChannelName(), d.GetTopicName(), controlDeliver})
		case op.GetSteer() != nil:
			s := op.GetSteer()
			out = append(out, dmDeliver{sessionID, s.GetMessage().GetId(), s.GetChannelName(), s.GetTopicName(), controlSteer})
		}
	}
	return out
}

// textBlock wraps one plain-text block, the only block shape these posts need.
func textBlock(body string) []*compassv1.MessageBlock {
	return []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: body}}}
}

// containsString reports whether xs contains want — the []string member-id
// membership check the OpenDMResponse channel invariants read (the store-typed
// variant containsAccountID lives in lifecycle_pgtest_test.go).
func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// channelHasText reports whether channelID (read as account, a member) contains a
// message whose first text block equals want — the read-back the reply-routing
// assertions compare against. A non-member read surfaces its edge error, failed
// loud so a misread never masquerades as an absence.
func channelHasText(t *testing.T, svc *comms.Comms, account store.AccountID, channelID, want string) bool {
	t.Helper()
	resp, err := svc.ListAsAccount(context.Background(), account, &compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: channelID},
	})
	if err != nil {
		t.Fatalf("ListAsAccount(%s, channel %s): %v", account, channelID, err)
	}
	for _, m := range resp.GetMessages() {
		for _, b := range m.GetBlocks() {
			if b.GetText() == want {
				return true
			}
		}
	}
	return false
}

// TestPeerDMTaskingLoopEndToEnd drives the full peer-DM tasking loop over the real
// spine: open -> post -> deliver (with source names) -> reply-lands-in-the-DM
// (the dogfood-symptom regression) -> re-open idempotent -> outsider isolation.
func TestPeerDMTaskingLoopEndToEnd(t *testing.T) {
	w := newDME2EWire(t)
	ctx := w.ctx

	// Agent A and agent B under the same owner (admin). Placements recorded so a
	// wake can act; DM membership comes from OpenDM below.
	agentA := w.seedAgent(t, "dm-a")
	agentB := w.seedAgent(t, "dm-b")

	// 1. OPEN: A opens a DM on B by handle. The returned channel is a real DM —
	//    kind=DM, mandatory subscription, both A and B members — and Created=true.
	open, err := w.comms.OpenDMAsAccount(ctx, agentA.ID, &compassv1.OpenDMRequest{PeerHandle: "dm-b"})
	if err != nil {
		t.Fatalf("OpenDMAsAccount(A->B): %v", err)
	}
	if !open.GetCreated() {
		t.Fatalf("open created = false, want true on a first open")
	}
	dm := open.GetChannel()
	dmName := dm.GetName()
	dmID := dm.GetId()
	if dm.GetKind() != compassv1.ChannelKind_CHANNEL_KIND_DM {
		t.Fatalf("DM kind = %v, want CHANNEL_KIND_DM", dm.GetKind())
	}
	if !dm.GetMandatorySubscription() {
		t.Fatalf("DM mandatory_subscription = false, want true (born-mandatory)")
	}
	if !containsString(dm.GetMemberAccountIds(), string(agentA.ID)) || !containsString(dm.GetMemberAccountIds(), string(agentB.ID)) {
		t.Fatalf("DM members = %v, want both A %s and B %s", dm.GetMemberAccountIds(), agentA.ID, agentB.ID)
	}

	// 2. POST + DELIVER: A posts into the DM NAMING the DM channel + a topic with
	//    create_topic (T0 name-addressed post). Then B comes live and the message
	//    is cursor-swept to B as a DELIVER whose recorded ChannelName/TopicName ARE
	//    the DM's — the T0 source-name denorm the reply-routing fix hangs on.
	const dmTopic = "tasking"
	const taskText = "A: please pick up the tasking in this DM"
	posted, err := w.comms.PostAsAccountByName(ctx, agentA.ID, &compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: dmName},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: dmTopic},
		CreateTopic: true,
		Blocks:      textBlock(taskText),
	})
	if err != nil {
		t.Fatalf("PostAsAccountByName(A into DM): %v", err)
	}
	taskMsgID := posted.GetMessage().GetId()

	// B comes live: the exported start edge fires the cursor sweep (the hub's real
	// promotion fires this in production; a direct-placement wake cannot, container
	// unbound). context.Background()-rooted post above is already committed, so the
	// sweep sees the undelivered DM message.
	const sessB = "sess-dm-b-1"
	w.consumer.OnSessionStarted(sessB, agentB.ID)

	// Event-gate on the observed deliver to B — never a sleep. The exact-count
	// check below is airtight because B's only owed message is this single DM post
	// (B's home channel is empty at start), so no second deliver can race in.
	got := waitForControlDelivers(t, w.runner, sessB, 1)
	if len(got) != 1 {
		t.Fatalf("dispatches to B on start = %d, want exactly 1 (the cursor-swept DM message)", len(got))
	}
	if got[0].kind != controlDeliver || got[0].messageID != taskMsgID {
		t.Fatalf("dispatch = %+v, want {deliver, %s}", got[0], taskMsgID)
	}
	// The T0 denorm: the wrapped deliver carries the DM's channel + topic NAMES.
	dd := dmDeliversForSession(w.runner, sessB)
	if len(dd) != 1 {
		t.Fatalf("deliver snapshots to B = %d, want 1", len(dd))
	}
	if dd[0].channelName != dmName || dd[0].topicName != dmTopic {
		t.Fatalf("delivered source names = {channel %q, topic %q}, want {%q, %q} (T0 source-name denorm)",
			dd[0].channelName, dd[0].topicName, dmName, dmTopic)
	}

	// 3. REPLY LANDS IN THE DM (the regression): B replies by posting NAMING that
	//    same DM channel + topic. The reply MUST land in the SAME DM topic and NOT
	//    in B's home channel. This is the dogfood-symptom guard: the old bug routed
	//    an agent reply to its home channel; here the reply is asserted present in
	//    the DM AND absent from B's home, so a home-misroute reddens both halves.
	const replyText = "B: on it — replying inside the DM topic, not my home channel"
	if _, err := w.comms.PostAsAccountByName(ctx, agentB.ID, &compassv1.PostMessageRequest{
		Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: dmName},
		Topic:     &compassv1.PostMessageRequest_TopicName{TopicName: dmTopic},
		Blocks:    textBlock(replyText),
	}); err != nil {
		t.Fatalf("PostAsAccountByName(B reply into DM): %v", err)
	}
	if !channelHasText(t, w.comms, agentB.ID, dmID, replyText) {
		t.Fatalf("B's reply is NOT in the DM channel %s — it did not land in the DM topic", dmID)
	}
	if channelHasText(t, w.comms, agentB.ID, string(agentB.Agent.HomeChannelID), replyText) {
		t.Fatalf("B's reply landed in B's HOME channel %s — the dogfood-symptom misroute regression", agentB.Agent.HomeChannelID)
	}

	// 4. RE-OPEN IDEMPOTENT: B opens the DM on A -> the SAME channel id, Created=false.
	reopen, err := w.comms.OpenDMAsAccount(ctx, agentB.ID, &compassv1.OpenDMRequest{PeerHandle: "dm-a"})
	if err != nil {
		t.Fatalf("OpenDMAsAccount(B->A reopen): %v", err)
	}
	if reopen.GetCreated() {
		t.Fatalf("reopen created = true, want false (resume)")
	}
	if reopen.GetChannel().GetId() != dmID {
		t.Fatalf("reopen channel id = %q, want the first open's %q", reopen.GetChannel().GetId(), dmID)
	}

	// 5. OUTSIDER ISOLATION: a third agent C (same owner, NOT a DM member) listing
	//    the DM by name collapses to not_found — the D9 visibility clip, so C never
	//    learns the DM exists.
	agentC := w.seedAgent(t, "dm-c")
	_, err = w.comms.ListAsAccountByName(ctx, agentC.ID, &compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: dmName},
	})
	if err == nil {
		t.Fatalf("outsider C ListAsAccountByName(DM) = success, want CodeNotFound (C is not a member)")
	}
	if code := connect.CodeOf(err); code != connect.CodeNotFound {
		t.Fatalf("outsider C list code = %v, want CodeNotFound", code)
	}
}

// TestPeerDMSpawnPathDelivers drives the spawn-path leg (design.md T6:826): a
// manager spawns a peer, the spawn auto-opens the manager<->peer DM and returns a
// live dm_channel_name whose channel satisfies the DM invariants, and a manager
// post into that DM reaches the spawned peer as a delivery target — event-gated
// on the recording runner. The spawn runs the REAL hub Provision->Start, so the
// peer is genuinely hub-live at the fake session id and the manager post reaches
// it via the live fan-out (not a manual OnSessionStarted).
func TestPeerDMSpawnPathDelivers(t *testing.T) {
	w := newDME2EWire(t)
	ctx := w.ctx

	// The manager is an agent under admin (SpawnAsAccount resolves the caller's
	// OWNER, so the caller must be an agent). No placement needed for the caller.
	manager := w.seedAgent(t, "mgr")

	resp, err := w.lc.SpawnAsAccount(ctx, manager.ID, &compassv1internal.SpawnPeerRequest{
		Handle:          "mgr-peer",
		DisplayName:     "Manager Peer",
		ClientRequestId: "spawn-t6-1",
		Role:            "manager",
	})
	if err != nil {
		t.Fatalf("SpawnAsAccount(manager) = %v, want success", err)
	}
	dmName := resp.GetDmChannelName()
	if dmName == "" {
		t.Fatal("spawn dm_channel_name = empty, want a live manager<->peer DM name (T3 R8 auto-open)")
	}
	peerID := store.AccountID(resp.GetAgentAccountId())
	if peerID == "" {
		t.Fatal("spawn agent_account_id = empty, want the new peer's id")
	}

	// The returned name resolves to a real DM manager<->peer with the full invariants.
	dm, err := w.store.ChannelByNameForViewer(ctx, manager.ID, dmName)
	if err != nil {
		t.Fatalf("ChannelByNameForViewer(manager, %q) = %v, want the auto-opened DM", dmName, err)
	}
	if dm.Kind != store.ChannelKindDM {
		t.Fatalf("spawn DM kind = %d, want ChannelKindDM", dm.Kind)
	}
	if !dm.Policy.MandatorySubscription {
		t.Fatalf("spawn DM mandatory_subscription = false, want true (born-mandatory)")
	}
	if !containsAccountID(dm.MemberAccountIDs, manager.ID) || !containsAccountID(dm.MemberAccountIDs, peerID) {
		t.Fatalf("spawn DM members = %v, want both manager %s and peer %s", dm.MemberAccountIDs, manager.ID, peerID)
	}

	// DELIVERY LEG: the manager posts into the auto-opened DM naming its channel +
	// topic. The spawned peer is hub-live at the fake session id (the real
	// Provision->Start binding), so the message reaches it via the live fan-out —
	// event-gated on the recording runner, never a sleep. The manager is
	// intentionally NOT hub-live (seedAgent records a placement but never
	// Provision->Starts it), so the post takes the offline-author path and fires
	// the deliver immediately rather than holding it to the author's settle edge —
	// a future change that makes the caller live would need to drive that edge.
	const spawnTopic = "kickoff"
	const spawnText = "manager: your first tasking, in our DM"
	posted, err := w.comms.PostAsAccountByName(ctx, manager.ID, &compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: dmName},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: spawnTopic},
		CreateTopic: true,
		Blocks:      textBlock(spawnText),
	})
	if err != nil {
		t.Fatalf("PostAsAccountByName(manager into spawn DM): %v", err)
	}
	msgID := posted.GetMessage().GetId()

	// got event-gates on the plain control record; dd re-snapshots the same
	// recorded commands with the denorm-carrying source names for the assertion.
	got := waitForControlDelivers(t, w.runner, fakeSessionID, 1)
	if len(got) < 1 {
		t.Fatalf("dispatches to the spawned peer = %d, want >= 1 (the DM tasking)", len(got))
	}
	dd := dmDeliversForSession(w.runner, fakeSessionID)
	var found bool
	for _, d := range dd {
		if d.messageID == msgID {
			found = true
			if d.channelName != dmName || d.topicName != spawnTopic {
				t.Fatalf("delivered source names = {channel %q, topic %q}, want {%q, %q} (T0 source-name denorm)",
					d.channelName, d.topicName, dmName, spawnTopic)
			}
		}
	}
	if !found {
		t.Fatalf("the manager's DM tasking %s never reached the spawned peer's session (delivers: %+v)", msgID, dd)
	}
}
