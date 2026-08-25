# Compass Linear Agent Session responder

Status: Draft

Tracking: RIG-2717 (greenlit by Matt 2026-08-25; scoped to production, not a
throwaway spike — Matt 2026-08-25: "Prod. Not really a spike now, we've figured
out the model, we need to be a Linear Agent App to make sense to users").
Predecessor: RIG-2682 installed the Linear `actor=app` agent "Compass"
(viewer.id `b7511616-41fe-4583-a52a-befdb4c3090c`, non-billable app user) onto
the Rigel workspace. Decision issue: RIG-2729 (all ten open questions ruled by
Matt 2026-08-25; the rulings are folded into this record and recorded under
§Resolved decisions).

## Problem / Intent

The Compass app is installed in Linear and appears in the delegate + @mention
menus, but nothing consumes its webhooks: delegating an issue to it does
nothing, and Linear auto-disabled the "Agent session events" category after
repeated 404s at the registered webhook URL. This record designs the production
bridge that makes the Compass app a live Linear Agent App: receive Linear Agent
Session webhooks, **route** a delegated issue to the right stable Compass
Manager, carry the conversation as a comms topic in that Manager's home channel,
and **link the Linear session back out to Compass** so the human drives the
exchange on the surface that actually works.

**The premise (do not get this backwards).** Compass's communication model is a
deliberate design, not an ad-hoc one: stable agents (Managers + peers) each with
a home channel, all *communication* carried on **Zulip-style threads** (channel
→ named topics), and the agent's *work* — streamed reasoning, tool calls, code —
kept OUT of the threads in a **side-panel session log**. The split is structural
and load-bearing: it keeps conversations scoped and stops them drowning between
long stretches of tool calls. This is captured as a first-class premise in
[the comms model concept](../../../concepts/comms-model.md) (ledger DL-098 Zulip
threading, DL-099 streamed-turn-writes-nothing-to-comms, DL-158 workspace = home
channel + session trace).

**Linear's Agent Session is the opposite shape**, and it is exactly what Compass
rejects wholesale: a single ephemeral session spun up per issue, no stable agent
behind it, no threads — everything (work and conversation alike) dumped into one
flat activity log. So the design is **not** "does Linear's session model carry
our workflow" (it does not, by construction). We are **not** adopting the Agent
Session model. The Linear session is a doorway, not a home: a delegation is
**routed to the right stable Compass Manager**, its conversation lives as a
**topic in that Manager's home channel**, and the Linear session **links out to
Compass** rather than trying to mirror a conversation it cannot represent. One
hard limit forces that last choice: Linear's Agent **Activity vocabulary is a
fixed, server-validated set of five types** (`thought`/`action`/`elicitation`/
`response`/`error`) with no extension point, so Compass's typed comms blocks
(the structured `ask` with discrete options, and planned kinds) **cannot**
round-trip through a Linear session — an `ask` would degrade to a plain-markdown
`elicitation`, losing its options.

## Approach

### The chosen shape: Option B — dumb link (Matt ruled 2026-08-25, RIG-2729)

The mapping-depth fork (how much of the interaction lives inside the Linear
session versus defers into Compass) was the record's central question. **Matt
ruled Option B — dumb link — with Option C as the ratified follow-up and Option
A off-table** until Linear makes its activity format configurable (Matt: "B,
with follow up for C. A doesn't make sense until Linear make the format more
configurable, which they might do eventually"). The three options as weighed:

- **Option A — full relay (bidirectional).** The Linear session mirrors the
  Compass conversation: prompts flow in, the agent's settled messages flow back
  out as Linear activities. Richest in-Linear UX, but bounded by Linear's fixed
  5-type activity vocabulary (a Compass `ask` degrades to a flat `elicitation`,
  losing its options), and it pulls in a settle-observability dependency and a
  session-lifecycle state machine. **Off-table** until Linear's activity format
  is configurable.
- **Option B — dumb link (chosen).** On `created`, the session emits one
  `thought` plus an `externalUrls` "Open in Compass" deep link (natively
  supported — linear.app/developers/agent-interaction §Session external URL, and
  it also prevents the session being marked unresponsive) into the mapped
  Manager's home channel, and does nothing else on the return path. All real
  interaction — including the typed `ask` blocks that cannot survive a Linear
  round-trip — happens in Compass's own comms surface. Simplest, and the cleaner
  UX: it never shows a degraded half-conversation in Linear.
- **Option C — hybrid (ratified follow-up).** Dumb link PLUS coarse one-way
  status back (e.g. a `response` when the turn settles or the issue's PR opens),
  no message-level mirroring. The natural next step once at-a-glance Linear
  status is wanted; a thin later addition on top of B.

**Everything below is Option B.** The return path is a deep link, not a relay:
there is no activity-relay consumer, no settle-edge observation, no
message-level mirroring. Option C adds a coarse status emitter on top of this
same foundation without changing the receive/route/map path; Option A is a
larger change deferred until Linear's vocabulary supports it.

### The loop in one paragraph

A plain `POST /webhooks` HTTP handler mounted on the compass-server **network
TLS door** (the one internet-facing surface) verifies the Linear HMAC-SHA256
signature over the raw body, acks 200 immediately, and hands the event to an
async dispatcher. On `created` the dispatcher **resolves the delegated issue to
a stable Compass Manager** (routing, Part 2 — keyed on Compass's own recorded
ownership truth, never a parsed forge header), gets-or-creates a comms topic for
the issue in that Manager's home channel, persists the association in a new
store table, emits **one** acknowledging `thought` activity plus an
`externalUrls` deep link to the Manager's home channel within Linear's 10-second
SLA, and posts the session's `promptContext` as a message into the mapped topic
— riding the existing durable deliver rail that starts a turn on an idle agent.
On `prompted` it looks the association up and posts the follow-up message into
the same conversation. **There is no return relay:** the agent's reply, the
typed asks, the whole exchange happen in Compass, and the Linear session's job
is done once it has pointed the human there. Auth for the two Linear-side emits
(the `thought` and the session-URL update) is a `client_credentials` app token
the responder re-mints on 401. No new proto, no new Connect service, no
agent-side changes: the responder is a server-side adapter between Linear's
webhook protocol and the routing + comms + delivery rails Compass already has.

### Part 1 — the webhook receiver on the network door

The network door is the only internet-facing surface and already carries the
hardening a public receiver needs. `buildNetworkServer`
(`go/server/network_door.go:233-326`) assembles `netMux :=
http.NewServeMux()` (`network_door.go:270`) and mounts the Connect handlers on
it (`netMux.Handle(netPath, netHandler)`, `network_door.go:271`); the whole mux
is wrapped by the slow-body guard — "Outermost: bound the request-body read so
a slow-body drip cannot tie up a connection (SEA-1298)"
(`network_door.go:306-310`) — and the server sets `ReadHeaderTimeout: 10 *
time.Second` with the comment "G112: the network door is the internet-facing
surface" (`network_door.go:315-323`).

The receiver is a **plain `netMux.Handle("/webhooks", …)` alongside the
Connect paths, NOT a Connect service**: Linear speaks bare HTTP POST + JSON,
carries its own authentication (the signature), and must bypass the bearer
interceptors (Linear cannot present a Compass bearer token). Mounting it on
`netMux` means it inherits `withBodyReadDeadline` and `ReadHeaderTimeout` for
free. `buildNetworkServer` grows one construction call for the handler; no
other door mounts it (the UDS/dev doors are loopback,
`go/server/serve.go:595-651`).

**Signature verification, fail-closed.** Linear "sends a `Linear-Signature`
HTTP header with every webhook request. This header contains a hex-encoded
HMAC-SHA256 signature of the raw body contents, signed using the webhook's
signing secret" (linear.app/developers/webhooks §Securing webhooks). The
handler reads the full raw body (bounded — see Global Constraints), computes
`HMAC-SHA256(signingSecret, rawBody)`, and compares with
`hmac.Equal` (constant-time; the doc's own example uses
`crypto.timingSafeEqual`). Mismatch or missing header → 400, body never
parsed — a deliberate choice: Linear's auto-disable heuristic keys on
failures generally (400 vs 401 makes no difference to it), and a request
that fails authentication must never be acked. After signature passes, the
parsed body's `webhookTimestamp` (UNIX millis) is checked to be within 60s
of server time — the doc's recommended replay guard ("verify it's within a
minute of the time your system sees it to guard against replay attacks").
A STALE timestamp returns **200-with-drop** (ack, discard, log), NOT 400:
the timestamp lives inside the SIGNED body, so if Linear replays the same
signed body on a >60s-later retry, a 400 would burn the whole retry ladder,
lose the delivery, and feed the failure streak that risks auto-disable —
acking keeps the replay defense intact (the stale event is never processed)
without punishing a legitimate retry. Whether stale-drop ever triggers
depends on whether Linear re-signs retries with a fresh timestamp — an
explicit ops-verification item at rollout (T8 gate). The doc's list of
Linear source IPs is noted but NOT enforced (it "may occasionally update";
the signature is the authentication).

**Ack fast, work async.** Linear requires a 200 "within 5 seconds (5000ms)"
or it retries and eventually disables the webhook
(linear.app/developers/webhooks §How does a Webhook work). So the handler does
only: verify signature → decode the envelope (`type`, `action`) → enqueue the
event onto the responder's dispatch goroutine → 200. All Linear API calls,
store writes, and agent work happen after the ack. A full queue returns 500
(Linear retries with backoff — at-least-once is our friend here; dedup below).

**Dedup.** Linear retries failed deliveries up to three times (≈1 min /
1 hr / 6 hrs later, with no ordering guarantee), so processing is
at-least-once — and the dedup rides the comms rail's OWN idempotency, not
new schema. Every `PostAsAccount` call the dispatcher makes carries
`client_request_id = "linear-delivery:" + <Linear-Delivery UUID>` (the
header: "A UUID (v4) that uniquely identifies this payload",
linear.app/developers/webhooks §Webhook Payload). PostAsAccount "delegates
to the same PostMessage handler path a human caller takes, so authz (D9),
idempotency (client_request_id), and MessagePosted fan-out are identical"
(`go/internal/comms/agent_caller.go:124-131`), and the store collapses a
replay onto the stored row — `ON CONFLICT (author_account_id,
client_request_id) … DO NOTHING` under the partial unique index
(`go/internal/store/messages.go:141-146`,
`go/internal/store/migrations/0001_init.sql:264-271`) — the exact mechanism
`postSetupThread` uses to dedup its re-fired Setup post
(`ClientRequestId: setupThreadClientRequestIDPrefix + …`,
`go/server/serve_seed.go:171-176`). A replayed delivery therefore re-posts
nothing and re-publishes no MessagePosted; the association insert is
independently idempotent (ON CONFLICT (linear_session_id) DO NOTHING). No
delivery ring, no dedup column, no new store method. **One residual caveat:**
the `created` acknowledging `thought` (Part 3) goes out via
`agentActivityCreate`, which carries no `client_request_id`, so a replayed
`created` delivery would re-emit a duplicate ack `thought` into the Linear
session. This is benign (a duplicate "starting…" thought is cosmetic) and
its own single emit under Option B — there is no relay to multiply it — and
whether it ever fires depends on Linear's retry re-sign behavior, an ops
observation item at rollout (T8 gate). If it proves annoying, the ack
`thought` can be made conditional on the association insert reporting
`created == true` (only the first, non-replayed delivery emits it).

### Part 2 — routing a delegated Linear session to a stable Manager

**The routing problem (Matt ruled routing IS in scope, RIG-2729 OQ-3).** A
Linear delegation or @mention names *the Compass app*, not a specific Manager.
The bridge must resolve which stable Manager runs it. Matt: "We need to do the
routing in any case so let's do that. We already have the whole forge stamping
thing on issues/prs/comments etc — can likely tie in with that. If not already
stamped, can just go to the supervisor/top level to route and stamp
accordingly, in a dedicated routing channel probably."

**The trusted routing source is Compass's own recorded ownership truth, never a
parsed forge header.** This is a hard constraint, not a preference: DL-050 /
DL-094 make an owner header *parsed from forge text* untrusted display metadata
that "must never reach any authz / routing / ownership decision"
(`go/internal/forge/owner.go:11-14`, `:136-140`). The forge-stamping mechanism
Matt points at has two halves — the unforgeable **write-time stamp**
(`forge.StampOwner`, DL-050) and the server-recorded **ownership index**
`forge_authored_artifacts` (DL-055 / DL-205: "the DL-055 ownership index
materializes as `forge_authored_artifacts` (PK = forge coordinate; agent/owner/
session columns), written by `forgeService` strictly after forge write
success"). Routing keys on the **index** (Compass's own recorded truth), not on
re-reading a header off the delegated issue's body:

- **If the delegated issue is already Compass-authored/owned** — a row exists in
  `forge_authored_artifacts` for the issue's forge coordinate — resolve to the
  stable **Manager** for that recorded work. The row records `agent_account_id`
  (the *authoring* agent, `go/internal/store/forge_authored.go:34-46`), which may
  be a transient peer/sub-agent, not itself a Manager — so `ResolveResponder`
  (T4) walks from the recorded agent to its **owning Manager** (up the agent
  tree / owner relation) and returns that Manager's home channel. "Stable
  Manager" is guaranteed by this walk, never assumed to equal the authoring
  agent. This is the "already stamped" path: the ownership index already knows
  whose work this is.
- **If there is no recorded ownership row** (a human-filed issue delegated cold,
  or any coordinate Compass has never authored) → route to the **supervisor /
  top-level Manager** via a **dedicated routing channel**, which decides the
  owning lane and stamps accordingly (a normal Compass forge write through the
  DL-050 chokepoint, which records the DL-055 row). Subsequent events on the
  same issue then resolve directly to that lane. The routing channel is the
  supervisor's home surface for these hand-offs; the spike-era assumption of a
  single config-pinned responder is superseded by this routing path.

The routing resolution is a narrow seam (`ResolveResponder`, T4) so the receiver
and dispatcher do not embed the ownership-index query shape. Its output is a
stable `(managerAccountID, homeChannelID)` — the association below is keyed on
that, so a later change to the resolution policy is dispatcher-side, not a
schema change.

**The mapping: one issue → one comms topic in the resolved Manager's home
channel** (Matt ruled the Linear-session ↔ topic mapping is NOT forced 1-1,
RIG-2729 OQ-4: "Issues and topics aren't necessarily 1-1 — forcing that would
get awkward. I think we just link out to the Manager's home channel/workspace,
the user can then navigate to the correct thread from there"). Compass's native
unit for a conversation is a **comms channel + topic**, and its unit for "make
an agent act on a message" is the durable deliver rail: a posted message fans
out through the delivery consumer (`startDeliveryConsumer`,
`go/server/sinks.go:132-146`), which wakes an offline agent via the `AgentWaker`
seam (`c.SetAgentWaker(newLifecycleService(st, hub))`, `sinks.go:140`;
`WakeAgent` "best-effort resumes an offline agent's most recent session so an
owed mention or a subscribed deliver reaches it promptly",
`go/server/lifecycle.go:69-71`). So:

- The prompt lands in a topic **get-or-created for the Linear issue** (topic
  names are get-or-created inside the append transaction — "a TopicRef.Name is
  get-or-created on (channel_id, lower(name))",
  `go/internal/store/messages.go:13-16`), named for the issue identifier when
  present (e.g. `RIG-2717`), falling back to the session id for a bare @mention
  with no issue. Multiple Linear sessions / re-mentions on **one issue coalesce
  into one topic** — that is exactly the "not 1-1" shape Matt wants, and it
  keeps the conversation coherent across re-delegations.
- The association row (below) records `session_id → (channel_id, topic_id)` so a
  `prompted` follow-up routes to the same topic; the topic is an internal
  delivery detail, **not** a forced-canonical 1-1 surface.
- The **deep link points at the Manager's home channel** (OQ-4), not at the
  specific topic: the human opens Compass at the Manager's home channel and
  navigates to the thread. Per-topic deep-linking is a possible later
  refinement, but Matt chose home-channel to avoid forcing the 1-1 mapping.

The association is persisted in a new store table:

```sql
-- new migration (go/internal/store/migrations/, embedded per store.go:18-24)
CREATE TABLE linear_agent_sessions (
    linear_session_id   TEXT PRIMARY KEY,      -- Linear AgentSession.id
    manager_account_id  TEXT NOT NULL,          -- the resolved Compass Manager
    channel_id          TEXT NOT NULL,          -- the Manager's home channel
    topic_id            TEXT NOT NULL,          -- comms topic of the conversation
    linear_issue_id     TEXT,                   -- provenance (issue delegated on)
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**The `@linear` bridge account.** Linear-origin messages are authored by a
dedicated seeded system account `@linear`, NOT by `@compass`. `@compass` is
the platform's trusted system author that gives seeded agents their first
turn (`postSetupThread` "posts the platform's Setup thread as @compass into
the supervisor's home channel, giving the seeded root Manager its first
turn", `go/server/serve_seed.go:154-164`); posting arbitrary Linear-user
free text under that identity would launder external input into the system
author. With `@linear`, provenance is structural: a Linear-origin message is
self-evidently bridged. Seeding `@linear` follows the `@compass` shape but is
NOT a call to the existing `EnsureSystemAccount` as-is: that function is
**hardcoded to the single reserved handle** `SystemAccountHandle = "compass"`
(`go/internal/store/types.go:121-127`), inserted with no handle parameter
(`go/internal/store/accounts.go:151-153`) and re-fetched by that fixed handle on
restart (`accounts.go:157`). So the seed path must be **generalized to admit a
second reserved system handle** (T3a below) — a `(handle, displayName)` parameter
or a sibling `EnsureLinearBridgeAccount` — minting `@linear` as one additional
`system_accounts`-subtype row (idempotent find-or-create, the `@compass` seed
shape at `accounts.go:143-176`), with the system-account structural exclusions
(no delivery cursor, no roster, not agent-by-handle) extending to it by
construction. `@linear` must be `EnsureChannelMember`'d into the resolved
Manager's home channel before posting (PostMessage D9-gates the post on
membership; the postSetupThread precondition, `serve_seed.go:166`, membership
seeded with no delivery cursor per `accounts.go:325-335`). Because the post is
authored by `@linear` (≠ the agent), the delivery sweep's author-exclusion does
not bite and the message is swept, delivered, and — via the waker — starts a
turn on an idle agent. This is the exact rail the ask-answer-recovery
record built on ("an answerer-authored message is NOT author-excluded, so
it is swept, delivered, acked, and reconnect-redelivered by the one
delivery rail that already exists",
`docs/designs/product/compass-ask-answer-recovery/design.md:38-41`).

On `created`: resolve the Manager (routing above) → EnsureChannelMember(@linear)
into its home channel → get-or-create the issue topic → insert the association
row → emit the ack `thought` + the session external URL via the T2 client (the
10s SLA leg — before the post) → PostAsAccount(@linear, promptContext) with
`client_request_id="linear-delivery:<uuid>"` (the Part 1 dedup) into the topic.

On `prompted`: look up `linear_session_id` → association row; the user's message
is in `agentActivity.body` (linear.app/developers/agent-interaction §Session
webhooks: "A user sent a new message into an existing Agent Session. This
message is located in the `agentActivity.body` field"). Post it as `@linear`
into the SAME channel + topic, quoting the Linear author, with the same
`client_request_id="linear-delivery:<uuid>"` dedup. Same rail, same wake. An
unknown `linear_session_id` (e.g. a session created while the responder was down
and whose `created` event exhausted retries) is handled by synthesizing the
association as if `created` had arrived — the `prompted` payload carries the
`agentSession` object too, so the same routing + get-or-create runs.

**Immediate ack.** Linear marks a session unresponsive unless the agent emits
"an activity or update[s] your external URL within 10 seconds" of `created`
(linear.app/developers/agent-interaction §Session webhooks). The dispatcher
emits the `thought` activity ("Compass received the session; opening in
Compass…") and sets the session external URL directly from the webhook dispatch
path, before any agent turn begins — the agent wake can take longer than 10s
(container provision/resume), and under Option B nothing else needs the wake to
have completed.

### Part 3 — the return path: a dumb link (Option B)

Under Option B there is **no activity relay** — no comms-bus consumer mirroring
agent output, no settle-edge observation, no message-level `thought`/`response`
emission. The entire return path is two Linear-side emits on `created`, both
from the dispatch path:

- **One `thought` activity** acknowledging receipt ("Compass received the
  session; opening in Compass…"), satisfying the 10-second liveness SLA and
  telling the human what happened.
- **A session external URL** (`agentSessionUpdate` with an `externalUrls`
  entry, linear.app/developers/agent-interaction §Session external URL) — the
  "Open in Compass" deep link to the resolved Manager's home channel (Part 2 /
  OQ-4). This both routes the human to the surface where the exchange actually
  happens and prevents the session being marked unresponsive.

That is the whole return path. The agent's reply, the typed `ask` blocks, and
the rest of the conversation live in Compass, where the comms model represents
them faithfully — the Linear session never shows a degraded half-conversation
because it never tries to. Because there is no relay, the record needs **no**
settle-edge observation (the spike-era `SetSettleSink`-multicast question
dissolves — see §Resolved decisions OQ-9), **no** streaming-cadence policy (OQ-6),
**no** elicitation round-trip through Linear (OQ-8), and **no** Linear
session-lifecycle state machine beyond the one external-URL update (OQ-10).

**Option C, the ratified follow-up**, would add a single coarse one-way status
emitter on top of this same foundation — e.g. a `response` activity when the
issue's PR opens or the agent's turn settles — observing a coarse signal Compass
already has (a board/PR-state transition or a turn-settle edge), with no
message-level mirroring. It is a thin addition to the dispatcher, not a
restructuring of the receive/route/map path, and is out of scope for this
record.

### Part 4 — Linear-side auth (client_credentials, re-mint on 401)

The two Linear-side emits (the ack `thought` and the session external-URL
update) call Linear's GraphQL API with an app-actor token from the
**`client_credentials` grant**: POST `https://api.linear.app/oauth/token` with
`grant_type=client_credentials`, client_id + client_secret + `scope` → an
`app`-actor access token, `expires_in` ≈ 30 days, NO refresh token; the
documented rule is "fetch a new token if you receive a 401"
(linear.app/developers docs, verified in the RIG-2682 session; "client
credentials tokens" must be toggled ON in the app config first). The token
client holds the token in memory only (never stored — no new long-lived stored
token), re-mints on 401 with singleflight so concurrent 401s collapse to one
mint, and **pins the scope string** — requesting a token with different scopes
revokes ALL existing app tokens.

### Part 5 — secrets: declared-by-name, provided-by-value

The responder needs three secret values: the webhook signing secret, the OAuth
client_id, and the OAuth client_secret. They follow the existing secrets
boundary: the names are declared in the store registry and the values live in
the human-controlled provider vault, resolved server-side through
`secrets.Resolver` ("Resolve reads the whole names registry, generates the
manifest, resolves values from the configured provider",
`go/internal/secrets/resolver.go:33-36`; "The resolver process (the Server) is
the only place SecretSpec runs — containers receive resolved values, never
provider access", `resolver.go:53-55`). Matt sets the values once via
`SetSecret` — user-only by design ("SetSecret … USER-ONLY (record §911-927):
an agent-token caller is CodePermissionDenied",
`go/server/secrets_service.go:75-77`) — as `SecretKindGeneric` declarations
("a plain declared secret (DB URL, API token)",
`go/internal/store/secrets.go:31-33`). Names:
`LINEAR_WEBHOOK_SIGNING_SECRET`, `LINEAR_OAUTH_CLIENT_ID`,
`LINEAR_OAUTH_CLIENT_SECRET` (matching SecretSpec's env-var-name grammar,
`store/secrets.go:42`). The responder resolves them **server-side at
startup/first use** via the same `secrets.Resolver` instance
`buildNetworkServer` already receives (`resolver secrets.Resolver`,
`network_door.go:243`) — they are server secrets, never delivered into any
agent container.

**Degraded mode: fail-open to stale, fail-closed only on never-resolved.**
`Resolve` is inject-ALL: it "reads the whole registry (inject-all: no
per-agent filter in the MVP — a names filter is the future grants seam)"
(`go/internal/secrets/resolver.go:33-36`), every declared name is
`required = true` ("a missing one is a MissingRequiredError at resolve,
surfaced loudly", `resolver.go:123-125`), and one unresolvable name fails
the whole `Load` (`resolver.go:165-170`). So a naive TTL-expiry re-resolve
would fail the `/webhooks` path the moment Matt declares ANY unrelated
secret before providing its value — a 503 streak, which is exactly the
failure mode that gets the Linear category auto-disabled (the incident
that started this project). Therefore: the responder caches the
last-known-good resolved values of its three Linear secrets; on a resolve
FAILURE it serves the stale cached value (fail-open to stale — a rotation
still lands on the next successful resolve). It fails closed (503, never
accept an unverifiable webhook) only when a secret has NEVER successfully
resolved. The proper long-term fix is the narrow per-name resolve the
resolver doc itself anticipates ("a names filter is the future grants
seam", `resolver.go:35-36`); the stale cache is the scoped decoupling.

### Part 6 — the public URL is per-deployment (Matt ruled OQ-2)

Both the registered webhook URL (where Linear POSTs) and the "Open in Compass"
deep-link base are **per-deployment**, not a hardcoded `compass.rigel.build`
(Matt, RIG-2729 OQ-2: "that primary link is the managed service — selfhosted
deploys will have their own URL. And our dev deploys are currently tailnet only
so we need to figure that out … only put it on their VPN"). There is no existing
server-side public-base-URL config today (the string `compass.rigel.build`
appears only in the forge attribution icon constant, `go/internal/forge/
linear.go:60`). So this record adds a **server deployment config value — a
public base URL (flag + env), mirroring the existing `--server-addr` /
`$COMPASS_SERVER_ADDR` pattern** (`go/cmd/compass/main.go:65-67`):

- **Managed service** sets it to `https://compass.rigel.build` (the default the
  managed deploy config carries; it is also the registered webhook URL host).
- **Self-hosted deploys** set their own public URL.
- **Dev deploys are tailnet-only**, so Linear's cloud cannot reach their
  `/webhooks` and cannot open their deep link without the operator being on the
  VPN. This is a per-deployer reachability concern, not a code path: a
  tailnet-only deploy that wants live Linear delivery needs a public ingress /
  tunnel to its webhook path (the same problem every self-hosted deployer on a
  private network has). Called out in the ops runbook; not a blocker for the
  managed service or any deploy with a public URL.

The deep-link builder (Part 3) and the ops runbook's Linear webhook-registration
step both read this one value; the association/topic mapping is URL-independent.

## Alternatives considered

### Rejected: a dedicated ingress service for the webhook receiver

A separate small HTTP service (or a serverless function, as Linear's own docs
suggest for getting started) would isolate the public attack surface from the
compass-server process. Rejected because the responder needs the server's
internals on every event — the store (association rows + the ownership index for
routing), the comms handler (PostAsAccount), and the secrets resolver — so a
separate service would need its own authenticated channel back into
compass-server carrying exactly the same data, doubling the surface instead of
shrinking it. The network door already exists as the hardened internet-facing
surface (TLS, `ReadHeaderTimeout` G112, `withBodyReadDeadline` SEA-1298,
`network_door.go:306-323`). (Matt confirmed reuse of the network door, RIG-2729
OQ-2.)

### Rejected: a Connect RPC as the receiver

Every existing surface on the door is a Connect handler, so a
`LinearWebhookService` was weighed for convention's sake. Rejected: Linear
sends bare `POST` + JSON with its own HMAC authentication; a Connect handler
would sit behind the bearer interceptors (`network_door.go:263-267`) that
Linear can never satisfy, and signature verification needs the raw body,
which Connect's message decode consumes. A plain `http.Handler` on `netMux`
is the smaller, correct seam.

### Rejected: routing off a header parsed from the delegated issue's body

The delegated issue's body may carry a `compass:owner` header, and it is
tempting to parse it to decide the lane. Rejected as a hard rule violation:
DL-050 / DL-094 make a header *parsed from forge text* untrusted display
metadata that "must never reach any authz / routing / ownership decision"
(`go/internal/forge/owner.go:11-14`). Routing keys on Compass's own recorded
ownership index (`forge_authored_artifacts`, DL-055 / DL-205) instead — the
trusted server-recorded truth — and unstamped issues route to the supervisor
(Part 2).

### Rejected: relaying agent output back into the Linear session (Option A)

Mirroring the Compass conversation into the Linear session as activities was the
maximal option (Option A). Rejected for now (Matt, RIG-2729 OQ-1): Linear's flat
5-type activity vocabulary cannot represent Compass's typed comms surface
without loss — a structured `ask` degrades to a plain-markdown `elicitation`
losing its options (linear.app/developers/agent-interaction §Activity content
payload: "validated server-side, and invalid shapes will be rejected"; no
extension point) — so a relay would fight the comms model it is bridging.
Deferred until Linear makes the activity format configurable; Option C (coarse
one-way status) is the ratified intermediate step.

### Rejected: relaying `promptContext` as a direct control-op / synthetic turn

Injecting the prompt straight into the agent's session (a bespoke
DeliverControl or a synthetic exec) would bypass the durable delivery rail —
recreating exactly the owed-marker/bespoke-wake architecture Matt ruled out in
RIG-2257 ("in favor of the answer to an ask becoming a normal message on the
existing durable delivery rail",
`docs/designs/product/compass-ask-answer-recovery/design.md:7-10`). A comms
message is durable, redelivered across reconnects, deduped agent-side, and
wakes an offline agent through the shipped AgentWaker seam — all properties
the Linear loop needs and a bespoke injection would have to rebuild.

## Global Constraints

- Go, module `github.com/RigelBuild/compass/go` (`go/go.mod`); match existing
  server package conventions: plain `http.Handler` construction in the server
  package, `slog` structured logging, table-driven tests, pgtest harness for
  store-backed tests (the `*_pgtest_test.go` convention in `go/server/`).
- The network door is internet-facing: signature verification is MANDATORY
  and fail-closed (bad/missing signature → 400 before parsing — a deliberate
  choice, Linear's disable heuristic keys on failures generally; secret
  never-resolved → 503, never accept). A STALE `webhookTimestamp` on a
  valid signature is 200-with-drop (ack + discard), never 400 — no retry
  burn on a replayed signed body. The mount inherits
  `withBodyReadDeadline` + `ReadHeaderTimeout` (G112/SEA-1298,
  `network_door.go:306-323`); additionally cap the webhook body read with
  `http.MaxBytesReader` (1 MiB) — webhook payloads are small.
- Constant-time signature compare (`crypto/hmac.Equal`), raw-body HMAC (never
  re-stringified JSON), `webhookTimestamp` within 60s
  (linear.app/developers/webhooks §Securing webhooks).
- **Routing keys on recorded ownership truth, never a parsed header** (DL-050 /
  DL-094): resolve via the `forge_authored_artifacts` ownership index
  (DL-055 / DL-205); an unstamped issue routes to the supervisor via the
  dedicated routing channel, which stamps it through the DL-050 chokepoint
  (Part 2).
- The public base URL (webhook host + deep-link base) is a per-deployment
  config value (flag + env, `--server-addr` pattern), never hardcoded; managed
  defaults to `https://compass.rigel.build`, self-host sets its own, dev/tailnet
  deploys need their own ingress for Linear reachability (Part 6).
- Secrets follow the declared-by-name / provided-by-value boundary
  (`go/internal/secrets/resolver.go:33-55`); the three Linear secrets are
  server-resolved, never container-delivered. Resolve failures serve the
  last-known-good cached value (fail-open to stale; fail closed only when
  never resolved) so an unrelated unprovided declaration can never 503 the
  webhook path (Part 5).
- No new long-lived stored token: `client_credentials` grant, token held in
  memory, re-mint on 401 (singleflight), scope string pinned — different
  scopes revoke all existing app tokens.
- Linear SLAs: HTTP 200 within 5s (ack before any work); a `thought` activity
  (and the session external-URL update) within 10s of `created`, emitted from
  the dispatch path, never gated on the agent wake
  (linear.app/developers/agent-interaction §Session webhooks).
- Webhook processing is at-least-once (Linear retries ×3, no order
  guarantee): dedup rides PostAsAccount's `client_request_id` idempotency
  keyed on the `Linear-Delivery` UUID
  (`"linear-delivery:<uuid>"`; `go/internal/store/messages.go:141-146`);
  every store write idempotent (association insert is ON CONFLICT DO
  NOTHING keyed on `linear_session_id`). No separate dedup schema. The one
  uncovered emit is the `created` ack `thought` (Part 1 caveat).
- Tests: red-green, deterministic, no retries. Signature verify, envelope
  routing, dedup, routing resolution, association mapping, deep-link building,
  and token renew-on-401 all need unit coverage; the loop needs an integration
  proof against a fake Linear endpoint (see T8).
- New store table via a new numbered embedded migration
  (`go/internal/store/store.go:18-24` "migrationsFS holds the ordered,
  versioned schema migrations, applied at Open").

## Plan

Most tasks live in a new `go/internal/linearagent` package (protocol types,
signature verify, token client, routing resolution, dispatcher) plus thin
wiring in `go/server` (mount + assembly + public-URL config), matching the
internal-package layout the delivery/presence consumers use; T3/T3a add the
association table, the by-coordinate ownership read, and the `@linear` system-
account seed in `go/internal/store`. Task order is
dependency order; T0–T7 are the complete production bridge, with T8 the
integration proof. There is no relay stage — Option B's return path is the two
emits in T6. Everything here is production code (Matt: "Prod. Not really a spike
now"); the task ordering front-loads a first end-to-end delegation round-trip
(receive → route → topic → deep link back), not a throwaway proof.

### T0 — Secrets + ops: declare the three Linear secrets, register the webhook

No code: an ops step + a doc note. Matt runs `SetSecret` (user-only,
`go/server/secrets_service.go:75-77`) for `LINEAR_WEBHOOK_SIGNING_SECRET`,
`LINEAR_OAUTH_CLIENT_ID`, `LINEAR_OAUTH_CLIENT_SECRET`, all
`SecretKindGeneric` (`go/internal/store/secrets.go:31-33`), toggles
"client credentials tokens" ON in the Linear app config, and registers the
per-deployment webhook URL (`<public-base-url>/webhooks`) + re-enables the
"Agent session events" category. The runbook section of the PR body carries the
exact commands and the per-deployment URL guidance (Part 6).

**Ops note (auto-disable recovery):** a sustained `/webhooks` failure
streak risks Linear auto-disabling the "Agent session events" category
(the incident that opened this project); recovery is a MANUAL re-enable of
the webhook category in the Linear app console — the runbook names that
step explicitly.

Interfaces: consumes `SecretsService.SetSecret`
(`go/server/secrets_service.go:92`); produces three resolvable declarations
the responder reads via `secrets.Resolver.Resolve(ctx, reason) ([]ResolvedSecret, error)`
(`go/internal/secrets/resolver.go:33-49`).

### T1 — linearagent: webhook envelope types + signature verification

`go/internal/linearagent/webhook.go`: the `AgentSessionEvent` payload structs
(envelope: `type`, `action`, `webhookTimestamp`; `agentSession{id, issue{id,
identifier}, comment, previousComments, guidance}`, `promptContext`,
`agentActivity{body}` — shaped from the AgentSessionEventWebhookPayload schema),
and `VerifySignature(secret, rawBody []byte, header string) bool` — hex-decode
header, `hmac.New(sha256.New, secret)` over rawBody, `hmac.Equal`. Plus
`CheckTimestamp(webhookTimestamp int64, now time.Time, skew time.Duration) bool`.
Pure functions, unit-tested with vectors generated in-test (known secret +
body → expected hex).

Interfaces: produces `func VerifySignature(secret []byte, rawBody []byte,
headerHex string) bool`, `type SessionEvent struct{…}`,
`func ParseSessionEvent(raw []byte) (*SessionEvent, error)`.

### T2 — linearagent: client-credentials token client + activity/session emitter

`go/internal/linearagent/client.go`: `TokenSource` — mints via POST
`https://api.linear.app/oauth/token` (`grant_type=client_credentials`,
pinned `scope`), caches in memory, `singleflight` re-mint on 401, never
persists. `Client` — `CreateActivity(ctx, sessionID string, content
ActivityContent) error` wrapping the `agentActivityCreate` GraphQL mutation
(used under Option B only for the one `thought` ack; content types per
linear.app/developers/agent-interaction §Activity content payload), and
`UpdateSession(ctx, sessionID string, externalURLs []ExternalURL) error`
(`agentSessionUpdate` — the deep-link external URL). Unit tests against
`httptest.Server`: token cached across calls, 401 → exactly one re-mint
(concurrent callers coalesce), non-401 errors surface.

Interfaces: consumes client_id/client_secret values (from T7 wiring);
produces `type Client interface { CreateActivity(…); UpdateSession(…) }` the
dispatcher (T6) calls.

### T3 — Store: `linear_agent_sessions` association table

New migration `000N_linear_agent_sessions.sql` (next free number,
`go/internal/store/migrations/`; embedded + verified at Open,
`store.go:18-24`) with the schema in Part 2 (columns: linear_session_id PK,
manager_account_id, channel_id, topic_id, linear_issue_id, created_at — NO
dedup column; dedup is the comms rail's client_request_id, Part 1), plus
store methods in `go/internal/store/linear_sessions.go`:
`UpsertLinearAgentSession(ctx, row) (created bool, err)` (ON CONFLICT
(linear_session_id) DO NOTHING; returns created=false on replay) and
`LinearAgentSession(ctx, linearSessionID) (row, error)` (ErrNotFound miss —
the `prompted` lookup). No reverse-by-topic lookup (that was the relay's; there
is no relay under Option B). **Also adds the by-coordinate ownership read T4
needs** — `AuthoredArtifactByCoordinate(ctx, coord) (AuthoredArtifact, error)`
in `go/internal/store/forge_authored.go` (a trivial PK lookup: the forge
coordinate IS the `forge_authored_artifacts` PK; today only
`AuthoredArtifactByRequestID` / `ListAuthoredArtifactsByAgent` exist,
`forge_authored.go:116,143`). pgtest coverage.

Interfaces: produces the two `linear_sessions.go` methods + the by-coordinate
ownership read on `*store.Store`; consumed by T4/T6.

### T3a — Store: seed the `@linear` bridge system account

Generalize the reserved-system-account seed to admit a second handle so
`@linear` (the Part 2 bridge author) exists before T6 posts under it. Today
`EnsureSystemAccount` is hardcoded to `SystemAccountHandle = "compass"`
(`go/internal/store/types.go:121-127`; inserted with no handle parameter,
`accounts.go:151-153`; re-fetched by that fixed handle on restart,
`accounts.go:157`), so it cannot mint a second system handle as-is. Add either a
`(handle, displayName)` parameter or a sibling `EnsureLinearBridgeAccount`, and
a reserved handle constant `LinearBridgeAccountHandle = "linear"`, minting
`@linear` as one additional `system_accounts`-subtype row (idempotent
find-or-create, the `@compass` seed shape at `accounts.go:143-176`). Seed it
at server boot beside the `@compass` seed. Extend the reserved-handle guard so
`linear` is rejected for user/agent creation (the `compass` guard, T1 of the
system-sender record). pgtest: `@linear` seeds idempotently; the
system-account structural exclusions (no delivery cursor, no roster, not
agent-by-handle — `system_account_exclusion_pgtest_test.go`) hold for it; the
reserved-handle guard rejects `linear` for user/agent creation.

Interfaces: produces the generalized seed + `LinearBridgeAccountHandle`; the
seeded `@linear` account id is consumed by T6 (EnsureChannelMember + PostAsAccount).

### T4 — linearagent: responder routing resolution (OQ-3)

`go/internal/linearagent/routing.go`: `ResolveResponder(ctx, ev *SessionEvent)
(managerAccountID store.AccountID, homeChannelID string, err error)` — the
routing seam (Part 2). Depends on a narrow ownership-index read, not concrete
server types:

```go
type OwnershipIndex interface { // backed by the forge_authored_artifacts index
    // AuthoringAgentForCoordinate returns the recorded AUTHORING agent for a
    // forge coordinate (the row's agent_account_id — may be a peer, not a
    // Manager), or ErrNotFound when Compass has never authored it. Recorded
    // truth only — never a header parsed from forge text (DL-050/DL-094).
    // ResolveResponder walks from this agent to its owning Manager.
    AuthoringAgentForCoordinate(ctx context.Context, coord ForgeCoordinate) (store.AccountID, error)
}
```

On a delegated issue with a recorded ownership row → walk the recorded authoring
agent to its owning Manager (Part 2) + that Manager's home channel. On no
recorded row → the supervisor / top-level Manager + the dedicated routing
channel (config-resolved), which is where the supervisor decides the lane and
stamps (a normal forge write through the DL-050 chokepoint, recording the DL-055
row so later events resolve directly). Unit tests: recorded coordinate whose
authoring agent IS a Manager → that Manager; recorded coordinate whose authoring
agent is a peer → the peer's owning Manager (the walk); unknown coordinate →
supervisor + routing channel; a bare @mention with no issue → supervisor.

Interfaces: consumes the ownership index (DL-055 / DL-205,
`forge_authored_artifacts`) + the supervisor/routing-channel config; produces
`ResolveResponder`, consumed by T6.

### T5 — Server: the per-deployment public base URL config + deep-link builder

`go/server` (config + a small builder): add a public-base-URL deployment config
value (flag `--public-url` + `$COMPASS_PUBLIC_URL`, mirroring `--server-addr` /
`$COMPASS_SERVER_ADDR`, `go/cmd/compass/main.go:65-67`), and a
`deepLinkFor(channelID string) string` that builds the "Open in Compass" URL to
a Manager's home channel from that base (Part 6). Managed default
`https://compass.rigel.build`. Unit tests: builder output for a given base +
channel; empty base is a legible boot rejection (a deploy that consumes Linear
webhooks needs a public URL).

Interfaces: consumes the deploy config; produces the base value + `deepLinkFor`,
consumed by T6.

### T6 — linearagent: the async dispatcher (created/prompted → route + post + link)

`go/internal/linearagent/dispatcher.go`: a single goroutine draining a bounded
channel of verified events (enqueue from the HTTP handler; full → 500 so
Linear retries). Depends on narrow seams, not concrete server types (the
package-boundary discipline `runnerhub.LifecycleCaller` models,
`go/server/lifecycle.go:3-9`):

```go
type CommsPoster interface { // *comms.Comms satisfies via PostAsAccount
    PostAsAccount(ctx context.Context, account store.AccountID,
        req *compassv1.PostMessageRequest) (*compassv1.PostMessageResponse, error)
}
type Memberships interface { // *store.Store satisfies
    EnsureChannelMember(ctx context.Context, channelID string, account store.AccountID) error
}
```

On `created`: `ResolveResponder` (T4) → EnsureChannelMember(@linear) into the
Manager's home channel (the postSetupThread precondition,
`serve_seed.go:166-170`) → upsert association with a topic get-or-created for
the issue (name = issue identifier, else session id) → emit the ack `thought`
and set the session external URL (`deepLinkFor(homeChannel)`) via the T2 client
(the 10s SLA leg — before the post) → PostAsAccount(@linear, promptContext)
with `client_request_id="linear-delivery:<uuid>"` (the dedup) into the topic.
On `prompted`: lookup (miss → synthesize association via T4 from the payload's
agentSession) → PostAsAccount(@linear, agentActivity.body), same
client_request_id scheme. Every failure after ack: emit an `error` activity to
Linear and log; never crash the drain loop. **No relay** — the dispatcher does
not observe or mirror agent output; the return path is the two `created` emits.

Interfaces: consumes T1 types, T2 Client, T3 store methods, T4 ResolveResponder,
T5 deepLinkFor, CommsPoster; produces `func (d *Dispatcher) Enqueue(ev
*SessionEvent) error` + `func (d *Dispatcher) Run(ctx context.Context) error`.

### T7 — Server: mount `/webhooks` on the network door + assembly

`go/server/linear_webhook.go`: `newLinearWebhookHandler(secretProvider,
dispatcher) http.Handler` — MaxBytesReader(1 MiB) → read raw body →
VerifySignature (missing/bad → 400; secret never-resolved → 503, stale
cache served on resolve failure per Part 5) → parse → timestamp check
(stale → 200-with-drop, logged) → filter `type == "AgentSessionEvent"`
(others → 200 no-op) → Enqueue (full → 500) → 200. In `buildNetworkServer`
(`network_door.go:233-326`): construct the routing resolver + dispatcher +
client + handler and `netMux.Handle("POST /webhooks", h)` beside the Connect
mounts (`network_door.go:270-277`); start the dispatcher goroutine on the serve
errgroup beside `startCommsBusConsumers` (`serve.go:540`). The signing secret
is resolved lazily per request batch through the `resolver secrets.Resolver`
parameter `buildNetworkServer` already takes (`network_door.go:243`), cached
with a short TTL AND kept as last-known-good on failure (Part 5). Handler unit
tests (httptest): bad signature 400, stale timestamp 200-with-drop (nothing
enqueued), non-session event 200, queue-full 500, happy 200; a
`network_door_test.go`-style mount test proving `/webhooks` bypasses bearer auth
but Connect paths still require it.

**T0–T7 rollout gate: the first live delegation.** Delegate a real Rigel issue
to Compass → session acked (thought visible in Linear + the "Open in Compass"
external URL set), routed to the correct Manager (recorded-owner path for a
Compass-authored issue; supervisor + routing channel for an unstamped one),
promptContext lands in the mapped topic, and the deep link opens Compass at the
Manager's home channel. Also on the gate checklist: (a) force a 500 and observe
whether Linear re-signs the retry with a fresh timestamp+signature (determines
whether stale-drop / duplicate-ack-thought ever trigger, Part 1); (b) confirm
the per-deployment webhook URL + deep link resolve correctly for the managed
deploy.

Interfaces: consumes T1/T4/T5/T6, `secrets.Resolver` (`resolver.go:37-49`),
`netMux` (`network_door.go:270`); produces the live `POST /webhooks` endpoint
at `<public-base-url>/webhooks`.

### T8 — E2E: the full loop against a fake Linear

`go/server/linear_webhook_e2e_pgtest_test.go`: an `httptest.Server` playing
Linear (records agentActivityCreate + agentSessionUpdate calls, serves the token
endpoint), the pgtest store, real comms + routing + dispatcher. Scenarios: (1)
signed `created` for a recorded-owner issue → 200 fast, thought emitted, session
external URL set to the owning Manager's home channel, promptContext in the
mapped topic, association row; (2) signed `created` for an UNSTAMPED issue →
routed to the supervisor + posted into the routing channel; (3) replayed
delivery (same Linear-Delivery UUID) → no duplicate post (the client_request_id
dedup); (4) `prompted` → same topic; (5) 401 from fake Linear → one re-mint,
the emit retried once, succeeds; (6) tampered signature → 400, nothing enqueued;
(7) stale timestamp → 200-with-drop, nothing enqueued.

Interfaces: consumes everything above; produces the integration/e2e proof the
Global Constraints require.

## Tasks

- [ ] T0 — Declare + provide the three Linear secrets; enable
      client-credentials tokens; register the per-deployment webhook URL +
      re-enable the category (ops runbook in PR body).
- [ ] T1 — `linearagent` webhook types + HMAC signature verify + timestamp
      check (pure, unit-tested).
- [ ] T2 — Client-credentials TokenSource (in-memory, singleflight re-mint on
      401, pinned scope) + `agentActivityCreate` (ack thought) +
      `agentSessionUpdate` (external-URL deep link) client.
- [ ] T3 — `linear_agent_sessions` migration + store methods (upsert, lookup)
      + the by-coordinate ownership read (`AuthoredArtifactByCoordinate`) with
      pgtest coverage; no dedup column, no reverse-by-topic lookup — dedup
      is client_request_id-keyed on the comms rail (Part 1).
- [ ] T3a — Seed the `@linear` bridge system account: generalize the reserved
      system-account seed to a second handle (`LinearBridgeAccountHandle`),
      seed at boot, extend the reserved-handle guard, pgtest the exclusions.
- [ ] T4 — Responder routing resolution: recorded-owner (`forge_authored_
      artifacts`, DL-055/DL-205) → walk authoring agent to its owning Manager;
      unstamped → supervisor + dedicated routing channel; NEVER a parsed header
      (DL-050/DL-094).
- [ ] T5 — Per-deployment public base URL config (flag + env) + deep-link
      builder to a Manager's home channel.
- [ ] T6 — Async dispatcher: created → route + get-or-create topic + ack
      thought + external-URL deep link + promptContext post; prompted →
      follow-up post; posts carry `client_request_id="linear-delivery:<uuid>"`;
      error activities on dispatcher-visible failure. No relay.
- [ ] T7 — `POST /webhooks` handler mounted on `netMux` in
      `buildNetworkServer`; dispatcher started on the serve errgroup; rollout
      gate: a real delegation round-trips (receive → route → deep link back),
      retry re-sign observed.
- [ ] T8 — E2E pgtest against a fake Linear: full loop (recorded-owner +
      unstamped routing), replay dedup, prompted follow-up, 401 re-mint, tamper
      rejection, stale-timestamp drop.

## Resolved decisions

All ten open questions were ruled by Matt on 2026-08-25 (RIG-2729). The rulings
are folded into the Approach, Plan, and Global Constraints above; this section
retains each question and its ruling as the reasoning trail. The record freezes
on merge with no live open questions.

### OQ-1 (ruled: Option B, with C as follow-up) — mapping depth

How much of the interaction lives in the Linear session versus defers into
Compass. **Ruled: Option B (dumb link)** — one `thought` + an `externalUrls`
deep link, no relay. **Option C (coarse one-way status) is the ratified
follow-up; Option A (full relay) is off-table** until Linear makes its activity
format configurable. Matt: "B, with follow up for C. A doesn't make sense until
Linear make the format more configurable, which they might do eventually
although they are trying to sell their own agent product so idk." Rationale: the
comms model's threads/log split makes conversation a first-class typed surface
that Linear's flat 5-type activity log cannot represent without loss, so
mirroring a conversation there fights the model; a deep link routes the human to
the surface that works.

### OQ-2 (ruled: reuse the network door; the public URL is per-deployment)

Endpoint placement and URL. **Ruled: keep `POST /webhooks` on the existing
network door, but the public base URL (webhook host + deep-link base) is
per-deployment** — managed = `compass.rigel.build`, self-host = its own URL,
dev = tailnet-only (needs its own ingress for Linear reachability). Matt: "Yes
but that primary link is the managed service — selfhosted deploys will have
their own URL. And our dev deploys are currently tailnet only so we need to
figure that out … only put it on their VPN." Folded as Part 6 + T5 (a new
deployment config value).

### OQ-3 (ruled: routing is in scope; tie into the ownership index)

Which Compass agent runs a delegated session. **Ruled: do the routing** —
resolve the delegated issue to a stable Manager, keyed on Compass's recorded
forge ownership truth (`forge_authored_artifacts`, DL-055/DL-205), NEVER a
header parsed from forge text (DL-050/DL-094); an unstamped issue routes to the
supervisor/top-level Manager via a dedicated routing channel, which routes and
stamps. Matt: "We need to do the routing in any case so let's do that. We
already have the whole forge stamping thing on issues/prs/comments etc — can
likely tie in with that. If not already stamped, can just go to the supervisor/
top level to route and stamp accordingly, in a dedicated routing channel
probably." Folded as Part 2 + T4. (Supersedes the spike-era single
config-pinned responder.)

### OQ-4 (ruled: not 1-1; link to the Manager's home channel)

Session↔topic mapping. **Ruled: do NOT force one Linear session ↔ one topic** —
link out to the Manager's home channel/workspace and let the user navigate to
the thread. Matt: "Issues and topics aren't necessarily 1-1 — forcing that
would get awkward. I think we just link out to the Manager's home channel/
workspace, the user can then navigate to the correct thread from there." Folded
as Part 2: the prompt lands in an issue-named topic (sessions on one issue
coalesce), the association records it for `prompted` routing, but the deep link
targets the home channel, not the topic.

### OQ-5 (ruled: as designed) — secret names + rotation posture

The three declarations (`LINEAR_WEBHOOK_SIGNING_SECRET`, `LINEAR_OAUTH_CLIENT_
ID`, `LINEAR_OAUTH_CLIENT_SECRET`, all `SecretKindGeneric`) server-resolved on
demand with a short-TTL cache + last-known-good. **Ruled: LGTM.** Folded as
Part 5 unchanged.

### OQ-6 (dissolved under B) — streaming cadence

What maps to a Linear activity. **Dissolves under Option B** — there is no
relay, so nothing streams to Linear; the only emits are the `created` ack
thought + the external URL. Matt: "Yes per message for sure — the topics aren't
streaming anyway iirc? Streaming is only in the actual agent session. But also a
follow up later, B first." Revisited only if Option C/A land.

### OQ-7 (ruled: production) — rigor

Throwaway proof or production responder. **Ruled: production.** Matt: "Prod. Not
really a spike now, we've figured out the model, we need to be a Linear Agent
App to make sense to users." Folded throughout — the "spike" framing is removed;
task staging front-loads a first live delegation but every task is production
code.

### OQ-8 (dissolved under B) — elicitation answers

Free text vs structured `RespondToAsk`. **Dissolves under Option B** — no
`elicitation` is emitted to Linear (asks happen in Compass, where the typed ask
model works natively), so there is no Linear reply to map. Matt: "B anyway. See
above, maybe C later, A only if they let us do stuff like this in future."

### OQ-9 (dissolved under B) — settle-edge observability

How the relay observes turn-settle. **Dissolves entirely under Option B** —
there is no relay, so no settle observation is needed and the hub's
`SetSettleSink` seam is untouched. Matt: "Lgtm if we do a later." The
`SetSettleSink`-multicast widening becomes relevant only if Option A is
revisited.

### OQ-10 (dissolved under B) — Linear session lifecycle

When a session is done. **Dissolves under Option B** — the responder emits the
one external-URL update + the ack thought and drives no further session-state
machine; the session's job is to point the human to Compass. Matt: "Same, lgtm
later if we do A." A full lifecycle (terminal states, `awaitingInput`) is
designed only if Option A lands.

## Ledger-impact

Ledger-impact: adds DL-254, DL-255, DL-256 to `docs/designs/DECISIONS.md` at
freeze — the driver applies the rows (highest row on current main is DL-253; the
driver MUST re-verify the next-free id against current main at freeze rather
than trusting a session-time snapshot). Record links are relative to
`docs/designs/DECISIONS.md`, so they carry the `product/` prefix
(`tools/design-ledger-gate/index.ts:52-60` includes `product` in
`GOVERNED_ROOTS`).

Proposed rows:

| ID | Decision | Status | Record |
| --- | --- | --- | --- |
| DL-254 | The Linear Agent Session responder is a plain `POST /webhooks` `http.Handler` mounted on the compass-server network TLS door (inside `buildNetworkServer`, beside the Connect mounts, inheriting the G112/SEA-1298 guards; NOT a Connect service, NOT a dedicated ingress), fail-closed on the raw-body HMAC-SHA256 `Linear-Signature` check (bad/missing signature → 400; a stale-but-validly-signed `webhookTimestamp` is 200-with-drop, never a retry-burning 400), acking 200 before any work (Linear's 5s SLA) with all agent work async; the public base URL (webhook host + deep-link base) is a per-deployment config value, never hardcoded | Active (Matt, YYYY-MM-DD) | [linear agent responder §Part 1](product/compass-linear-agent-responder/design.md#part-1--the-webhook-receiver-on-the-network-door) |
| DL-255 | A delegated Linear session is routed to a stable Compass Manager keyed on Compass's recorded forge ownership index (`forge_authored_artifacts`, DL-055/DL-205) — NEVER a header parsed from forge text (DL-050/DL-094 forbid it reaching a routing decision); an issue with no recorded ownership row routes to the supervisor/top-level Manager via a dedicated routing channel, which decides the lane and stamps it through the DL-050 write chokepoint so later events resolve directly | Active (Matt, YYYY-MM-DD) | [linear agent responder §Part 2](product/compass-linear-agent-responder/design.md#part-2--routing-a-delegated-linear-session-to-a-stable-manager) |
| DL-256 | The Linear return path is a dumb link (Option B, Matt 2026-08-25): on `created` the responder emits one `thought` plus an `externalUrls` "Open in Compass" deep link to the resolved Manager's home channel and nothing else — NO activity relay, NO settle observation, NO Linear session-lifecycle machine. One Linear session is NOT forced 1-1 to a comms topic; the prompt lands in an issue-named topic (persisted in a new `linear_agent_sessions` table) delivered as `@linear`-authored deliver-rail messages deduped by `PostAsAccount`'s `client_request_id` on the `Linear-Delivery` UUID, but the deep link targets the home channel. Option C (coarse one-way status) is the ratified follow-up; Option A (full bidirectional relay) is off-table until Linear's activity vocabulary is configurable | Active (Matt, YYYY-MM-DD) | [linear agent responder §Part 3](product/compass-linear-agent-responder/design.md#part-3--the-return-path-a-dumb-link-option-b) |

(The `YYYY-MM-DD` and attribution cells are the driver's to stamp at freeze;
the `ROW_ACTIVE_RE` grammar is `Active (<who>, YYYY-MM-DD)`,
`tools/design-ledger-gate/index.ts:105`.)
