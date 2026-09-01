# Compass Linear return path — amendment: server-resolved indirection link

Status: Active
Tracker: RIG-2794

> **Extends #625 (frozen).** This record is a sibling amendment to
> `docs/designs/server/compass-linear-agent-responder/design.md` (merged in
> #625). The merged record is frozen; per house convention a later change ADDS
> a record. This amendment changes ONE thing in that record's Part 3 return
> path: WHERE the session's `externalUrls` link points — a stable
> server-resolved indirection URL instead of a direct home-channel deep link.
> Every other DL-256 invariant survives unchanged.

## Problem / Intent

The frozen record's Part 3 sets the session external URL to a DIRECT deep link
resolved at dispatch time:

> "**A session external URL** (`agentSessionUpdate` with an `externalUrls`
> entry, linear.app/developers/agent-interaction §Session external URL) — the
> 'Open in Compass' deep link to the resolved Manager's home channel (Part 2 /
> OQ-4)." — #625 design.md:356-360

But the target that link should carry is not stable at `created` time. Part 2
routes an issue with no recorded ownership row "to the **supervisor /
top-level Manager** via a **dedicated routing channel**, which decides the
owning lane and stamps accordingly" (#625 design.md:230-237, DL-255,
DECISIONS.md:361) — so at `created` the best target may be the routing
channel, and only later (once the supervisor stamps a lane and a DL-055
ownership row exists) the resolved Manager's home channel. And there is no
later event to hang a correction on: under the dumb-link model, Linear
prompting Compass through the session is effectively a no-op surface (the
conversation lives in Compass, #625 design.md:362-365), so no follow-up
`prompted` activity reliably ever fires for a session the human drives from
Compass. A link resolved eagerly at `created` can therefore be wrong — pointing
a human at the routing channel long after the issue was routed — with no event
to correct it.

Matt ruled the fix (RIG-2794): a **server-resolved indirection link**. The
stored Linear URL never changes; the server resolves the current best target
at click time.

## Approach

Two halves: a stage-1 emit that gets simpler, and a new read-only redirect
route that carries all the routing smarts.

### Stage 1 — set the external URL once, to a stable indirection URL

On `created`, the dispatcher sets the session external URL ONCE, immediately,
to:

```text
https://<public-base>/l/session/<linear-session-id>
```

- `<public-base>` is the existing per-deployment public base URL — DL-254's
  "the public base URL (webhook host + deep-link base) is a per-deployment
  config value, never hardcoded" (DECISIONS.md:360); "the deep-link builder
  (Part 3) and the ops runbook's Linear webhook-registration step both read
  this one value" (#625 design.md:461-462).
- `<linear-session-id>` is the Linear `AgentSession.id` — already the primary
  key of the `linear_agent_sessions` association table (#625 design.md:279-286:
  `linear_session_id TEXT PRIMARY KEY, -- Linear AgentSession.id`).

The emit mechanism is the one already landed on `main`:
`go/internal/linearagent/client.go` — `ExternalURL{Label, URL}` (client.go:48-51),
the `agentSessionUpdateMutation` ("attaches external URLs (the deep link) to a
session", client.go:228-231), and
`UpdateSession(ctx, sessionID, externalURLs)` (client.go:244-251). Nothing in
that client changes; only the URL string the dispatcher hands it.

Properties this buys:

- **Post-independent.** The indirection URL is a pure function of the session
  id — it does not wait on routing, ownership resolution, topic creation, or
  the prompt post. It comfortably fits the 10-second liveness SLA the frozen
  record binds the external-URL emit to (#625 design.md:337-344: the
  dispatcher "sets the session external URL directly from the webhook dispatch
  path, before any agent turn begins").
- **Never rewritten — and now never stale.** The stored URL NEVER changes
  after the one emit; DL-256's one-emit dumb-link invariant is preserved
  unchanged (exactly one `agentSessionUpdate`, ever, per session). What the
  indirection *adds* is staleness-immunity: DL-256 already forbade rewriting
  the link, so a direct home-channel link that resolved wrong at `created`
  would stay wrong forever with no correction event (the failure this
  amendment removes). The indirection link is equally immutable but resolves
  to the correct target at click time, so immutability no longer implies
  staleness.

### The redirect resolver — `GET /l/session/<linear-session-id>`

A new server route on the same network TLS door as the DL-254 webhook
receiver (the `POST /webhooks` handler "mounted on the compass-server network
TLS door (inside `buildNetworkServer`, beside the Connect mounts …)",
DECISIONS.md:360). At CLICK time it:

1. Reads the `linear_agent_sessions` association row for the session id
   (#625 design.md:279-286) — **only to recover the session's
   `linear_issue_id`** (the issue's forge coordinate) for the ownership
   lookup. The row's stored `channel_id` / `manager_account_id` are the
   **created-time** target #646 wrote, and are deliberately **NOT** used as
   the redirect target — that stale value (e.g. the routing channel an issue
   was unrouted to at `created`) is exactly what this indirection exists to
   bypass.
2. Resolves the current best target from that coordinate by running the same
   click-time resolution the dispatcher runs at `created` — #625's
   `ResolveResponder` (T4) walk keyed on the DL-055 recorded ownership index
   ("routed to a stable Compass Manager keyed on Compass's recorded forge
   ownership index (`forge_authored_artifacts`, DL-055/DL-205) — NEVER a
   header parsed from forge text", DECISIONS.md:361): from the recorded
   authoring agent, walk the owner relation up to the owning Manager, then
   that Manager's home channel (#625 design.md:218-229).
3. Issues a **302 redirect** to the walk's current output:
   - the resolved **Manager's home channel** once a DL-055 ownership row
     exists for the coordinate;
   - the **dedicated routing channel** while the issue is still unrouted (no
     ownership row yet — the DL-255 supervisor-fallback state, #625
     design.md:230-237);
   - the **dedicated routing channel** also for a **bare `@mention` session
     with no issue** — `linear_issue_id` is nullable (#625 design.md:284) and
     #625 supports a session with no issue (the topic "[falls] back to the
     session id for a bare @mention with no issue", #625 design.md:262-266).
     With no forge coordinate no ownership row can ever resolve, so the
     routing channel is the only possible target, matching #646's
     `ResolveResponder` bare-@mention fallback.

All the routing smarts are a READ-ONLY server redirect computed at click time.
Nothing ever writes back to the Linear session; nothing observes agent
lifecycle; nothing relays activity. An unknown session id (no association row)
is a plain 404. Security posture, stated rather than asserted away: the
404-vs-302 split is an existence oracle over Linear `AgentSession` ids, and a
302 discloses a target channel id to any caller holding a session id. This is
acceptable — session ids are non-secret-grade opaque identifiers, and the
redirect target is itself an auth-gated Compass surface (the channel id alone
grants nothing without a Compass session) — so the route leaks no
authorization and mutates nothing. If id-enumeration is later judged a
concern, the route can 302 unknown ids to a generic landing rather than 404.

### What survives of DL-256 (everything except the link target)

DL-256 (DECISIONS.md:362) is AMENDED, not superseded. Its invariants all
survive verbatim:

- **Dumb link** — one `thought` + one `externalUrls` entry on `created` "and
  nothing else"; still exactly that, and the link is now immutable by
  construction.
- **NO activity relay** — unchanged; the resolver emits no Linear activity.
- **NO settle observation** — unchanged; the resolver only reads store state
  at click time (the association row + the ownership-index walk), it observes
  no turn edges or agent lifecycle.
- **NO Linear session-lifecycle machine** — unchanged; there is still exactly
  one external-URL update per session, ever.
- **NOT-1-1 topic mapping** — unchanged; the redirect targets a CHANNEL (the
  routing channel or the home channel), never a topic, exactly the frozen
  OQ-4 shape ("the deep link points at the Manager's home channel … not at
  the specific topic", #625 design.md:270-273).

The ONLY delta: the frozen "deep link to the resolved Manager's home channel"
clause of DL-256 becomes "stable indirection URL whose server-side redirect
resolves to the routing channel or the Manager's home channel at click time."

### Sequencing — #646 ships as-is; the resolver is the follow-on

The currently landed one-stage behavior — a direct home-channel deep link set
from the dispatch path — ships as-is in PR #646 and is NOT blocked by this
amendment. Per the #646 PR body: the dispatcher's `created` leg runs "resolve
-> ensure @linear membership -> get-or-create topic -> upsert association ->
ack thought + external-URL deep link (the 10s SLA leg, before the post)",
building the link through an injected `DeepLinkFor` seam ("Injected seams only
(CommsPoster, Memberships, Topics, Associations, Client, DeepLinkFor) so the
package never imports go/server"). The `linear_agent_sessions` association
table this amendment's resolver reads lands in that same stack (#625 T3,
design.md:650-655: migration `000N_linear_agent_sessions.sql`) — it is not yet
on `main`.

The indirection resolver is the follow-on this amendment specs:

1. **#646 merges unchanged** — direct link via `DeepLinkFor`.
2. **The follow-on** adds the `GET /l/session/<id>` route beside the DL-254
   webhook mount and swaps the `DeepLinkFor` implementation wired at assembly
   to emit the indirection URL. The seam is already the right shape — the
   dispatcher takes the URL as an injected function, so the swap touches
   assembly wiring and adds the route + resolver, not the dispatcher.

## Alternatives considered

Both alternatives were considered and killed in Matt's ruling; recorded here
so the fork stays closed.

- **(a) Update the link on the next event** — keep the eager `created`-time
  link and rewrite it via a second `agentSessionUpdate` when a later webhook
  (e.g. `prompted`) shows the issue was routed. REJECTED: under the dumb-link
  model the human drives the exchange from Compass, so Linear prompting is a
  no-op surface and no follow-up `prompted` event reliably ever fires to
  trigger the update (#625 design.md:362-365: "The agent's reply … and the
  rest of the conversation live in Compass"). The correction event never
  comes, and the wrong link persists. It would also break the one-emit
  invariant, making the stored URL mutable.
- **(b) Pick the final target eagerly at `created`** — resolve the Manager's
  home channel at dispatch time and link it directly. REJECTED: at `created`
  the issue may still be unrouted — DL-255's fallback path is precisely "an
  issue with no recorded ownership row routes to the supervisor/top-level
  Manager via a dedicated routing channel" (DECISIONS.md:361), and the #646
  `ResolveResponder` implements exactly that ("unstamped issue or bare
  @mention falls back to the supervisor + dedicated routing channel", #646 PR
  body). There is no ownership row to resolve against, so the eager link can
  only point at the routing channel, and per (a) no event ever corrects it
  once the lane is stamped.

Option (c) — the server-resolved indirection link — is the ruled design this
record captures.

## Ledger-impact

Ledger-impact: adds DL-268 to `docs/designs/DECISIONS.md` under the "Linear
agent responder" section. (Originally authored as DL-264 against a `main`
topping out at DL-263; a concurrent forge-notification record, RIG-2732, also
claimed DL-264 and merged first, taking the contiguous DL-264..DL-267 block, so
this row was renumbered to the next free id DL-268 to clear the duplicate.)
DL-256 does NOT
flip: its status cell stays `Active` and its frozen Decision-cell prose is
untouched (rows are append-only, `tools/design-ledger-gate/index.ts:25-27`) —
this amendment is its amending sibling, and the new row carries the delta,
mirroring how the ownership-layer amendment left every #995 row standing
(`compass-server-ownership-layer-amendment/design.md` §Ledger delta: "No
existing #995 row flips"). Record links are relative to
`docs/designs/DECISIONS.md`, so they carry the `product/` prefix
(`tools/design-ledger-gate/index.ts:52-60` includes `product` in
`GOVERNED_ROOTS`).

Spec-impact: none. This is a docs-only design record; it changes no
`compass.v1` contract and adds no requirement to `docs/specs/`.
