// The socket FrameSink: the outbound half of the agent↔Runner socket transport
// (transport-consolidation C4). A split-by-durability sink over AgentGateway,
// classifying each OutboundFrame by its `kind` (frame.ts):

// - a "session" frame (opaque trace + board lifecycle) is loss-tolerable and rides
//   the fire-and-forget Publish client-stream through the shared PublishSpine. A
//   lifecycle transition (SessionFrame.state set — notably terminal STOPPED) is a
//   PRIORITY frame the spine never drops; a trace-only frame rides the drop-oldest queue.

// - a "transcriptEntry" frame is DURABLE (RIG-1570): sent on the PostConversationFrame
//   UNARY via emitDurable(), awaited and retried with bounded backoff until delivered-
//   or-erred, carrying an agent-minted idempotency_key so a lost-response retry is
//   deduped by the Runner (C2). NEVER dropped on a reconnect.

// - a "deliveryAck" frame is a control-plane ack (RIG-1310 §8): it rides the Publish
//   spine's never-drop PRIORITY lane. NOT durable — the Runner's isConversationFrame
//   guard REJECTS an ack on the unary; the Runner consumes it off the PublishEvents
//   spine to advance the delivery cursor. Always enqueuePriority.

// emit() stays synchronous/void: the durable transcript send (emitDurable) is forked
// as a tracked fiber into the sink's FiberSet (effect-adoption T2), retained behind
// drain() the teardown path awaits (bounded by the shutdown deadline), so shutdown
// cannot abandon an uncommitted transcript frame.

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
	Ref,
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

// Bounded-backoff retry schedule for the durable unary (ms). A transient unary error
// is retried on this schedule; after the last delay the frame is definitively erred
// and the send resolves (the frame is on no droppable path, but an unbounded retry
// would wedge drain() past the shutdown deadline).
export const DURABLE_RETRY_BACKOFF_MS: readonly number[] = [50, 200, 800, 2000];

// Per-attempt deadline on the durable unary (ms). Without it, a Runner that accepts
// but never responds leaves postConversationFrame pending forever (Effect.retry
// advances only on a FAILED attempt). Kept as Connect's timeoutMs (it cancels the wire
// call) so a hang becomes a retryable DeadlineExceeded. Sized above the last backoff step.
const DURABLE_CALL_TIMEOUT_MS = 5000;

// Build the wire AgentFrame from a domain OutboundFrame — the domain `kind` matches the
// generated `case` 1:1, so the mapped init IS the oneof init (checked by the round-trip tests).
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
// (steer/deliver split-observation seam, F3): drop-oldest off the trace queue could make
// a cross-process observer miss the op-kind a recipient received. So it rides the never-
// drop priority lane like a lifecycle transition, though its board state is UNSPECIFIED.
function isInjection(frame: OutboundFrame): boolean {
	return (
		frame.kind === "session" &&
		frame.value.typedEvent?.event.case === "sessionInjection"
	);
}

// A "session" frame carrying a SessionError trace event is NOT loss-tolerable (DL-323):
// the surfaced failure content is observation-critical — for reason=aborted it is the
// SOLE signal, and for reason=error the content would still vanish off the trace queue
// under backlog. So it rides the never-drop priority lane, matching the Injection carve-out.
function isSessionError(frame: OutboundFrame): boolean {
	return (
		frame.kind === "session" &&
		frame.value.typedEvent?.event.case === "sessionError"
	);
}

export function createSocketFrameSink(transport: RunnerTransport): FrameSink {
	const spine = transport.publishSpine();
	// Borrow the single transport-owned ManagedRuntime when present; otherwise (a fake
	// transport) make and OWN a default (§T5). It backs the FiberSet of forked durable
	// sends. The fallback removes the default logger so a handled forked-send failure does
	// not double-report (the give-up already surfaces to emitDurable's caller).
	const borrowedRuntime = getTransportRuntime(transport);
	const ownsRuntime = borrowedRuntime === undefined;
	const runtime =
		borrowedRuntime ?? ManagedRuntime.make(Logger.remove(Logger.defaultLogger));
	// A sink-lifetime scope backing the FiberSet: it must outlive each fork (a scoped run
	// would interrupt the set the moment its Effect returned); drain() closes it after.
	const fiberScope = runtime.runSync(Scope.make());
	// In-flight durable sends are forked fibers in this set, retained so drain() awaits
	// every uncommitted transcript frame via FiberSet.awaitEmpty. A fiber removes itself
	// from the set on completion.
	const inflight = runtime.runSync(
		Scope.extend(FiberSet.make<void>(), fiberScope),
	);
	// Per-sink random nonce + monotonic counter feed the idempotency key: STABLE across
	// retries of one frame (Runner dedups a lost-response retry) AND DISTINCT across all
	// frames. A bare `<pid>-<seq>` fails cross-restart distinctness (pid reuse + seq reset);
	// a per-sink random nonce makes the key space unique per agent instance.
	const nonce = randomUUID();
	let seq = 0;
	// One-shot terminal latch for drain(). The Effect drain disposes the runtime and closes
	// fiberScope, so a second call would run awaitEmpty/dispose on a disposed runtime and
	// throw. Guard so a repeat drain is a no-op (mirrors publish-spine's `ended` flag).
	let drained = false;

	// Unwrap the original error from an Effect failure Cause so the give-up seam hands the
	// caller the real ConnectError, not a FiberFailure wrapper (T2). A tryPromise failure
	// carries the raw rejection (Cause.failureOption); a defect/interrupt squashes.
	function causeError(cause: Cause.Cause<unknown>): unknown {
		return Option.getOrElse(Cause.failureOption(cause), () =>
			Cause.squash(cause),
		);
	}

	// Fork one durable send into the FiberSet, retained behind drain(). Effect.tryPromise
	// over the unary — KEEPING the per-attempt Connect timeoutMs — piped through Effect.retry
	// on the backoff ladder; the idempotency key is minted ONCE so every retry reuses it.
	// onSettle wired at FORK time so a forked failure rejects emitDurable() before awaitEmpty.
	function launchDurable(
		frame: OutboundFrame,
		onSettle: (err: unknown) => void,
	): void {
		const idempotencyKey = `${nonce}-${seq++}`;
		const request = create(PostConversationFrameRequestSchema, {
			frame: toAgentFrame(frame),
			idempotencyKey,
		});
		// Per-send attempt counter (Decision 1 span attribute source). Minted ONCE per
		// launchDurable — outside the retried unit and the span scope, like the idempotency
		// key — so its read-and-increment inside the retried unit yields the 0-based attempt
		// index in lockstep with the durableAttempts tick sharing the same re-run unit.
		const send = Ref.make(0).pipe(
			Effect.flatMap((attemptRef) =>
				Metric.increment(durableAttempts).pipe(
					// Count each durable attempt (initial + every retry). The increment is
					// INSIDE the unit Effect.retry re-runs, so it ticks once per attempt
					// (Decision 2 counter, frame_sink.durable_attempts).
					Effect.zipRight(
						// Read-and-increment the attempt index in the SAME re-run unit as the
						// metric tick and the tryPromise, so span N carries attempt === N.
						Ref.getAndUpdate(attemptRef, (n) => n + 1),
					),
					Effect.flatMap((attempt) =>
						Effect.tryPromise({
							try: () =>
								transport.postConversationFrame(request, {
									timeoutMs: DURABLE_CALL_TIMEOUT_MS,
								}),
							// Preserve the raw rejection (ConnectError) in the failure channel — do
							// NOT let tryPromise wrap it in an UnknownException, so causeError can
							// hand the original error back at the reject seam.
							catch: (err) => err,
						}).pipe(
							// Decision 1 child span: one per attempt. A sibling under the one
							// durable_send parent via fiber-tree propagation (no manual parent
							// threading). `attempt` is the 0-based index for this re-run.
							Effect.withSpan(
								"compass_agent.transport.frame_sink.durable_attempt",
								{ attributes: { attempt } },
							),
						),
					),
					Effect.retry(
						Schedule.fromDelays(
							DURABLE_RETRY_BACKOFF_MS[0],
							...DURABLE_RETRY_BACKOFF_MS.slice(1),
						),
					),
				),
			),
			// Decision 1 parent span: the whole durable send (all attempts). Placed
			// INSIDE the Effect.exit boundary below so on retry-cap exhaustion the
			// span sees the failure and records ERROR status, while Effect.exit still
			// absorbs it for the fiber. `frame_kind` is known at launch.
			Effect.withSpan("compass_agent.transport.frame_sink.durable_send", {
				attributes: { frame_kind: frame.kind },
			}),
		);
		// Bridge the fiber's terminal exit to the caller's disposition. Effect.exit absorbs
		// the failure so the fiber always SUCCEEDS (the set's failure deferred never trips,
		// no unhandled fiber-failure logged); the give-up is delivered only through onSettle.
		// FiberSet.run forks synchronously, so the fiber is wired in before launchDurable returns.
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
				if (isLifecycle(frame) || isInjection(frame) || isSessionError(frame)) {
					spine.enqueuePriority(request);
				} else {
					spine.enqueueTrace(request);
				}
				return;
			}
			if (
				frame.kind === "deliveryAck" ||
				frame.kind === "forgeNotificationAck"
			) {
				// A per-notification/message delivery receipt is a control-plane ack (RIG-1310
				// §8; RIG-2732 W3 forge ack): it rides the Publish spine's never-drop PRIORITY
				// lane, NOT the durable unary. The Runner's isConversationFrame guard REJECTS an
				// ack on the unary and consumes it off the PublishEvents spine. Always enqueuePriority.
				const request = create(PublishFrameRequestSchema, {
					frame: toAgentFrame(frame),
				});
				spine.enqueuePriority(request);
				return;
			}
			// No other kind rides emit(): `transcriptEntry` (the durable rider) is sent via
			// emitDurable(), never here.
		},

		emitDurable(frame: OutboundFrame): Promise<void> {
			// RIG-1570 transcript lane: the durable send is forked into the FiberSet (same drain
			// tracking as emit's launch), but its definitive give-up PROPAGATES to the caller
			// (R4). The Deferred→promise reject bridge is wired at FORK time (T2), so a forked
			// failure rejects before awaitEmpty resolves. `session` frames never reach here.
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
				// Await every forked durable commit first (FiberSet.awaitEmpty), so no transcript
				// frame is abandoned uncommitted.
				await runtime.runPromise(
					FiberSet.awaitEmpty(inflight).pipe(
						// Decision 1 span: the sink-side teardown flush. Wraps ONLY the sink's own
						// awaitEmpty — spine.drain() below opens its own publish.drain span, so no
						// double-wrap. Closes before drain() resolves, hence before dispose.
						Effect.withSpan("compass_agent.transport.frame_sink.drain"),
					),
				);
				// Then flush + close the Publish spine: any queued priority frame (the terminal
				// STOPPED) goes ahead of the trace backlog.
				await spine.drain();
			} finally {
				// Terminal for the sink: close the FiberSet's own scope (always — it belongs to
				// the sink). Then dispose ONLY a runtime this sink owns; a borrowed runtime is
				// disposed by transport.close() after the drain barrier (disposing here would
				// break the sibling spine/source, §T5). In a finally so a reject cannot strand it.
				await runtime.runPromise(Scope.close(fiberScope, Exit.void));
				if (ownsRuntime) await runtime.dispose();
			}
		},
	};
}
