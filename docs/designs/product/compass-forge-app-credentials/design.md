# Compass forge App-credential cutover (GitHub + Linear)

Status: Draft

Tracking: RIG-2991 (thread 1, already filed as the W1 deferral) + the
five sibling threads Matt directed this session. Predecessors: RIG-2883
(read-path App-only cutover, Done), DL-201/F1 (two write identities),
DL-204/OQ-8 (Linear actor=app attribution), RIG-2682 (Linear app-actor
installed), RIG-2345/2425 (testbed), RIG-2561/2423 (oracle's Linear OAuth app).

## Problem / Intent

Finish the App-only credential move the read path already made, across the
whole forge surface. Matt's direction (verbatim intent, this session): "unify
onto one github app, and then we need to also add the second github app for the
reviewer functionality, and then we need to drop the old user PAT/keys path
from github and linear […] and then the live tests need to start using the
Apps. we also need to validate the webhooks actually work for the Apps on both
GitHub and Linear […] for linear we likely need to keep one user cred for the
live test because an agent on linear is a delegation of an issue from a main
user, not a user that can be assigned an issue by itself."

Six threads:

1. **Unify the two GitHub webhook lanes onto ONE shared App client**
   (RIG-2991). Today `buildBoardIngestLane` (serve.go:1048-1057) and
   `buildForgeNotifyLane` (serve.go:1333-1342) EACH construct
   `forge.NewAppTokenSource` + `forge.NewGitHub` with the identical
   `GitHubAppConfig{AppID: fc.App.AppID, InstallationID: fc.App.InstallationID,
   PrivateKey: newDeclaredSecretResolver(resolver, fc.App.AppPrivateKeySecret),
   Host: fc.Host}` — two clients over one installation, so the client-side
   rate-budget/`resetAt` gate is NOT shared and each lane burns the
   installation's budget blind to the other.
2. **Second GitHub App for the reviewer identity** — move both write
   identities onto Apps; a GitHub App is its own bot identity, so two distinct
   identities (DL-201/F1) means two App definitions.
3. **Drop the user-PAT / user-key path for GitHub AND Linear** in production.
4. **Move the live tests onto the Apps.**
5. **Validate real-App webhooks** end to end on both GitHub and Linear.
6. **Retain ONE Linear user credential for the live test** — the Linear
   app-actor is a delegation endpoint that cannot be *assigned* work
   (responder record: "a delegation is routed to the right stable Compass
   Manager … the Linear session is a doorway, not a home",
   `docs/designs/product/compass-linear-agent-responder/design.md:42-45`), so
   a live end-to-end delegation needs a user identity to perform the
   human→app handoff.

Why: App identities give forge-side scoping (per-App permissions, per-repo
installs) and installation-scoped rate limits instead of a user account's
global budget; installation tokens are short-lived (1 h, minted on demand —
`githubapp.go:67-69`: "RS256 App JWT (~10 min) -> POST
/app/installations/{id}/access_tokens -> cached until ~5 min before the 1 h
expiry") where the PATs are long-lived static secrets. The read path already
proved the model (RIG-2883: the board-webhook-ingestion record froze "remove
the static-PAT fallback; the GitHub App is the only GitHub credential",
`compass-forge-board-webhook-ingestion/design.md:54-55`); the write path is the
straggler.

### Grounded current state

**GitHub reads — App-only already.** `NewAppTokenSource`
(`go/internal/forge/githubapp.go:72-94`) mints installation tokens, cached and
singleflighted, `Invalidate()` on 401/bad-creds-403. Both webhook lanes gate on
`boardIngestionEnabled()` == `c.App.AppID != 0` (`serve.go:214-216`). The board
lane validates both App secrets once at boot (`serve.go:1031-1036`,
`validateForgeSecret` ×2); the notify lane skips re-validation
(`serve.go:1327-1342` goes straight from the gate to the token source).

**GitHub writes — still two PATs.** `registerGitHubForgeCoordinate`
(`serve.go:1614-1618`):

```go
author := forge.NewGitHub(forge.GitHubConfig{Host: fc.Host, Token: newForgeTokenSource(resolver, fc.SecretName)})
reviewer := forge.NewGitHub(forge.GitHubConfig{Host: fc.Host, Token: newForgeTokenSource(resolver, fc.ReviewerSecretName)})
```

Both ride `newForgeTokenSource` (`serve.go:1750-1752`), a 5-min-TTL
declared-secret resolver (`forgeTokenTTL = 5 * time.Minute`, `serve.go:207`) —
PAT values, not App tokens. Defaults: `GITHUB_FORGE_TOKEN` (author,
`serve.go:186`), `GITHUB_FORGE_REVIEWER_TOKEN` (reviewer, `serve.go:190`),
`LINEAR_FORGE_TOKEN` (`serve.go:195`). `forgeWritesEnabled`
(`serve.go:245-248`) requires both write secret NAMEs declared, independent of
the App gate.

**The two-identity rule is frozen; the token TYPE is not.** DL-201
(`docs/designs/DECISIONS.md:168`): "The arm executes under a DISTINCT reviewer
GitHub identity — a second `server_only` declared secret". The write-path
record (`compass-forge-write-path/design.md:275-283`) grounds why: "GitHub
rejects APPROVE and REQUEST_CHANGES from the PR's own author with a 422".
DL-201 freezes two IDENTITIES, not PAT-vs-App — the token type behind each
identity is exactly what this record decides. Attribution is invariant either
way: "The StampOwner header stays uniform: the stamp carries the ACTING agent
regardless of which credential posts" (`compass-forge-write-path/design.md:282-283`,
DL-050).

**Linear — one PAT everywhere, actor=app decided but unimplemented on the
token.** `LinearConfig` (`go/internal/forge/linear.go:80-85`) has
`Token TokenSource // required (its own LINEAR_FORGE_TOKEN, DL-052)` — no App
id/key/installation field, no author/reviewer split. Both the write coordinate
(`serve.go:1600`) and `buildLinearNotifyLane` (`serve.go:1394`) build
`forge.NewLinear(forge.LinearConfig{Token: newForgeTokenSource(resolver,
defaultForgeLinearSecretName), …})` — PAT-only. DL-204
(`docs/designs/DECISIONS.md:171`) already rules the intent: "the token is OAuth
actor=app, degrading to stamp-only via a named boot-time capability probe on a
non-actor token" — and the client carries the probe seams
(`linear.go:110-113`: "probeDone/actorCapable cache the one-time
actor-capability probe (A4)"), but the production token behind it is still a
plain resolved PAT. Separately, the responder's app-actor
(`go/internal/linearagent/client.go:82-96` `NewTokenSource`) already mints
OAuth `client_credentials` tokens (scope
`"read,write,app:assignable,app:mentionable"`, `client.go:30`) for outbound
emits — a working actor=app mint path exists in-tree.

**Live tests — all PAT, zero App path, no real-App webhook proof.**
`livegithub_test.go:61-67` pins the env contract: `LIVEGITHUB_AUTHOR_TOKEN` /
`LIVEGITHUB_REVIEWER_TOKEN` ("test-only author/reviewer bot PAT"),
`LINEAR_FORGE`, `LINEAR_FORGE_TEAM`; `requireLive`
(`livegithub_test.go:75-84`) wraps each in a `fakeTokenSource` — no App-token
mint is ever exercised. The CI oracle (`.github/workflows/ci.yml:965-1022`)
runs `go test -tags livegithub` against the `compass-forge-testbed` repo with
those PATs; its Linear leg ALREADY mints an app token
(`ci.yml:1001-1003`: "LINEAR_FORGE itself is the app-actor token the mint step
above just exported … a personal key 400s against Linear's Bearer endpoint —
RIG-2423"). Webhook signature verify exists and is unit-tested with FAKE
signers only: `VerifyGitHubSignature` (`githubapp_webhook.go:23-35`,
constant-time `sha256=<hex>`), `linearagent.VerifySignature` +
`CheckTimestamp` (`webhook.go:65-84`), handlers fail-closed
(`github_webhook.go:164-168`, `linear_webhook.go:140-144`). No test, CI leg,
or runbook has ever validated a delivery signed by a REAL GitHub App or REAL
Linear app against our verify path.

### Settled inputs (do NOT reopen)

- Read/board path is App-only; read-path PAT retired (RIG-2883, DL-264,
  board-webhook-ingestion record).
- Two distinct write IDENTITIES for author vs reviewer (DL-201/F1). This
  record changes only the token TYPE behind them.
- Linear write token intended as OAuth actor=app + `createAsUser` shared
  identity (DL-204/OQ-8). This record finishes that intent, not reopens it.
- Server-holds-write-creds as `server_only` declared secrets; the agent never
  carries a forge token (DL-052).
- Linear delegation model: app-actor routes; per-agent truth in the stamp
  (DL-055/DL-255, responder record).

## Approach

**Two GitHub Apps total, clean PAT cutover, tests and webhook validation on
the real App path.**

### Topology: 2 Apps (recommended; OQ-3 is the confirming fork)

- **Primary App** (the existing `ForgeConfig.App` — `serve.go:138-142`):
  serves ALL reads (board ingest + notify, unified onto one client per
  RIG-2991) AND the author write client. One bot identity authors every
  Compass PR/comment/issue.
- **Reviewer App**: a second App definition (own `AppID` + private key + one
  installation on the same org/repos), serving ONLY the `submit_review` arm's
  reviewer client. A second App *definition* — not a second install of the
  primary App — because installs of one App share the App's single bot login,
  which cannot satisfy F1's distinct-identity requirement (the 422 is keyed on
  the PR author's account).

This is the fewest identities satisfying F1, and it composes with what exists:
the reads already share the primary App (`serve.go:1048-1057`, 1333-1342). But
author-writes joining the primary App is a genuine tradeoff, not a pure
extension of RIG-2991: unifying the two READ lanes removes blind double-burn
of a budget they already share server-side, while folding writes in makes
background reads and interactive writes share ONE client-side fail-fast gate
(`resetAt`, `github.go:83-92` — every write POST checks it via `gateBlocked`,
`github.go:837-854`, in `doJSON`, `github.go:862-866`) AND one server-side
installation budget, where today the author PAT is a separate user-account
5,000/hr pool. A board-reconcile sweep that drives remaining under `reserve`
(= 10, `github.go:99-102`) arms the gate, and every interactive agent write
then fails fast with `ErrBudgetExhausted` until the window resets. OQ-3
carries this fork with the arithmetic; the recommendation stays 2 Apps. The
reviewer's traffic (review POSTs) is tiny and isolated on its own App budget.

### Cutover: clean, no PAT fallback

Production drops `newForgeTokenSource` usage for GitHub writes entirely, along
with the `GITHUB_FORGE_TOKEN`/`GITHUB_FORGE_REVIEWER_TOKEN` secret names and
their flags (AGENTS.md default-clean-cutover; the read path set the precedent
with "No PAT fallback on the read path (Constraint #3)", `serve.go:141`).
`forgeWritesEnabled` re-keys from "both PAT secret names declared"
(`serve.go:245-248`) to "reviewer App configured" (the author write rides the
primary App the board gate already checks). Linear's write/notify token moves
from the member PAT to the OAuth client_credentials actor=app mint the
responder already implements (`linearagent.NewTokenSource`,
`client.go:82-96`) — finishing DL-204. The degrade probe (`linear.go:110-113`)
stays, but it guards only the attribution channel, not the mint path; T4
pairs it with a boot-time mint check (see T4).

### Tests and validation ride the real path

The `livegithub` oracle's GitHub legs switch from PAT env vars to App
credentials (App id + installation id + PEM), minting installation tokens
through the SAME `forge.NewAppTokenSource` production uses — the oracle then
proves the production credential path, not a lookalike. Linear keeps exactly
one USER credential in the live tier: the delegation setup step, because the
app-actor cannot be assigned work (thread 6). Real-App webhook validation is
a live-tier capture-and-replay: one genuinely-signed delivery per provider
committed as a fixture — pulled via the GitHub App deliveries API under
`-update` (the oracle's existing regeneration lane,
`livegithub_test.go:55-58`); copied manually from the Linear webhook UI (no
deliveries API; the fork is OQ-6) — and asserted through
`VerifyGitHubSignature`/`linearagent.VerifySignature`, plus a one-time App
webhook-registration runbook (skill://human-action-handoff) since delivery
needs a public ingress the CI runner lacks.

### Alternatives considered

- **3 Apps (read App ≠ author-write App ≠ reviewer App).** Two real arguments
  for it: blast radius (a leaked write key can't read-scope, and vice versa)
  and — the stronger one — QoS rate-isolation: a separate write App keeps
  interactive author writes off the installation budget and client-side gate
  that background reads arm (`github.go:83-92`, `837-854`). Cost: a third
  identity, third key, third installation to rotate; the primary App's read
  permissions are a subset of what a write App needs anyway on the same
  repos. Not chosen, but not rejected on blast-radius grounds alone either —
  the rate-isolation tradeoff is surfaced with its arithmetic in OQ-3 for
  Matt to rule on.
- **One App, two installations.** Fails F1: both installations mint tokens for
  the SAME App bot login, so reviewer APPROVE on an author-authored PR still
  422s ("GitHub rejects APPROVE and REQUEST_CHANGES from the PR's own author",
  `compass-forge-write-path/design.md:277-278`). Rejected outright — not
  surfaced as a fork.
- **PAT deprecation window (dual-path fallback).** Keep `newForgeTokenSource`
  write wiring behind the App path for a release. Rejected: it doubles the
  gate matrix (`forgeWritesEnabled` would need a 2×2 of App-vs-PAT states),
  contradicts the house default-clean-cutover, and the read path already
  demonstrated the clean cut is operationally fine (RIG-2883). Surfaced in
  OQ-4 since the drop is production-visible.
- **Per-role App-config map** (`map[role]ForgeAppConfig`). More general than
  two named blocks, but nothing needs a third role, and the existing shape is
  a named struct field (`ForgeConfig.App`, `serve.go:142`); a map would be a
  second configuration convention beside it. Rejected; OQ-2 carries the
  config-shape fork.

## Global Constraints

Every task brief inherits these; do not re-litigate them per task.

- **Toolchain:** Go per `go/go.mod`; server tests run with `-tags unix`,
  store-touching tests with `pgtest`; the live oracle is `-tags livegithub`
  (`ci.yml:1020`), never compiled by moon's untagged `go test ./...`.
- **Team key:** Linear test issues use team key `RIG`, never `SEA`
  (`livegithub_test.go:66`: "test team key; no default (dead \"SEA\"
  dropped)").
- **No planning-metadata inline in code.** A `RIG-NNNN` in a file-header
  comment is fine; task ids/OQ numbers do not leak into identifiers or logic.
- **Every credential is a `server_only` declared secret,** provisioned by IaC
  (rule://no-human-clicks — no console clicks beyond the one-time App
  registrations, which get an explicit runbook, T6). Secret NAMEs cross
  config; VALUEs never do (`serve.go:173-178` sets the pattern).
- **Attribution invariant:** `StampOwner` carries the acting agent regardless
  of which credential posts (DL-050; `compass-forge-write-path/design.md:282-283`).
  No task may key routing/authz off a credential identity.
- **DL-201 (two distinct write identities) and DL-204 (Linear actor=app +
  `createAsUser`, degrade probe) are settled inputs** — this program changes
  token types and wiring, never those rules.
- **Fail-closed webhook posture is preserved:** signature verify before any
  body processing (`github_webhook.go:164-168`, `linear_webhook.go:132-144`);
  no task weakens it for testability.
- **Lane gating stays per-lane and boot-fail-fast:** a configured credential
  with a missing secret is a startup error; an unconfigured lane is a Warned
  off-state (`serve.go:1021-1036` pattern).

## Plan

Dependency order: T1 is independent (already filed as RIG-2991) and may land
first. T2 depends on T1's shared-client seam. T3 depends on T2's write-App
config shape. T3.5 is a DEPLOYMENT gate, not code: reviewer-App registration
executed + IaC add-merge applied + writes verified on App credentials. T4
depends on T2+T3 in code AND on T3.5 in deployment — both write clients
App-backed and PROVEN live before any PAT drops. T5 depends on T3 (needs both
Apps to exist) and T4's final env contract. T6 depends on T5's live-tier
harness, but its runbook half must land after T3 (T3.5 executes the runbook's
registration section).

### T1 — Unify the two GitHub read lanes onto one shared App client (RIG-2991)

Owner: compass-forge lane.

Extract the duplicated construction in `buildBoardIngestLane`
(`serve.go:1048-1057`) and `buildForgeNotifyLane` (`serve.go:1333-1342`) into
one shared token source + client built once in `Serve` and passed to both
builders, so the client-side rate-budget/`resetAt` gate is one gate. Move the
App-secret validation (`serve.go:1031-1036`) to the shared construction site so
the notify lane stops relying on the board lane having validated first.

Interfaces:

- Consumes: `forge.NewAppTokenSource(forge.GitHubAppConfig) (forge.TokenSource, error)`
  (`githubapp.go:72`); `forge.NewGitHub(forge.GitHubConfig) *forge.GitHub`.
- Produces: one `*forge.GitHub` value threaded into both
  `buildBoardIngestLane(ctx, cfg, st, issueBrd, client, log)` and
  `buildForgeNotifyLane(cfg, st, hub, client, log)` — signature change: the
  `resolver secrets.Resolver` parameter is replaced by the shared
  `client *forge.GitHub` in both builders (the resolver stays only where a
  webhook-secret resolver func is built).
- Tests: existing lane tests updated to inject the shared client; a unit test
  asserting both lanes observe one budget gate (drive `ErrBudgetExhausted`
  through one lane, assert the other fast-fails).

### T2 — Author writes onto the primary App

Owner: compass-forge lane.

Re-point the author write client from the PAT token source to the primary
App's installation-token source. `registerGitHubForgeCoordinate`
(`serve.go:1614-1618`) takes the shared App token source (or the shared
client's source) instead of `newForgeTokenSource(resolver, fc.SecretName)`.
Note the caching model difference: the write path currently assumes the
poll-driver's single-caller contract (`serve.go:1758-1764`); `appTokenSource`
is already concurrency-safe ("Safe for concurrent use; mint is singleflighted",
`githubapp.go:70-71`), so sharing it with the read lanes is sound.

Interfaces:

- Consumes: the T1 shared `forge.TokenSource`; `forge.GitHubConfig{Host, Token}`.
- Produces: `registerGitHubForgeCoordinate(reg *forgeProviderRegistry,
  fc ForgeConfig, authorTok, reviewerTok forge.TokenSource)` (resolver
  parameter dropped once T3+T4 complete; during T2 the reviewer stays
  PAT-backed via `newForgeTokenSource`).
- Tests: forge-write service tests exercising the author path over a stub
  token source; assert the author client and the read client share one budget.

### T3 — Reviewer App: second App definition + reviewer client

Owner: compass-forge lane. App registration itself is a one-time human action
(runbook in T6, executed at the T3.5 gate); the code lands first and gates
off until configured.

Add a `ReviewerApp ForgeAppConfig` field to `ForgeConfig` (mirroring `App`,
`serve.go:138-142`) with its own `--forge-reviewer-app-id`,
`--forge-reviewer-app-installation-id` flags and
`FORGE_REVIEWER_APP_PRIVATE_KEY` declared-secret name (webhook-secret field
unused: the reviewer App registers NO webhook — reads/webhooks stay on the
primary App per the 2-App topology). Build a second `appTokenSource` over it;
the reviewer client in `registerGitHubForgeCoordinate` rides it.

One deployment shape is knowingly lost here. Today writes-without-a-GitHub-App
is expressible: `forgeWritesEnabled` (`serve.go:245-248`) is independent of
`boardIngestionEnabled` (`serve.go:214-216`) — Matt's 2026-08-19 ruling,
referenced in the field comment (`serve.go:150-151`; the Linear-gate comment
states the lane independence outright, `serve.go:156-158`). After T2/T3,
configuring writes requires configuring the primary App, which flips
`boardIngestionEnabled` true, which makes `buildBoardIngestLane` demand
`AppWebhookSecretName` and start the ingest+notify lanes
(`serve.go:1031-1036`). This record recommends ACCEPTING the coupling —
enabling writes force-enables board ingestion — with a ledger note amending
the 2026-08-19 independent-gates ruling at freeze; the alternative (gate
separately: App + webhook-secret ⇒ ingestion, App alone ⇒ writes) is
preserved as a sub-point of OQ-3 for Matt to veto.

Interfaces:

- Consumes: `forge.NewAppTokenSource` with the reviewer
  `GitHubAppConfig{AppID, InstallationID, PrivateKey, Host}`.
- Produces: `ForgeConfig.ReviewerApp ForgeAppConfig`; `resolved()` defaulting;
  `forgeWritesEnabled` re-keyed to "primary App configured AND ReviewerApp
  configured (AppID != 0 + key secret declared)" replacing the two-PAT-names
  predicate (`serve.go:245-248`).
- Tests: config resolution + gate-predicate unit tests (both-configured /
  one-missing warn parity with `warnPartialForgeWriteSecrets`,
  `serve.go:256-267`).

### T3.5 — Deployment gate: Apps live + writes verified BEFORE the PAT drop

Owner: compass-forge lane; execution is human (skill://human-action-handoff).

The T1-T6 graph is a CODE dependency graph; this gate is the DEPLOYMENT
ordering that keeps the write path from going dark. If T4 deploys before the
reviewer App is registered+installed and its secrets applied,
`forgeWritesEnabled` (re-keyed by T3) evaluates false and the production
forge-WRITE path drops from enabled to a Warned OFF-state
(`warnPartialForgeWriteSecrets` parity, `serve.go:256-267`) — a silent write
outage dressed as a valid off-state. The read-path precedent this record
leans on did it in the safe order: RIG-2883 had the App live before the PAT
dropped. Explicitly:

1. T3 code merged (gates off until configured).
2. Reviewer-App registration runbook section EXECUTED (T6's runbook,
   registration half pulled forward to here); IaC add-merge (T4 merge A)
   applied.
3. Writes VERIFIED on App credentials (author comment + reviewer review on a
   scratch PR); the Linear boot-time mint check (T4) green in staging or
   equivalent.
4. Only then T4's PAT-deletion merge(s).

Interfaces:

- Consumes: the T6 runbook's registration section; T4 merge A secret rows.
- Produces: a checked-off precondition in the T4 PR description with the
  verification evidence (scratch-PR links) recorded there.

### T4 — Drop the PATs: GitHub clean cutover + Linear actor=app finish

Owner: compass-forge lane.

GitHub: delete the write-path `newForgeTokenSource` usage, the
`SecretName`/`ReviewerSecretName` fields, their flags, and the
`GITHUB_FORGE_TOKEN`/`GITHUB_FORGE_REVIEWER_TOKEN` defaults
(`serve.go:186-190`); `forgeTokenSource` itself survives only if Linear still
needs it (see below), else it goes too. Linear: build ONE
`linearagent.NewTokenSource(clientID, clientSecret, nil, "")`
(`client.go:82-96`) instance in `Serve` and pass it to BOTH build sites
(`serve.go:1394` notify, `serve.go:1600` write coordinate) — never one
instance per site: Linear revokes a client-credentials app's existing tokens
when a mint requests a different scope set (`client.go:26-30`), the mint
singleflight coalesces only WITHIN an instance, and whether concurrent
same-scope mints from independent instances coexist is unverified — one
shared instance removes the question entirely and is share-ready for the
future responder dispatcher (RIG-2717) on the same app. The identity behind
it is the PRODUCTION Linear app: the RIG-2682 "Compass" agent app (viewer.id
`b7511616-41fe-4583-a52a-befdb4c3090c`,
`compass-linear-agent-responder/design.md:8-9`), whose "client credentials
tokens" toggle must be ON (`compass-linear-agent-responder/design.md:387-388`)
— NOT the oracle's test OAuth app (RIG-2561/2423), even though the
declared-secret NAMEs (`LINEAR_FORGE_CLIENT_ID`/`LINEAR_FORGE_CLIENT_SECRET`)
mirror the Actions names CI already uses (`ci.yml:961-962`); the production
secret VALUEs are the Compass app's pair.

Two corrections to the safety story. The DL-204 degrade probe
(`linear.go:110-113`; probe at `linear.go:554-601`) gates ONLY the
`createAsUser`/`displayIconUrl` attribution channel (`applyAttribution`,
`linear.go:603-611`) against a wrong-KIND token; it protects nothing against
the MINT path failing — a bad `LINEAR_FORGE_CLIENT_SECRET` or a disabled
client_credentials toggle surfaces as a hard write error from
`TokenSource.Token`, and the probe never runs. So T4 adds a boot-time mint
check: one `Token(ctx)` call at build, fail-fast like `validateForgeSecret`
(`serve.go:1031-1036` pattern), so a misconfigured OAuth pair fails at
startup, not on the first write. And grounding on what production currently
holds: the Linear client sends `"Bearer "+token` unconditionally
(`linear.go:333-336`), and a personal API key 400s under Bearer (RIG-2423,
`ci.yml:1001-1003`) — so the deployed `LINEAR_FORGE_TOKEN` value is either
already an OAuth token (making this cutover a wiring formalization) or Linear
writes are latently broken today; the T3.5 verification step settles which
before the PAT row drops.

Interfaces:

- Consumes: `linearagent.NewTokenSource(clientID, clientSecret string,
  doer httpDoer, tokenURL string) *TokenSource` (`client.go:82`); the
  `forge.TokenSource` seam (`Token(ctx) (string, error)` + `Invalidate()`).
- Produces: `*linearagent.TokenSource` wired directly as the `forge.TokenSource`
  — it already satisfies the interface verbatim (`forge.TokenSource` =
  `Token(ctx context.Context) (string, error)` + `Invalidate()`,
  `github.go:32-35`; `linearagent.TokenSource` has exactly those methods,
  `client.go:101-115`), so NO adapter is needed, only the declared-secret
  resolution of client id/secret at build time; removal of the GitHub write
  secret names from `ForgeConfig`; Linear lane gates re-keyed to the
  client-id/secret pair declared.
- Tests: gate predicates; the Linear notify + write coordinate builders over a
  stubbed mint endpoint (`httptest` doer, the linearagent test pattern).
- IaC: TWO merges, not one. Merge A (before/with T3.5): add the new declared-
  secret rows (reviewer-App PEM, Linear client id/secret). Merge B (T4 proper,
  after T3.5 verifies writes on Apps): delete the PAT rows — and if those rows
  are `protect: true`, merge B itself splits into the unprotect-merge and the
  delete-merge per rule://pulumi-protected-teardown (never one PR, never a
  manual state unprotect).

### T5 — Live tests onto the Apps (retain one Linear user cred)

Owner: compass-forge lane; the CI workflow + testbed provisioning historically
sat with compass-server/core (RIG-2345/2425 provisioned
`compass-forge-testbed`; RIG-2561/2423 minted the oracle's Linear OAuth app) —
coordinate the Actions-secret changes there.

Replace the `LIVEGITHUB_AUTHOR_TOKEN`/`LIVEGITHUB_REVIEWER_TOKEN` PAT env vars
(`livegithub_test.go:63-64`) with App credentials: per identity, an App id +
installation id + PEM env var (`LIVEGITHUB_AUTHOR_APP_ID`,
`LIVEGITHUB_AUTHOR_APP_INSTALLATION_ID`, `LIVEGITHUB_AUTHOR_APP_KEY`; reviewer
likewise), built into real `forge.NewAppTokenSource` sources in `requireLive`
— the oracle then exercises the production mint path (JWT → installation
token) instead of `fakeTokenSource` (`livegithub_test.go:75-84`). Linear: the
oracle's write/notify legs keep the minted app token (`ci.yml:1001-1003`,
already actor=app); ADD one Linear USER credential env
(`LINEAR_FORGE_USER_TOKEN`) used ONLY by the delegation-setup step of the
end-to-end leg — the human→app delegation the app-actor cannot self-assign
(thread 6). Update the CI env block (`ci.yml:996-1011`) and the skip-guard
literals if the gating env set changes (`liveSkipMessage` contract,
`livegithub_test.go:46-49` — the CI guard greps it from source,
`ci.yml:1055-1062`).

Fidelity note: the oracle authenticates each identity with its own standalone
token source, so the production 2-App topology's riskiest property — board
reads and author writes sharing one client-side gate + installation budget
(OQ-3) — is never exercised live; T1's cross-lane `ErrBudgetExhausted` unit
test is the only coverage.

T5 goes green only after two HUMAN actions no code task performs: the two
testbed App registrations on `compass-forge-testbed` (author + reviewer test
Apps, installed on the testbed repo) and provisioning the six new Actions
secrets (`LIVEGITHUB_{AUTHOR,REVIEWER}_APP_{ID,INSTALLATION_ID,KEY}`). Both
are owned by the T6 runbook's testbed section (skill://human-action-handoff —
filed as a Linear issue, never console-clicked ad hoc); T5's PR links that
issue.

Interfaces:

- Consumes: `forge.NewAppTokenSource`; the existing skip-literal contract.
- Produces: `requireLive(t) (repo string, author, reviewer forge.TokenSource)`
  returning real App sources; new env contract constants; updated
  `ci.yml` forge-oracle env block + Actions secrets list.
- Tests: the oracle itself is the test; the skip-guard step must still derive
  its greps from source.

### T6 — Real-App webhook validation + registration runbook

Owner: compass-forge lane; the public-ingress piece touches deployment
(compass-server/core historically).

Two halves. (a) **Live-tier signed-delivery fixtures:** capture ONE real
delivery per provider — a GitHub delivery signed by the primary App's webhook
secret, a Linear delivery signed by the workspace webhook secret — as
committed fixtures (raw body + signature header), then a replay test asserting
`forge.VerifyGitHubSignature(secret, body, header)` (`githubapp_webhook.go:23`)
and `linearagent.VerifySignature(secret, body, headerHex)` +
`CheckTimestamp` (`webhook.go:65-84`) accept them. Capture differs per
provider because a webhook delivery is a PUSH and the oracle's CI runner has
no ingress: the GitHub fixture is fetched as a PULL via the App deliveries
API (`GET /app/hook/deliveries` + `GET /app/hook/deliveries/{id}`, which
returns the delivered payload and headers including `X-Hub-Signature-256`)
under `-update` like the oracle's regeneration lane (`liveUpdateSkipMessage`,
`livegithub_test.go:55-58`); the Linear fixture has no deliveries-API
equivalent and is captured MANUALLY — a human copies one delivery (raw body +
signature header) from the Linear webhook UI, a named runbook step (the
mechanism fork is OQ-6). The fixture secret is the capture-time secret stored
alongside (rotated test-tier secret, never the production one). Replay proves
the signature format, not ongoing ingress reachability — OQ-6 recommends a
webhook-delivery freshness metric/alert on the ingest lane as the standing
liveness signal. (b) **Runbook:** a one-time App registration runbook per
skill://human-action-handoff — filed as a Linear issue when the time comes,
never console-clicked ad hoc — covering, in sections:

- **Production registration:** GitHub App webhook URL
  (`POST /webhooks/github`, `github_webhook.go:114-122`) + secret; Linear
  webhook (`POST /webhooks/linear`, DL-302, `linear_webhook.go:76-91`);
  reviewer-App creation + installation (no webhook). The registration half is
  EXECUTED at the T3.5 gate, before any PAT drops.
- **Testbed provisioning (T5's precondition):** create + install the two
  testbed Apps on `compass-forge-testbed` and provision the six
  `LIVEGITHUB_{AUTHOR,REVIEWER}_APP_{ID,INSTALLATION_ID,KEY}` Actions secrets
  (coordinated with compass-server/core per T5).
- **PEM rotation:** post-cutover the fleet holds four App private keys
  (primary, reviewer, two testbed) — the one long-lived App credential, in a
  record whose motivation argues Apps beat PATs partly on credential
  lifetime. The procedure is cheap and lives here: generate a second key on
  the App, swap the declared-secret VALUE (`PrivateKey` is a resolver func,
  so the new value takes effect on the next mint, `githubapp.go:67-71`),
  verify a mint, delete the old key.

Interfaces:

- Consumes: `VerifyGitHubSignature`, `linearagent.VerifySignature`,
  `CheckTimestamp`; the `-update` fixture-regeneration convention.
- Produces: `go/internal/forge/testdata/` (+ linearagent testdata) signed
  fixtures; a `TestLiveWebhookSignature*` pair in the livegithub tier; the
  runbook doc (location per the fork in OQ-6).

## Tasks

- [ ] T1 — Unify board-ingest + notify onto one shared GitHub App client;
      shared budget gate; validation moved to the shared construction site
      (RIG-2991).
- [ ] T2 — Author write client onto the primary App token source;
      `registerGitHubForgeCoordinate` re-plumbed.
- [ ] T3 — `ForgeConfig.ReviewerApp` + flags + key secret; reviewer client on
      its own App token source; `forgeWritesEnabled` re-keyed.
- [ ] T3.5 — Deployment gate: reviewer App registered + installed; IaC
      add-merge applied; writes verified on App credentials (scratch PR).
- [ ] T4 — GitHub write-PAT clean cutover (fields, flags, defaults deleted);
      Linear PAT → ONE shared OAuth actor=app TokenSource for both build
      sites + boot-time mint check; IaC PAT-row teardown merge(s).
- [ ] T5 — `livegithub` oracle GitHub legs onto real App token sources; one
      Linear USER cred retained for delegation setup; CI env + skip guards
      updated.
- [ ] T6 — Real-App signed-delivery fixtures + replay tests (both providers);
      one-time webhook-registration runbook.

## Open Questions

For Matt to rule (batched `ask` by the spawning agent). Recommendations are
the designer's, grounded; none is ruled here.

- **OQ-1 — Reviewer as a second App definition vs. two installs of one App.**
  An App is a bot identity; author≠reviewer needs two identities (DL-201/F1 —
  GitHub 422s APPROVE/REQUEST_CHANGES from the PR's own author,
  `compass-forge-write-path/design.md:277-278`). Two separate App
  *definitions* (each its own `AppID`+key) gives a genuinely distinct reviewer
  login; a single App with two installations does NOT (same bot login), so it
  cannot satisfy F1. **Recommend: two App definitions.** Confirm — and confirm
  whether the reviewer App also serves any read lane or is write-only (the
  record assumes write-only, no webhook).
- **OQ-2 — Config shape for the write Apps.** Today writes gate on two PAT
  secret NAMEs (`serve.go:245-248`); the read lanes gate on one
  `ForgeAppConfig{AppID, InstallationID, AppPrivateKeySecret,
  AppWebhookSecretName}` (`serve.go:168-181`). Options: (a) a second
  `ForgeConfig.ReviewerApp ForgeAppConfig` block, the existing `App` block
  extended to serve author writes; (b) a per-role App-config map.
  **Recommend (a)** — explicit author-App + reviewer-App blocks mirroring the
  existing single-App shape; exact additions: `ForgeConfig.ReviewerApp
  ForgeAppConfig`, flags `--forge-reviewer-app-id` /
  `--forge-reviewer-app-installation-id`, declared-secret name
  `FORGE_REVIEWER_APP_PRIVATE_KEY` (T3). Tradeoff: (b) is more general but
  nothing needs a third role and it would sit beside the existing named-field
  convention as a second shape.
- **OQ-3 — Do the unified read App and the author write App collapse to ONE
  App (2 total) or stay separate (3 total)?** The load-bearing structural
  fork — and it is a RATE-ISOLATION fork, not only a blast-radius one.
  Moving author writes onto the read App means background board-reconcile
  sweeps + notify reads and interactive agent writes share ONE client-side
  fail-fast gate (`resetAt`, `github.go:83-92`; every write POST checks
  `gateBlocked`, `github.go:837-854`, in `doJSON`, `github.go:862-866`) AND
  one server-side installation budget, where today the author PAT is its own
  5,000/hr pool: a reconcile sweep that drives remaining under `reserve`
  (= 10, `github.go:99-102`) arms the gate and every interactive write fails
  fast with `ErrBudgetExhausted` until the window resets — background reads
  throttling user-facing writes — and two server-side budget pools become
  one. Sub-options:
  - **(a) 3 Apps** — read / author-write / reviewer: write QoS insulated
    from read bursts, at the cost of a third identity/key/installation to
    rotate.
  - **(b) 2 Apps + a priority/partitioned client gate** — writes bypass a
    read-armed gate down to a lower reserve; keeps two identities but
    reintroduces gate complexity into `forge.GitHub`.
  - **(c) 2 Apps, accept the coupling with the arithmetic shown** — measured
    read volume (board poll + notify + 30-min reconcile backstop cadence) vs
    the 5,000/hr installation budget on the current repo set, the bound
    stated in the freeze note.
  **Recommend (c), i.e. 2 Apps total** — fewest identities satisfying F1;
  reads already share the primary App (`serve.go:1048-1057`, `1333-1342`) —
  but rule SEEING the write-throttling coupling, not just blast radius.
  Sub-point (from T3): 2 Apps also deletes the currently-expressible
  writes-without-webhooks deployment shape — enabling writes force-enables
  board ingestion, amending the 2026-08-19 independent-gates ruling
  (`serve.go:150-151`). The record recommends accepting that with a ledger
  note; say if you want the gates kept separate instead (App +
  webhook-secret ⇒ ingestion, App alone ⇒ writes).
- **OQ-4 — PAT-drop scope + timeline.** Reads are already PAT-free. Dropping
  the write PATs is a hard cutover: remove the write `newForgeTokenSource`
  usage, the `GITHUB_FORGE_TOKEN`/`GITHUB_FORGE_REVIEWER_TOKEN` names
  (`serve.go:186-190`), the flags. Clean cutover vs. deprecation window.
  **Recommend: clean cutover for production** (AGENTS.md default; the RIG-2883
  read cutover set the precedent), no PAT fallback — live-test carve-out per
  OQ-5. Confirm Linear: dropping `LINEAR_FORGE_TOKEN` as a *member PAT* means
  the write path uses the DL-204 OAuth actor=app token — i.e. this is
  FINISHING DL-204, not a new decision. The record treats it so.
- **OQ-5 — Live-test credential model after the PAT drop.** Production goes
  App-only, but the `livegithub` oracle uses author/reviewer PATs today
  (`livegithub_test.go:63-64`). Options: (a) live tests mint real App
  installation tokens against the testbed repo's App installs — highest
  fidelity, validates the production mint path; (b) tests keep PATs as a
  test-only carve-out. **Recommend (a) for GitHub** — the point of thread 4 is
  that tests exercise the App path — **and explicitly (b) for Linear's
  delegation setup**: keep ONE Linear USER credential
  (`LINEAR_FORGE_USER_TOKEN`, new) used only to perform the human→app
  delegation the app-actor cannot self-assign; the Linear write/notify legs
  stay on the already-minted app token (`ci.yml:1001-1003`). Tradeoff of (a):
  two more testbed App registrations + three env vars per identity instead of
  one PAT.
- **OQ-6 — Real-App webhook validation mechanism.** How do we prove a
  delivery signed by the real GitHub App / real Linear app passes our verify
  path, given a webhook delivery is a PUSH needing a public ingress the
  oracle's CI runner does not have? The two providers have materially
  different feasibility — the fork is really about the Linear half:
  - **GitHub — specified, automatable:** capture is a PULL via the App
    deliveries API (`GET /app/hook/deliveries` +
    `GET /app/hook/deliveries/{id}` returns the delivered payload and
    headers including `X-Hub-Signature-256`), so a `livegithub`-tier
    `-update` run (the oracle's existing regeneration convention,
    `livegithub_test.go:55-58`) fetches one real signed delivery and commits
    it as the replay fixture with no ingress at all. **Recommend exactly
    this.**
  - **Linear — the real fork:** no deliveries-API equivalent exists in-tree;
    capture is either (i) a human copying one delivery (raw body + signature
    header) out of the Linear webhook UI into the fixture — honest, manual,
    once — or (ii) a temporary tunnel (smee.io-style) receiving one live
    delivery. **Recommend (i)**, named as a manual step in the T6 runbook.
  Caveat either way: capture-replay proves the signature FORMAT against our
  verify path (`VerifyGitHubSignature`,
  `linearagent.VerifySignature`+`CheckTimestamp`), not live-ingress
  reachability — that is proven exactly once, manually, at registration.
  Recommend additionally a cheap ongoing liveness signal (a webhook-delivery
  freshness metric/alert on the ingest lane) so post-registration breakage
  is loud instead of masked by the 30-min reconcile backstop.

---

Ledger-impact: intended DL rows at freeze (the spawning agent lands them):
one row per ruled OQ — the App topology (OQ-1/OQ-3 likely merge into one
"two GitHub Apps: primary read+author-write, reviewer write-only" row), the
write-App config shape (OQ-2), the PAT clean-cutover incl. Linear actor=app
finish amending DL-204's implementation status (OQ-4), the live-test
credential model incl. the one-Linear-user-cred carve-out (OQ-5), and the
real-App webhook validation mechanism (OQ-6) — plus, if OQ-3's coupling
sub-point is accepted, a note amending Matt's 2026-08-19 independent-gates
ruling (`serve.go:150-151`): configuring writes now implies board ingestion
(T3). Ledger: `docs/designs/DECISIONS.md` (the one ledger in-tree; no
`docs/designs/product/DECISIONS.md` exists).
