// The socket ControlSource: the inbound half of the agent↔Runner socket transport
// (transport-consolidation C4). It is a source over `AgentGateway.Control`, the
// agent-opened server-stream the Runner pushes one `AgentControl` per message down.

// It is a DISPATCHER, not a bare decoder: the agent's control loop is sequential
// and its `prompt` arm awaits the whole turn, so anything routed through that pull
// loop queues behind the running turn. To keep a mid-turn interrupt off the turn's
// await, the source consumes the stream on the event loop and routes by variant:

// - `prompt` / `replayComplete` are wire-representable today — decoded and yielded
//   on the iterable the CompassAgent pulls. ACKED apply-then-ack: the consumer
//   returning for the next op proves the previous applied, so the source advances
//   its ControlAck cursor at the start of each next(), never on receipt (P1 #6).

// - `steer` / `deliver` are the IMMEDIATE-dispatch class, processed at decode ahead
//   of any queued iterator op. Both carry the comms Message on the wire (RIG-1310
//   §8 / RIG-1569 T7) — dispatched through immediate.steer/deliver, where the agent
//   dedups on msg.id, injects, and acks per message.

// - An empty SteerControl decodes to undefined and is counted-unmapped (OQ-2(A)).
//   Barrier-enforced (invariant 1): a pre-ReplayComplete immediate op is refused-
//   and-counted, never applied. `replay` / `config` are empty shells in C1 (OQ-1),
//   counted-unmapped at decode until RIG-1310 populates them.

// Because an immediate op counted at decode is "applied" ahead of an earlier queued
// iterator op (invariant 2), the contiguous cursor alone cannot mark it done; the
// source names its seq in the ack's applied_above set, and the Runner drops it from
// retention so a redelivered copy is never re-applied (amended OQ-6).

// Close-reason contract (OQ-6): a clean Runner-initiated end ends the iterable (→
// STOPPED); a transport DROP triggers a bounded reconnect (the Runner redelivers
// every op past the acked cursor) and does NOT end the iterable. A redelivered op
// already applied/queued is seq-deduped, re-acked if applied (at-least-once → exactly-once).

// Acks ride the SAME ordered per-session Publish spine as the FrameSink's frames
// (OQ-1, OQ-4(i)): one ordered publisher per session keeps the Runner's gap-detection
// well-defined, so both producers reach it through the transport's memoized
// publishSpine() and the source pushes acks on its never-dropped priority lane.

import { create } from "@bufbuild/protobuf";
import { Effect, Either, Fiber, Logger, ManagedRuntime, Metric } from "effect";
import type { DeliverSourceNames } from "./../agent";
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
	noProgressDepthGauge,
	reconnects,
} from "./otel-metrics";
import { getTransportRuntime } from "./runtime-channel";

// Bounded-backoff reconnect schedule for the Control server-stream (ms). A non-clean
// end re-opens Control on this schedule; the Runner redelivers unacked ops from the
// cursor, so a transient drop recovers without a terminal STOPPED. After the last
// delay the drop is definitive and the iterable fails.

// This bounds a FAST-flapping socket: each connection shorter than the min-uptime
// floor never resets the climb, so the budget is reached. It does NOT bound a socket
// that stays up past the floor before each drop; the no-progress budget is the
// independent bound for that shape — both are needed, neither subsumes the other.
export const CONTROL_RECONNECT_BACKOFF_MS: readonly number[] = [
	50, 200, 800, 2000,
];

// The min-uptime floor for the reset-on-open flap-detector (ms). A reconnect resets
// the BACKOFF CLIMB only if the PRIOR connection stayed open at least this long.
// Longer than the whole schedule (Σ = 3.05s), so a socket that accepts-then-drops AT
// the retry cadence cannot reset the counter. Its scope is the ladder only.

// This replaces resetting on op-receipt, which never fired for a quiet-but-healthy
// session and let four silent flaps climb the budget to a spurious ERRORED.
export const CONTROL_RECONNECT_MIN_UPTIME_MS = 5000;

// The no-progress reconnect budget: at most this many consecutive reconnects WITHOUT
// progress before the drop is definitive. Progress is EITHER an op applied since the
// last drop (ack cursor advanced) OR an op in flight — one yielded to the consumer
// awaiting its apply-then-ack. Reset by progress, never by elapsed time.

// The in-flight arm is load-bearing (RIG-1540). The source is apply-then-ack and its
// consumer awaits the WHOLE turn before pulling the next op, so while a long turn
// applies op N the ack cursor cannot advance. A budget watching appliedCount alone
// reads a long apply as zero progress and would kill a healthy session mid-apply.

// Progress, not rate, is the discriminator. The backoff schedule catches a socket
// flapping FASTER than the min-uptime floor (attempt never resets, ladder runs out).
// It cannot catch a socket accepted, up past the floor, then failing — a wedged
// Runner, a server deadline, an idle timeout — each drop resets the climb.

// A sliding wall-clock window was tried and does not work: with the ladder reset on
// every past-floor drop the spacing is (lifetime + 50ms), so a 10-per-60s window only
// terminates lifetimes in the ~5.0–6.6s band — every real case spaces wider and ages
// out. It cannot separate a healthy sparse-blip session from an idle-timeout wedge.

// What separates them is whether the session is getting anything done. A wedged
// socket redelivers, applies, and holds nothing however slowly it flaps, so it
// exhausts this budget; a session applying ops between blips (or mid-apply on one)
// resets it and is never killed, at any spacing.

// The residual, stated: a genuinely IDLE session makes no progress either, so 10
// consecutive no-op reconnects fail it. Deliberate fail-closed — a stream re-opened
// ten times carrying nothing is indistinguishable from a Runner that will never send.
export const CONTROL_RECONNECT_NO_PROGRESS_MAX = 10;

// The immediate-dispatch handle: the actions a mid-turn steer / turn-end deliver
// drives without waiting for the iterator's next pull. Frozen C4 signature. As of
// RIG-1310 §8 the handle carries the full comms Message (.id intact): both arms decode
// their message field and forward it here, where the agent dedups, injects, and acks.
export interface ImmediateControl {
	// The second arg is the denormalized author from_handle off the wire (RIG-2486
	// T1), empty on a resolve miss. The third is the W3C traceparent (RIG-2508 T3),
	// empty when there was no active span. The fourth carries the denormalized SOURCE
	// channel + topic NAMES (peer-DM DL-292), the delivery's source/reply target.
	steer(
		msg: Message,
		fromHandle: string,
		traceparent: string,
		sourceNames: DeliverSourceNames,
	): void; // compass.v1.Message — .id intact
	deliver(
		msg: Message,
		fromHandle: string,
		traceparent: string,
		sourceNames: DeliverSourceNames,
	): void; // compass.v1.Message — .id intact
	// RIG-2732 W3 forge notification arm. UNLIKE steer/deliver (which ack at DECODE),
	// the forge arm enqueues on the agent's turn-end queue (RT-3) and defers BOTH acks
	// to the flush (a decode-ack would discard retain-until-acked durability). The
	// source hands an ackRail thunk the agent calls at flush to retire the op.
	forgeNotification(notification: ForgeNotification, ackRail: () => void): void;
}

// Decode the immediate-op payload into the comms Message the immediate handle applies.
// DeliverControl.message (RIG-1310 §8) and SteerControl.message (RIG-1569 T7) both
// carry the full Message — return it when present. An empty/malformed shell yields
// undefined → counted-unmapped at the caller. The caller never fabricates a payload.
function decodeImmediatePayload(
	shell: SteerControl | DeliverControl,
): Message | undefined {
	return "message" in shell ? shell.message : undefined;
}

// Wait `ms`, or wake early if `signal` aborts. The listener is ALWAYS detached, on
// both paths. `{ once: true }` alone is not enough: it removes the listener only if
// the event fires, so on the timer-wins path the closure would stay attached to the
// source-lifetime AbortController and leak one live listener per retry.
function sleepOrAbort(ms: number, signal: AbortSignal): Promise<void> {
	return new Promise<void>((resolve) => {
		// Already aborted: resolve now. addEventListener on an already-aborted signal
		// never fires (WHATWG), so without this the timer runs to term (measured 2001ms
		// on a 2000ms sleep) holding one live listener — the leak this function prevents.
		if (signal.aborted) {
			resolve();
			return;
		}
		const onAbort = (): void => {
			clearTimeout(timer);
			signal.removeEventListener("abort", onAbort);
			resolve();
		};
		// biome-ignore lint/style/noRestrictedGlobals: production abort-aware backoff wait (sleepOrAbort, resolves early on signal), cleared on abort; not a test wait
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
	/**
	 * Namespace prefix for the `no_progress_depth` LEVEL gauge this source sets.
	 * A plain string (never an `effect` type) so this exported options bag stays
	 * free of the `effect` package — `createSocketControlSource` is re-exported
	 * from the package entry, and the export-surface guard forbids an `effect`
	 * type on the public `.d.ts`. Defaults to "" — production yields the exact
	 * frozen metric name. A test passes a unique prefix so its gauge read hits a
	 * private registry entry, immune to the cross-file gauge race: the
	 * shared process-global registry keys structurally on the metric name, and a
	 * bare gauge would be moved by a concurrent sibling test file between this
	 * source's Metric.set and the test's synchronous read.
	 */
	readonly metricNamespace?: string;
}

const defaultOnUnmapped = (u: UnmappedEvent): void =>
	// biome-ignore lint/suspicious/noConsole: default unmapped-control sink surfaces protocol drift to the operator
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
 *   shells (OQ-2(A)), RIG-1310 populates the payload
 * @param options optional `onUnmapped` / `now` collaborators
 */
export function createSocketControlSource(
	transport: RunnerTransport,
	immediate: ImmediateControl,
	options: SocketControlSourceOptions = {},
): ControlSource {
	const onUnmapped = options.onUnmapped ?? defaultOnUnmapped;
	const now = options.now ?? (() => performance.now());
	const noProgressDepth = noProgressDepthGauge(options.metricNamespace ?? "");
	const spine = transport.publishSpine();
	const acks = new AckCursor(spine);
	const buffer = new AsyncBuffer();
	// Cancels the background pump + the underlying Control server-stream when the
	// consumer abandons the iterable (its `return()`), so an abandoned source does
	// not keep consuming the transport and dispatching into the buffer (M2).
	const abort = new AbortController();
	// Borrow the single transport-owned ManagedRuntime when present; otherwise (a fake
	// transport) make and OWN a default (§T5). It backs the forked reconnect pump fiber.
	// An OWNED runtime is disposed in return() (the only teardown seam), a BORROWED one
	// by transport.close(). The fallback removes the default logger (no double-report).
	const borrowedRuntime = getTransportRuntime(transport);
	const ownsRuntime = borrowedRuntime === undefined;
	const runtime =
		borrowedRuntime ?? ManagedRuntime.make(Logger.remove(Logger.defaultLogger));
	// Seqs decoded to a representable op and queued but not yet applied. Dedups a
	// redelivery of an op the source already holds against re-queueing; cleared as
	// each is applied on pull.
	const queued = new Set<bigint>();
	// The source's own view of the replay barrier, set when replayComplete is
	// decoded. The immediate path (which never reaches CompassAgent's barrier)
	// enforces it locally (invariant 1) — a belt-and-suspenders on the Runner's
	// hold and CompassAgent's iterator-side barrier.
	let replayComplete = false;
	// RIG-1540: an op is "in flight" when the iterator has yielded it and awaits the
	// apply-then-ack the next pull proves. Set by next(), read by pump's no-progress
	// budget: a drop while an apply is in flight is progress, not a wedge. Factory-
	// scope mutable state shared between the iterator and pump.
	let applyInFlight = false;

	function count(eventType: string, reason: string): void {
		// unmapped{event_type} (Decision 2). count() is a plain sync funnel called from
		// dispatch() on the event loop OUTSIDE Effect context, so the increment runs
		// through the runtime synchronously (same runSync shape publish-spine.ts uses).
		// event_type is tagged per-call because the wire eventType varies.
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
		// Fail-closed on an invalid seq. The Runner assigns strictly-positive 1-based
		// control_seq; a seq < 1 means a broken producer. controlSeq is proto3 uint64
		// (defaults 0n) and the AckCursor inits 0n, so an unguarded seq 0 satisfies
		// isApplied(0n) and is silently swallowed — eating a session's first control op.

		// Counted + dropped, not acked: the Runner retains it, so it re-drops on every
		// reconnect (one count each) — the fail-closed choice keeping a broken producer
		// visible, rate-limited by reconnect cadence. A producer streaming seq-0 within
		// one stream counts unbounded; throttle at onUnmapped if seen, never in dispatch.
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
				// Immediate-dispatch class. Barrier-enforced (invariant 1): a live immediate
				// op before ReplayComplete is refused-and-counted. Otherwise decode: both
				// STEER and DELIVER carry the comms Message (RIG-1310 §8; steer's via
				// RIG-1569 T7); an empty SteerControl → undefined, counted-unmapped (OQ-2(A)).
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
						"empty-shell steer/deliver — payload staged (RIG-1310)",
					);
				} else if (wire.control.case === "steer") {
					immediate.steer(
						msg,
						wire.control.value.fromHandle,
						wire.control.value.traceparent,
						{
							channelName: wire.control.value.channelName,
							topicName: wire.control.value.topicName,
						},
					);
				} else {
					immediate.deliver(
						msg,
						wire.control.value.fromHandle,
						wire.control.value.traceparent,
						{
							channelName: wire.control.value.channelName,
							topicName: wire.control.value.topicName,
						},
					);
				}
				// Applied (counted or dispatched) at decode → ack now, ahead of any
				// queued iterator op (invariant 2 → applied_above).
				acks.markApplied(seq);
				return;
			}
			case "replay":
			case "config": {
				// Empty shells in C1 (OQ-1): no payload to seed context / configure the
				// session, so counted-unmapped at decode. RIG-1310 populates them and
				// they flow through the iterable like prompt.
				count(
					`control:${kind}`,
					"empty-shell replay/config — payload staged (RIG-1310)",
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
				// Otherwise ENQUEUE on the agent's turn-end queue and DEFER both acks to the
				// flush. The seq is tracked in `queued` so a redelivery in the decode→flush
				// window dedups as already-queued; the ackRail thunk the agent calls at flush
				// removes it and markApplied's the seq, AFTER the ForgeNotificationAck frame.
				queued.add(seq);
				immediate.forgeNotification(wire.control.value, () => {
					queued.delete(seq);
					acks.markApplied(seq);
				});
				return;
			}
			default:
				// Unset/unknown oneof: an unrecognized control op, logged + counted, never a
				// crash. Acked so the Runner does not redeliver an op the source never applies.
				count(`control:${kind}`, "unrecognized control variant");
				acks.markApplied(seq);
				return;
		}
	}

	// Consume the Control stream, reconnecting on a drop. A clean end closes the buffer
	// (→ STOPPED); a non-clean end re-opens on the bounded backoff, from which the Runner
	// redelivers unacked ops. Runs as a forked interruptible fiber (§T4). The ladder is an
	// explicit attempt-indexed loop, NOT Schedule.fromDelays: the flap reset zeroes it mid-ladder.
	const pumpEffect = Effect.gen(function* () {
		let attempt = 0;
		// Consecutive reconnects without progress. Progress is the applied count advancing
		// OR an apply in flight (applyInFlight), not a timestamp: the count advances only
		// on a genuinely new application, and the in-flight arm keeps a long mid-turn apply
		// from reading as a wedge (RIG-1540).
		let noProgress = 0;
		let appliedAtLastDrop = acks.appliedCount;
		for (;;) {
			// Consumer abandoned the iterable (return() → abort) before this (re)open:
			// stop quietly, the iterator's return() already closed the buffer.
			if (abort.signal.aborted) return;
			// Uptime is measured from STREAM ESTABLISHMENT, not construction. connect's
			// server-stream is LAZY: the dial does not run until first next(), so a stamp
			// here records a connection not yet attempted. A dial hanging D ms would report
			// uptime ≈ D and reset the climb every attempt (measured 500+ reconnects, D≥~6.6s).

			// `onHeader` separates the two cases; it must be establishment, not first
			// message: a stream opening and yielding ZERO ops before dropping is quiet-but-
			// HEALTHY and must still reset the ladder (F2(a)). The header lands only once the
			// dial completes; read guarded by `established`, 0 so it is not a fallback stamp.
			let openedAt = 0;
			let established = false;
			// Consume one connection on the event loop — the for-await drives the Connect
			// server-stream. Effect.tryPromise carries its settlement into the fiber (clean
			// end → Right; drop → Left with the raw error, catch: identity) so buffer.fail()
			// surfaces the real ConnectError, not an Effect wrapper.
			const result = yield* Effect.either(
				Effect.gen(function* () {
					// `established` is set inside the onHeader JS closure, invoked OUTSIDE any
					// Effect context, so it becomes a span EVENT (Decision 1). Capture the live
					// span handle inside withSpan and a clock so the event carries a walltime-
					// nanos timestamp from the same source span start/end use.
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
			// No-progress bound, checked BEFORE the uptime reset — the termination the reset
			// cannot clear. A socket accepted, up past the floor, then failing resets the
			// ladder on every drop, so what bounds it is that it never makes progress.

			// Progress is any application since the last drop OR an op in flight — a long
			// apply mid-turn cannot advance the ack cursor, so without the in-flight arm a
			// healthy session flapping during one turn would be killed mid-apply (RIG-1540).
			// Either arm zeroes the counter, drawing the distinction a RATE window could not.
			const applied = acks.appliedCount;
			const madeProgress = applied > appliedAtLastDrop || applyInFlight;
			noProgress = madeProgress ? 0 : noProgress + 1;
			// no_progress_depth (Decision 2): the gauge tracks the current consecutive-no-
			// progress level, including the reset-to-0 madeProgress just applied.
			yield* Metric.set(noProgressDepth, noProgress);
			appliedAtLastDrop = applied;
			if (noProgress >= CONTROL_RECONNECT_NO_PROGRESS_MAX) {
				buffer.fail(err);
				return;
			}
			// Reset-on-open flap-detector: a connection open past the min-uptime floor was
			// healthy, so its drop resets the backoff climb. A rapid flap (each shorter than
			// the floor) never resets and climbs to a definitive fail. This replaces resetting
			// on op-receipt, which let four silent flaps spuriously ERROR a healthy session.

			// `established` does NOT reinstate op-receipt gating: a stream that opened and
			// sat quiet still resets (the header stamped openedAt). It excludes only a stream
			// that never opened — a lazy dial that hung and threw, where elapsed is dial
			// latency. Uptime is sampled on the INJECTED now(), never Effect Clock (§T4).
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
			// The backoff wait is sleepOrAbort raced against a single source-lifetime abort
			// listener, always detached on both settle paths (wrapped in Effect.promise, one
			// live listener for the wait, zero after; §T4, pinned by control-source.test.ts
			// F5). A bare Effect.sleep registers nothing on the signal and would redden F5.
			yield* Effect.promise(() => sleepOrAbort(delay, abort.signal));
		}
	});

	// The pump forks once for the source's life; the ControlSource is single-consumer by
	// contract. Two handles, NOT one: `started` is the fork-once latch (never reset, so a
	// second for-await never spawns a duplicate pump); `pumpFiber` is only the interrupt
	// handle return() nulls for idempotency.
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
					// one's application resolved. Ack it BEFORE awaiting the next pull, so a
					// ReplayCompleteAck reaches the Runner even when the next op is one held
					// behind that very barrier — no deadlock.
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
				// The consumer abandoned the for-await: tear the source down. abort.abort() is
				// the cancellation ROOT — cancels the in-flight Control RPC and wakes the
				// backoff wait. It must fire FIRST: the wait is uninterruptible Effect.promise,
				// so Fiber.interrupt alone cannot end a fiber parked in it (§T4).

				// buffer.close() settles an in-flight pull. Then the fiber is interrupted, and
				// the runtime is disposed ONLY if this source owns it — a BORROWED runtime is
				// disposed by transport.close(); disposing it here would break the sibling
				// sink/spine (§T5). Idempotent: abort/close/interrupt/dispose no-op once fired.

				// A pending lastYielded is deliberately left UNACKED. Correct for the only path
				// reaching return(): agent.ts's control loop exits solely by #applyControl
				// THROWING, and a failed apply must not be acked — the Runner redelivers to the
				// next session. A clean break after a SUCCESSFUL apply would redeliver an applied op.
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
