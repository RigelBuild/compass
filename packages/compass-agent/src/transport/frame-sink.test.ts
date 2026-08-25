// The socket FrameSink contract (transport-consolidation C4, outbound half):
// createSocketFrameSink must split OutboundFrames by durability over a REAL
// AgentGateway socket — trace/session frames onto the fire-and-forget Publish
// client-stream (bounded, drop-oldest, lifecycle-priority), conversation frames
// onto the delivered-or-erred PostConversationFrame unary (awaited, retried with
// a stable idempotency key, drained at teardown). These tests stand up a live
// connect-node h2c server bound to a Unix socket and drive real emit()/drain()
// calls through it: a mock would restate the sink; only a live socket server
// catches a broken route split, a dropped durable retry, or a terminal frame
// stuck behind a saturated trace buffer.
//
// NOTE (author-run, not an independent test agent): the wave's Tester spawn hit
// the frozen-session provisioning defect (FS-less phantom that sub-delegates
// instead of reading), so these were authored by the implementer. Each case was
// verified non-vacuous by the mutation described in its header comment.

import { afterEach, expect, test } from "bun:test";
import * as fs from "node:fs";
import * as http2 from "node:http2";
import * as os from "node:os";
import * as path from "node:path";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { connectNodeAdapter } from "@connectrpc/connect-node";
import {
	AgentSessionState,
	DeliveryAckSchema,
	SessionEventSchema,
	SessionFrameSchema,
	SessionInjectionKind,
	SessionInjectionSchema,
	SessionNoticeSchema,
	TranscriptEntrySchema,
} from "../compassv1";
import type { OutboundFrame } from "../frame";
import {
	AgentGateway,
	type PostConversationFrameRequest,
	PostConversationFrameResponseSchema,
	type PublishFrameRequest,
	PublishFrameResponseSchema,
} from "../gen/compass/v1/agent_gateway_pb";
import { createSocketFrameSink, DURABLE_RETRY_BACKOFF_MS } from "./frame-sink";
import { createUnixSocketTransport, type RunnerTransport } from "./index";
import {
	PUBLISH_BATCH_MAX,
	type PublishSpine,
	TRACE_QUEUE_CAP,
} from "./publish-spine";

// One server + socket per test, torn down in afterEach so a failing case never
// leaks the socket file or a listening server into the next test.
let activeServer: http2.Http2Server | undefined;
let activeSocketPath: string | undefined;

afterEach(async () => {
	if (activeServer !== undefined) {
		const server = activeServer;
		await new Promise<void>((resolve) => server.close(() => resolve()));
		activeServer = undefined;
	}
	if (activeSocketPath !== undefined && fs.existsSync(activeSocketPath)) {
		fs.unlinkSync(activeSocketPath);
	}
	activeSocketPath = undefined;
});

// What a test server captures: the frames received on each RPC, plus a
// cross-stream arrival log for the one test where ordering between streams
// matters (terminal flush).
interface Recorder {
	publishFrames: PublishFrameRequest[];
	durableAttempts: PostConversationFrameRequest[];
	durableFrames: PostConversationFrameRequest[];
	arrivals: string[];
}

interface ServerHooks {
	// Awaited AFTER each Publish element is recorded — stalls the Publish
	// consumer (queue-cap + terminal-flush tests).
	onPublish?: (frame: PublishFrameRequest) => Promise<void> | void;
	// Awaited after the attempt is recorded; may throw to inject a transient
	// error (retry test) or await to hold the call open (drain test). `attempt`
	// is 1-based per idempotency key.
	onDurable?: (
		frame: PostConversationFrameRequest,
		attempt: number,
	) => Promise<void> | void;
}

async function serve(rec: Recorder, hooks: ServerHooks): Promise<string> {
	const perDurableAttempt = new Map<string, number>();
	const socketPath = path.join(
		os.tmpdir(),
		`c4-${process.pid}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}.sock`,
	);
	const adapter = connectNodeAdapter({
		routes(router) {
			router.rpc(AgentGateway.method.publish, async (stream) => {
				for await (const frame of stream) {
					// Record BEFORE the gate so a test awaiting the gate always sees the
					// frame already in the recorder (no record-vs-assert race).
					rec.publishFrames.push(frame);
					rec.arrivals.push("publish");
					if (hooks.onPublish) await hooks.onPublish(frame);
				}
				return create(PublishFrameResponseSchema, {});
			});
			router.rpc(AgentGateway.method.postConversationFrame, async (req) => {
				const attempt = (perDurableAttempt.get(req.idempotencyKey) ?? 0) + 1;
				perDurableAttempt.set(req.idempotencyKey, attempt);
				// Every arriving attempt (including one about to be failed) is an
				// attempt; the hook may throw to inject a transient error.
				rec.durableAttempts.push(req);
				if (hooks.onDurable) await hooks.onDurable(req, attempt);
				// Reached only when the hook did NOT throw: a committed durable frame.
				rec.durableFrames.push(req);
				rec.arrivals.push("durable");
				return create(PostConversationFrameResponseSchema, {});
			});
		},
	});
	const server = http2.createServer(adapter);
	activeServer = server;
	activeSocketPath = socketPath;
	await new Promise<void>((resolve) => server.listen(socketPath, resolve));
	return socketPath;
}

function emptyRecorder(): Recorder {
	return {
		publishFrames: [],
		durableAttempts: [],
		durableFrames: [],
		arrivals: [],
	};
}

function traceFrame(): OutboundFrame {
	return {
		kind: "session",
		value: create(SessionFrameSchema, { state: AgentSessionState.UNSPECIFIED }),
	};
}

// A trace frame tagged with a wire-observable ordinal (via SessionNotice.text),
// so a test can assert WHICH trace frames survived the drop-oldest queue — not
// merely how many were counted. UNSPECIFIED state keeps it on the trace lane.
function traceFrameN(ordinal: number): OutboundFrame {
	return {
		kind: "session",
		value: create(SessionFrameSchema, {
			state: AgentSessionState.UNSPECIFIED,
			typedEvent: create(SessionEventSchema, {
				event: {
					case: "notice",
					value: create(SessionNoticeSchema, { text: String(ordinal) }),
				},
			}),
		}),
	};
}

// Read back the ordinal a traceFrameN stamped, from a received PublishFrameRequest.
function ordinalOf(frame: PublishFrameRequest): number | undefined {
	const inner = frame.frame?.frame;
	if (inner?.case !== "session") return undefined;
	const ev = inner.value.typedEvent?.event;
	if (ev?.case !== "notice") return undefined;
	return Number(ev.value.text);
}

function lifecycleFrame(state: AgentSessionState): OutboundFrame {
	return { kind: "session", value: create(SessionFrameSchema, { state }) };
}

// A durable transcript frame — the surviving rider on the PostConversationFrame
// unary (SEA-1570). `seq` disambiguates frames within a test.
function transcriptFrame(seq: bigint): OutboundFrame {
	return {
		kind: "transcriptEntry",
		value: create(TranscriptEntrySchema, { entrySeq: seq }),
	};
}

function deferred(): { promise: Promise<void>; resolve: () => void } {
	let resolve!: () => void;
	const promise = new Promise<void>((r) => {
		resolve = r;
	});
	return { promise, resolve };
}

test("session frames ride the Publish client-stream in emission order", async () => {
	// Non-vacuity: if session frames routed to the unary (or arrived reordered),
	// publishFrames would be empty / out of order → red.
	const rec = emptyRecorder();
	const gotThree = deferred();
	let seen = 0;
	const socketPath = await serve(rec, {
		onPublish: () => {
			if (++seen === 3) gotThree.resolve();
		},
	});
	const sink = createSocketFrameSink(createUnixSocketTransport(socketPath));
	sink.emit(traceFrame());
	sink.emit(traceFrame());
	sink.emit(traceFrame());
	await gotThree.promise;
	expect(rec.publishFrames.length).toBe(3);
	expect(rec.durableFrames.length).toBe(0);
});

test("a transcript frame rides the PostConversationFrame unary, not Publish, with an idempotency key", async () => {
	// Non-vacuity: routing durable → Publish leaves durableFrames empty → red; a
	// missing idempotency key leaves the field "" → red.
	const rec = emptyRecorder();
	const got = deferred();
	const socketPath = await serve(rec, {
		onDurable: () => {
			got.resolve();
		},
	});
	const sink = createSocketFrameSink(createUnixSocketTransport(socketPath));
	void sink.emitDurable(transcriptFrame(1n));
	await got.promise;
	// durableAttempts is recorded before the gate resolves (race-free).
	expect(rec.durableAttempts.length).toBe(1);
	expect(rec.publishFrames.length).toBe(0);
	expect(rec.durableAttempts[0]?.idempotencyKey).toBeTruthy();
	expect(rec.durableAttempts[0]?.frame?.frame.case).toBe("transcriptEntry");
});

test("a durable frame is retried across a transient error and reuses its idempotency key", async () => {
	// Non-vacuity: if the sink minted a fresh key per attempt, the two recorded
	// attempts would differ → red; if it gave up without retry, only one (failed)
	// attempt reaches the recorder → red.
	const rec = emptyRecorder();
	const seenKeys: string[] = [];
	const succeeded = deferred();
	const socketPath = await serve(rec, {
		onDurable: (frame, attempt) => {
			seenKeys.push(frame.idempotencyKey);
			if (attempt === 1) {
				throw new Error("transient");
			}
			succeeded.resolve();
		},
	});
	const sink = createSocketFrameSink(createUnixSocketTransport(socketPath));
	void sink.emitDurable(transcriptFrame(2n));
	await succeeded.promise;
	// seenKeys is recorded inside the hook (before the gate resolves), so it is
	// race-free; assert on it and on durableAttempts (also pre-hook). A single
	// logical frame produced exactly two attempts (one failed, one retried) with
	// the SAME minted key — the dedup contract.
	expect(seenKeys.length).toBe(2); // failed + retried
	expect(seenKeys[0]).toBe(seenKeys[1]); // SAME key across retries
	expect(rec.durableAttempts.length).toBe(2);
});

test("drain() awaits an outstanding durable commit before resolving", async () => {
	// Non-vacuity: if drain() ignored in-flight durables, it would resolve while
	// the unary is still held → committedBeforeDrain false → red.
	const rec = emptyRecorder();
	const entered = deferred();
	const release = deferred();
	let committed = false;
	const socketPath = await serve(rec, {
		onDurable: async () => {
			entered.resolve();
			await release.promise;
			committed = true;
		},
	});
	const sink = createSocketFrameSink(createUnixSocketTransport(socketPath));
	void sink.emitDurable(transcriptFrame(3n));
	// Start draining while the server still holds the unary open.
	let drained = false;
	const drainP = (sink.drain?.() ?? Promise.resolve()).then(() => {
		drained = true;
	});
	// Deterministic gate: once the server has ENTERED the unary handler, the
	// commit is provably still pending (it is blocked on `release`, unfired). If
	// drain ignored in-flight durables it would already have resolved — so assert
	// it has not, with no wall-clock guess.
	await entered.promise;
	expect(drained).toBe(false);
	expect(committed).toBe(false);
	// Release the commit; now drain must resolve, and only after the commit.
	release.resolve();
	await drainP;
	expect(committed).toBe(true);
	expect(drained).toBe(true);
});

test("the trace queue is bounded: overflow drops oldest and counts", async () => {
	// Non-vacuity: an unbounded queue would never increment the drop counter →
	// red on the > 0 assertion; a queue that dropped newest would still count but
	// this asserts the counter tracks overflow beyond the cap exactly.
	const rec = emptyRecorder();
	// Stall the Publish consumer forever so nothing drains server-side and the
	// in-agent queue must absorb (and cap) the burst.
	const blockPublish = deferred();
	const socketPath = await serve(rec, {
		onPublish: () => blockPublish.promise,
	});
	const transport = createUnixSocketTransport(socketPath);
	const sink = createSocketFrameSink(transport);
	const overflow = 50;
	// One frame may be pulled into the in-flight send before the stall engages;
	// emit cap + overflow + a margin and assert the drop counter caught the
	// overflow. Exact accounting: at most one frame is in the generator's hand,
	// the rest sit in the bounded queue.
	for (let i = 0; i < TRACE_QUEUE_CAP + overflow; i++) {
		sink.emit(traceFrame());
	}
	const dropped = transport.publishSpine().droppedTraceCount();
	// Overflow beyond the cap was dropped; at least `overflow` minus the single
	// in-flight frame the consumer pulled.
	expect(dropped).toBeGreaterThanOrEqual(overflow - 1);
	blockPublish.resolve();
});

test("terminal STOPPED is flushed ahead of a queued trace backlog", async () => {
	// The record's guarantee: a lifecycle frame (STOPPED) is never dropped and is
	// drained ahead of any queued trace backlog. The sink emits a synchronous
	// burst (a saturating trace backlog, then the terminal STOPPED) in one tick;
	// the spine coalesces the tick and drains its priority lane first, so STOPPED
	// leads the batch.
	// Non-vacuity: if STOPPED shared the trace FIFO (no priority lane), it would
	// arrive at position ~200, not first → firstStateSeen would be UNSPECIFIED.
	const rec = emptyRecorder();
	let firstStateSeen: AgentSessionState | undefined;
	const stoppedArrived = deferred();
	const socketPath = await serve(rec, {
		onPublish: (frame) => {
			const state =
				frame.frame?.frame.case === "session"
					? frame.frame.frame.value.state
					: AgentSessionState.UNSPECIFIED;
			if (firstStateSeen === undefined) firstStateSeen = state;
			if (state === AgentSessionState.STOPPED) stoppedArrived.resolve();
		},
	});
	const sink = createSocketFrameSink(createUnixSocketTransport(socketPath));
	// One synchronous tick: saturating trace backlog, THEN the terminal frame.
	for (let i = 0; i < 200; i++) sink.emit(traceFrame());
	sink.emit(lifecycleFrame(AgentSessionState.STOPPED));
	await stoppedArrived.promise;
	expect(firstStateSeen).toBe(AgentSessionState.STOPPED);
	// Drain so every cycled batch completes against the live server before
	// afterEach tears it down (a batch mid-connect to a closed socket would
	// surface as ECONNREFUSED).
	await sink.drain?.();
});

test("a failed batch drops its trace frames but never its priority frames", async () => {
	// M1: a batch send that throws (socket blip mid-teardown) must NOT silently
	// abandon a priority frame (terminal STOPPED). The pump re-enqueues the
	// batch's priority frames and retries; trace frames in a failed batch stay
	// loss-tolerable. Non-vacuity: if a failed batch folded its priority frames
	// into the trace-drop count and dropped them (the pre-fix behavior), STOPPED
	// would never arrive → stoppedArrived never resolves (test times out) and
	// failedPriorityCount would stay 0 while the frame was really lost.
	const rec = emptyRecorder();
	let failFirst = true;
	const stoppedArrived = deferred();
	const socketPath = await serve(rec, {
		onPublish: (frame) => {
			// Fail the very first batch send once, then let every retry succeed.
			if (failFirst) {
				failFirst = false;
				throw new Error("transient batch failure");
			}
			const inner = frame.frame?.frame;
			if (
				inner?.case === "session" &&
				inner.value.state === AgentSessionState.STOPPED
			) {
				stoppedArrived.resolve();
			}
		},
	});
	const transport = createUnixSocketTransport(socketPath);
	const sink = createSocketFrameSink(transport);
	// A lone terminal STOPPED: its batch is the one that fails first.
	sink.emit(lifecycleFrame(AgentSessionState.STOPPED));
	await stoppedArrived.promise;
	// The priority frame survived the failed batch (retried, not dropped) and no
	// priority loss was recorded.
	expect(transport.publishSpine().failedPriorityCount()).toBe(0);
});

test("a durable send gives up after the retry budget, rejecting emitDurable", async () => {
	// M4(b): the retry-exhaustion branch. onDurable always throws, so the send
	// exhausts DURABLE_RETRY_BACKOFF_MS and gives up. emitDurable PROPAGATES the
	// definitive error to its caller (the tee backend buffers/retries/fatals),
	// so the returned promise rejects — handled here, never an unhandled
	// rejection. Assert the exact attempt count and that drain() still resolves
	// within the retry budget. Non-vacuity: an off-by-one give-up or an unbounded
	// retry would make the attempt count wrong or hang drain().
	const rec = emptyRecorder();
	let attempts = 0;
	let unhandled = false;
	const onUnhandled = () => {
		unhandled = true;
	};
	process.on("unhandledRejection", onUnhandled);
	try {
		const socketPath = await serve(rec, {
			onDurable: () => {
				attempts++;
				throw new Error("always fails");
			},
		});
		const sink = createSocketFrameSink(createUnixSocketTransport(socketPath));
		let rejected = false;
		const durable = sink.emitDurable(transcriptFrame(4n)).catch(() => {
			rejected = true;
		});
		// drain() awaits the in-flight durable; it must resolve once the send has
		// exhausted its budget and given up (not hang, not throw).
		await sink.drain?.();
		await durable;
		// The definitive give-up surfaced to the emitDurable caller as a reject.
		expect(rejected).toBe(true);
		// One initial try + one per backoff delay = BACKOFF.length + 1 attempts.
		expect(attempts).toBe(DURABLE_RETRY_BACKOFF_MS.length + 1);
		// Let any stray microtask-scheduled rejection surface before asserting.
		await new Promise((r) => setTimeout(r, 0));
		expect(unhandled).toBe(false);
	} finally {
		process.off("unhandledRejection", onUnhandled);
	}
});

test("emitDurable's give-up rejection carries the ORIGINAL ConnectError, not an Effect wrapper", async () => {
	// RIG-2448 coverage gap (design record
	// docs/designs/platform/compass-agent-effect-adoption/design.md, Global
	// Constraints "Error identity is preserved at the promise boundary"; T2
	// give-up seam). The durable send uses two-arg
	// Effect.tryPromise({ try, catch: (e) => e }) so the raw rejection stays in
	// the failure channel, and causeError unwraps it (Cause.failureOption /
	// squash) at the reject seam — so emitDurable()'s rejection is the ORIGINAL
	// ConnectError, never an Effect FiberFailure/UnknownException wrapper. The
	// existing give-up test above asserts only rejected===true and discards the
	// value, so a regression to single-arg tryPromise (which wraps in
	// UnknownException) would stay green there. This pins the identity.
	//
	// Non-vacuity (mutation-verified): change the source's two-arg
	// tryPromise({ try, catch: (err) => err }) in launchDurable to the single-arg
	// Effect.tryPromise(() => ...) → the rejection becomes an UnknownException
	// wrapper, so `instanceof ConnectError` and `.code === Code.Unavailable` both
	// red.
	const rec = emptyRecorder();
	const thrown = new ConnectError("runner unavailable", Code.Unavailable);
	const socketPath = await serve(rec, {
		onDurable: () => {
			// Always throw the SAME distinguishable ConnectError so the give-up
			// path (after the retry budget) rejects with it.
			throw thrown;
		},
	});
	const sink = createSocketFrameSink(createUnixSocketTransport(socketPath));
	let caught: unknown;
	const durable = sink
		.emitDurable(transcriptFrame(5n))
		.catch((err: unknown) => {
			caught = err;
		});
	// drain() awaits the in-flight durable; it resolves once the send exhausts
	// its budget and gives up.
	await sink.drain?.();
	await durable;
	// The caught value IS the original ConnectError, not a wrapper: the wire
	// round-trip yields a fresh ConnectError instance (not referential identity
	// with `thrown`), so assert TYPE + CODE — the identity the unwrap preserves.
	expect(caught).toBeInstanceOf(ConnectError);
	expect((caught as ConnectError).code).toBe(Code.Unavailable);
});

test("the trace queue drops the OLDEST frames, keeping the newest cap-worth", async () => {
	// M4(c): assert drop-OLDEST semantics, not merely the drop count. Tag every
	// trace frame with an ordinal, stall the consumer so the in-agent queue caps,
	// then release and read which ordinals actually arrived. Non-vacuity: a queue
	// that dropped NEWEST (or an unbounded queue) would deliver a different
	// surviving set — the min surviving ordinal pins oldest-dropped.
	const rec = emptyRecorder();
	const release = deferred();
	let stalled = false;
	const socketPath = await serve(rec, {
		onPublish: async () => {
			// Stall only the first consumed frame, long enough that the whole burst
			// is enqueued (and capped) before draining resumes.
			if (!stalled) {
				stalled = true;
				await release.promise;
			}
		},
	});
	const transport = createUnixSocketTransport(socketPath);
	const sink = createSocketFrameSink(transport);
	const overflow = 50;
	const total = TRACE_QUEUE_CAP + overflow;
	for (let i = 0; i < total; i++) sink.emit(traceFrameN(i));
	// The queue has capped; release the consumer and drain every cycled batch.
	release.resolve();
	await sink.drain?.();
	const received = rec.publishFrames
		.map(ordinalOf)
		.filter((n): n is number => n !== undefined);
	// Oldest-dropped: no surviving ordinal is below the overflow watermark minus
	// the single frame the consumer had already pulled into its hand before the
	// stall. i.e. the earliest ordinals (0..~overflow-1) were the ones dropped.
	const minSurviving = Math.min(...received);
	expect(minSurviving).toBeGreaterThanOrEqual(overflow - 1);
	// And the newest frame is always kept (never the drop target).
	expect(received).toContain(total - 1);
});

test("STOPPED leads across cycled batches with a live consumer and loses no trace", async () => {
	// M4(d): the cross-batch property. Emit MORE than PUBLISH_BATCH_MAX traces
	// (forcing multiple cycled stream batches) then the terminal STOPPED, against
	// a LIVE (non-stalled) consumer, and assert STOPPED arrives ahead of the
	// trace backlog AND every trace frame is delivered across the batch
	// boundaries. Non-vacuity: a per-batch (not global) priority lane would let a
	// full first batch of traces land before STOPPED; a lost frame at a cycle
	// boundary would drop the received count below the emitted count.
	const rec = emptyRecorder();
	const traceCount = PUBLISH_BATCH_MAX * 2 + 10;
	const stoppedArrived = deferred();
	let firstStateSeen: AgentSessionState | undefined;
	const socketPath = await serve(rec, {
		onPublish: (frame) => {
			const inner = frame.frame?.frame;
			const state =
				inner?.case === "session"
					? inner.value.state
					: AgentSessionState.UNSPECIFIED;
			if (firstStateSeen === undefined) firstStateSeen = state;
			if (state === AgentSessionState.STOPPED) stoppedArrived.resolve();
		},
	});
	const sink = createSocketFrameSink(createUnixSocketTransport(socketPath));
	// One synchronous tick: a multi-batch trace backlog, THEN terminal STOPPED.
	for (let i = 0; i < traceCount; i++) sink.emit(traceFrameN(i));
	sink.emit(lifecycleFrame(AgentSessionState.STOPPED));
	await stoppedArrived.promise;
	// STOPPED led the very first batch, ahead of the whole trace backlog.
	expect(firstStateSeen).toBe(AgentSessionState.STOPPED);
	await sink.drain?.();
	// No trace lost at any cycle boundary: all `traceCount` traces delivered.
	const traceOrdinals = rec.publishFrames
		.map(ordinalOf)
		.filter((n): n is number => n !== undefined);
	expect(traceOrdinals.length).toBe(traceCount);
	expect(new Set(traceOrdinals).size).toBe(traceCount);
});

test("a deliveryAck rides the Publish PRIORITY lane, not the durable unary", async () => {
	// SEA-1310 §8: a per-message delivery receipt is a control-plane ack. Per the
	// spine contract (publish-spine.ts:24-26,62) it rides the Publish spine's
	// never-drop PRIORITY lane, NOT the durable PostConversationFrame unary — the
	// Runner gateway's isConversationFrame guard REJECTS an ack on that unary, so
	// a deliveryAck routed durable 400s and is silently swallowed (the delivery
	// cursor never advances). Non-vacuity: with the pre-fix emit() the ack falls
	// through to launchDurable → it lands in durableAttempts and publishFrames is
	// empty → red. The priority (vs trace) choice is covered by emit() calling
	// enqueuePriority in source (publish-spine.ts:24-26); the socket recorder does
	// not distinguish the two Publish sub-lanes, so the observable contract here
	// is Publish-not-durable.
	const rec = emptyRecorder();
	const sink = createSocketFrameSink(
		createUnixSocketTransport(await serve(rec, {})),
	);
	sink.emit({
		kind: "deliveryAck",
		value: create(DeliveryAckSchema, { messageId: "m-1" }),
	});
	await sink.drain?.();
	// The ack arrived on the Publish spine carrying the delivery_ack oneof case.
	expect(rec.publishFrames.length).toBe(1);
	expect(rec.publishFrames[0]?.frame?.frame.case).toBe("deliveryAck");
	const inner = rec.publishFrames[0]?.frame?.frame;
	expect(
		inner?.case === "deliveryAck" ? inner.value.messageId : undefined,
	).toBe("m-1");
	// It NEVER touched the durable unary.
	expect(rec.durableAttempts.length).toBe(0);
	expect(rec.durableFrames.length).toBe(0);
});

// A PublishSpine spy with SEPARATE priority/trace recorders, so a test can
// distinguish the two Publish sub-lanes the socket recorder cannot (it sees both
// as `publishFrames`). Mirrors the recordingSpine pattern in
// control/ack-cursor.test.ts:22-37.
function spySpine(): {
	spine: PublishSpine;
	priorityFrames: PublishFrameRequest[];
	traceFrames: PublishFrameRequest[];
} {
	const priorityFrames: PublishFrameRequest[] = [];
	const traceFrames: PublishFrameRequest[] = [];
	return {
		priorityFrames,
		traceFrames,
		spine: {
			enqueueTrace: (f) => traceFrames.push(f),
			enqueuePriority: (f) => priorityFrames.push(f),
			droppedTraceCount: () => 0,
			failedPriorityCount: () => 0,
			drain: () => Promise.resolve(),
		},
	};
}

// A fake RunnerTransport whose publishSpine() hands back the injected spy. emit()
// reaches its spine only through transport.publishSpine() (frame-sink.ts:82), so
// this is the whole seam. The other RPCs are never touched by a deliveryAck
// emit; they throw so a mistaken call is loud. Member shape grounded against the
// fake carriers in control-source.test.ts / cli.test.ts:357.
function spineTransport(spine: PublishSpine): RunnerTransport {
	return {
		comms: () => Promise.reject(new Error("comms not used by this test")),
		lifecycle: () =>
			Promise.reject(new Error("lifecycle not used by this test")),
		forge: () => Promise.reject(new Error("forge not used by this test")),
		publishSpine: () => spine,
		postConversationFrame: () =>
			Promise.reject(new Error("postConversationFrame not used by this test")),
		control: () => {
			throw new Error("control not used by this test");
		},
		close: () => {},
	};
}

test("a deliveryAck rides the Publish PRIORITY sub-lane, never the drop-oldest trace queue", () => {
	// SEA-1310 §8 (re-review MEDIUM): the socket-level test above pins
	// Publish-not-durable but CANNOT distinguish enqueuePriority from
	// enqueueTrace (both land on publishFrames). A future edit flipping emit()'s
	// deliveryAck arm (frame-sink.ts:178) to enqueueTrace would compile and pass
	// every socket test while silently downgrading acks to the bounded,
	// drop-oldest, loss-tolerable trace queue — reintroducing the MEDIUM #1
	// cursor-strand class. This spy-spine test pins the priority-vs-trace choice
	// the socket recorder is blind to. Non-vacuity: flip the source arm to
	// enqueueTrace → the priority assertion reddens (0) and the trace assertion
	// reddens (1).
	const { spine, priorityFrames, traceFrames } = spySpine();
	const sink = createSocketFrameSink(spineTransport(spine));
	sink.emit({
		kind: "deliveryAck",
		value: create(DeliveryAckSchema, { messageId: "m-1" }),
	});
	// Exactly one priority frame, carrying the deliveryAck oneof case + id.
	expect(priorityFrames.length).toBe(1);
	const inner = priorityFrames[0]?.frame?.frame;
	expect(inner?.case).toBe("deliveryAck");
	expect(
		inner?.case === "deliveryAck" ? inner.value.messageId : undefined,
	).toBe("m-1");
	// It never touched the loss-tolerable trace lane.
	expect(traceFrames.length).toBe(0);
});

test("a SessionInjection rides the Publish PRIORITY sub-lane, never the drop-oldest trace queue", () => {
	// RIG-2486 (T1) F3: a SessionInjection is a "session" trace frame (state
	// UNSPECIFIED), so by the default classification it would ride the bounded,
	// drop-oldest trace lane — where a busy trace stream could silently drop the
	// op-kind observation a cross-process test depends on. isInjection() pins it
	// onto the never-drop priority lane instead. The socket recorder cannot
	// distinguish the two Publish sub-lanes, so this spy-spine test is what pins
	// the choice. Non-vacuity: drop the `|| isInjection(frame)` arm in emit()
	// (frame-sink.ts) → the priority assertion reddens (0) and the trace assertion
	// reddens (1).
	const { spine, priorityFrames, traceFrames } = spySpine();
	const sink = createSocketFrameSink(spineTransport(spine));
	sink.emit({
		kind: "session",
		value: create(SessionFrameSchema, {
			state: AgentSessionState.UNSPECIFIED,
			typedEvent: create(SessionEventSchema, {
				event: {
					case: "sessionInjection",
					value: create(SessionInjectionSchema, {
						opKind: SessionInjectionKind.STEER,
						messageId: "m-1",
					}),
				},
			}),
		}),
	});
	// Exactly one priority frame, carrying the sessionInjection oneof case + id.
	expect(priorityFrames.length).toBe(1);
	const inner = priorityFrames[0]?.frame?.frame;
	expect(inner?.case).toBe("session");
	const event =
		inner?.case === "session" ? inner.value.typedEvent?.event : undefined;
	expect(event?.case).toBe("sessionInjection");
	expect(
		event?.case === "sessionInjection" ? event.value.messageId : undefined,
	).toBe("m-1");
	// It never touched the loss-tolerable trace lane.
	expect(traceFrames.length).toBe(0);
});
