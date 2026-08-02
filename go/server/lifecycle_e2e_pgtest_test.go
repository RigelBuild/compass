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
// real-runner-over-stub-engine socket shape lifted from
// runnerhub/integration_pgtest_test.go. Those helpers live in package
// runnerhub_test, so the ones this needs are PORTED below (copied + adapted,
// keeping the load-bearing WHY-comments — shortRuntimeDir's sun_path budget,
// runSessionsLoop's LIFO drain ordering); nothing is exported from runnerhub to
// make this compile.
//
// Each assertion carries a mutation comment: the plausible regression in the
// (already merged, green) spine that would redden it — the "red-first" the
// record's T7 acceptance means here.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	stdruntime "runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/board"
	"github.com/sealedsecurity/compass/go/internal/comms"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/sealedsecurity/compass/go/internal/pgtest"
	"github.com/sealedsecurity/compass/go/internal/runner"
	"github.com/sealedsecurity/compass/go/internal/runnerhub"
	"github.com/sealedsecurity/compass/go/internal/runtime"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// e2eTimeout bounds every wire wait so a wedged seam fails fast rather than
// hanging the suite. It is a deadline safety net, never a synchronization
// device: the wire is event-gated (provisionWhenSeamLive's retry, the socket
// round-trip's own blocking return), never slept on.
const e2eTimeout = 30 * time.Second

// e2eNamePrefix is the container-name prefix this test wires into its
// SpecDefaults, hoisted so shortRuntimeDir models the same name the Runner
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
	ctx := w.ctx

	// The peer the whole test spawns, despawns, and asserts fail-closed on.
	// Captured across subtests because the state is sequential.
	var (
		peerID        store.AccountID
		peerContainer string
		peerHome      store.ChannelID
	)

	t.Run("spawn happy-path inherits the caller's owner (F2)", func(t *testing.T) {
		resp, err := w.supervisorClient.Lifecycle(ctx, connect.NewRequest(&compassv1internal.LifecycleCallRequest{
			CallId: "spawn-call-1",
			Call: &compassv1internal.LifecycleCallRequest_Spawn{Spawn: &compassv1internal.SpawnPeerRequest{
				Handle:          "peer-1",
				DisplayName:     "Peer One",
				InitialPrompt:   "go",
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
	})

	t.Run("peer's own comms call resolves to the peer account", func(t *testing.T) {
		if peerID == "" {
			t.Fatal("spawn subtest did not run; peer is unset")
		}
		// Dial the PEER's per-container socket and post as the in-container peer
		// agent would. The Server resolves the peer's session->account binding and
		// attributes the post to the PEER — never the supervisor, never admin.
		// Mutation: if spawn promoted the session binding onto the wrong account
		// (or the supervisor's), this author check reddens — the proof that spawn
		// wired a genuinely independent bound session.
		client := w.dialPeer(t, peerContainer)
		resp, err := client.Comms(ctx, connect.NewRequest(&compassv1internal.CommsCallRequest{
			CallId: "peer-post-1",
			Call: &compassv1internal.CommsCallRequest_Post{Post: &compassv1.PostMessageRequest{
				Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(peerHome)},
				Blocks:    []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: peerPostText}}},
			}},
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
		msgs, err := w.store.ListMessages(ctx, peerID, store.ContainerRef{ChannelID: peerHome}, store.Page{Limit: 10})
		if err != nil {
			t.Fatalf("ListMessages(peer home) = %v", err)
		}
		if len(msgs) != 1 || textOfE2E(msgs[0]) != peerPostText {
			t.Fatalf("peer home has %d messages (want 1 with the posted body); got %+v", len(msgs), msgs)
		}
		if msgs[0].AuthorAccountID != peerID {
			t.Fatalf("stored peer message author = %q, want the peer account %q", msgs[0].AuthorAccountID, peerID)
		}
	})

	t.Run("despawn removes the container and deletes the placement", func(t *testing.T) {
		if peerID == "" {
			t.Fatal("spawn subtest did not run; peer is unset")
		}
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
	})

	t.Run("peer fails closed after despawn", func(t *testing.T) {
		if peerID == "" {
			t.Fatal("spawn subtest did not run; peer is unset")
		}
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
			Call: &compassv1internal.CommsCallRequest_Post{Post: &compassv1.PostMessageRequest{
				Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(peerHome)},
				Blocks:    []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "should never commit"}}},
			}},
		}))
		if err == nil {
			t.Fatal("peer comms post after despawn SUCCEEDED, want a fail-closed error (container + socket torn down)")
		}

		// And nothing committed: the peer's home channel still holds only the one
		// pre-despawn message.
		msgs, err := w.store.ListMessages(ctx, peerID, store.ContainerRef{ChannelID: peerHome}, store.Page{Limit: 10})
		if err != nil {
			t.Fatalf("ListMessages(peer home) after despawn = %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("peer home has %d messages after a fail-closed post, want 1 (nothing new committed)", len(msgs))
		}
	})
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
		InitialPrompt:   "go",
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
		Call: &compassv1internal.CommsCallRequest_Post{Post: &compassv1.PostMessageRequest{
			Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(peerBHome)},
			Blocks:    []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "peer B still alive"}}},
		}},
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
// host has drained its sockets) — see runSessionsLoop and the host-Close cleanup
// below.
func newE2EWire(t *testing.T) *e2eWire {
	t.Helper()
	dsn := pgtest.RequireDSN(t) // hard-fails on infra-missing (SEA-1523), never skips silently.
	// First cleanup registered, so LIFO removes the tree LAST — after the loop's
	// drain and the host's socket close have both run (see below).
	runtimeDir := shortRuntimeDirE2E(t)
	ctx, cancel := context.WithCancel(context.Background()) // the test root context
	// Registered adjacent to WithCancel so the fixture setup below cannot t.Fatalf
	// out with the context never cancelled. runSessionsLoop registers cancel again
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
	// shortRuntimeDirE2E budgeted the socket path against a MODEL of the account
	// id (e2eAccountIDHexLen "f"s), before an account existed. Tie the model to
	// the real minted width now: widen store ids and this reddens here, rather
	// than silently invalidating the budget and letting the real socket path
	// overrun.
	if got := len(supervisor.ID); got != e2eAccountIDHexLen {
		t.Fatalf("minted account id is %d chars, but shortRuntimeDirE2E budgeted for %d; update e2eAccountIDHexLen", got, e2eAccountIDHexLen)
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
	hub := newRunnerHub(brd, newSessionTail(), commsSvc, discardLogE2E())

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
		HTTPClient: h2cClientE2E(t),
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
	// runSessionsLoop so under LIFO it runs AFTER the loop's cancel+drain — the
	// production order (run.go: cancel, RunSessions returns, THEN host.Close), and
	// still before the runtime-dir removal registered at the very top. The fresh
	// bounded ctx is the sanctioned test-root exemption: the test ctx is cancelled
	// by the time this runs (mirrors the reference's assertCleanShutdown close).
	if closer, ok := host.(interface{ Close(context.Context) }); ok {
		t.Cleanup(func() {
			closeCtx, cancelClose := context.WithTimeout(context.Background(), e2eTimeout)
			defer cancelClose()
			closer.Close(closeCtx)
		})
	}
	runSessionsLoopE2E(t, ctx, cancel, link, host)

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
	return dialAgentSocketE2E(t, agentSocketPathE2E(w.runtimeDir, containerName))
}

// --- ported helpers (from runnerhub/integration_pgtest_test.go) --------------
//
// These are lifted from the runnerhub whole-wire reference and adapted to
// package server; the load-bearing WHY-comments are kept. They carry an `E2E`
// suffix so they never collide with the runnerhub originals or any existing
// server-package test helper, and so a future reader sees at a glance they are
// this file's ported copies rather than shared scaffolding.

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

// dialAgentSocketE2E builds a real generated AgentGatewayClient that dials the
// unix socket at path over prior-knowledge h2c — the same cleartext-HTTP/2 door
// the per-container listener serves — so the Gateway is exercised over the wire
// it ships on. The base URL is a placeholder; DialContext routes every dial to
// the socket.
func dialAgentSocketE2E(t *testing.T, path string) compassv1internalconnect.AgentGatewayClient {
	t.Helper()
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{
		Protocols: p,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return compassv1internalconnect.NewAgentGatewayClient(&http.Client{Transport: tr}, "http://unix")
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

// runSessionsLoopE2E starts the Runner's dispatch loop and registers the teardown
// that must bracket it. The ordering is load-bearing in both directions, which is
// why it lives beside the goroutine rather than beside context.WithCancel.
//
// It must run BEFORE httptest's srv.Close (registered inside mountRunnerServerE2E,
// therefore earlier, therefore later under LIFO): Close waits on its handlers,
// and the live Sessions handler returns only once ctx is cancelled, so cancelling
// after Close deadlocks the entire cleanup stack.
//
// It must run AFTER the runtime dir removal registered at the top of newE2EWire,
// so LIFO reclaims that tree only once the loop has left dispatch. Cancel alone
// would not do it: cancel signals and returns, so the WAIT is what makes the
// ordering mean anything.
//
// If the drain times out the later cleanups still run — a cleanup cannot cancel
// the ones registered before it — so the timeout arm reports the collision rather
// than averting it. That is the right trade at that point: the test has already
// failed.
func runSessionsLoopE2E(t *testing.T, ctx context.Context, cancel context.CancelFunc, link *runner.ServerLink, host runner.SessionHost) {
	t.Helper()
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- link.RunSessions(ctx, host, discardLogE2E())
		close(loopDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-loopDone:
			// A cancelled ctx is the expected end; a stream error that killed the
			// loop must surface here rather than be discarded.
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("RunSessions ended with %v", err)
			}
		case <-time.After(e2eTimeout):
			t.Errorf("RunSessions still running %s after cancel; the runtime dir removal runs anyway and will race an in-flight command", e2eTimeout)
		}
	})
}

// shortRuntimeDirE2E is a Runner RuntimeDir bounded to fit the AF_UNIX sun_path
// limit, replacing t.TempDir() for this file's socket-opening tests. The Runner
// appends a fixed tail to its RuntimeDir
// (/containers/compass-agent-<32hex>/agent.sock, host.go) at the store-minted
// 32-hex id. t.TempDir() derives its path from the TEST NAME, and this package's
// test names exceed that budget once a real socket is opened, so a fixed short
// root removes the TEST-NAME dependency. It does not make the budget
// unconditional: the root still comes from TMPDIR, and a deep one re-inflates it,
// so the resulting path is asserted rather than assumed.
//
// This site FAILS rather than skips on an over-budget root: a skip would silently
// drop the only end-to-end coverage of the socket path. The cap is DERIVED the
// way the production guard derives it (sun_path is not one size across the
// platforms //go:build unix admits — 108 on linux, 104 on darwin/BSD, 1023 on
// aix — and this file is //go:build unix) rather than written as a literal,
// because that constant is unexported. The T7 wire opens MULTIPLE sockets under
// this one root, but all share the RuntimeDir/containers/<name> layout and every
// name is the same length (prefix + 32-hex), so the single longest-path budget
// covers them all.
func shortRuntimeDirE2E(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cr") //nolint:usetesting // t.TempDir embeds the test name, which is what put this path over the sun_path cap — the bug this helper exists to prevent
	if err != nil {
		t.Fatalf("MkdirTemp for runner runtime dir: %v", err)
	}
	// sun_path holds the path plus a NUL, so the usable cap is one less than the
	// platform's array.
	const sunPathMax = len(syscall.RawSockaddrUnix{}.Path) - 1
	longest := filepath.Join(dir, "containers", e2eNamePrefix+strings.Repeat("f", e2eAccountIDHexLen), "agent.sock")
	if len(longest) > sunPathMax {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			t.Errorf("removing over-budget runner runtime dir %q: %v", dir, rmErr)
		}
		t.Fatalf("runner runtime dir %q yields a %d-byte agent socket path, over the %d-byte sun_path cap (TMPDIR too deep)", dir, len(longest), sunPathMax)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("removing runner runtime dir %q: %v", dir, err)
		}
	})
	return dir
}

// h2cClientE2E is the Runner's HTTP client speaking prior-knowledge cleartext
// HTTP/2 to the mounted RunnerService door.
func h2cClientE2E(t *testing.T) *http.Client {
	t.Helper()
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{
		Protocols: p,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// mountRunnerServerE2E mounts the real RunnerService door on an httptest server
// that speaks cleartext HTTP/2 (h2c + HTTP/1) and returns its base URL. The
// server is torn down via t.Cleanup — registered here (early) so under LIFO it
// closes AFTER the Sessions loop has been cancelled (see runSessionsLoopE2E).
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
func discardLogE2E() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// textOfE2E returns the first text block of a stored message.
func textOfE2E(m store.Message) string {
	for _, b := range m.Blocks {
		if b.Text != nil {
			return *b.Text
		}
	}
	return ""
}
