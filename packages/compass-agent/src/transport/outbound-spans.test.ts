// O4 span wiring on the OUTBOUND transport (frame-sink.ts + publish-spine.ts).
// Decision 1 of docs/designs/platform/compass-agent-effect-otel/design.md: the
// four outbound spans — the durable-send parent + its per-attempt children on
// the frame sink, and the two teardown drain spans (one per module).
//
// Spans need a RECORDING runtime — the no-op tracer discards them — so each case
// injects one through the module-private `setTransportRuntime` channel (a test
// in src/transport/ may call it) with an in-memory OTel exporter wired through
// effect's tracer via `NodeSdk.layer`, exactly as the O3 span case does
// (control-metrics.test.ts:461-583). The sink/spine BORROW that injected runtime
// (setTransportRuntime registered it / it is passed as borrowedRuntime), so they
// never dispose it — the test owns that teardown.
//
// Spans are readable SYNCHRONOUSLY after the driven work settles because
// SimpleSpanProcessor exports each span on END: once the awaited send/drain
// promise resolves, every span it opened is finished and readable via
// `exporter.getFinishedSpans()`. That read MUST happen BEFORE the test disposes
// its runtime — `ManagedRuntime.dispose()` shuts the SDK down and clears the
// in-memory exporter. No test-authored timer is used (ts-no-test-timers): each
// case is event-gated on the code-under-test's own promises; only that code's
// bounded backoff sleeps elapse, so a generous per-test timeout arg covers them.

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

// A recording runtime whose tracer records into an in-memory exporter, built the
// transport's way (the OTel layer merged with the logger removal). The OTel
// layer is declared Layer<never> exactly as otel-layer.ts / the O3 span case do:
// NodeSdk.layer returns Layer<Resource.Resource>, and Layer's contravariant ROut
// lets it widen to Layer<never>, so the runtime is a ManagedRuntime<never, never>
// — the TransportRuntime shape setTransportRuntime / createPublishSpine expect.
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

// A fake RunnerTransport giving exact control over the durable unary's per-call
// outcome, plus a REAL PublishSpine over the injected recording runtime so the
// sink's spine.drain() opens the genuine publish.drain span (a noop spine would
// not). `postConversationFrame`'s outcome is driven by `durablePlan(callIndex)`:
// "ok" resolves, "fail" rejects with a distinguishable error. Member shape
// grounded against the fake carriers in frame-sink.test.ts:623-636.
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

test("a durable send opens one durable_send parent with a durable_attempt child per attempt, parented via the fiber tree, indexed 0..N", async () => {
	// Drive ONE durable send whose unary fails the first two attempts and
	// succeeds on the third (two forced failures → three attempts total).
	// Assert the fiber-tree parenting is OBSERVED, not assumed (record §467):
	// each durable_attempt's parentSpanContext.spanId equals the one
	// durable_send span's spanContext().spanId, and the three attempt attrs are
	// 0/1/2 in order.
	// Mutation: dropping the durable_attempt withSpan → 0 attempt children →
	// red; dropping the Ref tick / hardcoding attempt → the 0/1/2 assertion →
	// red; dropping the durable_send withSpan → attempts have no matching
	// parent → red.
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

test("an always-failing durable send records ERROR status on the durable_send span after the retry ladder exhausts", async () => {
	// Drive one durable send whose unary ALWAYS fails, so it exhausts the
	// DURABLE_RETRY_BACKOFF_MS ladder and gives up. Because the durable_send
	// withSpan sits INSIDE the Effect.exit boundary, the give-up failure reaches
	// the span and records ERROR status (Effect.exit still absorbs it for the
	// fiber, so emitDurable rejects but no unhandled fiber-failure escapes).
	// Mutation: moving the withSpan OUTSIDE Effect.exit → the failure is
	// absorbed before the span sees it → status stays unset → red.
	// The ladder is [50,200,800,2000]ms of the CODE's own backoff sleeps
	// (frozen exported const, not shortenable); the 10s timeout covers the
	// ~3.05s of real backoff with no test-authored timer.
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

test("drain opens both the frame_sink.drain and publish.drain spans, finished before the test disposes its runtime", async () => {
	// Build a sink over a fake transport whose spine is a REAL spine on the
	// injected runtime, drive a couple of frames, then call sink.drain(). The
	// sink's drain wraps its own FiberSet.awaitEmpty in frame_sink.drain and
	// then calls spine.drain(), which opens publish.drain from inside the spine.
	// Both spans must be FINISHED once drain() resolves — before the test's own
	// dispose (the sink borrows the injected runtime, so it does NOT dispose it).
	// Mutation: dropping either drain withSpan → its span absent → red.
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

test("a cycled batch send opens a publish.batch span carrying batch_size/priority_count/retry_index", async () => {
	// Drive one batch directly through a real spine on the injected runtime:
	// enqueue a single priority frame (so a batch cycles), then drain to flush
	// it. The pump opens publish.batch around the Effect.either(tryPromise) send
	// with the batch composition + pump-scoped retry level as attributes. For a
	// single lone priority frame on a live (resolving) driver: batch_size=1,
	// priority_count=1, retry_index=0.
	// Mutation: dropping the withSpan → span absent → red; wiring the wrong
	// attribute source → the attribute assertion → red.
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
