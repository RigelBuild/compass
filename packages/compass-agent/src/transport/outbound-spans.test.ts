// Pins outbound spans with a recording runtime and event-gated sends/drains.

import { expect, test } from "bun:test";
import { create } from "@bufbuild/protobuf";
import { NodeSdk } from "@effect/opentelemetry";
import { SpanStatusCode } from "@opentelemetry/api";
import {
	InMemorySpanExporter,
	type ReadableSpan,
	SimpleSpanProcessor,
} from "@opentelemetry/sdk-trace-base";
import { Layer, Logger, ManagedRuntime } from "effect";
import {
	AgentSessionState,
	SessionFrameSchema,
	TranscriptEntrySchema,
} from "../compassv1";
import type { OutboundFrame } from "../frame";
import {
	PostConversationFrameResponseSchema,
	type PublishFrameRequest,
	PublishFrameRequestSchema,
} from "../gen/compass/v1/agent_gateway_pb";
import { createSocketFrameSink } from "./frame-sink";
import type { RunnerTransport } from "./index";
import { createPublishSpine, type PublishSpine } from "./publish-spine";
import { setTransportRuntime, type TransportRuntime } from "./runtime-channel";

// The sink and spine borrow this OTel runtime; the test owns its disposal.
function recordingRuntime(exporter: InMemorySpanExporter): TransportRuntime {
	const otel: Layer.Layer<never> = NodeSdk.layer(() => ({
		spanProcessor: new SimpleSpanProcessor(exporter),
		resource: { serviceName: "compass-agent-test" },
	}));
	return ManagedRuntime.make(
		Layer.merge(Logger.remove(Logger.defaultLogger), otel),
	);
}

function transcriptFrame(seq: bigint): OutboundFrame {
	return {
		kind: "transcriptEntry",
		value: create(TranscriptEntrySchema, { entrySeq: seq }),
	};
}

// A publish driver that consumes the cycled batch then resolves — the wire-safe
// stream-cycling contract, faked. Records nothing; the spine's own pump opens the
// publish.batch span around each cycled send.
async function drainDriver(
	stream: AsyncIterable<PublishFrameRequest>,
): Promise<void> {
	for await (const _frame of stream) {
		// consume the batch (the real client-stream flushes on stream end)
	}
}

// Real PublishSpine wiring lets drain open its span while the fake controls send outcomes.
interface FakeConfig {
	runtime: TransportRuntime;
	// Per-call durable outcome. Call index is 0-based across ALL attempts of the
	// single logical frame each durable test drives.
	durablePlan: (callIndex: number) => "ok" | "fail";
}

function fakeDurableTransport(config: FakeConfig): RunnerTransport {
	let calls = 0;
	// One memoized real spine over the injected runtime, mirroring the real
	// transport's memoized publishSpine() singleton.
	const spine: PublishSpine = createPublishSpine(drainDriver, config.runtime);
	return {
		comms: () => Promise.reject(new Error("comms not used by this test")),
		lifecycle: () =>
			Promise.reject(new Error("lifecycle not used by this test")),
		forge: () => Promise.reject(new Error("forge not used by this test")),
		board: () => Promise.reject(new Error("board not used by this test")),
		putSessionBlob: () =>
			Promise.reject(new Error("putSessionBlob not used by this test")),
		publishSpine: () => spine,
		postConversationFrame: () => {
			const index = calls++;
			return config.durablePlan(index) === "ok"
				? Promise.resolve(create(PostConversationFrameResponseSchema, {}))
				: Promise.reject(new Error(`durable attempt ${index} forced failure`));
		},
		control: () => {
			throw new Error("control not used by this test");
		},
		close: () => {},
	};
}

function spansNamed(
	exporter: InMemorySpanExporter,
	name: string,
): ReadableSpan[] {
	return exporter.getFinishedSpans().filter((s) => s.name === name);
}

// biome-ignore lint/plugin: 10s backstops a hung fiber: the assertions event-gate on the durable send committing; the ceiling only bounds a send that never settles.
test("a durable send opens one durable_send parent with a durable_attempt child per attempt, parented via the fiber tree, indexed 0..N", async () => {
	// Three sends pin parentage and attempt order; removing either span or its attributes fails the assertions.
	const exporter = new InMemorySpanExporter();
	const runtime = recordingRuntime(exporter);
	const transport = fakeDurableTransport({
		runtime,
		durablePlan: (i) => (i < 2 ? "fail" : "ok"),
	});
	// Register the recording runtime BEFORE the sink borrows it.
	setTransportRuntime(transport, runtime);
	const sink = createSocketFrameSink(transport);

	// Event-gate on the durable promise: it resolves once the send commits
	// (third attempt succeeds), so every span it opened is finished.
	await sink.emitDurable(transcriptFrame(1n));

	// Read BEFORE dispose: shutting the SDK down clears the in-memory exporter.
	const sends = spansNamed(
		exporter,
		"compass_agent.transport.frame_sink.durable_send",
	);
	const attempts = spansNamed(
		exporter,
		"compass_agent.transport.frame_sink.durable_attempt",
	);
	expect(sends.length).toBe(1);
	expect(attempts.length).toBe(3);

	// Parenting observed: every attempt's parent is the one durable_send span.
	const parentId = (sends[0] as ReadableSpan).spanContext().spanId;
	for (const attempt of attempts) {
		expect(attempt.parentSpanContext?.spanId).toBe(parentId);
	}
	// The attempt attribute is the 0-based re-run index, in order.
	const indices = attempts
		.map((s) => s.attributes.attempt as number)
		.sort((a, b) => a - b);
	expect(indices).toEqual([0, 1, 2]);
	// The eventually-succeeding send leaves the parent span status unset/ok.
	expect((sends[0] as ReadableSpan).status.code).not.toBe(SpanStatusCode.ERROR);
	// The sink borrows the injected runtime, so it never disposes it — this
	// test owns teardown.
	await runtime.dispose();
}, 10_000);

// biome-ignore lint/plugin: 10s backstops a hung fiber: the assertions event-gate on the durable send exhausting its retry ladder and rejecting; the ceiling only bounds a send that never settles.
test("an always-failing durable send records ERROR status on the durable_send span after the retry ladder exhausts", async () => {
	// Exhausted retries must mark the durable_send span as ERROR before Effect.exit absorbs the failure.
	// The code's 3.05s backoff needs no test-authored timer.
	const exporter = new InMemorySpanExporter();
	const runtime = recordingRuntime(exporter);
	const transport = fakeDurableTransport({
		runtime,
		durablePlan: () => "fail",
	});
	setTransportRuntime(transport, runtime);
	const sink = createSocketFrameSink(transport);

	// Event-gate on the give-up rejection (propagated to emitDurable's caller).
	let rejected = false;
	await sink.emitDurable(transcriptFrame(2n)).catch(() => {
		rejected = true;
	});
	expect(rejected).toBe(true);

	const sends = spansNamed(
		exporter,
		"compass_agent.transport.frame_sink.durable_send",
	);
	expect(sends.length).toBe(1);
	expect((sends[0] as ReadableSpan).status.code).toBe(SpanStatusCode.ERROR);
	await runtime.dispose();
}, 10_000);

// biome-ignore lint/plugin: 10s backstops a hung fiber: the assertions event-gate on sink.drain() resolving; the ceiling only bounds a drain that never completes.
test("drain opens both the frame_sink.drain and publish.drain spans, finished before the test disposes its runtime", async () => {
	// Both sink and spine drain spans must finish before the test disposes their borrowed runtime.
	const exporter = new InMemorySpanExporter();
	const runtime = recordingRuntime(exporter);
	const transport = fakeDurableTransport({
		runtime,
		durablePlan: () => "ok",
	});
	setTransportRuntime(transport, runtime);
	const sink = createSocketFrameSink(transport);

	// Drive a durable commit (forked into the FiberSet drain awaits) and a
	// priority session frame (enqueued on the spine so its pump has a batch to
	// flush at drain). Await the durable so it is committed before drain.
	await sink.emitDurable(transcriptFrame(3n));
	sink.emit({
		kind: "session",
		value: create(SessionFrameSchema, { state: AgentSessionState.STOPPED }),
	});

	// Event-gate on drain resolving; both drain spans close during it.
	await sink.drain?.();

	// Read BEFORE the test disposes its runtime — the spans closed during drain.
	const sinkDrain = spansNamed(
		exporter,
		"compass_agent.transport.frame_sink.drain",
	);
	const spineDrain = spansNamed(
		exporter,
		"compass_agent.transport.publish.drain",
	);
	expect(sinkDrain.length).toBe(1);
	expect(spineDrain.length).toBe(1);
	expect((sinkDrain[0] as ReadableSpan).ended).toBe(true);
	expect((spineDrain[0] as ReadableSpan).ended).toBe(true);
	await runtime.dispose();
}, 10_000);

// biome-ignore lint/plugin: 10s backstops a hung fiber: the assertions event-gate on spine.drain() joining the pump; the ceiling only bounds a drain that never completes.
test("a cycled batch send opens a publish.batch span carrying batch_size/priority_count/retry_index", async () => {
	// One priority frame pins the batch span's size, priority count, and retry attributes.
	const exporter = new InMemorySpanExporter();
	const runtime = recordingRuntime(exporter);
	const spine = createPublishSpine(drainDriver, runtime);
	// A priority frame guarantees a batch cycles immediately at drain.
	spine.enqueuePriority(
		create(PublishFrameRequestSchema, {
			frame: {
				frame: {
					case: "session",
					value: create(SessionFrameSchema, {
						state: AgentSessionState.STOPPED,
					}),
				},
			},
		}),
	);
	// Event-gate on drain: it joins the pump, which has cycled the batch.
	await spine.drain();

	const batches = spansNamed(exporter, "compass_agent.transport.publish.batch");
	expect(batches.length).toBeGreaterThanOrEqual(1);
	const batch = batches[0] as ReadableSpan;
	expect(batch.attributes.batch_size).toBe(1);
	expect(batch.attributes.priority_count).toBe(1);
	expect(batch.attributes.retry_index).toBe(0);
	await runtime.dispose();
}, 10_000);
