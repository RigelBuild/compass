# Compass forge integration testing

Status: Active
Follow-on to: [forge write path](../compass-forge-write-path/design.md) (RIG-2170, frozen) — this record ADDS the live-contract test tiers; it does not re-open that record.

## Problem / Intent

Every forge test in the tree is hermetic — `go/internal/forge/github_test.go`
drives the client through a stub `http.RoundTripper` ("`scriptedRoundTripper`
serves a queue of scripted responses (or a per-call func), recording each
request for assertions", `github_test.go:26-27`), and the write-path record's
frozen strategy is "differential-oracle pyramid per DL-174 (hermetic in-memory
reference — `FakeProvider` + `httptest`-backed clients — in the default gate"
(`compass-forge-write-path/design.md:136-139`). Hermetic tests prove our code
matches *the contract we believe GitHub has*; nothing proves we modeled that
contract correctly, and nothing detects GitHub drifting out from under our
model (field shapes, 422 bodies, the F1 reviewer≠author assumption). This
record adds the two missing tiers — a committed golden-fixture replay leg and
a live-credentials oracle leg — and settles where the oracle's credentials
live in CI.

## Approach

Matt ruled the two-part decision (2026-08-18); this record captures and
specifies it — it does not re-decide it.

### Leg 1 — hermetic golden-fixture replay (proves OUR code matches the captured contract)

A plain, untagged Go test in `go/internal/forge` replaying committed GitHub
response fixtures from a new `go/internal/forge/testdata/` directory (the
directory does not exist today — the package holds only ten `.go` files, no
`testdata/`) through the existing `scriptedRoundTripper` stub. Zero network,
zero credentials. Because it is untagged, it rides the `compass-go` battery:
moon's affected detection schedules it on every forge-affected PR AND the
full main + nightly sweep (`.github/workflows/ci.yml:25-26` "A pull request runs
`moon ci :ci`, which runs only the projects the PR affects"; the nightly
full-sweep trigger at `ci.yml:62-66`). **No `.github/workflows/ci.yml` edit
and no YAML enumeration** — the one-job rationale explicitly rejects "a
project list in a workflow is a second source of truth for something
`.moon/workspace.yml` already owns. That list goes stale silently"
(`ci.yml:9-10`), and leg 1 needs none: the new `_test.go` file is covered by
the `test` task's `**/*.go` input glob (`go/moon.yml:51-54` `&go_sources` —
`'**/*.go'`, `'go.mod'`, `'go.sum'`; consumed by `test` at
`go/moon.yml:145-155`).

One grounded correction to the "no moon.yml edit" assumption: the fixture
FILES are JSON, and `**/*.go` does not match them — a fixture-only change
(a `-update` regeneration commit) would schedule nothing on the affected-PR
path. T1 therefore adds `'**/testdata/**'` to the `&go_sources` anchor
(`go/moon.yml:51-54`). That is an *inputs-glob* edit in the project's own
moon file — the sanctioned single source of truth the one-job rationale
defends — not the banned workflow-YAML project enumeration.

Fixtures regenerate via the standard Go golden pattern, but the capturing
run needs the live credentials — which live ONLY in Actions secrets, so a
local `-update` run is impossible. Regeneration therefore runs as a
`workflow_dispatch` lane that executes
`go test -tags livegithub -run <update-test> -update` IN CI (where the
secrets live) and opens a BOT PR with the rewritten `testdata/` diff for
review. No human holds the PATs locally. This adds one bot-opens-a-PR
credential path (the same bot, scoped to opening PRs). The committed
fixtures are the reviewed contract snapshot.

### Leg 2 — live-credentials oracle (proves GitHub's contract has not drifted)

A `//go:build livegithub`-tagged Go test suite re-running the SAME scenarios
against a REAL throwaway GitHub repo under repo-scoped CI secrets (dedicated
test-only bot PATs), asserting the committed fixtures still match the live
responses. It mirrors the existing `//go:build podman` dogfood-e2e shape
exactly: the tag keeps it invisible to the moon battery's `go test ./...`
("It is build-tagged `podman`, so the moon `go test ./...` battery never
compiles it — this step is the ONLY thing that runs it", `ci.yml:704-705`;
`go/e2e/harness_test.go:1` `//go:build podman`), and a dedicated CI step runs
it with the capture-replay-exit discipline plus an assert-it-ran-not-skipped
guard (`ci.yml:719-748` derives the skip string from the harness source so
"rewording the skip cannot leave this grep matching nothing and reporting
success", `ci.yml:729-730`).

**Cadence (Matt's ruling): a REQUIRED check on forge-affected PRs AND
nightly.** Not nightly-only, not advisory.

The headline scenario is the F1 reviewer≠author assumption — the single most
load-bearing thing the oracle proves: the reviewer credential
(`GITHUB_FORGE_REVIEWER_TOKEN`) can APPROVE a PR authored by the author
credential (`GITHUB_FORGE_TOKEN`) on the throwaway repo, because "GitHub
rejects APPROVE and REQUEST_CHANGES from the PR's own author with a 422 (only
COMMENT is allowed from the author …), and every Compass-authored PR has the
author-credential account as its author"
(`compass-forge-write-path/design.md:277-280`; the two-credential ruling at
`:126-135`: "GitHub holds TWO: the AUTHOR credential (`GITHUB_FORGE_TOKEN`,
serve.go:761-762) … and a DISTINCT REVIEWER identity
(`GITHUB_FORGE_REVIEWER_TOKEN`) for the `submit_review` arm"; ledgered as
DL-201, `DECISIONS.md:145`).

**Credential separation.** The oracle's CI secrets are SEPARATE, test-only
bot PATs scoped to the throwaway repo — never the production `server_only`
declared secrets. Production forge credentials resolve through the declared-
secret path (`go/server/serve.go:122-124` "SecretName is the declared
server_only secret NAME holding the forge token (default
`GITHUB_FORGE_TOKEN`; the VALUE never crosses config or a flag)";
`validateForgeSecret`, `serve.go:779-790`; DL-052, `DECISIONS.md:75`). The
test PATs live only in GitHub Actions repo secrets, belong to a throwaway bot
account (plus one reviewer identity), and can be revoked with zero production
blast radius.

### How the dedicated CI step avoids the stale-enumeration failure

The one-job design's warning is about *per-project lists* in workflow YAML
(`ci.yml:6-18`). The live-oracle step is a single FIXED step, not a list: it
is keyed on one path filter (`go/internal/forge/**` + the workflow file
itself) computed in-workflow, it names one Go package
(`./internal/forge/...` with `-tags livegithub`), and there is nothing
per-project to enumerate or forget. Adding a second provider's oracle
(Linear) extends the same single step's tag/package, not a matrix. The
build-tag boundary is the same one the dogfood step and the pgtest step
already ride ("The suites are build-tagged `pgtest`, so the moon battery's
`go test ./...` never compiles them — the folded step below is the only thing
that runs them", `ci.yml:42-44`).

One honest caveat: the hardcoded `go/internal/forge/**` path glob is itself
a mini second source of truth — forge behavior affected from OUTSIDE that
path (e.g. `go/server/serve.go` wiring or a shared HTTP/token package) would
skip the oracle on that PR silently. That filter is a PR-COST optimization
only: push-to-main and the nightly run the step UNCONDITIONALLY, so a stale
filter delays detection by at most a day, never hides it — the same backstop
structure `ci.yml:28-36` articulates for moon's inputs globs.

### A new secret-bearing required check (D2's dogfood tier is unchanged)

This is a STANDALONE new decision, not an amendment to D2. The CI gate
GAINS one secret-bearing live-oracle step; D2's full-stack deterministic
tier stays exactly as decided — secret-free and per-PR. D2 lives in the
**platform dogfood-e2e record**, not a DECISIONS.md row: "D2 — Per-PR gate
cadence (Decision, Matt, 2026-08-05). The full-stack deterministic tier is
the per-PR gate" (`compass-dogfood-e2e/design.md:815-830`). That tier is
UNTOUCHED, and the `ci.yml:702-704` no-secrets comment describes the DOGFOOD
step specifically and stays true of it.

What actually changes is the implicit repo-wide posture that D2 merely
instantiated — "the PR gate carries no secrets." The forge oracle is a
SECOND, distinct per-PR step that DOES carry secrets; it neither modifies
D2's tier nor rewrites its no-secrets comment. It lands as a NEW DL row for
this record's decision (the frozen-record convention: a later record adds by
citation, never rewrites a frozen record). The `ci.yml:702-704` comment may
gain a clause clarifying that its no-secrets scope is the dogfood tier (T3's
diff touches that file anyway), but D2 itself is not re-opened.

**The decided tradeoff (recorded, not re-opened).** D2's no-secrets clause
was protecting two things: fork-PR secret safety and gate determinism. Matt
accepted the costs for the strongest correctness guarantee:

- *Determinism:* a live-GitHub flake, outage, or rate-limit exhaustion can
  now red an otherwise-good forge PR. Mitigations (retry-on-rerun, a
  concurrency cap, tight scenario budget) reduce but cannot eliminate this;
  it is the accepted price of a standing drift oracle.
- *Fork safety:* compass is PUBLIC with 0 forks (verified via the GitHub
  API, 2026-08-18: `"private": false, "forks_count": 0`). Name the hole
  honestly: because the same-repo-head guard skips the oracle on a fork-head
  PR — and GitHub withholds repo secrets from fork-head `pull_request` runs,
  so it could not run there anyway — a fork PR touching forge code SKIPS the
  oracle step and the single rolled-up CI check goes VACUOUSLY GREEN. The
  required-on-forge-affected-PRs guarantee structurally cannot hold for a
  fork PR. Matt's accepted compensating control (ruling): accept
  vacuous-green on forks; the push-to-main run (which HAS secrets) catches
  any drift within one merge; and by CONVENTION a maintainer re-pushes a fork
  contribution to a same-repo branch before merge, where the oracle runs with
  secrets. 0 forks today makes this dormant, not absent.

## Alternatives considered

Options Matt rejected, recorded so the record is self-explaining:

### (a) Manual-dogfood-only — no new test infrastructure

Rely on agents dogfooding the write path to surface contract mismatches.
Rejected: discovery is unbounded-latency and lands as production incidents
(a 422 in an agent's write, not a red check); nothing pins the contract a
fix was validated against.

### (b) Live oracle nightly + workflow_dispatch only (PR gate stays hermetic)

Preserves D2's per-PR determinism intact; drift is caught within a day.
Rejected: a forge PR can merge green against stale fixtures and be reverted
tomorrow — the strongest guarantee (no forge change merges unvalidated
against the live contract) requires the oracle on the PR gate.

### (c) Live oracle on PRs but NON-blocking (advisory)

Same signal, no red. Rejected: an advisory check is a check people learn to
ignore; the correctness guarantee only exists if the gate can refuse the
merge.

### (d) Record/replay fixtures with no standing live oracle

Fixtures regenerate only when a human runs `-update`. Rejected: the fixtures
decay silently between regenerations — exactly the drift class the oracle
exists to catch; (d) is leg 1 without leg 2.

### (e) go-vcr (`gopkg.in/dnaeon/go-vcr`) record/replay cassettes

Leg 1's hand-rolled fixture harness (schema + loader + `-update` +
request-match) reimplements what go-vcr ships: cassette record/replay via an
`http.RoundTripper` drop-in, request matchers, and re-record modes. Rejected
because we want the fixture to be a REVIEWED, diffable contract document with
an explicit request-assertion half, not an opaque cassette — but the
rejection is recorded so the reinvention is defended, not silent.

### (f) GitHub's official OpenAPI description (`github/rest-api-description`) as a hermetic shape oracle

Schema-validating our fixtures and request bodies against GitHub's published
versioned spec would catch field-shape drift with zero network/secrets/flake,
covering a large fraction of leg 2's drift class while leaving live-only
semantics (the F1 422 rule, auth-failure mapping, rate-limit headers) to a
smaller live suite — it would shrink the per-PR live surface. DEFERRED as a
possible future MIDDLE tier, not adopted in v1: this record's scope stays the
two decided legs.

### (g) Pact / consumer-driven contract testing

The obvious named tool for "integration testing an external API"; a poor fit
because contract testing presumes a provider who verifies the pact, and
GitHub/Linear will not.

## Global Constraints

- **Go floor:** `go 1.25.0` (`go/go.mod:15`); the module path is
  `github.com/sealedsecurity/compass/go` (`go.mod:13`).
- **No YAML project enumeration in CI.** The workflow may gain the single
  fixed live-oracle step, never a per-project/per-task list (`ci.yml:4-18`).
  Leg 1 requires zero `ci.yml` changes.
- **Fork-PR posture for leg 2 (accepted vacuous-green + push-to-main
  catch).** The secret-bearing step guards on a same-repo head
  (`github.event.pull_request.head.repo.full_name == github.repository`), and
  GitHub withholds repo secrets from fork-head `pull_request` runs — so a
  fork PR can never run the oracle. The honest consequence: a fork-head PR
  touching forge code skips the oracle and the rolled-up CI check goes
  VACUOUSLY GREEN; the required-on-forge-affected-PRs guarantee cannot hold
  for a fork PR. Matt's accepted compensating control: accept vacuous-green
  on forks, rely on the push-to-main run (which has secrets) to catch drift
  within one merge, and by convention re-push a fork contribution to a
  same-repo branch before merge. Repo Actions settings also require approval
  for ALL outside-collaborator workflow runs (defense in depth). Verified
  starting posture: public, 0 forks — dormant, not absent.
- **Test PATs ≠ production secrets.** The oracle uses dedicated bot PATs
  (author + reviewer identities) scoped to the throwaway repo, held only as
  GitHub Actions repo secrets — never the `server_only` declared secrets the
  server resolves (DL-052, `DECISIONS.md:75`; `serve.go:122-124`).
- **Fixtures live in `go/internal/forge/testdata/`** (new directory);
  regeneration is a `-update` flag gated on live credentials; committed
  fixtures are the reviewed contract snapshot.
- **The live suite is `//go:build livegithub`**, mirroring the `podman`
  precedent (`go/e2e/harness_test.go:1`) so the moon battery never compiles
  it; its CI step is the only runner.
- **Honor the assert-it-ran-not-skipped guard idiom**: the skip string is
  derived from the suite source, and the `ok` line is required for the
  package, per the pgtest and dogfood guards (`ci.yml:272-314`, `:719-748`).
- **Capture-replay-exit step shape**: redirect (never `| tee`), replay the
  log, exit on `go test`'s own status (`ci.yml:257-270`).
- **Sequencing:** leg 2's write scenarios depend on the write-path
  implementation tasks (GitHub write methods + `SubmitReview`,
  `compass-forge-write-path/design.md` T2/T3) being merged; `github.go` at
  main tip `7396fcff` still implements only the read half
  (`compass-forge-write-path/design.md:49-51`; grep for
  `func (g *GitHub) Create` returns nothing). Leg 1's read-path fixtures can
  land first.
- **Ledger coupling:** this record's PR must touch
  `docs/designs/product/DECISIONS.md` or declare `Ledger-impact:`
  (`tools/design-ledger-gate/index.ts:20-23`: "a PR whose changed set touches
  a product design record MUST also touch DECISIONS.md, unless it declares
  `Ledger-impact:` in the PR body").

## Plan

### T1 — Golden-fixture harness + replay test + seed fixtures (leg 1)

Add the fixture harness and the first fixture set for the existing read
methods, extending to the write methods as write-path T2/T3 merge.

Fixtures cover BOTH providers co-equally — GitHub and Linear issue/comment
create+read — under `testdata/<provider>/`; the harness is provider-generic.
The one GitHub-only concept is the PR/review family, which Linear has no
analogue for.

- `go/internal/forge/golden_test.go` (untagged): loads each fixture pair
  (request expectation + scripted response) from
  `go/internal/forge/testdata/<provider>/<scenario>.json`, replays it through
  `scriptedRoundTripper` (`github_test.go:28-32`) against the real client
  built by `newTestGitHub` (`github_test.go:70-76`), and asserts both halves:
  the request the client emitted (method, URL, headers, body) matches the
  captured request, and the decoded domain value matches the captured
  expectation.
- `go/internal/forge/golden.go` (test-support, or `_test.go`-only if no
  non-test consumer emerges): the fixture schema + loader + `-update`
  regeneration path. Regeneration hits the live throwaway repo with the leg-2
  credentials and rewrites the fixture files.
- `go/moon.yml`: extend the `&go_sources` anchor (`go/moon.yml:51-54`) with
  `'**/testdata/**'` so a fixture-only diff schedules the battery (see
  Approach — grounded correction).

Interfaces:

```go
// go/internal/forge/golden.go
// fixture is one captured GitHub exchange: the request our client is
// expected to emit and the scripted response to replay.
type fixture struct {
    Name     string          `json:"name"`
    Request  fixtureRequest  `json:"request"`  // method, path, query, headers (auth redacted), body
    Response fixtureResponse `json:"response"` // status, headers, body (verbatim GitHub JSON)
}

var update = flag.Bool("update", false, "regenerate testdata fixtures from the live throwaway repo (requires LIVEGITHUB_* env)")

func loadFixtures(t *testing.T, dir string) []fixture
func writeFixture(t *testing.T, dir string, f fixture)
```

Test cycle: red-green (`rule://red-green-testing`) — commit a deliberately
mis-shaped fixture first to watch the replay assert fail, then the real
capture; `moon run compass-go:test` green; a fixture-only commit demonstrably
schedules `compass-go` under `moon ci :ci`.

### T2 — `//go:build livegithub` oracle suite + `-update` capture + F1 headline scenario (leg 2)

- `go/internal/forge/livegithub_test.go` (`//go:build livegithub`): the same
  scenarios as T1's fixtures, executed against the real throwaway repo, each
  asserting the live response matches the committed fixture (shape-level
  comparison with an explicit volatile-field allowlist: ids, timestamps,
  URLs, rate-limit headers).
- Skip-when-unavailable, loud-in-CI: the suite `t.Skip`s when the credential
  env is absent (the local-sandbox posture, mirroring
  `go/e2e/harness_test.go:28-30`'s `podmanUsable()` skip), and the T3 CI
  guard turns that skip into a failure in CI (the `COMPASS_REQUIRE_LIVE`
  pattern, `ci.yml:252-256`, is the harness-level analogue if preferred).
- Headline scenario (F1): author PAT opens a PR on the throwaway repo →
  author PAT attempting APPROVE asserts the 422 rejection body matches the
  fixture → reviewer PAT APPROVEs successfully → teardown closes the PR and
  deletes the branch. Additional scenarios: create issue, comment on
  issue/PR, create PR, submit REQUEST_CHANGES/COMMENT reviews, each asserting
  the response fixture still matches; auth-failure mapping (revoked-token 401
  → the client's TokenSource invalidation path).
- Linear scenarios (co-equal, landing as the RIG-2209 Linear provider
  lands): create issue, comment on issue, get/list issues against the Linear
  test team under the `LINEAR_FORGE` token, each asserting the live response
  still matches the committed fixture. The GitHub PR/review family has no
  Linear analogue — those ops return `ErrUnsupported` and are NOT Linear
  scenarios. Mirrors the GitHub leg's shape under its own build tag.
- Scenario hygiene: every scenario creates uniquely-named artifacts
  (run-id-suffixed) and tears them down; a leaked artifact must not fail the
  next run.

Interfaces:

```go
// go/internal/forge/livegithub_test.go
//go:build livegithub

// Env contract (CI secrets → env in the T3 step):
//   LIVEGITHUB_REPO           — "owner/name" of the throwaway repo
//   LIVEGITHUB_AUTHOR_TOKEN   — test-only author bot PAT
//   LIVEGITHUB_REVIEWER_TOKEN — test-only reviewer bot PAT
//   LINEAR_FORGE              — test-only Linear token (dedicated test team)
func requireLive(t *testing.T) (repo string, author, reviewer forge.TokenSource) // t.Skip when unset
```

Test cycle: `go test -tags livegithub ./internal/forge/...` green locally
against the provisioned throwaway repo; a deliberately corrupted fixture
turns the oracle red (proving it compares, not just executes).

### T3 — CI leg wiring + repo-settings prerequisites

The one `ci.yml` edit this record authorizes: a single fixed step (plus its
guard step) in the existing `gates` job, after the pgtest step
(`ci.yml:244-314` is the shape to mirror).

- Step condition — the explicit boolean, NOT a single same-repo guard ANDed
  across all three events (that form nulls out the oracle on `push` and
  `schedule`, where `github.event.pull_request` is absent): run when
  `(event == pull_request AND same-repo-head AND path-affected) OR event ==
  push OR event == schedule`. Path-affected is the filter over
  `go/internal/forge/**` and the workflow file; the nightly trigger is
  `schedule` (`ci.yml:62-66`). The same-repo-head guard
  (`github.event.pull_request.head.repo.full_name == github.repository`,
  Global Constraints) applies ONLY to the `pull_request` arm — `push` to main
  and `schedule` run the oracle UNCONDITIONALLY with secrets, and are the
  backstop.
- Env: the `LIVEGITHUB_*` secrets and the `LINEAR_FORGE` token; capture-
  replay-exit run shape; a source-derived skip-string guard step asserting
  the suite ran rather than skipped (mirror `ci.yml:719-748`).
- Update the D2 comment at `ci.yml:702-704` to scope its no-secrets clause to
  the dogfood tier and cite this record (D2's tier is unchanged; the oracle
  is a separate secret-bearing step — see "A new secret-bearing required
  check").
- Regeneration lane: a `workflow_dispatch` job that runs
  `go test -tags livegithub -run <update-test> -update` in CI and opens a BOT
  PR with the rewritten `testdata/` diff for review (the PATs live only in
  Actions secrets, so no local `-update` is possible). One bot-opens-a-PR
  credential path, same bot scoped to opening PRs.
- Required-blocking activation ordering: the required-blocking flip activates
  AFTER the write scenarios exist (leg-1 read fixtures + the provider write
  methods land first); nightly-only/advisory in the interim is a STAGING
  order for the decided end state, not a re-litigation. The interim is
  short — the providers are landing now.
- Repo/infra prerequisites, each classified per `rule://no-human-clicks`
  (prefer IaC; genuine console-only steps become a `human-action` handoff
  issue per `skill://human-action-handoff`):
  - GitHub testbed: create `RigelBuild/compass-forge-testbed` (private)
    (API-scriptable: `gh repo create` / Pulumi GitHub provider — IaC).
  - Create the two GitHub bot accounts (author + reviewer) and mint their
    fine-grained PATs scoped to that repo (account creation + PAT minting is
    console-only — human-action handoff to Matt, with a copy-pasteable
    runbook).
  - Invite the reviewer bot as a collaborator on the private testbed AND
    accept the invite (author-PAT invites via the invitations API,
    reviewer-PAT accepts — both scriptable); without the accepted invite the
    reviewer's APPROVE 404s, and it is on the F1 headline critical path.
  - Linear testbed: provision a dedicated Linear test team and mint a test
    `LINEAR_FORGE` token (team creation is console-only — human-action
    handoff; token minting per the sanctioned declared-secret path where
    API-exposed).
  - Add the repo secrets — the `LIVEGITHUB_*` trio and `LINEAR_FORGE`
    (API-scriptable: `gh secret set` — IaC/CLI).
  - Actions settings: require approval for all outside-collaborator runs
    (API-scriptable via the Actions permissions API where exposed; otherwise
    handoff).
  - Branch-protection/required-check registration for the new check name is
    NOT needed: the step lives inside the one existing `CI` job, so the
    existing single required check already gates it (`ci.yml:20-21` "a
    single check name to require").

Interfaces: the step's env contract is T2's `LIVEGITHUB_*` trio plus the
Linear `LINEAR_FORGE` token; the run line is
`go test -tags livegithub -race -v -timeout 10m ./internal/forge/...` under
`working-directory: go`, `CGO_ENABLED: '1'` (the race-lane convention,
`go/moon.yml:150-152`), with the Linear live leg run under its own build tag
by the same step.

### T4 — Ledger delta

- Append the new DL row (next free id — DL-207 at drafting time; renumber to
  the ledger head at merge): the two-leg integration-testing decision, its
  required-on-affected-PRs + nightly cadence, the fork-PR vacuous-green
  posture, and the standalone secret-bearing-step decision recorded by
  citation (D2 is a platform dogfood-e2e record decision, not a ledger row —
  see Approach — and its dogfood tier is unchanged; this record's row records
  a NEW secret-bearing per-PR step, and the `ci.yml:702-704` comment update
  (T3) scopes D2's no-secrets clause to the dogfood tier).
- Extend the write-path record's pyramid BY CITATION only (its record is
  frozen; DL-174/DL-175, `DECISIONS.md:264-265`, stay Active — the two new
  legs sit above the pyramid, they do not replace any tier).
- The record PR itself carries this DECISIONS.md delta (the touch-coupling
  gate, `tools/design-ledger-gate/index.ts:20-23`).

Interfaces: one `DECISIONS.md` row under the forge topic heading; `Status:`
header of this record set per the record-Status grammar at merge.

### T5 — Frozen-record forward pointer (dogfood-e2e)

Add a one-line bidirectional pointer next to D2 in the frozen
`compass-dogfood-e2e/design.md` (§815-830) authorizing this record's
live-oracle secret-bearing carve-out — the frozen-record convention (add by
citation, never rewrite the frozen prose). This record points at D2; D2
points back here, so the carve-out is discoverable from both sides.

## Tasks

- [ ] **T1** *(compass-server)* — golden-fixture harness
  (`golden.go`/`golden_test.go`), `testdata/<provider>/` seed fixtures for
  BOTH providers' read + issue/comment create+read (GitHub and Linear
  co-equally; write methods as the provider write halves land), `-update`
  regeneration flag, `go/moon.yml` `&go_sources` + `'**/testdata/**'`;
  red-green + affected-detection smoke.
- [ ] **T2** *(compass-server)* — `//go:build livegithub` oracle suite (and
  the mirrored Linear live leg as RIG-2209 lands): fixture-match assertions
  with a volatile-field allowlist, the GitHub-only F1 reviewer≠author
  headline scenario (author 422 → reviewer APPROVE), Linear issue/comment
  scenarios (the PR/review family is `ErrUnsupported` on Linear, not
  scenarios), unique-name + teardown hygiene, skip-when-unavailable.
- [ ] **T3** *(compass-server + infra)* — the single fixed CI step + skip
  guard in the `gates` job (explicit `(pull_request AND same-repo-head AND
  path-affected) OR push OR schedule` condition, secrets env,
  capture-replay-exit), `ci.yml:702-704` D2 comment update, the
  `workflow_dispatch` `-update` bot-PR regen lane; provision the GitHub
  testbed repo + secrets via IaC/CLI, the reviewer-bot collaborator
  invite+accept, the Linear test team + `LINEAR_FORGE` token; bot accounts +
  PATs + (if API-unreachable) Actions approval setting as a `human-action`
  handoff.
- [ ] **T4** *(design ledger)* — new DL row (two-leg decision + cadence +
  fork-PR vacuous-green posture + the standalone secret-bearing-step decision
  recorded by citation, D2's tier unchanged); no Status flips
  (DL-174/175/201 stay Active); rides this record's PR.
- [ ] **T5** *(design docs)* — the frozen-record forward pointer next to D2
  in `compass-dogfood-e2e/design.md` (§815-830), added by citation.

## Resolved questions

Matt has ruled on all four; recorded here as decided (not open forks).

1. **Testbed naming, org, and identities (RESOLVED).** GitHub:
   `RigelBuild/compass-forge-testbed` (private — no public visibility needed,
   and privacy shrinks the abuse surface) with two fresh bot accounts
   (author + reviewer), each holding a fine-grained PAT scoped to that repo.
   Linear: a dedicated test team plus a test `LINEAR_FORGE` token. New scoped
   credentials for each provider; console-only creation steps (account/team
   creation, PAT minting) become a `human-action` handoff with a
   copy-pasteable runbook (T3).
2. **Fork-PR gating on a public repo (RESOLVED).** Accept vacuous-green on
   fork PRs and rely on the push-to-main run (which has secrets) to catch any
   drift within one merge; by convention a maintainer re-pushes a fork
   contribution to a same-repo branch before merge, where the oracle runs
   with secrets. Not merge_group, not a hard gate. The same-repo-head step
   condition + GitHub's fork-secret withholding + require-approval-for-
   outside-collaborators are the defense-in-depth guards. See "A new
   secret-bearing required check" and Global Constraints. 0 forks today makes
   this dormant, not absent.
3. **Rate-limit / flake budget (RESOLVED).** Accepted as decided; mitigate
   with the per-ref concurrency group (`ci.yml:86-88`), a tight scenario set
   (single-digit API calls per scenario), and re-run-clears-transient (the
   pgtest precedent, `ci.yml:40-42`). Additionally guard GitHub's SECONDARY
   rate limit (abuse detection on rapid content creation, independent of the
   5000/hr primary budget): two concurrent forge PRs share one author bot
   against one testbed repo, and the per-ref concurrency group does NOT
   serialize across different PRs — so add a repo-level concurrency group on
   the oracle step, or per-run backoff on 403-secondary responses.
4. **Linear scope (RESOLVED).** Linear is CO-EQUAL and in-scope now, not
   deferred: Compass writes to GitHub AND Linear, and both providers are
   first-class in both legs (golden-replay and live oracle). Linear's live
   oracle uses the Linear test team + `LINEAR_FORGE` token (question 1). The
   Linear provider is RIG-2209 (write-path T6, in flight now); its live write
   scenarios land as that provider lands. The GitHub-only F1 reviewer≠author
   scenario stays GitHub-only (Linear has no PR/review concept — the
   `ErrUnsupported` arm); the issue/comment create+read scenarios cover both.
