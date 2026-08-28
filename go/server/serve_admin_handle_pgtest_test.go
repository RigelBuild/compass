//go:build pgtest && unix

package server

// The --admin-handle wiring (RIG-1362 T1): the operator-settable bootstrap-admin
// handle must actually name the created account, not merely a log field. These
// tests OBSERVE the account the store minted — a round-trip of the flag into
// ServeConfig is necessary but not sufficient, because an inert knob round-trips
// too (that was the pre-T1 defect). White-box (package server) so they drive the
// real Serve and read the store of record directly.
//
// Store-gated (//go:build pgtest && unix): Serve opens the Postgres store at
// startup and BootstrapAdmin writes there, so every case needs a real database
// via the shared pgtest harness. DSN captured ONCE per test and shared between
// Serve and the observing reads (each pgtest.RequireDSN call mints a fresh
// schema, so a second call would read an empty one).

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// adminAccountRole reads the (handle -> user role) for the account named handle,
// or (found=false) when no account carries it. A direct read of the store of
// record: the observation that proves --admin-handle named the minted account,
// not a log field. role is the user_accounts.role SMALLINT (1 = admin, 0 =
// member); found=false means no account at that handle at all.
func adminAccountRole(t *testing.T, ctx context.Context, dsn, handle string) (role int, found bool) {
	t.Helper()
	conn := connectPG(t, ctx, dsn)
	err := conn.QueryRow(ctx,
		`SELECT u.role FROM accounts a JOIN user_accounts u ON u.account_id = a.id WHERE a.handle = $1`,
		handle,
	).Scan(&role)
	if err != nil {
		return 0, false // no such user account (unknown handle, or an agent-only account)
	}
	return role, true
}

// TestServeMintsBootstrapAdminUnderConfiguredHandle: with AdminHandle set to a
// non-default value, the account Serve creates carries THAT handle as an admin —
// observed against the store, not the config or a log — and no "admin"-handled
// account is created. This is the T1 defect's regression guard: before the
// wiring, --admin-handle reached only a log field while the account stayed
// "admin", so this test reddens on any reversion to an inert knob.
func TestServeMintsBootstrapAdminUnderConfiguredHandle(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.RequireDSN(t)
	dir := t.TempDir()

	serveInBackground(t, ServeConfig{
		SocketPath:  filepath.Join(dir, "compass.sock"),
		DatabaseDSN: dsn,
		Version:     "admin-handle-test",
		AdminHandle: "matt",
		// Socket-only: bootstrap runs unconditionally, so no --listen is needed.
	})
	waitServing(t, filepath.Join(dir, "compass.sock"))

	role, found := adminAccountRole(t, ctx, dsn, "matt")
	if !found {
		t.Fatal(`no account named "matt" after serving with --admin-handle=matt; the flag is inert (reached a log field, not BootstrapAdmin)`)
	}
	if role != int(store.UserRoleAdmin) {
		t.Fatalf(`account "matt" role = %d, want %d (admin)`, role, int(store.UserRoleAdmin))
	}
	// The collapsed default must not also mint an "admin" account: the handle is
	// the one the operator set, nothing else.
	if _, found := adminAccountRole(t, ctx, dsn, "admin"); found {
		t.Fatal(`an "admin"-handled account was created alongside "matt"; the configured handle must be the only bootstrap admin`)
	}
}

// TestServeMintsDefaultAdminHandleWhenUnset: with AdminHandle empty, the created
// account carries the collapsed default "admin" — the socket-only shipped path is
// byte-identical to before the flag existed.
func TestServeMintsDefaultAdminHandleWhenUnset(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.RequireDSN(t)
	dir := t.TempDir()

	serveInBackground(t, ServeConfig{
		SocketPath:  filepath.Join(dir, "compass.sock"),
		DatabaseDSN: dsn,
		Version:     "admin-handle-default-test",
		// AdminHandle intentionally unset.
	})
	waitServing(t, filepath.Join(dir, "compass.sock"))

	role, found := adminAccountRole(t, ctx, dsn, "admin")
	if !found {
		t.Fatal(`no "admin" account after serving with an unset --admin-handle; the default regressed`)
	}
	if role != int(store.UserRoleAdmin) {
		t.Fatalf(`default "admin" account role = %d, want %d (admin)`, role, int(store.UserRoleAdmin))
	}
}

// TestServeFailsWhenAdminHandleNamesExistingMember pins the hard startup failure
// the operator-settable handle introduces: naming a handle that already exists as
// a MEMBER account must fail Serve with ErrConflict rather than silently
// elevating that member to admin. Reachable only now that the handle is
// operator-settable (it was a package constant before), so red before the wiring,
// green after — the refusal is intended behavior, not an accident of adminByHandle.
//
// Serve is driven directly (not serveInBackground) because it must return the
// error, not reach the serving stage; the member account must remain a member.
func TestServeFailsWhenAdminHandleNamesExistingMember(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.RequireDSN(t)
	dir := t.TempDir()

	// Seed a MEMBER account at the handle the operator will (mis)configure as the
	// admin. Member handles are operator-reachable via CommsService.CreateUser.
	seed, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store Open (seed): %v", err)
	}
	if _, err := seed.CreateUser(ctx, store.NewUser{Handle: "matt", DisplayName: "Matt"}); err != nil {
		t.Fatalf("seed member account: %v", err)
	}
	seed.Close()

	// Drive Serve on a bounded ctx in a goroutine: the correct behavior is a fast
	// ErrConflict return, but a regression that WRONGLY succeeds would reach the
	// serving stage and block until ctx cancel — so the deadline turns that bug
	// into a fast, legible failure rather than a hung suite. cancel on return
	// tears down a wrongly-serving Serve.
	serveCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- Serve(serveCtx, ServeConfig{
			SocketPath:  filepath.Join(dir, "compass.sock"),
			DatabaseDSN: dsn,
			Version:     "admin-handle-conflict-test",
			AdminHandle: "matt",
		})
	}()
	select {
	case err = <-errCh:
	case <-serveCtx.Done():
		t.Fatal(`Serve did not fail fast on the member-handle conflict — it reached the serving stage, so the member was (wrongly) accepted as admin`)
	}
	if err == nil {
		t.Fatal(`Serve with --admin-handle naming an existing member = nil, want an ErrConflict startup failure (must not elevate the member)`)
	}
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Serve error = %v, want wrapping store.ErrConflict (handle exists but is not an admin)", err)
	}

	// The member must NOT have been elevated: it is still a member (role 0), and
	// no admin account carries the handle.
	role, found := adminAccountRole(t, ctx, dsn, "matt")
	if !found {
		t.Fatal(`seeded "matt" account vanished after the failed Serve`)
	}
	if role != int(store.UserRoleMember) {
		t.Fatalf(`"matt" role = %d after failed Serve, want %d (member, NOT elevated)`, role, int(store.UserRoleMember))
	}
}
