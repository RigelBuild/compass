//go:build pgtest && unix

package runnerhub

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// A Runner binds a tenant-B resume through the handler: the door carries no
// tenant, so the hub resolves it from the container's account. Every refused
// bind (foreign container, account mismatch, unknown session) is the same
// PermissionDenied and leaves the base untouched.
func TestBindLifetimeHandlerTenantScopedAndFailsClosed(t *testing.T) {
	ctx := context.Background() // test root
	st, agent, ctxB := openTenantBSession(t, ctx)
	const sess = "sess-b-resume"
	if err := st.RecordAgentSession(ctxB, sess, agent.ID); err != nil {
		t.Fatalf("RecordAgentSession: %v", err)
	}
	for seq, key := range []string{"k1", "k2"} {
		if err := st.AppendTranscriptEntry(ctxB, sess, uint64(seq+1), false, `{"e":1}`, key); err != nil {
			t.Fatalf("AppendTranscriptEntry: %v", err)
		}
	}
	owner, err := st.CreateUser(ctxB, store.NewUser{Handle: "owner-c", DisplayName: "owner-c"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	other, err := st.CreateAgent(ctxB, owner.ID, store.NewAgent{Handle: "agent-c", DisplayName: "agent-c"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	hub := newHubOnly()
	hub.SetLifetimeBinder(st)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("cont-b", agent.ID, "runner-1")
	hub.bindContainer("cont-other", other.ID, "runner-1")
	url := newMountedH2CServer(t, hub, runnerResolverForFetch().resolve)
	own := newRawRunnerClient(t, url, "runner-tok")
	foreign := newRawRunnerClient(t, url, "foreign-runner-tok")

	bind := func(client interface {
		BindLifetime(ctx context.Context, req *connect.Request[compassv1internal.BindLifetimeRequest]) (*connect.Response[compassv1internal.BindLifetimeResponse], error)
	}, container, session string,
	) error {
		_, err := client.BindLifetime(ctx, connect.NewRequest(&compassv1internal.BindLifetimeRequest{ContainerName: container, SessionId: session}))
		return err
	}

	denials := make([]string, 0, 3)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"foreign container", bind(foreign, "cont-b", sess)},
		{"account mismatch", bind(own, "cont-other", sess)},
		{"unknown session", bind(own, "cont-b", "sess-never")},
	} {
		if got := connect.CodeOf(tc.err); got != connect.CodePermissionDenied {
			t.Fatalf("%s: code = %v (%v), want PermissionDenied", tc.name, got, tc.err)
		}
		denials = append(denials, tc.err.Error())
	}
	for _, d := range denials[1:] {
		if d != denials[0] {
			t.Fatalf("denials differ: %q vs %q; an unknown session must match a foreign one byte for byte", denials[0], d)
		}
	}
	if base := baseFor(t, st, ctxB, sess); base != 0 {
		t.Fatalf("base after refused binds = %d, want 0 (untouched)", base)
	}

	if err := bind(own, "cont-b", sess); err != nil {
		t.Fatalf("own bind = %v, want success", err)
	}
	if base := baseFor(t, st, ctxB, sess); base != 2 {
		t.Fatalf("base after bind = %d, want 2 (the stored max)", base)
	}
}

// baseFor reads the session's bound base under its tenant.
func baseFor(t *testing.T, st *store.Store, ctx context.Context, sessionID string) int64 {
	t.Helper()
	var base int64
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT base_entry_seq FROM agent_sessions WHERE session_id = $1", sessionID).Scan(&base)
	}); err != nil {
		t.Fatalf("read base: %v", err)
	}
	return base
}
