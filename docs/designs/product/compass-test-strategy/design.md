# Compass full-system test strategy

Status: Draft

> **Design record** — ported from the frozen orion record (RigelBuild/orion
> PR #740) into compass, with Matt-ruled amendments grounding it against the
> shipped compass CI. Ledgered as DL-174..DL-181.

## Problem / Intent

The Compass backend is a Go module (`go/`): a Postgres store of record, a
comms vertical, a runner seam, an authenticated TLS network door, and a board
projection — every tier reading and writing structured state through the
Server. Coverage grew per-PR; this record makes it a designed contract: which
harness lives at each level, what each level's oracle is, how the live
Postgres dependency is provisioned in CI, and where the whole-flow seam sits.
Most of the strategy is now SHIPPED in compass — 61 `//go:build pgtest` files
run inline in the gating CI job against a Postgres service container — so this
port's job is to ratify the shipped shape as decisions and name the three
remaining unexecuted deliverables: in-harness require-live teeth, one thin
`runner.Dial`-through-the-production-door whole-flow test, and the bun
client↔server contract lane (a follow-up owned conceptually by the
dogfood-e2e harness record).

**Provenance and amendments.** This is a port of the frozen orion record
(RigelBuild/orion PR #740), which designed the strategy against orion's
Woodpecker/moon CI before compass forked. Two of its decisions are amended by
Matt's ruling to match compass's shipped reality (see D-A1/D-A2 below): the
orion record mandated a separate `compass-go:test-pg` CI lane — compass runs
the pgtest suites **inline in the one existing CI job**, with no extra job —
and the orion record provisioned CI Postgres as a per-step embedded
`postgresql_18` postmaster on a Unix socket — compass ships a digest-pinned
`postgres:16-alpine` **service container** with a TCP DSN. This record is a
**different scope** from the compass-dogfood-e2e harness
(`docs/designs/platform/compass-dogfood-e2e/design.md`): that is a full-stack
scenario harness (real containers, real agent turns); this is coverage wiring
and contract testing — the layered pyramid underneath it.

## Approach

### The current test surface, layer by layer (grounded on compass `main`)

**The default gate is hermetic.** The Go module's moon `test` task is
`go test -race ./...` with `CGO_ENABLED: '1'` and no `-tags`
(`go/moon.yml:138-141`); the `ci` aggregate is
`[fmt, vet, lint, nilaway, test, build, vuln, licenses]` (`go/moon.yml:176`).
Neither the `pgtest` nor the `podman` suites compile into it.

**The live-Postgres surface runs inline in CI — shipped.** Unlike the orion
ancestor (where no CI lane ran the pgtest suites at all), compass CI already
runs them: `.github/workflows/ci.yml` attaches a Postgres **service
container** to the one `CI` job (`ci.yml:108-127`) and, after the moon
battery, a folded-in step runs
`go test -tags pgtest -race -v -timeout 15m ./...` with
`COMPASS_TEST_DATABASE_DSN: postgres://postgres:compass-test@127.0.0.1:5432/compass?sslmode=disable`
(`ci.yml:295-316`). The workflow header records the fold explicitly: *"pgtest
… runs as a step in the same `CI` job, after the moon battery, against a
Postgres service container attached to the job. It was once a separate job …
folded in so there is one required check"* (`ci.yml:38-44`). A follow-on step
asserts the suites actually ran rather than skipped — it greps the harness's
own skip text out of `pgtest.go` and requires an `ok` line for every package
that calls `pgtest.RequireDSN` (`ci.yml:318-360`).

**The pgtest population.** 61 `//go:build pgtest` files on `main`:
`internal/store` (26), `server/` (15), `internal/comms` (12), `internal/auth`
(4), `internal/pgtest` (the harness, 2), `internal/board` (1),
`internal/runnerhub` (1, the T4 whole-seam integration test).

**The harness (`go/internal/pgtest/pgtest.go`)** — the one shared copy of
database acquisition, build-tagged `pgtest`, importable (non-`_test.go`).
`RequireDSN(t)` (`pgtest.go:97`) takes `COMPASS_TEST_DATABASE_DSN` when set,
giving each test its own uniquely-named schema on the shared database;
with no DSN it **skips** when no container runtime exists (`pgtest.go:105`)
and **fails loudly** when a runtime is present but the ~500x-slower container
path wasn't opted into via `COMPASS_TEST_USE_CONTAINER`
(`pgtest.go:106-108`, policy in `decideDSNSource`, `pgtest.go:74-85`). The
image pin `postgres:16-alpine@sha256:57c72…` (`pgtest.go:50`) must stay equal
to the CI service image — the file says so (`pgtest.go:48-49`), and the
workflow repeats it (`ci.yml:110-118`).

**Comms (`internal/comms`)** — the differential-oracle pattern this record
generalizes is already in place: `subscribe_failclosed_test.go` is the
untagged in-memory reference (fail-closed visibility with no database), and
the twelve `pgtest` files prove the real store obeys the same contract
end-to-end over a real connect server-stream.

**Server tier (`server/`)** — fifteen `pgtest` files drive the production
serving path: the authenticated TLS network door shipped in #747
(`ServeConfig{TLS, Listen, DatabaseDSN}`, `buildNetworkServer` at
`go/server/network_door.go:236` mounting CompassService + CommsService behind
the bearer + admin-gate chain, and the internal `RunnerService` door behind
its own Runner-kind bearer interceptor, `network_door.go:285-296`).
`comms_actor_pgtest_test.go` already composes served-door + comms over real
TLS; `lifecycle_e2e_pgtest_test.go` drives the whole agent-initiated
spawn/despawn wire against a real Postgres and a real Runner over a stub
engine.

**Runner seam (`internal/runnerhub`)** — shipped in #816:
`integration_pgtest_test.go` drives `hub.Provision → Sessions relay → real
Runner (runner.RunSessions) → AgentRuntime.Launch → hub.Deliver → real
store + bus`. But it mounts `RunnerService` on its **own** `httptest` h2c server,
not through `Serve`/`buildNetworkServer`
(`integration_pgtest_test.go:104-113` dials cleartext h2c), and the
server-tier `lifecycle_e2e_pgtest_test.go` does the same
(`mountRunnerServerE2E`, `lifecycle_e2e_pgtest_test.go:520-531`). So the
runner leg is proven end to end — but **no suite on `main` dials
`runner.Dial` through the production TLS door**. That is the one uncomposed
seam (S2).

**Container lifecycle (`//go:build podman`)** — `internal/runtime`'s
lifecycle/config/secrets suites plus the whole `go/e2e/` dogfood harness
(legs 2-6, its own frozen record) are podman-tagged, skip-if-absent,
developer-run. Their CI promotion is the dogfood/infra lane's concern, not
this record's (D4).

**Client (TS)** — `packages/compass-client` has typecheck + `bun test`
(`packages/compass-client/moon.yml:15-16`) against mocks; `apps/ui/src/*`
carries frame-level transport tests (`daemon-transport.test.ts`). Nothing
drives the generated TS client against a live Go server — the contract seam
between the two generated stubs is untested over a real wire (S3, a
follow-up).

### The shape: a differential-oracle pyramid, Postgres as the one live dependency

A deterministic in-memory/fake reference proves each contract cheaply, and a
live `pgtest` suite proves the real Postgres backend obeys the **same**
contract. Fidelity concentrates at the seams; one thin whole-flow test
composes the served seams; full browser e2e is rejected (D5).

| Level | Harness | Oracle | Live dep | Gate |
| --- | --- | --- | --- | --- |
| Unit / contract | `go test -race` (untagged + `unix`), `bun test` | in-memory doubles / fakes | none | moon `test` (`go/moon.yml:138`) — shipped |
| Live-backend integration | `//go:build pgtest` | the same contract, differentially, against the store | Postgres | **inline step in the existing CI job** (`ci.yml:295-316`) — shipped |
| Whole-flow | `//go:build pgtest` | one scripted flow composing the served seams, event-gated | Postgres | same inline step — **S2 unexecuted** |
| Container lifecycle / full-stack scenario | `//go:build podman` (incl. `go/e2e/`) | production Launch path + dogfood legs | rootless podman | skip-if-absent; owned by the dogfood-e2e record (D4) |
| Client transport contract | `bun test` vs a live Go server binary | generated `compass.v1` TS client ↔ real server | server binary + Postgres | **S3 unexecuted — follow-up, fold toward dogfood-e2e** |

### A1 — One CI job, pgtest inline: no separate lane (amends orion D2/DL-049)

**Matt's ruling, verbatim: "trimmed but no extra CI job, it goes in the same
existing job."** The orion record's D2 mandated a separate `compass-go:test-pg`
task beside `test`, landing outside the `ci` aggregate's deps until two
then-red suites closed. Compass took the other fork and it is shipped: the
pgtest step runs **inline in the one `CI` job**, after the moon battery
(`ci.yml:295-316`), and the workflow's own header documents why — it *was*
once a separate job, and was folded in *"so there is one required check, at
the cost that a service-container flake now reds `CI`"* (`ci.yml:38-44`).
That tradeoff is accepted and ratified here (DL-175). What the orion D2 was
really protecting survives intact at a different layer: the **hermetic moon
`test` task stays dependency-free** (a machine with no Postgres still runs
`moon run compass-go:test` green; the tag keeps the suites out of that gate),
and failure attribution comes from the step boundary inside the job rather
than a job boundary. No `test-pg` moon task is created — the inline workflow
step owns the lane.

**The gap that remains is teeth, not wiring.** The harness's no-DSN
no-runtime path is `t.Skip` (`pgtest.go:105`) — correct for a container-less
sandbox, silent in CI. Compass already guards this at the **workflow** level:
the assert step fails the job if the skip text appears in the log or any
RequireDSN-calling package fails to report `ok` (`ci.yml:318-360`). That
guard is real but lives outside the test binary — it cannot protect a local
`-tags pgtest` run, and it is string-coupled to the harness (it self-checks
that coupling, `ci.yml:335-340`). S1 adds the in-harness half: a
`COMPASS_REQUIRE_LIVE=1` mode that turns the skip path into `t.Fatal` at the
one seam every suite passes through (`RequireDSN`), set by the CI step env.
The workflow guard stays — two independent teeth, one contract.

### A2 — Postgres provisioning in CI: the shipped service container (amends orion D4)

**Decision, not open question — this is ground truth.** The orion record
ruled a per-step **embedded `postgresql_18` postmaster** on a Unix socket
(nix-vendored binary, `initdb` a throwaway datadir, socket-only DSN in URL
form), because orion's Woodpecker step image had no container CLI and no
service-step concept. Compass CI is GitHub Actions, which has exactly that
concept, and the shipped shape uses it: a `services:` **Postgres service
container**, digest-pinned —
`postgres:16-alpine@sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777`
(`ci.yml:119`) — with `POSTGRES_DB: compass`, a health-gated start
(`pg_isready`, `ci.yml:124-127`), and a TCP DSN exported to the step
(`ci.yml:301`). The pin is by digest deliberately (`16-alpine` is a mutable
tag) and must stay equal to `pgtest.go`'s `pgImage` (`pgtest.go:42-50`) so CI
and a local container run exercise byte-identical Postgres. The entire
embedded-postmaster apparatus the orion record designed — the socket-only
`sun_path` budget, the URL-form-DSN-or-schema-isolation-breaks analysis, the
`initdb -U postgres` provisioning flags, `max_connections` measurement — is
**not ported**: it solved a Woodpecker constraint compass does not have. What
survives of it is the two invariants the analysis protected, both already
held by the shipped shape: the DSN is URL-form (so the harness's query-string
`search_path` threading works unchanged), and version parity is pinned
(digest-equal image on both sides, at major **16**, not the orion record's
18 — the parity mechanism matters, the major is whatever both sides pin).

### A3 — The whole-flow level: compose the already-built seams; no browser e2e

Everything a from-scratch smoke would build has shipped: the network accept +
served bearer chain (`network_door_test.go`, real TLS accept on a real
loopback port), a served comms round-trip (`comms_actor_pgtest_test.go`), the
full runner flow (`integration_pgtest_test.go`), and the whole spawn/despawn
wire (`lifecycle_e2e_pgtest_test.go`). The one seam no suite composes is
`runner.Dial` → the **production** `RunnerService` door built by
`buildNetworkServer` (Runner-kind bearer gate, `network_door.go:285-296`):
both existing runner-dialing suites mount `RunnerService` on their own
`httptest` h2c servers and dial cleartext. S2 closes exactly that seam and
nothing else — it does not re-assert the served comms round-trip or the plain
TLS accept.

Browser (Playwright/WebDriver) e2e stays rejected: the UI transport adapter
is contract-tested at the frame level (`apps/ui/src/daemon-transport.test.ts`),
the client factory is generated code (proven by S3 once it lands), and a
browser adds the largest flake+infra surface for coverage the layered seams
already compose. Full-stack *scenario* coverage — real containers, real agent
turns, UI-inclusive tiers — is the dogfood-e2e record's charter, not a gap
here.

### A4 — Client-transport contract lane: a follow-up owned with dogfood-e2e

The orion F5 lane — a `bun test` suite spawning the real `compass-server`
binary and driving the generated TS client against it over a loopback
endpoint — remains a real gap (mock-fetch on the TS side, in-process servers
on the Go side; the two generated stubs never meet a real wire in any gate).
It is kept in this record's Plan as S3 **as a scoping note, not a build
task here**: conceptually it belongs with the compass-dogfood-e2e harness
(`docs/designs/platform/compass-dogfood-e2e/design.md`), which already owns
full-stack bring-up, authenticated Connect clients against a served door, and
a parked UI-inclusive tier (its H7/OQ3). Standing up a second
spawn-a-real-server fixture under `packages/compass-client` would duplicate
that bring-up. S3 therefore files the lane as a new feature against the
dogfood-e2e scope (a follow-up issue), rather than landing a parallel
fixture from this record. This record does not edit the dogfood-e2e record.

### A5 — Record placement: standalone product record

`docs/designs/product/compass-test-strategy/design.md` — cross-cutting over
the verticals and the gate contract, directory-form like its siblings.
Records freeze on merge; this one supersedes-by-citation and rewrites none.
It supersedes its own orion ancestor in the two amended decisions (D-A1,
D-A2).

## Global Constraints

- **Gate parity.** The Go test gate is `CGO_ENABLED=1 go test -race`
  (`go/moon.yml:138-141`), and the inline pgtest step keeps `-race` and CGO on
  (`ci.yml:302,314`). Every test this record adds keeps both.
- **One required check.** The pgtest step runs inline in the existing `CI`
  job (`ci.yml:38-44`); nothing in this record adds a job, a moon task, or a
  second required check.
- **Image-pin parity.** `pgtest.go`'s `pgImage` digest and the CI service
  image digest must stay equal (`pgtest.go:48-49`, `ci.yml:113-118`); any bump
  changes both in one PR.
- **No flaky-retry escape hatch.** A live-dependency test that flakes is a
  real bug — fix by event-gating or controlled time, never a retry knob.
  Every wait in every harness is event-gated (channel receives / readiness
  probes), bounded by a safety-net timeout that fails, never synchronizes.
- **Hermetic default stays hermetic.** `moon run compass-go:test` must remain
  green on a machine with no Postgres and no container runtime; the `pgtest`
  build tag is the mechanism, and no test this record adds may leak a live
  dependency into the untagged gate.
- **Harness policy lives in the harness.** Database-acquisition policy has
  one copy (`internal/pgtest`); the require-live teeth land inside
  `RequireDSN`, never at call sites, so every current and future pgtest suite
  inherits them.
- **Build-tag taxonomy.** untagged / `//go:build unix` = hermetic, runs in
  moon `test`; `pgtest` = live Postgres, runs in the inline CI step;
  `podman` = rootless podman (runtime lifecycle + the `go/e2e/` dogfood
  harness), developer-run / dogfood-lane-owned, never in this record's gate.
- **Frozen-record convention.** Records freeze on merge; this record
  supersedes-by-citation only. Ledger rows DL-174..DL-181 land in
  `docs/designs/product/DECISIONS.md` in the same PR.
- **markdownlint-clean** per the repo root config.

## Plan

Sequenced by dependency. S1-S3 are the only unexecuted deliverables; the rest
of the strategy is shipped and ratified by the Decisions below.

### S1 — Require-live teeth in the harness

Make CI's expectation enforceable **inside the test binary**: when the live
lane is demanded, an unreached database is a failure, not a silent skip. The
workflow-level assert step (`ci.yml:318-360`) already guards the CI log; this
adds the in-harness half so the contract holds anywhere the env var is set
(including local reproduction of CI), and the two guards fail independently.

*Interfaces:*

- Consumes: `pgtest.RequireDSN` (`go/internal/pgtest/pgtest.go:97-114`) and
  its `decideDSNSource` policy split (`pgtest.go:74-85`); the CI step env
  block (`ci.yml:297-302`).
- Produces:
  - A new exported const `RequireLiveEnvVar = "COMPASS_REQUIRE_LIVE"` beside
    `DSNEnvVar` (`pgtest.go:54`), and a check in `RequireDSN`: when set to a
    non-empty value, the `sourceSkipNoRuntime` arm becomes
    `t.Fatal("COMPASS_REQUIRE_LIVE is set but no COMPASS_TEST_DATABASE_DSN
    …")` instead of `t.Skip` (`pgtest.go:104-105`). The
    `sourceFailMisconfigured` arm already fails loudly and is unchanged.
  - `decideDSNSource` grows the require-live input so the policy stays
    unit-testable without a real Postgres (extend the existing untagged
    policy test beside it).
  - `COMPASS_REQUIRE_LIVE: '1'` added to the CI pgtest step env
    (`ci.yml:297-302`).
  - A taxonomy note in `go/AGENTS.md` (created if absent): untagged/`unix` =
    hermetic; `pgtest` = live Postgres, inline in CI; `podman` =
    developer-run / dogfood lane.
- Gate: locally, `COMPASS_REQUIRE_LIVE=1 go test -tags pgtest ./internal/store/`
  with no DSN and no runtime fails loudly (verified once by unsetting the
  DSN); with the DSN present, green; plain `go test ./...` unaffected; the CI
  step stays green with the new env var set.

### S2 — The whole-flow composing test: `runner.Dial` through the production door

One `//go:build pgtest && unix` test in package `server/` closing the single
uncomposed seam: a real Runner dialing the **production** `RunnerService`
door built by `buildNetworkServer` over TLS with a real Runner-kind token —
the path both existing runner suites bypass with their own h2c `httptest`
servers.

*Interfaces:*

- Consumes: `server.Serve` with `ServeConfig{TLS, Listen, DatabaseDSN}` and
  the served Runner door (`go/server/network_door.go:236,285-296` — Runner
  bearer interceptor via `auth.ResolveToken` Kind-gated to `SubjectRunner`);
  the in-test TLS + serve helpers already in package `server`
  (`freeLoopbackAddr` `network_door_test.go:72`, `writeSelfSignedCert` `:92`,
  `serveInBackground`/`waitServing` `:156+`); `pgtest.RequireDSN`;
  `runnerhub.MintRunnerToken` (`go/internal/runnerhub/mint.go:103` — mints
  and stores the SubjectRunner hash in one call); `runner.Dial` with a
  caller-supplied `*http.Client` on `RunnerConfig` (transport-agnostic — the
  existing suites inject h2c clients the same way,
  `lifecycle_e2e_pgtest_test.go:526-531`).
- Produces: one test that (1) boots the real network door against a real
  Postgres (`Serve` with `Listen` + `TLS` set), (2) mints a real Runner token
  into the live store via `runnerhub.MintRunnerToken`, (3) dials
  `runner.Dial` with a TLS-configured `*http.Client` trusting the in-test
  cert pool (note `newTLSClient` `network_door_test.go:143` returns a connect
  client, not an `*http.Client` — build the TLS transport directly or
  refactor its transport half out as a shared helper), and (4) asserts enroll
  succeeds and one provision/deliver round-trip commits to the real store.
  Negative arm: an **account** token on the same dial is `Unauthenticated`
  (the Kind gate, `network_door.go:229-231`). Every wait event-gated. It does
  NOT re-assert the served comms round-trip (`comms_actor_pgtest_test.go`)
  or the plain TLS accept (`network_door_test.go`).
- Gate: the inline CI pgtest step (`-tags pgtest ./...`) reaches it
  automatically — no wiring change; it reds if the Runner-kind bearer gate or
  the runner leg through the served door regresses.

### S3 — Bun client↔server contract lane: file toward dogfood-e2e

The generated-TS-client-vs-live-Go-server contract seam, scoped and filed —
not built here.

*Interfaces:*

- Consumes: the gap statement (A4); the dogfood-e2e record's harness core
  (`docs/designs/platform/compass-dogfood-e2e/design.md` — H1 fixture,
  authenticated Connect clients over the served door, parked UI tier
  H7/OQ3).
- Produces: a tracked issue against the dogfood-e2e scope: "bun contract
  lane — drive the generated `packages/compass-client` factories against the
  harness's live served door (GetServerInfo round-trip, SubscribeEvents
  snapshot→tail)", noting it consumes the H1 fixture's door URL + token
  rather than spawning its own server, and that its gate placement (per-PR vs
  on-demand) is that record's call. This record does not edit the dogfood-e2e
  record.
- Gate: the issue exists and links both records; no code lands here.

## Tasks

Shipped already on compass `main` (ratified, no work):

- [x] Live-Postgres CI lane — 61 `pgtest` files run inline in the `CI` job
      against the digest-pinned service container (`ci.yml:108-127,295-316`).
- [x] Workflow-level skip guard — the assert step reds the job if the harness
      skipped or any RequireDSN-calling package missed `ok` (`ci.yml:318-360`).
- [x] TLS network door + served interceptor chain (#747) and its seven-plus
      server-tier pgtest suites.
- [x] Runner seam whole-path integration (#816,
      `internal/runnerhub/integration_pgtest_test.go`) and the spawn/despawn
      whole-wire e2e (`server/lifecycle_e2e_pgtest_test.go`).

Unexecuted (the deliverables of this record):

- [ ] **S1 — require-live teeth** — `COMPASS_REQUIRE_LIVE=1` turns
      `RequireDSN`'s skip path into `t.Fatal`; set in the existing CI step
      env; taxonomy note in `go/AGENTS.md`.
- [ ] **S2 — whole-flow composing test** — `runner.Dial` over TLS with a real
      `MintRunnerToken` credential through the production `RunnerService`
      door: the one seam no suite on `main` composes.
- [ ] **S3 — bun contract lane** — filed as a follow-up feature against the
      compass-dogfood-e2e harness scope; not built from this record.

## Decisions

Ledgered as DL-174..DL-181 in `docs/designs/product/DECISIONS.md`. D-A1 and
D-A2 are the Matt-ruled amendments overriding the orion ancestor's D2/D4.

- **D-A1 (DL-175) — pgtest runs INLINE in the existing CI job; no separate
  lane.** Matt, verbatim: *"trimmed but no extra CI job, it goes in the same
  existing job."* Overrules the orion record's separate `test-pg` task/lane.
  The shipped shape (`ci.yml:38-44,295-316`) is ratified: one required check;
  a service-container flake reds `CI` (accepted); the hermetic moon `test`
  task stays dependency-free via the build tag; the remaining work is teeth
  (S1), not wiring.
- **D-A2 (DL-176) — CI Postgres is the shipped SERVICE CONTAINER, not an
  embedded postmaster.** Overrules the orion record's per-step
  `postgresql_18`-postmaster-on-a-Unix-socket ruling, which solved a
  Woodpecker-step constraint (no container CLI, no service steps) that
  GitHub Actions does not have. Shipped: `services: postgres` digest-pinned
  `16-alpine`, health-gated, TCP URL-form DSN (`ci.yml:108-127,301`). The
  two invariants the orion analysis protected survive: URL-form DSN (the
  harness's query-string `search_path` threading) and digest-equal
  image parity with `pgtest.go:50`.
- **D1 (DL-174) — the differential-oracle pyramid.** Every seam carries a
  hermetic in-memory reference in the default gate and a `pgtest` suite
  proving the real Postgres backend obeys the same contract; Postgres is the
  one live dependency the strategy gates on; fidelity concentrates at the
  seams, composed once by a thin whole-flow test.
- **D2 (DL-177) — require-live teeth live in the harness.** One policy point
  (`RequireDSN`), env-keyed (`COMPASS_REQUIRE_LIVE`), turning the skip path
  into a failure wherever the live lane is demanded — layered with, not
  replacing, the workflow-level assert step (S1).
- **D3 (DL-178) — whole-flow scope: one thin composing test.** `runner.Dial`
  → the production `RunnerService` door (Runner-kind bearer gate) is the one
  uncomposed seam; S2 closes it and nothing else. A full smoke rebuild was
  rejected as re-describing shipped work (carried from the orion D5 ruling).
- **D4 (DL-179) — podman/full-stack lanes stay out of this gate.** The
  `podman`-tagged surface (runtime lifecycle, `go/e2e/` dogfood legs) is
  skip-if-absent, developer-run, owned by the dogfood-e2e/infra lane; its CI
  promotion is not this record's scope (carried from the orion D1).
- **D5 (DL-180) — no browser e2e.** The whole-flow level is a Go/`pgtest`
  composing test; the UI transport is contract-tested at the frame level and
  the client factory over the S3 lane once it lands (carried from the orion
  D3).
- **D6 (DL-181) — the bun contract lane folds toward dogfood-e2e.** The
  TS-client↔Go-server contract lane is a real gap but consumes the dogfood
  harness's bring-up rather than duplicating it; it is filed as a follow-up
  feature against that record's scope (S3), not built from this one.

## Resolved decisions

The orion ancestor's open questions do not carry: its OQ1/OQ2 were folded
into rulings there, and its OQ3 (podman deferral) is restated here as D4 with
the dogfood-e2e record as the owner. No open questions remain — the two
forks this port faced (inline-vs-separate job, service-container-vs-embedded
Postgres) were ruled by Matt and are recorded as D-A1/D-A2.
