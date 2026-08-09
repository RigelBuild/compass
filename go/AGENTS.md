# AGENTS.md (go/)

Go-specific notes for this module. See the repository-root `AGENTS.md` for the
workspace-wide conventions and gate.

## Real-Postgres test harness (`internal/pgtest`)

Store/handler/serve integration tests run against a real Postgres via
`pgtest.RequireDSN`, gated behind the `pgtest` build tag. Its behavior is
controlled by these env vars:

- `COMPASS_TEST_DATABASE_DSN` — points the harness at an already-running
  Postgres; each test gets its own schema via `search_path`. This is the fast,
  CI-service path.
- `COMPASS_TEST_USE_CONTAINER` — with no DSN set, opts into a throwaway per-test
  container (~500x slower). Opt-in only, never a silent fallback.
- `COMPASS_REQUIRE_LIVE` — the require-live teeth. With no DSN and no container
  runtime the harness normally SKIPS (so the suite stays green in a
  container-less sandbox). Setting `COMPASS_REQUIRE_LIVE=1` turns that skip into
  a hard failure, for contexts where a live database is mandatory. CI sets it on
  the "Real-Postgres suites" step so an unreachable database fails the run rather
  than silently skipping.
