package auth

// Shared Connect v2 server-call scaffolding and the pure bearerToken parser
// table. Store-backed bearer tests run in the `pgtest` lane.

import (
	"context"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/RigelBuild/compass/go/internal/store"
)

// spyResult records what the wrapped handler observed: whether it ran at all,
// and the caller CallerFrom returned from the context the interceptor built.
type spyResult struct {
	tenant    store.TenantID
	called    bool
	caller    store.AccountID
	hasCaller bool
}

// recordingSpy records the caller and tenant observed by a server handler.
func recordingSpy(rec *spyResult) connect.ServerFunc {
	return func(ctx context.Context, _ connect.Spec, _ connect.ServerStream) error {
		rec.called = true
		rec.caller, rec.hasCaller = CallerFrom(ctx)
		rec.tenant, _ = store.TenantFromContext(ctx)
		return nil
	}
}

// fakeServerStream is an inert stream for direct Server.Call tests.
type fakeServerStream struct{}

func (fakeServerStream) Receive(any) error  { return nil }
func (fakeServerStream) SendHeaders() error { return nil }
func (fakeServerStream) Send(any) error     { return nil }

func runServerCall(interceptor connect.ServerInterceptor, ctx context.Context, spec connect.Spec, info *connect.CallInfo, next connect.ServerFunc) error {
	server := connect.NewServer(interceptor)
	server.Register(connect.Method{Spec: spec, Handler: next})
	return server.Call(ctx, spec.Procedure, info, fakeServerStream{})
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

func TestAmbientIdentityThreadsCaller(t *testing.T) {
	const acct = store.AccountID("ambient-acct")
	rec := &spyResult{}
	spec := connect.Spec{StreamType: connect.StreamTypeServer, Procedure: "/test.Auth/Stream"}
	err := runServerCall(AmbientIdentity(acct), context.Background(), spec, &connect.CallInfo{}, recordingSpy(rec))
	if err != nil {
		t.Fatalf("ambient door must not error: %v", err)
	}
	if !rec.called || !rec.hasCaller || rec.caller != acct {
		t.Fatalf("handler result = %+v, want caller %q", rec, acct)
	}
}
