# Compass v0.5 — Client → Server → Runner, with the communication layer as the spine

Status: Historical

> Internal design record — July 2026. A topology + product-shape pivot of the
> Compass design. The full ADE vision it builds on is the frozen v0.3 record
> ([`../compass.md`](../compass.md)); the strategic posture is the frozen v0.4
> record ([`../compass-0.4/design.md`](../compass-0.4/design.md)); the UI shell
> and state model are [`../compass-ade-shell/design.md`](../compass-ade-shell/design.md)
> and [`../compass-dock-in-sidebar/design.md`](../compass-dock-in-sidebar/design.md);
> the desktop shell is [`../compass-tauri-shell.md`](../compass-tauri-shell.md).
> This record captures only what v0.5 changes and why, and **supersedes specific
> prior decisions by citation** — it does not rewrite any frozen record (records
> freeze on merge: `../../platform/docs-system.md:31-33`, `../compass-0.4/design.md:191-193`).
> Living built-behavior is the spec ([`../../../specs/product/compass.md`](../../../specs/product/compass.md)).
>
> **Status: decided.** Every load-bearing Open Question is folded in as a
> Decision (Q1 → D1, Q2 → D9, Q3 → D10, Q4 → D11, Q5 + Q10 → D12, Q8 → D13,
> Q11 → D14); what remains under Open Questions is non-load-bearing deferrals
> only (Q6, Q7, Q9, Q12). The record is ready to freeze on merge as the contract
> executing agents build against.

## Problem / Intent

The v0.3/v0.4 design is a **single-machine ADE**: one persistent local daemon
that "owns everything privileged: the per-agent container lifecycle (rootless
podman …), agent process management, PTYs, the Warden security layer …"
(`../compass.md:274`), driving each agent as an ACP client on its own host, with
a thin desktop shell rendering the UI (`../compass.md:270-276`). v0.4 kept that
shape and chose the commodity layers to leverage behind seams — Cotal for
coordination (`../compass-0.4/design.md:36-38,113-134`), OMP as the
default/reference agent (`../compass-0.4/design.md:73-92`), seal scoped to
hosting Warden (`../compass-0.4/design.md:94-111`).

Two things force a topology pivot now:

- **The product wedge is collaboration, not a single-user cockpit.** v0.3
  explicitly parked "**Real-time human collaboration in the ADE** … rather than
  routing it through a separate tool like Slack (the '@agent do X' pattern) …
  a planned direction. It is not a v0 feature … its design is deferred until
  after this document lands" (`../compass.md:425`). v0.5 **promotes that exact
  deferred direction to the product's core pillar**: humans *and* agents are
  first-class accounts in a management hierarchy, conversing in channels, with
  the agent conversation itself flowing through the communication layer.
- **A single host can neither scale nor survive.** The daemon runs the agent
  containers itself on its own host (`../compass.md:274`), so a hung agent,
  crashed container, or dead host loses context, and there is no path to
multi-user, multi-host, or hyperscaler placement. The coordination bus v0.4
  chose (Cotal) is scoped to "agent↔Dispatcher signals, cross-agent presence,
  and replay-on-join" (`../compass-0.4/design.md:115-116`) — coordination, not
  the first-class users and ownership hierarchy v0.5's product depends on.

**Intent of v0.5:** split the daemon into a networked, multi-user **Server** and
a placement **Runner**, make a Discord-style **communication layer** the spine
of the product (agents as owned accounts, the ACP conversation as a DM), and
hold **all agent state in the Server** so runners and agent containers become
throwaway. The topology is not a from-scratch rebuild: the Client↔daemon seam
(`compass.v1`, `../compass.md:278-286`) and the hosted/remote-daemon transport
seam v0.3 reserved (`../compass-tauri-shell.md:107-121`) are exactly the seams
v0.5 realizes.

The MVP is deliberately narrowed on delivery form — browser-first (the desktop
Tauri app deferred, D7) and Warden deferred (D8) — but it **keeps multi-agent
orchestration** (D4): the product must dogfood the wave structure it runs on, a
supervisor agent coordinating worker agents on the Bridge board. A single agent
flowing end to end through the tiers is the first build increment, not the MVP
ceiling.

## Approach

### The three tiers

A **Client → Server → Runner** topology replaces the daemon/shell/UI split
(`../compass.md:270-276`). Each tier owns one responsibility:

- **Client** — the web UI, in a browser (MVP) or a Tauri app (deferred, D7).
  Reuses the SolidJS shell and its workstream/agent state model
  (`../compass-ade-shell/design.md:1-9`, `../compass-dock-in-sidebar/design.md:1-9`),
  repivoted so the communication layer is the primary surface (D5). It is a
  `compass.v1` client (D2) and assumes nothing "local" beyond the transport
  boundary — the invariant the hosted-mode seam already demands
  (`../compass-tauri-shell.md:119-121`).
- **Server** — the long-lived, networked, multi-user orchestrator and stateful
  store: the swimlane board (`../compass.md:91-98`), issues/CI status, the
  communication layer (D1), and the agent-state/config store (D6). It runs the
  first-party comms service behind the Compass-owned comms seam (D1), speaks the
  evolved `compass.v1` contract to Clients (D2), and accepts Runner connections
  (D3). This is the daemon of `../compass.md:274` promoted to a network service
  — realizing the reserved hosted-mode transport (`../compass-tauri-shell.md:107-121`),
  not a new architecture.
- **Runner** — a binary deployed on a machine you want to run agent containers
  on, like a CI runner: it manages the per-agent containers (the rootless-podman
  clone-per-container model of `../compass.md:141-149`), connects out to the
  Server, and streams container activity back. This splits out the "runs each
  agent in its own container … on its own host" responsibility the daemon held
  (`../compass.md:274`). Future Runner implementations target hyperscaler infra
  (e.g. per-agent ECS containers); the MVP Runner is a single local binary.

### The communication layer (the spine)

The communication layer is a Discord/Slack-style channel system where **humans
and agents are first-class accounts in a management hierarchy** — the distinction
from a pure coordination bus, and the reason v0.5 builds a first-party comms layer rather than adopting a coordination bus as-is (D1). Agents are
**owned by users**; a user's agents, and the agents of users they manage, are
theirs to converse with, share, and audit.

- **The ACP conversation is a DM channel.** v0.3 renders each agent's ACP
  session — "tool calls, diffs, plans, permission prompts, terminal output"
  (`../compass.md:282`) — as native UI via `compass.v1`. v0.5 routes that same
  ACP conversation **through the communication layer as a DM channel** between
  the agent's account and its owning user. The channel is upgraded to expose the
  interaction affordances the ACP surface needs: a first-class **`ask`**
  (ACP's `session/request_permission`, `../compass.md:139`), **tool-call
  rendering**, diffs, and plans — the ACP-as-native-UI rendering of
  `../compass.md:294-296` moving onto the channel surface.
- **All comms flow through the layer → audit/search for free.** Agent logs,
  agent↔agent messages, and human↔agent messages are all channel messages, so
  the audit log and search are properties of the substrate, not a separate
  pipeline.
- **DM → group-DM session sharing.** A user can share an agent session with
  another user by turning the agent's DM into a group DM — the concrete
  mechanism for "real-time human collaboration in the ADE" (`../compass.md:425`).

The concrete substrate is a **first-party Rust comms service on NATS/JetStream**,
sitting **behind a thin comms seam Compass owns** — the same seam pattern v0.4
used to keep Cotal swappable ("behind a thin interface Compass owns … so the bus
stays swappable", `../compass-0.4/design.md:118-122`). Cotal is the working
reference for the substrate, not a fork; the seam keeps it swappable (D1).

### Throwaway runners, state in the Server

All of an agent's state lives in the **Server DB** (D6), making runners and agent
containers **throwaway**:

- **Full transcripts** are held server-side and sent into a container on
  (re)start, so an agent can be stopped, restarted, or moved to a different (or
  bigger) machine without losing context. A hung/crashed agent, container, or
  Runner host no longer loses its context — the Server spins it back up.
- **Centralized agentic config** — skills, extensions, hooks, MCP config, auth
  config, agent settings — lives in one server-side place so the Server can
  update every agent's copy. The mechanism (D11) is a **read-only mounted volume**
  the agent reads its config from, plus an **update-notification path** so an
  agent learns a new skill or a new `AGENTS.md` rule landed. This matters
  because the base agent is assumed to update frequently (dozens of times a
  day): long-lived containers need a way to pull those updates plus the agent
  binary and base image.

This generalizes the per-agent container model of `../compass.md:141-149`
(rootless-podman, clone-per-container, scoped creds) by moving the *host* onto
the Runner and the *state* into the Server.

### MVP scope: what's in, what's deferred

- **Multi-agent orchestration is in the MVP** (D4). The Supervisor
  (`../compass.md:69-81`) and the Bridge board (`../compass.md:91-98`) ship,
  because the product must dogfood the wave structure it runs on. A single agent
  flowing end to end through the tiers is the first build increment; the
  supervisor + board layer onto it.
- **Browser-first; Tauri deferred** (D7), relative to the Tauri shell workstream
  (SEA-1022, `../compass-tauri-shell.md:3`). The Runner binary and its
  Server connection are still built (D3).
- **Warden deferred** (D8): the seal-hosted security auditor scoped in v0.4
  (`../compass-0.4/design.md:94-111`) comes later; v0.5 builds the
  orchestration + productivity product first.

## Decisions

### D1 — A first-party Rust comms service on NATS/JetStream (Cotal as reference), behind a Compass-owned seam

The communication layer is a **first-party comms service, built in Rust on NATS
JetStream**, behind a thin comms interface Compass owns. JetStream supplies the
durable-messaging substrate — streams as channels, subject hierarchies, durable
consumers for replay-on-join, KV for state — and Compass builds the first-class
**user + agent accounts and the ownership/management hierarchy** natively on top.
The seam keeps Compass's collaboration logic (accounts, ownership, ACP-as-DM,
session sharing, audit/search) above the substrate.

**Build, don't fork — and don't adopt as-is.** v0.4 adopted Cotal (Apache-2.0,
NATS/JetStream) as the coordination substrate, scoped to "agent↔Dispatcher
signals, cross-agent presence, and replay-on-join" (`../compass-0.4/design.md:115-116`)
— a coordination bus, not the account/ownership system v0.5's product depends on.
Cotal validates the substrate (it runs agent fleets on exactly this JetStream
foundation today), so it is the **working reference**, not a dependency to fork:

- **A fork diverges past rebase.** v0.5 must add first-class accounts, an
  ownership hierarchy, ACP events as first-class message types, and the
  ask/tool-call/diff/plan affordances — enough net-new that tracking upstream
  quickly becomes impossible; a fork accrues a divergent copy's maintenance cost
  with none of the upstream-rebase benefit.
- **Rust-native meshes with the product.** A first-party service in Rust sits in
  the same stack as the rest of the Server, rather than carrying a separate
  runtime and its rough edges as a leveraged black box.
- **Owning the spine lets us shape it.** The comms core is the product spine; a
  first-party build lets Compass evolve the schema and correctness properties
  directly instead of bending an upstream's.

Rationale: once Compass builds its own client and account model regardless (D5,
Q2), the value of adopting or forking an existing server collapses — its client
is discarded and its account model replaced — while its constraints remain.
Building first-party on the proven JetStream substrate keeps the already-solved
hard parts (durable ordered streams, replay-on-join) and spends the build effort
on Compass-specific value. The seam preserves the option to swap the substrate.

**Supersedes** v0.4's "Adopt Cotal … as the coordination substrate behind a thin
interface Compass owns" (`../compass-0.4/design.md:36-38,113-134`): the
**substrate choice (NATS/JetStream) is kept and vindicated**, but Cotal moves
from *adopted dependency* to *reference implementation* for a first-party Rust
service. **Implication:** the Cotal-adoption work tracked by SEA-1115 (In Review)
is reframed to the first-party build, and the Cotal high-assurance trust-tier
gate SEA-1113 (`../compass-0.4/design.md:124-132,158-159`) no longer gates a
Compass dependency — Compass owns the trust boundary directly. The comms seam
**keeps** v0.4's swappable-interface discipline; the substrate behind it becomes
first-party.

### D2 — Evolve `compass.v1` from a local daemon contract into the Client↔Server contract

The `compass.v1` gRPC contract — "one gRPC service: typed request/response
commands plus a server-streaming event channel … served over Connect so the same
contract reaches native clients … and the browser (gRPC-Web) alike"
(`../compass.md:278-286`) — **survives as the Client↔Server seam** and is
**evolved** to carry the networked, multi-user Server surface (accounts, channels,
the ACP-as-DM event stream, board/CI/issue updates). The "generated client is the
only sanctioned way to reach" the Server (`../compass.md:283`), and the daemon's
translation of ACP `session/update` into sequenced `compass.v1` events
(`../compass.md:282`) is retained and extended for the channel surface.

Rationale: the seam already exists and is designed to be UI-swappable and
CI-enforced (`../compass.md:284-286`); v0.5 needs the same door, now
network-facing. Building a new contract would discard a compiler/CI-enforced
boundary for no gain.

**Supersedes/refines** v0.3 §7.2 (`../compass.md:278-286`) by extending
`compass.v1` from a single-user local contract to a multi-user networked one; it
does not replace the contract.

### D3 — The Server tier realizes the reserved hosted-mode transport seam

The networked Server is the **realization of the hosted/remote-daemon seam v0.3
already reserved**, not a from-scratch rebuild. The Tauri-shell record documents
that "A hosted deployment — the daemon on a different machine than the client …
stays possible without changing this design … A hosted mode is a **sibling
transport** … the daemon has no authenticated network listener today (UDS +
dev-loopback only), so hosted mode needs a TLS+auth server transport on the
daemon plus a client-side transport-mode selector, its own future workstream"
(`../compass-tauri-shell.md:107-121`). v0.5 **is** that future workstream: add the
Server's authenticated network listener (TLS + auth) and let Clients select the
remote transport. The Runner connects to the Server as a separate outbound
connection (its own transport, distinct from the Client↔Server `compass.v1`
door).

Rationale: framing the Server as a reserved-seam realization is accurate and
keeps the invariant that "the shell and UI must never assume 'local' beyond the
transport boundary" (`../compass-tauri-shell.md:119-121`) load-bearing.

**Supersedes/refines** v0.3 §7.1's single-host daemon (`../compass.md:270-276`) by
splitting container hosting onto the Runner; realizes the reserved seam
(`../compass-tauri-shell.md:107-121`).

### D4 — Multi-agent orchestration is in the MVP (Supervisor + Bridge); single-agent is the first build increment

The usable MVP **dogfoods a multi-agent wave structure inside Compass**: a
supervisor agent coordinating worker agents across the communication layer,
surfaced on the Bridge board. So the **Supervisor** (v0.3's Dispatcher,
`../compass.md:69-81`, renamed per D13) and the **Bridge** swimlane board (`../compass.md:91-98`)
are **in the MVP**, not deferred.

Multi-agent orchestration is not bolted on — it falls out of the comms pivot.
v0.5 already makes agents first-class accounts in the communication layer (D1,
D5), so the Supervisor is a **supervisor agent account** coordinating **worker
agent accounts** in channels (the same pattern used to run agent fleets today),
and the Bridge board is a Server-side projection over those agents' workstream
state.

**Build order, not scope cut:** a single agent flowing end to end through
Client → Server → Runner is the **first build increment** (incremental PRs) — the
foundation the supervisor + board layer onto — so the tiers, the comms seam, and
`compass.v1` are de-risked before orchestration rides on them. OMP is the base
agent (`../compass.md:113-129`, `../compass-0.4/design.md:73-92`); users bring
whatever OMP auth they want (subscription, API keys).

Rationale: the product's core differentiator and its dogfooding target are both
multi-agent — a single-agent cockpit ships neither. Sequencing single-agent first
buys architectural de-risking without cutting orchestration from the MVP.

**Refines** v0.4's OMP-as-default (`../compass-0.4/design.md:73-92`): OMP stays
the base agent, now driven under multi-agent orchestration. The Supervisor and
Bridge (`../compass.md:69-81,91-98`) are in the MVP, sequenced after the
single-agent foundation.

### D5 — The UI pivots around the communication layer; the ACP conversation is a DM

The Client's primary surface is the communication layer. The agent's ACP
conversation renders as a **DM channel** to its owning user, with a first-class
**`ask`** (`../compass.md:139`), **tool-call rendering**, diffs, and plans — the
ACP-native-UI rendering of `../compass.md:294-296` moving onto the channel. The
existing SolidJS shell, its Orca-mirror layout, and its workstream/agent state
model (`../compass-ade-shell/design.md:1-9,32-34`) are reused; the dock-in-sidebar
record already moved the agent conversation into a first-class, full-height
sidebar tab beside the board (`../compass-dock-in-sidebar/design.md:26-30`), which
this pivot builds on. `DM → group DM` is the session-sharing mechanism
(`../compass.md:425`).

Rationale: routing the agent conversation through the same channel surface humans
use is what makes "@agent do X" collaboration and audit/search fall out of one
substrate rather than two.

**Refines** the shell records (`../compass-ade-shell/design.md`,
`../compass-dock-in-sidebar/design.md`): the swimlane/state-model work is reusable;
the interaction center of gravity moves to the communication layer.

### D6 — All agent state (transcripts + centralized config) lives in the Server; runners/containers are throwaway

The Server DB holds **full agent transcripts** and a **centralized store of all
agentic config** (skills, extensions, hooks, MCP config, auth config, settings).
On (re)start, the Server sends the transcript into the container; config is
served to agents (a **read-only mounted volume**, materialized Runner-locally by pull — D11) with an
**update-notification path** so an agent learns when a new skill or `AGENTS.md`
rule lands. Consequence: **runners and agent containers are throwaway** — stopping
an agent is free, and restarting or relocating it (to a bigger machine, another
Runner) loses no context.

Rationale: the base agent is assumed to update frequently; long-lived containers otherwise have
no clean way to pull updates and no recovery from a crash. Centralizing state
turns a hung agent/container/host from data loss into a cheap restart, and unlocks
Runner placement flexibility (D3).

**Supersedes/refines** v0.3 §5.3 per-agent containers (`../compass.md:141-149`):
the container model (rootless-podman, clone-per-container, scoped creds) is kept,
but its *host* moves to the Runner and its *state* moves to the Server, making the
container disposable rather than the seat of the agent's context.

### D7 — Browser-first; the Tauri app is deferred (Runner binary still built)

The MVP is a self-hosted Compass Server reachable in the **browser** (a pure
gRPC-Web client, already supported today per `../compass-tauri-shell.md:114-115`).
The **Tauri app** — which would package the Server + Runner as daemon-like
processes for the one-machine ADE case — is **deferred post-MVP**. The **Runner
binary and its Server connection are still built** in the MVP (D3).

Rationale: the browser client removes desktop-packaging work from the critical
path while the Server/Runner split is the load-bearing architecture; the Tauri
app is a distribution convenience that layers on later.

**Defers** the Tauri thin-shell workstream (SEA-1022, `../compass-tauri-shell.md:3`)
to post-MVP. The transport seam it documents (`../compass-tauri-shell.md:107-121`)
is what v0.5 leans on, so deferring the app costs nothing architecturally.

### D8 — Warden is deferred

The seal-hosted Warden security auditor — v0.4 scoped seal to "exactly what
hosting Warden requires" (`../compass-0.4/design.md:94-111`) — is **deferred**.
v0.5 builds the orchestration + productivity product first; the per-agent
container remains the structural sandbox (`../compass.md:141-149`) in the interim.

Rationale: Warden is the security moat and stays a named differentiator, but the
collaboration/orchestration product is the MVP wedge; sequencing Warden after it
keeps the MVP focused without abandoning the moat.

**Defers** v0.4's seal→Warden scoping (`../compass-0.4/design.md:94-111`); nothing
in the MVP depends on Warden shipping first.

### D9 — Users and agents are first-class accounts; agents are owned, permissioned subtypes; forge identity is configurable

The comms identity model (D1) has **two account classes**:

- **Users** — human accounts with a standard permission model: an **Admin** role
  and a **regular user** role to start, expandable later. Users own agents and may
  manage other users (the management hierarchy the communication-layer spine needs).
- **Agents** — a **separate account class**, each with an explicit **owning user**.
  An agent is permissioned like a user (which channels it can post to, and which it
  can see/subscribe to) but constrained by its owner: users have **first-class
  controls over what access their agents have**. So an agent account is a
  constrained, owned subtype — a real peer account in channels, gated by owner-set
  permissions rather than a fully autonomous peer.

**Spaces / nested channels.** The channel namespace nests, so a user's owned agents
work in that user's space by default (e.g. `#matt.announcements`,
`#matt.coordination`) while cross-agent and cross-team channels stay available for
wider collaboration. This scopes an owner's fleet without walling it off from
shared channels.

**Forge identity is configurable, not fixed.** How an agent's comms account relates
to its **forge machine-user identity** (`../compass.md:149`) depends on the forge:

- **Forgejo (first-class):** each agent gets **its own forge identity**, created by
  Compass when the agent is spun up — a first-class Forgejo integration provisions
  the machine user per agent.
- **Per-seat forges (e.g. GitHub):** a per-agent forge identity does not scale to
  per-seat pricing, so the **user selects which forge account/credentials the agent
  uses**. Forge identity is therefore a **configurable per-agent setting**, not a
  hardcoded one-identity-per-agent mapping.

Rationale: users owning agents with explicit, first-class access controls is the
trust model the communication-layer product depends on; making forge identity
configurable keeps the per-seat-pricing reality of hosted forges from forcing an
unaffordable identity-per-agent model, while first-class Forgejo provisioning gives
the self-hosted path the clean per-agent identity v0.3 assumed.

**Resolves Q2.** Refines v0.3's per-agent forge machine-user (`../compass.md:149`):
kept as the Forgejo default, generalized to a configurable forge identity for
per-seat forges.

### D10 — Runner↔Server transport is gRPC, authenticated by a per-Runner provisioned token; the container↔UI ACP transport also rides gRPC

The Runner connects out to the Server (D3) over **gRPC** — the same protocol family
as the Client↔Server `compass.v1` seam (D2), a second gRPC surface rather than a
distinct protocol, absent a specific reason to diverge.

**Auth is a per-Runner provisioned token.** The Server provisions **one token per
Runner**; the Runner authenticates with it on connect. This deliberately rejects
the **per-agent / multi-token** model some CI systems use (a token minted per
job/agent) — that pattern is a code smell here: it multiplies credential surface
and lifecycle for no isolation gain, since the Runner (not the Server) is the trust
boundary for the containers it hosts. One durable token per Runner, enrolled once,
is the model.

**The container↔UI ACP transport also rides gRPC.** Streaming an agent's ACP
activity (tool calls, diffs, plans, terminal output) from its container out to the
Client is a transport that must exist regardless. Rather than run it as a separate
JSON/HTTP channel, it is carried **over gRPC**, folded onto the same contract
surface — because an adapter from ACP into the channel event stream (D5) has to be
built either way, and one gRPC transport is cleaner to maintain than a second HTTP
one.

Rationale: a single transport technology (gRPC) across Client↔Server,
Runner↔Server, and container↔UI keeps one contract-generation + CI-drift discipline
(`../compass.md:284-286`) instead of a gRPC/HTTP split; a per-Runner token
minimizes credential lifecycle without weakening the container trust boundary.

**Resolves Q3.** Realizes the Runner's outbound connection named in D3.

### D11 — Config distribution is Runner-mediated pull into a Runner-local read-only mount; agent binary and base image ride versioned OCI pulls

Centralized agentic config (D6) reaches agents by **Runner-mediated pull**, not a
cross-host network volume:

- The **Server is the config store of record** (D12), holding each agent's config
  **versioned** (content-addressed per agent).
- The **Runner pulls** a hosted agent's config bundle over the connection it
  already holds (the gRPC Runner↔Server transport, D10) and materializes it as a
  **Runner-local read-only bind mount** into the container. The agent still just
  reads its config off a path — D6's read-only-mount model holds — with **no
  cross-host network filesystem**.
- **Change propagation:** the Server signals "config version N for agent X" over
  the Runner↔Server stream (D6's update-notification path); the Runner pulls the new
  version and **atomically swaps the mount**, and the agent's existing notification
  path tells the running process to re-read.
- **Agent binary + base image** — the heavy, frequently-updated artifacts (the base
  agent updates on the order of dozens of times a day, D6) — ride **versioned OCI
  image pulls** (the container is already an OCI image built from the project's
  devenv, `../compass.md:141-149,157-163`), applied via the **throwaway-container
  restart** property (D6): stop, restart on the new image, transcript replayed. A
  frequent bump costs nothing beyond a restart.

Rationale: a read-only *network* volume reachable from every Runner would force a
shared network filesystem onto arbitrary Runner hosts — breaking the "drop a binary
on any machine, like a CI runner" property (D3) — and does not fit config that is
**per-agent** (per-agent auth/MCP credentials). Pull-on-notification reuses the D10
connection and the D6 notification path already being built and keeps Server-side
connection state minimal, versus a Server-push model that makes the Server track and
fan out to every live agent.

**Resolves Q4.** Refines D6's "likely a read-only mounted volume": the mount is
kept, materialized **Runner-locally by pull** rather than as a network volume.

### D12 — Postgres is the store of record; transcript bodies live in S3-compatible object storage behind a blob seam; JetStream is comms-only

The Server's datastore is **PostgreSQL**, and it is the **system of record** for
all structured state: accounts, channels, config, board/issue state, and the
**transcript index/metadata**. **JetStream is treated as the communication layer
only** — a durable message bus, not long-term storage — so it is never the store of
record; the audit/search property (D1) is served from the canonical Postgres store,
keeping restart/replay idempotent against one authority.

**Transcript bodies go to object storage, behind a Compass-owned blob seam.**
Transcripts are large and append-heavy, so their **bodies** live in **S3-compatible
object storage** (keyed, indexed from Postgres), not in Postgres rows. The design
depends on the **S3 API behind a thin blob-store seam** — the same swappable-seam
discipline D1 uses for comms — so the backend is a deployment choice:

- **Hosted (seal):** Cloudflare **R2** (S3-compatible).
- **Self-hosted default:** **SeaweedFS** — Apache-2.0 (embeds/distributes with no
  copyleft obligation), mature, production-proven, strong small-object I/O.
- **Any S3-compatible backend** (AWS S3, Ceph RGW, Garage, …) drops in behind the
  same seam at the operator's choice.

Old transcripts are **not dropped** — retained in object storage as useful data;
volume-reduction (compaction, tiering) is a later optimization needing no schema
change.

Rationale: Postgres-as-record with object-storage bodies keeps the DB lean and makes
indefinite transcript retention cheap. Depending on the **S3 API**, not a vendor, is
what keeps Compass **self-hostable** — a hard product constraint; SeaweedFS is the
bundled self-hosted default specifically because it is Apache-2.0 (bundling an
AGPL-licensed store, or the now-archived MinIO community edition, would attach
copyleft/licensing strings to a **distributed** product).

**Resolves Q5 and Q10.** Q10's store-of-record question resolves to Postgres; Q5's
datastore resolves to Postgres + the S3-compatible blob seam. Couples to D1
(JetStream stays comms-only) and D6 (the state this store holds).

### D13 — The Supervisor (renamed from v0.3's Dispatcher) ships in the MVP with agent task-assignment; conflict map and backlog automation layer on later

**Naming:** v0.3's **Dispatcher** (`../compass.md:69-81`) is **renamed the
Supervisor** — the built-in supervisor agent that coordinates worker agents. This
record uses **Supervisor** in v0.5 prose; citations of the frozen v0.3/v0.4 records
keep their original "Dispatcher" wording (those records are not rewritten).

**MVP depth (the D4 scoping call):** the MVP Supervisor ships the ability to
**assign tasks/issues to worker agents** — the cheap, proven primitive (a supervisor
agent sending an assignment message to a worker over the communication layer,
exactly how agent fleets are supervised today). The **Bridge** board
(`../compass.md:91-98`) ships alongside as the Server-side projection of agents'
workstream state.

**Deferred to post-MVP** (layered onto this foundation, not in the first cut):

- the **conflict map** (advisory file-zone scheduling, `../compass.md:74`),
- **automatic assignment** and **backlog auto-pickup** (`../compass.md:73,75`).

Rationale: task-assignment over channels is nearly free (it is a message to a worker
agent, the pattern already in daily use) so it belongs in the MVP; the conflict map
and auto-assignment are genuine additional systems whose value comes after the
multi-agent loop is running, so they sequence after without blocking the MVP.

**Resolves Q8.** Refines D4: D4 puts the Supervisor + Bridge in the MVP; D13 fixes
the MVP depth (assignment yes; conflict-map/auto-assign later) and records the
rename.

### D14 — The centralized config store enforces a defined secret-handling boundary, upgradable over time

The centralized config/secret store (D6) serving disposable containers (T5) carries
a defined secret-handling boundary:

- **Encryption at rest** for stored secrets.
- **Per-user / per-agent authorization** on secret access (an agent reaches only its
  own scoped credentials, under D9's owner-gated access).
- **Runner/container isolation** of delivered secrets — a secret materialized into
  one agent's container (via the D11 read-only mount) is not visible to another,
  extending v0.3's scoped-`$HOME` container isolation (`../compass.md:143,149`).
- **Rotation and revocation** paths for stored credentials.
- **Redaction of credentials** from transcripts and the audit log, so the
  everything-through-the-comms-layer property (D1) never leaks secrets into
  searchable history.

This boundary is the **target contract for T5**; the concrete hardening (cipher
choice, key custody, rotation cadence) is an implementation detail that **does not
change the topology and can be strengthened over time** without a design change.

Rationale: centralizing config (D6) concentrates secret risk, so the boundary must
be named for the store task to build against; fixing the boundary while leaving the
hardening specifics upgradable avoids over-specifying crypto in a design record
while still giving T5 a clear security contract.

**Resolves Q11.** Downstream of D11 (config distribution) and D6 (centralized
store); extends v0.3 container isolation (`../compass.md:143,149`).

## Open Questions

> The load-bearing questions are resolved and folded in as Decisions (see the
> Status banner's map); each is kept below as a one-line pointer to its Decision.
> What stays live is **non-load-bearing** only — deferrals the merge ratifies (the
> design is correct without them; each is an optional refinement settled at its
> task).

- **Q1 — [Resolved → D1] Comms substrate.** First-party Rust comms service on
  NATS/JetStream, Cotal as reference. See D1.
- **Q2 — [Resolved → D9] Account / identity + management hierarchy.** See D9.
- **Q3 — [Resolved → D10] Runner↔Server transport + auth.** See D10.
- **Q4 — [Resolved → D11] Config-distribution mechanism.** See D11.
- **Q5 — [Resolved → D12] Server datastore + transcript storage.** See D12.
- **Q8 — [Resolved → D13] Multi-agent orchestration depth.** See D13.
- **Q10 — [Resolved → D12] Store of record for transcripts + messages.** See D12.
- **Q11 — [Resolved → D14] Secret-handling boundary.** See D14.

- **Q6 — How much of the existing SolidJS shell state model survives the comms
  pivot?** *(non-load-bearing)*
  D5 asserts the swimlane/state-model work (`../compass-ade-shell/design.md`,
  `../compass-dock-in-sidebar/design.md`) is reusable. The exact re-mapping of the
  agent-conversation sidebar tab onto a channel DM — rendered Slack-style as a DM
  entry in the left sidebar that opens the ACP surface (split panes + terminal
  tabs) on click — is a UI-task detail settled during that task; the design is
  correct either way.

- **Q7 — Transcript hand-off format into a (re)started container.** *(non-load-bearing)*
  D6 requires the Server to send the transcript into a container on restart. The
  serialization/replay format is an implementation detail of the state-store task
  (v1: write it into the base agent's session directory; a later extension may feed
  the transcript to the agent over ACP directly), not a cross-cutting contract;
  deferrable to that task.

- **Q9 — Managed-tier / hyperscaler Runner shape.** *(non-load-bearing)*
  Future Runners target hyperscaler infra (per-agent ECS-style containers). The
  managed-Runner design is post-MVP and deferred; the MVP Runner is a single local
  binary.

- **Q12 — Port-forwarding an agent-container dev server to the user.** *(non-load-bearing)*
  A user viewing a dev server running inside an agent's container on their own
  machine needs a port-forward path from the container out through the Runner/Server
  to the Client. The concrete mechanism (a tunnel over the existing Runner↔Server
  connection vs. a directly forwarded port) is a UI/transport detail resolvable at
  the Runner/UI tasks; it does not change the topology, so it is deferred.

## Plan

This PR lands the v0.5 design record alone; it makes no living-spec edit — the
forward-looking overview edit to `docs/specs/product/compass.md` is deferred to
task **T7** (see **Spec impact**). Because `docs/designs/<domain>/` records
are frozen once decided (`../../platform/docs-system.md:31-33`,
`../compass-0.4/design.md:191-193`), the pivot is captured *here* as a new record;
prior records are superseded by citation, never rewritten. The load-bearing Open
Questions are resolved and folded in as Decisions (Q2 → D9, Q3 → D10, Q4 → D11,
Q5/Q10 → D12, Q8 → D13, Q11 → D14); only non-load-bearing deferrals (Q6, Q7, Q9,
Q12) remain, and the merge ratifies them.

Tasks are ordered by dependency: comms substrate + seam → Server → Runner → UI →
state/config store → multi-agent orchestration. Each carries its own test/gate cycle.

## Global Constraints

- **Frozen-record convention.** New record at
  `docs/designs/product/compass-0.5/design.md`. Never rewrite v0.3
  (`../compass.md`), v0.4 (`../compass-0.4/design.md`), or the shell/Tauri records;
  supersede by citation (`../../platform/docs-system.md:31-33`,
  `../compass-0.4/design.md:191-193`).
- **Tracker = Linear, team SEA** (not GitHub Issues). Reference SEA-NNN;
  SEA-1115 (Cotal adoption, In Review) is reframed to the first-party comms
  build and SEA-1113 (Cotal trust-tier gate) no longer gates a Compass
  dependency (D1); SEA-1022 (Tauri shell) is deferred by D7.
- **No persona / agent-product names** in this record (it lives in the repo; per
  `AGENTS.md`). OMP, ACP, Tauri, the forge, NATS/JetStream, and Cotal are
  interop facts and fine to name; Cotal is named as the reference implementation
  the first-party comms service is built against (D1).
- **The comms seam is Compass-owned and thin** (D1): the first-party comms
  service sits behind it, mirroring v0.4's Cotal seam
  (`../compass-0.4/design.md:113-122`); the substrate stays swappable.
- **`compass.v1` evolves, not replaced** (D2, `../compass.md:278-286`): the
  Client↔Server seam is the existing contract extended to networked multi-user; a
  generated client stays the only sanctioned door.
- **`rule://planning-evidence`.** Every claim about existing code/design carries a
  file+line + quoted snippet verified in this repo.
- **The living spec states only built behavior** as `### Requirement:` +
  `#### Scenario:` contracts (`../compass-0.4/design.md:194-197`); the pivot is
  unbuilt, so it appears in the spec only as a design-record pointer + a
  forward-looking overview edit — no fabricated contracts.
- **markdownlint-clean** (`.markdownlint.json` / `.markdownlint-cli2.jsonc` at
  repo root); the design skill requires it.

## Tasks

- [ ] **T1 — Comms substrate + Compass-owned comms seam.** Define the thin
      Compass-owned comms interface (accounts, channels, DM/group-DM, messages,
      ask/tool-call rendering hooks, audit/search) and build the first-party
      Rust comms service on NATS/JetStream behind it, so the substrate stays
      swappable (D1); consumes the **D9** account model (users + owned,
      permissioned agent subtypes).
      *Interfaces:* consumes the D9 account model + the JetStream substrate;
      produces the comms seam interface definition and the first-party service
      behind it. Test/gate: seam contract tests green against the comms service:
      CRUD flows (account create, DM open, message post, group-DM promote, audit
      query), plus — per the D9 account model — authorization + isolation
      (owner/manager permissions, rejection of unauthorized account/channel
      access, group-DM share authorization, audit-visibility boundaries).

- [ ] **T2 — Server tier: networked multi-user, evolve `compass.v1`.** Promote the
      daemon (`../compass.md:270-276`) into a network Server: add the
      authenticated TLS network listener + client transport-mode selector the
      reserved seam names (`../compass-tauri-shell.md:107-121`, D3), and evolve
      `compass.v1` (`../compass.md:278-286`, D2) to carry multi-user accounts,
      channels, and the ACP-as-DM event stream.
      *Interfaces:* consumes the T1 comms seam + the existing `compass.v1` crate;
      produces the evolved `compass.v1` schema (regenerated, CI-verified per
      `../compass.md:284`) + the Server's authenticated listener. Test/gate:
      `compass.v1` contract-drift check green; multi-client connect + event
      resubscribe (sequenced, `../compass.md:282`) tests green.

- [ ] **T3 — Runner binary + Server connection.** Build the Runner binary that
      manages per-agent containers (the clone-per-container rootless-podman model,
      `../compass.md:141-149`), connects out to the Server, and streams container
      activity back. Uses the **D10** Runner↔Server transport (gRPC, per-Runner
      provisioned token; ACP streamed over gRPC).
      *Interfaces:* consumes the D10 transport/auth + the Server listener
      (T2); produces the Runner binary + its enrollment/stream path. Test/gate:
      Runner enrolls with the Server, starts a container, and streams activity end
      to end in an integration test.

- [ ] **T4 — Communication-layer UI (ACP-as-DM).** Repivot the SolidJS Client
      (`../compass-ade-shell/design.md`, `../compass-dock-in-sidebar/design.md`) so
      the communication layer is the primary surface: render the agent's ACP
      conversation as a DM channel with first-class `ask` (`../compass.md:139`),
      tool-call rendering, diffs, and plans (`../compass.md:294-296`), and the
      DM→group-DM sharing action (`../compass.md:425`), reusing the
      swimlane/state-model work (D5).
      *Interfaces:* consumes the evolved `compass.v1` channel/event surface (T2) +
      the existing shell; produces the comms-centric Client with ACP-as-DM
      rendering. Test/gate: DM renders a live ACP session (ask + tool-call +
      diff), group-DM promotion visible to a second user (component/E2E tests).

- [ ] **T5 — Agent state/config store + throwaway-container lifecycle.** Implement
      the Server-side store for full transcripts + centralized agentic config
      (skills/extensions/hooks/MCP/auth/settings), the config-distribution
      mechanism (Runner-mediated pull into a read-only mount, D11), and the
      throwaway-container lifecycle (stop free; restart/relocate replays transcript
      into a fresh container). Consumes **D11** (config distribution), **D12**
      (Postgres store of record + S3-compatible transcript blob seam), and **D14**
      (secret-handling boundary).
      *Interfaces:* consumes D11/D12/D14 + the Runner container lifecycle
      (T3); produces the Server state/config store + the transcript replay-into-
      container path. Test/gate: stop→restart an agent on a different Runner with no
      context loss; a config update notifies a running agent (integration test).

- [ ] **T6 — Multi-agent orchestration: Supervisor + Bridge board.**
      Layer multi-agent orchestration onto the single-agent foundation (D4): a
      **supervisor agent account** (the Supervisor — v0.3's Dispatcher,
      `../compass.md:69-81` — per D13) coordinating **worker agent accounts** over
      channels, and the **Bridge** swimlane board (`../compass.md:91-98`) as a
      Server-side projection of the agents' workstream state. Depends on the
      single-agent path (T1–T5) working end to end; scoped by **D13** (MVP ships
      task-assignment; conflict-map + auto-assign deferred).
      *Interfaces:* consumes the comms seam (T1), the multi-user Server + channel
      event surface (T2), the Runner's multi-container capability (T3), and the
      agent-state store (T5); produces the supervisor-agent coordination path + the
      Bridge board surface. Test/gate: a supervisor agent assigns work to two
      worker agents over channels and the board reflects their live workstream
      state (integration/E2E test).

- [ ] **T7 — Reconcile the living spec.** Update `docs/specs/product/compass.md`:
      point its design-record cross-reference at this v0.5 record and adjust the
      forward-looking overview so the not-yet-built runtime description reflects the
      Client→Server→Runner topology + the communication-layer spine — without
      inventing Requirement/Scenario contracts for unbuilt behavior
      (`../compass-0.4/design.md:194-197`). See **Spec impact**.
      *Interfaces:* consumes this record; produces the spec overview/cross-ref edit.
      Test/gate: `spec-impact` CI check satisfied (`../../platform/docs-system.md:123-128`);
      markdownlint clean.

- [ ] **T8 — Design record markdownlint-clean.** This file lints clean under the
      repo config (`.markdownlint.json` / `.markdownlint-cli2.jsonc`). *(This file.)*

> **Explicitly deferred** (not tasks in this MVP): **Warden**
> (`../compass-0.4/design.md:94-111`), deferred by D8; and the desktop Tauri app
> (SEA-1022, `../compass-tauri-shell.md:3`), deferred by D7 — the Runner binary
> still ships. A memory backend for all agents on the Server is noted post-MVP and
> not designed here.

## Spec impact

**Spec-impact for this PR: none.** This PR adds only the design record; it changes
no living spec. The v0.5 pivot is entirely unbuilt, so it creates no new
`### Requirement:` / `#### Scenario:` contracts — the living spec states only
**built** behavior (`../compass-0.4/design.md:194-197`), so there is nothing to
reconcile until the first increment ships.

When implementation begins, task T7 makes the forward-looking spec edit: repoint
`docs/specs/product/compass.md`'s design-record cross-reference to include this
v0.5 record (today it points at v0.3 and v0.4,
`../../../specs/product/compass.md:6-8`) and adjust its overview
(`../../../specs/product/compass.md:15-31`) so the "designed but not yet built"
description reflects the Client→Server→Runner topology and the communication-layer
spine rather than the single daemon — no fabricated contracts for unbuilt behavior
(the docs-system gate, `../../platform/docs-system.md:114-131`).
