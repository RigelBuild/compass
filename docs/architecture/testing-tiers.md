# Go testing tiers

The Go module has three test tiers. Pick the cheapest tier that can fail on
the behavior you are testing.

| Tier | Build tag | Runs against | Cost | CI job |
| --- | --- | --- | --- | --- |
| Default | none | Fakes and in-process code | Seconds | moon battery |
| pgtest | `pgtest` | A real Postgres | Minutes per package | `pgtest` |
| podman e2e | `podman` | The real stack and the real agent image | Tens of minutes | `e2e` |

The exact commands and flags are in the `pgtest` and `e2e` jobs of
`.github/workflows/ci.yml`.

## UI-inclusive tier

The `podman uitier` scenarios drive the real Vite UI in headless Chromium against
an H1 fixture. One checks that the agent reply renders in a topic started from the
UI. Another checks that a channel joined while the page is open shows its earlier
messages, and that an agent created then gets its home channel. The third checks
that a DM shows messages posted before the page loaded, and that a READY session's
Session Log header reads idle. Run them with
`cd go && direnv exec .. go test -tags 'podman uitier' -count=1 -p 1 -v -run '^TestUITier' ./e2e/`.
It runs on pushes to `main` and nightly, not on pull requests.

## Which tier

1. Does the assertion need real SQL: a query, a constraint, RLS, a migration?
   Use **pgtest**.
2. Does it need the process or container topology: stack children, the runner,
   an agent turn, teardown? Use **podman e2e**.
3. Otherwise use the **default** tier.

A tagged test that cannot reach its dependency skips. CI checks that the
pgtest and podman suites actually ran, so a skip there does not pass silently.

## pgtest

The entry point is `pgtest.RequireDSN(t)` (`go/internal/pgtest`). Open your own
store on the DSN it returns. It finds Postgres in this order:

- `COMPASS_TEST_DATABASE_DSN` points at a running Postgres. Each test gets a
  fresh schema, dropped when the test ends. CI uses this.
- `COMPASS_TEST_USE_CONTAINER=1` starts a throwaway container. Use this
  locally.
- With neither set, it fails when a container runtime exists, and skips when
  none does. CI sets `COMPASS_REQUIRE_LIVE=1`, so there a missing database
  fails.

For tests that must go through a transaction-mode pooler, use
`pgtest.RequirePoolerDSN(t)`. It needs both `COMPASS_TEST_DATABASE_DSN` and
`COMPASS_TEST_POOLER_DSN` in URL form, and gives each test a fresh database.

## podman e2e

Fixture-based legs live in `go/e2e`. `cmd/compass-stack` also has
podman-tagged supervisor tests that do not use the fixture.

In `go/e2e`, `TestMain` builds the stack binaries once and starts one suite
Postgres. Each leg starts with a `podmanUsable` check and skips when rootless
podman cannot run the agent image.

### Shared stack: the default

Most legs call `sharedFixture(t)`. Every such leg in the run uses one stack,
which attaches to the suite Postgres through `ExternalDatabase`.

- Register options the shared stack needs from a `register<Leg>FixtureOptions`
  function listed in `TestMain`. Never register from a leg body: canned routes
  are fixed when the stub is built. A registered option applies to every shared
  leg.
- You can register `WithForgeStub()` and the `WithCanned*` options.
  `WithObjectStore` needs a `*testing.T` to start, so it is own-stack only.
- Route each model turn by a marker unique to your leg. The positional canned
  script is one counter shared by every leg.
- Await every turn your leg starts before it returns, so no model request
  outlives the leg.
- `-count` above 1 is not supported; CI runs with `-count=1`. In a full run,
  `zz_legsix_test.go` closes the shared stack at the end of each pass, so a
  later pass cannot reach it. Without leg six, handles are create-only and the
  stack persists, so the second pass fails with `already_exists`.

### Own stack: the opt-out

Call `NewFixture(ctx, t, opts...)` when the leg needs a stack nobody else
touches, such as a restart, a teardown check, or object-store state. It costs a
full cold start: its own stack-owned Postgres, server and runner.

- `WithSite(site)` reuses one root, state dir and port across two `NewFixture`
  calls, so the second `Up` reattaches the same Postgres data.
- `WithEmbeddedPostgres()` states that the leg needs the stack-owned Postgres.
  Plain `NewFixture` already uses it; the option fails the test if an external
  database is also set.
- `WithForgeStub()`, `WithObjectStore(g)` and the `WithCanned*` options apply to
  this leg only.

`zz_legsix_test.go` closes the shared stack before its restart, so it must run
last. Its file name sorts it last, and it skips under `-shuffle`.

### The embedded-Postgres smoke leg

Shared legs use the suite Postgres. Other own-stack legs use the stack-owned
Postgres but do not check its lifecycle. `TestEmbeddedPostgresSmoke` is the
only leg that asserts the supervisor-owns-Postgres lifecycle and its pgid-record
teardown: the record names a live Postgres group after `Up`, and after `Down`
the group is gone and the record is removed. Keep it when you change how the
stack starts or stops Postgres.

## Related

- Design record: `RigelBuild/docs:docs/designs/compass/infra/ci/compass-test-harness-tiers/design.md`.
- CI jobs: [Build & CI](build-and-ci.md).
