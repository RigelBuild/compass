// The socket FrameSink: the outbound half of the agent↔Runner socket transport
// (transport-consolidation C4). It replaces the stdout ProtojsonLineSink (retired
// at C5) with a split-by-durability sink over AgentGateway, classifying each
// OutboundFrame by its frozen `kind` (frame.ts):
//
//   - a "session" frame (opaque trace + board lifecycle) is loss-tolerable and
//     rides the fire-and-forget Publish client-stream through the shared
//     PublishSpine. A lifecycle transition (SessionFrame.state set — notably the
//     terminal STOPPED) is a PRIORITY frame the spine never drops and flushes
//     ahead of the trace backlog; a trace-only frame (state UNSPECIFIED) rides
//     the bounded, drop-oldest trace queue.
//   - a "transcriptEntry" frame is DURABLE (SEA-1570): it is sent on the
//     PostConversationFrame UNARY via emitDurable(), awaited, and retried with
//     bounded backoff until delivered-or-erred, carrying an agent-minted
//     idempotency_key so a lost-response retry is deduped by the Runner (C2),
//     never duplicated. It is NEVER dropped on a reconnect.
//   - a "deliveryAck" frame is a control-plane ack (SEA-1310 §8): it rides the
//     Publish spine's never-drop PRIORITY lane, ahead of the trace backlog. It
//     is NOT durable — the Runner's isConversationFrame guard REJECTS an ack on
//     the PostConversationFrame unary; the Runner consumes it off the
//     PublishEvents spine to advance the delivery cursor. Control-plane acks are
//     never-drop by the spine contract, so it always enqueuePriority.
//
// emit() stays synchronous/void (CompassAgent's shape is unchanged): the durable
// transcript send (emitDurable) is forked as a tracked fiber into the sink's
// FiberSet (design record compass-agent-effect-adoption T2), retained behind
// drain() the teardown path awaits (bounded by the shutdown deadline), so
// shutdown cannot abandon an uncommitted transcript frame.

import { randomUUID } from "node:crypto";
import { create } from "@bufbuild/protobuf";
import {
	Cause,
	Effect,
	Exit,
	FiberSet,
	Logger,
	ManagedRuntime,
	Metric,
	Option,
	Schedule,
	Scope,
} from "effect";
import type { FrameSink, OutboundFrame } from "./../frame";
import {
	PostConversationFrameRequestSchema,
	PublishFrameRequestSchema,
} from "./../gen/compass/v1/agent_gateway_pb";
import {
	type AgentFrame,
	AgentFrameSchema,
} from "./../gen/compass/v1/agent_pb";
import { AgentSessionState } from "./../gen/compass/v1/compass_pb";
import type { RunnerTransport } from "./index";
import { durableAttempts, durableGiveUps } from "./otel-metrics";
import { getTransportRuntime } from "./runtime-channel";

// Bounded-backoff retry schedule for the durable unary (ms). A transient unary
// error (Runner mid-restart, socket blip) is retried on this fixed schedule;
// after the last delay the frame is treated as definitively erred and the send
// resolves (the agent has done all it can — the frame is on no droppable path,
// but an unbounded retry would wedge drain() past the shutdown deadline). The
// schedule is a named constant chosen once here.
export const DURABLE_RETRY_BACKOFF_MS: readonly number[] = [50, 200, 800, 2000];

// Per-attempt deadline on the durable unary (ms). Without it, a Runner that
// accepts the connection but never responds (a handler hung mid-teardown) would
// leave `postConversationFrame` pending forever: Effect.retry only advances on a
// FAILED attempt, so a hang — not an error — never fails the effect, never gives
// up, and wedges drain()'s FiberSet.awaitEmpty past the shutdown deadline. The
// deadline (kept as Connect's own timeoutMs — it cancels the wire call, which a
// bare Effect.timeout around the promise cannot) turns a hang into a retryable
// DeadlineExceeded the retry ladder advances on, so the give-up path is reachable
// for hangs too and drain() is bounded by the sink's own retry budget. Sized
// above the last backoff step so a merely-slow (not hung) Runner still gets its
// full retry.
const DURABLE_CALL_TIMEOUT_MS = 5000;

// Build the wire `AgentFrame` from a domain OutboundFrame — the same oneof stamp
// the ProtojsonLineSink used: the domain `kind` matches the generated `case`
// 1:1, so the mapped init IS the oneof init (the single assertion is checked by
// the frame round-trip tests).
function toAgentFrame(frame: OutboundFrame): AgentFrame {
	return create(AgentFrameSchema, {
		frame: { case: frame.kind, value: frame.value } as AgentFrame["frame"],
	});
}

// A "session" frame is a lifecycle transition (priority — never dropped) when it
// carries a board state; a bare trace frame leaves state UNSPECIFIED.
function isLifecycle(frame: OutboundFrame): boolean {
	return (
		frame.kind === "session" &&
		frame.value.state !== AgentSessionState.UNSPECIFIED
	);
}

// A "session" frame carrying a SessionInjection trace event is NOT loss-tolerable
// (steer/deliver split-observation seam, F3): a busy trace stream must not
// drop-oldest it off the bounded trace queue, or a cross-process observer could
// miss the op-kind a recipient session received. So it rides the never-drop
// priority lane, exactly like a lifecycle transition or a control ack — even
// though its board state is UNSPECIFIED (it is a trace event, not a transition).
function isInjection(frame: OutboundFrame): boolean {
	return (
		frame.kind === "session" &&
		frame.value.typedEvent?.event.case === "sessionInjection"
	);
}

export function createSocketFrameSink(transport: RunnerTransport): FrameSink {
	const spine = transport.publishSpine();
	// Borrow the single transport-owned ManagedRuntime through the module-private
	// channel when present; otherwise (a fake transport in a unit test) make and
	// OWN a default runtime as before (design record §T5). Effect is confined
	// module-private behind the sink: the runtime backs the FiberSet of forked
	// durable sends. The fallback runtime removes the default logger so a handled
	// forked-send failure does not double-report to the console (the give-up is
	// already surfaced to emitDurable's caller as a promise reject).
	const borrowedRuntime = getTransportRuntime(transport);
	const ownsRuntime = borrowedRuntime === undefined;
	const runtime =
		borrowedRuntime ?? ManagedRuntime.make(Logger.remove(Logger.defaultLogger));
	// A sink-lifetime scope backing the FiberSet: it must outlive each fork (a
	// scoped run would interrupt the set the moment its Effect returned), and
	// drain() closes it after the set has drained.
	const fiberScope = runtime.runSync(Scope.make());
	// In-flight durable sends are forked fibers in this set, retained so drain()
	// awaits every uncommitted transcript frame via FiberSet.awaitEmpty (replacing
	// the old snapshot-and-await over a Set<Promise>). A fiber removes itself from
	// the set on completion.
	const inflight = runtime.runSync(
		Scope.extend(FiberSet.make<void>(), fiberScope),
	);
	// Per-sink random nonce + monotonic counter feed the idempotency key. The key
	// must be STABLE across retries of one logical frame (one key minted per
	// emit, reused by every retry — the Runner dedups a lost-response retry) AND
	// DISTINCT across all frames the hub ever commits (its at-most-once unique
	// constraint on idempotency_key persists across agent replacement). A bare
	// `<pid>-<seq>` satisfies stability but NOT cross-restart distinctness: pid
	// reuse after wraparound + seq reset to 0 lets a respawned agent mint keys a
	// prior process already used, and the hub silently drops those genuinely-new
	// frames as duplicates. A per-sink random nonce makes the key space unique
	// per agent instance, so no two processes ever collide.
	const nonce = randomUUID();
	let seq = 0;
	// One-shot terminal latch for drain(). The old Set<Promise> drain was
	// idempotent (snapshot-and-await + idempotent spine.drain); the Effect drain
	// disposes the runtime and closes fiberScope, so a second call would run
	// FiberSet.awaitEmpty / dispose on an already-disposed runtime and throw.
	// Guard so a repeat drain is a no-op, preserving the old idempotent contract
	// (mirrors publish-spine's `ended` flag).
	let drained = false;

	// Unwrap the original error from an Effect failure Cause so the give-up seam
	// hands the caller the real ConnectError, not an Effect FiberFailure wrapper
	// (design record T2). A tryPromise failure carries the raw rejection in the
	// failure channel (Cause.failureOption); a defect/interrupt squashes.
	function causeError(cause: Cause.Cause<unknown>): unknown {
		return Option.getOrElse(Cause.failureOption(cause), () =>
			Cause.squash(cause),
		);
	}

	// Fork one durable send into the FiberSet, retained behind drain() so teardown
	// awaits its commit. The send is Effect.tryPromise over the unary — KEEPING
	// the per-attempt Connect timeoutMs, which cancels the wire call (a bare
	// Effect.timeout around the promise cannot: promise interruption does not abort
	// the RPC) — piped through Effect.retry on the fixed backoff ladder. The
	// idempotency key is minted ONCE here, outside the retried effect, so every
	// retry of one logical frame reuses it and the Runner dedups a lost-response
	// retry. `onSettle` is the give-up disposition, wired at FORK time (not at
	// drain/join) so a forked failure is observed by the caller's bridge BEFORE
	// FiberSet.awaitEmpty resolves: emitDurable() rejects its returned promise
	// (propagate, R4); a loss-tolerable launch would swallow the terminal error
	// (emit() carries no durable rider today — frame.ts — but the split is
	// preserved for T5). On retry-cap exhaustion the send fails and onSettle gets
	// the unwrapped error; on success onSettle gets undefined.
	function launchDurable(
		frame: OutboundFrame,
		onSettle: (err: unknown) => void,
	): void {
		const idempotencyKey = `${nonce}-${seq++}`;
		const request = create(PostConversationFrameRequestSchema, {
			frame: toAgentFrame(frame),
			idempotencyKey,
		});
		const send = Metric.increment(durableAttempts).pipe(
			// Count each durable attempt (initial + every retry). The increment is
			// INSIDE the unit Effect.retry re-runs, so it ticks once per attempt
			// (Decision 2 counter, frame_sink.durable_attempts).
			Effect.zipRight(
				Effect.tryPromise({
					try: () =>
						transport.postConversationFrame(request, {
							timeoutMs: DURABLE_CALL_TIMEOUT_MS,
						}),
					// Preserve the raw rejection (ConnectError) in the failure channel — do
					// NOT let tryPromise wrap it in an UnknownException, so causeError can
					// hand the original error back at the reject seam.
					catch: (err) => err,
				}),
			),
			Effect.retry(
				Schedule.fromDelays(
					DURABLE_RETRY_BACKOFF_MS[0],
					...DURABLE_RETRY_BACKOFF_MS.slice(1),
				),
			),
		);
		// Bridge the fiber's terminal exit to the caller's disposition. Effect.exit
		// absorbs the failure so the fiber itself always SUCCEEDS — the set's
		// failure deferred never trips and no unhandled fiber-failure is logged; the
		// give-up is delivered only through onSettle. FiberSet.run forks
		// synchronously, so the fiber (and this bridge) are wired into the set
		// before launchDurable returns.
		const bridged = Effect.flatMap(Effect.exit(send), (exit) =>
			Effect.gen(function* () {
				if (Exit.isFailure(exit)) {
					// Definitive give-up: retry budget exhausted (Decision 2 counter,
					// frame_sink.durable_give_ups). Only the error arm — never success.
					yield* Metric.increment(durableGiveUps);
					onSettle(causeError(exit.cause));
				} else {
					onSettle(undefined);
				}
			}),
		);
		runtime.runSync(FiberSet.run(inflight, bridged));
	}

	return {
		emit(frame: OutboundFrame): void {
			if (frame.kind === "session") {
				const request = create(PublishFrameRequestSchema, {
					frame: toAgentFrame(frame),
				});
				if (isLifecycle(frame) || isInjection(frame)) {
					spine.enqueuePriority(request);
				} else {
					spine.enqueueTrace(request);
				}
				return;
			}
			if (frame.kind === "deliveryAck") {
				// A per-message delivery receipt is a control-plane ack (SEA-1310 §8):
				// it rides the Publish spine's never-drop PRIORITY lane, NOT the durable
				// unary. The Runner's isConversationFrame guard REJECTS an ack on the
				// PostConversationFrame unary (post_conversation_frame.go:94-108), and
				// its consume side ingests the ack off the PublishEvents spine
				// (runnerhub/hub.go:358-360). Control-plane acks are never-drop by the
				// spine's own contract (publish-spine.ts:24-26,62), so enqueuePriority
				// unconditionally — never enqueueTrace, never launchDurable.
				const request = create(PublishFrameRequestSchema, {
					frame: toAgentFrame(frame),
				});
				spine.enqueuePriority(request);
				return;
			}
			// No other kind rides emit(): `transcriptEntry` (the surviving durable
			// rider) is sent via emitDurable(), never here.
		},

		emitDurable(frame: OutboundFrame): Promise<void> {
			// SEA-1570 transcript lane: the durable send is forked into the FiberSet
			// (same drain tracking as emit's launch), but its definitive give-up
			// PROPAGATES to the caller so the tee backend can buffer/retry/fatal (R4).
			// The Deferred→promise reject bridge is wired at FORK time (design record
			// T2), so a forked failure rejects this promise before drain's
			// FiberSet.awaitEmpty resolves. The backend awaits this inside the
			// per-path storage op, so per-session emit order == send order.
			// `session` frames never reach here (transcript is the only durable
			// rider on this lane).
			const { promise, resolve, reject } = Promise.withResolvers<void>();
			launchDurable(frame, (err) =>
				err === undefined ? resolve() : reject(err),
			);
			return promise;
		},

		async drain(): Promise<void> {
			// Idempotent: a second drain is a no-op (the first disposed the runtime).
			if (drained) return;
			drained = true;
			try {
				// Await every forked durable commit first (FiberSet.awaitEmpty), so no
				// transcript frame is abandoned uncommitted — the Effect equivalent of
				// the old snapshot-and-await over the in-flight promise set.
				await runtime.runPromise(FiberSet.awaitEmpty(inflight));
				// Then flush + close the Publish spine: any queued priority frame (the
				// terminal STOPPED) goes ahead of the trace backlog.
				await spine.drain();
			} finally {
				// Terminal for the sink (post-drain enqueues are no-ops by contract):
				// close the FiberSet's own scope (always — it belongs to the sink, not
				// the runtime). Then dispose ONLY a runtime this sink owns (the fallback
				// path); a borrowed transport-owned runtime is disposed by the
				// transport's close() after the drain barrier — disposing it here would
				// break the still-open sibling spine/source that share it (design record
				// §T5). In a `finally` so a rejecting awaitEmpty/spine.drain cannot
				// strand a self-owned runtime undisposed.
				await runtime.runPromise(Scope.close(fiberScope, Exit.void));
				if (ownsRuntime) await runtime.dispose();
			}
		},
	};
}
