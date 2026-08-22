//go:build pgtest && unix

package server

// End-to-end T7 of the FROZEN spawn/despawn design (SEA-1617 / record SEA-1360):
// the WHOLE agent-initiated spawn/despawn wire, driven over a REAL per-container
// AgentGateway unix socket against a real Postgres + a real Runner-over-stub-
// engine. Where lifecycle_pgtest_test.go (T5) drives the lifecycleService seam
// DIRECTLY (newLifecycleService, no wire), this drives every hop the record
// names:
//
//	in-container agent  ->  AgentGateway.Lifecycle (per-container unix socket)
//	  ->  Runner gateway.Lifecycle (maps socket->container->bound session)
//	  ->  RelayLifecycleCall(session_id, call)  (Runner asserts NO account)
//	  ->  Hub.RelayLifecycleCall (resolves session_id->caller account, fail-closed)
//	  ->  lifecycleService.Spawn/DespawnAsAccount under the resolved caller
//	  ->  store (accounts + placements) + hub Provision/Start/Stop/Remove
//
// PACKAGE PLACEMENT (the decisive design question, resolved to option B). The
// hub needs a real LifecycleCaller, which is *lifecycleService — unexported, in
// package server. Only package server can construct it (newLifecycleService) and
// wire it (hub.SetLifecycleCaller, serve.go:250), so the whole-wire lifecycle
// test cannot live in runnerhub_test without exporting a new production
// constructor (out of scope — T7 adds no production interfaces). So this test
// lives in package server and ASSEMBLES the full wire inline, combining the
// server-package hub wiring (newRunnerHub + SetLifecycleCaller) with the
// real-runner-over-stub-engine socket shape. The socket scaffolding it needs —
// the sun_path-bounded runtime dir, the h2c client, the cleartext-H2 dialer —
// is the same shape runnerhub/integration_pgtest_test.go needs, so it lives in
// the shared internal/runnertest package (ShortRuntimeDir carries the sun_path
// budget; runnerloop.RunSessionsLoop carries the LIFO drain ordering) and both
// tests import it rather than each carrying a copy.
//
// Each assertion carries a mutation comment: the plausible regression in the
// (already merged, green) spine that would redden it — the "red-first" the
// record's T7 acceptance means here.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	stdruntime "runtime"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/comms"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/runner"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/runnertest"
	"github.com/RigelBuild/compass/go/internal/runnertest/runnerloop"
	"github.com/RigelBuild/compass/go/internal/runtime"
	"github.com/RigelBuild/compass/go/internal/store"
)

// e2eTimeout bounds every wire wait so a wedged seam fails fast rather than
// hanging the suite. It is a deadline safety net, never a synchronization
// device: the wire is event-gated (provisionWhenSeamLive's retry, the socket
// round-trip's own blocking return), never slept on.
const e2eTimeout = 30 * time.Second

// e2eNamePrefix is the container-name prefix this test wires into its
// SpecDefaults, hoisted so runnertest.ShortRuntimeDir models the same name the Runner
// actually builds (BuildSpec in spec.go joins it with the account id). Editing
// the prefix in one place would otherwise silently shrink the modelled path and
// turn the budget assertion into a false negative.
const e2eNamePrefix = "compass-agent-"

// e2eAccountIDHexLen is the width of a store account id: 16 random bytes
// hex-encoded (internal/store/ids.go). Fixed at that minting site — the only one
// — rather than validated where the socket path is built, so it is the right
// width to model the tail with and the wrong thing to call a guarantee. Tied to
// the real minted value in newE2EWire so widening store ids reddens there rather
// than silently overrunning the socket path.
const e2eAccountIDHexLen = 32

// peerPostText is the body a spawned peer posts over ITS OWN socket, asserted
// back out of the store under the peer account — the proof that spawn wired a
// genuinely independent bound session, not a shadow of the supervisor's.
const peerPostText = "peer speaking over its own AgentGateway socket"

// TestSpawnDespawnOverTheWire drives the spawn happy-path, the spawned peer
// acting under its own account, and the despawn happy-path (+ fail-closed after)
// as one ordered wire: despawn genuinely needs a spawned peer, so the state
// flows through the subtests over a single shared supervisor session — the shape
// the runnerhub integration reference uses for its whole-wire test. Each subtest
// pins ONE property with its own mutation comment.
func TestSpawnDespawnOverTheWire(t *testing.T) {
	w := newE2EWire(t)

	// The peer the whole test spawns, despawns, and asserts fail-closed on.
	// Captured across subtests because the state is sequential.
	var (
		peerID        store.AccountID
		peerContainer string
		peerHome      store.ChannelID
	)

	t.Run("spawn happy-path inherits the caller's owner (F2)", func(t *testing.T) {
		peerID, peerContainer, peerHome = e2eSpawnHappyPath(t, w)
	})

	t.Run("peer's own comms call resolves to the peer account", func(t *testing.T) {
		if peerID == "" {
			t.Fatal("spawn subtest did not run; peer is unset")
		}
		e2ePeerPostsUnderOwnAccount(t, w, peerID, peerContainer, peerHome)
	})

	t.Run("despawn removes the container and deletes the placement", func(t *testing.T) {
		if peerID == "" {
			t.Fatal("spawn subtest did not run; peer is unset")
		}
		e2eDespawnPeer(t, w, peerID, peerContainer)
	})

	t.Run("peer fails closed after despawn", func(t *testing.T) {
		if peerID == "" {
			t.Fatal("spawn subtest did not run; peer is unset")
		}
		e2ePeerFailsClosedAfterDespawn(t, w, peerID, peerContainer, peerHome)
	})
}

// e2eSpawnHappyPath drives the spawn happy-path over the wire and returns the
// spawned peer's account id, container name, and home channel — the state the
// later phases act on. It pins the F2 ownership frame (peer owned by the
// supervisor's OWNER, never the caller agent, never admin) and that the spawn
// chain provisioned + placed + started a session under the peer account.
func e2eSpawnHappyPath(t *testing.T, w *e2eWire) (peerID store.AccountID, peerContainer string, peerHome store.ChannelID) {
	t.Helper()
	ctx := w.ctx
	resp, err := w.supervisorClient.Lifecycle(ctx, connect.NewRequest(&compassv1internal.LifecycleCallRequest{
		CallId: "spawn-call-1",
		Call: &compassv1internal.LifecycleCallRequest_Spawn{Spawn: &compassv1internal.SpawnPeerRequest{
			Handle:          "peer-1",
			DisplayName:     "Peer One",
			ClientRequestId: "spawn-req-1",
		}},
	}))
	if err != nil {
		t.Fatalf("Lifecycle(spawn) over the socket = %v, want the round-trip result", err)
	}
	// A spawn is a SUCCESS variant (never the in-band _Error): an _Error here
	// means the happy-path spawn tripped a tool-level guard it should not.
	if e := resp.Msg.GetError(); e != nil {
		t.Fatalf("spawn returned in-band error {code=%q msg=%q}, want a spawn result", e.GetCode(), e.GetMessage())
	}
	spawn := resp.Msg.GetSpawn()
	if spawn == nil {
		t.Fatal("spawn result carried no SpawnPeerResponse")
	}
	// call_id rides back verbatim: the hub stamps the inbound call_id onto the
	// result (RelayLifecycleCall). Dropping that stamp reddens this.
	if got := resp.Msg.GetCallId(); got != "spawn-call-1" {
		t.Fatalf("result call id = %q, want the verbatim %q", got, "spawn-call-1")
	}

	peerID = store.AccountID(spawn.GetAgentAccountId())
	peerContainer = spawn.GetContainerName()
	if peerID == "" || peerID == w.supervisor.ID {
		t.Fatalf("spawned peer id = %q, want a fresh id distinct from the supervisor %q", peerID, w.supervisor.ID)
	}

	// F2 ownership — the load-bearing security frame. The peer is owned by the
	// SUPERVISOR'S OWNER (a non-admin user), never the supervisor agent, never
	// the bootstrap admin. Mutation: creating the peer under the caller agent
	// id, or a hard-coded admin id, instead of the store-resolved caller owner
	// reddens this — the admin-literal arm is real because the fixture owns the
	// supervisor under a non-admin user distinct from admin.
	owner, err := w.store.AgentOwner(ctx, peerID)
	if err != nil {
		t.Fatalf("AgentOwner(peer) = %v", err)
	}
	if owner != w.supervisorOwner {
		t.Fatalf("peer owner = %q, want the supervisor's owner %q (never the caller agent, never admin-literal)", owner, w.supervisorOwner)
	}
	if owner == w.supervisor.ID {
		t.Fatalf("peer is owned by the CALLER AGENT %q — the F2 ownership frame is broken", w.supervisor.ID)
	}
	if owner == w.adminID {
		t.Fatalf("peer is owned by the bootstrap ADMIN %q — the caller->owner resolution collapsed to the admin literal instead of the session-bound caller's owner", w.adminID)
	}

	// The chain completed: provisioned + placed. Mutation: skipping
	// RecordAgentPlacement in provisionAndStart leaves the peer unplaced, so
	// PlacementForAgent misses.
	if _, container, err := w.store.PlacementForAgent(ctx, peerID); err != nil || container != peerContainer {
		t.Fatalf("PlacementForAgent(peer) = (%q, %v), want (%q, nil)", container, err, peerContainer)
	}
	// A real session was started and recorded under the PEER account.
	// Mutation: recording the session under the wrong account (or not at all)
	// reddens the session-owner read.
	if got := sessionOwner(t, ctx, w.dsn, spawn.GetSessionId()); got != string(peerID) {
		t.Fatalf("session owner = %q, want the spawned peer %q", got, peerID)
	}

	acc, err := w.store.GetAccount(ctx, peerID)
	if err != nil {
		t.Fatalf("GetAccount(peer) = %v", err)
	}
	peerHome = acc.Agent.HomeChannelID
	return peerID, peerContainer, peerHome
}

// e2ePeerPostsUnderOwnAccount dials the peer's own per-container socket and
// posts as the in-container peer would, asserting the post is attributed to the
// PEER account (never the supervisor, never admin) and committed to the real
// store — the proof spawn wired a genuinely independent bound session.
func e2ePeerPostsUnderOwnAccount(t *testing.T, w *e2eWire, peerID store.AccountID, peerContainer string, peerHome store.ChannelID) {
	t.Helper()
	ctx := w.ctx
	// Dial the PEER's per-container socket and post as the in-container peer
	// agent would. The Server resolves the peer's session->account binding and
	// attributes the post to the PEER — never the supervisor, never admin.
	// Mutation: if spawn promoted the session binding onto the wrong account
	// (or the supervisor's), this author check reddens — the proof that spawn
	// wired a genuinely independent bound session.
	client := w.dialPeer(t, peerContainer)
	resp, err := client.Comms(ctx, connect.NewRequest(&compassv1internal.CommsCallRequest{
		CallId: "peer-post-1",
		Call:   &compassv1internal.CommsCallRequest_Post{Post: &compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(peerHome)}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: peerPostText}}}}},
	}))
	if err != nil {
		t.Fatalf("Comms(post) over the peer socket = %v, want the round-trip result", err)
	}
	posted := resp.Msg.GetPost().GetMessage()
	if posted == nil {
		t.Fatal("peer comms post result carried no message")
	}
	if got := posted.GetAuthorAccountId(); got != string(peerID) {
		t.Fatalf("peer post author = %q, want the bound PEER account %q (not the supervisor, not admin)", got, peerID)
	}

	// Committed to the REAL store under the peer account.
	msgs, err := w.store.ListMessages(ctx, store.ListMessagesQuery{Actor: peerID, ChannelID: peerHome, Page: store.Page{Limit: 10}})
	if err != nil {
		t.Fatalf("ListMessages(peer home) = %v", err)
	}
	if len(msgs) != 1 || textOfE2E(msgs[0]) != peerPostText {
		t.Fatalf("peer home has %d messages (want 1 with the posted body); got %+v", len(msgs), msgs)
	}
	if msgs[0].AuthorAccountID != peerID {
		t.Fatalf("stored peer message author = %q, want the peer account %q", msgs[0].AuthorAccountID, peerID)
	}
}

// e2eDespawnPeer drives the despawn happy-path over the wire and asserts the
// container was removed on the stub engine and the durable placement released.
func e2eDespawnPeer(t *testing.T, w *e2eWire, peerID store.AccountID, peerContainer string) {
	t.Helper()
	ctx := w.ctx
	resp, err := w.supervisorClient.Lifecycle(ctx, connect.NewRequest(&compassv1internal.LifecycleCallRequest{
		CallId: "despawn-call-1",
		Call: &compassv1internal.LifecycleCallRequest_Despawn{Despawn: &compassv1internal.DespawnPeerRequest{
			AgentAccountId: string(peerID),
		}},
	}))
	if err != nil {
		t.Fatalf("Lifecycle(despawn) over the socket = %v, want the round-trip result", err)
	}
	// Same-owner despawn is a SUCCESS variant. Mutation: an authz regression
	// that treated the supervisor's own owner's peer as foreign would flip this
	// to the in-band _Error not_found.
	if e := resp.Msg.GetError(); e != nil {
		t.Fatalf("despawn returned in-band error {code=%q msg=%q}, want a despawn result", e.GetCode(), e.GetMessage())
	}
	if resp.Msg.GetDespawn() == nil {
		t.Fatal("despawn result carried no DespawnPeerResponse")
	}
	if got := resp.Msg.GetCallId(); got != "despawn-call-1" {
		t.Fatalf("result call id = %q, want the verbatim %q", got, "despawn-call-1")
	}

	// The container was torn down on the wire: the stub engine's Remove was
	// driven for the peer container (engine id == container name here).
	// Mutation: dropping the hub.Remove in DespawnAsAccount leaves the
	// container live and this reddens.
	if !w.engine.wasRemoved(peerContainer) {
		t.Fatalf("stub engine never saw Remove for the peer container %q; removed=%v", peerContainer, w.engine.removedIDs())
	}
	// The durable placement was released. Mutation: dropping
	// DeleteAgentPlacement leaves the row and this read succeeds instead of
	// missing.
	if _, _, err := w.store.PlacementForAgent(ctx, peerID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("PlacementForAgent(peer) after despawn = %v, want ErrNotFound (placement released)", err)
	}
}

// e2ePeerFailsClosedAfterDespawn asserts the despawned peer can no longer act:
// a fresh dial of its removed socket fails closed at the transport layer and
// nothing new commits to its home channel.
func e2ePeerFailsClosedAfterDespawn(t *testing.T, w *e2eWire, peerID store.AccountID, peerContainer string, peerHome store.ChannelID) {
	t.Helper()
	ctx := w.ctx
	// The peer can no longer act. Despawn's hub.Remove tore the container's
	// AgentGateway socket down (agentHost.Remove -> closeSocket removes the
	// listener AND the socket file), AND hub.Stop unbound the session->account
	// mapping first. So a fresh dial of the peer's (now-removed) socket fails
	// closed at the TRANSPORT layer — the socket file is gone — which is the
	// strongest fail-closed: the peer cannot even reach its door.
	//
	// LAYER NOTE (per the record's ask to document which layer returns what):
	// the observable here is a Connect TRANSPORT error (dial of a removed unix
	// socket), NOT the CodeNotFound RelayCommsCall would return. That
	// CodeNotFound is what surfaces when a session is merely UNBOUND while its
	// socket still serves (a Stop without a Remove); despawn does a full
	// Remove, so the socket layer errors first. Either way the peer is fail-
	// closed. A fresh client is dialed (not the scenario-2 client) because that
	// client's cached conn was force-closed when the listener closed.
	//
	// Mutation: a despawn that stopped short of removing the container (or left
	// the session bound) would let this post SUCCEED — the exact regression
	// this pins.
	client := w.dialPeer(t, peerContainer)
	_, err := client.Comms(ctx, connect.NewRequest(&compassv1internal.CommsCallRequest{
		CallId: "peer-post-after-despawn",
		Call:   &compassv1internal.CommsCallRequest_Post{Post: &compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(peerHome)}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "should never commit"}}}}},
	}))
	if err == nil {
		t.Fatal("peer comms post after despawn SUCCEEDED, want a fail-closed error (container + socket torn down)")
	}

	// And nothing committed: the peer's home channel still holds only the one
	// pre-despawn message.
	msgs, err := w.store.ListMessages(ctx, store.ListMessagesQuery{Actor: peerID, ChannelID: peerHome, Page: store.Page{Limit: 10}})
	if err != nil {
		t.Fatalf("ListMessages(peer home) after despawn = %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("peer home has %d messages after a fail-closed post, want 1 (nothing new committed)", len(msgs))
	}
}

// TestForeignOwnerDespawnOverTheWireIsIndistinguishableNoOp pins the load-
// bearing authz leg: a peer owned by a DIFFERENT owner, despawned by the
// supervisor over the wire, is an INDISTINGUISHABLE in-band not_found — and the
// foreign peer is UNCHANGED (still placed, still bound, still able to post). The
// not-found/forbidden merge: a caller can never probe a foreign peer's existence.
func TestForeignOwnerDespawnOverTheWireIsIndistinguishableNoOp(t *testing.T) {
	w := newE2EWire(t)
	ctx := w.ctx

	// A SECOND owner (user B) and its agent (owner B's agent). The peer under
	// owner B is brought fully online through the REAL spawn chain (lc.Spawn-
	// AsAccount, the same seam the wire delegates into) so it is a genuine placed
	// + bound + socket-serving peer — the fixture is real, only the ASSERTION
	// (the foreign despawn) rides the supervisor's wire.
	userB, err := w.store.CreateUser(ctx, store.NewUser{Handle: "owner-b", DisplayName: "Owner B"})
	if err != nil {
		t.Fatalf("CreateUser(owner B) = %v", err)
	}
	agentB, err := w.store.CreateAgent(ctx, userB.ID, store.NewAgent{Handle: "agent-b", DisplayName: "Agent B"})
	if err != nil {
		t.Fatalf("CreateAgent(agent B) = %v", err)
	}
	peerBResp, err := w.lc.SpawnAsAccount(ctx, agentB.ID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-b",
		DisplayName:     "Peer B",
		ClientRequestId: "spawn-b-1",
	})
	if err != nil {
		t.Fatalf("SpawnAsAccount(agent B) = %v, want success", err)
	}
	peerBID := store.AccountID(peerBResp.GetAgentAccountId())
	peerBContainer := peerBResp.GetContainerName()
	peerBAcc, err := w.store.GetAccount(ctx, peerBID)
	if err != nil {
		t.Fatalf("GetAccount(peer B) = %v", err)
	}
	peerBHome := peerBAcc.Agent.HomeChannelID

	// The supervisor (owner A's agent) despawns owner B's peer over its OWN
	// socket.
	resp, err := w.supervisorClient.Lifecycle(ctx, connect.NewRequest(&compassv1internal.LifecycleCallRequest{
		CallId: "foreign-despawn-1",
		Call: &compassv1internal.LifecycleCallRequest_Despawn{Despawn: &compassv1internal.DespawnPeerRequest{
			AgentAccountId: string(peerBID),
		}},
	}))
	if err != nil {
		t.Fatalf("Lifecycle(foreign despawn) over the socket = %v, want an in-band result", err)
	}
	// The foreign despawn rides back IN-BAND (the _Error variant), NOT a Connect
	// transport error: relay_lifecycle.go renders a tool-level failure as
	// LifecycleCallResult_Error so a single refused call never tears the transport
	// down. Mutation: returning a Connect error here (or a distinct code) instead
	// of the in-band not_found would redden this.
	e := resp.Msg.GetError()
	if e == nil {
		t.Fatalf("foreign despawn returned a success result %+v, want the in-band not_found error", resp.Msg.GetResult())
	}
	// The code is EXACTLY not_found — the same token an UNKNOWN id yields — so a
	// foreign-but-existing peer is indistinguishable from one that does not exist.
	// (connect.CodeNotFound.String() == "not_found".) Mutation: a
	// PermissionDenied (or any code that betrays "this peer exists but is not
	// yours") reddens this — the existence probe the merge exists to prevent.
	if got := e.GetCode(); got != connect.CodeNotFound.String() {
		t.Fatalf("foreign despawn code = %q, want %q (indistinguishable not-found/forbidden merge)", got, connect.CodeNotFound.String())
	}
	// Cross-check indistinguishability against a genuinely unknown id over the
	// same wire: both must return the SAME in-band not_found.
	unknownResp, err := w.supervisorClient.Lifecycle(ctx, connect.NewRequest(&compassv1internal.LifecycleCallRequest{
		CallId: "unknown-despawn-1",
		Call: &compassv1internal.LifecycleCallRequest_Despawn{Despawn: &compassv1internal.DespawnPeerRequest{
			AgentAccountId: "ffffffffffffffffffffffffffffffff", // well-formed, never minted
		}},
	}))
	if err != nil {
		t.Fatalf("Lifecycle(unknown despawn) = %v, want an in-band result", err)
	}
	ue := unknownResp.Msg.GetError()
	if ue == nil || ue.GetCode() != connect.CodeNotFound.String() {
		t.Fatalf("unknown despawn error = %+v, want the SAME in-band not_found the foreign despawn returned", ue)
	}
	// Indistinguishability is not only the CODE — the in-band MESSAGE rides the
	// wire too (LifecycleCallError.message = err.Error()). Lock message parity so a
	// regression that returns not_found with a DISTINCT message on the foreign path
	// (e.g. "peer not owned by you" vs "peer not found") — reopening the existence
	// side-channel the merge exists to close — reddens here.
	if e.GetMessage() != ue.GetMessage() {
		t.Fatalf("foreign despawn message = %q, unknown despawn message = %q; the two MUST be byte-identical or the message leaks peer existence", e.GetMessage(), ue.GetMessage())
	}

	// Owner B's peer is UNCHANGED — the no-op half of the merge. Still placed.
	// Mutation: any despawn that acted on the foreign target (tore it down before
	// the owner check) reddens this placement read.
	if _, container, err := w.store.PlacementForAgent(ctx, peerBID); err != nil || container != peerBContainer {
		t.Fatalf("PlacementForAgent(peer B) after foreign despawn = (%q, %v), want (%q, nil) — foreign peer must be untouched", container, err, peerBContainer)
	}
	// The stub engine never removed owner B's container.
	if w.engine.wasRemoved(peerBContainer) {
		t.Fatalf("stub engine saw Remove for foreign peer container %q — the foreign despawn tore it down", peerBContainer)
	}
	// And owner B's peer can STILL act: its socket + session binding are intact,
	// so a post over its socket still commits under the peer B account.
	clientB := w.dialPeer(t, peerBContainer)
	postResp, err := clientB.Comms(ctx, connect.NewRequest(&compassv1internal.CommsCallRequest{
		CallId: "peer-b-post-1",
		Call:   &compassv1internal.CommsCallRequest_Post{Post: &compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(peerBHome)}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "peer B still alive"}}}}},
	}))
	if err != nil {
		t.Fatalf("Comms(post) over peer B socket after the foreign despawn = %v, want success (peer B untouched)", err)
	}
	if got := postResp.Msg.GetPost().GetMessage().GetAuthorAccountId(); got != string(peerBID) {
		t.Fatalf("peer B post author = %q, want the peer B account %q (session binding intact)", got, peerBID)
	}
}

// --- the assembled wire ------------------------------------------------------

// e2eWire is the whole spawn/despawn wire stood up once per test: a real store,
// the server-package hub (comms as CommsCaller, lifecycleService as
// LifecycleCaller), a real Runner dialed into the mounted RunnerService door
// over a stub engine that serves real per-container sockets, and a bound
// SUPERVISOR session whose socket the tests drive Lifecycle over.
type e2eWire struct {
	ctx              context.Context
	dsn              string
	store            *store.Store
	hub              *runnerhub.Hub
	lc               *lifecycleService
	engine           *e2eStubRuntime
	runtimeDir       string
	supervisor       store.Account
	supervisorOwner  store.AccountID // the supervisor agent's owner (a NON-admin user) — the F2 inheritance target
	adminID          store.AccountID // the bootstrap admin — DISTINCT from supervisorOwner, so "never admin-literal" is a real assertion
	supervisorClient compassv1internalconnect.AgentGatewayClient
}

// newE2EWire assembles the full wire (option B): store + hub + real Runner over
// a socket-serving stub engine, then provisions and starts a supervisor session
// and returns a client dialed to its per-container socket. Cleanups are ordered
// so the runtime-dir removal runs LAST (after the loop has left dispatch and the
// host has drained its sockets) — see runnerloop.RunSessionsLoop and the host-Close cleanup
// below.
func newE2EWire(t *testing.T) *e2eWire {
	t.Helper()
	dsn := pgtest.RequireDSN(t) // hard-fails on infra-missing (SEA-1523), never skips silently.
	// First cleanup registered, so LIFO removes the tree LAST — after the loop's
	// drain and the host's socket close have both run (see below).
	runtimeDir := runnertest.ShortRuntimeDir(t, e2eNamePrefix, e2eAccountIDHexLen)
	ctx, cancel := context.WithCancel(context.Background()) // the test root context
	// Registered adjacent to WithCancel so the fixture setup below cannot t.Fatalf
	// out with the context never cancelled. runnerloop.RunSessionsLoop registers cancel again
	// later; CancelFunc is idempotent, so the ordering the loop documents is
	// unchanged.
	t.Cleanup(cancel)

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(st.Close)

	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "Administrator"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	// Own the supervisor under a NON-admin user, not the bootstrap admin. This is
	// what makes the F2 "never admin-literal" arm real: with supervisorOwner !=
	// admin.ID, a regression that hard-coded the bootstrap admin as the peer's
	// owner yields owner == admin.ID != supervisorOwner and reddens here, instead
	// of silently matching the fixture. (admin still exists — comms is anchored to
	// it — it just does not own the supervisor.)
	supervisorOwnerUser, err := st.CreateUser(ctx, store.NewUser{Handle: "supervisor-owner", DisplayName: "Supervisor Owner"})
	if err != nil {
		t.Fatalf("CreateUser(supervisor owner): %v", err)
	}
	supervisor, err := st.CreateAgent(ctx, supervisorOwnerUser.ID, store.NewAgent{Handle: "supervisor", DisplayName: "Supervisor"})
	if err != nil {
		t.Fatalf("CreateAgent(supervisor): %v", err)
	}
	// runnertest.ShortRuntimeDir budgeted the socket path against a MODEL of the account
	// id (e2eAccountIDHexLen "f"s), before an account existed. Tie the model to
	// the real minted width now: widen store ids and this reddens here, rather
	// than silently invalidating the budget and letting the real socket path
	// overrun.
	if got := len(supervisor.ID); got != e2eAccountIDHexLen {
		t.Fatalf("minted account id is %d chars, but ShortRuntimeDir budgeted for %d; update e2eAccountIDHexLen", got, e2eAccountIDHexLen)
	}

	// The hub, wired exactly as the server package builds it (sinks.go
	// newRunnerHub): the board as lifecycle sink, comms as the conversation sink +
	// CommsCaller, a real session tail. comms is the real agent-comms execution
	// leg the peer's socket Post rides. No relayed conversation/lifecycle frame
	// reaches the write-through sinks on this path, so they are effectively
	// no-ops; only the RelayCommsCall + RelayLifecycleCall legs are exercised.
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	brd := board.NewProjection(bus)
	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(commsBus.Close)
	commsSvc := comms.NewComms(st, commsBus, admin.ID)
	hub := newRunnerHub(st, brd, newSessionTail(), commsSvc, discardLogE2E())

	// Wire the lifecycleService as the hub's LifecycleCaller — the serve.go:250
	// pattern. Only package server can construct it (unexported) and set it.
	lc := newLifecycleService(st, hub)
	hub.SetLifecycleCaller(lc)

	// Mount the RunnerService door on an h2c server, accepting one Runner token.
	resolver := &e2eResolver{token: "runner-tok", subj: store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}}
	url := mountRunnerServerE2E(t, hub, resolver.resolve)

	// A real Runner dials in over the wire with a socket-serving stub engine.
	engine := newE2EStubRuntime(t)
	link, err := runner.Dial(ctx, runner.RunnerConfig{
		RunnerID:   "runner-1",
		ServerAddr: url,
		Token:      "runner-tok",
		Engine:     engine,
		HTTPClient: runnertest.H2CClient(t),
	})
	if err != nil {
		t.Fatalf("runner.Dial: %v", err)
	}
	if link.Reattached() {
		t.Fatal("first enroll reattached = true, want false")
	}

	specs, err := runner.NewConfigSpecBuilder(runner.SpecDefaults{
		Image:       "compass-agent:latest",
		Egress:      runtime.MustAllowEgress("github.com"),
		CheckoutDir: "/work/repo",
		HomeDir:     "/home/agent",
		UID:         1000,
		NamePrefix:  e2eNamePrefix,
	})
	if err != nil {
		t.Fatalf("NewConfigSpecBuilder: %v", err)
	}
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(engine, registry)
	host := runner.NewSessionHost(link, rt, registry, engine, specs, runner.AgentHostConfig{RuntimeDir: runtimeDir}, discardLogE2E(), nil)
	// host.Close drains every per-container socket. Registered BEFORE
	// runnerloop.RunSessionsLoop so under LIFO it runs AFTER the loop's cancel+drain — the
	// production order (run.go: cancel, RunSessions returns, THEN host.Close), and
	// still before the runtime-dir removal registered at the very top. The fresh
	// bounded ctx is the sanctioned test-root exemption: the test ctx is cancelled
	// by the time this runs (mirrors the reference's assertCleanShutdown close).
	if closer, ok := host.(interface{ Close(ctx context.Context) }); ok {
		t.Cleanup(func() {
			closeCtx, cancelClose := context.WithTimeout(context.Background(), e2eTimeout)
			defer cancelClose()
			closer.Close(closeCtx)
		})
	}
	runnerloop.RunSessionsLoop(t, ctx, cancel, link, host, e2eTimeout)

	// Provision + start the supervisor: this proves the seam live (the retrying
	// Provision is also the attach gate) and binds the supervisor's session to its
	// account, so a Lifecycle call over its socket resolves the supervisor as
	// caller.
	supervisorContainer := provisionWhenSeamLiveE2E(t, ctx, hub, supervisor.ID)
	if _, err := hub.Start(ctx, "start-supervisor", &compassv1.StartAgentSessionRequest{ContainerName: supervisorContainer}); err != nil {
		t.Fatalf("hub.Start(supervisor) = %v", err)
	}

	w := &e2eWire{
		ctx:             ctx,
		dsn:             dsn,
		store:           st,
		hub:             hub,
		lc:              lc,
		engine:          engine,
		runtimeDir:      runtimeDir,
		supervisor:      supervisor,
		supervisorOwner: supervisorOwnerUser.ID,
		adminID:         admin.ID,
	}
	w.supervisorClient = w.dialPeer(t, supervisorContainer)
	return w
}

// dialPeer builds a fresh AgentGatewayClient dialing the per-container socket at
// RuntimeDir/containers/<container>/agent.sock — the exact wire an in-container
// agent bind-mounts and dials. Fresh per call so a post-despawn dial cannot ride
// a cached conn to a torn-down socket.
func (w *e2eWire) dialPeer(t *testing.T, containerName string) compassv1internalconnect.AgentGatewayClient {
	t.Helper()
	return runnertest.DialAgentSocket(t, agentSocketPathE2E(w.runtimeDir, containerName))
}

// --- server-package-specific wire helpers -----------------------------------
//
// The generic socket scaffolding (runtime dir, h2c client, dialer, sessions
// loop) lives in the shared internal/runnertest package. What remains here is
// the wire this test builds on top of it and cannot share: the single-token
// resolver, the stub runtime, and the server-package assembly helpers. They
// carry an `E2E` suffix so they never collide with an existing server-package
// test helper. The load-bearing WHY-comments are kept.

// e2eResolver accepts exactly one Runner token — the minimal TokenResolver the
// mounted RunnerService door authenticates the stub Runner with.
type e2eResolver struct {
	token string
	subj  store.Subject
}

func (r *e2eResolver) resolve(_ context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
	if presented != r.token || r.subj.Kind != want {
		return store.Subject{}, store.ErrNotFound
	}
	return r.subj, nil
}

// e2eStubRuntime is the fake ContainerRuntime backing the Runner: ExecStreaming
// spawns a real, terminatable child (a shell-stub `podman` exec-ing `sleep`) so
// the session's exec reaps on ctx cancel / Stop and StartAgent's pipe drains end
// on that reap. Post-#16 nothing rides stdout/stderr — the agent's protocol
// travels the AgentGateway socket — so the child's own (empty) pipes are what
// StartAgent drains and no frame is injected.
//
// It DIFFERS from the reference stub in two load-bearing ways T7 needs:
//   - Create returns a PER-CALL-UNIQUE engine id (the container name), not a
//     fixed "fake-id". T7 provisions MULTIPLE containers through this one runner;
//     a fixed id would alias every container's engine handle onto one id. The
//     container NAME already differs per account (NamePrefix+accountID), so it is
//     the natural disambiguator.
//   - Remove is RECORDED (keyed by that engine id == container name), so
//     "despawn removed the container" is an observed wire fact, not a mock
//     expectation.
type e2eStubRuntime struct {
	cli *runtime.PodmanCLI // shell-stub podman → a real terminatable Process

	mu      sync.Mutex
	removed map[string]bool
}

func newE2EStubRuntime(t *testing.T) *e2eStubRuntime {
	t.Helper()
	dir := t.TempDir()
	stub := "#!/bin/sh\nexec sleep 120\n"
	prog := filepath.Join(dir, "podman-stub.sh")
	if err := os.WriteFile(prog, []byte(stub), 0o755); err != nil {
		t.Fatalf("writing streaming stub: %v", err)
	}
	return &e2eStubRuntime{cli: runtime.NewPodmanCLI().WithProgram(prog), removed: map[string]bool{}}
}

func (f *e2eStubRuntime) Create(_ context.Context, spec runtime.ContainerSpec) (runtime.ContainerID, error) {
	// Per-call-unique engine id: the container name (NamePrefix+accountID), which
	// already differs per account — see the type doc for why a fixed id collides.
	return runtime.ContainerID(spec.Name), nil
}
func (f *e2eStubRuntime) Start(context.Context, runtime.ContainerID) error { return nil }
func (f *e2eStubRuntime) Exec(context.Context, runtime.ContainerID, runtime.ExecSpec) (runtime.ExecOutput, error) {
	return runtime.ExecOutput{}, nil
}
func (f *e2eStubRuntime) ExecStreaming(ctx context.Context, id runtime.ContainerID, spec runtime.StreamingExecSpec) (*runtime.StreamingExec, error) {
	// A real streaming exec against the shell stub: a live, terminatable Process
	// whose stdout/stderr pipes StartAgent drains. The stub just sleeps, so the
	// pipes stay empty until the exec's context cancels (loop teardown) or Stop
	// terminates it (despawn/Stop).
	return f.cli.ExecStreaming(ctx, id, spec)
}
func (f *e2eStubRuntime) Stop(context.Context, runtime.ContainerID, time.Duration) error {
	return nil
}
func (f *e2eStubRuntime) Remove(_ context.Context, id runtime.ContainerID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed[string(id)] = true
	return nil
}
func (f *e2eStubRuntime) Exists(context.Context, string) (bool, error) { return false, nil }
func (f *e2eStubRuntime) MountLabel(context.Context, runtime.ContainerID) (string, error) {
	return "", nil
}
func (f *e2eStubRuntime) Resize(context.Context, runtime.ContainerID, runtime.ResourceLimits) error {
	return nil
}

// wasRemoved reports whether Remove was driven for the container (engine id ==
// container name). Guarded because the dispatch loop calls Remove on its own
// goroutine while the test reads.
func (f *e2eStubRuntime) wasRemoved(containerName string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.removed[containerName]
}

// removedIDs snapshots the recorded removals for failure output.
func (f *e2eStubRuntime) removedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.removed))
	for id := range f.removed {
		ids = append(ids, id)
	}
	return ids
}

// agentSocketPathE2E is the host path the Runner serves a container's
// AgentGateway socket at: RuntimeDir/containers/<container>/agent.sock (host.go's
// agentSocketDir/agentSocketFile layout). Reconstructed here rather than read off
// the host — h.sockets is unexported and package-internal — so it is the exact
// socket an in-container agent bind-mounts and dials.
func agentSocketPathE2E(runtimeDir, containerName string) string {
	return filepath.Join(runtimeDir, "containers", containerName, "agent.sock")
}

// provisionWhenSeamLiveE2E provisions the workspace through the public hub path →
// the Runner launches the (stub) container and returns its name.
//
// The Sessions stream is server-speaks-first: RunSessions' bootstrap Send
// flushes the headers that run the server handler's router.attach, and that
// round-trip is async to the loop goroutine. A command dispatched into the
// pre-attach window gets a retriable Unavailable ("no live runner sessions
// stream"). So gate on the seam being live by retrying, idempotent on the stable
// request id "prov-supervisor" so there is no double-provision, yielding to the
// handler goroutine between probes. A deadline bounds it, so a genuinely wedged
// seam fails fast rather than spinning, and nothing here is a sleep.
func provisionWhenSeamLiveE2E(t *testing.T, ctx context.Context, hub *runnerhub.Hub, agentID store.AccountID) string {
	t.Helper()
	deadline := time.After(e2eTimeout)
	for {
		resp, _, err := hub.Provision(ctx, "prov-supervisor", &compassv1.ProvisionAgentWorkspaceRequest{
			AgentAccountId: string(agentID),
		})
		if err == nil {
			name := resp.GetContainerName()
			if name == "" {
				t.Fatal("Provision returned an empty container name")
			}
			return name
		}
		if connect.CodeOf(err) != connect.CodeUnavailable {
			t.Fatalf("hub.Provision over the seam = %v", err)
		}
		select {
		case <-deadline:
			t.Fatalf("hub.Provision never reached a live Sessions stream: %v", err)
		default:
		}
		stdruntime.Gosched()
	}
}

// mountRunnerServerE2E mounts the real RunnerService door on an httptest server
// that speaks cleartext HTTP/2 (h2c + HTTP/1) and returns its base URL. The
// server is torn down via t.Cleanup — registered here (early) so under LIFO it
// closes AFTER the Sessions loop has been cancelled (see runnerloop.RunSessionsLoop).
func mountRunnerServerE2E(t *testing.T, hub *runnerhub.Hub, resolve runnerhub.TokenResolver) string {
	t.Helper()
	path, handler := runnerhub.NewMountedHandler(hub, resolve, nil, nil)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2() // the server-package helper (serve.go)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

// discardLogE2E is a throwaway logger for the Runner + hub diagnostics this test
// does not assert on.
func discardLogE2E() *slog.Logger { return slog.New(slog.DiscardHandler) }

// textOfE2E returns the first text block of a stored message.
func textOfE2E(m store.Message) string {
	for _, b := range m.Blocks {
		if b.Text != nil {
			return *b.Text
		}
	}
	return ""
}
