// CompassAgent — the first-party in-container agent (design §T5).
//
// Drives an `AgentSession` from @oh-my-pi/pi-coding-agent: subscribes its session
// event stream, maps each event to a compass.v1 `AgentFrame` (via EventMapper) and
// writes it to stdout through the FrameSink; consumes decoded `AgentControl` ops
// from the ControlSource and drives the session (`prompt`/`steer`/`setTools`/
// `setSystemPrompt`), gated by the replay barrier. One container = one agent = one
// session. The Runner starts this over ExecStreaming and relays its stdout up
// PublishEvents.
//
// Why the session, not the bare `Agent`: the typed session renderer (design: architecture-lineage)
// needs the `AgentSessionEvent` superset — `todo_reminder`/`todo_auto_clear`/
// `notice` and the compaction/retry orchestration events that the session emits
// but the core `Agent` event stream (`AgentEvent`, 10 cases) does not. Only
// `AgentSession.subscribe` yields that union, so the emitter subscribes at the
// session level.
//
// Why control drives `session.agent`, not the session: the control contract
// (prompt/steer/config/replay) is frozen at architecture-lineage and its wire payload
// representation is a parked follow-up (control.ts). Driving the inner `Agent`
// directly preserves that frozen contract byte-for-byte — the session-level
// prompt/steer add streaming-queue mediation the Runner already owns (it holds
// live input behind the replay barrier), and session-level steer takes plain text
// while the frozen `SteerControl` carries an `AgentMessage`. So this change swaps
// only the ruled event-stream source; the control path is unchanged.
//
// The wire envelopes (`AgentFrame` out, `AgentControl` in) are isolated behind
// FrameSink / ControlSource: this class produces/consumes typed domain values and
// never touches wire bytes, so the pending gen of those internal proto messages
// changes only the sink/source impls, not this class.

import type { AgentMessage, AgentTool } from "@oh-my-pi/pi-agent-core";
import type { AgentSession } from "@oh-my-pi/pi-coding-agent";
import {
	AgentSessionState,
	type Ask,
	create,
	type DeliveryAck,
	DeliveryAckSchema,
	type ForgeNotification,
	type ForgeNotificationAck,
	ForgeNotificationAckSchema,
	ForgeNotificationKind,
	type Message,
	type SessionFrame,
	SessionFrameSchema,
	SessionInjectionKind,
} from "./compassv1";
import type { AgentControl, ControlSource } from "./control";
import type { FrameSink } from "./frame";
import { EventMapper, type UnmappedEvent } from "./mapping";
import { flat } from "./render-guard";
import type { TurnTracer } from "./trace-bridge";

export interface CompassAgentOptions {
	// The session to drive. Constructed by the caller (container entrypoint) via
	// `createAgentSession` with its model/tools/system-prompt, so this class stays
	// IO-focused and the session's configuration is a separate concern. Required:
	// there is no no-arg `AgentSession` constructor (it needs an
	// `AgentSessionConfig`), so unlike the former bare-`Agent` default the caller
	// always supplies it.
	readonly session: AgentSession;
	// Outbound frame sink (stdout). The wire envelope lives behind it.
	readonly sink: FrameSink;
	// Inbound control source (stdin). Yields decoded AgentControl frames.
	readonly control: ControlSource;
	// Sink for frames the mapper could not map — logged + counted, never
	// dropped. Defaults to a no-op counter-less logger via console.
	readonly onUnmapped?: (u: UnmappedEvent) => void;
	// Optional trace-continuity tracer (design record:
	// docs/designs/platform/compass-agent-message-trace-continuity/design.md §T2).
	// The NARROW, OTel-type-free `TurnTracer` facet only, so this exported option
	// stays fence-clean. `undefined` (telemetry off) ⇒ every trace call below
	// no-ops via optional chaining, so frames stay bit-identical to today.
	readonly tracer?: TurnTracer;
}

export class CompassAgent {
	readonly #session: AgentSession;
	readonly #sink: FrameSink;
	readonly #control: ControlSource;
	readonly #mapper: EventMapper;
	readonly #onUnmapped: (u: UnmappedEvent) => void;
	readonly #tracer: TurnTracer | undefined;
	// The tools the session was constructed with (container entrypoint) — the
	// native set no control frame may drop or substitute (see #withNatives).
	// Snapshotted as a COPY: `agent.state` hands back the live, caller-owned
	// `state.tools` array by reference, so an in-place mutation of that array
	// after construction (a push, a truncation) would otherwise alter the native
	// set out from under us — reintroducing the very revocation this field exists
	// to prevent.
	readonly #natives: AgentTool[];
	// Runner holds live prompt/steer until the agent acks ReplayComplete, but the
	// agent also guards locally: control frames that arrive before replay settles
	// are applied as replay (context), and live prompt/steer are refused until
	// `#replayComplete` — a belt-and-suspenders on the frozen replay barrier.
	#replayComplete = false;
	// SEA-1310 §8 — RT-3 turn-end delivery (DELIVER arm). A delivered channel
	// message is coalesced to a turn-end prompt: mid-turn delivers queue and flush
	// as ONE prompt when the turn settles; an idle deliver starts a turn at once.
	//
	// `#turnActive` — true between a turn-start edge (agent_start/turn_start/
	// message_start) and `agent_end`, tracked off the same session event stream
	// the mapper reads. The flush trigger is the `agent_end` edge: it is safe to
	// call `session.agent.prompt` synchronously there because the inner Agent
	// clears `isStreaming` in the `agent_end` case of its own loop BEFORE emitting
	// (pi-agent-core agent.ts:1254) — so by the time this listener sees the edge
	// the inner `prompt` guard (agent.ts:985, reads `#state.isStreaming`) is
	// already open, no AgentBusyError. (The AgentSession-level deferral of
	// `agent_end` until in-flight prompts unwind, agent-session.ts:3799-3802, does
	// NOT bear on this path: it is gated on `#promptInFlightCount`, which only
	// session-level prompts bump — CompassAgent drives the inner `session.agent`
	// directly, so that count stays 0 for these flushes.) No deferral, no timer:
	// the flush is synchronous and deterministic under test. The idle-deliver
	// trigger additionally gates on `#session.isStreaming` (see `deliver`), which
	// closes the control-prompt spin-up race the event-derived flag alone cannot.
	#turnActive = false;
	// The coalescing queue: messages delivered mid-turn, drained into one prompt
	// at the next flush.
	#deliverQueue: Message[] = [];
	// RIG-2486 T1 — the denormalized author `from_handle` per queued deliver,
	// keyed on `Message.id`. A deliver coalesces into `#deliverQueue` and is
	// injected later at the turn-end flush, so its from_handle (carried on the
	// wire deliver control, resolved server-side) must be stashed here to travel
	// with the message to the `#emitInjection` at flush time. Populated at enqueue
	// beside `#deliverQueue.push`, read + deleted per message at flush.
	readonly #deliverFromHandles = new Map<string, string>();
	// Trace-continuity (design record §T2): the W3C `traceparent` string per
	// queued deliver, keyed on `Message.id`, EXACTLY mirroring `#deliverFromHandles`.
	// A deliver coalesces into `#deliverQueue` and is injected later at the
	// turn-end flush, so its traceparent (carried on the wire deliver control,
	// stamped server-side) must be stashed here to travel with the message to the
	// flush, where it becomes the turn span's parent (N=1) or a link (N>1).
	// Populated at enqueue beside `#deliverQueue.push`, read + deleted per message
	// at flush.
	readonly #deliverTraceparents = new Map<string, string>();
	// Trace-continuity (design record §T2): the ids of every channel message that
	// has fed the CURRENT turn span, in arrival order — the source for the
	// `compass.message.ids` stamp. T1's `stampActiveTurn` OVERWRITES the
	// attribute, so the topology-independent query key (design.md:199-200: "which
	// messages fed this turn" must NOT depend on parent-vs-link topology) is only
	// complete if every stamp passes the FULL accumulated list, not the delta.
	// Reset at each turn-START site (idle steer, deliver/forge flush); appended at
	// the mid-turn-steer stamp site. Reset at turn-start (not `agent_end`) keeps it
	// self-contained — a rejected turn-start fires no `agent_end`.
	readonly #turnMessageIds: string[] = [];
	// Session-lifetime dedup set keyed on `Message.id`. A sweep redelivery under a
	// fresh control_seq (independent of the control-source's seq dedup) is dropped
	// here so a message is injected at most once (frozen record :811-812).
	readonly #processedMessageIds = new Set<string>();
	// RIG-2732 W3 — the turn-end forge-notification queue, the RT-3 sibling of
	// `#deliverQueue`: forge changes pushed mid-turn coalesce here and flush as
	// ONE turn-end prompt (rendered per-kind), an idle notification flushes at
	// once. Each entry pairs the decoded notification with the control-source's
	// `ackRail` thunk — the deferred rail ack the flush calls AFTER emitting the
	// ForgeNotificationAck frame, retiring the op on the control rail (the forge
	// arm defers BOTH acks to flush, unlike steer/deliver which ack at decode;
	// design.md:1006-1013). Dedup of a redelivery within the decode->flush window
	// rides the control-source's SEQ-based `queued`/`acks.isApplied` path (no
	// content key); this queue holds only ops past that dedup.
	#forgeQueue: { notification: ForgeNotification; ackRail: () => void }[] = [];
	// RIG-2644 — strand-recovery latch. The idle-deliver gate (see `deliver`)
	// flushes immediately only when the session is idle; when it queues a message
	// because `#session.isStreaming` is true but no TRACKED turn is active
	// (`#turnActive` false), no `agent_end` edge is guaranteed to arrive to flush
	// it — a startup provider probe/prewarm (or any untracked in-flight that folds
	// into `AgentSession.isStreaming` via `#promptInFlightCount`,
	// agent-session.ts:6470) holds `isStreaming` true with no turn edge on the
	// subscription. Left alone the message strands forever: control-acked but never
	// injected, never `DeliveryAck`ed, board stuck STARTING. This latch arms a
	// single `waitForIdle`-gated re-check that flushes once the untracked stream
	// settles (surfaced investigating RIG-2617 Defect 2; the spin-up-race gate
	// intact — a real turn's `agent_end` flushes first and the recovery no-ops on
	// the now-empty queue). One recovery in flight at a time; re-armed if a fresh
	// probe is still streaming when the wait resolves.
	#strandRecoveryArmed = false;
	// RIG-2644 — set true in run()'s finally, on the same edge that emits the
	// terminal status and unsubscribes. A strand-recovery `waitForIdle` can
	// resolve AFTER the session has terminated (STOPPED/ERRORED); flushing then
	// would start a turn and emit a DeliveryAck past the terminal frame the board
	// already saw. The recovery re-check consults this and no-ops when closed.
	#closed = false;

	constructor(opts: CompassAgentOptions) {
		this.#session = opts.session;
		// Tolerate a session that exposes no `state`/`state.tools`: this is a
		// read for a safety net, and a constructor that throws on a shape it
		// merely inspects is worse than one that degrades to no natives.
		this.#natives = [...(opts.session.agent?.state?.tools ?? [])];
		this.#sink = opts.sink;
		this.#control = opts.control;
		this.#tracer = opts.tracer;
		this.#mapper = new EventMapper();
		this.#onUnmapped =
			opts.onUnmapped ??
			((u) =>
				console.error(
					`[compass-agent] unmapped: ${u.eventType} — ${u.reason}`,
				));
	}

	// Wire the session event stream to the frame sink, then consume control frames
	// until stdin closes. Emits a terminal status — STOPPED on a clean
	// control-stream close, ERRORED on an exception — then resolves (clean) or
	// re-throws (error).
	async run(): Promise<void> {
		const unsubscribe = this.#session.subscribe((event) => {
			// Turn-tracking (SEA-1310 §8): an ADDITIONAL read of the same event,
			// beside the mapper fan-out below — never disturbing it. A turn-start
			// edge marks the session active; `agent_end` settles it and flushes any
			// coalesced delivers into one turn-end prompt.
			this.#trackTurn(event.type);
			for (const out of this.#mapper.map(event)) {
				if (out.kind === "unmapped") {
					this.#onUnmapped(out);
				} else {
					this.#sink.emit(out);
				}
			}
		});
		// Announce STARTING immediately so the board shows the session coming up
		// before the first session event.
		this.#emitStatus(AgentSessionState.STARTING);
		try {
			for await (const control of this.#control) {
				await this.#applyControl(control);
			}
			// Clean control-stream close → STOPPED (the normal terminal state).
			this.#emitStatus(AgentSessionState.STOPPED);
		} catch (err) {
			// An unexpected exit (control decode / SDK op / stream error) →
			// ERRORED, distinct from a clean STOPPED (compass.proto:141). Emit it
			// as the terminal status, then re-propagate — a swallowed crash would
			// be a silent failure.
			this.#emitStatus(AgentSessionState.ERRORED);
			throw err;
		} finally {
			// Terminal edge: no strand-recovery re-check may start a turn past here.
			this.#closed = true;
			unsubscribe();
		}
	}

	// Emit a board lifecycle transition as a session frame carrying only the
	// state (SessionFrame.typed_event empty — no trace event). The former typed
	// AgentSessionStatus frame is gone under the spine-inversion; the Runner
	// extracts the state into an AgentSessionStatus server-side, stamping the
	// session_id it owns.
	#emitStatus(state: AgentSessionState): void {
		const value: SessionFrame = create(SessionFrameSchema, { state });
		this.#sink.emit({ kind: "session", value });
	}

	// Emit the SessionInjection observation (steer/deliver split-observation seam,
	// T1): a first-class trace frame recording that a channel message was injected
	// into the live session as a steer or a deliver. Emitted BESIDE the delivery
	// ack at injection time, on the SAME FrameSink path (idle-safe: the emit fires
	// at control decode on the event loop, not turn-scoped, so an idle leg-4 peer
	// that drives no turn is still observed). The mapper stamps event_id/at_unix_ms
	// so this frame is ordered on the one monotonic trace sequence; the sink pins
	// it off the drop-oldest trace lane onto the never-drop priority lane (F3), so
	// a busy trace stream cannot silently drop the observation. `from_handle` is the
	// steering/delivering author's handle, denormalized onto the wire steer/deliver
	// control server-side (RIG-2486 T1) — the comms Message carries only
	// author_account_id, so the Server resolves the handle once when wrapping the
	// AgentControl and the agent reads it straight off the control here. Empty only
	// when the Server could not resolve the author handle (a logged store miss).
	#emitInjection(
		opKind: SessionInjectionKind,
		messageId: string,
		fromHandle: string,
	): void {
		this.#sink.emit(
			this.#mapper.sessionInjection(opKind, messageId, fromHandle),
		);
	}

	// SEA-1310 §8 — deliver a channel message into the live session (RT-3). The
	// entry the immediate handle calls when a `DeliverControl.message` decodes.
	// The replay barrier is enforced UPSTREAM at the control source (a
	// pre-ReplayComplete immediate op is refused-and-counted before it reaches
	// this handle, control-source.ts), so this method does not re-check it.
	deliver(msg: Message, fromHandle = "", traceparent = ""): void {
		// A message with no id cannot be acked or deduped — fail-visible, never a
		// silent drop (and never injected, since there would be no receipt for it).
		if (msg.id === "") {
			this.#onUnmapped({
				kind: "unmapped",
				eventType: "deliver",
				reason: "deliver missing Message.id — cannot ack or dedup",
			});
			return;
		}
		// Sweep-redelivery safety: a message already processed this session is
		// dropped (counted), independent of the control-source's control_seq dedup
		// (frozen record :811-812).
		//
		// Re-ack subtlety (SEA-1310 §8 review MEDIUM): `#processedMessageIds` holds
		// ids from ENQUEUE time (:205, before injection), not injection time. The
		// Publish PRIORITY lane an ack rides is never-drop only within its retry
		// budget (publish-spine.ts:156-158) — a Runner restart or a >~1s socket
		// outage during the ack flush can still lose an ack for a message that WAS
		// injected. Its id stays in the processed set (the un-dedup at :297 fires
		// only on prompt REJECTION = not-injected), so the Server's redelivery sweep
		// re-arrives here and would strand the delivery cursor forever with no ack.
		// The frozen record's intent is that "the message_id dedup absorbs" a lost
		// ack (design.md:405-406) and "a duplicate ack is a no-op" on the Server
		// (:338) — so we RE-EMIT the ack to recover the cursor. But only when the id
		// is ALREADY INJECTED, i.e. NOT still pending in `#deliverQueue`: because the
		// set is populated at enqueue-time, a mid-turn duplicate of a message still
		// QUEUED (not yet flushed) must NOT be acked — "ack means injected"
		// (:257-262), and acking a still-queued message then losing it to a
		// crash-before-flush would strand it the other way. A still-queued duplicate
		// keeps the current behavior (counted, no re-ack); the queued copy is acked
		// when the queue flushes.
		if (this.#processedMessageIds.has(msg.id)) {
			if (!this.#deliverQueue.some((queued) => queued.id === msg.id)) {
				const value: DeliveryAck = create(DeliveryAckSchema, {
					messageId: msg.id,
				});
				this.#sink.emit({ kind: "deliveryAck", value });
			}
			this.#onUnmapped({
				kind: "unmapped",
				eventType: "deliver",
				reason: "duplicate deliver — message_id already processed",
			});
			return;
		}
		this.#processedMessageIds.add(msg.id);
		this.#deliverQueue.push(msg);
		this.#deliverFromHandles.set(msg.id, fromHandle);
		this.#deliverTraceparents.set(msg.id, traceparent);
		// Idle deliver starts a turn immediately (frozen :799/:810); a mid-turn
		// deliver waits for the `agent_end` flush. "Idle" consults BOTH the
		// event-derived `#turnActive` AND the authoritative `#session.isStreaming`
		// (pi-coding-agent agent-session.ts:6469, the inner Agent's real streaming
		// state): a control-driven prompt (`#applyControl` case "prompt") sets the
		// inner agent streaming SYNCHRONOUSLY (pi-agent-core agent.ts:1072) but
		// only flips `#turnActive` later, off the async `agent_start` event
		// (:1214/:1260). In that window `#turnActive` is still false while a turn
		// is genuinely live; flushing there would inject into a streaming agent,
		// the prompt would reject with AgentBusyError, and the message would be
		// acked-and-dropped. Gating on `isStreaming` too keeps the message queued
		// to ride the live turn's `agent_end` flush.
		if (!this.#turnActive && !this.#session.isStreaming) {
			this.#flushTurnEnd();
		} else if (!this.#turnActive) {
			// Queued because the session reports streaming while NO tracked turn is
			// active — the strand shape (RIG-2644): an untracked in-flight (startup
			// probe/prewarm) holds `isStreaming` true and no `agent_end` will arrive
			// to flush. Arm a recovery that flushes once the stream settles. A live
			// TRACKED turn (`#turnActive` true) is NOT this case — its `agent_end`
			// flushes normally, so it is left to that path.
			this.#armStrandRecovery();
		}
	}

	// RIG-2644 — flush the deliver queue once an UNTRACKED stream (a
	// startup probe/prewarm holding `#session.isStreaming` with no turn edge)
	// settles, so a message that queued against it is not stranded. `waitForIdle`
	// resolves only when the inner agent's streaming and post-prompt recovery are
	// done (agent-session.ts:6478-6481), i.e. `#session.isStreaming` is false — so
	// the re-check gate matches the caller's idle gate exactly. Idempotent via the
	// `#strandRecoveryArmed` latch: one recovery in flight at a time. On resolve:
	// no-op if the agent has closed (`#closed` — a resolve
	// racing terminal STOPPED/ERRORED must not start a post-terminal turn) or a
	// TRACKED turn started meanwhile (`#turnActive`) or the queue already drained;
	// flush if still-idle with a non-empty queue; re-arm if a fresh probe is still
	// streaming. Fire-and-forget: `waitForIdle` can reject (an abort/dispose during
	// the probe), so the chain terminates in a `.catch` that routes to onUnmapped
	// rather than an unhandled rejection; the flush's own prompt rejection is
	// belted separately in `#flushTurnEnd`.
	#armStrandRecovery(): void {
		if (this.#strandRecoveryArmed) return;
		this.#strandRecoveryArmed = true;
		void this.#session
			.waitForIdle()
			.then(() => {
				this.#strandRecoveryArmed = false;
				if (this.#closed) return;
				if (this.#deliverQueue.length === 0 && this.#forgeQueue.length === 0)
					return;
				if (!this.#turnActive && !this.#session.isStreaming) {
					this.#flushTurnEnd();
				} else if (!this.#turnActive) {
					// Still an untracked stream (a second probe) — re-arm.
					this.#armStrandRecovery();
				}
			})
			.catch((err) => {
				this.#strandRecoveryArmed = false;
				this.#onUnmapped({
					kind: "unmapped",
					eventType: "strand_recovery",
					reason: `waitForIdle recovery failed: ${err}`,
				});
			});
	}

	// SEA-1310 §8 — channel-borne steer arm. The entry the immediate handle calls
	// when a `SteerControl.message` decodes (populated by SEA-1569 (T7)). Unlike
	// deliver (which coalesces to a turn-end prompt), a steer is an @-mention
	// interrupt: mid-turn it injects into the running loop (interrupt at the next
	// tool boundary); idle it starts a fresh turn to drain the injected steer
	// (frozen record :788-814, precedence :537-562; design: architecture-lineage amendment
	// :399-408 — "a frame arriving while the agent is idle starts a new turn;
	// only the @-mention steer interrupts a turn in progress"). The `message_id`
	// dedup shares deliver's `#processedMessageIds` set (a message is steer XOR
	// deliver per D5, but a lost-ack sweep can re-arrive it as the other; the
	// shared set + guarded re-ack recovers the Server cursor). The ack means
	// "injected", emitted at injection time (frozen :540-546, :283). The replay
	// barrier is enforced UPSTREAM at the control source (control-source.ts), so
	// this method does not re-check it — same as `deliver`.
	steer(msg: Message, fromHandle = "", traceparent = ""): void {
		// A message with no id cannot be acked or deduped — fail-visible, never a
		// silent drop (and never injected, since there would be no receipt for it).
		// Mirrors deliver's empty-id guard.
		if (msg.id === "") {
			this.#onUnmapped({
				kind: "unmapped",
				eventType: "steer",
				reason: "steer missing Message.id — cannot ack or dedup",
			});
			return;
		}
		// Sweep-redelivery safety: a message already processed this session is not
		// re-injected. `#processedMessageIds` is SHARED with deliver, and an id can
		// cross-arrive as the other type (a message is steer XOR deliver per D5,
		// but a lost-ack sweep can re-arrive it as the other — :254-257). So the
		// re-ack must carry deliver's SAME queue-membership guard (:217): a
		// processed id still pending in `#deliverQueue` was only QUEUED by a
		// mid-turn deliver, NOT injected — re-acking it would break "ack means
		// injected" and expose the crash-before-flush data loss the deliver guard
		// prevents (the queued copy acks when it flushes). Only an id NOT in
		// `#deliverQueue` is genuinely already injected, so only then do we re-ack
		// (to recover a stranded Server cursor when the priority-lane ack was lost;
		// frozen design.md:405-406, a duplicate ack is a no-op on the Server :338).
		// The "duplicate steer" unmapped surface stays UNCONDITIONAL.
		if (this.#processedMessageIds.has(msg.id)) {
			if (!this.#deliverQueue.some((queued) => queued.id === msg.id)) {
				const value: DeliveryAck = create(DeliveryAckSchema, {
					messageId: msg.id,
				});
				this.#sink.emit({ kind: "deliveryAck", value });
			}
			this.#onUnmapped({
				kind: "unmapped",
				eventType: "steer",
				reason: "duplicate steer — message_id already processed",
			});
			return;
		}
		this.#processedMessageIds.add(msg.id);
		// The mention text, formatted once via the single deliver formatter (a
		// single-element batch — no new formatter). Used as the mid-turn steering
		// message's content AND as the idle turn-start prompt.
		const content = formatDeliversForPrompt([msg]);
		// Idle is computed exactly as deliver does: the event-derived `#turnActive`
		// AND the authoritative `#session.isStreaming` (closes the control-prompt
		// spin-up race the event flag alone cannot — see the deliver comment).
		const idle = !this.#turnActive && !this.#session.isStreaming;
		if (!idle) {
			// Mid-turn: ENQUEUE onto the steering queue so the running loop drains it
			// at its next injection boundary — an interrupt in place, no new turn.
			// The `steering` flag is what the agent's pre-LLM transform reads to wrap
			// the message as a mid-turn interrupt (pi-ai types.d.ts:490); timestamp is
			// a fixed 0 (never asserted; a wall-clock read would be non-deterministic).
			// `agent.steer` is synchronous void and cannot reject, so the ack (means
			// "injected") rides the next microtask, mirroring deliver's timing.
			const agentMsg: AgentMessage = {
				role: "user",
				content,
				steering: true,
				attribution: "user",
				timestamp: 0,
			};
			this.#session.agent.steer(agentMsg);
			// Case-3 LINK (design record §T2): the `invoke_agent` span already exists
			// and is already parented; a mid-turn steer adds a link to the captured
			// live span, then stamps the message id onto the topology-independent
			// query key. No-ops when the tracer is absent or no turn span is live.
			this.#tracer?.linkActiveTurn(traceparent, msg.id);
			this.#turnMessageIds.push(msg.id);
			this.#tracer?.stampActiveTurn(this.#turnMessageIds.join(","));
			queueMicrotask(() => {
				const value: DeliveryAck = create(DeliveryAckSchema, {
					messageId: msg.id,
				});
				this.#sink.emit({ kind: "deliveryAck", value });
				this.#emitInjection(SessionInjectionKind.STEER, msg.id, fromHandle);
			});
			return;
		}
		// Idle: START A NEW TURN with the mention as its content via `prompt()`,
		// mirroring the idle-DELIVER path (`#flushTurnEnd`). The frozen record ties
		// an idle frame to "starts a new turn" (design: architecture-lineage). `prompt()`
		// runs on ANY history — including a fresh, just-spawned peer's EMPTY history
		// — whereas `continue()` (the earlier mechanism) rejects "No messages to
		// continue from" on a zero-history session, so an @-mention to an idle,
		// never-run agent silently dropped its steer and never emitted its
		// SessionInjection (RIG-2488: the leg-4 e2e caught exactly this over the
		// wire). The idle path does NOT pre-enqueue onto the steering queue — the
		// prompt carries the mention as the turn's initial content, so an enqueued
		// copy would be a double-inject (drained again by a later turn).
		//
		// Rejection-safety belt (mirrors `#flushTurnEnd`): `prompt` injects
		// synchronously up to its first await and can only signal refusal as a
		// settled REJECTION — the AgentBusyError streaming guard or the "No model
		// configured" throw, both BEFORE any injection. (AgentBusyError cannot fire
		// here: steer() is synchronous from the idle gate to this call, so
		// `isStreaming` cannot change between them.) On rejection the turn did not
		// start: un-dedup the id (so the Server redelivers) and surface it, no ack.
		// Otherwise ack on the microtask, gated on the prompt not having
		// synchronously-rejected — the settled-rejection `.catch` is scheduled
		// before this `queueMicrotask`, so `rejected` is observed deterministically
		// under `tick()`: no timer, no race.
		// Optimistically mark a turn active — starting a turn here closes the same
		// spin-up window `#flushTurnEnd` does: before `agent_start` propagates,
		// `isStreaming` may still read false, so a follow-on deliver/steer in that
		// window would re-gate as idle and start a SECOND turn (→ AgentBusyError).
		// The rejection path below clears it, since a refused prompt starts no turn.
		this.#turnActive = true;
		let rejected = false;
		let acked = false;
		// Case-1 PARENT (design record §T2): the remote context becomes the new
		// turn's `invoke_agent` parent. `runWithParent` wraps the SYNCHRONOUS
		// `prompt()` call — the loop starts the span before its first await, still
		// inside the wrapped context, so the parentage rides `context.active()`
		// (the SDK-synchronicity property the idle-steer-parent test canaries).
		// When the tracer is absent, `prompt(content)` runs directly — bit-identical.
		const started =
			this.#tracer === undefined
				? this.#session.agent.prompt(content)
				: this.#tracer.runWithParent(traceparent, () =>
						this.#session.agent.prompt(content),
					);
		// Stamp the topology-independent query key on the captured span (the hook
		// fired synchronously inside `prompt()`, so the slot is set by now). This
		// message STARTS the turn, so it RESETS the accumulator to its own id
		// (a later mid-turn steer appends + re-stamps the full joined list). Reset
		// at every turn-start site keeps the accumulator self-contained — no
		// dependence on an `agent_end` edge that a rejected turn-start never fires.
		// No-op when the tracer is absent or no turn span is live.
		this.#turnMessageIds.length = 0;
		this.#turnMessageIds.push(msg.id);
		this.#tracer?.stampActiveTurn(this.#turnMessageIds.join(","));
		started.catch((err) => {
			if (acked) return;
			rejected = true;
			this.#turnActive = false;
			this.#processedMessageIds.delete(msg.id);
			this.#onUnmapped({
				kind: "unmapped",
				eventType: "steer:prompt",
				reason: `steer prompt rejected — not injected, un-acked for redelivery: ${String(err)}`,
			});
		});
		queueMicrotask(() => {
			if (rejected) return;
			acked = true;
			const value: DeliveryAck = create(DeliveryAckSchema, {
				messageId: msg.id,
			});
			this.#sink.emit({ kind: "deliveryAck", value });
			this.#emitInjection(SessionInjectionKind.STEER, msg.id, fromHandle);
		});
	}

	// RIG-2732 W3 — the turn-end forge-notification arm. The entry the immediate
	// handle calls when an AgentControl.forge_notification decodes (past the
	// replay barrier, enforced upstream at the control source, control-source.ts).
	// The RT-3 sibling of `deliver`: a mid-turn notification coalesces onto the
	// turn-end queue and flushes as one prompt at `agent_end`; an idle
	// notification flushes at once (frozen deliver model, agent.ts turn comments).
	//
	// UNLIKE deliver, BOTH acks are deferred to the FLUSH: the control-rail ack
	// (via `ackRail`, retiring the op on the AckCursor) AND the forge delivery ack
	// (the ForgeNotificationAck frame that advances delivered_revision) fire at
	// flush, never at decode — a decode-ack would discard the Runner's
	// retain-until-acked durability for the decode->flush window (design.md
	// 1006-1013). Rail-level redelivery dedup rides the control-source's SEQ-based
	// `queued`/`acks.isApplied` path (no content-tuple key), so this arm keeps NO
	// per-notification dedup set of its own: a duplicate in the decode->flush
	// window never reaches here (the source drops it as already-queued).
	forgeNotification(
		notification: ForgeNotification,
		ackRail: () => void,
	): void {
		this.#forgeQueue.push({ notification, ackRail });
		// Idle notification starts a turn immediately; a mid-turn one waits for the
		// `agent_end` flush. "Idle" consults BOTH `#turnActive` AND the
		// authoritative `#session.isStreaming`, exactly as deliver does (the
		// control-prompt spin-up race the event flag alone cannot close).
		if (!this.#turnActive && !this.#session.isStreaming) {
			this.#flushTurnEnd();
		} else if (!this.#turnActive) {
			// Queued while the session reports streaming with NO tracked turn — the
			// strand shape (RIG-2644): an untracked in-flight holds `isStreaming`
			// true and no `agent_end` will arrive to flush. Arm the shared recovery
			// that flushes once the stream settles.
			this.#armStrandRecovery();
		}
	}

	// Track a session turn edge (SEA-1310 §8, RIG-2732 W3). A turn-start edge
	// marks the session active; `agent_end` settles it and flushes the coalesced
	// deliver AND forge-notification queues as turn-end prompts. See the
	// `#turnActive` field comment for why the flush is safe synchronously on the
	// `agent_end` edge.
	#trackTurn(eventType: string): void {
		switch (eventType) {
			case "agent_start":
			case "turn_start":
			case "message_start":
				this.#turnActive = true;
				return;
			case "agent_end":
				this.#turnActive = false;
				this.#flushTurnEnd();
				return;
		}
	}

	// RIG-2732 W3 — flush BOTH coalesced turn-end queues (DELIVER + FORGE) as
	// EXACTLY ONE prompt on a single turn-end edge, then emit ALL acks in one
	// shared post-injection microtask. Coalescing to a single prompt is
	// load-bearing, not a tidy-up: `Agent.prompt` sets `#state.isStreaming` true
	// SYNCHRONOUSLY up to its first await (pi-agent-core agent.ts:1072), so two
	// independent `prompt()` calls on one synchronous `agent_end` edge collide —
	// the first sets isStreaming, the second throws AgentBusyError and its batch
	// is dropped. One prompt carrying both sections is the fix (the mixed-queue
	// turn is the normal case for an active agent).
	//
	// The prompt input concatenates the two pure renderers in a STABLE order —
	// delivers first, then forge — joined with the SAME "\n\n" separator both
	// renderers use between their own sections. A queue that is empty contributes
	// no section, so a single non-empty queue reproduces today's single-arm
	// prompt byte-for-byte (idle single-arm behavior preserved).
	//
	// The ack means "injected into the agent", NOT "the resulting turn finished"
	// (frozen :800): emitted at injection time (the microtask right after the
	// synchronous `prompt` call), never gated behind the prompt's completion — so
	// a crash mid-turn does not lose a receipt for a batch that WAS injected.
	//
	// Rejection-safety belt (SEA-1310 §8 / RIG-2732 W3): `Agent.prompt` injects
	// synchronously up to its first await and can only signal a NOT-injected
	// batch as a settled REJECTION — a synchronous throw at its very top: the
	// AgentBusyError streaming guard (agent.ts:985) or the "No model" check
	// (:990), both BEFORE any injection. A genuine mid-turn failure does NOT
	// reject — the loop swallows it, emits `agent_end`, and RESOLVES
	// (:1279-1332). So a rejection here means NEITHER batch was injected, and we
	// fail closed for BOTH kinds: the acks ride the next microtask, gated on the
	// prompt not having synchronously-rejected (a settled-rejected promise
	// schedules its `.catch` microtask BEFORE this `queueMicrotask`, so the guard
	// flag is observed deterministically — no timer, no race). On rejection: for
	// delivers, un-dedup the ids so the Server (whose delivery cursor never
	// advanced) redelivers; for forge, do NOT re-enqueue and do NOT fire ackRail
	// (the Runner retained every op past its rail cursor and redelivers on
	// reconnect) — the Runner/Server are the single redelivery authorities, so a
	// local re-enqueue on top of their resend would double-inject.
	#flushTurnEnd(): void {
		const delivers = this.#deliverQueue;
		const forges = this.#forgeQueue;
		this.#deliverQueue = [];
		this.#forgeQueue = [];
		if (delivers.length === 0 && forges.length === 0) return;
		const sections: string[] = [];
		if (delivers.length > 0) sections.push(formatDeliversForPrompt(delivers));
		if (forges.length > 0)
			sections.push(
				formatForgeNotifications(forges.map((e) => e.notification)),
			);
		const input = sections.join("\n\n");
		// Optimistically mark a turn active — an idle flush starts one; the
		// rejection path below clears it, since a refused prompt starts no turn.
		this.#turnActive = true;
		let rejected = false;
		let acked = false;
		// Reset the per-turn message-id accumulator at this turn-start (design
		// record §T2): a flush STARTS a new turn (including a forge-only flush with
		// zero delivers), so the accumulator must carry only THIS turn's feeding
		// messages. The deliver ids append in the ack microtask below; a later
		// mid-turn steer appends onto the same accumulator. Reset here (not at
		// `agent_end`) keeps it self-contained — a rejected turn-start fires no
		// `agent_end`.
		this.#turnMessageIds.length = 0;
		// Trace-continuity topology (design record §T2): a SINGLE-message deliver
		// batch PARENTS the new turn on that message's stashed traceparent (case-2,
		// N=1); a MULTI-message batch runs bare and LINKS each message's context
		// onto the turn span in the ack microtask (case-2, N>1). A forge-only flush
		// carries no channel message, so it runs bare with no parent/link. The
		// parent must wrap the SYNCHRONOUS `prompt()` (the loop starts the span
		// before its first await, inside the wrapped context); links + the
		// `compass.message.ids` stamp attach on the next microtask, once the hook
		// has captured the live span. Tracer absent ⇒ `prompt(input)` runs directly,
		// bit-identical to today.
		const startPrompt = (): Promise<void> => this.#session.agent.prompt(input);
		const started =
			this.#tracer !== undefined && delivers.length === 1
				? this.#tracer.runWithParent(
						this.#deliverTraceparents.get(delivers[0].id) ?? "",
						startPrompt,
					)
				: startPrompt();
		started.catch((err) => {
			// Reached ONLY on a settled-rejected prompt, i.e. neither batch was
			// injected (see the method comment). If the acks already went out this
			// is the SDK-impossible post-injection rejection — leave the injected
			// batches alone rather than un-dedup a message that WAS delivered.
			if (acked) return;
			rejected = true;
			this.#turnActive = false;
			// DELIVER: un-dedup every id so the Server redelivers + re-injects, and
			// drop the stashed from-handle + traceparent (the redelivery re-stashes).
			for (const msg of delivers) {
				this.#processedMessageIds.delete(msg.id);
				this.#deliverFromHandles.delete(msg.id);
				this.#deliverTraceparents.delete(msg.id);
			}
			if (delivers.length > 0) {
				this.#onUnmapped({
					kind: "unmapped",
					eventType: "deliver:prompt",
					reason: `deliver flush prompt rejected — batch not injected, un-acked for redelivery: ${String(err)}`,
				});
			}
			// FORGE: no ackRail fired, so the Runner still retains every op past
			// its rail cursor and redelivers on reconnect. Do NOT re-enqueue.
			if (forges.length > 0) {
				this.#onUnmapped({
					kind: "unmapped",
					eventType: "forge:prompt",
					reason: `forge notification flush prompt rejected — batch not injected, un-acked for redelivery: ${String(err)}`,
				});
			}
		});
		queueMicrotask(() => {
			if (rejected) return;
			acked = true;
			// Trace continuity (design record §T2): the hook fired synchronously
			// inside `prompt()`, so the captured turn span is live now. A
			// multi-message batch LINKS each message's stashed context onto it; every
			// non-empty deliver batch stamps the comma-joined ids as the
			// topology-independent query key. No-ops when the tracer is absent.
			if (delivers.length > 1) {
				for (const msg of delivers) {
					this.#tracer?.linkActiveTurn(
						this.#deliverTraceparents.get(msg.id) ?? "",
						msg.id,
					);
				}
			}
			for (const msg of delivers) {
				this.#turnMessageIds.push(msg.id);
			}
			if (delivers.length > 0) {
				this.#tracer?.stampActiveTurn(this.#turnMessageIds.join(","));
			}
			// DELIVER acks: one DeliveryAck + one DELIVER injection per message.
			for (const msg of delivers) {
				const value: DeliveryAck = create(DeliveryAckSchema, {
					messageId: msg.id,
				});
				this.#sink.emit({ kind: "deliveryAck", value });
				const fromHandle = this.#deliverFromHandles.get(msg.id) ?? "";
				this.#deliverFromHandles.delete(msg.id);
				this.#deliverTraceparents.delete(msg.id);
				this.#emitInjection(SessionInjectionKind.DELIVER, msg.id, fromHandle);
			}
			// FORGE acks: one ForgeNotificationAck frame (advances the Server's
			// delivered_revision) then the control-rail ack (`ackRail`, retiring
			// the op on the AckCursor) per notification, in that order.
			for (const entry of forges) {
				const value: ForgeNotificationAck = create(ForgeNotificationAckSchema, {
					subscriptionId: entry.notification.subscriptionId,
					revision: entry.notification.revision,
				});
				this.#sink.emit({ kind: "forgeNotificationAck", value });
				entry.ackRail();
			}
		});
	}

	// Apply one decoded control frame. Discriminated on the frozen AgentControl
	// oneof: replay applies to context (never live input); replay_complete lifts
	// the barrier; prompt/steer/config drive the session once replay has
	// settled. Control drives the inner `Agent` (`this.#session.agent`) to preserve
	// the frozen control contract (see the file header).
	async #applyControl(control: AgentControl): Promise<void> {
		switch (control.kind) {
			case "replay":
				// TranscriptReplay → seed context, never execute as live input.
				this.#session.agent.appendMessage(control.message);
				return;
			case "replayComplete":
				this.#replayComplete = true;
				return;
			case "config":
				if (control.systemPrompt !== undefined)
					this.#session.agent.setSystemPrompt(control.systemPrompt);
				if (control.tools !== undefined)
					this.#session.agent.setTools(this.#withNatives(control.tools));
				return;
			case "prompt":
				// Live input: the Runner holds these until ReplayComplete; the
				// local barrier is a backstop. A control that slips through early
				// is surfaced, not silently dropped.
				if (!this.#replayComplete) {
					this.#onUnmapped({
						kind: "unmapped",
						eventType: "control:prompt",
						reason:
							"live prompt arrived before ReplayComplete — refused by replay barrier",
					});
					return;
				}
				// A control prompt STARTS a fresh turn (a new `invoke_agent` span),
				// so reset the accumulator like every other turn-start site — else a
				// prior deliver-flush's ids would leak into this turn's
				// topology-independent query key via a later mid-turn steer. No-op
				// when the tracer is absent (the array op is unobservable off-path).
				this.#turnMessageIds.length = 0;
				await this.#session.agent.prompt(control.input);
				return;
			case "steer":
				if (!this.#replayComplete) {
					this.#onUnmapped({
						kind: "unmapped",
						eventType: "control:steer",
						reason:
							"live steer arrived before ReplayComplete — refused by replay barrier",
					});
					return;
				}
				this.#session.agent.steer(control.message);
				return;
		}
	}

	// Merge the control's tool list with the construction-time natives, keyed by
	// name, under a strong guarantee: a native ALWAYS wins. A control may add
	// tools and reorder freely, but it can neither drop a native nor substitute
	// its own instance for one. When a control tool's name collides with a
	// native, the native instance REPLACES the control's tool at the control's
	// position (control ordering for non-colliding tools is preserved; a replaced
	// slot keeps its index but holds the native). Non-colliding natives follow.
	//
	// Comms is not a grantable capability — it is what makes this process an agent
	// rather than a compute job; an agent silently stripped of it cannot even
	// report that it lost it. Restricting what an account may say or see is a
	// server-side authorization decision (visibility and channel membership,
	// enforced in SQL), so neither an omission nor a same-name substitution in a
	// control frame may make that decision here, in a layer with no authorization
	// code at all. An attempted substitution (a same-named tool that is NOT the
	// native instance) is a server misconfig: the native is kept and the attempt
	// is surfaced through #onUnmapped, never silently overridden. Re-supplying the
	// exact native instance is fine (no event). The input array passes through
	// unchanged when nothing is missing or substituted, so the common case
	// allocates nothing.
	#withNatives(tools: AgentTool[]): AgentTool[] {
		if (this.#natives.length === 0) return tools;
		const nativeByName = new Map(
			this.#natives.map((native) => [native.name, native]),
		);
		const present = new Set<string>();
		let merged: AgentTool[] | undefined;
		for (let i = 0; i < tools.length; i++) {
			const controlTool = tools[i];
			present.add(controlTool.name);
			const native = nativeByName.get(controlTool.name);
			if (native === undefined || native === controlTool) continue;
			// Same name, different instance: an attempted substitution of a
			// native. Keep the native at the control's position and surface the
			// rejected misconfig — never a silent override.
			merged ??= tools.slice();
			merged[i] = native;
			this.#onUnmapped({
				kind: "unmapped",
				eventType: "control:config",
				reason: `config control tried to replace native tool "${native.name}" — substitution rejected, native kept`,
			});
		}
		for (const native of this.#natives) {
			if (present.has(native.name)) continue;
			merged ??= tools.slice();
			merged.push(native);
		}
		return merged ?? tools;
	}
}

// Coalesce a batch of delivered channel messages into ONE prompt input string
// (SEA-1310 §8). Pure + exported so it is unit-testable. The batch is GROUPED
// per topic (`msg.topicId`) at format time: a topic belongs to exactly one
// channel, so topic-grouping is per-(channel, topic), one digest section per
// topic within the channel batch (D3/D4). This is FORMAT-TIME only — the flush
// still emits one coalesced prompt and one ack per message (`#flushTurnEnd`),
// so the SEA-1310 §8 ack-safety belt is untouched.
//
// Topic order is first-seen; message order within a topic is preserved. Each
// message's `text`-case blocks are concatenated and an `askAnswer`-case block
// (a delivered ask answer) renders via `formatAskAnswerForPrompt`; a bare `ask`
// block stays ignored — deliver carries channel text and answers, and a raised
// ask is a separate surface. A blank text (a message with no rendered blocks)
// still contributes its slot so a section is a faithful 1:1 with its group.
//
// RIG-2664: the coalesced prompt ends with ONE terse reply cue for the whole
// batch, pointing at `comms_post_message`, closing the gap a bare
// `Topic <id>:\n<text>` digest left open (the model would otherwise narrate its
// reply into its own turn, which the operator never reads). The cue does NOT
// re-list the batch's topic ids — each id already prints in its `Topic <id>:`
// section header one line up, so the model addresses the topic from the section
// it is replying to; re-listing them only duplicated 32-char hex ids per batch
// (~14 tokens each, unbounded in batch size) with no added addressing. Terse by
// design: this cue rides EVERY delivered batch, so the load-bearing "why" — the
// operator reads the channel, not your session log — lives once in the manager
// block-0 SYSTEM.md and is not re-paid here per delivery.
export function formatDeliversForPrompt(batch: readonly Message[]): string {
	// An empty batch has nothing to reply to — return "" rather than a bare cue
	// (both prod callers guard against this, but the exported pure fn's contract
	// is pinned here regardless).
	if (batch.length === 0) return "";
	const groups = new Map<string, string[]>();
	for (const msg of batch) {
		const text = msg.blocks
			.flatMap((block) => {
				if (block.block.case === "text") return [block.block.value];
				if (block.block.case === "askAnswer") {
					const ask = block.block.value.ask;
					return ask === undefined ? [] : [formatAskAnswerForPrompt(ask)];
				}
				return [];
			})
			.join("\n");
		const existing = groups.get(msg.topicId);
		if (existing) existing.push(text);
		else groups.set(msg.topicId, [text]);
	}
	const sections = Array.from(
		groups,
		([topicId, texts]) => `Topic ${topicId}:\n${texts.join("\n\n")}`,
	);
	sections.push("Reply via comms_post_message to the relevant topic.");
	return sections.join("\n\n");
}

// Render a delivered `ask_answer` message's answered `Ask` snapshot into a
// prompt section (RIG-2257 answer lane). Pure + exported so it is unit-testable,
// mirroring `formatDeliversForPrompt`. ONE section per question on the ask: the
// question text, the chosen option LABELS (each id in `question.chosenOptionIds`
// resolved against `question.options[].id` — the ids Lane 1 minted as "0","1",…
// in options order), and the free-text `question.customText` when present.
//
// Every interpolated value passes through `flat` (the same marker-line render
// guard the comms renderer uses), so an operator-controlled option label or
// free-text answer cannot break the section structure with an embedded newline.
// A chosen id that resolves to no recorded option is rendered DEFENSIVELY by its
// id rather than dropped — the answer is surfaced faithfully even when the
// correlation is partial, never fabricated into a wrong label.
export function formatAskAnswerForPrompt(ask: Ask): string {
	const sections = ask.questions.map((question) => {
		const lines = [`Question: ${flat(question.question)}`];
		const labelById = new Map(question.options.map((o) => [o.id, o.label]));
		const chosen = question.chosenOptionIds.map((id) => {
			const label = labelById.get(id);
			return label === undefined ? `option ${flat(id)} (unknown)` : flat(label);
		});
		if (chosen.length > 0) lines.push(`Chose: ${chosen.join(", ")}`);
		if (question.customText !== "")
			lines.push(`Custom answer: ${flat(question.customText)}`);
		return lines.join("\n");
	});
	return `Answer received for ask:\n${sections.join("\n\n")}`;
}

// Coalesce a batch of forge notifications into ONE prompt input string
// (RIG-2732 W3). Pure + exported so it is unit-testable, mirroring
// `formatDeliversForPrompt`. ONE section per notification, first-seen order:
// a header line `<forge> <repo>#<number> — <kind>` (the artifact coordinate the
// agent re-reads), then the per-kind payload:
//   - COMMENT: the new comment's author + body (CommentRef.forge_account / body).
//   - STATE:   the new forge state string ("closed", "merged", …).
//   - CHECKS:  the rolled-up CI/status state (ChecksSummary.state).
//   - REVIEW:  the verdict (ForgeNotification.state) + the review body
//              (CommentRef.body) — the two the REVIEW kind carries (proto
//              forge.proto:102-103).
//   - OPENED:  a container-scope new artifact — rendered "new <kind> <repo>#<n>"
//              (design.md:1005-1006); the envelope's number/url address it.
//   - UPDATE / UNSPECIFIED: header only (a payload-free re-read cue — the
//              synthesized catch-up UPDATE and any unmapped kind, design.md:369).
// Every interpolated value passes through `flat` (the marker-line render guard),
// so a forge-controlled title/body/verdict cannot break the section structure
// with an embedded newline. The batch ends with ONE terse cue: the notification
// is a signal to re-read the artifact with the forge tools, not a channel
// message to reply to.
export function formatForgeNotifications(
	batch: readonly ForgeNotification[],
): string {
	if (batch.length === 0) return "";
	const sections = batch.map((n) => {
		const coord = `${flat(n.repo)}#${n.number}`;
		const kind = forgeKindLabel(n.change);
		// The forge display name: the ForgeRef host when set, else "forge" (the
		// ForgeRef is optional on the wire, DL-091).
		const forge = flat(n.forge?.host ?? "forge");
		if (n.change === ForgeNotificationKind.OPENED) {
			return `${forge} ${coord} — new ${kind} ${coord}`;
		}
		const lines = [`${forge} ${coord} — ${kind}`];
		switch (n.change) {
			case ForgeNotificationKind.COMMENT: {
				const c = n.comment;
				if (c !== undefined) {
					const who =
						c.forgeAccount === "" ? "comment" : `@${flat(c.forgeAccount)}`;
					lines.push(`${who}: ${flat(c.body)}`);
				}
				break;
			}
			case ForgeNotificationKind.STATE:
				if (n.state !== "") lines.push(`State: ${flat(n.state)}`);
				break;
			case ForgeNotificationKind.CHECKS:
				if (n.checks !== undefined)
					lines.push(`Checks: ${flat(n.checks.state)}`);
				break;
			case ForgeNotificationKind.REVIEW:
				if (n.state !== "") lines.push(`Verdict: ${flat(n.state)}`);
				if (n.comment !== undefined && n.comment.body !== "")
					lines.push(`Review: ${flat(n.comment.body)}`);
				break;
		}
		if (n.url !== "") lines.push(flat(n.url));
		return lines.join("\n");
	});
	sections.push("Re-read the artifact with the forge tools to act on this.");
	return sections.join("\n\n");
}

// A stable human label per notification kind for the section header. UNSPECIFIED
// and any future/unmapped kind render "update" — a payload-free re-read cue,
// never a crash (symmetric with the mapper's unmapped arm).
function forgeKindLabel(kind: ForgeNotificationKind): string {
	switch (kind) {
		case ForgeNotificationKind.COMMENT:
			return "comment";
		case ForgeNotificationKind.STATE:
			return "state";
		case ForgeNotificationKind.CHECKS:
			return "checks";
		case ForgeNotificationKind.REVIEW:
			return "review";
		case ForgeNotificationKind.OPENED:
			return "opened";
		default:
			return "update";
	}
}
