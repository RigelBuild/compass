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
// transcript send (emitDurable) is launched as a tracked in-flight promise,
// retained behind drain() the teardown path awaits (bounded by the shutdown
// deadline), so shutdown cannot abandon an uncommitted transcript frame.

import { randomUUID } from "node:crypto";
import { create } from "@bufbuild/protobuf";
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

// Bounded-backoff retry schedule for the durable unary (ms). A transient unary
// error (Runner mid-restart, socket blip) is retried on this fixed schedule;
// after the last delay the frame is treated as definitively erred and the send
// resolves (the agent has done all it can — the frame is on no droppable path,
// but an unbounded retry would wedge drain() past the shutdown deadline). The
// schedule is a named constant chosen once here.
export const DURABLE_RETRY_BACKOFF_MS: readonly number[] = [50, 200, 800, 2000];

// Per-attempt deadline on the durable unary (ms). Without it, a Runner that
// accepts the connection but never responds (a handler hung mid-teardown) would
// leave `await postConversationFrame` pending forever: the retry loop only
// advances on a THROWN error, so a hang — not an error — never increments
// `attempt`, never gives up, and wedges drain()'s in-flight wait past the
// shutdown deadline. A deadline turns a hang into a retryable DeadlineExceeded
// the same catch handles, so the give-up path is reachable for hangs too and
// drain() is bounded by the sink's own retry budget. Sized above the last
// backoff step so a merely-slow (not hung) Runner still gets its full retry.
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

export function createSocketFrameSink(transport: RunnerTransport): FrameSink {
	const spine = transport.publishSpine();
	// In-flight durable sends, retained so drain() awaits every uncommitted
	// transcript frame. Each entry removes itself on settle.
	const inflight = new Set<Promise<void>>();
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

	// Send one durable frame on the unary, retrying transient errors on the
	// bounded backoff schedule. The idempotency key is minted ONCE and reused
	// across retries so the Runner dedups a lost-response retry. On retry-cap
	// exhaustion this REJECTS with the last error — the definitive give-up
	// signal. `emit()` swallows that reject (loss-tolerable session telemetry,
	// unchanged); `emitDurable()` propagates it so the transcript tee
	// backend can buffer/retry/fatal (SEA-1570 R4).
	async function sendDurable(req: OutboundFrame): Promise<void> {
		const idempotencyKey = `${nonce}-${seq++}`;
		const request = create(PostConversationFrameRequestSchema, {
			frame: toAgentFrame(req),
			idempotencyKey,
		});
		for (let attempt = 0; ; attempt++) {
			try {
				await transport.postConversationFrame(request, {
					timeoutMs: DURABLE_CALL_TIMEOUT_MS,
				});
				return;
			} catch (err) {
				if (attempt >= DURABLE_RETRY_BACKOFF_MS.length) {
					// Exhausted: definitively erred. Throw so the durable transcript
					// lane (emitDurable) observes it; the void emit() path catches and
					// swallows (its frame is delivered-or-erred and the give-up is the
					// Runner's problem via gap-detection, never a crash).
					throw err;
				}
				const { promise, resolve } = Promise.withResolvers<void>();
				setTimeout(resolve, DURABLE_RETRY_BACKOFF_MS[attempt]);
				await promise;
			}
		}
	}

	// Launch a tracked durable send, retained behind drain() so teardown awaits
	// its commit. `onError` decides the give-up disposition: emit() swallows
	// (void, loss-tolerable), emitDurable() rejects to its caller (R4). The
	// returned promise settles when the send does.
	function launchDurable(
		frame: OutboundFrame,
		onError: (err: unknown) => void,
	): Promise<void> {
		const send = sendDurable(frame);
		const tracked = send.then(
			() => {},
			(err) => onError(err),
		);
		const retained = tracked.finally(() => {
			inflight.delete(retained);
		});
		inflight.add(retained);
		return retained;
	}

	return {
		emit(frame: OutboundFrame): void {
			if (frame.kind === "session") {
				const request = create(PublishFrameRequestSchema, {
					frame: toAgentFrame(frame),
				});
				if (isLifecycle(frame)) {
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
			// SEA-1570 transcript lane: same durable unary + drain tracking as
			// emit(), but the definitive-error reject PROPAGATES to the caller so the
			// tee backend can buffer/retry/fatal (R4). The backend awaits this inside
			// the per-path storage op, so per-session emit order == send order.
			// `session` frames never reach here (transcript is the only durable
			// rider on this lane).
			const { promise, resolve, reject } = Promise.withResolvers<void>();
			launchDurable(frame, (err) => reject(err)).then(resolve, () => {});
			return promise;
		},

		async drain(): Promise<void> {
			// Await every in-flight durable commit first, so no transcript frame is
			// abandoned uncommitted. New durable sends are not expected during
			// teardown, but a snapshot-and-await loop covers any launched by a late
			// emit before the caller stops feeding the sink.
			while (inflight.size > 0) {
				await Promise.all([...inflight]);
			}
			// Then flush + close the Publish spine: any queued priority frame (the
			// terminal STOPPED) goes ahead of the trace backlog.
			await spine.drain();
		},
	};
}
