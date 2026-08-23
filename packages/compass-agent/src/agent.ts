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
import { createPendingAsks, type PendingAsks } from "./comms";
import {
	AgentSessionState,
	type AskQuestion,
	type AskQuestionAnswer,
	create,
	type DeliveryAck,
	DeliveryAckSchema,
	type Message,
	type SessionFrame,
	SessionFrameSchema,
	SessionInjectionKind,
} from "./compassv1";
import type { AgentControl, ControlSource } from "./control";
import type { FrameSink } from "./frame";
import { EventMapper, type UnmappedEvent } from "./mapping";
import { flat } from "./render-guard";

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
	// The correlation registry the raise tool (`comms_post_ask`) records minted
	// ask ids into, so the askAnswer apply arm can render an inbound answer
	// against the questions the model asked. Optional: when omitted the agent
	// mints a fresh empty registry (see the constructor), so the arm always has a
	// well-defined registry and an answer whose ask id it never recorded lands on
	// the unknown-ask-id safety net rather than crashing.
	readonly pendingAsks?: PendingAsks;
}

export class CompassAgent {
	readonly #session: AgentSession;
	readonly #sink: FrameSink;
	readonly #control: ControlSource;
	readonly #mapper: EventMapper;
	readonly #onUnmapped: (u: UnmappedEvent) => void;
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
	// Session-lifetime dedup set keyed on `Message.id`. A sweep redelivery under a
	// fresh control_seq (independent of the control-source's seq dedup) is dropped
	// here so a message is injected at most once (frozen record :811-812).
	readonly #processedMessageIds = new Set<string>();
	// RIG-1509 answer lane — the askAnswer sibling of `#deliverQueue`. A
	// post-barrier `askAnswer` is formatted into a plain prompt string and rides
	// the SAME turn-end coalescing edge (`agent_end`) delivers use, but it is a
	// STRING, not a `Message`: it carries no `Message.id`, so it earns no
	// DeliveryAck — an `askAnswer` op is acked by the control loop returning
	// (apply-then-ack), not by a delivery ack. It cannot live in the
	// Message-typed `#deliverQueue`, hence a sibling queue; both drain into ONE
	// prompt at the same flush (`#flushDelivers`) because two back-to-back
	// `agent.prompt` calls on one edge would collide — the first sets the inner
	// agent streaming synchronously (pi-agent-core agent.ts:1072) and the second
	// would reject AgentBusyError.
	#askAnswerQueue: string[] = [];
	// RIG-1509 correlation registry (co-ratified with RIG-1310): the ask ids the
	// raise tool minted, so the askAnswer arm renders an inbound answer against
	// the questions the model asked. Well-defined default (a fresh empty
	// registry) when the caller omits one, so the arm always has a registry and
	// an unrecorded ask id lands on the surfaced-not-fabricated safety net.
	readonly #pendingAsks: PendingAsks;

	constructor(opts: CompassAgentOptions) {
		this.#session = opts.session;
		// Tolerate a session that exposes no `state`/`state.tools`: this is a
		// read for a safety net, and a constructor that throws on a shape it
		// merely inspects is worse than one that degrades to no natives.
		this.#natives = [...(opts.session.agent?.state?.tools ?? [])];
		this.#sink = opts.sink;
		this.#control = opts.control;
		this.#mapper = new EventMapper();
		// Well-defined default: a fresh empty registry when the caller passed none,
		// so the askAnswer arm always has a `take()` to consult. cli.ts passes the
		// SAME instance the raise tool records into, joining the two lanes.
		this.#pendingAsks = opts.pendingAsks ?? createPendingAsks();
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
	deliver(msg: Message, fromHandle = ""): void {
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
		if (!this.#turnActive && !this.#session.isStreaming) this.#flushDelivers();
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
	steer(msg: Message, fromHandle = ""): void {
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
		// mirroring the idle-DELIVER path (`#flushDelivers`). The frozen record ties
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
		// Rejection-safety belt (mirrors `#flushDelivers`): `prompt` injects
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
		// spin-up window `#flushDelivers` does: before `agent_start` propagates,
		// `isStreaming` may still read false, so a follow-on deliver/steer in that
		// window would re-gate as idle and start a SECOND turn (→ AgentBusyError).
		// The rejection path below clears it, since a refused prompt starts no turn.
		this.#turnActive = true;
		let rejected = false;
		let acked = false;
		this.#session.agent.prompt(content).catch((err) => {
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

	// Track a session turn edge (SEA-1310 §8). A turn-start edge marks the session
	// active; `agent_end` settles it and flushes the coalesced deliver queue as
	// one turn-end prompt. See the `#turnActive` field comment for why the flush
	// is safe synchronously on the `agent_end` edge.
	#trackTurn(eventType: string): void {
		switch (eventType) {
			case "agent_start":
			case "turn_start":
			case "message_start":
				this.#turnActive = true;
				return;
			case "agent_end":
				this.#turnActive = false;
				this.#flushDelivers();
				return;
		}
	}

	// Flush the coalesced turn-end queues — the DELIVER queue (channel messages)
	// and the RIG-1509 ASK-ANSWER queue (formatted answer strings) — as ONE
	// prompt, then emit one delivery ack per DELIVERED message once injection is
	// known-accepted. Ask-answer strings carry no `Message.id` and earn no ack:
	// an `askAnswer` op is acked by the control loop returning (apply-then-ack),
	// not by a DeliveryAck. Both queues drain into a single `agent.prompt` on the
	// same edge because two back-to-back prompts on one edge collide (the first
	// sets the inner agent streaming synchronously, pi-agent-core agent.ts:1072,
	// and the second rejects AgentBusyError).
	//
	// The ack means "injected into the agent", NOT "the resulting turn finished"
	// (frozen :800): it is emitted at injection time (the microtask right after
	// the synchronous `prompt` call, by which point injection has happened),
	// never gated behind the prompt's completion — so a crash mid-turn does not
	// lose the receipt for a message that WAS injected.
	//
	// Rejection-safety belt (SEA-1310 §8): `Agent.prompt` injects SYNCHRONOUSLY up
	// to its first await (pi-agent-core agent.ts:1072) and can only signal refusal
	// as a promise REJECTION — a synchronous throw at its very top: the
	// AgentBusyError streaming guard (:985) or the "No model" check (:990), both
	// BEFORE any injection. A genuine mid-turn failure does NOT reject — the loop
	// swallows it, emits `agent_end`, and RESOLVES (:1279-1332). So a rejection
	// here means the batch was NOT injected. We therefore emit the acks on the
	// next microtask, gated on the prompt not having synchronously-rejected: a
	// settled-rejected promise schedules its `.catch` microtask BEFORE this
	// `queueMicrotask`, so the guard flag is observed deterministically — no
	// timer, no race. On rejection we fail closed: no ack, and the delivered ids
	// are un-deduped so the Server (which never saw an ack, so its delivery cursor
	// never advanced — agent_pb.ts:483) redelivers and re-injects them. The batch
	// is NOT locally re-enqueued: the Server is the single redelivery authority,
	// so re-enqueueing on top of its resend would double-inject. A rejected
	// ask-answer flush is surfaced (never a silent drop); its op is already acked
	// (apply-then-ack), so there is no ack to withhold and no local redelivery —
	// the within-session correlation entry was already `take()`n.
	#flushDelivers(): void {
		const batch = this.#deliverQueue;
		const answers = this.#askAnswerQueue;
		if (batch.length === 0 && answers.length === 0) return;
		this.#deliverQueue = [];
		this.#askAnswerQueue = [];
		// One prompt per edge: the coalesced deliver digest first (when any), then
		// each formatted answer section, joined by the same blank-line separator
		// `formatDeliversForPrompt` uses between its sections.
		const parts: string[] = [];
		if (batch.length > 0) parts.push(formatDeliversForPrompt(batch));
		for (const answer of answers) parts.push(answer);
		const input = parts.join("\n\n");
		// Optimistically mark a turn active — an idle flush starts one; the
		// rejection path below clears it, since a refused prompt starts no turn.
		this.#turnActive = true;
		let rejected = false;
		let acked = false;
		this.#session.agent.prompt(input).catch((err) => {
			// Reached ONLY on a settled-rejected prompt, i.e. a not-injected batch
			// (see the method comment). If the acks already went out this is the
			// SDK-impossible post-injection rejection — leave the injected batch
			// alone rather than un-dedup a message that WAS delivered.
			if (acked) return;
			rejected = true;
			this.#turnActive = false;
			for (const msg of batch) {
				this.#processedMessageIds.delete(msg.id);
				this.#deliverFromHandles.delete(msg.id);
			}
			if (batch.length > 0) {
				this.#onUnmapped({
					kind: "unmapped",
					eventType: "deliver:prompt",
					reason: `deliver flush prompt rejected — batch not injected, un-acked for redelivery: ${String(err)}`,
				});
			}
			if (answers.length > 0) {
				this.#onUnmapped({
					kind: "unmapped",
					eventType: "control:ask_answer",
					reason: `ask_answer flush prompt rejected — answer not injected: ${String(err)}`,
				});
			}
		});
		queueMicrotask(() => {
			if (rejected) return;
			acked = true;
			for (const msg of batch) {
				const value: DeliveryAck = create(DeliveryAckSchema, {
					messageId: msg.id,
				});
				this.#sink.emit({ kind: "deliveryAck", value });
				const fromHandle = this.#deliverFromHandles.get(msg.id) ?? "";
				this.#deliverFromHandles.delete(msg.id);
				this.#emitInjection(SessionInjectionKind.DELIVER, msg.id, fromHandle);
			}
		});
	}

	// Apply one decoded control frame. Discriminated on the frozen AgentControl
	// oneof: replay applies to context (never live input); replay_complete lifts
	// the barrier; prompt/steer/ask_answer/config drive the session once replay has
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
			case "askAnswer": {
				// Live input (a structured answer to an in-flight ask). Unlike
				// prompt/steer, a pre-barrier askAnswer must NOT be a counted-return
				// refusal: apply-then-ack means an arm that RETURNS is acked, and the
				// Runner then retires the op from retention (control-source.ts:16-18,
				// control.go:318) — a PERMANENT drop. Throwing instead exits the
				// control loop unacked, so the Runner redelivers the op to the next
				// session once the barrier has lifted (control-source.ts:568-570:
				// "an apply that failed must not be acked — the Runner redelivers").
				// `HoldForReplay` is not a fallback here — it has no production caller
				// (control.go:429-431). So a pre-barrier answer is thrown, converting
				// the drop into an at-least-once redelivery.
				if (!this.#replayComplete) {
					throw new Error(
						"live ask_answer arrived before ReplayComplete — thrown unacked so the Runner redelivers it post-barrier",
					);
				}
				// Correlate on the server-minted ask id (co-ratified with RIG-1310):
				// `take()` deletes-on-read, which gives free dedup — a redelivered
				// answer whose id was already consumed returns undefined and lands on
				// the unknown-ask-id arm below rather than double-injecting.
				const questions = this.#pendingAsks.take(control.askId);
				if (questions === undefined) {
					// Unknown ask id (no registry entry — e.g. a container restart wiped
					// the in-memory registry): surface a counted unmapped op naming the
					// missing correlation, NEVER a fabricated prompt. This is the
					// permanent within-session safety net; cross-restart survival is the
					// filed runner/hub owed-to-handle dependency (design T7).
					this.#onUnmapped({
						kind: "unmapped",
						eventType: "control:ask_answer",
						reason: `ask_answer for unknown ask ${control.askId} — no pending registry entry; not delivered`,
					});
					return;
				}
				// Render the answers against the recorded questions and enqueue on the
				// SAME turn-end coalescing path deliver uses: an idle answer starts a
				// turn at once; a mid-turn answer coalesces into the `agent_end` flush.
				this.#askAnswerQueue.push(
					formatAskAnswerForPrompt(questions, control.answers),
				);
				if (!this.#turnActive && !this.#session.isStreaming)
					this.#flushDelivers();
				return;
			}
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
// still emits one coalesced prompt and one ack per message (`#flushDelivers`),
// so the SEA-1310 §8 ack-safety belt is untouched.
//
// Topic order is first-seen; message order within a topic is preserved. Each
// message's `text`-case blocks are concatenated (ask-case blocks are ignored —
// deliver carries channel text; asks are a separate surface), so each message's
// text stays greppable in its section. A blank text (a message with no text
// blocks) still contributes its slot so a section is a faithful 1:1 with its
// group.
export function formatDeliversForPrompt(batch: readonly Message[]): string {
	const groups = new Map<string, string[]>();
	for (const msg of batch) {
		const text = msg.blocks
			.flatMap((block) =>
				block.block.case === "text" ? [block.block.value] : [],
			)
			.join("\n");
		const existing = groups.get(msg.topicId);
		if (existing) existing.push(text);
		else groups.set(msg.topicId, [text]);
	}
	return Array.from(
		groups,
		([topicId, texts]) => `Topic ${topicId}:\n${texts.join("\n\n")}`,
	).join("\n\n");
}

// Render an inbound `AskAnswerControl` against the questions the model asked
// (RIG-1509 answer lane). Pure + exported so it is unit-testable, mirroring
// `formatDeliversForPrompt`. ONE section per answered question: the question
// text, the chosen option LABELS (each `chosen_option_id` resolved against the
// recorded `question.options[].id` — the ids Lane 1 minted as "0","1",… in
// options order), and the free-text `custom_text` when present.
//
// Every interpolated value passes through `flat` (the same marker-line render
// guard the comms renderer uses), so an operator-controlled option label or
// free-text answer cannot break the section structure with an embedded newline.
// An answer whose question is not in the recorded set, or a chosen id that
// resolves to no recorded option, is rendered DEFENSIVELY by its id rather than
// dropped — the answer is surfaced faithfully even when the correlation is
// partial, never fabricated into a wrong label.
export function formatAskAnswerForPrompt(
	questions: readonly AskQuestion[],
	answers: readonly AskQuestionAnswer[],
): string {
	const byId = new Map(questions.map((q) => [q.questionId, q]));
	const sections = answers.map((answer) => {
		const question = byId.get(answer.questionId);
		const heading =
			question === undefined
				? `Answer to unknown question ${flat(answer.questionId)}:`
				: `Question: ${flat(question.question)}`;
		const lines = [heading];
		const options = question?.options ?? [];
		const labelById = new Map(options.map((o) => [o.id, o.label]));
		const chosen = answer.chosenOptionIds.map((id) => {
			const label = labelById.get(id);
			return label === undefined ? `option ${flat(id)} (unknown)` : flat(label);
		});
		if (chosen.length > 0) lines.push(`Chose: ${chosen.join(", ")}`);
		if (answer.customText !== "")
			lines.push(`Custom answer: ${flat(answer.customText)}`);
		return lines.join("\n");
	});
	return `Answer received for ask:\n${sections.join("\n\n")}`;
}
