# Compass agent↔Runner transport consolidation

Status: Active

> Freezes on merge; later changes supersede by
> citation, never rewrite (`../compass-0.5/design.md:10-12`, convention restated
> in `../compass-0.6/design.md:1116-1118`). Extends the frozen
> `../compass-agent-runner-transport/design.md` (merged as #849, RIG-1351) and
> supersedes-by-citation its Decision #2 and its OQ-8 (Matt's ruling,
> 2026-07-22: "get everything onto the unix socket(s)"), plus the v0.6 §T5
> stdio-*carrier* clauses those two rest on (`../compass-0.6/design.md:1416-1418`
> stdout, `:1439-1442` stdin) — the frame/control CONTENT contract in that same
> section stands; only the "newline-protojson on the pipe" carrier statements
> are overridden.

## Problem / Intent

The frozen transport record put agent-initiated *calls* on a per-container Unix
socket but deliberately left the stdout `AgentFrame` telemetry spine and the
(unbuilt) stdin control lane on stdio (`../compass-agent-runner-transport/design.md:41-43`,
Decision #2 "Off stdio"). Matt has now ruled the opposite: ALL agent↔Runner
traffic consolidates onto that one socket. This record designs the two
migrations — telemetry (agent→Runner, replacing the stdout relay) and control
(Runner→agent, replacing the parked stdin decoder) — as streaming RPCs on the
same `AgentGateway` service, retiring the bespoke newline-protojson framing so
every channel shares Connect's framing, correlation, and cancellation.

**The carrier swap is not free where the pipe's guarantees were load-bearing.**
A pipe cannot drop a frame (a blocked pipe *delays*; only a process/relay death
ends it, loudly) and cannot reorder within a stream. A socket stream CAN drop
mid-session (Runner restart, listener hiccup) and the telemetry spine carries
frames of two durability classes — an opaque, loss-tolerable execution trace
AND durable conversation frames that commit to comms `Message` rows. So the
failure-model layer is where this record does its real design work; the RPC
shapes themselves are the easy part. OQ-2 and OQ-6 carry that weight.

**Scope boundary.** The socket itself — its lifecycle, perms, bind-mount, uid
map, stale recovery, and the `Comms` unary — is the frozen record's; nothing
there is reopened. The Runner→Server leg (`PublishEvents`,
`proto/compass/v1/runner.proto:63-70`) is also untouched: the Runner
still relays frames upward Runner-sequenced; only the agent→Runner hop below it
changes carrier.

## Approach

### What is superseded, exactly

By citation (the frozen records are never edited):

- **Decision #2 "Off stdio"** (`../compass-agent-runner-transport/design.md:41-43`:
  "The stdout `AgentFrame` telemetry spine and the (unbuilt) stdin control lane
  are NOT this channel… stdio keeps its §T5 shape"). Overridden by Matt's
  2026-07-22 ruling: telemetry and control ALSO ride the socket. The rest of the
  frozen Decisions (#1 Runner-sole, #3 one seam, #4 per-container socket, #5
  additive network impl) stand unchanged — this record depends on them.
- **OQ-8** (`../compass-agent-runner-transport/design.md:583-589`), which
  recommended leaving telemetry on stdout. Superseded by the ruling. In
  fairness OQ-8's affirmative argument was properties-based ("a one-way,
  high-volume stream whose verbatim-relay + gap-detection properties are well
  served by the pipe"), and those properties are *preserved*, not discarded, by
  a client-stream RPC (one-way, ordered, Runner-sequenced — see Telemetry). What
  OQ-8 got wrong is treating the pipe as the only carrier with them; it is not.
- **The v0.6 §T5 stdio-carrier clauses** (`../compass-0.6/design.md:1416-1418`
  "stdout therefore carries newline-delimited protojson frames"; `:1439-1442`
  "stdin carries newline-delimited protojson frames of an additive internal
  `AgentControl`"). Decision #2 defers to exactly these, so they must be named
  too or a v0.6 reader has no citation trail to the change. Superseded: the
  *carrier* is the socket. The frame/control message CONTENT contract in that
  same section (the `AgentFrame`/`AgentControl` oneof variants, the durable-vs-
  trace split, the replay barrier) is UNCHANGED — this record moves where those
  messages travel, not what they are. The `frozen` stdio-framing comments in
  `control.ts:1-25` / `frame.ts:54-58` are code that C4/C5 edit anyway.

### What the migrations preserve (the load-bearing properties)

Today's stdout spine: the agent's `ProtojsonLineSink` writes one `AgentFrame`
per newline-protojson line (`packages/compass-agent/src/frame.ts:59-78`);
the Runner's `eventPublisher.relay` scans stdout, decodes each line, and sends
it up `PublishEvents` stamped `RunnerSeq: p.seq.Add(1)`
(`go/internal/runner/relay.go:142-167`), with an undecodable line
logged and skipped (`relay.go:152-156`); the hub records the sequence and
detects gaps (`go/internal/runnerhub/hub.go:125-126,146-152`). The
properties to preserve: **one-way, ordered, high-volume, Runner-sequenced at
the Runner (not the agent), gap-detectable upstream, malformed-frame tolerant.**

Today's stdin lane: nothing. `AgentControl` is deliberately undefined on the
wire (`proto/compass/v1/agent.proto:73-85` doc comment: it "lands
with its decoder once its payload shapes are settled" — RIG-1310's parked
payload decision); only the typed domain union + `ControlSource =
AsyncIterable<AgentControl>` exist
(`packages/compass-agent/src/control.ts:30-58`), and the Runner
writes nothing down stdin (the pipe exists,
`go/internal/runtime/podman.go:454-455` `StreamingIO{Stdin: stdin, …}`,
but has no writer). The control migration therefore replaces a *parked plan*,
not built code — the cheapest possible time to change carrier.

### Telemetry: `Publish` client-stream on `AgentGateway`

A new RPC on the frozen internal service
(`../compass-agent-runner-transport/design.md:316-318`):

```proto
// agent -> Runner, client-stream: the loss-tolerable telemetry spine. The agent
// opens ONE stream per session at boot and sends every TRACE and SESSION
// AgentFrame on it, in emission order. Durable conversation frames do NOT ride
// this stream — they take the correlated PostConversationFrame unary (OQ-2(c),
// below). Replaces the trace/session half of the stdout newline-protojson
// relay. INTERNAL surface.
rpc Publish(stream PublishFrameRequest) returns (PublishFrameResponse);
```

Direction note: from Connect's perspective the *agent* is the client dialing
the Runner's socket server, so agent→Runner telemetry is a **client-stream**
(the request half carries the frames), the mirror of `RunnerService.PublishEvents`
one hop up (`proto/compass/v1/runner.proto:70`). "Server-stream"
in the brief's sense — a one-way stream flowing INTO the server — is exactly
this; a Connect `server streaming` RPC would flow the wrong way.

- **Runner-sequencing stays at the Runner.** The `Publish` handler assigns
  `RunnerSeq` exactly where `relay` does today (`relay.go:157-160`) — the agent
  never mints the upstream sequence, so the gap-detection contract
  (`runner.proto:63-70`: "seq assigned at the Runner, not at Server publish …
  in-transit loss is detectable as a gap") is untouched, and the hub's
  `recordSeq` path (`hub.go:125-126,178-188`) needs no change. This is the crux
  of the loss model (OQ-2): because seq is minted at the Runner on receipt, a
  frame the agent drops BEFORE it reaches the Runner is never sequenced and
  produces NO gap.
- **Ordering** is HTTP/2 stream ordering — same in-stream order the pipe gave,
  minus the hand-rolled scanner and its 4 MiB line cap (`relay.go:145`). A frame
  that fails protojson decode cannot exist on this carrier (Connect delivers
  typed messages); the skip-on-undecodable arm (`relay.go:152-156`) retires with
  the scanner. But the failure mode does not vanish — it changes shape: a
  malformed/oversized message on the socket surfaces as a Receive() error that
  tears down the WHOLE `Publish` stream, where today one bad line is skipped and
  the session continues. The agent is semi-trusted container code, so the peer
  is not axiomatically well-behaved; C2 therefore pins an explicit
  `WithReadMaxBytes` bound (Global Constraints) and routes the stream error into
  OQ-2's reconnect path.
- **The trace/session queue is BOUNDED at the agent seam (P1 #2).** HTTP/2 flow
  control exists on the wire, but `FrameSink.emit()` is synchronous
  fire-and-forget (`frame.ts:50-52`; C4 keeps that contract for trace/session),
  so a slow Runner would produce unbounded in-agent queueing and container OOM —
  the userland-buffering posture Node stdout has today, now a real risk once the
  durable class no longer shares this queue. C4 therefore bounds the `Publish`
  send buffer at a named cap and drops-oldest-and-counts trace frames on
  overflow (loss-tolerable by OQ-2(c)); the durable path is NOT on this queue —
  it applies backpressure through its awaited unary. This is the OQ-2 cap made
  concrete under option (c).
- **Durable conversation frames take the correlated unary, NOT this stream
  (OQ-2(c), P1 #1).** `conversation_posted`/`conversation_updated` commit to
  comms `Message` rows (`hub.go:129-133`), so an agent-side drop on the lossy
  spine would be a silent gapless loss (the OQ-2 finding). Under (c) the agent
  classifies by frame kind (`frame.ts:43-46`) and routes the durable class to
  `PostConversationFrame` — request/response, delivered-or-erred, retried under
  a stable `idempotency_key` (at-most-once commit, so a lost-response retry is
  not duplicated) until the Runner accepts it — while the opaque trace/session
  frames stay on the fire-and-forget `Publish` stream. C1/C2/C4 carry the split;
  the Runner sequences both paths through one ordered per-session publisher
  (allocation order == emission order) so concurrent traffic opens no false gap
  and hub gap-detection is unchanged.
- **One stream per session, opened at agent boot, closed at agent exit.** The
  handler treats stream end as today's stdout-EOF (relay done); mid-session
  stream failure semantics are OQ-2.
- **`SessionFrame` opacity is untouched.** The frame payload is the same
  `AgentFrame` message; the `SessionFrame.event` opaque OMP bytes and the
  `state` the hub extracts for the board
  (`proto/compass/v1/agent.proto:68-71`, `hub.go:134-136`) ride
  inside it unchanged — the carrier swap is invisible above the Runner (OQ-3
  records the verification).

### Control: `Control` server-stream on `AgentGateway`

```proto
// agent -> Runner subscribe; Runner -> agent stream: the control lane. The
// agent opens the stream at boot; the Runner pushes one AgentControl per
// message. Replaces the never-built stdin decoder. INTERNAL surface.
rpc Control(ControlSubscribeRequest) returns (stream AgentControl);
```

The agent (Connect client) cannot be dialed, so Runner→agent push rides the
response half of an agent-opened server-stream — the same dial-direction
inversion `RunnerService.Sessions` uses one hop up
(`proto/compass/v1/runner.proto:52-61`: the Server "pushes session
*commands* downward" on the response stream of a Runner-opened RPC).

- **The wire message is the frozen-variant `AgentControl` oneof** — variant
  names/types per the v0.6 ratification
  (`../compass-0.6/design.md:1439-1451`), payload fields still owned by
  RIG-1310's parked decision (`agent.proto:76-85`). This record moves the
  CARRIER; it does not decide the payload shape (OQ-1 flags the interaction).
  The agent-side `ControlSource` seam is already carrier-blind
  (`control.ts:55-58`: "The wire decode … lives entirely behind this") — the
  socket impl is just a second source behind the same interface. One asymmetry
  to note: the built domain union has SIX members (`control.ts:30-53` —
  prompt/steer/askAnswer/config/replay/replayComplete); the v0.6-ratified wire
  oneof has SEVEN, adding `deliver` (`../compass-0.6/design.md:1442`). C1 lands
  all seven on the wire; C4's dispatcher fast-paths `deliver` (below) rather
  than through the domain union, so the union extension is deferred to when a
  built `deliver` domain op exists (RIG-1310/RT-3) — the seam is carrier-blind
  for the six it knows and routes the seventh explicitly.
- **Mid-turn delivery is off the turn's await — a dispatcher, with a stated
  ordering contract.** The agent's control loop is strictly sequential and its
  `prompt` arm awaits the whole turn
  (`packages/compass-agent/src/agent.ts:84-86`:
  `for await (const control of this.#control) { await this.#applyControl(control); }`;
  `:142` `await this.#agent.prompt(control.input)`), so anything routed
  through that pull loop queues behind the running turn — the deadlock class
  the comms socket already dissolved for call results
  (`../compass-agent-runner-transport/design.md:504-514`, the same event-loop-
  delivery insight the frozen record's T5 proved for comms results). The socket
  `ControlSource` therefore consumes the Connect stream on the Node event loop
  into a **dispatcher** that routes by variant: `steer` (the mid-turn interrupt,
  `../compass-0.6/design.md:1466`) and `deliver` (turn-end-queued,
  `:1452-1466`) are applied immediately, WITHOUT waiting for the loop's next
  pull; the turn-starting ops (`prompt`, and the replay-barrier ops that precede
  any turn) flow through the sequential iterator. This deliberately breaks the
  "wire order == apply order" identity the single loop gives today, so C4 pins
  two invariants (below): the immediate path's replay-barrier disposition and
  the cross-path ordering, both with tests.
- **Replay-barrier semantics unchanged in intent — barrier disposition made
  explicit.** The Runner still holds live prompt/steer/ask-answer until
  `ReplayComplete` (`control.ts:10-12`), and today the agent ALSO guards locally
  (`agent.ts:47-51,145-153`: steer refused until `#replayComplete`, "a belt-
  and-suspenders on the frozen replay barrier"). The immediate dispatcher path
  routes `immediate.steer(msg)` straight to the SDK, which would bypass that
  local backstop for exactly the fast-pathed variants — leaving Runner-side
  holding (C3) as the only barrier. **C4 keeps the local barrier on the
  immediate path**: the dispatcher checks `#replayComplete` before applying a
  fast-pathed `steer`/`deliver` and surfaces an early one as the counted
  unmapped op, identical to the iterator path's refusal — so a Runner bug or the
  OQ-4 ack race cannot slip a steer to the SDK against partially-restored
  context. Stated, not assumed.

### What happens to the pipes

After C5, stdin and stdout carry **no protocol traffic**: `ProtojsonLineSink`
and the Runner's `relay`/scanner retire; stdin — which never had a protocol
writer (`podman.go:454-455`; the only stdin use anywhere in the runtime is the
one-shot credential script feed to a separate `sh -s` exec,
`go/internal/runtime/agent.go:264-270`, which is NOT the agent's
stdio and is untouched) — stays wired but idle. **stderr keeps its exact
current job**: `drainStderr` copies it to the diagnostic log line by line
(`relay.go:99-107`) and continues to — raw process logs are not protocol
traffic and gain nothing from the socket. stdout likewise remains drained
(a scanner-less `io.Copy` to the diagnostic log) so a stray print — a crashed
wrapper, a dependency writing to stdout — can never fill the OS pipe buffer
and stall the process (the invariant `relay.go:63-65` names for stderr now
covers stdout too). The `StreamingIO` triple (`podman.go:177-181`) is
unchanged: the pipes exist, the protocol just no longer lives on them.

**Teardown ordering (C5).** Today the terminal STOPPED/ERRORED frame rides
stdout and the pipe survives until process exit, so the relay always drains it
(`agent.ts:87-97`). Over the socket the frozen T2 `Close` runs
`http.Server.Shutdown` under a bounded deadline then `srv.Close()` force-closes
(`../compass-agent-runner-transport/design.md:381-385,514-517`) — and the agent
emits its terminal frame AFTER its control iterator ends, i.e. inside that
teardown window. The ordering C5 states and tests: control-stream close (Runner
signals session end) → agent emits terminal SessionFrame on `Publish` +
CloseAndSend → the terminal frame's `Publish` handler is an in-flight handler
that `Shutdown`'s bounded drain awaits → Runner `Close()`. The force-close step
exists only for a handler that overruns the deadline; the terminal-frame flush
is a fast in-flight send, so the drain covers it — but only because the ordering
is stated, so C5 asserts "terminal status frame observed at the fake Server
before socket close" in its red set.

## Alternatives considered

- **Keep the frozen split (status quo — stdio for telemetry/control, socket for
  calls).** This was the frozen record's own Decision #2 + OQ-8 posture, chosen
  when the socket did not yet exist and telemetry's pipe was live and adequate.
  Matt overrode it once the socket was frozen: two carriers mean two framings
  (newline-protojson AND Connect), two failure models, a hand-rolled scanner
  with a line cap (`go/internal/runner/relay.go:144-145`), and a
  control lane that would still need its own bespoke stdin decoder built from
  scratch (`proto/compass/v1/agent.proto:82-85`) — all to preserve a
  pipe whose only remaining protocol job the socket already does better. The
  override is cheapest NOW: the stdin decoder is unbuilt, so control never pays
  a migration at all.
- **Migrate ONLY control; keep telemetry on stdout (the inverse half-move).**
  The strongest competitor, worth naming: control is unbuilt (zero-cost carrier
  change, dissolves the RIG-1310 §2 mid-turn class), while telemetry is the one
  LIVE, working channel — and migrating telemetry is precisely what introduces
  the OQ-2 loss model, the `ReadMaxBytes` question, and the teardown-ordering
  hazard. Foreclosed by Matt's ruling ("get EVERYTHING onto the unix
  socket(s)"), not by cost — recorded so the freeze shows the fork was seen. The
  ruling accepts telemetry's new failure surface as the price of one carrier;
  OQ-2 is where that price is made explicit for Matt.
- **Telemetry as unary request/response (one RPC per frame).** OQ-8's anti-
  pattern reading (`../compass-agent-runner-transport/design.md:583-589`) and
  rightly rejected: per-frame unary pays a full request/response round-trip +
  header block per frame on a high-volume stream, loses cheap ordering
  (concurrent unaries can reorder), and forces the agent to await acks it does
  not need. The client-stream keeps the pipe's fire-and-forget shape with
  Connect's framing.
- **One bidi stream carrying both telemetry and control.** Superficially "one
  channel", but it couples two independent lifecycles: telemetry must flow from
  first boot frame (STARTING is emitted before any control arrives,
  `packages/compass-agent/src/agent.ts:80-82`) while control has
  the replay barrier's own ordering; a fault on one leg would tear down the
  other; and gRPC bidi halves are independent anyway — the coupling buys
  nothing but shared failure. Two RPCs on ONE socket and ONE service keep the
  consolidation (one carrier, one framing) without inventing a combined
  envelope neither side wants. Rejected.
- **A sibling internal service instead of new RPCs on `AgentGateway`.** The
  frozen brief allows either. Rejected: the service boundary would carry no
  meaning — same socket, same server, same internal gen lanes, same fence — and
  a second service name is one more gen-fence symbol and one more handler
  registration for zero isolation. `AgentGateway` is already "everything the
  agent speaks to the Runner"; these are two more verbs on it.

## Global Constraints

- **Egress seal preserved.** All three new RPCs (`Publish`, `Control`,
  `PostConversationFrame`) ride the existing per-container Unix socket — a local
  hop, no new port, no network path; the nft posture
  (`../compass-agent-container-runtime.md:206-217`) is neither relied on nor
  disturbed (frozen Decision #4,
  `../compass-agent-runner-transport/design.md:61-75`).
- **`WithReadMaxBytes` on the socket server.** Retiring the scanner also retires
  the 4 MiB line cap (`relay.go:145`), today the only per-message size bound on
  the agent→Runner hop. connect-go imposes no read limit unless
  `WithReadMaxBytes` is set, so a compromised agent could stream one arbitrarily
  large message the Runner buffers in memory. C2 sets an explicit
  `WithReadMaxBytes` (a small multiple of the retired 4 MiB, chosen once here)
  on the socket handler covering `Publish`, `PostConversationFrame`, and
  `Comms`; a message past it is a stream/unary error routed to OQ-2's reconnect
  path, not an OOM.
- **RIG-1267 gen-fence: extend the symbol list for every new internal name.**
  The fence is a fixed literal grep (`proto/moon.yml:123`:
  `AgentFrame|AgentControl|SessionFrame|RunnerService|RunnerError|compassv1internal`,
  with `AgentGateway|CommsCall` added by the frozen record's T1) with the
  maintenance instruction "Extend the symbol list as internal messages are
  added" (`moon.yml:118-119`). C1 here MUST add `PublishFrame`,
  `PostConversationFrame`, `ControlSubscribe`, `ReplayCompleteAck`, and
  `ControlAck` (covering `PublishFrameRequest`/`PublishFrameResponse`/
  `PostConversationFrameRequest`/`PostConversationFrameResponse`/
  `ControlSubscribeRequest` and the two ack messages); `AgentControl` and
  `AgentFrame` are already listed. Red→green proves the fence bites (a
  deliberate leak greps RED before `exclude_paths` lands), per the frozen T1's
  pattern (`../compass-agent-runner-transport/design.md:347-356`).
- **Additive + buf-breaking-safe.** New RPCs on the existing internal
  `AgentGateway` service, a first wire definition of `AgentControl` (with an
  envelope `control_seq` field), and two new variants on the frozen `AgentFrame`
  oneof (`ReplayCompleteAck`, `ControlAck`) — pure additions in the owned
  `compass.v1` package; `buf lint`/`buf breaking` glob `compass/**/*.proto` and
  cover them. No existing message/field changes (new oneof variants and a new
  field number are additive); the stdout relay retirement deletes Go/TS code,
  not proto surface (`AgentFrame` stays — it is the stream payload).
- **`AgentControl` payload fields stay RIG-1310's.** This record defines the
  oneof CARRIER message with the frozen variant names
  (`../compass-0.6/design.md:1439-1451`) but leaves the payload message fields
  exactly as parked (`proto/compass/v1/agent.proto:76-85`) — C1
  lands empty-shell payloads for the not-yet-representable variants; see OQ-1
  for the split and the sequencing constraint it puts on the Runner-side
  callers.
- **Runner-sequencing stays Runner-side.** `RunnerSeq` is assigned in the
  Runner's `Publish` handler, never by the agent — the
  `PublishEventsRequest.runner_seq` contract
  (`proto/compass/v1/runner.proto:63-70`) and the hub's gap
  detection (`go/internal/runnerhub/hub.go:125-126,178-188`) are
  invariant across the carrier swap.
- **Biome `@connectrpc` fence.** The frozen record's T4 already carves
  `packages/compass-agent/src/transport/**` and widens the ban to
  `@connectrpc/connect-node` (`../compass-agent-runner-transport/design.md:229-248`).
  The telemetry sink and control source impls here live under that SAME carved
  transport module — no second carve; a new impl file outside `src/transport/`
  that imports `@connectrpc` MUST redden `biome check`.
- **Red→green testing.** Every task lands BDD + unit tests first (watch them
  fail), then the smallest impl to green (`rule://red-green-testing`).
- **Format / lint gates.** Per-area via `direnv exec . moon run <project>:<task>`
  (biome, gofmt, golangci, buf lint/breaking, gen-fence, drift, markdownlint).
- **Frozen-record convention.** This record freezes on merge and supersedes the
  transport record's Decision #2 and OQ-8, plus the v0.6 §T5 stdio-carrier
  clauses, BY CITATION (see Approach); the frozen records themselves are never
  edited. It also consumes, unchanged: Decisions #1/#3/#4/#5, the socket
  lifecycle + stale recovery (its T2), and the `Comms` unary (its T1/T3).
- **Ordering with the frozen record's tasks.** This record's tasks build ON the
  frozen T1/T2/T4 (the proto file, the socket listener, the agent transport
  module). None of that has landed yet (the frozen record froze at design
  time); execution here stacks after — or alongside, same lane — those tasks,
  never duplicates them.

## Plan

Dependency-ordered. C1 defines the wire; C2 (Runner telemetry ingest) and C3
(Runner control producer) depend on C1 and on the frozen record's T2 (the
socket listener exists); C4 (agent-side sink/source) depends on C1 and the
frozen T4 (the carved transport module exists); C5 retires the stdio protocol
path and proves end-to-end, last.

### C1 — Wire: `Publish` + `Control` + durable `PostConversationFrame`, `AgentControl` carrier, ack variants, gen-fence

Extend the frozen internal service (`proto/compass/v1/agent_gateway.proto`,
frozen T1) with the two streaming RPCs AND the correlated durable-frame unary
OQ-2→(c) requires; give `AgentControl` its first wire definition (the CARRIER
oneof with the ratified variant names, `../compass-0.6/design.md:1439-1451`,
payload fields only for the variants already representable — OQ-1); and add the
two agent→Runner ack `AgentFrame` variants OQ-4→(i) and the amended OQ-6 need.

`Interfaces:` (internal proto, `compass.v1`, internal gen lanes only)

```proto
service AgentGateway {
  rpc Comms(CommsCallRequest) returns (CommsCallResult);  // frozen T1, unchanged
  // agent -> Runner, client-stream: the loss-tolerable telemetry spine. Every
  // TRACE and SESSION AgentFrame rides it in emission order. Replaces the
  // stdout relay's trace/session half.
  rpc Publish(stream PublishFrameRequest) returns (PublishFrameResponse);
  // agent -> Runner, UNARY, delivered-or-erred: the durable conversation-frame
  // path (OQ-2(c)). conversation_posted / conversation_updated ride THIS, not
  // Publish — request/response so an agent-side drop is an ERROR the agent
  // retries (with a stable idempotency_key, so a commit whose response was lost
  // is deduped on retry, never duplicated), never a silent gapless loss.
  rpc PostConversationFrame(PostConversationFrameRequest) returns (PostConversationFrameResponse);
  // agent-opened server-stream: the control lane. The Runner pushes one
  // AgentControl per message. Replaces the never-built stdin decoder.
  rpc Control(ControlSubscribeRequest) returns (stream AgentControl);
}

message PublishFrameRequest { AgentFrame frame = 1; }  // trace/session only; no seq: Runner-assigned upstream
message PublishFrameResponse {}                        // ack at stream close, mirrors PublishEventsResponse
// The durable-frame unary carries the SAME AgentFrame message, constrained by
// C4 to a conversation_posted/conversation_updated variant, plus an agent-minted
// idempotency_key (envelope field, C2 dedup — NOT a payload field, so RIG-1310's
// parked decision is untouched). Runner-sequenced upstream through the same
// ordered per-session publisher as Publish frames (C2), so hub gap-detection is
// identical; the difference is delivered-or-erred to the agent.
message PostConversationFrameRequest {
  AgentFrame frame = 1;
  // agent-minted, stable across retries of the same logical frame; the Runner
  // commits at-most-once per key so a lost-response retry is not duplicated.
  string idempotency_key = 2;
}
message PostConversationFrameResponse {}               // returned only after the upstream PublishEvents forward is accepted
message ControlSubscribeRequest {}                     // the socket IS the session identity (frozen Decision #4)

// First wire definition (agent.proto). Variant names/types are the ratified
// v0.6 oneof; payload fields land per-variant as representable (OQ-1). The
// envelope carries a Runner-assigned control_seq for retention/redelivery
// (amended OQ-6) — an ENVELOPE field, not a payload field, so it does not touch
// RIG-1310's parked payload decision.
message AgentControl {
  uint64 control_seq = 8;  // Runner-assigned, monotonic per session; the redelivery cursor (amended OQ-6)
  oneof control {
    PromptControl prompt = 1;
    SteerControl steer = 2;
    DeliverControl deliver = 3;
    AskAnswerControl ask_answer = 4;
    ConfigControl config = 5;
    TranscriptReplay replay = 6;
    ReplayComplete replay_complete = 7;
  }
}
message PromptControl { string input = 1; }
message AskAnswerControl { string ask_id = 1; repeated string chosen_option_ids = 2; }
message ReplayComplete {}
// SteerControl / DeliverControl / TranscriptReplay carry an inbound SDK
// AgentMessage; ConfigControl carries a tool set. Their FIELDS remain
// RIG-1310's parked decision (agent.proto:76-85) — defined here as empty
// shells so the oneof is complete on the wire, populated by RIG-1310's
// stacked PR (OQ-1).
message SteerControl {}
message DeliverControl {}
message TranscriptReplay {}
message ConfigControl {}

// Two agent -> Runner ACK variants ADDED to the frozen AgentFrame oneof
// (agent.proto), riding Publish beside the ratified DeliveryAck
// (../compass-0.6/design.md:1424-1426,1459 — the established frame-spine ack
// convention, OQ-4(i)). Additive to the existing oneof; buf-breaking-safe.
//   replay_complete_ack — the agent's replay-barrier ack (OQ-4(i)): the Runner
//                         releases held live ops on receipt.
//   control_ack         — the agent's selective apply-ack (amended OQ-6): a
//                         contiguous cursor (highest contiguously-APPLIED
//                         control_seq) PLUS a bounded set of seqs applied out of
//                         order above it. The Runner retires retained ops up to
//                         the cursor and drops the individually-acked ones.
message ReplayCompleteAck {}
message ControlAck { uint64 acked_seq = 1; repeated uint64 applied_above = 2; }
```

Generated surfaces (internal lanes): `compassv1internalconnect.AgentGatewayHandler`
gains `Publish` (client-stream), `PostConversationFrame` (unary), and `Control`
(server-stream) methods (Go, Runner side); the TS `AgentGateway` client gains the
matching client-stream / unary / server-stream methods (agent side).

`Red→green:` RED: extend the gen-fence grep (`proto/moon.yml:123`)
with `PublishFrame|PostConversationFrame|ControlSubscribe|ReplayCompleteAck|ControlAck`
BEFORE touching `exclude_paths`, regenerate — the leaked symbols grep RED in a
public tree (proves the fence bites, the frozen T1 pattern). `buf breaking`
proves the additions (new RPCs, new `AgentControl`, the two new `AgentFrame`
variants, the `control_seq` field) non-breaking. GREEN: `exclude_paths` keeps
the file out of `buf.gen.yaml`; internal lanes emit handler/client; gen-fence +
`buf lint`/`buf breaking` + drift pass.

### C2 — Runner telemetry ingest: `Publish` handler, durable `PostConversationFrame` handler, ack routing

Three Runner-side ingest paths, all resolving the session via the frozen T3
socket→container→session mapping (`../compass-agent-runner-transport/design.md:404-416`),
fail-closed before Start:

1. **`Publish`** (client-stream) forwards each received TRACE/SESSION frame up
   the existing `PublishEvents` client-stream, Runner-sequenced — the exact
   stamping `relay` does today (`relay.go:157-160`), minus the scanner and
   protojson decode.
2. **`PostConversationFrame`** (unary, OQ-2(c)) forwards the durable
   conversation frame up `PublishEvents` through the SAME ordered per-session
   publisher as `Publish` (one critical section allocates the Runner sequence
   AND emits, so allocation order == emission order across both concurrent paths
   and the hub never records a false gap), and returns success ONLY after the
   upstream send is accepted — delivered-or-erred to the agent. A forward
   failure is a unary error the agent retries with the same `idempotency_key`;
   the Runner commits at-most-once per key, so the frame is never silently lost
   nor duplicated by a committed-but-response-lost retry.
3. **Ack routing.** A `Publish` frame whose variant is `ReplayCompleteAck` or
   `ControlAck` is NOT relayed upstream — it is a control-plane ack: the handler
   routes `ReplayCompleteAck` to C3's replay-barrier release and `ControlAck` to
   C3's retention (retire retained ops ≤ `acked_seq`, and drop the individually
   `applied_above` seqs so an out-of-order-applied op is not redelivered).

`Interfaces:` (package `go/internal/runner/gateway`, extending the
frozen T2/T3 `Gateway`)

```go
// Publish forwards trace/session frames up PublishEvents Runner-sequenced;
// ReplayCompleteAck / ControlAck frames are routed to the control lane (C3),
// not relayed. Stream end == the old stdout EOF (close PublishEvents, await
// ack). Fails closed (CodePermissionDenied) when no session is bound. A
// malformed/over-limit message is a Connect stream error (WithReadMaxBytes),
// terminating this stream; the agent reconnects per OQ-2.
func (g *Gateway) Publish(
    ctx context.Context,
    stream *connect.ClientStream[compassv1internal.PublishFrameRequest],
) (*connect.Response[compassv1internal.PublishFrameResponse], error)

// PostConversationFrame forwards ONE durable conversation frame up PublishEvents
// through the ordered per-session publisher and returns only once the upstream
// accepts it (delivered-or-erred, OQ-2(c)). Dedups on idempotency_key
// (at-most-once commit) so a retried committed-but-unacked frame is not
// duplicated. Rejects a non-conversation variant with CodeInvalidArgument.
// Fails closed when no session is bound.
func (g *Gateway) PostConversationFrame(
    ctx context.Context,
    req *connect.Request[compassv1internal.PostConversationFrameRequest],
) (*connect.Response[compassv1internal.PostConversationFrameResponse], error)
```

Runner sequencing rides one ordered per-session publisher wrapping the
`eventPublisher.seq` `atomic.Uint64` discipline (`relay.go:120-125`): a single
per-session critical section allocates the sequence AND performs the matching
`PublishEvents` send for BOTH `Publish` and `PostConversationFrame`, so emission
order matches allocation order and concurrent traffic across the two paths
cannot open a false gap. At-most-once per `idempotency_key` is a durability
property of the COMMITTING component, not this handler: the durable frame and
its `idempotency_key` commit ATOMICALLY at the hub/comms `Message` store (a
unique constraint on `idempotency_key`, or a transactional outbox), so a crash
on either side of the upstream `PublishEvents` send neither duplicates (retry
hits the committed key) nor drops (unacked until committed) the frame. The
handler's in-process committed-key set is an advisory fast-path only — a Runner
crash that loses it is safe, since the atomic commit is the boundary; the
concrete store mechanism is C2/hub impl. The per-Runner hoist stays deferred
(`relay.go:113-119`).
The socket handler is constructed with an explicit `connect.WithReadMaxBytes` (Global Constraints)
covering `Publish`, `PostConversationFrame`, and `Comms`.

`Red→green:` RED: a `Publish` test streaming three trace frames and asserting
the fake Server's `PublishEvents` receives them in order with `RunnerSeq` 1,2,3
(mirrors `relay_test.go`); a `PostConversationFrame` test asserting a durable
frame is forwarded Runner-sequenced AND the unary returns success only after the
upstream ack — plus a failure test where the upstream send errs and the unary
returns that error (delivered-or-erred, OQ-2(c)); a concurrency test driving
`Publish` and `PostConversationFrame` traffic simultaneously and asserting the
fake Server sees a strictly monotonic gapless `RunnerSeq` (no false gap from
interleaved allocation/emit); an idempotency test posting a durable frame, then
re-posting the SAME `idempotency_key` after a simulated committed-but-lost
response, asserting the upstream sees ONE forward and both unaries return success
(at-most-once commit) — plus a crash-atomicity test asserting that a crash on
EITHER side of the commit (frame committed but response lost; frame not yet
committed) resolves to exactly-one durable frame on retry, since the key commits
atomically with the frame, not via the advisory in-process set; an ack-routing test asserting
a `ControlAck{acked_seq:N, applied_above:[M]}` frame on `Publish` retires C3's retained ops ≤ N,
drops retained op M, and is NOT relayed upstream, and a `ReplayCompleteAck` frame releases the C3 barrier;
a no-session test asserting `CodePermissionDenied`; a close test asserting stream
end closes upstream and awaits its ack; an over-limit test asserting a message
past `ReadMaxBytes` fails the stream. GREEN: the handlers; all pass. Non-goal:
retiring the stdout relay (C5 — both paths coexist until cutover).

### C3 — Runner control producer: `Control` stream, `ControlSender`, retention + redelivery

The `AgentGateway.Control` handler registers the agent's subscription and is the
Runner's one way to deliver control ops. The session lifecycle (replay,
replay-complete, prompt, steer, deliver, ask-answer, config) writes typed
`AgentControl` messages, each stamped a Runner-assigned monotonic `control_seq`.

- **Replay barrier (OQ-4(i)).** Replay frames first; live ops held until the
  agent's replay ack arrives. The ack is a `ReplayCompleteAck` `AgentFrame` on
  `Publish`, routed here by C2; on receipt C3 releases the held ops. The barrier
  contract is unchanged from v0.6 (`../compass-0.6/design.md:1445-1450`).
- **Empty-control rejection (OQ-1, P1 #5).** `ControlSender.Send` REJECTS an
  empty `steer`/`deliver`/`replay`/`config` variant — the must-not-send rule is
  enforced at the seam, not left to the agent to count as unmapped. The
  representable variants (`prompt`/`ask_answer`/`replay_complete`) pass.
- **Retention + redelivery (amended OQ-6, P1 #6).** The Runner RETAINS every op
  with `control_seq` past the agent's last `ControlAck` cursor, and DROPS any op
  the ack reports individually applied above that cursor. On TAKEOVER (a second
  `Control` subscription) the stale subscription is cancelled and the retained
  ops are TRANSFERRED to the replacement; on a reconnect after a drop the
  retained ops are REDELIVERED from the cursor. Because the Runner — whose
  retention already survives agent/container replacement — owns dedup, the agent
  needs no durable store of its own. `Send` success therefore means "durably
  queued until acked", closing the caller-already-got-success hazard.
  At-least-once across the pre-ack window: a redelivered op may repeat before its
  ack lands; the agent seq-dedups applied ops within a subscription and
  counts-and-drops duplicates (`agent.ts:169-181`).

`Interfaces:`

```go
// Control registers the agent's subscription and drains the per-session send
// queue to the stream, stamping control_seq. A second Control call is a
// TAKEOVER (OQ-6): the stale subscription is cancelled, the new one bound, and
// all ops past the ack cursor transferred/redelivered to it. Returns when the
// session ends or is taken over.
func (g *Gateway) Control(
    ctx context.Context,
    req *connect.Request[compassv1internal.ControlSubscribeRequest],
    stream *connect.ServerStream[compassv1internal.AgentControl],
) error

// ControlSender is the seam the session lifecycle uses to deliver a control op.
// Send stamps control_seq, retains the op for redelivery, and queues it; the
// stream goroutine drains. Rejects empty steer/deliver/replay/config
// (CodeInvalidArgument, P1 #5). ErrNoAgent when no subscription is live and the
// caller opts out of retention (caller decides: hold for replay, or fail).
type ControlSender interface {
    Send(sessionID string, op *compassv1internal.AgentControl) error
}

// C2 ack-routing entry points — control-plane acks arrive as AgentFrames on
// Publish, not on this stream:
//   AckControl(sessionID string, ackedSeq uint64)  // retire retained ops <= ackedSeq
//   ReleaseReplayBarrier(sessionID string)         // ReplayCompleteAck received
```

`Red→green:` RED: a test opening `Control`, `Send`-ing a prompt, asserting the
client stream yields it with a stamped `control_seq`; an ordering test (ops in
`Send` order); a per-variant rejection test asserting `Send` of an empty
`steer`/`deliver`/`replay`/`config` returns `CodeInvalidArgument` (P1 #5); a
takeover test asserting a second subscription cancels the first, receives
subsequent ops, AND is transferred the ops the first never acked (P1 #6); a
redelivery test asserting ops past the `ControlAck` cursor are re-sent on a new
subscription, ops ≤ the cursor are NOT, and an op named in `applied_above` is NOT
re-sent even though it sits past the cursor; a barrier test asserting held live
ops are released only after `ReleaseReplayBarrier` (P1 #3); a no-agent test
asserting `ErrNoAgent`. GREEN: handler + sender + retention; all pass. Non-goal:
the Runner-side callers that DECIDE what to send (RIG-1310 / RT-3 lanes), and the
`deliver` payload cursor those lanes reconcile with this `control_seq`.

### C4 — Agent-side: socket `FrameSink` (split by durability) + socket `ControlSource` + dispatcher

Two impls in the carved transport module (frozen T4's
`packages/compass-agent/src/transport/**`), each behind an existing seam so
`CompassAgent` itself does not change shape:

`Interfaces:` (extending `packages/compass-agent/src/transport/`)

```ts
// FrameSink split by durability (OQ-2(c), P1 #1). emit() classifies the
// OutboundFrame by kind (frame.ts:43-46): a "session" frame is enqueued onto
// the fire-and-forget Publish client-stream (matching frame.ts:50-52); a
// "conversationPosted"/"conversationUpdated" frame is sent on the
// PostConversationFrame UNARY, awaited and retried with bounded backoff until
// delivered-or-erred — never dropped on a reconnect. The sink retains each
// in-flight durable send behind a `drain()` the agent's teardown path awaits
// (bounded by the shutdown deadline), so shutdown cannot abandon an uncommitted
// conversation frame; per-frame emit() stays void so CompassAgent's shape is
// unchanged. The trace queue is bounded (below); the durable path applies
// backpressure via the awaited unary.
export function createSocketFrameSink(transport: RunnerTransport): FrameSink;

// ControlSource over AgentGateway.Control with the mid-turn dispatcher and a
// close-reason contract (OQ-6, P1 #4): a clean, Runner-initiated close ends the
// iterator (→ STOPPED); a transport drop triggers bounded reconnect (re-open
// Control; the Runner redelivers unacked ops per amended OQ-6), NOT a terminal
// stop. steer/deliver apply immediately on the event loop (through an immediate
// handle that STILL enforces the replay barrier); prompt/replay/replayComplete/
// config/askAnswer flow through the yielded iterable (agent.ts:84-86). The
// source emits a ReplayCompleteAck frame after replay-complete and reports a
// ControlAck — the contiguous applied cursor PLUS any seqs applied out of order
// above it (applied_above) — ONLY after an op is successfully APPLIED, never on
// mere receipt; the Runner dedups from this ack, so a crash between receipt and
// application leaves the op un-acked and redelivered (P1 #6, apply-then-ack).
export function createSocketControlSource(
    transport: RunnerTransport,
    immediate: { steer(msg: AgentMessage): void; deliver(msg: AgentMessage): void },
): ControlSource;
```

(`RunnerTransport` is the frozen T4 Connect-client handle over the socket;
decode of the staged empty-shell variants surfaces as the counted unmapped op,
the same posture as the staged `askAnswer` arm, `agent.ts:169-181`.)

**Trace queue cap + overload policy (OQ-2(c), P1 #2).** Under (c) the durable
class is off `Publish`, but the remaining trace/session queue is still
synchronous fire-and-forget (`frame.ts:50-52`) — a slow Runner + HTTP/2 flow
control means unbounded in-agent queueing and container OOM. C4 bounds the
`Publish` send buffer at a concrete named cap; on overflow it DROPS the oldest
trace frame and increments a counter surfaced as a session diagnostic (trace is
loss-tolerable by OQ-2(c) — durable frames are not on this path). The cap is a
named constant chosen once here. Lifecycle/status frames (notably the terminal
`STOPPED`) are never the drop target and are flushed ahead of any queued trace
backlog, so teardown delivers `STOPPED` to the socket before the trace queue —
the Server observes the terminal state within the shutdown deadline even when
the trace buffer is saturated.

**Two ordering invariants this dispatcher introduces, both pinned by tests:**

1. **The immediate path keeps the local replay barrier.** `immediate.steer`/
   `immediate.deliver` check `#replayComplete` before touching the SDK and
   surface an early op as the counted unmapped event — identical to the iterator
   path's refusal (`agent.ts:145-153`). The local backstop is NOT dropped for
   the fast-pathed variants; Runner-side holding (C3) plus this local guard both
   stand, so no single failure (a C3 bug, the OQ-4 ack race) reaches the SDK
   against partially-restored context.
2. **Immediate ops may precede earlier-sent iterator ops.** Because steer/
   deliver skip the pull loop, a Runner sending prompt-then-steer in wire order
   can have the steer applied while the prompt is still queued behind the
   iterator — a mid-turn interrupt that arrives before its turn. This is the
   intended semantics (steer IS the mid-turn interrupt), but it inverts today's
   wire-order==apply-order identity; C4 states it and tests it so it is a
   contract, not a surprise. Because this out-of-order application means the
   highest-contiguous `ControlAck` cursor cannot alone mark seq N done while N-1
   is unfinished, the agent additionally names N in the ack's `applied_above`
   set, and the Runner (the durable dedup owner) drops it from retention so a
   redelivered N is never re-applied (amended OQ-6).

`Red→green:` RED: a sink test asserting a "session" frame lands on the test
server's `Publish` stream in order; a durable-routing test asserting a
"conversationPosted" frame goes to the `PostConversationFrame` UNARY (NOT
Publish) and is RETRIED to success across an injected transient unary error
(P1 #1); a shutdown-drain test asserting teardown awaits an outstanding durable
frame's commit before the socket closes, so no conversation frame is abandoned
uncommitted (P1 #1); a queue-cap test asserting that with the Publish consumer
stalled, the trace buffer stops growing at the cap and overflow
drops-oldest-and-counts (P1 #2); a terminal-flush test asserting that with the
trace buffer saturated at teardown, `STOPPED` reaches the test server ahead of
the trace backlog within the shutdown deadline (P1 #2); a source test asserting
yielded ops match the pushed stream; **the
mid-turn test** (with the iterator consumer suspended awaiting a long `prompt`
turn, a pushed `steer` reaches `immediate.steer` before the turn resolves — the
RIG-1310 §2 latent bug pinned red, deadlocks-by-queueing over a naive
pass-through source); an ordering-inversion test asserting prompt-then-steer
applies steer first (invariant 2); a barrier test asserting a pre-`ReplayComplete`
steer on the immediate path is refused-and-counted, not applied (invariant 1); a
reconnect test asserting a transport drop re-opens `Control` (bounded retry) and
does NOT emit STOPPED, vs a clean close that DOES (P1 #4); an ack test asserting
a `ReplayCompleteAck` is emitted after replay and the `ControlAck` cursor
advances ONLY after an op is successfully APPLIED (not on receipt) — including a
crash test: an op received-but-not-yet-applied (queued behind a running turn) is
NOT acked, so a reconnect from the persisted cursor REDELIVERS it (P1 #6
durability, apply-then-ack); and an out-of-order-crash test: an immediate op at
seq N applied AHEAD of an unfinished N-1 leaves the contiguous cursor ≤N-2 but
names N in the ack's `applied_above`; the test RECREATES the container (so no
in-memory agent state survives) and asserts the Runner, having dropped N on that
ack, redelivers only N-1 on reconnect — N is not applied twice. GREEN: both impls; biome passes with the fence
still active outside `src/transport/`.

### C5 — Cutover: retire protocol stdio, E2E over the socket only

Switch the entrypoint to the socket sink/source; delete `ProtojsonLineSink`
usage from the boot path and the Runner's `eventPublisher.relay` + scanner
(`relay.go:138-172`); replace the stdout relay goroutine (`relay.go:66-67`)
with a plain drain-to-log (stderr keeps `drainStderr`, `relay.go:99-107`,
verbatim). `StreamingIO` and the exec spawn (`podman.go:438-455`) are untouched
— pipes exist, protocol-idle.

> **Amended — stderr unified under `drainToLog`, not kept verbatim (Matt, 2026-07-31).**
> As shipped (compass #16, RIG-1364 C5), stderr does **not** keep `drainStderr`
> verbatim. `drainStderr` was retired and generalized into a single
> `drainToLog` run on **both** pipes (in `go/internal/runner/agent_exec.go` —
> stderr and stdout drains), backed by a bounded `readBoundedLine` in the same
> file. This diverges from the frozen clause above ("stderr keeps
> `drainStderr` … verbatim") and from the C5 task line ("stderr drain
> unchanged"), and it landed without a design-fork ruling. It is a **strict
> improvement**: the old `drainStderr` used `bufio.Scanner` with
> `sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)` (`relay.go:145-146`,
> retired), so a stderr line over that configured 1 MB max returned
> `ErrTooLong`, which ended the scan and left the pipe unread forever — an OS
> pipe-buffer fill that stalls the agent's next stderr write. `drainToLog` /
> `readBoundedLine` truncates-and-continues instead (`errLineTruncated`, unlike
> `bufio.Scanner`'s `ErrTooLong`), so both pipes now share
> one drain that cannot stall on an overlong line. The verbatim-stderr clause is
> superseded by this note; the frozen text is left intact above as the record of
> what was originally ratified. (OQ-5 below governs the *stdout* drain, a
> separate question, and is unaffected.)

`Interfaces:` entrypoint wiring only — `CompassAgent` constructed with
`createSocketFrameSink(...)` (which itself dials both `Publish` and
`PostConversationFrame`) / `createSocketControlSource(...)`; `StartAgent`
(`relay.go:51-70`) drops `newEventPublisher`/`relay` and gains
`go drainToLog(xs.IO.Stdout, …)` beside the existing stderr drain.

`Red→green:` RED: an E2E driving a session over the socket only — boot →
STARTING telemetry frame arrives at the fake Server Runner-sequenced → a prompt
pushed down `Control` runs a turn → mid-turn steer lands (the C4 invariant, now
end-to-end) → a durable `conversation_posted` frame arrives via
`PostConversationFrame` Runner-sequenced on the SAME counter as the trace frames,
delivered-or-erred (OQ-2(c)) → session frames keep flowing → stop tears down
cleanly WITH the terminal STOPPED frame observed at the fake Server BEFORE socket
close (teardown ordering, see Approach). Plus: a durable-loss regression
asserting that with the durable unary erroring once, the frame is RETRIED and
still reaches the store (no silent gapless loss); a control-drop redelivery E2E
asserting a control op unacked when the `Control` stream drops is REDELIVERED
after reconnect and applied exactly once (amended OQ-6); and a stdout regression
asserting bytes written to the agent's raw stdout do NOT surface as frames (the
relay is really gone) but DO land in the diagnostic log (the drain is really
there). GREEN: full wiring; the board's `AgentSessionState` extraction and the
session-tail opaque bytes verified unchanged through the hub (`hub.go:129-144`
untouched — OQ-3's proof).

**Note on the wire-level mid-turn assertion.** C4/C5's "mid-turn steer lands
end-to-end" exercises the dispatcher MECHANISM (a `steer` op routed to
`immediate.steer` ahead of the turn). Carrying a real `AgentMessage` steer
payload over the wire needs RIG-1310's payload decision; until then the E2E
drives the immediate path with the empty-shell `SteerControl` and asserts the
dispatcher ROUTES it immediately (reaching `immediate.steer`, distinct from the
iterator pull), not that a populated message reaches the SDK. The populated-
payload end-to-end assertion is owed to RIG-1310's stacked PR (OQ-1). Stated so
the acceptance criterion is satisfiable against C1's empty shells.

## Tasks

- [ ] C1 — `Publish` + `Control` + durable `PostConversationFrame` on
  `AgentGateway`; first `AgentControl` carrier (7-variant oneof, empty-shell
  payloads, envelope `control_seq`); `ReplayCompleteAck`/`ControlAck` AgentFrame
  variants; gen-fence extended
  (`PublishFrame|PostConversationFrame|ControlSubscribe|ReplayCompleteAck|ControlAck`),
  buf lint/breaking/drift green.
- [ ] C2 — Runner `Publish` handler (trace/session, Runner-sequenced),
  `PostConversationFrame` handler (durable, delivered-or-erred, ordered
  per-session publisher + `idempotency_key` committed ATOMICALLY with the frame
  at the committing component, advisory in-process fast-path only), ack routing
  (ReplayCompleteAck→barrier, ControlAck→retention cursor); `WithReadMaxBytes`
  with over-limit + upstream-error + cross-path ordering + idempotent-retry +
  crash-atomicity tests.
- [ ] C3 — Runner `Control` handler + `ControlSender`: ordered push with
  `control_seq`, empty-control rejection (P1 #5), subscription takeover with
  op-transfer + cursor redelivery (amended OQ-6, P1 #6), replay-barrier release
  on `ReplayCompleteAck` (P1 #3).
- [ ] C4 — Agent socket `FrameSink` split by durability (durable→unary retried,
  trace/session→Publish, P1 #1), bounded trace queue + drop-oldest overload
  policy (P1 #2), `ControlSource` dispatcher with close-reason + bounded retry
  (P1 #4) and ack emission over a PERSISTED applied-sequence set (crash-safe
  seq-dedup across agent replacement); mid-turn/ordering-inversion/barrier tests green;
  biome fence intact.
- [ ] C5 — Cutover: stdout relay + `ProtojsonLineSink` retired, stdout
  drained-to-log, stderr drain unchanged; socket-only E2E green with durable
  delivered-or-erred, control-drop redelivery, and teardown-ordering assertions;
  hub/board paths verified unchanged.

## Open Questions

Batched for Matt; each carried this record's recommendation. **Matt ruled on
2026-07-22: OQ-2 resolved (c) — route durable conversation frames off the lossy
`Publish` spine onto the correlated call path; every other recommendation
accepted (LGTM).** Folded below as the frozen decisions this record merges on.

- **OQ-1 (LOAD-BEARING; RESOLVED — Matt, 2026-07-22) — Does the parked `AgentMessage` payload decision
  (RIG-1310) block the control-lane migration?** The stdin decoder was parked
  because control ops carry an inbound SDK `AgentMessage` + a tool set no
  compass.v1 message represents (`proto/compass/v1/agent.proto:76-85`).
  Moving the CARRIER to a Connect stream does not resolve that — but it does
  not need to: the oneof variant names/types are ratified, and
  `prompt`/`askAnswer`/`replayComplete` payloads are already representable
  (string / id+options / empty). *Recommendation:* control rides the socket
  NOW with `SteerControl`/`DeliverControl`/`TranscriptReplay`/`ConfigControl`
  landed as empty-shell messages (C1) — additive field population is
  buf-breaking-safe when RIG-1310 rules — so consolidation resolves framing,
  correlation, and the mid-turn DISPATCH MECHANISM today, while the payload
  decision stays exactly as parked. **Sequencing constraint this creates:** an
  empty-shell `steer`/`deliver`/`replay`/`config` op is TRANSMITTABLE on the
  wire before its payload exists, so C3's Runner-side callers MUST NOT send
  those variants until RIG-1310 populates them (the agent can only count them as
  unmapped) — i.e. the RT-3/RIG-1310 lanes own the "start sending real payloads"
  switch, and this record lands only the carrier + the mechanism test. The
  claim is deliberately "resolves the mid-turn DISPATCH class," not "resolves
  mid-turn steer end-to-end" — the latter waits on the payload. The alternative
  (hold C3/C4-control until RIG-1310 rules) re-couples two decisions the frozen
  record already decoupled.
  **Resolved — ratified (Matt, 2026-07-22).** Control rides the socket now with
  empty-shell control messages (C1); C3's Runner-side callers must not send the
  `steer`/`deliver`/`replay`/`config` variants until RIG-1310 populates them.
- **OQ-2 (LOAD-BEARING, security-relevant; RESOLVED — Matt, 2026-07-22) — Telemetry stream reconnect + the
  new loss model.** Today a broken relay ends telemetry for the session
  (`relay.go:162-166`: send failure logs + breaks; stdout EOF ends the loop) and
  the pipe CANNOT drop a frame — it delays under backpressure, and its only
  mid-session failure is that loud, session-ending break. A socket stream CAN
  drop while the agent lives (Runner restart, listener hiccup), and here is the
  sharp edge the pipe never had: **`RunnerSeq` is minted at the Runner on
  receipt** (`relay.go:157-160`; `runner.proto:63-70`), and the gap detector
  fires only on `seq > lastSeq+1` (`hub.go:178-188`). A frame the agent drops
  BEFORE reconnecting is never received, so it is never sequenced — the Runner's
  counter simply does not advance, the hub sees a contiguous sequence, and
  `SeenGap` stays false. **Agent-side drops are therefore SILENT and gapless —
  a strictly new loss class**, not "a gap exactly like today." Worse, the spine
  is not uniformly loss-tolerable: it carries `conversation_posted`/
  `conversation_updated` frames that commit to durable comms `Message` rows
  (`hub.go:129-133` → `conversation.PostAgentMessage`) and `SessionFrame.state`
  lifecycle transitions the board depends on (`hub.go:134-136,165-172`). A
  dropped conversation frame is a permanently lost durable message the client-
  bus resync CANNOT recover — resync replays the STORE, and a frame that never
  reached the store was never written (`hub.go:146-148` covers only in-transit
  loss AFTER Runner sequencing). *The real fork for Matt, by durability class:*
  - **(a) Accept silent gapless loss, honestly scoped.** The agent retries
    `Publish` with bounded backoff; frames emitted while disconnected are
    buffered to a small cap then dropped-and-counted. Simplest; correct ONLY if
    every frame class tolerates silent loss. It does not — durable conversation
    frames do not — so (a) alone is not defensible for the whole spine.
  - **(b) Buffered replay for the durable class.** The agent holds unacked
    durable frames across a reconnect and resends them; the Runner de-dups on a
    frame id. Recovers the durable class at the cost of agent-side buffering +
    an ack/dedup protocol (machinery the current contract does not promise).
  - **(c) Route durable conversation frames OFF the lossy spine** onto the
    correlated call path (a `Comms`-style unary the socket already provides,
    request/response = delivered-or-erred), leaving the opaque trace on the
    fire-and-forget `Publish` stream. Splits the spine by durability, matching
    each class to the semantics it needs; today's single spine conflates them
    only because the pipe could not drop. *This is my recommendation* — it
    resolves the finding without a bespoke replay protocol and puts the
    guarantee where the durability requirement already is.
  Whichever Matt picks, `RunnerSeq` stays Runner-side and monotonic across
  reconnects, so in-transit loss (after the Runner) is still gap-detected as
  today. **Matt: rule (a)/(b)/(c) — this decides whether durable messages can be
  silently lost on a Runner restart.**
  **Resolved — (c) (Matt, 2026-07-22).** Durable conversation frames route OFF
  the lossy `Publish` spine onto the correlated call path (delivered-or-erred);
  the opaque session trace stays on fire-and-forget `Publish`. No bespoke replay
  protocol — the delivery guarantee sits where the durability requirement is.
- **OQ-3 (RESOLVED — Matt, 2026-07-22) — Does retiring stdout disturb the `SessionFrame` opaque-bytes path
  or the board's `AgentSessionState` extraction?** No, by construction: the
  carrier swap moves WHERE `AgentFrame` bytes travel agent→Runner; the
  message, the `PublishEvents` leg, and the hub's classification
  (`hub.go:129-144`) + state extraction are byte-identical above the Runner.
  One caveat worth the check: protojson vs Connect binary encoding of
  `SessionFrame.event` bytes — protobuf `bytes` is base64 in protojson and
  raw on the binary wire; both decode to identical bytes, but C5's E2E
  asserts the tail-stream bytes verbatim to pin it. *Recommendation:* treat
  as verification (C5), not a fork. Deferrable.
  **Resolved — treat as C5 verification, not a fork (Matt, 2026-07-22).**
- **OQ-4 (RESOLVED — Matt, 2026-07-22) — `ReplayComplete` ack path, and its tension with the ratified
  `DeliveryAck`.** v0.6 has the agent ACK replay completion so the Runner
  releases held live ops (`../compass-0.6/design.md:1445-1450`). With stdio
  retired the ack needs a carrier: a telemetry frame variant, or a tiny
  `AgentGateway` unary. **The tension to surface:** v0.6 ALREADY ratified
  `DeliveryAck` riding `AgentFrame` as a oneof variant (`:1424-1426,1459` — the
  RT-3 delivery ack on the telemetry spine). So "the ack should not re-mix
  planes" is not a clean argument — one ack already does. Two coherent
  positions: (i) acks-on-the-frame-spine is the ESTABLISHED convention →
  `ReplayComplete`-ack should ALSO be an `AgentFrame` variant, matching
  `DeliveryAck`, and OQ-4 should follow it; or (ii) plane separation is right
  and BOTH should be unaries → a further supersession of the `DeliveryAck`
  carrier this record would then own. *Recommendation:* (i) — follow the
  ratified convention (`ReplayComplete` ack as an `AgentFrame` variant beside
  `DeliveryAck`); it is one carrier, one precedent, and avoids a second
  supersession. Note this ack RACES the immediate dispatcher (the Runner
  releases held steers on ack receipt); C4's invariant-1 local barrier is the
  backstop if the release is early. **Matt: (i) frame-variant vs (ii) unary +
  move `DeliveryAck` too.**
  **Resolved — (i) (Matt, 2026-07-22).** The `ReplayComplete` ack rides
  `AgentFrame` as a oneof variant beside `DeliveryAck`, following the ratified
  convention; C4's invariant-1 local barrier backstops an early release.
- **OQ-5 (RESOLVED — Matt, 2026-07-22) — stdout drain vs close.** C5 keeps stdout drained-to-log so stray
  prints cannot stall the process. Alternative: point the exec's stdout at
  the log directly and drop the pipe read. *Recommendation:* keep the drain
  (smallest diff to `StreamingIO`, preserves diagnostics). Non-load-bearing;
  recorded so the leftover pipe is deliberate.
  **Resolved — keep the drain (Matt, 2026-07-22).** Non-load-bearing.
- **OQ-6 (LOAD-BEARING; RESOLVED — Matt, 2026-07-22; AMENDED — Matt, 2026-07-23) — Control-stream drop/reconnect + stale-subscription
  takeover.** OQ-2 covers telemetry; the Control server-stream can equally drop
  while the agent lives (same Runner restart / listener hiccup), and the failure
  is LESS tolerable: a prompt/steer/ask-answer sent while no subscription is
  live gets `ErrNoAgent` — lost CONTROL, not lossy telemetry — and only
  `deliver` has redelivery machinery (the RT-3 delivery cursor,
  `../compass-0.6/design.md:1452-1466`); prompt/steer/askAnswer have none.
  Compounding it: a naive "one live subscription, second is `CodeAlreadyExists`"
  rule interacts badly with reconnect — after a transient drop the Runner may
  not yet have observed the dead stream, so the agent's reconnect bounces
  `AlreadyExists` until the stale one is reaped. And on the agent side, today
  iterator-end means STOPPED (`agent.ts:87-89`); over a socket a transient drop
  that ends the iterator would terminate the session as a "clean" stop.
  *Recommendation:* (1) the agent retries `Control` with bounded backoff on a
  non-clean stream end; (2) the Runner treats a NEW `Control` subscription as a
  TAKEOVER — cancel the stale one, bind the new (C3's interface states this);
  (3) iterator-end is terminal (→ STOPPED) ONLY on a Runner-initiated clean
  close, distinguished from a transport drop (which triggers the retry, not a
  terminal status). Redelivery of missed prompt/steer/askAnswer across a control
  drop is RIG-1310/RT-3's cursor problem, not this record's — noted so the
  boundary is explicit. **Matt: confirm takeover + retry + clean-close-only-
  terminal, or flag if control-op redelivery must be designed here.**
  **Resolved — takeover + retry + clean-close-only-terminal (Matt, 2026-07-22).**
  (1) the agent retries `Control` with bounded backoff on a non-clean stream
  end; (2) the Runner treats a new `Control` subscription as a takeover (cancel
  the stale one, bind the new); (3) iterator-end is terminal (→ STOPPED) only on
  a Runner-initiated clean close.
  **Amended — control-op redelivery designed IN THIS RECORD (Matt, 2026-07-23).**
  The 2026-07-22 resolution's out-of-scope clause ("missed prompt/steer/
  askAnswer redelivery is RIG-1310/RT-3's cursor problem") is superseded: the
  control lane is now lossless end-to-end. Each `AgentControl` carries a
  Runner-assigned monotonic `control_seq` (envelope field, C1 — NOT a payload
  field, so RIG-1310's parked payload decision is untouched); the Runner RETAINS
  every op past the agent's `ControlAck` cursor and drops any op the ack reports
  individually applied (below); it TRANSFERS retained ops to the replacement
  subscription on takeover (C3) and REDELIVERS from the cursor on reconnect after
  a drop. The agent advances its contiguous `ControlAck` cursor (highest
  contiguous `control_seq`) ONLY after an op is successfully APPLIED — never on
  mere receipt, so a crash between receipt and application leaves the op unacked
  and it is redelivered from the cursor (apply-then-ack) — and seq-dedups
  redelivered ops (at-least-once; duplicates counted-and-dropped,
  `agent.ts:169-181`). Because an immediate op at seq N may be APPLIED ahead of
  an unfinished earlier op at N-1 (C4 invariant 2), the highest-contiguous cursor
  alone cannot record that N is done; the agent's `ControlAck` therefore also
  carries a bounded `applied_above` set naming ops applied out of order above the
  cursor. The Runner — whose retention already survives agent and container
  replacement — owns dedup: it drops an `applied_above` op and never redelivers
  it, so the agent needs NO durable store (the MVP container is stateless; the
  per-agent volume is deferred, `compass-agent-container-runtime.md:563-570`). A
  crash after N-applied-but-before-N-1, once N's ack has landed, redelivers only
  N-1 — the already-applied N is not re-sent — so steer/deliver never runs twice. This
  makes `ControlSender.Send` success mean "durably queued until acked," closing
  the caller-already-got-success hazard (P1 #6). The RT-3 `deliver` cursor
  (`../compass-0.6/design.md:1452-1466`) is reconciled with `control_seq` by the
  RIG-1310/RT-3 lanes; this record owns the generic control-op retention
  mechanism, they own the `deliver`-specific payload semantics layered on it.
