// O2 metric wiring on the two OUTBOUND transport modules — the publish spine
// (publish-spine.ts) and the durable frame sink (frame-sink.ts).
// Decision 2 of docs/designs/repo/compass-agent-effect-otel/design.md.
//
// The metrics no-op WITHOUT an OTel provider — but Effect still accumulates them
// in its process-global in-memory registry, which every ManagedRuntime shares by
// default (no metric-registry override in the transport's runtime, none here).
// So a forced overflow / failed batch / priority give-up / durable give-up moves
// the metric and `Metric.value` reads the delta back SYNCHRONOUSLY, with no live
// MetricReader, exporter, or network. This is the exact pattern otel-layer.test.ts
// uses (:64-67). Each case reads a baseline, forces the event, and asserts the
// delta AND that the frozen hand-rolled counters (droppedTraceCount /
// failedPriorityCount) are unchanged — the additive-not-replacement invariant.
//
// No wall-clock timer, no poll (ts-no-test-timers): failure is driven by the fake
// driver's rejection and gated on drain()'s returned promise. The frame-sink
// give-up case incurs the PRODUCTION retry backoff (frame-sink.ts's
// DURABLE_RETRY_BACKOFF_MS), event-gated by drain(), not a test-injected timer.

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
	durableAttempts,
	durableGiveUps,
	priorityBatchRetries,
	priorityFramesLost,
	priorityRetryDepthGauge,
	traceFramesLostFailedBatch,
	traceFramesLostOverflow,
	traceQueueDepthGauge,
} from "./otel-metrics";
import {
	createPublishSpine,
	PRIORITY_BATCH_RETRY_MS,
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
	// The metric caught every overflow drop, and the frozen hand-rolled counter
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
	// A resolving publish so the pump drains and drain() joins it. A backlog that
	// fits one batch (< PUBLISH_BATCH_MAX) is fully queued before the deferred
	// first takeBatch runs, so the gauge — sampled BEFORE the take — reads the
	// whole backlog, not the post-drain residual. A UNIQUE metric namespace gives
	// this gauge a private registry key, so a concurrent sibling test file writing
	// the shared key cannot move the absolute value between set and read (the
	// cross-file gauge race). The race-reproduction test below proves this
	// isolation is load-bearing, not decorative.
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
	const before = counterCount(priorityFramesLost);
	const retriesBefore = counterCount(priorityBatchRetries);
	// A unique namespace isolates the absolute retry-depth read from the shared
	// registry key: only the gauge is namespaced; the counters stay on
	// the shared key and are read as deltas, which are race-safe.
	const namespace = `${crypto.randomUUID()}.`;
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
	const retriesBefore = counterCount(priorityBatchRetries);
	// Unique namespace: the retry path first sets the gauge to 1, then the
	// successful send resets it to 0 — both writes land on this private key, so the
	// mutation check (drop the reset → gauge stuck at 1) stays non-vacuous while
	// the read is immune to the cross-file race.
	const namespace = `${crypto.randomUUID()}.`;
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
	expect(counterCount(priorityBatchRetries) - retriesBefore).toBe(1);
	// Reset to 0 on the successful send; read synchronously after drain.
	expect(gaugeValue(priorityRetryDepthGauge(namespace))).toBe(0);
	expect(spine.failedPriorityCount()).toBe(0);
	// Mutation check: removing `Metric.set(priorityRetryDepth, 0)` on the success
	// arm leaves the gauge at 1 → the depth assertion reddens.
});

test("a namespaced gauge read survives a concurrent writer clobbering the shared key", async () => {
	// The root cause of the gauge flake, reproduced deterministically. Under the concurrent
	// full suite a sibling test file constructs its own spine and writes the SAME
	// shared gauge key between this test's Metric.set and its synchronous read; a
	// gauge is an absolute last-writer-wins level, so the read saw the wrong value.
	// Here that hostile concurrent writer is made explicit: after the spine sets
	// its depth gauge, we clobber the SHARED (un-namespaced) key with a wrong
	// value, then read back. The namespaced read is unaffected — it hits a private
	// registry entry — while a read of the shared key would return the clobbered
	// value. This is the discriminating assertion the fix turns green: point the
	// spine at the empty namespace (the pre-fix shared key) and the two reads
	// collapse onto the same clobbered entry, reddening the inequality.
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
