package auth

// Shared unary-door test scaffolding + the pure header-parse boundary table
// (RIG-1195 T3, the S3 gate), transcribed from the authoritative Rust suite in
// crates/compass-daemon/src/auth.rs (#[cfg(test)] mod tests, the bearer_auth
// cases). The Rust interceptor mutates a tonic Request and attaches an
// AuthedAccount extension; the Go door is a connect UnaryInterceptorFunc that
// threads the caller into the request context, read back via CallerFrom — so the
// observable contract is "the wrapped handler sees CallerFrom(ctx) == the
// resolved account" on accept, and "connect.CodeOf(err) == CodeUnauthenticated
// and the handler never runs" on reject.
//
// The store-backed bearer accept/reject tests need a live Postgres token store
// (BearerInterceptor resolves against store.Store), so they live in the `pgtest`
// lane (interceptor_pgtest_test.go). This default-lane file holds only what needs
// no store: the shared spy/assert helpers (reused by the pgtest bearer tests, the
// streaming tests, and the admin-gate tests) and the bearerToken parser table,
// whose (string, bool) return is directly observable without any store.
//
// White-box (package auth) to match the T4 house style and reach the unexported
// bearerToken parser directly.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// authHeader is the wire key a real client sets; http.Header canonicalizes it to
// the same form the door reads.
const authHeader = "Authorization"

// spyResult records what the wrapped handler observed: whether it ran at all,
// and the caller CallerFrom returned from the context the interceptor built.
type spyResult struct {
	called    bool
	caller    store.AccountID
	hasCaller bool
}

// recordingSpy is the "next" UnaryFunc: it records CallerFrom(ctx) and returns a
// benign response. A door that rejects a request returns before calling this, so
// spyResult.called staying false proves the handler was gated.
func recordingSpy(rec *spyResult) connect.UnaryFunc {
	return func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		rec.called = true
		rec.caller, rec.hasCaller = CallerFrom(ctx)
		return connect.NewResponse(&compassv1.GetServerInfoResponse{}), nil
	}
}

// bearerRequest builds a connect request. When value != "" it carries an
// Authorization header set to value verbatim (so a test drives the raw wire form
// — scheme, spacing, and all); "" leaves the header absent.
func bearerRequest(value string) connect.AnyRequest {
	req := connect.NewRequest(&compassv1.GetServerInfoRequest{})
	if value != "" {
		req.Header().Set(authHeader, value)
	}
	return req
}

// TestBearerTokenParsesTheHeaderBoundary pins the bearerToken parser's guards
// directly, where each (string, bool) return is independently observable, so a
// flipped guard reddens exactly one named row. The store-backed door reject tests
// (interceptor_pgtest_test.go) run against a store miss, so dropping the scheme or
// interior-space guard would leave them green — this table is where the parse
// boundary is defended. It also covers the empty-credential case the door tests
// never exercise (IssueAccountToken can never mint an empty or space-bearing
// token).
//
// This maps onto the Rust bearer_token helper (crates/compass-daemon/src/auth.rs),
// which the Rust suite likewise only reaches through bearer_auth.
func TestBearerTokenParsesTheHeaderBoundary(t *testing.T) {
	const tok = "aGVsbG8td29ybGQ" // a base64url-shaped credential (no space, non-empty)

	cases := []struct {
		name   string
		header string
		want   string
		wantOK bool
	}{
		{"bearer scheme with token", "Bearer " + tok, tok, true},
		{"lowercase scheme", "bearer " + tok, tok, true},
		{"uppercase scheme", "BEARER " + tok, tok, true},
		{"mixed-case scheme", "BeArEr " + tok, tok, true},
		{"multiple spaces after scheme", "Bearer     " + tok, tok, true},
		{"non-bearer scheme", "Basic " + tok, "", false},
		{"schemeless bare token", tok, "", false},
		{"scheme only, no credential", "Bearer", "", false},
		{"empty credential after scheme", "Bearer ", "", false},
		{"only spaces after scheme", "Bearer    ", "", false},
		{"interior space in credential", "Bearer abc def", "", false},
		{"empty header", "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := bearerToken(tc.header)
			if ok != tc.wantOK {
				t.Fatalf("bearerToken(%q) ok = %v, want %v", tc.header, ok, tc.wantOK)
			}
			if got != tc.want {
				t.Fatalf("bearerToken(%q) token = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

// TestAmbientIdentityThreadsCallerUnary pins the socket door's unary ambient
// interceptor: it attaches the fixed account to every request with no token, so
// the wrapped handler reads that account back via CallerFrom. The socket door
// mounts no bearer interceptor (the 0600 socket mode is the credential), so
// without this the handler sees no caller and fail-closes. Neutralizing the
// interceptor to a pass-through (dropping withCaller) reddens hasCaller.
func TestAmbientIdentityThreadsCallerUnary(t *testing.T) {
	const acct = store.AccountID("ambient-acct")
	rec := &spyResult{}
	wrapped := AmbientIdentity(acct)(recordingSpy(rec))

	if _, err := wrapped(context.Background(), bearerRequest("")); err != nil {
		t.Fatalf("ambient unary door must not error on a token-less request: %v", err)
	}
	if !rec.called {
		t.Fatal("handler did not run: the ambient door must always call next")
	}
	if !rec.hasCaller {
		t.Fatal("CallerFrom(ctx) ok = false: the ambient door must attach a caller")
	}
	if rec.caller != acct {
		t.Fatalf("caller = %q, want %q (the fixed ambient account)", rec.caller, acct)
	}
}

// TestAmbientStreamInterceptorThreadsCallerStreaming pins the load-bearing half:
// AmbientStreamInterceptor's WrapStreamingHandler attaches the same fixed account
// to a streaming RPC's context, so SubscribeAgentSession (which reads
// CallerFrom, with no default) sees the ambient caller instead of fail-closing.
// Deleting the withCaller call in WrapStreamingHandler reddens this
// (hasCaller == false). fakeStreamConn (admin_gate_test.go) is a store-free
// StreamingHandlerConn stub; ambient ignores its headers.
func TestAmbientStreamInterceptorThreadsCallerStreaming(t *testing.T) {
	const acct = store.AccountID("ambient-stream-acct")
	rec := &spyResult{}
	wrapped := AmbientStreamInterceptor(acct).WrapStreamingHandler(recordingStreamHandler(rec))

	if err := wrapped(context.Background(), &fakeStreamConn{}); err != nil {
		t.Fatalf("ambient stream door must not error on a token-less stream: %v", err)
	}
	if !rec.called {
		t.Fatal("streaming handler did not run: the ambient stream door must always call next")
	}
	if !rec.hasCaller {
		t.Fatal("CallerFrom(ctx) ok = false: the ambient stream door must attach a caller")
	}
	if rec.caller != acct {
		t.Fatalf("caller = %q, want %q (the fixed ambient account on the stream leg)", rec.caller, acct)
	}
}
