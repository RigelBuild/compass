# Compass CI PR speed — gate the long jobs on the Go graph, cut the setup tax

> **Design record.** RIG-5325, raised from RIG-5288. Matt: "look into CI
> improvements to make that faster so a queue makes more sense, doing more
> affected gating on PR runs especially for longer time jobs, and seeing how
> we can speed up those jobs. But we're still a bit limited by GHA giving us
> runners." It extends
> [compass-ci-job-decomposition](../compass-ci-job-decomposition/design.md),
> which left `e2e` ungated. `main` pushes and the nightly schedule still run
> everything, on GitHub-hosted runners. Ledger-impact: none (CI mechanics).

## Resolved decisions

Matt ruled on 2026-10-15 (RIG-5369).

1. **Go analysis battery on the bun and nix legs: (a) scope parity per leg.**
   Those legs set `TOOLCHAIN_PARITY_SCOPE=core`, so `toolchain-parity:parity`
   skips the `goAnalysis` half and prints it as out of scope. Unset (local, the
   go leg), parity checks everything. This is the one named exception to
   "unverifiable is a failure" in `parity.ts`. It saves about 130 s on two legs
   that run on 59–60 of 60 PRs. A pushed nix cache comes later, on AKS, in its
   own record. Rejected: a nix store in the Actions cache (the repo cache is
   over its 10 GB limit), and leaving it.
2. **Post-merge backstop: (a) route red full sweeps (T7), landed before T4.**
   One job opens or updates a single `ci-backstop` issue naming the run and its
   failed jobs; a green full sweep closes it. Only that job gets
   `issues: write`. Rejected: running every main push to completion (about 118
   runner-minutes each), a full sweep on queue runs (against RIG-5288's goal),
   and accepting silent misses.

## Open Questions

All deferred and non-load-bearing; T1 data decides each.

1. **pgtest sharding (deferred).** After T3 and T5 pgtest is the long pole
   on no PR class (§5). Shard (`internal/store`, `server`, the rest) only if
   T1 shows pgtest p50 above `e2e` p50 on Go PRs.
2. **Splitting `e2e` by package (deferred).** `go/e2e` (771 s) and
   `go/cmd/compass-stack` (138 s) run serially under `-p 1`. Two jobs save
   about 140 s per Go PR for one more privileged job (about 300 s of
   setup). Decide on T1 data after T4.
3. **`flake-gate` on nearly every PR (deferred).** `flake-gate:ci` ran on
   58 of 60 PRs because it `dependsOn` `root`, holding `moon (nix)` at about
   565 s, the non-Go long pole after T4. Narrowing it changes what the flake
   gate guards: its own record.

## Problem / Intent

PR CI takes p50 1357 s and p90 2151 s of wall time (346 `ci.yml` runs,
2026-10-10 to 2026-10-12), with runner wait p50 6 s per run. The time goes
to one job that always runs and to setup every job repeats. Gate `e2e`,
narrow pgtest and microvm to the Go packages a change can reach, and cut
avoidable setup and database cost, keeping the push and schedule full
sweeps.

## Approach

### 1. Where the time goes (measured)

Per-job durations over 60 consecutive PR runs (GitHub REST, 2026-10-12);
"Ran" excludes `skipped`.

| Job | Ran | p50 s | p90 s | Long pole |
| --- | --- | --- | --- | --- |
| `e2e` | 60/60 | 1294 | 2064 | 58/60 |
| `pgtest` | 27/60 | 1071 | 1112 | 0 |
| `moon (go)` | 32/60 | 667 | 713 | 0 |
| `moon (nix)` | 59/60 | 572 | 1247 | 2 |
| `microvm` | 30/60 | 457 | 473 | 0 |
| `moon (bun)` | 60/60 | 378 | 1084 | 0 |
| `darwin` | 23/60 | 323 | 355 | 0 |

Mean wall stays within 10 s of mean `e2e` in every change class: no Go and
no image 1293 s (26 runs), Go without image 1292 s (24), image built from
the tree about 2040 s (10). Run 38306105844 changed only design docs, so
every setup flag was false, and `e2e` still took 1282 s. The rollup says
why: "`# e2e is not affected-gated — plain success required.`"
(`.github/workflows/ci.yml`, job `rollup`).

Runner wait (`started_at − created_at`, 600 runs) is p50 2 s per job; p90
is 77–177 s and p99 742–871 s for `moon`, `pgtest` and `microvm`, clustered
in 8 UTC hours. The Team plan caps hosted runners at 60 concurrent jobs
([Actions limits](https://docs.github.com/en/actions/reference/limits)),
and a Go PR starts about 10 jobs, so each skipped job also frees a slot in
bursts: the "limited by GHA giving us runners" constraint.

### 2. Gating: the Go package graph, not the moon project

`generate` in `tools/ci-matrix/index.ts` sets
`pgtestAffected = affectedSet.has(PGTEST_PROJECT)` (`"compass-go"`) and
`microvmAffected` from `compass-go` or `compass-guest-image`, so any change
in the Go module runs both whole suites. Some non-Go changes do too: a
`guest-image/agent-oci.lock`-only PR selected `compass-go:ci` through
`test-microvm`'s `deps: ['compass-guest-image:build']` and set
`pgtest=true`. A moon `inputs` glob cannot express "the packages this test
binary imports", so narrowing uses the Go graph; widening (§3) uses moon
`inputs`.

- **pgtest and microvm roots** are the packages whose test file set changes
  under the suite's tag: `go list -e -tags <tag> -f '{{.ImportPath}}
  {{.TestGoFiles}} {{.XTestGoFiles}}' ./...` against the untagged listing.
  Today:
  - pgtest, 12 roots: `internal/{auth,board,comms,delivery,gatewaycred,pgshare,runnerhub,secrets,stack/adapters,store,usage}` and `server`.
  - microvm, 4 roots: `internal/{microvmtest,runner,runtime,runtime/microvm}`.
- **`e2e` roots** are the `e2e` job's `go test` package arguments,
  `./e2e/...` and `./cmd/compass-stack/...`, asserted against `ci.yml`; the
  `podman` tag diff also yields four packages the job never runs.
  `TestMain` builds `cmd/compass-{postgres,server,runner,stack}`, so their
  `-deps` closures join through `E2E_BUILT_BINARIES`, asserted against the
  `go build` calls in `go/e2e/main_test.go` and
  `go/cmd/compass-stack/integration_podman_test.go`. The `e2e` closure is
  42 of 59 packages.
- **Closure per root:** the first-party paths in
  `go list -e -tags <tag> -test -deps ./<root>`. A changed file maps to the
  deepest package dir containing it, so `//go:embed` files, `testdata/` and
  tagged files count as changes to their package.
- **No network.** With an empty module cache and `GOPROXY=off
  GOFLAGS=-mod=mod`, the `-e` graph hashed identically to the warm one for
  all four tag sets, at about 0.1 s per closure.
- **Run only the roots.** pgtest and microvm run `go test -tags <tag> -race
  ./...` today, so about 36 untagged packages in pgtest (≈118 s) and 43 in
  microvm repeat `compass-go:test`. They run the selected roots instead:
  the affected ones on a PR, all on push or schedule.

Simulated over the 33 Go-touching PRs in the sample: a `server`-only PR
(#2106) runs 1 pgtest root (325 s) and skips microvm; a `runnerhub` PR
(#2105) runs 4; `internal/store` PRs (#2104, #2245) run 11 of 12; `go.mod`
(#2246) runs everything. `e2e` runs on every Go PR in the sample, so its
gate pays off on the non-Go 45 %.

### 3. Fail-open triggers, and how far a miss can travel

A selection that could miss widens instead, using moon task `inputs`, the
repo's source of truth for what a task reads. A suite selects all roots
when:

- **The gate itself changes:** `.github/workflows/ci.yml` or
  `tools/ci-matrix/**` (`GATE_PATHS`). Today a `ci.yml`-only PR (#2247,
  #2239) skips every gated job.
- **A `go/` path maps to no package**, for example `go/go.mod` or
  `go/moon.yml`.
- **A non-Go input of an owner task changes.** `main()` pipes the changed
  paths that map to no Go package (`residuePaths`) into
  `moon query tasks --affected` on stdin, as
  `tools/agent-image-env-gate/scheduling.test.ts` does. The suite widens
  when the result holds one of its `SUITE_OWNER_TASKS`.

| Suite | Owner tasks | Why |
| --- | --- | --- |
| all | `compass-go:test`, `toolchain-parity:parity` | The suites extend the `-race` lane and build `go` from `gate-tools.nix` `langs`. pgtest's `TestBuildBoardWebhookWiringArmedWithRealResolver` fails without the `secretspec-env.nix` CLI |
| pgtest | `compass-go:sqlc-vet` | The pgtest job runs it |
| e2e | `compass-agent-image:build`, `toolchain-parity:test` | The seed builds the image, and the job's PATH comes from `devenv.nix` |
| microvm | `compass-guest-image` affected on any path | Kept from today: guest Go code boots inside the guest, outside the microvm closure |

T4 adds `/config/profiles/**` and `/devenv.lock` to `compass-go:test`
inputs: the untagged `TestValidateConfigBundleAdmitsDefaultProfile` in
`internal/store` reads the shipped profile yet runs in no job on a
profile-only PR, and the lock pins the go-overlay the `-race` lanes build
with. A coverage test (T4) samples one file per owner-task input, so a new
input cannot silently narrow a suite. Prompt and doc edits now widen pgtest
too (0 of 60 sampled PRs touched `config/` or `docs/model-registry/`).

**The backstop is weak (decision 2).** Of 164 `push` runs in the last 1000, 148
were cancelled by the next merge (`cancel-in-progress: true` on group
`ci-${{ github.ref }}-${{ github.event_name }}`). The nightly `schedule`
run, the one full sweep that finishes, was red on 5 of its last 15
(`moon (bun)`, `moon (go)`, `e2e`, `forge` twice), started 6–9 h after its
`17 7 * * *` cron, and nothing routes its failure (no workflow holds
`issues: write`). T4 adds `e2e`, the only full-stack tier, to pgtest,
microvm and forge on it, and drops its per-PR run against the published
`:latest` agent image (`COMPASS_E2E_ALLOW_PUBLISHED_AGENT_IMAGE`) on
non-image PRs.

### 4. Make the remaining jobs cheaper

**Parity's attr parse builds tools it does not print (T2).** In
`tools/toolchain/parity.ts`, the `--print-nix-attrs` exit sits after
`const goAnalysis = nixClosedSet("goAnalysis")`. `nixClosedSet` runs
`nix eval … gate-tools.nix goAnalysis`, and `binsOf` in
`tools/toolchain/gate-tools.nix` reads `"${drv}/bin"`, which is
import-from-derivation: evaluating it builds the derivation. So the pgtest
step "Put the dev shell's nixpkgs tools on PATH (for sqlc vet)" (174–182 s)
and the `e2e` "Put the dev shell's nixpkgs tools on PATH" (169 s) spend
about 160 s building go-licenses, golangci-lint, govulncheck and nilaway,
which neither job uses (job 115013754828 built `go-licenses` 170 s into the
step). The fix moves the `--print-nix-attrs` exit to just after the
`devenvAttrs` empty-parse refusal, the only input it prints. The moon legs
built `goAnalysis` in the step before, so they do not change.

**pgtest: the service Postgres pays for durability it does not need (T3).**
CI store tests take p50 0.78 s each, native local runs 0.11–0.14 s. Locally
(all 649 `internal/store` tests passing) `-race`, a 4-CPU limit, TCP plus
SCRAM, concurrent packages and SQL time (315 statements for 3 tests in
251 ms) were each ruled out.

The pinned `postgres:16-alpine` service image, run locally, gave:

| Variant | Suite s | Test p50 s |
| --- | --- | --- |
| default | 227 | 0.34 |
| data dir on tmpfs | 151 | 0.23 |
| `fsync`, `synchronous_commit`, `full_page_writes` off | 151 | 0.23 |

The CI service log points at disk:

- 7 checkpoints, each writing 102–110 s and syncing 59 114–110 048 files;
- 502–537 MB of WAL distance per checkpoint.

Each test's schema migration (`0001_init.sql`) creates about 152 relations
and about 3.4 MB of WAL. With durability off the checkpoints still write but
no longer sync, and locally that matched tmpfs (151 s), so `max_wal_size`
and `checkpoint_timeout` are not raised: no measured gain is left. It runs
as one `docker exec … psql` step, since all three settings reload on
`SIGHUP` (`pg_reload_conf()`); the service definition does not change.
Rejected for now: a migrated template schema per test (SQL time is not the
cost) and `t.Parallel` in `internal/store` (0 of 588 tests use it, and
`migrationLockKey` in `go/internal/store/store.go` serializes them anyway).

### 5. Expected effect

The estimates combine measured step deltas; T1 verifies them. `needs:
setup` (T4) puts `setup` (63 s in run 38306105844) ahead of every `e2e`
that still runs.

| PR class (share of sample) | Today mean wall | After T2–T5 | Long pole after |
| --- | --- | --- | --- |
| no Go, no image (43 %) | 1293 s | ≈ 570 s; ≈ 440 s with T6 | `moon (nix)` (`flake-gate`) |
| Go, no image (40 %) | 1292 s | ≈ 1190 s | `e2e` (step 940 s) |
| image-affected (17 %) | ≈ 2040 s | ≈ 1940 s | `e2e` seed (678–762 s image build) |

pgtest on a `server`-only PR falls to about 350 s, and on a store PR to about
600 s, both under `e2e`, so sharding is deferred (Q3). The `devenv.lock` PRs
(3 of 60) newly widen pgtest, under the same `e2e` long pole. The remaining
Go-PR lever is the `e2e` test step itself (Q4).

### 6. Measurement (T1)

- **Source.** GitHub REST
  `GET /repos/RigelBuild/compass/actions/workflows/ci.yml/runs?created=<from>..<to>`,
  paginated, then `GET …/runs/{id}/jobs?filter=latest`. No logs: a gated
  job's skip rate is its `skipped` conclusion rate.
- **Classes.** `pr` (`pull_request`, branch not `trunk-merge/`); `queue`
  (`pull_request` on `trunk-merge/**`, since Trunk tests batches as pull
  requests and the last 1000 runs held 0 `merge_group` events);
  `merge_group` in case it appears; `push`, `schedule`, `dispatch`.
- **Runs.** Keep `success` and `failure`; count `cancelled` per class
  separately (for `push`, the backstop completion rate).
- **Metrics per class:** wall (`updated_at − run_started_at`) p50/p90;
  per-run maximum runner wait; per-job wait p50/p90/p99/max and duration
  p50/p90; long-pole counts; each gated job's `pr` skip rate; runner
  minutes; UTC hours with any job waiting over 120 s.
- **Windows.** 28 days before a task merges and 14 days after.

Baseline (2026-10-10T21:47Z to 2026-10-12T22:49Z, 346 kept runs):

| Class | n | Wall p50 | Wall p90 | Max wait p50 | Max wait p90 | Runner min p50 |
| --- | --- | --- | --- | --- | --- | --- |
| `pr` success | 290 | 1357 | 2151 | 6 | 238 | 77 |
| `pr` failure | 38 | 1353 | 2163 | 8 | 115 | 76 |
| `push` success | 7 | 1408 | 2124 | 6 | 80 | 118 |
| `dispatch` success | 7 | 2111 | 2196 | 8 | 329 | 124 |
| other (push, schedule, dispatch failures; schedule success) | 4 | — | — | — | — | — |

## Plan

### Global Constraints

- **Full sweep unchanged.** Push to `main` and `schedule` run every job over
  every root; `generate` keeps `isFullSweep`, and a unit test asserts that a
  full sweep selects all roots.
- **Paired flags.** Every new `*_affected` key is emitted by `outputLines`
  and checked in `rollup` through `require_gated`; a missing key reads `''`
  and reds the rollup.
- **The pure core stays pure.** `generate` and the selection module do no
  I/O; `go list` and `moon` run only in the edge.
- **No hand-kept package or path lists.** Roots and closures come from
  `go list`, widening from moon `inputs`. The only constants are
  `GATE_PATHS`, `E2E_ROOTS`, `E2E_BUILT_BINARIES` and `SUITE_OWNER_TASKS`,
  each asserted against the file that justifies it.
- **Toolchain.** The pinned `langs` (go 1.27.2, bun 1.4.3, moon 2.6.1);
  SHA-pinned actions; TypeScript on bun with `bun test` and biome, never
  bash.
- **`go list` runs offline and as the suites build:** `GOFLAGS=-mod=mod
  GOPROXY=off CGO_ENABLED=1`, with `-e`. A root with a non-empty `.Error`
  widens its suite; it never narrows.
- **Public repo.** Cite compass paths and bare tracker IDs only.
- **Stacks follow shared lines.** T2 → T6 (`parity.ts`) and T3 → T7 → T4 →
  T5 (`ci.yml` jobs `pgtest`, `rollup`, `e2e`); T1 stands alone.
- **One task, one PR, with a T1 report:** T1's before/after table for the
  touched jobs once 14 days have passed, the PR run's step durations until
  then.

### T1 — `tools/ci-timing`: the measurement tool

A new bun project, `tools/ci-timing/` (`moon.yml` tagged
`['bun', 'oss', 'ci-group.bun']`; tasks `typecheck`, `test` and a `ci` task
on both), a pure core with an edge like `tools/ci-matrix`.

Interfaces:

```ts
// tools/ci-timing/core.ts (pure)
export type RunClass = "pr" | "queue" | "merge_group" | "push" | "schedule" | "dispatch";
export type JobRecord = {
  name: string; conclusion: "success" | "failure" | "cancelled" | "skipped";
  createdAt: number; startedAt: number; completedAt: number; // epoch s
};
export type RunRecord = {
  id: number; event: string; headBranch: string; conclusion: "success" | "failure" | "cancelled";
  runStartedAt: number; updatedAt: number; jobs: JobRecord[];
};
export function classify(run: Pick<RunRecord, "event" | "headBranch">): RunClass;
export type Quantiles = { n: number; p50: number; p90: number; p99: number; max: number };
export type ClassSummary = {
  kept: number; cancelled: number; wall: Quantiles; maxWait: Quantiles; runnerMinutes: Quantiles;
  jobDuration: Record<string, Quantiles>; jobWait: Record<string, Quantiles>;
  skipRate: Record<string, number>; longPole: Record<string, number>;
  hoursOver120s: string[]; // "2026-10-11T00"
};
export function summarize(runs: readonly RunRecord[]): Record<RunClass, ClassSummary>;
// tools/ci-timing/index.ts (edge, `gh api --paginate`)
// bun tools/ci-timing/index.ts --since YYYY-MM-DD [--until YYYY-MM-DD] [--json]
```

Test cycle: fixture runs covering every class, cancelled exclusion, skip
rate and long pole, asserted against `summarize`.

### T2 — parity: print attrs before resolving closed sets

`tools/toolchain/parity.ts`: move the `--print-nix-attrs` block to directly
after `const devenvAttrs = parseDevenvPackages(...)` and an empty-parse
refusal for `devenvAttrs` alone. The full refusal (`langs`, `goAnalysis`,
`meissa`) stays where it is for the verdict path.

Interfaces: the CLI (`bun tools/toolchain/parity.ts [--print-nix-attrs]`)
and its stdout (a nix list literal) are unchanged.

Test cycle: with the `goAnalysis` outputs absent from the store,
`--print-nix-attrs` returns without building them. In CI, the pgtest sqlc
PATH step and the `e2e` dev-shell step each drop by about 150 s.

### T3 — pgtest: durability off on the service Postgres

`.github/workflows/ci.yml`, job `pgtest`: add a step before "Real-Postgres
suites":

```yaml
- name: Turn off durability on the throwaway service Postgres
  run: |
    docker exec "${{ job.services.postgres.id }}" psql -U postgres -v ON_ERROR_STOP=1 \
      -c "ALTER SYSTEM SET fsync = off" \
      -c "ALTER SYSTEM SET synchronous_commit = off" \
      -c "ALTER SYSTEM SET full_page_writes = off" \
      -c "SELECT pg_reload_conf()"
    docker exec "${{ job.services.postgres.id }}" psql -U postgres -tAc "SHOW fsync" | grep -qx off
```

Interfaces: none outside the job; service image, DSN and PgBouncer unchanged.

Test cycle: the step fails if `SHOW fsync` is not `off`. The PR's own pgtest
run shows the suite step against the 818–842 s baseline.

### T4 — Go-graph selection and the `e2e` gate

- **`tools/ci-matrix/go-impact.ts`** (pure) holds the types and constants
  below; **`go-graph.ts`** (edge) runs `go list` in `go/`.
- **`main()`** pipes `residuePaths` into `moon query tasks --affected` on
  stdin and passes the targets as `residueTasks`.
- **`generate`** takes `goSelection` and sets `pgtestAffected`,
  `microvmAffected` and the new `e2eAffected` from `selection.run`;
  **`outputLines`** adds `e2e_affected=`.
- **`go/moon.yml`** `test` inputs gain `/config/profiles/**` and
  `/devenv.lock` (§3).
- **`ci.yml` job `e2e`** gains `needs: setup` and the `pgtest`-shaped `if:`
  `(github.event_name != 'pull_request' || needs.setup.outputs.e2e_affected == 'true')`;
  **`rollup`** replaces the "e2e is not affected-gated" block with
  `require_gated e2e "$e2e" "$e2e_flag"`. The setup log prints each
  suite's `reason`.

Interfaces:

```ts
// tools/ci-matrix/go-impact.ts
export type GoSuite = "pgtest" | "microvm" | "e2e";
export type GoGraph = {
  /** Module-relative dirs of every package in `go list ./...`. */
  packages: readonly string[];
  /** Per suite, per root: module-relative dirs in `go list -e -tags <tag> -test -deps ./<root>`. */
  closure: Readonly<Record<GoSuite, Readonly<Record<string, readonly string[]>>>>;
  /** Roots whose `go list` entry carried a non-empty .Error (forces that suite wide). */
  erroredRoots: Readonly<Record<GoSuite, readonly string[]>>;
};
export type SuiteSelection = {
  run: boolean;
  reason: string; // "wide: go/go.sum", "owner: compass-go:test", "closure: internal/store", "none"
  roots: readonly string[]; // module-relative, sorted; every root when wide
};
export const GATE_PATHS: readonly string[]; // .github/workflows/ci.yml, tools/ci-matrix/, .moon/
export const E2E_ROOTS: readonly string[]; // e2e, cmd/compass-stack
export const E2E_BUILT_BINARIES: readonly string[]; // cmd/compass-{postgres,server,runner,stack}
export const SUITE_OWNER_TASKS: Readonly<Record<GoSuite, readonly string[]>>; // §3 table
/** Changed paths that map to no Go package. */
export function residuePaths(changedPaths: readonly string[], graph: GoGraph): string[];
export function selectGoSuites(input: {
  event: GenInput["event"];
  changedPaths: readonly string[];
  affectedIds: readonly string[]; // for compass-guest-image
  residueTasks: readonly string[]; // `moon query tasks --affected` over residuePaths
  graph: GoGraph;
}): Record<GoSuite, SuiteSelection>;
// tools/ci-matrix/go-graph.ts
export async function loadGoGraph(goDir: string): Promise<GoGraph>;
```

Test cycle (red first):

- Fixtures from the sample: #2106 selects pgtest `[server]`, #2104 11
  roots, #2218 (`devenv.lock` only) every root; a design-only diff leaves
  `e2e` `run:false`; a `ci.yml`-only diff selects everything.
- **Coverage** (real moon and `go list`, the `sampleFiles` method of
  `tools/agent-image-env-gate/scheduling.test.ts`): one file per input of
  every `SUITE_OWNER_TASKS` target maps to a Go package or widens its suite.
- **Escapes:** every `".."`-prefixed string literal in a root's tagged test
  files resolves inside that root's closure or an owner-task input.
- `E2E_ROOTS` equals the `e2e` job's `go test` package arguments in
  `ci.yml`; each `E2E_BUILT_BINARIES` entry appears in a `go build` call.
- A push or schedule selects every root; an errored root widens.

### T5 — pgtest and microvm run the selected roots

- **`outputLines`** adds `pgtest_packages=` and `microvm_packages=`, JSON
  arrays of `./<root>` from `selectGoSuites`.
- **`ci.yml`** replaces `./...` in "Real-Postgres suites" and "microVM
  suites" with
  `${{ join(fromJSON(needs.setup.outputs.pgtest_packages), ' ') }}` (and the
  microvm equivalent).
- **The two "Assert … ran rather than skipped" steps** require `ok` from
  every *selected* package that calls `pgtest.RequireDSN` or
  `microvmtest.Require`, and fail on an empty selection; the skip-text check
  is unchanged.
- **The microvm step** drops its `secretspec` staging when no selected root
  imports `internal/secrets`; its comment says `./...` is the only reason.

Interfaces: setup outputs `pgtest_packages` and `microvm_packages` (JSON
`string[]`, never empty when the paired flag is `true`).

Test cycle: the generator emits all 12 and 4 roots on push; in CI a
`server`-only PR's pgtest log shows only `server`, and the assertion step
reports 1 package.

### T6 — Go analysis battery only where it runs (decision 1)

- **`ci.yml` job `moon`:** "Put the Go analysis battery on PATH" gains
  `matrix.group == 'go'`, and the job sets `TOOLCHAIN_PARITY_SCOPE: core`
  when `matrix.group != 'go'`.
- **`tools/toolchain/parity.ts`:** under `TOOLCHAIN_PARITY_SCOPE=core`, skip
  `nixClosedSet("goAnalysis")` and its verdicts and print one
  `out of scope: goAnalysis (TOOLCHAIN_PARITY_SCOPE=core)` line; any other
  non-empty value exits 1.

Interfaces: env `TOOLCHAIN_PARITY_SCOPE` is unset or `core`;
`parity-core.ts` is unchanged.

Test cycle: a `parity-core` test on the scope parse. In CI, `moon (bun)` and
`moon (nix)` drop by about 130 s and the go leg is unchanged.

### T7 — route a red full sweep to an issue (decision 2, before T4)

- **`ci.yml`** adds job `backstop`: `needs: rollup`, `if: ${{ !cancelled()
  && contains(fromJSON('["push","schedule"]'), github.event_name) }}`, job
  `permissions: { contents: read, issues: write }`, the pinned bun step of
  `dl-reconcile.yml`, then `bun tools/ci-backstop/index.ts` with
  `ROLLUP_RESULT: ${{ needs.rollup.result }}`. A cancelled (superseded)
  push never touches the issue. The job sets `concurrency: { group:
  ci-backstop, cancel-in-progress: false }`, so push and schedule runs
  never edit the issue at once. The core also gets the newest finished
  full-sweep run's `run_started_at`. If that run is newer than this one,
  the action is `none`, so an older red run cannot reopen an issue a
  newer green run closed.
- **`tools/ci-backstop/`**, a bun project like T1. The edge lists the run's
  failed jobs and the open `ci-backstop` issue through `gh`, creating the
  label with `gh label create --force`, and applies the core's action.

Interfaces:

```ts
// tools/ci-backstop/core.ts (pure)
export type BackstopAction =
  | { kind: "open"; title: string; body: string }
  | { kind: "comment" | "close"; issue: number; body: string }
  | { kind: "none" };
export function backstopAction(input: {
  result: "success" | "failure"; event: "push" | "schedule"; runUrl: string;
  failedJobs: readonly string[]; openIssue: number | null;
  runStartedAt: string; newestSweepStartedAt: string;
}): BackstopAction;
```

Test cycle: a unit table over result × open issue (red with none opens, red
with one comments, green with one closes, green with none does nothing;
any result with a newer finished sweep does nothing),
and `--dry-run` against the live repo prints the action.

## Tasks

- [ ] T1 — `tools/ci-timing`: wall, wait, duration, skip rate and long pole
  per class; the 28-day baseline in the PR body.
- [ ] T2 — `parity.ts`: `--print-nix-attrs` exits before resolving the
  closed sets.
- [ ] T3 — pgtest: durability off on the service Postgres, asserted with
  `SHOW fsync`.
- [ ] T7 — a red push or schedule sweep opens or updates the `ci-backstop`
  issue; a green one closes it (decision 2, before T4).
- [ ] T4 — Go-graph selection, moon-derived widening, the `e2e` gate and
  `require_gated e2e` (after T7).
- [ ] T5 — pgtest and microvm run only the selected roots.
- [ ] T6 — the Go analysis battery on the go leg only (decision 1).
