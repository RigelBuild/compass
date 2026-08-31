# Compass `@compass` system sender + root-supervisor first turn (RIG-1820 case-1)

Status: Draft

## Problem / Intent

The merged first-turn-delivery record removed `initial_prompt` and froze the
shape of case-1 (root-manager boot): the seeded root Manager's first turn is a
Compass-authored **initial Setup thread in its home channel**, sent by a
**reserved `@compass` system-sender alias** — and scoped the details out to
this follow-up. Per the parent record
(`docs/designs/agent/compass-first-turn-delivery/design.md:299-301`): "The
follow-up record owes: the reserved-alias representation, reserved-handle
validation, Setup-thread creation trigger (root-manager first
`StartAgentSession`), thread content/versioning, and its ledger rows." This
record decides exactly those things — (A) the `@compass` sender
representation and (B) the Setup-thread first-turn flow — and nothing the
parent already froze.

## Global Constraints

1. **Go module**: `github.com/RigelBuild/compass/go`. Server-side work
   lands under `go/server` and `go/internal/store`; no new top-level packages.
2. **The mechanism is frozen, not re-litigated.** Matt ruled (parent record
   OQ-C, its DL-187 row text at
   `compass-first-turn-delivery/design.md:570`): "The `@compass` reserved
   alias is FROZEN as the system-sender mechanism for ANY system-level message
   sender (not just the root-manager Setup thread), requiring reserved-handle
   validation at account creation; case-1 root-manager boot (a Compass-authored
   initial Setup thread in the manager's home channel) uses it and is scoped
   OUT to follow-up RIG-1820, which owes only the sender representation +
   Setup flow." This record inherits that verbatim.
3. **No prompt field anywhere.** The `initial_prompt` removal is frozen by the
   parent record's DL-186-equivalent row (`compass-first-turn-delivery/`
   `design.md:569`); nothing in this record threads a prompt through any start
   contract. An agent session always starts idle; its first turn is a channel
   message.
4. **The deliver lane is BUILT — do not redesign it.** The Runner receive arm
   exists (`go/internal/runner/dispatch.go:457-472`, the
   `SessionsResponse_DeliverControl` case calling
   `d.host.Deliver(ctx, c.DeliverControl.GetSessionId(), c.DeliverControl.GetOp())`),
   the gateway admits it (`go/internal/runner/gateway/control.go:69-70`:
   "DeliverControl is no longer among them: it carries a defined
   `compass.v1.Message`, so it is representable and sent"), and the Server
   fan-out wraps posts in the deliver op
   (`go/internal/delivery/consumer.go:288-293`, `deliverOp`). The Setup thread
   rides this lane as an ordinary posted message.
5. **Event-gated tests only.** No sleep/poll loops; tests observe bus events,
   store rows, or returned responses.
6. **Ledger discipline.** Matt ruled the first-turn ledger reconciliation
   (resolved OQ-1): the driver retro-lands the parent record's four rows in
   `docs/designs/product/DECISIONS.md` renumbered DL-187..190 (content
   verbatim, with a ledger-side mapping note) in a reconciliation PR; this
   record's two rows then follow as **DL-191..192** (T6), appended only after
   that PR lands.
7. **The fresh-start barrier-lift is built BY THIS RECORD (T-BL), ordered
   before T4.** The Setup deliver's whole premise — an idle-deliver starting
   the supervisor's first turn — depends on the agent-side replay barrier
   being lifted on a fresh start. That barrier defaults CLOSED
   (`packages/compass-agent/src/transport/control-source.ts:278`,
   `let replayComplete = false`; a pre-barrier deliver is refused-and-counted,
   `:361-377`), and NO production code sends `AgentControl{replay_complete}`
   today: the parent record's T-R3 is unchecked on main
   (`compass-first-turn-delivery/design.md:626`) and only the `replayPath`
   classifier + the ack-receive path exist (`gateway/control.go:218-226`,
   `:423-431` "No production caller exists yet"). Matt ruled (resolved OQ-6)
   that this record ABSORBS parent T-R3 rather than waiting on the parent
   lane: T-BL builds the fresh-start `replay_complete` send, and T4 is
   ordered after it.

## Approach

### What exists today (grounded)

**The author invariant.** Every message author is a real `accounts` row,
enforced by the schema itself:
`go/internal/store/migrations/0001_init.sql:200`:

```sql
author_account_id TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
```

Every write path routes through this FK:
`go/internal/store/messages.go:91-93` (`AppendMessage`'s single INSERT:
`INSERT INTO messages (id, topic_id, author_account_id, …)`), reached from
`go/internal/comms/comms.go:353-354` (`PostMessage`:
`c.store.AppendMessage(ctx, store.Message{ AuthorAccountID:
c.actorFromContext(ctx), … }`) and from the agent leg
`go/internal/comms/agent_caller.go:143` (`PostAsAccount`:
`c.PostMessage(WithActor(ctx, account), connect.NewRequest(req))`).

**No reserved-handle guard exists.** `CreateUser` validates only non-empty
(`go/internal/store/accounts.go:14-16`: `if u.Handle == "" { return
Account{}, fmt.Errorf("%w: user handle is required", ErrInvalidArgument) }`);
`CreateAgent` likewise (`accounts.go:132-134`: `if a.Handle == "" { … "agent
handle is required" … }`); `BootstrapAdmin` likewise (`accounts.go:63-65`:
`"admin handle is required"`). Nothing stops a user or agent registering the
handle `compass` today — exactly the gap the parent record named
(`compass-first-turn-delivery/design.md:283-286`).

**The deliver set is structurally agent-only.** `SubscribedAgents` JOINs
`agent_accounts` (`go/internal/store/delivery_reads.go:31-33`: `SELECT
aa.account_id FROM channel_members cm JOIN agent_accounts aa ON aa.account_id
= cm.account_id`), and its doc comment states the guarantee
(`delivery_reads.go:25-27`): "The JOIN to agent_accounts is what scopes the
result to AGENT members: a human member has no agent_accounts row and is
excluded, so a deliver is only ever dispatched to an agent session."

**An idle deliver starts a turn.** The agent-side handle:
`packages/compass-agent/src/agent.ts:84-85`: "message is coalesced to a
turn-end prompt: mid-turn delivers queue and flush as ONE prompt when the turn
settles; an idle deliver starts a turn at once." The barrier that would refuse
a pre-`replayComplete` deliver
(`packages/compass-agent/src/transport/control-source.ts:278,361-377`) defaults
CLOSED (`let replayComplete = false`) and is lifted on a fresh start ONLY by
the fresh-start `replay_complete` mechanism — a RULED decision (DL-188,
`compass-first-turn-delivery/design.md:571`) whose implementation (parent
T-R3) is UNBUILT on main (checkbox unchecked, `:626`; no production sender of
`AgentControl{replay_complete}` exists). Matt ruled this record builds it
itself: T-BL absorbs parent T-R3 (Global Constraint 7, resolved OQ-6), and T4
is ordered after T-BL.

**The seed path.** `seedRootSupervisor` is the runner-ready-hook body
(`go/server/serve.go:404`: `hub.SetRunnerReadyHook(func() {
seedRootSupervisor(ctx, st, svc, admin.ID, seedLog) })`). It find-or-creates
the supervisor (`go/server/serve_seed.go:73`, `st.AgentByHandle(ctx,
rootSupervisorHandle)`; create at `:139-144` via `st.CreateAgent`) and starts
it under a fixed idempotency key (`serve_seed.go:32`: `const
seedClientRequestID = "compass-root-supervisor-seed"`; the `SpawnAgent` call
at `:103-106`). An already-live supervisor returns
`connect.CodeAlreadyExists` and the seed exits early (`:107-110`); success
falls through to the completion log (`:120`).

**Home channel.** Minted at `CreateAgent` in the same transaction
(`go/internal/store/accounts.go:188-199`: the `INSERT INTO channels` +
`INSERT INTO channel_members (channel_id, account_id, subscribed) VALUES ($1,
$2, FALSE), ($1, $3, TRUE)` seeding owner + agent), resolved by
`homeChannel()` (`go/internal/comms/agent_caller.go:405-413`, returning
`string(acc.Agent.HomeChannelID)` and failing closed for a non-agent
account).

### A — `@compass` representation: a dedicated `system` account type (Matt-ruled)

The parent record names the fork
(`compass-first-turn-delivery/design.md:286-289`): "A system sender needs its
own design: reserved-handle enforcement at account creation, whether
`@compass` is a real reserved account row or a sentinel author, how the UI
renders it, and how delivery treats its posts (it must never be a deliver
*recipient*)."

**A real row, not a sentinel.** The sentinel option is ruled out at the
schema: `messages.author_account_id` is `NOT NULL REFERENCES accounts (id)`
(`0001_init.sql:200`), so a sentinel author id cannot even be INSERTed
without weakening the FK — and beyond the schema it would special-case every
read path that resolves an author (`delivery_reads.go:120`,
`messages.go:236/461/501/566/709` all select `author_account_id` for
join/render) and the UI's author resolution
(`apps/ui/src/components/ChannelView.tsx:184`: `const author = () =>
props.byId.get(props.msg.authorAccountId)`). A real row reuses
`AppendMessage`/`PostAsAccount` unchanged.

**Matt ruled the row's shape (resolved OQ-2): a dedicated `system` account
type** — a third first-class account shape alongside user and agent, not a
`user_accounts` subtype and not a bare `accounts` row. The codebase already
models account subtypes as per-kind tables joined onto `accounts`
(`0001_init.sql:42-45` `user_accounts`, `:70-77` `agent_accounts`;
`types.go:109-117`: `Account{ …, User *UserAccount, Agent *AgentAccount }`),
reconstructed in one place (`accounts.go:603-606`: "scanAccount reads one
joined account row … setting exactly the User or Agent subtype by which side
of the join populated") and mapped to the wire by the `accountToWire` switch
with a User arm and an Agent arm and NO default (`mapping.go:24-35`), onto
the proto `Account.kind` oneof (`proto/compass/v1/comms.proto:140-143`:
`oneof kind { UserAccount user = 10; AgentAccount agent = 11; }`). The
system type mirrors that pattern end to end:

- **Schema**: a `system_accounts` subtype table — `account_id TEXT PRIMARY
  KEY REFERENCES accounts (id) ON DELETE RESTRICT`, no payload columns; the
  row's existence is the discriminator. Landed by amending `0001_init.sql`
  (the store ships one squashed pre-dogfood migration whose inline comments
  still carry the folded 0005/0007/0015 markers, `0001_init.sql:52/64/66`) —
  or as `0002_system_accounts.sql` if a squash is not wanted (the loader
  applies contiguous `1..N`, `store.go:287-293`); implementer's choice (T2).
- **Store**: `Account` gains `System *SystemAccount` beside `User`/`Agent`
  (`types.go:109-117`); a third `scanAccount` arm keyed off a scanned
  `system_accounts` join column. `scanAccount` (`accounts.go:607-645`) does
  one fixed-arity `row.Scan`, so adding a scanned column means EVERY
  projection that feeds it must add the matching `LEFT JOIN system_accounts`
  and select the column — all SIX: the four inline id/handle reads
  `GetAccount` (`accounts.go:245-251`), `adminByHandle` (`:107-113`),
  `AgentByHandle` (`:504-510`), and the `ReparentAgent` re-read
  (`:418-424`); the shared `accountVisibleFromWhere` (`:543-556`, feeding
  `ListAccounts`/`AccountVisibleTo`); and the tree projection
  `agentTreeProjection` (`agent_tree.go:14-20`, feeding `queryAgents` →
  `scanAccount` at `:33`). The tree projection keeps its INNER
  `JOIN agent_accounts` (an agent row never carries a system subtype, so the
  new column is always NULL there) but MUST still select it, or the fixed
  scan arity mismatches — a Scan panic across every core account read.
  (Rejected alternative: infer `System` from the `scanAccount` else-arm —
  neither subtype populated ⇒ system — which adds no column and touches no
  projection, but reads any subtype-less or corrupt row as the PRIVILEGED
  system sender, a fail-open on the one account that must be positively
  identified; the scanned column fails closed.)
- **Wire**: a new proto `SystemAccount` message + `system = 12` case in the
  `Account.kind` oneof (`comms.proto:140-143`), a third `accountToWire` arm
  (`mapping.go:24-35`), and a proto regen (the `proto` moon `gen` target
  runs the three `buf generate` passes, `proto/moon.yml:54`).
- **UI (T5, now REQUIRED)**: the domain `Account.kind` union is
  `"user" | "agent"` today (`apps/ui/src/stub-data.ts:322-323`) and
  `adaptAccount` maps any non-agent oneof case — including unset — to
  `kind: "user"` (`apps/ui/src/live/adapt.ts:116-137`, the unset-case
  tolerance documented at `:112-115`), so without a client arm a system
  author renders as a plain user. T5 adds the `"system"` domain kind, the
  `adaptAccount` branch, and a system author style in `MessageRow`
  (`ChannelView.tsx:184-190`, `roleClass`).

**Added scope, stated plainly.** Versus the previously-drafted user-subtype
row, Matt's direction adds a proto wire change (+ regen) and a required UI
arm. That is the cost of a first-class type; the visibility win below is why
it is worth it.

The invariants, re-derived under the system type:

- **Row shape + seeding**: one `accounts` row, handle `compass`, display name
  `Compass`, with a `system_accounts` subtype row — NOT an `agent_accounts`
  row. Seeded by an idempotent `EnsureSystemAccount(ctx)` store method
  (find-or-create by handle, mirroring `BootstrapAdmin`'s
  unique-violation-means-fetch pattern, `accounts.go:78-81`), called in
  `Serve` immediately after the `BootstrapAdmin` call
  (`go/server/serve.go:325`) — the existing bootstrap seam, not a SQL
  migration (no account row is seeded by migration today; startup seeding is
  the established convention).
- **Reserved-handle enforcement**: unchanged — `CreateUser`, `CreateAgent`,
  and `BootstrapAdmin` gain the shared guard (T1) rejecting reserved handles
  with `ErrInvalidArgument`; `EnsureSystemAccount`'s own insert is not
  routed through them, so the system row itself is mintable.
  `BootstrapAdmin` must carry the guard too because the admin handle is
  operator-configurable (`serve.go:325`: `cfg.resolvedAdminHandle()`).
- **Directory visibility — the win of the ruling**: the drafted user-subtype
  row was swept into every caller's `ListAccounts` by the
  `u.account_id IS NOT NULL` disjunct (`accounts.go:548` — the first-class
  member directory). A system row hits NO broad disjunct: it is visible only
  through the shared-channel EXISTS clause (`accounts.go:550-555`), i.e.
  exactly the co-members of channels `@compass` posts into can resolve its
  handle (so the UI's `handleOf`, `apps/ui/src/comms.ts:26-31`, renders
  `compass` where its messages appear), and nobody else ever sees it in the
  directory or mention autocomplete. `GetRoster` excludes it structurally —
  every tree read INNER-JOINs `agent_accounts` (`agent_tree.go:11-12`: "the
  tree is agents-only", the `JOIN` at `:20`). A distinct system type is
  cleanly out of BOTH surfaces, which the user-subtype could not deliver; no
  extra exclusion clause is added anywhere.
- **Never a deliver recipient — structural, not policed**: unchanged in
  mechanism. With no `agent_accounts` row, `@compass` can never appear in
  `SubscribedAgents` (`delivery_reads.go:31-33`, the INNER
  `JOIN agent_accounts`) or `ChannelAgentMembers` (same JOIN shape,
  `delivery_reads.go:69-73`), so no deliver or mention→steer is ever
  dispatched to it; `AgentByHandle` returns `ErrNotFound` for a non-agent
  handle (`accounts.go:496-498,518-525`: "a user handle is deliberately
  indistinguishable from an unknown one"), so an `@compass` mention is a
  routing no-op; `SpawnAgent` cannot provision it (no agent row to resolve).
  The absent agent subtype IS the guarantee — pinned by T3's guard tests.
- **Not authenticatable**: no token is issued for the row in the normal flow
  (token minting is an explicit admin action), and the reserved-handle guard
  prevents anyone re-registering the handle. One residual surface:
  `IssueToken` (`service.go:414-439`) mints a bearer for ANY existing
  account id, so T2 adds a one-line `IssueToken` refusal for the system
  account — structural, not policy-by-omission (the admin-only door already
  gates it — defense in depth). Server-internal posting attributes to it via
  `PostAsAccount(ctx, compassID, req)` — the same `WithActor` path an agent
  post takes (`agent_caller.go:131-148`).

**Finer sub-fork, noted for Matt's PR review (not re-opened).** The wire
change could be avoided with a lighter discriminator: leave the
`Account.kind` oneof UNSET for a system account and let clients key off the
handle — `adaptAccount` already tolerates an unset case (`adapt.ts:112-115`).
**Recommended: the full `system = 12` proto case.** The unset-oneof shape is
documented client-side as the MALFORMED-row fallback ("a single bad row
never blanks the whole roster"), so shipping it deliberately would repurpose
an error path as a contract, and `roleClass` would still style system posts
as `user` with no principled discriminator. The alternative is recorded here
only so the trade is visible at review.

### B — the Setup-thread first-turn flow

**Trigger.** A new `postSetupThread(ctx, comms, st, supervisor)` step in
`serve_seed.go`, fired from `seedRootSupervisor` on BOTH arms that leave the
supervisor up: after a successful `SpawnAgent` (the fall-through to the
completion log, `serve_seed.go:116-120`) AND on the
`connect.CodeAlreadyExists` reject-on-live arm (`:107-110`) — the latter
repairs a prior boot that crashed between Start and post. Both arms are safe
because the post is idempotent (below).

**Idempotency.** The post carries a supervisor-scoped
`client_request_id = "compass-root-supervisor-setup-<supervisorID>"` (the
prefix mirrors `seedClientRequestID`, `serve_seed.go:28-32`; the scope is
OQ-7's folded recommendation, built by T4). The store dedups on the
`(author_account_id, client_request_id)` partial unique index
(`0001_init.sql:222-224`) via `AppendMessage`'s `ON CONFLICT … DO NOTHING`
(`messages.go:91-96`), and a deduped retry returns `inserted=false`, which
suppresses the `MessagePosted` fan-out (`comms.go:363-368`: "Publish only on
a genuine insert") — so a re-fired seed posts no duplicate for the SAME
supervisor and triggers no duplicate deliver, while a RECREATED supervisor
(a new account id) yields a fresh key and a fresh post.

**Channel + membership.** The thread posts into the supervisor's home channel
(`supervisor.Agent.HomeChannelID`, already in hand from the seed's
find-or-create; the generic resolver is `homeChannel()`,
`agent_caller.go:405-413`). `AppendMessage` gates on channel membership
(`messages.go:57`: `requireChannelMember(ctx, tx, m.AuthorAccountID,
ChannelID(channelID))`, backed by `go/internal/store/authz.go:21-24`), so the
seed first ensures `@compass` is a member of that channel via the new
idempotent `st.EnsureChannelMember` (T4; `ON CONFLICT DO NOTHING`,
`subscribed = FALSE`, mirroring the coordination-hook insert,
`coordination.go:273-277`). Membership does not make it a deliver
recipient (agent-only JOIN, above), and no delivery cursor is seeded
(cursors are per-agent).

**Sender.** `comms.PostAsAccount(ctx, compassAccountID, req)` with the
explicit home-channel id (never the empty-channel default, which resolves the
ACTOR's home channel — `agent_caller.go:127-128` — and `@compass` has none),
topic name `Setup`, and the supervisor-scoped `client_request_id` (above).
This reuses the whole human/agent write path: D9 authz, idempotency,
`MessagePosted` fan-out.

**Content + versioning.** The thread body is an in-repo `go:embed` markdown
asset (`go/server/setup_thread.md`), versioned with the server binary. The
root Manager's block-0 is already applied via its role
(`serve_seed.go:19-21`: role `manager` "selects config/prompts/manager/
SYSTEM.md as the container's block-0 prompt (RIG-1732)"), so the Setup thread
is the first-TURN driver, not a system prompt: it opens the Setup flow Matt
described verbatim (`compass-first-turn-delivery/design.md:30-38` — ask the
user what repos/projects, set up the tree/devenv shells). Content changes
ride server releases and affect fresh installs only: the supervisor-scoped
key is content-invariant (same supervisor → same key), so an already-seeded
supervisor never re-posts on a content revision (a versioned-key re-post is
a named future seam, deliberately not built — OQ-4).

**Delivery ride (built, cited, not redesigned).** The post's `MessagePosted`
reaches the delivery consumer; `SubscribedAgents` resolves the supervisor via
the home-channel disjunct (`delivery_reads.go:36`: `cm.subscribed OR
cm.channel_id = aa.home_channel_id OR ch.mandatory_subscription`) with the
author excluded (`:37`); `@compass` has no agent row, so the author-split
settle gate treats the post as human-timed (dispatched at post, per the
consumer's contract, `consumer.go:5-9`); the op is wrapped by `deliverOp`
(`consumer.go:288-293`), dispatched through the Runner's `DeliverControl` arm
(`dispatch.go:457-472`), admitted by `representable`
(`control.go:191-195`), and lands on the idle supervisor, which "starts a
turn at once" (`agent.ts:84-85`). On the happy path `promoteSession` (and its
start-sweep, which finds nothing) runs inside `SpawnAgent` BEFORE
`postSetupThread`, so the post lands against a bound session and rides live
dispatch. If the post instead lands before the session is live-bound, the
session-start sweep (`consumer.go:99-105`, `startEvent`: "sweeps the
freshly-live session's owed messages") converges it — the same
live-dispatch-OR-sweep convergence the parent record proved for case 2
(`compass-first-turn-delivery/design.md:262-268`). Caveat: sweep convergence
latency is UNBOUNDED — a Runner-side async refusal (e.g. the memo-join
dead-session case the seed documents, `serve_seed.go:41-52`) leaves the cursor
unadvanced and the message waits for the NEXT session-start edge
(restart/re-enroll). All of this presumes the fresh-start barrier is lifted
(the T-BL barrier lift, Global Constraint 7) — that is the gating
precondition, not this caveat.

**Acceptance (RIG-1820 case-1).** The seeded supervisor starts idle; the
`@compass` Setup thread appears in its home channel; its first turn starts
from that deliver; no prompt field is threaded anywhere.

### The ledger discrepancy (grounded)

The parent record MERGED and proposed four rows as "DL-186..189" under a new
`## First-turn delivery` section
(`compass-first-turn-delivery/design.md:560-573`). On main today:

- `DECISIONS.md` has NO `## First-turn delivery` section and no occurrence of
  `DL-187`, `DL-188`, or `DL-189` (verified by search this session).
- The number **DL-186 is already consumed by an UNRELATED decision**:
  `DECISIONS.md:181` is "Pre-dogfood proto wire-compat is stripped: all
  `reserved` markers removed across compass/v1 …" — not the `initial_prompt`
  removal the parent record drafted under that number.
- The true highest row on main is therefore **DL-186** (the proto
  wire-compat strip; DL-183/184/185 precede it, no DL-19x exists).

So the parent record's four first-turn rows never landed, and their drafted
numbers are stale (collided). Matt ruled the reconciliation (resolved OQ-1):
the driver retro-lands the four parent rows renumbered DL-187..190, content
verbatim, with a ledger-side mapping note ("drafted as DL-186..189 in
compass-first-turn-delivery/design.md; renumbered on landing — DL-186 was
consumed by the proto-strip decision"), never editing the merged parent
record's frozen prose. This record's own two rows (the `@compass`
representation decision and the Setup-thread flow decision) then follow as
DL-191..192 (T6).

## Plan

Ordered; each task carries its own red-green cycle. T1–T3 and T4 are one
server lane; T-BL is a runner-lane task this record absorbs from the parent
(ordered before T4); T5 is the compass-ui lane (REQUIRED — the wire shape
changes); T6 is the driver's ledger gate.

### T1 — Reserved-handle validation at account creation

A shared `validateHandle` guard in `go/internal/store` rejecting reserved
handles at every public creation path. It is a POSITIVE grammar, not a bare
blocklist: require the handle to match the mention grammar
`^[a-z0-9][a-z0-9._-]*$` (the server's `mentionRE`, `consumer.go:319`, kept in
sync with `apps/ui/src/comms.ts` `MENTION_RE`) plus a reserved-name blocklist —
`compass` (the system handle) and the three names the server already expands as
reserved broadcast mentions, `everyone`/`agents`/`users` (`consumer.go:326`
`reservedMentions`, mirrored at `comms-stub.ts:239`), since an account
registered as one of those would shadow a live broadcast semantic. The positive
grammar subsumes a leading-`@` reject AND forecloses the
whitespace/uppercase/Unicode-confusable spoof class (an unmentionable handle
like `Compass\u200b` that renders as `Compass`) in one move. Applied inside
`CreateUser` (`accounts.go:13`), `CreateAgent` (`accounts.go:131`), and
`BootstrapAdmin` (`accounts.go:62`) directly after their existing non-empty
checks (grep confirms these three `INSERT INTO accounts` sites,
`accounts.go:26/75/148`, are every creation path); NOT applied to the new
`EnsureSystemAccount` (T2), which mints the reserved `compass` row itself.

- Red: pgtest cases asserting `CreateUser`/`CreateAgent`/`BootstrapAdmin`
  with handle `compass` (and `Compass`, `@compass`, `agents`, `everyone`,
  `users`, and grammar violators — a leading-space handle and a
  zero-width-space spoof `Compass\u200b`) return
  `store.ErrInvalidArgument` and write no row — fail before the guard exists.
- Green: the guard; existing creation tests stay green.

Interfaces:

```go
// go/internal/store (internal helper, called by the three creation paths)
func validateHandle(handle string) error // ErrInvalidArgument on reserved
```

Existing signatures unchanged:
`func (s *Store) CreateUser(ctx context.Context, u NewUser) (Account, error)`
(`accounts.go:13`),
`func (s *Store) CreateAgent(ctx context.Context, ownerUserID AccountID, a NewAgent) (Account, error)`
(`accounts.go:131`),
`func (s *Store) BootstrapAdmin(ctx context.Context, u NewUser) (Account, error)`
(`accounts.go:62`).

### T2 — the `system` account type + `EnsureSystemAccount`

The type itself: the `system_accounts` subtype table (§A, schema bullet),
the store `SystemAccount` subtype + third `scanAccount` arm + the
`LEFT JOIN system_accounts` added to ALL SIX `scanAccount` feeders (§A,
Store bullet — the four inline id/handle reads, `accountVisibleFromWhere`,
and `agentTreeProjection`; `accounts.go:603-645` is the shared scan),
the proto `system = 12` oneof case + regen (`comms.proto:140-143`,
`proto/moon.yml:54`), and the third `accountToWire` arm (`mapping.go:24-35`).
Then the seed: an idempotent find-or-create of the reserved account (handle
`compass`, display name `Compass`, `system_accounts` subtype), mirroring
`BootstrapAdmin`'s unique-violation-means-fetch shape (`accounts.go:74-81`).
Called in `Serve` directly after the `BootstrapAdmin` call
(`go/server/serve.go:325`); its returned id is threaded to the seed (T4). A
pre-existing `compass` row of the wrong shape (e.g. a user or agent row from
a pre-guard database) is `ErrConflict` and fails startup — never silent
adoption, mirroring `adminByHandle`'s posture (`accounts.go:101-104`).

Because the system account is now resolvable as a first-class type, T2 also
adds a one-line `IssueToken` (`go/server/service.go:414-439`) refusal for the
system account id (identified via the scanned `System` subtype), so
`@compass` is structurally not authenticatable — a residual on the
admin-only token-minting door, closed here rather than in T1 because the
refusal depends on the system account existing and being resolvable (both
T2).

- Red: pgtest asserting the row exists after the call with `System != nil`
  (and `User == nil`, `Agent == nil`), is idempotent across a second call
  (same id), conflicts on a wrong-shape squatter, and round-trips through
  `scanAccount` as the system subtype; a wire-mapping test asserting
  `accountToWire` emits the `system` kind case; plus an `IssueToken` refusal
  for the seeded `@compass` id (now that the system account is resolvable).
- Green: the migration + store type + scan/wire arms + the method + the
  `Serve` wiring.

Interfaces:

```go
// go/internal/store
const SystemAccountHandle = "compass"
type SystemAccount struct{} // empty payload: the row's existence is the discriminator
// Account (types.go:109-117) gains: System *SystemAccount
func (s *Store) EnsureSystemAccount(ctx context.Context) (Account, error)
```

```proto
// proto/compass/v1/comms.proto — Account.kind gains:
//   SystemAccount system = 12;
message SystemAccount {}
```

### T3 — `@compass` is never a deliver recipient, nor in directory/roster (guard tests, no code)

The guarantees are structural (no `agent_accounts` row ⇒ absent from
`SubscribedAgents`' and `ChannelAgentMembers`' INNER JOIN,
`delivery_reads.go:31-33` and `:69-73`; `AgentByHandle` → `ErrNotFound`,
`accounts.go:496-498`; no `user_accounts` row ⇒ outside `ListAccounts`'
broad user disjunct, `accounts.go:548`; every roster read INNER-JOINs
`agent_accounts`, `agent_tree.go:20`). This task pins them with tests so a
future subtype change reddens:

- pgtest: with `@compass` a member of a channel, `SubscribedAgents` and
  `ChannelAgentMembers` exclude it; `AgentByHandle(ctx, "compass")` is
  `ErrNotFound`; `ListAccounts` for a viewer sharing NO channel with it
  excludes it, while a home-channel co-member sees it (the EXISTS clause,
  `accounts.go:550-555`); no roster read ever returns it.

Interfaces: read-only against the T2 account; no signatures change.

### T-BL — fresh-start barrier lift (absorbs parent T-R3; Matt-ruled)

On a FRESH (non-resume) `Start` — the discriminator is an empty
`resume_session_id` (`go/internal/runner/host.go:353-356`: "A fresh
(non-resume) start does nothing here. The discriminator is a non-empty
resume_session_id") — the Runner sends `AgentControl{replay_complete}` as
the first control op after the session's control state is bound
(`host.go:415-417`: `listener.BindSession(sessionID)`, which creates the
producer state, `gateway/socket.go:266-271`). The send rides the existing
producer path: `SocketListener.SendControl` (`socket.go:290`) →
`controlProducer.Send`, which retains until acked whether or not the agent
has subscribed yet (`control.go:228-231`: "It succeeds whether or not a
subscription is live: retention is what makes 'queued until acked' true").
`replay_complete` is already classified replay-path, so it passes any
barrier and is exempt from the retention cap (`control.go:218-226`,
`:410-412`). `HoldForReplay` — the restart-path barrier raise whose "No
production caller exists yet" (`control.go:423-431`) — stays untouched: a
fresh start never raises the Runner-side barrier; what this task lifts is
the AGENT-side default-closed barrier (`control-source.ts:278`,
`let replayComplete = false`), which otherwise refuses-and-counts every
deliver forever (`control-source.ts:361-377`).

The TS agent side is ALREADY BUILT end to end: the source decodes
`replayComplete` and lifts its local barrier (`control-source.ts:355-359`),
`CompassAgent` applies it (`packages/compass-agent/src/agent.ts:470-471`),
and the apply-then-ack `ReplayCompleteAck` is emitted on the Publish spine
(`control-source.ts:550-551`; `transport/control/ack-cursor.ts:96-105`),
pinned by test (`control-source.test.ts:449-476`). So the absorbed work is
the Runner-side send plus the end-to-end proof — narrower than the parent
T-R3 framing, stated so the scope is honest.

Cross-lane coordination: the parent first-turn-delivery PR-A scoped this
same mechanism as its T-R3. This record now owns building it; the driver
coordinates lane ownership with compass-runner/compass-agent so it is built
exactly once (resolved OQ-6).

- Red: a runner gateway/host test — a fresh `Start` produces
  `replay_complete` as the FIRST op on the session's control stream; a
  resume `Start` (non-empty `resume_session_id`) sends none. An agent-side
  e2e leg: a deliver sent AFTER the fresh-start `replay_complete` is
  dispatched, not refused-and-counted (`control-source.ts:373-377`).
- Green: the send at the fresh-start arm of `agentHost.Start`.

Interfaces: none new — the send composes `SocketListener.SendControl`
(`socket.go:290`) with the existing `AgentControl_ReplayComplete` variant.

### T4 — Setup-thread post in the seed path (trigger + idempotency)

`postSetupThread` posts the Setup thread as `@compass` into the supervisor's
home channel, fired from `seedRootSupervisor` on both up-arms: after a
successful `SpawnAgent` (`serve_seed.go:116-120`) and on the
`CodeAlreadyExists` reject-on-live arm (`serve_seed.go:107-110`). Steps:
`st.EnsureChannelMember(ctx, homeChannelID, compassID)` — the new idempotent
membership insert (Interfaces below) — then
`comms.PostAsAccount(ctx, compassID, req)` with the explicit
`supervisor.Agent.HomeChannelID`, topic name `Setup`, blocks from the
embedded `setup_thread.md`, and a supervisor-scoped `client_request_id`
(`compass-root-supervisor-setup-<supervisorID>`, OQ-7) — deduped by the
`(author_account_id, client_request_id)` index (`0001_init.sql:222-224`)
through `AppendMessage`'s `ON CONFLICT DO NOTHING` (`messages.go:91-96`),
with `inserted=false` suppressing a duplicate `MessagePosted`
(`comms.go:363-368`). A post failure is logged, not fatal, matching the
seed's own posture (`serve_seed.go:64-65`); the next ready-hook re-fire
retries it (the reject-on-live arm now reaches the post).

**Idempotency-key scope (OQ-7).** The key is supervisor-scoped, not a single
global fixed key. The index is `(author, client_request_id)` global per author
(`0001_init.sql:217-221`) and the author is `@compass` forever — so a global
key would silently suppress the Setup post for a RECREATED root supervisor
(operator deletes it, the empty-tree gate re-seeds a NEW one via
`createRootSupervisor`, `serve_seed.go:127-160`): the new post dedups against
the OLD row and the recreated Manager gets no first turn. A supervisor-scoped
key is immune and costs nothing.

**Retry cadence + gating.** The post-failure-is-non-fatal path retries only on
the next runner-ready-hook fire (`serve.go:401-408`), i.e. a Runner re-enroll —
a persistent post failure leaves the Manager's ONLY first turn undelivered
until some restart; acceptable for dogfood, stated so it is not a surprise.
T4 is ordered after T-BL (Global Constraint 7): without the fresh-start
barrier lift, `postSetupThread` succeeds at the store but the deliver is
refused at the agent barrier and stranded, so a green pgtest here would
still mean no first turn end-to-end — the T4 e2e acceptance is verified
against a tree where T-BL is present.

`seedRootSupervisor` needs the comms poster and the `@compass` id: extend its
signature (it is package-private with one call site, `serve.go:404`).

- Red: pgtest driving the seed twice — exactly ONE Setup message exists in
  the supervisor's home channel, authored by the `@compass` account, in topic
  `Setup`; a seed re-fire on the already-live arm posts nothing new; the
  first genuine insert publishes exactly one `MessagePosted` on the bus
  (event-gated: subscribe before, assert the received event).
- Green: `postSetupThread` + the two call sites + the embedded content file.

Interfaces:

```go
// go/server/serve_seed.go
// Effective key is supervisor-scoped (OQ-7): prefix + "-" + supervisorAccountID
const setupThreadClientRequestIDPrefix = "compass-root-supervisor-setup"
const setupTopicName = "Setup"

//go:embed setup_thread.md
var setupThreadBody string

func postSetupThread(ctx context.Context, cm *comms.Comms, st *store.Store,
    compassID store.AccountID, supervisor store.Account, log *slog.Logger)

// seedRootSupervisor gains the poster + system account id:
func seedRootSupervisor(ctx context.Context, st *store.Store, svc *service,
    cm *comms.Comms, adminID, compassID store.AccountID, log *slog.Logger)
```

Consumes: `func (c *Comms) PostAsAccount(ctx context.Context, account
store.AccountID, req *compassv1.PostMessageRequest)
(*compassv1.PostMessageResponse, error)` (`agent_caller.go:131-135`).

Channel membership (review fold M3): the post is gated on membership
(`messages.go:57`, `requireChannelMember`), so T4 adds an idempotent store
method rather than reusing `UpdateChannelMembers` (`channels.go:407`) — that
path D9-gates on the ACTOR already being a channel member
(`channels.go:414-421`), a chicken-and-egg for this very insert (the seed
runs server-internal, with no naturally-authorized actor on the supervisor's
home channel), and fires membership events + owner-transitive add logic the
seed does not want:

```go
// go/internal/store — idempotent, unsubscribed member insert mirroring the
// coordination-hook insert (coordination.go:273-277):
//   INSERT INTO channel_members (channel_id, account_id, subscribed)
//   VALUES ($1, $2, FALSE) ON CONFLICT (channel_id, account_id) DO NOTHING
func (s *Store) EnsureChannelMember(ctx context.Context, channelID ChannelID, accountID AccountID) error
```

No delivery cursor is seeded (`seedDeliveryCursor` is agent-only and
self-guarding, `coordination.go:265-268`; `@compass` has no agent row).

### T5 — UI system-author rendering (compass-ui lane, REQUIRED)

Required, not polish: T2 adds a new wire `Account.kind` case, which forces
client handling. Today the domain `Account.kind` union is `"user" | "agent"`
(`apps/ui/src/stub-data.ts:322-323`) and `adaptAccount` maps any non-agent
case to `kind: "user"` (`adapt.ts:116-137`), so a system author would render
as a plain user (`ChannelView.tsx:184-190`, `roleClass`). Work: add the
`"system"` domain kind, the `adaptAccount` system branch, a distinct system
author style in `MessageRow`, and exclude `compass` from mention
autocomplete beside the existing
`RESERVED_MENTIONS = ["everyone", "agents", "users"]` (`comms-stub.ts:239`).
Owner: compass-ui lane; ordered after T2's proto regen publishes the new
case (the client is forward-tolerant meanwhile, `adapt.ts:112-115`, so
T2–T4 do not block on it — but the record ships complete only with T5 done).

Interfaces: consumes the regenerated `@compass/client` wire types (the
`system` oneof case); UI-lane discretion within DL-148..156 constraints.

### T6 — Ledger rows DL-191..192 (driver, after the reconciliation PR)

Two new rows — (a) DL-191, the `@compass` representation: a dedicated
`system` account type (third first-class shape), startup-seeded,
reserved-handle-guarded, structurally never a deliver recipient and outside
directory/roster; (b) DL-192, the Setup-thread flow: post-seed trigger on
both up-arms, supervisor-scoped `client_request_id` idempotency,
home-channel carrier, embedded versioned content. Gated on the driver's
reconciliation PR landing the four parent rows as DL-187..190 first
(resolved OQ-1); appended under the `## First-turn delivery` section that PR
creates.

Ledger-impact: 2 new Active rows; no status flips (the `@compass`
system-sender freeze, landed DL-188, is inherited, not amended).

## Tasks

- [ ] T1 — reserved-handle validation (positive grammar + reserved-mention
      names) in `CreateUser`/`CreateAgent`/`BootstrapAdmin` + red-green pgtest
- [ ] T2 — the `system` account type (migration + proto `system` case +
      regen + `scanAccount`/`accountToWire` arms across all six feeders) +
      `EnsureSystemAccount` + `Serve` wiring + `IssueToken` refusal +
      idempotency/conflict pgtest
- [ ] T3 — never-a-deliver-recipient + directory/roster-exclusion guard
      tests
- [ ] T-BL — fresh-start `replay_complete` send (Runner) + e2e barrier proof
      (absorbs parent T-R3; ordered before T4)
- [ ] T4 — `postSetupThread` trigger + `EnsureChannelMember` +
      supervisor-scoped idempotency + event-gated pgtest + embedded content
      (ordered after T-BL)
- [ ] T5 — UI system-author arm (compass-ui lane, REQUIRED — new wire kind
      case)
- [ ] T6 — ledger rows DL-191..192 (driver, after the reconciliation PR)

## Resolved decisions

Settled by Matt's rulings on the draft; recorded here so the frozen record
carries the decision, not the fork.

### OQ-1 (resolved) — parent ledger rows: renumber + ledger-side mapping note

The merged parent record directed four rows ("DL-186..189",
`compass-first-turn-delivery/design.md:560-573`) into a `## First-turn
delivery` section of `DECISIONS.md`; on main no such section exists and
DL-186 is consumed by an unrelated proto wire-compat decision
(`DECISIONS.md:181`), so the drafted numbers are stale (see "The ledger
discrepancy", above). **Matt ruled:** the driver retro-lands the four parent
rows in `DECISIONS.md` renumbered DL-187..190 (content verbatim) in a
reconciliation PR, WITH a mapping note in the ledger itself ("drafted as
DL-186..189 in compass-first-turn-delivery/design.md; renumbered on landing
— DL-186 was consumed by the proto-strip decision"); this record's two rows
then follow as DL-191..192 (T6). The reconciliation NEVER edits the merged
parent record's frozen prose (which names DL-186..189) — a reader resolves
the stale numbers through the ledger note, honoring the freeze rule (a later
change ADDS, never rewrites). Driver-owned; T6 is gated on that PR landing.

### OQ-2 (resolved) — `@compass` is a dedicated `system` account type

The draft carried a user-subtype vs bare-row fork. **Matt ruled: neither — a
dedicated `system` account type**, a third first-class shape alongside user
and agent (§A). The user-subtype's directory cost (swept into `ListAccounts`
and mention autocomplete via the broad user disjunct, `accounts.go:548`) and
the bare row's unset-oneof wire shape (`accountToWire`, `mapping.go:24-35`,
has no default arm) both disappear: a system row is visible only to
shared-channel co-members (`accounts.go:550-555`) and ships a defined
`system` wire kind. Cost accepted with the ruling: a proto wire change +
regen and a required UI arm (T5). A finer sub-fork is noted for PR review in
§A: the full `system = 12` proto case (recommended) vs a no-wire-change
unset-oneof discriminator (rejected as repurposing the client's
malformed-row fallback, `adapt.ts:112-115`).

### OQ-3 (resolved, folded into T1) — reserved namespace breadth

Folded: T1 normatively reserves `compass` plus the three server
reserved-mention names `everyone`/`agents`/`users` (`consumer.go:326`),
since an account registered as one of those would shadow a live broadcast
semantic — no fork here, only a cheap squatter-foreclosure the server
semantics already demand. A broader general reserved-namespace policy (e.g.
`system`, `admin`) remains out of scope; deferred, non-load-bearing.

### OQ-6 (resolved) — this record absorbs parent T-R3 (T-BL)

The Setup-thread delivery depends on the fresh-start barrier lift: the agent
replay barrier defaults CLOSED (`control-source.ts:278`) and no production
code sends `AgentControl{replay_complete}` today (parent T-R3 unchecked,
`compass-first-turn-delivery/design.md:626`). The draft forked on whether
T-R3 lands parent-lane first or this record builds it. **Matt ruled: this
record's implementation absorbs T-R3** — built here as T-BL, ordered before
T4 (Global Constraint 7). Coordination implication: this record now owns
building a Runner+agent mechanism the parent first-turn-delivery PR-A also
scoped; the driver coordinates lane ownership with
compass-runner/compass-agent so it is built exactly once.

## Open Questions

Non-load-bearing deferrals only; each carries the folded recommendation the
plan builds.

### OQ-4 — Setup-thread content updates on already-seeded installs

The supervisor-scoped `client_request_id` is content-invariant: the same
supervisor id yields the same key, so a content revision never re-posts to
an already-seeded supervisor (only a RECREATED supervisor — a new account id
— gets a fresh post). **Recommendation:** accept for dogfood (the Setup
thread is a first-boot artifact); if re-delivery of revised content is ever
wanted, version the key (`…-setup-v2-<supervisorID>`) as a deliberate
product decision then — not built here.

### OQ-5 — Setup-thread content sign-off

T4 embeds `go/server/setup_thread.md`. The flow it opens is Matt's verbatim
shape (repos/projects interrogation, tree/devenv bring-up,
`compass-first-turn-delivery/design.md:30-38`), but the actual copy is
product voice and needs Matt's review at the implementation PR. Placeholder
posture: short, imperative, ends by asking the user which repos/projects to
manage.

### OQ-7 (folded recommendation) — supervisor-scoped idempotency key

T4 builds a supervisor-scoped Setup key
(`compass-root-supervisor-setup-<supervisorAccountID>`) rather than a single
global fixed key. Rationale: the idempotency index is `(author,
client_request_id)` global per author (`0001_init.sql:217-221`) and the
author is `@compass` forever, so a global key would silently suppress the
Setup post for a RECREATED root supervisor (delete + empty-tree re-seed,
`serve_seed.go:127-160`) — a hole OQ-4's content-versioning does not cover.
The scoped key is immune and costs nothing. Deferred as non-load-bearing
(the record builds the scoped key; Matt may override to a global key if
per-supervisor recreation is explicitly out of scope for dogfood).
