//go:build pgtest && unix

package auth

// Store-backed BearerInterceptor contract tests (SEA-1195 T3, the S3 gate),
// transcribed from the authoritative Rust suite in
// crates/compass-daemon/src/auth.rs (#[cfg(test)] mod tests, the bearer_auth
// cases). BearerInterceptor now resolves a presented token against the Postgres
// store of record, so these need a live database and live in the `pgtest` lane;
// the shared spy/assert helpers (recordingSpy, bearerRequest, runInterceptor,
// wantUnauthenticated, spyResult) come from the default-lane interceptor_test.go.
//
// The observable contract: on accept the wrapped handler runs and CallerFrom(ctx)
// is the token's account; on reject the handler never runs and
// connect.CodeOf(err) == CodeUnauthenticated. The reject matrix covers every
// failure class the shared resolver surfaces (missing / non-bearer / unknown /
// revoked / cross-kind), and the door collapses all of them to one
// CodeUnauthenticated — the oracle-safety the server package pins on the wire.
//
// withCaller ALSO sets the comms actor (comms.WithActor), so a bearer-
// authenticated comms RPC is attributed to the real token holder rather than the
// bootstrap-admin fallback. comms exposes no reader, so that half is not
// observable from the auth package; it is pinned end-to-end in the server package
// (comms_actor_pgtest_test.go's TestNetworkDoorCommsActorIsBearerCallerNotAdmin:
// a bearer caller's CreateChannelGroup over the real network door is owned by the
// caller, not the admin).

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/comms"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// bearer_auth_injects_authed_account_for_a_valid_token: a valid Bearer token is
// accepted and the wrapped handler sees the token's account as the caller.
func TestBearerInterceptorInjectsCallerForAValidToken(t *testing.T) {
	ctx := context.Background()
	st, acct, _ := openTestStore(t)
	token, err := IssueAccountToken(ctx, st, acct)
	if err != nil {
		t.Fatalf("IssueAccountToken: %v", err)
	}

	rec, err := runInterceptor(BearerInterceptor(st), bearerRequest("Bearer "+token))
	if err != nil {
		t.Fatalf("a valid bearer token must be accepted: %v", err)
	}
	if !rec.called {
		t.Fatal("the handler must run on an accepted request")
	}
	if !rec.hasCaller || rec.caller != acct {
		t.Fatalf("the injected caller is the token's account: got %v hasCaller=%v, want %v", rec.caller, rec.hasCaller, acct)
	}
}

// TestBearerInterceptorSetsCommsActorNotAdminFallback pins the load-bearing
// second half of withCaller: it sets not only auth.CallerFrom (the AdminGate's
// reader) but ALSO comms.WithActor (the comms handlers' reader). The two readers
// are independent context keys, so setting only the first would leave the comms
// handlers falling back to the bootstrap-admin identity (comms attributes the
// admin when no actor is set) — silently attributing every network caller's comms
// writes to the admin. Observed end-to-end through a real comms handler: a member
// (non-admin) bearer caller creates a channel group, and the store-recorded owner
// MUST be the member, not the admin the comms service was constructed with. If
// withCaller dropped the comms.WithActor call, the group would be owned by the
// admin and this reddens. comms exposes no context reader, so this behavioral
// assertion is the only way to prove the comms half fired.
func TestBearerInterceptorSetsCommsActorNotAdminFallback(t *testing.T) {
	ctx := context.Background()
	st, admin, member := openTestStore(t)
	memberToken, err := IssueAccountToken(ctx, st, member)
	if err != nil {
		t.Fatalf("IssueAccountToken(member): %v", err)
	}

	// The comms service is constructed with the ADMIN as its ambient-fallback
	// identity — the same wiring the socket door uses. So a comms write with no
	// actor on the context is attributed to the admin; only a set comms actor
	// diverts attribution to the real caller.
	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(commsBus.Close)
	commsSvc := comms.NewComms(st, commsBus, admin)

	// Drive CreateChannelGroup through the bearer interceptor: the interceptor
	// resolves the member token and threads the caller into the ctx it hands the
	// handler. The handler (comms) reads the comms actor off that ctx.
	var ownerID string
	next := func(hctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		resp, err := commsSvc.CreateChannelGroup(hctx, connect.NewRequest(&compassv1.CreateChannelGroupRequest{Name: "caller-space"}))
		if err != nil {
			return nil, err
		}
		ownerID = resp.Msg.GetGroup().GetOwnerUserId()
		return resp, nil
	}
	wrapped := BearerInterceptor(st)(next)
	if _, err := wrapped(ctx, bearerRequest("Bearer "+memberToken)); err != nil {
		t.Fatalf("member bearer on CreateChannelGroup: %v", err)
	}

	if ownerID != string(member) {
		t.Fatalf("channel group owner = %q, want the member caller %q (withCaller must set comms.WithActor, not leave the admin fallback %q)", ownerID, member, admin)
	}
	if ownerID == string(admin) {
		t.Fatal("channel group owned by the admin fallback — the bearer interceptor did not set the comms actor")
	}
}

// bearer_auth_accepts_the_scheme_case_insensitively: RFC 7235 §2.1 — the scheme
// is case-insensitive, so bearer/BEARER/BeArEr all resolve; the token stays
// verbatim. Also covers the multi-space delimiter (1*SP allowed), so several
// spaces still resolve the token rather than folding into a space-prefixed value.
func TestBearerInterceptorAcceptsSchemeAndSpacingVariants(t *testing.T) {
	ctx := context.Background()
	st, acct, _ := openTestStore(t)
	token, err := IssueAccountToken(ctx, st, acct)
	if err != nil {
		t.Fatalf("IssueAccountToken: %v", err)
	}

	for _, header := range []string{
		"Bearer " + token,
		"bearer " + token,
		"BEARER " + token,
		"BeArEr " + token,
		"Bearer   " + token, // multiple spaces after the scheme
	} {
		t.Run(header[:len(header)-len(token)], func(t *testing.T) {
			rec, err := runInterceptor(BearerInterceptor(st), bearerRequest(header))
			if err != nil {
				t.Fatalf("header %q must be accepted: %v", header, err)
			}
			if !rec.hasCaller || rec.caller != acct {
				t.Fatalf("header %q must resolve to the token's account: got %v hasCaller=%v, want %v", header, rec.caller, rec.hasCaller, acct)
			}
		})
	}
}

// TestBearerInterceptorRejectsBadCredentials is the unary door's rejection
// matrix: a missing header, a non-bearer scheme, a schemeless bare token, an
// interior-space token, a well-formed-but-unknown token, a revoked token, and a
// cross-kind (Runner) token each reject with CodeUnauthenticated and never run
// the handler. Table-driven so a flipped rejection path reddens exactly the named
// row. The unknown/revoked/cross-kind rows exercise the store-backed resolver's
// three sentinels, all of which the door maps to one indistinguishable code.
func TestBearerInterceptorRejectsBadCredentials(t *testing.T) {
	ctx := context.Background()
	st, acct, _ := openTestStore(t)

	// A well-formed base64url token that is never issued, so its row is a store
	// miss (ErrTokenNotFound), not a parse failure.
	const unissued = "aW52YWxpZC10b2tlbg"

	// A revoked account token: issued, then its hash revoked.
	revokedToken, err := IssueAccountToken(ctx, st, acct)
	if err != nil {
		t.Fatalf("IssueAccountToken(revoked): %v", err)
	}
	if err := st.RevokeToken(ctx, hashToken(revokedToken)); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	// A cross-kind Runner token: resolves in the store but to SubjectRunner, so
	// the account door's want=SubjectAccount fails the kind gate (ErrWrongKind).
	const runnerToken = "cnVubmVyLXRva2Vu"
	if err := st.PutTokenHash(ctx, hashToken(runnerToken), store.Subject{Kind: store.SubjectRunner, ID: "some-runner"}); err != nil {
		t.Fatalf("PutTokenHash(runner): %v", err)
	}

	cases := []struct {
		name   string
		header string
	}{
		{"missing header", ""},
		{"non-bearer scheme", "Basic dXNlcjpwYXNz"},
		{"schemeless bare token", unissued},
		{"interior space in token", "Bearer abc def"},
		{"unknown token", "Bearer " + unissued},
		{"revoked token", "Bearer " + revokedToken},
		{"cross-kind runner token", "Bearer " + runnerToken},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := runInterceptor(BearerInterceptor(st), bearerRequest(tc.header))
			wantUnauthenticated(t, rec, err, "bearer door: "+tc.name)
		})
	}
}
