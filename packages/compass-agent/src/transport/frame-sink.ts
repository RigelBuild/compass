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
//   - a "conversationPosted"/"conversationUpdated" frame is DURABLE: it is sent
//     on the PostConversationFrame UNARY, awaited, and retried with bounded
//     backoff until delivered-or-erred, carrying an agent-minted idempotency_key
//     so a lost-response retry is deduped by the Runner (C2), never duplicated.
//     It is NEVER dropped on a reconnect.
//
// emit() stays synchronous/void (CompassAgent's shape is unchanged): a durable
// send is launched as a tracked in-flight promise, retained behind drain() the
// teardown path awaits (bounded by the shutdown deadline), so shutdown cannot
// abandon an uncommitted conversation frame.

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
	// conversation frame. Each entry removes itself on settle.
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

	// Send one durable conversation frame on the unary, retrying transient errors
	// on the bounded backoff schedule. The idempotency key is minted ONCE and
	// reused across retries so the Runner dedups a lost-response retry.
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
			} catch {
				if (attempt >= DURABLE_RETRY_BACKOFF_MS.length) {
					// Exhausted: definitively erred. Do NOT throw — a rejection here
					// would surface as an unhandled rejection off the void emit() path;
					// the frame is delivered-or-erred and the agent has retried to the
					// bound. Surfacing the give-up is the Runner's problem once the
					// socket returns (gap-detection), not the sink's to crash on.
					return;
				}
				await new Promise((r) =>
					setTimeout(r, DURABLE_RETRY_BACKOFF_MS[attempt]),
				);
			}
		}
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
			// Durable conversation frame: launch the tracked, retried send. emit()
			// stays void — the promise is retained for drain(), and its rejection
			// path is handled inside sendDurable (never throws), so this is not an
			// unhandled rejection.
			const send = sendDurable(frame).finally(() => {
				inflight.delete(send);
			});
			inflight.add(send);
		},

		async drain(): Promise<void> {
			// Await every in-flight durable commit first, so no conversation frame is
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
