# Compass v0.5 Server tier — the networked, multi-user `compass.v1` door (T2)

Status: Historical

> Internal design record — July 2026. A **child record refining task T2** of the
> frozen v0.5 pivot record
> ([`../compass-0.5/design.md`](../compass-0.5/design.md), task lines 663-673):
> "Promote the daemon (`../compass.md:270-276`) into a network Server: add the
> authenticated TLS network listener + client transport-mode selector the
> reserved seam names (`../compass-tauri-shell.md:107-121`, D3), and evolve
> `compass.v1` (`../compass.md:278-286`, D2)". It decomposes that one task into
> an executable contract; it does **not** rewrite the parent or any earlier
> frozen record (records freeze on merge; supersede by citation only).
>
> Code claims are grounded against the working tree at branch
> `compass-sea-1195-comms-server` (T1 landed on it, PR #535, now merged) and,
> for the contract, against **PR #533 head `8ec7dcf5`** — cited below as "the
> contract tip". **#533 has since merged (`5eb5a063`), freezing the contract**;
> the body's tip-shaped citations and their "binds at freeze" caveats resolve to
> the frozen schema, with the one shape delta the freeze introduced (the
> console→workspace rename) recorded in Global Constraints and OQ4.

## Problem / Intent

The daemon serves `compass.v1` only over an owner-restricted Unix socket —
"bind the `compass.v1` service to a Unix domain socket … The socket serves both
native gRPC (HTTP/2) and gRPC-Web (HTTP/1.1) … no localhost TCP on the shipped
path" (`crates/compass-daemon/src/serve.rs:1-4`), with the socket as
"the daemon's whole trust boundary on the local machine"
(`serve.rs:95-96`) — while the T1 communication layer
(`crates/compass-comms`, PR #535) is a library with no wire surface
at all. T2 promotes the daemon into the **Server tier**: it wires the T1 `Comms`
service to the generated `CommsService` gRPC trait, and adds the authenticated
TLS network listener the hosted-mode seam reserved — "the daemon has no
authenticated network listener today (UDS + dev-loopback only), so hosted mode
needs a TLS+auth server transport on the daemon plus a client-side
transport-mode selector" (`../compass-tauri-shell.md:116-119`) — so multiple
authenticated Clients reach one Server over the network (D2, D3).

## Approach

The Server is the **daemon promoted in place**, not a new architecture — the
parent already frames it so: "This is the daemon of `../compass.md:274` promoted
to a network service — realizing the reserved hosted-mode transport …, not a new
architecture" (`../compass-0.5/design.md:86-88`). Five design forks were live;
each is decided below with its grounding.

### F1 — Server crate location: promote `compass-daemon` in place

**Decision: extend `compass-daemon`; no new `compass-server` crate in T2.**

What the daemon crate already owns is exactly the scaffolding T2 reuses:

- The serve loop with eager bind, single-instance socket probing, 0600
  permissions, inode-checked cleanup, and a shutdown watch fanned out to every
  server task and every open stream (`serve.rs:50-227` — the `select!` racing
  the UDS task, the optional dev-TCP task, and the external shutdown at
  `serve.rs:175-202`).
- The sequenced event bus — "a monotonic-`seq` ring buffer plus a broadcast
  live-tail … snapshot the ring at `since_seq = 0`, replay only what follows a
  cursor, and signal a resync when the cursor predates the ring"
  (`crates/compass-daemon/src/events.rs:1-5`) — plus the
  `forward()` replay-then-tail stream driver selecting on shutdown
  (`src/service.rs:183-229`).
- The gRPC-Web stacking (`GrpcWebLayer` over the same tonic services,
  `serve.rs:132-150`).

Alternatives considered:

1. **New `compass-server` crate depending on `compass-comms` + reusing daemon
   scaffolding.** Rejected for T2. The daemon still owns the single-host
   container/session lifecycle (`src/runtime/` — podman, images, egress,
   workspaces, ACP sessions; `src/runtime/mod.rs:1-19`), which D3 splits onto
   the Runner only at T3 ("Supersedes/refines v0.3 §7.1's single-host daemon …
   by splitting container hosting onto the Runner",
   `../compass-0.5/design.md:253-255`). A new crate now must either duplicate
   the serve/event scaffolding or move it out from under a crate that still
   needs it — churn against a boundary T3 will move anyway. (Two weaker reasons
   were considered and are *not* load-bearing: `compass-shell` dev-depends on
   `compass-daemon` (`compass-shell/Cargo.toml:48`), but a dev-dependency does
   not link the server, so it would not block a library `compass-server`; and
   moon's "no application project `dependsOn` another" rule
   (`../compass-tauri-shell.md:102-103`) bites only an *application* project,
   whereas a `CommsGrpc`/serve library crate is library code. The decision rests
   on the scaffolding co-location + the T3-rename point below, not on these.)
2. **Rename the crate/binary to `compass-server` now.** Rejected as
   churn: the promotion is behavioral, and T3 — which physically removes
   `runtime/` onto the Runner — is the natural point to settle naming, when the
   crate's remaining responsibility set is final.

**Migration implication:** T3 extracts `src/runtime/` into the Runner binary and
may then rename what remains; nothing in T2 hard-codes against that split. New
T2 modules (`auth.rs`, `comms_grpc.rs`) depend on `compass-comms` +
`compass-proto` only, so they move (or stay) wholesale.

### F2 — The authenticated network listener: rustls via tonic `tls-ring`, operator-provisioned certs, bearer-token accounts

**Decision: terminate TLS in-process with tonic's rustls stack; authenticate
each Client RPC with a per-user bearer token resolved to a D9 account.**

**TLS stack.** The seal tree is already a rustls shop: `seal-daemon` pins
`rustls = { version = "0.23", default-features = false, features = ["ring", …] }`
with the comment "rustls process-level crypto provider needs to be installed
from main before the runtime's TLS stack tries to construct a ServerConfig /
ClientConfig" (`oss/seal/crates/seal-daemon/Cargo.toml:40-46`), installs it at
startup — `rustls::crypto::ring::default_provider().install_default()`
(`seal-daemon/src/main.rs:39-41`) — and `seal-runtime` already carries
`rcgen = "0.14"` + `tokio-rustls = { version = "0.26", … features = ["ring", …] }`
as dev-dependencies for TLS-exercising tests
(`oss/seal/crates/seal-runtime/Cargo.toml:82-86`). tonic 0.14 (the workspace
pin, root `Cargo.toml:22`) ships the matching server surface behind its
`tls-ring` feature (`tls-ring = ["_tls-any", "tokio-rustls/ring"]`,
tonic-0.14.6 `Cargo.toml`):

- `ServerTlsConfig` with `identity(Identity)`, `client_ca_root(Certificate)`,
  `client_auth_optional(bool)` (tonic-0.14.6
  `src/transport/server/tls.rs:8-56`), applied via
  `Server::builder().tls_config(cfg)` (`src/transport/server/mod.rs:166`).
- TLS applies to a **custom incoming stream** — `serve_internal` wraps the
  incoming connections with the configured acceptor
  (`src/transport/server/mod.rs:786-790`), so the daemon's existing
  `serve_with_incoming_shutdown` pattern (`serve.rs:145-147`) carries over
  unchanged to a TLS TCP listener.
- The acceptor advertises ALPN `h2` only
  (`src/transport/server/service/tls.rs:58`), so the network door is
  HTTP/2-native; gRPC-Web browser clients negotiate h2 via ALPN and work over
  the same port (`GrpcWebLayer` is protocol-version-agnostic; the daemon
  already stacks it, `serve.rs:141-144`).

**Cert provisioning.** Operator-provided PEM paths — `--listen <addr>
--tls-cert <cert.pem> --tls-key <key.pem>`, loaded with
`Identity::from_pem(cert, key)` (tonic-0.14.6 `src/transport/tls.rs:55-59`).
This is the standard self-hosted posture (D12's rationale makes self-hostable a
hard product constraint, `../compass-0.5/design.md:489-491`); operators bring a
Let's-Encrypt/internal-CA cert or front the Server with their own re-encrypting
proxy. A dev/test convenience path mints a self-signed pair with `rcgen`
(dev-dependency only, mirroring the `seal-runtime` precedent above) for the
integration tests. There is **no plaintext network listener**: a bearer token
over cleartext is credential disclosure, so TLS flags are required whenever
`--listen` is given (the loopback `--dev-http` endpoint stays, unchanged, for
browser dev — `serve.rs:152-169`). ACME automation is deferred (out of MVP
scope; an operator concern the PEM seam already accommodates).

**Client authentication → D9 account.** The contract tip pins the model:
"the caller is the account authenticated on the connection (the Server's TLS +
token transport, D3/D10) — never a field in a request, which would be
spoofable" (contract tip
`crates/compass-proto/proto/compass/v1/comms.proto:31-33`). mTLS
client certificates are rejected for Client↔Server: the MVP Client is a
**browser** (parent D7), and the browser gRPC-Web transport is fetch-based with
no workable client-cert story — while a bearer token rides both transports as
the `authorization` metadata/header (connect-web transports accept
`interceptors?: Interceptor[]`,
`packages/compass-client/node_modules/@connectrpc/connect-web/dist/esm/grpc-web-transport.d.ts:32`).
So:

- The Server keeps a **token store**: SHA-256 hashes of issued tokens →
  `AccountId` (`sha2`/`base64` are already workspace deps, root
  `Cargo.toml:85-86`; `AccountId` wraps a UUID,
  `crates/compass-comms/src/account.rs:16-19`). Tokens are
  32 random bytes, base64url-encoded, shown once at issuance; only hashes are
  stored. Like the T1 account state it maps onto, the store is in-memory in T2
  and swaps to the T5 Postgres store behind the same accessors (T1 already
  documents this swap: "in-memory here; the T5 store swaps Postgres in behind
  these same accessors, D12",
  `crates/compass-comms/src/service.rs:3-4`). **In-memory has two
  consequences T2 accepts explicitly** (both resolved by the T5 durable store,
  and bounded now by D12's single-host self-host posture): a Server restart
  wipes all issued tokens (operators re-bootstrap; no token outlives a restart
  in T2), and the store is **single-process** — two Server processes behind a
  load balancer would hold disjoint token/account state (a token issued on one
  is unknown on the other), so T2 is single-serving-process by construction
  until T5. Neither is a defect at MVP scope; both are named so the executor
  does not assume horizontal scale or restart-durability the store cannot give.
- A tonic **interceptor** — `Interceptor::call(&mut self, Request<()>) ->
  Result<Request<()>, Status>` (tonic-0.14.6
  `src/service/interceptor.rs:41-44`) — reads `authorization: Bearer <token>`
  from request metadata, resolves it in the token store, and injects the
  account into request extensions (`Request::extensions_mut`, tonic-0.14.6
  `src/request.rs:350`). Unknown/absent token ⇒ `Status::unauthenticated`.
  tonic 0.14's `Interceptor` is **synchronous** (`&mut self, Request<()>) ->
  Result<Request<()>, Status>`, which the in-memory hash-map lookup satisfies
  directly. **This is a real T5 seam, not a free swap:** an async Postgres
  resolve cannot run in the sync interceptor, so the "same accessors" swap (the
  token-store bullet) does *not* extend to the auth-path lookup unchanged — T5
  either fronts the durable store with a sync in-memory token cache (the
  interceptor stays sync, the cache is the accessor that swaps) or moves token
  resolution to an async Tower layer. T2 builds the sync interceptor; T5 owns
  that choice. Recorded so T5 does not discover it as a surprise re-architecture.
- **Bootstrap:** T1 already provides the no-actor bootstrap
  (`Comms::bootstrap_admin`, "Bootstrap only — there is no actor to authorize
  against when the system has no accounts yet",
  `compass-comms/src/service.rs:66-69`). On start with `--listen`, the Server
  creates the bootstrap admin (handle from `--admin-handle`, default `admin`),
  issues its token, and writes it **0600 under the daemon state dir — never to
  stdout or a centralized log**, since a logged bearer credential lets anyone
  who can read process output or aggregated logs impersonate the admin. The
  operator reads it from that file; an explicit one-time interactive display
  (behind a flag, with a documented "this is a live credential" notice) is the
  only sanctioned way to surface it on a terminal.
- **The UDS door stays token-free.** The 0600 socket is already the local trust
  boundary (`serve.rs:95-101`); its callers are the machine owner. Comms RPCs
  arriving over the UDS are attributed to the bootstrap admin account via a
  static-identity interceptor. (Alternative — requiring tokens on the UDS too —
  rejected: it breaks the shell's zero-config local connect for no security
  gain; the socket mode is the credential.) This mirrors how per-Runner tokens
  (not per-agent) are D10's model for the other outbound surface ("one durable
  token per Runner, enrolled once", `../compass-0.5/design.md:407-413`);
  per-user tokens are the Client-side analogue.

**Both services are served on both doors.** D2 keeps one contract and one door
("one gRPC service: typed request/response commands plus a server-streaming
event channel", `../compass.md:282`; "extending `compass.v1` from a single-user
local contract to a multi-user networked one",
`../compass-0.5/design.md:230-232`) — so the network listener serves
`CompassService` (the connect-time probe `GetDaemonInfo` is "the first
round-trip a UI makes after connecting",
`packages/compass-client/src/gen/compass/v1/compass_pb.ts:668-669`)
**and** `CommsService`, both behind the auth interceptor. This exposes the
agent-session RPCs on `CompassService` (`start`/`stop`/`reload`, etc.) to any
authenticated account, but those RPCs were written for the single-user UDS
trust boundary and take no account argument, so as-is a bearer token could
drive **another** user's session. **T2 therefore gates the `CompassService`
agent-session RPCs on the network door to the bootstrap admin only** — the
interceptor rejects a non-admin account on those methods with
`Status::permission_denied`, while `GetDaemonInfo` (the connect-time probe,
`compass_pb.ts:668-669`) and all of `CommsService` stay open to any
authenticated account. This keeps the single-owner semantics the RPCs were
built with until **T3 supersedes them with the per-account Runner path**, at
which point the admin gate lifts. (The alternative — threading an account
through every session RPC now — is T3's job, not a T2 security patch.)

**CORS on the network door defaults closed.** The permissive-CORS layer is a
dev-endpoint property (`serve.rs:239-254`); the TLS door adds no CORS by
default. A browser Client served from a different origin needs
`--cors-allowed-origin <origin>` (explicit, single origin, exposing the
gRPC-Web status trailers exactly as `grpc_web_cors()` does). Serving the UI
bundle same-origin off the Server (which would remove CORS entirely) is a T4
delivery decision this record leaves open — see Open Questions.

### F3 — Transport-mode selection: construction-time, in the client package; T2 owns the doors and the factories, T4 owns the UX

**Decision: keep transport choice exactly where the shell record put it — at
client construction — and scope T2 to (a) both server-side doors and (b) the
`@compass/client` factory additions that reach them.**

The seam already exists client-side: "Transport is chosen at client
construction (`createCompassClient(transport)`, §7.2/§7.5), so topology is just
which transport the client receives. A hosted mode is a **sibling transport**"
(`../compass-tauri-shell.md:110-113`). The generated TS package exports exactly
that shape today — `createCompassClient(transport)`,
`createCompassWebClient(baseUrl)`, `createCompassClientOverFetch(fetch, baseUrl)`
(`packages/compass-client/src/index.ts:13-27,37-50`) — but has **no
`CommsService` client factory** (PR #533 adds only the generated `comms_pb.ts`;
`index.ts` is untouched at the contract tip).

T2 therefore owns:

1. `createCommsClient(transport)` + `createCommsWebClient(baseUrl, token?)` +
   `createCommsClientOverFetch(fetch, baseUrl, token?)` mirroring the existing
   factories, where `token` installs a connect `Interceptor` setting
   `authorization: Bearer <token>` on every request (the transports accept
   `interceptors`, `grpc-web-transport.d.ts:32`). The same optional `token`
   parameter is added to the two `CompassService` remote factories.
2. The invariant, unchanged: "the shell and UI must never assume 'local' beyond
   the transport boundary — no socket path or `localhost` leaks above the
   `fetch`/command seam" (`../compass-tauri-shell.md:119-121`). Nothing in the
   factories or the UI may branch on which transport it received.

T2 explicitly **defers to T4**: where the browser Client gets its server URL +
token (settings UI, storage), any connection-picker UX, and the deferred Tauri
command bridge (parent D7 defers the desktop shell entirely). There is no
"mode enum" anywhere: local vs. remote is only *which factory the caller
invokes*, which is what keeps the invariant enforceable.

### F4 — Wiring the generated `CommsService` trait to the T1 `Comms` service

**Decision: a mechanical `CommsGrpc` adapter in `compass-daemon`; missing
domain capability is added to `compass-comms` behind the same actor/authz
discipline; `SubscribeComms` rides a genericized daemon event bus with
snapshot-as-events semantics.**

T1 was built for exactly this mapping: "The domain types here are deliberately
decoupled from the generated `compass.v1` wire types … and mirror its field
names, so the T2 mapping onto the proto is near-mechanical"
(`crates/compass-comms/src/lib.rs:16-18`; the comment's "not frozen
yet" parenthetical is now historical — #533 froze the contract), and
"Every mutating and reading call takes an `actor: AccountId` — the account
performing the action — and authorizes it before touching state or the bus"
(`compass-comms/src/service.rs:6-7`).

**Actor injection.** The adapter reads the authenticated account out of request
extensions (inserted by the F2 interceptor) and passes it as the `actor` every
`Comms` method requires. No request message carries identity (contract tip
`comms.proto:31-37`), so this is the *only* identity path — a request arriving
without the extension is a server wiring bug and maps to
`Status::unauthenticated`.

**Capability mapping (at contract tip `8ec7dcf5`; binds at freeze).** The
generated trait (`crates/compass-proto/src/gen/compass/v1/compass.v1.tonic.rs:1120-1218`
at the tip) has 13 methods; T1 covers the first group directly, the second group
is added to `compass-comms` in T2:

| RPC (trait method) | Backing |
| --- | --- |
| `create_user` | `Comms::create_user(actor, handle, display_name, UserRole::Member)` (`service.rs:84-96`) — the contract's `CreateUserRequest` carries no role (`comms.proto:353-356`); elevation is deferred (`comms.proto:39-41`) |
| `create_agent` | `Comms::create_agent(actor, owner_user_id: actor, handle, display_name, harness)` (`service.rs:101-129`) — owner is server-set to the caller (`comms.proto:44-46,131-133`); T1 authorizes owner-or-admin, satisfied by construction |
| `list_accounts` | `Comms::list_accounts(actor)` (`service.rs:133-151`) |
| `post_message` | `Comms::post_message(actor, container, blocks)` (`service.rs:252-284`), extended to the contract's `container` oneof (`comms.proto:440-447`) |
| `list_messages` | **new** `Comms::list_messages(actor, container, limit, before_message_id)` — newest-first paging over the substrate's retained window (`MessagingSubstrate::history`, `compass-comms/src/substrate/mod.rs:59-66`); the T5 store makes paging complete beyond retention. `limit` is clamped to a server-side `LIMIT_MAX` (a `compass-comms` const): a larger request is served the cap, never an unbounded scan, and `limit = 0` maps to the default page size. |
| `list_channels` / `create_channel_group` / `list_channel_groups` | **new** visibility-scoped reads + group CRUD on `Comms`, per the frozen group model (`comms.proto:140-167`) |
| `open_agent_console` / `share_agent_console` | **new** — the contract's console replaces T1's agent-DM shape (`open_dm`/`promote_to_group_dm`, `service.rs:159-245`); the console keeps the same substrate mechanics (one subject per container, `channel_subject`-style, `service.rs:32-36`) with `participant_user_ids` authorization (`comms.proto:193-209`) |
| `respond_to_ask` | **new** `Comms::respond_to_ask(actor, ask_id, chosen_option_ids)` — records the answer on the message's `Ask` block and emits `MessageUpdated`; delivering the answer into the asking agent's session is the Runner/ACP bridge (T3), out of T2 scope |
| `search_messages` | **new** `Comms::search_messages(actor, query, scope, limit)` — v1 is a substring scan over the caller-visible containers' retained history; the D12 Postgres store (T5) upgrades it to real FTS with no contract change. `limit` is clamped to the same server-side `LIMIT_MAX`, so an unbounded scan is not reachable from the wire. |
| `subscribe_comms` | the comms event bus, below |

**Error mapping.** T1's error split was designed for this edge: "The variants
split cleanly into client errors (unauthorized/not-found/invalid) and internal
errors (substrate), which is exactly the split the T2 gRPC layer needs to map
onto `tonic::Status` codes" (`compass-comms/src/error.rs:31-34`). Mapping:
`Unauthorized → permission_denied`, `AccountNotFound`/`ChannelNotFound`/
`AskNotFound → not_found`, `Invalid → invalid_argument`,
`Substrate → internal` (with the detail logged, not leaked).

**`SubscribeComms`: one caller-scoped stream over a genericized bus.** The
contract stream is a *single* stream per caller carrying every visible comms
event with one `seq` cursor + `instance_epoch`
(`comms.proto:288-312,478-485`), while the T1 substrate subscribes
**per-subject** (`MessagingSubstrate::subscribe(&subject, since_seq, req_epoch)`,
`substrate/mod.rs:73-78`). Fanning per-channel subscriptions into one stream
cannot produce a single monotonic cursor, so the design is:

1. **Genericize the daemon event bus** (`events.rs`) from its hard-coded
   `SubscribeEventsResponse` payload to `EventBus<P>` stamping a
   `Stamped<P> { seq, at_unix_ms, instance_epoch, payload }` envelope — the
   ring/broadcast mechanics, the under-lock replay/live handoff
   (`events.rs:104-157`), the `epoch_nonce()` per-boot epoch
   (`events.rs:174-188`), and the `BufferUnderflow` rules are unchanged. The
   comms and daemon envelopes are field-identical by construction (the contract
   "mirrors compass.proto's SubscribeEvents replay model exactly",
   `comms.proto:288-291`), so both streams map `Stamped<P>` onto their response
   message at the stream edge.
2. The Server holds one
   `EventBus<subscribe_comms_response::Payload>`; the `CommsGrpc` adapter
   publishes a projection event after each successful mutation
   (`MessagePosted`, `MessageUpdated`, `ChannelChanged`, `ChannelGroupChanged`,
   `AccountChanged`, `AgentConsoleChanged` — the tip's payload set,
   `comms.proto:303-311`). Single-process publishing is sound in T2 because the
   Server is the only writer; when a second writer appears (the Runner path),
   emission moves behind `Comms`/the substrate — noted as the seam, not built
   now.
3. **`since_seq = 0` is a state snapshot, not a ring replay.** The contract
   defines 0 as "snapshot current state as events, then tail"
   (`comms.proto:479-480`). The load-bearing subtlety the mechanism must respect:
   the daemon bus's own no-dup/no-drop handoff (`events.rs:104-157`) holds
   because `subscribe` attaches the live receiver **and** fills the replay batch
   **from the bus's own ring under one lock** — but the comms current state is
   *not* on the ring. It is `Comms` state (accounts, groups, channels,
   workspaces) behind a **separate subsystem lock**
   (`compass-comms/src/service.rs:59`), and the genericized
   `EventBus<P>::subscribe(since_seq, req_epoch)` (S1) holds no `Comms`
   reference — so the snapshot cannot be "synthesized under the bus lock": two
   subsystems, two locks. The correct mechanism is **attach-first across the two
   locks**: the adapter attaches the live receiver and reads the bus head
   (`next_seq - 1`) as the snapshot's consistency-point seq, *then* reads the
   `Comms` snapshot under the comms lock and emits it as `*Changed` events into
   the subscription's **`replay` batch** (the same `Vec<Stamped<P>>` a
   positioned cursor fills, `events.rs:145-151`), drained before any live event.
   Attach-first guarantees **no drop** — any mutation after attach lands on the
   already-attached `live` receiver. The narrow window between attach and
   snapshot-read can put one mutation in *both* the snapshot and the live tail,
   but every snapshotted variant is a **state-replace** `*Changed` (an entity's
   current value keyed by id), so the client applies it **idempotently** and the
   duplicate is a harmless re-apply, not a dup bug. `MessagePosted` is the one
   append-semantics event, and messages are **never snapshotted** —
   `ListMessages` is the paging path — so the append case never enters the
   snapshot and ring eviction can never make a fresh subscribe lossy. **The
   synthesized events carry that consistency-point seq as their cursor.** The
   resync sentinel is *not* "any event with `seq = 0`": the contract defines it
   as the `resync_required` **payload variant**, which carries `seq = 0` and is
   not a cursor (`comms.proto:305-308,315-323`). A snapshot `*Changed` is
   therefore never mistaken for it — a client distinguishes them by payload
   variant, and MUST NOT treat a bare `seq = 0` as resync. On a fresh bus that
   has published nothing, `next_seq - 1 == 0`, so a first snapshot's events
   (e.g. the bootstrap account) legitimately carry `seq = 0` — the correct
   cursor meaning "nothing on the ring yet, tail from the first live event."
4. **A positioned cursor** (`since_seq > 0`) replays the bus ring then tails —
   `events.rs` semantics verbatim; epoch mismatch or eviction underflow answers
   with a terminal `CommsResyncRequired` (seq 0) exactly as the daemon stream
   does today (`service.rs:106-114`, `resync_required()` `service.rs:231-240`).
5. **Visibility filtering at delivery:** every event forwarded to a subscriber
   passes a `Comms::visible_to(actor, &payload) -> bool` check (membership /
   ownership / group visibility / console participation — the same D9 rules the
   RPC reads enforce). Filtering makes per-subscriber seq *gaps* normal; the
   contract only requires monotonicity, which the shared bus seq provides. A
   membership change racing an in-flight event may deliver one event the caller
   could already/no-longer see — accepted for v1 (the snapshot path
   reconverges), documented in the module.
6. The per-subscriber forwarding task reuses the daemon's `forward()` idiom —
   bounded mpsc, select on the shutdown watch so held-open streams never pin
   graceful drain (`service.rs:183-229`, capacity `service.rs:26-29`).

The per-channel substrate keeps its T1 role — durable per-container history and
audit reads (`channel_history`, `service.rs:290-310`; JetStream mapping
`substrate/jetstream.rs:8-19`) — and its seq space never crosses the stream's:
stream cursors are bus seqs, paging cursors are message ids.

### F5 — `compass.v1` evolution + the CI drift gate

**Decision: T2 evolves no schema itself — it binds to the comms contract at
PR #533's merge (the freeze), and any T2-discovered contract delta ships as a
separate additive schema PR through the existing buf pipeline.**

The evolution the parent's T2 line names ("evolve `compass.v1` … to carry
multi-user accounts, channels, and the ACP-as-DM event stream",
`../compass-0.5/design.md:667-668`) *is* the comms contract PR #533 — additive
(`comms.proto` is a new file/service in the same package; its own header says
"an additive surface (new file, new service) … without a breaking change",
contract tip `comms.proto:13-18`). The gates it must stay green through are
already built in `compass-proto`'s moon project:

- **drift** — regenerate into a temp dir and `git diff --no-index` against both
  checked-in clients (`crates/compass-proto/moon.yml:59-67`), the
  concrete mechanism behind "the generated clients are checked in and
  CI-verified against the schema (drift fails the build)"
  (`../compass.md:284`).
- **breaking** — `buf breaking` against `origin/main`'s schema
  (`moon.yml:27-31`).

Consequences for T2 execution:

1. **Sequencing:** implementation tasks that touch the comms surface (S2 and
   later) start only after #533 merges — the merge is the data-model freeze.
   **#533 has now merged (`5eb5a063`), freezing the contract**, and the freeze
   did rename the provisional console type:
   `open_agent_console`/`share_agent_console` became
   `open_agent_workspace`/`share_agent_workspace`, plus a new
   `unshare_agent_workspace` (OQ4). This record's mapping table (F4) names the
   pre-freeze shapes; the re-bind to the workspace names is mechanical — the
   capabilities, authz model, and stream semantics above are shape-stable.
2. **T2 implementation PRs regenerate nothing:** they consume the checked-in
   generated code (`compass_proto::v1`, `compass-proto/src/lib.rs:12-15`), so
   drift/breaking stay green by construction.
3. **If T2 needs a contract delta** (the one identified candidate is token
   issuance — OQ1, scheduled as **S4a**): it ships as its **own additive
   schema PR** — `proto` edit + `moon run compass-proto:gen` + committed
   regenerated clients, through the same lint/breaking/drift lanes. #533 has
   merged, so the delta lands against the frozen `comms.proto` on `main` — no
   longer coordinated against an open contract PR.

## Global Constraints

- **Frozen-record convention.** This is a child record of
  `../compass-0.5/design.md`, refining its T2 only; it supersedes nothing and
  rewrites nothing. New decisions of record live here; the parent's D1-D14
  govern wherever this record is silent.
- **Contract freeze dependency.** The comms contract is now **frozen**: PR #533
  merged (`5eb5a063`), and the T1 `compass-comms` substrate/service landed with
  it (#535, `b71c86c7`). Tasks S2-S7 bind to that frozen schema; the
  field-level names in this record are the pre-freeze tip's and re-bind
  mechanically to the frozen names (the console→workspace rename, OQ4). No T2
  task edits `comms.proto`; any delta ships as an additive schema PR per F5.
- **Rust toolchain + pins.** Workspace `tonic = "0.14"` / `tonic-web = "0.14"`
  (root `Cargo.toml:22-26`); the TLS door uses tonic's `tls-ring` feature only —
  **not** `tls-native-roots`/`tls-webpki-roots`: server-side identity plus an
  operator-provided client CA need neither, and the workspace license fence
  (`deny.toml`) admits data-licensed CA bundles such as `webpki-root-certs` only
  as a seal-scoped exception (`deny.toml:50-55`), never for compass. The rustls
  process-level provider is installed once in `main` before any TLS
  construction, per the `seal-daemon` precedent
  (`oss/seal/crates/seal-daemon/src/main.rs:28-41`).
- **`cargo deny check bans licenses sources` stays green** (the workspace gate,
  `compass-proto/moon.yml:104-119`) — new deps are `rcgen` (dev-only) and the
  tonic TLS feature closure; both must clear the fence.
- **moon registration.** moon projects are an explicit map
  (`.moon/workspace.yml`); `compass-comms` is **still absent from it** — T1's PR
  (#535, merged) shipped the crate without registering it (only `compass-proto`
  and `compass-daemon` are mapped, `.moon/workspace.yml:50-51`), so its
  `moon.yml` lanes never run in CI. T2's first implementation PR registers
  `compass-comms` (and any lane it adds) before relying on its gates.
- **Authorization is transport-derived.** No RPC trusts a caller-identity
  field; the actor is the authenticated connection's account, injected
  server-side (contract tip `comms.proto:31-37`). Every new `Comms` method
  takes `actor: AccountId` first and authorizes before touching state — the T1
  discipline (`compass-comms/src/service.rs:6-10`).
- **No plaintext network listener; no permissive CORS off loopback.** TLS flags
  are required with `--listen`; the dev gRPC-Web endpoint keeps its loopback
  guard (`serve.rs:56-63`).
- **The transport-boundary invariant.** "The shell and UI must never assume
  'local' beyond the transport boundary" (`../compass-tauri-shell.md:119-121`):
  no socket path, `localhost`, or transport-mode branching above the
  `@compass/client` factory seam.
- **Spec discipline.** This record's PR changes no living spec
  (Spec-impact: none — design only). Each T2 *implementation* PR that lands
  served behavior updates `docs/specs/product/compass.md` in the same PR
  (`AGENTS.md:73-80`).
- **`rule://planning-evidence`.** Every claim about existing code/design in
  this record carries file+line verified this session; contract claims made
  against tip `8ec7dcf5` resolved at the freeze — #533 merged at `5eb5a063`,
  #535 (T1) at `b71c86c7`.
- **No persona / agent-product names** in this record (`AGENTS.md` convention,
  restated by the parent at `../compass-0.5/design.md:628-631`).
- **markdownlint-clean** under the repo config (`.markdownlint.json`,
  `.markdownlint-cli2.jsonc`).

## Plan

Tasks are ordered by dependency; each carries its own test gate. S1, S3 are
contract-independent and can start immediately; S2 and later start at the
contract freeze (F5). "Server crate" means `compass-daemon` (F1).

### S1 — Genericize the daemon event bus

Parameterize `events.rs` over its payload so one bus implementation backs both
sequenced streams (F4.1). `SubscribeEvents` behavior is unchanged; its existing
unit tests (`events.rs:190-467`) and the transport integration tests
(`crates/compass-daemon/tests/transport.rs`) stay green.

*Interfaces:*

- Produces (in `compass-daemon/src/events.rs`):

  ```rust
  pub struct Stamped<P> {
      pub seq: u64,
      pub at_unix_ms: i64,
      pub instance_epoch: u64,
      pub payload: P,
  }

  pub struct EventBus<P: Clone + Send + 'static> { /* ring + broadcast, as today */ }

  impl<P: Clone + Send + 'static> EventBus<P> {
      pub fn new() -> Self;
      pub(crate) fn publish(&self, payload: P) -> u64; // returns assigned seq
      pub fn subscribe(&self, since_seq: u64, req_epoch: u64)
          -> Result<Subscription<P>, BufferUnderflow>;
      pub(crate) fn instance_epoch(&self) -> u64;
  }

  pub struct Subscription<P> {
      pub replay: Vec<Stamped<P>>,
      pub live: tokio::sync::broadcast::Receiver<Stamped<P>>,
      pub epoch: u64,
  }
  ```

- Consumes: current `EventBus`/`Subscription`
  (`events.rs:31-37,52-59,67-164`). The daemon service maps
  `Stamped<subscribe_events_response::Payload>` → `SubscribeEventsResponse` at
  the stream edge (today the bus stores the response struct directly,
  `events.rs:89-94`); `serve()`'s `events.publish(Payload::DaemonStatus(…))`
  call (`serve.rs:107-110`) is mechanical to update.
- Gate: `cargo nextest run --locked -p compass-daemon` green with zero
  behavioral diffs in `tests/transport.rs`.

### S2 — Extend `compass-comms` to the frozen contract capability set

Add the domain capability the contract has and T1 lacks (F4 table), re-shaping
the T1 channel model to the frozen container shapes (groups; the agent console
replacing the T1 agent-DM `open_dm`/`promote_to_group_dm` pair,
`compass-comms/src/service.rs:159-245`, whose seam tests carry over
re-targeted). All methods follow the T1 actor/authz discipline and the existing
error taxonomy (`error.rs:36-63`).

*Interfaces:*

- Consumes: `Comms`, `MessagingSubstrate`, domain types as exported by
  `compass-comms/src/lib.rs:28-36`; the frozen contract shapes (at tip:
  `ChannelGroup` `comms.proto:146-157`, `AgentConsole` `comms.proto:201-209`,
  `Message.container` `comms.proto:217-230`, `Ask` `comms.proto:265-276`).
- Produces (signatures shown against tip shapes; rename with the freeze):

  ```rust
  pub enum Container { Channel(ChannelId), Console(ConsoleId) }

  impl Comms {
      pub fn create_channel_group(&self, actor: AccountId, name: &str,
          parent_group_id: Option<GroupId>, visibility: GroupVisibility)
          -> Result<ChannelGroup>;
      pub fn list_channel_groups(&self, actor: AccountId) -> Result<Vec<ChannelGroup>>;
      pub fn list_channels(&self, actor: AccountId) -> Result<Vec<Channel>>;
      pub fn open_agent_console(&self, actor: AccountId, agent: AccountId)
          -> Result<AgentConsole>;                       // idempotent
      pub fn share_agent_console(&self, actor: AccountId, console: ConsoleId,
          add_user: AccountId) -> Result<AgentConsole>;
      pub async fn list_messages(&self, actor: AccountId, container: Container,
          limit: u32, before_message_id: Option<MessageId>) -> Result<Vec<Message>>;
      pub async fn respond_to_ask(&self, actor: AccountId, ask_id: &str,
          chosen_option_ids: Vec<String>) -> Result<Message>; // emits MessageUpdated
      pub async fn search_messages(&self, actor: AccountId, query: &str,
          scope: Option<Container>, limit: u32) -> Result<Vec<Message>>;
      // Filter operates on the same envelope payload the bus carries, so the
      // adapter passes `&Stamped::payload` straight through — no second type.
      pub fn visible_to(&self, actor: AccountId,
          payload: &subscribe_comms_response::Payload) -> bool;
  }
  ```

- Gate: seam contract tests
  (`compass-comms/tests/seam_contract.rs` extended) covering each new method's
  authz rejection + success path against `InMemorySubstrate`; clippy + nextest
  for `compass-comms` (its moon lanes registered per Global Constraints).

### S3 — Token auth: store, interceptor, bootstrap, UDS ambient identity

The F2 authentication layer, self-contained in the Server crate.

*Interfaces:*

- Produces (in `compass-daemon/src/auth.rs`):

  ```rust
  pub(crate) struct TokenStore { /* parking_lot::RwLock<HashMap<[u8; 32], AccountId>> */ }

  impl TokenStore {
      pub fn new() -> Self;
      pub fn issue(&self, account: AccountId) -> String;      // 32B random, base64url; stores sha256
      pub fn resolve(&self, presented: &str) -> Option<AccountId>;
      pub fn revoke_account(&self, account: AccountId);
  }

  #[derive(Clone, Copy, Debug)]
  pub(crate) struct AuthedAccount(pub AccountId);

  /// Bearer-token interceptor for the network door.
  pub(crate) fn bearer_auth(store: Arc<TokenStore>)
      -> impl tonic::service::Interceptor + Clone;
  /// Static-identity interceptor for the UDS door (socket mode is the credential).
  pub(crate) fn ambient_identity(account: AccountId)
      -> impl tonic::service::Interceptor + Clone;
  ```

- Consumes: `tonic::service::Interceptor` (tonic-0.14.6
  `src/service/interceptor.rs:41-44`), `Request::extensions_mut()`
  (`src/request.rs:350`), `sha2`/`base64`/`getrandom` (root
  `Cargo.toml:95-97,127`; `getrandom` already a daemon dep,
  `compass-daemon/Cargo.toml:29`), `Comms::bootstrap_admin`
  (`compass-comms/src/service.rs:69-80`).
- Gate: unit tests — issue/resolve round-trip, unknown token rejected, hash-only
  storage (no plaintext token retrievable), interceptor injects
  `AuthedAccount`, missing/malformed header ⇒ `unauthenticated`.

### S4 — The `CommsGrpc` adapter + `SubscribeComms`

Implement the generated trait against `Comms` (F4): mechanical request/response
mapping, actor extraction, error mapping, the comms event bus, and the
snapshot/replay/resync stream semantics.

*Interfaces:*

- Consumes: the generated trait + server
  (`compass_proto::v1::comms_service_server::{CommsService, CommsServiceServer}`,
  contract tip `compass.v1.tonic.rs:1109-1226`), `Comms` (T1 + S2),
  `EventBus<subscribe_comms_response::Payload>` (S1), `AuthedAccount` (S3),
  the `forward()` idiom (`compass-daemon/src/service.rs:183-229`).
- Produces (in `compass-daemon/src/comms_grpc.rs`):

  ```rust
  #[derive(Clone)]
  pub(crate) struct CommsGrpc {
      comms: compass_comms::Comms,
      bus: Arc<EventBus<subscribe_comms_response::Payload>>,
      shutdown: tokio::sync::watch::Receiver<()>,
  }

  impl CommsGrpc {
      pub(crate) fn new(comms: compass_comms::Comms,
          bus: Arc<EventBus<subscribe_comms_response::Payload>>,
          shutdown: watch::Receiver<()>) -> Self;
  }

  #[tonic::async_trait]
  impl CommsService for CommsGrpc {
      type SubscribeCommsStream =
          tokio_stream::wrappers::ReceiverStream<Result<SubscribeCommsResponse, tonic::Status>>;
      // all 13 methods per the F4 mapping table
  }

  /// CommsError → tonic::Status per the F4 error mapping.
  fn comms_status(err: compass_comms::CommsError) -> tonic::Status;
  /// The authenticated actor, or `unauthenticated` if the extension is absent.
  fn actor<T>(req: &tonic::Request<T>) -> Result<AccountId, tonic::Status>;
  ```

- Stream semantics as decided in F4: `since_seq = 0` ⇒ visibility-scoped state
  snapshot delivered as the pre-live `replay` batch of `*Changed` events (each
  stamped the consistency-point seq, the bus head — which is legitimately `0` on
  a fresh bus); the resync sentinel is the `resync_required` **payload variant**
  (not a bare `seq = 0`), so a snapshot event is never mistaken for it; then live
  tail; positioned cursor ⇒ ring replay + tail; underflow/epoch mismatch ⇒
  terminal `CommsResyncRequired`; per-event `visible_to` filter; id strings
  parsed as UUIDs (`invalid_argument` on failure).
- Gate: in-process tests over `InMemorySubstrate` — one per RPC group
  (happy + authz-rejection mapped to the right `Status` code), plus stream
  tests: snapshot-then-live ordering, positioned resubscribe after a dropped
  stream is gap-free, stale cursor ⇒ resync, non-member never receives another
  channel's event.

### S5 — The network door in `serve()`

Add the TLS listener as a third raced server task, mirroring the existing
UDS/dev-TCP pattern (`serve.rs:136-202`), and register both services on all
doors with their interceptors.

*Interfaces:*

- Produces (in `compass-daemon`):

  ```rust
  pub struct NetworkListener {
      pub addr: std::net::SocketAddr,
      pub tls_cert: std::path::PathBuf,          // PEM chain
      pub tls_key: std::path::PathBuf,           // PEM key
      pub cors_allowed_origin: Option<http::HeaderValue>, // gRPC-Web browser origin; None = no CORS
  }

  pub async fn serve(
      socket_path: &Path,
      version: &str,
      dev_http: Option<SocketAddr>,
      network: Option<NetworkListener>,
      shutdown: impl Future<Output = ()>,
  ) -> Result<()>;
  ```

  CLI (`main.rs`): `--listen <ADDR>`, `--tls-cert <PATH>`, `--tls-key <PATH>`
  (all-or-none, validated up front like `--dev-http`'s loopback guard,
  `main.rs:44-51`), `--cors-allowed-origin <ORIGIN>`, `--admin-handle <HANDLE>`;
  `rustls::crypto::ring::default_provider().install_default()` before serving
  when `--listen` is set (precedent `seal-daemon/src/main.rs:39-41`).
- Consumes: `ServerTlsConfig::new().identity(Identity::from_pem(cert, key))` +
  `Server::builder().tls_config(…)` (tonic-0.14.6
  `src/transport/server/tls.rs:23-35`, `src/transport/server/mod.rs:166`);
  eager TCP bind before on-disk state (the dev-endpoint pattern,
  `serve.rs:65-78`); `CommsServiceServer::with_interceptor(inner, interceptor)
  -> InterceptedService<Self, F>` (generated per service; contract tip
  `compass.v1.tonic.rs:1240-1248`); the S3
  interceptors — `bearer_auth` on the network door for both services,
  `ambient_identity(bootstrap_admin)` on the UDS/dev doors for the comms
  service.
- Callers updated: `main.rs:61`, every `compass_daemon::serve(...)` call in
  `tests/transport.rs` (e.g. `tests/transport.rs:106,165`) passes
  `None` for `network`.
- Gate: integration tests (rcgen-minted self-signed pair, dev-dependency —
  precedent `seal-runtime/Cargo.toml:82-86`): TLS client with the test CA
  connects and `GetDaemonInfo` answers; no-token comms RPC ⇒ `unauthenticated`;
  bad-token ⇒ `unauthenticated`; valid token ⇒ authorized `actor` observed;
  `--listen` without cert/key refuses startup; UDS behavior byte-identical to
  today (existing transport tests unmodified except the added `None` arg).

### S6 — `@compass/client` comms factories + auth interceptor

The client-package half of F3.

*Interfaces:*

- Produces (in `packages/compass-client/src/index.ts`):

  ```ts
  export type CommsClient = Client<typeof CommsService>;
  export function createCommsClient(transport: Transport): CommsClient;
  export function createCommsWebClient(baseUrl: string, token?: string): CommsClient;
  export function createCommsClientOverFetch(
      fetch: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>,
      baseUrl?: string,
      token?: string,
  ): CommsClient;
  /** Sets `authorization: Bearer <token>` on every request. */
  export function bearerAuthInterceptor(token: string): Interceptor;
  ```

  `createCompassWebClient`/`createCompassClientOverFetch` gain the same
  optional `token` parameter (backwards-compatible).
- Consumes: the generated `CommsService` descriptor (`src/gen/compass/v1/comms_pb.ts`,
  landed by the contract PR), `createGrpcWebTransport({ baseUrl, fetch?, interceptors? })`
  (`node_modules/@connectrpc/connect-web/dist/esm/grpc-web-transport.d.ts:32`),
  the existing factory pattern (`src/index.ts:13-50`).
- Gate: `bun test` unit tests — the interceptor sets the header exactly once;
  factories construct against a mock fetch; no import outside `@compass/client`
  touches generated stubs (the existing fence, `src/index.ts:17-19`).

### S7 — End-to-end multi-client gate

The parent's T2 test gate, executed: "multi-client connect + event resubscribe
(sequenced, `../compass.md:282`) tests green; `compass.v1` contract-drift check
green" (`../compass-0.5/design.md:671-673`).

*Interfaces:*

- Consumes: everything above; the daemon integration-test harness idioms
  (`tests/transport.rs:37-118` — connector, readiness probe, spawn/shutdown).
- Produces: `compass-daemon/tests/comms_transport.rs` — over one TLS Server:
  1. bootstrap admin token → admin creates two users, issues their tokens
     (per the Open-Questions resolution on issuance), each user connects as a
     separate client;
  2. user A opens a group/channel per the frozen shape, posts; user B (member)
     receives `MessagePosted` on `SubscribeComms` with strictly-increasing
     `seq`; a non-member client never receives it;
  3. user B drops the stream, reconnects with its cursor + epoch ⇒ gap-free
     tail (no duplicate, no loss); reconnect with a stale epoch ⇒
     `CommsResyncRequired` then a clean `since_seq = 0` snapshot.
- Gate: the new integration test + full `moon ci` for the affected projects
  (includes `compass-proto:drift`/`breaking`, untouched and green per F5).

## Tasks

- [ ] **S1 — Genericize the daemon event bus.** `EventBus<P>`/`Stamped<P>` in
      `compass-daemon/src/events.rs`; `SubscribeEvents` behavior unchanged;
      existing unit + transport tests green.
- [ ] **S2 — Extend `compass-comms` to the frozen contract capability set.**
      Groups, console (per frozen shape), `list_channels`, `list_messages`
      paging, `respond_to_ask`, `search_messages` v1, `visible_to`; seam
      contract tests extended; `compass-comms` registered in
      `.moon/workspace.yml`. *(Starts at contract freeze.)*
- [ ] **S3 — Token auth + session-RPC admin gate.** `TokenStore` (hashed),
      `bearer_auth` + `ambient_identity` interceptors, bootstrap-admin token at
      first start; **plus the F2 method-level guard that rejects a non-admin
      account on the network door's `CompassService` agent-session RPCs
      (`start`/`stop`/`reload`/inspect) with `permission_denied`, while
      `GetDaemonInfo` and all of `CommsService` stay open to any authenticated
      account** — `bearer_auth` only authenticates the token and injects the
      account, so this per-method admin check is a distinct interceptor/guard,
      not implied by it. Unit tests incl. non-admin ⇒ `permission_denied`.
- [ ] **S4 — `CommsGrpc` adapter + `SubscribeComms`.** All 13 trait methods per
      the F4 mapping; error mapping; comms bus with snapshot-as-events,
      positioned replay, resync, visibility filter; in-process tests.
- [ ] **S4a — `IssueToken` schema PR** *(prerequisite for S7; nothing in
      S1–S6 depends on it)*. OQ1's admin-gated `IssueToken(account_id) → token`
      RPC lands as its **own additive schema PR** (proto edit + `moon run
      compass-proto:gen` + committed regenerated clients, through the buf
      lint/breaking/drift lanes per F5), coordinated with the #533 contract
      owner while it is open. Ships after #533 freezes. S3's token store binds
      this shape; **S7's two-user step is blocked until S4a lands** — it is the
      only public-contract path to mint the second user's token.
- [ ] **S5 — Network door.** `NetworkListener` in `serve()` (third raced task),
      TLS via tonic `tls-ring`, CLI flags, rustls provider install, CORS flag;
      TLS/auth integration tests **including a non-admin bearer token calling a
      `CompassService` session RPC over the network door and getting
      `permission_denied` (the S3 admin gate, end to end)**; UDS behavior
      unchanged.
- [ ] **S6 — TS client factories.** `createCommsClient` family + bearer
      interceptor in `@compass/client`; `bun test` green.
- [ ] **S7 — End-to-end gate** *(requires S4a landed — its two-user step mints
      the second token via `IssueToken`)*. Multi-client TLS connect,
      member-scoped event delivery, sequenced resubscribe, resync; `moon ci`
      (incl. proto drift + breaking) green.

> Each implementation PR that lands served behavior also updates
> `docs/specs/product/compass.md` in the same PR (Global Constraints). This
> record's own PR: **Spec-impact: none** (design only).

## Open Questions

- **OQ1 — Token issuance for non-bootstrap users** *(resolved — decided; the
  contract froze without a credential RPC, confirming this path)*. The
  contract has no credential RPC, and the account/token stores are in-memory
  until T5 — so a token minted out-of-band by a second process cannot reach the
  serving process's store. How does a user created via `CreateUser` obtain their
  bearer token?
  **Decision:** a dedicated admin-gated `IssueToken(account_id) → token` RPC,
  shipped as its **own additive schema PR** per F5. Chosen over a one-time
  `issued_token` field on `CreateUserResponse` because the RPC is the cleaner
  shape — re-issuance and revocation fall out of it naturally, and it decouples
  credential delivery from account creation. The RPC is admin-gated (only an
  admin mints a token for an account), resolving to the D9 account the
  bearer-token interceptor then keys on (F2). S3 builds the token store against
  this shape; S7's two-user gate mints its second user's token via `IssueToken`.
  The comms contract (#533) merged (`5eb5a063`) **without** a credential RPC, so
  the separate additive-PR path (S4a) is now the *only* path to this capability
  — not one option among several. The proto delta lands through the same buf
  lint/breaking/drift lanes as F5, coordinated with the contract owner.
- **OQ2 — Browser origin for the MVP Client** *(non-load-bearing — deferred to
  T4 with the seam named)*. T2 ships `--cors-allowed-origin` (closed by
  default), which is sufficient for any T4 hosting choice. If T4 prefers
  zero-CORS, the Server can grow a same-origin static route for the UI bundle
  on the TLS listener; nothing in T2 precludes it. Defaulting to the explicit
  CORS flag is the smaller, reversible surface.
- **OQ3 — Client self-identity (`WhoAmI`)** *(non-load-bearing — deferred)*.
  The contract has no "who am I" RPC; `ListAccounts` scoping does not uniquely
  identify the caller for an admin. MVP: the client's configuration carries its
  handle alongside its token (the operator issued both together). A tiny
  additive `WhoAmI` RPC can ride any later schema PR if T4 wants it; the design
  is correct without it.
- **OQ4 — Contract-freeze rename fallout** *(resolved — the freeze reshaped as
  anticipated; mechanical re-bind)*. The console type name was provisional at
  the contract tip; the freeze (#533, `5eb5a063`) **renamed** it —
  `open_agent_console`/`share_agent_console` became
  `open_agent_workspace`/`share_agent_workspace`, and a third
  `unshare_agent_workspace` was added, so the frozen `CommsService` trait has
  **14** methods, not the 13 named at the tip
  (`crates/compass-proto/src/gen/compass/v1/compass.v1.tonic.rs:1150-1254`;
  `comms.proto:38-99`). S2/S4's signatures re-bind 1:1 to the workspace names —
  the capability set, authz rules, and stream semantics in this record are
  shape-stable — and S2 gains the `unshare_agent_workspace` method on the same
  console/workspace substrate mechanics. Expected re-bind, not drift.
- **OQ5 — Should the network door expose `CompassService` agent-session RPCs at
  all in T2?** *(resolved — decided)*. F2 serves **both** services on the
  network door and gates the `CompassService` session RPCs
  (`start`/`stop`/`reload`/inspect) to the bootstrap admin, because those RPCs
  "were written for the single-user UDS trust boundary and take no account
  argument" (verified — `start_agent_session` etc. take only
  `container_name`/`session_id`, `crates/compass-daemon/src/service.rs:120-136`).
  The critic weighed an alternative: do **not** route `CompassService` session
  RPCs on the network door at all in T2 — serve only `GetDaemonInfo` plus all of
  `CommsService` there, keeping session lifecycle **UDS-only** until T3 gives it
  the per-account Runner path (a smaller network surface, no admin-gate branch
  over actor-less RPCs).
  **Decision (keep F2 as drafted):** expose the session RPCs on the network door
  behind the bootstrap-admin gate. The dogfood MVP that would exercise the
  alternative's tradeoff (a browser comms Client with no need for remote session
  control) is itself deferred until past this point, so admin-gated exposure
  carries no near-term cost and keeps the remote single-owner session-control
  path available rather than removing then re-adding it. The admin gate holds
  the single-owner semantics until **T3 supersedes these RPCs with the
  per-account Runner path**, at which point the gate lifts (F2). The gate is a
  distinct method-level interceptor check, not implied by `bearer_auth` (S3).
