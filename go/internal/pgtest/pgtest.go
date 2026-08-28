// Package pgtest is the shared real-Postgres test harness for the compass
// backend. A store of record is only proven against the database it targets, so
// every store/handler/serve integration test runs against an actual Postgres
// (design.md:1188-1190) — there is no mock. This package is the store suites'
// entry point to the acquisition policy: it acquires a database (an
// already-running one via COMPASS_TEST_DATABASE_DSN, else — only when explicitly
// opted in via COMPASS_TEST_USE_CONTAINER — a throwaway container started
// through the container CLI), resets its schema between tests, and SKIPs (never
// fails) when no container runtime is usable, so the hermetic gate stays green in
// a container-less sandbox while the assertions are real wherever a runtime
// exists. When a runtime exists but no DSN is set and the container path was not
// opted into, it FAILS LOUDLY rather than silently swapping in the ~500x-slower
// container path and changing what the suite measures.
//
// The acquisition + schema-isolation machinery lives in the shared pgshare
// package (internal/pgshare), which the suite-postgres lanes also build on; this
// package re-exports it as the RequireDSN API the store/server/comms suites have
// always called, so no call-site moves.
//
// It is deliberately store-agnostic: it hands back a DSN, and each package opens
// its own store against it. That keeps this package free of any dependency on
// the packages it serves (no import cycle) and makes it reusable by the store,
// comms, and server suites alike.
//
// It is build-tagged `pgtest` so it is compiled only into that opt-in test lane,
// never the default `go test` gate. It is a non-test file (not `_test.go`) so it
// is importable across packages; the build tag keeps it out of production
// binaries all the same.
//go:build pgtest && unix

package pgtest

import (
	"testing"

	"github.com/RigelBuild/compass/go/internal/pgshare"
)

// DSNEnvVar, when set, points the harness at an already-running Postgres and
// suppresses container management — the CI-service path (design.md:1188).
const DSNEnvVar = "COMPASS_TEST_DATABASE_DSN"

// UseContainerEnvVar, when set to a non-empty value, opts the harness into
// starting a throwaway per-test Postgres container when COMPASS_TEST_DATABASE_DSN
// is unset. The container path is ~500x slower than a shared DSN and silently
// changes what the suite measures, so it is opt-in rather than a fallback.
const UseContainerEnvVar = "COMPASS_TEST_USE_CONTAINER"

// RequireLiveEnvVar, when set to a non-empty value, turns the no-runtime SKIP
// path into a hard failure: with no COMPASS_TEST_DATABASE_DSN and no container
// runtime, RequireDSN would normally skip so the suite stays green in a
// container-less sandbox, but where a live database is mandatory (CI sets this)
// a skip would silently pass the suite without exercising anything. Setting
// COMPASS_REQUIRE_LIVE=1 makes that case fail loudly instead.
const RequireLiveEnvVar = "COMPASS_REQUIRE_LIVE"

// RequireDSN returns a DSN addressing a freshly-created, empty schema, ready for
// a caller to migrate by opening its store against it. It uses
// COMPASS_TEST_DATABASE_DSN when set — giving each test its OWN uniquely-named
// schema on that shared database (via search_path), dropped on cleanup, so
// parallel tests and packages are fully isolated. With no DSN set, it SKIPS the
// test when no container runtime is available, so the suite stays green in a
// container-less sandbox. When a runtime IS present but no DSN is set, it FAILS
// LOUDLY unless COMPASS_TEST_USE_CONTAINER is set: the ~500x-slower throwaway
// container path is opt-in, never a silent fallback that changes what the suite
// measures. When COMPASS_REQUIRE_LIVE is set and neither a DSN nor a container
// runtime is available, the skip becomes a hard failure instead (see
// RequireLiveEnvVar) — for contexts like CI where a live database is mandatory.
//
// It delegates to pgshare.AcquireDSN, the shared acquisition policy; the
// semantics above are that policy's, unchanged.
func RequireDSN(t *testing.T) string {
	t.Helper()
	return pgshare.AcquireDSN(t)
}
