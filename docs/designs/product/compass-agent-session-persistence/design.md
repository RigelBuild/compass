# Compass agent session persistence: server-owned durable log via tee-emit; resume via a Runner-materialized session file + SDK-native load

Status: Active

> Freezes on merge; later changes supersede by citation, never rewrite.
> Implements Linear SEA-1570 (pre-dogfood item 4) against TWO fixed rulings:
> Matt's storage-ownership ruling of 2026-07-31 (storage is SERVER-OWNED — the
> agent emits its transcript over the existing durable frame channel and holds
> zero S3 credentials), and Matt's collapse rulings of 2026-07-31, which
> replaced this record's interim control-lane-replay resume model wholesale:
> the session log and the live trace are two projections of ONE canonical
> artifact (the log); token-by-token streaming is mandatory, so MVP runs a
> durable settled-entry stream beside an ephemeral live per-token stream;
> resume is a server-reconstructed session file the Runner MATERIALIZES into
> the new container at provision, loaded by the agent through the SDK's own
> native loader; a definitively-erred emit escalates and FAILS the session at
> a bounded cap. The `TranscriptReplay` payload, the control-lane replay
> driver, the replay barrier, the replay admission bound, and the SEA-1310
> co-ratification are all DEAD (inventoried in the second appendix; ledger
> DL-086 → DL-087). SEA-1310 no longer blocks anything here. The post-MVP
> unification of the two wire streams into one token stream carrying log
> metadata is filed as SEA-1580, out of scope for this record.

## Problem / Intent

An agent container is stateless by construction: the container's main process
is `sleep infinity` and the agent is driven via exec
(`go/internal/runtime/agent.go:214-216`: "Keep the container alive so the
Runner can exec into it; the agent is driven via exec, not as the container's
main process"), so the SDK session JSONL the agent writes under its container
filesystem dies with the container. This is not merely a shutdown concern: a
crash leaves even the Runner's own socket files behind for the next Provision
to reclaim (`go/internal/runner/host.go:166-168`: "A crash instead leaves the
socket files on disk, which the next Provision reclaims"), and the in-container
transcript has no such reclaim path — it is simply gone with the container's
writable layer. A crashed, reloaded-elsewhere, or re-provisioned agent loses
its whole conversation. SEA-1570 (pre-dogfood item 4) requires the session log
to survive the container and a session to be resumable in a NEW container with
its context intact. Durability is server-owned: the agent TEES each committed
transcript entry upstream over the existing durable agent→Runner→Server frame
channel while keeping its normal ephemeral local session file, the Server
persists each entry durably (a Postgres hot-tail that is the resume set) and
archives superseded history to object storage, and resume is a
server-RECONSTRUCTED session file the Runner writes into the new container's
session dir at provision — which the agent then loads through the SDK's own
native session loader, with no agent-side replay code at all.

## Approach

**The rulings (Matt; fixed inputs, not reopened).** Two layers, both closed:

1. **Storage ownership (2026-07-31).** The agent does NOT persist to S3. It
   emits its session transcript to the Server over the EXISTING
   agent→Runner→Server durable frame channel and holds ZERO S3 credentials and
   zero bucket access. Matt's three reasons: direct-S3 leaks a Server storage
   detail into the agent; zero agent creds cuts blast radius and credential
   churn (the old OQ-B full-bucket threat vanishes at the agent); and the
   Server can swap storage backends later without touching the agent.
2. **The collapse (2026-07-31).** The five forks the reversal opened are all
   ruled. R1: the durable session log and the live client trace are two
   projections of ONE source, and the canonical artifact is the SDK
   session-entry log. R2: token-by-token streaming is mandatory, so MVP runs
   two agent→server streams — the durable settled-entry log stream and an
   ephemeral, best-effort live per-token trace stream. R3: resume is a
   server-reconstructed JSONL body the Runner materializes into the new
   container's session dir at provision, loaded SDK-native by the agent —
   which kills the control-lane replay driver, the `TranscriptReplay`
   payload, the replay barrier, the replay admission bound, and the SEA-1310
   co-ratification in one stroke. R4: a definitively-erred emit buffers and
   retries under a bounded cap with escalating warn→error logs, and FAILS THE
   SESSION loudly at cap exhaustion.

Everything below designs inside these rulings. The former Open Questions
section records each fork's disposition; none remains live.

### One canonical artifact: the session log, unified with the trace (R1)

Today the durable-candidate log and the client trace are DISTINCT because the
trace is a lossy DISPLAY projection: the mapper emits one `SessionEvent` per
delta and deliberately does not coalesce — "the emitter … does NOT buffer or
coalesce the session stream — one `SessionEvent` per delta. Coalescing by
`message_id` is `foldSession`'s job on the render side" (`mapping.ts:35-41` at
`packages/compass-agent/src/mapping.ts`) — and caps tool output at 4KB on the
wire (`OUTPUT_TEXT_LIMIT = 4_000`, `mapping.ts:89-92`: "The session renderer
shows a disclosure, not the full blob"). The session log, by contrast, is
settled whole `SessionEntry` records: `#recordEntry` pushes the entry, indexes
it, and appends it to the session file (`session-manager.ts:837-842`). Resume
needs the lossless log, not the trace.

The ruling unifies them at the SOURCE: both are projections of the one
canonical artifact, the SDK session-entry log. The agent emits committed
session entries; the SERVER persists them (it owns durable storage) AND
projects the same entries to clients as the block-level trace. The lossy
per-token stream survives only as a liveness layer (next section). The
post-MVP follow-up that collapses the two wire streams into ONE token-by-token
stream carrying log metadata is filed as **SEA-1580** — referenced here, not
designed here.

### Liveness: two agent→server streams for MVP (R2)

Token-by-token streaming is MANDATORY — every agent streams per-token; not
negotiable. MVP therefore runs TWO agent→server streams:

- **The DURABLE settled-entry log stream** — one `TranscriptEntry` frame per
  committed session entry on the delivered-or-erred conversation-frame lane.
  The Server persists it and projects the block-level trace from it.
- **An EPHEMERAL live per-token trace stream** — the existing session-frame
  Publish spine, best-effort and non-persisted, driving the live pane only.

The SDK itself already forks one internal source into exactly these two
granularities, which is WHY the streams cannot collapse into one for MVP:
`agent.subscribe(#handleAgentEvent)` is the single upstream subscribe
("Always subscribe to agent events for internal handling",
`agent-session.ts:2738-2740`); `#emit` fans per-delta events to external
subscribers — "rpc-mode stdout, ACP bridge, Cursor exec, TUI listeners"
(`agent-session.ts:2013-2014`); while `#recordEntry` records settled blocks
(`session-manager.ts:837-842`). And the log is a strict SUPERSET of the token
stream: metadata-only entries are minted directly, never delta-derived —
`mode_change` (`session-manager.ts:1493-1494`), `model_change` (`:1504-1505`),
`session_init` (`:1517-1518`), `compaction` (`:1532-1543`), `ttsr_injection`
(`:1609-1616`), `label` (`:1669-1670`). The durable leg is settled-entry; the
live leg is throwaway.

### The channel already exists (grounded)

The durable log stream rides infrastructure that is already on main;
grounding is split by surface — proto + generated bindings at compass
origin/main `153a2a4` (SEA-1569 merged), hand-written Go/TS unchanged between
`335bb06` and `153a2a4`:

- **Frame producer (agent).** `packages/compass-agent/src/transport/frame-sink.ts`
  exposes `emit(frame: OutboundFrame): void` with two lanes split by
  durability: durable conversation frames ride the `PostConversationFrame`
  unary — awaited, retried on a bounded backoff under a per-attempt deadline,
  carrying an agent-minted idempotency key that is stable across retries of
  one frame and unique across agent restarts (per-sink random nonce +
  monotonic counter, `frame-sink.ts:80-97`; the retry loop at `:96-122`) —
  while loss-tolerant session/trace frames ride the fire-and-forget Publish
  spine (`frame-sink.ts:125-145`). The teardown `drain()` awaits every
  in-flight durable commit (`frame-sink.ts:147-158`), and `cli.ts` already
  drains in its `finally` before closing the transport (`cli.ts:202-208`).
- **Wire.** `AgentFrame` is a oneof of six variants at `153a2a4`:
  `conversation_posted = 1`, `conversation_updated = 2`, `session = 3`,
  `replay_complete_ack = 4`, `control_ack = 5`, `delivery_ack = 6`
  (`agent.proto@153a2a4:40-75`). Additions to the oneof are additive and
  buf-breaking-safe — the ack variants and SEA-1569's `delivery_ack` were
  added exactly this way ("Additive to the frozen oneof; buf-breaking-safe",
  `agent_pb.ts@153a2a4:425-428`).
- **Relay.** The Runner is a pure verbatim forwarder on both lanes. Durable:
  `PostConversationFrame` → the `CommitConversationFrame` unary, which
  succeeds ONLY on a Server store commit ("the Runner is a pure forwarder: it
  sends the session_id it structurally owns and the agent's frame verbatim",
  `gateway.go:100-108`; `post_conversation_frame.go:32-77`); the Server
  commits at-most-once keyed on the agent's idempotency key
  (`runner.proto@153a2a4:332-335`; `Hub.CommitConversationFrame`,
  `relay_comms.go:158-186`). Lossy: `PublishEvents` with runner_seq gap
  detection (`runner.proto@153a2a4:274-297`; `handler.go:104-123`;
  `hub.go:237-255` routes by oneof variant; `recordSeq` gap-flags at
  `hub.go:457-467`).
- **Provision-by-exec (the resume delivery seam).** The Runner already drives
  container setup by exec at provision: `Launch` creates + starts the
  container, arms egress as root, and installs scoped credentials over an
  exec'd `sh -s` with the payload on stdin (the stdin-payload exec pattern,
  `go/internal/runtime/agent.go:266-272` — the surviving provision-exec
  precedent: sealed#1019 "SPAWN CARRIES NO REPO" deletes the server-side
  auto-clone, so the checkout may be empty at provision; the agent
  self-clones post-launch), all while the container's main process is
  `sleep infinity` and
  the agent is exec'd only later (`go/internal/runtime/agent.go:214-216`).
  `TestFailedProvisionRemovesThePartialContainer` pins that a provision-time
  exec failure surfaces at provision (`go/internal/runtime/agent_test.go:240-243`).
  Materializing a session file into the container BEFORE the agent exec is
  one more exec in exactly this family — no new mechanism.

**The three gaps that make this a BUILD, not a tweak:**

1. **No server-side durable transcript store exists.** `deliverSession`
   routes a session frame to the session-tail stream + lifecycle pub — never
   the DB (`hub.go:447-455`). Only comms Messages and the
   `session_id → agent_account_id` ownership row persist
   (`agent_sessions.go:31-56`). The Server must GAIN the store (T4).
2. **The raw transcript is not emitted today.** Conversation frames carry
   rendered comms Messages; session frames carry the typed trace
   (`agent.proto@153a2a4:40-59`). Neither is the raw SDK session entry that
   resume needs — a NEW durable oneof variant, `TranscriptEntry`, carries it
   (T1).
3. **No resume delivery path exists.** Nothing today reconstructs a session
   file from server state or places one into a new container: the Server has
   no body reconstructor (T5), the start path carries no resume identity
   (T6), and the Runner materializes nothing at provision (T8). The SDK side
   needs NO new mechanism — its native loader already reconstructs a session
   from a file (`loadEntriesFromFile`, `session-loader.ts:202-228`) and its
   resume seam already exists ("Switch to a different session file
   (resume / branch)", `setSessionFile`, `session-manager.ts:963-1013`).

### The model: tee-emit, server-persist, file-materialized resume

**WRITE (emit) path — a TEE, not a pure emitter.** The SDK's write seam is
kept — a storage injected at `createAgentSession({ sessionManager })`
(`sdk.ts:544-545`; the file-backed default applies only when none is passed,
`sdk.ts:1229-1233`; today `main()` passes only `cwd` + `modelPattern`,
`cli.ts:154-161`) — but the injected backend is a **tee**: it uses the
container-local (EPHEMERAL) filesystem for reads AND writes normally, so the
SDK's own loader, compaction, and rewrite machinery work against a real local
file, AND it tees each committed write upstream as a durable `TranscriptEntry`
frame (the entry verbatim in the SDK's own session-JSONL encoding) on the
PostConversationFrame-class lane: delivered-or-erred, idempotency-keyed,
at-most-once committed, persisted by the Server on receipt. Reads are real
local reads; writes are local write + emit. This is consistent with the
storage-ownership ruling: the container writable layer is inherently
ephemeral (the Problem statement above — the JSONL "dies with the
container"), so "the agent persists nothing / zero S3 creds" is about DURABLE
storage, not the SDK's normal local working file. The agent's durable record
and its resume path are entirely server-side.

**READ/RESUME path — the Runner materializes, the SDK loads.** On restart
into an existing logical session: the client calls
`StartAgentSession{container_name, resume_session_id}` (additive public
field, authz-gated by the existing `RequireAgentSessionSubscriber`,
`go/internal/store/agent_sessions.go:74-98`); the Server reads its persisted transcript and
reconstructs the post-supersession JSONL body — the latest checkpoint body
plus every later delta line, in the SDK's own session-JSONL line format (T5)
— and hands it to the Runner over the internal `SessionsResponse` relay
envelope, beside the start command it carries (T6); the
Runner WRITES that body into the new container's
session dir — HOME-relative, under the agent's scoped `$HOME`, mirroring the
Runner-materialized auth-seed (`authSeedPath(home)`, `cli.ts:48-49`), so
resume never depends on a populated checkout (HomeDir and CheckoutDir are
distinct, `go/internal/runtime/workspace.go:75-79`; under sealed#1019's
no-auto-clone provision the checkout may be empty) — at PROVISION time, via
the same provision-by-exec path that
installs credentials today (`go/internal/runtime/agent.go:266-272`), BEFORE
exec'ing the agent (T8); the agent, seeing the resume file, loads it through
the SDK's OWN native path — `setSessionFile` drains, reads the file via
`loadEntriesFromFile` (which parses the JSONL, runs
`elideSupersededCompactionEntries`, and validates the session header —
`session-loader.ts:202-228`, the elision at `:218`, header validation at
`:221-225`), migrates entries to the current version
(`session-manager.ts:985`), resolves blob refs (`:986`), and applies the
entries (`:1003`). No agent-side replay code exists.

Because the file is written BEFORE the agent is exec'd, no live input can
arrive before the agent is up — resume is a synchronous agent-startup load.
The replay barrier therefore DISSOLVES: `HoldForReplay` keeps having no
production caller (`go/internal/runner/gateway/control.go:421-429`, the doc
comment — "No production caller exists yet … exercised only by tests"; the
func body follows), and nothing in this record creates one. No storage
locator ever rides any message: there is no pointer row, no `ResumeContext`
envelope field, and nothing client-forgeable — the old F8 threat class is
structurally gone because the thing it protected no longer exists on the
wire. This does not touch the Runner's frame-forwarder contract, which is
about the relay LANES, not provisioning.

**No emit gate is needed on resume.** Loading a file via the SDK loader goes
through the backend's READ methods, never `append`/`writeFull` — so a resume
does not re-emit the loaded history; only post-resume NEW writes emit. One
deliberate edge: if `setSessionFile` flags a migration or sanitization
rewrite (`session-manager.ts:1007`, `:1012`), the SDK later fires a full-body
rewrite that lands as one checkpoint frame — correct by construction, since
server-side supersession treats it as a fresh snapshot, never a double-count.

**CHECKPOINT/compaction — supersession stays server-side.** The SDK still
does full-body rewrites the agent does not choose: `#rewriteAtomically`
(`session-manager.ts:621-635`) writes the full `#fileBody()` — title-slot
line first, then header, then ALL entries (`session-manager.ts:554-559`) —
via `writeTextAtomic` (`:655`), fired unconditionally on `appendCompaction`'s
superseded-compaction elision branch (`session-manager.ts:1544-1545`) and by
`rewriteEntries` (`:1560-1563`). Both vectors funnel through the injected
backend's `writeFull` (`indexed-session-storage.ts:143` and `:268`) while
plain appends go through `append` — so the tee has a PERFECT discriminator
for free: `append` → a delta `TranscriptEntry` (`checkpoint = false`),
`writeFull` → a checkpoint `TranscriptEntry` (`checkpoint = true`, the full
body as one entry payload). The SERVER's supersession keys on the flag: a
checkpoint frame supersedes every prior stored entry for the session, and the
reconstructed body is latest-checkpoint + later deltas (T4/T5). Because the
SDK's first write to a new session file is a full-body write (the wrapper
routes `writeTextSync` into `writeFull`, `indexed-session-storage.ts:139-143`),
a checkpoint — carrying the title slot and header — always precedes the first
delta, which is what makes T5's reconstruction a valid loadable session file.

### The SDK seam (grounded, revised for the tee)

The injection seam survives; what the backend DOES is now read-write. Rather
than implementing the 16-method, partly-synchronous `SessionStorage`
interface directly (`session-storage.ts:43-77`), the tee targets the SDK's
async-backend seam: `IndexedSessionStorage implements SessionStorage` with
`constructor(backend: SessionStorageBackend)`
(`indexed-session-storage.ts:101-103`), and the backend contract is ten async
methods (`indexed-session-storage.ts:25-36`):

```ts
export interface SessionStorageBackend {
  init(): Promise<void>;
  loadIndex(): Promise<Iterable<SessionStorageIndexEntry>>;
  readFull(path: string): Promise<string | null>;
  readSlices(path: string, prefixBytes: number, suffixBytes: number): Promise<[string, string]>;
  writeFull(path: string, content: string, mtimeMs: number, title?: SessionTitleUpdate): Promise<void>;
  append(path: string, line: string, mtimeMs: number): Promise<void>;
  updateSessionTitle(path: string, title: SessionTitleUpdate, mtimeMs: number): Promise<void>;
  truncate(path: string, mtimeMs: number): Promise<void>;
  remove(paths: string[]): Promise<void>;
  move(src: string, dst: string, mtimeMs: number): Promise<void>;
}
```

Properties the tee carries:

- **Real local reads, indexed.** `readFull`/`readSlices` serve the local
  file; `loadIndex` scans the local session dir. The index scan is
  load-bearing for resume: the wrapper's `readText` and `statSync` throw
  ENOENT for any path missing from the index
  (`indexed-session-storage.ts:204-207`, `:177-180`), and `setSessionFile`
  reads through exactly those (`readTitleSlotFromFile` at
  `session-manager.ts:973`, `loadEntriesFromFile(resolvedSessionFile,
  this.#storage)` at `:974`) — so the Runner-materialized file MUST be
  visible in `loadIndex` for the SDK loader to find it.
- **Tee'd writes, commit-order serialized.** `append` and `writeFull` write
  the local file AND emit the durable frame, AWAITING the send inside the op
  — the wrapper chains every op per path (the per-path tail map at
  `indexed-session-storage.ts:418-433`: `previous` maps each path to its
  queued tail; the new operation awaits it before running), so the
  per-session emit order IS the send order.
- **The drain barrier.** `IndexedSessionStorage.drain()` awaits the pending
  set and rethrows the first tracked error
  (`indexed-session-storage.ts:122-129`) — but it tracks only the APPEND
  vector: `writeTextSync` enqueues with `trackDrain: true`
  (`indexed-session-storage.ts:143`), while the `writeTextAtomic` checkpoint
  vector enqueues with `trackDrain: false` (`:270`), so a compaction-driven
  checkpoint mid-teardown is invisible to `storage.drain()`. The completeness
  guarantee for ALL durable sends — checkpoints included — is the SINK drain
  (`cli.ts:202-208`; `FrameSink.drain` at `frame.ts:52-58`), which retains
  every in-flight durable send; the composition root awaits
  `storage.drain()` beside it as a belt for the append vector.
- **Construction + the resume argument.** Fresh start: the manager is built
  with `SessionManager.create(cwd, SESSION_DIR, storage)`
  (`session-manager.ts:1839`; `SESSION_DIR` is HOME-relative — T2)
  and passed as `sessionManager`
  (`sdk.ts:544-545`). Resume: the Runner exports the materialized file's path
  as `COMPASS_RESUME_SESSION_FILE` on the agent exec (the same env seam that
  already carries `COMPASS_WORKDIR`/`COMPASS_MODEL`,
  `go/internal/runner/agent_exec.go:59-67`); when set, the composition root
  awaits `manager.setSessionFile(path)` — the SDK's designated resume seam
  ("Switch to a different session file (resume / branch)",
  `session-manager.ts:963-1013`) — BEFORE `createAgentSession`, so the
  session starts with its context applied. This is the resume-arg seam this
  record lands on: `create` + `setSessionFile` on the injected manager, not a
  new SDK option.

### The wire shape: TranscriptEntry

One additive oneof variant; SEA-1569 is the live precedent ("Additive to the
frozen oneof; buf-breaking-safe", `agent_pb.ts@153a2a4:425-428`). No control-
lane payload is added: the `TranscriptReplay` shell at `agent.proto@153a2a4:160`
stays EMPTY — nothing in this record fills, consumes, or depends on it, and
the SEA-1310 co-ratification is retracted (peer-contract relief; see the
Open Questions dispositions).

- **`TranscriptEntry transcript_entry = 7`** on the `AgentFrame` oneof (tags
  1–6 taken at `153a2a4`; the exact tag is re-confirmed at authoring, not
  load-bearing): `entry_json` (the SDK session entry verbatim), `checkpoint`
  (the full-snapshot-vs-delta discriminator), `entry_seq` (agent-stamped
  per-lifetime order, server-rebased to a session-scoped sequence — the
  identity model in T4). Full shape in T1.

### Erred-emit policy (R4): bounded buffer, escalate, fail the session at the cap

The durable lane's give-up path today resolves SILENTLY: after the bounded
retry, a definitively-erred frame returns without throwing
(`frame-sink.ts:109-115` at
`packages/compass-agent/src/transport/frame-sink.ts`: "Exhausted:
definitively erred. Do NOT throw"). For droppable telemetry that is right;
for the transcript it would be a HOLE in the durable log. The ruled policy:

- A definitively-erred transcript emit enters a bounded in-memory buffer and
  CONTINUES retrying, with escalating logs — warn at first, error as the
  buffer ages/grows.
- On CAP EXHAUSTION, FAIL THE SESSION loudly. The session is resumable from
  the last committed prefix — there is never a silent gap in the durable log.
- The cap value (bytes / entries / time) is a tuning parameter, explicitly
  NOT freeze-scope.

The SDK's own storage layer is the precedent that a durable-write failure is
fatal-by-design, not swallowed: once `#diskFailure` latches, every subsequent
append throws it (`session-manager.ts:674`).

### What stays out of scope

- **Server storage BACKEND choice.** The store is two-tier (a Postgres
  hot-tail + an object-store cold archive) per DL-019; WHICH object-store
  backend holds the archive (AWS S3 / Cloudflare R2 / self-hosted Garage or
  MinIO, endpoint-agnostic) and its credential posture remain Server-internal
  and swappable without touching the agent — that is still reason (3) of the
  storage-ownership ruling. This record specifies the store's contract (T4),
  not its eventual backend evolution.
- **The post-MVP analytics/index layer + retention GC.** The opt-in
  analytics/index layer built OFF the archive (Loki/ES/ClickHouse-style,
  rebuildable, never in the resume path — a new SEA-1580-adjacent follow-up)
  and the retention GC that reclaims ended-session PG hot-tails are both named
  seams, not built here.
- **The socket/control transport.** Frozen by the consolidation record;
  nothing here touches carrier, framing, or the `AgentControl` envelope —
  the parked `TranscriptReplay` shell stays parked, untouched.
- **Runner-side shipping.** No step ships logs after teardown; the Runner
  remains a pure verbatim forwarder on both relay lanes (materializing a
  file at provision is provisioning, not relaying).
- **The single-stream unification (SEA-1580).** Collapsing the durable
  settled-entry stream and the live per-token stream into ONE token stream
  carrying log metadata is the filed post-MVP follow-up; MVP ships the two
  streams above.

## Alternatives considered

### Agent-direct-to-S3 persistence — REJECTED by Matt's reversal (2026-07-31)

The 2026-07-30 draft of this record built exactly this: a Compass-owned S3
`SessionStorageBackend` injected via `createAgentSession({ sessionManager })`,
a segmented per-epoch object log under `sessions/<session_id>/`, a Postgres
pointer row, and a container-held S3 credential. Matt reversed it
(2026-07-31) for three stated reasons: **(1)** direct-S3 leaks a Server
storage detail into the agent — the agent should not know or care where
transcripts live; **(2)** it cuts blast radius and credential churn — with
zero agent creds, the old OQ-B full-bucket cross-agent read/tamper threat
vanishes at the agent entirely; **(3)** the Server can swap storage backends
later without touching the agent. The superseded machinery is inventoried in
the first appendix; the flipped ledger rows are DL-063/064/065/066/082/083.

### Control-lane replay resume — SUPERSEDED by the collapse (2026-07-31)

The interim revision of THIS record designed resume as a server-driven replay
down the SEA-1569 control lane: the Server pushed each stored entry as a
`TranscriptReplay` op via `DispatchControl`, the agent applied it through
`appendMessage`, a Runner replay barrier (`HoldForReplay`, gaining its first
production caller) held live input until `ReplayCompleteAck`, and a replay
admission bound capped the exempted admission path. Matt ruled it out: the
SDK already HAS a lossless, tested resume path — `loadEntriesFromFile`
reconstructs the full entry union including compaction handling
(`session-loader.ts:202-228`) and `setSessionFile` is its designated resume
seam (`session-manager.ts:963-1013`) — so replaying entry-by-entry over the
control lane rebuilds, op by op, what one materialized file gives for free,
while dragging in a barrier, an admission bound, and a cross-owner proto
ratification. The dead machinery is inventoried in the second appendix;
DL-086 is superseded by DL-087.

### A pure-emit backend + `readFull` serving the Runner-materialized file

Rejected for complexity. The backend could keep storing nothing locally and
instead answer `readFull`/`readSlices`/`loadIndex` from the one materialized
resume file. But that makes the backend a special-cased read shim — it must
synthesize an index entry for exactly one path, serve reads for it, and still
no-op every other storage method — while breaking the SDK's normal
compaction/rewrite cycle against local state it doesn't have. The tee keeps
the SDK's storage semantics fully real (reads, rewrites, blob resolution all
behave as on any filesystem) and adds exactly one behavior: emit on committed
write. Ephemeral local files are free; the writable layer dies with the
container either way.

### An event-stream tap or explicit emitter instead of the storage-backend seam

Rejected (OQ-R1, ruled: the tee backend). A tap on the session event stream
would have to re-derive the SDK's session-entry encoding (the exact bytes the
loader parses back); an explicit emitter forks the write path beside the
SDK's own. The backend seam sees the exact committed bytes at the exact
commit point, inherits per-path ordering (`indexed-session-storage.ts:418-433`)
and the drain barrier (`:122-129`), and reuses the injection seam the SDK
built for network stores (`session-storage.ts:70-74`).

### Implement `SessionStorage` directly instead of `SessionStorageBackend`

Rejected, unchanged. `SessionStorage` is 16 methods including synchronous
ones (`session-storage.ts:44-56`: `ensureDirSync`, `existsSync`,
`writeTextSync`, `statSync`, `listFilesSync`) that a custom impl can only
serve by maintaining the same in-memory index `IndexedSessionStorage` already
maintains (`indexed-session-storage.ts:93`; `:135-137`
`existsSync(path) { return this.#index.has(path); }`). The SDK built the
backend seam for exactly this case (`session-storage.ts:70-74`); re-deriving
its queueing and drain tracking would be a parallel convention beside an
existing one. The tee rides the same seam.

### A new `ResumeAgentSession` RPC instead of a request field

Rejected for v1, unchanged. Start and resume share every leg — resolve
container, relay over `SessionsResponse_Start`, exec the agent, record/verify
session rows — and differ only in the reconstruction + materialization kick
and the force-teardown fence. A sibling RPC would duplicate the dispatch case
(`go/internal/runner/dispatch.go:144-145`), the runnerhub command
(`commands.go:68-72`), and the host reasoning (`host.go:186-228`) to carry
one string. An additive proto3 field is buf-breaking-safe.

### Bind-mount the materialized file instead of exec-writing it

Noted as the alternative to T8's exec-write. The Runner could write the
reconstructed body to a host path and add a mount to the container spec
(`spec.Mounts`, `go/internal/runner/host.go:134`). Rejected for v1: the
exec-write path mirrors the existing provision-time setup family (egress
arming and credential install are both provision execs — the credential
installer even streams its payload over stdin the same way,
`go/internal/runtime/agent.go:266-272`), leaves no transcript data on the
Runner host filesystem after provision, and needs no mount-lifecycle
bookkeeping. A mount remains a valid future optimization for very large
transcripts.

### Disable or scope out auto-compaction for persisted sessions

Rejected, unchanged in substance. Auto-compaction fires when the context
window fills; disabling it strands a long resumed session with no way to free
context. The design ACCOMMODATES it twice over: a compaction full-body
rewrite funnels through the backend's `writeFull`
(`indexed-session-storage.ts:143`/`:268`) and lands as a `checkpoint = true`
frame the SERVER's supersession recognizes (T4), and on resume the SDK's own
loader elides superseded compactions natively
(`elideSupersededCompactionEntries`, `session-loader.ts:218`).

### Superseded AGENT-side S3-object-model alternatives (the backend choice is now live server-side)

The 2026-07-30 record adjudicated several alternatives that only exist inside
the AGENT-direct-S3 object model — a direct backend in the container,
per-epoch segments, a container-held credential; all stay superseded. But the
object-store BACKEND CHOICE is no longer moot: the SERVER now runs an
object-store cold archive tier (T4), so Garage (self-host) vs R2 (managed) vs
MinIO — endpoint-agnostic — is a LIVE Server-internal concern behind the
transcript store, decided at deploy, not in the agent. What stays retained by
citation, not carried: `create()`-then-assert vs `open()` path pinning (F2)
and the capture/restore epoch repoint — the local manager is ephemeral and
`create` is fine; a runtime file-identity assert and an epoch-aware
`writeFull` splitter — both replaced by the checkpoint discriminator +
server-side supersession; bucket-per-agent credential isolation on the AGENT
— moot, the agent has no credential at all (server-side prefix scoping is a
deployment-config concern, per E2/T4).

### Whole transcript bodies as permanent Postgres rows — REJECTED (the DL-084 model)

The interim revision (DL-084, 2026-07-31) kept every transcript entry as a
permanent Postgres row forever. Reversed by Matt (2026-07-31, DL-093) for DB
bloat: append-only bodies that are never queried by content churn TOAST,
vacuum, and backups without carrying their weight, while the object store
archives the same verbatim JSONL far more cheaply and analytics-ready. It
remains the DOCUMENTED FALLBACK if the object-store tier proves difficult to
land (Matt: "we can fall back to that if s3 proves difficult") — PG then holds
the whole history and the archive tier is deferred.

## Global Constraints

- **Matt's storage-ownership ruling (2026-07-31) is a fixed input**: durable
  storage is SERVER-OWNED. The agent tees its transcript over the existing
  durable agent→Runner→Server frame channel and holds **ZERO S3 credentials
  and zero bucket access**; its local session file is container-ephemeral
  working state, never a durable record. No task may give the agent a
  storage client, a storage credential, a storage endpoint env var, or a
  storage egress allowlist entry; no task may reintroduce a
  runner-ships-the-log step or put a storage locator on any client-forgeable
  message. The SERVER, by contrast, DOES hold the object-store
  client/credential/endpoint (the archive-tier storage-backend detail lives
  server-side, behind the transcript store) — this is the concrete form of
  "the Server can swap storage backends without touching the agent".
- **Matt's collapse rulings (2026-07-31) are fixed inputs**: one canonical
  artifact (the session log; the trace is a projection — SEA-1580 unifies
  the wire streams post-MVP); two agent→server streams for MVP (durable
  settled-entry + ephemeral per-token); resume is a Runner-materialized file
  loaded SDK-native — no task may add agent-side replay consumption, a
  replay barrier caller, a `TranscriptReplay` payload, or any SEA-1310
  dependency.
- **The durable emit lane's guarantees are the load-bearing invariant**:
  transcript frames ride the delivered-or-erred PostConversationFrame →
  CommitConversationFrame lane — agent-minted idempotency key stable across
  retries and unique across restarts (`frame-sink.ts:80-97`), bounded retry
  under a per-attempt deadline (`frame-sink.ts:96-122`), Server at-most-once
  commit keyed on it (`runner.proto@153a2a4:332-335`) — plus per-session
  ordering via the tee's awaited per-path ops and the `entry_seq` belt. A
  transcript frame NEVER rides the droppable Publish spine.
- **The `entry_seq` identity model is session-scoped, server-rebased**: the
  agent stamps `entry_seq` monotonic from 1 PER CONTAINER LIFETIME (it has no
  durable memory of prior lifetimes); the Server gap-checks the per-lifetime
  sequence and REBASES it onto the session's stored maximum at lifetime bind,
  so the persisted `entry_seq` is monotonic per SESSION across lifetimes and
  the store PK `(session_id, entry_seq)` holds across resumes (T4). Proto
  text and PK state the same model — no per-lifetime/per-session mismatch.
- **The erred-emit policy is fail-loud (R4)**: a definitively-erred
  transcript emit buffers under a bounded cap and keeps retrying with
  escalating warn→error logs; cap exhaustion FAILS THE SESSION (resumable
  from the last committed prefix). The cap value is tuning, not freeze-scope.
  Silent give-up (`frame-sink.ts:109-115`) is telemetry-only behavior and
  MUST NOT apply to the transcript lane.
- **Proto grounding is split by surface**: proto + generated bindings are
  grounded at compass origin/main `153a2a4` (SEA-1569 merged: `delivery_ack
  = 6` taken); hand-written Go/TS (`control.go`, `hub.go`, `handler.go`,
  `relay_comms.go`, `agent.ts`, `frame-sink.ts`, `cli.ts`) is unchanged
  between `335bb06` and `153a2a4` and cited at those files' current state.
- **OMP SDK floor: 16.5.2** (the version whose seams this record grounds:
  `SessionStorageBackend` at `indexed-session-storage.ts:25-36`,
  `sessionManager?` at `sdk.ts:544-545`, `setSessionFile` at
  `session-manager.ts:963-1013`, `loadEntriesFromFile` at
  `session-loader.ts:202-228`, `#rewriteAtomically` at
  `session-manager.ts:621-635`, `appendCompaction` elision at `:1544-1545`).
- **Proto changes are additive-only** (proto3 additions; buf-breaking-safe —
  SEA-1569 is the live precedent for an added oneof variant):
  `TranscriptEntry transcript_entry = 7` on the `AgentFrame` oneof (next free
  tag at `153a2a4`; re-confirm at authoring) and `resume_session_id` on the
  public `StartAgentSessionRequest`. The `TranscriptReplay` shell stays
  EMPTY — this record adds no control-lane payload and carries no SEA-1310
  dependency.
- **The checkpoint discriminator is mandatory on the emit path**: every
  `writeFull`-originated frame carries `checkpoint = true`; server-side
  supersession keys on it (T4), and T5's body reconstruction depends on it.
  Losing the discriminator double-counts compacted history on resume.
- **Migrations MUST be contiguous — a hard merge-order blocker.** The store
  refuses to boot on any gap: `checkContiguous` requires a gapless `1..N`
  sequence and returns `ErrSchemaVersion` at the first gap
  (`go/internal/store/store.go:271-278`, "embedded migrations not
  contiguous"). This record's transcript-entries migration takes the next
  contiguous slot at merge time — ≥ `0006`, since `0005_agent_persona.sql`
  already exists; the exact `NNNN` is assigned at merge (the slot floats
  with concurrent migrations) — authored by compass-server (the store is
  single-writer), FK-rooted in `agent_sessions` (created in
  `0003_agent_ownership.sql`).
- **Egress stays default-deny and UNCHANGED**: the agent reaches no storage
  endpoint, so no allowlist addition exists on this record.
- **Repo conventions**: Go per `golang-*` skills; commits Conventional; tests
  red→green per `rule://red-green-testing`; every task runs format + lint +
  tests for its area before done (`rule://pre-finish-checks`).

## Plan

Ordered by dependency; owners in brackets. T1 is the shared proto surface
(compass owns the repo's proto single-writer lane), T2–T3 compass-agent,
T4–T6 compass-server, T7–T8 compass-runner, T9 the end-to-end smoke. No task
gates on any cross-owner ratification: SEA-1310 is off the critical path.

### T1 [compass] — additive proto: `TranscriptEntry` frame + `resume_session_id`

Two additive changes, both buf-breaking-safe (proto3 field/variant
additions — the live precedent is SEA-1569, which added `delivery_ack = 6` to
the same oneof, `agent_pb.ts@153a2a4:425-428`):

```proto
// proto/compass/v1/agent.proto — AgentFrame oneof, additive. Tags 1-6 are
// taken at origin/main 153a2a4 (conversation_posted=1, conversation_updated=2,
// session=3, replay_complete_ack=4, control_ack=5, delivery_ack=6 — SEA-1569),
// so the next free tag is 7. Re-confirm at authoring time; the exact tag is
// not load-bearing.
message AgentFrame {
  oneof frame {
    // … existing variants 1-6 untouched …
    // One committed SDK session entry, teed upstream on the DURABLE
    // conversation-frame lane (PostConversationFrame → CommitConversationFrame).
    // The Server persists it; the agent's local copy is container-ephemeral.
    TranscriptEntry transcript_entry = 7;
  }
}

message TranscriptEntry {
  // One SDK session entry, verbatim in the SDK's own session-JSONL encoding
  // (the exact bytes the tee backend committed locally). Opaque to the proto
  // layer by design — the entry union embeds provider-specific payloads no
  // compass.v1 message represents (agent.proto@153a2a4:110-115), and the only
  // consumers are the Server store and the SDK's own session loader (via the
  // Server-reconstructed resume file).
  string entry_json = 1;
  // Full-snapshot-vs-delta discriminator: true = this entry is a complete
  // full-body snapshot (an SDK compaction/title rewrite via the backend's
  // writeFull) that SUPERSEDES every prior stored entry for the session; the
  // Server's supersession keys on this (T4). False = a plain delta append.
  bool checkpoint = 2;
  // Agent-stamped, monotonic from 1 PER CONTAINER LIFETIME (the agent has no
  // durable memory across restarts). The Server gap-checks this per-lifetime
  // sequence and REBASES it onto the session's stored maximum at lifetime
  // bind, so the persisted entry_seq is monotonic per SESSION and
  // (session_id, entry_seq) keys the store across resumes (T4). The
  // idempotency key on the durable envelope dedups retries.
  uint64 entry_seq = 3;
}
```

```proto
// proto/compass/v1/compass.proto — StartAgentSessionRequest, additive
message StartAgentSessionRequest {
  string container_name = 1;
  string initial_prompt = 2;
  // When set, the session in this container resumes the identified persisted
  // logical session: the Server (subscriber-authz gated) reconstructs the
  // stored transcript into a session-JSONL body the Runner materializes into
  // the new container at provision. Empty = fresh. No storage locator ever
  // rides any request — storage is Server-internal.
  string resume_session_id = 3;
}
```

```proto
// proto/compass/v1/runner.proto — the INTERNAL resume-body carrier message.
// It attaches as a TOP-LEVEL field on the SessionsResponse relay envelope (a
// sibling of request_id, OUTSIDE the command oneof — the leg commands.go:68-72
// forwards that envelope), never inside the verbatim public `start` variant.
// Additive, INTERNAL-ONLY: rides the path-filtered internal gen lane, never
// the public client surface. The Server attaches it on an authorized resume;
// no client can supply it, and the public request is relayed verbatim (T6).
message ResumeBody {
  // The reconstructed post-supersession session-JSONL body (T5) — string,
  // consistent with TranscriptEntry.entry_json above.
  string session_body = 1;
  // Inline-image blob bytes are OUT of MVP scope (SEA-1582): no grounded
  // agent-side capture seam exists, so the carrier holds only the JSONL body.
}

// SessionsResponse gains resume_body as a TOP-LEVEL sibling (beside request_id,
// OUTSIDE the command oneof) — NOT a field inside the `start` variant, which
// stays the frozen public StartAgentSessionRequest relayed verbatim.
message SessionsResponse {
  // string request_id = 1; oneof command { start = 2 … secrets_version = 8 }
  // unchanged — `start` is still the verbatim public request.
  ResumeBody resume_body = 9;  // fresh internal tag; populated only on resume
}
```

The carrier is a TOP-LEVEL sibling field on the internal `SessionsResponse`
envelope (`ResumeBody resume_body = <N>`, beside `request_id` and OUTSIDE the
`command` oneof), NOT a field inside the `start` variant — `start` stays the
frozen public `StartAgentSessionRequest` relayed verbatim. `<N>` is a FRESH
internal tag confirmed at authoring time (next free after `secrets_version =
8`) — NOT the retired `ResumeContext resume = 12` slot (F8/DL-065): reusing a
freed tag, even internal-only, invites confusion, and the record's
additive-only posture favors a fresh tag. The exact tag is authoring-time and
not load-bearing.

The `TranscriptReplay` shell (`agent.proto@153a2a4:160`) is NOT touched:
resume never rides the control lane, so the shell stays empty and parked.

Interfaces: the two proto shapes above; regenerated Go + TS bindings via the
existing gen lanes.

Test cycle: buf lint + breaking gates; frame round-trip tests in
`control-wire.test.ts` conventions (a `transcriptEntry` frame serializes to a
single top-level AgentFrame key and round-trips).

### T2 [compass-agent] — the tee seam: `TranscriptTeeBackend` + durable sink lane + resume load at the composition root

New file `packages/compass-agent/src/session-tee.ts`: a
`SessionStorageBackend` (`indexed-session-storage.ts:25-36`, the ten async
methods) backed by the container-local filesystem that TEES committed writes
upstream, wrapped in the SDK's `IndexedSessionStorage` and injected at the
`cli.ts` composition root (`createAgentSession({ sessionManager })`; today
`main()` passes only `cwd` + `modelPattern`, `cli.ts:154-161`):

- `append(path, line, mtimeMs)` — write the line to the local file, then emit
  one delta frame:
  `sink.emit({ kind: "transcriptEntry", value: { entryJson: line, checkpoint: false, entrySeq: next() } })`,
  awaited (see Ordering).
- `writeFull(path, content, mtimeMs, title?)` — write the local file
  atomically, then emit one CHECKPOINT frame (`checkpoint: true`,
  `entryJson = content`). Both SDK full-body rewrite vectors funnel here —
  `#rewriteAtomically` writes the full `#fileBody()` (title-slot first,
  `session-manager.ts:554-559`) via `writeTextAtomic` (`:655`), fired
  unconditionally on `appendCompaction`'s elision branch (`:1544-1545`) and
  by `rewriteEntries` (`:1560-1563`), and the wrapper routes them into the
  backend's `writeFull` (`indexed-session-storage.ts:143` / `:268`) — so a
  checkpoint frame ALWAYS means "supersedes all prior entries" and a delta
  ALWAYS rides `append`.
- `readFull`/`readSlices` — REAL local reads; `loadIndex` — a REAL scan of
  the local session dir. Load-bearing for resume: the wrapper throws ENOENT
  for un-indexed paths (`indexed-session-storage.ts:177-180`, `:204-207`),
  and `setSessionFile` reads the resume file through the wrapper
  (`session-manager.ts:973-974`), so the Runner-materialized file must
  appear in the scan. `updateSessionTitle`/`truncate`/`move`/`remove` —
  local-only for v1 (titles are a Server-side rendering concern; nothing on
  the durable path depends on them).
- **Ordering.** The backend AWAITS each emitted frame's durable send inside
  the op the wrapper queues — the per-path tail chain
  (`indexed-session-storage.ts:418-433`) serializes ops per path, so awaiting
  the send inside the op makes the per-session emit order the send order.
  The durable lane supplies the rest: delivered-or-erred bounded retry under
  a per-attempt deadline with a stable idempotency key
  (`frame-sink.ts:80-122`), committed at-most-once server-side on that key.
- **Erred-emit (R4).** The transcript lane does NOT inherit the sink's
  silent give-up (`frame-sink.ts:109-115` — telemetry semantics): a
  definitively-erred transcript send enters the backend's bounded buffer and
  keeps retrying with escalating warn→error logs; at cap exhaustion the
  backend surfaces a fatal session error (the SDK's `#diskFailure` latch is
  the precedent that a durable-write failure is fatal-by-design,
  `session-manager.ts:674`). Cap value is tuning, not freeze-scope.
- **No emit gate.** Resume loads through the READ methods; only post-resume
  NEW writes emit. A migration/sanitization rewrite flagged by
  `setSessionFile` (`session-manager.ts:1007`, `:1012`) lands as one
  checkpoint frame — safe under server-side supersession.
- **Construction + resume load.** Fresh:
  `SessionManager.create(cwd, SESSION_DIR, storage)`
  (`session-manager.ts:1839`). `SESSION_DIR` is a record-introduced constant
  (today `main()` passes no session dir, `cli.ts:154-161`) and is
  HOME-relative, under the agent's scoped `$HOME` — mirroring the auth-seed
  anchoring (`authSeedPath(home)`, `cli.ts:48-49`; `cli.ts:147-152` already
  throws if HOME is unset) — so the session dir never depends on a
  populated checkout (sealed#1019: no auto-clone). Resume: when
  `COMPASS_RESUME_SESSION_FILE` is
  set (T8 exports it on the agent exec beside
  `COMPASS_WORKDIR`/`COMPASS_MODEL`, `go/internal/runner/agent_exec.go:59-67`),
  the composition root awaits `manager.setSessionFile(path)`
  (`session-manager.ts:963-1013`: drain → `loadEntriesFromFile` →
  `migrateToCurrentVersion` → `resolveBlobRefsInEntries` → apply) BEFORE
  `createAgentSession` — SDK-native load, no replay code.
- **Drain barrier.** The existing `finally` already drains the sink before
  closing the transport (`cli.ts:202-208`) — the sink drain is the
  COMPLETENESS guarantee for durable sends including checkpoints
  (`FrameSink.drain`, `frame.ts:52-58`: the sink retains every in-flight
  durable send). Add `await storage.drain()`
  (`indexed-session-storage.ts:122-129`) beside it as a belt for the APPEND
  vector only: `writeTextSync` tracks drain (`indexed-session-storage.ts:143`,
  `trackDrain: true`) but the `writeTextAtomic` checkpoint vector does not
  (`:270`, `trackDrain: false`), so `storage.drain()` never covers a
  compaction checkpoint.

The sink itself gains the lane: `OutboundFrame` (`frame.ts:43-46`) gains
`{ kind: "transcriptEntry"; value: TranscriptEntry }`, classified DURABLE in
`createSocketFrameSink` — it rides the PostConversationFrame unary beside
`conversationPosted`/`conversationUpdated` (`frame-sink.ts:125-145`), never
the droppable Publish spine.

Interfaces:

```ts
// packages/compass-agent/src/session-tee.ts
import { IndexedSessionStorage, type SessionStorageBackend } from "@oh-my-pi/pi-coding-agent";
import type { FrameSink } from "./frame";

/** Container-local FS backend that tees every committed write upstream as a
 *  durable TranscriptEntry frame. Reads are real local reads (the SDK's own
 *  loader serves resume); erred emits buffer + retry and fail the session at
 *  the bounded cap (R4). */
export class TranscriptTeeBackend implements SessionStorageBackend {
  constructor(sink: FrameSink, sessionDir: string);
  // the ten SessionStorageBackend methods (indexed-session-storage.ts:25-36):
  // append → local append + awaited delta frame;
  // writeFull → local atomic write + awaited checkpoint frame;
  // readFull/readSlices/loadIndex → real local FS; rest → local-only.
}

/** Backend wrapped in the SDK adapter, initialize()d, ready to inject. */
export async function createTeeSessionStorage(sink: FrameSink, sessionDir: string): Promise<{ storage: IndexedSessionStorage; backend: TranscriptTeeBackend }>;

// frame.ts — OutboundFrame gains the variant (kind matches the generated
// oneof case name 1:1, per the existing contract, frame.ts:22-24):
//   | { readonly kind: "transcriptEntry"; readonly value: TranscriptEntry }

// cli.ts additions (composition root only): build sink → build tee storage →
// SessionManager.create(cwd, SESSION_DIR, storage) →
// if COMPASS_RESUME_SESSION_FILE: await manager.setSessionFile(it) →
// pass as sessionManager.
export interface MainDeps {
  createSession?: (options: CreateAgentSessionOptions) => Promise<{ session: AgentSession }>;
  createTransport?: (socketPath: string) => RunnerTransport;
  /** NEW seam, mirrors the existing two: tee-storage constructor for tests. */
  createSessionStorage?: (sink: FrameSink, sessionDir: string) => Promise<{ storage: IndexedSessionStorage; backend: TranscriptTeeBackend }>;
}
```

Test cycle (red→green): unit tests in
`packages/compass-agent/src/session-tee.test.ts` over a recording FrameSink +
a temp session dir — append writes locally AND emits a delta frame with the
verbatim line + monotonic entry_seq; writeFull writes locally AND emits a
checkpoint frame; local reads round-trip what was written; ops for one path
emit in commit order even under interleaved slow sends (the awaited-op
ordering); a compaction-shaped writeFull after appends emits checkpoint AFTER
the deltas it supersedes; a definitively-erred send buffers, retries,
escalates its log level, and fails the session at the cap (R4). cli tests
over `MainDeps.createSessionStorage`: manager built over the tee storage and
passed as `sessionManager`; `COMPASS_RESUME_SESSION_FILE` set → the manager
loads the file before session creation; drain runs on the error path too.

### T3 [compass-agent] — the resume proof: in-container SDK-native-load smoke

No agent-side resume code exists to build — the SDK loader IS the resume
path — so this task is the PROOF that the loader carries the model, plus the
tee tests that need a loaded-session fixture:

- Smoke fixture (test-only, no Runner): run one `main()` against a fake
  transport, emit two turns through the tee, and capture the durable
  TranscriptEntry frames; reconstruct the session-JSONL body the way T5 does
  (latest checkpoint body + later delta lines); write it to a temp session
  dir; start a second `main()` with `COMPASS_RESUME_SESSION_FILE` pointing at
  it; assert the second session's context contains the first run's turns
  (loaded via `setSessionFile` → `loadEntriesFromFile`,
  `session-manager.ts:963-1013` / `session-loader.ts:202-228`), that NO
  TranscriptEntry frames are emitted during the load — the reconstructed
  fixture is current-version, so no load-time SDK migration/sanitization
  rewrite fires (reads themselves never tee), and
  that a post-resume turn emits deltas with a fresh per-lifetime entry_seq
  starting at 1 (the server-rebase model, T4), and that the whole load is
  checkout-independent — the loader touches only the session dir under
  `$HOME`, and the fixture carries no populated repo checkout (consistent
  with sealed#1019's no-auto-clone container).
- A compaction round-trip: a fixture body whose file contains a superseded
  compaction loads with the SDK's own elision
  (`elideSupersededCompactionEntries`, `session-loader.ts:218`) — proving the
  reconstruction needs no compaction awareness beyond T4's checkpoint
  supersession.

Interfaces: none new — consumes T1/T2's seams.

Test cycle: this IS the test; red first (`COMPASS_RESUME_SESSION_FILE` is
ignored today — no composition-root resume path exists), green after T2.

### T4 [compass-server] — the two-tier durable transcript store: PG hot-tail + object-store cold archive

The Server has NO durable transcript store today: `deliverSession` routes
session frames to the session-tail stream + lifecycle pub only — never the DB
(`hub.go:447-455`); only comms Messages and the `agent_sessions` ownership row
persist (`go/internal/store/agent_sessions.go:31-56`); and the server carries
NO object-store client at all today (`go.mod` has none — grounded). This task
builds a TWO-TIER store: a Postgres HOT-TAIL holding `[latest checkpoint .. now]`
= the resume set, plus an S3-compatible object-store COLD ARCHIVE of superseded
history (verbatim JSONL segments) indexed by a PG manifest. Resume reads the PG
hot-tail ONLY in normal operation (T5); the archive is the permanent, complete,
analytics-ready history and feeds a post-MVP opt-in analytics layer built off it.

- **Migrations** in `go/internal/store/migrations/` — the `NNNN` prefix is the
  next contiguous slot at merge time, ≥ `0006` given `0005_agent_persona.sql`
  already exists; the exact `NNNN` is assigned at merge (`checkContiguous`
  fails the boot on any gap, `go/internal/store/store.go:271-278`). Two tables,
  BOTH FK-rooted in `agent_sessions` (created in `0003_agent_ownership.sql`) —
  `agent_session_transcript_entries` (the hot-tail: same columns/PK/UNIQUE as
  the interim DL-084 shape, but its CONTRACT is now "holds
  `[latest checkpoint .. now]`, pruned at flush", NOT "one row per entry
  forever") and the new `agent_session_archive_segments` manifest, added in the
  same contiguous slot family:

```sql
-- HOT TAIL. Holds only [latest checkpoint .. now] = the resume set, not the
-- whole history: superseded entries are flushed to the object store and PRUNED
-- from this table (only at flush, never on teardown). entry_seq is
-- SESSION-scoped and monotonic across container lifetimes (the server rebases
-- each lifetime's agent-stamped sequence onto the session's stored maximum at
-- lifetime bind); idempotency_key carries the durable lane's at-most-once
-- guarantee into this table; checkpoint marks a full-body snapshot that
-- supersedes all prior entries for the session (the read view starts at the
-- latest checkpoint).
CREATE TABLE agent_session_transcript_entries (
    session_id      TEXT   NOT NULL REFERENCES agent_sessions (session_id) ON DELETE RESTRICT,
    entry_seq       BIGINT NOT NULL,
    checkpoint      BOOLEAN NOT NULL DEFAULT FALSE,
    entry_json      TEXT   NOT NULL,
    idempotency_key TEXT   NOT NULL UNIQUE,
    PRIMARY KEY (session_id, entry_seq)
);

-- ARCHIVE MANIFEST. One row per flushed object-store segment (verbatim JSONL).
-- kind='superseded' segments hold pre-checkpoint history and are NEVER read on
-- resume; kind='safety_valve' segments hold entries AFTER the latest checkpoint
-- evicted by the high size-cap and ARE spliced back on resume (T5) — a later
-- checkpoint re-marks any now-pre-checkpoint safety_valve row to 'superseded'
-- (below), so a safety_valve row is post-latest-checkpoint by construction;
-- kind='session_end' segments archive the retained post-checkpoint tail at
-- teardown for analytics completeness and are NEVER read on resume (the PG tail
-- stays authoritative). The object key is prefixed sessions/<session_id>/;
-- bucket/endpoint are server config, not per-row.
CREATE TABLE agent_session_archive_segments (
    session_id    TEXT   NOT NULL REFERENCES agent_sessions (session_id) ON DELETE RESTRICT,
    object_key    TEXT   NOT NULL,
    min_entry_seq BIGINT NOT NULL,
    max_entry_seq BIGINT NOT NULL,
    kind          TEXT   NOT NULL CHECK (kind IN ('superseded', 'safety_valve', 'session_end')),
    PRIMARY KEY (session_id, object_key)
);
```

- **The `entry_seq` identity model (fixes the per-lifetime/PK mismatch).**
  The wire `entry_seq` is agent-stamped, monotonic from 1 PER CONTAINER
  LIFETIME — the agent has no durable memory of prior lifetimes, so it cannot
  continue a session-scoped sequence itself. The STORE's `entry_seq` is
  session-scoped: at lifetime bind — the StartAgentSession event that records
  the new live session row (T6), NOT the first frame receipt — the server
  snapshots `base = max(entry_seq)` for the session ONCE, write-once onto
  the live session row, and persists each incoming frame at
  `stored_base + frame.entry_seq`, read from the row per frame and never
  recomputed, gap-checking the per-lifetime sequence before rebase. Because
  base is a write-once property of the bind event rather than a cursor
  inferred from "first frame receipt", a retry of the very first frame reads
  the SAME stored base → the same persisted entry_seq → the idempotency key
  dedups the row; a first-frame retry can never re-snapshot base against an
  already-advanced max (no double-rebase). The PK `(session_id, entry_seq)`
  therefore holds across resumes — the first post-resume delta lands at
  `base + 1`, never colliding with a prior lifetime's rows. (A per-lifetime
  discriminator on the frame — e.g. the live session id, `host.go:200`
  `nextID` — would let the server group by lifetime instead, but is a wire
  change and unnecessary given the T6 bind event.) Proto comment (T1) states
  the same model.
- **Persist-on-receipt.** The durable lane's server terminus is
  `Hub.CommitConversationFrame` (`relay_comms.go:158-186` — the at-most-once
  commit keyed on the agent-minted `idempotency_key`,
  `runner.proto@153a2a4:332-335`), NOT the lossy `Deliver` path
  (`hub.go:237-255`, which serves PublishEvents). The hub's commit switch
  gains the `AgentFrame_TranscriptEntry` case → `AppendTranscriptEntry`,
  fail-closed on an unbound session exactly like the conversation variants.
- **Checkpoint supersession, server-side.** The hot-tail READ view (T5)
  reconstructs from the latest checkpoint forward: entries at or before the
  newest `checkpoint = true` row are superseded (the checkpoint row IS the full
  body). The checkpoint discriminator semantics are unchanged — a
  `checkpoint = true` frame is the SDK's context-window compaction full-body
  rewrite teed as a checkpoint (DL-089). Under the two-tier model, supersession
  is not merely a read filter: the superseded entries are FLUSHED to the
  archive and PRUNED from the hot-tail (below), so the hot-tail converges on
  `[latest checkpoint .. now]`.
- **Flush machinery (PG → S3, PUT-before-prune).**
  `FlushSuperseded(sessionID, uptoEntrySeq, kind)` writes the flushed entries as
  ONE verbatim-JSONL segment object to the object store; ONLY after the PUT is
  confirmed does it, in ONE Postgres transaction, INSERT the
  `agent_session_archive_segments` manifest row AND prune those rows from the
  hot-tail — so the manifest row and the prune commit atomically (a crash leaves
  either both or neither). Crash-safe by ordering AND idempotency: the segment
  object key is deterministic per `[min..max]` seq range, so a crash between the
  PUT and the commit re-flushes onto the SAME key (harmless re-PUT); the
  manifest INSERT is `ON CONFLICT (session_id, object_key) DO NOTHING`, so a
  re-run after a committed row can never wedge on a PK violation; a crash before
  the PUT leaves the hot-tail intact — no entry is ever lost to a flush crash.
  Three triggers, each with a named call-site seam:
  - **PRIMARY: compaction / checkpoint arrival** — invoked inside
    `AppendTranscriptEntry` when a `checkpoint = true` row commits. The flush
    target is ALL pre-latest-checkpoint entries STILL IN the hot-tail (not a
    fixed `[min..max]` captured at first attempt), so it is self-healing: if a
    crash lands after the checkpoint row commits but before the flush txn
    commits — and the retried checkpoint frame dedups on `idempotency_key` and
    short-circuits before re-invoking the flush — the un-pruned rows are ignored
    on resume (the read view filters to post-latest-checkpoint) and the NEXT
    compaction's flush idempotently reclaims them, bounding the bloat window to
    one compaction cycle. The superseded entries
    (`[previous checkpoint .. before this one]` still in PG) flush as one
    `superseded` segment; the hot-tail then holds `[new checkpoint .. now]`. In
    the SAME transaction as the flush's manifest INSERT + prune it re-marks any
    existing `safety_valve` manifest row whose `max_entry_seq < the new
    checkpoint seq` to `kind='superseded'` (a manifest-only UPDATE — the S3
    object is unmoved, only reclassified from resume-eligible to analytics-only),
    so no crash window leaves a superseded safety_valve row resume-eligible and
    a safety-valve segment a later checkpoint has superseded can NEVER be spliced
    ahead of that checkpoint on resume. Compaction fires at a predetermined
    context-token limit, so this bounds the hot-tail to ~one compaction window in
    normal operation.
  - **SAFETY VALVE: high size-cap** — checked in the same `AppendTranscriptEntry`
    path. If the hot-tail exceeds a HIGH size cap WITHOUT a compaction firing
    (compaction disabled/bugged/never-trips), flush the oldest post-checkpoint
    chunk as a `safety_valve` segment and prune. The cap sits ABOVE the normal
    compaction window so it never engages in normal operation — pure
    defense-in-depth. Its ONLY consequence: part of the post-checkpoint resume
    set now lives in S3, so resume falls back to S3 for those (T5). The cap
    value is a tuning parameter, not freeze-scope.
  - **SESSION-END archival** — invoked from the `StopAgentSession`/teardown
    handler. Flush the remaining hot-tail window to the archive as one
    `session_end` segment so the history is COMPLETE for analytics; do NOT prune
    the PG tail, and `session_end` is NEVER read on resume — a died/restarted
    agent still resumes fast from the retained, authoritative PG tail.
    Reclaiming ended-session PG tails is a NAMED post-MVP retention GC (a seam,
    not built here).
- **Object-store client seam (server-internal).** compass-server has NO
  object-store client today (`go.mod` has none — grounded); this task ADDS the
  dependency and its config. The client sits behind a small server-internal
  seam (`PutSegment(ctx, key, body) error`, plus a segment fetch for T5's
  fallback) so the backend is endpoint-agnostic (Garage/R2/MinIO) and
  swappable/mockable in store tests. Config is a `COMPASS_S3_*`-style
  endpoint/bucket/credential on the SERVER, mirroring the existing
  `COMPASS_DATABASE_DSN` flag/env precedence
  (`cmd/compass-server/main.go:61-110`). The agent and Runner never hold any of
  it (DL-089 stands).
- **Trace projection (R1).** The same store is the source for the
  block-level client trace: the Server projects the RETAINED hot-tail (= the
  post-compaction displayed view in normal operation) to clients, so the durable
  log and the displayed history do not diverge in normal operation.
  Pre-checkpoint history and any safety-valve-evicted segments are served, if
  ever, by the post-MVP analytics layer, not the live trace. The ephemeral
  per-token stream (R2) remains a separate liveness-only leg.

Interfaces:

```go
// go/internal/store/agent_transcripts.go (new file beside agent_sessions.go)

// AppendTranscriptEntry persists one emitted entry at-most-once on
// idempotencyKey (a duplicate key is a silent success — the retry dedup).
// lifetimeSeq is the agent-stamped per-lifetime sequence; the store rebases
// it onto the session's max entry_seq at lifetime bind (gap-checked).
// Unknown session_id is ErrInvalidArgument (the FK).
func (s *Store) AppendTranscriptEntry(ctx context.Context, sessionID string, lifetimeSeq uint64, checkpoint bool, entryJSON, idempotencyKey string) error

// SessionTranscript returns the hot-tail post-supersession view: the latest
// checkpoint entry (if any) followed by every later delta held in PG, ordered
// by entry_seq. This is the PG-only normal resume set. Unknown/empty session
// is ErrNotFound.
func (s *Store) SessionTranscript(ctx context.Context, sessionID string) ([]TranscriptEntryRow, error)

// FlushSuperseded writes the hot-tail entries up to uptoEntrySeq as one
// verbatim-JSONL segment via the object-store seam; after the PUT is confirmed
// it INSERTs the manifest row (kind) and prunes those PG rows in ONE
// transaction (both commit or neither). Crash-safe: a crash between PUT and
// commit re-flushes onto the deterministic key and the manifest INSERT is
// ON CONFLICT (session_id, object_key) DO NOTHING, so a re-run cannot wedge; a
// crash before the PUT leaves PG intact.
func (s *Store) FlushSuperseded(ctx context.Context, sessionID string, uptoEntrySeq uint64, kind SegmentKind) error

// SafetyValveSegments returns the manifest rows for a session whose kind is
// 'safety_valve' (entries after the latest checkpoint evicted to S3), ordered
// by min_entry_seq — the only segments T5's reconstructor fetches on resume.
// By construction these are all post-latest-checkpoint: the PRIMARY flush
// re-marks any safety_valve row a later checkpoint superseded to 'superseded',
// so a stale pre-checkpoint segment is never returned. Empty for a normal
// session ('superseded' and 'session_end' segments are excluded — never read
// on resume).
func (s *Store) SafetyValveSegments(ctx context.Context, sessionID string) ([]ArchiveSegmentRow, error)

type TranscriptEntryRow struct {
    EntrySeq   uint64
    Checkpoint bool
    EntryJSON  string
}

type ArchiveSegmentRow struct {
    ObjectKey   string
    MinEntrySeq uint64
    MaxEntrySeq uint64
    Kind        SegmentKind
}

// ObjectStore is the server-internal, endpoint-agnostic archive seam
// (faked in store tests; a real Garage/R2/MinIO client in production).
type ObjectStore interface {
    PutSegment(ctx context.Context, key string, body []byte) error
    GetSegment(ctx context.Context, key string) ([]byte, error)
}
```

Test cycle: store tests — FK rejection; idempotent duplicate key; ordering by
entry_seq; the rebase (two lifetimes' sequences land as one gapless
session-scoped sequence; the first post-resume delta does NOT collide);
supersession view (deltas → checkpoint → deltas returns checkpoint + tail
only, never a double-count); not-found. Flush tests (object-store seam FAKED —
no live S3): a compaction flush writes one `superseded` segment + a manifest
row + prunes the flushed PG rows so the hot-tail holds `[checkpoint..now]`;
PUT-before-prune ordering (a failed PutSegment leaves PG intact and records NO
manifest row); manifest-INSERT + prune atomicity (a simulated crash after the
PUT but before commit leaves NEITHER a row nor a prune, and the re-run completes
via ON CONFLICT DO NOTHING); PG-only reconstruction after a compaction flush
(the object store is NOT read); a size-cap flush records a `safety_valve`
manifest row; a `safety_valve` eviction FOLLOWED BY a later compaction re-marks
the stale safety_valve row to `superseded` so `SafetyValveSegments` returns it
no more; a session-end flush records a `session_end` segment WITHOUT pruning the
PG tail.
Hub test: a committed TranscriptEntry frame lands a row under the bound
session, fail-closed NotFound on an unbound one.

### T5 [compass-server] — the resume body reconstructor

On a resume start (T6), the Server assembles the post-supersession
session-JSONL body and hands it to the Runner for materialization (T8). No
control-lane ops, no `DispatchControl`, no RESOURCE_EXHAUSTED loop — this is a
pure read-and-concatenate. In NORMAL operation it reads the PG hot-tail ONLY;
the object-store archive is NOT touched on the resume hot path, so object-store
latency/consistency never gates a container restart:

- `SessionTranscript(session_id)` (T4) yields the PG hot-tail — latest
  checkpoint + later deltas in order, the entire resume set in normal
  operation. The checkpoint's `entry_json` IS a full SDK file body —
  title-slot line first, then header, then all entries
  (`#fileBody()`, `session-manager.ts:554-559`) — because it was captured
  verbatim from the backend's `writeFull`. The reconstructor emits the
  checkpoint body verbatim, then appends each later delta's `entry_json` as
  one line.
- The result is by construction a valid loadable session file: the SDK's
  first write to any session file is a full-body write (the wrapper routes
  `writeTextSync` into `writeFull`, `indexed-session-storage.ts:139-143`),
  so a checkpoint always exists and always carries the header
  `loadEntriesFromFile` validates (`session-loader.ts:221-225`). Superseded
  compactions inside the body are the SDK loader's own job
  (`elideSupersededCompactionEntries`, `session-loader.ts:218`) — the
  reconstructor never parses entry JSON.
- **S3 fallback — ONLY when the safety valve fired.** `SafetyValveSegments`
  (T4) reports whether any `safety_valve` segments hold post-checkpoint entries
  for this session. If any exist, the reconstructor emits the latest-checkpoint
  full body FIRST, then merges every later delta — from the fetched S3
  segment(s) and the PG tail alike — ordered by entry_seq BEHIND that checkpoint
  body, so the reconstructed file is header-first and complete. This is the ONLY
  resume path that touches the object store, and it does not fire in normal
  operation — normal sessions have no `safety_valve` segments, so resume stays
  PG-only.
- Inline-image blobs are out of MVP scope (SEA-1582): the reconstructed body
  carries only the session-JSONL. On load the SDK still runs
  `resolveBlobRefsInEntries` / `resolveImageData` (`session-loader.ts:265-269`;
  `blob-store.ts:256-266`); with no blob dir a missing ref logs a warning and
  is returned unchanged — the SDK does not crash, only inline-image context
  degrades.

Interfaces:

```go
// go/internal/runnerhub (beside the SEA-1569 dispatch path)

// ReconstructSessionBody assembles the post-supersession session-JSONL body
// for sessionID: the latest checkpoint's full body verbatim FIRST, then every
// later delta merged by entry_seq — from the PG hot-tail alone in normal
// operation, or from PG plus any safety_valve archive segments (fetched and
// merged behind the checkpoint body) when the manifest shows them.
// ErrNotFound for an unknown or empty session.
func (h *Hub) ReconstructSessionBody(ctx context.Context, sessionID string) ([]byte, error)
```

Test cycle: hub tests — N stored entries reconstruct to checkpoint body +
N-tail lines in entry order; a checkpointed store reconstructs checkpoint +
tail only (no double-count); the reconstructed bytes parse as a valid SDK
session file (header-first — asserted against a fixture captured from a real
tee run); a normal resume reads the PG hot-tail ONLY (the object-store seam is
asserted un-called); a session with a `safety_valve` manifest segment emits the
checkpoint body FIRST, then merges the fetched segment and the PG tail by
entry_seq (header-first, no double-count); not-found on an
unknown session.

### T6 [compass-server] — resume identity + authorization + body handoff

Resume reuses `StartAgentSession` with the additive public `resume_session_id`
(T1), not a new RPC — start and resume share every leg and the relay plumbing
exists end to end (`commands.go:68-72` relays `SessionsResponse_Start{Start: req}`).
The stable logical `session_id` keys the stored transcript; the freshly minted
live id (Runner `nextID`, `host.go:200`) tracks the new container lifetime —
the (session, lifetime) split survives, encoded as the server-side `entry_seq`
rebase (T4). Authz: `resume_session_id` is gated by the existing
`RequireAgentSessionSubscriber` (`go/internal/store/agent_sessions.go:74-98`, the
constant-shape EXISTS query that merges unknown-session and non-member into
one `ErrNotFound`). There is NO pointer row and NO storage locator on any
message — nothing forgeable exists. On a resume: verify authz, record the new
live session row — snapshotting the `entry_seq` rebase base onto it
write-once at this bind event (T4; a first-frame retry re-reads the stored
base, never re-snapshots) — call T5's reconstructor, and carry the
reconstructed body to the Runner on the INTERNAL start
envelope — an internal
field outside the public request, so no client can supply a body. The Runner
materializes it at provision (T8).

Interfaces: handler wiring over T4/T5 signatures; the internal
`SessionsResponse` relay envelope gains the T1-specified internal-only
`ResumeBody` carrier as a TOP-LEVEL sibling field (`resume_body`, outside the
`command` oneof — see T1); additive — the public `start` request is still
relayed verbatim, carrying only the authz-checked `resume_session_id`, no
locator, no body.

Test cycle: handler tests — resume with unknown/foreign id fails NotFound
before any Runner call; an authorized resume reconstructs and attaches the
body to the internal envelope; a fresh start attaches nothing; the stored
transcript is keyed on the stable logical id across two resumes.

### T7 [compass-runner] — admit the new durable variant; NO storage provisioning

The Runner's job on the RELAY lanes SHRINKS. Agent-side S3 provisioning is
GUTTED: no S3 endpoint host service, no S3 env in `AgentEnv.execSpec()`, no
egress allowlist addition — the agent holds zero storage detail. What remains
is ONE guard widening on the durable lane:

- Upstream: `PostConversationFrame` → `CommitConversationFrame` forwards the
  `AgentFrame` VERBATIM (`post_conversation_frame.go:32-77`,
  `gateway.go:100-108` — the committer narrows to the one RPC and the Runner
  "sends the session_id it structurally owns and the agent's frame verbatim").
  But the lane's guard is CLOSED today: `isConversationFrame`
  (`go/internal/runner/gateway/post_conversation_frame.go:95-105`) is a
  two-arm switch (`ConversationPosted`/`ConversationUpdated`,
  `default: return false`), and `PostConversationFrame` rejects anything
  else `CodeInvalidArgument` at `:54-56` — a `transcript_entry` frame is
  rejected TERMINAL at the gateway today. Widen `isConversationFrame` (or
  add a sibling predicate) to admit `AgentFrame_TranscriptEntry`; this pairs
  with T4's hub-side change (the `Hub.commitFrame` switch gains the
  `AgentFrame_TranscriptEntry` case, T4), so gateway and hub guards widen as
  ONE coherent change. The C4 constraint doc comment on
  `CommitConversationFrameRequest` (`runner.proto@153a2a4:303-306`) is
  revised on T1's proto surface to state the durable lane now also carries
  `transcript_entry`.
- Downstream: nothing new rides the control relay for this record (resume is
  provisioning, not control ops).

Interfaces: the `isConversationFrame` widening above; the deliberate ABSENCE
of `AgentEnv` S3 fields is the other interface change (the old T7's
`S3Endpoint/S3Bucket/...` fields are never built).

Test cycle (red→green): gateway test — a `transcript_entry` frame posted by
the agent is rejected `CodeInvalidArgument` today (red); after the widening
it REACHES the fake committer byte-identical with the idempotency key
(green).

### T8 [compass-runner] — resume orchestration: force-teardown + materialize the session file at provision

`agentHost.Start` threads resume: force-teardown of the old container remains
the primary writer fence (fencing a zombie EMITTER; the durable lane's
idempotency keys + the server-side entry_seq rebase are the belt). On a
resume start the Runner:

1. **Force-tears-down** the prior container (unchanged fence).
2. **Materializes the reconstructed body at provision.** Provision already
   composes exec-driven setup before the agent runs: `Launch` creates +
   starts the container (main process `sleep infinity`,
   `go/internal/runtime/agent.go:214-216`), arms egress, and installs
   credentials via an exec'd `sh -s` with the payload on stdin
   (`go/internal/runtime/agent.go:266-272` — the surviving precedent:
   sealed#1019 deletes the server-side auto-clone, and credentials install
   to the agent's HOME, which exists pre-clone — HomeDir and CheckoutDir
   are distinct, `go/internal/runtime/workspace.go:75-79`); a provision-time
   exec failure surfaces at provision
   (`TestFailedProvisionRemovesThePartialContainer`,
   `go/internal/runtime/agent_test.go:240-243`). The materializer is one
   more exec in this family: write the session-JSONL body into the agent's
   HOME-relative session dir (under the scoped
   `$HOME`, mirroring the auth-seed materialization, `cli.ts:48-49` — resume
   never requires a populated checkout) as the agent user, content over
   stdin — the credential-install pattern. The bind-mount alternative
   (`spec.Mounts`, `go/internal/runner/host.go:134`) is noted in
   Alternatives and not taken for v1.
3. **Execs the agent pointed at the file**: `AgentEnv.execSpec()` gains
   `COMPASS_RESUME_SESSION_FILE` beside `COMPASS_WORKDIR`/`COMPASS_MODEL`
   (`go/internal/runner/agent_exec.go:59-67`), set only on a resume start.

No barrier exists on this path: the file is complete before the agent is
exec'd, so there is nothing for live input to race. `HoldForReplay`
(`go/internal/runner/gateway/control.go:421-429`, the doc comment; the func
body follows) keeps its zero production callers; this record does not touch it.

Interfaces:

```go
// host.go — no signature change; Start reads the resume body from the
// internal envelope, force-tears-down the prior container, materializes the
// body into the new container via provision exec, and sets
// COMPASS_RESUME_SESSION_FILE on the agent exec.
```

Test cycle: host tests — a resume Start force-tears-down, execs the
materializer write BEFORE the agent exec, and sets
`COMPASS_RESUME_SESSION_FILE`; a fresh Start does none of those; a
materializer exec failure fails the provision (no half-resumed agent); the
written bytes reach the container verbatim (fixture runtime capture).

### T9 [all] — end-to-end resume smoke

Against a live Runner + Server: provision container A, start a session, run
two turns, assert the Server's transcript store holds the teed entries;
`StopAgentSession`, tear the container down; provision container B,
`StartAgentSession{resume_session_id}`, and assert: the old container is
force-torn-down; the reconstructed session file exists in container B's
session dir before the agent answers; the new session's first response
demonstrates prior context (loaded from the materialized file, not from any
surviving container state); a post-resume turn lands NEW entries in the store
under the same stable logical id at rebased entry_seq; and a mid-session
compaction produces a checkpoint row that the next resume materializes as
checkpoint + tail with no double-count. Two-tier assertions: after the
mid-session compaction, the superseded entries are archived to an object-store
segment AND an `agent_session_archive_segments` manifest row exists for it AND
the PG hot-tail is pruned to `[checkpoint..now]`; the resume that follows reads
PG-only — the object store is asserted NOT read on the normal resume path. This
is the acceptance gate for SEA-1570.

Test cycle: this IS the test — an integration test in the e2e suite
(`go/internal/runner/e2e_transport_test.go` conventions).

## Tasks

- [ ] T1 [compass] additive proto: `TranscriptEntry transcript_entry = 7` on the AgentFrame oneof (tags 1-6 taken at 153a2a4; re-confirm at authoring), `TranscriptEntry{entry_json, checkpoint, entry_seq}` with the per-lifetime-stamped / server-rebased entry_seq comment, `resume_session_id` public on `StartAgentSessionRequest`; internal `ResumeBody` (`string session_body`; inline-image blobs OUT of MVP scope, SEA-1582) as a TOP-LEVEL sibling field on the INTERNAL `SessionsResponse` envelope (outside the `command` oneof, NOT inside `start`) on a fresh internal tag (never the retired `ResumeContext=12` slot); `TranscriptReplay` shell untouched (buf gates; round-trip tests)
- [ ] T2 [compass-agent] `TranscriptTeeBackend` + `createTeeSessionStorage` in `packages/compass-agent/src/session-tee.ts`: local-FS read/write + awaited tee emit (append→delta frame, writeFull→checkpoint frame), real loadIndex over the session dir, R4 erred-emit buffer/escalate/fail-session, `OutboundFrame` + durable sink lane for `transcriptEntry`, `COMPASS_RESUME_SESSION_FILE` → `setSessionFile` at the composition root, drain barrier beside the sink drain, `MainDeps.createSessionStorage` seam (unit + cli tests)
- [ ] T3 [compass-agent] resume proof-smoke: reconstruct a captured tee run into a session-JSONL body, restart `main()` with `COMPASS_RESUME_SESSION_FILE`, assert SDK-native context load (`setSessionFile`/`loadEntriesFromFile`), no tee emission during load, fresh per-lifetime entry_seq after resume; compaction-elision round-trip
- [ ] T4a [compass-server] PG hot-tail tier: `NNNN_agent_session_transcript_entries.sql` (next contiguous slot, ≥0006 given `0005_agent_persona.sql`, exact NNNN assigned at merge; PK (session_id, entry_seq) with SESSION-scoped entry_seq via write-once lifetime-bind rebase base, UNIQUE idempotency_key, checkpoint flag; FK `agent_sessions`, created in `0003_agent_ownership.sql`) holding `[latest checkpoint..now]` (pruned at flush) + `AppendTranscriptEntry`/`SessionTranscript` store funcs + persist-on-receipt case in `Hub.CommitConversationFrame` + checkpoint-supersession read view + trace projection source + `agent_session_archive_segments` manifest table + PG-only reconstruction (store + hub tests)
- [ ] T4b [compass-server] S3 cold-archive tier: server-internal endpoint-agnostic object-store client (`PutSegment`/`GetSegment` seam, NEW `go.mod` dep + `COMPASS_S3_*` config mirroring `COMPASS_DATABASE_DSN` precedence at `cmd/compass-server/main.go:61-110`; agent/Runner hold none) + `FlushSuperseded` writer (verbatim JSONL segment, manifest row, PUT-before-prune) triggered at compaction (primary), high size-cap safety valve, and session-end archival; ended-session PG-tail retention GC is a named post-MVP seam (store tests, object-store seam faked)
- [ ] T5 [compass-server] resume body reconstructor `Hub.ReconstructSessionBody`: latest checkpoint body verbatim + later delta lines from the PG hot-tail ONLY in normal operation (S3 not touched), splicing `safety_valve` archive segments ahead of the tail only when the manifest shows them; valid SDK session-JSONL by construction (hub tests incl. loadability fixture + PG-only + fallback)
- [ ] T6 [compass-server] resume identity + authz: `resume_session_id` gated by `RequireAgentSessionSubscriber` before any Runner call; stable logical id keys the stored transcript across resumes; NO pointer row, NO locator on any message; reconstructed body rides the INTERNAL `SessionsResponse` sibling field only (outside the `start` command, handler tests)
- [ ] T7 [compass-runner] widen the durable-lane guard: `isConversationFrame` admits `transcript_entry` (today rejected `CodeInvalidArgument`; pairs with T4's hub `commitFrame` case + T1's C4 proto-comment revision); agent-side S3 provisioning/egress/env GUTTED — never built (gateway red→green tests)
- [ ] T8 [compass-runner] `agentHost.Start` resume orchestration: force-teardown primary fence; materialize the reconstructed JSONL into the new container's session dir at provision (exec-write, credential-install pattern); exec the agent with `COMPASS_RESUME_SESSION_FILE`; NO barrier (host tests)
- [ ] T9 [all] end-to-end resume smoke against live Runner + Server — the SEA-1570 acceptance gate

## Open Questions — all ruled (Matt, 2026-07-31); dispositions recorded

The 2026-07-31 storage-ownership reversal opened five forks (OQ-R1..R5); the
interim revision designed against stated assumptions and parked them for
Matt. Matt has now ruled everything. No fork remains live; the dispositions
are recorded here so the asked questions stay citable.

### OQ-R1 — the emit seam → RESOLVED: the tee backend

Ruled (a), refined into the tee: the injected `SessionStorageBackend` on the
`createAgentSession({ sessionManager })` seam (`sdk.ts:544-545`) uses the
container-local ephemeral FS for read AND write and tees each committed write
upstream as a durable `TranscriptEntry` frame — `append` → delta,
`writeFull` → checkpoint. The pure-emit variant (store nothing, `readFull`
no-op) died with the replay resume model: R3's resume loads a FILE through
the SDK's own loader, so the backend must serve real reads (see Alternatives
for the rejected read-shim variant). Decided design point, not a fork.

### OQ-R2 — SEA-1310 sequencing → DISSOLVED (peer-contract relief)

The fork existed only because resume WAS `TranscriptReplay`. Under R3 resume
never touches the control lane, the `TranscriptReplay` shell stays empty
(`agent.proto@153a2a4:160`), and no payload needs co-ratification. SEA-1310
no longer blocks SEA-1570 in either direction — an explicit RELIEF on the
peer contract; the co-ratification request is retracted (the driver is
messaging compass to retract it). The former T9 is deleted.

### OQ-R3 — inline-image blobs → OUT of MVP resume scope (deferred to SEA-1582)

MVP resume persists the SDK session-JSONL transcript losslessly — all text,
tool-call, and reasoning context. Inline-image blob BYTES are out of MVP
scope: no grounded agent-side capture seam exists. The tee is a
`SessionStorageBackend` with no blob hook (`indexed-session-storage.ts:25-36`);
the SDK's `BlobStore` is internal with no injection seam
(`session-manager.ts:456`); and inline images are externalized to
`blob:sha256:` refs BEFORE the JSONL line reaches the tee (`#lineFor`,
`session-manager.ts:542` → `prepareEntryForPersistence`,
`session-persistence.ts:264`), so `TranscriptEntry.entry_json` carries refs,
not image bytes. On resume the SDK's own load-time resolution still runs
(`setSessionFile` → `resolveBlobRefsInEntries`, `session-manager.ts:986`;
`session-loader.ts:265-269`), resolving each ref against the container-local
`BlobStore` (`blob-store.ts:256-266`); with no blob dir materialized a missing
ref logs `Blob not found for image reference` and is returned unchanged — the
SDK warns and does NOT crash, only inline-image context degrades. Agent-side
blob capture is deferred to SEA-1582.

### OQ-R4 — ordering/durability of the emit lane → RESOLVED (R4 + await-per-op + entry_seq)

Ordering: the tee AWAITS each transcript send inside its per-path queued op
(`indexed-session-storage.ts:418-433` serializes ops per path), making
per-session emit order the send order; the `entry_seq` belt (per-lifetime
agent stamp, server gap-check + rebase, T4) covers the rest. Durability of
the give-up path is the R4 ruling: a definitively-erred transcript emit
(today's silent return at `frame-sink.ts:109-115`) enters a bounded buffer
with continued retry and escalating warn→error logs, and CAP EXHAUSTION
FAILS THE SESSION loudly — resumable from the last committed prefix, never a
silent hole. Precedent that durable-write failure is fatal-by-design: the
SDK's `#diskFailure` latch (`session-manager.ts:674`). Cap value = tuning.

### OQ-R5 — replay admission bound → DISSOLVED

There is no control-lane replay, so there is no replay admission path to
bound. The retention-cap exemption warning (`control.go:549-555` at
`go/internal/runner/gateway/control.go`) remains accurate and remains
someone else's obligation IF anyone ever fills the `TranscriptReplay` shell
— this record never does.

## Appendix — superseded by the reversal (2026-07-31): the agent-direct-S3 model

Matt's 2026-07-31 ruling reversed storage ownership (agent-direct-to-S3 →
server-owned). This appendix marks the 2026-07-30 model's machinery
SUPERSEDED — recorded here rather than silently deleted, and the target of the
flipped ledger rows (DL-063, DL-064, DL-065, DL-066, DL-082, DL-083). None of
it is build scope.

- **The segmented per-epoch S3 log** — stable prefix `sessions/<session_id>/`,
  per-lifetime `<epoch>.jsonl` objects, the readFull-LIST-concat /
  append-single-object asymmetry, and checkpoint-aware read-side
  reconstruction (title-slot supersession). Superseded: the agent stores
  nothing durable; one durable frame per committed entry replaces the object
  model. (DL-064.)
- **Garage/R2 and the endpoint-agnostic S3 client** — Garage per Runner host
  for solo self-host, Cloudflare R2 with prefix-scoped tokens for real
  deployments, `COMPASS_S3_ENDPOINT` + `Bun.S3Client`. Superseded for the
  agent: the storage backend choice is now a Server-internal concern behind
  the Server's own transcript store; the agent and Runner never see it.
  (DL-063.)
- **The full-bucket credential posture (OQ-B)** — the cross-agent read/tamper
  threat and the R2-prefix-token hardening follow-up. Superseded: the agent
  holds ZERO storage credentials, so the threat class vanishes at the agent;
  credential posture moves wholly server-side. (DL-083.)
- **The Postgres pointer row + internal `ResumeContext` envelope field (F8)**
  — `agent_session_transcripts` (bucket/prefix/endpoint) and the
  non-forgeable internal relay of the resolved pointer. Superseded: the
  transcript store is Server-internal; no bucket/prefix ever needs to reach
  the Runner or the agent, so there is nothing to relay and nothing to forge.
  `resume_session_id` (public, authz-gated) survives as the only request
  addition. The internal envelope EVOLVES rather than silently regaining the
  field: the collapsed model's resume body rides a NEW internal `ResumeBody`
  carrier on a FRESH internal tag (T1/T6), never the retired `= 12` slot.
  (DL-065.)
- **Epoch-token writer fencing (OQ-A belt)** — the zombie-writer-clobbers-own-
  dead-segment property of the object key. Superseded: force-teardown remains
  the primary fence against a zombie EMITTER, and the Server store's
  at-most-once idempotency-key commit plus the session-scoped entry_seq
  rebase absorb what the epoch belt guarded. (DL-082.)
- **Continuous per-append PUT + terminal drain (OQ-2)** — becomes continuous
  per-entry durable frame emit + the same terminal `drain()` barrier (the
  loss-bound discussion carried over to the emit lane and is now closed by
  the R4 erred-emit ruling).
- **SEA-1310 independence (OQ-3, DL-066)** — first INVERTED by the interim
  replay model (co-ratification), then DISSOLVED by the collapse: no
  `TranscriptReplay` dependency remains in either direction (see the second
  appendix).
- **Blobs to an S3 sibling keyspace (OQ-4)** — superseded; settled as the
  OQ-R3 outcome: inline-image blobs are out of MVP resume scope, deferred to
  SEA-1582.
- **OQ-5 endpoint provenance / OQ-6 ranged readSlices / OQ-7 creds-via-env** —
  moot: no agent-side endpoint, no agent-side remote reads, no agent-side
  credentials.
- **OQ-C (the auto-compaction checkpoint hazard) — DISSOLVED at the agent;
  residual folded server-side.** The hazard was: the SDK fires a full-body
  rewrite the agent does not choose — `#rewriteAtomically`
  (`session-manager.ts:621-635`) writes the full `#fileBody()` (title-slot
  line FIRST, then header, then all entries — `session-manager.ts:554-559`,
  title-first at `:554-556`) via `writeTextAtomic` (`:655`), fired
  unconditionally on `appendCompaction`'s superseded-compaction elision branch
  (`session-manager.ts:1544-1545`) and by `rewriteEntries` (`:1560-1563`) —
  which defeated the old agent-side file-identity assert and forced
  checkpoint-aware read-side reconstruction. Under server-owned storage this
  dissolves at the agent: the agent never reconstructs from a rewritten file
  (resume loads a Server-reconstructed body through the SDK's own loader,
  which handles compaction natively — `session-loader.ts:218`). The RESIDUAL
  is build scope, not a fork: both rewrite vectors funnel through the
  backend's `writeFull` (`indexed-session-storage.ts:143` and `:268`) while
  plain appends go through `append`, so the tee maps `writeFull` → a
  `TranscriptEntry` frame with `checkpoint = true` (the
  full-snapshot-vs-delta discriminator, T2) and the SERVER's supersession
  drops all prior stored entries for the session on receipt (T4). SDK
  auto-compaction therefore stays safe — an unchosen full-body rewrite lands
  as a checkpoint frame the server recognizes and never double-counts.

## Appendix — superseded by the collapse (2026-07-31): the control-lane replay resume model

Matt's 2026-07-31 rulings replaced the interim revision's resume half
wholesale. This appendix marks that machinery SUPERSEDED — recorded rather
than silently deleted, and the target of the DL-086 → DL-087 ledger flip.
None of it is build scope.

- **The `TranscriptReplay` payload** (`agent_message_json` + index/total,
  filling the parked shell at `agent.proto@153a2a4:160`) — dead. Resume
  never rides the control lane; the shell stays empty. The agent-side
  decoder (`JSON.parse` into the `control.message` slot at `agent.ts:150-152`)
  and its index/total gap detection are never built.
- **The control-lane replay driver** (the former T5: `Hub.ReplayTranscript`
  pushing per-entry ops down `SessionsResponse.deliver_control` /
  `DispatchControl`, `runner.proto@153a2a4:183-203`, with a
  RESOURCE_EXHAUSTED refuse-and-redeliver cursor loop) — dead. T5 is now the
  body reconstructor; the `DispatchControl` relay carries nothing for this
  record.
- **The replay barrier / first production `HoldForReplay` caller** (the
  former T8's core: `HoldForReplay` at
  `go/internal/runner/gateway/control.go:421-429` — the doc comment; the
  func body follows) — dissolved. The session file is
  materialized BEFORE the agent is exec'd, so no live input can race the
  load; `HoldForReplay` keeps zero production callers ("exercised only by
  tests. Not dead code; not yet reachable", `control.go:421-429`).
- **The replay admission bound (OQ-R5)** — dissolved with the admission path
  itself. The in-place warning about filling the shell
  (`control.go:549-555`) stays with the shell, not with this record.
- **The SEA-1310 co-ratification (OQ-R2, the former T9)** — retracted.
  Resume no longer depends on any `AgentControl` payload, so the
  cross-record co-ratification is dropped and SEA-1310 is unblocked from
  SEA-1570 in both directions (peer-contract relief; DL-086 → DL-087).
- **The agent-side emit gate during replay** — dissolved. Replay applied
  entries through `appendMessage`, which would have re-emitted without a
  gate; the SDK-native load goes through the backend's READ methods, which
  never tee, so no gate exists to build.
