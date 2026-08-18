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
// Why the session, not the bare `Agent`: the typed session renderer (compass-0.8)
// needs the `AgentSessionEvent` superset — `todo_reminder`/`todo_auto_clear`/
// `notice` and the compaction/retry orchestration events that the session emits
// but the core `Agent` event stream (`AgentEvent`, 10 cases) does not. Only
// `AgentSession.subscribe` yields that union, so the emitter subscribes at the
// session level.
//
// Why control drives `session.agent`, not the session: the control contract
// (prompt/steer/config/replay) is frozen at compass-0.6 §T5 and its wire payload
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
import type {
	AgentSession,
	ExtensionAskDialogQuestion,
	ExtensionAskDialogResult,
	ExtensionAskDialogResultItem,
	ExtensionUIDialogOptions,
} from "@oh-my-pi/pi-coding-agent";
import {
	AgentSessionState,
	create,
	type DeliveryAck,
	DeliveryAckSchema,
	type Message,
	type SessionFrame,
	SessionFrameSchema,
} from "./compassv1";
import type { AgentControl, ControlSource } from "./control";
import type { FrameSink } from "./frame";
import { EventMapper, type UnmappedEvent } from "./mapping";

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
	// Session-lifetime dedup set keyed on `Message.id`. A sweep redelivery under a
	// fresh control_seq (independent of the control-source's seq dedup) is dropped
	// here so a message is injected at most once (frozen record :811-812).
	readonly #processedMessageIds = new Set<string>();
	// Single-slot pending ask (RIG-1509). The SDK `AskTool` is
	// `concurrency: "exclusive"` and its `execute()` awaits the `askDialog`
	// promise, so at most ONE ask is in flight per session at any time —
	// therefore this is a single slot, NOT a keyed registry. `askDialog` fills it
	// when the ask tool calls out; the `askAnswer` control arm resolves + clears
	// it. `questions` are retained so the inbound answer (index-string option ids)
	// can be reconstructed back to option LABELS. `resolve` settles the in-flight
	// promise; `abort` unsubscribes the dialog's AbortSignal listener when the
	// slot is cleared (answered or superseded).
	#pendingAsk:
		| {
				readonly questions: readonly ExtensionAskDialogQuestion[];
				readonly resolve: (
					result: ExtensionAskDialogResult | undefined,
				) => void;
				readonly abort: () => void;
		  }
		| undefined;

	constructor(opts: CompassAgentOptions) {
		this.#session = opts.session;
		// Tolerate a session that exposes no `state`/`state.tools`: this is a
		// read for a safety net, and a constructor that throws on a shape it
		// merely inspects is worse than one that degrades to no natives.
		this.#natives = [...(opts.session.agent?.state?.tools ?? [])];
		this.#sink = opts.sink;
		this.#control = opts.control;
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

	// SEA-1310 §8 — deliver a channel message into the live session (RT-3). The
	// entry the immediate handle calls when a `DeliverControl.message` decodes.
	// The replay barrier is enforced UPSTREAM at the control source (a
	// pre-ReplayComplete immediate op is refused-and-counted before it reaches
	// this handle, control-source.ts), so this method does not re-check it.
	deliver(msg: Message): void {
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
	// (frozen record :788-814, precedence :537-562; compass-0.6 amendment
	// :399-408 — "a frame arriving while the agent is idle starts a new turn;
	// only the @-mention steer interrupts a turn in progress"). The `message_id`
	// dedup shares deliver's `#processedMessageIds` set (a message is steer XOR
	// deliver per D5, but a lost-ack sweep can re-arrive it as the other; the
	// shared set + guarded re-ack recovers the Server cursor). The ack means
	// "injected", emitted at injection time (frozen :540-546, :283). The replay
	// barrier is enforced UPSTREAM at the control source (control-source.ts), so
	// this method does not re-check it — same as `deliver`.
	steer(msg: Message): void {
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
		// Build the AgentMessage the inner `agent.steer` accepts, reusing the
		// single deliver formatter (single-element batch — no new formatter). The
		// `steering` flag is what the agent's pre-LLM transform reads to wrap the
		// message as a mid-turn interrupt (pi-ai types.d.ts:490). Timestamp is a
		// fixed 0 — never asserted, and a wall-clock read would make tests
		// non-deterministic.
		const agentMsg: AgentMessage = {
			role: "user",
			content: formatDeliversForPrompt([msg]),
			steering: true,
			attribution: "user",
			timestamp: 0,
		};
		// Idle is computed exactly as deliver does: the event-derived `#turnActive`
		// AND the authoritative `#session.isStreaming` (closes the control-prompt
		// spin-up race the event flag alone cannot — see the deliver comment).
		const idle = !this.#turnActive && !this.#session.isStreaming;
		// Inject: `agent.steer` only ENQUEUES the steer onto the steering queue; a
		// running loop drains it at the next injection boundary (interrupt).
		this.#session.agent.steer(agentMsg);
		if (!idle) {
			// Mid-turn: the running loop will drain the enqueued steer. `agent.steer`
			// is synchronous void and cannot reject, so the ack (means "injected")
			// rides the next microtask, mirroring deliver's ack-at-injection timing.
			queueMicrotask(() => {
				const value: DeliveryAck = create(DeliveryAckSchema, {
					messageId: msg.id,
				});
				this.#sink.emit({ kind: "deliveryAck", value });
			});
			return;
		}
		// Idle: `agent.steer` alone only enqueues, so nothing drains it — wake a
		// turn with `agent.continue()` to drain the injected steer. Apply the same
		// rejection-safety belt as `#flushDelivers`: `continue` injects
		// synchronously up to its first await and can only signal refusal as a
		// settled REJECTION. On the idle path the reachable synchronous rejection
		// is `continue`'s empty-history throw ("No messages to continue from",
		// pi-agent-core agent.ts:1035) when the inner agent reaches ReplayComplete
		// with zero replayed messages; an AgentBusyError spin-up race CANNOT fire
		// here because steer() is fully synchronous from the idle gate (:308) to
		// this call, so `isStreaming` cannot change between them. (An async-settled
		// rejection from inside the run loop — e.g. "No model configured" — is also
		// handled here, exactly as deliver's prompt() equally is.) On rejection the
		// turn did not start: roll back the steer this method pre-pushed onto the
		// inner steering queue (so no orphan survives to be drained by a later turn
		// or double-injected by the Server's redelivery), un-dedup the id (so the
		// Server redelivers), and surface it, no ack. Otherwise ack on the
		// microtask. The settled-rejection `.catch` is scheduled before this
		// `queueMicrotask`, so the `rejected` flag is observed deterministically
		// under `tick()` — no timer, no race.
		let rejected = false;
		let acked = false;
		this.#session.agent.continue().catch((err) => {
			if (acked) return;
			// Roll back the steer pre-pushed at the top of the idle path: the
			// empty-history throw (pi-agent-core agent.ts:1035) fires BEFORE any
			// steering dequeue (:1038), so the orphan is still at the queue tail and
			// this LIFO pop removes exactly it. Placed after the `if (acked)` guard
			// so a post-injection settled-rejection never pops an injected steer.
			this.#session.agent.popLastSteer();
			rejected = true;
			this.#processedMessageIds.delete(msg.id);
			this.#onUnmapped({
				kind: "unmapped",
				eventType: "steer:continue",
				reason: `steer continue rejected — not injected, un-acked for redelivery: ${String(err)}`,
			});
		});
		queueMicrotask(() => {
			if (rejected) return;
			acked = true;
			const value: DeliveryAck = create(DeliveryAckSchema, {
				messageId: msg.id,
			});
			this.#sink.emit({ kind: "deliveryAck", value });
		});
	}

	// RIG-1509 — the SDK ask seam. The Compass `ExtensionUIContext.askDialog`
	// installed in `cli.ts` delegates here: the ask tool calls out with its
	// questions, this stores the single pending slot and returns a promise the
	// tool's `execute()` awaits. The promise settles when the inbound `askAnswer`
	// control arrives (`#applyControl`) — resolved with a reconstructed
	// `ExtensionAskDialogResult` — or with `undefined` (a cancel) when the tool's
	// AbortSignal fires.
	//
	// Single-slot invariant: `AskTool` is exclusive-concurrency (design §"the key
	// architectural fact"), so a second ask cannot legitimately open while one is
	// pending. Defensively, if it does, we fail SAFE for the caller that is
	// actually blocked — the OLDER pending ask — by resolving it `undefined` (a
	// cancel it can act on) and surfacing a counted unmapped op, rather than
	// leaving it hung forever; the newer ask then takes the slot.
	askDialog(
		questions: ExtensionAskDialogQuestion[],
		dialogOptions?: ExtensionUIDialogOptions,
	): Promise<ExtensionAskDialogResult | undefined> {
		const signal = dialogOptions?.signal;
		// An already-aborted call never opens an ask: resolve undefined at once.
		if (signal?.aborted) return Promise.resolve(undefined);

		if (this.#pendingAsk !== undefined) {
			// Should be unreachable under exclusive concurrency; if it happens,
			// release the older blocked caller with a cancel rather than hang it.
			const previous = this.#pendingAsk;
			this.#pendingAsk = undefined;
			previous.abort();
			previous.resolve(undefined);
			this.#onUnmapped({
				kind: "unmapped",
				eventType: "control:ask_answer",
				reason:
					"a second ask opened while one was pending — older ask cancelled (exclusive-concurrency invariant violated)",
			});
		}

		const { promise, resolve } = Promise.withResolvers<
			ExtensionAskDialogResult | undefined
		>();
		// Honor the tool's AbortSignal: on abort, clear the slot and resolve the
		// in-flight promise `undefined` (the SDK ask contract reads `undefined` as
		// a cancel). The listener is `once` and removed by `abort()` when the slot
		// is cleared for any reason, so no stray listener survives.
		const onAbort = () => {
			if (this.#pendingAsk?.resolve !== resolve) return;
			this.#pendingAsk = undefined;
			resolve(undefined);
		};
		signal?.addEventListener("abort", onAbort, { once: true });
		this.#pendingAsk = {
			questions,
			resolve,
			abort: () => signal?.removeEventListener("abort", onAbort),
		};
		return promise;
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

	// Flush the coalesced deliver queue: drain it, format the batch into ONE
	// prompt, issue that single prompt, and emit one delivery ack per message —
	// but ONLY once injection is known-accepted. The ack means "injected into the
	// agent", NOT "the resulting turn finished" (frozen :800): it is emitted at
	// injection time (the microtask right after the synchronous `prompt` call, by
	// which point injection has happened), never gated behind the prompt's
	// completion — so a crash mid-turn does not lose the receipt for a message
	// that WAS injected.
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
	// timer, no race. On rejection we fail closed: no ack, and the ids are
	// un-deduped so the Server (which never saw an ack, so its delivery cursor
	// never advanced — agent_pb.ts:483) redelivers and re-injects them. The batch
	// is NOT locally re-enqueued: the Server is the single redelivery authority,
	// so re-enqueueing on top of its resend would double-inject.
	#flushDelivers(): void {
		if (this.#deliverQueue.length === 0) return;
		const batch = this.#deliverQueue;
		this.#deliverQueue = [];
		const input = formatDeliversForPrompt(batch);
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
			for (const msg of batch) this.#processedMessageIds.delete(msg.id);
			this.#onUnmapped({
				kind: "unmapped",
				eventType: "deliver:prompt",
				reason: `deliver flush prompt rejected — batch not injected, un-acked for redelivery: ${String(err)}`,
			});
		});
		queueMicrotask(() => {
			if (rejected) return;
			acked = true;
			for (const msg of batch) {
				const value: DeliveryAck = create(DeliveryAckSchema, {
					messageId: msg.id,
				});
				this.#sink.emit({ kind: "deliveryAck", value });
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
			case "askAnswer":
				// Live input (a structured answer to an in-flight ask): the Runner
				// holds these behind the replay barrier like prompt/steer; a frame
				// slipping through early is surfaced, not dropped.
				if (!this.#replayComplete) {
					this.#onUnmapped({
						kind: "unmapped",
						eventType: "control:ask_answer",
						reason:
							"live ask_answer arrived before ReplayComplete — refused by replay barrier",
					});
					return;
				}
				// Deliver the answer to the in-flight `askDialog` promise (RIG-1509).
				// Single-slot invariant: `AskTool` is exclusive-concurrency, so THIS
				// answer resolves THE one pending ask — `askId` is informational
				// (surfaced only on the no-pending unmapped op), never a correlation
				// key. No pending ask → nothing to answer: surface a counted unmapped
				// op, never throw or drop.
				if (this.#pendingAsk === undefined) {
					this.#onUnmapped({
						kind: "unmapped",
						eventType: "control:ask_answer",
						reason: `no in-flight ask to answer (askId=${control.askId})`,
					});
					return;
				}
				{
					const pending = this.#pendingAsk;
					this.#pendingAsk = undefined;
					pending.abort();
					const result = this.#reconstructAskResult(pending.questions, control);
					pending.resolve(result);
				}
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

	// Reconstruct the SDK `ExtensionAskDialogResult` from the inbound answer
	// (RIG-1509). The wire `AskQuestionAnswer.chosenOptionIds` are option ids —
	// zero-based indices as decimal strings (`"0"`, `"1"`, …) minted in question
	// order (design §Mapping) — but the SDK result carries option LABELS in
	// `selectedOptions`. So each id is resolved back to its label by indexing the
	// original question's `options`. We emit one result item PER original question
	// in order (mirrors the native forward-skip): a question with no matching
	// answer entry yields an empty item (`selectedOptions: []`, no customInput).
	// A non-numeric or out-of-range option id is skipped and surfaced as a counted
	// unmapped op — never thrown, never dropped silently.
	#reconstructAskResult(
		questions: readonly ExtensionAskDialogQuestion[],
		control: Extract<AgentControl, { kind: "askAnswer" }>,
	): ExtensionAskDialogResult {
		const answerByQuestionId = new Map(
			control.answers.map((answer) => [answer.questionId, answer]),
		);
		const results: ExtensionAskDialogResultItem[] = questions.map(
			(question) => {
				const answer = answerByQuestionId.get(question.id);
				const selectedOptions: string[] = [];
				if (answer !== undefined) {
					for (const id of answer.chosenOptionIds) {
						// Strict decimal-literal parse: `Number()` alone would coerce
						// "", whitespace, "0x1", "1e0", and "01" to real indices and
						// silently mis-map them (worst: "" → 0 selects the first
						// option as a fabricated answer). Anything that is not a clean
						// non-negative integer literal must fall through to the
						// skip-and-surface branch below.
						const index = /^\d+$/.test(id) ? Number(id) : Number.NaN;
						const option =
							Number.isInteger(index) &&
							index >= 0 &&
							index < question.options.length
								? question.options[index]
								: undefined;
						if (option === undefined) {
							this.#onUnmapped({
								kind: "unmapped",
								eventType: "control:ask_answer",
								reason: `ask_answer option id "${id}" is not a valid index into question "${question.id}" — skipped`,
							});
							continue;
						}
						selectedOptions.push(option.label);
					}
				}
				const item: ExtensionAskDialogResultItem = {
					id: question.id,
					question: question.question,
					options: question.options.map((option) => option.label),
					multi: question.multi ?? false,
					selectedOptions,
				};
				const customInput = answer?.customText || undefined;
				if (customInput !== undefined) item.customInput = customInput;
				return item;
			},
		);
		return { kind: "submit", results };
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
