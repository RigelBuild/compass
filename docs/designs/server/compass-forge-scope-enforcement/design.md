# Design: Forge scope enforcement (A8, Beta tier)

Owner lane: compass-server. The git credential leg uses the existing Runner secret path. Refs: RIG-2679, RIG-2672, RIG-2732, RIG-3069.

## Problem / Intent

The forge write caller contract still says “MVP scope ships no scope rejection (single-trust-domain, Resolved decision 2)” (`ForgeCaller` in `go/internal/runnerhub/relay_forge.go`). That deferral is unsafe for Beta: the shared forge credential reaches multiple repositories, and `repo` is caller-controlled. Enforce per-account repository scope at the API write chokepoint and narrow the GitHub installation token delivered to each account, while retaining the existing workspace secret-delivery path.

Matt ruled scope enforcement mandatory for Beta, including clone/push/pull. Dogfood may retain the single-trust-domain defer. This record covers the write API and GitHub git credential; it does not gate API reads or define a Linear git path.

## Global Constraints

- Reject out-of-scope writes in-band as the same `not_found` result used for missing/forbidden forge artifacts. Do not expose an authorization oracle.
- Grants belong to an owning user and apply to that user's agents; agent self-grant is not allowed. GitHub repo names are normalized consistently; Linear team keys remain verbatim.
- Scope data is authorization data and MUST receive tenant RLS. Keep table creation and RLS enrollment in the same applicable migration. Confirm the zero-deployed-database premise before editing `0001_init.sql`; if it no longer holds, use a new migration containing both table and policy.
- The shipped GitHub App credential is not a PAT. `forge.NewAppTokenSource` in `go/internal/forge/githubapp.go` is wired in `go/server/serve.go`. Its ordinary source caches one installation token; per-account narrowed mints need a sibling minter and must account for GitHub installation rate limits.
- Production does not set `Workspace.Credentials` today. The existing container credential is `SecretGH`, installed by `SecretMaterializer.Install` into hosts.yml (`go/internal/runtime/secrets_materialize.go`). The narrowed token MUST ride that existing secret path; do not add a proto credential field.
- Public-repo content MUST NOT refer to private repositories.

## Approach

Add `account_forge_scopes`, keyed by tenant, account, forge provider, host, and repo. Exact repo rows grant one repo; `*` grants the coordinate. A store check accepts an agent's own or owning user's grant. Seed initial grants declaratively from ForgeConfig and expose store grant/revoke operations; agents never manage their own grants.

At `ExecuteForgeCallAsAccount` in `go/server/forge.go`, check the resolved coordinate and repo before each coordinate-keyed write. For create operations, check after the F3 dedup lookup: a memo hit returns an existing artifact and performs no write. Comment, review, subscribe, and state-transition operations (`transitionIssueState`, `transitionPullRequestState`) check after target resolution. `unsubscribeForge` is caller-scoped by subscription id in the store and has no repo coordinate to check. Keep reads ungated in this slice. Test the oneof arm classification and ensure every coordinate write is gated.

For git access, mint a GitHub App installation token restricted through GitHub's `repositories`/`repository_ids` field to the account's granted repos. The credential is delivered as `SecretGH` through `Host.Start` → `FetchSecretsByContainer` → `materializer.Install`, preserving the existing Runner pull and hosts.yml materialization flow. Wildcard means no repository narrowing beyond the installation. The token's permissions remain bounded by the App installation. A per-account mint must be separate from `appTokenSource.mint`, which caches one token per installation; set an explicit mint rate budget and avoid minting on every fetch.

A5 remains blocked on OQ-8: the server has no workstream-repository input because RIG-1527 removed repo carriage. Do not silently restore that input. GitHub installation tokens expire after about an hour, so the refresh cadence and revocation window must be specified before implementation.

The existing path is `Host.Start` → `FetchSecretsByContainer` → `materializer.Install` (`go/internal/runner/host.go`, `go/internal/runner/secrets_fetch.go`, `go/internal/runtime/secrets_materialize.go`): it installs resolved `SecretGH` values into hosts.yml. Production does not set `Workspace.Credentials`, so it is not the credential seam for this change.

### Alternatives considered

- Reuse `forge_repo_subscriptions`: rejected because it is deployment-wide board poll configuration, not per-account authorization.
- Put repo in the provider credential key: rejected because it duplicates credentials per repo and does not express account grants.
- In-container helper or server git proxy: rejected as heavier than GitHub-native token narrowing; the helper is controlled by the agent and the proxy adds an always-on data-path service.

## Plan

### T1 — Store scopes

Interfaces: `ForgeScope{AccountID, Provider, Host, Repo}`; `GrantForgeScope`, `RevokeForgeScope`, `HasForgeScope`, and `ListForgeScopeRepos` on `Store`.

Create the tenant-isolated table and methods. Normalize GitHub repo names at grant and check boundaries, preserve Linear team keys, support owner inheritance and wildcard, and test RLS plus grant/revoke behavior.

### T2 — Gate API writes

Interfaces: extend `forgeStore` with `HasForgeScope`; add `forgeService.requireForgeScope`.

Gate create arms after dedup, and comment/review/subscribe/transition arms after target resolution. Preserve caller-scoped unsubscribe. Return the fixed in-band not-found error. Add default-lane tests for allowed and rejected writes (including issue and PR state transitions), zero provider calls on rejection, create memo hits, subscribe, unsubscribe ownership, store errors, and a descriptor-based arm classification/signature cross-check.

### T3 — Configure grants and enforcement

Interfaces: `ForgeConfig.EnforceScopes`, `ForgeConfig.ScopeGrants`, and a scope seed reconciler in `go/server/serve.go`.

Wire the enforcement setting and declarative account grants into service assembly. Warn when enforcement is on with no grants. Test parsing, reconciliation, and the enforcement flag. OQ-1 determines whether the default is fail-open or fail-closed; keep that decision explicit.

### T4 — Per-account narrowed App mint

Interfaces: store method `ListForgeScopeRepos`; a sibling App minter near `appTokenSource.mint` that accepts account and host and returns token plus expiry.

Resolve exact grant repos and wildcard. Mint with repository IDs where needed to avoid same-name owner aliasing. Budget mints against the GitHub App installation's rate limit. OQ-8 blocks the required workstream repo input; OQ-7 determines whether grants also define the read/clone set. Test exact, wildcard, empty, cross-owner, and installation-boundary cases.

### T5 — Existing Runner secret delivery and refresh

Interfaces: no new proto fields. Use `Host.Start` → `FetchSecretsByContainer` → `materializer.Install` and resolved `SecretGH` values.

Deliver the narrowed token as the GitHub host's SecretGH value so hosts.yml receives it through the established materializer. Specify refresh before token expiry and preserve the prior usable credential on mint failure. Ensure host entries are not clobbered during refresh. OQ-8 blocks implementation; bound and coordinate refresh mints with the same rate budget.

### Tasks

- [ ] T1 — Tenant-isolated scope store, grant operations, owner inheritance, wildcard, normalization, and tests.
- [ ] T2 — In-band scope gate for coordinate writes, caller-scoped unsubscribe coverage, and exhaustive write-arm tests.
- [ ] T3 — Config seed, service wiring, enforcement setting, and tests.
- [ ] T4 — Per-account narrowed App mint, repository-ID handling, rate budget, and tests; blocked on OQ-8.
- [ ] T5 — Route narrowed token through existing SecretGH fetch/materialization and define safe refresh; blocked on OQ-8.

## Ledger delta

Append one Comms & tools row at ledger assembly time; derive the next available DL id from `docs/designs/DECISIONS.md` then. The decision is a per-account forge-scope allowlist enforced at API write calls and through a GitHub installation token delivered on the existing Runner secret path. This refines the deferred A8 posture without superseding the ForgeCaller seam decision.

Ledger-impact: adds one row (Comms & tools); refines A8 without superseding the ForgeCaller seam decision.

## Open Questions

- **OQ-1 — Enforcement default:** fail-open default false preserves Dogfood behavior but risks Beta misconfiguration; fail-closed requires an explicit Dogfood opt-out. Recommendation: fail closed. Matt decides.
- **OQ-2 — Operator grant surface:** are declarative config and store operations sufficient for Beta, or is an admin RPC required at launch? Recommendation: config seed for this slice.
- **OQ-3 — Grant granularity:** grants inherit from the owning user or must be per-agent? Recommendation: owner-level MVP; schema remains additive for per-agent grants.
- **OQ-6 — Git credential fallback:** the App path is shipped. Decide whether Beta may run without the App and, if so, whether to warn, fail startup, or accept a deployment-wide fine-grained PAT. No App sequencing question remains.
- **OQ-7 — Read/clone scope:** should the GitHub credential include only the workstream repo plus write grants, or a distinct read set for dependencies? Recommendation: one write set plus workstream repo; this loses access to ungranted read-only dependencies.
- **OQ-8 — Workstream repository association:** RIG-1527 removed repo carriage; no server-side workstream repo is available. Reintroducing an association reverses that decision and blocks T4/T5. Decide whether and how to record the association before implementation. Recommendation: server-side spawn-target record, not a new provision proto field.
- OQ-4 (read arms) and OQ-5 (wildcard grammar) were non-load-bearing and are settled above: reads stay ungated, and `*` is the only wildcard.
