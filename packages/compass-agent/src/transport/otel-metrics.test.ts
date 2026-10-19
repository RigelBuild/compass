// Pins outbound OTel metric deltas without a provider or network.
// Fake rejection drives failures; drain gates completion of each case.

import { expect, test } from "bun:test";
import { create } from "@bufbuild/protobuf";
import { Effect, Metric } from "effect";
import { TranscriptEntrySchema } from "../compassv1";
import type { OutboundFrame } from "../frame";
import {
	type PublishFrameRequest,
	PublishFrameRequestSchema,
} from "../gen/compass/v1/agent_gateway_pb";
import { createSocketFrameSink, DURABLE_RETRY_BACKOFF_MS } from "./frame-sink";
import type { RunnerTransport } from "./index";
import {
	batchesFlushedBy,
	batchSizeBy,
	durableAttempts,
	durableGiveUps,
	priorityBatchRetriesCounter,
	priorityFramesLostCounter,
	priorityRetryDepthGauge,
	traceFramesLostFailedBatch,
	traceFramesLostOverflow,
	traceQueueDepthGauge,
} from "./otel-metrics";
import {
	createPublishSpine,
	PRIORITY_BATCH_RETRY_MS,
	PUBLISH_BATCH_MAX,
	type PublishSpine,
	TRACE_QUEUE_CAP,
} from "./publish-spine";

// Read a counter's current count / a metric's Out synchronously off the global
// registry (the default the spine/sink runtimes and Effect.runSync all share).
function counterCount(metric: Metric.Metric.Counter<number>): number {
	return Effect.runSync(Metric.value(metric)).count;
}

// Read a gauge's current level synchronously off the same global registry.
function gaugeValue(metric: Metric.Metric.Gauge<number>): number {
	return Effect.runSync(Metric.value(metric)).value;
}

function traceFrame(): PublishFrameRequest {
	return create(PublishFrameRequestSchema, {});
}

function transcriptFrame(seq: bigint): OutboundFrame {
	return {
		kind: "transcriptEntry",
		value: create(TranscriptEntrySchema, { entrySeq: seq }),
	};
}

// A PublishSpine stub the frame-sink borrows: drain resolves, nothing recorded.
// The durable path never touches the spine, so a no-op spine is the whole seam.
function noopSpine(): PublishSpine {
	return {
		enqueueTrace: () => {},
		enqueuePriority: () => {},
		droppedTraceCount: () => 0,
		failedPriorityCount: () => 0,
		drain: () => Promise.resolve(),
	};
}

// A fake RunnerTransport whose postConversationFrame is scripted per-attempt.
// getTransportRuntime returns undefined for it (not registered), so the sink owns
// a fallback runtime. Member shape mirrors spineTransport in frame-sink.test.ts.
function durableTransport(
	postConversationFrame: RunnerTransport["postConversationFrame"],
): RunnerTransport {
	return {
		comms: () => Promise.reject(new Error("comms not used by this test")),
		lifecycle: () =>
			Promise.reject(new Error("lifecycle not used by this test")),
		forge: () => Promise.reject(new Error("forge not used by this test")),
		board: () => Promise.reject(new Error("board not used by this test")),
		publishSpine: () => noopSpine(),
		putSessionBlob: () =>
			Promise.reject(new Error("putSessionBlob not used by this test")),
		postConversationFrame,
		control: () => {
			throw new Error("control not used by this test");
		},
		close: () => {},
	};
}

test("a trace overflow increments trace_frames_lost{reason=overflow}, additive to droppedTraceCount", () => {
	// enqueueTrace is fully synchronous (runtime.runSync); the pump only runs on a
	// later scheduler tick (Effect.yieldNow defers its first batch), so a
	// synchronous burst of CAP + overflow enqueues fully populates and overflows
	// the bounded sliding queue before anything drains — exactly `overflow` drops.
	const overflow = 50;
	const before = counterCount(traceFramesLostOverflow);
	// A blocking publish so the pump, once it eventually runs, cannot drain the
	// queue and shrink the observed drop count.
	const spine = createPublishSpine(() => new Promise<never>(() => {}));
	for (let i = 0; i < TRACE_QUEUE_CAP + overflow; i++) {
		spine.enqueueTrace(traceFrame());
	}
	// The metric caught every overflow drop, and the hand-rolled counter
	// reports the SAME count — metric is additive, not a replacement.
	expect(counterCount(traceFramesLostOverflow) - before).toBe(overflow);
	expect(spine.droppedTraceCount()).toBe(overflow);
	// Mutation check: removing `runtime.runSync(Metric.increment(
	// traceFramesLostOverflow))` in enqueueTrace makes the delta 0 while
	// droppedTraceCount stays `overflow` → the first assertion reddens.
});

test("a failed trace batch increments trace_frames_lost{reason=failed_batch}, additive to droppedTraceCount", async () => {
	// A publish that always rejects, carrying trace-only frames: the pump's batch
	// fails, failedBatchFrames += batch.length (priorityCount 0), and — trace being
	// loss-tolerable — it is NOT retried, so drain() resolves.
	const frames = 7;
	const before = counterCount(traceFramesLostFailedBatch);
	const spine = createPublishSpine(() =>
		Promise.reject(new Error("batch send failed")),
	);
	for (let i = 0; i < frames; i++) spine.enqueueTrace(traceFrame());
	await spine.drain();
	expect(counterCount(traceFramesLostFailedBatch) - before).toBe(frames);
	expect(spine.droppedTraceCount()).toBe(frames);
});

test("trace_queue_depth samples the backlog at the take, before draining it", async () => {
	// A resolving send drains the queued backlog; a unique namespace isolates the gauge from concurrent tests.
	const namespace = `${crypto.randomUUID()}.`;
	const backlog = 100;
	const spine = createPublishSpine(
		() => Promise.resolve(undefined),
		undefined,
		namespace,
	);
	for (let i = 0; i < backlog; i++) spine.enqueueTrace(traceFrame());
	await spine.drain();
	expect(gaugeValue(traceQueueDepthGauge(namespace))).toBe(backlog);
	// Mutation check: removing `Metric.set(traceQueueDepth, traceSize())` from
	// takeBatch leaves the gauge at its prior value, not `backlog` → reddens.
});

test("a priority give-up increments priority_frames_lost, additive to failedPriorityCount", async () => {
	// A single priority frame against a persistently-dead socket: after the bounded
	// ladder is exhausted the frame is a definitive never-drop loss.
	// A unique namespace gives this spine private counters and gauge: bun runs files
	// concurrently, so even a delta on the shared key picks up a sibling's give-ups.
	const namespace = `${crypto.randomUUID()}.`;
	const priorityFramesLost = priorityFramesLostCounter(namespace);
	const priorityBatchRetries = priorityBatchRetriesCounter(namespace);
	const before = counterCount(priorityFramesLost);
	const retriesBefore = counterCount(priorityBatchRetries);
	const spine = createPublishSpine(
		() => Promise.reject(new Error("dead socket")),
		undefined,
		namespace,
	);
	spine.enqueuePriority(traceFrame());
	await spine.drain();
	expect(counterCount(priorityFramesLost) - before).toBe(1);
	expect(spine.failedPriorityCount()).toBe(1);
	// No trace loss was recorded on this path.
	expect(spine.droppedTraceCount()).toBe(0);
	// The bounded ladder ran to exhaustion: one retry attempt per delay (monotone
	// counter), and the pump-scoped depth level reached the ladder length and was
	// never reset (no send succeeded). Removing either publish-spine site zeroes a
	// delta.
	expect(counterCount(priorityBatchRetries) - retriesBefore).toBe(
		PRIORITY_BATCH_RETRY_MS.length,
	);
	expect(gaugeValue(priorityRetryDepthGauge(namespace))).toBe(
		PRIORITY_BATCH_RETRY_MS.length,
	);
});

test("a delivered priority batch resets priority_retry_depth to 0 after its retries", async () => {
	// Fail once, then deliver: one bounded retry, then a successful send resets the
	// pump-scoped depth level. Distinct from the give-up path — no frame is lost.
	const namespace = `${crypto.randomUUID()}.`;
	const retriesBefore = counterCount(priorityBatchRetriesCounter(namespace));
	// Unique namespace: the retry path first sets the gauge to 1, then the
	// successful send resets it to 0 — both writes land on this private key, so the
	// mutation check (drop the reset → gauge stuck at 1) stays non-vacuous while
	// the read is immune to the cross-file race.
	let attempt = 0;
	const spine = createPublishSpine(
		() => {
			attempt++;
			return attempt === 1
				? Promise.reject(new Error("transient blip"))
				: Promise.resolve(undefined);
		},
		undefined,
		namespace,
	);
	spine.enqueuePriority(traceFrame());
	await spine.drain();
	expect(
		counterCount(priorityBatchRetriesCounter(namespace)) - retriesBefore,
	).toBe(1);
	// Reset to 0 on the successful send; read synchronously after drain.
	expect(gaugeValue(priorityRetryDepthGauge(namespace))).toBe(0);
	expect(spine.failedPriorityCount()).toBe(0);
	// Mutation check: removing `Metric.set(priorityRetryDepth, 0)` on the success
	// arm leaves the gauge at 1 → the depth assertion reddens.
});

test("a namespaced gauge read survives a concurrent writer clobbering the shared key", async () => {
	// Clobbering the shared gauge proves the namespaced read remains isolated from concurrent writers.
	const namespace = `${crypto.randomUUID()}.`;
	const spine = createPublishSpine(
		() => Promise.reject(new Error("dead socket")),
		undefined,
		namespace,
	);
	spine.enqueuePriority(traceFrame());
	await spine.drain();
	// The spine set its private gauge to the exhausted ladder length.
	const isolated = gaugeValue(priorityRetryDepthGauge(namespace));
	expect(isolated).toBe(PRIORITY_BATCH_RETRY_MS.length);
	// A hostile concurrent writer clobbers the SHARED key with a value the spine
	// never wrote (the exact interleaving a sibling test file causes in CI).
	const clobbered = PRIORITY_BATCH_RETRY_MS.length + 999;
	Effect.runSync(Metric.set(priorityRetryDepthGauge(""), clobbered));
	// The namespaced read is immune; a shared-key read now returns the clobber.
	expect(gaugeValue(priorityRetryDepthGauge(namespace))).toBe(isolated);
	expect(gaugeValue(priorityRetryDepthGauge(""))).toBe(clobbered);
});

test("a durable send counts one attempt per try and one give-up when the retry budget is exhausted", async () => {
	// onDurable always throws → the send exhausts DURABLE_RETRY_BACKOFF_MS and
	// gives up. attempts = BACKOFF.length + 1 (initial + one per delay); give-ups
	// +1. Event-gated on drain() (which awaits the in-flight durable), no timer.
	const attemptsBefore = counterCount(durableAttempts);
	const giveUpsBefore = counterCount(durableGiveUps);
	let calls = 0;
	const sink = createSocketFrameSink(
		durableTransport(() => {
			calls++;
			return Promise.reject(new Error("always fails"));
		}),
	);
	const durable = sink.emitDurable(transcriptFrame(1n)).catch(() => {});
	await sink.drain?.();
	await durable;
	const expectedAttempts = DURABLE_RETRY_BACKOFF_MS.length + 1;
	expect(calls).toBe(expectedAttempts);
	expect(counterCount(durableAttempts) - attemptsBefore).toBe(expectedAttempts);
	expect(counterCount(durableGiveUps) - giveUpsBefore).toBe(1);
	// Mutation check: removing `Metric.increment(durableGiveUps)` from the
	// launchDurable failure arm makes the give-up delta 0 → the last assertion
	// reddens (attempts and drain still succeed, so the mutant reds not hangs).
});

test("a successful durable send counts its one attempt and no give-up", async () => {
	const attemptsBefore = counterCount(durableAttempts);
	const giveUpsBefore = counterCount(durableGiveUps);
	let calls = 0;
	const sink = createSocketFrameSink(
		durableTransport(() => {
			calls++;
			return Promise.resolve(undefined as never);
		}),
	);
	await sink.emitDurable(transcriptFrame(2n));
	await sink.drain?.();
	expect(calls).toBe(1);
	expect(counterCount(durableAttempts) - attemptsBefore).toBe(1);
	// The success arm never touches the give-up counter.
	expect(counterCount(durableGiveUps) - giveUpsBefore).toBe(0);
});

// A histogram's sample count and running total for one pre-tagged lane. `sum`
// pins the exact sizes recorded; the buckets are cumulative (`le`), so a
// per-bucket delta cannot tell a 1 from a 2 and is never asserted on.
function histShape(metric: Metric.Metric.Histogram<number>): {
	count: number;
	sum: number;
} {
	const state = Effect.runSync(Metric.value(metric));
	return { count: state.count, sum: state.sum };
}

// The histogram's finite upper bounds, in order. Pins the boundary
// CONFIGURATION, which count/sum alone cannot distinguish from another valid
// histogram. `+Inf` serializes as a non-finite key and is excluded.
function histBounds(metric: Metric.Metric.Histogram<number>): number[] {
	return Effect.runSync(Metric.value(metric))
		.buckets.map(([bound]) => bound)
		.filter((bound) => Number.isFinite(bound));
}

// All three reason counters for one private namespace. Absolute reads, not
// deltas: a unique namespace means nothing else can touch these keys, which is
// what lets a test assert the two NOT selected are exactly zero. On the shared
// key a concurrent sibling flush moves them and the exclusivity check flakes.
function reasonCounts(namespace: string): {
	full: number;
	drain: number;
	short: number;
} {
	const m = batchesFlushedBy(namespace);
	return {
		full: counterCount(m.full),
		drain: counterCount(m.drain),
		short: counterCount(m.short),
	};
}

// A unique registry prefix per test, so no two tests (or files) share a key.
function privateNamespace(): string {
	return `${crypto.randomUUID()}.`;
}

test("a saturated batch flushes as reason=full, exclusively", async () => {
	// PUBLISH_BATCH_MAX trace frames queue before the deferred first take, so the
	// first batch is exactly saturated. `full` outranks `drain` even though
	// drain() set `ended` — saturation is the more specific fact.
	const ns = privateNamespace();
	const spine = createPublishSpine(
		() => Promise.resolve(undefined),
		undefined,
		ns,
	);
	for (let i = 0; i < PUBLISH_BATCH_MAX; i++) spine.enqueueTrace(traceFrame());
	await spine.drain();
	expect(reasonCounts(ns)).toEqual({ full: 1, drain: 0, short: 0 });
	// One sample sized exactly to the cap, read absolutely off a private key.
	expect(histShape(batchSizeBy(ns).trace)).toEqual({
		count: 1,
		sum: PUBLISH_BATCH_MAX,
	});
	// The boundary configuration: the top finite bound is the cap, so a
	// saturated batch is the largest value the histogram can resolve exactly.
	expect(histBounds(batchSizeBy(ns).trace)).toEqual([
		1,
		2,
		4,
		8,
		16,
		32,
		64,
		128,
		PUBLISH_BATCH_MAX,
	]);
});

test("an ordinary partial batch flushes as reason=short, exclusively", async () => {
	// `short` needs a take while the spine is still LIVE: drain() sets `ended`
	// before the deferred first take, so an enqueue-then-drain batch is always
	// `drain`. Await the send itself, then drain the (now empty) spine.
	const ns = privateNamespace();
	let sent!: () => void;
	const firstSend = new Promise<void>((resolve) => {
		sent = resolve;
	});
	const spine = createPublishSpine(
		async (frames) => {
			for await (const _ of frames) {
			}
			sent();
		},
		undefined,
		ns,
	);
	spine.enqueueTrace(traceFrame());
	spine.enqueueTrace(traceFrame());
	await firstSend;
	expect(reasonCounts(ns)).toEqual({ full: 0, drain: 0, short: 1 });
	await spine.drain();
});

test("a batch taken after teardown began flushes as reason=drain, exclusively", async () => {
	// enqueuePriority before drain(), but the pump's first take is deferred one
	// scheduler yield — so drain() sets `ended` first and the take sees it.
	const ns = privateNamespace();
	const spine = createPublishSpine(
		() => Promise.resolve(undefined),
		undefined,
		ns,
	);
	spine.enqueuePriority(traceFrame());
	await spine.drain();
	expect(reasonCounts(ns)).toEqual({ full: 0, drain: 1, short: 0 });
});

test("a failed publish still counts its batch shape", async () => {
	// The update sits immediately after the take, so shape is recorded per send
	// ATTEMPT. Moving it after a successful publish would lose exactly the
	// batches an operator most wants to size.
	const ns = privateNamespace();
	const spine = createPublishSpine(
		() => Promise.reject(new Error("boom")),
		undefined,
		ns,
	);
	spine.enqueueTrace(traceFrame());
	await spine.drain();
	expect(reasonCounts(ns)).toEqual({ full: 0, drain: 1, short: 0 });
	expect(histShape(batchSizeBy(ns).trace)).toEqual({ count: 1, sum: 1 });
});

test("a priority batch re-taken after a failed send is counted again", async () => {
	// The retry path re-takes the same frames, so each ATTEMPT records its own
	// sample: two sends of one frame read as two samples summing to 2, not one.
	const ns = privateNamespace();
	let attempts = 0;
	const spine = createPublishSpine(
		() => {
			attempts += 1;
			return attempts === 1
				? Promise.reject(new Error("first attempt fails"))
				: Promise.resolve(undefined);
		},
		undefined,
		ns,
	);
	spine.enqueuePriority(traceFrame());
	await spine.drain();
	expect(attempts).toBeGreaterThanOrEqual(2);
	const size = histShape(batchSizeBy(ns).priority);
	expect(size.count).toBe(attempts);
	expect(size.sum).toBe(attempts);
});

test("batch_size tags the lane a batch was composed from", async () => {
	// priorityCount is fixed before trace frames are appended, so a batch holding
	// both is `mixed`. Separate spines keep the pure-lane batches from coalescing
	// into one mixed take; one private namespace keeps all three lanes readable.
	const ns = privateNamespace();
	const spineOn = () =>
		createPublishSpine(() => Promise.resolve(undefined), undefined, ns);

	const prioritySpine = spineOn();
	prioritySpine.enqueuePriority(traceFrame());
	await prioritySpine.drain();

	const traceSpine = spineOn();
	for (let i = 0; i < 2; i++) traceSpine.enqueueTrace(traceFrame());
	await traceSpine.drain();

	const mixedSpine = spineOn();
	mixedSpine.enqueuePriority(traceFrame());
	for (let i = 0; i < 2; i++) mixedSpine.enqueueTrace(traceFrame());
	await mixedSpine.drain();

	// Each lane gets a DISTINCT size (1, 2, 3), so swapping any two lane tags
	// moves a sum and fails. Equal sizes would let a swap pass.
	const size = batchSizeBy(ns);
	expect(histShape(size.priority)).toEqual({ count: 1, sum: 1 });
	expect(histShape(size.trace)).toEqual({ count: 1, sum: 2 });
	expect(histShape(size.mixed)).toEqual({ count: 1, sum: 3 });
});
