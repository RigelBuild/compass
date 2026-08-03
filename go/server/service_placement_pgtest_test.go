//go:build pgtest && unix

package server

// The two placement-dependent handler seams, against a real Postgres AND a real
// Runner door: ProvisionAgentWorkspace's placement write, and StartAgentSession's
// placement read plus its post-relay roll-back. Both defects these cover live
// exactly here — the store primitives are individually correct under either bug,
// so only a test that drives the handler with a relay behind it can observe them.
//
// The Runner is a fake Sessions loop dialed into the REAL mounted RunnerService
// door (the runnerhub seam's own test shape, runnerhub/seam_test.go): it records
// every command the Server pushes and answers Provision/Start/Stop, so "the
// handler stopped the session it could not record" is an observed wire command,
// not a mock expectation.
//
// Two facts are seeded/read with pgx directly rather than through the Store: the
// backfilled placement (runner_id = '' is exactly what RecordAgentPlacement
// REFUSES, by design — only the migration writes it) and the raw agent_sessions
// row (the store exposes only the authz predicate over it). Both go through the
// pgtest DSN, so they land in this test's isolated schema.

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
	"github.com/sealedsecurity/compass/go/internal/auth"
	"github.com/sealedsecurity/compass/go/internal/board"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/sealedsecurity/compass/go/internal/pgtest"
	"github.com/sealedsecurity/compass/go/internal/runnerhub"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// The fixed identities the fake Runner answers with, so assertions name them
// directly rather than threading values through the harness.
const (
	fakeRunnerID    = "runner-1"
	fakeRunnerToken = "cnVubmVyLXRva2Vu" //nolint:gosec // a test bearer, hashed into this test's own schema
	fakeContainer   = "compass-agent-c1"
	fakeSessionID   = "sess-relayed"
)

// placementFixture is one service wired to a real store and a real Runner door,
// with a fake Runner attached and its command router proven live.
type placementFixture struct {
	dsn    string
	store  *store.Store
	hub    *runnerhub.Hub
	client compassv1connect.CompassServiceClient
	runner *recordingRunner
	// agentID is a real agent account (with its home channel) that every
	// placement and session in these tests belongs to.
	agentID store.AccountID
}

// newPlacementFixture stands up store + hub + service + the mounted Runner door,
// dials the door with a fake Runner, and returns only once that Runner's command
// router is attached — so a dispatched command reaches the fake rather than
// racing the stream open.
func newPlacementFixture(t *testing.T) placementFixture {
	return newPlacementFixtureWith(t, false)
}

// newPlacementFixtureWith is newPlacementFixture with a fake Runner that either
// answers Stop (withholdStop=false) or accepts but never answers it
// (withholdStop=true) — the wedged-Runner shape the rollback-bound test drives.
func newPlacementFixtureWith(t *testing.T, withholdStop bool) placementFixture {
	ctx := context.Background() // the test root context
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
	agent, err := st.CreateAgent(ctx, admin.ID, store.NewAgent{Handle: "atlas", DisplayName: "Atlas"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// The Runner's bearer, resolvable through the production resolver: the door
	// authenticates the fake Runner exactly as it authenticates a real one.
	if err := st.PutTokenHash(ctx, sha256.Sum256([]byte(fakeRunnerToken)),
		store.Subject{Kind: store.SubjectRunner, ID: fakeRunnerID}); err != nil {
		t.Fatalf("PutTokenHash(runner): %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	brd := board.NewProjection(bus)
	tail := newSessionTail()
	hub := newRunnerHub(st, brd, tail, nil, slog.New(slog.DiscardHandler))
	svc := newService("test", bus, st, hub, brd, tail)

	return placementFixture{
		dsn:     dsn,
		store:   st,
		hub:     hub,
		client:  newH2CClient(t, newH2CTestServer(t, svc)),
		runner:  attachFakeRunner(t, st, hub, withholdStop),
		agentID: agent.ID,
	}
}

// TestStartAgentSessionWithBackfilledPlacementRecordsSession is the upgrade-path
// assertion at the handler seam: a placement whose runner_id is the EMPTY
// SENTINEL — byte-for-byte what 0004's backfill writes for a container
// provisioned before the upgrade — still lets StartAgentSession resolve its
// owner and record the session. Without that backfill such a container has no
// placement row at all, AgentForContainer is ErrNotFound, and the container is
// permanently un-Startable; this test is what that regression reddens.
//
// It also pins the other half of why ” is the RIGHT sentinel rather than a
// tolerated placeholder: the same row Start resolves is invisible to reattach,
// so it is never re-driven against a Runner we only guessed at.
func TestStartAgentSessionWithBackfilledPlacementRecordsSession(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background() // the test root context

	// Exactly the row 0004's backfill INSERT produces: real agent, real
	// container name, empty runner id. Seeded with SQL because
	// RecordAgentPlacement refuses an empty runner id — only the migration
	// writes this shape.
	execSQL(t, ctx, f.dsn,
		`INSERT INTO agent_placements (agent_account_id, runner_id, container_name) VALUES ($1, '', $2)`,
		string(f.agentID), fakeContainer)

	resp, err := f.client.StartAgentSession(ctx, connect.NewRequest(&compassv1.StartAgentSessionRequest{
		ContainerName: fakeContainer,
	}))
	if err != nil {
		t.Fatalf("StartAgentSession on a backfilled placement = %v, want success (a pre-upgrade container must stay Startable)", err)
	}
	if got := resp.Msg.GetSessionId(); got != fakeSessionID {
		t.Fatalf("session id = %q, want %q (the Runner's answer)", got, fakeSessionID)
	}

	// The ownership row exists and names the placement's agent: keeping the
	// container Startable is only worth anything if its session is owned.
	if got := sessionOwner(t, ctx, f.dsn, fakeSessionID); got != string(f.agentID) {
		t.Fatalf("session %q is owned by %q, want %q", fakeSessionID, got, f.agentID)
	}

	// Reattach cannot see the sentinel row: the guard rejects an empty runner id
	// outright, and no enrolled Runner has an empty id (it is the token
	// subject), so a backfilled placement is never re-driven at a guess.
	if _, err := f.store.ListAgentPlacementsForRunner(ctx, ""); !errors.Is(err, store.ErrInvalidArgument) {
		t.Fatalf(`ListAgentPlacementsForRunner("") = %v, want ErrInvalidArgument (the sentinel must be invisible to reattach)`, err)
	}
	placements, err := f.store.ListAgentPlacementsForRunner(ctx, fakeRunnerID)
	if err != nil {
		t.Fatalf("ListAgentPlacementsForRunner(%q): %v", fakeRunnerID, err)
	}
	if len(placements) != 0 {
		t.Fatalf("runner %q sees %d placements, want 0 (a backfilled row belongs to no known Runner)", fakeRunnerID, len(placements))
	}
}

// TestStartAgentSessionStopsTheSessionItCannotRecord pins the anti-stranding
// invariant: when a post-relay store step fails, the Runner has ALREADY started
// an agent, so the handler must tear it back down rather than return an error
// that discards the only handle to it. Driven by the real failure — an UNPLACED
// container, so AgentForContainer is ErrNotFound — and asserted on the wire: the
// fake Runner must have received a Stop naming the session Start just minted,
// and the returned error must name that session id so an operator can reap it
// even if the Stop itself had failed.
//
// Under the pre-fix handler the Runner sees a Start and no Stop and the error
// carries no session id: the session runs on, unaddressable forever.
func TestStartAgentSessionStopsTheSessionItCannotRecord(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background() // the test root context

	// No placement is seeded, so the post-relay AgentForContainer read fails
	// after the Runner has started the agent.
	_, err := f.client.StartAgentSession(ctx, connect.NewRequest(&compassv1.StartAgentSessionRequest{
		ContainerName: fakeContainer,
	}))
	if err == nil {
		t.Fatal("StartAgentSession on an unplaced container = success, want an error")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("error code = %v, want Internal", got)
	}
	if !strings.Contains(err.Error(), fakeSessionID) {
		t.Fatalf("error %q does not name the started session %q; an operator could not reap it", err, fakeSessionID)
	}

	// The Runner was told to stop the session it just started — the invariant.
	if !f.runner.sawStop(fakeSessionID) {
		t.Fatalf("runner never received a Stop for session %q; the started session is stranded (commands seen: %v)",
			fakeSessionID, f.runner.commands())
	}
	// And nothing was recorded, so the store agrees the session does not exist:
	// either it exists AND is recorded, or it does not exist.
	if got := sessionRowCount(t, ctx, f.dsn, fakeSessionID); got != 0 {
		t.Fatalf("agent_sessions has %d rows for the rolled-back session %q, want 0", got, fakeSessionID)
	}
}

// TestStartAgentSessionRollbackStopIsBounded pins that the anti-stranding Stop
// cannot itself hang StartAgentSession: against a Runner that ACCEPTS the Stop
// but never answers its result (the wedged-but-connected shape), the handler
// must still return within rollbackStopTimeout rather than blocking forever on
// a dispatch path that has no deadline of its own. Without the bound this test
// hangs until the go-test binary's own timeout kills it; with it the handler
// returns the same Internal error naming the session promptly. The timeout is
// shortened here so the test is fast and deterministic — no real long sleep.
func TestStartAgentSessionRollbackStopIsBounded(t *testing.T) {
	prev := rollbackStopTimeout
	rollbackStopTimeout = 200 * time.Millisecond
	t.Cleanup(func() { rollbackStopTimeout = prev })

	f := newPlacementFixtureWith(t, true) // Runner withholds the Stop result
	ctx := context.Background()           // the test root context

	// A generous ceiling relative to the 200ms bound: comfortably above it, far
	// below any real 30s hang, so a regression to unbounded reddens as a
	// timeout here rather than stalling the whole suite.
	done := make(chan error, 1)
	go func() {
		_, err := f.client.StartAgentSession(ctx, connect.NewRequest(&compassv1.StartAgentSessionRequest{
			ContainerName: fakeContainer,
		}))
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("StartAgentSession on an unplaced container = success, want an error")
		}
		if got := connect.CodeOf(err); got != connect.CodeInternal {
			t.Fatalf("error code = %v, want Internal", got)
		}
		if !strings.Contains(err.Error(), fakeSessionID) {
			t.Fatalf("error %q does not name the started session %q; an operator could not reap it", err, fakeSessionID)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("StartAgentSession did not return within 10s against a Runner that never answers Stop; the rollback Stop is unbounded")
	}

	// The Stop was still pushed — the invariant holds; it is merely bounded.
	if !f.runner.sawStop(fakeSessionID) {
		t.Fatalf("runner never received a Stop for session %q (commands seen: %v)", fakeSessionID, f.runner.commands())
	}
}

// TestProvisionAgentWorkspaceRecordsPlacementNamingServingRunner pins that the
// placement Provision writes names the Runner that ACTUALLY served the relay
// (the id the hub returns from the round trip), not a re-read of the registry —
// a placement pointing at the wrong Runner would make reattach re-drive the
// wrong set. Asserted through the reattach read itself, runner_id's only
// consumer, then carried one step further: the provisioned container is now
// Startable, because Provision's write is precisely what Start's read needs.
func TestProvisionAgentWorkspaceRecordsPlacementNamingServingRunner(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background() // the test root context

	resp, err := f.client.ProvisionAgentWorkspace(ctx, connect.NewRequest(&compassv1.ProvisionAgentWorkspaceRequest{
		AgentAccountId:  string(f.agentID),
		ClientRequestId: "prov-1",
	}))
	if err != nil {
		t.Fatalf("ProvisionAgentWorkspace = %v, want success", err)
	}
	if got := resp.Msg.GetContainerName(); got != fakeContainer {
		t.Fatalf("container name = %q, want %q (the Runner's answer)", got, fakeContainer)
	}

	placements, err := f.store.ListAgentPlacementsForRunner(ctx, fakeRunnerID)
	if err != nil {
		t.Fatalf("ListAgentPlacementsForRunner(%q): %v", fakeRunnerID, err)
	}
	if len(placements) != 1 {
		t.Fatalf("runner %q has %d placements after one provision, want 1", fakeRunnerID, len(placements))
	}
	got := placements[0]
	if got.AgentAccountID != f.agentID {
		t.Fatalf("placement agent = %q, want %q", got.AgentAccountID, f.agentID)
	}
	if got.RunnerID != fakeRunnerID {
		t.Fatalf("placement runner = %q, want %q (the Runner that served the relay)", got.RunnerID, fakeRunnerID)
	}
	if got.ContainerName != fakeContainer {
		t.Fatalf("placement container = %q, want %q", got.ContainerName, fakeContainer)
	}

	startResp, err := f.client.StartAgentSession(ctx, connect.NewRequest(&compassv1.StartAgentSessionRequest{
		ContainerName: fakeContainer,
	}))
	if err != nil {
		t.Fatalf("StartAgentSession after Provision = %v, want success", err)
	}
	if owner := sessionOwner(t, ctx, f.dsn, startResp.Msg.GetSessionId()); owner != string(f.agentID) {
		t.Fatalf("session owner = %q, want %q", owner, f.agentID)
	}
}

// TestProvisionAgentWorkspaceOverwritesPersonaFromStore pins the
// server-authoritative persona invariant: on the provision path the Server
// populates the outgoing persona from the store's AgentAccount.persona and
// overwrites any client-supplied value, so a caller cannot inject a system
// prompt (proto compass.proto persona=6). The client sends a bogus persona; the
// Runner must receive the store's value instead. Under the pre-fix handler the
// client value passes straight through and the Runner sees "CLIENT-INJECTED-EVIL".
func TestProvisionAgentWorkspaceOverwritesPersonaFromStore(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background() // the test root context

	// The fixture's default agent has an empty persona, so seed a second agent
	// under the same owner with a real persona to prove the read-through.
	seed, err := f.store.GetAccount(ctx, f.agentID)
	if err != nil {
		t.Fatalf("GetAccount(%q): %v", f.agentID, err)
	}
	const wantPersona = "You are Atlas, a meticulous senior engineer."
	personaAgent, err := f.store.CreateAgent(ctx, seed.Agent.OwnerUserID, store.NewAgent{
		Handle:      "withpersona",
		DisplayName: "P",
		Persona:     wantPersona,
	})
	if err != nil {
		t.Fatalf("CreateAgent(withpersona): %v", err)
	}

	f.runner.forget() // discard the attach probe
	if _, err := f.client.ProvisionAgentWorkspace(ctx, connect.NewRequest(&compassv1.ProvisionAgentWorkspaceRequest{
		AgentAccountId:  string(personaAgent.ID),
		ClientRequestId: "prov-persona",
		Persona:         "CLIENT-INJECTED-EVIL",
	})); err != nil {
		t.Fatalf("ProvisionAgentWorkspace = %v, want success", err)
	}

	if got := f.runner.provisionPersona(t); got != wantPersona {
		t.Fatalf("Runner received persona %q, want %q (server must overwrite the client value)", got, wantPersona)
	}
}

// TestProvisionAgentWorkspaceClearsPersonaForNonAgentAccount pins the non-agent
// branch of the server-authoritative persona invariant: when a user-account id
// is passed as agent_account_id, the store read-through finds a non-agent
// account (acc.IsAgent()==false) and must clear the client-supplied persona to
// empty, so a caller cannot inject a system prompt via a non-agent account.
func TestProvisionAgentWorkspaceClearsPersonaForNonAgentAccount(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background() // the test root context

	// The admin (a user account, not an agent) is the fixture agent's owner.
	seed, err := f.store.GetAccount(ctx, f.agentID)
	if err != nil {
		t.Fatalf("GetAccount(%q): %v", f.agentID, err)
	}
	adminID := seed.Agent.OwnerUserID

	f.runner.forget() // discard the attach probe
	// This call is EXPECTED to error: admin is a user account, absent from
	// agent_accounts, so the placement write fails on its FK (CodeInternal). The
	// persona-clear is still observable because the Provision command is recorded
	// (persona cleared) before the placement write runs. The error is expected
	// and not what this test pins, so it is deliberately discarded.
	_, _ = f.client.ProvisionAgentWorkspace(ctx, connect.NewRequest(&compassv1.ProvisionAgentWorkspaceRequest{
		AgentAccountId:  string(adminID),
		ClientRequestId: "prov-nonagent",
		Persona:         "CLIENT-INJECTED-EVIL",
	}))

	if got := f.runner.provisionPersona(t); got != "" {
		t.Fatalf("Runner received persona %q for a non-agent account, want empty (client value must be cleared)", got)
	}
}

// TestProvisionAgentWorkspaceUnknownAccountIsNotFound pins that the persona
// read-through fails closed: an unknown agent_account_id yields CodeNotFound,
// short-circuiting container creation before any Provision or placement.
func TestProvisionAgentWorkspaceUnknownAccountIsNotFound(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background() // the test root context

	_, err := f.client.ProvisionAgentWorkspace(ctx, connect.NewRequest(&compassv1.ProvisionAgentWorkspaceRequest{
		AgentAccountId:  "acct-does-not-exist",
		ClientRequestId: "prov-unknown",
		Persona:         "whatever",
	}))
	if err == nil {
		t.Fatalf("ProvisionAgentWorkspace = nil error, want CodeNotFound for an unknown account id")
	}
	if code := connect.CodeOf(err); code != connect.CodeNotFound {
		t.Fatalf("ProvisionAgentWorkspace error code = %v, want %v", code, connect.CodeNotFound)
	}
}

// --- direct SQL (the two facts the Store deliberately does not expose) --------

// execSQL runs one statement against the test's isolated schema. Used only to
// seed the backfilled placement, whose empty runner_id RecordAgentPlacement
// refuses by design — the migration is its only production writer.
func execSQL(t *testing.T, ctx context.Context, dsn, sql string, args ...any) {
	t.Helper()
	conn := connectPG(t, ctx, dsn)
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// sessionOwner reads the agent_sessions ownership row for sessionID, failing if
// there is none. The Store exposes only the authz predicate over this table, so
// asserting "the row exists and names this agent" reads it directly.
func sessionOwner(t *testing.T, ctx context.Context, dsn, sessionID string) string {
	t.Helper()
	conn := connectPG(t, ctx, dsn)
	var owner string
	if err := conn.QueryRow(ctx,
		`SELECT agent_account_id FROM agent_sessions WHERE session_id = $1`, sessionID,
	).Scan(&owner); err != nil {
		t.Fatalf("session %q has no recorded owner: %v", sessionID, err)
	}
	return owner
}

// sessionRowCount counts ownership rows for sessionID (0 or 1).
func sessionRowCount(t *testing.T, ctx context.Context, dsn, sessionID string) int {
	t.Helper()
	conn := connectPG(t, ctx, dsn)
	var n int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM agent_sessions WHERE session_id = $1`, sessionID,
	).Scan(&n); err != nil {
		t.Fatalf("count agent_sessions: %v", err)
	}
	return n
}

// connectPG opens a pgx connection on the pgtest DSN (so it lands in this test's
// isolated schema), closed on cleanup.
func connectPG(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) }) // cleanup close; nothing actionable remains
	return conn
}

// --- the fake Runner ---------------------------------------------------------

// attachFakeRunner mounts the real RunnerService door over hub, dials it with a
// fake Runner, runs its Sessions loop, and returns once the router is live.
func attachFakeRunner(t *testing.T, st *store.Store, hub *runnerhub.Hub, withholdStop bool) *recordingRunner {
	t.Helper()
	path, handler := runnerhub.NewMountedHandler(hub,
		func(ctx context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
			return auth.ResolveToken(ctx, st, presented, want)
		}, nil, nil)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)

	// The loop's own context: cancelled at cleanup to end the Sessions stream.
	ctx, cancel := context.WithCancel(context.Background()) // the test root context
	t.Cleanup(cancel)

	tr := h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
	t.Cleanup(tr.CloseIdleConnections)
	rc := compassv1internalconnect.NewRunnerServiceClient(
		&http.Client{Transport: tr}, srv.URL,
		connect.WithInterceptors(runnerBearer(fakeRunnerToken)),
	)
	if _, err := rc.Enroll(ctx, connect.NewRequest(&compassv1internal.EnrollRequest{RunnerId: fakeRunnerID})); err != nil {
		t.Fatalf("Enroll = %v, want success", err)
	}

	rec := &recordingRunner{attached: make(chan struct{}), withholdStop: withholdStop}
	stream := rc.Sessions(ctx)
	loopDone := make(chan error, 1)
	go rec.serve(stream, loopDone)
	t.Cleanup(func() {
		// Close the request half: the loop's Receive then sees a clean EOF and
		// the server's Sessions handler detaches the router — the seam test's
		// proven teardown. (Cancelling ctx alone does not unblock a client-side
		// Receive already parked on the HTTP/2 body.)
		if err := stream.CloseRequest(); err != nil {
			t.Errorf("CloseRequest = %v", err)
		}
		if err := <-loopDone; err != nil {
			t.Errorf("fake runner sessions loop ended with %v, want a clean EOF", err)
		}
		cancel()
	})

	// Gate on the SERVER-side router being live, not merely the client having
	// sent. connect-go initiates the request on the loop's bootstrap Send, but
	// the handler's router.attach runs asynchronously once that reaches the
	// server; a command dispatched into that window gets a retriable Unavailable
	// ("no live runner sessions stream"). Probing with a real round trip — a
	// read-only Status the fake answers — is the observed attach, the same gate
	// shape runnerhub's integration test uses. Yield between probes; the
	// deadline turns a genuinely wedged seam into a fast failure, never a sleep.
	select {
	case <-rec.attached:
	case <-timeAfter():
		t.Fatal("fake runner never opened its Sessions stream")
	}
	deadline := timeAfter()
	for {
		_, err := hub.Status(ctx, "attach-probe", &compassv1.GetAgentStatusRequest{SessionId: "attach-probe"})
		if err == nil {
			break
		}
		if connect.CodeOf(err) != connect.CodeUnavailable {
			t.Fatalf("attach probe = %v, want success or a transient Unavailable", err)
		}
		select {
		case <-deadline:
			t.Fatalf("runner command router never attached a live Sessions stream: %v", err)
		default:
		}
		runtime.Gosched()
	}
	// The probe is bookkeeping, not a command under test.
	rec.forget()
	return rec
}

// recordingRunner is a minimal Runner-side Sessions loop that records every
// command the Server pushes and answers the three this suite drives. Recording
// on the wire is what makes "the handler stopped the session" an observed fact
// rather than a mock expectation.
type recordingRunner struct {
	// attached closes once the bootstrap Send has flushed the stream open.
	attached chan struct{}

	// withholdStop makes the loop record a Stop but never answer its result —
	// the wedged-but-connected Runner that hangs an unbounded rollback Stop.
	withholdStop bool

	// failStart, when set, makes the loop answer every Start with a RunnerError
	// (ALREADY_RUNNING) instead of a session id — the mid-chain failure the
	// spawn-rollback test drives (SEA-1618 T5). Set under mu before the command
	// is driven, read under mu in serve, so -race sees a clean handoff.
	failStart bool

	mu   sync.Mutex
	seen []*compassv1internal.SessionsResponse
	// startIDs, when non-empty, overrides the fixed answer() Start session id one
	// per Start (FIFO) — so a test driving several Starts (e.g. two resumes of one
	// logical session) gets distinct live ids. Empty falls back to answer()'s
	// fixed fakeSessionID. Read/popped under mu.
	startIDs []string
}

// serve runs the dispatch loop. Like the seam test's loop it opens with one
// bootstrap Send, because connect-go does not initiate the request (and so the
// server's router.attach never runs) until the client's first Send; the empty
// result carries no request id, so router.complete treats it as an unknown-id
// no-op. It ends on the clean EOF the caller's CloseRequest produces.
func (r *recordingRunner) serve(
	stream *connect.BidiStreamForClient[compassv1internal.SessionsRequest, compassv1internal.SessionsResponse],
	done chan<- error,
) {
	if err := stream.Send(&compassv1internal.SessionsRequest{}); err != nil {
		done <- err
		return
	}
	close(r.attached)
	for {
		cmd, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				done <- nil
				return
			}
			done <- err
			return
		}
		r.record(cmd)
		if r.withholdStop && cmd.GetStop() != nil {
			continue // record it, but never answer: the wedged-Runner shape
		}
		if cmd.GetStart() != nil && r.failStarted() {
			// Answer Start with a RunnerError instead of a session id: the
			// mid-chain spawn failure the rollback test drives.
			if err := stream.Send(&compassv1internal.SessionsRequest{
				RequestId: cmd.GetRequestId(),
				Result: &compassv1internal.SessionsRequest_Error{Error: &compassv1internal.RunnerError{
					Code:    compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_ALREADY_RUNNING,
					Message: "start refused (test)",
				}},
			}); err != nil {
				done <- err
				return
			}
			continue
		}
		if cmd.GetStart() != nil {
			if id, ok := r.nextStartID(); ok {
				if err := stream.Send(&compassv1internal.SessionsRequest{
					RequestId: cmd.GetRequestId(),
					Result:    &compassv1internal.SessionsRequest_Start{Start: &compassv1.StartAgentSessionResponse{SessionId: id}},
				}); err != nil {
					done <- err
					return
				}
				continue
			}
		}
		if err := stream.Send(answer(cmd)); err != nil {
			done <- err
			return
		}
	}
}

func (r *recordingRunner) record(cmd *compassv1internal.SessionsResponse) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, cmd)
}

// forget drops every recorded command — used once, to discard the attach probe
// so the assertions see only the commands their handler drove.
func (r *recordingRunner) forget() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = nil
}

// commands summarizes every command seen, for failure output.
func (r *recordingRunner) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.seen))
	for _, c := range r.seen {
		switch v := c.GetCommand().(type) {
		case *compassv1internal.SessionsResponse_Provision:
			out = append(out, "provision "+v.Provision.GetAgentAccountId())
		case *compassv1internal.SessionsResponse_Start:
			out = append(out, "start "+v.Start.GetContainerName())
		case *compassv1internal.SessionsResponse_Stop:
			out = append(out, "stop "+v.Stop.GetSessionId())
		default:
			out = append(out, "other")
		}
	}
	return out
}

// sawStop reports whether the Server pushed a Stop for sessionID.
func (r *recordingRunner) sawStop(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.seen {
		if stop := c.GetStop(); stop != nil && stop.GetSessionId() == sessionID {
			return true
		}
	}
	return false
}

// failStarted reports whether the loop should refuse Start (read under mu in
// serve, so -race sees a clean handoff from setFailStart).
func (r *recordingRunner) failStarted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failStart
}

// setFailStart flips the Start-refusal mode. Set before the command it should
// affect is driven.
func (r *recordingRunner) setFailStart(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failStart = v
}

// nextStartID pops the next overriding Start session id (FIFO), returning
// ok=false once the queue is empty (the loop then falls back to answer()).
func (r *recordingRunner) nextStartID() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.startIDs) == 0 {
		return "", false
	}
	id := r.startIDs[0]
	r.startIDs = r.startIDs[1:]
	return id, true
}

// setStartIDs queues the session ids the loop answers successive Starts with.
// Set before the Starts it should affect are driven.
func (r *recordingRunner) setStartIDs(ids ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startIDs = append([]string(nil), ids...)
}

// sawRemove reports whether the Server pushed a Remove for containerName — the
// despawn/rollback teardown observed on the wire.
func (r *recordingRunner) sawRemove(containerName string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.seen {
		if rm := c.GetRemove(); rm != nil && rm.GetContainerName() == containerName {
			return true
		}
	}
	return false
}

// answer builds the correlated result for one command: a fixed container name
// for Provision, a fixed session id for Start, success for Stop, and an explicit
// error for anything this suite does not drive, so an unexpected command is a
// loud failure rather than a hang.
func answer(cmd *compassv1internal.SessionsResponse) *compassv1internal.SessionsRequest {
	out := &compassv1internal.SessionsRequest{RequestId: cmd.GetRequestId()}
	switch cmd.GetCommand().(type) {
	case *compassv1internal.SessionsResponse_Provision:
		out.Result = &compassv1internal.SessionsRequest_Provision{
			Provision: &compassv1.ProvisionAgentWorkspaceResponse{ContainerName: fakeContainer},
		}
	case *compassv1internal.SessionsResponse_Start:
		out.Result = &compassv1internal.SessionsRequest_Start{
			Start: &compassv1.StartAgentSessionResponse{SessionId: fakeSessionID},
		}
	case *compassv1internal.SessionsResponse_Status:
		out.Result = &compassv1internal.SessionsRequest_Status{Status: &compassv1.GetAgentStatusResponse{}}
	case *compassv1internal.SessionsResponse_Stop:
		out.Result = &compassv1internal.SessionsRequest_Stop{Stop: &compassv1.StopAgentSessionResponse{}}
	case *compassv1internal.SessionsResponse_Remove:
		out.Result = &compassv1internal.SessionsRequest_Remove{Remove: &compassv1.RemoveAgentWorkspaceResponse{}}
	default:
		out.Result = &compassv1internal.SessionsRequest_Error{Error: &compassv1internal.RunnerError{
			Code:    compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_INTERNAL,
			Message: "unexpected command",
		}}
	}
	return out
}

// provisionPersona returns the persona on the recorded Provision command — the
// value the Runner actually received on the wire. Fails if no Provision was
// seen, so a silent miss cannot masquerade as an empty persona.
func (r *recordingRunner) provisionPersona(t *testing.T) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.seen {
		switch v := c.GetCommand().(type) {
		case *compassv1internal.SessionsResponse_Provision:
			return v.Provision.GetPersona()
		}
	}
	t.Fatalf("no Provision command recorded, saw %v", r.commands())
	return ""
}

// runnerBearer stamps the Runner's bearer on every outbound RPC (unary +
// streaming), mirroring the Runner-side interceptor so the door authenticates.
type runnerBearer string

func (b runnerBearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+string(b))
		return next(ctx, req)
	}
}

func (b runnerBearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+string(b))
		return conn
	}
}

func (b runnerBearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
