# Forge scope enforcement — amendment: Open Question rulings

Tracker: RIG-4436

> **Amends the frozen record** [`compass-forge-scope-enforcement/design.md`](../compass-forge-scope-enforcement/design.md)
> (merged in #601). That record froze with seven load-bearing Open Questions.
> A merged record is never rewritten in place, so Matt's rulings live here.
> The frozen record's `## Open Questions` entries OQ-1, OQ-2, OQ-3, OQ-6, OQ-7,
> OQ-8 and OQ-10 are superseded by this file and read as history. OQ-9 moved to
> RIG-4433 and is not ruled here.

## Problem / Intent

The scope-enforcement record could not be executed past T3: OQ-1 blocked the
enforcement default, and OQ-8 blocked the narrowed-token tasks T4 and T5. The
brokered App token has since shipped (#2008), and it still lets a
user-declared GitHub secret win over the brokered token. That path is a PAT,
and it bypasses repository scope.

## Approach

Matt ruled on RIG-4436 (2026-10-10): "No PATs. Only App. All else lgtm." Every
recommendation in the frozen record stands except OQ-6, where the Dogfood PAT
carve-out is rejected.

### Rulings

| OQ | Ruling |
| -- | -- |
| OQ-1 | Enforcement fails closed. Scope enforcement is on unless a deployment opts out explicitly; Dogfood opts out to keep the single-trust-domain posture. |
| OQ-2 | Grants come from the declarative ForgeConfig seed plus store operations. No admin RPC for Beta. |
| OQ-3 | Grants are owner-level and apply to the owner's agents. The schema stays additive for per-agent grants. |
| OQ-6 | The GitHub App is the only GitHub credential, in every tier and on every host. No PAT path exists for Beta or Dogfood. See below. |
| OQ-7 | The git credential covers the workstream repository plus the account's write grants. Ungranted read-only dependencies are not reachable. |
| OQ-8 | The workstream repository is a server-side spawn-target record. No provision proto field is added. |
| OQ-10 | The token narrows permissions as well as repositories, to `contents: write` plus `metadata: read`. See Open Questions for the shipped `pull_requests: write`. |

### OQ-6: no PAT path

- A user-declared GitHub credential is never delivered to an agent, whether
  or not the App is configured and on any host. `SecretGH` does not
  distinguish an App token from a PAT, so the only `SecretGH` an agent
  receives is the brokered App token.
- Today two paths deliver a user credential. When the broker exists,
  `brokeredSecretResolver.ResolveFor` in `go/server/git_credential.go`
  returns a user `SecretGH` for the App host and skips the mint
  (`hasGitHubSecret`). When no App is configured, `buildGitCredentialBroker`
  returns a nil broker and `ResolveFor` returns the user secrets unchanged.
  Both paths close.
- Declaring a GitHub-kind secret (`SECRET_KIND_GH`, handled in
  `go/server/secrets_service.go`) is rejected, so a user cannot store a
  credential that would never be delivered.
- Without the App, an agent has no GitHub credential. Startup does not fail
  on that alone: today a missing App turns forge writes off with a warning
  (`wireForgeWriteCaller` in `go/server/serve.go`), and that stays.

## Plan

### A1: Remove the user-secret override

Interfaces: `brokeredSecretResolver.ResolveFor` in
`go/server/git_credential.go`; the secret kind mapping in
`go/server/secrets_service.go`.

Drop every user-declared `SecretGH` from the resolved set, then append the
brokered token when the broker exists. Apply the drop when the broker is nil
too. Reject `SECRET_KIND_GH` at declaration with an invalid-argument error.
Test with and without the App, and with and without grants: no user
`SecretGH` is delivered on any host, and declaring one fails.

### A2: Fail-closed enforcement default

Interfaces: `ForgeConfig.EnforceScopes` in `go/server/serve.go`; the
`--forge-enforce-scopes` flag and `resolveForge` in
`go/cmd/compass-server/main.go`.

A plain `bool` zero value cannot tell "unset" from "opted out", and direct
`ServeConfig` callers bypass the CLI default. Replace the field with an
opt-out (`ForgeConfig.ScopeEnforcementDisabled bool`) so the zero value
enforces, and rename the flag to `--forge-disable-scope-enforcement`.
Test that an unset config enforces through both the CLI and a direct
`ServeConfig`, and that the explicit opt-out does not.

### A3: Workstream repository record

The ruling fixes where the association lives: a server-side record written
when an agent is spawned onto a target, never a provision proto field.
Agents still self-clone after launch (DL-090); the record only adds the
workstream repository to the set the broker mints for. It widens the token
and never narrows it below the account's grants.

No server-side source for that repository exists today: `runSpawn` in
`go/server/spawn.go` and `SpawnPeerRequest` carry no repository. Choosing
the source, its storage key and its lifecycle is a design pass of its own,
filed as a follow-up record. The frozen record's T4/T5 build on it.

### Tasks

- [ ] A1: no user `SecretGH` delivered or declarable, with tests.
- [ ] A2: opt-out enforcement setting through the CLI and `ServeConfig`, with tests.
- [ ] A3: follow-up design record for the workstream repository source.

## Open Questions

- **OQ-10 permission set (load-bearing):** Matt accepted `contents: write`
  plus `metadata: read`. The broker shipped in #2008 also requests
  `pull_requests: write`, which lets an agent open a pull request with its
  git token. Either drop `pull_requests: write` from
  `gitCredentialPermissions`, so agents open pull requests only through the
  server's forge tool, or amend the ruling to keep it. Recommendation: keep
  it; agents open their own pull requests from their workspace today. Asked on
  RIG-4436.

## Ledger delta

- DL-443: the base per-account forge-scope allowlist from the frozen record,
  which never received its decision file.
- DL-441: the GitHub App is the only GitHub credential (OQ-6).
- DL-442: the remaining rulings (OQ-1, OQ-2, OQ-3, OQ-7, OQ-8, OQ-10).
