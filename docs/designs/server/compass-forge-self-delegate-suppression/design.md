# Design: Forge self-origin notification suppression

Status: Active

## Problem / Intent

When a Compass agent takes a forge action on an artifact it is also subscribed
to — it comments on its own PR, files-and-watches an issue, transitions its own
issue — the forge notification pipeline fans the resulting change straight back
to the acting agent: `NotifyRouter.Route` step 6
(`go/internal/ingest/notify_router.go:230-243`) dispatches to every subscriber
returned by `SubscribersForArtifact` (`notify_router.go:225`) with no identity
check, so the agent is notified of its OWN action. This record suppresses that
self-origin notification (RIG-3326), under Matt's three hard rulings:

1. Suppress COMMENT / REVIEW / OPENED / STATE when the actor IS the subscriber.
   The STATE arm keys on the acting agent of a real state-transition write
   (RIG-3331, the forge state-transition op Matt ruled must be built — agents
   set issue/PR state on GitHub AND Linear), NOT the DL-055 author-row proxy
   (§STATE below).
2. NEVER suppress CHECKS — CI results on an agent's own push are the point of
   watching CI, and a CHECKS event carries no actor anyway.
3. The identity match keys on Compass HANDLES, never internal account ids, and
   the handle is OWNER-QUALIFIED (owner-handle + agent-handle), because a bare
   agent handle is unique only per owner (§handles below).

This is the suppression sibling of the frozen Record A ("forge self-delegate
write path", `docs/designs/server/compass-forge-self-delegate/design.md`,
PR #900), which is write-path-only and explicitly defers suppression here
(Record A design.md:298-302). The write path is not re-designed here; Record A
is referenced only where the loop risk originates.

## Approach

### Where the filter lives

One suppression predicate in the notify-router fan-out loop
(`notify_router.go:230-243`), evaluated per subscriber immediately before
`r.dispatcher.Notify(ctx, sub.AgentAccountID, n)` (`notify_router.go:233`).
The router computes the event's ACTOR handle once per `Route` call (it is a
property of the event, not the subscriber), then per subscriber resolves the
SUBSCRIBER handle and skips the dispatch when the two are equal and non-empty.

The `ingest` package deliberately never imports the store (the no-store rule,
`notify_router.go:15-19`), so identity resolution enters through a new
package-local seam, mirroring how `NotifyStore` / `NotifyDispatcher` /
`ChecksRoller` are shaped (`notify_router.go:63-113`) and how `linearagent`
carved `OwnershipIndex` (`go/internal/linearagent/routing.go:42-44`):

```go
// IdentityResolver resolves OWNER-QUALIFIED Compass handles for self-origin
// suppression (package ingest). A zero Handle (empty ownerHandle OR empty
// agentHandle) with a nil error is a clean miss — the caller MUST fail open
// (deliver). A non-nil error is a store fault — log and fail open.
type Handle struct{ Owner, Agent string } // both must be non-empty to match

type IdentityResolver interface {
  // HandleForAccount resolves an agent account id to its owner-qualified
  // Compass handle.
  HandleForAccount(ctx context.Context, accountID string) (Handle, error)
  // AuthorHandle resolves the DL-055 ownership row's recorded authoring
  // agent at a coordinate to its owner-qualified handle. (provider, host)
  // are bound by the server adapter, like NotifyStore.
  AuthorHandle(ctx context.Context, repo string, kind compassv1internal.ForgeArtifactKind, number uint64) (Handle, error)
}
```

Two owner-qualified handles match iff `a.Owner == b.Owner && a.Agent == b.Agent`
and all four components are non-empty.

**The actor's owner is not on the wire today — this record puts it back
(Matt, 2026-09-05).** The parse already produces it: `StripOwner` yields
`forge.Author{AgentHandle, OwnerHandle, SessionID}`
(`go/internal/forge/owner.go:26,:162`). It is then DISCARDED at all three sites
that build the `CommentRef` the router reads —
`githubapp_webhook.go:280`, `linearagent/data_event.go:177`,
`ingest/notify_detect.go:328` — because the type has nowhere to put it:
`compass.v1.AgentAttribution` is `{agent_handle}` only
(`proto/compass/v1/compass.proto:909-911`). Left as-is, every COMMENT/REVIEW
actor resolves to `Handle{Owner: "", Agent: …}` → unqualified → the fail-open
rule DELIVERS, and the record's primary case (an agent commenting on its own PR)
could never suppress.

So **T0 restores `owner_handle` on `AgentAttribution`** and populates it at
those three sites from `author.OwnerHandle`. This AMENDS the surviving
"owner … never restated per artifact" clause that DL-094 wrote and DL-186
carried forward: that clause was scoped to attribution as a DISPLAY fact (the
board renders a bare `@handle`, so per-artifact owner was dead weight), and its
"resolved server-side" escape hatch does not survive a MATCHING consumer — the
only server-side key available for a comment actor IS the bare handle, which is
unique only per owner, the exact ambiguity owner-qualification exists to
resolve. Neither DL-094 nor DL-186 weighed an identity-matching consumer; this
record adds one, so the clause is amended rather than reversed (§Ledger delta
carries the superseding row; DL-186's field-number reclaim is what makes the
slot free to reuse).

With T0 landed, the actor's owner comes from the event; the OPENED/STATE and
subscriber sides come from the store.

The go/server adapter (wired in the two sibling lane builders —
`buildForgeNotifyLane` for GitHub (`go/server/serve.go:1389`, router at `:1406`)
and `buildLinearNotifyLane` for Linear (`:1431`, router at `:1453`)) binds
(provider, host) and backs the two methods with existing store reads. Note both
are TWO PK reads, not one — `GetAccount` returns the owner as an *id*
(`AgentAccount.OwnerUserID AccountID`, `go/internal/store/types.go:162-164`),
not a handle, and no id→handle projection exists (every handle helper in
`accounts.go:837-897` resolves handle→id, the opposite direction):

- `HandleForAccount` → `store.GetAccount(ctx, id)` for the agent handle +
  `OwnerUserID` (`go/internal/store/accounts.go:458-468`), then
  `store.GetAccount(ctx, OwnerUserID)` for the owner's `.Handle`;
  `ErrNotFound` at EITHER step → clean miss.
- `AuthorHandle` → `store.AuthoredArtifactByCoordinate(...)`
  (`go/internal/store/forge_authored.go:144`) for the recorded
  `AgentAccountID` (`forge_authored.go:36-49`), then the same two-read
  `HandleForAccount` resolution; `ErrNotFound` at any step → clean miss.

A nil `IdentityResolver` disables suppression entirely (every dispatch
delivered) — the seam's zero value IS the fail-open posture, which also keeps
the existing router tests valid until they opt in.

### Why owner-qualified handles, not account ids (Matt's hard rule)

The obvious cheap match — `authorRow.AgentAccountID == sub.AgentAccountID` —
is rejected. Handles are the stable identity the rest of the addressing layer
keys on (attribution carries `AgentAttribution.AgentHandle`,
`proto/compass/v1/compass.proto:909-911` — note the message has ONLY that one
field, which is load-bearing below; the handle resolvers in
`accounts.go:837-897` are the addressing chokepoint), while an account id is an
internal storage key behind the account/handle indirection: an agent identity
that is re-provisioned, or reached through different id-carrying paths (a
recorded ownership row vs a live subscription row), can present different ids
for the same logical agent, and the COMMENT/REVIEW actor arrives as a handle
with no id at all. Normalizing every actor source to a handle and comparing
handle-to-handle gives one comparison domain across all three sources, id
drift excluded by construction.

**Owner-qualified, because a bare handle is not globally unique.** The agent
handle's uniqueness key is `(tenant_id, owner_user_id, handle)`
(`account_handles_owner_key`, `go/internal/store/migrations/0001_init.sql:158`
— contrast the bare user/system key at `:157`, which IS tenant-global; the
schema comment there states the two-namespace contract outright: "agent handles
(owner_user_id IS NOT NULL) are unique only per owner"), so two DIFFERENT
owners' agents
may both legally be handle `atlas`. A BARE-handle actor==subscriber match would
then FALSE-suppress a genuine cross-agent notification between `atlas@owner-A`
and `atlas@owner-B` — a fail-CLOSED bug, the exact class fail-open exists to
prevent. So the match keys on the OWNER-QUALIFIED handle pair
(owner-handle + agent-handle), still handles (Matt's rule holds), just the full
addressing tuple. `StripOwner` already yields the owner handle; the resolver
returns an `(ownerHandle, agentHandle)` pair for every actor source, and
suppression fires only when BOTH components match and are non-empty. Either side
unqualified → fail open. A mid-flight handle RENAME makes the same agent present
two handles across the stamped-header path vs the live `GetAccount` path → a
missed suppression (fail-open, acceptable — a redundant self-wake, never a lost
cross-agent signal).

### Actor identity by change kind — the suppress/keep matrix

| Kind | Actor source | Behavior |
| --- | --- | --- |
| COMMENT | `ev.Comment.Agent.AgentHandle` (`go/internal/forge/notify_event.go:36-38`; `CommentRef.Agent` set only for a Compass commenter, `go/internal/gen/compass/v1/forge.pb.go:187-188`) | Suppress on handle match; a human commenter (`Agent` unset) always delivers |
| REVIEW | same `CommentRef` source (the notification builder treats REVIEW as COMMENT-plus-verdict, `notify_router.go:291-296`) | Suppress on handle match |
| OPENED | DL-055 ownership row's recorded author → owner-qualified handle (`IdentityResolver.AuthorHandle`); on OPENED the author IS the actor by construction — the row is written strictly after the create that fired the event (`go/server/forge.go:347-360`, DL-205). NOTE the webhook races the row write: the provider's `opened` webhook can reach the router before the DL-055 row commits → `AuthorHandle` clean miss → the self-notification DELIVERS (fail-open, so correct, just intermittently a no-op under real webhook latency) | Suppress on handle match |
| STATE | `ev.Actor` — the owner-qualified attribution stamped by the RIG-3331 state-transition op onto the `ForgeEvent.Actor` field T0 adds (symmetric with COMMENT/REVIEW's `CommentRef.Agent`), NOT the author-row proxy (§STATE below). Nil on every provider-parsed event, so STATE is interim-open until RIG-3331 lands | Suppress on owner-qualified handle match |
| CHECKS | none — a CHECKS event carries only the roll-up + head SHA (`notify_event.go:39-43`) | NEVER suppressed (invariant, dedicated test) |
| UPDATE | none | Never suppressed (no actor evidence → fail open) |

**The STATE arm keys on the real transition actor (RIG-3331), not a proxy.**
Unlike OPENED, a STATE event's actor is NOT necessarily the artifact's author.
The forge write surface has no state-transition op today (the `forgeService`
arms are create / comment / review / get / list / subscribe —
`go/server/forge.go:369,402,442,465,493,536,554,569`), so every STATE event
reaching the router TODAY is human/external-actored (a GitHub `closed`/`reopened`,
`go/internal/forge/githubapp_webhook.go:175-183`; a Linear workflow-state
change, `go/internal/linearagent/data_event.go:111-116`). Keying STATE on the
DL-055 author-row proxy would therefore suppress the authoring agent's
notification of a HUMAN closing its issue — the exact cross-actor signal
suppression must never eat. Matt ruled (2026-09-05) the fix is NOT to narrow
STATE but to BUILD the missing agent-driven state-transition write op
(**RIG-3331**: agents set issue/PR state on GitHub AND Linear), which stamps the
acting agent's identity on the emitted event. STATE suppression then keys on
THAT real actor handle — an agent's own transition is suppressed, a human's
close of the agent's issue is NOT (the human is not a Compass handle, so the
match fails → deliver).

**The carrier is frozen HERE, not left to RIG-3331 to invent.** `ForgeEvent`
carries no actor field today (`go/internal/forge/notify_event.go:19-49` is
Provider / Host / Repo / Kind / Number / Project / URL / Change / Comment /
Checks / HeadSHA / State / DeliveryID) — COMMENT/REVIEW are "symmetric" only
because `Comment *CommentRef` exists to hold their actor, and STATE has no
counterpart. So T0 adds `ForgeEvent.Actor *compassv1.AgentAttribution`,
owner-qualified by the same T0 proto change, set ONLY by the RIG-3331 op's
emitted event and nil on every provider-parsed event. T1's `actorHandle` STATE
arm reads `ev.Actor`.

**Dependency:** with the carrier frozen, the interim is true by construction
rather than by absence — until RIG-3331 populates `ev.Actor`, STATE resolves a
zero Handle, every match fails, and STATE delivers (the safe interim fail-open,
not a wrong suppression). RIG-3331 sequences before STATE suppression is
relied on; nothing here blocks on it.

### Fail-open, precisely

Suppression fires only on a POSITIVE owner-qualified identity match: both
handles resolved, all four components (both owners, both agents) non-empty, and
the pair equal. Every ambiguous state delivers:

- `CommentRef.Agent` unset (human commenter) → deliver.
- Either side's owner OR agent component empty (unqualified) → deliver.
- No ownership row at the coordinate (`ErrNotFound`) → deliver.
- `GetAccount` miss on either side → deliver.
- Any store fault from the resolver → log at warn, deliver.
- Nil `IdentityResolver` → deliver.

Rationale: suppressing a real cross-agent notification is strictly worse than a
redundant self-notification — the former silently loses work-relevant signal,
the latter costs one wasted wake.

### Suppression advances the delivery cursor — but ONLY when the subscriber was caught up

Skipping the dispatch alone is not enough. The subscriber's per-artifact
delivery cursor (`delivered_revision`) advances only via the hub's ack arm (W3,
`notify_router.go:19-20`), and the reconcile sweep re-notifies any subscriber
whose `delivered_revision` differs from the shared cursor revision by
synthesizing a payload-free UPDATE (`SynthesizeUpdate`, `notify_router.go:247-255`;
the sweep check is inequality-based — the cursor is "last revision told about",
not an ordered value). A suppressed notification is never acked, so the gap
persists and the sweep would deliver the self-notification anyway — as a
synthetic UPDATE, every sweep until acked. Therefore the suppress path advances
the subscriber's `delivered_revision` to the event's revision, through a new
method on the `NotifyStore` seam adapted over the existing
`store.AdvanceForgeDeliveredRevision`
(`go/internal/store/forge_subscriptions.go:467-478`, today called only from the
ack arm). This is a deliberate, narrow amendment to W3's "the router never
advances delivered_revision": a suppressed notification has no agent to ack it,
so the advance is the delivery outcome.

**The advance is CONDITIONAL — advance only when the subscriber was fully
caught up before this event.** The unconditional advance is UNSAFE: consider a
real cross-actor event E1 at revision R1 dispatched but never delivered (a
dispatcher error — the loop logs and continues, `notify_router.go:233-243` — or
a session that dies before the turn-end ack). W3's safety net is the sweep:
`delivered != cursor` → synthetic UPDATE. If a later self-origin event E2 at R2
is suppressed and UNCONDITIONALLY advances `delivered_revision` to R2 (= the
current cursor), the gap is erased, the sweep synthesizes nothing, and the agent
NEVER learns of E1 (e.g. a human's "please fix X" on its PR) — the exact
"silently loses work-relevant signal" outcome the fail-open rationale calls
strictly worse than a redundant wake. So the suppress path advances iff the
subscriber is CAUGHT UP: `sub.DeliveredRevision` (from step 5,
`notify_router.go:225`; field at `:30`) equals the PRIOR cursor revision —
`cur.Revision` when `cur != nil` (loaded at step 1,
`notify_router.go:157-158`, field at `:47`), and `""` when `cur` is nil, i.e.
the coordinate was never observed (the seam contract at
`notify_router.go:79-81`, enforced in the adapter at `serve.go:1223`), which
correctly matches a fresh subscriber's default. `cur` is a `*ArtifactCursor`,
so that nil guard is REQUIRED, not defensive — follow the existing guard style
at `notify_router.go:164-165` / `:207-208`. That equality is the caught-up
TEST, evaluated in Go: two distinct reads of two tables, five steps apart, not
one value. If the subscriber trails, skip the advance and accept one synthetic
UPDATE on the next sweep — which is then CORRECT, because the subscriber
genuinely has the missed E1 to catch up on (the sweep UPDATE covers
both). The write is then guarded by a SEPARATE compare-and-set in SQL
(`... AND delivered_revision = $prior`, with `prior = sub.DeliveredRevision`)
rather than a read-then-write in Go, so a concurrent route cannot erase a gap it
did not see — **required, not optional** (Global Constraints; T2 lands the
query). A lost CAS degrades open (one synthetic UPDATE), never closed. An
advance failure is logged and swallowed (worst case: the sweep synthesizes one
UPDATE — fail-open degradation, never a route failure).

**Scope carve-out: the advance is ARTIFACT-scope only.** OPENED is the one kind
whose fan-out deliberately includes CONTAINER-scope subscribers
(`SubscribersForArtifact` adds `scope = 2 AND number = 0 AND project = $7` when
`opened` is true, `store/queries/forge_subscriptions.sql:50-53`). Those cursors
are a different row against a different revision: a container subscriber's
`delivered_revision` lives on its `number = 0` row and the sweep compares it
against the CONTAINER cursor's revision (`ListForgeNotifyTargets` collapses
`scope = 2` to `coord_number 0`, `forge_subscriptions.sql:60-71`; the compare is
`notify_reconcile.go:201-206`), whereas `Route` computes `revision` for the
OPENED ARTIFACT's snapshot (`notify_router.go:196-197`). Writing an artifact
revision into a container cursor poisons that row — the container sweep would
then compare its own revision against a value that never equals it, synthesizing
an UPDATE every sweep. So a suppressed OPENED dispatch to a CONTAINER-scope
subscriber skips the dispatch and NEVER advances; the next container sweep may
synthesize one UPDATE, the same fail-open degradation accepted above. This
requires `NotifySubscriber` to carry its scope, which nothing carries today —
not the router struct (`notify_router.go:27-32` is SubscriptionID /
AgentAccountID / DeliveredRevision / Project), and not the store beneath it —
NEITHER `SubscribersForArtifact` nor `ListForgeNotifyTargets` projects scope,
though both reference it in their predicates. T1 widens every hop, both store
queries and both mappers included; see its Interfaces block.

### The DL-050/DL-094 tension on the COMMENT/REVIEW actor

`CommentRef.Agent` is populated by `StripOwner` parsing the owner header out of
the comment body (`githubapp_webhook.go:272-281`,
`data_event.go:168-178`), and DL-050/DL-094 forbid a parsed header from
reaching a ROUTING decision (`routing.go:8-11`). Suppression is not a routing
decision — it selects no responder and grants no authority; it is a delivery
filter whose worst-case abuse is: a non-Compass writer hand-forges a header
naming agent X, and X misses the notification of that ONE forged comment. The
header is server-stamped on every Compass write (DL-204's one chokepoint), so
Compass-originated attribution is trustworthy; the forgery window is bounded to
one missed notification of the forged comment itself (recoverable by reading the
artifact), strictly smaller than the alternative of matching on nothing. The
caught-up guard above is load-bearing here: WITHOUT it, an unconditional advance
would let a forged header be an unauthenticated remote primitive that advances
the victim's delivery cursor — composing with the forward-masking race to
permanently erase a pending recovery for an earlier undelivered REAL
notification. WITH the guard, the forged-comment suppression can advance the
cursor only across the forged comment's own revision AND only when the victim
was already caught up, collapsing the window back to the bounded one-missed-
comment shape. Recorded here so the exception is explicit rather than an
unnoticed DL-050 erosion.

### Surface (2): the Linear agent-session webhook

The self-origin case on `internal/linearagent` is: Record A's app-set
`delegateId` on a compass-authored issue fires a `created` AgentSessionEvent
back into our own responder, which routes it to the owning Manager
(`routing.go:91-115`) — a spurious session per self-delegated create. **This
cannot fire in Record A's world**: whether an app-set `delegateId` fires
`created` at all is unproven (observing it needs the public webhook ingress
only Record B builds — Record A design.md:127-135, RIG-3271-gated), and Record
A deliberately wires no production caller to `DelegateSelf: true` until Record
B answers (Record A design.md:137-142).

`SessionEvent` carries NO actor field (`go/internal/linearagent/webhook.go:19-35`)
and responder routing keys only on the issue coordinate, so surface (2) cannot
run the handle match at all. The ready seam this record defines (and Record B
implements, if its probe answers "fires"):

- **Insertion point:** the webhook handler's `created` arm, before
  `Dispatcher.Enqueue` — drop-before-enqueue, mirroring the router's
  skip-before-dispatch.
- **Signal:** self-origin is known at the SOURCE, not inferred at the sink.
  When the Server's create arm performs a self-delegate write, it already
  writes the DL-055 row in the same flow (`forge.go:347-360`); the seam
  contract is a "self-delegated by app at T" marker recorded with that write
  (a `self_delegated_at_unix_ms` column on `forge_authored_artifacts` is the
  natural shape), checked by the `created` arm: marker present and fresh →
  drop the session-create. This keeps surface (2) source-tagged rather than
  header-parsed, with no DL-050 exposure.
- **The marker is SINGLE-USE — the `created` arm CONSUMES it** (clears
  `self_delegated_at_unix_ms` on first match, in the same transaction as the
  drop). This is load-bearing, not an optimization. The marker keys on the
  COORDINATE, not on the event, so a purely time-windowed check would drop ANY
  `created` event on that issue inside the window — including a HUMAN's
  delegation or mention on the agent's own issue. That is the cross-actor
  signal suppression must never eat, and it is exactly why §Alternatives
  rejects the recency window on surface (1). Consuming the marker collapses the
  window to AT MOST ONE dropped session — the one the self-delegate write
  minted it for. The residual is an honest, bounded **fail-CLOSED** exception to
  the otherwise-absolute fail-open rule (surface (2) has no actor on its events,
  so it cannot be fail-open in surface (1)'s sense): if a human's `created`
  event races ahead of the app's own, it consumes the marker and is dropped,
  and the app's event then delivers. Bounded to one event, counted and logged,
  and the issue itself stays readable — recorded here rather than left as an
  unnoticed contradiction of the Global Constraint.
- **Not landed here:** shipping the gate before Record B proves the webhook
  fires would be dead code guarding an unobservable path. This record freezes
  the contract; Record B lands the column + check with its ingress (T4).

### Ledger delta (described here; the append happens in this record's PR)

TWO new rows in `docs/designs/DECISIONS.md` (the server ledger). DL IDs are
GLOBAL and the space is heavily contended by in-flight PRs — a cross-branch
sweep at freeze time found DL-327 (SubjectService) and DL-328 (gateway-creds
encryption) on main, and DL-329 (RIG-3239 supervision), DL-330 (Record A on
PR 900, also grabbed by the apple-container branch — a separate collision those
lanes resolve), and DL-331 / DL-332 / DL-333 (compass-managed delivery-cutover)
all claimed on open design branches — so this record claims the true next-free
**DL-338**
(re-confirm against every OPEN design PR's ledger at freeze, not just main, since
the space moves under you):

> DL-338 | Forge notifications are self-origin-suppressed at the notify-router
> fan-out: a COMMENT / REVIEW / OPENED / STATE notification is skipped when the
> event actor's OWNER-QUALIFIED Compass handle (owner-handle + agent-handle,
> never account ids; a bare agent handle is unique only per owner) equals the
> subscriber's — actor = `CommentRef.Agent.AgentHandle` + owner for
> COMMENT/REVIEW, the DL-055 ownership row's author for OPENED, and the RIG-3331
> state-transition op's stamped acting agent for STATE (NOT the author-row
> proxy) — failing OPEN on any unresolved/unqualified/ambiguous identity; CHECKS
> (and UPDATE) are NEVER suppressed; a suppressed notification advances the
> subscriber's `delivered_revision` ONLY when it was already caught up to the
> prior cursor (the one router-side advance, amending W3, CAS-guarded) and only
> for ARTIFACT-scope subscriptions (a CONTAINER-scope subscriber is skipped but
> never advanced — an artifact revision in a container cursor poisons the
> container sweep), so the reconcile sweep neither resurrects it nor masks a
> prior undelivered real notification; the Linear agent-session surface gets a source-tagged
> drop-before-enqueue seam, implementation Record-B-gated. STATE suppression is
> correct only once RIG-3331 lands (stamps the real actor); until then STATE
> has no Compass actor and delivers (safe interim).

Plus the owner-carriage amendment (**DL-339**), which T0 implements. DL-094 is
already `Superseded by DL-186`, and DL-186 (Active) carried its
"never restated per artifact" clause forward while freeing the field numbers —
so DL-186 STAYS Active (its wire-compat-strip clause, the row's actual subject,
is untouched) and this new row carries the amended-clause scope. That follows
the DL-308/DL-324 precedent (`DECISIONS.md:209,211`): a row whose OTHER clauses
stand stays Active, and the amending row carries the narrowed scope — flipping
DL-186 to Superseded would falsely retire the wire-compat strip:

> DL-339 | `compass.v1.AgentAttribution` regains `owner_handle` (field 2, the
> slot DL-186's wire-compat strip freed), populated at the three parse sites
> that already produce it and discard it (`githubapp_webhook.go:280`,
> `linearagent/data_event.go:177`, `ingest/notify_detect.go:328`) from
> `forge.Author.OwnerHandle`. AMENDS — does not reverse — the
> "owner is an account property, resolved server-side, never restated per
> artifact" clause DL-094 wrote and DL-186 carried forward: that clause was
> scoped to attribution as a DISPLAY fact, and its "resolved server-side"
> escape hatch does not survive an identity-MATCHING consumer, because the only
> server-side key available for a comment actor is the bare agent handle, which
> is unique only per `(tenant_id, owner_user_id, handle)` — the exact ambiguity
> owner-qualification exists to resolve. Neither DL-094 nor DL-186 weighed a
> matching consumer; DL-338's suppression is the first. Attribution stays a
> display fact, not a trust claim (DL-094's core semantics unchanged) — it now
> simply carries both halves of the identity it already parsed.
> `forge.ForgeEvent` additionally gains an internal `Actor` field carrying the
> same owner-qualified attribution for kinds with no `CommentRef` (STATE, via
> RIG-3331). DL-186 stays Active (its wire-compat-strip clause is untouched)
> (Matt, 2026-09-05).

No existing ROW is mutated (the ledger is append-only and Decision cells are
immutable): DL-339 carries the amended-clause scope as a new row, and DL-186
stays Active. DL-205/DL-255 and RIG-3331 are load-bearing dependencies, not
changed.

## Alternatives considered

- **Account-id match** (`authorRow.AgentAccountID == sub.AgentAccountID`, plus
  handle→id resolution for the COMMENT source via `globalHandleID`,
  `accounts.go:870-879`): cheaper (no per-subscriber `GetAccount`), but
  rejected per Matt's explicit ruling — ids are internal storage keys behind
  the account/handle indirection and can drift across re-provisioning and
  across the different id-carrying paths, while handles are the addressing
  layer's stable identity. Not taken.
- **Recency window against the DL-205 authored-artifact write** (suppress any
  notification to the author within N seconds of the ownership row's
  `CreatedAtUnixMS`, no actor identity at all): attribution-free, so it also
  covers surfaces with no actor field. Rejected for surface (1): it is a
  heuristic — it mis-suppresses a DIFFERENT agent's (or human's) action landing
  inside the window, mis-delivers the agent's own action outside it, and the
  window constant is unpickable (webhook latency vs sweep latency). The router
  has real actor identity available; a timing proxy is strictly worse where
  identity exists. It survives only as the shape of surface (2)'s marker
  freshness check, where no identity signal exists (and even there the marker
  is written by the source, not inferred from timing alone).
- **Suppress at the dispatcher** (`forgeNotifyDispatcher.Notify` in go/server)
  instead of the router loop: keeps `ingest` untouched, but the dispatcher sees
  only `(accountID, notification)` — the event's actor sources
  (`ev.Comment.Agent`, the coordinate for the author lookup) are router-side,
  and the delivered-revision advance on suppression belongs beside the
  dispatch decision. Rejected; the router loop is the chokepoint that sees
  both halves.

## Global Constraints

- Identity match on OWNER-QUALIFIED HANDLES, never account ids (Matt's hard
  rule) — every actor source normalized to an (owner-handle, agent-handle) pair,
  compared pair-to-pair; a bare (unqualified) handle on either side fails open.
- Fail OPEN on surface (1): a missing/unresolvable/unqualified handle on either
  side, a store fault, or a nil resolver DELIVERS. Suppression requires a
  positive fully-qualified match. Surface (2) has no actor on its events
  (`webhook.go:19-35`) and so cannot be fail-open in this sense; its
  marker+freshness gate is single-use (§Surface (2)) to collapse its bounded
  fail-closed window to the one event the marker was minted for.
- CHECKS is NEVER suppressed — an invariant with its own test, not an emergent
  property of the matrix.
- The suppress path MUST advance the subscriber's `delivered_revision` — but
  ONLY when the subscriber was already caught up to the PRIOR cursor revision,
  enforced as a compare-and-set (`AND delivered_revision = $prior`). A trailing
  subscriber is NOT advanced (the next sweep's synthetic UPDATE is then the
  correct recovery for its genuinely-missed prior event). An advance failure or
  lost CAS degrades open, never fails the route. The advance applies to
  ARTIFACT-scope subscriptions only (§advance); a suppressed OPENED dispatch to
  a CONTAINER-scope subscriber skips the dispatch but never advances. See
  §"Suppression advances the delivery cursor" for the full argument.
- The `ingest` no-store rule stands: identity enters through the package-local
  `IdentityResolver` seam; go/server adapts the store and binds
  (provider, host).
- Team key in fixtures/tests is `RIG` (or the live `LINEAR_FORGE_TEAM` value),
  never the retired `SEA`; never reference superseded tooling/credential names
  in new code.
- Tests: no `time.Sleep`; hermetic and deterministic. Two forge test tiers
  only (DL-210): hermetic golden replay (untagged) + `//go:build livegithub`
  live oracle. Suppression is server-side logic with no new provider wire
  behavior, so this record adds NO live-tier legs.
- No planning metadata (issue ids, task labels) inline in code.
- Surface (2) implementation is Record-B-gated (RIG-3271): this record freezes
  the seam contract only; no dead gate code lands here.

## Plan

### T0 — Carry the owner-qualified actor on the wire (prerequisite)

Restore the owner on attribution so the router has an owner-qualified actor at
all, and give STATE a carrier. Without T0 the whole COMMENT/REVIEW arm is
structurally inert (§"The actor's owner is not on the wire today").

Interfaces:

```go
// proto/compass/v1/compass.proto — AgentAttribution regains the owner:
message AgentAttribution {
  string agent_handle = 1;  // the authoring agent's handle, from the header
  string owner_handle = 2;  // the owning user's handle, from the same header parse
}

// go/internal/forge/notify_event.go — ForgeEvent gains an actor for the kinds
// whose actor is NOT carried on a CommentRef (STATE today, via RIG-3331):
//   Actor *compassv1.AgentAttribution
// Set ONLY by the RIG-3331 state-transition op's emitted event; nil on every
// provider-parsed event, so STATE fails open until RIG-3331 lands.
```

Populate `owner_handle` at the three sites that already parse it and currently
discard it — `githubapp_webhook.go:280`, `linearagent/data_event.go:177`,
`ingest/notify_detect.go:328` — each from `author.OwnerHandle`
(`forge/owner.go:26,:162`). No new parse, no new store read: the value is in
hand at every one of them.

Ledger: this amends DL-094's "owner … never restated per artifact" clause as
carried forward by DL-186 (§Ledger delta) — the superseding row lands in this
record's PR beside DL-338.

**One downstream consumer T0 must also update.** The Go render callers are
genuinely unaffected (they construct/read `AgentHandle` only:
`board/issue_projection.go:197-198,:219`, `server/forge.go:614-615,:629-630`,
`ingest/ingest.go:87-88`). The TypeScript UI adapter is NOT: `adaptAgentAttribution`
(`apps/ui/src/live/adapt.ts:367-376`) hardcodes `ownerHandle: ""` and its doc
comment justifies that by citing DL-094's reservation — "the domain's
`ownerHandle`/`verified` have no wire source, so they take honest hedged
defaults". Once T0 lands there IS a wire source, so the adapter would silently
discard a real value behind a stale citation. The domain type already carries
the field (`stub-data.ts:92-96`) and two tests pin the empty default
(`adapt.test.ts:834,:916`), so this is a live contract, not dead code. T0
threads `ownerHandle: w.ownerHandle`, re-points the comment at DL-339, and
updates both assertions. (`verified` stays hedged — DL-094's
attribution-is-not-a-trust-claim core is unchanged, and DL-339 amends only the
owner clause.)

Tests: the three parse sites round-trip `owner_handle` (a stamped comment body
→ `CommentRef.Agent{AgentHandle, OwnerHandle}` both populated); a body whose
header carries no owner yields an empty `OwnerHandle` (→ unqualified →
fail-open downstream, asserted in T1); the UI adapter carries the owner through
(the two updated `adapt.test.ts` assertions); the Go attribution-render callers
are unaffected by the added field.

### T1 — `IdentityResolver` seam + suppression predicate in `ingest`

The seam interface, the per-event actor-handle resolution, the per-subscriber
match, and the skip in the fan-out loop. Actor handle computed ONCE per `Route`
(before the loop); subscriber handle resolved per subscriber, memoized per route
in a small map. The memo's real win is the OWNER lookup, not the account lookup:
`agent_forge_subscriptions` is `UNIQUE (agent_account_id, forge_provider,
forge_host, repo, kind, number, project)` (`0001_init.sql:758`) and a non-OPENED
route selects only `scope = 1 AND number = $5`
(`store/queries/forge_subscriptions.sql:51`), so one account appears at most
once and an account-keyed memo never hits (except on OPENED, where an agent can
hold both an artifact and a container row). Owner lookups DO collapse hard —
most subscribers on an artifact share an owner — so the memo is keyed on both
legs (account id → Handle, and owner id → owner handle).

Interfaces:

```go
// go/internal/ingest/notify_router.go (package ingest)
type Handle struct{ Owner, Agent string } // both non-empty to match

type IdentityResolver interface {
  HandleForAccount(ctx context.Context, accountID string) (Handle, error)
  AuthorHandle(ctx context.Context, repo string, kind compassv1internal.ForgeArtifactKind, number uint64) (Handle, error)
}

// NewNotifyRouter gains the seam; nil ids = suppression disabled (fail open).
func NewNotifyRouter(st NotifyStore, disp NotifyDispatcher, checks ChecksRoller, ids IdentityResolver, forgeRef *compassv1.ForgeRef, log *slog.Logger) *NotifyRouter

// actorHandle resolves ev's owner-qualified actor handle per the matrix; a zero
// Handle = no actor / unresolvable (fail open). CHECKS and UPDATE return the
// zero Handle unconditionally.
func (r *NotifyRouter) actorHandle(ctx context.Context, ev forge.ForgeEvent) Handle

// selfOrigin reports whether dispatch to sub must be skipped: actor and
// subscriber handles both fully qualified (all four components non-empty) and
// equal.
func (r *NotifyRouter) selfOrigin(ctx context.Context, actor Handle, sub NotifySubscriber, memo map[string]Handle) bool

// NotifySubscriber gains its subscription scope, so the suppress path can apply
// the ARTIFACT-scope-only cursor advance (§advance scope carve-out). Today the
// struct is SubscriptionID / AgentAccountID / DeliveredRevision / Project
// (notify_router.go:27-32). NOTE the scope is NOT available to project today:
// SubscribersForArtifact FILTERS on scope but does not SELECT it
// (`queries/forge_subscriptions.sql:47` projects id/agent_account_id/
// delivered_revision/project only; scope appears just in the WHERE at :51-52),
// and neither the generated row (`db/forge_subscriptions.sql.go:317-322`) nor
// the store domain type `ForgeNotifySubscriber`
// (`store/forge_subscriptions.go:229-233`) carries it. So T1 ALSO does the
// store-side widening. `ForgeNotifySubscriber` has TWO producers, and the
// SELECT-vs-WHERE gap above is present in BOTH, so BOTH are in T1's scope:
//   (a) `SubscribersForArtifact` (the Route path) — add `scope` to its SELECT
//       (`queries/forge_subscriptions.sql:47`), regen sqlc, populate the mapper
//       (`forge_subscriptions.go:302-307`);
//   (b) `ListForgeNotifyTargets` (the reconcile-sweep path) — add `s.scope` to
//       its SELECT (`queries/forge_subscriptions.sql:60-64`, where scope today
//       drives only the container-collapse `CASE` — projected at `:61`,
//       repeated in the join at `:71` — and is never selected as a column),
//       regen sqlc (`db/forge_subscriptions.sql.go:198-213`), populate its
//       mapper (`forge_subscriptions.go:371-376`);
// plus `Scope ForgeSubscriptionScope` on the struct
// (`store/forge_subscriptions.go:229-233`), and a copy across the go/server
// adapter hop that bridges the store type to this ingest mirror:
// `toIngestSubscribers` (`serve.go:1280-1294`) is a hand-written field-by-field
// copy of exactly SubscriptionID/AgentAccountID/DeliveredRevision/Project —
// one function, the SOLE feeder of both lanes (`serve.go:1236` Route,
// `:1253` sweep), so widening it once covers both.
// An unnamed field at ANY of those hops arrives as
// `ForgeSubscriptionScopeUnspecified` (0), never `…Artifact` (1)
// (`store/forge_subscriptions.go:37-39`), which would
// silently disable T2's artifact-scope gate in production while the hermetic
// ingest tests — which build `NotifySubscriber` literals directly — still pass.
// (The store normalizes 0→ARTIFACT on WRITE, `forge_subscriptions.go:71-74`, so
// no persisted row is scope-0; a zero is injected purely by a missing copy.)
// The INVARIANT T1 owes, stated once so it outlives this enumeration: every
// `ForgeNotifySubscriber` and every `NotifySubscriber` carries its real scope,
// on BOTH the Route and sweep lanes — the two mappers must not drift, the same
// reasoning `toIngestCursor` already documents for cursors
// (`serve.go:1297-1299`). Note the sweep's current consumer
// (`notify_reconcile.go:201-206`) does not read Scope, so (b) breaks nothing on
// landing; it is required so the frozen invariant is not false for every
// sweep-produced subscriber. None of this is an assumed freebie.
type NotifySubscriber struct { /* … existing fields … */ Scope ForgeSubscriptionScope }
```

All existing `NewNotifyRouter` call sites updated in the same change (clean
cutover, nil for tests that don't exercise suppression). The binding invariant
is **every `NewNotifyRouter` caller in `go/internal/ingest` and `go/server`,
tests included** — the line numbers below go stale against a moving tree, so
re-grep at execution:
`go/server/serve.go:1406,1453`, `forge_notify_matrix_test.go:395,436,454,469,503`,
`forge_notify_pgtest_test.go:183,286,390`, `forge_notify_e2e_pgtest_test.go:156,166`,
and the three IN-PACKAGE `ingest` sites the go/server sweep misses:
`go/internal/ingest/notify_router_test.go:122`,
`go/internal/ingest/notify_reconcile_test.go:93,414`.

Tests (hermetic, `ingest` unit tier + the matrix-test style with a scripted
resolver): self-comment suppressed; human comment (Agent unset) delivered;
cross-agent comment delivered; **same bare agent handle under DIFFERENT owners
delivered (the owner-namespace-collision test — `atlas@A` acts, `atlas@B`
subscribes, must NOT suppress)**; OPENED to author's container sub suppressed,
OPENED to another agent's sub delivered; **CHECKS with a subscribed author
DELIVERED (the invariant test)**; UPDATE delivered; resolver clean-miss,
resolver-fault, and unqualified-handle (empty owner) all delivered (fail open);
nil resolver delivers everything.

### T2 — Delivery-cursor advance on suppression

Interfaces:

```go
// go/internal/ingest/notify_router.go — NotifyStore seam gains a compare-and-set
// advance (arg order matches the store method it adapts: agent first):
//   AdvanceDeliveredRevisionCAS(ctx context.Context, agentAccountID, subscriptionID, prior, next string) (advanced bool, err error)
// go/server adapter backs it with a NEW sqlc query + *Store wrapper:
//   func (s *Store) AdvanceForgeDeliveredRevisionCAS(ctx context.Context, agent AccountID, subscriptionID, prior, next string) (bool, error)
// backed by a new query AdvanceForgeDeliveredRevisionCAS adding `AND delivered_revision = $prior`
// to the existing UPDATE (store/queries/forge_subscriptions.sql:92-95), as :execrows;
// the wrapper returns affected-rows>0 as `advanced` (NOT folding zero rows into ErrNotFound),
// so a lost CAS (advanced=false, err=nil) is distinguishable from a real store fault (err!=nil).
// The landed non-CAS AdvanceForgeDeliveredRevision (store/forge_subscriptions.go:467, ack-arm
// only) is left untouched.
```

The suppress path in the loop calls the CAS with `prior = sub.DeliveredRevision`
(from step 5, `notify_router.go:225`; field at `:30`) and `next` = the route's
computed `revision`, **and ONLY for an ARTIFACT-scope subscription** (§advance
covers why a CONTAINER-scope sub is skipped). The CAS predicate
(`AND delivered_revision = $prior`) means a concurrent route cannot erase a gap
it did not see; `advanced=false` (lost CAS) or any error → warn log, continue
(fail-open degradation — at worst one synthetic UPDATE on the next sweep, never a
route failure). Doc comments on `NotifyRouter` and `NotifyStore` amend the W3
note ("never advances delivered_revision") to name this one suppression-only,
caught-up-guarded exception.

Tests (hermetic + the pgtest tier that already exercises the store-backed
seam): a suppressed dispatch of a CAUGHT-UP artifact-scope subscriber advances
`delivered_revision` to the event revision, and the next reconcile sweep
synthesizes NO update for it; **a suppressed dispatch of a TRAILING subscriber
(an undelivered prior event) does NOT advance, and the sweep DOES synthesize the
recovery UPDATE (the forward-masking-guard test)**; **a suppressed OPENED
dispatch to a CONTAINER-scope subscriber skips the dispatch and does NOT touch
its cursor row (the scope carve-out test — asserting the container cursor is
unchanged, so the container sweep is not poisoned with an artifact revision)**;
an advance/CAS failure still routes cleanly.

### T3 — go/server `IdentityResolver` adapter + lane wiring + e2e

Interfaces:

```go
// go/server (beside forgeNotifyStore / forgeNotifyDispatcher):
type forgeIdentityResolver struct {
  st       *store.Store
  provider store.ForgeProvider
  host     string
}
func (r *forgeIdentityResolver) HandleForAccount(ctx context.Context, accountID string) (ingest.Handle, error)
func (r *forgeIdentityResolver) AuthorHandle(ctx context.Context, repo string, kind compassv1internal.ForgeArtifactKind, number uint64) (ingest.Handle, error)
```

`HandleForAccount` = `GetAccount` (`accounts.go:458`) → `(ingest.Handle{Owner,
Agent}, nil)`, `ErrNotFound` → zero `Handle`. `AuthorHandle` =
`AuthoredArtifactByCoordinate` (`forge_authored.go:144`) then `GetAccount(...)`
for the recorded author's owner+agent handle, `ErrNotFound` at either step →
zero `Handle`, kind mapped `compassv1internal.ForgeArtifactKind` →
`store.ForgeArtifactKind` the same way the lane's `forgeNotifyStore` adapter
already maps it. Wired into BOTH lanes — `buildForgeNotifyLane`
(`serve.go:1406`) and its sibling `buildLinearNotifyLane` (`serve.go:1453`).

Tests: extend the e2e pgtest (`forge_notify_e2e_pgtest_test.go`) with the real
store-backed resolver — a seeded agent authors an issue (DL-055 row), holds a
subscription, a self-comment event routes → no `DeliverControl` reaches its
session AND (if it was caught up) its `delivered_revision` advanced; a second
agent's subscription on the same artifact receives the notification; a CHECKS
event on the same artifact reaches the author; and the owner-namespace pair
(two owners' `atlas` agents) does NOT cross-suppress.

### T4 — Surface (2) seam contract (Record-B-gated; no code in this record)

Freeze the contract Record B implements if its probe shows an app-set
`delegateId` fires `created`:

- `forge_authored_artifacts` gains `self_delegated_at_unix_ms BIGINT NULL`,
  written by the create arm's `record` step (`forge.go:347-360`) iff the
  create carried `DelegateSelf: true` (Record A's field).
- The Linear webhook handler's `created` arm checks the coordinate's row
  before `Dispatcher.Enqueue`: marker present and within the freshness bound →
  CONSUME the marker and drop (counted, logged), no session, no Manager prompt.
  The consume and the drop are one transaction, so the marker fires at most
  once per self-delegate write (§Surface (2) — this is what bounds the
  fail-closed window to a single event).

Interfaces (contract only, exact shape for Record B):

```go
// go/internal/linearagent — the gate seam, backed by the ownership index:
type SelfOriginGate interface {
  // ConsumeSelfDelegated atomically reports whether the coordinate's ownership
  // row carries a fresh app-self-delegation marker AND clears it in the same
  // statement (an UPDATE … SET self_delegated_at_unix_ms = NULL WHERE … AND
  // self_delegated_at_unix_ms > $fresh, :execrows). (false, nil) on a clean
  // miss or a stale marker. Single-use by construction: a second event on the
  // same coordinate sees no marker and is delivered.
  ConsumeSelfDelegated(ctx context.Context, provider store.ForgeProvider, host, repo string, kind store.ForgeArtifactKind, number uint64, now time.Time) (bool, error)
}
```

Deliverable in THIS record's PR: this frozen contract in the record itself —
no production code, no migration (dead code guarding an unobservable path is
worse than a documented contract).

### T5 — Ledger append

Append BOTH rows (text under "Ledger delta" above) to
`docs/designs/DECISIONS.md` in this record's freeze PR — **DL-338** (the
suppression decision) and **DL-339** (the `owner_handle` carriage amendment T0
implements) — confirming both are still next-free global ids across every OPEN
design PR at freeze time, not just main. No existing row is mutated; DL-186
stays Active.

## Tasks

- [ ] T0: `owner_handle` restored on `AgentAttribution` + populated at the three
      Go parse sites + the UI adapter (`adapt.ts` + its two test assertions) +
      `ForgeEvent.Actor` carrier added; round-trip tests
      (prerequisite — without it COMMENT/REVIEW is structurally inert)
- [ ] T1: `IdentityResolver` seam + `actorHandle`/`selfOrigin` + loop skip +
      `NotifySubscriber.Scope` (INCLUDING the store-side widening on BOTH
      producers: `scope` into the `SubscribersForArtifact` SELECT AND the
      `ListForgeNotifyTargets` SELECT + sqlc regen + `ForgeNotifySubscriber`
      field + BOTH mappers (`forge_subscriptions.go:302-307`, `:371-376`) + the
      `toIngestSubscribers` adapter hop,
      `serve.go:1280-1294`) + every `NewNotifyRouter` caller in
      `go/internal/ingest` AND `go/server` (tests included) + hermetic matrix
      tests (incl. the CHECKS invariant test, the owner-namespace-collision
      test, and all fail-open tests)
- [ ] T2: `NotifyStore.AdvanceDeliveredRevisionCAS` + new sqlc CAS query +
      `*Store` wrapper returning `advanced bool` + nil-cursor-guarded
      caught-up test + artifact-scope-only advance + W3 doc amendment + pgtests
      (caught-up advances / trailing does not / container-scope untouched)
- [ ] T3: go/server `forgeIdentityResolver` adapter (two-read owner
      resolution) + both-lane wiring + e2e pgtest (self suppressed / cross
      delivered / CHECKS delivered / cursor advanced / owner-namespace pair
      does not cross-suppress)
- [ ] T4: surface (2) `SelfOriginGate` consume-on-match contract frozen in this
      record (Record-B-gated; no code)
- [ ] T5: DL-338 + the DL-094/DL-186 owner-clause amendment rows appended in
      the freeze PR (confirm next-free ids vs every open design PR at freeze)

## Resolved decisions

- **STATE author-proxy (was OQ-1) — RULED (Matt).** No forge write op
  transitions state today (`go/server/forge.go` arms: create / comment / review
  / get / list / subscribe), so every current STATE event is human/external
  actored, and an author-row proxy on STATE would suppress the authoring agent's
  notification of a HUMAN closing its issue — the exact cross-actor signal the
  fail-open rule protects. Matt's ruling: **state transitions are required —
  agents MUST be able to update issue state on GitHub and Linear, and the
  missing write op is added immediately.** So STATE is NOT narrowed out of the
  suppressed set; instead the missing agent-driven state-transition write op is
  built as a prerequisite (**RIG-3331**, filed under Compass, Owner:
  compass-forge), which stamps the real acting agent on the transition. STATE
  suppression then matches on that REAL transition actor, not the DL-055
  author-row proxy. Until RIG-3331 lands there is no Compass actor on a STATE
  event, so STATE has no positive match and delivers (the safe interim, and the
  T1 STATE arm ships resolving the transition actor with the fail-open miss
  behaviour built in). This is a **dispatch-ordering dependency, not a freeze
  blocker**: the record is a valid frozen contract now — the STATE arm is
  interim-safe (fail-open) the moment T1 lands and becomes active-suppressing
  once RIG-3331's op stamps the real actor. RIG-3331 sequences before T1's STATE
  activation, and both land before this suppression is relied on for STATE.

## Open Questions

- **OQ-2 (surface (2) firing) — non-blocking.** Whether an app-set `delegateId`
  fires a `created` AgentSessionEvent is unobservable until Record B's ingress
  lands (RIG-3271-gated; Record A design.md:127-135). T4's contract is frozen
  here; Record B implements or discards it based on the probe. Not load-bearing
  for this record — nothing here blocks on the answer, so it does not gate the
  freeze.
