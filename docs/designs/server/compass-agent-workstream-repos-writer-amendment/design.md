# Agent workstream repository writer — amendment: last-credential guard

Tracker: RIG-5015

> **Amends the frozen record** [`compass-agent-workstream-repos-writer/design.md`](../compass-agent-workstream-repos-writer/design.md).
> A merged record is never rewritten in place. This file replaces the
> fallback rule in its `### Transient mint failure keeps the previous token`
> and `### T3` sections. Everything else in that record stands.

## Problem / Intent

The frozen record serves the agent's last delivered key on a transient mint
failure, with no condition. Review of the implementation found that this
undoes a revoke:

1. An agent holds key K1 (owner grants plus a repository R).
2. The owner removes R, so the agent's set becomes K2, which has no entry yet.
3. The K2 mint fails with 429. The broker serves K1's stale token.
4. `staleCredential` updates K1's `lastUsed`, so `refreshDue` keeps K1 and
   re-mints it. Each new K1 token reaches R again.

The record accepts that a removed repository stays reachable for up to one
hour (OQ-4). This loop has no bound.

## Approach

The last-key fallback serves only when the last key grants no repository
outside the key the agent asked for now.

- Both keys are the canonical, owner-qualified, lowercased lists that
  `gitCredentialScope` in `go/server/git_credential.go` builds.
- A requested `*` covers any last key. A last key of `*` is served only when
  `*` is requested.
- When the guard refuses, `credential` returns no token. The materializer then
  removes the GitHub credential until a mint succeeds. A narrowed set fails
  closed, as the revoke intends.

The record's own case still works. After a grant, the agent asks for the owner
set plus the new row. The last key is the owner set, which is a subset, so the
fallback serves it.

## Plan

### T3 — guard on the last-key fallback

In `go/server/git_credential.go`, `credentialForLastKey(agent, requested)`
returns the last key's stale token only when
`gitCredentialScopeWithin(last, requested)` holds. The broker records in
`last[agent]` only a key whose token it returned and which is still cached.

Tests in `go/server/git_credential_test.go`:

- A widened set that fails twice in a row with 429 serves the owner-set token
  both times.
- After a revoke narrows a finite set, a 429 on the narrowed set serves
  nothing, and the wider key is not minted again.
- A `*` token narrowed to one exact repository serves nothing on a 429.

## Tasks

- [x] T3 guard and its three tests, in the writer's implementation stack.
