//go:build pgtest && unix

package auth

// Streaming bearer-door contract tests (SEA-1195 T3, the S3 gate), transcribed
// from the authoritative Rust suite in crates/compass-daemon/src/auth.rs
// (#[cfg(test)] mod tests, the bearer_auth cases). Those cover the unary door;
// the Go door splits authentication across a UnaryInterceptorFunc and a streaming
// Interceptor (streamAuth), so the streaming leg needs its own coverage. This is
// the auth/token door — distinct from the AdminGate streaming authorization tests
// in admin_gate_test.go: here the contract is caller RESOLUTION and rejection at
// the stream door, not method-level admin gating.
//
// BearerStreamInterceptor resolves against the Postgres store of record, so these
// need a live database and live in the `pgtest` lane. The observable contract
// mirrors the unary door: on accept the wrapped streaming handler runs and sees
// CallerFrom(ctx) == the token's account; on reject the handler never runs and
// connect.CodeOf(err) == CodeUnauthenticated.
//
// White-box (package auth) to reuse the door's own resolution and the same-package
// helpers: fakeStreamConn + recordingStreamHandler (admin_gate_test.go),
// wantUnauthenticated + spyResult (interceptor_test.go), and the unexported
// authorizationHeader. A streaming test needs a StreamingHandlerConn stub because
// connect exposes no way to hand one to a unit test; fakeStreamConn's
// RequestHeader() carries the wire Authorization value the door reads.

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/store"
)

// streamConnWithAuth builds a streaming conn carrying value as its Authorization
// request header verbatim (scheme, spacing, and all), or no header when value is
// "". The streaming door ignores the procedure, so it is left empty.
func streamConnWithAuth(value string) *fakeStreamConn {
	conn := &fakeStreamConn{}
	if value != "" {
		conn.header = make(http.Header)
		conn.header.Set(authorizationHeader, value)
	}
	return conn
}

// runStreamInterceptor drives interceptor's streaming-handler leg over a spy
// handler with conn, returning what the handler observed and the door's error.
func runStreamInterceptor(interceptor connect.Interceptor, conn connect.StreamingHandlerConn) (*spyResult, error) {
	rec := &spyResult{}
	wrapped := interceptor.WrapStreamingHandler(recordingStreamHandler(rec))
	err := wrapped(context.Background(), conn)
	return rec, err
}

// TestBearerStreamInterceptorInjectsCallerForAValidToken: a valid issued Bearer
// token is accepted on the streaming door and the wrapped handler sees the
// token's account as the caller (proves the withCaller ctx threading in
// stream.go's WrapStreamingHandler — dropping it leaves the handler with no
// caller).
func TestBearerStreamInterceptorInjectsCallerForAValidToken(t *testing.T) {
	ctx := context.Background()
	st, acct, _ := openTestStore(t)
	token, err := IssueAccountToken(ctx, st, acct)
	if err != nil {
		t.Fatalf("IssueAccountToken: %v", err)
	}

	rec, err := runStreamInterceptor(BearerStreamInterceptor(st), streamConnWithAuth("Bearer "+token))
	if err != nil {
		t.Fatalf("a valid bearer token must be accepted on the stream door: %v", err)
	}
	if !rec.called {
		t.Fatal("the streaming handler must run on an accepted request")
	}
	if !rec.hasCaller || rec.caller != acct {
		t.Fatalf("the injected caller is the token's account: got %v hasCaller=%v, want %v", rec.caller, rec.hasCaller, acct)
	}
}

// TestBearerStreamInterceptorRejectsBadCredentials: the streaming bearer door
// rejects a missing, non-bearer, unknown, revoked, or cross-kind credential with
// CodeUnauthenticated and never runs the handler (proves the reject-before-next
// guard in WrapStreamingHandler — deleting it lets a rejected stream reach the
// handler). Table-driven so a flipped rejection path reddens exactly the named
// row.
func TestBearerStreamInterceptorRejectsBadCredentials(t *testing.T) {
	ctx := context.Background()
	st, acct, _ := openTestStore(t)

	// A well-formed base64url token that is never issued into the store, so the
	// "unknown token" row is rejected by a store miss, not a parse failure.
	const unissued = "aGVsbG8td29ybGQ"

	revokedToken, err := IssueAccountToken(ctx, st, acct)
	if err != nil {
		t.Fatalf("IssueAccountToken(revoked): %v", err)
	}
	if err := st.RevokeToken(ctx, hashToken(revokedToken)); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	const runnerToken = "cnVubmVyLXRva2Vu"
	if err := st.PutTokenHash(ctx, hashToken(runnerToken), store.Subject{Kind: store.SubjectRunner, ID: "some-runner"}); err != nil {
		t.Fatalf("PutTokenHash(runner): %v", err)
	}

	cases := []struct {
		name   string
		header string
	}{
		{"missing header", ""},
		{"non-bearer scheme", "Basic " + unissued},
		{"unknown token", "Bearer " + unissued},
		{"revoked token", "Bearer " + revokedToken},
		{"cross-kind runner token", "Bearer " + runnerToken},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := runStreamInterceptor(BearerStreamInterceptor(st), streamConnWithAuth(tc.header))
			wantUnauthenticated(t, rec, err, "streaming bearer door: "+tc.name)
		})
	}
}
