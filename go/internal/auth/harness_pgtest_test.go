//go:build pgtest && unix

package auth

// The auth package's thin adapter over the shared real-Postgres harness
// (internal/pgtest). The token seam the auth layer authenticates against —
// IssueAccountToken persists a hash, ResolveToken/BearerInterceptor resolve it —
// lives in the Postgres store of record (T1), so every test that mints or
// resolves a real token needs a live database. pgtest owns the container
// orchestration + per-test schema isolation and hands back a DSN, SKIPping (not
// failing) when no runtime is usable; openTestStore opens a store.Store against
// it.
//
// Build-tagged `pgtest && unix` so it is not part of the default `go test` gate;
// the pure header-parse and admin-gate-classification tests that need no store
// stay in the default lane (interceptor_test.go, admin_gate_test.go,
// stream_test.go's parse rows). Set COMPASS_TEST_DATABASE_DSN to point every
// test at an already-running Postgres instead of starting a container.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/internal/pgtest"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// openTestStore returns a Store connected to a fresh, migrated database and a
// bootstrap-admin plus a non-admin member account created in it — the two
// identities the bearer/stream door tests mint tokens for. It skips the test
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

// runInterceptor drives interceptor over a spy handler with req, returning what
// the handler observed and the door's error. Lives in the pgtest lane because its
// only callers are the store-backed bearer tests (interceptor_pgtest_test.go); it
// reuses the default-lane spy helpers (recordingSpy, spyResult), which the pgtest
// build includes.
func runInterceptor(interceptor connect.UnaryInterceptorFunc, req connect.AnyRequest) (*spyResult, error) {
	rec := &spyResult{}
	wrapped := interceptor(recordingSpy(rec))
	_, err := wrapped(context.Background(), req)
	return rec, err
}

// wantUnauthenticated asserts the door rejected before the handler ran, with
// CodeUnauthenticated. Shared by the store-backed unary and streaming bearer
// reject matrices (interceptor_pgtest_test.go, stream_test.go), so it lives in
// the pgtest lane with them.
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
