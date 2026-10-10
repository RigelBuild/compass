//go:build pgtest && unix

package server

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

type skipBatchWindowFixture struct {
	client            compassv1connect.CompassServiceClient
	runner            *recordingRunner
	ownerToken        string
	adminToken        string
	memberToken       string
	otherToken        string
	foreignAdminToken string
	sessionID         string
}

func newSkipBatchWindowFixture(t *testing.T, liveRunner bool) skipBatchWindowFixture {
	t.Helper()
	ctx := context.Background()
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
	owner, err := st.CreateUser(ctx, store.NewUser{Handle: "owner", DisplayName: "owner"})
	if err != nil {
		t.Fatalf("CreateUser(owner): %v", err)
	}
	member, err := st.CreateUser(ctx, store.NewUser{Handle: "member", DisplayName: "member"})
	if err != nil {
		t.Fatalf("CreateUser(member): %v", err)
	}
	otherOwner, err := st.CreateUser(ctx, store.NewUser{Handle: "other-owner", DisplayName: "other-owner"})
	if err != nil {
		t.Fatalf("CreateUser(other-owner): %v", err)
	}
	otherTenant := store.TenantID("skip-batch-window-other")
	tenantPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(tenantPool.Close)
	if _, err := tenantPool.Exec(ctx, "INSERT INTO tenants (id, slug, display_name, created_at_unix_ms) VALUES ($1, $2, $3, $4)", string(otherTenant), string(otherTenant), string(otherTenant), time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed other tenant: %v", err)
	}
	foreignAdmin, err := st.BootstrapAdmin(store.WithTenant(ctx, otherTenant), store.NewUser{Handle: "foreign-admin", DisplayName: "Foreign admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin(other tenant): %v", err)
	}
	agent, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "agent", DisplayName: "agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	otherAgent, err := st.CreateAgent(ctx, otherOwner.ID, store.NewAgent{Handle: "other-agent", DisplayName: "other-agent"})
	if err != nil {
		t.Fatalf("CreateAgent(other-agent): %v", err)
	}
	const sessionID = "owner-session"
	const foreignID = "other-owner-session"
	for session, account := range map[string]store.AccountID{sessionID: agent.ID, foreignID: otherAgent.ID} {
		if err := st.RecordAgentSession(ctx, session, account); err != nil {
			t.Fatalf("RecordAgentSession(%q): %v", session, err)
		}
	}
	if err := st.PutTokenHash(ctx, sha256.Sum256([]byte(fakeRunnerToken)),
		store.Subject{Kind: store.SubjectRunner, ID: fakeRunnerID}); err != nil {
		t.Fatalf("PutTokenHash(runner): %v", err)
	}
	if _, _, err := st.UpdateChannelMembers(ctx, owner.ID, agent.Agent.HomeChannelID,
		[]store.MemberUpdate{{AccountID: member.ID}}, store.MemberUpdatesOptions{}); err != nil {
		t.Fatalf("add home-channel member: %v", err)
	}

	issueToken := func(account store.AccountID) string {
		t.Helper()
		token, err := auth.IssueAccountToken(ctx, st, account)
		if err != nil {
			t.Fatalf("IssueAccountToken(%q): %v", account, err)
		}
		return token
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
	var runner *recordingRunner
	if liveRunner {
		runner = attachFakeRunner(t, st, hub, false)
	}

	return skipBatchWindowFixture{
		client:            newH2CClient(t, url),
		runner:            runner,
		ownerToken:        issueToken(owner.ID),
		adminToken:        issueToken(admin.ID),
		memberToken:       issueToken(member.ID),
		otherToken:        issueToken(otherOwner.ID),
		foreignAdminToken: issueToken(foreignAdmin.ID),
		sessionID:         sessionID,
	}
}

func skipBatchWindow(t *testing.T, f skipBatchWindowFixture, token, sessionID string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	req := connect.NewRequest(&compassv1.SkipBatchWindowRequest{SessionId: sessionID})
	req.Header().Set("Authorization", "Bearer "+token)
	_, err := f.client.SkipBatchWindow(ctx, req)
	return err
}

func assertSingleStartNow(t *testing.T, runner *recordingRunner) {
	t.Helper()
	deadline := timeAfter()
	for {
		controls := runner.controls()
		if len(controls) == 1 {
			if controls[0].GetStartNow() == nil {
				t.Fatalf("Runner control = %T, want start_now", controls[0].GetControl())
			}
			return
		}
		if len(controls) > 1 {
			t.Fatalf("Runner received %d controls, want exactly one (commands: %v)", len(controls), runner.commands())
		}
		select {
		case <-deadline:
			t.Fatalf("Runner received no start_now control (commands: %v)", runner.commands())
		default:
			runtime.Gosched()
		}
	}
}
func TestSkipBatchWindowAuthorizesOwnerAndAdminAndDispatchesStartNow(t *testing.T) {
	f := newSkipBatchWindowFixture(t, true)
	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "owner", token: f.ownerToken},
		{name: "admin", token: f.adminToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.runner.forget()
			if err := skipBatchWindow(t, f, tc.token, f.sessionID); err != nil {
				t.Fatalf("SkipBatchWindow as %s: %v", tc.name, err)
			}
			assertSingleStartNow(t, f.runner)
		})
	}
}

func TestSkipBatchWindowNotFoundParityDoesNotDispatch(t *testing.T) {
	f := newSkipBatchWindowFixture(t, true)
	for _, tc := range []struct {
		name      string
		token     string
		sessionID string
	}{
		{name: "non-owner home-channel member", token: f.memberToken, sessionID: f.sessionID},
		{name: "admin from another tenant", token: f.foreignAdminToken, sessionID: f.sessionID},
		{name: "owner of another agent", token: f.otherToken, sessionID: f.sessionID},
		{name: "unknown session", token: f.ownerToken, sessionID: "unknown-session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.runner.forget()
			err := skipBatchWindow(t, f, tc.token, tc.sessionID)
			if got := connect.CodeOf(err); got != connect.CodeNotFound {
				t.Fatalf("SkipBatchWindow error = %v, want NotFound", got)
			}
			ce, ok := err.(*connect.Error)
			if !ok {
				t.Fatalf("SkipBatchWindow error = %v, want connect.Error", err)
			}
			if got, want := ce.Message(), "agent session \""+tc.sessionID+"\""; got != want {
				t.Fatalf("SkipBatchWindow message = %q, want %q", got, want)
			}
			if controls := f.runner.controls(); len(controls) != 0 {
				t.Fatalf("refused SkipBatchWindow dispatched %d controls, want none", len(controls))
			}
		})
	}
}

func TestSkipBatchWindowDispatchFailureIsUnavailable(t *testing.T) {
	f := newSkipBatchWindowFixture(t, false)
	err := skipBatchWindow(t, f, f.ownerToken, f.sessionID)
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("SkipBatchWindow dispatch error = %v, want Unavailable", got)
	}
}
