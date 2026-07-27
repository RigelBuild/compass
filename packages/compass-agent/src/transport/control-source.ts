// The socket ControlSource: the inbound half of the agent↔Runner socket
// transport (transport-consolidation C4). It replaces the never-built stdin
// decoder with a source over `AgentGateway.Control`, the agent-opened
// server-stream the Runner pushes one `AgentControl` per message down.
//
// It is a DISPATCHER, not a bare decoder (design §Approach, "Mid-turn delivery
// is off the turn's await"): the agent's control loop is strictly sequential and
// its `prompt` arm awaits the whole turn, so anything routed through that pull
// loop queues behind the running turn. To keep a mid-turn interrupt off the
// turn's await, the source consumes the Connect stream on the Node event loop (a
// background pump) and routes by variant:
//
//   - `prompt` / `askAnswer` / `replayComplete` are REPRESENTABLE on the wire
//     today (string / id+answers / empty) — decoded to the domain union and
//     yielded on the iterable the CompassAgent pulls. They are ACKED on
//     apply-then-ack: the consumer returning for the next op is proof the
//     previous one applied (a sequential `for await` cannot pull op N+1 until op
//     N's body resolved), so the source advances its `ControlAck` cursor at the
//     start of each `next()`, never on mere receipt (P1 #6).
//   - `steer` / `deliver` are the IMMEDIATE-dispatch class (mid-turn interrupt /
//     turn-end delivery): processed on the event loop at decode, ahead of any
//     queued iterator op. C1 ships them as empty shells (OQ-1) — no representable
//     `AgentMessage` — so per Matt's OQ-2(A) ruling they are counted-unmapped
//     here WITHOUT fabricating a payload for `immediate.*`; the SEA-1310 stacked
//     PR decodes the real payload and dispatches it through the `immediate`
//     handle threaded here. Barrier-enforced (invariant 1): a pre-ReplayComplete
//     immediate op is refused-and-counted, never applied.
//   - `replay` / `config` are also empty shells in C1 (OQ-1) — no payload to seed
//     context / configure the session — so they too are counted-unmapped at
//     decode until SEA-1310 populates them (then yielded like `prompt`).
//
// Because an immediate op counted at decode is "applied" ahead of an earlier
// iterator op still queued behind a running turn (invariant 2), the highest
// contiguous `ControlAck` cursor alone cannot mark it done; the source names its
// seq in the ack's `applied_above` set, and the Runner (the durable dedup owner)
// drops it from retention so a redelivered copy is never re-applied (amended
// OQ-6).
//
// Close-reason contract (OQ-6): a clean, Runner-initiated stream end ends the
// iterable (→ CompassAgent emits STOPPED); a transport DROP (the stream throws)
// triggers a bounded reconnect — re-open `Control`, from which the Runner
// redelivers every op past the acked cursor — and does NOT end the iterable. A
// redelivered op the source already applied (or already has queued) is
// seq-deduped: counted-and-dropped, re-acked if applied so the Runner retires
// it, never re-yielded (at-least-once → exactly-once).
//
// Acks ride the SAME ordered per-session Publish spine as the FrameSink's
// trace/session frames (OQ-1, OQ-4(i)): one ordered publisher per session keeps
// the Runner's gap-detection well-defined, so both producers reach it through
// the transport's memoized `publishSpine()` and the source pushes acks on its
// never-dropped priority lane.

import { create } from "@bufbuild/protobuf";
import type { AgentMessage } from "@oh-my-pi/pi-agent-core";
import type { AgentControl, ControlSource } from "./../control";
import {
	ControlSubscribeRequestSchema,
	PublishFrameRequestSchema,
} from "./../gen/compass/v1/agent_gateway_pb";
import {
	AgentFrameSchema,
	ControlAckSchema,
	type DeliverControl,
	ReplayCompleteAckSchema,
	type SteerControl,
	type AgentControl as WireAgentControl,
} from "./../gen/compass/v1/agent_pb";
import type { UnmappedEvent } from "./../mapping";
import type { RunnerTransport } from "./index";
import type { PublishSpine } from "./publish-spine";

// Bounded-backoff reconnect schedule for the Control server-stream (ms). A
// non-clean stream end (Runner mid-restart, socket blip) re-opens `Control` on
// this fixed schedule; the Runner redelivers unacked ops from the cursor, so a
// transient drop is recovered without a terminal STOPPED. After the last delay
// the drop is treated as definitive and the iterable fails, so the source cannot
// spin forever on a dead socket. The schedule is a named constant chosen once
// here; any successful stream open resets the attempt counter.
export const CONTROL_RECONNECT_BACKOFF_MS: readonly number[] = [
	50, 200, 800, 2000,
];

// The immediate-dispatch handle: the SDK actions a mid-turn `steer` / turn-end
// `deliver` drives without waiting for the iterator's next pull. Frozen C4
// signature (design.md C4 Interfaces). Not invoked in C4b — the wire carries
// empty shells (OQ-1) so there is no `AgentMessage` to pass (OQ-2(A)); SEA-1310
// populates the payload and this handle carries the real message.
export interface ImmediateControl {
	steer(msg: AgentMessage): void;
	deliver(msg: AgentMessage): void;
}

// Decode the immediate-op payload into the SDK `AgentMessage` the `immediate`
// handle applies. `SteerControl` / `DeliverControl` are empty shells on the wire
// (OQ-1) — they carry no `AgentMessage` fields yet (SEA-1310 owns the payload
// shape) — so there is nothing to decode and the caller counts the op unmapped
// without fabricating a payload (OQ-2(A)). When SEA-1310 populates the payload
// this reads it and returns the message to dispatch.
function decodeImmediatePayload(
	_shell: SteerControl | DeliverControl,
): AgentMessage | undefined {
	return undefined;
}

// One op waiting for the consumer to pull, tagged with its Runner-assigned seq so
// the source can ack it (apply-then-ack) and dedup a redelivery of it.
interface Queued {
	readonly op: AgentControl;
	readonly seq: bigint;
}

// A single-consumer async queue: the pump `push`es decoded representable ops, the
// iterable `pull`s them. `close()` ends it cleanly (→ STOPPED); `fail()` ends it
// with the drop error once the reconnect budget is spent.
class AsyncBuffer {
	#items: Queued[] = [];
	#closed = false;
	#error: unknown;
	#waiting:
		| {
				resolve: (r: IteratorResult<Queued>) => void;
				reject: (e: unknown) => void;
		  }
		| undefined;

	push(item: Queued): void {
		if (this.#closed) return;
		const w = this.#waiting;
		if (w !== undefined) {
			this.#waiting = undefined;
			w.resolve({ value: item, done: false });
		} else {
			this.#items.push(item);
		}
	}

	close(): void {
		if (this.#closed) return;
		this.#closed = true;
		const w = this.#waiting;
		if (w !== undefined) {
			this.#waiting = undefined;
			w.resolve({ value: undefined, done: true });
		}
	}

	fail(err: unknown): void {
		if (this.#closed) return;
		this.#closed = true;
		this.#error = err;
		const w = this.#waiting;
		if (w !== undefined) {
			this.#waiting = undefined;
			w.reject(err);
		}
	}

	pull(): Promise<IteratorResult<Queued>> {
		const next = this.#items.shift();
		if (next !== undefined)
			return Promise.resolve({ value: next, done: false });
		if (this.#error !== undefined) return Promise.reject(this.#error);
		if (this.#closed) return Promise.resolve({ value: undefined, done: true });
		return new Promise((resolve, reject) => {
			this.#waiting = { resolve, reject };
		});
	}
}

// The selective apply-ack cursor: the highest CONTIGUOUS applied `control_seq`
// plus a bounded set of seqs applied out of order above it (`applied_above`,
// invariant 2). `markApplied` is idempotent — a redelivered already-applied op
// re-acks (so the Runner retires it) without corrupting the cursor. Every apply
// emits a `ControlAck` on the shared Publish spine's priority lane; the Runner
// retires retained ops up to the cursor and drops the individually-acked ones.
class AckCursor {
	readonly #spine: PublishSpine;
	#cursor = 0n;
	readonly #above = new Set<bigint>();

	constructor(spine: PublishSpine) {
		this.#spine = spine;
	}

	// True once the seq is durably applied from the source's view: at or below the
	// contiguous cursor (retired), or individually applied above it. Drives dedup
	// of a redelivered op.
	isApplied(seq: bigint): boolean {
		return seq <= this.#cursor || this.#above.has(seq);
	}

	// Record an op applied and emit the resulting ControlAck. Advances the
	// contiguous cursor as far as the applied set allows, pruning subsumed
	// out-of-order entries. Idempotent for an already-applied seq (re-ack only).
	markApplied(seq: bigint): void {
		if (seq > this.#cursor) {
			this.#above.add(seq);
			while (this.#above.delete(this.#cursor + 1n)) this.#cursor += 1n;
		}
		this.#spine.enqueuePriority(
			create(PublishFrameRequestSchema, {
				frame: create(AgentFrameSchema, {
					frame: {
						case: "controlAck",
						value: create(ControlAckSchema, {
							ackedSeq: this.#cursor,
							appliedAbove: [...this.#above],
						}),
					},
				}),
			}),
		);
	}

	// Emit the replay-barrier ack: on receipt the Runner releases the live ops it
	// held behind the restart replay barrier. Rides the same priority lane, ahead
	// of the ControlAck for the replayComplete op that triggers it.
	emitReplayCompleteAck(): void {
		this.#spine.enqueuePriority(
			create(PublishFrameRequestSchema, {
				frame: create(AgentFrameSchema, {
					frame: {
						case: "replayCompleteAck",
						value: create(ReplayCompleteAckSchema, {}),
					},
				}),
			}),
		);
	}
}

/**
 * A ControlSource over `AgentGateway.Control` (frozen C4 signature). Opens the
 * agent's control subscription, dispatches each pushed `AgentControl` by variant
 * (representable → yielded; immediate/empty-shell → counted-unmapped at decode),
 * emits apply-then-ack `ControlAck`/`ReplayCompleteAck` on the shared Publish
 * spine, and reconnects (bounded) on a transport drop while ending cleanly on a
 * Runner-initiated close.
 *
 * @param transport the Runner socket handle (its `control()` server-stream +
 *   `publishSpine()` ack lane)
 * @param immediate the SDK steer/deliver actions the immediate path drives —
 *   threaded per the frozen signature; not invoked while the wire carries empty
 *   shells (OQ-2(A)), SEA-1310 populates the payload
 * @param onUnmapped surfaces a decoded op the source could not apply (empty
 *   shell, barrier refusal, duplicate redelivery) — logged + counted, never
 *   silently dropped, mirroring `CompassAgent`'s channel. Defaults to console.
 */
export function createSocketControlSource(
	transport: RunnerTransport,
	immediate: ImmediateControl,
	onUnmapped: (u: UnmappedEvent) => void = (u) =>
		console.error(
			`[compass-agent] control unmapped: ${u.eventType} — ${u.reason}`,
		),
): ControlSource {
	const spine = transport.publishSpine();
	const acks = new AckCursor(spine);
	const buffer = new AsyncBuffer();
	// Seqs decoded to a representable op and queued but not yet applied. Dedups a
	// redelivery of an op the source already holds (reconnect/takeover) against
	// re-queueing it; cleared as each is applied on pull.
	const queued = new Set<bigint>();
	// The source's own view of the replay barrier, set when replayComplete is
	// decoded. The immediate path (which never reaches CompassAgent's barrier)
	// enforces it locally (invariant 1) — a belt-and-suspenders on the Runner's
	// hold and CompassAgent's iterator-side barrier.
	let replayComplete = false;

	function count(eventType: string, reason: string): void {
		onUnmapped({ kind: "unmapped", eventType, reason });
	}

	// Route one wire AgentControl. Immediate/empty-shell ops are applied (counted)
	// and acked here at decode; representable ops are queued for the iterable and
	// acked on apply-then-ack when the consumer pulls past them.
	function dispatch(wire: WireAgentControl): void {
		const seq = wire.controlSeq;
		const kind = wire.control.case ?? "unknown";
		// Dedup a redelivery (reconnect/takeover): an already-applied op is re-acked
		// so the Runner retires it, an already-queued op is dropped — neither is
		// re-applied or re-yielded (at-least-once → exactly-once, amended OQ-6).
		if (acks.isApplied(seq)) {
			count(`control:${kind}`, "duplicate redelivered op — already applied");
			acks.markApplied(seq);
			return;
		}
		if (queued.has(seq)) {
			count(`control:${kind}`, "duplicate redelivered op — already queued");
			return;
		}

		switch (wire.control.case) {
			case "prompt": {
				queued.add(seq);
				buffer.push({
					op: { kind: "prompt", input: wire.control.value.input },
					seq,
				});
				return;
			}
			case "askAnswer": {
				const v = wire.control.value;
				queued.add(seq);
				buffer.push({
					op: { kind: "askAnswer", askId: v.askId, answers: v.answers },
					seq,
				});
				return;
			}
			case "replayComplete": {
				replayComplete = true;
				queued.add(seq);
				buffer.push({ op: { kind: "replayComplete" }, seq });
				return;
			}
			case "steer":
			case "deliver": {
				// Immediate-dispatch class. Barrier-enforced (invariant 1): a live
				// immediate op before ReplayComplete is refused-and-counted. Otherwise
				// the empty-shell payload (OQ-1) yields no AgentMessage, so per OQ-2(A)
				// it is counted-unmapped without fabricating a payload for immediate.*;
				// SEA-1310 populates the payload and dispatches through `immediate`.
				const msg = replayComplete
					? decodeImmediatePayload(wire.control.value)
					: undefined;
				if (!replayComplete) {
					count(
						`control:${kind}`,
						"live immediate op before ReplayComplete — refused by replay barrier",
					);
				} else if (msg === undefined) {
					count(
						`control:${kind}`,
						"empty-shell steer/deliver — payload staged (SEA-1310)",
					);
				} else if (wire.control.case === "steer") {
					immediate.steer(msg);
				} else {
					immediate.deliver(msg);
				}
				// Applied (counted or dispatched) at decode → ack now, ahead of any
				// queued iterator op (invariant 2 → applied_above).
				acks.markApplied(seq);
				return;
			}
			case "replay":
			case "config": {
				// Empty shells in C1 (OQ-1): no payload to seed context / configure the
				// session, so counted-unmapped at decode. SEA-1310 populates them and
				// they flow through the iterable like prompt.
				count(
					`control:${kind}`,
					"empty-shell replay/config — payload staged (SEA-1310)",
				);
				acks.markApplied(seq);
				return;
			}
			default:
				// Unset/unknown oneof: an unrecognized control op, logged + counted,
				// never a crash (symmetric with the mapper's unmapped arm). Acked so the
				// Runner does not redeliver an op the source will never apply.
				count(`control:${kind}`, "unrecognized control variant");
				acks.markApplied(seq);
				return;
		}
	}

	// Consume the Control stream on the event loop, reconnecting on a drop. A
	// clean, Runner-initiated stream end closes the buffer (→ STOPPED); a
	// non-clean end (thrown) re-opens the subscription on the bounded backoff,
	// from which the Runner redelivers unacked ops. Runs detached; its terminal
	// state reaches the consumer through the buffer, never as an unhandled
	// rejection.
	async function pump(): Promise<void> {
		let attempt = 0;
		for (;;) {
			try {
				const stream = transport.control(
					create(ControlSubscribeRequestSchema, {}),
				);
				for await (const wire of stream) {
					attempt = 0;
					dispatch(wire);
				}
				buffer.close();
				return;
			} catch (err) {
				if (attempt >= CONTROL_RECONNECT_BACKOFF_MS.length) {
					buffer.fail(err);
					return;
				}
				const delay = CONTROL_RECONNECT_BACKOFF_MS[attempt++];
				await new Promise((r) => setTimeout(r, delay));
			}
		}
	}

	// The pump starts on first iteration and runs once for the source's life. The
	// ControlSource is single-consumer by contract (CompassAgent's one control
	// loop, agent.ts) — guard against a second `for await` spawning a duplicate
	// pump on the shared buffer/spine.
	let pumping = false;
	return {
		[Symbol.asyncIterator](): AsyncIterator<AgentControl> {
			if (!pumping) {
				pumping = true;
				void pump();
			}
			// The op yielded on the previous pull, awaiting the apply-then-ack the
			// consumer's return for the next op proves.
			let lastYielded: Queued | undefined;
			return {
				async next(): Promise<IteratorResult<AgentControl>> {
					// Apply-then-ack: the consumer is back for the next op, so the previous
					// one's application resolved. Ack it BEFORE awaiting the next pull, so
					// a ReplayCompleteAck reaches the Runner (releasing held live ops) even
					// when the next op is one the Runner holds behind that very barrier —
					// no deadlock.
					if (lastYielded !== undefined) {
						const applied = lastYielded;
						lastYielded = undefined;
						queued.delete(applied.seq);
						if (applied.op.kind === "replayComplete")
							acks.emitReplayCompleteAck();
						acks.markApplied(applied.seq);
					}
					const r = await buffer.pull();
					if (r.done) return { value: undefined, done: true };
					lastYielded = r.value;
					return { value: r.value.op, done: false };
				},
			};
		},
	};
}
