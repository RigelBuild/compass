//go:build pgtest && unix

package server

// T6 (SEA-1667): the resume branch of StartAgentSession. When
// resume_session_id is non-empty the handler (1) gates the caller via
// RequireAgentSessionSubscriber BEFORE any Runner call — an unknown or foreign
// id is NotFound and no Start is ever pushed; (2) BindLifetime write-once to
// snapshot the entry_seq rebase base for the new lifetime; (3) calls T5's
// ReconstructSessionBody; (4) carries the reconstructed body to the Runner on
// the INTERNAL SessionsResponse.resume_body envelope, never the public request.
// A fresh start (empty resume_session_id) attaches nothing and does not bind.
//
// Driven through the production network-door interceptor chain (bearer +
// admin-gate) over a real connect client so the handler reads a genuine caller
// identity the same way the shipped door supplies it, against a real Postgres
// (authz + transcript + bind) and a real Runner door (a fake Runner that records
// every relayed command, so "the body rode the internal envelope" and "no Start
// was pushed" are observed wire facts). Behind `pgtest && unix`.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
	"github.com/sealedsecurity/compass/go/internal/auth"
	"github.com/sealedsecurity/compass/go/internal/board"
	"github.com/sealedsecurity/compass/go/internal/pgtest"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// resumeFixture is placementFixture's shape but with the network-door
// interceptor chain mounted (bearer + admin gate), so the resume handler reads a
// real caller identity. The admin is the agent's owner and thus a member of its
// home channel — the authorized resumer. These tests exercise the PG-only normal
// resume path (no safety-valve segments), so the object store is never read and
// is left unwired.
type resumeFixture struct {
	dsn        string
	store      *store.Store
	client     compassv1connect.CompassServiceClient
	runner     *recordingRunner
	agentID    store.AccountID
	adminToken string // the agent owner (admin): authorized to resume
}

func newResumeFixture(t *testing.T) resumeFixture {
	ctx := context.Background() // test root
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

	if err := st.PutTokenHash(ctx, sha256.Sum256([]byte(fakeRunnerToken)),
		store.Subject{Kind: store.SubjectRunner, ID: fakeRunnerID}); err != nil {
		t.Fatalf("PutTokenHash(runner): %v", err)
	}
	adminTok, err := auth.IssueAccountToken(ctx, st, admin.ID)
	if err != nil {
		t.Fatalf("IssueAccountToken(admin): %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	brd := board.NewProjection(bus)
	tail := newSessionTail()
	hub := newRunnerHub(st, brd, tail, nil, slog.New(slog.DiscardHandler))
	svc := newService("test", bus, st, hub, brd, nil, tail)

	url := newH2CTestServerWithInterceptors(t, svc,
		auth.BearerInterceptor(st),
		auth.BearerStreamInterceptor(st),
		auth.NewAdminGate(admin.ID),
	)
	return resumeFixture{
		dsn:        dsn,
		store:      st,
		client:     newH2CClient(t, url),
		runner:     attachFakeRunner(t, st, hub, false),
		agentID:    agent.ID,
		adminToken: adminTok,
	}
}

// startResume drives a StartAgentSession carrying resume_session_id under the
// given bearer, over the real interceptor chain.
func (f resumeFixture) startResume(ctx context.Context, bearer, resumeSessionID string) (*connect.Response[compassv1.StartAgentSessionResponse], error) {
	req := connect.NewRequest(&compassv1.StartAgentSessionRequest{
		ContainerName:   fakeContainer,
		ResumeSessionId: resumeSessionID,
	})
	req.Header().Set("Authorization", "Bearer "+bearer)
	return f.client.StartAgentSession(ctx, req)
}

// relayedStartResumeBody returns the resume_body.session_body on the LAST Start
// command the fake Runner recorded, plus whether a Start was seen at all. The
// internal envelope's carrier is what the handoff attaches; reading it off the
// wire is what makes "the body rode the internal envelope" an observed fact.
func relayedStartResumeBody(t *testing.T, r *recordingRunner) (body string, sawStart bool) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.seen {
		if c.GetStart() != nil {
			sawStart = true
			body = c.GetResumeBody().GetSessionBody()
		}
	}
	return body, sawStart
}

// 1. A resume with an unknown or foreign session id fails NotFound BEFORE any
// Runner call — the authz gate precedes the relay, so the fake Runner records no
// Start. The outsider (member of nothing) resuming a real session and anyone
// resuming an unknown session are the SAME NotFound (the not-found/forbidden
// merge, D9), and in neither case does a container get started.
func TestStartAgentSessionResumeUnknownOrForeignIsNotFoundBeforeRunner(t *testing.T) {
	ctx := context.Background() // test root

	t.Run("foreign session", func(t *testing.T) {
		f := newResumeFixture(t)
		// A session owned by a FOREIGN agent (owned by the outsider user), whose
		// home channel the admin caller is not a member of. The admin passes the
		// network door's admin gate, but the handler's RequireAgentSessionSubscriber
		// still refuses — the resume authz is per-session membership, not door role.
		outsiderUser, err := f.store.CreateUser(ctx, store.NewUser{Handle: "outsider-owner", DisplayName: "outsider owner"})
		if err != nil {
			t.Fatalf("CreateUser(outsider-owner): %v", err)
		}
		foreignAgent, err := f.store.CreateAgent(ctx, outsiderUser.ID, store.NewAgent{Handle: "foreign", DisplayName: "Foreign"})
		if err != nil {
			t.Fatalf("CreateAgent(foreign): %v", err)
		}
		const logical = "sess-logical-foreign"
		if err := f.store.RecordAgentSession(ctx, logical, foreignAgent.ID); err != nil {
			t.Fatalf("RecordAgentSession: %v", err)
		}
		f.runner.forget() // drop the attach probe

		_, err = f.startResume(ctx, f.adminToken, logical)
		if err == nil {
			t.Fatal("admin resume of a foreign agent's session = success, want NotFound")
		}
		if got := connect.CodeOf(err); got != connect.CodeNotFound {
			t.Fatalf("foreign-resume code = %v, want NotFound", got)
		}
		if _, sawStart := relayedStartResumeBody(t, f.runner); sawStart {
			t.Fatalf("a rejected resume pushed a Start to the Runner (commands: %v); the authz gate must precede the relay", f.runner.commands())
		}
	})

	t.Run("unknown session", func(t *testing.T) {
		f := newResumeFixture(t)
		f.runner.forget()

		_, err := f.startResume(ctx, f.adminToken, "sess-does-not-exist")
		if err == nil {
			t.Fatal("resume of an unknown session = success, want NotFound")
		}
		if got := connect.CodeOf(err); got != connect.CodeNotFound {
			t.Fatalf("unknown-resume code = %v, want NotFound", got)
		}
		if _, sawStart := relayedStartResumeBody(t, f.runner); sawStart {
			t.Fatalf("a rejected resume pushed a Start to the Runner (commands: %v)", f.runner.commands())
		}
	})
}

// 2. An authorized resume reconstructs the session body and attaches it to the
// INTERNAL envelope: the owner (a home-channel member) resumes a real session
// that has a stored transcript, and the fake Runner receives a Start whose
// resume_body.session_body is exactly the reconstructed body (checkpoint verbatim
// + later deltas, newline-joined). The public request carried only the
// resume_session_id; the body is server-attached.
func TestStartAgentSessionResumeAttachesReconstructedBody(t *testing.T) {
	ctx := context.Background() // test root
	f := newResumeFixture(t)

	const logical = "sess-logical-ok"
	if err := f.store.RecordAgentSession(ctx, logical, f.agentID); err != nil {
		t.Fatalf("RecordAgentSession: %v", err)
	}
	// The resume mints a fresh live session id (the Runner's answer) whose
	// ownership row the handler records via AgentForContainer -> so the container
	// must be placed.
	if err := f.store.RecordAgentPlacement(ctx, f.agentID, fakeRunnerID, fakeContainer); err != nil {
		t.Fatalf("RecordAgentPlacement: %v", err)
	}
	// Seed a checkpoint + two later deltas — the PG hot-tail normal resume set.
	if err := f.store.AppendTranscriptEntry(ctx, logical, 1, true, `{"header":true}`, "k1"); err != nil {
		t.Fatalf("append checkpoint: %v", err)
	}
	if err := f.store.AppendTranscriptEntry(ctx, logical, 2, false, `{"d":2}`, "k2"); err != nil {
		t.Fatalf("append delta 2: %v", err)
	}
	if err := f.store.AppendTranscriptEntry(ctx, logical, 3, false, `{"d":3}`, "k3"); err != nil {
		t.Fatalf("append delta 3: %v", err)
	}
	f.runner.forget()

	if _, err := f.startResume(ctx, f.adminToken, logical); err != nil {
		t.Fatalf("authorized resume = %v, want success", err)
	}

	body, sawStart := relayedStartResumeBody(t, f.runner)
	if !sawStart {
		t.Fatalf("authorized resume pushed no Start (commands: %v)", f.runner.commands())
	}
	want := "{\"header\":true}\n{\"d\":2}\n{\"d\":3}"
	if body != want {
		t.Fatalf("relayed resume_body = %q, want %q (reconstructed body on the internal envelope)", body, want)
	}
}

// 3. A fresh start (empty resume_session_id) attaches NOTHING: the fake Runner's
// Start command carries an empty resume_body, and no bind is performed. This is
// the existing non-resume contract, pinned here so the resume branch never leaks
// a body onto a fresh start.
func TestStartAgentSessionFreshAttachesNoResumeBody(t *testing.T) {
	ctx := context.Background() // test root
	f := newResumeFixture(t)

	// A fresh start needs a placement so the post-Start ownership read resolves.
	if err := f.store.RecordAgentPlacement(ctx, f.agentID, fakeRunnerID, fakeContainer); err != nil {
		t.Fatalf("RecordAgentPlacement: %v", err)
	}
	f.runner.forget()

	if _, err := f.startResume(ctx, f.adminToken, ""); err != nil {
		t.Fatalf("fresh start = %v, want success", err)
	}
	body, sawStart := relayedStartResumeBody(t, f.runner)
	if !sawStart {
		t.Fatalf("fresh start pushed no Start (commands: %v)", f.runner.commands())
	}
	if body != "" {
		t.Fatalf("fresh start relayed resume_body = %q, want empty (a fresh start attaches nothing)", body)
	}
}

// 4. The stored transcript is keyed on the STABLE LOGICAL id across resumes: two
// resumes of the same logical session both reconstruct from the SAME stored
// transcript (keyed on the logical id, not a per-lifetime id), and each BindLifetime
// re-reads the same stored max as the base (idempotent within a lifetime, monotonic
// across them). Here both resumes reconstruct the same body from the one transcript,
// and the persisted base is the stored max after each bind.
func TestStartAgentSessionResumeKeyedOnStableLogicalIdAcrossResumes(t *testing.T) {
	ctx := context.Background() // test root
	f := newResumeFixture(t)

	const logical = "sess-logical-twice"
	if err := f.store.RecordAgentSession(ctx, logical, f.agentID); err != nil {
		t.Fatalf("RecordAgentSession: %v", err)
	}
	if err := f.store.AppendTranscriptEntry(ctx, logical, 1, true, `{"header":true}`, "k1"); err != nil {
		t.Fatalf("append checkpoint: %v", err)
	}
	if err := f.store.AppendTranscriptEntry(ctx, logical, 2, false, `{"d":2}`, "k2"); err != nil {
		t.Fatalf("append delta 2: %v", err)
	}
	want := "{\"header\":true}\n{\"d\":2}"

	// The container must be placed so each resume's ownership recording resolves,
	// and each of the two resumes mints a DISTINCT live session id (the fresh
	// lifetime), while the stored transcript stays keyed on the stable logical id.
	if err := f.store.RecordAgentPlacement(ctx, f.agentID, fakeRunnerID, fakeContainer); err != nil {
		t.Fatalf("RecordAgentPlacement: %v", err)
	}
	f.runner.setStartIDs("live-resume-1", "live-resume-2")

	for _, attempt := range []string{"first", "second"} {
		f.runner.forget()
		if _, err := f.startResume(ctx, f.adminToken, logical); err != nil {
			t.Fatalf("resume %s = %v, want success", attempt, err)
		}
		body, sawStart := relayedStartResumeBody(t, f.runner)
		if !sawStart {
			t.Fatalf("resume %s pushed no Start (commands: %v)", attempt, f.runner.commands())
		}
		if body != want {
			t.Fatalf("resume %s body = %q, want %q (reconstructed from the stable logical transcript)", attempt, body, want)
		}
		// BindLifetime snapshotted the base as the stored max (2) — write-once per
		// lifetime, re-reads the same max on a re-resume (monotonic across resumes).
		if base := boundBase(t, ctx, f.dsn, logical); base != 2 {
			t.Fatalf("resume %s bound base = %d, want 2 (max entry_seq over the stored transcript)", attempt, base)
		}
	}
}

// boundBase reads agent_sessions.base_entry_seq for a session directly — the
// write-once rebase base BindLifetime snapshots (the store exposes no public
// read).
func boundBase(t *testing.T, ctx context.Context, dsn, sessionID string) uint64 {
	t.Helper()
	conn := connectPG(t, ctx, dsn)
	var base int64
	if err := conn.QueryRow(ctx,
		`SELECT base_entry_seq FROM agent_sessions WHERE session_id = $1`, sessionID,
	).Scan(&base); err != nil {
		t.Fatalf("read base_entry_seq: %v", err)
	}
	return uint64(base)
}

// memObjectStore is a tiny in-memory store.ObjectStore fake used by the S3
// fallback end-to-end test: it holds segment bodies by key under a mutex so the
// real store's safety-valve flush can PUT and the resume reconstructor can GET
// against a genuine Postgres. Defined here (not the store package) so the S3 leg
// runs against the real store + a real object seam, not the runnerhub unit fake.
type memObjectStore struct {
	mu   sync.Mutex
	blob map[string][]byte
}

func newMemObjectStore() *memObjectStore {
	return &memObjectStore{blob: make(map[string][]byte)}
}

func (m *memObjectStore) PutSegment(_ context.Context, key string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(body))
	copy(cp, body)
	m.blob[key] = cp
	return nil
}

func (m *memObjectStore) GetSegment(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	body, ok := m.blob[key]
	if !ok {
		return nil, fmt.Errorf("memObjectStore: no segment for key %q", key)
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	return cp, nil
}

func (m *memObjectStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.blob)
}

// 5. END-TO-END S3 fallback against a REAL Postgres + a real object-store seam:
// wiring a fake object store and lowering the safety-valve cap so appending
// enough post-checkpoint entries trips the valve (evicting an oldest chunk to a
// safety_valve segment in the fake object store), then an authorized resume
// reconstructs the FULL body — checkpoint verbatim then every later delta merged
// by entry_seq, pulling the evicted entries back from the object store. This is
// the only resume path that touches the object store; here it runs end-to-end,
// not through the runnerhub unit fake.
func TestStartAgentSessionResumeS3FallbackReconstructsEvictedEntries(t *testing.T) {
	ctx := context.Background() // test root
	f := newResumeFixture(t)

	obj := newMemObjectStore()
	f.store.SetObjectStore(obj)
	f.store.SetSafetyValveCapBytesForTest(40) // well below the payload sizes below

	const logical = "sess-logical-valve"
	if err := f.store.RecordAgentSession(ctx, logical, f.agentID); err != nil {
		t.Fatalf("RecordAgentSession: %v", err)
	}
	if err := f.store.RecordAgentPlacement(ctx, f.agentID, fakeRunnerID, fakeContainer); err != nil {
		t.Fatalf("RecordAgentPlacement: %v", err)
	}
	// Checkpoint (header-first full body) then several post-checkpoint deltas,
	// each large enough that their combined octet length exceeds the 40-byte cap
	// and the oldest chunk is evicted to a safety_valve segment.
	if err := f.store.AppendTranscriptEntry(ctx, logical, 1, true, `{"header":true}`, "k1"); err != nil {
		t.Fatalf("append checkpoint: %v", err)
	}
	deltas := []string{
		`{"delta":"dddddddddddddddddddd2"}`,
		`{"delta":"dddddddddddddddddddd3"}`,
		`{"delta":"dddddddddddddddddddd4"}`,
		`{"delta":"dddddddddddddddddddd5"}`,
	}
	for i, d := range deltas {
		if err := f.store.AppendTranscriptEntry(ctx, logical, uint64(i+2), false, d, fmt.Sprintf("kd%d", i+2)); err != nil {
			t.Fatalf("append delta %d: %v", i+2, err)
		}
	}

	// The valve must have fired: a safety_valve segment recorded and a body in
	// the object store.
	segs, err := f.store.SafetyValveSegments(ctx, logical)
	if err != nil {
		t.Fatalf("SafetyValveSegments: %v", err)
	}
	if len(segs) == 0 {
		t.Fatal("no safety_valve segment recorded despite the tail exceeding the cap")
	}
	if obj.count() == 0 {
		t.Fatal("safety-valve fired but no segment body written to the object store")
	}

	f.runner.forget()
	if _, err := f.startResume(ctx, f.adminToken, logical); err != nil {
		t.Fatalf("authorized resume with a fired valve = %v, want success", err)
	}

	body, sawStart := relayedStartResumeBody(t, f.runner)
	if !sawStart {
		t.Fatalf("valve resume pushed no Start (commands: %v)", f.runner.commands())
	}
	want := "{\"header\":true}\n" + strings.Join(deltas, "\n")
	if body != want {
		t.Fatalf("relayed resume_body = %q, want %q (evicted entries merged back from the object store in seq order)", body, want)
	}
}
