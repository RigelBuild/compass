# Compass v0.6 — the end-to-end product architecture

Status: Historical

> Internal design record — July 2026. The complete current Compass architecture
> in one place: Client → Server → Runner plus a first-party in-container agent,
> with the communication layer as the spine, Postgres as the store of record,
> and gRPC as the transport. It supersedes specific decisions of frozen records
> by citation — it does not rewrite any frozen record (records freeze on merge;
> `../compass-0.5/design.md:10-12`).

## Problem / Intent

The current Compass architecture is spread across frozen records that each
capture one delta: the v0.3 ADE vision (`../compass.md`), the v0.4 posture
(`../compass-0.4/design.md`), the v0.5 topology pivot
(`../compass-0.5/design.md`, which "captures only what v0.5 changes and why",
`../compass-0.5/design.md:10`), the v0.5 Server-tier refinement
(`../compass-0.5-server/design.md`), the UI-shell records, and the ACP session
record (`../../agents/sea-1023-acp-session.md`). An executing agent must stitch
six citations to see the whole system — and on two axes the stitched picture no
longer matches the tree: the live implementation is the Go module
`go/**` (there is no Rust under ``), and the live
event-delivery layer is the in-memory server event bus
(`go/events/events.go:1-2`: "Package events is the server event
bus: a monotonic-seq ring buffer plus a per-subscriber live tail"), not a
brokered stream substrate.

**Intent of v0.6:** state the complete, current, end-to-end Compass product
architecture — Client → Server → Runner, the communication layer as the spine,
the `compass.v1` contract, a first-party agent runtime on the OMP SDK,
per-agent container isolation on the Runner, Postgres as the store of record
with an S3-compatible blob seam and the in-memory bus for live fan-out, config
distribution, the authenticated network door, and gRPC as the single transport
— in one record that an executing agent can build against without archaeology.
It **supersedes specific frozen decisions by citation** (the comms-substrate
seam, the internal broker role, the ACP/BYOA agent model, and — by amendment —
the agent's primary interaction surface, inverting it to channel-primary with an
observation-only trace; see *Superseded decisions* below) and **builds on** the
frozen vision/UI/platform records by citation without restating them: v0.3
(`../compass.md`), v0.4
(`../compass-0.4/design.md`), the UI shell (`../compass-ade-shell/design.md`,
`../compass-dock-in-sidebar/design.md`), the desktop shell
(`../compass-tauri-shell.md`), and the Rust→Go platform port
(`../../platform/go-toolchain-default.md` — SEA-1243, whose T8/T9 build the
Server↔Runner seam this record conforms to).

One structural gap drives the plan's sequencing: the Server today is fully
ephemeral. The Go module has no database dependency at all
(`grep -rlE 'pgx|database/sql|jackc|lib/pq' go/` returns no
matches, verified against `main` this session), and the only stateful component
the serve loop constructs is the bus — "The one event bus every sequenced
stream rides" (`go/server/serve.go:138-140`,
`bus := events.NewBus[busPayload]()`), which `NewBus` mints **empty** at every
boot ("NewBus constructs an empty bus with a fresh per-boot instance epoch",
`go/events/events.go:151-152`). A restart therefore loses every
message, channel, and account. The Postgres store of record is the fix, so it
lands as the plan's earliest substantive task, not as a late swap.

## Approach

### Superseded decisions (by citation)

This record supersedes exactly four decisions of the frozen records — two
axes of v0.5, the ACP/BYOA agent model of the SEA-1023 session record, and
(this amendment) the agent's *primary interaction surface* of v0.5 D5.
Every other v0.5 decision (D2–D4, D6–D11, D13, D14) carries forward unchanged
and is cited in place below.

**Carried, not superseded: the implementation language.** v0.5 D1's Rust
choice ("a first-party **Rust comms service on NATS/JetStream**",
`../compass-0.5/design.md:122`) was already superseded by the frozen platform
record, not here: Rust→Go is SEA-1243's ruling — "This record designs the
*how*; the *whether* is settled"
(`../../platform/go-toolchain-default.md:16-17`). v0.6 builds on it by citation:
the backend implementation is the Go module `go/**` (module
`github.com/RigelBuild/compass/go`,
`go/cmd/compass-server/main.go:19`), gated by the Go CI battery
(`go/moon.yml:141-147`, `deps: ['fmt', 'vet', 'lint', 'nilaway',
'test', 'build', 'vuln', 'licenses']`).

1. **The swappable comms-substrate seam.** v0.5 D1 keeps the substrate behind
   a seam so it "stays swappable" (`../compass-0.5/design.md:123-126`: "sitting
   **behind a thin comms seam Compass owns** … The seam preserves the option to
   swap the substrate", `:200`). Superseded: **Postgres is the substrate**, and
   it is not swappable — the store of record is a relational database with
   real queries, indexes, and transactions, and every tier reads and writes it
   through the Server. The only seam this record keeps is a **narrow
   event-fan-out seam** at the Runner↔Server hop (see *State + storage* and
   *Resolved decisions*), the sole place a broker could later earn a role.
2. **The internal broker role.** v0.5 D1/D12 give JetStream the
   "communication layer only" role — "JetStream is treated as the
   communication layer only — a durable message bus, not long-term storage"
   (`../compass-0.5/design.md:468-470`) — an **external broker process** the
   Server publishes to and subscribes from (D1 runs the comms service **on**
   NATS/JetStream, `../compass-0.5/design.md:122`). Superseded: there is **no
   broker at all**. With the Server as both sole publisher and sole
   subscriber, the broker is a self-loop — a redundant, non-authoritative
   copy of data Postgres already holds (D12 already reserved the store of
   record for Postgres) — plus an external operational dependency every
   self-hoster would have to run; the live fan-out role it would fill is
   already built as the in-memory event bus
   (`go/events/events.go:1-13`), which stays.
3. **The ACP/BYOA agent model.** SEA-1023 fixes the agent runtime as ACP:
   adopt the `agent-client-protocol` SDK (Fork 1,
   `../../agents/sea-1023-acp-session.md:39-44`), ACP over stdio across the
   container boundary (Fork 2, `:46-52`), an ACP→`compass.v1` translation
   seam (Fork 4, `:64-68`), and "BYOA over ACP" with OMP as the external
   default/reference agent (`:19`). Superseded (Matt's decision, July 2026):
   the in-container agent is a **first-party program built on the OMP SDK,
   emitting `compass.v1` natively** — no ACP, no translator, and no BYOA
   machinery in the MVP (see *Agent runtime*). The frozen platform record's
   ACP-facing rulings (OQ1 "adopt the upstream Go SDK",
   `../../platform/go-toolchain-default.md:1219-1226`; T7 "ACP client in Go",
   `:861-868`; the boundary-table row "OMP (the base agent) | Rust | outside
   the boundary — the Runner drives it over ACP", `:88`) are mooted by this
   product decision; that record is SEA-1243's to amend, so the ripple is
   recorded as a **coordination item for SEA-1243**, never an edit here.
4. **The agent's primary interaction surface (v0.5 D5).** The frozen comms
   contract fixes the agent's surface as a first-class `AgentWorkspace` that is
   *itself the conversation* — "the agent's tool calls and asks render here"
   (`proto/compass/v1/comms.proto:207-213`), an agent turn being "a
   workspace message whose blocks stream in and update" (`comms.proto:225-228`),
   with `Ask` blocking until answered (`comms.proto:271-276`). Superseded (Matt's
   decision, July 2026): **the channel is the primary human↔agent surface, and
   the execution trace is observation-only.** The agent communicates with its
   user through exactly two *durable* surfaces — **channel messages** (regular or
   `ask`) and **pull requests** — while its execution trace (assistant/thought
   chunks, tool calls, plans, diffs) is a live, ephemeral **observation** stream,
   not persisted as comms `Message` rows (see *The communication layer* and
   *State + storage*). Three consequences ride this inversion, all ratified (see
   *Resolved decisions*): (a) **`Ask` is async** — the "blocks until answered"
   clause is dropped; blocking becomes the agent's turn-level choice, and a human
   steer (`@`-mention) and an ask-answer are one session-injection path; (b) the
   **comms `MessageBlock` surface narrows** to the durable conversation
   (`text` + `ask`), the trace variants moving to the observation stream; (c)
   **owner-membership is transitive** — an agent's DMs and the channels it starts
   always include its owning user(s), so a user can inject in response to
   anything their agent said. Three round-two forks were then decided (July 2026,
   this amendment; see *Resolved decisions*): (d) **threading gets a carrier** —
   an additive `parent_message_id` on `Message`; (e) **the trace is a dedicated
   OMP-native session stream** (`SubscribeAgentSession`), not typed
   `SubscribeEvents` variants — the three ACP-translation variants are dropped
   and `SubscribeEvents` keeps only Compass projections (liveness, lifecycle,
   board); (f) **one ACL** — observation-pane access is a projection of channel
   membership, so the workspace `participant_user_ids` + share/unshare RPCs are
   removed. This supersedes only the *primacy and persistence* of the surface;
   the workspace type survives, demoted to the observation pane (the trace plus
   terminal/file panes), no longer a message container.

### The three tiers

The topology is **Client → Server → Runner** (`../compass-0.5/design.md:71-95`),
each tier one responsibility, all first-party code — Go on the Server and
Runner, TypeScript on the Client and the in-container agent (see *Global
Constraints*):

- **Client** — the web UI in a browser (MVP; the Tauri desktop app is deferred,
  `../compass-0.5/design.md:330-336`). It is a `compass.v1` client over
  gRPC-Web/Connect; the generated client is the only sanctioned way to reach
  the Server (`proto/compass/v1/compass.proto:1-4`: "The generated
  clients are the only sanctioned way to reach the server"). It reuses the
  SolidJS shell and its workstream/agent state model
  (`../compass-ade-shell/design.md`, `../compass-dock-in-sidebar/design.md`)
  with the communication layer as the primary surface, and assumes nothing
  "local" beyond the transport boundary
  (`../compass-tauri-shell.md:119-121`: "the shell and UI must never assume
  'local' beyond the transport boundary — no socket path or `localhost` leaks
  above the `fetch`/command seam").
- **Server** — the long-lived, networked, multi-user orchestrator and the seat
  of all state: accounts, channels, messages, agent workspaces, agent
  transcripts, centralized agent config, and the board projection. It serves
  both `compass.v1` services — `CompassService`
  (`proto/compass/v1/compass.proto:14-53`) and `CommsService`
  (`proto/compass/v1/comms.proto:38-100`) — off one connect-go
  handler stack (the serve loop already serves "native gRPC (HTTP/2), gRPC-Web,
  and Connect off one connect-go handler",
  `go/server/serve.go:3-5`), owns the Postgres store of record,
  and fans live events out on the in-memory bus.
- **Runner** — a binary dropped onto any machine that should host agent
  containers, "like a CI runner: it manages the per-agent containers … connects
  out to the Server, and streams container activity back"
  (`../compass-0.5/design.md:89-95`). The container-runtime layer it drives is
  built: `ContainerRuntime` is "the container engine seam … An interface so the
  Runner can hold a ContainerRuntime and tests can substitute a fake"
  (`go/internal/runtime/podman.go:271-276`), with `PodmanCLI` as
  the rootless-podman implementation (`podman.go:321-324`; "Rootless is a hard
  requirement … no daemon, no root, no rootful fallback", `podman.go:24-25`).
  Runners hold no durable state: all agent state lives in the Server, so a
  Runner host dying costs a restart, never context.

### The communication layer (the spine)

The communication layer is a Discord/Slack-style channel system in which
**humans and agents are first-class accounts in a management hierarchy** — the
product's core pillar, carried from v0.5
(`../compass-0.5/design.md:99-121`). The frozen `compass.v1` comms contract
states the model directly (`proto/compass/v1/comms.proto:1-11`):

> "The compass.v1 communication layer: the Discord/Slack-style channel system
> that is the spine of Compass … Humans and agents are first-class accounts in
> a management hierarchy. Channels nest in channel groups … An agent's
> interactive surface — its ACP UI: the conversation (tool calls, plans,
> diffs, structured asks) plus terminal and file panes — is a first-class
> AgentWorkspace … All comms flow through this layer, so audit and search are
> properties of the substrate, not a separate pipeline."

(The proto comment's "ACP UI" wording is pre-cutover doc-intent: under v0.6 the
agent is first-party, not ACP — see *Agent runtime*. This amendment goes
further and **inverts the surface's primacy** (superseded decision 4 above):
the frozen contract makes the `AgentWorkspace` *itself* the conversation, with
the agent's turn streaming in as workspace `Message` blocks. Under this
amendment the **channel is the primary human↔agent surface** and the workspace
is demoted to an **observation pane** — the live execution trace plus the
terminal and file panes — no longer a persisted-message container. The bullets
below are restated accordingly.)

Concretely, as the contract fixes it:

- **Accounts** (`comms.proto:104-142`): `Account` with `UserAccount` (role:
  member/admin) and `AgentAccount` subtypes; an agent is "a constrained, owned
  subtype … gated by its owning user, who has first-class control over which
  channels and groups the agent may see and post to" (`comms.proto:132-135`).
  An `AgentAccount` additionally carries an **additive `home_channel_id`** (RT-2,
  ratified) — the agent's named channel/DM, minted at `CreateAgent` — which fixes
  "the agent's own channel" for the always-subscribed row, turn-end delivery, and
  the observation-pane ACL (see *Ratified additive contract changes*, *Resolved
  decisions* RT-2).
- **Channel groups + channels** (`comms.proto:144-203`): nested namespace
  groups carry visibility that can only narrow toward the leaves ("a child
  group cannot widen its parent's scope", `comms.proto:150-151`), so a user's
  owned agents work in that user's space by default while shared channels stay
  open. **Owner-membership is transitive** (superseded decision 4): an agent's
  DMs and any channel an agent creates always include the agent's owning
  user(s) in `member_account_ids`, so a user can read and inject into anything
  their agent said or was told — enforced server-side at channel/DM creation
  (an agent↔agent DM carries *both* owners). **Precedence:** for `DM`/`GROUP_DM`
  kinds, `member_account_ids` **grants visibility regardless of the group's
  `ChannelGroupVisibility`** — the frozen `OWNER`/`SHARED` enum
  (`comms.proto:174-180`) has no value that represents a cross-owner DM (`OWNER`
  excludes the other owner; `SHARED` leaks to all accounts), so membership, not
  the lattice, governs DM visibility; plain channels stay lattice-governed. This
  is a **deliberate cross-owner disclosure**: both owners read the full
  agent↔agent DM, so one owner's agent can surface context it holds into a DM the
  other owner reads — the intended consequence of agents-as-accounts, not a leak.
  See *Ratified additive contract changes*.
- **The channel is the primary human↔agent surface** (superseded decision 4):
  a user talks to an agent in a channel named for it — a Slack/Discord-style DM
  conversation with threading (an additive `parent_message_id` on `Message`;
  see *Ratified additive contract changes*) — and the agent replies there. Two
  message kinds
  carry the whole durable interaction: a plain `text` message and a structured
  **`ask`** (`Ask`/`AskOption` + `RespondToAsk`, `comms.proto:277-296,86-88`).
  `Ask` is **async** — the frozen "blocks until answered" clause
  (`comms.proto:271-276`) is superseded: the agent posts the question and
  chooses at the *turn* level whether to wait (end the turn) or keep working and
  fold the answer when it lands (`rule://never-block` at the human boundary). A
  human **steer** — `@`-mentioning the agent in the channel — and an ask-answer
  are the **same session-injection path** into the running agent (see *Ratified
  additive contract changes*).
- **The `AgentWorkspace` is the observation pane, not the conversation**
  (`comms.proto:207-213`, superseded decision 4): it renders the agent's live
  **execution trace** (assistant/thought chunks, tool calls, plans, diffs) plus
  the terminal and file panes, streamed from the **dedicated OMP-native
  session-tail stream** (opaque frames rendered by OMP's own renderer; *not*
  `SubscribeEvents`, which keeps only Compass's own projections) —
  observation-only. The user watches here, can **stop** the agent, and steers
  *through the channel* (not here). **Access is a projection of the agent's
  home-channel membership** (RT-2, ratified) — a member of the agent's **home
  channel** may watch its pane, scoping `SubscribeAgentSession` the same way; the
  trace carries nothing more sensitive than the conversation the same members
  already read, so it is the one shared ACL, not a stricter trace-specific one.
  There is no separate workspace share (`participant_user_ids` and the
  `ShareAgentWorkspace`/`UnshareAgentWorkspace` RPCs are removed, fork f). It is
  **not** a persisted-`Message` container: the trace is live session frames plus
  the S3 session, never comms `Message` rows (see *State + storage*).
- **Durable messages are the conversation, not the trace** (`comms.proto:225-229`):
  a `Message` on the comms surface carries the durable human↔agent conversation
  — a human's channel message, or an agent's `text` reply or `ask`. The trace
  variants of `MessageBlock` (`thought`/`tool_call`/`plan`/`diff`,
  `comms.proto:247-262`) leave the comms surface entirely — they are OMP-native
  session data on the observation stream, and **diffs and plans are surfaced as
  pull requests** (link-out for the MVP; native PR viewing in Compass is a later
  increment). The `MessageBlock` oneof narrows to `text` + `ask` (the trace
  variants are removed, OQ-A resolved; see *Ratified additive contract changes*).
- **Audit + search cover the durable conversation** (D1, narrowed by superseded
  decision 4): `SearchMessages` is "the audit/search property served from the
  store of record" (`comms.proto:90-93`), scoped server-side to the caller's
  visible set — never a separate pipeline. With the trace no longer persisted as
  `Message` rows, full-text search covers the durable conversation (channel
  messages + asks) and the PR trail; the *execution trace* is reviewed by
  replaying the S3 session, not by comms search. This narrows D1's
  "search is a property of the substrate" from "everything the agent emitted" to
  "everything said in the conversation" — a deliberate, ratified cut (see
  *Resolved decisions*): search what was *said*, replay what was *done*.
- **Authorization is connection-bound**: "the caller is the account
  authenticated on the connection … never a field in a request, which would be
  spoofable" (`comms.proto:31-33`).

### The `compass.v1` contract (cited, not redesigned)

The contract is frozen and generated; this record builds on it (see *Ratified
additive contract changes* below) — **four** public RPCs (`CreateChannel`,
`ProvisionAgentWorkspace`, `SubscribeAgentSession`, `UpdateChannelMembers`), two
internal agent-stdio messages (`AgentFrame`, `AgentControl` — carrying the RT-3
`deliver` control + `delivery_ack` frame), the `snapshot_seq` +
`parent_message_id` + `home_channel_id` fields, the channel-membership carriers,
and a refinement of `since_seq = 0`'s
documented meaning (see *State + storage*) — **plus three pre-launch buf-breaking
removals** (the ACP-translation variants, the share/participant RPCs, and the
`Message.container` `workspace_id`; enumerated and justified below), and nothing
else beyond that set. Two services in one owned `compass.v1`
package:

- **`CompassService`** — the server/agent-session lifecycle door
  (`proto/compass/v1/compass.proto:14-53`): `GetServerInfo` (the
  connect-time liveness/version probe), `SubscribeEvents` (the sequenced
  server-stream event channel — "Each response carries a server-assigned
  monotonic `seq`; reconnect with `since_seq` for a gap-free resubscribe",
  `compass.proto:20-22`), `StartAgentSession` / `StopAgentSession` /
  `ReloadAgentSession` / `GetAgentStatus` (the agent-session lifecycle),
  `IssueToken` (admin-gated token minting: "Token is 32B random, base64url,
  returned once; the server stores only its SHA-256 hash",
  `compass.proto:47-49`), and — **added** by the inversion — `SubscribeAgentSession`,
  the dedicated OMP-native session-tail stream for the observation pane (see
  *Ratified additive contract changes*, fork e).
- **`CommsService`** — the communication-layer door
  (`proto/compass/v1/comms.proto:38-100`): `CreateUser`,
  `CreateAgent`, `ListAccounts`; `CreateChannelGroup`, `ListChannelGroups`,
  `ListChannels`; `OpenAgentWorkspace`, `ShareAgentWorkspace`,
  `UnshareAgentWorkspace` (the last two **removed** by the inversion — see
  *Ratified additive contract changes*, fork f); `ListMessages`, `PostMessage`,
  `RespondToAsk`, `SearchMessages`; `SubscribeComms` (the comms event
  stream, mirroring `SubscribeEvents`' seq/epoch replay contract,
  `comms.proto:95-99`); and — **added** by this record — `CreateChannel` and
  `UpdateChannelMembers` (the membership-mutation RPC, RT-1; see *Ratified
  additive contract changes*).

Both event streams share one replay model: a server-assigned monotonic `seq`
plus a per-boot `instance_epoch`, with a terminal resync signal when a cursor
cannot be served gap-free (`comms.proto:300-324`,
`compass.proto:119-123`). Go server stubs and the TS client are generated and
CI drift-gated (`go/moon.yml:39-42` — the `drift` task "Fail[s] if
the checked-in Go stubs are stale vs the schema"; generated handlers at
`go/gen/compass/v1/compassv1connect/`, e.g. `NewCommsServiceHandler`,
`comms.connect.go:393`). A contract change means: edit the schema → `moon run
compass-go:gen` → commit the regenerated output.

#### Ratified additive contract changes

The plan requires contract changes that do not exist in the frozen
contract. Most are **additive or doc-intent supersessions** (new methods / two
new internal messages / new fields / narrowed-or-clarified comments — safe
under the buf-breaking gate); **three are pre-launch buf-breaking removals**
(the ACP-translation variants, the share/participant RPCs, the
`Message.container` `workspace_id`) that will **fail** a buf-breaking check and
ride the pre-launch override — justified below and safe only because the Server
on `main` is ephemeral with no live client. All are ratified by Matt at this
record's freeze gate (see *Resolved decisions*), and land in their implementing
task's PR via the gen/drift cycle. The first four are the original v0.6 set; the
rest ride the interaction-surface inversion (superseded decision 4), detailed
after them:

- **`CreateChannel`** (T2). `CommsService` today can create a channel *group*
  but not a *channel* — the full RPC set (`comms.proto:38-100`) has
  `CreateChannelGroup` and no `CreateChannel`, yet the event surface already
  documents "A channel was created" (`ChannelChanged`, `comms.proto:338-339`).
  `rpc CreateChannel(CreateChannelRequest) returns (CreateChannelResponse)` —
  caller-authorized against the parent group, emitting `ChannelChanged`. (If
  DMs make the MVP cut, an `OpenDirectMessage` RPC is the same class of
  additive change; not required for the T1–T8 plan.)
- **`ProvisionAgentWorkspace`** (T4). No RPC launches a container:
  `StartAgentSession` assumes one exists ("The launched container's stable
  name", `compass.proto:198-199`), and none of `CompassService`'s seven RPCs
  provisions one. `rpc ProvisionAgentWorkspace(ProvisionAgentWorkspaceRequest)
  returns (ProvisionAgentWorkspaceResponse)` — agent ref + repo/workstream
  spec in, `container_name` out — routing Client → Server → RunnerHub →
  Runner and driving the built lifecycle façade
  (`go/internal/runtime/agent.go:1-5`). Provision and start stay
  separate RPCs, matching the frozen `StartAgentSession` semantics.
- **`AgentFrame` + `AgentControl`** (T5, internal-only). The agent's stdio
  streams need discriminated envelopes; no frozen message covers either
  direction, and neither existing response type carries both content and
  status (see *T5*). Two new internal-only messages, generated only into the
  agent + Runner (not the public client surface, matching the `RunnerService`
  internal-gen posture): **`AgentFrame`** (stdout) — a `oneof frame` split by the
  surface that owns each payload: **conversation** (`MessagePosted`/
  `MessageUpdated` content, reused without redefinition) and **session** (a
  `SessionFrame` opaque OMP-native envelope + `AgentSessionState` for the board);
  and **`AgentControl`** (stdin) — a
  `oneof control { prompt; steer; deliver; ask_answer; config; replay; replay_complete }`
  carrying the control ops + the restart replay barrier. `steer` is free text; an
  `ask_answer` is structured (`ask_id` + `chosen_option_ids`, mirroring
  `RespondToAskRequest`, `comms.proto:481-486`) so a late answer correlates to
  the right in-flight ask across turns — one *delivery* path (stdin control
  frames), two payload shapes. **Delivery timing (amended, round-three — Matt):**
  a frame arriving while the agent is **idle starts a new turn**. Mid-turn, the
  two paths differ: an **`@`-mention-borne `steer`** interjects into the running
  session immediately (the SDK's steer/followUp queuing, see *Agent runtime* and
  *Channel membership: join / subscribe / mention*), while a **plain message from a
  subscribed channel** is delivered immediately as a **`deliver`** frame that the
  agent queues and coalesces into a single `prompt` at its **turn end** (RT-3,
  acked via `AgentFrame.delivery_ack`), not as a mid-turn
  interrupt. An `ask_answer` re-wakes an idle agent that ended its turn to wait —
  the wake rule that makes an async `ask` safe. Only the `@`-mention steer
  interrupts a turn in progress.
- **`snapshot_seq` boundary field** (T2). To make `since_seq = 0` recovery a
  *consistent* point-in-time snapshot, the subscribe response carries a
  `snapshot_seq` and each read RPC (`ListMessages`/`ListChannels`/…) takes it,
  so every page reads one point-in-time view under concurrent writes (see the
  write-through property in *State + storage* and *Resolved decisions*).
  Additive fields on the existing subscribe-response + read-request messages.
- **Unified steer path** (T5/T7, superseded decision 4). A human steer and an
  ask-answer are one thing — injecting a human message into the running agent.
  The frozen contract already routes ask-answers (`RespondToAsk`,
  `comms.proto:86-88`); the amendment routes an `@`-mention in the agent's
  channel through the *same* injection path — a free-text `@`-mention is
  delivered as `AgentControl.steer`, a structured answer as
  `AgentControl.ask_answer` (see the `AgentControl` oneof under *Ratified
  additive contract changes*). **Authz sweep:** `RespondToAsk` is frozen-authored
  to authorize "the caller … a participant of the workspace the ask belongs to"
  (`comms.proto:86-88`) and `ask_id` is "resolved within the caller's authorized
  workspaces" (`comms.proto:278-279`); under channel-borne asks both become
  channel-membership** checks (doc-intent supersession, swept in T2). **Steer RPC
  shape (OQ-B, resolved — Matt):** steer reuses `PostMessage` into the agent's
  channel with server-side `@`-mention routing into the running session — no
  dedicated `SteerAgent` RPC. A steer *is* a channel message; reuse adds no RPC,
  unifies with ask-answer, and keeps the "everything is in the channel"
  property. The `@`-mention is what distinguishes an *immediate* steer from a
  turn-end delivery (see *Channel membership: join / subscribe / mention* below).
- **Channel membership: join / subscribe / mention — the delivery model**
  (T2/T5/T7, superseded decision 4, round-three, Matt). Membership is **tiered**,
  and the tier plus the `@`-mention decides how a message reaches an agent:
  - **Join** — an account with visibility perms (D9) joins a channel and may
    **read** its messages (added to `Channel.member_account_ids`,
    `comms.proto:194`). Read access only; no push.
  - **Subscribe** — a joined account opts in so **new messages are pushed to it**;
    for an agent, a plain (non-`@`) message in a subscribed channel is delivered
    **immediately as an `AgentControl.deliver`, queued by the agent and processed
    at its turn end** (RT-3, see *Agent runtime*), exactly like a message in its
    own channel (never a mid-turn interrupt). An agent is **always subscribed to its own channel**
    (implicit, not a togglable row; "its own channel" = the agent's **home
    channel**, an additive `home_channel_id` on the agent `Account`, minted at
    `CreateAgent` — ratified, see *Resolved decisions*, RT-2).
    **Agent vs user:** for an *agent*, subscribe
    governs turn-end *delivery* (above); for a *user*, it governs only
    notification/unread emphasis (the surface the user's attention tracks — see
    *Agent runtime*, "Reaching the user"), since the `SubscribeComms` fan-out is
    already visibility-scoped (`comms.proto:95-99`, no channel filter) and a
    user's subscribe bit changes nothing on that push path. Subscription is an
    additive per-member boolean on channel membership — the record fixes the
    semantics; T2 fixes the exact carrier (a `subscribed` flag on a membership
    row, or a parallel `subscriber_account_ids`).
  - **`@`-mention → immediate steer** — an `@`-mention of an agent **always
    interjects a mid-turn steer** into that agent (via `AgentControl.steer`),
    for **any agent that is a member of the channel**, regardless of its
    subscribe state and including **shared channels**. This is the one path that
    interrupts a running turn; a plain subscribed-channel message is not.
  - **Reserved group pings** — `@agents` (all agent members), `@users` (all user
    members), `@everyone` (both). Each resolves server-side to the matching
    channel members and steers every agent in that set. `@`-mentions work
    **agent→user and user→user** as well (a user or agent can `@` a user — a
    notification to that user, not a steer, since users aren't turn-driven).
  This supersedes the earlier flat "any control frame arriving mid-turn queues
  into the running session" wording (see the `AgentControl` bullet under
  *Ratified additive contract changes* above): only an `@`-mention-borne steer interrupts
  mid-turn; a plain subscribed message rides the turn-end wake.
- **`UpdateChannelMembers` — the membership-mutation RPC** (T2, RT-1, ratified).
  Join (`+member_account_ids`), the subscribe opt-in toggle, DM→GROUP_DM member
  additions, and fork f's "sharing = adding to the channel" are all membership
  **mutations**, but the frozen `CommsService` set (`comms.proto:38-100`) has no
  RPC for them and fork f removed the only ones (`Share`/`Unshare`) — the same
  gap the record closes for `CreateChannel` (`ChannelChanged` is literally "A
  channel was created, **or its membership changed**", `comms.proto:338`). Add
  **one** additive `rpc UpdateChannelMembers(UpdateChannelMembersRequest) returns
  (UpdateChannelMembersResponse)` to `CommsService` — add/remove members and flip
  the per-member `subscribed` flag — caller-authorized against channel
  visibility, emitting `ChannelChanged`. One RPC covers join, subscribe-toggle,
  DM-expansion, and share-replacement; it is the carrier for the `subscribed`
  flag T2 fixes above. Additive (buf-breaking-safe); lands in T2 via the
  gen/drift cycle.
- **`Ask` becomes async** (T5/T7, superseded decision 4; doc-intent
  supersession). The frozen `Ask` comment "blocks until a participant answers"
  (`comms.proto:271-276`) is superseded: an ask is a normal async channel
  message; the agent chooses at the turn level whether to wait. Comment-only
  change (the `Ask`/`RespondToAsk` shapes are unchanged), landing via the
  gen/drift cycle in T2.
- **Doc-intent comment sweep** (T2, superseded decision 4). Frozen doc-comments
  the inversion falsifies, swept as comment-only supersessions (no shape change):
  `PostMessage` "a human prompt into an agent's workspace" (`comms.proto:82-83`)
  → into an agent's **channel**; `ChannelKind` "An agent's ACP surface is an
  `AgentWorkspace`, not a channel" + "DMs are direct human conversations"
  (`comms.proto:197-198`) → the agent's surface **is** a channel/DM, and
  human↔agent and agent↔agent DMs both exist. **DMs are a channel kind, not a
  separate type** (`ChannelKind.DM`/`GROUP_DM` already exist, `comms.proto:199-203`):
  a two-party DM **expands to a multi-party conversation by adding members**
  (`DM` → `GROUP_DM` as `member_account_ids` grows), so a human↔agent DM widens
  into a multi-agent/multi-user working channel without a type change — one
  membership model across DMs and channels. Beyond these named comments, the
  sweep is a **rule, not a fixed list** — every frozen doc-comment that
  references workspace-as-container, workspace *participants*, or
  workspace-scoped authz is superseded to its channel-membership equivalent in
  T2. Known instances: `OpenAgentWorkspace` "the caller must own the agent or
  already be a participant" (`comms.proto:63-64`) → channel membership (the RPC
  survives, fork e; its authz becomes membership-based); `ListMessages` "a
  channel or an agent workspace the caller may see" (`comms.proto:78-79`) and
  `SearchMessages` "visible channels and workspaces" (`comms.proto:90-93`) →
  channels only; `AgentWorkspaceChanged` "or its participants changed (e.g. a
  share)" (`comms.proto:353`) and `Message` "An agent's ACP turn is a workspace
  message" (`comms.proto:225-228`) → channel-message framing. Completeness is the
  rule's, not this enumeration's.
- **`MessageBlock` narrows to the durable conversation** (T2/T5, superseded
  decision 4). With the execution trace delivered as opaque OMP-native session
  data on a dedicated stream (not comms, not typed `SubscribeEvents` variants),
  the comms `MessageBlock` oneof (`comms.proto:247-262`) narrows from
  text/thought/tool_call/plan/diff/ask to the durable conversation: `text` +
  `ask`. The trace variants (`thought`/`tool_call`/`plan`/`diff`) are removed —
  none is reused by the observation pane (it renders OMP-native frames), so none
  is kept "defined but unused"; `diff`/`plan` are additionally surfaced as PRs.
  Physical removal lands in T2 via the gen/drift cycle (OQ-A, resolved). The
  `Message.container` `workspace_id` is **dropped** (OQ-C, resolved — Matt): the
  container becomes **channel-only** (see *Resolved decisions*, container shape).
- **Dedicated agent-session stream + drop the ACP-translation variants** (T5/T7,
  superseded decision 4, round-two fork e). Add a session-tail RPC on
  `CompassService` — `rpc SubscribeAgentSession(SubscribeAgentSessionRequest) returns (stream AgentSessionFrame)`
  — carrying the agent's OMP-native session events as an **opaque** envelope
  (bytes/JSON) plus the `AgentSessionState`, consumed by OMP's own renderer in
  the observation pane and scoped to the caller's channel membership. The public
  `AgentSessionFrame` is the Server→Client repackaging of the internal
  agent-stdout `AgentFrame.session` `SessionFrame` (T5) — same opaque payload,
  relayed verbatim. In the same change, **remove** the three ACP-translation
  variants `agent_message_chunk` / `agent_tool_call` / `agent_plan` from
  `SubscribeEventsResponse.payload` (`compass.proto:103-105`) — under the
  first-party OMP agent they are neither the native render format nor needed;
  `SubscribeEvents` keeps `ServerStatus`, `ResyncRequired`, `AgentSessionStatus`,
  and the T8 board. This one removal is **buf-breaking, not additive** —
  permitted here only because the Server on `main` is ephemeral (no live client)
  and none of the three dropped variants was ever published (the only production
  Publish sites — `ServerStatus` at `serve.go:207`, `ResyncRequired` at
  `service.go:151` — are both kept variants; one **test** fixture,
  `server/service_test.go`'s `chunkEvent`, constructs `AgentMessageChunk` and
  migrates to a kept variant in the same change); it lands in T5/T7 via the
  gen/drift cycle.
- **`parent_message_id` on `Message`** (T2, superseded decision 4, round-two
  fork d). The primary surface is a threaded channel conversation, but `Message`
  (`comms.proto:229-241`) has no thread/parent field. Add an additive optional
  `parent_message_id` so a reply threads under its parent; a root message leaves
  it unset. Additive (buf-breaking-safe); lands in T2 with the message schema.

The first four are the original v0.6 additive set. The rest ride the
interaction-surface inversion (superseded decision 4): the async-`ask`,
doc-intent, and unified-steer changes are additive/comment-only; the
`parent_message_id` field, the session-tail RPC, and the channel-membership
carriers (the per-member `subscribed` flag + reserved-ping resolution) are
additive; the observation-pane ACL collapse (fork f) **removes**
`AgentWorkspace.participant_user_ids` and the `ShareAgentWorkspace` /
`UnshareAgentWorkspace` RPCs (`comms.proto:67-76,207-221`), the session-stream
change **removes** the three ACP-translation variants, and the container
narrowing (OQ-C) **removes** `Message.container.workspace_id` and its
co-narrowing read-request fields — **three** buf-breaking removals in all, the
only buf-breaking items, safe pre-launch (ephemeral Server, no live client). All
land in their implementing task's PR via the gen/drift cycle (see *Resolved
shape questions* and *Resolved decisions*).

### Agent runtime: a first-party agent emitting `compass.v1`

**Superseded here (Matt's decision, July 2026): the ACP/BYOA agent model of
SEA-1023** (`../../agents/sea-1023-acp-session.md` — Fork 1 "adopt
`agent-client-protocol`" `:39-44`, Fork 2 ACP-over-stdio `:46-52`, Fork 4 the
ACP→`compass.v1` translation seam `:64-68`, and the "BYOA over ACP …
default/reference agent" constraint `:19`). The in-container agent is a
**first-party program built on the OMP SDK**, and it emits `compass.v1`
natively — there is no ACP anywhere in the system.

- **The agent is ours, on the OMP SDK.** It is built on
  `@oh-my-pi/pi-agent-core` — the stateful agent loop ("General-purpose agent
  with transport abstraction, state management, and attachment support"; MIT;
  `engines.bun >=1.3.14` — `github.com/can1357/oh-my-pi`,
  `packages/agent/package.json`): the `Agent` class with `prompt`/`continue`,
  `steer`/`followUp` queuing, a subscribable event stream
  (`agent_start`/`message_update`/`tool_execution_*`/…), and programmatic
  state control — the runtime mutators `setTools`, `setSystemPrompt`,
  `setModel`, plus custom message types via declaration merging (all
  documented in `packages/agent/README.md`; `getToolContext` is a
  construction-time `AgentOptions` field, `packages/agent/src/agent.ts`
  `AgentOptions`, not a runtime mutator — the live tool/prompt surface is the
  three `set*` methods). The coding tool surface composes
  from `@oh-my-pi/pi-coding-agent` ("Coding agent CLI with read, bash, edit,
  write tools and session management", MIT,
  `packages/coding-agent/package.json`).
- **It speaks `compass.v1` natively; the translation seam is deleted.** The
  agent maps the SDK's event stream to `compass.v1` payloads **inside the agent
  itself**, its own testable surface. Under the inversion the mapping splits by
  surface (see *Conversation-vs-trace publication* below): the durable
  **conversation** maps `text`→`MessageBlock.text` and the `ask` tool→
  `MessageBlock.ask` (the two surviving block kinds); the **execution trace** —
  assistant `thought`, `tool_execution_*`, `todo` (plan), `edit`/`write` (diff)
  — is **not** re-typed into `compass.v1`, but wrapped **verbatim** as opaque
  OMP-native session frames (`SessionFrame`) for the observation pane, with
  `diff`/`plan` additionally surfaced as PRs. There is no ACP `session/update`
  and no Runner-side translator (SEA-1023 Fork 4 superseded).
- **Reaching the user is a channel reply, not session output (agent-behavior
  contract, round-three — Matt).** An agent's ordinary output streams to the
  **session log** — the observation pane the user watches *sometimes*, not
  continuously. To actually **reach** the user, the agent must post a **channel
  message** (a `text` reply or an `ask`); that is the surface the user's
  attention (unread/mention/thread signals) tracks. And it **should reply within
  a thread** (`parent_message_id`) whenever it is responding to something, so a
  channel's conversations stay logically grouped rather than flattened into one
  stream. This is the behavioral half of the surface split: the session log is
  *observation* (what the agent is doing); the channel is *communication* (what
  the agent needs the user to see). Skills/prompt guidance for the first-party
  agent encode this norm; the contract just makes the two surfaces distinct.
- **Transport across the container boundary is unchanged: the built streaming
  exec.** `ExecStreaming` "starts a long-lived streaming command in a running
  container, returning its live stdio pipes plus a kill/wait handle"
  (`go/internal/runtime/podman.go:289-296`; implementation
  `:417-422`). The pipes now carry the agent's **newline-framed `compass.v1`
  stream**, not ACP JSON-RPC — a payload-schema change only; the mechanism is
  schema-agnostic. (The built code's comments still say "ACP"
  (`podman.go:140-144,289-296`) — pre-cutover doc-intent, updated as T5
  touches those files.)
- **The Runner launches and relays; it does not translate.** The Runner
  starts the container through the built lifecycle façade ("build the image,
  create and start the container, arm the egress firewall … and tear it all
  down", `go/internal/runtime/agent.go:1-5`), spawns the
  first-party agent via the streaming exec, and relays its `compass.v1`
  stream up the Server↔Runner seam — the `PublishEvents` client-stream the
  frozen platform record fixes
  (`../../platform/go-toolchain-default.md:931-937`). No ACP client, no
  translator on the Runner.
- **One container = one agent = one session** — carried from SEA-1023
  (`../../agents/sea-1023-acp-session.md:66`); the agent works multiple
  branches via worktrees inside its clone, not via extra sessions.
- **Placement and sequencing (conforms to the frozen OQ6 ruling).** The
  streaming exec originates on the Runner; the Runner relays events to the
  Server over `PublishEvents`. That link stream is **Runner-sequenced** —
  "seq assigned at the Runner, not at Server publish" — "so in-transit loss
  is *detectable* as a gap and the Client bus's gap-free guarantee holds"
  (`../../platform/go-toolchain-default.md:1390-1392`). The Server then
  write-throughs each event and publishes it on its own bus, which assigns
  the **bus-seq** clients see (`Publish` "stamps the next seq",
  `go/events/events.go:160-165`). Link-seq (Runner→Server gap
  detection) and bus-seq (Server→Client replay cursor) are distinct sequence
  spaces. Lifecycle RPCs (`StartAgentSession` …) flow Client → Server →
  Runner.
- **Conversation-vs-trace publication is a specified split, not a fork**
  (superseded decision 4). The agent's durable **conversation** — a `text`
  reply or an `ask` — becomes channel `Message`/`MessageBlock` rows and fans out
  on the comms surface (`SubscribeComms` `MessagePosted`/`MessageUpdated`,
  `comms.proto:326-336`); the agent's **execution trace** (assistant/thought
  chunks, tool calls, plans, diffs) is OMP-native session data carried on a
  **dedicated session-tail stream** (opaque, rendered by OMP's own renderer),
  while its extracted lifecycle (`AgentSessionStatus`) and the board projection
  ride `SubscribeEvents` alongside `ServerStatus` (`compass.proto:102,97`).
  Complementary, not duplicative: a conversation event writes one Postgres row
  and fans out on comms; a trace event carries live on the session stream and
  persists only to the S3 session (never a comms row). This **retires** the
  frozen comms spine's staging note that "the ACP-as-native-UI payloads move
  onto the workspace/channel surface rather than being redefined in parallel"
  (`comms.proto:15-18`): under v0.6 the trace is neither a comms `MessageBlock`
  variant nor a typed `SubscribeEvents` variant — the three ACP-translation
  variants `agent_message_chunk`/`agent_tool_call`/`agent_plan` are **dropped**
  (they were never published, `serve.go:206`), and the durable comms surface
  narrows to `text` + `ask`.
- **Richer control is the point (Matt's stated motive).** Because the agent
  is ours on the SDK, server-pulled skills, config, and the tool surface
  inject **programmatically** into the running agent's state — `setTools`,
  `setSystemPrompt`, `setModel`, custom message types — rather than
  only as an opaque file mount. Config distribution (T6) keeps the D11
  read-only mount for file-shaped config and adds this structured-injection
  path for skills/tools.
- **Doc-intent supersession, not a wire change:** the frozen proto comment
  "bring the agent in an already-launched container online as an ACP session"
  (`proto/compass/v1/compass.proto:195-196`) now reads "…online
  as a compass agent session" — the RPC shape (takes `container_name`,
  returns `session_id`) is untouched; only what it drives changes.
- **BYOA is deferred, an unbuilt seam note.** A future third-party agent
  could be re-admitted behind the same streaming-exec transport by emitting
  `compass.v1` itself (or by re-adding a translator process); the MVP ships
  exactly one first-party agent and carries no ACP/BYOA machinery. This is a
  deferral note, not a plan task.

### Per-agent container isolation on the Runner

The container model is v0.3's, kept verbatim and hosted by the Runner
(`../compass.md:141-149`):

- **The container is the unit of isolation**: rootless podman, "a scoped
  `$HOME` for that agent's credentials, its own process namespace, a
  default-deny egress allowlist at the container layer"
  (`../compass.md:143`).
- **Clone-per-container**: each agent gets its own full git clone per in-scope
  repo, worktree-per-workstream inside it (`../compass.md:145`).
- **Scoped credentials**: each container carries only its own credentials in
  the agent's `$HOME`, belonging to the agent's own forge identity
  (`../compass.md:149`); forge identity is a configurable per-agent setting
  (first-class Forgejo provisioning; per-seat forges select an account,
  `../compass-0.5/design.md:380-389`).

The Go implementation of this layer exists under
`go/internal/runtime/`: the lifecycle façade "build the image,
create and start the container, arm the egress firewall as root, install
scoped credentials, clone the repo as the unprivileged agent user, and tear it
all down" (`go/internal/runtime/agent.go:1-5`), with `AgentSpec`
carrying workspace, egress policy, and read-only mounts (`agent.go:30-43`).
The Runner binary wraps this package; the Server never touches a container
engine. Because state lives in the Server, **runners and containers are
throwaway** (`../compass-0.5/design.md:309-323`, D6): stop is free, restart or
relocation replays the transcript into a fresh container.

### State + storage: Postgres is the substrate; the ring is a cache

**Postgres is THE durable substrate.** It is the store of record for all
structured state: accounts, channels, channel groups, agent workspaces,
**conversation** messages (channel text + asks), agent config, board/issue
state, issued-token hashes, and the transcript index/metadata. This carries
v0.5 D12's store-of-record ruling forward (`../compass-0.5/design.md:466-471`:
"The Server's datastore is **PostgreSQL**, and it is the **system of record**
for all structured state") and closes the present gap: on `main`, the Go module
has no database dependency at all and no comms persistence — the only stateful
component the serve loop constructs is the in-memory bus
(`go/server/serve.go:138-140`) — so the Postgres store is built
as a first-class early task (see *Plan*), not discovered as a late swap. No
component may treat a message bus as the store of record.

**The execution trace is not a comms `Message` — it is observation-only**
(superseded decision 4). The agent's assistant/thought chunks, tool calls,
plans, and diffs are *not* persisted as comms `Message` rows in Postgres. They
are OMP-native session data delivered live on the **dedicated session-tail
stream** (not `SubscribeEvents`, which keeps only Compass's own projections —
`ServerStatus`, `ResyncRequired`, `AgentSessionStatus`, the T8 board) for the
observation pane, and their durable form is the **S3 session transcript** (below)
— the same object-storage posture transcript *bodies* already take, extended to
the whole trace. Postgres holds only the trace's **index/metadata** (session id,
container, timing, blob keys), never the per-chunk/per-tool-call stream. The
durable *conversation* (channel messages +
asks) and the **PR trail** are the searchable record (D1, narrowed above);
the trace is *replayed* from S3, not searched. The volume this keeps out of the
relational store is real, but the driver is **write/fan-out amplification**, not
body bytes: the frozen event contract has every streaming update "carry the full
current block set" (`comms.proto:331-333`), so under the pre-inversion model each
per-token chunk re-wrote and re-fanned an `O(turn-length)` `MessageUpdated` row.
(Per-call tool *output* was never Postgres-bound regardless — the frozen
`AgentToolCall` has no output field, only `session_id`/`tool_call_id`/`title`/
`status`, `compass.proto:158-166`; the search the inversion gives up is thought
text and diff old/new text, since tool calls were title-only-searchable already.)
A middle option — a small `trace_events` **index** table (session id, seq, kind,
title, blob offset) with full-text search over just those title/summary fields,
bodies staying in S3 — was weighed and **rejected for the MVP**: "search what was
*said*, replay what was *done*" is the intended product stance, and the index is
an additive post-MVP capability if targeted trace search is later wanted.

**Transcript bodies go to S3-compatible object storage behind a blob seam** —
kept exactly as v0.5 D12 fixed it (`../compass-0.5/design.md:473-494`): bodies
are large and append-heavy, so they live keyed in object storage, indexed from
Postgres; hosted deployments use R2; the self-hosted default is SeaweedFS
(Apache-2.0); any S3-compatible backend drops in behind the seam. Old
transcripts are retained, not dropped. Full transcripts are held server-side
and streamed into a container on (re)start
(`../compass-0.5/design.md:130-136`, D6). **Replay completeness:** because the
trace is the only thing that persists to S3 while the conversation persists to
Postgres, the S3 session must be written as **one ordered append log of the
whole session context** — trace frames *and* the durable conversation (channel
`text`/`ask`) *and* inbound `AgentControl` (`prompt`/`steer`/`deliver`/`ask_answer`) — so a
restart replays a complete context in one sequence. Appending only trace frames
would restore the agent's thinking without the human's prompts, steers, and
answers or the agent's own replies; the alternative (merge S3 with Postgres
`Message` rows at replay) is rejected because link-seq, bus-seq, and message
timestamps are three distinct orderings (see *The communication layer*) with no
defined interleave. The S3 log is authored in the agent's own emission order.

**Live fan-out is the in-memory event bus, already built.** The bus is "a
monotonic-seq ring buffer plus a per-subscriber live tail, generic over the
payload it carries … one structure backs the server's SubscribeEvents and
(with the networked Server tier) the comms SubscribeComms"
(`go/events/events.go:1-7`). Its properties are exactly the
event-channel contract:

- `ringCapacity = 1024` bounds the replay window
  (`events.go:22-24`: "ringCapacity bounds replay memory; a subscriber that
  falls further behind than this recovers by re-snapshotting at
  sinceSeq = 0").
- A per-boot random `instance_epoch` distinguishes restarts
  (`events.go:339-346`: "epochNonce mints a per-boot instance epoch: a random
  uint64 from the OS … A reconnecting client echoes it back so the server can
  tell a live cursor from a prior instance's").
- Overrun and shutdown are distinguished per subscriber
  (`events.go:9-12`), so a lagged client gets a terminal `ResyncRequired`
  rather than a silent gap.

**The write path is write-through, Postgres first:** a message arrives → the
Server commits it to Postgres (durable, canonical, indexed) → publishes the
corresponding event on the in-memory bus (live fan-out to every subscribed
stream). Within the bus's retained window a reconnecting client replays from
the ring; beyond it, or across a restart (fresh `instance_epoch` ⇒
`ResyncRequired`), recovery is a **Postgres re-snapshot through the read
RPCs**. Four properties of this shape are load-bearing:

- **The ring is a cache; `since_seq = 0` no longer means whole state.** The
  bus's own doc-comment describes "snapshot the ring at sinceSeq = 0"
  (`go/events/events.go:5-7`) — in the ephemeral daemon the ring
  *was* the whole state, so that snapshot was total. With Postgres in front,
  the ring holds at most the last `ringCapacity = 1024` events
  (`events.go:22-24`), so the frozen field comment "0: snapshot current state
  as events, then tail" (`comms.proto:507-508`, `compass.proto:66-67`) is
  **refined, not broken**: the state snapshot comes from the Postgres read
  RPCs; the stream's `since_seq = 0` yields the ring window plus the live
  tail. The client protocol produces a **consistent point-in-time snapshot**
  via a snapshot-boundary token (Matt's ratified decision, see *Resolved
  decisions*): subscribe (`since_seq = 0`) → the subscribe response carries a
  `snapshot_seq` boundary + `instance_epoch` → snapshot state via the read RPCs
  **each passing that `snapshot_seq`**, so every page reads the same
  point-in-time view (no row crosses a page boundary under concurrent writes)
  → then the live stream tails from exactly `snapshot_seq + 1`. Message-id
  dedup (`comms.proto:229-230`) covers the boundary overlap. This also fixes
  the empty-ring bootstrap: `snapshot_seq` comes from the subscribe response,
  not from a first ring event that may not exist. The server's obligation:
  a consistent snapshot at `snapshot_seq`, gap-free tail after it, terminal
  resync otherwise. **Safe as a pre-launch change, not a break:** the Server on
  `main` is fully ephemeral and no client consumes the old whole-state
  semantics yet (the only `since_seq` references are generated types + shape
  tests, not a live subscriber). The now-inaccurate frozen field comments
  ("0: snapshot current state as events", `comms.proto:507-508`,
  `compass.proto:66-67`) are corrected to the boundary-token protocol as part
  of T2 — a doc-intent supersession of the comment (same class as
  `StartAgentSession`'s "as an ACP session"), landing via the gen/drift cycle.
- **Crash between commit and publish is safe — because epoch resync is
  total.** `Bus.Publish` cannot fail (no error return; a mutex plus
  non-blocking sends, `events.go:160-204`), so the real failure mode is a
  Server crash after the Postgres commit but before `Publish`: the row is
  durable, no live event fired. The crash also drops every stream, and every
  reconnect then sees a **fresh per-boot `instance_epoch`**
  (`events.go:151-152`) ⇒ `ResyncRequired` ⇒ a full Postgres re-snapshot,
  which surfaces the committed row. This recovery works **only** because the
  epoch mismatch forces a *total* resync — the dependency is explicit, and
  no partial-resync optimization may weaken it.
- **Bus order may differ from commit order.** Two handlers can commit A then
  B but publish B then A — the bus serializes publishes, not transactions.
  The message-id dedup above plus Postgres-as-truth-on-resync absorbs it;
  consumers must not treat bus arrival order as commit order.
- **The ring is never a store.** No component may treat the bus as the store
  of record (*Global Constraints*).

**The one seam kept: event fan-out at Runner↔Server — executed by the
platform record.** The Runner delivers agent events to the Server through a
narrow interface: the `PublishEvents` client-stream of the internal
`RunnerService`, whose three-RPC shape the frozen platform record fixes
(`../../platform/go-toolchain-default.md:931-937`; "the Server↔Runner split …
is realized *here*, by T8 + T9", `:363-365`). v0.6 records the product-level
architecture and **defers the seam's wire shape to that frozen record** — T4
below consumes it, it does not re-specify it. That consumption interface —
not the storage layer — is the sole place a broker could later slot in (see
*Alternatives considered* and *Resolved decisions*). Storage has no seam:
Postgres is not swappable, and multi-Runner topologies still converge on the
same database through the same Server.

**Secrets**: the centralized config store enforces the defined
secret-handling boundary of v0.5 D14 (`../compass-0.5/design.md:528-546`):
encryption at rest, per-user/per-agent authorization, per-container isolation
of delivered secrets, rotation/revocation, and redaction from transcripts and
the audit log.

### Config distribution: Runner-mediated pull, read-only mounts, OCI images

Carried from v0.5 D11 (`../compass-0.5/design.md:430-459`), plus a
programmatic-injection path the first-party agent enables:

- The Server is the config store of record, holding each agent's config
  versioned (content-addressed per agent).
- The Runner pulls a hosted agent's config bundle over its existing gRPC
  connection and materializes it as a **Runner-local read-only bind mount**
  into the container — no cross-host network filesystem. The mount surface is
  already in the Go runtime (`AgentSpec.Mounts` — "read-only host mounts",
  `go/internal/runtime/agent.go:41-42`).
- Change propagation: the Server signals "config version N for agent X" over
  the `Sessions` bidi stream (the Server→Runner path; agent events flow the
  other way on `PublishEvents`); the Runner pulls, atomically swaps the mount,
  and the agent's notification path tells the running process to re-read.
- **New under the cutover — programmatic injection:** for skills and the tool
  surface, the pull is not just a file mount: the Server-pulled bundle is
  injected as **structured state into the running first-party agent** over
  its control stream — the SDK's `setTools`/`setSystemPrompt`/`setModel`
  surface (`packages/agent/README.md`, `github.com/can1357/oh-my-pi`). The
  read-only mount (D11) stays for file-shaped config; injection covers what a
  mount cannot: swapping the live tool set and system prompt without an
  agent restart (see T6).
- The agent binary and base image ride **versioned OCI pulls**, applied via
  the throwaway-container restart: stop, restart on the new image, transcript
  replayed.

### Auth + the network door

The Server's network door carries the v0.5 Server-tier design forward
(`../compass-0.5-server/design.md`), re-expressed on the Go stack:

- **TLS listener, operator-provisioned certs.** The network door is TLS-only
  (`--listen` + PEM cert/key paths); a bearer token over cleartext is
  credential disclosure, so there is no plaintext network listener. The Go
  serve loop already carries the config seam: "TLSConfig carries
  operator-provisioned PEM paths for the authenticated TCP door … nil on the
  socket-only shipped path" (`go/server/serve.go:34-36`). The
  local Unix-socket door (0600, owner-only) remains the local trust boundary
  (`serve.go:55-62`).
- **Client auth is a per-user bearer token** resolved to an account:
  32 random bytes, base64url, shown once; the server stores only the SHA-256
  hash (`proto/compass/v1/compass.proto:47-49`). Token hashes are
  rows in Postgres, so tokens survive restarts and a future second Server
  process shares them. `IssueToken` is admin-gated
  (`compass.proto:45-46`: "only an admin may mint a token for an account; a
  non-admin caller gets permission_denied"); the bootstrap admin token is
  issued out-of-band at first start (`compass.proto:50-51`) and written 0600
  under the server state dir, never logged.
- **Runner auth is one provisioned token per Runner**, enrolled once —
  deliberately not per-agent tokens, since the Runner, not the Server, is the
  trust boundary for the containers it hosts (`../compass-0.5/design.md:407-413`:
  "One durable token per Runner, enrolled once, is the model"). The mechanism
  is the frozen OQ7 ruling
  (`../../platform/go-toolchain-default.md:1410-1423`): a **dedicated
  Runner-subject mint path** (not the Client-door `IssueToken`), reusing the
  hash-only token store under a **distinct subject-prefix keyspace**; the
  credential stored `0600` on the Runner host; delivery is an **operator
  provisioning step, not an automated RPC**. Enforcement is at the auth
  interceptor: it resolves the hash to the typed `Subject` (T1,
  `ResolveTokenHash → Subject{Kind, ID}`) and **rejects on `Kind` before
  projecting to an identity** — a `Kind == Runner` subject is
  `CodeUnauthenticated` on `CompassService`/`CommsService`, a `Kind == Account`
  subject is rejected on `RunnerService` — so the kind is never discarded into
  a bare `AccountID` that would make the two token classes indistinguishable
  (see T4). Both cross-door rejections are mandatory tests.
- **Transport-mode selection stays at client construction**: local vs. remote
  is only which factory the caller invokes; no mode enum, no
  local-assumption leaks above the transport seam
  (`../compass-tauri-shell.md:107-121`, `../compass-0.5-server/design.md`, F3).

### MVP scope

Carried from v0.5, unchanged:

- **Multi-agent orchestration is IN** (`../compass-0.5/design.md:257-286`,
  D4): a supervisor agent account coordinating worker agent accounts over
  channels, with the Bridge board as the Server-side projection of workstream
  state. MVP depth per D13 (`../compass-0.5/design.md:500-526`): task
  assignment ships; conflict map and auto-assignment layer on later. A single
  agent flowing end to end through the tiers is the first build increment,
  not the MVP ceiling.
- **Browser-first; Tauri deferred** (`../compass-0.5/design.md:330-344`, D7).
  The Runner binary and its Server connection are still built.
- **Warden deferred** (`../compass-0.5/design.md:346-358`, D8); the per-agent
  container remains the structural sandbox in the interim.

### Transport: gRPC everywhere

One transport technology on every network hop, carrying v0.5 D10 forward
(`../compass-0.5/design.md:401-426`); the container-boundary hop is the
stdio pipe pair the same events ride before they reach the network:

- **Client↔Server**: `compass.v1` over Connect — native gRPC (HTTP/2),
  gRPC-Web, and Connect off one handler (`go/server/serve.go:3-12`).
  The browser client forces gRPC-Web on this hop regardless of any other
  transport choice.
- **Runner↔Server**: gRPC, authenticated by the per-Runner token — an
  outbound connection from the Runner (NAT-friendly, like a CI runner). The
  wire shape is the frozen internal `RunnerService`
  (`../../platform/go-toolchain-default.md:931-937`): container lifecycle
  commands **and** Server→Runner config-version signals ride the `Sessions`
  bidi stream (the only Server→Runner path); agent events ride
  `PublishEvents` (Runner→Server only) (see T4).
- **Container↔Runner**: the first-party agent's newline-framed `compass.v1`
  stream over the built streaming-exec stdio pipes
  (`go/internal/runtime/podman.go:289-296`); the Runner relays
  it up the `RunnerService` stream to the Server, which fans it out to
  Clients (`SubscribeEvents`/`SubscribeComms`).

A single transport keeps one contract-generation + CI-drift discipline
(`../compass-0.5/design.md:423-425`). Whether a broker ever augments the
Runner↔Server hop is resolved below (*Resolved decisions*): not for the MVP.

## Alternatives considered

### NATS as a transport (Server-internal, Runner↔Server, Client↔Server)

NATS/JetStream was evaluated at each of the three places it could sit. Two of
the three are settled by this record; the third is resolved in *Resolved
decisions* (stay gRPC for the MVP).

**Server-internal broker: rejected (settled).** A broker the Server publishes
to and subscribes from inside its own process boundary is a self-loop: it
adds a second durable copy of data Postgres already holds authoritatively,
plus an operational dependency every self-hoster must run, and buys nothing
the in-memory bus does not already provide. JetStream is a log, not a query
store — a comms product needs indexed, transactional queries (message pages,
full-text search scoped to a caller's visible set, account/ownership joins),
which is what Postgres is for; once Postgres holds the message durably, a
broker's copy is redundant and non-authoritative. The live-fan-out role is
already built and proven in-process (`go/events/events.go:1-13`),
with bounded replay and restart-resync semantics that match the wire
contract exactly (`comms.proto:300-324`). This supersedes the v0.5 D1/D12
internal-broker framing by citation (see *Superseded decisions*).

**Client↔Server: rejected (settled).** The MVP Client is a browser
(`../compass-0.5/design.md:330-336`, D7), and a browser cannot speak native
NATS — the hop is gRPC-Web/Connect over fetch no matter what
(`go/server/serve.go:3-12`). A NATS Client hop would therefore
add a second wire technology *without removing the first*. Clients stay
gRPC unconditionally.

**Runner↔Server event transport: a real trade, deferred post-MVP (the Open
Question).** Runners are the one place a broker could earn its keep:

*For NATS here:*

- **Durable reconnect-replay for free.** JetStream durable consumers resume
  at the last-acked sequence, so a flaky, remote Runner that comes and goes
  ("like a CI runner", `../compass-0.5/design.md:89-95`) reconnects without
  hand-rolled cursor bookkeeping. (Weakened by the frozen OQ6 ruling: the
  link stream is Runner-sequenced with reattach reconciliation
  (`../../platform/go-toolchain-default.md:1388-1396`), so the Server tracks
  per-Runner link cursors anyway — the broker would remove code that must
  exist regardless.)
- **Decoupled subject addressing.** Runners and Server address subjects, not
  live connections — no N-connection stream matrix to manage as Runner count
  grows.
- **Multi-consumer fan-out** of one event stream (e.g. a second Server
  instance or an audit tap reading the same Runner events).
- **WAN backpressure** handled by the broker rather than hand-rolled on a
  gRPC stream.

*Against NATS here:*

- **A second wire technology** with a second versioning/contract story
  alongside the proto/gRPC discipline — today one schema, one codegen, one
  drift gate covers every hop (`go/moon.yml:39-42`).
- **Mandatory infrastructure for every self-hoster.** Self-hostable is a hard
  product constraint (`../compass-0.5/design.md:489-494`); a broker in the
  Runner path puts NATS in every deployment, not just large ones.
- **A second auth story.** NATS auth (nkeys/JWT) must be mapped onto Compass
  accounts, versus the one-bearer-token-per-Runner model already decided
  (`../compass-0.5/design.md:407-413`).
- **It cannot unify the stack.** The Client hop is gRPC-Web regardless (see
  above), so NATS would only ever be an *additional* transport, never a
  replacement.

*The cheaper multi-Server option first: Postgres LISTEN/NOTIFY.* If the
trigger that would justify a broker is a **second Server instance** (each
needing the write-through fanned onto its local bus), the store of record
already ships pub/sub: the committing Server `NOTIFY`s on commit and every
Server's listener republishes onto its local in-memory bus — zero new
infrastructure, one connection per Server, and consistent with this record's
thesis that no second system should do what Postgres already covers.
(Logical decoding is the heavier variant of the same idea.) NATS should be
weighed only after LISTEN/NOTIFY is shown insufficient (payload limits,
cross-DC fan-out, Runner-side consumption).

*Recommendation:* **gRPC everywhere for the MVP.** The against-column costs
are paid by every deployment immediately; the for-column benefits matter only
past a concrete scale trigger — more than one Server instance, or a Runner
count high enough that per-connection cursor bookkeeping measurably hurts —
and the first of those has the LISTEN/NOTIFY answer above before it has a
broker. Committing to NATS now would also **reopen the frozen platform
record**: SEA-1243 T8 fixes the gRPC `RunnerService` shape ("This shape is
fixed here", `../../platform/go-toolchain-default.md:935-937`), so a broker at
this hop is a supersession of that record, not a v0.6-local choice. Until a
trigger fires, the narrow event-fan-out seam at the Runner↔Server hop (the
`PublishEvents` consumption interface, task T4) keeps the option open: a
JetStream-backed implementation can slot in behind it later without
re-architecting storage, contract, or auth. The decision itself is recorded
in *Resolved decisions* (stay gRPC for the MVP; re-evaluate at a trigger).

## Global Constraints

Every task below inherits these; task briefs do not restate them.

- **Languages: Go on the backend tiers, TypeScript on the agent + client.**
  The Server and Runner are the Go module `go/**` (module
  `github.com/RigelBuild/compass/go`), gated by the full Go battery
  `moon run compass-go:ci` — gofmt, go vet, golangci-lint (exhaustiveness
  on), `-race` tests, build, govulncheck, go-licenses
  (`go/moon.yml:141-147`). The in-container agent is
  **TypeScript on Bun** — the OMP SDK's language (`@oh-my-pi/pi-agent-core`,
  `engines.bun >=1.3.14`, `packages/agent/package.json`) — joining the
  already-TS SolidJS Client; it lives at `packages/compass-agent`
  mirroring the existing `packages/compass-client` TS layout. This does not
  reopen SEA-1243's Rust→Go ruling for Server/Runner
  (`../../platform/go-toolchain-default.md:16-17`): the agent tier is TS by
  necessity of the SDK — a **first-party in-container agent tier** SEA-1243's
  boundary table never modeled (it placed OMP outside the boundary as an
  external Rust peer, `:88` — the row this record moots, see *Superseded
  decisions* #3).
- **Self-hostable is a hard product constraint.** No mandatory hosted-only or
  copyleft-encumbered dependency on the default self-host path; the blob-store
  self-hosted default is SeaweedFS, Apache-2.0
  (`../compass-0.5/design.md:479-494`).
- **Store of record = Postgres.** Transcript bodies = S3-compatible object
  storage behind the blob seam. No component may treat a message bus as the
  store of record; the in-memory bus ring is a cache, never a store.
- **Contract discipline.** `compass.v1` protos
  (`proto/compass/v1/`) are generated and CI drift-gated
  (`go/moon.yml:34-42`); a contract change means edit schema →
  regenerate → commit output. Generated clients are the only sanctioned door
  (`proto/compass/v1/compass.proto:1-4`).
- **Caller identity is connection-bound**, never a request field
  (`proto/compass/v1/comms.proto:31-37`); every RPC is
  authorized server-side against the authenticated account's visible set.
- **No plaintext network listener.** The network door is TLS + bearer token;
  the 0600 Unix socket remains the local trust boundary
  (`go/server/serve.go:34-39,55-62`).
- **Rootless podman, no rootful fallback**, for every container the Runner
  starts (`go/internal/runtime/podman.go:24-25`).
- **First-party agent, `compass.v1`-native.** The in-container agent is the
  first-party OMP-SDK program; it emits `compass.v1` directly. No ACP, no
  BYOA machinery in the MVP (supersedes
  `../../agents/sea-1023-acp-session.md:19` — see *Agent runtime*).
- **Frozen-platform conformance at the Runner seam.** The Server↔Runner wire
  shape, sequencing, failure matrix, and token model are the frozen platform
  record's rulings (`../../platform/go-toolchain-default.md:931-937,1378-1423`);
  tasks conform to them and never re-decide them.
- **Frozen-record convention.** This record freezes on merge; later changes
  supersede by citation, never rewrite
  (`../compass-0.5/design.md:10-12`).
- **markdownlint-clean** under the repo config (`.markdownlint.json` /
  `.markdownlint-cli2.jsonc`).

## Plan

Sequencing principle: **the Postgres store lands early** — it is the
durability story (the Server on `main` is fully ephemeral, see *Problem /
Intent*), and every later tier (Runner enrollment, tokens, transcripts,
config) writes into it. The comms vertical comes next so the spine is real
end to end on one machine; the Runner split, the UI pivot, and orchestration
layer on top. Each task carries its own test cycle and lands as its own PR
(or short PR stack).

### T1 — Postgres store of record

Stand up the Server's Postgres layer and put accounts, channel groups,
channels, agent workspaces, messages, and token hashes in it from day one.
Schema migrations are versioned and applied at Server start (embedded
migration files; refusing to serve on a failed migration). Search lands here
as Postgres full-text over message text blocks — the audit/search property is
served from the store of record (`comms.proto:90-93`), not a separate engine.

`Interfaces:`

- Package `go/internal/store`, consumed by the comms service and
  the auth layer:
  - `func Open(ctx context.Context, dsn string) (*Store, error)` — connects
    (pgx pool), runs migrations, verifies schema version.
  - `func (s *Store) Close()`
  - Accounts: `CreateUser(ctx, u UserAccount) (Account, error)`,
    `CreateAgent(ctx, a AgentAccount) (Account, error)`,
    `GetAccount(ctx, id AccountID) (Account, error)`,
    `ListAccounts(ctx, visibleTo AccountID) ([]Account, error)`.
  - Channels/groups/workspaces: `CreateChannelGroup`, `CreateChannel`,
    `UpdateChannelMembers` (RT-1: add/remove members + flip the per-member
    `subscribed` flag — the one membership-mutation method behind the RT-1 RPC),
    `ListChannelGroups`, `ListChannels`, `OpenAgentWorkspace` — same
    `(ctx, …, visibleTo/actor AccountID)` shape, mirroring the post-fork-f
    `CommsService` RPC set (`comms.proto:38-100`). `CreateAgent` sets the agent's
    `home_channel_id` (RT-2) at creation. There is no `SetWorkspaceParticipants`
    — `participant_user_ids` and the share/unshare RPCs are removed under
    superseded decision 4, so the observation pane has no per-workspace
    participant list to set; channel membership (above) is the one ACL.
  - Messages: `AppendMessage(ctx, m Message) (Message, error)` (assigns the
    row id + timestamp), `UpdateMessageBlocks(ctx, id MessageID, blocks
    []MessageBlock) error` (streaming-turn updates),
    `ListMessages(ctx, container ContainerRef, page Page) ([]Message, error)`
    (newest-first, clamped page size),
    `SearchMessages(ctx, actor AccountID, scope SearchScope, query string, page Page)
    ([]Message, error)` (visibility-scoped full text; `scope` optionally
    narrows to one channel, mirroring `SearchMessagesRequest`'s scope field,
    else the actor's whole visible set).
    (`ContainerRef` and the `scope` narrowing are **channel-only** — OQ-C
    resolved (Matt): the workspace is not a message container and
    `workspace_id` is dropped from the container + read-request scope under
    superseded decision 4; see *Resolved shape questions*.)
  - Tokens (subject-typed, so a Runner subject and an account subject share
    the store but never collide — the OQ7 prefix-separation T4 depends on):
    `PutTokenHash(ctx, hash [32]byte, subj Subject) error` where
    `Subject{ Kind SubjectKind; ID string }` and
    `SubjectKind ∈ {Account, Runner}`;
    `ResolveTokenHash(ctx, hash [32]byte) (Subject, error)` (returns the
    subject *with its kind*, so a door can reject a cross-kind token —
    a Runner token on `CompassService`/`CommsService`, an account token on
    `RunnerService`); `RevokeToken(ctx, hash [32]byte) error`.
- Consumes: a Postgres DSN (flag/env on `compass-server`); the domain types
  mirroring the `compass.v1` messages.
- Produces: the durable substrate every later task writes into.

Test cycle: store integration tests against a real Postgres (testcontainer or
CI service); red-first per `rule://red-green-testing`; restart test — write,
reopen the store, read back identical state. `moon run compass-go:ci` green.

### T2 — Comms service on the store + write-through fan-out

Implement the `CommsService` handler (the generated
`compassv1connect.CommsServiceHandler`,
`go/gen/compass/v1/compassv1connect/comms.connect.go:393`) over
the T1 store, with the D9 authorization model enforced on every RPC, and wire
`SubscribeComms` to a **second bus instance** for the comms payload type:
every mutation writes Postgres first, then publishes the corresponding event
(`MessagePosted`/`MessageUpdated`/`ChannelChanged`/…,
`comms.proto:326-356`). The built instance carries
`busPayload = *compassv1.SubscribeEventsResponse`
(`go/server/service.go:33-37`, constructed at
`server/serve.go:140`); the comms stream needs
`events.Bus[*compassv1.SubscribeCommsResponse]` — a different generic
instantiation, so a **separate instance with its own seq space and its own
per-boot `instance_epoch`** (each `NewBus` mints a fresh epoch,
`events.go:151-152`). The shared thing is the implementation, not the
instance: the two streams' seq/epoch spaces are independent. A cursor the
ring cannot serve, or a stale `instance_epoch`, yields `CommsResyncRequired`
and the client re-snapshots from Postgres via the read RPCs, deduping by
message id (the resync protocol under *State + storage*). This task also
lands the **additive `CreateChannel` RPC** (see *Ratified additive contract
changes*) via the gen/drift cycle — without it the vertical cannot create a
channel to post into (`comms.proto:38-100` has no `CreateChannel`).

`Interfaces:`

- `func NewComms(store *store.Store, bus *events.Bus[*compassv1.SubscribeCommsResponse]) *Comms`
  implementing `compassv1connect.CommsServiceHandler`; every method reads the
  authenticated `AccountID` from the request context (set by the T3
  interceptor; the local-socket door attributes the bootstrap admin).
- Contract delta: `rpc CreateChannel(CreateChannelRequest) returns (CreateChannelResponse)`
  on `CommsService` — additive, buf-breaking-safe; schema → `moon run
  compass-go:gen` → commit, in this task's PR.
- Serve-loop wiring: construct the second bus and mount the comms handler
  beside the existing `NewCompassServiceHandler` mount
  (`go/server/serve.go:144-153`).
- Consumes: T1 store; the generic `events` package.
- Produces: the full comms vertical — create accounts/channels, post, list,
  search, subscribe — durable across restart.

Test cycle: handler-level BDD tests (post → subscriber sees `MessagePosted`;
restart → fresh `instance_epoch` → `CommsResyncRequired` → re-snapshot via
`ListMessages` returns the message exactly once, deduped by id;
**consistent snapshot: writes concurrent with a paginated `snapshot_seq`
re-snapshot land wholly after the boundary — no row split across a page, none
lost**; empty-store snapshot bootstraps from the subscribe `snapshot_seq`, not
a first ring event; commit-then-crash-before-publish surfaces the row after
resync; authorization rejections for non-visible channels; **membership tiers:
join grants read but no push; a subscribe toggle flips the per-member boolean;
reserved pings (`@agents`/`@users`/`@everyone`) resolve to the correct member
set**); race-detector lane
on the write-through path.

### T3 — The network door: TLS listener + bearer-token auth

Add the authenticated TCP door to the serve loop: TLS from the
operator-provisioned PEM paths (`TLSConfig`,
`go/server/serve.go:34-39`), an auth interceptor resolving
`authorization: Bearer <token>` through `store.ResolveTokenHash` to a typed
`Subject` — rejecting `Kind == Runner` before projecting to the `AccountID`
injected into the request context (the cross-door rule under *Auth + the
network door*), `IssueToken` implemented
admin-gated (`compass.proto:45-52`), and the first-start bootstrap: create
the bootstrap admin, issue its token, write it 0600 under the server state
dir — never to stdout or a log. The Unix-socket door stays token-free (the
0600 socket is the local credential) and attributes the bootstrap admin.

`Interfaces:`

- `compass-server` flags: `--listen <addr>`, `--tls-cert <pem>`,
  `--tls-key <pem>`, `--db <dsn>`; `--listen` without both TLS flags is a
  startup error.
- `func authInterceptor(store *store.Store) connect.UnaryInterceptorFunc` (+
  the streaming variant) — unknown/absent token ⇒ `CodeUnauthenticated`; a
  `Kind == Runner` subject ⇒ `CodeUnauthenticated` on the client doors (never
  projected to an `AccountID`).
- `func AccountFromContext(ctx context.Context) (AccountID, bool)` — the one
  way handlers read the caller.
- Consumes: T1 token accessors; the existing serve loop.
- Produces: the multi-user network door both services are served on.

Test cycle: integration test dialing the TLS door with a minted token (rcgen
-equivalent self-signed pair for tests); wrong/absent token rejected;
a `Kind == Runner` token rejected on the client doors (the mandatory cross-door
test); token survives a Server restart (Postgres-backed).

### T4 — Runner binary + the frozen `RunnerService` seam

Split container hosting onto the Runner: a new `cmd/compass-runner` binary
wrapping the existing `internal/runtime` package (`ContainerRuntime` /
`PodmanCLI` / `AgentRuntime`,
`go/internal/runtime/podman.go:271-324`,
`internal/runtime/agent.go:1-11`), connecting **out** to the Server over gRPC
with its per-Runner token. **The seam is *specified and frozen* by the
platform record and *built* by its T8/T9** — SEA-1243's T8/T9 realize the
Server↔Runner split (`../../platform/go-toolchain-default.md:363-365`), which
are unbuilt tasks in that record's plan, not code in the tree today (no
`runner.proto`, no `RunnerService` stubs, no `AGENT_SESSION_STATE_DISCONNECTED`
enum yet). So v0.6-T4 **consumes that frozen shape and conforms to its
rulings** (it re-decides nothing) but is **sequencing-gated on SEA-1243 T8/T9
landing `proto/compass/v1/runner.proto` + the internal stubs + the
`DISCONNECTED` enum addition first**, or co-sequencing with them. The
`internal/runtime` container layer it wraps *is* built (`AgentRuntime`,
`PodmanCLI.ExecStreaming` — verified); the `RunnerService` wire is not:

- **Wire shape (frozen):** the internal `RunnerService` is three RPCs —
  `Enroll` (unary, at connect), a `Sessions` **bidi stream** (Server→Runner
  session commands / Runner→Server results, correlated by request id), and a
  `PublishEvents` **client-stream** (Runner→Server agent events) — "This
  shape is fixed here" (`../../platform/go-toolchain-default.md:931-937`).
- **Internal, never public:** `RunnerService` is an internal contract between
  the two binaries, "generated only into the internal Go consumers, never the
  module-root exported `gen/` nor the public TS client", enforced by the
  SEA-1267 drift fence (`go-toolchain-default.md:330-338`). It does not join
  the public `compass.v1` client surface.
- **Token model (frozen OQ7):** a dedicated Runner-subject mint path — not
  `IssueToken` — under a distinct subject-prefix keyspace in the T3 store;
  credential stored `0600`; delivery to the Runner host is an operator
  provisioning step, not an automated RPC; a Runner token is
  `CodeUnauthenticated` on `CompassService`/`CommsService` and an account
  token is rejected on `RunnerService`
  (`go-toolchain-default.md:1410-1423`).
- **Failure matrix (frozen OQ6):** a Runner disconnect moves its sessions to
  `AGENT_SESSION_STATE_DISCONNECTED` (a new, backward-compatible enum value),
  not `Errored`; a **bounded reattach window** governs recovery — reattach
  within it resumes, expiry falls to `Errored`; relay-`Start` carries a
  **request id** so a timeout-retry is idempotent (no duplicate container);
  duplicate enrollment re-attaches the same Runner; the Runner is
  authoritative for live session truth and the Server registry reconciles to
  it on reattach; the link stream is Runner-sequenced
  (`go-toolchain-default.md:1378-1396`).

What is product-new in this task: the `RunnerHub` consumption side
(write-through of relayed events into store + bus) and the **additive
`ProvisionAgentWorkspace` RPC** (see *Ratified additive contract changes*) —
today no RPC launches a container (`StartAgentSession` assumes one exists,
`compass.proto:198-199`).

`Interfaces:`

- Consumed, not defined here: `proto/compass/v1/runner.proto` — the internal
  `RunnerService` (`Enroll` + `Sessions` bidi + `PublishEvents`
  client-stream), generated internal-only per the gen-fence
  (`go-toolchain-default.md:330-338`); the Runner-side outbound client
  (`func Dial(ctx, serverAddr string, token string) (*ServerLink, error)`,
  attach loop, command dispatcher) per the platform record's T8
  (`go-toolchain-default.md:939-943`).
- Contract delta (public, additive): `rpc ProvisionAgentWorkspace(ProvisionAgentWorkspaceRequest) returns (ProvisionAgentWorkspaceResponse)`
  on `CompassService` — agent ref + repo/workstream spec in,
  `container_name` out; routes Client → Server → RunnerHub → Runner →
  `AgentRuntime` façade (`internal/runtime/agent.go:1-5`); provision and
  start stay separate RPCs.
- Runner side: `func Run(ctx context.Context, cfg RunnerConfig) error` where
  `RunnerConfig{ServerAddr, Token, Engine runtime.ContainerRuntime}`.
- Server side: `type RunnerHub` — enrollment registry, command router keyed
  by the session's owning Runner, and the event-fan-out seam:
  `func (h *RunnerHub) Deliver(ctx context.Context, ev RunnerEvent) error`
  is the sole entry point Runner events take into the Server — **fed by the
  `PublishEvents` stream** — so a future brokered transport replaces the
  stream feeding `Deliver`, nothing else.
- Consumes: T3 auth (plus the OQ7 Runner-subject mint), T1 store,
  `internal/runtime`, the frozen platform-record stubs.
- Produces: containers hosted on any enrolled machine; the Server free of
  container-engine code.

Test cycle: end-to-end integration — provision a container through
`ProvisionAgentWorkspace`, observe lifecycle events on `SubscribeEvents`
(podman-gated skip, mirroring the runtime package's existing lifecycle
tests). The OQ6 matrix is pinned, not hand-waved: Runner disconnect ⇒
sessions `DISCONNECTED`; reattach within the window resumes; window expiry ⇒
`Errored`; a relayed `Start` retried after a timeout creates no duplicate
container (request-id idempotency); duplicate enrollment re-attaches the
same Runner; `GetAgentStatus` reconciles to Runner truth on reattach. OQ7's
cross-door tests: a Runner token rejected on
`CompassService`/`CommsService`, an account token rejected on
`RunnerService`.

### T5 — First-party agent over the Runner + transcripts in the store

Build and wire the first-party agent end to end: a new TS/Bun package
`packages/compass-agent` (mirroring the `packages/compass-client`
TS layout) built on `@oh-my-pi/pi-agent-core` (MIT,
`packages/agent/package.json`), composing tools from
`@oh-my-pi/pi-coding-agent`. The agent subscribes to its own SDK event
stream and maps it to `compass.v1` payloads in-process — the mapping is the
agent's own testable surface; there is no Runner-side translator. The Runner
starts the agent in its container over the built streaming exec
(`PodmanCLI.ExecStreaming`,
`go/internal/runtime/podman.go:417-422`), speaks the
newline-framed `compass.v1` stream on its stdio pipes, and relays events up
the `PublishEvents` stream (T4, Runner-sequenced per OQ6). The Server
write-throughs per the **conversation-vs-trace split** (superseded decision 4):
the agent's durable **conversation** — a `text` reply or an `ask` — commits to
comms `Message`/`MessageBlock` rows (T1) + `SubscribeComms`
(`MessagePosted`/`MessageUpdated`); the **execution trace** (assistant/thought
chunks, tool calls, plans, diffs) is OMP-native session data, relayed verbatim
on the **dedicated session-tail stream** for the observation pane, with its
`AgentSessionState` extracted onto `SubscribeEvents` for lifecycle/board.
Durability splits by surface:
the conversation commits to Postgres; the trace's bodies append to the blob store
behind the blob seam. The **S3 session log** is the complete restart-replay
source — trace frames, the durable conversation, and inbound `AgentControl`
(`prompt`/`steer`/`deliver`/`ask_answer`), one ordered append log in the agent's emission
order (see *State + storage*, Replay completeness) — so on (re)start the Server
streams a complete transcript into the fresh container (D6). Session lifecycle
RPCs (`StartAgentSession`/`Stop`/`Reload`/`Status`,
`compass.proto:25-43`) route Client → Server → RunnerHub → Runner; the
frozen `StartAgentSession` comment's "as an ACP session"
(`compass.proto:195-196`) is superseded as doc-intent — same RPC, it now
starts the first-party agent process.

`Interfaces:`

- Agent package: `class CompassAgent` wrapping `Agent` from
  `@oh-my-pi/pi-agent-core` with
  `constructor(opts: { stdin: ReadableStream, stdout: WritableStream, workspace: string })`
  and `run(): Promise<void>`.
- **Output frame envelope (stdout).** The agent emits two classes of payload,
  routed to two surfaces with two owners. The durable **conversation** (an agent
  `text` reply or an `ask`) commits to comms `Message`/`MessageBlock` rows →
  `SubscribeComms`; it is Compass-owned and typed. The **execution trace**
  (assistant/thought chunks, tool calls, plans, diffs) is **OMP-native session
  data**, relayed **verbatim** to a dedicated session-tail stream (below) and
  rendered by OMP's own renderer — Compass does not re-type it. stdout therefore
  carries newline-delimited protojson frames of an **additive internal
  `AgentFrame`** message — a discriminated `oneof frame` with three variants:
  **conversation** — `MessagePosted conversation_posted` / `MessageUpdated
  conversation_updated` (an agent `text` reply or an `ask`, streamed as block
  appends while composing, reusing the frozen payloads without redefining them);
  **session** — `SessionFrame session` (an opaque envelope carrying one
  OMP-native session event as bytes/JSON, plus the `AgentSessionState` the board
  needs); and **delivery ack** — `DeliveryAck delivery_ack` (RT-3: the agent's
  receipt that an inbound `AgentControl.deliver` reached it, carrying the acked
  message id so the Server advances the session's delivery cursor). The reader
  classifies each line by the set `oneof` field; an unset or
  unrecognized field **is** the "unknown frame" the relay logs + counts. The
  Runner write-throughs each to the surface that owns it — conversation →
  comms `Message` rows + `SubscribeComms`; session frames → the dedicated
  **session-tail stream** (opaque OMP data) for the observation pane, plus the
  extracted `AgentSessionStatus` → `SubscribeEvents` for lifecycle/board; the
  delivery ack → the Server's delivery-cursor bookkeeping (not a client surface);
  none is a comms `Message` (superseded decision 4). All frames, plus inbound
  controls, also append to the ordered S3 session log for restart replay (see
  *State + storage*). Diffs and plans are surfaced as PRs (link-out for the MVP);
  they render live in the observation pane as part of the OMP session data, but
  their durable home is the PR, not a comms block.
- **Control frame envelope (stdin) + replay barrier.** stdin carries
  newline-delimited protojson frames of an additive internal `AgentControl`
  message — a discriminated
  `oneof control { PromptControl prompt; SteerControl steer; DeliverControl deliver; AskAnswerControl ask_answer; ConfigControl config; TranscriptReplay replay; ReplayComplete replay_complete }`
  (additive, internal-only, generated with the agent's proto per the gen
  fan-out below; same class as T4's additive deltas). The discriminator makes
  restart replay unambiguous: `TranscriptReplay` frames are applied to context
  (never interpreted as live input), and the Runner **holds all live
  prompt/steer controls until the agent acknowledges with `ReplayComplete`** —
  so a queued prompt can never execute against partially-restored context. The
  ack is idempotent and the Runner re-drives replay if it is lost (the whole
  session is re-established on a fresh container, so replay is replayable). The
  agent applies each via `prompt`/`steer`/`setTools`/`setSystemPrompt`.
- **Turn-end delivery: deliver → queue → coalesce → ack (RT-3, ratified).** A
  plain subscribed-channel message (not an `@`-mention) is delivered to the agent
  **immediately** as an `AgentControl.deliver` — distinct from `prompt` so its
  turn-end-queued semantics are explicit and from `steer` which interrupts. The
  CompassAgent **queues** each `deliver` while a turn is running and, at turn end,
  issues the queued set as a **single** `prompt` (coalescing everything that
  arrived mid-turn into one new-turn input); if idle, the turn-end boundary is
  immediate. On delivery the agent emits an `AgentFrame` **ack** (below); the
  **Server** tracks per-session delivery from those acks — advancing a delivery
  cursor and redelivering any un-acked `deliver` from Postgres on reconnect
  (crash-safe: the S3 session log appends inbound `AgentControl`, and the cursor
  re-derives undelivered messages by comparing the channel against it). The
  agent owns the **turn-end coalescing queue**; the Server owns the **durable
  delivery cursor** — two queues, two owners, one ack that links them. The
  `@`-mention `steer` remains the only mid-turn interrupt.
- **`stderr` is drained, never blocked.** `ExecStreaming` exposes a separate
  `Stderr` pipe (`podman.go:289-296`); the Runner drains it to the agent's
  diagnostic log continuously, so a chatty agent cannot fill the OS pipe
  buffer and stall `podman exec` / the frame stream.
- **Generated types (gen fan-out).** `compass.v1` TS types for the agent are
  generated into `packages/compass-agent/src/gen` via a second `out:` on
  `buf.gen.yaml`, its own drift-gated tree (mirrors the Go side's own gen and
  §T5's "mirror `compass-client` layout"; keeps the `@compass/client`
  server-door fence intact — the agent is not a server-door client).
- Blob seam: `type BlobStore interface { Put(ctx, key string, r io.Reader) error; Get(ctx, key string) (io.ReadCloser, error) }`
  with an S3-compatible implementation (SeaweedFS default self-hosted, R2
  hosted) and keys indexed from Postgres transcript-metadata rows.
- Runner side: `func (r *Runner) StartAgent(ctx context.Context, id runtime.ContainerID) (*AgentStream, error)`
  — spawns the agent via `ExecStreaming`, returns the framed event/control
  stream the relay loop consumes; a frame whose `oneof` variant is unset or
  unrecognized is logged + counted, never silently dropped.
- Consumes: T4 stream, T1/T2 write path, the OMP SDK.
- Produces: a live agent whose **conversation** appears in its channel and whose
  **execution trace** renders in its `AgentWorkspace` observation pane, a
  durable transcript (S3), and restart-with-context on any Runner.

Test cycle: agent-side unit tests (Bun) — SDK event fixtures map to exact
`AgentFrame` frames, exhaustive over emitted `oneof` variants; the split is
asserted directly: a **conversation** frame (agent `text`/`ask`) routes to a
comms `Message` write and **not** to the session stream, and a **session** frame
(opaque OMP event + `AgentSessionState`) routes to the dedicated session-tail
stream + the S3 session (and its extracted `AgentSessionStatus` to
`SubscribeEvents`) and **not** to a comms `Message`; a frame with an
unset/unrecognized variant is counted as unknown, never dropped; control-frame
decode covers each `AgentControl` variant, and a restart `TranscriptReplay` is
applied to context (asserted **not** treated as live input) and ordered before
any live prompt/steer. Runner-side: `stderr` drain
under a deliberately chatty agent does not stall the frame stream. **Delivery
model (the central mid-turn-vs-turn-end distinction): a plain message from a
subscribed channel arriving mid-turn is held and delivered at the agent's turn
end (not interjected); an `@`-mention-borne steer arriving mid-turn interjects
immediately; an agent always-subscribed to its own channel receives its own
channel's messages at turn end; a joined-not-subscribed agent receives no plain
push; an agent→user `@`-mention is a notification, not a steer.** Red-first
integration driving the real `compass-agent` binary in a container (the
precedent test shape, `../../agents/sea-1023-acp-session.md:96`); stop →
restart on a second Runner → context intact.

### T6 — Config distribution

Implement D11 on the T4 stream, plus the programmatic-injection path the
first-party agent enables: the Server stores versioned, content-addressed
per-agent config bundles (Postgres metadata + blob bodies); "config version N
for agent X" signals ride the Runner stream; the Runner pulls and (a)
materializes file-shaped config as a read-only bind mount
(`AgentSpec.Mounts`, `go/internal/runtime/agent.go:41-42`),
atomically swapped, and (b) injects skills/tool-surface/system-prompt changes
as **structured control frames into the running agent's stdio stream**,
where the agent applies them via the SDK's
`setTools`/`setSystemPrompt`/`setModel` surface
(`packages/agent/README.md`, `github.com/can1357/oh-my-pi`) — no agent
restart for a tool-set or prompt change. Secrets in config bundles observe
the D14 boundary (encrypted at rest, per-agent scoped, redacted from
transcripts).

`Interfaces:`

- `func (s *Store) PutConfigBundle(ctx, agent AccountID, b Bundle) (Version, error)`,
  `GetConfigBundle(ctx, agent AccountID, v Version) (Bundle, error)`.
- Runner: `func (r *Runner) SyncConfig(ctx, agent AccountID, v Version) error`
  — pull, verify content address, swap mount, emit the injection control
  frame on the agent's stdin (T5's control-frame channel).
- Agent: a `config` control frame handled by `CompassAgent` — applies
  `setSystemPrompt`/`setTools` and acknowledges with a status frame.
- Consumes: T4 stream, T1 store, T5 container lifecycle + control channel.
- Produces: centrally-updated agent config with no cross-host filesystem.

Test cycle: config update reaches a running agent (integration: bump a skill
file, observe the in-container mount swap + notification); injection test —
push a tool-set change, observe the agent's next turn using the new tool
surface without a restart.

### T7 — UI: the communication layer as the primary surface

Repivot the SolidJS Client so the **channel is the primary human↔agent
surface** and the workspace is the **observation pane** (superseded decision 4):
channel creation + listing (`CreateChannel` — T2's additive RPC — plus
`ListChannelGroups`/`ListChannels`), live conversation rendering from
`SubscribeComms` (channel `text` + `ask`, including the resync protocol:
re-snapshot via read RPCs, dedup by message id), with async **`ask`** rendered
inline in the channel and `RespondToAsk` wired, and **steer** by `@`-mentioning
the agent in its channel (the unified injection path). The **`AgentWorkspace`
view is the observation pane**: it renders the agent's live **execution trace**
from the **dedicated OMP-native session stream** (a session-tail RPC relaying the
agent's OMP session events verbatim, T5) plus the terminal and file panes —
observation-only, with a **stop** control and no message-composer. For the MVP
the trace pane **reuses OMP's own renderer** over that native stream rather than
a bespoke renderer (an explicit MVP lever — it lets T7 focus on the
channel/message/thread surface); a first-party trace renderer is a later
increment. Diffs/plans link out to their PRs. **Observation-pane access is a
projection of home-channel membership** (RT-2) — being in the agent's **home**
channel grants pane access, scoping `SubscribeAgentSession` the same way; there
is no separate workspace share (the `participant_user_ids` +
share/unshare model is removed, superseded decision 4). Transport factories carry
the bearer token as a connect interceptor; no local-assumption leaks
(`../compass-tauri-shell.md:119-121`).

`Interfaces:`

- Consume the **already-shipped** `createCommsClient` / `createCommsWebClient`
  factories (`packages/compass-client/src/index.ts:101,110`) — not
  new work; T7's genuinely-new deliverable is the SolidJS surface below.
- Consumes: the generated TS comms client, T2/T3 doors.
- Produces: the browser MVP surface.

Test cycle: component tests for channel conversation rendering (`text` + async
`ask` flow, `RespondToAsk`) and for the observation pane rendering the
OMP-native session stream with a working stop control and no composer; steer via
`@`-mention reaches the running agent. E2E: two browser sessions whose users are
both members of the agent's channel, both watch the live trace in the
observation pane while the conversation advances in the channel; an `ask` posted
mid-run is answerable without the agent having blocked.

### T8 — Supervisor + Bridge board

Layer multi-agent orchestration on the running spine (D4/D13): a supervisor
agent account assigning tasks to worker agent accounts over channels, and the
Bridge board as a Server-side projection of agents' workstream state,
streamed over `SubscribeEvents`. MVP depth: assignment only; conflict map and
auto-assignment deferred (`../compass-0.5/design.md:507-517`).

`Interfaces:`

- Consumes: everything T1–T7. Board projection **reuses the frozen
  `AgentSessionStatus` payload** (`compass.proto:102`) aggregated per
  workstream — no new proto surface for the MVP; any later board-specific
  variant is an additive `SubscribeEventsResponse.payload` oneof entry,
  drift-gated, and would be listed here when scoped.
- Produces: the MVP orchestration loop — a supervisor assigns work to two
  workers over channels and the board reflects their live state.

Test cycle: integration/E2E — supervisor assigns to two workers; board
updates; all messages auditable via `SearchMessages`.

### T9 — Living-spec reconciliation

As increments ship, update `docs/specs/product/compass.md` to state built
behavior as Requirement/Scenario contracts and point its design-record
cross-reference at this record. No contracts for unbuilt behavior.

`Interfaces:` consumes this record + shipped increments; produces the spec
edits, gated by the docs-system spec-impact check. **Test cycle:** this is the
one doc-only task with no runtime test cycle — its verification is the
spec-impact check green plus every new Requirement/Scenario citing a shipped
increment (no contract for unbuilt behavior).

## Tasks

- [ ] **T1 — Postgres store of record**: `internal/store` package, migrations,
      accounts/channels/workspaces/messages/tokens/search accessors;
      restart-durability integration test green.
- [ ] **T2 — Comms service on the store**: `CommsService` handler over T1 with
      D9 authorization; second bus instance for the comms payload; additive
      `CreateChannel` + `snapshot_seq` boundary; write-through + consistent
      snapshot/resync/dedup protocol; restart test green.
- [ ] **T3 — Network door**: TLS listener + bearer-token interceptor +
      `IssueToken` + bootstrap admin; tokens durable in Postgres.
- [ ] **T4 — Runner binary + frozen seam**: `cmd/compass-runner` consuming the
      frozen internal `RunnerService` (Enroll + Sessions bidi +
      PublishEvents); OQ6 failure matrix + OQ7 token model pinned; additive
      `ProvisionAgentWorkspace`; end-to-end launch test green.
- [ ] **T5 — First-party agent over the Runner**: `packages/compass-agent` on
      the OMP SDK emitting `compass.v1` natively; streaming-exec relay →
      write-through; blob seam for transcript bodies; restart-with-context
      test green.
- [ ] **T6 — Config distribution**: versioned bundles, Runner pull, atomic
      mount swap + programmatic injection into the running agent; live-update
      integration test green.
- [ ] **T7 — Comms-first UI**: comms client factories, channel surfaces +
      observation pane (channel-membership access, stop control), ask flow;
      E2E green.
- [ ] **T8 — Supervisor + Bridge board**: assignment over channels + board
      projection; orchestration E2E green.
- [ ] **T9 — Living-spec reconciliation** as increments ship.
- [ ] This record lints clean (markdownlint) and merges as the frozen v0.6
      contract.

## Resolved decisions

The four questions this record originally raised were ratified by Matt at the
first freeze gate (July 2026). The **interaction-surface inversion** (superseded
decision 4) was ratified in a second pass (July 2026, this amendment): its core
plus three round-two forks (threading carrier, the trace's stream, the
observation-pane ACL) are decided below; the three shape questions (OQ-A
`MessageBlock` narrowing, OQ-B steer RPC, OQ-C `workspace_id`) are all resolved
(see *Resolved shape questions*). All decided items
are recorded here as the contract.

- **Server↔Runner transport: stay gRPC for the MVP; re-evaluate at a concrete
  trigger.** The full analysis is in *Alternatives considered*. The MVP is
  gRPC on every hop; NATS is settled OUT for the Server-internal role (no
  broker — Postgres is the substrate, the in-memory bus is the fan-out) and
  OUT for the Client hop (browsers force gRPC-Web). The one defensible future
  home is the Server↔Runner event transport, and even there v0.6 does **not**
  adopt a broker now: committing to NATS at this hop would supersede the
  frozen SEA-1243 T8 gRPC `RunnerService` shape ("This shape is fixed here",
  `../../platform/go-toolchain-default.md:935-937`) — whose generated stubs
  the Runner-side client already consumes — and re-sequence the work built on
  it. **Decision:** stay gRPC for the MVP; keep the narrow event-fan-out seam
  (`RunnerHub.Deliver` fed by `PublishEvents`, task T4) so a brokered
  transport can slot in behind it later without re-architecting; re-evaluate
  only when a concrete trigger fires — more than one Server instance with
  Postgres LISTEN/NOTIFY (see *Alternatives considered*) shown insufficient,
  or a Runner count at which per-connection stream management measurably
  hurts. A future adoption is SEA-1243's amendment to make, not v0.6's.
- **`CreateChannel` (T2): ratified as an additive contract change.** See
  *Ratified additive contract changes*. `CommsService` gains
  `rpc CreateChannel(CreateChannelRequest) returns (CreateChannelResponse)` —
  caller-authorized against the parent group, emitting `ChannelChanged` —
  closing the gap where the event surface documents "A channel was created"
  (`comms.proto:338-339`) but no RPC creates one. Additive (buf-breaking-safe);
  lands in T2's implementation PR via the gen/drift cycle.
- **`ProvisionAgentWorkspace` (T4): ratified as an additive contract change.**
  See *Ratified additive contract changes*. `CompassService` gains
  `rpc ProvisionAgentWorkspace(ProvisionAgentWorkspaceRequest) returns (ProvisionAgentWorkspaceResponse)`
  — agent ref + repo/workstream spec in, `container_name` out — routing
  Client → Server → RunnerHub → Runner and driving the built lifecycle façade
  (`go/internal/runtime/agent.go:1-5`). Provision and start stay
  separate RPCs, matching the frozen `StartAgentSession` semantics. Additive
  (buf-breaking-safe); lands in T4's implementation PR.
- **Consistent-snapshot boundary (T2): ratified — add a `snapshot_seq` token
  to the read path.** The review found that the `since_seq = 0` refinement
  (snapshot from the Postgres read RPCs) could not produce a *consistent*
  point-in-time snapshot: paginated `ListMessages` without a boundary lets
  concurrent writes move rows across page edges, and an empty ring gives no
  first event to read the epoch/head-seq from. **Decision (Matt):** add an
  additive `snapshot_seq` boundary — the subscribe response returns it, every
  read RPC takes it, so the whole snapshot is one point-in-time view and the
  live tail resumes at `snapshot_seq + 1`; message-id dedup covers the overlap.
  Chosen over preserving whole-state stream snapshots (reintroduces unbounded
  connect cost through the ring) and over deferring the boundary post-MVP
  (ships a frozen contract with a known consistency hole). Additive fields
  (buf-breaking-safe); land in T2's implementation PR. Safe pre-launch: no live
  client depends on the old semantics (the Server on `main` is ephemeral).
- **Interaction surface: channel-primary, trace observation-only (superseded
  decision 4): ratified, with the three round-two forks decided (below).** The
  agent's primary human↔agent surface is its **channel** (a Slack/Discord-style
  DM with threading); the `AgentWorkspace` is demoted to an **observation pane**
  for the live execution trace plus terminal/file panes. The agent communicates
  through exactly two *durable* surfaces — **channel messages** (`text` + async
  **`ask`**) and **pull requests**. The execution trace (assistant/thought
  chunks, tool calls, plans, diffs) is observation-only, its durable form the S3
  session, **not** comms `Message` rows. **Decision (Matt):** (a) `Ask` is
  **async** — the "blocks until answered" clause (`comms.proto:271-276`) is
  dropped; blocking is the agent's turn-level choice; (b) a human **steer**
  (`@`-mention) and an ask-answer are one session-injection path; (c)
  **owner-membership is transitive** — an agent's DMs and the channels it starts
  always include its owning user(s). **Round-two forks (Matt, this amendment):**
  (d) **threading has a contract carrier** — add an additive `parent_message_id`
  to `Message` now (the primary surface is threaded, so the MVP contract carries
  it, not a later retrofit); (e) **the trace is a dedicated OMP-native session
  stream, not typed `SubscribeEvents` variants** — a new session-tail RPC relays
  the agent's OMP-native session events verbatim (opaque envelope) for the
  observation pane, rendered by OMP's own renderer; the three ACP-translation
  variants `agent_message_chunk`/`agent_tool_call`/`agent_plan` are **dropped**
  from `SubscribeEventsResponse`, which keeps only its Compass-owned projections
  (`ServerStatus` liveness, `ResyncRequired` replay control, `AgentSessionStatus`
  lifecycle, and the T8 board); (f) **one ACL** — observation-pane access is a
  projection of **home-channel membership** (being in the agent's **home**
  channel grants pane access — RT-2 ratified; the membership-mutation carrier is
  `UpdateChannelMembers`, RT-1), so `AgentWorkspace.participant_user_ids`
  and the `ShareAgentWorkspace`/`UnshareAgentWorkspace` RPCs
  (`comms.proto:67-76,207-221`) are **removed** — no second, cross-service ACL.
  Chosen over the frozen workspace-as-conversation model because it (i) unblocks
  the agent (async ask), (ii) keeps the load-bearing human signal — questions,
  blockers — in the searchable channel instead of buried in the trace, (iii)
  keeps the durable conversation as the natural cross-time record while the work
  stays in the session, (iv) removes the highest-volume data from Postgres + the
  comms event stream, and (v) delegates the session-render surface to OMP over
  its own stream while Compass owns only the communication surface + board. The
  trace's durable form is the S3 session (D6/D12); Postgres holds only its
  index/metadata. Contract impact: additive (`parent_message_id`, the session
  RPC, the channel-membership carriers) **plus three pre-launch buf-breaking
  removals** (the three trace variants; the share RPCs; the
  `Message.container.workspace_id` narrowing, OQ-C) — safe because no live client
  exists (the Server on `main` is ephemeral) and the dropped trace variants were
  never published (the only production Publish sites are `ServerStatus` at
  `serve.go:207` and `ResyncRequired` at `service.go:151` — both kept variants).
  Lands via the gen/drift cycle in T1/T2/T5/T7.
- **Round-three amendment (channel-membership model): the five red-team forks
  ratified (Matt, July 2026).** The round-three channel-membership model +
  reserved pings + DM expansion surfaced five contract gaps, all now decided:
  - **RT-1 — membership has an RPC carrier.** Add one additive
    `rpc UpdateChannelMembers(UpdateChannelMembersRequest) returns
    (UpdateChannelMembersResponse)` to `CommsService` (add/remove members +
    flip the per-member `subscribed` flag), authz'd against channel visibility,
    emitting `ChannelChanged` — covers join, subscribe-toggle, DM-expansion, and
    share-replacement. See *Ratified additive contract changes*. Additive.
  - **RT-2 — the agent's own channel is its home channel.** An `AgentAccount`
    carries an additive `home_channel_id` (minted at `CreateAgent`); the
    always-subscribed row, turn-end "own channel" delivery, and the
    observation-pane ACL all mean the home channel, and `SubscribeAgentSession`
    is scoped to **home-channel** membership. Matt's ruling: the trace carries
    nothing more sensitive than the conversation the same members already read,
    so it is the one shared ACL — no stricter trace-specific grant. Additive
    field; no second ACL (fork f preserved).
  - **RT-3 — turn-end delivery: agent-queued, coalesced, acked.** A subscribed
    channel message is delivered to the agent **immediately** as an additive
    `AgentControl.deliver` variant; the CompassAgent **queues** it and, at turn
    end, issues the queued set as a single `prompt` (coalescing any messages that
    arrived mid-turn); the agent returns an additive `AgentFrame` **ack** once
    delivered, so the Server advances a per-session delivery cursor and redelivers
    unacked messages on reconnect (`@`-mention `steer` stays the mid-turn
    interrupt). See *Ratified additive contract changes*. Additive.
  - **RT-4 — reserved-ping fan-out: accept unbounded for the MVP.** `@everyone`/
    `@agents` steering every agent member with no rate bound, permission gate, or
    coalescing is **accepted for the MVP** (dogfood, trusted small membership);
    abuse controls (rate limit / role-gate / coalesce) are post-MVP — a ratified
    stance, not an omission.
  - **RT-5 — DM→GROUP_DM expansion: add in place, history disclosed,
    auto-subscribe.** Any current member may add a member to a DM/GROUP_DM (via
    `UpdateChannelMembers`); the row's `ChannelKind` widens `DM`→`GROUP_DM` in
    place with **no new-channel minting**; prior history **is** disclosed to the
    added member (same deliberate cross-owner disclosure class the record already
    ratifies); an agent added to a DM/GROUP_DM is **auto-subscribed**. A
    fresh-channel Slack-style privacy model may be revisited post-MVP.

## Resolved shape questions

The three shape questions the interaction-surface inversion raised (superseded
decision 4) are all **resolved** — none remains open at freeze. Recorded here
with their decisions for the audit trail.

- **OQ-A — `MessageBlock` oneof narrowing → RESOLVED** by the session-stream
  decision. The trace is opaque OMP-native frames on a dedicated session stream,
  not typed `compass.v1` variants and not rendered from `MessageBlock`, so the
  comms `MessageBlock` oneof (`comms.proto:247-262`) drops **all** trace variants
  (`thought`/`tool_call`/`plan`/`diff`), narrowing to `text` + `ask`. Nothing on
  the observation pane consumes the typed variants, so none is kept "defined but
  unused." Physical removal lands in T2 via the gen/drift cycle (buf-breaking,
  pre-launch safe — no live client).
- **OQ-B — steer RPC shape → RESOLVED (Matt): reuse `PostMessage`.** A human
  steer reuses `PostMessage` into the agent's channel with server-side
  `@`-mention routing into the running session — no dedicated `SteerAgent` RPC.
  A steer *is* a channel message; reuse adds no RPC, unifies with ask-answer,
  and keeps the "everything is in the channel" property. See *Ratified additive
  contract changes*, unified steer path + *Channel membership: join / subscribe / mention*.
- **OQ-C — `Message.container` `workspace_id` → RESOLVED (Matt): dropped.** With
  the workspace no longer a message container, `workspace_id` is dropped from the
  `Message.container` oneof (`comms.proto:233-236`), and the co-narrowing
  `workspace_id` on `ListMessagesRequest` (`comms.proto:448-451`),
  `PostMessageRequest` (`comms.proto:465-468`), and `SearchMessagesRequest.scope`
  (`comms.proto:493-496`) drop with it — an agent's durable messages live in its
  **channel**, so `Message.container` becomes **channel-only**. Buf-breaking,
  pre-launch safe (no live client); lands **with T1** (the message-schema
  migrations + the store's `ContainerRef` fix the container shape). This is a
  third pre-launch buf-breaking removal alongside the trace variants + the share
  RPCs.

## Resolved round-three questions

The round-three amendment (channel-membership model + reserved pings + DM
expansion) surfaced five contract gaps a red-team pass flagged. All five were
**ratified by Matt** (July 2026) and fold into the contract above — the full
rulings are recorded under *Resolved decisions* ("Round-three amendment"), and
each lands in its implementing task via the gen/drift cycle. Summary:

- **RT-1 — membership-mutation RPC → RESOLVED.** One additive
  `UpdateChannelMembers` on `CommsService` carries join, subscribe-toggle,
  DM-expansion, and share-replacement (*Ratified additive contract changes*).
- **RT-2 — "the agent's own channel" → RESOLVED: home channel.** Additive
  `home_channel_id` on `AgentAccount`; the always-subscribed row, turn-end
  delivery, and the observation-pane ACL all scope to it. The trace carries
  nothing more sensitive than the conversation the same members already read, so
  it is the one shared ACL — no stricter trace-specific grant.
- **RT-3 — turn-end delivery carrier → RESOLVED: deliver → queue → coalesce →
  ack.** A subscribed message arrives immediately as `AgentControl.deliver`; the
  agent queues it and issues the coalesced set as one `prompt` at turn end, acking
  via an `AgentFrame.delivery_ack` so the Server advances a delivery cursor and
  redelivers unacked messages on reconnect (*Agent runtime*).
- **RT-4 — reserved-ping fan-out → RESOLVED: accept unbounded for the MVP.** Abuse
  controls (rate limit / role-gate / coalesce) are post-MVP — a ratified stance.
- **RT-5 — DM→GROUP_DM expansion → RESOLVED: add in place.** Any current member
  may add; `ChannelKind` widens in place with no new-channel minting; prior
  history is disclosed to the added member; an added agent is auto-subscribed.
