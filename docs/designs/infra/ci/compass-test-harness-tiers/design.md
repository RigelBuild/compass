# Consolidate the Go test-harness tiers (RIG-2742)

Status: Draft
Directory-form record under `docs/designs/infra/ci/`. All file:line citations
are grounded against `main@origin` `0c75fe91` (post-#668, the L1 TestMain
build-once merge).

## Problem / Intent

The compass Go repo has a real three-tier test pyramid — the in-process
`*stack.Stack` e2e fixture (`go/e2e/fixture.go:1`, `//go:build podman`), the
pgtest wire harness (`go/internal/pgtest/pgtest.go:24`, `//go:build pgtest`),
and the default httptest/RoundTripper unit suite — but tiers 1 and 2 duplicate
the real-Postgres standup + store wiring through different paths, forge
injection differs per tier, and no contributor doc says which tier a new test
belongs in. Separately, the `dogfood-e2e` CI job runs ~30-40 min because every
one of the ~11 podman legs stands up its own cold stack (a fresh `initdb`
postgres, a fresh server, a fresh runner enrollment, a fresh root-supervisor
seed — `go/e2e/fixture.go:190-372`). L1 (build the stack binaries once,
`go/e2e/main_test.go:29-51`) shipped in #668; L4 (CI Go-cache) is a separate
CI-only PR. This record designs L2 (one shared long-lived stack across legs)
and L3 (one shared external postgres), which are the same consolidation viewed
from the runtime side and the dominant remaining levers.

## Ruled context (frozen — this record designs against it, not over it)

- **DL-257 / RIG-2743, Option A (Matt; #578 merged 2026-08-26, re-confirmed on
  RIG-2853):** the embedded/bundled `postgres:18` container
  (`go/internal/stack/postgres_image.go:26`, pinned by digest) STAYS the
  zero-config stack default; `--database-external <DSN>` is the first-class BYO
  opt-out (`go/cmd/compass-stack/main.go:153` → `Config.ExternalDatabase`,
  `go/internal/stack/config.go:41-47`). This design is postgres-KEPT: the
  harness uses the external-DSN opt-out for speed; it removes nothing.
- **The DSN contract is byte-identical across all three postgres paths.**
  `Config.DatabaseDSN` (`go/internal/stack/config.go:29-31`) is the single
  connect seam whether postgres is the dev-path wrapper child, the supervised
  container, or external; `startPostgres` only chooses WHO runs postgres
  (`go/internal/stack/stack.go:309-325`), and on `ExternalDatabase` it starts
  no component and `spawnChain` probes the given DSN as-is
  (`go/internal/stack/stack.go:310-311`, `217-231`). "Stand postgres up once,
  connect via DSN" is therefore uniform by construction.
- **The supervisor-owns-postgres lifecycle machinery (pgid-record-v2 teardown
  identity, DL-259/T8a) is exercised ONLY on the non-external paths**
  (`go/internal/stack/stack.go:316-324` records the wrapper child;
  `stack.go:327-344` records the container by name). The external-DSN harness
  path deliberately never touches it — so the ONE embedded-postgres smoke leg
  below is the sole dedicated e2e coverage of that machinery, and that is
  that leg's explicit purpose.
- **Matt's e2e strategy (direct):** the general e2e suite stands up ONE
  postgres once and routes every leg through the external-DSN path; exactly one
  leg keeps the bundled/embedded postgres default under test.

## Approach

Four moves, one theme: converge on the seams that already exist
(`Config.DatabaseDSN` + `ExternalDatabase`, the pgtest `search_path` schema
isolation, the e2e `TestMain` per-package lifecycle, the `Fixture` functional
options) instead of growing a fourth harness.

### (a) One shared postgres-standup + store-wire helper for tiers 1 and 2

Today the two real-Postgres tiers acquire a database independently: the e2e
fixture forms a private-socket DSN and lets the stack spawn the
`compass-postgres` wrapper (`go/e2e/fixture.go:235`, wrapper contract
`go/cmd/compass-postgres/main.go:3-18`), while pgtest acquires a shared DSN
from `COMPASS_TEST_DATABASE_DSN` or an opt-in throwaway container
(`go/internal/pgtest/pgtest.go:99-132`) and isolates each test in a fresh
uniquely-named schema selected via a per-connection `search_path`
(`pgtest.go:139-171`, `173-194`).

The convergence point is pgtest's model, which is already the good pattern:

- Extract the **database-acquisition + per-test-schema logic** into a shared
  internal package, `go/internal/pgshare` (build-tagged `pgtest || podman` so
  it compiles into exactly the two opt-in lanes and never the default gate,
  mirroring `pgtest.go:20-24`'s rationale). `pgtest` keeps its public API
  (`RequireDSN(t)`) and becomes a thin wrapper; the e2e suite gains access to
  the same `isolatedSchemaDSN`/`withSearchPath` machinery without importing a
  `pgtest`-tagged package into a `podman`-tagged build.
- The e2e suite's shared postgres (below) is stood up ONCE per package run in
  `TestMain` — the per-package seam #668 already established
  (`go/e2e/main_test.go:29-51`) — by exec'ing the already-built
  `compass-postgres` binary directly (it is on PATH from `buildStackBinaries`,
  `main_test.go:40-46`) against a suite-lifetime state dir, then handing every
  leg a schema-isolated DSN derived from it.
- Forge injection converges on the single production seam both tiers already
  individually use — `hub.SetForgeCaller`
  (`go/internal/runnerhub/relay_forge.go:58-68`, production wiring
  `go/server/serve.go:908`) — by lifting the pgtest-side `newForgeE2EWire`
  mounting pattern (`go/server/forge_e2e_pgtest_test.go:109-112`) into the
  shared builder's optional knob rather than each suite hand-rolling it.

### (b) One shared long-lived stack across e2e legs

Replace per-leg `NewFixture` cold boots with a package-level shared stack
stood up once in `TestMain` (after `buildStackBinaries`, before `m.Run()`) and
torn down after. `NewFixture` grows a default path that ATTACHES to the shared
stack: it returns a `Fixture` whose clients dial the shared door and whose
per-leg isolation comes from data, not process, boundaries:

- **Per-leg accounts/channels:** every leg already creates its own uniquely
  named agents and channels through the fixture's RPC factories
  (`go/e2e/agent_ops.go:22-33` `CreateAgent`, `:39` `Provision` — e.g.
  `legtwo_test.go:31` creates `leg2-primitives`). Sharing a stack keeps those
  namespaces disjoint by the same handles the legs already choose.
- **The one-time gates run once:** runner enrollment
  (`go/e2e/fixture.go:336-347`) and the root-supervisor seed settle
  (`fixture.go:349-369`, budgets `go/e2e/timeouts.go:39-65`) are per-STACK
  conditions, not per-leg — on a shared stack they are paid once in `TestMain`
  instead of ~11 times.
- **Store-side assertions keep working unchanged:** legs open a store on
  `f.DSN()` (`go/e2e/seed_settle_test.go:44`); the shared fixture hands out
  the shared stack's DSN.

**Database isolation on the shared stack is the compatibility fork this
design resolves conservatively:** the server child opens ONE store on the DSN
`stack.Up` was given (`spawnChain` step 3, `go/internal/stack/stack.go:256`),
so per-leg `search_path` schemas cannot isolate legs that observe each other
THROUGH the shared server (accounts, channels, placements all live in the one
schema the server opened). Shared-stack legs therefore share one database and
isolate by per-leg account/channel namespace (which they already do); the
`search_path` schema isolation of move (a) applies to the store-direct tiers
(pgtest, and e2e legs' direct `store.Open` reads scope naturally to the same
schema the server uses). A leg that asserts on GLOBAL state (e.g. counts of
all accounts) must scope its assertions to its own handles — the existing legs
already do (`legcomms_test.go`, `legthreefour_test.go` assert on their own
created ids).

**Legs that keep their OWN stack (opt out via `WithOwnStack()` — see (d)):**

- `TestHarnessCore` (`go/e2e/harness_test.go:27-116`): its step 6 asserts
  `Down` drains cleanly and Health goes non-Ready (`harness_test.go:100-115`)
  — a teardown-idempotence assertion that would kill the shared stack.
- The stack-restart / teardown-idempotence leg
  (`TestLegSixTeardownIdempotence`, `legsix_test.go:72,136`): drives TWO
  stack lifecycles over one persistent site (`WithSite`,
  `fixture.go:131-143`) — its whole subject is stack restart. (Not to be
  confused with `TestLegFivePersistAndResume`, `legfive_test.go:13-19`, a
  SESSION resume across container teardown with NO stack restart — legfive
  MIGRATES to the shared stack as leg 5.)
- The embedded-postgres smoke leg (new, move (c)): needs its own stack by
  definition (different postgres path).
- The seed-settle leg (`seed_settle_test.go:42`): asserts the first-launch
  seed behavior, which only fires on a cold stack.

Everything else — legs 2/3/4/5, comms, client-mode — attaches to the shared
stack. The canned-model backend (`WithCannedScript`, `fixture.go:104-115`)
moves to the shared stack's config: the marker-routing mechanism
(`WithCannedMarkerReply`, `fixture.go:117-129`) already lets multiple
consumers share one scripted backend; legs register their scripts against the
shared stub per-leg (Task 3 carries the exact mechanism).

**Shared-stack tradeoff (ratify knowingly):** one shared stack converts
leg-isolation failures into suite-cascade failures — a leg that wedges the
shared runner or stub can fail every later attached leg, where today a bad
leg takes down only its own stack. This is the accepted cost of the ~10×
standup win; the four own-stack legs above and `WithOwnStack()` are the
escape hatch for any leg that cannot tolerate it.

### (c) One embedded-postgres smoke leg; everything else external-DSN

The shared stack of (b) is stood up with `ExternalDatabase: true` pointing at
the (a) suite postgres — so ~10 legs stop paying `initdb` + wrapper-boot, and
`waitPostgres` (`go/internal/stack/stack.go:425-449`) reduces to probing an
already-hot DSN.

ONE new leg, `TestEmbeddedPostgresSmoke`, stands up its own stack the way the
product default does — `ExternalDatabase: false` — and asserts Ready + one
authed RPC + clean Down. Its explicit purpose: it is the sole DEDICATED e2e
coverage of the supervisor-owns-postgres path and its pgid-record-v2 teardown
identity
(`stack.go:309-325`, container variant `:327-344`), per the ruled context
above. In CI (no `postgres:18` container image pre-pulled; the fixture's
current dev-path spawn is the cheap variant) it uses the wrapper-process path
(`PostgresImage` empty), which is the same `startPostgres` non-external arm
the fixture exercises today (`fixture.go:237-266` sets no `PostgresImage`);
the container-backed arm keeps its dedicated coverage in
`go/cmd/compass-stack/container_postgres_podman_test.go:51-55`.

### (d) One canonical fixture builder — extend `Fixture`, no fourth harness

All of the above lands as new functional options on the EXISTING
`fixtureOption` pattern (`go/e2e/fixture.go:83-86`):

- The zero-option default `NewFixture(ctx, t)` becomes attach-to-shared-stack.
- `WithOwnStack()` restores today's cold-boot semantics for the four opt-out
  legs.
- `WithSite` (`fixture.go:131-143`) implies own-stack (it already is one).
- `WithEmbeddedPostgres()` (own-stack implied) selects the non-external
  `startPostgres` arm for the smoke leg.

**Tier-2 consumer contract — DECIDED, not open: `pgtest.RequireDSN(t)` +
`store.Open` STAYS the tier-2 entry point, byte-for-byte.** The `pgshare`
extraction of move (a) sits UNDERNEATH `RequireDSN`, which becomes a thin
delegating wrapper with verbatim semantics (T1); no pgtest-tier call site
(`go/server/*_pgtest_test.go`, store/comms suites) changes signature, and no
NewFixture-style builder is introduced for the pgtest tier. The two tiers
reach the shared standup through tier-appropriate doors over one
implementation: pgtest callers via `pgtest.RequireDSN`, the podman e2e
TestMain via `pgshare.StartSuitePostgresMain` (T1/T2). No new harness package
is exported to test authors; the tier choice is documented (Task 6).

## Consumers / boundary

**RIG-2848 (forge-notification full-stack e2e) is a named cross-tier consumer
of the shared standup seam.** Its tier-2 suite lives in `go/server`
(`//go:build pgtest && unix`), mirrors the existing
`go/server/forge_e2e_pgtest_test.go` shape (real store via
`pgtest.RequireDSN` + `store.Open`, handler driven over httptest), and per
the decision above keeps calling `pgtest.RequireDSN(t)` unchanged — the
shared helper serves it invisibly from underneath. Its full-stack framing
(real Runner + agent turn) lands in the `go/e2e` podman tier, where it
attaches to the shared stack of move (b) like any other leg. The one seam
both halves reach is `go/internal/pgshare` (T1): `pgtest.RequireDSN` →
`pgshare.AcquireDSN`/`pgshare.IsolatedSchemaDSN` on the pgtest side, and
`pgshare.StartSuitePostgresMain` feeding the shared stack's
`Config.DatabaseDSN` on the podman side. RIG-2848's seam-independent doubles
(fake GitHub/Linear signed-webhook senders) do not touch this seam, and its
runnable end-to-end is gated on its own #677 T5/T7 + RIG-2717, not on this
record's tasks. T5's forge-injection convergence (`mountForgeForTest`)
is the helper their hub wiring should adopt when it lands.

## Alternatives considered

### testcontainers-go wholesale — REJECTED

The bespoke podman stack already solves rootless-podman realities the generic
library does not model (pasta host-gateway addressing `fixture.go:391-398`,
sun_path budgeting `config.go:100-114`, pgid-record teardown identity), and it
IS the product code under test — replacing the harness's container layer with
a dependency would test the dependency, not `stack.Up`. Borrow its principles
(pinned digests, readiness probes, per-run temp roots — all already present);
skip the dep.

### Collapsing the three tiers into one — REJECTED

The tiers sit at genuinely different fidelity/speed points: unit stubs run in
milliseconds with no runtime; pgtest proves store semantics against real
Postgres in seconds; the podman e2e proves the whole child-process +
container topology in minutes. Collapsing them either slows the default gate
unacceptably or deletes the only whole-system proof. The pyramid stays; only
the duplicated plumbing converges.

### Per-leg `search_path` schemas THROUGH the shared server — DEFERRED

True per-leg database isolation on the shared stack would need the server to
accept a per-session or per-request schema, a production change with no
product motivation. Namespace isolation by per-leg accounts/channels is
sufficient for the current legs and requires no server change. Revisit only
if a future leg genuinely needs global-state isolation (non-load-bearing;
see Open Questions).

**Forward path (Matt, freeze gate 2026-08-27): production tenant isolation
is coming — this is a short-term bridge.** The namespace-isolation call
above is explicitly a test-harness bridge, not a bet that per-tenant
isolation never lands. The managed-service multi-tenancy design owns that
production decision (RIG-2861, `compass-managed`; in-flight record
`docs/designs/infra/runtime/compass-managed-multitenancy/design.md` §Q1
(PR #687), not yet frozen; the load-bearing forks are parked for Matt in
RIG-2877). Its Q1 has firmed on one shared database with a `tenant_id`
column and Postgres row-level security, scoped per transaction by a `SET
LOCAL compass.tenant_id` GUC — not a second store, a per-session DSN, or
`search_path` switching (that record rejects schema-per-tenant `search_path`
as silently-wrong-prone). So this record's shared-stack premise holds
exactly: one server on one boot DSN (`stack.go:256`) stays the single store,
and production tenancy adds a per-transaction tenant GUC on it rather than
any new store-selection seam. When it lands, per-leg e2e isolation MAY ride
real `tenant_id` RLS scoping in place of name-spacing — a test-harness
adoption choice for this lane, not forced by RIG-2861 — retiring the bridge.
(The enforcement layer, RLS vs application-level scoping, is itself RIG-2877
OQ-2, still Matt's to rule.) Until RIG-2861 freezes, the bridge stands: no
server change, namespace isolation, serial suite.

### Own-STACK legs over the suite postgres (not embedded) — DEFERRED

`TestHarnessCore` and seed-settle need a fresh stack and an empty database
but not the embedded-postgres arm specifically: an own-stack `Up` with
`ExternalDatabase: true` over a fresh `IsolatedSchemaDSN` on the suite
postgres would give them an empty schema and cut two of the four remaining
`initdb` runs. Deferred: the marginal win (~2 `initdb` runs) is not worth a
third meaning of "own stack" (own-stack-own-postgres vs
own-stack-suite-postgres); the four opt-out legs keep today's cold-embedded
semantics. `legsix`'s `WithSite` two-`Up` re-attach stays real embedded-arm
coverage beyond the smoke leg (`legsix_test.go:56-59`).

## Global Constraints

- `go 1.25.0` (`go/go.mod`); no vendoring — modules are downloaded.
- Build tags are load-bearing and MUST be preserved: `//go:build podman` on
  every e2e-stack file (`go/e2e/fixture.go:1`, `main_test.go:1` — the
  untagged hermetic lane in `cannedmodel_test.go` must keep Go's default
the pgtest harness (`pgtest.go:24`), and the new shared package tagged
`pgtest || podman`. **`pgshare` may only be imported from build-tagged files
— never from package `e2e`'s deliberately-untagged files (`cannedmodel.go:7`),
which would drag `pgshare` onto the default gate and break the hermetic-lane
compile.**
- **No `time.Sleep` waits — readiness is event-gated** (RIG-2741;
  `rule://go-no-sleep-in-test`): every new gate follows the existing bounded
  fail-closed poll pattern (`waitPostgres` `stack.go:425-449`,
  `waitRunnerEnrolled`/`waitSeedSettled` budgets `go/e2e/timeouts.go:27-65`),
  never a fixed sleep, never a retry-as-sync.
- Dynamic ports stay as-is: `freePorts` binds `:0` and reads the kernel port
  (`fixture.go:460-483`); `Config.Validate` rejects `:0` listen addrs
  (`config.go:119-128`), so the shared stack allocates its fixed ports once
  in `TestMain` the same way.
- Container removal is exact-name only, never a filter
  (`go/e2e/teardown.go:11-32`, `rule://process-safety`).
- The pinned postgres digests do not move here: `pgtest.pgImage`
  (`pgtest.go:50`) and `stack.DefaultPostgresImage`
  (`postgres_image.go:26`) change only via reviewed manual PRs.
- The e2e suite remains serial (no `t.Parallel()` in `go/e2e` today); this
  design does not introduce parallel legs.
- No production (non-test, non-harness) behavior changes anywhere in this
  plan; `stack.Up`'s public contract is untouched.

## Plan

Dependency order: T1 → (T2, T5 independent) → T3 → T4 → T6. T5 can land any
time after T1.

### T1 — `pgshare`: shared database-acquisition + schema-isolation package

Extract `isolatedSchemaDSN`, `withSearchPath`, `schemaSeq`, and the
DSN/container acquisition policy (`decideDSNSource`, `startContainer`,
`waitReady`) from `go/internal/pgtest/pgtest.go:81-298` into
`go/internal/pgshare` with build tag `//go:build (pgtest || podman) && unix`.
`pgtest` re-exports its existing API unchanged; no call-site in the pgtest
suites moves.

- Interfaces (produced, package `pgshare`):
  - `func AcquireDSN(t *testing.T) string` — the current `RequireDSN` policy
    (env DSN / opt-in container / skip / require-live), verbatim semantics.
  - `func IsolatedSchemaDSN(t *testing.T, dsn string) string` — per-test
    schema + CASCADE-drop cleanup + `options=-c search_path=…` DSN,
    verbatim from `pgtest.go:139-194` including the existing-`options` panic
    guard.
  - `func StartSuitePostgres(tb testing.TB, stateDir, sockDir string, port int) (dsn string, stop func())`
    — NEW: exec the on-PATH `compass-postgres` binary (`--state-dir`,
    `--database <dsn>` per `go/cmd/compass-postgres/main.go:7`), event-gate on
    a bounded connect poll (the `pgtest.waitReady` pattern,
    `pgtest.go:279-298`), return the base DSN and a stop that SIGTERMs and
    waits the child. `testing.TB`-free variant for TestMain use:
    `StartSuitePostgresMain(stateDir, sockDir string, port int) (string, func(), error)`.
- Interfaces (consumed): `compass-postgres` argv contract
  (`cmd/compass-postgres/main.go:7`); libpq keyword/value DSN form
  (`fixture.go:232-235`).
- Test cycle: existing pgtest suites green under `-tags pgtest` (they are the
  behavior lock); one new unit test for the TestMain-variant error paths.

### T2 — shared e2e stack lifecycle in TestMain

Extend `go/e2e/main_test.go` `TestMain`: after `buildStackBinaries` and the
PATH export (`main_test.go:34-46`), start the suite postgres
(`pgshare.StartSuitePostgresMain`), then stand up ONE shared stack via the
existing `stack.Up` with `ExternalDatabase: true` and `DatabaseDSN` = the
suite DSN, run the enrollment + seed-settle gates once (today's
`waitRunnerEnrolled`/`waitSeedSettled`, `fixture.go:336-369`, lifted to
`testing.TB`-free forms), export the shared handles through package-level
state, `m.Run()`, then Down the stack and stop postgres in reverse order.

- Interfaces (produced, package `e2e`, podman-tagged):
  - `type sharedStack struct { stack *stack.Stack; dsn, serverURL, caPath, adminToken, runtimeDir string; stub *cannedModelServer }`
    — package-private; populated in TestMain, read by `NewFixture`.
  - `func upSharedStack(binPATHReady bool) (*sharedStack, func(), error)` —
    the TestMain body helper (t-free, like `buildStackBinaries`,
    `main_test.go:53-59`).
- Interfaces (consumed): `stack.Up(ctx, cfg, deps)` / `Stack.Down`
  (`stack.go:84-123`, `:173-196`); `Config.ExternalDatabase` + `DatabaseDSN`
  (`config.go:29-47`); adapter set as in `fixture.go:279-288`.
- Test cycle: `go test -tags podman ./e2e/ -run TestHarnessCore` (own-stack
  leg still green) plus one shared-stack attach smoke (T3's first migrated
  leg is the real proof).

### T3 — migrate legs onto the shared stack; `WithOwnStack` opt-out

Add `WithOwnStack()` to `fixtureOption`; flip `NewFixture`'s default to
attach-to-shared (build clients via `newAuthedClients` against the shared
door, skip Up and both gates). `WithSite` and `WithEmbeddedPostgres` imply
own-stack. Migrate legs 2/3/4/5, comms, and client-mode to the shared
default; pin `TestHarnessCore` and `seed_settle_test.go` to `WithOwnStack()`.
The canned-model backend becomes a shared-stack facility: the shared stack is
stood up with the canned stub (as `WithCannedModel` does today,
`fixture.go:268-277`), and legs load their per-leg scripts/markers into it via
a new `stub.LoadScript(t, script, markers)` that registers a t.Cleanup reset —
the marker-routing precedent is `WithCannedMarkerReply`
(`fixture.go:117-129`). Two contracts the reset MUST honor, because the shared
stub outlives each leg (this resolves OQ2):

- **The reset REPLACES the positional script + counter but PRESERVES every
  previously registered marker.** Markers are additive and body-keyed
  (`cannedmodel.go:132-136`), so a stale marker is harmless; wiping them is the
  hazard. A leg-exit trailing turn a leg triggered but never awaited
  (legthreefour's marker-routed mention turns settle off `AwaitControlDispatch`,
  NOT `AwaitTurnSettled` — `legthreefour_test.go:85-86,:362-366`) can still be
  in flight at the stub when the next leg's `LoadScript` lands; had the reset
  wiped markers, that trailing request would match nothing and DRAW SLOT 0 of
  the next leg's positional script, desyncing it. Preserving markers keeps the
  trailing hit harmless (Setup itself is marker-routed off a separate counter,
  `cannedmodel.go:169-184`).
- **Every leg MUST `AwaitTurnSettled` on every turn it triggers before
  returning — including assertion-free marker turns** — so no model request
  outlives the leg boundary. (`WithOwnStack` legs are exempt: their stack Down
  reaps any in-flight request.)

`-count>1` (or any same-process re-run) is UNSUPPORTED for shared-stack
`./e2e/` legs: handles are create-only (`legsix_test.go:139-141` — a duplicate
handle is an `already_exists` conflict, not find-or-create) and the shared
stack persists across iterations, so a second iteration collides on every
handle. Own-stack legs are unaffected (fresh stack per iteration). T3 documents
this in the `fixture.go` package doc; if `-count>1` is ever needed, suffix
shared-attach handles with a per-run nonce.

- Interfaces (produced): `func WithOwnStack() fixtureOption`;
  `func (s *cannedModelServer) LoadScript(t *testing.T, script []CannedTurn, markers []cannedMarker)`.
- Interfaces (consumed): `newAuthedClients(caPath, serverURL, adminToken)`
  (`fixture.go:318`); per-leg factories `CreateAgent`/`Provision`
  (`agent_ops.go:22,39`).
- Test cycle: full `go test -tags podman -race ./e2e/...` — the migrated legs
  are the assertion; record the wall-time delta in the PR body.

### T4 — the embedded-postgres smoke leg

New `go/e2e/embedded_postgres_test.go` (`//go:build podman`):
`TestEmbeddedPostgresSmoke` uses `NewFixture(ctx, t, WithEmbeddedPostgres())`
— own stack, `ExternalDatabase: false`, `PostgresImage` empty (wrapper-process
arm, `stack.go:316-324`) — asserts Ready health, one authed
`GetServerInfo`, then explicit `Down` + post-Down non-Ready (the
`harness_test.go:100-115` pattern). A doc comment states its purpose: sole
dedicated e2e coverage of the supervisor-owns-postgres lifecycle +
pgid-record teardown
(DL-257/DL-259 context).

- Interfaces (produced): `func WithEmbeddedPostgres() fixtureOption`.
- Interfaces (consumed): `startPostgres` non-external arm
  (`stack.go:309-325`); `Stack.Health`/`Down` (`stack.go:183-211`).
- Test cycle: `go test -tags podman -run TestEmbeddedPostgresSmoke ./e2e/`.

### T5 — converge forge injection on the shared builder knob

Lift the pgtest-side forge mounting (`newForgeE2EWire`,
`go/server/forge_e2e_pgtest_test.go:84-115`) so both real-Postgres tiers wire
forge through one helper that mounts a caller via `hub.SetForgeCaller`
(`runnerhub/relay_forge.go:64-68`) exactly as production does
(`server/serve.go:908`). Because `newForgeService` is package-private to
`server` (`server/forge.go:165`), the helper lives in package `server` as a
pgtest-tagged export-for-test; the e2e tier documents that forge writes are
exercised at the pgtest tier only (the podman tier has no forge secrets), so
this task is a pgtest-internal dedup, not a cross-tier API.

- Interfaces (produced, package `server`, pgtest-tagged):
  `func mountForgeForTest(t *testing.T, hub *runnerhub.Hub, st *store.Store, brd *board.IssueProjection, reg *forgeProviderRegistry)`.
- Interfaces (consumed): `Hub.SetForgeCaller(ForgeCaller)`
  (`relay_forge.go:64`).
- Test cycle: `go test -tags pgtest ./server/ -run ForgeE2E`.

### T6 — document the tier pyramid

`docs/eng/testing-tiers.md` (or the sibling location `docs/designs/repo/
compass-eng-docs/` prescribes — the implementer checks that record): one page
naming the three tiers, their build tags, their cost, the decision rule
("does the assertion need real SQL? → pgtest; does it need the process/
container topology? → podman e2e; else default suite"), the shared-stack
attach vs `WithOwnStack` choice, and the embedded-postgres smoke leg's
special role. Cross-link from `go/e2e/fixture.go` and `pgtest.go` package
docs.

- Interfaces: none (prose); MUST cite the same seams this record does.
- Test cycle: markdownlint.

## Tasks

- [ ] T1 — extract `go/internal/pgshare` (acquisition + schema isolation +
      `StartSuitePostgres`); pgtest keeps its API. Owner: implement lane.
- [ ] T2 — shared stack + suite postgres in e2e `TestMain`
      (`ExternalDatabase: true`), gates run once. Owner: implement lane
      (depends T1).
- [ ] T3 — `WithOwnStack()` + leg migration + shared canned-stub
      `LoadScript`. Owner: implement lane (depends T2).
- [ ] T4 — `TestEmbeddedPostgresSmoke` + `WithEmbeddedPostgres()`. Owner:
      implement lane (depends T3).
- [ ] T5 — forge-injection convergence (`mountForgeForTest`). Owner:
      implement lane (depends T1 only).
- [ ] T6 — tier-pyramid contributor doc. Owner: implement lane (depends
      T3/T4 landing so the doc describes reality).

## Open Questions (1–2 RULED by Matt at freeze; 3–4 deferred)

1. **[RULED — Matt, freeze gate 2026-08-27: namespace isolation, short-term]
   Shared-stack isolation bar: is per-leg account/channel namespacing
   sufficient, or must legs get true per-leg database isolation?** The server
   opens ONE store on the DSN it booted with (`stack.go:256`), so shared-stack
   legs share one database and isolate by the handles they already create
   (`agent_ops.go:22`); a per-leg schema through the shared server would
   require a production change (per-session schema selection) this design
   rejects. **Ruling: namespace isolation** — the existing legs already assert
   only on their own ids, and the serial suite means no concurrent cross-leg
   writes. Matt flagged this as a short-term bridge: real per-tenant DB
   selection is owned by the managed-service multi-tenancy design (RIG-2861),
   and the harness adopts it when it lands (see the "Forward path" note under
   Alternatives). Hard isolation now would change T2/T3 materially (per-leg
   stack per group, smaller win).
2. **[RULED — Matt, freeze gate 2026-08-27: ratify the fold] Canned-script
   sharing across legs on one stub.** Today each leg's fixture owns a fresh
   stub with a positional script (`fixture.go:104-115`); on a shared stack the
   stub is long-lived and legs load scripts serially (T3's `LoadScript` +
   reset). The real interleaving hazard is NOT the supervisor's Setup turn
   (marker-routed off a separate counter at boot, `cannedmodel.go:169-184` — it
   cannot consume a leg's positional slot) but a leg-exit trailing turn: a
   marker-routed mention turn a leg triggers but observes only via
   `AwaitControlDispatch`, never `AwaitTurnSettled`
   (`legthreefour_test.go:85-86,:362-366`), can outlive the leg boundary and, if
   the next leg's reset had wiped the marker table, draw slot 0 of the next
   leg's positional script. **Ruling: the T3 fold stands** — the reset
   PRESERVES markers (harmless — body-keyed, additive) and every leg must
   `AwaitTurnSettled` each turn it triggers before returning. (The rejected
   fallback was marker-routing ALL shared-stack scripts: mechanical, more
   verbose legs.)
3. **[Non-load-bearing — deferred] Per-leg schemas through the shared
   server** (see Alternatives): revisit only if a future leg needs
   global-state isolation. The design is correct without it.
4. **[Non-load-bearing — deferred] Suite-postgres reuse by the pgtest CI
   lane.** CI already provides a service-container DSN to pgtest
   (`pgtest.go:48-50` keeps the image pinned to it); unifying the e2e suite
   postgres with that service is a CI-topology choice for the L4/CI PR, not
   this record.

Port-binding note (screened out as an OQ — answered by source): the current
serial suite already binds fixed kernel-assigned ports per fixture
(`freePorts`, `fixture.go:460-483`); the shared stack binds ONE such pair for
the whole run in TestMain, strictly fewer binds, and own-stack legs keep
per-fixture pairs — no new contention. The podman engine-lock contention the
seed-settle gate exists for (`timeouts.go:44-65`, RIG-2403) likewise shrinks:
one seed Provision per run instead of ~11. Shared-postgres lifecycle
ownership (the other candidate OQ) is answered structurally: TestMain owns it
— stood up before `m.Run()`, stopped after, exactly like the binaries dir
(`main_test.go:48-50`).
