// CompassAgent — the first-party in-container agent (design §T5).
// Maps session events to compass.v1 `AgentFrame`s (EventMapper → FrameSink) and
// drives the session from decoded `AgentControl` ops (ControlSource), gated by the
// replay barrier. Drives the inner `session.agent` to keep the control contract.

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
	// The session to drive, constructed by the caller (container entrypoint) via
	// `createAgentSession`, so this class stays IO-focused. Required: there is no
	// no-arg `AgentSession` constructor.
	readonly session: AgentSession;
	// Outbound frame sink (stdout). The wire envelope lives behind it.
	readonly sink: FrameSink;
	// Inbound control source (stdin). Yields decoded AgentControl frames.
	readonly control: ControlSource;
	// Sink for frames the mapper could not map — logged + counted, never
	// dropped. Defaults to a no-op counter-less logger via console.
	readonly onUnmapped?: (u: UnmappedEvent) => void;
	// Optional trace-continuity tracer (design record §T2). The narrow,
	// OTel-type-free `TurnTracer` facet only, so this exported option stays
	// fence-clean. `undefined` (telemetry off) ⇒ every trace call no-ops via
	// optional chaining, so frames stay bit-identical.
	readonly tracer?: TurnTracer;
}

export class CompassAgent {
	readonly #session: AgentSession;
	readonly #sink: FrameSink;
	readonly #control: ControlSource;
	readonly #mapper: EventMapper;
	readonly #onUnmapped: (u: UnmappedEvent) => void;
	readonly #tracer: TurnTracer | undefined;
	// The native tools the session was constructed with — the set no control frame
	// may drop or substitute (see #withNatives). A COPY: `agent.state` hands back
	// the live, caller-owned `state.tools` array by reference, so an in-place
	// mutation would otherwise revoke natives out from under us.
	readonly #natives: AgentTool[];
	// Runner holds live prompt/steer until the agent acks ReplayComplete, but the
	// agent also guards locally: frames before replay settles apply as context, and
	// live prompt/steer are refused until `#replayComplete` — belt-and-suspenders.
	#replayComplete = false;
	// RIG-1310 §8 — RT-3 turn-end delivery (DELIVER arm). Mid-turn delivers queue
	// and flush as ONE prompt when the turn settles; an idle deliver starts a turn.
	// `#turnActive` tracks the turn-start→agent_end window off the session stream;
	// the idle-deliver trigger also gates on `#session.isStreaming` (see `deliver`).
	#turnActive = false;
	// The coalescing queue: messages delivered mid-turn, drained into one prompt
	// at the next flush.
	#deliverQueue: Message[] = [];
	// RIG-2486 T1 — denormalized author `from_handle` per queued deliver, keyed on
	// `Message.id`. A deliver injected at turn-end flush must stash its from_handle
	// (resolved server-side, carried on the wire control) to travel with the
	// message to `#emitInjection`. Set at enqueue, read + deleted per message at flush.
	readonly #deliverFromHandles = new Map<string, string>();
	// Trace-continuity (design record §T2): the W3C `traceparent` per queued
	// deliver, keyed on `Message.id`, mirroring `#deliverFromHandles`. Stashed so it
	// travels to the flush, where it becomes the turn span's parent (N=1) or link (N>1).
	readonly #deliverTraceparents = new Map<string, string>();
	// Peer-DM record DL-292 (T0 step 2): denormalized source channel + topic NAMES
	// per queued deliver, keyed on `Message.id`. Travel to `formatDeliversForPrompt`
	// at flush; either name is EMPTY on a server-side resolve miss (falls back to id).
	readonly #deliverSourceNames = new Map<string, DeliverSourceNames>();
	// Trace-continuity (design record §T2): ids of every channel message that fed the
	// CURRENT turn span, in arrival order — source for the `compass.message.ids` stamp.
	// T1 OVERWRITES the attribute, so every stamp must pass the full accumulated list.
	// Reset at each turn-start (not `agent_end`, so a rejected turn-start stays clean).
	readonly #turnMessageIds: string[] = [];
	// Session-lifetime dedup keyed on `Message.id`. A sweep redelivery under a fresh
	// control_seq is dropped here so a message is injected at most once (record :811-812).
	readonly #processedMessageIds = new Set<string>();
	// RIG-2732 W3 — turn-end forge-notification queue, the RT-3 sibling of
	// `#deliverQueue`. Each entry pairs the notification with the control-source's
	// `ackRail` thunk, called AFTER emitting the ForgeNotificationAck (the forge arm
	// defers BOTH acks to flush, unlike steer/deliver).
	#forgeQueue: { notification: ForgeNotification; ackRail: () => void }[] = [];
	// RIG-2644 — strand-recovery latch. A deliver that queues while streaming with no
	// TRACKED turn active gets no `agent_end` edge to flush it (a startup probe holds
	// `isStreaming` with no turn edge), so it would strand. This arms one
	// `waitForIdle`-gated re-check that flushes once the untracked stream settles.
	#strandRecoveryArmed = false;
	// RIG-2644 — set true in run()'s finally, on the terminal-status edge. A
	// strand-recovery `waitForIdle` can resolve AFTER termination; the re-check
	// no-ops when closed, so it never starts a turn past the terminal frame.
	#closed = false;

	constructor(opts: CompassAgentOptions) {
		this.#session = opts.session;
		// Tolerate a session exposing no `state`/`state.tools`: degrading to no natives
		// beats a constructor that throws on a shape it merely inspects.
		this.#natives = [...(opts.session.agent?.state?.tools ?? [])];
		this.#sink = opts.sink;
		this.#control = opts.control;
		this.#tracer = opts.tracer;
		this.#mapper = new EventMapper();
		this.#onUnmapped =
			opts.onUnmapped ??
			((u) =>
				// biome-ignore lint/suspicious/noConsole: default unmapped-event sink surfaces protocol drift to the operator
				console.error(
					`[compass-agent] unmapped: ${u.eventType} — ${u.reason}`,
				));
	}

	// Wire the session event stream to the frame sink, then consume control frames
	// until stdin closes. Emits a terminal status — STOPPED on a clean close,
	// ERRORED on an exception — then resolves (clean) or re-throws (error).
	async run(): Promise<void> {
		const unsubscribe = this.#session.subscribe((event) => {
			// Turn-tracking (RIG-1310 §8): an ADDITIONAL read of the same event,
			// beside the mapper fan-out. A turn-start edge marks the session active;
			// `agent_end` settles it and flushes coalesced delivers into one prompt.
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
			// An unexpected exit (control decode / SDK op / stream error) → ERRORED,
			// distinct from a clean STOPPED (compass.proto:141). Emit it, then
			// re-propagate — a swallowed crash would be a silent failure.
			this.#emitStatus(AgentSessionState.ERRORED);
			throw err;
		} finally {
			// Terminal edge: no strand-recovery re-check may start a turn past here.
			this.#closed = true;
			unsubscribe();
		}
	}

	// Emit a board lifecycle transition as a session frame carrying only the state
	// (no trace event). The Runner extracts the state into an AgentSessionStatus
	// server-side, stamping the session_id it owns.
	#emitStatus(state: AgentSessionState): void {
		const value: SessionFrame = create(SessionFrameSchema, { state });
		this.#sink.emit({ kind: "session", value });
	}

	// Emit the SessionInjection observation (T1): a trace frame recording a channel
	// message injected as a steer or deliver. Emitted BESIDE the delivery ack at
	// injection time on the same FrameSink (idle-safe), on the never-drop lane.
	// `from_handle` is the author's handle, resolved server-side (RIG-2486 T1).
	#emitInjection(
		opKind: SessionInjectionKind,
		messageId: string,
		fromHandle: string,
		traceparent: string,
	): void {
		this.#sink.emit(
			this.#mapper.sessionInjection(opKind, messageId, fromHandle, traceparent),
		);
	}

	// RIG-1310 §8 — deliver a channel message into the live session (RT-3), called
	// when a `DeliverControl.message` decodes. The replay barrier is enforced
	// UPSTREAM at the control source, so this method does not re-check it.
	deliver(
		msg: Message,
		fromHandle = "",
		traceparent = "",
		sourceNames: DeliverSourceNames = { channelName: "", topicName: "" },
	): void {
		// A message with no id cannot be acked or deduped — fail-visible, never a
		// silent drop (and never injected, since there would be no receipt).
		if (msg.id === "") {
			this.#onUnmapped({
				kind: "unmapped",
				eventType: "deliver",
				reason: "deliver missing Message.id — cannot ack or dedup",
			});
			return;
		}
		// Sweep-redelivery safety: a processed message is dropped (counted), independent
		// of the control_seq dedup (record :811-812). Re-ack subtlety: the set holds ids
		// from ENQUEUE time, so on a duplicate we RE-EMIT the ack ONLY when the id is
		// already injected (not still queued) — "ack means injected".
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
		this.#deliverSourceNames.set(msg.id, sourceNames);
		// Idle deliver starts a turn immediately (record :799/:810); a mid-turn deliver
		// waits for the `agent_end` flush. "Idle" consults BOTH `#turnActive` AND
		// `#session.isStreaming` — a control prompt sets streaming SYNCHRONOUSLY but flips
		// `#turnActive` later, so gating on `isStreaming` avoids an AgentBusyError inject.
		if (!this.#turnActive && !this.#session.isStreaming) {
			this.#flushTurnEnd();
		} else if (!this.#turnActive) {
			// Queued because streaming with NO tracked turn — the strand shape (RIG-2644):
			// an untracked in-flight holds `isStreaming` and no `agent_end` will arrive.
			// A live TRACKED turn is not this case — its `agent_end` flushes normally.
			this.#armStrandRecovery();
		}
	}

	// RIG-2644 — flush the deliver queue once an UNTRACKED stream (a startup probe
	// holding `#session.isStreaming` with no turn edge) settles. Idempotent via the
	// `#strandRecoveryArmed` latch. On resolve: no-op if closed / a tracked turn started
	// / the queue drained; flush if still-idle; re-arm if a fresh probe is still streaming.
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

	// RIG-1310 §8 — channel-borne steer arm, called when a `SteerControl.message`
	// decodes. Unlike deliver, a steer is an @-mention interrupt: mid-turn it injects
	// into the running loop; idle it starts a fresh turn (record :788-814).
	// Shares deliver's `#processedMessageIds` dedup; the ack means "injected".
	steer(
		msg: Message,
		fromHandle = "",
		traceparent = "",
		sourceNames: DeliverSourceNames = { channelName: "", topicName: "" },
	): void {
		// A message with no id cannot be acked or deduped — fail-visible, never a
		// silent drop. Mirrors deliver's empty-id guard.
		if (msg.id === "") {
			this.#onUnmapped({
				kind: "unmapped",
				eventType: "steer",
				reason: "steer missing Message.id — cannot ack or dedup",
			});
			return;
		}
		// Sweep-redelivery safety: a processed message is not re-injected. The set is
		// SHARED with deliver, so the re-ack carries deliver's SAME queue-membership
		// guard — a processed id still queued was only QUEUED, not injected. Only an id
		// NOT queued is re-acked. The "duplicate steer" unmapped surface stays UNCONDITIONAL.
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
		// The mention text, formatted once via the single deliver formatter. Used as
		// the mid-turn steering message's content AND as the idle turn-start prompt.
		// The source names ride the steer op, plumbed the same per-message way deliver does.
		const content = formatDeliversForPrompt(
			[msg],
			new Map([[msg.id, sourceNames]]),
		);
		// Idle is computed exactly as deliver does: the event-derived `#turnActive` AND
		// the authoritative `#session.isStreaming` (closes the spin-up race).
		const idle = !this.#turnActive && !this.#session.isStreaming;
		if (!idle) {
			// Mid-turn: ENQUEUE onto the steering queue so the running loop drains it at
			// its next injection boundary — interrupt in place, no new turn. The
			// `steering` flag is what the agent's pre-LLM transform reads; timestamp is a
			// fixed 0. `agent.steer` is synchronous void, so the ack rides the next microtask.
			const agentMsg: AgentMessage = {
				role: "user",
				content,
				steering: true,
				attribution: "user",
				timestamp: 0,
			};
			this.#session.agent.steer(agentMsg);
			// Case-3 LINK (design record §T2): the `invoke_agent` span already exists and
			// is parented; a mid-turn steer adds a link to the live span, then stamps the
			// message id onto the topology-independent query key. No-op without a tracer.
			this.#tracer?.linkActiveTurn(traceparent, msg.id);
			this.#turnMessageIds.push(msg.id);
			this.#tracer?.stampActiveTurn(this.#turnMessageIds.join(","));
			queueMicrotask(() => {
				const value: DeliveryAck = create(DeliveryAckSchema, {
					messageId: msg.id,
				});
				this.#sink.emit({ kind: "deliveryAck", value });
				this.#emitInjection(
					SessionInjectionKind.STEER,
					msg.id,
					fromHandle,
					traceparent,
				);
			});
			return;
		}
		// Idle: START A NEW TURN with the mention as content via `prompt()`. `prompt()`
		// runs on ANY history including a fresh peer's EMPTY history, whereas `continue()`
		// rejects on a zero-history session (RIG-2488). No pre-enqueue onto the steering
		// queue, else the initial-content prompt would double-inject.

		// Rejection-safety belt (mirrors `#flushTurnEnd`): `prompt` can only refuse as a
		// settled REJECTION before any injection; on rejection un-dedup the id, no ack.
		// Optimistically mark a turn active to close the spin-up window (before
		// `agent_start` propagates, a follow-on would re-gate idle and start a 2nd turn).
		this.#turnActive = true;
		let rejected = false;
		let acked = false;
		// Case-1 PARENT (design record §T2): the remote context becomes the new turn's
		// `invoke_agent` parent. `runWithParent` wraps the SYNCHRONOUS `prompt()` so
		// parentage rides `context.active()`; without a tracer it runs directly. This idle
		// steer is a TRUE 1:1 parent, so set the turn trigger for outbound-post linkage.
		this.#tracer?.setTurnTrigger(traceparent);
		const started =
			this.#tracer === undefined
				? this.#session.agent.prompt(content)
				: this.#tracer.runWithParent(traceparent, () =>
						this.#session.agent.prompt(content),
					);
		// Stamp the topology-independent query key on the captured span. This message
		// STARTS the turn, so it RESETS the accumulator to its own id (a later mid-turn
		// steer appends + re-stamps). Reset here keeps it self-contained on a rejected start.
		this.#turnMessageIds.length = 0;
		this.#turnMessageIds.push(msg.id);
		this.#tracer?.stampActiveTurn(this.#turnMessageIds.join(","));
		started.catch((err) => {
			if (acked) return;
			rejected = true;
			this.#turnActive = false;
			// Re-attach (RIG-2894): a refused prompt starts no turn, so clear the trigger
			// set above — else it leaks onto the NEXT turn's posts.
			this.#tracer?.clearTurnTrigger();
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
			this.#emitInjection(
				SessionInjectionKind.STEER,
				msg.id,
				fromHandle,
				traceparent,
			);
		});
	}

	// RIG-2732 W3 — turn-end forge-notification arm (past the replay barrier). The RT-3
	// sibling of `deliver`. UNLIKE deliver, BOTH acks defer to the FLUSH (the control-rail
	// `ackRail` AND the ForgeNotificationAck frame) — a decode-ack would discard the
	// Runner's retain-until-acked durability. Redelivery dedup rides the control-source seq.
	forgeNotification(
		notification: ForgeNotification,
		ackRail: () => void,
	): void {
		this.#forgeQueue.push({ notification, ackRail });
		// Idle notification starts a turn immediately; a mid-turn one waits for the
		// `agent_end` flush. "Idle" consults BOTH `#turnActive` AND `#session.isStreaming`,
		// exactly as deliver does.
		if (!this.#turnActive && !this.#session.isStreaming) {
			this.#flushTurnEnd();
		} else if (!this.#turnActive) {
			// Queued while streaming with NO tracked turn — the strand shape (RIG-2644):
			// an untracked in-flight holds `isStreaming` and no `agent_end` will arrive.
			// Arm the shared recovery that flushes once the stream settles.
			this.#armStrandRecovery();
		}
	}

	// Track a session turn edge (RIG-1310 §8, RIG-2732 W3). A turn-start edge marks
	// the session active; `agent_end` settles it and flushes the coalesced deliver AND
	// forge-notification queues. See the `#turnActive` field comment for why the flush
	// is safe synchronously on the edge.
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

	// RIG-2732 W3 — flush BOTH coalesced turn-end queues (DELIVER + FORGE) as EXACTLY ONE
	// prompt, then emit ALL acks in one microtask. One prompt is load-bearing: `prompt`
	// sets `isStreaming` SYNCHRONOUSLY, so two calls on one `agent_end` edge collide. The
	// input concatenates the two renderers (delivers, then forge) with "\n\n".

	// The ack means "injected", not "turn finished" (record :800), so a mid-turn crash
	// keeps the receipt. A settled REJECTION means NEITHER batch injected (a real mid-turn
	// failure resolves instead) — fail closed for both: un-dedup delivers for redelivery;
	// for forge do NOT re-enqueue or fire ackRail (the Runner is the redelivery authority).
	#flushTurnEnd(): void {
		const delivers = this.#deliverQueue;
		const forges = this.#forgeQueue;
		this.#deliverQueue = [];
		this.#forgeQueue = [];
		if (delivers.length === 0 && forges.length === 0) return;
		const sections: string[] = [];
		if (delivers.length > 0)
			sections.push(
				formatDeliversForPrompt(delivers, this.#deliverSourceNames),
			);
		if (forges.length > 0)
			sections.push(
				formatForgeNotifications(forges.map((e) => e.notification)),
			);
		const input = sections.join("\n\n");
		// Optimistically mark a turn active — an idle flush starts one; the rejection
		// path below clears it, since a refused prompt starts no turn.
		this.#turnActive = true;
		let rejected = false;
		let acked = false;
		// Reset the per-turn message-id accumulator at this turn-start (design record §T2):
		// a flush STARTS a new turn, so the accumulator carries only THIS turn's messages.
		// Deliver ids append in the ack microtask; a later mid-turn steer appends too.
		// Reset here (not `agent_end`) keeps it self-contained on a rejected turn-start.
		this.#turnMessageIds.length = 0;
		// Trace-continuity topology (design record §T2): a single-message deliver batch
		// PARENTS the turn on that traceparent (N=1); a multi-message batch runs bare and
		// LINKS each context (N>1); a forge-only flush runs bare. SET the trigger only for
		// the N=1 true 1:1 parent; every other shape CLEARS it (else a prior trigger leaks).
		if (delivers.length === 1) {
			this.#tracer?.setTurnTrigger(
				this.#deliverTraceparents.get(delivers[0].id) ?? "",
			);
		} else {
			this.#tracer?.clearTurnTrigger();
		}
		const startPrompt = (): Promise<void> => this.#session.agent.prompt(input);
		const started =
			this.#tracer !== undefined && delivers.length === 1
				? this.#tracer.runWithParent(
						this.#deliverTraceparents.get(delivers[0].id) ?? "",
						startPrompt,
					)
				: startPrompt();
		started.catch((err) => {
			// Reached ONLY on a settled-rejected prompt, i.e. neither batch injected. If
			// the acks already went out this is the SDK-impossible post-injection rejection
			// — leave the injected batches alone.
			if (acked) return;
			rejected = true;
			this.#turnActive = false;
			// Re-attach (RIG-2894): a refused prompt starts no turn, so clear any trigger
			// the N=1 branch set above — else it leaks onto the next turn.
			this.#tracer?.clearTurnTrigger();
			// DELIVER: un-dedup every id so the Server redelivers + re-injects, and drop
			// the stashed from-handle + traceparent (the redelivery re-stashes).
			for (const msg of delivers) {
				this.#processedMessageIds.delete(msg.id);
				this.#deliverFromHandles.delete(msg.id);
				this.#deliverTraceparents.delete(msg.id);
				this.#deliverSourceNames.delete(msg.id);
			}
			if (delivers.length > 0) {
				this.#onUnmapped({
					kind: "unmapped",
					eventType: "deliver:prompt",
					reason: `deliver flush prompt rejected — batch not injected, un-acked for redelivery: ${String(err)}`,
				});
			}
			// FORGE: no ackRail fired, so the Runner still retains every op past its rail
			// cursor and redelivers on reconnect. Do NOT re-enqueue.
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
			// Trace continuity (design record §T2): the captured turn span is live now.
			// A multi-message batch LINKS each message's stashed context onto it; every
			// non-empty deliver batch stamps the comma-joined ids as the query key.
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
				const traceparent = this.#deliverTraceparents.get(msg.id) ?? "";
				this.#deliverFromHandles.delete(msg.id);
				this.#deliverTraceparents.delete(msg.id);
				this.#deliverSourceNames.delete(msg.id);
				this.#emitInjection(
					SessionInjectionKind.DELIVER,
					msg.id,
					fromHandle,
					traceparent,
				);
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

	// Apply one decoded control frame, discriminated on the AgentControl oneof:
	// replay applies to context; replay_complete lifts the barrier; prompt/steer/config
	// drive the session once replay settled. Drives the inner `Agent` to preserve the
	// control contract (see the file header).
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
				// Live input: the Runner holds these until ReplayComplete; the local
				// barrier is a backstop. A control that slips through early is surfaced.
				if (!this.#replayComplete) {
					this.#onUnmapped({
						kind: "unmapped",
						eventType: "control:prompt",
						reason:
							"live prompt arrived before ReplayComplete — refused by replay barrier",
					});
					return;
				}
				// A control prompt STARTS a fresh turn, so reset the accumulator like every
				// other turn-start site — else a prior deliver-flush's ids would leak into
				// this turn's query key via a later mid-turn steer. No-op without a tracer.
				this.#turnMessageIds.length = 0;
				// Re-attach (RIG-2894): a control prompt STARTS a fresh turn with NO single
				// channel-message parent (a raw wire-driven injection). Clear the trigger
				// like every other non-1:1 turn-start, else a prior trigger leaks.
				this.#tracer?.clearTurnTrigger();
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

	// Merge the control's tool list with the construction-time natives, keyed by name:
	// a native ALWAYS wins. A control may add and reorder but can neither drop a native
	// nor substitute its instance (comms is not grantable; authorization is a server-side
	// SQL decision). A rejected substitution is surfaced through #onUnmapped, never applied.
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
			// Same name, different instance: an attempted substitution of a native. Keep
			// the native at the control's position and surface the rejected misconfig.
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

/**
 * The denormalized source NAMES for a delivered channel message (peer-DM record
 * DL-292), carried on the wire `DeliverControl.channel_name`/`topic_name` and
 * plumbed per message to `formatDeliversForPrompt` so the render names the
 * source channel + topic and the reply cue names both required post params.
 * Either is EMPTY on a server-side resolve miss (a name miss never blocks a
 * delivery); the renderer falls back per label.
 */
export interface DeliverSourceNames {
	readonly channelName: string;
	readonly topicName: string;
}

// Coalesce a batch of delivered channel messages into ONE prompt input string
// (RIG-1310 §8). Pure + exported. GROUPED per topic, FORMAT-TIME only. Each delivery
// renders under a `Channel <name> › topic <name>:` header (RIG-2664 / DL-292) ending in
// ONE terse reply cue; source names ride the deliver op, EMPTY names fall back.
export function formatDeliversForPrompt(
	batch: readonly Message[],
	sources: ReadonlyMap<string, DeliverSourceNames> = new Map(),
): string {
	// An empty batch has nothing to reply to — return "" rather than a bare cue (the
	// exported pure fn's contract is pinned here regardless of caller guards).
	if (batch.length === 0) return "";
	// Group by the rendered (channel, topic) header, first-seen order. The map value
	// carries the label so the header is computed once per group.
	const groups = new Map<string, { header: string; texts: string[] }>();
	for (const msg of batch) {
		const names = sources.get(msg.id);
		const channelLabel = names?.channelName || "(unknown channel)";
		const topicLabel = names?.topicName || msg.topicId;
		const key = `${channelLabel}\u0000${topicLabel}`;
		const header = `Channel ${channelLabel} › topic ${topicLabel}:`;
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
		const existing = groups.get(key);
		if (existing) existing.texts.push(text);
		else groups.set(key, { header, texts: [text] });
	}
	const sections = Array.from(
		groups.values(),
		({ header, texts }) => `${header}\n${texts.join("\n\n")}`,
	);
	sections.push(
		"Reply via comms_post_message, naming the channel and topic above.",
	);
	return sections.join("\n\n");
}

// Render a delivered `ask_answer` message's answered `Ask` snapshot into a prompt
// section (RIG-2257 answer lane). Pure + exported. ONE section per question (text, chosen
// LABELS, free-text). Every value passes through `flat` (the render guard); an unresolved
// chosen id renders DEFENSIVELY by its id rather than dropped.
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

// Coalesce a batch of forge notifications into ONE prompt input string (RIG-2732 W3).
// Pure + exported. ONE section per notification: a header `<forge> <repo>#<n> — <kind>`
// then per-kind payload (COMMENT/STATE/CHECKS/REVIEW/OPENED; UPDATE header only). Every
// value passes through `flat`; the batch ends with ONE terse re-read cue.
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
