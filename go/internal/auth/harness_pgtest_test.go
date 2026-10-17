//go:build pgtest && unix

package auth

// The auth package's thin adapter over the shared real-Postgres harness
// (internal/pgtest). The token seam auth authenticates against lives in the
// Postgres store of record, so tests that mint or resolve a real token need a
// live database; pgtest hands back a DSN, SKIPping when no runtime is usable.

// Build-tagged `pgtest && unix`, out of the default `go test` gate. Set
// COMPASS_TEST_DATABASE_DSN to point at an already-running Postgres.

import (
	"context"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// authHeader is the wire key a real client sets; http.Header canonicalizes it to
// the same form the door reads.
const authHeader = "Authorization"

func bearerRequest(value string) *connect.CallInfo {
	info := &connect.CallInfo{}
	if value != "" {
		info.RequestHeader().Set(authHeader, value)
	}
	return info
}

func runInterceptor(interceptor connect.ServerInterceptor, req *connect.CallInfo) (*spyResult, error) {
	rec := &spyResult{}
	spec := connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/test.Auth/Call"}
	err := runServerCall(interceptor, context.Background(), spec, req, recordingSpy(rec))
	return rec, err
}

func wantUnauthenticated(t *testing.T, rec *spyResult, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a rejection, got nil error", what)
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Fatalf("%s: expected CodeUnauthenticated, got %v", what, code)
	}
	if rec.called {
		t.Fatalf("%s: the handler must not run when the door rejects", what)
	}
}

// openTestStore returns a Store connected to a fresh, migrated database and a
// bootstrap-admin plus a non-admin member account created in it. It skips the test
// when no DSN and no container runtime are available.
func openTestStore(t *testing.T) (*store.Store, store.AccountID, store.AccountID) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, pgtest.RequireDSN(t))
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)

	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	member, err := st.CreateUser(ctx, store.NewUser{Handle: "member", DisplayName: "member"})
	if err != nil {
		t.Fatalf("CreateUser(member): %v", err)
	}
	return st, admin.ID, member.ID
}
