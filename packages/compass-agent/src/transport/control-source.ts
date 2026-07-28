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
// WITHOUT the agent applying a single control op before the drop is definitive
// and the iterable fails. Reset by PROGRESS — the ack cursor advancing, i.e. an
// op actually applied — never by elapsed time.
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
// socket redelivers nothing and applies nothing however slowly it flaps, so it
// exhausts this budget; a session that applies ops between blips resets it and
// is never killed, at any spacing.
//
// The residual, stated rather than hidden: a genuinely IDLE session — one with
// no control traffic at all — makes no progress either, so 10 consecutive
// no-op reconnects fail it. That is deliberate fail-closed behavior. A control
// stream that has re-opened ten times and carried nothing is indistinguishable
// at this seam from a Runner that will never send again, and surfacing ERRORED
// is better than an agent silently reconnecting forever against a dead peer.
export const CONTROL_RECONNECT_NO_PROGRESS_MAX = 10;

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
		// Consecutive reconnects since the last op the agent actually applied. The
		// progress signal is the AckCursor's applied count, not a timestamp: it
		// advances only on a genuinely new application, so a redelivery the source
		// dedups and re-acks is correctly NOT progress.
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
			try {
				const stream = transport.control(
					create(ControlSubscribeRequestSchema, {}),
					{
						signal: abort.signal,
						onHeader: () => {
							established = true;
							openedAt = now();
						},
					},
				);
				for await (const wire of stream) {
					dispatch(wire);
				}
				buffer.close();
				return;
			} catch (err) {
				// The consumer's return() aborted the stream — not a transport drop.
				// End quietly (buffer already closed by return()); never reconnect or
				// fail() an intentional cancellation.
				if (abort.signal.aborted) return;
				// No-progress bound, checked BEFORE the uptime reset below — this is
				// the termination the reset cannot clear. A socket that is accepted,
				// stays up past the floor, and then fails (wedged Runner, server-side
				// deadline, door idle timeout) resets the ladder on every drop, so the
				// ladder can never bound it; what bounds it is that it never delivers
				// an op the agent applies. Any single application zeroes the counter,
				// so a healthy session is untouched however widely its blips are
				// spaced — the distinction a reconnect-RATE window could not draw,
				// since a healthy sparse-blip session and a socket wedging at an idle
				// timeout reconnect at the same rate.
				const applied = acks.appliedCount;
				noProgress = applied > appliedAtLastDrop ? 0 : noProgress + 1;
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
				// threw, where the elapsed time is dial latency, not uptime.
				if (established && now() - openedAt >= CONTROL_RECONNECT_MIN_UPTIME_MS)
					attempt = 0;
				if (attempt >= CONTROL_RECONNECT_BACKOFF_MS.length) {
					buffer.fail(err);
					return;
				}
				const delay = CONTROL_RECONNECT_BACKOFF_MS[attempt++];
				await sleepOrAbort(delay, abort.signal);
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
				// The consumer abandoned the `for await` (agent.ts's control loop
				// erroring / breaking): cancel the background pump and the underlying
				// Control server-stream so an abandoned source stops consuming the
				// transport, and close the buffer so an in-flight pull settles. Idempotent
				// (abort/close both no-op once fired). Returns done so the iterator
				// protocol completes cleanly (M2).
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
					return { value: undefined, done: true };
				},
			};
		},
	};
}
