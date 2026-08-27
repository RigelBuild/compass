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
//   - `prompt` / `replayComplete` are REPRESENTABLE on the wire today (string /
//     empty) — decoded to the domain union and yielded on the iterable the
//     CompassAgent pulls. They are ACKED on apply-then-ack: the consumer
//     returning for the next op is proof the previous one applied (a sequential
//     `for await` cannot pull op N+1 until op N's body resolved), so the source
//     advances its `ControlAck` cursor at the start of each `next()`, never on
//     mere receipt (P1 #6).
//   - `steer` / `deliver` are the IMMEDIATE-dispatch class (mid-turn interrupt /
//     turn-end delivery): processed on the event loop at decode, ahead of any
//     queued iterator op. As of SEA-1310 §8 BOTH carry the comms Message on the
//     wire (`SteerControl.message` / `DeliverControl.message`, the latter added
//     by SEA-1569 (T7)) — decoded here and dispatched through `immediate.steer` /
//     `immediate.deliver`, where the CompassAgent dedups on `msg.id`, injects
//     (steer as a mid-turn interrupt / idle-start turn; deliver coalesced to a
//     turn-end prompt), and acks per message. An empty SteerControl (no `message`
//     field) still decodes to undefined and is counted-unmapped without
//     fabricating a payload (OQ-2(A)).
//     Barrier-enforced (invariant 1): a pre-ReplayComplete immediate op is
//     refused-and-counted, never applied.
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
import { Effect, Either, Fiber, Logger, ManagedRuntime, Metric } from "effect";
import type { ForgeNotification, Message } from "./../compassv1";
import type { AgentControl, ControlSource } from "./../control";
import { ControlSubscribeRequestSchema } from "./../gen/compass/v1/agent_gateway_pb";
import type {
	DeliverControl,
	SteerControl,
	AgentControl as WireAgentControl,
} from "./../gen/compass/v1/agent_pb";
import type { UnmappedEvent } from "./../mapping";
// The pull queue and the ack cursor live in `./control/` — see those files for
// the uncapped-queue policy and the contiguous-cursor invariant.
import { AckCursor } from "./control/ack-cursor";
import { AsyncBuffer, type Queued } from "./control/buffer";
import type { RunnerTransport } from "./index";
import {
	controlUnmapped,
	flapResets,
	noProgressDepth,
	reconnects,
} from "./otel-metrics";
import { getTransportRuntime } from "./runtime-channel";

// Bounded-backoff reconnect schedule for the Control server-stream (ms). A
// non-clean stream end (Runner mid-restart, socket blip) re-opens `Control` on
// this fixed schedule; the Runner redelivers unacked ops from the cursor, so a
// transient drop is recovered without a terminal STOPPED. After the last delay
// the drop is treated as definitive and the iterable fails. The schedule is a
// named constant chosen once here.
//
// This bounds a FAST-flapping socket: each connection shorter than the
// min-uptime floor never resets the climb, so the budget is reached and the
// iterable fails. It does NOT bound a socket that stays up past the floor
// before each drop — the reset below clears `attempt` every time. The
// no-progress budget is the independent bound for that shape; both are needed,
// neither subsumes the other.
export const CONTROL_RECONNECT_BACKOFF_MS: readonly number[] = [
	50, 200, 800, 2000,
];

// The min-uptime floor for the reset-on-open flap-detector (ms). A reconnect
// resets the BACKOFF CLIMB only if the PRIOR connection stayed open at least
// this long — the flap-detector's sense of a "successful open." Longer than the
// whole backoff schedule (Σ = 3.05s), so a socket that accepts-then-drops AT the
// retry cadence cannot reset the counter. Its scope is the ladder and nothing
// else: a socket that stays up past this floor before each drop resets the climb
// every attempt, and is bounded by the no-progress budget below.
// This replaces resetting on op-receipt, which never fired for a
// quiet-but-healthy session and let four silent flaps climb the budget to a
// spurious ERRORED.
export const CONTROL_RECONNECT_MIN_UPTIME_MS = 5000;

// The no-progress reconnect budget: at most this many consecutive reconnects
// WITHOUT the session making progress before the drop is definitive and the
// iterable fails. Progress is EITHER an op applied since the last drop (the ack
// cursor advanced) OR an op currently in flight — one yielded to the consumer
// and awaiting its apply-then-ack. Reset by progress, never by elapsed time.
//
// The in-flight arm is load-bearing, not a nicety (SEA-1540). The source is
// apply-then-ack and its single consumer (CompassAgent's control loop) awaits
// the WHOLE turn before pulling the next op, so while a long turn applies op N
// the ack cursor cannot advance — op N is acked only when the consumer returns
// for N+1. A budget watching `appliedCount` alone therefore reads a long apply
// as zero progress: if the Control socket flaps >= this many times during that
// one turn (each reconnect redelivering op N, deduped as already-queued so
// nothing new applies), the budget would fill and kill a healthy session
// mid-apply. An op in flight is progress precisely because the session IS
// getting that op done, however long the apply runs.
//
// Progress, not rate, is the discriminator, because rate cannot express the
// distinction being asked of it. The backoff schedule catches a socket flapping
// FASTER than the min-uptime floor: `attempt` never resets, so the ladder runs
// out. It cannot catch a socket that is accepted, stays up past the floor, and
// THEN fails — a wedged Runner, a server-side deadline, the door's idle timeout
// — because every drop looks like a healthy connection ending and resets the
// climb.
//
// A sliding wall-clock window was tried for that shape and does not work. With
// the ladder reset firing on every past-floor drop the delay is always
// BACKOFF_MS[0], so reconnect spacing is (connection lifetime + 50ms) and a
// 10-per-60s window only terminates lifetimes in the ~5.0–6.6s band — every case
// it was written for (a 120s idle timeout, a server deadline, a Runner wedging
// after 30s) spaces its reconnects wider than the window and ages out of it,
// reconnecting forever. Widening the window does not rescue it: a healthy
// long-lived session with sparse blips and a socket wedging at the idle timeout
// produce the SAME reconnect rate, so no threshold separates them.
//
// What separates them is whether the session is getting anything done. A wedged
// socket redelivers nothing, applies nothing, and holds nothing in flight
// however slowly it flaps, so it exhausts this budget; a session that applies
// ops between blips — or is mid-apply on one when a blip lands — resets it and
// is never killed, at any spacing.
//
// The residual, stated rather than hidden: a genuinely IDLE session — one with
// no control traffic at all, nothing applied and nothing in flight — makes no
// progress either, so 10 consecutive no-op reconnects fail it. That is
// deliberate fail-closed behavior. A control stream that has re-opened ten times
// and carried nothing is indistinguishable at this seam from a Runner that will
// never send again, and surfacing ERRORED is better than an agent silently
// reconnecting forever against a dead peer.
export const CONTROL_RECONNECT_NO_PROGRESS_MAX = 10;

// The immediate-dispatch handle: the SDK actions a mid-turn `steer` / turn-end
// `deliver` drives without waiting for the iterator's next pull. Frozen C4
// signature (design.md C4 Interfaces). As of SEA-1310 §8 the handle carries the
// full comms `Message` (`.id` intact) — no longer the empty shell of C4b: BOTH
// arms decode their `message` field (`SteerControl.message`, populated by
// SEA-1569 (T7); `DeliverControl.message`) and forward it here, where the CompassAgent
// dedups on `msg.id`, injects (steer as a mid-turn interrupt / idle-start turn;
// deliver coalesced to a turn-end prompt), and acks per message.
export interface ImmediateControl {
	// The second arg is the denormalized author `from_handle` off the wire
	// control (RIG-2486 T1) — the value the CompassAgent emits on the
	// SessionInjection observation without a per-injection roster lookup. Empty
	// when the Server could not resolve the author handle (a handle miss is
	// logged server-side, never a delivery block).
	steer(msg: Message, fromHandle: string): void; // compass.v1.Message — .id intact
	deliver(msg: Message, fromHandle: string): void; // compass.v1.Message — .id intact
	// RIG-2732 W3 forge notification arm. UNLIKE steer/deliver — which ack their
	// control_seq at DECODE because they dispatch at decode — the forge arm
	// enqueues on the CompassAgent's turn-end queue (RT-3 coalescing) and defers
	// BOTH acks to the turn-end FLUSH: a decode-ack would discard the Runner's
	// retain-until-acked durability for the decode->flush window (design.md
	// 1006-1013). So the source hands the agent an `ackRail` thunk closing over
	// this op's control_seq; the agent calls it at flush, AFTER emitting the
	// ForgeNotificationAck frame, to retire the op on the control rail (advancing
	// the AckCursor + deleting the in-window dedup entry). The agent sources the
	// forge delivery ack (subscription_id + revision) straight off `notification`.
	forgeNotification(notification: ForgeNotification, ackRail: () => void): void;
}

// Decode the immediate-op payload into the comms `Message` the `immediate`
// handle applies. `DeliverControl.message` (SEA-1310 §8) carries the full comms
// Message with its `.id` — return it when present. `SteerControl.message` (added
// by SEA-1569 (T7)) likewise carries the comms Message. An empty SteerControl (no
// `message` field) has nothing to read and yields `undefined` → counted-unmapped
// (staged) at the caller; a deliver whose `message` is absent is malformed → also
// `undefined` → counted-unmapped. The caller never fabricates a payload.
function decodeImmediatePayload(
	shell: SteerControl | DeliverControl,
): Message | undefined {
	return "message" in shell ? shell.message : undefined;
}

// Wait `ms`, or wake early if `signal` aborts — whichever comes first. The
// listener is ALWAYS detached, on both paths. `{ once: true }` alone is not
// enough: it only removes the listener if the event actually fires, so on the
// normal path (the timer wins, the pump reconnects) the closure would stay
// attached to the source-lifetime AbortController and accumulate one live
// listener — each retaining a timer handle — per retry, in a long-lived agent
// container. Factored out so the detach cannot be forgotten by a later caller.
function sleepOrAbort(ms: number, signal: AbortSignal): Promise<void> {
	return new Promise<void>((resolve) => {
		// Already aborted: resolve now. `addEventListener` on an already-aborted
		// signal never fires (WHATWG), so without this the timer runs to term —
		// measured 2001ms on a 2000ms sleep, holding one live listener the whole
		// time. That is precisely the leak this function's doc-comment exists to
		// prevent, on the abort path.
		if (signal.aborted) {
			resolve();
			return;
		}
		const onAbort = (): void => {
			clearTimeout(timer);
			signal.removeEventListener("abort", onAbort);
			resolve();
		};
		const timer = setTimeout(() => {
			signal.removeEventListener("abort", onAbort);
			resolve();
		}, ms);
		signal.addEventListener("abort", onAbort);
	});
}

/** Optional collaborators for {@link createSocketControlSource}. */
export interface SocketControlSourceOptions {
	/**
	 * Surfaces a decoded op the source could not apply (empty shell, barrier
	 * refusal, duplicate redelivery) — logged + counted, never silently
	 * dropped, mirroring `CompassAgent`'s channel. Defaults to console.
	 */
	readonly onUnmapped?: (u: UnmappedEvent) => void;
	/**
	 * Monotonic clock for the reconnect flap-detector (M1). `performance.now`
	 * and not `Date.now`: the uptime comparison spans an arbitrarily long
	 * connection, and a wall clock is subject to NTP steps, manual sets, and
	 * suspend/resume — a backward jump would suppress the reset on a healthy
	 * long-lived connection (the exact spurious-ERRORED failure M1 exists to
	 * fix) and a forward jump would fire a false one. Agent containers are
	 * where a step is most plausible (host suspend, VM migration, first NTP
	 * sync after container start). Injected so a test drives connection uptime
	 * deterministically instead of waiting the real multi-second min-uptime
	 * floor.
	 */
	readonly now?: () => number;
}

const defaultOnUnmapped = (u: UnmappedEvent): void =>
	console.error(
		`[compass-agent] control unmapped: ${u.eventType} — ${u.reason}`,
	);

/**
 * A ControlSource over `AgentGateway.Control`. Opens the agent's control
 * subscription, dispatches each pushed `AgentControl` by variant
 * (representable → yielded; immediate/empty-shell → counted-unmapped at
 * decode), emits apply-then-ack `ControlAck`/`ReplayCompleteAck` on the shared
 * Publish spine, and reconnects (bounded) on a transport drop while ending
 * cleanly on a Runner-initiated close.
 *
 * The two required arguments are the frozen C4 contract; the optional
 * collaborators are an options bag so a caller overriding only the clock does
 * not have to restate the unmapped handler positionally.
 *
 * @param transport the Runner socket handle (its `control()` server-stream +
 *   `publishSpine()` ack lane)
 * @param immediate the SDK steer/deliver actions the immediate path drives —
 *   threaded per the frozen signature; not invoked while the wire carries empty
 *   shells (OQ-2(A)), SEA-1310 populates the payload
 * @param options optional `onUnmapped` / `now` collaborators
 */
export function createSocketControlSource(
	transport: RunnerTransport,
	immediate: ImmediateControl,
	options: SocketControlSourceOptions = {},
): ControlSource {
	const onUnmapped = options.onUnmapped ?? defaultOnUnmapped;
	const now = options.now ?? (() => performance.now());
	const spine = transport.publishSpine();
	const acks = new AckCursor(spine);
	const buffer = new AsyncBuffer();
	// Cancels the background pump + the underlying Control server-stream when the
	// consumer abandons the iterable (its `return()`), so an abandoned source does
	// not keep consuming the transport and dispatching into the buffer (M2).
	const abort = new AbortController();
	// Borrow the single transport-owned ManagedRuntime through the module-private
	// channel when present; otherwise (a fake transport in a unit test) make and
	// OWN a default runtime as before (design record §T5). Effect is confined
	// module-private here: the runtime backs the forked, interruptible reconnect
	// pump fiber. An OWNED (fallback) runtime is disposed in the iterator's
	// return() — the AsyncIterable has no drain(), so return() is its only
	// teardown seam. A BORROWED runtime is disposed by the transport's close(),
	// never here. The fallback runtime removes the default logger so a swallowed
	// pump defect does not double-report to the console, mirroring the sibling
	// transport lanes (frame-sink.ts, publish-spine.ts).
	const borrowedRuntime = getTransportRuntime(transport);
	const ownsRuntime = borrowedRuntime === undefined;
	const runtime =
		borrowedRuntime ?? ManagedRuntime.make(Logger.remove(Logger.defaultLogger));
	// Seqs decoded to a representable op and queued but not yet applied. Dedups a
	// redelivery of an op the source already holds (reconnect/takeover) against
	// re-queueing it; cleared as each is applied on pull.
	const queued = new Set<bigint>();
	// The source's own view of the replay barrier, set when replayComplete is
	// decoded. The immediate path (which never reaches CompassAgent's barrier)
	// enforces it locally (invariant 1) — a belt-and-suspenders on the Runner's
	// hold and CompassAgent's iterator-side barrier.
	let replayComplete = false;
	// SEA-1540: an op is "in flight" when the iterator has yielded it to the
	// consumer and is awaiting the apply-then-ack the next pull proves. Set by
	// the iterator's next() and read by pump's no-progress budget: a drop while
	// an apply is in flight is progress, not a wedge — a long turn cannot advance
	// the ack cursor until the consumer returns for the next op. Factory-scope
	// mutable state shared between the iterator and pump, like the two above.
	let applyInFlight = false;

	function count(eventType: string, reason: string): void {
		// unmapped{event_type} (design.md Decision 2). `count()` is a plain sync
		// funnel called from `dispatch()` on the event loop, OUTSIDE Effect
		// context, so the increment runs through the runtime synchronously — the
		// same `runtime.runSync(Metric.increment(...))` shape publish-spine.ts's
		// sync enqueue site uses. `event_type` is tagged per-call because the wire
		// eventType varies.
		runtime.runSync(
			Metric.increment(Metric.tagged(controlUnmapped, "event_type", eventType)),
		);
		onUnmapped({ kind: "unmapped", eventType, reason });
	}

	// Route one wire AgentControl. Immediate/empty-shell ops are applied (counted)
	// and acked here at decode; representable ops are queued for the iterable and
	// acked on apply-then-ack when the consumer pulls past them.
	function dispatch(wire: WireAgentControl): void {
		const seq = wire.controlSeq;
		const kind = wire.control.case ?? "unknown";
		// Fail-closed on an invalid seq. The Runner assigns strictly-positive,
		// 1-based control_seq (design.md: "Runner-assigned, monotonic per session");
		// a seq < 1 means a broken/0-based producer, NOT a real op. Refuse it
		// loudly instead of letting it slip into the dedup path below: `controlSeq`
		// is proto3 uint64 (defaults 0n) and the AckCursor also inits 0n, so an
		// unguarded seq 0 would satisfy `isApplied(0n)` (0n <= 0n) and be silently
		// swallowed as an already-applied duplicate — eating a session's first
		// control op with no signal. Counted + dropped (not acked: there is no
		// legitimate op at this seq for the Runner to retire).
		//
		// The trade that buys, stated so it is not rediscovered from a log: the
		// Runner retains every op past the ack cursor, so an unacked seq-0 op is
		// retained indefinitely and re-dropped on every subsequent reconnect,
		// emitting one unmapped count per redelivery. That is the fail-closed
		// choice over acking it away — a broken producer stays visible rather than
		// being silently retired — and for THAT shape it is rate-limited by the
		// reconnect cadence. It is not a general throttle: a producer streaming
		// seq-0 ops within one open stream counts once per message here, unbounded.
		// If that shape is ever observed, throttle at `onUnmapped`, not in
		// `dispatch` — the count is the signal and dropping it hides the producer.
		if (seq < 1n) {
			count(
				`control:${kind}`,
				"invalid control_seq < 1 — Runner must assign 1-based seqs",
			);
			return;
		}
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
				// decode the payload: both STEER and DELIVER carry the comms Message
				// (SEA-1310 §8; steer's `message` field populated by SEA-1569 (T7)) and
				// dispatch through `immediate.steer` / `immediate.deliver`. An empty
				// SteerControl (no `message` field) decodes to undefined and is
				// counted-unmapped (staged) without fabricating a payload (OQ-2(A)).
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
					immediate.steer(msg, wire.control.value.fromHandle);
				} else {
					immediate.deliver(msg, wire.control.value.fromHandle);
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
			case "forgeNotification": {
				// RIG-2732 W3 turn-end forge arm. Barrier-enforced (invariant 1): a
				// live forge notification before ReplayComplete is refused-and-counted
				// and acked at decode, exactly as a pre-barrier steer/deliver — it never
				// reaches the turn-end queue.
				if (!replayComplete) {
					count(
						`control:${kind}`,
						"live forge notification before ReplayComplete — refused by replay barrier",
					);
					acks.markApplied(seq);
					return;
				}
				// Otherwise ENQUEUE on the CompassAgent's turn-end queue and DEFER both
				// acks to the flush (unlike steer/deliver, which ack here at decode).
				// The seq is tracked in `queued` so a redelivery in the decode->flush
				// window dedups as already-queued (the rail ack has not fired yet); the
				// `ackRail` thunk the agent calls at flush removes it and markApplied's
				// the control_seq, retiring the op on the rail AFTER the agent emitted
				// the ForgeNotificationAck frame. A decode-ack here would discard the
				// Runner's retain-until-acked durability for that window (design.md
				// 1006-1013).
				queued.add(seq);
				immediate.forgeNotification(wire.control.value, () => {
					queued.delete(seq);
					acks.markApplied(seq);
				});
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

	// Consume the Control stream, reconnecting on a drop. A clean, Runner-initiated
	// stream end closes the buffer (→ STOPPED); a non-clean end (thrown) re-opens
	// the subscription on the bounded backoff, from which the Runner redelivers
	// unacked ops. Runs as a forked interruptible fiber on the module-local
	// runtime (design record §T4); its terminal state reaches the consumer through
	// the buffer, never as an unhandled rejection. The iterator's return()
	// interrupts it and aborts the source-lifetime signal (cancelling the in-flight
	// Control RPC). The ladder is an explicit attempt-indexed loop, NOT
	// Schedule.fromDelays: the min-uptime flap reset (design record §T4) zeroes the
	// attempt index mid-ladder, and a Schedule consumed by Effect.retry advances
	// internally with no external reset handle.
	const pumpEffect = Effect.gen(function* () {
		let attempt = 0;
		// Consecutive reconnects without the session making progress. Progress is
		// the AckCursor's applied count advancing OR an apply in flight
		// (`applyInFlight`), not a timestamp: the count advances only on a
		// genuinely new application (a redelivery the source dedups and re-acks is
		// correctly NOT progress), and the in-flight arm keeps a long apply
		// mid-turn — which cannot advance the cursor until the consumer returns for
		// the next op — from reading as a wedge (SEA-1540).
		let noProgress = 0;
		let appliedAtLastDrop = acks.appliedCount;
		for (;;) {
			// Consumer abandoned the iterable (return() → abort) before this (re)open:
			// stop quietly, the iterator's return() already closed the buffer.
			if (abort.signal.aborted) return;
			// Uptime is measured from STREAM ESTABLISHMENT, not from construction.
			// connect's server-stream is LAZY: `transport.control()` returns an async
			// generator whose body — including the dial — does not run until the
			// first `next()`, so a stamp taken here or right after the call records
			// a connection that has not been attempted yet. A dial that hangs D ms
			// before failing would then report uptime ≈ D and reset the backoff
			// climb every attempt: measured unbounded reconnects (500+) for D ≥ ~6.6s,
			// the retry the no-progress budget exists to stop.
			//
			// `onHeader` is the seam that separates the two cases, and it must be
			// establishment rather than first message: a stream that opens and
			// yields ZERO ops before dropping is quiet-but-HEALTHY and must still
			// reset the ladder (F2(a)) — gating on a delivered message would
			// reinstate the op-receipt coupling this design removed. The header
			// lands only once the dial completes, so a hung dial never stamps.
			// Never read before `onHeader` writes it: the only read is guarded by
			// `established`, which that same callback sets. 0 rather than a `now()`
			// sample so it does not read as a meaningful fallback stamp.
			let openedAt = 0;
			let established = false;
			// Consume one connection on the event loop, exactly as before — the
			// for-await drives the Connect server-stream. Effect.tryPromise carries
			// its settlement into the fiber (clean end → Right; drop → Left with the
			// raw error) without changing the wire behaviour; the raw error is kept
			// in the failure channel (catch: identity) so buffer.fail() surfaces the
			// real ConnectError, not an Effect wrapper.
			const result = yield* Effect.either(
				Effect.gen(function* () {
					// The `established` fact is set inside the `onHeader` JS closure,
					// which the Connect stream invokes OUTSIDE any Effect context, so
					// `Effect.annotateCurrentSpan` cannot reach it — it becomes a span
					// EVENT instead (design.md Decision 1, "`…control.connection`
					// mechanism"). Capture the live span handle inside the withSpan
					// scope, and a clock so the event carries a walltime-nanos
					// timestamp from the SAME source effect stamps span start/end with
					// (`clock.unsafeCurrentTimeNanos()`; @effect/opentelemetry's tracer
					// treats the event bigint as walltime nanos). Header receipt always
					// happens while the tryPromise — and thus the span — is live, so
					// the handle is valid when `onHeader` fires.
					const span = yield* Effect.currentSpan;
					const clock = yield* Effect.clock;
					return yield* Effect.tryPromise({
						try: async () => {
							const stream = transport.control(
								create(ControlSubscribeRequestSchema, {}),
								{
									signal: abort.signal,
									onHeader: () => {
										established = true;
										openedAt = now();
										span.event("established", clock.unsafeCurrentTimeNanos());
									},
								},
							);
							for await (const wire of stream) {
								dispatch(wire);
							}
						},
						catch: (err) => err,
					});
				}).pipe(
					// `attempt` (the ladder index, known at span open) is a withSpan
					// attribute (design.md Decision 1). One span per connection attempt.
					Effect.withSpan("compass_agent.transport.control.connection", {
						attributes: { attempt },
					}),
				),
			);
			if (Either.isRight(result)) {
				buffer.close();
				return;
			}
			const err = result.left;
			// The consumer's return() aborted the stream — not a transport drop.
			// End quietly (buffer already closed by return()); never reconnect or
			// fail() an intentional cancellation.
			if (abort.signal.aborted) return;
			// No-progress bound, checked BEFORE the uptime reset below — this is
			// the termination the reset cannot clear. A socket that is accepted,
			// stays up past the floor, and then fails (wedged Runner, server-side
			// deadline, door idle timeout) resets the ladder on every drop, so the
			// ladder can never bound it; what bounds it is that it never makes
			// progress. Progress is any single application since the last drop OR
			// an op in flight at drop time (`applyInFlight`) — a long apply
			// mid-turn cannot advance the ack cursor until the consumer returns for
			// the next op, so without the in-flight arm a healthy session flapping
			// during one long turn would be killed mid-apply (SEA-1540). Either arm
			// zeroes the counter, so a healthy session is untouched however widely
			// its blips are spaced — the distinction a reconnect-RATE window could
			// not draw, since a healthy sparse-blip session and a socket wedging at
			// an idle timeout reconnect at the same rate.
			const applied = acks.appliedCount;
			const madeProgress = applied > appliedAtLastDrop || applyInFlight;
			noProgress = madeProgress ? 0 : noProgress + 1;
			// no_progress_depth (design.md Decision 2): the gauge tracks the current
			// consecutive-no-progress level, including the reset-to-0 that
			// madeProgress just applied above.
			yield* Metric.set(noProgressDepth, noProgress);
			appliedAtLastDrop = applied;
			if (noProgress >= CONTROL_RECONNECT_NO_PROGRESS_MAX) {
				buffer.fail(err);
				return;
			}
			// Reset-on-open flap-detector: a connection that stayed open past the
			// min-uptime floor was healthy, so its drop resets the backoff climb —
			// the next reconnect starts fast. A rapid flap (each connection
			// shorter than the floor) never resets, so it still climbs the budget
			// to a definitive fail. This replaces resetting on op-receipt, which
			// left a quiet-but-healthy session (a reconnect redelivering zero ops)
			// unable to reset and let four silent flaps spuriously ERROR it.
			//
			// `established` does NOT reinstate op-receipt gating: a stream that
			// opened and sat quiet still resets, because the header stamped
			// `openedAt` and the floor is measured from there. It excludes only a
			// stream that never opened at all — one whose lazy dial hung and
			// threw, where the elapsed time is dial latency, not uptime. Uptime is
			// sampled on the INJECTED `now()`, never Effect Clock (design record
			// §T4: the flap-detector tests inject and advance `now`).
			if (established && now() - openedAt >= CONTROL_RECONNECT_MIN_UPTIME_MS) {
				attempt = 0;
				// flap_resets (design.md Decision 2): count only when the reset fires.
				yield* Metric.increment(flapResets);
			}
			if (attempt >= CONTROL_RECONNECT_BACKOFF_MS.length) {
				buffer.fail(err);
				return;
			}
			const delay = CONTROL_RECONNECT_BACKOFF_MS[attempt++];
			// reconnects (design.md Decision 2): one increment per backoff taken.
			yield* Metric.increment(reconnects);
			// The backoff wait is Effect.sleep(delay) raced against a single
			// listener on the source-lifetime abort signal, always detached on both
			// settle paths — kept here as `sleepOrAbort` wrapped in Effect.promise,
			// which registers exactly ONE live abort listener for the duration of
			// the wait and ZERO after (design record §T4; pinned by
			// control-source.test.ts F5). A bare Effect.sleep under fiber
			// interruption registers nothing on the signal and would redden F5.
			yield* Effect.promise(() => sleepOrAbort(delay, abort.signal));
		}
	});

	// The pump starts on first iteration and runs once for the source's life,
	// forked as an interruptible fiber on the module-local runtime. The
	// ControlSource is single-consumer by contract (CompassAgent's one control
	// loop, agent.ts). Two separate handles, deliberately NOT one: `started` is
	// the fork-once latch — set on first iteration and never reset, so a second
	// `for await` (or a re-iterate after return()) never spawns a duplicate pump
	// on the shared buffer/spine, and never re-forks on the now-disposed runtime.
	// `pumpFiber` is only the interrupt handle return() nulls for idempotency;
	// nulling it must NOT re-open the fork path, which is why the latch is separate.
	let started = false;
	let pumpFiber: Fiber.RuntimeFiber<void> | undefined;
	return {
		[Symbol.asyncIterator](): AsyncIterator<AgentControl> {
			if (!started) {
				started = true;
				pumpFiber = runtime.runFork(pumpEffect);
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
						applyInFlight = false;
						queued.delete(applied.seq);
						if (applied.op.kind === "replayComplete")
							acks.emitReplayCompleteAck();
						acks.markApplied(applied.seq);
					}
					const r = await buffer.pull();
					if (r.done) return { value: undefined, done: true };
					lastYielded = r.value;
					applyInFlight = true;
					return { value: r.value.op, done: false };
				},
				// The consumer abandoned the `for await` (agent.ts's control loop
				// erroring / breaking): tear the source down. `abort.abort()` is the
				// cancellation ROOT — it cancels the in-flight Control RPC via the
				// `{ signal }` threaded into transport.control() AND wakes the backoff
				// wait (sleepOrAbort resolves, detaching its one live abort listener),
				// so the parked pump fiber unblocks and its top-of-loop guard returns.
				// It must fire FIRST: the backoff wait is an uninterruptible
				// Effect.promise, so Fiber.interrupt alone cannot end a fiber parked in
				// it — only the abort unparks it (design record §T4). buffer.close()
				// settles an in-flight pull. Then the now-unblocked fiber is interrupted
				// (a belt-and-suspenders join point) — always, since the pump is the
				// source's own — and the ManagedRuntime is disposed ONLY if this source
				// owns it (the fallback path). A BORROWED transport-owned runtime is
				// disposed by the transport's close() after the drain barrier; disposing
				// it here would break the still-open sibling sink/spine that share it
				// (design record §T5). The AsyncIterable has no drain(), so return() is a
				// self-owned runtime's only teardown seam.
				// Idempotent: abort/close/interrupt/dispose all no-op once fired.
				// A pending `lastYielded` is deliberately left UNACKED here. That is
				// correct for the only path that reaches return() today: agent.ts's
				// control loop exits solely by `#applyControl` THROWING, and an apply
				// that failed must not be acked — the Runner redelivers the op to the
				// next session (the at-least-once apply-then-ack seam). The invariant
				// is load-bearing: a consumer that added a clean `break` after a
				// SUCCESSFUL apply would silently trade that for a redelivery of an op
				// the agent already applied.
				async return(): Promise<IteratorResult<AgentControl>> {
					abort.abort();
					buffer.close();
					if (pumpFiber !== undefined) {
						await runtime.runPromise(Fiber.interrupt(pumpFiber));
						pumpFiber = undefined;
					}
					if (ownsRuntime) await runtime.dispose();
					return { value: undefined, done: true };
				},
			};
		},
	};
}
